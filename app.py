"""
grokcli-2api — OpenAI + Anthropic compatible local API using Grok session tokens.

Endpoints:
  GET  /health
  GET  /v1/models
  POST /v1/chat/completions       (OpenAI)
  POST /chat/completions          (alias)
  POST /v1/responses              (OpenAI Responses API; used by sub2api)
  POST /responses                 (alias)
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
from contextvars import ContextVar
from pathlib import Path
from typing import Any, AsyncIterator, Literal

import httpx
from fastapi import Depends, FastAPI, Header, HTTPException, Request
from fastapi.middleware.cors import CORSMiddleware
from fastapi.responses import FileResponse, JSONResponse, Response, StreamingResponse
from fastapi.staticfiles import StaticFiles
from pydantic import BaseModel, ConfigDict

import account_pool
import anthropic_compat as anth
import apikeys
import conversation_affinity
import openai_responses as oai_resp
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
import history_compact
from models import load_models_from_cache, resolve_model

APP_VERSION = "1.9.78"

# Per-request usage context (client IP / path / UA) for request-level ledger rows.
_usage_request_ctx: ContextVar[dict[str, Any] | None] = ContextVar(
    "usage_request_ctx", default=None
)

# Client-registered tool names for this request (Claude Code Edit vs Update, …).
# Used by the OpenAI chat/completions outbound path to remap Grok's invented
# Update/StrReplace tool names to what the client actually registered.
_allowed_tool_names_ctx: ContextVar[set[str] | None] = ContextVar(
    "allowed_tool_names_ctx", default=None
)

# Shared upstream HTTP client (per process / worker) — reuse TLS + keepalive.
# Default direct client + per-proxy clients so account-pool egress can rotate
# without reopening a TCP connection on every request.
_http_client: httpx.AsyncClient | None = None
_http_clients_by_proxy: dict[str, httpx.AsyncClient] = {}
_http_client_lock = asyncio.Lock() if hasattr(asyncio, "Lock") else None  # set later


def _http_timeouts() -> tuple[httpx.Timeout, httpx.Limits]:
    max_conn = int(os.getenv("GROK2API_HTTP_MAX_CONNECTIONS", "200") or 200)
    max_keep = int(os.getenv("GROK2API_HTTP_MAX_KEEPALIVE", "50") or 50)
    # Keep connect timeout tight for TTFT.
    connect_timeout = float(os.getenv("GROK2API_HTTP_CONNECT_TIMEOUT", "5") or 5)
    connect_timeout = max(1.0, min(30.0, connect_timeout))
    # Stream-friendly timeouts:
    # - read: idle gap between SSE lines (thinking / tool prep). Must be
    #   longer than typical silence but short enough to detect dead sockets.
    # - write/pool: leave generous; long tool loops can stall writes.
    read_timeout = float(os.getenv("GROK2API_HTTP_READ_TIMEOUT", "180") or 180)
    read_timeout = max(30.0, min(float(TIMEOUT), read_timeout))
    write_timeout = float(os.getenv("GROK2API_HTTP_WRITE_TIMEOUT", "60") or 60)
    write_timeout = max(10.0, min(300.0, write_timeout))
    pool_timeout = float(os.getenv("GROK2API_HTTP_POOL_TIMEOUT", "30") or 30)
    pool_timeout = max(5.0, min(120.0, pool_timeout))
    # Overall timeout covers multi-minute streams (tool loops).
    overall = max(float(TIMEOUT), read_timeout + 60.0)
    timeout = httpx.Timeout(
        timeout=overall,
        connect=connect_timeout,
        read=read_timeout,
        write=write_timeout,
        pool=pool_timeout,
    )
    limits = httpx.Limits(
        max_keepalive_connections=max_keep,
        max_connections=max_conn,
        keepalive_expiry=90.0,
    )
    return timeout, limits


def _new_async_client(*, proxy: str | None = None) -> httpx.AsyncClient:
    timeout, limits = _http_timeouts()
    kwargs: dict[str, Any] = {
        "timeout": timeout,
        "limits": limits,
        "http2": False,
    }
    proxy_url = (proxy or "").strip()
    if proxy_url:
        # httpx>=0.28 uses `proxy=`; older used `proxies=`. Try modern first.
        try:
            client = httpx.AsyncClient(proxy=proxy_url, **kwargs)
        except TypeError:
            client = httpx.AsyncClient(
                proxies={"http://": proxy_url, "https://": proxy_url},
                **kwargs,
            )
    else:
        client = httpx.AsyncClient(**kwargs)
    # Tag the loop this client was created under. Using a client after that
    # loop dies raises ``RuntimeError: Event loop is closed`` and is the main
    # intermittent hard-cut for Claude Code → sub2api → grokcli-2api.
    try:
        loop = asyncio.get_running_loop()
        setattr(client, "_g2a_loop_id", id(loop))
        setattr(client, "_g2a_loop_closed", lambda l=loop: l.is_closed())
    except RuntimeError:
        setattr(client, "_g2a_loop_id", None)
        setattr(client, "_g2a_loop_closed", lambda: True)
    return client


def _client_loop_ok(client: httpx.AsyncClient | None) -> bool:
    """False when the cached client is closed or bound to a dead event loop."""
    if client is None:
        return False
    try:
        if client.is_closed:
            return False
    except Exception:
        return False
    try:
        closed_fn = getattr(client, "_g2a_loop_closed", None)
        if callable(closed_fn) and closed_fn():
            return False
    except Exception:
        return False
    try:
        bound = getattr(client, "_g2a_loop_id", None)
        if bound is None:
            # Created without a running loop — never safe for request path.
            return False
        running = asyncio.get_running_loop()
        if id(running) != bound:
            return False
        if running.is_closed():
            return False
    except RuntimeError:
        return False
    except Exception:
        return False
    return True


def _is_local_infra_error(err: BaseException | str | None) -> bool:
    """True for local event-loop / transport glitches (NOT upstream/account).

    These must never cool accounts or latch client_gone — both amplify
    intermittent "no reply" on the Claude Code → sub2api path.
    """
    if err is None:
        return False
    if isinstance(err, BaseException):
        text = f"{type(err).__name__}: {err}"
    else:
        text = str(err)
    low = text.lower()
    needles = (
        "event loop is closed",
        "event loop is running",
        "no running event loop",
        "attached to a different loop",
        "cannot reuse already awaited coroutine",
        "transport is closed",
        "unable to perform operation on <tcptransport",
        "runtimeerror: event loop",
    )
    return any(n in low for n in needles)


def _report_upstream_failure(
    auth_key: str | None,
    *,
    error: str | BaseException | None = None,
    status_code: int | None = None,
    model: str | None = None,
    headers: dict | None = None,
) -> None:
    """account_pool.report_failure that skips local infra errors.

    Event-loop / transport glitches are process-local; cooling the account
    empties the pool and amplifies Claude Code → sub2api intermittent no-reply.
    """
    if not auth_key:
        return
    if _is_local_infra_error(error):
        try:
            invalidate_http_clients()
        except Exception:
            pass
        return
    try:
        account_pool.report_failure(
            auth_key,
            error=str(error) if error is not None else None,
            status_code=status_code,
            model=model,
            headers=headers,
        )
    except TypeError:
        # Older account_pool without headers kw.
        try:
            account_pool.report_failure(
                auth_key,
                error=str(error) if error is not None else None,
                status_code=status_code,
                model=model,
            )
        except Exception:
            pass
    except Exception:
        pass



async def get_http_client(
    account_id: str | None = None,
    *,
    proxy: str | None = None,
) -> httpx.AsyncClient:
    """Shared AsyncClient; optionally bound to a proxy for account-pool egress.

    When a proxy pool is configured, ``account_id`` selects a sticky proxy so
    multi-turn affinity keeps the same egress IP. Direct (no-proxy) client is
    process-wide; each distinct proxy URL gets its own pooled client.

    Clients are loop-bound: a cached client created under a dead / foreign
    event loop is discarded and rebuilt (fixes warmup-via-asyncio.run pollution
    and multi-worker recycle races that surface as Event loop is closed).
    """
    global _http_client
    proxy_url = (proxy or "").strip() or None
    if proxy_url is None and account_id:
        try:
            from proxy_pool import pick_proxy_for_account

            proxy_url = pick_proxy_for_account(account_id)
        except Exception:
            proxy_url = None

    if not proxy_url:
        if _client_loop_ok(_http_client):
            return _http_client  # type: ignore[return-value]
        # Drop stale client silently (do not aclose on foreign/dead loop).
        _http_client = _new_async_client()
        return _http_client

    client = _http_clients_by_proxy.get(proxy_url)
    if _client_loop_ok(client):
        return client  # type: ignore[return-value]
    client = _new_async_client(proxy=proxy_url)
    _http_clients_by_proxy[proxy_url] = client
    # Bound the map so a huge residential pool cannot retain thousands of clients.
    if len(_http_clients_by_proxy) > 32:
        # Drop an arbitrary idle-ish entry (not the one we just created).
        for old_key in list(_http_clients_by_proxy.keys()):
            if old_key == proxy_url:
                continue
            old = _http_clients_by_proxy.pop(old_key, None)
            if old is not None and not old.is_closed:
                try:
                    if _client_loop_ok(old):
                        await old.aclose()
                except Exception:
                    pass
            break
    return client


def invalidate_http_clients() -> None:
    """Mark cached clients for rebuild (proxy config / dead-loop recovery).

    Actual close is best-effort / async-safe: schedule aclose when a loop is
    running, otherwise drop references without asyncio.run (that would create
    yet another temp loop and re-introduce Event loop is closed).
    """
    global _http_client
    clients: list[httpx.AsyncClient] = []
    if _http_client is not None:
        clients.append(_http_client)
        _http_client = None
    clients.extend(list(_http_clients_by_proxy.values()))
    _http_clients_by_proxy.clear()

    async def _close_all() -> None:
        for c in clients:
            if c is not None and not c.is_closed:
                try:
                    await c.aclose()
                except Exception:
                    pass

    try:
        loop = asyncio.get_running_loop()
    except RuntimeError:
        loop = None
    if loop is not None and loop.is_running() and not loop.is_closed():
        try:
            loop.create_task(_close_all())
        except Exception:
            pass
        return
    # No running loop: just drop refs. Closing here via asyncio.run would bind
    # aclose coroutines to a temp loop that dies immediately — worse than leak.


async def _close_http_client() -> None:
    global _http_client
    clients: list[httpx.AsyncClient] = []
    if _http_client is not None:
        clients.append(_http_client)
        _http_client = None
    clients.extend(list(_http_clients_by_proxy.values()))
    _http_clients_by_proxy.clear()
    for c in clients:
        if c is not None and not c.is_closed:
            try:
                await c.aclose()
            except Exception:
                pass


def _on_startup() -> None:
    """Linux-friendly: normalize multi-account keys + start background workers.

    Large pools (hundreds of accounts) must not fan out network + rewrite
    multi-MB auth.json at process start — that freezes WSL. We only do a
    cheap normalize here; refresh/probe are staggered + concurrency-capped.

    Multi-worker: only the elected maintainer leader starts token_maintainer
    and model_health (see store.leader).
    """
    # Fail-closed: multi-worker without Redis must not serve split-brain state.
    try:
        from store.redis_client import ensure_redis_or_raise

        ensure_redis_or_raise()
    except Exception as e:  # noqa: BLE001
        print(f"  FATAL store: {e}")
        raise

    # Shared-store status (Redis / PG)
    # Apply admin-persisted runtime settings (password-adjacent tunables live in
    # settings store and must override env defaults after multi-worker boot).
    try:
        from settings_store import apply_runtime_settings_to_modules

        apply_runtime_settings_to_modules()
    except Exception as e:  # noqa: BLE001
        print(f"  (runtime settings apply skipped: {e})")

    try:
        from store import store_status

        st = store_status()
        redis_s = st.get("redis") or {}
        pg_s = st.get("postgres") or {}
        print(
            f"  store: backend={st.get('backend')} workers={st.get('workers')} "
            f"redis={'ok' if redis_s.get('ok') else ('cfg' if redis_s.get('configured') else 'off')} "
            f"pg={'ok' if pg_s.get('ok') else ('cfg' if pg_s.get('configured') else 'off')}"
        )
    except Exception as e:  # noqa: BLE001
        print(f"  store: status unavailable ({e})")

    # Seed model catalog into PostgreSQL when empty so multi-worker /v1/models
    # reads a shared durable source of truth (no models_cache.json).
    try:
        from models import ensure_models_catalog_seeded

        seed = ensure_models_catalog_seeded()
        if seed.get("ok"):
            print(
                "  models catalog: "
                + (
                    f"seeded baseline into postgres (count={seed.get('count')})"
                    if seed.get("seeded")
                    else f"postgres ready (count={seed.get('count')})"
                )
            )
        elif seed.get("error") and seed.get("error") != "pg disabled":
            print(f"  (models catalog seed skipped: {seed.get('error')})")
    except Exception as e:  # noqa: BLE001
        print(f"  (models catalog seed skipped: {e})")

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

    # Warm request-path caches so the first user request doesn't pay cold pick.
    try:
        import time as _time

        from auth import list_live_credentials
        from settings_store import get_account_pool_state
        import account_pool as _ap

        t0 = _time.perf_counter()
        live = list_live_credentials(include_expired=True, auto_refresh=False)
        _ = get_account_pool_state()
        chain = _ap.try_acquire_sequence(model=None)
        dt = int((_time.perf_counter() - t0) * 1000)
        print(
            f"  pick warmup: live={len(live)} chain={len(chain)} "
            f"took={dt}ms"
        )
    except Exception as e:  # noqa: BLE001
        print(f"  (pick warmup skipped: {e})")

    # Warm upstream TCP/TLS with a *throwaway* sync client only.
    # NEVER call get_http_client() from asyncio.run() here: that binds the
    # process-wide AsyncClient to a temporary loop which is closed when run()
    # returns, and the next real request fails with
    # ``RuntimeError: Event loop is closed`` (intermittent hard-cut / no-reply
    # on Claude Code → sub2api → grokcli-2api).
    try:
        import threading as _threading

        def _warm_upstream() -> None:
            try:
                base = (UPSTREAM_BASE or "").rstrip("/")
                if not base:
                    return
                import httpx as _httpx

                with _httpx.Client(timeout=_httpx.Timeout(2.5, connect=1.5)) as c:
                    try:
                        c.head(base)
                    except Exception:
                        try:
                            c.get(base)
                        except Exception:
                            pass
            except Exception:
                pass

        _threading.Thread(
            target=_warm_upstream, name="g2a-upstream-warmup", daemon=True
        ).start()
        print("  upstream warmup: armed (throwaway sync client)")
    except Exception as e:  # noqa: BLE001
        print(f"  (upstream warmup skipped: {e})")

    start_maintainers = True
    try:
        from store.leader import should_start_maintainers, status as leader_status

        start_maintainers = should_start_maintainers()
        ls = leader_status()
        print(
            f"  maintainer leader: is_leader={ls.get('is_leader')} "
            f"mode={ls.get('mode')} id={ls.get('leader_id')}"
        )
    except Exception as e:  # noqa: BLE001
        print(f"  (leader election skipped: {e})")
        start_maintainers = True

    if start_maintainers:
        # One-shot cleanup: permanently invalid refresh tokens leave the pool.
        # Default is hard-delete; set GROK2API_DELETE_INVALID_REFRESH=0 to
        # soft-disable instead.
        try:
            from oidc_auth import purge_refresh_invalid_accounts

            purged = purge_refresh_invalid_accounts(dry_run=False)
            deleted_n = int(purged.get("deleted") or 0)
            disabled_n = int(purged.get("disabled") or 0)
            if deleted_n > 0 or disabled_n > 0:
                print(
                    "  token maintainer: "
                    + (
                        f"HARD-purged {deleted_n} permanently invalid account(s)"
                        if deleted_n
                        else f"soft-disabled {disabled_n} permanently invalid account(s)"
                    )
                )
        except Exception as e:  # noqa: BLE001
            print(f"  (purge refresh_invalid skipped: {e})")
        try:
            import account_pool as _ap

            rr = _ap.reenable_probe_kick_accounts()
            if int(rr.get("reenabled") or 0) > 0:
                print(
                    "  model health: re-enabled "
                    f"{rr.get('reenabled')} accounts previously hard-disabled by probe"
                )
        except Exception as e:  # noqa: BLE001
            print(f"  (reenable probe_kick skipped: {e})")
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
            if mh.get("enabled") and (mh.get("running") or mh.get("local_running")):
                print(
                    "  model health: enabled "
                    f"(startup_delay={mh.get('startup_delay_sec')}s "
                    f"every {mh.get('interval_sec')}s "
                    f"workers={mh.get('probe_workers')} "
                    f"batch={mh.get('probe_batch')} "
                    f"models={mh.get('probe_models')})"
                )
            else:
                print(
                    "  model health: "
                    + ("disabled" if not mh.get("enabled") else "started (waiting first cycle)")
                )
        except Exception as e:  # noqa: BLE001
            print(f"  (model health failed: {e})")
    else:
        # Multi-worker: this process lost the first election. store.leader keeps
        # watching and will start maintainers when the lock becomes free.
        print("  token maintainer: waiting for leader election (re-elect armed)")
        print("  model health: waiting for leader election (re-elect armed)")

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


async def _on_shutdown() -> None:
    await _close_http_client()


app = FastAPI(
    title="grokcli-2api",
    description=(
        "OpenAI + Anthropic Messages API compatible gateway powered by Grok OIDC "
        "session tokens. High-concurrency multi-worker with Redis + PostgreSQL."
    ),
    version=APP_VERSION,
    on_startup=[_on_startup],
    on_shutdown=[_on_shutdown],
)

app.add_middleware(
    CORSMiddleware,
    allow_origins=["*"],
    allow_credentials=True,
    allow_methods=["*"],
    allow_headers=["*"],
)


@app.middleware("http")
async def _usage_request_context_middleware(request: Request, call_next):
    """Capture client IP / path / UA for request-level usage events."""
    try:
        xff = (request.headers.get("x-forwarded-for") or "").split(",")[0].strip()
        if xff:
            ip = xff[:80]
        elif request.client and request.client.host:
            ip = str(request.client.host)[:80]
        else:
            ip = None
        ua = (request.headers.get("user-agent") or "")[:300] or None
        path = str(request.url.path or "")[:200] or None
        token = _usage_request_ctx.set(
            {
                "client_ip": ip,
                "user_agent": ua,
                "path": path,
            }
        )
    except Exception:
        token = _usage_request_ctx.set(None)
    try:
        return await call_next(request)
    finally:
        try:
            _usage_request_ctx.reset(token)
        except Exception:
            pass


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
    # OpenAI prompt-cache request fields (forwarded when present; also used
    # for sticky affinity even if upstream ignores them).
    prompt_cache_key: str | None = None
    prompt_cache_retention: Any | None = None


# ── auth gate for local API ─────────────────────────────────────────────────


async def require_api_key(
    authorization: str | None = Header(default=None),
    x_api_key: str | None = Header(default=None, alias="x-api-key"),
) -> apikeys.ApiKeyRecord | None:
    """Validate client key when auth is required; return record or None.

    Runs verify_key off the event loop so Redis/PG/file IO never blocks SSE.
    """
    token = None
    if authorization and authorization.lower().startswith("bearer "):
        token = authorization[7:].strip()
    elif x_api_key:
        token = x_api_key.strip()

    required = await asyncio.to_thread(apikeys.auth_required)
    if not required:
        if token:
            return await asyncio.to_thread(apikeys.verify_key, token)
        return None

    rec = await asyncio.to_thread(apikeys.verify_key, token)
    if rec is None:
        try:
            from store.metrics import inc

            inc("g2a_auth_failures_total")
        except Exception:
            pass
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


# Built-in search tool types that new-api / OpenAI clients may inject.
# cli-chat-proxy chat/completions only accepts tools[].type = function | live_search,
# and live_search now requires `sources` AND is deprecated (410 Agent Tools API).
# So for the OpenAI chat path we DROP these built-ins instead of forwarding them.
_BUILTIN_SEARCH_TOOL_TYPES = frozenset(
    {
        "web_search",
        "web_search_preview",
        "live_search",
        "x_search",
        "builtin_function",
        "builtin",
    }
)


def _is_builtin_search_tool(tool: Any) -> bool:
    if not isinstance(tool, dict):
        return False
    ttype = (tool.get("type") or "").strip().lower()
    return ttype in _BUILTIN_SEARCH_TOOL_TYPES


def _empty_tool_parameters() -> dict[str, Any]:
    """Minimal JSON Schema object accepted by strict upstream deserializers."""
    return {"type": "object", "properties": {}}


def _ensure_tool_parameters(params: Any) -> dict[str, Any]:
    """Coerce tool parameters / input_schema to a JSON-schema object.

    Upstream rejects tools when `parameters` is missing, null, a non-object, or
    an empty bare value — error looks like:
      tools[0]: missing field `parameters`
    Always return a dict with at least type=object.
    """
    if params is None:
        return _empty_tool_parameters()
    if isinstance(params, str):
        text = params.strip()
        if not text:
            return _empty_tool_parameters()
        try:
            import json as _json

            parsed = _json.loads(text)
        except Exception:
            return _empty_tool_parameters()
        return _ensure_tool_parameters(parsed)
    if not isinstance(params, dict):
        return _empty_tool_parameters()
    out = dict(params)
    # Some clients send schema without top-level type.
    if "type" not in out:
        out["type"] = "object"
    if out.get("type") == "object" and "properties" not in out:
        out["properties"] = {}
    return out


def _normalize_function_tool(t: dict[str, Any]) -> dict[str, Any] | None:
    """Normalize one function tool so function.parameters is always present."""
    if isinstance(t.get("function"), dict):
        fn = dict(t["function"])
        name = fn.get("name") or t.get("name")
        if not name:
            return None
        fn["name"] = name
        raw_params = (
            fn.get("parameters")
            if fn.get("parameters") is not None
            else fn.get("input_schema")
            if fn.get("input_schema") is not None
            else t.get("parameters")
            if t.get("parameters") is not None
            else t.get("input_schema")
        )
        fn["parameters"] = _ensure_tool_parameters(raw_params)
        # Drop alternate schema key so upstream only sees `parameters`.
        fn.pop("input_schema", None)
        if t.get("description") is not None and fn.get("description") is None:
            fn["description"] = t["description"]
        return {"type": "function", "function": fn}

    # Flat function shape: {type,name,description,parameters|input_schema}
    name = t.get("name")
    if not name:
        return None
    fn: dict[str, Any] = {"name": name}
    if t.get("description") is not None:
        fn["description"] = t["description"]
    raw_params = (
        t.get("parameters")
        if t.get("parameters") is not None
        else t.get("input_schema")
    )
    fn["parameters"] = _ensure_tool_parameters(raw_params)
    return {"type": "function", "function": fn}


def _tool_sort_key(tool: dict[str, Any]) -> str:
    """Stable name for tool ordering (prefix-cache friendly)."""
    if not isinstance(tool, dict):
        return ""
    fn = tool.get("function")
    if isinstance(fn, dict) and fn.get("name"):
        return str(fn.get("name") or "").lower()
    return str(tool.get("name") or "").lower()


def _normalize_tools(tools: list[Any] | None) -> list[Any] | None:
    """
    Accept OpenAI Chat Completions tool shape and built-in tool types.

    OpenAI function:
      {"type":"function","function":{"name":...,"description":...,"parameters":...}}
    Flat function (some SDKs):
      {"type":"function","name":...,"description":...,"parameters":...}
    Anthropic-ish:
      {"name":...,"description":...,"input_schema":...}

    Built-in web/live search tools from new-api playground / OpenAI Responses:
      {"type":"web_search" | "web_search_preview" | "live_search" | "x_search", ...}

    Upstream cli-chat-proxy chat/completions:
      - tools[].type only allows `function` | `live_search`
      - bare `live_search` → 422 missing field `sources`
      - `live_search` + sources → 410 deprecated (Agent Tools API)
      - function tools MUST include function.parameters

    Therefore built-in search tools are **stripped** on this chat path so
    new-api / relays do not surface Upstream 422. Client function tools pass
    with parameters always filled.

    Tools are sorted by function name so multi-turn requests with shuffled tool
    arrays still share the same prompt prefix (automatic upstream cache).
    """
    if not tools:
        return tools
    out: list[Any] = []
    for t in tools:
        if not isinstance(t, dict):
            continue
        ttype = (t.get("type") or "function").lower()
        # Drop built-in search tools — do not map to broken/deprecated live_search.
        if ttype in _BUILTIN_SEARCH_TOOL_TYPES:
            continue
        # Anthropic tools often omit type and only have name + input_schema.
        if ttype != "function":
            # Unknown non-function types are unsafe for this upstream; drop them
            # rather than forwarding a shape that 422s the whole request.
            # Exception: bare name+schema without type already defaulted to function.
            if t.get("type") is not None:
                continue
        norm = _normalize_function_tool(t)
        if norm is not None:
            out.append(norm)
    if not out:
        return None
    # Stable order: same tool set → same schema bytes across turns.
    out.sort(key=_tool_sort_key)
    return out


def _normalize_tool_choice(tool_choice: Any) -> Any:
    """
    Accept OpenAI Chat Completions tool_choice and map to upstream shape.

    Supports: "none" | "auto" | "required" | {"type":"function","function":{"name":"..."}}
    Also accepts Responses/Anthropic-style flat forms:
      {"type":"function","name":"Bash"} / {"type":"tool","name":"Bash"}

    cli-chat-proxy often returns HTTP 200 with empty model output for nested
    function-forced tool_choice. Prefer string "required" when a concrete tool
    is forced — that path reliably yields tool_calls for Claude Code / sub2api.
    """
    if tool_choice is None:
        return None
    if isinstance(tool_choice, str):
        s = tool_choice.lower().strip()
        if s in ("any", "tool"):
            return "required"
        return s or None
    if not isinstance(tool_choice, dict):
        return tool_choice
    tc_type = (tool_choice.get("type") or "function").lower()
    if tc_type in _BUILTIN_SEARCH_TOOL_TYPES:
        # Fall back to auto rather than forcing a deprecated live_search choice.
        return "auto"
    if tc_type in ("auto", "none", "required"):
        return tc_type
    if tc_type in ("any", "tool"):
        # Anthropic tool_choice type "any"/"tool" → force a tool call.
        return "required"
    if tc_type != "function":
        # Unknown object choice — drop to auto to avoid upstream 422.
        return "auto"
    fn = tool_choice.get("function") if isinstance(tool_choice.get("function"), dict) else {}
    name = fn.get("name") or tool_choice.get("name")
    if name:
        # Forced named tool. Upstream empty-200s on nested function form often;
        # "required" is accepted and still yields a tool call for Claude Code.
        return "required"
    return "auto"


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
    # grok-search / web-search model aliases used to auto-inject live_search.
    # Upstream now deprecates live_search on chat/completions (410 Agent Tools).
    # Keep aliases as normal chat models; do not inject broken search tools.
    if req.model and req.model.strip().lower() in ("grok-search", "web-search"):
        # If the only client tools were built-in search (now stripped), tools may
        # be None — that is intentional and avoids Upstream 422.
        if tool_choice is not None and _is_builtin_search_tool(
            tool_choice if isinstance(tool_choice, dict) else {"type": str(tool_choice)}
        ):
            tool_choice = "auto"
    # Codex compact / pure-text turns often send tool_choice without tools.
    # Upstream rejects that with 400 invalid-argument.
    has_tools = bool(tools) or bool(req.functions)
    if not has_tools:
        tool_choice = None
        parallel_tool_calls = None
        function_call = None
    else:
        parallel_tool_calls = req.parallel_tool_calls
        function_call = req.function_call

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
        "parallel_tool_calls": parallel_tool_calls,
        "functions": req.functions,
        "function_call": function_call,
        "response_format": req.response_format,
        "n": req.n,
        # Prompt-cache request hints (OpenAI / secondary relays). Kept until
        # _sanitize_upstream_body decides whether upstream accepts them.
        "prompt_cache_key": req.prompt_cache_key,
        "prompt_cache_retention": req.prompt_cache_retention,
    }
    for k, v in optional.items():
        if v is not None:
            body[k] = v
    # Also pick prompt_cache_* from pydantic extras / metadata when clients put
    # them outside the typed fields (common with new-api param overrides).
    _merge_prompt_cache_request_fields(body, req)
    # cli-chat-proxy / grok-4.5 rejects several OpenAI sampling knobs that
    # new-api playground enables by default (presence/frequency_penalty, etc.).
    # Strip unsupported fields so secondary relays don't surface empty streams.
    _sanitize_upstream_body(body, model=model)
    # Secondary relays (newapi/sub2api) rely on final stream usage for billing.
    _ensure_stream_include_usage(body)
    # Canonicalize tools/messages formatting BEFORE compact so rewrites are
    # applied to a stable base (prefix-cache hit conditions).
    _stabilize_upstream_prompt_body(body)
    # Long Claude Code tool loops → huge bodies; compact past tool results.
    _apply_history_compact(body)
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
        # OpenAI prompt-cache request fields are not accepted by cli-chat-proxy.
        # We still accept them on the public API for sticky affinity + relay
        # compatibility, then strip before upstream.
        "prompt_cache_key",
        "prompt_cache_retention",
    }
)


def _merge_prompt_cache_request_fields(body: dict[str, Any], req: Any) -> None:
    """Copy prompt_cache_key / retention from request extras into body if missing."""
    if not isinstance(body, dict):
        return
    if body.get("prompt_cache_key") in (None, ""):
        pck = conversation_affinity.extract_prompt_cache_key(req)
        if pck:
            body["prompt_cache_key"] = pck
    if body.get("prompt_cache_retention") is None:
        ret = getattr(req, "prompt_cache_retention", None)
        if ret is None and isinstance(req, dict):
            ret = req.get("prompt_cache_retention")
        if ret is None:
            extra = getattr(req, "model_extra", None)
            if isinstance(extra, dict):
                ret = extra.get("prompt_cache_retention")
        if ret is None:
            meta = getattr(req, "metadata", None)
            if meta is None and isinstance(req, dict):
                meta = req.get("metadata")
            if isinstance(meta, dict):
                ret = meta.get("prompt_cache_retention")
        if ret is not None:
            body["prompt_cache_retention"] = ret


def _sanitize_upstream_body(body: dict[str, Any], *, model: str | None = None) -> None:
    """Drop/clamp fields that cli-chat-proxy rejects for Grok models."""
    # Internal bookkeeping must never reach upstream.
    body.pop("_history_compact", None)
    body.pop("_prompt_stabilize", None)
    body.pop("_prompt_cache_key", None)
    body.pop("_prompt_cache_key_minted", None)
    body.pop("_prompt_cache_retention", None)
    # Deprecated live-search knobs → 410 on current cli-chat-proxy builds.
    body.pop("search_parameters", None)
    body.pop("web_search_options", None)
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

    # Final tools scrub: chat/completions only allows function tools, and each
    # function tool must include `parameters` (upstream 422 otherwise).
    # Re-sort by name so multi-turn tool arrays stay prefix-stable.
    tools = body.get("tools")
    if isinstance(tools, list):
        cleaned: list[Any] = []
        for t in tools:
            if not isinstance(t, dict):
                continue
            if (t.get("type") or "function").lower() != "function":
                continue
            norm = _normalize_function_tool(t)
            if norm is not None:
                cleaned.append(norm)
        if cleaned:
            cleaned.sort(key=_tool_sort_key)
            body["tools"] = cleaned
        else:
            body.pop("tools", None)
    elif tools is not None:
        body.pop("tools", None)
    # Legacy OpenAI `functions` array also needs parameters if present.
    funcs = body.get("functions")
    if isinstance(funcs, list):
        fixed_fns: list[Any] = []
        for f in funcs:
            if not isinstance(f, dict) or not f.get("name"):
                continue
            fn = dict(f)
            raw = (
                fn.get("parameters")
                if fn.get("parameters") is not None
                else fn.get("input_schema")
            )
            fn["parameters"] = _ensure_tool_parameters(raw)
            fn.pop("input_schema", None)
            fixed_fns.append(fn)
        if fixed_fns:
            body["functions"] = fixed_fns
        else:
            body.pop("functions", None)
    elif funcs is not None:
        body.pop("functions", None)

    tc = body.get("tool_choice")
    if isinstance(tc, dict):
        tc_type = (tc.get("type") or "function").lower()
        if tc_type in ("auto", "none", "required", "any", "tool"):
            # Responses / Anthropic-style {"type":"auto"|"any"|"tool"} → string form.
            # "any"/"tool" both mean "must call a tool".
            body["tool_choice"] = (
                "required" if tc_type in ("any", "tool") else tc_type
            )
        elif tc_type != "function":
            body["tool_choice"] = "auto"
        else:
            fn = tc.get("function") if isinstance(tc.get("function"), dict) else {}
            name = fn.get("name") or tc.get("name")
            if name:
                # IMPORTANT: cli-chat-proxy frequently returns HTTP 200 with empty
                # model output for nested {"type":"function","function":{"name":…}}.
                # sub2api sends this for Claude Code tool_choice forced tools and
                # then relays an empty Anthropic envelope (message_start/stop only),
                # which Claude Code reports as empty/malformed HTTP 200.
                # String "required" is accepted and reliably yields tool_calls.
                body["tool_choice"] = "required"
            else:
                body["tool_choice"] = "auto"
    elif isinstance(tc, str):
        s = tc.lower().strip()
        if s in ("any", "tool"):
            body["tool_choice"] = "required"
        elif s:
            body["tool_choice"] = s

    # Codex compact / pure-text turns often send tool_choice without tools.
    # Upstream cli-chat-proxy rejects that with 400:
    #   "A tool_choice was set on the request but no tools were specified."
    if not body.get("tools") and not body.get("functions"):
        body.pop("tool_choice", None)
        body.pop("function_call", None)
        body.pop("parallel_tool_calls", None)


def _canonicalize_json_value(value: Any) -> Any:
    """Recursively sort object keys so identical schemas serialize identically."""
    if isinstance(value, dict):
        # Stable key order: plain sorted() is enough for ASCII schema keys.
        return {k: _canonicalize_json_value(value[k]) for k in sorted(value.keys(), key=str)}
    if isinstance(value, list):
        return [_canonicalize_json_value(v) for v in value]
    return value


def _canonical_json_text(raw: Any) -> str | None:
    """Return compact, key-sorted JSON text when ``raw`` is a JSON object/array.

    Used to neutralize pretty-print / key-order churn in tool arguments so the
    multi-turn prompt *prefix* stays byte-stable (automatic upstream cache).
    """
    if raw is None:
        return None
    if isinstance(raw, (dict, list)):
        try:
            return json.dumps(
                _canonicalize_json_value(raw),
                ensure_ascii=False,
                separators=(",", ":"),
            )
        except (TypeError, ValueError):
            return None
    if not isinstance(raw, str):
        return None
    text = raw.strip()
    if not text or text[0] not in "{[":
        return None
    try:
        parsed = json.loads(text)
    except (TypeError, ValueError, json.JSONDecodeError):
        return None
    if not isinstance(parsed, (dict, list)):
        return None
    try:
        return json.dumps(
            _canonicalize_json_value(parsed),
            ensure_ascii=False,
            separators=(",", ":"),
        )
    except (TypeError, ValueError):
        return None


def _stabilize_tool_parameters(params: Any) -> dict[str, Any]:
    base = _ensure_tool_parameters(params)
    canon = _canonicalize_json_value(base)
    return canon if isinstance(canon, dict) else base


def _stabilize_tool_calls(tool_calls: Any) -> list[Any] | None:
    """Normalize assistant tool_calls for prefix stability (no reordering).

    Order of tool_calls is preserved — it must stay aligned with subsequent
    tool result messages. Only argument JSON formatting is canonicalized.
    """
    if not isinstance(tool_calls, list) or not tool_calls:
        return tool_calls if isinstance(tool_calls, list) else None
    out: list[Any] = []
    for tc in tool_calls:
        if not isinstance(tc, dict):
            out.append(tc)
            continue
        item = dict(tc)
        # Drop volatile extras some relays inject.
        for drop in ("index", "extra_content", "thought_signature"):
            # Keep "index" only if present as int and needed? OpenAI stream uses
            # index; for history messages it's optional. Dropping index improves
            # stability when some turns include it and some don't.
            item.pop(drop, None)
        fn = item.get("function")
        if isinstance(fn, dict):
            fn2 = dict(fn)
            args = fn2.get("arguments")
            canon = _canonical_json_text(args)
            if canon is not None:
                fn2["arguments"] = canon
            elif args is None:
                fn2["arguments"] = "{}"
            elif not isinstance(args, str):
                # Coerce non-string args to compact JSON when possible.
                c2 = _canonical_json_text(args)
                fn2["arguments"] = c2 if c2 is not None else str(args)
            item["function"] = fn2
        # Stable field order for serialization locality (role/content handled elsewhere).
        ordered: dict[str, Any] = {}
        if item.get("id") is not None:
            ordered["id"] = item.get("id")
        ordered["type"] = item.get("type") or "function"
        if "function" in item:
            ordered["function"] = item["function"]
        for k, v in item.items():
            if k not in ordered:
                ordered[k] = v
        out.append(ordered)
    return out


def _stabilize_text_content(text: str, *, role: str) -> str:
    """Normalize text content for prefix stability without changing meaning.

    For system prompts, collapse trailing blank lines / CRLF so clients that
    send ``str`` vs ``[{type:text}]`` still share the same prefix bytes.
    Non-system text is left intact (tool outputs / user code may be whitespace-
    sensitive).
    """
    if not isinstance(text, str):
        return str(text or "")
    if role != "system":
        return text
    norm = text.replace("\r\n", "\n").replace("\r", "\n")
    # Keep at most one trailing newline for system prompts.
    had_trailing_nl = norm.endswith("\n")
    norm = norm.rstrip("\n").rstrip()
    if had_trailing_nl or text.endswith("\n"):
        return norm + "\n" if norm else ""
    return norm


def _stabilize_message_for_cache(msg: Any) -> Any:
    """Normalize one chat message so multi-turn prefixes stay comparable."""
    if not isinstance(msg, dict):
        return msg
    role = (msg.get("role") or "").strip() or "user"
    out: dict[str, Any] = {"role": role}

    # Preserve name / tool_call_id when present (tool rounds need them).
    if msg.get("name") not in (None, ""):
        out["name"] = msg.get("name")
    if msg.get("tool_call_id") not in (None, ""):
        out["tool_call_id"] = msg.get("tool_call_id")

    tcs = msg.get("tool_calls")
    if isinstance(tcs, list) and tcs:
        out["tool_calls"] = _stabilize_tool_calls(tcs)

    fc = msg.get("function_call")
    if isinstance(fc, dict) and fc.get("name"):
        fc2 = {"name": fc.get("name")}
        args = fc.get("arguments")
        canon = _canonical_json_text(args)
        if canon is not None:
            fc2["arguments"] = canon
        elif args is not None:
            fc2["arguments"] = args if isinstance(args, str) else str(args)
        else:
            fc2["arguments"] = "{}"
        out["function_call"] = fc2

    content = msg.get("content")
    if content is None:
        # OpenAI: assistant+tool_calls may use content=null; keep that shape.
        if out.get("tool_calls") or out.get("function_call"):
            out["content"] = None
        elif role == "tool":
            out["content"] = ""
        else:
            out["content"] = ""
    elif isinstance(content, str):
        out["content"] = _stabilize_text_content(content, role=role)
    elif isinstance(content, list):
        # Multimodal / content parts: keep order, drop empty text-only noise parts.
        parts: list[Any] = []
        for part in content:
            if isinstance(part, str):
                if part != "":
                    parts.append({"type": "text", "text": part})
                continue
            if not isinstance(part, dict):
                parts.append(part)
                continue
            p = dict(part)
            ptype = (p.get("type") or "text").lower()
            if ptype in ("text", "input_text", "output_text"):
                text = p.get("text")
                if text is None:
                    continue
                parts.append({"type": "text", "text": str(text)})
            elif ptype in ("image_url", "image"):
                # Keep image parts as-is (URLs/base64 are content-critical).
                parts.append(p)
            else:
                parts.append(p)
        if not parts:
            out["content"] = ""
        elif all(
            isinstance(p, dict) and p.get("type") == "text" for p in parts
        ):
            # Flatten pure-text part lists to a single string — many clients
            # alternate str vs [{type:text}] for the same system/user content.
            joined = "\n".join(str(p.get("text") or "") for p in parts)
            out["content"] = _stabilize_text_content(joined, role=role)
        else:
            out["content"] = parts
    elif isinstance(content, dict):
        # Rare: single content object — stringify deterministically when possible.
        if content.get("type") in ("text", "input_text") and "text" in content:
            out["content"] = _stabilize_text_content(
                str(content.get("text") or ""), role=role
            )
        else:
            canon = _canonical_json_text(content)
            out["content"] = canon if canon is not None else content
    else:
        out["content"] = content

    # Pass through reasoning_content only when non-empty (some clients omit it
    # on later turns; including empty would break prefix equality).
    rc = msg.get("reasoning_content")
    if isinstance(rc, str) and rc.strip():
        out["reasoning_content"] = rc

    return out


def _stabilize_upstream_prompt_body(body: dict[str, Any]) -> dict[str, Any]:
    """Create automatic prefix-cache hit conditions on the outbound body.

    Mirrors what superagent-ai/grok-cli effectively does by replaying a stable
    local transcript: same tools schema bytes + same message prefix shape.
    Does **not** invent conversation history — only canonicalizes formatting.
    """
    if not isinstance(body, dict):
        return {"messages_stabilized": 0, "tools_stabilized": 0}

    stats = {
        "messages_stabilized": 0,
        "tools_stabilized": 0,
        "tool_args_canonicalized": 0,
    }

    tools = body.get("tools")
    if isinstance(tools, list) and tools:
        cleaned: list[Any] = []
        for t in tools:
            if not isinstance(t, dict):
                continue
            norm = _normalize_function_tool(t)
            if norm is None:
                continue
            fn = norm.get("function")
            if isinstance(fn, dict):
                fn = dict(fn)
                fn["parameters"] = _stabilize_tool_parameters(fn.get("parameters"))
                # Drop description key when empty so presence/absence doesn't churn.
                if fn.get("description") in (None, ""):
                    fn.pop("description", None)
                # Stable function key order.
                ordered_fn: dict[str, Any] = {"name": fn.get("name")}
                if "description" in fn:
                    ordered_fn["description"] = fn["description"]
                ordered_fn["parameters"] = fn.get("parameters") or _empty_tool_parameters()
                for k, v in fn.items():
                    if k not in ordered_fn:
                        ordered_fn[k] = v
                norm = {"type": "function", "function": ordered_fn}
            cleaned.append(norm)
        cleaned.sort(key=_tool_sort_key)
        body["tools"] = cleaned
        stats["tools_stabilized"] = len(cleaned)

    funcs = body.get("functions")
    if isinstance(funcs, list) and funcs:
        fixed: list[Any] = []
        for f in funcs:
            if not isinstance(f, dict) or not f.get("name"):
                continue
            fn = dict(f)
            fn["parameters"] = _stabilize_tool_parameters(
                fn.get("parameters") if fn.get("parameters") is not None else fn.get("input_schema")
            )
            fn.pop("input_schema", None)
            if fn.get("description") in (None, ""):
                fn.pop("description", None)
            ordered = {"name": fn.get("name")}
            if "description" in fn:
                ordered["description"] = fn["description"]
            ordered["parameters"] = fn.get("parameters") or _empty_tool_parameters()
            for k, v in fn.items():
                if k not in ordered:
                    ordered[k] = v
            fixed.append(ordered)
        fixed.sort(key=lambda x: str(x.get("name") or "").lower())
        body["functions"] = fixed

    messages = body.get("messages")
    if isinstance(messages, list) and messages:
        new_msgs: list[Any] = []
        for m in messages:
            sm = _stabilize_message_for_cache(m)
            if (
                isinstance(sm, dict)
                and isinstance(sm.get("tool_calls"), list)
            ):
                # Count args we rewrote (heuristic: compact form).
                for tc in sm["tool_calls"]:
                    if not isinstance(tc, dict):
                        continue
                    fn = tc.get("function")
                    if isinstance(fn, dict) and isinstance(fn.get("arguments"), str):
                        stats["tool_args_canonicalized"] += 1
            new_msgs.append(sm)
            stats["messages_stabilized"] += 1
        body["messages"] = new_msgs

    # Drop client fields that are not part of the model prompt but sometimes
    # confuse secondary relays / get echoed into logs only.
    # (Do not drop model/stream/tools/messages.)
    body.pop("metadata", None)

    body["_prompt_stabilize"] = stats
    return stats


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


def _body_for_upstream(body: dict[str, Any]) -> dict[str, Any]:
    """Copy body without private grokcli-2api keys (never send to cli-chat-proxy)."""
    if not isinstance(body, dict):
        return body
    out = dict(body)
    out.pop("_history_compact", None)
    out.pop("_prompt_stabilize", None)
    out.pop("_prompt_cache_key", None)
    out.pop("_prompt_cache_key_minted", None)
    out.pop("_prompt_cache_retention", None)
    # OpenAI prompt-cache request fields are for sticky affinity / client echo only.
    out.pop("prompt_cache_key", None)
    out.pop("prompt_cache_retention", None)
    return out


def _apply_history_compact(body: dict[str, Any]) -> dict[str, Any]:
    """Compact inbound messages on an OpenAI-style upstream body; stash stats.

    Auto-enables when the messages JSON exceeds HISTORY_COMPACT_AUTO_CHARS so
    Codex / long agent loops do not push 100k+ token prompts (TTFT explodes).
    """
    force = False
    try:
        force = history_compact.should_auto_compact(body)
    except Exception:
        force = False
    stats = history_compact.compact_upstream_body(body, force=force)
    body["_history_compact"] = stats
    return stats


def _history_compact_headers(body: dict[str, Any]) -> dict[str, str]:
    """Expose compaction stats on responses for debugging long tool sessions."""
    stats = body.get("_history_compact") if isinstance(body, dict) else None
    if not isinstance(stats, dict):
        return {}
    hdr: dict[str, str] = {
        "X-Grok2API-History-Compact": "1" if stats.get("applied") else "0",
    }
    if stats.get("before_chars") is not None:
        hdr["X-Grok2API-History-Before"] = str(stats.get("before_chars"))
    if stats.get("after_chars") is not None:
        hdr["X-Grok2API-History-After"] = str(stats.get("after_chars"))
    if stats.get("tool_rounds") is not None:
        hdr["X-Grok2API-History-Tool-Rounds"] = str(stats.get("tool_rounds"))
    if stats.get("prefix_stable") is not None:
        hdr["X-Grok2API-History-Prefix-Stable"] = (
            "1" if stats.get("prefix_stable") else "0"
        )
    if stats.get("auto"):
        hdr["X-Grok2API-History-Auto"] = "1"
    return hdr


def _prompt_stabilize_headers(body: dict[str, Any]) -> dict[str, str]:
    """Expose outbound prompt stabilization stats for cache-hit debugging."""
    stats = body.get("_prompt_stabilize") if isinstance(body, dict) else None
    if not isinstance(stats, dict):
        return {"X-Grok2API-Prompt-Stable": "0"}
    return {
        "X-Grok2API-Prompt-Stable": "1",
        "X-Grok2API-Prompt-Stable-Messages": str(stats.get("messages_stabilized") or 0),
        "X-Grok2API-Prompt-Stable-Tools": str(stats.get("tools_stabilized") or 0),
    }


def _ensure_prompt_cache_key(
    *,
    body: dict[str, Any] | None = None,
    req: Any = None,
    headers: Any = None,
    api_key_id: str | None = None,
    conversation_id: str | None = None,
    previous_response_id: str | None = None,
    user: str | None = None,
    seed: str | None = None,
) -> tuple[str | None, bool]:
    """Return ``(prompt_cache_key, minted)``.

    Prefer client-provided key (body / metadata / headers). When missing, mint a
    stable key from conversation / previous_response_id / user so multi-turn
    stickiness works for sub2api/Claude Code even without an explicit cache key.
    The minted key is written onto ``body`` for later echo in responses.
    """
    pck = None
    if isinstance(body, dict):
        pck = conversation_affinity.normalize_prompt_cache_key(body.get("prompt_cache_key"))
    if not pck and req is not None:
        pck = conversation_affinity.extract_prompt_cache_key(req)
    if not pck and headers is not None:
        pck = conversation_affinity.extract_prompt_cache_key_from_headers(headers)
    if pck:
        if isinstance(body, dict):
            body["prompt_cache_key"] = pck
            body["_prompt_cache_key"] = pck
            body["_prompt_cache_key_minted"] = False
        return pck, False

    minted = conversation_affinity.mint_prompt_cache_key(
        api_key_id=api_key_id,
        conversation_id=conversation_id,
        previous_response_id=previous_response_id,
        user=user,
        seed=seed,
    )
    if isinstance(body, dict):
        body["prompt_cache_key"] = minted
        body["_prompt_cache_key"] = minted
        body["_prompt_cache_key_minted"] = True
    return minted, True


def _prompt_cache_key_headers(
    body: dict[str, Any] | None = None,
    *,
    prompt_cache_key: str | None = None,
    minted: bool | None = None,
) -> dict[str, str]:
    """Echo the effective prompt_cache_key so clients can reuse it next turn."""
    pck = prompt_cache_key
    is_minted = minted
    if isinstance(body, dict):
        if not pck:
            pck = body.get("_prompt_cache_key") or body.get("prompt_cache_key")
        if is_minted is None:
            is_minted = bool(body.get("_prompt_cache_key_minted"))
    pck = conversation_affinity.normalize_prompt_cache_key(pck)
    if not pck:
        return {}
    return {
        "X-Grok2API-Prompt-Cache-Key": pck,
        "X-Grok2API-Prompt-Cache-Key-Minted": "1" if is_minted else "0",
    }


def _cache_debug_headers(usage: dict[str, Any] | None) -> dict[str, str]:
    """Response headers for cache-hit observability (SSE + JSON)."""
    cached = _cache_tokens_from_usage(usage)
    hdr = {"X-Grok2API-Cache-Read-Tokens": str(int(cached))}
    prompt = 0
    if isinstance(usage, dict):
        try:
            prompt = int(usage.get("prompt_tokens") or usage.get("input_tokens") or 0)
        except (TypeError, ValueError):
            prompt = 0
    if prompt > 0 and cached > 0:
        ratio = min(1.0, cached / float(prompt))
        hdr["X-Grok2API-Cache-Hit-Ratio"] = f"{ratio:.4f}"
    else:
        hdr["X-Grok2API-Cache-Hit-Ratio"] = "0"
    return hdr


def _cache_tokens_from_usage(usage: dict[str, Any] | None) -> int:
    """Best-effort cached prompt tokens from a normalized or raw usage dict."""
    if not isinstance(usage, dict):
        return 0
    return int(
        _usage_detail_int(
            usage,
            ("prompt_tokens_details", "cached_tokens"),
            ("input_tokens_details", "cached_tokens"),
            "cached_tokens",
            "cache_read_input_tokens",
            "prompt_cache_hit_tokens",
            "cache_read_tokens",
        )
        or 0
    )


def _attach_cache_debug_fields(
    result: dict[str, Any],
    usage: dict[str, Any] | None,
    *,
    prefer_account: bool | None = None,
    prompt_cache_key: str | None = None,
    prompt_cache_key_minted: bool | None = None,
    body: dict[str, Any] | None = None,
) -> None:
    """Non-standard response fields so ops can see cache hits without log diving."""
    if not isinstance(result, dict):
        return
    cached = _cache_tokens_from_usage(usage if isinstance(usage, dict) else result.get("usage"))
    result["x_grok2api_cache_read_tokens"] = int(cached)
    prompt = 0
    u = usage if isinstance(usage, dict) else result.get("usage")
    if isinstance(u, dict):
        try:
            prompt = int(u.get("prompt_tokens") or u.get("input_tokens") or 0)
        except (TypeError, ValueError):
            prompt = 0
    if prompt > 0 and cached > 0:
        result["x_grok2api_cache_hit_ratio"] = round(min(1.0, cached / float(prompt)), 4)
    else:
        result["x_grok2api_cache_hit_ratio"] = 0.0
    if prefer_account is not None:
        result["x_grok2api_affinity"] = bool(prefer_account)
    pck = prompt_cache_key
    minted = prompt_cache_key_minted
    if isinstance(body, dict):
        if not pck:
            pck = body.get("_prompt_cache_key") or body.get("prompt_cache_key")
        if minted is None and "_prompt_cache_key_minted" in body:
            minted = bool(body.get("_prompt_cache_key_minted"))
    pck = conversation_affinity.normalize_prompt_cache_key(pck)
    if pck:
        result["x_grok2api_prompt_cache_key"] = pck
        # OpenAI-compatible echo so clients that read prompt_cache_key can reuse it.
        if result.get("prompt_cache_key") in (None, ""):
            result["prompt_cache_key"] = pck
        if minted is not None:
            result["x_grok2api_prompt_cache_key_minted"] = bool(minted)


def _json_result_with_cache_headers(
    result: dict[str, Any],
    *,
    body: dict[str, Any] | None = None,
    prefer_account: bool | None = None,
    conv_fp: str | None = None,
    status_code: int = 200,
    prompt_cache_key: str | None = None,
    affinity_source: str | None = None,
) -> JSONResponse:
    """Wrap a successful non-stream result with affinity / cache / compact headers."""
    usage = result.get("usage") if isinstance(result, dict) else None
    headers: dict[str, str] = {}
    if prefer_account is not None:
        headers["X-Grok2API-Affinity"] = "1" if prefer_account else "0"
    if affinity_source:
        headers["X-Grok2API-Affinity-Source"] = str(affinity_source)
    if conv_fp:
        headers["X-Grok2API-Conversation-Fp"] = str(conv_fp)
    headers.update(_cache_debug_headers(usage if isinstance(usage, dict) else None))
    if isinstance(body, dict):
        headers.update(_history_compact_headers(body))
        headers.update(_prompt_stabilize_headers(body))
        headers.update(_prompt_cache_key_headers(body, prompt_cache_key=prompt_cache_key))
    elif prompt_cache_key:
        headers.update(_prompt_cache_key_headers(prompt_cache_key=prompt_cache_key))
    return JSONResponse(content=result, status_code=status_code, headers=headers)


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


def _usage_detail_int(usage: dict[str, Any] | None, *paths: Any) -> int:
    """Read a nested usage detail int from the first matching path.

    Each path is either a top-level key (str) or a (parent, child) pair for
    nested objects like prompt_tokens_details.cached_tokens.
    """
    if not isinstance(usage, dict):
        return 0
    for path in paths:
        try:
            if isinstance(path, (tuple, list)) and len(path) == 2:
                parent, child = path
                node = usage.get(parent)
                if not isinstance(node, dict):
                    continue
                val = int(node.get(child) or 0)
            else:
                val = int(usage.get(path) or 0)
        except (TypeError, ValueError):
            continue
        if val > 0:
            return val
    return 0


def _normalize_usage(
    usage: dict[str, Any] | None,
    *,
    prompt_fallback: int = 0,
    completion_fallback: int = 0,
) -> dict[str, Any]:
    """Normalize OpenAI-style usage; fill missing fields for secondary relays.

    Always includes cache/reasoning detail containers (0 when unknown) so
    sub2api / Claude Code can read prompt-cache fields instead of treating
    a missing key as "no cache support".
    """
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

    # Prompt-cache / reasoning details: passthrough only, never invent hits.
    cached = _usage_detail_int(
        usage,
        ("prompt_tokens_details", "cached_tokens"),
        ("input_tokens_details", "cached_tokens"),
        "cached_tokens",
        "cache_read_input_tokens",
        "prompt_cache_hit_tokens",
    )
    cache_creation = _usage_detail_int(
        usage,
        "cache_creation_input_tokens",
        ("prompt_tokens_details", "cache_creation_tokens"),
        ("input_tokens_details", "cache_creation_tokens"),
    )
    reasoning = _usage_detail_int(
        usage,
        ("completion_tokens_details", "reasoning_tokens"),
        ("output_tokens_details", "reasoning_tokens"),
        "reasoning_tokens",
    )

    return {
        "prompt_tokens": int(prompt),
        "completion_tokens": int(completion),
        "total_tokens": int(total),
        # Dual aliases so chat-completions and Responses clients both work.
        "input_tokens": int(prompt),
        "output_tokens": int(completion),
        "prompt_tokens_details": {"cached_tokens": int(cached)},
        "input_tokens_details": {"cached_tokens": int(cached)},
        "completion_tokens_details": {"reasoning_tokens": int(reasoning)},
        "output_tokens_details": {"reasoning_tokens": int(reasoning)},
        # Anthropic-shaped mirrors (harmless for OpenAI clients).
        "cache_read_input_tokens": int(cached),
        "cache_creation_input_tokens": int(cache_creation),
    }


def _usage_from_body_and_output(
    body: dict[str, Any],
    *,
    content: str = "",
    reasoning: str = "",
    tool_calls: list[Any] | None = None,
    usage: dict[str, Any] | None = None,
) -> dict[str, Any]:
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


def _capture_usage_request_ctx(request: Request | None = None) -> dict[str, Any]:
    """Snapshot request meta for usage events (safe to pass into stream tasks)."""
    ctx = dict(_usage_request_ctx.get() or {})
    if request is None:
        return ctx
    try:
        if not ctx.get("client_ip"):
            xff = (request.headers.get("x-forwarded-for") or "").split(",")[0].strip()
            if xff:
                ctx["client_ip"] = xff[:80]
            elif request.client and request.client.host:
                ctx["client_ip"] = str(request.client.host)[:80]
        if not ctx.get("user_agent"):
            ua = (request.headers.get("user-agent") or "")[:300]
            if ua:
                ctx["user_agent"] = ua
        if not ctx.get("path"):
            path = str(request.url.path or "")[:200]
            if path:
                ctx["path"] = path
    except Exception:
        pass
    return ctx


def _bind_usage_request_ctx(ctx: dict[str, Any] | None):
    """Bind usage request context for the current async task; returns reset token."""
    try:
        return _usage_request_ctx.set(dict(ctx) if isinstance(ctx, dict) else None)
    except Exception:
        return None


def _reset_usage_request_ctx(token) -> None:
    if token is None:
        return
    try:
        _usage_request_ctx.reset(token)
    except Exception:
        pass


def _ttft_log_enabled() -> bool:
    """Default on. Set GROK2API_TTFT_LOG=0 to silence first-token timing logs."""
    raw = (os.getenv("GROK2API_TTFT_LOG") or "1").strip().lower()
    return raw not in ("0", "false", "no", "off")


def _ms_since(t0: float | None) -> int | None:
    if t0 is None:
        return None
    try:
        return max(0, int(round((time.perf_counter() - float(t0)) * 1000.0)))
    except Exception:
        return None


def _short_account_id(account_id: str | None) -> str:
    s = str(account_id or "").strip()
    if not s:
        return "-"
    if "::" in s:
        s = s.rsplit("::", 1)[-1]
    return s[:16]


class RequestTiming:
    """Lightweight first-token timing for chat / responses / anthropic streams.

    Logs one line when first client-visible content is produced (or on failure).
    Env: GROK2API_TTFT_LOG=1|0  (default 1)

    Fields:
      early_open — SSE envelope (message_start / response.created) opened at
                   upstream 200, before model tokens (v1.9.72+ default).
      tools_req  — request includes tools (pre-tool hold may delay first body).
      held       — first model bytes were buffered by tools-preface hold.
    """

    __slots__ = (
        "protocol",
        "model",
        "stream",
        "req_id",
        "t0",
        "t_affinity_done",
        "t_pick_done",
        "t_upstream_start",
        "t_upstream_headers",
        "t_envelope_open",
        "t_first_token",
        "account_id",
        "chain_n",
        "attempt",
        "affinity",
        "early_open",
        "tools_req",
        "held",
        "logged",
    )

    def __init__(
        self,
        *,
        protocol: str,
        model: str | None = None,
        stream: bool = True,
        req_id: str | None = None,
    ) -> None:
        self.protocol = protocol
        self.model = model or "-"
        self.stream = bool(stream)
        self.req_id = (req_id or uuid.uuid4().hex[:10])[:16]
        self.t0 = time.perf_counter()
        self.t_affinity_done: float | None = None
        self.t_pick_done: float | None = None
        self.t_upstream_start: float | None = None
        self.t_upstream_headers: float | None = None
        self.t_envelope_open: float | None = None
        self.t_first_token: float | None = None
        self.account_id: str | None = None
        self.chain_n = 0
        self.attempt = 0
        self.affinity = False
        self.early_open = False
        self.tools_req = False
        self.held = False
        self.logged = False

    def mark_affinity(self, prefer_account: str | None = None) -> None:
        self.t_affinity_done = time.perf_counter()
        self.affinity = bool(prefer_account)

    def mark_pick(
        self,
        chain: list[Any] | None = None,
        *,
        elapsed_ms: float | int | None = None,
    ) -> None:
        # When pick runs in parallel with body build, pass elapsed_ms so the
        # log reflects true pick cost instead of max(pick, body).
        if elapsed_ms is not None:
            try:
                base = self.t_affinity_done or self.t0
                self.t_pick_done = float(base) + max(0.0, float(elapsed_ms) / 1000.0)
            except Exception:
                self.t_pick_done = time.perf_counter()
        else:
            self.t_pick_done = time.perf_counter()
        try:
            self.chain_n = len(chain or [])
        except Exception:
            self.chain_n = 0

    def mark_upstream_start(self, *, account_id: str | None = None, attempt: int = 0) -> None:
        self.t_upstream_start = time.perf_counter()
        self.account_id = account_id
        self.attempt = int(attempt or 0)

    def mark_upstream_headers(self) -> None:
        if self.t_upstream_headers is None:
            self.t_upstream_headers = time.perf_counter()

    def mark_envelope_open(self, *, early: bool = True) -> None:
        """SSE envelope opened (message_start / response.created / role chunk)."""
        if self.t_envelope_open is None:
            self.t_envelope_open = time.perf_counter()
        if early:
            self.early_open = True

    def mark_tools_requested(self, tools_requested: bool = True) -> None:
        if tools_requested:
            self.tools_req = True

    def mark_held(self) -> None:
        """First model bytes were buffered by tools-preface hold policy."""
        self.held = True

    def mark_first_token(self, *, kind: str = "content", held: bool | None = None) -> None:
        if self.t_first_token is not None:
            return
        self.t_first_token = time.perf_counter()
        if held:
            self.held = True
        self.emit(ok=True, first=kind)

    def latency_ms(self) -> int:
        """Wall-clock ms since request start (best-effort for usage ledger)."""
        try:
            return max(0, int(round((time.perf_counter() - self.t0) * 1000.0)))
        except Exception:
            return 0

    def ttft_ms(self) -> int | None:
        """Client-visible first-token latency (ms from request start), if known."""
        if self.t_first_token is None:
            return None
        try:
            return max(0, int(round((self.t_first_token - self.t0) * 1000.0)))
        except Exception:
            return None

    def timing_snapshot(self) -> dict[str, Any]:
        """Compact timing fields for usage_events.detail / admin 使用明细."""
        out: dict[str, Any] = {}
        try:
            out["latency_ms"] = self.latency_ms()
        except Exception:
            pass
        ttft = self.ttft_ms()
        if ttft is not None:
            out["ttft_ms"] = ttft
        try:
            if self.t_envelope_open is not None:
                out["envelope_ms"] = max(
                    0, int(round((self.t_envelope_open - self.t0) * 1000.0))
                )
        except Exception:
            pass
        try:
            if self.t_upstream_headers is not None and self.t_upstream_start is not None:
                out["up_hdr_ms"] = max(
                    0,
                    int(
                        round(
                            (self.t_upstream_headers - self.t_upstream_start) * 1000.0
                        )
                    ),
                )
        except Exception:
            pass
        try:
            if self.t_upstream_start is not None:
                out["local_ms"] = max(
                    0, int(round((self.t_upstream_start - self.t0) * 1000.0))
                )
        except Exception:
            pass
        if self.early_open:
            out["early"] = True
        if self.tools_req:
            out["tools"] = True
        if self.held:
            out["held"] = True
        return out

    def emit(self, *, ok: bool = True, first: str | None = None, error: str | None = None) -> None:
        if self.logged or not _ttft_log_enabled():
            return
        self.logged = True
        try:
            total = _ms_since(self.t0)
            aff = _ms_since(self.t0) if self.t_affinity_done is None else max(
                0, int(round((self.t_affinity_done - self.t0) * 1000.0))
            )
            # pick cost measured from affinity-done (or t0 if affinity skipped)
            pick_base = self.t_affinity_done or self.t0
            pick = (
                None
                if self.t_pick_done is None
                else max(0, int(round((self.t_pick_done - pick_base) * 1000.0)))
            )
            # local = time until we start upstream request
            local = (
                None
                if self.t_upstream_start is None
                else max(0, int(round((self.t_upstream_start - self.t0) * 1000.0)))
            )
            # upstream TTFB = headers after request start
            up_hdr = (
                None
                if self.t_upstream_headers is None or self.t_upstream_start is None
                else max(
                    0,
                    int(
                        round(
                            (self.t_upstream_headers - self.t_upstream_start) * 1000.0
                        )
                    ),
                )
            )
            # envelope open after headers (0 when early-open at 200)
            env_ms = (
                None
                if self.t_envelope_open is None or self.t_upstream_headers is None
                else max(
                    0,
                    int(
                        round(
                            (self.t_envelope_open - self.t_upstream_headers) * 1000.0
                        )
                    ),
                )
            )
            # first token after headers (model generation)
            up_tok = (
                None
                if self.t_first_token is None or self.t_upstream_headers is None
                else max(
                    0,
                    int(
                        round((self.t_first_token - self.t_upstream_headers) * 1000.0)
                    ),
                )
            )
            ttft = (
                None
                if self.t_first_token is None
                else max(0, int(round((self.t_first_token - self.t0) * 1000.0)))
            )
            parts = [
                f"  [ttft] id={self.req_id}",
                f"proto={self.protocol}",
                f"model={self.model}",
                f"stream={1 if self.stream else 0}",
                f"ok={1 if ok else 0}",
                f"aff={aff if aff is not None else '-'}",
                f"pick={pick if pick is not None else '-'}",
                f"local={local if local is not None else '-'}",
                f"up_hdr={up_hdr if up_hdr is not None else '-'}",
                f"env={env_ms if env_ms is not None else '-'}",
                f"up_tok={up_tok if up_tok is not None else '-'}",
                f"ttft={ttft if ttft is not None else (total if not ok else '-')}",
                f"chain={self.chain_n}",
                f"try={self.attempt}",
                f"sticky={1 if self.affinity else 0}",
                f"early={1 if self.early_open else 0}",
                f"tools={1 if self.tools_req else 0}",
                f"held={1 if self.held else 0}",
                f"acc={_short_account_id(self.account_id)}",
            ]
            if first:
                parts.append(f"first={first}")
            if error:
                parts.append(f"err={str(error)[:160]}")
            print(" ".join(str(p) for p in parts), flush=True)
        except Exception:
            pass


def _record_usage_safe(
    *,
    usage: dict[str, Any] | None = None,
    ok: bool = True,
    api_key_id: str | None = None,
    account_id: str | None = None,
    model: str | None = None,
    protocol: str | None = None,
    stream: bool | None = None,
    path: str | None = None,
    client_ip: str | None = None,
    user_agent: str | None = None,
    status_code: int | None = None,
    latency_ms: int | None = None,
    ttft_ms: int | None = None,
    timing: RequestTiming | None = None,
    error: str | None = None,
    detail: dict[str, Any] | None = None,
) -> None:
    """Best-effort proxy usage ledger; never raises into the chat path.

    When ``timing`` is provided, first-token (ttft_ms) and completion
    (latency_ms) are filled automatically for admin 使用明细.
    """
    try:
        import usage_stats

        ctx = _usage_request_ctx.get() or {}
        # Failed rows with null error/status make admin "断联" debugging opaque
        # (looks like a silent drop). Always stamp a reason + status on !ok.
        err_out = error
        status_out = status_code
        detail_out = dict(detail) if isinstance(detail, dict) else {}
        latency_out = latency_ms
        ttft_out = ttft_ms
        if timing is not None:
            try:
                if latency_out is None:
                    latency_out = timing.latency_ms()
            except Exception:
                pass
            try:
                if ttft_out is None:
                    ttft_out = timing.ttft_ms()
            except Exception:
                pass
            try:
                snap = timing.timing_snapshot()
                # Explicit kwargs win over snapshot.
                if latency_out is not None:
                    snap["latency_ms"] = int(latency_out)
                if ttft_out is not None:
                    snap["ttft_ms"] = int(ttft_out)
                for k, v in snap.items():
                    if k not in detail_out or detail_out.get(k) is None:
                        detail_out[k] = v
            except Exception:
                pass
        elif ttft_out is not None and "ttft_ms" not in detail_out:
            detail_out["ttft_ms"] = int(ttft_out)
        if latency_out is not None and "latency_ms" not in detail_out:
            detail_out["latency_ms"] = int(latency_out)
        if not ok:
            if err_out is None or not str(err_out).strip():
                # Prefer an explicit detail.message over the opaque default.
                if isinstance(detail_out, dict):
                    for key in ("message", "error", "upstream", "reason"):
                        val = detail_out.get(key)
                        if val is not None and str(val).strip():
                            err_out = str(val).strip()[:800]
                            break
                if err_out is None or not str(err_out).strip():
                    err_out = "request_failed"
            else:
                err_out = str(err_out).strip()[:800]
            if status_out is None:
                status_out = 502
            # Empty detail {} is useless for ops; always keep a compact message.
            if not detail_out:
                detail_out = {"message": str(err_out)[:800]}
            elif "message" not in detail_out:
                detail_out["message"] = str(err_out)[:800]
        usage_stats.record_usage(
            usage=usage,
            ok=ok,
            api_key_id=api_key_id,
            account_id=account_id,
            model=model,
            protocol=protocol,
            stream=stream,
            path=path or ctx.get("path"),
            client_ip=client_ip or ctx.get("client_ip"),
            user_agent=user_agent or ctx.get("user_agent"),
            status_code=status_out,
            latency_ms=latency_out,
            ttft_ms=ttft_out,
            error=err_out,
            detail=detail_out or None,
        )
    except Exception:
        pass


def _api_key_id(rec: apikeys.ApiKeyRecord | None) -> str | None:
    if rec is None:
        return None
    kid = getattr(rec, "id", None)
    if not kid:
        return None
    s = str(kid).strip()
    return s or None


class _DisconnectProbe:
    """Debounced client-disconnect probe for long SSE turns.

    Starlette ``request.is_disconnected()`` can flip true once under write
    backpressure or briefly when a proxy probes the socket, then go false
    again. A single hit used to permanently set ``client_gone`` and hard-cut
    mid-turn (Claude Code / sub2api stop scheduling).

    Require several consecutive true hits **and** a minimum wall-clock span so
    a single backpressure stall (many true probes within ~100ms) does not latch.
    Probe exceptions never count.
    """

    def __init__(
        self,
        hits_needed: int | None = None,
        min_span_sec: float | None = None,
    ) -> None:
        raw = hits_needed
        if raw is None:
            try:
                raw = int(os.getenv("GROK2API_DISCONNECT_HITS", "5") or 5)
            except (TypeError, ValueError):
                raw = 5
        self.hits_needed = max(1, min(12, int(raw)))
        span = min_span_sec
        if span is None:
            try:
                span = float(os.getenv("GROK2API_DISCONNECT_SPAN_SEC", "2.5") or 2.5)
            except (TypeError, ValueError):
                span = 2.5
        self.min_span_sec = max(0.0, min(30.0, float(span)))
        self.hits = 0
        self.first_hit_at: float | None = None
        self.gone = False

    async def check(self, probe) -> bool:
        if self.gone:
            return True
        if probe is None:
            return False
        try:
            disconnected = await probe()
        except Exception as e:
            # Local event-loop / transport glitches and backpressure races must
            # never sticky-latch client_gone (that skips terminal frames).
            if _is_local_infra_error(e):
                try:
                    invalidate_http_clients()
                except Exception:
                    pass
            return False
        if disconnected:
            now = time.monotonic()
            if self.first_hit_at is None:
                self.first_hit_at = now
            self.hits += 1
            span_ok = (now - self.first_hit_at) >= self.min_span_sec
            # hits_needed==1 keeps old "immediate" behaviour for tests/debug.
            if self.hits >= self.hits_needed and (
                self.hits_needed <= 1 or span_ok or self.min_span_sec <= 0
            ):
                self.gone = True
                return True
            return False
        self.hits = 0
        self.first_hit_at = None
        return False


async def _aiter_sse_lines_with_keepalive(
    resp: httpx.Response,
    *,
    keepalive_interval: float | None = None,
) -> AsyncIterator[str | None]:
    """
    Yield SSE lines from upstream; yield None on keepalive ticks.

    Secondary relays (sub2api / newapi / nginx) often idle-timeout long
    thinking or tool-prep gaps. None means the caller should emit an SSE
    comment / Anthropic ping so the client-facing stream stays alive.

    Also treats upstream read timeouts / remote protocol drops as a clean
    end (or re-raise once) so the outer generator can finish with a terminal
    frame instead of hard-cutting the TCP stream mid-turn.
    """
    if keepalive_interval is None:
        keepalive_interval = max(2.0, float(SSE_KEEPALIVE_INTERVAL or 4.0))
    # Never wait longer than half the configured idle gap before poking the
    # client; clamp so misconfigured huge values still heart-beat.
    keepalive_interval = max(2.0, min(15.0, float(keepalive_interval)))
    aiter = resp.aiter_lines()
    pending: asyncio.Future[str] | None = asyncio.ensure_future(aiter.__anext__())
    silent_ticks = 0
    max_silent_ticks = int(
        max(30.0, float(os.getenv("GROK2API_SSE_MAX_SILENT_SEC", "300") or 300))
        / keepalive_interval
    )
    try:
        while pending is not None:
            try:
                line = await asyncio.wait_for(
                    asyncio.shield(pending), timeout=keepalive_interval
                )
            except asyncio.TimeoutError:
                silent_ticks += 1
                # Keep poking the client so sub2api/nginx don't idle-close.
                yield None
                if silent_ticks >= max_silent_ticks:
                    # Upstream hung too long with zero bytes — surface as end
                    # so the outer path can emit a clean terminal error/finish
                    # rather than hanging forever.
                    break
                continue
            except StopAsyncIteration:
                break
            except RuntimeError as e:
                # CPython may wrap StopAsyncIteration from __anext__ as RuntimeError
                if "StopAsyncIteration" in str(e):
                    break
                # Dead event loop / transport: end cleanly so outer generators
                # can still flush message_stop / response.completed+[DONE]
                # instead of hard-cutting Claude Code → sub2api mid-turn.
                if _is_local_infra_error(e):
                    try:
                        invalidate_http_clients()
                    except Exception:
                        pass
                    break
                raise
            except (
                httpx.ReadTimeout,
                httpx.ReadError,
                httpx.RemoteProtocolError,
                httpx.TransportError,
            ):
                # Dead / half-closed upstream: end the line iterator cleanly.
                break
            silent_ticks = 0
            yield line
            try:
                pending = asyncio.ensure_future(aiter.__anext__())
            except RuntimeError as e:
                if _is_local_infra_error(e) or "StopAsyncIteration" in str(e):
                    try:
                        invalidate_http_clients()
                    except Exception:
                        pass
                    break
                raise
    finally:
        if pending is not None and not pending.done():
            pending.cancel()
            try:
                await pending
            except (asyncio.CancelledError, StopAsyncIteration, Exception):
                pass


def openai_error(
    message: str,
    status: int = 500,
    err_type: str = "server_error",
    *,
    retry_after: float | int | None = None,
    code: str | int | None = None,
) -> JSONResponse:
    headers = {}
    if retry_after is not None:
        try:
            headers["Retry-After"] = str(max(1, int(float(retry_after))))
        except (TypeError, ValueError):
            headers["Retry-After"] = "5"
    return JSONResponse(
        status_code=status,
        content={
            "error": {
                "message": message,
                "type": err_type,
                "code": code if code is not None else status,
            }
        },
        headers=headers or None,
    )


def _client_pool_error(exc: Exception | str, *, default_status: int = 503) -> JSONResponse:
    """Map pool/auth selection failures to a client-friendly temporary error.

    Light agent/API callers should see 503 + Retry-After (retryable), not 401
    authentication_error which makes sub2api/Claude Code stop scheduling.
    """
    msg = str(exc or "No eligible accounts")
    low = msg.lower()
    retry_after = 5
    if "no live accounts" in low or "auth store" in low:
        status, err_type, code = 503, "service_unavailable", "no_accounts"
        retry_after = 15
    elif "no eligible" in low or "blocked for model" in low or "cooldown" in low:
        status, err_type, code = 503, "service_unavailable", "pool_exhausted"
        retry_after = 8
    elif "expired" in low:
        status, err_type, code = 503, "service_unavailable", "accounts_expired"
        retry_after = 20
    else:
        status, err_type, code = default_status, "service_unavailable", "pool_unavailable"
    # Keep message short & actionable for relays
    friendly = (
        "账号池暂不可用，正在恢复中，请稍后重试。"
        if status == 503
        else msg
    )
    if "model" in low:
        friendly = "当前模型账号暂时繁忙或额度滚动耗尽，请稍后重试。"
    return openai_error(
        friendly + f" ({msg[:160]})" if msg and msg not in friendly else friendly,
        status=status,
        err_type=err_type,
        retry_after=retry_after,
        code=code,
    )


def _sanitize_upstream_error_message(detail: str, status_code: int | None = None) -> str:
    """Short, non-leaky upstream error for clients (full detail stays in logs/pool)."""
    text = (detail or "").strip()
    low = text.lower()
    if "free-usage-exhausted" in low or "free usage" in low:
        return "上游临时额度耗尽，已自动切换账号"
    if status_code == 429 or "rate limit" in low or "too many requests" in low:
        return "上游限流，已自动切换账号"
    if status_code == 401:
        return "上游鉴权失败，已自动切换账号"
    if status_code in (502, 503, 504):
        return "上游暂时不可用，已自动切换账号"
    if status_code == 404 and "model" in low:
        return "上游模型不可用"
    # Collapse JSON blobs
    if text.startswith("{") and len(text) > 180:
        return f"上游错误 HTTP {status_code or '?'}"
    return (text[:220] + "…") if len(text) > 220 else (text or f"上游错误 HTTP {status_code or '?'}")


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
        # Explicit opt-in aliases for think_tag (legacy "on" meant inject into content).
        if self.mode in ("1", "true", "yes", "on"):
            self.mode = "think_tag"
        if self.mode in ("none", ""):
            self.mode = "off"
        # Unknown values fail closed: keep reasoning out of visible content.
        if self.mode not in ("off", "think_tag", "content"):
            self.mode = "off"
        self.think_open = False
        self.saw_reasoning = False

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
            # Close only while a think block is open so alternating
            # reasoning/content streams can reopen and re-close correctly.
            if self.mode == "think_tag" and self.think_open:
                pieces.append("\n</think>\n")
                self.think_open = False
            pieces.append(c)

        out = "".join(pieces) if pieces else None
        # When rewriting into content, suppress separate reasoning_content to
        # avoid double-rendering in new-api playground / Claude UIs.
        return out, None

    def close_tag_chunk(self) -> str | None:
        if self.mode == "think_tag" and self.think_open:
            self.think_open = False
            return "\n</think>\n"
        return None


def _sse_keepalive() -> str:
    """SSE comment keepalive for idle gaps (sub2api/newapi/nginx proxies).

    Some relays ignore pure comments; include a tiny data-less event-looking
    comment that still stays SSE-legal. Pure comments are widely accepted.
    """
    # Dual form: comment + empty data field keeps pickier proxies awake.
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


def _coerce_tool_arguments(raw: Any, *, tool_name: str | None = None) -> str:
    """Normalize tool arguments to the OpenAI streaming string form.

    Prefer alias-aware normalize (path→file_path, oldString→old_string) so
    Update/Edit readiness gates and outbound frames match Claude Code schema.
    """
    try:
        return anth.normalize_tool_arguments_json(raw, tool_name=tool_name)
    except Exception:
        return anth.sanitize_tool_arguments_json(raw)


def _merge_tool_arguments(
    current: str, incoming: str, *, tool_name: str | None = None
) -> str:
    """
    Merge streamed tool argument fragments without double-append corruption.

    OpenAI true deltas are pure suffixes. Secondary relays (sub2api / new-api)
    often re-send the full cumulative JSON on later chunks or on the final
    message; always-append would yield `{"file_path":"a"}{"file_path":"a"}`
    and break Claude Code Read / Write (missing required fields after parse).

    ``tool_name`` lets readiness-aware merge prefer richer Update/Edit payloads
    over early partial objects that only contain file_path.
    """
    return anth.merge_tool_argument_delta(
        current, incoming, tool_name=tool_name
    )



def _iter_tool_sse_chunks(
    *,
    chat_id: str,
    model: str,
    created: int,
    tool_calls: list[Any],
) -> list[str]:
    """One tool_calls[] entry per SSE frame (+ keepalive between tools).

    sub2api's CC→Responses→Anthropic path opens a content_block per tool in the
    same ChatCompletions chunk before closing the previous one. Emitting multiple
    tools in a single delta.tool_calls array therefore produces concurrent open
    blocks and Claude Code: "Content block not found" when later deltas/stops
    target the non-active index.

    Split so each tool is its own SSE event, and insert an SSE comment keepalive
    between tools so converters can close the previous content_block before the
    next tool_use starts (Read is especially sensitive).

    Prefer `_emit_tool_sse_serial` on live streams — it also inserts a real
    wall-clock gap (see GROK2API_OUTBOUND_TOOL_GAP_SEC) because keepalive-only
    bursts still race when sub2api drains a whole TCP window in one tick.
    """
    if not tool_calls:
        return []
    frames: list[str] = []
    first = True
    for tc in tool_calls:
        if not isinstance(tc, dict):
            continue
        if not first:
            frames.append(_sse_keepalive())
        first = False
        frames.append(
            _sse_chunk(
                chat_id=chat_id,
                model=model,
                created=created,
                tool_calls=[tc],
            )
        )
    return frames


async def _yield_anthropic_events_serial(
    events: list[str],
    *,
    client_gone: bool = False,
    protocol: str | None = "anthropic",
) -> AsyncIterator[str]:
    """Yield Anthropic SSE events, inserting a real gap before the next tool_use.

    sub2api keeps one content_block active. When multiple tool_use start/stop
    pairs land in one TCP window, Claude Code hits "Content block not found"
    and stops scheduling further agent turns. Sleep only when the next event
    opens another tool_use (not on text/thinking stops).
    """
    if not events:
        return
    gap = float(
        history_compact.resolve_outbound_tool_gap_sec(
            protocol,
            user_agent=(_usage_request_ctx.get() or {}).get("user_agent"),
        )
        if hasattr(history_compact, "resolve_outbound_tool_gap_sec")
        else (getattr(history_compact, "OUTBOUND_TOOL_GAP_SEC", 0.0) or 0.0)
    )
    n = len(events)
    for i, ev in enumerate(events):
        # Soft-disconnect probes are best-effort and false-positive under
        # backpressure. Dropping mid-stream content_block/tool_use events while
        # still advancing assembler state is what makes Claude Code / sub2api
        # see intermittent hard cuts ("stream interrupted" / open tool_use).
        # Always ship body events; only a failed write proves the client is gone.
        if (
            gap > 0
            and i > 0
            and '"type": "content_block_start"' in ev
            and "tool_use" in ev
        ):
            if not client_gone:
                await asyncio.sleep(gap)
        yield ev


async def _emit_tool_sse_serial(
    *,
    chat_id: str,
    model: str,
    created: int,
    tool_calls: list[Any],
    already_emitted: int = 0,
    max_tools: int | None = None,
    protocol: str | None = None,
) -> AsyncIterator[str]:
    """Yield tool SSE frames one-by-one with keepalive + optional real delay.

    Respects the protocol-aware outbound tool budget. Marks nothing in tool_acc —
    callers must only pass already-built outbound items.
    """
    if not tool_calls:
        return
    budget = history_compact.remaining_outbound_tool_budget(
        already_emitted, max_tools=max_tools, protocol=protocol
    )
    gap = float(
        history_compact.resolve_outbound_tool_gap_sec(
            protocol,
            user_agent=(_usage_request_ctx.get() or {}).get("user_agent"),
        )
        if hasattr(history_compact, "resolve_outbound_tool_gap_sec")
        else (getattr(history_compact, "OUTBOUND_TOOL_GAP_SEC", 0.0) or 0.0)
    )
    first = True
    emitted_here = 0
    for tc in tool_calls:
        if not isinstance(tc, dict):
            continue
        if budget is not None and emitted_here >= budget:
            break
        if not first:
            yield _sse_keepalive()
            if gap > 0:
                # Give sub2api time to close the previous content_block before
                # the next tool_use start (Read is the usual failure case).
                await asyncio.sleep(gap)
        first = False
        yield _sse_chunk(
            chat_id=chat_id,
            model=model,
            created=created,
            tool_calls=[tc],
        )
        emitted_here += 1

def _tool_slot_known(entry: dict[str, Any] | None) -> bool:
    """True when an accumulated tool slot has any identity/payload worth ordering."""
    if not entry:
        return False
    if entry.get("id"):
        return True
    fn = entry.get("function") or {}
    if (fn.get("name") or "").strip():
        return True
    args = fn.get("arguments") or ""
    return bool(str(args).strip())


def _assign_dense_tool_out_index(
    acc: dict[int, dict[str, Any]], entry: dict[str, Any]
) -> int:
    """Map internal OpenAI tool index → dense 0..n-1 outbound index.

    sub2api often binds the first seen tool_calls[].index to content_block 0.
    Sparse upstream indices (only index=1) must not open block 1 with no block 0.
    """
    existing = entry.get("_out_index")
    if isinstance(existing, int):
        return existing
    used = {
        e["_out_index"]
        for e in acc.values()
        if isinstance(e.get("_out_index"), int)
    }
    out_i = 0
    while out_i in used:
        out_i += 1
    entry["_out_index"] = out_i
    return out_i


def _ingest_tool_call_deltas(
    acc: dict[int, dict[str, Any]], deltas: list[Any]
) -> None:
    """Merge upstream tool_call deltas into acc (no outbound emission)."""
    if not deltas:
        return
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
                "_args_sent": 0,
            }
        entry = acc[idx]
        entry.setdefault("_args_sent", 0)
        if raw.get("id") and not entry.get("id"):
            entry["id"] = raw["id"]
        if raw.get("type"):
            entry["type"] = raw["type"]
        fn = raw.get("function") if isinstance(raw.get("function"), dict) else None
        if fn is not None:
            if fn.get("name"):
                entry["function"]["name"] = _merge_tool_name(
                    entry["function"].get("name") or "", str(fn["name"])
                )
            if fn.get("arguments") is not None:
                _tn = entry["function"].get("name") or ""
                entry["function"]["arguments"] = _merge_tool_arguments(
                    entry["function"].get("arguments") or "",
                    _coerce_tool_arguments(fn.get("arguments"), tool_name=_tn),
                    tool_name=_tn,
                )
        else:
            if raw.get("name"):
                entry["function"]["name"] = _merge_tool_name(
                    entry["function"].get("name") or "", str(raw["name"])
                )
            if raw.get("arguments") is not None:
                _tn = entry["function"].get("name") or ""
                entry["function"]["arguments"] = _merge_tool_arguments(
                    entry["function"].get("arguments") or "",
                    _coerce_tool_arguments(raw.get("arguments"), tool_name=_tn),
                    tool_name=_tn,
                )
            elif raw.get("input") is not None:
                _tn = entry["function"].get("name") or ""
                entry["function"]["arguments"] = _merge_tool_arguments(
                    entry["function"].get("arguments") or "",
                    _coerce_tool_arguments(raw.get("input"), tool_name=_tn),
                    tool_name=_tn,
                )


def _outbound_tool_name(name: str | None) -> str:
    """Remap Update/StrReplace → Edit for Claude Code via OpenAI chat path."""
    raw = (name or "").strip()
    if not raw:
        return raw
    try:
        allowed = _allowed_tool_names_ctx.get()
    except Exception:
        allowed = None
    try:
        return anth.canonical_outbound_tool_name(raw, allowed_names=allowed)
    except Exception:
        return raw


def _build_outbound_tool_item(
    acc: dict[int, dict[str, Any]], entry: dict[str, Any], *, remaining: str
) -> dict[str, Any]:
    """Build one complete OpenAI tool_calls[] item and mark it emitted.

    Always include a stable call id on first emission. sub2api keys Anthropic
    content blocks by tool call id; frames without id can later surface as
    Claude Code "Content block not found".

    Also remaps Grok's invented Update/StrReplace name → Edit (Claude Code)
    and alias-normalizes arguments (path→file_path, oldString→old_string).
    """
    out_index = _assign_dense_tool_out_index(acc, entry)
    fn = entry.get("function") if isinstance(entry.get("function"), dict) else {}
    raw_name = (fn.get("name") or "").strip()
    name = _outbound_tool_name(raw_name)
    if name and name != raw_name:
        fn = dict(fn)
        fn["name"] = name
        entry["function"] = fn
    if remaining and str(remaining).strip():
        try:
            remaining = anth.normalize_tool_arguments_json(
                remaining, tool_name=name or raw_name or None
            )
        except Exception:
            pass
    tool_id = (entry.get("id") or "").strip()
    if not tool_id:
        tool_id = f"call_{uuid.uuid4().hex[:24]}"
        entry["id"] = tool_id
    item: dict[str, Any] = {
        "index": out_index,
        "id": tool_id,
        "type": entry.get("type") or "function",
        "function": {"arguments": remaining},
    }
    entry["_id_emitted"] = True
    if name and not entry.get("_name_emitted"):
        item["function"]["name"] = name
        entry["_name_emitted"] = True
    entry["_sent_text"] = remaining
    entry["_args_sent"] = len(remaining)
    entry["_emitted"] = True
    return item


def _tool_call_argument_delta(
    acc: dict[int, dict[str, Any]], deltas: list[Any]
) -> list[dict[str, Any]]:
    """
    Merge tool_call deltas into acc and return sanitized OpenAI-style deltas.

    Critical for Claude Code via sub2api (platform=openai, upstream chat/completions):

    sub2api converts CC → Responses → Anthropic and keeps only one active
    content_block. It also special-cases tool name "Read" by *buffering* args
    and only flushing them on function_call_arguments.done. If we open tool 1
    while tool 0 (Read) is still open, later deltas/stops hit the wrong block
    and Claude Code raises:
        API Error: Content block not found

    Therefore outbound policy is strict:
      1. Hold every tool until name + complete non-empty JSON args are ready
      2. Emit **at most one** complete tool frame per call (atomic id+name+args)
      3. Never open a higher tool while a lower known tool is unfinished
      4. Never stream argument suffixes live — full JSON once, then done

    Intermediate JSON scalars like `"file_path"` and bare `{}` / `[]` stay held.
    """
    _ingest_tool_call_deltas(acc, deltas)

    # Emit at most ONE ready tool per invocation so sub2api can fully
    # start→args→stop that content_block before the next tool opens.
    for idx in sorted(acc.keys()):
        entry = acc[idx]
        if entry.get("_emitted"):
            continue
        args = entry.get("function", {}).get("arguments") or ""
        name = (entry.get("function", {}).get("name") or "").strip()

        # Never overtake a lower known unfinished tool (including sparse holes).
        blocked = False
        for lower in range(0, idx):
            low = acc.get(lower)
            if low is None:
                continue
            if low.get("_emitted"):
                continue
            if _tool_slot_known(low):
                blocked = True
                break
        if blocked:
            break

        if not name:
            # Name/id known but args incomplete — hold (do not open block early).
            # Critical for Update/Edit: {"file_path":"..."} alone is NOT ready.
            if entry.get("id") or str(args).strip():
                break
            continue
        # Alias-normalize before readiness (path→file_path, oldString→old_string).
        args = _coerce_tool_arguments(args, tool_name=name)
        entry.setdefault("function", {})["arguments"] = args
        if not args or not anth.is_complete_tool_arguments_json(
            args, tool_name=name
        ):
            # Name/id known but args incomplete — hold (do not open block early).
            # Critical for Update/Edit: {"file_path":"..."} alone is NOT ready.
            break

        return [_build_outbound_tool_item(acc, entry, remaining=str(args))]
    return []


def _flush_one_tool_call(acc: dict[int, dict[str, Any]]) -> list[dict[str, Any]]:
    """Flush at most one still-held tool (terminal / non-SSE safe for sub2api).

    Must not mark sibling tools as emitted — callers loop until empty.

    Prefer complete required-key payloads. Incomplete Update/Edit objects (only
    file_path) are *not* shipped at terminal: better empty-turn failover than a
    client-side wrong edit. Truncated non-JSON still ships once as best-effort
    only when no required-key schema is known.
    """
    for idx in sorted(acc.keys()):
        entry = acc[idx]
        if entry.get("_emitted"):
            continue
        fn = entry.get("function") or {}
        raw_name = (fn.get("name") or "").strip()
        name = _outbound_tool_name(raw_name)
        if name and name != raw_name:
            entry.setdefault("function", {})["name"] = name
            fn = entry.get("function") or fn
        args = fn.get("arguments") or ""
        args = _coerce_tool_arguments(args, tool_name=name or None)
        entry.setdefault("function", {})["arguments"] = args
        # Drop fully empty ghost slots.
        if not name and not entry.get("id") and not str(args).strip():
            continue
        if not name:
            # Cannot open a useful tool_use without a name.
            continue
        remaining = str(args) if str(args).strip() else "{}"
        if not anth.is_complete_tool_arguments_json(remaining, tool_name=name):
            # Known schema incomplete (e.g. Update missing old/new) — drop rather
            # than force a broken tool call that Claude Code will "run" wrong.
            required = ()
            try:
                required = anth._required_keys_for_tool(name)  # type: ignore[attr-defined]
            except Exception:
                required = ()
            if required:
                continue
            remaining = str(args) if str(args).strip() else "{}"
        return [_build_outbound_tool_item(acc, entry, remaining=remaining)]
    return []


def _flush_tool_call_argument_deltas(
    acc: dict[int, dict[str, Any]],
    *,
    max_tools: int | None = None,
    protocol: str | None = None,
) -> list[dict[str, Any]]:
    """Flush still-held tools as a list (one complete JSON blob per tool).

    Prefer _flush_one_tool_call + loop on the SSE path so sub2api never sees a
    multi-tool burst without per-tool framing. This helper remains for bulk
    collection; the protocol-aware tool cap can still trim the returned list
    without marking unreturned siblings as emitted.
    """
    out: list[dict[str, Any]] = []
    while True:
        one = _flush_one_tool_call(acc)
        if not one:
            break
        out.extend(one)
    capped = history_compact.cap_outbound_tools(
        out, max_tools=max_tools, protocol=protocol
    )
    if capped is None:
        return out
    if len(capped) < len(out):
        # Un-mark tools that the safety valve dropped so a later flush can ship them
        # if the operator raises the cap mid-process (best-effort).
        kept_ids = {x.get("id") for x in capped if isinstance(x, dict)}
        for entry in acc.values():
            if entry.get("_emitted") and entry.get("id") not in kept_ids:
                # Only unmark if this emission was part of this bulk flush's tail.
                # Safer: leave marked to avoid double-send. Cap is intentional drop.
                pass
    return capped if capped is not None else out


def _merge_tool_name(current: str, incoming: str) -> str:
    """
    Merge function names from streamed deltas without double-append corruption.

    OpenAI usually sends the full name once. Some proxies re-send the full name
    on later chunks; always-append would produce `web_searchweb_search` and break
    tool dispatch intermittently.

    After merging, remap Update/StrReplace → Edit so the OpenAI chat path never
    holds Grok's invented edit-tool name for Claude Code via sub2api.
    """
    cur = (current or "").strip()
    name = (incoming or "").strip()
    if not name:
        merged = cur
    elif not cur:
        merged = name
    elif name == cur:
        merged = cur
    elif name.startswith(cur):
        # progressive expansion (rare) or full name after prefix
        merged = name
    elif cur.startswith(name):
        # ignore shorter re-send / fragment
        merged = cur
    else:
        # Different name on same index — prefer the newer complete token
        merged = name
    return _outbound_tool_name(merged) if merged else merged


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
                "arguments": _coerce_tool_arguments(
                    args, tool_name=str(name or "") or None
                ),
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
                    _tn = str(fn.get("name") or item.get("name") or "") or None
                    fn["arguments"] = _coerce_tool_arguments(
                        fn.get("arguments"), tool_name=_tn
                    )
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
    _tool_call_argument_delta(acc, deltas)


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
        tool_id = entry.get("id") or f"call_{uuid.uuid4().hex[:24]}"
        args = fn.get("arguments")
        name = _outbound_tool_name((fn.get("name") or "").strip())
        if args is None:
            args = ""
        else:
            args = _coerce_tool_arguments(args, tool_name=name or None)
        if isinstance(args, str) and not args.strip():
            args = "{}"
        if not name:
            continue
        # Skip schema-incomplete known tools (Update without old/new, etc.).
        if not anth.is_complete_tool_arguments_json(str(args), tool_name=name):
            try:
                required = anth._required_keys_for_tool(name)  # type: ignore[attr-defined]
            except Exception:
                required = ()
            if required:
                continue
        out.append(
            {
                "id": tool_id,
                "type": entry.get("type") or "function",
                "function": {"name": name, "arguments": args},
            }
        )
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
    """Bounded readiness probe — never triggers OIDC refresh or full account dump."""
    reg: dict[str, Any] = {"available": False}
    try:
        import grok_build_adapter as _reg

        reg = _reg.registration_available()
    except Exception as e:  # noqa: BLE001
        reg = {"available": False, "error": str(e)}
    store_info: dict[str, Any] = {}
    leader_info: dict[str, Any] = {}
    try:
        from store import store_status

        store_info = store_status()
    except Exception as e:  # noqa: BLE001
        store_info = {"error": str(e)}
    try:
        from store.leader import status as leader_status

        leader_info = leader_status()
    except Exception as e:  # noqa: BLE001
        leader_info = {"error": str(e)}
    try:
        # Counts only; omit the hundreds-of-accounts payload.
        # Offload sync store IO so health never blocks the event loop.
        pool = await asyncio.to_thread(
            account_pool.pool_summary, include_accounts=False
        )
        # Health must stay a bounded read-only route. Do not make an OIDC
        # refresh request while resolving the representative account.
        creds = await asyncio.to_thread(account_pool.acquire, auto_refresh=False)
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
            # light=True avoids rescanning auth.json for min_remaining on every poll
            "token_maintainer": token_maintainer.status(light=True),
            "model_health": __import__("model_health").status(light=True),
            "conversation_affinity": conversation_affinity.status(),
            "registration": reg,
            "store": store_info,
            "maintainer_leader": leader_info,
        }
    except AuthError as e:
        return JSONResponse(
            status_code=503,
            content={
                "status": "auth_error",
                "message": str(e),
                "version": APP_VERSION,
                "registration": reg,
                "store": store_info,
                "maintainer_leader": leader_info,
            },
        )


@app.get("/metrics")
async def metrics():
    """Prometheus text exposition (in-process counters + store gauges)."""
    from fastapi.responses import PlainTextResponse

    try:
        from store.metrics import prometheus_text

        body = prometheus_text()
    except Exception as e:  # noqa: BLE001
        body = f"# error {e}\n"
    return PlainTextResponse(body, media_type="text/plain; version=0.0.4")


def _admin_page(name: str = "index"):
    """Serve multi-page admin HTML from static/admin/{name}.html."""
    # allow only known pages
    allowed = {
        "index": "index.html",
        "overview": "index.html",
        "login": "login.html",
        "keys": "keys.html",
        "accounts": "accounts.html",
        "models": "models.html",
        "guide": "guide.html",
        "settings": "settings.html",
        "logs": "logs.html",
        "usage": "usage.html",
    }
    filename = allowed.get((name or "index").strip().lower())
    if not filename:
        return None
    admin_file = STATIC_DIR / "admin" / filename
    if not admin_file.is_file():
        # fallback to legacy single-file console
        legacy = STATIC_DIR / "index.html"
        if legacy.is_file() and name in ("index", "overview", "login"):
            admin_file = legacy
        else:
            return None
    return FileResponse(
        admin_file,
        media_type="text/html; charset=utf-8",
        headers={
            "Cache-Control": "no-store, no-cache, must-revalidate, max-age=0",
            "Pragma": "no-cache",
            "Expires": "0",
        },
    )


def _admin_html_response():
    """Backward-compatible alias → overview page."""
    return _admin_page("index")


@app.get("/favicon.ico", include_in_schema=False)
async def favicon():
    icon = STATIC_DIR / "favicon.ico"
    if icon.is_file() and icon.stat().st_size > 0:
        return FileResponse(icon, media_type="image/x-icon")
    return JSONResponse({"detail": "not found"}, status_code=404)


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
            "POST /v1/responses",
            "POST /v1/messages",
            "POST /v1/messages/count_tokens",
            "Admin /admin",
        ],
        "hint": (
            "OpenAI base_url → <your-host>/v1 · "
            "Anthropic base_url → <your-host> (or /v1). "
            "Responses API (sub2api) → <your-host>/v1/responses. "
            "Use the same host/port you open in the browser; "
            "set GROK2API_PUBLIC_BASE_URL if behind reverse proxy."
        ),
    }


def _admin_or_404(name: str):
    html = _admin_page(name)
    if html is None:
        return JSONResponse(
            status_code=404,
            content={
                "error": f"Admin UI page not found: {name}. Missing static/admin/*.html"
            },
        )
    return html


@app.get("/admin")
@app.get("/admin/")
async def admin_overview_page():
    return _admin_or_404("index")


@app.get("/admin/login")
@app.get("/admin/login/")
async def admin_login_page():
    return _admin_or_404("login")


@app.get("/admin/keys")
@app.get("/admin/keys/")
async def admin_keys_page():
    return _admin_or_404("keys")


@app.get("/admin/accounts")
@app.get("/admin/accounts/")
async def admin_accounts_page():
    return _admin_or_404("accounts")


@app.get("/admin/logs")
@app.get("/admin/logs/")
async def admin_logs_page():
    return _admin_or_404("logs")


@app.get("/admin/usage")
@app.get("/admin/usage/")
async def admin_usage_page():
    return _admin_or_404("usage")


@app.get("/admin/models")
@app.get("/admin/models/")
async def admin_models_page():
    return _admin_or_404("models")


@app.get("/admin/guide")
@app.get("/admin/guide/")
async def admin_guide_page():
    return _admin_or_404("guide")


@app.get("/admin/settings")
@app.get("/admin/settings/")
async def admin_settings_page():
    return _admin_or_404("settings")


@app.get("/v1/models", dependencies=[Depends(require_api_key)])
@app.get("/models", dependencies=[Depends(require_api_key)])
async def list_models():
    return {"object": "list", "data": load_models_from_cache()}


def _retryable_status(code: int) -> bool:
    return code in (401, 403, 429, 500, 502, 503, 504)


def _is_empty_model_payload(
    *,
    content: str | None = None,
    reasoning: str | None = None,
    tool_calls: list[Any] | None = None,
    saw_tool: bool = False,
) -> bool:
    """True when upstream produced no usable model output.

    Secondary relays (Claude Code / sub2api) surface this as
    ``API returned an empty or malformed response (HTTP 200)``.
    """
    if saw_tool:
        return False
    if tool_calls:
        return False
    if (content or "").strip():
        return False
    if (reasoning or "").strip():
        return False
    return True


def _looks_like_gateway_intercept(raw: bytes | str | None) -> bool:
    """Detect HTML / WAF / proxy interstitial bodies that arrive as HTTP 200."""
    if raw is None:
        return False
    if isinstance(raw, bytes):
        sample = raw[:800].decode("utf-8", errors="replace")
    else:
        sample = str(raw)[:800]
    s = sample.lstrip().lower()
    if not s:
        return False
    if s.startswith("<!doctype html") or s.startswith("<html"):
        return True
    markers = (
        "cloudflare",
        "cf-ray",
        "attention required",
        "access denied",
        "just a moment",
        "enable javascript",
        "proxy authentication required",
        "squid error",
        "502 bad gateway",
        "503 service unavailable",
        "504 gateway timeout",
        "error code 5",
        "nginx/",
        "varnish cache server",
    )
    return any(m in s for m in markers)


def _classify_upstream_body_error(
    raw: bytes | str | None,
    *,
    content_type: str | None = None,
) -> str | None:
    """Return a retryable error message when body is empty / HTML / non-JSON."""
    if raw is None:
        return "Upstream returned HTTP 200 with empty body (no model output)"
    if isinstance(raw, bytes):
        if not raw or not raw.strip():
            return "Upstream returned HTTP 200 with empty body (no model output)"
        text = raw.decode("utf-8", errors="replace")
    else:
        text = str(raw)
        if not text.strip():
            return "Upstream returned HTTP 200 with empty body (no model output)"
    ctype = (content_type or "").lower()
    if "text/html" in ctype or "application/xhtml" in ctype:
        return (
            "Upstream returned HTTP 200 HTML (proxy/gateway intercept?) — "
            "not a model response"
        )
    if _looks_like_gateway_intercept(text):
        preview = text.strip().replace("\n", " ")[:160]
        return (
            "Upstream returned HTTP 200 non-JSON body "
            f"(proxy/gateway intercept?): {preview!r}"
        )
    return None


def _resolve_conversation_affinity(
    req: ChatCompletionRequest,
    request: Request,
    *,
    api_key_id: str | None = None,
    body: dict[str, Any] | None = None,
) -> tuple[str | None, str | None, str | None, bool]:
    """
    Returns (fingerprint, preferred_account_id, prompt_cache_key, pck_minted).
    Same multi-turn chat → same fingerprint → sticky account
    (pool rotation will not switch accounts mid-conversation).

    Prefer prompt_cache_key when present so OpenAI/sub2api multi-turn stays on
    one account for prompt-cache locality. When the client omits it, mint one
    so subsequent turns (or response echoes) can stick.
    """
    conv_id = conversation_affinity.extract_conversation_id_from_headers(
        request.headers
    ) or conversation_affinity.extract_conversation_id_from_body(req)
    pck, minted = _ensure_prompt_cache_key(
        body=body,
        req=req,
        headers=request.headers,
        api_key_id=api_key_id,
        conversation_id=conv_id,
        user=getattr(req, "user", None),
    )
    fp = conversation_affinity.conversation_fingerprint(
        req.messages,
        user=req.user,
        conversation_id=conv_id,
        api_key_id=api_key_id,
        prompt_cache_key=pck,
    )
    prefer = conversation_affinity.get_affinity(fp) if fp else None
    return fp, prefer, pck, minted


def _pick_account_chain(
    *,
    model: str,
    prefer_account_id: str | None = None,
) -> list[GrokCredentials]:
    """Build failover chain for one request (runs in a worker thread)."""
    chain = account_pool.try_acquire_sequence(
        model=model, prefer_account_id=prefer_account_id
    )
    if not chain:
        chain = [account_pool.acquire(model=model)]
    return chain


def _pick_account_chain_timed(
    *,
    model: str,
    prefer_account_id: str | None = None,
) -> tuple[list[GrokCredentials], float]:
    """Same as _pick_account_chain but also returns elapsed milliseconds."""
    t0 = time.perf_counter()
    chain = _pick_account_chain(model=model, prefer_account_id=prefer_account_id)
    return chain, max(0.0, (time.perf_counter() - t0) * 1000.0)


def _note_request_metrics(
    *,
    prefer_account: str | None,
    conv_fp: str | None,
) -> None:
    try:
        from store.metrics import inc

        inc("g2a_requests_total")
        if prefer_account:
            inc("g2a_affinity_hits_total")
        elif conv_fp:
            inc("g2a_affinity_misses_total")
    except Exception:
        pass


@app.post("/v1/chat/completions")
@app.post("/chat/completions")
async def chat_completions(
    req: ChatCompletionRequest,
    request: Request,
    api_key: apikeys.ApiKeyRecord | None = Depends(require_api_key),
):
    if not req.messages:
        return openai_error(
            "messages is required", status=400, err_type="invalid_request_error"
        )

    key_id = _api_key_id(api_key)
    timing = RequestTiming(protocol="openai", stream=bool(req.stream))
    conv_fp, prefer_account, pck, pck_minted = await asyncio.to_thread(
        _resolve_conversation_affinity, req, request, api_key_id=key_id
    )
    timing.mark_affinity(prefer_account)
    model = resolve_model(req.model)
    timing.model = model

    # Overlap account pick with body sanitize/compact so local TTFT is
    # max(pick, body) instead of pick + body on long tool histories.
    try:
        (chain, pick_ms), body = await asyncio.gather(
            asyncio.to_thread(
                _pick_account_chain_timed,
                model=model,
                prefer_account_id=prefer_account,
            ),
            asyncio.to_thread(build_upstream_body, req, model),
        )
        timing.mark_pick(chain, elapsed_ms=pick_ms)
    except AuthError as e:
        try:
            from store.metrics import inc

            inc("g2a_auth_failures_total")
        except Exception:
            pass
        timing.emit(ok=False, error=str(e))
        return _client_pool_error(e)

    # Stamp auto/minted prompt_cache_key onto body for response echo (stripped
    # before upstream by _sanitize_upstream_body / _body_for_upstream).
    if isinstance(body, dict) and pck:
        body["prompt_cache_key"] = pck
        body["_prompt_cache_key"] = pck
        body["_prompt_cache_key_minted"] = bool(pck_minted)

    # Bind client-registered tools so OpenAI chat outbound remaps
    # Update/StrReplace → Edit for Claude Code via sub2api.
    try:
        _allowed_tool_names_ctx.set(
            anth.extract_tool_names(body.get("tools") if isinstance(body, dict) else None)
            or set()
        )
    except Exception:
        try:
            _allowed_tool_names_ctx.set(set())
        except Exception:
            pass

    _note_request_metrics(prefer_account=prefer_account, conv_fp=conv_fp)

    url = f"{UPSTREAM_BASE}/chat/completions"
    chat_id = f"chatcmpl-{uuid.uuid4().hex[:24]}"
    timing.req_id = chat_id.replace("chatcmpl-", "")[:12]
    created = int(time.time())

    compact_hdr = {
        **_history_compact_headers(body),
        **_prompt_stabilize_headers(body),
        **_prompt_cache_key_headers(body, prompt_cache_key=pck, minted=pck_minted),
    }
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
                api_key_id=key_id,
                usage_ctx=_capture_usage_request_ctx(request),
                timing=timing,
            ),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache, no-transform",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
                # Hint proxies / sub2api not to buffer SSE frames.
                "Content-Type": "text/event-stream; charset=utf-8",
                "X-Grok2API-Accounts": str(len(chain)),
                "X-Grok2API-Affinity": "1" if prefer_account else "0",
                **compact_hdr,
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

    for attempt_i, creds in enumerate(chain):
        headers = upstream_headers(creds.token, model)
        try:
            timing.mark_upstream_start(account_id=creds.auth_key, attempt=attempt_i)
            content, reasoning, finish, usage, tool_calls = await _collect_completion(
                url=url, headers=headers, body=body, account_id=creds.auth_key
            )
            timing.mark_upstream_headers()
            timing.mark_first_token(kind="content" if content else ("tool" if tool_calls else "done"))
            await asyncio.to_thread(account_pool.report_success, creds.auth_key, model=model)
            used = creds
            # Keep multi-turn memory on this account; rebind if failover
            if conv_fp:
                if prefer_account and prefer_account != creds.auth_key:
                    await asyncio.to_thread(
                        conversation_affinity.rebind_on_failover,
                        conv_fp,
                        first_tried,
                        creds.auth_key,
                    )
                    try:
                        from store.metrics import inc

                        inc("g2a_account_failovers_total")
                    except Exception:
                        pass
                else:
                    await asyncio.to_thread(
                        conversation_affinity.bind_affinity, conv_fp, creds.auth_key
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
            _record_usage_safe(
                usage=result["usage"],
                ok=True,
                api_key_id=key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="openai",
                stream=False,
                timing=timing,
            )
            # non-standard but useful for multi-account / cache debugging
            result["x_grok2api_account"] = creds.email or creds.auth_key
            _attach_cache_debug_fields(
                result,
                result.get("usage"),
                prefer_account=bool(prefer_account),
                body=body,
                prompt_cache_key=pck,
                prompt_cache_key_minted=pck_minted,
            )
            hc_stats = body.get("_history_compact") if isinstance(body, dict) else None
            if isinstance(hc_stats, dict):
                result["x_grok2api_history_compact"] = hc_stats
            if conv_fp:
                result["x_grok2api_conversation_fp"] = conv_fp
            timing.emit(ok=True)
            return _json_result_with_cache_headers(
                result,
                body=body,
                prefer_account=bool(prefer_account),
                conv_fp=conv_fp,
                prompt_cache_key=pck,
            )
        except httpx.HTTPStatusError as e:
            code = e.response.status_code if e.response is not None else 502
            detail = e.response.text[:800] if e.response is not None else str(e)
            hdrs = dict(e.response.headers) if e.response is not None else None
            _report_upstream_failure(
                creds.auth_key,
                error=detail,
                status_code=code,
                model=model,
                headers=hdrs,
            )
            try:
                from store.metrics import inc

                inc("g2a_upstream_failures_total")
            except Exception:
                pass
            _record_usage_safe(
                ok=False,
                api_key_id=key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="openai",
                stream=False,
                status_code=code,
                error=f"Upstream {code}: {detail}",
                detail={
                    "message": f"Upstream {code}: {detail}",
                    "upstream_status": code,
                    "upstream_body": detail,
                },
                timing=timing,
            )
            last_error = f"Upstream {code}: {detail}"
            last_status = code
            if not _retryable_status(code):
                break
            continue
        except Exception as e:  # noqa: BLE001
            _report_upstream_failure(
                            creds.auth_key,
                            error=str(e),
                            status_code=502,
                            model=model,
                        )
            try:
                from store.metrics import inc

                inc("g2a_upstream_failures_total")
            except Exception:
                pass
            _record_usage_safe(
                ok=False,
                api_key_id=key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="openai",
                stream=False,
                status_code=502,
                error=f"Proxy error: {e}",
                detail={"message": f"Proxy error: {e}", "exc_type": type(e).__name__},
                timing=timing,
            )
            last_error = f"Proxy error: {e}"
            last_status = 502
            continue

    # All accounts in chain failed — tell clients this is temporary/retryable when
    # the last status was a known transient upstream code.
    final_status = last_status if last_status < 600 else 502
    retryable = final_status in (401, 403, 429, 500, 502, 503, 504)
    friendly = _sanitize_upstream_error_message(last_error or "", final_status)
    timing.emit(ok=False, error=friendly or last_error or "all_accounts_failed")
    if retryable:
        return openai_error(
            friendly or "所有账号暂时失败，请稍后重试",
            status=503,
            err_type="upstream_error",
            retry_after=8,
            code="all_accounts_failed",
        )
    return openai_error(
        friendly or last_error or "All accounts failed",
        status=final_status,
        err_type="upstream_error",
        code=final_status,
    )



def _body_requests_tools(body: dict[str, Any] | None) -> bool:
    """True when this turn may produce tool_calls (hold pre-tool text/reasoning).

    Claude Code via sub2api usually sends tools[]. Also treat tool_choice /
    functions / any non-none choice as tools-mode so reasoning cannot open
    content_block 0 before tool index 0.
    """
    if not isinstance(body, dict):
        return False
    if body.get("tools") or body.get("functions"):
        return True
    tc = body.get("tool_choice")
    if tc is None:
        return False
    if isinstance(tc, str):
        return tc.strip().lower() not in ("", "none")
    if isinstance(tc, dict):
        return True
    return bool(tc)


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
    api_key_id: str | None = None,
    usage_ctx: dict[str, Any] | None = None,
    timing: RequestTiming | None = None,
) -> AsyncIterator[str]:
    # Do NOT emit a premature role chunk before upstream accepts — secondary
    # relays treat early chunks as stream-started and cannot safely failover.
    _usage_tok = _bind_usage_request_ctx(usage_ctx)
    try:
        async for chunk in _stream_proxy_with_failover_inner(
            url=url,
            body=body,
            chain=chain,
            chat_id=chat_id,
            model=model,
            created=created,
            client_disconnected=client_disconnected,
            conversation_fp=conversation_fp,
            api_key_id=api_key_id,
            timing=timing,
        ):
            yield chunk
    finally:
        _reset_usage_request_ctx(_usage_tok)


async def _stream_proxy_with_failover_inner(
    *,
    url: str,
    body: dict[str, Any],
    chain: list[GrokCredentials],
    chat_id: str,
    model: str,
    created: int,
    client_disconnected,
    conversation_fp: str | None = None,
    api_key_id: str | None = None,
    timing: RequestTiming | None = None,
    protocol: str = "openai",
) -> AsyncIterator[str]:
    last_err: str | None = None
    first_tried = chain[0].auth_key if chain else None
    role_sent = False
    # When the client request includes tools, Grok often streams a long
    # reasoning_content preface before tool_calls. sub2api/Claude Code convert
    # that preface into content_block 0 (thinking/text), then map OpenAI
    # tool_calls[index=0] onto the same block index →
    # "apiError: Content block not found". Hold pre-tool text/reasoning and
    # drop it from the outbound stream once tool frames are actually emitted
    # (still counted in usage). Incomplete tool previews must not release the
    # hold or open a text block before the first tool frame.
    tools_requested = _body_requests_tools(body)
    if timing is not None:
        timing.mark_tools_requested(tools_requested)
    # Pure OpenAI chat / Codex-native Responses: unlimited tools.
    # Claude Code via sub2api stays capped at 1 (content_block race).
    _ua = None
    try:
        _ua = (_usage_request_ctx.get() or {}).get("user_agent")
    except Exception:
        _ua = None
    outbound_max_tools = history_compact.resolve_outbound_max_tools(
        protocol, user_agent=_ua
    )

    for idx, creds in enumerate(chain):
        headers = upstream_headers(creds.token, model)
        finished = False
        saw_tool_calls = False  # True only after a tool frame is outbound
        tools_pending = False  # upstream tool deltas seen but not yet emitted
        held_finish: str | None = None
        stream_started = False  # True once any content has been sent to client
        client_gone = False
        disconnect = _DisconnectProbe()
        usage: dict[str, Any] | None = None
        content_parts: list[str] = []
        reasoning_parts: list[str] = []
        tool_acc: dict[int, dict[str, Any]] = {}
        tools_emitted_count = 0  # enforces outbound tool cap across the turn
        reasoning_compat = _ReasoningCompatState()
        # Buffered pre-tool emissions (OpenAI path only). Flushed only if the
        # turn ends without any outbound tool frames.
        held_pre_tool: list[tuple[str | None, str | None]] = []
        try:
            if timing is not None:
                timing.mark_upstream_start(account_id=creds.auth_key, attempt=idx)
            upstream_body = _body_for_upstream(body)
            client = await get_http_client(creds.auth_key)
            async with client.stream(
                "POST", url, headers=headers, json=upstream_body
            ) as resp:
                if timing is not None:
                    timing.mark_upstream_headers()
                if resp.status_code >= 400:
                    err_text = (await resp.aread()).decode(
                        "utf-8", errors="replace"
                    )[:1500]
                    _report_upstream_failure(
                creds.auth_key,
                error=err_text,
                status_code=resp.status_code,
                model=model,
                headers=dict(resp.headers,
            ),
                    )
                    _record_usage_safe(
                        ok=False,
                        api_key_id=api_key_id,
                        account_id=creds.auth_key,
                        model=model,
                        protocol="openai",
                        stream=True,
                        status_code=resp.status_code,
                        latency_ms=timing.latency_ms() if timing is not None else None,
                        error=f"Upstream {resp.status_code}: {err_text}",
                        detail={
                            "message": f"Upstream {resp.status_code}: {err_text}",
                            "upstream_status": resp.status_code,
                            "upstream_body": err_text,
                        },
                        timing=timing,
                    )
                    last_err = f"Upstream {resp.status_code}: {err_text}"
                    # try next account if retryable and more remain
                    if _retryable_status(resp.status_code) and idx < len(chain) - 1:
                        continue
                    if timing is not None:
                        timing.emit(ok=False, error=last_err)
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

                # Defer success/affinity until after first client bytes.
                # These hit PG/Redis and used to block TTFT on every stream.
                # NOTE: empty HTTP 200 bodies are detected after drain and
                # treated as retryable before we permanently commit success.
                success_noted = False

                def _note_success_once() -> None:
                    nonlocal success_noted
                    if success_noted:
                        return
                    success_noted = True
                    asyncio.create_task(
                        asyncio.to_thread(
                            account_pool.report_success, creds.auth_key, model=model
                        )
                    )
                    if conversation_fp:
                        if idx > 0:
                            asyncio.create_task(
                                asyncio.to_thread(
                                    conversation_affinity.rebind_on_failover,
                                    conversation_fp,
                                    first_tried,
                                    creds.auth_key,
                                )
                            )
                        else:
                            asyncio.create_task(
                                asyncio.to_thread(
                                    conversation_affinity.bind_affinity,
                                    conversation_fp,
                                    creds.auth_key,
                                )
                            )

                if not role_sent:
                    # Role-only delta (no empty content) — required for new-api playground.
                    # Early envelope: clients see first SSE bytes at upstream 200.
                    # Role alone is not counted as first token; content/tool is.
                    yield _sse_chunk(
                        chat_id=chat_id,
                        model=model,
                        created=created,
                        role="assistant",
                    )
                    role_sent = True
                    if timing is not None:
                        timing.mark_envelope_open(early=True)

                ctype = (resp.headers.get("content-type") or "").lower()
                if "text/event-stream" in ctype or "stream" in ctype:
                    async for line in _aiter_sse_lines_with_keepalive(resp):
                        # Soft disconnect check: keep draining so we can still
                        # emit a terminal finish/tool_calls frame when possible.
                        # Debounced: a single is_disconnected() blip under
                        # backpressure must not sticky-latch client_gone.
                        client_gone = await disconnect.check(client_disconnected)
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
                        emit_tool_calls: list[Any] | None = None
                        if tool_calls:
                            tools_pending = True
                            # Upstream produced tools even if request tools[]
                            # was stripped — force tools-mode hold/suppress.
                            tools_requested = True
                            if timing is not None:
                                timing.mark_tools_requested(True)
                            # At most ONE complete tool per upstream SSE line.
                            # Burst-draining every ready tool still races
                            # sub2api's single active content_block (esp. Read).
                            # Remaining tools emit on later ticks / terminal
                            # flush, still one frame at a time.
                            budget = history_compact.remaining_outbound_tool_budget(
                                tools_emitted_count,
                                max_tools=outbound_max_tools,
                                protocol=protocol,
                            )
                            if budget is not None and budget <= 0:
                                # Cap reached: still ingest for accounting, no emit.
                                _ingest_tool_call_deltas(tool_acc, tool_calls)
                                emit_tool_calls = None
                            else:
                                emit_tool_calls = (
                                    _tool_call_argument_delta(tool_acc, tool_calls)
                                    or None
                                )
                                if (
                                    emit_tool_calls
                                    and budget is not None
                                    and len(emit_tool_calls) > budget
                                ):
                                    emit_tool_calls = emit_tool_calls[:budget]
                            # Only when a tool frame is actually outbound do
                            # tools "win" the turn. Incomplete name-only /
                            # partial-arg previews must keep holding preface.
                            if emit_tool_calls:
                                saw_tool_calls = True
                                if tools_requested and held_pre_tool:
                                    held_pre_tool.clear()
                                    reasoning_compat.think_open = False

                        emit_content, emit_reasoning = reasoning_compat.rewrite(
                            content if content else None,
                            reasoning if reasoning else None,
                        )

                        # Tools-requested turns: never interleave reasoning /
                        # content with tool_calls on the wire.
                        # - Hold preface until we know the turn is non-tool
                        # - Keep holding while tools are pending incomplete
                        # - Once tools are emitted, suppress ALL further
                        #   content/reasoning (sub2api maps text before tool
                        #   to content_block 0 and then fails on tool index 0)
                        if tools_requested and not saw_tool_calls:
                            # Still buffering (no outbound tool frame yet),
                            # whether or not incomplete tool previews arrived.
                            # Keep holding so a non-tool finish can flush text.
                            if emit_content or emit_reasoning:
                                held_pre_tool.append(
                                    (emit_content, emit_reasoning)
                                )
                                if timing is not None:
                                    timing.mark_held()
                            emit_content, emit_reasoning = None, None
                        elif tools_requested and saw_tool_calls:
                            # Tools already on the wire: drop all further
                            # text/reasoning (avoids content_block clashes).
                            emit_content, emit_reasoning = None, None

                        # Incomplete tool-name previews must NOT set stream_started or bind
                        # success/affinity. Only outbound frames that are actually
                        # yielded count as client-visible model bytes — do not
                        # mark started when client_gone skips the yield below.
                        if finish:
                            # Hold finish until stream drain so we can attach
                            # usage on the same terminal chunk. sub2api/new-api
                            # typically read usage from the finish_reason frame
                            # and ignore a later usage-only chunk.
                            finished = True
                            held_finish = finish
                            # Close <think> before terminal finish if still open.
                            # Skip while a tools-preface is still held — we may
                            # flush that preface below without an early close.
                            if (
                                not client_gone
                                and not (
                                    tools_requested
                                    and not saw_tool_calls
                                    and held_pre_tool
                                )
                                and not (tools_requested and saw_tool_calls)
                            ):
                                close_tag = reasoning_compat.close_tag_chunk()
                                if close_tag:
                                    yield _sse_chunk(
                                        chat_id=chat_id,
                                        model=model,
                                        created=created,
                                        content=close_tag,
                                    )

                        if emit_content or emit_reasoning or emit_tool_calls:
                            # Always ship body frames even on soft-disconnect
                            # probes. Skipping here drops tool_calls/text while
                            # upstream continues, so terminal arrives without the
                            # matching mid-stream items (Claude Code hard cut).
                            # Only after we know we will yield: bind success and
                            # lock failover. Premature stream_started + client_gone
                            # used to block silent account failover on empty turns.
                            stream_started = True
                            _note_success_once()
                            # Split content/reasoning and tool_calls into separate
                            # SSE frames. sub2api/Claude Code converters that open
                            # text then tool from one mixed delta can leave the
                            # wrong content_block active ("Content block not found").
                            if emit_content or emit_reasoning:
                                if timing is not None:
                                    timing.mark_first_token(
                                        kind="content" if emit_content else "reasoning"
                                    )
                                yield _sse_chunk(
                                    chat_id=chat_id,
                                    model=model,
                                    created=created,
                                    content=emit_content,
                                    reasoning=emit_reasoning,
                                )
                            if emit_tool_calls:
                                saw_tool_calls = True
                                if timing is not None:
                                    timing.mark_first_token(kind="tool")
                                _n_before = tools_emitted_count
                                async for _tc_frame in _emit_tool_sse_serial(
                                    chat_id=chat_id,
                                    model=model,
                                    created=created,
                                    tool_calls=emit_tool_calls,
                                    already_emitted=tools_emitted_count,
                                    max_tools=outbound_max_tools,
                                    protocol=protocol,
                                ):
                                    yield _tc_frame
                                    if _tc_frame.startswith("data: "):
                                        tools_emitted_count += 1
                                if tools_emitted_count < _n_before:
                                    tools_emitted_count = _n_before
                                # Continue draining any additional complete tools already
                                # held in tool_acc (serial + gap). Prevents agent loops
                                # from stalling when upstream packed multiple tools into
                                # one window but we only emit one per upstream line.
                                while True:
                                    _budget2 = history_compact.remaining_outbound_tool_budget(
                                        tools_emitted_count,
                                        max_tools=outbound_max_tools,
                                        protocol=protocol,
                                    )
                                    if _budget2 is not None and _budget2 <= 0:
                                        break
                                    # Re-check readiness without new deltas.
                                    more = _tool_call_argument_delta(tool_acc, [])
                                    if not more:
                                        break
                                    if _budget2 is not None and len(more) > _budget2:
                                        more = more[:_budget2]
                                    async for _tc_frame in _emit_tool_sse_serial(
                                        chat_id=chat_id,
                                        model=model,
                                        created=created,
                                        tool_calls=more,
                                        already_emitted=tools_emitted_count,
                                        max_tools=outbound_max_tools,
                                        protocol=protocol,
                                    ):
                                        yield _tc_frame
                                        if _tc_frame.startswith("data: "):
                                            tools_emitted_count += 1
                        elif finish:
                            # finish-only upstream frame: content already held
                            continue
                else:
                    raw = await resp.aread()
                    body_err = _classify_upstream_body_error(
                        raw,
                        content_type=resp.headers.get("content-type"),
                    )
                    if body_err:
                        raise RuntimeError(body_err)
                    try:
                        data = json.loads(raw)
                    except json.JSONDecodeError:
                        # Non-JSON 200 that isn't obvious HTML: still retryable.
                        preview = raw[:200].decode("utf-8", errors="replace")
                        raise RuntimeError(
                            "Upstream returned HTTP 200 with non-JSON body "
                            f"(empty/malformed): {preview!r}"
                        )
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
                        sanitized_tc: list[Any] | None = None
                        if emit_tc:
                            tools_pending = True
                            tools_requested = True
                            if isinstance(emit_tc, list):
                                # One tool per conversion tick (see stream path).
                                sanitized_tc = (
                                    _tool_call_argument_delta(tool_acc, emit_tc)
                                    or None
                                )
                            if sanitized_tc:
                                saw_tool_calls = True
                        # Flush remaining held tools one-by-one from non-SSE body.
                        if tool_acc and not client_gone:
                            flushed: list[Any] = []
                            while True:
                                one = _flush_one_tool_call(tool_acc)
                                if not one:
                                    break
                                flushed.extend(one)
                            if flushed:
                                saw_tool_calls = True
                                sanitized_tc = (sanitized_tc or []) + flushed
                        finish_reason = _normalize_stream_finish_reason(
                            finish_reason, saw_tool_calls=saw_tool_calls
                        ) or ("tool_calls" if saw_tool_calls else "stop")
                        if content:
                            content_parts.append(content)
                        if reasoning:
                            reasoning_parts.append(reasoning)
                        # Only mark progress when there is real payload — empty
                        # JSON 200 with finish_reason alone must remain failoverable.
                        if content or reasoning or sanitized_tc or msg_tool_calls:
                            stream_started = True
                            _note_success_once()
                        emit_content, emit_reasoning = reasoning_compat.rewrite(
                            content if content else None,
                            reasoning if reasoning else None,
                        )
                        # Same sub2api clash: if tools won, do not open a
                        # thinking/text block before (or with) tool_calls.
                        if tools_requested and (
                            saw_tool_calls or tools_pending or sanitized_tc
                        ):
                            emit_content, emit_reasoning = None, None
                            held_pre_tool.clear()
                            reasoning_compat.think_open = False
                        close_tag = reasoning_compat.close_tag_chunk()
                        if close_tag and not (
                            tools_requested and (saw_tool_calls or tools_pending)
                        ):
                            emit_content = (emit_content or "") + close_tag
                        # Tools first, then content only if this is a non-tool turn.
                        if sanitized_tc and not client_gone:
                            saw_tool_calls = True
                            stream_started = True
                            async for _tc_frame in _emit_tool_sse_serial(
                                chat_id=chat_id,
                                model=model,
                                created=created,
                                tool_calls=sanitized_tc,
                                already_emitted=tools_emitted_count,
                                max_tools=outbound_max_tools,
                                protocol=protocol,
                            ):
                                yield _tc_frame
                                if _tc_frame.startswith("data: "):
                                    tools_emitted_count += 1
                        if emit_content and not (
                            tools_requested and saw_tool_calls
                        ):
                            yield _sse_chunk(
                                chat_id=chat_id,
                                model=model,
                                created=created,
                                content=emit_content,
                            )
                        if (
                            emit_reasoning
                            and not reasoning_compat.enabled
                            and not (tools_requested and saw_tool_calls)
                        ):
                            yield _sse_chunk(
                                chat_id=chat_id,
                                model=model,
                                created=created,
                                reasoning=emit_reasoning,
                            )
                        # Defer finish_reason to terminal chunk with usage.
                        finished = True
                        held_finish = finish_reason

            # Flush deferred complete tool-argument snapshots before finish so
            # clients that only naive-append stream deltas still get full args.
            if tool_acc and not client_gone:
                # Flush remaining tools one SSE frame at a time (sub2api single
                # active content_block). Keepalive + wall-clock gap between tools.
                while True:
                    budget = history_compact.remaining_outbound_tool_budget(
                        tools_emitted_count,
                        max_tools=outbound_max_tools,
                        protocol=protocol,
                    )
                    if budget is not None and budget <= 0:
                        break
                    one = _flush_one_tool_call(tool_acc)
                    if not one:
                        break
                    saw_tool_calls = True
                    held_pre_tool.clear()
                    reasoning_compat.think_open = False
                    async for _tc_frame in _emit_tool_sse_serial(
                        chat_id=chat_id,
                        model=model,
                        created=created,
                        tool_calls=one,
                        already_emitted=tools_emitted_count,
                        max_tools=outbound_max_tools,
                        protocol=protocol,
                    ):
                        yield _tc_frame
                        if _tc_frame.startswith("data: "):
                            tools_emitted_count += 1
            final_tc = _finalize_tool_calls(tool_acc)
            # Only treat as tool-finish when something was (or will be) emitted.
            if final_tc and any(
                (e.get("_emitted") or (e.get("function") or {}).get("name"))
                for e in tool_acc.values()
            ):
                # Keep saw_tool_calls only if we actually shipped tool frames,
                # or flush above already set it. Avoid finish_reason=tool_calls
                # with zero outbound tool_calls (empty ghost slots).
                if any(e.get("_emitted") for e in tool_acc.values()):
                    saw_tool_calls = True
                    held_pre_tool.clear()
                    reasoning_compat.think_open = False

            # Empty HTTP 200: no content / tool_calls / held preface.
            # Failover before terminal frames so relays don't see "empty 200".
            joined_content = "".join(content_parts)
            joined_reasoning = "".join(reasoning_parts)
            has_held = any((c or r) for c, r in held_pre_tool)
            if (
                not client_gone
                and not saw_tool_calls
                and not has_held
                and _is_empty_model_payload(
                    content=joined_content,
                    reasoning=joined_reasoning,
                    tool_calls=final_tc,
                    saw_tool=saw_tool_calls,
                )
            ):
                empty_err = (
                    "Upstream returned HTTP 200 with empty model output "
                    "(no content/tool_calls)"
                )
                _report_upstream_failure(
                            creds.auth_key,
                            error=empty_err,
                            status_code=502,
                            model=model,
                        )
                _record_usage_safe(
                    ok=False,
                    api_key_id=api_key_id,
                    account_id=creds.auth_key,
                    model=model,
                    protocol="openai",
                    stream=True,
                    status_code=502,
                    latency_ms=timing.latency_ms() if timing is not None else None,
                    error=empty_err,
                    detail={"message": empty_err, "kind": "empty_upstream"},
                    timing=timing,
                )
                last_err = empty_err
                # Only role-only was sent: safe to failover to next account.
                if (not stream_started) and idx < len(chain) - 1:
                    continue
                if timing is not None:
                    timing.emit(ok=False, error=empty_err)
                err_payload = {
                    "id": chat_id,
                    "object": "error",
                    "error": {
                        "message": empty_err,
                        "type": "upstream_error",
                        "code": "empty_upstream",
                    },
                }
                yield f"data: {json.dumps(err_payload, ensure_ascii=False)}\n\n"
                yield "data: [DONE]\n\n"
                return

            _note_success_once()
            terminal_finish = _normalize_stream_finish_reason(
                held_finish if finished else None,
                saw_tool_calls=saw_tool_calls,
            ) or ("tool_calls" if saw_tool_calls else "stop")
            # Prefer real completion tokens from streamed content+reasoning; many
            # relays mark empty completion_tokens as a failed playground turn.
            # Compute usage BEFORE emitting finish so sub2api/new-api can read it
            # from the finish_reason chunk (they often ignore a later usage-only).
            # If tools won, omit pre-tool reasoning from usage "visible" estimate
            # still include it — billing should reflect upstream work.
            norm_usage = _usage_from_body_and_output(
                body,
                content=joined_content,
                reasoning=joined_reasoning,
                tool_calls=final_tc if saw_tool_calls else None,
                usage=usage,
            )
            # Soft disconnect must still close the SSE envelope. Skipping finish /
            # [DONE] after mid-stream content is the intermittent "hard cut" that
            # makes sub2api/Claude Code stop scheduling further turns.
            try:
                if not client_gone:
                    # Non-tool turn with tools_requested: flush held preface now.
                    # After flush above, saw_tool_calls is the source of truth —
                    # incomplete tool previews that never became outbound frames
                    # must not swallow a normal text answer.
                    if held_pre_tool and not saw_tool_calls:
                        for held_c, held_r in held_pre_tool:
                            if held_c or held_r:
                                stream_started = True
                                yield _sse_chunk(
                                    chat_id=chat_id,
                                    model=model,
                                    created=created,
                                    content=held_c,
                                    reasoning=held_r,
                                )
                        held_pre_tool.clear()
                    elif held_pre_tool and saw_tool_calls:
                        held_pre_tool.clear()
                    close_tag = reasoning_compat.close_tag_chunk()
                    if close_tag and not (tools_requested and saw_tool_calls):
                        yield _sse_chunk(
                            chat_id=chat_id,
                            model=model,
                            created=created,
                            content=close_tag,
                        )
                elif held_pre_tool:
                    held_pre_tool.clear()
                # Single terminal finish frame WITH usage (Scheme A). This is the
                # chunk secondary relays like sub2api inspect for token billing.
                # Always emit once the upstream turn ends (even if client_gone).
                yield _sse_chunk(
                    chat_id=chat_id,
                    model=model,
                    created=created,
                    finish_reason=terminal_finish,
                    usage=norm_usage,
                )
                # OpenAI-compatible usage-only fallback (empty choices) for
                # clients that follow stream_options.include_usage strictly.
                yield _sse_chunk(
                    chat_id=chat_id,
                    model=model,
                    created=created,
                    usage=norm_usage,
                    include_choices=False,
                )
                yield "data: [DONE]\n\n"
            except Exception:
                # Client socket already gone — nothing more we can ship.
                pass
            _record_usage_safe(
                usage=norm_usage,
                ok=True,
                api_key_id=api_key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="openai",
                stream=True,
                timing=timing,
            )
            return
        except asyncio.CancelledError:
            # Client/proxy cancelled mid-stream. Prefer a clean terminal frame
            # over a silent TCP drop when we already opened the SSE stream.
            if role_sent or stream_started:
                try:
                    yield _sse_chunk(
                        chat_id=chat_id,
                        model=model,
                        created=created,
                        finish_reason="stop",
                    )
                    yield "data: [DONE]\n\n"
                except Exception:
                    pass
            return
        except Exception as e:  # noqa: BLE001
            _report_upstream_failure(
                            creds.auth_key,
                            error=str(e),
                            status_code=502,
                            model=model,
                        )
            last_err = str(e)
            # Failover is still safe after role-only frames (no model bytes).
            # Only block failover once real content/tool frames were sent —
            # secondary relays treat mid-stream account switches as corruption.
            if stream_started:
                _record_usage_safe(
                    ok=False,
                    api_key_id=api_key_id,
                    account_id=creds.auth_key,
                    model=model,
                    protocol="openai",
                    stream=True,
                    status_code=502,
                    latency_ms=timing.latency_ms() if timing is not None else None,
                    error=f"Proxy error: {last_err}",
                    detail={
                        "message": f"Proxy error: {last_err}",
                        "exc_type": type(e).__name__,
                        "stream_started": True,
                    },
                    timing=timing,
                )
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
            _record_usage_safe(
                ok=False,
                api_key_id=api_key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="openai",
                stream=True,
                status_code=502,
                latency_ms=timing.latency_ms() if timing is not None else None,
                error=f"Proxy error: {last_err}",
                detail={
                    "message": f"Proxy error: {last_err}",
                    "exc_type": type(e).__name__,
                    "stream_started": False,
                },
                timing=timing,
            )
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
            "message": _sanitize_upstream_error_message(last_err or "", 503) or "All accounts failed",
            "type": "upstream_error",
        },
    }
    yield f"data: {json.dumps(err_payload, ensure_ascii=False)}\n\n"
    yield "data: [DONE]\n\n"


async def _collect_completion(
    *,
    url: str,
    headers: dict[str, str],
    body: dict[str, Any],
    account_id: str | None = None,
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
    req_body = _body_for_upstream(body)
    _ensure_stream_include_usage(req_body)

    client = await get_http_client(account_id)
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
            body_err = _classify_upstream_body_error(
                raw,
                content_type=resp.headers.get("content-type"),
            )
            if body_err:
                raise RuntimeError(body_err)
            try:
                data = json.loads(raw)
            except json.JSONDecodeError as e:
                preview = raw[:200].decode("utf-8", errors="replace")
                raise RuntimeError(
                    "Upstream returned HTTP 200 with non-JSON body "
                    f"(empty/malformed): {preview!r}"
                ) from e
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
    if _is_empty_model_payload(
        content=content, reasoning=reasoning, tool_calls=tool_calls
    ):
        raise RuntimeError(
            "Upstream returned HTTP 200 with empty model output "
            "(no content/tool_calls) — treat as retryable"
        )
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


def _resolve_anthropic_affinity(
    req: anth.AnthropicMessagesRequest,
    request: Request,
    *,
    api_key_id: str | None = None,
    body: dict[str, Any] | None = None,
) -> tuple[str | None, str | None, str | None, bool]:
    """Fingerprint for sticky multi-turn on Anthropic-shaped requests.

    Returns (fingerprint, preferred_account_id, prompt_cache_key, pck_minted).
    """
    conv_id = conversation_affinity.extract_conversation_id_from_headers(
        request.headers
    )
    if not conv_id and isinstance(req.metadata, dict):
        for k in ("conversation_id", "session_id", "thread_id"):
            if req.metadata.get(k):
                conv_id = str(req.metadata[k])
                break
    oa_msgs = anth.affinity_messages_from_request(req)
    # Prefer explicit metadata cache/session keys / headers; fall back to
    # cache_control fingerprint derived from system/tools so Claude Code sticks.
    pck = conversation_affinity.extract_prompt_cache_key(
        req
    ) or conversation_affinity.extract_prompt_cache_key_from_headers(request.headers)
    if not pck:
        pck = anth.extract_anthropic_prompt_cache_key(req)
    if pck:
        pck = conversation_affinity.normalize_prompt_cache_key(pck)
        minted = False
        if isinstance(body, dict):
            body["prompt_cache_key"] = pck
            body["_prompt_cache_key"] = pck
            body["_prompt_cache_key_minted"] = False
    else:
        pck, minted = _ensure_prompt_cache_key(
            body=body,
            req=req,
            headers=request.headers,
            api_key_id=api_key_id,
            conversation_id=conv_id,
            user=anth.metadata_user_id(req),
        )
    fp = conversation_affinity.conversation_fingerprint(
        oa_msgs,
        user=anth.metadata_user_id(req),
        conversation_id=conv_id,
        api_key_id=api_key_id,
        prompt_cache_key=pck,
    )
    prefer = conversation_affinity.get_affinity(fp) if fp else None
    return fp, prefer, pck, minted


def _anthropic_error_response(
    message: str,
    status: int = 500,
    err_type: str = "api_error",
    *,
    retry_after: float | int | None = None,
) -> JSONResponse:
    headers = {}
    if retry_after is not None:
        try:
            headers["Retry-After"] = str(max(1, int(float(retry_after))))
        except (TypeError, ValueError):
            headers["Retry-After"] = "5"
    return JSONResponse(
        status_code=status,
        content=anth.anthropic_error(message, status=status, err_type=err_type),
        headers=headers or None,
    )


@app.post("/v1/messages")
@app.post("/messages")
async def anthropic_messages(
    req: anth.AnthropicMessagesRequest,
    request: Request,
    anthropic_version: str | None = Header(default=None, alias="anthropic-version"),
    api_key: apikeys.ApiKeyRecord | None = Depends(require_api_key),
):
    """
    Anthropic Messages API compatible endpoint.
    Auth: `x-api-key` or `Authorization: Bearer …` (same managed keys as OpenAI).
    Optional header: `anthropic-version` (accepted, not enforced).
    """
    _ = anthropic_version  # accepted for client compatibility
    key_id = _api_key_id(api_key)
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

    timing = RequestTiming(protocol="anthropic", stream=bool(req.stream))
    conv_fp, prefer_account, pck, pck_minted = await asyncio.to_thread(
        _resolve_anthropic_affinity, req, request, api_key_id=key_id
    )
    timing.mark_affinity(prefer_account)
    model = resolve_model(req.model)
    timing.model = model
    message_id = f"msg_{uuid.uuid4().hex[:24]}"
    timing.req_id = message_id.replace("msg_", "")[:12]

    def _build_anthropic_body() -> dict[str, Any]:
        body_local = anth.build_openai_chat_body(
            req, model, force_stream=FORCE_UPSTREAM_STREAM
        )
        # Always stream upstream when forced; client may still want non-stream response
        if FORCE_UPSTREAM_STREAM:
            body_local["stream"] = True
        # Anthropic→OpenAI conversion can omit parameters; force the same scrub as
        # the OpenAI path so upstream never sees tools without `parameters`.
        _sanitize_upstream_body(body_local, model=model)
        _ensure_stream_include_usage(body_local)
        # Same prefix-stable prompt shaping as OpenAI path.
        _stabilize_upstream_prompt_body(body_local)
        # Same long-tool-loop compaction as OpenAI path (sub2api often hits OpenAI
        # chat/completions, but direct Anthropic /v1/messages also benefits).
        _apply_history_compact(body_local)
        return body_local

    try:
        (chain, pick_ms), body = await asyncio.gather(
            asyncio.to_thread(
                _pick_account_chain_timed,
                model=model,
                prefer_account_id=prefer_account,
            ),
            asyncio.to_thread(_build_anthropic_body),
        )
        timing.mark_pick(chain, elapsed_ms=pick_ms)
    except AuthError as e:
        try:
            from store.metrics import inc

            inc("g2a_auth_failures_total")
        except Exception:
            pass
        # Pool empty / temporary — 503 not 401 so clients retry instead of stopping.
        pe = _client_pool_error(e)
        detail = pe.body
        try:
            import json as _json
            detail = _json.loads(pe.body.decode("utf-8")).get("error", {}).get("message") or str(e)
        except Exception:
            detail = str(e)
        return _anthropic_error_response(
            detail, status=503, err_type="api_error", retry_after=8
        )
    _note_request_metrics(prefer_account=prefer_account, conv_fp=conv_fp)
    url = f"{UPSTREAM_BASE}/chat/completions"

    if isinstance(body, dict) and pck:
        body["prompt_cache_key"] = pck
        body["_prompt_cache_key"] = pck
        body["_prompt_cache_key_minted"] = bool(pck_minted)

    compact_hdr = {
        **_history_compact_headers(body),
        **_prompt_stabilize_headers(body),
        **_prompt_cache_key_headers(body, prompt_cache_key=pck, minted=pck_minted),
    }
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
                api_key_id=key_id,
                usage_ctx=_capture_usage_request_ctx(request),
                timing=timing,
            ),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache, no-transform",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
                "Content-Type": "text/event-stream; charset=utf-8",
                "X-Grok2API-Protocol": "anthropic",
                "X-Grok2API-Accounts": str(len(chain)),
                "X-Grok2API-Affinity": "1" if prefer_account else "0",
                **compact_hdr,
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
                url=url, headers=headers, body=body, account_id=creds.auth_key
            )
            await asyncio.to_thread(account_pool.report_success, creds.auth_key, model=model)
            if conv_fp:
                if prefer_account and prefer_account != creds.auth_key:
                    await asyncio.to_thread(
                        conversation_affinity.rebind_on_failover,
                        conv_fp,
                        first_tried,
                        creds.auth_key,
                    )
                    try:
                        from store.metrics import inc

                        inc("g2a_account_failovers_total")
                    except Exception:
                        pass
                else:
                    await asyncio.to_thread(
                        conversation_affinity.bind_affinity, conv_fp, creds.auth_key
                    )

            result = anth.openai_completion_to_anthropic(
                content=content or "",
                reasoning=reasoning or "",
                finish=finish,
                usage=usage,
                tool_calls=tool_calls,
                model=model,
                message_id=message_id,
                allowed_tool_names=anth.extract_tool_names(body.get("tools")),
            )
            # Normalize Anthropic usage (input/output + cache details) for the ledger.
            au = result.get("usage") if isinstance(result, dict) else None
            ledger_usage = None
            if isinstance(au, dict):
                ledger_usage = _normalize_usage(
                    {
                        "prompt_tokens": au.get("input_tokens") or 0,
                        "completion_tokens": au.get("output_tokens") or 0,
                        "cache_read_input_tokens": au.get("cache_read_input_tokens")
                        or 0,
                        "cache_creation_input_tokens": au.get(
                            "cache_creation_input_tokens"
                        )
                        or 0,
                    }
                )
            else:
                ledger_usage = _usage_from_body_and_output(
                    body,
                    content=content or "",
                    reasoning=reasoning or "",
                    tool_calls=tool_calls,
                    usage=usage,
                )
            _record_usage_safe(
                usage=ledger_usage,
                ok=True,
                api_key_id=key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="anthropic",
                stream=False,
                timing=timing,
            )
            # non-standard debug fields (ignored by strict SDKs that allow extra)
            result["x_grok2api_account"] = creds.email or creds.auth_key
            _attach_cache_debug_fields(
                result,
                ledger_usage if isinstance(ledger_usage, dict) else usage,
                prefer_account=bool(prefer_account),
                body=body,
                prompt_cache_key=pck,
                prompt_cache_key_minted=pck_minted,
            )
            if conv_fp:
                result["x_grok2api_conversation_fp"] = conv_fp
            timing.emit(ok=True)
            return _json_result_with_cache_headers(
                result,
                body=body,
                prefer_account=bool(prefer_account),
                conv_fp=conv_fp,
                prompt_cache_key=pck,
            )
        except httpx.HTTPStatusError as e:
            code = e.response.status_code if e.response is not None else 502
            detail = e.response.text[:800] if e.response is not None else str(e)
            hdrs = dict(e.response.headers) if e.response is not None else None
            _report_upstream_failure(
                creds.auth_key,
                error=detail,
                status_code=code,
                model=model,
                headers=hdrs,
            )
            try:
                from store.metrics import inc

                inc("g2a_upstream_failures_total")
            except Exception:
                pass
            _record_usage_safe(
                ok=False,
                api_key_id=key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="anthropic",
                stream=False,
                status_code=code,
                error=f"Upstream {code}: {detail}",
                detail={
                    "message": f"Upstream {code}: {detail}",
                    "upstream_status": code,
                    "upstream_body": detail,
                },
                timing=timing,
            )
            last_error = f"Upstream {code}: {detail}"
            last_status = code
            if not _retryable_status(code):
                break
            continue
        except Exception as e:  # noqa: BLE001
            _report_upstream_failure(
                            creds.auth_key,
                            error=str(e),
                            status_code=502,
                            model=model,
                        )
            try:
                from store.metrics import inc

                inc("g2a_upstream_failures_total")
            except Exception:
                pass
            _record_usage_safe(
                ok=False,
                api_key_id=key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="anthropic",
                stream=False,
                status_code=502,
                error=f"Proxy error: {e}",
                detail={"message": f"Proxy error: {e}", "exc_type": type(e).__name__},
                timing=timing,
            )
            last_error = f"Proxy error: {e}"
            last_status = 502
            continue

    final_status = last_status if last_status < 600 else 502
    retryable = final_status in (401, 403, 429, 500, 502, 503, 504)
    friendly = _sanitize_upstream_error_message(last_error or "", final_status)
    return _anthropic_error_response(
        friendly or last_error or "All accounts failed",
        status=503 if retryable else final_status,
        err_type="api_error",
        retry_after=8 if retryable else None,
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


# ── OpenAI Responses API (sub2api Anthropic→OpenAI path) ─────────────────────


def _responses_affinity(
    messages: list[dict[str, Any]],
    req_body: dict[str, Any],
    request: Request,
    *,
    api_key_id: str | None = None,
) -> tuple[str | None, str | None, str, str | None, bool]:
    """Sticky identity for OpenAI Responses multi-turn.

    Returns ``(session_fp, prefer_account_id, source, prompt_cache_key, pck_minted)``.

    Priority (see conversation_affinity.resolve_responses_affinity):
      1. explicit conversation_id
      2. prompt_cache_key (stable across turns — primary cache sticky path)
      3. previous_response_id chain → linked session_fp + account
      4. user / message root fingerprint

    When the client omits prompt_cache_key, mint one from conversation_id /
    previous_response_id so Claude Code / sub2api multi-turn still sticks and
    the key can be echoed for the next turn.
    """
    conv_id = conversation_affinity.extract_conversation_id_from_headers(
        request.headers
    )
    if not conv_id and isinstance(req_body.get("metadata"), dict):
        meta = req_body["metadata"]
        for k in ("conversation_id", "session_id", "thread_id"):
            if meta.get(k):
                conv_id = str(meta[k])
                break
    # Top-level Responses conversation object (OpenAI shape).
    if not conv_id:
        conv_obj = req_body.get("conversation")
        if isinstance(conv_obj, str) and conv_obj.strip():
            conv_id = conv_obj.strip()
        elif isinstance(conv_obj, dict) and conv_obj.get("id"):
            conv_id = str(conv_obj.get("id")).strip()
    user = req_body.get("user")
    if not user and isinstance(req_body.get("metadata"), dict):
        user = req_body["metadata"].get("user")
    prev_id = req_body.get("previous_response_id")
    prev_s = str(prev_id).strip() if prev_id else ""
    # Recover a stable seed from the response chain so minted keys do not
    # rotate every turn when clients only send previous_response_id.
    seed = None
    if prev_s and not conversation_affinity.extract_prompt_cache_key(req_body):
        try:
            entry = conversation_affinity.get_response_chain_entry(
                prev_s, api_key_id=api_key_id
            )
            if entry:
                seed = (entry.get("session_fp") or "").strip() or None
                # Prefer an explicitly stored synthetic key when present.
                stored = conversation_affinity.normalize_prompt_cache_key(
                    entry.get("prompt_cache_key")
                )
                if stored:
                    req_body["prompt_cache_key"] = stored
                    req_body["_prompt_cache_key"] = stored
                    req_body["_prompt_cache_key_minted"] = True
        except Exception:
            seed = None
    # Mint / normalize prompt_cache_key onto req_body so resolve_* and later
    # response echoes share the same value.
    pck, minted = _ensure_prompt_cache_key(
        body=req_body,
        req=req_body,
        headers=request.headers,
        api_key_id=api_key_id,
        conversation_id=conv_id,
        previous_response_id=prev_s or None,
        user=str(user) if user else None,
        seed=seed,
    )
    # Do NOT put previous_response_id into conversation_id — it changes every
    # turn and would shatter stickiness. Resolve it via response-chain binding.
    conv_fp, prefer, source = conversation_affinity.resolve_responses_affinity(
        messages,
        user=str(user) if user else None,
        conversation_id=conv_id,
        api_key_id=api_key_id,
        prompt_cache_key=pck,
        previous_response_id=str(prev_id) if prev_id else None,
    )
    # If we only had previous_response_id (no client pck) and mint used prev id,
    # re-key the sticky identity onto the minted pck fingerprint so future turns
    # that only echo our X-Grok2API-Prompt-Cache-Key keep the same account.
    # Always alias the minted/client prompt_cache_key fingerprint onto the same
    # sticky account. Codex multi-turn often only re-sends previous_response_id
    # (or later echoes our X-Grok2API-Prompt-Cache-Key); without this alias the
    # next turn looks sticky=0 and re-picks a cold account (cache miss + slow).
    if pck and prefer:
        try:
            pck_fp = conversation_affinity.conversation_fingerprint(
                messages,
                user=str(user) if user else None,
                conversation_id=None,
                api_key_id=api_key_id,
                prompt_cache_key=pck,
            )
            if pck_fp:
                conversation_affinity.bind_affinity(
                    pck_fp, prefer, session_fp=conv_fp or pck_fp
                )
                # Also bind under the active conv_fp so either key recovers the account.
                if conv_fp and conv_fp != pck_fp:
                    conversation_affinity.bind_affinity(
                        conv_fp, prefer, session_fp=conv_fp
                    )
        except Exception:
            pass
    return conv_fp, prefer, source, pck, minted


@app.post("/v1/responses")
@app.post("/responses")
async def openai_responses(
    request: Request,
    api_key: apikeys.ApiKeyRecord | None = Depends(require_api_key),
):
    """OpenAI Responses API compatibility endpoint.

    sub2api converts Claude Code /v1/messages → Responses and POSTs here when the
    account platform is openai. We translate to chat/completions against Grok
    upstream, then map the completion back to Responses JSON / SSE.
    """
    try:
        req_body = await request.json()
    except Exception:
        return openai_error(
            "Invalid JSON body", status=400, err_type="invalid_request_error"
        )
    if not isinstance(req_body, dict):
        return openai_error(
            "Request body must be a JSON object",
            status=400,
            err_type="invalid_request_error",
        )

    key_id = _api_key_id(api_key)
    model = resolve_model(req_body.get("model"))
    want_stream = bool(req_body.get("stream"))
    response_id = oai_resp.new_response_id()
    created_at = int(time.time())
    timing = RequestTiming(
        protocol="openai_responses",
        model=model,
        stream=want_stream,
        req_id=response_id.replace("resp_", "")[:12],
    )

    body = oai_resp.responses_request_to_chat_body(req_body, model=model)
    if not body.get("messages"):
        timing.emit(ok=False, error="empty input")
        return openai_error(
            "input must contain at least one message",
            status=400,
            err_type="invalid_request_error",
        )

    conv_fp, prefer_account, affinity_source, pck, pck_minted = await asyncio.to_thread(
        _responses_affinity,
        body.get("messages") or [],
        req_body,
        request,
        api_key_id=key_id,
    )
    timing.mark_affinity(prefer_account)

    def _prepare_responses_body() -> dict[str, Any]:
        # Force upstream stream collection path (same as chat non-stream clients).
        if FORCE_UPSTREAM_STREAM:
            body["stream"] = True
        _sanitize_upstream_body(body, model=model)
        _ensure_stream_include_usage(body)
        _stabilize_upstream_prompt_body(body)
        _apply_history_compact(body)
        return body

    try:
        (chain, pick_ms), body = await asyncio.gather(
            asyncio.to_thread(
                _pick_account_chain_timed,
                model=model,
                prefer_account_id=prefer_account,
            ),
            asyncio.to_thread(_prepare_responses_body),
        )
        timing.mark_pick(chain, elapsed_ms=pick_ms)
    except AuthError as e:
        try:
            from store.metrics import inc

            inc("g2a_auth_failures_total")
        except Exception:
            pass
        return _client_pool_error(e)

    _note_request_metrics(prefer_account=prefer_account, conv_fp=conv_fp)
    url = f"{UPSTREAM_BASE}/chat/completions"
    # Mirror minted/client prompt_cache_key onto the chat body for response echo.
    if isinstance(body, dict) and pck:
        body["prompt_cache_key"] = pck
        body["_prompt_cache_key"] = pck
        body["_prompt_cache_key_minted"] = bool(pck_minted)
    elif isinstance(req_body, dict) and req_body.get("_prompt_cache_key"):
        if isinstance(body, dict):
            body["prompt_cache_key"] = req_body.get("_prompt_cache_key")
            body["_prompt_cache_key"] = req_body.get("_prompt_cache_key")
            body["_prompt_cache_key_minted"] = bool(req_body.get("_prompt_cache_key_minted"))

    compact_hdr = {
        **_history_compact_headers(body),
        **_prompt_stabilize_headers(body),
        **_prompt_cache_key_headers(body, prompt_cache_key=pck, minted=pck_minted),
        "X-Grok2API-Affinity-Source": str(affinity_source or "none"),
    }
    prev_id = req_body.get("previous_response_id")
    metadata = req_body.get("metadata") if isinstance(req_body.get("metadata"), dict) else None

    async def _run_with_failover() -> tuple[
        str, str, str | None, dict[str, Any] | None, list[dict[str, Any]] | None, GrokCredentials
    ]:
        last_error: str | None = None
        last_status = 502
        first_tried: str | None = chain[0].auth_key if chain else None
        for creds in chain:
            headers = upstream_headers(creds.token, model)
            try:
                content, reasoning, finish, usage, tool_calls = await _collect_completion(
                    url=url,
                    headers=headers,
                    body=body,
                    account_id=creds.auth_key,
                )
                await asyncio.to_thread(
                    account_pool.report_success, creds.auth_key, model=model
                )
                if conv_fp:
                    if prefer_account and prefer_account != creds.auth_key:
                        await asyncio.to_thread(
                            conversation_affinity.rebind_on_failover,
                            conv_fp,
                            first_tried,
                            creds.auth_key,
                            session_fp=conv_fp,
                        )
                        try:
                            from store.metrics import inc

                            inc("g2a_account_failovers_total")
                        except Exception:
                            pass
                    else:
                        await asyncio.to_thread(
                            conversation_affinity.bind_affinity,
                            conv_fp,
                            creds.auth_key,
                            session_fp=conv_fp,
                        )
                # Pin emitted response_id so next turn's previous_response_id
                # recovers the same multi-turn session_fp (not just the account).
                await asyncio.to_thread(
                    conversation_affinity.bind_response_chain,
                    response_id,
                    creds.auth_key,
                    api_key_id=key_id,
                    session_fp=conv_fp,
                    prompt_cache_key=pck,
                )
                return content, reasoning, finish, usage, tool_calls, creds
            except httpx.HTTPStatusError as e:
                code = e.response.status_code if e.response is not None else 502
                detail = e.response.text[:800] if e.response is not None else str(e)
                hdrs = dict(e.response.headers) if e.response is not None else None
                _report_upstream_failure(
                creds.auth_key,
                error=detail,
                status_code=code,
                model=model,
                headers=hdrs,
            )
                try:
                    from store.metrics import inc

                    inc("g2a_upstream_failures_total")
                except Exception:
                    pass
                _record_usage_safe(
                    ok=False,
                    api_key_id=key_id,
                    account_id=creds.auth_key,
                    model=model,
                    protocol="openai_responses",
                    stream=want_stream,
                    status_code=code,
                    error=f"Upstream {code}: {detail}",
                    detail={
                        "message": f"Upstream {code}: {detail}",
                        "upstream_status": code,
                        "upstream_body": detail,
                    },
                    timing=timing,
                )
                last_error = f"Upstream {code}: {detail}"
                last_status = code
                if not _retryable_status(code):
                    break
                continue
            except Exception as e:  # noqa: BLE001
                _report_upstream_failure(
                            creds.auth_key,
                            error=str(e),
                            status_code=502,
                            model=model,
                        )
                try:
                    from store.metrics import inc

                    inc("g2a_upstream_failures_total")
                except Exception:
                    pass
                _record_usage_safe(
                    ok=False,
                    api_key_id=key_id,
                    account_id=creds.auth_key,
                    model=model,
                    protocol="openai_responses",
                    stream=want_stream,
                    status_code=502,
                    error=f"Proxy error: {e}",
                    detail={"message": f"Proxy error: {e}", "exc_type": type(e).__name__},
                    timing=timing,
                )
                last_error = f"Proxy error: {e}"
                last_status = 502
                continue
        final_status = last_status if last_status < 600 else 502
        friendly = _sanitize_upstream_error_message(last_error or "", final_status)
        raise RuntimeError(friendly or last_error or "All accounts failed")

    if want_stream:
        _resp_usage_ctx = _capture_usage_request_ctx(request)

        async def _sse_gen_live() -> AsyncIterator[str]:
            """True Responses streaming: first token as soon as upstream emits.

            Old path collected the full chat completion then replayed SSE, so
            TTFT equaled full completion latency for every sub2api client.
            """
            _usage_tok = _bind_usage_request_ctx(_resp_usage_ctx)
            last_error: str | None = None
            first_tried: str | None = chain[0].auth_key if chain else None
            try:
                for idx, creds in enumerate(chain):
                    headers = upstream_headers(creds.token, model)
                    streamer = oai_resp.ResponsesLiveStreamer(
                        response_id=response_id,
                        model=model,
                        created_at=created_at,
                        previous_response_id=str(prev_id) if prev_id else None,
                        metadata=metadata,
                        allowed_tool_names=anth.extract_tool_names(
                            body.get("tools")
                        ),
                    )
                    content_parts: list[str] = []
                    reasoning_parts: list[str] = []
                    tool_acc: dict[int, dict[str, Any]] = {}
                    usage: dict[str, Any] | None = None
                    stream_started = False
                    try:
                        timing.mark_upstream_start(
                            account_id=creds.auth_key, attempt=idx
                        )
                        upstream_body = _body_for_upstream(body)
                        client = await get_http_client(creds.auth_key)
                        async with client.stream(
                            "POST", url, headers=headers, json=upstream_body
                        ) as resp:
                            timing.mark_upstream_headers()
                            if resp.status_code >= 400:
                                err_text = (await resp.aread()).decode(
                                    "utf-8", errors="replace"
                                )[:1500]
                                _report_upstream_failure(
                creds.auth_key,
                error=err_text,
                status_code=resp.status_code,
                model=model,
                headers=dict(resp.headers,
            ),
                                )
                                _record_usage_safe(
                                    ok=False,
                                    api_key_id=key_id,
                                    account_id=creds.auth_key,
                                    model=model,
                                    protocol="openai_responses",
                                    stream=True,
                                    status_code=resp.status_code,
                                    latency_ms=timing.latency_ms(),
                                    error=f"Upstream {resp.status_code}: {err_text}",
                                    detail={
                                        "message": (
                                            f"Upstream {resp.status_code}: {err_text}"
                                        ),
                                        "upstream_status": resp.status_code,
                                        "upstream_body": err_text,
                                    },
                                    timing=timing,
                                )
                                last_error = (
                                    f"Upstream {resp.status_code}: {err_text}"
                                )
                                if (
                                    _retryable_status(resp.status_code)
                                    and idx < len(chain) - 1
                                    and not stream_started
                                ):
                                    continue
                                for frame in oai_resp.failed_responses_sse(
                                    response_id=response_id,
                                    message=_sanitize_upstream_error_message(
                                        last_error, resp.status_code
                                    )
                                    or last_error,
                                    model=model,
                                ):
                                    yield frame
                                return

                            # Early response.created at upstream 200 so sub2api/clients see first SSE
                            # bytes without waiting on model tokens (pre-1.9.63 TTFT).
                            # Trade-off: empty HTTP 200 cannot silent-failover after the
                            # envelope opens; we emit response.failed+[DONE] instead.
                            # Success/affinity still deferred until real content/tools.
                            success_noted = False
                            saw_model_output = False
                            if timing is not None:
                                timing.mark_tools_requested(_body_requests_tools(body))

                            def _note_success_once() -> None:
                                nonlocal success_noted
                                if success_noted:
                                    return
                                success_noted = True
                                asyncio.create_task(
                                    asyncio.to_thread(
                                        account_pool.report_success,
                                        creds.auth_key,
                                        model=model,
                                    )
                                )
                                if conv_fp:
                                    if prefer_account and prefer_account != creds.auth_key:
                                        asyncio.create_task(
                                            asyncio.to_thread(
                                                conversation_affinity.rebind_on_failover,
                                                conv_fp,
                                                first_tried,
                                                creds.auth_key,
                                                session_fp=conv_fp,
                                            )
                                        )
                                    else:
                                        asyncio.create_task(
                                            asyncio.to_thread(
                                                conversation_affinity.bind_affinity,
                                                conv_fp,
                                                creds.auth_key,
                                                session_fp=conv_fp,
                                            )
                                        )
                                # Pin emitted response_id for next previous_response_id
                                # so the linked multi-turn session_fp is recovered.
                                asyncio.create_task(
                                    asyncio.to_thread(
                                        conversation_affinity.bind_response_chain,
                                        response_id,
                                        creds.auth_key,
                                        api_key_id=key_id,
                                        session_fp=conv_fp,
                                        prompt_cache_key=pck,
                                    )
                                )

                            def _open_responses_stream() -> list[str]:
                                nonlocal stream_started
                                if stream_started and streamer._started:
                                    return []
                                frames = streamer.start()
                                if frames:
                                    stream_started = True
                                    if timing is not None:
                                        timing.mark_envelope_open(early=True)
                                return frames

                            # Open envelope ASAP after upstream 200 (early TTFT).
                            for frame in _open_responses_stream():
                                yield frame
                            # Sync pre-bind response_id → account as soon as the
                            # stream opens. Async create_task raced Codex's next
                            # turn (previous_response_id only) and caused sticky=0
                            # + cold pick + cache miss.
                            try:
                                await asyncio.to_thread(
                                    conversation_affinity.bind_response_chain,
                                    response_id,
                                    creds.auth_key,
                                    api_key_id=key_id,
                                    session_fp=conv_fp,
                                    prompt_cache_key=pck,
                                )
                            except Exception:
                                try:
                                    conversation_affinity.bind_response_chain(
                                        response_id,
                                        creds.auth_key,
                                        api_key_id=key_id,
                                        session_fp=conv_fp,
                                        prompt_cache_key=pck,
                                    )
                                except Exception:
                                    pass

                            ctype = (resp.headers.get("content-type") or "").lower()
                            if "text/event-stream" in ctype or "stream" in ctype:
                                client_gone = False
                                disconnect = _DisconnectProbe()
                                async for line in _aiter_sse_lines_with_keepalive(resp):
                                    # Debounced disconnect probe — single blip
                                    # under backpressure must not sticky-latch.
                                    client_gone = await disconnect.check(
                                        request.is_disconnected
                                    )
                                    if line is None:
                                        # Always poke the client during long thinking /
                                        # tool-prep gaps so secondary relays stay awake.
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
                                    if content:
                                        content_parts.append(content)
                                        # Only mark progress when client frames leave.
                                        text_frames = streamer.on_text_delta(content)
                                        if text_frames:
                                            saw_model_output = True
                                            _note_success_once()
                                            timing.mark_first_token(kind="content")
                                            # on_text_delta already opens envelope.
                                            stream_started = True
                                            for frame in text_frames:
                                                yield frame
                                    if reasoning:
                                        # Keep reasoning INTERNAL for every client.
                                        # v1.9.73 streamed reasoning as output_text for
                                        # Codex "accelerate" — that leaked model
                                        # monologues ("The user is just saying hello…")
                                        # into the visible assistant message. Codex
                                        # already gets keepalives + early envelope for
                                        # TTFT; thinking belongs in x_grok2api_reasoning
                                        # / usage only, never response.output_text.
                                        reasoning_parts.append(reasoning)
                                        if (
                                            timing is not None
                                            and timing.t_first_token is None
                                        ):
                                            timing.mark_held()
                                            timing.mark_first_token(
                                                kind="reasoning", held=True
                                            )
                                        if not client_gone:
                                            yield _sse_keepalive()
                                    if tool_calls:
                                        _merge_tool_call_delta(
                                            tool_acc, tool_calls
                                        )
                                        # on_tool_delta only returns frames when a
                                        # tool is complete enough to ship; incomplete
                                        # previews stay held and keep failover open.
                                        tool_frames = streamer.on_tool_delta(
                                            tool_calls
                                        )
                                        if tool_frames:
                                            saw_model_output = True
                                            _note_success_once()
                                            timing.mark_first_token(kind="tool")
                                            stream_started = True
                                            for frame in tool_frames:
                                                yield frame
                                        else:
                                            if timing is not None:
                                                timing.mark_held()
                                            if not client_gone:
                                                # Incomplete tool previews still mean the
                                                # upstream is alive — poke the client.
                                                yield _sse_keepalive()
                            else:
                                # Rare non-SSE upstream response: fall back to one-shot.
                                raw = await resp.aread()
                                body_err = _classify_upstream_body_error(
                                    raw,
                                    content_type=resp.headers.get("content-type"),
                                )
                                if body_err:
                                    raise RuntimeError(body_err)
                                try:
                                    data = json.loads(raw)
                                except json.JSONDecodeError as e:
                                    preview = raw[:200].decode(
                                        "utf-8", errors="replace"
                                    )
                                    raise RuntimeError(
                                        "Upstream returned HTTP 200 with non-JSON "
                                        f"body (empty/malformed): {preview!r}"
                                    ) from e
                                if isinstance(data.get("usage"), dict):
                                    usage = data["usage"]
                                choices = data.get("choices") or []
                                if choices:
                                    msg = (choices[0] or {}).get("message") or {}
                                    c = msg.get("content") or ""
                                    r = msg.get("reasoning_content") or ""
                                    if c:
                                        content_parts.append(c)
                                        text_frames = streamer.on_text_delta(c)
                                        if text_frames:
                                            saw_model_output = True
                                            _note_success_once()
                                            stream_started = True
                                            for frame in text_frames:
                                                yield frame
                                    if r:
                                        # Do not open envelope on reasoning alone.
                                        reasoning_parts.append(r)
                                    tcs = msg.get("tool_calls")
                                    if isinstance(tcs, list) and tcs:
                                        tool_frames = streamer.on_tool_delta(
                                            [
                                                {
                                                    "index": i,
                                                    "id": tc.get("id"),
                                                    "type": "function",
                                                    "function": tc.get("function")
                                                    or {},
                                                }
                                                for i, tc in enumerate(tcs)
                                                if isinstance(tc, dict)
                                            ]
                                        )
                                        if tool_frames:
                                            saw_model_output = True
                                            _note_success_once()
                                            stream_started = True
                                            for frame in tool_frames:
                                                yield frame

                            tool_calls_final = _finalize_tool_calls(tool_acc)
                            joined_content = "".join(content_parts)
                            joined_reasoning = "".join(reasoning_parts)

                            # Surface held reasoning as text when nothing else will
                            # ship. Claude Code / sub2api almost always send tools[],
                            # so the old "not tools requested" gate left pure-reasoning
                            # (and tools-requested + incomplete tool args) turns as
                            # empty_complete / empty_upstream → response.failed.
                            can_ship_tools = False
                            try:
                                can_ship_tools = bool(
                                    streamer.any_shipable_tool(terminal=True)
                                )
                            except Exception:
                                can_ship_tools = bool(
                                    tool_calls_final
                                    and any(
                                        (
                                            (tc.get("function") or {}).get("name")
                                            or ""
                                        ).strip()
                                        for tc in (tool_calls_final or [])
                                        if isinstance(tc, dict)
                                    )
                                )
                            # Do NOT dump joined_reasoning into output_text as a
                            # last-ditch body. That is how Codex users see the full
                            # thinking chain on short turns (hello / ack). Prefer
                            # empty_complete → response.failed / failover instead.

                            # Empty HTTP 200 / incomplete tool preview only:
                            # no client-visible text or completed tool frames.
                            # Envelope is already open (early TTFT) → emit
                            # response.failed+[DONE] instead of silent failover.
                            if (
                                not streamer.has_client_payload()
                                and not (joined_content or "").strip()
                                and not can_ship_tools
                                and not any(str(p or "") for p in streamer._text_parts)
                            ):
                                empty_err = (
                                    "Upstream returned HTTP 200 with empty model output "
                                    "(no content/tool_calls)"
                                )
                                held_tools = []
                                try:
                                    held_tools = streamer.held_tool_summary()
                                except Exception:
                                    held_tools = []
                                _report_upstream_failure(
                            creds.auth_key,
                            error=empty_err,
                            status_code=502,
                            model=model,
                        )
                                _record_usage_safe(
                                    ok=False,
                                    api_key_id=key_id,
                                    account_id=creds.auth_key,
                                    model=model,
                                    protocol="openai_responses",
                                    stream=True,
                                    status_code=502,
                                    latency_ms=timing.latency_ms(),
                                    error=empty_err,
                                    detail={
                                        "message": empty_err,
                                        "kind": "empty_upstream",
                                        "reasoning_chars": len(
                                            (joined_reasoning or "").strip()
                                        ),
                                        "held_tools": held_tools[:8],
                                    },
                                    timing=timing,
                                )
                                last_error = empty_err
                                if timing is not None:
                                    timing.emit(ok=False, error=empty_err)
                                # Always emit a Responses terminal. complete() used
                                # to set _closed on empty turns, which made fail() a
                                # no-op and left sub2api without response.failed/DONE
                                # ("stream usage incomplete: missing terminal event").
                                fail_frames = list(streamer.fail(empty_err))
                                if not fail_frames:
                                    fail_frames = list(
                                        oai_resp.failed_responses_sse(
                                            response_id=response_id,
                                            message=empty_err,
                                            model=model,
                                        )
                                    )
                                for frame in fail_frames:
                                    yield frame
                                return

                            ledger_usage = _usage_from_body_and_output(
                                body,
                                content=joined_content,
                                reasoning=joined_reasoning,
                                tool_calls=tool_calls_final,
                                usage=usage,
                            )
                            _note_success_once()
                            # Terminal flush ships named tools with parseable JSON
                            # (mid-stream required-key hold is relaxed only here).
                            # Truncated non-JSON args still return [] → failover.
                            complete_frames = streamer.complete(
                                usage=ledger_usage,
                                reasoning=joined_reasoning,
                                force_flush_partial_tools=True,
                            )
                            # Never promote joined_reasoning into output_text —
                            # that is the Codex "thinking chain as answer" leak on
                            # short turns (hello / ack). Empty body falls through to
                            # empty_complete → response.failed / failover below.
                            if not complete_frames and not streamer.has_client_payload():
                                empty_err = (
                                    "Upstream returned HTTP 200 with empty model output "
                                    "(no client-visible content/tool_calls)"
                                )
                                held_tools = []
                                try:
                                    held_tools = streamer.held_tool_summary()
                                except Exception:
                                    held_tools = []
                                _report_upstream_failure(
                            creds.auth_key,
                            error=empty_err,
                            status_code=502,
                            model=model,
                        )
                                _record_usage_safe(
                                    ok=False,
                                    api_key_id=key_id,
                                    account_id=creds.auth_key,
                                    model=model,
                                    protocol="openai_responses",
                                    stream=True,
                                    status_code=502,
                                    latency_ms=timing.latency_ms(),
                                    error=empty_err,
                                    detail={
                                        "message": empty_err,
                                        "kind": "empty_complete",
                                        "reasoning_chars": len(
                                            (joined_reasoning or "").strip()
                                        ),
                                        "held_tools": held_tools[:8],
                                    },
                                    timing=timing,
                                )
                                last_error = empty_err
                                if timing is not None:
                                    timing.emit(ok=False, error=empty_err)
                                # Always emit a Responses terminal. complete() used
                                # to set _closed on empty turns, which made fail() a
                                # no-op and left sub2api without response.failed/DONE
                                # ("stream usage incomplete: missing terminal event").
                                fail_frames = list(streamer.fail(empty_err))
                                if not fail_frames:
                                    fail_frames = list(
                                        oai_resp.failed_responses_sse(
                                            response_id=response_id,
                                            message=empty_err,
                                            model=model,
                                        )
                                    )
                                for frame in fail_frames:
                                    yield frame
                                return
                            # Soft disconnect still needs a clean terminal (completed/failed + DONE).
                            # Skipping here is the intermittent hard-cut that makes
                            # Claude Code / sub2api stop scheduling further turns.
                            # Prefer complete_frames (success path) over fail when
                            # we already have client-visible payload.
                            try:
                                if client_gone and not stream_started and not complete_frames:
                                    # Never opened and nothing to ship — stay silent.
                                    pass
                                elif complete_frames:
                                    for frame in complete_frames:
                                        stream_started = True
                                        yield frame
                                elif stream_started and not streamer._closed:
                                    for f in streamer.fail("client disconnected"):
                                        yield f
                            except Exception:
                                # Socket already gone — best-effort terminal only.
                                if stream_started and not streamer._closed:
                                    try:
                                        for f in streamer.fail("client disconnected"):
                                            yield f
                                    except Exception:
                                        pass
                            _record_usage_safe(
                                usage=ledger_usage,
                                ok=True,
                                api_key_id=key_id,
                                account_id=creds.auth_key,
                                model=model,
                                protocol="openai_responses",
                                stream=True,
                                timing=timing,
                            )
                            return
                    except asyncio.CancelledError:
                        # Client/proxy cancelled mid-stream. Prefer a clean
                        # Responses terminal over a silent TCP drop.
                        if stream_started:
                            try:
                                if not streamer._closed:
                                    for frame in streamer.fail("stream cancelled"):
                                        yield frame
                            except Exception:
                                pass
                        return
                    except Exception as e:  # noqa: BLE001
                        fail_msg = f"Proxy error: {e}"
                        _record_usage_safe(
                            ok=False,
                            api_key_id=key_id,
                            account_id=creds.auth_key,
                            model=model,
                            protocol="openai_responses",
                            stream=True,
                            status_code=502,
                            latency_ms=timing.latency_ms(),
                            error=fail_msg,
                            detail={
                                "message": fail_msg,
                                "exc_type": type(e).__name__,
                                "stream_started": bool(stream_started),
                            },
                            timing=timing,
                        )
                        if stream_started:
                            # Continue monotonic sequence_number after live deltas.
                            try:
                                for frame in streamer.fail(fail_msg):
                                    yield frame
                            except Exception:
                                pass
                            return
                        _report_upstream_failure(
                            creds.auth_key,
                            error=str(e),
                            status_code=502,
                            model=model,
                        )
                        last_error = fail_msg
                        if idx < len(chain) - 1:
                            continue
                        try:
                            for frame in streamer.fail(last_error):
                                yield frame
                        except Exception:
                            pass
                        return
                # No account produced a terminal response. Record a chain-level fail
                # so admin "断联" rows never show bare request_failed + empty detail.
                final_msg = (
                    _sanitize_upstream_error_message(last_error or "", 502)
                    or last_error
                    or "All accounts failed"
                )
                _record_usage_safe(
                    ok=False,
                    api_key_id=key_id,
                    account_id=first_tried,
                    model=model,
                    protocol="openai_responses",
                    stream=True,
                    status_code=502,
                    latency_ms=timing.latency_ms(),
                    error=final_msg,
                    detail={
                        "message": final_msg,
                        "kind": "all_accounts_failed",
                        "accounts": len(chain),
                        "last_error": last_error,
                    },
                    timing=timing,
                )
                for frame in oai_resp.failed_responses_sse(
                    response_id=response_id,
                    message=final_msg,
                    model=model,
                ):
                    yield frame
            finally:
                _reset_usage_request_ctx(_usage_tok)

        return StreamingResponse(
            _sse_gen_live(),
            media_type="text/event-stream",
            headers={
                "Cache-Control": "no-cache, no-transform",
                "Connection": "keep-alive",
                "X-Accel-Buffering": "no",
                "Content-Type": "text/event-stream; charset=utf-8",
                "X-Grok2API-Protocol": "openai_responses",
                "X-Grok2API-Accounts": str(len(chain)),
                "X-Grok2API-Affinity": "1" if prefer_account else "0",
                "X-Grok2API-Affinity-Source": str(affinity_source or "none"),
                **compact_hdr,
                **(
                    {"X-Grok2API-Conversation-Fp": conv_fp}
                    if conv_fp
                    else {}
                ),
                **(
                    {"X-Grok2API-Client": "openai-native"}
                    if history_compact.is_openai_native_client(
                        (_resp_usage_ctx or {}).get("user_agent")
                        if isinstance(_resp_usage_ctx, dict)
                        else None
                    )
                    else {}
                ),
            },
        )

    try:
        content, reasoning, _finish, usage, tool_calls, creds = await _run_with_failover()
    except RuntimeError as e:
        msg = str(e)
        # Pool / upstream transient → 503 so relays retry.
        return openai_error(
            msg or "All accounts failed",
            status=503,
            err_type="upstream_error",
            retry_after=8,
            code="all_accounts_failed",
        )
    except Exception as e:  # noqa: BLE001
        return openai_error(
            f"Proxy error: {e}",
            status=502,
            err_type="upstream_error",
        )

    ledger_usage = _usage_from_body_and_output(
        body,
        content=content or "",
        reasoning=reasoning or "",
        tool_calls=tool_calls,
        usage=usage,
    )
    _record_usage_safe(
        usage=ledger_usage,
        ok=True,
        api_key_id=key_id,
        account_id=creds.auth_key,
        model=model,
        protocol="openai_responses",
        stream=False,
        timing=timing,
    )
    result = oai_resp.build_responses_object(
        response_id=response_id,
        model=model,
        content=content or "",
        reasoning=reasoning or "",
        tool_calls=tool_calls,
        usage=ledger_usage,
        created_at=created_at,
        previous_response_id=str(prev_id) if prev_id else None,
        metadata=metadata,
        allowed_tool_names=anth.extract_tool_names(body.get("tools")),
    )
    result["x_grok2api_account"] = creds.email or creds.auth_key
    _attach_cache_debug_fields(
        result,
        ledger_usage,
        prefer_account=bool(prefer_account),
        body=body,
        prompt_cache_key=pck,
        prompt_cache_key_minted=pck_minted,
    )
    if conv_fp:
        result["x_grok2api_conversation_fp"] = conv_fp
    result["x_grok2api_affinity_source"] = str(affinity_source or "none")
    hc_stats = body.get("_history_compact") if isinstance(body, dict) else None
    if isinstance(hc_stats, dict):
        result["x_grok2api_history_compact"] = hc_stats
    timing.emit(ok=True)
    return _json_result_with_cache_headers(
        result,
        body=body,
        prefer_account=bool(prefer_account),
        conv_fp=conv_fp,
        prompt_cache_key=pck,
        affinity_source=affinity_source,
    )


async def _stream_anthropic_with_failover(
    *,
    url: str,
    body: dict[str, Any],
    chain: list[GrokCredentials],
    message_id: str,
    model: str,
    client_disconnected,
    conversation_fp: str | None = None,
    api_key_id: str | None = None,
    usage_ctx: dict[str, Any] | None = None,
    timing: RequestTiming | None = None,
) -> AsyncIterator[str]:
    """Upstream OpenAI SSE → Anthropic Messages SSE with account failover."""
    _usage_tok = _bind_usage_request_ctx(usage_ctx)
    try:
        async for chunk in _stream_anthropic_with_failover_inner(
            url=url,
            body=body,
            chain=chain,
            message_id=message_id,
            model=model,
            client_disconnected=client_disconnected,
            conversation_fp=conversation_fp,
            api_key_id=api_key_id,
            timing=timing,
        ):
            yield chunk
    finally:
        _reset_usage_request_ctx(_usage_tok)


async def _stream_anthropic_with_failover_inner(
    *,
    url: str,
    body: dict[str, Any],
    chain: list[GrokCredentials],
    message_id: str,
    model: str,
    client_disconnected,
    conversation_fp: str | None = None,
    api_key_id: str | None = None,
    timing: RequestTiming | None = None,
) -> AsyncIterator[str]:
    last_err: str | None = None
    first_tried = chain[0].auth_key if chain else None
    # TTFT: do NOT scan full messages/tools for prompt estimate before upstream.
    # message_start can open with 0; finish/message_delta carry real usage later.
    # Heavy estimate only runs if upstream never returns usage (fallback path).
    prompt_est = 0
    prompt_est_computed = False

    def _ensure_prompt_est() -> int:
        nonlocal prompt_est, prompt_est_computed
        if prompt_est_computed:
            return prompt_est
        prompt_est_computed = True
        est = _messages_prompt_estimate(body.get("messages"))
        if body.get("tools"):
            try:
                est += _estimate_text_tokens(
                    json.dumps(body.get("tools"), ensure_ascii=False)
                )
            except (TypeError, ValueError):
                pass
        prompt_est = int(est or 0)
        return prompt_est

    tools_requested = _body_requests_tools(body)
    allowed_tool_names = anth.extract_tool_names(body.get("tools"))
    if timing is not None:
        timing.mark_tools_requested(tools_requested)
    upstream_body = _body_for_upstream(body)
    for idx, creds in enumerate(chain):
        headers = upstream_headers(creds.token, model)
        assembler = anth.AnthropicStreamAssembler(
            message_id=message_id,
            model=model,
            tools_requested=tools_requested,
            max_tools=history_compact.resolve_outbound_max_tools("anthropic"),
            allowed_tool_names=allowed_tool_names,
        )
        finished = False
        stream_started = False
        usage: dict[str, Any] | None = None
        held_finish: str | None = None
        try:
            if timing is not None:
                timing.mark_upstream_start(account_id=creds.auth_key, attempt=idx)
            client = await get_http_client(creds.auth_key)
            async with client.stream(
                "POST", url, headers=headers, json=upstream_body
            ) as resp:
                if timing is not None:
                    timing.mark_upstream_headers()
                if resp.status_code >= 400:
                    err_text = (await resp.aread()).decode(
                        "utf-8", errors="replace"
                    )[:1500]
                    _report_upstream_failure(
                creds.auth_key,
                error=err_text,
                status_code=resp.status_code,
                model=model,
                headers=dict(resp.headers,
            ),
                    )
                    _record_usage_safe(
                        ok=False,
                        api_key_id=api_key_id,
                        account_id=creds.auth_key,
                        model=model,
                        protocol="anthropic",
                        stream=True,
                        status_code=resp.status_code,
                        latency_ms=timing.latency_ms() if timing is not None else None,
                        error=f"Upstream {resp.status_code}: {err_text}",
                        detail={
                            "message": f"Upstream {resp.status_code}: {err_text}",
                            "upstream_status": resp.status_code,
                            "upstream_body": err_text,
                        },
                        timing=timing,
                    )
                    last_err = f"Upstream {resp.status_code}: {err_text}"
                    if _retryable_status(resp.status_code) and idx < len(
                        chain
                    ) - 1:
                        continue
                    if timing is not None:
                        timing.emit(ok=False, error=last_err)
                    for _term_ev in anth.anthropic_stream_terminal_error(
                        last_err, err_type="api_error"
                    ):
                        yield _term_ev
                    return

                # Early message_start at upstream 200 so clients get first SSE bytes
                # without waiting on model tokens (restores pre-1.9.63 TTFT feel).
                # Trade-off: empty HTTP 200 can no longer silent-failover after the
                # envelope opens; we emit a clean api_error terminal instead (same
                # as v1.9.46). Success/affinity still deferred until real output.
                success_noted = False
                content_seen = False
                reasoning_seen = False
                saw_model_output = False

                def _note_success_once() -> None:
                    nonlocal success_noted
                    if success_noted:
                        return
                    success_noted = True
                    asyncio.create_task(
                        asyncio.to_thread(
                            account_pool.report_success, creds.auth_key, model=model
                        )
                    )
                    if conversation_fp:
                        if idx > 0:
                            asyncio.create_task(
                                asyncio.to_thread(
                                    conversation_affinity.rebind_on_failover,
                                    conversation_fp,
                                    first_tried,
                                    creds.auth_key,
                                )
                            )
                        else:
                            asyncio.create_task(
                                asyncio.to_thread(
                                    conversation_affinity.bind_affinity,
                                    conversation_fp,
                                    creds.auth_key,
                                )
                            )

                def _open_anthropic_stream() -> list[str]:
                    nonlocal stream_started
                    if stream_started:
                        return []
                    # Open with 0 input_tokens; finish() attaches real usage later.
                    frames = assembler.start(input_tokens=0)
                    stream_started = True
                    if timing is not None:
                        timing.mark_envelope_open(early=True)
                    return frames

                # Open envelope ASAP after upstream 200 (early TTFT).
                for ev in _open_anthropic_stream():
                    yield ev

                ctype = (resp.headers.get("content-type") or "").lower()
                client_gone = False
                disconnect = _DisconnectProbe()
                if "text/event-stream" in ctype or "stream" in ctype:
                    async for line in _aiter_sse_lines_with_keepalive(resp):
                        # Debounced disconnect probe — single blip under
                        # backpressure must not sticky-latch client_gone.
                        client_gone = await disconnect.check(client_disconnected)
                        if line is None:
                            # Always ping during long thinking / tool-prep gaps.
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
                            # Soft-disconnect before any client bytes: do not mark
                            # success / model-output. Keep draining for terminal.
                            if not (client_gone and not stream_started):
                                if content:
                                    content_seen = True
                                    saw_model_output = True
                                if reasoning:
                                    reasoning_seen = True
                                    if str(reasoning).strip():
                                        saw_model_output = True
                                if tool_calls:
                                    saw_model_output = True
                                if timing is not None and (
                                    content or tool_calls or reasoning
                                ):
                                    # tools-preface hold: body may not leave yet
                                    held_now = bool(
                                        tools_requested
                                        and (content or reasoning)
                                        and not tool_calls
                                        and not assembler._saw_tool
                                    )
                                    if held_now:
                                        timing.mark_held()
                                    timing.mark_first_token(
                                        kind=(
                                            "tool"
                                            if tool_calls
                                            else (
                                                "content" if content else "reasoning"
                                            )
                                        ),
                                        held=held_now,
                                    )
                                _note_success_once()
                                if not stream_started:
                                    open_frames = _open_anthropic_stream()
                                    if open_frames:
                                        for ev in open_frames:
                                            yield ev
                                async for ev in _yield_anthropic_events_serial(
                                    assembler.feed(
                                        content=content or None,
                                        reasoning=reasoning or None,
                                        tool_calls=tool_calls,
                                    ),
                                    client_gone=client_gone,
                                ):
                                    yield ev
                        if finish:
                            # Capture finish but keep reading — usage often
                            # arrives on a subsequent empty-choices chunk.
                            finished = True
                            held_finish = finish
                    # Empty HTTP 200: envelope already open → clean api_error
                    # (no silent failover after early open; same as v1.9.46).
                    if (
                        not saw_model_output
                        and not assembler._saw_tool
                        and not content_seen
                        and not reasoning_seen
                    ):
                        empty_err = (
                            "Upstream returned HTTP 200 with empty model output "
                            "(no content/tool_calls)"
                        )
                        _report_upstream_failure(
                            creds.auth_key,
                            error=empty_err,
                            status_code=502,
                            model=model,
                        )
                        _record_usage_safe(
                            ok=False,
                            api_key_id=api_key_id,
                            account_id=creds.auth_key,
                            model=model,
                            protocol="anthropic",
                            stream=True,
                            status_code=502,
                            latency_ms=timing.latency_ms() if timing is not None else None,
                            error=empty_err,
                            detail={"message": empty_err, "kind": "empty_upstream"},
                            timing=timing,
                        )
                        last_err = empty_err
                        if timing is not None:
                            timing.emit(ok=False, error=empty_err)
                        # Envelope already open: terminal error for the client.
                        if not stream_started:
                            for ev in _open_anthropic_stream():
                                yield ev
                        for _term_ev in anth.anthropic_stream_terminal_error(
                            empty_err, err_type="api_error"
                        ):
                            yield _term_ev
                        return
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
                    _note_success_once()
                    # Soft disconnect still needs message_delta/stop after the
                    # envelope opened. Skipping those frames is the intermittent
                    # hard-cut that makes Claude Code stop scheduling. If we never
                    # opened to the client, stay silent (no bare stop frames).
                    try:
                        if client_gone and not stream_started:
                            pass
                        else:
                            if not stream_started:
                                for ev in _open_anthropic_stream():
                                    yield ev
                            terminal_events = assembler.finish(
                                fr, usage=usage, input_tokens=_ensure_prompt_est()
                            )
                            if client_gone:
                                # Soft disconnect: still ship block closes +
                                # message_delta/stop so Claude Code / sub2api can
                                # leave "running" and update task status. Skipping
                                # content_block_stop leaves an open tool_use and
                                # freezes the agent turn.
                                for ev in terminal_events:
                                    if (
                                        '"type": "content_block_stop"' in ev
                                        or '"type": "message_delta"' in ev
                                        or '"type": "message_stop"' in ev
                                        or '"type": "error"' in ev
                                        or '"type": "content_block_start"' in ev
                                        or '"type": "content_block_delta"' in ev
                                    ):
                                        yield ev
                            else:
                                async for ev in _yield_anthropic_events_serial(
                                    terminal_events,
                                    client_gone=False,
                                ):
                                    yield ev
                    except Exception:
                        if stream_started:
                            try:
                                for _term_ev in anth.anthropic_stream_terminal_error(
                                    "client disconnected", err_type="api_error"
                                ):
                                    yield _term_ev
                            except Exception:
                                pass
                    _record_usage_safe(
                        usage=_usage_from_body_and_output(
                            body,
                            usage=usage,
                            content="",
                            reasoning="",
                        )
                        if usage
                        else {
                            "prompt_tokens": _ensure_prompt_est(),
                            "completion_tokens": 0,
                            "total_tokens": _ensure_prompt_est(),
                        },
                        ok=True,
                        api_key_id=api_key_id,
                        account_id=creds.auth_key,
                        model=model,
                        protocol="anthropic",
                        stream=True,
                        timing=timing,
                    )
                    return
                else:
                    raw = await resp.aread()
                    body_err = _classify_upstream_body_error(
                        raw,
                        content_type=resp.headers.get("content-type"),
                    )
                    if body_err:
                        _report_upstream_failure(
                            creds.auth_key,
                            error=body_err,
                            status_code=502,
                            model=model,
                        )
                        _record_usage_safe(
                            ok=False,
                            api_key_id=api_key_id,
                            account_id=creds.auth_key,
                            model=model,
                            protocol="anthropic",
                            stream=True,
                            status_code=502,
                            latency_ms=timing.latency_ms() if timing is not None else None,
                            error=body_err,
                            detail={"message": body_err, "kind": "upstream_body_error"},
                            timing=timing,
                        )
                        last_err = body_err
                        if (not stream_started) and idx < len(chain) - 1:
                            continue
                        if timing is not None:
                            timing.emit(ok=False, error=body_err)
                        if not stream_started:
                            for ev in _open_anthropic_stream():
                                yield ev
                        for _term_ev in anth.anthropic_stream_terminal_error(
                            body_err, err_type="api_error"
                        ):
                            yield _term_ev
                        return
                    try:
                        data = json.loads(raw)
                    except json.JSONDecodeError:
                        text = raw.decode("utf-8", errors="replace")
                        empty_err = (
                            "Upstream returned HTTP 200 with non-JSON body "
                            f"(empty/malformed): {text[:160]!r}"
                        )
                        _report_upstream_failure(
                            creds.auth_key,
                            error=empty_err,
                            status_code=502,
                            model=model,
                        )
                        _record_usage_safe(
                            ok=False,
                            api_key_id=api_key_id,
                            account_id=creds.auth_key,
                            model=model,
                            protocol="anthropic",
                            stream=True,
                            status_code=502,
                            latency_ms=timing.latency_ms() if timing is not None else None,
                            error=empty_err,
                            detail={"message": empty_err, "kind": "non_json_upstream"},
                            timing=timing,
                        )
                        last_err = empty_err
                        if (not stream_started) and idx < len(chain) - 1:
                            continue
                        if timing is not None:
                            timing.emit(ok=False, error=empty_err)
                        if not stream_started:
                            for ev in _open_anthropic_stream():
                                yield ev
                        for _term_ev in anth.anthropic_stream_terminal_error(
                            empty_err, err_type="api_error"
                        ):
                            yield _term_ev
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
                        if _is_empty_model_payload(
                            content=content,
                            reasoning=reasoning,
                            tool_calls=tool_calls
                            if isinstance(tool_calls, list)
                            else None,
                        ):
                            empty_err = (
                                "Upstream returned HTTP 200 with empty model output "
                                "(no content/tool_calls)"
                            )
                            _report_upstream_failure(
                            creds.auth_key,
                            error=empty_err,
                            status_code=502,
                            model=model,
                        )
                            _record_usage_safe(
                                ok=False,
                                api_key_id=api_key_id,
                                account_id=creds.auth_key,
                                model=model,
                                protocol="anthropic",
                                stream=True,
                                status_code=502,
                                latency_ms=timing.latency_ms() if timing is not None else None,
                                error=empty_err,
                                detail={"message": empty_err, "kind": "empty_upstream"},
                                timing=timing,
                            )
                            last_err = empty_err
                            if (not stream_started) and idx < len(chain) - 1:
                                continue
                            if timing is not None:
                                timing.emit(ok=False, error=empty_err)
                            if not stream_started:
                                for ev in _open_anthropic_stream():
                                    yield ev
                            for _term_ev in anth.anthropic_stream_terminal_error(
                                empty_err, err_type="api_error"
                            ):
                                yield _term_ev
                            return
                        _note_success_once()
                        for ev in _open_anthropic_stream():
                            yield ev
                        if content or reasoning or tool_calls:
                            async for ev in _yield_anthropic_events_serial(
                                assembler.feed(
                                    content=content or None,
                                    reasoning=reasoning or None,
                                    tool_calls=tool_calls,
                                )
                            ):
                                yield ev
                        if tool_calls and finish_reason in (
                            None,
                            "stop",
                            "end_turn",
                            "",
                        ):
                            finish_reason = "tool_calls"
                        async for ev in _yield_anthropic_events_serial(
                            assembler.finish(
                                finish_reason,
                                usage=usage,
                                input_tokens=_ensure_prompt_est(),
                            )
                        ):
                            yield ev
                        _record_usage_safe(
                            usage=_usage_from_body_and_output(
                                body,
                                usage=usage,
                                content="",
                                reasoning="",
                            )
                            if usage
                            else {
                                "prompt_tokens": _ensure_prompt_est(),
                                "completion_tokens": 0,
                                "total_tokens": _ensure_prompt_est(),
                            },
                            ok=True,
                            api_key_id=api_key_id,
                            account_id=creds.auth_key,
                            model=model,
                            protocol="anthropic",
                            stream=True,
                            timing=timing,
                        )
                        return

            if not finished:
                _note_success_once()
                if not stream_started:
                    for ev in _open_anthropic_stream():
                        yield ev
                async for ev in _yield_anthropic_events_serial(
                    assembler.finish(
                        "tool_calls" if assembler._saw_tool else "stop",
                        usage=usage,
                        input_tokens=_ensure_prompt_est(),
                    )
                ):
                    yield ev
            _record_usage_safe(
                usage=_usage_from_body_and_output(
                    body,
                    usage=usage,
                    content="",
                    reasoning="",
                )
                if usage
                else {
                    "prompt_tokens": _ensure_prompt_est(),
                    "completion_tokens": 0,
                    "total_tokens": _ensure_prompt_est(),
                },
                ok=True,
                api_key_id=api_key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="anthropic",
                stream=True,
                timing=timing,
            )
            return
        except asyncio.CancelledError:
            # Prefer a clean Anthropic error/stop over a hard cut when stream open.
            if stream_started:
                try:
                    for _term_ev in anth.anthropic_stream_terminal_error(
                        "stream cancelled", err_type="api_error"
                    ):
                        yield _term_ev
                except Exception:
                    pass
            return
        except Exception as e:  # noqa: BLE001
            _report_upstream_failure(
                creds.auth_key,
                error=str(e),
                status_code=502,
                model=model,
            )
            last_err = str(e)
            _record_usage_safe(
                ok=False,
                api_key_id=api_key_id,
                account_id=creds.auth_key,
                model=model,
                protocol="anthropic",
                stream=True,
                status_code=502,
                latency_ms=timing.latency_ms() if timing is not None else None,
                error=f"Proxy error: {e}",
                detail={
                    "message": f"Proxy error: {e}",
                    "exc_type": type(e).__name__,
                    "stream_started": bool(stream_started),
                },
                timing=timing,
            )
            # Mid-stream failures cannot safely failover for secondary relays
            if stream_started:
                for _term_ev in anth.anthropic_stream_terminal_error(
                    last_err or "proxy_error", err_type="api_error"
                ):
                    yield _term_ev
                return
            if idx < len(chain) - 1:
                continue
            for _term_ev in anth.anthropic_stream_terminal_error(
                last_err or "proxy_error", err_type="api_error"
            ):
                yield _term_ev
            return

    for _term_ev in anth.anthropic_stream_terminal_error(
        _sanitize_upstream_error_message(last_err or "", 503) or "All accounts failed", err_type="api_error"
    ):
        yield _term_ev


def _static_file_response(rel_path: str):
    """Serve static files safely under multi-worker.

    JS/CSS/dist assets are returned as a single in-memory Response with an exact
    Content-Length and Accept-Ranges disabled. This prevents browsers from
    reporting net::ERR_CONTENT_LENGTH_MISMATCH when a worker recycles mid-download
    or a proxy serves a partial body — which previously left the admin UI stuck
    until a manual hard refresh.
    """
    raw = (rel_path or "").lstrip("/")
    if not raw or ".." in raw.split("/"):
        return JSONResponse({"detail": "not found"}, status_code=404)
    target = (STATIC_DIR / raw).resolve()
    try:
        target.relative_to(STATIC_DIR.resolve())
    except Exception:
        return JSONResponse({"detail": "not found"}, status_code=404)
    if not target.is_file():
        return JSONResponse({"detail": "not found"}, status_code=404)

    suffix = target.suffix.lower()
    media = {
        ".js": "application/javascript; charset=utf-8",
        ".css": "text/css; charset=utf-8",
        ".html": "text/html; charset=utf-8",
        ".json": "application/json; charset=utf-8",
        ".map": "application/json; charset=utf-8",
        ".ico": "image/x-icon",
        ".svg": "image/svg+xml",
        ".png": "image/png",
        ".jpg": "image/jpeg",
        ".jpeg": "image/jpeg",
        ".woff": "font/woff",
        ".woff2": "font/woff2",
    }.get(suffix, "application/octet-stream")

    # Immutable content-hashed bundles under /static/dist/ can be cached long.
    # Logical /static/js/* kept for compatibility but still no-store to avoid
    # stale partials.
    is_dist = "/dist/" in ("/" + raw.replace("\\", "/")) or raw.startswith("dist/")
    is_text_asset = suffix in {".js", ".css", ".html", ".json", ".map"}

    if is_text_asset:
        data = target.read_bytes()
        if is_dist and suffix in {".js", ".css"}:
            cache = "public, max-age=31536000, immutable"
        else:
            cache = "no-store, no-cache, must-revalidate, max-age=0"
        headers = {
            "Cache-Control": cache,
            "Pragma": "no-cache" if not is_dist else "public",
            "X-Content-Type-Options": "nosniff",
            "Accept-Ranges": "none",
            "Content-Length": str(len(data)),
        }
        if not is_dist:
            headers["Pragma"] = "no-cache"
        else:
            headers.pop("Pragma", None)
        return Response(content=data, media_type=media, headers=headers)

    return FileResponse(
        target,
        media_type=media,
        headers={
            "Cache-Control": "public, max-age=3600, must-revalidate",
            "X-Content-Type-Options": "nosniff",
            "Accept-Ranges": "none",
        },
    )



@app.get("/static/{file_path:path}", include_in_schema=False)
async def static_assets(file_path: str):
    return _static_file_response(file_path)


# Mount static assets if present (css/js under /static) — kept as fallback for tools expecting mount
if STATIC_DIR.is_dir():
    # Prefer explicit route above; mount remains for compatibility when route not matched
    try:
        app.mount("/static-files", StaticFiles(directory=str(STATIC_DIR)), name="static_files")
    except Exception:
        pass


def _pick_listen_host() -> str:
    """Prefer explicit env host; keep loopback accessible via 127.0.0.1."""
    return HOST or "127.0.0.1"


def _detect_public_base_url(port: int) -> str | None:
    """Best-effort public origin when GROK2API_PUBLIC_BASE_URL is unset.

    Uses the host's outbound/default route IP so Docker/server banners show a
    reachable address without hardcoding a domain. Admin/UI still prefer the
    live request Host / X-Forwarded-* headers on each call.
    """
    import socket

    candidates: list[str] = []
    # UDP "connect" does not send packets; it reveals the preferred source IP.
    for family, probe in (
        (socket.AF_INET, ("1.1.1.1", 80)),
        (socket.AF_INET6, ("2606:4700:4700::1111", 80)),
    ):
        try:
            with socket.socket(family, socket.SOCK_DGRAM) as s:
                s.connect(probe)
                ip = s.getsockname()[0]
        except OSError:
            continue
        if not ip or ip.startswith("127.") or ip in ("::1",):
            continue
        # Skip typical Docker/bridge private ranges only when an explicit
        # public-looking address is also available later; still usable fallback.
        candidates.append(ip)

    if not candidates:
        try:
            hostname = socket.gethostname()
            for info in socket.getaddrinfo(hostname, None):
                ip = info[4][0]
                if ip and not ip.startswith("127.") and ip not in ("::1",):
                    candidates.append(ip)
        except OSError:
            pass

    # Prefer global-looking IPv4, then any non-loopback.
    def _score(ip: str) -> tuple[int, int]:
        private = (
            ip.startswith("10.")
            or ip.startswith("192.168.")
            or ip.startswith("172.")
            and any(ip.startswith(f"172.{n}.") for n in range(16, 32))
            or ip.startswith("fc")
            or ip.startswith("fd")
            or ip.startswith("fe80:")
        )
        v4 = 0 if ":" not in ip else 1
        return (1 if private else 0, v4)

    if not candidates:
        return None
    ip = sorted(set(candidates), key=_score)[0]
    host = f"[{ip}]" if ":" in ip else ip
    # Omit default http port for cleaner links.
    if int(port) == 80:
        return f"http://{host}"
    return f"http://{host}:{int(port)}"


def _admin_url(host: str, port: int) -> str:
    # Prefer explicit public URL for server deployments.
    public = (getattr(_config, "PUBLIC_BASE_URL", "") or "").strip().rstrip("/")
    if not public:
        public = _detect_public_base_url(port) or ""
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


def _reload_enabled() -> bool:
    """Dev-only hot reload. Off by default; multi-worker production stays stable."""
    try:
        from config import RELOAD

        return bool(RELOAD)
    except Exception:
        return (os.getenv("GROK2API_RELOAD") or "").strip().lower() in (
            "1",
            "true",
            "yes",
            "on",
        )


def _reload_kwargs() -> dict:
    """Build uvicorn reload options (dirs / include / exclude globs)."""
    root = Path(__file__).resolve().parent
    try:
        from config import RELOAD_DIRS, RELOAD_EXCLUDES, RELOAD_INCLUDES
    except Exception:
        RELOAD_DIRS = (os.getenv("GROK2API_RELOAD_DIRS") or "").strip()
        RELOAD_INCLUDES = (os.getenv("GROK2API_RELOAD_INCLUDES") or "").strip()
        RELOAD_EXCLUDES = (os.getenv("GROK2API_RELOAD_EXCLUDES") or "").strip()

    def _split(raw: str) -> list[str]:
        return [p.strip() for p in str(raw or "").split(",") if p.strip()]

    dirs = _split(RELOAD_DIRS)
    if not dirs:
        # Watch code + admin UI sources; skip data/logs/venv noise.
        dirs = [
            str(root),
            str(root / "store"),
            str(root / "static" / "js"),
            str(root / "static" / "admin"),
            str(root / "grok-build-auth"),
        ]
    else:
        resolved = []
        for d in dirs:
            p = Path(d)
            if not p.is_absolute():
                p = root / p
            resolved.append(str(p))
        dirs = resolved

    includes = _split(RELOAD_INCLUDES) or [
        "*.py",
        "*.html",
        "*.js",
        "*.css",
        "*.json",
    ]
    excludes = _split(RELOAD_EXCLUDES) or [
        "*/__pycache__/*",
        "*.pyc",
        "*/.git/*",
        "*/data/*",
        "*/static/dist/*",
        "*/turnstile-solver/logs/*",
        "*/.venv/*",
        "*/venv/*",
    ]
    return {
        "reload": True,
        "reload_dirs": dirs,
        "reload_includes": includes,
        "reload_excludes": excludes,
    }


def main() -> None:
    import socket

    import uvicorn

    from config import WORKERS

    host = _pick_listen_host()
    port = PORT
    reload_on = _reload_enabled()
    # uvicorn cannot combine reload with multi-worker; force 1 worker in dev.
    if reload_on:
        workers = 1
    else:
        # Honor explicit GROK2API_WORKERS (including 1) for low-RAM hosts.
        # Unset / invalid still defaults to at least 2 via config._default_workers.
        try:
            workers = max(1, int(WORKERS or 2))
        except (TypeError, ValueError):
            workers = 2
    # On Linux servers / headless, don't auto-open browser by default
    default_open = "0" if (os.name != "nt" and not os.environ.get("DISPLAY") and not os.environ.get("WAYLAND_DISPLAY")) else "1"
    open_browser = os.getenv("GROK2API_OPEN_BROWSER", default_open) not in (
        "0",
        "false",
        "False",
        "no",
    )

    # High-concurrency mode: Redis + PostgreSQL are mandatory (fail closed).
    try:
        from store import require_high_concurrency_stores

        require_high_concurrency_stores()
    except Exception as e:  # noqa: BLE001
        print(f"ERROR: high-concurrency store check failed: {e}")
        print(
            "  Hint: docker compose --profile store up -d\n"
            "        pip install -r requirements-store.txt\n"
            "        export REDIS_URL=redis://127.0.0.1:6379/0\n"
            "        export DATABASE_URL=postgresql://grok2api:grok2api@127.0.0.1:5432/grok2api\n"
            "        python migrate_json_to_pg.py --data-dir ./data"
        )
        raise SystemExit(2) from e

    # Fixed port bind (no auto-pick) so multi-worker parent shares one listen port.
    if os.getenv("GROK2API_PORT") is None:
        # Still allow override via busy-port only when explicitly single-worker emergency.
        pass

    # Keep admin API status / guide URLs in sync with actual bind
    _config.HOST = host
    _config.PORT = port

    configured_public = (
        getattr(_config, "PUBLIC_BASE_URL", "") or ""
    ).strip().rstrip("/")
    detected_public = None if configured_public else _detect_public_base_url(port)
    public = configured_public or detected_public or ""
    admin = _admin_url(host, port)
    if public:
        link_base = public
    elif host in ("0.0.0.0", "::", "127.0.0.1", "localhost"):
        # Bind-all on a server: print both bind and local loopback convenience links.
        link_base = f"http://127.0.0.1:{port}"
    else:
        link_base = f"http://{host}:{port}"
    print(f"grokcli-2api v{APP_VERSION} listening on http://{host}:{port}")
    print(f"  workers:            {workers}" + (" (forced by reload)" if reload_on else ""))
    print(f"  hot-reload:         {'ON  (dev only)' if reload_on else 'off'}")
    print(f"  OpenAI base_url:    {link_base}/v1")
    print(f"  Anthropic messages: {link_base}/v1/messages")
    print(f"  Admin console:      {admin}")
    print(f"  Docs:               {link_base}/docs")
    print(f"  Health:             {link_base}/health")
    if configured_public:
        print(f"  Public base URL:    {configured_public} (configured)")
    elif detected_public:
        print(f"  Public base URL:    {detected_public} (auto-detected)")
        print("  Admin/API links also follow request Host / X-Forwarded-* headers")
    elif host in ("0.0.0.0", "::"):
        print("  Tip: set GROK2API_PUBLIC_BASE_URL=https://your.domain if auto-detect is wrong")
    print(f"  Upstream:           {UPSTREAM_BASE}")
    if reload_on:
        print("  mode:               dev hot-reload (single worker + file watch)")
        print("  note:               set GROK2API_RELOAD=0 for multi-worker production")
    else:
        print("  mode:               high-concurrency (multi-worker + Redis + PostgreSQL)")
        print("  note:               only leader process runs token/model maintainers")

    run_kwargs: dict = {
        "app": "app:app",
        "host": host,
        "port": port,
        "workers": workers,
        "limit_concurrency": int(os.getenv("GROK2API_LIMIT_CONCURRENCY", "2000") or 2000),
        "timeout_keep_alive": int(os.getenv("GROK2API_KEEPALIVE", "30") or 30),
    }
    if reload_on:
        run_kwargs.update(_reload_kwargs())
        # workers must stay 1 when reload is on
        run_kwargs["workers"] = 1
    else:
        run_kwargs["reload"] = False

    uvicorn.run(**run_kwargs)


if __name__ == "__main__":
    main()
