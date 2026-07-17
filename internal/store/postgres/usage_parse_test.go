package postgres

import "testing"

func TestUsageFromOpenAICacheAliases(t *testing.T) {
	prompt, completion, total, cacheRead, _, reasoning := UsageFromOpenAI(map[string]any{
		"prompt_tokens":     100,
		"completion_tokens": 10,
		"total_tokens":      110,
		"prompt_tokens_details": map[string]any{
			"cached_tokens": 40,
		},
		"completion_tokens_details": map[string]any{
			"reasoning_tokens": 7,
		},
	})
	if prompt != 100 || completion != 10 || total != 110 {
		t.Fatalf("basic tokens %#v %#v %#v", prompt, completion, total)
	}
	if cacheRead != 40 {
		t.Fatalf("cacheRead=%d", cacheRead)
	}
	if reasoning != 7 {
		t.Fatalf("reasoning=%d", reasoning)
	}
	// anthropic-ish aliases
	_, _, _, cr, cc, _ := UsageFromOpenAI(map[string]any{
		"input_tokens":                50,
		"output_tokens":               5,
		"cache_read_input_tokens":     12,
		"cache_creation_input_tokens": 3,
	})
	if cr != 12 || cc != 3 {
		t.Fatalf("alias cache cr=%d cc=%d", cr, cc)
	}
}
