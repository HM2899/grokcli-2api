# Go / Python Boundary

## Goal

Production traffic and admin control plane run in **Go**.

Only these remain **Python**, integrated as loopback sidecars:

1. **SSO conversion scripts** (`scripts/sso_to_auth_json.py` and related import helpers)
2. **Registration machine** (email/device registration orchestration, mailbox providers, account minting)
3. **Captcha / Turnstile solving** (`turnstile-solver`, browser pool, captcha providers)

Sidecar wiring details: [PYTHON_SIDECAR.md](./PYTHON_SIDECAR.md)

Everything else is Go:

- public API: models / chat / messages / responses
- readiness, metrics, static admin UI hosting
- admin auth, keys, settings, account pool control (enable/kick/cooldown)
- usage/logs read paths
- token maintainer / model health (Go)
- durable PG/Redis access for app state
- Redis hot path: admin sessions, affinity, pool inflight/cooldown/stats, usage buckets, leader lock

## Hard rules

1. **Go must not reimplement** captcha, browser automation, Turnstile, MoeMail/YYDS mailbox flows, or registration device-code browser execution.
2. **Go may orchestrate** registration by calling an internal Python registration HTTP API:
   - registration base: `/internal/registration/v1/*`
   - SSO import base: `/internal/sso/v1/*`
   - contract: `contracts/registration-v1.openapi.json`
   - client: `internal/registration/client`
3. **Go may invoke SSO conversion scripts** as subprocesses for admin import endpoints, or call a thin Python helper HTTP wrapper. Prefer script/subprocess first to avoid re-hosting captcha stacks.
4. Python registration/captcha process is a **sidecar**, not the public API server.
5. Public `GROK2API_RUNTIME=go` is the target default once parity is sufficient. Docker may still default Python until cutover is complete.

## Process model

```
                  +----------------------+
 client/API  ---> |  Go grok2api         |  :40081/public
                  |  - proxy protocols   |
                  |  - admin API         |
                  |  - pool / usage      |
                  +----------+-----------+
                             |
                             | HTTP internal
                             v
                  +----------------------+
                  | Python registration  |  127.0.0.1 only
                  | + turnstile solver   |
                  | + sso scripts        |
                  +----------------------+
                             |
                             v
                        PG / Redis / upstream Grok
```

## Endpoint ownership

| Surface | Owner |
|---------|-------|
| `/v1/chat/completions`, `/v1/messages`, `/v1/responses`, `/v1/models` | Go |
| `/admin/api/status|dashboard|keys|settings|logs|usage` | Go |
| `/admin/api/accounts` read + enable/kick/cooldown clear | Go |
| `/admin/api/accounts` import/export/delete/logout/normalize (PG) | Go |
| `/admin/api/accounts` import-file(s), quota | Go |
| `/admin/api/accounts/{id}/probe` + probe-batch/probe-all | Go |
| `/admin/api/maintainer`, `/admin/api/model-health` | Go |
| `/admin/api/models/sync` | Go |
| `/admin/api/settings/{cliproxyapi,sub2api}` + export/push formats | Go |
| `/admin/api/accounts/register-*` | Go facade → Python registration service |
| `/admin/api/accounts/import-sso` and SSO conversion | Go facade → Python scripts/service |
| captcha solve endpoints / browser pool | Python only |
| `/internal/registration/v1/*` | Python only |
| `/internal/sso/v1/*` | Python only |

## Feature flags

Staged Go flags remain:

- `GROK2API_RUNTIME=go|python`
- `GROK2API_GO_PUBLIC_READ`
- `GROK2API_GO_CHAT`
- `GROK2API_GO_MESSAGES`
- `GROK2API_GO_RESPONSES`
- `GROK2API_GO_ADMIN_READ`
- `GROK2API_GO_ADMIN_WRITE`
- `GROK2API_GO_MAINTAINER`
- `GROK2API_GO_WRITES`
- `GROK2API_REGISTRATION_SERVICE_URL` (Python sidecar URL)
- `GROK2API_REGISTRATION_MODE=external`

## Non-goals for Go

- reimplement Camoufox/Playwright captcha solver
- reimplement mailbox provider clients for registration
- reimplement grok-build-auth browser/device registration internals
- replace `scripts/sso_to_auth_json.py` logic in Go

## Migration sequence

1. Keep Python-only boundary documented and contract-tested.
2. Go admin registration facade (proxy to Python service).
3. Go SSO import facade (helper HTTP API on registration sidecar).
4. Go account import/export/delete/probe against PostgreSQL.
5. Go token maintainer + model health (+ Redis leader).
6. Optional later: sub2api live push/groups, async export job download files.
7. Default runtime to Go; Python process becomes sidecar-only.

## Redis ownership

| Key family | Owner |
|------------|-------|
| `g2a:admin:sess:*` | Go |
| `g2a:affinity:*` | Go |
| `g2a:inflight:*`, `g2a:soft_used:*`, `g2a:cooldown:*`, `g2a:stats:*`, `g2a:rr:index` | Go |
| `g2a:usage:day:*`, `g2a:usage:life:*` | Go |
| `g2a:lock:maintainer_leader`, `g2a:lock:maintenance` | Go |
| `g2a:reg:sess:*`, `g2a:reg:batch:*` | Python registration sidecar |
| `g2a:sso_import:job:*`, `g2a:device:sess:*` | Python (SSO/device registration helpers) |

Go main process must not reintroduce Python redis clients for proxy/admin hot paths.

## Verification

- Go unit/e2e tests for proxy/admin non-registration paths.
- Registration contract fixtures against fake/internal registration service.
- Live canary: Go handles chat/messages; Python still performs register-email + captcha.
