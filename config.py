"""Configuration for grokcli-2api (standalone — no local Grok CLI required)."""

from __future__ import annotations

import os
from pathlib import Path

# Local server
HOST = os.getenv("GROK2API_HOST", "127.0.0.1")
PORT = int(os.getenv("GROK2API_PORT", "3000"))
# Optional public origin for admin UI / API guide links on public deployments.
# Example: https://api.example.com  or  http://1.2.3.4:40081
# When unset, request Host/X-Forwarded-* headers are preferred over 127.0.0.1.
PUBLIC_BASE_URL = (
    os.getenv("GROK2API_PUBLIC_BASE_URL")
    or os.getenv("GROK2API_PUBLIC_URL")
    or os.getenv("PUBLIC_BASE_URL")
    or ""
).strip().rstrip("/")
# Legacy single key (still accepted if set). Prefer managed keys in data/keys.json
API_KEY = os.getenv("GROK2API_API_KEY", "")

# Admin console password (required for /admin APIs & web when set).
# If empty, first-time setup can create one stored in data/settings.json
ADMIN_PASSWORD = os.getenv("GROK2API_ADMIN_PASSWORD", "")

# Upstream cli-chat-proxy (session-token compatible endpoint)
UPSTREAM_BASE = os.getenv(
    "GROK_CLI_CHAT_PROXY_BASE_URL",
    "https://cli-chat-proxy.grok.com/v1",
).rstrip("/")

# App data — fully self-contained under project (or GROK2API_DATA_DIR)
APP_ROOT = Path(__file__).resolve().parent
DATA_DIR = Path(os.getenv("GROK2API_DATA_DIR", APP_ROOT / "data"))
KEYS_FILE = DATA_DIR / "keys.json"
SETTINGS_FILE = DATA_DIR / "settings.json"
STATIC_DIR = APP_ROOT / "static"

# Auth + model cache live in DATA_DIR by default (NOT ~/.grok)
# Override with GROK2API_AUTH_FILE / GROK2API_MODELS_CACHE if needed.
AUTH_FILE = Path(os.getenv("GROK2API_AUTH_FILE", DATA_DIR / "auth.json"))
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

# Account rotation mode (also changeable in admin UI / settings.json)
# round_robin | random | least_used  (all accounts equal; no primary)
# Empty → settings.json / default round_robin
ACCOUNT_MODE = os.getenv("GROK2API_ACCOUNT_MODE", "").strip().lower()

# Sticky account per conversation (avoid mid-chat account rotation breaking memory)
CONVERSATION_AFFINITY = os.getenv(
    "GROK2API_CONVERSATION_AFFINITY", "1"
).lower() not in ("0", "false", "no")
# How long to keep conversation→account binding (seconds)
AFFINITY_TTL = float(os.getenv("GROK2API_AFFINITY_TTL", "7200"))
AFFINITY_MAX = int(os.getenv("GROK2API_AFFINITY_MAX", "5000"))

# Background token maintenance interval (seconds) for multi-account on Linux
TOKEN_MAINTAIN_INTERVAL = float(os.getenv("GROK2API_TOKEN_MAINTAIN_INTERVAL", "300"))

# Background model health probe interval (seconds). 0 = only on demand / on error
MODEL_HEALTH_INTERVAL = float(os.getenv("GROK2API_MODEL_HEALTH_INTERVAL", "600"))
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
TOKEN_REFRESH_WORKERS = _env_int("GROK2API_TOKEN_REFRESH_WORKERS", 4, maximum=16)
MODEL_PROBE_WORKERS = _env_int("GROK2API_MODEL_PROBE_WORKERS", 4, maximum=16)
QUOTA_WORKERS = _env_int("GROK2API_QUOTA_WORKERS", 4, maximum=16)
SSO_IMPORT_WORKERS = _env_int("GROK2API_SSO_IMPORT_WORKERS", 4, maximum=16)
# Startup stagger: first background cycle waits longer with large pools
TOKEN_MAINTAIN_STARTUP_DELAY = _env_float(
    "GROK2API_TOKEN_MAINTAIN_STARTUP_DELAY", 30.0, minimum=5.0
)
MODEL_HEALTH_STARTUP_DELAY = _env_float(
    "GROK2API_MODEL_HEALTH_STARTUP_DELAY", 90.0, minimum=15.0
)
# Max accounts to refresh/probe per background cycle (rest deferred)
TOKEN_REFRESH_BATCH = _env_int("GROK2API_TOKEN_REFRESH_BATCH", 40, maximum=500)
MODEL_PROBE_BATCH = _env_int("GROK2API_MODEL_PROBE_BATCH", 40, maximum=500)

# Background worker master switches (centralised here so other modules don't
# re-parse os.getenv). Background threads no longer run on the event loop,
# so these can safely default to enabled even in single-worker deployments.
TOKEN_MAINTAIN = os.getenv("GROK2API_TOKEN_MAINTAIN", "1").lower() not in (
    "0",
    "false",
    "no",
)
MODEL_HEALTH = os.getenv("GROK2API_MODEL_HEALTH", "1").lower() not in (
    "0",
    "false",
    "no",
)

# Admin / bulk operation timeouts (seconds). These bound synchronous work that
# is offloaded to threads so a stuck OIDC/network call cannot hang a request.
ADMIN_SYNC_TIMEOUT = float(os.getenv("GROK2API_ADMIN_SYNC_TIMEOUT", "60.0"))
ADMIN_BULK_TIMEOUT = float(os.getenv("GROK2API_ADMIN_BULK_TIMEOUT", "300.0"))
OIDC_NETWORK_TIMEOUT = float(os.getenv("GROK2API_OIDC_NETWORK_TIMEOUT", "60.0"))

# Heavy admin endpoints (refresh-all, probe-all, quota-all, SSO-import) return a
# job_id immediately and run in the background. Set to 0/false/no for legacy
# synchronous responses.
ADMIN_BULK_ASYNC = os.getenv("GROK2API_ADMIN_BULK_ASYNC", "1").lower() not in (
    "0",
    "false",
    "no",
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
MOEMAIL_BASE_URL = os.getenv("GROK2API_MOEMAIL_BASE_URL", "https://moemail.521884.xyz")
MOEMAIL_API_KEY = os.getenv("GROK2API_MOEMAIL_API_KEY", "")
MOEMAIL_DOMAIN = os.getenv("GROK2API_MOEMAIL_DOMAIN", "lolicc.online")
MOEMAIL_EXPIRY_MS = int(os.getenv("GROK2API_MOEMAIL_EXPIRY_MS", "3600000"))
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

# Request timeout (seconds) for non-stream collection
TIMEOUT = float(os.getenv("GROK2API_TIMEOUT", "600"))

# SSE idle keepalive interval for secondary relays (new-api / nginx).
# Emit `: keepalive` comments when upstream is silent (thinking gaps).
SSE_KEEPALIVE_INTERVAL = float(os.getenv("GROK2API_SSE_KEEPALIVE", "8"))

# Compatibility for relays/UIs that only render delta.content (not reasoning_content).
# - off: pass through reasoning_content only
# - think_tag: stream reasoning as content wrapped in <think>...</think>
# - content: merge reasoning into content without tags
REASONING_COMPAT = os.getenv("GROK2API_REASONING_COMPAT", "think_tag").strip().lower()

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
    "grok-build": "grok-build",
    "default": DEFAULT_MODEL,
}
