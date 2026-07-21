package server

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/hm2899/grokcli-2api/internal/accounts"
	"github.com/hm2899/grokcli-2api/internal/admin"
	adminauth "github.com/hm2899/grokcli-2api/internal/admin/auth"
	"github.com/hm2899/grokcli-2api/internal/auth"
	"github.com/hm2899/grokcli-2api/internal/buildinfo"
	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/integrations"
	"github.com/hm2899/grokcli-2api/internal/maintainer"
	"github.com/hm2899/grokcli-2api/internal/modelhealth"
	"github.com/hm2899/grokcli-2api/internal/models"
	"github.com/hm2899/grokcli-2api/internal/pool"
	"github.com/hm2899/grokcli-2api/internal/protocol/anthropic"
	"github.com/hm2899/grokcli-2api/internal/protocol/historycompact"
	"github.com/hm2899/grokcli-2api/internal/protocol/reasoning"
	"github.com/hm2899/grokcli-2api/internal/protocol/responses"
	"github.com/hm2899/grokcli-2api/internal/protocol/toolcall"
	"github.com/hm2899/grokcli-2api/internal/proxy"
	"github.com/hm2899/grokcli-2api/internal/quota"
	regclient "github.com/hm2899/grokcli-2api/internal/registration/client"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
)

type Options struct {
	Ready             func() bool
	Reason            func() string
	StaticDir         string
	PublicReadEnabled bool
	AdminReadEnabled  bool
	AdminWriteEnabled bool
	ChatEnabled       bool
	MessagesEnabled   bool
	ResponsesEnabled  bool
	APIKeys           *auth.APIKeyVerifier
	Models            *models.Catalog
	Store             *postgres.Connector
	// Candidates, when non-empty, is used by proxy routes instead of Store.ListPoolCandidates.
	// Intended for contract/e2e tests against a fake upstream.
	Candidates    []pool.Candidate
	AdminSessions admin.SessionVerifier
	PickObserver  proxy.PickObserver
	AffinityStore proxy.AffinityStore
	// Upstream is a shared Grok HTTP client (connection pool). Prefer this over
	// constructing a new client on every request.
	Upstream    *grok.Client
	Redis       *redis.Client
	Leader      *redis.Leader
	Maintainer  *maintainer.Service
	ModelHealth *modelhealth.Service
	Quota       *quota.Service
	Config      config.Config
	// RuntimeConfig is the live process config. When non-nil, request handlers
	// read streaming/tool policy from it so admin settings writes take effect
	// without restart. Config remains the startup snapshot.
	RuntimeConfig     *config.Config
	RegistrationURL   string
	RegistrationToken string
}

func (o Options) runtimeConfig() config.Config {
	if o.RuntimeConfig != nil {
		return *o.RuntimeConfig
	}
	return o.Config
}

func (o Options) applySettingsToRuntime(settings map[string]any) {
	if settings == nil {
		return
	}
	if o.RuntimeConfig != nil {
		o.RuntimeConfig.ApplyStoreSettings(settings)
	}
	// Hot-apply model health knobs (interval/batch/workers) without restart.
	if o.ModelHealth != nil && settings != nil {
		var intervalSec float64
		var batch, workers int
		switch v := settings["model_health_interval_sec"].(type) {
		case float64:
			intervalSec = v
		case int:
			intervalSec = float64(v)
		case int64:
			intervalSec = float64(v)
		}
		switch v := settings["model_health_probe_batch"].(type) {
		case float64:
			batch = int(v)
		case int:
			batch = v
		case int64:
			batch = int(v)
		}
		switch v := settings["model_health_probe_workers"].(type) {
		case float64:
			workers = int(v)
		case int:
			workers = v
		case int64:
			workers = int(v)
		}
		o.ModelHealth.Configure(intervalSec, batch, workers)
	}
	// History compact knobs live in historycompact package (not Config struct).
	applyHistoryCompactSettings(settings)
	// Shell projection debug logging (admin WebUI debug_shell_args).
	if v, ok := settings["debug_shell_args"].(bool); ok {
		toolcall.ConfigureDebugShellArgs(v)
	}
	// PowerShell special policy removed: Codex is auto-detected for outbound
	// cmd/workdir projection only (cache-safe; no prompt injection).
}

func applyHistoryCompactSettings(settings map[string]any) {
	if settings == nil {
		return
	}
	opts := historycompact.ConfigureOpts{}
	if v, ok := settings["history_compact_enabled"].(bool); ok {
		opts.Enabled = &v
	}
	if n, ok := asSettingsIntLocal(settings["history_compact_auto_chars"]); ok {
		opts.AutoChars = &n
	}
	if n, ok := asSettingsIntLocal(settings["history_keep_tool_rounds"]); ok {
		opts.KeepToolRounds = &n
	}
	if n, ok := asSettingsIntLocal(settings["history_max_tool_result_chars"]); ok {
		opts.MaxToolResultChars = &n
	}
	if opts.Enabled != nil || opts.AutoChars != nil || opts.KeepToolRounds != nil || opts.MaxToolResultChars != nil {
		historycompact.ConfigureFull(opts)
	}
}

func asSettingsIntLocal(value any) (int, bool) {
	switch v := value.(type) {
	case float64:
		return int(v), true
	case float32:
		return int(v), true
	case int:
		return v, true
	case int64:
		return int(v), true
	case json.Number:
		i, err := v.Int64()
		return int(i), err == nil
	case string:
		i, err := strconv.Atoi(strings.TrimSpace(v))
		return i, err == nil
	default:
		return 0, false
	}
}

// NewMigrationMux exposes migration-safe process probes plus low-risk read-only
// shells. Compatibility proxy/admin API endpoints are added only after their
// Python wire contracts are frozen.
func NewMigrationMux(ready func() bool) http.Handler {
	return NewMux(Options{Ready: ready})
}

func NewMux(options Options) http.Handler {
	mux := http.NewServeMux()
	staticDir := options.StaticDir
	if strings.TrimSpace(staticDir) == "" {
		staticDir = "static"
	}

	mux.HandleFunc("GET /live", func(w http.ResponseWriter, _ *http.Request) {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"implementation": buildinfo.Implementation,
			"version":        buildinfo.Version,
		})
	})
	mux.HandleFunc("GET /ready", func(w http.ResponseWriter, _ *http.Request) {
		if !isReady(options) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{
				"ok":             false,
				"implementation": buildinfo.Implementation,
				"version":        buildinfo.Version,
				"reason":         readyReason(options),
			})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":             true,
			"implementation": buildinfo.Implementation,
			"version":        buildinfo.Version,
		})
	})
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		status := "ok"
		if !isReady(options) {
			status = "starting"
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"status":         status,
			"implementation": buildinfo.Implementation,
			"version":        buildinfo.Version,
			"ready":          status == "ok",
		})
	})
	mux.HandleFunc("GET /metrics", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		ready := 0
		if isReady(options) {
			ready = 1
		}
		_, _ = w.Write([]byte("# HELP g2a_runtime_ready Go runtime readiness gate.\n"))
		_, _ = w.Write([]byte("# TYPE g2a_runtime_ready gauge\n"))
		_, _ = w.Write([]byte("g2a_runtime_ready{implementation=\"go\"} " + itoa(ready) + "\n"))
		_, _ = w.Write([]byte(streamMetricsPrometheus()))
	})
	// Exact root only. A bare "GET /" is a subtree pattern in Go 1.22+ and would
	// incorrectly serve index.html for every unmatched path (e.g. /unknown).
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, filepath.Join(staticDir, "index.html"), false)
	})
	mux.HandleFunc("GET /favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		serveFile(w, r, filepath.Join(staticDir, "favicon.ico"), false)
	})
	mux.HandleFunc("GET /admin", func(w http.ResponseWriter, r *http.Request) {
		serveAdminPage(w, r, staticDir, "index")
	})
	mux.HandleFunc("GET /admin/{page}", func(w http.ResponseWriter, r *http.Request) {
		serveAdminPage(w, r, staticDir, r.PathValue("page"))
	})
	mux.HandleFunc("GET /static/{file...}", func(w http.ResponseWriter, r *http.Request) {
		serveStatic(w, r, staticDir, r.PathValue("file"))
	})
	mux.HandleFunc("GET /v1/models", func(w http.ResponseWriter, r *http.Request) {
		serveModels(w, r, options)
	})
	mux.HandleFunc("GET /models", func(w http.ResponseWriter, r *http.Request) {
		serveModels(w, r, options)
	})
	mux.HandleFunc("POST /v1/chat/completions", func(w http.ResponseWriter, r *http.Request) {
		serveChatCompletions(w, r, options)
	})
	mux.HandleFunc("POST /chat/completions", func(w http.ResponseWriter, r *http.Request) {
		serveChatCompletions(w, r, options)
	})
	mux.HandleFunc("POST /v1/messages", func(w http.ResponseWriter, r *http.Request) {
		serveMessages(w, r, options)
	})
	mux.HandleFunc("POST /messages", func(w http.ResponseWriter, r *http.Request) {
		serveMessages(w, r, options)
	})
	mux.HandleFunc("POST /v1/messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		serveMessagesCountTokens(w, r, options)
	})
	mux.HandleFunc("POST /messages/count_tokens", func(w http.ResponseWriter, r *http.Request) {
		serveMessagesCountTokens(w, r, options)
	})
	mux.HandleFunc("POST /v1/responses", func(w http.ResponseWriter, r *http.Request) {
		serveResponses(w, r, options)
	})
	mux.HandleFunc("POST /responses", func(w http.ResponseWriter, r *http.Request) {
		serveResponses(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/version", func(w http.ResponseWriter, r *http.Request) {
		serveVersionInfo(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/version/check", func(w http.ResponseWriter, r *http.Request) {
		serveVersionCheck(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/version/update", func(w http.ResponseWriter, r *http.Request) {
		serveVersionUpdate(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/status", func(w http.ResponseWriter, r *http.Request) {
		serveAdminStatus(w, r, options, false)
	})
	mux.HandleFunc("GET /admin/api/dashboard", func(w http.ResponseWriter, r *http.Request) {
		serveAdminStatus(w, r, options, true)
	})
	mux.HandleFunc("GET /admin/api/models", func(w http.ResponseWriter, r *http.Request) {
		serveAdminModels(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/keys", func(w http.ResponseWriter, r *http.Request) {
		serveAdminKeys(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts", func(w http.ResponseWriter, r *http.Request) {
		serveAdminAccounts(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/settings", func(w http.ResponseWriter, r *http.Request) {
		serveAdminSettings(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/logs", func(w http.ResponseWriter, r *http.Request) {
		serveAdminLogs(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/logs/actions", func(w http.ResponseWriter, r *http.Request) {
		serveAdminLogActions(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/usage/summary", func(w http.ResponseWriter, r *http.Request) {
		serveUsageSummary(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/usage/series", func(w http.ResponseWriter, r *http.Request) {
		serveUsageSeries(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/usage/by-key", func(w http.ResponseWriter, r *http.Request) {
		serveUsageBreakdown(w, r, options, "key")
	})
	mux.HandleFunc("GET /admin/api/usage/by-account", func(w http.ResponseWriter, r *http.Request) {
		serveUsageBreakdown(w, r, options, "account")
	})
	mux.HandleFunc("GET /admin/api/usage/by-model", func(w http.ResponseWriter, r *http.Request) {
		serveUsageBreakdown(w, r, options, "model")
	})
	mux.HandleFunc("GET /admin/api/usage/events", func(w http.ResponseWriter, r *http.Request) {
		serveUsageEvents(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/setup", func(w http.ResponseWriter, r *http.Request) {
		serveAdminSetup(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/login", func(w http.ResponseWriter, r *http.Request) {
		serveAdminLogin(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/session", func(w http.ResponseWriter, r *http.Request) {
		serveAdminSession(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/logout", func(w http.ResponseWriter, r *http.Request) {
		serveAdminLogout(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/keys", func(w http.ResponseWriter, r *http.Request) {
		serveAdminCreateKey(w, r, options)
	})
	mux.HandleFunc("PATCH /admin/api/keys/{key_id}", func(w http.ResponseWriter, r *http.Request) {
		serveAdminUpdateKey(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/keys/{key_id}/regenerate", func(w http.ResponseWriter, r *http.Request) {
		serveAdminRegenerateKey(w, r, options)
	})
	mux.HandleFunc("DELETE /admin/api/keys/{key_id}", func(w http.ResponseWriter, r *http.Request) {
		serveAdminDeleteKey(w, r, options)
	})
	mux.HandleFunc("PATCH /admin/api/accounts/{account_id}/enabled", func(w http.ResponseWriter, r *http.Request) {
		serveAdminSetAccountEnabled(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/kick", func(w http.ResponseWriter, r *http.Request) {
		serveAdminKickAccount(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/cooldown/clear", func(w http.ResponseWriter, r *http.Request) {
		serveAdminClearCooldown(w, r, options)
	})
	mux.HandleFunc("PATCH /admin/api/accounts/{account_id}/status", func(w http.ResponseWriter, r *http.Request) {
		serveAdminSetAccountStatus(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/status", func(w http.ResponseWriter, r *http.Request) {
		serveAdminSetAccountStatus(w, r, options)
	})
	mux.HandleFunc("PUT /admin/api/settings", func(w http.ResponseWriter, r *http.Request) {
		serveAdminUpdateSettings(w, r, options)
	})
	mux.HandleFunc("PATCH /admin/api/settings", func(w http.ResponseWriter, r *http.Request) {
		serveAdminUpdateSettings(w, r, options)
	})
	mux.HandleFunc("PUT /admin/api/settings/runtime", func(w http.ResponseWriter, r *http.Request) {
		serveAdminUpdateSettings(w, r, options)
	})
	mux.HandleFunc("PATCH /admin/api/settings/runtime", func(w http.ResponseWriter, r *http.Request) {
		serveAdminUpdateSettings(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/register-email/availability", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationAvailability(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/register-email/config", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationConfigGet(w, r, options)
	})
	mux.HandleFunc("PUT /admin/api/accounts/register-email/config", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationConfigPut(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email/test-proxy", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationProxyTest(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/register-email/test-proxy", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationProxyTest(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/register-email/sessions", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationSessions(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/register-email/sessions/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationSession(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email/sessions/{session_id}/stop", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationStopSession(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/register-email/batches/{batch_id}", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationBatch(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email/batches/{batch_id}/stop", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationStopBatch(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email/batches/{batch_id}/resume", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationResumeBatch(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationStart(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email/reclaim", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationReclaim(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email/stop", func(w http.ResponseWriter, r *http.Request) {
		serveRegistrationStopAll(w, r, options)
	})
	// Registration SSO cookie export (sessions + durable account fallback).
	// Missing since Go migration — admin "导出 SSO" hit 404 without these routes.
	mux.HandleFunc("GET /admin/api/accounts/register-email/export-sso", func(w http.ResponseWriter, r *http.Request) {
		serveExportRegistrationSSO(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/register-email/export-sso", func(w http.ResponseWriter, r *http.Request) {
		serveExportRegistrationSSO(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/import-sso", func(w http.ResponseWriter, r *http.Request) {
		serveSSOImportStart(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/login", func(w http.ResponseWriter, r *http.Request) {
		serveDeviceLoginStart(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/login/sessions/{session_id}", func(w http.ResponseWriter, r *http.Request) {
		serveDeviceLoginSession(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/login/sessions", func(w http.ResponseWriter, r *http.Request) {
		serveDeviceLoginSessions(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/import-sso/jobs/{job_id}", func(w http.ResponseWriter, r *http.Request) {
		serveSSOImportJob(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/import", func(w http.ResponseWriter, r *http.Request) {
		serveAdminImportAccount(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/export", func(w http.ResponseWriter, r *http.Request) {
		serveAdminExportAccounts(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/export-batch", func(w http.ResponseWriter, r *http.Request) {
		serveAdminExportAccountsBatch(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/export/jobs/{job_id}", func(w http.ResponseWriter, r *http.Request) {
		serveExportJobStatus(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/export/jobs/{job_id}/download", func(w http.ResponseWriter, r *http.Request) {
		serveExportJobDownload(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/delete-batch", func(w http.ResponseWriter, r *http.Request) {
		serveAdminDeleteAccountsBatch(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/logout", func(w http.ResponseWriter, r *http.Request) {
		serveAdminClearAllAccounts(w, r, options)
	})
	mux.HandleFunc("DELETE /admin/api/accounts/{account_id}", func(w http.ResponseWriter, r *http.Request) {
		serveAdminDeleteAccount(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/{account_id}/probe", func(w http.ResponseWriter, r *http.Request) {
		serveAdminProbeAccount(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/probe-batch", func(w http.ResponseWriter, r *http.Request) {
		serveAdminProbeBatch(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/probe-all", func(w http.ResponseWriter, r *http.Request) {
		serveAdminProbeAll(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/model-health", func(w http.ResponseWriter, r *http.Request) {
		serveModelHealthStatus(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/upstream-status", func(w http.ResponseWriter, r *http.Request) {
		serveUpstreamStatus(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/maintainer", func(w http.ResponseWriter, r *http.Request) {
		serveMaintainerStatus(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/maintainer/run", func(w http.ResponseWriter, r *http.Request) {
		serveMaintainerRun(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/refresh", func(w http.ResponseWriter, r *http.Request) {
		serveAccountsRefresh(w, r, options)
	})
	mux.HandleFunc("PUT /admin/api/settings/token-maintain", func(w http.ResponseWriter, r *http.Request) {
		serveToggleTokenMaintain(w, r, options)
	})
	mux.HandleFunc("PUT /admin/api/settings/model-health", func(w http.ResponseWriter, r *http.Request) {
		serveToggleModelHealth(w, r, options)
	})
	mux.HandleFunc("PUT /admin/api/settings/account-mode", func(w http.ResponseWriter, r *http.Request) {
		serveSetAccountMode(w, r, options)
	})
	mux.HandleFunc("PUT /admin/api/settings/password", func(w http.ResponseWriter, r *http.Request) {
		serveChangeAdminPassword(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/model-blocks/prune", func(w http.ResponseWriter, r *http.Request) {
		servePruneModelBlocks(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/export-sso", func(w http.ResponseWriter, r *http.Request) {
		serveExportAccountsSSO(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/export-sso", func(w http.ResponseWriter, r *http.Request) {
		serveExportAccountsSSOSelected(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/import-file", func(w http.ResponseWriter, r *http.Request) {
		serveAdminImportFile(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/import-files", func(w http.ResponseWriter, r *http.Request) {
		serveAdminImportFiles(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/normalize", func(w http.ResponseWriter, r *http.Request) {
		serveAdminNormalizeAccounts(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/models/sync", func(w http.ResponseWriter, r *http.Request) {
		serveAdminModelsSync(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/models/fetch", func(w http.ResponseWriter, r *http.Request) {
		serveAdminModelsFetch(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/models/save", func(w http.ResponseWriter, r *http.Request) {
		serveAdminModelsSave(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/quota", func(w http.ResponseWriter, r *http.Request) {
		serveAdminAccountsQuota(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/accounts/{account_id}/quota", func(w http.ResponseWriter, r *http.Request) {
		serveAdminAccountQuota(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/settings/cliproxyapi", func(w http.ResponseWriter, r *http.Request) {
		serveIntegrationSettingsGet(w, r, options, "cliproxyapi_config")
	})
	mux.HandleFunc("PUT /admin/api/settings/cliproxyapi", func(w http.ResponseWriter, r *http.Request) {
		serveIntegrationSettingsPut(w, r, options, "cliproxyapi_config")
	})
	mux.HandleFunc("POST /admin/api/settings/cliproxyapi/test", func(w http.ResponseWriter, r *http.Request) {
		serveCLIProxyTest(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/export-cliproxyapi-format", func(w http.ResponseWriter, r *http.Request) {
		serveExportCLIProxyFormat(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/push-cliproxyapi", func(w http.ResponseWriter, r *http.Request) {
		servePushCLIProxy(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/settings/sub2api", func(w http.ResponseWriter, r *http.Request) {
		serveIntegrationSettingsGet(w, r, options, "sub2api_config")
	})
	mux.HandleFunc("PUT /admin/api/settings/sub2api", func(w http.ResponseWriter, r *http.Request) {
		serveIntegrationSettingsPut(w, r, options, "sub2api_config")
	})
	mux.HandleFunc("POST /admin/api/accounts/export-sub2api-format", func(w http.ResponseWriter, r *http.Request) {
		serveExportSub2APIFormat(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/accounts/push-sub2api", func(w http.ResponseWriter, r *http.Request) {
		servePushSub2API(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/settings/sub2api/test", func(w http.ResponseWriter, r *http.Request) {
		serveSub2APITest(w, r, options)
	})
	mux.HandleFunc("GET /admin/api/settings/sub2api/groups", func(w http.ResponseWriter, r *http.Request) {
		serveSub2APIGroupsList(w, r, options)
	})
	mux.HandleFunc("POST /admin/api/settings/sub2api/groups", func(w http.ResponseWriter, r *http.Request) {
		serveSub2APIGroupsCreate(w, r, options)
	})
	return mux
}

func serveModels(w http.ResponseWriter, r *http.Request, options Options) {
	if !options.PublicReadEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": "Go public read routes are not enabled"})
		return
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"error": readyReason(options)})
		return
	}
	if options.APIKeys != nil {
		if _, err := options.APIKeys.Require(r.Context(), r); err != nil {
			status := http.StatusInternalServerError
			message := err.Error()
			if errors.Is(err, auth.ErrInvalidAPIKey) {
				status = http.StatusUnauthorized
				message = "Invalid or missing API key"
			}
			writeJSON(w, status, map[string]any{"detail": message})
			return
		}
	}
	catalog := options.Models
	if catalog == nil {
		catalog = models.NewCatalog(config.Config{DefaultModel: "grok-4.5"}, nil)
	}
	writeJSON(w, http.StatusOK, catalog.OpenAIList(r.Context()))
}

func serveChatCompletions(w http.ResponseWriter, r *http.Request, options Options) {
	if !options.ChatEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go chat route is not enabled"})
		return
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return
	}
	var apiKey *auth.APIKeyRecord
	if options.APIKeys != nil {
		verified, err := options.APIKeys.Require(r.Context(), r)
		if err != nil {
			status := http.StatusInternalServerError
			message := err.Error()
			if errors.Is(err, auth.ErrInvalidAPIKey) {
				status = http.StatusUnauthorized
				message = "Invalid or missing API key"
			}
			writeJSON(w, status, map[string]any{"detail": message})
			return
		}
		apiKey = verified
	}
	if options.Store == nil && len(options.Candidates) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	chatReq, err := proxy.DecodeChatRequest(r.Body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	chatReq.UserAgent = r.UserAgent()

	// Codex shell schema uses "cmd"; remember preferred keys from the client tools
	// so outbound tool_calls project command→cmd (or honor pure command-only schemas).
	keys := toolcall.ShellArgKeyMap(chatReq.Raw["tools"])
	keys = ensureCodexShellCmdKeys(chatReq.Raw["tools"], keys)
	if keys == nil {
		keys = map[string]string{}
	}
	// Proxies often strip User-Agent — also detect Codex via tools schema (exec_command/cmd).
	if len(keys) == 0 && historycompact.LooksLikeCodexRequest(r.UserAgent(), chatReq.Raw["tools"], chatReq.Raw) {
		for _, name := range []string{"exec_command", "run_command", "shell", "bash", "local_shell", "shell_command", "Shell", "Bash", "localShell"} {
			keys[name] = "cmd"
			keys[strings.ToLower(name)] = "cmd"
			if nk := toolcall.NameKey(name); nk != "" {
				keys[nk] = "cmd"
			}
		}
	}
	if len(keys) > 0 {
		r = r.WithContext(withShellArgKeys(r.Context(), keys))
	}
	// Client-registered tools for Update/StrReplace → Edit remap on chat path.
	if allowed := allowedAnthropicToolNames(chatReq.Raw); len(allowed) > 0 {
		r = r.WithContext(withAllowedTools(r.Context(), allowed))
	}
	// Mint/recover a stable prompt_cache_key before affinity lookup so multi-turn
	// OpenAI chat sticks to one account (prefix cache). Without this, clients that
	// omit pck fall back to per-turn fingerprints and hop accounts → cache miss.
	pck := ensureOpenAIChatPromptCacheKey(r, &chatReq)
	candidates, err := listCandidatesForRequest(r.Context(), options, chatReq, r.Header)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	service := newChatService(options)
	// Always sticky-first for multi-turn cache warmth (config default is on; force when
	// we have any stable key so least_used never races ahead of a pinned account).
	if pck != "" || proxy.ChatFingerprint(chatReq) != "" {
		service.StickyFirstOnly = true
	}
	started := time.Now()
	if chatReq.Stream {
		// Match serveMessages: OpenStream already failovers inside one call, but
		// open-time empty storms still benefit from a fresh candidate window (exclude
		// just-failed accounts) before HTTP 200 is committed by streamChatCompletions.
		var (
			opened      proxy.StreamOpen
			err         error
			stats       proxy.StreamStats
			lastAccount string
			lastModel   = chatReq.Model
			exclude     = map[string]struct{}{}
		)
		maxOpenAttempts := 4
		candPool := candidates
		for attempt := 0; attempt < maxOpenAttempts; attempt++ {
			if attempt > 0 && lastAccount != "" {
				exclude[lastAccount] = struct{}{}
				filtered := make([]pool.Candidate, 0, len(candPool))
				for _, c := range candPool {
					if _, bad := exclude[c.ID]; bad {
						continue
					}
					filtered = append(filtered, c)
				}
				if len(filtered) == 0 {
					break
				}
				candPool = filtered
			}
			opened, err = service.OpenStreamWithResult(r.Context(), chatReq, candPool, resolvePickMode(options))
			openElapsed := time.Since(started)
			if err != nil {
				lastAccount = strings.TrimSpace(opened.AccountID)
				if lastAccount == "" {
					recordChatUsage(r, options, apiKey, opened.AccountID, chatReq.Model, chatReq.Stream, false, http.StatusBadGateway, started, nil, err, 0, chatReq.Raw)
					writeProxyError(w, err)
					return
				}
				reportChatPool(r, options, lastAccount, false, err, http.StatusBadGateway, lastModel)
				if attempt+1 >= maxOpenAttempts || !isRetryableUpstreamOpenErr(err) {
					recordChatUsage(r, options, apiKey, lastAccount, chatReq.Model, chatReq.Stream, false, http.StatusBadGateway, started, nil, err, 0, chatReq.Raw)
					writeProxyError(w, err)
					return
				}
				continue
			}
			lastAccount = opened.AccountID
			lastModel = opened.Model
			req := r
			if options.runtimeConfig().SSEKeepalive > 0 {
				req = r.WithContext(withAnthropicKeepalive(r.Context(), options.runtimeConfig().SSEKeepalive))
			}
			setProtocolObservationHeaders(w, protocolObservation{
				Protocol: "openai_chat", AccountID: opened.AccountID, PreferAccount: opened.PreferAccount,
				Failover: opened.Failover || attempt > 0, Fingerprint: opened.Fingerprint, Accounts: opened.Accounts, Prep: opened.Prep,
			})
			if pck != "" {
				w.Header().Set("X-Grok2API-Prompt-Cache-Key", pck)
			}
			stats, err = streamChatCompletions(w, req, opened.Body, optionsFromRequest(req).Keepalive)
			_ = opened.Body.Close()
			releaseServerPick(options, opened.AccountID)
			// Wall-clock TTFT: open/pick + first real payload (stream clock starts after headers).
			if stats.FirstTokenMS > 0 {
				wall := int(openElapsed.Milliseconds()) + stats.FirstTokenMS
				if wall <= 0 {
					wall = 1
				}
				stats.FirstTokenMS = wall
			}
			ok := err == nil || errors.Is(err, r.Context().Err())
			// Headers commit on first stream write; only rotate when nothing was delivered.
			if !ok && stats.FirstTokenMS == 0 && isRetryableUpstreamOpenErr(err) && attempt+1 < maxOpenAttempts {
				reportChatPool(r, options, opened.AccountID, false, err, http.StatusBadGateway, opened.Model)
				lastAccount = opened.AccountID
				lastModel = opened.Model
				continue
			}
			status := http.StatusOK
			if !ok {
				status = http.StatusBadGateway
			}
			recordChatUsageWithHint(r, options, apiKey, opened.AccountID, opened.Model, chatReq.Stream, ok, status, started, stats.Usage, err, stats.FirstTokenMS, chatReq.Raw, stats.CompletionHint)
			reportChatPool(r, options, opened.AccountID, ok, err, status, opened.Model)
			return
		}
		if err == nil {
			err = errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		}
		recordChatUsage(r, options, apiKey, lastAccount, chatReq.Model, true, false, http.StatusBadGateway, started, nil, err, 0, chatReq.Raw)
		writeProxyError(w, err)
		return
	}
	result, err := service.CompleteWithResult(r.Context(), chatReq, candidates, resolvePickMode(options))
	if result.AccountID != "" {
		defer releaseServerPick(options, result.AccountID)
	}
	if err != nil {
		// Failures already reported per-account via FailureReporter.
		recordChatUsage(r, options, apiKey, result.AccountID, chatReq.Model, chatReq.Stream, false, http.StatusBadGateway, started, nil, err, 0, chatReq.Raw)
		writeProxyError(w, err)
		return
	}
	// Project shell args for non-stream chat completions (Codex cmd / OpenAI command).
	if result.Payload != nil && len(keys) > 0 {
		projectChatPayloadShellArgs(result.Payload, keys)
	} else if result.Payload != nil {
		// Default shell-family tools to cmd even without an explicit map.
		projectChatPayloadShellArgs(result.Payload, nil)
	}
	recordChatUsage(r, options, apiKey, result.AccountID, result.Model, chatReq.Stream, true, http.StatusOK, started, result.Usage, nil, 0, chatReq.Raw)
	reportChatPool(r, options, result.AccountID, true, nil, http.StatusOK, result.Model)
	setProtocolObservationHeaders(w, protocolObservation{
		Protocol: "openai_chat", AccountID: result.AccountID, PreferAccount: result.PreferAccount,
		Failover: result.Failover, Fingerprint: result.Fingerprint, Accounts: result.Accounts, Prep: result.Prep,
	})
	if pck != "" {
		w.Header().Set("X-Grok2API-Prompt-Cache-Key", pck)
		if result.Payload != nil {
			if _, has := result.Payload["prompt_cache_key"]; !has {
				result.Payload["prompt_cache_key"] = pck
			}
		}
	}
	writeJSON(w, http.StatusOK, result.Payload)
}

// projectChatPayloadShellArgs rewrites shell tool_calls / function_call arguments
// in a non-stream chat.completion payload to the client preferred key (cmd/command).
func projectChatPayloadShellArgs(payload map[string]any, keys map[string]string) {
	if payload == nil {
		return
	}
	choices, _ := payload["choices"].([]any)
	if choices == nil {
		// collector uses []map[string]any
		if typed, ok := payload["choices"].([]map[string]any); ok {
			for _, ch := range typed {
				projectChatChoiceShellArgs(ch, keys)
			}
		}
		return
	}
	for _, item := range choices {
		ch, _ := item.(map[string]any)
		projectChatChoiceShellArgs(ch, keys)
	}
}

func projectChatChoiceShellArgs(choice map[string]any, keys map[string]string) {
	if choice == nil {
		return
	}
	msg, _ := choice["message"].(map[string]any)
	if msg == nil {
		return
	}
	if calls, ok := msg["tool_calls"].([]any); ok {
		for _, raw := range calls {
			call, _ := raw.(map[string]any)
			projectOneChatToolCall(call, keys)
		}
	} else if typed, ok := msg["tool_calls"].([]map[string]any); ok {
		for _, call := range typed {
			projectOneChatToolCall(call, keys)
		}
	}
	if fn, ok := msg["function_call"].(map[string]any); ok && fn != nil {
		name := strings.TrimSpace(fmt.Sprint(fn["name"]))
		args := strings.TrimSpace(fmt.Sprint(fn["arguments"]))
		if name != "" && args != "" && toolcall.IsShellTool(name) {
			pref := toolcall.DefaultShellArgKey(name)
			if keys != nil {
				if v := strings.TrimSpace(keys[name]); v != "" {
					pref = v
				} else if v := strings.TrimSpace(keys[strings.ToLower(name)]); v != "" {
					pref = v
				} else if nk := toolcall.NameKey(name); nk != "" {
					if v := strings.TrimSpace(keys[nk]); v != "" {
						pref = v
					}
				}
			}
			fn["arguments"] = toolcall.ProjectShellArgsForClient(args, name, pref)
		}
	}
}

func projectOneChatToolCall(call map[string]any, keys map[string]string) {
	if call == nil {
		return
	}
	fn, _ := call["function"].(map[string]any)
	if fn == nil {
		return
	}
	name := strings.TrimSpace(fmt.Sprint(fn["name"]))
	args := strings.TrimSpace(fmt.Sprint(fn["arguments"]))
	if name == "" || args == "" || !toolcall.IsShellTool(name) {
		return
	}
	pref := toolcall.DefaultShellArgKey(name)
	if keys != nil {
		if v := strings.TrimSpace(keys[name]); v != "" {
			pref = v
		} else if v := strings.TrimSpace(keys[strings.ToLower(name)]); v != "" {
			pref = v
		} else if nk := toolcall.NameKey(name); nk != "" {
			if v := strings.TrimSpace(keys[nk]); v != "" {
				pref = v
			}
		}
	}
	fn["arguments"] = toolcall.ProjectShellArgsForClient(args, name, pref)
}

func streamChatCompletions(w http.ResponseWriter, r *http.Request, body io.Reader, keepalive time.Duration) (proxy.StreamStats, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "streaming is not supported by this response writer"})
		return proxy.StreamStats{}, errors.New("streaming is not supported by this response writer")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	// Delay WriteHeader until first real SSE write so serveChatCompletions multi-open
	// can rotate accounts on open-time empty without committing HTTP 200.
	headersWritten := false
	ensureHeaders := func() {
		if headersWritten {
			return
		}
		w.WriteHeader(http.StatusOK)
		headersWritten = true
	}
	var stats proxy.StreamStats
	started := time.Now()
	// Assemble tool_calls across chunks so we can emit normalized args once complete
	// (alias rewrite + drop incomplete tools) without breaking partial JSON streams.
	assembler := proxy.NewChatToolStreamAssembler()
	// Project shell args to client schema (Codex: cmd) using keys from serveChatCompletions.
	assembler.SetShellArgKeys(shellArgKeysFrom(r.Context()))
	// Remap Grok Update/StrReplace → client Edit when tools are known.
	assembler.SetAllowedTools(allowedToolNamesFrom(r.Context()))
	// Soft client write/ctx blips must NOT abort ReadSSE: aborting drains the
	// remaining upstream tool-arg frames, force-finishes with half args, and
	// Claude Code surfaces that as intermittent "Tool use interrupted".
	sw := newSSEWriter(w, flusher, r.Context())
	// Coalesce pure text/reasoning passthrough frames to cut Flush storms.
	pending := make([]byte, 0, 1024)
	firstPayloadFlushed := false
	// Clear pending only after full delivery (LastOK). Soft short-writes advance by
	// LastWritten so the accepted prefix is not re-sent (duplicate text) and the
	// remainder stays buffered for drain / force retry (incomplete text fix).
	flushPending := func(force bool) error {
		if len(pending) == 0 {
			return nil
		}
		attempts := 1
		if force {
			attempts = 3
		}
		var lastErr error
		for i := 0; i < attempts && len(pending) > 0; i++ {
			if i > 0 {
				time.Sleep(time.Duration(i) * 2 * time.Millisecond)
			}
			payload := pending
			streamCoalesceFlush.Add(1)
			ensureHeaders()
			lastErr = sw.WriteBytes(payload, force || sw.SoftGone())
			if lastErr != nil {
				return lastErr
			}
			if sw.LastOK() {
				pending = pending[:0]
				firstPayloadFlushed = true
				return nil
			}
			// Soft fail: drop only the bytes that landed; keep the tail.
			if n := sw.LastWritten(); n > 0 {
				if n >= len(pending) {
					pending = pending[:0]
				} else {
					pending = append(pending[:0], pending[n:]...)
				}
				if len(pending) == 0 {
					firstPayloadFlushed = true
					return nil
				}
			}
		}
		return nil
	}
	writeJSONFrame := func(payload map[string]any, force bool) error {
		if err := flushPending(true); err != nil {
			return err
		}
		encoded, err := json.Marshal(payload)
		if err != nil {
			return err
		}
		frame := make([]byte, 0, len(encoded)+8)
		frame = append(frame, "data: "...)
		frame = append(frame, encoded...)
		frame = append(frame, '\n', '\n')
		ensureHeaders()
		if err := sw.WriteBytes(frame, force); err != nil {
			return err
		}
		if sw.LastOK() {
			assembler.AckPayload(string(encoded))
		} else if assembler.HasUnacked() {
			assembler.RequeueUnacked()
		}
		return nil
	}
	terminalFlushed := false
	// Real model payload only (text/reasoning/tools). Role-only / empty / usage-only
	// SSE must NOT count — that was an intermittent "soft-success empty" path when
	// upstream dropped after hollow frames (FirstTokenMS>0 with no client content).
	sawRealPayload := false
	sawFinishReason := false
	streamID := ""
	streamModel := ""
	// Track streamed output size so admin usage can estimate completion_tokens when
	// xAI omits the final usage frame (admin would otherwise show completion=0).
	outputRunes := 0
	noteOutput := func(s string) {
		if s == "" {
			return
		}
		outputRunes += len([]rune(s))
	}
	flushAssemblerTerminal := func() error {
		_ = flushPending(true)
		if terminalFlushed && !assembler.NeedsFinishRetry() {
			return nil
		}
		// End-of-stream / soft disconnect: force-finish incomplete tools and always
		// emit a finish_reason frame so clients do not hang as "Tool use interrupted"
		// or incomplete mid-response (text-only turns included).
		for attempt := 0; attempt < 5; attempt++ {
			terminalFlushed = true
			for _, frame := range assembler.Finish() {
				if err := writeJSONFrame(frame, true); err != nil {
					return err
				}
			}
			if frame := assembler.FinishReasonFrame(); frame != nil {
				if err := writeJSONFrame(frame, true); err != nil {
					return err
				}
				sawFinishReason = true
			} else if !sawFinishReason {
				// Text-only soft terminal (or tools all dropped): still emit finish_reason
				// so OpenAI clients / Claude Code leave "running" cleanly.
				id := streamID
				if id == "" {
					id = "chatcmpl-go-stream"
				}
				model := streamModel
				if model == "" {
					model = "grok-4.5"
				}
				term := map[string]any{
					"id":      id,
					"object":  "chat.completion.chunk",
					"created": time.Now().Unix(),
					"model":   model,
					"choices": []any{
						map[string]any{"index": 0, "delta": map[string]any{}, "finish_reason": "stop"},
					},
				}
				if err := writeJSONFrame(term, true); err != nil {
					return err
				}
				sawFinishReason = true
			}
			if !assembler.NeedsFinishRetry() {
				break
			}
		}
		ensureHeaders()
		return sw.WriteBytes([]byte("data: [DONE]\n\n"), true)
	}
	err := grok.ReadSSEWithIdle(body, keepalive, func(event grok.Event) error {
		if event.Done {
			// No client-visible payload → leave headers uncommitted for multi-open.
			if !sawRealPayload && !assembler.Holding() && !assembler.EmittedAny() {
				return nil
			}
			return flushAssemblerTerminal()
		}
		delta, err := proxy.ParseChatDelta(event.Data)
		if err == nil && delta.Usage != nil {
			// Merge partial usage frames — final frame often has completion only.
			if prev, ok := stats.Usage.(map[string]any); ok {
				if next, ok2 := delta.Usage.(map[string]any); ok2 {
					stats.Usage = mergeUsageMaps(prev, next)
				} else {
					stats.Usage = delta.Usage
				}
			} else {
				stats.Usage = delta.Usage
			}
		}
		if err == nil {
			if delta.ID != "" {
				streamID = delta.ID
			}
			if delta.Model != "" {
				streamModel = delta.Model
			}
			hasContent := strings.TrimSpace(delta.Content) != "" || strings.TrimSpace(delta.Reasoning) != ""
			hasTools := len(delta.ToolCalls) > 0 || delta.FunctionCall != nil
			if hasContent || hasTools {
				sawRealPayload = true
				// Accumulate streamed text/reasoning/tools for completion estimate.
				noteOutput(delta.Content)
				noteOutput(delta.Reasoning)
				for _, call := range delta.ToolCalls {
					if fn, ok := call["function"].(map[string]any); ok {
						noteOutput(proxyStringAny(fn["name"]))
						noteOutput(proxyStringAny(fn["arguments"]))
					}
				}
				if delta.FunctionCall != nil {
					noteOutput(proxyStringAny(delta.FunctionCall["name"]))
					noteOutput(proxyStringAny(delta.FunctionCall["arguments"]))
				}
				// TTFT = first real model payload, not hollow role/usage SSE.
				if stats.FirstTokenMS == 0 {
					stats.FirstTokenMS = int(time.Since(started).Milliseconds())
					if stats.FirstTokenMS <= 0 {
						stats.FirstTokenMS = 1
					}
				}
			}
			if delta.FinishReason != nil {
				sawFinishReason = true
			}
		}
		// Defer client writes until first real model payload so multi-open can still
		// rotate on hollow/empty streams (headers not committed).
		if err == nil && !sawRealPayload && !(len(delta.ToolCalls) > 0 || delta.FunctionCall != nil || assembler.Holding()) {
			return nil
		}
		// When tool deltas present (or assembler is already buffering tools), rewrite
		// through assembler; pure text/finish frames passthrough with coalesce.
		if err == nil && (len(delta.ToolCalls) > 0 || delta.FunctionCall != nil || assembler.Holding()) {
			frames, passthrough := assembler.Feed(event.Data, delta)
			if !passthrough {
				// Once tools are in flight, force writes so a soft disconnect still
				// delivers complete tool_calls + finish_reason rather than half frames.
				writeForce := assembler.Holding() || assembler.EmittedAny() || sw.SoftGone()
				for _, frame := range frames {
					if err := writeJSONFrame(frame, writeForce); err != nil {
						return err
					}
				}
				return nil
			}
		}
		// Coalesce pure text passthrough: first token immediate (TTFT), then batch
		// until textCoalesceMax / tool / idle / finish.
		frame := make([]byte, 0, len(event.Data)+8)
		frame = append(frame, "data: "...)
		frame = append(frame, event.Data...)
		frame = append(frame, '\n', '\n')
		// Force first real payload for TTFT; coalesce subsequent pure text.
		canCoalesce := firstPayloadFlushed && !assembler.Holding() && !assembler.EmittedAny() && !sw.SoftGone()
		if canCoalesce {
			pending = append(pending, frame...)
			if len(pending) >= textCoalesceMax {
				return flushPending(false)
			}
			return nil
		}
		if err := flushPending(true); err != nil {
			return err
		}
		ensureHeaders()
		if err := sw.WriteBytes(frame, true); err != nil {
			return err
		}
		if sw.LastOK() {
			if sawRealPayload {
				firstPayloadFlushed = true
			}
		} else if n := sw.LastWritten(); n > 0 && n < len(frame) {
			// Soft short-write of a forced frame: keep the unsent tail for drain.
			pending = append(pending, frame[n:]...)
			if sawRealPayload {
				firstPayloadFlushed = true
			}
		} else if !sw.LastOK() && sw.LastWritten() == 0 {
			// Soft fail with nothing landed: re-buffer whole frame for drain.
			pending = append(pending, frame...)
		}
		return nil
	}, func() error {
		_ = flushPending(true)
		// Do not write keepalive before first payload — would auto-commit HTTP 200
		// and block multi-open empty failover.
		if !headersWritten {
			return nil
		}
		select {
		case <-r.Context().Done():
			sw.MarkSoftGone()
			return sw.Keepalive(": keepalive\n\n", DefaultKeepaliveInterval, true)
		default:
		}
		return sw.Keepalive(": keepalive\n\n", DefaultKeepaliveInterval, false)
	})
	// Soft disconnect / write abort / mid-stream upstream drop after tools or
	// content started: force-finish so clients do not hang, and avoid a second
	// error JSON that Claude Code reports as "Server error mid-response".
	// Drain coalesced text with retries — soft-fail must not drop pending bytes.
	for drain := 0; drain < 3 && len(pending) > 0; drain++ {
		_ = flushPending(true)
		if sw.LastOK() && len(pending) == 0 {
			break
		}
	}
	hasStreamPayload := assembler.Holding() || assembler.EmittedAny() || sawRealPayload
	clientSoft := sw.SoftGone() || errors.Is(err, r.Context().Err()) || isSoftClientWriteError(err)
	upstreamMid := err != nil && !clientSoft
	if hasStreamPayload {
		if err == nil || clientSoft || upstreamMid {
			_ = flushAssemblerTerminal()
			if clientSoft || upstreamMid {
				// Soft-close after payload: still attach completion estimate.
				if outputRunes > 0 {
					tok := outputRunes / 4
					if tok < 1 {
						tok = 1
					}
					stats.CompletionHint = tok
				} else {
					stats.CompletionHint = 1
				}
				return stats, nil
			}
		}
	}
	if !hasStreamPayload && !clientSoft {
		// Hollow / empty / mid-drop with no client-visible content: treat as empty
		// model output so multi-open can rotate and pool classifiers soft-block.
		// Prefer empty classification over raw EOF so multi-open / model soft-block fire.
		empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		if !headersWritten {
			return stats, empty
		}
		msg, errType := openAIErrorFromCause(empty)
		encoded, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": errType}})
		ensureHeaders()
		_ = sw.WriteBytes(append(append([]byte("data: "), encoded...), '\n', '\n'), true)
		_ = sw.WriteBytes([]byte("data: [DONE]\n\n"), true)
		return stats, empty
	}
	if err != nil && !errors.Is(err, r.Context().Err()) && !hasStreamPayload {
		msg, errType := openAIErrorFromCause(err)
		encoded, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": errType}})
		ensureHeaders()
		_ = sw.WriteBytes(append(append([]byte("data: "), encoded...), '\n', '\n'), true)
		_ = sw.WriteBytes([]byte("data: [DONE]\n\n"), true)
		return stats, err
	}
	return stats, err
}

func releaseServerPick(options Options, accountID string) {
	if options.PickObserver == nil || accountID == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	options.PickObserver.ReleasePick(ctx, accountID)
}

func proxyStringAny(v any) string {
	s, _ := v.(string)
	return s
}

func recordChatUsage(r *http.Request, options Options, apiKey *auth.APIKeyRecord, accountID, model string, stream bool, ok bool, status int, started time.Time, usage any, cause error, ttftMS int, requestBody map[string]any) {
	recordChatUsageWithHint(r, options, apiKey, accountID, model, stream, ok, status, started, usage, cause, ttftMS, requestBody, 0)
}

// recordChatUsageWithHint is like recordChatUsage but accepts a streamer-derived
// completion token estimate when upstream omits the final usage frame.
func recordChatUsageWithHint(r *http.Request, options Options, apiKey *auth.APIKeyRecord, accountID, model string, stream bool, ok bool, status int, started time.Time, usage any, cause error, ttftMS int, requestBody map[string]any, completionHint int) {
	usageMap, _ := usage.(map[string]any)
	// Soft-close / omitted upstream usage: fill from request body + streamer hint
	// so admin does not show ok=true with completion_tokens=0 after a real stream.
	hint := completionHint
	if hint < 0 {
		hint = 0
	}
	filled, flags := fillMissingUsage(usageMap, requestBody, hint)
	// Only apply fill on successful deliveries; failures stay zero unless upstream sent real usage.
	if ok {
		usageMap = filled
		_, c, _, _, _, _ := postgres.UsageFromOpenAI(usageMap)
		// Real stream (TTFT>0) with still-zero completion: force at least 1 so
		// usage rows match what the client actually received. Prefer streamer hint.
		if c == 0 && ttftMS > 0 {
			forceHint := hint
			if forceHint < 1 {
				forceHint = 1
			}
			filled2, flags2 := fillMissingUsage(usageMap, requestBody, forceHint)
			usageMap = filled2
			flags = flags2
			flags.EstimatedCompletion = true
			flags.Missing = true
		}
	} else if usageMap == nil {
		usageMap = map[string]any{}
		flags = usageFillFlags{}
	}
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usageMap)
	streamValue := stream
	var apiKeyID string
	if apiKey != nil {
		apiKeyID = apiKey.ID
	}
	var errText string
	if cause != nil {
		errText = cause.Error()
	}
	latency := int(time.Since(started).Milliseconds())
	var ttftPtr *int
	if ttftMS > 0 {
		v := ttftMS
		ttftPtr = &v
	}
	detail := usageDetail("go_chat", requestBody, ttftMS, latency)
	if completionHint > 0 {
		detail["streamer_estimate_tokens"] = completionHint
		// Ensure admin surfaces completion as estimated when only streamer/hint filled it.
		if flags.EstimatedCompletion || flags.Missing {
			flags.EstimatedCompletion = true
		}
	}
	if ok {
		flags.apply(detail)
	}
	// Fire-and-forget with longer timeout - usage recording should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if options.Store != nil {
			if _, _, err := options.Store.RecordUsage(ctx, postgres.UsageRecord{
				RequestID:           requestID(r),
				Implementation:      "go",
				APIKeyID:            apiKeyID,
				AccountID:           accountID,
				Model:               model,
				Protocol:            "openai_chat",
				Path:                r.URL.Path,
				Stream:              &streamValue,
				OK:                  ok,
				PromptTokens:        prompt,
				CompletionTokens:    completion,
				TotalTokens:         total,
				CacheReadTokens:     cacheRead,
				CacheCreationTokens: cacheCreate,
				ReasoningTokens:     reasoning,
				ClientIP:            clientIP(r),
				UserAgent:           r.UserAgent(),
				StatusCode:          &status,
				LatencyMS:           &latency,
				TTFTMS:              ttftPtr,
				Error:               errText,
				Detail:              detail,
			}); err != nil {
				slog.Warn("record usage failed", "error", err, "account_id", accountID, "model", model, "ok", ok)
			}
		}
		recordRedisUsage(options, apiKeyID, accountID, model, prompt, completion, total, cacheRead, ok)
	}()
}

func reportChatPool(r *http.Request, options Options, accountID string, ok bool, cause error, status int, requestedModel ...string) {
	if strings.TrimSpace(accountID) == "" {
		return
	}
	modelHint := ""
	if len(requestedModel) > 0 {
		modelHint = strings.TrimSpace(requestedModel[0])
	}
	// Fire-and-forget pool reporting - should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ok {
			if options.Store != nil {
				// preserve still-active free-usage windows, but heal stale markers.
				_ = options.Store.ReportPoolSuccess(ctx, accountID, false) /* success clears sticky cool */
			}
			touchRedisPool(options, accountID, true, "", nil, status)
			return
		}
		var errText string
		if cause != nil {
			errText = cause.Error()
		}
		// Prefer upstream status/body when present (failover wraps).
		var upstream *grok.UpstreamError
		if errors.As(cause, &upstream) && upstream != nil {
			if upstream.Status > 0 {
				status = upstream.Status
			}
			if strings.TrimSpace(upstream.Body) != "" {
				errText = upstream.Body
			}
		}
		// Text-first classifier: free-usage / 额度用完 / rate-limit wording decides
		// whether the account enters the cooldown pool (and optionally soft-blocks a model).
		decision := pool.ClassifyUpstreamFailure(status, errText, modelHint)
		var cooldown *time.Time
		if decision.ShouldCooldown {
			cooldown = decision.Until
		}
		// Prefer body-extracted model, fall back to requested model for soft-block.
		coolModel := strings.TrimSpace(decision.Model)
		if coolModel == "" {
			coolModel = modelHint
		}
		failure := postgres.PoolFailure{
			AccountID:      accountID,
			Error:          errText,
			StatusCode:     &status,
			CooldownUntil:  cooldown,
			CooldownReason: firstNonEmptyStr(decision.Reason, errText),
			CooldownCode:   decision.Code,
			CooldownModel:  coolModel,
			Detail: map[string]any{
				"source":           "go_chat",
				"failure_class":    string(decision.Class),
				"should_cooldown":  decision.ShouldCooldown,
				"requested_model":  modelHint,
				"classified_model": decision.Model,
			},
		}
		if decision.TokensActual != nil {
			failure.CooldownTokensActual = decision.TokensActual
		}
		if decision.TokensLimit != nil {
			failure.CooldownTokensLimit = decision.TokensLimit
		}
		// Soft-block model when classifier sets BlockModel (empty model output → 模型封禁).
		// BlockUntil uses decision.Until even when ShouldCooldown is false so
		// empty-upstream rows land in model_blocked without sticky account cool.
		if decision.BlockModel && coolModel != "" {
			until := decision.Until
			if until == nil {
				t := time.Now().Add(10 * time.Minute)
				until = &t
			}
			failure.BlockedModel = coolModel
			failure.BlockedUntil = until
		}
		if options.Store != nil {
			// Only record durable failure/cooldown when classifier says so.
			// Still bump fail stats for non-cooldown errors without setting until.
			_ = options.Store.ReportPoolFailure(ctx, failure)
			// Drop hot candidate cache so next pick skips just-cooled / model-blocked accounts.
			if decision.ShouldCooldown || decision.BlockModel {
				postgres.InvalidatePoolCandidateCache()
			}
		}
		touchRedisPool(options, accountID, false, errText, cooldown, status)
	}()
}

// chatFailureCooldown is kept as a thin wrapper around pool.ClassifyUpstreamFailure
// for unit tests and call sites that still use the old signature.
func chatFailureCooldown(status int, errText string, requestedModel ...string) (until *time.Time, code, model string, tokensActual, tokensLimit *int64) {
	d := pool.ClassifyUpstreamFailure(status, errText, requestedModel...)
	if !d.ShouldCooldown {
		return nil, d.Code, d.Model, d.TokensActual, d.TokensLimit
	}
	return d.Until, d.Code, d.Model, d.TokensActual, d.TokensLimit
}

// chatPoolFailureReporter implements proxy.AccountFailureReporter so intermediate
// failover losers (quota-exhausted accounts) still enter the cooldown pool even
// when a later account eventually serves the request.
type chatPoolFailureReporter struct {
	options Options
}

func (r chatPoolFailureReporter) ReportAccountFailure(accountID, model string, err error) {
	if err == nil || strings.TrimSpace(accountID) == "" {
		return
	}
	status := http.StatusBadGateway
	reportChatPool(nil, r.options, accountID, false, err, status, model)
}

func newChatService(options Options) proxy.ChatService {
	return proxy.ChatService{
		Catalog:               modelCatalog(options),
		Client:                upstreamClient(options),
		PickObserver:          options.PickObserver,
		AffinityStore:         options.AffinityStore,
		FailureReporter:       chatPoolFailureReporter{options: options},
		StickyFirstOnly:       options.runtimeConfig().StickyFirstOnly,
		FirstByteProbeWorkers: options.runtimeConfig().FirstByteProbeWorkers,
	}
}

func extractGrokModelFromError(errText string) string {
	return pool.ExtractModelName(errText)
}

func parseTokenPair(errText string) (actual, limit int64, ok bool) {
	return pool.ParseTokenPair(errText)
}

func requestID(r *http.Request) string {
	for _, name := range []string{"X-Request-ID", "X-Correlation-ID", "X-Client-Request-ID"} {
		if value := strings.TrimSpace(r.Header.Get(name)); value != "" {
			return value
		}
	}
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "go-" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "go-" + hex.EncodeToString(buf)
}

func clientIP(r *http.Request) string {
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-For")); forwarded != "" {
		return strings.TrimSpace(strings.Split(forwarded, ",")[0])
	}
	if realIP := strings.TrimSpace(r.Header.Get("X-Real-IP")); realIP != "" {
		return realIP
	}
	return r.RemoteAddr
}

func writeProxyError(w http.ResponseWriter, err error) {
	// Keep OpenAI chat /v1/chat/completions error shape for this route.
	writeOpenAIProxyError(w, err)
}

// writeOpenAIProxyError maps upstream/proxy failures onto OpenAI error JSON
// with useful status codes and unwrapped human/quota messages.
func writeOpenAIProxyError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	message, errorType := openAIErrorFromCause(err)
	if errors.Is(err, pool.ErrNoEligibleAccounts) {
		status = http.StatusServiceUnavailable
	}
	var upstream *grok.UpstreamError
	if errors.As(err, &upstream) && upstream != nil {
		status = mapUpstreamStatusToOpenAI(upstream.Status)
	} else if unwrappedStatus, _, ok := unwrapAnthropicUpstreamMessage(errString(err)); ok && unwrappedStatus > 0 {
		// Reuse the same upstream-status peel helper (shared wrapper format).
		status = mapUpstreamStatusToOpenAI(unwrappedStatus)
	}
	writeOpenAIError(w, status, message, errorType)
}

func writeOpenAIError(w http.ResponseWriter, status int, message, errorType string) {
	if status <= 0 {
		status = http.StatusBadGateway
	}
	// Unwrap "upstream status N: {...}" and nested JSON error bodies.
	if unwrappedStatus, unwrappedBody, ok := unwrapAnthropicUpstreamMessage(message); ok {
		if unwrappedStatus > 0 {
			status = mapUpstreamStatusToOpenAI(unwrappedStatus)
		}
		if strings.TrimSpace(unwrappedBody) != "" {
			message = unwrappedBody
		}
		if errorType == "" || errorType == "server_error" || errorType == "api_error" {
			errorType = openAIErrorTypeForStatus(status)
		}
	}
	if errorType == "" {
		errorType = openAIErrorTypeForStatus(status)
	}
	if strings.TrimSpace(message) == "" {
		message = "request failed"
	}
	// OpenAI wire shape: { "error": { "message":"...", "type":"..." } }
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": errorType}})
}

func openAIErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "invalid_request_error"
	case http.StatusForbidden:
		return "invalid_request_error"
	case http.StatusNotFound:
		return "invalid_request_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge:
		return "invalid_request_error"
	default:
		return "server_error"
	}
}

func mapUpstreamStatusToOpenAI(status int) int {
	switch {
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		return status
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		return status
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		return status
	case status >= 500:
		return http.StatusBadGateway
	case status >= 400:
		return status
	default:
		return http.StatusBadGateway
	}
}

func openAIErrorFromCause(err error) (message, errorType string) {
	if err == nil {
		return "request failed", "server_error"
	}
	message = strings.TrimSpace(err.Error())
	errorType = "server_error"
	var upstream *grok.UpstreamError
	if errors.As(err, &upstream) && upstream != nil {
		if strings.TrimSpace(upstream.Body) != "" {
			message = preferAnthropicErrorBody(upstream.Body)
		}
		status := mapUpstreamStatusToOpenAI(upstream.Status)
		errorType = openAIErrorTypeForStatus(status)
		return firstNonEmptyStr(message, err.Error()), errorType
	}
	if status, body, ok := unwrapAnthropicUpstreamMessage(message); ok {
		if strings.TrimSpace(body) != "" {
			message = body
		}
		if status > 0 {
			errorType = openAIErrorTypeForStatus(mapUpstreamStatusToOpenAI(status))
		}
	}
	low := strings.ToLower(message)
	if strings.Contains(low, "empty model output") || strings.Contains(low, "no content/tool_calls") {
		errorType = "server_error"
	}
	if errors.Is(err, pool.ErrNoEligibleAccounts) {
		errorType = "server_error"
		if message == "" || strings.Contains(low, "no eligible") {
			message = "No eligible accounts available. All accounts may be in cooldown or disabled."
		}
	}
	return firstNonEmptyStr(message, "request failed"), errorType
}

func errString(err error) string {
	if err == nil {
		return ""
	}
	return err.Error()
}

func serveMessages(w http.ResponseWriter, r *http.Request, options Options) {
	apiKey, ok := messageRouteAllowed(w, r, options)
	if !ok {
		return
	}
	// Accepted for client compatibility (Claude SDKs send it); not enforced.
	_ = r.Header.Get("anthropic-version")
	// anthropic-beta: prompt-caching-* is accepted (sticky routing + usage fields).
	_ = r.Header.Get("anthropic-beta")
	var raw map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	messages, _ := raw["messages"].([]any)
	if len(messages) == 0 {
		writeAnthropicError(w, http.StatusBadRequest, "messages: Field required", "invalid_request_error")
		return
	}
	if !positiveNumber(raw["max_tokens"]) {
		writeAnthropicError(w, http.StatusBadRequest, "max_tokens: Input should be greater than or equal to 1", "invalid_request_error")
		return
	}
	stream, _ := raw["stream"].(bool)
	model := modelCatalog(options).Resolve(stringValue(raw["model"]))
	body, err := anthropic.BuildOpenAIChatBody(raw, model)
	if err != nil {
		writeAnthropicError(w, http.StatusBadRequest, err.Error(), "invalid_request_error")
		return
	}
	// Ensure Claude Code / Anthropic multi-turn always has a stable prompt_cache_key
	// before affinity + upstream (parity with OpenAI chat path). Seed from session
	// metadata / first-user when client omits explicit cache keys.
	pck := strings.TrimSpace(stringValue(body["prompt_cache_key"]))
	if pck == "" {
		pck = strings.TrimSpace(anthropic.ExtractPromptCacheKey(raw))
		if pck != "" {
			body["prompt_cache_key"] = pck
		}
	}
	// Echo sticky cache key so clients / relays can reuse it next turn.
	if pck != "" {
		w.Header().Set("X-Grok2API-Prompt-Cache-Key", pck)
	}
	allowedTools := allowedAnthropicToolNames(body)
	chatReq := proxy.ChatRequest{Model: model, Stream: false, Raw: body, UserAgent: r.UserAgent()}
	// Also inject pck into chatReq for affinity fingerprint when only raw had session metadata.
	if pck != "" {
		if chatReq.Raw == nil {
			chatReq.Raw = map[string]any{}
		}
		chatReq.Raw["prompt_cache_key"] = pck
	}
	candidates, err := listCandidatesForRequest(r.Context(), options, chatReq, r.Header)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	service := newChatService(options)
	// Claude Code multi-turn: always sticky-first when we have a session/cache key so
	// least_used / parallel probe cannot hop accounts and cold the prefix cache.
	if pck != "" || proxy.ChatFingerprint(chatReq) != "" {
		service.StickyFirstOnly = true
	}
	started := time.Now()
	messageID := newAnthropicMessageID()
	// Match Python resolve_outbound_max_tools for anthropic (Claude/sub2api-safe).
	policy := historycompact.ResolveOutboundToolPolicy(
		"anthropic",
		r.UserAgent(),
		options.runtimeConfig().OutboundMaxTools,
		options.runtimeConfig().OutboundMaxToolsOpenAI,
		options.runtimeConfig().OutboundMaxToolsResponsesNative,
		options.runtimeConfig().OutboundToolGap,
		options.runtimeConfig().OutboundToolGapNative,
	)
	maxTools := policy.MaxTools
	if maxTools < 0 {
		maxTools = 0
	}
	if stream {
		chatReq.Stream = true
		// Up to 3 open attempts: OpenStream already failovers inside one call, but
		// mid-stream empty after message_start cannot rotate. A fresh OpenStream after
		// a pure open-time empty (headers not yet written) recovers more often under
		// Claude Code high-effort empty storms. Once streamAnthropic writes headers,
		// we cannot retry transparently.
		var (
			opened       proxy.StreamOpen
			err          error
			usage        map[string]any
			firstTokenMS int
			lastAccount  string
			lastModel    = model
			exclude      = map[string]struct{}{}
		)
		maxOpenAttempts := 4
		candPool := candidates
		for attempt := 0; attempt < maxOpenAttempts; attempt++ {
			if attempt > 0 && lastAccount != "" {
				exclude[lastAccount] = struct{}{}
				// Drop just-failed account from the next window so OpenStream does not
				// sticky-repick a hollow token (prompt_cache pin is cleared on empty).
				filtered := make([]pool.Candidate, 0, len(candPool))
				for _, c := range candPool {
					if _, bad := exclude[c.ID]; bad {
						continue
					}
					filtered = append(filtered, c)
				}
				if len(filtered) == 0 {
					break
				}
				candPool = filtered
				// New message id so Claude Code does not merge half envelopes across tries.
				messageID = newAnthropicMessageID()
			}
			opened, err = service.OpenStreamWithResult(r.Context(), chatReq, candPool, resolvePickMode(options))
			openElapsed := time.Since(started)
			if err != nil {
				lastAccount = strings.TrimSpace(opened.AccountID)
				if lastAccount == "" {
					// Open failed with no account — nothing to rotate.
					recordAnthropicUsage(r, options, apiKey, opened.AccountID, model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
					writeAnthropicProxyError(w, err)
					return
				}
				// Open-time empty/5xx: report + try another window if attempts remain.
				reportChatPool(r, options, lastAccount, false, err, http.StatusBadGateway, lastModel)
				if attempt+1 >= maxOpenAttempts || !isRetryableUpstreamOpenErr(err) {
					recordAnthropicUsage(r, options, apiKey, lastAccount, model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
					writeAnthropicProxyError(w, err)
					return
				}
				continue
			}
			lastAccount = opened.AccountID
			lastModel = opened.Model
			req := r
			if options.runtimeConfig().SSEKeepalive > 0 {
				req = r.WithContext(withAnthropicKeepalive(r.Context(), options.runtimeConfig().SSEKeepalive))
			}
			if policy.ToolGap > 0 {
				req = req.WithContext(withOutboundToolGap(req.Context(), policy.ToolGap))
			}
			setAnthropicObservationHeaders(w, protocolObservation{Protocol: "anthropic",
				AccountID: opened.AccountID, PreferAccount: opened.PreferAccount, Failover: opened.Failover || attempt > 0,
				Fingerprint: opened.Fingerprint, Accounts: opened.Accounts, Prep: opened.Prep, Stream: true,
			})
			if pck := strings.TrimSpace(stringValue(body["prompt_cache_key"])); pck != "" {
				w.Header().Set("X-Grok2API-Prompt-Cache-Key", pck)
			}
			// streamAnthropicMessages writes headers + message_start. Only one attempt
			// can reach here per response; open-time empties are retried above.
			usage, firstTokenMS, err = streamAnthropicMessages(w, req, opened.Body, messageID, opened.Model, len(allowedTools) > 0, allowedTools, maxTools)
			_ = opened.Body.Close()
			releaseServerPick(options, opened.AccountID)
			if firstTokenMS > 0 {
				wall := int(openElapsed.Milliseconds()) + firstTokenMS
				if wall <= 0 {
					wall = 1
				}
				firstTokenMS = wall
			}
			ok := err == nil
			if !ok && errors.Is(err, r.Context().Err()) {
				ok = true
			}
			// Empty with no TTFT: headers likely uncommitted → try another account.
			if !ok && firstTokenMS == 0 && isRetryableUpstreamOpenErr(err) && attempt+1 < maxOpenAttempts {
				reportChatPool(r, options, opened.AccountID, false, err, http.StatusBadGateway, opened.Model)
				lastAccount = opened.AccountID
				lastModel = opened.Model
				continue
			}
			status := http.StatusOK
			if !ok {
				status = http.StatusBadGateway
			}
			recordAnthropicUsage(r, options, apiKey, opened.AccountID, opened.Model, true, ok, status, started, usage, err, firstTokenMS, raw)
			reportChatPool(r, options, opened.AccountID, ok, err, status, opened.Model)
			return
		}
		// Exhausted open attempts without streaming.
		if err == nil {
			err = errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		}
		recordAnthropicUsage(r, options, apiKey, lastAccount, model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
		writeAnthropicProxyError(w, err)
		return
	}
	result, err := service.CompleteWithResult(r.Context(), chatReq, candidates, resolvePickMode(options))
	if result.AccountID != "" {
		defer releaseServerPick(options, result.AccountID)
	}
	if err != nil {
		// Per-account free-usage / 额度用完 already kicked via FailureReporter.
		recordAnthropicUsage(r, options, apiKey, result.AccountID, model, false, false, http.StatusBadGateway, started, nil, err, 0, raw)
		writeAnthropicProxyError(w, err)
		return
	}
	recordAnthropicUsage(r, options, apiKey, result.AccountID, result.Model, false, true, http.StatusOK, started, result.Usage, nil, 0, raw)
	reportChatPool(r, options, result.AccountID, true, nil, http.StatusOK, result.Model)
	content, reasoning, finish, usage, toolCalls := anthropicCompletionParts(result.Payload)
	if maxTools > 0 && len(toolCalls) > maxTools {
		toolCalls = toolCalls[:maxTools]
	}
	setAnthropicObservationHeaders(w, protocolObservation{Protocol: "anthropic",
		AccountID: result.AccountID, PreferAccount: result.PreferAccount, Failover: result.Failover,
		Fingerprint: result.Fingerprint, Accounts: result.Accounts, Prep: result.Prep, Stream: false,
	})
	if pck := strings.TrimSpace(stringValue(body["prompt_cache_key"])); pck != "" {
		w.Header().Set("X-Grok2API-Prompt-Cache-Key", pck)
	}
	payload := anthropic.Completion(messageID, result.Model, content, reasoning, finish, toolCalls, usage, allowedTools)
	// Surface sticky key in body metadata for non-streaming clients.
	if pck := strings.TrimSpace(stringValue(body["prompt_cache_key"])); pck != "" {
		payload["prompt_cache_key"] = pck
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveMessagesCountTokens(w http.ResponseWriter, r *http.Request, options Options) {
	// Local heuristic only — no pool/store required (matches Python).
	if !messageCountRouteAllowed(w, r, options) {
		return
	}
	var raw map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	if !anthropic.HasMessagesOrSystem(raw) {
		writeAnthropicError(w, http.StatusBadRequest, "messages or system required", "invalid_request_error")
		return
	}
	writeJSON(w, http.StatusOK, anthropic.CountTokensForRequest(raw))
}

func messageRouteAllowed(w http.ResponseWriter, r *http.Request, options Options) (*auth.APIKeyRecord, bool) {
	if !options.MessagesEnabled {
		// Claude Code / Anthropic SDK parse type=error envelopes; plain detail breaks clients.
		writeAnthropicError(w, http.StatusServiceUnavailable, "Go messages route is not enabled", "api_error")
		return nil, false
	}
	if !isReady(options) {
		writeAnthropicError(w, http.StatusServiceUnavailable, readyReason(options), "api_error")
		return nil, false
	}
	var apiKey *auth.APIKeyRecord
	if options.APIKeys != nil {
		verified, err := options.APIKeys.Require(r.Context(), r)
		if err != nil {
			status := http.StatusInternalServerError
			message := err.Error()
			errType := "api_error"
			if errors.Is(err, auth.ErrInvalidAPIKey) {
				status = http.StatusUnauthorized
				message = "Invalid or missing API key"
				errType = "authentication_error"
			}
			writeAnthropicError(w, status, message, errType)
			return nil, false
		}
		apiKey = verified
	}
	if options.Store == nil && len(options.Candidates) == 0 {
		writeAnthropicError(w, http.StatusServiceUnavailable, "PostgreSQL store unavailable", "api_error")
		return nil, false
	}
	return apiKey, true
}

func messageCountRouteAllowed(w http.ResponseWriter, r *http.Request, options Options) bool {
	if !options.MessagesEnabled {
		writeAnthropicError(w, http.StatusServiceUnavailable, "Go messages route is not enabled", "api_error")
		return false
	}
	if !isReady(options) {
		writeAnthropicError(w, http.StatusServiceUnavailable, readyReason(options), "api_error")
		return false
	}
	if options.APIKeys != nil {
		if _, err := options.APIKeys.Require(r.Context(), r); err != nil {
			status := http.StatusInternalServerError
			message := err.Error()
			errType := "api_error"
			if errors.Is(err, auth.ErrInvalidAPIKey) {
				status = http.StatusUnauthorized
				message = "Invalid or missing API key"
				errType = "authentication_error"
			}
			writeAnthropicError(w, status, message, errType)
			return false
		}
	}
	return true
}

func clampCodexReasoning(raw, body map[string]any, userAgent string, enabled bool) {
	if !enabled || body == nil {
		return
	}
	tools := body["tools"]
	if tools == nil && raw != nil {
		tools = raw["tools"]
	}
	// Proxies may strip UA; detect Codex via tools schema (exec_command/cmd).
	if !historycompact.LooksLikeCodexRequest(userAgent, tools, raw) {
		return
	}

	// Honor explicit Codex thinking mode. Old behavior always rewrote effort to
	// low for TTFT, so UI Ultra/High/Proactive still showed (and ran) as low.
	// Mapping: Low/auto→low · Base/default→medium · High/Proactive→high · Ultra→high.
	client := reasoning.FromRequest(raw)
	if client == "" {
		client = reasoning.FromRequest(body)
	}
	up := reasoning.ToUpstream(client)
	if up == "" {
		// No explicit effort → keep TTFT-friendly low default.
		up = reasoning.Low
		client = reasoning.ClientLow
	}

	body["reasoning_effort"] = up
	body["reasoning"] = map[string]any{"effort": up, "summary": "auto"}
	if raw == nil {
		return
	}
	// Preserve client-facing label on raw for usage detail (ultra→ultracode…).
	// Only fill when the client omitted effort entirely.
	if reasoning.FromRequest(raw) == "" {
		raw["reasoning_effort"] = client
		if m, ok := raw["reasoning"].(map[string]any); ok && m != nil {
			if strings.TrimSpace(fmt.Sprint(m["effort"])) == "" {
				m["effort"] = client
			}
			if _, has := m["summary"]; !has {
				m["summary"] = "auto"
			}
			raw["reasoning"] = m
		}
	}
}

type shellArgKeysContextKey struct{}

func withShellArgKeys(ctx context.Context, keys map[string]string) context.Context {
	if len(keys) == 0 {
		return ctx
	}
	return context.WithValue(ctx, shellArgKeysContextKey{}, keys)
}

func shellArgKeysFrom(ctx context.Context) map[string]string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(shellArgKeysContextKey{}).(map[string]string)
	return v
}

type allowedToolsContextKey struct{}

func withAllowedTools(ctx context.Context, names []string) context.Context {
	if len(names) == 0 {
		return ctx
	}
	return context.WithValue(ctx, allowedToolsContextKey{}, append([]string(nil), names...))
}

func allowedToolNamesFrom(ctx context.Context) []string {
	if ctx == nil {
		return nil
	}
	v, _ := ctx.Value(allowedToolsContextKey{}).([]string)
	return v
}

// ensureCodexShellCmdKeys forces shell-like tools to project args as "cmd" for
// Codex clients, while preserving pure OpenAI/Hermes "command" schemas.
// Codex validates against its local tool schema (cmd); we still send "command"
// upstream to Grok. Hermes agent tool "terminal" requires "command".
func ensureCodexShellCmdKeys(tools any, keys map[string]string) map[string]string {
	if keys == nil {
		keys = map[string]string{}
	}
	items, _ := tools.([]any)
	for _, item := range items {
		tool, ok := item.(map[string]any)
		if !ok {
			continue
		}
		name := ""
		if fn, ok := tool["function"].(map[string]any); ok {
			name = strings.TrimSpace(fmt.Sprint(fn["name"]))
		}
		if name == "" {
			name = strings.TrimSpace(fmt.Sprint(tool["name"]))
		}
		if name == "" {
			continue
		}
		// Only shell family (include Codex exec_command / Hermes terminal / shell_command).
		if !toolcall.IsShellTool(name) {
			continue
		}
		// If client schema explicitly prefers command-only, keep it.
		pref := toolcall.PreferredShellArgKeyFromTool(tool)
		if pref == "command" {
			// Detect pure-command schema (no cmd property).
			params := any(nil)
			if fn, ok := tool["function"].(map[string]any); ok {
				params = fn["parameters"]
				if params == nil {
					params = fn["input_schema"]
				}
			}
			if params == nil {
				params = tool["parameters"]
			}
			if pm, ok := params.(map[string]any); ok {
				if props, ok := pm["properties"].(map[string]any); ok {
					_, hasCmd := props["cmd"]
					_, hasCommand := props["command"]
					if hasCommand && !hasCmd {
						keys[name] = "command"
						keys[strings.ToLower(name)] = "command"
						if nk := toolcall.NameKey(name); nk != "" {
							keys[nk] = "command"
						}
						continue
					}
				}
			}
			// Schema prefers command but properties missing/opaque (e.g. Hermes
			// terminal with incomplete params). Use name-aware default so we do
			// not rewrite Hermes "command" into Codex "cmd".
			if toolcall.DefaultShellArgKey(name) == "command" {
				keys[name] = "command"
				keys[strings.ToLower(name)] = "command"
				if nk := toolcall.NameKey(name); nk != "" {
					keys[nk] = "command"
				}
				continue
			}
		}
		// Default Codex shell → cmd (Codex local schema field name), unless the
		// tool itself is known to use OpenAI-style "command" (Hermes terminal).
		fallback := toolcall.DefaultShellArgKey(name)
		keys[name] = fallback
		keys[strings.ToLower(name)] = fallback
		if nk := toolcall.NameKey(name); nk != "" {
			keys[nk] = fallback
		}
	}
	return keys
}

func serveResponses(w http.ResponseWriter, r *http.Request, options Options) {
	if !options.ResponsesEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go responses route is not enabled"})
		return
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return
	}
	var apiKey *auth.APIKeyRecord
	if options.APIKeys != nil {
		verified, err := options.APIKeys.Require(r.Context(), r)
		if err != nil {
			status := http.StatusInternalServerError
			message := err.Error()
			if errors.Is(err, auth.ErrInvalidAPIKey) {
				status = http.StatusUnauthorized
				message = "Invalid or missing API key"
			}
			writeJSON(w, status, map[string]any{"detail": message})
			return
		}
		apiKey = verified
	}
	if options.Store == nil && len(options.Candidates) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var raw map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	stream, _ := raw["stream"].(bool)
	model := modelCatalog(options).Resolve(stringValue(raw["model"]))
	customToolNames := responses.CustomToolNameSet(raw["tools"])
	body := responses.BuildChatBody(raw, model)
	// Codex shell schema uses "cmd"; remember preferred keys from the *client* tools
	// before sanitize rewrites them to "command" for upstream Grok.
	keys := toolcall.ShellArgKeyMap(raw["tools"])
	// Always force shell-family tools onto "cmd". Codex validates local schema with
	// "cmd", not "command". Pure OpenAI command-only schemas are preserved by
	// ensureCodexShellCmdKeys when properties.command is exclusive and no cmd prop.
	// Do this even without UA detection — many proxies strip User-Agent.
	keys = ensureCodexShellCmdKeys(raw["tools"], keys)
	// If shell map still empty but request looks like Codex (UA or tools schema), seed common shell names.
	if len(keys) == 0 {
		var tools any
		if raw != nil {
			tools = raw["tools"]
		}
		if historycompact.LooksLikeCodexRequest(r.UserAgent(), tools, raw) {
			for _, name := range []string{
				"shell", "Shell", "bash", "Bash",
				"local_shell", "localShell", "exec", "run",
				"exec_command", "exec-command", "ExecCommand",
				"run_command", "shell_command", "ShellCommand",
			} {
				keys[name] = "cmd"
				keys[strings.ToLower(name)] = "cmd"
				keys[toolcall.NameKey(name)] = "cmd"
			}
		}
	}
	if len(keys) > 0 {
		r = r.WithContext(withShellArgKeys(r.Context(), keys))
	}
	clampCodexReasoning(raw, body, r.UserAgent(), options.runtimeConfig().CodexForceReasoningLow)
	messages, _ := body["messages"].([]map[string]any)
	if len(messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "input must contain at least one message", "invalid_request_error")
		return
	}
	chatReq := proxy.ChatRequest{Model: model, Stream: stream, Raw: body, UserAgent: r.UserAgent()}
	// Sticky keys for Codex multi-turn: prompt_cache_key first, then previous_response_id chain.
	// CRITICAL: never mint a *new* pck each turn from previous_response_id — that kills upstream
	// prompt cache. Always recover the same pck that produced the previous response when possible.
	pck := strings.TrimSpace(stringValue(raw["prompt_cache_key"]))
	if pck == "" {
		if meta, _ := raw["metadata"].(map[string]any); meta != nil {
			pck = strings.TrimSpace(stringValue(meta["prompt_cache_key"]))
		}
	}
	prevResp := strings.TrimSpace(stringValue(raw["previous_response_id"]))
	stickyFromPrev := ""
	if prevResp != "" {
		if acc, recoveredPCK := responseAffinityLookup(r.Context(), options, prevResp); acc != "" || recoveredPCK != "" {
			stickyFromPrev = strings.TrimSpace(acc)
			if pck == "" && recoveredPCK != "" {
				pck = recoveredPCK
			}
		}
	}
	if pck == "" {
		// Only mint a new session key for a fresh conversation root.
		// Prefer stable client session/conv headers over previous_response_id so
		// multi-turn without client pck still keeps one upstream cache key.
		if sid := strings.TrimSpace(r.Header.Get("X-Session-Id")); sid != "" {
			pck = "session:" + sid
		} else if cid := strings.TrimSpace(r.Header.Get("X-Grok-Conv-Id")); cid != "" {
			pck = "conv:" + cid
		} else if prevResp != "" {
			// Recovery missed (cold process / redis miss). Last resort only.
			pck = "respchain:" + prevResp
		}
	}
	if pck != "" {
		if chatReq.Raw == nil {
			chatReq.Raw = map[string]any{}
		}
		chatReq.Raw["prompt_cache_key"] = pck
		body["prompt_cache_key"] = pck
	}
	if prevResp != "" {
		if chatReq.Raw == nil {
			chatReq.Raw = map[string]any{}
		}
		chatReq.Raw["previous_response_id"] = prevResp
	}
	candidates, err := listCandidatesForRequest(r.Context(), options, chatReq, r.Header)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	// Hard-pin recovered previous_response account even when fingerprint lookup missed.
	if stickyFromPrev != "" {
		candidates = ensureStickyCandidate(r.Context(), options, candidates, stickyFromPrev)
		for i := range candidates {
			if candidates[i].ID == stickyFromPrev {
				if i > 0 {
					cand := candidates[i]
					copy(candidates[1:i+1], candidates[0:i])
					candidates[0] = cand
				}
				candidates[0].RequestCount -= 1_000_000_000
				break
			}
		}
	}
	service := newChatService(options)
	// Codex Responses multi-turn: sticky-first for prompt_cache / previous_response.
	service.StickyFirstOnly = true
	started := time.Now()
	responseID := responses.NewResponseID()
	respPolicy := historycompact.ResolveOutboundToolPolicy(
		"openai_responses",
		r.UserAgent(),
		options.runtimeConfig().OutboundMaxTools,
		options.runtimeConfig().OutboundMaxToolsOpenAI,
		options.runtimeConfig().OutboundMaxToolsResponsesNative,
		options.runtimeConfig().OutboundToolGap,
		options.runtimeConfig().OutboundToolGapNative,
	)
	if stream {
		// Multi-open BEFORE writing the Responses envelope so empty/hollow storms
		// can rotate accounts without response.failed on a half-open stream.
		// (Previously we wrote response.created immediately for Codex TTFT; empty
		// open then could only Fail — client saw empty model output. Open peek
		// already dominates TTFT under empty storms, so deferring Start is fine.)
		var (
			opened      proxy.StreamOpen
			err         error
			lastAccount string
			lastModel   = model
			exclude     = map[string]struct{}{}
		)
		maxOpenAttempts := 4
		candPool := candidates
		for attempt := 0; attempt < maxOpenAttempts; attempt++ {
			if attempt > 0 && lastAccount != "" {
				exclude[lastAccount] = struct{}{}
				filtered := make([]pool.Candidate, 0, len(candPool))
				for _, c := range candPool {
					if _, bad := exclude[c.ID]; bad {
						continue
					}
					filtered = append(filtered, c)
				}
				if len(filtered) == 0 {
					break
				}
				candPool = filtered
			}
			opened, err = service.OpenStreamWithResult(r.Context(), chatReq, candPool, resolvePickMode(options))
			if err != nil {
				lastAccount = strings.TrimSpace(opened.AccountID)
				if lastAccount == "" {
					recordResponsesUsage(r, options, apiKey, opened.AccountID, model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
					writeOpenAIProxyError(w, err)
					return
				}
				reportChatPool(r, options, lastAccount, false, err, http.StatusBadGateway, lastModel)
				if attempt+1 >= maxOpenAttempts || !isRetryableUpstreamOpenErr(err) {
					recordResponsesUsage(r, options, apiKey, lastAccount, model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
					writeOpenAIProxyError(w, err)
					return
				}
				continue
			}
			break
		}
		if err != nil {
			recordResponsesUsage(r, options, apiKey, lastAccount, model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
			writeOpenAIProxyError(w, err)
			return
		}
		if opened.Body == nil {
			err = errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
			recordResponsesUsage(r, options, apiKey, lastAccount, model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
			writeOpenAIProxyError(w, err)
			return
		}
		openElapsed := time.Since(started)
		flusher, canFlush := w.(http.Flusher)
		if !canFlush {
			_ = opened.Body.Close()
			releaseServerPick(options, opened.AccountID)
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "streaming is not supported by this response writer"})
			return
		}
		w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache, no-transform")
		w.Header().Set("Connection", "keep-alive")
		w.Header().Set("X-Accel-Buffering", "no")
		w.Header().Set("X-Grok2API-Protocol", "openai_responses")
		if pck != "" {
			w.Header().Set("X-Grok2API-Prompt-Cache-Key", pck)
		}
		// Bind this response_id so next Codex turn with previous_response_id sticks.
		bindResponseAffinity(r.Context(), options, responseID, opened.AccountID, opened.Model, pck)
		setProtocolObservationHeaders(w, protocolObservation{
			Protocol: "openai_responses", AccountID: opened.AccountID, PreferAccount: opened.PreferAccount,
			Failover: opened.Failover, Fingerprint: opened.Fingerprint, Accounts: opened.Accounts, Prep: opened.Prep,
		})
		w.WriteHeader(http.StatusOK)
		early := responses.NewLiveStreamerWithMaxTools(responseID, model, allowedResponsesToolNames(body), respPolicy.MaxTools)
		early.SetShellArgKeys(shellArgKeysFrom(r.Context()))
		early.SetCustomToolNames(customToolNames)
		for _, frame := range early.Start() {
			_, _ = w.Write([]byte(frame))
		}
		if canFlush {
			flusher.Flush()
		}
		defer opened.Body.Close()
		defer releaseServerPick(options, opened.AccountID)
		req := r
		if options.runtimeConfig().SSEKeepalive > 0 {
			req = r.WithContext(withAnthropicKeepalive(r.Context(), options.runtimeConfig().SSEKeepalive))
		}
		if respPolicy.ToolGap > 0 {
			req = req.WithContext(withOutboundToolGap(req.Context(), respPolicy.ToolGap))
		}
		usage, firstTokenMS, err := streamOpenAIResponsesContinue(w, req, opened.Body, early, effectiveResponsesKeepalive(optionsFromRequest(req).Keepalive, len(allowedResponsesToolNames(body)) > 0), respPolicy.MaxTools)
		// firstTokenMS from Continue is ms from stream-read start → first delivered payload.
		// Admin TTFT = open/pick duration + stream-local first payload (NOT full request latency).
		if firstTokenMS > 0 {
			wallFirst := int(openElapsed.Milliseconds()) + firstTokenMS
			if wallFirst <= 0 {
				wallFirst = 1
			}
			firstTokenMS = wallFirst
		}
		// Client cancel after headers is soft-ok only when the stream function
		// already soft-closed a real payload (returns nil). Empty/tool-vanished
		// returns an explicit empty error → not ok.
		ok := err == nil
		if !ok && errors.Is(err, r.Context().Err()) {
			ok = true
		}
		status := http.StatusOK
		if !ok {
			status = http.StatusBadGateway
		}
		// Hollow / half-open function_call streams return empty error from the
		// stream function (ClientDeliveryOK). Zero usage tokens alone are not a
		// failure — xAI often omits usage on short tool turns.
		recordResponsesUsage(r, options, apiKey, opened.AccountID, opened.Model, true, ok, status, started, usage, err, firstTokenMS, raw)
		reportChatPool(r, options, opened.AccountID, ok, err, status, opened.Model)
		return
	}
	chatReq.Stream = false
	result, err := service.CompleteWithResult(r.Context(), chatReq, candidates, resolvePickMode(options))
	if result.AccountID != "" {
		defer releaseServerPick(options, result.AccountID)
	}
	if err != nil {
		// Per-account free-usage / 额度用完 already kicked via FailureReporter.
		recordResponsesUsage(r, options, apiKey, result.AccountID, model, false, false, http.StatusBadGateway, started, nil, err, 0, raw)
		writeOpenAIProxyError(w, err)
		return
	}
	recordResponsesUsage(r, options, apiKey, result.AccountID, result.Model, false, true, http.StatusOK, started, result.Usage, nil, 0, raw)
	reportChatPool(r, options, result.AccountID, true, nil, http.StatusOK, result.Model)
	content, reasoning, _, _, toolCalls := anthropicCompletionParts(result.Payload)
	bindResponseAffinity(r.Context(), options, responseID, result.AccountID, result.Model, pck)
	setProtocolObservationHeaders(w, protocolObservation{
		Protocol: "openai_responses", AccountID: result.AccountID, PreferAccount: result.PreferAccount,
		Failover: result.Failover, Fingerprint: result.Fingerprint, Accounts: result.Accounts, Prep: result.Prep,
	})
	if pck != "" {
		w.Header().Set("X-Grok2API-Prompt-Cache-Key", pck)
	}
	writeJSON(w, http.StatusOK, responses.BuildObject(responseID, result.Model, content, reasoning, responseToolCalls(toolCalls, shellArgKeysFrom(r.Context()), customToolNames), usageMap(result.Usage), time.Now().Unix(), stringValue(raw["previous_response_id"]), metadataMap(raw["metadata"])))
}

// effectiveResponsesKeepalive tightens SSE keepalive for tool-heavy Codex turns
// so reverse proxies (~30–60s idle) do not cut the stream while incomplete
// function_call args are still buffering (upstream not idle → ReadSSEWithIdle
// would not fire keepalives otherwise — we force client keepalives from Feed).
func effectiveResponsesKeepalive(base time.Duration, toolsRequested bool) time.Duration {
	if base <= 0 {
		base = 4 * time.Second
	}
	if toolsRequested && base > 3*time.Second {
		return 3 * time.Second
	}
	// Even without declared tools, keep a sane upper bound for Codex multi-minute
	// tool loops that declare tools mid-session via previous_response.
	if base > 5*time.Second {
		return 5 * time.Second
	}
	return base
}

// responsesKeepaliveFrame is a pure SSE comment plus an OpenAI-compatible ping
// event so both dumb reverse proxies and Codex-style clients stay warm.
func responsesKeepaliveFrame() string {
	return ": keepalive\n\nevent: response.ping\ndata: {\"type\":\"response.ping\"}\n\n"
}

func streamOpenAIResponses(w http.ResponseWriter, r *http.Request, body io.Reader, responseID, model string, allowed []string, keepalive time.Duration, maxTools int) (map[string]any, int, error) {
	if _, ok := w.(http.Flusher); !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "streaming is not supported by this response writer"})
		return nil, 0, errors.New("streaming is not supported by this response writer")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Grok2API-Protocol", "openai_responses")
	w.WriteHeader(http.StatusOK)
	if maxTools < 0 {
		maxTools = 0
	}
	streamer := responses.NewLiveStreamerWithMaxTools(responseID, model, allowed, maxTools)
	streamer.SetShellArgKeys(shellArgKeysFrom(r.Context()))
	return runOpenAIResponsesStream(w, r, body, streamer, keepalive, maxTools, false, len(allowed) > 0)
}

func responsesToolDeltas(delta proxy.ChatDelta) []responses.ToolDelta {
	chatDeltas := delta.AnthropicToolDeltas()
	out := make([]responses.ToolDelta, 0, len(chatDeltas))
	for _, item := range chatDeltas {
		out = append(out, responses.ToolDelta{Index: item.Index, ID: item.ID, Name: item.Name, Arguments: item.Arguments})
	}
	return out
}

func responsesUsageFromOpenAI(usage map[string]any) responses.Usage {
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usage)
	return responses.Usage{InputTokens: int(prompt), OutputTokens: int(completion), TotalTokens: int(total), CachedTokens: int(cacheRead), CacheCreationTokens: int(cacheCreate), ReasoningTokens: int(reasoning)}
}

func allowedResponsesToolNames(body map[string]any) []string {
	return allowedAnthropicToolNames(body)
}

// streamOpenAIResponsesContinue continues an already-opened Responses SSE after early envelope.
// streamOpenAIResponsesContinue continues an already-opened Responses SSE after early envelope.
func streamOpenAIResponsesContinue(w http.ResponseWriter, r *http.Request, body io.Reader, streamer *responses.LiveStreamer, keepalive time.Duration, maxTools int) (map[string]any, int, error) {
	toolsRequested := streamer != nil && (streamer.HasPendingTools() || streamer.HasClientPayload())
	return runOpenAIResponsesStream(w, r, body, streamer, keepalive, maxTools, true, toolsRequested)
}

type responseAffinityStore interface {
	BindResponseAccount(ctx context.Context, responseID, accountID, promptCacheKey string) error
	GetResponseAccount(ctx context.Context, responseID string) (accountID, promptCacheKey string, err error)
}

func responseAffinityLookup(ctx context.Context, options Options, previousResponseID string) (accountID, promptCacheKey string) {
	previousResponseID = strings.TrimSpace(previousResponseID)
	if previousResponseID == "" || options.AffinityStore == nil {
		return "", ""
	}
	// Prefer dedicated response_id map (account + recovered prompt_cache_key).
	if store, ok := options.AffinityStore.(responseAffinityStore); ok {
		acc, pck, err := store.GetResponseAccount(ctx, previousResponseID)
		if err == nil && strings.TrimSpace(acc) != "" {
			return strings.TrimSpace(acc), strings.TrimSpace(pck)
		}
	}
	// Fallback: fingerprint bindings written by bindResponseAffinity (no pck recovery).
	if id, err := options.AffinityStore.GetAffinity(ctx, "chat:previous_response_id:"+previousResponseID); err == nil {
		if acc := strings.TrimSpace(id); acc != "" {
			return acc, ""
		}
	}
	return "", ""
}

func bindResponseAffinity(ctx context.Context, options Options, responseID, accountID, model, promptCacheKey string) {
	responseID = strings.TrimSpace(responseID)
	accountID = strings.TrimSpace(accountID)
	model = strings.TrimSpace(model)
	promptCacheKey = strings.TrimSpace(promptCacheKey)
	if responseID == "" || accountID == "" || options.AffinityStore == nil {
		return
	}
	// Durable sticky for next turn:
	// 1) response_id -> (account, pck)  [local cache + redis]
	// 2) pck fingerprints -> account
	// 3) previous_response_id fingerprints -> account
	// Adapter already does optimistic local cache + one background Redis write.
	// Do NOT wrap another short-timeout goroutine here (that caused cache miss races).
	if store, ok := options.AffinityStore.(responseAffinityStore); ok {
		_ = store.BindResponseAccount(ctx, responseID, accountID, promptCacheKey)
	}
	if promptCacheKey != "" {
		_ = options.AffinityStore.BindAffinity(ctx, "chat:prompt_cache_key:"+promptCacheKey, accountID)
		if model != "" {
			_ = options.AffinityStore.BindAffinity(ctx, "chat:"+model+":prompt_cache_key:"+promptCacheKey, accountID)
		}
	}
	_ = options.AffinityStore.BindAffinity(ctx, "chat:previous_response_id:"+responseID, accountID)
	if model != "" {
		_ = options.AffinityStore.BindAffinity(ctx, "chat:"+model+":previous_response_id:"+responseID, accountID)
	}
}

func recordResponsesUsage(r *http.Request, options Options, apiKey *auth.APIKeyRecord, accountID, model string, stream bool, ok bool, status int, started time.Time, usage any, cause error, ttftMS int, requestBody map[string]any) {
	usageMap, _ := usage.(map[string]any)
	// Stream path may already have filled completion from LiveStreamer (fillStreamUsage).
	// Always re-run fill with request body so prompt/total never stay hollow when ok.
	hint := 0
	if ok {
		// Prefer non-zero completion already in usage (from streamer estimate or upstream).
		if _, c, _, _, _, _ := postgres.UsageFromOpenAI(usageMap); c > 0 {
			hint = int(c)
		}
		// Also read output_tokens alias if completion was stored that way only.
		if hint <= 0 && usageMap != nil {
			if v, ok := usageMap["output_tokens"]; ok {
				if n := anyToInt64(v); n > 0 {
					hint = int(n)
				}
			}
		}
		if hint <= 0 && ttftMS > 0 {
			hint = 1 // TTFT>0 proves client saw payload
		}
	}
	// Propagate streamer estimate markers before re-fill.
	preEstCompletion := usageMap != nil && usageMap["_estimated_completion"] == true
	preEstPrompt := usageMap != nil && usageMap["_estimated_prompt"] == true
	preEstTotal := usageMap != nil && usageMap["_estimated_total"] == true
	preMissing := usageMap != nil && usageMap["_usage_missing"] == true
	streamerChars := 0
	streamerEst := 0
	if usageMap != nil {
		streamerChars = int(anyToInt64(usageMap["_streamer_output_chars"]))
		streamerEst = int(anyToInt64(usageMap["_streamer_estimate"]))
	}
	filled, flags := fillMissingUsage(usageMap, requestBody, hint)
	if preEstCompletion {
		flags.EstimatedCompletion = true
	}
	if preEstPrompt {
		flags.EstimatedPrompt = true
	}
	if preEstTotal {
		flags.EstimatedTotal = true
	}
	if preMissing {
		flags.Missing = true
	}
	if ok {
		usageMap = filled
		// Final hard guarantee: never write ok=true with completion=0 when TTFT>0.
		p, c, t, _, _, _ := postgres.UsageFromOpenAI(usageMap)
		if c == 0 && ttftMS > 0 {
			c = 1
			flags.EstimatedCompletion = true
			flags.Missing = true
			if p <= 0 {
				if est := estimatePromptTokens(requestBody); est > 0 {
					p = int64(est)
					flags.EstimatedPrompt = true
				} else {
					p = 1
					flags.EstimatedPrompt = true
				}
			}
			if t < p+c {
				t = p + c
				flags.EstimatedTotal = true
			}
			usageMap = map[string]any{
				"prompt_tokens":     p,
				"completion_tokens": c,
				"total_tokens":      t,
				"input_tokens":      p,
				"output_tokens":     c,
			}
			if _, _, _, cr, cc, rs := postgres.UsageFromOpenAI(filled); true {
				if cr > 0 {
					usageMap["cache_read_tokens"] = cr
					usageMap["cached_tokens"] = cr
				}
				if cc > 0 {
					usageMap["cache_creation_tokens"] = cc
				}
				if rs > 0 {
					usageMap["reasoning_tokens"] = rs
				}
			}
		} else if (p == 0 || t == 0 || t < p+c) && ttftMS > 0 {
			// Keep existing completion; only fix prompt/total holes.
			if p <= 0 {
				if est := estimatePromptTokens(requestBody); est > 0 {
					p = int64(est)
					flags.EstimatedPrompt = true
				} else {
					p = 1
					flags.EstimatedPrompt = true
				}
			}
			if t < p+c {
				t = p + c
				flags.EstimatedTotal = true
			}
			flags.Missing = true
			usageMap["prompt_tokens"] = p
			usageMap["completion_tokens"] = c
			usageMap["total_tokens"] = t
			usageMap["input_tokens"] = p
			usageMap["output_tokens"] = c
		}
	} else if usageMap == nil {
		usageMap = map[string]any{}
		flags = usageFillFlags{}
	}
	// Drop internal estimate markers before numeric parse / storage.
	if usageMap != nil {
		for _, k := range []string{"_estimated_completion", "_estimated_prompt", "_estimated_total", "_usage_missing", "_streamer_output_chars", "_streamer_estimate"} {
			delete(usageMap, k)
		}
	}
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usageMap)
	streamValue := stream
	var apiKeyID string
	if apiKey != nil {
		apiKeyID = apiKey.ID
	}
	var errText string
	if cause != nil {
		errText = cause.Error()
	}
	latency := int(time.Since(started).Milliseconds())
	var ttftPtr *int
	if ttftMS > 0 {
		v := ttftMS
		ttftPtr = &v
	}
	detail := map[string]any{"route": "go_responses"}
	if effort := extractReasoningEffort(requestBody); effort != "" {
		detail["reasoning_effort"] = effort
		detail["thinking_intensity"] = effort
	}
	if ttftMS > 0 {
		detail["ttft_ms"] = ttftMS
	}
	detail["latency_ms"] = latency
	if streamerChars > 0 {
		detail["streamer_output_chars"] = streamerChars
	}
	if streamerEst > 0 {
		detail["streamer_estimate_tokens"] = streamerEst
	}
	// If completion came from streamer estimate (markers) but fillMissingUsage
	// did not re-flag it, still surface completion in usage_estimated_fields.
	if preEstCompletion && !flags.EstimatedCompletion {
		flags.EstimatedCompletion = true
		flags.Missing = true
	}
	if ok {
		flags.apply(detail)
	}
	// Fire-and-forget with longer timeout - usage recording should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if options.Store != nil {
			if _, _, err := options.Store.RecordUsage(ctx, postgres.UsageRecord{
				RequestID:           requestID(r),
				Implementation:      "go",
				APIKeyID:            apiKeyID,
				AccountID:           accountID,
				Model:               model,
				Protocol:            "openai_responses",
				Path:                r.URL.Path,
				Stream:              &streamValue,
				OK:                  ok,
				PromptTokens:        prompt,
				CompletionTokens:    completion,
				TotalTokens:         total,
				CacheReadTokens:     cacheRead,
				CacheCreationTokens: cacheCreate,
				ReasoningTokens:     reasoning,
				ClientIP:            clientIP(r),
				UserAgent:           r.UserAgent(),
				StatusCode:          &status,
				LatencyMS:           &latency,
				TTFTMS:              ttftPtr,
				Error:               errText,
				Detail:              detail,
			}); err != nil {
				slog.Warn("record usage failed", "error", err, "account_id", accountID, "model", model, "ok", ok)
			}
		}
		recordRedisUsage(options, apiKeyID, accountID, model, prompt, completion, total, cacheRead, ok)
	}()
}

func responseToolCalls(calls []anthropic.ToolCall, keys map[string]string, customToolNames map[string]bool) []map[string]any {
	if keys == nil {
		keys = map[string]string{}
	}
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		name := strings.TrimSpace(call.Name)
		args := call.Arguments
		// Project shell args to client schema (Codex: cmd; Hermes terminal: command).
		// Internal form is always "command".
		if toolcall.IsShellTool(name) {
			preferred := toolcall.DefaultShellArgKey(name)
			if v := strings.TrimSpace(keys[name]); v != "" {
				preferred = v
			} else if v := strings.TrimSpace(keys[strings.ToLower(name)]); v != "" {
				preferred = v
			} else if nk := toolcall.NameKey(name); nk != "" {
				if v := strings.TrimSpace(keys[nk]); v != "" {
					preferred = v
				}
			}
			args = toolcall.ProjectShellArgsForClient(args, name, preferred)
		}
		item := map[string]any{
			"id":   call.ID,
			"type": "function",
			"function": map[string]any{
				"name":      name,
				"arguments": args,
			},
		}
		if customToolNames[name] || customToolNames[strings.ToLower(name)] || customToolNames[toolcall.NameKey(name)] {
			item["type"] = "custom"
		}
		out = append(out, item)
	}
	return out
}

func usageMap(value any) map[string]any {
	usage, _ := value.(map[string]any)
	if usage == nil {
		return map[string]any{}
	}
	return usage
}

func metadataMap(value any) map[string]any {
	metadata, _ := value.(map[string]any)
	return metadata
}

type protocolObservation struct {
	Protocol      string
	AccountID     string
	PreferAccount string
	Failover      bool
	Fingerprint   string
	Accounts      int
	Prep          proxy.BodyPrepStats
	Stream        bool
}

func setAnthropicObservationHeaders(w http.ResponseWriter, obs protocolObservation) {
	if obs.Protocol == "" {
		obs.Protocol = "anthropic"
	}
	setProtocolObservationHeaders(w, obs)
}

func setProtocolObservationHeaders(w http.ResponseWriter, obs protocolObservation) {
	if obs.Protocol == "" {
		obs.Protocol = "go"
	}
	w.Header().Set("X-Grok2API-Protocol", obs.Protocol)
	if obs.Accounts > 0 {
		w.Header().Set("X-Grok2API-Accounts", strconv.Itoa(obs.Accounts))
	}
	if obs.PreferAccount != "" {
		w.Header().Set("X-Grok2API-Affinity", "1")
	} else {
		w.Header().Set("X-Grok2API-Affinity", "0")
	}
	if obs.Failover {
		w.Header().Set("X-Grok2API-Affinity-Rebind", "1")
	}
	if obs.Fingerprint != "" {
		w.Header().Set("X-Grok2API-Conversation-Fp", obs.Fingerprint)
	}
	if obs.AccountID != "" {
		w.Header().Set("X-Grok2API-Account", obs.AccountID)
	}
	if compact := obs.Prep.Compact; compact != nil {
		if truthyAny(compact["applied"]) {
			w.Header().Set("X-Grok2API-History-Compact", "1")
		} else {
			w.Header().Set("X-Grok2API-History-Compact", "0")
		}
		if v, ok := compact["before_chars"]; ok {
			w.Header().Set("X-Grok2API-History-Before", fmt.Sprint(v))
		}
		if v, ok := compact["after_chars"]; ok {
			w.Header().Set("X-Grok2API-History-After", fmt.Sprint(v))
		}
		if v, ok := compact["tool_rounds"]; ok {
			w.Header().Set("X-Grok2API-History-Tool-Rounds", fmt.Sprint(v))
		}
		if truthyAny(compact["prefix_stable"]) {
			w.Header().Set("X-Grok2API-History-Prefix-Stable", "1")
		}
		if truthyAny(compact["auto"]) {
			w.Header().Set("X-Grok2API-History-Auto", "1")
		}
	}
	if stabilize := obs.Prep.Stabilize; stabilize != nil {
		w.Header().Set("X-Grok2API-Prompt-Stable", "1")
		w.Header().Set("X-Grok2API-Prompt-Stable-Messages", fmt.Sprint(stabilize["messages_stabilized"]))
		w.Header().Set("X-Grok2API-Prompt-Stable-Tools", fmt.Sprint(stabilize["tools_stabilized"]))
	} else {
		w.Header().Set("X-Grok2API-Prompt-Stable", "0")
	}
}

func truthyAny(value any) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case string:
		switch strings.ToLower(strings.TrimSpace(v)) {
		case "1", "true", "yes", "on":
			return true
		default:
			return false
		}
	default:
		return false
	}
}

func streamAnthropicMessages(w http.ResponseWriter, r *http.Request, body io.Reader, messageID, model string, toolsRequested bool, allowed []string, maxTools int) (map[string]any, int, error) {
	return streamAnthropicMessagesWithOptions(w, r, body, messageID, model, toolsRequested, allowed, maxTools, optionsFromRequest(r))
}

type anthropicStreamOptions struct {
	Keepalive time.Duration
}

func optionsFromRequest(r *http.Request) anthropicStreamOptions {
	// Default matches Python SSE_KEEPALIVE_INTERVAL (~4s). Tests can override via context value.
	keepalive := 4 * time.Second
	if r != nil {
		if value := r.Context().Value(anthropicKeepaliveContextKey{}); value != nil {
			if d, ok := value.(time.Duration); ok {
				keepalive = d
			}
		}
	}
	return anthropicStreamOptions{Keepalive: keepalive}
}

type anthropicKeepaliveContextKey struct{}

func withAnthropicKeepalive(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, anthropicKeepaliveContextKey{}, d)
}

type outboundToolGapContextKey struct{}

func withOutboundToolGap(ctx context.Context, d time.Duration) context.Context {
	return context.WithValue(ctx, outboundToolGapContextKey{}, d)
}

func outboundToolGapFrom(ctx context.Context) time.Duration {
	if ctx == nil {
		return 0
	}
	if value := ctx.Value(outboundToolGapContextKey{}); value != nil {
		if d, ok := value.(time.Duration); ok {
			return d
		}
	}
	return 0
}

func streamAnthropicMessagesWithOptions(w http.ResponseWriter, r *http.Request, body io.Reader, messageID, model string, toolsRequested bool, allowed []string, maxTools int, opts anthropicStreamOptions) (map[string]any, int, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": "streaming is not supported by this response writer"})
		return nil, 0, errors.New("streaming is not supported by this response writer")
	}
	w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.Header().Set("X-Grok2API-Protocol", "anthropic")
	// Delay WriteHeader until first real SSE write so open-time empty can rotate
	// accounts in serveMessages without committing HTTP 200.
	headersWritten := false
	ensureHeaders := func() {
		if headersWritten {
			return
		}
		w.WriteHeader(http.StatusOK)
		headersWritten = true
	}

	started := time.Now()
	firstTokenMS := 0

	assembler := anthropic.NewStreamAssembler(messageID, model, toolsRequested, maxTools, allowed)
	// Claude Code / reverse proxies often blip once under write pressure; require
	// multiple consecutive ctx-done hits across a short span before treating as gone.
	probe := newDisconnectProbe(3, 800*time.Millisecond)
	envelopeOpen := false
	// Soft client write/ctx blips after envelope open must NOT abort ReadSSE:
	// abort drains remaining upstream tool-arg frames → force-finish half tools →
	// Claude Code "Tool use interrupted". Keep assembling; only best-effort write.
	sw := newSSEWriter(w, flusher, r.Context())
	var writeMu sync.Mutex
	// writeFrames writes one logical batch via shared sseWriter.
	// Soft short-writes resume at complete SSE frame boundaries (not full-buffer
	// retry) so content_block_start is never duplicated mid-tool-group — that was
	// a major intermittent "Tool use interrupted" source under write pressure.
	// Force is automatic once envelope is open so terminal tools can still land.
	// Returns the complete-frame-aligned payload that landed (for Ack).
	writeFrames := func(frames []string, force bool) (delivered string, fullOK bool, err error) {
		if len(frames) == 0 {
			return "", true, nil
		}
		ensureHeaders()
		writeMu.Lock()
		defer writeMu.Unlock()
		forceWrite := force || envelopeOpen || sw.SoftGone()
		// Tool/terminal groups: more resume attempts under softGone so rem tail
		// (content_block_stop / message_stop) still lands after a short write.
		attempts := 3
		if forceWrite {
			attempts = 5
		}
		return sw.WriteStringsResumable(frames, forceWrite, attempts)
	}
	// ackBatchFromFrames marks only what this Write batch actually contained.
	// Blind AckEmittedTools after a text/thinking-only flush would wrongly mark
	// not-yet-written tools acked; missing Ack of message_stop leaves terminal
	// pending stuck. Both paths surface as intermittent "Tool use interrupted".
	// Each tool group is one content_block_start with content_block.type=tool_use
	// (see anthropic.event JSON: `"type":"tool_use"` once per group).
	ackBatchFromFrames := func(joined string, fullOK bool) {
		if joined == "" {
			return
		}
		if strings.Contains(joined, "event: message_start") || strings.Contains(joined, `"type":"message_start"`) {
			assembler.AckMessageStart()
		}
		// Tool Ack only when full group landed (fullOK) or content_block_stop is present.
		// Acking on content_block_start alone after short-write left half-open tool_use.
		hasTool := strings.Contains(joined, `"type":"tool_use"`) || strings.Contains(joined, "tool_use")
		hasToolStop := strings.Contains(joined, "content_block_stop")
		if hasTool && (fullOK || hasToolStop) {
			assembler.AckToolsInPayload(joined)
		}
		if strings.Contains(joined, "text_delta") || strings.Contains(joined, "thinking_delta") ||
			strings.Contains(joined, `"type":"text"`) || strings.Contains(joined, `"type":"thinking"`) {
			assembler.AckContentDelivered()
			if firstTokenMS == 0 {
				firstTokenMS = int(time.Since(started).Milliseconds())
				if firstTokenMS <= 0 {
					firstTokenMS = 1
				}
			}
		}
		// Tool start that fully landed also counts as first client-visible output.
		if hasTool && (fullOK || hasToolStop) && firstTokenMS == 0 {
			firstTokenMS = int(time.Since(started).Milliseconds())
			if firstTokenMS <= 0 {
				firstTokenMS = 1
			}
		}
		if strings.Contains(joined, "event: message_stop") || strings.Contains(joined, `"type":"message_stop"`) {
			assembler.AckTerminal()
		}
	}
	toolGap := outboundToolGapFrom(r.Context())
	toolsEmitted := 0
	// pendingSSE coalesces live thinking/text micro-deltas (Claude Code long
	// thinking turns) so we do not Flush once per token. Flushed on tool groups,
	// terminal, size, idle, or first client-visible payload (TTFT).
	//
	// Only clear after full delivery (LastOK). Soft short-writes advance by
	// LastWritten so the accepted prefix is not re-sent and the tail is kept
	// for tool-boundary / idle / Finish drain. Full-buffer drop on soft-fail
	// used to truncate Claude Code thinking/text (Finish only closes blocks —
	// it does not re-emit lost text_delta frames).
	pendingSSE := make([]byte, 0, textCoalesceMax*2)
	firstPayloadFlushed := false
	flushPendingSSE := func(force bool) error {
		if len(pendingSSE) == 0 {
			return nil
		}
		attempts := 1
		if force {
			attempts = 3
		}
		var lastErr error
		for i := 0; i < attempts && len(pendingSSE) > 0; i++ {
			if i > 0 {
				time.Sleep(time.Duration(i) * 2 * time.Millisecond)
			}
			payload := pendingSSE
			streamCoalesceFlush.Add(1)
			// Direct WriteBytes: payload is already concatenated SSE frames.
			ensureHeaders()
			writeMu.Lock()
			lastErr = sw.WriteBytes(payload, force || sw.SoftGone() || envelopeOpen)
			writeMu.Unlock()
			if lastErr != nil {
				return lastErr
			}
			if sw.LastOK() {
				pendingSSE = pendingSSE[:0]
				envelopeOpen = true
				firstPayloadFlushed = true
				assembler.AckContentDelivered()
				// Coalesced thinking/text often lands only on flush — mark TTFT here.
				if firstTokenMS == 0 {
					firstTokenMS = int(time.Since(started).Milliseconds())
					if firstTokenMS <= 0 {
						firstTokenMS = 1
					}
				}
				return nil
			}
			// Soft fail: advance past accepted bytes; keep unsent tail.
			if n := sw.LastWritten(); n > 0 {
				if n >= len(pendingSSE) {
					pendingSSE = pendingSSE[:0]
				} else {
					pendingSSE = append(pendingSSE[:0], pendingSSE[n:]...)
				}
				if len(pendingSSE) == 0 {
					envelopeOpen = true
					firstPayloadFlushed = true
					// Partial short-write may still have delivered some text.
					assembler.AckContentDelivered()
					if firstTokenMS == 0 {
						firstTokenMS = int(time.Since(started).Milliseconds())
						if firstTokenMS <= 0 {
							firstTokenMS = 1
						}
					}
					return nil
				}
			}
		}
		// Soft-fail with residual: leave pendingSSE for later force drain.
		return nil
	}
	// Precompute keepalive once — Ping()+CommentKeepalive() allocate every call.
	anthropicKeepaliveFrame := anthropic.Ping() + anthropic.CommentKeepalive()
	// flushGroup writes one logical batch (text/thinking, one tool group, or
	// message_delta+stop). requeueOnSoft controls whether a soft skip/fail
	// immediately rolls assembler state back.
	//
	// Finish multi-group path MUST use requeueOnSoft=false for every group and
	// only Requeue once AFTER all groups attempt. Mid-Finish Requeue clears
	// pendingTerminal while later groups still hold the same message_stop bytes;
	// a successful terminal Write then cannot AckTerminal → outer Finish re-emits
	// tools + second message_stop → Claude Code "Tool use interrupted".
	flushGroup := func(all []string, force bool, requeueOnSoft bool) error {
		if len(all) == 0 {
			return nil
		}
		delivered, fullOK, err := writeFrames(all, force || sw.SoftGone() || envelopeOpen)
		if err != nil {
			if requeueOnSoft && assembler.HasUnackedTools() {
				assembler.RequeueUnackedTools()
			}
			return err
		}
		if delivered != "" {
			envelopeOpen = true
		}
		if fullOK {
			ackBatchFromFrames(delivered, true)
			return nil
		}
		// Soft incomplete: Ack only safe complete pieces (tools need content_block_stop).
		if delivered != "" {
			ackBatchFromFrames(delivered, false)
		}
		if requeueOnSoft && assembler.HasUnackedTools() {
			assembler.RequeueUnackedTools()
		}
		return nil
	}
	// flushGroupRetry re-attempts the SAME frame bytes after soft write blips.
	// Never Requeue between attempts or between Finish groups — keep pending*
	// so a successful retry can Ack. Caller requeues after the full multi-group
	// emit if anything remains unacked.
	flushGroupRetry := func(all []string, force bool, attempts int) error {
		if attempts < 1 {
			attempts = 1
		}
		var lastErr error
		for i := 0; i < attempts; i++ {
			if i > 0 {
				// Brief backoff so a write-pressure blip can clear; keep total
				// Finish recovery under a few ms for local sockets.
				time.Sleep(time.Duration(i) * 2 * time.Millisecond)
			}
			lastErr = flushGroup(all, force, false)
			if lastErr != nil {
				return lastErr
			}
			if sw.LastOK() {
				return nil
			}
		}
		return lastErr
	}
	emitFrames := func(frames []string, force bool) error {
		if len(frames) == 0 {
			return nil
		}
		// Classify batch: pure thinking/text can coalesce; tools/terminal/message_start force.
		needImmediate := force
		if !needImmediate {
			for _, frame := range frames {
				if frameNeedsAnthropicImmediate(frame) {
					needImmediate = true
					break
				}
			}
		}
		if !needImmediate {
			// Pure thinking_delta / text_delta path — coalesce micro frames.
			for _, frame := range frames {
				pendingSSE = append(pendingSSE, frame...)
			}
			if !firstPayloadFlushed || len(pendingSSE) >= textCoalesceMax {
				return flushPendingSSE(true)
			}
			return nil
		}
		// Tools/terminal/start: drain coalesced thinking/text first (order: text then tools).
		if err := flushPendingSSE(true); err != nil {
			return err
		}
		// Keep each tool_use start+delta+stop in one Write+Flush. Flushing on
		// content_block_start alone leaves Claude Code with a half-open tool_use
		// block; a soft disconnect mid-group surfaces as "Tool use interrupted".
		// toolGap still applies BETWEEN tools (flush previous group first).
		//
		// Finish (message_stop present): emit tool groups and terminal SEPARATELY
		// with per-group Ack. A single giant multi-tool Write that soft-fails (or
		// short-writes) would either drop everything or Ack frames the client never
		// saw. Ordered per-tool Ack + terminal-last is what fully closes Claude Code.
		hasTerminal := false
		for _, f := range frames {
			if strings.Contains(f, "message_stop") {
				hasTerminal = true
				break
			}
		}
		// Split frames into ordered groups: non-tool prefix, each tool_use
		// start+delta+stop cluster, and trailing terminal (message_delta/stop).
		// Tool clusters start at content_block_start+tool_use.
		groups := make([][]string, 0, 4)
		cur := make([]string, 0, 4)
		flushCur := func() {
			if len(cur) == 0 {
				return
			}
			groups = append(groups, cur)
			cur = make([]string, 0, 4)
		}
		for _, frame := range frames {
			isToolStart := frameIsAnthropicToolStart(frame)
			isTerminalFrame := frameIsAnthropicTerminal(frame)
			if isToolStart {
				flushCur()
			} else if isTerminalFrame && len(cur) > 0 {
				// Do not mix tool/text frames with terminal in one Write.
				// Detect first terminal-ish frame after non-terminal content.
				joinedCur := strings.Join(cur, "")
				if strings.Contains(joinedCur, `"type":"tool_use"`) || strings.Contains(joinedCur, "content_block") ||
					strings.Contains(joinedCur, "message_start") {
					flushCur()
				}
			}
			cur = append(cur, frame)
		}
		flushCur()

		if hasTerminal {
			// Finish path: per-group retry (tools first, terminal last). Do NOT
			// requeue between groups — pending* must stay until Ack or until the
			// whole multi-group emit ends. Soft-fail of tool N leaves it unacked;
			// terminal success still Acks message_stop; outer Finish rebuilds
			// only the remaining unacked tools (toolsOnly when terminal already
			// delivered).
			var firstHard error
			for _, g := range groups {
				if err := flushGroupRetry(g, force, 3); err != nil {
					if firstHard == nil {
						firstHard = err
					}
					// Keep trying remaining groups (esp. message_stop) so Claude
					// Code can leave "running" even if one tool Write hard-failed.
				}
			}
			if assembler.HasUnackedTools() {
				// Roll back anything that never Ack'd so outer Finish can rebuild
				// complete start+delta+stop groups (and message_stop if needed).
				assembler.RequeueUnackedTools()
			}
			return firstHard
		}

		// Live mid-stream: same per-tool grouping; single attempt here — outer
		// Finish recovery re-emits anything left unacked.
		for i, g := range groups {
			// toolGap between live tool groups (not before the first).
			if toolGap > 0 && toolsEmitted > 0 {
				isToolGroup := false
				for _, f := range g {
					if strings.Contains(f, `"tool_use"`) {
						isToolGroup = true
						break
					}
				}
				if isToolGroup {
					if waitToolGap(r.Context(), toolGap) {
						sw.MarkSoftGone()
						if !envelopeOpen && !force {
							return r.Context().Err()
						}
					}
				}
			}
			// Live path: requeue on soft fail so Finish can rebuild complete groups.
			if err := flushGroup(g, force, true); err != nil {
				return err
			}
			for _, f := range g {
				if strings.Contains(f, `"tool_use"`) && strings.Contains(f, "content_block_start") {
					toolsEmitted++
					break
				}
			}
			_ = i
		}
		return nil
	}

	// Defer message_start until first real model payload. Early Start forced headers
	// open before hollow streams were known → empty 502 with no transparent failover.
	// Keepalive comments still flow via onIdle until envelope opens.
	var finish string
	var usage anthropic.Usage
	var openAIUsage map[string]any
	sawModel := false

	// Keepalive: prefer a slightly tighter interval for toolsRequested turns so
	// long thinking never sits near proxy idle cutoffs (~60s).
	keepalive := opts.Keepalive
	if keepalive <= 0 {
		keepalive = 4 * time.Second
	}
	if toolsRequested && keepalive > 3*time.Second {
		keepalive = 3 * time.Second
	}

	onIdle := func() error {
		// Drain coalesced thinking/text so clients are not stuck waiting for size.
		_ = flushPendingSSE(true)
		if probe.check(r.Context()) {
			sw.MarkSoftGone()
			if !envelopeOpen {
				return r.Context().Err()
			}
		}
		// Always force when holding tools/text so idle timer resets client-side even
		// under write-pressure (Claude Code multi-minute Update/Edit turns).
		if !envelopeOpen || !headersWritten {
			return nil
		}
		force := toolsRequested || assembler.NeedsClientKeepalive() || sw.SoftGone() || envelopeOpen
		return sw.Keepalive(anthropicKeepaliveFrame, DefaultKeepaliveInterval, force)
	}

	err := grok.ReadSSEWithIdle(body, keepalive, func(event grok.Event) error {
		// After envelope is open, soft-disconnect probes must not abort mid-stream;
		// terminal frames still need to land so Claude Code can leave "running".
		if probe.check(r.Context()) {
			sw.MarkSoftGone()
			if !envelopeOpen {
				return r.Context().Err()
			}
		}
		if event.Done {
			return nil
		}
		delta, err := proxy.ParseChatDelta(event.Data)
		if err != nil {
			// Ignore malformed keep-alive / partial frames; do not count as TTFT.
			return nil
		}
		if delta.FinishReason != nil {
			finish = stringValue(delta.FinishReason)
		}
		if delta.Usage != nil {
			if raw, ok := delta.Usage.(map[string]any); ok {
				openAIUsage = raw
				prompt, completion, total, cacheRead, cacheCreate, _ := postgres.UsageFromOpenAI(raw)
				usage = anthropic.Usage{PromptTokens: int(prompt), CompletionTokens: int(completion), TotalTokens: int(total), CacheReadTokens: int(cacheRead), CacheCreationTokens: int(cacheCreate)}
			}
		}
		hasContent := strings.TrimSpace(delta.Content) != ""
		hasReasoning := strings.TrimSpace(delta.Reasoning) != ""
		hasTools := len(delta.AnthropicToolDeltas()) > 0
		if hasContent || hasReasoning || hasTools {
			sawModel = true
			// First model payload: open Anthropic envelope (deferred message_start).
			if !envelopeOpen {
				if err := emitFrames(assembler.Start(0), true); err != nil {
					return err
				}
				envelopeOpen = true
			}
		}
		// Live path: do NOT force-flush pure thinking/text (coalesce). Tools/start
		// inside Feed still force via needImmediate classification in emitFrames.
		// Soft write errors are still swallowed by sseWriter after envelope open.
		wroteFrames := false
		if !envelopeOpen {
			// No model payload yet — do not write (would commit HTTP 200).
			return nil
		}
		feedFrames := assembler.Feed(delta.Content, delta.Reasoning, delta.AnthropicToolDeltas())
		if len(feedFrames) > 0 {
			if err := emitFrames(feedFrames, false); err != nil {
				return err
			}
			wroteFrames = true
		}
		// TTFT only after client-visible payload was successfully written (not mere
		// upstream delta arrival). Prevents admin rows with ttft>0 + empty 502 when
		// frames soft-failed / incomplete tools never emitted.
		if firstTokenMS == 0 && assembler.PayloadDelivered() {
			firstTokenMS = int(time.Since(started).Milliseconds())
			if firstTokenMS <= 0 {
				firstTokenMS = 1
			}
		}
		// Incomplete tool args / held text: throttled keepalive. Skip when we just
		// wrote real frames this tick (socket already warm).
		if assembler.NeedsClientKeepalive() && !wroteFrames && len(pendingSSE) == 0 {
			return sw.Keepalive(anthropicKeepaliveFrame, DefaultKeepaliveInterval, true)
		}
		return nil
	}, onIdle)

	// Drain any coalesced thinking/text before terminal Finish / empty checks.
	// Retry while soft-fail left bytes — lost text_delta is not rebuilt by Finish.
	for drain := 0; drain < 3 && len(pendingSSE) > 0; drain++ {
		_ = flushPendingSSE(true)
		if sw.LastOK() && len(pendingSSE) == 0 {
			break
		}
	}
	// End-of-stream TTFT backfill: coalesced content may only flush on drain, or
	// PayloadDelivered may flip after final Write. Never leave ok success with null TTFT
	// when the client clearly got text/tools (admin "首字时间无法获取").
	if firstTokenMS == 0 && (assembler.PayloadDelivered() || assembler.HasClientPayload() || sawModel || assembler.OutputRunes() > 0) {
		firstTokenMS = int(time.Since(started).Milliseconds())
		if firstTokenMS <= 0 {
			firstTokenMS = 1
		}
	}

	// fillAnthropicStreamUsage patches zero usage from assembler output when upstream
	// omitted the usage frame (soft-close / short tool turns → admin hollow success).
	// Stamps _estimated_* markers so recordAnthropicUsage can surface completion
	// in usage_estimated_fields (previously flags were dropped → "prompt,total" only).
	fillAnthropicStreamUsage := func(u map[string]any) map[string]any {
		hint := 0
		chars := 0
		if assembler != nil {
			hint = assembler.EstimateOutputTokens()
			chars = assembler.OutputRunes()
		}
		filled, flags := fillMissingUsage(u, nil, hint)
		if filled == nil {
			filled = map[string]any{}
		}
		if flags.EstimatedCompletion {
			filled["_estimated_completion"] = true
		}
		if flags.EstimatedPrompt {
			filled["_estimated_prompt"] = true
		}
		if flags.EstimatedTotal {
			filled["_estimated_total"] = true
		}
		if flags.Missing {
			filled["_usage_missing"] = true
		}
		if chars > 0 {
			filled["_streamer_output_chars"] = chars
		}
		if hint > 0 {
			filled["_streamer_estimate"] = hint
		}
		return filled
	}

	clientGone := sw.SoftGone() || probe.gone || errors.Is(err, r.Context().Err()) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) || isSoftClientWriteError(err)
	// Real payload = model deltas OR assembler emitted/held content/tools.
	// Early message_start alone must NOT count as success (admin showed ok=true,
	// tokens=0, ttft=null for ~1s intermittent empties).
	hasPayload := sawModel || assembler.HasClientPayload() || assembler.HasPendingTools() || assembler.HasHeldContent()
	// Upstream read/error mid-stream AFTER the client already has content/tools must
	// soft-close the Anthropic envelope (Finish only). Emitting event:error here is
	// what Claude Code surfaces as "API Error: Server error mid-response / response
	// above may be incomplete" even when most of the turn already landed.
	upstreamMidError := err != nil && !clientGone
	if upstreamMidError && !hasPayload {
		msg, errType := anthropicErrorFromCause(err)
		_ = emitFrames(anthropic.TerminalError(msg, errType), true)
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, err
	}
	if !hasPayload {
		empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		// Not committed → allow serveMessages multi-open to try another account.
		if !headersWritten {
			return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, empty
		}
		if !envelopeOpen {
			_ = emitFrames(assembler.Start(0), true)
			envelopeOpen = true
		}
		if clientGone {
			_ = emitFrames(assembler.Finish("stop", usage), true)
			return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, empty
		}
		msg, errType := anthropicErrorFromCause(empty)
		_ = emitFrames(anthropic.TerminalError(msg, errType), true)
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, empty
	}
	if finish == "" {
		finish = "stop"
	}
	// Always try terminal frames after any payload (including write-error soft disconnect
	// and mid-stream upstream errors) so Claude Code never hangs on a half-open
	// tool_use as "Tool use interrupted" / "Server error mid-response".
	// Finish requeues unacked tools first; emitFrames writes tool groups then
	// message_delta/stop as separate groups with per-group retry. Soft write of a
	// group Requeues (clears pending) — NeedsFinishRetry still sees
	// !TerminalDelivered / pending tools and forces another Finish.
	if termErr := emitFrames(assembler.Finish(finish, usage), true); termErr != nil && !clientGone && !upstreamMidError {
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, termErr
	}
	// Soft-fail recovery: more Finish rebuilds (tools + message_stop). Each rebuild
	// re-emits only unacked/pending work; Ack'd tools are skipped. Requeue also
	// UnackTerminal when tools need re-emit so tool_use never follows message_stop
	// ("Tool use interrupted").
	// More Finish rebuilds for stubborn soft short-writes (tool start landed,
	// stop frame still pending). Each rebuild re-emits only unacked work.
	for attempt := 0; attempt < 6 && assembler.NeedsFinishRetry(); attempt++ {
		_ = emitFrames(assembler.Finish(finish, usage), true)
	}
	// After Finish, incomplete-only tools (never started) + no text is empty —
	// even if sawModel was true from upstream tool deltas. That was the intermittent
	// ok=true tokens=0 TTFT>0 path Claude Code reports as "Tool use interrupted".
	if !assembler.HasClientPayload() {
		empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		if !assembler.TerminalDelivered() {
			_ = emitFrames(anthropic.TerminalError(empty.Error(), "api_error"), true)
		}
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, empty
	}
	if assembler.ClientDeliveryOK() {
		if clientGone || upstreamMidError {
			// Soft terminal: client left, or upstream dropped after FULL delivery.
			return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, nil
		}
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, err
	}
	// Recovery exhausted. Distinguish half-open tools vs delivered-payload soft-close
	// vs incomplete-only empty. Emitting TerminalError after a real tool/text delivery
	// is what Claude Code surfaces as intermittent "Tool use interrupted".
	if assembler.HalfOpenTools() {
		empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		if !assembler.TerminalDelivered() {
			_ = emitFrames(anthropic.TerminalError(empty.Error(), "api_error"), true)
		}
		if upstreamMidError && err != nil {
			return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, err
		}
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, empty
	}
	if assembler.PayloadDelivered() {
		// Soft-close: client already has usable output (tools Ack'd / content written).
		// Do not TerminalError over a real delivery when only message_stop soft-failed.
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, nil
	}
	empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
	if !assembler.TerminalDelivered() {
		_ = emitFrames(anthropic.TerminalError(empty.Error(), "api_error"), true)
	}
	if upstreamMidError && err != nil {
		return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, err
	}
	return fillAnthropicStreamUsage(openAIUsage), firstTokenMS, empty
}

// isSoftClientWriteError reports client-side stream aborts that should still run
// Finish/Complete so Claude Code / Codex leave "running" instead of hanging as
// "Tool use interrupted" on a half-open tool block.
func isSoftClientWriteError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"broken pipe",
		"connection reset",
		"connection refused",
		"use of closed network connection",
		"stream closed",
		"client disconnected",
		"i/o timeout",
		"write: connection",
		"short write",
		"wsasend",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

type disconnectProbe struct {
	hitsNeeded int
	minSpan    time.Duration
	hits       int
	firstHit   time.Time
	gone       bool
}

func newDisconnectProbe(hitsNeeded int, minSpan time.Duration) *disconnectProbe {
	if hitsNeeded < 1 {
		hitsNeeded = 1
	}
	return &disconnectProbe{hitsNeeded: hitsNeeded, minSpan: minSpan}
}

func (p *disconnectProbe) check(ctx context.Context) bool {
	if p == nil {
		return false
	}
	if p.gone {
		return true
	}
	select {
	case <-ctx.Done():
		now := time.Now()
		if p.hits == 0 {
			p.firstHit = now
		}
		p.hits++
		if p.hits >= p.hitsNeeded && (p.minSpan <= 0 || now.Sub(p.firstHit) >= p.minSpan) {
			p.gone = true
			return true
		}
		return false
	default:
		p.hits = 0
		p.firstHit = time.Time{}
		return false
	}
}

// isRetryableUpstreamOpenErr reports open-time failures worth a fresh OpenStream
// on a different account (empty/hollow, transient 5xx, rate limit). Auth/billing
// hard fails should not burn more accounts in a tight loop.
func isRetryableUpstreamOpenErr(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "empty model output") ||
		strings.Contains(msg, "no content/tool_calls") ||
		strings.Contains(msg, "no client-visible content") {
		return true
	}
	var ue *grok.UpstreamError
	if errors.As(err, &ue) && ue != nil {
		if ue.Status == http.StatusTooManyRequests || ue.Status >= 500 {
			return true
		}
		if ue.Status == 0 {
			return true
		}
		body := strings.ToLower(ue.Body)
		if strings.Contains(body, "empty") || strings.Contains(body, "rate") || strings.Contains(body, "overloaded") {
			return true
		}
	}
	return strings.Contains(msg, "timeout") || strings.Contains(msg, "temporar") || strings.Contains(msg, "connection reset")
}

func recordAnthropicUsage(r *http.Request, options Options, apiKey *auth.APIKeyRecord, accountID, model string, stream bool, ok bool, status int, started time.Time, usage any, cause error, ttftMS int, requestBody map[string]any) {
	usageMap, _ := usage.(map[string]any)
	// Prefer streamer completion estimate already stamped by fillAnthropicStreamUsage.
	hint := 0
	preEstCompletion := false
	preEstPrompt := false
	preEstTotal := false
	preMissing := false
	streamerChars := 0
	streamerEst := 0
	if usageMap != nil {
		if usageMap["_estimated_completion"] == true {
			preEstCompletion = true
		}
		if usageMap["_estimated_prompt"] == true {
			preEstPrompt = true
		}
		if usageMap["_estimated_total"] == true {
			preEstTotal = true
		}
		if usageMap["_usage_missing"] == true {
			preMissing = true
		}
		streamerChars = int(anyToInt64(usageMap["_streamer_output_chars"]))
		streamerEst = int(anyToInt64(usageMap["_streamer_estimate"]))
		if _, c, _, _, _, _ := postgres.UsageFromOpenAI(usageMap); c > 0 {
			hint = int(c)
		}
		if hint <= 0 && streamerEst > 0 {
			hint = streamerEst
		}
	}
	if ok && hint <= 0 && ttftMS > 0 {
		hint = 1
	}
	// Soft-close / omitted upstream usage: fill prompt from request body + streamer hint.
	filled, flags := fillMissingUsage(usageMap, requestBody, hint)
	if preEstCompletion {
		flags.EstimatedCompletion = true
	}
	if preEstPrompt {
		flags.EstimatedPrompt = true
	}
	if preEstTotal {
		flags.EstimatedTotal = true
	}
	if preMissing {
		flags.Missing = true
	}
	if ok {
		usageMap = filled
		_, c, _, _, _, _ := postgres.UsageFromOpenAI(usageMap)
		// Real delivery with still-zero completion: lift to streamer/floor.
		if c == 0 && ttftMS > 0 {
			forceHint := hint
			if forceHint < 1 {
				forceHint = 1
			}
			filled2, flags2 := fillMissingUsage(usageMap, requestBody, forceHint)
			usageMap = filled2
			flags = flags2
			flags.EstimatedCompletion = true
			flags.Missing = true
			_, c, _, _, _, _ = postgres.UsageFromOpenAI(usageMap)
		}
		// Tiny floor completion while streamer saw more → lift (same as Responses).
		if streamerEst > int(c) && c <= 2 && streamerEst >= 4 {
			filled2, flags2 := fillMissingUsage(usageMap, requestBody, streamerEst)
			usageMap = filled2
			flags = flags2
			flags.EstimatedCompletion = true
			flags.Missing = true
		}
	} else if usageMap == nil {
		usageMap = map[string]any{}
		flags = usageFillFlags{}
	}
	if usageMap != nil {
		for _, k := range []string{"_estimated_completion", "_estimated_prompt", "_estimated_total", "_usage_missing", "_streamer_output_chars", "_streamer_estimate"} {
			delete(usageMap, k)
		}
	}
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usageMap)
	streamValue := stream
	var apiKeyID string
	if apiKey != nil {
		apiKeyID = apiKey.ID
	}
	var errText string
	if cause != nil {
		errText = cause.Error()
	}
	latency := int(time.Since(started).Milliseconds())
	// Successful turns with output must not leave admin TTFT empty ("首字时间无法获取").
	// Prefer stream-measured firstTokenMS; if missing, use a lower-bound estimate.
	if ok && ttftMS <= 0 && (completion > 0 || total > prompt) {
		if latency > 2 {
			ttftMS = latency / 2
			if ttftMS < 1 {
				ttftMS = 1
			}
			// Cap estimate below full latency so it is not mistaken for total duration.
			if ttftMS >= latency {
				ttftMS = latency - 1
				if ttftMS < 1 {
					ttftMS = 1
				}
			}
		} else {
			ttftMS = 1
		}
	}
	var ttftPtr *int
	if ttftMS > 0 {
		v := ttftMS
		ttftPtr = &v
	}
	// Fire-and-forget with longer timeout - usage recording should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if options.Store != nil {
			if _, _, err := options.Store.RecordUsage(ctx, postgres.UsageRecord{
				RequestID:           requestID(r),
				Implementation:      "go",
				APIKeyID:            apiKeyID,
				AccountID:           accountID,
				Model:               model,
				Protocol:            "anthropic",
				Path:                r.URL.Path,
				Stream:              &streamValue,
				OK:                  ok,
				PromptTokens:        prompt,
				CompletionTokens:    completion,
				TotalTokens:         total,
				CacheReadTokens:     cacheRead,
				CacheCreationTokens: cacheCreate,
				ReasoningTokens:     reasoning,
				ClientIP:            clientIP(r),
				UserAgent:           r.UserAgent(),
				StatusCode:          &status,
				LatencyMS:           &latency,
				TTFTMS:              ttftPtr,
				Error:               errText,
				Detail: func() map[string]any {
					d := usageDetail("go_messages", requestBody, ttftMS, latency)
					if !ok && (prompt+completion+total) == 0 {
						d["empty_payload"] = true
						if cause != nil {
							d["message"] = cause.Error()
						}
					}
					if ttftMS <= 0 {
						d["ttft_missing"] = true
					}
					if streamerChars > 0 {
						d["streamer_output_chars"] = streamerChars
					}
					if streamerEst > 0 {
						d["streamer_estimate_tokens"] = streamerEst
					}
					if ok {
						// Propagate streamer completion estimate into usage_estimated_fields.
						if preEstCompletion {
							flags.EstimatedCompletion = true
							flags.Missing = true
						}
						flags.apply(d)
					}
					return d
				}(),
			}); err != nil {
				slog.Warn("record usage failed", "error", err, "account_id", accountID, "model", model, "ok", ok)
			}
		}
		recordRedisUsage(options, apiKeyID, accountID, model, prompt, completion, total, cacheRead, ok)
	}()
}

func anthropicCompletionParts(payload map[string]any) (content, reasoning, finish string, usage anthropic.Usage, calls []anthropic.ToolCall) {
	usage = anthropic.Usage{}
	if rawUsage, ok := payload["usage"].(map[string]any); ok {
		prompt, completion, total, cacheRead, cacheCreate, _ := postgres.UsageFromOpenAI(rawUsage)
		usage = anthropic.Usage{PromptTokens: int(prompt), CompletionTokens: int(completion), TotalTokens: int(total), CacheReadTokens: int(cacheRead), CacheCreationTokens: int(cacheCreate)}
	}
	choices, _ := payload["choices"].([]map[string]any)
	if len(choices) == 0 {
		return "", "", "stop", usage, nil
	}
	finish = stringValue(choices[0]["finish_reason"])
	message, _ := choices[0]["message"].(map[string]any)
	content = stringValue(message["content"])
	reasoning = firstNonEmpty(stringValue(message["reasoning_content"]), stringValue(message["reasoning"]))
	if items, ok := message["tool_calls"].([]map[string]any); ok {
		for _, item := range items {
			fn, _ := item["function"].(map[string]any)
			calls = append(calls, anthropic.ToolCall{ID: stringValue(item["id"]), Name: stringValue(fn["name"]), Arguments: stringValue(fn["arguments"])})
		}
	} else if itemsAny, ok := message["tool_calls"].([]any); ok {
		// Some decode paths yield []any rather than []map[string]any.
		for _, rawItem := range itemsAny {
			item, _ := rawItem.(map[string]any)
			if item == nil {
				continue
			}
			fn, _ := item["function"].(map[string]any)
			calls = append(calls, anthropic.ToolCall{ID: stringValue(item["id"]), Name: stringValue(fn["name"]), Arguments: stringValue(fn["arguments"])})
		}
	}
	if fn, ok := message["function_call"].(map[string]any); ok {
		calls = append(calls, anthropic.ToolCall{Name: stringValue(fn["name"]), Arguments: stringValue(fn["arguments"])})
	}
	return content, reasoning, finish, usage, calls
}

func allowedAnthropicToolNames(body map[string]any) []string {
	if body == nil {
		return nil
	}
	items, _ := body["tools"].([]any)
	if items == nil {
		// Some clients send tools as typed maps after decode.
		if typed, ok := body["tools"].([]map[string]any); ok {
			items = make([]any, len(typed))
			for i, t := range typed {
				items[i] = t
			}
		}
	}
	out := make([]string, 0, len(items))
	seen := map[string]bool{}
	for _, item := range items {
		tool, _ := item.(map[string]any)
		if tool == nil {
			continue
		}
		// OpenAI: {"type":"function","function":{"name":"Edit",...}}
		// Anthropic: {"name":"Edit","input_schema":{...}}
		fn, _ := tool["function"].(map[string]any)
		name := stringValue(fn["name"])
		if name == "" {
			name = stringValue(tool["name"])
		}
		if name == "" || seen[name] {
			continue
		}
		seen[name] = true
		out = append(out, name)
	}
	return out
}

func newAnthropicMessageID() string {
	buf := make([]byte, 12)
	if _, err := rand.Read(buf); err != nil {
		return "msg_go_" + strconv.FormatInt(time.Now().UnixNano(), 10)
	}
	return "msg_" + hex.EncodeToString(buf)
}

func positiveNumber(value any) bool {
	switch v := value.(type) {
	case int:
		return v >= 1
	case int64:
		return v >= 1
	case float64:
		return v >= 1
	case json.Number:
		n, err := v.Int64()
		return err == nil && n >= 1
	default:
		return false
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

// writeAnthropicProxyError maps upstream/proxy failures onto Anthropic JSON errors
// with useful status codes (429/401/503) instead of always 502 + raw Error() text.
func writeAnthropicProxyError(w http.ResponseWriter, err error) {
	status := http.StatusBadGateway
	message := "request failed"
	errorType := "api_error"
	if err != nil {
		message = strings.TrimSpace(err.Error())
	}
	if errors.Is(err, pool.ErrNoEligibleAccounts) {
		status = http.StatusServiceUnavailable
		message = "No eligible accounts available. All accounts may be in cooldown or disabled."
		errorType = "api_error"
		writeAnthropicError(w, status, message, errorType)
		return
	}
	var upstream *grok.UpstreamError
	if errors.As(err, &upstream) && upstream != nil {
		status = mapUpstreamStatusToAnthropic(upstream.Status)
		if strings.TrimSpace(upstream.Body) != "" {
			message = preferAnthropicErrorBody(upstream.Body)
		}
		errorType = anthropicErrorTypeForStatus(status)
		writeAnthropicError(w, status, message, errorType)
		return
	}
	msg, typ := anthropicErrorFromCause(err)
	if unwrappedStatus, unwrappedBody, ok := unwrapAnthropicUpstreamMessage(message); ok {
		if unwrappedStatus > 0 {
			status = mapUpstreamStatusToAnthropic(unwrappedStatus)
		}
		if strings.TrimSpace(unwrappedBody) != "" {
			msg = unwrappedBody
		}
		typ = anthropicErrorTypeForStatus(status)
	}
	writeAnthropicError(w, status, msg, typ)
}

func writeAnthropicError(w http.ResponseWriter, status int, message, errorType string) {
	// Prefer structured upstream body/status when callers pass cause.Error() wrappers
	// like "upstream status 429: {...}" so Claude clients see the real quota/rate text.
	if status <= 0 {
		status = http.StatusBadGateway
	}
	if unwrappedStatus, unwrappedBody, ok := unwrapAnthropicUpstreamMessage(message); ok {
		if unwrappedStatus > 0 {
			status = mapUpstreamStatusToAnthropic(unwrappedStatus)
		}
		if strings.TrimSpace(unwrappedBody) != "" {
			message = unwrappedBody
		}
		if errorType == "" || errorType == "api_error" {
			errorType = anthropicErrorTypeForStatus(status)
		}
	}
	if errorType == "" {
		errorType = anthropicErrorTypeForStatus(status)
	}
	if strings.TrimSpace(message) == "" {
		message = "request failed"
	}
	// Anthropic wire shape: { "type":"error", "error": { "type":"...", "message":"..." } }
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}})
}

func anthropicErrorTypeForStatus(status int) string {
	switch status {
	case http.StatusUnauthorized:
		return "authentication_error"
	case http.StatusForbidden:
		return "permission_error"
	case http.StatusNotFound:
		return "not_found_error"
	case http.StatusTooManyRequests:
		return "rate_limit_error"
	case http.StatusBadRequest:
		return "invalid_request_error"
	case http.StatusRequestEntityTooLarge:
		return "request_too_large"
	case http.StatusServiceUnavailable, http.StatusGatewayTimeout, http.StatusBadGateway:
		return "api_error"
	default:
		return "api_error"
	}
}

// mapUpstreamStatusToAnthropic keeps client-facing status useful for Claude SDKs
// without inventing codes Anthropic clients reject.
func mapUpstreamStatusToAnthropic(status int) int {
	switch {
	case status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable:
		return status
	case status == http.StatusUnauthorized || status == http.StatusForbidden || status == http.StatusNotFound:
		return status
	case status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge:
		return status
	case status >= 500:
		return http.StatusBadGateway
	case status >= 400:
		return status
	default:
		return http.StatusBadGateway
	}
}

// unwrapAnthropicUpstreamMessage peels "upstream status N: body" wrappers and
// nested JSON error objects so tool-loop clients see the human/quota message.
func unwrapAnthropicUpstreamMessage(message string) (status int, body string, ok bool) {
	text := strings.TrimSpace(message)
	if text == "" {
		return 0, "", false
	}
	lower := strings.ToLower(text)
	for _, p := range []string{"upstream status ", "status "} {
		if !strings.HasPrefix(lower, p) {
			continue
		}
		rest := strings.TrimSpace(text[len(p):])
		i := 0
		for i < len(rest) && rest[i] >= '0' && rest[i] <= '9' {
			status = status*10 + int(rest[i]-'0')
			i++
		}
		if status <= 0 || i == 0 {
			return 0, "", false
		}
		rest = strings.TrimSpace(rest[i:])
		if strings.HasPrefix(rest, ":") {
			rest = strings.TrimSpace(rest[1:])
		}
		return status, preferAnthropicErrorBody(rest), true
	}
	// Bare JSON body without status prefix.
	if text[0] == '{' {
		if cleaned := preferAnthropicErrorBody(text); cleaned != text {
			return 0, cleaned, true
		}
	}
	return 0, "", false
}

func preferAnthropicErrorBody(body string) string {
	body = strings.TrimSpace(body)
	if body == "" || body[0] != '{' {
		return body
	}
	var raw map[string]any
	if json.Unmarshal([]byte(body), &raw) != nil {
		return body
	}
	// {"error":"..."} or {"error":{"message":"..."}} or {"message":"..."}
	switch e := raw["error"].(type) {
	case string:
		if strings.TrimSpace(e) != "" {
			return strings.TrimSpace(e)
		}
	case map[string]any:
		if m, ok := e["message"].(string); ok && strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m)
		}
		if m, ok := e["error"].(string); ok && strings.TrimSpace(m) != "" {
			return strings.TrimSpace(m)
		}
	}
	if m, ok := raw["message"].(string); ok && strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m)
	}
	if m, ok := raw["detail"].(string); ok && strings.TrimSpace(m) != "" {
		return strings.TrimSpace(m)
	}
	return body
}

// anthropicErrorFromCause maps transport/upstream failures onto Anthropic
// terminal error type + human message for SSE event:error frames.
func anthropicErrorFromCause(err error) (message, errorType string) {
	if err == nil {
		return "request failed", "api_error"
	}
	message = strings.TrimSpace(err.Error())
	errorType = "api_error"
	var upstream *grok.UpstreamError
	if errors.As(err, &upstream) && upstream != nil {
		if strings.TrimSpace(upstream.Body) != "" {
			message = preferAnthropicErrorBody(upstream.Body)
		}
		errorType = anthropicErrorTypeForStatus(mapUpstreamStatusToAnthropic(upstream.Status))
		return firstNonEmptyStr(message, err.Error()), errorType
	}
	if status, body, ok := unwrapAnthropicUpstreamMessage(message); ok {
		if strings.TrimSpace(body) != "" {
			message = body
		}
		if status > 0 {
			errorType = anthropicErrorTypeForStatus(mapUpstreamStatusToAnthropic(status))
		}
	}
	// Empty upstream 200s are server-side transport issues for Claude clients.
	low := strings.ToLower(message)
	if strings.Contains(low, "empty model output") || strings.Contains(low, "no content/tool_calls") {
		errorType = "api_error"
	}
	if errors.Is(err, pool.ErrNoEligibleAccounts) {
		errorType = "api_error"
		if message == "" {
			message = "No eligible accounts available. All accounts may be in cooldown or disabled."
		}
	}
	return firstNonEmptyStr(message, "request failed"), errorType
}

func serveAdminStatus(w http.ResponseWriter, r *http.Request, options Options, protected bool) {
	if !options.AdminReadEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin read routes are not enabled"})
		return
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return
	}
	if protected {
		if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
			writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
			return
		}
	} else {
		// Unprotected /admin/api/status is polled every ~8s by the admin UI.
		// Serve a short-lived snapshot so reverse proxies never hit 502 on
		// 7k-account PoolSummary scans.
		statusPayloadMu.Lock()
		if statusPayloadCache != nil && time.Since(statusPayloadAt) < 3*time.Second {
			out := cloneStringAnyMap(statusPayloadCache)
			statusPayloadMu.Unlock()
			writeJSON(w, http.StatusOK, out)
			return
		}
		statusPayloadMu.Unlock()
	}
	store := options.Store
	accountCount, modelCount := int64(0), int64(0)
	keyStats := map[string]any{"total": int64(0), "enabled": int64(0), "disabled": int64(0), "total_requests": int64(0), "auth_required": false, "legacy_env_key": false}
	pool := postgres.PoolSummary{Mode: "round_robin", Source: "postgres"}
	if store != nil {
		if n, err := store.CountAccounts(r.Context()); err == nil {
			accountCount = n
		}
		if n, err := store.CountModels(r.Context(), false); err == nil {
			modelCount = n
		}
		if options.APIKeys != nil {
			if required, err := options.APIKeys.AuthRequired(r.Context()); err == nil {
				if stats, err := store.KeyStats(r.Context(), strings.TrimSpace(options.Config.LegacyAPIKey) != "", required); err == nil {
					keyStats = stats
				}
			}
		}
		if got, err := store.PoolSummary(r.Context()); err == nil {
			pool = got
		}
	}
	accounts := map[string]any{"account_count": accountCount, "active_count": pool.Live}
	setupNeeded := false
	if store != nil {
		if has, err := store.HasAdminPassword(r.Context()); err == nil {
			setupNeeded = !has
		}
	}
	// 构建前端兼容的数据库状态
	redisEnabled := options.Redis != nil && options.Redis.Enabled()
	redisConfigured := strings.TrimSpace(options.Config.RedisURL) != ""
	pgEnabled := store != nil
	pgConfigured := strings.TrimSpace(options.Config.DatabaseURL) != ""

	payload := map[string]any{
		"ok":           true,
		"setup_needed": setupNeeded,
		"version":      buildinfo.Version,
		"store": map[string]any{
			"backend": "hybrid",
			"postgres": map[string]any{
				"ok":         pgEnabled,
				"enabled":    pgEnabled,
				"configured": pgConfigured,
			},
			"redis": map[string]any{
				"ok":         redisEnabled,
				"enabled":    redisEnabled,
				"configured": redisConfigured,
			},
			"workers": options.Config.Workers,
		},
		"host":                 options.Config.Host,
		"port":                 options.Config.Port,
		"upstream":             options.Config.UpstreamBase,
		"default_model":        options.runtimeConfig().DefaultModel,
		"require_api_key_mode": options.Config.RequireAPIKey,
		"api_base":             publicAPIBase(r, options.Config.Port),
		"credentials_ok":       pool.Live > 0,
		"credentials_email":    nil,
		"account_mode":         pool.Mode,
		"accounts":             accounts,
		"pool": map[string]any{
			"mode": pool.Mode, "total": pool.Total, "live": pool.Live, "rotatable": pool.Rotatable,
			"enabled": pool.Enabled, "in_cooldown": pool.InCooldown, "cooldown_stacks": pool.CooldownStacks, "quota_disabled": pool.QuotaDisabled,
			"model_blocked": pool.ModelBlocked, "expired": pool.Expired, "disabled": pool.Disabled, "source": pool.Source,
		},
		"keys":                  keyStats,
		"models_count":          modelCount,
		"settings":              map[string]any{},
		"token_maintainer":      serviceStatus(options.Maintainer, options),
		"model_health":          serviceStatus(options.ModelHealth, options),
		"conversation_affinity": map[string]any{"enabled": options.AffinityStore != nil, "implementation": "go"},
		"registration":          map[string]any{"mode": options.Config.RegistrationMode, "external": true, "available": options.Config.RegistrationServiceURL != ""},
		"usage":                 usageLightSnapshot(r.Context(), options),
		"leader":                leaderStatus(r.Context(), options),
		"redis":                 map[string]any{"enabled": redisEnabled, "prefix": options.Config.RedisPrefix},
		"stream":                streamSnapshot(),
		// Never block /status on a live upstream probe — only attach a recent cache.
		"upstream_status": cachedUpstreamStatus(),
	}
	if protected {
		payload["credentials"] = map[string]any{"email": nil, "active_count": pool.Live, "account_count": accountCount, "ok": pool.Live > 0}
		payload["models"] = modelCatalog(options).PublicModels(r.Context())
		payload["account_modes"] = []string{"round_robin", "random", "least_used"}
		payload["full"] = false
	} else {
		statusPayloadMu.Lock()
		statusPayloadCache = cloneStringAnyMap(payload)
		statusPayloadAt = time.Now()
		statusPayloadMu.Unlock()
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveAdminModels(w http.ResponseWriter, r *http.Request, options Options) {
	if !options.AdminReadEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin read routes are not enabled"})
		return
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"object":        "list",
		"data":          modelCatalog(options).PublicModels(r.Context()),
		"default_model": options.runtimeConfig().DefaultModel,
		"storage":       "postgres",
		"meta":          map[string]any{},
	})
}

func serveAdminKeys(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	keys, err := options.Store.ListAPIKeys(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	public := make([]map[string]any, 0, len(keys))
	for _, key := range keys {
		public = append(public, key.PublicMap())
	}
	required := false
	if options.APIKeys != nil {
		required, _ = options.APIKeys.AuthRequired(r.Context())
	}
	// Stats are best-effort: a stats failure must not blank the key list UI.
	stats, err := options.Store.KeyStats(r.Context(), strings.TrimSpace(options.Config.LegacyAPIKey) != "", required)
	if err != nil {
		stats = map[string]any{
			"total":          int64(len(public)),
			"enabled":        int64(0),
			"disabled":       int64(0),
			"total_requests": int64(0),
			"auth_required":  required,
			"legacy_env_key": strings.TrimSpace(options.Config.LegacyAPIKey) != "",
			"error":          err.Error(),
		}
		for _, k := range public {
			if en, _ := k["enabled"].(bool); en {
				stats["enabled"] = stats["enabled"].(int64) + 1
			} else {
				stats["disabled"] = stats["disabled"].(int64) + 1
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"keys": public, "stats": stats, "store_source": "postgres", "store_backend": "postgres"})
}

func serveAdminAccounts(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	query := r.URL.Query()
	if truthy(query.Get("summary")) {
		count, _ := options.Store.CountAccounts(r.Context())
		pool, _ := options.Store.PoolSummary(r.Context())
		writeJSON(w, http.StatusOK, map[string]any{
			"account_count": count,
			"active_count":  pool.Live,
			"pool":          pool,
			"page":          1,
			"page_size":     0,
			"total":         count,
			"total_pages":   1,
			"q":             strings.TrimSpace(query.Get("q")),
			"sort":          query.Get("sort"),
		})
		return
	}
	page := intQuery(query.Get("page"), 1)
	pageSize := intQuery(query.Get("page_size"), 25)
	filter := postgres.AccountListFilter{
		Query:  query.Get("q"),
		Sort:   query.Get("sort"),
		Status: query.Get("status"),
	}
	if raw := strings.TrimSpace(strings.ToLower(query.Get("has_sso"))); raw != "" {
		v := raw == "1" || raw == "true" || raw == "yes" || raw == "on"
		// distinguish false explicitly
		if raw == "0" || raw == "false" || raw == "no" || raw == "off" {
			v = false
			filter.HasSSO = &v
		} else if raw == "1" || raw == "true" || raw == "yes" || raw == "on" {
			v = true
			filter.HasSSO = &v
		}
	}
	if truthy(query.Get("ids_only")) || truthy(query.Get("ids")) {
		filter.IDsOnly = true
		// return up to 20k matching ids for "筛选全选"
		if pageSize < 1000 {
			pageSize = 20000
		}
		page = 1
	}
	result, err := options.Store.ListAccountSummariesFiltered(r.Context(), page, pageSize, filter)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	// Ensure frontend can re-sync filter chips.
	if result.Status == "" {
		result.Status = strings.TrimSpace(query.Get("status"))
	}
	// Always include pool as a plain map so chips/overview match DB even if
	// AccountList.Pool pointer encoding is stripped by proxies/older clients.
	payload := map[string]any{
		"accounts":       result.Accounts,
		"total":          result.Total,
		"page":           result.Page,
		"page_size":      result.PageSize,
		"total_pages":    result.TotalPages,
		"q":              result.Query,
		"sort":           result.Sort,
		"status":         result.Status,
		"store_source":   result.StoreSource,
		"store_backend":  result.StoreBackend,
		"auth_file_role": result.AuthFileRole,
	}
	if result.HasSSO != nil {
		payload["has_sso"] = *result.HasSSO
	}
	if len(result.IDs) > 0 {
		payload["ids"] = result.IDs
	}
	poolSum := result.Pool
	if poolSum == nil {
		if got, err := options.Store.PoolSummary(r.Context()); err == nil {
			poolSum = &got
		}
	}
	if poolSum != nil {
		payload["pool"] = map[string]any{
			"mode":            poolSum.Mode,
			"total":           poolSum.Total,
			"live":            poolSum.Live,
			"rotatable":       poolSum.Rotatable,
			"enabled":         poolSum.Enabled,
			"in_cooldown":     poolSum.InCooldown,
			"cooldown_stacks": poolSum.CooldownStacks,
			"quota_disabled":  poolSum.QuotaDisabled,
			"model_blocked":   poolSum.ModelBlocked,
			"expired":         poolSum.Expired,
			"disabled":        poolSum.Disabled,
			"source":          poolSum.Source,
		}
	}
	writeJSON(w, http.StatusOK, payload)
}

// Shared registration HTTP client: admin UI polls batch/session every ~150–400ms.
// Reusing one Transport avoids dial/TLS thrash and cuts log-refresh latency under load.
var (
	regHTTPClientOnce sync.Once
	regHTTPClient     *http.Client
	regClientMu       sync.Mutex
	regClientCache    *regclient.Client
	regClientBase     string
	regClientToken    string
)

func sharedRegistrationHTTP() *http.Client {
	regHTTPClientOnce.Do(func() {
		// Keep under browser REG_POLL_TIMEOUT_MS (~900ms) so a stuck sidecar never freezes the log UI.
		regHTTPClient = &http.Client{
			Timeout: 750 * time.Millisecond,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 250 * time.Millisecond}).DialContext,
				MaxIdleConns:          128,
				MaxIdleConnsPerHost:   64,
				MaxConnsPerHost:       64,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 600 * time.Millisecond,
				ForceAttemptHTTP2:     true,
			},
		}
	})
	return regHTTPClient
}

// sharedRegistrationBulkHTTP is isolated from high-frequency status polling.
// A durable list with thousands of sessions can take seconds to serialize and
// transfer even over the loopback sidecar connection.
var (
	regHTTPBulkClientOnce sync.Once
	regHTTPBulkClient     *http.Client
)

func sharedRegistrationBulkHTTP() *http.Client {
	regHTTPBulkClientOnce.Do(func() {
		regHTTPBulkClient = &http.Client{
			Timeout: 30 * time.Second,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: time.Second}).DialContext,
				MaxIdleConns:          8,
				MaxIdleConnsPerHost:   4,
				MaxConnsPerHost:       4,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 15 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		}
	})
	return regHTTPBulkClient
}

// sharedRegistrationLongHTTP is used for long-running POST operations
// (job creation, device login, SSO import) that may take ~30s+ due to
// turnstile solving and email roundtrips (e.g. Cloud Mail registration
// takes ~26s end-to-end). The short sharedRegistrationHTTP (750ms) would
// prematurely abort these, causing "timeout awaiting response headers".
var (
	regHTTPLongClientOnce sync.Once
	regHTTPLongClient     *http.Client
)

func sharedRegistrationLongHTTP() *http.Client {
	regHTTPLongClientOnce.Do(func() {
		regHTTPLongClient = &http.Client{
			Timeout: 180 * time.Second,
			Transport: &http.Transport{
				DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
				MaxIdleConns:          32,
				MaxIdleConnsPerHost:   16,
				MaxConnsPerHost:       16,
				IdleConnTimeout:       90 * time.Second,
				ResponseHeaderTimeout: 120 * time.Second,
				ForceAttemptHTTP2:     true,
			},
		}
	})
	return regHTTPLongClient
}

func registrationClient(options Options) *regclient.Client {
	base := strings.TrimSpace(options.RegistrationURL)
	if base == "" {
		base = strings.TrimSpace(options.Config.RegistrationServiceURL)
	}
	token := strings.TrimSpace(options.RegistrationToken)
	if token == "" {
		token = strings.TrimSpace(options.Config.RegistrationToken)
	}
	if base == "" {
		return nil
	}
	regClientMu.Lock()
	defer regClientMu.Unlock()
	if regClientCache != nil && regClientBase == base && regClientToken == token {
		return regClientCache
	}
	regClientBase = base
	regClientToken = token
	regClientCache = &regclient.Client{
		BaseURL:  base,
		Token:    token,
		HTTP:     sharedRegistrationHTTP(),
		HTTPLong: sharedRegistrationLongHTTP(),
		HTTPBulk: sharedRegistrationBulkHTTP(),
	}
	return regClientCache
}

func requireAdminReadWrite(w http.ResponseWriter, r *http.Request, options Options, write bool) bool {
	if write {
		if !adminWriteAllowed(w, r, options) {
			return false
		}
	} else if !adminRouteAllowed(w, r, options) {
		return false
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return false
	}
	return true
}

func recordRedisUsage(options Options, apiKeyID, accountID, model string, prompt, completion, total, cacheRead int64, ok bool) {
	if options.Redis == nil || !options.Redis.Enabled() {
		return
	}
	// Store billed tokens so /admin/api/status light snapshot does not over-count
	// prompt cache hits (and does not need a full PG UsageSummary scan).
	billed := total - cacheRead
	if billed < 0 {
		billed = 0
	}
	promptBilled := prompt - cacheRead
	if promptBilled < 0 {
		promptBilled = 0
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = options.Redis.RecordUsage(ctx, redis.UsageDeltas{
		PromptTokens:     promptBilled,
		CompletionTokens: completion,
		TotalTokens:      billed,
		OK:               ok,
		APIKeyID:         apiKeyID,
		AccountID:        accountID,
		Model:            model,
		TS:               time.Now().UTC(),
	})
}

func touchRedisPool(options Options, accountID string, success bool, errText string, cooldown *time.Time, status int) {
	if options.Redis == nil || !options.Redis.Enabled() || strings.TrimSpace(accountID) == "" {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	touch := redis.PoolStatsTouch{Success: success, Error: errText, CooldownUntil: cooldown, LastStatusCode: &status}
	_, _ = options.Redis.TouchStats(ctx, accountID, touch)
}

// usageLight cache: /admin/api/status is polled every ~few seconds by the admin UI.
// Never run full UsageSummary / full-table scans here — reverse proxies
// (Cloudflare/nginx) return HTML 502 when this path stalls.
var (
	statusPayloadMu    sync.Mutex
	statusPayloadCache map[string]any
	statusPayloadAt    time.Time

	usageLightMu         sync.Mutex
	usageLightCache      map[string]any
	usageLightAt         time.Time
	usageLightPGAt       time.Time
	usageLightPGInFlight bool
)

func usageLightSnapshot(ctx context.Context, options Options) map[string]any {
	// 1) In-process cache — status auto-refresh must be free after first hit.
	usageLightMu.Lock()
	if usageLightCache != nil && time.Since(usageLightAt) < 20*time.Second {
		out := cloneStringAnyMap(usageLightCache)
		usageLightMu.Unlock()
		return out
	}
	usageLightMu.Unlock()

	// 2) Redis hot buckets (O(1)). Billed tokens are written at record time.
	if options.Redis != nil && options.Redis.Enabled() {
		snap := options.Redis.LightSnapshot(ctx)
		if snap != nil {
			// Normalize keys for overview card.
			if _, ok := snap["today_tokens"]; !ok {
				snap["today_tokens"] = snap["today_tokens"]
			}
			snap["source"] = "redis"
			usageLightMu.Lock()
			usageLightCache = cloneStringAnyMap(snap)
			usageLightAt = time.Now()
			// Rare PG reconcile (at most every 60s) — never on request path.
			needPG := time.Since(usageLightPGAt) > 60*time.Second && !usageLightPGInFlight
			if needPG {
				usageLightPGInFlight = true
			}
			usageLightMu.Unlock()
			if needPG {
				go refreshUsageLightFromPG(options)
			}
			return snap
		}
	}

	// 3) Stale cache better than a blocking PG scan under a reverse proxy.
	usageLightMu.Lock()
	if usageLightCache != nil {
		out := cloneStringAnyMap(usageLightCache)
		usageLightMu.Unlock()
		return out
	}
	usageLightMu.Unlock()

	// 4) Last resort: tiny PG light with hard 150ms budget (indexes on created_at).
	if options.Store != nil {
		cctx, cancel := context.WithTimeout(ctx, 150*time.Millisecond)
		light, err := options.Store.UsageLightBilled(cctx)
		cancel()
		if err == nil && light != nil {
			light["source"] = "postgres"
			usageLightMu.Lock()
			usageLightCache = cloneStringAnyMap(light)
			usageLightAt = time.Now()
			usageLightPGAt = time.Now()
			usageLightMu.Unlock()
			return light
		}
	}
	return map[string]any{"today_requests": 0, "today_tokens": 0, "total_tokens": 0, "source": "none"}
}

func refreshUsageLightFromPG(options Options) {
	defer func() {
		usageLightMu.Lock()
		usageLightPGInFlight = false
		usageLightMu.Unlock()
	}()
	if options.Store == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	light, err := options.Store.UsageLightBilled(ctx)
	if err != nil || light == nil {
		return
	}
	light["source"] = "postgres"
	usageLightMu.Lock()
	// Only replace redis snapshot if PG succeeded; keep 20s cache TTL from now.
	usageLightCache = cloneStringAnyMap(light)
	usageLightAt = time.Now()
	usageLightPGAt = time.Now()
	usageLightMu.Unlock()
}

func cloneStringAnyMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func leaderStatus(ctx context.Context, options Options) map[string]any {
	if options.Leader != nil {
		return options.Leader.Status(ctx)
	}
	return map[string]any{"is_leader": false, "mode": options.Config.MaintainerLeader, "implementation": "go", "started": false}
}

func maintainerStatus(options Options) map[string]any {
	started := options.Config.GoMaintainer && options.Leader != nil && options.Leader.IsLeader()
	return map[string]any{
		"enabled":         options.Config.GoMaintainer,
		"implementation":  "go",
		"started":         started,
		"leader_required": options.Config.Workers > 1,
	}
}

func serveRegistrationConfigGet(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	cfg, source := loadRegistrationConfig(r.Context(), options, true)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"config": cfg,
		"source": source,
	})
}

func serveRegistrationConfigPut(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	cfg, err := saveRegistrationConfig(r.Context(), options, body, false)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"config":  cfg,
		"message": "注册配置已保存到数据库",
	})
}

func serveRegistrationProxyTest(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	// Prefer explicit body; fill blanks from saved registration_config.
	merged := mergeRegistrationStartBody(r.Context(), options, body)
	proxy := strings.TrimSpace(stringValue(merged["proxy"]))
	user := strings.TrimSpace(stringValue(merged["proxy_username"]))
	pass := strings.TrimSpace(stringValue(merged["proxy_password"]))
	strategy := strings.TrimSpace(stringValue(merged["proxy_strategy"]))
	if strategy == "" {
		strategy = "round_robin"
	}
	lines := splitProxyLines(proxy)
	summary := map[string]any{
		"enabled":  len(lines) > 0,
		"count":    len(lines),
		"strategy": strategy,
		"preview":  previewProxyLines(lines, 3),
		"source":   "registration_config",
	}
	if len(lines) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":            true,
			"proxy_enabled": false,
			"proxy_pool":    summary,
			"message":       "未配置代理（将直连）",
		})
		return
	}
	testAll := truthy(fmt.Sprint(body["test_all"]))
	maxTest := 5
	if v, ok := asInt(body["max_test"]); ok && v > 0 {
		maxTest = v
	}
	if maxTest > 20 {
		maxTest = 20
	}
	targets := lines
	if !testAll || len(lines) == 1 {
		targets = lines[:1]
	} else if len(targets) > maxTest {
		targets = targets[:maxTest]
	}
	// Probe target for HTTP CONNECT / TCP reachability (registration hits accounts.x.ai).
	probeHost := strings.TrimSpace(stringValue(body["probe_host"]))
	if probeHost == "" {
		probeHost = "accounts.x.ai:443"
	}
	timeoutMs := 8000
	if v, ok := asInt(body["timeout_ms"]); ok && v > 0 {
		timeoutMs = v
	}
	if timeoutMs > 30000 {
		timeoutMs = 30000
	}
	timeout := time.Duration(timeoutMs) * time.Millisecond

	results := make([]map[string]any, 0, len(targets))
	okN := 0
	for _, raw := range targets {
		canonical, cerr := canonicalizeRegistrationProxyLine(raw, user, pass)
		item := map[string]any{
			"proxy": redactProxyURL(raw, user),
		}
		if user != "" && !strings.Contains(raw, "@") {
			item["proxy_username"] = user
			item["has_password"] = pass != ""
		}
		if cerr != nil {
			item["ok"] = false
			item["error"] = cerr.Error()
			item["message"] = "proxy entry parse failed"
			results = append(results, item)
			continue
		}
		item["proxy"] = redactProxyURL(canonical, user)
		probe := probeRegistrationProxy(canonical, probeHost, timeout)
		for k, v := range probe {
			item[k] = v
		}
		if truthy(fmt.Sprint(item["ok"])) {
			okN++
		}
		results = append(results, item)
	}
	msg := fmt.Sprintf("probed %d/%d proxy entries ok (target %s)", okN, len(results), probeHost)
	if okN == 0 {
		msg = fmt.Sprintf("全部代理探测失败 (target %s) — 注册不会静默直连", probeHost)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            okN > 0,
		"proxy_enabled": true,
		"proxy_pool":    summary,
		"tested":        len(results),
		"ok_count":      okN,
		"fail_count":    len(results) - okN,
		"results":       results,
		"probe_host":    probeHost,
		"message":       msg,
	})
}

// canonicalizeRegistrationProxyLine normalizes common proxy paste formats into a URL.
// Supports: scheme://[user:pass@]host:port, host:port, host:port:user:pass.
func canonicalizeRegistrationProxyLine(raw, sharedUser, sharedPass string) (string, error) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", fmt.Errorf("empty proxy line")
	}
	lower := strings.ToLower(s)
	if strings.HasPrefix(lower, "soket5://") {
		s = "socks5://" + s[len("soket5://"):]
	} else if strings.HasPrefix(lower, "socket5://") {
		s = "socks5://" + s[len("socket5://"):]
	}

	scheme := "http"
	rest := s
	if i := strings.Index(s, "://"); i >= 0 {
		scheme = strings.ToLower(strings.TrimSpace(s[:i]))
		if scheme == "" {
			scheme = "http"
		}
		if scheme == "soket5" || scheme == "socket5" {
			scheme = "socks5"
		}
		rest = s[i+3:]
	}

	// host:port:user:pass shorthand (no '@', ≥3 colons in rest after optional scheme).
	if !strings.Contains(rest, "@") {
		parts := strings.Split(rest, ":")
		if len(parts) >= 4 {
			host := strings.TrimSpace(parts[0])
			port := strings.TrimSpace(parts[1])
			u := parts[2]
			p := strings.Join(parts[3:], ":")
			if host != "" && port != "" {
				if _, err := strconv.Atoi(port); err == nil {
					auth := url.UserPassword(u, p)
					return fmt.Sprintf("%s://%s@%s:%s", scheme, auth.String(), host, port), nil
				}
			}
		}
	}

	// Bare host:port → http://host:port
	if !strings.Contains(s, "://") {
		s = scheme + "://" + rest
	} else {
		s = scheme + "://" + rest
	}

	u, err := url.Parse(s)
	if err != nil {
		return "", fmt.Errorf("invalid proxy url: %w", err)
	}
	if u.Host == "" {
		return "", fmt.Errorf("proxy missing host")
	}
	// Apply shared auth when line has no userinfo.
	if u.User == nil {
		su := strings.TrimSpace(sharedUser)
		sp := sharedPass
		if su != "" {
			if sp != "" {
				u.User = url.UserPassword(su, sp)
			} else {
				u.User = url.User(su)
			}
		}
	}
	// Default ports.
	if u.Port() == "" {
		switch strings.ToLower(u.Scheme) {
		case "https":
			u.Host = net.JoinHostPort(u.Hostname(), "443")
		case "socks5", "socks5h", "socks4":
			u.Host = net.JoinHostPort(u.Hostname(), "1080")
		default:
			u.Host = net.JoinHostPort(u.Hostname(), "8080")
		}
	}
	return u.String(), nil
}

// probeRegistrationProxy performs a real connectivity check through the proxy.
// HTTP(S) proxies: TCP dial + HTTP CONNECT to probeHost.
// SOCKS proxies: TCP dial to proxy only (no full SOCKS handshake without extra deps).
func probeRegistrationProxy(proxyURL, probeHost string, timeout time.Duration) map[string]any {
	start := time.Now()
	out := map[string]any{
		"ok":         false,
		"probe_host": probeHost,
	}
	u, err := url.Parse(proxyURL)
	if err != nil || u.Host == "" {
		out["error"] = "invalid proxy url"
		out["message"] = "proxy parse failed"
		return out
	}
	scheme := strings.ToLower(u.Scheme)
	if scheme == "" {
		scheme = "http"
	}
	out["scheme"] = scheme
	dialHost := u.Host
	if u.Port() == "" {
		// Should already be filled by canonicalize, but be defensive.
		switch scheme {
		case "https":
			dialHost = net.JoinHostPort(u.Hostname(), "443")
		case "socks5", "socks5h", "socks4":
			dialHost = net.JoinHostPort(u.Hostname(), "1080")
		default:
			dialHost = net.JoinHostPort(u.Hostname(), "8080")
		}
	}

	conn, err := net.DialTimeout("tcp", dialHost, timeout)
	if err != nil {
		out["error"] = err.Error()
		out["message"] = "tcp dial to proxy failed"
		out["latency_ms"] = time.Since(start).Milliseconds()
		return out
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	switch scheme {
	case "socks5", "socks5h", "socks4", "socks":
		// Full SOCKS handshake needs extra deps; TCP reachability is still useful
		// to catch dead hosts / wrong ports (the prior "accepted" check was fake).
		out["ok"] = true
		out["message"] = "tcp reachability ok (socks; CONNECT not probed)"
		out["method"] = "tcp"
		out["latency_ms"] = time.Since(start).Milliseconds()
		return out
	case "https":
		// TLS-wrapped HTTP proxy is uncommon; treat like TCP ok for now.
		out["ok"] = true
		out["message"] = "tcp reachability ok (https proxy; CONNECT skipped)"
		out["method"] = "tcp"
		out["latency_ms"] = time.Since(start).Milliseconds()
		return out
	default:
		// HTTP proxy: send CONNECT probeHost HTTP/1.1
		target := strings.TrimSpace(probeHost)
		if target == "" {
			target = "accounts.x.ai:443"
		}
		if !strings.Contains(target, ":") {
			target = target + ":443"
		}
		var req strings.Builder
		req.WriteString("CONNECT ")
		req.WriteString(target)
		req.WriteString(" HTTP/1.1\r\nHost: ")
		req.WriteString(target)
		req.WriteString("\r\n")
		if u.User != nil {
			user := u.User.Username()
			pass, _ := u.User.Password()
			token := base64.StdEncoding.EncodeToString([]byte(user + ":" + pass))
			req.WriteString("Proxy-Authorization: Basic ")
			req.WriteString(token)
			req.WriteString("\r\n")
		}
		req.WriteString("Proxy-Connection: close\r\nConnection: close\r\n\r\n")
		if _, err := io.WriteString(conn, req.String()); err != nil {
			out["error"] = err.Error()
			out["message"] = "CONNECT write failed"
			out["latency_ms"] = time.Since(start).Milliseconds()
			return out
		}
		// Read status line only (don't require full tunnel success body).
		buf := make([]byte, 512)
		n, rerr := conn.Read(buf)
		latency := time.Since(start).Milliseconds()
		out["latency_ms"] = latency
		if n <= 0 {
			if rerr != nil {
				out["error"] = rerr.Error()
			} else {
				out["error"] = "empty CONNECT response"
			}
			out["message"] = "CONNECT probe failed"
			out["method"] = "connect"
			return out
		}
		resp := string(buf[:n])
		// First line: HTTP/1.x 200 ... or 407 etc.
		first := resp
		if i := strings.Index(resp, "\r\n"); i >= 0 {
			first = resp[:i]
		} else if i := strings.Index(resp, "\n"); i >= 0 {
			first = resp[:i]
		}
		out["connect_status_line"] = strings.TrimSpace(first)
		out["method"] = "connect"
		code := 0
		fields := strings.Fields(first)
		if len(fields) >= 2 {
			if c, e := strconv.Atoi(fields[1]); e == nil {
				code = c
			}
		}
		out["connect_status"] = code
		if code >= 200 && code < 300 {
			out["ok"] = true
			out["message"] = fmt.Sprintf("CONNECT %s ok (%d)", target, code)
			return out
		}
		if code == 407 {
			out["error"] = "proxy authentication required (407)"
			out["message"] = "CONNECT 407 — check proxy username/password"
			return out
		}
		if code > 0 {
			out["error"] = fmt.Sprintf("CONNECT status %d", code)
			out["message"] = fmt.Sprintf("CONNECT %s failed: %s", target, strings.TrimSpace(first))
			return out
		}
		// Some proxies reply non-HTTP on bad auth; still report body snippet.
		out["error"] = "unrecognized CONNECT response"
		out["message"] = "CONNECT probe failed: " + strings.TrimSpace(first)
		return out
	}
}

var registrationSecretKeys = map[string]struct{}{
	"api_key":          {},
	"moemail_api_key":  {},
	"yyds_api_key":     {},
	"gptmail_api_key":  {},
	"cfmail_api_key":   {},
	"tempmail_api_key": {},
	"yescaptcha_key":   {},
	"proxy_password":   {},
}

func loadRegistrationConfig(ctx context.Context, options Options, includeSecrets bool) (map[string]any, string) {
	cfg := map[string]any{}
	source := "env"
	if options.Store != nil {
		if raw, err := options.Store.GetSetting(ctx, "registration_config"); err == nil {
			if m, ok := raw.(map[string]any); ok && m != nil {
				cfg = cloneMapAny(m)
				source = "database"
			}
		}
	}
	cfg = normalizeRegistrationConfig(cfg)
	sanitizeRegistrationMailSecrets(cfg)
	// Env fallback when durable MoeMail slot was wiped by historical key pollution.
	if strings.TrimSpace(stringValue(cfg["moemail_api_key"])) == "" {
		envKey := strings.TrimSpace(os.Getenv("GROK2API_MOEMAIL_API_KEY"))
		if envKey == "" {
			envKey = strings.TrimSpace(os.Getenv("MOEMAIL_API_KEY"))
		}
		if envKey != "" && mailSecretFitsSlot("moemail_api_key", envKey) {
			cfg["moemail_api_key"] = envKey
		}
	}
	if strings.TrimSpace(stringValue(cfg["moemail_base_url"])) == "" {
		u := strings.TrimSpace(os.Getenv("GROK2API_MOEMAIL_BASE_URL"))
		if u == "" {
			u = strings.TrimSpace(os.Getenv("MOEMAIL_BASE_URL"))
		}
		if u != "" {
			cfg["moemail_base_url"] = u
		}
	}
	if strings.TrimSpace(stringValue(cfg["moemail_domain"])) == "" {
		d := strings.TrimSpace(os.Getenv("GROK2API_MOEMAIL_DOMAIN"))
		if d == "" {
			d = strings.TrimSpace(os.Getenv("MOEMAIL_DOMAIN"))
		}
		if d != "" {
			cfg["moemail_domain"] = d
		}
	}
	// Rebuild active mirrors after env fill.
	sanitizeRegistrationMailSecrets(cfg)
	// Always expose per-provider slots so the admin form can bind each service.
	for _, k := range []string{
		"moemail_api_key", "yyds_api_key", "gptmail_api_key", "cfmail_api_key", "tempmail_api_key",
		"moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain", "tempmail_domain",
		"moemail_base_url", "cfmail_base_url",
	} {
		if _, ok := cfg[k]; !ok || cfg[k] == nil {
			cfg[k] = ""
		}
	}
	if !includeSecrets {
		for k := range registrationSecretKeys {
			if v := strings.TrimSpace(stringValue(cfg[k])); v != "" {
				cfg[k] = maskSecret(v)
				cfg[k+"_set"] = true
			} else {
				cfg[k] = ""
				cfg[k+"_set"] = false
			}
		}
	}
	return cfg, source
}

func saveRegistrationConfig(ctx context.Context, options Options, patch map[string]any, replace bool) (map[string]any, error) {
	if options.Store == nil {
		return nil, errors.New("store unavailable")
	}
	if patch == nil {
		patch = map[string]any{}
	}
	current := map[string]any{}
	if !replace {
		if raw, err := options.Store.GetSetting(ctx, "registration_config"); err == nil {
			if m, ok := raw.(map[string]any); ok && m != nil {
				current = cloneMapAny(m)
			}
		}
	}
	// Merge patch: empty secrets keep previous; masked secrets keep previous.
	// Never accept cross-provider key shapes into the wrong dedicated slot
	// (e.g. YYDS AC-* written into moemail_api_key by adapter remaps).
	for k, v := range patch {
		if _, isSecret := registrationSecretKeys[k]; isSecret {
			s := strings.TrimSpace(fmt.Sprint(v))
			if isMaskedSecret(s) {
				continue
			}
			// TempMail.lol free tier: allow explicit empty key to clear paid key.
			if s == "" {
				if k == "tempmail_api_key" {
					current[k] = ""
				}
				continue
			}
			if !mailSecretFitsSlot(k, s) {
				// Drop polluted values so previous good secret (or env fallback) wins.
				continue
			}
			current[k] = s
			continue
		}
		// Keep explicit empty strings for non-secrets (clears domain etc.).
		current[k] = v
	}
	current = normalizeRegistrationConfig(current)
	sanitizeRegistrationMailSecrets(current)
	// Ensure multi-provider slots are always explicit strings in DB so the admin
	// UI can restore each service independently after switch.
	for _, k := range []string{
		"mail_provider",
		"domain", "moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain", "tempmail_domain",
		"base_url", "moemail_base_url", "cfmail_base_url",
	} {
		if _, ok := current[k]; !ok {
			current[k] = ""
		} else if current[k] == nil {
			current[k] = ""
		} else {
			current[k] = strings.TrimSpace(fmt.Sprint(current[k]))
		}
	}
	// Dedicated API key slots always present in DB (independent per provider).
	for _, k := range []string{
		"moemail_api_key", "yyds_api_key", "gptmail_api_key", "cfmail_api_key", "tempmail_api_key",
	} {
		if _, ok := current[k]; !ok || current[k] == nil {
			current[k] = ""
		} else {
			current[k] = strings.TrimSpace(fmt.Sprint(current[k]))
		}
	}
	if err := options.Store.SetSetting(ctx, "registration_config", current); err != nil {
		return nil, err
	}
	return current, nil
}

// mailSecretFitsSlot reports whether a secret value belongs in the given
// registration_config key. Used to stop adapter remaps (active key mirrored into
// moemail_api_key) from permanently overwriting another provider's slot.
func mailSecretFitsSlot(key, value string) bool {
	v := strings.TrimSpace(value)
	if v == "" {
		return true
	}
	switch key {
	case "moemail_api_key":
		// MoeMail keys are typically mk_*; reject YYDS AC-* and common GPTMail sk-*.
		if strings.HasPrefix(v, "AC-") || strings.HasPrefix(strings.ToLower(v), "sk-") {
			return false
		}
		return true
	case "yyds_api_key":
		// YYDS is AC-*; reject MoeMail mk_* and GPTMail sk-*.
		if strings.HasPrefix(v, "mk_") || strings.HasPrefix(strings.ToLower(v), "sk-") {
			return false
		}
		return true
	case "gptmail_api_key":
		// GPTMail is optional / sk-* / opaque; never store MoeMail or YYDS shapes.
		if strings.HasPrefix(v, "mk_") || strings.HasPrefix(v, "AC-") {
			return false
		}
		return true
	case "tempmail_api_key":
		// Optional Plus/Ultra key; free tier empty. Reject other providers' key shapes.
		if strings.HasPrefix(v, "mk_") || strings.HasPrefix(v, "AC-") || strings.HasPrefix(strings.ToLower(v), "sk-") {
			return false
		}
		return true
	case "api_key":
		// Generic active mirror — accept anything; sanitizeRegistrationMailSecrets
		// rewrites it from the active provider slot.
		return true
	default:
		return true
	}
}

// sanitizeRegistrationMailSecrets removes cross-provider contamination already
// present in a config map and rebuilds active api_key / domain / base_url mirrors
// from dedicated slots only.
func sanitizeRegistrationMailSecrets(cfg map[string]any) {
	if cfg == nil {
		return
	}
	mk := strings.TrimSpace(stringValue(cfg["moemail_api_key"]))
	yk := strings.TrimSpace(stringValue(cfg["yyds_api_key"]))
	gk := strings.TrimSpace(stringValue(cfg["gptmail_api_key"]))
	ck := strings.TrimSpace(stringValue(cfg["cfmail_api_key"]))

	// Rescue: polluted MoeMail slot holding YYDS key → move into yyds if empty.
	if strings.HasPrefix(mk, "AC-") {
		if yk == "" {
			cfg["yyds_api_key"] = mk
			yk = mk
		}
		cfg["moemail_api_key"] = ""
		mk = ""
	}
	// Rescue: polluted GPTMail slot holding YYDS/MoeMail key.
	if strings.HasPrefix(gk, "AC-") {
		if yk == "" {
			cfg["yyds_api_key"] = gk
			yk = gk
		}
		cfg["gptmail_api_key"] = ""
		gk = ""
	}
	if strings.HasPrefix(gk, "mk_") {
		if mk == "" {
			cfg["moemail_api_key"] = gk
			mk = gk
		}
		cfg["gptmail_api_key"] = ""
		gk = ""
	}
	// Rescue: polluted YYDS slot holding MoeMail key.
	if strings.HasPrefix(yk, "mk_") {
		if mk == "" {
			cfg["moemail_api_key"] = yk
			mk = yk
		}
		cfg["yyds_api_key"] = ""
		yk = ""
	}
	if strings.HasPrefix(strings.ToLower(yk), "sk-") {
		if gk == "" {
			cfg["gptmail_api_key"] = yk
			gk = yk
		}
		cfg["yyds_api_key"] = ""
		yk = ""
	}

	// Read TempMail slot early (optional free-tier empty).
	tk := strings.TrimSpace(stringValue(cfg["tempmail_api_key"]))

	// Active mirrors from dedicated slots (never leave a foreign key as api_key).
	provider := strings.ToLower(strings.TrimSpace(stringValue(cfg["mail_provider"])))
	switch provider {
	case "yyds":
		cfg["api_key"] = yk
		if d := strings.TrimSpace(stringValue(cfg["yyds_domain"])); d != "" {
			cfg["domain"] = d
		}
		// Do not clear durable moemail_base_url here — only start-merge remaps clear it.
	case "gptmail":
		cfg["api_key"] = gk
		if d := strings.TrimSpace(stringValue(cfg["gptmail_domain"])); d != "" {
			cfg["domain"] = d
		}
	case "cfmail":
		cfg["api_key"] = ck
		if d := strings.TrimSpace(stringValue(cfg["cfmail_domain"])); d != "" {
			cfg["domain"] = d
		}
		if u := strings.TrimSpace(stringValue(cfg["cfmail_base_url"])); u != "" {
			cfg["base_url"] = u
		}
	case "tempmail":
		// Promote active api_key into dedicated slot when form only sent api_key.
		active := strings.TrimSpace(stringValue(cfg["api_key"]))
		if tk == "" && active != "" && mailSecretFitsSlot("tempmail_api_key", active) {
			cfg["tempmail_api_key"] = active
			tk = active
		}
		cfg["api_key"] = tk
		if d := strings.TrimSpace(stringValue(cfg["tempmail_domain"])); d != "" {
			cfg["domain"] = d
		} else if d := strings.TrimSpace(stringValue(cfg["domain"])); d != "" {
			// Keep domain only as display; dedicated slot is source of truth when set.
			cfg["tempmail_domain"] = d
		}
		cfg["base_url"] = ""
	default: // moemail
		cfg["api_key"] = mk
		if d := strings.TrimSpace(stringValue(cfg["moemail_domain"])); d != "" {
			cfg["domain"] = d
		}
		if u := strings.TrimSpace(stringValue(cfg["moemail_base_url"])); u != "" {
			cfg["base_url"] = u
		}
	}
	// Ensure dedicated slots always exist in the map so clients can round-trip them
	// (even when empty — free TempMail.lol has no key).
	if _, ok := cfg["tempmail_api_key"]; !ok {
		cfg["tempmail_api_key"] = tk
	}
	if _, ok := cfg["tempmail_domain"]; !ok {
		cfg["tempmail_domain"] = stringValue(cfg["tempmail_domain"])
	}
	_ = mk
	_ = yk
	_ = gk
	_ = ck
	_ = tk
}

// registrationConfigPatchForPersist builds a DB-safe patch from a start request.
// Adapter remaps put the active provider key into moemail_api_key / clear base URLs;
// those must never be written back into durable per-provider slots.
func registrationConfigPatchForPersist(req, merged map[string]any) map[string]any {
	out := map[string]any{}
	if merged != nil {
		for k, v := range merged {
			out[k] = v
		}
	}
	// Prefer original request dedicated secrets when present and well-shaped.
	for _, k := range []string{
		"moemail_api_key", "yyds_api_key", "gptmail_api_key", "cfmail_api_key", "tempmail_api_key",
		"yescaptcha_key", "proxy_password",
	} {
		if req == nil {
			break
		}
		if _, ok := req[k]; !ok {
			continue
		}
		s := strings.TrimSpace(stringValue(req[k]))
		if isMaskedSecret(s) {
			// Leave merged value for now; sanitize / save rules handle empties.
			continue
		}
		// Explicit empty from form: TempMail.lol free tier must clear paid key.
		// Other providers keep previous on empty (legacy "leave blank = unchanged").
		if s == "" {
			if k == "tempmail_api_key" {
				out[k] = ""
			}
			continue
		}
		if mailSecretFitsSlot(k, s) {
			out[k] = s
		}
	}
	// Restore durable host fields that merge may have cleared for non-MoeMail runs.
	if req != nil {
		for _, k := range []string{"moemail_base_url", "cfmail_base_url", "base_url"} {
			if s := strings.TrimSpace(stringValue(req[k])); s != "" {
				out[k] = s
			}
		}
		// Dedicated domains always from request when non-empty.
		for _, k := range []string{"moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain", "tempmail_domain", "domain"} {
			if _, ok := req[k]; ok {
				// Allow explicit empty to clear.
				out[k] = strings.TrimSpace(stringValue(req[k]))
			}
		}
		if p := strings.TrimSpace(stringValue(req["mail_provider"])); p != "" {
			out["mail_provider"] = p
		}
	}
	// After overlay, drop remapped moemail_api_key when provider is not moemail —
	// the durable MoeMail key must stay whatever was in the dedicated slot only.
	provider := strings.ToLower(strings.TrimSpace(stringValue(out["mail_provider"])))
	if provider != "moemail" && provider != "" {
		// Remove adapter remaps so saveRegistrationConfig won't overwrite MoeMail.
		delete(out, "moemail_api_key")
		// Keep moemail_base_url out of the patch if merge cleared it (empty would
		// not clear secrets, but would clear non-secret base on save).
		if strings.TrimSpace(stringValue(out["moemail_base_url"])) == "" {
			delete(out, "moemail_base_url")
		}
		if provider != "cfmail" && strings.TrimSpace(stringValue(out["base_url"])) == "" {
			delete(out, "base_url")
		}
	} else {
		// moemail: never persist a foreign key shape into moemail_api_key.
		if k := strings.TrimSpace(stringValue(out["moemail_api_key"])); !mailSecretFitsSlot("moemail_api_key", k) {
			delete(out, "moemail_api_key")
		}
		if k := strings.TrimSpace(stringValue(out["api_key"])); k != "" && !mailSecretFitsSlot("moemail_api_key", k) {
			// Generic api_key was remapped from another provider — drop it.
			delete(out, "api_key")
		}
	}
	sanitizeRegistrationMailSecrets(out)
	return out
}

func mergeRegistrationStartBody(ctx context.Context, options Options, body map[string]any) map[string]any {
	out := map[string]any{}
	saved, _ := loadRegistrationConfig(ctx, options, true)
	// Drop any historically polluted cross-provider keys before merge.
	sanitizeRegistrationMailSecrets(saved)
	for k, v := range saved {
		out[k] = v
	}
	// Request overrides. Secrets: non-empty wins; TempMail.lol empty key/domain
	// clears dedicated slots (free tier default). Other empty secrets keep saved.
	for k, v := range body {
		if v == nil {
			continue
		}
		if _, isSecret := registrationSecretKeys[k]; isSecret {
			s := strings.TrimSpace(fmt.Sprint(v))
			if isMaskedSecret(s) {
				continue
			}
			if s == "" {
				// Explicit clear for TempMail free-tier key only.
				if k == "tempmail_api_key" {
					out[k] = ""
				}
				continue
			}
			out[k] = s
			continue
		}
		switch vv := v.(type) {
		case string:
			// Empty non-secret: allow clear for per-provider domains (independent slots).
			// Generic "domain" empty still keeps saved when switching forms, except when
			// the active provider is tempmail and body sent tempmail_domain/domain empty.
			if strings.TrimSpace(vv) == "" {
				switch k {
				case "tempmail_domain", "moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain",
					"moemail_base_url", "cfmail_base_url", "tempmail_api_key":
					// Dedicated per-provider slots: empty means clear this service only.
					out[k] = ""
				case "domain", "base_url":
					// Generic mirrors: allow clear when body explicitly sends empty.
					out[k] = ""
				default:
					// keep saved for other empty strings
					continue
				}
				continue
			}
			out[k] = vv
		default:
			out[k] = v
		}
	}
	// Normalize mail_provider first.
	provider := strings.ToLower(strings.TrimSpace(stringValue(out["mail_provider"])))
	switch provider {
	case "yyds", "gptmail", "cfmail", "tempmail", "moemail":
		// ok
	default:
		provider = "moemail"
	}
	out["mail_provider"] = provider

	// Map active api_key into the provider-specific slot when missing.
	// Only when the key shape matches the target slot — never re-pollute MoeMail
	// with a leftover YYDS AC-* mirror in api_key.
	if apiKey := strings.TrimSpace(stringValue(out["api_key"])); apiKey != "" {
		switch provider {
		case "yyds":
			if strings.TrimSpace(stringValue(out["yyds_api_key"])) == "" && mailSecretFitsSlot("yyds_api_key", apiKey) {
				out["yyds_api_key"] = apiKey
			}
		case "gptmail":
			if strings.TrimSpace(stringValue(out["gptmail_api_key"])) == "" && mailSecretFitsSlot("gptmail_api_key", apiKey) {
				out["gptmail_api_key"] = apiKey
			}
		case "cfmail":
			if strings.TrimSpace(stringValue(out["cfmail_api_key"])) == "" {
				out["cfmail_api_key"] = apiKey
			}
		default: // moemail
			if strings.TrimSpace(stringValue(out["moemail_api_key"])) == "" && mailSecretFitsSlot("moemail_api_key", apiKey) {
				out["moemail_api_key"] = apiKey
			}
		}
	}

	// Domain: prefer provider-specific slot, then generic domain.
	activeDomain := strings.TrimSpace(stringValue(out["domain"]))
	switch provider {
	case "yyds":
		if d := strings.TrimSpace(stringValue(out["yyds_domain"])); d != "" {
			activeDomain = d
		}
		out["yyds_domain"] = activeDomain
		out["domain"] = activeDomain
	case "gptmail":
		if d := strings.TrimSpace(stringValue(out["gptmail_domain"])); d != "" {
			activeDomain = d
		}
		out["gptmail_domain"] = activeDomain
		out["domain"] = activeDomain
	case "cfmail":
		if d := strings.TrimSpace(stringValue(out["cfmail_domain"])); d != "" {
			activeDomain = d
		}
		out["cfmail_domain"] = activeDomain
		out["domain"] = activeDomain
	case "tempmail":
		// Dedicated slot only. Empty = free random domain; do not inherit generic domain.
		if d := strings.TrimSpace(stringValue(out["tempmail_domain"])); d != "" {
			activeDomain = d
		} else {
			activeDomain = ""
		}
		out["tempmail_domain"] = activeDomain
		out["domain"] = activeDomain
	default:
		if d := strings.TrimSpace(stringValue(out["moemail_domain"])); d != "" {
			activeDomain = d
		}
		out["moemail_domain"] = activeDomain
		out["domain"] = activeDomain
	}

	// CRITICAL: Python adapter historically only reads moemail_api_key / moemail_base_url.
	// When using YYDS/GPTMail/CFMail/TempMail we MUST overwrite moemail_api_key with the
	// active provider key — otherwise a previously saved MoeMail key is used.
	switch provider {
	case "yyds":
		if k := strings.TrimSpace(stringValue(out["yyds_api_key"])); k != "" {
			out["moemail_api_key"] = k
			out["api_key"] = k
		}
		// Fixed host; never pass MoeMail base into YYDS.
		out["moemail_base_url"] = ""
		out["base_url"] = ""
	case "gptmail":
		if k := strings.TrimSpace(stringValue(out["gptmail_api_key"])); k != "" && mailSecretFitsSlot("gptmail_api_key", k) {
			out["moemail_api_key"] = k
			out["api_key"] = k
		} else {
			// GPTMail allows public test key when empty — leave empty for adapter.
			// Also clear polluted AC-*/mk_* that must not be sent as GPTMail credentials.
			out["gptmail_api_key"] = ""
			out["moemail_api_key"] = ""
			out["api_key"] = ""
		}
		out["moemail_base_url"] = ""
		out["base_url"] = ""
	case "cfmail":
		if k := strings.TrimSpace(stringValue(out["cfmail_api_key"])); k != "" {
			out["moemail_api_key"] = k
			out["api_key"] = k
		}
		if u := strings.TrimSpace(stringValue(out["cfmail_base_url"])); u != "" {
			out["moemail_base_url"] = u
			out["base_url"] = u
		} else if u := strings.TrimSpace(stringValue(out["base_url"])); u != "" {
			out["moemail_base_url"] = u
			out["cfmail_base_url"] = u
		}
	case "tempmail":
		// Free: empty key + empty domain (random inbox). Paid: optional Bearer key.
		// Never fall through to MoeMail with a leftover mk_* key.
		k := strings.TrimSpace(stringValue(out["tempmail_api_key"]))
		if k != "" && mailSecretFitsSlot("tempmail_api_key", k) {
			out["moemail_api_key"] = k
			out["api_key"] = k
		} else {
			out["tempmail_api_key"] = ""
			out["moemail_api_key"] = ""
			out["api_key"] = ""
		}
		out["moemail_base_url"] = ""
		out["base_url"] = ""
	default: // moemail
		if k := strings.TrimSpace(stringValue(out["moemail_api_key"])); k != "" {
			// Drop foreign shapes that slipped past load sanitize.
			if !mailSecretFitsSlot("moemail_api_key", k) {
				out["moemail_api_key"] = ""
				k = ""
			} else {
				out["api_key"] = k
			}
		}
		if strings.TrimSpace(stringValue(out["moemail_api_key"])) == "" {
			if k := strings.TrimSpace(stringValue(out["api_key"])); k != "" && mailSecretFitsSlot("moemail_api_key", k) {
				out["moemail_api_key"] = k
			}
		}
		if u := strings.TrimSpace(stringValue(out["moemail_base_url"])); u != "" {
			out["base_url"] = u
		} else if u := strings.TrimSpace(stringValue(out["base_url"])); u != "" {
			out["moemail_base_url"] = u
		}
	}
	return out
}

func normalizeRegistrationConfig(raw map[string]any) map[string]any {
	cfg := map[string]any{}
	if raw != nil {
		for k, v := range raw {
			cfg[k] = v
		}
	}
	// defaults
	if strings.TrimSpace(stringValue(cfg["mail_provider"])) == "" {
		cfg["mail_provider"] = "moemail"
	}
	// Normalize known provider aliases.
	switch strings.ToLower(strings.TrimSpace(stringValue(cfg["mail_provider"]))) {
	case "yyds", "yydsmail", "yyds_mail", "vip215", "215", "maliapi":
		cfg["mail_provider"] = "yyds"
	case "gptmail", "gpt-mail", "chatgpt", "chatgptmail", "mail.chatgpt.org.uk":
		cfg["mail_provider"] = "gptmail"
	case "cfmail", "cf-mail", "cloudflare", "cloudflare_temp":
		cfg["mail_provider"] = "cfmail"
	case "tempmail", "tempmail.lol", "tempmaillol", "tempmail_lol", "lol", "tmlol":
		cfg["mail_provider"] = "tempmail"
	case "moemail", "moe", "moe-mail":
		cfg["mail_provider"] = "moemail"
	}
	provider := strings.ToLower(strings.TrimSpace(stringValue(cfg["captcha_provider"])))
	if provider != "yescaptcha" {
		provider = "local"
	}
	cfg["captcha_provider"] = provider
	if provider == "local" {
		cfg["local_solver_url"] = "http://127.0.0.1:5072"
	}
	strat := strings.ToLower(strings.TrimSpace(stringValue(cfg["proxy_strategy"])))
	switch strat {
	case "random", "rand":
		cfg["proxy_strategy"] = "random"
	case "sticky", "first", "fixed":
		cfg["proxy_strategy"] = "sticky"
	default:
		cfg["proxy_strategy"] = "round_robin"
	}
	// numeric defaults
	if _, ok := asInt(cfg["count"]); !ok {
		cfg["count"] = 1
	}
	if _, ok := asInt(cfg["concurrency"]); !ok {
		cfg["concurrency"] = 3
	}
	if _, ok := asInt(cfg["stagger_ms"]); !ok {
		cfg["stagger_ms"] = 300
	}
	if _, ok := asInt(cfg["probe_delay_sec"]); !ok {
		cfg["probe_delay_sec"] = 30
	}
	return cfg
}

func maskSecret(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if len(v) <= 4 {
		return "****"
	}
	return v[:2] + "…" + v[len(v)-2:]
}

func isMaskedSecret(v string) bool {
	s := strings.TrimSpace(v)
	if s == "" {
		return false
	}
	if s == "****" {
		return true
	}
	if strings.Contains(s, "…") || strings.Contains(s, "...") {
		return true
	}
	for _, ch := range s {
		if ch != '*' {
			return false
		}
	}
	return true
}

func splitProxyLines(text string) []string {
	raw := strings.FieldsFunc(text, func(r rune) bool {
		return r == '\n' || r == '\r' || r == ';' || r == ','
	})
	out := make([]string, 0, len(raw))
	for _, line := range raw {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		out = append(out, line)
	}
	return out
}

func previewProxyLines(lines []string, n int) []string {
	if n <= 0 || len(lines) == 0 {
		return []string{}
	}
	if len(lines) < n {
		n = len(lines)
	}
	out := make([]string, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, redactProxyURL(lines[i], ""))
	}
	return out
}

func redactProxyURL(raw, user string) string {
	s := strings.TrimSpace(raw)
	if s == "" {
		return ""
	}
	// hide password in user:pass@host
	if at := strings.LastIndex(s, "@"); at > 0 {
		head := s[:at]
		host := s[at+1:]
		if i := strings.Index(head, "://"); i >= 0 {
			scheme := head[:i+3]
			cred := head[i+3:]
			if colon := strings.Index(cred, ":"); colon >= 0 {
				return scheme + cred[:colon] + ":***@" + host
			}
		}
	}
	if user != "" && !strings.Contains(s, "@") {
		return s + " (auth user set)"
	}
	return s
}

func asInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	case string:
		s := strings.TrimSpace(n)
		if s == "" {
			return 0, false
		}
		i, err := strconv.Atoi(s)
		return i, err == nil
	default:
		return 0, false
	}
}

func cloneMapAny(in map[string]any) map[string]any {
	out := make(map[string]any, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func writeRegistrationError(w http.ResponseWriter, err error) {
	var re *regclient.Error
	if errors.As(err, &re) {
		status := re.Status
		if status < 400 {
			status = http.StatusBadGateway
		}
		writeJSON(w, status, map[string]any{"detail": re.Detail})
		return
	}
	writeJSON(w, http.StatusBadGateway, map[string]any{"detail": err.Error()})
}

func serveDeviceLoginStart(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration/device service URL is not configured"})
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	if body == nil {
		body = map[string]any{"mode": "device", "capture": true}
	}
	if _, ok := body["mode"]; !ok {
		body["mode"] = "device"
	}
	payload, err := client.StartDeviceLogin(r.Context(), body)
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveDeviceLoginSession(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration/device service URL is not configured"})
		return
	}
	sid := strings.TrimSpace(r.PathValue("session_id"))
	if sid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "session_id required"})
		return
	}
	payload, err := client.DeviceLoginSession(r.Context(), sid)
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveDeviceLoginSessions(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration/device service URL is not configured"})
		return
	}
	payload, err := client.DeviceLoginSessions(r.Context())
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveSSOImportStart(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration/sso service URL is not configured"})
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	payload, err := client.StartSSOImport(r.Context(), body)
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveSSOImportJob(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration/sso service URL is not configured"})
		return
	}
	payload, err := client.SSOImportJob(r.Context(), r.PathValue("job_id"))
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationAvailability(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured", "ok": false, "available": false})
		return
	}
	payload, err := client.Availability(r.Context())
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationSessions(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	payload, err := client.Sessions(r.Context())
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationSession(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	includeAuth := truthy(r.URL.Query().Get("include_auth_json"))
	payload, err := client.Session(r.Context(), r.PathValue("session_id"), includeAuth)
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationStopSession(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	payload, err := client.StopSession(r.Context(), r.PathValue("session_id"))
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationBatch(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	payload, err := client.Batch(r.Context(), r.PathValue("batch_id"))
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationStopBatch(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	payload, err := client.StopBatch(r.Context(), r.PathValue("batch_id"))
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationResumeBatch(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	force, _ := body["force"].(bool)
	payload, err := client.ResumeBatch(r.Context(), r.PathValue("batch_id"), force)
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationStart(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	if body == nil {
		body = map[string]any{}
	}
	// Keep the raw request so durable per-provider slots can be persisted without
	// adapter remaps (e.g. YYDS key mirrored into moemail_api_key).
	reqBody := cloneMapAny(body)
	// Merge non-empty request overrides onto durable registration_config so
	// empty form fields still use last-saved mail/captcha/proxy secrets.
	body = mergeRegistrationStartBody(r.Context(), options, body)
	// Auto-persist last-used config WITHOUT writing adapter remaps into DB.
	// Writing remapped moemail_api_key permanently polluted MoeMail with AC-* keys
	// and cleared moemail_base_url → "MoeMail create failed 401 无效的 API Key".
	if options.Store != nil {
		persist := registrationConfigPatchForPersist(reqBody, body)
		if _, err := saveRegistrationConfig(r.Context(), options, persist, false); err != nil {
			// non-fatal: registration can still start with merged body
		}
	}
	idem := strings.TrimSpace(r.Header.Get("Idempotency-Key"))
	if idem == "" {
		idem = strings.TrimSpace(stringValue(body["idempotency_key"]))
	}
	payload, err := client.Start(r.Context(), body, idem)
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationReclaim(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	autoResume := true
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		if v, ok := body["auto_resume"].(bool); ok {
			autoResume = v
		}
	}
	payload, err := client.Reclaim(r.Context(), autoResume)
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveRegistrationStopAll(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	client := registrationClient(options)
	if client == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "registration service URL is not configured"})
		return
	}
	payload, err := client.StopAll(r.Context())
	if err != nil {
		writeRegistrationError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveAdminUpdateSettings(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	var patch map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&patch); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	settings, err := options.Store.UpdateRuntimeSettings(r.Context(), patch)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	options.applySettingsToRuntime(settings)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": settings})
}

func serveAdminSettings(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	settings, err := options.Store.PublicSettings(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "settings": settings})
}

func serveAdminLogs(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	query := r.URL.Query()
	items, err := options.Store.ListTasks(r.Context(), intQuery(query.Get("page"), 1), intQuery(query.Get("page_size"), 50), query.Get("q"), firstNonEmpty(query.Get("kind"), query.Get("action")), query.Get("status"))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, items)
}

func serveAdminLogActions(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	kinds, err := options.Store.ListTaskKinds(r.Context(), 50)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error(), "actions": kinds})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "actions": kinds, "kinds": kinds})
}

func serveUsageSummary(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	payload, err := options.Store.UsageSummary(r.Context(), intQuery(r.URL.Query().Get("days"), 7))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveUsageSeries(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	payload, err := options.Store.UsageSeries(r.Context(), intQuery(r.URL.Query().Get("days"), 7))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveUsageBreakdown(w http.ResponseWriter, r *http.Request, options Options, dim string) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	query := r.URL.Query()
	payload, err := options.Store.UsageBreakdown(r.Context(), dim, intQuery(query.Get("days"), 7), intQuery(query.Get("limit"), 50))
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func serveUsageEvents(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminRouteAllowed(w, r, options) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	query := r.URL.Query()
	var okFlag *bool
	switch strings.ToLower(strings.TrimSpace(query.Get("ok"))) {
	case "1", "true", "yes", "ok", "success":
		v := true
		okFlag = &v
	case "0", "false", "no", "fail", "failed", "error":
		v := false
		okFlag = &v
	}
	payload, err := options.Store.UsageEvents(r.Context(), intQuery(query.Get("page"), 1), intQuery(query.Get("page_size"), 50), map[string]string{
		"q": query.Get("q"), "api_key_id": query.Get("api_key_id"), "account_id": query.Get("account_id"), "model": query.Get("model"), "protocol": query.Get("protocol"), "client_ip": query.Get("client_ip"), "stream": query.Get("stream"),
	}, okFlag)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, payload)
}

func adminRouteAllowed(w http.ResponseWriter, r *http.Request, options Options) bool {
	if !options.AdminReadEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin read routes are not enabled"})
		return false
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return false
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return false
	}
	return true
}

func truthy(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

func intQuery(value string, fallback int) int {
	parsed, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return fallback
	}
	return parsed
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
}

func upstreamClient(options Options) *grok.Client {
	if options.Upstream != nil {
		return options.Upstream
	}
	return &grok.Client{BaseURL: options.Config.UpstreamBase}
}

// resolvePickMode returns the configured account rotation mode for failover chains.
// Sticky accounts are still forced first; mode only ranks the rest of the window.
func resolvePickMode(options Options) string {
	mode := "least_used"
	if options.Store != nil {
		if raw, err := options.Store.GetSetting(context.Background(), "account_mode"); err == nil {
			if s, ok := raw.(string); ok {
				if v := strings.ToLower(strings.TrimSpace(s)); v != "" {
					mode = v
				}
			}
		}
	}
	switch mode {
	case "round_robin", "random", "least_used":
		return mode
	default:
		return "least_used"
	}
}

// ensureOpenAIChatPromptCacheKey mints or recovers a multi-turn-stable prompt_cache_key
// for /v1/chat/completions. Order:
//  1. body / metadata / x-prompt-cache-key header
//  2. X-Session-Id / X-Grok-Conv-Id / thread headers
//  3. stable seed (system + first user) via anthropic.ExtractPromptCacheKey
//
// The key is written into chatReq.Raw so affinity + upstream both see it.
func ensureOpenAIChatPromptCacheKey(r *http.Request, chatReq *proxy.ChatRequest) string {
	if chatReq == nil {
		return ""
	}
	if chatReq.Raw == nil {
		chatReq.Raw = map[string]any{}
	}
	pck := strings.TrimSpace(stringValue(chatReq.Raw["prompt_cache_key"]))
	if pck == "" {
		if meta, _ := chatReq.Raw["metadata"].(map[string]any); meta != nil {
			pck = strings.TrimSpace(stringValue(meta["prompt_cache_key"]))
			if pck == "" {
				pck = strings.TrimSpace(stringValue(meta["session_id"]))
			}
			if pck == "" {
				if sid := anthropic.ExtractClaudeCodeSessionID(stringValue(meta["user_id"])); sid != "" {
					pck = sid
				}
			}
		}
	}
	if pck == "" && r != nil {
		for _, name := range []string{"X-Prompt-Cache-Key", "x-prompt-cache-key", "X-Session-Id", "x-session-id", "X-Grok-Conv-Id", "x-grok-conv-id", "X-Thread-Id", "x-thread-id"} {
			if v := strings.TrimSpace(r.Header.Get(name)); v != "" {
				switch strings.ToLower(name) {
				case "x-session-id":
					pck = "session:" + v
				case "x-grok-conv-id":
					pck = "conv:" + v
				case "x-thread-id":
					pck = "thread:" + v
				default:
					pck = v
				}
				break
			}
		}
	}
	if pck == "" {
		// Seed from first user + system (stable across growing history).
		if seed := strings.TrimSpace(anthropic.ExtractPromptCacheKey(chatReq.Raw)); seed != "" {
			pck = seed
		}
	}
	if pck != "" {
		chatReq.Raw["prompt_cache_key"] = pck
	}
	return pck
}

func listCandidatesForRequest(ctx context.Context, options Options, chatReq proxy.ChatRequest, headers http.Header) ([]pool.Candidate, error) {
	candidates, err := listCandidates(ctx, options)
	if err != nil {
		return nil, err
	}
	fp := proxy.ChatFingerprint(chatReq)
	if fp == "" {
		fp = proxy.ChatFingerprintFromHeaders(headers, chatReq.Model)
	}
	// If still empty, try raw prompt_cache_key alone.
	if fp == "" && chatReq.Raw != nil {
		if pck, _ := chatReq.Raw["prompt_cache_key"].(string); strings.TrimSpace(pck) != "" {
			fp = "chat:" + strings.TrimSpace(chatReq.Model) + ":prompt_cache_key:" + strings.TrimSpace(pck)
		}
	}
	sticky := stickyAccountID(ctx, options, fp)
	// Direct pck recovery when fingerprint shape differed or model was empty.
	if sticky == "" && chatReq.Raw != nil && options.AffinityStore != nil {
		if pck, _ := chatReq.Raw["prompt_cache_key"].(string); strings.TrimSpace(pck) != "" {
			pck = strings.TrimSpace(pck)
			if id, err := options.AffinityStore.GetAffinity(ctx, "chat:prompt_cache_key:"+pck); err == nil {
				sticky = strings.TrimSpace(id)
			}
			if sticky == "" && strings.TrimSpace(chatReq.Model) != "" {
				if id, err := options.AffinityStore.GetAffinity(ctx, "chat:"+strings.TrimSpace(chatReq.Model)+":prompt_cache_key:"+pck); err == nil {
					sticky = strings.TrimSpace(id)
				}
			}
		}
	}
	// Codex previous_response_id recovery when fingerprint miss.
	if sticky == "" && chatReq.Raw != nil {
		if prev, _ := chatReq.Raw["previous_response_id"].(string); strings.TrimSpace(prev) != "" {
			if acc, recoveredPCK := responseAffinityLookup(ctx, options, prev); acc != "" {
				sticky = acc
				// If pck was recovered, also ensure request carries it for upstream cache.
				if recoveredPCK != "" {
					if _, has := chatReq.Raw["prompt_cache_key"]; !has || strings.TrimSpace(stringValue(chatReq.Raw["prompt_cache_key"])) == "" {
						chatReq.Raw["prompt_cache_key"] = recoveredPCK
					}
				}
			}
		}
	}
	candidates = ensureStickyCandidate(ctx, options, candidates, sticky)
	// Pin sticky to front so prepareChain can skip a second Redis affinity GET.
	if sticky != "" {
		for i := range candidates {
			if candidates[i].ID == sticky {
				if i > 0 {
					cand := candidates[i]
					copy(candidates[1:i+1], candidates[0:i])
					candidates[0] = cand
				}
				// Massive least_used boost so Chain keeps sticky first.
				candidates[0].RequestCount -= 1_000_000_000
				break
			}
		}
	}
	return candidates, nil
}

func listCandidates(ctx context.Context, options Options) ([]pool.Candidate, error) {
	if len(options.Candidates) > 0 {
		out := make([]pool.Candidate, len(options.Candidates))
		copy(out, options.Candidates)
		return out, nil
	}
	if options.Store == nil {
		return nil, errors.New("PostgreSQL store unavailable")
	}
	return options.Store.ListPoolCandidates(ctx)
}

// ensureStickyCandidate injects an affinity-bound account into the pick window
// even when it is outside the top-N least_used scan. Critical for prompt-cache
// multi-turn (Codex prompt_cache_key / X-Grok-Conv-Id) high hit rates.
func ensureStickyCandidate(ctx context.Context, options Options, candidates []pool.Candidate, stickyID string) []pool.Candidate {
	stickyID = strings.TrimSpace(stickyID)
	if stickyID == "" || options.Store == nil {
		return candidates
	}
	for _, c := range candidates {
		if c.ID == stickyID {
			return candidates
		}
	}
	extra, err := options.Store.GetPoolCandidate(ctx, stickyID)
	if err != nil || extra == nil {
		return candidates
	}
	// Skip cooled-down / disabled / expired sticky pins — injecting them only
	// burns TTFT and forces a known-bad first hop every multi-turn request.
	if !extra.Eligible("", time.Now()) {
		return candidates
	}
	// Put sticky first so prepareChain affinity boost is redundant but safe.
	out := make([]pool.Candidate, 0, len(candidates)+1)
	out = append(out, *extra)
	out = append(out, candidates...)
	return out
}

func stickyAccountID(ctx context.Context, options Options, fingerprint string) string {
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" || options.AffinityStore == nil {
		return ""
	}
	id, err := options.AffinityStore.GetAffinity(ctx, fingerprint)
	if err == nil {
		if s := strings.TrimSpace(id); s != "" {
			return s
		}
	}
	// Fallbacks for fingerprint shape drift across turns / model aliasing.
	// chat:{model}:prompt_cache_key:{pck} -> chat:prompt_cache_key:{pck}
	if i := strings.Index(fingerprint, ":prompt_cache_key:"); i >= 0 {
		alt := "chat" + fingerprint[i:]
		if alt != fingerprint {
			if id, err := options.AffinityStore.GetAffinity(ctx, alt); err == nil {
				if s := strings.TrimSpace(id); s != "" {
					return s
				}
			}
		}
	}
	// chat:{model}:previous_response_id:{id} -> chat:previous_response_id:{id}
	if i := strings.Index(fingerprint, ":previous_response_id:"); i >= 0 {
		alt := "chat" + fingerprint[i:]
		if alt != fingerprint {
			if id, err := options.AffinityStore.GetAffinity(ctx, alt); err == nil {
				if s := strings.TrimSpace(id); s != "" {
					return s
				}
			}
		}
	}
	return ""
}

func modelCatalog(options Options) *models.Catalog {
	if options.Models != nil {
		return options.Models
	}
	return models.NewCatalog(config.Config{DefaultModel: "grok-4.5"}, nil)
}

func publicAPIBase(r *http.Request, port int) string {
	host := r.Host
	if forwarded := strings.TrimSpace(r.Header.Get("X-Forwarded-Host")); forwarded != "" {
		host = forwarded
	}
	proto := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto"))
	if proto == "" {
		proto = "http"
	}
	if strings.TrimSpace(host) == "" {
		host = "127.0.0.1"
		if port > 0 {
			host = host + ":" + itoaPort(port)
		}
	}
	return proto + "://" + host + "/v1"
}

func serveAdminSetAccountEnabled(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	enabled, ok := body["enabled"].(bool)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "enabled bool required"})
		return
	}
	rec, err := options.Store.SetAccountEnabled(r.Context(), r.PathValue("account_id"), enabled)
	if err != nil {
		if postgres.IsAccountNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": rec})
}

func serveAdminImportAccount(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	merge := true
	if v, ok := body["merge"].(bool); ok {
		merge = v
	}
	payload := body["payload"]
	if payload == nil {
		// allow bare auth object / token fields at top level
		payload = body
	}
	normalized := accounts.CollectNormalizedEntries(payload)
	if !normalized.OK {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "detail": normalized.Error, "error": normalized.Error})
		return
	}
	result, err := options.Store.ImportNormalizedAccounts(r.Context(), normalized.Normalized, merge)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if normalized.Format != "" {
		result["format"] = normalized.Format
	}
	writeJSON(w, http.StatusOK, result)
}

func serveAdminExportAccounts(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	includeSecrets := r.URL.Query().Get("include_secrets") != "0" && r.URL.Query().Get("include_secrets") != "false"
	asyncJob := truthy(r.URL.Query().Get("async_job"))
	result, err := options.Store.ExportAuthMap(r.Context(), nil, includeSecrets)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if asyncJob {
		writeJSON(w, http.StatusOK, startExportJob(result, "grok2api-auth-export.json"))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func serveAdminExportAccountsBatch(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	includeSecrets := true
	if v, ok := body["include_secrets"].(bool); ok {
		includeSecrets = v
	}
	ids := stringSlice(body["ids"])
	asyncJob := truthy(r.URL.Query().Get("async_job")) || truthy(fmt.Sprint(body["async_job"]))
	result, err := options.Store.ExportAuthMap(r.Context(), ids, includeSecrets)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if asyncJob {
		name := fmt.Sprintf("grok2api-auth-export-selected-%d.json", len(ids))
		writeJSON(w, http.StatusOK, startExportJob(result, name))
		return
	}
	writeJSON(w, http.StatusOK, result)
}

func serveAdminDeleteAccount(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	accountID := r.PathValue("account_id")
	if strings.HasPrefix(accountID, "register-email") || strings.Contains(accountID, "/register-email") {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Not found"})
		return
	}
	ok, err := options.Store.DeleteAccount(r.Context(), accountID)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Account not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func serveAdminDeleteAccountsBatch(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	ids := stringSlice(body["ids"])
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "ids is required"})
		return
	}
	if len(ids) > 2000 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "too many ids (max 2000)"})
		return
	}
	result, err := options.Store.DeleteAccounts(r.Context(), ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	result["ok"] = true
	writeJSON(w, http.StatusOK, result)
}

func serveAdminClearAllAccounts(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	n, err := options.Store.ClearAllAccounts(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"message": "已清空账号池",
		"removed": n,
	})
}

func stringSlice(value any) []string {
	switch v := value.(type) {
	case []any:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s := stringValue(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	case []string:
		out := make([]string, 0, len(v))
		for _, item := range v {
			if s := strings.TrimSpace(item); s != "" {
				out = append(out, s)
			}
		}
		return out
	default:
		return nil
	}
}

func serveAdminProbeBatch(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.ModelHealth == nil || options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "model health unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	ids := stringSlice(body["ids"])
	if len(ids) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "ids is empty"})
		return
	}
	if len(ids) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "too many ids (max 500)"})
		return
	}
	model := stringValue(body["model"])
	autoDisable := true
	if v, ok := body["auto_disable"].(bool); ok {
		autoDisable = v
	}
	results := options.ModelHealth.ProbeIDs(r.Context(), ids, model, autoDisable, "manual")
	// attach pool views
	out := make([]map[string]any, 0, len(results))
	for _, item := range results {
		aid := stringValue(item["account_id"])
		if pool, err := options.Store.GetAccountPoolView(r.Context(), aid); err == nil {
			item["pool"] = pool
		}
		out = append(out, item)
	}
	// 新账号批量测活：对尚无 last_quota 的账号补拉类型 + 额度（最多 25，落库）。
	attachQuotasAfterProbeBatch(r.Context(), options, out)
	// Quota fetch can race-mark free exhaust and cool; undo for probes that passed.
	if options.Store != nil {
		for _, item := range out {
			if item == nil {
				continue
			}
			ok := item["ok"] == true
			if res, _ := item["result"].(map[string]any); res != nil {
				if res["ok"] == true || res["available"] == true {
					ok = true
				}
			}
			if !ok {
				continue
			}
			aid := strings.TrimSpace(stringValue(item["account_id"]))
			if aid == "" {
				continue
			}
			if _, err := options.Store.ClearAccountCooldown(r.Context(), aid); err == nil {
				if pool, _ := item["pool"].(map[string]any); pool != nil {
					pool["pool_status"] = "normal"
					pool["in_cooldown"] = false
					pool["cooldown_until"] = nil
					pool["cooldown_code"] = nil
					pool["cooldown_reason"] = nil
				}
				if q, _ := item["quota"].(map[string]any); q != nil {
					q["exhausted"] = false
					q["auto_disabled"] = false
					q["in_cooldown"] = false
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "results": out, "count": len(out)})
}

func serveAdminProbeAll(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.ModelHealth == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "model health unavailable"})
		return
	}
	// Default: async multi-wave job so large pools are not truncated by one
	// 150s budget / HTTP timeout. ?sync=1 forces a blocking single response
	// (still multi-wave until covered or job timeout).
	sync := r.URL.Query().Get("sync") == "1" || r.URL.Query().Get("sync") == "true"
	if sync {
		// Bound the request context so a hung upstream cannot pin the handler forever.
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Minute)
		defer cancel()
		result := options.ModelHealth.RunOnce(ctx, "manual_all")
		writeJSON(w, http.StatusOK, result)
		return
	}
	result := options.ModelHealth.StartProbeAll()
	writeJSON(w, http.StatusOK, result)
}

func serveModelHealthStatus(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.ModelHealth == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "implementation": "go", "started": false})
		return
	}
	writeJSON(w, http.StatusOK, options.ModelHealth.Status())
}

func serveMaintainerStatus(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Maintainer == nil {
		writeJSON(w, http.StatusOK, map[string]any{"enabled": false, "implementation": "go", "started": false})
		return
	}
	writeJSON(w, http.StatusOK, options.Maintainer.Status())
}

func serveMaintainerRun(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Maintainer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "maintainer unavailable"})
		return
	}
	force := true
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
		if v, ok := body["force"].(bool); ok {
			force = v
		}
	}
	writeJSON(w, http.StatusOK, options.Maintainer.RunOnce(r.Context(), force))
}

func serveAccountsRefresh(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Maintainer == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "maintainer unavailable"})
		return
	}
	force := true
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	if v, ok := body["force"].(bool); ok {
		force = v
	}
	ids := stringSlice(body["ids"])
	if len(ids) == 0 {
		ids = stringSlice(body["account_ids"])
	}
	var result map[string]any
	if len(ids) > 0 {
		// Selected-account renew: refresh only those ids and return per-row results
		// so the admin UI can paint immediately (busy rows / toast / pool patch).
		result = options.Maintainer.RunForIDs(r.Context(), ids, force)
	} else {
		result = options.Maintainer.RunOnce(r.Context(), force)
	}
	result["maintainer"] = options.Maintainer.Status()
	result["token_maintainer"] = result["maintainer"]
	writeJSON(w, http.StatusOK, result)
}

func serveToggleTokenMaintain(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	enabled, ok := body["enabled"].(bool)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "enabled bool required"})
		return
	}
	if err := options.Store.SetSetting(r.Context(), "token_maintain_enabled", enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if options.Maintainer != nil {
		if enabled {
			options.Maintainer.Start()
			options.Maintainer.RequestRunSoon(false)
		} else {
			options.Maintainer.Stop()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                     true,
		"token_maintain_enabled": enabled,
		"settings":               map[string]any{"token_maintain_enabled": enabled},
		"maintainer":             serviceStatus(options.Maintainer, options),
		"token_maintainer":       serviceStatus(options.Maintainer, options),
	})
}

func serveToggleModelHealth(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	enabled, ok := body["enabled"].(bool)
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "enabled bool required"})
		return
	}
	if err := options.Store.SetSetting(r.Context(), "model_health_enabled", enabled); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if options.ModelHealth != nil {
		if enabled {
			options.ModelHealth.Start()
			options.ModelHealth.RequestRunSoon()
		} else {
			options.ModelHealth.Stop()
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"model_health_enabled": enabled,
		"settings":             map[string]any{"model_health_enabled": enabled},
		"model_health":         serviceStatus(options.ModelHealth, options),
	})
}

func serveSetAccountMode(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	mode := strings.ToLower(stringValue(body["mode"]))
	switch mode {
	case "round_robin", "random", "least_used":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "Invalid account_mode. Use one of: round_robin, random, least_used"})
		return
	}
	if err := options.Store.SetSetting(r.Context(), "account_mode", mode); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account_mode": mode, "modes": []string{"round_robin", "random", "least_used"}})
}

func serveChangeAdminPassword(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	current := stringValue(body["current_password"])
	newPW := stringValue(body["new_password"])
	confirm := stringValue(body["confirm_password"])
	if len(newPW) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "password must contain at least 4 characters"})
		return
	}
	if confirm != "" && confirm != newPW {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "两次输入的新密码不一致"})
		return
	}
	ok, err := verifyAdminPassword(r.Context(), options, current)
	if err != nil || !ok {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "当前密码不正确"})
		return
	}
	hash, salt, err := adminauth.NewPassword(newPW)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if err := options.Store.SetAdminPassword(r.Context(), hash, salt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	settings, _ := options.Store.PublicSettings(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "message": "密码已更新", "settings": settings})
}

func servePruneModelBlocks(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	n, err := options.Store.PruneModelBlocks(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "pruned": n})
}

func serveExportAccountsSSO(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	// Before full SSO download: backfill any registration-session SSO that has not
	// yet landed as top-level sso on the durable account row (new registers).
	reconciled := reconcileRegistrationSSOIntoStore(r.Context(), options)
	authMap, err := options.Store.ExportAuthMap(r.Context(), nil, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	includePassword := truthy(r.URL.Query().Get("include_password"))
	out := buildSSOExport(authMap, includePassword)
	if reconciled > 0 {
		out["reconciled"] = reconciled
	}
	writeJSON(w, http.StatusOK, out)
}

func serveExportAccountsSSOSelected(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	ids := stringSlice(body["ids"])
	includePassword := truthyAny(body["include_password"])
	// Same reconcile as export-all so newly registered SSO is durable before filter.
	_ = reconcileRegistrationSSOIntoStore(r.Context(), options)
	authMap, err := options.Store.ExportAuthMap(r.Context(), ids, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, buildSSOExport(authMap, includePassword))
}

// serveExportRegistrationSSO exports SSO cookies from live registration sessions
// and/or durable account payloads (v1.9.78 / v1.9.81 Python parity).
// Admin UI: POST /accounts/register-email/export-sso under the SSO import panel.
func serveExportRegistrationSSO(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}

	// Collect filters from query (GET) and/or JSON body (POST). Body wins on conflict.
	q := r.URL.Query()
	fmtName := strings.TrimSpace(strings.ToLower(q.Get("format")))
	if fmtName == "" {
		fmtName = "sso"
	}
	batchID := strings.TrimSpace(q.Get("batch_id"))
	includePassword := truthy(q.Get("include_password"))
	download := truthy(q.Get("download"))
	if r.Method == http.MethodGet && q.Get("download") == "" {
		download = true // Python GET default download=1
	}
	wantStatus := map[string]struct{}{}
	for _, part := range strings.Split(q.Get("status"), ",") {
		part = strings.TrimSpace(strings.ToLower(part))
		if part != "" {
			wantStatus[part] = struct{}{}
		}
	}
	wantIDs := map[string]struct{}{}

	if r.Method == http.MethodPost || r.Method == http.MethodPut {
		var body map[string]any
		decoder := json.NewDecoder(r.Body)
		decoder.UseNumber()
		if err := decoder.Decode(&body); err != nil && !errors.Is(err, io.EOF) {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		if body != nil {
			if v := strings.TrimSpace(strings.ToLower(stringValue(body["format"]))); v != "" {
				fmtName = v
			}
			if v := strings.TrimSpace(stringValue(body["batch_id"])); v != "" {
				batchID = v
			}
			if _, ok := body["include_password"]; ok {
				includePassword = truthyAny(body["include_password"])
			}
			if _, ok := body["download"]; ok {
				download = truthyAny(body["download"])
			}
			for _, s := range stringSlice(body["status"]) {
				s = strings.TrimSpace(strings.ToLower(s))
				if s != "" {
					wantStatus[s] = struct{}{}
				}
			}
			for _, id := range stringSlice(body["session_ids"]) {
				id = strings.TrimSpace(id)
				if id != "" {
					wantIDs[id] = struct{}{}
				}
			}
			// Also accept ids as alias for session_ids.
			for _, id := range stringSlice(body["ids"]) {
				id = strings.TrimSpace(id)
				if id != "" {
					wantIDs[id] = struct{}{}
				}
			}
		}
	}
	switch fmtName {
	case "sso", "cookie", "email_sso", "email_password_sso", "json":
	default:
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "unsupported format: " + fmtName})
		return
	}
	if fmtName == "email_password_sso" {
		includePassword = true
	}

	type ssoRow struct {
		ID        string
		AccountID string
		BatchID   any
		Status    any
		Email     string
		Password  string
		SSO       string
		Source    string
	}
	rows := make([]ssoRow, 0, 64)

	// 1) Live registration sessions (optional — service may be down/unconfigured).
	if client := registrationClient(options); client != nil {
		if payload, err := client.Sessions(r.Context()); err == nil && payload != nil {
			var sessions []any
			switch v := payload["sessions"].(type) {
			case []any:
				sessions = v
			case []map[string]any:
				sessions = make([]any, len(v))
				for i, m := range v {
					sessions[i] = m
				}
			}
			for _, raw := range sessions {
				sess, _ := raw.(map[string]any)
				if sess == nil {
					continue
				}
				sid := strings.TrimSpace(stringValue(sess["id"]))
				if len(wantIDs) > 0 {
					if _, ok := wantIDs[sid]; !ok {
						continue
					}
				}
				if batchID != "" && strings.TrimSpace(stringValue(sess["batch_id"])) != batchID {
					continue
				}
				st := strings.TrimSpace(strings.ToLower(stringValue(sess["status"])))
				if len(wantStatus) > 0 {
					if _, ok := wantStatus[st]; !ok {
						continue
					}
				}
				sso := strings.TrimSpace(stringValue(sess["sso"]))
				if sso == "" {
					if cookies, ok := sess["session_cookies"].(map[string]any); ok {
						sso = strings.TrimSpace(firstNonEmpty(stringValue(cookies["sso"]), stringValue(cookies["sso-rw"])))
					}
				}
				if sso == "" {
					// Also try accounts.GetSSOValue on the session map.
					sso = accounts.GetSSOValue(sess)
				}
				if sso == "" {
					continue
				}
				email := strings.TrimSpace(stringValue(sess["email"]))
				password := strings.TrimSpace(stringValue(sess["password"]))
				if !includePassword {
					password = ""
				}
				rows = append(rows, ssoRow{
					ID: sid, BatchID: sess["batch_id"], Status: sess["status"],
					Email: email, Password: password, SSO: sso, Source: "session",
				})
			}
		}
	}

	// 2) Durable account store fallback / supplement (survives session expiry).
	if options.Store != nil {
		if authMap, err := options.Store.ExportAuthMap(r.Context(), nil, true); err == nil {
			auth, _ := authMap["auth"].(map[string]any)
			for aid, raw := range auth {
				entry, _ := raw.(map[string]any)
				if entry == nil {
					continue
				}
				sso := accounts.GetSSOValue(entry)
				if sso == "" {
					continue
				}
				if len(wantIDs) > 0 {
					sidHit := strings.TrimSpace(stringValue(entry["registration_session_id"]))
					if _, okID := wantIDs[aid]; !okID {
						if _, okSess := wantIDs[sidHit]; !okSess {
							continue
						}
					}
				}
				if batchID != "" {
					bid := strings.TrimSpace(stringValue(entry["registration_batch_id"]))
					if bid != "" && bid != batchID {
						continue
					}
					// Accounts without batch id are only included when no batch filter
					// was set; with filter, skip empty bid (stricter match).
					if bid == "" {
						continue
					}
				}
				if len(wantStatus) > 0 {
					// Account-store SSO is always from completed imports.
					_, okDone := wantStatus["done"]
					_, okImported := wantStatus["imported"]
					if !okDone && !okImported {
						continue
					}
				}
				email := strings.TrimSpace(stringValue(entry["email"]))
				password := strings.TrimSpace(firstNonEmpty(stringValue(entry["password"]), stringValue(entry["register_password"])))
				if !includePassword {
					password = ""
				}
				rows = append(rows, ssoRow{
					ID:        firstNonEmpty(strings.TrimSpace(stringValue(entry["registration_session_id"])), aid),
					AccountID: aid,
					BatchID:   entry["registration_batch_id"],
					Status:    "imported",
					Email:     email,
					Password:  password,
					SSO:       sso,
					Source:    "account",
				})
			}
		}
	}

	if len(rows) == 0 {
		// Business "nothing to export" — not a missing route. Return 200 so the
		// admin UI can show a clear empty state instead of treating it as 404.
		tsEmpty := time.Now().Format("20060102-150405")
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":          false,
			"count":       0,
			"matched":     0,
			"format":      fmtName,
			"batch_id":    nullIfEmpty(batchID),
			"exported_at": tsEmpty,
			"text":        "",
			"content":     "",
			"lines":       []string{},
			"items":       []any{},
			"detail":      "no registration sessions or accounts with SSO cookie matched filters",
		})
		return
	}

	// De-dupe by sso value, keep first row.
	seen := map[string]struct{}{}
	unique := make([]ssoRow, 0, len(rows))
	for _, row := range rows {
		key := row.SSO
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		unique = append(unique, row)
	}

	ts := time.Now().Format("20060102-150405")
	if fmtName == "json" {
		items := make([]map[string]any, 0, len(unique))
		for _, row := range unique {
			item := map[string]any{
				"id": row.ID, "status": row.Status, "email": row.Email,
				"sso": row.SSO, "source": row.Source, "batch_id": row.BatchID,
			}
			if row.AccountID != "" {
				item["account_id"] = row.AccountID
			}
			if includePassword {
				item["password"] = row.Password
			}
			items = append(items, item)
		}
		payload := map[string]any{
			"ok": true, "count": len(unique), "matched": len(rows),
			"format": fmtName, "batch_id": nullIfEmpty(batchID),
			"exported_at": ts, "items": items,
		}
		if download {
			raw, _ := json.MarshalIndent(payload, "", "  ")
			filename := "grok2api-sso-export-" + ts + ".json"
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
			w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
			w.Header().Set("X-Export-Count", strconv.Itoa(len(unique)))
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(raw)
			return
		}
		writeJSON(w, http.StatusOK, payload)
		return
	}

	lines := make([]string, 0, len(unique))
	for _, row := range unique {
		switch fmtName {
		case "cookie":
			lines = append(lines, "sso="+row.SSO)
		case "email_sso":
			if row.Email != "" {
				lines = append(lines, row.Email+"\t"+row.SSO)
			} else {
				lines = append(lines, row.SSO)
			}
		case "email_password_sso":
			if row.Email != "" && row.Password != "" {
				lines = append(lines, row.Email+":"+row.Password+":"+row.SSO)
			} else if row.Email != "" {
				lines = append(lines, row.Email+"::"+row.SSO)
			} else {
				lines = append(lines, row.SSO)
			}
		default: // sso raw
			lines = append(lines, row.SSO)
		}
	}
	textBody := strings.Join(lines, "\n")
	if textBody != "" {
		textBody += "\n"
	}
	filename := "grok2api-sso-export-" + ts + ".txt"
	if download {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		w.Header().Set("Content-Disposition", `attachment; filename="`+filename+`"`)
		w.Header().Set("X-Export-Count", strconv.Itoa(len(unique)))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(textBody))
		return
	}
	// Redacted preview items for JSON response (full sso stays in text/lines).
	preview := make([]map[string]any, 0, len(unique))
	for _, row := range unique {
		ssoPrev := row.SSO
		if len(ssoPrev) > 24 {
			ssoPrev = ssoPrev[:24] + "..."
		}
		preview = append(preview, map[string]any{
			"email": row.Email, "status": row.Status, "sso": ssoPrev, "source": row.Source,
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(unique), "matched": len(rows),
		"format": fmtName, "batch_id": nullIfEmpty(batchID),
		"exported_at": ts, "text": textBody, "content": textBody,
		"lines": lines, "items": preview, "filename": filename,
	})
}

func nullIfEmpty(s string) any {
	if strings.TrimSpace(s) == "" {
		return nil
	}
	return s
}

func serveAdminImportFile(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	if err := r.ParseMultipartForm(32 << 20); err != nil {
		// also accept JSON body {payload, merge}
		var body map[string]any
		if jerr := json.NewDecoder(r.Body).Decode(&body); jerr != nil {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
			return
		}
		merge := true
		if v, ok := body["merge"].(bool); ok {
			merge = v
		}
		norm := accounts.CollectNormalizedEntries(body["payload"])
		if !norm.OK {
			writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": norm.Error})
			return
		}
		result, err := options.Store.ImportNormalizedAccounts(r.Context(), norm.Normalized, merge)
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, result)
		return
	}
	merge := true
	if v := r.FormValue("merge"); v == "0" || strings.EqualFold(v, "false") {
		merge = false
	}
	file, _, err := r.FormFile("file")
	if err != nil {
		// try "files"
		file, _, err = r.FormFile("files")
	}
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "file required"})
		return
	}
	defer file.Close()
	raw, err := io.ReadAll(io.LimitReader(file, 16<<20))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	norm := accounts.CollectNormalizedEntries(string(raw))
	if !norm.OK {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": norm.Error})
		return
	}
	result, err := options.Store.ImportNormalizedAccounts(r.Context(), norm.Normalized, merge)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if norm.Format != "" {
		result["format"] = norm.Format
	}
	writeJSON(w, http.StatusOK, result)
}

func serveAdminImportFiles(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	if err := r.ParseMultipartForm(64 << 20); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	merge := true
	if v := r.FormValue("merge"); v == "0" || strings.EqualFold(v, "false") {
		merge = false
	}
	files := r.MultipartForm.File["files"]
	if len(files) == 0 {
		files = r.MultipartForm.File["file"]
	}
	if len(files) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "files required"})
		return
	}
	normalized := map[string]map[string]any{}
	fileResults := []map[string]any{}
	parseErrors := 0
	for i, fh := range files {
		f, err := fh.Open()
		if err != nil {
			parseErrors++
			fileResults = append(fileResults, map[string]any{"index": i + 1, "ok": false, "error": err.Error()})
			continue
		}
		raw, _ := io.ReadAll(io.LimitReader(f, 16<<20))
		f.Close()
		norm := accounts.CollectNormalizedEntries(string(raw))
		if !norm.OK {
			parseErrors++
			fileResults = append(fileResults, map[string]any{"index": i + 1, "ok": false, "error": norm.Error, "format": norm.Format})
			continue
		}
		for k, v := range norm.Normalized {
			normalized[k] = v
		}
		fileResults = append(fileResults, map[string]any{"index": i + 1, "ok": true, "count": len(norm.Normalized), "format": norm.Format})
	}
	if len(normalized) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "no valid account entries found", "file_results": fileResults, "parse_errors": parseErrors})
		return
	}
	result, err := options.Store.ImportNormalizedAccounts(r.Context(), normalized, merge)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	// Heal free-usage rows that were mis-tagged as model_blocked (cooldown fix).
	if n, rerr := options.Store.RepairFreeUsageModelBlocks(r.Context()); rerr == nil && n > 0 {
		result["cooldown_repaired"] = n
	}
	result["files"] = len(files)
	result["parse_errors"] = parseErrors
	result["file_results"] = fileResults
	writeJSON(w, http.StatusOK, result)
}

func serveAdminNormalizeAccounts(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	result, err := options.Store.NormalizeAccountKeys(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, result)
}

// fetchUpstreamModels pulls /models from xAI using a live account token.
// Does NOT write the database — callers preview or save separately.
func fetchUpstreamModels(ctx context.Context, options Options) (items []map[string]any, viaEmail string, origin string, err error) {
	if options.Store == nil {
		return nil, "", "", errors.New("store unavailable")
	}
	authList, err := options.Store.ListAccountAuths(ctx, 20, true)
	if err != nil {
		return nil, "", "", err
	}
	if len(authList) == 0 {
		return nil, "", "", errors.New("no live account for models fetch")
	}
	a := authList[0]
	origin = strings.TrimRight(options.Config.UpstreamBase, "/") + "/models"
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, origin, nil)
	if err != nil {
		return nil, "", origin, err
	}
	gc := upstreamClient(options)
	for k, v := range gc.Headers(a.Token, options.runtimeConfig().DefaultModel) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, a.Email, origin, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		return nil, a.Email, origin, fmt.Errorf("upstream %d: %s", resp.StatusCode, string(body)[:minInt(300, len(body))])
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		return nil, a.Email, origin, fmt.Errorf("parse: %w", err)
	}
	items = parseUpstreamModels(payload)
	// Always keep local extras (coding/build/search aliases) even when upstream list is tiny.
	items = ensureLocalModelExtras(items, options.runtimeConfig().DefaultModel)
	if len(items) == 0 {
		return nil, a.Email, origin, errors.New("no models in upstream response")
	}
	return items, a.Email, origin, nil
}

// serveAdminModelsFetch: 仅从上游获取模型列表（预览，不写库）。
func serveAdminModelsFetch(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	items, via, origin, err := fetchUpstreamModels(r.Context(), options)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"preview":        true,
		"saved":          false,
		"count":          len(items),
		"upstream_count": len(items),
		"fetched_via":    via,
		"origin":         origin,
		"models":         items,
		"data":           items,
		"default_model":  options.runtimeConfig().DefaultModel,
		"message":        fmt.Sprintf("已从上游获取 %d 个模型（未写入数据库，请点「保存到数据库」）", len(items)),
	})
}

// serveAdminModelsSave: 将预览列表或请求体中的模型目录写入 PostgreSQL。
func serveAdminModelsSave(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(io.LimitReader(r.Body, 8<<20)).Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	var items []map[string]any
	// Prefer explicit list from client preview.
	if raw, ok := body["models"].([]any); ok && len(raw) > 0 {
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				items = append(items, m)
			}
		}
	} else if raw, ok := body["data"].([]any); ok && len(raw) > 0 {
		for _, it := range raw {
			if m, ok := it.(map[string]any); ok {
				items = append(items, m)
			}
		}
	}
	via := strings.TrimSpace(stringValue(body["fetched_via"]))
	origin := strings.TrimSpace(stringValue(body["origin"]))
	// Empty body → re-fetch from upstream then save (convenience).
	if len(items) == 0 {
		fetched, email, org, err := fetchUpstreamModels(r.Context(), options)
		if err != nil {
			writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
			return
		}
		items = fetched
		if via == "" {
			via = email
		}
		if origin == "" {
			origin = org
		}
	}
	items = ensureLocalModelExtras(items, options.runtimeConfig().DefaultModel)
	if len(items) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "no models to save"})
		return
	}
	meta := map[string]any{
		"source":      "admin_save",
		"fetched_via": via,
		"origin":      origin,
		"saved_at":    time.Now().Unix(),
	}
	n, err := options.Store.ReplaceModels(r.Context(), items, meta)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	catalog := modelCatalog(options).PublicModels(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"saved":          true,
		"preview":        false,
		"count":          len(catalog),
		"pg_count":       n,
		"upstream_count": len(items),
		"fetched_via":    via,
		"origin":         origin,
		"storage":        "postgres",
		"models":         catalog,
		"data":           catalog,
		"default_model":  options.runtimeConfig().DefaultModel,
		"message":        fmt.Sprintf("已保存 %d 个模型到数据库", n),
	})
}

// serveAdminModelsSync: 一键获取并写入数据库（兼容旧 UI）。
func serveAdminModelsSync(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	items, via, origin, err := fetchUpstreamModels(r.Context(), options)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	n, err := options.Store.ReplaceModels(r.Context(), items, map[string]any{
		"source": "upstream", "fetched_via": via, "origin": origin,
	})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	catalog := modelCatalog(options).PublicModels(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "saved": true, "count": len(catalog), "pg_count": n, "upstream_count": len(items),
		"fetched_via": via, "origin": origin, "storage": "postgres", "models": catalog, "data": catalog,
		"message": fmt.Sprintf("已同步并保存 %d 个模型到数据库", n),
	})
}

// attachQuotaAfterProbe live-fetches account type + quota usage and merges into
// the probe pool view so 新导入/测活账号 immediately show Free/SuperGrok + usage.
// FetchOne already persists last_quota asynchronously (merge-safe with prior snap).
// Returns the quota item (may be nil) for the JSON response body.
func attachQuotaAfterProbe(ctx context.Context, options Options, accountID string, poolView map[string]any) map[string]any {
	accountID = strings.TrimSpace(accountID)
	if options.Quota == nil || accountID == "" {
		return nil
	}
	if ctx == nil {
		ctx = context.Background()
	}
	// Bound billing+token probe so a slow upstream cannot pin the admin click forever.
	qctx, cancel := context.WithTimeout(ctx, 22*time.Second)
	defer cancel()
	item, err := options.Quota.FetchOne(qctx, accountID)
	if err != nil || item == nil {
		return nil
	}
	// Model probe already proved the account is live. A follow-up free-token probe
	// can race-report exhausted=0 remaining and cool the account out of rotation.
	// Undo that cool so freshly registered / just-probed accounts stay 轮询中.
	probeOK := false
	if poolView != nil {
		if poolView["pool_status"] == "normal" || poolView["last_probe_status"] == "ok" {
			probeOK = true
		}
		if lp, _ := poolView["last_probe"].(map[string]any); lp != nil {
			if lp["ok"] == true || lp["available"] == true {
				probeOK = true
			}
		}
	}
	if probeOK && options.Store != nil {
		if item["exhausted"] == true || item["auto_disabled"] == true {
			// Keep usage numbers for UI, but clear false exhaust cool.
			item["exhausted"] = false
			item["auto_disabled"] = false
			delete(item, "exhaust_reason")
			item["in_cooldown"] = false
			if p, _ := item["pool"].(map[string]any); p != nil {
				p["pool_status"] = "normal"
				p["in_cooldown"] = false
			}
		}
		if _, err := options.Store.ClearAccountCooldown(qctx, accountID); err == nil {
			// re-enter rotation
		}
	}
	// Strip nested pool from quota item before embedding (cycle risk / noise).
	quotaSnap := make(map[string]any, len(item))
	for k, v := range item {
		if k == "pool" {
			continue
		}
		quotaSnap[k] = v
	}
	if poolView != nil {
		poolView["last_quota"] = quotaSnap
		// Promote type onto pool for list paint convenience.
		if at := strings.TrimSpace(fmt.Sprint(quotaSnap["account_type"])); at != "" && at != "<nil>" {
			poolView["account_type"] = at
			poolView["plan"] = at
		}
		if pl := strings.TrimSpace(fmt.Sprint(quotaSnap["plan_label"])); pl != "" && pl != "<nil>" {
			poolView["plan_label"] = pl
		}
		// IMPORTANT: do NOT paint cooldown from a concurrent quota fetch when model
		// probe just succeeded. Fresh free accounts often get a false "exhausted"
		// from a secondary token probe / header race; kicking them out of rotation
		// after a green 测活 is wrong. Only surface cool when probe already failed
		// or pool was already cool and quota independently confirms exhaust.
		probeOK := poolView["pool_status"] == "normal" || poolView["last_probe_status"] == "ok"
		if !probeOK && (quotaSnap["exhausted"] == true || quotaSnap["auto_disabled"] == true) {
			if poolView["in_cooldown"] != true {
				poolView["in_cooldown"] = true
				if ps, _ := poolView["pool_status"].(string); ps == "" || ps == "normal" {
					poolView["pool_status"] = "cooldown"
				}
			}
		}
	}
	return quotaSnap
}

// attachQuotasAfterProbeBatch fills missing last_quota for a probe-batch result set.
// Caps live fetches to 25 (same as FetchByIDs) — large batches rely on auto-refresh.
func attachQuotasAfterProbeBatch(ctx context.Context, options Options, results []map[string]any) {
	if len(results) == 0 {
		return
	}
	// Prefer accounts with no durable last_quota yet (newly imported).
	need := make([]string, 0, len(results))
	seen := map[string]struct{}{}
	for _, item := range results {
		if item == nil {
			continue
		}
		aid := strings.TrimSpace(stringValue(item["account_id"]))
		if aid == "" {
			continue
		}
		pool, _ := item["pool"].(map[string]any)
		if pool == nil {
			pool = map[string]any{}
			item["pool"] = pool
		}
		lq, _ := pool["last_quota"].(map[string]any)
		if len(lq) > 0 {
			// Already has durable snap — still promote type for UI if present.
			if at := strings.TrimSpace(fmt.Sprint(lq["account_type"])); at != "" && at != "<nil>" {
				item["account_type"] = at
				item["plan"] = at
			}
			if pl := strings.TrimSpace(fmt.Sprint(lq["plan_label"])); pl != "" && pl != "<nil>" {
				item["plan_label"] = pl
			}
			continue
		}
		if options.Quota == nil {
			continue
		}
		if _, ok := seen[aid]; ok {
			continue
		}
		seen[aid] = struct{}{}
		need = append(need, aid)
		if len(need) >= 25 {
			break
		}
	}
	if options.Quota == nil || len(need) == 0 {
		return
	}
	qctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	out, err := options.Quota.FetchByIDs(qctx, need)
	if err != nil || out == nil {
		// Fallback: sequential FetchOne for a couple of ids only.
		for i, aid := range need {
			if i >= 5 {
				break
			}
			snap := attachQuotaAfterProbe(qctx, options, aid, nil)
			if snap == nil {
				continue
			}
			for _, item := range results {
				if strings.TrimSpace(stringValue(item["account_id"])) != aid {
					continue
				}
				pool, _ := item["pool"].(map[string]any)
				if pool == nil {
					pool = map[string]any{}
					item["pool"] = pool
				}
				pool["last_quota"] = snap
				item["quota"] = snap
				if at := strings.TrimSpace(fmt.Sprint(snap["account_type"])); at != "" && at != "<nil>" {
					item["account_type"] = at
					item["plan"] = at
					pool["account_type"] = at
				}
			}
		}
		return
	}
	rows, _ := out["results"].([]map[string]any)
	if rows == nil {
		if arr, ok := out["results"].([]any); ok {
			for _, raw := range arr {
				if m, ok := raw.(map[string]any); ok {
					rows = append(rows, m)
				}
			}
		}
		if rows == nil {
			if arr, ok := out["accounts"].([]map[string]any); ok {
				rows = arr
			}
		}
	}
	byID := map[string]map[string]any{}
	for _, row := range rows {
		if row == nil {
			continue
		}
		id := strings.TrimSpace(stringValue(row["account_id"]))
		if id == "" {
			id = strings.TrimSpace(stringValue(row["id"]))
		}
		if id == "" {
			continue
		}
		snap := make(map[string]any, len(row))
		for k, v := range row {
			if k == "pool" {
				continue
			}
			snap[k] = v
		}
		byID[id] = snap
	}
	for _, item := range results {
		if item == nil {
			continue
		}
		aid := strings.TrimSpace(stringValue(item["account_id"]))
		snap := byID[aid]
		if snap == nil {
			continue
		}
		pool, _ := item["pool"].(map[string]any)
		if pool == nil {
			pool = map[string]any{}
			item["pool"] = pool
		}
		pool["last_quota"] = snap
		item["quota"] = snap
		if at := strings.TrimSpace(fmt.Sprint(snap["account_type"])); at != "" && at != "<nil>" {
			item["account_type"] = at
			item["plan"] = at
			pool["account_type"] = at
			pool["plan"] = at
		}
		if pl := strings.TrimSpace(fmt.Sprint(snap["plan_label"])); pl != "" && pl != "<nil>" {
			item["plan_label"] = pl
			pool["plan_label"] = pl
		}
	}
}

func serveAdminAccountsQuota(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Quota == nil {
		// fallback store-only cached
		if options.Store == nil {
			writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
			return
		}
		out, err := options.Store.ListCachedQuotas(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	cached := r.URL.Query().Get("cached") == "1" || r.URL.Query().Get("cached") == "true"
	refresh := r.URL.Query().Get("refresh") == "1" || r.URL.Query().Get("refresh") == "true"
	// Optional scope: only probe these account ids (visible page / missing quota).
	// Avoids multi-thousand full-pool stampede when UI only needs current page.
	idsParam := strings.TrimSpace(r.URL.Query().Get("ids"))
	var scopeIDs []string
	if idsParam != "" {
		for _, part := range strings.Split(idsParam, ",") {
			id := strings.TrimSpace(part)
			if id != "" {
				scopeIDs = append(scopeIDs, id)
			}
		}
	}
	if len(scopeIDs) == 0 {
		// Also accept repeated ?id= / form style.
		if vals := r.URL.Query()["id"]; len(vals) > 0 {
			for _, v := range vals {
				id := strings.TrimSpace(v)
				if id != "" {
					scopeIDs = append(scopeIDs, id)
				}
			}
		}
	}
	if cached && !refresh && len(scopeIDs) == 0 {
		out, err := options.Quota.FetchCached(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	var (
		out map[string]any
		err error
	)
	if len(scopeIDs) > 0 {
		out, err = options.Quota.FetchByIDs(r.Context(), scopeIDs)
	} else {
		out, err = options.Quota.FetchAll(r.Context())
	}
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func serveAdminAccountQuota(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	aid := strings.TrimSpace(r.PathValue("account_id"))
	if aid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "account_id required"})
		return
	}
	// Live fetch by default so "额度" button always works even without cache.
	// ?cached=1 keeps old behavior for bulk UI hydration.
	cachedOnly := truthy(r.URL.Query().Get("cached")) && !truthy(r.URL.Query().Get("refresh"))
	if cachedOnly {
		all, err := options.Store.ListCachedQuotas(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		results, _ := all["results"].([]map[string]any)
		if results == nil {
			if arr, ok := all["results"].([]any); ok {
				for _, item := range arr {
					if m, ok := item.(map[string]any); ok {
						results = append(results, m)
					}
				}
			}
		}
		for _, item := range results {
			if stringValue(item["account_id"]) == aid {
				writeJSON(w, http.StatusOK, item)
				return
			}
		}
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "quota cache not found"})
		return
	}
	if options.Quota == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "quota service unavailable"})
		return
	}
	item, err := options.Quota.FetchOne(r.Context(), aid)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if item == nil {
		item = map[string]any{"ok": false, "account_id": aid, "error": "empty quota result"}
	}
	// Prefer the synthetic pool from FetchOne (live billing result) so the admin
	// UI paints immediately. DB SaveQuotaSnapshot is already async; if a durable
	// pool view is available and not lagging, merge cooldown flags from it.
	if poolView, perr := options.Store.GetAccountPoolView(r.Context(), aid); perr == nil && poolView != nil {
		// Keep synthetic last_quota / quota_disabled from FetchOne; only enrich
		// non-quota fields (cooldown / blocked models) from DB.
		synth, _ := item["pool"].(map[string]any)
		if synth == nil {
			synth = map[string]any{}
			item["pool"] = synth
		}
		for _, k := range []string{"in_cooldown", "cooldown_until", "cooldown_code", "cooldown_model", "cooldown_reason", "blocked_models", "blocked_model_ids", "last_probe", "last_probe_status"} {
			if v, ok := poolView[k]; ok && v != nil {
				synth[k] = v
				item[k] = v
			}
		}
		// Prefer cooldown (new) over legacy permanent quota_disabled from DB.
		if v, ok := poolView["pool_status"].(string); ok && (v == "cooldown" || v == "quota_disabled") {
			if v == "quota_disabled" {
				// Legacy rows: present as cooldown until next healthy snapshot clears them.
				v = "cooldown"
			}
			synth["pool_status"] = v
			synth["disabled_for_quota"] = false
			synth["enabled"] = true
			synth["in_cooldown"] = true
			item["pool_status"] = v
			item["disabled_for_quota"] = false
			item["auto_disabled"] = true
			item["pool_disabled"] = false
			item["in_cooldown"] = true
		}
	}
	// Surface top-level fields used by frontend even when only synth pool exists.
	if pool, _ := item["pool"].(map[string]any); pool != nil {
		if v, ok := pool["pool_status"]; ok {
			item["pool_status"] = v
		}
		if v, ok := pool["disabled_for_quota"]; ok {
			item["disabled_for_quota"] = v
			if b, ok := v.(bool); ok && b {
				item["auto_disabled"] = true
				item["pool_disabled"] = true
			}
		}
		if v, ok := pool["enabled"]; ok {
			item["enabled"] = v
		}
		if v, ok := pool["in_cooldown"]; ok {
			item["in_cooldown"] = v
		}
	}
	writeJSON(w, http.StatusOK, item)
}

func serveIntegrationSettingsGet(w http.ResponseWriter, r *http.Request, options Options, key string) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "config": integrations.PublicConfig(r.Context(), options.Store, key)})
}

func serveIntegrationSettingsPut(w http.ResponseWriter, r *http.Request, options Options, key string) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	doTest := false
	if v, ok := body["test"].(bool); ok {
		doTest = v
		delete(body, "test")
	}
	cfg, err := integrations.SaveConfig(r.Context(), options.Store, key, body)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	out := map[string]any{"ok": true, "config": cfg}
	if doTest {
		raw, _ := options.Store.GetSetting(r.Context(), key)
		rm, _ := raw.(map[string]any)
		switch key {
		case "cliproxyapi_config":
			test := integrations.TestCLIProxy(r.Context(), rm)
			out["test"] = test
			out["ok"] = test["ok"] == true
		case "sub2api_config":
			test := integrations.TestSub2API(r.Context(), rm)
			out["test"] = test
			out["ok"] = test["ok"] == true
			if v, ok := test["groups"]; ok {
				out["groups"] = v
			}
		}
	}
	writeJSON(w, http.StatusOK, out)
}

func serveCLIProxyTest(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	raw, err := options.Store.GetSetting(r.Context(), "cliproxyapi_config")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "config missing"})
		return
	}
	rm, _ := raw.(map[string]any)
	test := integrations.TestCLIProxy(r.Context(), rm)
	writeJSON(w, http.StatusOK, map[string]any{"ok": test["ok"] == true, "test": test})
}

func serveExportCLIProxyFormat(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	ids := stringSlice(body["ids"])
	if len(ids) == 0 {
		ids = stringSlice(body["account_ids"])
	}
	if body["all"] == true {
		ids = nil
	}
	out, err := integrations.ExportCLIProxyBundle(r.Context(), options.Store, ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func servePushCLIProxy(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	ids := stringSlice(body["account_ids"])
	if body["all"] == true || body["account_ids"] == nil {
		ids = nil
	}
	out, err := integrations.PushCLIProxy(r.Context(), options.Store, ids, 4)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func serveExportSub2APIFormat(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	ids := stringSlice(body["ids"])
	if len(ids) == 0 {
		ids = stringSlice(body["account_ids"])
	}
	if body["all"] == true {
		ids = nil
	}
	out, err := integrations.ExportSub2APIFormat(r.Context(), options.Store, ids)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func parseUpstreamModels(payload any) []map[string]any {
	var data []any
	switch p := payload.(type) {
	case map[string]any:
		if raw, ok := p["data"].([]any); ok {
			data = raw
		} else if raw, ok := p["models"].([]any); ok {
			data = raw
		} else if raw, ok := p["result"].([]any); ok {
			data = raw
		} else if nested, ok := p["data"].(map[string]any); ok {
			if raw, ok := nested["data"].([]any); ok {
				data = raw
			} else if raw, ok := nested["models"].([]any); ok {
				data = raw
			}
		}
	case []any:
		data = p
	}
	items := []map[string]any{}
	seen := map[string]bool{}
	for _, raw := range data {
		m, ok := raw.(map[string]any)
		if !ok {
			if s, ok := raw.(string); ok {
				s = strings.TrimSpace(s)
				if s == "" || seen[strings.ToLower(s)] {
					continue
				}
				seen[strings.ToLower(s)] = true
				items = append(items, map[string]any{"id": s, "name": s, "owned_by": "xai"})
			}
			continue
		}
		id := firstNonEmptyStr(stringValue(m["id"]), stringValue(m["model"]), stringValue(m["name"]))
		id = strings.TrimSpace(id)
		if id == "" || seen[strings.ToLower(id)] {
			continue
		}
		seen[strings.ToLower(id)] = true
		item := map[string]any{"id": id, "owned_by": firstNonEmptyStr(stringValue(m["owned_by"]), "xai")}
		if n := firstNonEmptyStr(stringValue(m["name"]), id); n != "" {
			item["name"] = n
		}
		if d := stringValue(m["description"]); d != "" {
			item["description"] = d
		}
		if cw, ok := m["context_window"]; ok {
			item["context_window"] = cw
		} else if cw, ok := m["context_length"]; ok {
			item["context_window"] = cw
		} else if cw, ok := m["max_context_length"]; ok {
			item["context_window"] = cw
		}
		if v, ok := m["supports_reasoning_effort"]; ok {
			item["supports_reasoning_effort"] = v
		}
		// keep useful extras without huge blobs
		for _, key := range []string{"max_completion_tokens", "reasoning_effort", "reasoning_efforts", "supported_in_api"} {
			if v, ok := m[key]; ok && v != nil {
				item[key] = v
			}
		}
		items = append(items, item)
	}
	return items
}

func ensureLocalModelExtras(items []map[string]any, defaultModel string) []map[string]any {
	if defaultModel == "" {
		defaultModel = "grok-4.5"
	}
	have := map[string]bool{}
	for _, it := range items {
		if id := strings.ToLower(strings.TrimSpace(stringValue(it["id"]))); id != "" {
			have[id] = true
		}
	}
	extras := []map[string]any{
		{"id": defaultModel, "name": defaultModel, "owned_by": "xai"},
		{"id": "grok-build", "name": "Grok Build", "description": "Grok coding / build model (cli-chat-proxy)", "owned_by": "xai", "synthetic": true},
		{"id": "grok-search", "name": "Grok Search", "description": "Grok with web search enabled (local alias)", "owned_by": "xai", "synthetic": true},
	}
	for _, ex := range extras {
		id := strings.ToLower(stringValue(ex["id"]))
		if id == "" || have[id] {
			continue
		}
		items = append(items, ex)
		have[id] = true
	}
	return items
}

func usageDetail(route string, requestBody map[string]any, ttftMS, latency int) map[string]any {
	detail := map[string]any{"route": route, "latency_ms": latency}
	if ttftMS > 0 {
		detail["ttft_ms"] = ttftMS
	}
	if effort := extractReasoningEffort(requestBody); effort != "" {
		detail["reasoning_effort"] = effort
		detail["thinking_intensity"] = effort
	}
	return detail
}

// extractReasoningEffort returns the client-facing thinking intensity for usage
// detail: low|medium|high|xhigh|max|ultracode (Claude Code menu + Anthropic API).
// Upstream still receives low|medium|high via reasoning.ToUpstream / ApplyCanonical.
// Codex: Low/Base/High/Ultra/Proactive (+ auto/default/standard/extra-high) · Claude: output_config.effort + budget_tokens.
func extractReasoningEffort(payload map[string]any) string {
	return reasoning.FromRequest(payload)
}

func normalizeReasoningEffort(value any) string {
	// Client-facing label (for logs / admin). Use reasoning.ToUpstream for Grok.
	return reasoning.NormalizeClient(value)
}

func budgetToEffort(n int) string {
	// Client-facing budget mapping (may be xhigh/max).
	return reasoning.BudgetToClient(n)
}

func firstNonEmptyStr(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func minInt(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var (
	exportJobsMu sync.Mutex
	exportJobs   = map[string]map[string]any{}
)

func startExportJob(result map[string]any, filename string) map[string]any {
	jobID := fmt.Sprintf("exp_%d", time.Now().UnixNano())
	auth, _ := result["auth"].(map[string]any)
	count := 0
	if auth != nil {
		count = len(auth)
	} else if v, ok := result["count"].(int); ok {
		count = v
	}
	payload, _ := json.MarshalIndent(result, "", "  ")
	job := map[string]any{
		"ok": true, "job_id": jobID, "status": "done", "phase": "done",
		"message": fmt.Sprintf("导出完成：%d 个账号", count),
		"percent": 100, "done": count, "total": count, "count": count,
		"success": count, "fail": 0, "download_ready": true,
		"filename": filename, "payload": payload, "created_at": time.Now().Unix(),
	}
	exportJobsMu.Lock()
	if len(exportJobs) > 20 {
		exportJobs = map[string]map[string]any{}
	}
	exportJobs[jobID] = job
	exportJobsMu.Unlock()
	return map[string]any{
		"ok": true, "job_id": jobID, "status": "done", "message": job["message"],
		"total": count, "count": count, "filename": filename, "download_ready": true,
	}
}

func serveExportJobStatus(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	id := r.PathValue("job_id")
	exportJobsMu.Lock()
	job := exportJobs[id]
	exportJobsMu.Unlock()
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"ok": false, "error": "job not found"})
		return
	}
	out := map[string]any{}
	for k, v := range job {
		if k == "payload" {
			continue
		}
		out[k] = v
	}
	writeJSON(w, http.StatusOK, out)
}

func serveExportJobDownload(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	id := r.PathValue("job_id")
	exportJobsMu.Lock()
	job := exportJobs[id]
	exportJobsMu.Unlock()
	if job == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "job not found"})
		return
	}
	payload, _ := job["payload"].([]byte)
	if len(payload) == 0 {
		writeJSON(w, http.StatusConflict, map[string]any{"detail": "export not ready"})
		return
	}
	filename, _ := job["filename"].(string)
	if filename == "" {
		filename = "grok2api-auth-export.json"
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filename))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(payload)
}

func servePushSub2API(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	ids := stringSlice(body["account_ids"])
	if len(ids) == 0 {
		ids = stringSlice(body["ids"])
	}
	if body["all"] == true {
		ids = nil
	}
	var groupID *int
	if v, ok := body["group_id"]; ok && v != nil {
		switch t := v.(type) {
		case float64:
			n := int(t)
			groupID = &n
		case json.Number:
			if i, err := t.Int64(); err == nil {
				n := int(i)
				groupID = &n
			}
		case string:
			if i, err := strconv.Atoi(strings.TrimSpace(t)); err == nil {
				groupID = &i
			}
		}
	}
	out, err := integrations.PushSub2API(r.Context(), options.Store, ids, groupID, 4)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error(), "ok": false})
		return
	}
	writeJSON(w, http.StatusOK, out)
}

func serveSub2APITest(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	raw, err := options.Store.GetSetting(r.Context(), "sub2api_config")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "config missing"})
		return
	}
	rm, _ := raw.(map[string]any)
	test := integrations.TestSub2API(r.Context(), rm)
	// Flatten groups / group_count for the settings page (testSub2apiConnection /
	// renderSub2apiGroups expect top-level fields, not only nested under "test").
	out := map[string]any{"ok": test["ok"] == true, "test": test}
	if v, ok := test["groups"]; ok {
		out["groups"] = v
	}
	if v, ok := test["group_count"]; ok {
		out["group_count"] = v
	}
	if v, ok := test["error"]; ok {
		out["error"] = v
	}
	if v, ok := test["message"]; ok {
		out["message"] = v
	}
	writeJSON(w, http.StatusOK, out)
}

func serveSub2APIGroupsList(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, false) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	out := integrations.ListSub2APIGroups(r.Context(), options.Store)
	// Keep HTTP 200 with ok:false for remote/config errors so the admin toast
	// can show the message; only auth/store failures use non-200 above.
	writeJSON(w, http.StatusOK, out)
}

func serveSub2APIGroupsCreate(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	name := stringValue(body["name"])
	platform := stringValue(body["platform"])
	if platform == "" {
		platform = "grok"
	}
	setDefault := true
	if v, ok := body["set_default"].(bool); ok {
		setDefault = v
	}
	out := integrations.CreateSub2APIGroup(r.Context(), options.Store, name, platform, setDefault)
	writeJSON(w, http.StatusOK, out)
}

// reconcileRegistrationSSOIntoStore copies SSO cookies from live registration
// sessions into durable account payloads when the account row is missing top-level
// sso. Ensures "导出全部 SSO" includes newly registered accounts that already
// imported tokens but dropped nested-only cookie shapes.
func reconcileRegistrationSSOIntoStore(ctx context.Context, options Options) int {
	if options.Store == nil {
		return 0
	}
	client := registrationClient(options)
	if client == nil {
		return 0
	}
	payload, err := client.Sessions(ctx)
	if err != nil || payload == nil {
		return 0
	}
	var sessions []any
	switch v := payload["sessions"].(type) {
	case []any:
		sessions = v
	case []map[string]any:
		sessions = make([]any, len(v))
		for i, m := range v {
			sessions[i] = m
		}
	default:
		return 0
	}
	fixed := 0
	for _, raw := range sessions {
		sess, _ := raw.(map[string]any)
		if sess == nil {
			continue
		}
		sso := strings.TrimSpace(stringValue(sess["sso"]))
		if sso == "" {
			if cookies, ok := sess["session_cookies"].(map[string]any); ok {
				sso = strings.TrimSpace(firstNonEmpty(stringValue(cookies["sso"]), stringValue(cookies["sso-rw"])))
			}
		}
		if sso == "" {
			sso = accounts.GetSSOValue(sess)
		}
		if sso == "" {
			continue
		}
		aids := []string{}
		for _, x := range stringSlice(sess["imported_account_ids"]) {
			if strings.TrimSpace(x) != "" {
				aids = append(aids, strings.TrimSpace(x))
			}
		}
		if rows, ok := sess["imported_accounts"].([]any); ok {
			for _, r := range rows {
				m, _ := r.(map[string]any)
				if m == nil {
					continue
				}
				if id := strings.TrimSpace(stringValue(m["id"])); id != "" {
					aids = append(aids, id)
				}
			}
		}
		// Also match by registration_session_id / email if no imported ids yet.
		email := strings.TrimSpace(stringValue(sess["email"]))
		sid := strings.TrimSpace(stringValue(sess["id"]))
		if len(aids) == 0 && (email != "" || sid != "") {
			if authMap, err := options.Store.ExportAuthMap(ctx, nil, true); err == nil {
				if auth, _ := authMap["auth"].(map[string]any); auth != nil {
					for aid, rawE := range auth {
						e, _ := rawE.(map[string]any)
						if e == nil {
							continue
						}
						if sid != "" && strings.TrimSpace(stringValue(e["registration_session_id"])) == sid {
							aids = append(aids, aid)
							continue
						}
						if email != "" && strings.EqualFold(strings.TrimSpace(stringValue(e["email"])), email) {
							aids = append(aids, aid)
						}
					}
				}
			}
		}
		seen := map[string]struct{}{}
		for _, aid := range aids {
			aid = strings.TrimSpace(aid)
			if aid == "" {
				continue
			}
			if _, ok := seen[aid]; ok {
				continue
			}
			seen[aid] = struct{}{}
			// Load current payload; skip if already has canonical SSO.
			authMap, err := options.Store.ExportAuthMap(ctx, []string{aid}, true)
			if err != nil {
				continue
			}
			auth, _ := authMap["auth"].(map[string]any)
			cur, _ := auth[aid].(map[string]any)
			if cur == nil {
				cur = map[string]any{}
			}
			if strings.TrimSpace(stringValue(cur["sso"])) != "" || accounts.GetSSOValue(cur) != "" {
				// Still force top-level if only nested.
				if strings.TrimSpace(stringValue(cur["sso"])) != "" {
					continue
				}
			}
			fix := map[string]any{}
			for k, v := range cur {
				fix[k] = v
			}
			fix["sso"] = sso
			fix["sso_cookie"] = sso
			fix["sso_token"] = sso
			sc, _ := fix["session_cookies"].(map[string]any)
			if sc == nil {
				sc = map[string]any{}
			}
			sc["sso"] = sso
			sc["sso-rw"] = sso
			fix["session_cookies"] = sc
			if email != "" && strings.TrimSpace(stringValue(fix["email"])) == "" {
				fix["email"] = email
			}
			if sid != "" && strings.TrimSpace(stringValue(fix["registration_session_id"])) == "" {
				fix["registration_session_id"] = sid
			}
			if err := options.Store.UpsertAccount(ctx, aid, fix); err == nil {
				fixed++
			}
		}
	}
	return fixed
}

func buildSSOExport(authMap map[string]any, includePassword bool) map[string]any {
	auth, _ := authMap["auth"].(map[string]any)
	lines := []string{}
	items := []map[string]any{}
	hadPassword := false
	for id, raw := range auth {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		sso := accounts.GetSSOValue(entry)
		if sso == "" {
			continue
		}
		email := strings.TrimSpace(stringValue(entry["email"]))
		password := strings.TrimSpace(firstNonEmpty(stringValue(entry["password"]), stringValue(entry["register_password"])))
		line := sso
		if includePassword && email != "" && password != "" {
			line = email + "----" + sso + "----" + password
			hadPassword = true
		} else if email != "" {
			line = email + "----" + sso
		}
		lines = append(lines, line)
		item := map[string]any{"id": id, "email": email, "sso": sso}
		if includePassword && password != "" {
			item["password"] = password
		}
		items = append(items, item)
	}
	content := strings.Join(lines, "\n")
	if content != "" {
		content += "\n"
	}
	return map[string]any{
		"ok":               true,
		"count":            len(items),
		"with_sso":         len(items),
		"lines":            lines,
		"items":            items,
		"text":             content,
		"content":          content,
		"format":           "txt",
		"ext":              "txt",
		"media_type":       "text/plain;charset=utf-8",
		"filename":         "grok2api-accounts-sso.txt",
		"include_password": hadPassword || includePassword,
	}
}

type statusProvider interface{ Status() map[string]any }

func serviceStatus(svc statusProvider, options Options) map[string]any {
	if svc == nil {
		return map[string]any{"enabled": false, "implementation": "go", "started": false}
	}
	switch v := any(svc).(type) {
	case *maintainer.Service:
		if v == nil {
			return map[string]any{"enabled": false, "implementation": "go", "started": false}
		}
	case *modelhealth.Service:
		if v == nil {
			return map[string]any{"enabled": false, "implementation": "go", "started": false}
		}
	}
	return svc.Status()
}

func serveAdminProbeAccount(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	model := stringValue(body["model"])
	if model == "" {
		model = options.runtimeConfig().DefaultModel
	}
	model = modelCatalog(options).Resolve(model)
	accountID := strings.TrimSpace(r.PathValue("account_id"))
	if accountID == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "account_id required"})
		return
	}
	// Manual "模型测试" is full 测活: last_probe + kick/block/clear must land in DB
	// (same semantics as probe-batch / background model health).
	autoDisable := true
	if v, ok := body["auto_disable"].(bool); ok {
		autoDisable = v
	}
	if options.ModelHealth != nil {
		results := options.ModelHealth.ProbeIDs(r.Context(), []string{accountID}, model, autoDisable, "manual")
		if len(results) == 0 {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
			return
		}
		row := results[0]
		// ProbeIDs shape: {ok, account_id, email, result:probe} OR {ok:false, account_id, error}
		result, _ := row["result"].(map[string]any)
		if result == nil {
			result = map[string]any{
				"account_id": firstNonEmptyStr(stringValue(row["account_id"]), accountID),
				"model":      model,
				"available":  false,
				"ok":         false,
				"error":      firstNonEmptyStr(stringValue(row["error"]), "probe produced no result"),
				"source":     "manual",
				"probed_at":  time.Now().Unix(),
			}
		}
		aid := firstNonEmptyStr(stringValue(row["account_id"]), stringValue(result["account_id"]), accountID)
		email := firstNonEmptyStr(stringValue(row["email"]), stringValue(result["email"]))
		// ProbeIDs already deferred last_probe batch save; do not block the admin
		// response on another synchronous PG write. Kick/Clear already applied.
		ok := row["ok"] == true || result["available"] == true
		// Prefer live probe flags for immediate UI; enrich with DB pool if present.
		poolView := map[string]any{
			"id": aid, "account_id": aid, "last_probe": result,
			"last_probe_status": map[bool]string{true: "ok", false: "fail"}[ok],
		}
		if ok {
			poolView["pool_status"] = "normal"
			poolView["in_cooldown"] = false
			if result["recovered"] == true {
				poolView["recovered"] = true
			}
		} else {
			if result["kicked_cooldown"] == true {
				poolView["in_cooldown"] = true
				poolView["pool_status"] = "cooldown"
				poolView["cooldown_code"] = result["cooldown_code"]
			}
			if result["model_blocked"] == true {
				poolView["pool_status"] = "model_blocked"
			}
			if result["auto_disabled"] == true {
				poolView["enabled"] = false
				poolView["pool_status"] = "disabled"
			}
		}
		if dbPool, perr := options.Store.GetAccountPoolView(r.Context(), aid); perr == nil && dbPool != nil {
			// Merge durable fields without overwriting live last_probe snapshot.
			for k, v := range dbPool {
				if k == "last_probe" {
					continue
				}
				if _, has := poolView[k]; !has || poolView[k] == nil {
					poolView[k] = v
				}
			}
		}
		statusCode := 0
		switch v := result["status_code"].(type) {
		case int:
			statusCode = v
		case int64:
			statusCode = int(v)
		case float64:
			statusCode = int(v)
		case json.Number:
			if n, err := v.Int64(); err == nil {
				statusCode = int(n)
			}
		}
		errText := stringValue(result["error"])
		if ok {
			touchRedisPool(options, aid, true, "", nil, statusCode)
		} else {
			touchRedisPool(options, aid, false, errText, nil, statusCode)
		}
		// 测活同时拉账号类型 + 额度使用，写入 last_quota 并回传给管理台。
		// 新导入账号常无额度缓存；这里补齐 Free/SuperGrok 与 token/美元用量。
		quotaSnap := attachQuotaAfterProbe(r.Context(), options, aid, poolView)
		resp := map[string]any{
			"ok": ok, "account_id": aid, "email": email,
			"result": result, "pool": poolView,
		}
		if quotaSnap != nil {
			resp["quota"] = quotaSnap
			if at := strings.TrimSpace(fmt.Sprint(quotaSnap["account_type"])); at != "" && at != "<nil>" {
				resp["account_type"] = at
				resp["plan"] = at
			}
			if pl := strings.TrimSpace(fmt.Sprint(quotaSnap["plan_label"])); pl != "" && pl != "<nil>" {
				resp["plan_label"] = pl
			}
		}
		writeJSON(w, http.StatusOK, resp)
		return
	}
	// Fallback when ModelHealth is not wired (tests / degraded): still persist last_probe.
	auth, err := options.Store.GetAccountAuth(r.Context(), accountID)
	if err != nil || auth == nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found or has no token"})
		return
	}
	client := upstreamClient(options)
	probeBody := map[string]any{
		"model": model, "stream": true, "max_tokens": 1,
		"messages": []any{map[string]any{"role": "user", "content": "ping"}},
	}
	started := time.Now()
	resp, err := client.Open(r.Context(), grok.Account{ID: auth.ID, Token: auth.Token}, model, probeBody)
	result := map[string]any{
		"account_id": auth.ID, "email": auth.Email, "model": model,
		"probed_at": time.Now().Unix(), "source": "manual",
	}
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
		if autoDisable {
			decision := pool.ClassifyUpstreamFailure(status, errText, model)
			if status == 401 || status == 403 {
				_, _ = options.Store.SetAccountEnabled(r.Context(), auth.ID, false)
				result["auto_disabled"] = true
			} else if decision.BlockModel {
				// empty model output etc.: soft-block model only (模型封禁), no account cool.
				until := time.Now().Add(10 * time.Minute)
				if decision.Until != nil {
					until = *decision.Until
				}
				bm := model
				if decision.Model != "" {
					bm = decision.Model
				}
				_ = options.Store.BlockPoolModel(r.Context(), auth.ID, bm, &until)
				result["model_blocked"] = true
				result["blocked_model"] = bm
				if decision.ShouldCooldown {
					sec := until.Sub(time.Now()).Seconds()
					if sec < 60 {
						sec = 60
					}
					_, _ = options.Store.KickFromPool(r.Context(), auth.ID, errText, &sec)
					result["kicked_cooldown"] = true
				}
			} else if decision.ShouldCooldown {
				until := time.Now().Add(10 * time.Minute)
				if decision.Until != nil {
					until = *decision.Until
				}
				sec := until.Sub(time.Now()).Seconds()
				if sec < 60 {
					sec = 60
				}
				_, _ = options.Store.KickFromPool(r.Context(), auth.ID, errText, &sec)
				result["kicked_cooldown"] = true
			}
		}
		result["available"] = false
		result["ok"] = false
		result["error"] = errText
		result["status_code"] = status
		result["latency_ms"] = time.Since(started).Milliseconds()
		_ = options.Store.SaveLastProbe(r.Context(), auth.ID, result)
		poolView, _ := options.Store.GetAccountPoolView(r.Context(), auth.ID)
		touchRedisPool(options, auth.ID, false, errText, nil, status)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "account_id": auth.ID, "email": auth.Email, "result": result, "pool": poolView})
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	_ = options.Store.ReportPoolSuccess(r.Context(), auth.ID, false) /* probe/manual success clears sticky cool */
	if _, err := options.Store.ClearAccountCooldown(r.Context(), auth.ID); err == nil {
		result["recovered"] = true
	}
	if model != "" {
		if err := options.Store.UnblockPoolModel(r.Context(), auth.ID, model); err == nil {
			result["unblocked_model"] = model
		}
	}
	result["available"] = true
	result["ok"] = true
	result["status_code"] = resp.StatusCode
	result["latency_ms"] = time.Since(started).Milliseconds()
	_ = options.Store.SaveLastProbe(r.Context(), auth.ID, result)
	touchRedisPool(options, auth.ID, true, "", nil, resp.StatusCode)
	poolView, _ := options.Store.GetAccountPoolView(r.Context(), auth.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account_id": auth.ID, "email": auth.Email, "result": result, "pool": poolView})
}

func serveAdminSetAccountStatus(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	_ = decoder.Decode(&body)
	if body == nil {
		body = map[string]any{}
	}
	status := strings.TrimSpace(stringValue(body["status"]))
	if status == "" {
		status = strings.TrimSpace(stringValue(body["pool_status"]))
	}
	if status == "" {
		status = strings.TrimSpace(stringValue(body["tag"]))
	}
	if status == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{
			"detail":  "status required",
			"allowed": []string{"live", "cooldown", "model_blocked", "quota_disabled", "disabled", "expired"},
		})
		return
	}
	reason := strings.TrimSpace(stringValue(body["reason"]))
	if reason == "" {
		reason = strings.TrimSpace(stringValue(body["message"]))
	}
	model := strings.TrimSpace(stringValue(body["model"]))
	if model == "" {
		model = strings.TrimSpace(stringValue(body["blocked_model"]))
	}
	var cooldown *float64
	for _, key := range []string{"cooldown_sec", "cooldown_seconds", "sec"} {
		switch v := body[key].(type) {
		case float64:
			cooldown = &v
		case json.Number:
			if f, err := v.Float64(); err == nil {
				cooldown = &f
			}
		}
		if cooldown != nil {
			break
		}
	}
	rec, err := options.Store.SetAccountPoolStatus(r.Context(), r.PathValue("account_id"), status, reason, model, cooldown)
	if err != nil {
		if postgres.IsAccountNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "Account not found"})
			return
		}
		msg := err.Error()
		if strings.HasPrefix(msg, "unsupported status") {
			writeJSON(w, http.StatusBadRequest, map[string]any{"detail": msg})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": msg})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": rec, "pool": rec, "status": rec["pool_status"]})
}

func serveAdminKickAccount(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	reason := stringValue(body["reason"])
	var cooldown *float64
	switch v := body["cooldown_sec"].(type) {
	case float64:
		cooldown = &v
	case json.Number:
		if f, err := v.Float64(); err == nil {
			cooldown = &f
		}
	}
	rec, err := options.Store.KickFromPool(r.Context(), r.PathValue("account_id"), reason, cooldown)
	if err != nil {
		if postgres.IsAccountNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": rec})
}

func serveAdminClearCooldown(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	rec, err := options.Store.ClearAccountCooldown(r.Context(), r.PathValue("account_id"))
	if err != nil {
		if postgres.IsAccountNotFound(err) {
			writeJSON(w, http.StatusNotFound, map[string]any{"detail": "account not found"})
			return
		}
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account": rec})
	if options.Redis != nil {
		_ = options.Redis.MirrorCooldown(r.Context(), r.PathValue("account_id"), time.Time{})
		_, _ = options.Redis.TouchStats(r.Context(), r.PathValue("account_id"), redis.PoolStatsTouch{Success: true, ClearCooldown: true})
	}
}

func adminWriteAllowed(w http.ResponseWriter, r *http.Request, options Options) bool {
	if !options.AdminWriteEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin write routes are not enabled"})
		return false
	}
	if !options.AdminReadEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin read routes are not enabled"})
		return false
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return false
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return false
	}
	return true
}

func serveAdminSetup(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	password := stringValue(body["password"])
	if len(password) < 4 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "password must contain at least 4 characters"})
		return
	}
	has, err := options.Store.HasAdminPassword(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if has {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": "admin password already configured"})
		return
	}
	hash, salt, err := adminauth.NewPassword(password)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	if err := options.Store.SetAdminPassword(r.Context(), hash, salt); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	token, err := createAdminSession(options)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	setAdminCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token, "message": "Admin password created"})
}

func serveAdminLogin(w http.ResponseWriter, r *http.Request, options Options) {
	if !options.AdminReadEnabled && !options.AdminWriteEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin routes are not enabled"})
		return
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	password := stringValue(body["password"])
	ok, err := verifyAdminPassword(r.Context(), options, password)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Invalid admin password"})
		return
	}
	token, err := createAdminSession(options)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	setAdminCookie(w, token)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "token": token})
}

func serveAdminSession(w http.ResponseWriter, r *http.Request, options Options) {
	if !options.AdminReadEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin read routes are not enabled"})
		return
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return
	}
	token, ok := admin.RequireSession(r, options.AdminSessions)
	if !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"ok": false, "authenticated": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "authenticated": true, "token": token})
}

func serveAdminLogout(w http.ResponseWriter, r *http.Request, options Options) {
	if !options.AdminReadEnabled && !options.AdminWriteEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go admin routes are not enabled"})
		return
	}
	token := admin.ExtractSession(r)
	if token != "" {
		deleteAdminSession(options, token)
	}
	clearAdminCookie(w)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func serveAdminCreateKey(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return
	}
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	name := stringValue(body["name"])
	note := stringValue(body["note"])
	result, err := options.Store.CreateAPIKey(r.Context(), name, note)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	// Flat record for existing UI + nested aliases for clients that expect {key, secret}.
	payload := result.Record.PublicMap()
	payload["secret"] = result.Secret
	payload["key"] = result.Secret
	payload["api_key"] = result.Secret
	payload["record"] = result.Record.PublicMap()
	payload["ok"] = true
	writeJSON(w, http.StatusOK, payload)
}

func serveAdminUpdateKey(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	id := r.PathValue("key_id")
	var body map[string]any
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	var name, note *string
	var enabled *bool
	if v, ok := body["name"].(string); ok {
		name = &v
	}
	if v, ok := body["note"].(string); ok {
		note = &v
	}
	if v, ok := body["enabled"].(bool); ok {
		enabled = &v
	}
	rec, err := options.Store.UpdateAPIKey(r.Context(), id, name, note, enabled)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, rec.PublicMap())
}

func serveAdminRegenerateKey(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	result, err := options.Store.RegenerateAPIKey(r.Context(), r.PathValue("key_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	payload := result.Record.PublicMap()
	payload["secret"] = result.Secret
	payload["key"] = result.Secret
	writeJSON(w, http.StatusOK, payload)
}

func serveAdminDeleteKey(w http.ResponseWriter, r *http.Request, options Options) {
	if !adminWriteAllowed(w, r, options) {
		return
	}
	if _, ok := admin.RequireSession(r, options.AdminSessions); !ok {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"detail": "Admin authentication required"})
		return
	}
	ok, err := options.Store.DeleteAPIKey(r.Context(), r.PathValue("key_id"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
		return
	}
	if !ok {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": "api key not found"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func verifyAdminPassword(ctx context.Context, options Options, password string) (bool, error) {
	if options.Store == nil {
		return false, errors.New("store unavailable")
	}
	pw, err := options.Store.LoadAdminPassword(ctx)
	if err == nil && pw.Hash != "" && pw.Salt != "" {
		return adminauth.VerifyPassword(password, pw.Hash, pw.Salt), nil
	}
	// bootstrap via env password only when no store hash exists
	envPW := strings.TrimSpace(options.Config.LegacyAdminPassword)
	if envPW == "" {
		// fallback common env already loaded? use os.Getenv for ADMIN_PASSWORD
		envPW = strings.TrimSpace(os.Getenv("GROK2API_ADMIN_PASSWORD"))
	}
	if envPW != "" && subtle.ConstantTimeCompare([]byte(password), []byte(envPW)) == 1 {
		return true, nil
	}
	return false, nil
}

func createAdminSession(options Options) (string, error) {
	buf := make([]byte, 32)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	token := base64.RawURLEncoding.EncodeToString(buf)
	// Prefer Redis session store (Python path), fall back to Postgres sessions map.
	if rc, ok := options.AdminSessions.(interface{ CreateAdminSession(string) error }); ok {
		if err := rc.CreateAdminSession(token); err == nil {
			return token, nil
		}
	}
	if options.Store != nil {
		if err := options.Store.CreateAdminSession(token); err != nil {
			return "", err
		}
		return token, nil
	}
	return "", errors.New("no admin session store available")
}

func deleteAdminSession(options Options, token string) {
	if rc, ok := options.AdminSessions.(interface{ DeleteAdminSession(string) error }); ok {
		_ = rc.DeleteAdminSession(token)
	}
	if options.Store != nil {
		_ = options.Store.DeleteAdminSession(token)
	}
}

func setAdminCookie(w http.ResponseWriter, token string) {
	http.SetCookie(w, &http.Cookie{
		Name:     admin.AdminCookie,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		MaxAge:   int((7 * 24 * time.Hour).Seconds()),
	})
}

func clearAdminCookie(w http.ResponseWriter) {
	http.SetCookie(w, &http.Cookie{Name: admin.AdminCookie, Value: "", Path: "/", MaxAge: -1, HttpOnly: true})
}

func serveAdminPage(w http.ResponseWriter, r *http.Request, staticDir, page string) {
	name := strings.ToLower(strings.TrimSpace(strings.TrimSuffix(page, ".html")))
	// Reject common broken client links (mobile nav used to emit href="undefined"
	// when PAGE_HREF omitted usage/logs → browser hit /admin/undefined → 404).
	if name == "" || name == "undefined" || name == "null" {
		if name == "" {
			name = "index"
		} else {
			http.NotFound(w, r)
			return
		}
	}
	if name == "overview" {
		// Alias: menu meta key is "overview", file is admin/index.html.
		name = "index"
	}
	if !allowedAdminPage(name) {
		http.NotFound(w, r)
		return
	}
	serveFile(w, r, filepath.Join(staticDir, "admin", name+".html"), true)
}

func allowedAdminPage(name string) bool {
	switch name {
	// Keep in sync with static/js PAGE_HREF / PAGE_META and static/admin/*.html.
	case "index", "login", "keys", "accounts", "models", "guide", "settings", "logs", "usage":
		return true
	default:
		return false
	}
}

func serveStatic(w http.ResponseWriter, r *http.Request, staticDir, name string) {
	cleaned := filepath.Clean("/" + name)
	if cleaned == "/" || strings.Contains(cleaned, "..") {
		http.NotFound(w, r)
		return
	}
	// Content-hashed dist assets (core.f5e0a3148a.js) are immutable forever.
	// HTML admin pages stay no-store via serveAdminPage. Non-hashed sources
	// (static/js/*.js) are not long-cached so deploys without hash don't stick.
	longCache := isHashedDistAsset(cleaned)
	serveFile(w, r, filepath.Join(staticDir, cleaned), false, longCache)
}

// isHashedDistAsset reports whether cleaned path is under /dist/ and the
// basename looks like name.<8+hex>.js|css (content-hash fingerprint).
func isHashedDistAsset(cleaned string) bool {
	// cleaned is like "/dist/core.f5e0a3148a.js"
	if !strings.HasPrefix(cleaned, "/dist/") {
		return false
	}
	base := filepath.Base(cleaned)
	// e.g. core.f5e0a3148a.js, admin-antd.1031c5bb2f.css
	dot := strings.LastIndex(base, ".")
	if dot <= 0 {
		return false
	}
	ext := strings.ToLower(base[dot+1:])
	if ext != "js" && ext != "css" {
		return false
	}
	name := base[:dot]
	hashDot := strings.LastIndex(name, ".")
	if hashDot <= 0 || hashDot == len(name)-1 {
		return false
	}
	hash := name[hashDot+1:]
	if len(hash) < 8 {
		return false
	}
	for _, c := range hash {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')) {
			return false
		}
	}
	return true
}

func serveFile(w http.ResponseWriter, r *http.Request, name string, noStore bool, longCache ...bool) {
	if noStore {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
	} else if len(longCache) > 0 && longCache[0] {
		// Content-hashed dist filenames are safe to cache for 1y.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	}
	info, err := os.Stat(name)
	if err != nil || info.IsDir() {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, name)
}

func isReady(options Options) bool {
	return options.Ready != nil && options.Ready()
}

func readyReason(options Options) string {
	if options.Reason == nil {
		return "not ready"
	}
	return options.Reason()
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	// Marshal first so encode failures never produce HTTP 200 + empty body
	// (which the admin UI then treats as null and crashes on field access).
	payload, err := json.Marshal(value)
	if err != nil {
		slog.Error("writeJSON marshal failed", "error", err)
		payload, _ = json.Marshal(map[string]any{"ok": false, "detail": "encode response failed"})
		status = http.StatusInternalServerError
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	// Admin JSON is live state (logs/accounts/status). Never let a CDN or browser
	// serve a stale task list after a progress upsert ("日志更新不及时").
	w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
	if len(payload) == 0 || payload[len(payload)-1] != '\n' {
		_, _ = w.Write([]byte("\n"))
	}
}

func itoa(value int) string {
	if value == 0 {
		return "0"
	}
	return "1"
}

func itoaPort(value int) string {
	return strconv.Itoa(value)
}
