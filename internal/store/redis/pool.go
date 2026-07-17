package redis

import (
	"context"
	"strconv"
	"strings"
	"time"
)

const (
	InflightTTLSeconds = 90
	SoftUsedTTLSeconds = 45
)

func (c *Client) RRNext(ctx context.Context) (int64, error) {
	return c.Incr(ctx, c.key("rr", "index"))
}

func (c *Client) MarkInflight(ctx context.Context, accountID string, ttlSeconds int) (int64, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, nil
	}
	if ttlSeconds <= 0 {
		ttlSeconds = InflightTTLSeconds
	}
	key := c.key("inflight", accountID)
	value, err := c.Incr(ctx, key)
	if err != nil {
		return 0, err
	}
	_ = c.Expire(ctx, key, ttlSeconds)
	return value, nil
}

func (c *Client) ReleaseInflight(ctx context.Context, accountID string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	key := c.key("inflight", accountID)
	n, err := c.Decr(ctx, key)
	if err != nil {
		return err
	}
	if n <= 0 {
		return c.Del(ctx, key)
	}
	return c.Expire(ctx, key, InflightTTLSeconds)
}

func (c *Client) GetInflight(ctx context.Context, accountID string) (int64, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, nil
	}
	value, err := c.Get(ctx, c.key("inflight", accountID))
	if err != nil || strings.TrimSpace(value) == "" {
		return 0, err
	}
	n, err := strconv.ParseInt(strings.TrimSpace(value), 10, 64)
	if err != nil || n < 0 {
		return 0, nil
	}
	return n, nil
}

func (c *Client) MarkSoftUsed(ctx context.Context, accountID string, ttlSeconds int, now time.Time) (float64, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return 0, nil
	}
	if ttlSeconds <= 0 {
		ttlSeconds = SoftUsedTTLSeconds
	}
	if now.IsZero() {
		now = time.Now()
	}
	stamp := float64(now.UnixNano()) / 1e9
	err := c.SetEX(ctx, c.key("soft_used", accountID), strconv.FormatFloat(stamp, 'f', 6, 64), ttlSeconds)
	return stamp, err
}

func (c *Client) MirrorCooldown(ctx context.Context, accountID string, until time.Time) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	key := c.key("cooldown", accountID)
	if until.IsZero() || !until.After(time.Now()) {
		return c.Del(ctx, key)
	}
	ttl := int(time.Until(until).Seconds())
	if ttl < 1 {
		ttl = 1
	}
	// Python stores float unix seconds as string.
	return c.SetEX(ctx, key, strconv.FormatFloat(float64(until.Unix()), 'f', 0, 64), ttl)
}

type PoolStatsTouch struct {
	Success          bool
	Error            string
	CooldownUntil    *time.Time
	ClearCooldown    bool
	ConsecutiveFails *int64
	LastStatusCode   *int
	CooldownSec      *float64
}

// TouchStats mirrors Python pool_redis.touch_stats hot counters.
func (c *Client) TouchStats(ctx context.Context, accountID string, touch PoolStatsTouch) (map[string]any, error) {
	accountID = strings.TrimSpace(accountID)
	if !c.Enabled() || accountID == "" {
		return nil, nil
	}
	k := c.key("stats", accountID)
	if _, err := c.HIncrBy(ctx, k, "request_count", 1); err != nil {
		return nil, err
	}
	if touch.Success {
		_, _ = c.HIncrBy(ctx, k, "success_count", 1)
	} else {
		_, _ = c.HIncrBy(ctx, k, "fail_count", 1)
	}
	mapping := map[string]string{
		"last_used_at": strconv.FormatFloat(float64(time.Now().UnixNano())/1e9, 'f', 6, 64),
	}
	if strings.TrimSpace(touch.Error) != "" {
		errText := strings.TrimSpace(touch.Error)
		if len(errText) > 500 {
			errText = errText[:500]
		}
		mapping["last_error"] = errText
	}
	if touch.Success {
		mapping["consecutive_fails"] = "0"
	} else if touch.ConsecutiveFails != nil {
		mapping["consecutive_fails"] = strconv.FormatInt(*touch.ConsecutiveFails, 10)
	}
	if touch.LastStatusCode != nil {
		mapping["last_status_code"] = strconv.Itoa(*touch.LastStatusCode)
	}
	if touch.CooldownSec != nil {
		mapping["cooldown_sec"] = strconv.FormatFloat(*touch.CooldownSec, 'f', 3, 64)
	}
	_ = c.HSetMap(ctx, k, mapping)
	if touch.CooldownUntil != nil {
		_ = c.MirrorCooldown(ctx, accountID, *touch.CooldownUntil)
	}
	if touch.ClearCooldown {
		_ = c.MirrorCooldown(ctx, accountID, time.Time{})
		_ = c.HSetMap(ctx, k, map[string]string{"consecutive_fails": "0", "cooldown_sec": "0"})
	}
	return c.GetStats(ctx, accountID)
}

func (c *Client) GetStats(ctx context.Context, accountID string) (map[string]any, error) {
	accountID = strings.TrimSpace(accountID)
	if !c.Enabled() || accountID == "" {
		return map[string]any{}, nil
	}
	raw, err := c.HGetAll(ctx, c.key("stats", accountID))
	if err != nil {
		return nil, err
	}
	out := map[string]any{}
	for _, field := range []string{"request_count", "success_count", "fail_count", "consecutive_fails", "last_status_code"} {
		if v, ok := raw[field]; ok {
			if n, err := strconv.ParseFloat(v, 64); err == nil {
				out[field] = int64(n)
			}
		}
	}
	if v, ok := raw["cooldown_sec"]; ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			out["cooldown_sec"] = n
		}
	}
	if v, ok := raw["last_used_at"]; ok {
		if n, err := strconv.ParseFloat(v, 64); err == nil {
			out["last_used_at"] = n
		}
	}
	if v := strings.TrimSpace(raw["last_error"]); v != "" {
		out["last_error"] = v
	}
	if cdRaw, err := c.Get(ctx, c.key("cooldown", accountID)); err == nil && strings.TrimSpace(cdRaw) != "" {
		if n, err := strconv.ParseFloat(cdRaw, 64); err == nil {
			out["cooldown_until"] = n
		}
	}
	return out, nil
}

func (c *Client) GetInflightMany(ctx context.Context, accountIDs []string) map[string]int64 {
	out := map[string]int64{}
	ids := make([]string, 0, len(accountIDs))
	keys := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		ids = append(ids, id)
		keys = append(keys, c.key("inflight", id))
	}
	if len(keys) == 0 {
		return out
	}
	// One MGET round-trip instead of N GETs (each GET currently dials Redis).
	args := append([]string{"MGET"}, keys...)
	values, err := c.commandArray(ctx, args...)
	if err != nil || len(values) == 0 {
		// Fallback best-effort individual reads if MGET unsupported/fails.
		for _, id := range ids {
			if n, e := c.GetInflight(ctx, id); e == nil && n > 0 {
				out[id] = n
			}
		}
		return out
	}
	for i, id := range ids {
		if i >= len(values) {
			break
		}
		raw := strings.TrimSpace(values[i])
		if raw == "" {
			continue
		}
		n, err := strconv.ParseInt(raw, 10, 64)
		if err != nil || n <= 0 {
			continue
		}
		out[id] = n
	}
	return out
}
