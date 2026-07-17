package modelhealth

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

// Default probe knobs mirror Python grok2api.pool.model_health.
const (
	defaultWorkers            = 4
	defaultBatch              = 30
	defaultMaxModelsPerAcct   = 2
	defaultCycleBudget        = 150 * time.Second
	defaultManualCycleBudget  = 150 * time.Second
	defaultProbeTimeout       = 20 * time.Second
	defaultManualAccountLimit = 5000
	// Multi-wave manual_all: keep each wave short (lock-friendly) but continue
	// until the live pool is covered or maxWaves is hit.
	defaultManualMaxWaves   = 40
	defaultManualWaveBudget = 90 * time.Second
	// Full-job wall clock for async probe-all (multi-wave).
	defaultManualJobTimeout = 30 * time.Minute
	defaultSweepTTLSec      = 12 * 3600
)

type Service struct {
	Store               *postgres.Connector
	Redis               *redis.Client
	Upstream            string
	Models              []string
	Interval            time.Duration
	Batch               int
	Workers             int
	MaxModelsPerAccount int
	CycleBudget         time.Duration
	ManualCycleBudget   time.Duration
	ManualMaxWaves      int
	AutoDisable         bool
	Enabled             func() bool
	IsLeader            func() bool

	mu         sync.Mutex
	started    bool
	stop       chan struct{}
	runSoon    chan struct{}
	last       map[string]any
	modelRR    int
	httpClient *http.Client

	// Async manual_all job (admin "全部模型探测").
	jobMu      sync.Mutex
	job        map[string]any
	jobCancel  context.CancelFunc
	jobRunning bool

	// Local fallback when Redis sweep is unavailable.
	localSweepMu      sync.Mutex
	localSweepGen     int64
	localSweepStart   float64
	localSweepCovered map[string]struct{}
}

func New(store *postgres.Connector, redisClient *redis.Client, upstream string, models []string) *Service {
	models = normalizeModels(models)
	if len(models) == 0 {
		models = normalizeModels(splitCSV(os.Getenv("GROK2API_PROBE_MODELS")))
	}
	if len(models) == 0 {
		models = []string{"grok-4.5"}
	}
	return &Service{
		Store:               store,
		Redis:               redisClient,
		Upstream:            strings.TrimRight(upstream, "/"),
		Models:              models,
		Interval:            envDurationSec("GROK2API_MODEL_HEALTH_INTERVAL", 15*time.Minute, 30*time.Second, 2*time.Hour),
		Batch:               envInt("GROK2API_MODEL_PROBE_BATCH", defaultBatch, 1, 500),
		Workers:             envInt("GROK2API_MODEL_PROBE_WORKERS", defaultWorkers, 1, 32),
		MaxModelsPerAccount: envInt("GROK2API_MODEL_PROBE_MAX_MODELS_PER_ACCOUNT", defaultMaxModelsPerAcct, 1, 16),
		CycleBudget:         envDurationSec("GROK2API_MODEL_PROBE_CYCLE_BUDGET", defaultCycleBudget, 15*time.Second, 4*time.Minute),
		ManualCycleBudget:   envDurationSec("GROK2API_MODEL_PROBE_MANUAL_BUDGET", defaultManualCycleBudget, 30*time.Second, 4*time.Minute),
		AutoDisable:         true,
		ManualMaxWaves:      envInt("GROK2API_MODEL_PROBE_MANUAL_MAX_WAVES", defaultManualMaxWaves, 1, 200),
		Enabled:             func() bool { return true },
		IsLeader:            func() bool { return true },
		stop:                make(chan struct{}),
		runSoon:             make(chan struct{}, 1),
		last:                map[string]any{"ok": true, "started": false},
		httpClient:          newProbeHTTPClient(),
		job:                 map[string]any{"running": false},
		localSweepCovered:   map[string]struct{}{},
	}
}

func newProbeHTTPClient() *http.Client {
	// Shared client for dense probe batches: reuse TLS/TCP across accounts.
	// Sized for MODEL_PROBE_WORKERS up to 32 without thrashing the dialer.
	return &http.Client{
		Timeout: defaultProbeTimeout,
		Transport: &http.Transport{
			MaxIdleConns:          128,
			MaxIdleConnsPerHost:   64,
			MaxConnsPerHost:       96,
			IdleConnTimeout:       60 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 15 * time.Second,
			ForceAttemptHTTP2:     true,
			DisableKeepAlives:     false,
			DialContext: (&net.Dialer{
				Timeout:   8 * time.Second,
				KeepAlive: 30 * time.Second,
			}).DialContext,
		},
	}
}

func (s *Service) probeHTTP() *http.Client {
	if s == nil {
		return newProbeHTTPClient()
	}
	// httpClient is set in New and only replaced under rare config reloads;
	// read without lock on the hot probe path to avoid worker contention.
	if c := s.httpClient; c != nil {
		return c
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.httpClient == nil {
		s.httpClient = newProbeHTTPClient()
	}
	return s.httpClient
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

func (s *Service) RequestRunSoon() {
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
	workers := s.Workers
	models := append([]string{}, s.Models...)
	s.mu.Unlock()

	enabled := s.Enabled == nil || s.Enabled()
	isLeader := s.IsLeader == nil || s.IsLeader()
	running := started && enabled && isLeader

	if lastCopy != nil {
		if lastCopy["count"] == nil && lastCopy["probed"] != nil {
			lastCopy["count"] = lastCopy["probed"]
		}
		if lastCopy["available_count"] == nil && lastCopy["available"] != nil {
			lastCopy["available_count"] = lastCopy["available"]
		}
		if lastCopy["unavailable_count"] == nil && lastCopy["failed"] != nil {
			lastCopy["unavailable_count"] = lastCopy["failed"]
		}
		if lastCopy["models"] == nil {
			lastCopy["models"] = models
			lastCopy["models_configured"] = models
		}
	}

	out := map[string]any{
		"enabled":         enabled,
		"started":         started,
		"running":         running,
		"local_running":   running,
		"cluster_running": running,
		"leader_running":  running,
		"implementation":  "go",
		"interval_sec":    interval.Seconds(),
		"probe_batch":     batch,
		"batch":           batch,
		"probe_workers":   workers,
		"workers":         workers,
		"models":          models,
		"probe_models":    models,
		"is_leader":       isLeader,
		"last":            lastCopy,
		"selection":       "strict_sweep",
	}
	s.jobMu.Lock()
	jobCopy := map[string]any{}
	for k, v := range s.job {
		jobCopy[k] = v
	}
	s.jobMu.Unlock()
	out["job"] = jobCopy
	if s.Store != nil && batch > 0 && interval > 0 {
		if n, err := s.Store.CountEnabledAccounts(context.Background()); err == nil && n > 0 {
			cycles := (int(n) + batch - 1) / batch
			out["full_pool_eta_sec"] = float64(cycles) * interval.Seconds()
			covered, gen, mode := s.sweepSnapshot(int(n))
			remaining := int(n) - covered
			if remaining < 0 {
				remaining = int(n)
			}
			out["sweep"] = map[string]any{
				"mode": mode, "live": n, "remaining": remaining, "covered": covered,
				"generation": gen,
			}
		}
	}
	return out
}

func (s *Service) loop() {
	timer := time.NewTimer(8 * time.Second)
	defer timer.Stop()
	for {
		select {
		case <-s.stop:
			return
		case <-timer.C:
			s.maybeRun()
			timer.Reset(s.Interval)
		case <-s.runSoon:
			s.maybeRun()
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

func (s *Service) maybeRun() {
	if s.Enabled != nil && !s.Enabled() {
		return
	}
	if s.IsLeader != nil && !s.IsLeader() {
		return
	}
	// Background cycles stay inside the maintenance lock budget (~3 min).
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	result := s.RunOnce(ctx, "background")
	s.mu.Lock()
	s.last = result
	s.mu.Unlock()
	s.writeTaskLog(ctx, "background", result)
}

func (s *Service) RunOnce(ctx context.Context, source string) map[string]any {
	// Background: one priority batch under cycle budget.
	// manual_all: multi-wave until pool covered or max waves / job cancel.
	if source == "manual_all" || source == "manual" || source == "admin" {
		return s.runManualAll(ctx, source)
	}
	return s.runWave(ctx, source, false)
}

// StartProbeAll starts (or returns) an async full-pool probe job.
// Admin UI can poll JobStatus / model-health Status().job.
func (s *Service) StartProbeAll() map[string]any {
	if s == nil {
		return map[string]any{"ok": false, "error": "service unavailable"}
	}
	s.jobMu.Lock()
	if s.jobRunning {
		job := cloneMap(s.job)
		s.jobMu.Unlock()
		job["ok"] = true
		job["already_running"] = true
		return job
	}
	ctx, cancel := context.WithTimeout(context.Background(), defaultManualJobTimeout)
	s.jobCancel = cancel
	s.jobRunning = true
	started := time.Now()
	s.job = map[string]any{
		"ok": true, "running": true, "source": "manual_all",
		"started_at": started.Unix(), "wave": 0, "waves": 0,
		"probed": 0, "available": 0, "failed": 0,
		"implementation": "go",
	}
	jobSnap := cloneMap(s.job)
	s.jobMu.Unlock()

	go func() {
		result := s.runManualAll(ctx, "manual_all")
		cancel()
		s.jobMu.Lock()
		s.jobRunning = false
		s.jobCancel = nil
		result["running"] = false
		result["finished_at"] = time.Now().Unix()
		if result["ok"] == nil {
			result["ok"] = true
		}
		s.job = result
		s.jobMu.Unlock()
		s.mu.Lock()
		s.last = result
		s.mu.Unlock()
	}()
	return jobSnap
}

// JobStatus returns the current/last async probe-all job snapshot.
func (s *Service) JobStatus() map[string]any {
	if s == nil {
		return map[string]any{"running": false}
	}
	s.jobMu.Lock()
	defer s.jobMu.Unlock()
	return cloneMap(s.job)
}

func (s *Service) runManualAll(ctx context.Context, source string) map[string]any {
	startedAt := time.Now()
	result := map[string]any{"ok": true, "source": source, "implementation": "go", "at": startedAt.Unix()}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}

	maxWaves := s.ManualMaxWaves
	if maxWaves <= 0 {
		maxWaves = defaultManualMaxWaves
	}
	// Manual waves use a shorter per-wave budget so the maintenance lock renews between waves.
	waveBudget := s.ManualCycleBudget
	if waveBudget <= 0 {
		waveBudget = defaultManualWaveBudget
	}
	if waveBudget > 2*time.Minute {
		// Keep each lock hold reasonable; multi-wave covers the rest.
		waveBudget = 2 * time.Minute
	}
	if waveBudget < 30*time.Second {
		waveBudget = 30 * time.Second
	}

	totalAvailable, totalFailed, totalProbes, totalAccounts := 0, 0, 0, 0
	kickCooldown, kickDisabled, modelBlocks, recovered := 0, 0, 0, 0
	samples := []map[string]any{}
	var lastModels []string
	var lastWorkers int
	waves := 0
	budgetHitFinal := false

	for wave := 1; wave <= maxWaves; wave++ {
		if ctx.Err() != nil {
			budgetHitFinal = true
			break
		}
		// Per-wave context with its own budget; parent cancel still stops the job.
		waveCtx, cancel := context.WithTimeout(ctx, waveBudget+30*time.Second)
		waveRes := s.runWave(waveCtx, source, true)
		cancel()
		waves = wave

		probed, _ := waveRes["probed"].(int)
		count, _ := waveRes["count"].(int)
		avail, _ := waveRes["available_count"].(int)
		if avail == 0 {
			if v, ok := waveRes["available"].(int); ok {
				avail = v
			}
		}
		failed, _ := waveRes["unavailable_count"].(int)
		if failed == 0 {
			if v, ok := waveRes["failed"].(int); ok {
				failed = v
			}
		}
		totalAccounts += probed
		totalProbes += count
		totalAvailable += avail
		totalFailed += failed
		kickCooldown += intOf(waveRes["kick_cooldown"])
		kickDisabled += intOf(waveRes["kick_disabled"])
		modelBlocks += intOf(waveRes["model_blocked_count"])
		recovered += intOf(waveRes["recovered"])
		if ms, ok := waveRes["failed_sample"].([]map[string]any); ok {
			for _, row := range ms {
				if len(samples) < 8 {
					samples = append(samples, row)
				}
			}
		}
		if m, ok := waveRes["models"].([]string); ok {
			lastModels = m
		}
		lastWorkers = intOf(waveRes["workers"])
		if waveRes["budget_hit"] == true {
			budgetHitFinal = true
		}

		// Progress snapshot for UI polling.
		s.jobMu.Lock()
		if s.jobRunning {
			s.job["wave"] = wave
			s.job["waves"] = wave
			s.job["probed"] = totalAccounts
			s.job["count"] = totalProbes
			s.job["available"] = totalAvailable
			s.job["available_count"] = totalAvailable
			s.job["failed"] = totalFailed
			s.job["unavailable_count"] = totalFailed
			s.job["running"] = true
			s.job["elapsed_ms"] = time.Since(startedAt).Milliseconds()
			if sw, ok := waveRes["sweep"].(map[string]any); ok {
				s.job["sweep"] = sw
			}
		}
		s.jobMu.Unlock()

		// Stop when nothing left to probe this generation.
		remaining := intOfMap(waveRes, "sweep", "remaining")
		if remaining == 0 && probed == 0 {
			// empty pool
			break
		}
		if remaining == 0 {
			budgetHitFinal = false
			break
		}
		// If this wave probed nothing useful (lock busy / empty), stop to avoid spin.
		if waveRes["deferred_busy"] == true {
			result["deferred_busy"] = true
			break
		}
		if probed == 0 {
			break
		}
	}

	if len(lastModels) == 0 {
		lastModels = s.modelsForSource(source)
	}
	result["probed"] = totalAccounts
	result["count"] = totalProbes
	result["available"] = totalAvailable
	result["available_count"] = totalAvailable
	result["failed"] = totalFailed
	result["unavailable_count"] = totalFailed
	result["auto_action_count"] = kickCooldown + kickDisabled + modelBlocks
	result["kick_cooldown"] = kickCooldown
	result["kick_disabled"] = kickDisabled
	result["model_blocked_count"] = modelBlocks
	result["recovered"] = recovered
	result["failed_sample"] = samples
	result["models"] = lastModels
	result["models_configured"] = append([]string{}, s.Models...)
	if len(lastModels) > 0 {
		result["model"] = lastModels[0]
	}
	result["workers"] = lastWorkers
	result["waves"] = waves
	result["budget_hit"] = budgetHitFinal
	result["elapsed_ms"] = time.Since(startedAt).Milliseconds()
	// Attach current sweep snapshot.
	live := 0
	if s.Store != nil {
		if n, err := s.Store.CountEnabledAccounts(ctx); err == nil {
			live = int(n)
		}
	}
	covered, gen, mode := s.sweepSnapshot(live)
	remaining := live - covered
	if remaining < 0 {
		remaining = 0
	}
	result["sweep"] = map[string]any{
		"mode": mode, "live": live, "covered": covered, "remaining": remaining, "generation": gen,
	}
	if remaining > 0 {
		result["deferred"] = remaining
	}
	slog.Info("model health manual_all complete",
		"waves", waves, "probed", totalAccounts, "probes", totalProbes,
		"available", totalAvailable, "failed", totalFailed,
		"covered", covered, "remaining", remaining, "elapsed_ms", result["elapsed_ms"],
	)
	s.writeTaskLog(ctx, source, result)
	return result
}

// runWave performs one locked probe wave (background batch or one manual wave).
func (s *Service) runWave(ctx context.Context, source string, manualWave bool) map[string]any {
	startedAt := time.Now()
	result := map[string]any{"ok": true, "source": source, "implementation": "go", "at": startedAt.Unix()}
	if s.Store == nil {
		result["ok"] = false
		result["error"] = "store unavailable"
		return result
	}
	if s.Redis != nil && s.Redis.Enabled() {
		// Lock TTL must cover the wave; renew loop inside AcquireMaintenanceLock.
		lockTTL := 180 * time.Second
		ok, release, err := s.Redis.AcquireMaintenanceLock(ctx, "model_health", lockTTL, true)
		if err == nil && ok {
			defer release()
		} else if err == nil && !ok {
			result["deferred_busy"] = true
			return result
		}
	}

	batch := s.Batch
	if batch <= 0 {
		batch = defaultBatch
	}
	// Manual waves still use larger per-wave selection (up to 5000) so one wave
	// can chew through as many accounts as the budget allows.
	limit := batch
	if manualWave {
		limit = defaultManualAccountLimit
		if n, err := s.Store.CountEnabledAccounts(ctx); err == nil && n > 0 {
			if int(n) < limit {
				limit = int(n)
			}
			if limit < batch {
				limit = batch
			}
		}
	}

	// Over-fetch then filter by sweep covered set so we fill the batch with uncovered.
	fetchLimit := limit
	if !manualWave {
		// Background: fetch more candidates so covered filtering still fills Batch.
		fetchLimit = batch * 4
		if fetchLimit < 100 {
			fetchLimit = 100
		}
		if fetchLimit > 2000 {
			fetchLimit = 2000
		}
	}
	auths, err := s.Store.ListAccountAuthsForProbe(ctx, fetchLimit)
	if err != nil {
		auths, err = s.Store.ListAccountAuths(ctx, fetchLimit, true)
	}
	if err != nil {
		result["ok"] = false
		result["error"] = err.Error()
		return result
	}

	// Strict non-repeat sweep for background; manual_all also marks covered so
	// multi-wave does not re-probe the same accounts within the job.
	liveN := 0
	if n, err := s.Store.CountEnabledAccounts(ctx); err == nil {
		liveN = int(n)
	}
	sweepInfo, uncovered := s.filterUncovered(ctx, auths, liveN, source)
	if len(uncovered) > limit {
		uncovered = uncovered[:limit]
	}
	auths = uncovered

	cycleModels := s.modelsForSource(source)
	if len(cycleModels) == 0 {
		cycleModels = []string{"grok-4.5"}
	}

	workers := s.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	if manualWave {
		boosted := workers * 2
		if boosted > 8 {
			boosted = 8
		}
		if boosted < workers {
			boosted = workers
		}
		workers = boosted
	}
	if workers > len(auths) && len(auths) > 0 {
		workers = len(auths)
	}

	budget := s.CycleBudget
	if budget <= 0 {
		budget = defaultCycleBudget
	}
	if manualWave {
		if s.ManualCycleBudget > 0 {
			budget = s.ManualCycleBudget
		}
		if budget > 2*time.Minute {
			budget = 2 * time.Minute
		}
		if budget < 30*time.Second {
			budget = 30 * time.Second
		}
	}
	if dl, ok := ctx.Deadline(); ok {
		remain := time.Until(dl)
		if remain > 0 && remain < budget {
			budget = remain
		}
	}
	budgetCtx, cancel := context.WithTimeout(ctx, budget)
	defer cancel()

	probes := s.probeAccountsConcurrent(budgetCtx, auths, cycleModels, source, workers)

	available, failed := 0, 0
	kickCooldown, kickDisabled, modelBlocks, recovered := 0, 0, 0, 0
	samples := []map[string]any{}
	budgetHit := budgetCtx.Err() != nil
	coverIDs := make([]string, 0, len(auths))
	holdIDs := make([]string, 0)
	seenAcct := map[string]bool{}
	for _, probe := range probes {
		aid, _ := probe["account_id"].(string)
		if probe["available"] == true {
			available++
			if probe["recovered"] == true {
				recovered++
			}
			if aid != "" && !seenAcct[aid] {
				coverIDs = append(coverIDs, aid)
				seenAcct[aid] = true
			}
			continue
		}
		if probe["budget_cut"] == true {
			// Do not cover — leave for next wave.
			continue
		}
		failed++
		if probe["auto_disabled"] == true {
			kickDisabled++
		}
		if probe["kicked_cooldown"] == true {
			kickCooldown++
		}
		if probe["model_blocked"] == true {
			modelBlocks++
		}
		if len(samples) < 5 {
			samples = append(samples, probe)
		}
		// Recoverable free-usage / rate-limit stays UNcovered so the same
		// generation can re-check after cooldown (Python parity).
		if aid != "" && !seenAcct[aid] {
			errText, _ := probe["error"].(string)
			if isFreeUsageExhausted(errText) || probe["model_blocked"] == true || probe["kicked_cooldown"] == true {
				holdIDs = append(holdIDs, aid)
			} else {
				coverIDs = append(coverIDs, aid)
			}
			seenAcct[aid] = true
		}
	}
	// Mark covered after probes complete.
	if len(coverIDs) > 0 {
		coveredTotal := s.markCovered(ctx, coverIDs)
		sweepInfo["sweep_covered"] = coveredTotal
		if liveN > 0 {
			sweepInfo["sweep_remaining"] = maxInt(0, liveN-coveredTotal)
		}
	}
	if len(holdIDs) > 0 {
		sweepInfo["held_recoverable"] = len(holdIDs)
	}

	autoActions := kickCooldown + kickDisabled + modelBlocks
	result["probed"] = len(auths)
	result["count"] = len(probes)
	result["available"] = available
	result["available_count"] = available
	result["failed"] = failed
	result["unavailable_count"] = failed
	result["auto_action_count"] = autoActions
	result["kick_cooldown"] = kickCooldown
	result["kick_disabled"] = kickDisabled
	result["model_blocked_count"] = modelBlocks
	result["recovered"] = recovered
	result["failed_sample"] = samples
	result["model"] = cycleModels[0]
	result["models"] = append([]string{}, cycleModels...)
	result["models_configured"] = append([]string{}, s.Models...)
	result["models_per_account"] = len(cycleModels)
	result["workers"] = workers
	result["budget_sec"] = budget.Seconds()
	result["budget_hit"] = budgetHit
	result["elapsed_ms"] = time.Since(startedAt).Milliseconds()
	result["sweep"] = map[string]any{
		"mode":             sweepInfo["mode"],
		"live":             liveN,
		"covered":          sweepInfo["sweep_covered"],
		"remaining":        sweepInfo["sweep_remaining"],
		"generation":       sweepInfo["sweep_generation"],
		"held_recoverable": sweepInfo["held_recoverable"],
	}
	if rem, ok := sweepInfo["sweep_remaining"].(int); ok && rem > 0 {
		result["deferred"] = rem
	}
	slog.Info("model health wave",
		"probed", len(auths), "probes", len(probes),
		"available", available, "failed", failed,
		"models", cycleModels, "workers", workers, "budget_hit", budgetHit,
		"source", source, "elapsed_ms", time.Since(startedAt).Milliseconds(),
	)
	return result
}

// probeAccountsConcurrent fans out across accounts (models per account stay sequential).
// last_probe snapshots are flushed in one bulk upsert after the wave to cut DB round-trips.
func (s *Service) probeAccountsConcurrent(ctx context.Context, auths []postgres.AccountAuth, models []string, source string, workers int) []map[string]any {
	if len(auths) == 0 {
		return nil
	}
	if workers <= 0 {
		workers = 1
	}
	if workers > len(auths) {
		workers = len(auths)
	}

	type job struct {
		auth postgres.AccountAuth
	}
	jobs := make(chan job, workers*2)
	var (
		mu      sync.Mutex
		out     = make([]map[string]any, 0, len(auths)*len(models))
		wg      sync.WaitGroup
		skipped atomic.Int64
	)

	workerFn := func() {
		defer wg.Done()
		for j := range jobs {
			if ctx.Err() != nil {
				skipped.Add(1)
				continue
			}
			// Models for one account stay sequential so one bad model cannot
			// multiply concurrent load for the same token (Python parity).
			autoDisable := s.AutoDisable
			for _, model := range models {
				if ctx.Err() != nil {
					break
				}
				// Defer last_probe write; kick/disable still apply immediately.
				probe := s.probeAccount(ctx, j.auth, model, source, autoDisable, true)
				mu.Lock()
				out = append(out, probe)
				mu.Unlock()
			}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}

	for _, auth := range auths {
		if ctx.Err() != nil {
			skipped.Add(1)
			continue
		}
		select {
		case <-ctx.Done():
			skipped.Add(1)
		case jobs <- job{auth: auth}:
		}
	}
	close(jobs)
	wg.Wait()
	_ = skipped.Load()

	// Bulk flush last_probe (best-effort; individual SaveLastProbe already skipped).
	if s.Store != nil && len(out) > 0 {
		// Prefer newest probe per account when multi-model (last write wins in batch order).
		// Keep all rows — SaveLastProbesBatch upserts by account_id; later rows overwrite earlier.
		// To make "last model" stick, reverse-unique by account_id.
		byAcct := make(map[string]map[string]any, len(auths))
		order := make([]string, 0, len(auths))
		for _, p := range out {
			if p == nil || p["budget_cut"] == true {
				continue
			}
			aid, _ := p["account_id"].(string)
			if aid == "" {
				continue
			}
			if _, ok := byAcct[aid]; !ok {
				order = append(order, aid)
			}
			byAcct[aid] = p
		}
		batch := make([]map[string]any, 0, len(byAcct))
		for _, aid := range order {
			batch = append(batch, byAcct[aid])
		}
		if len(batch) > 0 {
			if _, err := s.Store.SaveLastProbesBatch(ctx, batch); err != nil {
				slog.Warn("model health batch last_probe failed; falling back", "error", err, "n", len(batch))
				for _, p := range batch {
					aid, _ := p["account_id"].(string)
					_ = s.Store.SaveLastProbe(ctx, aid, p)
				}
			}
		}
	}
	return out
}

func (s *Service) ProbeAccount(ctx context.Context, auth postgres.AccountAuth, model, source string) map[string]any {
	return s.probeAccount(ctx, auth, model, source, s.AutoDisable, false)
}

func (s *Service) probeAccount(ctx context.Context, auth postgres.AccountAuth, model, source string, autoDisable bool, deferSave bool) map[string]any {
	if model == "" && len(s.Models) > 0 {
		model = s.Models[0]
	}
	started := time.Now()
	base := map[string]any{
		"ok": false, "available": false,
		"account_id": auth.ID, "email": auth.Email, "model": model,
		"probed_at": started.Unix(), "source": source,
	}
	// Reuse shared transport; per-call Timeout is already on the client.
	client := &grok.Client{BaseURL: s.Upstream, HTTP: s.probeHTTP()}
	body := map[string]any{
		"model": model, "stream": true, "max_tokens": 8,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	}
	resp, err := client.Open(ctx, grok.Account{ID: auth.ID, Token: auth.Token}, model, body)
	if err != nil {
		status := 0
		errText := err.Error()
		var ue *grok.UpstreamError
		if errors.As(err, &ue) {
			status = ue.Status
			errText = ue.Body
			if len(errText) > 400 {
				errText = errText[:400]
			}
		}
		// Context cancel/timeout is not an account fault — surface cleanly.
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || ctx.Err() != nil {
			base["status_code"] = status
			base["error"] = "probe budget exceeded"
			base["latency_ms"] = time.Since(started).Milliseconds()
			base["budget_cut"] = true
			return base
		}
		base["status_code"] = status
		base["error"] = errText
		base["latency_ms"] = time.Since(started).Milliseconds()
		if autoDisable && s.Store != nil {
			switch {
			case status == 401 || status == 403:
				if _, e := s.Store.SetAccountEnabled(ctx, auth.ID, false); e == nil {
					base["auto_disabled"] = true
				}
			case isFreeUsageExhausted(errText) || status == 429:
				until := time.Now().Add(2 * time.Hour)
				if e := s.Store.BlockPoolModel(ctx, auth.ID, model, &until); e == nil {
					base["model_blocked"] = true
				} else {
					sec := 600.0
					if _, e2 := s.Store.KickFromPool(ctx, auth.ID, errText, &sec); e2 == nil {
						base["kicked_cooldown"] = true
					}
				}
			case status >= 500:
				sec := 300.0
				if _, e := s.Store.KickFromPool(ctx, auth.ID, errText, &sec); e == nil {
					base["kicked_cooldown"] = true
				}
			}
		}
		if s.Store != nil && !deferSave {
			_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		}
		return base
	}
	n, _ := io.Copy(io.Discard, io.LimitReader(resp.Body, 2048))
	_ = resp.Body.Close()
	if n == 0 && resp.StatusCode == 200 {
		base["status_code"] = 200
		base["error"] = "empty model output"
		base["latency_ms"] = time.Since(started).Milliseconds()
		if s.Store != nil && !deferSave {
			_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		}
		return base
	}
	base["ok"] = true
	base["available"] = true
	base["status_code"] = resp.StatusCode
	base["latency_ms"] = time.Since(started).Milliseconds()
	if s.Store != nil {
		// Clearing cooldown is still immediate so recovered accounts re-enter rotation
		// even when last_probe is deferred to the batch flush.
		if _, err := s.Store.ClearAccountCooldown(ctx, auth.ID); err == nil {
			base["recovered"] = true
		}
		// Successful probe for this model clears its soft/hard block (Python parity).
		if model != "" {
			if err := s.Store.UnblockPoolModel(ctx, auth.ID, model); err == nil {
				base["unblocked_model"] = model
			}
		}
		if !deferSave {
			_ = s.Store.SaveLastProbe(ctx, auth.ID, base)
		}
	}
	return base
}

func (s *Service) ProbeIDs(ctx context.Context, ids []string, model string, autoDisable bool, source string) []map[string]any {
	out := make([]map[string]any, 0, len(ids))
	if len(ids) == 0 {
		return out
	}

	// Resolve auths first (cheap DB lookups) then fan out probes.
	type resolved struct {
		id   string
		auth *postgres.AccountAuth
		err  error
	}
	resolvedList := make([]resolved, 0, len(ids))
	for _, id := range ids {
		auth, err := s.Store.GetAccountAuth(ctx, id)
		resolvedList = append(resolvedList, resolved{id: id, auth: auth, err: err})
	}

	workers := s.Workers
	if workers <= 0 {
		workers = defaultWorkers
	}
	if workers > len(resolvedList) {
		workers = len(resolvedList)
	}

	results := make([]map[string]any, len(resolvedList))
	jobs := make(chan int, workers*2)
	var wg sync.WaitGroup

	probeModel := firstNonEmpty(model, firstModel(s.Models))
	workerFn := func() {
		defer wg.Done()
		for i := range jobs {
			r := resolvedList[i]
			if r.err != nil {
				results[i] = map[string]any{"ok": false, "account_id": r.id, "error": r.err.Error()}
				continue
			}
			probe := s.probeAccount(ctx, *r.auth, probeModel, source, autoDisable, false)
			ok := probe["available"] == true
			results[i] = map[string]any{"ok": ok, "account_id": r.auth.ID, "email": r.auth.Email, "result": probe}
		}
	}

	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go workerFn()
	}
	for i := range resolvedList {
		jobs <- i
	}
	close(jobs)
	wg.Wait()

	kickCooldown, kickDisabled, modelBlocks, available := 0, 0, 0, 0
	for _, row := range results {
		out = append(out, row)
		if row["ok"] == true {
			available++
		}
		if res, ok := row["result"].(map[string]any); ok {
			if res["auto_disabled"] == true {
				kickDisabled++
			}
			if res["kicked_cooldown"] == true {
				kickCooldown++
			}
			if res["model_blocked"] == true {
				modelBlocks++
			}
		}
	}
	s.mu.Lock()
	s.last = map[string]any{
		"ok": true, "source": source, "implementation": "go", "at": time.Now().Unix(),
		"probed": len(ids), "count": len(ids),
		"available": available, "available_count": available,
		"failed": len(ids) - available, "unavailable_count": len(ids) - available,
		"auto_action_count": kickCooldown + kickDisabled + modelBlocks,
		"kick_cooldown":     kickCooldown, "kick_disabled": kickDisabled, "model_blocked_count": modelBlocks,
		"model": probeModel, "models": []string{probeModel},
		"workers": workers,
	}
	s.mu.Unlock()
	return out
}

// modelsForSource picks which models to probe this cycle.
// Background rotates ONE model (accounts×models explosion avoidance).
// Manual/admin may probe a small cap of models per account.
func (s *Service) modelsForSource(source string) []string {
	s.mu.Lock()
	models := append([]string{}, s.Models...)
	s.mu.Unlock()
	models = normalizeModels(models)
	if len(models) == 0 {
		return []string{"grok-4.5"}
	}
	if source == "background" {
		if len(models) == 1 {
			return models
		}
		s.mu.Lock()
		idx := s.modelRR % len(models)
		s.modelRR = (idx + 1) % len(models)
		picked := models[idx]
		s.mu.Unlock()
		return []string{picked}
	}
	capN := s.MaxModelsPerAccount
	if capN <= 0 {
		capN = defaultMaxModelsPerAcct
	}
	if capN > len(models) {
		capN = len(models)
	}
	return models[:capN]
}

// filterUncovered drops accounts already covered in the current sweep generation.
// Starts a new generation when none exists or the previous generation finished.
func (s *Service) filterUncovered(ctx context.Context, auths []postgres.AccountAuth, liveN int, source string) (map[string]any, []postgres.AccountAuth) {
	info := map[string]any{
		"mode":             "strict_sweep",
		"sweep_generation": int64(0),
		"sweep_covered":    0,
		"sweep_live":       liveN,
		"sweep_remaining":  liveN,
		"sweep_reset":      false,
	}
	if len(auths) == 0 {
		return info, nil
	}
	// Manual sequential: still use covered set so multi-wave does not re-hit.
	st := s.loadSweep(ctx)
	if st.Generation <= 0 {
		st = s.startSweep(ctx)
		info["sweep_reset"] = true
	}
	// If previous generation already covered entire live pool, start fresh
	// (background continuous sweep; manual_all also wants a full pass).
	if liveN > 0 && st.CoveredN >= liveN {
		st = s.startSweep(ctx)
		info["sweep_reset"] = true
	}
	info["sweep_generation"] = st.Generation
	info["sweep_covered"] = st.CoveredN
	info["sweep_remaining"] = maxInt(0, liveN-st.CoveredN)

	out := make([]postgres.AccountAuth, 0, len(auths))
	for _, a := range auths {
		if _, ok := st.Covered[a.ID]; ok {
			continue
		}
		out = append(out, a)
	}
	// If everything in the candidate list is covered but remaining > 0, the
	// priority query only returned already-covered rows — try unmarked by
	// returning empty so caller can stop or next wave re-fetches after mark.
	info["candidates"] = len(auths)
	info["selected"] = len(out)
	return info, out
}

func (s *Service) loadSweep(ctx context.Context) redis.SweepState {
	if s.Redis != nil && s.Redis.Enabled() {
		if st, err := s.Redis.LoadModelHealthSweep(ctx); err == nil && st.Generation > 0 {
			return st
		}
		if st, err := s.Redis.LoadModelHealthSweep(ctx); err == nil {
			return st
		}
	}
	s.localSweepMu.Lock()
	defer s.localSweepMu.Unlock()
	cov := make(map[string]struct{}, len(s.localSweepCovered))
	for k := range s.localSweepCovered {
		cov[k] = struct{}{}
	}
	return redis.SweepState{
		Generation: s.localSweepGen,
		StartedAt:  s.localSweepStart,
		Covered:    cov,
		CoveredN:   len(cov),
	}
}

func (s *Service) startSweep(ctx context.Context) redis.SweepState {
	if s.Redis != nil && s.Redis.Enabled() {
		if st, err := s.Redis.StartModelHealthSweep(ctx, s.sweepTTLSec()); err == nil {
			// Mirror locally for Status() without Redis round-trip.
			s.localSweepMu.Lock()
			s.localSweepGen = st.Generation
			s.localSweepStart = st.StartedAt
			s.localSweepCovered = map[string]struct{}{}
			s.localSweepMu.Unlock()
			return st
		}
	}
	now := time.Now()
	s.localSweepMu.Lock()
	defer s.localSweepMu.Unlock()
	s.localSweepGen = now.Unix()
	s.localSweepStart = float64(now.Unix())
	s.localSweepCovered = map[string]struct{}{}
	return redis.SweepState{
		Generation: s.localSweepGen,
		StartedAt:  s.localSweepStart,
		Covered:    map[string]struct{}{},
		CoveredN:   0,
	}
}

func (s *Service) markCovered(ctx context.Context, ids []string) int {
	if len(ids) == 0 {
		st := s.loadSweep(ctx)
		return st.CoveredN
	}
	if s.Redis != nil && s.Redis.Enabled() {
		if n, err := s.Redis.MarkModelHealthCovered(ctx, ids, s.sweepTTLSec()); err == nil {
			// Mirror local
			s.localSweepMu.Lock()
			if s.localSweepCovered == nil {
				s.localSweepCovered = map[string]struct{}{}
			}
			for _, id := range ids {
				s.localSweepCovered[id] = struct{}{}
			}
			s.localSweepMu.Unlock()
			return int(n)
		}
	}
	s.localSweepMu.Lock()
	defer s.localSweepMu.Unlock()
	if s.localSweepCovered == nil {
		s.localSweepCovered = map[string]struct{}{}
	}
	for _, id := range ids {
		s.localSweepCovered[id] = struct{}{}
	}
	return len(s.localSweepCovered)
}

func (s *Service) sweepSnapshot(liveN int) (covered int, gen int64, mode string) {
	mode = "strict_sweep"
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	st := s.loadSweep(ctx)
	covered = st.CoveredN
	gen = st.Generation
	if liveN > 0 && covered > liveN {
		covered = liveN
	}
	return covered, gen, mode
}

func (s *Service) sweepTTLSec() int {
	// Keep at least 6h, or ~3× full estimated coverage window (Python parity).
	interval := s.Interval
	if interval <= 0 {
		interval = 15 * time.Minute
	}
	sec := int(interval.Seconds() * 36)
	if sec < defaultSweepTTLSec {
		sec = defaultSweepTTLSec
	}
	if sec < 6*3600 {
		sec = 6 * 3600
	}
	return sec
}

func intOf(v any) int {
	switch n := v.(type) {
	case int:
		return n
	case int64:
		return int(n)
	case float64:
		return int(n)
	case float32:
		return int(n)
	default:
		return 0
	}
}

func intOfMap(m map[string]any, keys ...string) int {
	var cur any = m
	for _, k := range keys {
		mm, ok := cur.(map[string]any)
		if !ok {
			return 0
		}
		cur = mm[k]
	}
	return intOf(cur)
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func cloneMap(in map[string]any) map[string]any {
	out := map[string]any{}
	if in == nil {
		return out
	}
	for k, v := range in {
		out[k] = v
	}
	return out
}

func (s *Service) writeTaskLog(ctx context.Context, source string, result map[string]any) {
	if s == nil || s.Store == nil || result == nil {
		return
	}
	// Skip pure lock-busy no-ops.
	if result["deferred_busy"] == true {
		return
	}
	probed := intOf(result["probed"])
	if probed == 0 && intOf(result["count"]) == 0 && source == "background" {
		return
	}
	okVal := result["ok"] != false
	status := "done"
	if result["ok"] == false {
		status = "error"
		okVal = false
	} else if result["budget_hit"] == true || intOf(result["deferred"]) > 0 {
		status = "partial"
	}
	summary := "模型探测[" + source + "]：可用 " + itoa(intOf(result["available_count"])) + "/" + itoa(intOf(result["count"]))
	if intOf(result["kick_cooldown"])+intOf(result["kick_disabled"]) > 0 {
		summary += " · 冷却踢出 " + itoa(intOf(result["kick_cooldown"])) + " · 禁用 " + itoa(intOf(result["kick_disabled"]))
	}
	if intOf(result["model_blocked_count"]) > 0 {
		summary += " · 模型封禁 " + itoa(intOf(result["model_blocked_count"]))
	}
	detail := map[string]any{}
	for _, k := range []string{"probed", "count", "available_count", "unavailable_count", "kick_cooldown", "kick_disabled", "model_blocked_count", "workers", "waves", "budget_hit", "models", "source", "elapsed_ms", "sweep"} {
		if v, ok := result[k]; ok {
			detail[k] = v
		}
	}
	okPtr := okVal
	_, _ = s.Store.WriteTask(ctx, "probe", status, summary, "probe:"+source, &okPtr, detail, intOf(result["available_count"]), intOf(result["count"]), true)
}

func itoa(n int) string {
	return strconv.Itoa(n)
}

func isFreeUsageExhausted(errText string) bool {
	t := strings.ToLower(errText)
	return strings.Contains(t, "free-usage-exhausted") ||
		strings.Contains(t, "free usage") ||
		strings.Contains(t, "额度耗尽") ||
		(strings.Contains(t, "quota") && strings.Contains(t, "exhaust"))
}

func firstModel(models []string) string {
	if len(models) == 0 {
		return "grok-4.5"
	}
	return models[0]
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return v
		}
	}
	return ""
}

func normalizeModels(models []string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(models))
	for _, m := range models {
		mid := strings.TrimSpace(m)
		if mid == "" {
			continue
		}
		key := strings.ToLower(mid)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, mid)
	}
	return out
}

func splitCSV(raw string) []string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	parts := strings.Split(raw, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
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
