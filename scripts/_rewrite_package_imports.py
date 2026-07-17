#!/usr/bin/env python3
"""One-shot: rewrite bare root-module imports after light package restructure.

Package-internal modules (grok2api/**) and store/** must not depend on root
shims for peer imports (avoids import cycles). They import grok2api.* instead.

app.py may keep bare names (shims) — we still rewrite it to grok2api.* for
clarity and to reduce shim load during boot, but either works.

Run from repo root: python3 scripts/_rewrite_package_imports.py
"""
from __future__ import annotations

import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

# bare name -> fully qualified module path inside grok2api
MAP: dict[str, str] = {
    "config": "grok2api.config",
    "anthropic_compat": "grok2api.protocol.anthropic_compat",
    "openai_responses": "grok2api.protocol.openai_responses",
    "history_compact": "grok2api.protocol.history_compact",
    "account_pool": "grok2api.pool.account_pool",
    "accounts": "grok2api.pool.accounts",
    "auth": "grok2api.pool.auth",
    "auth_store": "grok2api.pool.auth_store",
    "token_maintainer": "grok2api.pool.token_maintainer",
    "conversation_affinity": "grok2api.pool.conversation_affinity",
    "quota": "grok2api.pool.quota",
    "model_health": "grok2api.pool.model_health",
    "admin_routes": "grok2api.admin.admin_routes",
    "apikeys": "grok2api.admin.apikeys",
    "settings_store": "grok2api.admin.settings_store",
    "usage_stats": "grok2api.admin.usage_stats",
    "task_log": "grok2api.admin.task_log",
    "maintenance_gate": "grok2api.admin.maintenance_gate",
    "grok_build_adapter": "grok2api.upstream.grok_build_adapter",
    "oidc_auth": "grok2api.upstream.oidc_auth",
    "moemail": "grok2api.upstream.moemail",
    "sub2api_client": "grok2api.upstream.sub2api_client",
    "proxy_pool": "grok2api.upstream.proxy_pool",
    "models": "grok2api.upstream.models",
    # ops scripts stay importable via root shims (not package)
    # sso_to_auth_json / migrate_json_to_pg intentionally omitted
}

# longest first so account_pool wins over accounts if ever ambiguous
NAMES = sorted(MAP.keys(), key=len, reverse=True)
NAME_ALT = "|".join(re.escape(n) for n in NAMES)

# from account_pool import X
FROM_RE = re.compile(
    rf"^([ \t]*)from[ \t]+({NAME_ALT})[ \t]+import[ \t]+",
    re.M,
)
# import account_pool
# import account_pool as ap
# import account_pool, auth  (rare — handle single-name form primarily)
IMPORT_RE = re.compile(
    rf"^([ \t]*)import[ \t]+({NAME_ALT})\b([ \t]*as[ \t]+\w+)?[ \t]*$",
    re.M,
)
# import a, b  multi — process carefully
IMPORT_MULTI_RE = re.compile(
    rf"^([ \t]*)import[ \t]+(.+)$",
    re.M,
)


def rewrite_text(text: str) -> tuple[str, int]:
    n = 0

    def from_sub(m: re.Match[str]) -> str:
        nonlocal n
        bare = m.group(2)
        n += 1
        return f"{m.group(1)}from {MAP[bare]} import "

    text = FROM_RE.sub(from_sub, text)

    def import_sub(m: re.Match[str]) -> str:
        nonlocal n
        bare = m.group(2)
        alias = m.group(3) or ""
        n += 1
        # `import grok2api.pool.account_pool as account_pool` keeps local name
        if alias:
            return f"{m.group(1)}import {MAP[bare]}{alias}"
        # default: keep short name available
        short = bare
        return f"{m.group(1)}import {MAP[bare]} as {short}"

    text = IMPORT_RE.sub(import_sub, text)

    # multi-import lines containing our names: import a, b as c
    def multi_sub(m: re.Match[str]) -> str:
        nonlocal n
        indent, rest = m.group(1), m.group(2).strip()
        if rest.startswith("from "):
            return m.group(0)
        # skip if already fully rewritten single form handled
        parts = [p.strip() for p in rest.split(",")]
        if len(parts) < 2:
            return m.group(0)
        out_parts: list[str] = []
        changed = False
        for p in parts:
            # name or name as alias
            mm = re.match(r"^(\w+)(\s+as\s+\w+)?$", p)
            if not mm:
                out_parts.append(p)
                continue
            name, als = mm.group(1), mm.group(2) or ""
            if name in MAP:
                changed = True
                n += 1
                if als:
                    out_parts.append(f"{MAP[name]}{als}")
                else:
                    out_parts.append(f"{MAP[name]} as {name}")
            else:
                out_parts.append(p)
        if not changed:
            return m.group(0)
        return f"{indent}import {', '.join(out_parts)}"

    text = IMPORT_MULTI_RE.sub(multi_sub, text)
    return text, n


def main() -> None:
    targets: list[Path] = []
    targets.extend(ROOT.joinpath("grok2api").rglob("*.py"))
    targets.extend(ROOT.joinpath("store").rglob("*.py"))
    targets.append(ROOT / "app.py")
    # scripts that previously imported root modules by bare name can keep
    # shims; optionally rewrite tests later. Ops scripts under scripts/ that
    # import config/account_pool etc. should keep bare names (shims).

    total = 0
    for path in sorted(targets):
        if path.name == "__init__.py" and path.parent.name in {
            "grok2api",
            "protocol",
            "pool",
            "admin",
            "upstream",
        }:
            # still rewrite if any imports appear later
            pass
        orig = path.read_text(encoding="utf-8")
        new, n = rewrite_text(orig)
        if n:
            path.write_text(new, encoding="utf-8")
            print(f"{path.relative_to(ROOT)}: {n} import(s)")
            total += n
    print(f"TOTAL rewrites: {total}")


if __name__ == "__main__":
    main()
