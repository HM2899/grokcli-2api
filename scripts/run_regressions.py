#!/usr/bin/env python3
"""Run every tracked standalone regression from the repository root."""

from __future__ import annotations

import os
import subprocess
import sys
import tempfile
from pathlib import Path

ROOT = Path(__file__).resolve().parents[1]


def _tracked_tests() -> list[Path]:
    result = subprocess.run(
        ["git", "ls-files", "scripts/_test_*.py"],
        cwd=ROOT,
        check=True,
        capture_output=True,
        text=True,
    )
    return [ROOT / line for line in result.stdout.splitlines() if line.strip()]


def main() -> int:
    if sys.flags.optimize:
        print("regressions require assertions; do not use python -O", file=sys.stderr)
        return 2

    tests = _tracked_tests()
    if not tests:
        print("no tracked regression scripts found", file=sys.stderr)
        return 1

    env = dict(os.environ)
    env["PYTHONPATH"] = os.pathsep.join(
        part for part in (str(ROOT), env.get("PYTHONPATH", "")) if part
    )
    # The standalone regressions are unit-level compatibility checks. Explicitly
    # disable shared stores so config.py's production localhost defaults cannot
    # make CI wait for a PostgreSQL/Redis instance that the tests never use.
    env["DATABASE_URL"] = ""
    env["GROK2API_DATABASE_URL"] = ""
    env["REDIS_URL"] = ""
    env["GROK2API_REDIS_URL"] = ""
    env["GROK2API_STORE_BACKEND"] = "file"
    env["GROK2API_WORKERS"] = "1"
    env["GROK2API_REQUIRE_SHARED_STORES"] = "0"
    env["GROK2API_OPEN_BROWSER"] = "0"
    env["PYTHONDONTWRITEBYTECODE"] = "1"
    env["PYTHONUNBUFFERED"] = "1"

    failed: list[str] = []
    with tempfile.TemporaryDirectory(prefix="g2a-regressions-") as data_dir:
        env["GROK2API_DATA_DIR"] = data_dir
        for test in tests:
            relative = test.relative_to(ROOT)
            print(f"\n==> {relative}", flush=True)
            try:
                result = subprocess.run(
                    [sys.executable, "-B", str(test)],
                    cwd=ROOT,
                    env=env,
                    timeout=120,
                )
            except subprocess.TimeoutExpired:
                print(f"TIMEOUT: {relative}", file=sys.stderr)
                failed.append(str(relative))
                continue
            if result.returncode:
                failed.append(str(relative))

    if failed:
        print("\nfailed regressions:", file=sys.stderr)
        for name in failed:
            print(f"  - {name}", file=sys.stderr)
        return 1

    print(f"\nall {len(tests)} regression scripts passed")
    return 0


if __name__ == "__main__":
    raise SystemExit(main())
