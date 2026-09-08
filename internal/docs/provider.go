// Package docs defines the documentation platform interface and the
// canonical Document type. Providers (markdown, confluence) implement it.
package docs

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"

	"github.com/Tzurrr/codeument/internal/model"
)

// Document is a page in canonical Markdown with a stable id owned by
// codeument. Providers upsert by ID.
type Document struct {
	ID       string            `json:"id"`
	Title    string            `json:"title"`
	BodyMD   string            `json:"body_md"`
	Location model.Location    `json:"location"`
	Labels   []string          `json:"labels"`
	Meta     map[string]string `json:"meta"`
	// RestrictToGroups, when set, asks the provider to limit read access to
	// these groups (used for pages carrying inline credentials).
	RestrictToGroups []string `json:"restrict_to_groups,omitempty"`
}

// ContentHash identifies the document content for change detection.
func (d Document) ContentHash() string {
	h := sha256.New()
	h.Write([]byte(d.Title))
	h.Write([]byte{0})
	h.Write([]byte(d.BodyMD))
	h.Write([]byte{0})
	h.Write([]byte(strings.Join(d.Labels, ",")))
	return hex.EncodeToString(h.Sum(nil))[:16]
}

// PageRef points at the provider's copy of a document.
type PageRef struct {
	Provider   string `json:"provider"`
	ProviderID string `json:"provider_id"`
	URL        string `json:"url"`
	Version    int    `json:"version"`
	Title      string `json:"title,omitempty"`
}

// Provider is a documentation platform.
type Provider interface {
	Name() string
	// Find returns the page for a doc id, or (nil, nil) when none exists.
	Find(ctx context.Context, docID string) (*PageRef, error)
	// Get fetches a page's content.
	Get(ctx context.Context, ref PageRef) (*Document, error)
	// Upsert creates or updates the page for doc.ID.
	Upsert(ctx context.Context, doc Document) (PageRef, error)
	// EnsureHierarchy creates missing parents and returns the parent id.
	EnsureHierarchy(ctx context.Context, loc model.Location) (string, error)
	Ping(ctx context.Context) error
}

// ErrNotFound is returned by Get when the page is gone.
var ErrNotFound = errors.New("docs: page not found")

// Config is the provider-independent view of config.Docs.
type Config struct {
	Provider string
	Markdown struct {
		Root string
	}
	Confluence struct {
		BaseURL  string
		Email    string
		APIToken string
		Space    string
	}
	DefaultLocation model.Location
}

// Factory builds a provider from config.
type Factory func(cfg Config) (Provider, error)

var factories = map[string]Factory{}

// Register adds a provider factory.
func Register(name string, f Factory) { factories[name] = f }

// New constructs the provider named in cfg.Provider.
func New(cfg Config) (Provider, error) {
	f, ok := factories[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("docs: unknown provider %q", cfg.Provider)
	}
	return f(cfg)
}

// Slug turns a title into a filesystem/URL friendly name.
func Slug(title string) string {
	var b strings.Builder
	lastDash := true
	for _, r := range strings.ToLower(title) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9':
			b.WriteRune(r)
			lastDash = false
		case r > 127 && (r != ' '):
			b.WriteRune(r)
			lastDash = false
		default:
			if !lastDash {
				b.WriteByte('-')
				lastDash = true
			}
		}
	}
	s := strings.Trim(b.String(), "-")
	if s == "" {
		s = "untitled"
	}
	if len(s) > 80 {
		s = s[:80]
	}
	return s
}
