# -*- coding: utf-8 -*-
"""Tempmail.lol email backend for the xconsole_client protocol.

Uses the Tempmail.lol REST API (api.tempmail.lol) as the default disposable
mailbox backend for signup verification codes.

API endpoint reference:
    POST /v2/inbox/create           -> { address, token }     (201)
    GET  /v2/inbox?token=<token>    -> { emails[], expired }  (200)

An Email object: { from, to, subject, body, html, date (unix ms) }.

Usage:
    from xconsole_client.tempmail_transport import TempmailInbox
    inbox = TempmailInbox(api_key="...", prefix="xai")
    address = inbox.create()
    # ... send email to address ...
    code = inbox.wait_for_code(timeout=90)
"""
from __future__ import annotations

import re
import time
from dataclasses import dataclass, field
from typing import List, Optional

import requests


BASE_URL = "https://api.tempmail.lol"


@dataclass
class TempmailInbox:
    """A Tempmail.lol inbox with polling for x.ai verification codes."""

    api_key: str
    prefix: str = ""
    base_url: str = BASE_URL
    timeout: float = 90.0
    interval: float = 3.0
    debug: bool = False

    # populated after create()
    address: str = ""
    token: str = ""
    _created: bool = field(default=False, init=False)

    def _auth_headers(self) -> dict:
        return {"Authorization": f"Bearer {self.api_key}"}

    def create(self) -> str:
        """Create a new inbox. Returns the email address."""
        if self._created:
            raise RuntimeError("Inbox already created")

        resp = requests.post(
            f"{self.base_url}/v2/inbox/create",
            headers=self._auth_headers(),
            json={"prefix": self.prefix},
            timeout=15,
        )
        if resp.status_code != 201:
            raise RuntimeError(
                f"Tempmail.lol create inbox failed: {resp.status_code} {resp.text[:300]}"
            )

        data = resp.json()
        self.address = data["address"]
        self.token = data["token"]
        self._created = True

        if self.debug:
            print(f"  [Tempmail] inbox created: {self.address}")

        return self.address

    def get_emails(self) -> list[dict]:
        """Fetch all emails currently in the inbox."""
        if not self._created:
            raise RuntimeError("Call create() first")

        resp = requests.get(
            f"{self.base_url}/v2/inbox",
            headers=self._auth_headers(),
            params={"token": self.token},
            timeout=15,
        )
        if resp.status_code != 200:
            return []

        data = resp.json()
        return data.get("emails", [])

    def wait_for_code(self, timeout: Optional[float] = None) -> str:
        """Poll until a 6-char x.ai code appears. Returns the code string.

        Raises TimeoutError if nothing arrives within the timeout.
        """
        deadline = time.time() + (timeout or self.timeout)
        seen_ids: set[str] = set()

        while True:
            emails = self.get_emails()
            for email in emails:
                # Use from+subject+date as a dedup key
                eid = f"{email.get('from','')}:{email.get('subject','')}:{email.get('date','')}"
                if eid in seen_ids:
                    continue
                seen_ids.add(eid)

                text = " ".join([
                    email.get("subject", "") or "",
                    email.get("body", "") or "",
                    email.get("from", "") or "",
                ])
                code = _extract_code(text)
                if code:
                    if self.debug:
                        print(f"  [Tempmail] code found: {code} (from: {email.get('from')})")
                    return code

            if time.time() >= deadline:
                raise TimeoutError(
                    f"Tempmail.lol: no x.ai code for {self.address} within "
                    f"{timeout or self.timeout:.0f}s ({len(seen_ids)} emails seen)"
                )

            if self.debug:
                print(f"  [Tempmail] polling... ({len(seen_ids)} emails so far)")
            time.sleep(self.interval)


# --------------------------------------------------------------------------- #
# Code extractor — same logic as mailbox.py, kept standalone for this module.
# --------------------------------------------------------------------------- #
_CODE_PATTERNS = (
    # x.ai current format: "LSQ-OPU" (3 alphanum + dash + 3 alphanum = 7 chars)
    re.compile(r"(?<![A-Z0-9])([A-Z0-9]{3}-[A-Z0-9]{3})(?![A-Z0-9])"),
    # x.ai legacy format: 6 uppercase alphanumeric, no dash (e.g. "XAI0X1")
    re.compile(r"(?<![A-Z0-9])([A-Z0-9]{6})(?![A-Z0-9])"),
    # keyword-anchored fallbacks
    re.compile(
        r"(?i)(?:code|otp|验证码|verification|verify)\s*[:：]?\s*([A-Z0-9]{3}-[A-Z0-9]{3})"
    ),
    re.compile(
        r"(?i)(?:code|otp|验证码|verification|verify)\s*[:：]?\s*([A-Z0-9]{6})"
    ),
)


def _extract_code(text: str) -> Optional[str]:
    if not text:
        return None
    for pat in _CODE_PATTERNS:
        m = pat.search(text)
        if m:
            raw = m.group(1) if m.groups() else m.group(0)
            # x.ai codes are uppercase alphanumeric (+ dash), not pure digits
            if raw.replace("-", "").isdigit():
                continue
            return raw.upper()
    return None