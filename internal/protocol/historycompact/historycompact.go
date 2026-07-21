// Package historycompact ports the Python soft-tier tool-loop history shrinker.
//
// Defaults are IQ-first (OFF). Enable via:
//   - env GROK2API_HISTORY_COMPACT=1
//   - admin setting history_compact_enabled (hot-reloaded via Configure)
//   - auto threshold GROK2API_HISTORY_COMPACT_AUTO_CHARS / history_compact_auto_chars
//
// Codex multi-turn tool loops: when auto_chars is unset (0), Apply still auto-forces
// soft compact once messages JSON exceeds CodexDefaultAutoChars (~200k), so long
// sessions do not blow upstream context while prefix-stable rewrites keep cache hits.
package historycompact

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

const placeholderPrefix = "[compacted tool result"

// CodexDefaultAutoChars is used when no global auto threshold is configured
// and the request looks like Codex / OpenAI-native agent. ~200k JSON chars of
// messages is a practical soft trigger well below grok-4.5's 500k context.
const CodexDefaultAutoChars = 200_000

type Options struct {
	Enabled            bool
	PrefixStable       bool
	KeepToolRounds     int
	MidToolRounds      int
	MaxToolResultChars int
	MidToolResultChars int
	OldToolResultChars int
	MaxMessagesChars   int
}

// runtime holds durable admin settings (and optional env overlay).
// Env is the floor/fallback; Configure() from app_settings takes precedence when set.
type runtimeConfig struct {
	enabledSet       bool
	enabled          bool
	autoCharsSet     bool
	autoChars        int
	keepRoundsSet    bool
	keepToolRounds   int
	maxToolResultSet bool
	maxToolResult    int
}

// ConfigureOpts is the admin/hot-reload payload for compact knobs.
type ConfigureOpts struct {
	Enabled            *bool
	AutoChars          *int
	KeepToolRounds     *int
	MaxToolResultChars *int
}

var (
	runtimeMu sync.RWMutex
	runtime   runtimeConfig
)

// Configure applies durable admin settings hot without restart.
// Pass enabled/autoChars pointers as nil to leave that field untouched.
// Note: autoChars=0 means "no global auto threshold" — Codex clients still get
// CodexDefaultAutoChars via EffectiveAutoChars (see Apply).
func Configure(enabled *bool, autoChars *int) {
	ConfigureFull(ConfigureOpts{Enabled: enabled, AutoChars: autoChars})
}

// ConfigureFull applies the full compact knob set from admin settings.
func ConfigureFull(opts ConfigureOpts) {
	runtimeMu.Lock()
	defer runtimeMu.Unlock()
	if opts.Enabled != nil {
		runtime.enabledSet = true
		runtime.enabled = *opts.Enabled
	}
	if opts.AutoChars != nil {
		v := *opts.AutoChars
		if v < 0 {
			v = 0
		}
		if v > 5_000_000 {
			v = 5_000_000
		}
		// 0 is valid (no global auto). Non-zero below 4000 is clamped to 4000 for safety.
		if v > 0 && v < 4000 {
			v = 4000
		}
		runtime.autoCharsSet = true
		runtime.autoChars = v
	}
	if opts.KeepToolRounds != nil {
		v := *opts.KeepToolRounds
		if v < 1 {
			v = 1
		}
		if v > 64 {
			v = 64
		}
		runtime.keepRoundsSet = true
		runtime.keepToolRounds = v
	}
	if opts.MaxToolResultChars != nil {
		v := *opts.MaxToolResultChars
		if v < 0 {
			v = 0
		}
		if v > 0 && v < 512 {
			v = 512
		}
		if v > 2_000_000 {
			v = 2_000_000
		}
		runtime.maxToolResultSet = true
		runtime.maxToolResult = v
	}
}

// Reset clears durable overrides so only process env applies (tests / process recycle).
func Reset() {
	runtimeMu.Lock()
	runtime = runtimeConfig{}
	runtimeMu.Unlock()
}

// Snapshot returns the effective compact knobs (for admin/debug headers).
func Snapshot() map[string]any {
	return map[string]any{
		"enabled":                  DefaultOptions().Enabled,
		"auto_chars":               AutoChars(),
		"codex_default_auto_chars": CodexDefaultAutoChars,
		"from_settings_enabled": func() bool {
			runtimeMu.RLock()
			defer runtimeMu.RUnlock()
			return runtime.enabledSet
		}(),
		"from_settings_auto_chars": func() bool {
			runtimeMu.RLock()
			defer runtimeMu.RUnlock()
			return runtime.autoCharsSet
		}(),
	}
}

func DefaultOptions() Options {
	enabled := envBool("GROK2API_HISTORY_COMPACT", false)
	keep := envInt("GROK2API_HISTORY_KEEP_TOOL_ROUNDS", 32, 1, 64)
	maxTR := envInt("GROK2API_HISTORY_MAX_TOOL_RESULT_CHARS", 48000, 512, 2_000_000)
	runtimeMu.RLock()
	if runtime.enabledSet {
		enabled = runtime.enabled
	}
	if runtime.keepRoundsSet {
		keep = runtime.keepToolRounds
	}
	if runtime.maxToolResultSet && runtime.maxToolResult > 0 {
		maxTR = runtime.maxToolResult
	}
	runtimeMu.RUnlock()
	return Options{
		Enabled:            enabled,
		PrefixStable:       envBool("GROK2API_HISTORY_PREFIX_STABLE", true),
		KeepToolRounds:     keep,
		MidToolRounds:      envInt("GROK2API_HISTORY_MID_TOOL_ROUNDS", 24, 0, 128),
		MaxToolResultChars: maxTR,
		MidToolResultChars: envInt("GROK2API_HISTORY_MID_TOOL_RESULT_CHARS", 16000, 512, 2_000_000),
		OldToolResultChars: envInt("GROK2API_HISTORY_OLD_TOOL_RESULT_CHARS", 8000, 256, 2_000_000),
		MaxMessagesChars:   envInt("GROK2API_HISTORY_MAX_MESSAGES_CHARS", 1_200_000, 8_000, 5_000_000),
	}
}

func AutoChars() int {
	runtimeMu.RLock()
	if runtime.autoCharsSet {
		v := runtime.autoChars
		runtimeMu.RUnlock()
		return v
	}
	runtimeMu.RUnlock()
	return envInt("GROK2API_HISTORY_COMPACT_AUTO_CHARS", 0, 0, 5_000_000)
}

// EffectiveAutoChars returns the global auto threshold, or Codex default when
// unset and the client is Codex/OpenAI-native (so long tool loops still compact).
func EffectiveAutoChars(userAgent string) int {
	return EffectiveAutoCharsFor(userAgent, nil, nil)
}

// EffectiveAutoCharsFor is like EffectiveAutoChars but can detect Codex without UA
// via tools schema / metadata (proxies often strip User-Agent).
func EffectiveAutoCharsFor(userAgent string, tools any, raw map[string]any) int {
	if th := AutoChars(); th > 0 {
		return th
	}
	if LooksLikeCodexRequest(userAgent, tools, raw) {
		return CodexDefaultAutoChars
	}
	return 0
}

func ShouldAutoCompact(body map[string]any) bool {
	return ShouldAutoCompactUA(body, "")
}

func ShouldAutoCompactUA(body map[string]any, userAgent string) bool {
	threshold := EffectiveAutoChars(userAgent)
	if threshold <= 0 || body == nil {
		return false
	}
	messages := normalizeMessages(body["messages"])
	if len(messages) == 0 {
		return false
	}
	return messagesCharSize(messages) >= threshold
}

// Apply mutates body["messages"] when compact is enabled or auto-forced.
// Stats are stored under body["_history_compact"].
// Optional userAgent enables Codex default auto-threshold when global auto_chars=0.
// When body carries tools/metadata, Codex is also detected without UA (proxy strip).
func Apply(body map[string]any, userAgent ...string) map[string]any {
	if body == nil {
		return map[string]any{"enabled": false, "applied": false}
	}
	ua := ""
	if len(userAgent) > 0 {
		ua = userAgent[0]
	}
	opts := DefaultOptions()
	autoTh := EffectiveAutoCharsFor(ua, body["tools"], body)
	// Fast path: disabled and no auto threshold for this client → skip work.
	if !opts.Enabled && autoTh <= 0 {
		stats := map[string]any{"enabled": false, "applied": false, "auto_chars": 0}
		body["_history_compact"] = stats
		return stats
	}
	force := false
	if autoTh > 0 {
		messages := normalizeMessages(body["messages"])
		if len(messages) > 0 && messagesCharSize(messages) >= autoTh {
			force = true
			opts.Enabled = true
		}
	}
	if !opts.Enabled {
		stats := map[string]any{"enabled": false, "applied": false, "auto_chars": autoTh}
		body["_history_compact"] = stats
		return stats
	}
	messages := normalizeMessages(body["messages"])
	// Codex long sessions: slightly tighter mid/old tool caps so 200k trigger
	// actually reduces payload (soft-tier with 32 recent rounds can stay huge).
	if LooksLikeCodexRequest(ua, body["tools"], body) {
		if opts.MidToolResultChars > 8000 {
			opts.MidToolResultChars = 8000
		}
		if opts.OldToolResultChars > 4000 {
			opts.OldToolResultChars = 4000
		}
		// Cap absolute message budget for Codex to leave room for tools/system.
		if opts.MaxMessagesChars > 900_000 {
			opts.MaxMessagesChars = 900_000
		}
	}
	compacted, stats := CompactMessages(messages, opts)
	body["messages"] = compacted
	stats["auto_chars"] = autoTh
	if force {
		stats["auto"] = true
		if AutoChars() <= 0 && autoTh == CodexDefaultAutoChars {
			stats["auto_source"] = "codex_default"
		} else {
			stats["auto_source"] = "threshold"
		}
	}
	body["_history_compact"] = stats
	return stats
}

// CompactMessages shrinks tool-loop history when opts.Enabled.
// Callers should avoid invoking this on the disabled fast path (see Apply).
func CompactMessages(messages []any, opts Options) ([]any, map[string]any) {
	stats := map[string]any{
		"enabled": false, "applied": false,
		"before_chars": 0, "after_chars": 0, "tool_rounds": 0,
		"compacted_tool_msgs": 0, "truncated_tool_msgs": 0, "soft_summary_msgs": 0,
		"prefix_stable": false, "keep_tool_rounds": 0, "mid_tool_rounds": 0,
		"policy": "soft-tier",
	}
	if len(messages) == 0 {
		return messages, stats
	}
	if !opts.Enabled {
		// Disabled: return original slice, no clone / no full JSON marshal.
		stats["enabled"] = false
		return messages, stats
	}
	keep := max(1, opts.KeepToolRounds)
	maxTR := max(512, opts.MaxToolResultChars)
	budget := max(8_000, opts.MaxMessagesChars)
	midN := max(0, opts.MidToolRounds)
	midChars := max(256, opts.MidToolResultChars)
	oldChars := max(128, opts.OldToolResultChars)
	stable := opts.PrefixStable

	out := make([]any, 0, len(messages))
	for _, item := range messages {
		if msg, ok := item.(map[string]any); ok {
			out = append(out, cloneMap(msg))
		} else {
			out = append(out, item)
		}
	}
	before := messagesCharSize(out)
	stats["before_chars"] = before
	stats["enabled"] = opts.Enabled
	stats["prefix_stable"] = stable
	stats["keep_tool_rounds"] = keep
	stats["mid_tool_rounds"] = midN

	spans := toolRoundSpans(out)
	stats["tool_rounds"] = len(spans)
	recentSpans, midSpans, oldSpans := splitSpans(spans, keep, midN)

	recentIdx := map[int]bool{}
	for _, span := range recentSpans {
		for i := span[0]; i < span[1]; i++ {
			recentIdx[i] = true
		}
	}

	for _, span := range recentSpans {
		for idx := span[0]; idx < span[1]; idx++ {
			msg, ok := out[idx].(map[string]any)
			if !ok || !isToolMessage(msg) {
				continue
			}
			if shrinkToolMessage(msg, maxTR, false, stable, false, "recent") {
				stats["truncated_tool_msgs"] = asInt(stats["truncated_tool_msgs"]) + 1
			}
		}
	}
	for _, span := range midSpans {
		for idx := span[0]; idx < span[1]; idx++ {
			msg, ok := out[idx].(map[string]any)
			if !ok || !isToolMessage(msg) {
				continue
			}
			if shrinkToolMessage(msg, midChars, true, stable, true, "mid round") {
				stats["soft_summary_msgs"] = asInt(stats["soft_summary_msgs"]) + 1
				stats["truncated_tool_msgs"] = asInt(stats["truncated_tool_msgs"]) + 1
			}
		}
	}
	for _, span := range oldSpans {
		for idx := span[0]; idx < span[1]; idx++ {
			msg, ok := out[idx].(map[string]any)
			if !ok || !isToolMessage(msg) {
				continue
			}
			if shrinkToolMessage(msg, oldChars, true, stable, true, "older round") {
				stats["soft_summary_msgs"] = asInt(stats["soft_summary_msgs"]) + 1
				stats["truncated_tool_msgs"] = asInt(stats["truncated_tool_msgs"]) + 1
			}
		}
	}

	after := messagesCharSize(out)
	if after > budget {
		tighterOld := max(512, oldChars/2)
		tighterMid := max(1024, midChars/2)
		for _, tier := range []struct {
			spans  [][2]int
			cap    int
			reason string
		}{
			{oldSpans, tighterOld, "budget/old"},
			{midSpans, tighterMid, "budget/mid"},
		} {
			if after <= budget {
				break
			}
			for _, span := range tier.spans {
				if after <= budget {
					break
				}
				for idx := span[0]; idx < span[1]; idx++ {
					msg, ok := out[idx].(map[string]any)
					if !ok || !isToolMessage(msg) {
						continue
					}
					text := contentToText(msg["content"])
					if text == "" || strings.HasPrefix(text, placeholderPrefix) || len(text) <= tier.cap {
						continue
					}
					newText := truncateText(text, tier.cap, "tool_result/"+tier.reason)
					if newText != text {
						if applyTextCompactIfSafe(msg, newText) {
							stats["truncated_tool_msgs"] = asInt(stats["truncated_tool_msgs"]) + 1
						}
					}
				}
				after = messagesCharSize(out)
			}
		}
	}

	after = messagesCharSize(out)
	if after > budget {
		for _, span := range append(append([][2]int{}, oldSpans...), midSpans...) {
			if after <= budget {
				break
			}
			for idx := span[0]; idx < span[1]; idx++ {
				if after <= budget {
					break
				}
				msg, ok := out[idx].(map[string]any)
				if !ok || !isToolMessage(msg) {
					continue
				}
				text := contentToText(msg["content"])
				if text == "" || strings.HasPrefix(text, placeholderPrefix) {
					continue
				}
				newText := placeholder(text, "size budget")
				if newText != text {
					if applyTextCompactIfSafe(msg, newText) {
						stats["compacted_tool_msgs"] = asInt(stats["compacted_tool_msgs"]) + 1
						after = messagesCharSize(out)
					}
				}
			}
		}
	}

	after = messagesCharSize(out)
	if after > budget {
		hard := max(2000, maxTR/3)
		for i := len(recentSpans) - 1; i >= 0; i-- {
			if after <= budget {
				break
			}
			span := recentSpans[i]
			for idx := span[0]; idx < span[1]; idx++ {
				msg, ok := out[idx].(map[string]any)
				if !ok || !isToolMessage(msg) {
					continue
				}
				text := contentToText(msg["content"])
				if text == "" || strings.HasPrefix(text, placeholderPrefix) || len(text) <= hard {
					continue
				}
				newText := truncateText(text, hard, "tool_result")
				if newText != text {
					if applyTextCompactIfSafe(msg, newText) {
						stats["truncated_tool_msgs"] = asInt(stats["truncated_tool_msgs"]) + 1
					}
				}
			}
			after = messagesCharSize(out)
		}
	}

	after = messagesCharSize(out)
	if after > budget {
		soft := max(2000, maxTR/2)
		for idx, item := range out {
			if after <= budget {
				break
			}
			msg, ok := item.(map[string]any)
			if !ok {
				continue
			}
			role := strings.ToLower(stringValue(msg["role"]))
			if role == "system" || isToolMessage(msg) || recentIdx[idx] {
				continue
			}
			if role != "user" && role != "assistant" {
				continue
			}
			text := contentToText(msg["content"])
			if text == "" {
				continue
			}
			limit := soft
			if role == "assistant" && !isAssistantToolCall(msg) {
				limit = soft * 2
			}
			if len(text) <= limit {
				continue
			}
			newText := truncateText(text, limit, role)
			if newText != text {
				if applyTextCompactIfSafe(msg, newText) {
					after = messagesCharSize(out)
				}
			}
		}
	}

	after = messagesCharSize(out)
	stats["after_chars"] = after
	stats["applied"] = asInt(stats["compacted_tool_msgs"]) > 0 ||
		asInt(stats["truncated_tool_msgs"]) > 0 ||
		asInt(stats["soft_summary_msgs"]) > 0 ||
		after < before
	return out, stats
}

func normalizeMessages(raw any) []any {
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

func toolRoundSpans(messages []any) [][2]int {
	spans := make([][2]int, 0)
	for i := 0; i < len(messages); {
		msg, ok := messages[i].(map[string]any)
		if !ok || !isAssistantToolCall(msg) {
			i++
			continue
		}
		j := i + 1
		for j < len(messages) {
			next, ok := messages[j].(map[string]any)
			if !ok || !isToolMessage(next) {
				break
			}
			j++
		}
		spans = append(spans, [2]int{i, j})
		i = j
	}
	return spans
}

func splitSpans(spans [][2]int, keep, midN int) (recent, mid, old [][2]int) {
	if keep >= len(spans) {
		return spans, nil, nil
	}
	recent = spans[len(spans)-keep:]
	rest := spans[:len(spans)-keep]
	if midN <= 0 || len(rest) == 0 {
		return recent, nil, rest
	}
	if midN >= len(rest) {
		return recent, rest, nil
	}
	return recent, rest[len(rest)-midN:], rest[:len(rest)-midN]
}

func isToolMessage(msg map[string]any) bool {
	role := strings.ToLower(stringValue(msg["role"]))
	return role == "tool" || role == "function"
}

func isAssistantToolCall(msg map[string]any) bool {
	if strings.ToLower(stringValue(msg["role"])) != "assistant" {
		return false
	}
	if calls, ok := msg["tool_calls"].([]any); ok && len(calls) > 0 {
		return true
	}
	if calls, ok := msg["tool_calls"].([]map[string]any); ok && len(calls) > 0 {
		return true
	}
	if fn, ok := msg["function_call"].(map[string]any); ok && stringValue(fn["name"]) != "" {
		return true
	}
	return false
}

func shrinkToolMessage(msg map[string]any, maxChars int, forcePlaceholder, prefixStable, softSummary bool, reason string) bool {
	original := contentToText(msg["content"])
	if original == "" {
		return false
	}
	if prefixStable && alreadyCompacted(original) {
		return false
	}
	var next string
	if forcePlaceholder {
		if softSummary && maxChars > 0 {
			next = softSummaryText(original, maxChars, reason)
		} else {
			next = placeholder(original, reason)
		}
	} else if len(original) > maxChars {
		next = truncateText(original, maxChars, "tool_result")
	} else {
		return false
	}
	if next == original {
		return false
	}
	if !applyTextCompactIfSafe(msg, next) {
		return false
	}
	return true
}

// isMultimodalContent reports whether content is a parts array with image/media
// blocks that must not be flattened to plain text (preserves vision inputs).
func isMultimodalContent(content any) bool {
	parts, ok := content.([]any)
	if !ok || len(parts) == 0 {
		return false
	}
	for _, block := range parts {
		item, ok := block.(map[string]any)
		if !ok {
			continue
		}
		btype := strings.ToLower(stringValue(item["type"]))
		switch btype {
		case "image_url", "image", "input_image", "input_file", "file", "audio", "input_audio", "video":
			return true
		}
		if item["image_url"] != nil || item["source"] != nil {
			return true
		}
	}
	return false
}

// applyTextCompactIfSafe rewrites message content only when it is plain text.
// Multimodal (image) messages keep their part arrays so vision inputs survive
// history compaction.
func applyTextCompactIfSafe(msg map[string]any, newText string) bool {
	if msg == nil {
		return false
	}
	if isMultimodalContent(msg["content"]) {
		return false
	}
	msg["content"] = newText
	return true
}

func contentToText(content any) string {
	switch value := content.(type) {
	case nil:
		return ""
	case string:
		return value
	case []any:
		parts := make([]string, 0, len(value))
		for _, block := range value {
			switch item := block.(type) {
			case string:
				parts = append(parts, item)
			case map[string]any:
				btype := strings.ToLower(stringValue(item["type"]))
				if btype == "text" || btype == "input_text" || btype == "output_text" {
					parts = append(parts, stringValue(item["text"]))
				} else if btype == "image_url" || btype == "image" || btype == "input_image" {
					// Size accounting only — keep structure via applyTextCompactIfSafe.
					parts = append(parts, "[image]")
				} else if btype == "tool_result" {
					parts = append(parts, contentToText(item["content"]))
				} else {
					parts = append(parts, jsonString(item))
				}
			default:
				parts = append(parts, stringValue(item))
			}
		}
		return strings.Join(parts, "")
	case map[string]any:
		return jsonString(value)
	default:
		return stringValue(value)
	}
}

func truncateText(text string, limit int, label string) string {
	if limit <= 0 || len([]rune(text)) <= limit {
		// Use byte length for parity with Python len(str); Python counts code points for unicode
		// but for ASCII-heavy tool dumps this matches. Prefer rune-aware when over.
	}
	runes := []rune(text)
	if limit <= 0 || len(runes) <= limit {
		return text
	}
	trailerBudget := 120
	body := max(0, limit-trailerBudget)
	digest := stableDigest(text)
	if body < 64 {
		head := max(0, limit-64)
		if head > len(runes) {
			head = len(runes)
		}
		omitted := len(runes) - head
		return string(runes[:head]) + "\n…[" + label + " truncated, " + strconv.Itoa(omitted) + " chars omitted, id=" + digest + "]"
	}
	headN := max(32, body/3)
	tailN := max(32, body-headN)
	if headN+tailN >= len(runes) {
		return text
	}
	omitted := len(runes) - headN - tailN
	return string(runes[:headN]) + "\n…[" + label + " truncated, " + strconv.Itoa(omitted) + " chars omitted, id=" + digest + "]\n" + string(runes[len(runes)-tailN:])
}

func softSummaryText(original string, maxChars int, reason string) string {
	if len([]rune(original)) <= maxChars {
		return original
	}
	return truncateText(original, maxChars, "tool_result/"+reason)
}

func placeholder(original, reason string) string {
	n := len([]rune(original))
	return placeholderPrefix + ": " + reason + "; original " + strconv.Itoa(n) + " chars; id=" + stableDigest(original) + " — re-Read if needed]"
}

func alreadyCompacted(text string) bool {
	if text == "" {
		return false
	}
	if strings.HasPrefix(text, placeholderPrefix) {
		return true
	}
	if strings.Contains(text, "…[tool_result") || strings.Contains(text, "…[content truncated") {
		return true
	}
	return strings.Contains(text, " chars omitted, id=")
}

func messagesCharSize(messages []any) int {
	encoded, err := json.Marshal(messages)
	if err != nil {
		total := 0
		for _, item := range messages {
			total += len(stringValue(item))
		}
		return total
	}
	return len(encoded)
}

func stableDigest(text string) string {
	sum := sha256.Sum256([]byte(text))
	return hex.EncodeToString(sum[:6])
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func jsonString(value any) string {
	encoded, err := json.Marshal(value)
	if err != nil {
		return ""
	}
	return string(encoded)
}

func stringValue(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case string:
		return v
	default:
		return strings.TrimSpace(jsonString(v))
	}
}

func truthy(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	default:
		return false
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

func envBool(name string, fallback bool) bool {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	switch strings.ToLower(raw) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func envInt(name string, fallback, minimum, maximum int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return clamp(fallback, minimum, maximum)
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return clamp(fallback, minimum, maximum)
	}
	return clamp(n, minimum, maximum)
}

func clamp(value, minimum, maximum int) int {
	if value < minimum {
		return minimum
	}
	if value > maximum {
		return maximum
	}
	return value
}

func max(a, b int) int {
	if a > b {
		return a
	}
	return b
}

// Outbound tool policy (Python history_compact.resolve_outbound_*).

// IsOpenAINativeClient reports whether UA looks like Codex / OpenAI SDK rather
// than Claude Code / Anthropic. Empty or unknown UA is conservative (false).
func IsOpenAINativeClient(userAgent string) bool {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	if ua == "" {
		return false
	}
	for _, marker := range []string{"claude-cli", "anthropic", "claude-code"} {
		if strings.Contains(ua, marker) {
			return false
		}
	}
	// Codex / OpenAI agent UAs. Keep broad for tool-policy (multi-tool, zero gap).
	for _, marker := range []string{
		"codex", "openai/python", "openai-python", "openai/", "chatgpt", "gpt-agent", "responses-sdk",
	} {
		if strings.Contains(ua, marker) {
			return true
		}
	}
	return false
}

// IsCodexClient is narrower than IsOpenAINativeClient. Used for TTFT clamps that
// must not affect sub2api / new-api OpenAI SDK relays.
func IsCodexClient(userAgent string) bool {
	ua := strings.ToLower(strings.TrimSpace(userAgent))
	if ua == "" {
		return false
	}
	for _, marker := range []string{
		"codex", "codex-cli", "codex-tui", "gpt-agent", "openai-codex",
	} {
		if strings.Contains(ua, marker) {
			return true
		}
	}
	return false
}

// LooksLikeCodexRequest detects Codex even when reverse proxies strip User-Agent.
// Signals: UA, tools schema (exec_command/cmd, apply_patch/input), metadata.session.
func LooksLikeCodexRequest(userAgent string, tools any, raw map[string]any) bool {
	if IsCodexClient(userAgent) || IsOpenAINativeClient(userAgent) {
		return true
	}
	// Explicit metadata hints from agents / gateways.
	if raw != nil {
		if meta, ok := raw["metadata"].(map[string]any); ok && meta != nil {
			for _, key := range []string{"client", "agent", "source", "app", "sdk"} {
				v := strings.ToLower(strings.TrimSpace(fmtString(meta[key])))
				if strings.Contains(v, "codex") || strings.Contains(v, "gpt-agent") {
					return true
				}
			}
		}
		for _, key := range []string{"prompt_cache_key", "previous_response_id"} {
			// previous_response_id alone is weak; only count with tools signal below
			_ = key
		}
	}
	// Tools schema: Codex local tools use exec_command{cmd} / apply_patch{input}.
	if toolsLookLikeCodex(tools) {
		return true
	}
	return false
}

func toolsLookLikeCodex(tools any) bool {
	list := toolsAsSlice(tools)
	if len(list) == 0 {
		return false
	}
	shellCmd := 0
	applyPatch := 0
	for _, item := range list {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := toolNameOf(tool)
		nk := strings.ToLower(strings.ReplaceAll(name, "-", "_"))
		props := toolProps(tool)
		if nk == "exec_command" || nk == "shell" || nk == "local_shell" || nk == "run_command" ||
			strings.HasSuffix(nk, "exec_command") || strings.HasSuffix(nk, "shell") {
			// Codex Desktop/CLI may advertise cmd or command; either counts.
			if _, hasCmd := props["cmd"]; hasCmd {
				shellCmd++
			}
			if _, hasCommand := props["command"]; hasCommand {
				// Prefer exec_command+command as Codex/OpenAI shell signal.
				if nk == "exec_command" || strings.HasSuffix(nk, "exec_command") || nk == "local_shell" {
					shellCmd++
				}
			}
			if req, ok := toolRequired(tool); ok {
				for _, r := range req {
					if r == "cmd" || (r == "command" && (nk == "exec_command" || strings.HasSuffix(nk, "exec_command") || nk == "local_shell")) {
						shellCmd++
					}
				}
			}
		}
		if strings.Contains(nk, "apply_patch") || nk == "apply_patch" || nk == "applypatch" {
			if _, hasIn := props["input"]; hasIn {
				applyPatch++
			}
		}
	}
	return shellCmd > 0 || applyPatch > 0
}

func toolsAsSlice(tools any) []any {
	switch v := tools.(type) {
	case []any:
		return v
	case []map[string]any:
		out := make([]any, len(v))
		for i, item := range v {
			out[i] = item
		}
		return out
	default:
		return nil
	}
}

func toolNameOf(tool map[string]any) string {
	if n, ok := tool["name"].(string); ok && strings.TrimSpace(n) != "" {
		return n
	}
	if fn, ok := tool["function"].(map[string]any); ok {
		if n, ok := fn["name"].(string); ok {
			return n
		}
	}
	return ""
}

func toolProps(tool map[string]any) map[string]any {
	params := tool["parameters"]
	if params == nil {
		params = tool["input_schema"]
	}
	if params == nil {
		if fn, ok := tool["function"].(map[string]any); ok {
			params = fn["parameters"]
			if params == nil {
				params = fn["input_schema"]
			}
		}
	}
	pm, _ := params.(map[string]any)
	if pm == nil {
		return map[string]any{}
	}
	props, _ := pm["properties"].(map[string]any)
	if props == nil {
		return map[string]any{}
	}
	return props
}

func toolRequired(tool map[string]any) ([]string, bool) {
	params := tool["parameters"]
	if params == nil {
		params = tool["input_schema"]
	}
	if params == nil {
		if fn, ok := tool["function"].(map[string]any); ok {
			params = fn["parameters"]
		}
	}
	pm, _ := params.(map[string]any)
	if pm == nil {
		return nil, false
	}
	raw, ok := pm["required"]
	if !ok {
		return nil, false
	}
	switch v := raw.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s, ok := item.(string); ok {
				out = append(out, s)
			}
		}
		return out, true
	case []string:
		return v, true
	default:
		return nil, false
	}
}

func fmtString(v any) string {
	switch t := v.(type) {
	case string:
		return t
	default:
		return ""
	}
}

// OutboundToolPolicy is the resolved per-request tool emission policy.
type OutboundToolPolicy struct {
	MaxTools int           // 0 = unlimited
	ToolGap  time.Duration // pause before consecutive tool frames
}

// ResolveOutboundMaxTools picks the per-turn tool cap.
//
//	chat/completions → maxToolsOpenAI (default 0 unlimited)
//	responses + OpenAI-native UA → maxToolsResponsesNative
//	else (anthropic / responses via sub2api / unknown) → maxToolsClaude
func ResolveOutboundMaxTools(protocol, userAgent string, maxToolsClaude, maxToolsOpenAI, maxToolsResponsesNative int) int {
	proto := strings.ToLower(strings.TrimSpace(protocol))
	switch proto {
	case "openai", "chat", "chat_completions", "openai_chat":
		return maxToolsOpenAI
	case "openai_responses", "responses":
		if IsOpenAINativeClient(userAgent) {
			return maxToolsResponsesNative
		}
	}
	return maxToolsClaude
}

// ResolveOutboundToolGap picks wall-clock gap between outbound tool frames.
func ResolveOutboundToolGap(protocol, userAgent string, gapClaude, gapNative time.Duration) time.Duration {
	proto := strings.ToLower(strings.TrimSpace(protocol))
	if proto == "openai" || proto == "chat" || proto == "chat_completions" || proto == "openai_chat" {
		return gapNative
	}
	if IsOpenAINativeClient(userAgent) {
		return gapNative
	}
	return gapClaude
}

// ResolveOutboundToolPolicy combines max tools + gap for a request.
func ResolveOutboundToolPolicy(protocol, userAgent string, maxToolsClaude, maxToolsOpenAI, maxToolsResponsesNative int, gapClaude, gapNative time.Duration) OutboundToolPolicy {
	return OutboundToolPolicy{
		MaxTools: ResolveOutboundMaxTools(protocol, userAgent, maxToolsClaude, maxToolsOpenAI, maxToolsResponsesNative),
		ToolGap:  ResolveOutboundToolGap(protocol, userAgent, gapClaude, gapNative),
	}
}
