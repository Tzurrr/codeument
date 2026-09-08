// Package onepassword stores credentials in 1Password through a Connect
// server on the local network.
package onepassword

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/Tzurrr/codeument/internal/secrets"
)

// Options configure the store.
type Options struct {
	ConnectURL    string
	Token         string
	VaultID       string
	TitleTemplate string
	Timeout       time.Duration
}

// Store talks to one Connect server.
type Store struct {
	opts   Options
	client *http.Client
}

// New validates the options.
func New(o Options) (*Store, error) {
	if o.ConnectURL == "" || o.Token == "" || o.VaultID == "" {
		return nil, errors.New("onepassword: connect_url, token and vault_id are required")
	}
	if o.TitleTemplate == "" {
		o.TitleTemplate = "{hostname} / {username}"
	}
	if o.Timeout <= 0 {
		o.Timeout = secrets.Timeout
	}
	o.ConnectURL = strings.TrimRight(o.ConnectURL, "/")
	return &Store{opts: o, client: &http.Client{Timeout: o.Timeout}}, nil
}

func (s *Store) Name() string { return "onepassword" }

type item struct {
	ID       string      `json:"id,omitempty"`
	Title    string      `json:"title"`
	Category string      `json:"category"`
	Vault    itemVault   `json:"vault"`
	Fields   []itemField `json:"fields,omitempty"`
	Tags     []string    `json:"tags,omitempty"`
	Version  int         `json:"version,omitempty"`
}

type itemVault struct {
	ID string `json:"id"`
}

type itemField struct {
	ID      string `json:"id,omitempty"`
	Type    string `json:"type"`
	Purpose string `json:"purpose,omitempty"`
	Label   string `json:"label,omitempty"`
	Value   string `json:"value"`
}

// Put creates or updates a LOGIN item for the credential.
func (s *Store) Put(ctx context.Context, c secrets.Credential) (secrets.Reference, error) {
	title := secrets.Expand(s.opts.TitleTemplate, c.Host, c.Username)
	body := item{
		Title: title, Category: "LOGIN", Vault: itemVault{ID: s.opts.VaultID},
		Tags: []string{"codeument", c.Host},
		Fields: []itemField{
			{ID: "username", Type: "STRING", Purpose: "USERNAME", Value: c.Username},
			{ID: "password", Type: "CONCEALED", Purpose: "PASSWORD", Value: c.Password},
			{Type: "STRING", Label: "host", Value: c.Host},
			{Type: "STRING", Label: "kind", Value: c.Kind},
			{Type: "STRING", Label: "managed_by", Value: "codeument"},
		},
	}
	if c.Notes != "" {
		body.Fields = append(body.Fields, itemField{Type: "STRING", Purpose: "NOTES", Label: "notesPlain", Value: c.Notes})
	}
	existing, err := s.find(ctx, title)
	if err != nil {
		return secrets.Reference{}, err
	}
	var out item
	if existing != nil {
		body.ID, body.Version = existing.ID, existing.Version
		err = s.do(ctx, http.MethodPut, "/v1/vaults/"+s.opts.VaultID+"/items/"+existing.ID, body, &out)
	} else {
		err = s.do(ctx, http.MethodPost, "/v1/vaults/"+s.opts.VaultID+"/items", body, &out)
	}
	if err != nil {
		return secrets.Reference{}, err
	}
	id := out.ID
	if id == "" && existing != nil {
		id = existing.ID
	}
	return secrets.Reference{
		Ref: fmt.Sprintf("op://%s/%s", s.opts.VaultID, title),
		URL: s.opts.ConnectURL + "/v1/vaults/" + s.opts.VaultID + "/items/" + id,
	}, nil
}

func (s *Store) find(ctx context.Context, title string) (*item, error) {
	var items []item
	q := url.Values{"filter": {fmt.Sprintf(`title eq "%s"`, strings.ReplaceAll(title, `"`, `\"`))}}
	if err := s.do(ctx, http.MethodGet, "/v1/vaults/"+s.opts.VaultID+"/items?"+q.Encode(), nil, &items); err != nil {
		return nil, err
	}
	for i := range items {
		if items[i].Title == title {
			return &items[i], nil
		}
	}
	return nil, nil
}

// Ping checks the Connect server and the vault.
func (s *Store) Ping(ctx context.Context) error {
	var v struct {
		ID string `json:"id"`
	}
	if err := s.do(ctx, http.MethodGet, "/v1/vaults/"+s.opts.VaultID, nil, &v); err != nil {
		return err
	}
	if v.ID == "" {
		return fmt.Errorf("onepassword: vault %q not found", s.opts.VaultID)
	}
	return nil
}

func (s *Store) do(ctx context.Context, method, path string, in, out any) error {
	var rdr io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.opts.ConnectURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.opts.Token)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("onepassword: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("onepassword: Connect rejected the token (%d): %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("onepassword: HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
