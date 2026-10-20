-- Approval markers and binding v2 columns. A legacy approval keeps its v1 binding, which can
-- never equal a v2 digest, so a migrated pending or decided approval is superseded, never dispatched.
ALTER TABLE agent_interactions
    ADD COLUMN marker text,
    ADD COLUMN operation_key text,
    ADD COLUMN name_digest text,
    ADD COLUMN schema_digest text,
    ADD COLUMN product_version text,
    ADD COLUMN catalog_digest text,
    ADD COLUMN active_journeys jsonb;
UPDATE agent_interactions i SET marker = CASE
    WHEN i.state = 'pending' THEN 'requested'
    WHEN i.state = 'expired' THEN 'denied'
    WHEN i.reply->>'decision' = 'deny' THEN 'denied'
    WHEN EXISTS (SELECT 1 FROM agent_operations o WHERE o.call_id = i.artifact->'call'->>'callId') THEN 'dispatched'
    ELSE 'decided' END
WHERE i.kind = 'approval';
ALTER TABLE agent_interactions ADD CONSTRAINT agent_interactions_marker_check
    CHECK (marker IN ('requested','decided','denied','dispatched','superseded'));
ALTER TABLE agent_interactions ADD CONSTRAINT agent_interactions_marker_kind
    CHECK ((kind = 'approval') = (marker IS NOT NULL));
