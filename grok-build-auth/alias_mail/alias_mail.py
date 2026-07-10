#!/usr/bin/env python3
# -*- coding: utf-8 -*-
"""Cloudflare D1-backed alias mailbox helper (optional email backend).

No secrets are hardcoded. Configure via environment variables:

  CLOUDFLARE_API_TOKEN
  CLOUDFLARE_ACCOUNT_ID
  CLOUDFLARE_D1_DB_ID
  ALIAS_MAIL_DOMAINS          comma-separated domains you control
  ALIAS_EXTRA_DOMAINS         optional extra allowed domains
  CLOUDFLARE_MCP_READ_ALL_TOKEN   alternate token name
"""

from __future__ import annotations

import argparse
import email
import html
import json
import os
import random
import re
import secrets
import string
import sys
import time
from dataclasses import dataclass
from email import policy
from email.header import decode_header, make_header
from email.parser import BytesParser
from pathlib import Path
from typing import Any

import requests


try:
    sys.stdout.reconfigure(encoding="utf-8", errors="replace")
    sys.stderr.reconfigure(encoding="utf-8", errors="replace")
except Exception:
    pass


def _env(name: str, default: str = "") -> str:
    return (os.environ.get(name) or default).strip()


def _split_domains(raw: str) -> list[str]:
    out: list[str] = []
    for part in (raw or "").split(","):
        d = part.strip().lower()
        if d and d not in out:
            out.append(d)
    return out


ACCOUNT_ID = _env("CLOUDFLARE_ACCOUNT_ID")
MAIL_DB_ID = _env("CLOUDFLARE_D1_DB_ID")

DOMAINS = _split_domains(_env("ALIAS_MAIL_DOMAINS"))
EXTRA_DOMAINS = _split_domains(_env("ALIAS_EXTRA_DOMAINS"))
ALLOWED_DOMAINS = DOMAINS + [d for d in EXTRA_DOMAINS if d not in DOMAINS]

API_BASE = "https://api.cloudflare.com/client/v4"
STATE_FILE = Path(__file__).with_name(".alias_domain_state.json")
HEALTH_FILE = Path(__file__).with_name(".alias_domain_health.json")


def _require_cloudflare_config() -> None:
    missing = [
        name
        for name, val in (
            ("CLOUDFLARE_ACCOUNT_ID", ACCOUNT_ID),
            ("CLOUDFLARE_D1_DB_ID", MAIL_DB_ID),
            ("ALIAS_MAIL_DOMAINS", ",".join(DOMAINS)),
        )
        if not val
    ]
    if missing:
        raise SystemExit(
            "Missing Cloudflare alias-mail config: "
            + ", ".join(missing)
            + ". Set them in the environment or a local .env (see .env.example)."
        )


def _domain_like_params(domains: list[str] | None = None) -> list[str]:
    ds = domains if domains is not None else DOMAINS
    if not ds:
        raise SystemExit("ALIAS_MAIL_DOMAINS is empty; set at least one domain.")
    return [f"%@{d}" for d in ds]


def _sql_like_or(column: str, n: int) -> str:
    return " OR ".join([f"{column} LIKE ?" for _ in range(n)])


@dataclass
class CF:
    token: str

    def request(self, method: str, path: str, **kwargs: Any) -> Any:
        url = API_BASE + path
        headers = kwargs.pop("headers", {})
        headers.update({
            "Authorization": f"Bearer {self.token}",
            "Content-Type": "application/json",
        })
        # Do not inherit a local HTTPS_PROXY for Cloudflare API calls.
        kwargs.pop("proxies", None)
        last_error: Exception | None = None
        for attempt in range(4):
            try:
                with requests.Session() as sess:
                    sess.trust_env = False
                    r = sess.request(method, url, headers=headers, timeout=30, **kwargs)
                break
            except requests.exceptions.SSLError as exc:
                last_error = exc
                if attempt >= 3:
                    raise
                time.sleep(1.2 * (attempt + 1))
            except requests.exceptions.RequestException as exc:
                last_error = exc
                if attempt >= 2:
                    raise
                time.sleep(0.8 * (attempt + 1))
        else:
            raise SystemExit(str(last_error or "cloudflare request failed"))
        try:
            data = r.json()
        except Exception:
            raise SystemExit(f"[HTTP {r.status_code}] {r.text[:500]}")
        if not data.get("success"):
            raise SystemExit(json.dumps(data, ensure_ascii=False, indent=2))
        return data

    def d1(self, sql: str, params: list[Any] | None = None) -> list[dict[str, Any]]:
        data = self.request(
            "POST",
            f"/accounts/{ACCOUNT_ID}/d1/database/{MAIL_DB_ID}/query",
            json={"sql": sql, "params": params or []},
        )
        result = data.get("result") or []
        if not result:
            return []
        return result[0].get("results") or []


def env_token() -> str:
    _require_cloudflare_config()
    token = _env("CLOUDFLARE_API_TOKEN") or _env("CLOUDFLARE_MCP_READ_ALL_TOKEN")
    if not token:
        raise SystemExit(
            "Missing CLOUDFLARE_API_TOKEN (or CLOUDFLARE_MCP_READ_ALL_TOKEN). "
            "See .env.example."
        )
    return token


def normalize_address(value: str) -> str:
    value = value.strip().lower()
    if "@" not in value:
        raise SystemExit(f"邮箱格式不正确: {value}")
    local, domain = value.rsplit("@", 1)
    if domain not in ALLOWED_DOMAINS:
        raise SystemExit(f"domain not allowed: {domain}; allowed={ALLOWED_DOMAINS}")
    if not re.fullmatch(r"[a-z0-9][a-z0-9._-]{0,62}", local):
        raise SystemExit(f"local-part 不合法: {local}")
    return f"{local}@{domain}"


ENGLISH_FIRST_NAMES = [
    "richard", "robert", "michael", "william", "david", "james", "john", "thomas",
    "daniel", "paul", "mark", "george", "kevin", "brian", "edward", "henry",
    "charles", "andrew", "steven", "patrick", "samuel", "arthur", "peter", "martin",
    "emily", "sophia", "olivia", "emma", "grace", "alice", "claire", "linda",
    "sarah", "laura", "anna", "julia", "victoria", "natalie", "rachel", "katherine",
]
ENGLISH_LAST_NAMES = [
    "miller", "wilson", "taylor", "anderson", "thomas", "moore", "martin", "jackson",
    "white", "harris", "clark", "lewis", "young", "walker", "hall", "allen",
    "king", "wright", "scott", "green", "baker", "adams", "nelson", "carter",
]


def random_local(prefix: str = "name") -> str:
    """Generate a human-looking English-name local-part for protocol registration flows."""
    first = secrets.choice(ENGLISH_FIRST_NAMES)
    last = secrets.choice(ENGLISH_LAST_NAMES)
    num = secrets.randbelow(900) + 100
    # ???????????????????/???/????
    # ????????????? local-part?
    local = f"{first}{last}"
    clean_prefix = (prefix or "").strip("-_.").lower()
    if clean_prefix and clean_prefix not in {"name", "person", "mail", "email"}:
        return f"{clean_prefix}.{local}"
    return local


def pick_domain(domain: str | None) -> str:
    if domain:
        domain = domain.lower().strip()
        if domain not in ALLOWED_DOMAINS:
            raise SystemExit(f"domain not allowed: {domain}; allowed={ALLOWED_DOMAINS}")
        return domain
    if not DOMAINS:
        raise SystemExit("ALIAS_MAIL_DOMAINS is empty; set at least one domain.")
    return random.choice(DOMAINS)


def load_state() -> dict[str, Any]:
    if not STATE_FILE.exists():
        return {}
    try:
        return json.loads(STATE_FILE.read_text(encoding="utf-8"))
    except Exception:
        return {}


def save_state(state: dict[str, Any]) -> None:
    tmp = STATE_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(state, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(STATE_FILE)


def load_health() -> dict[str, Any]:
    if not HEALTH_FILE.exists():
        return {}
    try:
        data = json.loads(HEALTH_FILE.read_text(encoding="utf-8"))
        return data if isinstance(data, dict) else {}
    except Exception:
        return {}


def save_health(data: dict[str, Any]) -> None:
    tmp = HEALTH_FILE.with_suffix(".tmp")
    tmp.write_text(json.dumps(data, ensure_ascii=False, indent=2), encoding="utf-8")
    tmp.replace(HEALTH_FILE)


def set_domain_health(domain: str, status: str, reason: str = "", sample: str = "") -> dict[str, Any]:
    domain = pick_domain(domain)
    data = load_health()
    data[domain] = {
        "status": status,
        "reason": reason,
        "sample": sample,
        "updated_at": int(time.time()),
    }
    save_health(data)
    return data[domain]


def healthy_domains(include_unhealthy: bool = False) -> list[str]:
    if include_unhealthy:
        return DOMAINS[:]
    h = load_health()
    bad = {d for d, v in h.items() if isinstance(v, dict) and v.get("status") in {"email_unreachable", "silent_drop", "disabled"}}
    good = [d for d in DOMAINS if d not in bad]
    return good


def next_rotating_domain(commit: bool = True) -> str:
    """????????????????? email_unreachable/silent_drop/disabled ????"""
    domains = healthy_domains()
    if not domains:
        raise SystemExit(f"no usable alias domains; inspect {HEALTH_FILE}")
    state = load_state()
    idx = int(state.get("next_index", 0)) % len(domains)
    domain = domains[idx]
    if commit:
        next_idx = (idx + 1) % len(domains)
        state["last_domain"] = domain
        state["next_index"] = next_idx
        state["next_domain"] = domains[next_idx]
        state["domains"] = domains
        state["all_domains"] = DOMAINS
        state["health_file"] = str(HEALTH_FILE)
        state["updated_at"] = int(time.time())
        save_state(state)
    return domain


def reset_rotating_domain(start_domain: str | None = None) -> dict[str, Any]:
    if start_domain:
        start_domain = pick_domain(start_domain)
        idx = DOMAINS.index(start_domain)
    else:
        idx = 0
    state = {
        "domains": DOMAINS,
        "next_index": idx,
        "next_domain": DOMAINS[idx],
        "updated_at": int(time.time()),
    }
    save_state(state)
    return state


def create_alias(cf: CF, address: str, password: str | None = None, source_meta: str = "local-tool") -> dict[str, Any]:
    address = normalize_address(address)
    cf.d1(
        """
        INSERT OR IGNORE INTO address(name, password, source_meta, created_at, updated_at)
        VALUES(?, ?, ?, datetime('now'), datetime('now'))
        """,
        [address, password, source_meta],
    )
    rows = cf.d1("SELECT id, name, password, source_meta, created_at, updated_at FROM address WHERE name = ?", [address])
    if not rows:
        raise SystemExit("创建失败：D1 未返回该地址")
    return rows[0]


def list_aliases(cf: CF, domain: str | None = None, limit: int = 30) -> list[dict[str, Any]]:
    if domain:
        domain = pick_domain(domain)
        return cf.d1(
            """
            SELECT a.id, a.name, a.created_at, a.updated_at,
                   (SELECT COUNT(*) FROM raw_mails r WHERE r.address = a.name) AS mail_count
            FROM address a
            WHERE a.name LIKE ?
            ORDER BY a.id DESC
            LIMIT ?
            """,
            [f"%@{domain}", limit],
        )
    likes = _domain_like_params()
    return cf.d1(
        f"""
        SELECT a.id, a.name, a.created_at, a.updated_at,
               (SELECT COUNT(*) FROM raw_mails r WHERE r.address = a.name) AS mail_count
        FROM address a
        WHERE {_sql_like_or("a.name", len(likes))}
        ORDER BY a.id DESC
        LIMIT ?
        """,
        likes + [limit],
    )


def decode_mime_header(value: str | None) -> str:
    if not value:
        return ""
    try:
        return str(make_header(decode_header(value)))
    except Exception:
        return value


def decode_raw(raw: str) -> dict[str, str]:
    msg = BytesParser(policy=policy.default).parsebytes(raw.encode("utf-8", errors="surrogateescape"))
    subject = decode_mime_header(msg.get("subject"))
    from_ = decode_mime_header(msg.get("from"))
    to = decode_mime_header(msg.get("to"))

    parts: list[str] = []
    if msg.is_multipart():
        for part in msg.walk():
            ctype = part.get_content_type()
            disp = (part.get("content-disposition") or "").lower()
            if "attachment" in disp:
                continue
            if ctype in ("text/plain", "text/html"):
                try:
                    text = part.get_content()
                except Exception:
                    payload = part.get_payload(decode=True) or b""
                    charset = part.get_content_charset() or "utf-8"
                    text = payload.decode(charset, errors="replace")
                if ctype == "text/html":
                    text = re.sub(r"<[^>]+>", " ", html.unescape(text))
                parts.append(text)
    else:
        try:
            parts.append(msg.get_content())
        except Exception:
            payload = msg.get_payload(decode=True) or b""
            charset = msg.get_content_charset() or "utf-8"
            parts.append(payload.decode(charset, errors="replace"))

    body = "\n".join(parts)
    body = re.sub(r"\s+", " ", body).strip()
    return {"subject": subject, "from": from_, "to": to, "body": body}


def list_mails(cf: CF, address: str, limit: int = 10) -> list[dict[str, Any]]:
    address = normalize_address(address)
    rows = cf.d1(
        """
        SELECT id, message_id, source, address, raw, metadata, created_at
        FROM raw_mails
        WHERE address = ?
        ORDER BY id DESC
        LIMIT ?
        """,
        [address, limit],
    )
    out = []
    for row in rows:
        parsed = decode_raw(row.get("raw") or "")
        item = dict(row)
        item.pop("raw", None)
        item.update(parsed)
        out.append(item)
    return out


def extract_code(text: str, digits: int = 6) -> str | None:
    patterns = [
        rf"(?<!\d)(\d{{{digits}}})(?!\d)",
        r"(?:code|otp|验证码|verification|verify)[^\d]{0,30}(\d{4,8})",
    ]
    for pat in patterns:
        m = re.search(pat, text, flags=re.I)
        if m:
            return m.group(1)
    return None


def latest_code(cf: CF, address: str, digits: int = 6) -> dict[str, Any] | None:
    for mail in list_mails(cf, address, limit=20):
        text = " ".join([mail.get("subject", ""), mail.get("body", ""), mail.get("from", "")])
        code = extract_code(text, digits=digits)
        if code:
            mail["code"] = code
            return mail
    return None


def domain_mail_rows(cf: CF, limit: int = 20, min_id: int | None = None) -> list[dict[str, Any]]:
    likes = _domain_like_params()
    where = f"({_sql_like_or('address', len(likes))})"
    params: list[Any] = list(likes)
    if min_id is not None:
        where += " AND id > ?"
        params.append(min_id)
    params.append(limit)
    return cf.d1(
        f"""
        SELECT id, message_id, source, address, raw, metadata, created_at
        FROM raw_mails
        WHERE {where}
        ORDER BY id DESC
        LIMIT ?
        """,
        params,
    )


def parse_mail_row(row: dict[str, Any]) -> dict[str, Any]:
    parsed = decode_raw(row.get("raw") or "")
    item = dict(row)
    item.pop("raw", None)
    item.update(parsed)
    text = " ".join([item.get("subject", ""), item.get("body", ""), item.get("from", "")])
    code = extract_code(text)
    if code:
        item["code"] = code
    return item


def latest_domain_mails(cf: CF, limit: int = 20, min_id: int | None = None) -> list[dict[str, Any]]:
    return [parse_mail_row(r) for r in domain_mail_rows(cf, limit=limit, min_id=min_id)]


def cmd_status(cf: CF, _args: argparse.Namespace) -> None:
    verify = cf.request("GET", "/user/tokens/verify")
    likes = _domain_like_params()
    where = _sql_like_or("name", len(likes))
    where_mail = _sql_like_or("address", len(likes))
    rows = cf.d1(f"SELECT COUNT(*) AS count FROM address WHERE {where}", likes)
    mails = cf.d1(f"SELECT COUNT(*) AS count FROM raw_mails WHERE {where_mail}", likes)
    print(json.dumps({
        "token_status": verify["result"]["status"],
        "domains": DOMAINS,
        "alias_count": rows[0]["count"] if rows else 0,
        "mail_count": mails[0]["count"] if mails else 0,
    }, ensure_ascii=False, indent=2))


def cmd_create(cf: CF, args: argparse.Namespace) -> None:
    if args.address:
        address = args.address
    else:
        domain = next_rotating_domain(commit=True) if args.rotate_domain else pick_domain(args.domain)
        address = f"{random_local(args.prefix)}@{domain}"
    row = create_alias(cf, address, args.password, args.source_meta)
    row["domain_rotation"] = {
        "enabled": bool(args.rotate_domain and not args.address),
        "used_domain": row["name"].rsplit("@", 1)[1],
        "next_domain": next_rotating_domain(commit=False),
    }
    print(json.dumps(row, ensure_ascii=False, indent=2))


def cmd_list(cf: CF, args: argparse.Namespace) -> None:
    print(json.dumps(list_aliases(cf, args.domain, args.limit), ensure_ascii=False, indent=2))


def cmd_inbox(cf: CF, args: argparse.Namespace) -> None:
    mails = list_mails(cf, args.address, args.limit)
    if args.full:
        print(json.dumps(mails, ensure_ascii=False, indent=2))
    else:
        compact = [
            {k: m.get(k) for k in ("id", "created_at", "from", "subject", "body")}
            for m in mails
        ]
        print(json.dumps(compact, ensure_ascii=False, indent=2))


def cmd_code(cf: CF, args: argparse.Namespace) -> None:
    deadline = time.time() + args.timeout
    while True:
        mail = latest_code(cf, args.address, args.digits)
        if mail:
            print(json.dumps({
                "address": normalize_address(args.address),
                "code": mail["code"],
                "mail_id": mail["id"],
                "created_at": mail["created_at"],
                "from": mail.get("from"),
                "subject": mail.get("subject"),
            }, ensure_ascii=False, indent=2))
            return
        if time.time() >= deadline:
            raise SystemExit("timeout: 未找到验证码")
        time.sleep(args.interval)


def cmd_domains_inbox(cf: CF, args: argparse.Namespace) -> None:
    mails = latest_domain_mails(cf, args.limit)
    compact = []
    for m in mails:
        compact.append({
            "id": m.get("id"),
            "address": m.get("address"),
            "created_at": m.get("created_at"),
            "from": m.get("from") or m.get("source"),
            "subject": m.get("subject"),
            "code": m.get("code"),
            "body": m.get("body") if args.full else (m.get("body") or "")[:240],
        })
    print(json.dumps(compact, ensure_ascii=False, indent=2))


def cmd_poll_domains(cf: CF, args: argparse.Namespace) -> None:
    likes = _domain_like_params()
    baseline_rows = cf.d1(
        f"""
        SELECT COALESCE(MAX(id), 0) AS max_id
        FROM raw_mails
        WHERE {_sql_like_or("address", len(likes))}
        """,
        likes,
    )
    baseline = int(baseline_rows[0]["max_id"] if baseline_rows else 0)
    min_id = baseline if args.since_now else None
    print(json.dumps({
        "polling_domains": DOMAINS,
        "baseline_max_id": baseline,
        "since_now": args.since_now,
        "timeout": args.timeout,
        "interval": args.interval,
    }, ensure_ascii=False))

    seen: set[int] = set()
    deadline = time.time() + args.timeout
    while True:
        mails = latest_domain_mails(cf, limit=args.limit, min_id=min_id)
        new_items = []
        for m in reversed(mails):
            mid = int(m["id"])
            if mid in seen:
                continue
            seen.add(mid)
            item = {
                "id": mid,
                "address": m.get("address"),
                "created_at": m.get("created_at"),
                "from": m.get("from") or m.get("source"),
                "subject": m.get("subject"),
                "code": m.get("code"),
                "body": (m.get("body") or "")[:300],
            }
            new_items.append(item)
        for item in new_items:
            print(json.dumps(item, ensure_ascii=False), flush=True)
            if args.stop_on_code and item.get("code"):
                return
        if time.time() >= deadline:
            print(json.dumps({"status": "timeout", "seen": len(seen)}, ensure_ascii=False))
            return
        time.sleep(args.interval)


def cmd_health(_cf: CF, args: argparse.Namespace) -> None:
    if args.set:
        info = set_domain_health(args.domain, args.set, reason=args.reason or "", sample=args.sample or "")
        print(json.dumps({"domain": args.domain, "health": info, "health_file": str(HEALTH_FILE)}, ensure_ascii=False, indent=2))
        return
    print(json.dumps({
        "domains": DOMAINS,
        "usable_domains": healthy_domains(),
        "health": load_health(),
        "health_file": str(HEALTH_FILE),
    }, ensure_ascii=False, indent=2))


def cmd_next_domain(_cf: CF, args: argparse.Namespace) -> None:
    if args.reset:
        state = reset_rotating_domain(args.start_domain)
        print(json.dumps({"reset": True, **state}, ensure_ascii=False, indent=2))
        return
    domain = next_rotating_domain(commit=not args.peek)
    print(json.dumps({
        "domain": domain,
        "committed": not args.peek,
        "next_domain": next_rotating_domain(commit=False),
        "state_file": str(STATE_FILE),
    }, ensure_ascii=False, indent=2))


def build_parser() -> argparse.ArgumentParser:
    p = argparse.ArgumentParser(description="Cloudflare D1 alias mailbox manager")
    sub = p.add_subparsers(dest="cmd", required=True)
    domain_choices = ALLOWED_DOMAINS or None

    sp = sub.add_parser("status", help="Verify token and show D1 stats")
    sp.set_defaults(func=cmd_status)

    sp = sub.add_parser("create", help="Create an alias address")
    sp.add_argument("--address", help="Full address, e.g. user@mail.example.com")
    sp.add_argument("--domain", choices=domain_choices, help="Domain for random create")
    sp.add_argument("--rotate-domain", action="store_true", help="Use rotating domain state")
    sp.add_argument("--prefix", default="xai", help="Random local-part prefix")
    sp.add_argument("--password", default=None, help="Optional mailbox password")
    sp.add_argument("--source-meta", default="alias_mail", help="source_meta tag")
    sp.set_defaults(func=cmd_create)

    sp = sub.add_parser("list", help="List aliases")
    sp.add_argument("--domain", choices=domain_choices)
    sp.add_argument("--limit", type=int, default=30)
    sp.set_defaults(func=cmd_list)

    sp = sub.add_parser("inbox", help="Show inbox for an address")
    sp.add_argument("address")
    sp.add_argument("--limit", type=int, default=10)
    sp.add_argument("--full", action="store_true")
    sp.set_defaults(func=cmd_inbox)

    sp = sub.add_parser("code", help="Poll for a verification code")
    sp.add_argument("address")
    sp.add_argument("--timeout", type=int, default=90)
    sp.add_argument("--interval", type=float, default=3)
    sp.add_argument("--digits", type=int, default=6)
    sp.set_defaults(func=cmd_code)

    sp = sub.add_parser("domains-inbox", help="Recent mail across configured domains")
    sp.add_argument("--limit", type=int, default=20)
    sp.add_argument("--full", action="store_true")
    sp.set_defaults(func=cmd_domains_inbox)

    sp = sub.add_parser("poll-domains", help="Poll configured domains for new mail/codes")
    sp.add_argument("--timeout", type=int, default=90)
    sp.add_argument("--interval", type=float, default=3)
    sp.add_argument("--limit", type=int, default=20)
    sp.add_argument("--since-now", action="store_true", help="Only mails after command start")
    sp.add_argument("--stop-on-code", action="store_true", help="Exit when a code is found")
    sp.set_defaults(func=cmd_poll_domains)

    sp = sub.add_parser("domain-health", help="View or set domain health state")
    sp.add_argument("--domain", choices=domain_choices, default=(DOMAINS[0] if DOMAINS else None))
    sp.add_argument("--set", choices=["ok", "email_unreachable", "silent_drop", "disabled"])
    sp.add_argument("--reason", default="")
    sp.add_argument("--sample", default="")
    sp.set_defaults(func=cmd_health)

    sp = sub.add_parser("next-domain", help="Peek/advance rotating domain")
    sp.add_argument("--peek", action="store_true", help="Do not advance state")
    sp.add_argument("--reset", action="store_true", help="Reset rotation state")
    sp.add_argument("--start-domain", choices=domain_choices, help="Domain after reset")
    sp.set_defaults(func=cmd_next_domain)

    return p


def main() -> None:
    args = build_parser().parse_args()
    cf = CF(env_token())
    args.func(cf, args)


if __name__ == "__main__":
    main()
