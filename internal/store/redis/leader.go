package redis

import (
	"context"
	"strings"
	"sync"
	"time"
)

// Leader elects a single maintainer owner using g2a:lock:maintainer_leader.
// Compatible with Python grok2api.store.leader key/token shape.
type Leader struct {
	Client   *Client
	Mode     string // auto|always|never (and aliases)
	Workers  int
	TTL      time.Duration
	Renew    time.Duration
	WorkerID string
	OnGain   func()
	OnLost   func()

	mu       sync.Mutex
	isLeader bool
	leaderID string
	started  bool
	stop     chan struct{}
}

func NewLeader(client *Client, mode string, workers int, ttl, renew time.Duration) *Leader {
	if workers <= 0 {
		workers = 1
	}
	if ttl <= 0 {
		ttl = 30 * time.Second
	}
	if renew <= 0 {
		renew = 10 * time.Second
	}
	return &Leader{
		Client:   client,
		Mode:     strings.ToLower(strings.TrimSpace(mode)),
		Workers:  workers,
		TTL:      ttl,
		Renew:    renew,
		WorkerID: WorkerID(),
		stop:     make(chan struct{}),
	}
}

func (l *Leader) forceMode() *bool {
	switch l.Mode {
	case "1", "true", "yes", "on", "always":
		v := true
		return &v
	case "0", "false", "no", "off", "never":
		v := false
		return &v
	default:
		return nil
	}
}

func (l *Leader) IsLeader() bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.isLeader
}

func (l *Leader) Status(ctx context.Context) map[string]any {
	l.mu.Lock()
	isLead := l.isLeader
	lid := l.leaderID
	l.mu.Unlock()
	if l.Client != nil && l.Client.Enabled() {
		if remote, err := l.Client.Get(ctx, l.Client.LeaderLockKey()); err == nil && strings.TrimSpace(remote) != "" {
			lid = remote
		}
	}
	return map[string]any{
		"is_leader":      isLead,
		"leader_id":      lid,
		"mode":           firstNonEmpty(l.Mode, "auto"),
		"workers":        l.Workers,
		"ttl_sec":        l.TTL.Seconds(),
		"renew_sec":      l.Renew.Seconds(),
		"implementation": "go",
		"redis_enabled":  l.Client != nil && l.Client.Enabled(),
	}
}

func (l *Leader) setLeader(isLead bool, id string) {
	l.mu.Lock()
	was := l.isLeader
	l.isLeader = isLead
	if isLead {
		l.leaderID = id
	} else {
		l.leaderID = ""
	}
	l.mu.Unlock()
	if isLead && !was {
		if l.OnGain != nil {
			l.OnGain()
		}
	} else if !isLead && was {
		if l.OnLost != nil {
			l.OnLost()
		}
	}
}

// ShouldStartMaintainers attempts election once and starts watcher for multi-worker auto mode.
func (l *Leader) ShouldStartMaintainers(ctx context.Context) bool {
	force := l.forceMode()
	if force != nil && !*force {
		l.setLeader(false, "")
		return false
	}
	if force != nil && *force {
		l.setLeader(true, "forced")
		return true
	}
	if l.Workers <= 1 {
		l.setLeader(true, "local")
		return true
	}
	acquired := l.TryBecomeLeader(ctx)
	l.ensureWatch()
	return acquired
}

func (l *Leader) TryBecomeLeader(ctx context.Context) bool {
	force := l.forceMode()
	if force != nil && !*force {
		l.setLeader(false, "")
		return false
	}
	if force != nil && *force {
		l.setLeader(true, "forced")
		return true
	}
	if l.Workers <= 1 {
		l.setLeader(true, "local")
		return true
	}
	if l.Client == nil || !l.Client.Enabled() {
		l.setLeader(false, "")
		return false
	}
	wid := l.WorkerID
	key := l.Client.LeaderLockKey()
	ok, err := l.Client.TryAcquireLock(ctx, key, wid, l.TTL)
	if err != nil {
		l.setLeader(false, "")
		return false
	}
	if !ok {
		cur, _ := l.Client.Get(ctx, key)
		if cur == wid {
			renewed, _ := l.Client.RenewLock(ctx, key, wid, l.TTL)
			ok = renewed
		}
	}
	if ok {
		l.setLeader(true, wid)
		return true
	}
	l.setLeader(false, "")
	return false
}

func (l *Leader) ensureWatch() {
	force := l.forceMode()
	if force != nil || l.Workers <= 1 {
		return
	}
	l.mu.Lock()
	if l.started {
		l.mu.Unlock()
		return
	}
	l.started = true
	l.mu.Unlock()
	go l.watchLoop()
}

func (l *Leader) watchLoop() {
	interval := l.Renew
	if interval > l.TTL/2 {
		interval = l.TTL / 2
	}
	if interval < 2*time.Second {
		interval = 2 * time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-l.stop:
			return
		case <-ticker.C:
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			if l.Client == nil || !l.Client.Enabled() {
				l.setLeader(false, "")
				cancel()
				continue
			}
			wid := l.WorkerID
			key := l.Client.LeaderLockKey()
			cur, err := l.Client.Get(ctx, key)
			if err != nil {
				cancel()
				continue
			}
			if cur == wid {
				if ok, _ := l.Client.RenewLock(ctx, key, wid, l.TTL); ok {
					l.setLeader(true, wid)
				} else {
					l.setLeader(false, "")
				}
				cancel()
				continue
			}
			if cur != "" {
				l.setLeader(false, "")
				cancel()
				continue
			}
			_ = l.TryBecomeLeader(ctx)
			cancel()
		}
	}
}

func (l *Leader) Release(ctx context.Context) {
	select {
	case <-l.stop:
	default:
		close(l.stop)
	}
	force := l.forceMode()
	if force != nil && *force {
		l.setLeader(false, "")
		return
	}
	if l.Workers <= 1 {
		l.setLeader(false, "")
		return
	}
	if l.Client != nil && l.Client.Enabled() {
		_, _ = l.Client.ReleaseLock(ctx, l.Client.LeaderLockKey(), l.WorkerID)
	}
	l.setLeader(false, "")
	l.mu.Lock()
	l.started = false
	l.mu.Unlock()
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}
