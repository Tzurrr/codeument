// Package client is the typed HTTP client for the relay, and the
// engine.Engine implementation used in relay mode.
package client

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/Tzurrr/codeument/internal/docs"
	"github.com/Tzurrr/codeument/internal/engine"
	"github.com/Tzurrr/codeument/internal/explain"
	"github.com/Tzurrr/codeument/internal/llm"
	"github.com/Tzurrr/codeument/internal/relay/api"
	"github.com/Tzurrr/codeument/internal/secrets"
	"github.com/Tzurrr/codeument/internal/summarize"
)

// Client talks to one relay.
type Client struct {
	BaseURL       string
	Token         string
	ClientVersion string
	HTTP          *http.Client
}

// Options configure a client.
type Options struct {
	BaseURL string
	Token   string
	CAFile  string
	Version string
	Timeout time.Duration
}

// New builds a client. CAFile adds a private CA for the relay certificate.
func New(o Options) (*Client, error) {
	if o.BaseURL == "" {
		return nil, errors.New("relay url is empty")
	}
	if o.Timeout <= 0 {
		o.Timeout = 5 * time.Minute
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	if o.CAFile != "" {
		pem, err := os.ReadFile(o.CAFile)
		if err != nil {
			return nil, fmt.Errorf("relay.ca_file: %w", err)
		}
		pool, _ := x509.SystemCertPool()
		if pool == nil {
			pool = x509.NewCertPool()
		}
		if !pool.AppendCertsFromPEM(pem) {
			return nil, errors.New("relay.ca_file: no certificates found")
		}
		transport.TLSClientConfig = &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	}
	return &Client{BaseURL: strings.TrimRight(o.BaseURL, "/"), Token: o.Token, ClientVersion: o.Version, HTTP: &http.Client{Timeout: o.Timeout, Transport: transport}}, nil
}

// APIError is a structured error from the relay.
type APIError struct {
	Status int
	Code   string
	Msg    string
}

func (e *APIError) Error() string { return fmt.Sprintf("relay: %s (%d %s)", e.Msg, e.Status, e.Code) }

func (c *Client) do(ctx context.Context, method, path string, in, out any) error {
	var body io.Reader
	if in != nil {
		raw, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.BaseURL+path, body)
	if err != nil {
		return err
	}
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.Token != "" {
		req.Header.Set("Authorization", "Bearer "+c.Token)
	}
	if c.ClientVersion != "" {
		req.Header.Set("X-Codeument-Version", c.ClientVersion)
	}
	resp, err := c.HTTP.Do(req)
	if err != nil {
		return fmt.Errorf("%w: relay %s: %v", llm.ErrUnavailable, c.BaseURL, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if resp.StatusCode/100 != 2 {
		var ae api.Error
		_ = json.Unmarshal(data, &ae)
		if ae.Error == "" {
			ae.Error = strings.TrimSpace(string(data))
		}
		err := &APIError{Status: resp.StatusCode, Code: ae.Code, Msg: ae.Error}
		switch ae.Code {
		case "rate_limited":
			return fmt.Errorf("%w: %v", llm.ErrRateLimited, err)
		case "refused":
			return fmt.Errorf("%w: %v", llm.ErrRefused, err)
		case "llm_unavailable":
			return fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
		case "invalid_output":
			return fmt.Errorf("%w: %v", llm.ErrInvalidJSON, err)
		case "not_found":
			return docs.ErrNotFound
		}
		return err
	}
	if out != nil {
		if err := json.Unmarshal(data, out); err != nil {
			return fmt.Errorf("relay: decode response: %w", err)
		}
	}
	return nil
}

// Enroll exchanges a one-time code for a token.
func (c *Client) Enroll(ctx context.Context, req api.EnrollRequest) (*api.EnrollResponse, error) {
	var out api.EnrollResponse
	if err := c.do(ctx, http.MethodPost, api.PathEnroll, req, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Version fetches the relay's description.
func (c *Client) Version(ctx context.Context) (*api.VersionResponse, error) {
	var out api.VersionResponse
	if err := c.do(ctx, http.MethodGet, api.PathVersion, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Engine adapts the client to engine.Engine.
type Engine struct {
	*Client
}

// NewEngine wraps a client.
func NewEngine(c *Client) *Engine { return &Engine{Client: c} }

func (e *Engine) Name() string { return "relay(" + e.BaseURL + ")" }

func (e *Engine) Summarize(ctx context.Context, in summarize.Input) (*summarize.Output, error) {
	var out api.SummarizeResponse
	if err := e.do(ctx, http.MethodPost, api.PathSummarize, api.SummarizeRequest{Input: in}, &out); err != nil {
		return nil, err
	}
	return &out.Output, nil
}

func (e *Engine) Explain(ctx context.Context, in explain.Input) ([]explain.Explanation, error) {
	var out api.ExplainResponse
	if err := e.do(ctx, http.MethodPost, api.PathExplain, api.ExplainRequest{Input: in}, &out); err != nil {
		return nil, err
	}
	return out.Explanations, nil
}

func (e *Engine) Publish(ctx context.Context, doc docs.Document) (docs.PageRef, error) {
	var out api.PublishResponse
	if err := e.do(ctx, http.MethodPost, api.PathPublish, api.PublishRequest{Document: doc}, &out); err != nil {
		return docs.PageRef{}, err
	}
	return out.PageRef, nil
}

func (e *Engine) FindDoc(ctx context.Context, id string) (*docs.PageRef, error) {
	var out api.FindResponse
	if err := e.do(ctx, http.MethodGet, api.PathDocs+id, nil, &out); err != nil {
		return nil, err
	}
	return out.PageRef, nil
}

func (e *Engine) GetDoc(ctx context.Context, ref docs.PageRef) (*docs.Document, error) {
	var out api.DocGetResponse
	if err := e.do(ctx, http.MethodPost, api.PathDocGet, api.DocGetRequest{Ref: ref}, &out); err != nil {
		return nil, err
	}
	if out.Document == nil {
		return nil, docs.ErrNotFound
	}
	return out.Document, nil
}

func (e *Engine) Defaults(ctx context.Context) (*engine.Defaults, error) {
	var out engine.Defaults
	if err := e.do(ctx, http.MethodGet, api.PathConfig, nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

func (e *Engine) StoreCredential(ctx context.Context, cred secrets.Credential) (secrets.Reference, error) {
	var out api.CredentialResponse
	if err := e.do(ctx, http.MethodPost, api.PathCredentials, api.CredentialRequest{Credential: cred}, &out); err != nil {
		return secrets.Reference{}, err
	}
	return out.Reference, nil
}

func (e *Engine) Ping(ctx context.Context) error {
	_, err := e.Defaults(ctx)
	return err
}
