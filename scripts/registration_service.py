#!/usr/bin/env python3
"""Internal registration + SSO + captcha sidecar for the Go main process.

Public API traffic must not hit this service. Go admin facades call:

  /internal/registration/v1/*   registration machine (mailbox + captcha + device)
  /internal/sso/v1/*            SSO cookie conversion jobs

Python owns:
  - registration orchestration (grok2api.upstream.grok_build_adapter)
  - mailbox providers
  - Turnstile solving via local solver / YesCaptcha
  - SSO conversion scripts/helpers (scripts/sso_to_auth_json.py)

Captcha browser pool itself is the sibling process turnstile-solver
(started by entrypoint.sh). This service only consumes it.
"""

from __future__ import annotations

import asyncio
import os
import secrets
import sys
import threading
import time
from pathlib import Path
import json
from typing import Any

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from fastapi import FastAPI, Header, HTTPException, Request
from fastapi.responses import JSONResponse

try:
    from grok2api.upstream import grok_build_adapter as reg
except Exception as exc:  # noqa: BLE001
    reg = None  # type: ignore[assignment]
    _IMPORT_ERROR = str(exc)
else:
    _IMPORT_ERROR = None


app = FastAPI(title="grok2api registration internal API", version="1.0.0")
API_PREFIX = "/internal/registration/v1"


def _require_auth(request: Request) -> None:
    expected = (os.environ.get("GROK2API_REGISTRATION_TOKEN") or "").strip()
    if not expected:
        return
    auth = (request.headers.get("authorization") or "").strip()
    if not auth.lower().startswith("bearer "):
        raise HTTPException(status_code=401, detail="registration token required")
    token = auth[7:].strip()
    if not secrets.compare_digest(token, expected):
        raise HTTPException(status_code=401, detail="invalid registration token")


def _adapter():
    if reg is None:
        raise HTTPException(
            status_code=503,
            detail=f"registration adapter unavailable: {_IMPORT_ERROR or 'import failed'}",
        )
    return reg


@app.get("/health")
def health() -> dict[str, Any]:
    """Sidecar liveness + lightweight captcha/registration readiness."""
    captcha_provider = (
        os.environ.get("GROK2API_CAPTCHA_PROVIDER")
        or os.environ.get("CAPTCHA_PROVIDER")
        or "local"
    ).strip().lower()
    local_solver = (
        os.environ.get("GROK2API_LOCAL_SOLVER_URL")
        or os.environ.get("LOCAL_SOLVER_URL")
        or "http://127.0.0.1:5072"
    ).strip().rstrip("/")
    out: dict[str, Any] = {
        "ok": reg is not None,
        "service": "registration-sso-sidecar",
        "adapter_error": _IMPORT_ERROR,
        "registration": reg is not None,
        "sso": True,  # SSO handlers import sso_import helpers lazily
        "captcha_provider": captcha_provider,
        "local_solver_url": local_solver if captcha_provider == "local" else None,
        "endpoints": {
            "registration": API_PREFIX,
            "sso": "/internal/sso/v1", "device": "/internal/device/v1",
        },
    }
    if reg is not None:
        try:
            avail = reg.registration_available()
            if isinstance(avail, dict):
                out["registration_available"] = avail
        except Exception as exc:  # noqa: BLE001
            out["registration_available_error"] = str(exc)[:300]
    return out



def _jsonable(value: Any, *, depth: int = 0) -> Any:
    """Best-effort JSON sanitizer for adapter responses.

    Registration sessions keep process-local objects under `_receiver` etc.
    If any leak into the response, FastAPI/Pydantic raises 500.
    """
    if depth > 8:
        return None
    if value is None or isinstance(value, (str, int, float, bool)):
        return value
    if isinstance(value, dict):
        out: dict[str, Any] = {}
        for k, v in value.items():
            if not isinstance(k, str):
                continue
            if k.startswith("_"):
                continue
            if callable(v):
                continue
            out[k] = _jsonable(v, depth=depth + 1)
        return out
    if isinstance(value, (list, tuple, set)):
        return [_jsonable(v, depth=depth + 1) for v in value]
    # Drop unknown objects (mail receivers, clients, threads, ...).
    try:
        json.dumps(value)
        return value
    except Exception:
        return str(value)


@app.get(f"{API_PREFIX}/availability")
def availability(request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return adapter.registration_available()


@app.post(f"{API_PREFIX}/mail/domains")
async def list_mail_domains(request: Request) -> dict[str, Any]:
    """List selectable domains for YYDS / GPTMail / CF Temp Email / TempMail.lol / Cloud Mail / MoeMail."""
    _require_auth(request)
    try:
        body = await request.json()
    except Exception:
        body = {}
    if not isinstance(body, dict):
        body = {}
    try:
        from grok2api.upstream import moemail as mail
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"moemail module unavailable: {exc}") from exc

    prov = mail.normalize_mail_provider(
        body.get("mail_provider") or body.get("provider"),
        base_url=str(body.get("base_url") or body.get("moemail_base_url") or body.get("cfmail_base_url") or body.get("cloudmail_base_url") or ""),
    )
    key = str(
        body.get("api_key")
        or body.get("moemail_api_key")
        or body.get("yyds_api_key")
        or body.get("gptmail_api_key")
        or body.get("cfmail_api_key")
        or body.get("tempmail_api_key")
        or body.get("cloudmail_api_key")
        or ""
    ).strip()
    base = str(
        body.get("base_url")
        or body.get("moemail_base_url")
        or body.get("cfmail_base_url")
        or body.get("cloudmail_base_url")
        or ""
    ).strip()
    domains: list[str] = []
    note = ""
    base_out = base
    try:
        if prov == "yyds":
            domains = mail.yyds_list_domains(api_key=key or None, base_url=base or None)
            note = "YYDS GET /v1/domains"
            base_out = mail.normalize_yyds_base_url(base or None)
        elif prov == "gptmail":
            domains = mail.gptmail_list_domains(api_key=key or None, base_url=base or None)
            note = "GPTMail GET /api/domains/public"
            base_out = mail.normalize_gptmail_base_url(base or None)
        elif prov == "cfmail":
            domains = mail.cfmail_list_domains(api_key=key or None, base_url=base or None)
            note = "CF Temp Email GET /open_api/settings"
            base_out = (base or mail.CFMAIL_DEFAULT_BASE_URL).rstrip("/")
        elif prov == "tempmail":
            domains = mail.tempmail_list_domains(api_key=key or None, base_url=base or None)
            note = "TempMail.lol free: random domain (no catalog)"
            base_out = mail.normalize_tempmail_base_url(base or None)
        elif prov == "cloudmail":
            domains = mail.cloudmail_list_domains(api_key=key or None, base_url=base or None)
            note = "Cloud Mail GET /api/setting/websiteConfig"
            base_out = mail.normalize_cloudmail_base_url(base or None)
        else:
            # MoeMail has no universal public catalog; return configured domain list.
            domains = mail.parse_domain_list(str(body.get("domain") or body.get("moemail_domain") or ""))
            note = "MoeMail: no public catalog; showing configured domains only"
            base_out = base
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=str(exc)) from exc
    return {
        "ok": True,
        "mail_provider": prov,
        "base_url": base_out,
        "domains": domains,
        "count": len(domains),
        "note": note,
    }


@app.post(f"{API_PREFIX}/jobs")
async def start_job(
    request: Request,
    idempotency_key: str | None = Header(default=None, alias="Idempotency-Key"),
) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    try:
        body = await request.json()
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"invalid JSON: {exc}") from exc
    if not isinstance(body, dict):
        raise HTTPException(status_code=400, detail="body must be object")
    # Idempotency key is accepted for contract compatibility; adapter currently
    # relies on its own session/batch ids.
    _ = idempotency_key
    # Accept full body keys then resolve active mail credentials into the
    # historical moemail_api_key / moemail_base_url fields the adapter reads.
    kwargs = {
        k: body.get(k)
        for k in (
            "captcha_provider",
            "local_solver_url",
            "yescaptcha_key",
            "proxy",
            "proxy_username",
            "proxy_password",
            "proxy_strategy",
            "moemail_api_key",
            "moemail_base_url",
            "prefix",
            "domain",
            "expiry_ms",
            "mail_provider",
            "count",
            "concurrency",
            "stagger_ms",
            "probe_delay_sec",
            "yyds_api_key",
            "gptmail_api_key",
            "cfmail_api_key",
            "tempmail_api_key",
            "cloudmail_api_key",
            "yyds_domain",
            "gptmail_domain",
            "cfmail_domain",
            "tempmail_domain",
            "cloudmail_domain",
            "moemail_domain",
            "cfmail_base_url",
            "cloudmail_base_url",
            "api_key",
            "base_url",
        )
        if k in body
    }
    try:
        from grok2api.upstream.moemail import normalize_mail_provider as _nmp

        prov = _nmp(
            kwargs.get("mail_provider"),
            base_url=str(kwargs.get("moemail_base_url") or kwargs.get("cfmail_base_url") or kwargs.get("cloudmail_base_url") or kwargs.get("base_url") or ""),
        )
    except Exception:
        prov = str(kwargs.get("mail_provider") or "moemail").strip().lower() or "moemail"
    kwargs["mail_provider"] = prov
    # Active domain from provider-specific slot.
    dom = str(kwargs.get("domain") or "").strip()
    if prov == "yyds":
        dom = str(kwargs.get("yyds_domain") or dom).strip()
        key = str(kwargs.get("yyds_api_key") or kwargs.get("api_key") or "").strip()
        # Prefer dedicated slot; only fall back to moemail_api_key if it looks like YYDS.
        if not key:
            mk = str(kwargs.get("moemail_api_key") or "").strip()
            if mk.startswith("AC-"):
                key = mk
        kwargs["moemail_api_key"] = key
        kwargs["moemail_base_url"] = ""  # fixed host
        kwargs["domain"] = dom
        if not key:
            raise HTTPException(
                status_code=400,
                detail="YYDS API Key missing. Save AC-… key in 协议注册配置 (YYDS panel).",
            )
    elif prov == "gptmail":
        dom = str(kwargs.get("gptmail_domain") or dom).strip()
        key = str(kwargs.get("gptmail_api_key") or kwargs.get("api_key") or "").strip()
        # Empty key is OK only if a real sk-… is later provided; reject other shapes.
        if key.startswith("mk_") or key.startswith("AC-"):
            key = ""
        kwargs["moemail_api_key"] = key
        kwargs["moemail_base_url"] = ""
        kwargs["domain"] = dom
        if not key:
            raise HTTPException(
                status_code=400,
                detail="GPTMail API Key missing. Save sk-… from https://mail.chatgpt.org.uk/zh/api/",
            )
    elif prov == "cfmail":
        dom = str(kwargs.get("cfmail_domain") or dom).strip()
        key = str(kwargs.get("cfmail_api_key") or kwargs.get("api_key") or kwargs.get("moemail_api_key") or "").strip()
        base = str(kwargs.get("cfmail_base_url") or kwargs.get("base_url") or kwargs.get("moemail_base_url") or "").strip()
        kwargs["moemail_api_key"] = key
        kwargs["moemail_base_url"] = base
        kwargs["domain"] = dom
        if not key:
            raise HTTPException(
                status_code=400,
                detail="CF Temp Email admin password/key missing.",
            )
        if not base:
            raise HTTPException(
                status_code=400,
                detail="CF Temp Email Base URL missing.",
            )
    elif prov == "cloudmail":
        # Cloud Mail (maillab/cloud-mail): admin_email:password credential +
        # self-hosted Worker origin. Adapter reads moemail_api_key /
        # moemail_base_url as the active mail slots.
        dom = str(kwargs.get("cloudmail_domain") or dom).strip()
        key = str(
            kwargs.get("cloudmail_api_key")
            or kwargs.get("api_key")
            or kwargs.get("moemail_api_key")
            or ""
        ).strip()
        base = str(
            kwargs.get("cloudmail_base_url")
            or kwargs.get("base_url")
            or kwargs.get("moemail_base_url")
            or ""
        ).strip()
        kwargs["moemail_api_key"] = key
        kwargs["moemail_base_url"] = base
        kwargs["domain"] = dom
        if not key:
            raise HTTPException(
                status_code=400,
                detail=(
                    "Cloud Mail admin credentials missing. Save "
                    "`admin_email:password` in 协议注册配置 (Cloud Mail panel). "
                    "Repo: https://github.com/maillab/cloud-mail"
                ),
            )
        if not base:
            raise HTTPException(
                status_code=400,
                detail=(
                    "Cloud Mail Base URL missing. Save self-hosted Worker origin "
                    "(e.g. https://skymail.example.com) in 协议注册配置 (Cloud Mail panel)."
                ),
            )
    elif prov == "tempmail":
        # Free tier: no API key required. Optional Plus/Ultra Bearer key.
        dom = str(kwargs.get("tempmail_domain") or dom).strip()
        key = str(kwargs.get("tempmail_api_key") or kwargs.get("api_key") or "").strip()
        kwargs["moemail_api_key"] = key
        kwargs["moemail_base_url"] = ""
        kwargs["domain"] = dom
    else:
        key = str(kwargs.get("moemail_api_key") or kwargs.get("api_key") or "").strip()
        base = str(kwargs.get("moemail_base_url") or kwargs.get("base_url") or "").strip()
        dom = str(kwargs.get("moemail_domain") or dom).strip()
        # Reject cross-provider pollution (YYDS AC-* / GPTMail sk-* in MoeMail slot).
        if key.startswith("AC-") or key.lower().startswith("sk-"):
            key = ""
        kwargs["moemail_api_key"] = key
        kwargs["moemail_base_url"] = base
        kwargs["domain"] = dom
        if not key:
            raise HTTPException(
                status_code=400,
                detail=(
                    "MoeMail API Key missing or invalid (got YYDS/GPTMail-shaped key). "
                    "Re-save MoeMail Key (mk_…) in 协议注册配置 — switching providers "
                    "must not overwrite the MoeMail slot."
                ),
            )
        if not base:
            raise HTTPException(
                status_code=400,
                detail="MoeMail Base URL missing. Re-save MoeMail Base URL in 协议注册配置.",
            )
    # Drop non-adapter kwargs
    for drop in (
        "yyds_api_key", "gptmail_api_key", "cfmail_api_key", "tempmail_api_key", "cloudmail_api_key",
        "yyds_domain", "gptmail_domain", "cfmail_domain", "tempmail_domain", "cloudmail_domain", "moemail_domain",
        "cfmail_base_url", "cloudmail_base_url", "api_key", "base_url",
    ):
        kwargs.pop(drop, None)
    # start_registration is sync and can block for seconds (local solver wait,
    # mailbox prepare, brief batch warm-up sleep). Running it on the asyncio
    # event loop freezes the whole sidecar — concurrent /sessions and /stop
    # then miss Go's 750ms poll budget and surface as admin 502s. Offload so
    # poll/stop handlers keep answering while start runs in a worker thread.
    try:
        result = await asyncio.to_thread(adapter.start_registration, **kwargs)
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"registration start failed: {exc}") from exc
    if not isinstance(result, dict):
        raise HTTPException(status_code=500, detail="invalid registration response")
    if result.get("ok") is False:
        raise HTTPException(status_code=400, detail=str(result.get("error") or "registration failed"))
    return _jsonable(result)


# --- Bounded /sessions response.
# Registration sessions accumulate in the Redis mirror (REG_TTL=72h) and
# list_registration_sessions() re-merges ALL of them and rebuilds one object per
# session on every poll -> 2879 sessions => ~9-11MB payload / ~1.3s build that
# timed out the polling client (Go Timeout=750ms / ResponseHeaderTimeout=600ms),
# making the UI "refresh progress" appear dead.
# Fixes:
#   (a) prune old *terminal* sessions from Redis in a BACKGROUND thread so the
#       rebuild stays small, without blocking the request path past the 600ms
#       header budget (cold full-delete of ~3k keys was ~1.5s);
#   (b) cache the bounded result briefly so repeated polls return instantly;
#   (c) hard-cap the returned list so body stays well under the 4MB read limit.
# ACTIVE (non-terminal) sessions are never pruned, so in-flight work is safe.
_LIST_CACHE: dict[str, Any] = {"t": 0.0, "data": None}
_LIST_CACHE_TTL = 2.0
_PRUNE_LAST = {"t": 0.0}
_PRUNE_INTERVAL = 30.0
_PRUNE_TERMINAL_TTL = 900.0  # keep terminal sessions in Redis <= 15 min
_PRUNE_MAX_PER_CYCLE = 300   # bound cold-path work so one cycle never hog Redis
_PRUNE_STATE = {"running": False}
_PRUNE_LOCK = threading.Lock()
_TERMINAL_STATUSES = frozenset({
    "success", "completed", "done", "failed", "error", "stopped",
    "abandoned", "expired", "imported", "cancelled", "canceled",
    "timed_out", "timeout",
})


def _prune_redis_sessions_once() -> int:
    """Delete up to _PRUNE_MAX_PER_CYCLE old terminal sessions. Returns count."""
    deleted = 0
    try:
        from grok2api.store import sessions_redis
        if not getattr(sessions_redis, "redis_enabled", lambda: False)():
            return 0
        now = time.time()
        cutoff = now - _PRUNE_TERMINAL_TTL
        for s in (sessions_redis.reg_sess_list() or []):
            if deleted >= _PRUNE_MAX_PER_CYCLE:
                break
            if not isinstance(s, dict):
                continue
            st = str(s.get("status", "")).lower()
            if st not in _TERMINAL_STATUSES and not s.get("finished"):
                continue  # never prune active sessions
            ua = float(s.get("updated_at") or s.get("created_at") or 0)
            if ua and ua < cutoff:
                sid = str(s.get("id") or "")
                if sid:
                    sessions_redis.reg_sess_delete(sid)
                    deleted += 1
    except Exception:
        pass
    return deleted


def _prune_worker() -> None:
    try:
        _prune_redis_sessions_once()
    finally:
        with _PRUNE_LOCK:
            _PRUNE_STATE["running"] = False


def _kick_prune_background() -> None:
    """Non-blocking: fire prune on a daemon thread if interval elapsed."""
    now = time.time()
    if now - _PRUNE_LAST["t"] < _PRUNE_INTERVAL:
        return
    with _PRUNE_LOCK:
        if _PRUNE_STATE["running"]:
            return
        _PRUNE_STATE["running"] = True
        _PRUNE_LAST["t"] = now
    t = threading.Thread(
        target=_prune_worker,
        name="reg-sess-prune",
        daemon=True,
    )
    t.start()


@app.get(f"{API_PREFIX}/sessions")
def list_sessions(request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    # Kick prune OFF the request path so ResponseHeaderTimeout(600ms) is safe
    # even when Redis still holds thousands of terminal rows.
    _kick_prune_background()
    now = time.time()
    if _LIST_CACHE["data"] is not None and (now - _LIST_CACHE["t"]) < _LIST_CACHE_TTL:
        return _jsonable(_LIST_CACHE["data"])
    data = adapter.list_registration_sessions()
    if isinstance(data, dict) and "sessions" in data:
        sessions = data.get("sessions") or []
        try:
            sessions = sorted(
                sessions,
                key=lambda s: (s.get("updated_at") or 0),
                reverse=True,
            )
        except Exception:
            pass
        terminal = _TERMINAL_STATUSES
        active = [s for s in sessions
                  if str(s.get("status", "")).lower() not in terminal]
        finished = [s for s in sessions
                    if str(s.get("status", "")).lower() in terminal]
        KEEP_TERMINAL = 150
        HARD_CAP = 250
        # Prefer active (UI progress); append recent terminal; hard-cap total.
        # If active alone exceeds HARD_CAP (shouldn't with REG_CONCURRENCY=3),
        # keep the newest active first so live work still shows up.
        bounded = active + finished[:KEEP_TERMINAL]
        if len(bounded) > HARD_CAP:
            bounded = bounded[:HARD_CAP]
        total = len(sessions)
        result = {
            "sessions": bounded,
            "total": total,
            "returned": len(bounded),
            "truncated": total != len(bounded),
        }
    else:
        result = data
    _LIST_CACHE["data"] = result
    _LIST_CACHE["t"] = time.time()
    return _jsonable(result)


@app.get(f"{API_PREFIX}/sessions/{{session_id}}")
def get_session(session_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    include_auth = (request.query_params.get("include_auth_json") or "").strip() in {
        "1",
        "true",
        "yes",
    }
    sess = adapter.get_registration_session(session_id, include_auth_json=include_auth)
    if not sess:
        raise HTTPException(status_code=404, detail="session not found")
    return _jsonable(sess)


@app.post(f"{API_PREFIX}/sessions/{{session_id}}/stop")
def stop_session(session_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return _jsonable(adapter.stop_registration_session(session_id))


@app.get(f"{API_PREFIX}/batches/{{batch_id}}")
def get_batch(batch_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    batch = adapter.get_registration_batch(batch_id)
    if not batch:
        raise HTTPException(status_code=404, detail="batch not found")
    return _jsonable(batch)


@app.post(f"{API_PREFIX}/batches/{{batch_id}}/resume")
async def resume_batch(batch_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    force = False
    try:
        body = await request.json()
        if isinstance(body, dict):
            force = bool(body.get("force"))
    except Exception:
        force = False
    try:
        result = await asyncio.to_thread(
            adapter.resume_registration_batch, batch_id, force=force
        )
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"registration resume failed: {exc}") from exc
    return _jsonable(result)


@app.post(f"{API_PREFIX}/batches/{{batch_id}}/stop")
def stop_batch(batch_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return _jsonable(adapter.stop_registration_batch(batch_id))


@app.post(f"{API_PREFIX}/reclaim")
async def reclaim(request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    auto_resume = True
    try:
        body = await request.json()
        if isinstance(body, dict) and "auto_resume" in body:
            auto_resume = bool(body.get("auto_resume"))
    except Exception:
        pass

    def _reclaim_sync() -> Any:
        # Prefer batch reclaim which also reclaims sessions.
        fn = getattr(adapter, "reclaim_orphaned_registration_batches", None)
        if callable(fn):
            # signature may not take auto_resume; call best-effort
            try:
                return fn(auto_resume=auto_resume)  # type: ignore[misc]
            except TypeError:
                return fn()
        fn2 = getattr(adapter, "reclaim_orphaned_registration_sessions", None)
        if callable(fn2):
            return fn2()
        return {"ok": True, "reclaimed": 0}

    try:
        result = await asyncio.to_thread(_reclaim_sync)
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"registration reclaim failed: {exc}") from exc
    return _jsonable(result)


@app.post(f"{API_PREFIX}/stop")
def stop_all(request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return _jsonable(adapter.stop_all_active_registrations())


# ---------------------------------------------------------------------------
# Device-code login (Python-owned OIDC). Go admin proxies these endpoints.
# ---------------------------------------------------------------------------
DEVICE_PREFIX = "/internal/device/v1"


@app.post(f"{DEVICE_PREFIX}/login")
async def device_login_start(request: Request) -> dict[str, Any]:
    """Start native OIDC device-code login (no Grok CLI required)."""
    _require_auth(request)
    mode = "device"
    capture = True
    try:
        body = await request.json()
        if isinstance(body, dict):
            mode = str(body.get("mode") or "device")
            if "capture" in body:
                capture = bool(body.get("capture"))
    except Exception:
        pass
    try:
        from grok2api.pool.accounts import start_login
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail=f"device login unavailable: {exc}") from exc
    try:
        result = await asyncio.to_thread(start_login, mode=mode, capture=capture)
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"device login failed: {exc}") from exc
    if not isinstance(result, dict):
        raise HTTPException(status_code=500, detail="invalid device login response")
    if result.get("ok") is False:
        raise HTTPException(status_code=400, detail=str(result.get("error") or "device login failed"))
    return _jsonable(result)


@app.get(f"{DEVICE_PREFIX}/sessions/{{session_id}}")
def device_login_session(session_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    try:
        from grok2api.pool.accounts import get_login_session
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail=f"device login unavailable: {exc}") from exc
    sess = get_login_session(session_id)
    if not sess:
        raise HTTPException(status_code=404, detail="device session not found")
    # Strip secrets from poll response.
    out = dict(sess) if isinstance(sess, dict) else {}
    out.pop("device_code", None)
    out.pop("password", None)
    out.pop("access_token", None)
    out.pop("refresh_token", None)
    out.pop("key", None)
    return _jsonable(out)


@app.get(f"{DEVICE_PREFIX}/sessions")
def device_login_sessions(request: Request) -> dict[str, Any]:
    _require_auth(request)
    try:
        from grok2api.pool.accounts import list_login_sessions
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail=f"device login unavailable: {exc}") from exc
    items = list_login_sessions() or []
    cleaned = []
    for sess in items:
        if not isinstance(sess, dict):
            continue
        row = dict(sess)
        row.pop("device_code", None)
        row.pop("password", None)
        row.pop("access_token", None)
        row.pop("refresh_token", None)
        row.pop("key", None)
        cleaned.append(row)
    return _jsonable({"ok": True, "sessions": cleaned, "count": len(cleaned)})


# ---------------------------------------------------------------------------
# SSO conversion (Python-owned). Go admin only proxies these endpoints.
# ---------------------------------------------------------------------------
SSO_PREFIX = "/internal/sso/v1"


@app.post(f"{SSO_PREFIX}/import")
async def sso_import_start(request: Request) -> dict[str, Any]:
    """Start async SSO cookie import using existing Python helpers/scripts."""
    _require_auth(request)
    try:
        body = await request.json()
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"invalid JSON: {exc}") from exc
    if not isinstance(body, dict):
        raise HTTPException(status_code=400, detail="body must be object")

    # Reuse sso_import helpers so conversion stays in original language/script path.
    try:
        from grok2api.admin import sso_import as ar
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail=f"sso import helpers unavailable: {exc}") from exc

    cookies = body.get("sso_cookies") or body.get("cookies") or []
    if isinstance(cookies, str):
        cookies = [cookies]
    if not isinstance(cookies, list):
        raise HTTPException(status_code=400, detail="sso_cookies must be list or string")
    sso_items = ar._parse_sso_lines([str(x) for x in cookies])
    if not sso_items:
        raise HTTPException(status_code=400, detail="No valid SSO cookies provided")

    merge = bool(body.get("merge", True))
    try:
        delay = int(body.get("delay") or 0)
    except Exception:
        delay = 0
    try:
        max_workers = int(body.get("max_workers") or 8)
    except Exception:
        max_workers = 8

    import threading
    import time
    import uuid

    try:
        from grok2api.config import SSO_IMPORT_WORKERS
    except Exception:
        SSO_IMPORT_WORKERS = 8
    workers = min(int(max_workers), int(SSO_IMPORT_WORKERS), max(1, len(sso_items)), 12)
    if delay and delay >= 2:
        workers = min(workers, 2)

    job_id = f"sso_{uuid.uuid4().hex[:16]}"
    now = time.time()
    job = {
        "id": job_id,
        "status": "queued",
        "phase": "queued",
        "message": f"已排队，共 {len(sso_items)} 条 SSO",
        "total": len(sso_items),
        "done": 0,
        "success": 0,
        "fail": 0,
        "converted": 0,
        "percent": 0,
        "workers": workers,
        "delay": int(delay or 0),
        "merge": bool(merge),
        "created_at": now,
        "updated_at": now,
        "finished_at": None,
        "results": [],
        "imported": [],
        "error": None,
        "ok": None,
    }
    ar._sso_job_put(job_id, job)
    t = threading.Thread(
        target=ar._run_sso_import_job,
        kwargs={
            "job_id": job_id,
            "sso_items": sso_items,
            "merge": bool(merge),
            "delay": int(delay or 0),
            "max_workers": int(max_workers or workers),
        },
        daemon=True,
        name=f"sso-import-job-{job_id[-8:]}",
    )
    t.start()
    return {
        "ok": True,
        "async": True,
        "job_id": job_id,
        "status": "queued",
        "total": len(sso_items),
        "workers": workers,
        "delay": int(delay or 0),
        "message": f"SSO 导入已启动（{len(sso_items)} 条，workers={workers}）",
        "poll_url": f"/admin/api/accounts/import-sso/jobs/{job_id}",
    }


@app.get(f"{SSO_PREFIX}/jobs/{{job_id}}")
def sso_import_job(job_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    try:
        from grok2api.admin import sso_import as ar
    except Exception as exc:  # noqa: BLE001
        raise HTTPException(status_code=503, detail=f"sso import helpers unavailable: {exc}") from exc
    job = ar._sso_job_get(str(job_id or "").strip())
    if not job:
        raise HTTPException(status_code=404, detail="SSO import job not found")
    return ar._sso_public_job(job)


@app.exception_handler(HTTPException)
async def http_error_handler(_: Request, exc: HTTPException) -> JSONResponse:
    return JSONResponse(status_code=exc.status_code, content={"detail": exc.detail})


def main() -> None:
    import uvicorn

    host = os.environ.get("GROK2API_REGISTRATION_HOST", "127.0.0.1")
    port = int(os.environ.get("GROK2API_REGISTRATION_PORT", "18070") or 18070)
    uvicorn.run(
        "scripts.registration_service:app",
        host=host,
        port=port,
        log_level=os.environ.get("GROK2API_REGISTRATION_LOG", "info"),
        factory=False,
    )


if __name__ == "__main__":
    # Support both `python scripts/registration_service.py` and module import.
    import uvicorn

    host = os.environ.get("GROK2API_REGISTRATION_HOST", "127.0.0.1")
    port = int(os.environ.get("GROK2API_REGISTRATION_PORT", "18070") or 18070)
    uvicorn.run(app, host=host, port=port, log_level="info")
