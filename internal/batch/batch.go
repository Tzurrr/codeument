// Package batch groups events into batches per shell session and decides
// when a batch is ready to be summarized.
package batch

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/model"
)

// Policy holds the trigger thresholds (see config.Triggers).
type Policy struct {
	IdleGap         time.Duration
	ScoreThreshold  int
	MeaningfulCount int
	Timer           time.Duration
	MinInterval     time.Duration
	Milestones      bool
	// MinMeaningful is the least number of meaningful events a batch needs
	// to be summarized at all.
	MinMeaningful int
	// MaxEvents splits a batch that grows past this size.
	MaxEvents int
}

// Trigger reasons.
const (
	TriggerScore         = "score"
	TriggerCount         = "meaningful_count"
	TriggerMilestone     = "milestone"
	TriggerConfigRestart = "config_restart"
	TriggerTimer         = "timer"
	TriggerIdle          = "idle"
	TriggerSessionEnd    = "session_end"
	TriggerManual        = "manual"
	TriggerMaxEvents     = "max_events"
)

// Decision is the outcome of observing an event.
type Decision struct {
	Batch   *model.Batch
	Trigger string // non-empty when the batch became pending_summary
	// Closed is a previous batch that was closed by this observation.
	Closed *model.Batch
}

// Observe attaches ev to the session's open batch (creating or rotating it)
// and returns whether a summary should run. ev.BatchID is set on return; the
// caller inserts the event afterwards.
func Observe(ctx context.Context, store *journal.Store, ev *model.Event, pol Policy, now time.Time) (Decision, error) {
	var dec Decision
	b, err := store.OpenBatch(ctx, ev.SessionID)
	switch {
	case errors.Is(err, journal.ErrNotFound):
		b = nil
	case err != nil:
		return dec, err
	}
	if b != nil && (now.Sub(b.LastEventAt) > pol.IdleGap || (pol.MaxEvents > 0 && b.EventCount >= pol.MaxEvents)) {
		reason := TriggerIdle
		if pol.MaxEvents > 0 && b.EventCount >= pol.MaxEvents {
			reason = TriggerMaxEvents
		}
		if err := Close(ctx, store, b, pol, reason, b.LastEventAt); err != nil {
			return dec, err
		}
		dec.Closed = b
		b = nil
	}
	if b == nil {
		b = &model.Batch{ID: uuid.New().String(), SessionID: ev.SessionID, StartedAt: ev.Start, LastEventAt: ev.Start, Status: model.BatchOpen}
		if err := store.CreateBatch(ctx, b); err != nil {
			return dec, err
		}
	}
	ev.BatchID = b.ID
	b.EventCount++
	b.LastEventAt = ev.Start
	if ev.Kind == model.KindMeaningful {
		b.MeaningfulCount++
		b.Score += ev.Weight
	}

	trigger := ""
	if b.MeaningfulCount >= pol.MinMeaningful {
		switch {
		case b.Score >= pol.ScoreThreshold:
			trigger = TriggerScore
		case b.MeaningfulCount >= pol.MeaningfulCount:
			trigger = TriggerCount
		case pol.Milestones && ev.Milestone && b.MeaningfulCount >= 2:
			trigger = TriggerMilestone
		case ev.Family == "service" && ev.Weight >= 4:
			if n, err := store.CountEvents(ctx, journal.EventQuery{BatchID: b.ID}); err == nil && n > 0 {
				evs, _ := store.ListEvents(ctx, journal.EventQuery{BatchID: b.ID})
				for _, e := range evs {
					if e.Family == "config" {
						trigger = TriggerConfigRestart
						break
					}
				}
			}
		}
		if trigger == "" && pol.Timer > 0 {
			if first, err := firstMeaningful(ctx, store, b.ID); err == nil && !first.IsZero() && now.Sub(first) >= pol.Timer {
				trigger = TriggerTimer
			}
		}
	}
	if trigger != "" && !cooledDown(ctx, store, ev.SessionID, pol.MinInterval, now) {
		trigger = ""
	}
	if trigger != "" {
		b.Status = model.BatchPending
		b.TriggerReason = trigger
		t := now
		b.ClosedAt = &t
		_ = store.SetKV(ctx, "last_summary:"+ev.SessionID, fmt.Sprint(now.UnixNano()))
	}
	if err := store.UpdateBatch(ctx, b); err != nil {
		return dec, err
	}
	dec.Batch = b
	dec.Trigger = trigger
	return dec, nil
}

// Close finalises an open batch: pending_summary when it holds enough
// meaningful work, skipped otherwise.
func Close(ctx context.Context, store *journal.Store, b *model.Batch, pol Policy, reason string, at time.Time) error {
	if b.Status != model.BatchOpen {
		return nil
	}
	t := at
	b.ClosedAt = &t
	if b.MeaningfulCount >= pol.MinMeaningful {
		b.Status = model.BatchPending
		b.TriggerReason = reason
	} else {
		b.Status = model.BatchSkipped
		b.TriggerReason = reason
	}
	return store.UpdateBatch(ctx, b)
}

// CloseStale closes open batches whose last event is older than the idle gap
// and fires timer triggers. It returns the batches that became pending.
func CloseStale(ctx context.Context, store *journal.Store, pol Policy, now time.Time) ([]model.Batch, error) {
	open, err := store.ListBatches(ctx, model.BatchOpen)
	if err != nil {
		return nil, err
	}
	var pending []model.Batch
	for i := range open {
		b := &open[i]
		switch {
		case now.Sub(b.LastEventAt) > pol.IdleGap:
			if err := Close(ctx, store, b, pol, TriggerIdle, b.LastEventAt); err != nil {
				return nil, err
			}
		case pol.Timer > 0 && b.MeaningfulCount >= pol.MinMeaningful:
			first, err := firstMeaningful(ctx, store, b.ID)
			if err != nil || first.IsZero() || now.Sub(first) < pol.Timer {
				continue
			}
			if !cooledDown(ctx, store, b.SessionID, pol.MinInterval, now) {
				continue
			}
			if err := Close(ctx, store, b, pol, TriggerTimer, now); err != nil {
				return nil, err
			}
			_ = store.SetKV(ctx, "last_summary:"+b.SessionID, fmt.Sprint(now.UnixNano()))
		default:
			continue
		}
		if b.Status == model.BatchPending {
			pending = append(pending, *b)
		}
	}
	return pending, nil
}

// CloseAll forces every open batch (or one session's) to close now.
func CloseAll(ctx context.Context, store *journal.Store, pol Policy, sessionID string, now time.Time) ([]model.Batch, error) {
	open, err := store.ListBatches(ctx, model.BatchOpen)
	if err != nil {
		return nil, err
	}
	var pending []model.Batch
	for i := range open {
		b := &open[i]
		if sessionID != "" && b.SessionID != sessionID {
			continue
		}
		if err := Close(ctx, store, b, pol, TriggerManual, now); err != nil {
			return nil, err
		}
		if b.Status == model.BatchPending {
			pending = append(pending, *b)
		}
	}
	return pending, nil
}

func firstMeaningful(ctx context.Context, store *journal.Store, batchID string) (time.Time, error) {
	evs, err := store.ListEvents(ctx, journal.EventQuery{BatchID: batchID, Kind: model.KindMeaningful, Limit: 1})
	if err != nil || len(evs) == 0 {
		return time.Time{}, err
	}
	return evs[0].Start, nil
}

func cooledDown(ctx context.Context, store *journal.Store, sessionID string, min time.Duration, now time.Time) bool {
	if min <= 0 {
		return true
	}
	v, err := store.GetKV(ctx, "last_summary:"+sessionID)
	if err != nil {
		return true
	}
	var ns int64
	if _, err := fmt.Sscan(v, &ns); err != nil {
		return true
	}
	return now.Sub(time.Unix(0, ns)) >= min
}
