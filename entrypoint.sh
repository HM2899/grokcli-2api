#!/usr/bin/env bash
# Main container entrypoint:
# 1) optionally start in-process Turnstile Solver on 127.0.0.1:5072  (Python captcha)
# 2) when runtime=go, start Python registration/SSO sidecar on 127.0.0.1:18070
# 3) start grokcli-2api using the selected main runtime (Go preferred; Python fallback)
set -euo pipefail
cd /app

runtime="$(echo "${GROK2API_RUNTIME:-python}" | tr '[:upper:]' '[:lower:]')"
case "${runtime}" in
  go)
    APP_CMD=("/app/bin/grok2api")
    ;;
  python|"")
    APP_CMD=("python" "app.py")
    ;;
  *)
    echo "[entrypoint] invalid GROK2API_RUNTIME=${GROK2API_RUNTIME}; expected python or go" >&2
    exit 2
    ;;
esac
if [[ "$#" -gt 0 ]]; then
  # Docker's default CMD is still `python app.py` for Python fallback. Do not let
  # that default mask GROK2API_RUNTIME=go, but keep explicit command overrides.
  if [[ "${runtime}" == "go" && "$#" -eq 2 && "$1" == "python" && "$2" == "app.py" ]]; then
    :
  else
    APP_CMD=("$@")
  fi
fi

provider="$(echo "${GROK2API_CAPTCHA_PROVIDER:-${CAPTCHA_PROVIDER:-local}}" | tr '[:upper:]' '[:lower:]')"
enable_solver="${GROK2API_INLINE_SOLVER:-1}"
solver_port="${TURNSTILE_PORT:-5072}"
# Keep captcha browser pool size aligned with registration concurrency.
reg_concurrency="${GROK2API_REG_CONCURRENCY:-3}"
solver_thread="${TURNSTILE_THREAD:-${reg_concurrency}}"
solver_browser="${TURNSTILE_BROWSER_TYPE:-camoufox}"
solver_host="${TURNSTILE_HOST:-127.0.0.1}"
solver_pid=""
reg_pid=""

# Python package roots used by registration/SSO/captcha sidecars.
export PYTHONPATH="${PYTHONPATH:-/app:/app/grok-build-auth}"
case ":${PYTHONPATH}:" in
  *":/app:"*) ;;
  *) export PYTHONPATH="/app:${PYTHONPATH}" ;;
esac
case ":${PYTHONPATH}:" in
  *":/app/grok-build-auth:"*) ;;
  *) export PYTHONPATH="${PYTHONPATH}:/app/grok-build-auth" ;;
esac

start_inline_solver() {
  if [[ ! -f /app/turnstile-solver/api_solver.py ]]; then
    echo "[entrypoint] turnstile-solver missing; skip inline solver"
    return 0
  fi
  mkdir -p /app/turnstile-solver/logs /app/turnstile-solver/keys
  # Lazy browsers (default): pool warms on first captcha, reclaims after idle.
  # TURNSTILE_LAZY=0 restores eager warm-up. TURNSTILE_IDLE_SEC=0 disables reclaim.
  export TURNSTILE_LAZY="${TURNSTILE_LAZY:-1}"
  export TURNSTILE_IDLE_SEC="${TURNSTILE_IDLE_SEC:-180}"
  echo "[entrypoint] starting Python turnstile-solver on ${solver_host}:${solver_port} (thread=${solver_thread}, browser=${solver_browser}, lazy=${TURNSTILE_LAZY}, idle=${TURNSTILE_IDLE_SEC}s)"
  (
    cd /app/turnstile-solver
    exec python api_solver.py \
      --browser_type "${solver_browser}" \
      --thread "${solver_thread}" \
      --host "${solver_host}" \
      --port "${solver_port}" \
      --debug
  ) > /app/turnstile-solver/logs/turnstile_solver.log 2>&1 &
  solver_pid=$!
  echo "${solver_pid}" > /app/turnstile-solver/logs/turnstile_solver.pid
  echo "[entrypoint] turnstile-solver pid=${solver_pid}"

  # Wait until solver HTTP is ready (best-effort, but fail loud if still down)
  for i in $(seq 1 90); do
    if curl -fsS -m 1 "http://127.0.0.1:${solver_port}/health" >/dev/null 2>&1 \
      || curl -fsS -m 1 "http://127.0.0.1:${solver_port}/" >/dev/null 2>&1; then
      echo "[entrypoint] turnstile-solver ready"
      return 0
    fi
    if ! kill -0 "${solver_pid}" 2>/dev/null; then
      echo "[entrypoint] WARN: turnstile-solver exited early; see turnstile-solver/logs/turnstile_solver.log" >&2
      return 0
    fi
    sleep 1
  done
  echo "[entrypoint] WARN: turnstile-solver not ready after 90s; registration will wait/block until it is" >&2
}

start_registration_sidecar() {
  local reg_host reg_port wait_sec
  reg_host="${GROK2API_REGISTRATION_HOST:-127.0.0.1}"
  reg_port="${GROK2API_REGISTRATION_PORT:-18070}"
  wait_sec="${GROK2API_REGISTRATION_READY_WAIT_SEC:-30}"
  export GROK2API_REGISTRATION_SERVICE_URL="${GROK2API_REGISTRATION_SERVICE_URL:-http://${reg_host}:${reg_port}}"
  # Force loopback local solver for in-container captcha path.
  export GROK2API_LOCAL_SOLVER_URL="${GROK2API_LOCAL_SOLVER_URL:-http://127.0.0.1:${solver_port}}"
  export LOCAL_SOLVER_URL="${LOCAL_SOLVER_URL:-http://127.0.0.1:${solver_port}}"

  if [[ ! -f /app/scripts/registration_service.py ]]; then
    echo "[entrypoint] WARN: scripts/registration_service.py missing; SSO/registration admin facade unavailable" >&2
    return 0
  fi
  mkdir -p /app/turnstile-solver/logs
  echo "[entrypoint] starting Python registration/SSO sidecar on ${reg_host}:${reg_port}"
  (
    cd /app
    exec python scripts/registration_service.py
  ) > /app/turnstile-solver/logs/registration_sidecar.log 2>&1 &
  reg_pid=$!
  echo "${reg_pid}" > /app/turnstile-solver/logs/registration_sidecar.pid
  echo "[entrypoint] registration sidecar pid=${reg_pid} url=${GROK2API_REGISTRATION_SERVICE_URL}"

  for i in $(seq 1 "${wait_sec}"); do
    if curl -fsS -m 1 "${GROK2API_REGISTRATION_SERVICE_URL}/health" >/dev/null 2>&1; then
      echo "[entrypoint] registration/SSO sidecar ready"
      return 0
    fi
    if ! kill -0 "${reg_pid}" 2>/dev/null; then
      echo "[entrypoint] WARN: registration sidecar exited early; see turnstile-solver/logs/registration_sidecar.log" >&2
      return 0
    fi
    sleep 1
  done
  echo "[entrypoint] WARN: registration sidecar not ready after ${wait_sec}s; Go facades will 503 until it is" >&2
}

stop_pid() {
  local name="$1"
  local pid="$2"
  if [[ -n "${pid}" ]] && kill -0 "${pid}" 2>/dev/null; then
    echo "[entrypoint] stopping ${name} pid=${pid}"
    kill "${pid}" 2>/dev/null || true
    sleep 1
    kill -9 "${pid}" 2>/dev/null || true
  fi
}

cleanup() {
  stop_pid "registration sidecar" "${reg_pid}"
  stop_pid "turnstile-solver" "${solver_pid}"
}
trap cleanup EXIT INT TERM

# Force local solver URL to loopback when using inline mode. This only keeps the
# existing registration/solver sidecar path available; the Go runtime does not
# implement captcha solving or registration execution.
if [[ "${provider}" == "local" && "${enable_solver}" != "0" ]]; then
  export GROK2API_CAPTCHA_PROVIDER=local
  export CAPTCHA_PROVIDER=local
  export GROK2API_LOCAL_SOLVER_URL="http://127.0.0.1:${solver_port}"
  export LOCAL_SOLVER_URL="http://127.0.0.1:${solver_port}"
  start_inline_solver
fi

# When the main runtime is Go, keep Python only for registration/captcha/SSO.
# The registration sidecar is loopback-only and implements:
#   /internal/registration/v1/*
#   /internal/sso/v1/*
if [[ "${runtime}" == "go" && "${GROK2API_REGISTRATION_SIDECAR:-1}" != "0" ]]; then
  start_registration_sidecar
fi

echo "[entrypoint] starting app (${runtime}): ${APP_CMD[*]}"
exec "${APP_CMD[@]}"
