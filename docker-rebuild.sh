#!/usr/bin/env bash
# Force rebuild/restart so Docker cannot keep an old source image.
set -euo pipefail
cd "$(dirname "$0")"

echo "== git =="
git fetch --all
git reset --hard origin/main
echo "HEAD=$(git rev-parse --short HEAD)"

echo "== local fingerprint =="
python3 - <<'PY' || python - <<'PY'
from pathlib import Path
import re
adapter = Path('grok_build_adapter.py').read_text(encoding='utf-8')
app = Path('app.py').read_text(encoding='utf-8')
m1 = re.search(r'ADAPTER_BUILD\s*=\s*"([^"]+)"', adapter)
m2 = re.search(r'APP_VERSION\s*=\s*"([^"]+)"', app)
print('ADAPTER_BUILD=', m1.group(1) if m1 else None)
print('APP_VERSION=', m2.group(1) if m2 else None)
print('adapter_present=', Path('grok_build_adapter.py').exists())
print('engine_dir_present=', Path('grok-build-auth/xconsole_client').exists())
print('browser_runner_present=', Path('register_runner.py').exists())
PY

echo "== docker stop/remove =="
docker compose down --remove-orphans || true
docker rm -f grokcli-2api 2>/dev/null || true
docker rmi grokcli-2api:local 2>/dev/null || true

echo "== build no-cache =="
DOCKER_BUILDKIT=1 docker compose build --no-cache --pull

echo "== up =="
docker compose up -d

echo "== logs =="
sleep 2
docker compose logs --tail=60

echo "== health =="
curl -sS "http://127.0.0.1:3000/health" || true
echo
echo "Done. /health should show version=1.8.0 and registration.engine=dongguatanglinux/grok-build-auth"
