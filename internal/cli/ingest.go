package cli

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/batch"
	"github.com/Tzurrr/codeument/internal/capture"
	"github.com/Tzurrr/codeument/internal/classify"
	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/redact"
)

func (a *App) ingestCmd() *cobra.Command {
	var sessionEnd bool
	cmd := &cobra.Command{
		Use:    "ingest",
		Short:  "Record one command from the shell hook (reads a record on stdin)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			a.quiet = true
			rec, err := capture.ParseRecord(cmd.InOrStdin())
			if err != nil {
				return err
			}
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			store, err := a.openStore(cmd)
			if err != nil {
				return err
			}
			if sessionEnd {
				return a.endSession(cmd, store, cfg, rec)
			}
			if paused(cfg) {
				return nil
			}
			return a.ingest(cmd, store, cfg, rec)
		},
	}
	cmd.Flags().BoolVar(&sessionEnd, "session-end", false, "the shell is exiting")
	return cmd
}

// paused reports whether capture is paused; an expired pause file is removed.
func paused(cfg *config.Config) bool {
	data, err := os.ReadFile(cfg.PausePath())
	if err != nil {
		return false
	}
	until, err := strconv.ParseInt(strings.TrimSpace(string(data)), 10, 64)
	if err != nil || until == 0 {
		return true // indefinite
	}
	if time.Now().Unix() >= until {
		_ = os.Remove(cfg.PausePath())
		return false
	}
	return true
}

func (a *App) endSession(cmd *cobra.Command, store *journal.Store, cfg *config.Config, rec capture.Record) error {
	c := ctx(cmd)
	if err := store.EndSession(c, rec.SessionID, rec.End); err != nil {
		return err
	}
	b, err := store.OpenBatch(c, rec.SessionID)
	if errors.Is(err, journal.ErrNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := batch.Close(c, store, b, policyFrom(cfg), batch.TriggerSessionEnd, rec.End); err != nil {
		return err
	}
	if b.Status == model.BatchPending {
		spawnWorker(cfg)
	}
	_ = os.Remove(cfg.SeenDir() + "/" + rec.SessionID)
	return nil
}

func (a *App) ingest(cmd *cobra.Command, store *journal.Store, cfg *config.Config, rec capture.Record) error {
	if rec.Command == "" {
		return nil
	}
	c := ctx(cmd)
	host, username := hostIdentity()

	if err := store.UpsertSession(c, model.Session{ID: rec.SessionID, StartedAt: rec.Start, Hostname: host, Username: username, Shell: rec.Shell, PID: rec.PID}); err != nil {
		return err
	}

	// Relay defaults (cached by tick/enroll) are merged under the local config.
	ropts := redact.Options{ExtraPatterns: cfg.Redact.ExtraPatterns, ExtraSensitiveCommands: cfg.Redact.ExtraSensitiveCommands, Capture: cfg.Credentials.CaptureFromCommands}
	copts := classify.Options{Ignore: cfg.Capture.Ignore, Unignore: cfg.Capture.Unignore, Weights: cfg.Capture.Weights}
	if cfg.Mode == config.ModeRelay {
		if d := cachedDefaults(c, store); d != nil {
			ropts.ExtraPatterns = append(append([]string{}, d.ExtraPatterns...), ropts.ExtraPatterns...)
			ropts.Capture = ropts.Capture || d.CaptureCreds
			copts.Ignore = append(append([]string{}, d.Ignore...), copts.Ignore...)
			copts.Unignore = append(append([]string{}, d.Unignore...), copts.Unignore...)
			merged := map[string]int{}
			for k, v := range d.Weights {
				merged[k] = v
			}
			for k, v := range copts.Weights {
				merged[k] = v
			}
			copts.Weights = merged
		}
	}
	redactor, err := redact.New(ropts)
	if err != nil {
		slog.Warn("redact config invalid, using defaults", "err", err)
		redactor = redact.Default
	}
	red := redactor.Redact(rec.Command)
	if len(red.Captured) > 0 {
		a.storeCaptured(cmd, cfg, host, red.Captured)
	}

	cls := classify.New(copts).Classify(red.Text, rec.CWD)
	if cls.Kind == model.KindNoise && !cfg.Capture.StoreNoise {
		return nil
	}

	ev := &model.Event{
		SessionID: rec.SessionID, Start: rec.Start, End: rec.End, Command: red.Text, ExitCode: rec.ExitCode, CWD: rec.CWD,
		Hostname: host, Username: username, Shell: rec.Shell, Kind: cls.Kind, Family: cls.Family, Weight: cls.Weight,
		Milestone: cls.Milestone, FilesTouched: cls.FilesTouched, Redactions: red.Kinds,
	}
	if git := capture.LookupGit(rec.CWD); git.Root != "" {
		ev.GitRoot, ev.GitBranch, ev.GitHead = git.Root, git.Branch, git.Head
	}

	dec, err := batch.Observe(c, store, ev, policyFrom(cfg), time.Now())
	if err != nil {
		return err
	}
	if err := store.InsertEvent(c, ev); err != nil {
		return err
	}
	if dec.Trigger != "" || (dec.Closed != nil && dec.Closed.Status == model.BatchPending) {
		slog.Debug("batch pending", "batch", dec.Batch.ID, "trigger", dec.Trigger)
		spawnWorker(cfg)
	}
	return nil
}

func policyFrom(cfg *config.Config) batch.Policy {
	return batch.Policy{
		IdleGap:         cfg.Triggers.IdleGap,
		ScoreThreshold:  cfg.Triggers.ScoreThreshold,
		MeaningfulCount: cfg.Triggers.MeaningfulCount,
		Timer:           cfg.Triggers.Timer,
		MinInterval:     cfg.Triggers.MinInterval,
		Milestones:      cfg.Triggers.Milestones,
		MinMeaningful:   2,
		MaxEvents:       200,
	}
}

// spawnWorker starts a detached `codeument work` process. In local mode the
// worker will simply leave batches pending, so nothing is spawned.
func spawnWorker(cfg *config.Config) {
	if cfg.Mode == config.ModeLocal || os.Getenv("CODEUMENT_NO_WORKER") != "" {
		return
	}
	args := []string{"work"}
	if p := os.Getenv("CODEUMENT_CONFIG"); p != "" {
		args = append(args, "--config", p)
	}
	proc := exec.Command(selfPath(), args...) //nolint:gosec // our own binary
	proc.Stdin, proc.Stdout, proc.Stderr = nil, nil, nil
	proc.Env = os.Environ()
	detach(proc)
	if err := proc.Start(); err != nil {
		slog.Warn("spawn worker", "err", err)
		return
	}
	go func() { _ = proc.Wait() }()
	// Give the child a moment to become independent before we exit.
	time.Sleep(20 * time.Millisecond)
	_ = proc.Process.Release()
}

func init() {
	// Make sure a stray `codeument ingest` never blocks a shell for long.
	if len(os.Args) > 1 && os.Args[1] == "ingest" {
		go func() {
			time.Sleep(10 * time.Second)
			fmt.Fprintln(os.Stderr, "codeument: ingest timed out")
			os.Exit(2)
		}()
	}
}
