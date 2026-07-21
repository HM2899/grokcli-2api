"""Process-wide admission control for registration workers.

The adapter needs a cancel-friendly semaphore wait, but a bare polling loop is
invisible to health checks and can wait forever.  This wrapper keeps observable
counters, emits periodic heartbeats, and bounds the wait without changing the
number of concurrent registration jobs.
"""

from __future__ import annotations

import threading
import time
from collections.abc import Callable
from typing import Any


class AdmissionTimeout(TimeoutError):
    """Raised when a registration worker cannot obtain a slot in time."""


class RegistrationAdmission:
    """Cancel-friendly, observable process-wide registration admission gate."""

    def __init__(self, limit: int) -> None:
        self.limit = max(1, int(limit))
        self._semaphore = threading.BoundedSemaphore(self.limit)
        self._lock = threading.Lock()
        self._in_use = 0
        self._waiters = 0

    def snapshot(self) -> dict[str, int]:
        with self._lock:
            in_use = self._in_use
            waiters = self._waiters
        return {
            "limit": self.limit,
            "in_use": in_use,
            "available": max(0, self.limit - in_use),
            "waiters": waiters,
        }

    def acquire(
        self,
        *,
        check_cancel: Callable[[], None] | None = None,
        heartbeat: Callable[[float, dict[str, int]], Any] | None = None,
        pause_remaining: Callable[[], float] | None = None,
        timeout_seconds: float = 600.0,
        heartbeat_seconds: float = 10.0,
        poll_seconds: float = 0.15,
    ) -> float:
        """Wait for a slot and return the number of seconds spent waiting."""

        started = time.monotonic()
        timeout_seconds = max(0.01, float(timeout_seconds))
        heartbeat_seconds = max(0.01, float(heartbeat_seconds))
        poll_seconds = max(0.01, min(1.0, float(poll_seconds)))
        next_heartbeat = started

        with self._lock:
            self._waiters += 1
        try:
            while True:
                if check_cancel is not None:
                    check_cancel()

                now = time.monotonic()
                waited = now - started
                if heartbeat is not None and now >= next_heartbeat:
                    try:
                        heartbeat(waited, self.snapshot())
                    except Exception:
                        # Observability must never prevent forward progress.
                        pass
                    next_heartbeat = now + heartbeat_seconds

                if waited >= timeout_seconds:
                    state = self.snapshot()
                    raise AdmissionTimeout(
                        "registration admission timeout after "
                        f"{waited:.0f}s (in_use={state['in_use']}/{state['limit']}, "
                        f"waiters={state['waiters']})"
                    )

                pause = max(0.0, float(pause_remaining() or 0.0)) if pause_remaining else 0.0
                if pause <= 0 and self._semaphore.acquire(blocking=False):
                    with self._lock:
                        self._in_use += 1
                    return waited

                sleep_for = min(poll_seconds, timeout_seconds - waited)
                if pause > 0:
                    sleep_for = min(max(0.01, pause), 1.0, timeout_seconds - waited)
                time.sleep(max(0.01, sleep_for))
        finally:
            with self._lock:
                self._waiters = max(0, self._waiters - 1)

    def release(self) -> bool:
        """Release one acquired slot; return false for an unmatched release."""

        with self._lock:
            if self._in_use <= 0:
                return False
            self._in_use -= 1
        self._semaphore.release()
        return True
