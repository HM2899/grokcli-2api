package anthropic

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

func BuildOpenAIChatBody(raw map[string]any, model string) (map[string]any, error) {
	messages, _ := raw["messages"].([]any)
	body := map[string]any{
		"model":      model,
		"messages":   MessagesToOpenAI(messages, raw["system"]),
		"stream":     boolValue(raw["stream"]),
		"max_tokens": raw["max_tokens"],
	}
	if tools := ToolsToOpenAI(raw["tools"]); len(tools) > 0 {
		body["tools"] = tools
		if tc := ToolChoiceToOpenAI(raw["tool_choice"]); tc != nil {
			body["tool_choice"] = tc
		}
	}
	copyIfPresent(body, raw, "temperature")
	copyIfPresent(body, raw, "top_p")
	if stops, ok := raw["stop_sequences"].([]any); ok && len(stops) > 0 {
		body["stop"] = stops
	}
	if metadata, ok := raw["metadata"].(map[string]any); ok {
		if user := stringValue(metadata["user_id"]); user != "" {
			body["user"] = user
		}
	}
	if pck := ExtractPromptCacheKey(raw); pck != "" {
		body["prompt_cache_key"] = pck
	}
	if effort := ThinkingToReasoningEffort(raw["thinking"]); effort != "" {
		body["reasoning_effort"] = effort
	}
	if boolValue(body["stream"]) {
		opts, _ := body["stream_options"].(map[string]any)
		if opts == nil {
			opts = map[string]any{}
		}
		opts["include_usage"] = true
		body["stream_options"] = opts
	}
	return body, nil
}

// ToolsToOpenAI converts Anthropic tools into OpenAI function tools.
// Sorting by tool name keeps multi-turn prompt prefixes byte-stable when
// clients reshuffle the tools array (important for sticky affinity).
func ToolsToOpenAI(tools any) []any {
	items, ok := tools.([]any)
	if !ok || len(items) == 0 {
		return nil
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		if fn, ok := tool["function"].(map[string]any); ok {
			name := firstNonEmptyString(fn["name"], tool["name"])
			if name == "" {
				continue
			}
			outFn := cloneMap(fn)
			outFn["name"] = name
			rawParams := firstNonNil(outFn["parameters"], outFn["input_schema"], tool["parameters"], tool["input_schema"])
			outFn["parameters"] = ensureToolParameters(rawParams)
			delete(outFn, "input_schema")
			if outFn["description"] == nil && tool["description"] != nil {
				outFn["description"] = tool["description"]
			}
			out = append(out, map[string]any{"type": "function", "function": outFn})
			continue
		}
		name := stringValue(tool["name"])
		if name == "" {
			continue
		}
		outFn := map[string]any{"name": name}
		if tool["description"] != nil {
			outFn["description"] = tool["description"]
		}
		outFn["parameters"] = ensureToolParameters(firstNonNil(tool["input_schema"], tool["parameters"]))
		out = append(out, map[string]any{"type": "function", "function": outFn})
	}
	if len(out) == 0 {
		return nil
	}
	sort.SliceStable(out, func(i, j int) bool {
		return toolNameKey(out[i]) < toolNameKey(out[j])
	})
	converted := make([]any, 0, len(out))
	for _, tool := range out {
		converted = append(converted, tool)
	}
	return converted
}

func ensureToolParameters(params any) map[string]any {
	if params == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	if text, ok := params.(string); ok {
		text = strings.TrimSpace(text)
		if text == "" {
			return map[string]any{"type": "object", "properties": map[string]any{}}
		}
		var parsed any
		if err := json.Unmarshal([]byte(text), &parsed); err != nil {
			return map[string]any{"type": "object", "properties": map[string]any{}}
		}
		return ensureToolParameters(parsed)
	}
	input, ok := params.(map[string]any)
	if !ok {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	out := cloneMap(input)
	if out["type"] == nil {
		out["type"] = "object"
	}
	if out["type"] == "object" && out["properties"] == nil {
		out["properties"] = map[string]any{}
	}
	return out
}

func toolNameKey(tool map[string]any) string {
	fn, _ := tool["function"].(map[string]any)
	if fn != nil {
		return strings.ToLower(stringValue(fn["name"]))
	}
	return strings.ToLower(stringValue(tool["name"]))
}

// ExtractPromptCacheKey derives a sticky cache key from Anthropic cache_control
// markers or metadata. Upstream Grok ignores Anthropic prompt caching, but
// multi-turn Claude Code sessions still benefit from sticky account routing.
func ExtractPromptCacheKey(raw map[string]any) string {
	if raw == nil {
		return ""
	}
	if meta, ok := raw["metadata"].(map[string]any); ok {
		for _, key := range []string{"prompt_cache_key", "promptCacheKey", "cache_key", "cacheKey", "session_id", "sessionId", "user_id"} {
			if value := stringValue(meta[key]); value != "" {
				if len(value) > 240 {
					return value[:240]
				}
				return value
			}
		}
	}
	pieces := make([]string, 0, 12)
	switch system := raw["system"].(type) {
	case []any:
		for _, item := range system {
			block, ok := item.(map[string]any)
			if !ok {
				continue
			}
			if mark := cacheControlFingerprint(block); mark != "" {
				text := stringValue(block["text"])
				if len(text) > 120 {
					text = text[:120]
				}
				pieces = append(pieces, "sys:"+mark+":"+text)
			}
		}
	case map[string]any:
		if mark := cacheControlFingerprint(system); mark != "" {
			text := stringValue(system["text"])
			if len(text) > 120 {
				text = text[:120]
			}
			pieces = append(pieces, "sys:"+mark+":"+text)
		}
	}
	if tools, ok := raw["tools"].([]any); ok {
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			mark := cacheControlFingerprint(tool)
			if mark == "" {
				continue
			}
			name := stringValue(tool["name"])
			if name == "" {
				if fn, ok := tool["function"].(map[string]any); ok {
					name = stringValue(fn["name"])
				}
			}
			if len(name) > 80 {
				name = name[:80]
			}
			pieces = append(pieces, "tool:"+mark+":"+name)
		}
	}
	if messages, ok := raw["messages"].([]any); ok {
		limit := len(messages)
		if limit > 4 {
			limit = 4
		}
		for _, item := range messages[:limit] {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content, ok := message["content"].([]any)
			if !ok {
				continue
			}
			for _, blockItem := range content {
				block, ok := blockItem.(map[string]any)
				if !ok {
					continue
				}
				mark := cacheControlFingerprint(block)
				if mark == "" {
					continue
				}
				btype := stringValue(block["type"])
				if len(btype) > 32 {
					btype = btype[:32]
				}
				pieces = append(pieces, "msg:"+mark+":"+btype)
				if len(pieces) >= 12 {
					break
				}
			}
			if len(pieces) >= 12 {
				break
			}
		}
	}
	if len(pieces) == 0 {
		return ""
	}
	sum := sha256.Sum256([]byte(strings.Join(pieces, "|")))
	return "acc:" + hex.EncodeToString(sum[:16])
}

func cacheControlFingerprint(block map[string]any) string {
	if block == nil {
		return ""
	}
	cc := block["cache_control"]
	switch value := cc.(type) {
	case nil:
		return ""
	case bool:
		if value {
			return "cc:1"
		}
		return ""
	case string:
		text := strings.TrimSpace(value)
		if text == "" {
			return ""
		}
		if len(text) > 40 {
			text = text[:40]
		}
		return "cc:" + text
	case map[string]any:
		ctype := strings.TrimSpace(stringValue(value["type"]))
		if ctype == "" {
			ctype = "ephemeral"
		}
		if ttl := stringValue(value["ttl"]); ttl != "" {
			if len(ttl) > 24 {
				ttl = ttl[:24]
			}
			return "cc:" + ctype + ":" + ttl
		}
		return "cc:" + ctype
	default:
		return "cc:1"
	}
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func MessagesToOpenAI(messages []any, system any) []map[string]any {
	out := make([]map[string]any, 0, len(messages)+1)
	if system != nil {
		if text := strings.TrimSpace(asText(system)); text != "" {
			out = append(out, map[string]any{"role": "system", "content": text})
		}
	}
	for _, item := range messages {
		raw, ok := item.(map[string]any)
		if !ok {
			continue
		}
		role := strings.ToLower(strings.TrimSpace(stringValue(firstNonNil(raw["role"], "user"))))
		content := raw["content"]
		switch role {
		case "user":
			out = append(out, userContentMessages(content)...)
		case "assistant":
			out = append(out, assistantMessage(content))
		case "system", "developer":
			if text := strings.TrimSpace(asText(content)); text != "" {
				out = append(out, map[string]any{"role": "system", "content": text})
			}
		default:
			out = append(out, map[string]any{"role": "user", "content": asText(content)})
		}
	}
	return out
}

func userContentMessages(content any) []map[string]any {
	blocks, ok := content.([]any)
	if !ok {
		return []map[string]any{{"role": "user", "content": userContentToOpenAI(content)}}
	}
	out := []map[string]any{}
	pending := []any{}
	flush := func() {
		if len(pending) == 0 {
			return
		}
		out = append(out, map[string]any{"role": "user", "content": userContentToOpenAI(pending)})
		pending = nil
	}
	for _, item := range blocks {
		block, ok := item.(map[string]any)
		if !ok || strings.ToLower(stringValue(block["type"])) != "tool_result" {
			pending = append(pending, item)
			continue
		}
		flush()
		toolID := firstNonEmptyString(block["tool_use_id"], block["tool_call_id"], block["id"])
		out = append(out, map[string]any{"role": "tool", "tool_call_id": toolID, "content": toolResultToText(block)})
	}
	flush()
	return out
}

func userContentToOpenAI(content any) any {
	if content == nil {
		return ""
	}
	if text, ok := content.(string); ok {
		return text
	}
	blocks, ok := content.([]any)
	if !ok {
		return asText(content)
	}
	parts := []any{}
	hasNonText := false
	for _, item := range blocks {
		switch block := item.(type) {
		case string:
			parts = append(parts, map[string]any{"type": "text", "text": block})
		case map[string]any:
			blockType := strings.ToLower(stringValue(firstNonNil(block["type"], "text")))
			switch blockType {
			case "text", "input_text":
				if text := firstNonEmptyString(block["text"], block["content"]); text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			case "image":
				if image := imageToOpenAI(block); image != nil {
					hasNonText = true
					parts = append(parts, image)
				}
			case "tool_result":
				continue
			default:
				if text := firstNonEmptyString(block["text"], block["title"]); text != "" {
					parts = append(parts, map[string]any{"type": "text", "text": text})
				}
			}
		}
	}
	if len(parts) == 0 {
		return ""
	}
	if !hasNonText {
		texts := make([]string, 0, len(parts))
		for _, part := range parts {
			p, _ := part.(map[string]any)
			if p == nil || p["type"] != "text" {
				return parts
			}
			texts = append(texts, stringValue(p["text"]))
		}
		return strings.Join(texts, "\n")
	}
	return parts
}

func assistantMessage(content any) map[string]any {
	textParts := []string{}
	thinkingParts := []string{}
	toolCalls := []any{}
	if text, ok := content.(string); ok {
		textParts = append(textParts, text)
	} else if blocks, ok := content.([]any); ok {
		for _, item := range blocks {
			block, ok := item.(map[string]any)
			if !ok {
				if text, ok := item.(string); ok {
					textParts = append(textParts, text)
				}
				continue
			}
			switch strings.ToLower(stringValue(firstNonNil(block["type"], "text"))) {
			case "text", "output_text":
				textParts = append(textParts, stringValue(block["text"]))
			case "thinking":
				thinkingParts = append(thinkingParts, stringValue(block["thinking"]))
			case "tool_use":
				name := stringValue(block["name"])
				arguments := "{}"
				if raw, ok := block["input"].(string); ok {
					arguments = raw
				} else if block["input"] != nil {
					arguments = jsonString(block["input"], map[string]any{})
				}
				toolID := stringValue(block["id"])
				if toolID == "" {
					toolID = "toolu_go_" + fmt.Sprint(len(toolCalls))
				}
				toolCalls = append(toolCalls, map[string]any{"id": toolID, "type": "function", "function": map[string]any{"name": name, "arguments": arguments}})
			}
		}
	} else {
		textParts = append(textParts, asText(content))
	}
	msg := map[string]any{"role": "assistant"}
	joined := strings.Join(nonEmpty(textParts), "\n")
	if len(thinkingParts) > 0 {
		msg["reasoning_content"] = strings.Join(nonEmpty(thinkingParts), "\n")
	}
	if len(toolCalls) > 0 {
		msg["tool_calls"] = toolCalls
		if joined != "" {
			msg["content"] = joined
		} else {
			msg["content"] = nil
		}
	} else {
		msg["content"] = joined
	}
	return msg
}

func imageToOpenAI(block map[string]any) map[string]any {
	source, _ := block["source"].(map[string]any)
	if source == nil {
		return nil
	}
	sourceType := strings.ToLower(stringValue(source["type"]))
	switch sourceType {
	case "base64":
		media := stringValue(source["media_type"])
		if media == "" {
			media = "image/png"
		}
		return map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:" + media + ";base64," + stringValue(source["data"])}}
	case "url":
		if url := stringValue(source["url"]); url != "" {
			return map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}}
		}
	}
	return nil
}

func ToolChoiceToOpenAI(choice any) any {
	if choice == nil {
		return nil
	}
	if text, ok := choice.(string); ok {
		low := strings.ToLower(strings.TrimSpace(text))
		if low == "any" {
			return "required"
		}
		if low == "auto" || low == "none" || low == "required" {
			return low
		}
		return text
	}
	item, ok := choice.(map[string]any)
	if !ok {
		return choice
	}
	choiceType := strings.ToLower(stringValue(item["type"]))
	switch choiceType {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{"type": "function", "function": map[string]any{"name": stringValue(item["name"])}}
	case "function":
		return choice
	default:
		return choice
	}
}

func ThinkingToReasoningEffort(thinking any) string {
	switch value := thinking.(type) {
	case nil:
		return ""
	case bool:
		if value {
			return "medium"
		}
		return ""
	case string:
		low := strings.ToLower(strings.TrimSpace(value))
		switch low {
		case "", "disabled", "none", "false", "0":
			return ""
		case "enabled", "true", "1", "on":
			return "medium"
		case "low", "medium", "high", "xhigh":
			return low
		default:
			return low
		}
	case map[string]any:
		typeName := strings.ToLower(stringValue(value["type"]))
		switch typeName {
		case "", "disabled", "none":
			return ""
		case "enabled", "true", "on":
			// continue to budget mapping below
		case "low", "medium", "high", "xhigh":
			return typeName
		default:
			if effort := stringValue(value["effort"]); effort != "" {
				return strings.ToLower(effort)
			}
		}
		budget, ok := numericInt(value["budget_tokens"])
		if !ok || budget <= 0 {
			return "medium"
		}
		if budget <= 2048 {
			return "low"
		}
		if budget <= 8192 {
			return "medium"
		}
		if budget <= 48000 {
			return "high"
		}
		return "xhigh"
	default:
		return ""
	}
}

func copyIfPresent(dst, src map[string]any, key string) {
	if value, ok := src[key]; ok && value != nil {
		dst[key] = value
	}
}

func boolValue(value any) bool {
	v, _ := value.(bool)
	return v
}

func numericInt(value any) (int, bool) {
	switch v := value.(type) {
	case int:
		return v, true
	case int64:
		return int(v), true
	case float64:
		return int(v), true
	case json.Number:
		n, err := v.Int64()
		return int(n), err == nil
	default:
		return 0, false
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

func nonEmpty(items []string) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		if item != "" {
			out = append(out, item)
		}
	}
	return out
}
