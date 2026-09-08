package batch

import (
	"context"
	"testing"
	"time"

	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/model"
)

func testPolicy() Policy {
	return Policy{IdleGap: 20 * time.Minute, ScoreThreshold: 8, MeaningfulCount: 6, Timer: 45 * time.Minute, MinInterval: 10 * time.Minute, Milestones: true, MinMeaningful: 2, MaxEvents: 200}
}

func observe(t *testing.T, s *journal.Store, sess, cmd string, kind model.Kind, weight int, milestone bool, family string, at time.Time) Decision {
	t.Helper()
	ev := &model.Event{SessionID: sess, Start: at, End: at, Command: cmd, Kind: kind, Weight: weight, Milestone: milestone, Family: family}
	dec, err := Observe(context.Background(), s, ev, testPolicy(), at)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.InsertEvent(context.Background(), ev); err != nil {
		t.Fatal(err)
	}
	return dec
}

func TestScoreTrigger(t *testing.T) {
	s, _ := journal.OpenMemory()
	defer s.Close()
	now := time.Now()
	d := observe(t, s, "s", "ls", model.KindNoise, 0, false, "noise", now)
	if d.Trigger != "" || d.Batch.EventCount != 1 {
		t.Fatalf("noise should not trigger: %+v", d)
	}
	d = observe(t, s, "s", "apt install x", model.KindMeaningful, 3, false, "package", now.Add(time.Minute))
	if d.Trigger != "" {
		t.Fatalf("one meaningful event must not trigger: %+v", d)
	}
	d = observe(t, s, "s", "systemctl restart x", model.KindMeaningful, 5, true, "service", now.Add(2*time.Minute))
	if d.Trigger != TriggerScore {
		t.Fatalf("expected score trigger, got %q", d.Trigger)
	}
	if d.Batch.Status != model.BatchPending {
		t.Fatalf("status = %s", d.Batch.Status)
	}
	// The next event starts a fresh batch and the cool-down blocks a new trigger.
	d = observe(t, s, "s", "git push", model.KindMeaningful, 5, true, "git", now.Add(3*time.Minute))
	if d.Batch.Status != model.BatchOpen || d.Batch.EventCount != 1 {
		t.Fatalf("expected new open batch: %+v", d.Batch)
	}
	d = observe(t, s, "s", "git push", model.KindMeaningful, 5, true, "git", now.Add(4*time.Minute))
	if d.Trigger != "" {
		t.Fatalf("cool-down should block trigger, got %q", d.Trigger)
	}
	d = observe(t, s, "s", "git push", model.KindMeaningful, 5, true, "git", now.Add(15*time.Minute))
	if d.Trigger != TriggerScore {
		t.Fatalf("after cool-down expected trigger, got %q", d.Trigger)
	}
}

func TestMilestoneAndIdle(t *testing.T) {
	s, _ := journal.OpenMemory()
	defer s.Close()
	now := time.Now()
	observe(t, s, "s", "vim main.go", model.KindMeaningful, 2, false, "edit", now)
	d := observe(t, s, "s", "git commit -m x", model.KindMeaningful, 5, true, "git", now.Add(time.Minute))
	if d.Trigger != TriggerMilestone {
		t.Fatalf("expected milestone trigger, got %q (score %d)", d.Trigger, d.Batch.Score)
	}
	// Idle gap closes an open batch with too little work as skipped.
	d = observe(t, s, "s2", "make", model.KindMeaningful, 2, false, "build", now)
	first := d.Batch.ID
	d = observe(t, s, "s2", "make", model.KindMeaningful, 2, false, "build", now.Add(time.Hour))
	if d.Closed == nil || d.Closed.ID != first || d.Closed.Status != model.BatchSkipped {
		t.Fatalf("expected first batch skipped on idle: %+v", d.Closed)
	}
	if d.Batch.ID == first {
		t.Fatal("expected a new batch after idle gap")
	}
}

func TestConfigRestartAndCloseStale(t *testing.T) {
	s, _ := journal.OpenMemory()
	defer s.Close()
	now := time.Now()
	observe(t, s, "s", "vim /etc/nginx/nginx.conf", model.KindMeaningful, 3, false, "config", now)
	d := observe(t, s, "s", "systemctl reload nginx", model.KindMeaningful, 4, false, "service", now.Add(time.Minute))
	if d.Trigger != TriggerConfigRestart {
		t.Fatalf("expected config_restart, got %q", d.Trigger)
	}

	observe(t, s, "s3", "make", model.KindMeaningful, 2, false, "build", now)
	observe(t, s, "s3", "make test", model.KindMeaningful, 2, false, "build", now.Add(time.Minute))
	pending, err := CloseStale(context.Background(), s, testPolicy(), now.Add(2*time.Minute))
	if err != nil || len(pending) != 0 {
		t.Fatalf("nothing stale yet: %v %d", err, len(pending))
	}
	pending, err = CloseStale(context.Background(), s, testPolicy(), now.Add(time.Hour))
	if err != nil || len(pending) != 1 || pending[0].TriggerReason != TriggerIdle {
		t.Fatalf("expected one idle-closed pending batch: %v %+v", err, pending)
	}

	observe(t, s, "s4", "make", model.KindMeaningful, 2, false, "build", now)
	observe(t, s, "s4", "make", model.KindMeaningful, 2, false, "build", now.Add(time.Minute))
	pending, _ = CloseAll(context.Background(), s, testPolicy(), "s4", now.Add(2*time.Minute))
	if len(pending) != 1 || pending[0].TriggerReason != TriggerManual {
		t.Fatalf("CloseAll: %+v", pending)
	}
}
