"""Adapter: grok-build-auth -> grokcli-2api account pool.

Drives the vendored ``grok-build-auth/xconsole_client`` protocol client to:

1. register an x.ai account with MoeMail + YesCaptcha
2. extract SSO/session cookies
3. convert SSO via sso_to_auth_json into a local auth.json entry
4. import that entry into the multi-account pool

Import of ``xconsole_client`` is deferred so the main API can start even when
optional deps are missing. Registration endpoints then return a clear error
instead of crashing process startup.

``grok-build-auth`` is vendored in-tree (not a git submodule).
Legacy browser (DrissionPage) and grpc-session registration engines were removed.
"""
from __future__ import annotations

import json
import os
import secrets
import sys
import threading
import time
import uuid
from pathlib import Path
from typing import Any

ROOT = Path(__file__).resolve().parent
GBA = ROOT / "grok-build-auth"
ADAPTER_BUILD = "2026-07-11-protocol-5"

YESCAPTCHA_KEY = (
    os.environ.get("GROK2API_YESCAPTCHA_KEY")
    or os.environ.get("YESCAPTCHA_API_KEY")
    or ""
).strip()

# Hard caps for multi-thread registration (YesCaptcha + xAI rate limits).
MAX_BATCH_COUNT = int(os.environ.get("GROK2API_REG_MAX_COUNT", "50") or 50)
MAX_CONCURRENCY = int(os.environ.get("GROK2API_REG_MAX_CONCURRENCY", "10") or 10)
DEFAULT_CONCURRENCY = int(os.environ.get("GROK2API_REG_CONCURRENCY", "3") or 3)

# --------------------------------------------------------------------------- #
# session state
# --------------------------------------------------------------------------- #
_sessions: dict[str, dict[str, Any]] = {}
_batches: dict[str, dict[str, Any]] = {}
_lock = threading.RLock()
_xconsole_ready = False
_xconsole_error: str | None = None


def _now() -> float:
    return time.time()


def _clean_old_sessions() -> None:
    cutoff = _now() - 6 * 3600
    for sid in list(_sessions.keys()):
        sess = _sessions.get(sid) or {}
        if float(sess.get("updated_at") or 0) < cutoff:
            _sessions.pop(sid, None)


def _compact_session(sess: dict[str, Any]) -> dict[str, Any]:
    out = dict(sess)
    out.pop("_client", None)
    out.pop("_oauth_client", None)
    if out.get("auth_json"):
        out["auth_json_count"] = len(out["auth_json"])
        out.pop("auth_json", None)
    return out


def ensure_xconsole() -> None:
    """Ensure vendored grok-build-auth/xconsole_client is importable.

    Raises RuntimeError with actionable message when unavailable.
    Safe to call multiple times.
    """
    global _xconsole_ready, _xconsole_error
    if _xconsole_ready:
        return
    if _xconsole_error:
        raise RuntimeError(_xconsole_error)

    if not GBA.is_dir():
        _xconsole_error = (
            "grok-build-auth 目录不存在。请确认仓库完整检出，"
            "或重新 clone 本项目。"
        )
        raise RuntimeError(_xconsole_error)

    xc = GBA / "xconsole_client"
    if not xc.is_dir():
        _xconsole_error = (
            "grok-build-auth/xconsole_client 不存在。"
            "请确认仓库完整检出（该目录已内置，不再使用 git submodule）。"
        )
        raise RuntimeError(_xconsole_error)

    # Put vendored package root on sys.path so `import xconsole_client` works.
    gba_str = str(GBA.resolve())
    if gba_str not in sys.path:
        sys.path.insert(0, gba_str)

    try:
        # Import side-effect: validate package is loadable.
        import xconsole_client  # noqa: F401
        from xconsole_client import (  # noqa: F401
            XConsoleAuthClient,
            YesCaptchaSolver,
            create_solver,
            xai_oauth_login_protocol,
        )
        from xconsole_client.oauth_protocol import (  # noqa: F401
            extract_cookies_from_auth_client,
        )
        from xconsole_client.xai_oauth import (  # noqa: F401
            CLIPROXYAPI_GROK_HEADERS,
            build_cliproxyapi_auth_record,
        )
    except ModuleNotFoundError as e:
        missing = getattr(e, "name", None) or str(e)
        if missing in ("curl_cffi", "requests") or "curl_cffi" in str(e) or "requests" in str(e):
            _xconsole_error = (
                f"注册机依赖缺失: {missing}。请执行: pip install -r requirements.txt"
            )
        else:
            _xconsole_error = (
                f"无法导入 xconsole_client ({e})。请执行: pip install -r requirements.txt"
            )
        raise RuntimeError(_xconsole_error) from e
    except Exception as e:  # noqa: BLE001
        _xconsole_error = f"加载 grok-build-auth 失败: {e}"
        raise RuntimeError(_xconsole_error) from e

    _xconsole_ready = True
    _xconsole_error = None


def registration_available() -> dict[str, Any]:
    """Non-raising health probe for admin UI / startup logs."""
    moemail_configured = bool(
        os.environ.get("GROK2API_MOEMAIL_API_KEY")
        or os.environ.get("MOEMAIL_API_KEY")
    )
    try:
        from config import MOEMAIL_API_KEY as _cfg_moemail

        moemail_configured = moemail_configured or bool(_cfg_moemail)
    except Exception:
        pass
    try:
        ensure_xconsole()
        return {
            "ok": True,
            "available": True,
            "engine": "dongguatanglinux/grok-build-auth",
            "path": str(GBA),
            "vendored": True,
            "adapter_build": ADAPTER_BUILD,
            "yescaptcha_configured": bool(YESCAPTCHA_KEY),
            "moemail_configured": moemail_configured,
        }
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "available": False,
            "engine": "dongguatanglinux/grok-build-auth",
            "path": str(GBA),
            "vendored": True,
            "adapter_build": ADAPTER_BUILD,
            "error": str(e),
            "yescaptcha_configured": bool(YESCAPTCHA_KEY),
            "moemail_configured": moemail_configured,
        }


# --------------------------------------------------------------------------- #
# mail provider: moemail (reuse grokcli-2api config)
# --------------------------------------------------------------------------- #
def _make_email_receiver(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
):
    from moemail import moemail_create_mailbox
    from config import MOEMAIL_API_KEY, MOEMAIL_BASE_URL, MOEMAIL_DOMAIN, MOEMAIL_EXPIRY_MS

    key = (api_key or MOEMAIL_API_KEY or "").strip()
    if not key:
        raise ValueError(
            "MoeMail API key missing. Set GROK2API_MOEMAIL_API_KEY or pass api_key."
        )
    base = (base_url or MOEMAIL_BASE_URL).rstrip("/")
    dom = (domain or MOEMAIL_DOMAIN).strip(".")
    pre = (prefix or f"grok-{secrets.token_hex(4)}").lower()

    mailbox = moemail_create_mailbox(
        name=pre,
        domain=dom,
        expiry_ms=expiry_ms if expiry_ms is not None else MOEMAIL_EXPIRY_MS,
        api_key=key,
        base_url=base,
    )
    email_id = mailbox["id"]
    address = mailbox["email"]

    class _MoeMailReceiver:
        def __init__(self, email: str, email_id: str, api_key: str | None, base_url: str | None):
            self.email = email
            self.email_id = email_id
            self.api_key = api_key
            self.base_url = base_url or "https://moemail.521884.xyz"

        def wait_for_code(self, timeout: float = 120) -> str:
            from moemail import moemail_fetch_messages
            import re as _re

            deadline = time.time() + timeout
            poll = 1.5
            while time.time() < deadline:
                try:
                    messages = moemail_fetch_messages(
                        self.email_id,
                        api_key=self.api_key,
                        base_url=self.base_url,
                        include_details=True,
                    )
                    for item in messages:
                        # Prefer xAI AAA-BBB codes first.
                        text = "\n".join(
                            str(item.get(k) or "")
                            for k in (
                                "subject",
                                "content",
                                "html",
                                "from_address",
                                "from",
                            )
                        )
                        match = _re.search(
                            r"\b([A-Z0-9]{3})-([A-Z0-9]{3})\b", text, flags=_re.I
                        )
                        if match:
                            return "".join(match.groups()).upper()
                        # Also accept plain 6-char alnum codes from xAI mails.
                        match2 = _re.search(
                            r"\b([A-Z0-9]{6})\b", text, flags=_re.I
                        )
                        if match2 and "x.ai" in text.lower():
                            return match2.group(1).upper()
                        extracted = item.get("extracted") or {}
                        codes = extracted.get("codes") or []
                        for code in codes:
                            clean = str(code).replace("-", "").strip().upper()
                            if len(clean) == 6 and _re.fullmatch(r"[A-Z0-9]{6}", clean):
                                return clean
                except Exception:
                    pass
                time.sleep(poll)
                poll = min(3.0, poll + 0.25)
            raise RuntimeError("timeout waiting for xAI email verification code")

    return address, _MoeMailReceiver(address, email_id, api_key=key, base_url=base)


def _proxy_url() -> str:
    from moemail import normalize_proxy_config
    from config import XAI_PROXY

    cfg = normalize_proxy_config(XAI_PROXY or None)
    return cfg["proxy"] if cfg else ""


# --------------------------------------------------------------------------- #
# registration flow
# --------------------------------------------------------------------------- #
def _start_one_registration(
    *,
    yescaptcha_key: str,
    proxy: str,
    moemail_api_key: str | None = None,
    moemail_base_url: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    batch_id: str | None = None,
    batch_index: int | None = None,
    batch_total: int | None = None,
    start_delay: float = 0.0,
) -> dict[str, Any]:
    """Create one session object and spawn its worker thread."""
    if start_delay > 0:
        time.sleep(start_delay)

    try:
        email, receiver = _make_email_receiver(
            api_key=moemail_api_key,
            base_url=moemail_base_url,
            prefix=prefix,
            domain=domain,
            expiry_ms=expiry_ms,
        )
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "error": str(e)}

    # xAI password rules: mix upper/lower/digit/symbol.
    password = f"Aa{os.urandom(5).hex()}9!xZ"
    sid = f"gba_{uuid.uuid4().hex[:16]}"

    sess = {
        "id": sid,
        "status": "started",
        "created_at": _now(),
        "updated_at": _now(),
        "email": email,
        "password": password,
        "message": f"started; email={email}",
        "sso": None,
        "oauth": None,
        "auth_json": None,
        "error": None,
        "yescaptcha_key": yescaptcha_key,
        "proxy": proxy or None,
        "adapter_build": ADAPTER_BUILD,
        "batch_id": batch_id,
        "batch_index": batch_index,
        "batch_total": batch_total,
    }
    with _lock:
        _sessions[sid] = sess
        if batch_id and batch_id in _batches:
            _batches[batch_id]["session_ids"].append(sid)
            _batches[batch_id]["updated_at"] = _now()

    threading.Thread(
        target=_run_registration,
        args=(sid, yescaptcha_key, proxy or "", receiver),
        daemon=True,
        name=f"gba-reg-{sid[-8:]}",
    ).start()
    return {"ok": True, **_compact_session(sess)}


def start_registration(
    *,
    yescaptcha_key: str | None = None,
    proxy: str | None = None,
    moemail_api_key: str | None = None,
    moemail_base_url: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    count: int | None = None,
    concurrency: int | None = None,
    stagger_ms: int | None = None,
) -> dict[str, Any]:
    """Start one or many registration sessions (multi-thread).

    ``count`` > 1 enables batch mode. Workers are capped by ``concurrency``.
    """
    try:
        ensure_xconsole()
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "error": str(e)}

    _clean_old_sessions()

    key = (yescaptcha_key or YESCAPTCHA_KEY or "").strip()
    if not key:
        return {
            "ok": False,
            "error": "YESCAPTCHA_KEY is required (set GROK2API_YESCAPTCHA_KEY or pass yescaptcha_key)",
        }

    try:
        n = int(count if count is not None else 1)
    except (TypeError, ValueError):
        n = 1
    n = max(1, min(n, MAX_BATCH_COUNT))

    try:
        workers = int(
            concurrency
            if concurrency is not None
            else DEFAULT_CONCURRENCY
        )
    except (TypeError, ValueError):
        workers = DEFAULT_CONCURRENCY
    workers = max(1, min(workers, MAX_CONCURRENCY, n))

    try:
        stagger = int(stagger_ms if stagger_ms is not None else 400)
    except (TypeError, ValueError):
        stagger = 400
    stagger = max(0, min(stagger, 10_000))

    proxy_val = (proxy or _proxy_url() or "").strip()

    # Single job — keep original response shape for UI compatibility.
    if n == 1:
        return _start_one_registration(
            yescaptcha_key=key,
            proxy=proxy_val,
            moemail_api_key=moemail_api_key,
            moemail_base_url=moemail_base_url,
            prefix=prefix,
            domain=domain,
            expiry_ms=expiry_ms,
        )

    batch_id = f"batch_{uuid.uuid4().hex[:12]}"
    batch = {
        "id": batch_id,
        "status": "running",
        "created_at": _now(),
        "updated_at": _now(),
        "count": n,
        "concurrency": workers,
        "stagger_ms": stagger,
        "session_ids": [],
        "adapter_build": ADAPTER_BUILD,
        "message": f"batch started count={n} concurrency={workers}",
        "error": None,
    }
    with _lock:
        _batches[batch_id] = batch

    def _spawn_batch() -> None:
        from concurrent.futures import ThreadPoolExecutor, as_completed

        results: list[dict[str, Any]] = []
        errors: list[str] = []

        def _job(i: int) -> dict[str, Any]:
            delay = (stagger / 1000.0) * ((i - 1) % max(1, workers))
            return _start_one_registration(
                yescaptcha_key=key,
                proxy=proxy_val,
                moemail_api_key=moemail_api_key,
                moemail_base_url=moemail_base_url,
                prefix=prefix,
                domain=domain,
                expiry_ms=expiry_ms,
                batch_id=batch_id,
                batch_index=i,
                batch_total=n,
                start_delay=delay,
            )

        try:
            with ThreadPoolExecutor(
                max_workers=workers, thread_name_prefix=f"gba-batch-{batch_id[-6:]}"
            ) as pool:
                futs = {pool.submit(_job, i): i for i in range(1, n + 1)}
                for fut in as_completed(futs):
                    idx = futs[fut]
                    try:
                        r = fut.result()
                        results.append(r)
                        if not r.get("ok"):
                            errors.append(f"#{idx}: {r.get('error') or 'start failed'}")
                    except Exception as e:  # noqa: BLE001
                        errors.append(f"#{idx}: {e}")
        finally:
            with _lock:
                b = _batches.get(batch_id)
                if b is not None:
                    b["updated_at"] = _now()
                    b["status"] = "spawned"
                    b["spawned"] = len(b.get("session_ids") or [])
                    b["spawn_errors"] = errors
                    b["message"] = (
                        f"spawned {len(b.get('session_ids') or [])}/{n} "
                        f"(concurrency={workers})"
                        + (f"; errors={len(errors)}" if errors else "")
                    )
                    if errors and not b.get("session_ids"):
                        b["status"] = "error"
                        b["error"] = "; ".join(errors[:5])

    threading.Thread(
        target=_spawn_batch,
        daemon=True,
        name=f"gba-batch-{batch_id[-8:]}",
    ).start()

    # Give the spawner a brief moment so the first session ids are often present.
    time.sleep(min(0.35, 0.05 * workers + 0.05))
    with _lock:
        b = dict(_batches.get(batch_id) or batch)
        sids = list(b.get("session_ids") or [])
        sessions = [_compact_session(_sessions[s]) for s in sids if s in _sessions]

    return {
        "ok": True,
        "batch": True,
        "batch_id": batch_id,
        "count": n,
        "concurrency": workers,
        "stagger_ms": stagger,
        "session_ids": sids,
        "sessions": sessions,
        "adapter_build": ADAPTER_BUILD,
        "message": (
            f"batch started: count={n}, concurrency={workers}, "
            f"spawned={len(sids)}"
        ),
        # Back-compat: first session fields for old UI single-session path.
        **(sessions[0] if sessions else {"id": None, "status": "starting"}),
    }


def _run_registration(
    sid: str,
    yescaptcha_key: str,
    proxy: str,
    receiver: Any,
) -> None:
    sess = _sessions.get(sid)
    if not sess:
        return

    def update(status: str, message: str, **kwargs: Any) -> None:
        sess["status"] = status
        sess["message"] = message
        sess["updated_at"] = _now()
        sess.update(kwargs)

    email = str(sess.get("email") or "").strip().lower()
    password = sess["password"]
    sess["email"] = email
    client = None

    try:
        ensure_xconsole()
        from xconsole_client import (
            XConsoleAuthClient,
            YesCaptchaSolver,
            xai_oauth_login_protocol,
        )
        from xconsole_client import config as C
        from xconsole_client.oauth_protocol import extract_cookies_from_auth_client
        from xconsole_client.xai_oauth import (
            CLIPROXYAPI_GROK_HEADERS,
            build_cliproxyapi_auth_record,
        )
        import accounts
        from config import UPSTREAM_BASE

        update("registering", "visiting signup page")
        client = XConsoleAuthClient(
            debug=True,
            proxy=proxy or "",
            signup_url="https://accounts.x.ai/sign-up?redirect=grok-com",
        )
        client.visit_home()
        client.load_signup_page()

        sitekey = (
            getattr(client, "turnstile_sitekey", None)
            or getattr(C, "TURNSTILE_SITEKEY", None)
            or ""
        ).strip()
        website_url = (getattr(client, "signup_url", None) or C.SIGNUP_URL or "").strip()
        if not sitekey:
            raise RuntimeError(
                "Turnstile sitekey missing. Signup page scrape failed and "
                "config TURNSTILE_SITEKEY is empty."
            )

        endpoint = (
            os.environ.get("GROK2API_YESCAPTCHA_ENDPOINT")
            or os.environ.get("YESCAPTCHA_ENDPOINT")
            or ""
        ).strip() or None

        def _turnstile_progress(msg: str) -> None:
            update("solving_turnstile", f"Turnstile: {msg}")

        solver = YesCaptchaSolver(
            yescaptcha_key,
            endpoint=endpoint,
            timeout=float(os.environ.get("GROK2API_YESCAPTCHA_TIMEOUT", "180") or 180),
            debug=True,
            on_progress=_turnstile_progress,
            auto_fallback_endpoint=True,
        )
        print(
            f"[grok-build-auth] turnstile website_url={website_url} "
            f"sitekey={sitekey} endpoint={getattr(solver, '_endpoint', '?')}"
        )

        # Critical ordering:
        # 1) solve Turnstile first (slow, ~20-40s)
        # 2) send email code
        # 3) wait for mailbox code
        # 4) immediately verify + create_account
        # Old order verified the code then waited for captcha; create_account then
        # failed with WKE=email:invalid-validation-code because the code expired /
        # was single-use after the slow captcha step.
        update("solving_turnstile", "solving Turnstile via YesCaptcha (before email code)")
        try:
            turnstile = solver.solve_turnstile(
                website_url=website_url,
                website_key=sitekey,
                premium=True,
                fallback_non_premium=True,
            )
        except Exception as captcha_err:
            alt_url = "https://accounts.x.ai/sign-up?redirect=cloud-console"
            if website_url.rstrip("/") == alt_url.rstrip("/"):
                alt_url = "https://accounts.x.ai/sign-up?redirect=grok-com"
            update(
                "solving_turnstile",
                f"primary Turnstile failed ({captcha_err}); retry {alt_url}",
            )
            turnstile = solver.solve_turnstile(
                website_url=alt_url,
                website_key=sitekey,
                premium=False,
                fallback_non_premium=True,
            )
        if not turnstile:
            raise RuntimeError("YesCaptcha returned empty Turnstile token")

        # Password can be validated any time before create; do it while warm.
        client.validate_password(email, password)

        update("registering", "sending email validation code")
        send_res = client.create_email_validation_code(email)
        if hasattr(send_res, "ok") and send_res.ok is False:
            print(
                f"[grok-build-auth] CreateEmailValidationCode ok=False "
                f"http={getattr(send_res, 'http_status', None)} "
                f"grpc={getattr(send_res, 'grpc_status', None)}"
            )

        update("waiting_email", "waiting for xAI verification code")
        code = receiver.wait_for_code(timeout=120)
        code = str(code or "").strip().upper().replace(" ", "").replace("-", "")
        if len(code) != 6:
            raise RuntimeError(
                f"invalid email verification code shape: {code!r} "
                f"(expect 6 alnum chars)"
            )
        update("registering", f"code received: {code}; verifying + creating immediately")

        # Prefer empty castle token (YesCaptcha cannot mint Castle fingerprints).
        # Retry create_account once with a fresh Turnstile + fresh email code when
        # the first flight is a structured hard error (expired code / turnstile).
        create_attempts = 2
        res = None
        sc: list[str] = []
        rsc_body = ""
        rsc_preview = ""
        http_status = 0
        signup_err: str | None = None
        for ca in range(1, create_attempts + 1):
            if ca > 1:
                # Full refresh path for invalid code / captcha failures.
                update(
                    "solving_turnstile",
                    f"create_account hard error ({signup_err}); refreshing Turnstile+email code",
                )
                try:
                    turnstile = solver.solve_turnstile(
                        website_url=website_url,
                        website_key=sitekey,
                        premium=True,
                        fallback_non_premium=True,
                    )
                except Exception as captcha_err:  # noqa: BLE001
                    print(f"[grok-build-auth] turnstile refresh failed: {captcha_err}")
                    break
                # New email code required after invalid-validation-code.
                try:
                    client.create_email_validation_code(email)
                    update("waiting_email", "waiting for fresh xAI verification code")
                    code = receiver.wait_for_code(timeout=120)
                    code = (
                        str(code or "")
                        .strip()
                        .upper()
                        .replace(" ", "")
                        .replace("-", "")
                    )
                    if len(code) != 6:
                        raise RuntimeError(f"fresh email code invalid: {code!r}")
                    update("registering", f"fresh code received: {code}")
                except Exception as mail_err:  # noqa: BLE001
                    print(f"[grok-build-auth] email code refresh failed: {mail_err}")
                    break

            # verify immediately before create_account (same second when possible)
            try:
                vres = client.verify_email_validation_code(email, code)
                print(
                    f"[grok-build-auth] VerifyEmailValidationCode "
                    f"ok={getattr(vres, 'ok', None)} "
                    f"http={getattr(vres, 'http_status', None)} "
                    f"grpc={getattr(vres, 'grpc_status', None)}"
                )
            except Exception as v_err:  # noqa: BLE001
                print(f"[grok-build-auth] verify_email error: {v_err}")

            update(
                "creating_account",
                f"creating xAI account (attempt {ca}/{create_attempts})",
            )
            res = client.create_account(
                email=email,
                given_name="User",
                family_name="Grok",
                password=password,
                email_validation_code=code,
                turnstile_token=turnstile,
                castle_request_token="",
                conversion_id=str(uuid.uuid4()),
            )
            sc = list(getattr(res, "set_cookies", None) or [])
            rsc_body = getattr(res, "rsc_body", "") or ""
            rsc_preview = rsc_body[:800]
            http_status = int(getattr(res, "http_status", 0) or 0)
            try:
                signup_err = client.extract_signup_error(rsc_body)
            except Exception:
                signup_err = None
            print(f"[grok-build-auth] create_account HTTP={http_status}")
            print(f"[grok-build-auth] create_account set-cookies count={len(sc)}")
            print(f"[grok-build-auth] create_account ok={bool(getattr(res, 'ok', False))}")
            print(f"[grok-build-auth] create_account error={signup_err!r}")
            print(f"[grok-build-auth] create_account rsc_body preview: {rsc_preview}")
            print(f"[grok-build-auth] adapter_build={ADAPTER_BUILD}")
            sess["create_account_http"] = http_status
            sess["create_account_ok_flag"] = bool(getattr(res, "ok", False))
            sess["create_account_set_cookies"] = len(sc)
            sess["create_account_error"] = signup_err

            # Persist full body for offline diagnosis (truncated).
            try:
                debug_path = (
                    ROOT / "data" / "register_sso" / f"{sid}.create_account.rsc.txt"
                )
                debug_path.parent.mkdir(parents=True, exist_ok=True)
                debug_path.write_text(rsc_body[:200_000], encoding="utf-8")
            except Exception:
                pass

            if http_status != 200:
                # Non-200 is terminal for this attempt; try once more only on 5xx.
                if http_status >= 500 and ca < create_attempts:
                    continue
                raise RuntimeError(
                    "create_account transport failed. "
                    f"adapter_build={ADAPTER_BUILD}; HTTP {http_status}; "
                    f"error={signup_err!r}; set_cookies={len(sc)}; "
                    f"body_preview={rsc_preview!r}"
                )

            # Structured hard error: retry with fresh captcha when recoverable.
            if signup_err:
                recoverable = any(
                    x in str(signup_err).lower()
                    for x in (
                        "turnstile",
                        "rate_limited",
                        "rate limit",
                        "captcha",
                        "account_signup_error",
                    )
                )
                if recoverable and ca < create_attempts:
                    continue
                raise RuntimeError(
                    "create_account rejected by xAI. "
                    f"adapter_build={ADAPTER_BUILD}; HTTP {http_status}; "
                    f"error={signup_err!r}; set_cookies={len(sc)}; "
                    f"body_preview={rsc_preview!r}"
                )

            # HTTP 200 without structured error — proceed even if res.ok is False
            # due to historical false negatives on RSC-only flights.
            break

        update(
            "fetching_sso",
            f"create_account HTTP {http_status} accepted; extracting SSO [{ADAPTER_BUILD}]",
        )

        sso = None
        try:
            sso = client.fetch_sso_token(
                email=email, password=password, save=True, retries=4
            )
        except Exception as sso_fetch_err:  # noqa: BLE001
            print(f"[grok-build-auth] fetch_sso_token error: {sso_fetch_err}")

        if not sso:
            try:
                from xconsole_client.sso import (
                    SSOExtractor,
                    parse_all_set_cookie_urls,
                    parse_sso_from_set_cookies,
                    parse_sso_jwt_url,
                    parse_sso_token_from_text,
                )

                sso = parse_sso_from_set_cookies(sc) or parse_sso_token_from_text(
                    rsc_body
                )
                if not sso and rsc_body:
                    print(
                        f"[grok-build-auth] set-cookie candidates="
                        f"{parse_all_set_cookie_urls(rsc_body)[:3]}"
                    )
                    print(
                        f"[grok-build-auth] primary set-cookie url="
                        f"{parse_sso_jwt_url(rsc_body)}"
                    )
                    extractor = SSOExtractor(
                        transport_request=client._request,
                        base_headers=client._base_headers,
                        cookie_jar=client._t.cookies,
                        debug=True,
                    )
                    sso = extractor.extract(
                        rsc_body, email=email, password=password, save=False
                    )
            except Exception as recover_err:  # noqa: BLE001
                print(f"[grok-build-auth] SSO recover failed: {recover_err}")

        # Current xAI create_account often returns only RSC chunks + CF cookies,
        # with no set-cookie JWT chain. Fall back to password CreateSession and
        # treat the returned session JWT as the sso cookie for sso_to_auth_json.
        if not sso:
            update(
                "fetching_sso",
                f"RSC has no sso chain; CreateSession password fallback [{ADAPTER_BUILD}]",
            )
            try:
                # Fresh turnstile for sign-in page improves CreateSession success.
                # Allow account propagation delay before first login attempt.
                time.sleep(2.0)
                signin_url = "https://accounts.x.ai/sign-in?redirect=grok-com"
                try:
                    signin_turnstile = solver.solve_turnstile(
                        website_url=signin_url,
                        website_key=sitekey,
                        premium=True,
                        fallback_non_premium=True,
                    )
                except Exception:
                    signin_turnstile = turnstile
                sso = client.obtain_session_via_password(
                    email=email,
                    password=password,
                    turnstile_token=signin_turnstile,
                    referer=signin_url,
                    retries=4,
                )
                # One more captcha + login if first CreateSession returned empty.
                if not sso:
                    try:
                        signin_turnstile = solver.solve_turnstile(
                            website_url=signin_url,
                            website_key=sitekey,
                            premium=False,
                            fallback_non_premium=True,
                        )
                        time.sleep(1.5)
                        sso = client.obtain_session_via_password(
                            email=email,
                            password=password,
                            turnstile_token=signin_turnstile,
                            referer=signin_url,
                            retries=2,
                        )
                    except Exception as cs2_err:  # noqa: BLE001
                        print(
                            f"[grok-build-auth] CreateSession second pass failed: {cs2_err}"
                        )
                print(
                    f"[grok-build-auth] CreateSession fallback sso="
                    f"{(sso[:60] if sso else None)}"
                )
            except Exception as cs_err:  # noqa: BLE001
                print(f"[grok-build-auth] CreateSession fallback failed: {cs_err}")

        print(f"[grok-build-auth] fetch_sso_token result: {sso[:60] if sso else None}")
        sess["sso"] = sso
        session_cookies = extract_cookies_from_auth_client(client)
        print(
            f"[grok-build-auth] session cookies after signup: "
            f"{sorted((session_cookies or {}).keys())}"
        )
        if sso:
            session_cookies = dict(session_cookies or {})
            session_cookies["sso"] = sso
            session_cookies["sso-rw"] = sso

        if not sso:
            raise RuntimeError(
                "SSO_COOKIE_MISSING after create_account. "
                f"adapter_build={ADAPTER_BUILD}; HTTP {http_status}; "
                f"create_ok={bool(getattr(res, 'ok', False))}; "
                f"signup_error={signup_err!r}; set_cookies={len(sc)}; "
                f"cookie_keys={sorted((session_cookies or {}).keys())}; "
                f"body_preview={rsc_preview!r}. "
                "Account may have been created, but neither RSC set-cookie chain "
                "nor CreateSession password fallback produced an sso cookie. "
                "Common causes: turnstile_failed, rate_limited, or account not yet "
                "visible to CreateSession."
            )

        # Required path: SSO/session JWT -> sso_to_auth_json device flow -> auth.json
        update(
            "importing",
            f"SSO obtained; converting via sso_to_auth_json [{ADAPTER_BUILD}]",
        )
        import sso_to_auth_json as sso_import

        token = sso_import.sso_to_token(sso)
        if not token or not token.get("access_token"):
            raise RuntimeError(
                "SSO obtained but sso_to_auth_json conversion failed "
                "(device verify/approve/token poll). "
                f"adapter_build={ADAPTER_BUILD}; sso_prefix={sso[:24]!r}"
            )
        _key, entry = sso_import.token_to_auth_entry(token, email=email)
        import_result = accounts.import_auth_payload(
            {
                "key": entry["key"],
                "auth_mode": entry.get("auth_mode", "oidc"),
                "email": entry.get("email") or email,
                "refresh_token": entry.get("refresh_token", ""),
                "expires_at": entry.get("expires_at"),
                "oidc_issuer": entry.get("oidc_issuer", "https://auth.x.ai"),
                "oidc_client_id": entry.get("oidc_client_id", ""),
            },
            merge=True,
        )
        if not import_result.get("ok"):
            raise RuntimeError(
                f"SSO json import failed: {import_result.get('error')}; "
                f"adapter_build={ADAPTER_BUILD}"
            )
        sess["auth_json"] = import_result
        sess["oauth"] = {
            "path": "sso_to_auth_json",
            "access_token": (token.get("access_token") or "")[:20] + "...",
            "refresh_token": bool(token.get("refresh_token")),
            "email": email,
        }
        update(
            "imported",
            f"imported via sso_to_auth_json "
            f"({len(import_result.get('imported') or [])} account(s)) "
            f"[{ADAPTER_BUILD}]",
        )
        return
    except Exception as exc:  # noqa: BLE001
        update("error", f"failed: {exc}", error=str(exc))
    finally:
        if client is not None:
            try:
                client.close()
            except Exception:
                pass


def list_registration_sessions() -> dict[str, Any]:
    _clean_old_sessions()
    with _lock:
        sessions = [_compact_session(s) for s in _sessions.values()]
        batches = []
        for b in _batches.values():
            sids = list(b.get("session_ids") or [])
            stats = _batch_stats(sids)
            batches.append({**b, **stats})
    return {
        "sessions": sessions,
        "batches": batches,
        "active": sum(
            1
            for s in sessions
            if s.get("status")
            not in ("imported", "error", "failed", "expired", "completed", "success")
        ),
    }


def get_registration_session(
    sid: str, *, include_auth_json: bool = False
) -> dict[str, Any] | None:
    with _lock:
        sess = _sessions.get(sid)
        if not sess:
            return None
        out = dict(sess)
    out.pop("_client", None)
    out.pop("_oauth_client", None)
    out.pop("password", None)
    out.pop("yescaptcha_key", None)
    if not include_auth_json:
        out.pop("auth_json", None)
    return out


def _batch_stats(session_ids: list[str]) -> dict[str, Any]:
    imported = error = running = 0
    for sid in session_ids:
        sess = _sessions.get(sid) or {}
        st = str(sess.get("status") or "")
        if st in ("imported", "success", "completed"):
            imported += 1
        elif st in ("error", "failed", "expired", "protocol_error", "protocol_blocked"):
            error += 1
        else:
            running += 1
    total = len(session_ids)
    done = imported + error
    status = "running"
    if total and done >= total:
        status = "done" if error == 0 else ("partial" if imported else "error")
    elif total and imported and error:
        status = "running"
    return {
        "total": total,
        "imported": imported,
        "error": error,
        "running": running,
        "done": done,
        "batch_status": status,
    }


def get_registration_batch(batch_id: str) -> dict[str, Any] | None:
    with _lock:
        b = _batches.get(batch_id)
        if not b:
            return None
        sids = list(b.get("session_ids") or [])
        stats = _batch_stats(sids)
        sessions = [_compact_session(_sessions[s]) for s in sids if s in _sessions]
        out = {**b, **stats, "sessions": sessions}
    return out


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #
def main() -> int:
    print("grok-build-auth adapter for grokcli-2api")
    result = start_registration()
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if not result.get("ok"):
        return 1

    sid = result["id"]
    deadline = time.time() + 600
    while time.time() < deadline:
        sess = get_registration_session(sid, include_auth_json=True)
        if not sess:
            print("session disappeared", file=sys.stderr)
            return 1
        status = sess.get("status")
        print(f"[{time.strftime('%H:%M:%S')}] {status}: {sess.get('message')}")
        if status in ("imported", "error"):
            print(json.dumps(sess, ensure_ascii=False, indent=2))
            return 0 if status == "imported" else 1
        time.sleep(5)

    print("timeout", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
