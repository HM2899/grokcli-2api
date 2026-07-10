"""MoeMail helpers + proxy normalization for protocol registration.

Kept intentionally small: only the pieces used by ``grok_build_adapter``
(and optional admin proxy smoke tests). The legacy full-session
``email_registration`` flow was removed in favor of grok-build-auth.
"""
from __future__ import annotations

import re
from typing import Any
from urllib.parse import quote, unquote, urlparse, urlunparse

import httpx

from config import (
    MOEMAIL_API_KEY,
    MOEMAIL_BASE_URL,
    MOEMAIL_DOMAIN,
    MOEMAIL_EXPIRY_MS,
    XAI_PROXY,
    XAI_PROXY_PASSWORD,
    XAI_PROXY_USERNAME,
)


def _headers(api_key: str | None = None) -> dict[str, str]:
    key = api_key or MOEMAIL_API_KEY
    if not key:
        return {}
    return {"X-API-Key": key}


def normalize_proxy_config(
    proxy: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
) -> dict[str, Any] | None:
    """Normalize a proxy URL into curl/httpx-friendly forms."""
    raw = (proxy or XAI_PROXY or "").strip()
    if not raw:
        return None
    env_user = XAI_PROXY_USERNAME
    env_pass = XAI_PROXY_PASSWORD
    lower = raw.lower()
    if lower.startswith("soket5://"):
        raw = "socks5://" + raw.split("://", 1)[1]
    elif lower.startswith("socket5://"):
        raw = "socks5://" + raw.split("://", 1)[1]
    elif "://" not in raw:
        raw = f"http://{raw}"

    parsed = urlparse(raw)
    if parsed.scheme not in {"http", "https", "socks5", "socks5h"}:
        raise ValueError("proxy scheme must be http, https, socks5, or socks5h")
    if not parsed.netloc or not parsed.hostname:
        raise ValueError("proxy must include host and port")
    try:
        port = parsed.port
    except ValueError as e:
        raise ValueError("proxy port is invalid") from e
    proxy_user = (username if username is not None else "").strip()
    proxy_pass = (password if password is not None else "").strip()
    if not proxy_user and username is None:
        proxy_user = env_user
    if not proxy_pass and password is None:
        proxy_pass = env_pass
    if not proxy_user and parsed.username:
        proxy_user = unquote(parsed.username)
    if not proxy_pass and parsed.password:
        proxy_pass = unquote(parsed.password)

    if proxy_pass and not proxy_user:
        raise ValueError("proxy username is required when proxy password is set")

    host = parsed.hostname or ""
    if ":" in host and not host.startswith("["):
        host = f"[{host}]"
    if port is not None:
        host = f"{host}:{port}"
    proxy_no_auth = urlunparse(
        (
            parsed.scheme,
            host,
            parsed.path or "",
            parsed.params or "",
            parsed.query or "",
            parsed.fragment or "",
        )
    )
    proxy_auth = (proxy_user, proxy_pass) if proxy_user else None
    proxy_with_auth = proxy_no_auth
    if proxy_user:
        auth = quote(proxy_user, safe="")
        if proxy_pass:
            auth = f"{auth}:{quote(proxy_pass, safe='')}"
        proxy_with_auth = urlunparse(
            (
                parsed.scheme,
                f"{auth}@{host}",
                parsed.path or "",
                parsed.params or "",
                parsed.query or "",
                parsed.fragment or "",
            )
        )
    return {
        "proxy": proxy_with_auth,
        "curl_proxy": proxy_no_auth,
        "proxy_auth": proxy_auth,
    }


# Back-compat alias used by older adapter code paths.
_normalize_proxy_config = normalize_proxy_config


def _extract_codes_and_links(text: str) -> dict[str, list[str]]:
    codes = sorted(set(re.findall(r"(?<!\d)\d{6,8}(?!\d)", text or "")))
    links = sorted(set(re.findall(r"https?://[^\s\"'<>)]+", text or "")))
    return {"codes": codes, "links": links}


def _moemail_infer_domain(
    client: httpx.Client,
    base: str,
    *,
    api_key: str | None = None,
) -> str | None:
    try:
        resp = client.get(f"{base}/api/emails", headers=_headers(api_key))
        if resp.status_code >= 400:
            return None
        data = resp.json()
    except Exception:
        return None
    emails = data.get("emails") if isinstance(data, dict) else None
    if not isinstance(emails, list):
        return None
    for item in emails:
        if not isinstance(item, dict):
            continue
        address = item.get("email") or item.get("address")
        if isinstance(address, str) and "@" in address:
            return address.rsplit("@", 1)[1].strip() or None
    return None


def moemail_create_mailbox(
    *,
    name: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    api_key: str | None = None,
    base_url: str | None = None,
    proxy: str | None = None,  # accepted for API compat; unused by httpx path
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    if not (api_key or MOEMAIL_API_KEY):
        raise ValueError(
            "MoeMail API key missing. Set GROK2API_MOEMAIL_API_KEY or pass api_key."
        )

    base = (base_url or MOEMAIL_BASE_URL).rstrip("/")
    payload: dict[str, Any] = {
        "expiryTime": int(expiry_ms or MOEMAIL_EXPIRY_MS),
        "domain": domain or MOEMAIL_DOMAIN,
    }
    if name:
        payload["name"] = name

    with httpx.Client(timeout=30.0) as client:
        headers = {**_headers(api_key), "Content-Type": "application/json"}
        resp = client.post(f"{base}/api/emails/generate", json=payload, headers=headers)
        if resp.status_code == 400 and "域名" in resp.text and not domain:
            inferred = _moemail_infer_domain(client, base, api_key=api_key)
            if inferred and inferred != payload.get("domain"):
                payload["domain"] = inferred
                resp = client.post(
                    f"{base}/api/emails/generate",
                    json=payload,
                    headers=headers,
                )
        if resp.status_code >= 400:
            raise RuntimeError(
                f"MoeMail create failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json()

    email_id = data.get("id") or data.get("emailId")
    address = data.get("email") or data.get("address")
    if not email_id or not address:
        raise RuntimeError(f"Unexpected MoeMail create response: {data}")
    return {"id": str(email_id), "email": str(address), "raw": data}


def moemail_fetch_messages(
    email_id: str,
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    include_details: bool = True,
) -> list[dict[str, Any]]:
    if not email_id:
        return []
    if not (api_key or MOEMAIL_API_KEY):
        return []

    base = (base_url or MOEMAIL_BASE_URL).rstrip("/")
    with httpx.Client(timeout=30.0) as client:
        resp = client.get(f"{base}/api/emails/{email_id}", headers=_headers(api_key))
        if resp.status_code >= 400:
            raise RuntimeError(
                f"MoeMail list failed {resp.status_code}: {resp.text[:500]}"
            )
        data = resp.json()
        messages = data.get("messages") if isinstance(data, dict) else None
        if not isinstance(messages, list):
            return []

        out: list[dict[str, Any]] = []
        for raw in messages[:20]:
            if not isinstance(raw, dict):
                continue
            item = dict(raw)
            msg_id = item.get("id") or item.get("messageId")
            if include_details and msg_id:
                detail = client.get(
                    f"{base}/api/emails/{email_id}/{msg_id}",
                    headers=_headers(api_key),
                )
                if detail.status_code == 200:
                    d = detail.json()
                    msg = d.get("message") if isinstance(d, dict) else None
                    if isinstance(msg, dict):
                        item.update(msg)
            text = "\n".join(
                str(item.get(k) or "")
                for k in ("subject", "content", "html", "from_address", "from")
            )
            item["extracted"] = _extract_codes_and_links(text)
            out.append(item)
        return out


# Private aliases matching historical names used by grok_build_adapter.
_moemail_create_mailbox = moemail_create_mailbox
_moemail_fetch_messages = moemail_fetch_messages


def test_xai_proxy(
    *,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
) -> dict[str, Any]:
    """Smoke-test whether a proxy can reach accounts.x.ai."""
    try:
        proxy_cfg = normalize_proxy_config(
            proxy,
            username=proxy_username,
            password=proxy_password,
        )
    except ValueError as e:
        return {"ok": False, "error": str(e), "proxy_enabled": False}

    url = "https://accounts.x.ai/sign-up?redirect=grok-com"
    headers = {
        "user-agent": (
            "Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
            "(KHTML, like Gecko) Chrome/144.0.0.0 Safari/537.36"
        ),
        "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
    }
    try:
        from curl_cffi import requests as curl_requests
    except Exception:
        curl_requests = None

    if curl_requests is not None:
        try:
            kwargs: dict[str, Any] = {
                "headers": headers,
                "timeout": 45,
                "allow_redirects": True,
                "impersonate": "chrome",
            }
            if proxy_cfg:
                kwargs["proxies"] = {
                    "http": proxy_cfg["proxy"],
                    "https": proxy_cfg["proxy"],
                }
            resp = curl_requests.get(url, **kwargs)
            return {
                "ok": 200 <= int(resp.status_code) < 400,
                "status_code": int(resp.status_code),
                "body_preview": (resp.text or "")[:500],
                "transport": "curl_cffi",
                "proxy_enabled": bool(proxy_cfg),
            }
        except Exception as e:  # noqa: BLE001
            return {
                "ok": False,
                "status_code": 0,
                "body_preview": str(e)[:500],
                "transport": "curl_cffi",
                "proxy_enabled": bool(proxy_cfg),
            }

    try:
        with httpx.Client(
            timeout=45.0,
            proxy=proxy_cfg["proxy"] if proxy_cfg else None,
            follow_redirects=True,
        ) as client:
            resp = client.get(url, headers=headers)
            return {
                "ok": 200 <= int(resp.status_code) < 400,
                "status_code": int(resp.status_code),
                "body_preview": (resp.text or "")[:500],
                "transport": "httpx",
                "proxy_enabled": bool(proxy_cfg),
            }
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "status_code": 0,
            "body_preview": str(e)[:500],
            "transport": "httpx",
            "proxy_enabled": bool(proxy_cfg),
        }
