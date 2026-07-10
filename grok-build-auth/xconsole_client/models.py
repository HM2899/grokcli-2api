# -*- coding: utf-8 -*-
"""Typed results for the x.ai console auth protocol."""
from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Dict, List, Optional


@dataclass
class GrpcResult:
    """Outcome of one gRPC-web unary call."""
    ok: bool
    http_status: int
    grpc_status: Optional[int]
    messages: List[List[Dict[str, Any]]] = field(default_factory=list)
    trailers: Dict[str, str] = field(default_factory=dict)
    raw: bytes = b""

    @property
    def first_message(self) -> List[Dict[str, Any]]:
        return self.messages[0] if self.messages else []


@dataclass
class PasswordStrength:
    """Decoded ValidatePassword response.

    The wire schema observed (field numbers are stable across the capture):
      field 1  varint   -> always 8 in the capture; treated as an opaque score/flags value
      field 2  fixed32  -> float, present for an accepted password (e.g. 4.0)
      field 3  fixed32  -> float (e.g. 3.0)
      field 4  fixed32  -> float (e.g. 4.0)
      field 5  bytes    -> nested feedback sub-message, present for WEAK passwords (warning)
      field 7  bytes    -> nested feedback sub-message, present for WEAK passwords (suggestion)
    Semantics are best-effort; the raw fields are always preserved.
    """
    raw_fields: List[Dict[str, Any]]

    def _val(self, num: int) -> Optional[Dict[str, Any]]:
        for f in self.raw_fields:
            if f.get("field") == num:
                return f
        return None

    @property
    def score(self) -> Optional[int]:
        f = self._val(1)
        return f.get("value") if f else None

    @property
    def metrics(self) -> List[float]:
        out = []
        for num in (2, 3, 4):
            f = self._val(num)
            if f and f.get("type") == "fixed32":
                out.append(f["float"])
        return out

    @property
    def has_feedback(self) -> bool:
        """Weak passwords come back with nested feedback messages (fields 5 / 7)."""
        return any(self._val(n) for n in (5, 6, 7))


@dataclass
class SignupResult:
    """Outcome of the Next.js sign-up server action (account creation)."""
    ok: bool
    http_status: int
    set_cookies: List[str] = field(default_factory=list)
    rsc_body: str = ""

    @property
    def last_logged_in_with(self) -> Optional[str]:
        for c in self.set_cookies:
            if c.startswith("last-logged-in-with="):
                return c.split("=", 1)[1].split(";")[0]
        return None
