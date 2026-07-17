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

import os
import secrets
import sys
from pathlib import Path
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
        "sso": True,  # SSO handlers import admin helpers lazily
        "captcha_provider": captcha_provider,
        "local_solver_url": local_solver if captcha_provider == "local" else None,
        "endpoints": {
            "registration": API_PREFIX,
            "sso": "/internal/sso/v1",
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


@app.get(f"{API_PREFIX}/availability")
def availability(request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return adapter.registration_available()


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
        )
        if k in body
    }
    result = adapter.start_registration(**kwargs)
    if not isinstance(result, dict):
        raise HTTPException(status_code=500, detail="invalid registration response")
    if result.get("ok") is False:
        raise HTTPException(status_code=400, detail=str(result.get("error") or "registration failed"))
    return result


@app.get(f"{API_PREFIX}/sessions")
def list_sessions(request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return adapter.list_registration_sessions()


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
    return sess


@app.post(f"{API_PREFIX}/sessions/{{session_id}}/stop")
def stop_session(session_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return adapter.stop_registration_session(session_id)


@app.get(f"{API_PREFIX}/batches/{{batch_id}}")
def get_batch(batch_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    batch = adapter.get_registration_batch(batch_id)
    if not batch:
        raise HTTPException(status_code=404, detail="batch not found")
    return batch


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
    return adapter.resume_registration_batch(batch_id, force=force)


@app.post(f"{API_PREFIX}/batches/{{batch_id}}/stop")
def stop_batch(batch_id: str, request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return adapter.stop_registration_batch(batch_id)


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


@app.post(f"{API_PREFIX}/stop")
def stop_all(request: Request) -> dict[str, Any]:
    _require_auth(request)
    adapter = _adapter()
    return adapter.stop_all_active_registrations()


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

    # Reuse admin_routes helpers so conversion stays in original language/script path.
    try:
        from grok2api.admin import admin_routes as ar
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
    workers = min(int(max_workers), int(SSO_IMPORT_WORKERS), max(1, len(sso_items)))
    if delay and delay >= 5:
        workers = min(workers, 4)

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
        from grok2api.admin import admin_routes as ar
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
