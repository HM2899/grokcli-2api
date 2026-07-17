package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

func TestDecodeChatRequest(t *testing.T) {
	req, err := DecodeChatRequest(strings.NewReader(`{"model":"gpt-4o","stream":false,"messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if req.Model != "gpt-4o" || req.Stream || req.Raw["messages"] == nil {
		t.Fatalf("unexpected request %#v", req)
	}
}

func TestChatServiceCompleteRejectsStreamPath(t *testing.T) {
	_, err := (&ChatService{}).Complete(t.Context(), ChatRequest{Stream: true, Raw: map[string]any{}}, nil, "")
	if err == nil || !strings.Contains(err.Error(), "ChatService.Stream") {
		t.Fatalf("expected stream path error, got %v", err)
	}
}

func TestChatServiceNoEligible(t *testing.T) {
	_, err := (&ChatService{Now: func() time.Time { return time.Unix(1000, 0) }}).Complete(
		t.Context(),
		ChatRequest{Model: "grok", Raw: map[string]any{"model": "grok"}},
		[]pool.Candidate{{ID: "disabled", Token: "t", Enabled: false}},
		"round_robin",
	)
	if err == nil || err != pool.ErrNoEligibleAccounts {
		t.Fatalf("expected no eligible, got %v", err)
	}
}

type fakeAffinityStore struct {
	bound map[string]string
}

func (f *fakeAffinityStore) GetAffinity(_ context.Context, fingerprint string) (string, error) {
	if f.bound == nil {
		return "", nil
	}
	return f.bound[fingerprint], nil
}

func (f *fakeAffinityStore) BindAffinity(_ context.Context, fingerprint, accountID string) error {
	if f.bound == nil {
		f.bound = map[string]string{}
	}
	f.bound[fingerprint] = accountID
	return nil
}

type fakePickObserver struct {
	penalties map[string]int64
	marked    []string
	released  []string
}

func (f *fakePickObserver) LoadPenalty(_ context.Context, accountID string) int64 {
	if f.penalties == nil {
		return 0
	}
	return f.penalties[accountID]
}

func (f *fakePickObserver) MarkPick(_ context.Context, accountID string) {
	f.marked = append(f.marked, accountID)
}

func (f *fakePickObserver) ReleasePick(_ context.Context, accountID string) {
	f.released = append(f.released, accountID)
}

func TestChatFingerprintUsesExplicitConversation(t *testing.T) {
	fp := ChatFingerprint(ChatRequest{Model: "grok", Raw: map[string]any{"conversation_id": "conv-1", "messages": []any{map[string]any{"role": "user", "content": "ignored"}}}})
	if fp != "chat:grok:conversation_id:conv-1" {
		t.Fatalf("fingerprint = %q", fp)
	}
}

func TestChatFingerprintFallsBackToMessagesHash(t *testing.T) {
	fp1 := ChatFingerprint(ChatRequest{Model: "grok", Raw: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}})
	fp2 := ChatFingerprint(ChatRequest{Model: "grok", Raw: map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}})
	if fp1 == "" || fp1 != fp2 || !strings.HasPrefix(fp1, "chat:grok:messages:") {
		t.Fatalf("unexpected fingerprints %q %q", fp1, fp2)
	}
}

func TestPreparePrefersAffinity(t *testing.T) {
	request := ChatRequest{Model: "grok", Raw: map[string]any{"conversation_id": "conv-1"}}
	store := &fakeAffinityStore{bound: map[string]string{"chat:grok:conversation_id:conv-1": "sticky"}}
	_, picked, _, err := (&ChatService{AffinityStore: store, Now: func() time.Time { return time.Unix(1000, 0) }}).prepare(
		t.Context(),
		request,
		[]pool.Candidate{
			{ID: "cold", Token: "t1", Enabled: true, RequestCount: 0},
			{ID: "sticky", Token: "t2", Enabled: true, RequestCount: 100},
		},
		"least_used",
	)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "sticky" {
		t.Fatalf("picked %q, want sticky", picked.ID)
	}
}

func TestPrepareBindsAffinity(t *testing.T) {
	request := ChatRequest{Model: "grok", Raw: map[string]any{"conversation_id": "conv-2"}}
	store := &fakeAffinityStore{}
	_, picked, _, err := (&ChatService{AffinityStore: store, Now: func() time.Time { return time.Unix(1000, 0) }}).prepare(
		t.Context(),
		request,
		[]pool.Candidate{{ID: "acc", Token: "t", Enabled: true}},
		"least_used",
	)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "acc" || store.bound["chat:grok:conversation_id:conv-2"] != "acc" {
		t.Fatalf("picked=%q bound=%#v", picked.ID, store.bound)
	}
}

func TestPrepareAppliesPickObserverPenalty(t *testing.T) {
	observer := &fakePickObserver{penalties: map[string]int64{"busy": 1000}}
	_, picked, _, err := (&ChatService{PickObserver: observer, Now: func() time.Time { return time.Unix(1000, 0) }}).prepare(
		t.Context(),
		ChatRequest{Model: "grok", Raw: map[string]any{"model": "grok"}},
		[]pool.Candidate{
			{ID: "busy", Token: "t1", Enabled: true, RequestCount: 0},
			{ID: "idle", Token: "t2", Enabled: true, RequestCount: 1},
		},
		"least_used",
	)
	if err != nil {
		t.Fatal(err)
	}
	if picked.ID != "idle" {
		t.Fatalf("picked %q, want idle", picked.ID)
	}
	if len(observer.marked) != 1 || observer.marked[0] != "idle" {
		t.Fatalf("marked %#v", observer.marked)
	}
}

func TestReleasePickCallsObserver(t *testing.T) {
	observer := &fakePickObserver{}
	(&ChatService{PickObserver: observer}).releasePick(t.Context(), "acc")
	if len(observer.released) != 1 || observer.released[0] != "acc" {
		t.Fatalf("released %#v", observer.released)
	}
}

func TestChatServiceCompleteRetriesEligibleChainBeforeCommit(t *testing.T) {
	attempts := []string{}
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		token := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
		attempts = append(attempts, token)
		if token == "bad" {
			w.WriteHeader(http.StatusTooManyRequests)
			_ = json.NewEncoder(w).Encode(map[string]any{"error": "rate"})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"id\":\"ok\",\"model\":\"grok\",\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}],\"usage\":{\"total_tokens\":1}}\n\ndata: [DONE]\n\n"))
	}))
	defer server.Close()

	observer := &fakePickObserver{}
	store := &fakeAffinityStore{}
	result, err := (&ChatService{
		Client:        &grok.Client{BaseURL: server.URL + "/v1", HTTP: server.Client()},
		PickObserver:  observer,
		AffinityStore: store,
		Now:           func() time.Time { return time.Unix(1000, 0) },
	}).CompleteWithResult(t.Context(), ChatRequest{Model: "grok", Raw: map[string]any{"model": "grok", "conversation_id": "conv"}}, []pool.Candidate{
		{ID: "bad", Token: "bad", Enabled: true, RequestCount: 0},
		{ID: "ok", Token: "ok", Enabled: true, RequestCount: 1},
	}, "least_used")
	if err != nil {
		t.Fatal(err)
	}
	if result.AccountID != "ok" || !strings.Contains(result.Payload["id"].(string), "ok") {
		t.Fatalf("unexpected result %#v", result)
	}
	if strings.Join(attempts, ",") != "bad,ok" {
		t.Fatalf("attempts = %#v", attempts)
	}
	if strings.Join(observer.marked, ",") != "bad,ok" {
		t.Fatalf("marked = %#v", observer.marked)
	}
	if strings.Join(observer.released, ",") != "bad" {
		t.Fatalf("released = %#v", observer.released)
	}
	if store.bound["chat:grok:conversation_id:conv"] != "ok" {
		t.Fatalf("affinity bound %#v", store.bound)
	}
}

func TestChatServiceCompleteReleasesChainWhenAllFail(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(map[string]any{"error": "rate"})
	}))
	defer server.Close()

	observer := &fakePickObserver{}
	_, err := (&ChatService{
		Client:       &grok.Client{BaseURL: server.URL + "/v1", HTTP: server.Client()},
		PickObserver: observer,
		Now:          func() time.Time { return time.Unix(1000, 0) },
	}).CompleteWithResult(t.Context(), ChatRequest{Model: "grok", Raw: map[string]any{"model": "grok"}}, []pool.Candidate{
		{ID: "a", Token: "a", Enabled: true, RequestCount: 0},
		{ID: "b", Token: "b", Enabled: true, RequestCount: 1},
	}, "least_used")
	if err == nil {
		t.Fatal("expected error")
	}
	if strings.Join(observer.marked, ",") != "a,b" {
		t.Fatalf("marked = %#v", observer.marked)
	}
	if strings.Join(observer.released, ",") != "a,b" {
		t.Fatalf("released = %#v", observer.released)
	}
}

func TestCollectorBuildsNonStreamCompletion(t *testing.T) {
	collector := newChatCollector("grok-4.5")
	sse := bytes.NewBufferString(`data: {"id":"abc","created":123,"model":"grok-4.5","choices":[{"delta":{"content":"he"}}]}

data: {"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}],"usage":{"total_tokens":3}}

data: [DONE]

`)
	if err := grok.ReadSSE(sse, collector.feed); err != nil {
		t.Fatal(err)
	}
	resp := collector.response()
	choices := resp["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)
	if message["content"] != "hello" || resp["id"] != "abc" {
		t.Fatalf("unexpected response %#v", resp)
	}
}

func TestCollectorPreservesNonStreamToolCalls(t *testing.T) {
	collector := newChatCollector("grok-4.5")
	sse := bytes.NewBufferString(`data: {"id":"abc","created":123,"model":"grok-4.5","choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Edit","arguments":"{\"file_path\":"}}]}}]}

data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"/x\",\"old_string\":\"a\",\"new_string\":\"\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"total_tokens":3}}

data: [DONE]

	`)
	if err := grok.ReadSSE(sse, collector.feed); err != nil {
		t.Fatal(err)
	}
	resp := collector.response()
	choices := resp["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)
	toolCalls := message["tool_calls"].([]map[string]any)
	if len(toolCalls) != 1 {
		t.Fatalf("tool_calls = %#v", message["tool_calls"])
	}
	function := toolCalls[0]["function"].(map[string]any)
	if toolCalls[0]["id"] != "call_1" || function["name"] != "Edit" || function["arguments"] != `{"file_path":"/x","old_string":"a","new_string":""}` {
		t.Fatalf("unexpected tool call %#v", toolCalls[0])
	}
	if choices[0]["finish_reason"] != "tool_calls" {
		t.Fatalf("finish_reason = %#v", choices[0]["finish_reason"])
	}
}

func TestCollectorPreservesLegacyFunctionCall(t *testing.T) {
	collector := newChatCollector("grok-4.5")
	sse := bytes.NewBufferString(`data: {"choices":[{"delta":{"function_call":{"name":"legacy","arguments":"{\"a\":"}}}]}

data: {"choices":[{"delta":{"function_call":{"arguments":"1}"}},"finish_reason":"function_call"}]}

data: [DONE]

	`)
	if err := grok.ReadSSE(sse, collector.feed); err != nil {
		t.Fatal(err)
	}
	resp := collector.response()
	choices := resp["choices"].([]map[string]any)
	message := choices[0]["message"].(map[string]any)
	call := message["function_call"].(map[string]any)
	if call["name"] != "legacy" || call["arguments"] != `{"a":1}` {
		t.Fatalf("unexpected function_call %#v", call)
	}
}

func TestForwardChatStreamForwardsValidFrames(t *testing.T) {
	sse := bytes.NewBufferString(`data: {"id":"abc","choices":[{"delta":{"content":"he"}}]}

data: not-json

data: {"choices":[{"delta":{"content":"llo"},"finish_reason":"stop"}]}

data: [DONE]

`)
	var frames []StreamFrame
	if err := ForwardChatStream(sse, func(frame StreamFrame) error {
		frames = append(frames, frame)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(frames) != 3 {
		t.Fatalf("expected 3 frames, got %#v", frames)
	}
	if !strings.Contains(string(frames[0].Data), `"he"`) || !strings.Contains(string(frames[1].Data), `"llo"`) {
		t.Fatalf("unexpected frames %#v", frames)
	}
	if !frames[2].Done {
		t.Fatalf("expected done frame, got %#v", frames[2])
	}
}

func TestParseChatDeltaReasoning(t *testing.T) {
	delta, err := ParseChatDelta([]byte(`{"choices":[{"delta":{"content":"hi","reasoning_content":"plan"},"finish_reason":"stop"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if delta.Content != "hi" || delta.Reasoning != "plan" {
		t.Fatalf("delta = %#v", delta)
	}
}

func TestPrepareUpstreamBodyStabilizesTools(t *testing.T) {
	body := PrepareUpstreamBody(map[string]any{
		"model": "grok",
		"messages": []any{
			map[string]any{"role": "system", "content": "sys\n\n"},
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"index": 0, "id": "c1", "type": "function", "function": map[string]any{"name": "Edit", "arguments": `{"file_path": "/x", "old_string": "a", "new_string": ""}`}}}},
		},
		"tools": []any{
			map[string]any{"name": "Write", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "Edit", "input_schema": map[string]any{"type": "object"}},
		},
		"prompt_cache_key": "should-forward",
	})
	if body["prompt_cache_key"] != nil {
		t.Fatalf("prompt_cache_key should be stripped before upstream: %#v", body["prompt_cache_key"])
	}
	tools := body["tools"].([]any)
	first := tools[0].(map[string]any)["function"].(map[string]any)
	if first["name"] != "Edit" {
		t.Fatalf("tools not sorted: %#v", tools)
	}
	msgs := body["messages"].([]any)
	sys := msgs[0].(map[string]any)
	if sys["content"] != "sys\n" {
		t.Fatalf("system content = %#v", sys["content"])
	}
	assistant := msgs[1].(map[string]any)
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if _, ok := call["index"]; ok {
		t.Fatalf("index should be dropped: %#v", call)
	}
}

func TestGuardStreamAgainstEmptyDetectsEmpty(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1}}\n\ndata: [DONE]\n\n"))
	guarded, empty, err := guardStreamAgainstEmpty(body)
	if err != nil {
		t.Fatal(err)
	}
	if !empty {
		t.Fatalf("expected empty stream")
	}
	if guarded != nil {
		_ = guarded.Close()
	}
}

func TestGuardStreamAgainstEmptyReplaysModelOutput(t *testing.T) {
	body := io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"hi\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"))
	guarded, empty, err := guardStreamAgainstEmpty(body)
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatalf("expected non-empty")
	}
	defer guarded.Close()
	var got strings.Builder
	err = grok.ReadSSE(guarded, func(event grok.Event) error {
		if event.Done {
			return nil
		}
		got.Write(event.Data)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got.String(), "hi") {
		t.Fatalf("replay missing content: %q", got.String())
	}
}

func TestParseChatDeltaPreservesSpaces(t *testing.T) {
	delta, err := ParseChatDelta([]byte(`{"choices":[{"delta":{"reasoning_content":"The user said: hello","content":"a b"}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if delta.Reasoning != "The user said: hello" {
		t.Fatalf("reasoning=%q", delta.Reasoning)
	}
	if delta.Content != "a b" {
		t.Fatalf("content=%q", delta.Content)
	}
}

func TestPrepareChainCapsFailover(t *testing.T) {
	candidates := make([]pool.Candidate, 0, 20)
	for i := 0; i < 20; i++ {
		candidates = append(candidates, pool.Candidate{ID: "a" + strconv.Itoa(i), Token: "t", Enabled: true, RequestCount: int64(i)})
	}
	_, chain, _, err := (&ChatService{Now: func() time.Time { return time.Unix(1000, 0) }}).prepareChain(
		t.Context(),
		ChatRequest{Model: "grok", Raw: map[string]any{"model": "grok"}},
		candidates,
		"least_used",
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(chain) != defaultFailoverChain {
		t.Fatalf("chain len=%d want %d", len(chain), defaultFailoverChain)
	}
}

func TestGuardStreamAgainstEmptySlowFirstTokenPasses(t *testing.T) {
	// Slow TTFT: no frames within the 1s peek window must NOT be treated as empty.
	pr, pw := io.Pipe()
	go func() {
		time.Sleep(1200 * time.Millisecond)
		_, _ = pw.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\ndata: [DONE]\n\n"))
		_ = pw.Close()
	}()
	guarded, empty, err := guardStreamAgainstEmpty(pr)
	if err != nil {
		t.Fatal(err)
	}
	if empty {
		t.Fatalf("slow first token must not be treated as empty")
	}
	if guarded == nil {
		t.Fatal("expected reader")
	}
	_ = guarded.Close()
}
