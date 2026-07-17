package server_test

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
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

// Contract-style e2e against a deterministic fake upstream. Scenarios mirror
// contracts/fake_upstream.py (normal / tool-rewrite / thinking / empty-stream).

func TestAnthropicMessagesE2EAgainstFakeUpstream(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" && r.URL.Path != "/chat/completions" {
			http.NotFound(w, r)
			return
		}
		scenario := r.URL.Query().Get("scenario")
		if scenario == "" {
			scenario = r.Header.Get("X-Fake-Scenario")
		}
		if scenario == "" {
			scenario = "normal"
		}
		// Auth / model override headers should always be present.
		if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
			t.Errorf("missing auth header")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		write := func(payload map[string]any) {
			encoded, _ := json.Marshal(payload)
			_, _ = w.Write([]byte("data: " + string(encoded) + "\n\n"))
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
		}
		base := map[string]any{"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": "grok-4.5"}
		write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"role": "assistant"}, "finish_reason": nil}}}))
		switch scenario {
		case "tool-rewrite":
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call_fixture", "type": "function", "function": map[string]any{"name": "Update", "arguments": `{"path":"/wrong"}`}}}}, "finish_reason": nil}}}))
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "Update", "arguments": `{"file_path":"/right","old_string":"a","new_string":""}`}}}}, "finish_reason": nil}}}))
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "tool_calls"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}}))
		case "thinking":
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"reasoning_content": "plan "}, "finish_reason": nil}}}))
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "done"}, "finish_reason": nil}}}))
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}}))
		case "empty-stream":
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 1, "completion_tokens": 0, "total_tokens": 1}}))
		default:
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "fixture "}, "finish_reason": nil}}}))
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{"content": "response"}, "finish_reason": nil}}}))
			write(merge(base, map[string]any{"choices": []any{map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"}}, "usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}}))
		}
		_, _ = w.Write([]byte("data: [DONE]\n\n"))
	}))
	defer upstream.Close()

	handler := server.NewMux(server.Options{
		Ready:           func() bool { return true },
		MessagesEnabled: true,
		APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
		Candidates: []pool.Candidate{
			{ID: "acc-a", Token: "tok-a", Enabled: true, RequestCount: 1},
			{ID: "acc-b", Token: "tok-b", Enabled: true, RequestCount: 0},
		},
		AffinityStore: &memAffinity{},
		Config: config.Config{
			UpstreamBase: upstream.URL + "/v1",
			SSEKeepalive: 4 * time.Second,
			DefaultModel: "grok-4.5",
		},
	})

	t.Run("normal non-stream", func(t *testing.T) {
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"metadata":{"session_id":"sess-1"}}`))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("anthropic-version", "2023-06-01")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Grok2API-Protocol") != "anthropic" {
			t.Fatalf("protocol header missing: %v", rec.Header())
		}
		if rec.Header().Get("X-Grok2API-Prompt-Stable") == "" {
			t.Fatalf("prompt stable header missing")
		}
		var payload map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &payload); err != nil {
			t.Fatal(err)
		}
		if payload["type"] != "message" || payload["role"] != "assistant" {
			t.Fatalf("payload %#v", payload)
		}
		content, _ := payload["content"].([]any)
		if len(content) == 0 {
			t.Fatalf("no content blocks: %#v", payload)
		}
		text := content[0].(map[string]any)["text"]
		if text != "fixture response" {
			t.Fatalf("text=%#v", text)
		}
	})

	t.Run("stream thinking", func(t *testing.T) {
		// Force thinking scenario by pointing BaseURL with query is hard; instead use header-aware server.
		// Rebuild upstream scenario via dedicated handler path: encode scenario in model alias is overkill.
		// Use a second handler instance with scenario sticky via token mapping: simpler - temporary override.
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			frames := []string{
				`data: {"choices":[{"delta":{"reasoning_content":"plan "}}]}` + "\n\n",
				`data: {"choices":[{"delta":{"content":"done"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}` + "\n\n",
				"data: [DONE]\n\n",
			}
			for _, frame := range frames {
				_, _ = w.Write([]byte(frame))
				if f, ok := w.(http.Flusher); ok {
					f.Flush()
				}
			}
		}))
		defer local.Close()
		h := server.NewMux(server.Options{
			Ready:           func() bool { return true },
			MessagesEnabled: true,
			APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
			Candidates:      []pool.Candidate{{ID: "acc", Token: "t", Enabled: true}},
			Config:          config.Config{UpstreamBase: local.URL + "/v1", DefaultModel: "grok-4.5"},
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":16,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		body := rec.Body.String()
		for _, marker := range []string{"event: message_start", "thinking_delta", "plan", "text_delta", "done", "event: message_delta", "event: message_stop"} {
			if !strings.Contains(body, marker) {
				t.Fatalf("missing %q in %q", marker, body)
			}
		}
		if rec.Header().Get("X-Grok2API-Protocol") != "anthropic" {
			t.Fatalf("headers=%v", rec.Header())
		}
	})

	t.Run("stream tool rewrite", func(t *testing.T) {
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			frames := []string{
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Update","arguments":"{\"path\":\"/wrong\"}"}}]}}]}` + "\n\n",
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Update","arguments":"{\"file_path\":\"/right\",\"old_string\":\"a\",\"new_string\":\"\"}"}}]},"finish_reason":"tool_calls"}]}` + "\n\n",
				"data: [DONE]\n\n",
			}
			for _, frame := range frames {
				_, _ = w.Write([]byte(frame))
			}
		}))
		defer local.Close()
		h := server.NewMux(server.Options{
			Ready:           func() bool { return true },
			MessagesEnabled: true,
			APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
			Candidates:      []pool.Candidate{{ID: "acc", Token: "t", Enabled: true}},
			Config:          config.Config{UpstreamBase: local.URL + "/v1", DefaultModel: "grok-4.5"},
		})
		body := `{"model":"grok-4.5","max_tokens":16,"stream":true,"tools":[{"name":"Edit","input_schema":{"type":"object"}}],"messages":[{"role":"user","content":"edit"}]}`
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		out := rec.Body.String()
		for _, marker := range []string{`"type":"tool_use"`, `"name":"Edit"`, "input_json_delta", `"stop_reason":"tool_use"`, "event: message_stop"} {
			if !strings.Contains(out, marker) {
				t.Fatalf("missing %q in %q", marker, out)
			}
		}
	})

	t.Run("empty stream failovers then errors", func(t *testing.T) {
		hits := 0
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			hits++
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":0,"total_tokens":1}}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer local.Close()
		h := server.NewMux(server.Options{
			Ready:           func() bool { return true },
			MessagesEnabled: true,
			APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
			Candidates: []pool.Candidate{
				{ID: "acc-a", Token: "t1", Enabled: true, RequestCount: 0},
				{ID: "acc-b", Token: "t2", Enabled: true, RequestCount: 1},
			},
			Config: config.Config{UpstreamBase: local.URL + "/v1", DefaultModel: "grok-4.5"},
		})
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":8,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if hits < 2 {
			t.Fatalf("expected failover across accounts, hits=%d", hits)
		}
		// After both empty, stream may open with terminal error frames.
		out := rec.Body.String()
		if rec.Code == http.StatusOK {
			if !strings.Contains(out, "event: error") && !strings.Contains(out, "empty model output") {
				t.Fatalf("expected empty error terminal, body=%q", out)
			}
		}
	})

	t.Run("affinity prefer header", func(t *testing.T) {
		store := &memAffinity{bound: map[string]string{}}
		// Pre-bind a fingerprint for session metadata path: prompt_cache_key from metadata.session_id.
		// ChatFingerprint looks at prompt_cache_key on OpenAI body after BuildOpenAIChatBody.
		// ExtractPromptCacheKey puts session_id onto prompt_cache_key.
		// Prepare then fingerprint uses prompt_cache_key.
		// We can't easily know hash; instead bind after first request.
		local := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\n"))
			_, _ = w.Write([]byte("data: [DONE]\n\n"))
		}))
		defer local.Close()
		h := server.NewMux(server.Options{
			Ready:           func() bool { return true },
			MessagesEnabled: true,
			APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "true"}, nil),
			Candidates: []pool.Candidate{
				{ID: "acc-a", Token: "t1", Enabled: true, RequestCount: 10},
				{ID: "acc-b", Token: "t2", Enabled: true, RequestCount: 0},
			},
			AffinityStore: store,
			Config:        config.Config{UpstreamBase: local.URL + "/v1", DefaultModel: "grok-4.5"},
		})
		// First call binds affinity for session.
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":8,"messages":[{"role":"user","content":"hi"}],"metadata":{"session_id":"sticky-session"}}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("first status=%d body=%s", rec.Code, rec.Body.String())
		}
		if rec.Header().Get("X-Grok2API-Account") == "" {
			t.Fatalf("missing account header: %v", rec.Header())
		}
		// Second call should prefer affinity (even if higher request count).
		req = httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(`{"model":"grok-4.5","max_tokens":8,"messages":[{"role":"user","content":"hi again"}],"metadata":{"session_id":"sticky-session"}}`))
		req.Header.Set("Authorization", "Bearer secret")
		rec = httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		if rec.Header().Get("X-Grok2API-Affinity") != "1" {
			t.Fatalf("expected affinity header, got %v body=%s", rec.Header(), rec.Body.String())
		}
	})
}

type memAffinity struct {
	bound map[string]string
}

func (m *memAffinity) GetAffinity(_ context.Context, fingerprint string) (string, error) {
	if m.bound == nil {
		return "", nil
	}
	return m.bound[fingerprint], nil
}

func (m *memAffinity) BindAffinity(_ context.Context, fingerprint, accountID string) error {
	if m.bound == nil {
		m.bound = map[string]string{}
	}
	m.bound[fingerprint] = accountID
	return nil
}

func merge(base map[string]any, extra map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range base {
		out[k] = v
	}
	for k, v := range extra {
		out[k] = v
	}
	return out
}

// Ensure scanner helpers stay available for future SSE asserts.
var _ = bufio.NewScanner
var _ = io.EOF
