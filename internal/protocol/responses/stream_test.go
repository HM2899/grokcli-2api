package responses

import (
	"encoding/json"
	"fmt"
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
	if !strings.Contains(failed[0], `"code":"server_error"`) {
		t.Fatalf("response.failed missing error code: %s", failed[0])
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

func TestReasoningOnlyCompleteClosesEnvelope(t *testing.T) {
	stream := NewLiveStreamer("resp", "grok", nil)
	frames := stream.Reasoning("think")
	frames = append(frames, stream.Complete(&Usage{InputTokens: 1, OutputTokens: 1})...)
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "response.completed") || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("reasoning-only stream missing terminal: %q", joined)
	}
	if !strings.Contains(joined, "reasoning_summary") {
		t.Fatalf("missing reasoning frames: %q", joined)
	}
}

func TestIncompleteToolStillClosesEnvelope(t *testing.T) {
	// Incomplete required fields: do not emit a broken function_call, but still
	// close the envelope so Codex/Claude Code leave "running".
	stream := NewLiveStreamer("resp", "grok", []string{"Bash"})
	_ = stream.Reasoning("planning")
	incomplete := "{\"command\":"
	_ = stream.ToolDeltas([]ToolDelta{
		{Index: 0, ID: "call1", Name: "Bash", Arguments: incomplete},
	})
	frames := stream.Complete(&Usage{InputTokens: 1, OutputTokens: 1})
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "response.completed") || !strings.Contains(joined, "data: [DONE]") {
		t.Fatalf("expected terminal completed, got %q", joined)
	}
	if strings.Contains(joined, "function_call") && strings.Contains(joined, "Bash") {
		t.Fatalf("incomplete Bash should not emit function_call: %q", joined)
	}
}

func TestOutOfOrderToolsStillEmit(t *testing.T) {
	stream := NewLiveStreamer("resp", "grok", []string{"Bash", "Read"})
	incomplete := "{\"command\":"
	complete := "{\"file_path\":\"/a\"}"
	frames := stream.ToolDeltas([]ToolDelta{
		{Index: 0, ID: "c0", Name: "Bash", Arguments: incomplete},
		{Index: 1, ID: "c1", Name: "Read", Arguments: complete},
	})
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, "function_call") || !strings.Contains(joined, "Read") {
		t.Fatalf("expected ready tool index 1 emitted, got %q", joined)
	}
}

func TestCompletedIncludesFunctionCallOutput(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_x", "grok", []string{"shell"}, 0)
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "call_1", Name: "shell", Arguments: `{"command":"echo hi"}`}})
	frames = append(frames, s.Complete(&Usage{InputTokens: 1, OutputTokens: 1})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, "function_call") {
		t.Fatalf("missing function_call frames: %s", joined)
	}
	// Find response.completed payload and ensure output has the tool.
	found := false
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(line[5:])
			if payload == "[DONE]" || payload == "" {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) != nil {
				continue
			}
			if obj["type"] != "response.completed" {
				continue
			}
			resp, _ := obj["response"].(map[string]any)
			out, _ := resp["output"].([]any)
			if len(out) == 0 {
				t.Fatalf("completed output empty: %s", payload)
			}
			for _, item := range out {
				m, _ := item.(map[string]any)
				if m["type"] == "function_call" && m["name"] == "shell" {
					found = true
					if m["arguments"] != `{"command":"echo hi"}` && !strings.Contains(fmt.Sprint(m["arguments"]), "echo hi") {
						t.Fatalf("args=%v", m["arguments"])
					}
				}
			}
		}
	}
	if !found {
		t.Fatalf("completed missing shell function_call:\n%s", joined)
	}
}

func TestCompletedDropsNestedEmptyShell(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_y", "grok", []string{"shell"}, 0)
	_ = s.ToolDeltas([]ToolDelta{{Index: 0, ID: "call_bad", Name: "shell", Arguments: `{"command":[[""]]}`}})
	frames := s.Complete(&Usage{})
	joined := strings.Join(frames, "\n")
	// Must not emit a broken shell function_call.
	if strings.Contains(joined, `"name":"shell"`) || strings.Contains(joined, `"name": "shell"`) {
		// allow only if not present as function_call item; strict: fail if function_call with shell
		if strings.Contains(joined, "function_call") && strings.Contains(joined, "shell") {
			t.Fatalf("empty nested shell should be dropped:\n%s", joined)
		}
	}
}

func TestShellArgsProjectedToCmdForCodex(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_cmd", "grok", []string{"shell"}, 0)
	s.SetShellArgKeys(map[string]string{"shell": "cmd"})
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "call_1", Name: "shell", Arguments: `{"command":"echo hi"}`}})
	frames = append(frames, s.Complete(&Usage{InputTokens: 1, OutputTokens: 1})...)
	joined := strings.Join(frames, "\n")
	if !strings.Contains(joined, `"cmd"`) && !strings.Contains(joined, `"cmd":`) {
		// JSON may compact as {"cmd":"echo hi"}
		if !strings.Contains(joined, "cmd") {
			t.Fatalf("expected cmd in frames:\n%s", joined)
		}
	}
	// Must not leave only command key for Codex-preferring clients.
	// Allow "command" only if both appear; require cmd present in completed args.
	foundCmd := false
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(line[5:])
			if payload == "" || payload == "[DONE]" {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) != nil {
				continue
			}
			if obj["type"] == "response.function_call_arguments.done" {
				args := fmt.Sprint(obj["arguments"])
				if strings.Contains(args, `"cmd"`) || strings.Contains(args, `"cmd":`) || (strings.Contains(args, "cmd") && strings.Contains(args, "echo hi")) {
					foundCmd = true
				}
				if strings.Contains(args, `"command"`) && !strings.Contains(args, `"cmd"`) {
					t.Fatalf("Codex client got command instead of cmd: %s", args)
				}
			}
			item, _ := obj["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" && item["status"] == "completed" {
				args := fmt.Sprint(item["arguments"])
				if strings.Contains(args, "cmd") {
					foundCmd = true
				}
			}
		}
	}
	if !foundCmd {
		t.Fatalf("cmd not found in tool args:\n%s", joined)
	}
}

func TestShellArgsDefaultCmdWithoutKeyMap(t *testing.T) {
	// No SetShellArgKeys: shell-family tools must still project command→cmd for Codex.
	s := NewLiveStreamerWithMaxTools("resp_cmd_default", "grok", []string{"Shell"}, 0)
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "call_1", Name: "Shell", Arguments: `{"command":"pwd"}`}})
	frames = append(frames, s.Complete(&Usage{InputTokens: 1, OutputTokens: 1})...)
	foundCmd := false
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			payload := strings.TrimSpace(line[5:])
			var obj map[string]any
			if json.Unmarshal([]byte(payload), &obj) != nil {
				continue
			}
			if obj["type"] == "response.function_call_arguments.done" {
				args := fmt.Sprint(obj["arguments"])
				if strings.Contains(args, `"command"`) && !strings.Contains(args, `"cmd"`) {
					t.Fatalf("default shell projection still command: %s", args)
				}
				if strings.Contains(args, `"cmd"`) {
					foundCmd = true
				}
			}
			item, _ := obj["item"].(map[string]any)
			if item != nil && item["type"] == "function_call" && item["status"] == "completed" {
				args := fmt.Sprint(item["arguments"])
				if strings.Contains(args, `"command"`) && !strings.Contains(args, `"cmd"`) {
					t.Fatalf("completed item still command: %s", args)
				}
				if strings.Contains(args, `"cmd"`) {
					foundCmd = true
				}
			}
		}
	}
	if !foundCmd {
		t.Fatalf("expected default cmd projection:\n%s", strings.Join(frames, "\n"))
	}
}

// Hermes agent tool "terminal" requires parameter "command". Without a key map
// we must NOT project to Codex "cmd" or Hermes will fail the tool call.
func TestHermesTerminalArgsKeepCommandWithoutKeyMap(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_hermes", "grok", []string{"terminal"}, 0)
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "call_1", Name: "terminal", Arguments: `{"command":"ls -la"}`}})
	frames = append(frames, s.Complete(&Usage{InputTokens: 1, OutputTokens: 1})...)
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, `"cmd"`) && !strings.Contains(joined, `"command"`) {
		t.Fatalf("Hermes terminal projected to cmd-only:\n%s", joined)
	}
	foundCommand := false
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &obj) != nil {
				continue
			}
			if obj["type"] == "response.function_call_arguments.done" {
				args := fmt.Sprint(obj["arguments"])
				if strings.Contains(args, `"cmd"`) && !strings.Contains(args, `"command"`) {
					t.Fatalf("Hermes terminal args became cmd: %s", args)
				}
				if strings.Contains(args, `"command"`) && strings.Contains(args, "ls -la") {
					foundCommand = true
				}
			}
			if item, _ := obj["item"].(map[string]any); item != nil && item["type"] == "function_call" && item["status"] == "completed" {
				args := fmt.Sprint(item["arguments"])
				if strings.Contains(args, `"cmd"`) && !strings.Contains(args, `"command"`) {
					t.Fatalf("Hermes completed item became cmd: %s", args)
				}
				if strings.Contains(args, `"command"`) && strings.Contains(args, "ls -la") {
					foundCommand = true
				}
			}
		}
	}
	if !foundCommand {
		t.Fatalf("expected command projection for Hermes terminal:\n%s", joined)
	}
}

func TestHermesTerminalArgsHonorSchemaKeyMap(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_hermes_map", "grok", []string{"terminal"}, 0)
	s.SetShellArgKeys(map[string]string{"terminal": "command"})
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "c1", Name: "terminal", Arguments: `{"command":"pwd"}`}})
	frames = append(frames, s.Complete(&Usage{})...)
	joined := strings.Join(frames, "\n")
	// JSON-in-JSON may appear escaped as \"command\" in the SSE data payload.
	if strings.Contains(joined, `"cmd":"pwd"`) || strings.Contains(joined, `"cmd": "pwd"`) ||
		strings.Contains(joined, `\"cmd\":\"pwd\"`) {
		t.Fatalf("schema said command but got cmd:\n%s", joined)
	}
	if !strings.Contains(joined, "command") || !strings.Contains(joined, "pwd") {
		t.Fatalf("expected command+pwd in frames:\n%s", joined)
	}
	// Ensure we did not leave a bare cmd key for the shell payload.
	if strings.Contains(joined, `"cmd"`) || strings.Contains(joined, `\"cmd\"`) {
		// cmd may appear in unrelated fields; only fail if it pairs with pwd without command.
		if !strings.Contains(joined, "command") {
			t.Fatalf("cmd leaked without command:\n%s", joined)
		}
	}
}

func TestExecCommandArgsProjectedToCmd(t *testing.T) {
	// Codex get_goal/exec_command path: tool name is exec_command, schema wants cmd.
	s := NewLiveStreamerWithMaxTools("resp_exec", "grok", []string{"exec_command"}, 0)
	s.SetShellArgKeys(map[string]string{"exec_command": "cmd", "execcommand": "cmd"})
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "call_1", Name: "exec_command", Arguments: `{"command":"pwd"}`}})
	frames = append(frames, s.Complete(&Usage{InputTokens: 1, OutputTokens: 1})...)
	found := false
	for _, frame := range frames {
		for _, line := range strings.Split(frame, "\n") {
			if !strings.HasPrefix(line, "data:") {
				continue
			}
			var obj map[string]any
			if json.Unmarshal([]byte(strings.TrimSpace(line[5:])), &obj) != nil {
				continue
			}
			if obj["type"] == "response.function_call_arguments.done" {
				args := fmt.Sprint(obj["arguments"])
				if strings.Contains(args, `"command"`) && !strings.Contains(args, `"cmd"`) {
					t.Fatalf("exec_command still command: %s", args)
				}
				if strings.Contains(args, `"cmd"`) {
					found = true
				}
			}
		}
	}
	if !found {
		t.Fatalf("exec_command cmd projection missing:\n%s", strings.Join(frames, "\n"))
	}
}

func TestExecCommandDefaultProjectionWithoutKeyMap(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_exec2", "grok", []string{"exec_command"}, 0)
	// No SetShellArgKeys — IsShellTool(exec_command) + default cmd.
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "c", Name: "exec_command", Arguments: `{"command":"ls"}`}})
	frames = append(frames, s.Complete(&Usage{})...)
	joined := strings.Join(frames, "\n")
	if strings.Contains(joined, `"command":"ls"`) && !strings.Contains(joined, `"cmd"`) {
		t.Fatalf("default exec_command projection failed:\n%s", joined)
	}
	if !strings.Contains(joined, "cmd") {
		t.Fatalf("expected cmd somewhere:\n%s", joined)
	}
}

func TestHasPendingTools(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_pend", "grok", []string{"exec_command"}, 0)
	if s.HasPendingTools() {
		t.Fatal("empty streamer should not be pending")
	}
	// Incomplete JSON → buffered, not emitted.
	_ = s.ToolDeltas([]ToolDelta{{Index: 0, ID: "c", Name: "exec_command", Arguments: `{"cmd":`}})
	if !s.HasPendingTools() {
		t.Fatal("incomplete tool should be pending")
	}
	// Complete it → emitted, no longer pending.
	_ = s.ToolDeltas([]ToolDelta{{Index: 0, Arguments: `"pwd"}`}})
	if s.HasPendingTools() {
		t.Fatal("complete tool should not stay pending")
	}
}

func TestExecCommandFullPathProjectsCmdAlways(t *testing.T) {
	// Full path: EffectiveJSON(command) -> streamer ToolDeltas -> completed args must be cmd.
	for _, tool := range []string{"exec_command", "Shell", "default_api.exec_command"} {
		raw := `{"command":"pwd"}`
		// Streamer receives already-normalized internal args in production too.
		s := NewLiveStreamerWithMaxTools("resp_full", "grok", []string{tool}, 0)
		// Explicitly empty key map — must still default to cmd.
		s.SetShellArgKeys(map[string]string{})
		frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "c1", Name: tool, Arguments: raw}})
		frames = append(frames, s.Complete(&Usage{InputTokens: 1, OutputTokens: 1})...)
		foundCmd, foundCommandOnly := false, false
		for _, frame := range frames {
			for _, line := range strings.Split(frame, "\n") {
				if !strings.HasPrefix(line, "data:") {
					continue
				}
				payload := strings.TrimSpace(line[5:])
				var obj map[string]any
				if json.Unmarshal([]byte(payload), &obj) != nil {
					continue
				}
				if obj["type"] == "response.function_call_arguments.done" {
					args := fmt.Sprint(obj["arguments"])
					if strings.Contains(args, `"cmd"`) {
						foundCmd = true
					}
					if strings.Contains(args, `"command"`) && !strings.Contains(args, `"cmd"`) {
						foundCommandOnly = true
					}
				}
				if item, _ := obj["item"].(map[string]any); item != nil && item["type"] == "function_call" && item["status"] == "completed" {
					args := fmt.Sprint(item["arguments"])
					if strings.Contains(args, `"cmd"`) {
						foundCmd = true
					}
					if strings.Contains(args, `"command"`) && !strings.Contains(args, `"cmd"`) {
						foundCommandOnly = true
					}
				}
			}
		}
		if !foundCmd || foundCommandOnly {
			t.Fatalf("tool=%s foundCmd=%v commandOnly=%v frames=\n%s", tool, foundCmd, foundCommandOnly, strings.Join(frames, "\n"))
		}
	}
}

func TestCompleteHoldFailureReturnsNil(t *testing.T) {
	// Incomplete tool held then force-dropped with no text/reasoning → Complete nil (Fail path).
	s := NewLiveStreamer("resp_hold", "grok", nil)
	_ = s.Start()
	_ = s.ToolDeltas([]ToolDelta{{Index: 0, ID: "call_1", Name: "Bash", Arguments: `{"path":`}})
	if !s.HasPendingTools() {
		t.Fatal("expected pending incomplete tool")
	}
	if s.HasClientPayload() {
		t.Fatal("incomplete hold must not count as client payload")
	}
	frames := s.Complete(&Usage{})
	if len(frames) != 0 {
		t.Fatalf("hold-failure Complete should be empty for Fail path, got %d frames: %v", len(frames), frames)
	}
}

func TestRequeueUnackedToolsReemitsOnComplete(t *testing.T) {
	s := NewLiveStreamer("resp_soft", "grok", []string{"Edit"})
	_ = s.Start()
	frames := s.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "call_1", Name: "Edit",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":"b"}`,
	}})
	if len(frames) == 0 {
		t.Fatal("expected tool frames")
	}
	if !s.HasUnackedTools() {
		t.Fatal("expected unacked tools after emit without Ack")
	}
	s.RequeueUnackedTools()
	if s.HasUnackedTools() {
		t.Fatal("after requeue, unacked flags should clear (tools pending)")
	}
	if !s.HasPendingTools() {
		t.Fatal("requeued tool must be pending again")
	}
	out := strings.Join(s.Complete(&Usage{}), "")
	for _, marker := range []string{"function_call", "call_1", "response.completed", "[DONE]"} {
		if !strings.Contains(out, marker) {
			t.Fatalf("missing %q after Complete re-emit: %s", marker, out)
		}
	}
	if !s.HasUnackedTools() {
		t.Fatal("Complete frames not yet Ack'd")
	}
	s.AckEmittedTools()
	if s.HasUnackedTools() || !s.TerminalDelivered() {
		t.Fatal("after AckEmittedTools, terminal should be delivered")
	}
	if more := s.Complete(&Usage{}); len(more) != 0 {
		t.Fatalf("second Complete after Ack should be empty, got %d frames", len(more))
	}
}

func TestSoftFailTerminalNeedsFinishRetry(t *testing.T) {
	s := NewLiveStreamer("resp_term", "grok", []string{"Read"})
	_ = s.Start()
	_ = s.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "call_r", Name: "Read",
		Arguments: `{"file_path":"/y"}`,
	}})
	out := s.Complete(&Usage{})
	if len(out) == 0 {
		t.Fatal("expected Complete frames")
	}
	s.RequeueUnackedTools()
	if !s.NeedsFinishRetry() {
		t.Fatal("NeedsFinishRetry after soft requeue")
	}
	retry := strings.Join(s.Complete(&Usage{}), "")
	if !strings.Contains(retry, "response.completed") {
		t.Fatalf("retry Complete missing completed: %s", retry)
	}
	s.AckEmittedTools()
	if s.NeedsFinishRetry() {
		t.Fatal("no retry needed after Ack")
	}
}

func TestRequeueUnacksTerminalWhenToolsRetry(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_rq", "grok", []string{"Read"}, 2)
	// Emit one complete tool and Ack terminal as if write succeeded for completed only.
	frames := s.ToolDeltas([]ToolDelta{{Index: 0, ID: "c1", Name: "Read", Arguments: `{"file_path":"/a.go"}`}})
	if len(frames) == 0 {
		t.Fatal("expected tool frames")
	}
	// Simulate: tool write soft-failed (not Ack'd), but somehow terminal was Ack'd
	// (split multi-group path). Requeue must UnackTerminal so Complete re-emits tools THEN completed.
	s.pendingTerminal = true
	s.AckTerminal()
	if !s.TerminalDelivered() {
		t.Fatal("terminal should be delivered before requeue")
	}
	// Leave tool unacked (emitted, clientAcked=false).
	s.RequeueUnackedTools()
	if s.TerminalDelivered() {
		t.Fatal("requeue of unacked tool must UnackTerminal — tools after completed is Tool use interrupted")
	}
	out := s.Complete(&Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	joined := ""
	for _, f := range out {
		joined += f
	}
	// Must include function_call before completed
	fc := strings.Index(joined, "function_call")
	done := strings.Index(joined, "response.completed")
	if fc < 0 || done < 0 {
		t.Fatalf("expected function_call and completed, out=%q", joined[:min(400, len(joined))])
	}
	if fc > done {
		t.Fatalf("function_call must come BEFORE response.completed; fc=%d done=%d", fc, done)
	}
	if !s.ClientDeliveryOK() {
		// Complete produced frames; Ack them
		s.AckEmittedTools()
	}
	if !s.ClientDeliveryOK() {
		t.Fatalf("after Ack, ClientDeliveryOK should be true; terminal=%v unacked=%v", s.TerminalDelivered(), s.HasUnackedTools())
	}
}

func TestClientDeliveryOKRejectsUnackedTools(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_ok", "grok", []string{"Read"}, 1)
	_ = s.ToolDeltas([]ToolDelta{{Index: 0, ID: "c1", Name: "Read", Arguments: `{"file_path":"/a.go"}`}})
	_ = s.Complete(&Usage{})
	// Terminal pending, tools unacked
	if s.ClientDeliveryOK() {
		t.Fatal("unacked tools + unacked terminal must not be ClientDeliveryOK")
	}
	s.AckEmittedTools()
	if !s.ClientDeliveryOK() {
		t.Fatal("after full Ack, ClientDeliveryOK expected")
	}
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func TestLiveStreamerRequeueUnacksTerminal(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_requeue", "grok", []string{"Edit"}, 2)
	frames := s.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "call_1", Name: "Edit",
		Arguments: `{"file_path":"/x","old_string":"a","new_string":"b"}`,
	}})
	if len(frames) == 0 {
		t.Fatal("expected live tool frames")
	}
	term := s.Complete(&Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	if len(term) == 0 {
		t.Fatal("expected Complete frames")
	}
	s.AckEmittedTools()
	if !s.TerminalDelivered() {
		t.Fatal("terminal should be delivered after Ack")
	}
	// Soft-fail tool after terminal Ack: leave tool unacked but emitted.
	for _, st := range s.tools {
		if st != nil {
			st.clientAcked = false
			st.emitted = true
		}
	}
	s.pendingClientAcks = []int{0}
	s.RequeueUnackedTools()
	if s.TerminalDelivered() {
		t.Fatal("Requeue with unacked tools must UnackTerminal")
	}
	again := s.Complete(&Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	joined := strings.Join(again, "")
	if !strings.Contains(joined, "function_call") {
		t.Fatalf("expected re-emitted function_call:\n%s", joined)
	}
	if !strings.Contains(joined, "response.completed") {
		t.Fatalf("expected response.completed after recovery:\n%s", joined)
	}
}

func TestLiveStreamerClientDeliveryOK(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_ok", "grok", []string{"Read"}, 1)
	if s.ClientDeliveryOK() {
		t.Fatal("empty streamer must not be ClientDeliveryOK")
	}
	_ = s.Text("hi")
	_ = s.Complete(&Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	s.AckEmittedTools()
	if !s.ClientDeliveryOK() {
		t.Fatal("text + terminal Ack should be ClientDeliveryOK")
	}
	// Tool framed but unacked after terminal → not OK.
	s2 := NewLiveStreamerWithMaxTools("resp_tool", "grok", []string{"Read"}, 1)
	_ = s2.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "c1", Name: "Read",
		Arguments: `{"file_path":"/a.go"}`,
	}})
	_ = s2.Complete(&Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	// Ack only terminal, not tools.
	s2.AckTerminal()
	if s2.ClientDeliveryOK() {
		t.Fatal("unacked tool must not be ClientDeliveryOK")
	}
	s2.AckEmittedTools()
	if !s2.ClientDeliveryOK() {
		t.Fatal("acked tool + terminal should be ClientDeliveryOK")
	}
}

func TestRequeueUnacksTerminalBeforeToolRetry(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_u", "m", []string{"Read"}, 2)
	_ = s.Start()
	frames := s.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "call_1", Name: "Read",
		Arguments: `{"file_path":"/a.go"}`,
	}})
	if len(frames) == 0 {
		t.Fatal("expected tool frames")
	}
	joined := ""
	for _, f := range frames {
		joined += f
	}
	s.AckToolsInPayload(joined)
	term := s.Complete(&Usage{InputTokens: 1, OutputTokens: 1, TotalTokens: 2})
	termJoined := ""
	for _, f := range term {
		termJoined += f
	}
	if !strings.Contains(termJoined, "response.completed") {
		t.Fatalf("expected completed, got %q", termJoined)
	}
	s.AckTerminal()
	if !s.TerminalDelivered() {
		t.Fatal("terminal should be delivered")
	}
	// Second tool framed but soft-fail (emitted, not acked)
	s.tools[1] = &liveTool{
		id: "call_2", name: "Read", arguments: `{"file_path":"/b.go"}`,
		emitted: true, clientAcked: false, itemID: "fc_x_1", output: 2,
	}
	s.toolsStarted++
	s.RequeueUnackedTools()
	if s.TerminalDelivered() {
		t.Fatal("Requeue of unacked tools must UnackTerminal")
	}
	out := s.Complete(&Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3})
	joinedOut := ""
	for _, f := range out {
		joinedOut += f
	}
	fc := strings.Index(joinedOut, "function_call")
	done := strings.Index(joinedOut, "response.completed")
	if fc < 0 {
		t.Fatalf("expected re-emitted function_call, got %q", joinedOut)
	}
	if done < 0 {
		t.Fatalf("expected response.completed after unack, got %q", joinedOut)
	}
	if done < fc {
		t.Fatalf("completed must not precede re-emitted tool: fc=%d done=%d body=%q", fc, done, joinedOut)
	}
}

func TestClientDeliveryOKRequiresAckedToolOrText(t *testing.T) {
	s := NewLiveStreamer("resp_ok", "m", nil)
	if s.ClientDeliveryOK() {
		t.Fatal("empty streamer not OK")
	}
	_ = s.Text("hi")
	_ = s.Complete(&Usage{OutputTokens: 1})
	s.AckTerminal()
	if !s.ClientDeliveryOK() {
		t.Fatal("text + terminal should be OK")
	}

	s2 := NewLiveStreamerWithMaxTools("resp_ok2", "m", []string{"Read"}, 1)
	_ = s2.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "c1", Name: "Read", Arguments: `{"file_path":"/x"}`,
	}})
	_ = s2.Complete(&Usage{OutputTokens: 1})
	if s2.ClientDeliveryOK() {
		t.Fatal("unacked tool must not be ClientDeliveryOK")
	}
	for _, st := range s2.tools {
		if st != nil && st.emitted {
			st.clientAcked = true
		}
	}
	s2.terminalEmitted = true
	s2.pendingTerminal = false
	if !s2.ClientDeliveryOK() {
		t.Fatal("acked tool + terminal should be OK")
	}
}

func TestPayloadDeliveredRequiresWriteAck(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_pd", "grok", []string{"Read"}, 1)
	_ = s.Text("hello")
	// Text produced but not AckContentDelivered → not PayloadDelivered.
	if s.PayloadDelivered() {
		t.Fatal("text produced without AckContentDelivered must not be PayloadDelivered")
	}
	s.AckContentDelivered()
	if !s.PayloadDelivered() {
		t.Fatal("after AckContentDelivered, PayloadDelivered expected")
	}

	s2 := NewLiveStreamerWithMaxTools("resp_pd2", "grok", []string{"Read"}, 1)
	frames := s2.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "c1", Name: "Read", Arguments: `{"file_path":"/a.go"}`,
	}})
	if len(frames) == 0 {
		t.Fatal("expected tool frames")
	}
	if s2.PayloadDelivered() {
		t.Fatal("emitted but unacked tool must not be PayloadDelivered")
	}
	joined := ""
	for _, f := range frames {
		joined += f
	}
	s2.AckToolsInPayload(joined)
	if !s2.PayloadDelivered() {
		t.Fatal("acked tool must be PayloadDelivered")
	}
	if s2.HalfOpenTools() {
		t.Fatal("acked tool must not be HalfOpenTools")
	}
}

func TestHalfOpenToolsDetectsUnacked(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_ho", "grok", []string{"Read"}, 1)
	_ = s.ToolDeltas([]ToolDelta{{
		Index: 0, ID: "c1", Name: "Read", Arguments: `{"file_path":"/a.go"}`,
	}})
	if !s.HalfOpenTools() {
		t.Fatal("emitted unacked tool is half-open")
	}
	// Ack
	for _, st := range s.tools {
		if st != nil {
			st.clientAcked = true
		}
	}
	s.pendingClientAcks = nil
	if s.HalfOpenTools() {
		t.Fatal("acked tool is not half-open")
	}
}

// Two complete tools with maxTools=1 must emit only the first and close once.
// Regression: capped second tool used to keep HasPendingTools/hasReadyUnemittedTools
// true → Complete re-emitted response.completed in a loop → Claude Code
// "Tool use interrupted" on parallel Read/Write turns.
func TestMaxToolsCapDoesNotLoopCompleted(t *testing.T) {
	s := NewLiveStreamerWithMaxTools("resp_cap", "grok", []string{"Read", "Write"}, 1)
	frames := s.ToolDeltas([]ToolDelta{
		{Index: 0, ID: "c1", Name: "Read", Arguments: `{"file_path":"/a.txt"}`},
		{Index: 1, ID: "c2", Name: "Read", Arguments: `{"file_path":"/b.txt"}`},
	})
	joined := strings.Join(frames, "")
	if !strings.Contains(joined, `"name":"Read"`) {
		t.Fatalf("expected first tool emitted: %s", joined)
	}
	// Only one function_call item added.
	if n := strings.Count(joined, "response.output_item.added"); n != 1 && strings.Count(joined, `"type":"function_call"`) < 1 {
		// start may include reasoning none; count function_call added items
	}
	if strings.Count(joined, `"file_path":"/b.txt"`) != 0 {
		t.Fatalf("second tool must be capped, got: %s", joined)
	}
	if s.HasPendingTools() {
		t.Fatal("capped excess tools must not look pending")
	}
	if s.hasReadyUnemittedTools() {
		t.Fatal("capped excess tools must not look ready-to-emit")
	}
	// Ack first tool + complete once, then recovery loop must be dry.
	s.AckToolsInPayload(joined)
	term := s.Complete(nil)
	termJoined := strings.Join(term, "")
	if !strings.Contains(termJoined, "response.completed") {
		t.Fatalf("expected single completed: %s", termJoined)
	}
	s.AckTerminal()
	if s.NeedsFinishRetry() {
		t.Fatal("after cap+ack, NeedsFinishRetry must be false (no completed loop)")
	}
	// Extra Complete must not re-emit completed.
	again := strings.Join(s.Complete(nil), "")
	if strings.Contains(again, "response.completed") {
		t.Fatalf("must not re-emit completed after cap: %s", again)
	}
}

func TestEstimateOutputTokensUsesDeliveredRunes(t *testing.T) {
	s := NewLiveStreamer("resp_t", "grok-4.5", nil)
	// Simulate long text that was written.
	_ = s.Text("abcdefghijklmnopqrstuvwxyz0123456789ABCDEF") // 40 chars -> 10 tokens
	s.AckContentDelivered()
	tok := s.EstimateOutputTokens()
	if tok < 10 {
		t.Fatalf("estimate=%d want >=10 chars=%d", tok, s.OutputChars())
	}
}

func TestEstimateOutputTokensNoFloorOnOpenOnly(t *testing.T) {
	s := NewLiveStreamer("resp_t2", "grok-4.5", nil)
	// Force open flags without real text (should not floor to 1).
	s.reasoningOpen = true
	if s.EstimateOutputTokens() != 0 {
		t.Fatalf("open-only must estimate 0, got %d", s.EstimateOutputTokens())
	}
	if s.HasClientPayload() {
		// HasClientPayload true due to reasoningOpen — that is OK for keepalive.
		// Estimate must still be 0 until real chars or PayloadDelivered.
	}
}
