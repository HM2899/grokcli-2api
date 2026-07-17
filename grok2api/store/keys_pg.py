"""PostgreSQL backend for managed API keys."""

from __future__ import annotations

import time
from typing import Any

from grok2api.store.pg import _ts, _unix, connection, pg_enabled


def enabled() -> bool:
    return pg_enabled()


def list_raw() -> list[dict[str, Any]]:
    if not enabled():
        return []
    try:
        from grok2api.store.crypto import maybe_decrypt_key_record
    except Exception:
        maybe_decrypt_key_record = lambda r: r  # type: ignore
    with connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT id, name, prefix, key_hash, secret, enabled, note,
                       created_at, last_used_at, request_count
                FROM api_keys ORDER BY created_at
                """
            )
            rows = cur.fetchall()
    out = []
    for r in rows:
        rec = {
            "id": r[0],
            "name": r[1],
            "prefix": r[2],
            "key_hash": r[3],
            "secret": r[4],
            "enabled": bool(r[5]),
            "note": r[6] or "",
            "created_at": _unix(r[7]) or 0.0,
            "last_used_at": _unix(r[8]),
            "request_count": int(r[9] or 0),
        }
        out.append(maybe_decrypt_key_record(rec))
    return out


def replace_all(keys: list[dict[str, Any]]) -> None:
    if not enabled():
        return
    with connection() as conn:
        with conn.cursor() as cur:
            cur.execute("DELETE FROM api_keys")
            for k in keys:
                _upsert(cur, k)
        conn.commit()


def insert(key: dict[str, Any]) -> None:
    """Insert one key without replacing a concurrently-created key set."""
    if not enabled():
        return
    encrypted = _encrypted_record(key)
    with connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                INSERT INTO api_keys (
                  id, name, prefix, key_hash, secret, enabled, note,
                  created_at, last_used_at, request_count
                ) VALUES (
                  %s, %s, %s, %s, %s, %s, %s, %s, %s, %s
                )
                """,
                _record_values(encrypted),
            )
        conn.commit()


def upsert(key: dict[str, Any]) -> None:
    if not enabled():
        return
    with connection() as conn:
        with conn.cursor() as cur:
            _upsert(cur, key)
        conn.commit()


def delete(key_id: str) -> bool:
    if not enabled() or not key_id:
        return False
    with connection() as conn:
        with conn.cursor() as cur:
            cur.execute("DELETE FROM api_keys WHERE id = %s", (key_id,))
            n = cur.rowcount
        conn.commit()
    return n > 0


def touch_usage(key_id: str) -> None:
    """Increment request_count + last_used_at (durable). Prefer Redis for hot path."""
    if not enabled() or not key_id:
        return
    with connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                UPDATE api_keys
                SET request_count = request_count + 1,
                    last_used_at = now()
                WHERE id = %s
                """,
                (key_id,),
            )
        conn.commit()


def find_by_hash(key_hash: str) -> dict[str, Any] | None:
    if not enabled() or not key_hash:
        return None
    with connection() as conn:
        with conn.cursor() as cur:
            cur.execute(
                """
                SELECT id, name, prefix, key_hash, secret, enabled, note,
                       created_at, last_used_at, request_count
                FROM api_keys WHERE key_hash = %s LIMIT 1
                """,
                (key_hash,),
            )
            r = cur.fetchone()
    if not r:
        return None
    return {
        "id": r[0],
        "name": r[1],
        "prefix": r[2],
        "key_hash": r[3],
        "secret": r[4],
        "enabled": bool(r[5]),
        "note": r[6] or "",
        "created_at": _unix(r[7]) or 0.0,
        "last_used_at": _unix(r[8]),
        "request_count": int(r[9] or 0),
    }


def _encrypted_record(k: dict[str, Any]) -> dict[str, Any]:
    try:
        from grok2api.store.crypto import maybe_encrypt_key_record

        return maybe_encrypt_key_record(k)
    except Exception:
        return k


def _record_values(k: dict[str, Any]) -> tuple[Any, ...]:
    return (
        k.get("id"),
        k.get("name") or "unnamed",
        k.get("prefix") or "",
        k.get("key_hash"),
        k.get("secret"),
        bool(k.get("enabled", True)),
        k.get("note") or "",
        _ts(k.get("created_at") or time.time()),
        _ts(k.get("last_used_at")),
        int(k.get("request_count") or 0),
    )


def _upsert(cur, k: dict[str, Any]) -> None:
    k = _encrypted_record(k)
    cur.execute(
        """
        INSERT INTO api_keys (
          id, name, prefix, key_hash, secret, enabled, note,
          created_at, last_used_at, request_count
        ) VALUES (
          %s, %s, %s, %s, %s, %s, %s, %s, %s, %s
        )
        ON CONFLICT (id) DO UPDATE SET
          name = EXCLUDED.name,
          prefix = EXCLUDED.prefix,
          key_hash = EXCLUDED.key_hash,
          secret = EXCLUDED.secret,
          enabled = EXCLUDED.enabled,
          note = EXCLUDED.note,
          last_used_at = EXCLUDED.last_used_at,
          request_count = EXCLUDED.request_count
        """,
        _record_values(k),
    )
