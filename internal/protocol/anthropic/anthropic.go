package anthropic

import (
	"encoding/json"
	"fmt"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

type ToolCall struct {
	ID        string
	Name      string
	Arguments string
}

type Usage struct {
	PromptTokens        int
	CompletionTokens    int
	TotalTokens         int
	CacheReadTokens     int
	CacheCreationTokens int
}

func StopReason(finish string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_use"
	}
	switch finish {
	case "length", "max_tokens":
		return "max_tokens"
	case "content_filter":
		return "refusal"
	case "stop_sequence":
		return "stop_sequence"
	default:
		return "end_turn"
	}
}

func Completion(messageID, model, content, reasoning, finish string, calls []ToolCall, usage Usage, allowed []string) map[string]any {
	blocks := make([]any, 0, 2+len(calls))
	if reasoning != "" {
		blocks = append(blocks, map[string]any{"type": "thinking", "thinking": reasoning})
	}
	if content != "" {
		blocks = append(blocks, map[string]any{"type": "text", "text": content})
	}
	emittedTools := 0
	for _, call := range calls {
		name := toolcall.CanonicalName(call.Name, allowed)
		arguments := toolcall.EffectiveJSON(call.Arguments, name)
		if name == "" || !toolcall.CompleteJSON(arguments, name) {
			continue
		}
		var input any
		if err := json.Unmarshal([]byte(arguments), &input); err != nil {
			continue
		}
		id := call.ID
		if id == "" {
			id = fmt.Sprintf("toolu_go_%d", emittedTools)
		}
		blocks = append(blocks, map[string]any{
			"type": "tool_use", "id": id, "name": name, "input": input,
		})
		emittedTools++
	}
	if len(blocks) == 0 {
		blocks = append(blocks, map[string]any{"type": "text", "text": ""})
	}

	outputTokens := usage.CompletionTokens
	if outputTokens <= 0 {
		chars := len([]rune(content)) + len([]rune(reasoning))
		if chars > 0 {
			outputTokens = (chars + 3) / 4
		}
	}
	inputTokens := usage.PromptTokens
	if inputTokens <= 0 && outputTokens <= 0 && usage.TotalTokens > 0 {
		inputTokens = usage.TotalTokens
	}

	return map[string]any{
		"id":            messageID,
		"type":          "message",
		"role":          "assistant",
		"content":       blocks,
		"model":         model,
		"stop_reason":   StopReason(finish, emittedTools > 0),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":                inputTokens,
			"output_tokens":               outputTokens,
			"cache_creation_input_tokens": usage.CacheCreationTokens,
			"cache_read_input_tokens":     usage.CacheReadTokens,
		},
	}
}

func event(name string, payload any) string {
	encoded, _ := json.Marshal(payload)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", name, encoded)
}

func TerminalError(message, errorType string) []string {
	if errorType == "" {
		errorType = "api_error"
	}
	return []string{
		event("error", map[string]any{
			"type": "error", "error": map[string]any{"type": errorType, "message": message},
		}),
		event("message_delta", map[string]any{
			"type":  "message_delta",
			"delta": map[string]any{"stop_reason": "end_turn", "stop_sequence": nil},
			"usage": map[string]any{"output_tokens": 0},
		}),
		event("message_stop", map[string]any{"type": "message_stop"}),
	}
}

// Ping is the Anthropic SSE keepalive used during long thinking / tool-prep gaps.
func Ping() string {
	return event("ping", map[string]any{"type": "ping"})
}

// CommentKeepalive is a pure SSE comment for reverse proxies that ignore named
// ping events but still need idle traffic.
func CommentKeepalive() string {
	return ": keepalive\n\n"
}
