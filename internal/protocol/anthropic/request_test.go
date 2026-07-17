package anthropic

import (
	"strings"
	"testing"
)

func TestBuildOpenAIChatBodyConvertsAnthropicMessages(t *testing.T) {
	body, err := BuildOpenAIChatBody(map[string]any{
		"model":      "claude-sonnet-4-5",
		"system":     []any{map[string]any{"type": "text", "text": "be useful"}},
		"max_tokens": 64,
		"stream":     true,
		"messages": []any{
			map[string]any{"role": "user", "content": []any{
				map[string]any{"type": "text", "text": "hi"},
				map[string]any{"type": "tool_result", "tool_use_id": "toolu_1", "content": "done"},
				map[string]any{"type": "text", "text": "continue"},
			}},
			map[string]any{"role": "assistant", "content": []any{
				map[string]any{"type": "thinking", "thinking": "plan"},
				map[string]any{"type": "text", "text": "ok"},
				map[string]any{"type": "tool_use", "id": "toolu_2", "name": "Edit", "input": map[string]any{"file_path": "/x"}},
			}},
		},
		"tools": []any{
			map[string]any{"name": "Write", "description": "write files", "input_schema": map[string]any{"type": "object"}},
			map[string]any{"name": "Edit", "input_schema": map[string]any{"type": "object"}},
		},
		"tool_choice": map[string]any{"type": "any"},
		"thinking":    map[string]any{"type": "enabled", "budget_tokens": 4096},
		"metadata":    map[string]any{"user_id": "u1"},
	}, "grok-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if body["model"] != "grok-4.5" || body["max_tokens"] != 64 || body["tool_choice"] != "required" || body["reasoning_effort"] != "medium" || body["user"] != "u1" {
		t.Fatalf("unexpected body %#v", body)
	}
	opts, _ := body["stream_options"].(map[string]any)
	if opts == nil || opts["include_usage"] != true {
		t.Fatalf("stream_options = %#v", body["stream_options"])
	}
	tools := body["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	first := tools[0].(map[string]any)
	fn := first["function"].(map[string]any)
	if first["type"] != "function" || fn["name"] != "Edit" {
		t.Fatalf("tools should be sorted OpenAI functions, got %#v", tools)
	}
	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("parameters = %#v", params)
	}
	messages := body["messages"].([]map[string]any)
	if len(messages) != 5 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0]["role"] != "system" || messages[0]["content"] != "be useful" {
		t.Fatalf("system message = %#v", messages[0])
	}
	if messages[1]["role"] != "user" || messages[1]["content"] != "hi" {
		t.Fatalf("first user message = %#v", messages[1])
	}
	if messages[2]["role"] != "tool" || messages[2]["tool_call_id"] != "toolu_1" || messages[2]["content"] != "done" {
		t.Fatalf("tool result message = %#v", messages[2])
	}
	assistant := messages[4]
	if assistant["role"] != "assistant" || assistant["reasoning_content"] != "plan" || assistant["content"] != "ok" {
		t.Fatalf("assistant message = %#v", assistant)
	}
	calls := assistant["tool_calls"].([]any)
	call := calls[0].(map[string]any)
	if call["id"] != "toolu_2" || call["type"] != "function" {
		t.Fatalf("tool call = %#v", call)
	}
}

func TestBuildOpenAIChatBodyConvertsImagesAndThinkingBudget(t *testing.T) {
	body, err := BuildOpenAIChatBody(map[string]any{
		"messages":   []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "image", "source": map[string]any{"type": "base64", "media_type": "image/jpeg", "data": "abc"}}}}},
		"max_tokens": 1,
		"thinking":   map[string]any{"type": "enabled", "budget_tokens": 9000},
	}, "grok")
	if err != nil {
		t.Fatal(err)
	}
	messages := body["messages"].([]map[string]any)
	parts := messages[0]["content"].([]any)
	image := parts[0].(map[string]any)["image_url"].(map[string]any)
	if image["url"] != "data:image/jpeg;base64,abc" {
		t.Fatalf("image = %#v", image)
	}
	if body["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %#v", body["reasoning_effort"])
	}
}

func TestExtractPromptCacheKeyFromMetadataAndCacheControl(t *testing.T) {
	if got := ExtractPromptCacheKey(map[string]any{"metadata": map[string]any{"session_id": "sess-1"}}); got != "sess-1" {
		t.Fatalf("metadata key = %q", got)
	}
	key := ExtractPromptCacheKey(map[string]any{
		"system": []any{map[string]any{"type": "text", "text": "sys", "cache_control": map[string]any{"type": "ephemeral"}}},
		"tools":  []any{map[string]any{"name": "Edit", "cache_control": map[string]any{"type": "ephemeral"}}},
	})
	if !strings.HasPrefix(key, "acc:") || len(key) < 10 {
		t.Fatalf("cache_control key = %q", key)
	}
	if ExtractPromptCacheKey(map[string]any{"messages": []any{map[string]any{"role": "user", "content": "hi"}}}) != "" {
		t.Fatalf("expected empty key without markers")
	}
}

func TestThinkingToReasoningEffort(t *testing.T) {
	if got := ThinkingToReasoningEffort(true); got != "medium" {
		t.Fatalf("bool = %q", got)
	}
	if got := ThinkingToReasoningEffort("high"); got != "high" {
		t.Fatalf("string = %q", got)
	}
	if got := ThinkingToReasoningEffort(map[string]any{"type": "enabled", "budget_tokens": 1000}); got != "low" {
		t.Fatalf("budget low = %q", got)
	}
	if got := ThinkingToReasoningEffort(map[string]any{"type": "disabled"}); got != "" {
		t.Fatalf("disabled = %q", got)
	}
}
