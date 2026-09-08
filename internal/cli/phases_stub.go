package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/redact"
	"github.com/Tzurrr/codeument/internal/worker"
)

// The functions below are replaced as later phases land.

func (a *App) registerPhase5(*cobra.Command) {}
func (a *App) registerPhase6(*cobra.Command) {}

func (a *App) storeCaptured(*cobra.Command, *config.Config, string, []redact.Captured) {}

func (a *App) snapshotHook(*config.Config, *worker.Worker) func(context.Context) error { return nil }

func (a *App) credentialPrompt(*config.Config) func(context.Context, *model.DraftRecord) error {
	return nil
}
