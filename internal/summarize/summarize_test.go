package summarize

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/Tzurrr/codeument/internal/llm"
	"github.com/Tzurrr/codeument/internal/llm/fake"
	"github.com/Tzurrr/codeument/internal/model"
)

var update = flag.Bool("update", false, "rewrite golden files")

func sampleInput() Input {
	base := time.Date(2026, 9, 8, 10, 0, 0, 0, time.UTC)
	evs := []model.Event{
		{Seq: 1, Start: base, End: base.Add(time.Second), Command: "vim /etc/nginx/nginx.conf", ExitCode: 0, CWD: "/etc/nginx", Hostname: "web-01", Username: "alice", Shell: "zsh", Kind: model.KindMeaningful, Family: "config", Weight: 3, FilesTouched: []string{"/etc/nginx/nginx.conf"}},
		{Seq: 2, Start: base.Add(time.Minute), End: base.Add(time.Minute + 200*time.Millisecond), Command: "nginx -t", ExitCode: 1, CWD: "/etc/nginx", Hostname: "web-01", Username: "alice", Shell: "zsh", Kind: model.KindMeaningful, Family: "config", Weight: 2},
		{Seq: 3, Start: base.Add(2 * time.Minute), End: base.Add(2*time.Minute + 100*time.Millisecond), Command: "sudo systemctl reload nginx", ExitCode: 0, CWD: "/etc/nginx", Hostname: "web-01", Username: "alice", Shell: "zsh", Kind: model.KindMeaningful, Family: "service", Weight: 5, GitRoot: "/srv/app", GitBranch: "main"},
	}
	return Input{
		Batch:           model.Batch{ID: "b1", SessionID: "sess-1", Events: evs},
		Hostname:        "web-01",
		Username:        "alice",
		RecentDocs:      []DocRef{{ID: "doc-9", Title: "nginx tuning"}},
		Hints:           []string{"Team pages live under OPS / Runbooks"},
		DefaultLocation: model.Location{Space: "OPS", ParentPath: []string{"Runbooks"}},
	}
}

func TestBuildPromptGolden(t *testing.T) {
	system, user, hash, err := BuildPrompt(sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" || len(hash) != 16 {
		t.Fatalf("hash = %q", hash)
	}
	got := "=== system ===\n" + system + "\n=== user ===\n" + user
	golden := filepath.Join("testdata", "prompt.golden")
	if *update {
		_ = os.MkdirAll("testdata", 0o755)
		if err := os.WriteFile(golden, []byte(got), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	want, err := os.ReadFile(golden)
	if err != nil {
		t.Fatalf("missing golden file (run with -update): %v", err)
	}
	if string(want) != got {
		t.Fatalf("prompt differs from golden:\n%s", got)
	}
}

func TestSummarizeWithFake(t *testing.T) {
	p := fake.New()
	out, err := Summarize(context.Background(), p, sampleInput())
	if err != nil {
		t.Fatal(err)
	}
	if out.Draft.Title == "" || len(out.Draft.Commands) != 3 || out.Draft.SuggestedLocation.Space != "OPS" {
		t.Fatalf("draft = %+v", out.Draft)
	}
	if out.Provider != "fake" || out.PromptHash == "" {
		t.Fatalf("out = %+v", out)
	}
	md := RenderMarkdown(out.Draft, sampleInput().Batch.Events)
	if !strings.Contains(md, "## Steps") || !strings.Contains(md, "sudo systemctl reload nginx") || !strings.Contains(md, "Recorded by codeument on web-01") {
		t.Fatalf("markdown:\n%s", md)
	}
}

func TestSummarizeRetriesOnBadOutput(t *testing.T) {
	p := fake.New()
	calls := 0
	p.Responder = func(req llm.Request) (json.RawMessage, error) {
		calls++
		if calls == 1 {
			return json.RawMessage(`{"title":"","summary":"x"}`), nil
		}
		if !strings.Contains(req.User, "rejected") {
			t.Fatalf("retry prompt lacks the validation error")
		}
		return json.RawMessage(`{"title":"ok","summary":"done","doc_kind":"runbook","confidence":"high"}`), nil
	}
	out, err := Summarize(context.Background(), p, sampleInput())
	if err != nil || out.Draft.Title != "ok" || calls != 2 {
		t.Fatalf("err=%v calls=%d out=%+v", err, calls, out)
	}

	p.Responder = func(llm.Request) (json.RawMessage, error) { return nil, llm.ErrRefused }
	if _, err := Summarize(context.Background(), p, sampleInput()); !errors.Is(err, llm.ErrRefused) {
		t.Fatalf("expected refusal to propagate, got %v", err)
	}
}

func TestValidate(t *testing.T) {
	d := &model.Draft{Title: "t", Summary: "s", DocKind: "weird"}
	if err := Validate(d); err == nil {
		t.Fatal("expected doc_kind error")
	}
	d = &model.Draft{Title: "has <redacted:token>", Summary: "s"}
	if err := Validate(d); err == nil {
		t.Fatal("expected marker error")
	}
	d = &model.Draft{Title: "t", Summary: "s"}
	if err := Validate(d); err != nil || d.DocKind != "note" || d.Confidence != "medium" {
		t.Fatalf("defaults: %v %+v", err, d)
	}
}
