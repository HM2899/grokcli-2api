package responses

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

type ToolDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type liveTool struct {
	id          string
	name        string
	arguments   string
	clientInput string
	itemID      string
	output      int
	emitted     bool
	// clientAcked is true only after the full function_call group was written.
	// Soft write failures leave emitted=true but unacked so RequeueUnackedTools
	// can re-emit a complete added+delta+done cluster ("Tool use interrupted").
	clientAcked bool
}

// LiveStreamer emits a valid Responses envelope with monotonic sequence
// numbers. Complete deliberately remains open when no client payload exists so
// callers can still emit response.failed for an empty upstream HTTP 200.
type LiveStreamer struct {
	responseID      string
	model           string
	allowed         []string
	maxTools        int
	toolsStarted    int
	sequence        Sequence
	started         bool
	closed          bool
	textOpen        bool
	reasoningOpen   bool
	messageID       string
	reasoningID     string
	text            string
	reasoning       string
	output          int
	textOut         int // output_index of the open text message item (-1 if none)
	tools           map[int]*liveTool
	shellArgKeys    map[string]string
	customToolNames map[string]bool
	// pendingClientAcks: tool indexes framed but not yet Ack'd as written.
	pendingClientAcks []int
	// pendingTerminal: Complete frames produced but not yet AckTerminal'd.
	pendingTerminal bool
	// terminalEmitted: Complete was successfully written (AckTerminal).
	terminalEmitted bool
	// contentDelivered: text/reasoning frames survived a successful Write+Flush.
	// Distinct from text/reasoning non-empty (produced) — soft-fail before Ack
	// must not count as client-visible for TTFT / soft-close decisions.
	contentDelivered bool
	// deliveredRunes: client-visible output runes successfully written (text,
	// reasoning, tool name/args). Used for usage estimation when upstream omits
	// the final usage frame — more accurate than open-block flags alone.
	deliveredRunes int
}

func NewLiveStreamer(responseID, model string, allowed []string) *LiveStreamer {
	return NewLiveStreamerWithMaxTools(responseID, model, allowed, 0)
}

// NewLiveStreamerWithMaxTools caps outbound function_call items per turn.
// maxTools <= 0 means unlimited (Codex / OpenAI-native).
func NewLiveStreamerWithMaxTools(responseID, model string, allowed []string, maxTools int) *LiveStreamer {
	return &LiveStreamer{
		responseID:      responseID,
		model:           model,
		allowed:         append([]string(nil), allowed...),
		maxTools:        maxTools,
		messageID:       "msg_" + responseID,
		reasoningID:     "rs_" + responseID,
		textOut:         -1,
		tools:           make(map[int]*liveTool),
		shellArgKeys:    map[string]string{},
		customToolNames: map[string]bool{},
	}
}

// SetShellArgKeys configures client-facing shell parameter names (Codex uses "cmd").
func (s *LiveStreamer) SetShellArgKeys(keys map[string]string) {
	if s == nil {
		return
	}
	if keys == nil {
		s.shellArgKeys = map[string]string{}
		return
	}
	s.shellArgKeys = keys
}

// SetCustomToolNames configures Responses API free-form tools. Internally the
// upstream sees ordinary function tools with an {input:string} schema; this map
// restores custom_tool_call/input on the client-facing boundary.
func (s *LiveStreamer) SetCustomToolNames(names map[string]bool) {
	if s == nil {
		return
	}
	s.customToolNames = map[string]bool{}
	for name, custom := range names {
		if custom {
			s.customToolNames[name] = true
		}
	}
}

func (s *LiveStreamer) isCustomTool(name string) bool {
	return s != nil && isCustomToolName(name, s.customToolNames)
}

func (s *LiveStreamer) projectArgs(toolName, args string) string {
	if s == nil || args == "" {
		return args
	}
	// Prefer client tool schema key when known (Codex: "cmd"; Hermes terminal: "command").
	// Fall back to DefaultShellArgKey for shell-family tools without a map entry.
	preferred := ""
	if s.shellArgKeys != nil {
		if v := strings.TrimSpace(s.shellArgKeys[toolName]); v != "" {
			preferred = v
		} else if v := strings.TrimSpace(s.shellArgKeys[strings.ToLower(toolName)]); v != "" {
			preferred = v
		} else if nk := toolcall.NameKey(toolName); nk != "" {
			if v := strings.TrimSpace(s.shellArgKeys[nk]); v != "" {
				preferred = v
			}
		}
	}
	// Hard rule: shell-family tools always project. Prefer schema map; else
	// DefaultShellArgKey (Codex "cmd", Hermes terminal "command").
	// Pure OpenAI command-only schemas are honored when keys map says "command".
	if preferred == "" {
		if toolcall.IsShellTool(toolName) {
			preferred = toolcall.DefaultShellArgKey(toolName)
		} else if looksLikeShellArgs(args) {
			// Unknown/custom tool name but payload looks like a shell call —
			// Codex is the common case that needs cmd projection.
			preferred = "cmd"
		} else {
			return args
		}
	}
	out := toolcall.ProjectShellArgsForClient(args, toolName, preferred)
	// Final safety: if we preferred cmd but output still only has command, force rewrite.
	if preferred == "cmd" && strings.Contains(out, `"command"`) && !strings.Contains(out, `"cmd"`) {
		out = toolcall.ProjectShellArgsForClient(out, "shell", "cmd")
	}
	return out
}

// looksLikeShellArgs detects shell payloads even when the tool name is unknown
// (custom names / namespaced wrappers) so we still project command→cmd.
func looksLikeShellArgs(args string) bool {
	a := strings.TrimSpace(args)
	if a == "" {
		return false
	}
	// Common shell arg keys.
	if strings.Contains(a, `"command"`) || strings.Contains(a, `"cmd"`) ||
		strings.Contains(a, `"shell_command"`) || strings.Contains(a, `"cmdline"`) {
		// Avoid treating random tools that merely mention "command" in a string value.
		// Require object-looking JSON with those keys near the start half.
		if strings.HasPrefix(a, "{") || strings.Contains(a, `{"command"`) || strings.Contains(a, `{"cmd"`) {
			return true
		}
	}
	return false
}

func (s *LiveStreamer) initial() map[string]any {
	return map[string]any{
		"id": s.responseID, "object": "response", "created_at": 0,
		"status": "in_progress", "model": s.model, "output": []any{},
		"usage": NormalizeUsage(nil),
	}
}

func (s *LiveStreamer) Start() []string {
	if s.started {
		return nil
	}
	s.started = true
	initial := s.initial()
	return []string{
		s.sequence.Event("response.created", map[string]any{"response": initial}),
		s.sequence.Event("response.in_progress", map[string]any{"response": initial}),
	}
}

func (s *LiveStreamer) Reasoning(delta string) []string {
	if s.closed || delta == "" {
		return nil
	}
	frames := s.Start()
	if !s.reasoningOpen {
		s.reasoningOpen = true
		frames = append(frames,
			s.sequence.Event("response.output_item.added", map[string]any{
				"output_index": s.output,
				"item": map[string]any{
					"id": s.reasoningID, "type": "reasoning", "status": "in_progress",
					"summary": []any{},
				},
			}),
			s.sequence.Event("response.reasoning_summary_part.added", map[string]any{
				"item_id": s.reasoningID, "output_index": s.output, "summary_index": 0,
				"part": map[string]any{"type": "summary_text", "text": ""},
			}),
		)
	}
	s.reasoning += delta
	frames = append(frames, s.sequence.Event("response.reasoning_summary_text.delta", map[string]any{
		"item_id": s.reasoningID, "output_index": s.output, "summary_index": 0,
		"delta": delta,
	}))
	return frames
}

func (s *LiveStreamer) Text(delta string) []string {
	if s.closed || delta == "" {
		return nil
	}
	frames := s.Start()
	if s.reasoningOpen {
		frames = append(frames, s.closeReasoning()...)
	}
	if !s.textOpen {
		s.textOpen = true
		s.textOut = s.output
		frames = append(frames,
			s.sequence.Event("response.output_item.added", map[string]any{
				"output_index": s.output,
				"item": map[string]any{
					"id": s.messageID, "type": "message", "role": "assistant",
					"status": "in_progress", "content": []any{},
				},
			}),
			s.sequence.Event("response.content_part.added", map[string]any{
				"item_id": s.messageID, "output_index": s.output, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": ""},
			}),
		)
	}
	s.text += delta
	frames = append(frames, s.sequence.Event("response.output_text.delta", map[string]any{
		"item_id": s.messageID, "output_index": s.textOut, "content_index": 0,
		"delta": delta,
	}))
	return frames
}

func (s *LiveStreamer) ToolDeltas(deltas []ToolDelta) []string {
	if s.closed || len(deltas) == 0 {
		return nil
	}
	frames := s.Start()
	for _, delta := range deltas {
		state := s.tools[delta.Index]
		if state == nil {
			id := delta.ID
			if id == "" {
				id = fmt.Sprintf("call_go_%d", delta.Index)
			}
			state = &liveTool{id: id, output: -1}
			s.tools[delta.Index] = state
		}
		if delta.ID != "" && state.id == "" {
			state.id = delta.ID
		}
		if delta.Name != "" {
			state.name = mergeName(state.name, delta.Name)
			state.name = toolcall.CanonicalName(state.name, s.allowed)
		}
		if delta.Arguments != "" {
			state.arguments = toolcall.Merge(state.arguments, delta.Arguments, state.name)
		}
	}
	frames = append(frames, s.emitReadyTools(false)...)
	return frames
}

// emitReadyTools flushes complete function_call items. When force=true (stream end),
// we still only emit tools whose JSON is CompleteJSON after EffectiveJSON coercion.
// Incomplete required fields are dropped so clients never hang on a half tool, and
// the envelope still closes via response.completed.
func (s *LiveStreamer) emitReadyTools(force bool) []string {
	if s.closed {
		return nil
	}
	indexes := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	frames := make([]string, 0)
	for _, index := range indexes {
		state := s.tools[index]
		if state.emitted || state.name == "" {
			continue
		}
		if force {
			// Force-finish: recover trailing junk / mild truncation so intermittent
			// incomplete tools still emit instead of vanishing at stream end.
			// Prefer emitting a best-effort complete tool over dropping it — a drop
			// after content_block_start / function_call in_progress is what clients
			// report as "Tool use interrupted".
			state.arguments = toolcall.CoerceCompleteJSON(state.arguments, state.name)
			if !toolcall.CompleteJSON(state.arguments, state.name) {
				// Retry under common shell/edit aliases before giving up.
				retryOK := false
				for _, alt := range []string{"shell", "exec_command", "Edit", "apply_patch"} {
					if alt == state.name {
						continue
					}
					if c := toolcall.CoerceCompleteJSON(state.arguments, alt); toolcall.CompleteJSON(c, alt) {
						// Keep original name for client schema projection; args are usable.
						state.arguments = c
						retryOK = true
						break
					}
					if c := toolcall.CoerceCompleteJSON(state.arguments, alt); toolcall.CompleteJSON(c, state.name) {
						state.arguments = c
						retryOK = true
						break
					}
				}
				if !retryOK {
					// Last resort: if Coerce returned any object-looking JSON, emit it.
					// Better a slightly soft payload than a vanished tool mid-turn.
					args := strings.TrimSpace(state.arguments)
					if args == "" || (args[0] != '{' && args[0] != '[') {
						continue
					}
					var raw any
					if json.Unmarshal([]byte(args), &raw) != nil {
						continue
					}
					// Accept non-empty object/array as emit-able force-finish salvage.
					switch v := raw.(type) {
					case map[string]any:
						if len(v) == 0 {
							continue
						}
					case []any:
						if len(v) == 0 {
							continue
						}
					default:
						continue
					}
				}
			}
		} else {
			// Live path: normalize only; CompleteJSONStrict rejects truncation
			// "repairs" so unterminated fragments stay pending until real end.
			normalized := toolcall.NormalizeJSON(state.arguments, state.name)
			if normalized == "" {
				normalized = state.arguments
			}
			if !toolcall.CompleteJSONStrict(normalized, state.name) {
				continue
			}
			state.arguments = normalized
		}
		if s.maxTools > 0 && s.toolsStarted >= s.maxTools {
			break
		}
		// Close open reasoning before tools so envelope order stays valid.
		if s.reasoningOpen {
			frames = append(frames, s.closeReasoning()...)
		}
		state.emitted = true
		state.clientAcked = false
		s.toolsStarted++
		state.output = s.output
		custom := s.isCustomTool(state.name)
		if custom {
			state.itemID = fmt.Sprintf("ctc_%s_%d", s.responseID, index)
		} else {
			state.itemID = fmt.Sprintf("fc_%s_%d", s.responseID, index)
		}
		s.pendingClientAcks = append(s.pendingClientAcks, index)
		if custom {
			state.clientInput = customToolInput(state.arguments)
			if state.clientInput == "" {
				state.emitted = false
				s.toolsStarted--
				s.pendingClientAcks = s.pendingClientAcks[:len(s.pendingClientAcks)-1]
				continue
			}
			frames = append(frames,
				s.sequence.Event("response.output_item.added", map[string]any{
					"output_index": state.output,
					"item": map[string]any{
						"id": state.itemID, "type": "custom_tool_call", "status": "in_progress",
						"call_id": state.id, "name": state.name, "input": "",
					},
				}),
				s.sequence.Event("response.custom_tool_call_input.delta", map[string]any{
					"item_id": state.itemID, "output_index": state.output,
					"delta": state.clientInput,
				}),
				s.sequence.Event("response.custom_tool_call_input.done", map[string]any{
					"item_id": state.itemID, "output_index": state.output,
					"input": state.clientInput,
				}),
				s.sequence.Event("response.output_item.done", map[string]any{
					"output_index": state.output,
					"item": map[string]any{
						"id": state.itemID, "type": "custom_tool_call", "status": "completed",
						"call_id": state.id, "name": state.name, "input": state.clientInput,
					},
				}),
			)
			s.output++
			continue
		}
		// Project to the client's shell schema key (Codex: "cmd"; OpenAI: "command").
		clientArgs := s.projectArgs(state.name, state.arguments)
		state.arguments = clientArgs
		frames = append(frames,
			s.sequence.Event("response.output_item.added", map[string]any{
				"output_index": state.output,
				"item": map[string]any{
					"id": state.itemID, "type": "function_call", "status": "in_progress",
					"call_id": state.id, "name": state.name, "arguments": "",
				},
			}),
			s.sequence.Event("response.function_call_arguments.delta", map[string]any{
				"item_id": state.itemID, "output_index": state.output,
				"delta": clientArgs,
			}),
			s.sequence.Event("response.function_call_arguments.done", map[string]any{
				"item_id": state.itemID, "output_index": state.output,
				"arguments": clientArgs,
			}),
			s.sequence.Event("response.output_item.done", map[string]any{
				"output_index": state.output,
				"item": map[string]any{
					"id": state.itemID, "type": "function_call", "status": "completed",
					"call_id": state.id, "name": state.name, "arguments": clientArgs,
				},
			}),
		)
		s.output++
	}
	return frames
}

func (s *LiveStreamer) closeReasoning() []string {
	if !s.reasoningOpen {
		return nil
	}
	frames := []string{
		s.sequence.Event("response.reasoning_summary_part.done", map[string]any{
			"item_id": s.reasoningID, "output_index": s.output, "summary_index": 0,
			"part": map[string]any{"type": "summary_text", "text": s.reasoning},
		}),
		s.sequence.Event("response.output_item.done", map[string]any{
			"output_index": s.output,
			"item": map[string]any{
				"id": s.reasoningID, "type": "reasoning", "status": "completed",
				"summary": []any{map[string]any{"type": "summary_text", "text": s.reasoning}},
			},
		}),
	}
	s.reasoningOpen = false
	s.output++
	return frames
}

func (s *LiveStreamer) HasClientPayload() bool {
	// True only when the client has received user-visible output: text,
	// reasoning, or *emitted* tools. Incomplete tools that are still held
	// (name set, not emitted) are NOT payload — they are tracked by
	// HasPendingTools for SSE keepalive. After force-finish drops incomplete
	// tools, this must return false so callers Fail instead of completing empty
	// (Codex "hold-failure" / empty output hang).
	if s.text != "" || s.reasoning != "" || s.reasoningOpen || s.textOpen {
		return true
	}
	for _, state := range s.tools {
		if state != nil && state.emitted {
			return true
		}
	}
	return false
}

// toolCapReached is true when the outbound max-tools policy has already emitted
// its full budget. Remaining complete tools must be treated as dropped — not
// "pending" — otherwise Complete/NeedsFinishRetry re-emit response.completed
// in a loop (Claude Code "Tool use interrupted" on Read/Write multi-tool turns).
func (s *LiveStreamer) toolCapReached() bool {
	return s != nil && s.maxTools > 0 && s.toolsStarted >= s.maxTools
}

// HasPendingTools reports whether any tool args are buffered but not yet
// emitted (incomplete JSON). Used by the server to keep the client SSE warm
// while we hold incomplete function_call frames — otherwise proxies cut the
// stream during multi-second tool-arg drips (upstream not idle, so ReadSSE
// keepalive never fires, and the client sees silence).
//
// When the max-tools cap is already filled, excess tools are never emitted and
// do not count as pending (avoids duplicate response.completed after cap).
func (s *LiveStreamer) HasPendingTools() bool {
	if s == nil || s.toolCapReached() {
		return false
	}
	for _, state := range s.tools {
		if state != nil && !state.emitted && state.name != "" {
			return true
		}
	}
	return false
}

func hasNonStartPayload(frames []string) bool {
	for _, f := range frames {
		if strings.Contains(f, "response.output_item") ||
			strings.Contains(f, "response.function_call") ||
			strings.Contains(f, "response.output_text") ||
			strings.Contains(f, "response.reasoning") ||
			strings.Contains(f, "response.completed") ||
			strings.Contains(f, "response.failed") {
			return true
		}
	}
	return false
}

// HasUnackedTools reports tools framed but not yet client-Ack'd, or pending terminal.
// Prefer live tool.clientAcked over pendingClientAcks — the list can lag if a caller
// acked via tool state (or AckToolsInPayload partially cleared). Stale pending
// entries must not keep ClientDeliveryOK false after real acks.
func (s *LiveStreamer) HasUnackedTools() bool {
	if s == nil {
		return false
	}
	if s.pendingTerminal {
		return true
	}
	for _, state := range s.tools {
		if state != nil && state.emitted && !state.clientAcked {
			return true
		}
	}
	for _, idx := range s.pendingClientAcks {
		state := s.tools[idx]
		if state == nil {
			return true
		}
		if state.emitted && !state.clientAcked {
			return true
		}
	}
	return false
}

// TerminalDelivered is true after response.completed was Ack'd.
func (s *LiveStreamer) TerminalDelivered() bool {
	return s != nil && s.terminalEmitted
}

// PayloadDelivered is true when the client has already received useful output:
// text/reasoning that survived Write+Flush (contentDelivered), or at least one
// tool that was client-Ack'd. Unlike HasClientPayload (true as soon as frames
// are *produced*), this only counts content that landed on the wire.
//
// Used after recovery exhaustion so we soft-close a real delivery instead of
// Fail("empty model output") when only response.completed soft-failed —
// that false empty is what Claude Code surfaces as intermittent
// "Tool use interrupted" with admin ok=false tokens=0 ttft>0.
func (s *LiveStreamer) PayloadDelivered() bool {
	if s == nil {
		return false
	}
	if s.contentDelivered {
		return true
	}
	for _, state := range s.tools {
		if state != nil && state.clientAcked {
			return true
		}
	}
	return false
}

// AckContentDelivered marks text/reasoning frames as successfully written.
// Call after a Write+Flush of non-tool payload (output_text / reasoning deltas).
// Only counts when real text/reasoning characters exist — open-only envelopes
// must not inflate TTFT/usage (that produced completion_tokens=1 floors).
func (s *LiveStreamer) AckContentDelivered() {
	if s == nil {
		return
	}
	if s.text != "" || s.reasoning != "" {
		s.contentDelivered = true
		s.SyncDeliveredFromBuffers()
	}
}

// SyncDeliveredFromBuffers sets deliveredRunes to the high-water mark of
// buffered text/reasoning/emitted tools. Call after successful client writes.
func (s *LiveStreamer) SyncDeliveredFromBuffers() {
	if s == nil {
		return
	}
	n := s.bufferOutputChars()
	if n > s.deliveredRunes {
		s.deliveredRunes = n
	}
}

// bufferOutputChars counts buffered content without deliveredRunes (no recursion).
func (s *LiveStreamer) bufferOutputChars() int {
	if s == nil {
		return 0
	}
	n := len([]rune(s.text)) + len([]rune(s.reasoning))
	for _, state := range s.tools {
		if state == nil || !(state.emitted || state.clientAcked) {
			continue
		}
		n += len([]rune(state.name)) + len([]rune(state.arguments))
	}
	return n
}

// NoteToolDelivered recounts buffers after a successful function_call write.
func (s *LiveStreamer) NoteToolDelivered() {
	if s == nil {
		return
	}
	s.SyncDeliveredFromBuffers()
}

// HalfOpenTools is true when any tool was framed (emitted) but never client-Ack'd.
// That is the real "Tool use interrupted" case — not a missing terminal alone.
func (s *LiveStreamer) HalfOpenTools() bool {
	if s == nil {
		return false
	}
	for _, state := range s.tools {
		if state != nil && state.emitted && !state.clientAcked {
			return true
		}
	}
	for _, idx := range s.pendingClientAcks {
		state := s.tools[idx]
		if state == nil {
			return true
		}
		if state.emitted && !state.clientAcked {
			return true
		}
	}
	return false
}

// ClientDeliveryOK reports a fully closed Responses turn safe for ok=true:
//   - terminal (response.completed) must be Ack'd, AND
//   - either text/reasoning was framed, or at least one tool was client-Ack'd.
//
// Tools that were only framed (emitted) but never Ack'd do NOT count — that is
// the half-open function_call path Claude Code calls "Tool use interrupted".
// Pending incomplete tools that never emitted do not poison a text delivery.
func (s *LiveStreamer) ClientDeliveryOK() bool {
	if s == nil || !s.terminalEmitted {
		return false
	}
	if s.HasUnackedTools() {
		return false
	}
	if s.text != "" || s.reasoning != "" {
		return true
	}
	anyEmitted := false
	anyAcked := false
	for _, state := range s.tools {
		if state == nil {
			continue
		}
		// Only emitted/acked tools matter. name-only pending tools were never
		// framed; force-finish drop is empty, not half-open.
		if state.emitted || state.clientAcked {
			anyEmitted = true
		}
		if state.clientAcked {
			anyAcked = true
		}
	}
	if anyEmitted {
		return anyAcked
	}
	// Envelope-only / empty: not OK.
	return false
}

// OutputChars counts framed text/reasoning/tool-arg runes for usage fallback.
// Used when upstream omits the usage frame (soft-close / short tool turns) so
// admin does not record ok=true with all-zero tokens (hollow success).
// Prefers max(buffered content, successfully delivered runes).
func (s *LiveStreamer) OutputChars() int {
	if s == nil {
		return 0
	}
	n := s.bufferOutputChars()
	if s.deliveredRunes > n {
		n = s.deliveredRunes
	}
	return n
}

// EstimateOutputTokens approximates completion tokens (~4 runes/token).
// Returns at least 1 only when real client-visible characters or Ack'd payload
// exist — never floors solely on open block flags (that caused completion=1
// for hollow short streams).
func (s *LiveStreamer) EstimateOutputTokens() int {
	if s == nil {
		return 0
	}
	s.SyncDeliveredFromBuffers()
	chars := s.OutputChars()
	if chars > 0 {
		tok := (chars + 3) / 4
		if tok < 1 {
			tok = 1
		}
		return tok
	}
	// PayloadDelivered implies text/tools actually written; still floor at 1.
	if s.PayloadDelivered() {
		return 1
	}
	return 0
}

// UndeliveredTools is true when any tool was framed but never client-Ack'd.
func (s *LiveStreamer) UndeliveredTools() bool {
	if s == nil {
		return false
	}
	for _, state := range s.tools {
		if state != nil && state.emitted && !state.clientAcked {
			return true
		}
	}
	for _, idx := range s.pendingClientAcks {
		state := s.tools[idx]
		if state == nil {
			return true
		}
		if state.emitted && !state.clientAcked {
			return true
		}
	}
	return false
}

// NeedsFinishRetry is true when soft-fail recovery still has work.
// Incomplete pending tools must not loop Complete after terminal — that re-emits
// response.completed and surfaces as Claude Code "Tool use interrupted".
func (s *LiveStreamer) NeedsFinishRetry() bool {
	if s == nil {
		return false
	}
	if s.HasUnackedTools() {
		return true
	}
	if !s.terminalEmitted {
		return s.HasPendingTools() || s.HasClientPayload() || s.started
	}
	// Terminal landed: only retry ready-to-emit tools (soft-fail requeue).
	return s.hasReadyUnemittedTools()
}

// hasReadyUnemittedTools reports a non-emitted tool that force-finish can still emit.
// Respects maxTools: once the outbound cap is full, extra complete tools are dropped
// and must not keep NeedsFinishRetry / Complete looping.
func (s *LiveStreamer) hasReadyUnemittedTools() bool {
	if s == nil || s.toolCapReached() {
		return false
	}
	for _, state := range s.tools {
		if state == nil || state.emitted || state.clientAcked {
			continue
		}
		if state.name == "" {
			continue
		}
		args := toolcall.CoerceCompleteJSON(state.arguments, state.name)
		if toolcall.CompleteJSON(args, state.name) {
			return true
		}
	}
	return false
}

// AckToolsInPayload marks tools whose item id / call_id appears in a successful write.
func (s *LiveStreamer) AckToolsInPayload(payload string) {
	if s == nil || payload == "" {
		return
	}
	if !strings.Contains(payload, "function_call") {
		return
	}
	acked := make(map[int]bool)
	for index, state := range s.tools {
		if state == nil || state.clientAcked || !state.emitted {
			continue
		}
		matched := false
		if state.itemID != "" && strings.Contains(payload, state.itemID) {
			matched = true
		} else if state.id != "" && strings.Contains(payload, state.id) {
			matched = true
		}
		if !matched {
			continue
		}
		state.clientAcked = true
		acked[index] = true
	}
	if len(acked) == 0 {
		return
	}
	if len(s.pendingClientAcks) == 0 {
		return
	}
	kept := s.pendingClientAcks[:0]
	for _, idx := range s.pendingClientAcks {
		if !acked[idx] {
			kept = append(kept, idx)
		}
	}
	s.pendingClientAcks = kept
}

// AckTerminal marks a successfully written response.completed/[DONE] batch.
func (s *LiveStreamer) AckTerminal() {
	if s == nil || !s.pendingTerminal {
		return
	}
	s.terminalEmitted = true
	s.pendingTerminal = false
}

// UnackTerminal rolls back a previously Ack'd terminal so Complete can re-emit
// response.completed AFTER any requeued tools. Emitting function_call groups
// after completed/[DONE] is what Claude Code reports as "Tool use interrupted".
func (s *LiveStreamer) UnackTerminal() {
	if s == nil {
		return
	}
	s.terminalEmitted = false
	s.pendingTerminal = false
	s.closed = false
}

// AckEmittedTools marks all pending tools + terminal as written (full-batch success).
func (s *LiveStreamer) AckEmittedTools() {
	if s == nil {
		return
	}
	s.AckTerminal()
	for _, idx := range s.pendingClientAcks {
		if state := s.tools[idx]; state != nil {
			state.clientAcked = true
		}
	}
	s.pendingClientAcks = s.pendingClientAcks[:0]
	for _, state := range s.tools {
		if state != nil && state.emitted {
			state.clientAcked = true
		}
	}
}

// RequeueUnackedTools rolls back tools that were framed but never acked so
// Complete/emitReadyTools can re-emit a complete function_call group.
//
// If any tool is requeued while terminal was already Ack'd, also UnackTerminal:
// Claude Code / Codex reject function_call after response.completed ("Tool use
// interrupted"). Recovery must re-emit tools THEN completed in one turn.
func (s *LiveStreamer) RequeueUnackedTools() {
	if s == nil {
		return
	}
	if s.pendingTerminal && !s.terminalEmitted {
		s.pendingTerminal = false
		// Allow Complete to re-emit response.completed.
		s.closed = false
	}
	requeuedTool := false
	for _, state := range s.tools {
		if state == nil || state.clientAcked || !state.emitted {
			continue
		}
		// Roll back so emitReadyTools can re-frame. Keep args/name/id.
		state.emitted = false
		state.itemID = ""
		state.output = -1
		if s.toolsStarted > 0 {
			s.toolsStarted--
		}
		requeuedTool = true
	}
	s.pendingClientAcks = s.pendingClientAcks[:0]
	// Never leave tools to re-emit after a delivered completed envelope.
	if requeuedTool && s.terminalEmitted {
		s.UnackTerminal()
	}
}

func (s *LiveStreamer) Complete(usage *Usage) []string {
	// Soft write may have left unacked tools/terminal; requeue before force-finish.
	s.RequeueUnackedTools()
	// Preserve empty-stream contract used by callers: if we never opened any client
	// payload AND never started the envelope, emit nothing so Fail can still run.
	if s.closed && s.terminalEmitted && !s.HasPendingTools() && !s.HasUnackedTools() {
		return nil
	}
	// Allow Complete recovery after soft-fail: reopen if terminal never Ack'd.
	if s.closed && !s.terminalEmitted {
		s.closed = false
	}
	// Terminal already Ack'd with nothing left to re-emit: toolsOnly would skip
	// completed. RequeueUnackedTools clears terminalEmitted whenever tools need
	// re-emit, so toolsOnly is only true when tools are done and terminal landed.
	toolsOnly := s.terminalEmitted && !s.HasPendingTools()
	if !s.started && !s.HasClientPayload() {
		return nil
	}
	// Empty envelope-only start with nothing pending and no open reasoning/text:
	// leave Complete empty so Fail can surface upstream empty HTTP 200.
	if s.started && s.text == "" && s.reasoning == "" && !s.textOpen && !s.reasoningOpen && !s.HasPendingTools() && !s.HasClientPayload() {
		return nil
	}

	frames := s.Start()
	// Force-flush remaining tools (incomplete may coerce+emit, or drop).
	frames = append(frames, s.emitReadyTools(true)...)
	// Close reasoning even if tools were all held+dropped (Codex still needs terminal).
	frames = append(frames, s.closeReasoning()...)
	// True hold-failure: no text/reasoning/emitted tools after force-finish.
	// Abort completed and let server Fail (empty model output) instead of a hollow
	// response.completed with empty output (Codex hang / "running").
	if !s.HasClientPayload() && s.text == "" && s.reasoning == "" && !s.textOpen && !s.reasoningOpen {
		// If we only produced Start frames, drop them so Fail owns the terminal.
		if !hasNonStartPayload(frames) {
			return nil
		}
	}

	if s.textOpen {
		textOut := s.output
		if textOut < 0 {
			textOut = 0
		}
		// Prefer the output index of the open message item: when reasoning/tools
		// ran first, text may not be at 0. Track via messageID scan is complex;
		// use current output-1 if text was opened at a higher index. Best effort:
		// LiveStreamer opens text at s.output and does not bump until close — so
		// the open text item index is s.output (not yet advanced). When tools
		// after text bump s.output, textOpen close must use the original index.
		// We store message output as the index at open time via s.messageID only;
		// fix: record textOutputIndex.
		frames = append(frames,
			s.sequence.Event("response.output_text.done", map[string]any{
				"item_id": s.messageID, "output_index": s.textOutputIndex(), "content_index": 0,
				"text": s.text,
			}),
			s.sequence.Event("response.content_part.done", map[string]any{
				"item_id": s.messageID, "output_index": s.textOutputIndex(), "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": s.text},
			}),
			s.sequence.Event("response.output_item.done", map[string]any{
				"output_index": s.textOutputIndex(),
				"item": map[string]any{
					"id": s.messageID, "type": "message", "role": "assistant",
					"status":  "completed",
					"content": []any{map[string]any{"type": "output_text", "text": s.text}},
				},
			}),
		)
		s.textOpen = false
	}
	completed := map[string]any{
		"id": s.responseID, "object": "response", "created_at": 0,
		"status": "completed", "model": s.model, "usage": NormalizeUsage(usage),
		// Codex / OpenAI SDKs read response.output on completed. Leaving it empty
		// makes clients drop streamed function_call items (tool call "succeeds"
		// in SSE but disappears from the final response object).
		"output": s.snapshotOutput(),
	}
	if toolsOnly {
		// Tools re-emit after prior Ack'd completed; do not duplicate terminal.
		return frames
	}
	frames = append(frames,
		s.sequence.Event("response.completed", map[string]any{"response": completed}),
		"data: [DONE]\n\n",
	)
	// Mark closed for mid-stream Feed rejection; pendingTerminal allows re-emit
	// via Requeue+Complete if the write soft-fails before AckTerminal.
	s.closed = true
	s.pendingTerminal = true
	return frames
}

// snapshotOutput rebuilds the ordered output array for response.completed.
// Includes completed reasoning / message / function_call items that were emitted.
func (s *LiveStreamer) snapshotOutput() []any {
	type piece struct {
		index int
		item  map[string]any
	}
	pieces := make([]piece, 0, 4+len(s.tools))
	// Reasoning item (closed or open — Complete closes it first).
	if s.reasoning != "" {
		// reasoning always opened at some index; approximate 0 when unknown.
		// Prefer ordering by emission: tools after closeReasoning bump output.
		pieces = append(pieces, piece{index: -2, item: map[string]any{
			"id": s.reasoningID, "type": "reasoning", "status": "completed",
			"summary": []any{map[string]any{"type": "summary_text", "text": s.reasoning}},
		}})
	}
	if s.text != "" {
		pieces = append(pieces, piece{index: s.textOutputIndex(), item: map[string]any{
			"id": s.messageID, "type": "message", "role": "assistant", "status": "completed",
			"content": []any{map[string]any{"type": "output_text", "text": s.text}},
		}})
	}
	// Tools by emission index.
	indexes := make([]int, 0, len(s.tools))
	for index, state := range s.tools {
		if state == nil || !state.emitted {
			continue
		}
		indexes = append(indexes, index)
	}
	// stable order by state.output then map key
	for _, index := range indexes {
		state := s.tools[index]
		outIdx := state.output
		if outIdx < 0 {
			outIdx = 1000 + index
		}
		itemID := state.itemID
		if itemID == "" {
			if s.isCustomTool(state.name) {
				itemID = fmt.Sprintf("ctc_%s_%d", s.responseID, index)
			} else {
				itemID = fmt.Sprintf("fc_%s_%d", s.responseID, index)
			}
		}
		if s.isCustomTool(state.name) {
			input := state.clientInput
			if input == "" {
				input = customToolInput(state.arguments)
			}
			pieces = append(pieces, piece{index: outIdx, item: map[string]any{
				"id": itemID, "type": "custom_tool_call", "status": "completed",
				"call_id": state.id, "name": state.name, "input": input,
			}})
			continue
		}
		pieces = append(pieces, piece{index: outIdx, item: map[string]any{
			"id": itemID, "type": "function_call", "status": "completed",
			"call_id": state.id, "name": state.name, "arguments": state.arguments,
		}})
	}
	// Sort: reasoning first (-2), then by index.
	sort.SliceStable(pieces, func(i, j int) bool {
		return pieces[i].index < pieces[j].index
	})
	out := make([]any, 0, len(pieces))
	for _, p := range pieces {
		out = append(out, p.item)
	}
	return out
}

// textOutputIndex returns the output_index used when the text message item was opened.
// Falls back to 0 for legacy simple streams.
func (s *LiveStreamer) textOutputIndex() int {
	if s == nil {
		return 0
	}
	if s.textOut >= 0 {
		return s.textOut
	}
	return 0
}

func (s *LiveStreamer) Fail(message, errorType string) []string {
	if s.closed {
		return nil
	}
	if errorType == "" {
		errorType = "server_error"
	}
	frames := s.Start()
	failed := map[string]any{
		"id": s.responseID, "object": "response", "status": "failed", "model": s.model,
		"error": map[string]any{"type": errorType, "message": message},
	}
	frames = append(frames,
		s.sequence.Event("response.failed", map[string]any{"response": failed}),
		"data: [DONE]\n\n",
	)
	s.closed = true
	return frames
}

func mergeName(current, incoming string) string {
	if current == "" {
		return incoming
	}
	if incoming == "" || current == incoming || len(current) >= len(incoming) && current[:len(incoming)] == incoming {
		return current
	}
	if len(incoming) >= len(current) && incoming[:len(current)] == current {
		return incoming
	}
	return incoming
}
