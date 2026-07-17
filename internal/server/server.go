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
	"net/http"
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
	"github.com/hm2899/grokcli-2api/internal/protocol/responses"
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
	Upstream          *grok.Client
	Redis             *redis.Client
	Leader            *redis.Leader
	Maintainer        *maintainer.Service
	ModelHealth       *modelhealth.Service
	Quota             *quota.Service
	Config            config.Config
	RegistrationURL   string
	RegistrationToken string
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
	mux.HandleFunc("POST /admin/api/accounts/import-sso", func(w http.ResponseWriter, r *http.Request) {
		serveSSOImportStart(w, r, options)
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
	candidates, err := listCandidatesForRequest(r.Context(), options, chatReq, r.Header)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	service := proxy.ChatService{
		Catalog:       modelCatalog(options),
		Client:        upstreamClient(options),
		PickObserver:  options.PickObserver,
		AffinityStore: options.AffinityStore,
	}
	started := time.Now()
	if chatReq.Stream {
		opened, err := service.OpenStreamWithResult(r.Context(), chatReq, candidates, "least_used")
		if err != nil {
			recordChatUsage(r, options, apiKey, "", chatReq.Model, chatReq.Stream, false, http.StatusBadGateway, started, nil, err, 0, chatReq.Raw)
			writeProxyError(w, err)
			return
		}
		defer opened.Body.Close()
		defer releaseServerPick(options, opened.AccountID)
		req := r
		if options.Config.SSEKeepalive > 0 {
			req = r.WithContext(withAnthropicKeepalive(r.Context(), options.Config.SSEKeepalive))
		}
		setProtocolObservationHeaders(w, protocolObservation{
			Protocol: "openai_chat", AccountID: opened.AccountID, PreferAccount: opened.PreferAccount,
			Failover: opened.Failover, Fingerprint: opened.Fingerprint, Accounts: opened.Accounts, Prep: opened.Prep,
		})
		stats, err := streamChatCompletions(w, req, opened.Body, optionsFromRequest(req).Keepalive)
		ok := err == nil || errors.Is(err, r.Context().Err())
		status := http.StatusOK
		if !ok {
			status = http.StatusBadGateway
		}
		recordChatUsage(r, options, apiKey, opened.AccountID, opened.Model, chatReq.Stream, ok, status, started, stats.Usage, err, stats.FirstTokenMS, chatReq.Raw)
		reportChatPool(r, options, opened.AccountID, ok, err, status)
		return
	}
	result, err := service.CompleteWithResult(r.Context(), chatReq, candidates, "least_used")
	if result.AccountID != "" {
		defer releaseServerPick(options, result.AccountID)
	}
	if err != nil {
		recordChatUsage(r, options, apiKey, result.AccountID, chatReq.Model, chatReq.Stream, false, http.StatusBadGateway, started, nil, err, 0, chatReq.Raw)
		writeProxyError(w, err)
		return
	}
	recordChatUsage(r, options, apiKey, result.AccountID, result.Model, chatReq.Stream, true, http.StatusOK, started, result.Usage, nil, 0, chatReq.Raw)
	reportChatPool(r, options, result.AccountID, true, nil, http.StatusOK)
	setProtocolObservationHeaders(w, protocolObservation{
		Protocol: "openai_chat", AccountID: result.AccountID, PreferAccount: result.PreferAccount,
		Failover: result.Failover, Fingerprint: result.Fingerprint, Accounts: result.Accounts, Prep: result.Prep,
	})
	writeJSON(w, http.StatusOK, result.Payload)
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
	w.WriteHeader(http.StatusOK)
	var stats proxy.StreamStats
	started := time.Now()
	write := func(data []byte, force bool) error {
		if !force {
			select {
			case <-r.Context().Done():
				return r.Context().Err()
			default:
			}
		}
		if _, err := w.Write(data); err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	err := grok.ReadSSEWithIdle(body, keepalive, func(event grok.Event) error {
		if event.Done {
			return write([]byte("data: [DONE]\n\n"), true)
		}
		if stats.FirstTokenMS == 0 && len(event.Data) > 0 {
			stats.FirstTokenMS = int(time.Since(started).Milliseconds())
			if stats.FirstTokenMS <= 0 {
				stats.FirstTokenMS = 1
			}
		}
		delta, err := proxy.ParseChatDelta(event.Data)
		if err == nil && delta.Usage != nil {
			stats.Usage = delta.Usage
		}
		data := append([]byte("data: "), event.Data...)
		data = append(data, '\n', '\n')
		return write(data, false)
	}, func() error {
		return write([]byte(": keepalive\n\n"), false)
	})
	if err != nil && !errors.Is(err, r.Context().Err()) {
		encoded, _ := json.Marshal(map[string]any{"error": map[string]any{"message": err.Error(), "type": "api_error"}})
		_ = write(append(append([]byte("data: "), encoded...), '\n', '\n'), true)
		_ = write([]byte("data: [DONE]\n\n"), true)
	}
	if err == nil || errors.Is(err, r.Context().Err()) {
		// Ensure terminal DONE if upstream ended without it (soft disconnect).
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

func recordChatUsage(r *http.Request, options Options, apiKey *auth.APIKeyRecord, accountID, model string, stream bool, ok bool, status int, started time.Time, usage any, cause error, ttftMS int, requestBody map[string]any) {
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usage)
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
	// Fire-and-forget with longer timeout - usage recording should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if options.Store != nil {
			_, _, _ = options.Store.RecordUsage(ctx, postgres.UsageRecord{
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
				Detail:              usageDetail("go_chat", requestBody, ttftMS, latency),
			})
		}
		recordRedisUsage(options, apiKeyID, accountID, model, prompt, completion, total, ok)
	}()
}

func reportChatPool(r *http.Request, options Options, accountID string, ok bool, cause error, status int) {
	if strings.TrimSpace(accountID) == "" {
		return
	}
	// Fire-and-forget pool reporting - should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if ok {
			if options.Store != nil {
				_ = options.Store.ReportPoolSuccess(ctx, accountID, true)
			}
			touchRedisPool(options, accountID, true, "", nil, status)
			return
		}
		var cooldown *time.Time
		if status == http.StatusTooManyRequests || status >= 500 {
			until := time.Now().Add(15 * time.Minute)
			cooldown = &until
		}
		var errText string
		if cause != nil {
			errText = cause.Error()
		}
		if options.Store != nil {
			_ = options.Store.ReportPoolFailure(ctx, postgres.PoolFailure{AccountID: accountID, Error: errText, StatusCode: &status, CooldownUntil: cooldown, CooldownReason: errText, Detail: map[string]any{"source": "go_chat"}})
		}
		touchRedisPool(options, accountID, false, errText, cooldown, status)
	}()
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
	status := http.StatusBadGateway
	message := err.Error()
	if errors.Is(err, pool.ErrNoEligibleAccounts) {
		status = http.StatusServiceUnavailable
		message = "No eligible accounts available. All accounts may be in cooldown or disabled."
	}
	// 检查是否为上游错误并保留正确的状态码
	var upstreamErr *grok.UpstreamError
	if errors.As(err, &upstreamErr) {
		// 对于 429/503 等上游错误，使用原始状态码
		if upstreamErr.Status == http.StatusTooManyRequests || upstreamErr.Status == http.StatusServiceUnavailable {
			status = upstreamErr.Status
			message = upstreamErr.Body
		} else if upstreamErr.Status >= 500 {
			status = http.StatusBadGateway
		} else if upstreamErr.Status >= 400 && upstreamErr.Status < 500 {
			status = upstreamErr.Status
		}
	}
	writeJSON(w, status, map[string]any{"detail": message})
}

func serveMessages(w http.ResponseWriter, r *http.Request, options Options) {
	apiKey, ok := messageRouteAllowed(w, r, options)
	if !ok {
		return
	}
	// Accepted for client compatibility (Claude SDKs send it); not enforced.
	_ = r.Header.Get("anthropic-version")
	var raw map[string]any
	decoder := json.NewDecoder(r.Body)
	decoder.UseNumber()
	if err := decoder.Decode(&raw); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]any{"detail": err.Error()})
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
	allowedTools := allowedAnthropicToolNames(body)
	chatReq := proxy.ChatRequest{Model: model, Stream: false, Raw: body}
	candidates, err := listCandidatesForRequest(r.Context(), options, chatReq, r.Header)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, err.Error(), "api_error")
		return
	}
	service := proxy.ChatService{
		Catalog:       modelCatalog(options),
		Client:        upstreamClient(options),
		PickObserver:  options.PickObserver,
		AffinityStore: options.AffinityStore,
	}
	started := time.Now()
	messageID := newAnthropicMessageID()
	// Match Python resolve_outbound_max_tools for anthropic (Claude/sub2api-safe).
	policy := historycompact.ResolveOutboundToolPolicy(
		"anthropic",
		r.UserAgent(),
		options.Config.OutboundMaxTools,
		options.Config.OutboundMaxToolsOpenAI,
		options.Config.OutboundMaxToolsResponsesNative,
		options.Config.OutboundToolGap,
		options.Config.OutboundToolGapNative,
	)
	maxTools := policy.MaxTools
	if maxTools < 0 {
		maxTools = 0
	}
	if stream {
		chatReq.Stream = true
		opened, err := service.OpenStreamWithResult(r.Context(), chatReq, candidates, "least_used")
		if err != nil {
			recordAnthropicUsage(r, options, apiKey, "", model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
			writeAnthropicError(w, http.StatusBadGateway, err.Error(), "api_error")
			return
		}
		defer opened.Body.Close()
		defer releaseServerPick(options, opened.AccountID)
		req := r
		if options.Config.SSEKeepalive > 0 {
			req = r.WithContext(withAnthropicKeepalive(r.Context(), options.Config.SSEKeepalive))
		}
		if policy.ToolGap > 0 {
			req = req.WithContext(withOutboundToolGap(req.Context(), policy.ToolGap))
		}
		setAnthropicObservationHeaders(w, protocolObservation{Protocol: "anthropic",
			AccountID: opened.AccountID, PreferAccount: opened.PreferAccount, Failover: opened.Failover,
			Fingerprint: opened.Fingerprint, Accounts: opened.Accounts, Prep: opened.Prep, Stream: true,
		})
		usage, firstTokenMS, err := streamAnthropicMessages(w, req, opened.Body, messageID, opened.Model, len(allowedTools) > 0, allowedTools, maxTools)
		ok := err == nil || errors.Is(err, r.Context().Err())
		status := http.StatusOK
		if !ok {
			status = http.StatusBadGateway
		}
		recordAnthropicUsage(r, options, apiKey, opened.AccountID, opened.Model, true, ok, status, started, usage, err, firstTokenMS, raw)
		reportChatPool(r, options, opened.AccountID, ok, err, status)
		return
	}
	result, err := service.CompleteWithResult(r.Context(), chatReq, candidates, "least_used")
	if result.AccountID != "" {
		defer releaseServerPick(options, result.AccountID)
	}
	if err != nil {
		recordAnthropicUsage(r, options, apiKey, result.AccountID, model, false, false, http.StatusBadGateway, started, nil, err, 0, raw)
		writeAnthropicError(w, http.StatusBadGateway, err.Error(), "api_error")
		return
	}
	recordAnthropicUsage(r, options, apiKey, result.AccountID, result.Model, false, true, http.StatusOK, started, result.Usage, nil, 0, raw)
	reportChatPool(r, options, result.AccountID, true, nil, http.StatusOK)
	content, reasoning, finish, usage, toolCalls := anthropicCompletionParts(result.Payload)
	if maxTools > 0 && len(toolCalls) > maxTools {
		toolCalls = toolCalls[:maxTools]
	}
	setAnthropicObservationHeaders(w, protocolObservation{Protocol: "anthropic",
		AccountID: result.AccountID, PreferAccount: result.PreferAccount, Failover: result.Failover,
		Fingerprint: result.Fingerprint, Accounts: result.Accounts, Prep: result.Prep, Stream: false,
	})
	writeJSON(w, http.StatusOK, anthropic.Completion(messageID, result.Model, content, reasoning, finish, toolCalls, usage, allowedTools))
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
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go messages route is not enabled"})
		return nil, false
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return nil, false
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
			return nil, false
		}
		apiKey = verified
	}
	if options.Store == nil && len(options.Candidates) == 0 {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "PostgreSQL store unavailable"})
		return nil, false
	}
	return apiKey, true
}

func messageCountRouteAllowed(w http.ResponseWriter, r *http.Request, options Options) bool {
	if !options.MessagesEnabled {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "Go messages route is not enabled"})
		return false
	}
	if !isReady(options) {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": readyReason(options)})
		return false
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
			return false
		}
	}
	return true
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
	body := responses.BuildChatBody(raw, model)
	messages, _ := body["messages"].([]map[string]any)
	if len(messages) == 0 {
		writeOpenAIError(w, http.StatusBadRequest, "input must contain at least one message", "invalid_request_error")
		return
	}
	chatReq := proxy.ChatRequest{Model: model, Stream: stream, Raw: body}
	// Preserve client prompt_cache_key on chatReq for sticky fingerprint (stripped later for upstream).
	if pck := strings.TrimSpace(stringValue(raw["prompt_cache_key"])); pck != "" {
		if chatReq.Raw == nil {
			chatReq.Raw = map[string]any{}
		}
		chatReq.Raw["prompt_cache_key"] = pck
	}
	candidates, err := listCandidatesForRequest(r.Context(), options, chatReq, r.Header)
	if err != nil {
		writeOpenAIError(w, http.StatusInternalServerError, err.Error(), "server_error")
		return
	}
	service := proxy.ChatService{Catalog: modelCatalog(options), Client: upstreamClient(options), PickObserver: options.PickObserver, AffinityStore: options.AffinityStore}
	started := time.Now()
	responseID := responses.NewResponseID()
	respPolicy := historycompact.ResolveOutboundToolPolicy(
		"openai_responses",
		r.UserAgent(),
		options.Config.OutboundMaxTools,
		options.Config.OutboundMaxToolsOpenAI,
		options.Config.OutboundMaxToolsResponsesNative,
		options.Config.OutboundToolGap,
		options.Config.OutboundToolGapNative,
	)
	if stream {
		opened, err := service.OpenStreamWithResult(r.Context(), chatReq, candidates, "least_used")
		if err != nil {
			recordResponsesUsage(r, options, apiKey, "", model, true, false, http.StatusBadGateway, started, nil, err, 0, raw)
			writeOpenAIError(w, http.StatusBadGateway, err.Error(), "server_error")
			return
		}
		defer opened.Body.Close()
		defer releaseServerPick(options, opened.AccountID)
		req := r
		if options.Config.SSEKeepalive > 0 {
			req = r.WithContext(withAnthropicKeepalive(r.Context(), options.Config.SSEKeepalive))
		}
		if respPolicy.ToolGap > 0 {
			req = req.WithContext(withOutboundToolGap(req.Context(), respPolicy.ToolGap))
		}
		setProtocolObservationHeaders(w, protocolObservation{
			Protocol: "openai_responses", AccountID: opened.AccountID, PreferAccount: opened.PreferAccount,
			Failover: opened.Failover, Fingerprint: opened.Fingerprint, Accounts: opened.Accounts, Prep: opened.Prep,
		})
		usage, firstTokenMS, err := streamOpenAIResponses(w, req, opened.Body, responseID, opened.Model, allowedResponsesToolNames(body), optionsFromRequest(req).Keepalive, respPolicy.MaxTools)
		ok := err == nil || errors.Is(err, r.Context().Err())
		status := http.StatusOK
		if !ok {
			status = http.StatusBadGateway
		}
		recordResponsesUsage(r, options, apiKey, opened.AccountID, opened.Model, true, ok, status, started, usage, err, firstTokenMS, raw)
		reportChatPool(r, options, opened.AccountID, ok, err, status)
		return
	}
	chatReq.Stream = false
	result, err := service.CompleteWithResult(r.Context(), chatReq, candidates, "least_used")
	if result.AccountID != "" {
		defer releaseServerPick(options, result.AccountID)
	}
	if err != nil {
		recordResponsesUsage(r, options, apiKey, result.AccountID, model, false, false, http.StatusBadGateway, started, nil, err, 0, raw)
		writeOpenAIError(w, http.StatusBadGateway, err.Error(), "server_error")
		return
	}
	recordResponsesUsage(r, options, apiKey, result.AccountID, result.Model, false, true, http.StatusOK, started, result.Usage, nil, 0, raw)
	reportChatPool(r, options, result.AccountID, true, nil, http.StatusOK)
	content, reasoning, _, _, toolCalls := anthropicCompletionParts(result.Payload)
	setProtocolObservationHeaders(w, protocolObservation{
		Protocol: "openai_responses", AccountID: result.AccountID, PreferAccount: result.PreferAccount,
		Failover: result.Failover, Fingerprint: result.Fingerprint, Accounts: result.Accounts, Prep: result.Prep,
	})
	writeJSON(w, http.StatusOK, responses.BuildObject(responseID, result.Model, content, reasoning, responseToolCalls(toolCalls), usageMap(result.Usage), time.Now().Unix(), stringValue(raw["previous_response_id"]), metadataMap(raw["metadata"])))
}

func streamOpenAIResponses(w http.ResponseWriter, r *http.Request, body io.Reader, responseID, model string, allowed []string, keepalive time.Duration, maxTools int) (map[string]any, int, error) {
	flusher, ok := w.(http.Flusher)
	if !ok {
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
	writeFrame := func(frame string, force bool) error {
		if !force {
			select {
			case <-r.Context().Done():
				return r.Context().Err()
			default:
			}
		}
		_, err := w.Write([]byte(frame))
		if err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	toolGap := outboundToolGapFrom(r.Context())
	toolsEmitted := 0
	emitFrames := func(frames []string, force bool) error {
		for _, frame := range frames {
			if toolGap > 0 && toolsEmitted > 0 && strings.Contains(frame, "function_call") && strings.Contains(frame, "response.output_item.added") {
				timer := time.NewTimer(toolGap)
				select {
				case <-r.Context().Done():
					timer.Stop()
					return r.Context().Err()
				case <-timer.C:
				}
			}
			if err := writeFrame(frame, force); err != nil {
				return err
			}
			if strings.Contains(frame, "function_call") && strings.Contains(frame, "response.output_item.added") {
				toolsEmitted++
			}
		}
		return nil
	}
	var usage map[string]any
	firstTokenMS := 0
	started := time.Now()
	err := grok.ReadSSEWithIdle(body, keepalive, func(event grok.Event) error {
		if event.Done {
			return nil
		}
		if firstTokenMS == 0 && len(event.Data) > 0 {
			firstTokenMS = int(time.Since(started).Milliseconds())
			if firstTokenMS <= 0 {
				firstTokenMS = 1
			}
		}
		delta, err := proxy.ParseChatDelta(event.Data)
		if err != nil {
			return nil
		}
		if raw, ok := delta.Usage.(map[string]any); ok {
			usage = raw
		}
		if err := emitFrames(streamer.Reasoning(delta.Reasoning), true); err != nil {
			return err
		}
		if err := emitFrames(streamer.Text(delta.Content), true); err != nil {
			return err
		}
		return emitFrames(streamer.ToolDeltas(responsesToolDeltas(delta)), true)
	}, func() error {
		return writeFrame(": keepalive\n\n", false)
	})
	if err != nil && !errors.Is(err, r.Context().Err()) {
		_ = emitFrames(streamer.Fail(err.Error(), "server_error"), true)
		return usage, firstTokenMS, err
	}
	respUsage := responsesUsageFromOpenAI(usage)
	if err := emitFrames(streamer.Complete(&respUsage), true); err != nil {
		return usage, firstTokenMS, err
	}
	return usage, firstTokenMS, err
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

func writeOpenAIError(w http.ResponseWriter, status int, message, errorType string) {
	if errorType == "" {
		errorType = "server_error"
	}
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": message, "type": errorType}})
}

func recordResponsesUsage(r *http.Request, options Options, apiKey *auth.APIKeyRecord, accountID, model string, stream bool, ok bool, status int, started time.Time, usage any, cause error, ttftMS int, requestBody map[string]any) {
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usage)
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
	// Fire-and-forget with longer timeout - usage recording should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if options.Store != nil {
			_, _, _ = options.Store.RecordUsage(ctx, postgres.UsageRecord{
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
			})
		}
		recordRedisUsage(options, apiKeyID, accountID, model, prompt, completion, total, ok)
	}()
}

func responseToolCalls(calls []anthropic.ToolCall) []map[string]any {
	out := make([]map[string]any, 0, len(calls))
	for _, call := range calls {
		out = append(out, map[string]any{"id": call.ID, "type": "function", "function": map[string]any{"name": call.Name, "arguments": call.Arguments}})
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
	w.WriteHeader(http.StatusOK)

	started := time.Now()
	firstTokenMS := 0

	assembler := anthropic.NewStreamAssembler(messageID, model, toolsRequested, maxTools, allowed)
	probe := newDisconnectProbe(5, 2500*time.Millisecond)
	envelopeOpen := false
	var writeMu sync.Mutex

	writeFrame := func(frame string, force bool) error {
		writeMu.Lock()
		defer writeMu.Unlock()
		if !force {
			select {
			case <-r.Context().Done():
				// Soft disconnect: only hard-stop before envelope open.
				if !envelopeOpen {
					return r.Context().Err()
				}
			default:
			}
		}
		_, err := w.Write([]byte(frame))
		if err != nil {
			return err
		}
		flusher.Flush()
		return nil
	}
	toolGap := outboundToolGapFrom(r.Context())
	toolsEmitted := 0
	emitFrames := func(frames []string, force bool) error {
		for _, frame := range frames {
			if toolGap > 0 && toolsEmitted > 0 && strings.Contains(frame, "\"tool_use\"") && strings.Contains(frame, "content_block_start") {
				timer := time.NewTimer(toolGap)
				select {
				case <-r.Context().Done():
					timer.Stop()
					if !envelopeOpen {
						return r.Context().Err()
					}
				case <-timer.C:
				}
			}
			if err := writeFrame(frame, force); err != nil {
				return err
			}
			envelopeOpen = true
			if strings.Contains(frame, "\"tool_use\"") && strings.Contains(frame, "content_block_start") {
				toolsEmitted++
			}
		}
		return nil
	}

	var finish string
	var usage anthropic.Usage
	var openAIUsage map[string]any
	sawModel := false

	onIdle := func() error {
		if probe.check(r.Context()) && !envelopeOpen {
			return r.Context().Err()
		}
		// Anthropic named ping + SSE comment for picky reverse proxies.
		if err := writeFrame(anthropic.Ping(), false); err != nil {
			return err
		}
		return writeFrame(anthropic.CommentKeepalive(), false)
	}

	err := grok.ReadSSEWithIdle(body, opts.Keepalive, func(event grok.Event) error {
		if firstTokenMS == 0 && len(event.Data) > 0 {
			firstTokenMS = int(time.Since(started).Milliseconds())
			if firstTokenMS <= 0 {
				firstTokenMS = 1
			}
		}
		if probe.check(r.Context()) && !envelopeOpen {
			return r.Context().Err()
		}
		if event.Done {
			return nil
		}
		delta, err := proxy.ParseChatDelta(event.Data)
		if err != nil {
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
		if strings.TrimSpace(delta.Content) != "" || strings.TrimSpace(delta.Reasoning) != "" || len(delta.AnthropicToolDeltas()) > 0 {
			sawModel = true
		}
		return emitFrames(assembler.Feed(delta.Content, delta.Reasoning, delta.AnthropicToolDeltas()), true)
	}, onIdle)

	clientGone := probe.gone || errors.Is(err, r.Context().Err()) || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
	if err != nil && !clientGone {
		_ = emitFrames(anthropic.TerminalError(err.Error(), "api_error"), true)
		return openAIUsage, firstTokenMS, err
	}
	if !sawModel && !envelopeOpen {
		// Empty stream with no client bytes yet: surface as error without a half-open envelope.
		empty := errors.New("Upstream returned HTTP 200 with empty model output (no content/tool_calls)")
		_ = emitFrames(anthropic.TerminalError(empty.Error(), "api_error"), true)
		return openAIUsage, firstTokenMS, empty
	}
	if finish == "" {
		finish = "stop"
	}
	// Soft disconnect after envelope open still needs terminal frames so Claude Code
	// can leave "running" and update task status.
	if termErr := emitFrames(assembler.Finish(finish, usage), true); termErr != nil && !clientGone {
		return openAIUsage, firstTokenMS, termErr
	}
	if clientGone {
		return openAIUsage, firstTokenMS, nil
	}
	return openAIUsage, firstTokenMS, err
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

func recordAnthropicUsage(r *http.Request, options Options, apiKey *auth.APIKeyRecord, accountID, model string, stream bool, ok bool, status int, started time.Time, usage any, cause error, ttftMS int, requestBody map[string]any) {
	prompt, completion, total, cacheRead, cacheCreate, reasoning := postgres.UsageFromOpenAI(usage)
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
	// Fire-and-forget with longer timeout - usage recording should not block response
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if options.Store != nil {
			_, _, _ = options.Store.RecordUsage(ctx, postgres.UsageRecord{
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
				Detail:              usageDetail("go_messages", requestBody, ttftMS, latency),
			})
		}
		recordRedisUsage(options, apiKeyID, accountID, model, prompt, completion, total, ok)
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
	}
	if fn, ok := message["function_call"].(map[string]any); ok {
		calls = append(calls, anthropic.ToolCall{Name: stringValue(fn["name"]), Arguments: stringValue(fn["arguments"])})
	}
	return content, reasoning, finish, usage, calls
}

func allowedAnthropicToolNames(body map[string]any) []string {
	items, _ := body["tools"].([]any)
	out := make([]string, 0, len(items))
	for _, item := range items {
		tool, _ := item.(map[string]any)
		fn, _ := tool["function"].(map[string]any)
		if name := stringValue(fn["name"]); name != "" {
			out = append(out, name)
		}
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

func writeAnthropicError(w http.ResponseWriter, status int, message, errorType string) {
	if errorType == "" {
		switch status {
		case http.StatusUnauthorized:
			errorType = "authentication_error"
		case http.StatusForbidden:
			errorType = "permission_error"
		case http.StatusNotFound:
			errorType = "not_found_error"
		case http.StatusTooManyRequests:
			errorType = "rate_limit_error"
		case http.StatusBadRequest:
			errorType = "invalid_request_error"
		default:
			errorType = "api_error"
		}
	}
	writeJSON(w, status, map[string]any{"type": "error", "error": map[string]any{"type": errorType, "message": message}})
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
		"default_model":        options.Config.DefaultModel,
		"require_api_key_mode": options.Config.RequireAPIKey,
		"api_base":             publicAPIBase(r, options.Config.Port),
		"credentials_ok":       pool.Live > 0,
		"credentials_email":    nil,
		"account_mode":         pool.Mode,
		"accounts":             accounts,
		"pool": map[string]any{
			"mode": pool.Mode, "total": pool.Total, "live": pool.Live, "rotatable": pool.Rotatable,
			"enabled": pool.Enabled, "in_cooldown": pool.InCooldown, "quota_disabled": pool.QuotaDisabled,
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
	}
	if protected {
		payload["credentials"] = map[string]any{"email": nil, "active_count": pool.Live, "account_count": accountCount, "ok": pool.Live > 0}
		payload["models"] = modelCatalog(options).PublicModels(r.Context())
		payload["account_modes"] = []string{"round_robin", "random", "least_used"}
		payload["full"] = false
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
		"default_model": options.Config.DefaultModel,
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
	stats, err := options.Store.KeyStats(r.Context(), strings.TrimSpace(options.Config.LegacyAPIKey) != "", required)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
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
	writeJSON(w, http.StatusOK, result)
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
	return &regclient.Client{BaseURL: base, Token: token}
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

func recordRedisUsage(options Options, apiKeyID, accountID, model string, prompt, completion, total int64, ok bool) {
	if options.Redis == nil || !options.Redis.Enabled() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_ = options.Redis.RecordUsage(ctx, redis.UsageDeltas{
		PromptTokens:     prompt,
		CompletionTokens: completion,
		TotalTokens:      total,
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

func usageLightSnapshot(ctx context.Context, options Options) map[string]any {
	if options.Redis != nil && options.Redis.Enabled() {
		return options.Redis.LightSnapshot(ctx)
	}
	return map[string]any{"today_requests": 0, "today_tokens": 0, "total_tokens": 0, "source": "none"}
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
		"q": query.Get("q"), "api_key_id": query.Get("api_key_id"), "account_id": query.Get("account_id"), "model": query.Get("model"), "protocol": query.Get("protocol"), "client_ip": query.Get("client_ip"),
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
	return ensureStickyCandidate(ctx, options, candidates, sticky), nil
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
	if err != nil {
		return ""
	}
	return strings.TrimSpace(id)
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
	// selected ids currently use same batch path (best-effort full cycle)
	result := options.Maintainer.RunOnce(r.Context(), force)
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
	authMap, err := options.Store.ExportAuthMap(r.Context(), nil, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, buildSSOExport(authMap))
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
	authMap, err := options.Store.ExportAuthMap(r.Context(), ids, true)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, buildSSOExport(authMap))
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

func serveAdminModelsSync(w http.ResponseWriter, r *http.Request, options Options) {
	if !requireAdminReadWrite(w, r, options, true) {
		return
	}
	if options.Store == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]any{"detail": "store unavailable"})
		return
	}
	authList, err := options.Store.ListAccountAuths(r.Context(), 20, true)
	if err != nil || len(authList) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]any{"ok": false, "error": "no live account for models sync"})
		return
	}
	a := authList[0]
	client := &http.Client{Timeout: 30 * time.Second}
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, strings.TrimRight(options.Config.UpstreamBase, "/")+"/models", nil)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	gc := upstreamClient(options)
	for k, v := range gc.Headers(a.Token, options.Config.DefaultModel) {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if resp.StatusCode >= 400 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": fmt.Sprintf("upstream %d: %s", resp.StatusCode, string(body)[:minInt(300, len(body))])})
		return
	}
	var payload any
	if err := json.Unmarshal(body, &payload); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "parse: " + err.Error()})
		return
	}
	items := parseUpstreamModels(payload)
	// Always keep local extras (coding/build/search aliases) even when upstream list is tiny.
	items = ensureLocalModelExtras(items, options.Config.DefaultModel)
	if len(items) == 0 {
		writeJSON(w, http.StatusBadGateway, map[string]any{"ok": false, "error": "no models in upstream response"})
		return
	}
	n, err := options.Store.ReplaceModels(r.Context(), items, map[string]any{"source": "upstream", "fetched_via": a.Email, "origin": strings.TrimRight(options.Config.UpstreamBase, "/") + "/models"})
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"ok": false, "error": err.Error()})
		return
	}
	catalog := modelCatalog(options).PublicModels(r.Context())
	writeJSON(w, http.StatusOK, map[string]any{
		"ok": true, "count": len(catalog), "pg_count": n, "upstream_count": n,
		"fetched_via": a.Email, "storage": "postgres", "models": catalog, "data": catalog,
	})
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
	if cached && !refresh {
		out, err := options.Quota.FetchCached(r.Context())
		if err != nil {
			writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
			return
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	out, err := options.Quota.FetchAll(r.Context())
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
	// return cached for single account if present
	all, err := options.Store.ListCachedQuotas(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	aid := r.PathValue("account_id")
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
	if doTest && key == "cliproxyapi_config" {
		// use raw secret
		raw, _ := options.Store.GetSetting(r.Context(), key)
		rm, _ := raw.(map[string]any)
		test := integrations.TestCLIProxy(r.Context(), rm)
		out["test"] = test
		out["ok"] = test["ok"] == true
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

// extractReasoningEffort normalizes client thinking intensity to low/medium/high/xhigh.
func extractReasoningEffort(payload map[string]any) string {
	if payload == nil {
		return ""
	}
	for _, key := range []string{"reasoning_effort", "thinking_effort", "effort", "thinking_intensity"} {
		if v, ok := payload[key]; ok && v != nil {
			if got := normalizeReasoningEffort(v); got != "" {
				return got
			}
		}
	}
	if v, ok := payload["reasoning"]; ok && v != nil {
		if got := normalizeReasoningEffort(v); got != "" {
			return got
		}
	}
	if v, ok := payload["thinking"]; ok && v != nil {
		if got := normalizeReasoningEffort(v); got != "" {
			return got
		}
	}
	// Responses API sometimes nests under text/reasoning configs.
	if raw, ok := payload["text"].(map[string]any); ok {
		if got := extractReasoningEffort(raw); got != "" {
			return got
		}
	}
	return ""
}

func normalizeReasoningEffort(value any) string {
	switch v := value.(type) {
	case nil:
		return ""
	case bool:
		if v {
			return "medium"
		}
		return ""
	case float64:
		return budgetToEffort(int(v))
	case float32:
		return budgetToEffort(int(v))
	case int:
		return budgetToEffort(v)
	case int64:
		return budgetToEffort(int(v))
	case json.Number:
		n, _ := v.Int64()
		return budgetToEffort(int(n))
	case string:
		s := strings.ToLower(strings.TrimSpace(v))
		if s == "" || s == "none" || s == "null" || s == "false" || s == "off" || s == "disabled" {
			return ""
		}
		aliases := map[string]string{
			"minimal": "low", "min": "low", "l": "low",
			"m": "medium", "med": "medium", "enabled": "medium", "adaptive": "medium", "true": "medium", "on": "medium",
			"h":   "high",
			"max": "xhigh", "maximum": "xhigh", "ultra": "xhigh", "extra_high": "xhigh", "extrahigh": "xhigh",
		}
		if a, ok := aliases[s]; ok {
			return a
		}
		if s == "low" || s == "medium" || s == "high" || s == "xhigh" {
			return s
		}
		if len(s) > 32 {
			s = s[:32]
		}
		return s
	case map[string]any:
		for _, key := range []string{"effort", "reasoning_effort", "thinking_effort", "intensity", "level"} {
			if vv, ok := v[key]; ok && vv != nil {
				if got := normalizeReasoningEffort(vv); got != "" {
					return got
				}
			}
		}
		if vv, ok := v["budget_tokens"]; ok && vv != nil {
			return normalizeReasoningEffort(vv)
		}
		tt := strings.ToLower(strings.TrimSpace(fmt.Sprint(v["type"])))
		if v["enabled"] == true || tt == "enabled" || tt == "adaptive" {
			return "medium"
		}
		return ""
	default:
		return normalizeReasoningEffort(fmt.Sprint(v))
	}
}

func budgetToEffort(n int) string {
	if n <= 0 {
		return ""
	}
	if n <= 4096 {
		return "low"
	}
	if n <= 16000 {
		return "medium"
	}
	if n <= 48000 {
		return "high"
	}
	return "xhigh"
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
	writeJSON(w, http.StatusOK, map[string]any{"ok": test["ok"] == true, "test": test})
}

func buildSSOExport(authMap map[string]any) map[string]any {
	auth, _ := authMap["auth"].(map[string]any)
	lines := []string{}
	items := []map[string]any{}
	includePassword := false
	for id, raw := range auth {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		sso := accounts.GetSSOValue(entry)
		if sso == "" {
			continue
		}
		email := ""
		if v, ok := entry["email"].(string); ok {
			email = strings.TrimSpace(v)
		}
		password := ""
		if v, ok := entry["password"].(string); ok {
			password = strings.TrimSpace(v)
		}
		if password == "" {
			if v, ok := entry["register_password"].(string); ok {
				password = strings.TrimSpace(v)
			}
		}
		line := sso
		if email != "" && password != "" {
			line = email + "----" + sso + "----" + password
			includePassword = true
		} else if email != "" {
			line = email + "----" + sso
		}
		lines = append(lines, line)
		items = append(items, map[string]any{"id": id, "email": email, "sso": sso})
	}
	content := strings.Join(lines, "\n")
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
		"include_password": includePassword,
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
		model = options.Config.DefaultModel
	}
	model = modelCatalog(options).Resolve(model)
	auth, err := options.Store.GetAccountAuth(r.Context(), r.PathValue("account_id"))
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]any{"detail": err.Error()})
		return
	}
	client := upstreamClient(options)
	// Lightweight connectivity probe: open a short streamed completion and abort after headers/body start.
	probeBody := map[string]any{
		"model":      model,
		"stream":     true,
		"max_tokens": 1,
		"messages":   []any{map[string]any{"role": "user", "content": "ping"}},
	}
	started := time.Now()
	resp, err := client.Open(r.Context(), grok.Account{ID: auth.ID, Token: auth.Token}, model, probeBody)
	result := map[string]any{
		"account_id": auth.ID,
		"email":      auth.Email,
		"model":      model,
		"probed_at":  time.Now().Unix(),
		"source":     "manual",
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
		autoDisable, _ := body["auto_disable"].(bool)
		if autoDisable && (status == 401 || status == 403) {
			_, _ = options.Store.SetAccountEnabled(r.Context(), auth.ID, false)
		}
		result["available"] = false
		result["error"] = errText
		result["status_code"] = status
		result["latency_ms"] = time.Since(started).Milliseconds()
		poolView, _ := options.Store.GetAccountPoolView(r.Context(), auth.ID)
		touchRedisPool(options, auth.ID, false, errText, nil, status)
		writeJSON(w, http.StatusOK, map[string]any{"ok": false, "account_id": auth.ID, "email": auth.Email, "result": result, "pool": poolView})
		return
	}
	// Drain a tiny amount then close.
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1024))
	_ = resp.Body.Close()
	_ = options.Store.ReportPoolSuccess(r.Context(), auth.ID, true)
	touchRedisPool(options, auth.ID, true, "", nil, resp.StatusCode)
	result["available"] = true
	result["status_code"] = resp.StatusCode
	result["latency_ms"] = time.Since(started).Milliseconds()
	poolView, _ := options.Store.GetAccountPoolView(r.Context(), auth.ID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "account_id": auth.ID, "email": auth.Email, "result": result, "pool": poolView})
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
	var body map[string]any
	_ = json.NewDecoder(r.Body).Decode(&body)
	name := stringValue(body["name"])
	note := stringValue(body["note"])
	result, err := options.Store.CreateAPIKey(r.Context(), name, note)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]any{"detail": err.Error()})
		return
	}
	payload := result.Record.PublicMap()
	payload["secret"] = result.Secret
	payload["key"] = result.Secret
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
	name := strings.TrimSpace(strings.TrimSuffix(page, ".html"))
	if name == "" {
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
	case "index", "overview", "login", "keys", "accounts", "models", "guide", "settings", "logs", "usage":
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
	serveFile(w, r, filepath.Join(staticDir, cleaned), false)
}

func serveFile(w http.ResponseWriter, r *http.Request, name string, noStore bool) {
	if noStore {
		w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
		w.Header().Set("Pragma", "no-cache")
		w.Header().Set("Expires", "0")
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
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
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
