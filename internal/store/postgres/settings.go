package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (c *Connector) PublicSettings(ctx context.Context) (map[string]any, error) {
	rows, err := c.Pool.Query(ctx, "SELECT key, value, EXTRACT(EPOCH FROM updated_at)::bigint FROM app_settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	values := map[string]any{}
	var maxUpdated int64
	for rows.Next() {
		var key string
		var data []byte
		var updated *int64
		if err := rows.Scan(&key, &data, &updated); err != nil {
			return nil, err
		}
		var decoded any
		if err := json.Unmarshal(data, &decoded); err != nil {
			continue
		}
		values[key] = decoded
		if updated != nil && *updated > maxUpdated {
			maxUpdated = *updated
		}
	}
	if maxUpdated > 0 {
		values["__updated_at"] = maxUpdated
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := map[string]any{
		"account_mode":                  stringSetting(values, "account_mode", "round_robin"),
		"account_modes":                 []string{"round_robin", "random", "least_used"},
		"has_admin_password":            hasAdminPassword(values),
		"setup_needed":                  !hasAdminPassword(values),
		"admin_password_from_env":       false,
		"admin_password_in_store":       hasAdminPassword(values),
		"token_maintain_enabled":        boolSetting(values, "token_maintain_enabled", false),
		"model_health_enabled":          boolSetting(values, "model_health_enabled", false),
		"reasoning_compat":              stringSetting(values, "reasoning_compat", "off"),
		"reasoning_compat_options":      []string{"off", "think_tag", "content"},
		"outbound_max_tools":            intSetting(values, "outbound_max_tools", 0),
		"outbound_max_tools_openai":     intSetting(values, "outbound_max_tools_openai", 0),
		"outbound_tool_gap_sec":         floatSetting(values, "outbound_tool_gap_sec", 0),
		"history_compact_enabled":       boolSetting(values, "history_compact_enabled", false),
		"history_compact_auto_chars":    intSetting(values, "history_compact_auto_chars", 0),
		"history_keep_tool_rounds":      intSetting(values, "history_keep_tool_rounds", 8),
		"history_max_tool_result_chars": intSetting(values, "history_max_tool_result_chars", 0),
		"debug_shell_args":              boolSetting(values, "debug_shell_args", false),
		"sse_keepalive":                 floatSetting(values, "sse_keepalive", 4),
		"conversation_affinity_enabled": boolSetting(values, "conversation_affinity_enabled", true),
		"conversation_affinity_ttl_sec": floatSetting(values, "conversation_affinity_ttl_sec", 7200),
		"token_maintain_interval_sec":   floatSetting(values, "token_maintain_interval_sec", 90),
		"token_refresh_skew_sec":        floatSetting(values, "token_refresh_skew_sec", 300),
		"model_health_interval_sec":     floatSetting(values, "model_health_interval_sec", 180),
		"model_health_auto_disable":     boolSetting(values, "model_health_auto_disable", true),
		"model_health_probe_batch":      floatSetting(values, "model_health_probe_batch", 120),
		"model_health_probe_workers":    floatSetting(values, "model_health_probe_workers", 12),
		"probe_models":                  valueOr(values, "probe_models", []string{}),
		"default_model":                 stringSetting(values, "default_model", ""),
		"registration_config":           mapSetting(values, "registration_config"),
		"outbound_proxy_config":         publicOutboundProxyConfig(mapSetting(values, "outbound_proxy_config")),
		"outbound_proxy_pool":           outboundProxyPoolSummary(mapSetting(values, "outbound_proxy_config")),
		"sub2api_config":                publicIntegrationConfig("sub2api_config", mapSetting(values, "sub2api_config")),
		"cliproxyapi_config":            publicIntegrationConfig("cliproxyapi_config", mapSetting(values, "cliproxyapi_config")),
		"updated_at":                    values["__updated_at"],
	}
	// Pool / cooldown / kick policy: always return effective defaults so the
	// admin UI "轮询 / 冷却 / 踢出策略" section is never blank. DB overrides
	// (flat keys or nested pool_policy) win when present.
	policy := defaultPoolPolicy()
	// Flat per-key settings (written by UpdateRuntimeSettings) take precedence.
	for key := range policy {
		if v, ok := values[key]; ok && v != nil {
			policy[key] = v
		}
	}
	// Nested pool_policy blob (if any) overlays last.
	if nested := mapSetting(values, "pool_policy"); len(nested) > 0 {
		for key, value := range nested {
			if value != nil {
				policy[key] = value
			}
		}
	}
	out["pool_policy"] = policy
	for key, value := range policy {
		out[key] = value
	}
	return out, nil
}

func hasAdminPassword(values map[string]any) bool {
	admin := mapSetting(values, "admin_password")
	if admin["admin_password_hash"] != nil && admin["admin_password_salt"] != nil {
		return true
	}
	return false
}

func valueOr(values map[string]any, key string, fallback any) any {
	if value, ok := values[key]; ok && value != nil {
		return value
	}
	return fallback
}

func mapSetting(values map[string]any, key string) map[string]any {
	value, ok := values[key].(map[string]any)
	if !ok || value == nil {
		return map[string]any{}
	}
	return value
}

// publicIntegrationConfig copies nested integration settings for admin UI while
// redacting secrets. Keeps auto_push_on_register / enabled so the "注册后自动入库"
// checkboxes round-trip correctly through GET /admin/api/settings.
func publicIntegrationConfig(kind string, raw map[string]any) map[string]any {
	if raw == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	switch kind {
	case "sub2api_config":
		pw, _ := out["password"].(string)
		tok, _ := out["token"].(string)
		out["has_password"] = strings.TrimSpace(pw) != "" || strings.TrimSpace(tok) != ""
		delete(out, "password")
		delete(out, "token")
		// Normalize bool so JSON null does not leave the checkbox sticky-false.
		if v, ok := out["auto_push_on_register"]; ok {
			out["auto_push_on_register"] = truthySetting(v)
		} else {
			out["auto_push_on_register"] = false
		}
		if v, ok := out["enabled"]; ok {
			out["enabled"] = truthySetting(v)
		} else {
			out["enabled"] = false
		}
	case "cliproxyapi_config":
		mk, _ := out["management_key"].(string)
		out["has_management_key"] = strings.TrimSpace(mk) != ""
		delete(out, "management_key")
		if v, ok := out["auto_push_on_register"]; ok {
			out["auto_push_on_register"] = truthySetting(v)
		} else {
			out["auto_push_on_register"] = false
		}
		if v, ok := out["enabled"]; ok {
			out["enabled"] = truthySetting(v)
		} else {
			out["enabled"] = false
		}
	}
	return out
}

// publicOutboundProxyConfig redacts proxy_password for admin settings UI.
func publicOutboundProxyConfig(raw map[string]any) map[string]any {
	if raw == nil {
		return map[string]any{}
	}
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	pw, _ := out["proxy_password"].(string)
	out["has_password"] = strings.TrimSpace(pw) != ""
	delete(out, "proxy_password")
	return out
}
func truthySetting(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.TrimSpace(strings.ToLower(t))
		return s == "1" || s == "true" || s == "yes" || s == "on"
	case float64:
		return t != 0
	case int:
		return t != 0
	case int64:
		return t != 0
	default:
		return false
	}
}

func stringSetting(values map[string]any, key, fallback string) string {
	value, ok := values[key].(string)
	if !ok || value == "" {
		return fallback
	}
	return value
}

func boolSetting(values map[string]any, key string, fallback bool) bool {
	value, ok := values[key].(bool)
	if !ok {
		return fallback
	}
	return value
}

func intSetting(values map[string]any, key string, fallback int64) int64 {
	switch value := values[key].(type) {
	case float64:
		return int64(value)
	case int64:
		return value
	case int:
		return int64(value)
	default:
		return fallback
	}
}

func floatSetting(values map[string]any, key string, fallback float64) float64 {
	switch value := values[key].(type) {
	case float64:
		return value
	case int64:
		return float64(value)
	case int:
		return float64(value)
	default:
		return fallback
	}
}

type AdminPassword struct {
	Hash string
	Salt string
}

func (c *Connector) LoadAdminPassword(ctx context.Context) (AdminPassword, error) {
	if c == nil || c.Pool == nil {
		return AdminPassword{}, errors.New("postgres unavailable")
	}
	var data []byte
	err := c.Pool.QueryRow(ctx, "SELECT value FROM app_settings WHERE key = 'admin_password'").Scan(&data)
	if err != nil {
		return AdminPassword{}, err
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return AdminPassword{}, err
	}
	hash, _ := raw["admin_password_hash"].(string)
	salt, _ := raw["admin_password_salt"].(string)
	return AdminPassword{Hash: strings.TrimSpace(hash), Salt: strings.TrimSpace(salt)}, nil
}

func (c *Connector) HasAdminPassword(ctx context.Context) (bool, error) {
	pw, err := c.LoadAdminPassword(ctx)
	if err != nil {
		// missing row means setup needed
		if errors.Is(err, pgx.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return pw.Hash != "" && pw.Salt != "", nil
}

func (c *Connector) SetAdminPassword(ctx context.Context, hash, salt string) error {
	hash = strings.TrimSpace(hash)
	salt = strings.TrimSpace(salt)
	if hash == "" || salt == "" {
		return errors.New("password hash/salt required")
	}
	payload := map[string]any{
		"admin_password_hash":       hash,
		"admin_password_salt":       salt,
		"admin_password_updated_at": float64(time.Now().Unix()),
		"admin_password_source":     "store",
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO app_settings (key, value, updated_at)
		VALUES ('admin_password', $1::jsonb, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
	`, encoded)
	return err
}

func (c *Connector) SetSetting(ctx context.Context, key string, value any) error {
	key = strings.TrimSpace(key)
	if key == "" {
		return errors.New("setting key required")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return err
	}
	_, err = c.Pool.Exec(ctx, `
		INSERT INTO app_settings (key, value, updated_at)
		VALUES ($1, $2::jsonb, now())
		ON CONFLICT (key) DO UPDATE SET value = EXCLUDED.value, updated_at = now()
	`, key, encoded)
	return err
}

// UpdateRuntimeSettings applies a partial admin settings patch to app_settings.
// Only known scalar/runtime keys are accepted; registration secrets and proxy
// credentials are intentionally not written here.
func (c *Connector) UpdateRuntimeSettings(ctx context.Context, patch map[string]any) (map[string]any, error) {
	if c == nil || c.Pool == nil {
		return nil, errors.New("postgres unavailable")
	}
	if len(patch) == 0 {
		return nil, errors.New("没有可更新的字段")
	}
	// Normalize aliases.
	if v, ok := patch["history_keep_recent_turns"]; ok {
		if _, exists := patch["history_keep_tool_rounds"]; !exists {
			patch["history_keep_tool_rounds"] = v
		}
		delete(patch, "history_keep_recent_turns")
	}
	if v, ok := patch["history_tool_result_max_chars"]; ok {
		if _, exists := patch["history_max_tool_result_chars"]; !exists {
			patch["history_max_tool_result_chars"] = v
		}
		delete(patch, "history_tool_result_max_chars")
	}

	type fieldSpec struct {
		key   string
		kind  string // string|bool|int|float
		minF  float64
		maxF  float64
		enums []string
	}
	specs := []fieldSpec{
		{key: "account_mode", kind: "string", enums: []string{"round_robin", "random", "least_used"}},
		{key: "token_maintain_enabled", kind: "bool"},
		{key: "model_health_enabled", kind: "bool"},
		{key: "reasoning_compat", kind: "string", enums: []string{"off", "think_tag", "content"}},
		{key: "outbound_max_tools", kind: "int", minF: 0, maxF: 64},
		{key: "outbound_max_tools_openai", kind: "int", minF: 0, maxF: 64},
		{key: "outbound_tool_gap_sec", kind: "float", minF: 0, maxF: 2},
		{key: "history_compact_enabled", kind: "bool"},
		{key: "history_compact_auto_chars", kind: "int", minF: 0, maxF: 5_000_000},
		{key: "history_keep_tool_rounds", kind: "int", minF: 1, maxF: 64},
		{key: "history_max_tool_result_chars", kind: "int", minF: 512, maxF: 2_000_000},
		{key: "debug_shell_args", kind: "bool"},
		{key: "sse_keepalive", kind: "float", minF: 2, maxF: 120},
		{key: "conversation_affinity_enabled", kind: "bool"},
		{key: "conversation_affinity_ttl_sec", kind: "float", minF: 60, maxF: 86400},
		{key: "token_maintain_interval_sec", kind: "float", minF: 0, maxF: 3600},
		{key: "token_refresh_skew_sec", kind: "float", minF: 0, maxF: 1800},
		{key: "model_health_interval_sec", kind: "float", minF: 0, maxF: 86400},
		{key: "model_health_auto_disable", kind: "bool"},
		{key: "model_health_probe_batch", kind: "float", minF: 1, maxF: 1000},
		{key: "model_health_probe_workers", kind: "float", minF: 1, maxF: 64},
		{key: "default_model", kind: "string"},
		{key: "max_failover_attempts", kind: "int", minF: 1, maxF: 64},
		{key: "cooldown_default_sec", kind: "float", minF: 1, maxF: 600},
		{key: "cooldown_auth_sec", kind: "float", minF: 5, maxF: 1800},
		{key: "cooldown_rate_limit_sec", kind: "float", minF: 5, maxF: 1800},
		{key: "cooldown_server_error_sec", kind: "float", minF: 1, maxF: 600},
		{key: "cooldown_max_sec", kind: "float", minF: 30, maxF: 3600},
		{key: "soft_model_block_ttl_sec", kind: "float", minF: 30, maxF: 3600},
		{key: "durable_model_block_ttl_sec", kind: "float", minF: 60, maxF: 86400},
		{key: "probe_fail_kick_streak", kind: "int", minF: 1, maxF: 20},
		{key: "probe_fail_disable_streak", kind: "int", minF: 2, maxF: 50},
		{key: "probe_kick_cooldown_sec", kind: "float", minF: 30, maxF: 7200},
	}
	byKey := map[string]fieldSpec{}
	for _, s := range specs {
		byKey[s.key] = s
	}

	applied := 0
	for key, raw := range patch {
		if raw == nil {
			continue
		}
		// probe_models accepts string or list
		if key == "probe_models" {
			var models []string
			switch v := raw.(type) {
			case string:
				for _, part := range strings.Split(v, ",") {
					part = strings.TrimSpace(part)
					if part != "" {
						models = append(models, part)
					}
				}
			case []any:
				for _, item := range v {
					if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
						models = append(models, strings.TrimSpace(s))
					}
				}
			case []string:
				for _, s := range v {
					if strings.TrimSpace(s) != "" {
						models = append(models, strings.TrimSpace(s))
					}
				}
			default:
				return nil, fmt.Errorf("probe_models must be string or list")
			}
			if err := c.SetSetting(ctx, "probe_models", models); err != nil {
				return nil, err
			}
			applied++
			continue
		}
		spec, ok := byKey[key]
		if !ok {
			// ignore unknown keys (including registration/proxy secrets)
			continue
		}
		var value any
		switch spec.kind {
		case "bool":
			b, ok := raw.(bool)
			if !ok {
				return nil, fmt.Errorf("%s must be bool", key)
			}
			value = b
		case "string":
			s, ok := raw.(string)
			if !ok {
				return nil, fmt.Errorf("%s must be string", key)
			}
			s = strings.TrimSpace(s)
			if len(spec.enums) > 0 {
				found := false
				for _, e := range spec.enums {
					if s == e {
						found = true
						break
					}
				}
				if !found {
					return nil, fmt.Errorf("%s must be one of: %s", key, strings.Join(spec.enums, ", "))
				}
			}
			if key == "default_model" && len(s) > 128 {
				s = s[:128]
			}
			value = s
		case "int":
			f, ok := asFloat(raw)
			if !ok {
				return nil, fmt.Errorf("%s must be number", key)
			}
			if f < spec.minF {
				f = spec.minF
			}
			if f > spec.maxF {
				f = spec.maxF
			}
			value = int64(f)
		case "float":
			f, ok := asFloat(raw)
			if !ok {
				return nil, fmt.Errorf("%s must be number", key)
			}
			if f < spec.minF {
				f = spec.minF
			}
			if f > spec.maxF {
				f = spec.maxF
			}
			value = f
		}
		if err := c.SetSetting(ctx, key, value); err != nil {
			return nil, err
		}
		applied++
	}

	// Outbound proxy pool fields → durable outbound_proxy_config JSON.
	proxyKeys := []string{
		"outbound_proxy", "outbound_proxy_enabled", "outbound_proxy_username",
		"outbound_proxy_password", "outbound_proxy_strategy", "outbound_proxy_config",
	}
	hasProxyPatch := false
	for _, k := range proxyKeys {
		if _, ok := patch[k]; ok {
			hasProxyPatch = true
			break
		}
	}
	if hasProxyPatch {
		current := mapSetting(map[string]any{"outbound_proxy_config": nil}, "outbound_proxy_config")
		// load existing
		if raw, err := c.GetSetting(ctx, "outbound_proxy_config"); err == nil {
			if m, ok := raw.(map[string]any); ok && m != nil {
				current = m
			}
		}
		if v, ok := patch["outbound_proxy_config"].(map[string]any); ok && v != nil {
			for k, val := range v {
				current[k] = val
			}
		}
		if v, ok := patch["outbound_proxy"]; ok {
			current["proxy"] = fmt.Sprint(v)
		}
		if v, ok := patch["outbound_proxy_enabled"].(bool); ok {
			current["enabled"] = v
		}
		if v, ok := patch["outbound_proxy_username"]; ok {
			current["proxy_username"] = fmt.Sprint(v)
		}
		if v, ok := patch["outbound_proxy_password"]; ok {
			s := fmt.Sprint(v)
			if strings.TrimSpace(s) != "" {
				current["proxy_password"] = s
			}
		}
		if v, ok := patch["outbound_proxy_strategy"]; ok {
			st := strings.ToLower(strings.TrimSpace(fmt.Sprint(v)))
			if st != "random" && st != "sticky" && st != "round_robin" {
				st = "round_robin"
			}
			current["proxy_strategy"] = st
		}
		if err := c.SetSetting(ctx, "outbound_proxy_config", current); err != nil {
			return nil, err
		}
		applied++
	}

	// Pool policy fields are stored as a single pool_policy JSON object so
	// admin UI round-trips without losing values under env-only config.
	policyKeys := []string{
		"max_failover_attempts", "cooldown_default_sec", "cooldown_auth_sec",
		"cooldown_rate_limit_sec", "cooldown_server_error_sec", "cooldown_max_sec",
		"soft_model_block_ttl_sec", "durable_model_block_ttl_sec",
		"probe_fail_kick_streak", "probe_fail_disable_streak", "probe_kick_cooldown_sec",
	}
	hasPolicy := false
	for _, k := range policyKeys {
		if _, ok := patch[k]; ok {
			hasPolicy = true
			break
		}
	}
	if hasPolicy {
		policy := map[string]any{}
		if raw, err := c.GetSetting(ctx, "pool_policy"); err == nil {
			if m, ok := raw.(map[string]any); ok && m != nil {
				policy = m
			}
		}
		for _, k := range policyKeys {
			if v, ok := patch[k]; ok && v != nil {
				// already validated/written as scalars above; mirror into policy blob
				if raw, err := c.GetSetting(ctx, k); err == nil {
					policy[k] = raw
				} else {
					policy[k] = v
				}
			}
		}
		if err := c.SetSetting(ctx, "pool_policy", policy); err != nil {
			return nil, err
		}
		applied++
	}

	if applied == 0 {
		return nil, errors.New("没有可更新的字段")
	}
	return c.PublicSettings(ctx)
}

func asFloat(value any) (float64, bool) {
	switch v := value.(type) {
	case float64:
		return v, true
	case float32:
		return float64(v), true
	case int:
		return float64(v), true
	case int64:
		return float64(v), true
	case json.Number:
		f, err := v.Float64()
		return f, err == nil
	default:
		return 0, false
	}
}

func (c *Connector) GetSetting(ctx context.Context, key string) (any, error) {
	row := c.Pool.QueryRow(ctx, `SELECT value FROM app_settings WHERE key = $1`, key)
	var data []byte
	if err := row.Scan(&data); err != nil {
		return nil, err
	}
	var decoded any
	if err := json.Unmarshal(data, &decoded); err != nil {
		return nil, err
	}
	return decoded, nil
}

// defaultPoolPolicy mirrors Python account_pool.cooldown_defaults /
// settings_store.get_pool_policy so admin UI always has numbers to show.
func defaultPoolPolicy() map[string]any {
	return map[string]any{
		"cooldown_default_sec":        float64(20),
		"cooldown_auth_sec":           float64(90),
		"cooldown_rate_limit_sec":     float64(45),
		"cooldown_server_error_sec":   float64(20),
		"cooldown_max_sec":            float64(600),
		"soft_model_block_ttl_sec":    float64(180),
		"durable_model_block_ttl_sec": float64(3600),
		"probe_fail_kick_streak":      int64(2),
		"probe_fail_disable_streak":   int64(4),
		"probe_kick_cooldown_sec":     float64(600),
		"max_failover_attempts":       int64(4),
	}
}

func outboundProxyPoolSummary(cfg map[string]any) map[string]any {
	enabled := true
	if v, ok := cfg["enabled"].(bool); ok {
		enabled = v
	}
	proxyText, _ := cfg["proxy"].(string)
	count := 0
	for _, line := range strings.Split(proxyText, "\n") {
		for _, part := range strings.FieldsFunc(line, func(r rune) bool {
			return r == ';' || r == ','
		}) {
			part = strings.TrimSpace(part)
			if part == "" || strings.HasPrefix(part, "#") {
				continue
			}
			count++
		}
	}
	strategy, _ := cfg["proxy_strategy"].(string)
	if strategy == "" {
		strategy = "round_robin"
	}
	source := "none"
	if count > 0 {
		source = "settings"
	}
	return map[string]any{
		"enabled":  enabled,
		"count":    count,
		"strategy": strategy,
		"source":   source,
		"preview":  []any{},
	}
}
