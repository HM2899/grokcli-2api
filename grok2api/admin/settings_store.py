"""Admin settings (password hash, flags, account pool)."""

from __future__ import annotations

import hashlib
import os
import hmac
import json
import secrets
import threading
import time
from typing import Any

from grok2api.config import ACCOUNT_MODE, ADMIN_PASSWORD, DATA_DIR, SETTINGS_FILE

_lock = threading.RLock()

# All modes treat accounts equally — no "primary" account concept.
VALID_ACCOUNT_MODES = ("round_robin", "random", "least_used")
DEFAULT_ACCOUNT_MODE = "round_robin"
# Legacy mode name migrated to round_robin
_LEGACY_MODES = {"primary": "round_robin"}

# In-memory settings cache + deferred dirty flush so probe/refresh of hundreds
# of accounts doesn't rewrite settings.json on every single account touch.
_mem: dict[str, Any] | None = None
_mem_dirty = False
_flush_timer: threading.Timer | None = None
_FLUSH_DELAY_SEC = 1.0


def _ensure() -> None:
    DATA_DIR.mkdir(parents=True, exist_ok=True)


_mem_mtime_ns: int | None = None
_MEM_STAT_MIN_INTERVAL = 0.5
_mem_stat_at = 0.0


def _pg_settings():
    try:
        from grok2api.store.settings_pg import enabled

        if enabled():
            from grok2api.store import settings_pg

            return settings_pg
    except Exception:
        return None
    return None


def _file_mtime_ns() -> int | None:
    try:
        st = SETTINGS_FILE.stat()
        return getattr(st, "st_mtime_ns", int(st.st_mtime * 1e9))
    except OSError:
        return None


# Scalar / JSON keys that must survive multi-worker restarts via app_settings.
# (account_pool / admin_password / sessions have dedicated tables or Redis.)
_PG_SCALAR_KEYS = (
    "account_mode",
    "token_maintain_enabled",
    "model_health_enabled",
    "reasoning_compat",
    "outbound_max_tools",
    "outbound_max_tools_openai",
    "outbound_tool_gap_sec",
    "history_compact_enabled",
    "history_compact_auto_chars",
    "history_keep_tool_rounds",
    "history_keep_recent_turns",  # legacy alias key
    "history_max_tool_result_chars",
    "history_tool_result_max_chars",  # legacy alias key
    "sse_keepalive",
    "conversation_affinity_enabled",
    "conversation_affinity_ttl_sec",
    "token_maintain_interval_sec",
    "token_refresh_skew_sec",
    "model_health_interval_sec",
    "model_health_auto_disable",
    "probe_models",
    "default_model",
    "cooldown_default_sec",
    "cooldown_auth_sec",
    "cooldown_rate_limit_sec",
    "cooldown_server_error_sec",
    "cooldown_max_sec",
    "soft_model_block_ttl_sec",
    "durable_model_block_ttl_sec",
    "probe_fail_kick_streak",
    "probe_fail_disable_streak",
    "probe_kick_cooldown_sec",
    "max_failover_attempts",
    # Protocol registration (MoeMail / YesCaptcha / proxy) — admin UI config
    "registration_config",
    # Outbound proxy pool for account-pool traffic (chat / probe / refresh)
    "outbound_proxy_config",
    # sub2api push (URL / login / default group)
    "sub2api_config",
    # CLIProxyAPI push (management API)
    "cliproxyapi_config",
)


def _load_disk() -> dict[str, Any]:
    _ensure()
    # PostgreSQL: compose a settings-shaped dict from durable tables.
    pg = _pg_settings()
    if pg is not None:
        try:
            data: dict[str, Any] = {}
            admin = pg.get_setting("admin_password")
            if isinstance(admin, dict):
                data.update(admin)
            for key in _PG_SCALAR_KEYS:
                try:
                    fv = pg.get_setting(key)
                except Exception:
                    fv = None
                if fv is not None:
                    data[key] = fv
            # Prefer dedicated pool table over any legacy blob
            data["account_pool"] = pg.get_account_pool_state()
            # Keep sessions only when Redis is unavailable
            try:
                from grok2api.store.redis_client import redis_enabled

                if not redis_enabled():
                    sessions = pg.get_setting("sessions")
                    if isinstance(sessions, dict):
                        data["sessions"] = sessions
            except Exception:
                pass
            data["updated_at"] = time.time()
            return data
        except Exception:
            pass
    if not SETTINGS_FILE.is_file():
        return {}
    try:
        data = json.loads(SETTINGS_FILE.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except (OSError, json.JSONDecodeError):
        return {}


def _refresh_pg_scalar_keys_into(data: dict[str, Any]) -> None:
    """Overlay durable scalar settings from PostgreSQL onto an in-memory map.

    Multi-worker hybrid mode: each process keeps a sticky ``_mem`` cache. Writes
    go to PG, but other workers would otherwise keep serving stale values
    (especially ``registration_config`` after admin save / start).
    """
    pg = _pg_settings()
    if pg is None or not isinstance(data, dict):
        return
    for key in _PG_SCALAR_KEYS:
        try:
            fv = pg.get_setting(key)
        except Exception:
            continue
        if fv is not None:
            data[key] = fv
        else:
            # Explicit delete/clear on another worker — drop local copy.
            data.pop(key, None)


def _load() -> dict[str, Any]:
    """Return the live in-memory settings map (revalidates PG / file)."""
    global _mem, _mem_mtime_ns, _mem_stat_at
    with _lock:
        now = time.time()
        # Hybrid / multi-worker: periodically re-read durable scalar keys from PG
        # so registration config / mode flags stay consistent across workers.
        if _mem is not None and _pg_settings() is not None:
            if now - _mem_stat_at >= _MEM_STAT_MIN_INTERVAL:
                _mem_stat_at = now
                try:
                    _refresh_pg_scalar_keys_into(_mem)
                except Exception:
                    pass
        # File backend: re-read settings.json when another process rewrote it.
        elif _mem is not None and _pg_settings() is None:
            if now - _mem_stat_at >= _MEM_STAT_MIN_INTERVAL:
                _mem_stat_at = now
                mt = _file_mtime_ns()
                if mt is not None and _mem_mtime_ns is not None and mt != _mem_mtime_ns:
                    _mem = _load_disk()
                    _mem_mtime_ns = mt
        if _mem is None:
            _mem = _load_disk()
            _mem_mtime_ns = _file_mtime_ns()
            _mem_stat_at = now
        return _mem


def _write_disk(data: dict[str, Any]) -> None:
    global _mem_mtime_ns
    pg = _pg_settings()
    if pg is not None:
        try:
            # Split durable pieces into PG tables / app_settings rows.
            for key in _PG_SCALAR_KEYS:
                if key not in data:
                    continue
                try:
                    val = data.get(key)
                    if key in ("token_maintain_enabled", "model_health_enabled"):
                        val = bool(val)
                    pg.set_setting(key, val)
                except Exception:
                    pass
            admin = {
                k: data[k]
                for k in (
                    "admin_password_hash",
                    "admin_password_salt",
                )
                if k in data
            }
            if admin:
                pg.set_setting("admin_password", admin)
            # account_pool is owned exclusively by the account_pool table when PG is on.
            # Never rewrite it from in-memory JSON blobs on settings flush.
            # sessions prefer Redis; if present and no redis, keep a side key
            if isinstance(data.get("sessions"), dict):
                try:
                    from grok2api.store.redis_client import redis_enabled

                    if not redis_enabled():
                        pg.set_setting("sessions", data["sessions"])
                except Exception:
                    pg.set_setting("sessions", data.get("sessions") or {})
            # PostgreSQL is the durable store — do not mirror settings.json at runtime.
            # settings.json remains only for STORE_BACKEND=file and migrate import.
            return
        except Exception:
            pass
    # File-mode fallback (explicit STORE_BACKEND=file / PG unavailable).
    _ensure()
    tmp = SETTINGS_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(SETTINGS_FILE)
    _mem_mtime_ns = _file_mtime_ns()


def _schedule_flush_locked() -> None:
    global _flush_timer, _mem_dirty
    _mem_dirty = True
    if _flush_timer is not None:
        return

    def _flush() -> None:
        global _flush_timer, _mem_dirty
        with _lock:
            _flush_timer = None
            if not _mem_dirty or _mem is None:
                _mem_dirty = False
                return
            snapshot = json.loads(json.dumps(_mem))  # deep-ish copy via json
            _mem_dirty = False
        try:
            _write_disk(snapshot)
        except Exception:
            # re-mark dirty so a later touch retries
            with _lock:
                _mem_dirty = True
                if _flush_timer is None:
                    _schedule_flush_locked()

    t = threading.Timer(_FLUSH_DELAY_SEC, _flush)
    t.daemon = True
    _flush_timer = t
    t.start()


def _save(data: dict[str, Any], *, immediate: bool = False) -> None:
    """
    Persist settings. Default is coalesced (1s) to avoid thrashing disk when
    model probes / quota checks touch hundreds of pool entries.
    """
    global _mem
    with _lock:
        _mem = data
        if immediate:
            # cancel pending timer
            global _flush_timer, _mem_dirty
            if _flush_timer is not None:
                try:
                    _flush_timer.cancel()
                except Exception:
                    pass
                _flush_timer = None
            _mem_dirty = False
            snapshot = json.loads(json.dumps(data))
            _write_disk(snapshot)
        else:
            _schedule_flush_locked()


def flush_settings() -> None:
    """Force any deferred settings writes to disk (call on shutdown if needed)."""
    global _flush_timer, _mem_dirty
    with _lock:
        if _flush_timer is not None:
            try:
                _flush_timer.cancel()
            except Exception:
                pass
            _flush_timer = None
        if _mem is None:
            _mem_dirty = False
            return
        snapshot = json.loads(json.dumps(_mem))
        _mem_dirty = False
    _write_disk(snapshot)


def _hash_password(password: str, salt: str) -> str:
    return hashlib.pbkdf2_hmac(
        "sha256", password.encode("utf-8"), salt.encode("utf-8"), 120_000
    ).hex()


def _stored_admin_hash_present(data: dict[str, Any] | None = None) -> bool:
    d = data if isinstance(data, dict) else _load()
    return bool(d.get("admin_password_hash") and d.get("admin_password_salt"))


def ensure_admin_password_seeded() -> dict[str, Any]:
    """Ensure admin password lives in durable store (PG / settings file).

    Auth source of truth is the stored password hash — **not** the env var.
    ``GROK2API_ADMIN_PASSWORD`` is only a bootstrap seed when no hash exists
    yet (first deploy). After seeding / setup / password change, login uses
    the database hash exclusively.
    """
    data = _load()
    if _stored_admin_hash_present(data):
        return {
            "ok": True,
            "source": "store",
            "seeded": False,
            "has_password": True,
        }
    env_pw = (ADMIN_PASSWORD or "").strip()
    if env_pw:
        # First boot with env password: persist hash so multi-worker / restarts
        # do not depend on env remaining the live credential.
        set_admin_password(env_pw)
        return {
            "ok": True,
            "source": "env_seeded",
            "seeded": True,
            "has_password": True,
        }
    return {
        "ok": True,
        "source": "none",
        "seeded": False,
        "has_password": False,
    }


def is_setup_needed() -> bool:
    # Prefer durable store; env alone no longer skips setup forever — it only
    # seeds once via ensure_admin_password_seeded().
    try:
        ensure_admin_password_seeded()
    except Exception:
        pass
    data = _load()
    return not _stored_admin_hash_present(data)


def has_admin_password() -> bool:
    try:
        ensure_admin_password_seeded()
    except Exception:
        pass
    return _stored_admin_hash_present()


def set_admin_password(password: str) -> None:
    if len(password) < 4:
        raise ValueError("密码至少 4 位")
    salt = secrets.token_hex(16)
    with _lock:
        data = _load()
        data["admin_password_hash"] = _hash_password(password, salt)
        data["admin_password_salt"] = salt
        data["admin_password_updated_at"] = time.time()
        # Record that password is managed in store (not live-env auth).
        data["admin_password_source"] = "store"
        data["updated_at"] = time.time()
        _save(data, immediate=True)


def change_admin_password(*, current: str, new_password: str) -> None:
    """Change admin password after verifying current credentials.

    Always writes the new hash to durable store (PostgreSQL / settings file).
    Environment ``GROK2API_ADMIN_PASSWORD`` is never required after setup and
    is not used as a parallel live password once a store hash exists.
    """
    if not verify_admin_password(current or ""):
        raise ValueError("当前密码不正确")
    if not new_password or len(new_password) < 4:
        raise ValueError("新密码至少 4 位")
    if current == new_password:
        raise ValueError("新密码不能与当前密码相同")
    set_admin_password(new_password)


def verify_admin_password(password: str) -> bool:
    if not password:
        return False
    # Best-effort seed from env on first boot before any hash exists.
    try:
        ensure_admin_password_seeded()
    except Exception:
        pass
    data = _load()
    salt = data.get("admin_password_salt")
    expected = data.get("admin_password_hash")
    if salt and expected:
        got = _hash_password(password, salt)
        if hmac.compare_digest(got, expected):
            return True
        # Store hash is authoritative: do not fall back to env once present.
        return False
    # No store hash yet: allow env only as emergency bootstrap login, then
    # callers (setup/change) should persist a hash.
    env_pw = (ADMIN_PASSWORD or "").strip()
    if env_pw and secrets.compare_digest(password, env_pw):
        return True
    return False


def _redis_admin_sessions() -> bool:
    try:
        from grok2api.store.redis_client import redis_enabled

        return redis_enabled()
    except Exception:
        return False


def create_session_token() -> str:
    token = secrets.token_urlsafe(32)
    if _redis_admin_sessions():
        try:
            from grok2api.store.sessions_redis import admin_session_put

            admin_session_put(token)
            return token
        except Exception:
            pass
    with _lock:
        data = _load()
        sessions = data.setdefault("sessions", {})
        now = time.time()
        sessions = {
            k: v
            for k, v in sessions.items()
            if isinstance(v, (int, float)) and now - float(v) < 7 * 86400
        }
        sessions[token] = now
        data["sessions"] = sessions
        _save(data, immediate=True)
    return token


def verify_session_token(token: str | None) -> bool:
    if not token:
        return False
    if _redis_admin_sessions():
        try:
            from grok2api.store.sessions_redis import admin_session_get, admin_session_touch

            if admin_session_get(token):
                admin_session_touch(token)
                return True
            # fall through: token may still live in settings.json (pre-migration)
        except Exception:
            pass
    with _lock:
        data = _load()
        sessions = data.get("sessions") or {}
        ts = sessions.get(token)
        if ts is None:
            return False
        if time.time() - float(ts) > 7 * 86400:
            sessions.pop(token, None)
            data["sessions"] = sessions
            _save(data, immediate=True)
            return False
        # sliding refresh — coalesce to avoid rewrite-per-request
        sessions[token] = time.time()
        data["sessions"] = sessions
        _save(data, immediate=False)
        return True


def revoke_session(token: str | None) -> None:
    if not token:
        return
    if _redis_admin_sessions():
        try:
            from grok2api.store.sessions_redis import admin_session_delete

            admin_session_delete(token)
        except Exception:
            pass
    with _lock:
        data = _load()
        sessions = data.get("sessions") or {}
        if token in sessions:
            sessions.pop(token, None)
            data["sessions"] = sessions
            _save(data, immediate=True)


def _normalize_mode(mode: str | None) -> str:
    mode = (mode or "").strip().lower()
    mode = _LEGACY_MODES.get(mode, mode)
    if mode not in VALID_ACCOUNT_MODES:
        return DEFAULT_ACCOUNT_MODE
    return mode


def get_account_mode() -> str:
    # Env override wins when set
    if ACCOUNT_MODE:
        return _normalize_mode(ACCOUNT_MODE)
    data = _load()
    return _normalize_mode(str(data.get("account_mode") or DEFAULT_ACCOUNT_MODE))


def set_account_mode(mode: str) -> str:
    raw = (mode or "").strip().lower()
    raw = _LEGACY_MODES.get(raw, raw)
    if raw not in VALID_ACCOUNT_MODES:
        raise ValueError(
            f"Invalid account_mode. Use one of: {', '.join(VALID_ACCOUNT_MODES)}"
        )
    mode = raw
    with _lock:
        data = _load()
        data["account_mode"] = mode
        # Drop legacy preferred-account setting if present
        data.pop("preferred_account_id", None)
        data["updated_at"] = time.time()
        _save(data, immediate=True)
    return mode


def get_account_pool_state() -> dict[str, Any]:
    """Load account pool status.

    PostgreSQL is the only durable source for account status/cooldown.
    JSON/file is no longer used as pool storage when DATABASE_URL is set.
    """
    pg = _pg_settings()
    if pg is not None:
        try:
            state = pg.get_account_pool_state()
            # Fast path: do NOT rewrite the whole settings JSON on every request.
            # PG already caches briefly; mutating local settings.json here made
            # every acquire() take a file lock and hurt TTFT under load.
            return dict(state) if isinstance(state, dict) else {}
        except Exception:
            pass
    data = _load()
    pool = data.get("account_pool") or {}
    return dict(pool) if isinstance(pool, dict) else {}


def get_account_pool_meta(account_id: str) -> dict[str, Any]:
    """Load durable pool meta for one account without full-pool scan when possible.

    Sticky multi-turn TTFT path uses this so a single affinity hit does not
    re-read all 1k+ account_pool rows.
    """
    if not account_id:
        return {}
    aid = str(account_id).strip()
    if not aid:
        return {}
    pg = _pg_settings()
    if pg is not None:
        try:
            if hasattr(pg, "get_pool_meta"):
                meta = pg.get_pool_meta(aid)
                return dict(meta) if isinstance(meta, dict) else {}
            # Older backends: fall back to many-ids helper if present.
            if hasattr(pg, "get_pool_meta_many"):
                m = pg.get_pool_meta_many([aid]).get(aid)
                return dict(m) if isinstance(m, dict) else {}
        except Exception:
            pass
    state = get_account_pool_state()
    meta = state.get(aid) if isinstance(state, dict) else None
    return dict(meta) if isinstance(meta, dict) else {}


def get_account_pool_meta_many(account_ids: list[str]) -> dict[str, Any]:
    """Batch durable pool meta for a candidate window (cold-path TTFT)."""
    ids = [str(x).strip() for x in (account_ids or []) if str(x).strip()]
    if not ids:
        return {}
    pg = _pg_settings()
    if pg is not None:
        try:
            if hasattr(pg, "get_pool_meta_many"):
                out = pg.get_pool_meta_many(ids)
                return dict(out) if isinstance(out, dict) else {}
        except Exception:
            pass
    state = get_account_pool_state()
    if not isinstance(state, dict):
        return {}
    return {
        aid: dict(state[aid])
        for aid in ids
        if isinstance(state.get(aid), dict)
    }


def get_cached_account_pool_state() -> dict[str, Any] | None:
    """Warm pool-state only; None when cache is cold (never rebuilds)."""
    pg = _pg_settings()
    if pg is not None:
        try:
            if hasattr(pg, "get_cached_account_pool_state"):
                cached = pg.get_cached_account_pool_state()
                return dict(cached) if isinstance(cached, dict) else None
        except Exception:
            return None
    return None


# Account rotation status fields that must hit PostgreSQL/file on every change.
# (Derived UI status: normal / cooldown / disabled / quota-disabled.)
_POOL_STATUS_KEYS = frozenset(
    {
        "enabled",
        "weight",
        "disabled_for_quota",
        "disabled_reason",
        "disabled_source",
        "quota_disabled_at",
        "quota_source",
        "cooldown_until",
        "cooldown_sec",
        "cooldown_count",
        "blocked_models",
        "last_error",
        "last_status_code",
        "consecutive_fails",
        "probe_fail_streak",
        "last_probe",
        "last_quota",
        "last_probe_ok_at",
        "last_probe_fail_at",
    }
)


def _patch_is_status(patch: dict[str, Any]) -> bool:
    return any(k in _POOL_STATUS_KEYS for k in (patch or {}))


def save_account_pool_state(state: dict[str, Any]) -> None:
    try:
        from grok2api.pool.account_pool import invalidate_pool_summary_cache
        invalidate_pool_summary_cache()
    except Exception:
        pass
    pg = _pg_settings()
    if pg is not None:
        pg.save_account_pool_state(state if isinstance(state, dict) else {})
        # keep in-process mem coherent
        with _lock:
            data = _load()
            data["account_pool"] = state
            data["updated_at"] = time.time()
        return
    try:
        from grok2api.config import STORE_BACKEND

        if str(STORE_BACKEND or "hybrid").strip().lower() != "file":
            raise RuntimeError(
                "PostgreSQL pool write failed in hybrid mode; refusing file "
                "fallback to prevent split-brain data"
            )
    except ImportError:
        pass
    with _lock:
        data = _load()
        data["account_pool"] = state
        data["updated_at"] = time.time()
        # Full pool rewrite is status-bearing → flush immediately when no PG.
        _save(data, immediate=True)


def patch_account_pool_meta(account_id: str, patch: dict[str, Any]) -> dict[str, Any]:
    """Update one account's pool meta.

    PostgreSQL path: every patch is committed immediately (field-level upsert).
    File path: status fields flush immediately; pure counter bumps may coalesce.
    """
    try:
        from grok2api.pool.account_pool import invalidate_pool_summary_cache
        invalidate_pool_summary_cache()
    except Exception:
        pass
    if not account_id:
        return {}
    if not isinstance(patch, dict) or not patch:
        return {}
    pg = _pg_settings()
    if pg is not None:
        try:
            meta = pg.patch_pool_meta(account_id, patch)
            with _lock:
                data = _load()
                pool = data.setdefault("account_pool", {})
                if isinstance(pool, dict):
                    pool[account_id] = meta
            return meta
        except Exception:
            # Fall through to file/memory so status is never silently dropped.
            pass
    with _lock:
        data = _load()
        pool = data.setdefault("account_pool", {})
        if not isinstance(pool, dict):
            pool = {}
            data["account_pool"] = pool
        meta = dict(pool.get(account_id) or {})
        for k, v in patch.items():
            if v is None:
                meta.pop(k, None)
            else:
                meta[k] = v
        # Derived status label for operators / debugging (always recompute).
        meta["pool_status"] = _derive_pool_status(meta)
        pool[account_id] = meta
        data["updated_at"] = time.time()
        # Every account status mutation flushes immediately (no delayed buffer).
        _save(data, immediate=True)
        return meta


def _derive_pool_status(meta: dict[str, Any]) -> str:
    """Canonical status string persisted with pool meta."""
    if not isinstance(meta, dict):
        return "normal"
    if meta.get("disabled_for_quota"):
        return "quota_disabled"
    if meta.get("enabled") is False:
        return "disabled"
    try:
        if int(meta.get("cooldown_count") or 0) > 0:
            return "cooldown"
    except (TypeError, ValueError):
        pass
    until = meta.get("cooldown_until")
    try:
        if until is not None and float(until) > time.time():
            return "cooldown"
    except (TypeError, ValueError):
        pass
    if str(meta.get("pool_status") or "").strip().lower() == "expired":
        return "expired"
    blocked = meta.get("blocked_models") or {}
    if isinstance(blocked, dict) and blocked:
        return "model_blocked"
    return "normal"


def touch_account_stats(
    account_id: str,
    *,
    success: bool = True,
    error: str = "",
    cooldown_until: float | None = None,
    clear_cooldown: bool = False,
    consecutive_fails: int | None = None,
    last_status_code: int | None = None,
    cooldown_sec: float | None = None,
    preserve_cooldown: bool = False,
    current_meta: dict[str, Any] | None = None,
) -> dict[str, Any] | None:
    """Update per-account pool stats + status.

    Source of truth for rotation status (normal / cooldown / disabled) is always
    PostgreSQL (or settings file when PG is off). Redis only mirrors hot counters
    and a short TTL copy of cooldown_until for multi-worker reads.

    preserve_cooldown=True (live request success): never clear/rewrite cooldown
    fields or force pool_status away from cooldown — only counters / last_used.
    current_meta lets hot paths reuse a single-account meta read instead of
    fetching the same row again before patching counters.
    """
    if not account_id:
        return None

    now = time.time()
    # When preserving cooldown on live success, never ask Redis to clear it.
    if preserve_cooldown:
        clear_cooldown = False

    durable_patch: dict[str, Any] = {
        "last_used_at": now,
    }
    if consecutive_fails is not None:
        durable_patch["consecutive_fails"] = int(consecutive_fails)
    elif success and not preserve_cooldown:
        durable_patch["consecutive_fails"] = 0
    elif success and preserve_cooldown:
        # Still reset fail streak counters, but do not touch cooldown fields.
        durable_patch["consecutive_fails"] = 0

    if success:
        # Do not wipe last_error while cooling — it explains why account is cooling.
        if not preserve_cooldown:
            durable_patch["last_error"] = None
        if clear_cooldown and not preserve_cooldown:
            durable_patch["cooldown_until"] = None
            durable_patch["cooldown_sec"] = None
    else:
        if error:
            durable_patch["last_error"] = error
        if last_status_code is not None:
            durable_patch["last_status_code"] = int(last_status_code)
        if cooldown_until is not None:
            durable_patch["cooldown_until"] = float(cooldown_until)
        if cooldown_sec is not None:
            durable_patch["cooldown_sec"] = float(cooldown_sec)

    # Mirror hot counters in Redis. When preserve_cooldown, do not clear redis CD
    # and do not overwrite redis cooldown key with None.
    try:
        from grok2api.store.pool_redis import touch_stats
        from grok2api.store.redis_client import redis_enabled

        if redis_enabled():
            touch_stats(
                account_id,
                success=success,
                error=("" if (success and preserve_cooldown) else error),
                cooldown_until=(None if preserve_cooldown else cooldown_until),
                clear_cooldown_flag=False if preserve_cooldown else clear_cooldown,
                consecutive_fails=consecutive_fails if consecutive_fails is not None else (0 if success else None),
                last_status_code=None if (success and preserve_cooldown) else last_status_code,
                cooldown_sec=None if preserve_cooldown else cooldown_sec,
            )
    except Exception:
        pass

    # Always persist counters to durable store NOW.
    pg = _pg_settings()
    if pg is not None:
        cur: dict[str, Any] = dict(current_meta or {}) if isinstance(current_meta, dict) else {}
        if not cur:
            try:
                from grok2api.store.settings_pg import get_pool_meta_many

                cur = (get_pool_meta_many([account_id]) or {}).get(account_id) or {}
            except Exception:
                try:
                    with _lock:
                        cur = dict((_load().get("account_pool") or {}).get(account_id) or {})
                except Exception:
                    cur = {}
        if not isinstance(cur, dict):
            cur = {}

        # If currently cooling and preserve_cooldown: only bump counters / last_used.
        cur_until = cur.get("cooldown_until")
        cur_cooling = False
        try:
            cur_cooling = cur_until is not None and float(cur_until) > now
        except (TypeError, ValueError):
            cur_cooling = False

        durable_patch["request_count"] = int(cur.get("request_count") or 0) + 1
        if success:
            durable_patch["success_count"] = int(cur.get("success_count") or 0) + 1
        else:
            durable_patch["fail_count"] = int(cur.get("fail_count") or 0) + 1

        if preserve_cooldown and (cur_cooling or success):
            # Explicitly keep durable cooldown / status; do not re-derive to normal.
            if cur_cooling:
                durable_patch.pop("last_error", None)  # keep existing reason
                # Do not set cooldown_until/sec in patch at all (leave row as-is).
                durable_patch["pool_status"] = "cooldown"
            else:
                merged_for_status = dict(cur)
                for k, v in durable_patch.items():
                    if v is None:
                        merged_for_status.pop(k, None)
                    else:
                        merged_for_status[k] = v
                durable_patch["pool_status"] = _derive_pool_status(merged_for_status)
            return patch_account_pool_meta(account_id, durable_patch)

        merged_for_status = dict(cur)
        for k, v in durable_patch.items():
            if v is None:
                merged_for_status.pop(k, None)
            else:
                merged_for_status[k] = v
        durable_patch["pool_status"] = _derive_pool_status(merged_for_status)
        return patch_account_pool_meta(account_id, durable_patch)

    # No PostgreSQL: update file-backed pool immediately for status fields.
    with _lock:
        data = _load()
        pool = data.setdefault("account_pool", {})
        if not isinstance(pool, dict):
            pool = {}
            data["account_pool"] = pool
        meta = dict(pool.get(account_id) or {}) if isinstance(pool.get(account_id), dict) else {}
        meta.setdefault("enabled", True)
        meta.setdefault("weight", 1)
        meta["request_count"] = int(meta.get("request_count") or 0) + 1
        meta["last_used_at"] = now
        cur_cooling = False
        try:
            cu = meta.get("cooldown_until")
            cur_cooling = cu is not None and float(cu) > now
        except (TypeError, ValueError):
            cur_cooling = False
        if success:
            meta["success_count"] = int(meta.get("success_count") or 0) + 1
            meta["consecutive_fails"] = 0
            if not preserve_cooldown:
                meta.pop("last_error", None)
            if clear_cooldown and not preserve_cooldown:
                meta.pop("cooldown_until", None)
                meta.pop("cooldown_sec", None)
        else:
            meta["fail_count"] = int(meta.get("fail_count") or 0) + 1
            if consecutive_fails is not None:
                meta["consecutive_fails"] = int(consecutive_fails)
            else:
                meta["consecutive_fails"] = int(meta.get("consecutive_fails") or 0) + 1
            if error:
                meta["last_error"] = error
            if last_status_code is not None:
                meta["last_status_code"] = int(last_status_code)
            if cooldown_until is not None:
                meta["cooldown_until"] = float(cooldown_until)
            if cooldown_sec is not None:
                meta["cooldown_sec"] = float(cooldown_sec)
        if preserve_cooldown and cur_cooling:
            meta["pool_status"] = "cooldown"
        else:
            meta["pool_status"] = _derive_pool_status(meta)
        pool[account_id] = meta
        data["updated_at"] = now
        _save(data, immediate=True)
        return meta


def _env_bool(name: str, default: bool = True) -> bool:
    raw = os.getenv(name)
    if raw is None or str(raw).strip() == "":
        return default
    return str(raw).strip().lower() not in ("0", "false", "no", "off")


def _get_feature_flag(key: str, env_name: str, default: bool = True) -> bool:
    """Runtime flag: settings store overrides env default."""
    data = _load()
    if key in data and data.get(key) is not None:
        return bool(data.get(key))
    return _env_bool(env_name, default)


def _set_feature_flag(key: str, enabled: bool) -> bool:
    enabled = bool(enabled)
    with _lock:
        data = _load()
        data[key] = enabled
        data["updated_at"] = time.time()
        _save(data, immediate=True)
    # also mirror into PG app_settings when available so multi-worker sees it
    pg = _pg_settings()
    if pg is not None:
        try:
            pg.set_setting(key, enabled)
        except Exception:
            pass
    return enabled


def get_token_maintain_enabled() -> bool:
    return _get_feature_flag("token_maintain_enabled", "GROK2API_TOKEN_MAINTAIN", True)


def set_token_maintain_enabled(enabled: bool) -> bool:
    val = _set_feature_flag("token_maintain_enabled", enabled)
    try:
        import grok2api.pool.token_maintainer as token_maintainer
        if val:
            # Prefer current leader; if this process can lead, start here.
            try:
                from grok2api.store.leader import is_leader, should_start_maintainers, try_become_leader
                can = is_leader() or should_start_maintainers() or try_become_leader()
            except Exception:
                can = True
            if can:
                token_maintainer.start_background()
                token_maintainer.request_run_soon(force=False)
            else:
                # Signal any leader via wakeup path: request_run_soon is local-only,
                # so also poke redis leader lock owner by writing a flag.
                try:
                    from grok2api.store.redis_client import key, redis_enabled, set_ex
                    if redis_enabled():
                        set_ex(key("flag", "token_maintain_on"), "1", 60)
                except Exception:
                    pass
        else:
            token_maintainer.stop_background()
            try:
                from grok2api.store.redis_client import delete, key, redis_enabled
                if redis_enabled():
                    delete(key("flag", "token_maintain_on"))
            except Exception:
                pass
    except Exception:
        pass
    return val


def get_model_health_enabled() -> bool:
    return _get_feature_flag("model_health_enabled", "GROK2API_MODEL_HEALTH", True)


def set_model_health_enabled(enabled: bool) -> bool:
    val = _set_feature_flag("model_health_enabled", enabled)
    try:
        import grok2api.pool.model_health as model_health
        if val:
            try:
                from grok2api.store.leader import is_leader, should_start_maintainers, try_become_leader
                can = is_leader() or should_start_maintainers() or try_become_leader()
            except Exception:
                can = True
            if can:
                model_health.start_background()
                model_health.request_run_soon()
            else:
                try:
                    from grok2api.store.redis_client import key, redis_enabled, set_ex
                    if redis_enabled():
                        set_ex(key("flag", "model_health_on"), "1", 60)
                except Exception:
                    pass
        else:
            model_health.stop_background()
            try:
                from grok2api.store.redis_client import delete, key, redis_enabled
                if redis_enabled():
                    delete(key("flag", "model_health_on"))
            except Exception:
                pass
    except Exception:
        pass
    return val


# ── runtime tunable settings (admin UI) ─────────────────────────────────────

_VALID_REASONING = ("off", "think_tag", "content")


def _get_setting_value(key: str, default: Any = None) -> Any:
    # Durable scalars: always prefer PostgreSQL so multi-worker reads see the
    # latest admin save (registration_config, account_mode, …).
    if key in _PG_SCALAR_KEYS:
        pg = _pg_settings()
        if pg is not None:
            try:
                fv = pg.get_setting(key)
            except Exception:
                fv = None
            if fv is not None:
                with _lock:
                    data = _load()
                    data[key] = fv
                return fv
            # Missing in PG → fall through to mem/default (first boot / env only).
    data = _load()
    if key in data and data.get(key) is not None:
        return data.get(key)
    return default


def _set_setting_value(key: str, value: Any) -> Any:
    # Write PostgreSQL first so other workers can observe the change immediately
    # via _get_setting_value / periodic refresh (do not rely on sticky mem alone).
    pg = _pg_settings()
    if pg is not None and key in _PG_SCALAR_KEYS:
        try:
            pg.set_setting(key, value)
        except Exception:
            # Still update local mem so this worker keeps working.
            pass
    with _lock:
        data = _load()
        data[key] = value
        data["updated_at"] = time.time()
        _save(data, immediate=True)
    # Non-scalar / non-PG keys still get a best-effort PG side write when enabled.
    if pg is not None and key not in _PG_SCALAR_KEYS:
        try:
            pg.set_setting(key, value)
        except Exception:
            pass
    return value


def get_reasoning_compat() -> str:
    """Effective reasoning_compat: settings override, else env/config."""
    raw = _get_setting_value("reasoning_compat", None)
    if raw is None or str(raw).strip() == "":
        try:
            from grok2api.config import REASONING_COMPAT

            raw = REASONING_COMPAT
        except Exception:
            raw = "off"
    mode = str(raw or "off").strip().lower()
    if mode not in _VALID_REASONING:
        return "off"
    return mode


def set_reasoning_compat(mode: str) -> str:
    m = (mode or "off").strip().lower()
    if m not in _VALID_REASONING:
        raise ValueError(f"reasoning_compat 必须是: {', '.join(_VALID_REASONING)}")
    _set_setting_value("reasoning_compat", m)
    # Hot-update config module so new requests pick it up without restart.
    try:
        import grok2api.config as _cfg

        _cfg.REASONING_COMPAT = m
    except Exception:
        pass
    return m


def get_outbound_max_tools() -> int:
    raw = _get_setting_value("outbound_max_tools", None)
    if raw is None:
        try:
            import grok2api.protocol.history_compact as hc

            return int(getattr(hc, "OUTBOUND_MAX_TOOLS", 1) or 0)
        except Exception:
            return 1
    try:
        v = int(raw)
    except (TypeError, ValueError):
        v = 1
    return max(0, min(64, v))


def set_outbound_max_tools(value: int | str) -> int:
    try:
        v = int(value)
    except (TypeError, ValueError) as e:
        raise ValueError("outbound_max_tools 必须是整数 0–64") from e
    v = max(0, min(64, v))
    _set_setting_value("outbound_max_tools", v)
    try:
        import grok2api.protocol.history_compact as hc

        hc.OUTBOUND_MAX_TOOLS = v
    except Exception:
        pass
    return v


def get_outbound_max_tools_openai() -> int:
    """Per-turn tool cap for OpenAI chat/completions (0 = unlimited)."""
    raw = _get_setting_value("outbound_max_tools_openai", None)
    if raw is None:
        try:
            import grok2api.protocol.history_compact as hc

            return int(getattr(hc, "OUTBOUND_MAX_TOOLS_OPENAI", 0) or 0)
        except Exception:
            return 0
    try:
        v = int(raw)
    except (TypeError, ValueError):
        v = 0
    return max(0, min(64, v))


def set_outbound_max_tools_openai(value: int | str) -> int:
    try:
        v = int(value)
    except (TypeError, ValueError) as e:
        raise ValueError("outbound_max_tools_openai 必须是整数 0–64") from e
    v = max(0, min(64, v))
    _set_setting_value("outbound_max_tools_openai", v)
    try:
        import grok2api.protocol.history_compact as hc

        hc.OUTBOUND_MAX_TOOLS_OPENAI = v
    except Exception:
        pass
    return v


def get_outbound_tool_gap_sec() -> float:
    raw = _get_setting_value("outbound_tool_gap_sec", None)
    if raw is None:
        try:
            import grok2api.protocol.history_compact as hc

            return float(getattr(hc, "OUTBOUND_TOOL_GAP_SEC", 0.08) or 0.0)
        except Exception:
            return 0.08
    try:
        v = float(raw)
    except (TypeError, ValueError):
        v = 0.08
    return max(0.0, min(2.0, v))


def set_outbound_tool_gap_sec(value: float | str) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError) as e:
        raise ValueError("outbound_tool_gap_sec 必须是数字 0–2") from e
    v = max(0.0, min(2.0, v))
    _set_setting_value("outbound_tool_gap_sec", v)
    try:
        import grok2api.protocol.history_compact as hc

        hc.OUTBOUND_TOOL_GAP_SEC = v
    except Exception:
        pass
    return v


def get_history_compact_enabled() -> bool:
    raw = _get_setting_value("history_compact_enabled", None)
    if raw is None:
        try:
            import grok2api.protocol.history_compact as hc

            return bool(getattr(hc, "HISTORY_COMPACT_ENABLED", False))
        except Exception:
            return False
    return bool(raw)


def set_history_compact_enabled(enabled: bool) -> bool:
    val = bool(enabled)
    _set_setting_value("history_compact_enabled", val)
    try:
        import grok2api.protocol.history_compact as hc

        hc.HISTORY_COMPACT_ENABLED = val
    except Exception:
        pass
    return val


def get_history_compact_auto_chars() -> int:
    """0 disables auto threshold; otherwise chars of messages JSON that trigger compact."""
    raw = _get_setting_value("history_compact_auto_chars", None)
    if raw is None:
        try:
            import grok2api.protocol.history_compact as hc

            return int(getattr(hc, "HISTORY_COMPACT_AUTO_CHARS", 0) or 0)
        except Exception:
            return 0  # IQ-first: no auto-force
    try:
        v = int(raw)
    except (TypeError, ValueError):
        v = 0
    # 0 = off; otherwise clamp to a sane band
    if v <= 0:
        return 0
    return max(4_000, min(5_000_000, v))


def set_history_compact_auto_chars(value: int | str) -> int:
    try:
        v = int(value)
    except (TypeError, ValueError) as e:
        raise ValueError("history_compact_auto_chars 必须是整数 0 或 4000–5000000") from e
    if v <= 0:
        v = 0
    else:
        v = max(4_000, min(5_000_000, v))
    _set_setting_value("history_compact_auto_chars", v)
    try:
        import grok2api.protocol.history_compact as hc

        hc.HISTORY_COMPACT_AUTO_CHARS = v
    except Exception:
        pass
    return v


def get_history_keep_tool_rounds() -> int:
    raw = _get_setting_value("history_keep_tool_rounds", None)
    if raw is None:
        # Accept legacy key from earlier UI draft
        raw = _get_setting_value("history_keep_recent_turns", None)
    if raw is None:
        try:
            import grok2api.protocol.history_compact as hc

            return int(getattr(hc, "HISTORY_KEEP_TOOL_ROUNDS", 32) or 32)
        except Exception:
            return 32
    try:
        v = int(raw)
    except (TypeError, ValueError):
        v = 32
    return max(1, min(64, v))


def set_history_keep_tool_rounds(value: int | str) -> int:
    try:
        v = int(value)
    except (TypeError, ValueError) as e:
        raise ValueError("history_keep_tool_rounds 必须是整数 1–64") from e
    v = max(1, min(64, v))
    _set_setting_value("history_keep_tool_rounds", v)
    try:
        import grok2api.protocol.history_compact as hc

        hc.HISTORY_KEEP_TOOL_ROUNDS = v
    except Exception:
        pass
    return v


# Back-compat aliases (older draft names)
def get_history_keep_recent_turns() -> int:
    return get_history_keep_tool_rounds()


def set_history_keep_recent_turns(value: int | str) -> int:
    return set_history_keep_tool_rounds(value)


def get_history_max_tool_result_chars() -> int:
    raw = _get_setting_value("history_max_tool_result_chars", None)
    if raw is None:
        raw = _get_setting_value("history_tool_result_max_chars", None)
    if raw is None:
        try:
            import grok2api.protocol.history_compact as hc

            return int(getattr(hc, "HISTORY_MAX_TOOL_RESULT_CHARS", 48_000) or 48_000)
        except Exception:
            return 48_000
    try:
        v = int(raw)
    except (TypeError, ValueError):
        v = 48_000
    return max(512, min(2_000_000, v))


def set_history_max_tool_result_chars(value: int | str) -> int:
    try:
        v = int(value)
    except (TypeError, ValueError) as e:
        raise ValueError("history_max_tool_result_chars 必须是整数 512–2000000") from e
    v = max(512, min(2_000_000, v))
    _set_setting_value("history_max_tool_result_chars", v)
    try:
        import grok2api.protocol.history_compact as hc

        hc.HISTORY_MAX_TOOL_RESULT_CHARS = v
    except Exception:
        pass
    return v


def get_history_tool_result_max_chars() -> int:
    return get_history_max_tool_result_chars()


def set_history_tool_result_max_chars(value: int | str) -> int:
    return set_history_max_tool_result_chars(value)


def get_sse_keepalive() -> float:
    raw = _get_setting_value("sse_keepalive", None)
    if raw is None:
        try:
            from grok2api.config import SSE_KEEPALIVE_INTERVAL

            return float(SSE_KEEPALIVE_INTERVAL or 8.0)
        except Exception:
            return 8.0
    try:
        v = float(raw)
    except (TypeError, ValueError):
        v = 8.0
    return max(2.0, min(120.0, v))


def set_sse_keepalive(value: float | str) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError) as e:
        raise ValueError("sse_keepalive 必须是数字 2–120") from e
    v = max(2.0, min(120.0, v))
    _set_setting_value("sse_keepalive", v)
    try:
        import grok2api.config as _cfg

        _cfg.SSE_KEEPALIVE_INTERVAL = v
    except Exception:
        pass
    return v


def get_conversation_affinity_enabled() -> bool:
    raw = _get_setting_value("conversation_affinity_enabled", None)
    if raw is None:
        try:
            return bool(
                __import__("os")
                .getenv("GROK2API_CONVERSATION_AFFINITY", "1")
                .lower()
                not in ("0", "false", "no", "off")
            )
        except Exception:
            return True
    return bool(raw)


def set_conversation_affinity_enabled(enabled: bool) -> bool:
    val = bool(enabled)
    _set_setting_value("conversation_affinity_enabled", val)
    # conversation_affinity reads env each call in some paths; also set env
    # for process-local modules that cache on import.
    try:
        import os

        os.environ["GROK2API_CONVERSATION_AFFINITY"] = "1" if val else "0"
    except Exception:
        pass
    try:
        import grok2api.config as _cfg

        _cfg.CONVERSATION_AFFINITY = val
    except Exception:
        pass
    try:
        import grok2api.pool.conversation_affinity as ca

        if hasattr(ca, "_enabled_cache"):
            ca._enabled_cache = None  # type: ignore[attr-defined]
    except Exception:
        pass
    return val


def get_conversation_affinity_ttl_sec() -> float:
    raw = _get_setting_value("conversation_affinity_ttl_sec", None)
    if raw is None:
        try:
            from grok2api.config import AFFINITY_TTL

            return float(AFFINITY_TTL or 7200.0)
        except Exception:
            return 7200.0
    try:
        v = float(raw)
    except (TypeError, ValueError):
        v = 7200.0
    return max(60.0, min(86_400.0, v))


def set_conversation_affinity_ttl_sec(value: float | str) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError) as e:
        raise ValueError("conversation_affinity_ttl_sec 必须是数字 60–86400") from e
    v = max(60.0, min(86_400.0, v))
    _set_setting_value("conversation_affinity_ttl_sec", v)
    try:
        import grok2api.config as _cfg

        _cfg.AFFINITY_TTL = v
    except Exception:
        pass
    return v


def get_token_maintain_interval_sec() -> float:
    raw = _get_setting_value("token_maintain_interval_sec", None)
    if raw is None:
        try:
            from grok2api.config import TOKEN_MAINTAIN_INTERVAL

            return float(TOKEN_MAINTAIN_INTERVAL or 90.0)
        except Exception:
            return 90.0
    try:
        v = float(raw)
    except (TypeError, ValueError):
        v = 90.0
    return max(30.0, min(3_600.0, v))


def set_token_maintain_interval_sec(value: float | str) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError) as e:
        raise ValueError("token_maintain_interval_sec 必须是数字 30–3600") from e
    v = max(30.0, min(3_600.0, v))
    _set_setting_value("token_maintain_interval_sec", v)
    try:
        import grok2api.config as _cfg

        _cfg.TOKEN_MAINTAIN_INTERVAL = v
    except Exception:
        pass
    try:
        import grok2api.pool.token_maintainer as token_maintainer

        if hasattr(token_maintainer, "request_run_soon"):
            # Nudge loop so a shorter interval takes effect without waiting full cycle.
            token_maintainer.request_run_soon(force=False)
    except Exception:
        pass
    return v


def get_token_refresh_skew_sec() -> float:
    raw = _get_setting_value("token_refresh_skew_sec", None)
    if raw is None:
        try:
            from grok2api.config import TOKEN_REFRESH_SKEW

            return float(TOKEN_REFRESH_SKEW or 120.0)
        except Exception:
            return 120.0
    try:
        v = float(raw)
    except (TypeError, ValueError):
        v = 120.0
    return max(30.0, min(1_800.0, v))


def set_token_refresh_skew_sec(value: float | str) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError) as e:
        raise ValueError("token_refresh_skew_sec 必须是数字 30–1800") from e
    v = max(30.0, min(1_800.0, v))
    _set_setting_value("token_refresh_skew_sec", v)
    try:
        import grok2api.config as _cfg

        _cfg.TOKEN_REFRESH_SKEW = v
    except Exception:
        pass
    return v


def get_model_health_interval_sec() -> float:
    raw = _get_setting_value("model_health_interval_sec", None)
    if raw is None:
        try:
            from grok2api.config import MODEL_HEALTH_INTERVAL

            return float(MODEL_HEALTH_INTERVAL or 900.0)
        except Exception:
            return 900.0
    try:
        v = float(raw)
    except (TypeError, ValueError):
        v = 900.0
    # 0 = on-demand only (still allowed via env); UI min is 60 when enabling periodic.
    return max(0.0, min(86_400.0, v))


def set_model_health_interval_sec(value: float | str) -> float:
    try:
        v = float(value)
    except (TypeError, ValueError) as e:
        raise ValueError("model_health_interval_sec 必须是数字 0–86400") from e
    v = max(0.0, min(86_400.0, v))
    if 0 < v < 60:
        v = 60.0
    _set_setting_value("model_health_interval_sec", v)
    try:
        import grok2api.config as _cfg

        _cfg.MODEL_HEALTH_INTERVAL = v
    except Exception:
        pass
    try:
        import grok2api.pool.model_health as model_health

        if hasattr(model_health, "request_run_soon"):
            model_health.request_run_soon()
    except Exception:
        pass
    return v


def get_model_health_auto_disable() -> bool:
    raw = _get_setting_value("model_health_auto_disable", None)
    if raw is None:
        try:
            from grok2api.config import MODEL_HEALTH_AUTO_DISABLE

            return bool(MODEL_HEALTH_AUTO_DISABLE)
        except Exception:
            return True
    return bool(raw)


def set_model_health_auto_disable(enabled: bool) -> bool:
    val = bool(enabled)
    _set_setting_value("model_health_auto_disable", val)
    try:
        import grok2api.config as _cfg

        _cfg.MODEL_HEALTH_AUTO_DISABLE = val
    except Exception:
        pass
    return val


def _parse_probe_models(raw: Any) -> list[str]:
    if raw is None:
        return []
    if isinstance(raw, (list, tuple)):
        items = [str(x).strip() for x in raw if str(x).strip()]
    else:
        text = str(raw).replace(";", ",").replace("\n", ",").replace("\r", ",")
        items = [p.strip() for p in text.split(",") if p.strip()]
    # Dedup preserve order
    seen: set[str] = set()
    out: list[str] = []
    for m in items:
        if m in seen:
            continue
        seen.add(m)
        out.append(m)
    return out[:32]


def get_probe_models() -> list[str]:
    raw = _get_setting_value("probe_models", None)
    models = _parse_probe_models(raw)
    if models:
        return models
    try:
        from grok2api.config import PROBE_MODELS, DEFAULT_MODEL

        env_models = [str(m).strip() for m in (PROBE_MODELS or []) if str(m).strip()]
        if env_models:
            return env_models
        return [str(DEFAULT_MODEL or "grok-4.5")]
    except Exception:
        return ["grok-4.5"]


def set_probe_models(value: Any) -> list[str]:
    models = _parse_probe_models(value)
    if not models:
        try:
            from grok2api.config import DEFAULT_MODEL

            models = [str(DEFAULT_MODEL or "grok-4.5")]
        except Exception:
            models = ["grok-4.5"]
    # Persist as comma-separated for simple UI round-trip
    joined = ", ".join(models)
    _set_setting_value("probe_models", joined)
    try:
        import grok2api.config as _cfg

        _cfg.PROBE_MODELS = list(models)
    except Exception:
        pass
    return models


def get_default_model_setting() -> str:
    raw = _get_setting_value("default_model", None)
    if raw is None or str(raw).strip() == "":
        try:
            from grok2api.config import DEFAULT_MODEL

            return str(DEFAULT_MODEL or "grok-4.5")
        except Exception:
            return "grok-4.5"
    return str(raw).strip()


def set_default_model_setting(model: str) -> str:
    m = (model or "").strip()
    if not m:
        raise ValueError("default_model 不能为空")
    if len(m) > 128:
        raise ValueError("default_model 过长")
    _set_setting_value("default_model", m)
    try:
        import grok2api.config as _cfg

        _cfg.DEFAULT_MODEL = m
    except Exception:
        pass
    return m


def apply_runtime_settings_to_modules() -> None:
    """Push persisted settings into in-process modules (call on startup)."""
    # Admin password: seed durable store from env once if empty.
    try:
        seed = ensure_admin_password_seeded()
        if seed.get("seeded"):
            print("  admin password: seeded from GROK2API_ADMIN_PASSWORD → store")
        elif seed.get("has_password"):
            print("  admin password: loaded from store (DB/settings)")
        else:
            print("  admin password: not set (setup required)")
    except Exception as e:  # noqa: BLE001
        print(f"  admin password: seed/load skipped ({e})")
    try:
        set_reasoning_compat(get_reasoning_compat())
    except Exception:
        pass
    try:
        set_outbound_max_tools(get_outbound_max_tools())
    except Exception:
        pass
    try:
        set_outbound_max_tools_openai(get_outbound_max_tools_openai())
    except Exception:
        pass
    try:
        set_outbound_tool_gap_sec(get_outbound_tool_gap_sec())
    except Exception:
        pass
    try:
        set_history_compact_enabled(get_history_compact_enabled())
    except Exception:
        pass
    try:
        set_history_compact_auto_chars(get_history_compact_auto_chars())
    except Exception:
        pass
    try:
        set_history_keep_tool_rounds(get_history_keep_tool_rounds())
    except Exception:
        pass
    try:
        set_history_max_tool_result_chars(get_history_max_tool_result_chars())
    except Exception:
        pass
    try:
        set_sse_keepalive(get_sse_keepalive())
    except Exception:
        pass
    try:
        set_conversation_affinity_enabled(get_conversation_affinity_enabled())
    except Exception:
        pass
    try:
        set_conversation_affinity_ttl_sec(get_conversation_affinity_ttl_sec())
    except Exception:
        pass
    try:
        set_token_maintain_interval_sec(get_token_maintain_interval_sec())
    except Exception:
        pass
    try:
        set_token_refresh_skew_sec(get_token_refresh_skew_sec())
    except Exception:
        pass
    try:
        set_model_health_interval_sec(get_model_health_interval_sec())
    except Exception:
        pass
    try:
        set_model_health_auto_disable(get_model_health_auto_disable())
    except Exception:
        pass
    try:
        set_probe_models(get_probe_models())
    except Exception:
        pass
    try:
        set_default_model_setting(get_default_model_setting())
    except Exception:
        pass
    # Hydrate registration secrets (YesCaptcha / MoeMail / proxy) from DB into
    # process env so adapter modules that read env/config at call time work.
    try:
        apply_registration_config_to_runtime()
    except Exception:
        pass
    # Outbound proxy pool (account chat / probe / refresh).
    try:
        apply_outbound_proxy_config_to_runtime()
    except Exception:
        pass


# ── protocol registration config (MoeMail / YesCaptcha / proxy) ────────────

_REG_CONFIG_KEYS = (
    "mail_provider",
    "base_url",
    # Per-provider base URLs for self-hosted services (do not share one field).
    "moemail_base_url",
    "cfmail_base_url",
    # Active key (derived from selected provider). Kept for adapter/env compat.
    "api_key",
    # Per-provider secrets — all persist in DB so switching provider keeps keys.
    "moemail_api_key",
    "yyds_api_key",
    "gptmail_api_key",
    "cfmail_api_key",
    # Active domain + per-provider domains (same pattern as keys).
    "domain",
    "moemail_domain",
    "yyds_domain",
    "gptmail_domain",
    "cfmail_domain",
    "prefix",
    "expiry_ms",
    "captcha_provider",
    "local_solver_url",
    "yescaptcha_key",
    "proxy",
    "proxy_username",
    "proxy_password",
    "proxy_strategy",
    "count",
    "concurrency",
    "stagger_ms",
    "probe_delay_sec",
)

_REG_SECRET_KEYS = frozenset(
    {
        "api_key",
        "moemail_api_key",
        "yyds_api_key",
        "gptmail_api_key",
        "cfmail_api_key",
        "yescaptcha_key",
        "proxy_password",
    }
)

_MAIL_PROVIDER_KEY_FIELDS = {
    "moemail": "moemail_api_key",
    "yyds": "yyds_api_key",
    "gptmail": "gptmail_api_key",
    "cfmail": "cfmail_api_key",
}

_MAIL_PROVIDER_DOMAIN_FIELDS = {
    "moemail": "moemail_domain",
    "yyds": "yyds_domain",
    "gptmail": "gptmail_domain",
    "cfmail": "cfmail_domain",
}

# Self-hosted providers keep independent base URLs so switching never overwrites
# another provider's host (MoeMail vs Cloudflare Temp Email).
_MAIL_PROVIDER_BASE_URL_FIELDS = {
    "moemail": "moemail_base_url",
    "cfmail": "cfmail_base_url",
}


def _mask_secret(value: str | None) -> str:
    s = (value or "").strip()
    if not s:
        return ""
    if len(s) <= 8:
        return "****"
    return f"{s[:4]}…{s[-4:]}"


def _env_registration_defaults() -> dict[str, Any]:
    """Build defaults from env / config (non-secret fields always; secrets only as presence)."""
    out: dict[str, Any] = {}
    try:
        from grok2api.config import (
            MOEMAIL_API_KEY,
            MOEMAIL_BASE_URL,
            MOEMAIL_DOMAIN,
            MOEMAIL_EXPIRY_MS,
            XAI_PROXY,
            XAI_PROXY_PASSWORD,
            XAI_PROXY_USERNAME,
        )

        if MOEMAIL_BASE_URL:
            # Env base URL only seeds MoeMail — never bleed into CF Temp Email.
            out["moemail_base_url"] = str(MOEMAIL_BASE_URL)
            out["base_url"] = str(MOEMAIL_BASE_URL)
        if MOEMAIL_DOMAIN:
            # Env domain only seeds MoeMail — never bleed into YYDS/GPTMail/CF.
            out["moemail_domain"] = str(MOEMAIL_DOMAIN)
        if MOEMAIL_EXPIRY_MS is not None:
            out["expiry_ms"] = int(MOEMAIL_EXPIRY_MS)
        if MOEMAIL_API_KEY:
            # Legacy single-key env still seeds the active + moemail slot.
            out["api_key"] = str(MOEMAIL_API_KEY)
            out["moemail_api_key"] = str(MOEMAIL_API_KEY)
        # Optional dedicated env overrides for other providers.
        yyds_key = (
            os.environ.get("GROK2API_YYDS_API_KEY")
            or os.environ.get("YYDS_API_KEY")
            or ""
        ).strip()
        if yyds_key:
            out["yyds_api_key"] = yyds_key
        gpt_key = (
            os.environ.get("GROK2API_GPTMAIL_API_KEY")
            or os.environ.get("GPTMAIL_API_KEY")
            or ""
        ).strip()
        if gpt_key:
            out["gptmail_api_key"] = gpt_key
        cf_key = (
            os.environ.get("GROK2API_CFMAIL_API_KEY")
            or os.environ.get("CFMAIL_API_KEY")
            or os.environ.get("GROK2API_CF_TEMP_EMAIL_ADMIN")
            or ""
        ).strip()
        if cf_key:
            out["cfmail_api_key"] = cf_key
        yyds_dom = (
            os.environ.get("GROK2API_YYDS_DOMAIN")
            or os.environ.get("YYDS_DOMAIN")
            or ""
        ).strip().lstrip("@").strip(".")
        if yyds_dom:
            out["yyds_domain"] = yyds_dom
        gpt_dom = (
            os.environ.get("GROK2API_GPTMAIL_DOMAIN")
            or os.environ.get("GPTMAIL_DOMAIN")
            or ""
        ).strip().lstrip("@").strip(".")
        if gpt_dom:
            out["gptmail_domain"] = gpt_dom
        cf_dom = (
            os.environ.get("GROK2API_CFMAIL_DOMAIN")
            or os.environ.get("CFMAIL_DOMAIN")
            or ""
        ).strip().lstrip("@").strip(".")
        if cf_dom:
            out["cfmail_domain"] = cf_dom
        cf_base = (
            os.environ.get("GROK2API_CFMAIL_BASE_URL")
            or os.environ.get("CFMAIL_BASE_URL")
            or ""
        ).strip()
        if cf_base:
            # Dedicated CF host only — never overwrite MoeMail base_url.
            out["cfmail_base_url"] = cf_base
        # Prefer multi-line pool env when present; keep single-proxy fallback.
        pool_env = (
            os.environ.get("GROK2API_XAI_PROXY_POOL")
            or os.environ.get("GROK2API_PROXY_POOL")
            or ""
        ).strip()
        if pool_env:
            out["proxy"] = pool_env
        elif XAI_PROXY:
            out["proxy"] = str(XAI_PROXY)
        if XAI_PROXY_USERNAME:
            out["proxy_username"] = str(XAI_PROXY_USERNAME)
        if XAI_PROXY_PASSWORD:
            out["proxy_password"] = str(XAI_PROXY_PASSWORD)
        strat = (
            os.environ.get("GROK2API_XAI_PROXY_STRATEGY")
            or os.environ.get("GROK2API_PROXY_STRATEGY")
            or ""
        ).strip().lower()
        if strat:
            out["proxy_strategy"] = strat
        mail_provider = (
            os.environ.get("GROK2API_MAIL_PROVIDER")
            or os.environ.get("MAIL_PROVIDER")
            or ""
        ).strip().lower()
        if mail_provider:
            out["mail_provider"] = mail_provider
        elif MOEMAIL_BASE_URL:
            try:
                from grok2api.upstream.moemail import normalize_mail_provider

                out["mail_provider"] = normalize_mail_provider(
                    None, base_url=str(MOEMAIL_BASE_URL)
                )
            except Exception:
                out["mail_provider"] = "moemail"
    except Exception:
        pass
    yes = (
        os.environ.get("GROK2API_YESCAPTCHA_KEY")
        or os.environ.get("YESCAPTCHA_API_KEY")
        or ""
    ).strip()
    if yes:
        out["yescaptcha_key"] = yes
    captcha_provider = (
        os.environ.get("GROK2API_CAPTCHA_PROVIDER")
        or os.environ.get("CAPTCHA_PROVIDER")
        or ""
    ).strip().lower()
    if captcha_provider in {"local", "yescaptcha"}:
        out["captcha_provider"] = captcha_provider
    local_solver_url = (
        os.environ.get("GROK2API_LOCAL_SOLVER_URL")
        or os.environ.get("LOCAL_SOLVER_URL")
        or os.environ.get("GROK2API_YESCAPTCHA_ENDPOINT")
        or os.environ.get("YESCAPTCHA_ENDPOINT")
        or ""
    ).strip()
    if local_solver_url:
        out["local_solver_url"] = local_solver_url.rstrip("/")
    return out


def _normalize_registration_config(
    raw: dict[str, Any] | None,
    *,
    merge_env: bool = True,
) -> dict[str, Any]:
    """Sanitize registration config; optionally fill missing fields from env."""
    cfg: dict[str, Any] = {}
    src = raw if isinstance(raw, dict) else {}
    env = _env_registration_defaults() if merge_env else {}

    def _pick_str(key: str, max_len: int = 512, *, allow_env: bool = True) -> str:
        # Prefer explicit source values, including empty string (means cleared).
        if key in src and src.get(key) is not None:
            s = str(src.get(key) or "").strip()
            return s[:max_len]
        if allow_env:
            s = str(env.get(key, "") or "").strip()
            return s[:max_len]
        return ""

    def _pick_domain(key: str) -> str:
        # Domain slots: empty string is a real value (auto domain). Only fall
        # back to env when the key is completely absent from src.
        if key in src:
            return str(src.get(key) or "").strip().lstrip("@").strip(".")[:128]
        return str(env.get(key, "") or "").strip().lstrip("@").strip(".")[:128]

    legacy_base_url = _pick_str("base_url", 256)
    cfg["moemail_base_url"] = _pick_str("moemail_base_url", 256)
    cfg["cfmail_base_url"] = _pick_str("cfmail_base_url", 256)
    legacy_api_key = _pick_str("api_key", 512)
    cfg["moemail_api_key"] = _pick_str("moemail_api_key", 512)
    cfg["yyds_api_key"] = _pick_str("yyds_api_key", 512)
    cfg["gptmail_api_key"] = _pick_str("gptmail_api_key", 512)
    cfg["cfmail_api_key"] = _pick_str("cfmail_api_key", 512)
    # Do NOT env-fill legacy domain into every provider — only use explicit src.
    if "domain" in src:
        legacy_domain = str(src.get("domain") or "").strip().lstrip("@").strip(".")[:128]
    else:
        legacy_domain = ""
    cfg["moemail_domain"] = _pick_domain("moemail_domain")
    cfg["yyds_domain"] = _pick_domain("yyds_domain")
    cfg["gptmail_domain"] = _pick_domain("gptmail_domain")
    cfg["cfmail_domain"] = _pick_domain("cfmail_domain")
    cfg["prefix"] = _pick_str("prefix", 64)
    try:
        from grok2api.upstream.moemail import (
            normalize_cfmail_base_url,
            normalize_gptmail_base_url,
            normalize_mail_provider,
            normalize_yyds_base_url,
        )
    except Exception:
        normalize_mail_provider = None  # type: ignore[assignment]
        normalize_yyds_base_url = None  # type: ignore[assignment]
        normalize_gptmail_base_url = None  # type: ignore[assignment]
        normalize_cfmail_base_url = None  # type: ignore[assignment]

    mail_raw = _pick_str("mail_provider", 32).lower()
    # Prefer explicit mail_provider; only use base_url as a hint when empty.
    if normalize_mail_provider is not None:
        cfg["mail_provider"] = normalize_mail_provider(
            mail_raw or None,
            base_url=legacy_base_url
            or cfg.get("cfmail_base_url")
            or cfg.get("moemail_base_url")
            or None,
        )
    else:
        if mail_raw in {"yyds", "yydsmail"}:
            cfg["mail_provider"] = "yyds"
        elif mail_raw in {"gptmail", "gpt-mail", "chatgptmail"}:
            cfg["mail_provider"] = "gptmail"
        elif mail_raw in {"cfmail", "cloudflare", "cloudflare_temp_email", "awsl"}:
            cfg["mail_provider"] = "cfmail"
        else:
            cfg["mail_provider"] = (
                mail_raw
                if mail_raw in {"moemail", "yyds", "gptmail", "cfmail"}
                else "moemail"
            )

    # Migrate legacy single api_key into the selected provider slot when empty.
    if legacy_api_key:
        slot = _MAIL_PROVIDER_KEY_FIELDS.get(cfg["mail_provider"], "moemail_api_key")
        if not cfg.get(slot):
            cfg[slot] = legacy_api_key
        # Also seed moemail if nothing else was stored yet (oldest installs).
        if not cfg.get("moemail_api_key") and cfg["mail_provider"] == "moemail":
            cfg["moemail_api_key"] = legacy_api_key

    # Migrate legacy single domain into the selected provider slot when empty.
    # Only when the active provider slot is absent/empty AND legacy domain was
    # explicitly provided for that same save — never copy env domain across.
    if legacy_domain:
        dslot = _MAIL_PROVIDER_DOMAIN_FIELDS.get(
            cfg["mail_provider"], "moemail_domain"
        )
        if dslot not in src or not str(src.get(dslot) or "").strip():
            if not cfg.get(dslot):
                cfg[dslot] = legacy_domain
        if cfg["mail_provider"] == "moemail" and not cfg.get("moemail_domain"):
            cfg["moemail_domain"] = legacy_domain

    # Migrate legacy single base_url into the selected self-hosted provider slot.
    # Never copy CF host into MoeMail or vice-versa.
    if legacy_base_url:
        bslot = _MAIL_PROVIDER_BASE_URL_FIELDS.get(cfg["mail_provider"])
        if bslot and not cfg.get(bslot):
            # Only seed when the dedicated slot was absent/empty.
            if bslot not in src or not str(src.get(bslot) or "").strip():
                cfg[bslot] = legacy_base_url
        if cfg["mail_provider"] == "moemail" and not cfg.get("moemail_base_url"):
            cfg["moemail_base_url"] = legacy_base_url

    # Active key always mirrors the selected provider (adapter reads api_key).
    active_slot = _MAIL_PROVIDER_KEY_FIELDS.get(
        cfg["mail_provider"], "moemail_api_key"
    )
    cfg["api_key"] = str(cfg.get(active_slot) or "").strip()

    # Active domain mirrors the selected provider (adapter reads domain).
    active_dom_slot = _MAIL_PROVIDER_DOMAIN_FIELDS.get(
        cfg["mail_provider"], "moemail_domain"
    )
    cfg["domain"] = str(cfg.get(active_dom_slot) or "").strip().lstrip("@").strip(".")

    # Per-provider base URL normalization. Active base_url always mirrors the
    # selected self-hosted provider so adapter/env keep working.
    if normalize_cfmail_base_url is not None and cfg.get("cfmail_base_url"):
        cfg["cfmail_base_url"] = normalize_cfmail_base_url(cfg["cfmail_base_url"])
    if cfg.get("moemail_base_url"):
        cfg["moemail_base_url"] = str(cfg["moemail_base_url"]).strip().rstrip("/")

    # YYDS / GPTMail use fixed official hosts — no user URL required.
    # CF Temp Email / MoeMail are self-hosted and keep independent slots.
    if cfg["mail_provider"] == "yyds":
        cfg["base_url"] = (
            normalize_yyds_base_url(None)
            if normalize_yyds_base_url is not None
            else "https://maliapi.215.im"
        )
    elif cfg["mail_provider"] == "gptmail":
        cfg["base_url"] = (
            normalize_gptmail_base_url(None)
            if normalize_gptmail_base_url is not None
            else "https://mail.chatgpt.org.uk"
        )
    elif cfg["mail_provider"] == "cfmail":
        raw_base = cfg.get("cfmail_base_url") or ""
        cfg["cfmail_base_url"] = (
            normalize_cfmail_base_url(str(raw_base) if raw_base else None)
            if normalize_cfmail_base_url is not None
            else (str(raw_base).strip() or "https://temp-email-api.awsl.uk")
        )
        cfg["base_url"] = cfg["cfmail_base_url"]
    else:
        # MoeMail: active base_url mirrors moemail_base_url.
        cfg["base_url"] = str(cfg.get("moemail_base_url") or "").strip()
        cfg["moemail_base_url"] = cfg["base_url"]

    provider_raw = _pick_str("captcha_provider", 32).lower()
    if provider_raw not in {"local", "yescaptcha"}:
        # Prefer local when a local solver URL is configured; otherwise YesCaptcha.
        has_local = bool(
            str(src.get("local_solver_url") or env.get("local_solver_url") or "").strip()
        )
        has_yes = bool(
            str(src.get("yescaptcha_key") or env.get("yescaptcha_key") or "").strip()
        )
        provider_raw = "local" if has_local or not has_yes else "yescaptcha"
    cfg["captcha_provider"] = provider_raw
    # Local is always inline; YesCaptcha must not carry local URL.
    if provider_raw == "local":
        cfg["local_solver_url"] = "http://127.0.0.1:5072"
    else:
        cfg["local_solver_url"] = ""
    cfg["yescaptcha_key"] = _pick_str("yescaptcha_key", 512)
    # Proxy field accepts a single URL or a multi-line proxy pool.
    # Cap is large enough for residential lists (one proxy per line).
    cfg["proxy"] = _pick_str("proxy", 64_000)
    cfg["proxy_username"] = _pick_str("proxy_username", 256)
    cfg["proxy_password"] = _pick_str("proxy_password", 512)
    strat_raw = _pick_str("proxy_strategy", 32).lower().replace("-", "_")
    if strat_raw in {"rr", "round", "roundrobin", "round_robin"}:
        cfg["proxy_strategy"] = "round_robin"
    elif strat_raw in {"rand", "random"}:
        cfg["proxy_strategy"] = "random"
    elif strat_raw in {"sticky", "first", "fixed"}:
        cfg["proxy_strategy"] = "sticky"
    else:
        cfg["proxy_strategy"] = "round_robin"

    # expiry_ms — MoeMail official presets; YYDS temp mail is ~24h (map to 1 day).
    expiry_raw = src.get("expiry_ms", env.get("expiry_ms", 3600000))
    try:
        expiry = int(expiry_raw)
    except (TypeError, ValueError):
        expiry = 3600000
    presets = {0, 3600000, 86400000, 259200000}
    if expiry not in presets:
        # nearest timed preset
        timed = (3600000, 86400000, 259200000)
        expiry = min(timed, key=lambda p: abs(p - expiry))
    if cfg["mail_provider"] in {"yyds", "gptmail"}:
        # YYDS / GPTMail temp inboxes auto-expire ~24h; permanent/3d not meaningful.
        if expiry in (0, 259200000):
            expiry = 86400000
    # CF Temp Email addresses are durable (JWT mailbox); keep user expiry for UI only.
    cfg["expiry_ms"] = expiry

    def _int_field(key: str, default: int, lo: int, hi: int) -> int:
        raw_v = src.get(key, default)
        try:
            v = int(raw_v)
        except (TypeError, ValueError):
            v = default
        return max(lo, min(hi, v))

    cfg["count"] = _int_field("count", 1, 1, 10_000)
    cfg["concurrency"] = _int_field("concurrency", 5, 1, 10)
    cfg["stagger_ms"] = _int_field("stagger_ms", 400, 0, 10_000)
    # New-account auto-probe settle window (seconds). 0 = probe immediately.
    # Prefer form/DB value; fall back to env GROK2API_REG_PROBE_DELAY_SEC.
    try:
        env_probe = int(float(os.environ.get("GROK2API_REG_PROBE_DELAY_SEC", "30") or 30))
    except (TypeError, ValueError):
        env_probe = 30
    probe_raw = src.get("probe_delay_sec", env.get("probe_delay_sec", env_probe))
    try:
        probe_delay = int(float(probe_raw))
    except (TypeError, ValueError):
        probe_delay = env_probe
    cfg["probe_delay_sec"] = max(0, min(600, probe_delay))
    return cfg


def get_registration_config(*, include_secrets: bool = True) -> dict[str, Any]:
    """Effective registration config: DB override merged with env defaults.

    Always reads ``registration_config`` through ``_get_setting_value`` which
    prefers PostgreSQL under hybrid multi-worker, so admin saves are visible
    on every worker without restart.
    """
    stored = _get_setting_value("registration_config", None)
    if not isinstance(stored, dict):
        stored = {}
    cfg = _normalize_registration_config(stored, merge_env=True)
    if include_secrets:
        return cfg
    public = dict(cfg)
    for k in _REG_SECRET_KEYS:
        if public.get(k):
            public[k] = _mask_secret(str(public[k]))
            public[f"{k}_set"] = True
        else:
            public[k] = ""
            public[f"{k}_set"] = False
    provider = str(cfg.get("captcha_provider") or "local").strip().lower()
    mail_provider = str(cfg.get("mail_provider") or "moemail").strip().lower()
    has_moemail = bool(cfg.get("moemail_api_key") or (
        cfg.get("api_key") if mail_provider == "moemail" else ""
    ))
    has_yyds = bool(cfg.get("yyds_api_key"))
    has_gpt = bool(cfg.get("gptmail_api_key"))
    has_cf = bool(cfg.get("cfmail_api_key"))
    has_active = bool(cfg.get("api_key"))
    public["configured"] = {
        "moemail": has_moemail,
        "yyds": has_yyds,
        "gptmail": has_gpt,
        "cfmail": has_cf,
        "mail": has_active or has_moemail or has_yyds or has_gpt or has_cf,
        "yescaptcha": bool(cfg.get("yescaptcha_key")),
        "local_solver": bool(cfg.get("local_solver_url")),
        "captcha": (
            bool(cfg.get("local_solver_url"))
            if provider == "local"
            else bool(cfg.get("yescaptcha_key"))
        ),
        "proxy": bool(cfg.get("proxy")),
    }
    public["captcha_provider"] = provider
    public["mail_provider"] = mail_provider
    public["proxy_strategy"] = str(cfg.get("proxy_strategy") or "round_robin")
    try:
        from grok2api.upstream.proxy_pool import pool_summary

        public["proxy_pool"] = pool_summary(
            str(cfg.get("proxy") or ""),
            username=str(cfg.get("proxy_username") or "") or None,
            password=str(cfg.get("proxy_password") or "") or None,
            strategy=str(cfg.get("proxy_strategy") or "round_robin"),
            fallback_env=False,
        )
    except Exception:
        public["proxy_pool"] = {
            "enabled": bool(cfg.get("proxy")),
            "count": 1 if cfg.get("proxy") else 0,
            "strategy": str(cfg.get("proxy_strategy") or "round_robin"),
            "preview": [],
        }
    # Fixed hosts — UI should not require URL for yyds/gptmail.
    public["mail_base_url_fixed"] = mail_provider in {"yyds", "gptmail"}
    return public


def _is_masked_secret(value: str | None) -> bool:
    s = "" if value is None else str(value).strip()
    if not s:
        return False
    return ("…" in s) or s == "****" or set(s) <= {"*"}


def set_registration_config(
    patch: dict[str, Any] | None,
    *,
    replace: bool = False,
) -> dict[str, Any]:
    """Persist registration config to DB/settings and apply to runtime.

    Secrets:
      - masked placeholder → keep previous
      - empty string on the *active* provider key → clear (user deleted + saved)
      - empty string on inactive provider keys → keep previous
      - non-empty → overwrite
    Domains:
      - empty on active provider → clear and keep empty in DB
      - empty on inactive provider slots → keep previous
    """
    if patch is not None and not isinstance(patch, dict):
        raise ValueError("registration_config must be an object")
    patch = dict(patch or {})

    current_stored = _get_setting_value("registration_config", None)
    if not isinstance(current_stored, dict):
        current_stored = {}

    if replace:
        base: dict[str, Any] = {}
    else:
        base = dict(current_stored)

    # Resolve selected provider early so we can treat inactive slots carefully.
    try:
        from grok2api.upstream.moemail import normalize_mail_provider as _nmp

        prov = _nmp(
            str(
                patch.get("mail_provider")
                or base.get("mail_provider")
                or current_stored.get("mail_provider")
                or "moemail"
            ),
            base_url=str(patch.get("base_url") or base.get("base_url") or ""),
        )
    except Exception:
        prov = str(
            patch.get("mail_provider") or base.get("mail_provider") or "moemail"
        ).strip().lower() or "moemail"
    active_key_slot = _MAIL_PROVIDER_KEY_FIELDS.get(prov, "moemail_api_key")
    active_dom_slot = _MAIL_PROVIDER_DOMAIN_FIELDS.get(prov, "moemail_domain")
    active_base_slot = _MAIL_PROVIDER_BASE_URL_FIELDS.get(prov)

    for key in _REG_CONFIG_KEYS:
        if key not in patch:
            continue
        val = patch.get(key)
        if key in _REG_SECRET_KEYS:
            s = "" if val is None else str(val).strip()
            # Masked UI value → keep previous secret.
            if _is_masked_secret(s):
                if key in current_stored and current_stored.get(key):
                    base[key] = current_stored[key]
                continue
            # Empty secret:
            # - active provider key / active api_key → clear (user deleted + saved)
            # - inactive provider keys → keep previous (field not shown/edited)
            if not s:
                is_active_secret = key in {"api_key", active_key_slot}
                if is_active_secret:
                    base[key] = ""
                elif key in current_stored and current_stored.get(key):
                    base[key] = current_stored[key]
                else:
                    base[key] = ""
                continue
            base[key] = s
            continue
        if val is None:
            base.pop(key, None)
            continue
        # Per-provider domain slots for *inactive* providers: empty string means
        # "not edited this save", keep previous DB value (don't wipe).
        if (
            key in _MAIL_PROVIDER_DOMAIN_FIELDS.values()
            and key != active_dom_slot
            and isinstance(val, str)
            and not val.strip()
        ):
            if key in current_stored:
                base[key] = current_stored[key]
            continue
        # Same for inactive self-hosted base URL slots.
        if (
            key in _MAIL_PROVIDER_BASE_URL_FIELDS.values()
            and key != active_base_slot
            and isinstance(val, str)
            and not val.strip()
        ):
            if key in current_stored:
                base[key] = current_stored[key]
            continue
        base[key] = val

    # Mirror active api_key ↔ provider slot.
    # Empty active key after explicit edit must clear the provider slot too.
    if "api_key" in patch and not _is_masked_secret(patch.get("api_key")):
        active = str(patch.get("api_key") or "").strip()
        base["api_key"] = active
        if active_key_slot not in patch or not str(patch.get(active_key_slot) or "").strip():
            # UI edited the visible key field for this provider.
            base[active_key_slot] = active
        elif str(patch.get(active_key_slot) or "").strip():
            base[active_key_slot] = str(patch.get(active_key_slot) or "").strip()
    elif active_key_slot in patch and not _is_masked_secret(patch.get(active_key_slot)):
        base[active_key_slot] = str(patch.get(active_key_slot) or "").strip()
        base["api_key"] = base[active_key_slot]
    else:
        # No key edit this save — keep active key mirrored from slot.
        base["api_key"] = str(base.get(active_key_slot) or base.get("api_key") or "").strip()

    if "domain" in patch:
        # Explicit active domain edit for the current provider.
        # Empty string is allowed (means auto/random / cleared).
        active_dom = str(patch.get("domain") or "").strip().lstrip("@").strip(".")
        base["domain"] = active_dom
        base[active_dom_slot] = active_dom
    elif active_dom_slot in patch:
        active_dom = str(patch.get(active_dom_slot) or "").strip().lstrip("@").strip(".")
        base[active_dom_slot] = active_dom
        base["domain"] = active_dom
    else:
        base["domain"] = str(
            base.get(active_dom_slot) or base.get("domain") or ""
        ).strip().lstrip("@").strip(".")

    # Mirror active base_url ↔ selected self-hosted provider slot only.
    # Never write CF host into moemail_base_url or vice-versa.
    if active_base_slot:
        if "base_url" in patch:
            active_base = str(patch.get("base_url") or "").strip().rstrip("/")
            base["base_url"] = active_base
            if active_base_slot not in patch or not str(
                patch.get(active_base_slot) or ""
            ).strip():
                base[active_base_slot] = active_base
            else:
                base[active_base_slot] = str(
                    patch.get(active_base_slot) or ""
                ).strip().rstrip("/")
                base["base_url"] = base[active_base_slot]
        elif active_base_slot in patch:
            base[active_base_slot] = str(
                patch.get(active_base_slot) or ""
            ).strip().rstrip("/")
            base["base_url"] = base[active_base_slot]
        else:
            base["base_url"] = str(
                base.get(active_base_slot) or base.get("base_url") or ""
            ).strip().rstrip("/")
            base[active_base_slot] = base["base_url"]
    # Always keep inactive self-hosted host slots from previous DB when absent.
    for bslot in _MAIL_PROVIDER_BASE_URL_FIELDS.values():
        if bslot == active_base_slot:
            continue
        if bslot not in patch and bslot in current_stored:
            base[bslot] = current_stored.get(bslot) or ""

    cfg = _normalize_registration_config(base, merge_env=False)
    # Drop empty optional strings to keep the row small — except domains/keys/urls:
    # empty is a real "cleared" value and must stay in DB so env cannot revive it.
    keep_empty = {
        "expiry_ms",
        "domain",
        "moemail_domain",
        "yyds_domain",
        "gptmail_domain",
        "cfmail_domain",
        "api_key",
        "moemail_api_key",
        "yyds_api_key",
        "gptmail_api_key",
        "cfmail_api_key",
        "base_url",
        "moemail_base_url",
        "cfmail_base_url",
        # Proxy pool: empty means "cleared" so env cannot revive a deleted list.
        "proxy",
        "proxy_username",
        "proxy_password",
        "proxy_strategy",
    }
    cleaned = {
        k: v
        for k, v in cfg.items()
        if not (isinstance(v, str) and v == "" and k not in keep_empty)
    }
    # Always keep numeric defaults
    for k in ("expiry_ms", "count", "concurrency", "stagger_ms", "probe_delay_sec"):
        cleaned[k] = cfg[k]
    # Always persist active + per-provider domain slots (including empty).
    for k in ("domain", "moemail_domain", "yyds_domain", "gptmail_domain", "cfmail_domain"):
        cleaned[k] = str(cfg.get(k) or "").strip().lstrip("@").strip(".")
    # Always persist active + per-provider keys (including empty after clear).
    for k in ("api_key", "moemail_api_key", "yyds_api_key", "gptmail_api_key", "cfmail_api_key"):
        cleaned[k] = str(cfg.get(k) or "").strip()
    # Always persist active + per-provider base URLs (including empty after clear).
    for k in ("base_url", "moemail_base_url", "cfmail_base_url"):
        cleaned[k] = str(cfg.get(k) or "").strip().rstrip("/")

    _set_setting_value("registration_config", cleaned)
    apply_registration_config_to_runtime(cleaned)
    # Return the just-saved config as-is (do not re-merge env, which may still
    # hold stale keys for a brief moment / from process env).
    return dict(cleaned)


def apply_registration_config_to_runtime(cfg: dict[str, Any] | None = None) -> None:
    """Push registration secrets into env + config module for adapter use."""
    if cfg is None:
        cfg = get_registration_config(include_secrets=True)
    if not isinstance(cfg, dict):
        return

    def _set_env(name: str, value: str) -> None:
        if value:
            os.environ[name] = value
        else:
            os.environ.pop(name, None)

    mail_provider = str(cfg.get("mail_provider") or "moemail").strip().lower()
    if mail_provider not in {"moemail", "yyds", "gptmail", "cfmail"}:
        mail_provider = "moemail"
    # Prefer per-provider key; fall back to legacy api_key.
    slot = _MAIL_PROVIDER_KEY_FIELDS.get(mail_provider, "moemail_api_key")
    api_key = str(cfg.get(slot) or cfg.get("api_key") or "").strip()
    moe_base = str(cfg.get("moemail_base_url") or "").strip().rstrip("/")
    cf_base = str(cfg.get("cfmail_base_url") or "").strip().rstrip("/")
    if mail_provider == "yyds":
        base_url = "https://maliapi.215.im"
    elif mail_provider == "gptmail":
        base_url = "https://mail.chatgpt.org.uk"
    elif mail_provider == "cfmail":
        base_url = cf_base or str(cfg.get("base_url") or "").strip().rstrip("/")
    else:
        base_url = moe_base or str(cfg.get("base_url") or "").strip().rstrip("/")
    dslot = _MAIL_PROVIDER_DOMAIN_FIELDS.get(mail_provider, "moemail_domain")
    domain = str(cfg.get(dslot) or cfg.get("domain") or "").strip().lstrip("@").strip(".")
    provider = str(cfg.get("captcha_provider") or "local").strip().lower()
    if provider not in {"local", "yescaptcha"}:
        provider = "local"
    # Local captcha is always the in-container inline solver.
    local_solver_url = "http://127.0.0.1:5072" if provider == "local" else ""
    yes = str(cfg.get("yescaptcha_key") or "").strip()
    proxy = str(cfg.get("proxy") or "").strip()
    proxy_user = str(cfg.get("proxy_username") or "").strip()
    proxy_pass = str(cfg.get("proxy_password") or "").strip()
    proxy_strategy = str(cfg.get("proxy_strategy") or "round_robin").strip().lower()
    if proxy_strategy not in {"round_robin", "random", "sticky"}:
        proxy_strategy = "round_robin"

    _set_env("GROK2API_MAIL_PROVIDER", mail_provider)
    _set_env("MAIL_PROVIDER", mail_provider)
    # Active key used by helpers via MOEMAIL_API_KEY — clear when deleted.
    _set_env("GROK2API_MOEMAIL_API_KEY", api_key)
    _set_env("MOEMAIL_API_KEY", api_key)
    # Dedicated env mirrors (clear when empty so get_registration_config
    # cannot re-hydrate a just-deleted key from process env).
    mkey = str(cfg.get("moemail_api_key") or "").strip()
    ykey = str(cfg.get("yyds_api_key") or "").strip()
    gkey = str(cfg.get("gptmail_api_key") or "").strip()
    ckey = str(cfg.get("cfmail_api_key") or "").strip()
    _set_env("GROK2API_MOEMAIL_ONLY_API_KEY", mkey)
    _set_env("GROK2API_YYDS_API_KEY", ykey)
    _set_env("YYDS_API_KEY", ykey)
    _set_env("GROK2API_GPTMAIL_API_KEY", gkey)
    _set_env("GPTMAIL_API_KEY", gkey)
    _set_env("GROK2API_CFMAIL_API_KEY", ckey)
    _set_env("CFMAIL_API_KEY", ckey)
    # Keep MoeMail / CF hosts in dedicated env slots — never overwrite one with
    # the other when switching the active provider.
    _set_env("GROK2API_MOEMAIL_BASE_URL", moe_base if mail_provider == "moemail" else moe_base)
    _set_env("GROK2API_CFMAIL_BASE_URL", cf_base)
    _set_env("CFMAIL_BASE_URL", cf_base)
    if base_url:
        # Active adapter host (helpers still read MOEMAIL_BASE_URL historically).
        _set_env("GROK2API_MOEMAIL_BASE_URL", base_url if mail_provider != "cfmail" else moe_base)
        if mail_provider == "cfmail":
            _set_env("GROK2API_CFMAIL_BASE_URL", base_url)
            _set_env("CFMAIL_BASE_URL", base_url)
            # Active helpers fall back to MOEMAIL_BASE_URL — mirror CF host only
            # while CF is selected so create/fetch hit the right Workers URL.
            _set_env("GROK2API_MOEMAIL_BASE_URL", base_url)
            _set_env("MOEMAIL_BASE_URL", base_url)
        elif mail_provider == "moemail":
            _set_env("MOEMAIL_BASE_URL", base_url)
    _set_env("GROK2API_MOEMAIL_DOMAIN", domain if mail_provider == "moemail" else str(cfg.get("moemail_domain") or "").strip())
    if mail_provider == "cfmail":
        _set_env("GROK2API_CFMAIL_DOMAIN", domain)
        _set_env("CFMAIL_DOMAIN", domain)
    else:
        # Keep dedicated CF domain env in sync with stored slot even when inactive.
        _set_env("GROK2API_CFMAIL_DOMAIN", str(cfg.get("cfmail_domain") or "").strip())
        _set_env("CFMAIL_DOMAIN", str(cfg.get("cfmail_domain") or "").strip())
    try:
        probe_delay = int(cfg.get("probe_delay_sec", 30))
    except (TypeError, ValueError):
        probe_delay = 30
    probe_delay = max(0, min(600, probe_delay))
    _set_env("GROK2API_REG_PROBE_DELAY_SEC", str(probe_delay))
    _set_env("GROK2API_CAPTCHA_PROVIDER", provider)
    _set_env("CAPTCHA_PROVIDER", provider)
    if provider == "local":
        # Local solver speaks createTask protocol; reuse YesCaptcha client with custom endpoint.
        _set_env("GROK2API_LOCAL_SOLVER_URL", local_solver_url)
        _set_env("LOCAL_SOLVER_URL", local_solver_url)
        _set_env("GROK2API_YESCAPTCHA_ENDPOINT", local_solver_url)
        _set_env("YESCAPTCHA_ENDPOINT", local_solver_url)
        # Local solver does not require a real key; keep a stable placeholder.
        local_key = "local"
        _set_env("GROK2API_YESCAPTCHA_KEY", local_key)
        _set_env("YESCAPTCHA_API_KEY", local_key)
        yes = local_key
    else:
        # YesCaptcha cloud mode must not inherit local solver endpoint/key.
        for k in (
            "GROK2API_LOCAL_SOLVER_URL",
            "LOCAL_SOLVER_URL",
            "GROK2API_YESCAPTCHA_ENDPOINT",
            "YESCAPTCHA_ENDPOINT",
            "YESCAPTCHA_API_BASE",
        ):
            os.environ.pop(k, None)
        if yes:
            _set_env("GROK2API_YESCAPTCHA_KEY", yes)
            _set_env("YESCAPTCHA_API_KEY", yes)
        else:
            os.environ.pop("GROK2API_YESCAPTCHA_KEY", None)
            os.environ.pop("YESCAPTCHA_API_KEY", None)
    # Mirror full multi-line text into both pool + legacy single-proxy envs.
    # Adapter / SSO helpers still read GROK2API_XAI_PROXY for first-proxy fallback.
    if proxy:
        _set_env("GROK2API_XAI_PROXY_POOL", proxy)
        # Keep first usable line in the classic single-proxy env for older paths.
        first_line = next(
            (ln.strip() for ln in proxy.replace("\r", "\n").split("\n") if ln.strip() and not ln.strip().startswith("#")),
            proxy,
        )
        _set_env("GROK2API_XAI_PROXY", first_line)
    else:
        os.environ.pop("GROK2API_XAI_PROXY_POOL", None)
        os.environ.pop("GROK2API_XAI_PROXY", None)
    if proxy_user:
        _set_env("GROK2API_XAI_PROXY_USERNAME", proxy_user)
    else:
        os.environ.pop("GROK2API_XAI_PROXY_USERNAME", None)
    if proxy_pass:
        _set_env("GROK2API_XAI_PROXY_PASSWORD", proxy_pass)
    else:
        os.environ.pop("GROK2API_XAI_PROXY_PASSWORD", None)
    _set_env("GROK2API_XAI_PROXY_STRATEGY", proxy_strategy)
    _set_env("GROK2API_PROXY_STRATEGY", proxy_strategy)
    if cfg.get("expiry_ms") is not None:
        try:
            _set_env("GROK2API_MOEMAIL_EXPIRY_MS", str(int(cfg["expiry_ms"])))
        except (TypeError, ValueError):
            pass

    try:
        import grok2api.config as _cfg

        # Always mirror active key/domain, including empty clears.
        _cfg.MOEMAIL_API_KEY = api_key
        if base_url:
            _cfg.MOEMAIL_BASE_URL = base_url
        _cfg.MOEMAIL_DOMAIN = domain
        if hasattr(_cfg, "MAIL_PROVIDER"):
            _cfg.MAIL_PROVIDER = mail_provider
        if cfg.get("expiry_ms") is not None:
            try:
                _cfg.MOEMAIL_EXPIRY_MS = int(cfg["expiry_ms"])
            except (TypeError, ValueError):
                pass
        if hasattr(_cfg, "XAI_PROXY_POOL"):
            _cfg.XAI_PROXY_POOL = proxy
        if hasattr(_cfg, "XAI_PROXY_STRATEGY"):
            _cfg.XAI_PROXY_STRATEGY = proxy_strategy
        # Classic single-proxy field: first non-empty line for back-compat.
        if proxy:
            first_line = next(
                (
                    ln.strip()
                    for ln in proxy.replace("\r", "\n").split("\n")
                    if ln.strip() and not ln.strip().startswith("#")
                ),
                proxy,
            )
            _cfg.XAI_PROXY = first_line
        else:
            _cfg.XAI_PROXY = ""
        _cfg.XAI_PROXY_USERNAME = proxy_user
        _cfg.XAI_PROXY_PASSWORD = proxy_pass
    except Exception:
        pass

    # Adapter caches YESCAPTCHA_KEY / probe delay at import — refresh module attrs.
    try:
        import grok2api.upstream.grok_build_adapter as gba

        if hasattr(gba, "CAPTCHA_PROVIDER"):
            gba.CAPTCHA_PROVIDER = provider
        if hasattr(gba, "MAIL_PROVIDER"):
            gba.MAIL_PROVIDER = mail_provider
        if hasattr(gba, "REGISTER_PROBE_DELAY_SEC"):
            gba.REGISTER_PROBE_DELAY_SEC = float(probe_delay)
        if provider == "local":
            gba.YESCAPTCHA_KEY = "local"
            if hasattr(gba, "LOCAL_SOLVER_URL"):
                gba.LOCAL_SOLVER_URL = "http://127.0.0.1:5072"
        else:
            gba.YESCAPTCHA_KEY = yes or ""
            if hasattr(gba, "LOCAL_SOLVER_URL"):
                gba.LOCAL_SOLVER_URL = ""
    except Exception:
        pass


def resolve_registration_inputs(
    overrides: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Merge request overrides on top of saved/env registration config.

    Empty string is meaningful for the *active* mail domain / key fields:
    it means the user cleared them. Do not silently fall back to the previous
    DB value for those active slots, or the admin UI will appear to "restore"
    a deleted YYDS/GPTMail domain after save/start.
    """
    base = get_registration_config(include_secrets=True)
    overrides = overrides if isinstance(overrides, dict) else {}
    merged = dict(base)

    # Resolve target provider early so empty active domain/key can clear.
    try:
        from grok2api.upstream.moemail import normalize_mail_provider as _nmp

        prov = _nmp(
            str(
                overrides.get("mail_provider")
                or base.get("mail_provider")
                or "moemail"
            ),
            base_url=str(overrides.get("base_url") or base.get("base_url") or ""),
        )
    except Exception:
        prov = str(
            overrides.get("mail_provider") or base.get("mail_provider") or "moemail"
        ).strip().lower() or "moemail"
    active_key_slot = _MAIL_PROVIDER_KEY_FIELDS.get(prov, "moemail_api_key")
    active_dom_slot = _MAIL_PROVIDER_DOMAIN_FIELDS.get(prov, "moemail_domain")
    clearable_empty = {
        "domain",
        active_dom_slot,
        "api_key",
        active_key_slot,
    }

    for key in _REG_CONFIG_KEYS:
        if key not in overrides:
            continue
        val = overrides.get(key)
        if val is None:
            continue
        if isinstance(val, str) and not val.strip():
            # Empty inactive fields: ignore (UI did not edit them).
            # Empty active domain/key: honor clear.
            if key in clearable_empty:
                merged[key] = ""
            continue
        if key in _REG_SECRET_KEYS and isinstance(val, str):
            s = val.strip()
            if "…" in s or s == "****" or set(s) <= {"*"}:
                continue
        merged[key] = val

    # Keep active domain/key mirrored onto the provider-specific slots.
    if "domain" in overrides:
        merged[active_dom_slot] = str(overrides.get("domain") or "").strip()
        merged["domain"] = merged[active_dom_slot]
    elif active_dom_slot in overrides:
        merged["domain"] = str(overrides.get(active_dom_slot) or "").strip()
        merged[active_dom_slot] = merged["domain"]

    if "api_key" in overrides and not (
        isinstance(overrides.get("api_key"), str)
        and ("…" in overrides.get("api_key") or overrides.get("api_key") == "****")
    ):
        merged[active_key_slot] = str(overrides.get("api_key") or "").strip()
        merged["api_key"] = merged[active_key_slot]
    elif active_key_slot in overrides and not (
        isinstance(overrides.get(active_key_slot), str)
        and (
            "…" in str(overrides.get(active_key_slot) or "")
            or str(overrides.get(active_key_slot) or "") == "****"
        )
    ):
        merged["api_key"] = str(overrides.get(active_key_slot) or "").strip()
        merged[active_key_slot] = merged["api_key"]

    # merge_env=False: already started from full DB+env base; re-merging env
    # would revive cleared active keys/domains from process env.
    return _normalize_registration_config(merged, merge_env=False)


# ── pool rotation / cooldown policy ────────────────────────────────────────

def get_pool_policy() -> dict[str, Any]:
    """Effective pool rotation / cooldown policy for admin UI + runtime."""
    try:
        import grok2api.pool.account_pool as ap

        cd = ap.cooldown_defaults()
        return {
            "cooldown_default_sec": cd["default"],
            "cooldown_auth_sec": cd["auth"],
            "cooldown_rate_limit_sec": cd["rate_limit"],
            "cooldown_server_error_sec": cd["server_error"],
            "cooldown_max_sec": cd["max"],
            "soft_model_block_ttl_sec": cd["soft_block_ttl"],
            "durable_model_block_ttl_sec": cd.get("durable_block_ttl", 3600.0),
            "probe_fail_kick_streak": int(cd.get("probe_fail_kick_streak") or 2),
            "probe_fail_disable_streak": int(cd.get("probe_fail_disable_streak") or 4),
            "probe_kick_cooldown_sec": cd.get("probe_kick_cooldown_sec", 600.0),
            "max_failover_attempts": ap.max_failover_attempts(),
        }
    except Exception:
        return {
            "cooldown_default_sec": 20,
            "cooldown_auth_sec": 90,
            "cooldown_rate_limit_sec": 45,
            "cooldown_server_error_sec": 20,
            "cooldown_max_sec": 600,
            "soft_model_block_ttl_sec": 180,
            "durable_model_block_ttl_sec": 3600,
            "probe_fail_kick_streak": 2,
            "probe_fail_disable_streak": 4,
            "probe_kick_cooldown_sec": 600,
            "max_failover_attempts": 8,
        }


def set_pool_policy(patch: dict[str, Any]) -> dict[str, Any]:
    if not isinstance(patch, dict):
        raise ValueError("pool policy must be an object")
    mapping = {
        "cooldown_default_sec": (1.0, 600.0),
        "cooldown_auth_sec": (5.0, 1800.0),
        "cooldown_rate_limit_sec": (5.0, 1800.0),
        "cooldown_server_error_sec": (1.0, 600.0),
        "cooldown_max_sec": (30.0, 3600.0),
        "soft_model_block_ttl_sec": (30.0, 3600.0),
        "durable_model_block_ttl_sec": (60.0, 86400.0),
        "probe_fail_kick_streak": (1.0, 20.0),
        "probe_fail_disable_streak": (2.0, 50.0),
        "probe_kick_cooldown_sec": (30.0, 7200.0),
        "max_failover_attempts": (1.0, 64.0),
    }
    for key, (lo, hi) in mapping.items():
        if key not in patch or patch[key] is None:
            continue
        try:
            val = float(patch[key])
        except (TypeError, ValueError) as e:
            raise ValueError(f"{key} 必须是数字") from e
        val = max(lo, min(hi, val))
        if key in (
            "max_failover_attempts",
            "probe_fail_kick_streak",
            "probe_fail_disable_streak",
        ):
            _set_setting_value(key, int(val))
        else:
            _set_setting_value(key, float(val))
    return get_pool_policy()


# ── outbound proxy pool (account chat / probe / refresh) ───────────────────


def _env_outbound_proxy_defaults() -> dict[str, Any]:
    out: dict[str, Any] = {"enabled": True}
    try:
        from grok2api.config import (
            XAI_PROXY,
            XAI_PROXY_PASSWORD,
            XAI_PROXY_POOL,
            XAI_PROXY_STRATEGY,
            XAI_PROXY_USERNAME,
        )
    except Exception:
        XAI_PROXY = ""
        XAI_PROXY_POOL = ""
        XAI_PROXY_USERNAME = ""
        XAI_PROXY_PASSWORD = ""
        XAI_PROXY_STRATEGY = "round_robin"
    pool = str(XAI_PROXY_POOL or XAI_PROXY or "").strip()
    if pool:
        out["proxy"] = pool
    if XAI_PROXY_USERNAME:
        out["proxy_username"] = str(XAI_PROXY_USERNAME)
    if XAI_PROXY_PASSWORD:
        out["proxy_password"] = str(XAI_PROXY_PASSWORD)
    if XAI_PROXY_STRATEGY:
        out["proxy_strategy"] = str(XAI_PROXY_STRATEGY)
    return out


def _normalize_outbound_proxy_config(
    raw: dict[str, Any] | None,
    *,
    merge_env: bool = True,
) -> dict[str, Any]:
    src = raw if isinstance(raw, dict) else {}
    env = _env_outbound_proxy_defaults() if merge_env else {}

    def _pick(key: str, max_len: int = 512) -> str:
        if key in src and src.get(key) is not None:
            return str(src.get(key) or "").strip()[:max_len]
        return str(env.get(key, "") or "").strip()[:max_len]

    cfg: dict[str, Any] = {}
    if "enabled" in src and src.get("enabled") is not None:
        cfg["enabled"] = bool(src.get("enabled"))
    else:
        cfg["enabled"] = bool(env.get("enabled", True))
    # Multi-line pool text — same cap as registration.
    cfg["proxy"] = _pick("proxy", 64_000)
    cfg["proxy_username"] = _pick("proxy_username", 256)
    cfg["proxy_password"] = _pick("proxy_password", 512)
    strat = _pick("proxy_strategy", 32).lower().replace("-", "_")
    if strat in {"rr", "round", "roundrobin", "round_robin"}:
        cfg["proxy_strategy"] = "round_robin"
    elif strat in {"rand", "random"}:
        cfg["proxy_strategy"] = "random"
    elif strat in {"sticky", "first", "fixed"}:
        cfg["proxy_strategy"] = "sticky"
    else:
        cfg["proxy_strategy"] = "round_robin"
    return cfg


def get_outbound_proxy_config(*, include_secrets: bool = True) -> dict[str, Any]:
    stored = _get_setting_value("outbound_proxy_config", None)
    if not isinstance(stored, dict):
        stored = {}
    cfg = _normalize_outbound_proxy_config(stored, merge_env=True)
    if include_secrets:
        return cfg
    public = dict(cfg)
    if public.get("proxy_password"):
        public["proxy_password"] = _mask_secret(str(public.get("proxy_password") or ""))
    return public


def set_outbound_proxy_config(
    patch: dict[str, Any] | None,
    *,
    replace: bool = False,
) -> dict[str, Any]:
    """Persist outbound proxy pool config and apply to runtime."""
    if patch is not None and not isinstance(patch, dict):
        raise ValueError("outbound_proxy_config must be an object")
    patch = dict(patch or {})
    current = _get_setting_value("outbound_proxy_config", None)
    if not isinstance(current, dict):
        current = {}
    base: dict[str, Any] = {} if replace else dict(current)

    if "enabled" in patch and patch.get("enabled") is not None:
        base["enabled"] = bool(patch.get("enabled"))

    for key in ("proxy", "proxy_username", "proxy_password", "proxy_strategy"):
        if key not in patch:
            continue
        val = patch.get(key)
        if val is None:
            continue
        s = str(val).strip() if not isinstance(val, bool) else str(val)
        if key == "proxy_password" and _is_masked_secret(s):
            if current.get("proxy_password"):
                base["proxy_password"] = current["proxy_password"]
            continue
        # Empty string is intentional clear for proxy text / auth.
        base[key] = s

    cfg = _normalize_outbound_proxy_config(base, merge_env=False)
    cleaned = {
        "enabled": bool(cfg.get("enabled", True)),
        "proxy": str(cfg.get("proxy") or "").strip(),
        "proxy_username": str(cfg.get("proxy_username") or "").strip(),
        "proxy_password": str(cfg.get("proxy_password") or "").strip(),
        "proxy_strategy": str(cfg.get("proxy_strategy") or "round_robin"),
    }
    _set_setting_value("outbound_proxy_config", cleaned)
    apply_outbound_proxy_config_to_runtime(cleaned)
    return dict(cleaned)


def apply_outbound_proxy_config_to_runtime(
    cfg: dict[str, Any] | None = None,
) -> None:
    """Push outbound proxy pool into env/config and drop cached HTTP clients."""
    if cfg is None:
        cfg = get_outbound_proxy_config(include_secrets=True)
    if not isinstance(cfg, dict):
        return

    def _set_env(name: str, value: str) -> None:
        if value:
            os.environ[name] = value
        else:
            os.environ.pop(name, None)

    enabled = bool(cfg.get("enabled", True))
    proxy = str(cfg.get("proxy") or "").strip() if enabled else ""
    proxy_user = str(cfg.get("proxy_username") or "").strip()
    proxy_pass = str(cfg.get("proxy_password") or "").strip()
    strategy = str(cfg.get("proxy_strategy") or "round_robin").strip().lower()
    if strategy not in {"round_robin", "random", "sticky"}:
        strategy = "round_robin"

    if proxy:
        _set_env("GROK2API_XAI_PROXY_POOL", proxy)
        first_line = next(
            (
                ln.strip()
                for ln in proxy.replace("\r", "\n").split("\n")
                if ln.strip() and not ln.strip().startswith("#")
            ),
            proxy,
        )
        _set_env("GROK2API_XAI_PROXY", first_line)
    else:
        # Only clear pool envs when admin explicitly disabled/cleared outbound
        # config — do not wipe registration proxy envs if they share the same keys
        # and outbound was never set. When enabled=False with empty text we still
        # clear so direct-connect is honoured.
        if "proxy" in cfg or not enabled:
            os.environ.pop("GROK2API_XAI_PROXY_POOL", None)
            # Keep single-proxy env if registration still owns it.
            try:
                reg = get_registration_config(include_secrets=True) or {}
                reg_proxy = str(reg.get("proxy") or "").strip()
            except Exception:
                reg_proxy = ""
            if not reg_proxy:
                os.environ.pop("GROK2API_XAI_PROXY", None)
    if proxy_user:
        _set_env("GROK2API_XAI_PROXY_USERNAME", proxy_user)
    if proxy_pass:
        _set_env("GROK2API_XAI_PROXY_PASSWORD", proxy_pass)
    _set_env("GROK2API_XAI_PROXY_STRATEGY", strategy)
    _set_env("GROK2API_PROXY_STRATEGY", strategy)

    try:
        import grok2api.config as _cfg

        if hasattr(_cfg, "XAI_PROXY_POOL"):
            _cfg.XAI_PROXY_POOL = proxy
        if hasattr(_cfg, "XAI_PROXY_STRATEGY"):
            _cfg.XAI_PROXY_STRATEGY = strategy
        if proxy:
            first_line = next(
                (
                    ln.strip()
                    for ln in proxy.replace("\r", "\n").split("\n")
                    if ln.strip() and not ln.strip().startswith("#")
                ),
                proxy,
            )
            _cfg.XAI_PROXY = first_line
        _cfg.XAI_PROXY_USERNAME = proxy_user
        _cfg.XAI_PROXY_PASSWORD = proxy_pass
    except Exception:
        pass

    # Drop process-local proxy/client caches so next request sees new config.
    try:
        from grok2api.upstream.proxy_pool import invalidate_outbound_proxy_cache

        invalidate_outbound_proxy_cache()
    except Exception:
        pass
    try:
        import app as _app

        if hasattr(_app, "invalidate_http_clients"):
            _app.invalidate_http_clients()
    except Exception:
        pass
    try:
        import grok2api.pool.model_health as _mh

        if hasattr(_mh, "invalidate_probe_http_client"):
            _mh.invalidate_probe_http_client()
    except Exception:
        pass


def update_runtime_settings(patch: dict[str, Any]) -> dict[str, Any]:
    """Apply a partial settings patch from admin UI. Returns effective public settings."""
    if not isinstance(patch, dict):
        raise ValueError("settings body must be an object")
    if "reasoning_compat" in patch and patch["reasoning_compat"] is not None:
        set_reasoning_compat(str(patch["reasoning_compat"]))
    if "outbound_max_tools" in patch and patch["outbound_max_tools"] is not None:
        set_outbound_max_tools(patch["outbound_max_tools"])
    if "outbound_max_tools_openai" in patch and patch["outbound_max_tools_openai"] is not None:
        set_outbound_max_tools_openai(patch["outbound_max_tools_openai"])
    if "outbound_tool_gap_sec" in patch and patch["outbound_tool_gap_sec"] is not None:
        set_outbound_tool_gap_sec(patch["outbound_tool_gap_sec"])
    if "history_compact_enabled" in patch and patch["history_compact_enabled"] is not None:
        set_history_compact_enabled(bool(patch["history_compact_enabled"]))
    if "history_compact_auto_chars" in patch and patch["history_compact_auto_chars"] is not None:
        set_history_compact_auto_chars(patch["history_compact_auto_chars"])
    if "history_keep_tool_rounds" in patch and patch["history_keep_tool_rounds"] is not None:
        set_history_keep_tool_rounds(patch["history_keep_tool_rounds"])
    elif "history_keep_recent_turns" in patch and patch["history_keep_recent_turns"] is not None:
        set_history_keep_tool_rounds(patch["history_keep_recent_turns"])
    if (
        "history_max_tool_result_chars" in patch
        and patch["history_max_tool_result_chars"] is not None
    ):
        set_history_max_tool_result_chars(patch["history_max_tool_result_chars"])
    elif (
        "history_tool_result_max_chars" in patch
        and patch["history_tool_result_max_chars"] is not None
    ):
        set_history_max_tool_result_chars(patch["history_tool_result_max_chars"])
    if "sse_keepalive" in patch and patch["sse_keepalive"] is not None:
        set_sse_keepalive(patch["sse_keepalive"])
    if (
        "conversation_affinity_enabled" in patch
        and patch["conversation_affinity_enabled"] is not None
    ):
        set_conversation_affinity_enabled(bool(patch["conversation_affinity_enabled"]))
    if (
        "conversation_affinity_ttl_sec" in patch
        and patch["conversation_affinity_ttl_sec"] is not None
    ):
        set_conversation_affinity_ttl_sec(patch["conversation_affinity_ttl_sec"])
    if (
        "token_maintain_interval_sec" in patch
        and patch["token_maintain_interval_sec"] is not None
    ):
        set_token_maintain_interval_sec(patch["token_maintain_interval_sec"])
    if "token_refresh_skew_sec" in patch and patch["token_refresh_skew_sec"] is not None:
        set_token_refresh_skew_sec(patch["token_refresh_skew_sec"])
    if (
        "model_health_interval_sec" in patch
        and patch["model_health_interval_sec"] is not None
    ):
        set_model_health_interval_sec(patch["model_health_interval_sec"])
    if (
        "model_health_auto_disable" in patch
        and patch["model_health_auto_disable"] is not None
    ):
        set_model_health_auto_disable(bool(patch["model_health_auto_disable"]))
    if "probe_models" in patch and patch["probe_models"] is not None:
        set_probe_models(patch["probe_models"])
    if "default_model" in patch and patch["default_model"] is not None:
        set_default_model_setting(str(patch["default_model"]))
    if "account_mode" in patch and patch["account_mode"] is not None:
        set_account_mode(str(patch["account_mode"]))
    if "token_maintain_enabled" in patch and patch["token_maintain_enabled"] is not None:
        set_token_maintain_enabled(bool(patch["token_maintain_enabled"]))
    if "model_health_enabled" in patch and patch["model_health_enabled"] is not None:
        set_model_health_enabled(bool(patch["model_health_enabled"]))
    # Pool rotation / cooldown policy (nested or flat)
    pool_keys = (
        "cooldown_default_sec",
        "cooldown_auth_sec",
        "cooldown_rate_limit_sec",
        "cooldown_server_error_sec",
        "cooldown_max_sec",
        "soft_model_block_ttl_sec",
        "durable_model_block_ttl_sec",
        "probe_fail_kick_streak",
        "probe_fail_disable_streak",
        "probe_kick_cooldown_sec",
        "max_failover_attempts",
    )
    pool_patch: dict[str, Any] = {}
    nested = patch.get("pool_policy")
    if isinstance(nested, dict):
        pool_patch.update(nested)
    for k in pool_keys:
        if k in patch and patch[k] is not None:
            pool_patch[k] = patch[k]
    if pool_patch:
        set_pool_policy(pool_patch)
    if "registration_config" in patch and patch["registration_config"] is not None:
        set_registration_config(patch["registration_config"])
    if "sub2api_config" in patch and patch["sub2api_config"] is not None:
        try:
            from grok2api.upstream.sub2api_client import set_sub2api_config

            set_sub2api_config(patch["sub2api_config"], replace=False)
        except Exception as e:  # noqa: BLE001
            raise ValueError(f"sub2api_config: {e}") from e
    if "cliproxyapi_config" in patch and patch["cliproxyapi_config"] is not None:
        try:
            from grok2api.upstream.cliproxyapi_client import set_cliproxyapi_config

            set_cliproxyapi_config(patch["cliproxyapi_config"], replace=False)
        except Exception as e:  # noqa: BLE001
            raise ValueError(f"cliproxyapi_config: {e}") from e
    # Outbound proxy pool (flat fields or nested outbound_proxy / outbound_proxy_config)
    ob_patch: dict[str, Any] = {}
    for nested_key in ("outbound_proxy_config", "outbound_proxy"):
        nested_ob = patch.get(nested_key)
        if isinstance(nested_ob, dict):
            ob_patch.update(nested_ob)
    for k in (
        "outbound_proxy_enabled",
        "outbound_proxy",
        "outbound_proxy_username",
        "outbound_proxy_password",
        "outbound_proxy_strategy",
    ):
        if k in patch and patch[k] is not None:
            short = k.replace("outbound_", "", 1) if k.startswith("outbound_") else k
            if short == "proxy_enabled":
                ob_patch["enabled"] = patch[k]
            else:
                ob_patch[short] = patch[k]
    # Also accept non-prefixed keys when nested under clear names from UI collect.
    if "proxy_pool" in patch and patch["proxy_pool"] is not None:
        ob_patch["proxy"] = patch["proxy_pool"]
    if ob_patch:
        set_outbound_proxy_config(ob_patch, replace=False)
    return get_public_settings()


def get_public_settings() -> dict[str, Any]:
    data = _load()
    # Secrets stay full for admin session API (admin-auth only); UI masks display.
    reg = get_registration_config(include_secrets=True)
    outbound = get_outbound_proxy_config(include_secrets=True)
    try:
        from grok2api.upstream.proxy_pool import outbound_pool_public_summary

        outbound_summary = outbound_pool_public_summary()
    except Exception:
        outbound_summary = {
            "enabled": bool(outbound.get("enabled") and outbound.get("proxy")),
            "count": 0,
            "strategy": outbound.get("proxy_strategy") or "round_robin",
            "source": "settings" if outbound.get("proxy") else "none",
            "preview": [],
        }
    try:
        from grok2api.upstream.sub2api_client import public_sub2api_config
        sub2api_pub = public_sub2api_config()
    except Exception:
        sub2api_pub = {"enabled": False, "base_url": "", "email": "", "has_password": False}
    try:
        from grok2api.upstream.cliproxyapi_client import public_cliproxyapi_config
        cliproxyapi_pub = public_cliproxyapi_config()
    except Exception:
        cliproxyapi_pub = {
            "enabled": False,
            "base_url": "",
            "has_management_key": False,
        }
    pool_policy = get_pool_policy()

    return {
        "account_mode": get_account_mode(),
        "account_modes": list(VALID_ACCOUNT_MODES),
        "has_admin_password": has_admin_password(),
        "setup_needed": is_setup_needed(),
        # True only when env was used (or could still be used) as bootstrap seed
        # because no durable hash existed yet. After setup/seed, always false
        # for "live env auth" — password is store-backed.
        "admin_password_from_env": bool(
            ADMIN_PASSWORD and not _stored_admin_hash_present()
        ),
        "admin_password_in_store": _stored_admin_hash_present(),
        "token_maintain_enabled": get_token_maintain_enabled(),
        "model_health_enabled": get_model_health_enabled(),
        "reasoning_compat": get_reasoning_compat(),
        "reasoning_compat_options": list(_VALID_REASONING),
        "outbound_max_tools": get_outbound_max_tools(),
        "outbound_max_tools_openai": get_outbound_max_tools_openai(),
        "outbound_tool_gap_sec": get_outbound_tool_gap_sec(),
        "history_compact_enabled": get_history_compact_enabled(),
        "history_compact_auto_chars": get_history_compact_auto_chars(),
        "history_keep_tool_rounds": get_history_keep_tool_rounds(),
        "history_max_tool_result_chars": get_history_max_tool_result_chars(),
        "sse_keepalive": get_sse_keepalive(),
        "conversation_affinity_enabled": get_conversation_affinity_enabled(),
        "conversation_affinity_ttl_sec": get_conversation_affinity_ttl_sec(),
        "token_maintain_interval_sec": get_token_maintain_interval_sec(),
        "token_refresh_skew_sec": get_token_refresh_skew_sec(),
        "model_health_interval_sec": get_model_health_interval_sec(),
        "model_health_auto_disable": get_model_health_auto_disable(),
        "probe_models": get_probe_models(),
        "default_model": get_default_model_setting(),
        # Expose both nested and flat policy fields. Older UI/builds read the
        # flat keys directly; current UI prefers pool_policy.
        **pool_policy,
        "pool_policy": pool_policy,
        "registration_config": reg,
        "outbound_proxy_config": outbound,
        "outbound_proxy_pool": outbound_summary,
        "sub2api_config": sub2api_pub,
        "cliproxyapi_config": cliproxyapi_pub,
        "updated_at": data.get("updated_at"),
    }
