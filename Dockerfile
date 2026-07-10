# grokcli-2api — self-contained image (vendored grok-build-auth protocol engine)
FROM python:3.12-slim-bookworm

ENV PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    PIP_DISABLE_PIP_VERSION_CHECK=1 \
    GROK2API_HOST=0.0.0.0 \
    GROK2API_PORT=3000 \
    GROK2API_OPEN_BROWSER=0 \
    PYTHONPATH=/app/grok-build-auth

WORKDIR /app

# System deps: TLS only (protocol registration needs no browser)
RUN apt-get update \
    && apt-get install -y --no-install-recommends \
        ca-certificates \
        curl \
    && rm -rf /var/lib/apt/lists/*

COPY requirements.txt /app/requirements.txt
RUN python -m pip install --no-cache-dir -r /app/requirements.txt

# Copy full source last for better layer caching of deps
COPY . /app

# Ensure vendored registration packages are present
RUN test -f /app/grok-build-auth/xconsole_client/client.py \
    && test -f /app/grok_build_adapter.py \
    && python -c "import grok_build_adapter, app; print('build-check', app.APP_VERSION, grok_build_adapter.ADAPTER_BUILD)"

EXPOSE 3000

# Persist runtime data
VOLUME ["/app/data"]

CMD ["python", "app.py"]
