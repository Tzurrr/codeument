package anthropic

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tzurrr/codeument/internal/llm"
)

func TestGenerateJSONAgainstStub(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/messages" {
			w.WriteHeader(404)
			return
		}
		if r.Header.Get("x-api-key") != "k" {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(401)
			_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"bad key"}}`))
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&got)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
			"content":     []map[string]any{{"type": "text", "text": `{"title":"hello"}`}},
			"stop_reason": "end_turn", "usage": map[string]int{"input_tokens": 42, "output_tokens": 7},
		})
	}))
	defer srv.Close()

	p := New(Options{APIKey: "k", BaseURL: srv.URL, Fallbacks: true})
	resp, err := p.GenerateJSON(context.Background(), llm.Request{System: "sys", User: "usr", Schema: json.RawMessage(`{"type":"object","additionalProperties":false,"properties":{"title":{"type":"string"}},"required":["title"]}`)})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"title":"hello"}` || resp.Usage.InputTokens != 42 || resp.Model != "claude-opus-5" {
		t.Fatalf("resp = %+v", resp)
	}
	if got["model"] != "claude-opus-5" {
		t.Fatalf("model = %v", got["model"])
	}
	oc, _ := got["output_config"].(map[string]any)
	format, _ := oc["format"].(map[string]any)
	if format["type"] != "json_schema" || format["schema"] == nil {
		t.Fatalf("output_config not sent: %v", got["output_config"])
	}
	if got["fallbacks"] != "default" {
		t.Fatalf("fallbacks = %v", got["fallbacks"])
	}
	sys, _ := got["system"].([]any)
	if len(sys) != 1 {
		t.Fatalf("system = %v", got["system"])
	}

	bad := New(Options{APIKey: "wrong", BaseURL: srv.URL})
	_, err = bad.GenerateJSON(context.Background(), llm.Request{User: "x"})
	if err == nil || errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("expected an authentication error, got %v", err)
	}
}

func TestRefusalAndTruncation(t *testing.T) {
	stop := "refusal"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "msg_1", "type": "message", "role": "assistant", "model": "claude-opus-5",
			"content": []map[string]any{}, "stop_reason": stop,
			"stop_details": map[string]any{"type": "refusal", "category": "cyber", "explanation": "nope"},
			"usage":        map[string]int{"input_tokens": 1, "output_tokens": 1},
		})
	}))
	defer srv.Close()
	p := New(Options{APIKey: "k", BaseURL: srv.URL})
	if _, err := p.GenerateJSON(context.Background(), llm.Request{User: "x"}); !errors.Is(err, llm.ErrRefused) {
		t.Fatalf("expected ErrRefused, got %v", err)
	}
	stop = "max_tokens"
	if _, err := p.GenerateJSON(context.Background(), llm.Request{User: "x"}); !errors.Is(err, llm.ErrTruncated) {
		t.Fatalf("expected ErrTruncated, got %v", err)
	}
}
