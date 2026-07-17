package anthropic

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

type ToolDelta struct {
	Index     int
	ID        string
	Name      string
	Arguments string
}

type toolState struct {
	id        string
	name      string
	arguments string
	block     int
	started   bool
	stopped   bool
}

// StreamAssembler converts chat-completion deltas into an Anthropic event
// sequence while preserving dense block indexes and one active tool block.
type StreamAssembler struct {
	messageID string
	model     string
	allowed   []string
	maxTools  int

	started        bool
	nextBlock      int
	textBlock      int
	thinkingBlock  int
	tools          map[int]*toolState
	toolsStarted   int
	sawTool        bool
	toolsRequested bool
	held           []heldDelta
	outputRunes    int
}

type heldDelta struct {
	content   string
	reasoning string
}

func NewStreamAssembler(messageID, model string, toolsRequested bool, maxTools int, allowed []string) *StreamAssembler {
	return &StreamAssembler{
		messageID:      messageID,
		model:          model,
		allowed:        append([]string(nil), allowed...),
		maxTools:       maxTools,
		textBlock:      -1,
		thinkingBlock:  -1,
		tools:          make(map[int]*toolState),
		toolsRequested: toolsRequested,
	}
}

func (s *StreamAssembler) Start(inputTokens int) []string {
	if s.started {
		return nil
	}
	s.started = true
	return []string{event("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": s.messageID, "type": "message", "role": "assistant",
			"content": []any{}, "model": s.model,
			"stop_reason": nil, "stop_sequence": nil,
			"usage": map[string]any{
				"input_tokens": inputTokens, "output_tokens": 0,
				"cache_creation_input_tokens": 0, "cache_read_input_tokens": 0,
			},
		},
	})}
}

func (s *StreamAssembler) Feed(content, reasoning string, calls []ToolDelta) []string {
	frames := s.Start(0)
	if s.toolsRequested && !s.sawTool && (content != "" || reasoning != "") {
		s.held = append(s.held, heldDelta{content: content, reasoning: reasoning})
		s.outputRunes += len([]rune(content)) + len([]rune(reasoning))
		content, reasoning = "", ""
	} else if s.toolsRequested && s.sawTool {
		content, reasoning = "", ""
	}
	if content != "" || reasoning != "" {
		frames = append(frames, s.emitText(reasoning, content)...)
	}
	if len(calls) == 0 {
		return frames
	}
	frames = append(frames, s.closeThinking()...)
	frames = append(frames, s.closeText()...)
	for _, call := range calls {
		state := s.tools[call.Index]
		if state == nil {
			id := call.ID
			if id == "" {
				id = fmt.Sprintf("toolu_go_%d", call.Index)
			}
			state = &toolState{id: id, block: -1}
			s.tools[call.Index] = state
		}
		if state.stopped {
			continue
		}
		if state.id == "" && call.ID != "" {
			state.id = call.ID
		}
		if call.Name != "" {
			state.name = mergeName(state.name, call.Name)
			state.name = toolcall.CanonicalName(state.name, s.allowed)
		}
		if call.Arguments != "" {
			state.arguments = toolcall.Merge(state.arguments, call.Arguments, state.name)
		}
	}
	frames = append(frames, s.emitReadyTools()...)
	return frames
}

func (s *StreamAssembler) Finish(finishReason string, usage Usage) []string {
	frames := s.Start(usage.PromptTokens)
	for _, state := range s.tools {
		if state.started || state.stopped || state.name == "" {
			continue
		}
		state.arguments = toolcall.EffectiveJSON(state.arguments, state.name)
	}
	hasReady := false
	for _, state := range s.tools {
		if !state.stopped && state.name != "" && toolcall.CompleteJSON(state.arguments, state.name) {
			hasReady = true
			break
		}
	}
	if s.sawTool || hasReady {
		s.held = nil
	} else {
		for _, delta := range s.held {
			frames = append(frames, s.emitText(delta.reasoning, delta.content)...)
		}
		s.held = nil
	}
	frames = append(frames, s.closeThinking()...)
	frames = append(frames, s.closeText()...)
	frames = append(frames, s.emitReadyTools()...)
	frames = append(frames, s.closeTools()...)

	outputTokens := usage.CompletionTokens
	if outputTokens <= 0 && s.outputRunes > 0 {
		outputTokens = s.outputRunes / 4
		if outputTokens == 0 {
			outputTokens = 1
		}
	}
	frames = append(frames, event("message_delta", map[string]any{
		"type": "message_delta",
		"delta": map[string]any{
			"stop_reason": StopReason(finishReason, s.sawTool), "stop_sequence": nil,
		},
		"usage": map[string]any{
			"output_tokens":               outputTokens,
			"input_tokens":                usage.PromptTokens,
			"cache_read_input_tokens":     usage.CacheReadTokens,
			"cache_creation_input_tokens": usage.CacheCreationTokens,
		},
	}))
	frames = append(frames, event("message_stop", map[string]any{"type": "message_stop"}))
	return frames
}

func (s *StreamAssembler) emitReadyTools() []string {
	indexes := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	frames := make([]string, 0)
	for _, index := range indexes {
		state := s.tools[index]
		if state.stopped || state.started {
			continue
		}
		if state.name == "" || !toolcall.CompleteJSON(state.arguments, state.name) {
			break
		}
		if s.maxTools > 0 && s.toolsStarted >= s.maxTools {
			break
		}
		state.block = s.nextBlock
		s.nextBlock++
		state.started = true
		s.toolsStarted++
		s.sawTool = true
		s.held = nil
		frames = append(frames, event("content_block_start", map[string]any{
			"type": "content_block_start", "index": state.block,
			"content_block": map[string]any{
				"type": "tool_use", "id": state.id, "name": state.name, "input": map[string]any{},
			},
		}))
		frames = append(frames, event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": state.block,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": state.arguments},
		}))
		s.outputRunes += len([]rune(state.arguments))
		frames = append(frames, event("content_block_stop", map[string]any{
			"type": "content_block_stop", "index": state.block,
		}))
		state.stopped = true
	}
	return frames
}

func (s *StreamAssembler) emitText(reasoning, content string) []string {
	frames := make([]string, 0, 4)
	if reasoning != "" {
		frames = append(frames, s.closeTools()...)
		frames = append(frames, s.closeText()...)
		if s.thinkingBlock < 0 {
			s.thinkingBlock = s.nextBlock
			s.nextBlock++
			frames = append(frames, event("content_block_start", map[string]any{
				"type": "content_block_start", "index": s.thinkingBlock,
				"content_block": map[string]any{"type": "thinking", "thinking": ""},
			}))
		}
		frames = append(frames, event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.thinkingBlock,
			"delta": map[string]any{"type": "thinking_delta", "thinking": reasoning},
		}))
		s.outputRunes += len([]rune(reasoning))
	}
	if content != "" {
		frames = append(frames, s.closeTools()...)
		frames = append(frames, s.closeThinking()...)
		if s.textBlock < 0 {
			s.textBlock = s.nextBlock
			s.nextBlock++
			frames = append(frames, event("content_block_start", map[string]any{
				"type": "content_block_start", "index": s.textBlock,
				"content_block": map[string]any{"type": "text", "text": ""},
			}))
		}
		frames = append(frames, event("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": s.textBlock,
			"delta": map[string]any{"type": "text_delta", "text": content},
		}))
		s.outputRunes += len([]rune(content))
	}
	return frames
}

func (s *StreamAssembler) closeText() []string {
	if s.textBlock < 0 {
		return nil
	}
	index := s.textBlock
	s.textBlock = -1
	return []string{event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})}
}

func (s *StreamAssembler) closeThinking() []string {
	if s.thinkingBlock < 0 {
		return nil
	}
	index := s.thinkingBlock
	s.thinkingBlock = -1
	return []string{event("content_block_stop", map[string]any{"type": "content_block_stop", "index": index})}
}

func (s *StreamAssembler) closeTools() []string {
	frames := make([]string, 0)
	for _, state := range s.tools {
		if state.started && !state.stopped {
			frames = append(frames, event("content_block_stop", map[string]any{
				"type": "content_block_stop", "index": state.block,
			}))
			state.stopped = true
		}
	}
	return frames
}

func mergeName(current, incoming string) string {
	if current == "" {
		return incoming
	}
	if incoming == "" || current == incoming || len(current) > len(incoming) && current[:len(incoming)] == incoming {
		return current
	}
	if len(incoming) > len(current) && incoming[:len(current)] == current {
		return incoming
	}
	return incoming
}

func ParseEvents(frames []string) []map[string]any {
	out := make([]map[string]any, 0, len(frames))
	for _, frame := range frames {
		for _, line := range splitLines(frame) {
			if len(line) < 5 || line[:5] != "data:" {
				continue
			}
			var payload map[string]any
			if json.Unmarshal([]byte(trimSpace(line[5:])), &payload) == nil {
				out = append(out, payload)
			}
		}
	}
	return out
}

func splitLines(value string) []string {
	var lines []string
	start := 0
	for index, r := range value {
		if r == '\n' {
			lines = append(lines, value[start:index])
			start = index + 1
		}
	}
	if start < len(value) {
		lines = append(lines, value[start:])
	}
	return lines
}

func trimSpace(value string) string {
	for len(value) > 0 && (value[0] == ' ' || value[0] == '\t') {
		value = value[1:]
	}
	return value
}
