"""Account map store: PostgreSQL primary, auth.json only for file-mode / export.

Production (hybrid) reads and writes go to PostgreSQL. Local auth.json is:
  - never written as a runtime mirror when PG is enabled
  - still used as the durable store when STORE_BACKEND=file (escape hatch)
  - still produced on demand by admin export / migrate_json_to_pg import

Centralizes read/write with:
  - process-local RLock (thread safety)
  - optional file lock via fcntl / msvcrt (file mode only)
  - atomic tmp + replace writes (file mode only)
  - mtime-based in-process cache for file-mode large maps
"""

from __future__ import annotations

import json
import os
import sys
import threading
import time
from contextlib import contextmanager
from pathlib import Path
from typing import Any, Iterator

from grok2api.config import AUTH_FILE, STORE_BACKEND

_thread_lock = threading.RLock()


def _hybrid_mode() -> bool:
    return str(STORE_BACKEND or "hybrid").strip().lower() != "file"


def _raise_hybrid_store_error(operation: str) -> None:
    raise RuntimeError(
        f"PostgreSQL {operation} failed in hybrid mode; refusing file fallback "
        "to prevent split-brain data"
    )

# In-process read cache (invalidated on write / mtime change)
_cache_lock = threading.RLock()
_cache_path: str | None = None
_cache_mtime_ns: int | None = None
_cache_data: dict[str, Any] | None = None
_cache_stat_at = 0.0
_CACHE_STAT_MIN_INTERVAL = 0.25  # seconds between mtime checks under load


def _lock_path(path: Path) -> Path:
    return path.with_suffix(path.suffix + ".lock")


@contextmanager
def _file_lock(path: Path, *, timeout: float = 10.0) -> Iterator[None]:
    """Best-effort exclusive file lock (Linux fcntl / Windows msvcrt)."""
    lock_file = _lock_path(path)
    lock_file.parent.mkdir(parents=True, exist_ok=True)
    fh = open(lock_file, "a+b")
    try:
        if fh.tell() == 0:
            fh.write(b"0")
            fh.flush()
    except OSError:
        pass
    deadline = time.time() + timeout
    locked = False
    try:
        while True:
            try:
                if sys.platform == "win32":
                    import msvcrt

                    fh.seek(0)
                    msvcrt.locking(fh.fileno(), msvcrt.LK_NBLCK, 1)
                else:
                    import fcntl

                    fcntl.flock(fh.fileno(), fcntl.LOCK_EX | fcntl.LOCK_NB)
                locked = True
                break
            except (OSError, BlockingIOError):
                if time.time() >= deadline:
                    # proceed without lock rather than deadlock the API
                    break
                time.sleep(0.05)
        yield
    finally:
        if locked:
            try:
                if sys.platform == "win32":
                    import msvcrt

                    fh.seek(0)
                    msvcrt.locking(fh.fileno(), msvcrt.LK_UNLCK, 1)
                else:
                    import fcntl

                    fcntl.flock(fh.fileno(), fcntl.LOCK_UN)
            except OSError:
                pass
        try:
            fh.close()
        except OSError:
            pass


@contextmanager
def auth_lock(timeout: float = 10.0) -> Iterator[None]:
    with _thread_lock:
        with _file_lock(AUTH_FILE, timeout=timeout):
            yield


def _invalidate_live_credentials_cache() -> None:
    try:
        from grok2api.pool.auth import invalidate_live_credentials_cache

        invalidate_live_credentials_cache()
    except Exception:
        pass


def _invalidate_cache(path: Path | None = None) -> None:
    global _cache_path, _cache_mtime_ns, _cache_data, _cache_stat_at
    with _cache_lock:
        if path is None or _cache_path in (None, str(path)):
            _cache_path = None
            _cache_mtime_ns = None
            _cache_data = None
            _cache_stat_at = 0.0
    # Account map changed (or may have): drop request-path live credential cache.
    _invalidate_live_credentials_cache()


def _set_cache(path: Path, data: dict[str, Any], mtime_ns: int | None) -> None:
    global _cache_path, _cache_mtime_ns, _cache_data, _cache_stat_at
    with _cache_lock:
        _cache_path = str(path)
        _cache_mtime_ns = mtime_ns
        # Store a shallow copy of the map; values are still shared dicts.
        # Callers that mutate must write via write/mutate APIs.
        _cache_data = dict(data)
        _cache_stat_at = time.time()


def _cached_read(path: Path) -> dict[str, Any] | None:
    """Return cached map if mtime unchanged; None on miss."""
    global _cache_stat_at
    with _cache_lock:
        if _cache_data is None or _cache_path != str(path):
            return None
        now = time.time()
        # Under write lock callers already have exclusive access; still cheap-check.
        if now - _cache_stat_at < _CACHE_STAT_MIN_INTERVAL and _cache_mtime_ns is not None:
            return dict(_cache_data)
        try:
            st = path.stat()
            mtime_ns = getattr(st, "st_mtime_ns", int(st.st_mtime * 1e9))
        except OSError:
            return None
        _cache_stat_at = now
        if _cache_mtime_ns is not None and mtime_ns == _cache_mtime_ns:
            return dict(_cache_data)
        return None


def _path_mtime_ns(path: Path) -> int | None:
    try:
        st = path.stat()
        return getattr(st, "st_mtime_ns", int(st.st_mtime * 1e9))
    except OSError:
        return None


def _dump_json(data: dict[str, Any]) -> str:
    """
    Compact JSON for large multi-account files (much faster + smaller than indent=2).
    Set GROK2API_AUTH_PRETTY=1 to keep human-readable formatting.
    """
    pretty = os.getenv("GROK2API_AUTH_PRETTY", "0").lower() in ("1", "true", "yes")
    if pretty:
        return json.dumps(data, ensure_ascii=False, indent=2)
    return json.dumps(data, ensure_ascii=False, separators=(",", ":"))


def _pg_accounts():
    try:
        from grok2api.store.accounts_pg import enabled

        if enabled():
            from grok2api.store import accounts_pg

            return accounts_pg
    except Exception:
        return None
    return None


def read_auth_map(path: Path | None = None) -> dict[str, Any]:
    path = path or AUTH_FILE
    # PostgreSQL durable backend (multi-worker / multi-host)
    if path == AUTH_FILE or path.resolve() == AUTH_FILE.resolve():
        pg = _pg_accounts()
        if pg is not None:
            return pg.read_auth_map()
        if _hybrid_mode():
            _raise_hybrid_store_error("account read")

    # Fast path: cache hit without taking file lock (safe for read-mostly)
    cached = _cached_read(path)
    if cached is not None:
        return cached

    with auth_lock():
        # re-check cache under lock (writer may have just finished)
        cached = _cached_read(path)
        if cached is not None:
            return cached
        if not path.is_file():
            _set_cache(path, {}, None)
            return {}
        try:
            text = path.read_text(encoding="utf-8")
            data = json.loads(text)
            if not isinstance(data, dict):
                data = {}
        except (OSError, json.JSONDecodeError):
            data = {}
        _set_cache(path, data, _path_mtime_ns(path))
        return dict(data)


def read_auth_entry(
    account_id: str, path: Path | None = None
) -> tuple[str, dict[str, Any]] | None:
    """Load one account entry without scanning the whole pool when possible."""
    if not account_id:
        return None
    path = path or AUTH_FILE
    aid = str(account_id).strip()
    if not aid:
        return None
    if path == AUTH_FILE or path.resolve() == AUTH_FILE.resolve():
        pg = _pg_accounts()
        if pg is not None:
            return pg.read_auth_entry(aid)
        if _hybrid_mode():
            _raise_hybrid_store_error("account entry read")
    data = read_auth_map(path)
    if not isinstance(data, dict) or not data:
        return None
    hit = data.get(aid)
    if isinstance(hit, dict):
        return aid, hit
    for k, v in data.items():
        if not isinstance(v, dict):
            continue
        if (
            k == aid
            or v.get("user_id") == aid
            or v.get("principal_id") == aid
            or str(k).endswith(f"::{aid}")
        ):
            return str(k), v
    return None


def _write_auth_file(data: dict[str, Any], path: Path) -> None:
    """Atomic write of auth map to a local file (file-mode primary or explicit export).

    Not used as a runtime mirror when PostgreSQL is enabled.
    """
    path.parent.mkdir(parents=True, exist_ok=True)
    tmp = path.with_suffix(path.suffix + f".tmp.{os.getpid()}")
    payload = _dump_json(data if isinstance(data, dict) else {})
    tmp.write_text(payload, encoding="utf-8")
    last_err: Exception | None = None
    for _ in range(8):
        try:
            os.replace(str(tmp), str(path))
            last_err = None
            break
        except OSError as e:
            last_err = e
            time.sleep(0.03)
    if last_err is not None:
        try:
            tmp.unlink(missing_ok=True)  # type: ignore[call-arg]
        except TypeError:
            if tmp.exists():
                tmp.unlink()
        raise last_err
    _set_cache(path, data if isinstance(data, dict) else {}, _path_mtime_ns(path))


def write_auth_map(data: dict[str, Any], path: Path | None = None) -> None:
    """Persist auth map. PostgreSQL is primary when enabled — no auth.json mirror."""
    path = path or AUTH_FILE
    data = data if isinstance(data, dict) else {}
    if path == AUTH_FILE or path.resolve() == AUTH_FILE.resolve():
        pg = _pg_accounts()
        if pg is not None:
            pg.write_auth_map(data)
            _invalidate_cache(path)
            return
        if _hybrid_mode():
            _raise_hybrid_store_error("full account write")
    with auth_lock():
        _write_auth_file(data, path)


def mutate_auth_map(mutator) -> dict[str, Any]:
    """
    Read → mutate(dict) → write under one lock.
    mutator receives the map and may modify in place; return value is ignored.
    """
    pg = _pg_accounts()
    if pg is not None:
        data = pg.mutate_auth_map(mutator)
        _invalidate_cache(AUTH_FILE)
        return data
    if _hybrid_mode():
        _raise_hybrid_store_error("account mutation")
    with auth_lock():
        path = AUTH_FILE
        data: dict[str, Any] = {}
        if path.is_file():
            try:
                raw = json.loads(path.read_text(encoding="utf-8"))
                if isinstance(raw, dict):
                    data = raw
            except (OSError, json.JSONDecodeError):
                data = {}
        mutator(data)
        _write_auth_file(data, path)
        return data


def upsert_auth_entry(
    account_id: str,
    entry: dict[str, Any],
    *,
    merge_same_user: bool = True,
) -> str:
    """Prefer row-level upsert on PG; fall back to full-map mutate on file."""
    if not account_id or not isinstance(entry, dict):
        return account_id or ""
    pg = _pg_accounts()
    if pg is not None:
        pg.upsert_account_merged(
            account_id, entry, merge_same_user=merge_same_user
        )
        _invalidate_cache(AUTH_FILE)
        return account_id
    if _hybrid_mode():
        _raise_hybrid_store_error("account upsert")

    def _mut(data: dict[str, Any]) -> None:
        uid = entry.get("user_id") or entry.get("principal_id")
        token = entry.get("key")
        if merge_same_user:
            for k in list(data.keys()):
                if k == account_id:
                    continue
                v = data.get(k)
                if not isinstance(v, dict):
                    continue
                same_user = bool(
                    uid
                    and (v.get("user_id") == uid or v.get("principal_id") == uid)
                )
                same_token = bool(token and v.get("key") == token)
                if same_user or same_token:
                    del data[k]
        data[account_id] = entry

    mutate_auth_map(_mut)
    return account_id
