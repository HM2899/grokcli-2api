package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEmptyCompleteCanStillFail(t *testing.T) {
	stream := NewLiveStreamer("resp", "grok", nil)
	stream.Start()
	if frames := stream.Complete(nil); len(frames) != 0 {
		t.Fatalf("empty complete emitted %#v", frames)
	}
	failed := stream.Fail("empty upstream", "")
	if len(failed) != 2 || !strings.Contains(failed[0], "response.failed") || failed[1] != "data: [DONE]\n\n" {
		t.Fatalf("unexpected failure %#v", failed)
	}
}

func TestToolStreamUsesStableIDsAndMonotonicSequence(t *testing.T) {
	stream := NewLiveStreamer("resp", "grok", []string{"Edit"})
	frames := stream.ToolDeltas([]ToolDelta{{
		Index: 3, ID: "call", Name: "Update",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":""}`,
	}})
	frames = append(frames, stream.Complete(&Usage{InputTokens: 2, OutputTokens: 1})...)
	sequence := 0
	itemID := ""
	for _, frame := range frames {
		if frame == "data: [DONE]\n\n" {
			continue
		}
		parts := strings.SplitN(frame, "data: ", 2)
		var payload map[string]any
		if len(parts) != 2 || json.Unmarshal([]byte(strings.TrimSpace(parts[1])), &payload) != nil {
			t.Fatalf("invalid SSE %q", frame)
		}
		if int(payload["sequence_number"].(float64)) != sequence {
			t.Fatalf("sequence %v want %d", payload, sequence)
		}
		sequence++
		if value, ok := payload["item_id"].(string); ok {
			if itemID == "" {
				itemID = value
			} else if value != itemID {
				t.Fatalf("item id changed %q to %q", itemID, value)
			}
		}
	}
	if itemID == "" {
		t.Fatal("no function item id observed")
	}
}
