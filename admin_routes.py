"""Admin API routes: setup, login, keys, accounts, pool, quota, models, status."""

from __future__ import annotations

import asyncio
import json
import threading
import time
import uuid
from typing import Any

from fastapi import Response, APIRouter, File, Form, Header, HTTPException, Request, UploadFile
from fastapi.responses import JSONResponse, Response
from pydantic import BaseModel, Field

import account_pool
import accounts
import apikeys
import conversation_affinity
import model_health
import quota
import token_maintainer
from auth import AuthError, load_credentials, load_credentials_by_id
import config as _config
import sso_to_auth_json as sso_import

# Registration adapter: dongguatanglinux/grok-build-auth protocol client.
try:
    import grok_build_adapter as reg_adapter
except Exception as _reg_import_err:  # noqa: BLE001
    reg_adapter = None  # type: ignore[assignment]
    _REG_IMPORT_ERROR = str(_reg_import_err)
else:
    _REG_IMPORT_ERROR = None
from config import (
    CLI_VERSION,
    DEFAULT_MODEL,
    PUBLIC_BASE_URL,
    REQUIRE_API_KEY,
    UPSTREAM_BASE,
)
from models import load_models_from_cache, resolve_model, sync_models_from_upstream
from settings_store import (
    VALID_ACCOUNT_MODES,
    change_admin_password,
    create_session_token,
    get_account_mode,
    get_public_settings,
    get_registration_config,
    is_setup_needed,
    resolve_registration_inputs,
    revoke_session,
    set_account_mode,
    set_model_health_enabled,
    set_registration_config,
    set_token_maintain_enabled,
    set_admin_password,
    update_runtime_settings,
    verify_admin_password,
    verify_session_token,
)

router = APIRouter(prefix="/admin/api", tags=["admin"])


def _usage_light() -> dict[str, Any]:
    """Best-effort today/lifetime usage for status cards."""
    try:
        import usage_stats

        return usage_stats.light_snapshot()
    except Exception as e:  # noqa: BLE001
        return {"error": str(e)[:200], "today_requests": 0, "today_tokens": 0, "total_tokens": 0}


# ── request bodies ──────────────────────────────────────────────────────────


class SetupBody(BaseModel):
    password: str = Field(min_length=4, max_length=128)


class LoginBody(BaseModel):
    password: str = Field(min_length=1, max_length=128)


class CreateKeyBody(BaseModel):
    name: str = Field(default="default", max_length=64)
    note: str = Field(default="", max_length=256)


class UpdateKeyBody(BaseModel):
    name: str | None = Field(default=None, max_length=64)
    note: str | None = Field(default=None, max_length=256)
    enabled: bool | None = None


class LoginModeBody(BaseModel):
    """Device-code only (OAuth / local CLI login removed)."""

    mode: str = Field(default="device", pattern="^(device|oauth)$")
    capture: bool | None = None


class FeatureToggleBody(BaseModel):
    enabled: bool


class AccountModeBody(BaseModel):
    mode: str = Field(description="round_robin | random | least_used")


class ChangePasswordBody(BaseModel):
    current_password: str = Field(min_length=1, max_length=128)
    new_password: str = Field(min_length=4, max_length=128)
    confirm_password: str | None = Field(default=None, max_length=128)


class RuntimeSettingsBody(BaseModel):
    """Partial update for admin system settings page."""

    account_mode: str | None = None
    token_maintain_enabled: bool | None = None
    model_health_enabled: bool | None = None
    reasoning_compat: str | None = Field(
        default=None, description="off | think_tag | content"
    )
    outbound_max_tools: int | None = Field(default=None, ge=0, le=64)
    outbound_tool_gap_sec: float | None = Field(default=None, ge=0.0, le=2.0)
    history_compact_enabled: bool | None = None
    sse_keepalive: float | None = Field(default=None, ge=2.0, le=120.0)
    conversation_affinity_enabled: bool | None = None
    default_model: str | None = Field(default=None, max_length=128)
    # Pool rotation / cooldown policy
    cooldown_default_sec: float | None = Field(default=None, ge=1, le=600)
    cooldown_auth_sec: float | None = Field(default=None, ge=5, le=1800)
    cooldown_rate_limit_sec: float | None = Field(default=None, ge=5, le=1800)
    cooldown_server_error_sec: float | None = Field(default=None, ge=1, le=600)
    cooldown_max_sec: float | None = Field(default=None, ge=30, le=3600)
    soft_model_block_ttl_sec: float | None = Field(default=None, ge=30, le=3600)
    durable_model_block_ttl_sec: float | None = Field(default=None, ge=60, le=86400)
    probe_fail_kick_streak: int | None = Field(default=None, ge=1, le=20)
    probe_fail_disable_streak: int | None = Field(default=None, ge=2, le=50)
    probe_kick_cooldown_sec: float | None = Field(default=None, ge=30, le=7200)
    max_failover_attempts: int | None = Field(default=None, ge=1, le=64)
    pool_policy: dict[str, Any] | None = None
    # Outbound proxy pool for account-pool traffic (chat / probe / refresh)
    outbound_proxy_enabled: bool | None = None
    outbound_proxy: str | None = Field(
        default=None,
        max_length=64_000,
        description="Multi-line proxy pool for account egress",
    )
    outbound_proxy_username: str | None = Field(default=None, max_length=256)
    outbound_proxy_password: str | None = Field(default=None, max_length=512)
    outbound_proxy_strategy: str | None = Field(
        default=None,
        pattern="^(round_robin|random|sticky|rr|rand|first|fixed)?$",
    )
    outbound_proxy_config: dict[str, Any] | None = None


class KickAccountBody(BaseModel):
    reason: str = Field(default="手动移出轮询", max_length=300)
    # >0: temporary cooldown kick; null/0: disable account from pool
    cooldown_sec: float | None = Field(default=None, ge=0, le=3600)


class AccountEnabledBody(BaseModel):
    enabled: bool = True


class ImportAuthBody(BaseModel):
    """Import token / auth.json content for Linux servers."""
    payload: str | dict = Field(
        description="JWT string, single entry JSON, or full auth.json map"
    )
    merge: bool = Field(default=True, description="Merge into existing auth.json")


class ImportSsoBody(BaseModel):
    """Import one or more xAI SSO cookies via HTTP device flow."""
    sso_cookies: list[str] = Field(
        min_length=1,
        description="SSO cookie JWTs; one per line, supports email----pass----sso lines",
    )
    merge: bool = Field(default=True, description="Merge into existing auth.json")
    delay: int = Field(default=0, ge=0, le=300, description="Seconds between accounts")
    max_workers: int = Field(
        default=8,
        ge=1,
        le=32,
        description="Concurrent SSO convert threads (capped by GROK2API_SSO_IMPORT_WORKERS)",
    )


class EmailRegistrationBody(BaseModel):
    """Start email-assisted accounts.x.ai registration."""

    # Backward-compat: older clients sent provider=moemail for the mail service.
    provider: str | None = Field(
        default=None,
        pattern="^(moemail|yyds|gptmail|cfmail)$",
        description="Deprecated alias of mail_provider",
    )
    mail_provider: str | None = Field(
        default=None,
        pattern="^(moemail|yyds|gptmail|cfmail)$",
        description="Temp-mail: moemail | yyds | gptmail | cfmail (cloudflare_temp_email)",
    )
    protocol: str = Field(default="grpc", pattern="^grpc$")
    email: str | None = Field(default=None, max_length=256)
    mailbox_id: str | None = Field(default=None, max_length=256)
    prefix: str | None = Field(default=None, max_length=64)
    domain: str | None = Field(default=None, max_length=128)
    # MoeMail official presets: 1h / 24h / 3d / permanent(0). YYDS is ~24h temp.
    expiry_ms: int | None = Field(default=None, ge=0, le=259200000)
    api_key: str | None = Field(
        default=None,
        max_length=512,
        description="Active mail API key (mirrored into per-provider slot)",
    )
    moemail_api_key: str | None = Field(default=None, max_length=512)
    yyds_api_key: str | None = Field(default=None, max_length=512)
    gptmail_api_key: str | None = Field(default=None, max_length=512)
    cfmail_api_key: str | None = Field(default=None, max_length=512)
    moemail_domain: str | None = Field(default=None, max_length=128)
    yyds_domain: str | None = Field(default=None, max_length=128)
    gptmail_domain: str | None = Field(default=None, max_length=128)
    cfmail_domain: str | None = Field(default=None, max_length=128)
    captcha_provider: str | None = Field(
        default=None,
        pattern="^(local|yescaptcha)$",
        description="Turnstile provider: local solver or YesCaptcha",
    )
    local_solver_url: str | None = Field(
        default=None,
        max_length=256,
        description="Local Turnstile Solver base URL, e.g. http://127.0.0.1:5072",
    )
    yescaptcha_key: str | None = Field(default=None, max_length=512)
    base_url: str | None = Field(
        default=None,
        max_length=256,
        description="MoeMail only; YYDS/GPTMail use fixed hosts",
    )
    proxy: str | None = Field(
        default=None,
        max_length=64_000,
        description="Single proxy URL or multi-line proxy pool (one per line)",
    )
    proxy_username: str | None = Field(default=None, max_length=256)
    proxy_password: str | None = Field(default=None, max_length=512)
    proxy_strategy: str | None = Field(
        default=None,
        pattern="^(round_robin|random|sticky|rr|rand|first|fixed)?$",
        description="Proxy pool strategy: round_robin | random | sticky",
    )
    # Multi-thread batch registration
    count: int | None = Field(
        default=1,
        ge=1,
        description="How many accounts to register (batch/multi-thread; no hard cap, concurrency limits parallelism)",
    )
    concurrency: int | None = Field(
        default=5,
        ge=1,
        le=10,
        description="Max concurrent registration workers",
    )
    stagger_ms: int | None = Field(
        default=400,
        ge=0,
        le=10000,
        description="Stagger delay between worker starts (ms)",
    )
    probe_delay_sec: int | None = Field(
        default=None,
        ge=0,
        le=600,
        description="Seconds to wait after import before auto health probe (0=immediate)",
    )


class EmailRegistrationProxyTestBody(BaseModel):
    proxy: str | None = Field(
        default=None,
        max_length=64_000,
        description="Single proxy or multi-line pool; tests first (or all when test_all)",
    )
    proxy_username: str | None = Field(default=None, max_length=256)
    proxy_password: str | None = Field(default=None, max_length=512)
    proxy_strategy: str | None = Field(default=None, max_length=32)
    test_all: bool | None = Field(
        default=False,
        description="When true, smoke-test every proxy in the pool (capped)",
    )
    max_test: int | None = Field(
        default=5,
        ge=1,
        le=20,
        description="Max proxies to test when test_all is true",
    )


class RegistrationConfigBody(BaseModel):
    """Persist protocol-registration form (mail provider / captcha / proxy)."""

    mail_provider: str | None = Field(
        default=None,
        pattern="^(moemail|yyds|gptmail|cfmail)$",
        description="Temp-mail: moemail | yyds | gptmail | cfmail (cloudflare_temp_email)",
    )
    base_url: str | None = Field(
        default=None,
        max_length=256,
        description="MoeMail only; YYDS/GPTMail ignore this (fixed hosts)",
    )
    api_key: str | None = Field(
        default=None,
        max_length=512,
        description="Active key for selected provider (also stored in per-provider field)",
    )
    moemail_api_key: str | None = Field(default=None, max_length=512)
    yyds_api_key: str | None = Field(default=None, max_length=512)
    gptmail_api_key: str | None = Field(default=None, max_length=512)
    domain: str | None = Field(
        default=None,
        max_length=128,
        description="Active domain for selected mail provider",
    )
    moemail_domain: str | None = Field(default=None, max_length=128)
    yyds_domain: str | None = Field(default=None, max_length=128)
    gptmail_domain: str | None = Field(default=None, max_length=128)
    prefix: str | None = Field(default=None, max_length=64)
    expiry_ms: int | None = Field(default=None, ge=0, le=259200000)
    captcha_provider: str | None = Field(
        default=None,
        pattern="^(local|yescaptcha)$",
        description="Turnstile provider: local solver or YesCaptcha",
    )
    local_solver_url: str | None = Field(
        default=None,
        max_length=256,
        description="Local Turnstile Solver base URL",
    )
    yescaptcha_key: str | None = Field(default=None, max_length=512)
    proxy: str | None = Field(
        default=None,
        max_length=64_000,
        description="Single proxy URL or multi-line proxy pool",
    )
    proxy_username: str | None = Field(default=None, max_length=256)
    proxy_password: str | None = Field(default=None, max_length=512)
    proxy_strategy: str | None = Field(
        default=None,
        pattern="^(round_robin|random|sticky|rr|rand|first|fixed)?$",
        description="Proxy pool strategy: round_robin | random | sticky",
    )
    count: int | None = Field(default=None, ge=1, le=10000)
    concurrency: int | None = Field(default=None, ge=1, le=10)
    stagger_ms: int | None = Field(default=None, ge=0, le=10000)
    probe_delay_sec: int | None = Field(
        default=None,
        ge=0,
        le=600,
        description="Seconds to wait after import before auto health probe (0=immediate)",
    )


class RefreshBody(BaseModel):
    force: bool = Field(
        default=True,
        description="True = refresh all tokens; False = only near-expiry",
    )
    ids: list[str] = Field(
        default_factory=list,
        max_length=2000,
        description="Optional account ids to renew; empty = all accounts",
    )


class AccountBulkExportBody(BaseModel):
    ids: list[str] = Field(default_factory=list, max_length=2000)
    include_secrets: bool = Field(
        default=True,
        description="Include access/refresh tokens (required for re-import)",
    )


class ExportRegistrationSsoBody(BaseModel):
    """Export SSO cookies collected during email registration sessions."""

    batch_id: str | None = Field(
        default=None, description="Only sessions from this registration batch"
    )
    session_ids: list[str] = Field(
        default_factory=list,
        max_length=5000,
        description="Optional explicit session ids; empty = all matching filters",
    )
    status: list[str] = Field(
        default_factory=list,
        description="Filter by session status (e.g. imported). Empty = any status with SSO",
    )
    include_password: bool = Field(
        default=False,
        description="Include registration password when present (email:password:sso lines)",
    )
    format: str = Field(
        default="sso",
        description="sso | cookie | email_sso | email_password_sso | json",
    )
    download: bool = Field(default=True, description="Return as file attachment")


class AccountProbeBody(BaseModel):
    """Per-account model connectivity probe."""

    model: str | None = Field(
        default=None, description="Model id; default DEFAULT_MODEL / PROBE_MODELS"
    )
    auto_disable: bool | None = Field(
        default=None,
        description="On hard error: block model / disable account (default config)",
    )


class AccountProbeBatchBody(BaseModel):
    """Probe selected accounts (multi-select)."""

    ids: list[str] = Field(default_factory=list, max_length=500)
    model: str | None = Field(
        default=None, description="Model id; default DEFAULT_MODEL / PROBE_MODELS"
    )
    auto_disable: bool | None = Field(default=None)


# ── auth helpers ────────────────────────────────────────────────────────────

ADMIN_COOKIE = "g2a_admin"
ADMIN_COOKIE_MAX_AGE = 7 * 24 * 3600  # match redis/file session TTL


def _set_admin_cookie(response: Response, token: str) -> None:
    """Persist admin session in HttpOnly cookie so page navigations keep auth
    even if localStorage is missing / cleared, until cookie or server session expires.
    """
    if not token:
        return
    response.set_cookie(
        key=ADMIN_COOKIE,
        value=token,
        max_age=ADMIN_COOKIE_MAX_AGE,
        httponly=True,
        samesite="lax",
        secure=False,  # allow plain http admin deployments
        path="/",
    )


def _clear_admin_cookie(response: Response) -> None:
    response.delete_cookie(key=ADMIN_COOKIE, path="/")




def _extract_session(request: Request, x_admin_token: str | None) -> str | None:
    if x_admin_token:
        return x_admin_token.strip()
    auth = request.headers.get("authorization") or ""
    if auth.lower().startswith("bearer "):
        return auth[7:].strip()
    return request.cookies.get("g2a_admin")


def require_admin(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
) -> str:
    token = _extract_session(request, x_admin_token)
    if verify_session_token(token):
        return token or ""
    raise HTTPException(status_code=401, detail="Admin authentication required")


def _client_ip(request: Request | None) -> str | None:
    if request is None:
        return None
    try:
        xff = (request.headers.get("x-forwarded-for") or "").split(",")[0].strip()
        if xff:
            return xff[:80]
        if request.client and request.client.host:
            return str(request.client.host)[:80]
    except Exception:
        return None
    return None


def audit_log(
    request: Request | None,
    *,
    action: str,
    summary: str = "",
    target_type: str | None = None,
    target_id: str | None = None,
    detail: dict[str, Any] | None = None,
    ok: bool = True,
    actor: str = "admin",
) -> None:
    """Best-effort write of an admin operation log to PostgreSQL."""
    try:
        from store.audit_pg import write_log

        write_log(
            action=action,
            summary=summary,
            actor=actor,
            target_type=target_type,
            target_id=target_id,
            detail=detail,
            ip=_client_ip(request),
            user_agent=(request.headers.get("user-agent") if request is not None else None),
            ok=ok,
        )
    except Exception:
        pass


# ── public (no session) ─────────────────────────────────────────────────────


def _is_loopback_host(host: str | None) -> bool:
    h = (host or "").strip().lower().strip("[]")
    return h in {"", "127.0.0.1", "localhost", "0.0.0.0", "::", "::1"}


def _request_public_origin(request: Request | None = None) -> str | None:
    """Best-effort public origin for admin/UI links.

    Priority:
      1) GROK2API_PUBLIC_BASE_URL (explicit)
      2) current request Host / X-Forwarded-* (public reverse-proxy friendly)
    Never invent 127.0.0.1 when the request itself is non-loopback.
    """
    configured = (PUBLIC_BASE_URL or getattr(_config, "PUBLIC_BASE_URL", "") or "").strip()
    if configured:
        return configured.rstrip("/")

    if request is None:
        return None

    xf_proto = (request.headers.get("x-forwarded-proto") or "").split(",")[0].strip()
    xf_host = (request.headers.get("x-forwarded-host") or "").split(",")[0].strip()
    host_header = (request.headers.get("host") or "").strip()
    host = xf_host or host_header
    if not host:
        return None

    # Drop default ports for cleaner links.
    host_only = host
    if host_only.endswith(":80") or host_only.endswith(":443"):
        maybe = host_only.rsplit(":", 1)[0]
        if maybe and "/" not in maybe:
            # keep IPv6 bracket forms untouched if present
            if not (maybe.startswith("[") and not host_only.startswith("[")):
                host_only = maybe

    scheme = xf_proto or (request.url.scheme or "http")
    if scheme not in ("http", "https"):
        scheme = "http"
    return f"{scheme}://{host_only}".rstrip("/")


def _public_api_base(request: Request | None = None) -> str:
    origin = _request_public_origin(request)
    if origin:
        return f"{origin}/v1"

    # Fallback only when no request context (startup / offline tools).
    host = str(getattr(_config, "HOST", "") or "")
    port = int(getattr(_config, "PORT", 3000) or 3000)
    if _is_loopback_host(host) or host in ("0.0.0.0", "::"):
        display = "127.0.0.1"
    else:
        display = host
    return f"http://{display}:{port}/v1"


_status_store_cache: dict[str, Any] | None = None
_status_store_at = 0.0
_status_reg_cache: dict[str, Any] | None = None
_status_reg_at = 0.0


@router.get("/status")
async def admin_status(request: Request):
    """Fast admin heartbeat.

    Avoid account acquire() and full account dumps — those dominate latency with
    500+ accounts and make UI auto-refresh feel stuck.
    """
    global _status_store_cache, _status_store_at, _status_reg_cache, _status_reg_at
    setup = is_setup_needed()
    account = accounts.account_status(include_accounts=False)
    key_stats = apikeys.stats()
    pool = account_pool.pool_summary(include_accounts=False)

    # Derive credentials snapshot from counts (no acquire/OIDC).
    creds_ok = int(account.get("active_count") or pool.get("live") or 0) > 0
    creds_email = None

    host = _config.HOST
    port = _config.PORT
    public_origin = _request_public_origin(request)
    api_base = _public_api_base(request)

    now = time.time()
    reg_status: dict[str, Any]
    if _status_reg_cache is not None and now - _status_reg_at < 30:
        reg_status = dict(_status_reg_cache)
    else:
        reg_status = {"available": False}
        try:
            if reg_adapter is not None:
                reg_status = reg_adapter.registration_available()
            else:
                reg_status = {"available": False, "error": _REG_IMPORT_ERROR}
        except Exception as e:  # noqa: BLE001
            reg_status = {"available": False, "error": str(e)}
        _status_reg_cache = dict(reg_status)
        _status_reg_at = now

    try:
        from app import APP_VERSION as _app_ver
    except Exception:
        _app_ver = "unknown"

    if _status_store_cache is not None and now - _status_store_at < 5:
        store_info = dict(_status_store_cache)
    else:
        store_info = {}
        try:
            from store import store_status
            store_info = store_status()
        except Exception as e:  # noqa: BLE001
            store_info = {"backend": "unknown", "error": str(e)}
        _status_store_cache = dict(store_info)
        _status_store_at = now

    return {
        "ok": True,
        "setup_needed": setup,
        "version": _app_ver,
        "store": store_info,
        "cli_version": CLI_VERSION,
        "host": host,
        "port": port,
        "public_origin": public_origin,
        "upstream": UPSTREAM_BASE,
        "default_model": DEFAULT_MODEL,
        "require_api_key_mode": REQUIRE_API_KEY,
        "api_base": api_base,
        "credentials_ok": creds_ok,
        "credentials_email": creds_email,
        "account_mode": get_account_mode(),
        "accounts": account,
        "pool": {
            "mode": pool.get("mode"),
            "total": pool.get("total"),
            "live": pool.get("live"),
            "enabled": pool.get("enabled"),
            "in_cooldown": pool.get("in_cooldown"),
            "quota_disabled": pool.get("quota_disabled"),
            "model_blocked": pool.get("model_blocked"),
            "source": pool.get("source") or "postgres",
        },
        "keys": key_stats,
        "models_count": len(load_models_from_cache()),
        "settings": get_public_settings(),
        "token_maintainer": token_maintainer.status(light=True),
        "model_health": model_health.status(light=True),
        "conversation_affinity": conversation_affinity.status(),
        "registration": reg_status,
        "usage": _usage_light(),
    }


@router.post("/setup")
async def admin_setup(body: SetupBody):
    if not is_setup_needed():
        raise HTTPException(status_code=400, detail="Already set up")
    try:
        set_admin_password(body.password)
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    token = create_session_token()
    resp = JSONResponse({"ok": True, "token": token, "message": "Admin password created"})
    _set_admin_cookie(resp, token)
    return resp


@router.post("/login")
async def admin_login(body: LoginBody, request: Request):
    if is_setup_needed():
        raise HTTPException(status_code=400, detail="Setup required first")
    if not verify_admin_password(body.password):
        raise HTTPException(status_code=401, detail="Invalid password")
    token = create_session_token()
    audit_log(request, action="admin.login", summary="管理员登录成功")
    resp = JSONResponse({"ok": True, "token": token})
    _set_admin_cookie(resp, token)
    return resp


@router.get("/session")
async def admin_session(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Cheap session probe: 200 if cookie/header token still valid."""
    token = _extract_session(request, x_admin_token)
    if verify_session_token(token):
        # sliding refresh already happens inside verify_session_token
        return {"ok": True, "authenticated": True}
    raise HTTPException(status_code=401, detail="Admin authentication required")


@router.post("/logout")
async def admin_logout(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    token = _extract_session(request, x_admin_token)
    revoke_session(token)
    resp = JSONResponse({"ok": True})
    _clear_admin_cookie(resp)
    return resp


# ── protected ───────────────────────────────────────────────────────────────


@router.get("/dashboard")
async def dashboard(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    full: bool = False,
):
    """Overview payload.

    Default is summary-only (fast). Pass ?full=1 only when a page truly needs
    embedded account rows (legacy). Accounts page should use /accounts.
    """
    require_admin(request, x_admin_token)
    account = accounts.account_status(include_accounts=bool(full))
    key_stats = apikeys.stats()
    # Overview uses light pool summary (DB snapshot). full=1 still embeds accounts.
    pool = account_pool.pool_summary(include_accounts=bool(full))
    # Do NOT call load_credentials()/OIDC on overview refresh — it can stall and
    # surface browser "Failed to fetch" when a worker is busy.
    cred = {
        "email": None,
        "active_count": account.get("active_count"),
        "account_count": account.get("account_count"),
        "ok": int(account.get("active_count") or pool.get("live") or 0) > 0,
    }
    host = _config.HOST
    port = _config.PORT
    public_origin = _request_public_origin(request)
    return {
        "credentials": cred,
        "accounts": account,
        "pool": pool,
        "keys": key_stats,
        "account_mode": get_account_mode(),
        "account_modes": list(VALID_ACCOUNT_MODES),
        "models": load_models_from_cache(),
        "settings": get_public_settings(),
        "host": host,
        "port": port,
        "public_origin": public_origin,
        "api_base": _public_api_base(request),
        "cli_version": CLI_VERSION,
        "upstream": UPSTREAM_BASE,
        "default_model": DEFAULT_MODEL,
        "token_maintainer": token_maintainer.status(light=True),
        "model_health": model_health.status(light=True),
        "conversation_affinity": conversation_affinity.status(),
        "usage": _usage_light(),
        "full": bool(full),
    }


@router.get("/keys")
async def list_keys(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    return {
        "keys": apikeys.list_keys(),
        "stats": apikeys.stats(),
        "store_source": apikeys.keys_store_source(),
        "store_backend": apikeys.keys_store_source(),
    }


@router.post("/keys")
async def create_key(
    body: CreateKeyBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    rec = apikeys.create_key(body.name, body.note)
    audit_log(
        request,
        action="keys.create",
        summary=f"创建 API Key：{rec.get('name') or body.name}",
        target_type="api_key",
        target_id=rec.get("id"),
        detail={"name": rec.get("name"), "prefix": rec.get("prefix")},
    )
    return {"ok": True, "key": rec}


@router.post("/keys/{key_id}/regenerate")
async def regenerate_key(
    key_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    rec = apikeys.regenerate_key(key_id)
    if not rec:
        raise HTTPException(status_code=404, detail="Key not found")
    audit_log(
        request,
        action="keys.regenerate",
        summary=f"重建 API Key：{rec.get('name') or key_id}",
        target_type="api_key",
        target_id=key_id,
        detail={"name": rec.get("name"), "prefix": rec.get("prefix")},
    )
    return {"ok": True, "key": rec}


@router.patch("/keys/{key_id}")
async def update_key(
    key_id: str,
    body: UpdateKeyBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    rec = None
    if body.enabled is not None:
        rec = apikeys.set_enabled(key_id, body.enabled)
        if not rec:
            raise HTTPException(status_code=404, detail="Key not found")
    if body.name is not None or body.note is not None:
        rec = apikeys.update_key(key_id, name=body.name, note=body.note)
        if not rec:
            raise HTTPException(status_code=404, detail="Key not found")
    if rec is None:
        for k in apikeys.list_keys():
            if k["id"] == key_id:
                rec = k
                break
    if rec is None:
        raise HTTPException(status_code=404, detail="Key not found")
    return {"ok": True, "key": rec}


@router.delete("/keys/{key_id}")
async def delete_key(
    key_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    if not apikeys.delete_key(key_id):
        raise HTTPException(status_code=404, detail="Key not found")
    audit_log(
        request,
        action="keys.delete",
        summary=f"删除 API Key：{key_id}",
        target_type="api_key",
        target_id=key_id,
    )
    return {"ok": True}


def _sort_account_rows(rows: list[dict], sort_key: str) -> list[dict]:
    """In-memory sort for file-backend /accounts fallback (mirrors PG sort keys)."""
    key = str(sort_key or "newest")

    def _f(v, default=0.0):
        try:
            if v is None or v == "":
                return default
            return float(v)
        except Exception:
            return default

    def _s(v):
        return str(v or "").lower()

    def sorter(row: dict):
        p = row.get("_pool") or {}
        exp = _f(row.get("expires_at"), 0.0)
        upd = _f(row.get("updated_at") or row.get("create_time"), 0.0)
        used = _f(p.get("last_used_at"), 0.0)
        reqs = _f(p.get("request_count"), 0.0)
        email = _s(row.get("email"))
        rid = _s(row.get("id"))
        in_cd = 0 if p.get("in_cooldown") else 1
        disabled = 0 if (p.get("enabled") is False or p.get("disabled_for_quota")) else 1
        if key == "oldest":
            return (upd, rid)
        if key == "expires_desc":
            return (-exp, -upd)
        if key == "expires_asc":
            return (exp if exp else 1e18, -upd)
        if key == "email_asc":
            return (email, rid)
        if key == "email_desc":
            return (email, rid)  # reverse later
        if key == "last_used_desc":
            return (-used, -upd)
        if key == "last_used_asc":
            return (used if used else 1e18, -upd)
        if key == "requests_desc":
            return (-reqs, -upd)
        if key == "cooldown_first":
            return (in_cd, -upd)
        if key == "disabled_first":
            return (disabled, -upd)
        # newest (default)
        return (-upd, rid)

    reverse = key == "email_desc"
    try:
        return sorted(rows, key=sorter, reverse=reverse)
    except Exception:
        return rows


@router.get("/accounts")
async def list_accounts_route(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    page: int = 1,
    page_size: int = 25,
    q: str = "",
    sort: str = "newest",
    summary: bool = False,
):
    """List accounts with server-side SQL pagination (PostgreSQL path).

    - default: paged rows for admin UI (fast first paint)
    - summary=1: counts only
    - page_size<=0 or page_size>=10000: return all summary rows (export/legacy)
    - sort: newest|oldest|expires_desc|expires_asc|email_asc|email_desc|
            last_used_desc|last_used_asc|requests_desc|cooldown_first|disabled_first
    """
    require_admin(request, x_admin_token)

    try:
        from store.accounts_pg import normalize_account_sort

        sort_key = normalize_account_sort(sort)
    except Exception:
        sort_key = (sort or "newest").strip().lower() or "newest"

    if summary:
        status = accounts.account_status(include_accounts=False)
        status["pool"] = account_pool.pool_summary(include_accounts=False)
        status["page"] = 1
        status["page_size"] = 0
        status["total"] = int(status.get("account_count") or 0)
        status["total_pages"] = 1
        status["q"] = (q or "").strip()
        status["sort"] = sort_key
        return status

    # Fast path: page in PostgreSQL, attach pool meta only for current page.
    try:
        from store.accounts_pg import enabled as pg_on, list_account_summaries
        from store.settings_pg import get_pool_meta_many, pool_counts
        from settings_store import get_account_mode
        import time as _time

        if pg_on():
            paged = list_account_summaries(
                q=q, page=page, page_size=page_size, sort=sort_key
            )
            page_items = list(paged.get("accounts") or [])
            ids = [str(a.get("id")) for a in page_items if a.get("id")]
            pool_map = get_pool_meta_many(ids) if ids else {}
            now = _time.time()
            # Optional redis overlay only for the current page (not whole pool).
            try:
                from store.pool_redis import merge_pool_meta
            except Exception:
                merge_pool_meta = None  # type: ignore
            for item in page_items:
                aid = item.get("id")
                meta = dict(pool_map.get(aid) or {})
                if merge_pool_meta is not None and aid:
                    try:
                        meta = merge_pool_meta(str(aid), meta)
                    except Exception:
                        pass
                blocked = meta.get("blocked_models") or {}
                if not isinstance(blocked, dict):
                    blocked = {}
                cd_until = meta.get("cooldown_until")
                in_cd = False
                cd_left = 0.0
                if cd_until is not None:
                    try:
                        cd_left = max(0.0, float(cd_until) - now)
                        in_cd = cd_left > 0
                    except Exception:
                        in_cd = False
                        cd_left = 0.0
                item["_pool"] = {
                    "id": aid,
                    "enabled": bool(meta.get("enabled", True)),
                    "weight": int(meta.get("weight") or 1),
                    "request_count": int(meta.get("request_count") or 0),
                    "success_count": int(meta.get("success_count") or 0),
                    "fail_count": int(meta.get("fail_count") or 0),
                    "last_used_at": meta.get("last_used_at"),
                    "last_error": meta.get("last_error"),
                    "cooldown_until": cd_until,
                    "cooldown_remaining_sec": cd_left,
                    "disabled_for_quota": bool(meta.get("disabled_for_quota")),
                    "disabled_reason": meta.get("disabled_reason"),
                    "quota_disabled_at": meta.get("quota_disabled_at"),
                    "quota_source": meta.get("quota_source"),
                    "last_quota": meta.get("last_quota"),
                    "last_probe": meta.get("last_probe"),
                    "blocked_model_ids": list(blocked.keys()),
                    "in_cooldown": in_cd,
                }

            counts = pool_counts()
            # live ≈ account rows with non-expired token; use page totals for chips
            total = int(paged.get("total") or 0)
            # cheap live estimate from account_status counts path
            base_counts = accounts.account_status(include_accounts=False)
            return {
                "store_source": "postgres",
                "store_backend": "postgres",
                "auth_file": base_counts.get("auth_file"),
                "auth_file_exists": base_counts.get("auth_file_exists"),
                "auth_file_role": "mirror",
                "logged_in": base_counts.get("logged_in"),
                "account_count": base_counts.get("account_count") or total,
                "active_count": base_counts.get("active_count") or total,
                "account_mode": get_account_mode(),
                "platform": base_counts.get("platform"),
                "is_linux": base_counts.get("is_linux"),
                "is_headless": base_counts.get("is_headless"),
                "native_oidc_available": base_counts.get("native_oidc_available"),
                "multi_account": True,
                "accounts": page_items,
                "pool": {
                    "mode": get_account_mode(),
                    "total": counts.get("total") or total,
                    "live": base_counts.get("active_count") or total,
                    "enabled": counts.get("enabled") or total,
                    "in_cooldown": counts.get("in_cooldown") or 0,
                    "quota_disabled": counts.get("quota_disabled") or 0,
                    "model_blocked": counts.get("model_blocked") or 0,
                },
                "page": paged.get("page") or 1,
                "page_size": paged.get("page_size") or page_size,
                "total": total,
                "total_pages": paged.get("total_pages") or 1,
                "q": (q or "").strip(),
                "sort": paged.get("sort") or sort_key,
                "paged": True,
                "fast_path": True,
            }
    except Exception:
        # Fall through to legacy full-scan path (file backend / unexpected errors).
        pass

    # Legacy path: build full merged rows then filter/page (file backend).
    base = accounts.account_status(include_accounts=True)
    pool_full = account_pool.pool_summary(include_accounts=True)
    pool_accounts = list(pool_full.get("accounts") or [])
    pool_map = {a.get("id"): a for a in pool_accounts if isinstance(a, dict) and a.get("id")}
    rows = []
    for acc in (base.get("accounts") or []):
        if not isinstance(acc, dict):
            continue
        aid = acc.get("id")
        p = dict(pool_map.get(aid) or {"id": aid})
        item = dict(acc)
        item["_pool"] = {
            "id": p.get("id") or aid,
            "enabled": p.get("enabled", True),
            "weight": p.get("weight", 1),
            "request_count": p.get("request_count", 0),
            "success_count": p.get("success_count", 0),
            "fail_count": p.get("fail_count", 0),
            "last_used_at": p.get("last_used_at"),
            "last_error": p.get("last_error"),
            "cooldown_until": p.get("cooldown_until"),
            "disabled_for_quota": p.get("disabled_for_quota"),
            "disabled_reason": p.get("disabled_reason"),
            "quota_disabled_at": p.get("quota_disabled_at"),
            "quota_source": p.get("quota_source"),
            "last_quota": p.get("last_quota"),
            "last_probe": p.get("last_probe"),
            "blocked_model_ids": p.get("blocked_model_ids") or [],
            "in_cooldown": p.get("in_cooldown", False),
        }
        rows.append(item)

    query = (q or "").strip().lower()
    if query:
        filtered = []
        for arow in rows:
            p = arow.get("_pool") or {}
            hay = " ".join([
                str(arow.get("email") or ""),
                str(arow.get("id") or ""),
                str(arow.get("user_id") or ""),
                "expired 已过期" if arow.get("expired") else "valid 有效",
                "disabled 已禁用" if p.get("enabled") is False else "enabled 启用",
                "cooldown 冷却" if p.get("in_cooldown") else "",
                "quota 额度禁用 耗尽" if p.get("disabled_for_quota") else "",
                str(p.get("last_error") or ""),
                " ".join(p.get("blocked_model_ids") or []),
            ]).lower()
            if query in hay:
                filtered.append(arow)
        rows = filtered

    rows = _sort_account_rows(rows, sort_key)

    total = len(rows)
    try:
        page_size_i = int(page_size)
    except Exception:
        page_size_i = 25
    try:
        page_i = max(1, int(page))
    except Exception:
        page_i = 1

    if page_size_i <= 0 or page_size_i >= 10000:
        page_items = rows
        page_i = 1
        page_size_i = total or 0
        total_pages = 1
    else:
        page_size_i = max(1, min(200, page_size_i))
        total_pages = max(1, (total + page_size_i - 1) // page_size_i)
        page_i = min(page_i, total_pages)
        start = (page_i - 1) * page_size_i
        page_items = rows[start:start + page_size_i]

    out = {
        "store_source": base.get("store_source") or base.get("store_backend") or "file",
        "store_backend": base.get("store_backend") or base.get("store_source") or "file",
        "auth_file": base.get("auth_file"),
        "auth_file_exists": base.get("auth_file_exists"),
        "auth_file_role": base.get("auth_file_role") or "mirror",
        "logged_in": base.get("logged_in"),
        "account_count": base.get("account_count"),
        "active_count": base.get("active_count"),
        "account_mode": base.get("account_mode"),
        "platform": base.get("platform"),
        "is_linux": base.get("is_linux"),
        "is_headless": base.get("is_headless"),
        "native_oidc_available": base.get("native_oidc_available"),
        "multi_account": base.get("multi_account"),
        "accounts": page_items,
        "pool": {
            "mode": pool_full.get("mode"),
            "total": pool_full.get("total"),
            "live": pool_full.get("live"),
            "enabled": pool_full.get("enabled"),
            "in_cooldown": pool_full.get("in_cooldown"),
            "quota_disabled": pool_full.get("quota_disabled"),
            "model_blocked": pool_full.get("model_blocked"),
        },
        "page": page_i,
        "page_size": page_size_i,
        "total": total,
        "total_pages": total_pages,
        "q": (q or "").strip(),
        "sort": sort_key,
        "paged": True,
        "fast_path": False,
    }
    return out


@router.post("/accounts/login")
async def account_login(
    body: LoginModeBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    return accounts.start_login(body.mode, capture=body.capture)


@router.get("/accounts/login/sessions")
async def login_sessions(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    return {"sessions": accounts.list_login_sessions()}


@router.get("/accounts/login/sessions/{session_id}")
async def login_session_status(
    session_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    sess = accounts.get_login_session(session_id)
    if not sess:
        raise HTTPException(status_code=404, detail="Login session not found")
    return sess


@router.post("/accounts/import")
async def import_account(
    body: ImportAuthBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Import JWT / auth.json JSON body (API / script). Prefer file upload for UI."""
    require_admin(request, x_admin_token)
    result = accounts.import_auth_payload(body.payload, merge=body.merge)
    if not result.get("ok"):
        raise HTTPException(status_code=400, detail=result.get("error") or "import failed")
    return result


# ── SSO import jobs (progress polling across multi-worker via Redis) ───────

_SSO_JOB_TTL_SEC = 3600
_sso_jobs_lock = threading.Lock()
_sso_jobs_local: dict[str, dict[str, Any]] = {}


def _sso_job_key(job_id: str) -> str:
    try:
        from store.redis_client import key as rk

        return rk("sso_import", "job", job_id)
    except Exception:
        return f"g2a:sso_import:job:{job_id}"


def _sso_job_put(job_id: str, job: dict[str, Any]) -> None:
    payload = dict(job)
    with _sso_jobs_lock:
        _sso_jobs_local[job_id] = payload
    try:
        from store.redis_client import set_json

        set_json(_sso_job_key(job_id), payload, _SSO_JOB_TTL_SEC)
    except Exception:
        pass


def _sso_job_get(job_id: str) -> dict[str, Any] | None:
    try:
        from store.redis_client import get_json

        data = get_json(_sso_job_key(job_id))
        if isinstance(data, dict):
            return data
    except Exception:
        pass
    with _sso_jobs_lock:
        job = _sso_jobs_local.get(job_id)
        return dict(job) if isinstance(job, dict) else None


def _sso_job_patch(job_id: str, **fields: Any) -> dict[str, Any] | None:
    job = _sso_job_get(job_id)
    if not isinstance(job, dict):
        return None
    job.update(fields)
    job["updated_at"] = time.time()
    total = max(1, int(job.get("total") or 1))
    done = int(job.get("done") or 0)
    job["percent"] = min(100, int(round(100.0 * done / total)))
    _sso_job_put(job_id, job)
    return job


def _sso_public_job(job: dict[str, Any] | None) -> dict[str, Any]:
    if not isinstance(job, dict):
        return {"ok": False, "error": "job not found"}
    # Never leak full SSO cookies / tokens in progress responses.
    out = {
        "ok": True,
        "job_id": job.get("id"),
        "status": job.get("status") or "unknown",
        "phase": job.get("phase") or "",
        "message": job.get("message") or "",
        "total": int(job.get("total") or 0),
        "done": int(job.get("done") or 0),
        "success": int(job.get("success") or 0),
        "fail": int(job.get("fail") or 0),
        "converted": int(job.get("converted") or 0),
        "percent": int(job.get("percent") or 0),
        "workers": int(job.get("workers") or 0),
        "delay": int(job.get("delay") or 0),
        "created_at": job.get("created_at"),
        "updated_at": job.get("updated_at"),
        "finished_at": job.get("finished_at"),
        "results": job.get("results") or [],
        "imported": job.get("imported") or [],
        "error": job.get("error"),
    }
    return out


def _parse_sso_lines(lines: list[str]) -> list[tuple[str, str]]:
    out: list[tuple[str, str]] = []
    for raw in lines:
        for line in raw.splitlines():
            line = line.strip()
            if not line or line.startswith("#"):
                continue
            email = ""
            if "----" in line:
                parts = line.split("----")
                email = parts[0].strip()
                line = parts[-1].strip()
            elif ":" in line and not line.startswith("eyJ"):
                parts = line.rsplit(":", 1)
                email = parts[0].strip()
                line = parts[-1].strip()
            out.append((email, line))
    return out


def _run_sso_import_job(
    job_id: str,
    *,
    sso_items: list[tuple[str, str]],
    merge: bool,
    delay: int,
    max_workers: int,
) -> None:
    """Background worker: convert SSO cookies then bulk-import with progress updates."""
    from concurrent.futures import ThreadPoolExecutor, as_completed

    total = len(sso_items)
    try:
        from config import SSO_IMPORT_WORKERS
    except Exception:
        SSO_IMPORT_WORKERS = 8
    workers = min(int(max_workers), int(SSO_IMPORT_WORKERS), max(1, total))
    # delay only staggers starts; keep enough parallelism for throughput.
    if delay and delay >= 5:
        workers = min(workers, 4)

    _sso_job_patch(
        job_id,
        status="running",
        phase="converting",
        message=f"正在转换 SSO → token（{workers} 线程）…",
        workers=workers,
        done=0,
        success=0,
        fail=0,
        converted=0,
        results=[],
        imported=[],
    )

    pending_entries: list[dict[str, Any]] = []
    results: list[dict[str, Any]] = []
    converted_count = 0
    fail = 0
    progress_lock = threading.Lock()
    last_progress_at = 0.0
    # Throttle Redis/progress writes under high concurrency.
    progress_every = 1 if total <= 20 else max(2, total // 25)

    def _convert_one(args: tuple[int, str, str]) -> dict[str, Any]:
        i, email_hint, sso = args
        if delay > 0 and i > 1:
            # tiny staggered start only (seconds), not cumulative per index
            time.sleep(
                min(float(delay), 2.0)
                * (((i - 1) % max(1, workers)) / max(1.0, float(workers)))
            )
        item: dict[str, Any] = {
            "index": i,
            "sso_hint": (sso[:12] + "...") if len(sso) > 12 else sso,
        }
        try:
            # quiet=True: less stdout lock contention under multi-thread import
            token = sso_import.sso_to_token(sso, quiet=True)
            if not token:
                item["status"] = "failed"
                item["error"] = "device flow failed or invalid sso"
                return item
            _key, entry = sso_import.token_to_auth_entry(token, email=email_hint)
            item["status"] = "converted"
            item["email"] = entry.get("email", email_hint)
            item["entry"] = {
                "key": entry["key"],
                "auth_mode": entry.get("auth_mode", "oidc"),
                "email": entry.get("email", email_hint),
                "refresh_token": entry.get("refresh_token", ""),
                "expires_at": entry.get("expires_at"),
                "oidc_issuer": entry.get("oidc_issuer", sso_import.OIDC_ISSUER),
                "oidc_client_id": entry.get(
                    "oidc_client_id", sso_import.GROK_CLI_CLIENT_ID
                ),
                "user_id": entry.get("user_id") or entry.get("principal_id"),
            }
            return item
        except TypeError:
            # Older sso_to_token without quiet=
            try:
                token = sso_import.sso_to_token(sso)
                if not token:
                    item["status"] = "failed"
                    item["error"] = "device flow failed or invalid sso"
                    return item
                _key, entry = sso_import.token_to_auth_entry(token, email=email_hint)
                item["status"] = "converted"
                item["email"] = entry.get("email", email_hint)
                item["entry"] = {
                    "key": entry["key"],
                    "auth_mode": entry.get("auth_mode", "oidc"),
                    "email": entry.get("email", email_hint),
                    "refresh_token": entry.get("refresh_token", ""),
                    "expires_at": entry.get("expires_at"),
                    "oidc_issuer": entry.get("oidc_issuer", sso_import.OIDC_ISSUER),
                    "oidc_client_id": entry.get(
                        "oidc_client_id", sso_import.GROK_CLI_CLIENT_ID
                    ),
                    "user_id": entry.get("user_id") or entry.get("principal_id"),
                }
                return item
            except Exception as e:  # noqa: BLE001
                item["status"] = "failed"
                item["error"] = str(e)
                return item
        except Exception as e:  # noqa: BLE001
            item["status"] = "failed"
            item["error"] = str(e)
            return item

    try:
        with ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="sso-import-"
        ) as ex:
            futs = [
                ex.submit(_convert_one, (i, e, s))
                for i, (e, s) in enumerate(sso_items, 1)
            ]
            for fut in as_completed(futs):
                item = fut.result()
                with progress_lock:
                    if item.get("status") == "converted":
                        converted_count += 1
                        pending_entries.append(item["entry"])
                        pub = {k: v for k, v in item.items() if k != "entry"}
                        # Keep status as converting until bulk import finishes.
                        pub["status"] = "converted"
                        results.append(pub)
                    else:
                        fail += 1
                        results.append(
                            {k: v for k, v in item.items() if k != "entry"}
                        )
                    done = converted_count + fail
                    # Sort for stable UI order.
                    results.sort(key=lambda x: int(x.get("index") or 0))
                    now = time.time()
                    should_publish = (
                        done >= total
                        or done % progress_every == 0
                        or (now - last_progress_at) >= 0.8
                    )
                    if should_publish:
                        last_progress_at = now
                        _sso_job_patch(
                            job_id,
                            status="running",
                            phase="converting",
                            message=(
                                f"转换中 {done}/{total}"
                                f"（成功 {converted_count} · 失败 {fail}）"
                            ),
                            done=done,
                            converted=converted_count,
                            success=0,
                            fail=fail,
                            results=list(results),
                        )

        # Stage 2: one storage write for all converted accounts.
        imported: list[dict[str, Any]] = []
        ok = 0
        if pending_entries:
            _sso_job_patch(
                job_id,
                status="running",
                phase="importing",
                message=f"正在写入账号池（{len(pending_entries)} 个）…",
                done=total,  # convert phase finished
                converted=converted_count,
                fail=fail,
                results=list(results),
            )
            bulk = accounts.import_auth_payloads_bulk(pending_entries, merge=merge)
            if not bulk.get("ok"):
                err = bulk.get("error") or "bulk import failed"
                for item in results:
                    if item.get("status") == "converted":
                        item["status"] = "failed"
                        item["error"] = err
                        fail += 1
                converted_count = 0
            else:
                imp = bulk.get("imported") or []
                imp_iter = iter(imp)
                for item in results:
                    if item.get("status") != "converted":
                        continue
                    info = next(imp_iter, None)
                    if not info:
                        item["status"] = "ok"
                        ok += 1
                        continue
                    item["status"] = "ok"
                    item["account_id"] = info.get("id")
                    item["email"] = info.get("email") or item.get("email")
                    item["user_id"] = info.get("user_id")
                    item["expires_at"] = info.get("expires_at")
                    item["has_refresh_token"] = info.get("has_refresh_token")
                    ok += 1
                    imported.append(
                        {
                            "id": info.get("id"),
                            "email": info.get("email"),
                            "user_id": info.get("user_id"),
                            "expires_at": info.get("expires_at"),
                            "has_refresh_token": info.get("has_refresh_token"),
                        }
                    )

        fail = sum(1 for x in results if x.get("status") != "ok")
        ok = sum(1 for x in results if x.get("status") == "ok")
        msg = f"SSO 导入完成：{ok} 成功, {fail} 失败（workers={workers}）"
        _sso_job_patch(
            job_id,
            status="done",
            phase="done",
            message=msg,
            done=total,
            success=ok,
            fail=fail,
            converted=converted_count,
            imported=imported,
            results=results,
            finished_at=time.time(),
            percent=100,
            ok=fail == 0,
        )
        try:
            import task_log

            task_log.record(
                "sso_import",
                task_id=job_id,
                summary=msg,
                status="done" if fail == 0 else ("partial" if ok else "error"),
                ok=fail == 0,
                progress_done=total,
                progress_total=total,
                detail={
                    "success": ok,
                    "fail": fail,
                    "converted": converted_count,
                    "workers": workers,
                    "imported_count": len(imported),
                },
            )
        except Exception:
            pass
    except Exception as e:  # noqa: BLE001
        msg = f"SSO 导入失败：{e}"
        _sso_job_patch(
            job_id,
            status="error",
            phase="error",
            message=msg,
            error=str(e),
            finished_at=time.time(),
            ok=False,
        )
        try:
            import task_log

            task_log.record(
                "sso_import",
                task_id=job_id,
                summary=msg,
                status="error",
                ok=False,
                progress_done=0,
                progress_total=total,
                detail={"error": str(e)[:400]},
            )
        except Exception:
            pass


@router.post("/accounts/import-sso")
async def import_sso(
    body: ImportSsoBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """
    Start SSO cookie import as a background job with progress polling.

    Returns immediately with ``job_id``. Poll
    ``GET /accounts/import-sso/jobs/{job_id}`` until ``status`` is
    ``done`` / ``error``.

    Each SSO cookie is validated, used to authorize a device code, and the
    resulting access_token / refresh_token is merged into the account pool.
    """
    require_admin(request, x_admin_token)

    sso_items = _parse_sso_lines(body.sso_cookies)
    if not sso_items:
        raise HTTPException(status_code=400, detail="No valid SSO cookies provided")

    try:
        from config import SSO_IMPORT_WORKERS
    except Exception:
        SSO_IMPORT_WORKERS = 8
    workers = min(int(body.max_workers), int(SSO_IMPORT_WORKERS), max(1, len(sso_items)))
    if body.delay and body.delay >= 5:
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
        "delay": int(body.delay or 0),
        "merge": bool(body.merge),
        "created_at": now,
        "updated_at": now,
        "finished_at": None,
        "results": [],
        "imported": [],
        "error": None,
        "ok": None,
    }
    _sso_job_put(job_id, job)

    t = threading.Thread(
        target=_run_sso_import_job,
        kwargs={
            "job_id": job_id,
            "sso_items": sso_items,
            "merge": bool(body.merge),
            "delay": int(body.delay or 0),
            "max_workers": int(body.max_workers or workers),
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
        "delay": int(body.delay or 0),
        "message": f"SSO 导入已启动（{len(sso_items)} 条，workers={workers}）",
        "poll_url": f"/admin/api/accounts/import-sso/jobs/{job_id}",
    }


@router.get("/accounts/import-sso/jobs/{job_id}")
async def get_sso_import_job(
    job_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Poll SSO import job progress (percent / done / success / fail / results)."""
    require_admin(request, x_admin_token)
    job = _sso_job_get(str(job_id or "").strip())
    if not job:
        raise HTTPException(status_code=404, detail="SSO import job not found")
    return _sso_public_job(job)


def _require_register_adapter():
    if reg_adapter is None:
        raise HTTPException(
            status_code=503,
            detail=(
                "注册机模块不可用: "
                f"{_REG_IMPORT_ERROR or 'grok_build_adapter import failed'}. "
                "请确认 grok-build-auth/ 已完整检出，并执行: pip install -r requirements.txt "
                "(协议注册依赖 curl_cffi/requests；Turnstile 需要 YesCaptcha；邮箱使用 MoeMail)"
            ),
        )
    probe = getattr(reg_adapter, "registration_available", None)
    if callable(probe):
        st = probe()
        if not st.get("available"):
            raise HTTPException(
                status_code=503,
                detail=st.get("error")
                or "注册组件不可用，请检查 grok-build-auth 与 YesCaptcha/MoeMail 配置",
            )
    return reg_adapter


def _registration_cfg_from_body(body: EmailRegistrationBody | RegistrationConfigBody) -> dict:
    mail_provider = getattr(body, "mail_provider", None)
    if not mail_provider:
        # Legacy field on EmailRegistrationBody.
        mail_provider = getattr(body, "provider", None)
    return {
        "mail_provider": mail_provider,
        "base_url": body.base_url,
        "moemail_base_url": getattr(body, "moemail_base_url", None),
        "cfmail_base_url": getattr(body, "cfmail_base_url", None),
        "api_key": getattr(body, "api_key", None),
        "moemail_api_key": getattr(body, "moemail_api_key", None),
        "yyds_api_key": getattr(body, "yyds_api_key", None),
        "gptmail_api_key": getattr(body, "gptmail_api_key", None),
        "cfmail_api_key": getattr(body, "cfmail_api_key", None),
        "domain": getattr(body, "domain", None),
        "moemail_domain": getattr(body, "moemail_domain", None),
        "yyds_domain": getattr(body, "yyds_domain", None),
        "gptmail_domain": getattr(body, "gptmail_domain", None),
        "cfmail_domain": getattr(body, "cfmail_domain", None),
        "prefix": getattr(body, "prefix", None),
        "expiry_ms": getattr(body, "expiry_ms", None),
        "captcha_provider": getattr(body, "captcha_provider", None),
        "local_solver_url": getattr(body, "local_solver_url", None),
        "yescaptcha_key": getattr(body, "yescaptcha_key", None),
        "proxy": body.proxy,
        "proxy_username": getattr(body, "proxy_username", None),
        "proxy_password": getattr(body, "proxy_password", None),
        "proxy_strategy": getattr(body, "proxy_strategy", None),
        "count": getattr(body, "count", None),
        "concurrency": getattr(body, "concurrency", None),
        "stagger_ms": getattr(body, "stagger_ms", None),
        "probe_delay_sec": getattr(body, "probe_delay_sec", None),
    }


@router.get("/accounts/register-email/config")
async def get_email_registration_config(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Load protocol registration form config (DB + env defaults)."""
    require_admin(request, x_admin_token)
    cfg = get_registration_config(include_secrets=True)
    return {
        "ok": True,
        "config": cfg,
        "source": "database" if _registration_has_db_row() else "env",
    }


def _registration_has_db_row() -> bool:
    try:
        from settings_store import _get_setting_value

        return isinstance(_get_setting_value("registration_config", None), dict)
    except Exception:
        return False


@router.put("/accounts/register-email/config")
async def put_email_registration_config(
    body: RegistrationConfigBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Save protocol registration config to database (and apply to runtime)."""
    require_admin(request, x_admin_token)
    try:
        cfg = set_registration_config(
            body.model_dump(exclude_none=False),
            replace=False,
        )
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"保存注册配置失败: {e}") from e
    audit_log(
        request,
        action="register.config_save",
        summary="保存协议注册配置",
        target_type="registration",
        detail={"keys": [k for k, v in body.model_dump(exclude_none=False).items() if v not in (None, "")]},
    )
    return {"ok": True, "config": cfg, "message": "注册配置已保存到数据库"}


@router.post("/accounts/register-email")
async def start_email_registration(
    body: EmailRegistrationBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Start protocol registration (grok-build-auth) + mail provider + SSO import.

    Supports multi-thread batch via count/concurrency/stagger_ms.
    Non-empty form fields override the saved DB/env config; empty fields fall
    back to the persisted registration_config. Successful starts also auto-save
    non-secret form defaults (and any newly provided secrets) to the DB.
    """
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    overrides = _registration_cfg_from_body(body)
    resolved = resolve_registration_inputs(overrides)

    # Auto-persist: keep last used form values so next open works after restart.
    try:
        set_registration_config(resolved, replace=False)
    except Exception:
        pass

    try:
        result = adapter.start_registration(
            proxy=resolved.get("proxy") or None,
            proxy_username=resolved.get("proxy_username") or None,
            proxy_password=resolved.get("proxy_password") or None,
            proxy_strategy=resolved.get("proxy_strategy") or None,
            moemail_api_key=resolved.get("api_key") or None,
            moemail_base_url=resolved.get("base_url") or None,
            prefix=resolved.get("prefix") or None,
            domain=resolved.get("domain") or None,
            expiry_ms=resolved.get("expiry_ms"),
            mail_provider=resolved.get("mail_provider") or None,
            captcha_provider=resolved.get("captcha_provider") or None,
            local_solver_url=resolved.get("local_solver_url") or None,
            yescaptcha_key=resolved.get("yescaptcha_key") or None,
            count=resolved.get("count"),
            concurrency=resolved.get("concurrency"),
            stagger_ms=resolved.get("stagger_ms"),
            probe_delay_sec=resolved.get("probe_delay_sec"),
        )
    except TypeError:
        # Older adapter without batch / mail_provider / pool kwargs.
        try:
            result = adapter.start_registration(
                proxy=resolved.get("proxy") or None,
                moemail_api_key=resolved.get("api_key") or None,
                moemail_base_url=resolved.get("base_url") or None,
                prefix=resolved.get("prefix") or None,
                domain=resolved.get("domain") or None,
                captcha_provider=resolved.get("captcha_provider") or None,
                local_solver_url=resolved.get("local_solver_url") or None,
                yescaptcha_key=resolved.get("yescaptcha_key") or None,
            )
        except Exception as e:  # noqa: BLE001
            raise HTTPException(status_code=400, detail=str(e)) from e
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=str(e)) from e
    if not result.get("ok"):
        raise HTTPException(status_code=400, detail=result.get("error") or "start failed")
    return result


@router.post("/accounts/register-email/test-proxy")
async def test_email_registration_proxy(
    body: EmailRegistrationProxyTestBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    from moemail import test_xai_proxy
    from proxy_pool import parse_proxy_pool, pool_summary, pick_proxy

    resolved = resolve_registration_inputs(
        {
            "proxy": body.proxy,
            "proxy_username": body.proxy_username,
            "proxy_password": body.proxy_password,
            "proxy_strategy": body.proxy_strategy,
        }
    )
    proxy_text = resolved.get("proxy") or None
    proxy_user = resolved.get("proxy_username") or None
    proxy_pass = resolved.get("proxy_password") or None
    strategy = resolved.get("proxy_strategy") or "round_robin"
    pool = parse_proxy_pool(
        proxy_text,
        username=proxy_user,
        password=proxy_pass,
        fallback_env=True,
    )
    summary = pool_summary(
        proxy_text,
        username=proxy_user,
        password=proxy_pass,
        strategy=strategy,
        fallback_env=True,
    )
    if not pool:
        # Fall back to classic single-proxy smoke test (may be empty / direct).
        result = test_xai_proxy(
            proxy=proxy_text,
            proxy_username=proxy_user,
            proxy_password=proxy_pass,
        )
        result["proxy_pool"] = summary
        return result

    test_all = bool(body.test_all)
    max_test = int(body.max_test or 5)
    max_test = max(1, min(20, max_test))
    if test_all and len(pool) > 1:
        targets = pool[:max_test]
        results = []
        ok_n = 0
        for url in targets:
            r = test_xai_proxy(proxy=url)
            r["proxy"] = url
            results.append(r)
            if r.get("ok"):
                ok_n += 1
        return {
            "ok": ok_n > 0,
            "proxy_enabled": True,
            "proxy_pool": summary,
            "tested": len(results),
            "ok_count": ok_n,
            "fail_count": len(results) - ok_n,
            "results": results,
            "message": f"tested {len(results)}/{len(pool)} proxies, ok={ok_n}",
        }

    # Default: test one proxy picked by strategy (first for sticky/rr).
    chosen = pick_proxy(pool, strategy=strategy, index=0) or pool[0]
    result = test_xai_proxy(proxy=chosen)
    result["proxy_pool"] = summary
    result["proxy_tested"] = chosen
    return result


@router.post("/register-email/test-proxy")
async def test_email_registration_proxy_unscoped(
    body: EmailRegistrationProxyTestBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    # Alias of /accounts/register-email/test-proxy for older UI paths.
    return await test_email_registration_proxy(body, request, x_admin_token)


@router.get("/accounts/register-email/sessions")
async def list_email_registration_sessions(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    if reg_adapter is None:
        return {"sessions": [], "error": _REG_IMPORT_ERROR}
    out = reg_adapter.list_registration_sessions()
    try:
        st = reg_adapter.registration_available()
        if isinstance(out, dict):
            out["adapter_build"] = st.get("adapter_build")
            out["available"] = st.get("available")
            out["engine"] = st.get("engine") or "dongguatanglinux/grok-build-auth"
            out["yescaptcha_configured"] = st.get("yescaptcha_configured")
            out["captcha_provider"] = st.get("captcha_provider")
            out["local_solver_configured"] = st.get("local_solver_configured")
            out["local_solver_url"] = st.get("local_solver_url")
    except Exception:
        pass
    return out


@router.get("/accounts/register-email/export-sso")
async def export_registration_sso_get(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    batch_id: str | None = None,
    status: str | None = None,
    include_password: int = 0,
    format: str = "sso",
    download: int = 1,
):
    """GET convenience wrapper for export-sso (query params)."""
    body = ExportRegistrationSsoBody(
        batch_id=(batch_id or "").strip() or None,
        status=[s.strip() for s in (status or "").split(",") if s.strip()],
        include_password=bool(include_password),
        format=(format or "sso").strip().lower() or "sso",
        download=bool(download),
    )
    return await export_registration_sso(body, request, x_admin_token)


@router.post("/accounts/register-email/export-sso")
async def export_registration_sso(
    body: ExportRegistrationSsoBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Export SSO cookies from registration sessions.

    Registration keeps ``sess["sso"]`` after import (in-memory session store).
    OIDC accounts themselves only store tokens — use this to re-export the raw
    SSO cookie for re-import / backup.
    """
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    listed = adapter.list_registration_sessions() or {}
    sessions = listed.get("sessions") if isinstance(listed, dict) else listed
    if not isinstance(sessions, list):
        sessions = []

    want_ids = {str(x).strip() for x in (body.session_ids or []) if str(x).strip()}
    want_status = {str(x).strip().lower() for x in (body.status or []) if str(x).strip()}
    want_batch = (body.batch_id or "").strip() or None
    fmt = (body.format or "sso").strip().lower() or "sso"
    if fmt not in {"sso", "cookie", "email_sso", "email_password_sso", "json"}:
        raise HTTPException(status_code=400, detail=f"unsupported format: {fmt}")

    rows: list[dict[str, Any]] = []
    for sess in sessions:
        if not isinstance(sess, dict):
            continue
        sid = str(sess.get("id") or "").strip()
        if want_ids and sid not in want_ids:
            continue
        if want_batch and str(sess.get("batch_id") or "").strip() != want_batch:
            continue
        st = str(sess.get("status") or "").strip().lower()
        if want_status and st not in want_status:
            continue
        sso = str(sess.get("sso") or "").strip()
        if not sso:
            cookies = sess.get("session_cookies")
            if isinstance(cookies, dict):
                sso = str(cookies.get("sso") or cookies.get("sso-rw") or "").strip()
        if not sso:
            continue
        email = str(sess.get("email") or "").strip()
        password = str(sess.get("password") or "").strip()
        rows.append(
            {
                "id": sid,
                "batch_id": sess.get("batch_id"),
                "batch_index": sess.get("batch_index"),
                "status": sess.get("status"),
                "email": email,
                "password": password if body.include_password else "",
                "sso": sso,
            }
        )

    if not rows:
        raise HTTPException(
            status_code=404,
            detail="no registration sessions with SSO cookie matched filters",
        )

    # De-dupe by sso value, keep first email
    seen: set[str] = set()
    unique_rows: list[dict[str, Any]] = []
    for r in rows:
        key = r["sso"]
        if key in seen:
            continue
        seen.add(key)
        unique_rows.append(r)

    ts = time.strftime("%Y%m%d-%H%M%S", time.localtime())
    if fmt == "json":
        payload = {
            "ok": True,
            "count": len(unique_rows),
            "matched": len(rows),
            "format": fmt,
            "batch_id": want_batch,
            "exported_at": ts,
            "items": unique_rows,
        }
        body_bytes = json.dumps(payload, ensure_ascii=False, indent=2).encode("utf-8")
        media = "application/json; charset=utf-8"
        filename = f"grok2api-sso-export-{ts}.json"
        if body.download:
            return Response(
                content=body_bytes,
                media_type=media,
                headers={
                    "Content-Disposition": f'attachment; filename="{filename}"',
                    "X-Export-Count": str(len(unique_rows)),
                },
            )
        return payload

    lines: list[str] = []
    for r in unique_rows:
        sso = r["sso"]
        email = r.get("email") or ""
        password = r.get("password") or ""
        if fmt == "cookie":
            lines.append(f"sso={sso}")
        elif fmt == "email_sso":
            lines.append(f"{email}\t{sso}" if email else sso)
        elif fmt == "email_password_sso":
            if email and password:
                lines.append(f"{email}:{password}:{sso}")
            elif email:
                lines.append(f"{email}::{sso}")
            else:
                lines.append(sso)
        else:  # sso raw
            lines.append(sso)

    text_body = "\n".join(lines) + ("\n" if lines else "")
    filename = f"grok2api-sso-export-{ts}.txt"
    if body.download:
        return Response(
            content=text_body.encode("utf-8"),
            media_type="text/plain; charset=utf-8",
            headers={
                "Content-Disposition": f'attachment; filename="{filename}"',
                "X-Export-Count": str(len(unique_rows)),
            },
        )
    return {
        "ok": True,
        "count": len(unique_rows),
        "matched": len(rows),
        "format": fmt,
        "batch_id": want_batch,
        "exported_at": ts,
        "text": text_body,
        "items": [
            {"email": r.get("email"), "status": r.get("status"), "sso": r.get("sso")[:24] + "..."}
            for r in unique_rows
        ],
    }


@router.get("/accounts/register-email/batches/{batch_id}")
async def get_email_registration_batch(
    batch_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    getter = getattr(adapter, "get_registration_batch", None)
    if not callable(getter):
        raise HTTPException(status_code=404, detail="batch API not available")
    result = getter(batch_id)
    if not result:
        raise HTTPException(status_code=404, detail="registration batch not found")
    return result


@router.get("/accounts/register-email/sessions/{session_id}")
async def get_email_registration_session(
    session_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    include_auth_json: int = 0,
):
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    result = adapter.get_registration_session(
        session_id,
        include_auth_json=bool(include_auth_json),
    )
    if not result:
        raise HTTPException(status_code=404, detail="registration session not found")
    return result


@router.post("/accounts/register-email/sessions/{session_id}/stop")
async def stop_email_registration_session(
    session_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Cooperatively stop one registration session (in-flight worker exits ASAP)."""
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    stopper = getattr(adapter, "stop_registration_session", None)
    if not callable(stopper):
        raise HTTPException(status_code=501, detail="stop API not available")
    result = stopper(session_id)
    if not result.get("ok"):
        raise HTTPException(status_code=404, detail=result.get("error") or "stop failed")
    audit_log(
        request,
        action="register.session_stop",
        summary=f"停止注册会话：{session_id}",
        target_type="registration",
        target_id=session_id,
    )
    return result


@router.post("/accounts/register-email/batches/{batch_id}/stop")
async def stop_email_registration_batch(
    batch_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Stop all non-terminal sessions in a registration batch."""
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    stopper = getattr(adapter, "stop_registration_batch", None)
    if not callable(stopper):
        raise HTTPException(status_code=501, detail="batch stop API not available")
    result = stopper(batch_id)
    if not result.get("ok"):
        raise HTTPException(status_code=404, detail=result.get("error") or "stop failed")
    audit_log(
        request,
        action="register.batch_stop",
        summary=f"停止注册批次：{batch_id}",
        target_type="registration",
        target_id=batch_id,
    )
    return result


@router.post("/accounts/register-email/stop")
async def stop_all_email_registrations(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Stop every active registration session currently visible."""
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    stopper = getattr(adapter, "stop_all_active_registrations", None)
    if not callable(stopper):
        raise HTTPException(status_code=501, detail="stop-all API not available")
    result = stopper()
    audit_log(
        request,
        action="register.stop_all",
        summary=f"停止全部注册：{result.get('stopped_count') or 0}",
        target_type="registration",
    )
    return result


# ── JSON import / export jobs (progress polling) ───────────────────────────

_IO_JOB_TTL_SEC = 3600
_io_jobs_lock = threading.Lock()
_io_jobs_local: dict[str, dict[str, Any]] = {}


def _io_job_key(job_id: str) -> str:
    try:
        from store.redis_client import key as rk

        return rk("io_job", job_id)
    except Exception:
        return f"g2a:io_job:{job_id}"


def _io_job_put(job_id: str, job: dict[str, Any]) -> None:
    payload = dict(job)
    with _io_jobs_lock:
        _io_jobs_local[job_id] = payload
    try:
        from store.redis_client import set_json

        # Never put full export auth map into Redis — keep secrets process-local.
        public = {k: v for k, v in payload.items() if k not in ("payload", "payload_bytes", "raw_files")}
        set_json(_io_job_key(job_id), public, _IO_JOB_TTL_SEC)
    except Exception:
        pass


def _io_job_get(job_id: str) -> dict[str, Any] | None:
    with _io_jobs_lock:
        job = _io_jobs_local.get(job_id)
        if isinstance(job, dict):
            return dict(job)
    try:
        from store.redis_client import get_json

        data = get_json(_io_job_key(job_id))
        if isinstance(data, dict):
            return data
    except Exception:
        pass
    return None


def _io_job_patch(job_id: str, **fields: Any) -> dict[str, Any] | None:
    job = _io_job_get(job_id)
    if not isinstance(job, dict):
        return None
    job.update(fields)
    job["updated_at"] = time.time()
    total = max(1, int(job.get("total") or 1))
    done = int(job.get("done") or 0)
    # Prefer explicit percent when provided.
    if "percent" not in fields:
        job["percent"] = min(100, int(round(100.0 * done / total)))
    else:
        try:
            job["percent"] = min(100, max(0, int(fields["percent"])))
        except Exception:
            job["percent"] = min(100, int(round(100.0 * done / total)))
    _io_job_put(job_id, job)
    return job


def _io_public_job(job: dict[str, Any] | None) -> dict[str, Any]:
    if not isinstance(job, dict):
        return {"ok": False, "error": "job not found"}
    return {
        "ok": True,
        "job_id": job.get("id"),
        "kind": job.get("kind") or "",
        "status": job.get("status") or "unknown",
        "phase": job.get("phase") or "",
        "message": job.get("message") or "",
        "total": int(job.get("total") or 0),
        "done": int(job.get("done") or 0),
        "success": int(job.get("success") or 0),
        "fail": int(job.get("fail") or 0),
        "percent": int(job.get("percent") or 0),
        "count": int(job.get("count") or 0),
        "parse_errors": int(job.get("parse_errors") or 0),
        "filename": job.get("filename"),
        "download_ready": bool(job.get("download_ready")),
        "created_at": job.get("created_at"),
        "updated_at": job.get("updated_at"),
        "finished_at": job.get("finished_at"),
        "file_meta": job.get("file_meta") or [],
        "error": job.get("error"),
    }


def _parse_json_import_text(text: str, filename: str | None = None) -> tuple[Any | None, str | None]:
    """Return (payload, error). payload may be dict or JWT string."""
    raw = (text or "").strip()
    if not raw:
        return None, "empty content"
    try:
        return json.loads(raw), None
    except json.JSONDecodeError as e:
        if raw.startswith("eyJ") and "." in raw and "\n" not in raw:
            return raw, None
        return None, f"invalid JSON: {e}"


def _run_json_import_job(
    job_id: str,
    *,
    file_items: list[dict[str, Any]],
    merge: bool,
) -> None:
    """Background: parse uploaded JSON texts then bulk-import with progress."""
    total = max(1, len(file_items))
    payloads: list[Any] = []
    file_meta: list[dict[str, Any]] = []
    parse_errors = 0
    try:
        _io_job_patch(
            job_id,
            status="running",
            phase="parsing",
            message=f"正在解析 JSON（0/{len(file_items)}）…",
            done=0,
            total=len(file_items),
            success=0,
            fail=0,
            percent=0,
        )
        for i, item in enumerate(file_items, 1):
            name = item.get("filename") or f"file-{i}.json"
            text = item.get("text") or ""
            payload, err = _parse_json_import_text(text, name)
            if err or payload is None:
                parse_errors += 1
                file_meta.append({"filename": name, "ok": False, "error": err or "parse failed"})
            else:
                payloads.append(payload)
                file_meta.append({"filename": name, "ok": True})
            _io_job_patch(
                job_id,
                status="running",
                phase="parsing",
                message=f"正在解析 JSON（{i}/{len(file_items)}）…",
                done=i,
                total=len(file_items),
                fail=parse_errors,
                success=len(payloads),
                file_meta=list(file_meta),
                parse_errors=parse_errors,
                percent=min(50, int(round(50.0 * i / total))),
            )

        if not payloads:
            msg = "没有可导入的有效 JSON"
            _io_job_patch(
                job_id,
                status="error",
                phase="error",
                message=msg,
                error=msg,
                done=len(file_items),
                total=len(file_items),
                fail=parse_errors or len(file_items),
                success=0,
                file_meta=file_meta,
                parse_errors=parse_errors,
                finished_at=time.time(),
                percent=100,
                ok=False,
            )
            try:
                import task_log

                task_log.record(
                    "json_import",
                    task_id=job_id,
                    summary=msg,
                    status="error",
                    ok=False,
                    progress_done=0,
                    progress_total=len(file_items),
                    detail={"parse_errors": parse_errors, "files": len(file_items)},
                )
            except Exception:
                pass
            return

        _io_job_patch(
            job_id,
            status="running",
            phase="importing",
            message=f"正在写入账号池（{len(payloads)} 个文件已解析）…",
            done=len(file_items),
            total=len(file_items),
            percent=70,
            file_meta=file_meta,
            parse_errors=parse_errors,
        )
        result = accounts.import_auth_payloads_bulk(payloads, merge=merge)
        count = int(result.get("count") or 0)
        ok = bool(result.get("ok"))
        if not ok:
            msg = str(result.get("error") or "import failed")
            _io_job_patch(
                job_id,
                status="error",
                phase="error",
                message=msg,
                error=msg,
                count=0,
                success=0,
                fail=len(file_items),
                file_meta=file_meta,
                parse_errors=parse_errors,
                finished_at=time.time(),
                percent=100,
                ok=False,
            )
            try:
                import task_log

                task_log.record(
                    "json_import",
                    task_id=job_id,
                    summary=msg,
                    status="error",
                    ok=False,
                    progress_done=0,
                    progress_total=len(file_items),
                    detail={"parse_errors": parse_errors, "files": len(file_items)},
                )
            except Exception:
                pass
            return

        st = "done" if not parse_errors else "partial"
        msg = result.get("message") or f"JSON 导入完成：{count} 个账号"
        if parse_errors:
            msg = f"{msg}（{parse_errors} 个文件解析失败）"
        _io_job_patch(
            job_id,
            status=st,
            phase="done",
            message=msg,
            count=count,
            success=count,
            fail=parse_errors,
            file_meta=file_meta,
            parse_errors=parse_errors,
            finished_at=time.time(),
            percent=100,
            ok=True,
            imported=result.get("imported") or [],
        )
        try:
            import task_log

            task_log.record(
                "json_import",
                task_id=job_id,
                summary=msg,
                status=st,
                ok=True,
                progress_done=len(file_items) - parse_errors,
                progress_total=len(file_items),
                detail={
                    "count": count,
                    "files": len(file_items),
                    "parse_errors": parse_errors,
                    "merge": merge,
                },
            )
        except Exception:
            pass
    except Exception as e:  # noqa: BLE001
        msg = f"JSON 导入失败：{e}"
        _io_job_patch(
            job_id,
            status="error",
            phase="error",
            message=msg,
            error=str(e)[:400],
            finished_at=time.time(),
            percent=100,
            ok=False,
        )
        try:
            import task_log

            task_log.record(
                "json_import",
                task_id=job_id,
                summary=msg,
                status="error",
                ok=False,
                detail={"error": str(e)[:400]},
            )
        except Exception:
            pass


def _run_json_export_job(
    job_id: str,
    *,
    account_ids: list[str] | None,
    include_secrets: bool,
    filename_prefix: str,
) -> None:
    """Background: build export JSON and keep bytes process-local for download."""
    try:
        selected_n = len(account_ids) if account_ids is not None else 0
        _io_job_patch(
            job_id,
            status="running",
            phase="exporting",
            message=(
                f"正在导出选中账号（{selected_n}）…"
                if account_ids is not None
                else "正在导出全部账号…"
            ),
            done=0,
            total=max(1, selected_n or 1),
            percent=10,
        )
        result = accounts.export_auth_payload(
            include_secrets=include_secrets,
            account_ids=account_ids,
        )
        count = int(result.get("count") or 0)
        if account_ids is not None and count <= 0:
            msg = "没有匹配的账号可导出"
            _io_job_patch(
                job_id,
                status="error",
                phase="error",
                message=msg,
                error=msg,
                count=0,
                finished_at=time.time(),
                percent=100,
                ok=False,
            )
            try:
                import task_log

                task_log.record(
                    "json_export",
                    task_id=job_id,
                    summary=msg,
                    status="error",
                    ok=False,
                    progress_done=0,
                    progress_total=selected_n,
                    detail={"selected": selected_n},
                )
            except Exception:
                pass
            return

        _io_job_patch(
            job_id,
            status="running",
            phase="serializing",
            message=f"正在序列化 {count} 个账号…",
            done=count,
            total=max(1, count),
            count=count,
            percent=70,
        )
        payload = {
            "exported_at": result.get("exported_at") or time.time(),
            "source": "grokcli-2api",
            "count": count,
            "auth": result.get("auth") or {},
        }
        if result.get("selected") is not None:
            payload["selected"] = result.get("selected")
        if result.get("missing") is not None:
            payload["missing"] = result.get("missing")
        body = json.dumps(payload, ensure_ascii=False, indent=2).encode("utf-8")
        ts = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
        filename = f"{filename_prefix}-{count}-{ts}.json"
        msg = (
            f"导出完成：{count} 个账号"
            + (f"（选中 {selected_n}）" if account_ids is not None else "")
        )
        _io_job_patch(
            job_id,
            status="done",
            phase="done",
            message=msg,
            count=count,
            success=count,
            fail=0,
            done=count,
            total=max(1, count),
            filename=filename,
            download_ready=True,
            payload_bytes=body,
            finished_at=time.time(),
            percent=100,
            ok=True,
        )
        try:
            import task_log

            task_log.record(
                "json_export",
                task_id=job_id,
                summary=msg,
                status="done",
                ok=True,
                progress_done=count,
                progress_total=count,
                detail={
                    "count": count,
                    "selected": selected_n if account_ids is not None else None,
                    "include_secrets": include_secrets,
                    "filename": filename,
                    "bytes": len(body),
                },
            )
        except Exception:
            pass
    except Exception as e:  # noqa: BLE001
        msg = f"JSON 导出失败：{e}"
        _io_job_patch(
            job_id,
            status="error",
            phase="error",
            message=msg,
            error=str(e)[:400],
            finished_at=time.time(),
            percent=100,
            ok=False,
        )
        try:
            import task_log

            task_log.record(
                "json_export",
                task_id=job_id,
                summary=msg,
                status="error",
                ok=False,
                detail={"error": str(e)[:400]},
            )
        except Exception:
            pass


@router.post("/accounts/import-file")
async def import_account_file(
    request: Request,
    file: UploadFile = File(..., description="auth.json or export JSON"),
    merge: str = Form(default="true"),
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Import one JSON file as a background job with progress polling."""
    require_admin(request, x_admin_token)
    merge_flag = str(merge).strip().lower() not in ("0", "false", "no", "off")
    try:
        raw_bytes = await file.read()
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=400, detail=f"read file failed: {e}") from e
    if not raw_bytes:
        raise HTTPException(status_code=400, detail="empty file")
    if len(raw_bytes) > 8 * 1024 * 1024:
        raise HTTPException(status_code=400, detail="file too large (max 8MB)")
    try:
        text = raw_bytes.decode("utf-8-sig")
    except UnicodeDecodeError as e:
        raise HTTPException(
            status_code=400, detail=f"file must be UTF-8 JSON: {e}"
        ) from e
    text = text.strip()
    if not text:
        raise HTTPException(status_code=400, detail="empty file content")

    job_id = f"jsonimp_{uuid.uuid4().hex[:16]}"
    now = time.time()
    job = {
        "id": job_id,
        "kind": "json_import",
        "status": "queued",
        "phase": "queued",
        "message": f"已排队：{file.filename or '1 个文件'}",
        "total": 1,
        "done": 0,
        "success": 0,
        "fail": 0,
        "percent": 0,
        "count": 0,
        "parse_errors": 0,
        "created_at": now,
        "updated_at": now,
        "finished_at": None,
        "file_meta": [],
        "error": None,
        "ok": None,
    }
    _io_job_put(job_id, job)
    t = threading.Thread(
        target=_run_json_import_job,
        kwargs={
            "job_id": job_id,
            "file_items": [{"filename": file.filename, "text": text}],
            "merge": merge_flag,
        },
        daemon=True,
        name=f"json-import-{job_id[-8:]}",
    )
    t.start()
    return {
        "ok": True,
        "async": True,
        "job_id": job_id,
        "status": "queued",
        "total": 1,
        "message": job["message"],
        "poll_url": f"/admin/api/accounts/import-files/jobs/{job_id}",
    }


@router.post("/accounts/import-files")
async def import_account_files_bulk(
    request: Request,
    files: list[UploadFile] = File(..., description="one or more auth.json files"),
    merge: str = Form(default="true"),
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Bulk JSON import as background job (parse → write) with progress polling."""
    require_admin(request, x_admin_token)
    merge_flag = str(merge).strip().lower() not in ("0", "false", "no", "off")
    if not files:
        raise HTTPException(status_code=400, detail="no files")
    if len(files) > 200:
        raise HTTPException(status_code=400, detail="too many files (max 200)")

    file_items: list[dict[str, Any]] = []
    for f in files:
        try:
            raw_bytes = await f.read()
        except Exception as e:  # noqa: BLE001
            raise HTTPException(status_code=400, detail=f"read file failed: {e}") from e
        if not raw_bytes:
            continue
        if len(raw_bytes) > 8 * 1024 * 1024:
            raise HTTPException(
                status_code=400,
                detail=f"file too large (max 8MB): {f.filename}",
            )
        try:
            text = raw_bytes.decode("utf-8-sig").strip()
        except UnicodeDecodeError as e:
            raise HTTPException(
                status_code=400,
                detail=f"file must be UTF-8 JSON ({f.filename}): {e}",
            ) from e
        if not text:
            continue
        file_items.append({"filename": f.filename, "text": text})

    if not file_items:
        raise HTTPException(status_code=400, detail="no valid files")

    job_id = f"jsonimp_{uuid.uuid4().hex[:16]}"
    now = time.time()
    job = {
        "id": job_id,
        "kind": "json_import",
        "status": "queued",
        "phase": "queued",
        "message": f"已排队，共 {len(file_items)} 个 JSON 文件",
        "total": len(file_items),
        "done": 0,
        "success": 0,
        "fail": 0,
        "percent": 0,
        "count": 0,
        "parse_errors": 0,
        "created_at": now,
        "updated_at": now,
        "finished_at": None,
        "file_meta": [],
        "error": None,
        "ok": None,
    }
    _io_job_put(job_id, job)
    t = threading.Thread(
        target=_run_json_import_job,
        kwargs={
            "job_id": job_id,
            "file_items": file_items,
            "merge": merge_flag,
        },
        daemon=True,
        name=f"json-import-{job_id[-8:]}",
    )
    t.start()
    return {
        "ok": True,
        "async": True,
        "job_id": job_id,
        "status": "queued",
        "total": len(file_items),
        "message": job["message"],
        "poll_url": f"/admin/api/accounts/import-files/jobs/{job_id}",
    }


@router.get("/accounts/import-files/jobs/{job_id}")
async def get_json_import_job(
    job_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Poll JSON import job progress."""
    require_admin(request, x_admin_token)
    job = _io_job_get(str(job_id or "").strip())
    pub = _io_public_job(job)
    if not pub.get("ok") and pub.get("error") == "job not found":
        raise HTTPException(status_code=404, detail="job not found")
    return pub


def _export_response(
    result: dict[str, Any],
    *,
    download: bool,
    filename_prefix: str = "grok2api-auth-export",
) -> Response:
    payload = {
        "exported_at": result.get("exported_at") or time.time(),
        "source": "grokcli-2api",
        "count": result.get("count", 0),
        "auth": result.get("auth") or {},
    }
    if result.get("selected") is not None:
        payload["selected"] = result.get("selected")
    if result.get("missing") is not None:
        payload["missing"] = result.get("missing")
    body = json.dumps(payload, ensure_ascii=False, indent=2)
    if not download:
        return JSONResponse(content=payload)
    ts = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
    count = int(result.get("count") or 0)
    filename = f"{filename_prefix}-{count}-{ts}.json"
    return Response(
        content=body.encode("utf-8"),
        media_type="application/json; charset=utf-8",
        headers={
            "Content-Disposition": f'attachment; filename="{filename}"',
            "Cache-Control": "no-store",
        },
    )


def _start_export_job(
    *,
    account_ids: list[str] | None,
    include_secrets: bool,
    filename_prefix: str,
) -> dict[str, Any]:
    job_id = f"jsonexp_{uuid.uuid4().hex[:16]}"
    now = time.time()
    total_hint = len(account_ids) if account_ids is not None else 0
    job = {
        "id": job_id,
        "kind": "json_export",
        "status": "queued",
        "phase": "queued",
        "message": (
            f"已排队导出选中 {total_hint} 个账号"
            if account_ids is not None
            else "已排队导出全部账号"
        ),
        "total": max(1, total_hint or 1),
        "done": 0,
        "success": 0,
        "fail": 0,
        "percent": 0,
        "count": 0,
        "download_ready": False,
        "filename": None,
        "created_at": now,
        "updated_at": now,
        "finished_at": None,
        "error": None,
        "ok": None,
    }
    _io_job_put(job_id, job)
    t = threading.Thread(
        target=_run_json_export_job,
        kwargs={
            "job_id": job_id,
            "account_ids": account_ids,
            "include_secrets": include_secrets,
            "filename_prefix": filename_prefix,
        },
        daemon=True,
        name=f"json-export-{job_id[-8:]}",
    )
    t.start()
    return {
        "ok": True,
        "async": True,
        "job_id": job_id,
        "status": "queued",
        "total": job["total"],
        "message": job["message"],
        "poll_url": f"/admin/api/accounts/export/jobs/{job_id}",
        "download_url": f"/admin/api/accounts/export/jobs/{job_id}/download",
    }


@router.get("/accounts/export")
async def export_accounts(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    download: int = 1,
    async_job: int = 0,
):
    """
    Export full auth.json (with tokens) for backup / migration.

    - async_job=1 → start background job, poll /accounts/export/jobs/{id}
    - download=1 → attachment (sync path only)
    - download=0 → JSON body (sync path only)
    """
    require_admin(request, x_admin_token)
    if async_job:
        return _start_export_job(
            account_ids=None,
            include_secrets=True,
            filename_prefix="grok2api-auth-export",
        )
    result = accounts.export_auth_payload(include_secrets=True)
    return _export_response(result, download=bool(download))


@router.post("/accounts/export-batch")
async def export_accounts_batch(
    body: AccountBulkExportBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    download: int = 1,
    async_job: int = 0,
):
    """Export selected accounts only (multi-select). Supports async_job=1."""
    require_admin(request, x_admin_token)
    ids = [str(x).strip() for x in (body.ids or []) if str(x).strip()]
    if not ids:
        raise HTTPException(status_code=400, detail="ids is required")
    if len(ids) > 2000:
        raise HTTPException(status_code=400, detail="too many ids (max 2000)")
    if async_job:
        return _start_export_job(
            account_ids=ids,
            include_secrets=bool(body.include_secrets),
            filename_prefix="grok2api-auth-export-selected",
        )
    result = accounts.export_auth_payload(
        include_secrets=bool(body.include_secrets),
        account_ids=ids,
    )
    if not result.get("count"):
        raise HTTPException(status_code=404, detail="no matching accounts to export")
    return _export_response(
        result,
        download=bool(download),
        filename_prefix="grok2api-auth-export-selected",
    )


@router.get("/accounts/export/jobs/{job_id}")
async def get_json_export_job(
    job_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Poll JSON export job progress."""
    require_admin(request, x_admin_token)
    job = _io_job_get(str(job_id or "").strip())
    pub = _io_public_job(job)
    if not pub.get("ok") and pub.get("error") == "job not found":
        raise HTTPException(status_code=404, detail="job not found")
    return pub


@router.get("/accounts/export/jobs/{job_id}/download")
async def download_json_export_job(
    job_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Download finished export payload for a job (process-local bytes)."""
    require_admin(request, x_admin_token)
    job = _io_job_get(str(job_id or "").strip())
    if not isinstance(job, dict):
        raise HTTPException(status_code=404, detail="job not found")
    if str(job.get("status") or "") != "done" or not job.get("download_ready"):
        raise HTTPException(status_code=409, detail="export not ready")
    body = job.get("payload_bytes")
    if not isinstance(body, (bytes, bytearray)):
        raise HTTPException(
            status_code=410,
            detail="export payload expired or unavailable on this worker; re-export",
        )
    filename = job.get("filename") or "grok2api-auth-export.json"
    return Response(
        content=bytes(body),
        media_type="application/json; charset=utf-8",
        headers={
            "Content-Disposition": f'attachment; filename="{filename}"',
            "Cache-Control": "no-store",
        },
    )


@router.post("/accounts/logout")
async def account_logout(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    result = accounts.run_logout()
    audit_log(
        request,
        action="accounts.logout_all",
        summary=result.get("message") if isinstance(result, dict) else "清空账号池",
        target_type="pool",
        detail=result if isinstance(result, dict) else None,
        ok=bool((result or {}).get("ok", True)) if isinstance(result, dict) else True,
    )
    return result


@router.delete("/accounts/{account_id:path}")
async def delete_account(
    account_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    # Guard against path-greedy matches swallowing register-email subpaths
    # (e.g. /accounts/register-email/batches/.../stop) if route order shifts.
    aid = str(account_id or "")
    if aid.startswith("register-email") or "/register-email" in aid:
        raise HTTPException(status_code=404, detail="Not found")
    require_admin(request, x_admin_token)
    if not accounts.remove_account(account_id):
        raise HTTPException(status_code=404, detail="Account not found")
    audit_log(
        request,
        action="accounts.delete",
        summary=f"删除账号：{account_id}",
        target_type="account",
        target_id=account_id,
    )
    return {"ok": True}


class AccountBulkDeleteBody(BaseModel):
    ids: list[str] = Field(default_factory=list, max_length=2000)


@router.post("/accounts/delete-batch")
async def delete_accounts_batch(
    body: AccountBulkDeleteBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Delete multiple accounts from durable store (PostgreSQL, or auth.json in file mode)."""
    require_admin(request, x_admin_token)
    ids = [str(x).strip() for x in (body.ids or []) if str(x).strip()]
    if not ids:
        raise HTTPException(status_code=400, detail="ids is required")
    if len(ids) > 2000:
        raise HTTPException(status_code=400, detail="too many ids (max 2000)")
    result = accounts.remove_accounts(ids)
    audit_log(
        request,
        action="accounts.delete_batch",
        summary=f"批量删除账号 {result.get('removed_count') or len(result.get('removed') or [])} 个",
        target_type="account",
        detail={
            "requested": result.get("requested"),
            "removed_count": result.get("removed_count"),
            "missing_count": result.get("missing_count"),
            "removed_sample": (result.get("removed") or [])[:20],
        },
    )
    return {"ok": True, **result}


@router.patch("/accounts/{account_id:path}/enabled")
async def set_account_enabled_route(
    account_id: str,
    body: AccountEnabledBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    found = any(a["id"] == account_id for a in accounts.list_accounts())
    if not found:
        raise HTTPException(status_code=404, detail="Account not found")
    rec = account_pool.set_account_enabled(account_id, body.enabled)
    audit_log(
        request,
        action="accounts.set_enabled",
        summary=f"{'启用' if body.enabled else '禁用'}账号：{account_id}",
        target_type="account",
        target_id=account_id,
        detail={"enabled": bool(body.enabled)},
    )
    return {"ok": True, "account": rec}


@router.get("/accounts/quota")
async def list_accounts_quota(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    cached: bool = False,
    refresh: bool = False,
):
    """Quota list.

    - cached=1/refresh=0: return DB-cached last_quota only (fast)
    - default/refresh=1: query upstream and persist snapshots
    """
    require_admin(request, x_admin_token)
    if cached and not refresh:
        return quota.list_cached_quotas(include_expired=True)
    return await quota.fetch_all_quotas(include_expired=False)


def _probe_result_with_pool(account_id: str, probe: dict[str, Any] | None) -> dict[str, Any]:
    """Attach durable pool status so admin UI can patch rows without reload."""
    out = dict(probe or {})
    aid = str(out.get("account_id") or account_id or "").strip()
    if not aid:
        return out
    try:
        state = account_pool.get_account_pool_state() if hasattr(account_pool, "get_account_pool_state") else None
    except Exception:
        state = None
    try:
        from settings_store import get_account_pool_state

        if state is None:
            state = get_account_pool_state()
        meta = (state or {}).get(aid) or {}
        if not isinstance(meta, dict):
            meta = {}
        # Prefer account_pool helper for derived flags when available.
        try:
            pool_view = account_pool._pool_meta(aid, {aid: meta})  # noqa: SLF001
        except Exception:
            pool_view = meta
        out["pool"] = {
            "id": aid,
            "enabled": pool_view.get("enabled", True),
            "in_cooldown": bool(pool_view.get("in_cooldown")),
            "pool_status": pool_view.get("pool_status") or "normal",
            "cooldown_count": int(pool_view.get("cooldown_count") or 0),
            "cooldown_until": pool_view.get("cooldown_until"),
            "cooldown_sec": pool_view.get("cooldown_sec"),
            "cooldown_reason": pool_view.get("cooldown_reason"),
            "cooldown_code": pool_view.get("cooldown_code"),
            "cooldown_model": pool_view.get("cooldown_model"),
            "cooldown_tokens_actual": pool_view.get("cooldown_tokens_actual"),
            "cooldown_tokens_limit": pool_view.get("cooldown_tokens_limit"),
            "status_stack": pool_view.get("status_stack") or [],
            "last_error": pool_view.get("last_error"),
            "last_probe": pool_view.get("last_probe"),
            "last_probe_status": pool_view.get("last_probe_status"),
            "blocked_model_ids": pool_view.get("blocked_model_ids")
            or list((pool_view.get("blocked_models") or {}).keys()),
            "consecutive_fails": pool_view.get("consecutive_fails") or 0,
            "probe_fail_streak": pool_view.get("probe_fail_streak") or 0,
            "disabled_for_quota": bool(pool_view.get("disabled_for_quota")),
        }
        # Convenience top-level mirrors for older UI.
        out["pool_status"] = out["pool"]["pool_status"]
        out["in_cooldown"] = out["pool"]["in_cooldown"]
        out["cooldown_count"] = out["pool"]["cooldown_count"]
    except Exception:
        pass
    return out


@router.post("/accounts/{account_id:path}/probe")
async def account_probe(
    account_id: str,
    body: AccountProbeBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Probe a single account with one model (connectivity / error check)."""
    require_admin(request, x_admin_token)
    model = resolve_model(body.model) if body.model else None
    try:
        result = await asyncio.to_thread(
            model_health.probe_single_account,
            account_id,
            model,
            auto_disable=body.auto_disable,
            source="manual",
        )
    except AuthError as e:
        raise HTTPException(status_code=404, detail=str(e)) from e
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=str(e)[:400]) from e
    return _probe_result_with_pool(account_id, result if isinstance(result, dict) else {"ok": False})


@router.post("/accounts/probe-batch")
async def accounts_probe_batch(
    body: AccountProbeBatchBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Probe selected accounts; return per-account results + durable pool status."""
    require_admin(request, x_admin_token)
    ids = [str(x).strip() for x in (body.ids or []) if str(x).strip()]
    if not ids:
        raise HTTPException(status_code=400, detail="ids is empty")
    if len(ids) > 500:
        raise HTTPException(status_code=400, detail="too many ids (max 500)")
    model = resolve_model(body.model) if body.model else None

    def _run() -> list[dict[str, Any]]:
        # Parallel across accounts (same cap as background probes) — sequential
        # loop over hundreds of ids freezes admin requests when many models exist.
        from concurrent.futures import ThreadPoolExecutor, as_completed
        from config import MODEL_PROBE_WORKERS

        out: list[dict[str, Any]] = []
        workers = max(1, min(int(MODEL_PROBE_WORKERS or 4), len(ids), 16))

        def _one(aid: str) -> dict[str, Any]:
            try:
                r = model_health.probe_single_account(
                    aid,
                    model,
                    auto_disable=body.auto_disable,
                    source="manual_batch",
                )
                return _probe_result_with_pool(
                    aid, r if isinstance(r, dict) else {"ok": False}
                )
            except Exception as e:  # noqa: BLE001
                return _probe_result_with_pool(
                    aid,
                    {
                        "ok": False,
                        "account_id": aid,
                        "error": str(e)[:300],
                        "result": {"available": False, "error": str(e)[:300]},
                    },
                )

        with ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="probe-batch-"
        ) as ex:
            futs = {ex.submit(_one, aid): aid for aid in ids}
            # Preserve input order for stable UI.
            by_id: dict[str, dict[str, Any]] = {}
            for fut in as_completed(futs):
                aid = futs[fut]
                try:
                    by_id[aid] = fut.result()
                except Exception as e:  # noqa: BLE001
                    by_id[aid] = _probe_result_with_pool(
                        aid,
                        {
                            "ok": False,
                            "account_id": aid,
                            "error": str(e)[:300],
                            "result": {"available": False, "error": str(e)[:300]},
                        },
                    )
            out = [by_id[aid] for aid in ids if aid in by_id]
        return out

    results = await asyncio.to_thread(_run)
    ok_n = sum(1 for r in results if r.get("ok"))
    cool_n = sum(1 for r in results if (r.get("pool") or {}).get("in_cooldown"))
    summary = f"批量模型探测：成功 {ok_n}/{len(results)} · 冷却 {cool_n}"
    try:
        import task_log

        task_log.record(
            "probe_batch",
            summary=summary,
            status="done" if ok_n == len(results) else ("partial" if ok_n else "error"),
            ok=ok_n > 0 or not results,
            progress_done=ok_n,
            progress_total=len(results),
            detail={"count": len(results), "ok": ok_n, "cooldown": cool_n},
        )
    except Exception:
        pass
    return {
        "ok": True,
        "count": len(results),
        "available_count": ok_n,
        "unavailable_count": len(results) - ok_n,
        "cooldown_count": cool_n,
        "results": results,
    }


@router.post("/accounts/probe-all")
async def accounts_probe_all(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Run model probe for every live account (same as background cycle)."""
    require_admin(request, x_admin_token)
    result = await asyncio.to_thread(model_health.run_once, source="manual_all")
    # task_log is written inside model_health.run_once
    return result


@router.get("/model-health")
async def model_health_status(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    return model_health.status()


@router.get("/accounts/{account_id:path}/quota")
async def account_quota(
    account_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    cached: bool = False,
    refresh: bool = False,
):
    """Query billing quota for one account.

    cached=1 returns last_quota from DB when present; otherwise falls back to live query.
    """
    require_admin(request, x_admin_token)
    if cached and not refresh:
        try:
            import account_pool
            for a in account_pool.list_pool_accounts():
                if a.get("id") == account_id and isinstance(a.get("last_quota"), dict):
                    q = dict(a["last_quota"])
                    q.setdefault("account_id", account_id)
                    q.setdefault("email", a.get("email"))
                    q["cached"] = True
                    q["pool_disabled"] = bool(a.get("disabled_for_quota") or a.get("enabled") is False)
                    if not q.get("display") and q.get("summary"):
                        q["display"] = {"summary": q.get("summary")}
                    return q
        except Exception:
            pass
    try:
        # Prefer shared helper (handles id alias / remount / refresh).
        result = await asyncio.to_thread(quota.fetch_quota_by_account_id, account_id)
    except AuthError as e:
        raise HTTPException(status_code=404, detail=str(e)) from e
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"quota query failed: {e}") from e
    return result





@router.post("/accounts/{account_id:path}/cooldown/clear")
async def clear_account_cooldown_route(
    account_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Clear cooldown so account re-enters rotation immediately."""
    require_admin(request, x_admin_token)
    rec = account_pool.clear_account_cooldown(account_id)
    if rec is None:
        raise HTTPException(status_code=404, detail="account not found")
    audit_log(
        request,
        action="accounts.cooldown_clear",
        summary=f"清除冷却：{account_id}",
        target_type="account",
        target_id=account_id,
    )
    return {"ok": True, "account": rec}


@router.post("/accounts/{account_id:path}/kick")
async def kick_account_route(
    account_id: str,
    body: KickAccountBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Kick account from rotation: temporary cooldown or hard disable."""
    require_admin(request, x_admin_token)
    rec = account_pool.kick_from_pool(
        account_id,
        reason=body.reason,
        cooldown_sec=body.cooldown_sec,
    )
    if rec is None:
        raise HTTPException(status_code=404, detail="account not found")
    audit_log(
        request,
        action="accounts.cooldown",
        summary=f"账号进入冷却：{account_id}",
        target_type="account",
        target_id=account_id,
        detail={"reason": body.reason, "cooldown_sec": body.cooldown_sec},
    )
    return {"ok": True, "account": rec}


@router.post("/accounts/model-blocks/prune")
async def prune_model_blocks_route(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Drop expired soft model blocks across the pool."""
    require_admin(request, x_admin_token)
    n = account_pool.prune_expired_model_blocks()
    return {"ok": True, "removed": n}


@router.get("/settings")
async def get_settings(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Full public settings payload for the system settings page."""
    require_admin(request, x_admin_token)
    return {"ok": True, "settings": get_public_settings()}


@router.put("/settings")
async def put_settings(
    body: RuntimeSettingsBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Update one or more runtime settings from admin UI."""
    require_admin(request, x_admin_token)
    patch = body.model_dump(exclude_unset=True)
    if not patch:
        raise HTTPException(status_code=400, detail="没有可更新的字段")
    try:
        settings = update_runtime_settings(patch)
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    audit_log(
        request,
        action="settings.update",
        summary="更新系统设置",
        target_type="settings",
        detail={"keys": sorted(list(patch.keys()))[:40]},
    )
    return {"ok": True, "settings": settings}


@router.put("/settings/password")
async def put_admin_password(
    body: ChangePasswordBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Change admin password (requires current password)."""
    require_admin(request, x_admin_token)
    new_pw = body.new_password
    if body.confirm_password is not None and body.confirm_password != new_pw:
        raise HTTPException(status_code=400, detail="两次输入的新密码不一致")
    try:
        change_admin_password(current=body.current_password, new_password=new_pw)
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    audit_log(request, action="settings.password_change", summary="修改管理员密码", target_type="settings")
    return {
        "ok": True,
        "message": "密码已更新",
        "settings": get_public_settings(),
    }


@router.put("/settings/token-maintain")
async def set_token_maintain(
    body: FeatureToggleBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Enable/disable background Token auto-renewal worker."""
    require_admin(request, x_admin_token)
    enabled = set_token_maintain_enabled(bool(body.enabled))
    audit_log(
        request,
        action="settings.token_maintain",
        summary=f"Token 自动续期：{'开启' if enabled else '关闭'}",
        target_type="settings",
        detail={"enabled": bool(enabled)},
    )

    return {
        "ok": True,
        "token_maintain_enabled": enabled,
        "settings": {"token_maintain_enabled": enabled},
        "maintainer": token_maintainer.status(light=True),
        "token_maintainer": token_maintainer.status(light=True),
    }


@router.put("/settings/model-health")
async def set_model_health_flag(
    body: FeatureToggleBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Enable/disable background model health probe worker."""
    require_admin(request, x_admin_token)
    enabled = set_model_health_enabled(bool(body.enabled))
    audit_log(
        request,
        action="settings.model_health",
        summary=f"模型健康探测：{'开启' if enabled else '关闭'}",
        target_type="settings",
        detail={"enabled": bool(enabled)},
    )

    return {
        "ok": True,
        "model_health_enabled": enabled,
        "settings": {"model_health_enabled": enabled},
        "model_health": model_health.status(light=True),
    }


@router.put("/settings/account-mode")
async def set_mode(
    body: AccountModeBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    try:
        mode = set_account_mode(body.mode)
    except ValueError as e:
        raise HTTPException(status_code=400, detail=str(e)) from e
    return {
        "ok": True,
        "account_mode": mode,
        "modes": list(VALID_ACCOUNT_MODES),
    }


@router.post("/accounts/refresh")
async def refresh_accounts(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    body: RefreshBody | None = None,
):
    """
    Refresh access tokens via refresh_token and update expires_at.
    force=true (default) refreshes all accounts with a refresh_token.
    Pass body.ids to renew only selected accounts.
    """
    require_admin(request, x_admin_token)
    force = True if body is None else bool(body.force)
    ids = None
    if body is not None and body.ids:
        ids = [str(x).strip() for x in body.ids if str(x).strip()]
        if not ids:
            raise HTTPException(status_code=400, detail="ids is empty")
        if len(ids) > 2000:
            raise HTTPException(status_code=400, detail="too many ids (max 2000)")
    result = accounts.do_refresh_all(force=force, account_ids=ids)
    # also kick background maintainer
    try:
        token_maintainer.request_run_soon()
    except Exception:
        pass
    # Prefer maintainer cycle summary when available for overview widgets.
    try:
        result["maintainer"] = token_maintainer.status(light=True)
        result["token_maintainer"] = result["maintainer"]
    except Exception:
        result["maintainer"] = token_maintainer.status()
        result["token_maintainer"] = result["maintainer"]
    # Normalize common fields for frontend overview text.
    if isinstance(result, dict):
        if "refresh" not in result:
            result["refresh"] = {
                "refreshed": result.get("refreshed"),
                "attempted": result.get("attempted"),
                "failed": result.get("failed"),
                "skipped": result.get("skipped"),
                "deferred": result.get("deferred"),
            }
        try:
            import task_log

            refreshed = result.get("refreshed") or (result.get("refresh") or {}).get("refreshed") or 0
            attempted = (result.get("refresh") or {}).get("attempted") or 0
            failed = (result.get("refresh") or {}).get("failed") or 0
            task_log.record(
                "token_refresh",
                summary=f"Token 续期{'（强制）' if force else ''}：刷新 {refreshed}",
                status="done" if not failed else ("partial" if refreshed else "error"),
                ok=bool(result.get("ok", True)) and not (failed and not refreshed),
                progress_done=int(refreshed or 0),
                progress_total=int(attempted or refreshed or 0),
                detail=result.get("refresh") if isinstance(result.get("refresh"), dict) else None,
            )
        except Exception:
            pass
    return result


@router.get("/maintainer")
async def maintainer_status(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Token auto-refresh worker status (no full account dump)."""
    require_admin(request, x_admin_token)
    # Full account rows already available via /accounts; keep this route small
    # so admin polling stays responsive on 400+ pools.
    return token_maintainer.status()


@router.post("/maintainer/run")
async def maintainer_run(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    body: RefreshBody | None = None,
):
    """Run one maintenance cycle immediately (normalize + refresh)."""
    require_admin(request, x_admin_token)
    force = True if body is None else bool(body.force)
    # task_log is written inside token_maintainer.run_once
    return token_maintainer.run_once(force=force)


@router.post("/accounts/normalize")
async def normalize_accounts(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Re-key auth.json to per-user multi-account layout."""
    require_admin(request, x_admin_token)
    return accounts.do_normalize_keys()


@router.get("/models")
async def admin_models(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    meta: dict = {}
    try:
        from store import models_pg

        if models_pg.enabled():
            meta = models_pg.get_meta() or {}
    except Exception:
        pass
    return {
        "object": "list",
        "data": load_models_from_cache(),
        "default_model": DEFAULT_MODEL,
        "storage": "postgres",
        "meta": meta,
    }


@router.post("/models/sync")
async def models_sync(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Fetch model list from cli-chat-proxy and store in PostgreSQL."""
    require_admin(request, x_admin_token)
    result = sync_models_from_upstream()
    if not result.get("ok"):
        raise HTTPException(status_code=502, detail=result.get("error") or "sync failed")
    audit_log(
        request,
        action="models.sync",
        summary=f"同步上游模型 {result.get('count') or ''}",
        target_type="models",
        detail={
            "count": result.get("count"),
            "storage": "postgres",
            "pg_count": result.get("pg_count"),
        },
    )
    return result



@router.get("/logs")
async def list_admin_logs(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    page: int = 1,
    page_size: int = 50,
    q: str = "",
    action: str = "",
    kind: str = "",
    status: str = "",
):
    """Query task logs (registration / SSO / probe / renew…).

    ``action`` is accepted as an alias of ``kind`` for older UI clients.
    """
    require_admin(request, x_admin_token)
    try:
        from store.task_logs_pg import list_tasks

        kk = (kind or action or "").strip()
        return list_tasks(
            q=q,
            kind=kk,
            status=status,
            page=page,
            page_size=page_size,
        )
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"logs query failed: {e}") from e


@router.get("/logs/actions")
async def list_admin_log_actions(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Return distinct task kinds (kept path for older UI: /logs/actions)."""
    require_admin(request, x_admin_token)
    try:
        from store.task_logs_pg import list_kinds

        return {"ok": True, "actions": list_kinds(), "kinds": list_kinds()}
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=str(e)[:300]) from e


# ── usage / token stats ─────────────────────────────────────────────────────


@router.get("/usage/summary")
async def usage_summary(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    days: int = 7,
):
    """Today / last N days / lifetime token + request aggregates."""
    require_admin(request, x_admin_token)
    try:
        import usage_stats

        return await asyncio.to_thread(usage_stats.summary, days=days)
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=f"usage summary failed: {e}") from e


@router.get("/usage/series")
async def usage_series(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    days: int = 7,
):
    require_admin(request, x_admin_token)
    try:
        import usage_stats

        return await asyncio.to_thread(usage_stats.series, days=days)
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=str(e)[:300]) from e


@router.get("/usage/by-key")
async def usage_by_key(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    days: int = 7,
    limit: int = 50,
):
    require_admin(request, x_admin_token)
    try:
        import usage_stats

        return await asyncio.to_thread(
            usage_stats.breakdown, "key", days=days, limit=limit
        )
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=str(e)[:300]) from e


@router.get("/usage/by-account")
async def usage_by_account(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    days: int = 7,
    limit: int = 50,
):
    require_admin(request, x_admin_token)
    try:
        import usage_stats

        return await asyncio.to_thread(
            usage_stats.breakdown, "account", days=days, limit=limit
        )
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=str(e)[:300]) from e


@router.get("/usage/by-model")
async def usage_by_model(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    days: int = 7,
    limit: int = 50,
):
    require_admin(request, x_admin_token)
    try:
        import usage_stats

        return await asyncio.to_thread(
            usage_stats.breakdown, "model", days=days, limit=limit
        )
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=str(e)[:300]) from e


@router.get("/usage/events")
async def usage_events(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    q: str = "",
    api_key_id: str = "",
    account_id: str = "",
    model: str = "",
    protocol: str = "",
    client_ip: str = "",
    ok: str = "",
    page: int = 1,
    page_size: int = 50,
):
    """Per-request usage details: tokens, API key, client IP, cache hits."""
    require_admin(request, x_admin_token)
    ok_flag: bool | None = None
    ov = (ok or "").strip().lower()
    if ov in ("1", "true", "yes", "ok", "success"):
        ok_flag = True
    elif ov in ("0", "false", "no", "fail", "failed", "error"):
        ok_flag = False
    try:
        import usage_stats

        return await asyncio.to_thread(
            usage_stats.list_events,
            q=q,
            api_key_id=api_key_id,
            account_id=account_id,
            model=model,
            protocol=protocol,
            client_ip=client_ip,
            ok=ok_flag,
            page=page,
            page_size=page_size,
        )
    except Exception as e:  # noqa: BLE001
        raise HTTPException(status_code=500, detail=str(e)[:300]) from e
