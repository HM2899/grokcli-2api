package redis

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

type AffinityEntry struct {
	AccountID      string  `json:"account_id"`
	BoundAt        float64 `json:"bound_at"`
	LastSeen       float64 `json:"last_seen"`
	Hits           int64   `json:"hits"`
	SessionFP      string  `json:"session_fp,omitempty"`
	PromptCacheKey string  `json:"prompt_cache_key,omitempty"`
}

func (c *Client) GetAffinity(ctx context.Context, fingerprint string, ttl time.Duration) (*AffinityEntry, error) {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil, nil
	}
	raw, err := c.Get(ctx, c.key("affinity", fingerprint))
	if err != nil || strings.TrimSpace(raw) == "" {
		return nil, err
	}
	entry := parseAffinity(raw)
	if entry == nil || entry.AccountID == "" {
		return nil, nil
	}
	// Hot-path read: do NOT rewrite affinity on every lookup.
	// Hits/last_seen are refreshed on BindAffinity after a successful pick.
	// This saves a Redis write RTT before upstream request (TTFT critical path).
	_ = ttl
	return entry, nil
}

func (c *Client) BindAffinity(ctx context.Context, fingerprint, accountID string, ttl time.Duration, sessionFP, promptCacheKey string) error {
	fingerprint = strings.TrimSpace(fingerprint)
	accountID = strings.TrimSpace(accountID)
	if fingerprint == "" || accountID == "" {
		return nil
	}
	now := unixFloat(time.Now())
	entry := AffinityEntry{AccountID: accountID, BoundAt: now, LastSeen: now, Hits: 1}
	raw, _ := c.Get(ctx, c.key("affinity", fingerprint))
	if prev := parseAffinity(raw); prev != nil {
		entry.BoundAt = prev.BoundAt
		entry.Hits = prev.Hits + 1
		if sessionFP == "" {
			sessionFP = prev.SessionFP
		}
		if promptCacheKey == "" {
			promptCacheKey = prev.PromptCacheKey
		}
	}
	entry.SessionFP = strings.TrimSpace(sessionFP)
	entry.PromptCacheKey = strings.TrimSpace(promptCacheKey)
	return c.setAffinity(ctx, fingerprint, entry, ttl)
}

func (c *Client) ClearAffinity(ctx context.Context, fingerprint string) error {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return nil
	}
	return c.Del(ctx, c.key("affinity", fingerprint))
}

func (c *Client) setAffinity(ctx context.Context, fingerprint string, entry AffinityEntry, ttl time.Duration) error {
	if ttl <= 0 {
		ttl = 2 * time.Hour
	}
	data, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	return c.SetEX(ctx, c.key("affinity", fingerprint), string(data), int(ttl.Seconds()))
}

func parseAffinity(raw string) *AffinityEntry {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	var entry AffinityEntry
	if err := json.Unmarshal([]byte(raw), &entry); err == nil && entry.AccountID != "" {
		return &entry
	}
	return &AffinityEntry{AccountID: raw, BoundAt: unixFloat(time.Now())}
}

func unixFloat(t time.Time) float64 {
	return float64(t.UnixNano()) / 1e9
}

func stringInt(value int) string {
	return strconv.Itoa(value)
}
