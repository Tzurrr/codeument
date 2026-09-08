package ollama

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/Tzurrr/codeument/internal/llm"
)

func TestGenerateAndPing(t *testing.T) {
	var got chatRequest
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/chat":
			_ = json.NewDecoder(r.Body).Decode(&got)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"model": "qwen2.5:14b", "done": true, "prompt_eval_count": 10, "eval_count": 5,
				"message": map[string]string{"role": "assistant", "content": "Here you go:\n```json\n{\"title\":\"x\"}\n```"},
			})
		case "/api/tags":
			_ = json.NewEncoder(w).Encode(map[string]any{"models": []map[string]string{{"name": "qwen2.5:14b"}}})
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv.Close()
	p := New(srv.URL, "qwen2.5:14b", 4096)
	resp, err := p.GenerateJSON(context.Background(), llm.Request{System: "sys", User: "usr", Schema: json.RawMessage(`{"type":"object"}`), MaxTokens: 100})
	if err != nil {
		t.Fatal(err)
	}
	if string(resp.JSON) != `{"title":"x"}` || resp.Usage.InputTokens != 10 {
		t.Fatalf("resp = %+v", resp)
	}
	if got.Model != "qwen2.5:14b" || string(got.Format) != `{"type":"object"}` || got.Options["num_ctx"].(float64) != 4096 || got.Messages[0].Content != "sys" {
		t.Fatalf("request = %+v", got)
	}
	if err := p.Ping(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := New(srv.URL, "missing", 0).Ping(context.Background()); err == nil {
		t.Fatal("expected missing-model error")
	}
	if _, err := New("http://127.0.0.1:1", "m", 0).GenerateJSON(context.Background(), llm.Request{}); !errors.Is(err, llm.ErrUnavailable) {
		t.Fatalf("expected ErrUnavailable, got %v", err)
	}
}
