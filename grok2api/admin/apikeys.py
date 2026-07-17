"""Managed API key store for client distribution."""

from __future__ import annotations

import hashlib
import json
import os
import secrets
import threading
import time
import uuid
from dataclasses import asdict, dataclass, field
from pathlib import Path
from typing import Any

from grok2api.config import API_KEY, KEYS_FILE, REQUIRE_API_KEY, STORE_BACKEND


_lock = threading.RLock()

# Short-lived verify cache: key_hash -> (record_dict, expires_at)
_verify_cache: dict[str, tuple[dict[str, Any], float]] = {}
_VERIFY_CACHE_TTL = float(os.getenv("GROK2API_APIKEY_CACHE_TTL", "5") or 5)
_VERIFY_CACHE_MAX = 2048


def _cache_get(h: str) -> dict[str, Any] | None:
    ent = _verify_cache.get(h)
    if not ent:
        return None
    rec, exp = ent
    if time.time() > exp:
        _verify_cache.pop(h, None)
        return None
    return rec


def _cache_put(h: str, rec: dict[str, Any]) -> None:
    if len(_verify_cache) >= _VERIFY_CACHE_MAX:
        # drop arbitrary old entries
        for k in list(_verify_cache.keys())[:64]:
            _verify_cache.pop(k, None)
    _verify_cache[h] = (rec, time.time() + max(0.5, _VERIFY_CACHE_TTL))


def _cache_invalidate() -> None:
    _verify_cache.clear()


@dataclass
class ApiKeyRecord:
    id: str
    name: str
    prefix: str
    key_hash: str
    created_at: float
    enabled: bool = True
    note: str = ""
    last_used_at: float | None = None
    request_count: int = 0
    # Full plaintext for admin re-copy (local self-host). Older keys may lack it.
    secret: str | None = None

    def public_dict(self) -> dict[str, Any]:
        d: dict[str, Any] = {
            "id": self.id,
            "name": self.name,
            "prefix": self.prefix,
            "created_at": self.created_at,
            "enabled": self.enabled,
            "note": self.note,
            "last_used_at": self.last_used_at,
            "request_count": self.request_count,
            "key_hint": f"{self.prefix}…****",
            "has_secret": bool(self.secret),
        }
        # Admin list / create only — never exposed on public client routes
        if self.secret:
            d["secret"] = self.secret
        return d


def _ensure_parent(path: Path) -> None:
    path.parent.mkdir(parents=True, exist_ok=True)


def _hash_key(raw: str) -> str:
    return hashlib.sha256(raw.encode("utf-8")).hexdigest()


def _new_key_secret() -> str:
    return "sk-g2a-" + secrets.token_urlsafe(32)


def _pg_keys():
    try:
        from grok2api.store.keys_pg import enabled

        if enabled():
            from grok2api.store import keys_pg

            return keys_pg
    except Exception:
        return None
    return None


def _load_raw() -> dict[str, Any]:
    pg = _pg_keys()
    if pg is not None:
        return {"keys": pg.list_raw()}
    if str(STORE_BACKEND or "hybrid").strip().lower() != "file":
        raise RuntimeError(
            "PostgreSQL API key read failed in hybrid mode; refusing file "
            "fallback to prevent split-brain data"
        )
    if not KEYS_FILE.is_file():
        return {"keys": []}
    try:
        data = json.loads(KEYS_FILE.read_text(encoding="utf-8"))
        if not isinstance(data, dict):
            return {"keys": []}
        data.setdefault("keys", [])
        return data
    except (OSError, json.JSONDecodeError):
        return {"keys": []}


def _save_raw(data: dict[str, Any]) -> None:
    """Persist key list. PostgreSQL is primary; keys.json only in file mode."""
    keys = list(data.get("keys") or [])
    pg = _pg_keys()
    if pg is not None:
        pg.replace_all(keys)
        return
    if str(STORE_BACKEND or "hybrid").strip().lower() != "file":
        raise RuntimeError(
            "PostgreSQL API key write failed in hybrid mode; refusing file "
            "fallback to prevent split-brain data"
        )
    _ensure_parent(KEYS_FILE)
    tmp = KEYS_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(KEYS_FILE)


def _from_dict(d: dict[str, Any]) -> ApiKeyRecord:
    secret = d.get("secret") or d.get("key") or None
    if isinstance(secret, str):
        secret = secret.strip() or None
    else:
        secret = None
    return ApiKeyRecord(
        id=d["id"],
        name=d.get("name") or "unnamed",
        prefix=d.get("prefix") or "",
        key_hash=d["key_hash"],
        created_at=float(d.get("created_at") or time.time()),
        enabled=bool(d.get("enabled", True)),
        note=d.get("note") or "",
        last_used_at=d.get("last_used_at"),
        request_count=int(d.get("request_count") or 0),
        secret=secret,
    )


def keys_store_source() -> str:
    return "postgres" if _pg_keys() is not None else "file"


def list_keys() -> list[dict[str, Any]]:
    """List API keys from durable store (PostgreSQL when enabled)."""
    with _lock:
        data = _load_raw()
        return [_from_dict(k).public_dict() for k in data["keys"]]


def create_key(name: str, note: str = "") -> dict[str, Any]:
    """Create a new key. Stores secret for admin re-copy; also returns `key` once."""
    name = (name or "default").strip() or "default"
    raw = _new_key_secret()
    prefix = raw[:12]
    rec = ApiKeyRecord(
        id=str(uuid.uuid4()),
        name=name,
        prefix=prefix,
        key_hash=_hash_key(raw),
        created_at=time.time(),
        enabled=True,
        note=(note or "").strip(),
        secret=raw,
    )
    row = asdict(rec)
    with _lock:
        pg = _pg_keys()
        if pg is not None:
            pg.insert(row)
        else:
            data = _load_raw()
            data["keys"].append(row)
            _save_raw(data)
        _cache_invalidate()
    out = rec.public_dict()
    out["key"] = raw  # alias for older admin UI
    return out


def regenerate_key(key_id: str) -> dict[str, Any] | None:
    """Rotate an existing key and store the new plaintext for admin copying."""
    with _lock:
        data = _load_raw()
        for k in data["keys"]:
            if k.get("id") == key_id:
                raw = _new_key_secret()
                k["prefix"] = raw[:12]
                k["key_hash"] = _hash_key(raw)
                k["secret"] = raw
                pg = _pg_keys()
                if pg is not None:
                    pg.upsert(k)
                else:
                    _save_raw(data)
                _cache_invalidate()
                out = _from_dict(k).public_dict()
                out["key"] = raw
                return out
    return None


def set_enabled(key_id: str, enabled: bool) -> dict[str, Any] | None:
    with _lock:
        data = _load_raw()
        for k in data["keys"]:
            if k.get("id") == key_id:
                k["enabled"] = bool(enabled)
                pg = _pg_keys()
                if pg is not None:
                    pg.upsert(k)
                else:
                    _save_raw(data)
                _cache_invalidate()
                return _from_dict(k).public_dict()
    return None


def delete_key(key_id: str) -> bool:
    with _lock:
        pg = _pg_keys()
        if pg is not None:
            ok = bool(pg.delete(key_id))
            if ok:
                _cache_invalidate()
            return ok
        if str(STORE_BACKEND or "hybrid").strip().lower() != "file":
            raise RuntimeError(
                "PostgreSQL API key delete failed in hybrid mode; refusing file "
                "fallback to prevent split-brain data"
            )
        data = _load_raw()
        before = len(data["keys"])
        data["keys"] = [k for k in data["keys"] if k.get("id") != key_id]
        if len(data["keys"]) == before:
            return False
        _save_raw(data)
        _cache_invalidate()
        return True


def update_key(key_id: str, *, name: str | None = None, note: str | None = None) -> dict[str, Any] | None:
    with _lock:
        data = _load_raw()
        for k in data["keys"]:
            if k.get("id") == key_id:
                if name is not None:
                    k["name"] = name.strip() or k["name"]
                if note is not None:
                    k["note"] = note
                pg = _pg_keys()
                if pg is not None:
                    pg.upsert(k)
                else:
                    _save_raw(data)
                _cache_invalidate()
                return _from_dict(k).public_dict()
    return None


def verify_key(raw: str | None) -> ApiKeyRecord | None:
    """Validate client API key. Accepts managed keys and legacy env API_KEY."""
    if not raw:
        return None
    raw = raw.strip()
    if not raw:
        return None

    # Legacy env single key
    if API_KEY and secrets.compare_digest(raw, API_KEY):
        return ApiKeyRecord(
            id="env",
            name="env:GROK2API_API_KEY",
            prefix=raw[:12] if len(raw) >= 12 else raw,
            key_hash=_hash_key(raw),
            created_at=0,
            enabled=True,
            note="from environment",
        )

    h = _hash_key(raw)
    cached = _cache_get(h)
    if cached is not None:
        if not cached.get("enabled", True):
            return None
        rec = _from_dict(cached)
        _bump_key_usage(rec)
        return rec

    with _lock:
        # Prefer PG hash lookup when available (avoids full table scan in Python)
        try:
            from grok2api.store.keys_pg import enabled as pg_on, find_by_hash

            if pg_on():
                row = find_by_hash(h)
                if row and row.get("enabled", True):
                    _cache_put(h, row)
                    rec = _from_dict(row)
                    _bump_key_usage(rec)
                    return rec
                if row and not row.get("enabled", True):
                    return None
        except Exception:
            pass

        data = _load_raw()
        for k in data["keys"]:
            if not k.get("enabled", True):
                continue
            if secrets.compare_digest(k.get("key_hash", ""), h):
                rec = _from_dict(k)
                _cache_put(h, dict(k))
                # Prefer Redis counters under multi-worker to avoid full-file
                # RMW races on every authenticated request.
                redis_counted = False
                try:
                    from grok2api.store.redis_client import hincrby, hset_map, key, redis_enabled

                    if redis_enabled():
                        rk = key("apikey", "stats", rec.id)
                        hincrby(rk, "request_count", 1)
                        hset_map(rk, {"last_used_at": time.time()})
                        redis_counted = True
                except Exception:
                    redis_counted = False
                if not redis_counted:
                    k["last_used_at"] = time.time()
                    k["request_count"] = int(k.get("request_count") or 0) + 1
                    _save_raw(data)
                else:
                    rec.last_used_at = time.time()
                    rec.request_count = int(rec.request_count or 0) + 1
                return rec
    return None


def _bump_key_usage(rec: ApiKeyRecord) -> None:
    """Best-effort usage counter (Redis preferred)."""
    try:
        from grok2api.store.redis_client import hincrby, hset_map, key, redis_enabled

        if redis_enabled() and rec.id:
            rk = key("apikey", "stats", rec.id)
            hincrby(rk, "request_count", 1)
            hset_map(rk, {"last_used_at": time.time()})
            rec.last_used_at = time.time()
            rec.request_count = int(rec.request_count or 0) + 1
            return
    except Exception:
        pass
    # File/PG durable bump (slower) — only when no Redis
    try:
        from grok2api.store.keys_pg import enabled as pg_on, touch_usage

        if pg_on() and rec.id:
            touch_usage(rec.id)
            return
    except Exception:
        pass


def has_any_keys() -> bool:
    with _lock:
        data = _load_raw()
        if API_KEY:
            return True
        return any(k.get("enabled", True) for k in data["keys"])


def auth_required() -> bool:
    """Whether /v1 must present a valid API key."""
    mode = (REQUIRE_API_KEY or "auto").lower()
    if mode in ("1", "true", "yes", "on"):
        return True
    if mode in ("0", "false", "no", "off"):
        return False
    # auto: require if any key exists
    return has_any_keys()


def stats() -> dict[str, Any]:
    with _lock:
        data = _load_raw()
        keys = [_from_dict(k) for k in data["keys"]]
        return {
            "total": len(keys),
            "enabled": sum(1 for k in keys if k.enabled),
            "disabled": sum(1 for k in keys if not k.enabled),
            "total_requests": sum(k.request_count for k in keys),
            "auth_required": auth_required(),
            "legacy_env_key": bool(API_KEY),
        }
