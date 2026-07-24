import inspect
import sys
import unittest
from pathlib import Path


ROOT = Path(__file__).resolve().parents[1]
sys.path.insert(0, str(ROOT / "grok-build-auth"))

from xconsole_client.client import XConsoleAuthClient  # noqa: E402
from xconsole_client.fingerprint import DEFAULT_IMPERSONATE  # noqa: E402
from xconsole_client.oauth_protocol import ProtocolOAuthClient  # noqa: E402


class XConsoleSignupPageTests(unittest.TestCase):
    @staticmethod
    def _bare_client(response):
        client = XConsoleAuthClient.__new__(XConsoleAuthClient)
        client.debug = False
        client.signup_url = "https://accounts.x.ai/sign-up?redirect=grok-com"
        client._base_headers = lambda: {}
        client._request = lambda *_args, **_kwargs: response
        return client

    def test_browser_defaults_follow_current_chrome_profile(self) -> None:
        self.assertEqual(
            inspect.signature(XConsoleAuthClient).parameters["impersonate"].default,
            "chrome",
        )
        self.assertEqual(DEFAULT_IMPERSONATE, "chrome")
        self.assertEqual(
            inspect.signature(ProtocolOAuthClient).parameters["impersonate"].default,
            "chrome",
        )

    def test_cloudflare_rejection_reports_fingerprint_or_egress(self) -> None:
        client = self._bare_client(
            (403, {"server": "cloudflare", "cf-ray": "test-ray"}, [], b"blocked")
        )

        with self.assertRaisesRegex(
            RuntimeError,
            r"HTTP 403 by Cloudflare.*browser fingerprint or proxy egress",
        ):
            client.load_signup_page()

    def test_empty_signup_response_is_reported_before_parsing(self) -> None:
        client = self._bare_client((200, {}, [], b""))

        with self.assertRaisesRegex(RuntimeError, "empty response body"):
            client.load_signup_page()

    def test_missing_nextjs_chunks_has_actionable_error(self) -> None:
        client = XConsoleAuthClient.__new__(XConsoleAuthClient)
        client.debug = False

        with self.assertRaisesRegex(RuntimeError, "did not reference any Next.js"):
            client._scrape_action_id("<html><body>challenge</body></html>")


if __name__ == "__main__":
    unittest.main()
