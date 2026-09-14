CREATE TABLE agent_definition_current (
 journey_id text PRIMARY KEY,
 definition_digest text NOT NULL CHECK (length(definition_digest) = 64)
);
