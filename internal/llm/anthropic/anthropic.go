// Package anthropic implements llm.Provider with the official Anthropic Go
// SDK, using structured outputs so the model returns schema-valid JSON.
package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	sdk "github.com/anthropics/anthropic-sdk-go"
	"github.com/anthropics/anthropic-sdk-go/option"

	"github.com/Tzurrr/codeument/internal/llm"
)

// DefaultModel is used when the config does not name one.
const DefaultModel = "claude-opus-5"

func init() {
	llm.Register("anthropic", func(cfg llm.Config) (llm.Provider, error) {
		if cfg.Anthropic.APIKey == "" {
			return nil, errors.New("anthropic: api key is empty (set llm.anthropic.api_key or ANTHROPIC_API_KEY)")
		}
		return New(Options{APIKey: cfg.Anthropic.APIKey, Model: cfg.Anthropic.Model, MaxTokens: cfg.Anthropic.MaxTokens, Fallbacks: cfg.Anthropic.Fallbacks, BaseURL: cfg.Anthropic.BaseURL}), nil
	})
}

// Options configure the provider.
type Options struct {
	APIKey    string
	Model     string
	MaxTokens int
	Fallbacks bool
	BaseURL   string
	Timeout   time.Duration
}

// Provider calls the Messages API.
type Provider struct {
	client    sdk.Client
	model     string
	maxTokens int
	fallbacks bool
}

// New builds a provider.
func New(o Options) *Provider {
	if o.Model == "" {
		o.Model = DefaultModel
	}
	if o.MaxTokens <= 0 {
		o.MaxTokens = 8000
	}
	if o.Timeout <= 0 {
		o.Timeout = 90 * time.Second
	}
	opts := []option.RequestOption{option.WithAPIKey(o.APIKey), option.WithRequestTimeout(o.Timeout)}
	if o.BaseURL != "" {
		opts = append(opts, option.WithBaseURL(o.BaseURL))
	}
	return &Provider{client: sdk.NewClient(opts...), model: o.Model, maxTokens: o.MaxTokens, fallbacks: o.Fallbacks}
}

func (p *Provider) Name() string  { return "anthropic" }
func (p *Provider) Model() string { return p.model }

// GenerateJSON runs one request with a JSON-schema constrained output.
func (p *Provider) GenerateJSON(ctx context.Context, req llm.Request) (*llm.Response, error) {
	maxTokens := int64(p.maxTokens)
	if req.MaxTokens > 0 {
		maxTokens = int64(req.MaxTokens)
	}
	params := sdk.BetaMessageNewParams{
		Model:     sdk.Model(p.model),
		MaxTokens: maxTokens,
		Messages:  []sdk.BetaMessageParam{sdk.NewBetaUserMessage(sdk.NewBetaTextBlock(req.User))},
	}
	if req.System != "" {
		params.System = []sdk.BetaTextBlockParam{{Text: req.System, CacheControl: sdk.NewBetaCacheControlEphemeralParam()}}
	}
	if len(req.Schema) > 0 {
		params.OutputConfig = sdk.BetaOutputConfigParam{Format: sdk.BetaJSONOutputFormatParam{Schema: json.RawMessage(req.Schema)}}
	}
	if p.fallbacks {
		params.Betas = []sdk.AnthropicBeta{sdk.AnthropicBetaServerSideFallback2026_07_01}
		params.Fallbacks = sdk.BetaFallbacksParamOfDefault()
	}
	msg, err := p.client.Beta.Messages.New(ctx, params)
	if err != nil {
		return nil, mapError(err)
	}
	switch msg.StopReason {
	case sdk.BetaStopReasonRefusal:
		return nil, fmt.Errorf("%w: %s", llm.ErrRefused, msg.StopDetails.Explanation)
	case sdk.BetaStopReasonMaxTokens:
		return nil, llm.ErrTruncated
	}
	var text string
	for _, block := range msg.Content {
		if tb, ok := block.AsAny().(sdk.BetaTextBlock); ok {
			text += tb.Text
		}
	}
	out, err := llm.ExtractJSON(text)
	if err != nil {
		return nil, err
	}
	return &llm.Response{
		JSON:  out,
		Model: string(msg.Model),
		Usage: llm.Usage{InputTokens: int(msg.Usage.InputTokens), OutputTokens: int(msg.Usage.OutputTokens)},
	}, nil
}

// Ping performs a tiny request to validate the key and model.
func (p *Provider) Ping(ctx context.Context) error {
	_, err := p.client.Beta.Messages.New(ctx, sdk.BetaMessageNewParams{
		Model:     sdk.Model(p.model),
		MaxTokens: 16,
		Messages:  []sdk.BetaMessageParam{sdk.NewBetaUserMessage(sdk.NewBetaTextBlock("Reply with the single word: ok"))},
	})
	return mapError(err)
}

func mapError(err error) error {
	if err == nil {
		return nil
	}
	var apiErr *sdk.Error
	if errors.As(err, &apiErr) {
		switch apiErr.StatusCode {
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: %v", llm.ErrRateLimited, err)
		case http.StatusUnauthorized, http.StatusForbidden:
			return fmt.Errorf("anthropic: authentication failed (check the api key): %v", err)
		case http.StatusNotFound:
			return fmt.Errorf("anthropic: model not found: %v", err)
		}
		if apiErr.StatusCode >= 500 {
			return fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
		}
		return err
	}
	return fmt.Errorf("%w: %v", llm.ErrUnavailable, err)
}
