// Package vault stores credentials in HashiCorp Vault KV v2.
package vault

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/Tzurrr/codeument/internal/secrets"
)

// Options configure the store.
type Options struct {
	Address      string
	Token        string
	Auth         string // token | approle
	RoleID       string
	SecretID     string
	Mount        string
	PathTemplate string
	Timeout      time.Duration
}

// Store writes to one Vault.
type Store struct {
	opts   Options
	client *http.Client

	mu    sync.Mutex
	token string
}

// New validates the options and builds a store.
func New(o Options) (*Store, error) {
	if o.Address == "" {
		return nil, errors.New("vault: address is required")
	}
	if o.Mount == "" {
		o.Mount = "secret"
	}
	if o.PathTemplate == "" {
		o.PathTemplate = "servers/{hostname}/{username}"
	}
	if o.Auth == "" {
		o.Auth = "token"
	}
	switch o.Auth {
	case "token":
		if o.Token == "" {
			return nil, errors.New("vault: auth is token but no token is configured")
		}
	case "approle":
		if o.RoleID == "" || o.SecretID == "" {
			return nil, errors.New("vault: auth is approle but role_id or secret_id is missing")
		}
	default:
		return nil, fmt.Errorf("vault: auth must be token or approle (got %q)", o.Auth)
	}
	if o.Timeout <= 0 {
		o.Timeout = secrets.Timeout
	}
	o.Address = strings.TrimRight(o.Address, "/")
	return &Store{opts: o, client: &http.Client{Timeout: o.Timeout}, token: o.Token}, nil
}

func (s *Store) Name() string { return "vault" }

// Path renders the KV path for a credential.
func (s *Store) Path(c secrets.Credential) string {
	return strings.Trim(secrets.Expand(s.opts.PathTemplate, c.Host, c.Username), "/")
}

// Put writes the credential and returns a `vault://` reference.
func (s *Store) Put(ctx context.Context, c secrets.Credential) (secrets.Reference, error) {
	tok, err := s.auth(ctx)
	if err != nil {
		return secrets.Reference{}, err
	}
	path := s.Path(c)
	body := map[string]any{"data": map[string]string{
		"username": c.Username, "password": c.Password, "host": c.Host, "kind": c.Kind, "notes": c.Notes,
		"managed_by": "codeument", "updated_at": time.Now().UTC().Format(time.RFC3339),
	}}
	if err := s.do(ctx, http.MethodPost, "/v1/"+s.opts.Mount+"/data/"+path, tok, body, nil); err != nil {
		return secrets.Reference{}, err
	}
	return secrets.Reference{
		Ref: "vault://" + s.opts.Mount + "/" + path,
		URL: s.opts.Address + "/ui/vault/secrets/" + s.opts.Mount + "/show/" + path,
	}, nil
}

// Ping checks the address, credentials and mount.
func (s *Store) Ping(ctx context.Context) error {
	tok, err := s.auth(ctx)
	if err != nil {
		return err
	}
	var health struct {
		Sealed bool `json:"sealed"`
	}
	if err := s.do(ctx, http.MethodGet, "/v1/sys/health", "", nil, &health); err != nil {
		return err
	}
	if health.Sealed {
		return errors.New("vault: server is sealed")
	}
	err = s.do(ctx, http.MethodGet, "/v1/"+s.opts.Mount+"/config", tok, nil, nil)
	if err != nil && !strings.Contains(err.Error(), "404") {
		return fmt.Errorf("vault: mount %q not usable: %w", s.opts.Mount, err)
	}
	return nil
}

// auth returns a usable token, logging in with AppRole when configured.
func (s *Store) auth(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.opts.Auth == "token" {
		return s.opts.Token, nil
	}
	if s.token != "" {
		return s.token, nil
	}
	var out struct {
		Auth struct {
			ClientToken string `json:"client_token"`
		} `json:"auth"`
	}
	if err := s.do(ctx, http.MethodPost, "/v1/auth/approle/login", "", map[string]string{"role_id": s.opts.RoleID, "secret_id": s.opts.SecretID}, &out); err != nil {
		return "", fmt.Errorf("vault: approle login: %w", err)
	}
	if out.Auth.ClientToken == "" {
		return "", errors.New("vault: approle login returned no token")
	}
	s.token = out.Auth.ClientToken
	return s.token, nil
}

func (s *Store) do(ctx context.Context, method, path, token string, in, out any) error {
	var rdr io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, s.opts.Address+path, rdr)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("X-Vault-Token", token)
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return fmt.Errorf("vault: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 && resp.StatusCode != 204 {
		msg := strings.TrimSpace(string(data))
		if resp.StatusCode == 403 {
			return fmt.Errorf("vault: permission denied (403): %s", msg)
		}
		return fmt.Errorf("vault: HTTP %d: %s", resp.StatusCode, msg)
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}
