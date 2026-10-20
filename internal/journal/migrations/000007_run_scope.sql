-- Run scope: the product version, allowlisted endpoint ID and resolved settings
-- that governed a run, plus the MCP initialize server info. All columns are
-- nullable so runs written before this migration stay valid (NULL = unrecorded).
ALTER TABLE agent_runs ADD COLUMN product_version text;
ALTER TABLE agent_runs ADD COLUMN endpoint_id text;
ALTER TABLE agent_runs ADD COLUMN settings jsonb CHECK (settings IS NULL OR jsonb_typeof(settings) = 'object');
ALTER TABLE agent_runs ADD COLUMN server_info jsonb CHECK (server_info IS NULL OR jsonb_typeof(server_info) = 'object');
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_scope_check CHECK (
    (product_version IS NULL AND endpoint_id IS NULL)
    OR (product_version IS NOT NULL AND endpoint_id IS NOT NULL AND product_version <> '' AND endpoint_id <> ''));

-- A dependency (tenant directory, settings source, MCP endpoint) was unavailable.
ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_state_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_state_check CHECK (state IN
    ('pending','running','completed','failed','auth_required','interrupted','awaiting_input','dependency_unavailable'));
ALTER TABLE agent_commands DROP CONSTRAINT agent_commands_terminal_reason_check;
ALTER TABLE agent_commands ADD CONSTRAINT agent_commands_terminal_reason_check CHECK (terminal_reason IN
    ('','failed','auth_required','interrupted','consumed','dependency_unavailable'));
