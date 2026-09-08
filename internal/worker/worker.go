// Package worker drives batches through summarization into drafts, and
// drafts through review into published documents. It is used by the
// detached `codeument work` process, `codeument now`, `tick`, the TUI and
// the drafts CLI.
package worker

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/llm"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/notify"
	"github.com/Tzurrr/codeument/internal/summarize"
)

// Worker holds the dependencies.
type Worker struct {
	Store      *journal.Store
	Engine     engine.Engine
	Hostname   string
	Username   string
	NotifyPath string
	Hints      []string
	Location   model.Location
	Log        *slog.Logger
	// MaxAttempts before a batch is marked failed.
	MaxAttempts int
}

func (w *Worker) log() *slog.Logger {
	if w.Log == nil {
		return slog.Default()
	}
	return w.Log
}

// ProcessPending summarizes every pending batch and returns how many drafts
// were created.
func (w *Worker) ProcessPending(ctx context.Context) (int, error) {
	pending, err := w.Store.ListBatches(ctx, model.BatchPending, model.BatchFailed)
	if err != nil {
		return 0, err
	}
	created := 0
	var firstErr error
	for i := range pending {
		b := &pending[i]
		if b.Status == model.BatchFailed && b.Attempts >= w.maxAttempts() {
			continue
		}
		if _, err := w.SummarizeBatch(ctx, b, ""); err != nil {
			w.log().Warn("summarize batch", "batch", b.ID, "err", err)
			if firstErr == nil {
				firstErr = err
			}
			if errors.Is(err, llm.ErrUnavailable) || errors.Is(err, llm.ErrRateLimited) {
				break // no point hammering an unreachable provider
			}
			continue
		}
		created++
	}
	if err := w.UpdateNotify(ctx); err != nil {
		w.log().Warn("notify", "err", err)
	}
	return created, firstErr
}

func (w *Worker) maxAttempts() int {
	if w.MaxAttempts <= 0 {
		return 3
	}
	return w.MaxAttempts
}

// SummarizeBatch turns one batch into a draft.
func (w *Worker) SummarizeBatch(ctx context.Context, b *model.Batch, note string) (*model.DraftRecord, error) {
	evs, err := w.Store.BatchEvents(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		b.Status = model.BatchSkipped
		b.Error = "no events"
		return nil, w.Store.UpdateBatch(ctx, b)
	}
	b.Status = model.BatchSummarizing
	b.Attempts++
	if err := w.Store.UpdateBatch(ctx, b); err != nil {
		return nil, err
	}
	in := w.buildInput(ctx, *b, evs, note)
	out, err := w.Engine.Summarize(ctx, in)
	if err != nil {
		b.Status = model.BatchFailed
		b.Error = err.Error()
		if errors.Is(err, llm.ErrRefused) {
			b.Attempts = w.maxAttempts()
		}
		_ = w.Store.UpdateBatch(ctx, b)
		return nil, err
	}
	rec := &model.DraftRecord{
		ID: uuid.New().String(), BatchID: b.ID, Status: model.DraftStatusDraft,
		Title: out.Draft.Title, BodyMD: summarize.RenderMarkdown(out.Draft, evs), Draft: out.Draft,
		Location: out.Draft.SuggestedLocation, Tags: out.Draft.Tags, DocID: out.Draft.RelatedDocID,
		Provider: out.Provider, Model: out.Model, PromptHash: out.PromptHash,
		UsageIn: out.Usage.InputTokens, UsageOut: out.Usage.OutputTokens, Hostname: w.Hostname,
	}
	if err := w.Store.SaveDraft(ctx, rec); err != nil {
		return nil, err
	}
	b.Status = model.BatchSummarized
	b.Error = ""
	if err := w.Store.UpdateBatch(ctx, b); err != nil {
		return nil, err
	}
	return rec, nil
}

// Regenerate re-runs the LLM for an existing draft with an instruction.
func (w *Worker) Regenerate(ctx context.Context, d *model.DraftRecord, instruction string) (*model.DraftRecord, error) {
	b, err := w.Store.GetBatch(ctx, d.BatchID)
	if err != nil {
		return nil, fmt.Errorf("batch %s: %w", d.BatchID, err)
	}
	evs, err := w.Store.BatchEvents(ctx, b.ID)
	if err != nil {
		return nil, err
	}
	if len(evs) == 0 {
		return nil, errors.New("the batch's events were forgotten; cannot regenerate")
	}
	in := w.buildInput(ctx, *b, evs, "")
	prev := d.Draft
	in.PreviousDraft = &prev
	in.Instruction = instruction
	out, err := w.Engine.Summarize(ctx, in)
	if err != nil {
		return nil, err
	}
	d.Title = out.Draft.Title
	d.Draft = out.Draft
	d.BodyMD = summarize.RenderMarkdown(out.Draft, evs)
	d.Location = out.Draft.SuggestedLocation
	d.Tags = out.Draft.Tags
	d.Provider, d.Model, d.PromptHash = out.Provider, out.Model, out.PromptHash
	d.UsageIn += out.Usage.InputTokens
	d.UsageOut += out.Usage.OutputTokens
	d.Status = model.DraftStatusDraft
	return d, w.Store.SaveDraft(ctx, d)
}

func (w *Worker) buildInput(ctx context.Context, b model.Batch, evs []model.Event, note string) summarize.Input {
	b.Events = evs
	in := summarize.Input{Batch: b, Hostname: w.Hostname, Username: w.Username, Hints: w.Hints, Note: note, DefaultLocation: w.Location}
	if recent, err := w.Store.RecentDocTitles(ctx, 10); err == nil {
		for _, r := range recent {
			id := r.DocID
			if id == "" {
				continue
			}
			in.RecentDocs = append(in.RecentDocs, summarize.DocRef{ID: id, Title: r.Title})
		}
	}
	in.GitLog, in.GitDiffStat = summarize.GitContext(ctx, evs)
	return in
}

// Accept marks a draft accepted, assigning a doc id when it has none.
func (w *Worker) Accept(ctx context.Context, d *model.DraftRecord) error {
	if d.DocID == "" {
		d.DocID = uuid.New().String()
	}
	d.Status = model.DraftStatusAccepted
	return w.Store.SaveDraft(ctx, d)
}

// Dismiss marks a draft dismissed.
func (w *Worker) Dismiss(ctx context.Context, d *model.DraftRecord) error {
	d.Status = model.DraftStatusDismissed
	return w.Store.SaveDraft(ctx, d)
}

// Publish accepts (if needed) and publishes a draft. When the draft's doc id
// already exists on the platform, the draft is appended to that page as a
// dated section instead of replacing it.
func (w *Worker) Publish(ctx context.Context, d *model.DraftRecord) (docs.PageRef, error) {
	if d.Status != model.DraftStatusAccepted && d.Status != model.DraftStatusPublished {
		if err := w.Accept(ctx, d); err != nil {
			return docs.PageRef{}, err
		}
	}
	doc := w.Document(d)
	if existing, err := w.Engine.FindDoc(ctx, d.DocID); err == nil && existing != nil {
		if prev, err := w.Engine.GetDoc(ctx, *existing); err == nil && prev != nil && prev.BodyMD != "" && !strings.Contains(prev.BodyMD, d.ID) {
			doc.Title = prev.Title
			if prev.Title == "" {
				doc.Title = d.Title
			}
			doc.BodyMD = strings.TrimRight(prev.BodyMD, "\n") + "\n\n## " + d.Title + " (" + time.Now().Format("2006-01-02") + ")\n\n" + d.BodyMD + "\n<!-- codeument-draft:" + d.ID + " -->\n"
			doc.Location = prev.Location
			if doc.Location.Space == "" {
				doc.Location = d.Location
			}
			doc.Labels = union(prev.Labels, doc.Labels)
		}
	}
	ref, err := w.Engine.Publish(ctx, doc)
	if err != nil {
		return docs.PageRef{}, err
	}
	now := time.Now()
	d.Status = model.DraftStatusPublished
	d.PublishedAt = &now
	d.PageRef = mustJSON(ref)
	if err := w.Store.SaveDraft(ctx, d); err != nil {
		return ref, err
	}
	_ = w.Store.SaveDocPage(ctx, journal.DocPage{DocID: d.DocID, Provider: ref.Provider, ProviderPageID: ref.ProviderID, URL: ref.URL, Version: ref.Version, ContentHash: doc.ContentHash(), Title: doc.Title})
	_ = w.UpdateNotify(ctx)
	return ref, nil
}

// Document builds the canonical document for a draft.
func (w *Worker) Document(d *model.DraftRecord) docs.Document {
	labels := append([]string{"codeument"}, d.Tags...)
	if d.Draft.DocKind != "" {
		labels = append(labels, d.Draft.DocKind)
	}
	loc := d.Location
	if loc.Space == "" {
		loc = w.Location
	}
	return docs.Document{
		ID: d.DocID, Title: d.Title, BodyMD: d.BodyMD + "\n<!-- codeument-draft:" + d.ID + " -->\n", Location: loc, Labels: dedupe(labels),
		Meta: map[string]string{"hostname": d.Hostname, "kind": "draft", "draft_id": d.ID, "batch_id": d.BatchID, "doc_kind": d.Draft.DocKind},
	}
}

// QueuePublish schedules a retry after a failed publish.
func (w *Worker) QueuePublish(ctx context.Context, d *model.DraftRecord, cause error) error {
	return w.Store.Enqueue(ctx, d.ID, time.Now().Add(5*time.Minute), cause.Error())
}

// RetryQueue publishes drafts whose retry time has come.
func (w *Worker) RetryQueue(ctx context.Context) (int, error) {
	items, err := w.Store.DueQueue(ctx, time.Now())
	if err != nil {
		return 0, err
	}
	done := 0
	for _, it := range items {
		d, err := w.Store.GetDraft(ctx, it.DraftID)
		if err != nil || d.Status == model.DraftStatusDismissed || d.Status == model.DraftStatusPublished {
			_ = w.Store.DequeueItem(ctx, it.ID)
			continue
		}
		if _, err := w.Publish(ctx, d); err != nil {
			backoff := time.Duration(1<<minInt(it.Attempts, 6)) * 5 * time.Minute
			_ = w.Store.RequeueItem(ctx, it.ID, time.Now().Add(backoff), err.Error())
			continue
		}
		_ = w.Store.DequeueItem(ctx, it.ID)
		done++
	}
	return done, nil
}

// UpdateNotify refreshes the prompt-time notice from the draft count.
func (w *Worker) UpdateNotify(ctx context.Context) error {
	if w.NotifyPath == "" {
		return nil
	}
	drafts, err := w.Store.ListDrafts(ctx, model.DraftStatusDraft)
	if err != nil {
		return err
	}
	return notify.Set(w.NotifyPath, notify.DraftsReady(len(drafts)))
}

func union(a, b []string) []string { return dedupe(append(append([]string{}, a...), b...)) }

func dedupe(in []string) []string {
	seen := map[string]bool{}
	var out []string
	for _, s := range in {
		s = strings.TrimSpace(s)
		if s == "" || seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}
