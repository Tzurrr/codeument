package cli

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/spf13/cobra"

	"github.com/Tzurrr/codeument/internal/config"
	"github.com/Tzurrr/codeument/internal/docs"
	_ "github.com/Tzurrr/codeument/internal/docs/fake"     // register
	_ "github.com/Tzurrr/codeument/internal/docs/markdown" // register
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/journal"
	"github.com/Tzurrr/codeument/internal/llm"
	_ "github.com/Tzurrr/codeument/internal/llm/anthropic" // register
	_ "github.com/Tzurrr/codeument/internal/llm/fake"      // register
	_ "github.com/Tzurrr/codeument/internal/llm/ollama"    // register
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/worker"
)

// engineHooks lets later phases supply engines and stores without import
// cycles in this package.
var (
	newRelayEngine  func(cfg *config.Config) (engine.Engine, error)
	newSecretsStore func(cfg *config.Config) (secrets.Store, error)
)

// buildEngine constructs the engine for the configured mode, or ErrNoEngine
// in local mode.
func (a *App) buildEngine(cfg *config.Config) (engine.Engine, error) {
	switch cfg.Mode {
	case config.ModeLocal:
		return nil, engine.ErrNoEngine
	case config.ModeRelay:
		if newRelayEngine == nil {
			return nil, fmt.Errorf("relay mode is not available in this build")
		}
		return newRelayEngine(cfg)
	}
	lp, err := llm.New(llmConfig(cfg))
	if err != nil {
		return nil, err
	}
	dp, err := docs.New(docsConfig(cfg))
	if err != nil {
		return nil, err
	}
	pol := secrets.Policy{Mode: cfg.Credentials.Mode, CaptureFromCommands: cfg.Credentials.CaptureFromCommands, PromptOnReview: cfg.Credentials.PromptOnReview, References: cfg.Snapshot.CredentialReferences, RestrictGroups: cfg.Credentials.Inline.PageRestrictionGroups}
	if cfg.Credentials.Manager.Provider != "" && newSecretsStore != nil {
		st, err := newSecretsStore(cfg)
		if err != nil {
			return nil, err
		}
		pol.Store = st
	}
	return &engine.Local{LLM: lp, Docs: dp, Policy: pol, Def: defaultsFrom(cfg)}, nil
}

func defaultsFrom(cfg *config.Config) engine.Defaults {
	return engine.Defaults{
		Hints: cfg.Hints, DefaultLocation: cfg.Docs.DefaultLocation, SnapshotLocation: cfg.Snapshot.DocLocation,
		Ignore: cfg.Capture.Ignore, Unignore: cfg.Capture.Unignore, Weights: cfg.Capture.Weights, ExtraPatterns: cfg.Redact.ExtraPatterns,
		CredentialMode: cfg.Credentials.Mode, CaptureCreds: cfg.Credentials.CaptureFromCommands, PromptOnReview: cfg.Credentials.PromptOnReview,
		CredentialRefs: cfg.Snapshot.CredentialReferences, ManagerName: cfg.Credentials.Manager.Provider,
	}
}

func llmConfig(cfg *config.Config) llm.Config {
	var c llm.Config
	c.Provider = cfg.LLM.Provider
	c.Anthropic.APIKey = cfg.LLM.Anthropic.APIKey
	c.Anthropic.Model = cfg.LLM.Anthropic.Model
	c.Anthropic.MaxTokens = cfg.LLM.Anthropic.MaxTokens
	c.Anthropic.Fallbacks = cfg.LLM.Anthropic.Fallbacks
	c.Ollama.BaseURL = cfg.LLM.Ollama.BaseURL
	c.Ollama.Model = cfg.LLM.Ollama.Model
	c.Ollama.NumCtx = cfg.LLM.Ollama.NumCtx
	return c
}

func docsConfig(cfg *config.Config) docs.Config {
	var c docs.Config
	c.Provider = cfg.Docs.Provider
	c.Markdown.Root = cfg.Docs.Markdown.Root
	c.Confluence.BaseURL = cfg.Docs.Confluence.BaseURL
	c.Confluence.Email = cfg.Docs.Confluence.Email
	c.Confluence.APIToken = cfg.Docs.Confluence.APIToken
	c.Confluence.Space = cfg.Docs.Confluence.Space
	c.DefaultLocation = cfg.Docs.DefaultLocation
	return c
}

// buildWorker wires a worker for the current mode. In local mode the worker
// has no engine and callers must check for ErrNoEngine before summarizing.
func (a *App) buildWorker(cmd *cobra.Command) (*worker.Worker, *config.Config, *journal.Store, error) {
	cfg, err := a.loadConfig(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	store, err := a.openStore(cmd)
	if err != nil {
		return nil, nil, nil, err
	}
	eng, err := a.buildEngine(cfg)
	if err != nil && err != engine.ErrNoEngine {
		return nil, nil, nil, err
	}
	host, user := hostIdentity()
	w := &worker.Worker{Store: store, Engine: eng, Hostname: host, Username: user, NotifyPath: cfg.NotifyPath(), Hints: cfg.Hints, Location: cfg.Docs.DefaultLocation, Log: slog.Default()}
	if cfg.Notify.Style == "silent" {
		w.NotifyPath = ""
	}
	if eng != nil {
		if d, err := eng.Defaults(context.Background()); err == nil && d != nil {
			applyDefaults(w, cfg, d)
		}
	}
	return w, cfg, store, nil
}

// applyDefaults merges relay/engine defaults under the local config.
func applyDefaults(w *worker.Worker, cfg *config.Config, d *engine.Defaults) {
	if len(d.Hints) > 0 {
		w.Hints = append(append([]string{}, d.Hints...), cfg.Hints...)
	}
	if cfg.Mode == config.ModeRelay {
		if d.DefaultLocation.Space != "" {
			w.Location = d.DefaultLocation
		}
	}
	if w.Location.Space == "" {
		w.Location = model.Location{Space: "OPS", ParentPath: []string{"Runbooks"}}
	}
}
