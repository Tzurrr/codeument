package cli

import (
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/model"
)

func (a *App) statusCmd() *cobra.Command {
	var privacy bool
	var sessions, events int
	cmd := &cobra.Command{
		Use:   "status",
		Short: "Show what codeument has captured and where things stand",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if privacy {
				return a.printPrivacy(cmd, cfg)
			}
			store, err := a.openStore(cmd)
			if err != nil {
				return err
			}
			c := ctx(cmd)
			stats, err := store.Stats(c)
			if err != nil {
				return err
			}
			sess, err := store.ListSessions(c, sessions)
			if err != nil {
				return err
			}
			pending, err := store.ListBatches(c, model.BatchPending, model.BatchFailed)
			if err != nil {
				return err
			}
			drafts, err := store.ListDrafts(c, model.DraftStatusDraft)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, map[string]any{
					"config": a.cfgRes.Path, "mode": cfg.Mode, "data_dir": cfg.DataDir, "paused": paused(cfg),
					"stats": stats, "sessions": sess, "pending_batches": pending, "drafts": len(drafts),
				})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "config:   %s\n", a.cfgRes.Path)
			fmt.Fprintf(out, "mode:     %s%s\n", cfg.Mode, modeHint(cfg))
			fmt.Fprintf(out, "data:     %s\n", cfg.DataDir)
			if paused(cfg) {
				fmt.Fprintf(out, "capture:  PAUSED (run `codeument resume`)\n")
			}
			fmt.Fprintf(out, "events:   %d (%d meaningful) across %d sessions\n", stats.Events, stats.Meaningful, stats.Sessions)
			if !stats.LastEvent.IsZero() {
				fmt.Fprintf(out, "last:     %s ago\n", humanDuration(time.Since(stats.LastEvent)))
			}
			fmt.Fprintf(out, "batches:  %d open, %d pending summary\n", stats.OpenBatches, stats.Pending)
			fmt.Fprintf(out, "drafts:   %d waiting for review", len(drafts))
			if len(drafts) > 0 {
				fmt.Fprintf(out, "  (run `codeument review`)")
			}
			fmt.Fprintln(out)
			if len(pending) > 0 && cfg.Mode == config.ModeLocal {
				fmt.Fprintf(out, "\n%d batch(es) are ready to summarize but mode is local. Run `codeument init` to enable direct mode or `codeument enroll` for a relay.\n", len(pending))
			}
			for _, s := range sess {
				evs, err := store.ListEvents(c, journal.EventQuery{SessionID: s.ID, Desc: true, Limit: events})
				if err != nil {
					return err
				}
				state := "active"
				if s.EndedAt != nil {
					state = "ended"
				}
				fmt.Fprintf(out, "\nsession %s  %s@%s  %s  started %s  %d events  %s\n", shortID(s.ID), s.Username, s.Hostname, s.Shell, s.StartedAt.Format("Jan 2 15:04"), s.EventCount, state)
				tw := tabwriter.NewWriter(out, 2, 4, 2, ' ', 0)
				for i := len(evs) - 1; i >= 0; i-- {
					e := evs[i]
					flag := " "
					if e.Milestone {
						flag = "*"
					}
					kind := "-"
					if e.Kind == model.KindMeaningful {
						kind = fmt.Sprintf("%d", e.Weight)
					}
					red := ""
					if len(e.Redactions) > 0 {
						red = " [" + strings.Join(e.Redactions, ",") + "]"
					}
					fmt.Fprintf(tw, "  %s\t%s\t%s%s\t%s\t%d\t%s%s\n", e.Start.Format("15:04:05"), kind, flag, e.Family, shortPath(e.CWD), e.ExitCode, truncate(e.Command, 80), red)
				}
				_ = tw.Flush()
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&privacy, "privacy", false, "explain what leaves this machine in the current mode")
	cmd.Flags().IntVar(&sessions, "sessions", 3, "how many recent sessions to show")
	cmd.Flags().IntVar(&events, "events", 15, "how many recent events per session to show")
	return cmd
}

func modeHint(cfg *config.Config) string {
	switch cfg.Mode {
	case config.ModeLocal:
		return "  (nothing leaves this machine)"
	case config.ModeDirect:
		return fmt.Sprintf("  (llm: %s, docs: %s)", cfg.LLM.Provider, cfg.Docs.Provider)
	case config.ModeRelay:
		return fmt.Sprintf("  (relay: %s)", cfg.Relay.URL)
	}
	return ""
}

func (a *App) printPrivacy(cmd *cobra.Command, cfg *config.Config) error {
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "mode: %s\n\n", cfg.Mode)
	fmt.Fprintln(out, "Always, before anything is stored: command lines are redacted (tokens, passwords,")
	fmt.Fprintln(out, "auth headers, key material, URL passwords, sensitive programs' arguments).")
	fmt.Fprintln(out, "Command output, environment variables and file contents are never captured.")
	fmt.Fprintln(out)
	switch cfg.Mode {
	case config.ModeLocal:
		fmt.Fprintln(out, "Nothing leaves this machine. The journal lives at", cfg.JournalPath())
	case config.ModeDirect:
		fmt.Fprintf(out, "Sent to the LLM provider (%s) when a batch is summarized:\n", cfg.LLM.Provider)
		fmt.Fprintln(out, "  redacted commands, exit codes, timestamps, working directories, git branch and")
		fmt.Fprintln(out, "  diff stat (file names and counts only), touched file paths, hostname, username.")
		fmt.Fprintf(out, "Sent to the docs provider (%s) when you accept a draft: the draft text you approved.\n", cfg.Docs.Provider)
	case config.ModeRelay:
		fmt.Fprintf(out, "Sent to the relay at %s (over TLS, per-client token):\n", cfg.Relay.URL)
		fmt.Fprintln(out, "  redacted commands, exit codes, timestamps, working directories, git branch and")
		fmt.Fprintln(out, "  diff stat (file names and counts only), touched file paths, hostname, username,")
		fmt.Fprintln(out, "  and drafts you approve. The relay forwards to the LLM and docs providers it holds.")
	}
	if cfg.Snapshot.Enabled {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Server snapshots send: OS, resources, mounts, listening ports, services, cron jobs,")
		fmt.Fprintln(out, "containers, account names and groups, directory listings and redacted heads of")
		fmt.Fprintf(out, "README/config files. Credential policy: %s.\n", cfg.Credentials.Mode)
	}
	if cfg.Credentials.CaptureFromCommands {
		fmt.Fprintln(out)
		fmt.Fprintln(out, "Credential capture is ON: secrets seen in commands are kept in the encrypted local")
		fmt.Fprintln(out, "cache and only sent when you attach them to an account during review.")
	}
	fmt.Fprintln(out)
	fmt.Fprintln(out, "Controls: `codeument pause 1h`, CODEUMENT_OFF=1, `codeument forget --last 5`, `codeument purge --all`.")
	return nil
}

func (a *App) forgetCmd() *cobra.Command {
	var last int
	var since time.Duration
	var session string
	cmd := &cobra.Command{
		Use:   "forget",
		Short: "Delete captured events (drafts built on them are marked stale)",
		Example: `  codeument forget --last 5
  codeument forget --since 1h
  codeument forget --session abc123`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.openStore(cmd)
			if err != nil {
				return err
			}
			c := ctx(cmd)
			var n int64
			switch {
			case last > 0:
				n, err = store.DeleteLastEvents(c, last)
			case since > 0:
				n, err = store.DeleteEvents(c, journal.EventQuery{Since: time.Now().Add(-since)})
			case session != "":
				n, err = store.DeleteEvents(c, journal.EventQuery{SessionID: session})
			default:
				return fmt.Errorf("pass one of --last N, --since DURATION, --session ID")
			}
			if err != nil {
				return err
			}
			a.infof(cmd, "forgot %d event(s)\n", n)
			return nil
		},
	}
	cmd.Flags().IntVar(&last, "last", 0, "delete the N most recent events")
	cmd.Flags().DurationVar(&since, "since", 0, "delete events newer than this duration (e.g. 1h)")
	cmd.Flags().StringVar(&session, "session", "", "delete every event of a session")
	return cmd
}

func (a *App) pauseCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "pause [duration]",
		Short: "Stop capturing for a while (default 1h; 0 = until resume)",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			d := time.Hour
			if len(args) == 1 {
				if d, err = time.ParseDuration(args[0]); err != nil {
					return err
				}
			}
			until := int64(0)
			if d > 0 {
				until = time.Now().Add(d).Unix()
			}
			if err := os.WriteFile(cfg.PausePath(), []byte(fmt.Sprint(until)), 0o600); err != nil {
				return err
			}
			if until == 0 {
				a.infof(cmd, "capture paused until `codeument resume`\n")
			} else {
				a.infof(cmd, "capture paused for %s\n", d)
			}
			return nil
		},
	}
}

func (a *App) resumeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "resume",
		Short: "Resume capturing after a pause",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if err := os.Remove(cfg.PausePath()); err != nil && !os.IsNotExist(err) {
				return err
			}
			a.infof(cmd, "capture resumed\n")
			return nil
		},
	}
}

func (a *App) purgeCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "purge --all",
		Short: "Delete the whole local journal (events, batches, drafts, snapshots)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if !all {
				return fmt.Errorf("refusing without --all")
			}
			store, err := a.openStore(cmd)
			if err != nil {
				return err
			}
			if err := store.Purge(ctx(cmd)); err != nil {
				return err
			}
			cfg, _ := a.loadConfig(cmd)
			_ = os.Remove(cfg.NotifyPath())
			a.infof(cmd, "journal purged\n")
			return nil
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "confirm deletion of everything")
	return cmd
}

func shortID(id string) string {
	if len(id) > 12 {
		return id[:12]
	}
	return id
}

func shortPath(p string) string {
	if h, err := os.UserHomeDir(); err == nil && strings.HasPrefix(p, h) {
		p = "~" + strings.TrimPrefix(p, h)
	}
	if len(p) > 28 {
		return "…" + p[len(p)-27:]
	}
	return p
}

func truncate(s string, n int) string {
	s = strings.ReplaceAll(s, "\n", "⏎")
	if len(s) <= n {
		return s
	}
	return s[:n-1] + "…"
}

func humanDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 48*time.Hour:
		return fmt.Sprintf("%dh%02dm", int(d.Hours()), int(d.Minutes())%60)
	}
	return fmt.Sprintf("%dd", int(d.Hours()/24))
}
