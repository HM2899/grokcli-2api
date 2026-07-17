package redis

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

func testRedisClient(t *testing.T) *Client {
	t.Helper()
	url := os.Getenv("GROK2API_TEST_REDIS_URL")
	if url == "" {
		t.Skip("GROK2API_TEST_REDIS_URL is not set")
	}
	client := New(url, fmt.Sprintf("g2a-it-%d", time.Now().UnixNano()))
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		t.Fatalf("ping redis: %v", err)
	}
	return client
}

func TestRedisPoolPrimitivesIntegration(t *testing.T) {
	client := testRedisClient(t)
	ctx := context.Background()
	accountID := "acc-1"
	t.Cleanup(func() {
		_, _ = client.command(context.Background(), "DEL", client.key("inflight", accountID), client.key("soft_used", accountID), client.key("cooldown", accountID), client.key("rr", "index"))
	})

	n, err := client.RRNext(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("first rr index = %d", n)
	}
	n, err = client.MarkInflight(ctx, accountID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("first inflight = %d", n)
	}
	n, err = client.MarkInflight(ctx, accountID, 30)
	if err != nil {
		t.Fatal(err)
	}
	if n != 2 {
		t.Fatalf("second inflight = %d", n)
	}
	got, err := client.GetInflight(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 2 {
		t.Fatalf("get inflight = %d", got)
	}
	if err := client.ReleaseInflight(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	got, err = client.GetInflight(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 1 {
		t.Fatalf("after release inflight = %d", got)
	}
	if err := client.ReleaseInflight(ctx, accountID); err != nil {
		t.Fatal(err)
	}
	got, err = client.GetInflight(ctx, accountID)
	if err != nil {
		t.Fatal(err)
	}
	if got != 0 {
		t.Fatalf("after final release inflight = %d", got)
	}

	stamp, err := client.MarkSoftUsed(ctx, accountID, 30, time.Unix(123, 456000000))
	if err != nil {
		t.Fatal(err)
	}
	if stamp <= 123 {
		t.Fatalf("soft stamp = %f", stamp)
	}
	raw, err := client.command(ctx, "GET", client.key("soft_used", accountID))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(raw, "123.") {
		t.Fatalf("soft_used raw = %q", raw)
	}

	until := time.Now().Add(time.Minute)
	if err := client.MirrorCooldown(ctx, accountID, until); err != nil {
		t.Fatal(err)
	}
	raw, err = client.command(ctx, "GET", client.key("cooldown", accountID))
	if err != nil {
		t.Fatal(err)
	}
	if raw == "" {
		t.Fatal("cooldown mirror missing")
	}
	if err := client.MirrorCooldown(ctx, accountID, time.Time{}); err != nil {
		t.Fatal(err)
	}
	raw, err = client.command(ctx, "GET", client.key("cooldown", accountID))
	if err != nil {
		t.Fatal(err)
	}
	if raw != "" {
		t.Fatalf("cooldown mirror after clear = %q", raw)
	}
}

func TestRedisAffinityIntegration(t *testing.T) {
	client := testRedisClient(t)
	ctx := context.Background()
	fingerprint := "fp:test"
	t.Cleanup(func() { _ = client.ClearAffinity(context.Background(), fingerprint) })

	if err := client.BindAffinity(ctx, fingerprint, "acc-a", time.Minute, "session-a", "pck-a"); err != nil {
		t.Fatal(err)
	}
	entry, err := client.GetAffinity(ctx, fingerprint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if entry == nil || entry.AccountID != "acc-a" || entry.SessionFP != "session-a" || entry.PromptCacheKey != "pck-a" {
		t.Fatalf("unexpected affinity entry %#v", entry)
	}
	if entry.Hits < 2 {
		t.Fatalf("hits = %d", entry.Hits)
	}
	if err := client.ClearAffinity(ctx, fingerprint); err != nil {
		t.Fatal(err)
	}
	entry, err = client.GetAffinity(ctx, fingerprint, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if entry != nil {
		t.Fatalf("entry after clear = %#v", entry)
	}
}
