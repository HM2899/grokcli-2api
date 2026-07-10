# -*- coding: utf-8 -*-
"""Browser-fingerprint transport layer for xconsole_client.

Why: the x.ai endpoints sit behind Cloudflare and are sensitive to TLS / HTTP2 /
header-order fingerprints. A bare `urllib` request will get a 403 with
`cf-mitigated: challenge` on most paths. This module wraps `curl_cffi`, which
performs TLS+HTTP2+header-order impersonation of a real browser at the libcurl
level (no headless browser required), and lets us match the captured Chrome
148 / Windows fingerprint.

The default impersonate target is `chrome131` (closest stable preset available
in `curl_cffi 0.15.0`). The visible User-Agent and most other surface headers
still come from `xconsole_client.config` so they stay exactly consistent with
the original capture.

You can swap to `urllib` (no fingerprint) by setting
`XConsoleAuthClient(transport="urllib")` for offline code-only tests, but
against the real `accounts.x.ai` you'll almost certainly be challenged.
"""
from __future__ import annotations

import gzip
import io
from typing import Dict, List, Optional, Tuple

try:
    from curl_cffi import requests as cc_requests  # type: ignore
    _HAS_CURL_CFFI = True
except Exception:  # pragma: no cover
    cc_requests = None
    _HAS_CURL_CFFI = False


# Defaults that match the captured Chrome 148 / Windows profile.
DEFAULT_IMPERSONATE = "chrome131"
DEFAULT_HTTP_VERSION = "v2"  # curl_cffi: "v2" or "v3" — accounts.x.ai serves HTTP/2
DEFAULT_ACCEPT_ENCODING = "gzip, deflate, br, zstd"
DEFAULT_JA3: Optional[str] = None  # let curl_cffi derive from impersonate target


class FingerprintTransport:
    """Wraps a `curl_cffi.Session` with a curl-cffi cookie jar and the captured
    Chrome 148 / Windows header order.

    The transport is intentionally thin: it does not understand gRPC-web or
    React Server Actions — those live in `XConsoleAuthClient`. This layer only
    guarantees that, on the wire, we look like the browser that produced the
    capture.
    """

    def __init__(
        self,
        *,
        impersonate: str = DEFAULT_IMPERSONATE,
        http_version: str = DEFAULT_HTTP_VERSION,
        accept_encoding: str = DEFAULT_ACCEPT_ENCODING,
        timeout: float = 30.0,
        debug: bool = False,
        proxy: Optional[str] = None,
    ):
        if not _HAS_CURL_CFFI:
            raise RuntimeError(
                "curl_cffi is not installed. Install with: pip install curl_cffi"
            )
        self._impersonate = impersonate
        self._http_version = http_version
        self._timeout = timeout
        self._debug = debug
        self._proxy = (proxy or "").strip() or None
        # A new Session per client. The browser-equivalent fingerprint is
        # established by `impersonate=`; it is fixed for the session's life.
        self._session = cc_requests.Session(
            impersonate=impersonate,
            http_version=http_version,
            ja3=DEFAULT_JA3,
        )
        # Make sure default Accept-Encoding is exactly the Chrome order.
        self._session.headers["accept-encoding"] = accept_encoding
        # Apply proxy if provided (was previously accepted but ignored).
        if self._proxy:
            self._session.proxies = {
                "http": self._proxy,
                "https": self._proxy,
            }

    # ----------------------------------------------------------------- transport
    def request(
        self, method: str, url: str, *, headers: Dict[str, str], body: Optional[bytes] = None
    ) -> Tuple[int, Dict[str, str], List[str], bytes]:
        # curl_cffi lowercases keys on send; we don't rely on case here.
        merged: Dict[str, str] = {}
        # Surface order matters less than (a) presence, (b) Accept-Encoding order,
        # (c) sec-ch-* consistency. We still try to put `Host` first implicitly,
        # then `User-Agent`, then `Accept` family, then the rest — matching what
        # a real browser sends. curl_cffi fills in User-Agent, sec-ch-ua, etc.
        priority_prefix = ("user-agent", "accept", "accept-language", "accept-encoding",
                            "content-type", "content-length", "origin", "referer",
                            "sec-ch-ua", "sec-ch-ua-mobile", "sec-ch-ua-platform")
        for k in priority_prefix:
            if k in headers:
                merged[k] = headers[k]
        for k, v in headers.items():
            if k not in merged:
                merged[k] = v

        req_kwargs = {
            "method": method,
            "url": url,
            "headers": merged,
            "data": body,
            "timeout": self._timeout,
            "allow_redirects": False,  # we want to see 3xx, like the real browser
        }
        if self._proxy:
            req_kwargs["proxies"] = {"http": self._proxy, "https": self._proxy}
        resp = self._session.request(**req_kwargs)
        status = resp.status_code
        raw = resp.content
        # Defensive: if server sent gzip but curl didn't decode, do it here.
        ce = resp.headers.get("content-encoding", "").lower()
        if "gzip" in ce and raw[:2] == b"\x1f\x8b":
            try:
                raw = gzip.GzipFile(fileobj=io.BytesIO(raw)).read()
            except OSError:
                pass
        # curl_cffi folds duplicate Set-Cookie into one comma-joined header.
        # Split them back apart by recognizing the cookie-attribute pattern.
        raw_sc = resp.headers.get("set-cookie", "")
        set_cookies = _split_set_cookie(raw_sc) if raw_sc else []
        hdrs = {k.lower(): v for k, v in resp.headers.items()}
        if self._debug:
            print(f"  <- {status} {method} {url}  ({len(raw)} bytes, {len(set_cookies)} set-cookie, "
                  f"impersonate={self._impersonate}, http={self._http_version})")
        return status, hdrs, set_cookies, raw

    @property
    def cookies(self):
        """Return the underlying curl_cffi Cookies object (dict-like).

        curl_cffi exposes `session.cookies` as a property/method depending on
        version; both shapes are accepted here.
        """
        c = self._session.cookies
        return c() if callable(c) else c

    def close(self):
        self._session.close()


def _split_set_cookie(joined: str) -> List[str]:
    """curl_cffi (like requests) collapses multi Set-Cookie into a single header
    joined by ', '. Split them back into individual cookie strings. Heuristic:
    a new cookie starts with `<name>=<value>` where the value comes right after
    '=', and the next segment begins with a known cookie-attribute name
    (Path, Expires, Max-Age, Domain, Secure, HttpOnly, SameSite)."""
    out: List[str] = []
    cur = joined
    while True:
        # Find the next cookie start: look for '=<value>; Attribute' pattern.
        # If there are no commas, return as is.
        if "," not in cur:
            out.append(cur.strip())
            break
        # Find commas that are NOT inside a Date value (HttpDate uses commas).
        # The safest split is to find ";" followed by space and an attribute name.
        idx = _next_cookie_boundary(cur)
        if idx < 0:
            out.append(cur.strip())
            break
        out.append(cur[:idx].strip())
        cur = cur[idx + 1:].lstrip()
    return [c for c in out if c]


_KNOWN_ATTRS = ("Path=", "Expires=", "Max-Age=", "Domain=", "Secure", "HttpOnly",
                "SameSite=", "Partitioned")


def _next_cookie_boundary(joined: str) -> int:
    """Return the index of the comma that ends the first cookie in `joined`,
    or -1 if it cannot be split (single cookie)."""
    pos = 0
    n = len(joined)
    while pos < n:
        comma = joined.find(",", pos)
        if comma < 0:
            return -1
        # Look ahead: after a comma, the next cookie starts with 'Name='.
        # Accept it as a split if the chunk after the comma is a new cookie
        # AND the previous segment ends with an attribute (or has at least
        # one ';' before the comma).
        after = joined[comma + 1:].lstrip()
        head = joined[:comma]
        if ";" in head and after:
            # Check if 'after' looks like the start of a new cookie (Name=Value)
            if "=" in after.split(";", 1)[0]:
                # And either 'after' is the start of a known attribute
                # OR head has a cookie-attribute terminator
                first_token = after.split("=", 1)[0].strip()
                if first_token and all(c.isalnum() or c in "-_." for c in first_token):
                    return comma
        pos = comma + 1
    return -1
