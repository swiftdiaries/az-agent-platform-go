ALTER TABLE agent_commands ADD COLUMN terminal_reason text NOT NULL DEFAULT ''
    CHECK (terminal_reason IN ('','failed','auth_required','interrupted'));
ALTER TABLE agent_commands ADD CHECK (NOT included OR terminal_reason='');
ALTER TABLE agent_events ADD COLUMN reason text NOT NULL DEFAULT '';
-- Existing unincluded commands on terminal runs cannot execute again. Preserve
-- inclusion truth while making their terminal disposition explicit in snapshots.
UPDATE agent_commands c SET terminal_reason=r.state FROM agent_runs r
WHERE r.id=c.execution_run_id AND NOT c.included
    AND r.state IN ('failed','auth_required','interrupted');
