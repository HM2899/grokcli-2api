#!/usr/bin/env python3
"""grok-build-auth — 一键注册 x.ai 账号 + SSO + Grok Build OAuth（CLIProxyAPI 可用）

流程:
  1) 协议注册（邮箱验证 + Turnstile + create_account）
  2) 提取 SSO
  3) xAI OAuth PKCE（含 grok-cli:access）
  4) 导出 CLIProxyAPI auth：cli-chat-proxy.grok.com + grok-cli headers
     → 可直接用 grok-4.5 走 Build/CLI 编码通道

环境变量（按需设置）:
    YESCAPTCHA_API_KEY     YesCaptcha API key (Turnstile 打码)
    TEMPMAIL_API_KEY       Tempmail.lol API key (邮箱后端)
    CLOUDFLARE_API_TOKEN   Cloudflare API token (alias_mail 邮箱后端)
    CLIPROXYAPI_AUTH_DIR   CLIProxyAPI data/auth 目录（可选）
    HTTPS_PROXY / HTTP_PROXY  代理（OAuth 换 token / Playwright 可选）
"""
from __future__ import annotations

import sys
import os
import uuid
import json
import base64
import time
import threading
import argparse
from concurrent.futures import ThreadPoolExecutor, as_completed
from pathlib import Path
from typing import Optional

_ROOT = Path(__file__).resolve().parent
sys.path.insert(0, str(_ROOT))

# Load local .env if present (optional dependency).
try:
    from dotenv import load_dotenv

    load_dotenv(_ROOT / ".env")
except Exception:
    pass

from xconsole_client import XConsoleAuthClient, YesCaptchaSolver, config as C
from xconsole_client.xai_oauth import (
    CLIPROXYAPI_GROK_BASE_URL,
    complete_build_oauth,
    default_cliproxyapi_auth_dir,
)
from xconsole_client.oauth_protocol import extract_cookies_from_auth_client

# -- secrets from environment only ---------------------------------------
YESCAPTCHA_KEY = os.environ.get("YESCAPTCHA_API_KEY", "")
TEMPMAIL_KEY = os.environ.get("TEMPMAIL_API_KEY", "")
SIGNUP_URL = "https://accounts.x.ai/sign-up?redirect=grok-com"
PROXY = os.environ.get("HTTPS_PROXY") or os.environ.get("HTTP_PROXY") or ""

_results_lock = threading.Lock()
_cf_lock = threading.Lock()
_oauth_lock = threading.Lock()  # Playwright/browser OAuth is safer serialized
_results: list[dict] = []
_done = 0
_total = 0
_t0 = 0.0


def _log(i: int, msg: str):
    elapsed = time.time() - _t0
    bar = f"[{_done}/{_total}]" if _total > 1 else ""
    print(f"  {bar} [#{i}] {msg}  ({elapsed:.0f}s)")


def _make_email_provider(backend: str):
    """Return (email, receiver) — receiver has .wait_for_code(timeout)."""
    if backend == "tempmail":
        if not TEMPMAIL_KEY:
            raise RuntimeError("TEMPMAIL_API_KEY 环境变量未设置")
        from xconsole_client.tempmail_transport import TempmailInbox
        inbox = TempmailInbox(api_key=TEMPMAIL_KEY, prefix="xai", debug=False)
        email = inbox.create()
        return email, inbox
    elif backend == "cloudflare":
        from xconsole_client.mailbox import AliasMailAccount, AliasMailCodeReceiver
        with _cf_lock:
            cf = AliasMailAccount.ensure_cf()
            alloc = AliasMailAccount(cf)
            address = alloc.create(prefix="xai")
        receiver = AliasMailCodeReceiver(cf, address=address, timeout=120, interval=3, since_now=True)
        return address, receiver
    else:
        raise ValueError(f"unknown email backend: {backend}")


def _save_account_bundle(result: dict, output_dir: Path) -> Path:
    """Persist a combined signup+oauth record for later tooling."""
    output_dir.mkdir(parents=True, exist_ok=True)
    email = str(result.get("email") or "unknown")
    safe = "".join(ch if ch.isalnum() or ch in "._-@" else "_" for ch in email) or "unknown"
    ts = time.strftime("%Y%m%dT%H%M%SZ", time.gmtime())
    path = output_dir / f"account_{safe}_{ts}.json"
    path.write_text(json.dumps(result, ensure_ascii=False, indent=2), encoding="utf-8")
    return path


def register_one(
    index: int,
    email_backend: str = "tempmail",
    *,
    do_oauth: bool = True,
    oauth_headless: bool = True,
    oauth_timeout: float = 180.0,
    oauth_interactive_fallback: bool = False,
    oauth_protocol: bool = True,
    oauth_debug: bool = False,
    cliproxyapi_auth_dir: Optional[str | Path] = None,
    cliproxyapi_base_url: str = CLIPROXYAPI_GROK_BASE_URL,
    accounts_output_dir: Optional[str | Path] = None,
) -> dict:
    """Run signup (+ optional Build OAuth export). Thread-safe."""
    if not YESCAPTCHA_KEY:
        return {
            "email": "",
            "password": "",
            "sso": None,
            "oauth_access_token": None,
            "cliproxyapi_auth": None,
            "error": "YESCAPTCHA_API_KEY 环境变量未设置",
        }

    # Per-client signup_url — never mutate global C.SIGNUP_URL under concurrency.
    c = XConsoleAuthClient(debug=False, signup_url=SIGNUP_URL)
    email = ""
    password = ""
    sso = None

    try:
        # 1. warm-up + scrape
        c.visit_home()
        c.load_signup_page()
        _log(index, "cookie + scrape OK")

        # 2. email
        email, receiver = _make_email_provider(email_backend)
        password = f"Pw{os.urandom(6).hex()}!a#A"
        _log(index, f"email: {email}")

        c.create_email_validation_code(email)
        code = receiver.wait_for_code(timeout=120)
        _log(index, f"code: {code}")
        c.verify_email_validation_code(email, code)
        c.validate_password(email, password)
        _log(index, "email verified")

        # 3. turnstile
        solver = YesCaptchaSolver(YESCAPTCHA_KEY)
        turnstile = solver.solve_turnstile(
            website_url=SIGNUP_URL, website_key=C.TURNSTILE_SITEKEY, premium=True)
        _log(index, f"Turnstile {len(turnstile)} chars")

        # 4. create account
        res = c.create_account(
            email=email, given_name="Test", family_name="User",
            password=password, email_validation_code=code,
            turnstile_token=turnstile, castle_request_token="",
            conversion_id=str(uuid.uuid4()),
        )
        if not res.ok:
            _log(index, f"FAIL create_account HTTP {res.http_status}")
            return {
                "email": email,
                "password": password,
                "sso": None,
                "oauth_access_token": None,
                "cliproxyapi_auth": None,
                "error": f"HTTP {res.http_status}",
            }
        _log(index, "account created")

        # 5. SSO (retries + RSC chain + grok.com fallback inside client)
        sso = c.fetch_sso_token(email=email, password=password, save=True, retries=3)
        if not sso:
            _log(index, "FAIL SSO extraction")
            return {
                "email": email,
                "password": password,
                "sso": None,
                "oauth_access_token": None,
                "cliproxyapi_auth": None,
                "error": "SSO failed",
            }
        payload = json.loads(base64.urlsafe_b64decode(sso.split(".")[1] + "=="))
        _log(index, f"SSO saved  session_id={payload.get('session_id', '?')[:12]}...")

        result = {
            "email": email,
            "password": password,
            "sso": sso,
            "oauth_access_token": None,
            "oauth_refresh_token": None,
            "oauth_record": None,
            "cliproxyapi_auth": None,
            "build_base_url": cliproxyapi_base_url,
            "error": None,
        }

        # 6. OAuth → CLIProxyAPI Grok Build path (coding-ready)
        if do_oauth:
            auth_dir = Path(cliproxyapi_auth_dir) if cliproxyapi_auth_dir else default_cliproxyapi_auth_dir()
            # Reuse signup session cookies so OAuth can skip password login when possible.
            session_cookies = extract_cookies_from_auth_client(c)
            # Grok SSO JWT (from fetch_sso_token) also works as accounts.x.ai `sso` cookie.
            if sso:
                session_cookies = dict(session_cookies or {})
                session_cookies.setdefault("sso", sso)
            _log(index, f"OAuth Build path → {auth_dir}  (cookies={len(session_cookies)})")
            with _oauth_lock:
                oauth = complete_build_oauth(
                    email,
                    password,
                    cliproxyapi_auth_dir=auth_dir,
                    cliproxyapi_base_url=cliproxyapi_base_url,
                    headless=oauth_headless,
                    timeout=oauth_timeout,
                    proxy=PROXY,
                    interactive_fallback=oauth_interactive_fallback,
                    yescaptcha_key=YESCAPTCHA_KEY,
                    protocol=oauth_protocol,
                    debug=oauth_debug,
                    session_cookies=session_cookies,
                    auth_client=c,
                )
            result["oauth_access_token"] = oauth.access_token
            result["oauth_refresh_token"] = oauth.refresh_token
            result["oauth_record"] = str(oauth.path) if oauth.path else None
            result["cliproxyapi_auth"] = str(oauth.cliproxyapi_path) if oauth.cliproxyapi_path else None
            _log(
                index,
                f"Build OAuth OK  access={oauth.access_token[:20]}...  "
                f"cliproxy={oauth.cliproxyapi_path.name if oauth.cliproxyapi_path else '?'}",
            )
        else:
            _log(index, "OAuth skipped (--no-oauth)")

        if accounts_output_dir:
            bundle = _save_account_bundle(result, Path(accounts_output_dir))
            result["account_bundle"] = str(bundle)

        return result

    except Exception as e:
        _log(index, f"ERROR: {e}")
        return {
            "email": email,
            "password": password,
            "sso": sso,
            "oauth_access_token": None,
            "cliproxyapi_auth": None,
            "error": str(e),
        }
    finally:
        c.close()
        with _results_lock:
            global _done
            _done += 1


def main():
    global _total, _t0
    default_auth = str(default_cliproxyapi_auth_dir())
    p = argparse.ArgumentParser(
        description="grok-build-auth: x.ai register + SSO + Grok Build OAuth (CLIProxyAPI-ready)",
    )
    p.add_argument("-n", "--count", type=int, default=1, help="账号数量")
    p.add_argument("-t", "--threads", type=int, default=1, help="并发线程数（注册阶段；OAuth 串行）")
    p.add_argument(
        "-e", "--email",
        choices=["tempmail", "cloudflare"],
        default="tempmail",
        help="邮箱后端: tempmail | cloudflare",
    )
    p.add_argument(
        "--no-oauth",
        action="store_true",
        help="只注册+SSO，不走 Build OAuth / CLIProxyAPI 导出",
    )
    p.add_argument(
        "--cliproxyapi-auth-dir",
        default=default_auth,
        help=f"CLIProxyAPI auth 目录（默认: {default_auth}）",
    )
    p.add_argument(
        "--cliproxyapi-base-url",
        default=CLIPROXYAPI_GROK_BASE_URL,
        help="Build 上游 base_url（默认 cli-chat-proxy.grok.com/v1）",
    )
    p.add_argument(
        "--oauth-headed",
        action="store_true",
        help="Playwright 有头模式（仅非协议回退时使用）",
    )
    p.add_argument(
        "--oauth-timeout",
        type=float,
        default=180.0,
        help="OAuth 等待超时秒数",
    )
    p.add_argument(
        "--no-oauth-protocol",
        action="store_true",
        help="禁用纯协议 OAuth（默认用 YesCaptcha+CreateSession，不启浏览器）",
    )
    p.add_argument(
        "--oauth-interactive-fallback",
        action="store_true",
        help="协议/Playwright 失败时回退到系统浏览器手动登录",
    )
    p.add_argument(
        "--oauth-debug",
        action="store_true",
        help="打印协议 OAuth 调试日志",
    )
    p.add_argument(
        "--accounts-output-dir",
        default=str(Path(__file__).resolve().parent / "accounts_output"),
        help="合并账号记录输出目录",
    )
    args = p.parse_args()

    _total = args.count
    _t0 = time.time()
    threads = min(args.threads, args.count)
    do_oauth = not args.no_oauth

    print(
        f"grok-build-auth: {args.count} accounts, {threads} threads, email={args.email}, "
        f"oauth={'on' if do_oauth else 'off'}"
    )
    if do_oauth:
        print(f"  cliproxyapi-auth-dir: {args.cliproxyapi_auth_dir}")
        print(f"  build-base-url:       {args.cliproxyapi_base_url}")
    print()

    common_kwargs = dict(
        do_oauth=do_oauth,
        oauth_headless=not args.oauth_headed,
        oauth_timeout=args.oauth_timeout,
        oauth_interactive_fallback=args.oauth_interactive_fallback,
        oauth_protocol=not args.no_oauth_protocol,
        oauth_debug=args.oauth_debug,
        cliproxyapi_auth_dir=args.cliproxyapi_auth_dir,
        cliproxyapi_base_url=args.cliproxyapi_base_url,
        accounts_output_dir=args.accounts_output_dir,
    )

    if args.count == 1:
        result = register_one(1, email_backend=args.email, **common_kwargs)
        _results.append(result)
    else:
        with ThreadPoolExecutor(max_workers=threads) as ex:
            futures = [
                ex.submit(register_one, i, args.email, **common_kwargs)
                for i in range(1, args.count + 1)
            ]
            for f in as_completed(futures):
                _results.append(f.result())

    # summary
    ok_build = [r for r in _results if r.get("cliproxyapi_auth") or (r.get("sso") and not do_oauth)]
    ok_sso = [r for r in _results if r.get("sso")]
    fail = [r for r in _results if r.get("error")]
    print(f"\n{'=' * 50}")
    print(
        f"Done in {time.time() - _t0:.0f}s  |  "
        f"SSO OK: {len(ok_sso)}  BUILD OK: {len([r for r in _results if r.get('cliproxyapi_auth')])}  "
        f"FAIL: {len(fail)}"
    )
    print(f"{'=' * 50}")
    for r in _results:
        email = r.get("email") or "?"
        if r.get("cliproxyapi_auth"):
            print(f"  {email:40s}  BUILD  {r['cliproxyapi_auth']}")
        elif r.get("sso") and not do_oauth:
            print(f"  {email:40s}  SSO    {r['sso'][:36]}...")
        elif r.get("sso") and r.get("error"):
            print(f"  {email:40s}  SSO-ok OAuth-FAIL: {r.get('error')}")
        else:
            print(f"  {email:40s}  FAIL: {r.get('error', '?')}")


if __name__ == "__main__":
    main()
