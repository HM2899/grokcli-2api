package historycompact

import (
	"os"
	"strings"
	"testing"
)

func TestCompactMessagesSoftTierPreservesRecentToolResults(t *testing.T) {
	messages := []any{
		map[string]any{"role": "system", "content": "sys"},
	}
	// 3 old rounds + 1 recent
	for i := 0; i < 4; i++ {
		messages = append(messages,
			map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{map[string]any{"id": "c" + string(rune('0'+i)), "type": "function", "function": map[string]any{"name": "Read", "arguments": `{"file_path":"/x"}`}}}},
			map[string]any{"role": "tool", "tool_call_id": "c" + string(rune('0'+i)), "content": strings.Repeat("A", 200) + "-round-" + string(rune('0'+i)) + "-" + strings.Repeat("Z", 200)},
		)
	}
	opts := Options{
		Enabled: true, PrefixStable: true,
		KeepToolRounds: 1, MidToolRounds: 1,
		MaxToolResultChars: 10_000, MidToolResultChars: 120, OldToolResultChars: 80,
		MaxMessagesChars: 1_000_000,
	}
	out, stats := CompactMessages(messages, opts)
	if !truthy(stats["applied"]) {
		t.Fatalf("expected applied stats %#v", stats)
	}
	// last tool message should stay full (within max)
	last := out[len(out)-1].(map[string]any)
	if !strings.Contains(stringValue(last["content"]), "-round-3-") || strings.Contains(stringValue(last["content"]), "truncated") {
		t.Fatalf("recent tool mutated: %q", last["content"])
	}
	// older tool should be soft-summarized
	old := out[2].(map[string]any)
	if !alreadyCompacted(stringValue(old["content"])) && len([]rune(stringValue(old["content"]))) > 120 {
		t.Fatalf("old tool not compacted: %q", old["content"])
	}
}

func TestApplyDisabledByDefault(t *testing.T) {
	_ = os.Unsetenv("GROK2API_HISTORY_COMPACT")
	_ = os.Unsetenv("GROK2API_HISTORY_COMPACT_AUTO_CHARS")
	body := map[string]any{
		"messages": []any{
			map[string]any{"role": "assistant", "tool_calls": []any{map[string]any{"id": "1", "type": "function", "function": map[string]any{"name": "Read", "arguments": "{}"}}}},
			map[string]any{"role": "tool", "tool_call_id": "1", "content": strings.Repeat("x", 5000)},
		},
	}
	stats := Apply(body)
	if truthy(stats["enabled"]) || truthy(stats["applied"]) {
		t.Fatalf("default should be off: %#v", stats)
	}
	content := body["messages"].([]any)[1].(map[string]any)["content"]
	if len(stringValue(content)) != 5000 {
		t.Fatalf("content mutated while disabled")
	}
}

func TestShouldAutoCompact(t *testing.T) {
	t.Setenv("GROK2API_HISTORY_COMPACT_AUTO_CHARS", "100")
	body := map[string]any{"messages": []any{map[string]any{"role": "user", "content": strings.Repeat("y", 200)}}}
	if !ShouldAutoCompact(body) {
		t.Fatalf("expected auto compact")
	}
}
