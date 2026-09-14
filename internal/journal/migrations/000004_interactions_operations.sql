ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_state_check;
ALTER TABLE agent_runs ADD CHECK (state IN ('pending','running','completed','failed','auth_required','interrupted','awaiting_input'));
CREATE TABLE agent_interactions (
 id text PRIMARY KEY, thread_id text NOT NULL, run_id text NOT NULL,
 journey_id text NOT NULL, definition_digest text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('clarification','approval')),
 artifact jsonb NOT NULL, checkpoint jsonb NOT NULL, history jsonb NOT NULL,
 expires_at timestamptz NOT NULL, state text NOT NULL DEFAULT 'pending' CHECK(state IN ('pending','consumed','expired')),
 continuation_run_id text, reply jsonb,
 FOREIGN KEY(thread_id,run_id) REFERENCES agent_runs(thread_id,id),
 FOREIGN KEY(thread_id,journey_id,definition_digest) REFERENCES agent_sessions(thread_id,journey_id,definition_digest)
);
CREATE UNIQUE INDEX agent_one_pending_interaction ON agent_interactions(thread_id) WHERE state='pending';
CREATE TABLE agent_operations (
 call_id text PRIMARY KEY, thread_id text NOT NULL, run_id text NOT NULL, name text NOT NULL,
 arguments text NOT NULL, binding text NOT NULL, policy text NOT NULL,
 outcome text NOT NULL CHECK(outcome IN ('dispatching','completed','rejected','auth_required','outcome_unknown')),
 FOREIGN KEY(thread_id,run_id) REFERENCES agent_runs(thread_id,id)
);
CREATE TABLE agent_attempts (
 call_id text NOT NULL REFERENCES agent_operations(call_id), attempt integer NOT NULL CHECK(attempt>0),
 outcome text NOT NULL, PRIMARY KEY(call_id,attempt)
);
