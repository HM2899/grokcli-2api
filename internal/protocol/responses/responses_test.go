package responses

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestFailureSequenceIsMonotonicAndDone(t *testing.T) {
	frames := Failure("resp", "grok", "boom", "")
	if len(frames) != 4 || frames[3] != "data: [DONE]\n\n" {
		t.Fatalf("unexpected frames %#v", frames)
	}
	for index, frame := range frames[:3] {
		parts := strings.Split(frame, "data: ")
		if len(parts) != 2 {
			t.Fatalf("bad SSE %q", frame)
		}
		var body map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(parts[1])), &body); err != nil {
			t.Fatal(err)
		}
		if int(body["sequence_number"].(float64)) != index {
			t.Fatalf("sequence %v at %d", body, index)
		}
	}
}

func TestUsageAlwaysHasCacheAndReasoningShape(t *testing.T) {
	usage := NormalizeUsage(nil)
	if usage["input_tokens_details"].(map[string]any)["cached_tokens"] != 0 {
		t.Fatalf("unexpected usage %#v", usage)
	}
	if usage["output_tokens_details"].(map[string]any)["reasoning_tokens"] != 0 {
		t.Fatalf("unexpected usage %#v", usage)
	}
}
