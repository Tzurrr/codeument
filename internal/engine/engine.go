// Package engine separates "what the client does" from "where inference and
// publishing happen". LocalEngine runs providers in-process (direct mode);
// RelayEngine forwards to a relay (relay mode). Everything above the engine
// is identical in both modes.
package engine

import (
	"context"
	"errors"
	"fmt"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/explain"
	"github.com/Tzurrr/codeument/internal/llm"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/summarize"
)

// Defaults are settings the relay pushes to clients; in direct mode they are
// derived from the local config.
type Defaults struct {
	Hints            []string          `json:"hints,omitempty"`
	DefaultLocation  model.Location    `json:"default_location"`
	SnapshotLocation model.Location    `json:"snapshot_location"`
	Ignore           []string          `json:"ignore,omitempty"`
	Unignore         []string          `json:"unignore,omitempty"`
	Weights          map[string]int    `json:"weights,omitempty"`
	ExtraPatterns    []string          `json:"extra_patterns,omitempty"`
	CredentialMode   string            `json:"credential_mode"`
	CaptureCreds     bool              `json:"capture_from_commands"`
	PromptOnReview   bool              `json:"prompt_on_review"`
	CredentialRefs   map[string]string `json:"credential_references,omitempty"`
	MinClientVersion string            `json:"min_client_version,omitempty"`
	ManagerName      string            `json:"manager_name,omitempty"`
}

// Engine is what the client talks to.
type Engine interface {
	Name() string
	Summarize(ctx context.Context, in summarize.Input) (*summarize.Output, error)
	Explain(ctx context.Context, in explain.Input) ([]explain.Explanation, error)
	Publish(ctx context.Context, doc docs.Document) (docs.PageRef, error)
	FindDoc(ctx context.Context, id string) (*docs.PageRef, error)
	GetDoc(ctx context.Context, ref docs.PageRef) (*docs.Document, error)
	Defaults(ctx context.Context) (*Defaults, error)
	StoreCredential(ctx context.Context, c secrets.Credential) (secrets.Reference, error)
	Ping(ctx context.Context) error
}

// ErrNoEngine is returned in local mode.
var ErrNoEngine = errors.New("mode is local: nothing is summarized or published (run `codeument init` or `codeument enroll`)")

// Local runs everything in-process.
type Local struct {
	LLM    llm.Provider
	Docs   docs.Provider
	Policy secrets.Policy
	Def    Defaults
}

func (l *Local) Name() string { return "local(" + l.LLM.Name() + "," + l.Docs.Name() + ")" }

func (l *Local) Summarize(ctx context.Context, in summarize.Input) (*summarize.Output, error) {
	return summarize.Summarize(ctx, l.LLM, in)
}

func (l *Local) Explain(ctx context.Context, in explain.Input) ([]explain.Explanation, error) {
	return explain.Run(ctx, l.LLM, in)
}

func (l *Local) Publish(ctx context.Context, doc docs.Document) (docs.PageRef, error) {
	if _, err := l.Docs.EnsureHierarchy(ctx, doc.Location); err != nil {
		return docs.PageRef{}, fmt.Errorf("ensure hierarchy: %w", err)
	}
	return l.Docs.Upsert(ctx, doc)
}

func (l *Local) FindDoc(ctx context.Context, id string) (*docs.PageRef, error) {
	return l.Docs.Find(ctx, id)
}

func (l *Local) GetDoc(ctx context.Context, ref docs.PageRef) (*docs.Document, error) {
	return l.Docs.Get(ctx, ref)
}

func (l *Local) Defaults(context.Context) (*Defaults, error) {
	d := l.Def
	return &d, nil
}

func (l *Local) StoreCredential(ctx context.Context, c secrets.Credential) (secrets.Reference, error) {
	return l.Policy.Apply(ctx, c)
}

func (l *Local) Ping(ctx context.Context) error {
	if err := l.LLM.Ping(ctx); err != nil {
		return fmt.Errorf("llm (%s): %w", l.LLM.Name(), err)
	}
	if err := l.Docs.Ping(ctx); err != nil {
		return fmt.Errorf("docs (%s): %w", l.Docs.Name(), err)
	}
	if l.Policy.Store != nil {
		if err := l.Policy.Store.Ping(ctx); err != nil {
			return fmt.Errorf("password manager (%s): %w", l.Policy.Store.Name(), err)
		}
	}
	return nil
}

// Unavailable is the engine used in local mode: every operation that would
// need a provider fails with ErrNoEngine, lookups report nothing.
type Unavailable struct{}

func (Unavailable) Name() string { return "local" }
func (Unavailable) Summarize(context.Context, summarize.Input) (*summarize.Output, error) {
	return nil, ErrNoEngine
}
func (Unavailable) Explain(context.Context, explain.Input) ([]explain.Explanation, error) {
	return nil, ErrNoEngine
}
func (Unavailable) Publish(context.Context, docs.Document) (docs.PageRef, error) {
	return docs.PageRef{}, ErrNoEngine
}
func (Unavailable) FindDoc(context.Context, string) (*docs.PageRef, error) { return nil, nil }
func (Unavailable) GetDoc(context.Context, docs.PageRef) (*docs.Document, error) {
	return nil, docs.ErrNotFound
}
func (Unavailable) Defaults(context.Context) (*Defaults, error) { return &Defaults{}, nil }
func (Unavailable) StoreCredential(context.Context, secrets.Credential) (secrets.Reference, error) {
	return secrets.Reference{}, ErrNoEngine
}
func (Unavailable) Ping(context.Context) error { return ErrNoEngine }
