package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/models"
	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/protocol/anthropic"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

type ChatService struct {
	Catalog       *models.Catalog
	Client        *grok.Client
	Now           func() time.Time
	PickObserver  PickObserver
	AffinityStore AffinityStore
}

type PickObserver interface {
	LoadPenalty(context.Context, string) int64
	MarkPick(context.Context, string)
	ReleasePick(context.Context, string)
}

// optional batching extension for hot-path candidate windows
type batchPickObserver interface {
	LoadPenalties(context.Context, []string) map[string]int64
}

type AffinityStore interface {
	GetAffinity(context.Context, string) (string, error)
	BindAffinity(context.Context, string, string) error
}

type ChatRequest struct {
	Model  string         `json:"model"`
	Stream bool           `json:"stream"`
	Raw    map[string]any `json:"-"`
}

type StreamFrame struct {
	Data []byte
	Done bool
}

type ChatDelta struct {
	ID           string
	Model        string
	Created      int64
	Content      string
	Reasoning    string
	ToolCalls    []map[string]any
	FunctionCall map[string]any
	FinishReason any
	Usage        any
}

type ChatResult struct {
	Payload       map[string]any
	AccountID     string
	Model         string
	Usage         any
	PreferAccount string
	FirstAccount  string
	Failover      bool
	Fingerprint   string
	Accounts      int
	Prep          BodyPrepStats
}

type StreamOpen struct {
	Body          io.ReadCloser
	AccountID     string
	Model         string
	PreferAccount string
	FirstAccount  string
	Failover      bool
	Fingerprint   string
	Accounts      int
	Prep          BodyPrepStats
}

type StreamStats struct {
	Usage        any
	FirstTokenMS int // 0 if never observed
}

func DecodeChatRequest(reader io.Reader) (ChatRequest, error) {
	var raw map[string]any
	decoder := json.NewDecoder(reader)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		return ChatRequest{}, err
	}
	model, _ := raw["model"].(string)
	stream, _ := raw["stream"].(bool)
	return ChatRequest{Model: model, Stream: stream, Raw: raw}, nil
}

func (s *ChatService) Complete(ctx context.Context, request ChatRequest, candidates []pool.Candidate, mode string) (map[string]any, error) {
	result, err := s.CompleteWithResult(ctx, request, candidates, mode)
	if err != nil {
		return nil, err
	}
	return result.Payload, nil
}

func (s *ChatService) CompleteWithResult(ctx context.Context, request ChatRequest, candidates []pool.Candidate, mode string) (ChatResult, error) {
	if request.Stream {
		return ChatResult{}, fmt.Errorf("Go chat streaming requires ChatService.Stream")
	}
	model, chain, client, err := s.prepareChain(ctx, request, candidates, mode)
	if err != nil {
		return ChatResult{}, err
	}
	accounts := upstreamAccounts(chain)
	body, prep := PrepareUpstreamBodyDetailed(request.Raw)
	fingerprint := ChatFingerprint(request)
	prefer := ""
	if s.AffinityStore != nil && fingerprint != "" {
		prefer, _ = s.AffinityStore.GetAffinity(ctxOrBackground(ctx), fingerprint)
	}
	first := ""
	if len(accounts) > 0 {
		first = accounts[0].ID
	}
	var lastEmpty error
	for i, account := range accounts {
		s.markAttempt(ctx, account.ID)
		attempt, err := OpenWithFailover(ctx, client, []grok.Account{account}, model, body, &CommitState{})
		if err != nil {
			// Retryable/non-retryable both continue within short chain until exhausted.
			if i == len(accounts)-1 {
				s.releaseChain(ctx, chain)
				if lastEmpty != nil {
					return ChatResult{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep}, lastEmpty
				}
				return ChatResult{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep}, err
			}
			continue
		}
		collector := newChatCollector(model)
		readErr := grok.ReadSSE(attempt.Body, collector.feed)
		_ = attempt.Body.Close()
		if readErr != nil {
			s.releaseChain(ctx, chain)
			return ChatResult{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep, AccountID: attempt.Account.ID}, readErr
		}
		if !collector.emptyModelOutput() {
			s.releaseChainExcept(ctx, chain, attempt.Account.ID)
			s.bindAffinity(ctx, request, attempt.Account.ID)
			return ChatResult{
				Payload: collector.response(), AccountID: attempt.Account.ID, Model: collector.model, Usage: collector.usage,
				PreferAccount: prefer, FirstAccount: first, Failover: first != "" && attempt.Account.ID != first,
				Fingerprint: fingerprint, Accounts: len(chain), Prep: prep,
			}, nil
		}
		lastEmpty = &grok.UpstreamError{Status: 502, Body: "Upstream returned HTTP 200 with empty model output (no content/tool_calls)"}
	}
	s.releaseChain(ctx, chain)
	if lastEmpty == nil {
		lastEmpty = &grok.UpstreamError{Status: 502, Body: "Upstream returned HTTP 200 with empty model output (no content/tool_calls)"}
	}
	return ChatResult{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep, Failover: true}, lastEmpty
}

func (s *ChatService) Stream(ctx context.Context, request ChatRequest, candidates []pool.Candidate, mode string, emit func(StreamFrame) error) error {
	body, err := s.OpenStream(ctx, request, candidates, mode)
	if err != nil {
		return err
	}
	defer body.Close()
	return ForwardChatStream(body, emit)
}

func (s *ChatService) OpenStream(ctx context.Context, request ChatRequest, candidates []pool.Candidate, mode string) (io.ReadCloser, error) {
	opened, err := s.OpenStreamWithResult(ctx, request, candidates, mode)
	if err != nil {
		return nil, err
	}
	return opened.Body, nil
}

func (s *ChatService) OpenStreamWithResult(ctx context.Context, request ChatRequest, candidates []pool.Candidate, mode string) (StreamOpen, error) {
	model, chain, client, err := s.prepareChain(ctx, request, candidates, mode)
	if err != nil {
		return StreamOpen{}, err
	}
	accounts := upstreamAccounts(chain)
	body, prep := PrepareUpstreamBodyDetailed(request.Raw)
	fingerprint := ChatFingerprint(request)
	prefer := ""
	if s.AffinityStore != nil && fingerprint != "" {
		prefer, _ = s.AffinityStore.GetAffinity(ctxOrBackground(ctx), fingerprint)
	}
	first := ""
	if len(accounts) > 0 {
		first = accounts[0].ID
	}
	var lastEmpty error
	for i, account := range accounts {
		s.markAttempt(ctx, account.ID)
		attempt, err := OpenWithFailover(ctx, client, []grok.Account{account}, model, body, &CommitState{})
		if err != nil {
			if i == len(accounts)-1 {
				s.releaseChain(ctx, chain)
				if lastEmpty != nil {
					return StreamOpen{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep}, lastEmpty
				}
				return StreamOpen{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep}, err
			}
			continue
		}
		guarded, empty, err := guardStreamAgainstEmpty(attempt.Body)
		if err != nil {
			_ = attempt.Body.Close()
			s.releaseChain(ctx, chain)
			return StreamOpen{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep, AccountID: attempt.Account.ID}, err
		}
		if empty {
			_ = guarded.Close()
			lastEmpty = &grok.UpstreamError{Status: 502, Body: "Upstream returned HTTP 200 with empty model output (no content/tool_calls)"}
			continue
		}
		s.releaseChainExcept(ctx, chain, attempt.Account.ID)
		s.bindAffinity(ctx, request, attempt.Account.ID)
		return StreamOpen{
			Body: guarded, AccountID: attempt.Account.ID, Model: model,
			PreferAccount: prefer, FirstAccount: first, Failover: first != "" && attempt.Account.ID != first,
			Fingerprint: fingerprint, Accounts: len(chain), Prep: prep,
		}, nil
	}
	s.releaseChain(ctx, chain)
	if lastEmpty == nil {
		lastEmpty = &grok.UpstreamError{Status: 502, Body: "Upstream returned HTTP 200 with empty model output (no content/tool_calls)"}
	}
	return StreamOpen{PreferAccount: prefer, FirstAccount: first, Fingerprint: fingerprint, Accounts: len(chain), Prep: prep, Failover: true}, lastEmpty
}

func ForwardChatStream(reader io.Reader, emit func(StreamFrame) error) error {
	_, err := ForwardChatStreamWithStats(reader, emit)
	return err
}

func ForwardChatStreamWithStats(reader io.Reader, emit func(StreamFrame) error) (StreamStats, error) {
	if emit == nil {
		return StreamStats{}, fmt.Errorf("stream emitter is required")
	}
	var stats StreamStats
	started := time.Now()
	err := grok.ReadSSE(reader, func(event grok.Event) error {
		if event.Done {
			return emit(StreamFrame{Done: true})
		}
		if stats.FirstTokenMS == 0 && len(event.Data) > 0 {
			stats.FirstTokenMS = int(time.Since(started).Milliseconds())
			if stats.FirstTokenMS <= 0 {
				stats.FirstTokenMS = 1
			}
		}
		delta, err := ParseChatDelta(event.Data)
		if err != nil {
			return nil
		}
		if delta.Usage != nil {
			stats.Usage = delta.Usage
		}
		return emit(StreamFrame{Data: append([]byte(nil), event.Data...)})
	})
	return stats, err
}

func (s *ChatService) prepare(ctx context.Context, request ChatRequest, candidates []pool.Candidate, mode string) (string, pool.Candidate, *grok.Client, error) {
	model := s.resolveModel(request)
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	candidates = append([]pool.Candidate(nil), candidates...)
	fingerprint := ChatFingerprint(request)
	if s.AffinityStore != nil && fingerprint != "" {
		preferAffinity(ctxOrBackground(ctx), candidates, s.AffinityStore, fingerprint)
	}
	if s.PickObserver != nil {
		adjustCandidatesForObserver(ctxOrBackground(ctx), candidates, s.PickObserver)
	}
	picked, err := pool.Pick(candidates, model, mode, now)
	if err != nil {
		return "", pool.Candidate{}, nil, err
	}
	s.bindAffinity(ctx, request, picked.ID)
	if s.PickObserver != nil {
		s.PickObserver.MarkPick(ctxOrBackground(ctx), picked.ID)
	}
	client := s.Client
	if client == nil {
		client = &grok.Client{}
	}
	return model, picked, client, nil
}

// defaultFailoverChain matches Python MAX_FAILOVER_ATTEMPTS (short sticky-friendly chain).
const defaultFailoverChain = 4

func (s *ChatService) prepareChain(ctx context.Context, request ChatRequest, candidates []pool.Candidate, mode string) (string, []pool.Candidate, *grok.Client, error) {
	model := s.resolveModel(request)
	now := time.Now()
	if s.Now != nil {
		now = s.Now()
	}
	candidates = append([]pool.Candidate(nil), candidates...)
	fingerprint := ChatFingerprint(request)
	if s.AffinityStore != nil && fingerprint != "" {
		preferAffinity(ctxOrBackground(ctx), candidates, s.AffinityStore, fingerprint)
	}
	if s.PickObserver != nil {
		adjustCandidatesForObserver(ctxOrBackground(ctx), candidates, s.PickObserver)
	}
	// Never build a full-pool chain: Python caps failover attempts (default 4).
	chain := pool.Chain(candidates, model, mode, now, defaultFailoverChain)
	if len(chain) == 0 {
		return "", nil, nil, pool.ErrNoEligibleAccounts
	}
	// Account picks are marked when an attempt actually starts.
	client := s.Client
	if client == nil {
		client = &grok.Client{}
	}
	return model, chain, client, nil
}

type chatCollector struct {
	id           string
	model        string
	content      string
	reasoning    string
	toolCalls    []map[string]any
	functionCall map[string]any
	finishReason any
	usage        any
	created      int64
}

func newChatCollector(model string) *chatCollector {
	return &chatCollector{model: model, created: time.Now().Unix()}
}

func (c *chatCollector) feed(event grok.Event) error {
	if event.Done {
		return nil
	}
	delta, err := parseChatDelta(event.Data)
	if err != nil {
		return nil
	}
	if delta.ID != "" && c.id == "" {
		c.id = delta.ID
	}
	if delta.Model != "" {
		c.model = delta.Model
	}
	if delta.Created > 0 {
		c.created = delta.Created
	}
	if delta.Usage != nil {
		c.usage = delta.Usage
	}
	if delta.FinishReason != nil {
		c.finishReason = delta.FinishReason
	}
	if len(delta.ToolCalls) > 0 {
		c.mergeToolCalls(delta.ToolCalls)
	}
	if delta.FunctionCall != nil {
		c.mergeFunctionCall(delta.FunctionCall)
	}
	c.content += delta.Content
	c.reasoning += delta.Reasoning
	return nil
}

func (c *chatCollector) response() map[string]any {
	id := c.id
	if id == "" {
		id = fmt.Sprintf("chatcmpl-go-%d", c.created)
	}
	finish := c.finishReason
	if finish == nil {
		finish = "stop"
	}
	usage := c.usage
	if usage == nil {
		usage = map[string]any{"prompt_tokens": 0, "completion_tokens": 0, "total_tokens": 0}
	}
	message := map[string]any{
		"role":    "assistant",
		"content": c.content,
	}
	if c.reasoning != "" {
		message["reasoning_content"] = c.reasoning
	}
	if len(c.toolCalls) > 0 {
		message["tool_calls"] = c.toolCalls
	}
	if c.functionCall != nil {
		message["function_call"] = c.functionCall
	}
	return map[string]any{
		"id":      id,
		"object":  "chat.completion",
		"created": c.created,
		"model":   c.model,
		"choices": []map[string]any{{
			"index":         0,
			"message":       message,
			"finish_reason": finish,
		}},
		"usage": usage,
	}
}

func (c *chatCollector) mergeToolCalls(calls []map[string]any) {
	for _, incoming := range calls {
		idx := len(c.toolCalls)
		if rawIndex, ok := numberToInt64(incoming["index"]); ok && rawIndex >= 0 {
			idx = int(rawIndex)
		}
		for len(c.toolCalls) <= idx {
			c.toolCalls = append(c.toolCalls, map[string]any{"index": len(c.toolCalls)})
		}
		mergeToolCall(c.toolCalls[idx], incoming)
	}
}

func mergeToolCall(dst, src map[string]any) {
	for key, value := range src {
		if key == "function" {
			incoming, _ := value.(map[string]any)
			if incoming == nil {
				continue
			}
			existing, _ := dst["function"].(map[string]any)
			if existing == nil {
				existing = map[string]any{}
				dst["function"] = existing
			}
			mergeStringFields(existing, incoming)
			continue
		}
		if text, ok := value.(string); ok {
			if key == "id" || key == "type" {
				if text != "" {
					dst[key] = text
				}
			} else {
				dst[key] = stringValueAny(dst[key]) + text
			}
			continue
		}
		dst[key] = value
	}
}

func (c *chatCollector) mergeFunctionCall(call map[string]any) {
	if c.functionCall == nil {
		c.functionCall = map[string]any{}
	}
	mergeStringFields(c.functionCall, call)
}

func mergeStringFields(dst, src map[string]any) {
	for key, value := range src {
		if text, ok := value.(string); ok {
			if key == "name" {
				if text != "" {
					dst[key] = text
				}
			} else {
				dst[key] = stringValueAny(dst[key]) + text
			}
			continue
		}
		dst[key] = value
	}
}

func ParseChatDelta(data []byte) (ChatDelta, error) {
	return parseChatDelta(data)
}

func (d ChatDelta) AnthropicToolDeltas() []anthropic.ToolDelta {
	out := make([]anthropic.ToolDelta, 0, len(d.ToolCalls))
	for _, call := range d.ToolCalls {
		index := 0
		if rawIndex, ok := numberToInt64(call["index"]); ok && rawIndex >= 0 {
			index = int(rawIndex)
		}
		fn, _ := call["function"].(map[string]any)
		out = append(out, anthropic.ToolDelta{
			Index:     index,
			ID:        stringValueAny(call["id"]),
			Name:      stringValueAny(fn["name"]),
			Arguments: stringValueAny(fn["arguments"]),
		})
	}
	if d.FunctionCall != nil {
		out = append(out, anthropic.ToolDelta{Index: len(out), Name: stringValueAny(d.FunctionCall["name"]), Arguments: stringValueAny(d.FunctionCall["arguments"])})
	}
	return out
}

func parseChatDelta(data []byte) (ChatDelta, error) {
	var payload map[string]any
	if err := json.Unmarshal(data, &payload); err != nil {
		return ChatDelta{}, err
	}
	delta := ChatDelta{}
	delta.ID, _ = payload["id"].(string)
	delta.Model, _ = payload["model"].(string)
	if created, ok := numberToInt64(payload["created"]); ok {
		delta.Created = created
	}
	delta.Usage = payload["usage"]
	choices, _ := payload["choices"].([]any)
	for _, item := range choices {
		choice, _ := item.(map[string]any)
		if reason := choice["finish_reason"]; reason != nil {
			delta.FinishReason = reason
		}
		if itemDelta, _ := choice["delta"].(map[string]any); itemDelta != nil {
			if text, _ := itemDelta["content"].(string); text != "" {
				delta.Content += text
			}
			if text := rawString(itemDelta["reasoning_content"]); text != "" {
				delta.Reasoning += text
			} else if text := rawString(itemDelta["reasoning"]); text != "" {
				delta.Reasoning += text
			}
			if calls := toolCallsFromAny(itemDelta["tool_calls"]); len(calls) > 0 {
				delta.ToolCalls = append(delta.ToolCalls, calls...)
			}
			if call, _ := itemDelta["function_call"].(map[string]any); call != nil {
				delta.FunctionCall = call
			}
		}
		if message, _ := choice["message"].(map[string]any); message != nil {
			if text, _ := message["content"].(string); text != "" {
				delta.Content += text
			}
			if text := rawString(message["reasoning_content"]); text != "" {
				delta.Reasoning += text
			} else if text := rawString(message["reasoning"]); text != "" {
				delta.Reasoning += text
			}
			if calls := toolCallsFromAny(message["tool_calls"]); len(calls) > 0 {
				delta.ToolCalls = append(delta.ToolCalls, calls...)
			}
			if call, _ := message["function_call"].(map[string]any); call != nil {
				delta.FunctionCall = call
			}
		}
	}
	return delta, nil
}

func toolCallsFromAny(value any) []map[string]any {
	items, ok := value.([]any)
	if !ok {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		call, ok := item.(map[string]any)
		if !ok {
			continue
		}
		out = append(out, call)
	}
	return out
}

func stringValueAny(value any) string {
	text, _ := value.(string)
	return text
}

func rawString(value any) string {
	switch v := value.(type) {
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return ""
	}
}

func (s *ChatService) resolveModel(request ChatRequest) string {
	model := request.Model
	if s.Catalog != nil {
		model = s.Catalog.Resolve(model)
	}
	if model == "" {
		model = "grok-4.5"
	}
	return model
}

func (s *ChatService) releasePick(ctx context.Context, accountID string) {
	if s.PickObserver == nil || accountID == "" {
		return
	}
	s.PickObserver.ReleasePick(ctxOrBackground(ctx), accountID)
}

func (s *ChatService) releaseChain(ctx context.Context, chain []pool.Candidate) {
	if s.PickObserver == nil {
		return
	}
	for _, candidate := range chain {
		s.releasePick(ctx, candidate.ID)
	}
}

func (s *ChatService) releaseChainExcept(ctx context.Context, chain []pool.Candidate, keepID string) {
	if s.PickObserver == nil {
		return
	}
	for _, candidate := range chain {
		if candidate.ID == keepID {
			continue
		}
		s.releasePick(ctx, candidate.ID)
	}
}

func (s *ChatService) bindAffinity(ctx context.Context, request ChatRequest, accountID string) {
	if s.AffinityStore == nil || accountID == "" {
		return
	}
	fingerprint := ChatFingerprint(request)
	if fingerprint == "" {
		return
	}
	_ = s.AffinityStore.BindAffinity(ctxOrBackground(ctx), fingerprint, accountID)
}

func upstreamAccounts(chain []pool.Candidate) []grok.Account {
	accounts := make([]grok.Account, 0, len(chain))
	for _, candidate := range chain {
		accounts = append(accounts, candidate.UpstreamAccount())
	}
	return accounts
}

func ChatFingerprint(request ChatRequest) string {
	if request.Raw == nil {
		return ""
	}
	// Explicit sticky keys first (Codex / OpenAI Responses multi-turn).
	for _, key := range []string{"prompt_cache_key", "conversation_id", "conversation", "thread_id", "session_id"} {
		if value, _ := request.Raw[key].(string); strings.TrimSpace(value) != "" {
			return "chat:" + strings.TrimSpace(request.Model) + ":" + key + ":" + strings.TrimSpace(value)
		}
	}
	// Nested metadata (Anthropic / some relays).
	if meta, _ := request.Raw["metadata"].(map[string]any); meta != nil {
		for _, key := range []string{"prompt_cache_key", "session_id", "sessionId", "thread_id", "conversation_id", "user_id"} {
			if value, _ := meta[key].(string); strings.TrimSpace(value) != "" {
				return "chat:" + strings.TrimSpace(request.Model) + ":meta:" + key + ":" + strings.TrimSpace(value)
			}
		}
	}
	messages, ok := request.Raw["messages"].([]any)
	if !ok || len(messages) == 0 {
		return ""
	}
	encoded, err := json.Marshal(messages)
	if err != nil || len(encoded) == 0 {
		return ""
	}
	sum := sha256.Sum256(encoded)
	return "chat:" + strings.TrimSpace(request.Model) + ":messages:" + hex.EncodeToString(sum[:16])
}

// ChatFingerprintFromHeaders picks sticky keys from common client/proxy headers
// (Codex X-Grok-Conv-Id, session/thread headers).
func ChatFingerprintFromHeaders(headers http.Header, model string) string {
	if headers == nil {
		return ""
	}
	get := func(names ...string) string {
		for _, name := range names {
			if v := strings.TrimSpace(headers.Get(name)); v != "" {
				return v
			}
		}
		return ""
	}
	if v := get("X-Grok-Conv-Id", "x-grok-conv-id", "X-Grok2API-Conv-Id"); v != "" {
		return "chat:" + strings.TrimSpace(model) + ":conv:" + v
	}
	if v := get("X-Session-Id", "x-session-id", "Session-Id"); v != "" {
		return "chat:" + strings.TrimSpace(model) + ":session:" + v
	}
	if v := get("X-Thread-Id", "x-thread-id", "Thread-Id"); v != "" {
		return "chat:" + strings.TrimSpace(model) + ":thread:" + v
	}
	if v := get("X-Prompt-Cache-Key", "x-prompt-cache-key"); v != "" {
		return "chat:" + strings.TrimSpace(model) + ":prompt_cache_key:" + v
	}
	return ""
}

func preferAffinity(ctx context.Context, candidates []pool.Candidate, store AffinityStore, fingerprint string) {
	accountID, err := store.GetAffinity(ctx, fingerprint)
	if err != nil || accountID == "" {
		return
	}
	for i := range candidates {
		if candidates[i].ID == accountID {
			candidates[i].RequestCount -= 1_000_000
			return
		}
	}
}

func adjustCandidatesForObserver(ctx context.Context, candidates []pool.Candidate, observer PickObserver) {
	if observer == nil || len(candidates) == 0 {
		return
	}
	if batch, ok := observer.(batchPickObserver); ok {
		ids := make([]string, 0, len(candidates))
		for _, c := range candidates {
			if id := strings.TrimSpace(c.ID); id != "" {
				ids = append(ids, id)
			}
		}
		penalties := batch.LoadPenalties(ctx, ids)
		for i := range candidates {
			if p := penalties[candidates[i].ID]; p > 0 {
				candidates[i].RequestCount += p
			}
		}
		return
	}
	for i := range candidates {
		penalty := observer.LoadPenalty(ctx, candidates[i].ID)
		if penalty <= 0 {
			continue
		}
		candidates[i].RequestCount += penalty
	}
}

func ctxOrBackground(ctx context.Context) context.Context {
	if ctx == nil {
		return context.Background()
	}
	return ctx
}

func numberToInt64(value any) (int64, bool) {
	switch v := value.(type) {
	case float64:
		return int64(v), true
	case json.Number:
		n, err := v.Int64()
		return n, err == nil
	case int64:
		return v, true
	case int:
		return int64(v), true
	default:
		return 0, false
	}
}

// guardStreamAgainstEmpty peeks upstream SSE until the first model payload or
// stream end. Empty HTTP 200 bodies can then failover before the client envelope
// is opened. On success, returns a reader that replays peeked frames + remainder.
//
// 为了支持首字延迟较长的模型（如某些 newapi 实例），这里使用超时机制：
// - 如果 1 秒内收到有效内容 → 正常返回
// - 如果 1 秒内收到 [DONE] → 判定为空
// - 如果 1 秒内没有收到任何数据 → 放弃检测，直接通过（避免误杀慢启动模型）
func guardStreamAgainstEmpty(body io.ReadCloser) (io.ReadCloser, bool, error) {
	if body == nil {
		return nil, true, &grok.UpstreamError{Status: 502, Body: "Upstream returned HTTP 200 with empty model output (no content/tool_calls)"}
	}

	type peekResult struct {
		sawModel bool
		sawDone  bool
		buffered string
		err      error
	}

	resultCh := make(chan peekResult, 1)
	go func() {
		var buffered strings.Builder
		sawModel := false
		sawDone := false
		err := grok.ReadSSE(io.TeeReader(body, &buffered), func(event grok.Event) error {
			if event.Done {
				sawDone = true
				return errStopPeek
			}
			delta, parseErr := parseChatDelta(event.Data)
			if parseErr != nil {
				return nil
			}
			if strings.TrimSpace(delta.Content) != "" || strings.TrimSpace(delta.Reasoning) != "" || len(delta.ToolCalls) > 0 || delta.FunctionCall != nil {
				sawModel = true
				return errStopPeek
			}
			return nil
		})
		if err != nil && !errors.Is(err, errStopPeek) {
			resultCh <- peekResult{err: err}
			return
		}
		resultCh <- peekResult{sawModel: sawModel, sawDone: sawDone, buffered: buffered.String()}
	}()

	// Ultra-short empty-stream peek (80ms): only catch instant empty/[DONE], never wait for slow first tokens.
	select {
	case result := <-resultCh:
		if result.err != nil {
			_ = body.Close()
			return nil, false, result.err
		}
		if !result.sawModel && result.sawDone {
			// 确实是空响应（收到 [DONE] 但没有内容）
			_, _ = io.Copy(io.Discard, body)
			_ = body.Close()
			return io.NopCloser(strings.NewReader(result.buffered)), true, nil
		}
		// 有内容或者流还在继续 → 正常返回
		replayed := io.MultiReader(strings.NewReader(result.buffered), body)
		return &multiClose{Reader: replayed, closer: body}, false, nil
	case <-time.After(80 * time.Millisecond):
		// Peek deadline elapsed: pass through without waiting for full first token.
		// 注意：这里不关闭 body，让它继续流式输出
		return body, false, nil
	}
}

var errStopPeek = errors.New("stop peek")

type multiClose struct {
	io.Reader
	closer io.Closer
}

func (m *multiClose) Close() error {
	if m.closer == nil {
		return nil
	}
	return m.closer.Close()
}

func (s *ChatService) markAttempt(ctx context.Context, accountID string) {
	if s == nil || s.PickObserver == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	s.PickObserver.MarkPick(ctxOrBackground(ctx), accountID)
}

func remainingAccounts(accounts []grok.Account, usedID string) []grok.Account {
	out := make([]grok.Account, 0, len(accounts))
	for _, account := range accounts {
		if account.ID == usedID {
			continue
		}
		out = append(out, account)
	}
	return out
}

func (c *chatCollector) emptyModelOutput() bool {
	if len(c.toolCalls) > 0 || c.functionCall != nil {
		return false
	}
	if strings.TrimSpace(c.content) != "" {
		return false
	}
	if strings.TrimSpace(c.reasoning) != "" {
		return false
	}
	return true
}
