// Package scheduler runs the periodic work (`codeument tick`) and installs
// the timers that call it.
package scheduler

import (
	"bytes"
	"context"
	"embed"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"text/template"
	"time"

	"github.com/Tzurrr/codeument/internal/batch"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/notify"
	"github.com/Tzurrr/codeument/internal/worker"
)

//go:embed units/*
var unitFS embed.FS

// Hooks let later phases plug into a tick without import cycles.
type Hooks struct {
	// RefreshDefaults pulls relay defaults (relay mode).
	RefreshDefaults func(ctx context.Context) error
	// Snapshot runs a server snapshot when due.
	Snapshot func(ctx context.Context) error
	// System marks a system-scope tick (snapshots run as root).
	System bool
}

// TickOptions configure one pass.
type TickOptions struct {
	Policy    batch.Policy
	Retention time.Duration
	SeenDir   string
	Hooks     Hooks
	// Summarize is false in local mode.
	Summarize bool
}

// Report says what a tick did.
type Report struct {
	ClosedBatches int
	Drafts        int
	Published     int
	Pruned        int64
	Errors        []error
}

// Tick runs one maintenance pass.
func Tick(ctx context.Context, store *journal.Store, w *worker.Worker, opts TickOptions) Report {
	var rep Report
	now := time.Now()
	pending, err := batch.CloseStale(ctx, store, opts.Policy, now)
	if err != nil {
		rep.Errors = append(rep.Errors, fmt.Errorf("close stale: %w", err))
	}
	rep.ClosedBatches = len(pending)
	if opts.Hooks.RefreshDefaults != nil {
		if err := opts.Hooks.RefreshDefaults(ctx); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("refresh defaults: %w", err))
		}
	}
	if opts.Summarize && w != nil {
		n, err := w.ProcessPending(ctx)
		rep.Drafts = n
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("summarize: %w", err))
		}
		p, err := w.RetryQueue(ctx)
		rep.Published = p
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("retry publish: %w", err))
		}
	}
	if opts.Retention > 0 {
		n, err := store.PruneEvents(ctx, now.Add(-opts.Retention))
		rep.Pruned = n
		if err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("prune: %w", err))
		}
	}
	if opts.SeenDir != "" {
		notify.PruneSeen(opts.SeenDir, 7*24*time.Hour)
	}
	if opts.Hooks.Snapshot != nil {
		if err := opts.Hooks.Snapshot(ctx); err != nil {
			rep.Errors = append(rep.Errors, fmt.Errorf("snapshot: %w", err))
		}
	}
	_ = store.SetKV(ctx, "last_tick", fmt.Sprint(now.Unix()))
	return rep
}

// InstallOptions describe the timer to install.
type InstallOptions struct {
	Binary     string
	ConfigPath string
	Scope      string // user | system
	Interval   time.Duration
	Daemon     bool // install a long-running `daemon` service instead of a timer
	DryRun     bool
}

// Installed describes what was written.
type Installed struct {
	Files    []string
	Commands []string
}

type unitData struct {
	Binary, ConfigFlag, Interval, User string
	IntervalSeconds                    int
}

// Install writes the systemd (Linux) or launchd (macOS) units.
func Install(opts InstallOptions) (*Installed, error) {
	if opts.Interval <= 0 {
		opts.Interval = 5 * time.Minute
	}
	data := unitData{Binary: opts.Binary, Interval: fmt.Sprintf("%dmin", int(opts.Interval.Minutes())), IntervalSeconds: int(opts.Interval.Seconds())}
	if opts.ConfigPath != "" {
		data.ConfigFlag = " --config " + opts.ConfigPath
	}
	switch runtime.GOOS {
	case "linux":
		return installSystemd(opts, data)
	case "darwin":
		return installLaunchd(opts, data)
	}
	return nil, fmt.Errorf("install-daemon is not supported on %s; run `%s daemon` from your own supervisor", runtime.GOOS, opts.Binary)
}

func render(name string, data unitData) ([]byte, error) {
	src, err := unitFS.ReadFile("units/" + name)
	if err != nil {
		return nil, err
	}
	tmpl, err := template.New(name).Parse(string(src))
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func installSystemd(opts InstallOptions, data unitData) (*Installed, error) {
	var dir, ctl string
	if opts.Scope == "system" {
		dir, ctl = "/etc/systemd/system", "systemctl"
	} else {
		home, _ := os.UserHomeDir()
		dir, ctl = filepath.Join(home, ".config", "systemd", "user"), "systemctl --user"
	}
	out := &Installed{}
	files := map[string]string{}
	if opts.Daemon {
		svc, err := render("codeument-daemon.service", data)
		if err != nil {
			return nil, err
		}
		files["codeument.service"] = string(svc)
		out.Commands = []string{ctl + " daemon-reload", ctl + " enable --now codeument.service"}
	} else {
		svc, err := render("codeument.service", data)
		if err != nil {
			return nil, err
		}
		tim, err := render("codeument.timer", data)
		if err != nil {
			return nil, err
		}
		files["codeument.service"] = string(svc)
		files["codeument.timer"] = string(tim)
		out.Commands = []string{ctl + " daemon-reload", ctl + " enable --now codeument.timer"}
	}
	for name, content := range files {
		path := filepath.Join(dir, name)
		out.Files = append(out.Files, path)
		if opts.DryRun {
			continue
		}
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, err
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return nil, err
		}
	}
	return out, nil
}

func installLaunchd(opts InstallOptions, data unitData) (*Installed, error) {
	var dir, label string
	if opts.Scope == "system" {
		dir, label = "/Library/LaunchDaemons", "io.codeument.system"
	} else {
		home, _ := os.UserHomeDir()
		dir, label = filepath.Join(home, "Library", "LaunchAgents"), "io.codeument.agent"
	}
	name := "codeument.plist"
	if opts.Daemon {
		name = "codeument-daemon.plist"
	}
	content, err := render(name, data)
	if err != nil {
		return nil, err
	}
	path := filepath.Join(dir, label+".plist")
	out := &Installed{Files: []string{path}, Commands: []string{"launchctl unload " + path + " 2>/dev/null; launchctl load -w " + path}}
	if opts.DryRun {
		return out, nil
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return nil, err
	}
	return out, os.WriteFile(path, content, 0o644)
}
