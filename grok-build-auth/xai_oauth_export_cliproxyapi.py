#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Export an existing xAI OAuth record to CLIProxyAPI auth format.

Example:

    python xai_oauth_export_cliproxyapi.py --cliproxyapi-auth-dir ./cliproxyapi_auth

If --record is omitted, the newest oauth_output/xai_oauth_*.json is used.
"""
from __future__ import annotations

import argparse
import json
from pathlib import Path

from xconsole_client.xai_oauth import (
    CLIPROXYAPI_GROK_BASE_URL,
    save_cliproxyapi_auth_record,
)


def newest_oauth_record() -> Path:
    records = sorted(Path("oauth_output").glob("xai_oauth_*.json"))
    if not records:
        raise FileNotFoundError("no oauth_output/xai_oauth_*.json records found")
    return records[-1]


def main() -> None:
    p = argparse.ArgumentParser(description="Export xAI OAuth JSON to CLIProxyAPI auth JSON")
    p.add_argument("--record", default=None, help="Path to oauth_output/xai_oauth_*.json; defaults to newest")
    p.add_argument(
        "--cliproxyapi-auth-dir",
        required=True,
        help="CLIProxyAPI auth dir, e.g. ./cliproxyapi_auth",
    )
    p.add_argument("--cliproxyapi-base-url", default=CLIPROXYAPI_GROK_BASE_URL)
    p.add_argument("--disabled", action="store_true", help="Write exported auth as disabled")
    args = p.parse_args()

    record_path = Path(args.record) if args.record else newest_oauth_record()
    source = json.loads(record_path.read_text(encoding="utf-8"))
    token = source.get("token") if isinstance(source.get("token"), dict) else source
    userinfo = source.get("userinfo") if isinstance(source.get("userinfo"), dict) else {}

    out = save_cliproxyapi_auth_record(
        token,
        userinfo=userinfo,
        auth_dir=args.cliproxyapi_auth_dir,
        disabled=args.disabled,
        base_url=args.cliproxyapi_base_url,
    )
    print(f"source: {record_path}")
    print(f"cliproxyapi_auth: {out}")


if __name__ == "__main__":
    main()
