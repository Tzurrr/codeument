// Package fake is an in-memory llm.Provider for tests and offline demos. It
// answers with a canned Draft-shaped JSON derived from the prompt, or with
// whatever Responder returns.
package fake

import (
	"context"
	"encoding/json"
	"regexp"
	"strings"
	"sync"

	"github.com/Tzurrr/codeument/internal/llm"
)

func init() {
	llm.Register("fake", func(llm.Config) (llm.Provider, error) { return New(), nil })
}

// Provider records requests and answers them.
type Provider struct {
	mu       sync.Mutex
	Requests []llm.Request
	// Responder, when set, produces the JSON for a request.
	Responder func(req llm.Request) (json.RawMessage, error)
	// Err, when set, is returned by every call.
	Err error
}

// New returns a fake provider with the default responder.
func New() *Provider { return &Provider{} }

func (p *Provider) Name() string  { return "fake" }
func (p *Provider) Model() string { return "fake-1" }

// Ping always succeeds.
func (p *Provider) Ping(context.Context) error { return p.Err }

// GenerateJSON records the request and answers it.
func (p *Provider) GenerateJSON(_ context.Context, req llm.Request) (*llm.Response, error) {
	p.mu.Lock()
	p.Requests = append(p.Requests, req)
	p.mu.Unlock()
	if p.Err != nil {
		return nil, p.Err
	}
	var out json.RawMessage
	var err error
	if p.Responder != nil {
		out, err = p.Responder(req)
	} else {
		out, err = defaultResponse(req)
	}
	if err != nil {
		return nil, err
	}
	return &llm.Response{JSON: out, Model: p.Model(), Usage: llm.Usage{InputTokens: len(req.User) / 4, OutputTokens: len(out) / 4}}, nil
}

var reCommand = regexp.MustCompile(`(?m)^\s*\[\d+\]\s+\S+\s+exit=(\d+)\s+\S+\s+\S+\s+(.+)$`)

// isContext reports whether a prompt line is marked as context (noise).
func isContext(cmd string) bool { return strings.HasPrefix(cmd, "(context) ") }

// defaultResponse builds a plausible draft from the commands in the prompt so
// end-to-end tests exercise the real pipeline.
func defaultResponse(req llm.Request) (json.RawMessage, error) {
	if strings.Contains(req.User, `"dirs"`) || strings.Contains(req.System, "directories") {
		return explainResponse(req)
	}
	var cmds []string
	for _, m := range reCommand.FindAllStringSubmatch(req.User, -1) {
		cmd := strings.TrimSpace(m[2])
		if isContext(cmd) {
			continue
		}
		cmds = append(cmds, cmd)
	}
	title := "Shell work"
	if len(cmds) > 0 {
		title = "Work: " + firstWords(cmds[0], 4)
	}
	draft := map[string]any{
		"title":            title,
		"summary":          "Ran " + itoa(len(cmds)) + " commands. (fake provider)",
		"intent":           "unclear",
		"doc_kind":         "note",
		"steps":            []map[string]any{{"description": "Commands run", "commands": cmds, "notes": ""}},
		"commands":         cmds,
		"affected_systems": []string{},
		"files_touched":    []string{},
		"tags":             []string{"fake"},
		"suggested_location": map[string]any{
			"space": "", "parent_path": []string{}, "page_title": title,
		},
		"related_doc_id": "",
		"confidence":     "low",
		"open_questions": []string{},
	}
	return json.Marshal(draft)
}

func explainResponse(req llm.Request) (json.RawMessage, error) {
	var in struct {
		Dirs []struct {
			Path string `json:"path"`
		} `json:"dirs"`
	}
	_ = json.Unmarshal([]byte(extractJSON(req.User)), &in)
	items := make([]map[string]any, 0, len(in.Dirs))
	for _, d := range in.Dirs {
		items = append(items, map[string]any{
			"path": d.Path, "purpose": "Directory " + d.Path + " (fake explanation)", "what_runs_here": "",
			"how_to_operate": "", "config_files": []string{}, "risks": "", "confidence": "low",
		})
	}
	return json.Marshal(map[string]any{"explanations": items})
}

func extractJSON(s string) string {
	if i := strings.Index(s, "{"); i >= 0 {
		if j := strings.LastIndex(s, "}"); j > i {
			return s[i : j+1]
		}
	}
	return "{}"
}

func firstWords(s string, n int) string {
	f := strings.Fields(s)
	if len(f) > n {
		f = f[:n]
	}
	return strings.Join(f, " ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}
