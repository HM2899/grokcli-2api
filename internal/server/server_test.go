package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/auth"
	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
)

func TestLive(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/live", nil)
	NewMigrationMux(func() bool { return true }).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected status %d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["implementation"] != "go" || body["ok"] != true {
		t.Fatalf("unexpected body %#v", body)
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "application/json") {
		t.Fatalf("unexpected content type %q", got)
	}
}

func TestReadyFailClosed(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodGet, "/ready", nil)
	NewMux(Options{
		Ready:  func() bool { return false },
		Reason: func() string { return "migration pending" },
	}).ServeHTTP(recorder, request)

	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("unexpected status %d", recorder.Code)
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["reason"] != "migration pending" {
		t.Fatalf("unexpected body %#v", body)
	}
}

func TestMethodAndUnknownRoute(t *testing.T) {
	handler := NewMux(Options{Ready: func() bool { return true }, StaticDir: t.TempDir()})

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/live", nil))
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST /live status = %d", recorder.Code)
	}

	// Root is exact-match only; unknown paths must not fall through to index.html.
	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/unknown", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET /unknown status = %d body=%q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	// Missing index still 404, but route itself must match exactly "/".
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("GET / with empty static dir status = %d", recorder.Code)
	}
}

func TestHealthAndMetricsAreReadOnlyShells(t *testing.T) {
	handler := NewMigrationMux(func() bool { return false })

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/health", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/health status = %d", recorder.Code)
	}
	var health map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &health); err != nil {
		t.Fatal(err)
	}
	if health["implementation"] != "go" || health["ready"] != false {
		t.Fatalf("unexpected health %#v", health)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("/metrics status = %d", recorder.Code)
	}
	if !strings.Contains(recorder.Body.String(), "g2a_runtime_ready") {
		t.Fatalf("unexpected metrics %q", recorder.Body.String())
	}
}

func TestModelsRouteFlagAndAuth(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled models route status = %d", recorder.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	recorder = httptest.NewRecorder()
	NewMux(Options{
		Ready:             func() bool { return true },
		PublicReadEnabled: true,
		APIKeys:           auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
	}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("missing auth status = %d", recorder.Code)
	}

	req = httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	req.Header.Set("Authorization", "Bearer secret")
	recorder = httptest.NewRecorder()
	NewMux(Options{
		Ready:             func() bool { return true },
		PublicReadEnabled: true,
		APIKeys:           auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
	}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("authorized models status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body["object"] != "list" {
		t.Fatalf("unexpected models body %#v", body)
	}
}

type fakeAdminSessions struct{ ok bool }

func (f fakeAdminSessions) VerifyAdminSession(string) bool { return f.ok }

func TestAdminReadRoutesRequireFlagReadinessAndSession(t *testing.T) {
	recorder := httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("disabled status route = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return false }, AdminReadEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if recorder.Code != http.StatusServiceUnavailable {
		t.Fatalf("not-ready status route = %d", recorder.Code)
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/status", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("public admin status = %d body=%q", recorder.Code, recorder.Body.String())
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true, AdminSessions: fakeAdminSessions{ok: false}}).ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/api/dashboard", nil))
	if recorder.Code != http.StatusUnauthorized {
		t.Fatalf("dashboard without session = %d", recorder.Code)
	}

	req := httptest.NewRequest(http.MethodGet, "/admin/api/models", nil)
	req.Header.Set("X-Admin-Token", "token")
	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, AdminReadEnabled: true, AdminSessions: fakeAdminSessions{ok: true}}).ServeHTTP(recorder, req)
	if recorder.Code != http.StatusOK {
		t.Fatalf("admin models with session = %d body=%q", recorder.Code, recorder.Body.String())
	}
}

func TestMessagesRouteGates(t *testing.T) {
	for _, path := range []string{"/v1/messages", "/messages"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("disabled messages route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return false }, MessagesEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("not-ready messages route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("missing auth messages route = %d", recorder.Code)
			}

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer secret")
			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("store-unavailable messages route = %d", recorder.Code)
			}
		})
	}

	for _, path := range []string{"/v1/messages/count_tokens", "/messages/count_tokens"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("disabled count_tokens route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return false }, MessagesEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("not-ready count_tokens route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("missing auth count_tokens route = %d", recorder.Code)
			}

			// count_tokens is a pure local heuristic: no store required.
			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer secret")
			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:           func() bool { return true },
				MessagesEnabled: true,
				APIKeys:         auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusBadRequest {
				t.Fatalf("empty count_tokens without store = %d body=%q", recorder.Code, recorder.Body.String())
			}
		})
	}
}

func TestMessagesCountTokensMatchesPythonHeuristic(t *testing.T) {
	body := `{"system":"abcd","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"tool_use","name":"Edit","input":{"file_path":"/x","old_string":"a","new_string":""}}]}],"tools":[{"name":"Edit","description":"edit files","input_schema":{"type":"object"}}]}`
	recorder := httptest.NewRecorder()
	NewMux(Options{
		Ready:           func() bool { return true },
		MessagesEnabled: true,
		Store:           &postgres.Connector{},
	}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("count_tokens status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	var payload map[string]int
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["input_tokens"] != 27 {
		t.Fatalf("input_tokens = %d", payload["input_tokens"])
	}

	recorder = httptest.NewRecorder()
	NewMux(Options{Ready: func() bool { return true }, MessagesEnabled: true, Store: &postgres.Connector{}}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/messages/count_tokens", strings.NewReader(`{}`)))
	if recorder.Code != http.StatusBadRequest {
		t.Fatalf("empty count_tokens status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "invalid_request_error") {
		t.Fatalf("unexpected empty count_tokens body=%q", recorder.Body.String())
	}
}

func TestResponsesRouteGates(t *testing.T) {
	for _, path := range []string{"/v1/responses", "/responses"} {
		t.Run(path, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return true }}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("disabled responses route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{Ready: func() bool { return false }, ResponsesEnabled: true}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("not-ready responses route = %d", recorder.Code)
			}

			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:            func() bool { return true },
				ResponsesEnabled: true,
				APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`)))
			if recorder.Code != http.StatusUnauthorized {
				t.Fatalf("missing auth responses route = %d", recorder.Code)
			}

			req := httptest.NewRequest(http.MethodPost, path, strings.NewReader(`{}`))
			req.Header.Set("Authorization", "Bearer secret")
			recorder = httptest.NewRecorder()
			NewMux(Options{
				Ready:            func() bool { return true },
				ResponsesEnabled: true,
				APIKeys:          auth.NewAPIKeyVerifier(config.Config{LegacyAPIKey: "secret", RequireAPIKey: "auto"}, nil),
			}).ServeHTTP(recorder, req)
			if recorder.Code != http.StatusServiceUnavailable {
				t.Fatalf("store-unavailable responses route = %d", recorder.Code)
			}
		})
	}
}

func TestStreamAnthropicMessagesWritesSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	usage, _, err := streamAnthropicMessages(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\",\"reasoning_content\":\"plan\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"), "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage["total_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	for _, marker := range []string{"event: message_start", "event: content_block_start", "\"type\":\"thinking\"", "plan", "hi", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamAnthropicMessagesWritesToolUse(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, _, err := streamAnthropicMessages(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), "msg_test", "grok", true, []string{"Edit"}, 1)
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"\"type\":\"tool_use\"", "\"name\":\"Edit\"", "input_json_delta", "\"stop_reason\":\"tool_use\""} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamAnthropicMessagesEmitsThinkingDelta(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	_, _, err := streamAnthropicMessages(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"think\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"), "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"\"type\":\"thinking\"", "thinking_delta", "think", "event: message_stop"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamOpenAIResponsesWritesSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	usage, _, err := streamOpenAIResponses(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":2,\"completion_tokens\":1,\"total_tokens\":3}}\n\ndata: [DONE]\n\n"), "resp_test", "grok", nil, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if usage["total_tokens"] != float64(3) {
		t.Fatalf("usage = %#v", usage)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"event: response.created", "event: response.output_item.added", "event: response.output_text.delta", "event: response.completed", "data: [DONE]"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamOpenAIResponsesWritesFunctionCall(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	_, _, err := streamOpenAIResponses(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call_1\",\"type\":\"function\",\"function\":{\"name\":\"Edit\",\"arguments\":\"{\\\"file_path\\\":\\\"/x\\\",\\\"old_string\\\":\\\"a\\\",\\\"new_string\\\":\\\"\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n"), "resp_test", "grok", []string{"Edit"}, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	body := recorder.Body.String()
	for _, marker := range []string{"event: response.function_call_arguments.delta", "event: response.function_call_arguments.done", "event: response.output_item.done", "\"name\":\"Edit\"", "event: response.completed"} {
		if !strings.Contains(body, marker) {
			t.Fatalf("missing %q in %q", marker, body)
		}
	}
}

func TestStreamChatCompletionsWritesSSE(t *testing.T) {
	recorder := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	stats, err := streamChatCompletions(recorder, request, strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n"), 0)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Usage != nil {
		t.Fatalf("unexpected usage stats %#v", stats.Usage)
	}
	if recorder.Code != http.StatusOK {
		t.Fatalf("stream status = %d body=%q", recorder.Code, recorder.Body.String())
	}
	if got := recorder.Header().Get("Content-Type"); !strings.HasPrefix(got, "text/event-stream") {
		t.Fatalf("unexpected content-type %q", got)
	}
	body := recorder.Body.String()
	if !strings.Contains(body, `data: {"choices"`) || !strings.Contains(body, "data: [DONE]") {
		t.Fatalf("unexpected stream body %q", body)
	}
}

func TestAdminAndStaticFiles(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "index.html"), "home")
	mustWrite(t, filepath.Join(dir, "favicon.ico"), "ico")
	mustWrite(t, filepath.Join(dir, "admin", "index.html"), "admin")
	mustWrite(t, filepath.Join(dir, "admin", "keys.html"), "keys")
	mustWrite(t, filepath.Join(dir, "js", "app.js"), "console.log(1)")

	handler := NewMux(Options{Ready: func() bool { return true }, StaticDir: dir})

	for _, path := range []string{"/", "/favicon.ico", "/admin", "/admin/keys", "/static/js/app.js"} {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, path, nil))
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s status = %d body=%q", path, recorder.Code, recorder.Body.String())
		}
	}

	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/keys", nil))
	if got := recorder.Header().Get("Cache-Control"); !strings.Contains(got, "no-store") {
		t.Fatalf("admin cache-control = %q", got)
	}

	recorder = httptest.NewRecorder()
	handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/admin/nope", nil))
	if recorder.Code != http.StatusNotFound {
		t.Fatalf("/admin/nope status = %d", recorder.Code)
	}
}

func mustWrite(t *testing.T, path, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
		t.Fatal(err)
	}
}

type slowReader struct {
	chunks []string
	idx    int
	delay  time.Duration
}

func (s *slowReader) Read(p []byte) (int, error) {
	if s.idx >= len(s.chunks) {
		return 0, io.EOF
	}
	if s.delay > 0 {
		time.Sleep(s.delay)
	}
	n := copy(p, s.chunks[s.idx])
	s.idx++
	if n < len(s.chunks[s.idx-1]) {
		// should not happen with full chunk sizes in tests
	}
	return n, nil
}

func TestStreamAnthropicKeepaliveEmitsPing(t *testing.T) {
	recorder := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	req = req.WithContext(withAnthropicKeepalive(req.Context(), 30*time.Millisecond))
	body := &slowReader{
		delay: 80 * time.Millisecond,
		chunks: []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n",
			"data: [DONE]\n\n",
		},
	}
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	if !strings.Contains(out, "event: ping") && !strings.Contains(out, ": keepalive") {
		t.Fatalf("expected keepalive/ping in %q", out)
	}
	if !strings.Contains(out, "hi") || !strings.Contains(out, "event: message_stop") {
		t.Fatalf("missing content/terminal in %q", out)
	}
}

func TestStreamAnthropicSoftDisconnectStillClosesEnvelope(t *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", nil).WithContext(ctx)
	// Deliver content first, then cancel before [DONE] so finish path sees client_gone.
	body := &cancelAfterChunk{
		chunks: []string{
			"data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\n",
			"data: [DONE]\n\n",
		},
		cancelAfter: 1,
		cancel:      cancel,
	}
	_, _, err := streamAnthropicMessages(recorder, req, body, "msg_test", "grok", false, nil, 0)
	if err != nil {
		t.Fatal(err)
	}
	out := recorder.Body.String()
	for _, marker := range []string{"event: message_start", "hi", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q in soft-disconnect body %q", marker, out)
		}
	}
}

type cancelAfterChunk struct {
	chunks      []string
	idx         int
	cancelAfter int
	cancel      context.CancelFunc
}

func (c *cancelAfterChunk) Read(p []byte) (int, error) {
	if c.idx >= len(c.chunks) {
		return 0, io.EOF
	}
	n := copy(p, c.chunks[c.idx])
	c.idx++
	if c.idx == c.cancelAfter && c.cancel != nil {
		c.cancel()
	}
	return n, nil
}

type cancelAfterRead struct {
	io.Reader
	cancel context.CancelFunc
	after  int
	n      int
}

func (c *cancelAfterRead) Read(p []byte) (int, error) {
	n, err := c.Reader.Read(p)
	c.n++
	if c.n >= c.after && c.cancel != nil {
		c.cancel()
	}
	return n, err
}

func TestMessagesCountTokensWithoutStore(t *testing.T) {
	body := `{"system":"abcd","messages":[{"role":"user","content":"hello"}]}`
	recorder := httptest.NewRecorder()
	NewMux(Options{
		Ready:           func() bool { return true },
		MessagesEnabled: true,
		// no Store — count_tokens is a pure local heuristic
	}).ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/messages/count_tokens", strings.NewReader(body)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("count_tokens without store = %d body=%q", recorder.Code, recorder.Body.String())
	}
}
