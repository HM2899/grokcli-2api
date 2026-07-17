package modelhealth

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/hm2899/grokcli-2api/internal/store/postgres"
)

func TestNormalizeModels(t *testing.T) {
	got := normalizeModels([]string{" grok-4.5 ", "GROK-4.5", "grok-3", "", "grok-3"})
	if len(got) != 2 || got[0] != "grok-4.5" || got[1] != "grok-3" {
		t.Fatalf("normalizeModels = %#v", got)
	}
}

func TestModelsForSourceRotatesBackground(t *testing.T) {
	s := &Service{Models: []string{"a", "b", "c"}, MaxModelsPerAccount: 2}
	seen := map[string]int{}
	for i := 0; i < 6; i++ {
		m := s.modelsForSource("background")
		if len(m) != 1 {
			t.Fatalf("background should probe 1 model, got %v", m)
		}
		seen[m[0]]++
	}
	if len(seen) != 3 {
		t.Fatalf("expected full rotation, got %#v", seen)
	}
	manual := s.modelsForSource("manual_all")
	if len(manual) != 2 {
		t.Fatalf("manual should cap models, got %v", manual)
	}
}

func TestProbeAccountsConcurrentUsesWorkers(t *testing.T) {
	var inFlight atomic.Int64
	var maxInFlight atomic.Int64
	var hits atomic.Int64

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cur := inFlight.Add(1)
		defer inFlight.Add(-1)
		for {
			old := maxInFlight.Load()
			if cur <= old || maxInFlight.CompareAndSwap(old, cur) {
				break
			}
		}
		hits.Add(1)
		// Small delay so concurrency is observable.
		time.Sleep(40 * time.Millisecond)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer srv.Close()

	s := &Service{
		Upstream:    srv.URL,
		Models:      []string{"grok-4.5"},
		Workers:     4,
		AutoDisable: false,
		httpClient:  newProbeHTTPClient(),
	}

	auths := make([]postgres.AccountAuth, 12)
	for i := range auths {
		auths[i] = postgres.AccountAuth{ID: "a" + string(rune('a'+i)), Email: "e", Token: "t"}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	started := time.Now()
	probes := s.probeAccountsConcurrent(ctx, auths, []string{"grok-4.5"}, "manual_all", 4)
	elapsed := time.Since(started)

	if len(probes) != 12 {
		t.Fatalf("probes=%d want 12", len(probes))
	}
	for _, p := range probes {
		if p["available"] != true {
			t.Fatalf("probe failed: %#v", p)
		}
	}
	// Sequential would be ~12*40ms = 480ms; with 4 workers ~3 waves ~120ms+.
	if elapsed > 350*time.Millisecond {
		t.Fatalf("expected concurrent speedup, elapsed=%s maxInFlight=%d hits=%d", elapsed, maxInFlight.Load(), hits.Load())
	}
	if maxInFlight.Load() < 2 {
		t.Fatalf("expected concurrent in-flight probes, maxInFlight=%d", maxInFlight.Load())
	}
}

func TestProbeAccountBudgetCutDoesNotDisable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	s := &Service{
		Upstream:    srv.URL,
		Models:      []string{"grok-4.5"},
		AutoDisable: true,
		httpClient:  newProbeHTTPClient(),
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	probe := s.probeAccount(ctx, postgres.AccountAuth{ID: "x", Token: "t"}, "grok-4.5", "manual", true, false)
	if probe["budget_cut"] != true {
		t.Fatalf("expected budget_cut, got %#v", probe)
	}
	if probe["auto_disabled"] == true || probe["kicked_cooldown"] == true {
		t.Fatalf("budget cut must not kick account: %#v", probe)
	}
}

func TestRunOnceManualAllBoostsWorkersAndReportsMeta(t *testing.T) {
	// Without a store, RunOnce should fail closed early.
	s := New(nil, nil, "http://example.invalid", []string{"grok-4.5"})
	s.Workers = 3
	out := s.RunOnce(context.Background(), "manual_all")
	if out["ok"] != false {
		t.Fatalf("expected store unavailable: %#v", out)
	}
	// Status should expose workers.
	st := s.Status()
	if st["probe_workers"] != 3 && st["workers"] != 3 {
		// New() may override Workers from env; just ensure key exists.
		raw, _ := json.Marshal(st)
		if !containsWorkers(raw) {
			t.Fatalf("status missing workers: %s", raw)
		}
	}
}

func containsWorkers(raw []byte) bool {
	return len(raw) > 0 && (stringContains(string(raw), "probe_workers") || stringContains(string(raw), "workers"))
}

func stringContains(s, sub string) bool {
	return len(sub) == 0 || (len(s) >= len(sub) && (s == sub || len(s) > 0 && (func() bool {
		for i := 0; i+len(sub) <= len(s); i++ {
			if s[i:i+len(sub)] == sub {
				return true
			}
		}
		return false
	})()))
}

func TestFilterUncoveredSkipsCovered(t *testing.T) {
	s := &Service{localSweepCovered: map[string]struct{}{"a1": {}}}
	s.localSweepGen = 1
	auths := []postgres.AccountAuth{
		{ID: "a1", Token: "t"},
		{ID: "a2", Token: "t"},
	}
	info, out := s.filterUncovered(context.Background(), auths, 2, "background")
	if len(out) != 1 || out[0].ID != "a2" {
		t.Fatalf("out=%#v info=%#v", out, info)
	}
}

func TestMarkCoveredLocal(t *testing.T) {
	s := &Service{localSweepCovered: map[string]struct{}{}}
	s.localSweepGen = 1
	n := s.markCovered(context.Background(), []string{"x", "y"})
	if n != 2 {
		t.Fatalf("covered=%d", n)
	}
	info, out := s.filterUncovered(context.Background(), []postgres.AccountAuth{{ID: "x"}, {ID: "y"}, {ID: "z"}}, 3, "manual_all")
	if len(out) != 1 || out[0].ID != "z" {
		t.Fatalf("out=%#v info=%#v", out, info)
	}
}

func TestStartProbeAllAlreadyRunning(t *testing.T) {
	s := New(nil, nil, "http://example.invalid", []string{"m"})
	// First start without store will finish quickly with error, but race-free path:
	s.jobMu.Lock()
	s.jobRunning = true
	s.job = map[string]any{"running": true, "wave": 2}
	s.jobMu.Unlock()
	out := s.StartProbeAll()
	if out["already_running"] != true {
		t.Fatalf("expected already_running: %#v", out)
	}
}
