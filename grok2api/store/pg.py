"""PostgreSQL connection pool + schema bootstrap (optional Phase 2 backend).

Requires: pip install 'psycopg[binary,pool]>=3.1'
Configured via DATABASE_URL / GROK2API_DATABASE_URL.
"""

from __future__ import annotations

import json
import threading
import time
from contextlib import contextmanager
from typing import Any, Iterator

from grok2api.config import DATABASE_URL

_pool = None
_pool_lock = threading.Lock()
_schema_ready = False
_import_error: str | None = None

SCHEMA_SQL = """
CREATE TABLE IF NOT EXISTS accounts (
  id TEXT PRIMARY KEY,
  email TEXT,
  user_id TEXT,
  team_id TEXT,
  payload JSONB NOT NULL,
  expires_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_accounts_email ON accounts (email);
CREATE INDEX IF NOT EXISTS idx_accounts_user_id ON accounts (user_id);

CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  secret TEXT,
  enabled BOOLEAN NOT NULL DEFAULT true,
  note TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  request_count BIGINT NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS app_settings (
  key TEXT PRIMARY KEY,
  value JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account_pool (
  account_id TEXT PRIMARY KEY,
  enabled BOOLEAN NOT NULL DEFAULT true,
  weight INT NOT NULL DEFAULT 1,
  disabled_for_quota BOOLEAN NOT NULL DEFAULT false,
  disabled_reason TEXT,
  quota_disabled_at TIMESTAMPTZ,
  quota_source TEXT,
  last_quota JSONB,
  last_probe JSONB,
  blocked_models JSONB NOT NULL DEFAULT '{}'::jsonb,
  request_count BIGINT NOT NULL DEFAULT 0,
  success_count BIGINT NOT NULL DEFAULT 0,
  fail_count BIGINT NOT NULL DEFAULT 0,
  last_used_at TIMESTAMPTZ,
  last_error TEXT,
  cooldown_until TIMESTAMPTZ,
  -- Extra durable meta (probe streaks, disabled_source, consecutive_fails, …)
  extra JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
"""

# Columns / tables added after initial deploy — applied idempotently on connect.
_SCHEMA_MIGRATIONS = (
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS extra JSONB NOT NULL DEFAULT '{}'::jsonb",
    # Durable account status fields (bound to account_id; not recomputed from Redis).
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS pool_status TEXT NOT NULL DEFAULT 'normal'",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_count INT NOT NULL DEFAULT 0",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_reason TEXT",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_code TEXT",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_model TEXT",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_tokens_actual BIGINT",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_tokens_limit BIGINT",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS last_probe_status TEXT",
    "CREATE INDEX IF NOT EXISTS idx_account_pool_status ON account_pool (pool_status)",
    "CREATE INDEX IF NOT EXISTS idx_account_pool_cooldown_count ON account_pool (cooldown_count) WHERE cooldown_count > 0",
    """
    CREATE TABLE IF NOT EXISTS admin_audit_logs (
      id BIGSERIAL PRIMARY KEY,
      created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      actor TEXT,
      action TEXT NOT NULL,
      target_type TEXT,
      target_id TEXT,
      summary TEXT,
      detail JSONB NOT NULL DEFAULT '{}'::jsonb,
      ip TEXT,
      user_agent TEXT,
      ok BOOLEAN NOT NULL DEFAULT true
    )
    """,
    "CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_created_at ON admin_audit_logs (created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_action ON admin_audit_logs (action)",
    "CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_target ON admin_audit_logs (target_type, target_id)",
    # Background / long-running task log (registration, SSO import, probe, renew…).
    """
    CREATE TABLE IF NOT EXISTS task_logs (
      id BIGSERIAL PRIMARY KEY,
      created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      finished_at TIMESTAMPTZ,
      kind TEXT NOT NULL,
      task_id TEXT,
      status TEXT NOT NULL DEFAULT 'running',
      summary TEXT,
      detail JSONB NOT NULL DEFAULT '{}'::jsonb,
      ok BOOLEAN,
      progress_done INTEGER NOT NULL DEFAULT 0,
      progress_total INTEGER NOT NULL DEFAULT 0
    )
    """,
    "CREATE INDEX IF NOT EXISTS idx_task_logs_created_at ON task_logs (created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_task_logs_kind ON task_logs (kind)",
    "CREATE INDEX IF NOT EXISTS idx_task_logs_status ON task_logs (status)",
    "CREATE INDEX IF NOT EXISTS idx_task_logs_task_id ON task_logs (task_id)",
    # Token / request usage daily rollups (proxy-side analytics).
    """
    CREATE TABLE IF NOT EXISTS usage_daily (
      day DATE NOT NULL,
      dim TEXT NOT NULL,
      dim_id TEXT NOT NULL DEFAULT '',
      requests BIGINT NOT NULL DEFAULT 0,
      success BIGINT NOT NULL DEFAULT 0,
      fail BIGINT NOT NULL DEFAULT 0,
      prompt_tokens BIGINT NOT NULL DEFAULT 0,
      completion_tokens BIGINT NOT NULL DEFAULT 0,
      total_tokens BIGINT NOT NULL DEFAULT 0,
      updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      PRIMARY KEY (day, dim, dim_id)
    )
    """,
    "CREATE INDEX IF NOT EXISTS idx_usage_daily_dim_day ON usage_daily (dim, day DESC)",
    "ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS prompt_tokens_total BIGINT NOT NULL DEFAULT 0",
    "ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS completion_tokens_total BIGINT NOT NULL DEFAULT 0",
    "ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS total_tokens_total BIGINT NOT NULL DEFAULT 0",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS prompt_tokens_total BIGINT NOT NULL DEFAULT 0",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS completion_tokens_total BIGINT NOT NULL DEFAULT 0",
    "ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS total_tokens_total BIGINT NOT NULL DEFAULT 0",
    # Per-request usage detail (token breakdown, caller API key, client IP, cache).
    """
    CREATE TABLE IF NOT EXISTS usage_events (
      id BIGSERIAL PRIMARY KEY,
      created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
      api_key_id TEXT,
      account_id TEXT,
      model TEXT,
      protocol TEXT,
      path TEXT,
      stream BOOLEAN,
      ok BOOLEAN NOT NULL DEFAULT true,
      prompt_tokens BIGINT NOT NULL DEFAULT 0,
      completion_tokens BIGINT NOT NULL DEFAULT 0,
      total_tokens BIGINT NOT NULL DEFAULT 0,
      cache_read_tokens BIGINT NOT NULL DEFAULT 0,
      cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
      reasoning_tokens BIGINT NOT NULL DEFAULT 0,
      client_ip TEXT,
      user_agent TEXT,
      status_code INT,
      latency_ms INT,
      ttft_ms INT,
      error TEXT,
      detail JSONB NOT NULL DEFAULT '{}'::jsonb
    )
    """,
    # Existing DBs created before ttft_ms: add column idempotently.
    "ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS ttft_ms INT",
    "CREATE INDEX IF NOT EXISTS idx_usage_events_created_at ON usage_events (created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_usage_events_api_key ON usage_events (api_key_id, created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_usage_events_account ON usage_events (account_id, created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_usage_events_model ON usage_events (model, created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_usage_events_protocol ON usage_events (protocol, created_at DESC)",
    "CREATE INDEX IF NOT EXISTS idx_usage_events_client_ip ON usage_events (client_ip, created_at DESC)",
    # Upstream model catalog (synced from cli-chat-proxy /v1/models).
    """
    CREATE TABLE IF NOT EXISTS models (
      id TEXT PRIMARY KEY,
      name TEXT,
      description TEXT,
      owned_by TEXT NOT NULL DEFAULT 'xai',
      hidden BOOLEAN NOT NULL DEFAULT false,
      synthetic BOOLEAN NOT NULL DEFAULT false,
      context_window BIGINT,
      supports_reasoning_effort BOOLEAN,
      extra JSONB NOT NULL DEFAULT '{}'::jsonb,
      sort_order INT NOT NULL DEFAULT 100,
      fetched_at TIMESTAMPTZ,
      updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
    )
    """,
    "CREATE INDEX IF NOT EXISTS idx_models_hidden ON models (hidden)",
    "CREATE INDEX IF NOT EXISTS idx_models_sort ON models (sort_order, id)",
)


def database_url() -> str:
    try:
        import grok2api.config as _cfg

        u = (getattr(_cfg, "DATABASE_URL", None) or "").strip()
        if u:
            return u
    except Exception:
        pass
    return (DATABASE_URL or "").strip()


def pg_enabled() -> bool:
    if not database_url():
        return False
    try:
        get_pool()
        return _pool is not None
    except Exception:
        return False


def get_pool():
    global _pool, _import_error, _schema_ready
    if not database_url():
        return None
    if _pool is not None:
        return _pool
    with _pool_lock:
        if _pool is not None:
            return _pool
        try:
            from psycopg_pool import ConnectionPool  # type: ignore
        except ImportError as e:
            _import_error = (
                "psycopg not installed; pip install -r requirements-store.txt"
            )
            raise RuntimeError(_import_error) from e
        # Scale pool with process workers; cap to protect Postgres.
        try:
            import grok2api.config as _cfg
            workers = max(1, int(getattr(_cfg, "WORKERS", 2) or 2))
        except Exception:
            workers = 2
        max_size = max(4, min(32, workers * 3))
        min_size = 1 if workers <= 2 else 2
        _pool = ConnectionPool(
            conninfo=database_url(),
            min_size=min_size,
            max_size=max_size,
            kwargs={"autocommit": False},
            open=True,
        )
        _ensure_schema(_pool)
        return _pool


def _ensure_schema(pool) -> None:
    global _schema_ready
    if _schema_ready:
        return
    with pool.connection() as conn:
        with conn.cursor() as cur:
            cur.execute(SCHEMA_SQL)
            for stmt in _SCHEMA_MIGRATIONS:
                # These statements are idempotent. Any failure must abort startup:
                # swallowing one error leaves PostgreSQL's transaction aborted and
                # can make the process serve against a partially upgraded schema.
                cur.execute(stmt)
        conn.commit()
    _schema_ready = True


@contextmanager
def connection() -> Iterator[Any]:
    pool = get_pool()
    if pool is None:
        raise RuntimeError("PostgreSQL not configured")
    with pool.connection() as conn:
        yield conn


def ping(*, force: bool = False) -> bool:
    try:
        if not database_url():
            return False
        with connection() as conn:
            with conn.cursor() as cur:
                cur.execute("SELECT 1")
                cur.fetchone()
        return True
    except Exception:
        return False


def import_error() -> str | None:
    return _import_error


def _ts(value: Any) -> Any:
    """Convert unix float / ISO string to datetime-friendly value for psycopg."""
    if value is None or value == "":
        return None
    if hasattr(value, "timestamp"):
        return value
    try:
        from datetime import datetime, timezone

        if isinstance(value, (int, float)):
            return datetime.fromtimestamp(float(value), tz=timezone.utc)
        s = str(value).strip()
        if not s:
            return None
        # allow ISO
        if s.endswith("Z"):
            s = s[:-1] + "+00:00"
        return datetime.fromisoformat(s)
    except Exception:
        return None


def _unix(value: Any) -> float | None:
    if value is None:
        return None
    try:
        if hasattr(value, "timestamp"):
            return float(value.timestamp())
        return float(value)
    except Exception:
        return None


def json_dump(obj: Any) -> str:
    return json.dumps(obj, ensure_ascii=False, separators=(",", ":"), default=str)
