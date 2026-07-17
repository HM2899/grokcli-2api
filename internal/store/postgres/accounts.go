package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/hm2899/grokcli-2api/internal/accounts"
)

type AccountList struct {
	Accounts   []map[string]any `json:"accounts"`
	IDs        []string         `json:"ids,omitempty"`
	Total      int64            `json:"total"`
	Page       int              `json:"page"`
	PageSize   int              `json:"page_size"`
	TotalPages int              `json:"total_pages"`
	Query      string           `json:"q"`
	Sort       string           `json:"sort"`
	Status     string           `json:"status,omitempty"`
	HasSSO     *bool            `json:"has_sso,omitempty"`
}

// AccountListFilter controls server-side account list filters.
type AccountListFilter struct {
	Query   string
	Sort    string
	Status  string // "", live, cooldown, disabled, quota_disabled, model_blocked, expired, normal
	HasSSO  *bool
	IDsOnly bool // when true, return only matching ids (for "筛选全选")
}

func (c *Connector) ListAccountSummaries(ctx context.Context, page, pageSize int, query, sort string) (AccountList, error) {
	return c.ListAccountSummariesFiltered(ctx, page, pageSize, AccountListFilter{Query: query, Sort: sort})
}

func (c *Connector) ListAccountSummariesFiltered(ctx context.Context, page, pageSize int, filter AccountListFilter) (AccountList, error) {
	sort := normalizeAccountSort(filter.Sort)
	orderBy := accountOrderSQL(sort)
	query := strings.TrimSpace(strings.ToLower(filter.Query))
	status := normalizeAccountStatusFilter(filter.Status)
	if page < 1 {
		page = 1
	}
	if pageSize <= 0 || pageSize >= 10000 {
		pageSize = 0
	} else if pageSize > 200 && !filter.IDsOnly {
		pageSize = 200
	}
	// IDs-only selection may need a larger page; hard-cap to protect memory.
	if filter.IDsOnly && (pageSize <= 0 || pageSize > 20000) {
		pageSize = 20000
	}

	where, args := buildAccountListWhere(query, status, filter.HasSSO)

	var total int64
	countSQL := "SELECT COUNT(*) FROM accounts a LEFT JOIN account_pool ap ON ap.account_id = a.id " + where
	if err := c.Pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return AccountList{}, err
	}
	limitClause := ""
	pageOut := page
	pageSizeOut := pageSize
	totalPages := 1
	if pageSize == 0 {
		pageOut = 1
		pageSizeOut = int(total)
	} else {
		totalPages = int(math.Max(1, math.Ceil(float64(total)/float64(pageSize))))
		if pageOut > totalPages {
			pageOut = totalPages
		}
		offset := (pageOut - 1) * pageSize
		limitClause = " LIMIT $" + itoaSQL(len(args)+1) + " OFFSET $" + itoaSQL(len(args)+2)
		args = append(args, pageSize, offset)
	}

	sql := `
		SELECT a.id, a.email, a.user_id, a.team_id, a.payload, a.expires_at, a.updated_at,
		       ap.enabled, ap.weight, ap.request_count, ap.success_count, ap.fail_count,
		       ap.last_used_at, ap.last_error, ap.cooldown_until, ap.disabled_for_quota,
		       ap.disabled_reason, ap.quota_disabled_at, ap.quota_source, ap.last_quota,
		       ap.last_probe, ap.blocked_models,
		       COALESCE(ap.pool_status, 'normal'), COALESCE(ap.cooldown_count, 0),
		       ap.cooldown_reason, ap.cooldown_code, ap.cooldown_model,
		       ap.cooldown_tokens_actual, ap.cooldown_tokens_limit,
		       ap.last_probe_status, COALESCE(ap.extra, '{}'::jsonb)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id ` + where + ` ORDER BY ` + orderBy + limitClause
	rows, err := c.Pool.Query(ctx, sql, args...)
	if err != nil {
		return AccountList{}, err
	}
	defer rows.Close()

	now := time.Now()
	accounts := []map[string]any{}
	for rows.Next() {
		var id string
		var email, userID, teamID *string
		var payloadBytes []byte
		var expiresAt, updatedAt, lastUsedAt, cooldownUntil, quotaDisabledAt *time.Time
		var enabled, disabledForQuota *bool
		var weight *int
		var requestCount, successCount, failCount *int64
		var lastError, disabledReason, quotaSource *string
		var lastQuota, lastProbe, blockedModels, extraBytes []byte
		var poolStatus *string
		var cooldownCount *int64
		var cooldownReason, cooldownCode, cooldownModel, lastProbeStatus *string
		var cooldownTokensActual, cooldownTokensLimit *int64
		if err := rows.Scan(
			&id, &email, &userID, &teamID, &payloadBytes, &expiresAt, &updatedAt,
			&enabled, &weight, &requestCount, &successCount, &failCount,
			&lastUsedAt, &lastError, &cooldownUntil, &disabledForQuota,
			&disabledReason, &quotaDisabledAt, &quotaSource, &lastQuota,
			&lastProbe, &blockedModels,
			&poolStatus, &cooldownCount,
			&cooldownReason, &cooldownCode, &cooldownModel,
			&cooldownTokensActual, &cooldownTokensLimit,
			&lastProbeStatus, &extraBytes,
		); err != nil {
			return AccountList{}, err
		}
		payload := decodeMap(payloadBytes)
		extra := decodeMap(extraBytes)
		token, _ := firstString(payload, "key", "access_token", "token")
		expired := expiresAt != nil && now.After(*expiresAt)
		poolEnabled := true
		if enabled != nil {
			poolEnabled = *enabled
		}
		poolWeight := int64(1)
		if weight != nil {
			poolWeight = int64(*weight)
		}
		quotaDisabled := false
		if disabledForQuota != nil {
			quotaDisabled = *disabledForQuota
		}
		blocked := activeBlockedModels(decodeMap(blockedModels), now)
		cdRemain := cooldownRemaining(now, cooldownUntil)
		cdCount := int64OrZero(cooldownCount)
		statusStack := statusStackFromExtra(extra)
		if cdCount <= 0 && len(statusStack) > 0 {
			cdCount = int64(len(statusStack))
		}
		// Count-based cooling (Python parity) OR wall-clock cooldown_until.
		inCooldown := cdRemain > 0 || cdCount > 0 || len(statusStack) > 0
		rawStatus := ""
		if poolStatus != nil {
			rawStatus = strings.TrimSpace(*poolStatus)
		}
		status := derivePoolStatus(map[string]any{
			"pool_status":        rawStatus,
			"enabled":            poolEnabled,
			"disabled_for_quota": quotaDisabled,
			"in_cooldown":        inCooldown,
			"blocked_model_ids":  mapKeys(blocked),
			"expired":            expired,
			"last_renew_status":  stringFromMap(extra, "last_renew_status"),
			"token_expired_at":   extra["token_expired_at"],
		})
		pool := map[string]any{
			"id":                     id,
			"enabled":                poolEnabled,
			"weight":                 poolWeight,
			"request_count":          int64OrZero(requestCount),
			"success_count":          int64OrZero(successCount),
			"fail_count":             int64OrZero(failCount),
			"last_used_at":           unixOrNil(lastUsedAt),
			"last_error":             stringPtr(lastError),
			"cooldown_until":         unixOrNil(cooldownUntil),
			"cooldown_remaining_sec": cdRemain,
			"cooldown_count":         cdCount,
			"cooldown_reason":        stringPtr(cooldownReason),
			"cooldown_code":          stringPtr(cooldownCode),
			"cooldown_model":         stringPtr(cooldownModel),
			"cooldown_tokens_actual": int64PtrOrNil(cooldownTokensActual),
			"cooldown_tokens_limit":  int64PtrOrNil(cooldownTokensLimit),
			"in_cooldown":            inCooldown,
			"disabled_for_quota":     quotaDisabled,
			"disabled_reason":        stringPtr(disabledReason),
			"quota_disabled_at":      unixOrNil(quotaDisabledAt),
			"quota_source":           stringPtr(quotaSource),
			"last_quota":             decodeMap(lastQuota),
			"last_probe":             decodeMap(lastProbe),
			"last_probe_status":      stringPtr(lastProbeStatus),
			"blocked_models":         blocked,
			"blocked_model_ids":      mapKeys(blocked),
			"pool_status":            status,
			"status_stack":           statusStack,
			"consecutive_fails":      intFromMap(extra, "consecutive_fails"),
			"probe_fail_streak":      intFromMap(extra, "probe_fail_streak"),
			"token_expired_at":       extra["token_expired_at"],
			"token_expired_reason":   stringFromMap(extra, "token_expired_reason"),
			"renew_fail_count":       intFromMap(extra, "renew_fail_count"),
			"last_renew_error":       stringFromMap(extra, "last_renew_error"),
			"last_renew_status":      stringFromMap(extra, "last_renew_status"),
			"last_renew_source":      stringFromMap(extra, "last_renew_source"),
		}
		accounts = append(accounts, map[string]any{
			"id":                id,
			"email":             firstNonNilString(email, stringFromMap(payload, "email")),
			"user_id":           firstNonNilString(userID, firstMapString(payload, "user_id", "principal_id")),
			"team_id":           firstNonNilString(teamID, stringFromMap(payload, "team_id")),
			"auth_mode":         payload["auth_mode"],
			"create_time":       payload["create_time"],
			"updated_at":        unixOrNil(updatedAt),
			"expires_at":        unixOrNil(expiresAt),
			"expired":           expired,
			"has_refresh_token": strings.TrimSpace(stringFromMap(payload, "refresh_token")) != "",
			"has_sso":           hasSSO(payload),
			"token_hint":        tokenHint(token),
			"first_name":        payload["first_name"],
			"last_name":         payload["last_name"],
			"principal_type":    payload["principal_type"],
			"source":            payload["source"],
			"_pool":             pool,
		})
	}
	if err := rows.Err(); err != nil {
		return AccountList{}, err
	}
	out := AccountList{
		Accounts: accounts, Total: total, Page: pageOut, PageSize: pageSizeOut,
		TotalPages: totalPages, Query: query, Sort: sort, Status: status, HasSSO: filter.HasSSO,
	}
	if filter.IDsOnly {
		ids := make([]string, 0, len(accounts))
		for _, a := range accounts {
			if id, _ := a["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
		out.IDs = ids
		// Keep lightweight rows optional; UI primarily uses ids.
	}
	return out, nil
}

func normalizeAccountStatusFilter(status string) string {
	s := strings.ToLower(strings.TrimSpace(status))
	switch s {
	case "", "all", "*":
		return ""
	case "live", "ok", "active", "polling":
		return "live"
	case "normal":
		return "normal"
	case "cooldown", "cooling":
		return "cooldown"
	case "disabled", "disable":
		return "disabled"
	case "quota", "quota_disabled", "quota-disabled":
		return "quota_disabled"
	case "model_blocked", "model-blocked", "blocked":
		return "model_blocked"
	case "expired", "expire":
		return "expired"
	default:
		return s
	}
}

// buildAccountListWhere builds WHERE for accounts a LEFT JOIN account_pool ap.
// Status filters mirror derivePoolStatus used by the UI.
func buildAccountListWhere(query, status string, hasSSO *bool) (string, []any) {
	clauses := make([]string, 0, 4)
	args := make([]any, 0, 8)
	next := 1
	add := func(sqlFrag string, vals ...any) {
		// sqlFrag uses ? placeholders
		var b strings.Builder
		vi := 0
		for i := 0; i < len(sqlFrag); i++ {
			if sqlFrag[i] == '?' {
				b.WriteByte('$')
				b.WriteString(strconv.Itoa(next))
				next++
				vi++
				continue
			}
			b.WriteByte(sqlFrag[i])
		}
		if vi != len(vals) {
			// still append; caller bug would surface as query error
		}
		clauses = append(clauses, b.String())
		args = append(args, vals...)
	}

	if query != "" {
		like := "%" + query + "%"
		add("(lower(COALESCE(a.email,'')) LIKE ? OR lower(a.id) LIKE ? OR lower(COALESCE(a.user_id,'')) LIKE ?)", like, like, like)
	}
	if hasSSO != nil {
		// Parity with Python accounts_pg list filter + accounts.GetSSOValue shapes.
		ssoExpr := `(
			NULLIF(btrim(COALESCE(a.payload->>'sso','')), '') IS NOT NULL OR
			NULLIF(btrim(COALESCE(a.payload->>'sso_cookie','')), '') IS NOT NULL OR
			NULLIF(btrim(COALESCE(a.payload->>'sso_token','')), '') IS NOT NULL OR
			NULLIF(btrim(COALESCE(a.payload#>>'{session_cookies,sso}','')), '') IS NOT NULL OR
			NULLIF(btrim(COALESCE(a.payload#>>'{session_cookies,sso-rw}','')), '') IS NOT NULL OR
			NULLIF(btrim(COALESCE(a.payload#>>'{cookies,sso}','')), '') IS NOT NULL OR
			NULLIF(btrim(COALESCE(a.payload#>>'{cookies,sso-rw}','')), '') IS NOT NULL OR
			COALESCE(a.payload->>'cookie','') ILIKE '%sso=%' OR
			COALESCE(a.payload->>'cookies','') ILIKE '%sso=%' OR
			COALESCE(a.payload->>'set_cookie','') ILIKE '%sso=%' OR
			COALESCE(a.payload->>'set-cookie','') ILIKE '%sso=%' OR
			COALESCE(a.payload->>'set_cookies','') ILIKE '%sso=%'
		)`
		if *hasSSO {
			clauses = append(clauses, ssoExpr)
		} else {
			clauses = append(clauses, "NOT "+ssoExpr)
		}
	}
	if status != "" {
		expiredExpr := `((a.expires_at IS NOT NULL AND a.expires_at <= now()) OR COALESCE(ap.pool_status,'') = 'expired')`
		quotaExpr := `(COALESCE(ap.disabled_for_quota, false) = true OR COALESCE(ap.pool_status,'') = 'quota_disabled')`
		disabledExpr := `(COALESCE(ap.enabled, true) = false OR COALESCE(ap.pool_status,'') = 'disabled')`
		cooldownExpr := `((ap.cooldown_until IS NOT NULL AND ap.cooldown_until > now()) OR COALESCE(ap.cooldown_count,0) > 0 OR COALESCE(ap.pool_status,'') = 'cooldown')`
		// Active model block: non-empty blocked_models with at least one entry that is
		// permanent (true/object without expired until) or until > now().
		modelBlockExpr := `(
			EXISTS (
				SELECT 1
				FROM jsonb_each(COALESCE(ap.blocked_models, '{}'::jsonb)) AS e(model, value)
				WHERE
					-- permanent boolean true
					(jsonb_typeof(e.value) = 'boolean' AND e.value = 'true'::jsonb)
					-- bare unix until still in future (number)
					OR (jsonb_typeof(e.value) = 'number' AND (
						(e.value::text::float8 > 1000000000000 AND (e.value::text::float8/1000.0) > extract(epoch from now()))
						OR (e.value::text::float8 > 1577836800 AND e.value::text::float8 <= 1000000000000 AND e.value::text::float8 > extract(epoch from now()))
						OR (e.value::text::float8 > 0 AND e.value::text::float8 <= 1577836800) -- small permanent markers
					))
					-- object form {"until": ...} still active, or permanent object without expired until
					OR (jsonb_typeof(e.value) = 'object' AND (
						(e.value ? 'until' AND NULLIF(e.value->>'until','') IS NOT NULL AND (
							CASE
								WHEN (e.value->>'until')::float8 > 1000000000000 THEN ((e.value->>'until')::float8/1000.0) > extract(epoch from now())
								ELSE (e.value->>'until')::float8 > extract(epoch from now())
							END
						))
						OR (NOT (e.value ? 'until') OR NULLIF(e.value->>'until','') IS NULL)
						OR ((e.value ? 'blocked') AND (e.value->>'blocked') IN ('true','1'))
					))
			)
			OR COALESCE(ap.pool_status,'') = 'model_blocked'
		)`
		liveExpr := `(COALESCE(ap.enabled, true) = true
			AND COALESCE(ap.disabled_for_quota, false) = false
			AND NOT ` + expiredExpr + `
			AND NOT ` + cooldownExpr + `
			AND NOT ` + modelBlockExpr + `
			AND COALESCE(ap.pool_status,'normal') NOT IN ('expired','disabled','quota_disabled','cooldown','model_blocked'))`
		switch status {
		case "live":
			clauses = append(clauses, liveExpr)
		case "normal":
			clauses = append(clauses, `(COALESCE(ap.enabled, true) = true
				AND COALESCE(ap.disabled_for_quota, false) = false
				AND NOT `+cooldownExpr+`
				AND NOT `+expiredExpr+`
				AND NOT `+modelBlockExpr+`
				AND COALESCE(ap.pool_status,'normal') = 'normal')`)
		case "cooldown":
			clauses = append(clauses, `(NOT `+expiredExpr+` AND NOT `+quotaExpr+` AND COALESCE(ap.enabled, true) = true AND `+cooldownExpr+`)`)
		case "disabled":
			clauses = append(clauses, `(NOT `+quotaExpr+` AND `+disabledExpr+`)`)
		case "quota_disabled":
			clauses = append(clauses, quotaExpr)
		case "model_blocked":
			clauses = append(clauses, `(NOT `+expiredExpr+` AND NOT `+quotaExpr+` AND COALESCE(ap.enabled, true) = true AND NOT `+cooldownExpr+` AND `+modelBlockExpr+`)`)
		case "expired":
			clauses = append(clauses, expiredExpr)
		}
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(clauses, " AND "), args
}

func normalizeAccountSort(sort string) string {
	key := strings.ReplaceAll(strings.ToLower(strings.TrimSpace(sort)), "-", "_")
	switch key {
	case "old", "updated_asc":
		return "oldest"
	case "new", "updated_desc", "":
		return "newest"
	case "email_asc", "email_desc", "expires_desc", "expires_asc", "last_used_desc", "last_used_asc", "requests_desc", "cooldown_first", "disabled_first":
		return key
	default:
		return "newest"
	}
}

func accountOrderSQL(sort string) string {
	switch sort {
	case "oldest":
		return "a.updated_at ASC NULLS LAST, a.id ASC"
	case "email_asc":
		return "lower(COALESCE(a.email, '')) ASC, a.id ASC"
	case "email_desc":
		return "lower(COALESCE(a.email, '')) DESC, a.id DESC"
	case "expires_desc":
		return "a.expires_at DESC NULLS LAST, a.updated_at DESC"
	case "expires_asc":
		return "a.expires_at ASC NULLS LAST, a.updated_at DESC"
	case "last_used_desc":
		return "ap.last_used_at DESC NULLS LAST, a.updated_at DESC"
	case "last_used_asc":
		return "ap.last_used_at ASC NULLS LAST, a.updated_at DESC"
	case "requests_desc":
		return "COALESCE(ap.request_count, 0) DESC, a.updated_at DESC"
	case "cooldown_first":
		return "(CASE WHEN ap.cooldown_until IS NOT NULL AND ap.cooldown_until > now() THEN 0 ELSE 1 END) ASC, a.updated_at DESC"
	case "disabled_first":
		return "(CASE WHEN COALESCE(ap.enabled, true) = false OR COALESCE(ap.disabled_for_quota, false) = true THEN 0 ELSE 1 END) ASC, a.updated_at DESC"
	default:
		return "a.updated_at DESC NULLS LAST, a.id DESC"
	}
}

func decodeMap(data []byte) map[string]any {
	var out map[string]any
	if err := json.Unmarshal(data, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func firstString(m map[string]any, keys ...string) (string, bool) {
	for _, key := range keys {
		if s := stringFromMap(m, key); s != "" {
			return s, true
		}
	}
	return "", false
}

func firstMapString(m map[string]any, keys ...string) string {
	s, _ := firstString(m, keys...)
	return s
}

func stringFromMap(m map[string]any, key string) string {
	if value, ok := m[key].(string); ok {
		return strings.TrimSpace(value)
	}
	return ""
}

func firstNonNilString(ptr *string, fallback string) any {
	if ptr != nil && *ptr != "" {
		return *ptr
	}
	if fallback != "" {
		return fallback
	}
	return nil
}

func stringPtr(ptr *string) any {
	if ptr == nil {
		return nil
	}
	return *ptr
}

func int64OrZero(ptr *int64) int64 {
	if ptr == nil {
		return 0
	}
	return *ptr
}

func cooldownRemaining(now time.Time, until *time.Time) float64 {
	if until == nil || !until.After(now) {
		return 0
	}
	return until.Sub(now).Seconds()
}

func tokenHint(token string) string {
	if len(token) > 12 {
		return token[:6] + "..." + token[len(token)-4:]
	}
	if token != "" {
		return "****"
	}
	return ""
}

// normalizeExportPayload ensures email + canonical SSO fields are present so
// exports / sub2api / SSO download never miss nested cookie shapes.
func normalizeExportPayload(payload map[string]any, id string, email *string) map[string]any {
	if payload == nil {
		payload = map[string]any{}
	}
	if stringFromMap(payload, "email") == "" && email != nil && strings.TrimSpace(*email) != "" {
		payload["email"] = strings.TrimSpace(*email)
	}
	if stringFromMap(payload, "id") == "" && id != "" {
		payload["id"] = id
	}
	if sso := strings.TrimSpace(accounts.GetSSOValue(payload)); sso != "" {
		payload["sso"] = sso
		if stringFromMap(payload, "sso_cookie") == "" {
			payload["sso_cookie"] = sso
		}
	}
	// Mirror password aliases used by re-import / SSO text export.
	if stringFromMap(payload, "password") == "" && stringFromMap(payload, "register_password") != "" {
		payload["password"] = stringFromMap(payload, "register_password")
	}
	if stringFromMap(payload, "register_password") == "" && stringFromMap(payload, "password") != "" {
		payload["register_password"] = stringFromMap(payload, "password")
	}
	return payload
}

func hasSSO(payload map[string]any) bool {
	// Must match accounts.GetSSOValue / Python has_sso_value — nested cookies and
	// cookie-header forms count, not only top-level sso_* string fields.
	return strings.TrimSpace(accounts.GetSSOValue(payload)) != ""
}

// activeBlockedModels drops expired soft blocks so the UI / picker only see
// currently enforced model bans.
//
// Supported value shapes (Python + Go):
//
//	true | 1
//	<unix until seconds/ms>
//	{"until": <unix>, "reason": "...", "source": "...", ...}
//	{"blocked": true}
//
// Empty map / false / expired until => not blocked.
func activeBlockedModels(blocked map[string]any, now time.Time) map[string]any {
	if len(blocked) == 0 {
		return map[string]any{}
	}
	out := make(map[string]any, len(blocked))
	nowUnix := float64(now.Unix())
	for mid, entry := range blocked {
		if !modelBlockEntryActive(entry, nowUnix) {
			continue
		}
		out[mid] = entry
	}
	return out
}

func modelBlockEntryActive(entry any, nowUnix float64) bool {
	if entry == nil {
		return false
	}
	switch v := entry.(type) {
	case bool:
		return v
	case float64:
		if v <= 0 {
			// permanent-ish numeric marker used by older writers
			return true
		}
		u := v
		if u > 1e12 {
			u = u / 1000
		}
		// If value looks like a unix timestamp in the future => still blocked.
		// Heuristic: timestamps after year 2020.
		if u > 1577836800 {
			return u > nowUnix
		}
		// small numbers (1) treat as permanent true
		return true
	case float32:
		return modelBlockEntryActive(float64(v), nowUnix)
	case int:
		return modelBlockEntryActive(float64(v), nowUnix)
	case int64:
		return modelBlockEntryActive(float64(v), nowUnix)
	case json.Number:
		f, err := v.Float64()
		if err != nil {
			return true
		}
		return modelBlockEntryActive(f, nowUnix)
	case string:
		s := strings.TrimSpace(v)
		if s == "" || s == "0" || strings.EqualFold(s, "false") {
			return false
		}
		if f, err := strconv.ParseFloat(s, 64); err == nil {
			return modelBlockEntryActive(f, nowUnix)
		}
		return true
	case map[string]any:
		// Explicit false/disabled
		if b, ok := v["blocked"].(bool); ok && !b {
			return false
		}
		if until, ok := v["until"]; ok && until != nil {
			var u float64
			switch x := until.(type) {
			case float64:
				u = x
			case float32:
				u = float64(x)
			case int:
				u = float64(x)
			case int64:
				u = float64(x)
			case json.Number:
				u, _ = x.Float64()
			case string:
				u, _ = strconv.ParseFloat(x, 64)
			}
			if u > 1e12 {
				u = u / 1000
			}
			if u > 0 {
				return u > nowUnix
			}
		}
		// Object without until (or until=0) => permanent block while present.
		return true
	default:
		return true
	}
}

func derivePoolStatus(fields map[string]any) string {
	status := ""
	if v, ok := fields["pool_status"]; ok {
		switch t := v.(type) {
		case string:
			status = strings.ToLower(strings.TrimSpace(t))
		case *string:
			if t != nil {
				status = strings.ToLower(strings.TrimSpace(*t))
			}
		}
	}
	renew := ""
	if v, ok := fields["last_renew_status"].(string); ok {
		renew = strings.ToLower(strings.TrimSpace(v))
	}
	expired, _ := fields["expired"].(bool)
	if status == "expired" || expired ||
		renew == "failed" || renew == "expired" || renew == "sso_failed" ||
		renew == "no_sso_removed" || renew == "no_sso_deleted" || renew == "sso_attempt" {
		return "expired"
	}
	if fields["token_expired_at"] != nil && status == "" {
		return "expired"
	}
	if quota, _ := fields["disabled_for_quota"].(bool); quota || status == "quota_disabled" {
		return "quota_disabled"
	}
	enabled := true
	if v, ok := fields["enabled"].(bool); ok {
		enabled = v
	}
	if !enabled || status == "disabled" {
		return "disabled"
	}
	if cooling, _ := fields["in_cooldown"].(bool); cooling || status == "cooldown" {
		return "cooldown"
	}
	if ids, ok := fields["blocked_model_ids"].([]string); ok && len(ids) > 0 {
		return "model_blocked"
	}
	if status == "model_blocked" {
		return "model_blocked"
	}
	if status != "" {
		return status
	}
	return "normal"
}

func statusStackFromExtra(extra map[string]any) []any {
	if extra == nil {
		return []any{}
	}
	raw, ok := extra["status_stack"]
	if !ok || raw == nil {
		return []any{}
	}
	switch v := raw.(type) {
	case []any:
		return v
	default:
		return []any{}
	}
}

func intFromMap(m map[string]any, key string) int64 {
	if m == nil {
		return 0
	}
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case float32:
		return int64(v)
	case int:
		return int64(v)
	case int64:
		return v
	case json.Number:
		n, _ := v.Int64()
		return n
	case string:
		n, _ := strconv.ParseInt(v, 10, 64)
		return n
	default:
		return 0
	}
}

func int64PtrOrNil(ptr *int64) any {
	if ptr == nil {
		return nil
	}
	return *ptr
}

func mapKeys(value map[string]any) []string {
	keys := make([]string, 0, len(value))
	for key := range value {
		keys = append(keys, key)
	}
	return keys
}

func itoaSQL(value int) string {
	return strconv.Itoa(value)
}

type AccountAuth struct {
	ID    string
	Email string
	Token string
}

func (c *Connector) GetAccountAuth(ctx context.Context, accountID string) (*AccountAuth, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil, errors.New("account id required")
	}
	row := c.Pool.QueryRow(ctx, `SELECT id, email, payload FROM accounts WHERE id = $1`, accountID)
	var id string
	var email *string
	var payloadBytes []byte
	if err := row.Scan(&id, &email, &payloadBytes); err != nil {
		return nil, err
	}
	payload := decodeMap(payloadBytes)
	token, _ := firstString(payload, "key", "access_token", "token")
	if strings.TrimSpace(token) == "" {
		return nil, errors.New("account has no access token")
	}
	out := &AccountAuth{ID: id, Token: token}
	if email != nil {
		out.Email = *email
	} else {
		out.Email = stringFromMap(payload, "email")
	}
	return out, nil
}

// DeleteAccount removes one account and its pool row from PostgreSQL.
func (c *Connector) DeleteAccount(ctx context.Context, accountID string) (bool, error) {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return false, errors.New("account id required")
	}
	tag, err := c.Pool.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, accountID)
	if err != nil {
		return false, err
	}
	_, _ = c.Pool.Exec(ctx, `DELETE FROM account_pool WHERE account_id = $1`, accountID)
	return tag.RowsAffected() > 0, nil
}

// DeleteAccounts removes many accounts in one transaction.
func (c *Connector) DeleteAccounts(ctx context.Context, accountIDs []string) (map[string]any, error) {
	seen := map[string]struct{}{}
	ids := make([]string, 0, len(accountIDs))
	for _, raw := range accountIDs {
		id := strings.TrimSpace(raw)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	if len(ids) == 0 {
		return map[string]any{
			"removed":       []string{},
			"missing":       []string{},
			"removed_count": 0,
			"missing_count": 0,
			"requested":     0,
		}, nil
	}

	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	removed := make([]string, 0, len(ids))
	missing := make([]string, 0)
	for _, id := range ids {
		tag, err := tx.Exec(ctx, `DELETE FROM accounts WHERE id = $1`, id)
		if err != nil {
			return nil, err
		}
		if tag.RowsAffected() > 0 {
			removed = append(removed, id)
			_, _ = tx.Exec(ctx, `DELETE FROM account_pool WHERE account_id = $1`, id)
		} else {
			missing = append(missing, id)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return map[string]any{
		"removed":       removed,
		"missing":       missing,
		"removed_count": len(removed),
		"missing_count": len(missing),
		"requested":     len(ids),
	}, nil
}

// ClearAllAccounts wipes every account + pool row.
func (c *Connector) ClearAllAccounts(ctx context.Context) (int64, error) {
	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `DELETE FROM accounts`)
	if err != nil {
		return 0, err
	}
	_, _ = tx.Exec(ctx, `DELETE FROM account_pool`)
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// UpsertAccount writes one account payload and ensures a pool row exists.
func (c *Connector) UpsertAccount(ctx context.Context, accountID string, entry map[string]any) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" || entry == nil {
		return errors.New("account id and entry required")
	}
	// Preserve durable metadata from an existing row.
	var oldBytes []byte
	_ = c.Pool.QueryRow(ctx, `SELECT payload FROM accounts WHERE id = $1`, accountID).Scan(&oldBytes)
	if len(oldBytes) > 0 {
		entry = mergeDurableLocal(entry, decodeMap(oldBytes))
	} else {
		entry = mergeDurableLocal(entry, nil)
	}

	email := stringFromMap(entry, "email")
	userID := firstMapString(entry, "user_id", "principal_id")
	teamID := stringFromMap(entry, "team_id")
	var expires any
	if exp, ok := entry["expires_at"]; ok && exp != nil {
		switch v := exp.(type) {
		case float64:
			expires = time.Unix(int64(v), 0).UTC()
		case int64:
			expires = time.Unix(v, 0).UTC()
		case int:
			expires = time.Unix(int64(v), 0).UTC()
		case json.Number:
			if f, err := v.Float64(); err == nil {
				expires = time.Unix(int64(f), 0).UTC()
			}
		case string:
			if f, err := strconv.ParseFloat(strings.TrimSpace(v), 64); err == nil {
				expires = time.Unix(int64(f), 0).UTC()
			} else if t, err := time.Parse(time.RFC3339, strings.TrimSpace(v)); err == nil {
				expires = t.UTC()
			}
		}
	}
	payloadBytes, err := json.Marshal(entry)
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO accounts (id, email, user_id, team_id, payload, expires_at, updated_at)
		VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), $5::jsonb, $6, now())
		ON CONFLICT (id) DO UPDATE SET
			email = EXCLUDED.email,
			user_id = EXCLUDED.user_id,
			team_id = EXCLUDED.team_id,
			payload = EXCLUDED.payload,
			expires_at = EXCLUDED.expires_at,
			updated_at = now()
	`, accountID, email, userID, teamID, payloadBytes, expires)
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO account_pool (
			account_id, enabled, weight, disabled_for_quota, blocked_models,
			request_count, success_count, fail_count, extra, updated_at,
			pool_status, cooldown_count
		) VALUES (
			$1, true, 1, false, '{}'::jsonb,
			0, 0, 0, '{}'::jsonb, now(),
			'normal', 0
		)
		ON CONFLICT (account_id) DO NOTHING
	`, accountID)
	return err
}

// ImportNormalizedAccounts merges or replaces accounts from a normalized map.
// When merge=true, same-user collisions are removed and durable fields preserved.
func (c *Connector) ImportNormalizedAccounts(ctx context.Context, normalized map[string]map[string]any, merge bool) (map[string]any, error) {
	if len(normalized) == 0 {
		total, _ := c.CountAccounts(ctx)
		return map[string]any{
			"ok":             false,
			"error":          "no valid account entries found",
			"imported":       []any{},
			"total_accounts": total,
		}, nil
	}

	tx, err := c.Pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	if !merge {
		if _, err := tx.Exec(ctx, `DELETE FROM accounts`); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM account_pool`); err != nil {
			return nil, err
		}
	}

	imported := make([]map[string]any, 0, len(normalized))
	for aid, entry := range normalized {
		entry = cloneMapAny(entry)
		// same-user dedupe when merging
		if merge {
			uid := firstMapString(entry, "user_id", "principal_id")
			token, _ := firstString(entry, "key", "access_token", "token")
			if uid != "" || token != "" {
				// preserve durable fields from colliding rows
				rows, qerr := tx.Query(ctx, `
					SELECT id, payload FROM accounts
					WHERE id = $1
					   OR ($2 <> '' AND (user_id = $2 OR payload->>'user_id' = $2 OR payload->>'principal_id' = $2))
					   OR ($3 <> '' AND payload->>'key' = $3)
				`, aid, uid, token)
				if qerr == nil {
					for rows.Next() {
						var oldID string
						var oldBytes []byte
						if rows.Scan(&oldID, &oldBytes) == nil {
							entry = mergeDurableLocal(entry, decodeMap(oldBytes))
						}
					}
					rows.Close()
				}
				if uid != "" && token != "" {
					_, _ = tx.Exec(ctx, `
						DELETE FROM accounts
						WHERE id <> $1 AND (
							user_id = $2 OR payload->>'user_id' = $2 OR payload->>'principal_id' = $2 OR payload->>'key' = $3
						)
					`, aid, uid, token)
				} else if uid != "" {
					_, _ = tx.Exec(ctx, `
						DELETE FROM accounts
						WHERE id <> $1 AND (user_id = $2 OR payload->>'user_id' = $2 OR payload->>'principal_id' = $2)
					`, aid, uid)
				} else if token != "" {
					_, _ = tx.Exec(ctx, `DELETE FROM accounts WHERE id <> $1 AND payload->>'key' = $2`, aid, token)
				}
				_, _ = tx.Exec(ctx, `
					DELETE FROM account_pool ap
					WHERE NOT EXISTS (SELECT 1 FROM accounts a WHERE a.id = ap.account_id)
				`)
			} else {
				var oldBytes []byte
				_ = tx.QueryRow(ctx, `SELECT payload FROM accounts WHERE id = $1`, aid).Scan(&oldBytes)
				if len(oldBytes) > 0 {
					entry = mergeDurableLocal(entry, decodeMap(oldBytes))
				}
			}
		}

		email := stringFromMap(entry, "email")
		userID := firstMapString(entry, "user_id", "principal_id")
		teamID := stringFromMap(entry, "team_id")
		var expires any
		if exp, ok := entry["expires_at"]; ok && exp != nil {
			switch v := exp.(type) {
			case float64:
				expires = time.Unix(int64(v), 0).UTC()
			case int64:
				expires = time.Unix(v, 0).UTC()
			case int:
				expires = time.Unix(int64(v), 0).UTC()
			case json.Number:
				if f, err := v.Float64(); err == nil {
					expires = time.Unix(int64(f), 0).UTC()
				}
			}
		}
		payloadBytes, err := json.Marshal(entry)
		if err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO accounts (id, email, user_id, team_id, payload, expires_at, updated_at)
			VALUES ($1, NULLIF($2, ''), NULLIF($3, ''), NULLIF($4, ''), $5::jsonb, $6, now())
			ON CONFLICT (id) DO UPDATE SET
				email = EXCLUDED.email,
				user_id = EXCLUDED.user_id,
				team_id = EXCLUDED.team_id,
				payload = EXCLUDED.payload,
				expires_at = EXCLUDED.expires_at,
				updated_at = now()
		`, aid, email, userID, teamID, payloadBytes, expires); err != nil {
			return nil, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO account_pool (
				account_id, enabled, weight, disabled_for_quota, blocked_models,
				request_count, success_count, fail_count, extra, updated_at,
				pool_status, cooldown_count
			) VALUES (
				$1, true, 1, false, '{}'::jsonb,
				0, 0, 0, '{}'::jsonb, now(),
				'normal', 0
			)
			ON CONFLICT (account_id) DO NOTHING
		`, aid); err != nil {
			return nil, err
		}
		imported = append(imported, map[string]any{
			"id":                aid,
			"email":             entry["email"],
			"user_id":           entry["user_id"],
			"expires_at":        entry["expires_at"],
			"has_refresh_token": stringFromMap(entry, "refresh_token") != "",
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	total, _ := c.CountAccounts(ctx)
	return map[string]any{
		"ok":             true,
		"message":        "已导入 " + itoaSQL(len(imported)) + " 个账号",
		"imported":       imported,
		"count":          len(imported),
		"total_accounts": total,
		"merged":         merge,
	}, nil
}

// ExportAuthMap returns the full durable auth map (optionally filtered).
func (c *Connector) ExportAuthMap(ctx context.Context, accountIDs []string, includeSecrets bool) (map[string]any, error) {
	wanted := map[string]struct{}{}
	for _, id := range accountIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			wanted[id] = struct{}{}
		}
	}
	rows, err := c.Pool.Query(ctx, `SELECT id, email, payload FROM accounts ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	auth := map[string]any{}
	missing := []string{}
	for rows.Next() {
		var id string
		var email *string
		var payloadBytes []byte
		if err := rows.Scan(&id, &email, &payloadBytes); err != nil {
			return nil, err
		}
		if len(wanted) > 0 {
			if _, ok := wanted[id]; !ok {
				continue
			}
		}
		raw := decodeMap(payloadBytes)
		payload := normalizeExportPayload(raw, id, email)
		if !includeSecrets {
			hasSSO := strings.TrimSpace(accounts.GetSSOValue(payload)) != ""
			hasRT := strings.TrimSpace(stringFromMap(payload, "refresh_token")) != ""
			tok := firstMapString(payload, "key", "access_token", "token")
			for _, key := range []string{"key", "access_token", "token", "refresh_token", "sso", "sso_cookie", "sso_token", "password", "register_password", "id_token", "session_cookies", "cookies", "cookie", "set_cookie", "set-cookie", "set_cookies"} {
				delete(payload, key)
			}
			payload["has_sso"] = hasSSO
			payload["has_refresh_token"] = hasRT
			if tok != "" {
				payload["token_hint"] = tokenHint(tok)
			}
		}
		auth[id] = payload
	}
	if len(wanted) > 0 {
		for id := range wanted {
			if _, ok := auth[id]; !ok {
				missing = append(missing, id)
			}
		}
	}
	out := map[string]any{
		"ok":          true,
		"auth":        auth,
		"count":       len(auth),
		"exported_at": float64(time.Now().Unix()),
	}
	if len(wanted) > 0 {
		out["selected"] = len(wanted)
		out["missing"] = missing
	}
	return out, nil
}

func mergeDurableLocal(entry, old map[string]any) map[string]any {
	if entry == nil {
		return map[string]any{}
	}
	out := cloneMapAny(entry)
	if old == nil {
		return out
	}
	durable := []string{
		"sso", "sso_cookie", "sso_token", "session_cookies", "cookies", "cookie",
		"set_cookie", "set-cookie", "set_cookies", "password", "register_password",
		"registration_session_id", "registration_batch_id", "sso_backup_path",
		"source", "id_token", "refresh_token",
	}
	// SSO first
	if stringFromMap(out, "sso") == "" && stringFromMap(out, "sso_cookie") == "" {
		if s := firstMapString(old, "sso", "sso_cookie", "sso_token"); s != "" {
			out["sso"] = s
			out["sso_cookie"] = s
		}
	}
	for _, key := range durable {
		if (out[key] == nil || out[key] == "") && old[key] != nil && old[key] != "" {
			out[key] = old[key]
		}
	}
	if stringFromMap(out, "password") == "" && stringFromMap(out, "register_password") != "" {
		out["password"] = stringFromMap(out, "register_password")
	}
	if stringFromMap(out, "register_password") == "" && stringFromMap(out, "password") != "" {
		out["register_password"] = stringFromMap(out, "password")
	}
	return out
}

func cloneMapAny(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

// AccountRefreshRow is one durable account payload used by token maintainer.
type AccountRefreshRow struct {
	ID      string
	Email   string
	Payload map[string]any
}

// ListRefreshableAccounts returns accounts that have a refresh_token (or are near expiry).
func (c *Connector) ListRefreshableAccounts(ctx context.Context, limit int) ([]AccountRefreshRow, error) {
	if limit <= 0 {
		limit = 40
	}
	if limit > 500 {
		limit = 500
	}
	rows, err := c.Pool.Query(ctx, `
		SELECT id, email, payload
		FROM accounts
		WHERE payload ? 'refresh_token'
		   OR (expires_at IS NOT NULL AND expires_at <= now() + interval '1 hour')
		ORDER BY expires_at ASC NULLS FIRST, updated_at ASC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AccountRefreshRow, 0, limit)
	for rows.Next() {
		var id string
		var email *string
		var payloadBytes []byte
		if err := rows.Scan(&id, &email, &payloadBytes); err != nil {
			return nil, err
		}
		payload := decodeMap(payloadBytes)
		row := AccountRefreshRow{ID: id, Payload: payload}
		if email != nil {
			row.Email = *email
		} else {
			row.Email = stringFromMap(payload, "email")
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListAccountAuths returns access tokens for probe paths.
func (c *Connector) ListAccountAuths(ctx context.Context, limit int, onlyEnabled bool) ([]AccountAuth, error) {
	if limit <= 0 {
		limit = 50
	}
	// Allow large manual full-pool probes; still hard-capped to protect memory.
	if limit > 5000 {
		limit = 5000
	}
	sql := `
		SELECT a.id, a.email, a.payload
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
	`
	if onlyEnabled {
		sql += ` WHERE COALESCE(ap.enabled, true) = true AND COALESCE(ap.disabled_for_quota, false) = false `
	}
	sql += ` ORDER BY a.updated_at DESC LIMIT $1`
	rows, err := c.Pool.Query(ctx, sql, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AccountAuth, 0, limit)
	for rows.Next() {
		var id string
		var email *string
		var payloadBytes []byte
		if err := rows.Scan(&id, &email, &payloadBytes); err != nil {
			return nil, err
		}
		payload := decodeMap(payloadBytes)
		token, _ := firstString(payload, "key", "access_token", "token")
		if strings.TrimSpace(token) == "" {
			continue
		}
		item := AccountAuth{ID: id, Token: token}
		if email != nil {
			item.Email = *email
		} else {
			item.Email = stringFromMap(payload, "email")
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SaveLastProbe stores probe result snapshot on account_pool.

// ListAccountAuthsForProbe prioritizes accounts that need health checks:
// never probed / last fail / oldest probe first. Skips currently cooling accounts.
// Cap is 5000 so admin "全部模型探测" can cover large live pools in one cycle.
func (c *Connector) ListAccountAuthsForProbe(ctx context.Context, limit int) ([]AccountAuth, error) {
	if limit <= 0 {
		limit = 50
	}
	if limit > 5000 {
		limit = 5000
	}
	rows, err := c.Pool.Query(ctx, `
		SELECT a.id, a.email, a.payload
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE COALESCE(ap.enabled, true) = true
		  AND COALESCE(ap.disabled_for_quota, false) = false
		  AND (ap.cooldown_until IS NULL OR ap.cooldown_until <= now())
		  AND COALESCE(ap.pool_status, 'normal') NOT IN ('expired', 'disabled')
		  AND (a.expires_at IS NULL OR a.expires_at > now())
		ORDER BY
		  CASE
		    WHEN ap.last_probe_status IS NULL OR ap.last_probe_status = '' THEN 0
		    WHEN ap.last_probe_status = 'fail' THEN 1
		    ELSE 2
		  END ASC,
		  COALESCE((ap.last_probe->>'probed_at')::bigint, 0) ASC,
		  a.updated_at ASC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]AccountAuth, 0, limit)
	for rows.Next() {
		var id string
		var email *string
		var payloadBytes []byte
		if err := rows.Scan(&id, &email, &payloadBytes); err != nil {
			return nil, err
		}
		payload := decodeMap(payloadBytes)
		token, _ := firstString(payload, "key", "access_token", "token")
		if strings.TrimSpace(token) == "" {
			continue
		}
		item := AccountAuth{ID: id, Token: token}
		if email != nil {
			item.Email = *email
		} else {
			item.Email = stringFromMap(payload, "email")
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

// SaveLastProbesBatch upserts many last_probe snapshots in one round-trip.
// Used by concurrent model-health cycles to cut per-account write latency.
func (c *Connector) SaveLastProbesBatch(ctx context.Context, probes []map[string]any) (int, error) {
	if c == nil || c.Pool == nil || len(probes) == 0 {
		return 0, nil
	}
	// Chunk to keep SQL payload reasonable under dense full-pool probes.
	const chunk = 100
	saved := 0
	for i := 0; i < len(probes); i += chunk {
		end := i + chunk
		if end > len(probes) {
			end = len(probes)
		}
		n, err := c.saveLastProbesChunk(ctx, probes[i:end])
		saved += n
		if err != nil {
			return saved, err
		}
	}
	return saved, nil
}

func (c *Connector) saveLastProbesChunk(ctx context.Context, probes []map[string]any) (int, error) {
	if len(probes) == 0 {
		return 0, nil
	}
	// Build unnest arrays for bulk upsert.
	ids := make([]string, 0, len(probes))
	payloads := make([][]byte, 0, len(probes))
	statuses := make([]string, 0, len(probes))
	for _, probe := range probes {
		if probe == nil {
			continue
		}
		aid, _ := probe["account_id"].(string)
		aid = strings.TrimSpace(aid)
		if aid == "" {
			continue
		}
		// Skip budget-cut placeholders — they are not real probe outcomes.
		if probe["budget_cut"] == true {
			continue
		}
		raw, err := json.Marshal(probe)
		if err != nil {
			continue
		}
		status := "ok"
		if ok, _ := probe["available"].(bool); !ok {
			status = "fail"
		}
		ids = append(ids, aid)
		payloads = append(payloads, raw)
		statuses = append(statuses, status)
	}
	if len(ids) == 0 {
		return 0, nil
	}
	_, err := c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, last_probe, last_probe_status, extra, updated_at)
		SELECT x.account_id, x.last_probe, x.last_probe_status, '{}'::jsonb, now()
		FROM unnest($1::text[], $2::jsonb[], $3::text[]) AS x(account_id, last_probe, last_probe_status)
		ON CONFLICT (account_id) DO UPDATE SET
			last_probe = EXCLUDED.last_probe,
			last_probe_status = EXCLUDED.last_probe_status,
			updated_at = now()
	`, ids, payloads, statuses)
	if err != nil {
		// Fallback to per-row so a single bad payload does not drop the batch.
		n := 0
		for i := range ids {
			p := map[string]any{}
			_ = json.Unmarshal(payloads[i], &p)
			if e := c.SaveLastProbe(ctx, ids[i], p); e == nil {
				n++
			}
		}
		return n, err
	}
	return len(ids), nil
}

func (c *Connector) SaveLastProbe(ctx context.Context, accountID string, probe map[string]any) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	payload, err := json.Marshal(probe)
	if err != nil {
		return err
	}
	status := "ok"
	if ok, _ := probe["available"].(bool); !ok {
		status = "fail"
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, last_probe, last_probe_status, extra, updated_at)
		VALUES ($1, $2::jsonb, $3, '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET
			last_probe = EXCLUDED.last_probe,
			last_probe_status = EXCLUDED.last_probe_status,
			updated_at = now()
	`, accountID, payload, status)
	return err
}

// ExpireDueCooldowns clears finished cooldowns so accounts re-enter rotation.
func (c *Connector) ExpireDueCooldowns(ctx context.Context, limit int) (int64, error) {
	if limit <= 0 {
		limit = 200
	}
	tag, err := c.Pool.Exec(ctx, `
		UPDATE account_pool
		SET cooldown_until = NULL,
		    cooldown_reason = NULL,
		    cooldown_code = NULL,
		    cooldown_model = NULL,
		    pool_status = CASE WHEN enabled = false OR disabled_for_quota = true THEN 'disabled' ELSE 'normal' END,
		    updated_at = now()
		WHERE cooldown_until IS NOT NULL AND cooldown_until <= now()
		  AND account_id IN (
			SELECT account_id FROM account_pool
			WHERE cooldown_until IS NOT NULL AND cooldown_until <= now()
			LIMIT $1
		  )
	`, limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// MarkRefreshInvalid stamps permanent refresh failure on payload.
func (c *Connector) MarkRefreshInvalid(ctx context.Context, accountID, reason string) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	if len(reason) > 300 {
		reason = reason[:300]
	}
	_, err := c.Pool.Exec(ctx, `
		UPDATE accounts
		SET payload = COALESCE(payload, '{}'::jsonb) || jsonb_build_object(
			'refresh_invalid', true,
			'refresh_invalid_reason', $2::text,
			'refresh_invalid_at', extract(epoch from now())
		),
		updated_at = now()
		WHERE id = $1
	`, accountID, reason)
	return err
}

// PruneModelBlocks clears blocked_models map entries.
func (c *Connector) PruneModelBlocks(ctx context.Context) (int64, error) {
	tag, err := c.Pool.Exec(ctx, `
		UPDATE account_pool
		SET blocked_models = '{}'::jsonb, updated_at = now()
		WHERE blocked_models IS NOT NULL AND blocked_models <> '{}'::jsonb
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// NormalizeAccountKeys rewrites storage ids to https://auth.x.ai::{user_id} when possible.
func (c *Connector) NormalizeAccountKeys(ctx context.Context) (map[string]any, error) {
	rows, err := c.Pool.Query(ctx, `SELECT id, payload FROM accounts`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type row struct {
		id      string
		payload map[string]any
	}
	all := []row{}
	for rows.Next() {
		var id string
		var payloadBytes []byte
		if err := rows.Scan(&id, &payloadBytes); err != nil {
			return nil, err
		}
		all = append(all, row{id: id, payload: decodeMap(payloadBytes)})
	}
	renamed, skipped := 0, 0
	for _, r := range all {
		uid := firstMapString(r.payload, "user_id", "principal_id", "sub")
		if uid == "" {
			skipped++
			continue
		}
		newID := "https://auth.x.ai::" + uid
		if newID == r.id {
			skipped++
			continue
		}
		// upsert under new id then delete old
		if err := c.UpsertAccount(ctx, newID, r.payload); err != nil {
			return nil, err
		}
		// move pool row best-effort
		_, _ = c.Pool.Exec(ctx, `
			INSERT INTO account_pool (account_id, enabled, weight, disabled_for_quota, blocked_models, request_count, success_count, fail_count, extra, updated_at, pool_status, cooldown_count)
			SELECT $2, enabled, weight, disabled_for_quota, blocked_models, request_count, success_count, fail_count, extra, now(), pool_status, cooldown_count
			FROM account_pool WHERE account_id = $1
			ON CONFLICT (account_id) DO NOTHING
		`, r.id, newID)
		_, _ = c.DeleteAccount(ctx, r.id)
		renamed++
	}
	total, _ := c.CountAccounts(ctx)
	return map[string]any{
		"ok":      true,
		"renamed": renamed,
		"skipped": skipped,
		"total":   total,
		"message": "normalized " + itoaSQL(renamed) + " account keys",
	}, nil
}

// ListCachedQuotas returns last_quota snapshots from account_pool.
func (c *Connector) ListCachedQuotas(ctx context.Context) (map[string]any, error) {
	rows, err := c.Pool.Query(ctx, `
		SELECT a.id, a.email, a.user_id, ap.last_quota, COALESCE(ap.enabled, true), COALESCE(ap.disabled_for_quota, false)
		FROM accounts a
		LEFT JOIN account_pool ap ON ap.account_id = a.id
		WHERE ap.last_quota IS NOT NULL AND ap.last_quota <> 'null'::jsonb
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	results := []map[string]any{}
	for rows.Next() {
		var id string
		var email, userID *string
		var quotaBytes []byte
		var enabled, disabledForQuota bool
		if err := rows.Scan(&id, &email, &userID, &quotaBytes, &enabled, &disabledForQuota); err != nil {
			return nil, err
		}
		q := decodeMap(quotaBytes)
		if len(q) == 0 {
			continue
		}
		item := map[string]any{}
		for k, v := range q {
			item[k] = v
		}
		item["account_id"] = id
		if email != nil {
			item["email"] = *email
		}
		if userID != nil {
			item["user_id"] = *userID
		}
		item["cached"] = true
		item["pool_disabled"] = disabledForQuota || !enabled
		if item["ok"] == nil {
			item["ok"] = item["error"] == nil || item["error"] == ""
		}
		results = append(results, item)
	}
	exhausted := 0
	okN := 0
	for _, r := range results {
		if r["exhausted"] == true || r["auto_disabled"] == true {
			exhausted++
		}
		if r["ok"] == true && r["exhausted"] != true {
			okN++
		}
	}
	return map[string]any{
		"ok":              true,
		"cached":          true,
		"count":           len(results),
		"ok_count":        okN,
		"exhausted_count": exhausted,
		"results":         results,
	}, nil
}

// SaveQuotaSnapshot persists last_quota for an account.
func (c *Connector) SaveQuotaSnapshot(ctx context.Context, accountID string, quota map[string]any) error {
	accountID = strings.TrimSpace(accountID)
	if accountID == "" {
		return nil
	}
	payload, err := json.Marshal(quota)
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO account_pool (account_id, last_quota, extra, updated_at)
		VALUES ($1, $2::jsonb, '{}'::jsonb, now())
		ON CONFLICT (account_id) DO UPDATE SET last_quota = EXCLUDED.last_quota, updated_at = now()
	`, accountID, payload)
	return err
}
