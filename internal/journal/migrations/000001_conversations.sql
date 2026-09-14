CREATE TABLE chat_threads (
 principal text NOT NULL, external_id text NOT NULL, id text NOT NULL UNIQUE,
 PRIMARY KEY (principal, external_id)
);
CREATE TABLE chat_runs (
 thread_id text NOT NULL REFERENCES chat_threads(id), external_id text NOT NULL,
 id text NOT NULL UNIQUE, communication_id text NOT NULL UNIQUE,
 PRIMARY KEY (thread_id, external_id)
);
CREATE TABLE agent_conversations (
 id text PRIMARY KEY, principal text NOT NULL, sequence bigint NOT NULL DEFAULT 0 CHECK (sequence >= 0)
);
CREATE TABLE agent_commands (
 thread_id text NOT NULL REFERENCES agent_conversations(id), communication_id text NOT NULL,
 run_id text NOT NULL UNIQUE, payload jsonb NOT NULL CHECK (jsonb_typeof(payload) = 'object'),
 PRIMARY KEY (thread_id, communication_id), UNIQUE(thread_id,run_id)
);
CREATE TABLE agent_sessions (
 thread_id text NOT NULL REFERENCES agent_conversations(id), journey_id text NOT NULL,
 definition_digest text NOT NULL CHECK (length(definition_digest) = 64),
 history jsonb NOT NULL DEFAULT '[]' CHECK (jsonb_typeof(history) = 'array'),
 PRIMARY KEY (thread_id,journey_id), UNIQUE(thread_id,journey_id,definition_digest)
);
CREATE TABLE agent_runs (
 id text PRIMARY KEY, thread_id text NOT NULL REFERENCES agent_conversations(id),
 UNIQUE(thread_id,id),
 state text NOT NULL CHECK (state IN ('pending','running','completed','failed','auth_required')),
 journey_id text, definition_digest text, answer text NOT NULL DEFAULT '',
 FOREIGN KEY(thread_id,id) REFERENCES agent_commands(thread_id,run_id),
 FOREIGN KEY(thread_id,journey_id,definition_digest) REFERENCES agent_sessions(thread_id,journey_id,definition_digest),
 CHECK ((journey_id IS NULL) = (definition_digest IS NULL))
);
CREATE UNIQUE INDEX agent_one_live_run ON agent_runs(thread_id) WHERE state IN ('pending','running');
CREATE TABLE agent_events (
 thread_id text NOT NULL REFERENCES agent_conversations(id), sequence bigint NOT NULL CHECK (sequence > 0),
 run_id text NOT NULL, kind text NOT NULL, call_id text NOT NULL DEFAULT '', tool_name text NOT NULL DEFAULT '',
 answer text NOT NULL DEFAULT '', PRIMARY KEY(thread_id,sequence),
 FOREIGN KEY(thread_id,run_id) REFERENCES agent_runs(thread_id,id)
);
