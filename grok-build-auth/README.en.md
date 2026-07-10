> [简体中文](README.md) | **English**

# grok-build-auth

A **protocol-research client** for publicly observable **x.ai / Grok web authentication** flows. It reimplements, over pure HTTP:

`signup → SSO → OAuth PKCE (Grok Build / CLI scopes) → local auth JSON export`

for protocol analysis, interoperability research, and **authorized** local integration testing.

The default path does not launch a browser. Cloudflare Turnstile is solved via a createTask-compatible captcha service.

[![License: MIT](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)
[![Python](https://img.shields.io/badge/python-3.9%2B-blue)](https://www.python.org/)
[![Use](https://img.shields.io/badge/use-research%20%2F%20authorized%20only-red)](#legal-boundary)

---

> [!CAUTION]
> **Using this project constitutes acceptance of all terms in [`NOTICE`](NOTICE).**  
> Provided **AS IS**, with **no warranties**. Maintainers accept **no liability**.  
> **Allowed only** on systems you own / legitimate CTF / authorized bug-bounty in-scope assets / security research & education.  
> **Prohibited:** fraud, bulk account farming for resale, unauthorized targets, intentional ToS abuse.  
> You bear all legal responsibility. If you do not accept the terms, **do not use, do not clone, delete every copy**.

---

## Legal boundary

| | |
|---|---|
| **Allowed** | Your own accounts and environments; clearly authorized security research; CTF / academic protocol study; offline source reading |
| **Prohibited** | Fraud, bulk signup for resale, unlicensed automation against unauthorized targets, intentional platform abuse |
| **Liability** | Account bans, quota loss, civil / criminal / administrative outcomes — **all on the user** |
| **Affiliation** | **Not** affiliated with, endorsed by, or sponsored by xAI, Grok, Cloudflare, CLIProxyAPI, captcha vendors, or mailbox vendors |

Full terms: [`NOTICE`](NOTICE). License is [MIT](LICENSE), but **MIT is not the entire disclaimer**.

If you are unsure whether your use is lawful — **do not run**. Ask a lawyer first, or contact the target platform’s security team.

---

## What this is

A research-oriented protocol client, **not** an official SDK.

| Stage | Content |
|---|---|
| **Signup** | Email code (gRPC-web) + Turnstile + Next.js Server Action on `accounts.x.ai` |
| **SSO** | Session JWT extraction for OAuth session reuse |
| **OAuth** | `auth.x.ai` PKCE + cookie-setter + consent; CreateSession fallback |
| **Export** | Local `type=xai` auth files compatible with [CLIProxyAPI](https://github.com/router-for-me/CLIProxyAPI) (Grok Build channel) |

Highlights:

- **Protocol-first** pure HTTP (`curl_cffi`) by default  
- **SSO reuse** can skip a second Turnstile on OAuth  
- **Interop export** for `cli-chat-proxy.grok.com` + grok-cli headers — **not** the paid `api.x.ai` credits API path  
- Concurrent signup workers; OAuth serialized by default  

SSO **alone cannot** become a CPA auth file; OAuth tokens are required.

---

## Architecture

```mermaid
flowchart LR
    A[run.py] --> B[signup client.py]
    B --> C[SSO sso.py]
    C --> D[OAuth oauth_protocol.py]
    D --> E[token exchange]
    E --> F[cliproxyapi_auth/*.json]
```

---

## Requirements

- Python 3.9+
- YesCaptcha (or createTask-compatible) API key for Turnstile  
- Tempmail.lol API key **or** your own Cloudflare D1 alias mailbox  
- Optional HTTP(S) proxy  
- Optional local CLIProxyAPI install to load exported auth files  

Platform terms, risk controls, and API changes may break the flow at any time. Maintainers have **no duty** to keep it working.

---

## Getting started

### Install

```bash
git clone https://github.com/<you>/grok-build-auth.git
cd grok-build-auth
python -m venv .venv
source .venv/bin/activate   # Windows: .venv\Scripts\activate
pip install -r requirements.txt
cp .env.example .env
# put only your own secrets in .env — never commit it
```

### Configure

See [`.env.example`](.env.example). Never commit `.env` or runtime token directories. See [`SECURITY.md`](SECURITY.md).

### Run (research / accounts you own)

```bash
python run.py -n 1
python run.py -n 5 -t 3
python run.py -n 1 -e cloudflare
python run.py -n 1 --no-oauth
python run.py -n 1 --cliproxyapi-auth-dir /path/to/CLIProxyAPI/data/auth
python run.py -n 1 --oauth-debug
```

Helpers: `xai_oauth_login.py`, `xai_oauth_export_cliproxyapi.py`, `xai_build_quota_probe.py`.

---

## Contributing

Contributions welcome **only** for lawful research and authorized use:

1. Protocol adaptations with repro steps / capture diffs  
2. Docs and translations  
3. Robustness (timeouts, retries, error taxonomy)  
4. Redacted research notes — **no** real tokens, emails, or cookies  

PRs/issues that request help with unauthorized abuse or bulk farming will be closed.

Security: private channel only — [`SECURITY.md`](SECURITY.md).

---

## Community

| Channel | Purpose |
|---|---|
| [**LINUX DO**](https://linux.do/) | Technical discussion, protocol research feedback, long-term notes |
| QQ group **`1058789350`** | Real-time Chinese community chat |
| GitHub Issues | Bug reports and PRs (primary entry) |

---

## Disclaimer

> [!IMPORTANT]
> Using this project means you have read, understood, and accepted **all** of [`NOTICE`](NOTICE).  
> If you cannot accept — do not use; delete all copies.

**Summary (NOTICE controls):** AS IS; authorized scope only; user bears all liability; maintainers have no support or adaptation duty; no affiliation with xAI / Grok / Cloudflare / CLIProxyAPI or other named vendors.

License: [MIT](LICENSE) · Notice: [NOTICE](NOTICE) · Security: [SECURITY.md](SECURITY.md)
