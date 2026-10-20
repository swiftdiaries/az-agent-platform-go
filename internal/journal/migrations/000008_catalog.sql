-- Catalog snapshots: the immutable tools/list capture a conversation is pinned to,
-- the latest capture per (tenant, principal), per-run and per-conversation pins,
-- and the operation policy model (alias, operation key, approval requirement).
CREATE TABLE agent_catalog_snapshots (
    digest text PRIMARY KEY CHECK (length(digest) = 64),
    tenant text NOT NULL,
    principal text NOT NULL,
    product_version text NOT NULL CHECK (product_version <> ''),
    endpoint_id text NOT NULL CHECK (endpoint_id <> ''),
    server_version text NOT NULL,
    descriptors jsonb NOT NULL CHECK (jsonb_typeof(descriptors) = 'array'),
    captured_at timestamptz NOT NULL DEFAULT clock_timestamp()
);
CREATE TABLE agent_catalog_latest (
    tenant text NOT NULL,
    principal text NOT NULL,
    digest text NOT NULL REFERENCES agent_catalog_snapshots(digest),
    updated_at timestamptz NOT NULL DEFAULT clock_timestamp(),
    PRIMARY KEY (tenant, principal)
);

ALTER TABLE agent_conversations ADD COLUMN catalog_digest text REFERENCES agent_catalog_snapshots(digest);
ALTER TABLE agent_conversations ADD COLUMN product_version text;
ALTER TABLE agent_conversations ADD COLUMN catalog_validated_at timestamptz;
ALTER TABLE agent_conversations ADD CONSTRAINT agent_conversations_catalog_check CHECK (
    (catalog_digest IS NULL AND product_version IS NULL AND catalog_validated_at IS NULL)
    OR (catalog_digest IS NOT NULL AND product_version IS NOT NULL AND catalog_validated_at IS NOT NULL));

ALTER TABLE agent_runs ADD COLUMN catalog_digest text REFERENCES agent_catalog_snapshots(digest);

-- The model-visible name becomes the alias; the stable operation key is separate
-- ('' for operations written before this migration).
ALTER TABLE agent_operations RENAME COLUMN name TO alias;
ALTER TABLE agent_operations ADD COLUMN operation_key text NOT NULL DEFAULT '';
ALTER TABLE agent_operations ADD COLUMN requires_approval boolean;
UPDATE agent_operations SET policy = 'idempotent' WHERE policy = 'deduplicated';
UPDATE agent_operations SET requires_approval = (policy <> 'read_only');
ALTER TABLE agent_operations ALTER COLUMN requires_approval SET NOT NULL;
ALTER TABLE agent_operations ADD CONSTRAINT agent_operations_policy_check CHECK (policy IN ('read_only','idempotent','effectful'));
ALTER TABLE agent_operations ADD CONSTRAINT agent_operations_approval_check CHECK (policy = 'read_only' OR requires_approval);

ALTER TABLE agent_events ADD COLUMN detail jsonb CHECK (detail IS NULL OR jsonb_typeof(detail) = 'object');
