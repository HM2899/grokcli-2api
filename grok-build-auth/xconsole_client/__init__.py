# -*- coding: utf-8 -*-
"""xconsole_client — a protocolization of the x.ai Cloud Console sign-up/sign-in flow.

Reconstructed from a mitmproxy capture of https://console.x.ai/home (2026-06-03/04).

Public API:
    from xconsole_client import XConsoleAuthClient
    c = XConsoleAuthClient(debug=True)                          # default: curl_cffi (browser fingerprint)
    c = XConsoleAuthClient(transport="urllib", debug=True)      # stdlib fallback (no fingerprint)

    c.create_email_validation_code("you@example.com")
    c.verify_email_validation_code("you@example.com", "ABC123")
    strength = c.validate_password("you@example.com", "hunter2hunter2")
    c.create_account(... turnstile_token=..., castle_request_token=..., conversion_id=...)
"""
from .client import XConsoleAuthClient
from .models import GrpcResult, PasswordStrength, SignupResult
from .solver import YesCaptchaSolver, create_solver
from .sso import (
    SSOExtractor,
    parse_sso_jwt_url,
    parse_jwt_payload,
    parse_sso_jwt_payload,
    parse_sso_from_set_cookies,
    save_sso,
    list_saved_tokens,
)
from .xai_oauth import (
    CLIPROXYAPI_GROK_BASE_URL,
    CLIPROXYAPI_GROK_HEADERS,
    DEFAULT_CLIENT_ID as XAI_OAUTH_CLIENT_ID,
    DEFAULT_SCOPES as XAI_OAUTH_SCOPES,
    OAuthLoginResult,
    build_authorization_url,
    build_cliproxyapi_auth_record,
    complete_build_oauth,
    default_cliproxyapi_auth_dir,
    exchange_code_for_token,
    fetch_userinfo as fetch_xai_userinfo,
    login_with_browser as xai_oauth_login,
    login_with_playwright as xai_oauth_login_playwright,
    refresh_access_token as refresh_xai_access_token,
    save_cliproxyapi_auth_record,
    save_oauth_record as save_xai_oauth,
)
from .oauth_protocol import login_with_protocol as xai_oauth_login_protocol
from . import grpcweb, config, sso

# fingerprint.py is optional (depends on curl_cffi); expose it only if importable.
try:
    from .fingerprint import FingerprintTransport  # noqa: F401
    _has_fingerprint = True
except Exception:
    _has_fingerprint = False

__all__ = [
    "XConsoleAuthClient",
    "GrpcResult",
    "PasswordStrength",
    "SignupResult",
    "YesCaptchaSolver",
    "create_solver",
    "SSOExtractor",
    "parse_sso_jwt_url",
    "parse_jwt_payload",
    "parse_sso_jwt_payload",
    "parse_sso_from_set_cookies",
    "save_sso",
    "list_saved_tokens",
    "XAI_OAUTH_CLIENT_ID",
    "XAI_OAUTH_SCOPES",
    "CLIPROXYAPI_GROK_BASE_URL",
    "CLIPROXYAPI_GROK_HEADERS",
    "OAuthLoginResult",
    "build_authorization_url",
    "build_cliproxyapi_auth_record",
    "complete_build_oauth",
    "default_cliproxyapi_auth_dir",
    "exchange_code_for_token",
    "fetch_xai_userinfo",
    "xai_oauth_login",
    "xai_oauth_login_playwright",
    "xai_oauth_login_protocol",
    "refresh_xai_access_token",
    "save_cliproxyapi_auth_record",
    "save_xai_oauth",
    "grpcweb",
    "config",
    "sso",
]
if _has_fingerprint:
    __all__.append("FingerprintTransport")
