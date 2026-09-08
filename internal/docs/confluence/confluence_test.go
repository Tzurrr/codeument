package confluence

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/model"
)

// stub is a tiny in-memory Confluence implementing the routes the provider
// uses.
type stub struct {
	mu       sync.Mutex
	pages    map[string]*stubPage
	props    map[string]map[string]stubProp // pageID -> key -> prop
	labels   map[string][]string
	restrict map[string][]string
	next     int
	calls    []string
	fail429  int
}

type stubPage struct {
	ID, Title, SpaceID, ParentID, Body string
	Version                            int
}

type stubProp struct {
	ID      string
	Value   json.RawMessage
	Version int
}

func newStub() *stub {
	s := &stub{pages: map[string]*stubPage{}, props: map[string]map[string]stubProp{}, labels: map[string][]string{}, restrict: map[string][]string{}, next: 100}
	s.pages["10"] = &stubPage{ID: "10", Title: "OPS Home", SpaceID: "1", Version: 1}
	return s
}

func (s *stub) id() string {
	s.next++
	return strconv.Itoa(s.next)
}

func (s *stub) handler() http.Handler {
	mux := http.NewServeMux()
	auth := func(next http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			u, p, ok := r.BasicAuth()
			if !ok || u != "bot@acme" || p != "tok" {
				w.WriteHeader(401)
				return
			}
			s.mu.Lock()
			s.calls = append(s.calls, r.Method+" "+r.URL.Path)
			if s.fail429 > 0 {
				s.fail429--
				s.mu.Unlock()
				w.Header().Set("Retry-After", "0")
				w.WriteHeader(429)
				return
			}
			s.mu.Unlock()
			next(w, r)
		}
	}
	mux.HandleFunc("GET /wiki/api/v2/spaces", auth(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("keys") != "OPS" {
			writeJSON(w, map[string]any{"results": []any{}})
			return
		}
		writeJSON(w, map[string]any{"results": []map[string]any{{"id": "1", "key": "OPS", "homepageId": "10"}}})
	}))
	mux.HandleFunc("GET /wiki/api/v2/pages", auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []map[string]any
		for _, p := range s.pages {
			if p.Title == r.URL.Query().Get("title") {
				out = append(out, s.pageJSON(p, false))
			}
		}
		writeJSON(w, map[string]any{"results": out})
	}))
	mux.HandleFunc("POST /wiki/api/v2/pages", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			SpaceID, Title, ParentID string
			Body                     struct{ Value string }
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, p := range s.pages {
			if p.Title == in.Title {
				w.WriteHeader(400)
				_, _ = w.Write([]byte(`{"message":"A page with this title already exists"}`))
				return
			}
		}
		p := &stubPage{ID: s.id(), Title: in.Title, SpaceID: in.SpaceID, ParentID: in.ParentID, Body: in.Body.Value, Version: 1}
		s.pages[p.ID] = p
		writeJSON(w, s.pageJSON(p, true))
	}))
	mux.HandleFunc("GET /wiki/api/v2/pages/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		p, ok := s.pages[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		writeJSON(w, s.pageJSON(p, r.URL.Query().Get("body-format") == "storage"))
	}))
	mux.HandleFunc("PUT /wiki/api/v2/pages/{id}", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Title   string
			Body    struct{ Value string }
			Version struct{ Number int }
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		p, ok := s.pages[r.PathValue("id")]
		if !ok {
			w.WriteHeader(404)
			return
		}
		if in.Version.Number != p.Version+1 {
			w.WriteHeader(409)
			_, _ = w.Write([]byte(`{"message":"version conflict"}`))
			return
		}
		p.Title, p.Body, p.Version = in.Title, in.Body.Value, in.Version.Number
		writeJSON(w, s.pageJSON(p, true))
	}))
	mux.HandleFunc("GET /wiki/api/v2/pages/{id}/properties", auth(func(w http.ResponseWriter, r *http.Request) {
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []map[string]any
		for k, pr := range s.props[r.PathValue("id")] {
			out = append(out, map[string]any{"id": pr.ID, "key": k, "value": pr.Value, "version": map[string]int{"number": pr.Version}})
		}
		writeJSON(w, map[string]any{"results": out})
	}))
	mux.HandleFunc("POST /wiki/api/v2/pages/{id}/properties", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key   string
			Value json.RawMessage
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		id := r.PathValue("id")
		if s.props[id] == nil {
			s.props[id] = map[string]stubProp{}
		}
		s.props[id][in.Key] = stubProp{ID: s.id(), Value: in.Value, Version: 1}
		writeJSON(w, map[string]any{"id": s.props[id][in.Key].ID})
	}))
	mux.HandleFunc("PUT /wiki/api/v2/pages/{id}/properties/{pid}", auth(func(w http.ResponseWriter, r *http.Request) {
		var in struct {
			Key     string
			Value   json.RawMessage
			Version struct{ Number int }
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		id := r.PathValue("id")
		pr := s.props[id][in.Key]
		if in.Version.Number != pr.Version+1 {
			w.WriteHeader(409)
			return
		}
		pr.Value, pr.Version = in.Value, in.Version.Number
		s.props[id][in.Key] = pr
		writeJSON(w, map[string]any{"id": pr.ID})
	}))
	mux.HandleFunc("POST /wiki/rest/api/content/{id}/label", auth(func(w http.ResponseWriter, r *http.Request) {
		var in []struct{ Name string }
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, l := range in {
			s.labels[r.PathValue("id")] = append(s.labels[r.PathValue("id")], l.Name)
		}
		writeJSON(w, map[string]any{"results": []any{}})
	}))
	mux.HandleFunc("PUT /wiki/rest/api/content/{id}/restriction", auth(func(w http.ResponseWriter, r *http.Request) {
		var in []struct {
			Operation    string
			Restrictions struct {
				Group []struct{ Name string }
			}
		}
		_ = json.NewDecoder(r.Body).Decode(&in)
		s.mu.Lock()
		defer s.mu.Unlock()
		for _, op := range in {
			for _, g := range op.Restrictions.Group {
				s.restrict[r.PathValue("id")] = append(s.restrict[r.PathValue("id")], g.Name)
			}
		}
		writeJSON(w, map[string]any{})
	}))
	mux.HandleFunc("GET /wiki/rest/api/content/search", auth(func(w http.ResponseWriter, r *http.Request) {
		cql := r.URL.Query().Get("cql")
		s.mu.Lock()
		defer s.mu.Unlock()
		var out []map[string]any
		for id, props := range s.props {
			for _, pr := range props {
				var v struct {
					DocID string `json:"doc_id"`
				}
				_ = json.Unmarshal(pr.Value, &v)
				if v.DocID != "" && strings.Contains(cql, v.DocID) {
					out = append(out, map[string]any{"id": id})
				}
			}
		}
		writeJSON(w, map[string]any{"results": out})
	}))
	return mux
}

func (s *stub) pageJSON(p *stubPage, body bool) map[string]any {
	m := map[string]any{"id": p.ID, "title": p.Title, "spaceId": p.SpaceID, "parentId": p.ParentID, "status": "current",
		"version": map[string]int{"number": p.Version}, "_links": map[string]string{"webui": "/spaces/OPS/pages/" + p.ID, "base": "https://acme.atlassian.net/wiki"}}
	if body {
		m["body"] = map[string]any{"storage": map[string]string{"value": p.Body, "representation": "storage"}}
	}
	return m
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func newProvider(t *testing.T, s *stub) (*Provider, *httptest.Server) {
	t.Helper()
	ts := httptest.NewServer(s.handler())
	t.Cleanup(ts.Close)
	p, err := New(Options{BaseURL: ts.URL + "/wiki", Email: "bot@acme", APIToken: "tok", Space: "OPS"})
	if err != nil {
		t.Fatal(err)
	}
	return p, ts
}

func TestUpsertCreateUpdateAndFind(t *testing.T) {
	s := newStub()
	p, _ := newProvider(t, s)
	ctx := context.Background()
	if err := p.Ping(ctx); err != nil {
		t.Fatal(err)
	}
	doc := docs.Document{ID: "doc-1", Title: "Rotate certs", BodyMD: "Did it.\n\n```bash\ncertbot renew\n```\n", Location: model.Location{Space: "OPS", ParentPath: []string{"Runbooks", "Web"}}, Labels: []string{"TLS stuff"}, Meta: map[string]string{"hostname": "web-01", "kind": "draft"}}
	ref, err := p.Upsert(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	if ref.Provider != "confluence" || ref.Version != 1 || !strings.Contains(ref.URL, "/spaces/OPS/pages/") {
		t.Fatalf("ref = %+v", ref)
	}
	// Hierarchy: Runbooks under home, Web under Runbooks, page under Web.
	var runbooks, web *stubPage
	for _, pg := range s.pages {
		switch pg.Title {
		case "Runbooks":
			runbooks = pg
		case "Web":
			web = pg
		}
	}
	if runbooks == nil || web == nil || runbooks.ParentID != "10" || web.ParentID != runbooks.ID {
		t.Fatalf("hierarchy wrong: runbooks=%+v web=%+v", runbooks, web)
	}
	page := s.pages[ref.ProviderID]
	if page.ParentID != web.ID || !strings.Contains(page.Body, `ac:name="code"`) || !strings.Contains(page.Body, "doc-1") {
		t.Fatalf("page = %+v", page)
	}
	if got := s.labels[ref.ProviderID]; len(got) != 2 || got[0] != "codeument" || got[1] != "tls-stuff" {
		t.Fatalf("labels = %v", got)
	}

	// Update in place: version bumps, path unchanged, property updated.
	doc.BodyMD = "Did it again.\n"
	ref2, err := p.Upsert(ctx, doc)
	if err != nil || ref2.ProviderID != ref.ProviderID || ref2.Version != 2 {
		t.Fatalf("update: %v %+v", err, ref2)
	}
	got, err := p.Get(ctx, ref2)
	if err != nil || got.ID != "doc-1" || got.BodyMD != "Did it again.\n" || got.Meta["hostname"] != "web-01" {
		t.Fatalf("get: %v %+v", err, got)
	}

	// A fresh provider (no cache) finds the page by property search.
	p2, _ := newProviderSharing(t, s, p)
	found, err := p2.Find(ctx, "doc-1")
	if err != nil || found == nil || found.ProviderID != ref.ProviderID {
		t.Fatalf("find: %v %+v", err, found)
	}
	if none, err := p2.Find(ctx, "nope"); err != nil || none != nil {
		t.Fatalf("find missing: %v %+v", err, none)
	}

	// Title collision with a different doc id gets a suffix.
	ref3, err := p.Upsert(ctx, docs.Document{ID: "doc-2", Title: "Rotate certs", BodyMD: "x", Location: doc.Location})
	if err != nil || s.pages[ref3.ProviderID].Title != "Rotate certs (2)" {
		t.Fatalf("collision: %v %+v", err, ref3)
	}

	// Inline-credential pages get read restrictions.
	_, err = p.Upsert(ctx, docs.Document{ID: "srv-1", Title: "Server: web-01", BodyMD: "x", Location: doc.Location, RestrictToGroups: []string{"ops-admins"}})
	if err != nil {
		t.Fatal(err)
	}
	restricted := false
	for _, g := range s.restrict {
		if len(g) == 1 && g[0] == "ops-admins" {
			restricted = true
		}
	}
	if !restricted {
		t.Fatalf("restrictions = %v", s.restrict)
	}
}

func newProviderSharing(t *testing.T, s *stub, old *Provider) (*Provider, *httptest.Server) {
	t.Helper()
	p, err := New(Options{BaseURL: strings.TrimSuffix(old.base, "/wiki") + "/wiki", Email: "bot@acme", APIToken: "tok", Space: "OPS"})
	if err != nil {
		t.Fatal(err)
	}
	p.client = old.client
	return p, nil
}

func TestRetryOn429AndConflict(t *testing.T) {
	s := newStub()
	p, _ := newProvider(t, s)
	ctx := context.Background()
	s.fail429 = 2
	ref, err := p.Upsert(ctx, docs.Document{ID: "d", Title: "T", BodyMD: "b", Location: model.Location{Space: "OPS"}})
	if err != nil {
		t.Fatalf("expected retries to succeed: %v", err)
	}
	// Simulate someone else bumping the version between our read and write.
	s.mu.Lock()
	s.pages[ref.ProviderID].Version = 5
	s.mu.Unlock()
	ref2, err := p.Upsert(ctx, docs.Document{ID: "d", Title: "T", BodyMD: "c", Location: model.Location{Space: "OPS"}})
	if err != nil || ref2.Version != 6 {
		t.Fatalf("conflict retry: %v %+v", err, ref2)
	}
	bad, _ := New(Options{BaseURL: p.base, Email: "bot@acme", APIToken: "wrong", Space: "OPS"})
	bad.client = p.client
	if err := bad.Ping(ctx); err == nil || !strings.Contains(err.Error(), "authentication") {
		t.Fatalf("expected auth error, got %v", err)
	}
	if fmt.Sprint(status(&apiError{Status: 409})) != "409" {
		t.Fatal("status helper")
	}
}
