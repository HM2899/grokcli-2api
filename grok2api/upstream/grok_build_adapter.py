"""Adapter: grok-build-auth -> grokcli-2api account pool.

Drives the vendored ``grok-build-auth/xconsole_client`` protocol client to:

1. register an x.ai account with MoeMail + YesCaptcha
2. extract SSO/session cookies
3. convert SSO via sso_to_auth_json into a local auth.json entry
4. import that entry into the multi-account pool

Import of ``xconsole_client`` is deferred so the main API can start even when
optional deps are missing. Registration endpoints then return a clear error
instead of crashing process startup.

``grok-build-auth`` is vendored in-tree (not a git submodule).
Legacy browser (DrissionPage) and grpc-session registration engines were removed.
"""
from __future__ import annotations

import json
import os
import secrets
import sys
import threading
import time
import uuid
from pathlib import Path
from typing import Any

from grok2api.upstream.registration_admission import (
    AdmissionTimeout,
    RegistrationAdmission,
)

# This file lives at grok2api/upstream/; repo root is two levels up.
ROOT = Path(__file__).resolve().parents[2]
GBA = ROOT / "grok-build-auth"
DATA_DIR = ROOT / "data"
REGISTER_SSO_DIR = DATA_DIR / "register_sso"
ADAPTER_BUILD = "v1.9.96-reg-mt-captcha-success"
# Newly registered accounts often need a short settle window before probe.
REGISTER_PROBE_DELAY_SEC = float(
    os.environ.get("GROK2API_REG_PROBE_DELAY_SEC", "30") or 30
)

YESCAPTCHA_KEY = (
    os.environ.get("GROK2API_YESCAPTCHA_KEY")
    or os.environ.get("YESCAPTCHA_API_KEY")
    or ""
).strip()

CAPTCHA_PROVIDER = (
    os.environ.get("GROK2API_CAPTCHA_PROVIDER")
    or os.environ.get("CAPTCHA_PROVIDER")
    or "local"
).strip().lower()
if CAPTCHA_PROVIDER not in {"local", "yescaptcha"}:
    CAPTCHA_PROVIDER = "local"

LOCAL_SOLVER_URL = (
    os.environ.get("GROK2API_LOCAL_SOLVER_URL")
    or os.environ.get("LOCAL_SOLVER_URL")
    or os.environ.get("GROK2API_YESCAPTCHA_ENDPOINT")
    or os.environ.get("YESCAPTCHA_ENDPOINT")
    or "http://127.0.0.1:5072"
).strip().rstrip("/")

# Hard cap for multi-thread registration concurrency only (YesCaptcha + xAI rate limits).
# Batch count is intentionally uncapped — only concurrency bounds parallelism.
# Local Camoufox ≈ 200–400MB per browser; keep MAX <= TURNSTILE_THREAD_MAX.
try:
    MAX_CONCURRENCY = max(1, min(8, int(os.environ.get("GROK2API_REG_MAX_CONCURRENCY", "4") or 4)))
except (TypeError, ValueError):
    MAX_CONCURRENCY = 4
try:
    DEFAULT_CONCURRENCY = max(1, min(MAX_CONCURRENCY, int(os.environ.get("GROK2API_REG_CONCURRENCY", "3") or 3)))
except (TypeError, ValueError):
    DEFAULT_CONCURRENCY = 3
# Hard cap when captcha_provider=local. Default 3 so multi-thread reg can use a
# warm browser pool; override via GROK2API_REG_LOCAL_CONCURRENCY (1–6).
try:
    LOCAL_CAPTCHA_MAX_CONCURRENCY = max(
        1,
        min(6, int(os.environ.get("GROK2API_REG_LOCAL_CONCURRENCY", "3") or 3)),
    )
except (TypeError, ValueError):
    LOCAL_CAPTCHA_MAX_CONCURRENCY = 3

# When captcha_provider=local, registration must wait for the inline Turnstile
# Solver HTTP to answer before spawning workers. Prevents "already running but
# captcha always fails because solver is still booting / restarting".
LOCAL_SOLVER_WAIT_SEC = float(
    os.environ.get("GROK2API_LOCAL_SOLVER_WAIT_SEC", "120") or 120
)
LOCAL_SOLVER_POLL_SEC = float(
    os.environ.get("GROK2API_LOCAL_SOLVER_POLL_SEC", "1.0") or 1.0
)

# --------------------------------------------------------------------------- #
# session state
# --------------------------------------------------------------------------- #
_sessions: dict[str, dict[str, Any]] = {}
_batches: dict[str, dict[str, Any]] = {}
_lock = threading.RLock()
# batch_id -> True while a local ThreadPool spawner is alive in THIS process.
_active_batch_runners: dict[str, bool] = {}
# Local captcha: limit concurrent solves to the Camoufox browser pool size.
# RLock serialization used to force 1-at-a-time even with TURNSTILE_THREAD=4,
# killing multi-thread throughput. Semaphore lets N browsers work in parallel.
def _local_captcha_slots() -> int:
    # Align with TURNSTILE_THREAD when set, else LOCAL_CAPTCHA_MAX_CONCURRENCY.
    slots = LOCAL_CAPTCHA_MAX_CONCURRENCY
    for env_name in ("TURNSTILE_THREAD", "GROK2API_TURNSTILE_THREAD"):
        raw = (os.environ.get(env_name) or "").strip()
        if not raw:
            continue
        try:
            n = int(raw)
            if n > 0:
                slots = max(1, min(6, n, LOCAL_CAPTCHA_MAX_CONCURRENCY))
                break
        except (TypeError, ValueError):
            pass
    return max(1, min(6, int(slots)))


_local_captcha_sem = threading.Semaphore(_local_captcha_slots())
_local_captcha_slots_n = _local_captcha_slots()
# Cross-batch in-flight registration jobs (3 open batches * 8 workers used to
# stampede local Camoufox + xAI device-flow). Cap total concurrent registrations.
try:
    _GLOBAL_REG_INFLIGHT_MAX = max(
        1,
        min(
            8,
            int(os.environ.get("GROK2API_REG_GLOBAL_INFLIGHT", "4") or 4),
        ),
    )
except (TypeError, ValueError):
    _GLOBAL_REG_INFLIGHT_MAX = 4
_reg_admission = RegistrationAdmission(_GLOBAL_REG_INFLIGHT_MAX)
try:
    _REG_ADMISSION_TIMEOUT_SEC = max(
        30.0,
        min(
            1800.0,
            float(os.environ.get("GROK2API_REG_ADMISSION_TIMEOUT_SEC", "600") or 600),
        ),
    )
except (TypeError, ValueError):
    _REG_ADMISSION_TIMEOUT_SEC = 600.0
try:
    _REG_ADMISSION_HEARTBEAT_SEC = max(
        2.0,
        min(
            60.0,
            float(os.environ.get("GROK2API_REG_ADMISSION_HEARTBEAT_SEC", "10") or 10),
        ),
    )
except (TypeError, ValueError):
    _REG_ADMISSION_HEARTBEAT_SEC = 10.0
# Throttle Redis mirror writes: progress still updates local RAM every step;
# cross-worker / admin poll only needs ~100–200ms freshness.
_REG_MIRROR_MIN_INTERVAL_SEC = float(os.environ.get("GROK2API_REG_MIRROR_MIN_SEC", "0.12") or 0.12)
_REG_MIRROR_MIN_INTERVAL_SEC = max(0.04, min(1.0, _REG_MIRROR_MIN_INTERVAL_SEC))
_reg_mirror_last: dict[str, float] = {}
# Soft rate-limit signal: after device-flow / captcha storms, temporarily reduce
# new job admission without killing already-running workers.
_reg_soft_pause_until = 0.0
_reg_soft_pause_lock = threading.Lock()
# Mid-run self-heal: startup reclaim alone cannot rescue a dead runner while the
# process stays up. Watchdog refreshes Redis TTL and auto-resumes unfinished
# batches whose spawner disappeared mid-flight (common on multi-hour bulk jobs).
_reg_watchdog_started = False
_reg_watchdog_lock = threading.Lock()
_xconsole_ready = False
_xconsole_error: str | None = None


def _note_reg_pressure(reason: str = "", *, pause_sec: float | None = None) -> None:
    """Backoff new admissions briefly when upstream rate-limits hit."""
    global _reg_soft_pause_until
    try:
        sec = float(
            pause_sec
            if pause_sec is not None
            else (os.environ.get("GROK2API_REG_SOFT_PAUSE_SEC", "5") or 5)
        )
    except (TypeError, ValueError):
        sec = 8.0
    sec = max(1.0, min(60.0, sec))
    with _reg_soft_pause_lock:
        _reg_soft_pause_until = max(_reg_soft_pause_until, time.time() + sec)
    if reason:
        print(f"[registration] soft-pause {sec:.0f}s ({reason})")


def _reg_soft_pause_remaining() -> float:
    """Return seconds left in the process-wide registration soft pause."""

    with _reg_soft_pause_lock:
        return max(0.0, _reg_soft_pause_until - time.time())


def _wait_reg_admission(*, check_cancel=None, heartbeat=None) -> float:
    """Wait for global admission while remaining cancel- and health-friendly."""

    return _reg_admission.acquire(
        check_cancel=check_cancel,
        heartbeat=heartbeat,
        pause_remaining=_reg_soft_pause_remaining,
        timeout_seconds=_REG_ADMISSION_TIMEOUT_SEC,
        heartbeat_seconds=_REG_ADMISSION_HEARTBEAT_SEC,
    )


def _release_reg_admission() -> None:
    try:
        _reg_admission.release()
    except Exception:
        pass


def _release_reg_admission_once(flag: dict[str, bool]) -> None:
    """Idempotent admission release (safe to call from job + post-import probe)."""
    if not isinstance(flag, dict):
        return
    if flag.get("released"):
        return
    flag["released"] = True
    _release_reg_admission()


REG_BATCH_RUNNER_LOCK_TTL = int(os.environ.get("GROK2API_REG_RUNNER_LOCK_TTL", "90") or 90)
# How many jobs may be pre-created (mailbox + session) beyond the live concurrency
# cap. Keep small so stop/cancel doesn't waste dozens of mailboxes.
REG_PREFETCH_SLOTS = int(os.environ.get("GROK2API_REG_PREFETCH_SLOTS", "1") or 1)
try:
    # Default 25s so dead runners / orphan mid-captcha sessions are healed faster
    # without thrashing healthy workers (was 45s → felt "stuck" for a minute+).
    REG_WATCHDOG_SEC = max(
        10.0,
        min(
            600.0,
            float(os.environ.get("GROK2API_REG_WATCHDOG_SEC", "25") or 25),
        ),
    )
except (TypeError, ValueError):
    REG_WATCHDOG_SEC = 25.0
try:
    # Batch-level stale: resume dead runners sooner after process restart.
    REG_WATCHDOG_STALE_SEC = max(
        30.0,
        min(
            3600.0,
            float(os.environ.get("GROK2API_REG_WATCHDOG_STALE_SEC", "90") or 90),
        ),
    )
except (TypeError, ValueError):
    REG_WATCHDOG_STALE_SEC = 90.0


def _now() -> float:
    return time.time()


def _persist_registration_sso(
    *,
    sid: str,
    email: str = "",
    password: str = "",
    sso: str = "",
    batch_id: str | None = None,
) -> str:
    """Write one durable SSO backup file under data/register_sso/.

    Registration sessions are in-memory/Redis-TTL; account auth payload is the
    primary store. This file is a secondary backup for operators who want raw
    SSO cookies without querying the account store.
    """
    cookie = str(sso or "").strip()
    if not cookie:
        return ""
    try:
        REGISTER_SSO_DIR.mkdir(parents=True, exist_ok=True)
    except Exception:
        pass
    ts = time.strftime("%Y%m%d-%H%M%S", time.localtime())
    safe_email = "".join(
        ch if ch.isalnum() or ch in "._@+-" else "_"
        for ch in str(email or "unknown")
    )[:80]
    safe_sid = "".join(ch if ch.isalnum() or ch in "-_" else "_" for ch in str(sid or "x"))[:24]
    path = REGISTER_SSO_DIR / f"{ts}_{safe_email}_{safe_sid}.json"
    payload = {
        "session_id": sid,
        "batch_id": batch_id,
        "email": email,
        "password": password,
        "sso": cookie,
        "sso_cookie": cookie,
        "created_at": ts,
        "source": "register-email",
    }
    try:
        path.write_text(json.dumps(payload, ensure_ascii=False, indent=2), encoding="utf-8")
        return str(path)
    except Exception as e:  # noqa: BLE001
        print(f"[grok-build-auth] WARN: write SSO backup failed: {e}")
        return ""


def _reg_redis() -> bool:
    try:
        from grok2api.store.redis_client import redis_enabled

        return redis_enabled()
    except Exception:
        return False


def _batch_runner_lock_key(batch_id: str) -> str:
    try:
        from grok2api.store.redis_client import key

        return key("reg", "runner", batch_id)
    except Exception:
        return f"g2a:reg:runner:{batch_id}"


def _try_acquire_batch_runner(batch_id: str) -> tuple[bool, str | None]:
    """Claim exclusive spawner ownership for a batch (cross-worker).

    Returns (acquired, token). Local-process claim always required; Redis NX
    is used when available so multi-worker won't double-spawn.
    """
    bid = str(batch_id or "").strip()
    if not bid:
        return False, None
    with _lock:
        if _active_batch_runners.get(bid):
            return False, None
        _active_batch_runners[bid] = True
    token = f"{uuid.uuid4().hex}|{os.getpid()}|{_now():.0f}"
    if _reg_redis():
        try:
            from grok2api.store.redis_client import set_nx_ex, worker_id

            token = f"{worker_id()}|{os.getpid()}|{uuid.uuid4().hex[:10]}"
            ok = set_nx_ex(_batch_runner_lock_key(bid), token, REG_BATCH_RUNNER_LOCK_TTL)
            if not ok:
                with _lock:
                    _active_batch_runners.pop(bid, None)
                return False, None
        except Exception:
            # Fall through to local-only claim.
            pass
    return True, token


def _renew_batch_runner(batch_id: str, token: str | None) -> None:
    if not token or not _reg_redis():
        return
    try:
        from grok2api.store.redis_client import renew_if_owner

        renew_if_owner(_batch_runner_lock_key(batch_id), token, REG_BATCH_RUNNER_LOCK_TTL)
    except Exception:
        pass


def _release_batch_runner(batch_id: str, token: str | None) -> None:
    bid = str(batch_id or "").strip()
    with _lock:
        _active_batch_runners.pop(bid, None)
    if token and _reg_redis():
        try:
            from grok2api.store.redis_client import compare_and_delete

            compare_and_delete(_batch_runner_lock_key(bid), token)
        except Exception:
            pass



def _resolve_mail_credentials(
    *,
    mail_provider: str | None = None,
    moemail_api_key: str | None = None,
    moemail_base_url: str | None = None,
    domain: str | None = None,
    reg_config: dict[str, Any] | None = None,
) -> tuple[str, str, str, str]:
    """Return (provider, api_key, base_url, domain) for mailbox create.

    Adapter call sites historically pass a single moemail_api_key. When the
    batch was started as YYDS/GPTMail, that field is already the active key
    (set by Go merge / registration_service). Prefer explicit reg_config slots
    when present so resume stays correct after process restart.
    """
    cfg = reg_config if isinstance(reg_config, dict) else {}
    try:
        from grok2api.upstream.moemail import normalize_mail_provider as _nmp

        prov = _nmp(
            mail_provider or cfg.get("mail_provider"),
            base_url=str(moemail_base_url or cfg.get("moemail_base_url") or ""),
        )
    except Exception:
        prov = str(mail_provider or cfg.get("mail_provider") or "moemail").strip().lower() or "moemail"

    key = str(moemail_api_key or cfg.get("moemail_api_key") or "").strip()
    base = str(moemail_base_url or cfg.get("moemail_base_url") or "").strip()
    dom = str(domain if domain is not None else cfg.get("domain") or "").strip()

    if prov == "yyds":
        key = str(cfg.get("yyds_api_key") or key).strip()
        if cfg.get("yyds_domain") not in (None, ""):
            dom = str(cfg.get("yyds_domain") or "").strip()
        base = ""  # fixed host in moemail.py
    elif prov == "gptmail":
        key = str(cfg.get("gptmail_api_key") or key).strip()
        if cfg.get("gptmail_domain") not in (None, ""):
            dom = str(cfg.get("gptmail_domain") or "").strip()
        base = ""
    elif prov == "cfmail":
        key = str(cfg.get("cfmail_api_key") or key).strip()
        if cfg.get("cfmail_domain") not in (None, ""):
            dom = str(cfg.get("cfmail_domain") or "").strip()
        base = str(cfg.get("cfmail_base_url") or base).strip()
    elif prov == "cloudmail":
        # Cloud Mail (maillab/cloud-mail): admin_email:password credential +
        # self-hosted Worker origin. The active key/base were mirrored into
        # moemail_* slots by registration_service.start_job; on resume, also
        # consult the dedicated cloudmail_* slots in the reg_config snapshot.
        key = str(cfg.get("cloudmail_api_key") or key).strip()
        if cfg.get("cloudmail_domain") not in (None, ""):
            dom = str(cfg.get("cloudmail_domain") or "").strip()
        base = str(cfg.get("cloudmail_base_url") or base).strip()
    elif prov == "tempmail":
        # Free tier: no key. Optional Bearer key for Plus/Ultra.
        key = str(cfg.get("tempmail_api_key") or key).strip()
        if cfg.get("tempmail_domain") not in (None, ""):
            dom = str(cfg.get("tempmail_domain") or "").strip()
        base = ""  # fixed https://api.tempmail.lol
    else:
        if cfg.get("moemail_domain") not in (None, ""):
            dom = str(cfg.get("moemail_domain") or "").strip()
        base = str(cfg.get("moemail_base_url") or base).strip()
        key = str(cfg.get("moemail_api_key") or key).strip()

    return prov, key, base, dom


def _snapshot_reg_config(
    *,
    captcha_provider: str,
    yescaptcha_key: str,
    proxy: str,
    moemail_api_key: str | None,
    moemail_base_url: str | None,
    prefix: str | None,
    domain: str | None,
    expiry_ms: int | None,
    concurrency: int,
    stagger_ms: int,
    mail_provider: str | None = None,
) -> dict[str, Any]:
    """Config snapshot kept with the in-memory/Redis batch while it is running."""
    return {
        "captcha_provider": captcha_provider,
        "yescaptcha_key": yescaptcha_key if captcha_provider == "yescaptcha" else "",
        "proxy": proxy or "",
        "moemail_api_key": moemail_api_key or "",
        "moemail_base_url": moemail_base_url or "",
        "prefix": prefix or "",
        "domain": domain or "",
        "expiry_ms": expiry_ms,
        "concurrency": concurrency,
        "stagger_ms": stagger_ms,
        "local_solver_url": "http://127.0.0.1:5072",
        "mail_provider": (mail_provider or "moemail").strip().lower() or "moemail",
    }


class _RegCancelled(Exception):
    """Cooperative cancel for in-flight registration workers."""


_TERMINAL_STATUSES = frozenset(
    {
        "imported",
        "success",
        "completed",
        "error",
        "failed",
        "expired",
        "protocol_error",
        "protocol_blocked",
        "cancelled",
        "stopped",
    }
)


def _is_cancel_status(status: str | None) -> bool:
    return str(status or "").lower() in ("cancelled", "stopped", "stopping")


def _session_cancel_requested(sess: dict[str, Any] | None) -> bool:
    if not isinstance(sess, dict):
        return False
    if sess.get("cancel_requested"):
        return True
    return _is_cancel_status(sess.get("status"))


def _mirror_reg_sess(sid: str, sess: dict[str, Any] | None, *, force: bool = False) -> None:
    if not _reg_redis() or not sid:
        return
    try:
        from grok2api.store import sessions_redis

        if sess is None:
            sessions_redis.reg_sess_delete(sid)
            _reg_mirror_last.pop(f"s:{sid}", None)
            return
        # Throttle non-terminal mirrors — local RAM still updates every step for
        # same-process batch GET; Redis only needs ~100ms freshness for multi-worker UI.
        st = str(sess.get("status") or "").lower()
        terminal = st in _TERMINAL_STATUSES or bool(sess.get("finished")) or force
        if not terminal:
            now = float(_now())
            key = f"s:{sid}"
            last = float(_reg_mirror_last.get(key) or 0.0)
            if (now - last) < _REG_MIRROR_MIN_INTERVAL_SEC:
                return
            _reg_mirror_last[key] = now
        # Always strip process-local fields before Redis write.
        payload = {
            k: v
            for k, v in sess.items()
            if isinstance(k, str) and not k.startswith("_") and not callable(v)
        }
        # Cap log payload in Redis — admin only needs recent steps.
        lines = list(payload.get("log_lines") or [])
        if len(lines) > 20:
            payload["log_lines"] = lines[-20:]
        if payload.get("log") and len(str(payload.get("log"))) > 900:
            payload["log"] = str(payload.get("log"))[-900:]
            payload["output_tail"] = payload["log"]
        sessions_redis.reg_sess_put(sid, payload)
        if terminal:
            _reg_mirror_last.pop(f"s:{sid}", None)
    except Exception:
        pass


def _mirror_reg_batch(batch_id: str, batch: dict[str, Any] | None, *, force: bool = False) -> None:
    if not _reg_redis() or not batch_id or batch is None:
        return
    try:
        from grok2api.store import sessions_redis

        st = str(batch.get("status") or batch.get("batch_status") or "").lower()
        terminal = st in ("done", "partial", "error", "cancelled", "stopped", "failed") or force
        if not terminal:
            now = float(_now())
            key = f"b:{batch_id}"
            last = float(_reg_mirror_last.get(key) or 0.0)
            if (now - last) < max(_REG_MIRROR_MIN_INTERVAL_SEC, 0.2):
                return
            _reg_mirror_last[key] = now
        sessions_redis.reg_batch_put(batch_id, batch)
        if terminal:
            _reg_mirror_last.pop(f"b:{batch_id}", None)
    except Exception:
        pass


def _record_register_task(
    *,
    task_id: str | None,
    summary: str,
    status: str,
    ok: bool | None = None,
    progress_done: int = 0,
    progress_total: int = 0,
    finished: bool = True,
    detail: dict[str, Any] | None = None,
) -> None:
    """Best-effort write into admin「任务日志」for protocol registration."""
    tid = str(task_id or "").strip()
    if not tid:
        return
    try:
        import grok2api.admin.task_log as task_log

        task_log.record(
            "register",
            task_id=tid,
            summary=str(summary or f"协议注册 {tid}")[:500],
            status=str(status or "done"),
            ok=ok,
            progress_done=int(progress_done or 0),
            progress_total=int(progress_total or 0),
            finished=bool(finished),
            detail=detail if isinstance(detail, dict) else {},
        )
    except Exception:
        # Never break registration workers because of logging.
        pass




def _append_session_log(sess: dict[str, Any], status: str, message: str, *, max_lines: int = 22) -> None:
    """Append a timestamped progress line for real-time admin registration log UI.

    Keep the ring buffer small: batch GET embeds many sessions; oversized log_lines
    dominate payload size and inflate Redis + Go→browser latency.
    """
    try:
        lines = list(sess.get("log_lines") or [])
    except Exception:
        lines = []
    try:
        ts = time.strftime("%H:%M:%S", time.localtime(float(_now())))
    except Exception:
        ts = time.strftime("%H:%M:%S")
    st = str(status or "").strip() or "—"
    msg = str(message or "").strip().replace("\n", " ")
    if len(msg) > 140:
        msg = msg[:140] + "…"
    line = f"[{ts}] {st}: {msg}" if msg else f"[{ts}] {st}"
    # Dedup consecutive identical status+message (ignore timestamp prefix).
    # Also throttle high-frequency captcha poll slices ("waiting poll slice 1.0/2.0s")
    # so Redis mirrors and UI stay cheap while still heartbeating updated_at via update().
    if lines:
        prev = str(lines[-1])
        if len(prev) > 10 and prev[10:] == line[10:]:
            return
        # Collapse noisy poll-slice chatter: keep at most one every ~4s.
        noisy = (
            "waiting poll slice" in msg.lower()
            or "still processing" in msg.lower()
            or "poll slice" in msg.lower()
            or "heartbeat" in msg.lower()
        )
        if noisy and len(prev) > 10:
            prev_body = prev[10:]
            if prev_body.startswith(f" {st}:") or f"] {st}:" in prev:
                # Same status family — skip if last line is also noisy and recent.
                try:
                    last_noisy = (
                        "waiting poll slice" in prev_body.lower()
                        or "still processing" in prev_body.lower()
                        or "poll slice" in prev_body.lower()
                        or "heartbeat" in prev_body.lower()
                    )
                except Exception:
                    last_noisy = False
                if last_noisy:
                    # Replace last noisy line so UI shows latest progress without growth.
                    lines[-1] = line
                    sess["log_lines"] = lines[-max_lines:]
                    sess["log"] = "\n".join(lines[-12:])
                    sess["output_tail"] = sess["log"]
                    return
    lines.append(line)
    if len(lines) > max_lines:
        lines = lines[-max_lines:]
    sess["log_lines"] = lines
    # Compact tail for older UI paths that only read log / output_tail.
    sess["log"] = "\n".join(lines[-12:])
    sess["output_tail"] = sess["log"]


def _throttle_task_log(
    *,
    task_id: str,
    status: str,
    summary: str,
    progress_done: int = 0,
    progress_total: int = 0,
    finished: bool = False,
    ok: bool | None = None,
    detail: dict[str, Any] | None = None,
    min_interval_sec: float = 1.5,
    force: bool = False,
) -> None:
    """Throttle PG task_logs writes so progress is near-real-time without thrashing DB."""
    tid = str(task_id or "").strip()
    if not tid:
        return
    now = float(_now())
    with _lock:
        cache = getattr(_throttle_task_log, "_last", None)
        if not isinstance(cache, dict):
            cache = {}
            setattr(_throttle_task_log, "_last", cache)
        prev = cache.get(tid) or {}
        last_ts = float(prev.get("ts") or 0)
        last_summary = str(prev.get("summary") or "")
        last_status = str(prev.get("status") or "")
        last_done = int(prev.get("done") or -1)
        if (
            not force
            and not finished
            and (now - last_ts) < float(min_interval_sec)
            and last_status == str(status or "")
            and last_done == int(progress_done or 0)
            and last_summary == str(summary or "")[:500]
        ):
            return
        cache[tid] = {
            "ts": now,
            "summary": str(summary or "")[:500],
            "status": str(status or ""),
            "done": int(progress_done or 0),
        }
        if len(cache) > 4000:
            items = sorted(cache.items(), key=lambda kv: float((kv[1] or {}).get("ts") or 0))
            for k, _ in items[: max(1, len(items) // 5)]:
                cache.pop(k, None)
    _record_register_task(
        task_id=tid,
        summary=summary,
        status=status,
        ok=ok,
        progress_done=progress_done,
        progress_total=progress_total,
        finished=finished,
        detail=detail,
    )


def _session_task_log_payload(sess: dict[str, Any] | None) -> dict[str, Any]:
    s = sess if isinstance(sess, dict) else {}
    st = str(s.get("status") or "done").lower() or "done"
    ok: bool | None
    if st in ("imported", "success", "completed", "done"):
        ok = True
    elif st in ("error", "failed", "expired", "protocol_error", "protocol_blocked"):
        ok = False
    elif st in ("cancelled", "stopped"):
        ok = False
    else:
        ok = None
    email = str(s.get("email") or "").strip()
    summary = str(s.get("message") or "").strip()
    if not summary:
        summary = f"协议注册 {email or s.get('id') or ''}".strip()
    return {
        "task_id": str(s.get("id") or ""),
        "summary": summary,
        "status": st,
        "ok": ok,
        "progress_done": 1 if st in _TERMINAL_STATUSES else 0,
        "progress_total": 1,
        "finished": st in _TERMINAL_STATUSES,
        "detail": {
            "session_id": s.get("id"),
            "batch_id": s.get("batch_id"),
            "email": email or None,
            "status": st,
            "error": s.get("error"),
            "imported_account_ids": list(s.get("imported_account_ids") or [])[:20],
            "sub2api_push": s.get("sub2api_push"),
            "adapter_build": s.get("adapter_build") or ADAPTER_BUILD,
            "log_tail": list(s.get("log_lines") or [])[-30:],
        },
    }


def _merge_reg_remote(local: dict[str, Any], remote: dict[str, Any]) -> dict[str, Any]:
    """Prefer Redis when it is newer; keep process-only ``_`` fields from local."""
    merged = {**local, **remote}
    for k, v in local.items():
        if isinstance(k, str) and k.startswith("_") and k not in remote:
            merged[k] = v
    return merged


def _load_reg_sess(sid: str) -> dict[str, Any] | None:
    """Load a registration session, always refreshing from Redis when newer.

    Multi-worker note: the admin poll often lands on a different process than
    the registration worker. Returning local cache blindly freezes the UI
    progress log until that worker happens to serve the request.
    """
    with _lock:
        local = _sessions.get(sid)
    remote = None
    if _reg_redis():
        try:
            from grok2api.store import sessions_redis

            remote = sessions_redis.reg_sess_get(sid)
            if remote is not None and not isinstance(remote, dict):
                remote = None
        except Exception:
            remote = None

    if local is None and remote is None:
        return None
    if local is None:
        with _lock:
            cur = _sessions.get(sid)
            if cur is not None:
                local_ts = float(cur.get("updated_at") or 0)
                remote_ts = float((remote or {}).get("updated_at") or 0)
                if remote is not None and remote_ts > local_ts:
                    merged = _merge_reg_remote(cur, remote)
                    _sessions[sid] = merged
                    return merged
                return cur
            _sessions[sid] = remote  # type: ignore[assignment]
        return remote
    if remote is None:
        return local

    local_ts = float(local.get("updated_at") or 0)
    remote_ts = float(remote.get("updated_at") or 0)
    if remote_ts > local_ts:
        with _lock:
            cur = _sessions.get(sid) or local
            cur_ts = float(cur.get("updated_at") or 0)
            remote_ts2 = float(remote.get("updated_at") or 0)
            if remote_ts2 > cur_ts:
                merged = _merge_reg_remote(cur, remote)
                _sessions[sid] = merged
                return merged
            return cur
    return local


def _load_reg_batch(batch_id: str) -> dict[str, Any] | None:
    """Load a registration batch, refreshing from Redis when newer."""
    with _lock:
        local = _batches.get(batch_id)
    remote = None
    if _reg_redis():
        try:
            from grok2api.store import sessions_redis

            remote = sessions_redis.reg_batch_get(batch_id)
            if remote is not None and not isinstance(remote, dict):
                remote = None
        except Exception:
            remote = None

    if local is None and remote is None:
        return None
    if local is None:
        with _lock:
            cur = _batches.get(batch_id)
            if cur is not None:
                local_ts = float(cur.get("updated_at") or 0)
                remote_ts = float((remote or {}).get("updated_at") or 0)
                if remote is not None and remote_ts > local_ts:
                    ids = list(cur.get("session_ids") or [])
                    for x in remote.get("session_ids") or []:
                        if x not in ids:
                            ids.append(x)
                    merged = {**cur, **remote, "session_ids": ids}
                    _batches[batch_id] = merged
                    return merged
                return cur
            _batches[batch_id] = remote  # type: ignore[assignment]
        return remote
    if remote is None:
        return local

    local_ts = float(local.get("updated_at") or 0)
    remote_ts = float(remote.get("updated_at") or 0)
    if remote_ts > local_ts:
        with _lock:
            cur = _batches.get(batch_id) or local
            cur_ts = float(cur.get("updated_at") or 0)
            remote_ts2 = float(remote.get("updated_at") or 0)
            if remote_ts2 > cur_ts:
                ids = list(cur.get("session_ids") or [])
                for x in remote.get("session_ids") or []:
                    if x not in ids:
                        ids.append(x)
                merged = {**cur, **remote, "session_ids": ids}
                _batches[batch_id] = merged
                return merged
            return cur
    return local


def _clean_old_sessions() -> None:
    """Bound in-process session memory during bulk registration.

    Terminal sessions are dropped after a short TTL; unfinished keep longer.
    Absolute cap prevents 500-session batches from retaining full log blobs.
    """
    now = _now()
    try:
        # Terminal rows only need a short window for the admin final card / SSO export.
        term_ttl = float(os.environ.get("GROK2API_REG_SESSION_TERM_TTL", "300") or 300)
    except (TypeError, ValueError):
        term_ttl = 300.0
    try:
        live_ttl = float(os.environ.get("GROK2API_REG_SESSION_LIVE_TTL", "14400") or 14400)
    except (TypeError, ValueError):
        live_ttl = 14400.0
    try:
        max_keep = int(os.environ.get("GROK2API_REG_SESSION_MAX", "48") or 48)
    except (TypeError, ValueError):
        max_keep = 48
    max_keep = max(16, min(200, max_keep))
    term_ttl = max(60.0, min(3600.0, term_ttl))
    live_ttl = max(term_ttl, min(24 * 3600.0, live_ttl))

    for sid in list(_sessions.keys()):
        sess = _sessions.get(sid) or {}
        updated = float(sess.get("updated_at") or sess.get("created_at") or 0)
        st = str(sess.get("status") or "").lower()
        terminal = st in _TERMINAL_STATUSES or bool(sess.get("finished"))
        ttl = term_ttl if terminal else live_ttl
        if updated and (now - updated) > ttl:
            # Drop heavy fields first if still needed by UI via Redis.
            _sessions.pop(sid, None)
            # Keep Redis mirror for admin poll; local RAM is the OOM risk.
            continue

    # Absolute cap: keep newest max_keep; strip logs from older terminal rows first.
    if len(_sessions) > max_keep:
        items = sorted(
            _sessions.items(),
            key=lambda kv: float((kv[1] or {}).get("updated_at") or 0),
            reverse=True,
        )
        keep = {sid for sid, _ in items[:max_keep]}
        for sid in list(_sessions.keys()):
            if sid in keep:
                continue
            sess = _sessions.get(sid) or {}
            st = str(sess.get("status") or "").lower()
            if st in _TERMINAL_STATUSES or bool(sess.get("finished")):
                _sessions.pop(sid, None)
            else:
                # Live but over cap: strip bulky logs only.
                if isinstance(sess, dict):
                    sess.pop("log_lines", None)
                    sess.pop("log", None)
                    sess.pop("output_tail", None)
                    sess.pop("auth_json", None)


def _compact_session(sess: dict[str, Any]) -> dict[str, Any]:
    # Only JSON-safe public fields. Process-local objects (e.g. `_receiver`,
    # `_client`) must never leak into FastAPI / admin responses.
    out: dict[str, Any] = {}
    for k, v in (sess or {}).items():
        if not isinstance(k, str):
            continue
        if k.startswith("_"):
            continue
        if callable(v):
            continue
        out[k] = v
    out.pop("_client", None)
    out.pop("_oauth_client", None)
    out.pop("_receiver", None)
    out.pop("password", None)
    out.pop("yescaptcha_key", None)
    # Prefer explicit imported ids; fall back to auth_json summary for UI/logs.
    imported_ids = list(out.get("imported_account_ids") or [])
    imported_accounts = list(out.get("imported_accounts") or [])
    aj = out.get("auth_json")
    if isinstance(aj, dict):
        rows = [x for x in (aj.get("imported") or []) if isinstance(x, dict)]
        out["auth_json_count"] = len(rows)
        if not imported_ids:
            imported_ids = [str(x.get("id")) for x in rows if x.get("id")]
        if not imported_accounts:
            imported_accounts = [
                {"id": x.get("id"), "email": x.get("email")}
                for x in rows
                if x.get("id") or x.get("email")
            ]
    elif aj is not None:
        try:
            out["auth_json_count"] = len(aj)  # type: ignore[arg-type]
        except Exception:
            out["auth_json_count"] = 0
    if imported_ids:
        out["imported_account_ids"] = imported_ids
    if imported_accounts:
        out["imported_accounts"] = imported_accounts
    # Drop full auth payload from list/poll responses (secrets).
    out.pop("auth_json", None)
    # Compact: keep only short tail for admin UI (bulk reg holds hundreds of sessions).
    lines = list(out.get("log_lines") or [])
    if len(lines) > 14:
        out["log_lines"] = lines[-14:]
    if out.get("log") and len(str(out.get("log"))) > 600:
        out["log"] = str(out.get("log"))[-600:]
        out["output_tail"] = out["log"]
    # Drop heavy probe result blobs from list/batch embeds (keep summary counts).
    probe = out.get("probe")
    if isinstance(probe, dict) and isinstance(probe.get("results"), list) and len(probe["results"]) > 6:
        out["probe"] = {
            "ok": probe.get("ok"),
            "fail": probe.get("fail"),
            "count": probe.get("count") or len(probe.get("results") or []),
            "results": list(probe.get("results") or [])[-6:],
        }
    return out


def ensure_xconsole() -> None:
    """Ensure vendored grok-build-auth/xconsole_client is importable.

    Raises RuntimeError with actionable message when unavailable.
    Safe to call multiple times.
    """
    global _xconsole_ready, _xconsole_error
    if _xconsole_ready:
        return
    if _xconsole_error:
        raise RuntimeError(_xconsole_error)

    if not GBA.is_dir():
        _xconsole_error = (
            "grok-build-auth 目录不存在。请确认仓库完整检出，"
            "或重新 clone 本项目。"
        )
        raise RuntimeError(_xconsole_error)

    xc = GBA / "xconsole_client"
    if not xc.is_dir():
        _xconsole_error = (
            "grok-build-auth/xconsole_client 不存在。"
            "请确认仓库完整检出（该目录已内置，不再使用 git submodule）。"
        )
        raise RuntimeError(_xconsole_error)

    # Put vendored package root on sys.path so `import xconsole_client` works.
    gba_str = str(GBA.resolve())
    if gba_str not in sys.path:
        sys.path.insert(0, gba_str)

    try:
        # Import side-effect: validate package is loadable.
        import xconsole_client  # noqa: F401
        from xconsole_client import (  # noqa: F401
            XConsoleAuthClient,
            YesCaptchaSolver,
            create_solver,
            xai_oauth_login_protocol,
        )
        from xconsole_client.oauth_protocol import (  # noqa: F401
            extract_cookies_from_auth_client,
        )
        from xconsole_client.xai_oauth import (  # noqa: F401
            CLIPROXYAPI_GROK_HEADERS,
            build_cliproxyapi_auth_record,
        )
    except ModuleNotFoundError as e:
        missing = getattr(e, "name", None) or str(e)
        if missing in ("curl_cffi", "requests") or "curl_cffi" in str(e) or "requests" in str(e):
            _xconsole_error = (
                f"注册机依赖缺失: {missing}。请执行: pip install -r requirements.txt"
            )
        else:
            _xconsole_error = (
                f"无法导入 xconsole_client ({e})。请执行: pip install -r requirements.txt"
            )
        raise RuntimeError(_xconsole_error) from e
    except Exception as e:  # noqa: BLE001
        _xconsole_error = f"加载 grok-build-auth 失败: {e}"
        raise RuntimeError(_xconsole_error) from e

    _xconsole_ready = True
    _xconsole_error = None


def _local_solver_base_url(url: str | None = None) -> str:
    """Always pin local captcha to the in-container inline solver."""
    raw = (
        (url or "").strip()
        or (LOCAL_SOLVER_URL or "").strip()
        or os.environ.get("GROK2API_LOCAL_SOLVER_URL")
        or os.environ.get("LOCAL_SOLVER_URL")
        or "http://127.0.0.1:5072"
    ).strip().rstrip("/")
    # Registration must never hit an external "local" URL; force loopback.
    if (
        not raw
        or "127.0.0.1" not in raw
        and "localhost" not in raw
    ):
        return "http://127.0.0.1:5072"
    return raw


def probe_local_solver(
    url: str | None = None,
    *,
    timeout: float = 2.0,
) -> dict[str, Any]:
    """Probe inline Turnstile Solver readiness.

    HTTP up is enough to accept createTask (lazy browser mode warms on first
    solve). We prefer ``/health`` but also accept ``/`` returning any HTTP body.
    """
    base = _local_solver_base_url(url)
    out: dict[str, Any] = {
        "ok": False,
        "ready": False,
        "url": base,
        "http_up": False,
        "lazy": None,
        "pool_ready": None,
        "error": None,
        "status_code": None,
    }
    try:
        import urllib.error
        import urllib.request
    except Exception as e:  # noqa: BLE001
        out["error"] = f"urllib unavailable: {e}"
        return out

    # 1) /health — structured
    try:
        req = urllib.request.Request(
            f"{base}/health",
            headers={"Accept": "application/json"},
            method="GET",
        )
        with urllib.request.urlopen(req, timeout=max(0.5, float(timeout))) as resp:
            out["status_code"] = int(getattr(resp, "status", 200) or 200)
            body = resp.read().decode("utf-8", errors="replace")
        out["http_up"] = True
        try:
            data = json.loads(body) if body else {}
        except Exception:
            data = {}
        if isinstance(data, dict):
            out["lazy"] = data.get("lazy")
            out["pool_ready"] = data.get("pool_ready")
            # Solver process is ready when /health answers ok=true.
            # pool_ready may still be false under TURNSTILE_LAZY=1 — that is OK;
            # browsers warm on the first captcha task.
            if data.get("ok") is False:
                out["error"] = f"solver health ok=false body={body[:200]}"
            else:
                out["ok"] = True
                out["ready"] = True
                return out
        else:
            out["ok"] = True
            out["ready"] = True
            return out
    except Exception as e:  # noqa: BLE001
        out["error"] = f"health: {e}"

    # 2) fallback: any response from /
    try:
        req = urllib.request.Request(f"{base}/", method="GET")
        with urllib.request.urlopen(req, timeout=max(0.5, float(timeout))) as resp:
            out["status_code"] = int(getattr(resp, "status", 200) or 200)
            _ = resp.read(256)
        out["http_up"] = True
        out["ok"] = True
        out["ready"] = True
        out["error"] = None
        return out
    except Exception as e:  # noqa: BLE001
        if not out.get("error"):
            out["error"] = f"root: {e}"
        else:
            out["error"] = f"{out['error']}; root: {e}"
        return out


def wait_for_local_solver(
    url: str | None = None,
    *,
    timeout_sec: float | None = None,
    poll_sec: float | None = None,
    progress: Any | None = None,
) -> dict[str, Any]:
    """Block until local Turnstile Solver is HTTP-ready, or fail.

    Used by registration start so workers never race a still-booting solver.
    """
    base = _local_solver_base_url(url)
    try:
        wait_s = float(
            timeout_sec
            if timeout_sec is not None
            else LOCAL_SOLVER_WAIT_SEC
        )
    except (TypeError, ValueError):
        wait_s = 120.0
    wait_s = max(1.0, min(wait_s, 600.0))
    try:
        every = float(
            poll_sec if poll_sec is not None else LOCAL_SOLVER_POLL_SEC
        )
    except (TypeError, ValueError):
        every = 1.0
    every = max(0.2, min(every, 5.0))

    deadline = time.time() + wait_s
    last: dict[str, Any] = {
        "ok": False,
        "ready": False,
        "url": base,
        "waited_sec": 0.0,
        "error": "not started",
    }
    started = time.time()
    attempt = 0
    while True:
        attempt += 1
        last = probe_local_solver(base, timeout=min(2.0, every + 0.5))
        last["waited_sec"] = round(time.time() - started, 2)
        last["attempts"] = attempt
        last["url"] = base
        if last.get("ready"):
            last["ok"] = True
            if progress:
                try:
                    progress(
                        f"本地过盾已就绪 url={base} waited={last['waited_sec']}s"
                    )
                except Exception:
                    pass
            return last
        remaining = deadline - time.time()
        if remaining <= 0:
            break
        msg = (
            f"等待本地过盾就绪… url={base} "
            f"attempt={attempt} left={remaining:.0f}s "
            f"err={last.get('error') or 'down'}"
        )
        if progress:
            try:
                progress(msg)
            except Exception:
                pass
        if attempt == 1 or attempt % 5 == 0:
            print(f"[grok-build-auth] {msg}")
        time.sleep(min(every, max(0.2, remaining)))

    last["ok"] = False
    last["ready"] = False
    last["error"] = (
        f"本地过盾服务未在 {wait_s:.0f}s 内就绪 "
        f"(url={base}; last={last.get('error') or 'unreachable'}). "
        f"请确认 inline Turnstile Solver 已启动（entrypoint / TURNSTILE_PORT），"
        f"或稍后重试注册。"
    )
    return last


def registration_available() -> dict[str, Any]:
    """Non-raising health probe for admin UI / startup logs."""
    moemail_configured = bool(
        os.environ.get("GROK2API_MOEMAIL_API_KEY")
        or os.environ.get("MOEMAIL_API_KEY")
    )
    try:
        from grok2api.config import MOEMAIL_API_KEY as _cfg_moemail

        moemail_configured = moemail_configured or bool(_cfg_moemail)
    except Exception:
        pass
    provider = (
        CAPTCHA_PROVIDER
        or os.environ.get("GROK2API_CAPTCHA_PROVIDER")
        or os.environ.get("CAPTCHA_PROVIDER")
        or "local"
    ).strip().lower()
    if provider not in {"local", "yescaptcha"}:
        provider = "local"
    local_url = _local_solver_base_url(
        LOCAL_SOLVER_URL
        or os.environ.get("GROK2API_LOCAL_SOLVER_URL")
        or os.environ.get("LOCAL_SOLVER_URL")
        or ""
    )
    captcha_ready = bool(local_url) if provider == "local" else bool(YESCAPTCHA_KEY)
    local_solver_live: dict[str, Any] | None = None
    if provider == "local":
        local_solver_live = probe_local_solver(local_url, timeout=1.2)
        captcha_ready = bool(local_solver_live.get("ready"))
    try:
        ensure_xconsole()
        out = {
            "ok": True,
            "available": True,
            "engine": "dongguatanglinux/grok-build-auth",
            "path": str(GBA),
            "vendored": True,
            "adapter_build": ADAPTER_BUILD,
            "captcha_provider": provider,
            "local_solver_url": local_url,
            "local_solver_configured": bool(local_url),
            "local_solver_ready": (
                bool(local_solver_live.get("ready")) if local_solver_live else None
            ),
            "local_solver_probe": local_solver_live,
            "yescaptcha_configured": (
                captcha_ready if provider == "local" else bool(YESCAPTCHA_KEY)
            ),
            "moemail_configured": moemail_configured,
            "admission": _reg_admission.snapshot(),
        }
        return out
    except Exception as e:  # noqa: BLE001
        return {
            "ok": False,
            "available": False,
            "engine": "dongguatanglinux/grok-build-auth",
            "path": str(GBA),
            "vendored": True,
            "adapter_build": ADAPTER_BUILD,
            "error": str(e),
            "captcha_provider": provider,
            "local_solver_url": local_url,
            "local_solver_configured": bool(local_url),
            "local_solver_ready": (
                bool(local_solver_live.get("ready")) if local_solver_live else None
            ),
            "local_solver_probe": local_solver_live,
            "yescaptcha_configured": (
                captcha_ready if provider == "local" else bool(YESCAPTCHA_KEY)
            ),
            "moemail_configured": moemail_configured,
            "admission": _reg_admission.snapshot(),
        }


# --------------------------------------------------------------------------- #
# mail provider: moemail / yyds (reuse grokcli-2api config)
# --------------------------------------------------------------------------- #
def _make_email_receiver(
    *,
    api_key: str | None = None,
    base_url: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    mail_provider: str | None = None,
    domain_index: int | None = None,
):
    from grok2api.upstream.moemail import (
        create_mailbox,
        fetch_messages,
        normalize_mail_provider,
        pick_domain_from_list,
        parse_domain_list,
    )
    from grok2api.config import MOEMAIL_API_KEY, MOEMAIL_BASE_URL, MOEMAIL_DOMAIN, MOEMAIL_EXPIRY_MS

    base = (base_url or MOEMAIL_BASE_URL or "").rstrip("/")
    prov = normalize_mail_provider(mail_provider, base_url=base)
    key = (api_key or MOEMAIL_API_KEY or "").strip()
    # GPTMail allows empty key (public gpt-test). YYDS/MoeMail/CF require a key.
    if not key and prov not in {"tempmail"}:
        raise ValueError(
            f"Mail API key missing for provider={prov}. "
            "Save the key in 协议注册配置 (each provider keeps its own key; TempMail.lol free needs none)."
        )
    # Multi-domain config (newlines/commas): rotate by domain_index in batch jobs.
    # Empty list => provider auto-pick (YYDS/GPTMail/CF/Cloud Mail) or MOEMAIL_DOMAIN fallback.
    domains = parse_domain_list(domain)
    if domains:
        if domain_index is not None:
            dom = pick_domain_from_list(domain, index=domain_index, strategy="round_robin")
        else:
            dom = pick_domain_from_list(domain, strategy="random")
    else:
        # YYDS/GPTMail/CFMail/Cloud Mail: empty domain means provider-side
        # auto/random pick (Cloud Mail calls /setting/websiteConfig inside
        # cloudmail_create_mailbox). Never bleed MoeMail's MOEMAIL_DOMAIN
        # (default example.com) into them.
        if prov in {"yyds", "gptmail", "cfmail", "tempmail", "cloudmail"}:
            dom = ""
        else:
            dom = (domain or MOEMAIL_DOMAIN or "").strip().lstrip("@").strip(".")

    # Random local-part only (no "grok-" brand prefix). Admin UI no longer exposes
    # a custom prefix field; ignore leftover config values for new mailboxes.
    pre = secrets.token_hex(5).lower()

    mailbox = create_mailbox(

        provider=prov,
        name=pre,
        domain=dom or None,
        expiry_ms=expiry_ms if expiry_ms is not None else MOEMAIL_EXPIRY_MS,
        api_key=key,
        base_url=base,
    )
    email_id = mailbox["id"]
    address = mailbox["email"]
    token = str(mailbox.get("token") or "")

    class _MailReceiver:
        def __init__(
            self,
            email: str,
            email_id: str,
            api_key: str | None,
            base_url: str | None,
            *,
            provider: str,
            token: str = "",
        ):
            self.email = email
            self.email_id = email_id
            self.api_key = api_key
            if provider == "yyds":
                default_base = "https://maliapi.215.im"
            elif provider == "gptmail":
                default_base = "https://mail.chatgpt.org.uk"
            elif provider == "cfmail":
                default_base = "https://temp-email-api.awsl.uk"
            elif provider == "tempmail":
                default_base = "https://api.tempmail.lol"
            elif provider == "cloudmail":
                # Cloud Mail is self-hosted; base_url is required and validated
                # upstream. Empty default here keeps the receiver honest if the
                # caller forgot to pass base_url (will surface as a network error
                # rather than silently hitting the wrong host).
                default_base = ""
            else:
                default_base = "https://moemail.521884.xyz"
            self.base_url = base_url or default_base
            self.provider = provider
            self.token = token

        def wait_for_code(
            self,
            timeout: float = 120,
            *,
            should_cancel=None,
            poll_interval: float | None = None,
            on_tick=None,
        ) -> str:
            import re as _re

            deadline = time.time() + float(timeout or 120)
            # Keep polls short so cooperative cancel can land quickly.
            poll = float(poll_interval if poll_interval is not None else 1.0)
            poll = max(0.4, min(poll, 2.0))
            started = time.time()
            last_tick = 0.0
            while time.time() < deadline:
                if callable(should_cancel) and should_cancel():
                    raise _RegCancelled("cancelled while waiting for email code")
                try:
                    messages = []
                    # YYDS: prefer GET /v1/messages/next (long-poll + verificationCode).
                    if self.provider == "yyds":
                        try:
                            from grok2api.upstream.moemail import yyds_wait_next_message

                            nxt = yyds_wait_next_message(
                                address=self.email,
                                api_key=self.api_key,
                                base_url=self.base_url,
                                token=self.token or None,
                                wait=min(15, max(1, int(deadline - time.time()))),
                                email_id=self.email_id,
                            )
                            if nxt:
                                messages = [nxt]
                        except Exception:
                            messages = []
                    if not messages:
                        messages = fetch_messages(
                            self.email_id,
                            provider=self.provider,
                            api_key=self.api_key,
                            base_url=self.base_url,
                            include_details=True,
                            address=self.email,
                            token=self.token or None,
                        )
                    for item in messages:
                        # Prefer xAI AAA-BBB codes first.
                        text = "\n".join(
                            str(item.get(k) or "")
                            for k in (
                                "subject",
                                "content",
                                "text",
                                "textBody",
                                "html",
                                "htmlBody",
                                "body",
                                "from_address",
                                "from",
                                "verificationCode",
                            )
                        )
                        match = _re.search(
                            r"\b([A-Z0-9]{3})-([A-Z0-9]{3})\b", text, flags=_re.I
                        )
                        if match:
                            return "".join(match.groups()).upper()
                        # Also accept plain 6-char alnum codes from xAI mails.
                        match2 = _re.search(
                            r"\b([A-Z0-9]{6})\b", text, flags=_re.I
                        )
                        if match2 and "x.ai" in text.lower():
                            return match2.group(1).upper()
                        extracted = item.get("extracted") or {}
                        codes = extracted.get("codes") or []
                        for code in codes:
                            clean = str(code).replace("-", "").strip().upper()
                            if len(clean) == 6 and _re.fullmatch(r"[A-Z0-9]{6}", clean):
                                return clean
                except Exception:
                    pass
                # Heartbeat for admin UI so waiting_email is not a silent freeze.
                now = time.time()
                if callable(on_tick) and (now - last_tick) >= 2.0:
                    last_tick = now
                    try:
                        on_tick(
                            elapsed=max(0.0, now - started),
                            remaining=max(0.0, deadline - now),
                        )
                    except Exception:
                        pass
                # Sleep in small slices so stop can interrupt mid-wait.
                slept = 0.0
                while slept < poll:
                    if callable(should_cancel) and should_cancel():
                        raise _RegCancelled("cancelled while waiting for email code")
                    step = min(0.25, poll - slept)
                    time.sleep(step)
                    slept += step
                poll = min(2.0, poll + 0.15)
            raise RuntimeError("timeout waiting for xAI email verification code")

    return address, _MailReceiver(
        address,
        email_id,
        api_key=key,
        base_url=base,
        provider=prov,
        token=token,
    )


def _proxy_url() -> str:
    """Pick one proxy from the configured pool (env / registration config)."""
    try:
        from grok2api.upstream.proxy_pool import resolve_proxy_for_request

        return resolve_proxy_for_request(fallback_env=True) or ""
    except Exception:
        try:
            from grok2api.upstream.moemail import normalize_proxy_config
            from grok2api.config import XAI_PROXY

            cfg = normalize_proxy_config(XAI_PROXY or None)
            return cfg["proxy"] if cfg else ""
        except Exception:
            return ""


def _proxy_pool(
    proxy_text: str | None = None,
    *,
    username: str | None = None,
    password: str | None = None,
) -> list[str]:
    """Parse multi-line proxy text into full proxy URLs.

    Preference:
      1) explicit proxy_text (+ optional user/pass)
      2) env / outbound_proxy_config pool
      3) auto-discovered peer proxy (privoxy etc.)
    """
    try:
        from grok2api.upstream.proxy_pool import (
            parse_proxy_pool,
            get_outbound_proxy_source,
            first_working_proxy,
        )

        pool = parse_proxy_pool(
            proxy_text,
            username=username,
            password=password,
            fallback_env=True,
        )
        if pool:
            return pool
        # When registration form proxy is empty, reuse outbound pool text/auth.
        src = get_outbound_proxy_source() or {}
        if src.get("enabled", True):
            pool = list(src.get("pool") or [])
            if not pool:
                pool = parse_proxy_pool(
                    src.get("text"),
                    username=src.get("username"),
                    password=src.get("password"),
                    fallback_env=False,
                )
            if pool:
                return pool
        auto = first_working_proxy()
        if auto:
            return [auto]
    except Exception:
        pass
    # Fallback: treat as single proxy via classic normalizer.
    one = (proxy_text or "").strip() or _proxy_url()
    return [one] if one else []


def _pick_proxy_from_pool(
    pool: list[str],
    *,
    strategy: str | None = None,
    index: int | None = None,
) -> str:
    try:
        from grok2api.upstream.proxy_pool import pick_proxy, normalize_proxy_strategy

        strat = strategy
        if not strat:
            try:
                from grok2api.config import XAI_PROXY_STRATEGY as _strat
            except Exception:
                _strat = "round_robin"
            strat = _strat
        return pick_proxy(pool, strategy=normalize_proxy_strategy(strat), index=index) or ""
    except Exception:
        return pool[0] if pool else ""


# --------------------------------------------------------------------------- #
# registration flow
# --------------------------------------------------------------------------- #
def _prepare_registration_session(
    *,
    yescaptcha_key: str,
    proxy: str,
    moemail_api_key: str | None = None,
    moemail_base_url: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    mail_provider: str | None = None,
    batch_id: str | None = None,
    batch_index: int | None = None,
    batch_total: int | None = None,
    start_delay: float = 0.0,
) -> dict[str, Any]:
    """Create mailbox + session record. Does NOT start the registration worker."""
    if start_delay > 0:
        time.sleep(start_delay)

    try:
        email, receiver = _make_email_receiver(
            api_key=moemail_api_key,
            base_url=moemail_base_url,
            prefix=prefix,
            domain=domain,
            expiry_ms=expiry_ms,
            mail_provider=mail_provider,
            domain_index=batch_index,
        )
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "error": str(e)}

    # xAI password rules: mix upper/lower/digit/symbol.
    password = f"Aa{os.urandom(5).hex()}9!xZ"
    sid = f"gba_{uuid.uuid4().hex[:16]}"

    sess = {
        "id": sid,
        "status": "queued",
        "created_at": _now(),
        "updated_at": _now(),
        "email": email,
        "password": password,
        "message": f"queued; email={email}",
        "sso": None,
        "oauth": None,
        "auth_json": None,
        "error": None,
        "yescaptcha_key": yescaptcha_key,
        "proxy": proxy or None,
        "adapter_build": ADAPTER_BUILD,
        "batch_id": batch_id,
        "batch_index": batch_index,
        "batch_total": batch_total,
        # Keep receiver process-local only (not mirrored to Redis).
        "_receiver": receiver,
    }
    with _lock:
        _sessions[sid] = sess
        if batch_id and batch_id in _batches:
            _batches[batch_id]["session_ids"].append(sid)
            _batches[batch_id]["updated_at"] = _now()
            _mirror_reg_batch(batch_id, dict(_batches[batch_id]))
    _mirror_reg_sess(sid, sess)
    return {"ok": True, **_compact_session(sess)}


def _start_one_registration(
    *,
    yescaptcha_key: str,
    proxy: str,
    moemail_api_key: str | None = None,
    moemail_base_url: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    mail_provider: str | None = None,
    batch_id: str | None = None,
    batch_index: int | None = None,
    batch_total: int | None = None,
    start_delay: float = 0.0,
) -> dict[str, Any]:
    """Create one session and spawn its worker thread (single-job path)."""
    prepared = _prepare_registration_session(
        yescaptcha_key=yescaptcha_key,
        proxy=proxy,
        moemail_api_key=moemail_api_key,
        moemail_base_url=moemail_base_url,
        prefix=prefix,
        domain=domain,
        expiry_ms=expiry_ms,
        mail_provider=mail_provider,
        batch_id=batch_id,
        batch_index=batch_index,
        batch_total=batch_total,
        start_delay=start_delay,
    )
    if not prepared.get("ok"):
        return prepared
    sid = str(prepared.get("id") or "")
    with _lock:
        sess = _sessions.get(sid) or {}
        receiver = sess.get("_receiver")
    if not sid or receiver is None:
        return {"ok": False, "error": "registration session prepare failed"}
    with _lock:
        if sid in _sessions:
            _sessions[sid]["status"] = "started"
            _sessions[sid]["message"] = f"started; email={_sessions[sid].get('email') or ''}"
            _sessions[sid]["updated_at"] = _now()
            _mirror_reg_sess(sid, _sessions[sid])
    # Single-job starts are not covered by the batch finalizer — log "running"
    # immediately so 任务日志 shows the registration task right away.
    if not batch_id:
        with _lock:
            started_sess = dict(_sessions.get(sid) or {})
        if started_sess:
            payload = _session_task_log_payload(started_sess)
            _record_register_task(
                task_id=payload["task_id"],
                summary=payload["summary"] or f"协议注册启动 {started_sess.get('email') or sid}",
                status="running",
                ok=None,
                progress_done=0,
                progress_total=1,
                finished=False,
                detail={**payload["detail"], "phase": "started"},
            )
    threading.Thread(
        target=_run_registration,
        args=(sid, yescaptcha_key, proxy or "", receiver),
        daemon=True,
        name=f"gba-reg-{sid[-8:]}",
    ).start()
    with _lock:
        sess = _sessions.get(sid)
        if sess is None:
            return prepared
        return {"ok": True, **_compact_session(sess)}


def start_registration(
    *,
    captcha_provider: str | None = None,
    local_solver_url: str | None = None,
    yescaptcha_key: str | None = None,
    proxy: str | None = None,
    proxy_username: str | None = None,
    proxy_password: str | None = None,
    proxy_strategy: str | None = None,
    moemail_api_key: str | None = None,
    moemail_base_url: str | None = None,
    prefix: str | None = None,
    domain: str | None = None,
    expiry_ms: int | None = None,
    mail_provider: str | None = None,
    count: int | None = None,
    concurrency: int | None = None,
    stagger_ms: int | None = None,
    probe_delay_sec: float | int | None = None,
) -> dict[str, Any]:
    """Start one or many registration sessions (multi-thread).

    ``count`` > 1 enables batch mode. ``concurrency`` is the real in-flight
    limit: e.g. concurrency=3 means only 3 accounts register at the same time;
    when one finishes, the next queued account starts.

    ``proxy`` may be a single URL or a multi-line proxy pool. Each registration
    job picks one entry via ``proxy_strategy`` (round_robin / random / sticky).
    """
    try:
        ensure_xconsole()
    except Exception as e:  # noqa: BLE001
        return {"ok": False, "error": str(e)}

    _clean_old_sessions()

    # Allow admin form / env to override settle window for post-import probe.
    if probe_delay_sec is not None:
        try:
            globals()["REGISTER_PROBE_DELAY_SEC"] = max(
                0.0, min(600.0, float(probe_delay_sec))
            )
            os.environ["GROK2API_REG_PROBE_DELAY_SEC"] = str(
                int(globals()["REGISTER_PROBE_DELAY_SEC"])
            )
        except (TypeError, ValueError):
            pass

    provider = (
        captcha_provider
        or CAPTCHA_PROVIDER
        or os.environ.get("GROK2API_CAPTCHA_PROVIDER")
        or os.environ.get("CAPTCHA_PROVIDER")
        or "local"
    ).strip().lower()
    if provider not in {"local", "yescaptcha"}:
        provider = "local"
    try:
        globals()["CAPTCHA_PROVIDER"] = provider
    except Exception:
        pass

    if provider == "local":
        # Always inline in main container; ignore any external/custom URL.
        solver_url = _local_solver_base_url(local_solver_url)
        try:
            globals()["LOCAL_SOLVER_URL"] = solver_url
        except Exception:
            pass
        os.environ["GROK2API_LOCAL_SOLVER_URL"] = solver_url
        os.environ["LOCAL_SOLVER_URL"] = solver_url
        os.environ["GROK2API_YESCAPTCHA_ENDPOINT"] = solver_url
        os.environ["YESCAPTCHA_ENDPOINT"] = solver_url
        key = "local"
        # Gate: never spawn registration workers before the inline solver answers.
        # Lazy browser mode is fine (pool_ready may be false); HTTP /health is enough.
        solver_wait = wait_for_local_solver(solver_url)
        if not solver_wait.get("ready"):
            return {
                "ok": False,
                "error": solver_wait.get("error")
                or f"本地过盾未就绪: {solver_url}",
                "local_solver": solver_wait,
            }
    else:
        # Cloud YesCaptcha must not inherit local solver endpoint/key.
        try:
            globals()["LOCAL_SOLVER_URL"] = ""
        except Exception:
            pass
        for k in (
            "GROK2API_LOCAL_SOLVER_URL",
            "LOCAL_SOLVER_URL",
            "GROK2API_YESCAPTCHA_ENDPOINT",
            "YESCAPTCHA_ENDPOINT",
            "YESCAPTCHA_API_BASE",
        ):
            os.environ.pop(k, None)
        key = (
            yescaptcha_key
            or YESCAPTCHA_KEY
            or os.environ.get("GROK2API_YESCAPTCHA_KEY")
            or os.environ.get("YESCAPTCHA_API_KEY")
            or ""
        ).strip()
        if key == "local":
            key = ""
        if not key:
            return {
                "ok": False,
                "error": "YESCAPTCHA_KEY is required (set GROK2API_YESCAPTCHA_KEY, save in 协议注册配置, or pass yescaptcha_key)",
            }

    if key and key != YESCAPTCHA_KEY:
        # keep module attr in sync for subsequent workers
        try:
            globals()["YESCAPTCHA_KEY"] = key
        except Exception:
            pass

    try:
        n = int(count if count is not None else 1)
    except (TypeError, ValueError):
        n = 1
    n = max(1, n)

    try:
        workers = int(
            concurrency
            if concurrency is not None
            else DEFAULT_CONCURRENCY
        )
    except (TypeError, ValueError):
        workers = DEFAULT_CONCURRENCY
    requested_workers = workers
    workers = max(1, min(workers, MAX_CONCURRENCY, n))
    # Local Camoufox: never run more parallel registrations than local solver
    # can safely host (each browser ≈ hundreds of MB). Cap comes from
    # GROK2API_REG_LOCAL_CONCURRENCY (UI concurrency is honored up to this).
    if provider == "local":
        workers = max(1, min(workers, LOCAL_CAPTCHA_MAX_CONCURRENCY, n))

    try:
        stagger = int(stagger_ms if stagger_ms is not None else 400)
    except (TypeError, ValueError):
        stagger = 400
    stagger = max(0, min(stagger, 10_000))
    # Do not raise stagger here — batch runner / resume may apply MIN_STAGGER only
    # when env is set. Admin form value must win by default.

    # Build proxy pool once; each job picks one URL (rotation / random / sticky).
    proxy_pool = _proxy_pool(
        proxy,
        username=proxy_username,
        password=proxy_password,
    )
    try:
        from grok2api.config import XAI_PROXY_STRATEGY as _default_strat
    except Exception:
        _default_strat = "round_robin"
    proxy_strat = (proxy_strategy or _default_strat or "round_robin").strip().lower()
    proxy_val = _pick_proxy_from_pool(proxy_pool, strategy=proxy_strat, index=0)
    mail_prov, mail_key, mail_base, mail_dom = _resolve_mail_credentials(
        mail_provider=mail_provider,
        moemail_api_key=moemail_api_key,
        moemail_base_url=moemail_base_url,
        domain=domain,
    )
    moemail_api_key = mail_key
    moemail_base_url = mail_base
    domain = mail_dom

    # Single job — keep original response shape for UI compatibility.
    if n == 1:
        return _start_one_registration(
            yescaptcha_key=key,
            proxy=proxy_val,
            moemail_api_key=moemail_api_key,
            moemail_base_url=moemail_base_url,
            prefix=prefix,
            domain=domain,
            expiry_ms=expiry_ms,
            mail_provider=mail_prov,
        )

    batch_id = f"batch_{uuid.uuid4().hex[:12]}"
    # Snapshot keeps the full multi-line text so resume / UI can re-parse the pool.
    proxy_snapshot = (proxy or "\n".join(proxy_pool) or proxy_val or "").strip()
    reg_cfg = _snapshot_reg_config(
        captcha_provider=provider,
        yescaptcha_key=key,
        proxy=proxy_snapshot,
        moemail_api_key=moemail_api_key,
        moemail_base_url=moemail_base_url,
        prefix=prefix,
        domain=domain,
        expiry_ms=expiry_ms,
        concurrency=workers,
        stagger_ms=stagger,
        mail_provider=mail_prov,
    )
    reg_cfg["proxy_strategy"] = proxy_strat
    reg_cfg["proxy_pool_count"] = len(proxy_pool)
    batch = {
        "id": batch_id,
        "status": "running",
        "created_at": _now(),
        "updated_at": _now(),
        "count": n,
        "concurrency": workers,
        "stagger_ms": stagger,
        "session_ids": [],
        "adapter_build": ADAPTER_BUILD,
        "message": (
            f"batch started count={n} concurrency={workers}"
            + (f" (requested={requested_workers})" if requested_workers != workers else "")
            + (f" local_cap={LOCAL_CAPTCHA_MAX_CONCURRENCY}" if provider == "local" else "")
            + f" max={MAX_CONCURRENCY} inflight_global={_GLOBAL_REG_INFLIGHT_MAX}"
        ),
        "error": None,
        "finished": 0,
        "ok_count": 0,
        "fail_count": 0,
        "spawned": 0,
        "reg_config": reg_cfg,
        "owner_pid": os.getpid(),
        "runner_alive": True,
        "cancel_requested": False,
    }
    with _lock:
        _batches[batch_id] = batch
    _mirror_reg_batch(batch_id, batch)
    # Log batch start so 任务日志 has a running row even before the first
    # session finishes (previously only the terminal row was written).
    _record_register_task(
        task_id=batch_id,
        summary=f"协议注册批次启动 count={n} concurrency={workers}",
        status="running",
        ok=None,
        progress_done=0,
        progress_total=n,
        finished=False,
        detail={
            "batch_id": batch_id,
            "count": n,
            "concurrency": workers,
            "stagger_ms": stagger,
            "phase": "started",
            "adapter_build": ADAPTER_BUILD,
        },
    )

    started = _spawn_batch_runner(
        batch_id,
        remaining=n,
        concurrency=workers,
        stagger_ms=stagger,
        captcha_provider=provider,
        yescaptcha_key=key,
        proxy=proxy_snapshot,
        proxy_strategy=proxy_strat,
        moemail_api_key=moemail_api_key,
        moemail_base_url=moemail_base_url,
        prefix=prefix,
        domain=domain,
        expiry_ms=expiry_ms,
        mail_provider=mail_prov,
    )
    if not started.get("ok"):
        return started
    _ensure_registration_watchdog()

    # Brief wait so the first wave (up to `workers`) is usually visible to UI.
    time.sleep(min(0.45, 0.08 * workers + 0.08))
    with _lock:
        b = dict(_batches.get(batch_id) or batch)
        sids = list(b.get("session_ids") or [])
        sessions = [_compact_session(_sessions[s]) for s in sids if s in _sessions]

    return {
        "ok": True,
        "batch": True,
        "batch_id": batch_id,
        "count": n,
        "concurrency": workers,
        "stagger_ms": stagger,
        "session_ids": sids,
        "sessions": sessions,
        "adapter_build": ADAPTER_BUILD,
        "message": (
            f"batch started: count={n}, threads={workers} "
            f"(in-flight cap), queued/started={len(sids)}"
        ),
        # Back-compat: first session fields for old UI single-session path.
        **(sessions[0] if sessions else {"id": None, "status": "starting"}),
    }


def _spawn_batch_runner(
    batch_id: str,
    *,
    remaining: int,
    concurrency: int,
    stagger_ms: int,
    captcha_provider: str,
    yescaptcha_key: str,
    proxy: str,
    proxy_strategy: str | None = None,
    moemail_api_key: str | None,
    moemail_base_url: str | None,
    prefix: str | None,
    domain: str | None,
    expiry_ms: int | None,
    mail_provider: str | None = None,
) -> dict[str, Any]:
    """Start the ThreadPool spawner for a batch (also used by resume/reclaim)."""
    bid = str(batch_id or "").strip()
    if not bid:
        return {"ok": False, "error": "missing batch id"}
    batch = _load_reg_batch(bid)
    if not batch:
        return {"ok": False, "error": "registration batch not found"}

    if remaining <= 0:
        with _lock:
            b = _batches.get(bid) or dict(batch)
            b["runner_alive"] = False
            b["status"] = "done"
            b["updated_at"] = _now()
            b["message"] = "nothing to spawn"
            _batches[bid] = b
            _mirror_reg_batch(bid, dict(b))
        return {
            "ok": True,
            "batch_id": bid,
            "already_complete": True,
            "remaining": 0,
            "batch": get_registration_batch(bid),
        }

    acquired, lock_token = _try_acquire_batch_runner(bid)
    if not acquired:
        return {
            "ok": False,
            "error": "batch runner already active on another worker",
            "batch_id": bid,
            "already_running": True,
        }

    provider = (captcha_provider or "local").strip().lower()
    if provider not in {"local", "yescaptcha"}:
        provider = "local"
    key = (yescaptcha_key or "").strip()
    if provider == "local":
        key = "local"
        solver_url = _local_solver_base_url(None)
        try:
            globals()["CAPTCHA_PROVIDER"] = "local"
            globals()["LOCAL_SOLVER_URL"] = solver_url
        except Exception:
            pass
        os.environ["GROK2API_CAPTCHA_PROVIDER"] = "local"
        os.environ["CAPTCHA_PROVIDER"] = "local"
        os.environ["GROK2API_LOCAL_SOLVER_URL"] = solver_url
        os.environ["LOCAL_SOLVER_URL"] = solver_url
        os.environ["GROK2API_YESCAPTCHA_ENDPOINT"] = solver_url
        os.environ["YESCAPTCHA_ENDPOINT"] = solver_url
        solver_wait = wait_for_local_solver(solver_url)
        if not solver_wait.get("ready"):
            _release_batch_runner(bid, lock_token)
            return {
                "ok": False,
                "error": solver_wait.get("error")
                or f"本地过盾未就绪: {solver_url}",
                "batch_id": bid,
                "local_solver": solver_wait,
            }
    else:
        if not key:
            _release_batch_runner(bid, lock_token)
            return {
                "ok": False,
                "error": "YESCAPTCHA_KEY missing",
                "batch_id": bid,
            }
        try:
            globals()["CAPTCHA_PROVIDER"] = "yescaptcha"
            globals()["YESCAPTCHA_KEY"] = key
            globals()["LOCAL_SOLVER_URL"] = ""
        except Exception:
            pass
        for k in (
            "GROK2API_LOCAL_SOLVER_URL",
            "LOCAL_SOLVER_URL",
            "GROK2API_YESCAPTCHA_ENDPOINT",
            "YESCAPTCHA_ENDPOINT",
            "YESCAPTCHA_API_BASE",
        ):
            os.environ.pop(k, None)

    # `proxy` may be multi-line pool text; expand once for this runner.
    proxy_pool = _proxy_pool(proxy)
    try:
        from grok2api.config import XAI_PROXY_STRATEGY as _default_strat
    except Exception:
        _default_strat = "round_robin"
    proxy_strat = (proxy_strategy or _default_strat or "round_robin").strip().lower()
    proxy_snapshot = (proxy or "\n".join(proxy_pool) or "").strip()
    requested_workers = int(concurrency or DEFAULT_CONCURRENCY)
    workers = max(1, min(int(concurrency or DEFAULT_CONCURRENCY), MAX_CONCURRENCY, remaining))
    # captcha_provider may be passed in kwargs path; prefer batch reg_config.
    try:
        prov = str(captcha_provider or "").strip().lower()
    except Exception:
        prov = ""
    if not prov:
        try:
            with _lock:
                bb = _batches.get(str(batch_id or "")) or {}
            prov = str((bb.get("reg_config") or {}).get("captcha_provider") or "").lower()
        except Exception:
            prov = ""
    if prov == "local" or (not prov and str(os.environ.get("GROK2API_CAPTCHA_PROVIDER") or os.environ.get("CAPTCHA_PROVIDER") or "local").lower() == "local"):
        workers = max(1, min(workers, LOCAL_CAPTCHA_MAX_CONCURRENCY, remaining))
    stagger = max(0, min(int(stagger_ms or 400), 10_000))
    # Honor batch stagger; optional env floor only when MIN_STAGGER>0.
    try:
        min_stagger = int(os.environ.get("GROK2API_REG_MIN_STAGGER_MS", "0") or 0)
    except (TypeError, ValueError):
        min_stagger = 0
    if (prov == "local" or str(os.environ.get("GROK2API_CAPTCHA_PROVIDER") or "local").lower() == "local") and min_stagger > 0:
        stagger = max(stagger, min_stagger)

    with _lock:
        b = _batches.get(bid) or dict(batch)
        b["status"] = "running"
        b["cancel_requested"] = False
        b["concurrency"] = workers
        b["stagger_ms"] = stagger
        b["runner_alive"] = True
        b["owner_pid"] = os.getpid()
        b["adapter_build"] = ADAPTER_BUILD
        b["reg_config"] = _snapshot_reg_config(
            captcha_provider=provider,
            yescaptcha_key=key,
            proxy=proxy_snapshot,
            moemail_api_key=moemail_api_key,
            moemail_base_url=moemail_base_url,
            prefix=prefix,
            domain=domain,
            expiry_ms=expiry_ms,
            concurrency=workers,
            stagger_ms=stagger,
            mail_provider=mail_provider,
        )
        b["reg_config"]["proxy_strategy"] = proxy_strat
        b["reg_config"]["proxy_pool_count"] = len(proxy_pool)
        b["updated_at"] = _now()
        # Preserve historical counters when reclaiming after process restart;
        # only reset if this is a brand-new spawn with no prior progress.
        prior_finished = int(b.get("finished") or 0)
        prior_ok = int(b.get("ok_count") or 0)
        prior_fail = int(b.get("fail_count") or 0)
        b["message"] = (
            f"starting remaining={remaining} threads={workers}"
            + (f" requested={requested_workers}" if requested_workers != workers else "")
            + (f" already_done={prior_finished}" if prior_finished else "")
            + (f" proxies={len(proxy_pool)}" if proxy_pool else "")
        )
        if prior_finished <= 0 and not (b.get("session_ids") or []):
            b["finished"] = 0
            b["ok_count"] = 0
            b["fail_count"] = 0
        else:
            b["finished"] = prior_finished
            b["ok_count"] = prior_ok
            b["fail_count"] = prior_fail
        _batches[bid] = b
        _mirror_reg_batch(bid, dict(b))

    def _run_batch() -> None:
        from concurrent.futures import FIRST_COMPLETED, ThreadPoolExecutor, wait

        errors: list[str] = []
        with _lock:
            _seed = dict(_batches.get(bid) or batch or {})
        finished = int(_seed.get("finished") or 0)
        ok_n = int(_seed.get("ok_count") or 0)
        fail_n = int(_seed.get("fail_count") or 0)
        stop_renew = False
        # Feed the pool gradually: only keep ~workers(+prefetch) jobs prepared
        # at once. Submitting all remaining jobs up-front used to create hundreds
        # of mailboxes immediately and made stop/cancel racey under multi-thread.
        next_i = 1
        in_flight: dict[Any, int] = {}
        prefetch = max(0, min(int(REG_PREFETCH_SLOTS), max(0, workers)))
        # Never queue more work than global admission + local captcha can take.
        # Prefetch used to create extra mailboxes/sessions that pile up in RAM.
        max_inflight = max(1, min(workers + prefetch, _GLOBAL_REG_INFLIGHT_MAX, workers + max(1, prefetch)))

        def _batch_cancel_requested() -> bool:
            with _lock:
                local = _batches.get(bid) or {}
            if local.get("cancel_requested") or str(local.get("status") or "").lower() in (
                "stopping",
                "cancelled",
                "stopped",
            ):
                return True
            if not _reg_redis():
                return False
            try:
                from grok2api.store import sessions_redis

                remote = sessions_redis.reg_batch_get(bid)
                if not isinstance(remote, dict):
                    return False
                if remote.get("cancel_requested") or str(remote.get("status") or "").lower() in (
                    "stopping",
                    "cancelled",
                    "stopped",
                ):
                    with _lock:
                        cur = _batches.get(bid) or dict(remote)
                        cur["cancel_requested"] = True
                        if str(cur.get("status") or "").lower() not in (
                            "cancelled",
                            "stopped",
                            "done",
                            "partial",
                            "error",
                        ):
                            cur["status"] = remote.get("status") or "stopping"
                            if remote.get("message"):
                                cur["message"] = remote.get("message")
                        cur["updated_at"] = _now()
                        _batches[bid] = cur
                    return True
            except Exception:
                pass
            return False

        def _renew_loop() -> None:
            while not stop_renew:
                time.sleep(max(5.0, REG_BATCH_RUNNER_LOCK_TTL / 3))
                if stop_renew:
                    break
                if _batch_cancel_requested():
                    # Keep heartbeat while draining, but mark status as stopping.
                    with _lock:
                        bb = _batches.get(bid)
                        if bb is not None:
                            bb["cancel_requested"] = True
                            if str(bb.get("status") or "").lower() not in (
                                "cancelled",
                                "stopped",
                                "done",
                                "partial",
                                "error",
                            ):
                                bb["status"] = "stopping"
                            bb["updated_at"] = _now()
                            bb["runner_alive"] = True
                            _mirror_reg_batch(bid, dict(bb))
                _renew_batch_runner(bid, lock_token)
                with _lock:
                    bb = _batches.get(bid)
                    if bb is not None:
                        bb["updated_at"] = _now()
                        bb["runner_alive"] = True
                        bb["owner_pid"] = os.getpid()
                        _mirror_reg_batch(bid, dict(bb))

        renew_t = threading.Thread(
            target=_renew_loop,
            daemon=True,
            name=f"gba-batch-lock-{bid[-8:]}",
        )
        renew_t.start()

        def _job(i: int) -> dict[str, Any]:
            # Honour batch-level stop before creating more mailboxes.
            if _batch_cancel_requested():
                return {
                    "ok": False,
                    "id": None,
                    "status": "cancelled",
                    "error": "cancelled before start",
                }
            # One proxy per registration job (pool rotation / random / sticky).
            job_proxy = _pick_proxy_from_pool(
                proxy_pool, strategy=proxy_strat, index=max(0, int(i) - 1)
            )
            # Small per-slot stagger only (not cumulative across the whole batch).
            delay = (stagger / 1000.0) * ((i - 1) % max(1, workers))
            prepared = _prepare_registration_session(
                yescaptcha_key=key,
                proxy=job_proxy,
                moemail_api_key=moemail_api_key,
                moemail_base_url=moemail_base_url,
                prefix=prefix,
                domain=domain,
                expiry_ms=expiry_ms,
                mail_provider=mail_provider,
                batch_id=bid,
                batch_index=i,
                batch_total=int((_load_reg_batch(bid) or {}).get("count") or remaining),
                start_delay=delay,
            )
            if not prepared.get("ok"):
                return prepared
            sid = str(prepared.get("id") or "")
            with _lock:
                # Re-check cancel after prepare (user may stop mid-queue).
                b1 = _batches.get(bid) or {}
                sess = _sessions.get(sid) or {}
                if (
                    b1.get("cancel_requested")
                    or str(b1.get("status") or "").lower() in ("stopping", "cancelled", "stopped")
                    or sess.get("cancel_requested")
                ):
                    if sid in _sessions:
                        _sessions[sid]["status"] = "cancelled"
                        _sessions[sid]["message"] = "cancelled before worker start"
                        _sessions[sid]["error"] = "cancelled"
                        _sessions[sid]["cancel_requested"] = True
                        _sessions[sid]["updated_at"] = _now()
                        _sessions[sid].pop("_receiver", None)
                        _mirror_reg_sess(sid, _sessions[sid])
                    return {
                        "ok": False,
                        "id": sid,
                        "status": "cancelled",
                        "error": "cancelled",
                        "email": sess.get("email"),
                    }
                receiver = sess.get("_receiver")
                if sid in _sessions:
                    _sessions[sid]["status"] = "started"
                    _sessions[sid]["message"] = (
                        f"started; email={_sessions[sid].get('email') or ''}"
                    )
                    _sessions[sid]["updated_at"] = _now()
                    _mirror_reg_sess(sid, _sessions[sid])
            if not sid or receiver is None:
                return {"ok": False, "error": "registration session prepare failed", "id": sid}
            # Cross-batch admission control: many resumed batches otherwise all
            # run concurrency=8 and overwhelm local captcha + device-flow.
            admitted = False
            admission_flag = {"released": False}
            try:
                def _job_cancel() -> None:
                    with _lock:
                        sess2 = _sessions.get(sid) or {}
                        b2 = _batches.get(bid) or {}
                        if (
                            sess2.get("cancel_requested")
                            or b2.get("cancel_requested")
                        ):
                            raise _RegCancelled("cancelled while waiting for admission")

                def _job_admission_heartbeat(
                    waited: float,
                    admission: dict[str, int],
                ) -> None:
                    with _lock:
                        sess2 = _sessions.get(sid)
                        if not sess2 or str(sess2.get("status") or "") in _TERMINAL_STATUSES:
                            return
                        sess2["status"] = "waiting_admission"
                        sess2["message"] = (
                            f"waiting for registration admission ({waited:.0f}s; "
                            f"in_use={admission['in_use']}/{admission['limit']}; "
                            f"waiters={admission['waiters']})"
                        )
                        sess2["updated_at"] = _now()
                        _mirror_reg_sess(sid, sess2, force=True)

                _wait_reg_admission(
                    check_cancel=_job_cancel,
                    heartbeat=_job_admission_heartbeat,
                )
                admitted = True
                _run_registration(
                    sid,
                    key,
                    job_proxy or "",
                    receiver,
                    admission_flag=admission_flag,
                )
            except (AdmissionTimeout, _RegCancelled) as e:
                timed_out = isinstance(e, AdmissionTimeout)
                final_status = "error" if timed_out else "cancelled"
                with _lock:
                    if sid in _sessions:
                        _sessions[sid]["status"] = final_status
                        _sessions[sid]["error"] = str(e)
                        _sessions[sid]["message"] = str(e)
                        _sessions[sid]["updated_at"] = _now()
                        _mirror_reg_sess(sid, _sessions[sid], force=True)
                return {
                    "ok": False,
                    "id": sid,
                    "status": final_status,
                    "error": str(e),
                }
            finally:
                if admitted:
                    _release_reg_admission_once(admission_flag)
                with _lock:
                    if sid in _sessions:
                        _sessions[sid].pop("_receiver", None)
            with _lock:
                final = _sessions.get(sid) or {}
            st = str(final.get("status") or "")
            ok = st in ("imported", "success", "completed")
            return {
                "ok": ok,
                "id": sid,
                "status": st,
                "error": final.get("error"),
                "email": final.get("email"),
            }

        def _note_result(idx: int, r: dict[str, Any] | None = None, exc: Exception | None = None) -> None:
            nonlocal finished, ok_n, fail_n
            finished += 1
            if finished % 10 == 0:
                try:
                    _clean_old_sessions()
                except Exception:
                    pass
            if exc is not None:
                fail_n += 1
                errors.append(f"#{idx}: {exc}")
            elif not isinstance(r, dict):
                fail_n += 1
                errors.append(f"#{idx}: empty result")
            elif r.get("ok"):
                ok_n += 1
            else:
                fail_n += 1
                errors.append(
                    f"#{idx}: {r.get('error') or r.get('status') or 'failed'}"
                )
            with _lock:
                b = _batches.get(bid)
                if b is not None:
                    b["updated_at"] = _now()
                    # Don't clobber explicit stop marker.
                    if not b.get("cancel_requested"):
                        b["status"] = "running"
                    b["finished"] = finished
                    b["ok_count"] = ok_n
                    b["fail_count"] = fail_n
                    b["spawned"] = len(b.get("session_ids") or [])
                    b["spawn_errors"] = errors[-20:]
                    b["runner_alive"] = True
                    b["inflight"] = len(in_flight)
                    b["message"] = (
                        f"running {finished}/{target_total} done "
                        f"(ok={ok_n} fail={fail_n}, threads={workers}, "
                        f"inflight={len(in_flight)})"
                    )
                    _mirror_reg_batch(bid, dict(b))
                    # Real-time batch progress into 任务日志 (throttled).
                    try:
                        _throttle_task_log(
                            task_id=str(bid),
                            status="running",
                            summary=str(b.get("message") or f"协议注册批次 {bid}"),
                            progress_done=int(finished or 0),
                            progress_total=int(target_total or 0),
                            finished=False,
                            ok=None,
                            detail={
                                "batch_id": bid,
                                "ok_count": ok_n,
                                "fail_count": fail_n,
                                "threads": workers,
                                "inflight": len(in_flight),
                                "phase": "progress",
                                "adapter_build": ADAPTER_BUILD,
                            },
                            min_interval_sec=1.5,
                        )
                    except Exception:
                        pass

        try:
            target_total = int((_load_reg_batch(bid) or {}).get("count") or remaining)
            with ThreadPoolExecutor(
                max_workers=workers, thread_name_prefix=f"gba-batch-{bid[-6:]}"
            ) as pool:
                while True:
                    # Fill up to concurrency(+prefetch) only while not cancelled.
                    while (
                        next_i <= remaining
                        and len(in_flight) < max_inflight
                        and not _batch_cancel_requested()
                    ):
                        fut = pool.submit(_job, next_i)
                        in_flight[fut] = next_i
                        next_i += 1
                        with _lock:
                            bb = _batches.get(bid)
                            if bb is not None:
                                bb["inflight"] = len(in_flight)
                                bb["updated_at"] = _now()
                                if not bb.get("cancel_requested"):
                                    bb["status"] = "running"
                                bb["message"] = (
                                    f"running {finished}/{target_total} done "
                                    f"(ok={ok_n} fail={fail_n}, threads={workers}, "
                                    f"inflight={len(in_flight)})"
                                )
                                _mirror_reg_batch(bid, dict(bb))
                                try:
                                    _throttle_task_log(
                                        task_id=str(bid),
                                        status="running",
                                        summary=str(bb.get("message") or f"协议注册批次 {bid}"),
                                        progress_done=int(finished or 0),
                                        progress_total=int(target_total or 0),
                                        finished=False,
                                        ok=None,
                                        detail={
                                            "batch_id": bid,
                                            "ok_count": ok_n,
                                            "fail_count": fail_n,
                                            "threads": workers,
                                            "inflight": len(in_flight),
                                            "phase": "progress",
                                            "adapter_build": ADAPTER_BUILD,
                                        },
                                        min_interval_sec=1.5,
                                    )
                                except Exception:
                                    pass

                    if not in_flight:
                        break

                    done, _pending = wait(
                        set(in_flight.keys()),
                        return_when=FIRST_COMPLETED,
                        timeout=0.5,
                    )
                    if not done:
                        # Timeout tick: re-check cancel and refresh progress.
                        if _batch_cancel_requested():
                            # Stop feeding new jobs; still drain in-flight workers.
                            pass
                        continue
                    for fut in done:
                        idx = in_flight.pop(fut, 0)
                        try:
                            r = fut.result()
                            _note_result(idx, r=r)
                        except Exception as e:  # noqa: BLE001
                            _note_result(idx, exc=e)

                    # If cancelled and no more work in flight, exit promptly.
                    if _batch_cancel_requested() and not in_flight:
                        break
                    # If cancelled, do not submit more jobs even if capacity frees.
                    if _batch_cancel_requested():
                        continue
        finally:
            stop_renew = True
            # Best-effort cancel of any leftover futures (usually empty now).
            for fut in list(in_flight.keys()):
                try:
                    fut.cancel()
                except Exception:
                    pass
            with _lock:
                b = _batches.get(bid)
                if b is not None:
                    b["updated_at"] = _now()
                    b["finished"] = finished
                    b["ok_count"] = ok_n
                    b["fail_count"] = fail_n
                    b["spawned"] = len(b.get("session_ids") or [])
                    b["spawn_errors"] = errors[-20:]
                    b["runner_alive"] = False
                    b["inflight"] = 0
                    target_total = int(b.get("count") or finished or 0)
                    cancelled = bool(b.get("cancel_requested")) or str(b.get("status") or "").lower() in (
                        "stopping",
                        "cancelled",
                        "stopped",
                    )
                    if cancelled and finished < target_total:
                        b["status"] = "cancelled"
                        b["message"] = (
                            f"stopped {finished}/{target_total} "
                            f"(ok={ok_n} fail={fail_n}, threads={workers})"
                        )
                    elif fail_n and not ok_n:
                        b["status"] = "error"
                        b["error"] = "; ".join(errors[:5]) or "all failed"
                        b["message"] = (
                            f"finished {finished}/{target_total} "
                            f"(ok={ok_n} fail={fail_n}, threads={workers})"
                            + (f"; errors={len(errors)}" if errors else "")
                        )
                    elif fail_n:
                        b["status"] = "partial"
                        b["message"] = (
                            f"finished {finished}/{target_total} "
                            f"(ok={ok_n} fail={fail_n}, threads={workers})"
                            + (f"; errors={len(errors)}" if errors else "")
                        )
                    else:
                        b["status"] = "done"
                        b["message"] = (
                            f"finished {finished}/{target_total} "
                            f"(ok={ok_n} fail={fail_n}, threads={workers})"
                        )
                    _mirror_reg_batch(bid, dict(b))
                    st = str(b.get("status") or "done")
                    _record_register_task(
                        task_id=str(bid),
                        summary=str(b.get("message") or f"协议注册批次 {bid}"),
                        status=st,
                        ok=st in {"done", "partial"} and ok_n > 0,
                        progress_done=int(finished or 0),
                        progress_total=int(target_total or finished or 0),
                        finished=True,
                        detail={
                            "batch_id": bid,
                            "ok_count": ok_n,
                            "fail_count": fail_n,
                            "threads": workers,
                            "status": st,
                            "errors": (errors or [])[:10],
                            "phase": "finished",
                            "adapter_build": ADAPTER_BUILD,
                        },
                    )
            _release_batch_runner(bid, lock_token)

    threading.Thread(
        target=_run_batch,
        daemon=True,
        name=f"gba-batch-{bid[-8:]}",
    ).start()

    return {
        "ok": True,
        "batch_id": bid,
        "remaining": remaining,
        "concurrency": workers,
        "message": f"started batch {bid}: remaining={remaining} threads={workers}",
    }


def _run_registration(
    sid: str,
    yescaptcha_key: str,
    proxy: str,
    receiver: Any,
    admission_flag: dict[str, bool] | None = None,
) -> None:
    with _lock:
        sess = _sessions.get(sid)
    if not sess:
        # Another worker may hold the durable copy; still try to load.
        sess = _load_reg_sess(sid)
    if not sess:
        return
    # Re-bind process-local map so later progress stays readable on this worker.
    with _lock:
        _sessions[sid] = sess

    def _refresh_cancel_from_redis() -> None:
        """Pull cancel_requested from Redis so multi-worker stop works.

        Also honour batch-level stop so stopping a batch reaches in-flight
        sessions even if the session mirror lags.
        """
        if not _reg_redis():
            return
        try:
            from grok2api.store import sessions_redis

            remote = sessions_redis.reg_sess_get(sid)
            batch_cancel = False
            remote_batch = None
            bid = ""
            with _lock:
                local_sess = _sessions.get(sid) or sess or {}
                bid = str(local_sess.get("batch_id") or "")
            if not bid and isinstance(remote, dict):
                bid = str(remote.get("batch_id") or "")
            if bid:
                try:
                    remote_batch = sessions_redis.reg_batch_get(bid)
                except Exception:
                    remote_batch = None
                if isinstance(remote_batch, dict) and (
                    remote_batch.get("cancel_requested")
                    or _is_cancel_status(remote_batch.get("status"))
                ):
                    batch_cancel = True
                    with _lock:
                        bb = _batches.get(bid) or dict(remote_batch)
                        bb["cancel_requested"] = True
                        if str(bb.get("status") or "").lower() not in (
                            "cancelled",
                            "stopped",
                            "done",
                            "partial",
                            "error",
                        ):
                            bb["status"] = remote_batch.get("status") or "stopping"
                        bb["updated_at"] = _now()
                        _batches[bid] = bb

            sess_cancel = isinstance(remote, dict) and (
                remote.get("cancel_requested")
                or _is_cancel_status(remote.get("status"))
            )
            if not sess_cancel and not batch_cancel:
                return
            with _lock:
                cur = _sessions.get(sid) or sess
                cur["cancel_requested"] = True
                if str(cur.get("status") or "").lower() not in _TERMINAL_STATUSES:
                    if sess_cancel and str(remote.get("status") or "").lower() in (
                        "stopping",
                        "cancelled",
                        "stopped",
                    ):
                        cur["status"] = remote.get("status") or "stopping"
                        if remote.get("message"):
                            cur["message"] = remote.get("message")
                    elif batch_cancel:
                        cur["status"] = "stopping"
                        cur["message"] = "stop requested via batch"
                _sessions[sid] = cur
        except Exception:
            pass

    def update(status: str, message: str, **kwargs: Any) -> None:
        _refresh_cancel_from_redis()
        write_task_now = False
        task_payload: dict[str, Any] | None = None
        with _lock:
            cur = _sessions.get(sid) or sess
            # Batch-level cancel also aborts this worker.
            bid = str(cur.get("batch_id") or "")
            batch_hit = False
            if bid:
                bb = _batches.get(bid) or {}
                if bb.get("cancel_requested") or _is_cancel_status(bb.get("status")):
                    batch_hit = True
                    cur["cancel_requested"] = True
            # Do not overwrite a terminal cancel with intermediate progress.
            if (_session_cancel_requested(cur) or batch_hit) and status not in (
                "cancelled",
                "stopped",
                "error",
                "imported",
            ):
                raise _RegCancelled(cur.get("message") or "cancelled by user")
            cur["status"] = status
            cur["message"] = message
            cur["updated_at"] = _now()
            cur.update(kwargs)
            # Real-time progress lines for admin UI (mirrored via Redis).
            try:
                _append_session_log(cur, status, message)
            except Exception:
                pass
            _sessions[sid] = cur
            # Force Redis write on terminal / cancel so multi-worker UI sees exit ASAP.
            _mirror_reg_sess(
                sid,
                cur,
                force=bool(
                    str(status or "").lower() in _TERMINAL_STATUSES
                    or str(status or "").lower() in ("stopping", "cancelled", "stopped")
                ),
            )
            # Single-session jobs: stream progress into task_logs (throttled).
            # Batch sessions are summarized on the batch row to avoid N*step spam.
            if not bid:
                write_task_now = True
                st_l = str(status or "").lower()
                terminal = st_l in _TERMINAL_STATUSES
                task_payload = {
                    "task_id": sid,
                    "summary": str(message or status or f"协议注册 {email or sid}")[:500],
                    "status": "running" if not terminal else st_l,
                    "ok": (
                        True
                        if st_l in ("imported", "success", "completed", "done")
                        else False
                        if terminal
                        else None
                    ),
                    "progress_done": 1
                    if terminal and st_l in ("imported", "success", "completed", "done")
                    else 0,
                    "progress_total": 1,
                    "finished": terminal,
                    "detail": {
                        "session_id": sid,
                        "email": email or cur.get("email"),
                        "status": st_l,
                        "phase": "progress" if not terminal else "finished",
                        "log_tail": list(cur.get("log_lines") or [])[-24:],
                        "adapter_build": ADAPTER_BUILD,
                        "error": cur.get("error"),
                    },
                    "force": terminal,
                }
        if write_task_now and task_payload:
            try:
                _throttle_task_log(
                    task_id=str(task_payload["task_id"]),
                    status=str(task_payload["status"]),
                    summary=str(task_payload["summary"]),
                    progress_done=int(task_payload["progress_done"] or 0),
                    progress_total=int(task_payload["progress_total"] or 0),
                    finished=bool(task_payload["finished"]),
                    ok=task_payload.get("ok"),
                    detail=task_payload.get("detail")
                    if isinstance(task_payload.get("detail"), dict)
                    else {},
                    min_interval_sec=1.0,
                    force=bool(task_payload.get("force")),
                )
            except Exception:
                pass

    def _check_cancel() -> None:
        _refresh_cancel_from_redis()
        with _lock:
            cur = _sessions.get(sid) or sess
            bid = str(cur.get("batch_id") or "")
            if bid:
                bb = _batches.get(bid) or {}
                if bb.get("cancel_requested") or _is_cancel_status(bb.get("status")):
                    cur["cancel_requested"] = True
                    _sessions[sid] = cur
        if _session_cancel_requested(cur):
            raise _RegCancelled(cur.get("message") or "cancelled by user")

    email = str(sess.get("email") or "").strip().lower()
    password = sess.get("password") or ""
    if not password:
        update("error", "missing password for registration session", error="missing password")
        return
    sess["email"] = email
    client = None

    try:
        _check_cancel()
        ensure_xconsole()
        from xconsole_client import (
            XConsoleAuthClient,
            YesCaptchaSolver,
            xai_oauth_login_protocol,
        )
        from xconsole_client import config as C
        from xconsole_client.oauth_protocol import extract_cookies_from_auth_client
        from xconsole_client.xai_oauth import (
            CLIPROXYAPI_GROK_HEADERS,
            build_cliproxyapi_auth_record,
        )
        import grok2api.pool.accounts as accounts
        from grok2api.config import UPSTREAM_BASE

        update("registering", "visiting signup page")
        _check_cancel()
        client = XConsoleAuthClient(
            debug=True,
            proxy=proxy or "",
            signup_url="https://accounts.x.ai/sign-up?redirect=grok-com",
        )
        client.visit_home()
        _check_cancel()
        client.load_signup_page()

        sitekey = (
            getattr(client, "turnstile_sitekey", None)
            or getattr(C, "TURNSTILE_SITEKEY", None)
            or ""
        ).strip()
        website_url = (getattr(client, "signup_url", None) or C.SIGNUP_URL or "").strip()
        if not sitekey:
            raise RuntimeError(
                "Turnstile sitekey missing. Signup page scrape failed and "
                "config TURNSTILE_SITEKEY is empty."
            )

        provider = (
            CAPTCHA_PROVIDER
            or os.environ.get("GROK2API_CAPTCHA_PROVIDER")
            or os.environ.get("CAPTCHA_PROVIDER")
            or "local"
        ).strip().lower()
        if provider not in {"local", "yescaptcha"}:
            provider = "local"

        if provider == "local":
            # Always use in-container inline solver; ignore external/custom URL.
            endpoint = _local_solver_base_url(None)
            solver_key = "local"
            auto_fallback = False
            # Re-check right before first solve so a mid-batch solver restart
            # doesn't burn mailboxes while HTTP is still down.
            wait = wait_for_local_solver(
                endpoint,
                timeout_sec=min(60.0, max(5.0, LOCAL_SOLVER_WAIT_SEC)),
                progress=lambda m: update("waiting_solver", m),
            )
            if not wait.get("ready"):
                raise RuntimeError(
                    wait.get("error")
                    or f"本地过盾未就绪，无法开始打码: {endpoint}"
                )
        else:
            # Cloud YesCaptcha only; never inherit local solver endpoint.
            endpoint = (
                os.environ.get("GROK2API_YESCAPTCHA_ENDPOINT")
                or os.environ.get("YESCAPTCHA_ENDPOINT")
                or os.environ.get("YESCAPTCHA_API_BASE")
                or ""
            ).strip() or None
            # Guard against accidental local leftover endpoint.
            if endpoint and (
                "127.0.0.1" in endpoint
                or "localhost" in endpoint
                or endpoint.rstrip("/").endswith(":5072")
            ):
                endpoint = None
            solver_key = (
                yescaptcha_key
                or YESCAPTCHA_KEY
                or os.environ.get("GROK2API_YESCAPTCHA_KEY")
                or os.environ.get("YESCAPTCHA_API_KEY")
                or ""
            ).strip()
            if not solver_key or solver_key == "local":
                raise RuntimeError("YesCaptcha 模式需要有效的 YESCAPTCHA_KEY")
            auto_fallback = True

        def _turnstile_progress(msg: str) -> None:
            # Raise cancel out of solver polling so stop doesn't wait full captcha timeout.
            _check_cancel()
            update("solving_turnstile", f"Turnstile: {msg}")

        solver = YesCaptchaSolver(
            solver_key,
            endpoint=endpoint,
            # Keep captcha wait bounded; cancel still interrupts via on_progress.
            timeout=float(os.environ.get("GROK2API_YESCAPTCHA_TIMEOUT", "120") or 120),
            poll_interval=float(os.environ.get("GROK2API_YESCAPTCHA_POLL", "2") or 2),
            debug=True,
            on_progress=_turnstile_progress,
            # Local: no cloud fallback. YesCaptcha: allow cn/global peer fallback.
            auto_fallback_endpoint=auto_fallback,
        )
        print(
            f"[grok-build-auth] turnstile provider={provider} website_url={website_url} "
            f"sitekey={sitekey} endpoint={getattr(solver, '_endpoint', '?')}"
        )

        # Critical ordering:
        # 1) solve Turnstile first (slow, ~20-40s)
        # 2) send email code
        # 3) wait for mailbox code
        # 4) immediately verify + create_account
        # Old order verified the code then waited for captcha; create_account then
        # failed with WKE=email:invalid-validation-code because the code expired /
        # was single-use after the slow captcha step.
        solver_label = "本地过盾" if provider == "local" else "YesCaptcha"
        update("solving_turnstile", f"solving Turnstile via {solver_label} (before email code)")
        _check_cancel()

        def _solve_turnstile(url: str, *, premium: bool = True) -> Any:
            # Local inline solver is single-process and browser-backed; concurrent
            # createTask storms from many registration workers cause timeouts /
            # mixed results. Serialize local solves while keeping YesCaptcha parallel.
            # Local Camoufox has no premium tier — force Proxyless only.
            use_premium = bool(premium) and provider != "local"
            kwargs = {
                "website_url": url,
                "website_key": sitekey,
                "premium": use_premium,
                "fallback_non_premium": True,
            }
            if provider != "local":
                return solver.solve_turnstile(**kwargs)

            # Local path: semaphore permits up to browser-pool size concurrent
            # solves. Queue heartbeats keep registration sessions alive while
            # waiting for a free Camoufox slot.
            wait_started = time.time()
            last_beat = 0.0
            while True:
                _check_cancel()
                acquired = _local_captcha_sem.acquire(timeout=5.0)
                if not acquired:
                    waited = time.time() - wait_started
                    if waited - last_beat >= 12.0:
                        last_beat = waited
                        update(
                            "solving_turnstile",
                            f"queued for local Turnstile ({int(waited)}s, "
                            f"slots={_local_captcha_slots_n}) [{ADAPTER_BUILD}]",
                        )
                    continue
                try:
                    waited = time.time() - wait_started
                    if waited >= 1.0:
                        update(
                            "solving_turnstile",
                            f"solving Turnstile via {solver_label} "
                            f"(after {int(waited)}s queue) [{ADAPTER_BUILD}]",
                        )
                    else:
                        update(
                            "solving_turnstile",
                            f"solving Turnstile via {solver_label} (before email code)",
                        )
                    _check_cancel()
                    try:
                        return solver.solve_turnstile(**kwargs)
                    except Exception as e:
                        # Camoufox queue meltdown / timeout → brief global pause
                        msg = str(e).lower()
                        if any(
                            k in msg
                            for k in (
                                "timeout",
                                "timed out",
                                "queue",
                                "busy",
                                "no browser",
                                "target closed",
                                "crashed",
                            )
                        ):
                            # Short pressure only — multi-thread already paces via slots.
                            _note_reg_pressure(f"local captcha: {e}", pause_sec=4)
                        raise
                finally:
                    try:
                        _local_captcha_sem.release()
                    except Exception:
                        pass

        try:
            # Local: Proxyless only. Remote YesCaptcha: premium M1 first.
            turnstile = _solve_turnstile(website_url, premium=(provider != "local"))
        except _RegCancelled:
            raise
        except Exception as captcha_err:
            _check_cancel()
            _note_reg_pressure(f"primary turnstile: {captcha_err}", pause_sec=6)
            alt_urls = [
                "https://accounts.x.ai/sign-up?redirect=cloud-console",
                "https://accounts.x.ai/sign-up?redirect=grok-com",
                "https://accounts.x.ai/sign-up",
            ]
            # Prefer an alternate redirect that differs from the primary page.
            alts = [u for u in alt_urls if u.rstrip("/") != website_url.rstrip("/")]
            if not alts:
                alts = alt_urls
            last_cap_err: Exception | None = captcha_err
            turnstile = None
            for ai, alt_url in enumerate(alts[:2]):
                update(
                    "solving_turnstile",
                    f"primary Turnstile failed ({captcha_err}); "
                    f"retry alt {ai + 1}/{min(2, len(alts))} {alt_url}",
                )
                try:
                    # First alt uses non-premium; second flips premium for remote.
                    use_prem = (provider != "local") and (ai == 1)
                    turnstile = _solve_turnstile(alt_url, premium=use_prem)
                    if turnstile:
                        website_url = alt_url  # keep subsequent refresh aligned
                        break
                except _RegCancelled:
                    raise
                except Exception as alt_err:  # noqa: BLE001
                    last_cap_err = alt_err
                    print(f"[grok-build-auth] alt turnstile failed: {alt_err}")
                    continue
            if not turnstile:
                raise RuntimeError(
                    f"Turnstile solve failed after primary+alt: {last_cap_err}"
                ) from last_cap_err
        if not turnstile:
            raise RuntimeError("YesCaptcha returned empty Turnstile token")
        _check_cancel()

        # Password can be validated any time before create; do it while warm.
        client.validate_password(email, password)

        update("registering", "sending email validation code")
        _check_cancel()
        send_res = client.create_email_validation_code(email)
        if hasattr(send_res, "ok") and send_res.ok is False:
            print(
                f"[grok-build-auth] CreateEmailValidationCode ok=False "
                f"http={getattr(send_res, 'http_status', None)} "
                f"grpc={getattr(send_res, 'grpc_status', None)}"
            )

        update("waiting_email", "waiting for xAI verification code")
        # Poll mailbox with cancel-aware receiver so stop lands in ~0.25–1s.
        _check_cancel()

        def _mail_should_cancel() -> bool:
            # _check_cancel raises _RegCancelled when stop is requested.
            _check_cancel()
            return False

        def _mail_on_tick(*, elapsed: float = 0.0, remaining: float = 0.0, **_kw) -> None:
            # Heartbeat keeps admin progress log fresh during long mailbox waits.
            update(
                "waiting_email",
                f"waiting for xAI verification code · {int(elapsed)}s elapsed"
                + (f", {int(remaining)}s left" if remaining else ""),
            )

        try:
            code = receiver.wait_for_code(
                timeout=120.0,
                should_cancel=_mail_should_cancel,
                poll_interval=1.0,
                on_tick=_mail_on_tick,
            )
        except TypeError:
            # Older receiver signature fallback (no on_tick / should_cancel).
            code = None
            mail_deadline = time.time() + 120.0
            mail_started = time.time()
            while time.time() < mail_deadline:
                _check_cancel()
                try:
                    code = receiver.wait_for_code(
                        timeout=min(4.0, max(1.0, mail_deadline - time.time()))
                    )
                except Exception:
                    code = None
                if code:
                    break
                _mail_on_tick(
                    elapsed=time.time() - mail_started,
                    remaining=max(0.0, mail_deadline - time.time()),
                )
        if not code:
            raise RuntimeError("email verification code timeout")
        code = str(code or "").strip().upper().replace(" ", "").replace("-", "")
        if len(code) != 6:
            raise RuntimeError(
                f"invalid email verification code shape: {code!r} "
                f"(expect 6 alnum chars)"
            )
        update("registering", f"code received: {code}; verifying + creating immediately")

        # Prefer empty castle token (YesCaptcha cannot mint Castle fingerprints).
        # Retry create_account with a fresh Turnstile (+ email code when needed)
        # on structured hard errors (expired code / turnstile / CF hold / rate).
        try:
            create_attempts = int(
                os.environ.get("GROK2API_REG_CREATE_ATTEMPTS", "3") or 3
            )
        except (TypeError, ValueError):
            create_attempts = 3
        create_attempts = max(2, min(5, create_attempts))
        res = None
        sc: list[str] = []
        rsc_body = ""
        rsc_preview = ""
        http_status = 0
        signup_err: str | None = None
        need_fresh_email_code = False

        def _signup_err_recoverable(err: str | None) -> bool:
            low = str(err or "").lower()
            if not low:
                return False
            needles = (
                "turnstile",
                "rate_limited",
                "rate limit",
                "captcha",
                "account_signup_error",
                "validation_error",
                "invalid-validation-code",
                "invalid_verification_code",
                "access_denied",
                "cf-",
                "challenge",
                "bot",
                "perimeter",
                "blocked",
                "forbidden",
                "temporarily",
                "try again",
                "wke=email:invalid-validation-code",
            )
            return any(n in low for n in needles)

        def _signup_err_needs_new_code(err: str | None) -> bool:
            low = str(err or "").lower()
            return any(
                n in low
                for n in (
                    "invalid-validation-code",
                    "invalid_verification_code",
                    "email validation code is invalid",
                    "wke=email:invalid-validation-code",
                    "expired",
                )
            )

        for ca in range(1, create_attempts + 1):
            if ca > 1:
                # Full refresh path for invalid code / captcha / CF hold failures.
                update(
                    "solving_turnstile",
                    f"create_account hard error ({signup_err}); "
                    f"refreshing Turnstile"
                    + ("+email code" if need_fresh_email_code else "")
                    + f" (attempt {ca}/{create_attempts})",
                )
                # Brief cool-down so CF/Turnstile risk score can drop.
                try:
                    cool = float(
                        os.environ.get("GROK2API_REG_CREATE_COOLDOWN_SEC", "2.5")
                        or 2.5
                    )
                except (TypeError, ValueError):
                    cool = 2.5
                cool = max(0.5, min(15.0, cool))
                time.sleep(cool + 0.4 * (ca - 1))
                try:
                    # Alternate premium/non-premium across attempts for remote.
                    use_prem = (provider != "local") and (ca % 2 == 1)
                    turnstile = _solve_turnstile(website_url, premium=use_prem)
                except Exception as captcha_err:  # noqa: BLE001
                    print(f"[grok-build-auth] turnstile refresh failed: {captcha_err}")
                    _note_reg_pressure(f"create refresh captcha: {captcha_err}", pause_sec=12)
                    break
                if need_fresh_email_code:
                    # New email code required after invalid-validation-code.
                    try:
                        client.create_email_validation_code(email)
                        update("waiting_email", "waiting for fresh xAI verification code")
                        try:
                            code = receiver.wait_for_code(
                                timeout=120,
                                should_cancel=_mail_should_cancel,
                                poll_interval=1.0,
                                on_tick=_mail_on_tick,
                            )
                        except TypeError:
                            code = receiver.wait_for_code(timeout=120)
                        code = (
                            str(code or "")
                            .strip()
                            .upper()
                            .replace(" ", "")
                            .replace("-", "")
                        )
                        if len(code) != 6:
                            raise RuntimeError(f"fresh email code invalid: {code!r}")
                        update("registering", f"fresh code received: {code}")
                    except Exception as mail_err:  # noqa: BLE001
                        print(f"[grok-build-auth] email code refresh failed: {mail_err}")
                        break

            # verify immediately before create_account (same second when possible)
            try:
                vres = client.verify_email_validation_code(email, code)
                print(
                    f"[grok-build-auth] VerifyEmailValidationCode "
                    f"ok={getattr(vres, 'ok', None)} "
                    f"http={getattr(vres, 'http_status', None)} "
                    f"grpc={getattr(vres, 'grpc_status', None)}"
                )
            except Exception as v_err:  # noqa: BLE001
                print(f"[grok-build-auth] verify_email error: {v_err}")

            update(
                "creating_account",
                f"creating xAI account (attempt {ca}/{create_attempts})",
            )
            res = client.create_account(
                email=email,
                given_name="User",
                family_name="Grok",
                password=password,
                email_validation_code=code,
                turnstile_token=turnstile,
                castle_request_token="",
                conversion_id=str(uuid.uuid4()),
            )
            sc = list(getattr(res, "set_cookies", None) or [])
            rsc_body = getattr(res, "rsc_body", "") or ""
            rsc_preview = rsc_body[:800]
            http_status = int(getattr(res, "http_status", 0) or 0)
            try:
                signup_err = client.extract_signup_error(rsc_body)
            except Exception:
                signup_err = None
            print(f"[grok-build-auth] create_account HTTP={http_status}")
            print(f"[grok-build-auth] create_account set-cookies count={len(sc)}")
            print(f"[grok-build-auth] create_account ok={bool(getattr(res, 'ok', False))}")
            print(f"[grok-build-auth] create_account error={signup_err!r}")
            print(f"[grok-build-auth] create_account rsc_body preview: {rsc_preview}")
            print(f"[grok-build-auth] adapter_build={ADAPTER_BUILD}")
            sess["create_account_http"] = http_status
            sess["create_account_ok_flag"] = bool(getattr(res, "ok", False))
            sess["create_account_set_cookies"] = len(sc)
            sess["create_account_error"] = signup_err
            sess["create_account_attempt"] = ca

            # Persist full body for offline diagnosis (truncated).
            try:
                debug_path = (
                    ROOT / "data" / "register_sso" / f"{sid}.create_account.rsc.txt"
                )
                debug_path.parent.mkdir(parents=True, exist_ok=True)
                debug_path.write_text(rsc_body[:200_000], encoding="utf-8")
            except Exception:
                pass

            if http_status != 200:
                # Non-200: retry on 403/408/429/5xx (CF hold / rate / upstream flake).
                retryable_http = http_status in (403, 408, 409, 425, 429) or http_status >= 500
                if retryable_http and ca < create_attempts:
                    need_fresh_email_code = False
                    _note_reg_pressure(
                        f"create_account HTTP {http_status}",
                        pause_sec=6 if http_status in (403, 429) else 4,
                    )
                    signup_err = signup_err or f"http_{http_status}"
                    continue
                raise RuntimeError(
                    "create_account transport failed. "
                    f"adapter_build={ADAPTER_BUILD}; HTTP {http_status}; "
                    f"error={signup_err!r}; set_cookies={len(sc)}; "
                    f"body_preview={rsc_preview!r}"
                )

            # Structured hard error: retry with fresh captcha when recoverable.
            if signup_err:
                recoverable = _signup_err_recoverable(signup_err)
                if recoverable and ca < create_attempts:
                    need_fresh_email_code = _signup_err_needs_new_code(signup_err)
                    if any(
                        x in str(signup_err).lower()
                        for x in ("turnstile", "captcha", "rate_limited", "rate limit")
                    ):
                        _note_reg_pressure(
                            f"create_account recoverable: {signup_err}",
                            pause_sec=8,
                        )
                    continue
                raise RuntimeError(
                    "create_account rejected by xAI. "
                    f"adapter_build={ADAPTER_BUILD}; HTTP {http_status}; "
                    f"error={signup_err!r}; set_cookies={len(sc)}; "
                    f"body_preview={rsc_preview!r}"
                )

            # HTTP 200 without structured error — proceed even if res.ok is False
            # due to historical false negatives on RSC-only flights.
            break

        update(
            "fetching_sso",
            f"create_account HTTP {http_status} accepted; extracting SSO [{ADAPTER_BUILD}]",
        )
        # Mark that xAI accepted create_account. Even if SSO extraction fails,
        # the mailbox is usually consumed and the remote account may already exist.
        try:
            sess["account_created"] = True
            sess["create_http_status"] = http_status
            sess["create_ok"] = bool(getattr(res, "ok", False))
        except Exception:
            pass

        sso = None
        sso_attempts: list[str] = []
        try:
            sso = client.fetch_sso_token(
                email=email, password=password, save=True, retries=5
            )
            if sso:
                sso_attempts.append("fetch_sso_token")
        except Exception as sso_fetch_err:  # noqa: BLE001
            print(f"[grok-build-auth] fetch_sso_token error: {sso_fetch_err}")
            sso_attempts.append(f"fetch_sso_token_err:{sso_fetch_err}")

        if not sso:
            try:
                from xconsole_client.sso import (
                    SSOExtractor,
                    parse_all_set_cookie_urls,
                    parse_sso_from_set_cookies,
                    parse_sso_jwt_url,
                    parse_sso_token_from_text,
                )

                sso = parse_sso_from_set_cookies(sc) or parse_sso_token_from_text(
                    rsc_body
                )
                if sso:
                    sso_attempts.append("parse_set_cookie_or_rsc")
                if not sso and rsc_body:
                    print(
                        f"[grok-build-auth] set-cookie candidates="
                        f"{parse_all_set_cookie_urls(rsc_body)[:3]}"
                    )
                    print(
                        f"[grok-build-auth] primary set-cookie url="
                        f"{parse_sso_jwt_url(rsc_body)}"
                    )
                    extractor = SSOExtractor(
                        transport_request=client._request,
                        base_headers=client._base_headers,
                        cookie_jar=client._t.cookies,
                        debug=True,
                    )
                    sso = extractor.extract(
                        rsc_body, email=email, password=password, save=False
                    )
                    if sso:
                        sso_attempts.append("SSOExtractor")
            except Exception as recover_err:  # noqa: BLE001
                print(f"[grok-build-auth] SSO recover failed: {recover_err}")
                sso_attempts.append(f"rsc_recover_err:{recover_err}")

        # Current xAI create_account often returns only RSC chunks + CF cookies,
        # with no set-cookie JWT chain. Fall back to password CreateSession and
        # treat the returned session JWT as the sso cookie for sso_to_auth_json.
        #
        # Bulk registration hits a race: account is created (HTTP 200) but not
        # yet visible to CreateSession for a few seconds. Multi-round login with
        # fresh turnstile + propagation backoff recovers most SSO_COOKIE_MISSING.
        if not sso:
            try:
                sso_rounds = max(
                    1,
                    min(
                        8,
                        int(os.environ.get("GROK2API_REG_SSO_LOGIN_ROUNDS", "5") or 5),
                    ),
                )
            except (TypeError, ValueError):
                sso_rounds = 5
            try:
                sso_prop_sec = max(
                    0.5,
                    min(
                        20.0,
                        float(
                            os.environ.get("GROK2API_REG_SSO_PROPAGATE_SEC", "2.5")
                            or 2.5
                        ),
                    ),
                )
            except (TypeError, ValueError):
                sso_prop_sec = 2.5

            update(
                "fetching_sso",
                f"RSC has no sso chain; CreateSession password fallback "
                f"rounds={sso_rounds} [{ADAPTER_BUILD}]",
            )
            signin_url = "https://accounts.x.ai/sign-in?redirect=grok-com"
            # Initial propagation wait before first login attempt.
            time.sleep(sso_prop_sec)
            for round_i in range(1, sso_rounds + 1):
                _check_cancel()
                if sso:
                    break
                update(
                    "fetching_sso",
                    f"CreateSession password fallback "
                    f"{round_i}/{sso_rounds} [{ADAPTER_BUILD}]",
                )
                try:
                    # Alternate premium/non-premium for remote captcha; local is
                    # always proxyless. Round 1 prefers premium when available.
                    use_premium = (provider != "local") and (round_i % 2 == 1)
                    try:
                        signin_turnstile = _solve_turnstile(
                            signin_url, premium=use_premium
                        )
                    except Exception as ts_err:  # noqa: BLE001
                        print(
                            f"[grok-build-auth] sign-in turnstile round "
                            f"{round_i} failed: {ts_err}"
                        )
                        # Reuse signup token for first 2 rounds — still often
                        # accepted by CreateSession and avoids a second captcha
                        # under multi-thread pressure.
                        signin_turnstile = turnstile if round_i <= 2 else None
                    if not signin_turnstile:
                        sso_attempts.append(f"round{round_i}:no_turnstile")
                        time.sleep(min(8.0, sso_prop_sec + round_i))
                        continue
                    try:
                        sso = client.obtain_session_via_password(
                            email=email,
                            password=password,
                            turnstile_token=signin_turnstile,
                            referer=signin_url,
                            retries=max(2, 5 - round_i // 2),
                        )
                    except Exception as cs_err:  # noqa: BLE001
                        print(
                            f"[grok-build-auth] CreateSession round "
                            f"{round_i} failed: {cs_err}"
                        )
                        sso_attempts.append(f"round{round_i}:err:{cs_err}")
                        sso = None
                    if sso:
                        sso_attempts.append(f"CreateSession:round{round_i}")
                        break
                    sso_attempts.append(f"round{round_i}:empty")
                except _RegCancelled:
                    raise
                except Exception as cs_loop_err:  # noqa: BLE001
                    print(
                        f"[grok-build-auth] CreateSession loop "
                        f"{round_i} error: {cs_loop_err}"
                    )
                    sso_attempts.append(f"round{round_i}:loop_err:{cs_loop_err}")
                # Backoff so newly created accounts become visible / rate limits cool.
                # Multi-thread: keep waits shorter; still grow with round index.
                time.sleep(min(8.0, sso_prop_sec + round_i * 1.0))

            # Final jar scrape in case a late set-cookie landed.
            if not sso:
                try:
                    sso = client._read_sso_from_jar()  # noqa: SLF001
                    if sso:
                        sso_attempts.append("cookie_jar_final")
                except Exception:
                    pass
            print(
                f"[grok-build-auth] CreateSession fallback sso="
                f"{(sso[:60] if sso else None)} attempts={sso_attempts}"
            )

        print(f"[grok-build-auth] fetch_sso_token result: {sso[:60] if sso else None}")
        sess["sso"] = sso
        sess["sso_attempts"] = list(sso_attempts)
        session_cookies = extract_cookies_from_auth_client(client)
        print(
            f"[grok-build-auth] session cookies after signup: "
            f"{sorted((session_cookies or {}).keys())}"
        )
        if sso:
            session_cookies = dict(session_cookies or {})
            session_cookies["sso"] = sso
            session_cookies["sso-rw"] = sso

        if not sso:
            # Keep recoverable markers for later manual/auto re-login import.
            try:
                sess["sso_recoverable"] = True
                sess["sso_missing"] = True
            except Exception:
                pass
            raise RuntimeError(
                "SSO_COOKIE_MISSING after create_account. "
                f"adapter_build={ADAPTER_BUILD}; HTTP {http_status}; "
                f"create_ok={bool(getattr(res, 'ok', False))}; "
                f"signup_error={signup_err!r}; set_cookies={len(sc)}; "
                f"cookie_keys={sorted((session_cookies or {}).keys())}; "
                f"attempts={sso_attempts}; "
                f"body_preview={rsc_preview!r}. "
                "Account may have been created, but neither RSC set-cookie chain "
                "nor CreateSession password fallback produced an sso cookie. "
                "Common causes: turnstile_failed, rate_limited, or account not yet "
                "visible to CreateSession."
            )

        # Required path: SSO/session JWT -> sso_to_auth_json device flow -> auth.json
        update(
            "importing",
            f"SSO obtained; converting via sso_to_auth_json [{ADAPTER_BUILD}]",
        )
        import scripts.sso_to_auth_json as sso_import

        token = sso_import.sso_to_token(sso)
        if not token or not token.get("access_token"):
            _note_reg_pressure("device-flow conversion failed", pause_sec=10)
            raise RuntimeError(
                "SSO obtained but sso_to_auth_json conversion failed "
                "(device verify/approve/token poll; often xAI device-flow "
                "rate_limited/slow_down under concurrent registration). "
                f"adapter_build={ADAPTER_BUILD}; sso_prefix={sso[:24]!r}"
            )
        _key, entry = sso_import.token_to_auth_entry(token, email=email)
        # Keep the raw SSO cookie with the account so export/re-import works
        # after process restart (registration sessions are ephemeral).
        sso_cookie = str(sso or sess.get("sso") or "").strip()
        reg_password = str(password or sess.get("password") or "").strip()
        sso_backup_path = ""
        if sso_cookie:
            try:
                sso_backup_path = _persist_registration_sso(
                    sid=sid,
                    email=str(entry.get("email") or email or ""),
                    password=reg_password,
                    sso=sso_cookie,
                    batch_id=str(sess.get("batch_id") or "") or None,
                )
            except Exception as e:  # noqa: BLE001
                print(f"[grok-build-auth] WARN: persist SSO backup failed: {e}")
        import_payload: dict[str, Any] = {
            "key": entry["key"],
            "auth_mode": entry.get("auth_mode", "oidc"),
            "email": entry.get("email") or email,
            "refresh_token": entry.get("refresh_token", ""),
            "expires_at": entry.get("expires_at"),
            "oidc_issuer": entry.get("oidc_issuer", "https://auth.x.ai"),
            "oidc_client_id": entry.get("oidc_client_id", ""),
            "source": "register-email",
            "registration_session_id": sid,
        }
        if sess.get("batch_id"):
            import_payload["registration_batch_id"] = sess.get("batch_id")
        if sso_cookie:
            # Always write top-level + nested shapes so PG payload, has_sso filters,
            # and admin "导出全部 SSO" see newly registered accounts without reconcile.
            import_payload["sso"] = sso_cookie
            import_payload["sso_cookie"] = sso_cookie
            import_payload["sso_token"] = sso_cookie
            sc = dict(session_cookies or {})
            sc["sso"] = sso_cookie
            sc["sso-rw"] = sso_cookie
            import_payload["session_cookies"] = sc
            # Also keep a cookie-header form for GetSSOValue regex paths.
            import_payload.setdefault("cookie", f"sso={sso_cookie}")
        if reg_password:
            import_payload["password"] = reg_password
            import_payload["register_password"] = reg_password
        if sso_backup_path:
            import_payload["sso_backup_path"] = sso_backup_path
        # Force-normalize durable SSO aliases before write (defensive).
        try:
            from grok2api.pool.accounts import merge_durable_account_fields as _mdf
            merge_durable_account_fields = _mdf
            merge_durable_account_fields(import_payload, None)
        except Exception:
            pass
        import_result = accounts.import_auth_payload(import_payload, merge=True)
        if not import_result.get("ok"):
            raise RuntimeError(
                f"SSO account import failed: {import_result.get('error')}; "
                f"adapter_build={ADAPTER_BUILD}"
            )
        # Registration import is durable PostgreSQL (accounts + account_pool).
        # auth.json is not written at runtime in hybrid mode (export-only).
        storage = import_result.get("storage")
        if not storage:
            try:
                storage = accounts._accounts_store_source()
            except Exception:
                storage = "unknown"
        import_result["storage"] = storage
        if storage != "postgres":
            print(
                f"[grok-build-auth] WARN: import storage={storage} "
                f"(expected postgres). Check DATABASE_URL / STORE_BACKEND."
            )
        # Verify SSO cookie actually landed in durable store (re-read by id).
        try:
            imported_probe_ids = [
                str(x.get("id"))
                for x in (import_result.get("imported") or [])
                if isinstance(x, dict) and x.get("id")
            ]
            if sso_cookie and imported_probe_ids:
                from grok2api.pool.accounts import get_sso_value as _gsv
                try:
                    from grok2api.pool.auth_store import read_auth_entry as _rae
                except Exception:
                    _rae = None  # type: ignore[assignment]
                for aid in imported_probe_ids[:5]:
                    hit = _rae(aid) if _rae is not None else None
                    payload = hit[1] if isinstance(hit, tuple) and len(hit) == 2 else None
                    if not isinstance(payload, dict):
                        continue
                    # Require top-level sso string (not only nested) for export-all.
                    top = str(payload.get("sso") or payload.get("sso_cookie") or "").strip()
                    if top and _gsv(payload):
                        continue
                    print(
                        f"[grok-build-auth] WARN: account {aid} imported without top-level SSO "
                        f"in payload; rewriting sso field [{ADAPTER_BUILD}]"
                    )
                    fix = dict(payload)
                    fix["sso"] = sso_cookie
                    fix["sso_cookie"] = sso_cookie
                    fix["sso_token"] = sso_cookie
                    sc = dict(fix.get("session_cookies") or {}) if isinstance(fix.get("session_cookies"), dict) else {}
                    sc["sso"] = sso_cookie
                    sc["sso-rw"] = sso_cookie
                    fix["session_cookies"] = sc
                    if reg_password:
                        fix.setdefault("password", reg_password)
                        fix.setdefault("register_password", reg_password)
                    accounts.import_auth_payload(fix, merge=True)
        except Exception as e:  # noqa: BLE001
            print(f"[grok-build-auth] WARN: post-import SSO verify failed: {e}")
        imported_rows = [
            x for x in (import_result.get("imported") or []) if isinstance(x, dict)
        ]
        imported_ids = [str(x.get("id")) for x in imported_rows if x.get("id")]
        imported_accounts = [
            {"id": x.get("id"), "email": x.get("email") or email}
            for x in imported_rows
            if x.get("id") or x.get("email")
        ]
        sess["auth_json"] = import_result
        sess["imported_account_ids"] = imported_ids
        sess["imported_accounts"] = imported_accounts
        sess["oauth"] = {
            "path": "sso_to_auth_json",
            "access_token": (token.get("access_token") or "")[:20] + "...",
            "refresh_token": bool(token.get("refresh_token")),
            "email": email,
        }
        # Optional: auto-push newly registered accounts into sub2api.
        # Controlled by settings → sub2api → auto_push_on_register.
        # Failures are recorded on the session but never fail registration.
        sub2api_push: dict[str, Any] | None = None
        if imported_ids:
            try:
                update(
                    "pushing_sub2api",
                    f"imported {len(imported_ids)} account(s); "
                    f"checking auto-push to sub2api [{ADAPTER_BUILD}]",
                    imported_account_ids=imported_ids,
                    imported_accounts=imported_accounts,
                )
                from grok2api.upstream.sub2api_client import maybe_auto_push_registered_accounts

                sub2api_push = maybe_auto_push_registered_accounts(
                    imported_ids,
                    source="register-email",
                )
                sess["sub2api_push"] = sub2api_push
                if sub2api_push and not sub2api_push.get("skipped"):
                    ok_n = int(sub2api_push.get("success") or 0)
                    fail_n = int(sub2api_push.get("failed") or 0)
                    update(
                        "pushing_sub2api",
                        f"sub2api auto-push done: ok={ok_n} fail={fail_n} "
                        f"[{ADAPTER_BUILD}]",
                        imported_account_ids=imported_ids,
                        imported_accounts=imported_accounts,
                        sub2api_push=sub2api_push,
                    )
            except Exception as e:  # noqa: BLE001
                sub2api_push = {
                    "ok": False,
                    "skipped": False,
                    "error": str(e),
                    "total": len(imported_ids),
                }
                sess["sub2api_push"] = sub2api_push
                print(f"[grok-build-auth] WARN: sub2api auto-push failed: {e}")
            # Optional: auto-push newly registered accounts into CLIProxyAPI.
            try:
                from grok2api.upstream.cliproxyapi_client import (
                    maybe_auto_push_registered_accounts as maybe_auto_push_cpa,
                )

                cpa_push = maybe_auto_push_cpa(
                    imported_ids,
                    source="register-email",
                )
                sess["cliproxyapi_push"] = cpa_push
                if cpa_push and not cpa_push.get("skipped"):
                    ok_n = int(cpa_push.get("success") or 0)
                    fail_n = int(cpa_push.get("failed") or 0)
                    update(
                        "pushing_cliproxyapi",
                        f"CLIProxyAPI auto-push done: ok={ok_n} fail={fail_n} "
                        f"[{ADAPTER_BUILD}]",
                        imported_account_ids=imported_ids,
                        imported_accounts=imported_accounts,
                        cliproxyapi_push=cpa_push,
                    )
            except Exception as e:  # noqa: BLE001
                sess["cliproxyapi_push"] = {
                    "ok": False,
                    "skipped": False,
                    "error": str(e),
                    "total": len(imported_ids),
                }
                print(f"[grok-build-auth] WARN: CLIProxyAPI auto-push failed: {e}")
        # Auto probe newly imported accounts so they are validated in the pool.
        # Release global admission BEFORE the settle sleep so bulk jobs don't
        # hold scarce inflight slots for REGISTER_PROBE_DELAY_SEC after success.
        if admission_flag is not None:
            _release_reg_admission_once(admission_flag)
        probe_summaries: list[dict[str, Any]] = []
        if imported_ids:
            delay = max(0.0, float(REGISTER_PROBE_DELAY_SEC or 0.0))
            if delay > 0:
                update(
                    "probing",
                    f"imported {len(imported_ids)} account(s); wait {int(delay)}s "
                    f"before probe [{ADAPTER_BUILD}]",
                    imported_account_ids=imported_ids,
                    imported_accounts=imported_accounts,
                    probe_delay_sec=delay,
                )
                # Heartbeat during settle wait so multi-worker reclaim does not
                # treat a healthy post-import session as an orphan.
                deadline = time.time() + delay
                while True:
                    remaining_wait = deadline - time.time()
                    if remaining_wait <= 0:
                        break
                    time.sleep(min(10.0, remaining_wait))
                    left = max(0.0, deadline - time.time())
                    if left <= 0:
                        break
                    update(
                        "probing",
                        f"imported {len(imported_ids)} account(s); wait "
                        f"{int(left)}s before probe [{ADAPTER_BUILD}]",
                        imported_account_ids=imported_ids,
                        imported_accounts=imported_accounts,
                        probe_delay_sec=delay,
                    )
            update(
                "probing",
                f"imported {len(imported_ids)} account(s); probing pool health "
                f"(delay={int(delay)}s) [{ADAPTER_BUILD}]",
                imported_account_ids=imported_ids,
                imported_accounts=imported_accounts,
                probe_delay_sec=delay,
            )
            try:
                import grok2api.pool.model_health as model_health

                for aid in imported_ids:
                    try:
                        # New registrations must enter the live rotation pool.
                        # Never auto-disable / free-usage-cool on the post-import
                        # probe — free accounts often report free-usage-exhausted
                        # on the first ping and were wrongly kicked out of 轮询.
                        pr = model_health.probe_single_account(
                            aid, None, auto_disable=False, source="register"
                        )
                        detail = pr.get("result") if isinstance(pr, dict) else None
                        if not isinstance(detail, dict):
                            detail = pr if isinstance(pr, dict) else {}
                        err_text = (
                            detail.get("error")
                            or detail.get("message")
                            or (pr.get("error") if isinstance(pr, dict) else None)
                            or ""
                        )
                        latency = (
                            detail.get("latency_ms")
                            or detail.get("elapsed_ms")
                            or detail.get("duration_ms")
                        )
                        probe_summaries.append(
                            {
                                "account_id": aid,
                                "ok": bool(pr.get("ok") if isinstance(pr, dict) else False),
                                "model": detail.get("model")
                                or (pr.get("model") if isinstance(pr, dict) else None),
                                "error": (str(err_text)[:180] if err_text else None),
                                "latency_ms": latency,
                            }
                        )
                    except Exception as pe:  # noqa: BLE001
                        probe_summaries.append(
                            {
                                "account_id": aid,
                                "ok": False,
                                "error": str(pe)[:180],
                            }
                        )
            except Exception as pe:  # noqa: BLE001
                probe_summaries.append(
                    {
                        "account_id": None,
                        "ok": False,
                        "error": f"probe module error: {pe}"[:180],
                    }
                )
        # Ensure newly registered accounts stay enabled + not cooling even if a
        # concurrent background health wave cooled them during import.
        try:
            import grok2api.pool.account_pool as account_pool
            from grok2api.admin.settings_store import get_account_pool_meta, patch_account_pool_meta
            for aid in imported_ids:
                try:
                    account_pool.clear_account_cooldown(aid)
                except Exception:
                    pass
                try:
                    meta = get_account_pool_meta(aid) or {}
                    if isinstance(meta, dict) and meta.get("enabled") is False and not meta.get("disabled_for_quota"):
                        patch_account_pool_meta(
                            aid,
                            {
                                "enabled": True,
                                "pool_status": "normal",
                                "disabled_reason": None,
                                "disabled_source": None,
                                "cooldown_until": None,
                                "cooldown_count": 0,
                            },
                        )
                except Exception:
                    pass
        except Exception as rexc:
            print(f"[grok-build-auth] WARN: post-register re-enable failed: {rexc}")
        sess["probe"] = {
            "count": len(probe_summaries),
            "ok": sum(1 for p in probe_summaries if p.get("ok")),
            "fail": sum(1 for p in probe_summaries if not p.get("ok")),
            "results": probe_summaries,
        }
        ok_n = int(sess["probe"]["ok"])
        fail_n = int(sess["probe"]["fail"])
        update(
            "imported",
            f"imported via sso_to_auth_json "
            f"({len(imported_ids) or len(imported_rows)} account(s)); "
            f"probe ok={ok_n} fail={fail_n} "
            f"[{ADAPTER_BUILD}]",
            imported_account_ids=imported_ids,
            imported_accounts=imported_accounts,
            probe=sess.get("probe"),
        )
        return
    except _RegCancelled as exc:
        with _lock:
            cur = _sessions.get(sid) or sess
            cur["status"] = "cancelled"
            cur["message"] = str(exc) or "cancelled by user"
            cur["error"] = "cancelled"
            cur["cancel_requested"] = True
            cur["updated_at"] = _now()
            _sessions[sid] = cur
            _mirror_reg_sess(sid, cur)
        return
    except Exception as exc:  # noqa: BLE001
        try:
            update("error", f"failed: {exc}", error=str(exc))
        except _RegCancelled:
            with _lock:
                cur = _sessions.get(sid) or sess
                cur["status"] = "cancelled"
                cur["message"] = "cancelled by user"
                cur["error"] = "cancelled"
                cur["cancel_requested"] = True
                cur["updated_at"] = _now()
                _sessions[sid] = cur
                _mirror_reg_sess(sid, cur)
    finally:
        if client is not None:
            try:
                client.close()
            except Exception:
                pass
        # Always drop process-local handles so mailbox / captcha clients do not
        # pin sockets after cancel/error/import. Mirror once more so other workers
        # see the stripped session promptly.
        with _lock:
            final_sess = dict(_sessions.get(sid) or sess or {})
            if sid in _sessions:
                for k in ("_receiver", "_client", "_oauth_client"):
                    obj = _sessions[sid].pop(k, None)
                    if obj is None:
                        continue
                    for meth in ("close", "stop", "shutdown", "cancel"):
                        fn = getattr(obj, meth, None)
                        if callable(fn):
                            try:
                                fn()
                            except Exception:
                                pass
                            break
                try:
                    _mirror_reg_sess(sid, _sessions[sid], force=True)
                except Exception:
                    pass
                final_sess = dict(_sessions[sid])
        # Single sessions write their own terminal task log. Batch sessions are
        # summarized once by the batch finalizer (avoids N noise rows).
        if final_sess and not final_sess.get("batch_id"):
            payload = _session_task_log_payload(final_sess)
            if payload.get("finished"):
                _record_register_task(
                    task_id=payload["task_id"] or sid,
                    summary=payload["summary"],
                    status=payload["status"],
                    ok=payload["ok"],
                    progress_done=payload["progress_done"],
                    progress_total=payload["progress_total"],
                    finished=True,
                    detail={**payload["detail"], "phase": "finished"},
                )



def _nonterminal_session_statuses() -> frozenset[str]:
    return frozenset(
        {
            "pending",
            "queued",
            "starting",
            "started",
            "waiting_admission",
            "running",
            "probing",
            "solving_turnstile",
            "waiting_email",
            "fetching_sso",
            "converting",
            "importing",
            "stopping",
        }
    )


def reclaim_orphaned_registration_sessions(
    *,
    stale_sec: float | None = None,
    batch_id: str | None = None,
) -> dict[str, Any]:
    """Mark abandoned non-terminal sessions as error so a batch can resume.

    After process restart / image upgrade, in-flight workers die but Redis still
    shows ``solving_turnstile`` etc. Those sessions never finish, and the
    batch runner is gone — registration appears "hung mid-way".
    """
    try:
        ttl = float(
            stale_sec
            if stale_sec is not None
            else (os.environ.get("GROK2API_REG_STALE_SEC", "120") or 120)
        )
    except (TypeError, ValueError):
        ttl = 120.0
    ttl = max(30.0, min(ttl, 3600.0))
    now = _now()
    nonterm = _nonterminal_session_statuses()
    want_batch = (batch_id or "").strip() or None

    reclaimed: list[dict[str, Any]] = []
    # Prefer durable Redis list when available.
    sessions: list[dict[str, Any]] = []
    if _reg_redis():
        try:
            from grok2api.store import sessions_redis

            listed = sessions_redis.reg_sess_list() or []
            if isinstance(listed, list):
                sessions = [s for s in listed if isinstance(s, dict)]
        except Exception:
            sessions = []
    if not sessions:
        with _lock:
            sessions = [dict(v) for v in _sessions.values() if isinstance(v, dict)]

    # Cache batch liveness so multi-worker reclaim does not thrash Redis.
    batch_live: dict[str, dict[str, Any]] = {}

    def _batch_liveness(bid0: str) -> dict[str, Any]:
        if bid0 in batch_live:
            return batch_live[bid0]
        info = {
            "has_runner": False,
            "batch_age": 1e18,
            "batch_status": "",
            "runner_alive_flag": False,
        }
        if not bid0:
            batch_live[bid0] = info
            return info
        with _lock:
            info["has_runner"] = bool(_active_batch_runners.get(bid0))
        b = _load_reg_batch(bid0) or {}
        if isinstance(b, dict) and b:
            info["batch_status"] = str(b.get("status") or "").strip().lower()
            info["runner_alive_flag"] = bool(b.get("runner_alive"))
            info["batch_age"] = now - float(
                b.get("updated_at") or b.get("created_at") or 0
            )
        if not info["has_runner"] and _reg_redis():
            try:
                from grok2api.store.redis_client import get_str as redis_get

                if redis_get(_batch_runner_lock_key(bid0)):
                    info["has_runner"] = True
            except Exception:
                pass
        # IMPORTANT: do NOT treat runner_alive/updated_at alone as live.
        # Multi-worker reclaim/TTL paths used to refresh updated_at and leave
        # runner_alive=true after the real runner died, which permanently
        # blocked auto-resume and stranded sessions in solving_turnstile.
        batch_live[bid0] = info
        return info

    for sess in sessions:
        sid = str(sess.get("id") or "").strip()
        if not sid:
            continue
        bid = str(sess.get("batch_id") or "").strip()
        if want_batch and bid != want_batch:
            continue
        st = str(sess.get("status") or "").strip().lower()
        if st in _TERMINAL_STATUSES or st not in nonterm:
            continue
        age = now - float(sess.get("updated_at") or sess.get("created_at") or 0)
        live = _batch_liveness(bid) if bid else {
            "has_runner": False,
            "batch_age": 1e18,
            "batch_status": "",
            "runner_alive_flag": False,
        }
        has_runner = bool(live.get("has_runner"))
        batch_age = float(live.get("batch_age") or 1e18)
        # Live batch: only reclaim truly stalled sessions.
        # Local Turnstile is process-global serialized — under bulk load
        # healthy sessions wait for the lock, but dead runners must free slots
        # much sooner than the old 15min captcha grace (felt permanently stuck).
        long_wait_statuses = {
            "waiting_admission",
            "solving_turnstile",
            "waiting_email",
            "converting",
            "probing",
            "creating_account",
            "registering",
            "waiting_solver",
        }
        try:
            captcha_grace = float(
                os.environ.get("GROK2API_REG_CAPTCHA_STALE_SEC", "420") or 420
            )
        except (TypeError, ValueError):
            captcha_grace = 420.0
        # Floor 180s (one full local turnstile timeout + margin); cap 30min.
        captcha_grace = max(180.0, min(1800.0, captcha_grace))
        if has_runner:
            if st in long_wait_statuses:
                # Still alive runner: give captcha/email the full grace.
                min_age = max(ttl, captcha_grace)
            else:
                min_age = max(ttl, 120.0)
        elif batch_age < max(ttl, 60.0) and str(live.get("batch_status") or "") in {
            "running",
            "starting",
            "stopping",
            "partial",
        }:
            # Batch metadata is still being touched (another worker owns runner).
            if st in long_wait_statuses:
                min_age = max(ttl, captcha_grace)
            else:
                min_age = max(ttl, 120.0)
        else:
            # Dead batch / no runner: free slots ASAP so watchdog can resume.
            # Previously captcha stages waited up to captcha_grace/2 (~7.5min)
            # with runner_alive=False — the main "偶发性卡住" symptom.
            if st in long_wait_statuses:
                min_age = min(max(75.0, ttl * 0.75), captcha_grace * 0.35, 180.0)
            else:
                min_age = min(max(30.0, ttl / 3.0), 90.0)
        if age < min_age:
            continue
        msg = (
            f"reclaimed orphan session after {age:.0f}s "
            f"(status was {st}; runner_alive={has_runner})"
        )
        with _lock:
            cur = _sessions.get(sid) or dict(sess)
            prev = str(cur.get("status") or "").strip().lower()
            already_terminal = prev in _TERMINAL_STATUSES
            # Another worker may have finished this session since we listed it.
            if already_terminal or prev in _TERMINAL_STATUSES:
                continue
            # Fresh update since list snapshot → still alive; skip.
            cur_age = now - float(cur.get("updated_at") or cur.get("created_at") or 0)
            if cur_age < min_age:
                continue
            cur["status"] = "error"
            cur["error"] = msg
            cur["message"] = msg
            # Ask any still-alive worker to exit; do NOT bump batch finished here.
            # Bumping finished permanently consumes bulk slots that should be
            # retried after a dead runner, and can double-count when a live
            # worker later returns and _note_result() increments again.
            cur["cancel_requested"] = True if has_runner else False
            cur["updated_at"] = now
            _sessions[sid] = cur
            _mirror_reg_sess(sid, dict(cur))
            if bid:
                b = _batches.get(bid) or _load_reg_batch(bid) or {"id": bid}
                b = dict(b)
                # Do NOT bump batch updated_at here — that falsely signals a live
                # runner to multi-worker reclaim/resume and blocks auto-heal.
                if not b.get("message"):
                    b["message"] = "reclaimed orphan sessions"
                # No local/remote lock ⇒ runner is dead; clear sticky flag.
                if not has_runner:
                    b["runner_alive"] = False
                    b["inflight"] = 0
                _batches[bid] = b
                _mirror_reg_batch(bid, dict(b))
        reclaimed.append(
            {
                "id": sid,
                "batch_id": bid,
                "prev_status": st,
                "age_sec": int(age),
                "email": sess.get("email"),
                "already_terminal": already_terminal,
                "had_runner": has_runner,
            }
        )
    return {
        "ok": True,
        "reclaimed": len(reclaimed),
        "stale_sec": ttl,
        "items": reclaimed[:100],
    }


def resume_registration_batch(
    batch_id: str,
    *,
    force: bool = False,
    reclaim_stale_sec: float | None = None,
) -> dict[str, Any]:
    """Reclaim orphan sessions and re-spawn the batch runner for remaining count.

    Used after process restart when Redis still shows status=running but no
    worker is actually spawning jobs.
    """
    bid = str(batch_id or "").strip()
    if not bid:
        return {"ok": False, "error": "missing batch id"}
    batch = _load_reg_batch(bid)
    if not batch:
        return {"ok": False, "error": "registration batch not found"}

    st = str(batch.get("status") or "").strip().lower()
    if st in {"done", "cancelled", "stopped"} and not force:
        return {
            "ok": False,
            "error": f"batch already terminal ({st}); pass force=true to resume",
            "batch_id": bid,
            "status": st,
        }
    if batch.get("cancel_requested") and not force:
        return {
            "ok": False,
            "error": "batch cancel_requested; clear stop or pass force=true",
            "batch_id": bid,
        }

    # Drop dead runner lock if our process does not own it (TTL may still hold).
    if force and _reg_redis():
        try:
            from grok2api.store.redis_client import get_str as redis_get

            lock_k = _batch_runner_lock_key(bid)
            token = redis_get(lock_k)
            with _lock:
                local_alive = bool(_active_batch_runners.get(bid))
            if token and not local_alive:
                try:
                    from grok2api.store.redis_client import compare_and_delete

                    compare_and_delete(lock_k, str(token))
                except Exception:
                    try:
                        # last resort: overwrite with short TTL empty marker then let expire
                        from grok2api.store.redis_client import set_ex

                        set_ex(lock_k, "reclaimed", 1)
                    except Exception:
                        pass
        except Exception:
            pass

    reclaimed = reclaim_orphaned_registration_sessions(
        stale_sec=reclaim_stale_sec, batch_id=bid
    )

    count = int(batch.get("count") or 0)
    finished = int(batch.get("finished") or 0)
    # Prefer durable session list to compute remaining if counters lag.
    sids = list(batch.get("session_ids") or [])
    terminal = 0
    if sids and _reg_redis():
        try:
            from grok2api.store import sessions_redis

            for sid in sids:
                sess = sessions_redis.reg_sess_get(str(sid)) or {}
                st_s = str(sess.get("status") or "").lower()
                if st_s in _TERMINAL_STATUSES:
                    terminal += 1
        except Exception:
            terminal = finished
    else:
        terminal = finished
    remaining = max(0, count - max(finished, terminal))
    if remaining <= 0:
        with _lock:
            b = _batches.get(bid) or dict(batch)
            b["status"] = "done" if int(b.get("fail_count") or 0) == 0 else "partial"
            b["runner_alive"] = False
            b["updated_at"] = _now()
            b["message"] = (
                f"resume: nothing remaining "
                f"(count={count} finished={finished} terminal={terminal})"
            )
            _batches[bid] = b
            _mirror_reg_batch(bid, dict(b))
        return {
            "ok": True,
            "batch_id": bid,
            "remaining": 0,
            "reclaimed": reclaimed.get("reclaimed") or 0,
            "message": "batch already complete",
            "status": (_load_reg_batch(bid) or {}).get("status"),
        }

    cfg = batch.get("reg_config") if isinstance(batch.get("reg_config"), dict) else {}
    provider = str(cfg.get("captcha_provider") or CAPTCHA_PROVIDER or "local").strip().lower()
    key = str(cfg.get("yescaptcha_key") or "").strip()
    proxy = str(cfg.get("proxy") or "").strip()
    workers = int(cfg.get("concurrency") or batch.get("concurrency") or DEFAULT_CONCURRENCY)
    # Local captcha is the bottleneck; when resuming after restart, prefer a
    # safer concurrency so multiple auto-resumed batches don't thrash Camoufox.
    if provider == "local":
        try:
            local_cap = int(
                os.environ.get("GROK2API_REG_LOCAL_CONCURRENCY")
                or LOCAL_CAPTCHA_MAX_CONCURRENCY
                or 3
            )
        except (TypeError, ValueError):
            local_cap = LOCAL_CAPTCHA_MAX_CONCURRENCY
        workers = max(1, min(workers, max(1, local_cap)))
    stagger = int(cfg.get("stagger_ms") or batch.get("stagger_ms") or 400)
    # Optional floor for local captcha (env). Default 0 so admin "错峰 ms" is effective.
    # Set GROK2API_REG_MIN_STAGGER_MS>0 only when you want a safety minimum under load.
    try:
        min_stagger = int(os.environ.get("GROK2API_REG_MIN_STAGGER_MS", "0") or 0)
    except (TypeError, ValueError):
        min_stagger = 0
    if provider == "local" and min_stagger > 0:
        stagger = max(stagger, min_stagger)
    mail_provider, mail_key, mail_base, mail_dom = _resolve_mail_credentials(
        mail_provider=str(cfg.get("mail_provider") or "moemail"),
        moemail_api_key=cfg.get("moemail_api_key"),
        moemail_base_url=cfg.get("moemail_base_url"),
        domain=cfg.get("domain"),
        reg_config=cfg,
    )
    proxy_strategy = str(cfg.get("proxy_strategy") or "round_robin").strip().lower()

    # Clear cancel so spawn is allowed.
    with _lock:
        b = _batches.get(bid) or dict(batch)
        b["cancel_requested"] = False
        if str(b.get("status") or "").lower() in {"stopping", "cancelled", "stopped", "error"}:
            b["status"] = "running"
        b["updated_at"] = _now()
        b["message"] = (
            f"resume requested remaining={remaining} "
            f"(reclaimed={reclaimed.get('reclaimed') or 0})"
        )
        _batches[bid] = b
        _mirror_reg_batch(bid, dict(b))

    _ensure_registration_watchdog()
    spawned = _spawn_batch_runner(
        bid,
        remaining=remaining,
        concurrency=workers,
        stagger_ms=stagger,
        captcha_provider=provider,
        yescaptcha_key=key,
        proxy=proxy,
        proxy_strategy=proxy_strategy,
        moemail_api_key=mail_key,
        moemail_base_url=mail_base,
        prefix=cfg.get("prefix"),
        domain=mail_dom,
        expiry_ms=cfg.get("expiry_ms"),
        mail_provider=mail_provider,
    )
    out = {
        "ok": bool(spawned.get("ok")),
        "batch_id": bid,
        "remaining": remaining,
        "reclaimed": reclaimed.get("reclaimed") or 0,
        "reclaim": reclaimed,
        "spawn": spawned,
        "message": spawned.get("message") or spawned.get("error"),
    }
    if not out["ok"]:
        out["error"] = spawned.get("error") or "spawn failed"
    return out


def reclaim_orphaned_registration_batches(
    *,
    stale_sec: float | None = None,
    auto_resume: bool = True,
    max_batches: int | None = None,
) -> dict[str, Any]:
    """On startup: reclaim orphan sessions and optionally resume open batches."""
    try:
        ttl = float(
            stale_sec
            if stale_sec is not None
            else (os.environ.get("GROK2API_REG_STALE_SEC", "120") or 120)
        )
    except (TypeError, ValueError):
        ttl = 120.0
    if max_batches is None:
        try:
            max_batches = int(os.environ.get("GROK2API_REG_AUTO_RESUME_MAX", "1") or 1)
        except (TypeError, ValueError):
            max_batches = 1
    max_batches = max(0, min(10, int(max_batches)))

    # First pass: mark dead in-flight sessions.
    sess_result = reclaim_orphaned_registration_sessions(stale_sec=ttl)

    batches: list[dict[str, Any]] = []
    if _reg_redis():
        try:
            from grok2api.store import sessions_redis

            listed = sessions_redis.reg_batch_list() or []
            if isinstance(listed, list):
                batches = [b for b in listed if isinstance(b, dict)]
        except Exception:
            batches = []
    if not batches:
        with _lock:
            batches = [dict(v) for v in _batches.values() if isinstance(v, dict)]

    resumed: list[dict[str, Any]] = []
    skipped: list[dict[str, Any]] = []
    now_ts = _now()
    # Newest first.
    batches = sorted(
        batches, key=lambda b: float(b.get("updated_at") or b.get("created_at") or 0), reverse=True
    )
    for b in batches:
        if len(resumed) >= max(0, int(max_batches)):
            break
        bid = str(b.get("id") or "").strip()
        if not bid:
            continue
        st = str(b.get("status") or "").strip().lower()
        if st not in {"running", "starting", "stopping", "partial"} and not (
            st == "error" and int(b.get("finished") or 0) < int(b.get("count") or 0)
        ):
            continue
        if b.get("cancel_requested") and st in {"stopping", "cancelled", "stopped"}:
            skipped.append({"batch_id": bid, "reason": "cancel_requested", "status": st})
            continue
        # Skip only if THIS process has a live runner. Redis locks from a dead
        # process (image restart) must not block auto-resume forever — force
        # clear them when no local runner owns the batch.
        has_local = False
        with _lock:
            has_local = bool(_active_batch_runners.get(bid))
        if has_local:
            skipped.append({"batch_id": bid, "reason": "local_runner_alive", "status": st})
            continue
        # Multi-worker: another process may own the runner. Never force-clear a
        # live lock / recently-updated running batch — that causes thrash and
        # orphan reclaim of healthy in-flight sessions.
        batch_age = now_ts - float(b.get("updated_at") or b.get("created_at") or 0)
        lock_token = None
        if _reg_redis():
            try:
                from grok2api.store.redis_client import get_str as redis_get

                lock_token = redis_get(_batch_runner_lock_key(bid))
            except Exception:
                lock_token = None
        if lock_token:
            # Live lock means another process still owns the runner.
            skipped.append(
                {
                    "batch_id": bid,
                    "reason": "remote_runner_lock_live",
                    "status": st,
                    "batch_age": int(batch_age),
                }
            )
            continue
        # No lock + no local runner = dead runner. Clear sticky runner_alive
        # even if updated_at is recent (reclaim/TTL used to refresh it).
        if bool(b.get("runner_alive")) or int(b.get("inflight") or 0) > 0:
            with _lock:
                cur = _batches.get(bid) or dict(b)
                cur = dict(cur)
                cur["runner_alive"] = False
                cur["inflight"] = 0
                # Keep updated_at unchanged so we don't create a false heartbeat.
                cur["message"] = (
                    "cleared sticky runner_alive (no runner lock)"
                )
                _batches[bid] = cur
                _mirror_reg_batch(bid, dict(cur))
            b = cur
        count = int(b.get("count") or 0)
        finished = int(b.get("finished") or 0)
        if count > 0 and finished >= count:
            skipped.append({"batch_id": bid, "reason": "already_finished", "status": st})
            continue
        if not auto_resume:
            skipped.append({"batch_id": bid, "reason": "auto_resume_disabled", "status": st})
            continue
        r = resume_registration_batch(bid, force=True, reclaim_stale_sec=ttl)
        resumed.append(r)

    return {
        "ok": True,
        "sessions_reclaimed": sess_result.get("reclaimed") or 0,
        "session_reclaim": sess_result,
        "batches_resumed": sum(1 for r in resumed if r.get("ok")),
        "resumed": resumed,
        "skipped": skipped[:20],
    }


def _refresh_active_registration_ttls() -> int:
    """Refresh Redis TTLs for active batches without faking liveness timestamps.

    Important: multi-worker reclaim/resume uses `updated_at` + `runner_alive` to
    decide whether a batch is still owned. Rewriting those fields here would
    keep a dead batch forever "alive" and prevent auto-resume.
    """
    if not _reg_redis():
        return 0
    refreshed = 0
    try:
        from grok2api.store import sessions_redis
    except Exception:
        return 0
    try:
        batches = sessions_redis.reg_batch_list() or []
    except Exception:
        batches = []
    if not isinstance(batches, list):
        return 0
    for b in batches:
        if not isinstance(b, dict):
            continue
        bid = str(b.get("id") or "").strip()
        if not bid:
            continue
        st = str(b.get("status") or "").strip().lower()
        count = int(b.get("count") or 0)
        finished = int(b.get("finished") or 0)
        active = st in {"running", "starting", "stopping", "partial"} or (
            count > 0 and finished < count and st not in {"cancelled", "stopped", "done"}
        )
        if not active:
            continue
        try:
            if sessions_redis.reg_batch_touch(bid):
                refreshed += 1
        except Exception:
            pass
        # Refresh non-terminal session TTLs only (no payload rewrite). Cap per tick.
        try:
            listed = sessions_redis.reg_sess_list() or []
        except Exception:
            listed = []
        touched = 0
        for sess in listed:
            if not isinstance(sess, dict):
                continue
            if str(sess.get("batch_id") or "").strip() != bid:
                continue
            st_s = str(sess.get("status") or "").strip().lower()
            if st_s in _TERMINAL_STATUSES:
                continue
            sid_s = str(sess.get("id") or "").strip()
            if not sid_s:
                continue
            try:
                sessions_redis.reg_sess_touch(sid_s)
                touched += 1
            except Exception:
                pass
            if touched >= 200:
                break
        if touched == 0:
            for sid in list(b.get("session_ids") or [])[-200:]:
                sid_s = str(sid or "").strip()
                if not sid_s:
                    continue
                try:
                    sessions_redis.reg_sess_touch(sid_s)
                except Exception:
                    pass
    return refreshed


def _ensure_registration_watchdog() -> None:
    """Start a process-local daemon that self-heals unfinished bulk batches."""
    global _reg_watchdog_started
    with _reg_watchdog_lock:
        if _reg_watchdog_started:
            return
        _reg_watchdog_started = True

    def _loop() -> None:
        # First pass soon after startup so orphan mid-captcha sessions recover
        # quickly after image restarts (was up to 20s of "stuck" UI).
        time.sleep(min(8.0, max(3.0, REG_WATCHDOG_SEC / 3.0)))
        while True:
            try:
                refreshed = _refresh_active_registration_ttls()
                # Only resume batches that still have remaining work and no local runner.
                try:
                    max_batches = int(
                        os.environ.get("GROK2API_REG_WATCHDOG_RESUME_MAX", "3") or 3
                    )
                except (TypeError, ValueError):
                    max_batches = 3
                result = reclaim_orphaned_registration_batches(
                    auto_resume=True,
                    max_batches=max(0, min(10, max_batches)),
                    stale_sec=REG_WATCHDOG_STALE_SEC,
                )
                resumed_n = int(result.get("batches_resumed") or 0)
                reclaimed_n = int(result.get("sessions_reclaimed") or 0)
                if refreshed or resumed_n or reclaimed_n:
                    print(
                        f"[registration] watchdog tick: "
                        f"ttl_refresh={refreshed} reclaimed={reclaimed_n} "
                        f"resumed={resumed_n}"
                    )
            except Exception as e:  # noqa: BLE001
                print(f"[registration] watchdog error: {e}")
            time.sleep(REG_WATCHDOG_SEC)

    threading.Thread(
        target=_loop,
        daemon=True,
        name="gba-reg-watchdog",
    ).start()


def stop_registration_session(session_id: str) -> dict[str, Any]:
    """Request cooperative cancel for one registration session."""
    sid = str(session_id or "").strip()
    if not sid:
        return {"ok": False, "error": "missing session id"}
    sess = _load_reg_sess(sid)
    if not sess:
        return {"ok": False, "error": "registration session not found"}
    st = str(sess.get("status") or "").lower()
    if st in _TERMINAL_STATUSES:
        return {
            "ok": True,
            "id": sid,
            "status": st,
            "already_terminal": True,
            "message": sess.get("message") or st,
        }
    with _lock:
        cur = _sessions.get(sid) or dict(sess)
        cur["cancel_requested"] = True
        cur["status"] = "stopping"
        cur["message"] = "stop requested; waiting for worker to exit"
        cur["updated_at"] = _now()
        # Best-effort immediate release of process-local handles so Camoufox /
        # mailbox sockets do not linger until the worker next hits update().
        for k in ("_receiver", "_client", "_oauth_client"):
            obj = cur.pop(k, None)
            if obj is None:
                continue
            for meth in ("close", "stop", "shutdown", "cancel"):
                fn = getattr(obj, meth, None)
                if callable(fn):
                    try:
                        fn()
                    except Exception:
                        pass
                    break
        try:
            _append_session_log(cur, "stopping", "stop requested")
        except Exception:
            pass
        _sessions[sid] = cur
        _mirror_reg_sess(sid, cur, force=True)
        out = _compact_session(cur)
    return {"ok": True, "id": sid, **out}


def stop_registration_batch(batch_id: str) -> dict[str, Any]:
    """Request cooperative cancel for every non-terminal session in a batch."""
    bid = str(batch_id or "").strip()
    if not bid:
        return {"ok": False, "error": "missing batch id"}
    batch = _load_reg_batch(bid)
    if not batch:
        return {"ok": False, "error": "registration batch not found"}

    # Mark batch cancelled FIRST so spawner/workers observe stop even before
    # individual session mirrors catch up (multi-worker / Redis path).
    with _lock:
        b = _batches.get(bid) or dict(batch)
        b["cancel_requested"] = True
        if str(b.get("status") or "").lower() not in (
            "done",
            "partial",
            "error",
            "cancelled",
            "stopped",
        ):
            b["status"] = "stopping"
        b["message"] = "stop requested; signalling sessions"
        b["updated_at"] = _now()
        _batches[bid] = b
        _mirror_reg_batch(bid, dict(b))
        sids = list(b.get("session_ids") or [])

    stopped: list[str] = []
    already: list[str] = []
    missing: list[str] = []
    for sid in sids:
        r = stop_registration_session(str(sid))
        if not r.get("ok"):
            missing.append(str(sid))
            continue
        if r.get("already_terminal"):
            already.append(str(sid))
        else:
            stopped.append(str(sid))

    with _lock:
        b = _batches.get(bid) or dict(batch)
        b["cancel_requested"] = True
        if str(b.get("status") or "").lower() not in (
            "done",
            "partial",
            "error",
            "cancelled",
            "stopped",
        ):
            b["status"] = "stopping"
        b["message"] = (
            f"stop requested: stopping={len(stopped)} "
            f"already_done={len(already)} missing={len(missing)}"
        )
        b["updated_at"] = _now()
        _batches[bid] = b
        _mirror_reg_batch(bid, dict(b))
        out = dict(b)
    return {
        "ok": True,
        "batch_id": bid,
        "stopped": stopped,
        "already_terminal": already,
        "missing": missing,
        "message": out.get("message") or "stop requested",
        "batch": out,
    }


def stop_all_active_registrations() -> dict[str, Any]:
    """Stop every non-terminal registration session currently visible."""
    listed = list_registration_sessions()
    sessions = list(listed.get("sessions") or [])
    stopped = []
    already = []
    for s in sessions:
        sid = str(s.get("id") or "")
        if not sid:
            continue
        r = stop_registration_session(sid)
        if r.get("already_terminal"):
            already.append(sid)
        elif r.get("ok"):
            stopped.append(sid)
    # Also mark running batches as stopping.
    for b in list(listed.get("batches") or []):
        bid = str(b.get("id") or b.get("batch_id") or "")
        if not bid:
            continue
        st = str(b.get("status") or b.get("batch_status") or "").lower()
        if st in ("done", "partial", "error", "cancelled", "stopped"):
            continue
        try:
            stop_registration_batch(bid)
        except Exception:
            pass
    return {
        "ok": True,
        "stopped": stopped,
        "already_terminal": already,
        "stopped_count": len(stopped),
        "already_count": len(already),
    }


def list_registration_sessions() -> dict[str, Any]:
    _clean_old_sessions()
    # Merge Redis-visible sessions/batches so other workers can observe progress.
    if _reg_redis():
        try:
            from grok2api.store import sessions_redis

            for remote in sessions_redis.reg_sess_list():
                sid = str(remote.get("id") or "")
                if not sid:
                    continue
                with _lock:
                    if sid not in _sessions:
                        _sessions[sid] = remote
                    else:
                        # Prefer newer updated_at, but keep local process-only fields.
                        local = _sessions[sid]
                        if float(remote.get("updated_at") or 0) >= float(
                            local.get("updated_at") or 0
                        ):
                            merged = {**local, **remote}
                            for k, v in local.items():
                                if isinstance(k, str) and k.startswith("_") and k not in remote:
                                    merged[k] = v
                            _sessions[sid] = merged
            for remote_b in sessions_redis.reg_batch_list():
                bid = str(remote_b.get("id") or remote_b.get("batch_id") or "")
                if not bid:
                    continue
                with _lock:
                    if bid not in _batches:
                        _batches[bid] = remote_b
                    else:
                        local_b = _batches[bid]
                        if float(remote_b.get("updated_at") or 0) >= float(
                            local_b.get("updated_at") or 0
                        ):
                            # Union session_ids so late workers don't drop early ones.
                            ids = list(local_b.get("session_ids") or [])
                            for x in remote_b.get("session_ids") or []:
                                if x not in ids:
                                    ids.append(x)
                            merged_b = {**local_b, **remote_b, "session_ids": ids}
                            _batches[bid] = merged_b
        except Exception:
            pass
    with _lock:
        sessions = [_compact_session(s) for s in _sessions.values()]
        sessions.sort(
            key=lambda s: float(s.get("updated_at") or s.get("created_at") or 0),
            reverse=True,
        )
        batches = []
        for b in _batches.values():
            sids = list(b.get("session_ids") or [])
            stats = _batch_stats(sids, batch=b)
            # If all observed sessions cancelled, surface batch as cancelled.
            if sids and stats.get("running") == 0 and stats.get("cancelled", 0) > 0:
                if (
                    stats.get("imported", 0) == 0
                    and stats.get("error", 0) == 0
                    and stats.get("missing", 0) == 0
                ):
                    stats["batch_status"] = "cancelled"
            item = {**b, **stats}
            # Align top-level status with computed batch_status for UI restore filters.
            bst = str(stats.get("batch_status") or "").lower()
            cur = str(b.get("status") or "").lower()
            if bst and (
                cur in ("", "running", "starting")
                or (bst in ("done", "partial", "error", "cancelled", "stopped") and stats.get("running", 0) == 0)
            ):
                if cur != "stopping" or stats.get("running", 0) == 0:
                    item["status"] = bst if bst != "running" or cur != "stopping" else cur
            batches.append(item)
        batches.sort(
            key=lambda b: float(b.get("updated_at") or b.get("created_at") or 0),
            reverse=True,
        )
    return {
        "sessions": sessions,
        "batches": batches,
        "active": sum(
            1
            for s in sessions
            if str(s.get("status") or "").lower() not in _TERMINAL_STATUSES
        ),
    }


def get_registration_session(
    sid: str, *, include_auth_json: bool = False
) -> dict[str, Any] | None:
    sess = _load_reg_sess(sid)
    if not sess:
        return None
    out = _compact_session(sess)
    if include_auth_json and isinstance(sess.get("auth_json"), (dict, list)):
        out["auth_json"] = sess.get("auth_json")
    return out


def _batch_stats(
    session_ids: list[str],
    *,
    batch: dict[str, Any] | None = None,
) -> dict[str, Any]:
    """Compute batch counters from live sessions.

    Missing sessions (TTL expired / not mirrored) are *not* treated as running —
    that previously made finished historical batches look active after Redis
    session keys aged out. When no live sessions remain, fall back to the
    persisted batch status/message counters.
    """
    imported = error = running = cancelled = missing = 0
    for sid in session_ids:
        sess = _load_reg_sess(sid)
        if not sess:
            missing += 1
            continue
        st = str(sess.get("status") or "").lower()
        if st in ("imported", "success", "completed"):
            imported += 1
        elif st in ("cancelled", "stopped"):
            cancelled += 1
        elif st in ("error", "failed", "expired", "protocol_error", "protocol_blocked"):
            error += 1
        else:
            running += 1

    total = len(session_ids)
    observed = imported + error + cancelled + running
    done = imported + error + cancelled
    target = 0
    if isinstance(batch, dict):
        try:
            target = int(batch.get("count") or 0)
        except Exception:
            target = 0
    if target <= 0:
        target = total

    status = "running"
    if observed == 0:
        # No live sessions left — trust last mirrored batch status if terminal.
        stored = ""
        if isinstance(batch, dict):
            stored = str(batch.get("batch_status") or batch.get("status") or "").lower()
        if stored in ("done", "partial", "error", "cancelled", "stopped"):
            status = stored
            # Prefer stored counters when present so UI keeps final totals.
            try:
                imported = int(batch.get("imported") or imported)
            except Exception:
                pass
            try:
                error = int(batch.get("error") or error)
            except Exception:
                pass
            try:
                cancelled = int(batch.get("cancelled") or cancelled)
            except Exception:
                pass
            try:
                done = int(batch.get("done") or (imported + error + cancelled))
            except Exception:
                done = imported + error + cancelled
            running = 0
        elif total and missing >= total:
            # All session keys gone and no terminal marker.
            # Prefer counters / message fragments; never keep a fully-missing
            # batch as "running" forever (ghost cards after Redis TTL).
            msg = str((batch or {}).get("message") or "")
            if isinstance(batch, dict):
                try:
                    imported = int(batch.get("imported") or imported or 0)
                except Exception:
                    pass
                try:
                    error = int(batch.get("error") or error or 0)
                except Exception:
                    pass
                try:
                    cancelled = int(batch.get("cancelled") or cancelled or 0)
                except Exception:
                    pass
            # Parse "ok=N fail=M" style messages written by the spawner.
            if imported == 0 and error == 0 and cancelled == 0 and msg:
                import re as _re

                m_ok = _re.search(r"ok\s*=\s*(\d+)", msg)
                m_fail = _re.search(r"fail\s*=\s*(\d+)", msg)
                if m_ok:
                    try:
                        imported = int(m_ok.group(1))
                    except Exception:
                        pass
                if m_fail:
                    try:
                        error = int(m_fail.group(1))
                    except Exception:
                        pass
            done = imported + error + cancelled
            if cancelled and not imported and not error:
                status = "cancelled"
            elif imported and not error and not cancelled:
                status = "done"
            elif imported:
                status = "partial"
            elif error:
                status = "error"
            elif stored in ("stopping",):
                status = "stopped"
            else:
                status = "done"
            running = 0
        else:
            status = "running"
    elif done >= max(target, total) and running == 0:
        if cancelled and not imported and not error:
            status = "cancelled"
        elif error == 0 and cancelled == 0:
            status = "done"
        elif imported:
            status = "partial"
        else:
            status = "error"
    elif running == 0 and missing > 0 and done > 0 and observed < total:
        # Partial visibility (some sessions expired) but nothing live.
        if imported and (error or cancelled or missing):
            status = "partial"
        elif imported and not error and not cancelled:
            status = "done"
        elif cancelled and not imported and not error:
            status = "cancelled"
        elif error and not imported:
            status = "error"
        else:
            status = "partial"
    elif total and (imported or error or cancelled) and running:
        status = "running"
    elif running:
        status = "running"

    # Honour explicit cooperative stop marker on the batch itself.
    if isinstance(batch, dict):
        bst = str(batch.get("status") or "").lower()
        if bst in ("stopping", "cancelled", "stopped") and running == 0:
            if status == "running":
                status = "cancelled" if cancelled or bst != "stopping" else "stopped"
        if bst == "stopping" and running:
            status = "running"

    return {
        "total": max(total, target),
        "imported": imported,
        "error": error,
        "cancelled": cancelled,
        "running": running,
        "missing": missing,
        "done": done,
        "batch_status": status,
    }


def get_registration_batch(batch_id: str) -> dict[str, Any] | None:
    b = _load_reg_batch(batch_id)
    if not b:
        return None
    sids = list(b.get("session_ids") or [])
    stats = _batch_stats(sids, batch=b)
    # Keep response bounded for large batches. Prefer LIVE (non-terminal) sessions
    # so the admin log always shows in-flight progress, not only the newest finished ones.
    try:
        MAX_BATCH_SESSIONS = max(8, min(32, int(os.environ.get("GROK2API_REG_BATCH_EMBED", "24") or 24)))
    except (TypeError, ValueError):
        MAX_BATCH_SESSIONS = 24
    loaded: list[dict[str, Any]] = []
    for s in sids:
        sess = _load_reg_sess(s)
        if sess:
            loaded.append(_compact_session(sess))
    try:
        loaded.sort(
            key=lambda s: float(s.get("updated_at") or s.get("created_at") or 0),
            reverse=True,
        )
    except Exception:
        pass
    nonterm = []
    term = []
    for sess in loaded:
        st = str(sess.get("status") or "").strip().lower()
        if st in _TERMINAL_STATUSES:
            term.append(sess)
        else:
            nonterm.append(sess)
    # Live first (all of them up to cap), then recent terminal fillers.
    sessions = nonterm[:MAX_BATCH_SESSIONS]
    if len(sessions) < MAX_BATCH_SESSIONS:
        sessions.extend(term[: MAX_BATCH_SESSIONS - len(sessions)])
    out = {**b, **stats, "sessions": sessions}
    # Surface effective status for older UIs that only read `status`.
    if stats.get("batch_status"):
        # Don't clobber an explicit cooperative "stopping" marker while workers live.
        if str(b.get("status") or "").lower() != "stopping" or stats.get("running", 0) == 0:
            if stats.get("running", 0) == 0 or str(b.get("status") or "").lower() in (
                "",
                "running",
                "starting",
            ):
                out["status"] = stats["batch_status"]
    return out


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #
def main() -> int:
    print("grok-build-auth adapter for grokcli-2api")
    result = start_registration()
    print(json.dumps(result, ensure_ascii=False, indent=2))
    if not result.get("ok"):
        return 1

    sid = result["id"]
    deadline = time.time() + 600
    while time.time() < deadline:
        sess = get_registration_session(sid, include_auth_json=True)
        if not sess:
            print("session disappeared", file=sys.stderr)
            return 1
        status = sess.get("status")
        print(f"[{time.strftime('%H:%M:%S')}] {status}: {sess.get('message')}")
        if status in ("imported", "error"):
            print(json.dumps(sess, ensure_ascii=False, indent=2))
            return 0 if status == "imported" else 1
        time.sleep(5)

    print("timeout", file=sys.stderr)
    return 1


if __name__ == "__main__":
    raise SystemExit(main())
