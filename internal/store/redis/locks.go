package redis

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

// WorkerID matches Python redis_client.worker_id closely enough for leadership.
func WorkerID() string {
	host, err := os.Hostname()
	if err != nil || strings.TrimSpace(host) == "" {
		host = "host"
	}
	return fmt.Sprintf("%d@%s", os.Getpid(), host)
}

func (c *Client) LeaderLockKey() string {
	return c.key("lock", "maintainer_leader")
}

func (c *Client) MaintenanceLockKey() string {
	return c.key("lock", "maintenance")
}

// TryAcquireLock SET NX EX.
func (c *Client) TryAcquireLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	if !c.Enabled() {
		return false, nil
	}
	sec := int(ttl.Seconds())
	if sec < 1 {
		sec = 1
	}
	return c.SetNXEX(ctx, key, token, sec)
}

func (c *Client) RenewLock(ctx context.Context, key, token string, ttl time.Duration) (bool, error) {
	if !c.Enabled() {
		return false, nil
	}
	sec := int(ttl.Seconds())
	if sec < 1 {
		sec = 1
	}
	return c.RenewIfOwner(ctx, key, token, sec)
}

func (c *Client) ReleaseLock(ctx context.Context, key, token string) (bool, error) {
	if !c.Enabled() {
		return false, nil
	}
	return c.CompareAndDelete(ctx, key, token)
}

// AcquireMaintenanceLock acquires g2a:lock:maintenance with renew loop.
// Returns release function and whether acquired.
func (c *Client) AcquireMaintenanceLock(ctx context.Context, owner string, timeout time.Duration, blocking bool) (acquired bool, release func(), err error) {
	release = func() {}
	if !c.Enabled() {
		return false, release, nil
	}
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	token := fmt.Sprintf("%s|%s|%d", strings.TrimSpace(owner), WorkerID(), time.Now().Unix())
	lockKey := c.MaintenanceLockKey()
	deadline := time.Now()
	if blocking {
		deadline = time.Now().Add(timeout)
	}
	for {
		ok, aerr := c.TryAcquireLock(ctx, lockKey, token, timeout)
		if aerr != nil {
			return false, release, aerr
		}
		if ok {
			acquired = true
			break
		}
		if !blocking || time.Now().After(deadline) {
			break
		}
		select {
		case <-ctx.Done():
			return false, release, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
	if !acquired {
		return false, release, nil
	}
	stop := make(chan struct{})
	go func() {
		ticker := time.NewTicker(timeout / 3)
		if timeout/3 < time.Second {
			ticker.Reset(time.Second)
		}
		defer ticker.Stop()
		for {
			select {
			case <-stop:
				return
			case <-ticker.C:
				_, _ = c.RenewLock(context.Background(), lockKey, token, timeout)
			}
		}
	}()
	release = func() {
		close(stop)
		_, _ = c.ReleaseLock(context.Background(), lockKey, token)
	}
	return true, release, nil
}

func (c *Client) MaintenanceLockStatus(ctx context.Context) map[string]any {
	if !c.Enabled() {
		return map[string]any{"backend": "none"}
	}
	cur, err := c.Get(ctx, c.MaintenanceLockKey())
	if err != nil || strings.TrimSpace(cur) == "" {
		return map[string]any{"backend": "redis", "busy": false, "holder": nil, "token": nil}
	}
	holder := cur
	if i := strings.Index(cur, "|"); i >= 0 {
		holder = cur[:i]
	}
	return map[string]any{"backend": "redis", "busy": true, "holder": holder, "token": cur}
}
