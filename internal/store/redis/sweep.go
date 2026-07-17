package redis

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// Model-health sweep keys (Python parity: model_health/sweep/{meta,covered}).
const (
	sweepMetaParts    = "model_health"
	sweepCoveredParts = "model_health"
	defaultSweepTTL   = 12 * 3600
)

func (c *Client) sweepMetaKey() string {
	return c.key("model_health", "sweep", "meta")
}

func (c *Client) sweepCoveredKey() string {
	return c.key("model_health", "sweep", "covered")
}

// SweepState is one non-repeat generation of background model probes.
type SweepState struct {
	Generation int64
	StartedAt  float64
	Covered    map[string]struct{}
	CoveredN   int
}

// LoadModelHealthSweep returns current sweep generation + covered account ids.
func (c *Client) LoadModelHealthSweep(ctx context.Context) (SweepState, error) {
	out := SweepState{Covered: map[string]struct{}{}}
	if c == nil || !c.Enabled() {
		return out, nil
	}
	meta, err := c.Get(ctx, c.sweepMetaKey())
	if err != nil {
		return out, err
	}
	if strings.TrimSpace(meta) != "" {
		parts := strings.SplitN(meta, "|", 2)
		if g, e := strconv.ParseInt(strings.TrimSpace(parts[0]), 10, 64); e == nil {
			out.Generation = g
		}
		if len(parts) > 1 {
			if s, e := strconv.ParseFloat(strings.TrimSpace(parts[1]), 64); e == nil {
				out.StartedAt = s
			}
		}
	}
	members, err := c.SMembers(ctx, c.sweepCoveredKey())
	if err != nil {
		// set may not exist yet
		members = nil
	}
	for _, m := range members {
		m = strings.TrimSpace(m)
		if m == "" {
			continue
		}
		out.Covered[m] = struct{}{}
	}
	out.CoveredN = len(out.Covered)
	return out, nil
}

// StartModelHealthSweep begins a new generation and clears covered set.
func (c *Client) StartModelHealthSweep(ctx context.Context, ttlSec int) (SweepState, error) {
	out := SweepState{Covered: map[string]struct{}{}}
	if c == nil || !c.Enabled() {
		now := float64(time.Now().Unix())
		out.Generation = time.Now().Unix()
		out.StartedAt = now
		return out, nil
	}
	if ttlSec < 600 {
		ttlSec = defaultSweepTTL
	}
	now := time.Now()
	gen := now.Unix()
	started := float64(now.Unix())
	_ = c.Del(ctx, c.sweepCoveredKey())
	meta := fmt.Sprintf("%d|%g", gen, started)
	if err := c.SetEX(ctx, c.sweepMetaKey(), meta, ttlSec); err != nil {
		return out, err
	}
	out.Generation = gen
	out.StartedAt = started
	return out, nil
}

// MarkModelHealthCovered adds account ids to the current sweep covered set.
// Returns best-effort new covered cardinality.
func (c *Client) MarkModelHealthCovered(ctx context.Context, accountIDs []string, ttlSec int) (int64, error) {
	if c == nil || !c.Enabled() {
		return 0, nil
	}
	ids := make([]string, 0, len(accountIDs))
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			ids = append(ids, id)
		}
	}
	if ttlSec < 600 {
		ttlSec = defaultSweepTTL
	}
	// Ensure meta exists / TTL refreshed.
	meta, _ := c.Get(ctx, c.sweepMetaKey())
	if strings.TrimSpace(meta) == "" {
		_, _ = c.StartModelHealthSweep(ctx, ttlSec)
	} else {
		_ = c.SetEX(ctx, c.sweepMetaKey(), meta, ttlSec)
	}
	if len(ids) == 0 {
		return c.SCard(ctx, c.sweepCoveredKey())
	}
	// Chunk SADD to keep RESP command size reasonable.
	const chunk = 200
	for i := 0; i < len(ids); i += chunk {
		end := i + chunk
		if end > len(ids) {
			end = len(ids)
		}
		if _, err := c.SAdd(ctx, c.sweepCoveredKey(), ttlSec, ids[i:end]...); err != nil {
			return 0, err
		}
	}
	_ = c.Expire(ctx, c.sweepCoveredKey(), ttlSec)
	return c.SCard(ctx, c.sweepCoveredKey())
}
