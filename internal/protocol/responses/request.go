package responses

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/protocol/reasoning"
	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
)

func BuildChatBody(raw map[string]any, model string) map[string]any {
	body := map[string]any{
		"model":    model,
		"messages": InputToMessages(raw["input"], stringValue(raw["instructions"])),
		"stream":   boolValue(raw["stream"]),
	}
	if value := firstNonNil(raw["max_output_tokens"], raw["max_tokens"]); value != nil {
		body["max_tokens"] = value
	}
	if tools := ConvertTools(raw["tools"]); len(tools) > 0 {
		body["tools"] = tools
		if raw["tool_choice"] != nil {
			body["tool_choice"] = raw["tool_choice"]
		}
		if raw["parallel_tool_calls"] != nil {
			body["parallel_tool_calls"] = raw["parallel_tool_calls"]
		}
	}
	copyIfPresent(body, raw, "temperature")
	copyIfPresent(body, raw, "top_p")
	copyIfPresent(body, raw, "user")
	if effort := ReasoningEffort(raw); effort != "" {
		body["reasoning_effort"] = effort
	}
	copyIfPresent(body, raw, "prompt_cache_key")
	copyIfPresent(body, raw, "prompt_cache_retention")
	if meta, ok := raw["metadata"].(map[string]any); ok {
		body["metadata"] = meta // keep for Codex client detection / sticky hints
		if body["user"] == nil && meta["user"] != nil {
			body["user"] = meta["user"]
		}
		if body["prompt_cache_key"] == nil && meta["prompt_cache_key"] != nil {
			body["prompt_cache_key"] = meta["prompt_cache_key"]
		}
		if body["prompt_cache_retention"] == nil && meta["prompt_cache_retention"] != nil {
			body["prompt_cache_retention"] = meta["prompt_cache_retention"]
		}
	}
	return body
}

func InputToMessages(rawInput any, instructions string) []map[string]any {
	messages := []map[string]any{}
	if strings.TrimSpace(instructions) != "" {
		messages = append(messages, map[string]any{"role": "system", "content": strings.TrimSpace(instructions)})
	}
	if rawInput == nil {
		return messages
	}
	if text, ok := rawInput.(string); ok {
		if strings.TrimSpace(text) != "" {
			messages = append(messages, map[string]any{"role": "user", "content": strings.TrimSpace(text)})
		}
		return messages
	}
	items := []any{}
	switch value := rawInput.(type) {
	case map[string]any:
		items = append(items, value)
	case []any:
		items = value
	default:
		return messages
	}
	for _, item := range items {
		entry, ok := item.(map[string]any)
		if !ok {
			if text, ok := item.(string); ok && strings.TrimSpace(text) != "" {
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
			continue
		}
		typeName := strings.ToLower(stringValue(entry["type"]))
		role := strings.ToLower(stringValue(entry["role"]))
		switch {
		case typeName == "function_call_output" || typeName == "custom_tool_call_output" || typeName == "tool_result":
			callID := firstNonEmptyString(entry["call_id"], entry["tool_call_id"])
			if callID == "" {
				callID = "call_go"
			}
			output := firstNonNil(entry["output"], entry["content"])
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": callID, "content": stringify(output)})
		case typeName == "function_call" || typeName == "custom_tool_call":
			callID := firstNonEmptyString(entry["call_id"], entry["id"])
			if callID == "" {
				callID = "call_go"
			}
			name := stringValue(entry["name"])
			args := entry["arguments"]
			if typeName == "custom_tool_call" {
				args = map[string]any{"input": firstNonNil(entry["input"], entry["arguments"])}
			}
			argText, ok := args.(string)
			if !ok {
				argText = stringify(args)
			}
			call := map[string]any{"id": callID, "type": "function", "function": map[string]any{"name": name, "arguments": firstNonEmptyString(argText, "{}")}}
			if len(messages) > 0 && messages[len(messages)-1]["role"] == "assistant" && messages[len(messages)-1]["tool_calls"] != nil && strings.TrimSpace(stringValue(messages[len(messages)-1]["content"])) == "" {
				messages[len(messages)-1]["tool_calls"] = append(messages[len(messages)-1]["tool_calls"].([]any), call)
			} else {
				messages = append(messages, map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{call}})
			}
		case (typeName == "input_text" || typeName == "text") && role == "":
			if text := stringValue(entry["text"]); text != "" {
				messages = append(messages, map[string]any{"role": "user", "content": text})
			}
		case typeName == "output_text" && role == "":
			if text := stringValue(entry["text"]); text != "" {
				messages = append(messages, map[string]any{"role": "assistant", "content": text})
			}
		case typeName == "message" || role != "":
			if role == "" {
				role = "user"
			}
			content := entry["content"]
			if content == nil && entry["text"] != nil {
				content = stringValue(entry["text"])
			}
			// sub2api OpenAI test / Responses clients send:
			//   content: [{type:"input_text", text:"hi"}]
			// Upstream cli-chat-proxy rejects those parts as "Empty content block".
			// Flatten to OpenAI chat multimodal/text shape (Python parity).
			content = normalizeMessageContent(content)
			var calls []any
			if rawCalls, ok := entry["tool_calls"].([]any); ok {
				calls = rawCalls
			}
			// Drop fully empty messages (no text, no tools) — they become Empty content block upstream.
			if isEmptyMessageContent(content) && len(calls) == 0 {
				continue
			}
			msg := map[string]any{"role": role, "content": content}
			if len(calls) > 0 {
				msg["tool_calls"] = calls
				if isEmptyMessageContent(content) {
					msg["content"] = nil
				}
			}
			messages = append(messages, msg)
		}
	}
	return messages
}

// normalizeMessageContent converts Responses content parts into chat.completions
// content. Text-only input_text/output_text/text parts become a plain string so
// upstream does not see empty/unknown content blocks.
func normalizeMessageContent(content any) any {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		return multimodalContentFromParts(value)
	case map[string]any:
		typeName := strings.ToLower(stringValue(firstNonNil(value["type"], "text")))
		if typeName == "text" || typeName == "input_text" || typeName == "output_text" {
			return stringValue(value["text"])
		}
		if value["text"] != nil {
			return stringValue(value["text"])
		}
		return stringify(value)
	default:
		return stringify(value)
	}
}

func multimodalContentFromParts(parts []any) any {
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
			typeName := strings.ToLower(stringValue(item["type"]))
			switch typeName {
			case "input_text", "output_text", "text", "":
				text := stringValue(item["text"])
				// Drop empty text parts entirely — upstream rejects empty content blocks.
				if text == "" {
					continue
				}
				textOnly = append(textOnly, text)
				out = append(out, map[string]any{"type": "text", "text": text})
			case "input_image", "image", "image_url":
				anyNonText = true
				imageURL := firstNonNil(item["image_url"], item["url"], item["image"])
				if url, ok := imageURL.(string); ok && url != "" {
					out = append(out, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
				} else if urlMap, ok := imageURL.(map[string]any); ok {
					out = append(out, map[string]any{"type": "image_url", "image_url": urlMap})
				} else {
					out = append(out, cloneMap(item))
				}
			default:
				if item["text"] != nil {
					text := stringValue(item["text"])
					textOnly = append(textOnly, text)
					out = append(out, map[string]any{"type": "text", "text": text})
				} else {
					anyNonText = true
					out = append(out, cloneMap(item))
				}
			}
		default:
			if text := strings.TrimSpace(fmt.Sprint(item)); text != "" && text != "<nil>" {
				textOnly = append(textOnly, text)
				out = append(out, map[string]any{"type": "text", "text": text})
			}
		}
	}
	if len(out) == 0 {
		return ""
	}
	if !anyNonText {
		return strings.Join(textOnly, "")
	}
	return out
}

func isEmptyMessageContent(content any) bool {
	switch v := content.(type) {
	case nil:
		return true
	case string:
		return strings.TrimSpace(v) == ""
	case []any:
		return len(v) == 0
	default:
		return false
	}
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func ConvertTools(raw any) []any {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := []any{}
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		toolType := strings.ToLower(firstNonEmptyString(tool["type"], "function"))
		if toolType == "custom" {
			name := stringValue(tool["name"])
			if name == "" {
				continue
			}
			description := stringValue(tool["description"])
			const customHint = "Return the custom tool's raw payload in the input string parameter."
			if description == "" {
				description = customHint
			} else if !strings.Contains(strings.ToLower(description), "input string parameter") {
				description += " " + customHint
			}
			if format, _ := firstNonNil(tool["format"], tool["input_format"]).(map[string]any); format != nil {
				definition := firstNonEmptyString(format["definition"], format["grammar"])
				if definition != "" {
					syntax := firstNonEmptyString(format["syntax"], format["type"], "custom")
					description += "\n\nCustom input grammar (" + syntax + "):\n" + definition
				}
			}
			out = append(out, map[string]any{"type": "function", "function": map[string]any{
				"name":        name,
				"description": description,
				"parameters": map[string]any{
					"type":                 "object",
					"properties":           map[string]any{"input": map[string]any{"type": "string", "description": "Raw custom-tool input."}},
					"required":             []any{"input"},
					"additionalProperties": false,
				},
			}})
			continue
		}
		if toolType != "function" {
			continue
		}
		if _, ok := tool["function"].(map[string]any); ok {
			out = append(out, tool)
			continue
		}
		name := stringValue(tool["name"])
		if name == "" {
			continue
		}
		fn := map[string]any{"name": name}
		copyIfPresent(fn, tool, "description")
		fn["parameters"] = firstNonNil(tool["parameters"], tool["input_schema"], map[string]any{"type": "object", "properties": map[string]any{}})
		if tool["strict"] != nil {
			fn["strict"] = tool["strict"]
		}
		out = append(out, map[string]any{"type": "function", "function": fn})
	}
	return out
}

// CustomToolNameSet returns the names of Responses API free-form custom tools.
// They are converted to upstream functions, but must be projected back to
// custom_tool_call/input on the client-facing Responses boundary.
func CustomToolNameSet(raw any) map[string]bool {
	out := map[string]bool{}
	items, _ := raw.([]any)
	for _, item := range items {
		tool, _ := item.(map[string]any)
		if tool == nil || strings.ToLower(stringValue(tool["type"])) != "custom" {
			continue
		}
		name := stringValue(tool["name"])
		if name == "" {
			continue
		}
		out[name] = true
		out[strings.ToLower(name)] = true
		if key := toolcall.NameKey(name); key != "" {
			out[key] = true
		}
	}
	return out
}

func isCustomToolName(name string, names map[string]bool) bool {
	if len(names) == 0 {
		return false
	}
	return names[name] || names[strings.ToLower(name)] || names[toolcall.NameKey(name)]
}

func customToolInput(arguments any) string {
	text, ok := arguments.(string)
	if !ok {
		text = stringify(arguments)
	}
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}
	var obj map[string]any
	if json.Unmarshal([]byte(trimmed), &obj) == nil {
		for _, key := range []string{"input", "patch", "diff", "content", "text", "body"} {
			if value, exists := obj[key]; exists {
				if input, ok := value.(string); ok {
					return input
				}
				return stringify(value)
			}
		}
	}
	return text
}

// ReasoningEffort returns a Grok-safe low|medium|high for Responses bodies.
// Accepts Claude Code (low|medium|high|xhigh|max|ultracode), Codex
// (Low/Base/High/Ultra/Proactive + auto/default/standard/extra-high), and thinking budgets — then folds to 3 tiers.
func ReasoningEffort(raw map[string]any) string {
	return reasoning.FromRequestUpstream(raw)
}

func BuildObject(responseID, model, content, reasoning string, toolCalls []map[string]any, usage map[string]any, createdAt int64, previous string, metadata map[string]any) map[string]any {
	output := []any{}
	if content != "" || len(toolCalls) == 0 {
		output = append(output, map[string]any{"id": "msg_" + responseID, "type": "message", "role": "assistant", "status": "completed", "content": []any{map[string]any{"type": "output_text", "text": content}}})
	}
	for i, call := range toolCalls {
		fn, _ := call["function"].(map[string]any)
		name := firstNonEmptyString(fn["name"], call["name"])
		if name == "" {
			continue
		}
		args := firstNonEmptyString(fn["arguments"], call["arguments"], "{}")
		callID := firstNonEmptyString(call["id"], call["call_id"])
		if callID == "" {
			callID = fmt.Sprintf("call_go_%d", i)
		}
		if strings.EqualFold(firstNonEmptyString(call["type"]), "custom") {
			output = append(output, map[string]any{"id": fmt.Sprintf("ctc_%s_%d", responseID, i), "type": "custom_tool_call", "status": "completed", "call_id": callID, "name": name, "input": customToolInput(args)})
			continue
		}
		output = append(output, map[string]any{"id": fmt.Sprintf("fc_%s_%d", responseID, i), "type": "function_call", "status": "completed", "call_id": callID, "name": name, "arguments": args})
	}
	obj := map[string]any{"id": responseID, "object": "response", "created_at": createdAt, "status": "completed", "model": model, "output": output, "usage": ChatUsageToResponses(usage)}
	if previous != "" {
		obj["previous_response_id"] = previous
	}
	if metadata != nil {
		obj["metadata"] = metadata
	}
	if reasoning != "" {
		obj["x_grok2api_reasoning"] = reasoning
	}
	return obj
}

func ChatUsageToResponses(usage map[string]any) map[string]any {
	input := intValue(firstNonNil(usage["prompt_tokens"], usage["input_tokens"]))
	output := intValue(firstNonNil(usage["completion_tokens"], usage["output_tokens"]))
	total := intValue(usage["total_tokens"])
	if total == 0 {
		total = input + output
	}
	cached := intValue(usage["cached_tokens"])
	return map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": total, "input_tokens_details": map[string]any{"cached_tokens": cached}, "output_tokens_details": map[string]any{"reasoning_tokens": intValue(usage["reasoning_tokens"])}, "prompt_tokens": input, "completion_tokens": output, "prompt_tokens_details": map[string]any{"cached_tokens": cached}, "completion_tokens_details": map[string]any{"reasoning_tokens": intValue(usage["reasoning_tokens"])}, "cache_read_input_tokens": cached, "cache_creation_input_tokens": intValue(usage["cache_creation_input_tokens"])}
}

func NewResponseID() string {
	return fmt.Sprintf("resp_%d", time.Now().UnixNano())
}

func copyIfPresent(dst, src map[string]any, key string) {
	if value, ok := src[key]; ok && value != nil {
		dst[key] = value
	}
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
		if text := stringValue(value); text != "" {
			return text
		}
	}
	return ""
}

func boolValue(value any) bool {
	v, _ := value.(bool)
	return v
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func stringify(value any) string {
	if value == nil {
		return ""
	}
	if text, ok := value.(string); ok {
		return text
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return fmt.Sprint(value)
	}
	return string(encoded)
}

func intValue(value any) int {
	switch v := value.(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	case json.Number:
		n, _ := v.Int64()
		return int(n)
	default:
		return 0
	}
}
