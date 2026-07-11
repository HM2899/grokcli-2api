"""Multi-account pool: rotation, enable/disable, cooldown, failover stats.

All accounts are equal — there is no primary/preferred account.
"""

from __future__ import annotations

import asyncio
import random
import threading
import time
from typing import Any

from auth import AuthError, GrokCredentials, list_live_credentials, load_credentials_by_id
from settings_store import (
    get_account_mode,
    get_account_pool_state,
    save_account_pool_state,
    touch_account_stats,
)

# Modes (all accounts treated equally):
#   round_robin  — cycle all enabled live accounts
#   random       — pick randomly among enabled live accounts
#   least_used   — prefer account with fewest requests
VALID_MODES = ("round_robin", "random", "least_used")

# Default cooldown after 401 / 429 / 5xx (seconds)
DEFAULT_COOLDOWN = 60
AUTH_COOLDOWN = 300  # longer for hard auth failures

_lock = threading.RLock()
_rr_index = 0


def _now() -> float:
    return time.time()


def _pool_meta(account_id: str, state: dict[str, Any]) -> dict[str, Any]:
    meta = state.get(account_id) or {}
    blocked = meta.get("blocked_models") or {}
    if not isinstance(blocked, dict):
        blocked = {}
    return {
        "enabled": bool(meta.get("enabled", True)),
        "weight": max(1, int(meta.get("weight") or 1)),
        "request_count": int(meta.get("request_count") or 0),
        "success_count": int(meta.get("success_count") or 0),
        "fail_count": int(meta.get("fail_count") or 0),
        "last_used_at": meta.get("last_used_at"),
        "last_error": meta.get("last_error"),
        "cooldown_until": meta.get("cooldown_until"),
        "disabled_for_quota": bool(meta.get("disabled_for_quota")),
        "disabled_reason": meta.get("disabled_reason"),
        "quota_disabled_at": meta.get("quota_disabled_at"),
        "quota_source": meta.get("quota_source"),
        "last_quota": meta.get("last_quota"),
        "last_probe": meta.get("last_probe"),
        "blocked_models": blocked,
        "blocked_model_ids": list(blocked.keys()),
    }


def is_model_blocked(account_id: str, model: str | None, state: dict[str, Any] | None = None) -> bool:
    """True if this account must not be scheduled for `model`."""
    if not account_id or not model:
        return False
    if state is None:
        state = get_account_pool_state()
    meta = _pool_meta(account_id, state)
    blocked = meta.get("blocked_models") or {}
    return model in blocked


def is_in_cooldown(meta: dict[str, Any]) -> bool:
    until = meta.get("cooldown_until")
    if until is None:
        return False
    try:
        return _now() < float(until)
    except (TypeError, ValueError):
        return False


def list_pool_accounts() -> list[dict[str, Any]]:
    """Live credentials merged with pool metadata (for admin UI).

    Listing/status calls must never perform OIDC network refreshes: on a large
    account pool a single expired token used to synchronously block Uvicorn for
    tens of seconds. Token renewal belongs to the explicit maintenance task or
    request-time account selection, not admin/health reads.
    """
    state = get_account_pool_state()
    out: list[dict[str, Any]] = []
    for creds in list_live_credentials(include_expired=True, auto_refresh=False):
        meta = _pool_meta(creds.auth_key or "", state)
        out.append(
            {
                "id": creds.auth_key,
                "email": creds.email,
                "user_id": creds.user_id,
                "team_id": creds.team_id,
                "expires_at": creds.expires_at,
                "expired": creds.expired,
                "has_refresh_token": bool(creds.refresh_token),
                "token_hint": _mask(creds.token),
                **meta,
                "in_cooldown": is_in_cooldown(meta),
            }
        )
    return out


def _mask(token: str | None) -> str:
    if not token:
        return ""
    if len(token) <= 12:
        return "****"
    return token[:6] + "..." + token[-4:]


def _eligible(
    creds: GrokCredentials,
    state: dict[str, Any],
    *,
    model: str | None = None,
) -> bool:
    if creds.expired:
        return False
    aid = creds.auth_key or ""
    meta = _pool_meta(aid, state)
    if not meta["enabled"]:
        return False
    if is_in_cooldown(meta):
        return False
    if model and is_model_blocked(aid, model, state):
        return False
    return True


def _pick_round_robin(eligible: list[GrokCredentials]) -> GrokCredentials:
    global _rr_index
    with _lock:
        if not eligible:
            raise AuthError("No eligible accounts for round-robin")
        idx = _rr_index % len(eligible)
        _rr_index = (idx + 1) % len(eligible)
        return eligible[idx]


def _pick_random(eligible: list[GrokCredentials], state: dict[str, Any]) -> GrokCredentials:
    weights = []
    for c in eligible:
        meta = _pool_meta(c.auth_key or "", state)
        weights.append(meta["weight"])
    return random.choices(eligible, weights=weights, k=1)[0]


def _pick_least_used(eligible: list[GrokCredentials], state: dict[str, Any]) -> GrokCredentials:
    def score(c: GrokCredentials) -> tuple[int, float]:
        meta = _pool_meta(c.auth_key or "", state)
        return (meta["request_count"], float(meta["last_used_at"] or 0))

    return min(eligible, key=score)


_last_normalize_at = 0.0
_NORMALIZE_MIN_INTERVAL = 30.0  # avoid re-scanning auth.json every request


def _ensure_multi_account_layout() -> None:
    """Re-key CLI client_id single-slot into per-user keys (throttled)."""
    global _last_normalize_at
    now = time.time()
    if now - _last_normalize_at < _NORMALIZE_MIN_INTERVAL:
        return
    try:
        from oidc_auth import normalize_auth_file_keys

        normalize_auth_file_keys()
        _last_normalize_at = now
    except Exception:
        pass


def acquire(
    exclude: set[str] | None = None,
    *,
    model: str | None = None,
    auto_refresh: bool = True,
) -> GrokCredentials:
    """
    Select next account according to configured mode.
    `exclude` skips already-tried accounts in a failover pass.
    `model` skips accounts that blocked this model as unavailable.
    Auto-refreshes near-expiry tokens via refresh_token when available.
    """
    exclude = exclude or set()
    mode = get_account_mode()
    if mode not in VALID_MODES:
        mode = "round_robin"

    _ensure_multi_account_layout()

    # Request paths may opt in to a tiny on-demand renewal. Read-only routes
    # pass False so an expired account cannot block the event loop on OIDC.
    all_live = list_live_credentials(include_expired=False, auto_refresh=auto_refresh)
    if not all_live:
        raise AuthError(
            "No live accounts in auth store. "
            "Use device-code login, import token/auth.json, "
            "or add more accounts to the pool."
        )

    state = get_account_pool_state()
    candidates = [c for c in all_live if (c.auth_key or "") not in exclude]

    eligible = [c for c in candidates if _eligible(c, state, model=model)]
    # If everything is cooling down, relax cooldown and still try
    # (but still respect model blocks + enabled)
    if not eligible:
        eligible = [
            c
            for c in candidates
            if not c.expired
            and _pool_meta(c.auth_key or "", state)["enabled"]
            and not (model and is_model_blocked(c.auth_key or "", model, state))
        ]
    if not eligible:
        msg = "No eligible accounts (all disabled, expired, excluded"
        if model:
            msg += f", or blocked for model `{model}`"
        msg += "). Enable accounts, clear model blocks, or re-login."
        raise AuthError(msg)

    if mode == "round_robin":
        return _pick_round_robin(eligible)
    if mode == "random":
        return _pick_random(eligible, state)
    if mode == "least_used":
        return _pick_least_used(eligible, state)
    return eligible[0]


def report_success(account_id: str | None) -> None:
    if not account_id:
        return
    touch_account_stats(
        account_id,
        success=True,
        clear_cooldown=True,
    )


def report_failure(
    account_id: str | None,
    *,
    error: str = "",
    status_code: int | None = None,
    cooldown: float | None = None,
    model: str | None = None,
) -> None:
    if not account_id:
        return
    if cooldown is None:
        if status_code == 401:
            cooldown = AUTH_COOLDOWN
        elif status_code in (429, 503, 502):
            cooldown = DEFAULT_COOLDOWN
        else:
            cooldown = DEFAULT_COOLDOWN / 2
    until = _now() + float(cooldown)
    touch_account_stats(
        account_id,
        success=False,
        error=(error or "")[:300],
        cooldown_until=until,
    )
    # Hard quota/credit errors → remove from rotation immediately
    try:
        from quota import handle_upstream_error_for_quota

        handle_upstream_error_for_quota(
            account_id, error=error, status_code=status_code
        )
    except Exception:
        pass
    # Model unavailable → stop scheduling this account for that model ONLY if
    # the error clearly names this model. Do not let errors from other models
    # (e.g. a model-not-found for model A) block the account for model B.
    try:
        from model_health import handle_upstream_error_for_model, is_model_unavailable_error

        if model and is_model_unavailable_error(error, status_code):
            # extra guard: ensure the error text references this model id
            err_lower = (error or "").lower()
            if model.lower() in err_lower or f"model `{model}`" in err_lower:
                handle_upstream_error_for_model(
                    account_id, model=model, error=error, status_code=status_code
                )
    except Exception:
        pass


def set_account_enabled(account_id: str, enabled: bool) -> dict[str, Any] | None:
    state = get_account_pool_state()
    # ensure key exists even if new
    meta = state.get(account_id) or {}
    meta["enabled"] = bool(enabled)
    if enabled:
        # Manual re-enable clears auto quota-disable + model blocks
        meta.pop("disabled_for_quota", None)
        meta.pop("disabled_reason", None)
        meta.pop("quota_disabled_at", None)
        meta.pop("quota_source", None)
        meta.pop("blocked_models", None)
    state[account_id] = meta
    save_account_pool_state(state)
    for a in list_pool_accounts():
        if a["id"] == account_id:
            return a
    return {"id": account_id, "enabled": enabled}


def block_model(
    account_id: str,
    model: str,
    *,
    reason: str = "模型不可用",
    source: str = "probe",
) -> dict[str, Any] | None:
    """Stop scheduling this account for a specific model."""
    if not account_id or not model:
        return None
    state = get_account_pool_state()
    meta = state.get(account_id) or {}
    if not isinstance(meta, dict):
        meta = {}
    blocked = meta.get("blocked_models")
    if not isinstance(blocked, dict):
        blocked = {}
    already = model in blocked
    blocked[model] = {
        "reason": (reason or "模型不可用")[:300],
        "blocked_at": _now(),
        "source": source,
    }
    meta["blocked_models"] = blocked
    meta["last_error"] = f"[{model}] {blocked[model]['reason']}"
    state[account_id] = meta
    save_account_pool_state(state)
    if not already:
        print(
            f"  [model] blocked {model} for account "
            f"{account_id}: {blocked[model]['reason']}"
        )
    for a in list_pool_accounts():
        if a["id"] == account_id:
            return a
    return {
        "id": account_id,
        "blocked_models": blocked,
        "model": model,
        "reason": blocked[model]["reason"],
    }


def unblock_model(account_id: str, model: str | None = None) -> dict[str, Any] | None:
    """Clear one model block, or all model blocks if model is None."""
    if not account_id:
        return None
    state = get_account_pool_state()
    meta = state.get(account_id) or {}
    if not isinstance(meta, dict):
        return None
    blocked = meta.get("blocked_models")
    if not isinstance(blocked, dict):
        blocked = {}
    if model is None:
        meta.pop("blocked_models", None)
    elif model in blocked:
        blocked = dict(blocked)
        blocked.pop(model, None)
        if blocked:
            meta["blocked_models"] = blocked
        else:
            meta.pop("blocked_models", None)
    state[account_id] = meta
    save_account_pool_state(state)
    for a in list_pool_accounts():
        if a["id"] == account_id:
            return a
    return {"id": account_id, "blocked_models": meta.get("blocked_models") or {}}


def disable_for_quota(
    account_id: str,
    *,
    reason: str = "额度已耗尽",
    source: str = "billing",
) -> dict[str, Any] | None:
    """Disable account permanently from rotation due to quota exhaustion."""
    state = get_account_pool_state()
    meta = state.get(account_id) or {}
    if not isinstance(meta, dict):
        meta = {}
    already = meta.get("enabled") is False and meta.get("disabled_for_quota")
    meta["enabled"] = False
    meta["disabled_for_quota"] = True
    meta["disabled_reason"] = (reason or "额度已耗尽")[:300]
    meta["quota_disabled_at"] = _now()
    meta["quota_source"] = source
    meta["last_error"] = meta["disabled_reason"]
    state[account_id] = meta
    save_account_pool_state(state)
    if not already:
        print(
            f"  [quota] account disabled from pool: "
            f"{account_id} — {meta['disabled_reason']}"
        )
    for a in list_pool_accounts():
        if a["id"] == account_id:
            return a
    return {
        "id": account_id,
        "enabled": False,
        "disabled_for_quota": True,
        "disabled_reason": meta["disabled_reason"],
    }


def save_quota_snapshot(account_id: str, quota_result: dict[str, Any]) -> None:
    """Cache last successful quota snapshot on pool meta (no secrets)."""
    if not account_id:
        return
    snap = {
        "fetched_at": quota_result.get("fetched_at") or _now(),
        "monthly_limit": quota_result.get("monthly_limit"),
        "used": quota_result.get("used"),
        "remaining": quota_result.get("remaining"),
        "usage_percent": quota_result.get("usage_percent"),
        "unlimited_or_free": quota_result.get("unlimited_or_free"),
        "exhausted": quota_result.get("exhausted"),
        "summary": (quota_result.get("display") or {}).get("summary"),
        "billing_period_end": quota_result.get("billing_period_end"),
    }
    state = get_account_pool_state()
    meta = state.get(account_id) or {}
    if not isinstance(meta, dict):
        meta = {}
    meta["last_quota"] = snap
    state[account_id] = meta
    save_account_pool_state(state)


def pool_summary() -> dict[str, Any]:
    accounts = list_pool_accounts()
    live = [a for a in accounts if not a.get("expired")]
    enabled = [a for a in live if a.get("enabled")]
    cooling = [a for a in enabled if a.get("in_cooldown")]
    quota_disabled = [a for a in accounts if a.get("disabled_for_quota")]
    model_blocked = [
        a for a in accounts if (a.get("blocked_model_ids") or a.get("blocked_models"))
    ]
    return {
        "mode": get_account_mode(),
        "total": len(accounts),
        "live": len(live),
        "enabled": len(enabled),
        "in_cooldown": len(cooling),
        "quota_disabled": len(quota_disabled),
        "model_blocked": len(model_blocked),
        "accounts": accounts,
    }


def try_acquire_sequence(
    max_attempts: int | None = None,
    *,
    model: str | None = None,
    prefer_account_id: str | None = None,
) -> list[GrokCredentials]:
    """
    Build an ordered list of accounts to try for one request (failover chain).
    Covers all enabled live accounts equally; skips model-blocked accounts.

    `prefer_account_id`: conversation affinity — put this account first so
    multi-turn chats stay on the same account (memory continuity).
    """
    _ensure_multi_account_layout()
    mode = get_account_mode()
    all_live = list_live_credentials(include_expired=False, auto_refresh=True)
    state = get_account_pool_state()
    enabled = [
        c
        for c in all_live
        if _pool_meta(c.auth_key or "", state)["enabled"]
        and not (model and is_model_blocked(c.auth_key or "", model, state))
    ]
    if not enabled:
        # fall back to all non-blocked enabled; if empty, all live (last resort)
        enabled = [
            c
            for c in all_live
            if not (model and is_model_blocked(c.auth_key or "", model, state))
        ]
    if not enabled:
        enabled = list(all_live)

    # De-dupe by user_id (legacy dual keys)
    seen_users: set[str] = set()
    deduped: list[GrokCredentials] = []
    for c in enabled:
        uid = c.user_id or c.auth_key or ""
        if uid in seen_users:
            continue
        seen_users.add(uid)
        deduped.append(c)
    enabled = deduped

    # sort: not cooling first, then by strategy bias
    def cool_key(c: GrokCredentials) -> tuple[int, int, float]:
        meta = _pool_meta(c.auth_key or "", state)
        cooling = 1 if is_in_cooldown(meta) else 0
        used = meta["request_count"]
        last = float(meta["last_used_at"] or 0)
        return (cooling, used if mode == "least_used" else 0, last)

    if mode == "random":
        ordered = list(enabled)
        random.shuffle(ordered)
        ordered.sort(key=lambda c: 1 if is_in_cooldown(_pool_meta(c.auth_key or "", state)) else 0)
    elif mode == "least_used":
        ordered = sorted(enabled, key=cool_key)
    else:  # round_robin — start from current RR head
        if not enabled:
            return []
        global _rr_index
        with _lock:
            start = _rr_index % len(enabled)
            _rr_index = (start + 1) % max(len(enabled), 1)
        rotated = enabled[start:] + enabled[:start]
        # non-cooling first, preserve RR order within each group
        not_cooling = [
            c
            for c in rotated
            if not is_in_cooldown(_pool_meta(c.auth_key or "", state))
        ]
        cooling = [
            c
            for c in rotated
            if is_in_cooldown(_pool_meta(c.auth_key or "", state))
        ]
        ordered = not_cooling + cooling

    # Conversation affinity: pin multi-turn chat to same account first
    if prefer_account_id and ordered:
        sticky: list[GrokCredentials] = []
        rest: list[GrokCredentials] = []
        pref = prefer_account_id
        for c in ordered:
            aid = c.auth_key or ""
            if aid == pref or c.user_id == pref or aid.endswith(f"::{pref}"):
                sticky.append(c)
            else:
                rest.append(c)
        if sticky:
            ordered = sticky + rest

    if max_attempts is not None:
        ordered = ordered[: max(1, max_attempts)]
    return ordered


def load_for_id(account_id: str) -> GrokCredentials:
    return load_credentials_by_id(account_id)


async def acquire_async(
    exclude: set[str] | None = None,
    *,
    model: str | None = None,
    auto_refresh: bool = True,
) -> GrokCredentials:
    """Event-loop-friendly wrapper for account selection."""
    return await asyncio.to_thread(acquire, exclude, model=model, auto_refresh=auto_refresh)


async def try_acquire_sequence_async(
    max_attempts: int | None = None,
    *,
    model: str | None = None,
    prefer_account_id: str | None = None,
) -> list[GrokCredentials]:
    """Event-loop-friendly wrapper for failover chain building."""
    return await asyncio.to_thread(
        try_acquire_sequence,
        max_attempts,
        model=model,
        prefer_account_id=prefer_account_id,
    )
