package server_test

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/auth"
	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/server"
)

func TestChatAndResponsesE2EAgainstFakeUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		frames := []string{
			`data: {"id":"chatcmpl_x","choices":[{"delta":{"reasoning_content":"plan "}}]}` + "\n\n",
			`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
			"data: [DONE]\n\n",
		}
		for _, frame := range frames {
			_, _ = w.Write([]byte(frame))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
	}))
	defer upstream.Close()

	opts := server.Options{
		Ready:            func() bool { return true },
		ChatEnabled:      true,
		ResponsesEnabled: true,
		APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
		Candidates:       []pool.Candidate{{ID: "acc", Token: "t", Enabled: true}},
		Config:           config.Config{UpstreamBase: upstream.URL + "/v1", DefaultModel: "grok-4.5", SSEKeepalive: 4 * time.Second},
	}
	h := server.NewMux(opts)

	t.Run("chat non-stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.5","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Grok2API-Protocol") != "openai_chat" {
			t.Fatalf("headers=%v", rec.Header())
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		choices, _ := payload["choices"].([]any)
		if len(choices) == 0 {
			t.Fatalf("payload %#v", payload)
		}
		msg := choices[0].(map[string]any)["message"].(map[string]any)
		if msg["content"] != "hi" {
			t.Fatalf("message %#v", msg)
		}
		if msg["reasoning_content"] != "plan " && msg["reasoning_content"] != "plan" {
			// rawString preserves trailing space if present
			if rc, _ := msg["reasoning_content"].(string); !strings.Contains(rc, "plan") {
				t.Fatalf("reasoning %#v", msg["reasoning_content"])
			}
		}
	})

	t.Run("chat stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"grok-4.5","stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		for _, marker := range []string{"data: ", "hi", "data: [DONE]"} {
			if !strings.Contains(body, marker) {
				t.Fatalf("missing %q in %q", marker, body)
			}
		}
	})

	t.Run("responses stream with reasoning", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"grok-4.5","stream":true,"input":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		for _, marker := range []string{"event: response.created", "reasoning_summary_text.delta", "response.output_text.delta", "event: response.completed", "data: [DONE]"} {
			if !strings.Contains(body, marker) {
				t.Fatalf("missing %q in %q", marker, body)
			}
		}
		if rec.Header().Get("X-Grok2API-Protocol") != "openai_responses" {
			t.Fatalf("headers=%v", rec.Header())
		}
	})
}
