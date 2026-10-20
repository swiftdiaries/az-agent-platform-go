-- The conversation owns an active journey set (an ordered JSON array of journey IDs).
ALTER TABLE agent_conversations ADD CONSTRAINT agent_conversations_active_journeys_array
    CHECK (jsonb_typeof(active_journeys) = 'array');
-- a run may now have a definition pin without a journey (no explicit target)
ALTER TABLE agent_runs DROP CONSTRAINT agent_runs_check;
ALTER TABLE agent_runs ADD CONSTRAINT agent_runs_journey_requires_definition
    CHECK (journey_id IS NULL OR definition_digest IS NOT NULL);
-- a clarification is not owned by a journey
ALTER TABLE agent_interactions ALTER COLUMN journey_id DROP NOT NULL;
