# -*- coding: utf-8 -*-
"""YesCaptcha solver integration for x.ai Console protocol.

Provides automated solving of:
  - Cloudflare Turnstile tokens
  - Castle device fingerprint tokens (via browser automation if supported)
  - Cloudflare cf_clearance cookies (via challenge page if supported)

Usage:
    from xconsole_client.solver import YesCaptchaSolver
    solver = YesCaptchaSolver(api_key="your_key")
    turnstile_token = solver.solve_turnstile(
        website_url="https://accounts.x.ai/sign-up",
        website_key="0x4XXXXXXXXXXXXXXXXX"  # extract from browser DevTools
    )

API endpoints:
  - International: https://api.yescaptcha.com
  - China domestic: https://cn.yescaptcha.com

Task types:
  - TurnstileTaskProxyless (25 points): standard Turnstile solve
  - TurnstileTaskProxylessM1 (30 points): premium tier, higher success rate
  - CloudFlareTaskS2 (25 points): 5-second challenge (experimental)
"""
from __future__ import annotations

import os
import time
from typing import Callable, Optional

import requests


DEFAULT_ENDPOINTS = (
    "https://api.yescaptcha.com",
    "https://cn.yescaptcha.com",
)


def resolve_yescaptcha_endpoint(explicit: str | None = None) -> str:
    """Pick YesCaptcha API host.

    Prefer env override, then explicit arg, then international host.
    """
    env = (
        os.environ.get("GROK2API_YESCAPTCHA_ENDPOINT")
        or os.environ.get("YESCAPTCHA_ENDPOINT")
        or os.environ.get("YESCAPTCHA_API_BASE")
        or ""
    ).strip()
    if env:
        return env.rstrip("/")
    if explicit:
        return explicit.rstrip("/")
    # Default: international. Domestic users should set
    # GROK2API_YESCAPTCHA_ENDPOINT=https://cn.yescaptcha.com
    return DEFAULT_ENDPOINTS[0]


class YesCaptchaSolver:
    """YesCaptcha API client for solving CAPTCHA challenges."""

    def __init__(
        self,
        api_key: str,
        *,
        endpoint: str | None = None,
        timeout: float = 180.0,
        poll_interval: float = 3.0,
        debug: bool = False,
        on_progress: Optional[Callable[[str], None]] = None,
        auto_fallback_endpoint: bool = True,
    ):
        """Initialize the solver.

        Args:
            api_key: YesCaptcha clientKey (API key)
            endpoint: API endpoint (use cn.yescaptcha.com for China)
            timeout: Maximum seconds to wait for task completion
            poll_interval: Seconds between polling attempts
            debug: Print debug output
            on_progress: optional callback for status strings (UI updates)
            auto_fallback_endpoint: try cn/international peer on network errors
        """
        self._api_key = (api_key or "").strip()
        self._endpoint = resolve_yescaptcha_endpoint(endpoint)
        self._timeout = float(timeout)
        self._poll_interval = float(poll_interval)
        self._debug = debug
        self._on_progress = on_progress
        self._auto_fallback_endpoint = auto_fallback_endpoint

    def _progress(self, msg: str) -> None:
        if self._debug:
            print(f"  [YesCaptcha] {msg}")
        if self._on_progress:
            try:
                self._on_progress(msg)
            except Exception:
                pass

    def _post_json(self, path: str, payload: dict, *, timeout: float = 30.0) -> dict:
        url = f"{self._endpoint}{path}"
        try:
            resp = requests.post(url, json=payload, timeout=timeout)
            resp.raise_for_status()
            data = resp.json()
            if not isinstance(data, dict):
                raise RuntimeError(f"YesCaptcha non-object response: {data!r}")
            return data
        except Exception as first_err:
            if not self._auto_fallback_endpoint:
                raise
            # Network / DNS / TLS issues: try the other region once
            peer = None
            if "cn.yescaptcha.com" in self._endpoint:
                peer = "https://api.yescaptcha.com"
            elif "api.yescaptcha.com" in self._endpoint:
                peer = "https://cn.yescaptcha.com"
            if not peer or peer.rstrip("/") == self._endpoint.rstrip("/"):
                raise
            self._progress(f"endpoint {self._endpoint} failed ({first_err}); fallback {peer}")
            self._endpoint = peer
            resp = requests.post(f"{self._endpoint}{path}", json=payload, timeout=timeout)
            resp.raise_for_status()
            data = resp.json()
            if not isinstance(data, dict):
                raise RuntimeError(f"YesCaptcha non-object response: {data!r}")
            return data

    def _create_task(self, task: dict) -> str:
        """Create a task and return the taskId. Raises on error."""
        payload = {
            "clientKey": self._api_key,
            "task": task,
        }
        self._progress(f"createTask type={task.get('type')} endpoint={self._endpoint}")
        data = self._post_json("/createTask", payload, timeout=45)

        if data.get("errorId", 0) != 0:
            raise RuntimeError(
                f"YesCaptcha createTask failed: "
                f"{data.get('errorCode')}: {data.get('errorDescription')}"
            )

        task_id = data.get("taskId")
        if not task_id:
            raise RuntimeError(f"YesCaptcha createTask returned no taskId: {data}")

        self._progress(f"task created: {task_id}")
        return str(task_id)

    def _get_result(self, task_id: str) -> dict:
        """Poll for task result. Returns the full response dict when ready."""
        payload = {
            "clientKey": self._api_key,
            "taskId": task_id,
        }
        self._progress(f"polling getTaskResult for {task_id[:16]}...")

        started = time.time()
        deadline = started + self._timeout
        last_status = ""
        while time.time() < deadline:
            data = self._post_json("/getTaskResult", payload, timeout=45)

            if data.get("errorId", 0) != 0:
                raise RuntimeError(
                    f"YesCaptcha getTaskResult error: "
                    f"{data.get('errorCode')}: {data.get('errorDescription')}"
                )

            status = str(data.get("status") or "")
            if status == "ready":
                self._progress(f"solved in ~{int(time.time() - started)}s")
                return data
            if status in ("processing", "idle", ""):
                # YesCaptcha may omit status or use idle while queuing
                elapsed = int(time.time() - started)
                if status != last_status or elapsed % 9 < self._poll_interval:
                    self._progress(
                        f"still processing ({elapsed}s/{int(self._timeout)}s)..."
                    )
                last_status = status or "processing"
                time.sleep(self._poll_interval)
                continue
            raise RuntimeError(f"YesCaptcha unexpected status: {status} body={data}")

        raise TimeoutError(
            f"YesCaptcha task {task_id} did not complete within {self._timeout}s "
            f"(endpoint={self._endpoint})"
        )

    def solve_turnstile(
        self,
        website_url: str,
        website_key: str,
        *,
        premium: bool = False,
        fallback_non_premium: bool = True,
    ) -> str:
        """Solve a Cloudflare Turnstile challenge and return the token.

        Args:
            website_url: The page URL where Turnstile is embedded
            website_key: The Turnstile sitekey (format: 0x4...)
            premium: Prefer TurnstileTaskProxylessM1 first
            fallback_non_premium: if premium fails/timeouts, retry standard type

        Returns:
            The Turnstile token string (valid for ~120s)
        """
        website_url = (website_url or "").strip()
        website_key = (website_key or "").strip()
        if not website_url or not website_key:
            raise ValueError("website_url and website_key are required for Turnstile")

        task_types: list[str] = []
        if premium:
            task_types.append("TurnstileTaskProxylessM1")
            if fallback_non_premium:
                task_types.append("TurnstileTaskProxyless")
        else:
            task_types.append("TurnstileTaskProxyless")
            if fallback_non_premium:
                task_types.append("TurnstileTaskProxylessM1")

        errors: list[str] = []
        for idx, task_type in enumerate(task_types):
            task = {
                "type": task_type,
                "websiteURL": website_url,
                "websiteKey": website_key,
            }
            try:
                self._progress(
                    f"solve_turnstile try {idx + 1}/{len(task_types)} "
                    f"type={task_type} url={website_url} key={website_key[:12]}..."
                )
                task_id = self._create_task(task)
                result = self._get_result(task_id)
                solution = result.get("solution") or {}
                token = (
                    solution.get("token")
                    or solution.get("gRecaptchaResponse")
                    or solution.get("cf_clearance")
                )
                if not token:
                    raise RuntimeError(f"YesCaptcha returned no token: {result}")
                return str(token)
            except Exception as e:  # noqa: BLE001
                msg = f"{task_type}: {e}"
                errors.append(msg)
                self._progress(f"failed: {msg}")
                # short pause before alternate task type
                if idx + 1 < len(task_types):
                    time.sleep(1.0)
                    continue
                break

        raise RuntimeError(
            "YesCaptcha Turnstile solve failed after fallbacks: "
            + " | ".join(errors[:4])
        )

    def solve_cloudflare_challenge(
        self,
        website_url: str,
        website_key: Optional[str] = None,
    ) -> dict:
        """Solve a Cloudflare 5-second challenge (experimental)."""
        task = {
            "type": "CloudFlareTaskS2",
            "websiteURL": website_url,
        }
        if website_key:
            task["websiteKey"] = website_key

        task_id = self._create_task(task)
        result = self._get_result(task_id)

        solution = result.get("solution", {})
        if not solution:
            raise RuntimeError(f"YesCaptcha returned no solution: {result}")

        return solution

    def solve_castle(self, website_url: str) -> str:
        """Solve a Castle device fingerprint challenge (not supported)."""
        raise NotImplementedError(
            "YesCaptcha does not support Castle device fingerprint tokens. "
            "Castle tokens must be generated by running the Castle JS SDK in a browser. "
            "Consider using Puppeteer/Playwright to load https://castlesdk.io and extract the token."
        )


# --------------------------------------------------------------------------- #
# Convenience factory
# --------------------------------------------------------------------------- #
def create_solver(api_key: Optional[str] = None, **kwargs) -> YesCaptchaSolver:
    """Create a YesCaptchaSolver instance.

    If api_key is not provided, reads from YESCAPTCHA_API_KEY / GROK2API_YESCAPTCHA_KEY.
    """
    key = (
        api_key
        or os.environ.get("GROK2API_YESCAPTCHA_KEY")
        or os.environ.get("YESCAPTCHA_API_KEY")
        or ""
    ).strip()
    if not key:
        raise ValueError(
            "YesCaptcha API key required. Pass api_key= or set "
            "GROK2API_YESCAPTCHA_KEY / YESCAPTCHA_API_KEY."
        )
    return YesCaptchaSolver(key, **kwargs)
