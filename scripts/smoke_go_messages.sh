#!/usr/bin/env bash
# Local smoke for the staged Go Anthropic Messages canary.
# Does not touch production Python. Requires a free port and (for ready=200)
# healthy Redis/Postgres + applied migrations when REQUIRE_* defaults stay on.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

HOST="${GROK2API_HOST:-127.0.0.1}"
PORT="${GROK2API_PORT:-18085}"
BIN="${GROK2API_BIN:-/tmp/grok2api-go}"
API_KEY="${GROK2API_API_KEY:-smoke-secret}"
BASE="http://${HOST}:${PORT}"

echo "[smoke] building ${BIN}"
go build -o "${BIN}" ./cmd/grok2api

export GROK2API_HOST="${HOST}"
export GROK2API_PORT="${PORT}"
export GROK2API_RUNTIME=go
export GROK2API_GO_PUBLIC_READ="${GROK2API_GO_PUBLIC_READ:-1}"
export GROK2API_GO_MESSAGES="${GROK2API_GO_MESSAGES:-1}"
export GROK2API_API_KEY="${API_KEY}"
export GROK2API_REQUIRE_API_KEY=true
# Local smoke without shared stores stays fail-closed on /ready unless overridden.
export GROK2API_REQUIRE_SHARED_STORES="${GROK2API_REQUIRE_SHARED_STORES:-0}"
export GROK2API_REQUIRE_MIGRATIONS="${GROK2API_REQUIRE_MIGRATIONS:-0}"

echo "[smoke] starting ${BIN} on ${BASE}"
"${BIN}" >"/tmp/grok2api-messages-smoke.log" 2>&1 &
PID=$!
cleanup() {
  kill -TERM "${PID}" 2>/dev/null || true
  for _ in $(seq 1 20); do
    kill -0 "${PID}" 2>/dev/null || break
    sleep 0.1
  done
  kill -9 "${PID}" 2>/dev/null || true
}
trap cleanup EXIT

for _ in $(seq 1 50); do
  if curl -fsS "${BASE}/live" >/dev/null 2>&1; then
    break
  fi
  sleep 0.1
done

echo "[smoke] /live"
live="$(curl -fsS "${BASE}/live")"
echo "${live}" | grep -q '"implementation":"go"'
echo "${live}" | grep -q '"ok":true'

echo "[smoke] disabled-route semantics already covered by unit tests; messages enabled here"
echo "[smoke] count_tokens is fail-closed without healthy shared stores/readiness"
code="$(curl -s -o /tmp/smoke_count.json -w '%{http_code}' \
  -X POST "${BASE}/v1/messages/count_tokens" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"system":"hi","messages":[{"role":"user","content":"hello"}]}')"
echo "count_tokens status=${code} body=$(cat /tmp/smoke_count.json)"
# Local smoke without Redis/Postgres remains 503 (readiness or store gate).
test "${code}" = "503"

echo "[smoke] messages is fail-closed without healthy shared stores/readiness"
code="$(curl -s -o /tmp/smoke_msg.json -w '%{http_code}' \
  -X POST "${BASE}/v1/messages" \
  -H "Authorization: Bearer ${API_KEY}" \
  -H 'Content-Type: application/json' \
  -d '{"model":"grok-4.5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}')"
echo "messages status=${code} body=$(cat /tmp/smoke_msg.json)"
test "${code}" = "503"

echo "[smoke] unit e2e suite"
go test ./internal/server -run 'AnthropicMessagesE2E|StreamAnthropic|Messages' -count=1

echo "[smoke] OK — process canary gates healthy; full upstream path covered by messages_e2e_test.go"
echo "[smoke] log: /tmp/grok2api-messages-smoke.log"
echo "[smoke] rollback: stop this process; production default remains GROK2API_RUNTIME=python"
