# Security Policy

## Before you report

This project is governed by [`NOTICE`](NOTICE). We **do not** provide assistance for unauthorized use, bulk abuse, or ToS evasion. Such requests will be ignored or result in a ban.

## Secrets

This repository **must not** contain:

- YesCaptcha / captcha vendor API keys  
- Cloudflare tokens or D1 identifiers with real credentials  
- Temp mailbox API keys  
- OAuth access / refresh / id tokens  
- SSO JWTs, account passwords, or personal emails  

Use a local `.env` (see `.env.example`) or shell environment variables.

**Never commit:**

- `.env`  
- `sso_output/`  
- `oauth_output/`  
- `accounts_output/`  
- `cliproxyapi_auth/`  

If a secret was ever committed to a fork or mirror, **rotate it immediately**.

## Reporting a vulnerability

If you discover:

- accidental secret exposure in history or documentation,  
- unsafe defaults that leak credentials, or  
- a security issue **in this repository itself**,  

please open a **private** GitHub Security Advisory on the published repository (or contact the maintainer through a private channel).

Do **not** file a public issue with live tokens or account details.

## Platform operators

If you represent xAI, Cloudflare, a captcha vendor, or another mentioned service and have compliance concerns about this research client, contact the maintainer via a private Security Advisory. We will treat such reports seriously.

## Responsible use

Automation against third-party services may violate their Terms of Service and local law. **You** are solely responsible for how you run this software. See [`NOTICE`](NOTICE).
