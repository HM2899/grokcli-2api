"""Account (auth.json) management — standalone, no local Grok CLI.

Supports:
  - Native OIDC device-code (no grok binary; works on headless Linux)
  - Multi-account import / merge (per-user storage keys)
  - Token refresh via refresh_token
"""

from __future__ import annotations

import json
import shutil
import sys
import time
import uuid
from typing import Any

from auth_store import read_auth_map, write_auth_map
from config import AUTH_FILE
from oidc_auth import (
    account_storage_id,
    decode_jwt_claims,
    get_device_session as oidc_get_device_session,
    list_device_sessions as oidc_list_device_sessions,
    normalize_auth_file_keys,
    parse_expires_at,
    refresh_all_accounts,
    start_device_authorization,
    upsert_entry,
)


def _mask_token(token: str | None) -> str:
    if not token:
        return ""
    if len(token) <= 12:
        return "****"
    return token[:6] + "..." + token[-4:]


def list_accounts() -> list[dict[str, Any]]:
    """List all session entries in auth.json (no full tokens)."""
    data = read_auth_map()
    if not data:
        return []

    now = time.time()
    out: list[dict[str, Any]] = []
    for key, entry in data.items():
        if not isinstance(entry, dict):
            continue
        token = entry.get("key") or entry.get("access_token") or entry.get("token")
        if not token:
            continue
        exp_f = parse_expires_at(
            entry.get("expires_at"), token if isinstance(token, str) else None
        )
        expired = bool(exp_f is not None and now >= exp_f)
        out.append(
            {
                "id": key,
                "email": entry.get("email"),
                "user_id": entry.get("user_id") or entry.get("principal_id"),
                "team_id": entry.get("team_id"),
                "auth_mode": entry.get("auth_mode"),
                "create_time": entry.get("create_time"),
                "expires_at": exp_f,
                "expired": expired,
                "has_refresh_token": bool(entry.get("refresh_token")),
                "token_hint": _mask_token(token if isinstance(token, str) else None),
                "first_name": entry.get("first_name"),
                "last_name": entry.get("last_name"),
                "principal_type": entry.get("principal_type"),
            }
        )
    out.sort(key=lambda a: a.get("expires_at") or 0, reverse=True)
    return out


def account_status() -> dict[str, Any]:
    accounts = list_accounts()
    active = [a for a in accounts if not a.get("expired")]
    try:
        from settings_store import get_account_mode

        mode = get_account_mode()
    except Exception:
        mode = "round_robin"
    return {
        "auth_file": str(AUTH_FILE),
        "auth_file_exists": AUTH_FILE.is_file(),
        "logged_in": bool(active),
        "account_count": len(accounts),
        "active_count": len(active),
        "accounts": accounts,
        "account_mode": mode,
        "platform": sys.platform,
        "is_linux": sys.platform.startswith("linux"),
        "is_headless": _is_headless(),
        "native_oidc_available": True,
        "multi_account": len(accounts) > 1,
    }


def _is_headless() -> bool:
    if sys.platform == "win32":
        return False
    import os

    display = os.environ.get("DISPLAY") or os.environ.get("WAYLAND_DISPLAY")
    return not bool(display)


def _resolve_account_key(data: dict, account_id: str) -> str | None:
    if account_id in data:
        return account_id
    for k, v in data.items():
        if k == account_id:
            return k
        if isinstance(v, dict) and (
            v.get("user_id") == account_id or k.endswith(f"::{account_id}")
        ):
            return k
    return None


def remove_account(account_id: str) -> bool:
    data = read_auth_map()
    matched = _resolve_account_key(data, account_id)
    if matched is None:
        return False
    if AUTH_FILE.is_file():
        backup = AUTH_FILE.with_suffix(f".bak.{int(time.time())}")
        try:
            shutil.copy2(AUTH_FILE, backup)
        except OSError:
            pass
    del data[matched]
    write_auth_map(data)
    return True


def remove_accounts(account_ids: list[str]) -> dict:
    """Remove many accounts with a single auth.json rewrite."""
    data = read_auth_map()
    removed: list[str] = []
    missing: list[str] = []
    seen: set[str] = set()
    for raw in account_ids:
        account_id = str(raw or "").strip()
        if not account_id or account_id in seen:
            continue
        seen.add(account_id)
        matched = _resolve_account_key(data, account_id)
        if matched is None or matched not in data:
            missing.append(account_id)
            continue
        del data[matched]
        removed.append(matched)
    if removed:
        if AUTH_FILE.is_file():
            backup = AUTH_FILE.with_suffix(f".bak.{int(time.time())}")
            try:
                shutil.copy2(AUTH_FILE, backup)
            except OSError:
                pass
        write_auth_map(data)
    return {
        "removed": removed,
        "missing": missing,
        "removed_count": len(removed),
        "missing_count": len(missing),
        "requested": len(seen),
    }


def clear_all_accounts() -> bool:
    if not AUTH_FILE.is_file():
        return True
    backup = AUTH_FILE.with_suffix(f".bak.{int(time.time())}")
    try:
        shutil.copy2(AUTH_FILE, backup)
        AUTH_FILE.unlink()
        return True
    except OSError:
        return False


def get_login_session(session_id: str) -> dict[str, Any] | None:
    return oidc_get_device_session(session_id)


def list_login_sessions() -> list[dict[str, Any]]:
    return oidc_list_device_sessions()


def start_login(mode: str = "device", *, capture: bool | None = None) -> dict[str, Any]:
    """
    Start native OIDC device-code login only.

    No local Grok CLI, no browser OAuth. Works on headless Linux.
    `mode` / `capture` kept for API compatibility; only device flow is used.
    """
    _ = capture  # unused; always native OIDC poll
    mode = (mode or "device").lower()
    if mode not in ("device", "oauth"):
        return {"ok": False, "error": "mode must be device (oauth removed)"}
    if mode == "oauth":
        # OAuth / local CLI login removed — fall through to device flow
        mode = "device"

    try:
        try:
            normalize_auth_file_keys()
        except Exception:
            pass
        result = start_device_authorization()
        if result.get("ok"):
            result["platform"] = sys.platform
            result["headless"] = _is_headless()
            result["auto_device_from_oauth"] = False
            result["message"] = result.get("message") or (
                "已启动设备码登录（原生 OIDC，无需本地 Grok CLI）。"
                "请用任意浏览器打开验证链接并输入设备码；完成后会自动写入账号池。"
            )
        return result
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "error": (
                f"设备码登录失败: {e}。"
                "请重试，或使用「导入 Token / auth.json」。"
            ),
        }


def run_logout() -> dict[str, Any]:
    """Clear local auth.json (no external CLI)."""
    ok = clear_all_accounts()
    return {
        "ok": ok,
        "message": "已清除本地 auth.json" if ok else "清除 auth.json 失败",
    }


# ── import tokens ───────────────────────────────────────────────────────────


def _normalize_entry(
    entry: dict[str, Any], preferred_id: str | None = None
) -> tuple[str, dict[str, Any]]:
    """Normalize one account entry and return (storage_id, entry)."""
    tok = entry.get("key") or entry.get("access_token") or entry.get("token")
    if not tok or not isinstance(tok, str):
        raise ValueError("missing token")
    entry = dict(entry)
    entry["key"] = tok
    claims = decode_jwt_claims(tok)

    uid = (
        entry.get("user_id")
        or entry.get("principal_id")
        or claims.get("principal_id")
        or claims.get("sub")
    )
    if uid:
        entry["user_id"] = str(uid)
        entry.setdefault("principal_id", str(uid))
    if not entry.get("email") and claims.get("email"):
        entry["email"] = claims["email"]
    if not entry.get("team_id") and claims.get("team_id"):
        entry["team_id"] = claims["team_id"]
    if not entry.get("principal_type") and claims.get("principal_type"):
        entry["principal_type"] = claims["principal_type"]
    if not entry.get("oidc_client_id"):
        cid = claims.get("client_id") or claims.get("aud") or entry.get("oidc_client_id")
        if isinstance(cid, list):
            cid = cid[0] if cid else None
        if cid:
            entry["oidc_client_id"] = str(cid)

    exp = parse_expires_at(entry.get("expires_at"), tok)
    if exp is not None:
        entry["expires_at"] = float(exp)

    entry.setdefault("auth_mode", entry.get("auth_mode") or "imported")
    entry.setdefault(
        "create_time",
        time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
    )

    aid = account_storage_id(
        user_id=str(uid) if uid else None,
        client_id=str(entry.get("oidc_client_id"))
        if entry.get("oidc_client_id")
        else None,
        fallback=preferred_id
        or f"https://auth.x.ai::imported-{uuid.uuid4().hex[:10]}",
    )
    return aid, entry


def export_auth_payload(*, include_secrets: bool = True) -> dict[str, Any]:
    """
    Export auth.json map for download/backup.
    include_secrets=True returns full tokens (needed for re-import).
    """
    data = read_auth_map()
    if not data:
        return {
            "ok": True,
            "auth": {},
            "count": 0,
            "auth_file": str(AUTH_FILE),
            "exported_at": time.time(),
        }
    if include_secrets:
        out = {k: dict(v) if isinstance(v, dict) else v for k, v in data.items()}
    else:
        out = {}
        for k, v in data.items():
            if not isinstance(v, dict):
                continue
            safe = {
                kk: vv
                for kk, vv in v.items()
                if kk
                not in (
                    "key",
                    "access_token",
                    "token",
                    "refresh_token",
                )
            }
            tok = v.get("key") or v.get("access_token") or v.get("token")
            if isinstance(tok, str):
                safe["token_hint"] = _mask_token(tok)
            safe["has_refresh_token"] = bool(v.get("refresh_token"))
            out[k] = safe
    return {
        "ok": True,
        "auth": out,
        "count": len(out),
        "auth_file": str(AUTH_FILE),
        "exported_at": time.time(),
    }


def import_auth_payload(
    raw: str | dict[str, Any], *, merge: bool = True
) -> dict[str, Any]:
    """
    Import credentials into auth.json (multi-account safe).

    Accepts:
      - full auth.json object { "https://auth.x.ai::uuid": { key, email, ... }, ... }
      - single entry object { key, email, ... }
      - { "token"|"key"|"access_token": "eyJ...", "email"?, "account_id"? }
      - export wrapper { "auth": { ... }, "count": N } from export_auth_payload
      - raw JWT string
    """
    if isinstance(raw, str):
        text = raw.strip()
        if not text:
            return {"ok": False, "error": "empty payload"}
        if text.startswith("{"):
            try:
                parsed: Any = json.loads(text)
            except json.JSONDecodeError as e:
                return {"ok": False, "error": f"invalid JSON: {e}"}
        else:
            parsed = {"key": text}
    else:
        parsed = raw

    if not isinstance(parsed, dict):
        return {"ok": False, "error": "payload must be object or JWT string"}

    # Unwrap export format: { "ok", "auth": {...}, "count", ... }
    if (
        "auth" in parsed
        and isinstance(parsed.get("auth"), dict)
        and "key" not in parsed
        and "access_token" not in parsed
        and "token" not in parsed
    ):
        auth_map = parsed["auth"]
        if not auth_map:
            return {
                "ok": True,
                "message": "导出文件中无账号，未变更",
                "imported": [],
                "auth_file": str(AUTH_FILE),
                "total_accounts": len(read_auth_map()) if AUTH_FILE.is_file() else 0,
            }
        if all(isinstance(v, dict) for v in auth_map.values()):
            parsed = auth_map

    raw_entries: list[tuple[str | None, dict[str, Any]]] = []

    looks_like_map = False
    if parsed and all(isinstance(v, dict) for v in parsed.values()):
        sample_vals = list(parsed.values())
        if sample_vals and any(
            "key" in v or "access_token" in v or "token" in v
            for v in sample_vals
            if isinstance(v, dict)
        ):
            if any(
                ("auth.x.ai" in str(k))
                or ("accounts.x.ai" in str(k))
                or ("::" in str(k))
                for k in parsed.keys()
            ):
                looks_like_map = True
            elif (
                "key" not in parsed
                and "access_token" not in parsed
                and "token" not in parsed
            ):
                looks_like_map = True

    if looks_like_map:
        for k, v in parsed.items():
            if isinstance(v, dict) and (
                v.get("key") or v.get("access_token") or v.get("token")
            ):
                raw_entries.append((str(k), dict(v)))
    else:
        token = (
            parsed.get("key")
            or parsed.get("token")
            or parsed.get("access_token")
            or parsed.get("accessToken")
        )
        if not token or not isinstance(token, str):
            return {
                "ok": False,
                "error": "missing token/key. Provide JWT or auth.json entry.",
            }
        account_id = (
            parsed.get("account_id") or parsed.get("id") or parsed.get("auth_key")
        )
        entry: dict[str, Any] = {
            "key": token,
            "auth_mode": parsed.get("auth_mode") or "imported",
            "create_time": parsed.get("create_time")
            or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        }
        if parsed.get("expires_at") is not None:
            entry["expires_at"] = parsed["expires_at"]
        if parsed.get("refresh_token"):
            entry["refresh_token"] = parsed["refresh_token"]
        for field in (
            "email",
            "user_id",
            "team_id",
            "first_name",
            "last_name",
            "principal_type",
            "oidc_client_id",
        ):
            if parsed.get(field) is not None:
                entry[field] = parsed[field]
        raw_entries.append((str(account_id) if account_id else None, entry))

    if not raw_entries:
        return {"ok": False, "error": "no valid account entries found"}

    normalized: dict[str, dict[str, Any]] = {}
    for pref_id, ent in raw_entries:
        try:
            aid, nent = _normalize_entry(ent, preferred_id=pref_id)
        except ValueError:
            continue
        normalized[aid] = nent

    if not normalized:
        return {"ok": False, "error": "entries missing token"}

    existing: dict[str, Any] = {}
    if merge and AUTH_FILE.is_file():
        existing = read_auth_map()
        backup = AUTH_FILE.with_suffix(f".bak.{int(time.time())}")
        try:
            shutil.copy2(AUTH_FILE, backup)
        except OSError:
            pass
        try:
            normalize_auth_file_keys()
            existing = read_auth_map()
        except Exception:
            pass

    if merge:
        for aid, nent in normalized.items():
            uid = nent.get("user_id")
            if not uid:
                continue
            for k in list(existing.keys()):
                v = existing.get(k)
                if not isinstance(v, dict):
                    continue
                if v.get("user_id") == uid or v.get("principal_id") == uid:
                    del existing[k]
        existing.update(normalized)
        final = existing
    else:
        final = normalized

    try:
        write_auth_map(final)
        normalize_auth_file_keys()
        final = read_auth_map()
    except OSError as e:
        return {"ok": False, "error": f"write auth.json failed: {e}"}

    imported = []
    for k, e in normalized.items():
        actual_id = k
        for fk, fv in final.items():
            if isinstance(fv, dict) and fv.get("key") == e.get("key"):
                actual_id = fk
                break
        imported.append(
            {
                "id": actual_id,
                "email": e.get("email"),
                "user_id": e.get("user_id"),
                "expires_at": e.get("expires_at"),
                "has_refresh_token": bool(e.get("refresh_token")),
                "token_hint": _mask_token(e.get("key")),
            }
        )
    return {
        "ok": True,
        "message": f"已导入 {len(imported)} 个账号到 {AUTH_FILE}（多账号合并={merge}）",
        "imported": imported,
        "auth_file": str(AUTH_FILE),
        "total_accounts": len(final) if isinstance(final, dict) else 0,
    }


def do_refresh_all(*, force: bool = True) -> dict[str, Any]:
    """
    Refresh accounts that have refresh_token.
    force=True: refresh all; force=False: only near-expiry.
    """
    from config import TOKEN_REFRESH_SKEW

    result = refresh_all_accounts(
        only_near_expiry=not force,
        skew_seconds=max(300.0, float(TOKEN_REFRESH_SKEW) * 2),
    )
    now = time.time()
    for r in result.get("results") or []:
        exp = r.get("expires_at")
        if isinstance(exp, (int, float)):
            r["remaining_sec"] = max(0, int(float(exp) - now))
    result["accounts"] = list_accounts()
    result["force"] = force
    try:
        import token_maintainer

        token_maintainer.request_run_soon()
    except Exception:
        pass
    return result


def do_normalize_keys() -> dict[str, Any]:
    return normalize_auth_file_keys()
