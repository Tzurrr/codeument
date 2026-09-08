// Package fake is an in-memory docs.Provider for tests.
package fake

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/model"
)

func init() {
	docs.Register("fake", func(docs.Config) (docs.Provider, error) { return New(), nil })
}

// Provider keeps pages in a map.
type Provider struct {
	mu    sync.Mutex
	Pages map[string]docs.Document
	Refs  map[string]docs.PageRef
	Err   error
}

// New returns an empty provider.
func New() *Provider {
	return &Provider{Pages: map[string]docs.Document{}, Refs: map[string]docs.PageRef{}}
}

func (p *Provider) Name() string               { return "fake" }
func (p *Provider) Ping(context.Context) error { return p.Err }

func (p *Provider) Find(_ context.Context, docID string) (*docs.PageRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if r, ok := p.Refs[docID]; ok {
		return &r, nil
	}
	return nil, nil
}

func (p *Provider) Get(_ context.Context, ref docs.PageRef) (*docs.Document, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	for id, r := range p.Refs {
		if r.ProviderID == ref.ProviderID {
			d := p.Pages[id]
			return &d, nil
		}
	}
	return nil, docs.ErrNotFound
}

func (p *Provider) EnsureHierarchy(_ context.Context, loc model.Location) (string, error) {
	return strings.Join(append([]string{loc.Space}, loc.ParentPath...), "/"), nil
}

func (p *Provider) Upsert(_ context.Context, doc docs.Document) (docs.PageRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.Err != nil {
		return docs.PageRef{}, p.Err
	}
	ref, ok := p.Refs[doc.ID]
	if !ok {
		ref = docs.PageRef{Provider: "fake", ProviderID: fmt.Sprintf("page-%d", len(p.Refs)+1), URL: "fake://" + doc.ID}
	}
	ref.Version++
	ref.Title = doc.Title
	p.Refs[doc.ID] = ref
	p.Pages[doc.ID] = doc
	return ref, nil
}

// IDs returns stored doc ids in order.
func (p *Provider) IDs() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	var out []string
	for id := range p.Pages {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}
