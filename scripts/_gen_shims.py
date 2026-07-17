#!/usr/bin/env python3
"""Generate root compatibility shims for grok2api domain packages."""
from __future__ import annotations

from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]

SHIMS: dict[str, str] = {
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
}

TEMPLATE = '''"""Compatibility shim — implementation lives in {target}."""
from __future__ import annotations

from importlib import import_module as _import_module
import sys as _sys

_impl = _import_module("{target}")
_sys.modules[__name__] = _impl
'''

APP_TEMPLATE = '''"""Compatibility launcher — real application lives in grok2api.app."""
from __future__ import annotations

from importlib import import_module as _import_module
import sys as _sys

_impl = _import_module("grok2api.app")

if __name__ == "__main__":
    _impl.main()
else:
    globals().update(_impl.__dict__)
    _sys.modules[__name__] = _impl
'''

STORE_INIT_TEMPLATE = '''"""Compatibility package — implementation lives in grok2api.store."""
from __future__ import annotations

from importlib import import_module as _import_module

_impl = _import_module("grok2api.store")

for _name in getattr(_impl, "__all__", ()):  # pragma: no cover - currently empty
    globals()[_name] = getattr(_impl, _name)

# Export public helpers from grok2api.store while keeping this root package's
# __path__, so imports like `store.redis_client` load wrapper modules that alias
# to the real `grok2api.store.redis_client` module instead of duplicating state.
for _name, _value in _impl.__dict__.items():
    if not _name.startswith("__") or _name in {"__doc__", "__all__"}:
        globals()[_name] = _value
'''

STORE_MODULE_TEMPLATE = '''"""Compatibility shim — implementation lives in grok2api.store.{name}."""
from __future__ import annotations

from importlib import import_module as _import_module
import sys as _sys

_impl = _import_module("grok2api.store.{name}")
globals().update(_impl.__dict__)
_sys.modules[__name__] = _impl
'''


def main() -> None:
    for name, target in SHIMS.items():
        path = ROOT / f"{name}.py"
        path.write_text(TEMPLATE.format(target=target), encoding="utf-8")
        print("wrote", path.name, "->", target)

    # App: keep root launcher thin; real FastAPI implementation is grok2api.app.
    (ROOT / "app.py").write_text(APP_TEMPLATE, encoding="utf-8")
    print("wrote app.py launcher -> grok2api.app")

    # Store: keep root package import-compatible without duplicating store state.
    store_root = ROOT / "store"
    store_root.mkdir(exist_ok=True)
    (store_root / "__init__.py").write_text(STORE_INIT_TEMPLATE, encoding="utf-8")
    for src in sorted((ROOT / "grok2api" / "store").glob("*.py")):
        if src.name == "__init__.py":
            continue
        name = src.stem
        (store_root / src.name).write_text(
            STORE_MODULE_TEMPLATE.format(name=name), encoding="utf-8"
        )
        print("wrote", f"store/{src.name}", "->", f"grok2api.store.{name}")

    # Ops: migrate CLI wrapper
    (ROOT / "migrate_json_to_pg.py").write_text(
        '''"""Compatibility wrapper — real CLI lives in scripts/migrate_json_to_pg.py."""
from __future__ import annotations

import runpy
from pathlib import Path

runpy.run_path(
    str(Path(__file__).resolve().parent / "scripts" / "migrate_json_to_pg.py"),
    run_name="__main__",
)
''',
        encoding="utf-8",
    )
    print("wrote migrate_json_to_pg.py wrapper")

    # Ops: sso library shim (import sso_to_auth_json)
    (ROOT / "sso_to_auth_json.py").write_text(
        '''"""Compatibility shim — implementation lives in scripts/sso_to_auth_json.py."""
from __future__ import annotations

import importlib.util
import sys
from pathlib import Path

_path = Path(__file__).resolve().parent / "scripts" / "sso_to_auth_json.py"
_spec = importlib.util.spec_from_file_location("sso_to_auth_json", _path)
if _spec is None or _spec.loader is None:
    raise ImportError(f"cannot load sso_to_auth_json from {_path}")
_mod = importlib.util.module_from_spec(_spec)
sys.modules[__name__] = _mod
sys.modules["sso_to_auth_json"] = _mod
_spec.loader.exec_module(_mod)
''',
        encoding="utf-8",
    )
    print("wrote sso_to_auth_json.py shim")


if __name__ == "__main__":
    main()
