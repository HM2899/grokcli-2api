package redis

import (
	"context"
	"strconv"
	"strings"
	"time"
)

const usageDayTTLSeconds = 40 * 24 * 3600

var usageFields = []string{
	"requests",
	"success",
	"fail",
	"prompt_tokens",
	"completion_tokens",
	"total_tokens",
}

type UsageDeltas struct {
	PromptTokens     int64
	CompletionTokens int64
	TotalTokens      int64
	OK               bool
	APIKeyID         string
	AccountID        string
	Model            string
	TS               time.Time
}

func (c *Client) usageDayKey(day, dim, dimID string) string {
	dim = strings.TrimSpace(dim)
	if dim == "" {
		dim = "global"
	}
	dimID = strings.TrimSpace(dimID)
	if dim == "global" {
		return c.key("usage", "day", day, "global")
	}
	if dimID == "" {
		dimID = "_"
	}
	return c.key("usage", "day", day, dim, dimID)
}

func (c *Client) usageLifeKey(dim, dimID string) string {
	dim = strings.TrimSpace(dim)
	if dim == "" {
		dim = "global"
	}
	dimID = strings.TrimSpace(dimID)
	if dim == "global" {
		return c.key("usage", "life", "global")
	}
	if dimID == "" {
		dimID = "_"
	}
	return c.key("usage", "life", dim, dimID)
}

func usageDayString(ts time.Time) string {
	if ts.IsZero() {
		ts = time.Now().UTC()
	} else {
		ts = ts.UTC()
	}
	return ts.Format("20060102")
}

// RecordUsage bumps Python-compatible hot buckets:
//
//	g2a:usage:day:{YYYYMMDD}:{dim}[:{id}]
//	g2a:usage:life:{dim}[:{id}]
func (c *Client) RecordUsage(ctx context.Context, rec UsageDeltas) bool {
	if !c.Enabled() {
		return false
	}
	day := usageDayString(rec.TS)
	pt := max64(0, rec.PromptTokens)
	ct := max64(0, rec.CompletionTokens)
	tt := max64(0, rec.TotalTokens)
	if tt <= 0 {
		tt = pt + ct
	}
	deltas := map[string]int64{
		"requests": 1,
	}
	if rec.OK {
		deltas["success"] = 1
		deltas["fail"] = 0
		deltas["prompt_tokens"] = pt
		deltas["completion_tokens"] = ct
		deltas["total_tokens"] = tt
	} else {
		deltas["success"] = 0
		deltas["fail"] = 1
		deltas["prompt_tokens"] = 0
		deltas["completion_tokens"] = 0
		deltas["total_tokens"] = 0
	}

	dims := [][2]string{{"global", ""}}
	if id := strings.TrimSpace(rec.APIKeyID); id != "" {
		dims = append(dims, [2]string{"key", id})
	}
	if id := strings.TrimSpace(rec.AccountID); id != "" {
		dims = append(dims, [2]string{"account", id})
	}
	if model := strings.TrimSpace(rec.Model); model != "" {
		if len(model) > 120 {
			model = model[:120]
		}
		dims = append(dims, [2]string{"model", model})
	}

	anyOK := false
	for _, dim := range dims {
		if c.incrUsageHash(ctx, c.usageDayKey(day, dim[0], dim[1]), deltas, usageDayTTLSeconds) {
			anyOK = true
		}
		if dim[0] == "global" || dim[0] == "key" || dim[0] == "account" {
			_ = c.incrUsageHash(ctx, c.usageLifeKey(dim[0], dim[1]), deltas, 0)
		}
	}
	return anyOK
}

func (c *Client) incrUsageHash(ctx context.Context, key string, deltas map[string]int64, ttlSeconds int) bool {
	touched := false
	for field, amount := range deltas {
		if amount == 0 {
			continue
		}
		if _, err := c.HIncrBy(ctx, key, field, amount); err == nil {
			touched = true
		}
	}
	if touched && ttlSeconds > 0 {
		_ = c.Expire(ctx, key, ttlSeconds)
	}
	return touched
}

func (c *Client) GetUsageDay(ctx context.Context, dim, dimID string, ts time.Time) map[string]int64 {
	if !c.Enabled() {
		return emptyUsage()
	}
	raw, err := c.HGetAll(ctx, c.usageDayKey(usageDayString(ts), dim, dimID))
	if err != nil {
		return emptyUsage()
	}
	return parseUsageHash(raw)
}

func (c *Client) GetUsageLifetime(ctx context.Context, dim, dimID string) map[string]int64 {
	if !c.Enabled() {
		return emptyUsage()
	}
	raw, err := c.HGetAll(ctx, c.usageLifeKey(dim, dimID))
	if err != nil {
		return emptyUsage()
	}
	return parseUsageHash(raw)
}

// LightSnapshot matches Python usage_redis.light_snapshot for status cards.
func (c *Client) LightSnapshot(ctx context.Context) map[string]any {
	today := c.GetUsageDay(ctx, "global", "", time.Now().UTC())
	life := c.GetUsageLifetime(ctx, "global", "")
	return map[string]any{
		"today_requests":          today["requests"],
		"today_success":           today["success"],
		"today_fail":              today["fail"],
		"today_tokens":            today["total_tokens"],
		"today_prompt_tokens":     today["prompt_tokens"],
		"today_completion_tokens": today["completion_tokens"],
		"total_requests":          life["requests"],
		"total_tokens":            life["total_tokens"],
		"total_prompt_tokens":     life["prompt_tokens"],
		"total_completion_tokens": life["completion_tokens"],
		"source":                  "redis",
	}
}

func emptyUsage() map[string]int64 {
	out := make(map[string]int64, len(usageFields))
	for _, f := range usageFields {
		out[f] = 0
	}
	return out
}

func parseUsageHash(raw map[string]string) map[string]int64 {
	out := emptyUsage()
	for _, f := range usageFields {
		if v, ok := raw[f]; ok {
			if n, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil && n > 0 {
				out[f] = int64(n)
			}
		}
	}
	return out
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
