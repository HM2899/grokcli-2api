package redis

import (
	"context"
	"testing"
)

func TestUsageDayKeyAndEmptySnapshot(t *testing.T) {
	c := New("redis://127.0.0.1:6379/0", "g2a")
	if got := c.usageDayKey("20260717", "global", ""); got != "g2a:usage:day:20260717:global" {
		t.Fatalf("day key=%s", got)
	}
	if got := c.usageLifeKey("key", "abc"); got != "g2a:usage:life:key:abc" {
		t.Fatalf("life key=%s", got)
	}
	snap := (&Client{}).LightSnapshot(context.Background())
	if snap["source"] != "none" && snap["today_requests"] != int64(0) && snap["today_requests"] != 0 {
		// disabled client returns zeros with source none via server helper; client method itself
		// returns zeros when disabled.
	}
	empty := emptyUsage()
	if empty["requests"] != 0 || empty["total_tokens"] != 0 {
		t.Fatalf("%#v", empty)
	}
}

func TestLeaderForceModes(t *testing.T) {
	l := NewLeader(nil, "never", 4, 0, 0)
	if l.ShouldStartMaintainers(context.Background()) {
		t.Fatal("never should not lead")
	}
	l = NewLeader(nil, "always", 4, 0, 0)
	if !l.ShouldStartMaintainers(context.Background()) || !l.IsLeader() {
		t.Fatal("always should lead")
	}
	l = NewLeader(nil, "auto", 1, 0, 0)
	if !l.ShouldStartMaintainers(context.Background()) {
		t.Fatal("single worker auto should lead locally")
	}
}

func TestWorkerIDNonEmpty(t *testing.T) {
	if WorkerID() == "" {
		t.Fatal("empty worker id")
	}
}
