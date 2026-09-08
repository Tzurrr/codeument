// Package cli wires the cobra commands. It holds no business logic: each
// command loads config, opens the journal and calls into the packages.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/user"
	"path/filepath"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/paths"
)

// Version is set at build time with -ldflags "-X .../cli.Version=v1.2.3".
var Version = "dev"

// App carries shared state between commands.
type App struct {
	configPath string
	jsonOut    bool
	verbose    bool
	quiet      bool

	cfgRes *config.Result
	store  *journal.Store
	out    io.Writer
	errOut io.Writer
}

// Execute runs the CLI.
func Execute() error {
	app := &App{out: os.Stdout, errOut: os.Stderr}
	root := app.rootCmd()
	return root.Execute()
}

func (a *App) rootCmd() *cobra.Command {
	root := &cobra.Command{
		Use:   "codeument",
		Short: "Documentation that writes itself from the work you do in the shell",
		Long: `codeument watches the commands you run, groups them into meaningful batches,
drafts documentation for each batch and lets you review before it is published
to your documentation platform. On servers it also keeps a living page of the
machine's state.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		PersistentPreRun: func(cmd *cobra.Command, _ []string) {
			level := slog.LevelWarn
			if a.verbose {
				level = slog.LevelDebug
			}
			slog.SetDefault(slog.New(slog.NewTextHandler(cmd.ErrOrStderr(), &slog.HandlerOptions{Level: level})))
		},
		PersistentPostRun: func(*cobra.Command, []string) {
			if a.store != nil {
				_ = a.store.Close()
				a.store = nil
			}
		},
	}
	root.PersistentFlags().StringVar(&a.configPath, "config", "", "config file (default: ~/.config/codeument/config.yaml or $CODEUMENT_CONFIG)")
	root.PersistentFlags().BoolVar(&a.jsonOut, "json", false, "machine-readable JSON output where supported")
	root.PersistentFlags().BoolVarP(&a.verbose, "verbose", "v", false, "debug logging")
	root.PersistentFlags().BoolVarP(&a.quiet, "quiet", "q", false, "suppress informational output")

	root.AddCommand(
		a.hookCmd(),
		a.ingestCmd(),
		a.statusCmd(),
		a.forgetCmd(),
		a.pauseCmd(),
		a.resumeCmd(),
		a.purgeCmd(),
		a.configCmd(),
		a.versionCmd(),
	)
	a.registerPhase2(root)
	a.registerPhase3(root)
	a.registerPhase5(root)
	a.registerPhase6(root)
	return root
}

// loadConfig loads (creating when needed) the config once per process.
func (a *App) loadConfig(cmd *cobra.Command) (*config.Config, error) {
	if a.cfgRes != nil {
		return a.cfgRes.Config, nil
	}
	res, err := config.Load(a.configPath)
	if err != nil {
		return nil, err
	}
	a.cfgRes = res
	if res.Created && !a.quiet {
		fmt.Fprintf(cmd.ErrOrStderr(), "codeument: created default config at %s\n", res.Path)
	}
	for _, w := range res.Warnings {
		slog.Warn("config", "warning", w)
	}
	if err := paths.EnsureDir(res.Config.DataDir); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	return res.Config, nil
}

// openStore opens the journal once per process.
func (a *App) openStore(cmd *cobra.Command) (*journal.Store, error) {
	if a.store != nil {
		return a.store, nil
	}
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return nil, err
	}
	s, err := journal.Open(cfg.JournalPath())
	if err != nil {
		return nil, fmt.Errorf("open journal: %w", err)
	}
	a.store = s
	return s, nil
}

func (a *App) printJSON(cmd *cobra.Command, v any) error {
	enc := json.NewEncoder(cmd.OutOrStdout())
	enc.SetIndent("", "  ")
	return enc.Encode(v)
}

func (a *App) infof(cmd *cobra.Command, format string, args ...any) {
	if a.quiet {
		return
	}
	fmt.Fprintf(cmd.OutOrStdout(), format, args...)
}

// hostIdentity returns hostname and username for events.
func hostIdentity() (host, username string) {
	host, _ = os.Hostname()
	if u, err := user.Current(); err == nil {
		username = u.Username
	} else {
		username = os.Getenv("USER")
	}
	return host, username
}

// selfPath is the absolute path of the running binary.
func selfPath() string {
	p, err := os.Executable()
	if err != nil {
		return "codeument"
	}
	if resolved, err := filepath.EvalSymlinks(p); err == nil {
		return resolved
	}
	return p
}

func ctx(cmd *cobra.Command) context.Context {
	if cmd.Context() != nil {
		return cmd.Context()
	}
	return context.Background()
}

func (a *App) versionCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "Print the version",
		RunE: func(cmd *cobra.Command, _ []string) error {
			if a.jsonOut {
				return a.printJSON(cmd, map[string]string{"version": Version})
			}
			fmt.Fprintln(cmd.OutOrStdout(), "codeument", Version)
			return nil
		},
	}
}
