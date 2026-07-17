// Package accounts implements durable account import/export helpers for the Go runtime.
// Account rows live in PostgreSQL; SSO conversion / registration / captcha stay in Python.
package accounts

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

var ssoCookieRE = regexp.MustCompile(`(?i)(?:^|[;,\s])sso(?:-rw)?=([^;,\s]+)`)

var durableFields = []string{
	"sso", "sso_cookie", "sso_token", "session_cookies", "cookies", "cookie",
	"set_cookie", "set-cookie", "set_cookies", "password", "register_password",
	"registration_session_id", "registration_batch_id", "sso_backup_path",
	"source", "id_token", "refresh_token",
}

// NormalizeResult is the dry-run parse of an import payload.
type NormalizeResult struct {
	OK         bool
	Error      string
	Format     string
	Normalized map[string]map[string]any
}

// CollectNormalizedEntries parses JWT / auth.json / single-token objects into a
// normalized account map. CLIProxyAPI bundles are accepted when they already look
// like token objects; full CPA-specific edge cases remain best-effort.
func CollectNormalizedEntries(raw any) NormalizeResult {
	parsed, err := coerceParsed(raw)
	if err != nil {
		return NormalizeResult{Error: err.Error()}
	}
	if parsed == nil {
		return NormalizeResult{Error: "empty payload"}
	}

	// list of account objects
	if list, ok := parsed.([]any); ok {
		normalized := map[string]map[string]any{}
		for _, item := range list {
			obj, ok := item.(map[string]any)
			if !ok {
				continue
			}
			// CPA-ish single records
			if isCLIProxyRecord(obj) {
				ent, err := cliproxyRecordToEntry(obj)
				if err != nil {
					continue
				}
				pref := firstNonEmpty(stringField(obj, "email"), stringField(obj, "account_id"), stringField(obj, "sub"))
				aid, nent, err := normalizeEntry(ent, pref)
				if err != nil {
					continue
				}
				normalized[aid] = nent
				continue
			}
			aid, nent, err := normalizeEntry(obj, firstNonEmpty(stringField(obj, "account_id"), stringField(obj, "id")))
			if err != nil {
				continue
			}
			normalized[aid] = nent
		}
		if len(normalized) == 0 {
			return NormalizeResult{Error: "no valid account entries found"}
		}
		return NormalizeResult{OK: true, Normalized: normalized, Format: "list"}
	}

	obj, ok := parsed.(map[string]any)
	if !ok {
		return NormalizeResult{Error: "payload must be object, list, or JWT string"}
	}

	// export wrapper {auth: {...}}
	if authMap, ok := obj["auth"].(map[string]any); ok &&
		obj["key"] == nil && obj["access_token"] == nil && obj["token"] == nil {
		if len(authMap) == 0 {
			return NormalizeResult{OK: true, Normalized: map[string]map[string]any{}}
		}
		if allDictValues(authMap) {
			obj = authMap
		}
	}

	// CLIProxyAPI single record
	if isCLIProxyRecord(obj) {
		ent, err := cliproxyRecordToEntry(obj)
		if err != nil {
			return NormalizeResult{Error: err.Error()}
		}
		pref := firstNonEmpty(stringField(obj, "email"), stringField(obj, "account_id"), stringField(obj, "sub"))
		aid, nent, err := normalizeEntry(ent, pref)
		if err != nil {
			return NormalizeResult{Error: err.Error()}
		}
		return NormalizeResult{
			OK:         true,
			Normalized: map[string]map[string]any{aid: nent},
			Format:     "cliproxyapi",
		}
	}

	// CPA bundle
	if strings.EqualFold(stringField(obj, "type"), "cliproxyapi-auth-bundle") {
		accounts, _ := obj["accounts"].([]any)
		normalized := map[string]map[string]any{}
		for _, item := range accounts {
			rec, ok := item.(map[string]any)
			if !ok {
				continue
			}
			ent, err := cliproxyRecordToEntry(rec)
			if err != nil {
				continue
			}
			pref := firstNonEmpty(stringField(rec, "email"), stringField(rec, "account_id"), stringField(rec, "sub"))
			aid, nent, err := normalizeEntry(ent, pref)
			if err != nil {
				continue
			}
			normalized[aid] = nent
		}
		if len(normalized) == 0 {
			return NormalizeResult{Error: "CLIProxyAPI 文件中没有可用的 access_token"}
		}
		return NormalizeResult{OK: true, Normalized: normalized, Format: "cliproxyapi"}
	}

	// multi-account map
	if looksLikeAuthMap(obj) {
		normalized := map[string]map[string]any{}
		for k, v := range obj {
			ent, ok := v.(map[string]any)
			if !ok {
				continue
			}
			if tokenOf(ent) == "" {
				continue
			}
			aid, nent, err := normalizeEntry(ent, k)
			if err != nil {
				continue
			}
			normalized[aid] = nent
		}
		if len(normalized) == 0 {
			return NormalizeResult{Error: "no valid account entries found"}
		}
		return NormalizeResult{OK: true, Normalized: normalized, Format: "auth_map"}
	}

	// single token object
	token := firstNonEmpty(stringField(obj, "key"), stringField(obj, "token"), stringField(obj, "access_token"), stringField(obj, "accessToken"))
	if token == "" {
		return NormalizeResult{Error: "missing token/key. Provide JWT、auth.json 或 CLIProxyAPI auth JSON。"}
	}
	entry := map[string]any{
		"key":         token,
		"auth_mode":   firstNonEmpty(stringField(obj, "auth_mode"), "imported"),
		"create_time": firstNonEmpty(stringField(obj, "create_time"), time.Now().UTC().Format(time.RFC3339)),
	}
	if obj["expires_at"] != nil {
		entry["expires_at"] = obj["expires_at"]
	} else if obj["expired"] != nil {
		entry["expires_at"] = obj["expired"]
	}
	if rt := stringField(obj, "refresh_token"); rt != "" {
		entry["refresh_token"] = rt
	}
	for _, field := range []string{
		"email", "user_id", "team_id", "first_name", "last_name", "principal_type",
		"oidc_client_id", "oidc_issuer", "sso", "sso_cookie", "sso_token",
		"session_cookies", "cookies", "cookie", "set_cookie", "set-cookie", "set_cookies",
		"password", "register_password", "source", "registration_session_id",
		"registration_batch_id", "sso_backup_path", "id_token", "sub",
	} {
		if v, ok := obj[field]; ok && v != nil && v != "" {
			entry[field] = v
		}
	}
	if stringField(entry, "user_id") == "" && stringField(entry, "sub") != "" {
		entry["user_id"] = stringField(entry, "sub")
	}
	if GetSSOValue(entry) != "" {
		entry["sso"] = GetSSOValue(entry)
		if stringField(entry, "sso_cookie") == "" {
			entry["sso_cookie"] = GetSSOValue(entry)
		}
	}
	pref := firstNonEmpty(stringField(obj, "account_id"), stringField(obj, "id"), stringField(obj, "auth_key"))
	aid, nent, err := normalizeEntry(entry, pref)
	if err != nil {
		return NormalizeResult{Error: err.Error()}
	}
	return NormalizeResult{OK: true, Normalized: map[string]map[string]any{aid: nent}, Format: "single"}
}

func coerceParsed(raw any) (any, error) {
	switch v := raw.(type) {
	case nil:
		return nil, fmt.Errorf("empty payload")
	case string:
		text := strings.TrimSpace(v)
		if text == "" {
			return nil, fmt.Errorf("empty payload")
		}
		if strings.HasPrefix(text, "{") || strings.HasPrefix(text, "[") {
			var parsed any
			if err := json.Unmarshal([]byte(text), &parsed); err != nil {
				return nil, fmt.Errorf("invalid JSON: %w", err)
			}
			return parsed, nil
		}
		return map[string]any{"key": text}, nil
	case map[string]any, []any:
		return v, nil
	case json.RawMessage:
		var parsed any
		if err := json.Unmarshal(v, &parsed); err != nil {
			return nil, fmt.Errorf("invalid JSON: %w", err)
		}
		return parsed, nil
	default:
		// re-marshal unknown structured values
		b, err := json.Marshal(v)
		if err != nil {
			return nil, err
		}
		var parsed any
		if err := json.Unmarshal(b, &parsed); err != nil {
			return nil, err
		}
		return parsed, nil
	}
}

func looksLikeAuthMap(obj map[string]any) bool {
	if len(obj) == 0 || !allDictValues(obj) {
		return false
	}
	hasTokenChild := false
	for _, v := range obj {
		m, ok := v.(map[string]any)
		if !ok {
			continue
		}
		if tokenOf(m) != "" {
			hasTokenChild = true
			break
		}
	}
	if !hasTokenChild {
		return false
	}
	if tokenOf(obj) != "" {
		// still could be a single object that also has nested dicts; prefer single
		// only when top-level looks like auth map keys.
	}
	for k := range obj {
		ks := strings.ToLower(k)
		if strings.Contains(ks, "auth.x.ai") || strings.Contains(ks, "accounts.x.ai") || strings.Contains(k, "::") {
			return true
		}
	}
	return tokenOf(obj) == ""
}

func allDictValues(obj map[string]any) bool {
	if len(obj) == 0 {
		return false
	}
	for _, v := range obj {
		if _, ok := v.(map[string]any); !ok {
			return false
		}
	}
	return true
}

func isCLIProxyRecord(obj map[string]any) bool {
	t := strings.ToLower(stringField(obj, "type"))
	if t == "xai" || t == "codex" || t == "grok" {
		return stringField(obj, "access_token") != "" || stringField(obj, "token") != "" || stringField(obj, "key") != ""
	}
	// CPA files often omit type but include expired + refresh_token + access_token
	if stringField(obj, "access_token") != "" && (obj["expired"] != nil || stringField(obj, "refresh_token") != "") {
		return true
	}
	return false
}

func cliproxyRecordToEntry(obj map[string]any) (map[string]any, error) {
	token := firstNonEmpty(stringField(obj, "access_token"), stringField(obj, "key"), stringField(obj, "token"))
	if token == "" {
		return nil, fmt.Errorf("missing access_token")
	}
	entry := map[string]any{
		"key":       token,
		"auth_mode": "imported",
		"source":    "cliproxyapi",
	}
	if rt := stringField(obj, "refresh_token"); rt != "" {
		entry["refresh_token"] = rt
	}
	if email := stringField(obj, "email"); email != "" {
		entry["email"] = email
	}
	if sub := firstNonEmpty(stringField(obj, "sub"), stringField(obj, "account_id"), stringField(obj, "user_id")); sub != "" {
		entry["user_id"] = sub
		entry["principal_id"] = sub
	}
	if obj["expired"] != nil {
		entry["expires_at"] = obj["expired"]
	} else if obj["expires_at"] != nil {
		entry["expires_at"] = obj["expires_at"]
	}
	if idt := stringField(obj, "id_token"); idt != "" {
		entry["id_token"] = idt
	}
	return entry, nil
}

func normalizeEntry(entry map[string]any, preferredID string) (string, map[string]any, error) {
	out := cloneMap(entry)
	tok := tokenOf(out)
	if tok == "" {
		return "", nil, fmt.Errorf("missing token")
	}
	out["key"] = tok
	claims := DecodeJWTClaims(tok)

	uid := firstNonEmpty(
		stringField(out, "user_id"),
		stringField(out, "principal_id"),
		stringField(claims, "principal_id"),
		stringField(claims, "sub"),
	)
	if uid != "" {
		out["user_id"] = uid
		if stringField(out, "principal_id") == "" {
			out["principal_id"] = uid
		}
	}
	if stringField(out, "email") == "" && stringField(claims, "email") != "" {
		out["email"] = stringField(claims, "email")
	}
	if stringField(out, "team_id") == "" && stringField(claims, "team_id") != "" {
		out["team_id"] = stringField(claims, "team_id")
	}
	if stringField(out, "principal_type") == "" && stringField(claims, "principal_type") != "" {
		out["principal_type"] = stringField(claims, "principal_type")
	}
	if stringField(out, "oidc_client_id") == "" {
		cid := claims["client_id"]
		if cid == nil {
			cid = claims["aud"]
		}
		switch v := cid.(type) {
		case string:
			if strings.TrimSpace(v) != "" {
				out["oidc_client_id"] = strings.TrimSpace(v)
			}
		case []any:
			if len(v) > 0 {
				if s, ok := v[0].(string); ok && strings.TrimSpace(s) != "" {
					out["oidc_client_id"] = strings.TrimSpace(s)
				}
			}
		}
	}

	if exp := ParseExpiresAt(out["expires_at"], tok); exp != nil {
		out["expires_at"] = *exp
	}
	if stringField(out, "auth_mode") == "" {
		out["auth_mode"] = "imported"
	}
	if stringField(out, "create_time") == "" {
		out["create_time"] = time.Now().UTC().Format(time.RFC3339)
	}
	if sso := GetSSOValue(out); sso != "" {
		out["sso"] = sso
		if stringField(out, "sso_cookie") == "" {
			out["sso_cookie"] = sso
		}
	}
	if pwd := firstNonEmpty(stringField(out, "password"), stringField(out, "register_password")); pwd != "" {
		out["password"] = pwd
	}

	fallback := preferredID
	if fallback == "" {
		fallback = "https://auth.x.ai::imported-" + randomHex(5)
	}
	aid := AccountStorageID(uid, stringField(out, "oidc_client_id"), fallback)
	return aid, out, nil
}

// MergeDurableAccountFields preserves SSO/password metadata across re-import.
func MergeDurableAccountFields(entry, old map[string]any) map[string]any {
	if entry == nil {
		return entry
	}
	if old != nil {
		if GetSSOValue(entry) == "" {
			if sso := GetSSOValue(old); sso != "" {
				entry["sso"] = sso
				entry["sso_cookie"] = sso
			}
		}
		for _, key := range durableFields {
			newV := entry[key]
			oldV := old[key]
			if (newV == nil || newV == "") && oldV != nil && oldV != "" {
				entry[key] = oldV
			}
		}
	}
	if sso := GetSSOValue(entry); sso != "" {
		entry["sso"] = sso
		if stringField(entry, "sso_cookie") == "" {
			entry["sso_cookie"] = sso
		}
	}
	if stringField(entry, "password") == "" && stringField(entry, "register_password") != "" {
		entry["password"] = stringField(entry, "register_password")
	}
	if stringField(entry, "register_password") == "" && stringField(entry, "password") != "" {
		entry["register_password"] = stringField(entry, "password")
	}
	return entry
}

// GetSSOValue extracts an xAI SSO cookie from known payload shapes.
func GetSSOValue(entry map[string]any) string {
	if entry == nil {
		return ""
	}
	for _, key := range []string{"sso", "sso_cookie", "sso_token"} {
		if val := stringField(entry, key); val != "" {
			if strings.HasPrefix(strings.ToLower(val), "sso=") {
				return strings.TrimSpace(strings.SplitN(val, "=", 2)[1])
			}
			return val
		}
	}
	for _, key := range []string{"session_cookies", "cookies"} {
		if nested, ok := entry[key].(map[string]any); ok {
			for _, cookieKey := range []string{"sso", "sso-rw"} {
				if val := stringField(nested, cookieKey); val != "" {
					return val
				}
			}
		}
	}
	for _, key := range []string{"cookie", "cookies", "set_cookie", "set-cookie", "set_cookies"} {
		if val := stringField(entry, key); val != "" {
			if m := ssoCookieRE.FindStringSubmatch(val); len(m) == 2 {
				return strings.TrimSpace(m[1])
			}
		}
	}
	return ""
}

// DecodeJWTClaims decodes the JWT payload section without signature verification.
func DecodeJWTClaims(token string) map[string]any {
	parts := strings.Split(token, ".")
	if len(parts) < 2 {
		return map[string]any{}
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		// try padded std encoding fallback
		raw, err = base64.URLEncoding.DecodeString(parts[1])
		if err != nil {
			return map[string]any{}
		}
	}
	var out map[string]any
	if err := json.Unmarshal(raw, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

// ParseExpiresAt accepts unix float/int, ISO-8601, or JWT exp fallback.
func ParseExpiresAt(value any, token string) *float64 {
	switch v := value.(type) {
	case nil:
	case int:
		f := float64(v)
		return &f
	case int64:
		f := float64(v)
		return &f
	case float64:
		return &v
	case json.Number:
		if f, err := v.Float64(); err == nil {
			return &f
		}
	case string:
		s := strings.TrimSpace(v)
		if s != "" {
			if f, err := strconv.ParseFloat(s, 64); err == nil {
				return &f
			}
			if strings.HasSuffix(s, "Z") {
				s = strings.TrimSuffix(s, "Z") + "+00:00"
			}
			if t, err := time.Parse(time.RFC3339, s); err == nil {
				f := float64(t.Unix())
				return &f
			}
			if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
				f := float64(t.Unix())
				return &f
			}
		}
	}
	if token != "" {
		if exp := DecodeJWTClaims(token)["exp"]; exp != nil {
			switch e := exp.(type) {
			case float64:
				return &e
			case json.Number:
				if f, err := e.Float64(); err == nil {
					return &f
				}
			case int64:
				f := float64(e)
				return &f
			case int:
				f := float64(e)
				return &f
			}
		}
	}
	return nil
}

// AccountStorageID builds a stable multi-account storage key.
func AccountStorageID(userID, clientID, fallback string) string {
	if strings.TrimSpace(userID) != "" {
		return "https://auth.x.ai::" + strings.TrimSpace(userID)
	}
	if strings.TrimSpace(clientID) != "" {
		return "https://auth.x.ai::" + strings.TrimSpace(clientID)
	}
	if strings.TrimSpace(fallback) != "" {
		return strings.TrimSpace(fallback)
	}
	return "https://auth.x.ai::imported-" + randomHex(6)
}

func tokenOf(m map[string]any) string {
	return firstNonEmpty(stringField(m, "key"), stringField(m, "access_token"), stringField(m, "token"))
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	switch v := m[key].(type) {
	case string:
		return strings.TrimSpace(v)
	case json.Number:
		return strings.TrimSpace(v.String())
	case float64:
		return strconv.FormatFloat(v, 'f', -1, 64)
	case int:
		return strconv.Itoa(v)
	case int64:
		return strconv.FormatInt(v, 10)
	default:
		return ""
	}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func cloneMap(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func randomHex(n int) string {
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return fmt.Sprintf("%x", buf)
}
