package journal

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/Tzurrr/codeument/internal/model"
)

func TestOpenMigrateAndEvents(t *testing.T) {
	ctx := context.Background()
	s, err := Open(filepath.Join(t.TempDir(), "j.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()

	if v, err := s.GetKV(ctx, "schema_version"); err != nil || v != "1" {
		t.Fatalf("schema_version = %q, %v", v, err)
	}

	now := time.Now()
	if err := s.UpsertSession(ctx, model.Session{ID: "s1", StartedAt: now, Hostname: "h", Username: "u", Shell: "bash"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertSession(ctx, model.Session{ID: "s1", StartedAt: now}); err != nil {
		t.Fatal("second upsert must be a no-op:", err)
	}

	for i, cmd := range []string{"ls", "git commit -m x", "systemctl restart nginx"} {
		ev := &model.Event{SessionID: "s1", Start: now.Add(time.Duration(i) * time.Second), End: now.Add(time.Duration(i)*time.Second + 50*time.Millisecond),
			Command: cmd, Kind: model.KindMeaningful, Weight: 2, FilesTouched: []string{"/etc/x"}, Redactions: []string{"token"}, BatchID: "b1"}
		if i == 0 {
			ev.Kind = model.KindNoise
			ev.Weight = 0
		}
		if err := s.InsertEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
		if ev.Seq != i+1 || ev.ID == 0 {
			t.Fatalf("seq/id not assigned: %+v", ev)
		}
	}
	evs, err := s.ListEvents(ctx, EventQuery{SessionID: "s1"})
	if err != nil || len(evs) != 3 {
		t.Fatalf("list: %v %d", err, len(evs))
	}
	if evs[1].Command != "git commit -m x" || evs[1].FilesTouched[0] != "/etc/x" || evs[1].Redactions[0] != "token" {
		t.Fatalf("round trip lost data: %+v", evs[1])
	}
	if evs[1].Duration() != 50*time.Millisecond {
		t.Fatalf("duration = %v", evs[1].Duration())
	}
	n, err := s.CountEvents(ctx, EventQuery{Kind: model.KindMeaningful})
	if err != nil || n != 2 {
		t.Fatalf("count meaningful = %d %v", n, err)
	}

	b := &model.Batch{ID: "b1", SessionID: "s1", StartedAt: now, LastEventAt: now, Status: model.BatchOpen, EventCount: 3, MeaningfulCount: 2, Score: 4}
	if err := s.CreateBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	got, err := s.OpenBatch(ctx, "s1")
	if err != nil || got.ID != "b1" {
		t.Fatalf("open batch: %v %+v", err, got)
	}
	if _, err := s.OpenBatch(ctx, "nope"); err != ErrNotFound {
		t.Fatalf("expected ErrNotFound, got %v", err)
	}

	d := &model.DraftRecord{ID: "d1", BatchID: "b1", Status: model.DraftStatusDraft, Title: "T", BodyMD: "body", Draft: model.Draft{Title: "T", Confidence: "high"}}
	if err := s.SaveDraft(ctx, d); err != nil {
		t.Fatal(err)
	}

	deleted, err := s.DeleteLastEvents(ctx, 1)
	if err != nil || deleted != 1 {
		t.Fatalf("delete last: %d %v", deleted, err)
	}
	dr, err := s.GetDraft(ctx, "d1")
	if err != nil {
		t.Fatal(err)
	}
	if dr.Status != model.DraftStatusStale {
		t.Fatalf("draft should be stale after forget, got %s", dr.Status)
	}

	deleted, err = s.DeleteEvents(ctx, EventQuery{SessionID: "s1"})
	if err != nil || deleted != 2 {
		t.Fatalf("delete session: %d %v", deleted, err)
	}
	if _, err := s.GetBatch(ctx, "b1"); err != ErrNotFound {
		t.Fatalf("empty open batch should be removed, got %v", err)
	}

	sessions, err := s.ListSessions(ctx, 10)
	if err != nil || len(sessions) != 1 {
		t.Fatalf("sessions: %v %d", err, len(sessions))
	}
	st, err := s.Stats(ctx)
	if err != nil || st.Sessions != 1 || st.Events != 0 {
		t.Fatalf("stats: %+v %v", st, err)
	}
}

func TestKVAndDocPages(t *testing.T) {
	ctx := context.Background()
	s, err := OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	if err := s.SetKV(ctx, "a", "1"); err != nil {
		t.Fatal(err)
	}
	if v, _ := s.GetKV(ctx, "a"); v != "1" {
		t.Fatal("kv")
	}
	if err := s.SaveDocPage(ctx, DocPage{DocID: "doc1", Provider: "markdown", ProviderPageID: "x.md", Version: 1}); err != nil {
		t.Fatal(err)
	}
	if err := s.SaveDocPage(ctx, DocPage{DocID: "doc1", Provider: "markdown", ProviderPageID: "x.md", Version: 2}); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetDocPage(ctx, "doc1")
	if err != nil || p.Version != 2 {
		t.Fatalf("doc page: %+v %v", p, err)
	}
}
