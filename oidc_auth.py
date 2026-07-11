"""Pure OIDC device-code + refresh for xAI (no Grok CLI binary required).

Works on headless Linux servers: show user_code, poll token endpoint,
persist access_token + refresh_token into auth.json with per-user keys
so multiple accounts can coexist.
"""

from __future__ import annotations

import base64
import json
import threading
import time
import uuid
from datetime import datetime, timezone
from typing import Any
import httpx

from auth_store import mutate_auth_map, read_auth_map, write_auth_map
from config import GROK_CLI_CLIENT_ID, OIDC_DEVICE_URL, OIDC_SCOPES, OIDC_TOKEN_URL

# In-memory device sessions (server-side poll)
_lock = threading.RLock()
_device_sessions: dict[str, dict[str, Any]] = {}
# Serialize refresh for same account (avoid parallel refresh_token races)
_refresh_locks: dict[str, threading.Lock] = {}
_refresh_locks_guard = threading.Lock()


def _b64url_json(segment: str) -> dict[str, Any]:
    try:
        pad = "=" * (-len(segment) % 4)
        raw = base64.urlsafe_b64decode(segment + pad)
        data = json.loads(raw.decode("utf-8"))
        return data if isinstance(data, dict) else {}
    except Exception:
        return {}


def decode_jwt_claims(token: str) -> dict[str, Any]:
    parts = token.split(".")
    if len(parts) < 2:
        return {}
    return _b64url_json(parts[1])


def parse_expires_at(value: Any, token: str | None = None) -> float | None:
    """Accept unix float/int, ISO-8601 string, or JWT exp fallback."""
    if value is None:
        pass
    elif isinstance(value, (int, float)):
        return float(value)
    elif isinstance(value, str):
        s = value.strip()
        if not s:
            pass
        else:
            try:
                return float(s)
            except ValueError:
                pass
            try:
                # handle nanoseconds / trailing Z
                if s.endswith("Z"):
                    s = s[:-1] + "+00:00"
                # trim >6 fractional digits for fromisoformat
                if "." in s:
                    head, rest = s.split(".", 1)
                    digits = ""
                    tz = ""
                    for i, ch in enumerate(rest):
                        if ch.isdigit():
                            digits += ch
                        else:
                            tz = rest[i:]
                            break
                    digits = (digits + "000000")[:6]
                    s = f"{head}.{digits}{tz}"
                dt = datetime.fromisoformat(s)
                if dt.tzinfo is None:
                    dt = dt.replace(tzinfo=timezone.utc)
                return dt.timestamp()
            except ValueError:
                pass
    if token:
        exp = decode_jwt_claims(token).get("exp")
        try:
            return float(exp) if exp is not None else None
        except (TypeError, ValueError):
            return None
    return None


def account_storage_id(
    *,
    user_id: str | None = None,
    client_id: str | None = None,
    fallback: str | None = None,
) -> str:
    """
    Stable multi-account key. Prefer user_id so multiple humans sharing the
    same OAuth client_id do not overwrite each other (CLI default key is
    issuer::client_id which is single-slot).
    """
    if user_id:
        return f"https://auth.x.ai::{user_id}"
    if client_id:
        return f"https://auth.x.ai::{client_id}"
    return fallback or f"https://auth.x.ai::imported-{uuid.uuid4().hex[:12]}"


def entry_from_token_response(
    token_data: dict[str, Any],
    *,
    previous: dict[str, Any] | None = None,
) -> tuple[str, dict[str, Any]]:
    access = token_data.get("access_token") or token_data.get("key")
    if not access or not isinstance(access, str):
        raise ValueError("token response missing access_token")

    claims = decode_jwt_claims(access)
    prev = previous or {}
    user_id = (
        prev.get("user_id")
        or claims.get("principal_id")
        or claims.get("sub")
        or prev.get("principal_id")
    )
    client_id = (
        prev.get("oidc_client_id")
        or claims.get("client_id")
        or claims.get("aud")
        or GROK_CLI_CLIENT_ID
    )
    if isinstance(client_id, list):
        client_id = client_id[0] if client_id else GROK_CLI_CLIENT_ID

    expires_in = token_data.get("expires_in")
    exp = parse_expires_at(None, access)
    if exp is None and expires_in is not None:
        try:
            exp = time.time() + float(expires_in)
        except (TypeError, ValueError):
            exp = None

    entry: dict[str, Any] = {
        "key": access,
        "auth_mode": prev.get("auth_mode") or "oidc",
        "create_time": prev.get("create_time")
        or time.strftime("%Y-%m-%dT%H:%M:%SZ", time.gmtime()),
        "oidc_issuer": prev.get("oidc_issuer") or "https://auth.x.ai",
        "oidc_client_id": str(client_id),
    }
    if exp is not None:
        entry["expires_at"] = float(exp)

    refresh = token_data.get("refresh_token") or prev.get("refresh_token")
    if refresh:
        entry["refresh_token"] = refresh

    email = prev.get("email") or claims.get("email")
    if email:
        entry["email"] = email
    if user_id:
        entry["user_id"] = str(user_id)
        entry["principal_id"] = str(user_id)
    for field in ("first_name", "last_name", "principal_type", "team_id"):
        if prev.get(field) is not None:
            entry[field] = prev[field]
        elif claims.get(field) is not None:
            entry[field] = claims[field]
    if claims.get("team_id") and "team_id" not in entry:
        entry["team_id"] = claims["team_id"]
    if claims.get("principal_type") and "principal_type" not in entry:
        entry["principal_type"] = claims["principal_type"]
    # given_name / family_name from userinfo-like claims
    if claims.get("given_name") and "first_name" not in entry:
        entry["first_name"] = claims["given_name"]
    if claims.get("family_name") and "last_name" not in entry:
        entry["last_name"] = claims["family_name"]

    aid = account_storage_id(user_id=str(user_id) if user_id else None, client_id=str(client_id))
    return aid, entry


def upsert_entry(account_id: str, entry: dict[str, Any], *, merge_same_user: bool = True) -> str:
    """
    Save one account. If another key holds the same user_id, replace/remove it
    so we never keep duplicate tokens for the same person.
    Multi-account safe: keys are per-user (issuer::user_id), not client_id slot.
    """

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
                    and (
                        v.get("user_id") == uid
                        or v.get("principal_id") == uid
                    )
                )
                same_token = bool(token and v.get("key") == token)
                if same_user or same_token:
                    del data[k]
        data[account_id] = entry

    mutate_auth_map(_mut)
    return account_id


def normalize_auth_file_keys() -> dict[str, Any]:
    """
    Re-key entries that only use client_id slot into per-user keys so multiple
    accounts can coexist. Safe no-op when already unique.
    Call on startup and after import/login — legacy keys used
    https://auth.x.ai::<client_id> which breaks multi-account.
    """
    data = read_auth_map()
    if not data:
        return {"ok": True, "changed": 0, "total": 0}

    changed = 0
    new_map: dict[str, Any] = {}
    for old_key, entry in data.items():
        if not isinstance(entry, dict):
            continue
        token = entry.get("key") or entry.get("access_token") or entry.get("token")
        if not token:
            new_map[old_key] = entry
            continue
        entry = dict(entry)
        if entry.get("expires_at") is not None:
            exp = parse_expires_at(
                entry.get("expires_at"), token if isinstance(token, str) else None
            )
            if exp is not None:
                entry["expires_at"] = exp
                entry["key"] = token
        elif isinstance(token, str):
            exp = parse_expires_at(None, token)
            if exp is not None:
                entry["expires_at"] = exp
                entry["key"] = token
        uid = entry.get("user_id") or entry.get("principal_id")
        if not uid and isinstance(token, str):
            claims = decode_jwt_claims(token)
            uid = claims.get("principal_id") or claims.get("sub")
            if uid:
                entry["user_id"] = str(uid)
                entry.setdefault("principal_id", str(uid))
                if claims.get("email") and not entry.get("email"):
                    entry["email"] = claims["email"]
                if claims.get("team_id") and not entry.get("team_id"):
                    entry["team_id"] = claims["team_id"]
                if entry.get("expires_at") is None:
                    exp = parse_expires_at(None, token)
                    if exp is not None:
                        entry["expires_at"] = exp
                if not entry.get("refresh_token") and claims.get("jti"):
                    pass  # refresh only from token response
        new_key = account_storage_id(
            user_id=str(uid) if uid else None,
            fallback=old_key,
        )
        if new_key != old_key:
            changed += 1
        # Prefer entry that has refresh_token when colliding on same user
        if new_key in new_map:
            prev = new_map[new_key]
            if isinstance(prev, dict) and prev.get("refresh_token") and not entry.get(
                "refresh_token"
            ):
                continue
        new_map[new_key] = entry

    if changed or new_map != data:
        write_auth_map(new_map)
    return {"ok": True, "changed": changed, "total": len(new_map)}


def refresh_access_token(
    entry: dict[str, Any],
    *,
    client: httpx.Client | None = None,
    timeout: float = 30.0,
) -> dict[str, Any]:
    """
    Exchange refresh_token for a new access_token (and rotated refresh_token).
    Raises ValueError / httpx.HTTPError on failure.

    Pass a shared `client` when refreshing many accounts to avoid opening
    hundreds of TLS sessions at once (WSL/low-RAM friendly).
    """
    rt = entry.get("refresh_token")
    if not rt:
        raise ValueError("no refresh_token on account")
    client_id = (
        entry.get("oidc_client_id")
        or GROK_CLI_CLIENT_ID
    )
    form = {
        "grant_type": "refresh_token",
        "refresh_token": rt,
        "client_id": str(client_id),
    }
    headers = {"Content-Type": "application/x-www-form-urlencoded"}
    if client is not None:
        resp = client.post(OIDC_TOKEN_URL, data=form, headers=headers)
    else:
        with httpx.Client(timeout=timeout) as c:
            resp = c.post(OIDC_TOKEN_URL, data=form, headers=headers)
    if resp.status_code >= 400:
        raise ValueError(f"refresh failed {resp.status_code}: {resp.text[:400]}")
    data = resp.json()
    if not isinstance(data, dict) or not data.get("access_token"):
        raise ValueError("invalid refresh response")
    return data


def _account_refresh_lock(account_id: str) -> threading.Lock:
    with _refresh_locks_guard:
        lock = _refresh_locks.get(account_id)
        if lock is None:
            lock = threading.Lock()
            _refresh_locks[account_id] = lock
        return lock


def refresh_and_persist(
    account_id: str,
    entry: dict[str, Any],
    *,
    client: httpx.Client | None = None,
    persist: bool = True,
) -> dict[str, Any]:
    """
    Refresh one account under a per-account lock (multi-account safe).

    When `persist=False`, only performs the OIDC exchange and returns the new
    entry — caller is responsible for a single batched write (startup bulk
    refresh). This avoids rewriting a multi-MB auth.json once per account.
    """
    lock = _account_refresh_lock(account_id)
    with lock:
        # re-read latest entry — another thread may have just refreshed
        latest_map = read_auth_map()
        latest = latest_map.get(account_id)
        if not isinstance(latest, dict):
            # try by user_id
            uid = entry.get("user_id") or entry.get("principal_id")
            if uid:
                for k, v in latest_map.items():
                    if isinstance(v, dict) and (
                        v.get("user_id") == uid or v.get("principal_id") == uid
                    ):
                        latest = v
                        account_id = k
                        break
            if not isinstance(latest, dict):
                latest = entry
        token_data = refresh_access_token(latest, client=client)
        new_id, new_entry = entry_from_token_response(token_data, previous=latest)
        uid = new_entry.get("user_id")
        if uid:
            new_id = account_storage_id(user_id=str(uid))
        else:
            new_id = account_id
        if persist:
            upsert_entry(new_id, new_entry)
        return {"account_id": new_id, "entry": new_entry}


def ensure_fresh_entry(
    account_id: str,
    entry: dict[str, Any],
    *,
    skew_seconds: float = 120.0,
) -> dict[str, Any]:
    """Refresh if expired / near expiry and refresh_token exists."""
    token = entry.get("key")
    exp = parse_expires_at(entry.get("expires_at"), token if isinstance(token, str) else None)
    now = time.time()
    if exp is not None and exp > now + skew_seconds:
        return entry
    if not entry.get("refresh_token"):
        return entry
    try:
        result = refresh_and_persist(account_id, entry)
        return result["entry"]
    except Exception:
        return entry


# ── Device authorization flow ───────────────────────────────────────────────


def start_device_authorization(
    *,
    client_id: str | None = None,
    scopes: str | None = None,
) -> dict[str, Any]:
    """Start OIDC device flow; returns session for UI polling."""
    cid = client_id or GROK_CLI_CLIENT_ID
    scope = scopes or OIDC_SCOPES
    form = {"client_id": cid, "scope": scope}
    with httpx.Client(timeout=30.0) as client:
        resp = client.post(
            OIDC_DEVICE_URL,
            data=form,
            headers={"Content-Type": "application/x-www-form-urlencoded"},
        )
        if resp.status_code >= 400:
            return {
                "ok": False,
                "error": f"device code request failed {resp.status_code}: {resp.text[:400]}",
            }
        data = resp.json()

    device_code = data.get("device_code")
    user_code = data.get("user_code")
    if not device_code or not user_code:
        return {"ok": False, "error": f"unexpected device response: {data}"}

    session_id = uuid.uuid4().hex[:12]
    verification_url = (
        data.get("verification_uri_complete")
        or data.get("verification_uri")
        or "https://accounts.x.ai/oauth2/device"
    )
    interval = int(data.get("interval") or 5)
    expires_in = int(data.get("expires_in") or 1800)
    started = time.time()

    sess = {
        "id": session_id,
        "mode": "device_oidc",
        "status": "waiting_user",
        "device_code": device_code,
        "user_code": str(user_code).upper(),
        "verification_url": verification_url,
        "client_id": cid,
        "interval": max(3, interval),
        "expires_at": started + expires_in,
        "started_at": started,
        "finished_at": None,
        "message": (
            f"请在浏览器打开 {verification_url} ，输入设备码 {str(user_code).upper()}"
        ),
        "error": None,
        "output": json.dumps(data, ensure_ascii=False),
        "account_id": None,
        "email": None,
    }
    with _lock:
        _device_sessions[session_id] = sess

    # background poller
    t = threading.Thread(target=_device_poll_worker, args=(session_id,), daemon=True)
    t.start()

    return {
        "ok": True,
        "session_id": session_id,
        "user_code": sess["user_code"],
        "verification_url": verification_url,
        "status": "waiting_user",
        "message": sess["message"],
        "interval": sess["interval"],
        "expires_in": expires_in,
        "capture": True,
        "native_oidc": True,
        "command": f"OIDC device @ {OIDC_DEVICE_URL}",
    }


def _device_poll_worker(session_id: str) -> None:
    while True:
        with _lock:
            sess = _device_sessions.get(session_id)
            if not sess or sess.get("status") in ("success", "error", "expired"):
                return
            if time.time() > float(sess.get("expires_at") or 0):
                sess["status"] = "expired"
                sess["error"] = "device code expired"
                sess["message"] = "设备码已过期，请重新发起登录"
                sess["finished_at"] = time.time()
                return
            device_code = sess["device_code"]
            client_id = sess["client_id"]
            interval = int(sess.get("interval") or 5)

        form = {
            "grant_type": "urn:ietf:params:oauth:grant-type:device_code",
            "device_code": device_code,
            "client_id": client_id,
        }
        try:
            with httpx.Client(timeout=30.0) as client:
                resp = client.post(
                    OIDC_TOKEN_URL,
                    data=form,
                    headers={"Content-Type": "application/x-www-form-urlencoded"},
                )
                body_text = resp.text
                try:
                    body = resp.json()
                except Exception:
                    body = {}
        except Exception as e:  # noqa: BLE001
            with _lock:
                sess = _device_sessions.get(session_id)
                if sess:
                    sess["message"] = f"轮询网络异常，重试中: {e}"
            time.sleep(interval)
            continue

        err = body.get("error") if isinstance(body, dict) else None
        if resp.status_code == 200 and body.get("access_token"):
            try:
                account_id, entry = entry_from_token_response(body)
                # enrich email via userinfo if missing
                if not entry.get("email"):
                    try:
                        with httpx.Client(timeout=15.0) as client:
                            ui = client.get(
                                "https://auth.x.ai/oauth2/userinfo",
                                headers={"Authorization": f"Bearer {entry['key']}"},
                            )
                            if ui.status_code == 200:
                                u = ui.json()
                                if isinstance(u, dict):
                                    if u.get("email"):
                                        entry["email"] = u["email"]
                                    if u.get("given_name"):
                                        entry["first_name"] = u["given_name"]
                                    if u.get("family_name"):
                                        entry["last_name"] = u["family_name"]
                    except Exception:
                        pass
                upsert_entry(account_id, entry)
                with _lock:
                    sess = _device_sessions.get(session_id)
                    if sess:
                        sess["status"] = "success"
                        sess["message"] = f"登录成功: {entry.get('email') or account_id}"
                        sess["account_id"] = account_id
                        sess["email"] = entry.get("email")
                        sess["finished_at"] = time.time()
                        sess["output"] = (sess.get("output") or "") + "\n" + body_text[:500]
            except Exception as e:  # noqa: BLE001
                with _lock:
                    sess = _device_sessions.get(session_id)
                    if sess:
                        sess["status"] = "error"
                        sess["error"] = str(e)
                        sess["message"] = f"保存凭证失败: {e}"
                        sess["finished_at"] = time.time()
            return

        if err in ("authorization_pending", "slow_down"):
            if err == "slow_down":
                interval = min(interval + 5, 30)
                with _lock:
                    sess = _device_sessions.get(session_id)
                    if sess:
                        sess["interval"] = interval
            time.sleep(interval)
            continue

        if err == "expired_token":
            with _lock:
                sess = _device_sessions.get(session_id)
                if sess:
                    sess["status"] = "expired"
                    sess["error"] = err
                    sess["message"] = "设备码已过期，请重新发起登录"
                    sess["finished_at"] = time.time()
            return

        if err in ("access_denied", "access_denied"):
            with _lock:
                sess = _device_sessions.get(session_id)
                if sess:
                    sess["status"] = "error"
                    sess["error"] = err
                    sess["message"] = "用户拒绝授权"
                    sess["finished_at"] = time.time()
            return

        # other errors
        with _lock:
            sess = _device_sessions.get(session_id)
            if sess:
                # keep waiting on transient unknown if still 4xx authorization_pending style
                if resp.status_code in (400, 401) and err:
                    sess["status"] = "error"
                    sess["error"] = f"{err}: {body.get('error_description') or body_text[:200]}"
                    sess["message"] = sess["error"]
                    sess["finished_at"] = time.time()
                    return
                sess["message"] = f"等待授权… ({resp.status_code})"
        time.sleep(interval)


def get_device_session(session_id: str) -> dict[str, Any] | None:
    with _lock:
        sess = _device_sessions.get(session_id)
        if not sess:
            return None
        return {
            "session_id": sess["id"],
            "mode": sess.get("mode"),
            "status": sess.get("status"),
            "user_code": sess.get("user_code"),
            "verification_url": sess.get("verification_url"),
            "message": sess.get("message"),
            "error": sess.get("error"),
            "output_tail": (sess.get("output") or "")[-2000:],
            "started_at": sess.get("started_at"),
            "finished_at": sess.get("finished_at"),
            "account_id": sess.get("account_id"),
            "email": sess.get("email"),
            "ok": sess.get("status") in ("running", "waiting_user", "success"),
            "native_oidc": True,
        }


def list_device_sessions() -> list[dict[str, Any]]:
    with _lock:
        now = time.time()
        dead = [
            k
            for k, v in _device_sessions.items()
            if v.get("finished_at") and now - float(v["finished_at"]) > 3600
        ]
        for k in dead:
            _device_sessions.pop(k, None)
        return [get_device_session(k) for k in list(_device_sessions.keys()) if get_device_session(k)]


def refresh_all_accounts(
    *,
    only_near_expiry: bool = True,
    skew_seconds: float = 300.0,
    max_workers: int | None = None,
    max_accounts: int | None = None,
    account_ids: list[str] | None = None,
) -> dict[str, Any]:
    """
    Refresh accounts that have refresh_token (optionally only near expiry).

    Designed for large pools (hundreds of accounts):
      - bounded thread pool (default TOKEN_REFRESH_WORKERS)
      - shared httpx client per worker (no 1-client-per-request storm)
      - single batched auth.json write at the end (not one rewrite per account)
      - optional max_accounts cap so a cycle never tries all 700 at once
      - optional account_ids to refresh only selected accounts
    """
    from concurrent.futures import ThreadPoolExecutor, as_completed

    try:
        from config import TOKEN_REFRESH_BATCH, TOKEN_REFRESH_WORKERS, OIDC_NETWORK_TIMEOUT
    except Exception:
        TOKEN_REFRESH_WORKERS = 4
        TOKEN_REFRESH_BATCH = 40
        OIDC_NETWORK_TIMEOUT = 60.0

    if max_workers is None:
        max_workers = TOKEN_REFRESH_WORKERS
    if max_accounts is None:
        # Selected-account renew should not be silently truncated by the
        # background batch cap used for full-pool maintenance.
        max_accounts = None if account_ids else TOKEN_REFRESH_BATCH

    data = read_auth_map()
    results: list[dict[str, Any]] = []
    candidates: list[tuple[str, dict[str, Any]]] = []
    now = time.time()
    wanted: set[str] | None = None
    if account_ids is not None:
        wanted = {str(x).strip() for x in account_ids if str(x).strip()}
        if not wanted:
            return {
                "ok": True,
                "results": [],
                "refreshed": 0,
                "deferred": 0,
                "attempted": 0,
                "workers": 0,
                "selected": 0,
            }

    for aid, entry in list(data.items()):
        if not isinstance(entry, dict):
            continue
        if wanted is not None and aid not in wanted:
            continue
        if not entry.get("refresh_token"):
            results.append({"id": aid, "ok": False, "error": "no refresh_token"})
            continue
        token = entry.get("key")
        exp = parse_expires_at(
            entry.get("expires_at"), token if isinstance(token, str) else None
        )
        if only_near_expiry and exp is not None and exp > now + skew_seconds:
            results.append(
                {"id": aid, "ok": True, "skipped": True, "reason": "still_valid"}
            )
            continue
        candidates.append((aid, entry))

    if wanted is not None:
        existing = set(data.keys())
        for missing in sorted(wanted - existing):
            results.append({"id": missing, "ok": False, "error": "account_not_found"})

    # Prefer soonest-expiring accounts first when batch-capped
    def _exp_key(item: tuple[str, dict[str, Any]]) -> float:
        aid, entry = item
        token = entry.get("key")
        exp = parse_expires_at(
            entry.get("expires_at"), token if isinstance(token, str) else None
        )
        return float(exp) if exp is not None else 0.0

    candidates.sort(key=_exp_key)
    deferred = 0
    if max_accounts and len(candidates) > max_accounts:
        deferred = len(candidates) - max_accounts
        for aid, _ in candidates[max_accounts:]:
            results.append(
                {
                    "id": aid,
                    "ok": True,
                    "skipped": True,
                    "reason": "batch_deferred",
                }
            )
        candidates = candidates[:max_accounts]

    updates: dict[str, dict[str, Any]] = {}
    updates_lock = threading.Lock()

    def _refresh_one(item: tuple[str, dict[str, Any]]) -> dict[str, Any]:
        aid, entry = item
        # One short-lived client per worker task is still better than unbounded
        # fan-out; pool size is already capped by max_workers.
        try:
            with httpx.Client(timeout=OIDC_NETWORK_TIMEOUT) as client:
                r = refresh_and_persist(aid, entry, client=client, persist=False)
            with updates_lock:
                updates[r["account_id"]] = r["entry"]
                # Drop old key if remounted to a different storage id
                if r["account_id"] != aid:
                    updates.setdefault("__delete__", {})  # type: ignore[arg-type]
            return {
                "id": r["account_id"],
                "ok": True,
                "email": r["entry"].get("email"),
                "expires_at": r["entry"].get("expires_at"),
            }
        except Exception as e:  # noqa: BLE001
            return {"id": aid, "ok": False, "error": str(e)[:300]}

    workers = max(1, min(int(max_workers or 1), max(1, len(candidates))))
    if candidates:
        with ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="tok-refresh-"
        ) as ex:
            futs = [ex.submit(_refresh_one, c) for c in candidates]
            for fut in as_completed(futs):
                try:
                    results.append(fut.result())
                except Exception as e:  # noqa: BLE001
                    results.append({"id": "?", "ok": False, "error": str(e)[:300]})

    # Single batched write for all successful refreshes
    if updates:
        def _apply(m: dict[str, Any]) -> None:
            for aid, entry in updates.items():
                if aid == "__delete__" or not isinstance(entry, dict):
                    continue
                # remove any other keys with same user_id / token to avoid dupes
                uid = entry.get("user_id") or entry.get("principal_id")
                token = entry.get("key")
                for k in list(m.keys()):
                    if k == aid:
                        continue
                    v = m.get(k)
                    if not isinstance(v, dict):
                        continue
                    same_user = bool(
                        uid
                        and (v.get("user_id") == uid or v.get("principal_id") == uid)
                    )
                    same_token = bool(token and v.get("key") == token)
                    if same_user or same_token:
                        del m[k]
                m[aid] = entry

        try:
            mutate_auth_map(_apply)
        except Exception as e:  # noqa: BLE001
            return {
                "ok": False,
                "error": f"batch write failed: {e}"[:400],
                "results": results,
                "refreshed": 0,
                "deferred": deferred,
                "attempted": len(candidates),
            }

    out = {
        "ok": True,
        "results": results,
        "refreshed": sum(1 for r in results if r.get("ok") and not r.get("skipped")),
        "deferred": deferred,
        "attempted": len(candidates),
        "workers": workers,
    }
    if wanted is not None:
        out["selected"] = len(wanted)
    return out
