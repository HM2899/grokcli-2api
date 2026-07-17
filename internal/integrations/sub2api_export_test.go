package integrations

import (
	"context"
	"testing"
)

type memStore struct {
	auth map[string]any
	cfg  map[string]any
}

func (m *memStore) PublicSettings(ctx context.Context) (map[string]any, error) {
	return map[string]any{"sub2api_config": redactIntegrationConfig("sub2api_config", m.cfg)}, nil
}
func (m *memStore) SetSetting(ctx context.Context, key string, value any) error { return nil }
func (m *memStore) GetSetting(ctx context.Context, key string) (any, error) {
	if key == "sub2api_config" {
		return m.cfg, nil
	}
	return nil, nil
}
func (m *memStore) ExportAuthMap(ctx context.Context, accountIDs []string, includeSecrets bool) (map[string]any, error) {
	return map[string]any{"ok": true, "auth": m.auth, "count": len(m.auth)}, nil
}

func TestExportSub2APIFormatDataPayload(t *testing.T) {
	st := &memStore{
		cfg: map[string]any{"notes_prefix": "g2a", "account_concurrency": 3},
		auth: map[string]any{
			"acc1": map[string]any{
				"email":         "a@x.com",
				"access_token":  "tok",
				"refresh_token": "rt",
				"expires_at":    float64(1700000000),
				"sso":           "cookie",
			},
			"acc2": map[string]any{"email": "b@x.com"}, // skip no token
		},
	}
	out, err := ExportSub2APIFormat(context.Background(), st, nil)
	if err != nil {
		t.Fatal(err)
	}
	if out["type"] != "sub2api-data" {
		t.Fatalf("type=%v", out["type"])
	}
	if _, ok := out["proxies"].([]any); !ok {
		// may be typed empty slice
		if out["proxies"] == nil {
			t.Fatal("proxies missing")
		}
	}
	accs, _ := out["accounts"].([]map[string]any)
	if accs == nil {
		if arr, ok := out["accounts"].([]any); ok {
			for _, a := range arr {
				if m, ok := a.(map[string]any); ok {
					accs = append(accs, m)
				}
			}
		}
	}
	if len(accs) != 1 {
		t.Fatalf("accounts=%d out=%#v", len(accs), out["accounts"])
	}
	creds, _ := accs[0]["credentials"].(map[string]any)
	if creds["access_token"] != "tok" {
		t.Fatalf("creds=%#v", creds)
	}
}

func TestPublicConfigRedactsPassword(t *testing.T) {
	st := &memStore{cfg: map[string]any{"base_url": "http://x", "password": "secret", "email": "e"}}
	out := PublicConfig(context.Background(), st, "sub2api_config")
	if out["password"] != nil && out["password"] != "" {
		t.Fatalf("password leaked: %#v", out)
	}
	if out["has_password"] != true {
		t.Fatalf("has_password missing: %#v", out)
	}
}
