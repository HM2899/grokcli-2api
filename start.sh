#!/usr/bin/env bash
# Start grokcli-2api on Linux / macOS
set -euo pipefail
cd "$(dirname "$0")"

if ! command -v python3 >/dev/null 2>&1 && ! command -v python >/dev/null 2>&1; then
  echo "ERROR: python3 not found. Install Python 3.10+ first." >&2
  exit 1
fi

PY=python3
command -v python3 >/dev/null 2>&1 || PY=python

if ! $PY -c "import fastapi, uvicorn, httpx" 2>/dev/null; then
  echo "Installing dependencies..."
  $PY -m pip install -r requirements.txt
fi

if ! $PY -c "import curl_cffi, requests" 2>/dev/null; then
  echo "Installing remaining dependencies..."
  $PY -m pip install -r requirements.txt
fi

# Vendored grok-build-auth package path
export PYTHONPATH="$(pwd)/grok-build-auth${PYTHONPATH:+:$PYTHONPATH}"

export GROK2API_OPEN_BROWSER="${GROK2API_OPEN_BROWSER:-0}"
export GROK2API_HOST="${GROK2API_HOST:-127.0.0.1}"
export GROK2API_PORT="${GROK2API_PORT:-3000}"
export GROK2API_ACCOUNT_MODE="${GROK2API_ACCOUNT_MODE:-round_robin}"
export GROK2API_TOKEN_MAINTAIN="${GROK2API_TOKEN_MAINTAIN:-1}"

PORT="$GROK2API_PORT"
echo "Starting grokcli-2api..."
echo "  Admin:  http://127.0.0.1:${PORT}/admin"
echo "  Health: http://127.0.0.1:${PORT}/health"
echo "  OpenAI: http://127.0.0.1:${PORT}/v1"
echo "  Account mode: ${GROK2API_ACCOUNT_MODE}"
echo "  Registration: grok-build-auth (HTTP protocol, no browser)"
echo ""

exec $PY app.py
