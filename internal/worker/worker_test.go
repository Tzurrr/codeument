package worker

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	docsfake "github.com/Tzurrr/codeument/internal/docs/fake"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/journal"
	llmfake "github.com/Tzurrr/codeument/internal/llm/fake"
	"github.com/Tzurrr/codeument/internal/model"
)

func setup(t *testing.T) (*Worker, *journal.Store, *docsfake.Provider, string) {
	t.Helper()
	s, err := journal.OpenMemory()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	dp := docsfake.New()
	notifyPath := filepath.Join(t.TempDir(), "notify")
	w := &Worker{Store: s, Engine: &engine.Local{LLM: llmfake.New(), Docs: dp}, Hostname: "web-01", Username: "alice", NotifyPath: notifyPath, Location: model.Location{Space: "OPS", ParentPath: []string{"Runbooks"}}}
	return w, s, dp, notifyPath
}

func seedBatch(t *testing.T, s *journal.Store, id string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now()
	b := &model.Batch{ID: id, SessionID: "s", StartedAt: now, LastEventAt: now, Status: model.BatchPending, TriggerReason: "score"}
	if err := s.CreateBatch(ctx, b); err != nil {
		t.Fatal(err)
	}
	for i, cmd := range []string{"vim /etc/nginx/nginx.conf", "nginx -t", "sudo systemctl reload nginx"} {
		ev := &model.Event{SessionID: "s", BatchID: id, Start: now.Add(time.Duration(i) * time.Minute), End: now.Add(time.Duration(i) * time.Minute), Command: cmd, Kind: model.KindMeaningful, Weight: 3, Hostname: "web-01", Shell: "zsh", CWD: "/etc/nginx"}
		if err := s.InsertEvent(ctx, ev); err != nil {
			t.Fatal(err)
		}
	}
}

func TestProcessPendingAndPublish(t *testing.T) {
	w, s, dp, notifyPath := setup(t)
	ctx := context.Background()
	seedBatch(t, s, "b1")

	n, err := w.ProcessPending(ctx)
	if err != nil || n != 1 {
		t.Fatalf("process: %d %v", n, err)
	}
	drafts, _ := s.ListDrafts(ctx, model.DraftStatusDraft)
	if len(drafts) != 1 {
		t.Fatalf("drafts = %d", len(drafts))
	}
	d := drafts[0]
	if d.BatchID != "b1" || !strings.Contains(d.BodyMD, "systemctl reload nginx") || d.Location.Space != "OPS" {
		t.Fatalf("draft = %+v", d)
	}
	b, _ := s.GetBatch(ctx, "b1")
	if b.Status != model.BatchSummarized {
		t.Fatalf("batch status = %s", b.Status)
	}
	data, err := os.ReadFile(notifyPath)
	if err != nil || !strings.Contains(string(data), "1 draft ready") {
		t.Fatalf("notify = %q %v", data, err)
	}

	ref, err := w.Publish(ctx, &d)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "fake" || ref.Version != 1 {
		t.Fatalf("ref = %+v", ref)
	}
	got, _ := s.GetDraft(ctx, d.ID)
	if got.Status != model.DraftStatusPublished || got.DocID == "" || got.PublishedAt == nil {
		t.Fatalf("draft after publish = %+v", got)
	}
	page, _ := s.GetDocPage(ctx, got.DocID)
	if page == nil || page.ProviderPageID != ref.ProviderID {
		t.Fatalf("doc page mapping missing: %+v", page)
	}
	if _, err := os.Stat(notifyPath); err == nil {
		t.Fatal("notify file should be cleared once no drafts remain")
	}

	// A second draft pointed at the same doc id is appended to the page.
	seedBatch(t, s, "b2")
	if _, err := w.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	drafts, _ = s.ListDrafts(ctx, model.DraftStatusDraft)
	d2 := drafts[0]
	d2.DocID = got.DocID
	if _, err := w.Publish(ctx, &d2); err != nil {
		t.Fatal(err)
	}
	merged := dp.Pages[got.DocID]
	if !strings.Contains(merged.BodyMD, "## "+d2.Title) || strings.Count(merged.BodyMD, "codeument-draft:") != 2 {
		t.Fatalf("merged body:\n%s", merged.BodyMD)
	}
	if len(dp.IDs()) != 1 {
		t.Fatalf("expected a single page, got %v", dp.IDs())
	}
}

func TestRegenerateAndDismiss(t *testing.T) {
	w, s, _, _ := setup(t)
	ctx := context.Background()
	seedBatch(t, s, "b1")
	if _, err := w.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	drafts, _ := s.ListDrafts(ctx, model.DraftStatusDraft)
	d := drafts[0]
	if _, err := w.Regenerate(ctx, &d, "shorter"); err != nil {
		t.Fatal(err)
	}
	fp := w.Engine.(*engine.Local).LLM.(*llmfake.Provider)
	last := fp.Requests[len(fp.Requests)-1]
	if !strings.Contains(last.User, "## Instruction\nshorter") || !strings.Contains(last.User, "Previous draft") {
		t.Fatalf("regenerate prompt:\n%s", last.User)
	}
	if err := w.Dismiss(ctx, &d); err != nil {
		t.Fatal(err)
	}
	if got, _ := s.GetDraft(ctx, d.ID); got.Status != model.DraftStatusDismissed {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestRetryQueue(t *testing.T) {
	w, s, dp, _ := setup(t)
	ctx := context.Background()
	seedBatch(t, s, "b1")
	if _, err := w.ProcessPending(ctx); err != nil {
		t.Fatal(err)
	}
	drafts, _ := s.ListDrafts(ctx, model.DraftStatusDraft)
	d := drafts[0]
	dp.Err = os.ErrPermission
	if _, err := w.Publish(ctx, &d); err == nil {
		t.Fatal("expected publish failure")
	}
	if err := w.QueuePublish(ctx, &d, os.ErrPermission); err != nil {
		t.Fatal(err)
	}
	items, _ := s.DueQueue(ctx, time.Now().Add(time.Hour))
	if len(items) != 1 {
		t.Fatalf("queue = %+v", items)
	}
	dp.Err = nil
	// Force the item due now.
	_ = s.RequeueItem(ctx, items[0].ID, time.Now().Add(-time.Second), "x")
	n, err := w.RetryQueue(ctx)
	if err != nil || n != 1 {
		t.Fatalf("retry: %d %v", n, err)
	}
	if got, _ := s.GetDraft(ctx, d.ID); got.Status != model.DraftStatusPublished {
		t.Fatalf("status = %s", got.Status)
	}
}

func TestTryLock(t *testing.T) {
	p := filepath.Join(t.TempDir(), "lock")
	l, ok, err := TryLock(p)
	if err != nil || !ok {
		t.Fatal(err, ok)
	}
	if _, ok2, _ := TryLock(p); ok2 {
		t.Fatal("second lock should fail")
	}
	l.Unlock()
	if _, ok3, _ := TryLock(p); !ok3 {
		t.Fatal("lock should be free after unlock")
	}
}
