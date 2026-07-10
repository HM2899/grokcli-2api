# -*- coding: utf-8 -*-
"""SSO session-cookie extraction for x.ai account sign-up.

After ``create_account`` succeeds, the RSC response body contains a JWT chain URL
that kicks off cross-domain SSO cookie propagation.  This module follows that chain
(HTTP only, no browser) and extracts the ``sso`` cookie — a JWT carrying a
``session_id``.

Usage::

    from xconsole_client.sso import SSOExtractor, parse_sso_jwt_payload

    res = client.create_account(...)
    extractor = SSOExtractor(client._t, client._base_headers, debug=True)
    sso_token = extractor.extract(res.rsc_body)
    if sso_token:
        print(parse_sso_jwt_payload(sso_token))   # {"session_id": "..."}

Protocol (reverse-engineered from the live flow, 2026-06-29):
    1. Parse the first JWT URL from the RSC body (``...set-cookie?q=<JWT>``).
    2. Decode the JWT payload — it carries a ``success_url`` pointing to
       ``auth.grokusercontent.com/set-cookie?q=<next-JWT>``.
    3. GET that URL.  The endpoint replies **303** but first sets the ``sso``
       cookie on ``.grok.com`` via ``Set-Cookie``.
    4. Read ``sso`` from the transport cookie jar.

The old 3-hop fan-out (grokipedia → grokusercontent → grok.com) is partially
decommissioned; only the ``grokusercontent`` hop is still functional.
"""
from __future__ import annotations

import base64
import json
import os
import re
from datetime import datetime, timezone
from pathlib import Path
from typing import Any, Callable, Dict, List, Optional, Tuple

from . import config as C


# --------------------------------------------------------------------------- #
# public helpers
# --------------------------------------------------------------------------- #

def _normalize_rsc_text(rsc_body: str) -> str:
    """Unescape common RSC/JSON encodings that hide set-cookie URLs."""
    if not rsc_body:
        return ""
    text = rsc_body
    # Multiple unescape passes for nested RSC string encoding.
    for _ in range(3):
        nxt = (
            text.replace("\\u0026", "&")
            .replace("\\u003d", "=")
            .replace("\\u003f", "?")
            .replace("\\u002F", "/")
            .replace("\\u002f", "/")
            .replace("\\/", "/")
            .replace("\\\\/", "/")
            .replace("&amp;", "&")
            .replace("\\u0026amp;", "&")
        )
        if nxt == text:
            break
        text = nxt
    return text


def parse_sso_jwt_url(rsc_body: str) -> Optional[str]:
    """Return the first SSO ``set-cookie?q=<JWT>`` URL found in *rsc_body*.

    xAI occasionally changes host / path / encoding. Match several shapes:
      - https://.../set-cookie?q=eyJ...
      - https://.../set-cookie/?q=eyJ...
      - escaped JSON/RSC forms with \\u0026 / \\u003d / &amp;
      - bare JWT then reconstructed set-cookie URL
    """
    if not rsc_body:
        return None
    text = _normalize_rsc_text(rsc_body)
    patterns = (
        # canonical
        r'https?://[^\s"\'<>\\]+set-cookie/?\?q='
        r'(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
        # host-agnostic q= JWT near set-cookie
        r'(?:https?:)?//[^\s"\'<>\\]*set-cookie[^\s"\'<>\\]*[?&]q='
        r'(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
        # bare path
        r'/[^\s"\'<>\\]*set-cookie/?\?q='
        r'(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
        # set-cookie host with encoded separators
        r'https?://auth\.(?:x\.ai|grokusercontent\.com)/set-cookie\?q='
        r'(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
    )
    for pat in patterns:
        m = re.search(pat, text, flags=re.IGNORECASE)
        if not m:
            continue
        url = m.group(0)
        if url.startswith("//"):
            url = "https:" + url
        elif url.startswith("/"):
            # prefer accounts host for relative paths
            url = "https://accounts.x.ai" + url
        return url

    # Last resort: find a JWT near "set-cookie" and reconstruct a hop URL.
    m = re.search(
        r'set-cookie[^e]{0,80}(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
        text,
        flags=re.IGNORECASE,
    )
    if m:
        jwt = m.group(1)
        return f"https://auth.grokusercontent.com/set-cookie?q={jwt}"
    return None


def parse_sso_token_from_text(text: str) -> Optional[str]:
    """Extract a raw ``sso=<JWT>`` value embedded in HTML/RSC/body text."""
    if not text:
        return None
    text = _normalize_rsc_text(text)
    m = re.search(
        r'(?:^|[;,\s\'"\\])sso=(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
        text,
        flags=re.IGNORECASE | re.MULTILINE,
    )
    return m.group(1) if m else None


def parse_all_set_cookie_urls(rsc_body: str) -> List[str]:
    """Return all candidate set-cookie hop URLs from an RSC body."""
    if not rsc_body:
        return []
    text = _normalize_rsc_text(rsc_body)
    found: List[str] = []
    for m in re.finditer(
        r'https?://[^\s"\'<>\\]+set-cookie/?\?q='
        r'eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+',
        text,
        flags=re.IGNORECASE,
    ):
        url = m.group(0)
        if url not in found:
            found.append(url)
    # relative
    for m in re.finditer(
        r'/[^\s"\'<>\\]*set-cookie/?\?q='
        r'eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+',
        text,
        flags=re.IGNORECASE,
    ):
        url = "https://accounts.x.ai" + m.group(0)
        if url not in found:
            found.append(url)
    return found


def parse_jwt_payload(jwt: str) -> Optional[Dict[str, Any]]:
    """Decode the payload segment of a JWT (no signature verification)."""
    try:
        parts = jwt.split(".")
        if len(parts) < 2:
            return None
        raw = parts[1]
        raw += "=" * (4 - len(raw) % 4)
        return json.loads(base64.urlsafe_b64decode(raw))
    except Exception:
        return None


def parse_sso_jwt_payload(sso_token: str) -> Optional[Dict[str, Any]]:
    """Decode the ``sso`` cookie JWT payload (e.g. ``{"session_id":"..."}``)."""
    return parse_jwt_payload(sso_token)


def parse_sso_from_set_cookies(set_cookies: List[str]) -> Optional[str]:
    """Extract an ``sso=<JWT>`` value from raw Set-Cookie header strings."""
    if not set_cookies:
        return None
    for sc in set_cookies:
        if not sc:
            continue
        # Match sso=<jwt> at cookie start or after a comma-joined boundary.
        m = re.search(
            r'(?:^|,\s*)sso=(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
            sc,
            flags=re.IGNORECASE,
        )
        if m:
            return m.group(1)
    return None


def _extract_jwt_from_url(url: str) -> Optional[str]:
    """Pull the ``q=<JWT>`` parameter out of a set-cookie URL."""
    m = re.search(
        r'q=(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)', url
    )
    return m.group(1) if m else None


# --------------------------------------------------------------------------- #
# SSOExtractor
# --------------------------------------------------------------------------- #

# Type for the transport request method:
#   (method, url, *, headers, body) -> (status, resp_headers, set_cookies, raw_bytes)
TransportRequest = Callable[
    [str, str, Dict[str, str], Optional[bytes]],
    Tuple[int, Dict[str, str], List[str], bytes],
]

# Type for base-headers factory: () -> Dict[str, str]
HeadersFactory = Callable[[], Dict[str, str]]

# Type for cookie-jar accessor (callable or property-like object)
CookieJarSource = Any


class SSOExtractor:
    """Follow the SSO cookie chain and return the ``sso`` token.

    Parameters
    ----------
    transport_request:
        A callable matching the signature of ``XConsoleAuthClient._request``
        (or ``FingerprintTransport.request``).  It is used to make the single
        GET to ``auth.grokusercontent.com/set-cookie``.
    base_headers:
        A zero-arg callable that returns a dict of common browser headers
        (like ``XConsoleAuthClient._base_headers``).
    cookie_jar:
        The cookie jar object attached to the transport.  Must have a ``.jar``
        attribute that is iterable, yielding objects with ``.name`` / ``.value``
        attributes (``curl_cffi.requests.cookies.Cookies`` satisfies this).
    debug:
        Print progress lines.
    """

    GROKUSERCONTENT_SET_COOKIE = "https://auth.grokusercontent.com/set-cookie"

    def __init__(
        self,
        transport_request: TransportRequest,
        base_headers: HeadersFactory,
        cookie_jar: CookieJarSource,
        *,
        debug: bool = False,
    ) -> None:
        self._request = transport_request
        self._base_headers = base_headers
        self._cookies = cookie_jar
        self.debug = debug

    # ----------------------------------------------------------------- public

    def extract(
        self,
        rsc_body: str,
        *,
        email: str = "",
        password: str = "",
        save: bool = False,
        output_dir: Optional[str | Path] = None,
    ) -> Optional[str]:
        """Extract the ``sso`` cookie value from *rsc_body*.

        *rsc_body* is the ``.rsc_body`` attribute of the ``SignupResult``
        returned by ``XConsoleAuthClient.create_account()``.

        If *save* is ``True`` (or *email* is provided), the token is
        persisted to ``<xconsole>/sso_output/sso_<timestamp>.json``
        via :func:`save_sso`.

        Returns the ``sso`` JWT string, or ``None`` if extraction fails.
        """
        # 0. RSC body may already embed sso=eyJ...
        token = parse_sso_token_from_text(rsc_body)
        if token:
            if self.debug:
                print("  [sso] found raw sso token in RSC body")
            if save or email:
                path = save_sso(
                    token, email=email, password=password, output_dir=output_dir
                )
                if self.debug:
                    print(f"  [sso] saved to: {path}")
            return token

        # 1. collect all candidate hop URLs from RSC body
        hop_urls = parse_all_set_cookie_urls(rsc_body)
        primary = parse_sso_jwt_url(rsc_body)
        if primary and primary not in hop_urls:
            hop_urls.insert(0, primary)
        if not hop_urls:
            if self.debug:
                print("  [sso] no JWT set-cookie URL in RSC body")
            # Still try default grokusercontent endpoint in case body only had a JWT
            jwt_only = re.search(
                r'(eyJ[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+\.[a-zA-Z0-9_-]+)',
                _normalize_rsc_text(rsc_body or ""),
            )
            if jwt_only:
                hop_urls = [
                    f"https://auth.grokusercontent.com/set-cookie?q={jwt_only.group(1)}"
                ]
            else:
                return None

        if self.debug:
            print(f"  [sso] candidate hop URLs: {len(hop_urls)}")
            for u in hop_urls[:5]:
                print(f"  [sso]  - {u[:100]}")

        # 2. expand each hop with JWT success_url if present
        expanded: List[str] = []
        for sso_url in hop_urls:
            if sso_url not in expanded:
                expanded.append(sso_url)
            jwt = _extract_jwt_from_url(sso_url)
            if not jwt:
                continue
            success_url = self._resolve_success_url(jwt)
            if success_url and success_url not in expanded:
                expanded.append(success_url)
        hop_urls = expanded

        # 3. hit set-cookie hops. Some deployments set the cookie on the first hop;
        # others only on grokusercontent; redirects may continue the chain.
        headers = self._base_headers()
        headers.update({
            "accept": "text/html,application/xhtml+xml,application/xml;q=0.9,*/*;q=0.8",
            "sec-fetch-site": "cross-site",
            "sec-fetch-mode": "navigate",
            "sec-fetch-dest": "document",
            "referer": C.ACCOUNTS_ORIGIN + "/",
        })

        set_cookies: List[str] = []
        last_exc: Optional[BaseException] = None
        token = None
        for hop in list(hop_urls):
            for attempt in range(2):
                try:
                    try:
                        _status, _hdrs, set_cookies, _raw = self._request(
                            "GET", hop, headers=headers, body=None,
                        )
                    except TypeError:
                        _status, _hdrs, set_cookies, _raw = self._request(
                            "GET", hop, headers=headers,
                        )
                    last_exc = None
                    if self.debug:
                        print(
                            f"  [sso] hop HTTP {_status} {hop[:64]}..., "
                            f"set-cookies={len(set_cookies or [])}"
                        )
                    # Follow redirects if present (303/302 often carries SSO)
                    loc = ""
                    if isinstance(_hdrs, dict):
                        loc = str(_hdrs.get("location") or _hdrs.get("Location") or "")
                    token = (
                        parse_sso_from_set_cookies(set_cookies or [])
                        or parse_sso_token_from_text(
                            (_raw or b"").decode("utf-8", "replace")
                        )
                        or self._read_sso_from_jar()
                    )
                    if token:
                        break
                    if loc:
                        if loc.startswith("/"):
                            loc = "https://accounts.x.ai" + loc
                        if loc.startswith("http") and loc not in hop_urls:
                            hop_urls.append(loc)
                    break
                except Exception as exc:
                    last_exc = exc
                    if self.debug:
                        print(f"  [sso] request failed (attempt {attempt + 1}): {exc}")
                    if attempt == 0:
                        import time as _time
                        _time.sleep(0.4)
            if token:
                break
        if not token and last_exc is not None and self.debug:
            print(f"  [sso] request failed: {last_exc}")

        # 4. prefer Set-Cookie header, then cookie jar
        if not token:
            token = parse_sso_from_set_cookies(set_cookies or []) or self._read_sso_from_jar()

        # 5. persist if requested
        if token and (save or email):
            path = save_sso(token, email=email, password=password,
                            output_dir=output_dir)
            if self.debug:
                print(f"  [sso] saved to: {path}")

        return token

    # ----------------------------------------------------------------- internal

    def _resolve_success_url(self, jwt: str) -> str:
        """Decode *jwt* and return its ``success_url``, falling back to
        the hard-coded ``auth.grokusercontent.com/set-cookie``."""
        payload = parse_jwt_payload(jwt)
        if payload:
            cfg = payload.get("config", {})
            url = cfg.get("success_url")
            if isinstance(url, str) and url.startswith("https://"):
                return url
        return self.GROKUSERCONTENT_SET_COOKIE

    def _read_sso_from_jar(self) -> Optional[str]:
        """Scan the transport cookie jar for an ``sso`` cookie."""
        cj = self._cookies
        if hasattr(cj, "jar"):
            for cookie in cj.jar:
                name = str(getattr(cookie, "name", ""))
                if name == "sso":
                    val = str(getattr(cookie, "value", ""))
                    if self.debug:
                        print(f"  [sso] extracted: {val[:60]}...")
                    return val
        return None


# --------------------------------------------------------------------------- #
# SSO persistence — save extracted tokens to a dedicated directory
# --------------------------------------------------------------------------- #

# Default output directory, resolved relative to the xconsole repo root.
def _default_output_dir() -> Path:
    return Path(__file__).resolve().parent.parent / "sso_output"


def save_sso(
    token: str,
    *,
    email: str = "",
    password: str = "",
    output_dir: Optional[str | Path] = None,
) -> Path:
    """Save an extracted SSO token to a JSON file.

    Each file is named ``sso_<timestamp>.json`` and contains::

        {
            "email": "...",
            "password": "...",
            "sso": "eyJ...",
            "created_at": "2026-06-29T14:30:00Z"
        }

    Args:
        token: The ``sso`` cookie value (JWT string).
        email: Associated email address (optional, for bookkeeping).
        password: Associated password (optional, for bookkeeping).
        output_dir: Target directory.  Defaults to ``<xconsole>/sso_output/``.

    Returns:
        The path to the written file.
    """
    target = Path(output_dir) if output_dir else _default_output_dir()
    target.mkdir(parents=True, exist_ok=True)

    # Microsecond + short email salt so concurrent writers don't clobber.
    ts = datetime.now(timezone.utc).strftime("%Y%m%dT%H%M%S%fZ")
    email_slug = ""
    if email:
        email_slug = "_" + re.sub(r"[^a-zA-Z0-9._-]+", "_", email.split("@")[0])[:24]
    filename = f"sso_{ts}{email_slug}.json"
    record: Dict[str, Any] = {
        "email": email,
        "password": password,
        "sso": token,
        "created_at": datetime.now(timezone.utc).isoformat(),
    }
    # If the JWT is decodable, include the payload for convenience.
    payload = parse_sso_jwt_payload(token)
    if payload:
        record["payload"] = payload

    filepath = target / filename
    filepath.write_text(
        json.dumps(record, ensure_ascii=False, indent=2), encoding="utf-8"
    )
    return filepath


def list_saved_tokens(output_dir: Optional[str | Path] = None) -> List[Dict[str, Any]]:
    """Load all saved SSO records from the output directory.

    Returns a list of dicts (newest first), or an empty list if the directory
    does not exist or is empty.
    """
    target = Path(output_dir) if output_dir else _default_output_dir()
    if not target.is_dir():
        return []
    records: List[Dict[str, Any]] = []
    for f in sorted(target.glob("sso_*.json"), reverse=True):
        try:
            records.append(json.loads(f.read_text(encoding="utf-8")))
        except Exception:
            pass
    return records
