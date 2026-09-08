package cli

import (
	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/redact"
)

// The functions below are replaced as later phases land.

func (a *App) registerPhase2(*cobra.Command) {}
func (a *App) registerPhase3(*cobra.Command) {}
func (a *App) registerPhase5(*cobra.Command) {}
func (a *App) registerPhase6(*cobra.Command) {}

func (a *App) validateProviders(*cobra.Command, *config.Config) error { return nil }

func (a *App) storeCaptured(*cobra.Command, *config.Config, string, []redact.Captured) {}
