// Package llm defines the inference provider interface used for
// summarization and directory explanations, and constructs providers from
// config.
package llm

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
)

// Request is a single-turn structured generation request.
type Request struct {
	System    string
	User      string
	Schema    json.RawMessage // JSON schema the response must satisfy
	MaxTokens int
}

// Usage is token accounting for one call.
type Usage struct {
	InputTokens  int `json:"input_tokens"`
	OutputTokens int `json:"output_tokens"`
}

// Response is the raw JSON the model produced plus metadata.
type Response struct {
	JSON  json.RawMessage
	Usage Usage
	Model string
}

// Provider is implemented by each backend.
type Provider interface {
	Name() string
	Model() string
	GenerateJSON(ctx context.Context, req Request) (*Response, error)
	Ping(ctx context.Context) error
}

// Sentinel errors callers can branch on.
var (
	ErrRefused     = errors.New("llm: request refused by safety policy")
	ErrRateLimited = errors.New("llm: rate limited")
	ErrTruncated   = errors.New("llm: output truncated (max_tokens)")
	ErrInvalidJSON = errors.New("llm: response is not valid JSON")
	ErrUnavailable = errors.New("llm: provider unavailable")
)

// Config is the provider-independent view of config.LLM so this package does
// not import config.
type Config struct {
	Provider  string
	Anthropic struct {
		APIKey    string
		Model     string
		MaxTokens int
		Fallbacks bool
		BaseURL   string
	}
	Ollama struct {
		BaseURL string
		Model   string
		NumCtx  int
	}
}

// Factory builds a provider from config; registered by each implementation
// package so importing them is what enables them.
type Factory func(cfg Config) (Provider, error)

var factories = map[string]Factory{}

// Register adds a provider factory.
func Register(name string, f Factory) { factories[name] = f }

// New constructs the provider named in cfg.Provider.
func New(cfg Config) (Provider, error) {
	f, ok := factories[cfg.Provider]
	if !ok {
		return nil, fmt.Errorf("llm: unknown provider %q", cfg.Provider)
	}
	return f(cfg)
}

// ExtractJSON finds the first JSON object in text, tolerating code fences and
// prose around it.
func ExtractJSON(text string) (json.RawMessage, error) {
	start := -1
	depth := 0
	inStr := false
	esc := false
	for i := 0; i < len(text); i++ {
		c := text[i]
		if start == -1 {
			if c == '{' {
				start = i
				depth = 1
			}
			continue
		}
		if inStr {
			switch {
			case esc:
				esc = false
			case c == '\\':
				esc = true
			case c == '"':
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				candidate := json.RawMessage(text[start : i+1])
				if json.Valid(candidate) {
					return candidate, nil
				}
				return nil, ErrInvalidJSON
			}
		}
	}
	return nil, ErrInvalidJSON
}
