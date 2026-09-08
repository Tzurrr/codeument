// Package ollama implements llm.Provider against an Ollama server's
// /api/chat endpoint with JSON-schema constrained output.
package ollama

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

	"github.com/Tzurrr/codeument/internal/llm"
)

func init() {
	llm.Register("ollama", func(cfg llm.Config) (llm.Provider, error) {
		return New(cfg.Ollama.BaseURL, cfg.Ollama.Model, cfg.Ollama.NumCtx), nil
	})
}

// Provider talks to one Ollama server.
type Provider struct {
	baseURL string
	model   string
	numCtx  int
	client  *http.Client
}

// New builds a provider. Timeouts are generous because CPU inference is slow.
func New(baseURL, model string, numCtx int) *Provider {
	if baseURL == "" {
		baseURL = "http://127.0.0.1:11434"
	}
	if model == "" {
		model = "qwen2.5:14b"
	}
	return &Provider{baseURL: strings.TrimRight(baseURL, "/"), model: model, numCtx: numCtx, client: &http.Client{Timeout: 5 * time.Minute}}
}

func (p *Provider) Name() string  { return "ollama" }
func (p *Provider) Model() string { return p.model }

type chatRequest struct {
	Model    string          `json:"model"`
	Messages []chatMessage   `json:"messages"`
	Stream   bool            `json:"stream"`
	Format   json.RawMessage `json:"format,omitempty"`
	Options  map[string]any  `json:"options,omitempty"`
}

type chatMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type chatResponse struct {
	Model   string      `json:"model"`
	Message chatMessage `json:"message"`
	Done    bool        `json:"done"`
	// Ollama reports token counts with these names.
	PromptEvalCount int    `json:"prompt_eval_count"`
	EvalCount       int    `json:"eval_count"`
	Error           string `json:"error"`
}

// GenerateJSON runs one chat completion constrained to req.Schema.
func (p *Provider) GenerateJSON(ctx context.Context, req llm.Request) (*llm.Response, error) {
	body := chatRequest{
		Model:  p.model,
		Stream: false,
		Format: req.Schema,
		Messages: []chatMessage{
			{Role: "system", Content: req.System},
			{Role: "user", Content: req.User},
		},
		Options: map[string]any{"temperature": 0},
	}
	if p.numCtx > 0 {
		body.Options["num_ctx"] = p.numCtx
	}
	if req.MaxTokens > 0 {
		body.Options["num_predict"] = req.MaxTokens
	}
	if len(req.Schema) == 0 {
		body.Format = json.RawMessage(`"json"`)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, p.baseURL+"/api/chat", bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusTooManyRequests {
		return nil, llm.ErrRateLimited
	}
	if resp.StatusCode/100 != 2 {
		return nil, fmt.Errorf("ollama: %s: %s", resp.Status, strings.TrimSpace(string(data)))
	}
	var cr chatResponse
	if err := json.Unmarshal(data, &cr); err != nil {
		return nil, fmt.Errorf("ollama: decode: %w", err)
	}
	if cr.Error != "" {
		return nil, fmt.Errorf("ollama: %s", cr.Error)
	}
	out, err := llm.ExtractJSON(cr.Message.Content)
	if err != nil {
		return nil, fmt.Errorf("%w: %s", llm.ErrInvalidJSON, truncate(cr.Message.Content, 200))
	}
	return &llm.Response{JSON: out, Model: cr.Model, Usage: llm.Usage{InputTokens: cr.PromptEvalCount, OutputTokens: cr.EvalCount}}, nil
}

// Ping checks the server is up and the model is pulled.
func (p *Provider) Ping(ctx context.Context) error {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, p.baseURL+"/api/tags", nil)
	if err != nil {
		return err
	}
	resp, err := p.client.Do(httpReq)
	if err != nil {
		return fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("ollama: %s", resp.Status)
	}
	var tags struct {
		Models []struct {
			Name string `json:"name"`
		} `json:"models"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&tags); err != nil {
		return err
	}
	want := p.model
	if !strings.Contains(want, ":") {
		want += ":latest"
	}
	for _, m := range tags.Models {
		if m.Name == want || m.Name == p.model {
			return nil
		}
	}
	return errors.New("ollama: model " + p.model + " is not pulled (run: ollama pull " + p.model + ")")
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
