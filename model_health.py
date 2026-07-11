"""Per-account model probe + periodic error check.

- Manual probe for a single account (admin UI)
- Background worker: periodically probe each live account; on hard errors
  block model / disable account and record last_probe on pool meta
"""

from __future__ import annotations

import os
import re
import threading
import time
from concurrent.futures import ThreadPoolExecutor, as_completed
from typing import Any

import httpx

from auth import GrokCredentials, list_live_credentials, load_credentials_by_id, upstream_headers
from config import (
    DEFAULT_MODEL,
    MODEL_HEALTH_AUTO_DISABLE,
    MODEL_HEALTH_INTERVAL,
    MODEL_HEALTH_STARTUP_DELAY,
    MODEL_PROBE_BATCH,
    MODEL_PROBE_WORKERS,
    PROBE_MODELS,
    UPSTREAM_BASE,
)

_PROBE_TIMEOUT = 45.0

# Background worker state
_stop = threading.Event()
_thread: threading.Thread | None = None
_wakeup = threading.Event()
_last_run: dict[str, Any] = {}
_lock = threading.RLock()

# Hard signals that this account cannot use the requested model
_MODEL_UNAVAILABLE_RE = re.compile(
    r"("
    r"model[_ -]?not[_ -]?found|"
    r"model[_ -]?not[_ -]?available|"
    r"model[_ -]?unavailable|"
    r"unknown[_ -]?model|"
    r"does\s+not\s+(?:have\s+)?access|"
    r"not\s+(?:allowed|authorized|permitted)\s+to\s+use|"
    r"no\s+access\s+to\s+(?:this\s+)?model|"
    r"unsupported[_ -]?model|"
    r"invalid[_ -]?model|"
    r"model[_ -]?is[_ -]?not[_ -]?supported|"
    r"not\s+supported\s+for\s+(?:this\s+)?model|"
    r"subscription\s+required|"
    r"need\s+a\s+(?:grok\s+)?subscription|"
    r"plan\s+does\s+not\s+include|"
    r"not\s+available\s+(?:for|on)\s+your|"
    r"access[_ -]?denied|"
    r"forbidden.*model|"
    r"model[_ -]?access[_ -]?denied|"
    r"cannot\s+use\s+(?:this\s+)?model|"
    r"disabled\s+model|"
    r"model\s+disabled|"
    r"free-usage-exhausted|"
    r"usage[_ -]?exhausted|"
    r"used\s+all\s+the\s+included"
    r")",
    re.IGNORECASE,
)

# Account-wide hard blocks (stop all scheduling)
_ACCOUNT_BLOCK_RE = re.compile(
    r"("
    r"user[_ -]?blocked|"
    r"account[_ -]?blocked|"
    r"account[_ -]?suspended|"
    r"account[_ -]?disabled|"
    r"personal-team-blocked|"
    r"need\s+a\s+grok\s+subscription|"
    r"run\s+out\s+of\s+credits|"
    r"out\s+of\s+credits|"
    r"usage[_ -]?limit[_ -]?reached|"
    r"usage[_ -]?pool[_ -]?exhausted"
    r")",
    re.IGNORECASE,
)


def is_model_unavailable_error(
    error: str | None, status_code: int | None = None
) -> bool:
    text = (error or "").strip()
    if not text:
        return False
    if _MODEL_UNAVAILABLE_RE.search(text):
        return True
    if status_code in (403, 404) and re.search(r"\bmodel\b", text, re.I):
        return True
    return False


def is_account_block_error(
    error: str | None, status_code: int | None = None
) -> bool:
    text = (error or "").strip()
    if not text:
        return False
    if _ACCOUNT_BLOCK_RE.search(text):
        return True
    return False


def handle_upstream_error_for_model(
    account_id: str | None,
    *,
    model: str | None = None,
    error: str = "",
    status_code: int | None = None,
) -> dict[str, Any] | None:
    """
    On upstream failure: block model (or whole account) from scheduling
    when the error indicates the model / account is unusable.
    """
    if not account_id or not MODEL_HEALTH_AUTO_DISABLE:
        return None

    import account_pool

    if is_account_block_error(error, status_code):
        reason = f"账号不可用 (HTTP {status_code}): {(error or '')[:120]}"
        return account_pool.disable_for_quota(
            account_id, reason=reason, source="model_health"
        )

    if model and is_model_unavailable_error(error, status_code):
        reason = f"模型不可用 (HTTP {status_code}): {(error or '')[:160]}"
        return account_pool.block_model(
            account_id,
            model,
            reason=reason,
            source="upstream_error",
        )
    return None


def _save_last_probe(account_id: str | None, result: dict[str, Any], *, overwrite: bool = True) -> None:
    """Persist probe snapshot on pool meta for admin UI."""
    if not account_id:
        return
    try:
        from settings_store import get_account_pool_state, save_account_pool_state

        state = get_account_pool_state()
        meta = state.get(account_id) or {}
        if not isinstance(meta, dict):
            meta = {}
        snap = {
            "ok": bool(result.get("ok")),
            "available": bool(result.get("available")),
            "model": result.get("model"),
            "status_code": result.get("status_code"),
            "error": (result.get("error") or "")[:400] or None,
            "probed_at": result.get("probed_at") or time.time(),
            "source": result.get("source") or "manual",
            "auto_disabled": bool(result.get("auto_disabled")),
            "stream_ok": result.get("stream_ok"),
        }
        # Only update last_probe if it's an explicit probe, or if there is no
        # existing probe snapshot. API call failures must not overwrite the
        # admin/model-health probe display.
        existing = meta.get("last_probe")
        if overwrite or not existing:
            meta["last_probe"] = snap
        if not snap["available"] and snap.get("error") and overwrite:
            meta["last_error"] = f"[probe {snap.get('model')}] {snap['error']}"[:300]
        elif snap["available"]:
            # clear probe-sourced last_error prefix only if success
            le = meta.get("last_error") or ""
            if isinstance(le, str) and le.startswith("[probe "):
                meta.pop("last_error", None)
        state[account_id] = meta
        save_account_pool_state(state)
    except Exception:
        pass


def probe_model_for_creds(
    creds: GrokCredentials,
    model: str,
    *,
    auto_disable: bool | None = None,
    source: str = "manual",
    report_stats: bool = True,
) -> dict[str, Any]:
    """
    Lightweight chat probe to verify the account can use `model`.
    On hard failure + auto_disable, blocks model / disables account.
    Always writes last_probe onto pool meta.
    """
    if auto_disable is None:
        auto_disable = MODEL_HEALTH_AUTO_DISABLE

    t0 = time.time()
    base: dict[str, Any] = {
        "ok": False,
        "available": False,
        "account_id": creds.auth_key,
        "email": creds.email,
        "user_id": creds.user_id,
        "model": model,
        "probed_at": t0,
        "source": source,
    }
    url = f"{UPSTREAM_BASE}/chat/completions"
    headers = upstream_headers(creds.token, model)
    headers["Accept"] = "text/event-stream, application/json"
    body = {
        "model": model,
        "messages": [{"role": "user", "content": "ping"}],
        "stream": True,
        "max_tokens": 8,
        "max_completion_tokens": 8,
    }
    try:
        with httpx.Client(timeout=_PROBE_TIMEOUT) as client:
            with client.stream("POST", url, headers=headers, json=body) as resp:
                status = resp.status_code
                if status >= 400:
                    err_text = (resp.read()).decode("utf-8", errors="replace")[:800]
                    base["status_code"] = status
                    base["error"] = err_text
                    base["available"] = False
                    base["latency_ms"] = int((time.time() - t0) * 1000)
                    if report_stats and creds.auth_key:
                        try:
                            import account_pool

                            account_pool.report_failure(
                                creds.auth_key,
                                error=err_text,
                                status_code=status,
                                model=model,
                            )
                        except Exception:
                            pass
                    if auto_disable:
                        action = handle_upstream_error_for_model(
                            creds.auth_key,
                            model=model,
                            error=err_text,
                            status_code=status,
                        )
                        if action:
                            base["auto_action"] = {
                                "enabled": action.get("enabled"),
                                "disabled_for_quota": action.get("disabled_for_quota"),
                                "blocked_model_ids": action.get("blocked_model_ids"),
                                "disabled_reason": action.get("disabled_reason"),
                            }
                            base["auto_disabled"] = bool(
                                action.get("enabled") is False
                                or action.get("blocked_models")
                                or action.get("disabled_for_quota")
                            )
                    _save_last_probe(creds.auth_key, base, overwrite=report_stats)
                    return base

                got_data = False
                for line in resp.iter_lines():
                    if not line:
                        continue
                    if isinstance(line, bytes):
                        line = line.decode("utf-8", errors="replace")
                    if line.startswith("data:"):
                        got_data = True
                        break
                base["ok"] = True
                base["available"] = True
                base["status_code"] = status
                base["stream_ok"] = got_data
                base["latency_ms"] = int((time.time() - t0) * 1000)
                if report_stats and creds.auth_key:
                    try:
                        import account_pool

                        account_pool.report_success(creds.auth_key)
                    except Exception:
                        pass
                if creds.auth_key:
                    try:
                        import account_pool

                        account_pool.unblock_model(creds.auth_key, model)
                    except Exception:
                        pass
                _save_last_probe(creds.auth_key, base, overwrite=report_stats)
                return base
    except httpx.HTTPError as e:
        base["error"] = f"network: {e}"
        base["latency_ms"] = int((time.time() - t0) * 1000)
        if report_stats and creds.auth_key:
            try:
                import account_pool

                account_pool.report_failure(
                    creds.auth_key, error=base["error"], status_code=502, model=model
                )
            except Exception:
                pass
        _save_last_probe(creds.auth_key, base, overwrite=report_stats)
        return base
    except Exception as e:  # noqa: BLE001
        base["error"] = str(e)[:300]
        base["latency_ms"] = int((time.time() - t0) * 1000)
        if report_stats and creds.auth_key:
            try:
                import account_pool

                account_pool.report_failure(
                    creds.auth_key, error=base["error"], status_code=502, model=model
                )
            except Exception:
                pass
        _save_last_probe(creds.auth_key, base, overwrite=report_stats)
        return base


async def probe_model_for_creds_async(
    creds: GrokCredentials,
    model: str,
    *,
    auto_disable: bool | None = None,
    source: str = "manual",
) -> dict[str, Any]:
    import asyncio

    return await asyncio.to_thread(
        probe_model_for_creds,
        creds,
        model,
        auto_disable=auto_disable,
        source=source,
    )


def probe_single_account(
    account_id: str,
    model: str | None = None,
    *,
    auto_disable: bool | None = None,
    source: str = "manual",
) -> dict[str, Any]:
    """Probe one account with one model (default DEFAULT / PROBE_MODELS[0])."""
    model = (model or (PROBE_MODELS[0] if PROBE_MODELS else DEFAULT_MODEL)).strip()
    creds = load_credentials_by_id(account_id)
    result = probe_model_for_creds(
        creds, model, auto_disable=auto_disable, source=source
    )
    return {
        "ok": bool(result.get("available")),
        "account_id": result.get("account_id") or account_id,
        "email": result.get("email") or creds.email,
        "result": result,
    }


def _unique_live_creds(*, auto_refresh: bool = False) -> list[GrokCredentials]:
    """De-dupe live credentials. Default auto_refresh=False avoids startup storms."""
    all_c = list_live_credentials(include_expired=False, auto_refresh=auto_refresh)
    seen: set[str] = set()
    out: list[GrokCredentials] = []
    for c in all_c:
        uid = c.user_id or c.auth_key or ""
        if uid in seen:
            continue
        seen.add(uid)
        out.append(c)
    return out


def probe_account_models(
    account_id: str | None = None,
    models: list[str] | None = None,
    *,
    auto_disable: bool | None = None,
    source: str = "manual",
    max_workers: int | None = None,
    max_accounts: int | None = None,
) -> dict[str, Any]:
    """Probe one or all accounts for model availability (concurrency-capped)."""
    models = models or list(PROBE_MODELS) or [DEFAULT_MODEL]
    if account_id:
        creds_list = [load_credentials_by_id(account_id)]
        deferred = 0
    else:
        # Do NOT auto-refresh all tokens here — token_maintainer owns that path.
        creds_list = _unique_live_creds(auto_refresh=False)
        deferred = 0
        # Background cycles batch; manual all can go larger but still hard-capped
        if max_accounts is None:
            max_accounts = (
                MODEL_PROBE_BATCH if source == "background" else MODEL_PROBE_BATCH * 2
            )
        if max_accounts and len(creds_list) > max_accounts:
            deferred = len(creds_list) - max_accounts
            # Prefer accounts without a recent successful probe
            def _probe_age(c: GrokCredentials) -> float:
                try:
                    from settings_store import get_account_pool_state

                    meta = (get_account_pool_state().get(c.auth_key or "") or {})
                    lp = meta.get("last_probe") if isinstance(meta, dict) else None
                    if isinstance(lp, dict) and lp.get("probed_at"):
                        return float(lp["probed_at"])
                except Exception:
                    pass
                return 0.0

            creds_list = sorted(creds_list, key=_probe_age)[:max_accounts]

    results: list[dict[str, Any]] = []

    def _probe_one(args: tuple[GrokCredentials, str]) -> dict[str, Any]:
        creds, model = args
        return probe_model_for_creds(
            creds, model, auto_disable=auto_disable, source=source
        )

    tasks = [(creds, model) for creds in creds_list for model in models]
    workers = max_workers if max_workers is not None else MODEL_PROBE_WORKERS
    workers = min(int(workers), max(1, len(tasks))) if tasks else 1
    if tasks:
        with ThreadPoolExecutor(
            max_workers=workers, thread_name_prefix="model-probe-"
        ) as ex:
            for fut in as_completed(ex.submit(_probe_one, t) for t in tasks):
                try:
                    results.append(fut.result())
                except Exception as e:  # noqa: BLE001
                    results.append({
                        "ok": False,
                        "available": False,
                        "error": str(e)[:300],
                        "source": source,
                        "probed_at": time.time(),
                    })

    available = sum(1 for r in results if r.get("available"))
    blocked = sum(
        1 for r in results if not r.get("available") and r.get("auto_disabled")
    )
    return {
        "ok": True,
        "probed_at": time.time(),
        "models": models,
        "count": len(results),
        "available_count": available,
        "unavailable_count": len(results) - available,
        "auto_action_count": blocked,
        "deferred": deferred,
        "workers": workers,
        "results": results,
        "source": source,
    }


def probe_all_accounts_concurrent(
    models: list[str] | None = None,
    *,
    auto_disable: bool | None = None,
    source: str = "manual",
    max_workers: int | None = None,
    max_accounts: int | None = None,
) -> dict[str, Any]:
    """Probe accounts concurrently (admin UI "全部模型探测") with hard caps."""
    if max_workers is None:
        max_workers = MODEL_PROBE_WORKERS
    # Reuse batched probe_account_models for consistent limits
    return probe_account_models(
        None,
        models,
        auto_disable=auto_disable,
        source=source,
        max_workers=max_workers,
        max_accounts=max_accounts,
    )


# ── Background periodic checker ─────────────────────────────────────────────


def _interval() -> float:
    try:
        # 0 = disabled (on-demand only)
        v = float(os.getenv("GROK2API_MODEL_HEALTH_INTERVAL", str(MODEL_HEALTH_INTERVAL)))
        return max(0.0, v)
    except ValueError:
        return float(MODEL_HEALTH_INTERVAL)


def run_once(*, source: str = "background") -> dict[str, Any]:
    """Probe every live account with PROBE_MODELS (error check cycle)."""
    result = probe_account_models(
        None, list(PROBE_MODELS) or [DEFAULT_MODEL], auto_disable=True, source=source
    )
    with _lock:
        _last_run.clear()
        _last_run.update(result)
        _last_run["at"] = time.time()
    bad = [r for r in result.get("results") or [] if not r.get("available")]
    if bad:
        print(
            f"  [model-health] cycle: {result.get('available_count')}/"
            f"{result.get('count')} ok; "
            f"{len(bad)} error(s) — auto_action={result.get('auto_action_count')}"
        )
    return result


def request_run_soon() -> None:
    _wakeup.set()


def _startup_delay() -> float:
    try:
        return max(15.0, float(MODEL_HEALTH_STARTUP_DELAY))
    except Exception:
        return 90.0


def _worker() -> None:
    # Stagger well after token maintainer so we never double-fan-out on boot
    # (700 accounts × probe was freezing WSL via thread/network peak).
    if _stop.wait(_startup_delay()):
        return
    while not _stop.is_set():
        interval = _interval()
        if interval <= 0:
            # disabled: sleep long, only run on wakeup
            _wakeup.clear()
            triggered = _wakeup.wait(timeout=3600.0)
            if _stop.is_set():
                break
            if triggered:
                run_once(source="manual_all")
            continue
        try:
            run_once(source="background")
        except Exception as e:  # noqa: BLE001
            with _lock:
                _last_run.clear()
                _last_run.update({"ok": False, "error": str(e)[:400], "at": time.time()})
            print(f"  [model-health] cycle error: {e}")
        _wakeup.clear()
        triggered = _wakeup.wait(timeout=interval)
        if _stop.is_set():
            break
        if triggered:
            try:
                run_once(source="manual_all")
            except Exception as e:  # noqa: BLE001
                print(f"  [model-health] forced cycle error: {e}")


def start_background() -> None:
    global _thread
    try:
        from config import MODEL_HEALTH
    except Exception:
        MODEL_HEALTH = True
    if not MODEL_HEALTH:
        return
    if _thread and _thread.is_alive():
        return
    _stop.clear()
    _thread = threading.Thread(
        target=_worker, name="g2a-model-health", daemon=True
    )
    _thread.start()


def stop_background() -> None:
    _stop.set()
    _wakeup.set()


def status() -> dict[str, Any]:
    interval = _interval()
    try:
        from config import MODEL_HEALTH
    except Exception:
        MODEL_HEALTH = True
    return {
        "running": bool(_thread and _thread.is_alive()),
        "enabled": bool(MODEL_HEALTH),
        "interval_sec": interval,
        "startup_delay_sec": _startup_delay(),
        "probe_workers": MODEL_PROBE_WORKERS,
        "probe_batch": MODEL_PROBE_BATCH,
        "probe_models": list(PROBE_MODELS) or [DEFAULT_MODEL],
        "auto_disable": MODEL_HEALTH_AUTO_DISABLE,
        "last": dict(_last_run) if _last_run else None,
    }
