#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Probe Grok Build/CLI free usage quota from CLIProxyAPI xAI auth files.

This does NOT print tokens. It sends a tiny non-streaming request to the
Grok Build endpoint and reads quota signals from response headers or 429 body.

Examples:

    python xai_build_quota_probe.py --auth-dir ./cliproxyapi_auth
    python xai_build_quota_probe.py --auth-dir ./cliproxyapi_auth --include-disabled
"""
from __future__ import annotations

import argparse
import json
import re
from pathlib import Path
from typing import Any
from urllib.parse import urljoin

import requests


DEFAULT_BASE_URL = "https://cli-chat-proxy.grok.com/v1"
DEFAULT_HEADERS = {
    "X-XAI-Token-Auth": "xai-grok-cli",
    "x-grok-client-version": "0.2.93",
    "x-grok-client-identifier": "grok-shell",
}


def mask_email(value: str) -> str:
    value = (value or "").strip()
    if "@" not in value:
        return value or "(unknown)"
    name, domain = value.split("@", 1)
    if len(name) <= 2:
        masked = name[:1] + "*"
    else:
        masked = name[:2] + "***" + name[-1:]
    return f"{masked}@{domain}"


def load_auth_files(auth_dir: Path, include_disabled: bool) -> list[Path]:
    files = sorted(auth_dir.glob("xai-*.json"))
    if include_disabled:
        return files
    out: list[Path] = []
    for path in files:
        try:
            data = json.loads(path.read_text(encoding="utf-8"))
        except Exception:
            continue
        if data.get("disabled") is True:
            continue
        out.append(path)
    return out


def build_url(base_url: str) -> str:
    base = (base_url or DEFAULT_BASE_URL).rstrip("/") + "/"
    return urljoin(base, "responses")


def header_int(headers: requests.structures.CaseInsensitiveDict[str], name: str) -> int | None:
    raw = headers.get(name)
    if raw is None:
        return None
    try:
        return int(raw)
    except ValueError:
        return None


def parse_exhausted(body: str) -> tuple[int | None, int | None]:
    # Example:
    # tokens (actual/limit): 1053503/1000000
    m = re.search(r"tokens\s*\(actual/limit\)\s*:\s*(\d+)\s*/\s*(\d+)", body, re.I)
    if not m:
        return None, None
    return int(m.group(1)), int(m.group(2))


def summarize_response(resp: requests.Response) -> dict[str, Any]:
    out: dict[str, Any] = {"status": resp.status_code}
    limit_tokens = header_int(resp.headers, "x-ratelimit-limit-tokens")
    remaining_tokens = header_int(resp.headers, "x-ratelimit-remaining-tokens")
    limit_requests = header_int(resp.headers, "x-ratelimit-limit-requests")
    remaining_requests = header_int(resp.headers, "x-ratelimit-remaining-requests")

    if limit_tokens is not None:
        out["limit_tokens"] = limit_tokens
    if remaining_tokens is not None:
        out["remaining_tokens"] = remaining_tokens
    if limit_requests is not None:
        out["limit_requests"] = limit_requests
    if remaining_requests is not None:
        out["remaining_requests"] = remaining_requests

    body_text = resp.text or ""
    if resp.status_code == 429:
        actual, limit = parse_exhausted(body_text)
        if actual is not None and limit is not None:
            out.update(
                {
                    "code": "subscription:free-usage-exhausted",
                    "actual_tokens": actual,
                    "limit_tokens": limit,
                    "remaining_tokens": max(0, limit - actual),
                    "reset": "rolling 24h window",
                }
            )
        else:
            out["error"] = body_text[:300]
        return out

    try:
        data = resp.json()
    except Exception:
        if body_text:
            out["body"] = body_text[:300]
        return out

    if isinstance(data, dict):
        if model := data.get("model"):
            out["model"] = model
        usage = data.get("usage")
        if isinstance(usage, dict):
            if total := usage.get("total_tokens"):
                out["probe_total_tokens"] = total
            elif total := usage.get("totalTokens"):
                out["probe_total_tokens"] = total
    return out


def probe(path: Path, timeout: float, use_auth_base_url: bool = False) -> dict[str, Any]:
    auth = json.loads(path.read_text(encoding="utf-8"))
    token = str(auth.get("access_token") or "").strip()
    if not token:
        return {"file": path.name, "error": "missing access_token"}

    headers = {
        "Authorization": f"Bearer {token}",
        "Accept": "application/json",
        "Content-Type": "application/json",
        "User-Agent": "grok-cli/0.2.93",
        **DEFAULT_HEADERS,
    }
    extra = auth.get("headers")
    if isinstance(extra, dict):
        for k, v in extra.items():
            if isinstance(k, str) and isinstance(v, str) and k.strip():
                headers[k] = v

    body = {
        "model": "grok-4.5",
        "input": "Reply exactly: OK",
        "max_output_tokens": 8,
    }
    # Build/CLI free quota lives on cli-chat-proxy.grok.com, not api.x.ai paid API.
    base_url = str(auth.get("base_url") or DEFAULT_BASE_URL) if use_auth_base_url else DEFAULT_BASE_URL
    if "api.x.ai" in base_url:
        base_url = DEFAULT_BASE_URL
    url = build_url(base_url)
    resp = requests.post(url, headers=headers, json=body, timeout=timeout)
    summary = summarize_response(resp)
    summary.update(
        {
            "file": path.name,
            "email": auth.get("email") or "",
            "email_masked": mask_email(str(auth.get("email") or "")),
            "disabled": bool(auth.get("disabled")),
            "url": url,
        }
    )
    return summary


def main() -> None:
    parser = argparse.ArgumentParser(description="Probe xAI/Grok Build free quota from auth JSON files")
    parser.add_argument("--auth-dir", required=True, help="CLIProxyAPI auth directory")
    parser.add_argument("--include-disabled", action="store_true", help="Also probe disabled auth files")
    parser.add_argument("--timeout", type=float, default=60.0)
    parser.add_argument("--json", action="store_true", help="Print JSON instead of a table")
    parser.add_argument(
        "--use-auth-base-url",
        action="store_true",
        help="Use each auth file's base_url instead of forcing the Grok Build endpoint",
    )
    args = parser.parse_args()

    auth_dir = Path(args.auth_dir)
    results = []
    for path in load_auth_files(auth_dir, include_disabled=args.include_disabled):
        try:
            results.append(probe(path, timeout=args.timeout, use_auth_base_url=args.use_auth_base_url))
        except Exception as exc:  # keep probing other accounts
            results.append({"file": path.name, "error": f"{type(exc).__name__}: {exc}"})

    if args.json:
        print(json.dumps(results, ensure_ascii=False, indent=2))
        return

    if not results:
        print("No xai-*.json auth files found.")
        return

    for item in results:
        label = item.get("email_masked") or item.get("file")
        status = item.get("status", "-")
        disabled = " disabled" if item.get("disabled") else ""
        print(f"\n{label} [{item.get('file')}] status={status}{disabled}")
        if item.get("error"):
            print(f"  error: {item['error']}")
            continue
        if "actual_tokens" in item:
            print(
                f"  exhausted: actual/limit={item['actual_tokens']}/{item['limit_tokens']} "
                f"remaining={item['remaining_tokens']} reset={item.get('reset')}"
            )
            continue
        print(
            "  tokens: "
            f"remaining={item.get('remaining_tokens', '--')} / limit={item.get('limit_tokens', '--')}"
        )
        print(
            "  requests: "
            f"remaining={item.get('remaining_requests', '--')} / limit={item.get('limit_requests', '--')}"
        )
        if item.get("probe_total_tokens") is not None:
            print(f"  probe_used_tokens: {item['probe_total_tokens']}")


if __name__ == "__main__":
    main()
