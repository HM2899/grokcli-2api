package runtime

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/hm2899/grokcli-2api/internal/config"
	"github.com/hm2899/grokcli-2api/internal/migrate"
	"github.com/hm2899/grokcli-2api/internal/store/postgres"
	"github.com/hm2899/grokcli-2api/internal/store/redis"
)

type Readiness struct {
	ready  atomic.Bool
	reason atomic.Value
}

func NewReadiness(reason string) *Readiness {
	r := &Readiness{}
	r.Set(false, reason)
	return r
}

func (r *Readiness) Ready() bool {
	return r != nil && r.ready.Load()
}

func (r *Readiness) Reason() string {
	if r == nil {
		return "readiness not configured"
	}
	value, _ := r.reason.Load().(string)
	return value
}

func (r *Readiness) Set(ready bool, reason string) {
	if r == nil {
		return
	}
	r.ready.Store(ready)
	if reason == "" && ready {
		reason = "ready"
	}
	r.reason.Store(reason)
}

func CheckStartup(ctx context.Context, cfg config.Config, migrations fs.FS) error {
	if cfg.StoreBackend == "file" {
		return errors.New("Go runtime requires PostgreSQL/Redis hybrid stores; run JSON-to-PostgreSQL migration before enabling Go")
	}
	if cfg.RequireSharedStores && (cfg.DatabaseURL == "" || cfg.RedisURL == "") {
		return errors.New("shared PostgreSQL and Redis stores are required")
	}
	if cfg.DatabaseURL == "" {
		return errors.New("PostgreSQL database URL is required")
	}
	if cfg.RedisURL == "" {
		return errors.New("Redis URL is required")
	}
	if err := redis.New(cfg.RedisURL, cfg.RedisPrefix).Ping(ctx); err != nil {
		return fmt.Errorf("connect Redis: %w", err)
	}

	connector, err := postgres.Open(ctx, cfg.DatabaseURL)
	if err != nil {
		return fmt.Errorf("connect PostgreSQL: %w", err)
	}
	defer connector.Close()

	if cfg.RequireMigrations {
		loaded, err := migrate.Load(migrations)
		if err != nil {
			return fmt.Errorf("load migrations: %w", err)
		}
		if err := migrate.VerifyApplied(ctx, connector, loaded); err != nil {
			return fmt.Errorf("verify migrations: %w", err)
		}
	}
	return nil
}

func StartReadinessProbe(parent context.Context, cfg config.Config, migrationsDir string) *Readiness {
	readiness := NewReadiness("startup checks pending")
	go func() {
		ctx, cancel := context.WithTimeout(parent, 15*time.Second)
		defer cancel()
		migrations := os.DirFS(filepath.Clean(migrationsDir))
		if err := CheckStartup(ctx, cfg, migrations); err != nil {
			readiness.Set(false, err.Error())
			slog.Warn("Go runtime readiness is fail-closed", "reason", err)
			return
		}
		readiness.Set(true, "ready")
		slog.Info("Go runtime readiness checks passed")
	}()
	return readiness
}
