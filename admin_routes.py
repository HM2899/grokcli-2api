"""Admin API routes: setup, login, keys, accounts, pool, quota, models, status."""

from __future__ import annotations

import asyncio
import json
import time
from typing import Any

from fastapi import APIRouter, File, Form, Header, HTTPException, Request, UploadFile
from fastapi.responses import JSONResponse, Response
from pydantic import BaseModel, Field

import account_pool
import accounts
import apikeys
import conversation_affinity
import model_health
import quota
import token_maintainer
from auth import AuthError, load_credentials
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
    REQUIRE_API_KEY,
    UPSTREAM_BASE,
)
from models import load_models_from_cache, resolve_model, sync_models_from_upstream
from settings_store import (
    VALID_ACCOUNT_MODES,
    create_session_token,
    get_account_mode,
    get_public_settings,
    is_setup_needed,
    revoke_session,
    set_account_mode,
    set_admin_password,
    verify_admin_password,
    verify_session_token,
)

router = APIRouter(prefix="/admin/api", tags=["admin"])


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


class AccountModeBody(BaseModel):
    mode: str = Field(description="round_robin | random | least_used")


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
        default=4,
        ge=1,
        le=16,
        description="Concurrent import threads (hard-capped to avoid WSL freezes)",
    )


class EmailRegistrationBody(BaseModel):
    """Start email-assisted accounts.x.ai registration."""

    provider: str = Field(default="moemail", pattern="^moemail$")
    protocol: str = Field(default="grpc", pattern="^grpc$")
    email: str | None = Field(default=None, max_length=256)
    mailbox_id: str | None = Field(default=None, max_length=256)
    prefix: str | None = Field(default=None, max_length=64)
    domain: str | None = Field(default=None, max_length=128)
    expiry_ms: int | None = Field(default=None, ge=60000, le=86400000)
    api_key: str | None = Field(default=None, max_length=512)
    yescaptcha_key: str | None = Field(default=None, max_length=512)
    base_url: str | None = Field(default=None, max_length=256)
    proxy: str | None = Field(default=None, max_length=512)
    proxy_username: str | None = Field(default=None, max_length=256)
    proxy_password: str | None = Field(default=None, max_length=512)
    # Multi-thread batch registration
    count: int | None = Field(
        default=1,
        ge=1,
        le=50,
        description="How many accounts to register (batch/multi-thread)",
    )
    concurrency: int | None = Field(
        default=3,
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


class EmailRegistrationProxyTestBody(BaseModel):
    proxy: str | None = Field(default=None, max_length=512)
    proxy_username: str | None = Field(default=None, max_length=256)
    proxy_password: str | None = Field(default=None, max_length=512)


class RefreshBody(BaseModel):
    force: bool = Field(
        default=True,
        description="True = refresh all tokens; False = only near-expiry",
    )


class AccountProbeBody(BaseModel):
    """Per-account model connectivity probe."""

    model: str | None = Field(
        default=None, description="Model id; default DEFAULT_MODEL / PROBE_MODELS"
    )
    auto_disable: bool | None = Field(
        default=None,
        description="On hard error: block model / disable account (default config)",
    )


# ── auth helpers ────────────────────────────────────────────────────────────


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


# ── public (no session) ─────────────────────────────────────────────────────


@router.get("/status")
async def admin_status():
    setup = is_setup_needed()
    account = accounts.account_status()
    key_stats = apikeys.stats()
    pool = account_pool.pool_summary()
    creds_ok = False
    creds_email = None
    try:
        c = load_credentials()
        creds_ok = True
        creds_email = c.email
    except AuthError:
        pass

    host = _config.HOST
    port = _config.PORT
    base_host = "127.0.0.1" if host in ("0.0.0.0", "::", "localhost") else host

    # registration fingerprint — useful to detect stale Docker images
    reg_status: dict[str, Any] = {"available": False}
    try:
        if reg_adapter is not None:
            reg_status = reg_adapter.registration_available()
        else:
            reg_status = {"available": False, "error": _REG_IMPORT_ERROR}
    except Exception as e:  # noqa: BLE001
        reg_status = {"available": False, "error": str(e)}

    try:
        from app import APP_VERSION as _app_ver
    except Exception:
        _app_ver = "unknown"

    return {
        "ok": True,
        "setup_needed": setup,
        "version": _app_ver,
        "cli_version": CLI_VERSION,
        "host": host,
        "port": port,
        "upstream": UPSTREAM_BASE,
        "default_model": DEFAULT_MODEL,
        "require_api_key_mode": REQUIRE_API_KEY,
        "api_base": f"http://{base_host}:{port}/v1",
        "credentials_ok": creds_ok,
        "credentials_email": creds_email,
        "account_mode": get_account_mode(),
        "accounts": account,
        "pool": {
            "mode": pool["mode"],
            "total": pool["total"],
            "live": pool["live"],
            "enabled": pool["enabled"],
            "in_cooldown": pool["in_cooldown"],
        },
        "keys": key_stats,
        "models_count": len(load_models_from_cache()),
        "settings": get_public_settings(),
        "token_maintainer": token_maintainer.status(),
        "model_health": model_health.status(),
        "conversation_affinity": conversation_affinity.status(),
        "registration": reg_status,
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
    return {"ok": True, "token": token, "message": "Admin password created"}


@router.post("/login")
async def admin_login(body: LoginBody):
    if is_setup_needed():
        raise HTTPException(status_code=400, detail="Setup required first")
    if not verify_admin_password(body.password):
        raise HTTPException(status_code=401, detail="Invalid password")
    token = create_session_token()
    return {"ok": True, "token": token}


@router.post("/logout")
async def admin_logout(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    token = _extract_session(request, x_admin_token)
    revoke_session(token)
    return {"ok": True}


# ── protected ───────────────────────────────────────────────────────────────


@router.get("/dashboard")
async def dashboard(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    account = accounts.account_status()
    key_stats = apikeys.stats()
    pool = account_pool.pool_summary()
    try:
        c = load_credentials()
        cred = {
            "email": c.email,
            "user_id": c.user_id,
            "expires_at": c.expires_at,
            "auth_key": c.auth_key,
            "team_id": c.team_id,
        }
    except AuthError as e:
        cred = {"error": str(e)}
    host = _config.HOST
    port = _config.PORT
    base_host = "127.0.0.1" if host in ("0.0.0.0", "::", "localhost") else host
    return {
        "credentials": cred,
        "accounts": account,
        "pool": pool,
        "keys": key_stats,
        "account_mode": get_account_mode(),
        "account_modes": list(VALID_ACCOUNT_MODES),
        "models": load_models_from_cache(),
        "settings": get_public_settings(),
        "api_base": f"http://{base_host}:{port}/v1",
        "cli_version": CLI_VERSION,
        "upstream": UPSTREAM_BASE,
        "default_model": DEFAULT_MODEL,
        "token_maintainer": token_maintainer.status(),
        "model_health": model_health.status(),
        "conversation_affinity": conversation_affinity.status(),
    }


@router.get("/keys")
async def list_keys(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    return {"keys": apikeys.list_keys(), "stats": apikeys.stats()}


@router.post("/keys")
async def create_key(
    body: CreateKeyBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    rec = apikeys.create_key(body.name, body.note)
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
    return {"ok": True}


@router.get("/accounts")
async def list_accounts_route(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    status = accounts.account_status()
    status["pool"] = account_pool.pool_summary()
    return status


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


@router.post("/accounts/import-sso")
async def import_sso(
    body: ImportSsoBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """
    Import accounts from xAI SSO cookies via pure HTTP device flow.
    Each SSO cookie is validated, used to authorize a device code, and the
    resulting access_token / refresh_token is merged into auth.json.
    """
    require_admin(request, x_admin_token)

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

    sso_items = _parse_sso_lines(body.sso_cookies)
    sso_cookies = [sso for _, sso in sso_items]
    if not sso_cookies:
        raise HTTPException(status_code=400, detail="No valid SSO cookies provided")

    results: list[dict[str, Any]] = []
    imported: list[dict[str, Any]] = []
    ok = 0
    fail = 0

    def _import_one(args: tuple[int, str, str]) -> dict[str, Any]:
        i, email_hint, sso = args
        if body.delay > 0 and i > 1:
            time.sleep(body.delay * (i - 1))
        item: dict[str, Any] = {"index": i, "sso_hint": sso[:12] + "..." if len(sso) > 12 else "..."}
        try:
            token = sso_import.sso_to_token(sso)
            if not token:
                item["status"] = "failed"
                item["error"] = "device flow failed or invalid sso"
                return item
            key, entry = sso_import.token_to_auth_entry(token, email=email_hint)
            import_result = accounts.import_auth_payload(
                {
                    "key": entry["key"],
                    "auth_mode": entry.get("auth_mode", "oidc"),
                    "email": entry.get("email", email_hint),
                    "refresh_token": entry.get("refresh_token", ""),
                    "expires_at": entry.get("expires_at"),
                    "oidc_issuer": entry.get("oidc_issuer", sso_import.OIDC_ISSUER),
                    "oidc_client_id": entry.get("oidc_client_id", sso_import.GROK_CLI_CLIENT_ID),
                },
                merge=body.merge,
            )
            if not import_result.get("ok"):
                item["status"] = "failed"
                item["error"] = import_result.get("error") or "import failed"
                return item
            info = (import_result.get("imported") or [{}])[0]
            item["status"] = "ok"
            item["account_id"] = info.get("id")
            item["email"] = info.get("email")
            item["user_id"] = info.get("user_id")
            item["expires_at"] = info.get("expires_at")
            item["has_refresh_token"] = info.get("has_refresh_token")
            return item
        except Exception as e:  # noqa: BLE001
            item["status"] = "failed"
            item["error"] = str(e)
            return item

    from concurrent.futures import ThreadPoolExecutor, as_completed

    try:
        from config import SSO_IMPORT_WORKERS
    except Exception:
        SSO_IMPORT_WORKERS = 4
    # Hard cap regardless of client-supplied max_workers (714×curl_cffi freezes WSL)
    workers = min(int(body.max_workers), int(SSO_IMPORT_WORKERS), max(1, len(sso_items)))
    with ThreadPoolExecutor(max_workers=workers, thread_name_prefix="sso-import-") as ex:
        for fut in as_completed(ex.submit(_import_one, (i, e, s)) for i, (e, s) in enumerate(sso_items, 1)):
            item = fut.result()
            results.append(item)
            if item.get("status") == "ok":
                ok += 1
                imported.append({
                    "id": item.get("account_id"),
                    "email": item.get("email"),
                    "user_id": item.get("user_id"),
                    "expires_at": item.get("expires_at"),
                    "has_refresh_token": item.get("has_refresh_token"),
                })
            else:
                fail += 1

    return {
        "ok": fail == 0,
        "message": f"SSO 导入完成：{ok} 成功, {fail} 失败",
        "total": len(sso_cookies),
        "success": ok,
        "fail": fail,
        "imported": imported,
        "results": results,
    }


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


@router.post("/accounts/register-email")
async def start_email_registration(
    body: EmailRegistrationBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Start protocol registration (grok-build-auth) + MoeMail + SSO import.

    Supports multi-thread batch via count/concurrency/stagger_ms.
    """
    require_admin(request, x_admin_token)
    adapter = _require_register_adapter()
    try:
        result = adapter.start_registration(
            proxy=body.proxy,
            moemail_api_key=body.api_key,
            moemail_base_url=body.base_url,
            prefix=body.prefix,
            domain=body.domain,
            expiry_ms=body.expiry_ms,
            yescaptcha_key=body.yescaptcha_key,
            count=body.count,
            concurrency=body.concurrency,
            stagger_ms=body.stagger_ms,
        )
    except TypeError:
        # Older adapter without batch kwargs.
        try:
            result = adapter.start_registration(
                proxy=body.proxy,
                moemail_api_key=body.api_key,
                moemail_base_url=body.base_url,
                prefix=body.prefix,
                domain=body.domain,
                yescaptcha_key=body.yescaptcha_key,
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

    return test_xai_proxy(
        proxy=body.proxy,
        proxy_username=body.proxy_username,
        proxy_password=body.proxy_password,
    )


@router.post("/register-email/test-proxy")
async def test_email_registration_proxy_unscoped(
    body: EmailRegistrationProxyTestBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    from moemail import test_xai_proxy

    return test_xai_proxy(
        proxy=body.proxy,
        proxy_username=body.proxy_username,
        proxy_password=body.proxy_password,
    )


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
    except Exception:
        pass
    return out


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


@router.post("/accounts/import-file")
async def import_account_file(
    request: Request,
    file: UploadFile = File(..., description="auth.json or export JSON"),
    merge: str = Form(default="true"),
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Import accounts from a JSON file upload (no paste / textarea)."""
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
    try:
        payload: Any = json.loads(text)
    except json.JSONDecodeError as e:
        if text.startswith("eyJ") and "." in text and "\n" not in text:
            payload = text
        else:
            raise HTTPException(status_code=400, detail=f"invalid JSON: {e}") from e
    result = accounts.import_auth_payload(payload, merge=merge_flag)
    if not result.get("ok"):
        raise HTTPException(
            status_code=400, detail=result.get("error") or "import failed"
        )
    result["filename"] = file.filename
    return result


@router.get("/accounts/export")
async def export_accounts(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    download: int = 1,
):
    """
    Export full auth.json (with tokens) for backup / migration.
    download=1 → attachment; download=0 → JSON body.
    """
    require_admin(request, x_admin_token)
    result = accounts.export_auth_payload(include_secrets=True)
    # Prefer plain auth map for re-import compatibility; wrap with meta
    payload = {
        "exported_at": result.get("exported_at") or time.time(),
        "source": "grokcli-2api",
        "count": result.get("count", 0),
        "auth": result.get("auth") or {},
    }
    body = json.dumps(payload, ensure_ascii=False, indent=2)
    if download:
        ts = time.strftime("%Y%m%d-%H%M%S", time.gmtime())
        filename = f"grok2api-auth-export-{ts}.json"
        return Response(
            content=body.encode("utf-8"),
            media_type="application/json; charset=utf-8",
            headers={
                "Content-Disposition": f'attachment; filename="{filename}"',
                "Cache-Control": "no-store",
            },
        )
    return JSONResponse(content=payload)


@router.post("/accounts/logout")
async def account_logout(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    return accounts.run_logout()


@router.delete("/accounts/{account_id:path}")
async def delete_account(
    account_id: str,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    require_admin(request, x_admin_token)
    if not accounts.remove_account(account_id):
        raise HTTPException(status_code=404, detail="Account not found")
    return {"ok": True}


class AccountBulkDeleteBody(BaseModel):
    ids: list[str] = Field(default_factory=list, max_length=2000)


@router.post("/accounts/delete-batch")
async def delete_accounts_batch(
    body: AccountBulkDeleteBody,
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Delete multiple accounts from auth.json in one request."""
    require_admin(request, x_admin_token)
    ids = [str(x).strip() for x in (body.ids or []) if str(x).strip()]
    if not ids:
        raise HTTPException(status_code=400, detail="ids is required")
    if len(ids) > 2000:
        raise HTTPException(status_code=400, detail="too many ids (max 2000)")
    result = accounts.remove_accounts(ids)
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
    return {"ok": True, "account": rec}


@router.get("/accounts/quota")
async def list_accounts_quota(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Query billing quota for all accounts (cli-chat-proxy /v1/billing)."""
    require_admin(request, x_admin_token)
    return await quota.fetch_all_quotas(include_expired=False)


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
    return result


@router.post("/accounts/probe-all")
async def accounts_probe_all(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Run model probe for every live account (same as background cycle)."""
    require_admin(request, x_admin_token)
    return await asyncio.to_thread(model_health.run_once, source="manual_all")


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
):
    """Query billing quota for one account."""
    require_admin(request, x_admin_token)
    try:
        from auth import load_credentials_by_id

        creds = load_credentials_by_id(account_id)
    except AuthError as e:
        raise HTTPException(status_code=404, detail=str(e)) from e
    result = await quota.fetch_quota_for_creds_async(creds)
    if not result.get("ok"):
        # still return payload so UI can show the error
        return result
    return result


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
    """
    require_admin(request, x_admin_token)
    force = True if body is None else bool(body.force)
    result = accounts.do_refresh_all(force=force)
    # also kick background maintainer
    try:
        token_maintainer.request_run_soon()
    except Exception:
        pass
    result["maintainer"] = token_maintainer.status()
    return result


@router.get("/maintainer")
async def maintainer_status(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Token auto-refresh worker status + current remaining lifetimes."""
    require_admin(request, x_admin_token)
    st = token_maintainer.status()
    st["accounts"] = accounts.list_accounts()
    return st


@router.post("/maintainer/run")
async def maintainer_run(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
    body: RefreshBody | None = None,
):
    """Run one maintenance cycle immediately (normalize + refresh)."""
    require_admin(request, x_admin_token)
    force = True if body is None else bool(body.force)
    result = token_maintainer.run_once(force=force)
    return result


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
    return {
        "object": "list",
        "data": load_models_from_cache(),
        "default_model": DEFAULT_MODEL,
    }


@router.post("/models/sync")
async def models_sync(
    request: Request,
    x_admin_token: str | None = Header(default=None, alias="X-Admin-Token"),
):
    """Fetch model list from cli-chat-proxy and update models_cache.json."""
    require_admin(request, x_admin_token)
    result = sync_models_from_upstream()
    if not result.get("ok"):
        raise HTTPException(status_code=502, detail=result.get("error") or "sync failed")
    return result
