# -*- coding: utf-8 -*-
"""xAI OAuth2 / OIDC PKCE login helper.

This module implements the browser authorization-code flow used by Grok CLI-like
clients:

    1. Generate state / nonce / PKCE code_verifier + S256 code_challenge.
    2. Open https://auth.x.ai/oauth2/authorize?... in the browser.
    3. Receive http://127.0.0.1:<port>/callback?code=...&state=...
    4. Exchange the authorization code at https://auth.x.ai/oauth2/token.
    5. Save access_token / refresh_token / id_token and userinfo.

It is intentionally separate from the older account-signup + grok.com SSO cookie
flow in client.py/sso.py.  OAuth tokens authenticate with Bearer tokens; SSO
cookies authenticate browser-style grok.com requests.
"""
from __future__ import annotations

import base64
import hashlib
import json
import os
import secrets
import threading
import time
import webbrowser
from dataclasses import dataclass
from datetime import datetime, timezone
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from pathlib import Path
from typing import Any, Dict, List, Optional
from urllib.parse import parse_qs, urlencode, urlparse

import requests


ISSUER = "https://auth.x.ai"
AUTHORIZATION_ENDPOINT = f"{ISSUER}/oauth2/authorize"
TOKEN_ENDPOINT = f"{ISSUER}/oauth2/token"
USERINFO_ENDPOINT = f"{ISSUER}/oauth2/userinfo"

# Observed public client id from Grok/xAI CLI OAuth links.
DEFAULT_CLIENT_ID = "b1a00492-073a-47ea-816f-4c329264a828"

DEFAULT_SCOPES = [
    "openid",
    "profile",
    "email",
    "offline_access",
    "grok-cli:access",
    "api:access",
]

# CLIProxyAPI can consume xAI OAuth records as auth JSON files.  For Grok CLI /
# Build usage, the upstream is not api.x.ai credits billing; it is the Grok CLI
# chat proxy plus the same headers sent by the official @xai-official/grok CLI.
CLIPROXYAPI_GROK_BASE_URL = "https://cli-chat-proxy.grok.com/v1"
CLIPROXYAPI_GROK_HEADERS = {
    "X-XAI-Token-Auth": "xai-grok-cli",
    "x-grok-client-version": "0.2.93",
    "x-grok-client-identifier": "grok-shell",
}


def _b64url(raw: bytes) -> str:
    return base64.urlsafe_b64encode(raw).decode("ascii").rstrip("=")


def generate_code_verifier() -> str:
    # RFC 7636: 43-128 chars, unreserved URL-safe charset.
    return _b64url(secrets.token_bytes(48))


def code_challenge_s256(code_verifier: str) -> str:
    return _b64url(hashlib.sha256(code_verifier.encode("ascii")).digest())


def parse_jwt_payload(jwt_token: str) -> Optional[Dict[str, Any]]:
    try:
        parts = jwt_token.split(".")
        if len(parts) < 2:
            return None
        payload = parts[1] + "=" * (-len(parts[1]) % 4)
        return json.loads(base64.urlsafe_b64decode(payload))
    except Exception:
        return None


def default_output_dir() -> Path:
    return Path(__file__).resolve().parent.parent / "oauth_output"


@dataclass
class OAuthLoginResult:
    token: Dict[str, Any]
    userinfo: Dict[str, Any]
    id_token_payload: Optional[Dict[str, Any]]
    path: Optional[Path] = None
    cliproxyapi_path: Optional[Path] = None
    redirect_uri: str = ""

    @property
    def access_token(self) -> str:
        return str(self.token.get("access_token") or "")

    @property
    def refresh_token(self) -> str:
        return str(self.token.get("refresh_token") or "")

    @property
    def email(self) -> str:
        return str(self.userinfo.get("email") or "")


class _CallbackState:
    def __init__(self) -> None:
        self.event = threading.Event()
        self.code = ""
        self.state = ""
        self.error = ""
        self.error_description = ""


def _make_callback_handler(expected_state: str, sink: _CallbackState):
    class CallbackHandler(BaseHTTPRequestHandler):
        server_version = "XAIAuthCallback/1.0"

        def log_message(self, fmt: str, *args: Any) -> None:
            return

        def do_GET(self) -> None:  # noqa: N802 - stdlib naming
            parsed = urlparse(self.path)
            qs = parse_qs(parsed.query)
            sink.code = (qs.get("code") or [""])[0]
            sink.state = (qs.get("state") or [""])[0]
            sink.error = (qs.get("error") or [""])[0]
            sink.error_description = (qs.get("error_description") or [""])[0]

            ok = bool(sink.code) and sink.state == expected_state and not sink.error
            status = 200 if ok else 400
            title = "xAI OAuth login successful" if ok else "xAI OAuth login failed"
            body = (
                "<html><head><meta charset='utf-8'><title>{title}</title></head>"
                "<body style='font-family: sans-serif; padding: 2rem'>"
                "<h2>{title}</h2>"
                "<p>{message}</p>"
                "<p>You can close this window.</p>"
                "</body></html>"
            ).format(
                title=title,
                message=(
                    "Authorization code received by local client."
                    if ok
                    else "State mismatch, missing code, or provider returned an error."
                ),
            )
            data = body.encode("utf-8")
            self.send_response(status)
            self.send_header("Content-Type", "text/html; charset=utf-8")
            self.send_header("Content-Length", str(len(data)))
            self.end_headers()
            self.wfile.write(data)
            sink.event.set()

    return CallbackHandler


def build_authorization_url(
    *,
    client_id: str,
    redirect_uri: str,
    state: str,
    nonce: str,
    code_challenge: str,
    scopes: list[str],
    plan: str = "generic",
    referrer: str = "cli-proxy-api",
) -> str:
    params = {
        "client_id": client_id,
        "code_challenge": code_challenge,
        "code_challenge_method": "S256",
        "nonce": nonce,
        "plan": plan,
        "redirect_uri": redirect_uri,
        "referrer": referrer,
        "response_type": "code",
        "scope": " ".join(scopes),
        "state": state,
    }
    return AUTHORIZATION_ENDPOINT + "?" + urlencode(params)


def exchange_code_for_token(
    *,
    code: str,
    code_verifier: str,
    redirect_uri: str,
    client_id: str = DEFAULT_CLIENT_ID,
    timeout: float = 30.0,
    proxy: str = "",
) -> Dict[str, Any]:
    proxies = {"http": proxy, "https": proxy} if proxy else None
    data = {
        "grant_type": "authorization_code",
        "client_id": client_id,
        "code": code,
        "redirect_uri": redirect_uri,
        "code_verifier": code_verifier,
    }
    resp = requests.post(
        TOKEN_ENDPOINT,
        data=data,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        timeout=timeout,
        proxies=proxies,
    )
    if resp.status_code != 200:
        raise RuntimeError(f"token exchange failed: HTTP {resp.status_code}: {resp.text[:500]}")
    token = resp.json()
    now = int(time.time())
    if "expires_in" in token and "expires_at" not in token:
        try:
            token["expires_at"] = now + int(token["expires_in"])
        except Exception:
            pass
    return token


def refresh_access_token(
    refresh_token: str,
    *,
    client_id: str = DEFAULT_CLIENT_ID,
    timeout: float = 30.0,
    proxy: str = "",
) -> Dict[str, Any]:
    proxies = {"http": proxy, "https": proxy} if proxy else None
    data = {
        "grant_type": "refresh_token",
        "client_id": client_id,
        "refresh_token": refresh_token,
    }
    resp = requests.post(
        TOKEN_ENDPOINT,
        data=data,
        headers={"Content-Type": "application/x-www-form-urlencoded"},
        timeout=timeout,
        proxies=proxies,
    )
    if resp.status_code != 200:
        raise RuntimeError(f"refresh failed: HTTP {resp.status_code}: {resp.text[:500]}")
    token = resp.json()
    now = int(time.time())
    if "expires_in" in token and "expires_at" not in token:
        try:
            token["expires_at"] = now + int(token["expires_in"])
        except Exception:
            pass
    if "refresh_token" not in token:
        token["refresh_token"] = refresh_token
    return token


def fetch_userinfo(access_token: str, *, timeout: float = 30.0, proxy: str = "") -> Dict[str, Any]:
    if not access_token:
        return {}
    proxies = {"http": proxy, "https": proxy} if proxy else None
    resp = requests.get(
        USERINFO_ENDPOINT,
        headers={"Authorization": f"Bearer {access_token}"},
        timeout=timeout,
        proxies=proxies,
    )
    if resp.status_code != 200:
        return {"_error": f"HTTP {resp.status_code}", "_body": resp.text[:300]}
    return resp.json()


def save_oauth_record(
    token: Dict[str, Any],
    *,
    userinfo: Optional[Dict[str, Any]] = None,
    client_id: str = DEFAULT_CLIENT_ID,
    output_dir: Optional[str | Path] = None,
) -> Path:
    target = Path(output_dir) if output_dir else default_output_dir()
    target.mkdir(parents=True, exist_ok=True)

    id_payload = parse_jwt_payload(str(token.get("id_token") or ""))
    email = ""
    if userinfo:
        email = str(userinfo.get("email") or "")
    if not email and id_payload:
        email = str(id_payload.get("email") or "")
    safe_email = "".join(ch if ch.isalnum() or ch in "._-" else "_" for ch in email) or "unknown"

    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%SZ")
    path = target / f"xai_oauth_{safe_email}_{ts}.json"
    record = {
        "type": "xai",
        "provider": "xai",
        "client_id": client_id,
        "email": email,
        "token": token,
        "userinfo": userinfo or {},
        "id_token_payload": id_payload,
        "created_at": datetime.now(timezone.utc).isoformat(),
        # Convenient flat fields for tools that do not inspect token{}.
        "access_token": token.get("access_token", ""),
        "refresh_token": token.get("refresh_token", ""),
        "expires_at": token.get("expires_at", None),
        "scope": token.get("scope", ""),
    }
    path.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")
    return path


def _safe_email_for_filename(email: str) -> str:
    safe = "".join(ch if ch.isalnum() or ch in "._-@" else "_" for ch in email)
    return safe or "unknown"


def _iso_utc_from_unix(ts: Any) -> str:
    try:
        return datetime.fromtimestamp(int(ts), tz=timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ")
    except Exception:
        return ""


def build_cliproxyapi_auth_record(
    token: Dict[str, Any],
    *,
    userinfo: Optional[Dict[str, Any]] = None,
    redirect_uri: str = "",
    disabled: bool = False,
    base_url: str = CLIPROXYAPI_GROK_BASE_URL,
    headers: Optional[Dict[str, str]] = None,
) -> Dict[str, Any]:
    """Build a CLIProxyAPI-compatible xAI auth JSON record.

    The generated record intentionally uses ``cli-chat-proxy.grok.com`` by
    default.  That is the Grok CLI / Build channel used by tokens containing
    ``grok-cli:access``.  Using ``https://api.x.ai/v1`` instead routes requests
    to xAI API-credit billing and can return 402 even when Grok CLI works.
    """

    id_payload = parse_jwt_payload(str(token.get("id_token") or "")) or {}
    email = ""
    if userinfo:
        email = str(userinfo.get("email") or "")
    if not email:
        email = str(id_payload.get("email") or "")

    expires_at = token.get("expires_at")
    if expires_at is None and token.get("expires_in") is not None:
        try:
            expires_at = int(time.time()) + int(token["expires_in"])
        except Exception:
            expires_at = None

    merged_headers = dict(CLIPROXYAPI_GROK_HEADERS)
    if headers:
        merged_headers.update({str(k): str(v) for k, v in headers.items() if str(k).strip() and str(v).strip()})

    record = {
        "type": "xai",
        "auth_kind": "oauth",
        "email": email,
        "sub": str(id_payload.get("sub") or userinfo.get("sub") if userinfo else id_payload.get("sub") or ""),
        "access_token": token.get("access_token", ""),
        "refresh_token": token.get("refresh_token", ""),
        "id_token": token.get("id_token", ""),
        "token_type": token.get("token_type", "Bearer"),
        "expires_in": token.get("expires_in", None),
        "expired": _iso_utc_from_unix(expires_at),
        "last_refresh": datetime.now(timezone.utc).strftime("%Y-%m-%dT%H:%M:%SZ"),
        "redirect_uri": redirect_uri,
        "token_endpoint": TOKEN_ENDPOINT,
        "base_url": base_url,
        "disabled": disabled,
        "headers": merged_headers,
    }
    return record


def save_cliproxyapi_auth_record(
    token: Dict[str, Any],
    *,
    userinfo: Optional[Dict[str, Any]] = None,
    auth_dir: str | Path,
    redirect_uri: str = "",
    disabled: bool = False,
    base_url: str = CLIPROXYAPI_GROK_BASE_URL,
    headers: Optional[Dict[str, str]] = None,
) -> Path:
    """Write a CLIProxyAPI-ready ``xai-<email>.json`` auth file."""

    record = build_cliproxyapi_auth_record(
        token,
        userinfo=userinfo,
        redirect_uri=redirect_uri,
        disabled=disabled,
        base_url=base_url,
        headers=headers,
    )
    target = Path(auth_dir)
    target.mkdir(parents=True, exist_ok=True)
    email = str(record.get("email") or "")
    safe = _safe_email_for_filename(email)
    # Avoid "xai-xai..." when the address local-part already starts with "xai".
    lower = safe.lower()
    if lower.startswith("xai-") or lower.startswith("xai_") or lower.startswith("xai"):
        fname = safe
    else:
        fname = f"xai-{safe}"
    path = target / f"{fname}.json"
    path.write_text(json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8")
    return path


def _start_pkce_callback_server(
    *,
    client_id: str,
    scopes: list[str],
    host: str,
    port: int,
) -> tuple[ThreadingHTTPServer, _CallbackState, str, str, str, str]:
    """Start local callback server and return (server, sink, auth_url, redirect_uri, state, verifier)."""
    state = secrets.token_hex(16)
    nonce = secrets.token_hex(16)
    verifier = generate_code_verifier()
    challenge = code_challenge_s256(verifier)
    sink = _CallbackState()
    server = ThreadingHTTPServer((host, int(port)), _make_callback_handler(state, sink))
    actual_port = int(server.server_address[1])
    redirect_uri = f"http://{host}:{actual_port}/callback"
    auth_url = build_authorization_url(
        client_id=client_id,
        redirect_uri=redirect_uri,
        state=state,
        nonce=nonce,
        code_challenge=challenge,
        scopes=scopes,
    )
    thread = threading.Thread(target=server.serve_forever, name="xai-oauth-callback", daemon=True)
    thread.start()
    return server, sink, auth_url, redirect_uri, state, verifier


def _finalize_oauth_code(
    *,
    code: str,
    code_verifier: str,
    redirect_uri: str,
    client_id: str,
    proxy: str = "",
    output_dir: Optional[str | Path] = None,
    cliproxyapi_auth_dir: Optional[str | Path] = None,
    cliproxyapi_base_url: str = CLIPROXYAPI_GROK_BASE_URL,
    cliproxyapi_disabled: bool = False,
) -> OAuthLoginResult:
    token = exchange_code_for_token(
        code=code,
        code_verifier=code_verifier,
        redirect_uri=redirect_uri,
        client_id=client_id,
        proxy=proxy,
    )
    userinfo = fetch_userinfo(str(token.get("access_token") or ""), proxy=proxy)
    path = save_oauth_record(token, userinfo=userinfo, client_id=client_id, output_dir=output_dir)
    cliproxy_path: Optional[Path] = None
    if cliproxyapi_auth_dir:
        cliproxy_path = save_cliproxyapi_auth_record(
            token,
            userinfo=userinfo,
            auth_dir=cliproxyapi_auth_dir,
            redirect_uri=redirect_uri,
            disabled=cliproxyapi_disabled,
            base_url=cliproxyapi_base_url,
        )
    return OAuthLoginResult(
        token=token,
        userinfo=userinfo,
        id_token_payload=parse_jwt_payload(str(token.get("id_token") or "")),
        path=path,
        cliproxyapi_path=cliproxy_path,
        redirect_uri=redirect_uri,
    )


def _wait_oauth_code(sink: _CallbackState, state: str, redirect_uri: str, timeout: float) -> str:
    if not sink.event.wait(timeout):
        raise TimeoutError(f"timed out waiting for OAuth callback on {redirect_uri}")
    if sink.error:
        detail = sink.error_description or sink.error
        raise RuntimeError(f"authorization failed: {detail}")
    if sink.state != state:
        raise RuntimeError("authorization failed: state mismatch")
    if not sink.code:
        raise RuntimeError("authorization failed: missing code")
    return sink.code


def login_with_browser(
    *,
    client_id: str = DEFAULT_CLIENT_ID,
    scopes: Optional[list[str]] = None,
    host: str = "127.0.0.1",
    port: int = 0,
    no_browser: bool = False,
    timeout: float = 300.0,
    output_dir: Optional[str | Path] = None,
    proxy: str = "",
    cliproxyapi_auth_dir: Optional[str | Path] = None,
    cliproxyapi_base_url: str = CLIPROXYAPI_GROK_BASE_URL,
    cliproxyapi_disabled: bool = False,
) -> OAuthLoginResult:
    scopes = scopes or list(DEFAULT_SCOPES)
    server, sink, auth_url, redirect_uri, state, verifier = _start_pkce_callback_server(
        client_id=client_id, scopes=scopes, host=host, port=port,
    )
    try:
        print("\nOpen this URL to authorize xAI/Grok:\n")
        print(auth_url)
        print()
        if not no_browser:
            try:
                webbrowser.open(auth_url)
            except Exception:
                pass
        code = _wait_oauth_code(sink, state, redirect_uri, timeout)
        return _finalize_oauth_code(
            code=code,
            code_verifier=verifier,
            redirect_uri=redirect_uri,
            client_id=client_id,
            proxy=proxy,
            output_dir=output_dir,
            cliproxyapi_auth_dir=cliproxyapi_auth_dir,
            cliproxyapi_base_url=cliproxyapi_base_url,
            cliproxyapi_disabled=cliproxyapi_disabled,
        )
    finally:
        server.shutdown()
        server.server_close()


def _playwright_click_first(page: Any, selectors: list[str]) -> bool:
    for sel in selectors:
        try:
            loc = page.locator(sel)
            if loc.count() > 0 and loc.first.is_visible():
                loc.first.click(timeout=2000)
                return True
        except Exception:
            continue
    return False


def _playwright_fill_first(page: Any, selectors: list[str], value: str) -> bool:
    for sel in selectors:
        try:
            loc = page.locator(sel)
            if loc.count() > 0 and loc.first.is_visible():
                loc.first.fill(value, timeout=2000)
                return True
        except Exception:
            continue
    return False


def _playwright_drive_login(page: Any, email: str, password: str, sink: _CallbackState, deadline: float) -> None:
    """Drive auth.x.ai login/consent UI until local OAuth callback fires."""
    email_selectors = [
        'input[type="email"]',
        'input[name="email"]',
        'input[autocomplete="username"]',
        'input[id*="email" i]',
        'input[placeholder*="email" i]',
        'input[placeholder*="Email" i]',
    ]
    password_selectors = [
        'input[type="password"]',
        'input[name="password"]',
        'input[autocomplete="current-password"]',
    ]
    submit_selectors = [
        'button[type="submit"]',
        'button:has-text("Continue")',
        'button:has-text("Sign in")',
        'button:has-text("Sign In")',
        'button:has-text("Log in")',
        'button:has-text("Log In")',
        'button:has-text("Next")',
        'input[type="submit"]',
    ]
    consent_selectors = [
        'button:has-text("Authorize")',
        'button:has-text("Allow")',
        'button:has-text("Accept")',
        'button:has-text("Approve")',
        'button:has-text("Continue")',
        'button[type="submit"]',
    ]

    filled_email = False
    filled_password = False
    while time.time() < deadline and not sink.event.is_set():
        try:
            if not filled_email and _playwright_fill_first(page, email_selectors, email):
                filled_email = True
            if not filled_password and _playwright_fill_first(page, password_selectors, password):
                filled_password = True
                _playwright_click_first(page, submit_selectors)
            elif filled_email and not filled_password:
                # email-first multi-step login
                _playwright_click_first(page, submit_selectors)
            else:
                _playwright_click_first(page, consent_selectors)
        except Exception:
            pass
        page.wait_for_timeout(500)


def login_with_playwright(
    email: str,
    password: str,
    *,
    client_id: str = DEFAULT_CLIENT_ID,
    scopes: Optional[list[str]] = None,
    host: str = "127.0.0.1",
    port: int = 0,
    timeout: float = 180.0,
    headless: bool = True,
    output_dir: Optional[str | Path] = None,
    proxy: str = "",
    cliproxyapi_auth_dir: Optional[str | Path] = None,
    cliproxyapi_base_url: str = CLIPROXYAPI_GROK_BASE_URL,
    cliproxyapi_disabled: bool = False,
    session_cookies: Optional[Dict[str, str]] = None,
) -> OAuthLoginResult:
    """Complete xAI OAuth with Playwright.

    Preferred post-signup path: inject *session_cookies* (especially ``sso``)
    so the authorize/consent flow completes without re-typing credentials.
    Falls back to filling email/password when cookies are insufficient.
    """
    email = (email or "").strip()
    password = password or ""

    try:
        from playwright.sync_api import sync_playwright
    except ImportError as exc:
        raise RuntimeError(
            "playwright is required for automated OAuth. "
            "Install with: pip install playwright && playwright install chromium"
        ) from exc

    scopes = scopes or list(DEFAULT_SCOPES)
    server, sink, auth_url, redirect_uri, state, verifier = _start_pkce_callback_server(
        client_id=client_id, scopes=scopes, host=host, port=port,
    )
    deadline = time.time() + max(30.0, float(timeout))
    try:
        launch_kwargs: Dict[str, Any] = {"headless": headless}
        if proxy:
            launch_kwargs["proxy"] = {"server": proxy}

        with sync_playwright() as p:
            browser = p.chromium.launch(**launch_kwargs)
            try:
                context = browser.new_context(
                    user_agent=(
                        "Mozilla/5.0 (Windows NT 10.0; Win64; x64) "
                        "AppleWebKit/537.36 (KHTML, like Gecko) "
                        "Chrome/148.0.0.0 Safari/537.36"
                    ),
                    viewport={"width": 1280, "height": 900},
                )
                # Inject signup session cookies before hitting authorize.
                if session_cookies:
                    cookie_list = []
                    for name, value in session_cookies.items():
                        if not name or value is None:
                            continue
                        cookie_list.append(
                            {
                                "name": str(name),
                                "value": str(value),
                                "domain": ".x.ai",
                                "path": "/",
                            }
                        )
                        # also accounts host
                        cookie_list.append(
                            {
                                "name": str(name),
                                "value": str(value),
                                "domain": "accounts.x.ai",
                                "path": "/",
                            }
                        )
                    if cookie_list:
                        try:
                            context.add_cookies(cookie_list)
                        except Exception:
                            pass

                page = context.new_page()
                page.goto(auth_url, wait_until="domcontentloaded", timeout=int(min(timeout, 60) * 1000))
                # If still on login form, fill credentials.
                if email and password and not sink.event.is_set():
                    _playwright_drive_login(page, email, password, sink, deadline)
                # Auto-click consent/authorize if present.
                while time.time() < deadline and not sink.event.is_set():
                    try:
                        for sel in (
                            'button:has-text("Authorize")',
                            'button:has-text("Allow")',
                            'button:has-text("Continue")',
                            'button:has-text("Approve")',
                            'button[type="submit"]',
                        ):
                            loc = page.locator(sel)
                            if loc.count() > 0 and loc.first.is_visible():
                                loc.first.click(timeout=1500)
                                break
                    except Exception:
                        pass
                    page.wait_for_timeout(300)
            finally:
                try:
                    browser.close()
                except Exception:
                    pass

        remaining = max(1.0, deadline - time.time() + 2.0)
        code = _wait_oauth_code(sink, state, redirect_uri, remaining)
        return _finalize_oauth_code(
            code=code,
            code_verifier=verifier,
            redirect_uri=redirect_uri,
            client_id=client_id,
            proxy=proxy,
            output_dir=output_dir,
            cliproxyapi_auth_dir=cliproxyapi_auth_dir,
            cliproxyapi_base_url=cliproxyapi_base_url,
            cliproxyapi_disabled=cliproxyapi_disabled,
        )
    finally:
        server.shutdown()
        server.server_close()


def complete_build_oauth(
    email: str,
    password: str,
    *,
    cliproxyapi_auth_dir: Optional[str | Path] = None,
    cliproxyapi_base_url: str = CLIPROXYAPI_GROK_BASE_URL,
    headless: bool = True,
    timeout: float = 180.0,
    port: int = 0,
    proxy: str = "",
    interactive_fallback: bool = False,
    yescaptcha_key: Optional[str] = None,
    protocol: bool = True,
    debug: bool = False,
    session_cookies: Optional[Dict[str, str]] = None,
    auth_client: Any = None,
) -> OAuthLoginResult:
    """Obtain Grok Build/CLI OAuth tokens after protocol signup.

    Preference order:
      1) Pure HTTP protocol (reuse signup cookies, else CreateSession+YesCaptcha)
      2) Playwright auto-login (if protocol=False or protocol fails)
      3) Interactive system-browser fallback (if interactive_fallback=True)
    """
    key = (yescaptcha_key or os.environ.get("YESCAPTCHA_API_KEY") or "").strip()
    errors: list[str] = []

    if protocol:
        try:
            from .oauth_protocol import login_with_protocol
            return login_with_protocol(
                email,
                password,
                yescaptcha_key=key,
                proxy=proxy,
                debug=debug,
                cliproxyapi_auth_dir=str(cliproxyapi_auth_dir) if cliproxyapi_auth_dir else None,
                cliproxyapi_base_url=cliproxyapi_base_url,
                redirect_port=port or 56121,
                session_cookies=session_cookies,
                auth_client=auth_client,
            )
        except Exception as exc:
            errors.append(f"protocol OAuth failed: {exc}")
            print(f"Protocol OAuth failed ({exc})")

    try:
        return login_with_playwright(
            email,
            password,
            timeout=timeout,
            headless=headless,
            port=port,
            proxy=proxy,
            cliproxyapi_auth_dir=cliproxyapi_auth_dir,
            cliproxyapi_base_url=cliproxyapi_base_url,
            session_cookies=session_cookies,
        )
    except Exception as auto_err:
        errors.append(f"playwright OAuth failed: {auto_err}")
        if not interactive_fallback:
            raise RuntimeError("; ".join(errors)) from auto_err
        print(f"Playwright OAuth failed ({auto_err}); falling back to interactive browser login...")
        return login_with_browser(
            timeout=max(timeout, 300.0),
            port=port,
            proxy=proxy,
            cliproxyapi_auth_dir=cliproxyapi_auth_dir,
            cliproxyapi_base_url=cliproxyapi_base_url,
        )


def default_cliproxyapi_auth_dir() -> Path:
    """Resolve CLIProxyAPI auth directory.

    Order:
      1. ``CLIPROXYAPI_AUTH_DIR`` environment variable
      2. ``./cliproxyapi_auth`` under the project root (created on write)
    """
    env = (os.environ.get("CLIPROXYAPI_AUTH_DIR") or "").strip()
    if env:
        return Path(env).expanduser()
    return Path(__file__).resolve().parent.parent / "cliproxyapi_auth"


def main() -> None:
    import argparse

    p = argparse.ArgumentParser(description="xAI/Grok OAuth PKCE login")
    p.add_argument("--port", type=int, default=0, help="Local callback port; 0 = random free port")
    p.add_argument("--host", default="127.0.0.1", help="Local callback host")
    p.add_argument("--no-browser", action="store_true", help="Print auth URL only; do not open browser")
    p.add_argument("--timeout", type=float, default=300.0, help="Callback wait timeout seconds")
    p.add_argument("--client-id", default=os.getenv("XAI_OAUTH_CLIENT_ID", DEFAULT_CLIENT_ID))
    p.add_argument("--scope", action="append", help="Override scopes; may be repeated")
    p.add_argument("--output-dir", default=None)
    p.add_argument(
        "--cliproxyapi-auth-dir",
        default=None,
        help=(
            "Also write CLIProxyAPI-ready xai-<email>.json into this auth dir. "
            "The exported record defaults to Grok CLI chat proxy, not api.x.ai credits."
        ),
    )
    p.add_argument(
        "--cliproxyapi-base-url",
        default=CLIPROXYAPI_GROK_BASE_URL,
        help="Base URL for the optional CLIProxyAPI auth export.",
    )
    p.add_argument(
        "--cliproxyapi-disabled",
        action="store_true",
        help="Mark the optional CLIProxyAPI auth export as disabled.",
    )
    p.add_argument("--proxy", default=os.getenv("HTTPS_PROXY") or os.getenv("HTTP_PROXY") or "")
    args = p.parse_args()

    result = login_with_browser(
        client_id=args.client_id,
        scopes=args.scope,
        host=args.host,
        port=args.port,
        no_browser=args.no_browser,
        timeout=args.timeout,
        output_dir=args.output_dir,
        proxy=args.proxy,
        cliproxyapi_auth_dir=args.cliproxyapi_auth_dir,
        cliproxyapi_base_url=args.cliproxyapi_base_url,
        cliproxyapi_disabled=args.cliproxyapi_disabled,
    )
    print("xAI OAuth login successful")
    if result.email:
        print(f"email: {result.email}")
    print(f"access_token: {result.access_token[:24]}...")
    if result.refresh_token:
        print(f"refresh_token: {result.refresh_token[:24]}...")
    if result.path:
        print(f"saved: {result.path}")
    if result.cliproxyapi_path:
        print(f"cliproxyapi_auth: {result.cliproxyapi_path}")


if __name__ == "__main__":
    main()
