// Package bitwarden stores credentials in Bitwarden Secrets Manager (and
// Vaultwarden deployments that expose the same API).
package bitwarden

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/Tzurrr/codeument/internal/secrets"
)

// Options configure the store.
type Options struct {
	ServerURL      string
	AccessToken    string
	OrganizationID string
	ProjectID      string
	Timeout        time.Duration
}

// Store talks to one Secrets Manager API.
type Store struct {
	opts   Options
	client *http.Client
}

// New validates the options.
func New(o Options) (*Store, error) {
	if o.ServerURL == "" || o.AccessToken == "" || o.OrganizationID == "" {
		return nil, errors.New("bitwarden: server_url, access_token and organization_id are required")
	}
	if o.Timeout <= 0 {
		o.Timeout = secrets.Timeout
	}
	o.ServerURL = strings.TrimRight(o.ServerURL, "/")
	return &Store{opts: o, client: &http.Client{Timeout: o.Timeout}}, nil
}

func (s *Store) Name() string { return "bitwarden" }

// Key is the secret key for a credential.
func (s *Store) Key(c secrets.Credential) string { return c.Host + "/" + c.Username }

type secretPayload struct {
	Key            string   `json:"key"`
	Value          string   `json:"value"`
	Note           string   `json:"note"`
	ProjectIDs     []string `json:"projectIds,omitempty"`
	OrganizationID string   `json:"organizationId,omitempty"`
}

type secretResponse struct {
	ID  string `json:"id"`
	Key string `json:"key"`
}

// Put creates or updates the secret for a credential.
func (s *Store) Put(ctx context.Context, c secrets.Credential) (secrets.Reference, error) {
	key := s.Key(c)
	note := fmt.Sprintf("codeument: %s on %s", c.Username, c.Host)
	if c.Notes != "" {
		note += " — " + c.Notes
	}
	payload := secretPayload{Key: key, Value: c.Password, Note: note, OrganizationID: s.opts.OrganizationID}
	if s.opts.ProjectID != "" {
		payload.ProjectIDs = []string{s.opts.ProjectID}
	}
	existing, err := s.find(ctx, key)
	if err != nil {
		return secrets.Reference{}, err
	}
	var out secretResponse
	if existing != "" {
		err = s.do(ctx, http.MethodPut, "/api/secrets/"+existing, payload, &out)
	} else {
		err = s.do(ctx, http.MethodPost, "/api/organizations/"+s.opts.OrganizationID+"/secrets", payload, &out)
	}
	if err != nil {
		return secrets.Reference{}, err
	}
	id := out.ID
	if id == "" {
		id = existing
	}
	return secrets.Reference{
		Ref: "bitwarden://" + key,
		URL: s.opts.ServerURL + "/#/sm/" + s.opts.OrganizationID + "/secrets/" + id,
	}, nil
}

func (s *Store) find(ctx context.Context, key string) (string, error) {
	var list struct {
		Data []secretResponse `json:"data"`
	}
	if err := s.do(ctx, http.MethodGet, "/api/organizations/"+s.opts.OrganizationID+"/secrets", nil, &list); err != nil {
		return "", err
	}
	for _, sec := range list.Data {
		if sec.Key == key {
			return sec.ID, nil
		}
	}
	return "", nil
}

// Ping checks the server and the organization.
func (s *Store) Ping(ctx context.Context) error {
	var list struct {
		Data []secretResponse `json:"data"`
	}
	return s.do(ctx, http.MethodGet, "/api/organizations/"+s.opts.OrganizationID+"/secrets", nil, &list)
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
	req, err := http.NewRequestWithContext(ctx, method, s.opts.ServerURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+s.opts.AccessToken)
	req.Header.Set("Accept", "application/json")
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("bitwarden: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		msg := strings.TrimSpace(string(data))
		switch resp.StatusCode {
		case 401, 403:
			return fmt.Errorf("bitwarden: access token rejected (%d): %s", resp.StatusCode, msg)
		case 404:
			return fmt.Errorf("bitwarden: not found (%d) — this server may not expose Secrets Manager: %s", resp.StatusCode, msg)
		}
		return fmt.Errorf("bitwarden: HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
