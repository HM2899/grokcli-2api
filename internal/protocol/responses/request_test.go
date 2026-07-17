package responses

import "testing"

func TestBuildChatBodyConvertsResponsesInput(t *testing.T) {
	body := BuildChatBody(map[string]any{
		"instructions":        "be useful",
		"input":               []any{map[string]any{"type": "input_text", "text": "hi"}, map[string]any{"type": "function_call_output", "call_id": "call_1", "output": map[string]any{"ok": true}}},
		"max_output_tokens":   64,
		"tools":               []any{map[string]any{"type": "function", "name": "Edit", "parameters": map[string]any{"type": "object"}}},
		"tool_choice":         "required",
		"parallel_tool_calls": true,
		"reasoning":           map[string]any{"effort": "medium"},
		"metadata":            map[string]any{"user": "u1", "prompt_cache_key": "pck"},
	}, "grok")
	if body["model"] != "grok" || body["max_tokens"] != 64 || body["tool_choice"] != "required" || body["reasoning_effort"] != "medium" || body["user"] != "u1" || body["prompt_cache_key"] != "pck" {
		t.Fatalf("unexpected body %#v", body)
	}
	messages := body["messages"].([]map[string]any)
	if len(messages) != 3 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0]["role"] != "system" || messages[1]["content"] != "hi" || messages[2]["role"] != "tool" || messages[2]["tool_call_id"] != "call_1" {
		t.Fatalf("unexpected messages %#v", messages)
	}
	tools := body["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["name"] != "Edit" {
		t.Fatalf("tools = %#v", tools)
	}
}

func TestBuildObjectConvertsChatResult(t *testing.T) {
	obj := BuildObject("resp_1", "grok", "hello", "plan", []map[string]any{{"id": "call_1", "function": map[string]any{"name": "Edit", "arguments": "{\"file_path\":\"/x\"}"}}}, map[string]any{"prompt_tokens": 2, "completion_tokens": 3, "total_tokens": 5}, 123, "resp_0", map[string]any{"a": "b"})
	if obj["id"] != "resp_1" || obj["status"] != "completed" || obj["previous_response_id"] != "resp_0" {
		t.Fatalf("unexpected object %#v", obj)
	}
	output := obj["output"].([]any)
	if len(output) != 2 {
		t.Fatalf("output = %#v", output)
	}
	msg := output[0].(map[string]any)
	call := output[1].(map[string]any)
	if msg["type"] != "message" || call["type"] != "function_call" || call["call_id"] != "call_1" {
		t.Fatalf("unexpected output %#v", output)
	}
	usage := obj["usage"].(map[string]any)
	if usage["input_tokens"] != 2 || usage["output_tokens"] != 3 || usage["total_tokens"] != 5 {
		t.Fatalf("usage = %#v", usage)
	}
}

func TestInputToMessagesFlattensInputTextParts(t *testing.T) {
	messages := InputToMessages([]any{
		map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "input_text", "text": "hi"},
			},
		},
	}, "be useful")
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	if messages[0]["role"] != "system" || messages[0]["content"] != "be useful" {
		t.Fatalf("system = %#v", messages[0])
	}
	if messages[1]["role"] != "user" || messages[1]["content"] != "hi" {
		t.Fatalf("user content not flattened: %#v", messages[1])
	}
}
