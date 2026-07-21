#!/usr/bin/env python3
"""Build content-hashed admin static assets into static/dist and rewrite HTML."""
from __future__ import annotations

import hashlib
import json
import re
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]
STATIC = ROOT / "static"
ADMIN = STATIC / "admin"
DIST = STATIC / "dist"
DIST.mkdir(exist_ok=True)

ASSETS = {
    "utils.js": STATIC / "js" / "utils.js",
    "api.js": STATIC / "js" / "api.js",
    "state.js": STATIC / "js" / "state.js",
    "auth.js": STATIC / "js" / "auth.js",
    "core.js": STATIC / "js" / "core.js",
    "admin-antd.css": STATIC / "css" / "admin-antd.css",
}


def main() -> None:
    manifest: dict[str, str] = {}
    for name, src in ASSETS.items():
        data = src.read_bytes()
        h = hashlib.sha1(data).hexdigest()[:10]
        out = DIST / (
            f"{name[:-3]}.{h}.js" if name.endswith(".js") else f"{name[:-4]}.{h}.css"
        )
        out.write_bytes(data)
        manifest[name] = f"/static/dist/{out.name}"
        print("built", name, "->", manifest[name])
    (DIST / "manifest.json").write_text(json.dumps(manifest, indent=2) + "\n")

    for path in sorted(ADMIN.glob("*.html")):
        # Force UTF-8: Windows default locale (e.g. gbk) breaks Chinese admin HTML.
        html = path.read_text(encoding="utf-8")
        html = re.sub(
            r'href="/static/css/admin-antd\.css[^"]*"',
            f'href="{manifest["admin-antd.css"]}"',
            html,
        )
        html = re.sub(
            r'href="/static/dist/admin-antd\.[^"]+\.css"',
            f'href="{manifest["admin-antd.css"]}"',
            html,
        )
        for logical, hashed in manifest.items():
            if not logical.endswith(".js"):
                continue
            base = logical[:-3]
            html = re.sub(
                rf'src="/static/js/{re.escape(logical)}[^"]*"',
                f'src="{hashed}"',
                html,
            )
            html = re.sub(
                rf'src="/static/dist/{re.escape(base)}\.[^"]+\.js"',
                f'src="{hashed}"',
                html,
            )
        path.write_text(html, encoding="utf-8")
        print("html", path.name)
    print("OK")


if __name__ == "__main__":
    main()
