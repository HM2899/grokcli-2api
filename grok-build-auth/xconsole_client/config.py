# -*- coding: utf-8 -*-
"""Constants captured from the live x.ai Cloud Console sign-up flow.

Source capture: account_chain_20260603_235909.mitm
Decoded canonical flows: _capture\\business_flows.json
"""

# ---- hosts ----
CONSOLE_HOST = "console.x.ai"          # the Cloud Console SPA (Next.js)
ACCOUNTS_HOST = "accounts.x.ai"        # auth / account management (Next.js + gRPC-web)
ACCOUNTS_ORIGIN = "https://accounts.x.ai"

# entry point: visiting this unauthenticated redirects to the sign-in page below
HOME_URL = "https://console.x.ai/home"
SIGNIN_URL = "https://accounts.x.ai/sign-in?redirect=cloud-console"
SIGNUP_URL = "https://accounts.x.ai/sign-up?redirect=cloud-console"

# ---- gRPC-web AuthManagement service ----
GRPC_SERVICE = "auth_mgmt.AuthManagement"
RPC_CREATE_CODE = f"https://accounts.x.ai/{GRPC_SERVICE}/CreateEmailValidationCode"
RPC_VERIFY_CODE = f"https://accounts.x.ai/{GRPC_SERVICE}/VerifyEmailValidationCode"
RPC_VALIDATE_PW = f"https://accounts.x.ai/{GRPC_SERVICE}/ValidatePassword"

# ---- Next.js server action that actually creates the account ----
# (POST accounts.x.ai/sign-up?redirect=cloud-console)
#
# DEPRECATED hard-coded values — these change on every deployment of accounts.x.ai.
# The client now dynamically scrapes them from the live page in load_signup_page().
# Keep these as documentation / offline-reference only; they are NOT used by
# create_account() unless dynamic scraping fails (then a RuntimeError is raised).
NEXT_ACTION_SIGNUP = "7f5c12078de072886cfb0e08eae73e54745587675d"
NEXT_ROUTER_STATE_TREE = (
    "%5B%22%22%2C%7B%22children%22%3A%5B%22(app)%22%2C%7B%22children%22%3A%5B%22(auth)"
    "%22%2C%7B%22children%22%3A%5B%22sign-up%22%2C%7B%22children%22%3A%5B%22__PAGE__"
    "%3F%7B%5C%22redirect%5C%22%3A%5C%22cloud-console%5C%22%7D%22%2C%7B%7D%5D%7D%5D%7D"
    "%5D%7D%2C%22%24undefined%22%2C%22%24undefined%22%2C16%5D"
)

# ---- realistic browser fingerprint (Chrome 148 on Windows, from the capture) ----
USER_AGENT = ("Mozilla/5.0 (Windows NT 10.0; Win64; x64) AppleWebKit/537.36 "
              "(KHTML, like Gecko) Chrome/148.0.0.0 Safari/537.36")
SEC_CH_UA = '"Chromium";v="148", "Google Chrome";v="148", "Not/A)Brand";v="99"'
SEC_CH_UA_PLATFORM = '"Windows"'
ACCEPT_LANGUAGE = "zh-CN,zh;q=0.9"

# connect-es client marker sent on every gRPC-web call
CONNECT_ES_VERSION = "connect-es/2.1.1"

# ---- captured SAMPLE values — THROWAWAY TEST DATA, not secrets. Use as placeholders. ----
SAMPLE_EMAIL = "test@xai.test"          # 13 chars — synthetic, never a real address
SAMPLE_GIVEN_NAME = "<givenName>"
SAMPLE_FAMILY_NAME = "<familyName>"
SAMPLE_PASSWORD = "NotARealPwd123!@#"   # 17 chars — synthetic, never a real password
SAMPLE_EMAIL_CODE = "XAI0X1"            # 6-char code — synthetic test fixture
SAMPLE_CONVERSION_ID = "806733ff-ba51-4928-b62c-f682857d962b"   # a per-attempt UUID
# ---- anti-bot keys extracted from accounts.x.ai/sign-up page (public, not secrets) ----
TURNSTILE_SITEKEY = "0x4AAAAAAAhr9JGVDZbrZOo0"
TURNSTILE_URL = SIGNUP_URL
CASTLE_PK = "pk_p8GGWvD3TmFJZRsX3BQcqAv9aFVispNz"
# turnstileToken / castleRequestToken are per-attempt anti-bot tokens (see README) and
# are intentionally NOT stored here; they must be obtained live.
