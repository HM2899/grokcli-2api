package config

import (
	"testing"
	"time"
)

func TestLoadDefaults(t *testing.T) {
	for _, name := range []string{
		"GROK2API_HOST",
		"GROK2API_PORT",
		"GROK2API_DEFAULT_MODEL",
		"GROK_CLI_CHAT_PROXY_BASE_URL",
		"GROK2API_REDIS_URL",
		"REDIS_URL",
		"GROK2API_DATABASE_URL",
		"DATABASE_URL",
		"GROK2API_REDIS_PREFIX",
		"GROK2API_STORE_BACKEND",
		"GROK2API_API_KEY",
		"GROK2API_REQUIRE_API_KEY",
		"GROK2API_REQUIRE_SHARED_STORES",
		"GROK2API_REQUIRE_MIGRATIONS",
		"GROK2API_RUNTIME",
		"GROK2API_GO_PUBLIC_READ",
		"GROK2API_GO_CHAT",
		"GROK2API_GO_MESSAGES",
		"GROK2API_GO_RESPONSES",
		"GROK2API_GO_ADMIN_READ",
		"GROK2API_GO_ADMIN_WRITE",
		"GROK2API_GO_MAINTAINER",
		"GROK2API_GO_WRITES",
		"GROK2API_GO_OWNERSHIP_MODE",
		"GROK2API_REGISTRATION_MODE",
		"GROK2API_REGISTRATION_SERVICE_URL",
		"GROK2API_STATIC_DIR",
		"GROK2API_SSE_KEEPALIVE",
		"GROK2API_TIMEOUT",
	} {
		t.Setenv(name, "")
	}

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Address() != "0.0.0.0:3000" {
		t.Fatalf("unexpected address %q", cfg.Address())
	}
	if cfg.DefaultModel != "grok-4.5" {
		t.Fatalf("unexpected model %q", cfg.DefaultModel)
	}
	if cfg.SSEKeepalive != 4*time.Second {
		t.Fatalf("unexpected keepalive %s", cfg.SSEKeepalive)
	}
	if !cfg.RequireSharedStores || !cfg.RequireMigrations {
		t.Fatal("shared stores and migrations must default to required")
	}
	if cfg.Runtime != "go" || cfg.StoreBackend != "hybrid" || cfg.GoOwnershipMode != "disabled" || cfg.RequireAPIKey != "auto" {
		t.Fatalf("unexpected staged defaults: runtime=%s store=%s ownership=%s auth=%s", cfg.Runtime, cfg.StoreBackend, cfg.GoOwnershipMode, cfg.RequireAPIKey)
	}
	if cfg.GoWrites || cfg.GoChat || cfg.GoAdminWrite {
		t.Fatal("Go route/write flags must default to off")
	}
	if cfg.OutboundMaxTools != 1 {
		t.Fatalf("outbound max tools default = %d", cfg.OutboundMaxTools)
	}
	if cfg.OutboundMaxToolsOpenAI != 0 {
		t.Fatalf("openai max tools default = %d", cfg.OutboundMaxToolsOpenAI)
	}
	if cfg.OutboundMaxToolsResponsesNative != 0 {
		t.Fatalf("responses native max tools default = %d", cfg.OutboundMaxToolsResponsesNative)
	}
	if cfg.OutboundToolGap != 80*time.Millisecond {
		t.Fatalf("tool gap default = %s", cfg.OutboundToolGap)
	}
	if cfg.OutboundToolGapNative != 0 {
		t.Fatalf("native tool gap default = %s", cfg.OutboundToolGapNative)
	}
}

func TestLoadAliasesAndOverrides(t *testing.T) {
	t.Setenv("REDIS_URL", "redis://redis:6379/1")
	t.Setenv("GROK2API_REDIS_URL", "redis://preferred:6379/2")
	t.Setenv("DATABASE_URL", "postgresql://db/fallback")
	t.Setenv("GROK2API_PORT", "40081")
	t.Setenv("GROK2API_SSE_KEEPALIVE", "0.08")
	t.Setenv("GROK2API_REQUIRE_SHARED_STORES", "off")
	t.Setenv("GROK2API_REQUIRE_MIGRATIONS", "false")
	t.Setenv("GROK2API_API_KEY", "legacy-secret")
	t.Setenv("GROK2API_REQUIRE_API_KEY", "true")
	t.Setenv("GROK2API_GO_PUBLIC_READ", "on")
	t.Setenv("GROK2API_GO_OWNERSHIP_MODE", "canary")
	t.Setenv("GROK2API_REGISTRATION_SERVICE_URL", "http://registration:8080/")

	cfg, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.RedisURL != "redis://preferred:6379/2" {
		t.Fatalf("unexpected redis URL %q", cfg.RedisURL)
	}
	if cfg.DatabaseURL != "postgresql://db/fallback" {
		t.Fatalf("unexpected database URL %q", cfg.DatabaseURL)
	}
	if cfg.Port != 40081 || cfg.SSEKeepalive != 80*time.Millisecond {
		t.Fatalf("unexpected parsed values: port=%d keepalive=%s", cfg.Port, cfg.SSEKeepalive)
	}
	if cfg.RequireSharedStores || cfg.RequireMigrations {
		t.Fatal("expected shared store and migration overrides off")
	}
	if !cfg.GoPublicRead || cfg.GoOwnershipMode != "canary" || cfg.LegacyAPIKey != "legacy-secret" || cfg.RequireAPIKey != "true" {
		t.Fatalf("unexpected Go overrides: public=%v ownership=%s legacy=%q auth=%q", cfg.GoPublicRead, cfg.GoOwnershipMode, cfg.LegacyAPIKey, cfg.RequireAPIKey)
	}
	if cfg.RegistrationServiceURL != "http://registration:8080" {
		t.Fatalf("unexpected registration URL %q", cfg.RegistrationServiceURL)
	}
}

func TestLoadRejectsInvalidValues(t *testing.T) {
	t.Setenv("GROK2API_PORT", "70000")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid port error")
	}
}

func TestLoadRejectsInvalidEnum(t *testing.T) {
	t.Setenv("GROK2API_GO_OWNERSHIP_MODE", "maybe")
	if _, err := Load(); err == nil {
		t.Fatal("expected invalid ownership mode error")
	}
}
