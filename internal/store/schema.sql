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
CREATE TABLE IF NOT EXISTS attachments (
    id TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL REFERENCES app_users(subject) ON DELETE CASCADE,
    name TEXT NOT NULL,
    relative_path TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'file' CHECK (source IN ('file','image','folder')),
    media_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    extract_status TEXT NOT NULL DEFAULT 'stored',
    extracted_text TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS attachments_owner_created_idx ON attachments (owner_subject, created_at DESC) WHERE deleted_at IS NULL;

CREATE TABLE IF NOT EXISTS entitlement_policies (
    id BIGSERIAL PRIMARY KEY,
    scope_type TEXT NOT NULL CHECK (scope_type IN ('system','role','user','api_key','workspace','project','plan')),
    scope_id TEXT NOT NULL,
    quota_mode TEXT NOT NULL DEFAULT 'unlimited' CHECK (quota_mode IN ('unlimited','limited')),
    token_limit BIGINT CHECK (token_limit IS NULL OR token_limit >= 0),
    interval_kind TEXT NOT NULL DEFAULT 'lifetime' CHECK (interval_kind IN ('hour','day','week','month','rolling','custom','lifetime')),
	interval_count INTEGER NOT NULL DEFAULT 1 CHECK (interval_count > 0),
    interval_seconds BIGINT CHECK (interval_seconds IS NULL OR interval_seconds > 0),
	parallel_limits JSONB NOT NULL DEFAULT '[]'::jsonb,
    resource_limits JSONB NOT NULL DEFAULT '{}'::jsonb,
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
ALTER TABLE entitlement_policies ADD COLUMN IF NOT EXISTS interval_count INTEGER NOT NULL DEFAULT 1;
ALTER TABLE entitlement_policies ADD COLUMN IF NOT EXISTS parallel_limits JSONB NOT NULL DEFAULT '[]'::jsonb;
ALTER TABLE entitlement_policies ADD COLUMN IF NOT EXISTS resource_limits JSONB NOT NULL DEFAULT '{}'::jsonb;
CREATE INDEX IF NOT EXISTS entitlement_policies_active_idx ON entitlement_policies (scope_type, scope_id, effective_from, expires_at);
CREATE TABLE IF NOT EXISTS guest_access_policy (
    singleton BOOLEAN PRIMARY KEY DEFAULT TRUE CHECK (singleton),
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    quota_mode TEXT NOT NULL DEFAULT 'limited' CHECK (quota_mode IN ('limited','unlimited')),
    token_limit BIGINT NOT NULL DEFAULT 4000 CHECK (token_limit >= 0),
    interval_kind TEXT NOT NULL DEFAULT 'day' CHECK (interval_kind IN ('hour','day','week','month','rolling','custom','lifetime')),
    interval_seconds BIGINT CHECK (interval_seconds IS NULL OR interval_seconds > 0),
    requests_per_hour INTEGER NOT NULL DEFAULT 6 CHECK (requests_per_hour >= 0),
    min_interval_seconds INTEGER NOT NULL DEFAULT 45 CHECK (min_interval_seconds >= 0),
    max_completion_tokens INTEGER NOT NULL DEFAULT 384 CHECK (max_completion_tokens > 0),
    fast_model TEXT NOT NULL DEFAULT 'fast',
    allow_uploads BOOLEAN NOT NULL DEFAULT TRUE,
    allow_image_generation BOOLEAN NOT NULL DEFAULT TRUE,
    allow_file_generation BOOLEAN NOT NULL DEFAULT TRUE,
    max_upload_bytes BIGINT NOT NULL DEFAULT 10485760 CHECK (max_upload_bytes >= 0),
    max_image_upload_bytes BIGINT NOT NULL DEFAULT 4194304 CHECK (max_image_upload_bytes >= 0),
    max_uploads_per_hour INTEGER NOT NULL DEFAULT 10 CHECK (max_uploads_per_hour >= 0),
    max_stored_files INTEGER NOT NULL DEFAULT 20 CHECK (max_stored_files >= 0),
    max_stored_bytes BIGINT NOT NULL DEFAULT 52428800 CHECK (max_stored_bytes >= 0),
    max_attachments_per_message INTEGER NOT NULL DEFAULT 10 CHECK (max_attachments_per_message >= 0),
    attachment_retention_hours INTEGER NOT NULL DEFAULT 24 CHECK (attachment_retention_hours > 0),
    image_generations_per_day INTEGER NOT NULL DEFAULT 3 CHECK (image_generations_per_day >= 0),
    file_generations_per_day INTEGER NOT NULL DEFAULT 5 CHECK (file_generations_per_day >= 0),
    max_generated_file_bytes BIGINT NOT NULL DEFAULT 1048576 CHECK (max_generated_file_bytes >= 0),
    max_generated_image_bytes BIGINT NOT NULL DEFAULT 4194304 CHECK (max_generated_image_bytes >= 0),
    updated_by TEXT,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS quota_mode TEXT NOT NULL DEFAULT 'limited';
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS allow_uploads BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS allow_image_generation BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS allow_file_generation BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_upload_bytes BIGINT NOT NULL DEFAULT 10485760;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_image_upload_bytes BIGINT NOT NULL DEFAULT 4194304;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_uploads_per_hour INTEGER NOT NULL DEFAULT 10;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_stored_files INTEGER NOT NULL DEFAULT 20;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_stored_bytes BIGINT NOT NULL DEFAULT 52428800;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_attachments_per_message INTEGER NOT NULL DEFAULT 10;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS attachment_retention_hours INTEGER NOT NULL DEFAULT 24;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS image_generations_per_day INTEGER NOT NULL DEFAULT 3;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS file_generations_per_day INTEGER NOT NULL DEFAULT 5;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_generated_file_bytes BIGINT NOT NULL DEFAULT 1048576;
ALTER TABLE guest_access_policy ADD COLUMN IF NOT EXISTS max_generated_image_bytes BIGINT NOT NULL DEFAULT 4194304;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_quota_mode_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_requests_per_hour_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_upload_bytes_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_uploads_per_hour_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_stored_files_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_stored_bytes_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_generated_file_bytes_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_image_upload_bytes_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_generated_image_bytes_check;
ALTER TABLE guest_access_policy DROP CONSTRAINT IF EXISTS guest_access_policy_max_attachments_per_message_check;
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_quota_mode_check CHECK (quota_mode IN ('limited','unlimited'));
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_requests_per_hour_check CHECK (requests_per_hour >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_upload_bytes_check CHECK (max_upload_bytes >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_uploads_per_hour_check CHECK (max_uploads_per_hour >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_stored_files_check CHECK (max_stored_files >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_stored_bytes_check CHECK (max_stored_bytes >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_generated_file_bytes_check CHECK (max_generated_file_bytes >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_image_upload_bytes_check CHECK (max_image_upload_bytes >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_generated_image_bytes_check CHECK (max_generated_image_bytes >= 0);
ALTER TABLE guest_access_policy ADD CONSTRAINT guest_access_policy_max_attachments_per_message_check CHECK (max_attachments_per_message >= 0);
CREATE TABLE IF NOT EXISTS guest_devices (
    guest_subject TEXT NOT NULL,
    device_id TEXT NOT NULL,
    device_name TEXT NOT NULL DEFAULT '',
    user_agent_hash TEXT NOT NULL DEFAULT '',
    first_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (guest_subject, device_id)
);
CREATE INDEX IF NOT EXISTS guest_devices_last_seen_idx ON guest_devices(last_seen_at DESC);
CREATE TABLE IF NOT EXISTS guest_quota_reset_events (
    id BIGSERIAL PRIMARY KEY,
    guest_subject TEXT NOT NULL,
    actor_subject TEXT NOT NULL,
    reset_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS guest_quota_reset_events_subject_time_idx ON guest_quota_reset_events(guest_subject,reset_at DESC);
CREATE TABLE IF NOT EXISTS guest_attachments (
    id TEXT PRIMARY KEY,
    guest_subject TEXT NOT NULL,
    device_id TEXT NOT NULL,
    name TEXT NOT NULL,
    relative_path TEXT NOT NULL DEFAULT '',
    source TEXT NOT NULL DEFAULT 'file' CHECK (source IN ('file','image','generated-file','generated-image')),
    media_type TEXT NOT NULL DEFAULT 'application/octet-stream',
    size_bytes BIGINT NOT NULL CHECK (size_bytes >= 0),
    sha256 TEXT NOT NULL,
    storage_path TEXT NOT NULL,
    extract_status TEXT NOT NULL DEFAULT 'stored',
    extracted_text TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    deleted_at TIMESTAMPTZ
);
CREATE INDEX IF NOT EXISTS guest_attachments_owner_created_idx ON guest_attachments(guest_subject,device_id,created_at DESC) WHERE deleted_at IS NULL;

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

CREATE TABLE IF NOT EXISTS quota_reset_grants (
    id BIGSERIAL PRIMARY KEY,
    user_subject TEXT NOT NULL REFERENCES app_users(subject) ON DELETE CASCADE,
    total_resets INTEGER NOT NULL CHECK (total_resets > 0),
    remaining_resets INTEGER NOT NULL CHECK (remaining_resets >= 0 AND remaining_resets <= total_resets),
    expires_at TIMESTAMPTZ NOT NULL,
    note TEXT NOT NULL DEFAULT '',
    created_by TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS quota_reset_grants_active_idx ON quota_reset_grants(user_subject,expires_at,remaining_resets);

CREATE TABLE IF NOT EXISTS quota_reset_events (
    id BIGSERIAL PRIMARY KEY,
    user_subject TEXT NOT NULL REFERENCES app_users(subject) ON DELETE CASCADE,
    grant_id BIGINT REFERENCES quota_reset_grants(id) ON DELETE SET NULL,
    reset_kind TEXT NOT NULL CHECK (reset_kind IN ('admin','credit')),
    actor_subject TEXT NOT NULL,
    reset_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    note TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS quota_reset_events_user_time_idx ON quota_reset_events(user_subject,reset_at DESC);

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

CREATE TABLE IF NOT EXISTS oauth_integrations (
    provider TEXT PRIMARY KEY,
    subject TEXT NOT NULL,
    email TEXT NOT NULL,
    encrypted_refresh_token TEXT NOT NULL,
    scopes TEXT[] NOT NULL DEFAULT '{}',
    connected_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE IF NOT EXISTS password_reset_tokens (
    token_hash TEXT PRIMARY KEY,
    keycloak_user_id TEXT NOT NULL,
    email TEXT NOT NULL,
    expires_at TIMESTAMPTZ NOT NULL,
    used_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS password_reset_tokens_expiry_idx ON password_reset_tokens (expires_at) WHERE used_at IS NULL;

CREATE TABLE IF NOT EXISTS model_providers (
    id TEXT PRIMARY KEY,
    name TEXT NOT NULL,
    provider_type TEXT NOT NULL CHECK (provider_type IN ('vllm','lmstudio','ollama','openai-compatible','anthropic','gemini')),
    base_url TEXT NOT NULL,
    encrypted_api_key TEXT NOT NULL DEFAULT '',
    enabled BOOLEAN NOT NULL DEFAULT TRUE,
    last_test_status TEXT NOT NULL DEFAULT 'unknown',
    last_test_message TEXT NOT NULL DEFAULT '',
    last_test_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

DO $$
DECLARE
    constraint_name TEXT;
BEGIN
    SELECT conname INTO constraint_name
    FROM pg_constraint
    WHERE conrelid='model_providers'::regclass AND contype='c'
      AND pg_get_constraintdef(oid) ILIKE '%provider_type%';
    IF constraint_name IS NOT NULL THEN
        EXECUTE format('ALTER TABLE model_providers DROP CONSTRAINT %I', constraint_name);
    END IF;
    ALTER TABLE model_providers
        ADD CONSTRAINT model_providers_provider_type_check
        CHECK (provider_type IN ('vllm','lmstudio','ollama','openai-compatible','anthropic','gemini'));
EXCEPTION WHEN duplicate_object THEN
    NULL;
END $$;
CREATE TABLE IF NOT EXISTS provider_models (
    id TEXT PRIMARY KEY,
    provider_id TEXT NOT NULL REFERENCES model_providers(id) ON DELETE CASCADE,
    upstream_model TEXT NOT NULL,
    litellm_model_name TEXT NOT NULL UNIQUE,
    litellm_model_id TEXT,
    status TEXT NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','active','error','disabled')),
    last_error TEXT NOT NULL DEFAULT '',
    max_input_tokens INTEGER NOT NULL DEFAULT 0 CHECK (max_input_tokens >= 0),
    max_output_tokens INTEGER NOT NULL DEFAULT 0 CHECK (max_output_tokens >= 0),
    tpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (tpm_limit >= 0),
    itpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (itpm_limit >= 0),
    otpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (otpm_limit >= 0),
    rpm_limit INTEGER NOT NULL DEFAULT 0 CHECK (rpm_limit >= 0),
    rpd_limit INTEGER NOT NULL DEFAULT 0 CHECK (rpd_limit >= 0),
    timeout_seconds INTEGER NOT NULL DEFAULT 300 CHECK (timeout_seconds > 0 AND timeout_seconds <= 1800),
    stream_timeout_seconds INTEGER NOT NULL DEFAULT 300 CHECK (stream_timeout_seconds > 0 AND stream_timeout_seconds <= 1800),
    max_retries INTEGER NOT NULL DEFAULT 2 CHECK (max_retries >= 0 AND max_retries <= 8),
    provider_max_retries INTEGER NOT NULL DEFAULT 0 CHECK (provider_max_retries >= 0 AND provider_max_retries <= 8),
    retry_backoff_ms INTEGER NOT NULL DEFAULT 500 CHECK (retry_backoff_ms >= 0 AND retry_backoff_ms <= 30000),
    context_strategy TEXT NOT NULL DEFAULT 'adaptive' CHECK (context_strategy IN ('adaptive','trim','fallback','reject')),
    context_target_tokens INTEGER NOT NULL DEFAULT 0 CHECK (context_target_tokens >= 0),
    agent_overhead_tokens INTEGER NOT NULL DEFAULT 0 CHECK (agent_overhead_tokens >= 0),
    research_overhead_tokens INTEGER NOT NULL DEFAULT 0 CHECK (research_overhead_tokens >= 0),
    fallback_model_name TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    UNIQUE(provider_id, upstream_model)
);
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS max_input_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS max_output_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS tpm_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS itpm_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS otpm_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS rpm_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS rpd_limit INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS timeout_seconds INTEGER NOT NULL DEFAULT 300;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS stream_timeout_seconds INTEGER NOT NULL DEFAULT 300;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS max_retries INTEGER NOT NULL DEFAULT 2;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS provider_max_retries INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS retry_backoff_ms INTEGER NOT NULL DEFAULT 500;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS context_strategy TEXT NOT NULL DEFAULT 'adaptive';
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS context_target_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS agent_overhead_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS research_overhead_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE provider_models ADD COLUMN IF NOT EXISTS fallback_model_name TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS model_aliases (
    alias TEXT PRIMARY KEY CHECK (alias IN ('fast','balanced','deep','vision')),
    litellm_model_name TEXT NOT NULL,
    updated_by TEXT NOT NULL DEFAULT '',
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE TABLE IF NOT EXISTS chat_sessions (
    id TEXT PRIMARY KEY,
    owner_subject TEXT NOT NULL REFERENCES app_users(subject) ON DELETE CASCADE,
    title TEXT NOT NULL DEFAULT 'New chat',
    model_alias TEXT NOT NULL DEFAULT 'auto',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE chat_sessions ADD COLUMN IF NOT EXISTS pinned_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS chat_sessions_owner_updated_idx ON chat_sessions (owner_subject, updated_at DESC);
CREATE INDEX IF NOT EXISTS chat_sessions_owner_pinned_updated_idx ON chat_sessions (owner_subject, pinned_at DESC, updated_at DESC);
CREATE TABLE IF NOT EXISTS chat_messages (
    id BIGSERIAL PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    role TEXT NOT NULL CHECK (role IN ('user','assistant')),
    content TEXT NOT NULL DEFAULT '',
    attachment_ids TEXT[] NOT NULL DEFAULT '{}',
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS chat_messages_session_created_idx ON chat_messages (session_id, created_at ASC, id ASC);


CREATE TABLE IF NOT EXISTS chat_runs (
    id TEXT PRIMARY KEY,
    session_id TEXT NOT NULL REFERENCES chat_sessions(id) ON DELETE CASCADE,
    owner_subject TEXT NOT NULL REFERENCES app_users(subject) ON DELETE CASCADE,
    status TEXT NOT NULL DEFAULT 'queued' CHECK (status IN ('queued','running','paused','completed','failed','cancelled')),
    research_mode TEXT NOT NULL DEFAULT 'auto',
    thinking_mode TEXT NOT NULL DEFAULT 'medium',
    command_mode TEXT NOT NULL DEFAULT '',
    command_skills TEXT[] NOT NULL DEFAULT '{}',
    request_id TEXT NOT NULL DEFAULT '',
    content TEXT NOT NULL DEFAULT '',
    error TEXT NOT NULL DEFAULT '',
    activity JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE chat_runs ADD COLUMN IF NOT EXISTS command_mode TEXT NOT NULL DEFAULT '';
ALTER TABLE chat_runs ADD COLUMN IF NOT EXISTS command_skills TEXT[] NOT NULL DEFAULT '{}';
CREATE INDEX IF NOT EXISTS chat_runs_session_created_idx ON chat_runs(session_id,created_at DESC);
CREATE INDEX IF NOT EXISTS chat_runs_owner_status_idx ON chat_runs(owner_subject,status,updated_at DESC);
ALTER TABLE chat_messages ADD COLUMN IF NOT EXISTS run_id TEXT;
CREATE INDEX IF NOT EXISTS chat_messages_run_idx ON chat_messages(run_id) WHERE run_id IS NOT NULL;
