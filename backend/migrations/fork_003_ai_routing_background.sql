-- Durable Responses execution/billing identity only: no prompts, outputs or credentials.
CREATE TABLE IF NOT EXISTS fork_ai_response_jobs (
 id text PRIMARY KEY,
 user_id bigint NOT NULL,
 api_key_id bigint NOT NULL,
 account_id bigint NOT NULL,
 subscription_id bigint,
 platform text NOT NULL,
 hold_amount numeric(20,8) NOT NULL CHECK (hold_amount >= 0),
 settled_actual_cost numeric(20,8) NOT NULL DEFAULT 0 CHECK (settled_actual_cost >= 0),
 identity jsonb NOT NULL CHECK (jsonb_typeof(identity) = 'object'),
 response_id text,
 status text NOT NULL DEFAULT 'admitted' CHECK (status IN ('admitted','submitting','submission_unknown','active','usage_unknown','ready','settled','released')),
 failure_reason text NOT NULL DEFAULT '',
 version bigint NOT NULL DEFAULT 1 CHECK (version > 0),
 cache_synced boolean NOT NULL DEFAULT FALSE,
 lease_owner text,
 lease_until timestamptz,
 next_attempt_at timestamptz NOT NULL DEFAULT NOW(),
 terminal jsonb CHECK (terminal IS NULL OR jsonb_typeof(terminal) = 'object'),
 billing_command jsonb CHECK (billing_command IS NULL OR jsonb_typeof(billing_command) = 'object'),
 created_at timestamptz NOT NULL DEFAULT NOW(),
 updated_at timestamptz NOT NULL DEFAULT NOW(),
 settled_at timestamptz,
 CHECK (octet_length(identity::text) <= 131072)
);
CREATE INDEX IF NOT EXISTS fork_ai_response_jobs_pending_user ON fork_ai_response_jobs(user_id) WHERE status NOT IN ('settled','released');
CREATE INDEX IF NOT EXISTS fork_ai_response_jobs_group_created ON fork_ai_response_jobs(((identity->>'source_group_id')::bigint),created_at DESC);
CREATE INDEX IF NOT EXISTS fork_ai_response_jobs_pending_key ON fork_ai_response_jobs(api_key_id) WHERE status NOT IN ('settled','released');
CREATE INDEX IF NOT EXISTS fork_ai_response_jobs_pending_subscription ON fork_ai_response_jobs(subscription_id) WHERE status NOT IN ('settled','released');
CREATE INDEX IF NOT EXISTS fork_ai_response_jobs_platform_usage ON fork_ai_response_jobs(user_id,platform,settled_at) WHERE subscription_id IS NULL AND status<>'released';
CREATE UNIQUE INDEX IF NOT EXISTS fork_ai_response_jobs_account_response ON fork_ai_response_jobs(account_id,response_id) WHERE response_id IS NOT NULL;
CREATE INDEX IF NOT EXISTS fork_ai_response_jobs_due ON fork_ai_response_jobs(next_attempt_at,lease_until) WHERE status NOT IN ('settled','released') OR NOT cache_synced;

-- Ordinary quota cache snapshots must never reset the durable background
-- contribution. Only explicit administrator resets write these cutoffs.
CREATE TABLE IF NOT EXISTS fork_ai_platform_windows (
 user_id bigint NOT NULL,
 platform text NOT NULL,
 monthly_window_start timestamptz,
 daily_reset_at timestamptz,
 weekly_reset_at timestamptz,
 monthly_reset_at timestamptz,
 PRIMARY KEY (user_id,platform)
);
