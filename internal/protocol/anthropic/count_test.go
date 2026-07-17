package anthropic

import "testing"

func TestCountTokensForRequestMatchesPythonHeuristic(t *testing.T) {
	got := CountTokensForRequest(map[string]any{
		"system": "abcd",
		"messages": []any{map[string]any{
			"role": "user",
			"content": []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "tool_use", "name": "Edit", "input": map[string]any{"file_path": "/x", "old_string": "a", "new_string": ""}},
			},
		}},
		"tools": []any{map[string]any{"name": "Edit", "description": "edit files", "input_schema": map[string]any{"type": "object"}}},
	})
	if got["input_tokens"] != 27 {
		t.Fatalf("input_tokens = %#v", got)
	}
}

func TestEstimateTokensCountsRunes(t *testing.T) {
	if EstimateTokens("额度耗尽") != 1 {
		t.Fatalf("unexpected unicode estimate")
	}
	if EstimateTokens("abcde") != 2 {
		t.Fatalf("unexpected ascii estimate")
	}
}

func TestHasMessagesOrSystem(t *testing.T) {
	if HasMessagesOrSystem(map[string]any{}) {
		t.Fatalf("empty request should not pass")
	}
	if !HasMessagesOrSystem(map[string]any{"system": "x"}) {
		t.Fatalf("system should pass")
	}
	if !HasMessagesOrSystem(map[string]any{"messages": []any{map[string]any{"role": "user"}}}) {
		t.Fatalf("messages should pass")
	}
}
