#!/usr/bin/env python3
"""Regression: YYDS accepts comma-separated configured domains."""
from __future__ import annotations

import sys
from pathlib import Path
from unittest.mock import patch

ROOT = Path(__file__).resolve().parents[1]
if str(ROOT) not in sys.path:
    sys.path.insert(0, str(ROOT))

from grok2api.upstream import moemail  # noqa: E402
from grok2api.admin.settings_store import _normalize_registration_config  # noqa: E402


def main() -> None:
    configured = ",".join(f"domain-{index}.example" for index in range(20))
    assert len(configured) > 128
    normalized = _normalize_registration_config(
        {
            "mail_provider": "yyds",
            "domain": configured,
            "yyds_domain": configured,
        },
        merge_env=False,
    )
    assert normalized["domain"] == configured
    assert normalized["yyds_domain"] == configured

    choices: list[list[str]] = []
    posted: dict = {}

    def choose(items: list[str]) -> str:
        choices.append(items)
        return items[1]

    class FakeResponse:
        status_code = 200
        text = ""

        @staticmethod
        def json() -> dict:
            return {
                "data": {
                    "id": "mailbox-1",
                    "address": "user@two.example",
                    "token": "token-1",
                }
            }

    class FakeClient:
        def __init__(self, **_kwargs) -> None:
            pass

        def __enter__(self):
            return self

        def __exit__(self, *_args) -> None:
            pass

        @staticmethod
        def post(url: str, *, json: dict, headers: dict):
            posted.update(url=url, json=json, headers=headers)
            return FakeResponse()

    with (
        patch.object(moemail.random, "choice", side_effect=choose),
        patch.object(moemail.httpx, "Client", FakeClient),
        patch.object(
            moemail,
            "yyds_pick_domain",
            side_effect=AssertionError("configured domains must skip auto-fetch"),
        ),
    ):
        result = moemail.yyds_create_mailbox(
            name="user",
            domain=" @one.example, two.example，@three.example. , ",
            api_key="AC-test",
        )

    assert choices == [["one.example", "two.example", "three.example"]]
    assert posted["json"] == {"domain": "two.example", "localPart": "user"}
    assert result["email"] == "user@two.example"
    print("OK: YYDS multi-domain selection passed")


if __name__ == "__main__":
    main()
