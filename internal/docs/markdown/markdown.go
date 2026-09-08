// Package markdown is a docs.Provider that writes pages as Markdown files
// with YAML front matter under a root directory. It needs no setup and
// doubles as a docs-as-code target.
package markdown

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/model"
	"github.com/Tzurrr/codeument/internal/paths"
)

func init() {
	docs.Register("markdown", func(cfg docs.Config) (docs.Provider, error) {
		return New(cfg.Markdown.Root)
	})
}

// Provider writes to a directory tree.
type Provider struct {
	root string
	mu   sync.Mutex
}

// New creates the root directory when needed.
func New(root string) (*Provider, error) {
	if root == "" {
		return nil, errors.New("markdown: root directory is empty")
	}
	root = paths.Expand(root)
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, err
	}
	return &Provider{root: root}, nil
}

// Root returns the directory.
func (p *Provider) Root() string { return p.root }

func (p *Provider) Name() string { return "markdown" }

// Ping checks the root is writable.
func (p *Provider) Ping(context.Context) error {
	f, err := os.CreateTemp(p.root, ".ping-*")
	if err != nil {
		return fmt.Errorf("markdown: root not writable: %w", err)
	}
	name := f.Name()
	_ = f.Close()
	return os.Remove(name)
}

type frontMatter struct {
	CodeumentID string            `yaml:"codeument_id"`
	Title       string            `yaml:"title"`
	Updated     string            `yaml:"updated"`
	Labels      []string          `yaml:"labels,omitempty"`
	Meta        map[string]string `yaml:"meta,omitempty"`
	Version     int               `yaml:"version"`
	Hash        string            `yaml:"hash"`
}

type index struct {
	Pages map[string]indexEntry `json:"pages"`
}

type indexEntry struct {
	Path    string `json:"path"`
	Title   string `json:"title"`
	Version int    `json:"version"`
}

func (p *Provider) indexPath() string { return filepath.Join(p.root, ".codeument-index.json") }

func (p *Provider) loadIndex() index {
	var idx index
	data, err := os.ReadFile(p.indexPath())
	if err == nil {
		_ = json.Unmarshal(data, &idx)
	}
	if idx.Pages == nil {
		idx.Pages = map[string]indexEntry{}
	}
	return idx
}

func (p *Provider) saveIndex(idx index) error {
	data, err := json.MarshalIndent(idx, "", "  ")
	if err != nil {
		return err
	}
	return writeAtomic(p.indexPath(), data, 0o644)
}

// Find looks the doc id up in the index, falling back to scanning front
// matter when the index is missing or stale.
func (p *Provider) Find(_ context.Context, docID string) (*docs.PageRef, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	idx := p.loadIndex()
	if e, ok := idx.Pages[docID]; ok {
		if _, err := os.Stat(filepath.Join(p.root, e.Path)); err == nil {
			return &docs.PageRef{Provider: "markdown", ProviderID: e.Path, URL: "file://" + filepath.Join(p.root, e.Path), Version: e.Version, Title: e.Title}, nil
		}
	}
	var found *docs.PageRef
	err := filepath.WalkDir(p.root, func(path string, d fs.DirEntry, err error) error {
		if err != nil || found != nil {
			return nil
		}
		if d.IsDir() {
			if strings.HasPrefix(d.Name(), ".") && path != p.root {
				return fs.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(d.Name(), ".md") {
			return nil
		}
		fm, _, err := readPage(path)
		if err != nil || fm.CodeumentID != docID {
			return nil
		}
		rel, _ := filepath.Rel(p.root, path)
		found = &docs.PageRef{Provider: "markdown", ProviderID: rel, URL: "file://" + path, Version: fm.Version, Title: fm.Title}
		idx.Pages[docID] = indexEntry{Path: rel, Title: fm.Title, Version: fm.Version}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if found != nil {
		_ = p.saveIndex(idx)
	}
	return found, nil
}

// Get reads a page back.
func (p *Provider) Get(_ context.Context, ref docs.PageRef) (*docs.Document, error) {
	fm, body, err := readPage(filepath.Join(p.root, ref.ProviderID))
	if errors.Is(err, os.ErrNotExist) {
		return nil, docs.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(ref.ProviderID)
	loc := model.Location{}
	if dir != "." {
		parts := strings.Split(filepath.ToSlash(dir), "/")
		loc.Space = parts[0]
		loc.ParentPath = parts[1:]
	}
	return &docs.Document{ID: fm.CodeumentID, Title: fm.Title, BodyMD: body, Labels: fm.Labels, Meta: fm.Meta, Location: loc}, nil
}

// EnsureHierarchy creates the directory for a location.
func (p *Provider) EnsureHierarchy(_ context.Context, loc model.Location) (string, error) {
	dir := p.dirFor(loc)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", err
	}
	rel, _ := filepath.Rel(p.root, dir)
	return rel, nil
}

func (p *Provider) dirFor(loc model.Location) string {
	parts := []string{p.root}
	if loc.Space != "" {
		parts = append(parts, docs.Slug(loc.Space))
	}
	for _, s := range loc.ParentPath {
		parts = append(parts, docs.Slug(s))
	}
	return filepath.Join(parts...)
}

// Upsert writes the page, keeping its path when it already exists.
func (p *Provider) Upsert(ctx context.Context, doc docs.Document) (docs.PageRef, error) {
	if doc.ID == "" {
		return docs.PageRef{}, errors.New("markdown: document has no id")
	}
	existing, err := p.Find(ctx, doc.ID)
	if err != nil {
		return docs.PageRef{}, err
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	version := 1
	var rel string
	if existing != nil {
		rel = existing.ProviderID
		version = existing.Version + 1
	} else {
		dir := p.dirFor(doc.Location)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return docs.PageRef{}, err
		}
		base := docs.Slug(doc.Title)
		candidate := filepath.Join(dir, base+".md")
		for i := 2; ; i++ {
			if _, err := os.Stat(candidate); errors.Is(err, os.ErrNotExist) {
				break
			}
			candidate = filepath.Join(dir, fmt.Sprintf("%s-%d.md", base, i))
		}
		rel, _ = filepath.Rel(p.root, candidate)
	}
	fm := frontMatter{CodeumentID: doc.ID, Title: doc.Title, Updated: time.Now().UTC().Format(time.RFC3339), Labels: doc.Labels, Meta: doc.Meta, Version: version, Hash: doc.ContentHash()}
	head, err := yaml.Marshal(fm)
	if err != nil {
		return docs.PageRef{}, err
	}
	content := "---\n" + string(head) + "---\n\n# " + doc.Title + "\n\n" + strings.TrimSpace(doc.BodyMD) + "\n"
	perm := os.FileMode(0o644)
	if len(doc.RestrictToGroups) > 0 {
		perm = 0o600
	}
	full := filepath.Join(p.root, rel)
	if err := writeAtomic(full, []byte(content), perm); err != nil {
		return docs.PageRef{}, err
	}
	idx := p.loadIndex()
	idx.Pages[doc.ID] = indexEntry{Path: rel, Title: doc.Title, Version: version}
	if err := p.saveIndex(idx); err != nil {
		return docs.PageRef{}, err
	}
	return docs.PageRef{Provider: "markdown", ProviderID: rel, URL: "file://" + full, Version: version, Title: doc.Title}, nil
}

// List returns all pages (for the TUI's "merge into" picker).
func (p *Provider) List() ([]docs.PageRef, error) {
	idx := p.loadIndex()
	out := make([]docs.PageRef, 0, len(idx.Pages))
	for id, e := range idx.Pages {
		out = append(out, docs.PageRef{Provider: "markdown", ProviderID: e.Path, URL: "file://" + filepath.Join(p.root, e.Path), Version: e.Version, Title: e.Title + " (" + id + ")"})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Title < out[j].Title })
	return out, nil
}

func readPage(path string) (frontMatter, string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return frontMatter{}, "", err
	}
	text := string(data)
	if !strings.HasPrefix(text, "---\n") {
		return frontMatter{}, text, nil
	}
	end := strings.Index(text[4:], "\n---\n")
	if end < 0 {
		return frontMatter{}, text, nil
	}
	var fm frontMatter
	if err := yaml.Unmarshal([]byte(text[4:4+end]), &fm); err != nil {
		return frontMatter{}, "", err
	}
	body := strings.TrimLeft(text[4+end+5:], "\n")
	// Strip the H1 we add on write.
	if strings.HasPrefix(body, "# "+fm.Title+"\n") {
		body = strings.TrimLeft(strings.TrimPrefix(body, "# "+fm.Title+"\n"), "\n")
	}
	return fm, body, nil
}

func writeAtomic(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	name := tmp.Name()
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(name)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(name)
		return err
	}
	return os.Rename(name, path)
}
