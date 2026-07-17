package anthropic

import (
	"encoding/json"
	"strings"
)

func EstimateTokens(text string) int {
	if text == "" {
		return 0
	}
	chars := len([]rune(text))
	if chars == 0 {
		return 0
	}
	return (chars + 3) / 4
}

func CountTokensForRequest(raw map[string]any) map[string]any {
	total := 0
	if system, ok := raw["system"]; ok && system != nil {
		total += EstimateTokens(asText(system))
	}
	if messages, ok := raw["messages"].([]any); ok {
		for _, item := range messages {
			message, ok := item.(map[string]any)
			if !ok {
				continue
			}
			content := message["content"]
			total += EstimateTokens(asText(content))
			if blocks, ok := content.([]any); ok {
				for _, block := range blocks {
					b, ok := block.(map[string]any)
					if !ok || strings.ToLower(stringValue(b["type"])) != "tool_use" {
						continue
					}
					total += EstimateTokens(stringValue(b["name"]))
					total += EstimateTokens(jsonString(b["input"], map[string]any{}))
				}
			}
		}
	}
	if tools, ok := raw["tools"].([]any); ok {
		for _, item := range tools {
			tool, ok := item.(map[string]any)
			if !ok {
				continue
			}
			total += EstimateTokens(stringValue(tool["name"]))
			total += EstimateTokens(stringValue(tool["description"]))
			schema := tool["input_schema"]
			if schema == nil {
				schema = tool["parameters"]
			}
			if schema != nil {
				total += EstimateTokens(jsonString(schema, nil))
			}
		}
	}
	return map[string]any{"input_tokens": total}
}

func HasMessagesOrSystem(raw map[string]any) bool {
	if system, ok := raw["system"]; ok && system != nil {
		return true
	}
	if messages, ok := raw["messages"].([]any); ok && len(messages) > 0 {
		return true
	}
	return false
}

func asText(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, block := range value {
			switch b := block.(type) {
			case string:
				parts = append(parts, b)
			case map[string]any:
				blockType := strings.ToLower(stringValue(b["type"]))
				if text := stringValue(b["text"]); text != "" {
					parts = append(parts, text)
					continue
				}
				if blockType == "thinking" {
					if thinking := stringValue(b["thinking"]); thinking != "" {
						parts = append(parts, thinking)
					}
					continue
				}
				if blockType == "tool_result" {
					parts = append(parts, toolResultToText(b))
				}
			}
		}
		return strings.Join(parts, "\n")
	case map[string]any:
		if text := stringValue(value["text"]); text != "" {
			return text
		}
		return jsonString(value, nil)
	default:
		return stringValue(value)
	}
}

func toolResultToText(block map[string]any) string {
	content := block["content"]
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		return asText(value)
	default:
		return jsonString(value, nil)
	}
}

func jsonString(value any, fallback any) string {
	if value == nil {
		value = fallback
	}
	if value == nil {
		return ""
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return pythonJSONSpacing(string(encoded))
}

func pythonJSONSpacing(encoded string) string {
	var builder strings.Builder
	builder.Grow(len(encoded))
	inString := false
	escaped := false
	for _, ch := range encoded {
		builder.WriteRune(ch)
		if escaped {
			escaped = false
			continue
		}
		if ch == '\\' && inString {
			escaped = true
			continue
		}
		if ch == '"' {
			inString = !inString
			continue
		}
		if !inString && (ch == ':' || ch == ',') {
			builder.WriteByte(' ')
		}
	}
	return builder.String()
}

func stringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	case json.Number:
		return v.String()
	default:
		return strings.TrimSpace(jsonString(v, nil))
	}
}
