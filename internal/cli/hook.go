package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/capture"
	"github.com/Tzurrr/codeument/internal/paths"
)

func (a *App) hookCmd() *cobra.Command {
	var install bool
	cmd := &cobra.Command{
		Use:   "hook [bash|zsh|fish]",
		Short: "Print (or install) the shell hook that records your commands",
		Long: `Print the shell snippet that records commands into the local journal.

  bash/zsh:  eval "$(codeument hook bash)"
  fish:      codeument hook fish | source

With --install the line above is appended to your shell rc file once.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			shell := ""
			if len(args) == 1 {
				shell = args[0]
			} else {
				shell = detectShell()
			}
			if shell == "" {
				return fmt.Errorf("could not detect your shell; pass one of %s", strings.Join(capture.Shells, ", "))
			}
			opts := capture.HookOptions{Binary: selfPath(), NotifyFile: cfg.NotifyPath(), SeenDir: cfg.SeenDir()}
			if install {
				return a.installHook(cmd, shell, opts.Binary)
			}
			out, err := capture.RenderHook(shell, opts)
			if err != nil {
				return err
			}
			_, err = fmt.Fprint(cmd.OutOrStdout(), out)
			return err
		},
	}
	cmd.Flags().BoolVar(&install, "install", false, "append the hook line to your shell rc file")
	return cmd
}

func (a *App) installHook(cmd *cobra.Command, shell, binary string) error {
	rc := capture.RCFile(shell, paths.Home())
	if rc == "" {
		return fmt.Errorf("unsupported shell %q", shell)
	}
	line := capture.RCLine(shell, binary)
	existing, err := os.ReadFile(rc)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	if strings.Contains(string(existing), "codeument hook") {
		a.infof(cmd, "codeument hook already present in %s\n", rc)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(rc), 0o755); err != nil {
		return err
	}
	f, err := os.OpenFile(rc, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	defer f.Close()
	prefix := "\n"
	if len(existing) == 0 || strings.HasSuffix(string(existing), "\n") {
		prefix = ""
	}
	if _, err := fmt.Fprintf(f, "%s# codeument: record shell work into the local journal\n%s\n", prefix, line); err != nil {
		return err
	}
	a.infof(cmd, "added to %s:\n  %s\nopen a new shell (or source the file) to start capturing.\n", rc, line)
	return nil
}

func detectShell() string {
	sh := filepath.Base(os.Getenv("SHELL"))
	for _, s := range capture.Shells {
		if sh == s {
			return s
		}
	}
	return ""
}
