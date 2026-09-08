// Package confluence is a docs.Provider for Confluence Cloud (REST API v2
// with a few v1 calls for labels, search and restrictions). Pages are keyed
// by the codeument document id stored in a content property; the original
// Markdown is kept in the same property so pages round-trip exactly.
package confluence

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/docs/render"
	"github.com/Tzurrr/codeument/internal/model"
)

func init() {
	docs.Register("confluence", func(cfg docs.Config) (docs.Provider, error) {
		return New(Options{BaseURL: cfg.Confluence.BaseURL, Email: cfg.Confluence.Email, APIToken: cfg.Confluence.APIToken, Space: cfg.Confluence.Space})
	})
}

// PropertyKey is the content property that carries codeument metadata.
const PropertyKey = "codeument"

// Label marks pages managed by codeument.
const Label = "codeument"

// Options configure the provider.
type Options struct {
	BaseURL  string // https://acme.atlassian.net/wiki
	Email    string
	APIToken string
	Space    string // default space key
	Timeout  time.Duration
}

// Provider talks to one Confluence site.
type Provider struct {
	base   string
	email  string
	token  string
	space  string
	client *http.Client

	mu       sync.Mutex
	spaceIDs map[string]spaceInfo // key -> id/homepage
	pageIDs  map[string]string    // doc id -> page id
}

type spaceInfo struct {
	ID       string
	Homepage string
}

// New validates the options and builds a provider.
func New(o Options) (*Provider, error) {
	if o.BaseURL == "" || o.Email == "" || o.APIToken == "" {
		return nil, errors.New("confluence: base_url, email and api_token are required")
	}
	if o.Timeout <= 0 {
		o.Timeout = 60 * time.Second
	}
	base := strings.TrimRight(o.BaseURL, "/")
	if !strings.HasSuffix(base, "/wiki") && strings.Contains(base, "atlassian.net") {
		base += "/wiki"
	}
	return &Provider{base: base, email: o.Email, token: o.APIToken, space: o.Space, client: &http.Client{Timeout: o.Timeout}, spaceIDs: map[string]spaceInfo{}, pageIDs: map[string]string{}}, nil
}

func (p *Provider) Name() string { return "confluence" }

// property is what we store on each page.
type property struct {
	DocID    string `json:"doc_id"`
	Hash     string `json:"hash"`
	Version  int    `json:"version"`
	Markdown string `json:"markdown"`
	Title    string `json:"title"`
	Hostname string `json:"hostname,omitempty"`
	Kind     string `json:"kind,omitempty"`
}

// --- HTTP -------------------------------------------------------------------

type apiError struct {
	Status int
	Body   string
}

func (e *apiError) Error() string {
	return fmt.Sprintf("confluence: HTTP %d: %s", e.Status, truncate(e.Body, 300))
}

func (p *Provider) do(ctx context.Context, method, path string, query url.Values, in, out any) error {
	u := p.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var body []byte
	if in != nil {
		var err error
		if body, err = json.Marshal(in); err != nil {
			return err
		}
	}
	var lastErr error
	for attempt := 0; attempt < 4; attempt++ {
		var rdr io.Reader
		if body != nil {
			rdr = bytes.NewReader(body)
		}
		req, err := http.NewRequestWithContext(ctx, method, u, rdr)
		if err != nil {
			return err
		}
		req.SetBasicAuth(p.email, p.token)
		req.Header.Set("Accept", "application/json")
		if body != nil {
			req.Header.Set("Content-Type", "application/json")
		}
		resp, err := p.client.Do(req)
		if err != nil {
			return fmt.Errorf("confluence: %w", err)
		}
		data, rerr := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
		_ = resp.Body.Close()
		if rerr != nil {
			return rerr
		}
		switch {
		case resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500:
			lastErr = &apiError{Status: resp.StatusCode, Body: string(data)}
			wait := time.Duration(attempt+1) * 2 * time.Second
			if ra := resp.Header.Get("Retry-After"); ra != "" {
				if secs, err := strconv.Atoi(ra); err == nil && secs >= 0 && secs <= 120 {
					wait = time.Duration(secs)*time.Second + 100*time.Millisecond
				}
			}
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
			continue
		case resp.StatusCode/100 != 2:
			return &apiError{Status: resp.StatusCode, Body: string(data)}
		}
		if out != nil && len(data) > 0 {
			if err := json.Unmarshal(data, out); err != nil {
				return fmt.Errorf("confluence: decode %s: %w", path, err)
			}
		}
		return nil
	}
	return lastErr
}

func status(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Status
	}
	return 0
}

// --- spaces and hierarchy ---------------------------------------------------

func (p *Provider) spaceFor(ctx context.Context, key string) (spaceInfo, error) {
	if key == "" {
		key = p.space
	}
	if key == "" {
		return spaceInfo{}, errors.New("confluence: no space key configured")
	}
	p.mu.Lock()
	if s, ok := p.spaceIDs[key]; ok {
		p.mu.Unlock()
		return s, nil
	}
	p.mu.Unlock()
	var res struct {
		Results []struct {
			ID         string `json:"id"`
			Key        string `json:"key"`
			HomepageID string `json:"homepageId"`
		} `json:"results"`
	}
	if err := p.do(ctx, http.MethodGet, "/api/v2/spaces", url.Values{"keys": {key}}, nil, &res); err != nil {
		return spaceInfo{}, err
	}
	for _, s := range res.Results {
		if strings.EqualFold(s.Key, key) {
			info := spaceInfo{ID: s.ID, Homepage: s.HomepageID}
			p.mu.Lock()
			p.spaceIDs[key] = info
			p.mu.Unlock()
			return info, nil
		}
	}
	return spaceInfo{}, fmt.Errorf("confluence: space %q not found (or no permission)", key)
}

type page struct {
	ID       string `json:"id"`
	Title    string `json:"title"`
	SpaceID  string `json:"spaceId"`
	ParentID string `json:"parentId"`
	Status   string `json:"status"`
	Version  struct {
		Number int `json:"number"`
	} `json:"version"`
	Body struct {
		Storage struct {
			Value string `json:"value"`
		} `json:"storage"`
	} `json:"body"`
	Links struct {
		WebUI string `json:"webui"`
		Base  string `json:"base"`
	} `json:"_links"`
}

func (p *Provider) pagesByTitle(ctx context.Context, spaceID, title string) ([]page, error) {
	var res struct {
		Results []page `json:"results"`
	}
	if err := p.do(ctx, http.MethodGet, "/api/v2/pages", url.Values{"space-id": {spaceID}, "title": {title}, "status": {"current"}, "limit": {"50"}}, nil, &res); err != nil {
		return nil, err
	}
	return res.Results, nil
}

func (p *Provider) getPage(ctx context.Context, id string, withBody bool) (*page, error) {
	q := url.Values{}
	if withBody {
		q.Set("body-format", "storage")
	}
	var pg page
	if err := p.do(ctx, http.MethodGet, "/api/v2/pages/"+id, q, nil, &pg); err != nil {
		if status(err) == 404 {
			return nil, docs.ErrNotFound
		}
		return nil, err
	}
	return &pg, nil
}

// EnsureHierarchy creates the parent pages named in loc and returns the id
// of the innermost one (the space homepage when the path is empty).
func (p *Provider) EnsureHierarchy(ctx context.Context, loc model.Location) (string, error) {
	sp, err := p.spaceFor(ctx, loc.Space)
	if err != nil {
		return "", err
	}
	parent := sp.Homepage
	for _, title := range loc.ParentPath {
		title = strings.TrimSpace(title)
		if title == "" {
			continue
		}
		found := ""
		pages, err := p.pagesByTitle(ctx, sp.ID, title)
		if err != nil {
			return "", err
		}
		for _, pg := range pages {
			if pg.ParentID == parent || parent == "" {
				found = pg.ID
				break
			}
		}
		if found == "" && len(pages) == 1 {
			found = pages[0].ID // title is unique in the space; accept it wherever it lives
		}
		if found == "" {
			created, err := p.createPage(ctx, sp.ID, parent, title, "<p>Section created by codeument.</p>")
			if err != nil {
				return "", err
			}
			found = created.ID
		}
		parent = found
	}
	return parent, nil
}

func (p *Provider) createPage(ctx context.Context, spaceID, parentID, title, storage string) (*page, error) {
	body := map[string]any{
		"spaceId": spaceID, "status": "current", "title": title,
		"body": map[string]string{"representation": "storage", "value": storage},
	}
	if parentID != "" {
		body["parentId"] = parentID
	}
	var pg page
	if err := p.do(ctx, http.MethodPost, "/api/v2/pages", nil, body, &pg); err != nil {
		return nil, err
	}
	return &pg, nil
}

func (p *Provider) updatePage(ctx context.Context, id, title, storage string, version int, message string) (*page, error) {
	body := map[string]any{
		"id": id, "status": "current", "title": title,
		"body":    map[string]string{"representation": "storage", "value": storage},
		"version": map[string]any{"number": version, "message": message},
	}
	var pg page
	if err := p.do(ctx, http.MethodPut, "/api/v2/pages/"+id, nil, body, &pg); err != nil {
		return nil, err
	}
	return &pg, nil
}

// --- properties, labels, restrictions --------------------------------------

type propertyRow struct {
	ID      string          `json:"id"`
	Key     string          `json:"key"`
	Value   json.RawMessage `json:"value"`
	Version struct {
		Number int `json:"number"`
	} `json:"version"`
}

func (p *Provider) getProperty(ctx context.Context, pageID string) (*propertyRow, *property, error) {
	var res struct {
		Results []propertyRow `json:"results"`
	}
	if err := p.do(ctx, http.MethodGet, "/api/v2/pages/"+pageID+"/properties", url.Values{"key": {PropertyKey}}, nil, &res); err != nil {
		return nil, nil, err
	}
	for i := range res.Results {
		if res.Results[i].Key == PropertyKey {
			var prop property
			_ = json.Unmarshal(res.Results[i].Value, &prop)
			return &res.Results[i], &prop, nil
		}
	}
	return nil, nil, nil
}

func (p *Provider) setProperty(ctx context.Context, pageID string, prop property) error {
	row, _, err := p.getProperty(ctx, pageID)
	if err != nil {
		return err
	}
	if row == nil {
		return p.do(ctx, http.MethodPost, "/api/v2/pages/"+pageID+"/properties", nil, map[string]any{"key": PropertyKey, "value": prop}, nil)
	}
	return p.do(ctx, http.MethodPut, "/api/v2/pages/"+pageID+"/properties/"+row.ID, nil, map[string]any{"key": PropertyKey, "value": prop, "version": map[string]any{"number": row.Version.Number + 1}}, nil)
}

func (p *Provider) addLabels(ctx context.Context, pageID string, labels []string) error {
	if len(labels) == 0 {
		return nil
	}
	body := make([]map[string]string, 0, len(labels))
	for _, l := range labels {
		l = sanitizeLabel(l)
		if l != "" {
			body = append(body, map[string]string{"prefix": "global", "name": l})
		}
	}
	if len(body) == 0 {
		return nil
	}
	return p.do(ctx, http.MethodPost, "/rest/api/content/"+pageID+"/label", nil, body, nil)
}

func sanitizeLabel(l string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(l)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_':
			b.WriteRune(r)
		case r == ' ' || r == '/' || r == '.':
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

func (p *Provider) restrictRead(ctx context.Context, pageID string, groups []string) error {
	if len(groups) == 0 {
		return nil
	}
	gs := make([]map[string]any, 0, len(groups))
	for _, g := range groups {
		gs = append(gs, map[string]any{"type": "group", "name": g})
	}
	body := []map[string]any{{"operation": "read", "restrictions": map[string]any{"group": gs}}}
	return p.do(ctx, http.MethodPut, "/rest/api/content/"+pageID+"/restriction", nil, body, nil)
}

// --- docs.Provider ----------------------------------------------------------

// Find locates the page for a doc id: cached page id first, then a CQL
// search on the content property, then a text search for the visible id.
func (p *Provider) Find(ctx context.Context, docID string) (*docs.PageRef, error) {
	p.mu.Lock()
	id, ok := p.pageIDs[docID]
	p.mu.Unlock()
	if ok {
		if pg, err := p.getPage(ctx, id, false); err == nil {
			if _, prop, perr := p.getProperty(ctx, id); perr == nil && prop != nil && prop.DocID == docID {
				return p.ref(pg), nil
			}
		}
		p.mu.Lock()
		delete(p.pageIDs, docID)
		p.mu.Unlock()
	}
	cqls := []string{
		fmt.Sprintf(`type=page and content.property[%s].doc_id="%s"`, PropertyKey, cqlEscape(docID)),
		fmt.Sprintf(`type=page and label="%s" and text ~ "%s"`, Label, cqlEscape(docID)),
	}
	for _, cql := range cqls {
		if p.space != "" {
			cql = fmt.Sprintf(`space="%s" and `, cqlEscape(p.space)) + cql
		}
		var res struct {
			Results []struct {
				ID string `json:"id"`
			} `json:"results"`
		}
		if err := p.do(ctx, http.MethodGet, "/rest/api/content/search", url.Values{"cql": {cql}, "limit": {"5"}}, nil, &res); err != nil {
			continue
		}
		for _, r := range res.Results {
			_, prop, err := p.getProperty(ctx, r.ID)
			if err != nil || prop == nil || prop.DocID != docID {
				continue
			}
			pg, err := p.getPage(ctx, r.ID, false)
			if err != nil {
				continue
			}
			p.mu.Lock()
			p.pageIDs[docID] = pg.ID
			p.mu.Unlock()
			return p.ref(pg), nil
		}
	}
	return nil, nil
}

func cqlEscape(s string) string { return strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) }

func (p *Provider) ref(pg *page) *docs.PageRef {
	return &docs.PageRef{Provider: "confluence", ProviderID: pg.ID, URL: p.pageURL(pg), Version: pg.Version.Number, Title: pg.Title}
}

func (p *Provider) pageURL(pg *page) string {
	if pg.Links.WebUI != "" {
		base := pg.Links.Base
		if base == "" {
			base = p.base
		}
		return strings.TrimRight(base, "/") + pg.Links.WebUI
	}
	return p.base + "/pages/" + pg.ID
}

// Get returns the page content. The Markdown source comes from the content
// property; when it is missing (page created outside codeument) the storage
// XHTML is returned as the body.
func (p *Provider) Get(ctx context.Context, ref docs.PageRef) (*docs.Document, error) {
	pg, err := p.getPage(ctx, ref.ProviderID, true)
	if err != nil {
		return nil, err
	}
	doc := &docs.Document{Title: pg.Title, Meta: map[string]string{"page_id": pg.ID}}
	_, prop, err := p.getProperty(ctx, pg.ID)
	if err == nil && prop != nil {
		doc.ID = prop.DocID
		doc.BodyMD = prop.Markdown
		doc.Meta["hash"] = prop.Hash
		if prop.Hostname != "" {
			doc.Meta["hostname"] = prop.Hostname
		}
		if prop.Kind != "" {
			doc.Meta["kind"] = prop.Kind
		}
	}
	if doc.BodyMD == "" {
		doc.BodyMD = pg.Body.Storage.Value
		doc.Meta["format"] = "storage"
	}
	return doc, nil
}

// Upsert creates or updates the page for doc.ID.
func (p *Provider) Upsert(ctx context.Context, doc docs.Document) (docs.PageRef, error) {
	if doc.ID == "" {
		return docs.PageRef{}, errors.New("confluence: document has no id")
	}
	storage, err := p.render(doc)
	if err != nil {
		return docs.PageRef{}, err
	}
	prop := property{DocID: doc.ID, Hash: doc.ContentHash(), Markdown: doc.BodyMD, Title: doc.Title, Hostname: doc.Meta["hostname"], Kind: doc.Meta["kind"]}
	existing, err := p.Find(ctx, doc.ID)
	if err != nil {
		return docs.PageRef{}, err
	}
	var pg *page
	if existing != nil {
		cur, err := p.getPage(ctx, existing.ProviderID, false)
		if err != nil {
			return docs.PageRef{}, err
		}
		title := doc.Title
		if title == "" {
			title = cur.Title
		}
		pg, err = p.updatePage(ctx, cur.ID, title, storage, cur.Version.Number+1, "codeument update")
		if status(err) == 409 {
			cur, gerr := p.getPage(ctx, existing.ProviderID, false)
			if gerr != nil {
				return docs.PageRef{}, gerr
			}
			pg, err = p.updatePage(ctx, cur.ID, title, storage, cur.Version.Number+1, "codeument update")
		}
		if err != nil {
			return docs.PageRef{}, err
		}
	} else {
		sp, err := p.spaceFor(ctx, doc.Location.Space)
		if err != nil {
			return docs.PageRef{}, err
		}
		parent, err := p.EnsureHierarchy(ctx, doc.Location)
		if err != nil {
			return docs.PageRef{}, err
		}
		title := doc.Title
		for attempt := 0; ; attempt++ {
			pg, err = p.createPage(ctx, sp.ID, parent, title, storage)
			if err == nil {
				break
			}
			if status(err) == 400 && strings.Contains(strings.ToLower(err.Error()), "title") && attempt < 3 {
				title = fmt.Sprintf("%s (%d)", doc.Title, attempt+2)
				continue
			}
			return docs.PageRef{}, err
		}
	}
	prop.Version = pg.Version.Number
	if err := p.setProperty(ctx, pg.ID, prop); err != nil {
		return docs.PageRef{}, fmt.Errorf("set content property: %w", err)
	}
	if err := p.addLabels(ctx, pg.ID, append([]string{Label}, doc.Labels...)); err != nil {
		// Labels are cosmetic; do not fail the publish.
		_ = err
	}
	if len(doc.RestrictToGroups) > 0 {
		if err := p.restrictRead(ctx, pg.ID, doc.RestrictToGroups); err != nil {
			return docs.PageRef{}, fmt.Errorf("restrict page: %w", err)
		}
	}
	p.mu.Lock()
	p.pageIDs[doc.ID] = pg.ID
	p.mu.Unlock()
	return *p.ref(pg), nil
}

func (p *Provider) render(doc docs.Document) (string, error) {
	body := doc.BodyMD
	if doc.Meta["format"] == "storage" {
		return body + render.Footer(doc.ID, doc.Meta["hostname"]), nil
	}
	xhtml, err := render.ConfluenceStorage(body)
	if err != nil {
		return "", err
	}
	return xhtml + render.Footer(doc.ID, doc.Meta["hostname"]), nil
}

// Ping checks credentials and the default space.
func (p *Provider) Ping(ctx context.Context) error {
	_, err := p.spaceFor(ctx, p.space)
	if status(err) == 401 || status(err) == 403 {
		return errors.New("confluence: authentication failed (check email and api token)")
	}
	return err
}

// Remember seeds the page id cache (for example from the local doc_pages
// table) so Find can skip the search.
func (p *Provider) Remember(docID, pageID string) {
	p.mu.Lock()
	p.pageIDs[docID] = pageID
	p.mu.Unlock()
}

func truncate(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
