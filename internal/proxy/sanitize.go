package proxy

import (
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"

	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"

	"github.com/hm2899/grokcli-2api/internal/protocol/historycompact"
)

var builtinSearchToolTypes = map[string]bool{
	"web_search":         true,
	"web_search_preview": true,
	"live_search":        true,
	"x_search":           true,
	"builtin_function":   true,
	"builtin":            true,
}

var unsupportedUpstreamParams = map[string]bool{
	"presence_penalty":  true,
	"frequency_penalty": true,
	"logit_bias":        true,
	"logprobs":          true,
	"top_logprobs":      true,
	"n":                 true,
	// Keep public prompt_cache_key / prompt_cache_retention for upstream
	// (CPA-style Grok prompt cache). Only strip private affinity copies.
	"_history_compact":         true,
	"_prompt_stabilize":        true,
	"_prompt_cache_key":        true,
	"_prompt_cache_key_minted": true,
	"_prompt_cache_retention":  true,
	"search_parameters":        true,
	"web_search_options":       true,
	"group":                    true,
	// Client-only metadata (Codex session/client hints). Stripped after compact
	// detection so LooksLikeCodexRequest can still see metadata.client.
	"metadata": true,
}

func SanitizeUpstreamBody(body map[string]any) map[string]any {
	out := cloneAnyMap(body)
	for key := range unsupportedUpstreamParams {
		delete(out, key)
	}
	clampFloat(out, "temperature", 0, 2)
	clampFloat(out, "top_p", 0, 1)
	for _, key := range []string{"max_tokens", "max_completion_tokens"} {
		if value, ok := out[key]; ok && !positiveInt(value) {
			delete(out, key)
		}
	}
	normalizeToolsField(out)
	normalizeFunctionsField(out)
	normalizeToolChoiceField(out)
	if out["tools"] == nil && out["functions"] == nil {
		delete(out, "tool_choice")
		delete(out, "function_call")
		delete(out, "parallel_tool_calls")
	}
	return out
}

func StabilizePromptBody(body map[string]any) map[string]any {
	if body == nil {
		return map[string]any{"messages_stabilized": 0, "tools_stabilized": 0}
	}
	stats := map[string]any{"messages_stabilized": 0, "tools_stabilized": 0, "tool_args_canonicalized": 0}
	normalizeToolsField(body)
	normalizeFunctionsField(body)
	if tools, ok := body["tools"].([]any); ok {
		stats["tools_stabilized"] = len(tools)
	}
	messages := normalizeMessageList(body["messages"])
	if len(messages) > 0 {
		stabilized := make([]any, 0, len(messages))
		for _, item := range messages {
			msg, ok := item.(map[string]any)
			if !ok {
				stabilized = append(stabilized, item)
				continue
			}
			out, canon := stabilizeMessage(msg)
			stats["tool_args_canonicalized"] = asInt(stats["tool_args_canonicalized"]) + canon
			stabilized = append(stabilized, out)
			stats["messages_stabilized"] = asInt(stats["messages_stabilized"]) + 1
		}
		body["messages"] = stabilized
	}
	body["_prompt_stabilize"] = stats
	return stats
}

// BodyPrepStats captures non-upstream metadata from request preparation.
type BodyPrepStats struct {
	Stabilize map[string]any
	Compact   map[string]any
}

// PrepareUpstreamBody stabilizes + optionally compacts history, then strips
// private keys / unsupported upstream fields.
func PrepareUpstreamBody(body map[string]any) map[string]any {
	prepared, _ := PrepareUpstreamBodyDetailed(body)
	return prepared
}

// PrepareUpstreamBodyDetailed returns the sanitized upstream body plus stabilize/compact stats.
// Optional userAgent enables Codex default auto-compact when admin auto_chars is 0.
//
// Cache-safe body prep: stabilize + optional history compact, then strip
// private keys. Do NOT inject client-specific instructions here — that would
// mutate the prompt prefix and bust Grok prompt-cache hits. Codex detection
// is used only for outbound shell key projection (cmd↔command), not for
// rewriting instructions.
func PrepareUpstreamBodyDetailed(body map[string]any, userAgent ...string) (map[string]any, BodyPrepStats) {
	out := cloneAnyMap(body)
	ua := ""
	if len(userAgent) > 0 {
		ua = userAgent[0]
	}
	// Intentionally no prompt injection (cache-safe). Codex shell key projection
	// happens only on the outbound tool_call path.
	stabilize := StabilizePromptBody(out)
	compact := historycompact.Apply(out, ua)
	return SanitizeUpstreamBody(out), BodyPrepStats{Stabilize: stabilize, Compact: compact}
}

func normalizeMessageList(raw any) []any {
	switch value := raw.(type) {
	case []any:
		return value
	case []map[string]any:
		out := make([]any, 0, len(value))
		for _, item := range value {
			out = append(out, item)
		}
		return out
	default:
		return nil
	}
}

func stabilizeMessage(msg map[string]any) (map[string]any, int) {
	role := strings.TrimSpace(stringFromAny(msg["role"]))
	if role == "" {
		role = "user"
	}
	out := map[string]any{"role": role}
	if name := stringFromAny(msg["name"]); name != "" {
		out["name"] = name
	}
	if toolCallID := stringFromAny(msg["tool_call_id"]); toolCallID != "" {
		out["tool_call_id"] = toolCallID
	}
	canonCount := 0
	if calls := toolCallsFromAny(msg["tool_calls"]); len(calls) > 0 {
		stableCalls := make([]any, 0, len(calls))
		for _, call := range calls {
			item := cloneAnyMap(call)
			delete(item, "index")
			delete(item, "extra_content")
			delete(item, "thought_signature")
			if fn, ok := item["function"].(map[string]any); ok {
				fn2 := cloneAnyMap(fn)
				toolName := firstNonEmptyString(fn2["name"], item["name"])
				if args, ok := fn2["arguments"].(string); ok {
					// Normalize shell aliases (cmd→command) for upstream Grok history.
					if isShellToolName(toolName) {
						if fixed := toolcall.NormalizeJSON(args, toolName); fixed != "" {
							args = fixed
						}
					}
					if canon := canonicalJSONText(args); canon != "" {
						fn2["arguments"] = canon
						canonCount++
					} else {
						fn2["arguments"] = args
					}
				} else if fn2["arguments"] == nil {
					fn2["arguments"] = "{}"
				} else {
					fn2["arguments"] = canonicalJSONText(fn2["arguments"])
					if fn2["arguments"] == "" {
						fn2["arguments"] = "{}"
					}
					// Shell object args: re-normalize if possible.
					if isShellToolName(toolName) {
						if fixed := toolcall.NormalizeJSON(fmt.Sprint(fn2["arguments"]), toolName); fixed != "" {
							fn2["arguments"] = fixed
						}
					}
					canonCount++
				}
				item["function"] = fn2
			}
			ordered := map[string]any{}
			if item["id"] != nil {
				ordered["id"] = item["id"]
			}
			ordered["type"] = firstNonEmptyString(item["type"], "function")
			if item["function"] != nil {
				ordered["function"] = item["function"]
			}
			for key, value := range item {
				if _, exists := ordered[key]; !exists {
					ordered[key] = value
				}
			}
			stableCalls = append(stableCalls, ordered)
		}
		out["tool_calls"] = stableCalls
	}
	if fc, ok := msg["function_call"].(map[string]any); ok && stringFromAny(fc["name"]) != "" {
		toolName := stringFromAny(fc["name"])
		fn2 := map[string]any{"name": toolName}
		if args, ok := fc["arguments"].(string); ok {
			// Normalize shell aliases (cmd→command) for upstream Grok history.
			if isShellToolName(toolName) {
				if fixed := toolcall.NormalizeJSON(args, toolName); fixed != "" {
					args = fixed
				}
			}
			if canon := canonicalJSONText(args); canon != "" {
				fn2["arguments"] = canon
			} else {
				fn2["arguments"] = args
			}
		} else if fc["arguments"] != nil {
			fn2["arguments"] = canonicalJSONText(fc["arguments"])
			if fn2["arguments"] == "" {
				fn2["arguments"] = fmt.Sprint(fc["arguments"])
			}
			if isShellToolName(toolName) {
				if fixed := toolcall.NormalizeJSON(fmt.Sprint(fn2["arguments"]), toolName); fixed != "" {
					fn2["arguments"] = fixed
				}
			}
		} else {
			fn2["arguments"] = "{}"
		}
		out["function_call"] = fn2
	}
	switch content := msg["content"].(type) {
	case nil:
		if out["tool_calls"] != nil || out["function_call"] != nil {
			out["content"] = nil
		} else {
			out["content"] = ""
		}
	case string:
		out["content"] = stabilizeTextContent(content, role)
	case []any:
		out["content"] = stabilizeContentParts(content, role)
	default:
		out["content"] = content
	}
	if rc := firstNonEmptyString(msg["reasoning_content"], msg["reasoning"]); rc != "" {
		out["reasoning_content"] = rc
	}
	return out, canonCount
}

func stabilizeContentParts(parts []any, role string) any {
	out := make([]any, 0, len(parts))
	textOnly := make([]string, 0, len(parts))
	anyNonText := false
	for _, part := range parts {
		switch item := part.(type) {
		case string:
			if item == "" {
				continue
			}
			textOnly = append(textOnly, item)
			out = append(out, map[string]any{"type": "text", "text": item})
		case map[string]any:
			typeName := strings.ToLower(stringFromAny(firstNonNil(item["type"], "text")))
			switch typeName {
			case "text", "input_text", "output_text":
				text := stringFromAny(firstNonNil(item["text"], item["content"]))
				if text == "" {
					continue
				}
				textOnly = append(textOnly, text)
				out = append(out, map[string]any{"type": "text", "text": text})
			case "image_url", "image", "input_image":
				anyNonText = true
				out = append(out, item)
			default:
				if text := stringFromAny(firstNonNil(item["text"], item["content"])); text != "" {
					textOnly = append(textOnly, text)
					out = append(out, map[string]any{"type": "text", "text": text})
				} else {
					anyNonText = true
					out = append(out, item)
				}
			}
		default:
			anyNonText = true
			out = append(out, part)
		}
	}
	if len(out) == 0 {
		return ""
	}
	if !anyNonText {
		return stabilizeTextContent(strings.Join(textOnly, "\n"), role)
	}
	return out
}

func stabilizeTextContent(text, role string) string {
	if role != "system" {
		return text
	}
	norm := strings.ReplaceAll(text, "\r\n", "\n")
	norm = strings.ReplaceAll(norm, "\r", "\n")
	hadTrailing := strings.HasSuffix(norm, "\n") || strings.HasSuffix(text, "\n")
	norm = strings.TrimRight(norm, "\n")
	norm = strings.TrimRight(norm, " \t")
	if hadTrailing && norm != "" {
		return norm + "\n"
	}
	return norm
}

func canonicalJSONText(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return "{}"
		}
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			return text
		}
		encoded, err := json.Marshal(parsed)
		if err != nil {
			return text
		}
		return string(encoded)
	default:
		encoded, err := json.Marshal(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

func asInt(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	default:
		return 0
	}
}

func normalizeToolsField(body map[string]any) {
	raw, exists := body["tools"]
	if !exists {
		return
	}
	items, ok := raw.([]any)
	if !ok {
		delete(body, "tools")
		return
	}
	cleaned := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.ToLower(strings.TrimSpace(stringFromAny(firstNonNil(tool["type"], "function"))))
		if builtinSearchToolTypes[toolType] {
			continue
		}
		if toolType != "function" && tool["type"] != nil {
			continue
		}
		if normalized := normalizeFunctionTool(tool); normalized != nil {
			cleaned = append(cleaned, normalized)
		}
	}
	if len(cleaned) == 0 {
		delete(body, "tools")
		return
	}
	sort.SliceStable(cleaned, func(i, j int) bool { return toolSortKey(cleaned[i]) < toolSortKey(cleaned[j]) })
	body["tools"] = mapsToAny(cleaned)
}

func normalizeFunctionsField(body map[string]any) {
	raw, exists := body["functions"]
	if !exists {
		return
	}
	items, ok := raw.([]any)
	if !ok {
		delete(body, "functions")
		return
	}
	fixed := make([]map[string]any, 0, len(items))
	for _, item := range items {
		fn, ok := item.(map[string]any)
		if !ok || strings.TrimSpace(stringFromAny(fn["name"])) == "" {
			continue
		}
		out := cloneAnyMap(fn)
		rawParams := firstNonNil(out["parameters"], out["input_schema"])
		out["parameters"] = ensureToolParameters(rawParams)
		delete(out, "input_schema")
		fixed = append(fixed, out)
	}
	if len(fixed) == 0 {
		delete(body, "functions")
		return
	}
	body["functions"] = mapsToAny(fixed)
}

func normalizeToolChoiceField(body map[string]any) {
	raw, exists := body["tool_choice"]
	if !exists || raw == nil {
		return
	}
	switch value := raw.(type) {
	case string:
		choice := strings.ToLower(strings.TrimSpace(value))
		if choice == "any" || choice == "tool" {
			body["tool_choice"] = "required"
		} else if choice == "" {
			delete(body, "tool_choice")
		} else {
			body["tool_choice"] = choice
		}
	case map[string]any:
		choiceType := strings.ToLower(strings.TrimSpace(stringFromAny(firstNonNil(value["type"], "function"))))
		switch {
		case builtinSearchToolTypes[choiceType]:
			body["tool_choice"] = "auto"
		case choiceType == "any" || choiceType == "tool":
			body["tool_choice"] = "required"
		case choiceType == "auto" || choiceType == "none" || choiceType == "required":
			body["tool_choice"] = choiceType
		case choiceType != "function":
			body["tool_choice"] = "auto"
		default:
			fn, _ := value["function"].(map[string]any)
			name := stringFromAny(value["name"])
			if name == "" && fn != nil {
				name = stringFromAny(fn["name"])
			}
			if name != "" {
				body["tool_choice"] = "required"
			} else {
				body["tool_choice"] = "auto"
			}
		}
	}
}

func normalizeFunctionTool(tool map[string]any) map[string]any {
	if fn, ok := tool["function"].(map[string]any); ok {
		outFn := cloneAnyMap(fn)
		name := firstNonEmptyString(outFn["name"], tool["name"])
		if name == "" {
			return nil
		}
		outFn["name"] = name
		rawParams := firstNonNil(outFn["parameters"], outFn["input_schema"], tool["parameters"], tool["input_schema"])
		outFn["parameters"] = ensureToolParameters(rawParams)
		outFn["parameters"] = hardenShellToolParameters(name, outFn["parameters"])
		delete(outFn, "input_schema")
		if outFn["description"] == nil && tool["description"] != nil {
			outFn["description"] = tool["description"]
		}
		// Nudge description so the model sticks to the schema parameter name.
		if isShellToolName(name) {
			desc := stringFromAny(outFn["description"])
			hint := " Argument is a single shell command string."
			if desc == "" {
				outFn["description"] = "Run a shell command." + hint
			} else if !strings.Contains(strings.ToLower(desc), `parameter name "command"`) && !strings.Contains(strings.ToLower(desc), "parameter name 'command'") {
				outFn["description"] = desc + hint
			}
		}
		return map[string]any{"type": "function", "function": outFn}
	}
	name := stringFromAny(tool["name"])
	if name == "" {
		return nil
	}
	outFn := map[string]any{"name": name}
	if tool["description"] != nil {
		outFn["description"] = tool["description"]
	}
	outFn["parameters"] = ensureToolParameters(firstNonNil(tool["parameters"], tool["input_schema"]))
	outFn["parameters"] = hardenShellToolParameters(name, outFn["parameters"])
	if isShellToolName(name) {
		desc := stringFromAny(outFn["description"])
		hint := " Argument is a single shell command string."
		if desc == "" {
			outFn["description"] = "Run a shell command." + hint
		} else if !strings.Contains(strings.ToLower(desc), `parameter name "command"`) {
			outFn["description"] = desc + hint
		}
	}
	return map[string]any{"type": "function", "function": outFn}
}

func isShellToolName(name string) bool {
	// Keep parity with toolcall.IsShellTool so Codex exec_command / run_command
	// get schema harden (cmd→command) and history alias rewrite.
	return toolcall.IsShellTool(name)
}

// hardenShellToolParameters forces a single canonical "command" property for
// shell-like tools so Grok stops oscillating between cmd/command/args.
func hardenShellToolParameters(name string, params any) map[string]any {
	out, ok := params.(map[string]any)
	if !ok || out == nil {
		out = emptyToolParameters()
	} else {
		out = cloneAnyMap(out)
	}
	if !isShellToolName(name) {
		return out
	}
	if out["type"] == nil {
		out["type"] = "object"
	}
	props, _ := out["properties"].(map[string]any)
	if props == nil {
		props = map[string]any{}
	} else {
		props = cloneAnyMap(props)
	}
	// Keep existing command schema if present; always force a single STRING type.
	// Array argv was allowed historically but trained Grok to emit argv that then
	// got POSIX-quoted for Codex PowerShell hosts (ParserError). String-only
	// matches Codex Desktop local schema and the gold-standard gateway.
	cmdSchema, _ := props["command"].(map[string]any)
	if cmdSchema == nil {
		// Steal description from common wrong names if any.
		desc := ""
		for _, alt := range []string{"cmd", "argv", "args", "shell_command", "cmdline", "command_line", "script"} {
			if altSchema, ok := props[alt].(map[string]any); ok {
				if d := stringFromAny(altSchema["description"]); d != "" {
					desc = d
					break
				}
			}
		}
		if desc == "" {
			desc = "Single shell command string to execute on the client host."
		}
		cmdSchema = map[string]any{
			"description": desc,
			"type":        "string",
		}
	} else {
		cmdSchema = cloneAnyMap(cmdSchema)
		// Force string-only even if the client advertised string|array.
		cmdSchema["type"] = "string"
		delete(cmdSchema, "items")
		d := stringFromAny(cmdSchema["description"])
		ld := strings.ToLower(d)
		if d == "" || strings.Contains(ld, "argv") || strings.Contains(ld, "array") {
			cmdSchema["description"] = "Single shell command string to execute on the client host."
		}
	}
	// Rebuild properties: command first, then non-conflicting extras (workdir/timeout).
	newProps := map[string]any{"command": cmdSchema}
	for k, v := range props {
		lk := strings.ToLower(strings.TrimSpace(k))
		switch lk {
		case "command", "cmd", "argv", "args", "shell_command", "cmdline", "command_line", "script", "bash", "line":
			continue // drop aliases from schema so model cannot pick them
		default:
			newProps[k] = v
		}
	}
	out["properties"] = newProps
	// required must include command and must not list aliases
	reqOut := []any{"command"}
	if rawReq, ok := out["required"].([]any); ok {
		for _, item := range rawReq {
			key := strings.ToLower(strings.TrimSpace(fmt.Sprint(item)))
			switch key {
			case "command", "cmd", "argv", "args", "shell_command", "cmdline", "command_line", "script":
				continue
			case "":
				continue
			default:
				reqOut = append(reqOut, item)
			}
		}
	}
	out["required"] = reqOut
	// Strict-ish: discourage invented keys.
	if out["additionalProperties"] == nil {
		out["additionalProperties"] = false
	}
	return out
}

func ensureToolParameters(params any) map[string]any {
	if params == nil {
		return emptyToolParameters()
	}
	if text, ok := params.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return emptyToolParameters()
		}
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			return emptyToolParameters()
		}
		return ensureToolParameters(parsed)
	}
	input, ok := params.(map[string]any)
	if !ok {
		return emptyToolParameters()
	}
	out := cloneAnyMap(input)
	if out["type"] == nil {
		out["type"] = "object"
	}
	if out["type"] == "object" && out["properties"] == nil {
		out["properties"] = map[string]any{}
	}
	return out
}

func emptyToolParameters() map[string]any {
	return map[string]any{"type": "object", "properties": map[string]any{}}
}

func toolSortKey(tool map[string]any) string {
	fn, _ := tool["function"].(map[string]any)
	if fn != nil {
		return strings.ToLower(stringFromAny(fn["name"]))
	}
	return strings.ToLower(stringFromAny(tool["name"]))
}

func clampFloat(body map[string]any, key string, minValue, maxValue float64) {
	value, ok := body[key]
	if !ok {
		return
	}
	f, ok := floatFromAny(value)
	if !ok {
		delete(body, key)
		return
	}
	if f < minValue {
		f = minValue
	}
	if f > maxValue {
		f = maxValue
	}
	body[key] = f
}

func positiveInt(value any) bool {
	switch v := value.(type) {
	case int:
		return v >= 1
	case int64:
		return v >= 1
	case float64:
		return v >= 1
	case json.Number:
		n, err := v.Int64()
		return err == nil && n >= 1
	case string:
		n, err := strconv.Atoi(strings.TrimSpace(v))
		return err == nil && n >= 1
	default:
		return false
	}
}

func floatFromAny(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(v), 64)
		return f, err == nil
	default:
		return 0, false
	}
}

func cloneAnyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func firstNonNil(values ...any) any {
	for _, value := range values {
		if value != nil {
			return value
		}
	}
	return nil
}

func firstNonEmptyString(values ...any) string {
	for _, value := range values {
		if text := stringFromAny(value); text != "" {
			return text
		}
	}
	return ""
}

func stringFromAny(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return v.String()
	default:
		return strings.TrimSpace(fmt.Sprint(v))
	}
}

func mapsToAny(items []map[string]any) []any {
	out := make([]any, 0, len(items))
	for _, item := range items {
		out = append(out, item)
	}
	return out
}
