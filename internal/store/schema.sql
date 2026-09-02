CREATE TABLE IF NOT EXISTS app_users (
    subject TEXT PRIMARY KEY,
    email TEXT NOT NULL DEFAULT '',
    display_name TEXT NOT NULL DEFAULT '',
    auth_provider TEXT NOT NULL DEFAULT 'keycloak',
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','suspended','rejected')),
    roles TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_login_at TIMESTAMPTZ,
    approved_by TEXT,
    approved_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS app_users_status_created_idx ON app_users (status, created_at DESC);

CREATE TABLE IF NOT EXISTS entitlement_policies (
    id BIGSERIAL PRIMARY KEY,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('system','role','user','api_key','workspace','project','plan')),
    scope_id TEXT NOT NULL,
    quota_mode TEXT NOT NULL DEFAULT 'unlimited' CHECK (quota_mode IN ('unlimited','limited')),
    token_limit BIGINT CHECK (token_limit IS NULL OR token_limit >= 0),
    interval_kind TEXT NOT NULL DEFAULT 'lifetime' CHECK (interval_kind IN ('hour','day','week','month','rolling','custom','lifetime')),
    interval_seconds BIGINT CHECK (interval_seconds IS NULL OR interval_seconds > 0),
    priority INTEGER NOT NULL DEFAULT 0,
    allowed_models JSONB NOT NULL DEFAULT '[]'::jsonb,
    concurrency_limit INTEGER,
    workload_limits JSONB NOT NULL DEFAULT '{}'::jsonb,
    effective_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    created_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE (scope_type, scope_id)
);
CREATE INDEX IF NOT EXISTS entitlement_policies_active_idx ON entitlement_policies (scope_type, scope_id, effective_from, expires_at);

CREATE TABLE IF NOT EXISTS quota_grants (
    id BIGSERIAL PRIMARY KEY,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user','api_key','workspace','project')),
    subject_id TEXT NOT NULL,
    token_amount BIGINT NOT NULL CHECK (token_amount > 0),
    remaining_tokens BIGINT NOT NULL CHECK (remaining_tokens >= 0),
    effective_from TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    reason TEXT NOT NULL DEFAULT '',
    created_by TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS quota_grants_active_idx ON quota_grants (subject_type, subject_id, effective_from, expires_at);

CREATE TABLE IF NOT EXISTS usage_ledger (
    id BIGSERIAL PRIMARY KEY,
    request_id TEXT NOT NULL UNIQUE,
    subject_type TEXT NOT NULL CHECK (subject_type IN ('user','api_key')),
    subject_id TEXT NOT NULL,
    user_subject TEXT,
    api_key_id TEXT,
    model_alias TEXT NOT NULL DEFAULT 'auto',
    workload TEXT NOT NULL DEFAULT 'fast',
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    total_tokens BIGINT NOT NULL DEFAULT 0,
    reserved_tokens BIGINT NOT NULL DEFAULT 0,
    status TEXT NOT NULL CHECK (status IN ('reserved','completed','cancelled','failed')),
    started_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    completed_at TIMESTAMPTZ,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb
);
CREATE INDEX IF NOT EXISTS usage_ledger_subject_time_idx ON usage_ledger (subject_type, subject_id, started_at DESC);

CREATE TABLE IF NOT EXISTS access_audit_log (
    id BIGSERIAL PRIMARY KEY,
    actor_subject TEXT NOT NULL,
    action TEXT NOT NULL,
    target_type TEXT NOT NULL,
    target_id TEXT NOT NULL,
    old_value JSONB,
    new_value JSONB,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS access_audit_log_time_idx ON access_audit_log (created_at DESC);

CREATE TABLE IF NOT EXISTS api_keys (
    id TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL REFERENCES app_users(subject) ON DELETE CASCADE,
    name TEXT NOT NULL,
    key_prefix TEXT NOT NULL,
    key_hash TEXT NOT NULL UNIQUE,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    status TEXT NOT NULL DEFAULT 'active' CHECK (status IN ('active','revoked')),
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at TIMESTAMPTZ,
    last_used_at TIMESTAMPTZ,
    revoked_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS api_keys_owner_status_idx ON api_keys (owner_subject, status, created_at DESC);
