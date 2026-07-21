import threading
import time
import unittest

from grok2api.upstream.registration_admission import (
    AdmissionTimeout,
    RegistrationAdmission,
)


class RegistrationAdmissionTests(unittest.TestCase):
    def test_waiter_heartbeats_until_slot_is_released(self) -> None:
        admission = RegistrationAdmission(1)
        admission.acquire(timeout_seconds=0.5)
        heartbeats: list[dict[str, int]] = []
        acquired = threading.Event()
        errors: list[BaseException] = []

        def worker() -> None:
            try:
                admission.acquire(
                    heartbeat=lambda _waited, state: heartbeats.append(state),
                    timeout_seconds=1,
                    heartbeat_seconds=0.02,
                    poll_seconds=0.01,
                )
                acquired.set()
                admission.release()
            except BaseException as exc:  # pragma: no cover - asserted below
                errors.append(exc)

        thread = threading.Thread(target=worker)
        thread.start()
        deadline = time.monotonic() + 0.5
        while not heartbeats and time.monotonic() < deadline:
            time.sleep(0.01)

        self.assertTrue(heartbeats)
        self.assertEqual(heartbeats[0]["in_use"], 1)
        self.assertEqual(heartbeats[0]["waiters"], 1)
        self.assertTrue(admission.release())
        thread.join(timeout=1)

        self.assertFalse(thread.is_alive())
        self.assertFalse(errors)
        self.assertTrue(acquired.is_set())
        self.assertEqual(
            admission.snapshot(),
            {"limit": 1, "in_use": 0, "available": 1, "waiters": 0},
        )

    def test_timeout_is_explicit_and_cleans_waiter_count(self) -> None:
        admission = RegistrationAdmission(1)
        admission.acquire(timeout_seconds=0.5)

        with self.assertRaisesRegex(AdmissionTimeout, "in_use=1/1"):
            admission.acquire(
                timeout_seconds=0.05,
                heartbeat_seconds=0.01,
                poll_seconds=0.01,
            )

        self.assertEqual(admission.snapshot()["waiters"], 0)
        self.assertEqual(admission.snapshot()["in_use"], 1)
        self.assertTrue(admission.release())

    def test_cancel_exception_propagates_and_cleans_waiter_count(self) -> None:
        admission = RegistrationAdmission(1)

        class Cancelled(Exception):
            pass

        with self.assertRaises(Cancelled):
            admission.acquire(
                check_cancel=lambda: (_ for _ in ()).throw(Cancelled()),
                timeout_seconds=0.5,
            )

        self.assertEqual(admission.snapshot()["waiters"], 0)
        self.assertFalse(admission.release())


if __name__ == "__main__":
    unittest.main()
