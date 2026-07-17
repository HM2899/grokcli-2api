package pool

import (
	"errors"
	"testing"
	"time"
)

func TestCandidateEligibility(t *testing.T) {
	now := time.Unix(1000, 0)
	future := now.Add(time.Minute)
	past := now.Add(-time.Minute)
	cases := []struct {
		name string
		c    Candidate
		want bool
	}{
		{"ok", Candidate{ID: "a", Token: "tok", Enabled: true}, true},
		{"missing token", Candidate{ID: "a", Enabled: true}, false},
		{"disabled", Candidate{ID: "a", Token: "tok", Enabled: false}, false},
		{"quota", Candidate{ID: "a", Token: "tok", Enabled: true, DisabledForQuota: true}, false},
		{"expired", Candidate{ID: "a", Token: "tok", Enabled: true, ExpiresAt: &past}, false},
		{"cooldown", Candidate{ID: "a", Token: "tok", Enabled: true, CooldownUntil: &future}, false},
		{"model blocked", Candidate{ID: "a", Token: "tok", Enabled: true, BlockedModels: map[string]any{"grok-4.5": true}}, false},
	}
	for _, tc := range cases {
		if got := tc.c.Eligible("grok-4.5", now); got != tc.want {
			t.Fatalf("%s eligible=%v want %v", tc.name, got, tc.want)
		}
	}
}

func TestChainSortsAndLimits(t *testing.T) {
	now := time.Unix(1000, 0)
	candidates := []Candidate{
		{ID: "busy", Token: "t", Enabled: true, RequestCount: 10, Weight: 1},
		{ID: "idle", Token: "t", Enabled: true, RequestCount: 1, Weight: 1},
		{ID: "heavy", Token: "t", Enabled: true, RequestCount: 5, Weight: 3},
	}
	least := Chain(candidates, "grok", "least_used", now, 2)
	if len(least) != 2 || least[0].ID != "idle" || least[1].ID != "heavy" {
		t.Fatalf("unexpected least_used chain %#v", least)
	}
	rr := Chain(candidates, "grok", "round_robin", now, 1)
	if len(rr) != 1 || rr[0].ID != "heavy" {
		t.Fatalf("unexpected weighted chain %#v", rr)
	}
}

func TestPickNoEligible(t *testing.T) {
	_, err := Pick([]Candidate{{ID: "disabled", Token: "t", Enabled: false}}, "grok", "round_robin", time.Now())
	if !errors.Is(err, ErrNoEligibleAccounts) {
		t.Fatalf("expected ErrNoEligibleAccounts, got %v", err)
	}
}

func TestModelBlockedObjectUntil(t *testing.T) {
	now := time.Unix(1_700_000_100, 0)
	blocked := map[string]any{
		"grok-4.5": map[string]any{"until": float64(1_700_000_200), "source": "temp_usage"},
	}
	if !modelBlocked(blocked, "grok-4.5", now) {
		t.Fatal("future until should block")
	}
	blocked["grok-4.5"] = map[string]any{"until": float64(1_700_000_000)}
	if modelBlocked(blocked, "grok-4.5", now) {
		t.Fatal("past until should not block")
	}
	// other model not blocked
	if modelBlocked(blocked, "grok-3", now) {
		t.Fatal("unrelated model should not block")
	}
}
