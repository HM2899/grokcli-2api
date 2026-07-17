> Architecture boundary: [ARCHITECTURE_GO_PYTHON_BOUNDARY.md](./ARCHITECTURE_GO_PYTHON_BOUNDARY.md)

# Anthropic Messages Go canary

This runbook enables the staged Go implementation of:

- `POST /v1/messages`
- `POST /messages`
- `POST /v1/messages/count_tokens`
- `POST /messages/count_tokens`

Python remains the production oracle until this canary is explicitly enabled and
verified. Docker still defaults `GROK2API_RUNTIME=python`.

## Preconditions

1. Shared stores are hybrid and healthy:
   - PostgreSQL reachable (`DATABASE_URL` / `GROK2API_DATABASE_URL`)
   - Redis reachable (`REDIS_URL` / `GROK2API_REDIS_URL`)
2. Migrations applied with the dedicated migrator (never by app startup):

```bash
go build -o bin/grok2api-migrate ./cmd/grok2api-migrate
./bin/grok2api-migrate up
```

3. Go binary available:

```bash
go build -o bin/grok2api ./cmd/grok2api
go test ./...
```

4. At least one live account in the pool (same pool Python already uses).

## Recommended staged enable order

Do **not** flip every Go route at once.

| Step | Flags | Why |
|------|-------|-----|
| 0 | `GROK2API_RUNTIME=go` only | Process probes + static shell |
| 1 | `GROK2API_GO_PUBLIC_READ=1` | `/v1/models` |
| 2 | `GROK2API_GO_MESSAGES=1` | Anthropic Messages canary |
| optional later | `GROK2API_GO_CHAT=1` / `GROK2API_GO_RESPONSES=1` | Other protocol surfaces |
| optional later | admin/write flags | Separate data-safety review |

Keep write/admin/maintainer flags off for the Messages canary:

```bash
GROK2API_GO_ADMIN_WRITE=0
GROK2API_GO_WRITES=0
GROK2API_GO_OWNERSHIP_MODE=disabled
GROK2API_GO_MAINTAINER=0
```

## Example canary env

```bash
GROK2API_RUNTIME=go
GROK2API_HOST=0.0.0.0
GROK2API_PORT=3000
GROK2API_STORE_BACKEND=hybrid
GROK2API_REQUIRE_SHARED_STORES=1
GROK2API_REQUIRE_MIGRATIONS=1

# staged routes
GROK2API_GO_PUBLIC_READ=1
GROK2API_GO_MESSAGES=1
GROK2API_GO_CHAT=0
GROK2API_GO_RESPONSES=0
GROK2API_GO_ADMIN_READ=0
GROK2API_GO_ADMIN_WRITE=0
GROK2API_GO_WRITES=0

# Claude Code defaults
GROK2API_SSE_KEEPALIVE=4
GROK2API_OUTBOUND_MAX_TOOLS=1
GROK2API_HISTORY_COMPACT=0
```

### Docker / Compose

Image default is still Python. Override at runtime:

```bash
docker compose exec app printenv GROK2API_RUNTIME
# then restart with:
# GROK2API_RUNTIME=go
# GROK2API_GO_MESSAGES=1
```

Entrypoint selects `/app/bin/grok2api` when `GROK2API_RUNTIME=go`.

### systemd

See comments in `deploy/grok2api.service`. Example:

```ini
Environment=GROK2API_RUNTIME=go
Environment=GROK2API_GO_PUBLIC_READ=1
Environment=GROK2API_GO_MESSAGES=1
ExecStart=/opt/grokcli-2api/bin/grok2api
```

## Verify

### Process probes

```bash
curl -fsS http://127.0.0.1:3000/live
curl -fsS http://127.0.0.1:3000/ready
# ready must be 200 only when PG/migrations/redis checks pass
```

`/live` and `/ready` should report `"implementation":"go"`.

### Models (if public_read enabled)

```bash
curl -fsS -H "Authorization: Bearer $API_KEY" http://127.0.0.1:3000/v1/models
```

### count_tokens

```bash
curl -fsS -X POST http://127.0.0.1:3000/v1/messages/count_tokens \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{"system":"hi","messages":[{"role":"user","content":"hello"}]}'
```

### Non-stream messages

```bash
curl -fsS -X POST http://127.0.0.1:3000/v1/messages \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -H "anthropic-version: 2023-06-01" \
  -d '{
    "model":"grok-4.5",
    "max_tokens":64,
    "messages":[{"role":"user","content":"ping"}]
  }' -D -
```

Expect:

- HTTP 200
- Anthropic message object (`type=message`)
- headers such as:
  - `X-Grok2API-Protocol: anthropic`
  - `X-Grok2API-Affinity: 0|1`
  - `X-Grok2API-Prompt-Stable: 1`

### Stream messages

```bash
curl -N -X POST http://127.0.0.1:3000/v1/messages \
  -H "Authorization: Bearer $API_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model":"grok-4.5",
    "max_tokens":64,
    "stream":true,
    "messages":[{"role":"user","content":"stream ping"}]
  }'
```

Expect ordered Anthropic SSE:

1. `message_start`
2. content block start/delta/stop
3. `message_delta`
4. `message_stop`

During long gaps you may also see `event: ping` and/or `: keepalive`.

### Tool use (Claude Code critical)

Send a request with Anthropic tools and force a rewrite path if possible
(Update/StrReplace → Edit). Confirm:

- dense block indexes
- `stop_reason=tool_use` only when a tool block was emitted
- at most `GROK2API_OUTBOUND_MAX_TOOLS` tool blocks (default 1)

### Automated checks

```bash
go test ./...
go test ./internal/server -run 'AnthropicMessagesE2E|StreamAnthropic|Messages'
python3 scripts/run_regressions.py   # Python oracle still green
# process + gate smoke (no real upstream required):
./scripts/smoke_go_messages.sh
```

## Observe

Useful response headers:

| Header | Meaning |
|--------|---------|
| `X-Grok2API-Protocol` | `anthropic` |
| `X-Grok2API-Account` | selected account id |
| `X-Grok2API-Accounts` | failover chain length |
| `X-Grok2API-Affinity` | sticky route hit |
| `X-Grok2API-Affinity-Rebind` | rebound after failover |
| `X-Grok2API-Conversation-Fp` | affinity fingerprint |
| `X-Grok2API-History-*` | compact stats |
| `X-Grok2API-Prompt-Stable*` | stabilize stats |

Usage ledger rows from Go should set:

- `implementation=go`
- `protocol=anthropic`
- `detail.route=go_messages`

## Rollback

Fastest safe rollback is runtime cutover, not a rebuild:

```bash
GROK2API_RUNTIME=python
# optional explicit disable
GROK2API_GO_MESSAGES=0
```

Then restart the process/container/service.

Python continues to own:

- registration captcha/browser execution
- full admin write surfaces (unless separately enabled)
- any route whose Go flag remains false

## Known remaining gaps vs Python

Acceptable for canary if monitored; not blockers for a limited canary:

- no full history-compact parity knobs beyond env defaults already ported
- no TTFT/admin timing dashboard parity
- no soft-disconnect usage-detail polish identical to Python admin rows
- maintainer / model-health loops still Python (or disabled in Go)

If Claude Code shows empty/malformed 200s, multi-tool races, or sticky routing
loss, roll back immediately and capture:

1. request body shape (tools/thinking/stream)
2. response headers
3. usage event row
4. upstream account id

## Canary success criteria

Leave the canary running only if:

1. `/ready` stays 200 under normal load
2. Claude Code multi-turn tool loops complete without “Content block not found”
3. empty upstream 200s fail over without client-visible half envelopes when
   possible
4. affinity stickiness holds for `metadata.session_id` / cache markers
5. error rate and latency are not worse than the Python baseline for the same
   traffic slice


## Chat / Responses canary

After Messages is healthy, enable independently:

```bash
GROK2API_RUNTIME=go
GROK2API_GO_PUBLIC_READ=1
GROK2API_GO_CHAT=1
GROK2API_GO_RESPONSES=1
```

Smoke:

```bash
# chat
curl -fsS -X POST "$BASE/v1/chat/completions" \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"grok-4.5","messages":[{"role":"user","content":"ping"}]}' -D -

# responses stream
curl -N -X POST "$BASE/v1/responses" \
  -H "Authorization: Bearer $API_KEY" -H 'Content-Type: application/json' \
  -d '{"model":"grok-4.5","stream":true,"input":[{"role":"user","content":"ping"}]}'
```

Expect headers `X-Grok2API-Protocol: openai_chat` / `openai_responses`, short failover chain (`Accounts` ~4), and stream terminal frames (`data: [DONE]` / `response.completed`).


## Admin canary

Admin routes are staged separately and default off:

```bash
GROK2API_RUNTIME=go
GROK2API_GO_ADMIN_READ=1
GROK2API_GO_ADMIN_WRITE=1
```

Supported now:

- read: status/dashboard/keys/accounts/settings/models/logs/usage
- auth: setup/login/session/logout (Redis session preferred, PG fallback)
- write: keys create / patch / regenerate / delete
- write: accounts enable/disable, kick (cooldown/hard), cooldown clear
- write: accounts JSON import/export/delete/logout (PostgreSQL-backed)
- write: accounts single probe
- write: settings PUT/PATCH runtime scalars (no registration/proxy secrets)
- facade: registration-email + SSO import → Python sidecar

Not yet in Go (Python sidecar / later):

- SSO conversion execution (`scripts/sso_to_auth_json.py` + browser/captcha)
- registration machine execution + Turnstile solver
- model-health background loops / probe-all
- bulk multipart import-file jobs, sub2api/cliproxy push integrations
- full runtime settings patch surface beyond public read
