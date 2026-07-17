package maintainer

import (
	"context"
	"log/slog"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/accounts"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/oidc"
)

type Service struct {
	Store    *postgres.Connector
	Redis    *redis.Client
	OIDC     *oidc.Client
	Interval time.Duration
	Batch    int
	Skew     time.Duration
	Enabled  func() bool
	IsLeader func() bool

	mu        sync.Mutex
	started   bool
	stop      chan struct{}
	runSoon   chan struct{}
	last      map[string]any
	forceNext bool
}

func New(store *postgres.Connector, redisClient *redis.Client, oidcClient *oidc.Client) *Service {
	return &Service{
		Store:    store,
		Redis:    redisClient,
		OIDC:     oidcClient,
		Interval: envDurationSec("GROK2API_TOKEN_MAINTAIN_INTERVAL", 60*time.Second, 5*time.Second, 30*time.Minute),
		Batch:    envInt("GROK2API_TOKEN_REFRESH_BATCH", 50, 1, 500),
		Skew:     envDurationSec("GROK2API_TOKEN_REFRESH_SKEW", 180*time.Second, 30*time.Second, 2*time.Hour),
		Enabled:  func() bool { return true },
		IsLeader: func() bool { return true },
		stop:     make(chan struct{}),
		runSoon:  make(chan struct{}, 1),
		last:     map[string]any{"ok": true, "started": false},
	}
}

func (s *Service) Start() {
	s.mu.Lock()
	if s.started {
		s.mu.Unlock()
		return
	}
	s.started = true
	s.mu.Unlock()
	go s.loop()
}

func (s *Service) Stop() {
	s.mu.Lock()
	if !s.started {
		s.mu.Unlock()
		return
	}
	s.started = false
	close(s.stop)
	s.stop = make(chan struct{})
	s.mu.Unlock()
}

func (s *Service) RequestRunSoon(force bool) {
	s.mu.Lock()
	s.forceNext = s.forceNext || force
	s.mu.Unlock()
	select {
	case s.runSoon <- struct{}{}:
	default:
	}
}

func (s *Service) Status() map[string]any {
	if s == nil {
		return map[string]any{"enabled": false, "implementation": "go", "started": false, "running": false}
	}
	s.mu.Lock()
	lastCopy := map[string]any{}
	for k, v := range s.last {
		lastCopy[k] = v
	}
	started := s.started
	interval := s.Interval
	batch := s.Batch
	skew := s.Skew
	s.mu.Unlock()

	enabled := s.Enabled == nil || s.Enabled()
	isLeader := s.IsLeader == nil || s.IsLeader()
	running := started && enabled && isLeader
	out := map[string]any{
		"enabled":            enabled,
		"started":            started,
		"running":            running,
		"local_running":      running,
		"cluster_running":    running,
		"leader_running":     running,
		"implementation":     "go",
		"interval_sec":       interval.Seconds(),
		"next_wait_sec":      interval.Seconds(),
		"batch":              batch,
		"refresh_batch":      batch,
		"adaptive_batch":     batch,
		"refresh_skew_sec":   skew.Seconds(),
		"background_skew_sec": skew.Seconds(),
		"is_leader":          isLeader,
		"last":               lastCopy,
	}
	if rem, ok := s.computeMinRemainingSec(context.Background()); ok {
		out["min_remaining_sec"] = rem
		lastCopy["min_remaining_sec"] = rem
		out["last"] = lastCopy
	}
	return out
}

func (s *Service) enrichStatusMinRemaining(out map[string]any) {
	rem, ok := s.computeMinRemainingSec(context.Background())
	if !ok {
		return
	}
	out["min_remaining_sec"] = rem
	if last, ok := out["last"].(map[string]any); ok {
		last["min_remaining_sec"] = rem
		out["last"] = last
	}
}

func (s *Service) loop() {
	// short startup delay like Python
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			s.maybeRun(false)
			timer.Reset(s.Interval)
		case <-s.runSoon:
			force := false
			s.mu.Lock()
			force = s.forceNext
			s.forceNext = false
			s.mu.Unlock()
			s.maybeRun(force)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(s.Interval)
		}
	}
}

func (s *Service) maybeRun(force bool) {
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	if s.IsLeader != nil && !s.IsLeader() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	result := s.RunOnce(ctx, force)
	s.mu.Lock()
	s.last = result
	s.mu.Unlock()
}

// RunOnce performs one normalize-ish + refresh cycle against PostgreSQL.
func (s *Service) RunOnce(ctx context.Context, force bool) map[string]any {
	result := map[string]any{
		"ok":             true,
		"force":          force,
		"implementation": "go",
		"at":             time.Now().Unix(),
	}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}
	// maintenance lock best-effort
	if s.Redis != nil && s.Redis.Enabled() {
		ok, release, err := s.Redis.AcquireMaintenanceLock(ctx, "token_maintainer", 180*time.Second, true)
		if err == nil && ok {
			defer release()
		} else if err == nil && !ok {
			result["deferred_busy"] = true
			result["error"] = "maintenance slot busy — deferred"
			return result
		}
	}
	if n, err := s.Store.ExpireDueCooldowns(ctx, 200); err == nil {
		result["expired_cooldowns"] = n
	}
	batch := s.Batch
	if batch <= 0 {
		batch = 40
	}
	rows, err := s.Store.ListRefreshableAccounts(ctx, batch*2)
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}
	now := time.Now()
	skew := s.Skew
	if skew <= 0 {
		skew = 2 * time.Minute
	}
	candidates := make([]postgres.AccountRefreshRow, 0, batch)
	for _, row := range rows {
		rt := stringFrom(row.Payload, "refresh_token")
		if rt == "" {
			continue
		}
		if truthy(row.Payload["refresh_invalid"]) {
			continue
		}
		if !force {
			exp := accounts.ParseExpiresAt(row.Payload["expires_at"], stringFrom(row.Payload, "key"))
			if exp != nil && float64(now.Unix())+skew.Seconds() < *exp {
				continue
			}
		}
		candidates = append(candidates, row)
		if len(candidates) >= batch {
			break
		}
	}
	refreshed, failed, skipped, deleted := 0, 0, 0, 0
	failedSample := []map[string]any{}
	oidcClient := s.OIDC
	if oidcClient == nil {
		oidcClient = &oidc.Client{}
	}
	for _, row := range candidates {
		tokenData, err := oidcClient.RefreshAccessToken(ctx, row.Payload)
		if err != nil {
			failed++
			permanent := false
			errText := err.Error()
			var re *oidc.RefreshError
			if asRefresh(err, &re) {
				permanent = re.Permanent
				errText = re.Error()
			}
			if permanent {
				_ = s.Store.MarkRefreshInvalid(ctx, row.ID, errText)
				// hard-delete accounts without SSO when RT permanently dead
				if accounts.GetSSOValue(row.Payload) == "" {
					if ok, _ := s.Store.DeleteAccount(ctx, row.ID); ok {
						deleted++
					}
				}
			}
			if len(failedSample) < 5 {
				failedSample = append(failedSample, map[string]any{"id": row.ID, "error": errText, "permanent": permanent})
			}
			continue
		}
		newID, entry, err := oidc.EntryFromTokenResponse(tokenData, row.Payload)
		if err != nil {
			failed++
			continue
		}
		if newID == "" {
			newID = row.ID
		}
		if newID != row.ID {
			// move row if storage id changed
			_ = s.Store.UpsertAccount(ctx, newID, entry)
			if newID != row.ID {
				_, _ = s.Store.DeleteAccount(ctx, row.ID)
			}
		} else {
			_ = s.Store.UpsertAccount(ctx, row.ID, entry)
		}
		// clear cooldown after successful renew
		_, _ = s.Store.ClearAccountCooldown(ctx, newID)
		refreshed++
	}
	if rem, ok := s.computeMinRemainingSec(ctx); ok {
		result["min_remaining_sec"] = rem
	}
	result["next_wait_sec"] = s.Interval.Seconds()
	result["adaptive"] = map[string]any{"batch": batch, "skew_sec": skew.Seconds()}
	result["refresh"] = map[string]any{
		"attempted":     len(candidates),
		"refreshed":     refreshed,
		"failed":        failed,
		"skipped":       skipped,
		"deleted":       deleted,
		"failed_sample": failedSample,
		"workers":       1,
		"batch":         batch,
	}
	result["accounts_total"] = len(rows)
	slog.Info("token maintainer cycle", "attempted", len(candidates), "refreshed", refreshed, "failed", failed, "deleted", deleted)
	return result
}

func asRefresh(err error, target **oidc.RefreshError) bool {
	if err == nil {
		return false
	}
	if e, ok := err.(*oidc.RefreshError); ok {
		*target = e
		return true
	}
	return false
}

func stringFrom(m map[string]any, key string) string {
	if m == nil {
		return ""
	}
	if s, ok := m[key].(string); ok {
		return strings.TrimSpace(s)
	}
	return ""
}

func truthy(v any) bool {
	switch t := v.(type) {
	case bool:
		return t
	case string:
		s := strings.ToLower(strings.TrimSpace(t))
		return s == "1" || s == "true" || s == "yes"
	default:
		return false
	}
}


func (s *Service) computeMinRemainingSec(ctx context.Context) (float64, bool) {
	if s == nil || s.Store == nil {
		return 0, false
	}
	rows, err := s.Store.ListRefreshableAccounts(ctx, 200)
	if err != nil || len(rows) == 0 {
		return 0, false
	}
	now := float64(time.Now().Unix())
	minRem := 0.0
	found := false
	for _, row := range rows {
		exp := accounts.ParseExpiresAt(row.Payload["expires_at"], stringFrom(row.Payload, "key"))
		if exp == nil {
			continue
		}
		rem := *exp - now
		if !found || rem < minRem {
			minRem = rem
			found = true
		}
	}
	return minRem, found
}

func envDurationSec(name string, fallback, min, max time.Duration) time.Duration {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	sec, err := strconv.ParseFloat(raw, 64)
	if err != nil {
		return fallback
	}
	d := time.Duration(sec * float64(time.Second))
	if d < min {
		return min
	}
	if d > max {
		return max
	}
	return d
}

func envInt(name string, fallback, min, max int) int {
	raw := strings.TrimSpace(os.Getenv(name))
	if raw == "" {
		return fallback
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return fallback
	}
	if n < min {
		return min
	}
	if n > max {
		return max
	}
	return n
}
