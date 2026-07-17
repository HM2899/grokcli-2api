package anthropic

import (
	"strings"
	"testing"
)

func TestCompletionOnlyUsesToolStopWhenToolEmitted(t *testing.T) {
	incomplete := Completion("m", "g", "", "", "tool_calls", []ToolCall{{Name: "Update", Arguments: `{"file_path":"/x"}`}}, Usage{}, nil)
	if incomplete["stop_reason"] != "end_turn" {
		t.Fatalf("unexpected stop reason %#v", incomplete)
	}

	complete := Completion("m", "g", "", "", "tool_calls", []ToolCall{{ID: "t", Name: "Update", Arguments: `{"file_path":"/x","old_string":"a","new_string":""}`}}, Usage{}, []string{"Edit"})
	if complete["stop_reason"] != "tool_use" {
		t.Fatalf("unexpected stop reason %#v", complete)
	}
	blocks := complete["content"].([]any)
	tool := blocks[0].(map[string]any)
	if tool["name"] != "Edit" {
		t.Fatalf("unexpected tool %#v", tool)
	}
}

func TestTerminalErrorClosesEnvelope(t *testing.T) {
	frames := TerminalError("boom", "")
	joined := strings.Join(frames, "")
	for _, marker := range []string{"event: error", "event: message_delta", `"stop_reason":"end_turn"`, "event: message_stop"} {
		if !strings.Contains(joined, marker) {
			t.Fatalf("missing %q in %s", marker, joined)
		}
	}
}
