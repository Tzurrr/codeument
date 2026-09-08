package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/batch"
	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/scheduler"
	"github.com/Tzurrr/codeument/internal/tui"
	"github.com/Tzurrr/codeument/internal/worker"
)

func (a *App) registerPhase2(root *cobra.Command) {
	root.AddCommand(a.workCmd(), a.nowCmd(), a.reviewCmd(), a.draftsCmd(), a.tickCmd(), a.daemonCmd(), a.installDaemonCmd(), a.initCmd())
}

func (a *App) validateProviders(cmd *cobra.Command, cfg *config.Config) error {
	if cfg.Mode == config.ModeLocal {
		fmt.Fprintln(cmd.OutOrStdout(), "mode local: no providers to check")
		return nil
	}
	eng, err := a.buildEngine(cfg)
	if err != nil {
		return err
	}
	c, cancel := context.WithTimeout(ctx(cmd), 60*time.Second)
	defer cancel()
	if err := eng.Ping(c); err != nil {
		return fmt.Errorf("%s: %w", eng.Name(), err)
	}
	fmt.Fprintf(cmd.OutOrStdout(), "%s: ok\n", eng.Name())
	return nil
}

// --- work -------------------------------------------------------------------

func (a *App) workCmd() *cobra.Command {
	return &cobra.Command{
		Use:    "work",
		Short:  "Summarize pending batches (spawned automatically; safe to run by hand)",
		Hidden: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, cfg, _, err := a.buildWorker(cmd)
			if err != nil {
				return err
			}
			if w.Engine == nil {
				return nil
			}
			lock, ok, err := worker.TryLock(cfg.LockPath())
			if err != nil {
				return err
			}
			if !ok {
				return nil // another worker is on it
			}
			defer lock.Unlock()
			c, cancel := context.WithTimeout(ctx(cmd), 10*time.Minute)
			defer cancel()
			n, err := w.ProcessPending(c)
			if n > 0 {
				a.infof(cmd, "%d draft(s) ready\n", n)
			}
			if _, qerr := w.RetryQueue(c); qerr != nil && err == nil {
				err = qerr
			}
			return err
		},
	}
}

// --- now --------------------------------------------------------------------

func (a *App) nowCmd() *cobra.Command {
	var note string
	var all bool
	var session string
	cmd := &cobra.Command{
		Use:   "now",
		Short: "Close the current batch and summarize it right away",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, cfg, store, err := a.buildWorker(cmd)
			if err != nil {
				return err
			}
			c := ctx(cmd)
			sess := session
			if sess == "" && !all {
				sess = os.Getenv("CODEUMENT_SESSION")
			}
			pending, err := batch.CloseAll(c, store, policyFrom(cfg), sess, time.Now())
			if err != nil {
				return err
			}
			if sess != "" && len(pending) == 0 {
				// Fall back to every open batch when this shell has nothing.
				if _, err = batch.CloseAll(c, store, policyFrom(cfg), "", time.Now()); err != nil {
					return err
				}
			}
			already, _ := store.ListBatches(c, model.BatchPending, model.BatchFailed)
			total := len(already)
			if total == 0 {
				a.infof(cmd, "nothing to summarize (need at least 2 meaningful commands in a batch)\n")
				return nil
			}
			if w.Engine == nil {
				a.infof(cmd, "%d batch(es) closed and waiting; mode is local so nothing is summarized. Run `codeument init` or `codeument enroll`.\n", total)
				return nil
			}
			created := 0
			for i := range already {
				b := &already[i]
				a.infof(cmd, "summarizing batch %s (%d commands)…\n", shortID(b.ID), b.EventCount)
				d, err := w.SummarizeBatch(c, b, note)
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "  failed: %v\n", err)
					continue
				}
				created++
				a.infof(cmd, "  draft: %s\n", d.Title)
			}
			_ = w.UpdateNotify(c)
			if created > 0 {
				a.infof(cmd, "%d draft(s) ready — run `codeument review`\n", created)
			}
			return nil
		},
	}
	cmd.Flags().StringVarP(&note, "message", "m", "", "a note for the summarizer (what you were doing)")
	cmd.Flags().BoolVar(&all, "all", false, "close every open batch, not just this shell's")
	cmd.Flags().StringVar(&session, "session", "", "close a specific session's batch")
	return cmd
}

// --- review -----------------------------------------------------------------

func (a *App) reviewCmd() *cobra.Command {
	var all bool
	cmd := &cobra.Command{
		Use:   "review",
		Short: "Review drafts: edit, accept, dismiss, publish",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, cfg, store, err := a.buildWorker(cmd)
			if err != nil {
				return err
			}
			c := ctx(cmd)
			statuses := []model.DraftStatus{model.DraftStatusDraft, model.DraftStatusAccepted}
			if all {
				statuses = nil
			}
			drafts, err := store.ListDrafts(c, statuses...)
			if err != nil {
				return err
			}
			if len(drafts) == 0 {
				a.infof(cmd, "no drafts to review\n")
				return nil
			}
			if w.Engine == nil {
				w.Engine = engine.Unavailable{}
			}
			opts := tui.Options{Worker: w, Editor: cfg.Editor, Drafts: drafts, KnownDocs: a.knownDocs(c, store), CredentialPrompt: a.credentialPrompt(cfg)}
			if err := tui.Run(c, opts); err != nil {
				return err
			}
			return w.UpdateNotify(c)
		},
	}
	cmd.Flags().BoolVar(&all, "all", false, "include published and dismissed drafts")
	return cmd
}

func (a *App) knownDocs(c context.Context, store *journal.Store) []tui.DocChoice {
	var out []tui.DocChoice
	seen := map[string]bool{}
	if pages, err := store.ListDocPages(c); err == nil {
		for _, p := range pages {
			if !seen[p.DocID] {
				seen[p.DocID] = true
				out = append(out, tui.DocChoice{ID: p.DocID, Title: p.Title})
			}
		}
	}
	if recent, err := store.RecentDocTitles(c, 50); err == nil {
		for _, r := range recent {
			if r.DocID != "" && !seen[r.DocID] {
				seen[r.DocID] = true
				out = append(out, tui.DocChoice{ID: r.DocID, Title: r.Title})
			}
		}
	}
	return out
}

// --- drafts -----------------------------------------------------------------

func (a *App) draftsCmd() *cobra.Command {
	cmd := &cobra.Command{Use: "drafts", Short: "Non-interactive draft management"}
	var id string
	list := &cobra.Command{
		Use:   "list",
		Short: "List drafts",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, err := a.openStore(cmd)
			if err != nil {
				return err
			}
			drafts, err := store.ListDrafts(ctx(cmd))
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, drafts)
			}
			for _, d := range drafts {
				fmt.Fprintf(cmd.OutOrStdout(), "%s  %-10s  %s  %s\n", d.ID, d.Status, d.CreatedAt.Format("2006-01-02 15:04"), d.Title)
			}
			return nil
		},
	}
	show := &cobra.Command{
		Use:   "show",
		Short: "Print one draft as editable Markdown",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, _, err := a.draftByID(cmd, id)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, d)
			}
			fmt.Fprint(cmd.OutOrStdout(), tui.EditableMarkdown(*d))
			return nil
		},
	}
	accept := &cobra.Command{
		Use:   "accept",
		Short: "Accept a draft",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, w, err := a.draftByID(cmd, id)
			if err != nil {
				return err
			}
			if err := w.Accept(ctx(cmd), d); err != nil {
				return err
			}
			a.infof(cmd, "accepted %s (doc %s)\n", d.ID, d.DocID)
			return w.UpdateNotify(ctx(cmd))
		},
	}
	dismiss := &cobra.Command{
		Use:   "dismiss",
		Short: "Dismiss a draft",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, w, err := a.draftByID(cmd, id)
			if err != nil {
				return err
			}
			if err := w.Dismiss(ctx(cmd), d); err != nil {
				return err
			}
			return w.UpdateNotify(ctx(cmd))
		},
	}
	publish := &cobra.Command{
		Use:   "publish",
		Short: "Publish a draft (accepting it first when needed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, w, err := a.draftByID(cmd, id)
			if err != nil {
				return err
			}
			if w.Engine == nil {
				return engine.ErrNoEngine
			}
			ref, err := w.Publish(ctx(cmd), d)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, ref)
			}
			a.infof(cmd, "published: %s\n", ref.URL)
			return nil
		},
	}
	var file string
	update := &cobra.Command{
		Use:   "update",
		Short: "Replace a draft's content from an edited Markdown file (as printed by show)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			d, w, err := a.draftByID(cmd, id)
			if err != nil {
				return err
			}
			data, err := os.ReadFile(file)
			if err != nil {
				return err
			}
			if err := tui.ApplyEditedMarkdown(d, string(data)); err != nil {
				return err
			}
			return w.Store.SaveDraft(ctx(cmd), d)
		},
	}
	update.Flags().StringVar(&file, "file", "", "markdown file to read")
	_ = update.MarkFlagRequired("file")
	for _, c := range []*cobra.Command{show, accept, dismiss, publish, update} {
		c.Flags().StringVar(&id, "id", "", "draft id (prefix is enough)")
		_ = c.MarkFlagRequired("id")
	}
	cmd.AddCommand(list, show, accept, dismiss, publish, update)
	return cmd
}

func (a *App) draftByID(cmd *cobra.Command, id string) (*model.DraftRecord, *worker.Worker, error) {
	w, _, store, err := a.buildWorker(cmd)
	if err != nil {
		return nil, nil, err
	}
	c := ctx(cmd)
	d, err := store.GetDraft(c, id)
	if errors.Is(err, journal.ErrNotFound) {
		all, lerr := store.ListDrafts(c)
		if lerr != nil {
			return nil, nil, lerr
		}
		var matches []model.DraftRecord
		for _, x := range all {
			if strings.HasPrefix(x.ID, id) {
				matches = append(matches, x)
			}
		}
		switch len(matches) {
		case 0:
			return nil, nil, fmt.Errorf("no draft with id %q", id)
		case 1:
			d = &matches[0]
		default:
			return nil, nil, fmt.Errorf("id prefix %q matches %d drafts", id, len(matches))
		}
	} else if err != nil {
		return nil, nil, err
	}
	return d, w, nil
}

// --- tick / daemon ----------------------------------------------------------

func (a *App) tickOptions(cfg *config.Config, w *worker.Worker) scheduler.TickOptions {
	return scheduler.TickOptions{
		Policy:    policyFrom(cfg),
		Retention: cfg.Retention.Events,
		SeenDir:   cfg.SeenDir(),
		Summarize: w != nil && w.Engine != nil,
		Hooks:     a.tickHooks(cfg, w),
	}
}

func (a *App) tickCmd() *cobra.Command {
	var system bool
	cmd := &cobra.Command{
		Use:   "tick",
		Short: "Run one maintenance pass (close idle batches, summarize, retry publishes, snapshot when due)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, cfg, store, err := a.buildWorker(cmd)
			if err != nil {
				return err
			}
			lock, ok, err := worker.TryLock(cfg.LockPath())
			if err != nil {
				return err
			}
			if !ok {
				a.infof(cmd, "another worker is running; skipping\n")
				return nil
			}
			defer lock.Unlock()
			c, cancel := context.WithTimeout(ctx(cmd), 20*time.Minute)
			defer cancel()
			opts := a.tickOptions(cfg, w)
			if system {
				opts.Hooks.System = true
			}
			rep := scheduler.Tick(c, store, w, opts)
			if a.jsonOut {
				return a.printJSON(cmd, rep)
			}
			a.infof(cmd, "tick: closed %d batch(es), %d draft(s), %d published, %d event(s) pruned\n", rep.ClosedBatches, rep.Drafts, rep.Published, rep.Pruned)
			for _, e := range rep.Errors {
				fmt.Fprintln(cmd.ErrOrStderr(), "tick:", e)
			}
			return nil
		},
	}
	cmd.Flags().BoolVar(&system, "system", false, "system scope (snapshots as root)")
	return cmd
}

func (a *App) daemonCmd() *cobra.Command {
	var interval time.Duration
	var system bool
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Run tick in a loop (for hosts without systemd timers or launchd)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, stop := signal.NotifyContext(ctx(cmd), os.Interrupt, syscall.SIGTERM)
			defer stop()
			for {
				w, cfg, store, err := a.buildWorker(cmd)
				if err != nil {
					return err
				}
				opts := a.tickOptions(cfg, w)
				opts.Hooks.System = system
				if lock, ok, err := worker.TryLock(cfg.LockPath()); err == nil && ok {
					rep := scheduler.Tick(c, store, w, opts)
					lock.Unlock()
					for _, e := range rep.Errors {
						fmt.Fprintln(cmd.ErrOrStderr(), "tick:", e)
					}
				}
				a.cfgRes = nil // re-read config every loop
				select {
				case <-c.Done():
					return nil
				case <-time.After(interval):
				}
			}
		},
	}
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "time between ticks")
	cmd.Flags().BoolVar(&system, "system", false, "system scope (snapshots as root)")
	return cmd
}

func (a *App) installDaemonCmd() *cobra.Command {
	var scope string
	var interval time.Duration
	var daemon, dryRun bool
	cmd := &cobra.Command{
		Use:   "install-daemon",
		Short: "Install a systemd timer (Linux) or launchd agent (macOS) that runs `codeument tick`",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfgPath := a.configPath
			if cfgPath == "" {
				cfgPath = os.Getenv("CODEUMENT_CONFIG")
			}
			if scope == "system" && os.Geteuid() != 0 && !dryRun {
				return fmt.Errorf("--scope system needs root (sudo)")
			}
			out, err := scheduler.Install(scheduler.InstallOptions{Binary: selfPath(), ConfigPath: cfgPath, Scope: scope, Interval: interval, Daemon: daemon, DryRun: dryRun})
			if err != nil {
				return err
			}
			verb := "wrote"
			if dryRun {
				verb = "would write"
			}
			for _, f := range out.Files {
				a.infof(cmd, "%s %s\n", verb, f)
			}
			a.infof(cmd, "now run:\n")
			for _, c := range out.Commands {
				a.infof(cmd, "  %s\n", c)
			}
			return nil
		},
	}
	cmd.Flags().StringVar(&scope, "scope", "user", "user (laptop) or system (server, runs snapshots as root)")
	cmd.Flags().DurationVar(&interval, "interval", 5*time.Minute, "how often to tick")
	cmd.Flags().BoolVar(&daemon, "daemon", false, "install a long-running service instead of a timer")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "print what would be written")
	return cmd
}

// --- init -------------------------------------------------------------------

func (a *App) initCmd() *cobra.Command {
	var llmProvider, docsProvider, apiKey, ollamaURL, ollamaModel, mdRoot, confURL, confEmail, confToken, confSpace string
	var yes bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Switch to direct mode: pick an LLM provider and a docs provider",
		Long: `Configure direct mode, where this client talks to the LLM and the docs
platform itself. Without flags the command asks interactively. To join a
company relay instead, use ` + "`codeument enroll`" + `.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			in := bufio.NewReader(cmd.InOrStdin())
			ask := func(prompt, def string) string {
				if yes {
					return def
				}
				if def != "" {
					fmt.Fprintf(cmd.OutOrStdout(), "%s [%s]: ", prompt, def)
				} else {
					fmt.Fprintf(cmd.OutOrStdout(), "%s: ", prompt)
				}
				line, _ := in.ReadString('\n')
				line = strings.TrimSpace(line)
				if line == "" {
					return def
				}
				return line
			}
			set := map[string]string{"mode": config.ModeDirect}
			if llmProvider == "" {
				llmProvider = ask("LLM provider (anthropic, ollama)", cfg.LLM.Provider)
			}
			set["llm.provider"] = llmProvider
			switch llmProvider {
			case "anthropic":
				if apiKey == "" && cfg.LLM.Anthropic.APIKey == "" {
					apiKey = ask("Anthropic API key (or leave empty to use ANTHROPIC_API_KEY / file://path)", "")
				}
				if apiKey != "" {
					set["llm.anthropic.api_key"] = apiKey
				}
			case "ollama":
				if ollamaURL == "" {
					ollamaURL = ask("Ollama URL", cfg.LLM.Ollama.BaseURL)
				}
				if ollamaModel == "" {
					ollamaModel = ask("Ollama model", cfg.LLM.Ollama.Model)
				}
				set["llm.ollama.base_url"] = ollamaURL
				set["llm.ollama.model"] = ollamaModel
			default:
				return fmt.Errorf("unknown llm provider %q", llmProvider)
			}
			if docsProvider == "" {
				docsProvider = ask("Docs provider (markdown, confluence)", cfg.Docs.Provider)
			}
			set["docs.provider"] = docsProvider
			switch docsProvider {
			case "markdown":
				if mdRoot == "" {
					mdRoot = ask("Markdown root directory", cfg.Docs.Markdown.Root)
				}
				set["docs.markdown.root"] = mdRoot
			case "confluence":
				if confURL == "" {
					confURL = ask("Confluence base URL (https://acme.atlassian.net/wiki)", cfg.Docs.Confluence.BaseURL)
				}
				if confEmail == "" {
					confEmail = ask("Atlassian account email", cfg.Docs.Confluence.Email)
				}
				if confToken == "" && cfg.Docs.Confluence.APIToken == "" {
					confToken = ask("Atlassian API token", "")
				}
				if confSpace == "" {
					confSpace = ask("Space key", cfg.Docs.Confluence.Space)
				}
				set["docs.confluence.base_url"] = confURL
				set["docs.confluence.email"] = confEmail
				if confToken != "" {
					set["docs.confluence.api_token"] = confToken
				}
				set["docs.confluence.space"] = confSpace
				set["docs.default_location.space"] = confSpace
			default:
				return fmt.Errorf("unknown docs provider %q", docsProvider)
			}
			for k, v := range set {
				if err := config.SetValue(a.cfgRes.Path, k, v); err != nil {
					return err
				}
			}
			a.cfgRes = nil
			cfg, err = a.loadConfig(cmd)
			if err != nil {
				return err
			}
			a.infof(cmd, "mode: direct (llm %s, docs %s). Checking providers…\n", cfg.LLM.Provider, cfg.Docs.Provider)
			if err := a.validateProviders(cmd, cfg); err != nil {
				fmt.Fprintf(cmd.ErrOrStderr(), "warning: %v\n(config saved; fix it and run `codeument config validate`)\n", err)
			}
			a.infof(cmd, "\nWhat leaves this machine from now on:\n")
			return a.printPrivacy(cmd, cfg)
		},
	}
	cmd.Flags().StringVar(&llmProvider, "llm", "", "anthropic or ollama")
	cmd.Flags().StringVar(&docsProvider, "docs", "", "markdown or confluence")
	cmd.Flags().StringVar(&apiKey, "anthropic-key", "", "Anthropic API key")
	cmd.Flags().StringVar(&ollamaURL, "ollama-url", "", "Ollama base URL")
	cmd.Flags().StringVar(&ollamaModel, "ollama-model", "", "Ollama model")
	cmd.Flags().StringVar(&mdRoot, "markdown-root", "", "Markdown output directory")
	cmd.Flags().StringVar(&confURL, "confluence-url", "", "Confluence base URL")
	cmd.Flags().StringVar(&confEmail, "confluence-email", "", "Atlassian email")
	cmd.Flags().StringVar(&confToken, "confluence-token", "", "Atlassian API token")
	cmd.Flags().StringVar(&confSpace, "confluence-space", "", "Confluence space key")
	cmd.Flags().BoolVarP(&yes, "yes", "y", false, "accept defaults without prompting")
	return cmd
}
