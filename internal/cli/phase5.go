package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/explain"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/snapshot"
	"github.com/Tzurrr/codeument/internal/snapshot/collect"
	"github.com/Tzurrr/codeument/internal/worker"
)

const kvLastSnapshot = "last_snapshot_at"

func (a *App) registerPhase5(root *cobra.Command) {
	root.AddCommand(a.snapshotCmd())
}

// dirCache adapts the journal to snapshot.ExplainCache.
type dirCache struct {
	store *journal.Store
}

func (c dirCache) Get(ctx context.Context, path string) (string, *explain.Explanation, bool) {
	row, err := c.store.GetDirExplanation(ctx, path)
	if err != nil {
		return "", nil, false
	}
	var e explain.Explanation
	if json.Unmarshal(row.Explanation, &e) != nil {
		return "", nil, false
	}
	return row.ContentHash, &e, true
}

func (c dirCache) Put(ctx context.Context, path, hash string, e explain.Explanation) error {
	raw, err := json.Marshal(e)
	if err != nil {
		return err
	}
	return c.store.SaveDirExplanation(ctx, journal.DirExplanation{Path: path, ContentHash: hash, Explanation: raw})
}

// snapshotRun is one snapshot pass, shared by the command and the tick hook.
type snapshotRun struct {
	Snapshot  *snapshot.Snapshot
	Diff      snapshot.Diff
	Explained int
	Published bool
	PageRef   docs.PageRef
	Skipped   string
}

type snapshotOpts struct {
	publish bool
	force   bool
	dryRun  bool
	noLLM   bool
}

// runSnapshot collects, explains, diffs, stores and optionally publishes.
func (a *App) runSnapshot(ctx context.Context, cmd *cobra.Command, w *worker.Worker, cfg *config.Config, store *journal.Store, opts snapshotOpts) (*snapshotRun, error) {
	run := &snapshotRun{}
	s := snapshot.Collect(ctx, snapshot.Options{
		Env:        collect.Default,
		DirInclude: cfg.Snapshot.Dirs.Include,
		DirExclude: cfg.Snapshot.Dirs.Exclude,
	})
	run.Snapshot = s

	prev, err := store.LatestSnapshot(ctx, s.MachineID)
	var prevSnap *snapshot.Snapshot
	switch {
	case err == nil:
		if prevSnap, err = snapshot.Unmarshal(prev.Data); err != nil {
			prevSnap = nil
		}
	case err != journal.ErrNotFound:
		return nil, err
	}

	if !opts.noLLM && w != nil && w.Engine != nil {
		n, err := snapshot.Explain(ctx, s, w.Engine, dirCache{store: store}, w.Hints)
		run.Explained = n
		if err != nil {
			fmt.Fprintf(cmd.ErrOrStderr(), "codeument: directory explanations unavailable: %v\n", err)
		}
	} else if !opts.noLLM {
		// Still reuse cached explanations when there is no engine.
		_, _ = snapshot.Explain(ctx, s, nil, dirCache{store: store}, nil)
	}

	run.Diff = snapshot.Compare(prevSnap, s)
	if opts.dryRun {
		return run, nil
	}

	raw, err := s.Marshal()
	if err != nil {
		return nil, err
	}
	diffRaw, _ := json.Marshal(run.Diff)
	row := journal.SnapshotRow{ID: s.ID, TakenAt: s.TakenAt, Hostname: s.Hostname, MachineID: s.MachineID, Data: raw, Diff: diffRaw}
	if err := store.SaveSnapshot(ctx, row); err != nil {
		return nil, err
	}
	_ = store.SetKV(ctx, kvLastSnapshot, strconv.FormatInt(time.Now().Unix(), 10))

	if !opts.publish {
		run.Skipped = "publishing not requested"
		return run, nil
	}
	if !run.Diff.Material && !opts.force {
		run.Skipped = "no material change since the last snapshot"
		return run, nil
	}
	if w == nil || w.Engine == nil {
		run.Skipped = "mode is local, nothing published"
		return run, nil
	}

	if err := a.promptForAccounts(cmd, cfg, a.credentialPolicy(ctx, cfg, w), s); err != nil {
		return nil, err
	}
	doc, err := a.serverDocument(ctx, w, cfg, store, s)
	if err != nil {
		return nil, err
	}
	ref, err := w.Engine.Publish(ctx, doc)
	if err != nil {
		return nil, err
	}
	run.Published, run.PageRef = true, ref
	refRaw, _ := json.Marshal(ref)
	row.Published, row.PageRef = true, refRaw
	if err := store.SaveSnapshot(ctx, row); err != nil {
		return nil, err
	}
	return run, store.SaveDocPage(ctx, journal.DocPage{DocID: doc.ID, Provider: ref.Provider, ProviderPageID: ref.ProviderID, URL: ref.URL, Version: ref.Version, ContentHash: doc.ContentHash(), Title: doc.Title})
}

// serverDocument renders the server page, applying the credential policy to
// the accounts table.
func (a *App) serverDocument(ctx context.Context, w *worker.Worker, cfg *config.Config, store *journal.Store, s *snapshot.Snapshot) (docs.Document, error) {
	history, err := snapshotHistory(ctx, store, s.MachineID)
	if err != nil {
		return docs.Document{}, err
	}
	pol := a.credentialPolicy(ctx, cfg, w)
	resolve := a.credentialResolver(ctx, w, cfg, pol, s)
	body := snapshot.Render(s, snapshot.RenderOptions{Credential: resolve, History: history, ManagerName: pol.managerName})

	loc := cfg.Snapshot.DocLocation
	if w != nil && w.Engine != nil {
		if d, err := w.Engine.Defaults(ctx); err == nil && d != nil && d.SnapshotLocation.Space != "" && cfg.Mode == config.ModeRelay {
			loc = d.SnapshotLocation
		}
	}
	if loc.Space == "" {
		loc = model.Location{Space: cfg.Docs.DefaultLocation.Space, ParentPath: []string{"Servers"}}
	}
	doc := docs.Document{
		ID: snapshot.DocID(s.MachineID), Title: snapshot.Title(s.Hostname), BodyMD: body, Location: loc,
		Labels: []string{"codeument", "server", s.Hostname},
		Meta:   map[string]string{"hostname": s.Hostname, "kind": "server", "machine_id": s.MachineID, "snapshot_id": s.ID},
	}
	if pol.mode == secrets.ModeInline && len(pol.restrictGroups) > 0 {
		doc.RestrictToGroups = pol.restrictGroups
	}
	return doc, nil
}

func snapshotHistory(ctx context.Context, store *journal.Store, machineID string) ([]snapshot.HistoryEntry, error) {
	rows, err := store.ListSnapshots(ctx, machineID, 10)
	if err != nil {
		return nil, err
	}
	var out []snapshot.HistoryEntry
	for _, r := range rows {
		if len(r.Diff) == 0 {
			continue
		}
		var d snapshot.Diff
		if json.Unmarshal(r.Diff, &d) != nil || len(d.Changes) == 0 || !d.Material {
			continue
		}
		out = append(out, snapshot.HistoryEntry{At: r.TakenAt, Changes: d.Summary(5)})
	}
	return out, nil
}

// snapshotHook returns the tick hook that runs a snapshot when one is due.
func (a *App) snapshotHook(cfg *config.Config, w *worker.Worker) func(context.Context) error {
	if !cfg.Snapshot.Enabled || w == nil || w.Store == nil {
		return nil
	}
	return func(ctx context.Context) error {
		store := w.Store
		if v, err := store.GetKV(ctx, kvLastSnapshot); err == nil {
			if secs, err := strconv.ParseInt(v, 10, 64); err == nil {
				if time.Since(time.Unix(secs, 0)) < cfg.Snapshot.Interval {
					return nil
				}
			}
		}
		run, err := a.runSnapshot(ctx, dummyCmd(), w, cfg, store, snapshotOpts{publish: cfg.Snapshot.Publish})
		if err != nil {
			return err
		}
		if run.Published {
			a.infof(dummyCmd(), "snapshot published: %s\n", run.PageRef.URL)
		}
		return nil
	}
}

// dummyCmd gives hook code a command with the standard streams.
func dummyCmd() *cobra.Command {
	c := &cobra.Command{}
	c.SetOut(os.Stdout)
	c.SetErr(os.Stderr)
	return c
}

func (a *App) snapshotCmd() *cobra.Command {
	var opts snapshotOpts
	cmd := &cobra.Command{
		Use:   "snapshot",
		Short: "Capture this machine's state and update its server page",
		Long: `Collect the machine's OS, resources, directories, services, ports, scheduled
jobs, containers and accounts, explain the important directories with the LLM,
and publish the "Server: <hostname>" page when something material changed.

Run it as root for complete results (sudoers, listening processes, other
users' crontabs). Passwords are never read from the system; the accounts
table shows where each credential is kept, per the credential policy.`,
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, cfg, store, err := a.buildWorker(cmd)
			if err != nil {
				return err
			}
			c, cancel := context.WithTimeout(ctx(cmd), 15*time.Minute)
			defer cancel()
			start := time.Now()
			a.infof(cmd, "collecting…\n")
			run, err := a.runSnapshot(c, cmd, w, cfg, store, opts)
			if err != nil {
				return err
			}
			s := run.Snapshot
			if a.jsonOut {
				return a.printJSON(cmd, map[string]any{"snapshot": s, "diff": run.Diff, "explained": run.Explained, "published": run.Published, "page_ref": run.PageRef, "skipped": run.Skipped})
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "host:       %s (%s)\n", s.Hostname, s.MachineID)
			fmt.Fprintf(out, "os:         %s, kernel %s, up %s\n", strings.TrimSpace(s.OS.Distro+" "+s.OS.DistroVersion), s.OS.Kernel, s.OS.Uptime)
			fmt.Fprintf(out, "resources:  %d cpu, %.0f%% busy, %d/%d MB memory, %d disks\n", s.Resources.CPUs, s.Resources.CPUPercent, s.Resources.MemUsedMB, s.Resources.MemTotalMB, len(s.Resources.Disks))
			fmt.Fprintf(out, "found:      %d directories, %d services, %d ports, %d cron jobs, %d containers, %d accounts\n", len(s.Dirs), len(s.Services), len(s.Ports), len(s.Cron), len(s.Containers), len(s.Accounts))
			if run.Explained > 0 {
				fmt.Fprintf(out, "explained:  %d director(ies) sent to the model\n", run.Explained)
			}
			if len(s.Partial) > 0 {
				for k, v := range s.Partial {
					fmt.Fprintf(out, "partial:    %s: %s\n", k, v)
				}
				if os.Geteuid() != 0 {
					fmt.Fprintf(out, "            (run with sudo for the complete picture)\n")
				}
			}
			fmt.Fprintf(out, "changes:    %d", len(run.Diff.Changes))
			if !run.Diff.Material {
				fmt.Fprintf(out, " (none material)")
			}
			fmt.Fprintln(out)
			for _, line := range run.Diff.Summary(10) {
				fmt.Fprintf(out, "  - %s\n", line)
			}
			switch {
			case opts.dryRun:
				fmt.Fprintf(out, "dry run: nothing stored or published\n")
			case run.Published:
				fmt.Fprintf(out, "published:  %s\n", run.PageRef.URL)
			case run.Skipped != "":
				fmt.Fprintf(out, "not published: %s\n", run.Skipped)
			}
			fmt.Fprintf(out, "took %s\n", time.Since(start).Round(time.Millisecond))
			return nil
		},
	}
	cmd.Flags().BoolVar(&opts.publish, "publish", false, "publish the server page when something material changed")
	cmd.Flags().BoolVar(&opts.force, "force", false, "publish even when nothing changed")
	cmd.Flags().BoolVar(&opts.dryRun, "dry-run", false, "collect and print, store nothing")
	cmd.Flags().BoolVar(&opts.noLLM, "no-llm", false, "skip directory explanations")
	return cmd
}

// credPolicy is the effective credential policy for rendering.
type credPolicy struct {
	mode           string
	references     map[string]string
	restrictGroups []string
	managerName    string
	promptOnReview bool
	capture        bool
}

// credentialPolicy resolves the policy: the relay's copy wins in relay mode.
func (a *App) credentialPolicy(ctx context.Context, cfg *config.Config, w *worker.Worker) credPolicy {
	p := credPolicy{
		mode: cfg.Credentials.Mode, references: cfg.Snapshot.CredentialReferences,
		restrictGroups: cfg.Credentials.Inline.PageRestrictionGroups, managerName: cfg.Credentials.Manager.Provider,
		promptOnReview: cfg.Credentials.PromptOnReview, capture: cfg.Credentials.CaptureFromCommands,
	}
	if cfg.Mode != config.ModeRelay || w == nil || w.Engine == nil {
		return p
	}
	d, err := w.Engine.Defaults(ctx)
	if err != nil || d == nil {
		if cached := cachedDefaults(ctx, w.Store); cached != nil {
			d = cached
		} else {
			return p
		}
	}
	applyRelayCredentialPolicy(&p, d)
	return p
}

func applyRelayCredentialPolicy(p *credPolicy, d *engine.Defaults) {
	if d.CredentialMode != "" {
		p.mode = d.CredentialMode
	}
	if len(d.CredentialRefs) > 0 {
		p.references = d.CredentialRefs
	}
	if d.ManagerName != "" {
		p.managerName = d.ManagerName
	}
	p.promptOnReview = d.PromptOnReview
	p.capture = p.capture || d.CaptureCreds
}

// credentialResolver returns the function that fills the credential column.
// Phase 6 extends it with cached passwords; until then every account shows
// its reference string.
func (a *App) credentialResolver(ctx context.Context, w *worker.Worker, cfg *config.Config, pol credPolicy, s *snapshot.Snapshot) func(collect.Account) secrets.Reference {
	base := secrets.Policy{Mode: secrets.ModeReference, References: pol.references}
	return func(acct collect.Account) secrets.Reference {
		ref := base.ReferenceOnly(s.Hostname, acct.Username)
		if stored := a.storedCredential(ctx, w, cfg, pol, s, acct); stored != nil {
			return *stored
		}
		return ref
	}
}
