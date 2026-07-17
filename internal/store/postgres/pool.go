package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/pool"
)

// Short in-process candidate window cache to avoid repeated full-row scans under burst.
// TTL is intentionally tiny so cooldown/disable changes still show up quickly.
var (
	candidateCacheMu   sync.Mutex
	candidateCacheAt   time.Time
	candidateCacheData []pool.Candidate
)

const candidateCacheTTL = 400 * time.Millisecond

// GetPoolCandidate loads one account as a pick candidate (sticky TTFT / prompt-cache path).
func (c *Connector) GetPoolCandidate(ctx context.Context, accountID string) (*pool.Candidate, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, nil
	}
	row := c.Pool.QueryRow(ctx, `
		SELECT a.id, a.payload, a.email, a.user_id, a.team_id, a.expires_at,
		       COALESCE(ap.enabled, true), COALESCE(ap.disabled_for_quota, false),
		       ap.cooldown_until, COALESCE(ap.blocked_models, '{}'::jsonb),
		       COALESCE(ap.request_count, 0), COALESCE(ap.weight, 1)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE a.id = $1
		LIMIT 1`, accountID)
	var candidate pool.Candidate
	var payloadBytes, blockedBytes []byte
	var email, userID, teamID *string
	var expiresAt, cooldownUntil *time.Time
	if err := row.Scan(&candidate.ID, &payloadBytes, &email, &userID, &teamID, &expiresAt, &candidate.Enabled, &candidate.DisabledForQuota, &cooldownUntil, &blockedBytes, &candidate.RequestCount, &candidate.Weight); err != nil {
		return nil, err
	}
	payload := decodeMap(payloadBytes)
	candidate.Token, _ = firstString(payload, "key", "access_token", "token")
	candidate.Email = stringValue(email, stringFromMap(payload, "email"))
	candidate.UserID = stringValue(userID, firstMapString(payload, "user_id", "principal_id"))
	candidate.TeamID = stringValue(teamID, stringFromMap(payload, "team_id"))
	candidate.ExpiresAt = expiresAt
	candidate.CooldownUntil = cooldownUntil
	candidate.BlockedModels = decodeMap(blockedBytes)
	if strings.TrimSpace(candidate.Token) == "" {
		return nil, nil
	}
	return &candidate, nil
}

func (c *Connector) ListPoolCandidates(ctx context.Context) ([]pool.Candidate, error) {
	candidateCacheMu.Lock()
	if time.Since(candidateCacheAt) < candidateCacheTTL && len(candidateCacheData) > 0 {
		out := make([]pool.Candidate, len(candidateCacheData))
		copy(out, candidateCacheData)
		candidateCacheMu.Unlock()
		return out, nil
	}
	candidateCacheMu.Unlock()

	// Hot path: only load a small eligible window instead of the full pool.
	// Token is required and must not be expired/cooldown/disabled. LIMIT keeps
	// picker work and JSON payload decode cheap while preserving least_used order.
	rows, err := c.Pool.Query(ctx, `
		SELECT a.id, a.payload, a.email, a.user_id, a.team_id, a.expires_at,
		       COALESCE(ap.enabled, true), COALESCE(ap.disabled_for_quota, false),
		       ap.cooldown_until, COALESCE(ap.blocked_models, '{}'::jsonb),
		       COALESCE(ap.request_count, 0), COALESCE(ap.weight, 1)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE COALESCE(ap.enabled, true) = true
		  AND COALESCE(ap.disabled_for_quota, false) = false
		  AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
		  AND (a.expires_at IS NULL OR a.expires_at > now())
		  AND (
		        COALESCE(a.payload->>'key', '') <> ''
		     OR COALESCE(a.payload->>'access_token', '') <> ''
		     OR COALESCE(a.payload->>'token', '') <> ''
		  )
		ORDER BY COALESCE(ap.weight, 1) DESC, COALESCE(ap.fail_count, 0) ASC, COALESCE(ap.request_count, 0) ASC, a.id ASC
		LIMIT 32`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []pool.Candidate{}
	for rows.Next() {
		var candidate pool.Candidate
		var payloadBytes, blockedBytes []byte
		var email, userID, teamID *string
		var expiresAt, cooldownUntil *time.Time
		if err := rows.Scan(&candidate.ID, &payloadBytes, &email, &userID, &teamID, &expiresAt, &candidate.Enabled, &candidate.DisabledForQuota, &cooldownUntil, &blockedBytes, &candidate.RequestCount, &candidate.Weight); err != nil {
			return nil, err
		}
		payload := decodeMap(payloadBytes)
		candidate.Token, _ = firstString(payload, "key", "access_token", "token")
		candidate.Email = stringValue(email, stringFromMap(payload, "email"))
		candidate.UserID = stringValue(userID, firstMapString(payload, "user_id", "principal_id"))
		candidate.TeamID = stringValue(teamID, stringFromMap(payload, "team_id"))
		candidate.ExpiresAt = expiresAt
		candidate.CooldownUntil = cooldownUntil
		candidate.BlockedModels = decodeMap(blockedBytes)
		if strings.TrimSpace(candidate.Token) != "" {
			out = append(out, candidate)
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}
	candidateCacheMu.Lock()
	candidateCacheData = append([]pool.Candidate(nil), out...)
	candidateCacheAt = time.Now()
	candidateCacheMu.Unlock()
	return out, nil
}

// InvalidateCandidateCache drops the short-lived pick window (after kick/disable).
func (c *Connector) InvalidateCandidateCache() {
	candidateCacheMu.Lock()
	candidateCacheData = nil
	candidateCacheAt = time.Time{}
	candidateCacheMu.Unlock()
}

type PoolFailure struct {
	AccountID            string
	Error                string
	StatusCode           *int
	CooldownUntil        *time.Time
	CooldownReason       string
	CooldownCode         string
	CooldownModel        string
	CooldownTokensActual *int64
	CooldownTokensLimit  *int64
	BlockedModel         string
	BlockedUntil         *time.Time
	Detail               map[string]any
}

func (c *Connector) ReportPoolSuccess(ctx context.Context, accountID string, preserveCooldown bool) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	if preserveCooldown {
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, request_count, success_count, last_used_at, extra, updated_at)
			VALUES ($1, 1, 1, now(), '{}'::jsonb, now())
			ON CONFLICT (account_id) DO UPDATE SET
				request_count = account_pool.request_count + 1,
				success_count = account_pool.success_count + 1,
				last_used_at = now(),
				extra = jsonb_set(COALESCE(account_pool.extra, '{}'::jsonb), '{consecutive_fails}', '0'::jsonb, true),
				pool_status = CASE WHEN account_pool.cooldown_until IS NOT NULL AND account_pool.cooldown_until > now() THEN 'cooldown' ELSE account_pool.pool_status END,
				updated_at = now()`, accountID)
		return err
	}
	_, err := c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, request_count, success_count, last_used_at, pool_status, extra, updated_at)
		VALUES ($1, 1, 1, now(), 'normal', '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET
			request_count = account_pool.request_count + 1,
			success_count = account_pool.success_count + 1,
			last_used_at = now(),
			last_error = NULL,
			cooldown_until = NULL,
			cooldown_reason = NULL,
			cooldown_code = NULL,
			cooldown_model = NULL,
			cooldown_tokens_actual = NULL,
			cooldown_tokens_limit = NULL,
			pool_status = CASE WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled' ELSE 'normal' END,
			extra = jsonb_set(COALESCE(account_pool.extra, '{}'::jsonb), '{consecutive_fails}', '0'::jsonb, true),
			updated_at = now()`, accountID)
	return err
}

func (c *Connector) ReportPoolFailure(ctx context.Context, failure PoolFailure) error {
	failure.AccountID = strings.TrimSpace(failure.AccountID)
	if failure.AccountID == "" {
		return nil
	}
	detailBytes, err := json.Marshal(failure.Detail)
	if err != nil {
		return err
	}
	blockedBytes := []byte(`{}`)
	if model := strings.TrimSpace(failure.BlockedModel); model != "" {
		blocked := map[string]any{model: true}
		if failure.BlockedUntil != nil {
			blocked[model] = failure.BlockedUntil.Unix()
		}
		blockedBytes, err = json.Marshal(blocked)
		if err != nil {
			return err
		}
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO account_pool (
			account_id, request_count, fail_count, last_used_at, last_error,
			cooldown_until, pool_status, cooldown_count, cooldown_reason,
			cooldown_code, cooldown_model, cooldown_tokens_actual, cooldown_tokens_limit,
			blocked_models, extra, updated_at
		) VALUES (
			$1, 1, 1, now(), $2,
			$3, CASE WHEN $3::timestamptz IS NULL THEN 'normal' ELSE 'cooldown' END,
			CASE WHEN $3::timestamptz IS NULL THEN 0 ELSE 1 END, $4,
			$5, $6, $7, $8,
			$9::jsonb, jsonb_build_object('last_status_code', $10::int, 'cooldown_detail', $11::jsonb, 'consecutive_fails', 1), now()
		)
		ON CONFLICT (account_id) DO UPDATE SET
			request_count = account_pool.request_count + 1,
			fail_count = account_pool.fail_count + 1,
			last_used_at = now(),
			last_error = COALESCE($2, account_pool.last_error),
			cooldown_until = COALESCE($3, account_pool.cooldown_until),
			pool_status = CASE WHEN COALESCE($3, account_pool.cooldown_until) IS NOT NULL AND COALESCE($3, account_pool.cooldown_until) > now() THEN 'cooldown' WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled' ELSE 'normal' END,
			cooldown_count = account_pool.cooldown_count + CASE WHEN $3::timestamptz IS NULL THEN 0 ELSE 1 END,
			cooldown_reason = COALESCE($4, account_pool.cooldown_reason),
			cooldown_code = COALESCE($5, account_pool.cooldown_code),
			cooldown_model = COALESCE($6, account_pool.cooldown_model),
			cooldown_tokens_actual = COALESCE($7, account_pool.cooldown_tokens_actual),
			cooldown_tokens_limit = COALESCE($8, account_pool.cooldown_tokens_limit),
			blocked_models = COALESCE(account_pool.blocked_models, '{}'::jsonb) || $9::jsonb,
			extra = COALESCE(account_pool.extra, '{}'::jsonb) || jsonb_build_object(
				'last_status_code', $10::int,
				'cooldown_detail', $11::jsonb,
				'consecutive_fails', COALESCE((account_pool.extra->>'consecutive_fails')::int, 0) + 1
			),
			updated_at = now()`, failure.AccountID, nilIfEmpty(failure.Error), failure.CooldownUntil, nilIfEmpty(failure.CooldownReason), nilIfEmpty(failure.CooldownCode), nilIfEmpty(failure.CooldownModel), failure.CooldownTokensActual, failure.CooldownTokensLimit, blockedBytes, failure.StatusCode, detailBytes)
	return err
}

func (c *Connector) BlockPoolModel(ctx context.Context, accountID, model string, until *time.Time) error {
	accountID = strings.TrimSpace(accountID)
	model = strings.TrimSpace(model)
	if accountID == "" || model == "" {
		return nil
	}
	// Match Python account_pool.block_model shape so UI/picker/SQL all agree.
	entry := map[string]any{
		"blocked_at": float64(time.Now().Unix()),
		"source":     "go_model_health",
	}
	if until != nil {
		u := float64(until.Unix())
		entry["until"] = u
		entry["ttl_sec"] = u - float64(time.Now().Unix())
		entry["reason"] = "model temporarily blocked"
	} else {
		entry["blocked"] = true
		entry["reason"] = "model permanently blocked"
	}
	blocked, err := json.Marshal(map[string]any{model: entry})
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, blocked_models, pool_status, extra, updated_at)
		VALUES ($1, $2::jsonb, 'model_blocked', '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET
			blocked_models = COALESCE(account_pool.blocked_models, '{}'::jsonb) || $2::jsonb,
			pool_status = CASE
				WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled'
				WHEN account_pool.cooldown_until IS NOT NULL AND account_pool.cooldown_until > now() THEN 'cooldown'
				ELSE 'model_blocked'
			END,
			updated_at = now()`, accountID, blocked)
	if err == nil {
		c.InvalidateCandidateCache()
	}
	return err
}

// UnblockPoolModel removes one model (or all when model empty) from blocked_models.
func (c *Connector) UnblockPoolModel(ctx context.Context, accountID, model string) error {
	accountID = strings.TrimSpace(accountID)
	model = strings.TrimSpace(model)
	if accountID == "" {
		return nil
	}
	if model == "" {
		_, err := c.Pool.Exec(ctx, `
			UPDATE account_pool
			SET blocked_models = '{}'::jsonb,
			    pool_status = CASE
					WHEN enabled = false OR disabled_for_quota = true THEN 'disabled'
					WHEN cooldown_until IS NOT NULL AND cooldown_until > now() THEN 'cooldown'
					ELSE 'normal'
				END,
			    updated_at = now()
			WHERE account_id = $1`, accountID)
		return err
	}
	_, err := c.Pool.Exec(ctx, `
		UPDATE account_pool
		SET blocked_models = COALESCE(blocked_models, '{}'::jsonb) - $2,
		    pool_status = CASE
				WHEN enabled = false OR disabled_for_quota = true THEN 'disabled'
				WHEN cooldown_until IS NOT NULL AND cooldown_until > now() THEN 'cooldown'
				WHEN (COALESCE(blocked_models, '{}'::jsonb) - $2) = '{}'::jsonb THEN 'normal'
				ELSE 'model_blocked'
			END,
		    updated_at = now()
		WHERE account_id = $1`, accountID, model)
	if err == nil {
		c.InvalidateCandidateCache()
	}
	return err
}

func stringValue(ptr *string, fallback string) string {
	if ptr != nil && strings.TrimSpace(*ptr) != "" {
		return *ptr
	}
	return fallback
}

// SetAccountEnabled toggles pool enabled flag. Re-enable clears cooldown/quota/model blocks.
func (c *Connector) SetAccountEnabled(ctx context.Context, accountID string, enabled bool) (map[string]any, error) {
	c.InvalidateCandidateCache()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	if err := c.ensureAccountExists(ctx, accountID); err != nil {
		return nil, err
	}
	if enabled {
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, pool_status, extra, updated_at)
			VALUES ($1, true, 'normal', '{}'::jsonb, now())
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = true,
				disabled_for_quota = false,
				disabled_reason = NULL,
				quota_disabled_at = NULL,
				quota_source = NULL,
				blocked_models = '{}'::jsonb,
				cooldown_until = NULL,
				cooldown_reason = NULL,
				cooldown_code = NULL,
				cooldown_model = NULL,
				cooldown_tokens_actual = NULL,
				cooldown_tokens_limit = NULL,
				last_error = NULL,
				pool_status = 'normal',
				extra = jsonb_set(COALESCE(account_pool.extra, '{}'::jsonb), '{consecutive_fails}', '0'::jsonb, true),
				updated_at = now()
		`, accountID)
		if err != nil {
			return nil, err
		}
	} else {
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, pool_status, extra, updated_at)
			VALUES ($1, false, 'disabled', '{}'::jsonb, now())
			ON CONFLICT (account_id) DO UPDATE SET
				enabled = false,
				pool_status = 'disabled',
				updated_at = now()
		`, accountID)
		if err != nil {
			return nil, err
		}
	}
	return c.GetAccountPoolView(ctx, accountID)
}

// ClearAccountCooldown clears durable cooldown so the account re-enters rotation.
func (c *Connector) ClearAccountCooldown(ctx context.Context, accountID string) (map[string]any, error) {
	c.InvalidateCandidateCache()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	if err := c.ensureAccountExists(ctx, accountID); err != nil {
		return nil, err
	}
	_, err := c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, pool_status, extra, updated_at)
		VALUES ($1, 'normal', '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET
			cooldown_until = NULL,
			cooldown_reason = NULL,
			cooldown_code = NULL,
			cooldown_model = NULL,
			cooldown_tokens_actual = NULL,
			cooldown_tokens_limit = NULL,
			last_error = NULL,
			pool_status = CASE
				WHEN account_pool.enabled = false OR account_pool.disabled_for_quota = true THEN 'disabled'
				ELSE 'normal'
			END,
			updated_at = now()
	`, accountID)
	if err != nil {
		return nil, err
	}
	return c.GetAccountPoolView(ctx, accountID)
}

// KickFromPool temporarily cools down or hard-disables an account.
// cooldownSec > 0: temporary cooldown; otherwise enabled=false.
func (c *Connector) KickFromPool(ctx context.Context, accountID, reason string, cooldownSec *float64) (map[string]any, error) {
	c.InvalidateCandidateCache()
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	if err := c.ensureAccountExists(ctx, accountID); err != nil {
		return nil, err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "手动移出轮询"
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	if cooldownSec != nil && *cooldownSec > 0 {
		// stack cooldown count when possible
		var prevCount int64
		_ = c.Pool.QueryRow(ctx, `SELECT COALESCE(cooldown_count, 0) FROM account_pool WHERE account_id = $1`, accountID).Scan(&prevCount)
		newCount := prevCount + 1
		if newCount < 1 {
			newCount = 1
		}
		until := time.Now().Add(time.Duration(maxFloat(*cooldownSec, 60)*float64(newCount)) * time.Second)
		_, err := c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, pool_status, cooldown_until, cooldown_reason, cooldown_count, last_error, extra, updated_at)
			VALUES ($1, true, 'cooldown', $2, $3, $4::int, $3, jsonb_build_object('cooldown_count', $4::int), now())
			ON CONFLICT (account_id) DO UPDATE SET
				pool_status = 'cooldown',
				cooldown_until = EXCLUDED.cooldown_until,
				cooldown_reason = EXCLUDED.cooldown_reason,
				cooldown_count = EXCLUDED.cooldown_count,
				last_error = EXCLUDED.last_error,
				extra = COALESCE(account_pool.extra, '{}'::jsonb) || jsonb_build_object('cooldown_count', EXCLUDED.cooldown_count),
				updated_at = now()
		`, accountID, until, reason, int(newCount))
		if err != nil {
			return nil, err
		}
		return c.GetAccountPoolView(ctx, accountID)
	}
	return c.SetAccountEnabled(ctx, accountID, false)
}

func (c *Connector) ensureAccountExists(ctx context.Context, accountID string) error {
	var exists bool
	if err := c.Pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM accounts WHERE id = $1)`, accountID).Scan(&exists); err != nil {
		return err
	}
	if !exists {
		return errAccountNotFound
	}
	return nil
}

var errAccountNotFound = errors.New("account not found")

func IsAccountNotFound(err error) bool {
	return err != nil && (err == errAccountNotFound || strings.Contains(err.Error(), "account not found"))
}

func (c *Connector) GetAccountPoolView(ctx context.Context, accountID string) (map[string]any, error) {
	accountID = strings.TrimSpace(accountID)
	row := c.Pool.QueryRow(ctx, `
		SELECT a.id, a.email, a.user_id, a.team_id, a.expires_at, a.updated_at,
		       COALESCE(ap.enabled, true), COALESCE(ap.weight, 1), COALESCE(ap.request_count, 0),
		       COALESCE(ap.success_count, 0), COALESCE(ap.fail_count, 0), ap.last_used_at, ap.last_error,
		       ap.cooldown_until, COALESCE(ap.disabled_for_quota, false), ap.disabled_reason,
		       ap.quota_disabled_at, COALESCE(ap.pool_status, 'normal'), COALESCE(ap.cooldown_count, 0),
		       COALESCE(ap.blocked_models, '{}'::jsonb)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE a.id = $1
	`, accountID)
	var id string
	var email, userID, teamID, lastError, disabledReason, poolStatus *string
	var expiresAt, updatedAt, lastUsedAt, cooldownUntil, quotaDisabledAt *time.Time
	var enabled, disabledForQuota bool
	var weight, requestCount, successCount, failCount, cooldownCount int64
	var blockedBytes []byte
	if err := row.Scan(&id, &email, &userID, &teamID, &expiresAt, &updatedAt, &enabled, &weight, &requestCount, &successCount, &failCount, &lastUsedAt, &lastError, &cooldownUntil, &disabledForQuota, &disabledReason, &quotaDisabledAt, &poolStatus, &cooldownCount, &blockedBytes); err != nil {
		return nil, err
	}
	now := time.Now()
	inCooldown := cooldownUntil != nil && cooldownUntil.After(now) || cooldownCount > 0
	rawStatus := "normal"
	if poolStatus != nil && strings.TrimSpace(*poolStatus) != "" {
		rawStatus = strings.TrimSpace(*poolStatus)
	}
	blocked := activeBlockedModels(decodeMap(blockedBytes), now)
	status := derivePoolStatus(map[string]any{
		"pool_status":        rawStatus,
		"enabled":            enabled,
		"disabled_for_quota": disabledForQuota,
		"in_cooldown":        inCooldown,
		"blocked_model_ids":  mapKeys(blocked),
		"expired":            expiresAt != nil && now.After(*expiresAt),
	})
	out := map[string]any{
		"id":                     id,
		"email":                  stringPtr(email),
		"user_id":                stringPtr(userID),
		"team_id":                stringPtr(teamID),
		"enabled":                enabled,
		"weight":                 weight,
		"request_count":          requestCount,
		"success_count":          successCount,
		"fail_count":             failCount,
		"last_used_at":           unixOrNil(lastUsedAt),
		"last_error":             stringPtr(lastError),
		"cooldown_until":         unixOrNil(cooldownUntil),
		"cooldown_remaining_sec": cooldownRemaining(now, cooldownUntil),
		"in_cooldown":            inCooldown,
		"disabled_for_quota":     disabledForQuota,
		"disabled_reason":        stringPtr(disabledReason),
		"quota_disabled_at":      unixOrNil(quotaDisabledAt),
		"pool_status":            status,
		"cooldown_count":         cooldownCount,
		"blocked_models":         blocked,
		"blocked_model_ids":      mapKeys(blocked),
		"expires_at":             unixOrNil(expiresAt),
		"updated_at":             unixOrNil(updatedAt),
	}
	return out, nil
}

func maxFloat(v, min float64) float64 {
	if v < min {
		return min
	}
	return v
}

func (c *Connector) CountEnabledAccounts(ctx context.Context) (int64, error) {
	var n int64
	err := c.Pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE COALESCE(ap.enabled, true) = true
		  AND COALESCE(ap.disabled_for_quota, false) = false
	`).Scan(&n)
	return n, err
}
