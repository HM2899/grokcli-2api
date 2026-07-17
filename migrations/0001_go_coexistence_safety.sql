-- PostgreSQL schema baseline plus Go/Python coexistence safety foundation.
--
-- This migration is additive and remains readable by Python v1.9.91. It must
-- run through the dedicated migrator under a PostgreSQL advisory lock; the
-- application must not execute it concurrently at startup.
--
-- The first block mirrors the Python inline bootstrap in grok2api/store/pg.py
-- so fresh PostgreSQL databases and databases already bootstrapped by Python
-- converge without deleting or rewriting existing data.

CREATE TABLE IF NOT EXISTS accounts (
  id TEXT PRIMARY KEY,
  email TEXT,
  user_id TEXT,
  team_id TEXT,
  payload JSONB NOT NULL,
  expires_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_accounts_email ON accounts (email);
CREATE INDEX IF NOT EXISTS idx_accounts_user_id ON accounts (user_id);

CREATE TABLE IF NOT EXISTS api_keys (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  prefix TEXT NOT NULL,
  key_hash TEXT NOT NULL UNIQUE,
  secret TEXT,
  enabled BOOLEAN NOT NULL DEFAULT true,
  note TEXT NOT NULL DEFAULT '',
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  last_used_at TIMESTAMPTZ,
  request_count BIGINT NOT NULL DEFAULT 0
);
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS prompt_tokens_total BIGINT NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS completion_tokens_total BIGINT NOT NULL DEFAULT 0;
ALTER TABLE api_keys ADD COLUMN IF NOT EXISTS total_tokens_total BIGINT NOT NULL DEFAULT 0;

CREATE TABLE IF NOT EXISTS app_settings (
  key TEXT PRIMARY KEY,
  value JSONB NOT NULL,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account_pool (
  account_id TEXT PRIMARY KEY,
  enabled BOOLEAN NOT NULL DEFAULT true,
  weight INT NOT NULL DEFAULT 1,
  disabled_for_quota BOOLEAN NOT NULL DEFAULT false,
  disabled_reason TEXT,
  quota_disabled_at TIMESTAMPTZ,
  quota_source TEXT,
  last_quota JSONB,
  last_probe JSONB,
  blocked_models JSONB NOT NULL DEFAULT '{}'::jsonb,
  request_count BIGINT NOT NULL DEFAULT 0,
  success_count BIGINT NOT NULL DEFAULT 0,
  fail_count BIGINT NOT NULL DEFAULT 0,
  last_used_at TIMESTAMPTZ,
  last_error TEXT,
  cooldown_until TIMESTAMPTZ,
  extra JSONB NOT NULL DEFAULT '{}'::jsonb,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS extra JSONB NOT NULL DEFAULT '{}'::jsonb;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS pool_status TEXT NOT NULL DEFAULT 'normal';
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_count INT NOT NULL DEFAULT 0;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_reason TEXT;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_code TEXT;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_model TEXT;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_tokens_actual BIGINT;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS cooldown_tokens_limit BIGINT;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS last_probe_status TEXT;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS prompt_tokens_total BIGINT NOT NULL DEFAULT 0;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS completion_tokens_total BIGINT NOT NULL DEFAULT 0;
ALTER TABLE account_pool ADD COLUMN IF NOT EXISTS total_tokens_total BIGINT NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_account_pool_status ON account_pool (pool_status);
CREATE INDEX IF NOT EXISTS idx_account_pool_cooldown_count ON account_pool (cooldown_count) WHERE cooldown_count > 0;

CREATE TABLE IF NOT EXISTS admin_audit_logs (
  id BIGSERIAL PRIMARY KEY,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  actor TEXT,
  action TEXT NOT NULL,
  target_type TEXT,
  target_id TEXT,
  summary TEXT,
  detail JSONB NOT NULL DEFAULT '{}'::jsonb,
  ip TEXT,
  user_agent TEXT,
  ok BOOLEAN NOT NULL DEFAULT true
);
CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_created_at ON admin_audit_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_action ON admin_audit_logs (action);
CREATE INDEX IF NOT EXISTS idx_admin_audit_logs_target ON admin_audit_logs (target_type, target_id);

CREATE TABLE IF NOT EXISTS task_logs (
  id BIGSERIAL PRIMARY KEY,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  kind TEXT NOT NULL,
  task_id TEXT,
  status TEXT NOT NULL DEFAULT 'running',
  summary TEXT,
  detail JSONB NOT NULL DEFAULT '{}'::jsonb,
  ok BOOLEAN,
  progress_done INTEGER NOT NULL DEFAULT 0,
  progress_total INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_task_logs_created_at ON task_logs (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_task_logs_kind ON task_logs (kind);
CREATE INDEX IF NOT EXISTS idx_task_logs_status ON task_logs (status);
CREATE INDEX IF NOT EXISTS idx_task_logs_task_id ON task_logs (task_id);

CREATE TABLE IF NOT EXISTS usage_daily (
  day DATE NOT NULL,
  dim TEXT NOT NULL,
  dim_id TEXT NOT NULL DEFAULT '',
  requests BIGINT NOT NULL DEFAULT 0,
  success BIGINT NOT NULL DEFAULT 0,
  fail BIGINT NOT NULL DEFAULT 0,
  prompt_tokens BIGINT NOT NULL DEFAULT 0,
  completion_tokens BIGINT NOT NULL DEFAULT 0,
  total_tokens BIGINT NOT NULL DEFAULT 0,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  PRIMARY KEY (day, dim, dim_id)
);
CREATE INDEX IF NOT EXISTS idx_usage_daily_dim_day ON usage_daily (dim, day DESC);

CREATE TABLE IF NOT EXISTS usage_events (
  id BIGSERIAL PRIMARY KEY,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  api_key_id TEXT,
  account_id TEXT,
  model TEXT,
  protocol TEXT,
  path TEXT,
  stream BOOLEAN,
  ok BOOLEAN NOT NULL DEFAULT true,
  prompt_tokens BIGINT NOT NULL DEFAULT 0,
  completion_tokens BIGINT NOT NULL DEFAULT 0,
  total_tokens BIGINT NOT NULL DEFAULT 0,
  cache_read_tokens BIGINT NOT NULL DEFAULT 0,
  cache_creation_tokens BIGINT NOT NULL DEFAULT 0,
  reasoning_tokens BIGINT NOT NULL DEFAULT 0,
  client_ip TEXT,
  user_agent TEXT,
  status_code INT,
  latency_ms INT,
  ttft_ms INT,
  error TEXT,
  detail JSONB NOT NULL DEFAULT '{}'::jsonb
);
ALTER TABLE usage_events ADD COLUMN IF NOT EXISTS ttft_ms INT;
CREATE INDEX IF NOT EXISTS idx_usage_events_created_at ON usage_events (created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_api_key ON usage_events (api_key_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_account ON usage_events (account_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_model ON usage_events (model, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_protocol ON usage_events (protocol, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_usage_events_client_ip ON usage_events (client_ip, created_at DESC);

CREATE TABLE IF NOT EXISTS models (
  id TEXT PRIMARY KEY,
  name TEXT,
  description TEXT,
  owned_by TEXT NOT NULL DEFAULT 'xai',
  hidden BOOLEAN NOT NULL DEFAULT false,
  synthetic BOOLEAN NOT NULL DEFAULT false,
  context_window BIGINT,
  supports_reasoning_effort BOOLEAN,
  extra JSONB NOT NULL DEFAULT '{}'::jsonb,
  sort_order INT NOT NULL DEFAULT 100,
  fetched_at TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_models_hidden ON models (hidden);
CREATE INDEX IF NOT EXISTS idx_models_sort ON models (sort_order, id);

CREATE TABLE IF NOT EXISTS schema_migrations (
  version BIGINT PRIMARY KEY,
  name TEXT NOT NULL,
  checksum TEXT NOT NULL,
  applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS account_runtime_ownership (
  account_id TEXT PRIMARY KEY,
  owner TEXT NOT NULL DEFAULT 'python',
  epoch BIGINT NOT NULL DEFAULT 1 CHECK (epoch >= 1),
  lease_until TIMESTAMPTZ,
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  CONSTRAINT account_runtime_ownership_owner_check
    CHECK (owner IN ('python', 'go', 'none'))
);

INSERT INTO account_runtime_ownership (account_id, owner, epoch)
SELECT id, 'python', 1
FROM accounts
ON CONFLICT (account_id) DO NOTHING;

CREATE INDEX IF NOT EXISTS idx_account_runtime_ownership_owner_lease
  ON account_runtime_ownership (owner, lease_until);

CREATE OR REPLACE FUNCTION g2a_seed_python_ownership()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  INSERT INTO account_runtime_ownership (account_id, owner, epoch)
  VALUES (NEW.id, 'python', 1)
  ON CONFLICT (account_id) DO NOTHING;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_accounts_seed_python_ownership ON accounts;
CREATE TRIGGER trg_accounts_seed_python_ownership
AFTER INSERT ON accounts
FOR EACH ROW EXECUTE FUNCTION g2a_seed_python_ownership();

ALTER TABLE accounts
  ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE api_keys
  ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE account_pool
  ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1;
ALTER TABLE models
  ADD COLUMN IF NOT EXISTS row_version BIGINT NOT NULL DEFAULT 1;

CREATE OR REPLACE FUNCTION g2a_bump_row_version()
RETURNS trigger
LANGUAGE plpgsql
AS $$
BEGIN
  NEW.row_version := OLD.row_version + 1;
  RETURN NEW;
END;
$$;

DROP TRIGGER IF EXISTS trg_accounts_row_version ON accounts;
CREATE TRIGGER trg_accounts_row_version
BEFORE UPDATE ON accounts
FOR EACH ROW EXECUTE FUNCTION g2a_bump_row_version();
DROP TRIGGER IF EXISTS trg_api_keys_row_version ON api_keys;
CREATE TRIGGER trg_api_keys_row_version
BEFORE UPDATE ON api_keys
FOR EACH ROW EXECUTE FUNCTION g2a_bump_row_version();
DROP TRIGGER IF EXISTS trg_account_pool_row_version ON account_pool;
CREATE TRIGGER trg_account_pool_row_version
BEFORE UPDATE ON account_pool
FOR EACH ROW EXECUTE FUNCTION g2a_bump_row_version();
DROP TRIGGER IF EXISTS trg_models_row_version ON models;
CREATE TRIGGER trg_models_row_version
BEFORE UPDATE ON models
FOR EACH ROW EXECUTE FUNCTION g2a_bump_row_version();

ALTER TABLE usage_events
  ADD COLUMN IF NOT EXISTS request_id TEXT;
ALTER TABLE usage_events
  ADD COLUMN IF NOT EXISTS implementation TEXT;
ALTER TABLE usage_events
  ADD COLUMN IF NOT EXISTS ownership_epoch BIGINT;

CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_request_id_unique
  ON usage_events (request_id)
  WHERE request_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS idx_usage_events_implementation_created
  ON usage_events (implementation, created_at DESC)
  WHERE implementation IS NOT NULL;

CREATE TABLE IF NOT EXISTS request_usage_idempotency (
  request_id TEXT PRIMARY KEY,
  implementation TEXT NOT NULL,
  usage_event_id BIGINT,
  recorded_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_request_usage_idempotency_recorded
  ON request_usage_idempotency (recorded_at DESC);

CREATE TABLE IF NOT EXISTS registration_jobs (
  id TEXT PRIMARY KEY,
  kind TEXT NOT NULL,
  status TEXT NOT NULL DEFAULT 'pending',
  idempotency_key TEXT NOT NULL UNIQUE,
  request JSONB NOT NULL DEFAULT '{}'::jsonb,
  progress JSONB NOT NULL DEFAULT '{}'::jsonb,
  result_summary JSONB NOT NULL DEFAULT '{}'::jsonb,
  error TEXT,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  finished_at TIMESTAMPTZ,
  row_version BIGINT NOT NULL DEFAULT 1
);

CREATE INDEX IF NOT EXISTS idx_registration_jobs_status_updated
  ON registration_jobs (status, updated_at);

CREATE TABLE IF NOT EXISTS registration_results (
  id BIGSERIAL PRIMARY KEY,
  job_id TEXT NOT NULL REFERENCES registration_jobs(id) ON DELETE CASCADE,
  idempotency_key TEXT NOT NULL UNIQUE,
  account_hint TEXT,
  encrypted_bundle TEXT NOT NULL,
  bundle_checksum TEXT NOT NULL,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  consumed_at TIMESTAMPTZ,
  consumed_by TEXT,
  consume_error TEXT,
  attempts INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_registration_results_unconsumed
  ON registration_results (created_at, id)
  WHERE consumed_at IS NULL;

CREATE TABLE IF NOT EXISTS registration_outbox (
  id BIGSERIAL PRIMARY KEY,
  job_id TEXT NOT NULL REFERENCES registration_jobs(id) ON DELETE CASCADE,
  event_type TEXT NOT NULL,
  idempotency_key TEXT NOT NULL UNIQUE,
  payload JSONB NOT NULL DEFAULT '{}'::jsonb,
  created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
  delivered_at TIMESTAMPTZ,
  attempts INTEGER NOT NULL DEFAULT 0,
  last_error TEXT
);

CREATE INDEX IF NOT EXISTS idx_registration_outbox_pending
  ON registration_outbox (available_at, id)
  WHERE delivered_at IS NULL;
