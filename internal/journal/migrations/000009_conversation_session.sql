-- One session per conversation: the history moves onto agent_conversations and the
-- per-journey session table is archived. Runs and interactions keep their
-- journey_id/definition_digest values but no longer reference a session row.
-- history keeps agent_sessions.history's array invariant.
ALTER TABLE agent_conversations ADD COLUMN history jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(history) = 'array'),
    ADD COLUMN definition_digest text, ADD COLUMN active_journeys jsonb NOT NULL DEFAULT '[]';
-- latest *executing* run with a journey, per thread; reply/steering rows are not executing runs
UPDATE agent_conversations c SET history = s.history
FROM (SELECT DISTINCT ON (r.thread_id) r.thread_id, r.journey_id
      FROM agent_runs r JOIN agent_commands k ON k.execution_run_id = r.id
      WHERE r.journey_id IS NOT NULL
      ORDER BY r.thread_id, k.ordinal DESC) latest
JOIN agent_sessions s ON (s.thread_id, s.journey_id) = (latest.thread_id, latest.journey_id)
WHERE c.id = latest.thread_id;
ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_thread_id_journey_id_definition_digest_fkey;
ALTER TABLE agent_interactions DROP CONSTRAINT agent_interactions_thread_id_journey_id_definition_digest_fkey;
ALTER TABLE agent_sessions RENAME TO agent_sessions_archived;
ALTER TABLE agent_definition_current RENAME TO agent_definition_current_archived;
ALTER INDEX agent_definition_current_pkey RENAME TO agent_definition_current_archived_pkey;
CREATE TABLE agent_definition_current (
    singleton boolean PRIMARY KEY DEFAULT true CHECK (singleton),
    definition_digest text NOT NULL CHECK (length(definition_digest) = 64));
ALTER TABLE agent_operations ADD COLUMN journey_id text;
