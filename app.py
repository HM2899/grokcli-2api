"""
grokcli-2api — OpenAI + Anthropic compatible local API using Grok session tokens.

Endpoints:
  GET  /health
  GET  /v1/models
  POST /v1/chat/completions       (OpenAI)
  POST /chat/completions          (alias)
  POST /v1/messages               (Anthropic Messages API)
  POST /messages                  (alias)
  POST /v1/messages/count_tokens  (Anthropic token estimate)
  Admin console at /admin
"""

from __future__ import annotations

import asyncio
import json
import os
import time
import uuid
from pathlib import Path
from typing import Any, AsyncIterator, Literal

import httpx
from fastapi import Depends, FastAPI, Header, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, JSONResponse, StreamingResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, ConfigDict

import account_pool
import anthropic_compat as anth
import apikeys
import conversation_affinity
import token_maintainer
from admin_routes import router as admin_router
from auth import AuthError, GrokCredentials, load_credentials, upstream_headers
from config import (
    FORCE_UPSTREAM_STREAM,
    HOST,
    PORT,
    REASONING_COMPAT,
    SSE_KEEPALIVE_INTERVAL,
    STATIC_DIR,
    TIMEOUT,
    UPSTREAM_BASE,
)
import config as _config
from models import load_models_from_cache, resolve_model

APP_VERSION = "1.8.8"


def _on_startup() -> None:
    """Linux-friendly: normalize multi-account keys + start background workers.

    Large pools (hundreds of accounts) must not fan out network + rewrite
    multi-MB auth.json at process start — that freezes WSL. We only do a
    cheap normalize here; refresh/probe are staggered + concurrency-capped.
    """
    try:
        from oidc_auth import normalize_auth_file_keys
        from auth_store import read_auth_map

        r = normalize_auth_file_keys()
        n_accounts = len(read_auth_map()) if not r.get("total") else int(r.get("total") or 0)
        if r.get("changed"):
            print(
                f"  multi-account: remounted {r['changed']} auth key(s) "
                f"→ per-user layout (total={r.get('total')})"
            )
        else:
            print(f"  multi-account: {n_accounts} account(s) loaded")
    except Exception as e:  # noqa: BLE001
        print(f"  (auth normalize skipped: {e})")
    try:
        token_maintainer.start_background()
        ts = token_maintainer.status()
        print(
            "  token maintainer: enabled "
            f"(startup_delay={ts.get('startup_delay_sec')}s "
            f"workers={ts.get('refresh_workers')} "
            f"batch={ts.get('refresh_batch')})"
        )
    except Exception as e:  # noqa: BLE001
        print(f"  (token maintainer failed: {e})")
    try:
        import model_health

        model_health.start_background()
        mh = model_health.status()
        if mh.get("enabled") and mh.get("running"):
            print(
                "  model health: enabled "
                f"(startup_delay={mh.get('startup_delay_sec')}s "
                f"every {mh.get('interval_sec')}s "
                f"workers={mh.get('probe_workers')} "
                f"batch={mh.get('probe_batch')} "
                f"models={mh.get('probe_models')})"
            )
        else:
            print("  model health: disabled or not started")
    except Exception as e:  # noqa: BLE001
        print(f"  (model health failed: {e})")
    # Registration engine is optional — never block API startup.
    # Engine: dongguatanglinux/grok-build-auth (HTTP protocol) + MoeMail + sso_to_auth_json.
    try:
        import grok_build_adapter as _reg

        st = _reg.registration_available()
        if st.get("available"):
            print(
                "  registration: ready "
                f"(engine={st.get('engine') or 'grok-build-auth'} "
                f"build={st.get('adapter_build')})"
            )
        else:
            print(
                f"  registration: unavailable ({st.get('error')}) "
                f"(build={st.get('adapter_build')})"
            )
    except Exception as e:  # noqa: BLE001
        print(f"  registration: unavailable ({e})")


app = FastAPI(
    title="grokcli-2api",
    description=(
        "OpenAI + Anthropic Messages API compatible gateway powered by Grok OIDC "
        "session tokens. Standalone (no local Grok CLI); multi-account pool with "
        "device-code login."
    ),
    version=APP_VERSION,
    on_startup=[_on_startup],
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)

app.include_router(admin_router)


# ── request models ──────────────────────────────────────────────────────────


class ChatMessage(BaseModel):
    """OpenAI-compatible chat message, including tool / function-call fields."""

    role: str
    content: Any = None
    name: str | None = None
    # assistant → tool_calls; tool → tool_call_id; legacy function_call
    tool_calls: list[Any] | None = None
    tool_call_id: str | None = None
    function_call: Any | None = None


class ChatCompletionRequest(BaseModel):
    model_config = ConfigDict(extra="allow")

    model: str | None = None
    messages: list[ChatMessage]
    stream: bool = False
    temperature: float | None = None
    top_p: float | None = None
    max_tokens: int | None = None
    max_completion_tokens: int | None = None
    stop: str | list[str] | None = None
    presence_penalty: float | None = None
    frequency_penalty: float | None = None
    user: str | None = None
    reasoning_effort: str | None = None
    tools: list[Any] | None = None
    tool_choice: Any | None = None
    parallel_tool_calls: bool | None = None
    functions: list[Any] | None = None  # legacy OpenAI
    function_call: Any | None = None  # legacy OpenAI
    response_format: Any | None = None
    n: int | None = 1
    # Optional sticky-session hints (clients may set these)
    conversation_id: str | None = None
    metadata: dict[str, Any] | None = None


# ── auth gate for local API ─────────────────────────────────────────────────


def require_api_key(
    authorization: str | None = Header(default=None),
    x_api_key: str | None = Header(default=None, alias="x-api-key"),
) -> apikeys.ApiKeyRecord | None:
    """Validate client key when auth is required; return record or None."""
    token = None
    if authorization and authorization.lower().startswith("bearer "):
        token = authorization[7:].strip()
    elif x_api_key:
        token = x_api_key.strip()

    if not apikeys.auth_required():
        # open mode: still accept & track valid keys if provided
        if token:
            rec = apikeys.verify_key(token)
            return rec
        return None

    rec = apikeys.verify_key(token)
    if rec is None:
        raise HTTPException(status_code=401, detail="Invalid or missing API key")
    return rec


# ── helpers ─────────────────────────────────────────────────────────────────


def _normalize_content(content: Any) -> Any:
    """Keep OpenAI multimodal content; stringify only when needed."""
    if content is None:
        return None
    if isinstance(content, (str, list, dict)):
        return content
    return str(content)


def _normalize_tools(tools: list[Any] | None) -> list[Any] | None:
    """
    Accept OpenAI Chat Completions tool shape and built-in tool types.

    OpenAI function:
      {"type":"function","function":{"name":...,"description":...,"parameters":...}}
    Flat function (some SDKs):
      {"type":"function","name":...,"description":...,"parameters":...}
    Built-in web search (OpenAI Responses API / Grok):
      {"type":"web_search_preview", ...}
      {"type":"web_search", ...}
    """
    if not tools:
        return tools
    out: list[Any] = []
    for t in tools:
        if not isinstance(t, dict):
            out.append(t)
            continue
        ttype = (t.get("type") or "function").lower()
        # Built-in search tools: normalize to upstream web_search_preview
        if ttype in ("web_search_preview", "web_search", "builtin_function"):
            # Upstream requires parameters field on every tool entry
            normalized: dict[str, Any] = {
                "type": "web_search_preview",
                "parameters": {"type": "object", "properties": {}},
            }
            if isinstance(t.get("user_location"), dict):
                normalized["user_location"] = t["user_location"]
            if isinstance(t.get("search_context_size"), str):
                normalized["search_context_size"] = t["search_context_size"]
            out.append(normalized)
            continue
        if ttype != "function":
            # pass through other tool types as-is
            out.append(t)
            continue
        if isinstance(t.get("function"), dict):
            fn = t["function"]
            # Ensure parameters is present for upstream deserialization
            fn_out = dict(fn)
            if "parameters" not in fn_out:
                fn_out["parameters"] = {"type": "object", "properties": {}}
            out.append({"type": "function", "function": fn_out})
            continue
        # flatten → nest
        name = t.get("name")
        if not name:
            out.append(t)
            continue
        fn: dict[str, Any] = {"name": name}
        if t.get("description") is not None:
            fn["description"] = t["description"]
        params = t.get("parameters") if t.get("parameters") is not None else t.get("input_schema")
        if params is not None:
            fn["parameters"] = params
        out.append({"type": "function", "function": fn})
    return out


def _normalize_tool_choice(tool_choice: Any) -> Any:
    """
    Accept OpenAI Chat Completions tool_choice and map to upstream shape.
    Supports: "none" | "auto" | "required" | {"type":"function","function":{"name":"..."}}
              | {"type":"web_search_preview"} | {"type":"web_search"}
    """
    if tool_choice is None:
        return None
    if isinstance(tool_choice, str):
        return tool_choice.lower()
    if not isinstance(tool_choice, dict):
        return tool_choice
    tc_type = (tool_choice.get("type") or "function").lower()
    if tc_type in ("web_search_preview", "web_search"):
        return {"type": "web_search_preview"}
    if tc_type != "function":
        return tool_choice
    fn = tool_choice.get("function")
    if isinstance(fn, dict) and fn.get("name"):
        return {"type": "function", "function": {"name": fn["name"]}}
    return tool_choice


def _message_to_upstream(m: ChatMessage) -> dict[str, Any]:
    """Serialize a chat message including tool-call round-trip fields."""
    msg: dict[str, Any] = {"role": m.role}
    if m.name:
        msg["name"] = m.name
    if m.tool_call_id:
        msg["tool_call_id"] = m.tool_call_id
    if m.tool_calls:
        msg["tool_calls"] = m.tool_calls
    if m.function_call is not None:
        msg["function_call"] = m.function_call

    content = _normalize_content(m.content)
    # OpenAI: assistant messages with tool_calls may have content=null
    if content is None:
        if m.tool_calls or m.function_call is not None:
            msg["content"] = None
        elif m.role == "tool":
            msg["content"] = ""
        else:
            msg["content"] = ""
    else:
        msg["content"] = content
    return msg


def build_upstream_body(req: ChatCompletionRequest, model: str) -> dict[str, Any]:
    body: dict[str, Any] = {
        "model": model,
        "messages": [_message_to_upstream(m) for m in req.messages],
        "stream": True if FORCE_UPSTREAM_STREAM else bool(req.stream),
    }

    tools = _normalize_tools(req.tools)
    tool_choice = _normalize_tool_choice(req.tool_choice)
    # If client asks for grok-search model, auto-inject web_search tool
    if req.model and req.model.strip().lower() in ("grok-search", "web-search"):
        search_tool = {
            "type": "web_search_preview",
            "parameters": {"type": "object", "properties": {}},
        }
        if not tools:
            tools = [search_tool]
        elif not any(
            (t.get("type") or "").lower() in ("web_search_preview", "web_search")
            for t in tools
            if isinstance(t, dict)
        ):
            tools = tools + [search_tool]
        if tool_choice is None:
            tool_choice = {"type": "web_search_preview"}

    optional = {
        "temperature": req.temperature,
        "top_p": req.top_p,
        "max_tokens": req.max_tokens,
        "max_completion_tokens": req.max_completion_tokens,
        "stop": req.stop,
        "presence_penalty": req.presence_penalty,
        "frequency_penalty": req.frequency_penalty,
        "user": req.user,
        "reasoning_effort": req.reasoning_effort,
        "tools": tools,
        "tool_choice": tool_choice,
        "parallel_tool_calls": req.parallel_tool_calls,
        "functions": req.functions,
        "function_call": req.function_call,
        "response_format": req.response_format,
        "n": req.n,
    }
    for k, v in optional.items():
        if v is not None:
            body[k] = v
    # cli-chat-proxy / grok-4.5 rejects several OpenAI sampling knobs that
    # new-api playground enables by default (presence/frequency_penalty, etc.).
    # Strip unsupported fields so secondary relays don't surface empty streams.
    _sanitize_upstream_body(body, model=model)
    # Secondary relays (newapi/sub2api) rely on final stream usage for billing.
    _ensure_stream_include_usage(body)
    return body


# Parameters known to be rejected by cli-chat-proxy for current Grok Build models.
# Keep this list conservative: only drop fields that upstream 400s on.
_UPSTREAM_UNSUPPORTED_PARAMS = frozenset(
    {
        "presence_penalty",
        "frequency_penalty",
        # Some builds also reject these OpenAI extras when forwarded blindly.
        "logit_bias",
        "logprobs",
        "top_logprobs",
        "n",
    }
)


def _sanitize_upstream_body(body: dict[str, Any], *, model: str | None = None) -> None:
    """Drop/clamp fields that cli-chat-proxy rejects for Grok models."""
    # Always drop known-unsupported OpenAI knobs for this upstream.
    for key in list(body.keys()):
        if key in _UPSTREAM_UNSUPPORTED_PARAMS:
            body.pop(key, None)

    # n>1 is unsupported; force single completion.
    if body.get("n") not in (None, 1):
        body["n"] = 1

    # Zero penalties are still rejected by name, so already removed above.
    # Clamp temperature/top_p to sane ranges if present.
    if "temperature" in body:
        try:
            t = float(body["temperature"])
            body["temperature"] = max(0.0, min(2.0, t))
        except (TypeError, ValueError):
            body.pop("temperature", None)
    if "top_p" in body:
        try:
            p = float(body["top_p"])
            body["top_p"] = max(0.0, min(1.0, p))
        except (TypeError, ValueError):
            body.pop("top_p", None)

    # max_tokens=0 / negative is invalid for many clients and upstreams.
    for mk in ("max_tokens", "max_completion_tokens"):
        if mk in body:
            try:
                if int(body[mk]) < 1:
                    body.pop(mk, None)
            except (TypeError, ValueError):
                body.pop(mk, None)

    # new-api playground may inject non-OpenAI fields (e.g. group) via extra="allow".
    body.pop("group", None)


def _ensure_stream_include_usage(body: dict[str, Any]) -> None:
    """Ask upstream for usage on the final SSE chunk when streaming."""
    if not body.get("stream"):
        return
    opts = body.get("stream_options")
    if not isinstance(opts, dict):
        opts = {}
    else:
        opts = dict(opts)
    opts["include_usage"] = True
    body["stream_options"] = opts


def _estimate_text_tokens(text: str) -> int:
    """Rough token estimate (~4 chars / token). Enough for relay billing fallback."""
    if not text:
        return 0
    return max(1, (len(text) + 3) // 4)


def _messages_prompt_estimate(messages: Any) -> int:
    total = 0
    if not isinstance(messages, list):
        return 0
    for m in messages:
        if not isinstance(m, dict):
            continue
        content = m.get("content")
        if isinstance(content, str):
            total += _estimate_text_tokens(content)
        elif isinstance(content, list):
            for part in content:
                if isinstance(part, dict):
                    if isinstance(part.get("text"), str):
                        total += _estimate_text_tokens(part["text"])
                    elif part.get("type") == "image_url":
                        total += 85
                elif isinstance(part, str):
                    total += _estimate_text_tokens(part)
        if m.get("name"):
            total += _estimate_text_tokens(str(m["name"]))
        if isinstance(m.get("tool_calls"), list):
            for tc in m["tool_calls"]:
                if not isinstance(tc, dict):
                    continue
                fn = tc.get("function") if isinstance(tc.get("function"), dict) else {}
                total += _estimate_text_tokens(str(fn.get("name") or ""))
                total += _estimate_text_tokens(str(fn.get("arguments") or ""))
        if m.get("tool_call_id"):
            total += 4
    return total


def _completion_tokens_estimate(
    content: str = "",
    reasoning: str = "",
    tool_calls: list[Any] | None = None,
) -> int:
    total = _estimate_text_tokens(content) + _estimate_text_tokens(reasoning)
    if tool_calls:
        try:
            total += _estimate_text_tokens(json.dumps(tool_calls, ensure_ascii=False))
        except (TypeError, ValueError):
            total += _estimate_text_tokens(str(tool_calls))
    return total


def _normalize_usage(
    usage: dict[str, Any] | None,
    *,
    prompt_fallback: int = 0,
    completion_fallback: int = 0,
) -> dict[str, int]:
    """Normalize OpenAI-style usage; fill missing fields for secondary relays."""
    prompt = 0
    completion = 0
    if isinstance(usage, dict):
        try:
            prompt = int(
                usage.get("prompt_tokens")
                or usage.get("input_tokens")
                or 0
            )
        except (TypeError, ValueError):
            prompt = 0
        try:
            completion = int(
                usage.get("completion_tokens")
                or usage.get("output_tokens")
                or 0
            )
        except (TypeError, ValueError):
            completion = 0
        if not prompt and not completion:
            # Some upstreams only send total_tokens
            try:
                total_only = int(usage.get("total_tokens") or 0)
            except (TypeError, ValueError):
                total_only = 0
            if total_only > 0 and completion_fallback >= 0:
                # Prefer splitting with fallbacks when available
                if completion_fallback and completion_fallback < total_only:
                    completion = completion_fallback
                    prompt = max(0, total_only - completion)
                elif prompt_fallback and prompt_fallback < total_only:
                    prompt = prompt_fallback
                    completion = max(0, total_only - prompt)
                else:
                    prompt = total_only
    if prompt <= 0 and prompt_fallback > 0:
        prompt = prompt_fallback
    if completion <= 0 and completion_fallback > 0:
        completion = completion_fallback
    total = prompt + completion
    if isinstance(usage, dict):
        try:
            reported_total = int(usage.get("total_tokens") or 0)
        except (TypeError, ValueError):
            reported_total = 0
        if reported_total > total:
            total = reported_total
    return {
        "prompt_tokens": int(prompt),
        "completion_tokens": int(completion),
        "total_tokens": int(total),
    }


def _usage_from_body_and_output(
    body: dict[str, Any],
    *,
    content: str = "",
    reasoning: str = "",
    tool_calls: list[Any] | None = None,
    usage: dict[str, Any] | None = None,
) -> dict[str, int]:
    prompt_fb = _messages_prompt_estimate(body.get("messages"))
    # tools schema also consumes prompt tokens roughly
    if body.get("tools"):
        try:
            prompt_fb += _estimate_text_tokens(
                json.dumps(body.get("tools"), ensure_ascii=False)
            )
        except (TypeError, ValueError):
            pass
    completion_fb = _completion_tokens_estimate(content, reasoning, tool_calls)
    return _normalize_usage(
        usage, prompt_fallback=prompt_fb, completion_fallback=completion_fb
    )


async def _aiter_sse_lines_with_keepalive(
    resp: httpx.Response,
    *,
    keepalive_interval: float | None = None,
) -> AsyncIterator[str | None]:
    """
    Yield SSE lines from upstream; yield None on keepalive ticks.

    Secondary relays (newapi etc.) often idle-timeout long thinking gaps.
    None means the caller should emit an SSE comment / ping.
    """
    if keepalive_interval is None:
        keepalive_interval = max(2.0, float(SSE_KEEPALIVE_INTERVAL or 8.0))
    aiter = resp.aiter_lines()
    pending: asyncio.Future[str] | None = asyncio.ensure_future(aiter.__anext__())
    try:
        while pending is not None:
            try:
                line = await asyncio.wait_for(
                    asyncio.shield(pending), timeout=keepalive_interval
                )
            except asyncio.TimeoutError:
                yield None
                continue
            except StopAsyncIteration:
                break
            except RuntimeError as e:
                # CPython may wrap StopAsyncIteration from __anext__ as RuntimeError
                if "StopAsyncIteration" in str(e):
                    break
                raise
            yield line
            pending = asyncio.ensure_future(aiter.__anext__())
    finally:
        if pending is not None and not pending.done():
            pending.cancel()
            try:
                await pending
            except (asyncio.CancelledError, StopAsyncIteration, Exception):
                pass


def openai_error(
    message: str, status: int = 500, err_type: str = "server_error"
) -> JSONResponse:
    return JSONResponse(
        status_code=status,
        content={
            "error": {
                "message": message,
                "type": err_type,
                "code": status,
            }
        },
    )


def _sse_chunk(
    *,
    chat_id: str,
    model: str,
    created: int,
    content: str | None = None,
    role: str | None = None,
    finish_reason: str | None = None,
    reasoning: str | None = None,
    tool_calls: list[Any] | None = None,
    usage: dict[str, Any] | None = None,
    include_choices: bool = True,
) -> str:
    payload: dict[str, Any] = {
        "id": chat_id,
        "object": "chat.completion.chunk",
        "created": created,
        "model": model,
    }
    if include_choices:
        delta: dict[str, Any] = {}
        if role is not None:
            delta["role"] = role
        # Never emit empty content strings — some relays treat "" as a real token
        # and playground UIs may lock/clear the output pane on empty deltas.
        if content is not None and content != "":
            delta["content"] = content
        if reasoning is not None and reasoning != "":
            delta["reasoning_content"] = reasoning
        if tool_calls is not None:
            delta["tool_calls"] = tool_calls
        payload["choices"] = [
            {
                "index": 0,
                "delta": delta,
                "finish_reason": finish_reason,
            }
        ]
    else:
        # OpenAI final usage-only chunk uses empty choices
        payload["choices"] = []
    if usage is not None:
        payload["usage"] = usage
    return f"data: {json.dumps(payload, ensure_ascii=False)}\n\n"


class _ReasoningCompatState:
    """Track <think> open/close when rewriting reasoning for secondary relays."""

    def __init__(self, mode: str | None = None) -> None:
        self.mode = (mode or REASONING_COMPAT or "off").strip().lower()
        if self.mode in ("1", "true", "yes", "on"):
            self.mode = "think_tag"
        if self.mode not in ("off", "think_tag", "content", "none", ""):
            self.mode = "think_tag"
        if self.mode in ("none", ""):
            self.mode = "off"
        self.think_open = False
        self.saw_reasoning = False
        self.closed = False

    @property
    def enabled(self) -> bool:
        return self.mode in ("think_tag", "content")

    def rewrite(
        self, content: str | None, reasoning: str | None
    ) -> tuple[str | None, str | None]:
        """Return (content, reasoning) after compatibility rewrite."""
        if not self.enabled:
            return content, reasoning

        c = content if content else None
        r = reasoning if reasoning else None
        if not r and not c:
            return c, None

        pieces: list[str] = []
        if r:
            self.saw_reasoning = True
            if self.mode == "think_tag":
                if not self.think_open:
                    pieces.append("<think>\n")
                    self.think_open = True
                pieces.append(r)
            else:
                # plain content merge
                pieces.append(r)

        if c:
            if self.mode == "think_tag" and self.think_open and not self.closed:
                pieces.append("\n</think>\n")
                self.think_open = False
                self.closed = True
            pieces.append(c)

        out = "".join(pieces) if pieces else None
        # When rewriting into content, suppress separate reasoning_content to
        # avoid double-rendering in new-api playground / Claude UIs.
        return out, None

    def close_tag_chunk(self) -> str | None:
        if self.mode == "think_tag" and self.think_open and not self.closed:
            self.think_open = False
            self.closed = True
            return "\n</think>\n"
        return None


def _sse_keepalive() -> str:
    """SSE comment keepalive for idle gaps (newapi/nginx proxies)."""
    return ": keepalive\n\n"


def _parse_sse_line(line: str) -> dict[str, Any] | None | Literal["[DONE]"]:
    line = line.strip()
    if not line or line.startswith(":"):
        return None
    if not line.startswith("data:"):
        return None
    data = line[5:].strip()
    if data == "[DONE]":
        return "[DONE]"
    try:
        return json.loads(data)
    except json.JSONDecodeError:
        return None


def _extract_delta_text(chunk: dict[str, Any]) -> tuple[str, str]:
    """Return (content, reasoning) from various upstream chunk shapes."""
    content, reasoning, _ = _extract_delta_parts(chunk)
    return content, reasoning


def _coerce_tool_arguments(raw: Any) -> str:
    """Normalize tool arguments to the OpenAI streaming string form."""
    if raw is None:
        return ""
    if isinstance(raw, str):
        return raw
    try:
        return json.dumps(raw, ensure_ascii=False)
    except (TypeError, ValueError):
        return str(raw)


def _merge_tool_name(current: str, incoming: str) -> str:
    """
    Merge function names from streamed deltas without double-append corruption.

    OpenAI usually sends the full name once. Some proxies re-send the full name
    on later chunks; always-append would produce `web_searchweb_search` and break
    tool dispatch intermittently.
    """
    cur = (current or "").strip()
    name = (incoming or "").strip()
    if not name:
        return cur
    if not cur:
        return name
    if name == cur:
        return cur
    if name.startswith(cur):
        # progressive expansion (rare) or full name after prefix
        return name
    if cur.startswith(name):
        # ignore shorter re-send / fragment
        return cur
    # Different name on same index — prefer the newer complete token
    return name


def _legacy_function_call_to_tool_calls(function_call: Any) -> list[dict[str, Any]] | None:
    """Map deprecated OpenAI `function_call` into tool_calls deltas."""
    if not isinstance(function_call, dict):
        return None
    name = function_call.get("name")
    args = function_call.get("arguments")
    if name is None and args is None:
        return None
    return [
        {
            "index": 0,
            "id": function_call.get("id") or f"call_{uuid.uuid4().hex[:24]}",
            "type": "function",
            "function": {
                "name": str(name or ""),
                "arguments": _coerce_tool_arguments(args),
            },
        }
    ]


def _extract_delta_parts(
    chunk: dict[str, Any],
) -> tuple[str, str, list[Any] | None]:
    """Return (content, reasoning, tool_calls_delta) from upstream chunks."""
    content = ""
    reasoning = ""
    tool_calls: list[Any] | None = None

    choices = chunk.get("choices")
    if isinstance(choices, list) and choices:
        c0 = choices[0] or {}
        delta = c0.get("delta") or {}
        msg = c0.get("message") or {}
        if isinstance(delta.get("content"), str):
            content += delta["content"]
        elif isinstance(delta.get("content"), list):
            # rare content-part array
            for part in delta["content"]:
                if isinstance(part, dict) and isinstance(part.get("text"), str):
                    content += part["text"]
                elif isinstance(part, str):
                    content += part
        if isinstance(msg.get("content"), str) and not content:
            content += msg["content"]
        for key in ("reasoning_content", "reasoning", "thinking"):
            if isinstance(delta.get(key), str):
                reasoning += delta[key]
            if isinstance(msg.get(key), str) and not reasoning:
                reasoning += msg[key]

        # streaming tool_calls live on delta; complete ones may be on message
        if isinstance(delta.get("tool_calls"), list) and delta["tool_calls"]:
            tool_calls = delta["tool_calls"]
        elif isinstance(msg.get("tool_calls"), list) and msg["tool_calls"]:
            # re-emit full tool_calls as a single synthetic delta list
            tool_calls = []
            for i, tc in enumerate(msg["tool_calls"]):
                if not isinstance(tc, dict):
                    continue
                item = dict(tc)
                item.setdefault("index", i)
                # ensure arguments are strings for streaming clients
                fn = item.get("function")
                if isinstance(fn, dict) and fn.get("arguments") is not None and not isinstance(
                    fn.get("arguments"), str
                ):
                    fn = dict(fn)
                    fn["arguments"] = _coerce_tool_arguments(fn.get("arguments"))
                    item["function"] = fn
                tool_calls.append(item)
        else:
            # legacy function_call on delta or message
            fc = delta.get("function_call")
            if not isinstance(fc, dict):
                fc = msg.get("function_call")
            tool_calls = _legacy_function_call_to_tool_calls(fc)

    if not content:
        for key in ("content", "text", "output_text"):
            v = chunk.get(key)
            if isinstance(v, str):
                content = v
                break

    return content, reasoning, tool_calls


def _merge_tool_call_delta(
    acc: dict[int, dict[str, Any]], deltas: list[Any]
) -> None:
    """Accumulate streamed tool_calls deltas into complete tool_call objects."""
    for raw in deltas:
        if not isinstance(raw, dict):
            continue
        try:
            idx = int(raw.get("index", 0))
        except (TypeError, ValueError):
            idx = 0
        if idx not in acc:
            acc[idx] = {
                "id": "",
                "type": "function",
                "function": {"name": "", "arguments": ""},
            }
        entry = acc[idx]
        if raw.get("id"):
            # lock id once set to keep tool_result matching stable
            if not entry.get("id"):
                entry["id"] = raw["id"]
        if raw.get("type"):
            entry["type"] = raw["type"]
        fn = raw.get("function")
        if isinstance(fn, dict):
            if fn.get("name"):
                entry["function"]["name"] = _merge_tool_name(
                    entry["function"].get("name") or "", str(fn["name"])
                )
            if fn.get("arguments") is not None:
                entry["function"]["arguments"] = (
                    entry["function"].get("arguments") or ""
                ) + _coerce_tool_arguments(fn.get("arguments"))
        # some upstreams put name/arguments at top level
        elif raw.get("name") or raw.get("arguments") is not None:
            if raw.get("name"):
                entry["function"]["name"] = _merge_tool_name(
                    entry["function"].get("name") or "", str(raw["name"])
                )
            if raw.get("arguments") is not None:
                entry["function"]["arguments"] = (
                    entry["function"].get("arguments") or ""
                ) + _coerce_tool_arguments(raw.get("arguments"))


def _finalize_tool_calls(
    acc: dict[int, dict[str, Any]],
) -> list[dict[str, Any]] | None:
    if not acc:
        return None
    out: list[dict[str, Any]] = []
    for idx in sorted(acc.keys()):
        entry = acc[idx]
        fn = entry.get("function") or {}
        if not entry.get("id") and not fn.get("name"):
            continue
        # ensure stable id for clients that require it
        if not entry.get("id"):
            entry["id"] = f"call_{uuid.uuid4().hex[:24]}"
        entry.setdefault("type", "function")
        args = fn.get("arguments")
        if args is None:
            args = ""
        elif not isinstance(args, str):
            args = _coerce_tool_arguments(args)
        # Empty / whitespace-only args → valid empty JSON object for tool runners
        if isinstance(args, str) and not args.strip():
            args = "{}"
        entry["function"] = {
            "name": (fn.get("name") or "").strip(),
            "arguments": args,
        }
        if not entry["function"]["name"]:
            # nameless tool call is unusable — drop rather than break clients
            continue
        out.append(entry)
    return out or None


def _normalize_stream_finish_reason(
    finish: str | None, *, saw_tool_calls: bool
) -> str | None:
    """Force tool_calls finish when tools were streamed (upstream often says stop)."""
    if finish is None:
        return "tool_calls" if saw_tool_calls else None
    if saw_tool_calls and finish in ("stop", "end_turn", ""):
        return "tool_calls"
    return finish


# ── routes ──────────────────────────────────────────────────────────────────


@app.get("/health")
async def health():
    reg: dict[str, Any] = {"available": False}
    try:
        import grok_build_adapter as _reg

        reg = _reg.registration_available()
    except Exception as e:  # noqa: BLE001
        reg = {"available": False, "error": str(e)}
    try:
        pool = account_pool.pool_summary()
        creds = None
        try:
            # Health is read-only: never synchronously refresh an expired OIDC
            # token while serving the event loop.
            creds = account_pool.acquire(auto_refresh=False)
        except AuthError:
            # Keep health bounded when every token is expired; maintenance or a
            # real API request may renew tokens later.
            raise
        return {
            "status": "ok",
            "version": APP_VERSION,
            "email": creds.email,
            "expires_at": creds.expires_at,
            "auth_key": creds.auth_key,
            "upstream": UPSTREAM_BASE,
            "auth_required": apikeys.auth_required(),
            "account_mode": pool.get("mode"),
            "accounts_live": pool.get("live"),
            "accounts_enabled": pool.get("enabled"),
            "accounts_total": pool.get("total"),
            "multi_account": (pool.get("live") or 0) > 1,
            "token_maintainer": token_maintainer.status(),
            "model_health": __import__("model_health").status(),
            "conversation_affinity": conversation_affinity.status(),
            "registration": reg,
        }
    except AuthError as e:
        return JSONResponse(
            status_code=503,
            content={
                "status": "auth_error",
                "message": str(e),
                "version": APP_VERSION,
                "registration": reg,
            },
        )


def _admin_html_response():
    admin_index = STATIC_DIR / "index.html"
    if not admin_index.is_file():
        return None
    return FileResponse(
        admin_index,
        media_type="text/html; charset=utf-8",
        headers={
            "Cache-Control": "no-cache, no-store, must-revalidate",
            "Pragma": "no-cache",
        },
    )


@app.get("/")
async def root():
    html = _admin_html_response()
    if html is not None:
        return html
    return {
        "name": "grokcli-2api",
        "version": APP_VERSION,
        "docs": "/docs",
        "admin": "/admin",
        "endpoints": [
            "GET /health",
            "GET /v1/models",
            "POST /v1/chat/completions",
            "POST /v1/messages",
            "POST /v1/messages/count_tokens",
            "Admin /admin",
        ],
        "hint": (
            "OpenAI base_url → <your-host>/v1 · "
            "Anthropic base_url → <your-host> (or /v1). "
            "Use the same host/port you open in the browser; "
            "set GROK2API_PUBLIC_BASE_URL if behind reverse proxy."
        ),
    }


@app.get("/admin")
@app.get("/admin/")
@app.get("/admin/login")
@app.get("/admin/login/")
async def admin_page():
    html = _admin_html_response()
    if html is None:
        return JSONResponse(
            status_code=404,
            content={"error": "Admin UI not found. Missing static/index.html"},
        )
    return html


@app.get("/v1/models", dependencies=[Depends(require_api_key)])
@app.get("/models", dependencies=[Depends(require_api_key)])
async def list_models():
    return {"object": "list", "data": load_models_from_cache()}


def _retryable_status(code: int) -> bool:
    return code in (401, 403, 429, 500, 502, 503, 504)


async def _resolve_conversation_affinity(
    req: ChatCompletionRequest, request: Request
) -> tuple[str | None, str | None]:
    """
    Returns (fingerprint, preferred_account_id).
    Same multi-turn chat → same fingerprint → sticky account
    (pool rotation will not switch accounts mid-conversation).
    """
    conv_id = conversation_affinity.extract_conversation_id_from_headers(
        request.headers
    ) or conversation_affinity.extract_conversation_id_from_body(req)
    fp = conversation_affinity.conversation_fingerprint(
        req.messages,
        user=req.user,
        conversation_id=conv_id,
    )
    prefer = conversation_affinity.get_affinity(fp) if fp else None
    return fp, prefer


@app.post("/v1/chat/completions", dependencies=[Depends(require_api_key)])
@app.post("/chat/completions", dependencies=[Depends(require_api_key)])
async def chat_completions(req: ChatCompletionRequest, request: Request):
    if not req.messages:
        return openai_error(
            "messages is required", status=400, err_type="invalid_request_error"
        )

    conv_fp, prefer_account = await _resolve_conversation_affinity(req, request)
    model = resolve_model(req.model)

    try:
        chain = await account_pool.try_acquire_sequence_async(
            model=model, prefer_account_id=prefer_account
        )
        if not chain:
            chain = [await account_pool.acquire_async(model=model)]
    except AuthError as e:
        return openai_error(str(e), status=401, err_type="authentication_error")

    body = build_upstream_body(req, model)
    url = f"{UPSTREAM_BASE}/chat/completions"
    chat_id = f"chatcmpl-{uuid.uuid4().hex[:24]}"
    created = int(time.time())

    if req.stream:
        return StreamingResponse(
            _stream_proxy_with_failover(
                url=url,
                body=body,
                chain=chain,
                chat_id=chat_id,
                model=model,
                created=created,
                client_disconnected=request.is_disconnected,
                conversation_fp=conv_fp,
            ),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
                "X-Grok2API-Accounts": str(len(chain)),
                "X-Grok2API-Affinity": "1" if prefer_account else "0",
                **(
                    {"X-Grok2API-Conversation-Fp": conv_fp}
                    if conv_fp
                    else {}
                ),
            },
        )

    last_error: str | None = None
    last_status = 502
    used: GrokCredentials | None = None
    first_tried: str | None = chain[0].auth_key if chain else None

    for creds in chain:
        headers = upstream_headers(creds.token, model)
        try:
            content, reasoning, finish, usage, tool_calls = await _collect_completion(
                url=url, headers=headers, body=body
            )
            account_pool.report_success(creds.auth_key)
            used = creds
            # Keep multi-turn memory on this account; rebind if failover
            if conv_fp:
                if prefer_account and prefer_account != creds.auth_key:
                    await asyncio.to_thread(
                        conversation_affinity.rebind_on_failover,
                        conv_fp, first_tried, creds.auth_key,
                    )
                else:
                    await asyncio.to_thread(
                        conversation_affinity.bind_affinity,
                        conv_fp, creds.auth_key,
                    )
            message: dict[str, Any] = {
                "role": "assistant",
                "content": content if content else (None if tool_calls else ""),
            }
            if reasoning:
                message["reasoning_content"] = reasoning
            if tool_calls:
                message["tool_calls"] = tool_calls
                if not finish or finish == "stop":
                    finish = "tool_calls"
            result: dict[str, Any] = {
                "id": chat_id,
                "object": "chat.completion",
                "created": created,
                "model": model,
                "choices": [
                    {
                        "index": 0,
                        "message": message,
                        "finish_reason": finish or "stop",
                    }
                ],
            }
            result["usage"] = _usage_from_body_and_output(
                body,
                content=content or "",
                reasoning=reasoning or "",
                tool_calls=tool_calls,
                usage=usage,
            )
            # non-standard but useful for multi-account debugging
            result["x_grok2api_account"] = creds.email or creds.auth_key
            result["x_grok2api_affinity"] = bool(prefer_account)
            if conv_fp:
                result["x_grok2api_conversation_fp"] = conv_fp
            return result
        except httpx.HTTPStatusError as e:
            code = e.response.status_code if e.response is not None else 502
            detail = e.response.text[:800] if e.response is not None else str(e)
            account_pool.report_failure(
                creds.auth_key, error=detail, status_code=code, model=model
            )
            last_error = f"Upstream {code}: {detail}"
            last_status = code
            if not _retryable_status(code):
                break
            continue
        except Exception as e:  # noqa: BLE001
            account_pool.report_failure(
                creds.auth_key, error=str(e), status_code=502, model=model
            )
            last_error = f"Proxy error: {e}"
            last_status = 502
            continue

    return openai_error(
        last_error or "All accounts failed",
        status=last_status if last_status < 600 else 502,
        err_type="upstream_error",
    )


async def _stream_proxy_with_failover(
    *,
    url: str,
    body: dict[str, Any],
    chain: list[GrokCredentials],
    chat_id: str,
    model: str,
    created: int,
    client_disconnected,
    conversation_fp: str | None = None,
) -> AsyncIterator[str]:
    # Do NOT emit a premature role chunk before upstream accepts — secondary
    # relays treat early chunks as stream-started and cannot safely failover.
    last_err: str | None = None
    first_tried = chain[0].auth_key if chain else None
    role_sent = False

    for idx, creds in enumerate(chain):
        headers = upstream_headers(creds.token, model)
        finished = False
        saw_tool_calls = False
        held_finish: str | None = None
        stream_started = False  # True once any content has been sent to client
        client_gone = False
        usage: dict[str, Any] | None = None
        content_parts: list[str] = []
        reasoning_parts: list[str] = []
        tool_acc: dict[int, dict[str, Any]] = {}
        reasoning_compat = _ReasoningCompatState()
        try:
            async with httpx.AsyncClient(
                timeout=httpx.Timeout(TIMEOUT, connect=30.0),
                limits=httpx.Limits(max_keepalive_connections=20, max_connections=100),
            ) as client:
                async with client.stream(
                    "POST", url, headers=headers, json=body
                ) as resp:
                    if resp.status_code >= 400:
                        err_text = (await resp.aread()).decode(
                            "utf-8", errors="replace"
                        )[:1500]
                        account_pool.report_failure(
                            creds.auth_key,
                            error=err_text,
                            status_code=resp.status_code,
                            model=model,
                        )
                        last_err = f"Upstream {resp.status_code}: {err_text}"
                        # try next account if retryable and more remain
                        if _retryable_status(resp.status_code) and idx < len(chain) - 1:
                            continue
                        err_payload = {
                            "id": chat_id,
                            "object": "error",
                            "error": {
                                "message": last_err,
                                "type": "upstream_error",
                                "code": resp.status_code,
                            },
                        }
                        yield f"data: {json.dumps(err_payload, ensure_ascii=False)}\n\n"
                        yield "data: [DONE]\n\n"
                        return

                    account_pool.report_success(creds.auth_key)
                    if conversation_fp:
                        if idx > 0:
                            conversation_affinity.rebind_on_failover(
                                conversation_fp, first_tried, creds.auth_key
                            )
                        else:
                            conversation_affinity.bind_affinity(
                                conversation_fp, creds.auth_key
                            )

                    if not role_sent:
                        # Role-only delta (no empty content) — required for new-api playground.
                        yield _sse_chunk(
                            chat_id=chat_id,
                            model=model,
                            created=created,
                            role="assistant",
                        )
                        role_sent = True

                    ctype = (resp.headers.get("content-type") or "").lower()
                    if "text/event-stream" in ctype or "stream" in ctype:
                        async for line in _aiter_sse_lines_with_keepalive(resp):
                            # Soft disconnect check: keep draining so we can still
                            # emit a terminal finish/tool_calls frame when possible.
                            try:
                                if await client_disconnected():
                                    client_gone = True
                            except Exception:
                                client_gone = True
                            if line is None:
                                # idle keepalive for newapi / reverse proxies
                                if not client_gone:
                                    yield _sse_keepalive()
                                continue
                            parsed = _parse_sse_line(line)
                            if parsed is None:
                                continue
                            if parsed == "[DONE]":
                                break
                            assert isinstance(parsed, dict)
                            if isinstance(parsed.get("usage"), dict):
                                usage = parsed["usage"]
                            content, reasoning, tool_calls = _extract_delta_parts(
                                parsed
                            )
                            finish = None
                            choices = parsed.get("choices")
                            if isinstance(choices, list) and choices:
                                finish = choices[0].get("finish_reason")
                            # usage-only final chunk (choices empty / null)
                            if (
                                not content
                                and not reasoning
                                and not tool_calls
                                and not finish
                                and isinstance(parsed.get("usage"), dict)
                            ):
                                usage = parsed["usage"]
                                continue
                            if content:
                                content_parts.append(content)
                            if reasoning:
                                reasoning_parts.append(reasoning)
                            if tool_calls:
                                saw_tool_calls = True
                                _merge_tool_call_delta(tool_acc, tool_calls)

                            emit_content, emit_reasoning = reasoning_compat.rewrite(
                                content if content else None,
                                reasoning if reasoning else None,
                            )

                            if (
                                emit_content
                                or emit_reasoning
                                or tool_calls
                                or finish
                            ):
                                stream_started = True
                                if finish:
                                    finished = True
                                    held_finish = finish
                                # Close <think> before finish if still open.
                                if finish and not client_gone:
                                    close_tag = reasoning_compat.close_tag_chunk()
                                    if close_tag:
                                        yield _sse_chunk(
                                            chat_id=chat_id,
                                            model=model,
                                            created=created,
                                            content=close_tag,
                                        )
                                emit_finish = (
                                    _normalize_stream_finish_reason(
                                        finish, saw_tool_calls=saw_tool_calls
                                    )
                                    if finish
                                    else None
                                )
                                if client_gone:
                                    continue
                                # Skip completely empty deltas (no content/reasoning/tools/finish)
                                if not (
                                    emit_content
                                    or emit_reasoning
                                    or tool_calls
                                    or emit_finish
                                ):
                                    continue
                                yield _sse_chunk(
                                    chat_id=chat_id,
                                    model=model,
                                    created=created,
                                    content=emit_content,
                                    reasoning=emit_reasoning,
                                    tool_calls=tool_calls,
                                    finish_reason=emit_finish,
                                )
                    else:
                        raw = await resp.aread()
                        try:
                            data = json.loads(raw)
                        except json.JSONDecodeError:
                            text = raw.decode("utf-8", errors="replace")
                            content_parts.append(text)
                            stream_started = True
                            yield _sse_chunk(
                                chat_id=chat_id,
                                model=model,
                                created=created,
                                content=text,
                                finish_reason="stop",
                            )
                            finished = True
                        else:
                            if isinstance(data.get("usage"), dict):
                                usage = data["usage"]
                            content, reasoning, tool_calls = _extract_delta_parts(data)
                            msg_tool_calls: list[Any] | None = None
                            finish_reason = "stop"
                            if not content and not tool_calls:
                                choices = data.get("choices") or []
                                if choices:
                                    ch0 = choices[0] or {}
                                    msg = ch0.get("message") or {}
                                    content = msg.get("content") or ""
                                    reasoning = (
                                        msg.get("reasoning_content") or reasoning
                                    )
                                    if isinstance(msg.get("tool_calls"), list):
                                        msg_tool_calls = msg["tool_calls"]
                                    finish_reason = (
                                        ch0.get("finish_reason") or finish_reason
                                    )
                            else:
                                choices = data.get("choices") or []
                                if choices:
                                    ch0 = choices[0] or {}
                                    finish_reason = (
                                        ch0.get("finish_reason") or finish_reason
                                    )
                                    msg = ch0.get("message") or {}
                                    if (
                                        not tool_calls
                                        and isinstance(msg.get("tool_calls"), list)
                                    ):
                                        msg_tool_calls = msg["tool_calls"]

                            emit_tc = tool_calls or msg_tool_calls
                            if emit_tc:
                                saw_tool_calls = True
                                if isinstance(emit_tc, list):
                                    _merge_tool_call_delta(tool_acc, emit_tc)
                            finish_reason = _normalize_stream_finish_reason(
                                finish_reason, saw_tool_calls=saw_tool_calls
                            ) or ("tool_calls" if saw_tool_calls else "stop")
                            if content:
                                content_parts.append(content)
                            if reasoning:
                                reasoning_parts.append(reasoning)
                            stream_started = True
                            emit_content, emit_reasoning = reasoning_compat.rewrite(
                                content if content else None,
                                reasoning if reasoning else None,
                            )
                            close_tag = reasoning_compat.close_tag_chunk()
                            if close_tag:
                                emit_content = (emit_content or "") + close_tag
                            if emit_content:
                                yield _sse_chunk(
                                    chat_id=chat_id,
                                    model=model,
                                    created=created,
                                    content=emit_content,
                                )
                            if emit_reasoning and not reasoning_compat.enabled:
                                yield _sse_chunk(
                                    chat_id=chat_id,
                                    model=model,
                                    created=created,
                                    reasoning=emit_reasoning,
                                )
                            if emit_tc:
                                indexed: list[Any] = []
                                for i, tc in enumerate(emit_tc):
                                    if isinstance(tc, dict):
                                        item = dict(tc)
                                        item.setdefault("index", i)
                                        indexed.append(item)
                                    else:
                                        indexed.append(tc)
                                yield _sse_chunk(
                                    chat_id=chat_id,
                                    model=model,
                                    created=created,
                                    tool_calls=indexed,
                                )
                            yield _sse_chunk(
                                chat_id=chat_id,
                                model=model,
                                created=created,
                                finish_reason=finish_reason,
                            )
                            finished = True
                            held_finish = finish_reason

            final_tc = _finalize_tool_calls(tool_acc)
            if final_tc:
                saw_tool_calls = True
            terminal_finish = _normalize_stream_finish_reason(
                held_finish if finished else None,
                saw_tool_calls=saw_tool_calls,
            ) or ("tool_calls" if saw_tool_calls else "stop")
            if not client_gone:
                close_tag = reasoning_compat.close_tag_chunk()
                if close_tag:
                    yield _sse_chunk(
                        chat_id=chat_id,
                        model=model,
                        created=created,
                        content=close_tag,
                    )
            if not finished and not client_gone:
                yield _sse_chunk(
                    chat_id=chat_id,
                    model=model,
                    created=created,
                    finish_reason=terminal_finish,
                )
            elif finished and saw_tool_calls and held_finish in (None, "stop", "end_turn", ""):
                # Upstream finished with stop despite tools — emit a corrective
                # finish frame so secondary relays/tool runners don't hang.
                if not client_gone:
                    yield _sse_chunk(
                        chat_id=chat_id,
                        model=model,
                        created=created,
                        finish_reason="tool_calls",
                    )
            # OpenAI-compatible final usage chunk (empty choices) for sub2api/newapi
            # Prefer real completion tokens from streamed content+reasoning; many
            # relays mark empty completion_tokens as a failed playground turn.
            norm_usage = _usage_from_body_and_output(
                body,
                content="".join(content_parts),
                reasoning="".join(reasoning_parts),
                tool_calls=final_tc,
                usage=usage,
            )
            if not client_gone:
                yield _sse_chunk(
                    chat_id=chat_id,
                    model=model,
                    created=created,
                    usage=norm_usage,
                    include_choices=False,
                )
                yield "data: [DONE]\n\n"
            return
        except asyncio.CancelledError:
            return
        except Exception as e:  # noqa: BLE001
            account_pool.report_failure(creds.auth_key, error=str(e), status_code=502)
            last_err = str(e)
            # Never failover after bytes were already streamed to the client —
            # secondary relays treat that as a mid-stream corruption / break.
            if stream_started or role_sent:
                err_payload = {
                    "id": chat_id,
                    "object": "error",
                    "error": {
                        "message": last_err,
                        "type": "proxy_error",
                    },
                }
                yield f"data: {json.dumps(err_payload, ensure_ascii=False)}\n\n"
                yield "data: [DONE]\n\n"
                return
            if idx < len(chain) - 1:
                continue
            err_payload = {
                "id": chat_id,
                "object": "error",
                "error": {"message": last_err, "type": "proxy_error"},
            }
            yield f"data: {json.dumps(err_payload, ensure_ascii=False)}\n\n"
            yield "data: [DONE]\n\n"
            return

    err_payload = {
        "id": chat_id,
        "object": "error",
        "error": {
            "message": last_err or "All accounts failed",
            "type": "upstream_error",
        },
    }
    yield f"data: {json.dumps(err_payload, ensure_ascii=False)}\n\n"
    yield "data: [DONE]\n\n"


async def _collect_completion(
    *, url: str, headers: dict[str, str], body: dict[str, Any]
) -> tuple[
    str,
    str,
    str | None,
    dict[str, Any] | None,
    list[dict[str, Any]] | None,
]:
    """Consume upstream (usually SSE) and return full text + tool_calls + usage."""
    content_parts: list[str] = []
    reasoning_parts: list[str] = []
    finish: str | None = None
    usage: dict[str, Any] | None = None
    tool_acc: dict[int, dict[str, Any]] = {}
    complete_tool_calls: list[dict[str, Any]] | None = None

    # Ensure stream usage is requested when we force-stream for non-stream clients
    req_body = dict(body)
    _ensure_stream_include_usage(req_body)

    async with httpx.AsyncClient(timeout=httpx.Timeout(TIMEOUT, connect=30.0)) as client:
        async with client.stream("POST", url, headers=headers, json=req_body) as resp:
            if resp.status_code >= 400:
                raw = await resp.aread()
                # attach body text onto response for callers
                try:
                    resp._content = raw  # type: ignore[attr-defined]
                except Exception:
                    pass
                raise httpx.HTTPStatusError(
                    f"Upstream error: {raw.decode('utf-8', errors='replace')[:500]}",
                    request=resp.request,
                    response=resp,
                )

            ctype = (resp.headers.get("content-type") or "").lower()
            if "text/event-stream" in ctype or "stream" in ctype:
                async for line in resp.aiter_lines():
                    parsed = _parse_sse_line(line)
                    if parsed is None:
                        continue
                    if parsed == "[DONE]":
                        break
                    assert isinstance(parsed, dict)
                    if isinstance(parsed.get("usage"), dict):
                        usage = parsed["usage"]
                    c, r, tc_delta = _extract_delta_parts(parsed)
                    if c:
                        content_parts.append(c)
                    if r:
                        reasoning_parts.append(r)
                    if tc_delta:
                        _merge_tool_call_delta(tool_acc, tc_delta)
                    choices = parsed.get("choices")
                    if isinstance(choices, list) and choices:
                        fr = choices[0].get("finish_reason")
                        if fr:
                            finish = fr
                        # non-stream-style message embedded in SSE
                        msg = choices[0].get("message") or {}
                        if isinstance(msg.get("tool_calls"), list) and msg["tool_calls"]:
                            complete_tool_calls = [
                                tc
                                for tc in msg["tool_calls"]
                                if isinstance(tc, dict)
                            ]
            else:
                raw = await resp.aread()
                data = json.loads(raw)
                if isinstance(data.get("usage"), dict):
                    usage = data["usage"]
                choices = data.get("choices") or []
                if choices:
                    msg = (choices[0] or {}).get("message") or {}
                    content_parts.append(msg.get("content") or "")
                    if msg.get("reasoning_content"):
                        reasoning_parts.append(msg["reasoning_content"])
                    if isinstance(msg.get("tool_calls"), list):
                        complete_tool_calls = [
                            tc for tc in msg["tool_calls"] if isinstance(tc, dict)
                        ]
                    finish = choices[0].get("finish_reason") or "stop"
                else:
                    c, r, tc_delta = _extract_delta_parts(data)
                    content_parts.append(c)
                    reasoning_parts.append(r)
                    if tc_delta:
                        _merge_tool_call_delta(tool_acc, tc_delta)
                    finish = "stop"

    tool_calls = complete_tool_calls or _finalize_tool_calls(tool_acc)
    if tool_calls and (not finish or finish == "stop"):
        finish = "tool_calls"
    content = "".join(content_parts)
    reasoning = "".join(reasoning_parts)
    # Always normalize so secondary relays never see missing/zero usage
    usage = _usage_from_body_and_output(
        req_body,
        content=content,
        reasoning=reasoning,
        tool_calls=tool_calls,
        usage=usage,
    )
    return (
        content,
        reasoning,
        finish,
        usage,
        tool_calls,
    )


# ── Anthropic Messages API ──────────────────────────────────────────────────


async def _resolve_anthropic_affinity(
    req: anth.AnthropicMessagesRequest, request: Request
) -> tuple[str | None, str | None]:
    """Fingerprint for sticky multi-turn on Anthropic-shaped requests."""
    conv_id = conversation_affinity.extract_conversation_id_from_headers(
        request.headers
    )
    if not conv_id and isinstance(req.metadata, dict):
        for k in ("conversation_id", "session_id", "thread_id"):
            if req.metadata.get(k):
                conv_id = str(req.metadata[k])
                break
    oa_msgs = anth.affinity_messages_from_request(req)
    fp = conversation_affinity.conversation_fingerprint(
        oa_msgs,
        user=anth.metadata_user_id(req),
        conversation_id=conv_id,
    )
    prefer = conversation_affinity.get_affinity(fp) if fp else None
    return fp, prefer


def _anthropic_error_response(
    message: str, status: int = 500, err_type: str = "api_error"
) -> JSONResponse:
    return JSONResponse(
        status_code=status,
        content=anth.anthropic_error(message, status=status, err_type=err_type),
    )


@app.post("/v1/messages", dependencies=[Depends(require_api_key)])
@app.post("/messages", dependencies=[Depends(require_api_key)])
async def anthropic_messages(
    req: anth.AnthropicMessagesRequest,
    request: Request,
    anthropic_version: str | None = Header(default=None, alias="anthropic-version"),
):
    """
    Anthropic Messages API compatible endpoint.
    Auth: `x-api-key` or `Authorization: Bearer …` (same managed keys as OpenAI).
    Optional header: `anthropic-version` (accepted, not enforced).
    """
    _ = anthropic_version  # accepted for client compatibility
    if not req.messages:
        return _anthropic_error_response(
            "messages: Field required",
            status=400,
            err_type="invalid_request_error",
        )
    if req.max_tokens is None or req.max_tokens < 1:
        return _anthropic_error_response(
            "max_tokens: Input should be greater than or equal to 1",
            status=400,
            err_type="invalid_request_error",
        )

    conv_fp, prefer_account = await _resolve_anthropic_affinity(req, request)
    model = resolve_model(req.model)
    message_id = f"msg_{uuid.uuid4().hex[:24]}"

    try:
        chain = await account_pool.try_acquire_sequence_async(
            model=model, prefer_account_id=prefer_account
        )
        if not chain:
            chain = [await account_pool.acquire_async(model=model)]
    except AuthError as e:
        return _anthropic_error_response(
            str(e), status=401, err_type="authentication_error"
        )

    body = anth.build_openai_chat_body(
        req, model, force_stream=FORCE_UPSTREAM_STREAM
    )
    # Always stream upstream when forced; client may still want non-stream response
    if FORCE_UPSTREAM_STREAM:
        body["stream"] = True
    _ensure_stream_include_usage(body)
    url = f"{UPSTREAM_BASE}/chat/completions"

    if req.stream:
        return StreamingResponse(
            _stream_anthropic_with_failover(
                url=url,
                body=body,
                chain=chain,
                message_id=message_id,
                model=model,
                client_disconnected=request.is_disconnected,
                conversation_fp=conv_fp,
            ),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
                "X-Grok2API-Protocol": "anthropic",
                "X-Grok2API-Accounts": str(len(chain)),
                "X-Grok2API-Affinity": "1" if prefer_account else "0",
                **(
                    {"X-Grok2API-Conversation-Fp": conv_fp}
                    if conv_fp
                    else {}
                ),
            },
        )

    last_error: str | None = None
    last_status = 502
    first_tried: str | None = chain[0].auth_key if chain else None

    for creds in chain:
        headers = upstream_headers(creds.token, model)
        try:
            content, reasoning, finish, usage, tool_calls = await _collect_completion(
                url=url, headers=headers, body=body
            )
            account_pool.report_success(creds.auth_key)
            if conv_fp:
                if prefer_account and prefer_account != creds.auth_key:
                    await asyncio.to_thread(
                        conversation_affinity.rebind_on_failover,
                        conv_fp, first_tried, creds.auth_key,
                    )
                else:
                    await asyncio.to_thread(
                        conversation_affinity.bind_affinity,
                        conv_fp, creds.auth_key,
                    )

            result = anth.openai_completion_to_anthropic(
                content=content or "",
                reasoning=reasoning or "",
                finish=finish,
                usage=usage,
                tool_calls=tool_calls,
                model=model,
                message_id=message_id,
            )
            # non-standard debug fields (ignored by strict SDKs that allow extra)
            result["x_grok2api_account"] = creds.email or creds.auth_key
            result["x_grok2api_affinity"] = bool(prefer_account)
            if conv_fp:
                result["x_grok2api_conversation_fp"] = conv_fp
            return result
        except httpx.HTTPStatusError as e:
            code = e.response.status_code if e.response is not None else 502
            detail = e.response.text[:800] if e.response is not None else str(e)
            account_pool.report_failure(
                creds.auth_key, error=detail, status_code=code, model=model
            )
            last_error = f"Upstream {code}: {detail}"
            last_status = code
            if not _retryable_status(code):
                break
            continue
        except Exception as e:  # noqa: BLE001
            account_pool.report_failure(
                creds.auth_key, error=str(e), status_code=502, model=model
            )
            last_error = f"Proxy error: {e}"
            last_status = 502
            continue

    return _anthropic_error_response(
        last_error or "All accounts failed",
        status=last_status if last_status < 600 else 502,
        err_type="api_error",
    )


@app.post("/v1/messages/count_tokens", dependencies=[Depends(require_api_key)])
@app.post("/messages/count_tokens", dependencies=[Depends(require_api_key)])
async def anthropic_count_tokens(req: anth.AnthropicMessagesRequest):
    """Approximate token count (local heuristic; no upstream tokenizer)."""
    if not req.messages and req.system is None:
        return _anthropic_error_response(
            "messages or system required",
            status=400,
            err_type="invalid_request_error",
        )
    return anth.count_tokens_for_request(req)


async def _stream_anthropic_with_failover(
    *,
    url: str,
    body: dict[str, Any],
    chain: list[GrokCredentials],
    message_id: str,
    model: str,
    client_disconnected,
    conversation_fp: str | None = None,
) -> AsyncIterator[str]:
    """Upstream OpenAI SSE → Anthropic Messages SSE with account failover."""
    last_err: str | None = None
    first_tried = chain[0].auth_key if chain else None
    # Estimate prompt tokens for message_start (sub2api reads this early)
    prompt_est = _messages_prompt_estimate(body.get("messages"))
    if body.get("tools"):
        try:
            prompt_est += _estimate_text_tokens(
                json.dumps(body.get("tools"), ensure_ascii=False)
            )
        except (TypeError, ValueError):
            pass

    for idx, creds in enumerate(chain):
        headers = upstream_headers(creds.token, model)
        assembler = anth.AnthropicStreamAssembler(
            message_id=message_id, model=model
        )
        finished = False
        stream_started = False
        usage: dict[str, Any] | None = None
        held_finish: str | None = None
        try:
            async with httpx.AsyncClient(
                timeout=httpx.Timeout(TIMEOUT, connect=30.0)
            ) as client:
                async with client.stream(
                    "POST", url, headers=headers, json=body
                ) as resp:
                    if resp.status_code >= 400:
                        err_text = (await resp.aread()).decode(
                            "utf-8", errors="replace"
                        )[:1500]
                        account_pool.report_failure(
                            creds.auth_key,
                            error=err_text,
                            status_code=resp.status_code,
                            model=model,
                        )
                        last_err = f"Upstream {resp.status_code}: {err_text}"
                        if _retryable_status(resp.status_code) and idx < len(
                            chain
                        ) - 1:
                            continue
                        yield anth.anthropic_stream_error(
                            last_err, err_type="api_error"
                        )
                        return

                    account_pool.report_success(creds.auth_key)
                    if conversation_fp:
                        if idx > 0:
                            await asyncio.to_thread(
                                conversation_affinity.rebind_on_failover,
                                conversation_fp, first_tried, creds.auth_key,
                            )
                        else:
                            await asyncio.to_thread(
                                conversation_affinity.bind_affinity,
                                conversation_fp, creds.auth_key,
                            )

                    # message_start first — only after upstream accepted
                    for ev in assembler.start(input_tokens=prompt_est):
                        yield ev
                    stream_started = True

                    ctype = (resp.headers.get("content-type") or "").lower()
                    client_gone = False
                    if "text/event-stream" in ctype or "stream" in ctype:
                        async for line in _aiter_sse_lines_with_keepalive(resp):
                            try:
                                if await client_disconnected():
                                    client_gone = True
                            except Exception:
                                client_gone = True
                            if line is None:
                                if not client_gone:
                                    yield anth.anthropic_stream_ping()
                                continue
                            parsed = _parse_sse_line(line)
                            if parsed is None:
                                continue
                            if parsed == "[DONE]":
                                break
                            assert isinstance(parsed, dict)
                            if isinstance(parsed.get("usage"), dict):
                                usage = parsed["usage"]
                            content, reasoning, tool_calls = _extract_delta_parts(
                                parsed
                            )
                            finish = None
                            choices = parsed.get("choices")
                            if isinstance(choices, list) and choices:
                                finish = choices[0].get("finish_reason")
                            # usage-only final OpenAI chunk
                            if (
                                not content
                                and not reasoning
                                and not tool_calls
                                and not finish
                                and isinstance(parsed.get("usage"), dict)
                            ):
                                usage = parsed["usage"]
                                continue
                            if content or reasoning or tool_calls:
                                for ev in assembler.feed(
                                    content=content or None,
                                    reasoning=reasoning or None,
                                    tool_calls=tool_calls,
                                ):
                                    if not client_gone:
                                        yield ev
                            if finish:
                                # Capture finish but keep reading — usage often
                                # arrives on a subsequent empty-choices chunk.
                                finished = True
                                held_finish = finish
                        # Drain complete: now emit terminal events with best usage
                        fr = held_finish or (
                            "tool_calls" if assembler._saw_tool else "stop"
                        )
                        if assembler._saw_tool and fr in (
                            None,
                            "stop",
                            "end_turn",
                            "",
                        ):
                            fr = "tool_calls"
                        for ev in assembler.finish(
                            fr, usage=usage, input_tokens=prompt_est
                        ):
                            if not client_gone:
                                yield ev
                        return
                    else:
                        raw = await resp.aread()
                        try:
                            data = json.loads(raw)
                        except json.JSONDecodeError:
                            text = raw.decode("utf-8", errors="replace")
                            for ev in assembler.feed(content=text):
                                yield ev
                            for ev in assembler.finish(
                                "stop", usage=usage, input_tokens=prompt_est
                            ):
                                yield ev
                            return
                        else:
                            if isinstance(data.get("usage"), dict):
                                usage = data["usage"]
                            content, reasoning, tool_calls = _extract_delta_parts(
                                data
                            )
                            finish_reason = "stop"
                            choices = data.get("choices") or []
                            if choices:
                                ch0 = choices[0] or {}
                                msg = ch0.get("message") or {}
                                if not content:
                                    content = msg.get("content") or ""
                                if not reasoning:
                                    reasoning = msg.get("reasoning_content") or ""
                                if not tool_calls and isinstance(
                                    msg.get("tool_calls"), list
                                ):
                                    tool_calls = msg["tool_calls"]
                                # legacy function_call
                                if not tool_calls and isinstance(
                                    msg.get("function_call"), dict
                                ):
                                    tool_calls = _legacy_function_call_to_tool_calls(
                                        msg.get("function_call")
                                    )
                                finish_reason = (
                                    ch0.get("finish_reason") or finish_reason
                                )
                            if content or reasoning or tool_calls:
                                for ev in assembler.feed(
                                    content=content or None,
                                    reasoning=reasoning or None,
                                    tool_calls=tool_calls,
                                ):
                                    yield ev
                            if tool_calls and finish_reason in (
                                None,
                                "stop",
                                "end_turn",
                                "",
                            ):
                                finish_reason = "tool_calls"
                            for ev in assembler.finish(
                                finish_reason,
                                usage=usage,
                                input_tokens=prompt_est,
                            ):
                                yield ev
                            return

            if not finished:
                for ev in assembler.finish(
                    "tool_calls" if assembler._saw_tool else "stop",
                    usage=usage,
                    input_tokens=prompt_est,
                ):
                    yield ev
            return
        except asyncio.CancelledError:
            return
        except Exception as e:  # noqa: BLE001
            account_pool.report_failure(
                creds.auth_key, error=str(e), status_code=502
            )
            last_err = str(e)
            # Mid-stream failures cannot safely failover for secondary relays
            if stream_started:
                yield anth.anthropic_stream_error(
                    last_err or "proxy_error", err_type="api_error"
                )
                return
            if idx < len(chain) - 1:
                continue
            yield anth.anthropic_stream_error(
                last_err or "proxy_error", err_type="api_error"
            )
            return

    yield anth.anthropic_stream_error(
        last_err or "All accounts failed", err_type="api_error"
    )


# Mount static assets if present (css/js under /static)
if STATIC_DIR.is_dir():
    app.mount("/static", StaticFiles(directory=str(STATIC_DIR)), name="static")


def _pick_listen_host() -> str:
    """Prefer explicit env host; keep loopback accessible via 127.0.0.1."""
    return HOST or "127.0.0.1"


def _admin_url(host: str, port: int) -> str:
    # Prefer explicit public URL for server deployments.
    public = (getattr(_config, "PUBLIC_BASE_URL", "") or "").strip().rstrip("/")
    if public:
        return f"{public}/admin"
    # Local console: use 127.0.0.1 for loopback binds (avoid IPv6 ::1 quirks).
    display = "127.0.0.1" if host in ("0.0.0.0", "::", "127.0.0.1", "localhost") else host
    return f"http://{display}:{port}/admin"


def _open_admin_browser(url: str, delay: float = 1.2) -> None:
    """Open admin UI after server is likely ready (Windows-friendly)."""
    import threading
    import webbrowser

    def _run() -> None:
        import time

        time.sleep(delay)
        try:
            # Prefer os.startfile / default browser on Windows
            if os.name == "nt":
                try:
                    os.startfile(url)  # type: ignore[attr-defined]
                    return
                except OSError:
                    pass
            webbrowser.open(url)
        except Exception as e:  # noqa: BLE001
            print(f"  (could not auto-open browser: {e})")

    threading.Thread(target=_run, daemon=True).start()


def main() -> None:
    import socket

    import uvicorn

    host = _pick_listen_host()
    port = PORT
    # On Linux servers / headless, don't auto-open browser by default
    default_open = "0" if (os.name != "nt" and not os.environ.get("DISPLAY") and not os.environ.get("WAYLAND_DISPLAY")) else "1"
    open_browser = os.getenv("GROK2API_OPEN_BROWSER", default_open) not in (
        "0",
        "false",
        "False",
        "no",
    )

    # If default port is busy, try a few next ports instead of silent fail
    if os.getenv("GROK2API_PORT") is None:
        for candidate in range(port, port + 20):
            with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
                s.setsockopt(socket.SOL_SOCKET, socket.SO_REUSEADDR, 1)
                try:
                    s.bind((host if host != "0.0.0.0" else "127.0.0.1", candidate))
                except OSError:
                    continue
                port = candidate
                break
        else:
            print(f"ERROR: ports {PORT}-{PORT + 19} are all in use")
            raise SystemExit(1)

    # Keep admin API status / guide URLs in sync with actual bind
    _config.HOST = host
    _config.PORT = port

    admin = _admin_url(host, port)
    public = (getattr(_config, "PUBLIC_BASE_URL", "") or "").strip().rstrip("/")
    if public:
        link_base = public
    elif host in ("0.0.0.0", "::", "127.0.0.1", "localhost"):
        # Bind-all on a server: print both bind and local loopback convenience links.
        link_base = f"http://127.0.0.1:{port}"
    else:
        link_base = f"http://{host}:{port}"
    print(f"grokcli-2api v{APP_VERSION} listening on http://{host}:{port}")
    print(f"  OpenAI base_url:    {link_base}/v1")
    print(f"  Anthropic messages: {link_base}/v1/messages")
    print(f"  Admin console:      {admin}")
    print(f"  Docs:               {link_base}/docs")
    print(f"  Health:             {link_base}/health")
    if public:
        print(f"  Public base URL:    {public}")
    elif host in ("0.0.0.0", "::"):
        print("  Tip: set GROK2API_PUBLIC_BASE_URL=https://your.domain to avoid 127.0.0.1 links")
    print(f"  Upstream:           {UPSTREAM_BASE}")
    if port != PORT:
        print(f"  NOTE: port {PORT} busy, using {port} instead")

    if open_browser:
        print(f"  Opening browser → {admin}")
        _open_admin_browser(admin)

    # Pass app object + actual host/port (auto-picked port is used)
    uvicorn.run(app, host=host, port=port, reload=False)


if __name__ == "__main__":
    main()
