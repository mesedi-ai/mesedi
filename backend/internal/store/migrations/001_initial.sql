-- Mesedi schema v1 — Phase 1.5 persistence.
--
-- Tables defined here match §6 of the detailed concept document
-- (mesedi/concept idea/DETAILED_CONCEPT.md §6 Data model).
--
-- Designed to be Postgres-compatible (no SQLite-specific syntax) so the
-- same migration can run against either backend. SQLite-specific features
-- like WAL mode are configured at connection time, not in the schema.

-- Schema version tracking. Bumped by each migration file applied.
CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- A project is the top-level container for one customer's agent telemetry.
-- One developer can have multiple projects (e.g., one per agent they
-- instrument, or one per environment).
CREATE TABLE IF NOT EXISTS projects (
    project_id    TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    owner_user_id TEXT,
    created_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

-- API keys authenticate SDK clients against a specific project. The raw
-- key is shown once at mint time and never stored — only the SHA-256 hash
-- is persisted. key_prefix is a non-secret display string (first ~12
-- characters) for the developer to identify keys without revealing them.
CREATE TABLE IF NOT EXISTS api_keys (
    key_id       TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    key_hash     TEXT NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    name         TEXT,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_api_keys_project ON api_keys(project_id);

-- Executions are the root records for one agent invocation. Status moves
-- from "started" to a terminal state (completed / crashed / halted /
-- timeout / validation_failed) exactly once via PATCH /executions/{id}.
CREATE TABLE IF NOT EXISTS executions (
    execution_id        TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    parent_execution_id TEXT REFERENCES executions(execution_id),
    status              TEXT NOT NULL,
    started_at          TIMESTAMP NOT NULL,
    ended_at            TIMESTAMP,
    duration_ms         INTEGER,
    total_tokens_in     INTEGER,
    total_tokens_out    INTEGER,
    estimated_cost_usd  REAL,
    input_summary       TEXT,
    output_summary      TEXT,
    crash_signature     TEXT,
    sdk_version         TEXT,
    sdk_language        TEXT,
    created_at          TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_executions_project_started ON executions(project_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_executions_status ON executions(status);

-- Events are individual steps within an execution. The polymorphic
-- payload (LLM call, tool call, checkpoint, exception, validator result,
-- drift signal, injection alert) is stored as JSON text. In Postgres
-- this becomes jsonb with operator-based indexing; in SQLite it's just
-- TEXT but still parseable via json_extract().
CREATE TABLE IF NOT EXISTS events (
    event_id     TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL REFERENCES executions(execution_id) ON DELETE CASCADE,
    event_type   TEXT NOT NULL,
    sequence     INTEGER NOT NULL,
    timestamp    TIMESTAMP NOT NULL,
    duration_ms  INTEGER,
    payload      TEXT,
    created_at   TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_events_execution_sequence ON events(execution_id, sequence);
CREATE INDEX IF NOT EXISTS idx_events_type_time ON events(event_type, timestamp);

-- Mark this migration as applied.
INSERT OR IGNORE INTO schema_migrations (version) VALUES (1);
