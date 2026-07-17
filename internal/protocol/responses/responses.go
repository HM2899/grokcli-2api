package responses

import (
	"encoding/json"
	"fmt"
)

type Usage struct {
	InputTokens         int
	OutputTokens        int
	TotalTokens         int
	CachedTokens        int
	CacheCreationTokens int
	ReasoningTokens     int
}

func NormalizeUsage(usage *Usage) map[string]any {
	if usage == nil {
		usage = &Usage{}
	}
	total := usage.TotalTokens
	if total == 0 {
		total = usage.InputTokens + usage.OutputTokens
	}
	return map[string]any{
		"input_tokens":  usage.InputTokens,
		"output_tokens": usage.OutputTokens,
		"total_tokens":  total,
		"input_tokens_details": map[string]any{
			"cached_tokens": usage.CachedTokens,
		},
		"output_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
		"prompt_tokens":     usage.InputTokens,
		"completion_tokens": usage.OutputTokens,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": usage.CachedTokens,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": usage.ReasoningTokens,
		},
		"cache_read_input_tokens":     usage.CachedTokens,
		"cache_creation_input_tokens": usage.CacheCreationTokens,
	}
}

type Sequence struct{ next int }

func (s *Sequence) Event(name string, payload map[string]any) string {
	body := make(map[string]any, len(payload)+2)
	for key, value := range payload {
		body[key] = value
	}
	if _, ok := body["type"]; !ok {
		body["type"] = name
	}
	body["sequence_number"] = s.next
	s.next++
	encoded, _ := json.Marshal(body)
	return fmt.Sprintf("event: %s\ndata: %s\n\n", name, encoded)
}

func Failure(responseID, model, message, errorType string) []string {
	if errorType == "" {
		errorType = "server_error"
	}
	seq := &Sequence{}
	initial := map[string]any{
		"id": responseID, "object": "response", "created_at": 0,
		"status": "in_progress", "model": model, "output": []any{},
		"usage": NormalizeUsage(nil),
	}
	failed := map[string]any{
		"id": responseID, "object": "response", "status": "failed", "model": model,
		"error": map[string]any{"type": errorType, "message": message},
	}
	return []string{
		seq.Event("response.created", map[string]any{"response": initial}),
		seq.Event("response.in_progress", map[string]any{"response": initial}),
		seq.Event("response.failed", map[string]any{"response": failed}),
		"data: [DONE]\n\n",
	}
}
