package accounts

import (
	"encoding/base64"
	"encoding/json"
	"testing"
)

func TestCollectNormalizedEntriesJWTString(t *testing.T) {
	payload := base64.RawURLEncoding.EncodeToString([]byte(`{"sub":"user-1","email":"a@x.ai","exp":4102444800}`))
	token := "aaa." + payload + ".sig"
	result := CollectNormalizedEntries(token)
	if !result.OK || len(result.Normalized) != 1 {
		t.Fatalf("result=%#v", result)
	}
	for id, ent := range result.Normalized {
		if id != "https://auth.x.ai::user-1" {
			t.Fatalf("id=%s", id)
		}
		if ent["email"] != "a@x.ai" || ent["key"] != token {
			t.Fatalf("entry=%#v", ent)
		}
	}
}

func TestCollectNormalizedEntriesAuthMap(t *testing.T) {
	raw := map[string]any{
		"https://auth.x.ai::u1": map[string]any{
			"key":   "tok-1",
			"email": "u1@x.ai",
		},
	}
	result := CollectNormalizedEntries(raw)
	if !result.OK || len(result.Normalized) != 1 {
		t.Fatalf("%#v", result)
	}
}

func TestCollectNormalizedEntriesExportWrapper(t *testing.T) {
	raw := map[string]any{
		"auth": map[string]any{
			"https://auth.x.ai::u2": map[string]any{"access_token": "tok-2", "email": "u2@x.ai"},
		},
		"count": 1,
	}
	b, _ := json.Marshal(raw)
	result := CollectNormalizedEntries(string(b))
	if !result.OK || len(result.Normalized) != 1 {
		t.Fatalf("%#v", result)
	}
}

func TestMergeDurableAccountFields(t *testing.T) {
	entry := map[string]any{"key": "new"}
	old := map[string]any{"sso": "cookie-1", "password": "p"}
	MergeDurableAccountFields(entry, old)
	if entry["sso"] != "cookie-1" || entry["password"] != "p" {
		t.Fatalf("%#v", entry)
	}
}
