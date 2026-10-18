-- Model usage rows, one per successful completion, fenced by the run owner.
CREATE TABLE agent_usage (
    thread_id text NOT NULL,
    run_id text NOT NULL,
    seq integer NOT NULL CHECK (seq > 0),
    model text NOT NULL DEFAULT '',
    input_tokens bigint NOT NULL CHECK (input_tokens >= 0),
    output_tokens bigint NOT NULL CHECK (output_tokens >= 0),
    cached_tokens bigint NOT NULL CHECK (cached_tokens >= 0),
    tenant text NOT NULL CHECK (tenant <> ''),
    principal text NOT NULL CHECK (principal <> ''),
    journey_id text CHECK (journey_id IS NULL OR journey_id <> ''),
    recorded_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (thread_id, run_id, seq),
    FOREIGN KEY (thread_id, run_id) REFERENCES agent_runs(thread_id, id));
CREATE INDEX agent_usage_tenant_time ON agent_usage (tenant, recorded_at);

-- A cost gateway rejected the run for a budget or rate limit.
ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_state_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_state_check CHECK (state IN
    ('pending','running','completed','failed','auth_required','interrupted','awaiting_input','dependency_unavailable','limit_exceeded'));
ALTER TABLE agent_commands DROP CONSTRAINT agent_commands_terminal_reason_check;
ALTER TABLE agent_commands ADD CONSTRAINT agent_commands_terminal_reason_check CHECK (terminal_reason IN
    ('','failed','auth_required','interrupted','consumed','dependency_unavailable','limit_exceeded'));
