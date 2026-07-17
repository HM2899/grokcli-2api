package postgres

import (
	"context"
	"strings"
)

type KeyStats struct {
	Total         int64 `json:"total"`
	Enabled       int64 `json:"enabled"`
	Disabled      int64 `json:"disabled"`
	TotalRequests int64 `json:"total_requests"`
}

type PoolSummary struct {
	Mode          string `json:"mode,omitempty"`
	Total         int64  `json:"total"`
	Live          int64  `json:"live"`
	Rotatable     int64  `json:"rotatable"`
	Enabled       int64  `json:"enabled"`
	InCooldown    int64  `json:"in_cooldown"`
	QuotaDisabled int64  `json:"quota_disabled"`
	ModelBlocked  int64  `json:"model_blocked"`
	Expired       int64  `json:"expired"`
	Disabled      int64  `json:"disabled"`
	Source        string `json:"source"`
}

func (c *Connector) CountAccounts(ctx context.Context) (int64, error) {
	return countQuery(ctx, c, "SELECT COUNT(*) FROM accounts")
}

func (c *Connector) CountModels(ctx context.Context, includeHidden bool) (int64, error) {
	if includeHidden {
		return countQuery(ctx, c, "SELECT COUNT(*) FROM models")
	}
	return countQuery(ctx, c, "SELECT COUNT(*) FROM models WHERE hidden = false")
}

func (c *Connector) KeyStats(ctx context.Context, legacyEnvKey bool, authRequired bool) (map[string]any, error) {
	var stats KeyStats
	err := c.Pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE enabled = true),
		       COUNT(*) FILTER (WHERE enabled = false),
		       COALESCE(SUM(request_count), 0)
		FROM api_keys`,
	).Scan(&stats.Total, &stats.Enabled, &stats.Disabled, &stats.TotalRequests)
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"total":          stats.Total,
		"enabled":        stats.Enabled,
		"disabled":       stats.Disabled,
		"total_requests": stats.TotalRequests,
		"auth_required":  authRequired,
		"legacy_env_key": legacyEnvKey,
	}, nil
}

func (c *Connector) PoolSummary(ctx context.Context) (PoolSummary, error) {
	var summary PoolSummary
	// Accurate admin pool counters:
	// - model_blocked: only currently active model soft/hard blocks (until still in future, or permanent true)
	// - expired: access token expired (accounts.expires_at) or pool_status=expired
	// - live/rotatable: enabled, not quota-disabled, not cooling, not expired
	// - in_cooldown: wall-clock cooldown still active
	err := c.Pool.QueryRow(ctx, `
		SELECT
		  COUNT(*) AS total,
		  COUNT(*) FILTER (WHERE COALESCE(ap.enabled, true)) AS enabled,
		  COUNT(*) FILTER (
		    WHERE COALESCE(ap.enabled, true)
		      AND COALESCE(ap.disabled_for_quota, false) = false
		      AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
		      AND COALESCE(ap.pool_status, 'normal') NOT IN ('expired', 'disabled')
		      AND (a.expires_at IS NULL OR a.expires_at > now())
		  ) AS live,
		  COUNT(*) FILTER (
		    WHERE COALESCE(ap.enabled, true)
		      AND COALESCE(ap.disabled_for_quota, false) = false
		      AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
		      AND COALESCE(ap.pool_status, 'normal') NOT IN ('expired', 'disabled')
		      AND (a.expires_at IS NULL OR a.expires_at > now())
		  ) AS rotatable,
		  COUNT(*) FILTER (
		    WHERE ap.cooldown_until IS NOT NULL AND ap.cooldown_until > now()
		  ) AS in_cooldown,
		  COUNT(*) FILTER (WHERE COALESCE(ap.disabled_for_quota, false)) AS quota_disabled,
		  COUNT(*) FILTER (
		    WHERE EXISTS (
		      SELECT 1
		      FROM jsonb_each(COALESCE(ap.blocked_models, '{}'::jsonb)) AS e(model, value)
		      WHERE
		        -- permanent bool true
		        (jsonb_typeof(e.value) = 'boolean' AND e.value = 'true'::jsonb)
		        -- unix timestamp number still active
		        OR (jsonb_typeof(e.value) = 'number' AND (e.value #>> '{}')::double precision > EXTRACT(EPOCH FROM now()))
		        -- object form with until still active, or missing until (durable block)
		        OR (
		          jsonb_typeof(e.value) = 'object'
		          AND (
		            (e.value ? 'until' AND COALESCE((e.value->>'until')::double precision, 0) > EXTRACT(EPOCH FROM now()))
		            OR (NOT (e.value ? 'until'))
		          )
		        )
		    )
		  ) AS model_blocked,
		  COUNT(*) FILTER (
		    WHERE COALESCE(ap.pool_status, '') = 'expired'
		       OR (a.expires_at IS NOT NULL AND a.expires_at <= now())
		  ) AS expired,
		  COUNT(*) FILTER (
		    WHERE COALESCE(ap.enabled, true) = false
		       OR COALESCE(ap.pool_status, '') = 'disabled'
		  ) AS disabled
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id`,
	).Scan(
		&summary.Total,
		&summary.Enabled,
		&summary.Live,
		&summary.Rotatable,
		&summary.InCooldown,
		&summary.QuotaDisabled,
		&summary.ModelBlocked,
		&summary.Expired,
		&summary.Disabled,
	)
	if err != nil {
		return summary, err
	}
	// Prefer configured account mode when present.
	if modeVal, err := c.GetSetting(ctx, "account_mode"); err == nil {
		switch v := modeVal.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				summary.Mode = strings.TrimSpace(v)
			}
		}
	}
	if summary.Mode == "" {
		summary.Mode = "round_robin"
	}
	summary.Source = "postgres"
	return summary, nil
}

func countQuery(ctx context.Context, c *Connector, sql string) (int64, error) {
	var count int64
	if err := c.Pool.QueryRow(ctx, sql).Scan(&count); err != nil {
		return 0, err
	}
	return count, nil
}
