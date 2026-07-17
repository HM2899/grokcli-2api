"""Configuration for grokcli-2api (standalone — no local Grok CLI required)."""

from __future__ import annotations

import os
from pathlib import Path

# Local server
HOST = os.getenv("GROK2API_HOST", "0.0.0.0")
PORT = int(os.getenv("GROK2API_PORT", "3000"))


def _default_workers() -> int:
    """High-concurrency default: scale with CPU, min 2, max 8 (override via env)."""
    raw = (os.getenv("GROK2API_WORKERS") or "").strip()
    if raw:
        try:
            return max(1, int(raw))
        except ValueError:
            pass
    try:
        import os as _os

        n = int(_os.cpu_count() or 2)
    except Exception:
        n = 2
    # Leave a core for Redis/PG/OS; never default to single-worker.
    return max(2, min(8, n))


# Uvicorn worker processes (high-concurrency default). Requires Redis + PostgreSQL.
WORKERS = _default_workers()


def _env_truthy(name: str, default: str = "0") -> bool:
    return (os.getenv(name, default) or default).strip().lower() in (
        "1",
        "true",
        "yes",
        "on",
    )


# Dev hot-reload (uvicorn --reload). Production must keep this off.
# When enabled, workers are forced to 1 (uvicorn cannot combine reload + multi-worker).
RELOAD = _env_truthy("GROK2API_RELOAD", "0")
# Comma-separated extra watch dirs (relative to app root or absolute). Empty = defaults.
RELOAD_DIRS = (os.getenv("GROK2API_RELOAD_DIRS") or "").strip()
# Comma-separated glob includes; empty = uvicorn defaults (*.py etc.).
RELOAD_INCLUDES = (os.getenv("GROK2API_RELOAD_INCLUDES") or "").strip()
# Comma-separated glob excludes (always ignore data/ and __pycache__ noise).
RELOAD_EXCLUDES = (os.getenv("GROK2API_RELOAD_EXCLUDES") or "").strip()
# Optional public origin for admin UI / API guide links on public deployments.
# Leave empty to auto-detect:
#   - admin/API responses use request Host / X-Forwarded-* first
#   - startup banner may fall back to the host's outbound/public IP
# Explicit override still works: https://api.example.com  or  http://1.2.3.4:40081
PUBLIC_BASE_URL = (
    os.getenv("GROK2API_PUBLIC_BASE_URL")
    or os.getenv("GROK2API_PUBLIC_URL")
    or os.getenv("PUBLIC_BASE_URL")
    or ""
).strip().rstrip("/")
# Legacy single key (still accepted if set). Prefer managed keys in PostgreSQL
# (or keys.json only when STORE_BACKEND=file).
API_KEY = os.getenv("GROK2API_API_KEY", "")

# Admin console password bootstrap only.
# Live auth uses the durable password hash in PostgreSQL (hybrid) or
# settings.json (file mode). GROK2API_ADMIN_PASSWORD is imported into the
# store once when no hash exists yet; it is NOT a parallel live password.
ADMIN_PASSWORD = os.getenv("GROK2API_ADMIN_PASSWORD", "")

# Upstream cli-chat-proxy (session-token compatible endpoint)
UPSTREAM_BASE = os.getenv(
    "GROK_CLI_CHAT_PROXY_BASE_URL",
    "https://cli-chat-proxy.grok.com/v1",
).rstrip("/")

# App data — fully self-contained under project (or GROK2API_DATA_DIR).
# This file lives at grok2api/config.py; repo root is one level up.
APP_ROOT = Path(__file__).resolve().parent.parent
DATA_DIR = Path(os.getenv("GROK2API_DATA_DIR", APP_ROOT / "data"))
# File-mode / migration paths only. Hybrid runtime does not write these.
KEYS_FILE = DATA_DIR / "keys.json"
SETTINGS_FILE = DATA_DIR / "settings.json"
STATIC_DIR = APP_ROOT / "static"

# Auth file path (file mode / admin export target). Model catalog lives in PostgreSQL.
# Override with GROK2API_AUTH_FILE if needed. Hybrid runtime never mirrors here.
AUTH_FILE = Path(os.getenv("GROK2API_AUTH_FILE", DATA_DIR / "auth.json"))
# Deprecated: models_cache.json is no longer used at runtime. Kept only so old
# env/docs don't break imports; migrate_json_to_pg may still read this path.
MODELS_CACHE = Path(
    os.getenv("GROK2API_MODELS_CACHE", DATA_DIR / "models_cache.json")
)

# Client headers for upstream proxy (version string only — no local CLI binary)
# Keep surface as grok-cli so cli-chat-proxy accepts the session.
CLI_VERSION = os.getenv("GROK2API_CLI_VERSION", "0.2.93")
CLIENT_SURFACE = os.getenv("GROK2API_CLIENT_SURFACE", "grok-cli")
CLIENT_IDENTIFIER = os.getenv("GROK2API_CLIENT_IDENTIFIER", "grokcli-2api")

# Default model when client omits / sends generic names
DEFAULT_MODEL = os.getenv("GROK2API_DEFAULT_MODEL", "grok-4.5")

# Account rotation mode (also changeable in admin UI / settings store)
# round_robin | random | least_used  (all accounts equal; no primary)
# Empty → settings store / default round_robin
ACCOUNT_MODE = os.getenv("GROK2API_ACCOUNT_MODE", "").strip().lower()

# Sticky account per conversation (avoid mid-chat account rotation breaking memory)
CONVERSATION_AFFINITY = os.getenv(
    "GROK2API_CONVERSATION_AFFINITY", "1"
).lower() not in ("0", "false", "no")
# How long to keep conversation→account binding (seconds)
AFFINITY_TTL = float(os.getenv("GROK2API_AFFINITY_TTL", "7200"))
AFFINITY_MAX = int(os.getenv("GROK2API_AFFINITY_MAX", "5000"))

# Background token maintenance interval (seconds) for multi-account on Linux.
# Large pools refresh in small batches, so a shorter base interval still keeps
# access tokens warm without one huge fan-out.
TOKEN_MAINTAIN_INTERVAL = float(os.getenv("GROK2API_TOKEN_MAINTAIN_INTERVAL", "90"))

# Background model health probe interval (seconds). 0 = only on demand / on error
MODEL_HEALTH_INTERVAL = float(os.getenv("GROK2API_MODEL_HEALTH_INTERVAL", "900"))
# Auto-disable account from rotation when model probe fails
MODEL_HEALTH_AUTO_DISABLE = os.getenv(
    "GROK2API_MODEL_HEALTH_AUTO_DISABLE", "1"
).lower() not in ("0", "false", "no")
# Models to probe periodically (comma-separated); empty = DEFAULT_MODEL only
_probe_env = os.getenv("GROK2API_PROBE_MODELS", "").strip()
PROBE_MODELS: list[str] = (
    [m.strip() for m in _probe_env.split(",") if m.strip()]
    if _probe_env
    else [DEFAULT_MODEL]
)

# Large multi-account pools (hundreds of entries) can freeze WSL/low-RAM hosts
# if startup fans out network + rewrites 1MB auth.json per account.
# These caps keep peak concurrency / I/O bounded.
def _env_int(name: str, default: int, *, minimum: int = 1, maximum: int = 64) -> int:
    try:
        v = int(os.getenv(name, str(default)))
    except (TypeError, ValueError):
        v = default
    return max(minimum, min(maximum, v))


def _env_float(name: str, default: float, *, minimum: float = 0.0) -> float:
    try:
        v = float(os.getenv(name, str(default)))
    except (TypeError, ValueError):
        v = default
    return max(minimum, v)


# Concurrent OIDC refresh / model probe / quota / SSO-import workers
# Higher defaults for multi-worker / large pools (override downward if needed).
TOKEN_REFRESH_WORKERS = _env_int("GROK2API_TOKEN_REFRESH_WORKERS", 4, maximum=32)
MODEL_PROBE_WORKERS = _env_int("GROK2API_MODEL_PROBE_WORKERS", 4, maximum=32)
QUOTA_WORKERS = _env_int("GROK2API_QUOTA_WORKERS", 6, maximum=32)
# SSO cookie → token is network-bound device flow; 8–16 works well on hybrid stacks.
SSO_IMPORT_WORKERS = _env_int("GROK2API_SSO_IMPORT_WORKERS", 12, maximum=32)
# Startup stagger: first background cycle waits longer with large pools
TOKEN_MAINTAIN_STARTUP_DELAY = _env_float(
    "GROK2API_TOKEN_MAINTAIN_STARTUP_DELAY", 20.0, minimum=5.0
)
MODEL_HEALTH_STARTUP_DELAY = _env_float(
    "GROK2API_MODEL_HEALTH_STARTUP_DELAY", 45.0, minimum=10.0
)
# Max accounts to refresh/probe per background cycle (rest deferred)
TOKEN_REFRESH_BATCH = _env_int("GROK2API_TOKEN_REFRESH_BATCH", 40, maximum=500)
MODEL_PROBE_BATCH = _env_int("GROK2API_MODEL_PROBE_BATCH", 30, maximum=500)
# Manual / admin probes: max models per account in one cycle (background always 1).
# Prevents PROBE_MODELS cartesian explosion from freezing large pools.
MODEL_PROBE_MAX_MODELS_PER_ACCOUNT = _env_int(
    "GROK2API_MODEL_PROBE_MAX_MODELS_PER_ACCOUNT", 2, minimum=1, maximum=16
)
# Serialize heavy maintenance so refresh + probe never stampede together.
# Token refresh may wait this long for a probe cycle to finish.
MAINTENANCE_LOCK_TIMEOUT = _env_float(
    "GROK2API_MAINTENANCE_LOCK_TIMEOUT", 180.0, minimum=5.0
)

# xAI OIDC (public client — device code + refresh; no local CLI binary)
GROK_CLI_CLIENT_ID = os.getenv(
    "GROK2API_OIDC_CLIENT_ID",
    "b1a00492-073a-47ea-816f-4c329264a828",
)
OIDC_ISSUER = os.getenv("GROK2API_OIDC_ISSUER", "https://auth.x.ai")
OIDC_DEVICE_URL = os.getenv(
    "GROK2API_OIDC_DEVICE_URL",
    f"{OIDC_ISSUER.rstrip('/')}/oauth2/device/code",
)
OIDC_TOKEN_URL = os.getenv(
    "GROK2API_OIDC_TOKEN_URL",
    f"{OIDC_ISSUER.rstrip('/')}/oauth2/token",
)
OIDC_SCOPES = os.getenv(
    "GROK2API_OIDC_SCOPES",
    "openid profile email offline_access grok-cli:access api:access "
    "conversations:read conversations:write",
)
# Email-assisted account registration.
XAI_ACCOUNTS_URL = os.getenv("GROK2API_XAI_ACCOUNTS_URL", "https://accounts.x.ai/")
XAI_PROXY = (
    os.getenv("GROK2API_XAI_PROXY")
    or os.getenv("GROK2API_PROXY")
    or ""
).strip()
# Multi-line proxy pool (preferred). Falls back to XAI_PROXY when empty.
# One proxy per line / comma / semicolon. Supports host:port:user:pass.
XAI_PROXY_POOL = (
    os.getenv("GROK2API_XAI_PROXY_POOL")
    or os.getenv("GROK2API_PROXY_POOL")
    or XAI_PROXY
    or ""
).strip()
XAI_PROXY_USERNAME = (
    os.getenv("GROK2API_XAI_PROXY_USERNAME")
    or os.getenv("GROK2API_PROXY_USERNAME")
    or ""
).strip()
XAI_PROXY_PASSWORD = (
    os.getenv("GROK2API_XAI_PROXY_PASSWORD")
    or os.getenv("GROK2API_PROXY_PASSWORD")
    or ""
).strip()
# Proxy rotation for multi-proxy pools: round_robin | random | sticky
XAI_PROXY_STRATEGY = (
    os.getenv("GROK2API_XAI_PROXY_STRATEGY")
    or os.getenv("GROK2API_PROXY_STRATEGY")
    or "round_robin"
).strip().lower() or "round_robin"
MOEMAIL_BASE_URL = os.getenv("GROK2API_MOEMAIL_BASE_URL", "https://moemail.example.com")
MOEMAIL_API_KEY = os.getenv("GROK2API_MOEMAIL_API_KEY", "")
MOEMAIL_DOMAIN = os.getenv("GROK2API_MOEMAIL_DOMAIN", "example.com")
MOEMAIL_EXPIRY_MS = int(os.getenv("GROK2API_MOEMAIL_EXPIRY_MS", "3600000"))
# Temp-mail provider for protocol registration:
# moemail | yyds (vip.215.im) | gptmail (mail.chatgpt.org.uk)
MAIL_PROVIDER = (
    os.getenv("GROK2API_MAIL_PROVIDER") or os.getenv("MAIL_PROVIDER") or "moemail"
).strip().lower() or "moemail"
# Auto-refresh access tokens this many seconds before expiry
TOKEN_REFRESH_SKEW = float(os.getenv("GROK2API_TOKEN_REFRESH_SKEW", "120"))

# Force stream upstream (most models only support streaming on this proxy)
FORCE_UPSTREAM_STREAM = os.getenv("GROK2API_FORCE_STREAM", "1") not in (
    "0",
    "false",
    "False",
)

# When True (default): if any managed key exists OR env API_KEY set, require a key.
# When no keys at all, open access (dev mode) unless REQUIRE_API_KEY=1
REQUIRE_API_KEY = os.getenv("GROK2API_REQUIRE_API_KEY", "auto")

# Request timeout (seconds) for non-stream collection / overall upstream call.
# Stream reads use a separate read timeout (see HTTP_READ_TIMEOUT) so a silent
# thinking gap does not burn the whole request budget.
TIMEOUT = float(os.getenv("GROK2API_TIMEOUT", "900"))

# SSE idle keepalive interval for secondary relays (sub2api / new-api / nginx).
# Emit `: keepalive` / Anthropic ping when upstream is silent (thinking gaps).
# Default 4s — sub2api and some frontends idle-close around 10–15s.
SSE_KEEPALIVE_INTERVAL = float(os.getenv("GROK2API_SSE_KEEPALIVE", "4"))

# Compatibility for relays/UIs that only render delta.content (not reasoning_content).
# Default off: keep reasoning in reasoning_content so OpenAI→Claude relays (sub2api)
# do not dump thinking into visible assistant content.
# - off: pass through reasoning_content only (recommended for sub2api / Claude clients)
# - think_tag: stream reasoning as content wrapped in <think>...</think>
# - content: merge reasoning into content without tags
REASONING_COMPAT = os.getenv("GROK2API_REASONING_COMPAT", "off").strip().lower()

# ── Shared stores (high-concurrency mode: Redis + PostgreSQL required) ─────
# hybrid is the only supported production mode. File JSON is migration-only.
# Defaults point at compose profile `store` services on localhost.


def _env_url(*names: str, default: str = "") -> str:
    for n in names:
        v = (os.getenv(n) or "").strip()
        if v:
            return v
    return default


_store_backend_raw = (os.getenv("GROK2API_STORE_BACKEND") or "hybrid").strip().lower()
if _store_backend_raw == "file":
    # Explicit escape hatch only — not recommended; multi-worker will refuse.
    STORE_BACKEND = "file"
else:
    STORE_BACKEND = "hybrid"

# File mode must not opportunistically connect to localhost shared stores. An
# explicit URL still opts in for migration tooling, but an absent URL remains
# absent instead of inheriting the hybrid localhost default.
_store_url_default = "" if STORE_BACKEND == "file" else "postgresql://grok2api:grok2api@127.0.0.1:5432/grok2api"
_redis_url_default = "" if STORE_BACKEND == "file" else "redis://127.0.0.1:6379/0"
DATABASE_URL = _env_url(
    "GROK2API_DATABASE_URL",
    "DATABASE_URL",
    default=_store_url_default,
)
REDIS_URL = _env_url(
    "GROK2API_REDIS_URL",
    "REDIS_URL",
    default=_redis_url_default,
)

# Maintainer leader: only one process runs token_maintainer + model_health.
# auto: elect via Redis (required in high-concurrency mode).
# 1/true: always start maintainers in this process.
# 0/false: never start maintainers here (another process owns them).
MAINTAINER_LEADER = (os.getenv("GROK2API_MAINTAINER_LEADER") or "auto").strip().lower()

# Redis key namespace prefix (all g2a:* keys).
REDIS_KEY_PREFIX = (os.getenv("GROK2API_REDIS_PREFIX") or "g2a").strip() or "g2a"
# Leader lock TTL / renew interval (seconds).
MAINTAINER_LEADER_TTL = _env_float(
    "GROK2API_MAINTAINER_LEADER_TTL", 30.0, minimum=5.0
)
MAINTAINER_LEADER_RENEW = _env_float(
    "GROK2API_MAINTAINER_LEADER_RENEW", 10.0, minimum=2.0
)

# Fail closed unless both shared stores are configured (set 0 only for emergency).
REQUIRE_SHARED_STORES = os.getenv("GROK2API_REQUIRE_SHARED_STORES", "1").lower() not in (
    "0",
    "false",
    "no",
    "off",
)

# ── History compaction (Claude Code / long tool loops via sub2api) ──
# Shrink past tool results before upstream so multi-round agent sessions
# do not blow past body size / context and surface as client API errors.
# Implemented in history_compact.py; env knobs live there too (imported below).

# Map common aliases -> real model ids (OpenAI + Anthropic client defaults)
MODEL_ALIASES: dict[str, str] = {
    "gpt-4": DEFAULT_MODEL,
    "gpt-4o": DEFAULT_MODEL,
    "gpt-3.5-turbo": DEFAULT_MODEL,
    "gpt-4-turbo": DEFAULT_MODEL,
    "claude": DEFAULT_MODEL,
    "claude-3": DEFAULT_MODEL,
    "claude-3-5-sonnet": DEFAULT_MODEL,
    "claude-3-5-sonnet-20240620": DEFAULT_MODEL,
    "claude-3-5-sonnet-20241022": DEFAULT_MODEL,
    "claude-3-5-haiku": DEFAULT_MODEL,
    "claude-3-5-haiku-20241022": DEFAULT_MODEL,
    "claude-3-haiku": DEFAULT_MODEL,
    "claude-3-haiku-20240307": DEFAULT_MODEL,
    "claude-3-opus": DEFAULT_MODEL,
    "claude-3-opus-20240229": DEFAULT_MODEL,
    "claude-3-sonnet": DEFAULT_MODEL,
    "claude-3-sonnet-20240229": DEFAULT_MODEL,
    "claude-sonnet-4": DEFAULT_MODEL,
    "claude-sonnet-4-0": DEFAULT_MODEL,
    "claude-sonnet-4-20250514": DEFAULT_MODEL,
    "claude-sonnet-4-5": DEFAULT_MODEL,
    "claude-sonnet-4-5-20250929": DEFAULT_MODEL,
    "claude-opus-4": DEFAULT_MODEL,
    "claude-opus-4-0": DEFAULT_MODEL,
    "claude-opus-4-20250514": DEFAULT_MODEL,
    "claude-opus-4-5": DEFAULT_MODEL,
    "claude-haiku-4": DEFAULT_MODEL,
    "claude-haiku-4-5": DEFAULT_MODEL,
    "claude-haiku-4-5-20251001": DEFAULT_MODEL,
    "grok": DEFAULT_MODEL,
    "grok-latest": DEFAULT_MODEL,
    # Real cli-chat-proxy model id (often omitted from /v1/models list).
    "grok-build": "grok-build",
    "grok-build-latest": "grok-build",
    # Historical free-tier name seen in free-usage error payloads.
    "grok-4.5-build-free": "grok-build",
    "grok-4.5-build": "grok-build",
    "default": DEFAULT_MODEL,
}
