package anthropic

import (
	"strings"
	"testing"
)

func TestStreamContractTextThinkingToolOrder(t *testing.T) {
	// text
	a := NewStreamAssembler("m1", "grok", false, 1, nil)
	frames := a.Feed("hello", "", nil)
	frames = append(frames, a.Finish("stop", Usage{PromptTokens: 1, CompletionTokens: 1})...)
	joined := strings.Join(frames, "")
	for _, marker := range []string{"event: message_start", "text_delta", "hello", "event: message_delta", "event: message_stop"} {
		if !strings.Contains(joined, marker) {
			t.Fatalf("text contract missing %q in %s", marker, joined)
		}
	}

	// thinking then text
	a = NewStreamAssembler("m2", "grok", false, 1, nil)
	frames = a.Feed("", "plan", nil)
	frames = append(frames, a.Feed("ok", "", nil)...)
	frames = append(frames, a.Finish("stop", Usage{})...)
	events := ParseEvents(frames)
	types := make([]string, 0, len(events))
	for _, e := range events {
		types = append(types, stringValue(e["type"]))
	}
	joinedTypes := strings.Join(types, ",")
	if !strings.Contains(joinedTypes, "content_block_start") || !strings.Contains(joinedTypes, "message_delta") || !strings.Contains(joinedTypes, "message_stop") {
		t.Fatalf("thinking/text event order unexpected: %s", joinedTypes)
	}

	// tool rewrite Update->Edit dense index 0
	a = NewStreamAssembler("m3", "grok", true, 1, []string{"Edit"})
	frames = a.Feed("preface", "", []ToolDelta{{Index: 0, ID: "c1", Name: "Update", Arguments: `{"file_path":"/x","old_string":"a","new_string":""}`}})
	frames = append(frames, a.Finish("tool_calls", Usage{})...)
	events = ParseEvents(frames)
	toolIndex := -1
	stop := ""
	for _, e := range events {
		if e["type"] == "content_block_start" {
			block, _ := e["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				toolIndex = int(e["index"].(float64))
				if block["name"] != "Edit" {
					t.Fatalf("tool name = %#v", block)
				}
			}
		}
		if e["type"] == "message_delta" {
			delta := e["delta"].(map[string]any)
			stop, _ = delta["stop_reason"].(string)
		}
	}
	if toolIndex != 0 || stop != "tool_use" {
		t.Fatalf("tool contract index=%d stop=%q", toolIndex, stop)
	}
}

func TestPingAndKeepaliveFrames(t *testing.T) {
	if !strings.Contains(Ping(), "event: ping") {
		t.Fatalf("ping = %q", Ping())
	}
	if CommentKeepalive() != ": keepalive\n\n" {
		t.Fatalf("comment = %q", CommentKeepalive())
	}
}
