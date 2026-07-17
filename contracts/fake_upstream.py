#!/usr/bin/env python3
"""Deterministic Grok-like upstream for language-neutral contract tests.

The server intentionally uses only Python's standard library so CI can drive
Python and Go implementations with identical scripted upstream behavior.
"""

from __future__ import annotations

import argparse
import json
import time
from http.server import BaseHTTPRequestHandler, ThreadingHTTPServer
from urllib.parse import parse_qs, urlparse


class Handler(BaseHTTPRequestHandler):
    protocol_version = "HTTP/1.1"

    def log_message(self, fmt: str, *args: object) -> None:
        print(f"[fake-upstream] {self.address_string()} {fmt % args}")

    def do_GET(self) -> None:  # noqa: N802
        path = urlparse(self.path).path
        if path == "/health":
            self._json(200, {"ok": True})
            return
        if path == "/v1/models":
            self._json(200, {"object": "list", "data": [{"id": "grok-4.5", "object": "model", "owned_by": "xai"}]})
            return
        self._json(404, {"error": "not_found"})

    def do_POST(self) -> None:  # noqa: N802
        parsed = urlparse(self.path)
        if parsed.path != "/v1/chat/completions":
            self._json(404, {"error": "not_found"})
            return

        query = parse_qs(parsed.query)
        scenario = query.get("scenario", [self.headers.get("X-Fake-Scenario", "normal")])[0]
        length = int(self.headers.get("Content-Length", "0") or 0)
        raw = self.rfile.read(length) if length else b"{}"
        try:
            body = json.loads(raw)
        except json.JSONDecodeError:
            self._json(400, {"error": {"message": "invalid JSON", "type": "invalid_request_error"}})
            return

        if scenario.startswith("status-"):
            status = int(scenario.removeprefix("status-"))
            self._json(status, {"error": {"message": f"scripted {status}", "type": "server_error"}}, {"Retry-After": "7"})
            return
        if scenario == "empty-200":
            self.send_response(200)
            self.send_header("Content-Length", "0")
            self.end_headers()
            return
        if scenario == "html-200":
            payload = b"<!doctype html><title>scripted WAF</title>"
            self.send_response(200)
            self.send_header("Content-Type", "text/html")
            self.send_header("Content-Length", str(len(payload)))
            self.end_headers()
            self.wfile.write(payload)
            return

        model = str(body.get("model") or "grok-4.5")
        if body.get("stream"):
            self._stream(model=model, scenario=scenario)
            return
        self._json(
            200,
            {
                "id": "chatcmpl_fixture",
                "object": "chat.completion",
                "created": 1700000000,
                "model": model,
                "choices": [{"index": 0, "message": {"role": "assistant", "content": "fixture response"}, "finish_reason": "stop"}],
                "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5},
            },
        )

    def _stream(self, *, model: str, scenario: str) -> None:
        self.send_response(200)
        self.send_header("Content-Type", "text/event-stream")
        self.send_header("Cache-Control", "no-cache")
        self.send_header("Connection", "close")
        self.end_headers()

        frames = [
            {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {"role": "assistant"}, "finish_reason": None}]},
        ]
        if scenario == "tool-rewrite":
            frames.extend(
                [
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "id": "call_fixture", "type": "function", "function": {"name": "Update", "arguments": "{\"path\":\"/wrong\"}"}}]}, "finish_reason": None}]},
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {"tool_calls": [{"index": 0, "function": {"name": "Update", "arguments": "{\"file_path\":\"/right\",\"old_string\":\"a\",\"new_string\":\"\"}"}}]}, "finish_reason": None}]},
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {}, "finish_reason": "tool_calls"}], "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}},
                ]
            )
        elif scenario == "thinking":
            frames.extend(
                [
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {"reasoning_content": "plan "}, "finish_reason": None}]},
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {"content": "done"}, "finish_reason": None}]},
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}},
                ]
            )
        elif scenario == "empty-stream":
            frames.extend(
                [
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 1, "completion_tokens": 0, "total_tokens": 1}},
                ]
            )
        else:
            frames.extend(
                [
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {"content": "fixture "}, "finish_reason": None}]},
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {"content": "response"}, "finish_reason": None}]},
                    {"id": "chatcmpl_fixture", "object": "chat.completion.chunk", "created": 1700000000, "model": model, "choices": [{"index": 0, "delta": {}, "finish_reason": "stop"}], "usage": {"prompt_tokens": 3, "completion_tokens": 2, "total_tokens": 5}},
                ]
            )

        for index, frame in enumerate(frames):
            data = json.dumps(frame, ensure_ascii=False, separators=(",", ":"))
            self.wfile.write(f"data: {data}\n\n".encode())
            self.wfile.flush()
            if scenario == "slow" and index == 0:
                time.sleep(0.1)
        if scenario != "truncate":
            self.wfile.write(b"data: [DONE]\n\n")
            self.wfile.flush()
        self.close_connection = True

    def _json(self, status: int, value: object, headers: dict[str, str] | None = None) -> None:
        payload = json.dumps(value, ensure_ascii=False, separators=(",", ":")).encode()
        self.send_response(status)
        self.send_header("Content-Type", "application/json; charset=utf-8")
        self.send_header("Content-Length", str(len(payload)))
        for name, value in (headers or {}).items():
            self.send_header(name, value)
        self.end_headers()
        self.wfile.write(payload)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("--host", default="127.0.0.1")
    parser.add_argument("--port", type=int, default=18080)
    args = parser.parse_args()
    server = ThreadingHTTPServer((args.host, args.port), Handler)
    print(f"fake upstream listening on http://{args.host}:{args.port}")
    server.serve_forever()


if __name__ == "__main__":
    main()
