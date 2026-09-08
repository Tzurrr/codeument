// Package webhook hands credentials to an endpoint the IT team owns, so any
// password manager can be wired up without a dedicated integration.
package webhook

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
	URL         string
	BearerToken string
	Timeout     time.Duration
}

// Store posts to one endpoint.
type Store struct {
	opts   Options
	client *http.Client
}

// New validates the options.
func New(o Options) (*Store, error) {
	if o.URL == "" {
		return nil, errors.New("webhook: url is required")
	}
	if !strings.HasPrefix(o.URL, "https://") && !strings.HasPrefix(o.URL, "http://127.0.0.1") && !strings.HasPrefix(o.URL, "http://localhost") {
		return nil, errors.New("webhook: url must be https (or loopback http for testing)")
	}
	if o.Timeout <= 0 {
		o.Timeout = secrets.Timeout
	}
	return &Store{opts: o, client: &http.Client{Timeout: o.Timeout}}, nil
}

func (s *Store) Name() string { return "webhook" }

// Request is what the endpoint receives.
type Request struct {
	Host      string `json:"host"`
	MachineID string `json:"machine_id,omitempty"`
	Username  string `json:"username"`
	Kind      string `json:"kind"`
	Password  string `json:"password"`
	Notes     string `json:"notes,omitempty"`
	Source    string `json:"source"`
}

// Response is what the endpoint returns.
type Response struct {
	Reference string `json:"reference"`
	URL       string `json:"url"`
	Error     string `json:"error"`
}

// Put posts the credential and returns the reference the endpoint reports.
func (s *Store) Put(ctx context.Context, c secrets.Credential) (secrets.Reference, error) {
	raw, err := json.Marshal(Request{Host: c.Host, MachineID: c.MachineID, Username: c.Username, Kind: c.Kind, Password: c.Password, Notes: c.Notes, Source: "codeument"})
	if err != nil {
		return secrets.Reference{}, err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.opts.URL, bytes.NewReader(raw))
	if err != nil {
		return secrets.Reference{}, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	if s.opts.BearerToken != "" {
		req.Header.Set("Authorization", "Bearer "+s.opts.BearerToken)
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return secrets.Reference{}, fmt.Errorf("webhook: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return secrets.Reference{}, err
	}
	if resp.StatusCode/100 != 2 {
		return secrets.Reference{}, fmt.Errorf("webhook: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(data)))
	}
	var out Response
	if len(data) > 0 {
		if err := json.Unmarshal(data, &out); err != nil {
			return secrets.Reference{}, fmt.Errorf("webhook: response is not JSON: %w", err)
		}
	}
	if out.Error != "" {
		return secrets.Reference{}, fmt.Errorf("webhook: %s", out.Error)
	}
	if out.Reference == "" && out.URL == "" {
		return secrets.Reference{}, errors.New("webhook: response carried neither reference nor url")
	}
	if out.Reference == "" {
		out.Reference = out.URL
	}
	return secrets.Reference{Ref: out.Reference, URL: out.URL}, nil
}

// Ping does a HEAD (falling back to GET) to check the endpoint answers.
func (s *Store) Ping(ctx context.Context) error {
	for _, method := range []string{http.MethodHead, http.MethodGet} {
		req, err := http.NewRequestWithContext(ctx, method, s.opts.URL, nil)
		if err != nil {
			return err
		}
		if s.opts.BearerToken != "" {
			req.Header.Set("Authorization", "Bearer "+s.opts.BearerToken)
		}
		resp, err := s.client.Do(req)
		if err != nil {
			return fmt.Errorf("webhook: %w", err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusMethodNotAllowed && method == http.MethodHead {
			continue // endpoints that only accept POST answer 405; try GET
		}
		if resp.StatusCode == 401 || resp.StatusCode == 403 {
			return fmt.Errorf("webhook: endpoint rejected the token (%d)", resp.StatusCode)
		}
		return nil
	}
	return nil
}
