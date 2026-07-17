package postgres

import (
	"strings"
	"testing"
	"time"
)

func TestDerivePoolStatus(t *testing.T) {
	cases := []struct {
		name string
		in   map[string]any
		want string
	}{
		{"quota", map[string]any{"disabled_for_quota": true, "enabled": true}, "quota_disabled"},
		{"disabled", map[string]any{"enabled": false}, "disabled"},
		{"cooldown", map[string]any{"enabled": true, "in_cooldown": true}, "cooldown"},
		{"model_blocked", map[string]any{"enabled": true, "blocked_model_ids": []string{"grok-4.5"}}, "model_blocked"},
		{"expired", map[string]any{"enabled": true, "expired": true}, "expired"},
		{"normal", map[string]any{"enabled": true}, "normal"},
		{"raw_model_blocked", map[string]any{"enabled": true, "pool_status": "model_blocked", "blocked_model_ids": []string{}}, "model_blocked"},
	}
	for _, tc := range cases {
		got := derivePoolStatus(tc.in)
		if got != tc.want {
			t.Fatalf("%s: got %q want %q", tc.name, got, tc.want)
		}
	}
}

func TestActiveBlockedModelsExpires(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	blocked := map[string]any{
		"alive": map[string]any{"until": float64(1_700_000_200)},
		"dead":  map[string]any{"until": float64(1_700_000_000)},
		"perm":  map[string]any{"reason": "nope"},
	}
	out := activeBlockedModels(blocked, now)
	if _, ok := out["dead"]; ok {
		t.Fatalf("expired block should drop: %#v", out)
	}
	if _, ok := out["alive"]; !ok {
		t.Fatalf("active block missing: %#v", out)
	}
	if _, ok := out["perm"]; !ok {
		t.Fatalf("permanent block missing: %#v", out)
	}
}

func TestBuildAccountListWhereStatus(t *testing.T) {
	where, args := buildAccountListWhere("", "cooldown", nil)
	if where == "" || !containsFold(where, "cooldown") {
		t.Fatalf("cooldown where=%q args=%v", where, args)
	}
	where, args = buildAccountListWhere("foo", "disabled", nil)
	if len(args) != 3 {
		t.Fatalf("query args=%v", args)
	}
	if !containsFold(where, "enabled") {
		t.Fatalf("disabled where=%q", where)
	}
	trueVal := true
	where, args = buildAccountListWhere("", "live", &trueVal)
	if where == "" || !containsFold(where, "sso") {
		t.Fatalf("live+sso where=%q", where)
	}
	where, _ = buildAccountListWhere("", "model_blocked", nil)
	if !containsFold(where, "blocked_models") {
		t.Fatalf("model_blocked where=%q", where)
	}
}

func containsFold(s, sub string) bool {
	return strings.Contains(strings.ToLower(s), strings.ToLower(sub))
}

func TestActiveBlockedModelsObjectForm(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	blocked := map[string]any{
		"future": map[string]any{"until": float64(1_700_000_200), "reason": "temp", "source": "temp_usage"},
		"past":   map[string]any{"until": float64(1_700_000_000), "reason": "temp", "source": "temp_usage"},
		"perm":   map[string]any{"blocked": true, "reason": "hard"},
		"bare":   float64(1_700_000_200),
		"oldnum": float64(1_700_000_000),
	}
	out := activeBlockedModels(blocked, now)
	if _, ok := out["past"]; ok {
		t.Fatalf("expired object should drop: %#v", out)
	}
	if _, ok := out["oldnum"]; ok {
		t.Fatalf("expired bare until should drop: %#v", out)
	}
	for _, k := range []string{"future", "perm", "bare"} {
		if _, ok := out[k]; !ok {
			t.Fatalf("missing active block %s: %#v", k, out)
		}
	}
}
