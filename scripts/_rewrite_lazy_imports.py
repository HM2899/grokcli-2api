#!/usr/bin/env python3
"""Bulk rewrite lazy/bare imports under grok2api/ to package paths."""
from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1] / "grok2api"

SUBS = [
    (re.compile(r"^(\s*)import account_pool\s*$", re.M), r"\1from grok2api.pool import account_pool"),
    (re.compile(r"^(\s*)import account_pool as (\w+)\s*$", re.M), r"\1from grok2api.pool import account_pool as \2"),
    (re.compile(r"^(\s*)import usage_stats\s*$", re.M), r"\1from grok2api.admin import usage_stats"),
    (re.compile(r"^(\s*)import usage_stats as (\w+)\s*$", re.M), r"\1from grok2api.admin import usage_stats as \2"),
    (re.compile(r"^(\s*)import config as (\w+)\s*$", re.M), r"\1import grok2api.config as \2"),
    (re.compile(r"^(\s*)import config\s*$", re.M), r"\1import grok2api.config as config"),
    (re.compile(r"^(\s*)import settings_store\s*$", re.M), r"\1from grok2api.admin import settings_store"),
    (re.compile(r"^(\s*)import settings_store as (\w+)\s*$", re.M), r"\1from grok2api.admin import settings_store as \2"),
    (re.compile(r"^(\s*)import proxy_pool\s*$", re.M), r"\1from grok2api.upstream import proxy_pool"),
    (re.compile(r"^(\s*)import proxy_pool as (\w+)\s*$", re.M), r"\1from grok2api.upstream import proxy_pool as \2"),
    (re.compile(r"^(\s*)import moemail\s*$", re.M), r"\1from grok2api.upstream import moemail"),
    (re.compile(r"^(\s*)import oidc_auth\s*$", re.M), r"\1from grok2api.upstream import oidc_auth"),
    (re.compile(r"^(\s*)import oidc_auth as (\w+)\s*$", re.M), r"\1from grok2api.upstream import oidc_auth as \2"),
    (re.compile(r"^(\s*)import models\s*$", re.M), r"\1from grok2api.upstream import models"),
    (re.compile(r"^(\s*)import models as (\w+)\s*$", re.M), r"\1from grok2api.upstream import models as \2"),
    (re.compile(r"^(\s*)import auth_store\s*$", re.M), r"\1from grok2api.pool import auth_store"),
    (re.compile(r"^(\s*)import auth\s*$", re.M), r"\1from grok2api.pool import auth"),
    (re.compile(r"^(\s*)import token_maintainer\s*$", re.M), r"\1from grok2api.pool import token_maintainer"),
    (re.compile(r"^(\s*)import model_health\s*$", re.M), r"\1from grok2api.pool import model_health"),
    (re.compile(r"^(\s*)import quota\s*$", re.M), r"\1from grok2api.pool import quota"),
    (re.compile(r"^(\s*)import accounts\s*$", re.M), r"\1from grok2api.pool import accounts"),
    (re.compile(r"^(\s*)import apikeys\s*$", re.M), r"\1from grok2api.admin import apikeys"),
    (re.compile(r"^(\s*)import sub2api_client\s*$", re.M), r"\1from grok2api.upstream import sub2api_client"),
    (re.compile(r"^(\s*)import grok_build_adapter as (\w+)\s*$", re.M), r"\1from grok2api.upstream import grok_build_adapter as \2"),
    (re.compile(r"^(\s*)import grok_build_adapter\s*$", re.M), r"\1from grok2api.upstream import grok_build_adapter"),
    (re.compile(r"^(\s*)from settings_store import ", re.M), r"\1from grok2api.admin.settings_store import "),
    (re.compile(r"^(\s*)from config import ", re.M), r"\1from grok2api.config import "),
    (re.compile(r"^(\s*)from auth import ", re.M), r"\1from grok2api.pool.auth import "),
    (re.compile(r"^(\s*)from auth_store import ", re.M), r"\1from grok2api.pool.auth_store import "),
    (re.compile(r"^(\s*)from models import ", re.M), r"\1from grok2api.upstream.models import "),
    (re.compile(r"^(\s*)from account_pool import ", re.M), r"\1from grok2api.pool.account_pool import "),
    (re.compile(r"^(\s*)from oidc_auth import ", re.M), r"\1from grok2api.upstream.oidc_auth import "),
    (re.compile(r"^(\s*)from maintenance_gate import ", re.M), r"\1from grok2api.admin.maintenance_gate import "),
    (re.compile(r"^(\s*)from task_log import ", re.M), r"\1from grok2api.admin.task_log import "),
    (re.compile(r"^(\s*)from usage_stats import ", re.M), r"\1from grok2api.admin.usage_stats import "),
    (re.compile(r"^(\s*)from proxy_pool import ", re.M), r"\1from grok2api.upstream.proxy_pool import "),
    (re.compile(r"^(\s*)from moemail import ", re.M), r"\1from grok2api.upstream.moemail import "),
    (re.compile(r"^(\s*)from anthropic_compat import ", re.M), r"\1from grok2api.protocol.anthropic_compat import "),
    (re.compile(r"^(\s*)from openai_responses import ", re.M), r"\1from grok2api.protocol.openai_responses import "),
    (re.compile(r"^(\s*)from history_compact import ", re.M), r"\1from grok2api.protocol.history_compact import "),
    (re.compile(r"^(\s*)from sub2api_client import ", re.M), r"\1from grok2api.upstream.sub2api_client import "),
    (re.compile(r"^(\s*)from grok_build_adapter import ", re.M), r"\1from grok2api.upstream.grok_build_adapter import "),
]


def main() -> None:
    total = 0
    for path in sorted(ROOT.rglob("*.py")):
        text = path.read_text(encoding="utf-8")
        n = 0
        for rx, rep in SUBS:
            text2, c = rx.subn(rep, text)
            if c:
                text = text2
                n += c
        if n:
            path.write_text(text, encoding="utf-8")
            print(f"{path.relative_to(ROOT.parent)}: {n}")
            total += n
    print("TOTAL", total)


if __name__ == "__main__":
    main()
