package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/paths"
)

func (a *App) configCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show, initialise, validate or edit the configuration file",
	}
	cmd.AddCommand(a.configShowCmd(), a.configInitCmd(), a.configValidateCmd(), a.configSetCmd(), a.configPathCmd())
	return cmd
}

func (a *App) configPathCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "path",
		Short: "Print the config file path",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := a.configPath
			if p == "" {
				p = paths.UserConfigPath()
			}
			fmt.Fprintln(cmd.OutOrStdout(), p)
			return nil
		},
	}
}

func (a *App) configShowCmd() *cobra.Command {
	var reveal bool
	cmd := &cobra.Command{
		Use:   "show",
		Short: "Print the effective configuration (secrets masked)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			c := cfg.Masked()
			if reveal {
				c = cfg
			}
			if a.jsonOut {
				return a.printJSON(cmd, c)
			}
			data, err := c.Marshal()
			if err != nil {
				return err
			}
			fmt.Fprintf(cmd.OutOrStdout(), "# %s\n", a.cfgRes.Path)
			if a.cfgRes.SystemLoaded {
				fmt.Fprintf(cmd.OutOrStdout(), "# merged over %s\n", a.cfgRes.SystemPath)
			}
			_, err = cmd.OutOrStdout().Write(data)
			return err
		},
	}
	cmd.Flags().BoolVar(&reveal, "reveal", false, "print secret values")
	return cmd
}

func (a *App) configInitCmd() *cobra.Command {
	var force bool
	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write the default configuration file (use --force to overwrite)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			p := a.configPath
			if p == "" {
				p = paths.UserConfigPath()
			}
			if _, err := os.Stat(p); err == nil && !force {
				a.infof(cmd, "config already exists at %s (use --force to overwrite)\n", p)
				return nil
			}
			if err := paths.WriteFilePrivate(p, config.DefaultYAML()); err != nil {
				return err
			}
			a.infof(cmd, "wrote %s\n", p)
			return nil
		},
	}
	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing file")
	return cmd
}

func (a *App) configValidateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "validate",
		Short: "Check the configuration, file permissions and provider connectivity",
		RunE: func(cmd *cobra.Command, _ []string) error {
			cfg, err := a.loadConfig(cmd)
			if err != nil {
				return err
			}
			out := cmd.OutOrStdout()
			fmt.Fprintf(out, "config %s: ok (mode %s)\n", a.cfgRes.Path, cfg.Mode)
			for _, w := range a.cfgRes.Warnings {
				fmt.Fprintf(out, "warning: %s\n", w)
			}
			if st, err := os.Stat(a.cfgRes.Path); err == nil && st.Mode().Perm()&0o077 != 0 {
				fmt.Fprintf(out, "warning: %s is readable by others (perm %o); chmod 600 recommended\n", a.cfgRes.Path, st.Mode().Perm())
			}
			if st, err := os.Stat(cfg.DataDir); err == nil && st.Mode().Perm()&0o077 != 0 {
				fmt.Fprintf(out, "warning: %s is readable by others (perm %o); chmod 700 recommended\n", cfg.DataDir, st.Mode().Perm())
			}
			return a.validateProviders(cmd, cfg)
		},
	}
}

func (a *App) configSetCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "set KEY VALUE",
		Short: "Set one key in the config file, e.g. `config set llm.provider ollama`",
		Args:  cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := a.configPath
			if p == "" {
				p = paths.UserConfigPath()
			}
			if err := config.SetValue(p, args[0], args[1]); err != nil {
				return err
			}
			a.cfgRes = nil
			if _, err := a.loadConfig(cmd); err != nil {
				return fmt.Errorf("config now invalid: %w", err)
			}
			a.infof(cmd, "%s = %s\n", args[0], args[1])
			return nil
		},
	}
}
