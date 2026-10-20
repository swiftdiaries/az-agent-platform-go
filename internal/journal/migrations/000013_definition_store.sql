-- Content-addressed, append-only definition sets (the bytes behind a definition digest).
CREATE TABLE agent_definition_sets (
    definition_digest text PRIMARY KEY CHECK (definition_digest ~ '^[0-9a-f]{64}$'),
    created_at timestamptz NOT NULL DEFAULT now(),
    -- first activation time; forward-only activation refuses a set once this is set
    activated_at timestamptz);
CREATE TABLE agent_definition_set_files (
    definition_digest text NOT NULL REFERENCES agent_definition_sets(definition_digest),
    path text NOT NULL CHECK (path <> ''),
    content bytea NOT NULL,
    PRIMARY KEY (definition_digest, path));
