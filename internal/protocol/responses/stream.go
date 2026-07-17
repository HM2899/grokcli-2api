package responses

import (
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

type liveTool struct {
	id        string
	name      string
	arguments string
	itemID    string
	output    int
	emitted   bool
}

// LiveStreamer emits a valid Responses envelope with monotonic sequence
// numbers. Complete deliberately remains open when no client payload exists so
// callers can still emit response.failed for an empty upstream HTTP 200.
type LiveStreamer struct {
	responseID    string
	model         string
	allowed       []string
	maxTools      int
	toolsStarted  int
	sequence      Sequence
	started       bool
	closed        bool
	textOpen      bool
	reasoningOpen bool
	messageID     string
	reasoningID   string
	text          string
	reasoning     string
	output        int
	tools         map[int]*liveTool
}

func NewLiveStreamer(responseID, model string, allowed []string) *LiveStreamer {
	return NewLiveStreamerWithMaxTools(responseID, model, allowed, 0)
}

// NewLiveStreamerWithMaxTools caps outbound function_call items per turn.
// maxTools <= 0 means unlimited (Codex / OpenAI-native).
func NewLiveStreamerWithMaxTools(responseID, model string, allowed []string, maxTools int) *LiveStreamer {
	return &LiveStreamer{
		responseID: responseID,
		model:      model,
		allowed:    append([]string(nil), allowed...),
		maxTools:   maxTools,
		messageID:  "msg_" + responseID,
		tools:      make(map[int]*liveTool),
	}
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
		frames = append(frames,
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
		)
		s.reasoningOpen = false
		s.output++
	}
	if !s.textOpen {
		s.textOpen = true
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
		"item_id": s.messageID, "output_index": s.output, "content_index": 0,
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
	indexes := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		state := s.tools[index]
		if state.emitted || state.name == "" || !toolcall.CompleteJSON(state.arguments, state.name) {
			continue
		}
		if s.maxTools > 0 && s.toolsStarted >= s.maxTools {
			break
		}
		state.emitted = true
		s.toolsStarted++
		state.output = s.output
		state.itemID = fmt.Sprintf("fc_%s_%d", s.responseID, index)
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
				"delta": state.arguments,
			}),
			s.sequence.Event("response.function_call_arguments.done", map[string]any{
				"item_id": state.itemID, "output_index": state.output,
				"arguments": state.arguments,
			}),
			s.sequence.Event("response.output_item.done", map[string]any{
				"output_index": state.output,
				"item": map[string]any{
					"id": state.itemID, "type": "function_call", "status": "completed",
					"call_id": state.id, "name": state.name, "arguments": state.arguments,
				},
			}),
		)
		s.output++
	}
	return frames
}

func (s *LiveStreamer) HasClientPayload() bool {
	if s.text != "" {
		return true
	}
	for _, state := range s.tools {
		if state.emitted {
			return true
		}
	}
	return false
}

func (s *LiveStreamer) Complete(usage *Usage) []string {
	if s.closed || !s.HasClientPayload() {
		return nil
	}
	frames := make([]string, 0, 5)
	if s.textOpen {
		frames = append(frames,
			s.sequence.Event("response.output_text.done", map[string]any{
				"item_id": s.messageID, "output_index": 0, "content_index": 0,
				"text": s.text,
			}),
			s.sequence.Event("response.content_part.done", map[string]any{
				"item_id": s.messageID, "output_index": 0, "content_index": 0,
				"part": map[string]any{"type": "output_text", "text": s.text},
			}),
			s.sequence.Event("response.output_item.done", map[string]any{
				"output_index": 0,
				"item": map[string]any{
					"id": s.messageID, "type": "message", "role": "assistant",
					"status":  "completed",
					"content": []any{map[string]any{"type": "output_text", "text": s.text}},
				},
			}),
		)
	}
	completed := map[string]any{
		"id": s.responseID, "object": "response", "created_at": 0,
		"status": "completed", "model": s.model, "usage": NormalizeUsage(usage),
		"output": []any{},
	}
	frames = append(frames,
		s.sequence.Event("response.completed", map[string]any{"response": completed}),
		"data: [DONE]\n\n",
	)
	s.closed = true
	return frames
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
