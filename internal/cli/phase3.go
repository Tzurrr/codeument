package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/paths"
	"github.com/Tzurrr/codeument/internal/relay/api"
	"github.com/Tzurrr/codeument/internal/relay/client"
	"github.com/Tzurrr/codeument/internal/relay/server"
	"github.com/Tzurrr/codeument/internal/scheduler"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/worker"
)

func init() {
	newRelayEngine = func(cfg *config.Config) (engine.Engine, error) {
		token, err := os.ReadFile(cfg.Relay.TokenFile)
		if err != nil {
			return nil, fmt.Errorf("relay token %s: %w (run `codeument enroll`)", cfg.Relay.TokenFile, err)
		}
		c, err := client.New(client.Options{BaseURL: cfg.Relay.URL, Token: strings.TrimSpace(string(token)), CAFile: cfg.Relay.CAFile, Version: Version})
		if err != nil {
			return nil, err
		}
		return client.NewEngine(c), nil
	}
}

const kvRelayDefaults = "relay_defaults"

func (a *App) registerPhase3(root *cobra.Command) {
	root.AddCommand(a.enrollCmd(), a.relayCmd())
}

// cachedDefaults returns the relay defaults saved by the last tick/enroll.
func cachedDefaults(ctx context.Context, store *journal.Store) *engine.Defaults {
	v, err := store.GetKV(ctx, kvRelayDefaults)
	if err != nil {
		return nil
	}
	var d engine.Defaults
	if json.Unmarshal([]byte(v), &d) != nil {
		return nil
	}
	return &d
}

func saveDefaults(ctx context.Context, store *journal.Store, d *engine.Defaults) error {
	b, err := json.Marshal(d)
	if err != nil {
		return err
	}
	return store.SetKV(ctx, kvRelayDefaults, string(b))
}

func (a *App) tickHooks(cfg *config.Config, w *worker.Worker) scheduler.Hooks {
	h := scheduler.Hooks{}
	if cfg.Mode == config.ModeRelay && w != nil && w.Engine != nil {
		eng := w.Engine
		h.RefreshDefaults = func(ctx context.Context) error {
			d, err := eng.Defaults(ctx)
			if err != nil {
				return err
			}
			return saveDefaults(ctx, w.Store, d)
		}
	}
	h.Snapshot = a.snapshotHook(cfg, w)
	return h
}

// --- enroll -----------------------------------------------------------------

func (a *App) enrollCmd() *cobra.Command {
	var relayURL, code, caFile, scope string
	cmd := &cobra.Command{
		Use:   "enroll",
		Short: "Join a company relay with a one-time code and switch to relay mode",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if relayURL == "" || code == "" {
				return fmt.Errorf("--relay and --code are required (ask IT for a code: `codeument relay client add`)")
			}
			cfgPath := a.configPath
			if scope == "system" {
				if os.Geteuid() != 0 {
					return fmt.Errorf("--scope system needs root")
				}
				if cfgPath == "" {
					cfgPath = paths.SystemConfigPath()
				}
				a.configPath = cfgPath
			}
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			if caFile != "" {
				caFile = paths.Expand(caFile)
			}
			c, err := client.New(client.Options{BaseURL: relayURL, CAFile: caFile, Version: Version, Timeout: 30 * time.Second})
			if err != nil {
				return err
			}
			host, user := hostIdentity()
			c2, cancel := context.WithTimeout(ctx(cmd), 30*time.Second)
			defer cancel()
			resp, err := c.Enroll(c2, api.EnrollRequest{Code: code, Hostname: host, Username: user, OS: server.OSName(), Version: Version, Scope: scope})
			if err != nil {
				return err
			}
			tokenFile := cfg.Relay.TokenFile
			if scope == "system" {
				tokenFile = paths.SystemRelayTokenPath()
			}
			if err := paths.WriteFilePrivate(tokenFile, []byte(resp.Token+"\n")); err != nil {
				return err
			}
			for k, v := range map[string]string{"mode": config.ModeRelay, "relay.url": strings.TrimRight(relayURL, "/"), "relay.token_file": tokenFile, "relay.ca_file": caFile} {
				if err := config.SetValue(a.cfgRes.Path, k, v); err != nil {
					return err
				}
			}
			a.cfgRes = nil
			cfg, err = a.loadConfig(cmd)
			if err != nil {
				return err
			}
			c.Token = resp.Token
			eng := client.NewEngine(c)
			if d, err := eng.Defaults(c2); err == nil {
				if store, serr := a.openStore(cmd); serr == nil {
					_ = saveDefaults(c2, store, d)
				}
			}
			a.infof(cmd, "enrolled as %q (client %s) with relay %s\nmode: relay. What leaves this machine from now on:\n\n", resp.Name, resp.ClientID, relayURL)
			return a.printPrivacy(cmd, cfg)
		},
	}
	cmd.Flags().StringVar(&relayURL, "relay", "", "relay URL, e.g. https://relay.corp:8443")
	cmd.Flags().StringVar(&code, "code", "", "one-time enrollment code from IT")
	cmd.Flags().StringVar(&caFile, "ca", "", "PEM file with the CA that signed the relay certificate (private CAs)")
	cmd.Flags().StringVar(&scope, "scope", "user", "user or system (system enrols the root snapshot job)")
	return cmd
}

// --- relay ------------------------------------------------------------------

func (a *App) relayCmd() *cobra.Command {
	var cfgPath string
	cmd := &cobra.Command{Use: "relay", Short: "Run and administer the company relay"}
	cmd.PersistentFlags().StringVar(&cfgPath, "relay-config", "", "relay config file (default /etc/codeument/relay.yaml)")

	serve := &cobra.Command{
		Use:   "serve",
		Short: "Run the relay server",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := server.Load(cfgPath)
			if err != nil {
				return err
			}
			if res.Created {
				fmt.Fprintf(cmd.ErrOrStderr(), "codeument relay: created default config at %s (listening on loopback only until tls is configured)\n", res.Path)
			}
			lp, dp, pol, err := server.BuildProviders(res.Config, relaySecretsStore)
			if err != nil {
				return err
			}
			store, err := server.OpenStore(res.Config.DBPath)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			srv := server.New(res.Config, store, lp, dp, pol, Version)
			c, stop := signal.NotifyContext(ctx(cmd), os.Interrupt, syscall.SIGTERM)
			defer stop()
			go func() {
				t := time.NewTicker(6 * time.Hour)
				defer t.Stop()
				for {
					select {
					case <-c.Done():
						return
					case <-t.C:
						_ = store.PruneAudit(c, time.Duration(res.Config.Audit.RetentionDays)*24*time.Hour)
					}
				}
			}()
			return srv.ListenAndServe(c)
		},
	}

	initCmd := &cobra.Command{
		Use:   "init",
		Short: "Write the default relay config",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := cfgPath
			if p == "" {
				p = paths.RelayConfigPath()
			}
			if _, err := os.Stat(p); err == nil {
				a.infof(cmd, "relay config already exists at %s\n", p)
				return nil
			}
			if err := paths.WriteFilePrivate(p, server.DefaultYAML()); err != nil {
				return err
			}
			a.infof(cmd, "wrote %s\n", p)
			return nil
		},
	}

	check := &cobra.Command{
		Use:   "check",
		Short: "Validate the relay config and ping its providers",
		RunE: func(cmd *cobra.Command, _ []string) error {
			res, err := server.Load(cfgPath)
			if err != nil {
				return err
			}
			lp, dp, pol, err := server.BuildProviders(res.Config, relaySecretsStore)
			if err != nil {
				return err
			}
			c, cancel := context.WithTimeout(ctx(cmd), 60*time.Second)
			defer cancel()
			fmt.Fprintf(cmd.OutOrStdout(), "config %s: ok (listen %s, tls %v)\n", res.Path, res.Config.Listen, res.Config.HasTLS())
			if err := lp.Ping(c); err != nil {
				return fmt.Errorf("llm (%s): %w", lp.Name(), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "llm %s (%s): ok\n", lp.Name(), lp.Model())
			if err := dp.Ping(c); err != nil {
				return fmt.Errorf("docs (%s): %w", dp.Name(), err)
			}
			fmt.Fprintf(cmd.OutOrStdout(), "docs %s: ok\n", dp.Name())
			if pol.Store != nil {
				if err := pol.Store.Ping(c); err != nil {
					return fmt.Errorf("password manager (%s): %w", pol.Store.Name(), err)
				}
				fmt.Fprintf(cmd.OutOrStdout(), "password manager %s: ok\n", pol.Store.Name())
			}
			fmt.Fprintf(cmd.OutOrStdout(), "credential policy: %s\n", pol.Mode)
			return nil
		},
	}

	clientCmd := &cobra.Command{Use: "client", Short: "Manage enrolled clients"}
	var name string
	var ttl time.Duration
	add := &cobra.Command{
		Use:   "add",
		Short: "Create a one-time enrollment code for a client",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if name == "" {
				return fmt.Errorf("--name is required")
			}
			store, cfg, err := openRelayStore(cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			code, err := store.NewEnrollCode(ctx(cmd), name, ttl)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, map[string]any{"name": name, "code": code, "expires_in": ttl.String()})
			}
			fmt.Fprintf(cmd.OutOrStdout(), "enrollment code for %q (valid %s):\n\n  %s\n\non the client run:\n  codeument enroll --relay https://%s --code %s\n", name, ttl, code, cfg.Listen, code)
			return nil
		},
	}
	add.Flags().StringVar(&name, "name", "", "client name, e.g. alice-laptop or web-01")
	add.Flags().DurationVar(&ttl, "ttl", 24*time.Hour, "how long the code stays valid")

	list := &cobra.Command{
		Use:   "list",
		Short: "List enrolled clients",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, _, err := openRelayStore(cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			clients, err := store.ListClients(ctx(cmd))
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, clients)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "ID\tNAME\tHOST\tUSER\tOS\tVERSION\tSCOPE\tENROLLED\tLAST SEEN\tSTATE")
			for _, c := range clients {
				state := "active"
				if c.Revoked {
					state = "revoked"
				}
				seen := "-"
				if !c.LastSeen.IsZero() {
					seen = humanDuration(time.Since(c.LastSeen)) + " ago"
				}
				fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n", c.ID, c.Name, c.Hostname, c.Username, c.OS, c.Version, c.Scope, c.CreatedAt.Format("2006-01-02"), seen, state)
			}
			return tw.Flush()
		},
	}
	revoke := &cobra.Command{
		Use:   "revoke ID-OR-NAME",
		Short: "Revoke a client's token",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			store, _, err := openRelayStore(cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			n, err := store.RevokeClient(ctx(cmd), args[0])
			if err != nil {
				return err
			}
			a.infof(cmd, "revoked %d client(s)\n", n)
			return nil
		},
	}
	var limit int
	audit := &cobra.Command{
		Use:   "audit",
		Short: "Show recent relay activity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			store, _, err := openRelayStore(cfgPath)
			if err != nil {
				return err
			}
			defer func() { _ = store.Close() }()
			rows, err := store.RecentAudit(ctx(cmd), limit)
			if err != nil {
				return err
			}
			if a.jsonOut {
				return a.printJSON(cmd, rows)
			}
			tw := tabwriter.NewWriter(cmd.OutOrStdout(), 2, 4, 2, ' ', 0)
			fmt.Fprintln(tw, "AT\tCLIENT\tENDPOINT\tSTATUS\tEVENTS\tTOKENS IN/OUT\tDETAIL")
			for _, r := range rows {
				fmt.Fprintf(tw, "%s\t%s\t%s\t%d\t%d\t%d/%d\t%s\n", r.At.Format("01-02 15:04"), r.ClientID, r.Endpoint, r.Status, r.EventCount, r.TokensIn, r.TokensOut, truncate(r.Detail, 60))
			}
			return tw.Flush()
		},
	}
	audit.Flags().IntVar(&limit, "limit", 50, "rows to show")
	clientCmd.AddCommand(add, list, revoke)
	cmd.AddCommand(serve, initCmd, check, clientCmd, audit)
	return cmd
}

func openRelayStore(cfgPath string) (*server.Store, *server.Config, error) {
	res, err := server.Load(cfgPath)
	if err != nil {
		return nil, nil, err
	}
	store, err := server.OpenStore(res.Config.DBPath)
	if err != nil {
		return nil, nil, err
	}
	return store, res.Config, nil
}

// relaySecretsStore is set by Phase 6; nil means no password manager.
var relaySecretsStore func(config.Credentials) (secrets.Store, error)
