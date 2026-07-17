package proxy

import "testing"

func TestSanitizeUpstreamBodyNormalizesToolsAndToolChoice(t *testing.T) {
	body := map[string]any{
		"model":            "grok",
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"presence_penalty": 0,
		"temperature":      3,
		"top_p":            -1,
		"max_tokens":       0,
		"tool_choice":      map[string]any{"type": "function", "function": map[string]any{"name": "Bash"}},
		"tools": []any{
			map[string]any{"type": "web_search_preview"},
			map[string]any{"name": "Zed", "description": "last", "input_schema": map[string]any{"properties": map[string]any{"x": map[string]any{"type": "string"}}}},
			map[string]any{"type": "function", "function": map[string]any{"name": "Bash", "parameters": `{"type":"object"}`}},
		},
	}
	got := SanitizeUpstreamBody(body)
	if _, ok := got["presence_penalty"]; ok {
		t.Fatalf("unsupported field leaked: %#v", got)
	}
	if got["temperature"] != float64(2) || got["top_p"] != float64(0) {
		t.Fatalf("unexpected clamps %#v", got)
	}
	if _, ok := got["max_tokens"]; ok {
		t.Fatalf("invalid max_tokens leaked: %#v", got)
	}
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v", got["tool_choice"])
	}
	tools := got["tools"].([]any)
	if len(tools) != 2 {
		t.Fatalf("tools = %#v", tools)
	}
	first := tools[0].(map[string]any)["function"].(map[string]any)
	second := tools[1].(map[string]any)["function"].(map[string]any)
	if first["name"] != "Bash" || second["name"] != "Zed" {
		t.Fatalf("tools not sorted/normalized: %#v", tools)
	}
	if _, ok := first["input_schema"]; ok {
		t.Fatalf("input_schema leaked in function: %#v", first)
	}
	params := second["parameters"].(map[string]any)
	if params["type"] != "object" {
		t.Fatalf("missing object type: %#v", params)
	}
}

func TestSanitizeUpstreamBodyDropsToolChoiceWithoutTools(t *testing.T) {
	got := SanitizeUpstreamBody(map[string]any{
		"messages":            []any{map[string]any{"role": "user", "content": "hi"}},
		"tool_choice":         "required",
		"parallel_tool_calls": true,
		"function_call":       map[string]any{"name": "legacy"},
		"tools":               []any{map[string]any{"type": "web_search"}},
	})
	if got["tools"] != nil || got["tool_choice"] != nil || got["parallel_tool_calls"] != nil || got["function_call"] != nil {
		t.Fatalf("tool-only fields leaked without tools: %#v", got)
	}
}

func TestSanitizeUpstreamBodyNormalizesLegacyFunctions(t *testing.T) {
	got := SanitizeUpstreamBody(map[string]any{
		"messages":    []any{map[string]any{"role": "user", "content": "hi"}},
		"functions":   []any{map[string]any{"name": "legacy", "input_schema": map[string]any{}}},
		"tool_choice": map[string]any{"type": "any"},
	})
	if got["tool_choice"] != "required" {
		t.Fatalf("tool_choice = %#v", got["tool_choice"])
	}
	functions := got["functions"].([]any)
	fn := functions[0].(map[string]any)
	if fn["input_schema"] != nil {
		t.Fatalf("input_schema leaked: %#v", fn)
	}
	params := fn["parameters"].(map[string]any)
	if params["type"] != "object" || params["properties"] == nil {
		t.Fatalf("parameters not repaired: %#v", params)
	}
}

func TestSanitizeUpstreamBodyDoesNotMutateInput(t *testing.T) {
	input := map[string]any{"temperature": 3, "tools": []any{map[string]any{"name": "Bash"}}}
	got := SanitizeUpstreamBody(input)
	if input["temperature"] != 3 {
		t.Fatalf("input mutated: %#v", input)
	}
	if got["temperature"] != float64(2) {
		t.Fatalf("output not sanitized: %#v", got)
	}
}

func TestPrepareUpstreamBodyStripsPrivateKeys(t *testing.T) {
	out := PrepareUpstreamBody(map[string]any{
		"messages":         []any{map[string]any{"role": "user", "content": "hi"}},
		"tools":            []any{map[string]any{"name": "Edit", "input_schema": map[string]any{"type": "object"}}},
		"_history_compact": map[string]any{"applied": true},
		"prompt_cache_key": "x",
	})
	if out["_history_compact"] != nil || out["_prompt_stabilize"] != nil {
		t.Fatalf("private keys leaked: %#v", out)
	}
	if out["prompt_cache_key"] != nil {
		t.Fatalf("prompt_cache_key should be stripped before upstream: %#v", out)
	}
	tools := out["tools"].([]any)
	fn := tools[0].(map[string]any)["function"].(map[string]any)
	if fn["parameters"] == nil {
		t.Fatalf("parameters missing: %#v", tools)
	}
}

func TestSanitizeUpstreamBodyDropsPromptCacheKey(t *testing.T) {
	out := SanitizeUpstreamBody(map[string]any{
		"messages":               []any{map[string]any{"role": "user", "content": "hi"}},
		"prompt_cache_key":       "019f668b-9052-7842-ae62-12580fdf5005",
		"prompt_cache_retention": "session",
		"presence_penalty":       0.5,
	})
	if out["prompt_cache_key"] != nil {
		t.Fatalf("prompt_cache_key should be stripped: %#v", out)
	}
	if out["prompt_cache_retention"] != nil {
		t.Fatalf("prompt_cache_retention should be stripped: %#v", out)
	}
	if out["presence_penalty"] != nil {
		t.Fatalf("unsupported field leaked: %#v", out)
	}
}

func TestSanitizeUpstreamBodyPromptCacheKeyAbsent(t *testing.T) {
	out := SanitizeUpstreamBody(map[string]any{
		"messages": []any{map[string]any{"role": "user", "content": "hi"}},
	})
	if _, ok := out["prompt_cache_key"]; ok {
		t.Fatalf("prompt_cache_key should not be present: %#v", out)
	}
	if _, ok := out["prompt_cache_retention"]; ok {
		t.Fatalf("prompt_cache_retention should not be present: %#v", out)
	}
}
