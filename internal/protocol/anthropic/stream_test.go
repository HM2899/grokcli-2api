package anthropic

import "testing"

func TestStreamIncompleteToolKeepsTerminalWithoutToolBlock(t *testing.T) {
	assembler := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := assembler.Feed("", "", []ToolDelta{{Index: 0, ID: "t", Name: "Update", Arguments: `{"file_path":"/x"}`}})
	frames = append(frames, assembler.Finish("tool_calls", Usage{})...)
	events := ParseEvents(frames)
	sawTool, sawStop := false, false
	for _, payload := range events {
		if payload["type"] == "content_block_start" {
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				sawTool = true
			}
		}
		if payload["type"] == "message_stop" {
			sawStop = true
		}
	}
	if sawTool || !sawStop {
		t.Fatalf("tool=%v stop=%v events=%#v", sawTool, sawStop, events)
	}
}

func TestStreamCompleteUpdateIsDenseEditTool(t *testing.T) {
	assembler := NewStreamAssembler("m", "g", true, 1, []string{"Edit"})
	frames := assembler.Feed("preface", "", []ToolDelta{{
		Index: 2, ID: "t", Name: "Update",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":""}`,
	}})
	frames = append(frames, assembler.Finish("tool_calls", Usage{PromptTokens: 2, CompletionTokens: 1})...)
	events := ParseEvents(frames)
	toolIndex := -1
	stopReason := ""
	for _, payload := range events {
		if payload["type"] == "content_block_start" {
			block, _ := payload["content_block"].(map[string]any)
			if block["type"] == "tool_use" {
				toolIndex = int(payload["index"].(float64))
				if block["name"] != "Edit" {
					t.Fatalf("unexpected block %#v", block)
				}
			}
		}
		if payload["type"] == "message_delta" {
			delta := payload["delta"].(map[string]any)
			stopReason, _ = delta["stop_reason"].(string)
		}
	}
	if toolIndex != 0 || stopReason != "tool_use" {
		t.Fatalf("index=%d stop=%q events=%#v", toolIndex, stopReason, events)
	}
}
