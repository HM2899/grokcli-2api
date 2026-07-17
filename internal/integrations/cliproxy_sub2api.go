package integrations

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/accounts"
)

type Store interface {
	PublicSettings(ctx context.Context) (map[string]any, error)
	SetSetting(ctx context.Context, key string, value any) error
	GetSetting(ctx context.Context, key string) (any, error)
	ExportAuthMap(ctx context.Context, accountIDs []string, includeSecrets bool) (map[string]any, error)
}

func PublicConfig(ctx context.Context, store Store, key string) map[string]any {
	out := map[string]any{"enabled": false, "base_url": ""}
	if store == nil {
		return out
	}
	// Prefer raw GetSetting so we can mark has_password without leaking secrets.
	rawAny, err := store.GetSetting(ctx, key)
	if err != nil || rawAny == nil {
		// fallback public settings map
		settings, _ := store.PublicSettings(ctx)
		if settings != nil {
			if m, ok := settings[key].(map[string]any); ok && m != nil {
				return redactIntegrationConfig(key, m)
			}
		}
		return out
	}
	raw, _ := rawAny.(map[string]any)
	if raw == nil {
		return out
	}
	return redactIntegrationConfig(key, raw)
}

func redactIntegrationConfig(key string, raw map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range raw {
		out[k] = v
	}
	if key == "sub2api_config" {
		pw := strings.TrimSpace(fmt.Sprint(out["password"]))
		out["has_password"] = pw != "" && pw != "<nil>"
		delete(out, "password")
		delete(out, "token")
	}
	if key == "cliproxyapi_config" {
		mk := strings.TrimSpace(fmt.Sprint(out["management_key"]))
		out["has_management_key"] = mk != "" && mk != "<nil>"
		delete(out, "management_key")
	}
	return out
}

func SaveConfig(ctx context.Context, store Store, key string, patch map[string]any) (map[string]any, error) {
	if store == nil {
		return nil, fmt.Errorf("store unavailable")
	}
	cur := map[string]any{}
	if raw, err := store.GetSetting(ctx, key); err == nil {
		if m, ok := raw.(map[string]any); ok {
			cur = m
		}
	}
	// merge
	for k, v := range patch {
		if k == "management_key" || k == "password" {
			if strings.TrimSpace(fmt.Sprint(v)) == "" {
				continue // keep previous secret
			}
		}
		cur[k] = v
	}
	if err := store.SetSetting(ctx, key, cur); err != nil {
		return nil, err
	}
	return PublicConfig(ctx, store, key), nil
}

func ExportCLIProxyBundle(ctx context.Context, store Store, ids []string) (map[string]any, error) {
	authMap, err := store.ExportAuthMap(ctx, ids, true)
	if err != nil {
		return nil, err
	}
	auth, _ := authMap["auth"].(map[string]any)
	accountsOut := []map[string]any{}
	skipped := 0
	for aid, raw := range auth {
		entry, _ := raw.(map[string]any)
		rec := buildCLIProxyRecord(entry, aid)
		if rec == nil {
			skipped++
			continue
		}
		accountsOut = append(accountsOut, rec)
	}
	return map[string]any{
		"type":        "cliproxyapi-auth-bundle",
		"version":     1,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"accounts":    accountsOut,
		"count":       len(accountsOut),
		"skipped":     skipped,
		"ok":          true,
	}, nil
}

func ExportSub2APIFormat(ctx context.Context, store Store, ids []string) (map[string]any, error) {
	authMap, err := store.ExportAuthMap(ctx, ids, true)
	if err != nil {
		return nil, err
	}
	auth, _ := authMap["auth"].(map[string]any)
	cfg := sub2Config(ctx, store)
	notesPrefix := firstNonEmpty(stringField(cfg, "notes_prefix"), "grokcli-2api")
	accConc := intField(cfg, "account_concurrency", 3)
	if accConc < 1 {
		accConc = 3
	}
	if accConc > 100 {
		accConc = 100
	}
	accPrio := intField(cfg, "account_priority", 50)
	if accPrio < 0 {
		accPrio = 0
	}
	if accPrio > 100 {
		accPrio = 100
	}
	accRate := floatField(cfg, "account_rate_multiplier", 1)
	if accRate < 0.1 {
		accRate = 0.1
	}
	if accRate > 10 {
		accRate = 10
	}

	dataAccounts := []map[string]any{}
	skipped := 0
	for aid, raw := range auth {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			skipped++
			continue
		}
		access := firstNonEmpty(stringField(entry, "key"), stringField(entry, "access_token"), stringField(entry, "token"))
		if access == "" {
			skipped++
			continue
		}
		refresh := stringField(entry, "refresh_token")
		email := stringField(entry, "email")
		name := firstNonEmpty(email, aid)
		credentials := map[string]any{
			"access_token": access,
			"email":        email,
		}
		if refresh != "" {
			credentials["refresh_token"] = refresh
		}
		row := map[string]any{
			"name":            trimLen(name, 200),
			"notes":           notesPrefix + ":" + aid,
			"platform":        "grok",
			"type":            "oauth",
			"credentials":     credentials,
			"extra":           map[string]any{"email": email, "local_account_id": aid, "source": "grokcli-2api"},
			"concurrency":     accConc,
			"priority":        accPrio,
			"rate_multiplier": accRate,
		}
		if exp := accounts.ParseExpiresAt(entry["expires_at"], access); exp != nil {
			row["expires_at"] = int64(*exp) // unix seconds for sub2api DataAccount
		}
		dataAccounts = append(dataAccounts, row)
	}
	return map[string]any{
		"type":        "sub2api-data",
		"version":     1,
		"exported_at": time.Now().UTC().Format(time.RFC3339),
		"proxies":     []any{},
		"accounts":    dataAccounts,
		"count":       len(dataAccounts),
		"skipped":     skipped,
		"ok":          true,
	}, nil
}

func buildCLIProxyRecord(entry map[string]any, aid string) map[string]any {
	if entry == nil {
		return nil
	}
	access := firstNonEmpty(stringField(entry, "key"), stringField(entry, "access_token"), stringField(entry, "token"))
	if access == "" {
		return nil
	}
	claims := accounts.DecodeJWTClaims(access)
	email := firstNonEmpty(stringField(entry, "email"), stringField(claims, "email"))
	sub := firstNonEmpty(stringField(entry, "user_id"), stringField(entry, "principal_id"), stringField(entry, "sub"), stringField(claims, "principal_id"), stringField(claims, "sub"))
	expiredISO := ""
	if exp := accounts.ParseExpiresAt(entry["expires_at"], access); exp != nil {
		expiredISO = time.Unix(int64(*exp), 0).UTC().Format(time.RFC3339)
	}
	headers, _ := entry["cliproxyapi_headers"].(map[string]any)
	if headers == nil {
		headers = map[string]any{
			"X-XAI-Token-Auth":         "xai-grok-cli",
			"x-grok-client-version":    "0.2.93",
			"x-grok-client-identifier": "grok-shell",
		}
	}
	baseURL := firstNonEmpty(stringField(entry, "cliproxyapi_base_url"), "https://cli-chat-proxy.grok.com/v1")
	rec := map[string]any{
		"type":          firstNonEmpty(stringField(entry, "cliproxyapi_type"), "xai"),
		"auth_kind":     firstNonEmpty(stringField(entry, "cliproxyapi_auth_kind"), "oauth"),
		"email":         email,
		"sub":           sub,
		"access_token":  access,
		"refresh_token": stringField(entry, "refresh_token"),
		"id_token":      stringField(entry, "id_token"),
		"token_type":    "Bearer",
		"expired":       expiredISO,
		"last_refresh":  time.Now().UTC().Format(time.RFC3339),
		"base_url":      baseURL,
		"disabled":      false,
		"headers":       headers,
	}
	if aid != "" {
		rec["local_account_id"] = aid
	}
	if sub != "" {
		rec["account_id"] = sub
	}
	return rec
}

// TestCLIProxy does a lightweight management auth-files list.
func TestCLIProxy(ctx context.Context, cfg map[string]any) map[string]any {
	base := strings.TrimRight(stringField(cfg, "base_url"), "/")
	key := stringField(cfg, "management_key")
	if base == "" || key == "" {
		return map[string]any{"ok": false, "error": "base_url and management_key required"}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/v0/management/auth-files", nil)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("X-Management-Key", key)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return map[string]any{"ok": false, "status_code": resp.StatusCode, "error": string(body)}
	}
	return map[string]any{"ok": true, "status_code": resp.StatusCode, "body_preview": string(body[:min(200, len(body))])}
}

// PushCLIProxy uploads selected/all accounts as auth files.
func PushCLIProxy(ctx context.Context, store Store, ids []string, concurrency int) (map[string]any, error) {
	raw := map[string]any{}
	if v, err := store.GetSetting(ctx, "cliproxyapi_config"); err == nil {
		if m, ok := v.(map[string]any); ok {
			raw = m
		}
	}
	base := strings.TrimRight(stringField(raw, "base_url"), "/")
	key := stringField(raw, "management_key")
	if base == "" {
		return map[string]any{"ok": false, "error": "CLIProxyAPI base_url missing"}, nil
	}
	if key == "" {
		return map[string]any{"ok": false, "error": "CLIProxyAPI management_key missing (ensure secret is stored)"}, nil
	}
	bundle, err := ExportCLIProxyBundle(ctx, store, ids)
	if err != nil {
		return nil, err
	}
	list, _ := bundle["accounts"].([]map[string]any)
	// type assert from []any if needed
	if list == nil {
		if arr, ok := bundle["accounts"].([]any); ok {
			for _, item := range arr {
				if m, ok := item.(map[string]any); ok {
					list = append(list, m)
				}
			}
		}
	}
	success, failed := 0, 0
	errors := []map[string]any{}
	client := &http.Client{Timeout: 45 * time.Second}
	for _, rec := range list {
		name := firstNonEmpty(stringField(rec, "email"), stringField(rec, "sub"), "account")
		name = sanitizeName(name) + ".json"
		payload, _ := json.Marshal(rec)
		url := base + "/v0/management/auth-files?name=" + name
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(payload))
		if err != nil {
			failed++
			continue
		}
		req.Header.Set("Authorization", "Bearer "+key)
		req.Header.Set("X-Management-Key", key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			failed++
			errors = append(errors, map[string]any{"name": name, "error": err.Error()})
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
		resp.Body.Close()
		if resp.StatusCode >= 400 {
			failed++
			errors = append(errors, map[string]any{"name": name, "status": resp.StatusCode, "error": string(body)})
			continue
		}
		success++
	}
	return map[string]any{
		"ok":      failed == 0,
		"success": success,
		"failed":  failed,
		"total":   len(list),
		"errors":  errors,
		"message": fmt.Sprintf("CLIProxyAPI push: success=%d failed=%d", success, failed),
	}, nil
}

func sanitizeName(s string) string {
	s = strings.TrimSpace(s)
	s = strings.ReplaceAll(s, "@", "_at_")
	out := strings.Builder{}
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' || r == '.' {
			out.WriteRune(r)
		} else {
			out.WriteByte('_')
		}
	}
	if out.Len() == 0 {
		return "account"
	}
	return out.String()
}

func stringField(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

func sub2Config(ctx context.Context, store Store) map[string]any {
	if store == nil {
		return map[string]any{}
	}
	raw, err := store.GetSetting(ctx, "sub2api_config")
	if err != nil {
		return map[string]any{}
	}
	m, _ := raw.(map[string]any)
	if m == nil {
		return map[string]any{}
	}
	return m
}

func TestSub2API(ctx context.Context, cfg map[string]any) map[string]any {
	base := strings.TrimRight(stringField(cfg, "base_url"), "/")
	email := stringField(cfg, "email")
	password := stringField(cfg, "password")
	if base == "" || email == "" || password == "" {
		return map[string]any{"ok": false, "error": "base_url / email / password required"}
	}
	token, err := sub2Login(ctx, base, email, password)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error()}
	}
	// list groups smoke test
	groups, err := sub2ListGroups(ctx, base, token)
	if err != nil {
		return map[string]any{"ok": false, "error": err.Error(), "token_ok": true}
	}
	return map[string]any{"ok": true, "message": "login ok", "groups": len(groups), "token_ok": true}
}

// PushSub2API pushes selected/all local accounts into sub2api via OAuth create API.
func PushSub2API(ctx context.Context, store Store, ids []string, groupID *int, concurrency int) (map[string]any, error) {
	cfg := sub2Config(ctx, store)
	base := strings.TrimRight(stringField(cfg, "base_url"), "/")
	if base == "" {
		return map[string]any{"ok": false, "error": "请先在设置页填写 sub2api URL 与登录信息", "success": 0, "failed": 0, "total": 0}, nil
	}
	email := stringField(cfg, "email")
	password := stringField(cfg, "password")
	if email == "" || password == "" {
		return map[string]any{"ok": false, "error": "sub2api 登录邮箱/密码未配置", "success": 0, "failed": 0, "total": 0}, nil
	}
	token, err := sub2Login(ctx, base, email, password)
	if err != nil {
		return map[string]any{"ok": false, "error": "sub2api login failed: " + err.Error(), "success": 0, "failed": 0, "total": 0}, nil
	}
	gid := 0
	if groupID != nil && *groupID > 0 {
		gid = *groupID
	} else {
		gid = intField(cfg, "group_id", 0)
	}
	if gid <= 0 {
		// try resolve by name or first group
		groups, gerr := sub2ListGroups(ctx, base, token)
		if gerr != nil {
			return map[string]any{"ok": false, "error": "group_id missing and list groups failed: " + gerr.Error(), "success": 0, "failed": 0, "total": 0}, nil
		}
		wantName := stringField(cfg, "group_name")
		for _, g := range groups {
			if wantName != "" && strings.EqualFold(stringField(g, "name"), wantName) {
				gid = intField(g, "id", 0)
				break
			}
		}
		if gid <= 0 && len(groups) > 0 {
			gid = intField(groups[0], "id", 0)
		}
	}
	if gid <= 0 {
		return map[string]any{"ok": false, "error": "sub2api group_id 未配置，请先选择/创建分组", "success": 0, "failed": 0, "total": 0}, nil
	}

	authMap, err := store.ExportAuthMap(ctx, ids, true)
	if err != nil {
		return nil, err
	}
	auth, _ := authMap["auth"].(map[string]any)
	notesPrefix := firstNonEmpty(stringField(cfg, "notes_prefix"), "grokcli-2api")
	accConc := intField(cfg, "account_concurrency", 3)
	accPrio := intField(cfg, "account_priority", 50)
	accRate := floatField(cfg, "account_rate_multiplier", 1)
	if concurrency <= 0 {
		concurrency = intField(cfg, "concurrency", 4)
	}
	if concurrency < 1 {
		concurrency = 1
	}
	if concurrency > 8 {
		concurrency = 8
	}

	type item struct {
		id    string
		entry map[string]any
	}
	list := make([]item, 0, len(auth))
	for aid, raw := range auth {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		list = append(list, item{id: aid, entry: entry})
	}

	type res struct {
		row map[string]any
	}
	ch := make(chan res, len(list))
	sem := make(chan struct{}, concurrency)
	var wg sync.WaitGroup
	client := &http.Client{Timeout: 60 * time.Second}
	for _, it := range list {
		wg.Add(1)
		go func(it item) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			row := pushOneSub2(ctx, client, base, token, gid, notesPrefix, accConc, accPrio, accRate, it.id, it.entry)
			ch <- res{row: row}
		}(it)
	}
	wg.Wait()
	close(ch)
	results := []map[string]any{}
	success, failed := 0, 0
	for r := range ch {
		results = append(results, r.row)
		if r.row["ok"] == true {
			success++
		} else {
			failed++
		}
	}
	return map[string]any{
		"ok":       failed == 0,
		"success":  success,
		"failed":   failed,
		"total":    len(list),
		"results":  results,
		"group_id": gid,
		"message":  fmt.Sprintf("sub2api 导入完成：成功 %d / 失败 %d / 共 %d", success, failed, len(list)),
	}, nil
}

func pushOneSub2(ctx context.Context, client *http.Client, base, token string, gid int, notesPrefix string, accConc, accPrio int, accRate float64, aid string, entry map[string]any) map[string]any {
	email := stringField(entry, "email")
	access := firstNonEmpty(stringField(entry, "key"), stringField(entry, "access_token"), stringField(entry, "token"))
	refresh := stringField(entry, "refresh_token")
	out := map[string]any{"account_id": aid, "email": email, "ok": false, "method": "oauth_token"}
	if access == "" {
		// try SSO path
		sso := accounts.GetSSOValue(entry)
		if sso == "" {
			out["error"] = "missing access_token and sso"
			out["method"] = "none"
			return out
		}
		// SSO → OAuth
		creds, err := sub2SSOToOAuth(ctx, client, base, token, []string{sso})
		if err != nil || len(creds) == 0 {
			out["error"] = "sso-to-oauth failed"
			if err != nil {
				out["error"] = "sso-to-oauth failed: " + err.Error()
			}
			out["method"] = "sso"
			return out
		}
		c0 := creds[0]
		access = firstNonEmpty(stringField(c0, "access_token"), stringField(c0, "AccessToken"))
		refresh = firstNonEmpty(stringField(c0, "refresh_token"), stringField(c0, "RefreshToken"), refresh)
		if email == "" {
			email = firstNonEmpty(stringField(c0, "email"), stringField(c0, "Email"))
		}
		out["method"] = "sso"
		if access == "" {
			out["error"] = "sso-to-oauth produced no access_token"
			return out
		}
	}
	name := firstNonEmpty(email, aid)
	credentials := map[string]any{"access_token": access, "email": email}
	if refresh != "" {
		credentials["refresh_token"] = refresh
	}
	if exp := accounts.ParseExpiresAt(entry["expires_at"], access); exp != nil {
		credentials["expires_at"] = time.Unix(int64(*exp), 0).UTC().Format(time.RFC3339)
	}
	body := map[string]any{
		"name":            trimLen(name, 200),
		"platform":        "grok",
		"type":            "oauth",
		"credentials":     credentials,
		"extra":           map[string]any{},
		"proxy_id":        nil,
		"group_ids":       []int{gid},
		"concurrency":     accConc,
		"priority":        accPrio,
		"rate_multiplier": accRate,
		"notes":           notesPrefix + ":" + aid,
	}
	created, err := sub2CreateAccount(ctx, client, base, token, body)
	if err != nil {
		out["error"] = err.Error()
		return out
	}
	out["ok"] = true
	out["remote"] = map[string]any{"id": created["id"], "name": created["name"]}
	out["group_id"] = gid
	return out
}

func sub2Login(ctx context.Context, base, email, password string) (string, error) {
	payload, _ := json.Marshal(map[string]any{"email": email, "password": password})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grokcli-2api-sub2api-push/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return "", fmt.Errorf("login status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed any
	_ = json.Unmarshal(raw, &parsed)
	token := digString(parsed, "token", "access_token", "data.token", "data.access_token")
	if token == "" {
		return "", fmt.Errorf("login ok but token missing: %s", string(raw[:min(200, len(raw))]))
	}
	return token, nil
}

func sub2ListGroups(ctx context.Context, base, token string) ([]map[string]any, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+"/api/v1/admin/groups", nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("User-Agent", "grokcli-2api-sub2api-push/1.0")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("groups status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed any
	_ = json.Unmarshal(raw, &parsed)
	arr := digArray(parsed, "data", "data.items", "items", "groups")
	out := []map[string]any{}
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			out = append(out, m)
		}
	}
	// maybe data itself is array
	if len(out) == 0 {
		if m, ok := parsed.(map[string]any); ok {
			if data, ok := m["data"].([]any); ok {
				for _, item := range data {
					if mm, ok := item.(map[string]any); ok {
						out = append(out, mm)
					}
				}
			}
		} else if data, ok := parsed.([]any); ok {
			for _, item := range data {
				if mm, ok := item.(map[string]any); ok {
					out = append(out, mm)
				}
			}
		}
	}
	return out, nil
}

func sub2CreateAccount(ctx context.Context, client *http.Client, base, token string, body map[string]any) (map[string]any, error) {
	payload, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/admin/accounts", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grokcli-2api-sub2api-push/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("create status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed any
	_ = json.Unmarshal(raw, &parsed)
	if m := digMap(parsed, "data"); m != nil {
		return m, nil
	}
	if m, ok := parsed.(map[string]any); ok {
		return m, nil
	}
	return map[string]any{"raw": string(raw)}, nil
}

func sub2SSOToOAuth(ctx context.Context, client *http.Client, base, token string, ssoList []string) ([]map[string]any, error) {
	payload, _ := json.Marshal(map[string]any{"sso_tokens": ssoList, "proxy_id": nil})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+"/api/v1/admin/grok/sso-to-oauth", bytes.NewReader(payload))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "grokcli-2api-sub2api-push/1.0")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode >= 400 {
		return nil, fmt.Errorf("sso-to-oauth status %d: %s", resp.StatusCode, string(raw))
	}
	var parsed any
	_ = json.Unmarshal(raw, &parsed)
	arr := digArray(parsed, "data.results", "results", "data")
	out := []map[string]any{}
	for _, item := range arr {
		if m, ok := item.(map[string]any); ok {
			// flatten nested credentials
			if c, ok := m["credentials"].(map[string]any); ok {
				for k, v := range c {
					if _, exists := m[k]; !exists {
						m[k] = v
					}
				}
			}
			out = append(out, m)
		}
	}
	return out, nil
}

func intField(m map[string]any, key string, fallback int) int {
	if m == nil {
		return fallback
	}
	switch v := m[key].(type) {
	case float64:
		return int(v)
	case float32:
		return int(v)
	case int:
		return v
	case int64:
		return int(v)
	case json.Number:
		i, err := v.Int64()
		if err == nil {
			return int(i)
		}
	case string:
		var n int
		fmt.Sscanf(strings.TrimSpace(v), "%d", &n)
		if n != 0 {
			return n
		}
	}
	return fallback
}

func floatField(m map[string]any, key string, fallback float64) float64 {
	if m == nil {
		return fallback
	}
	switch v := m[key].(type) {
	case float64:
		return v
	case float32:
		return float64(v)
	case int:
		return float64(v)
	case int64:
		return float64(v)
	case json.Number:
		f, err := v.Float64()
		if err == nil {
			return f
		}
	}
	return fallback
}

func trimLen(s string, n int) string {
	s = strings.TrimSpace(s)
	if len(s) > n {
		return s[:n]
	}
	return s
}

func digString(v any, paths ...string) string {
	for _, p := range paths {
		cur := v
		ok := true
		for _, part := range strings.Split(p, ".") {
			m, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur = m[part]
		}
		if !ok || cur == nil {
			continue
		}
		if s, ok := cur.(string); ok && strings.TrimSpace(s) != "" {
			return strings.TrimSpace(s)
		}
	}
	return ""
}

func digMap(v any, path string) map[string]any {
	cur := v
	for _, part := range strings.Split(path, ".") {
		m, ok := cur.(map[string]any)
		if !ok {
			return nil
		}
		cur = m[part]
	}
	if m, ok := cur.(map[string]any); ok {
		return m
	}
	return nil
}

func digArray(v any, paths ...string) []any {
	for _, p := range paths {
		cur := v
		ok := true
		for _, part := range strings.Split(p, ".") {
			m, isMap := cur.(map[string]any)
			if !isMap {
				ok = false
				break
			}
			cur = m[part]
		}
		if !ok {
			continue
		}
		if arr, ok := cur.([]any); ok {
			return arr
		}
	}
	return nil
}
