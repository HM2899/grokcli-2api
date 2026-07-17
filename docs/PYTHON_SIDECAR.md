# Python Sidecar Integration (SSO / Registration / Captcha)

Go owns the public API and most admin control plane.

**Python remains the only implementation** for:

1. **SSO conversion** — `scripts/sso_to_auth_json.py` + helpers
2. **Registration machine** — `grok2api.upstream.grok_build_adapter` + mailbox providers
3. **Captcha / Turnstile** — `turnstile-solver` (Camoufox/Playwright) and YesCaptcha clients

## Process model

```
 client ──► Go grok2api (:GROK2API_PORT)
               │
               │ HTTP loopback
               ├─► Python registration/SSO sidecar  127.0.0.1:18070
               │     /internal/registration/v1/*
               │     /internal/sso/v1/*
               │
               └─► Python turnstile-solver           127.0.0.1:5072
                     /turnstile  /health
```

`entrypoint.sh` starts both Python processes when:

- captcha provider is `local` and `GROK2API_INLINE_SOLVER!=0` → turnstile-solver
- `GROK2API_RUNTIME=go` and `GROK2API_REGISTRATION_SIDECAR!=0` → registration/SSO sidecar

## Admin entrypoints (Go facade → Python)

| Admin route | Python internal |
|-------------|-----------------|
| `POST /admin/api/accounts/register-email` | `POST /internal/registration/v1/jobs` |
| `GET  /admin/api/accounts/register-email/*` | `/internal/registration/v1/sessions|batches|...` |
| `POST /admin/api/accounts/import-sso` | `POST /internal/sso/v1/import` |
| `GET  /admin/api/accounts/import-sso/jobs/{id}` | `GET /internal/sso/v1/jobs/{id}` |

Captcha is **not** exposed on the public admin port. Registration workers call the local solver at `GROK2API_LOCAL_SOLVER_URL` (default `http://127.0.0.1:5072`).

## Required env (Docker defaults already set)

```bash
GROK2API_RUNTIME=go
GROK2API_REGISTRATION_SIDECAR=1
GROK2API_REGISTRATION_SERVICE_URL=http://127.0.0.1:18070
# optional shared secret for internal calls
# GROK2API_REGISTRATION_TOKEN=...

GROK2API_CAPTCHA_PROVIDER=local
GROK2API_INLINE_SOLVER=1
GROK2API_LOCAL_SOLVER_URL=http://127.0.0.1:5072
TURNSTILE_PORT=5072
TURNSTILE_THREAD=3          # keep aligned with GROK2API_REG_CONCURRENCY
GROK2API_REG_CONCURRENCY=3
```

YesCaptcha alternative:

```bash
GROK2API_CAPTCHA_PROVIDER=yescaptcha
GROK2API_YESCAPTCHA_KEY=...
# GROK2API_INLINE_SOLVER=0
```

## Source ownership

| Concern | Path |
|---------|------|
| Registration orchestration | `grok2api/upstream/grok_build_adapter.py` |
| Protocol client | `grok-build-auth/xconsole_client/*` |
| Mailbox providers | `grok2api/upstream/moemail.py` |
| SSO cookie → token | `scripts/sso_to_auth_json.py` |
| Sidecar HTTP | `scripts/registration_service.py` |
| Captcha browser pool | `turnstile-solver/api_solver.py` |
| Go client | `internal/registration/client` |
| Go admin facades | `internal/server/server.go` |

## Logs

- Turnstile: `/app/turnstile-solver/logs/turnstile_solver.log`
- Registration/SSO sidecar: `/app/turnstile-solver/logs/registration_sidecar.log`

## Health checks

```bash
curl -fsS http://127.0.0.1:5072/health
curl -fsS http://127.0.0.1:18070/health
# Go facade (admin session required for most routes)
curl -fsS http://127.0.0.1:${GROK2API_PORT}/admin/api/accounts/register-email/availability
```

## Hard rules

1. Do **not** reimplement captcha/browser/email registration in Go.
2. Do **not** reimplement `scripts/sso_to_auth_json.py` device-flow conversion in Go.
3. Go may only orchestrate via `/internal/registration/v1` and `/internal/sso/v1`.
4. Sidecars bind loopback only; never publish 18070/5072 on the public host by default.
