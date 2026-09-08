package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"golang.org/x/term"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/credcache"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/redact"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/secrets/factory"
	"github.com/Tzurrr/codeument/internal/snapshot"
	"github.com/Tzurrr/codeument/internal/snapshot/collect"
	"github.com/Tzurrr/codeument/internal/worker"
)

func init() {
	// Both sides build password-manager stores from the same factory.
	relaySecretsStore = factory.New
	newSecretsStore = func(cfg *config.Config) (secrets.Store, error) { return factory.New(cfg.Credentials) }
}

func (a *App) registerPhase6(root *cobra.Command) {
	root.AddCommand(a.credentialsCmd())
}

// openCredCache opens the encrypted credential cache.
func openCredCache(cfg *config.Config) (*credcache.Cache, error) {
	return credcache.Open(cfg.CredCachePath(), cfg.CredCacheKeyPath())
}

// storeCaptured saves credentials the redactor extracted from a command.
// Values never reach the journal; only this encrypted cache.
func (a *App) storeCaptured(cmd *cobra.Command, cfg *config.Config, host string, captured []redact.Captured) {
	if !cfg.Credentials.CaptureFromCommands || len(captured) == 0 {
		return
	}
	cache, err := openCredCache(cfg)
	if err != nil {
		return // capture is best-effort; never break the shell hook
	}
	defer func() { _ = cache.Close() }()
	c := ctx(cmd)
	for _, cap := range captured {
		if cap.Kind != "password" && cap.Kind != "basic_auth" && cap.Kind != "url_password" {
			continue
		}
		user := cap.Username
		if user == "" {
			continue // without a user we cannot attach it to an account
		}
		_ = cache.Put(c, credcache.Entry{
			Host: host, Username: user, Kind: kindFor(cap.Program), Password: cap.Value,
			Source: credcache.SourceCommand, Program: cap.Program,
			Notes: "seen in a command on " + time.Now().Format("2006-01-02"),
		})
	}
}

func kindFor(program string) string {
	switch program {
	case "mysql", "psql", "mongosh", "redis-cli":
		return "db"
	case "chpasswd", "passwd", "useradd", "usermod", "":
		return "os"
	}
	return "service"
}

// storedCredential resolves an account's credential through the policy: a
// cached password is pushed to the manager (or inlined) and the resulting
// reference is what the page shows.
func (a *App) storedCredential(ctx context.Context, w *worker.Worker, cfg *config.Config, pol credPolicy, s *snapshot.Snapshot, acct collect.Account) *secrets.Reference {
	if pol.mode == secrets.ModeReference {
		return nil
	}
	cache, err := openCredCache(cfg)
	if err != nil {
		return nil
	}
	defer func() { _ = cache.Close() }()
	entry, err := cache.Get(ctx, s.Hostname, acct.Username)
	if err != nil {
		return nil
	}
	if pol.mode == secrets.ModeInline {
		return &secrets.Reference{Mode: secrets.ModeInline, Password: entry.Password}
	}
	if w == nil || w.Engine == nil {
		return nil
	}
	ref, err := w.Engine.StoreCredential(ctx, entry.Credential(s.MachineID))
	if err != nil {
		return nil
	}
	_ = cache.MarkPushed(ctx, entry.ID, ref)
	return &ref
}

// credentialPrompt asks for missing passwords before a server page is
// published. It is wired into the review screen and `snapshot --publish`.
func (a *App) credentialPrompt(cfg *config.Config) func(context.Context, *model.DraftRecord) error {
	if !cfg.Credentials.PromptOnReview || cfg.Credentials.Mode == config.CredentialsReference {
		return nil
	}
	return func(ctx context.Context, d *model.DraftRecord) error {
		return nil // drafts carry no accounts; snapshots prompt through PromptForAccounts
	}
}

// PromptForAccounts asks for a password per account that has none cached.
func (a *App) promptForAccounts(cmd *cobra.Command, cfg *config.Config, pol credPolicy, s *snapshot.Snapshot) error {
	if !pol.promptOnReview || pol.mode == secrets.ModeReference {
		return nil
	}
	in, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(in.Fd())) {
		return nil // non-interactive: keep references
	}
	cache, err := openCredCache(cfg)
	if err != nil {
		return err
	}
	defer func() { _ = cache.Close() }()
	c := ctx(cmd)
	sugg, _ := cache.Suggestions(c, s.Hostname)
	byUser := map[string]credcache.Entry{}
	for _, e := range sugg {
		byUser[e.Username] = e
	}
	out := cmd.OutOrStdout()
	fmt.Fprintf(out, "\nCredential policy is %q. Enter passwords to record, or press enter to skip.\n", pol.mode)
	for _, acct := range s.Accounts {
		if _, err := cache.Get(c, s.Hostname, acct.Username); err == nil {
			continue
		}
		hint := ""
		if e, ok := byUser[acct.Username]; ok {
			hint = fmt.Sprintf(" (one was seen in `%s` on %s; press y to use it)", e.Program, e.CreatedAt.Format("Jan 2"))
		}
		fmt.Fprintf(out, "  %s@%s%s: ", acct.Username, s.Hostname, hint)
		pw, err := term.ReadPassword(int(in.Fd()))
		fmt.Fprintln(out)
		if err != nil {
			return err
		}
		value := strings.TrimSpace(string(pw))
		if value == "" {
			continue
		}
		if value == "y" && hint != "" {
			value = byUser[acct.Username].Password
		}
		if err := cache.Put(c, credcache.Entry{Host: s.Hostname, Username: acct.Username, Kind: "os", Password: value, Source: credcache.SourceReview}); err != nil {
			return err
		}
	}
	return nil
}

func (a *App) credentialsCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "credentials",
		Short: "Manage the encrypted local credential cache",
		Long: `Passwords you record for server accounts live in an encrypted cache on this
machine. Depending on the credential policy they are referenced only, pushed
to the company password manager, or written onto the server page.`,
	}

	var host, username, kind string
	add := &cobra.Command{
		Use:   "add",
		Short: "Record a password for an account (asked for, never passed as an argument)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if username == "" {
				return errors.New("--user is required")
			}
			if host == "" {
				host, _ = hostIdentity()
			}
			pw, err := readSecret(cmd, fmt.Sprintf("password for %s@%s: ", username, host))
			if err != nil {
				return err
			}
			if pw == "" {
				return errors.New("empty password, nothing stored")
			}
			cache, err := openCredCache(cfg)
			if err != nil {
				return err
			}
			defer func() { _ = cache.Close() }()
			if err := cache.Put(ctx(cmd), credcache.Entry{Host: host, Username: username, Kind: kind, Password: pw, Source: credcache.SourceReview}); err != nil {
				return err
			}
			a.infof(cmd, "stored %s@%s in the local cache (%s)\n", username, host, cfg.CredCachePath())
			if cfg.Credentials.Mode == config.CredentialsReference {
				a.infof(cmd, "credential policy is 'reference': the page will show the reference string, not this password.\n")
			}
			return nil
		},
	}
	add.Flags().StringVar(&host, "host", "", "host the account belongs to (default: this machine)")
	add.Flags().StringVar(&username, "user", "", "account name")
	add.Flags().StringVar(&kind, "kind", "os", "os, db or service")

	list := &cobra.Command{
		Use:   "list",
		Short: "List cached credentials (passwords are never printed)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			cache, err := openCredCache(cfg)
			if err != nil {
				return err
			}
			defer func() { _ = cache.Close() }()
			entries, err := cache.List(ctx(cmd))
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, entries) // Entry omits the password
			}
			if len(entries) == 0 {
				a.infof(cmd, "no cached credentials\n")
				return nil
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "HOST\tUSER\tKIND\tSOURCE\tRECORDED\tPUSHED\tREFERENCE")
			for _, e := range entries {
				pushed := "-"
				if e.PushedAt != nil {
					pushed = e.PushedAt.Format("2006-01-02")
				}
				src := e.Source
				if e.Program != "" {
					src += " (" + e.Program + ")"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", e.Host, e.Username, e.Kind, src, e.CreatedAt.Format("2006-01-02"), pushed, e.Reference.Ref)
			}
			return tw.Flush()
		},
	}

	push := &cobra.Command{
		Use:   "push",
		Short: "Send cached credentials to the configured password manager",
		RunE: func(cmd *cobra.Command, _ []string) error {
			w, cfg, _, err := a.buildWorker(cmd)
			if err != nil {
				return err
			}
			if w.Engine == nil {
				return errors.New("mode is local: configure direct or relay mode first")
			}
			c := ctx(cmd)
			pol := a.credentialPolicy(c, cfg, w)
			if pol.mode != config.CredentialsManager {
				return fmt.Errorf("credential policy is %q, not 'manager': nothing to push", pol.mode)
			}
			cache, err := openCredCache(cfg)
			if err != nil {
				return err
			}
			defer func() { _ = cache.Close() }()
			entries, err := cache.List(c)
			if err != nil {
				return err
			}
			machineID := collect.MachineID(collect.Default)
			pushed := 0
			for _, e := range entries {
				ref, err := w.Engine.StoreCredential(c, e.Credential(machineID))
				if err != nil {
					fmt.Fprintf(cmd.ErrOrStderr(), "  %s@%s: %v\n", e.Username, e.Host, err)
					continue
				}
				if err := cache.MarkPushed(c, e.ID, ref); err != nil {
					return err
				}
				a.infof(cmd, "  %s@%s -> %s\n", e.Username, e.Host, ref.Ref)
				pushed++
			}
			a.infof(cmd, "pushed %d of %d credential(s)\n", pushed, len(entries))
			return nil
		},
	}

	var all bool
	forget := &cobra.Command{
		Use:   "forget",
		Short: "Delete cached credentials",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if !all && username == "" && host == "" {
				return errors.New("pass --all, or --host and/or --user")
			}
			cache, err := openCredCache(cfg)
			if err != nil {
				return err
			}
			defer func() { _ = cache.Close() }()
			h, u := host, username
			if all {
				h, u = "", ""
			}
			n, err := cache.Forget(ctx(cmd), h, u)
			if err != nil {
				return err
			}
			a.infof(cmd, "forgot %d credential(s)\n", n)
			return nil
		},
	}
	forget.Flags().StringVar(&host, "host", "", "only this host")
	forget.Flags().StringVar(&username, "user", "", "only this account (with --host)")
	forget.Flags().BoolVar(&all, "all", false, "wipe the cache")

	cmd.AddCommand(add, list, push, forget)
	return cmd
}

// readSecret prompts for a value without echoing it.
func readSecret(cmd *cobra.Command, prompt string) (string, error) {
	fmt.Fprint(cmd.OutOrStdout(), prompt)
	if f, ok := cmd.InOrStdin().(*os.File); ok && term.IsTerminal(int(f.Fd())) {
		pw, err := term.ReadPassword(int(f.Fd()))
		fmt.Fprintln(cmd.OutOrStdout())
		return strings.TrimSpace(string(pw)), err
	}
	line, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	return strings.TrimSpace(line), err
}
