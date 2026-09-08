package scheduler

import (
	"context"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/Tzurrr/codeument/internal/batch"
	docsfake "github.com/Tzurrr/codeument/internal/docs/fake"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/journal"
	llmfake "github.com/Tzurrr/codeument/internal/llm/fake"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/worker"
)

func TestTickClosesAndSummarizes(t *testing.T) {
	s, _ := journal.OpenMemory()
	defer s.Close()
	ctx := context.Background()
	old := time.Now().Add(-2 * time.Hour)
	b := &model.Batch{ID: "b1", SessionID: "s", StartedAt: old, LastEventAt: old, Status: model.BatchOpen, MeaningfulCount: 3, EventCount: 3, Score: 6}
	_ = s.CreateBatch(ctx, b)
	for i := 0; i < 3; i++ {
		_ = s.InsertEvent(ctx, &model.Event{SessionID: "s", BatchID: "b1", Start: old, End: old, Command: "make", Kind: model.KindMeaningful, Weight: 2})
	}
	w := &worker.Worker{Store: s, Engine: &engine.Local{LLM: llmfake.New(), Docs: docsfake.New()}}
	pol := batch.Policy{IdleGap: 20 * time.Minute, MinMeaningful: 2, ScoreThreshold: 100, MeaningfulCount: 100}
	rep := Tick(ctx, s, w, TickOptions{Policy: pol, Summarize: true})
	if len(rep.Errors) != 0 || rep.ClosedBatches != 1 || rep.Drafts != 1 {
		t.Fatalf("report = %+v", rep)
	}
	snapCalled := false
	rep = Tick(ctx, s, nil, TickOptions{Policy: pol, Hooks: Hooks{Snapshot: func(context.Context) error { snapCalled = true; return nil }}})
	if !snapCalled || rep.ClosedBatches != 0 {
		t.Fatalf("second tick: %+v called=%v", rep, snapCalled)
	}
}

func TestInstallDryRun(t *testing.T) {
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" {
		t.Skip()
	}
	out, err := Install(InstallOptions{Binary: "/usr/local/bin/codeument", Scope: "user", DryRun: true})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Files) == 0 || len(out.Commands) == 0 {
		t.Fatalf("out = %+v", out)
	}
	svc, err := render("codeument.service", unitData{Binary: "/x/codeument", ConfigFlag: " --config /c"})
	if err != nil || !strings.Contains(string(svc), "/x/codeument tick --quiet --config /c") {
		t.Fatalf("render: %v %s", err, svc)
	}
}
