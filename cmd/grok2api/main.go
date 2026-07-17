package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/hm2899/grokcli-2api/internal/admin"
	"github.com/hm2899/grokcli-2api/internal/auth"
	"github.com/hm2899/grokcli-2api/internal/buildinfo"
	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/maintainer"
	"github.com/hm2899/grokcli-2api/internal/modelhealth"
	"github.com/hm2899/grokcli-2api/internal/models"
	"github.com/hm2899/grokcli-2api/internal/quota"
	appruntime "github.com/hm2899/grokcli-2api/internal/runtime"
	"github.com/hm2899/grokcli-2api/internal/server"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
	"github.com/hm2899/grokcli-2api/internal/upstream/grok"
	"github.com/hm2899/grokcli-2api/internal/upstream/oidc"
)

func main() {
	cfg, err := config.Load()
	if err != nil {
		slog.Error("invalid configuration", "error", err)
		os.Exit(2)
	}

	stop, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	readiness := appruntime.StartReadinessProbe(stop, cfg, "migrations")
	var store *postgres.Connector
	var adminSessions admin.SessionVerifier
	var redisClient *redis.Client
	var leader *redis.Leader
	var maintSvc *maintainer.Service
	var healthSvc *modelhealth.Service

	if cfg.GoAdminRead || cfg.GoAdminWrite || cfg.GoChat || cfg.GoMessages || cfg.GoResponses || cfg.GoMaintainer {
		redisClient = redis.New(cfg.RedisURL, cfg.RedisPrefix)
	}
	if cfg.GoAdminRead || cfg.GoAdminWrite {
		adminSessions = redisClient
	}
	if redisClient != nil {
		leader = redis.NewLeader(redisClient, cfg.MaintainerLeader, cfg.Workers, cfg.MaintainerLeaderTTL, cfg.MaintainerLeaderRenew)
	}

	if cfg.GoPublicRead || cfg.GoAdminRead || cfg.GoAdminWrite || cfg.GoChat || cfg.GoMessages || cfg.GoResponses || cfg.GoMaintainer {
		ctx, done := context.WithTimeout(context.Background(), 10*time.Second)
		opened, err := postgres.Open(ctx, cfg.DatabaseURL)
		done()
		if err != nil {
			slog.Warn("read-only store unavailable; staged routes remain fail-closed", "error", err)
		} else {
			store = opened
			defer store.Close()
			if adminSessions == nil {
				adminSessions = store
			}
		}
	}

	if store != nil {
		oidcClient := &oidc.Client{}
		maintSvc = maintainer.New(store, redisClient, oidcClient)
		healthSvc = modelhealth.New(store, redisClient, cfg.UpstreamBase, []string{cfg.DefaultModel})
		if leader != nil {
			maintSvc.IsLeader = leader.IsLeader
			healthSvc.IsLeader = leader.IsLeader
		} else {
			maintSvc.IsLeader = func() bool { return true }
			healthSvc.IsLeader = func() bool { return true }
		}
		maintSvc.Enabled = func() bool {
			if !cfg.GoMaintainer {
				return false
			}
			settings, err := store.PublicSettings(context.Background())
			if err != nil {
				return true
			}
			v, _ := settings["token_maintain_enabled"].(bool)
			return v
		}
		healthSvc.Enabled = func() bool {
			if !cfg.GoMaintainer {
				return false
			}
			settings, err := store.PublicSettings(context.Background())
			if err != nil {
				return true
			}
			v, _ := settings["model_health_enabled"].(bool)
			return v
		}
	}

	if cfg.GoMaintainer {
		if leader != nil {
			if leader.ShouldStartMaintainers(context.Background()) {
				slog.Info("go maintainer leadership acquired", "leader_id", leader.Status(context.Background())["leader_id"])
			} else {
				slog.Info("go maintainer waiting for leadership")
			}
		}
		if maintSvc != nil {
			maintSvc.Start()
		}
		if healthSvc != nil {
			healthSvc.Start()
		}
	}

	handler := server.NewMux(server.Options{
		Ready:             readiness.Ready,
		Reason:            readiness.Reason,
		StaticDir:         cfg.StaticDir,
		PublicReadEnabled: cfg.GoPublicRead,
		AdminReadEnabled:  cfg.GoAdminRead,
		AdminWriteEnabled: cfg.GoAdminWrite,
		ChatEnabled:       cfg.GoChat,
		MessagesEnabled:   cfg.GoMessages,
		ResponsesEnabled:  cfg.GoResponses,
		APIKeys:           auth.NewAPIKeyVerifier(cfg, store),
		Models:            models.NewCatalog(cfg, store),
		Store:             store,
		AdminSessions:     adminSessions,
		PickObserver:      redis.NewPickObserver(redisClient),
		AffinityStore:     redis.NewChatAffinity(redisClient, cfg.SSEKeepalive*1800),
		Upstream:          &grok.Client{BaseURL: cfg.UpstreamBase},
		Redis:             redisClient,
		Leader:            leader,
		Maintainer:        maintSvc,
		ModelHealth:       healthSvc,
		Quota:             quota.New(store, cfg.UpstreamBase),
		Config:            cfg,
		RegistrationURL:   cfg.RegistrationServiceURL,
		RegistrationToken: cfg.RegistrationToken,
	})
	httpServer := &http.Server{
		Addr:              cfg.Address(),
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       0, // 不设置 ReadTimeout，避免影响流式响应
		WriteTimeout:      0, // 不设置 WriteTimeout，流式响应需要持续写入
		IdleTimeout:       120 * time.Second, // 增加空闲超时，支持长连接
		MaxHeaderBytes:    1 << 20, // 1MB header limit
	}

	go func() {
		<-stop.Done()
		ctx, done := context.WithTimeout(context.Background(), 30*time.Second)
		defer done()
		if maintSvc != nil {
			maintSvc.Stop()
		}
		if healthSvc != nil {
			healthSvc.Stop()
		}
		if leader != nil {
			leader.Release(ctx)
		}
		if err := httpServer.Shutdown(ctx); err != nil {
			slog.Error("graceful shutdown failed", "error", err)
		}
	}()

	slog.Info("starting migration-safe Go probe server",
		"address", cfg.Address(),
		"implementation", buildinfo.Implementation,
		"version", buildinfo.Version,
	)
	if err := httpServer.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		slog.Error("server stopped", "error", err)
		os.Exit(1)
	}
}
