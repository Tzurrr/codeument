package cli

import (
	"context"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/redact"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/snapshot"
	"github.com/Tzurrr/codeument/internal/snapshot/collect"
	"github.com/Tzurrr/codeument/internal/worker"
)

// The functions below are replaced as later phases land.

func (a *App) registerPhase6(*cobra.Command) {}

func (a *App) storeCaptured(*cobra.Command, *config.Config, string, []redact.Captured) {}

// storedCredential returns a credential from the local cache when Phase 6 is
// wired up; until then accounts show only their reference string.
func (a *App) storedCredential(context.Context, *worker.Worker, *config.Config, credPolicy, *snapshot.Snapshot, collect.Account) *secrets.Reference {
	return nil
}

func (a *App) credentialPrompt(*config.Config) func(context.Context, *model.DraftRecord) error {
	return nil
}
