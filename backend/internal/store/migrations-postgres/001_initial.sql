-- Mesedi schema v1, Postgres-flavored.
--
-- Translated from migrations/001_initial.sql (the SQLite original).
-- Differences from SQLite:
--   * TIMESTAMP -> TIMESTAMPTZ (timezone-aware)
--   * REAL -> DOUBLE PRECISION
--   * INSERT OR IGNORE INTO ... -> INSERT INTO ... ON CONFLICT DO NOTHING
--   * payload column stays TEXT, not JSONB. Reason: we already store
--     application-side JSON via encoding/json. Adding JSONB would
--     require dual-format SDK / handler awareness for the migration
--     window. The dialect translator in postgres.go casts to ::jsonb at
--     query time where json operators are needed (json_extract -> ->>).

CREATE TABLE IF NOT EXISTS schema_migrations (
    version    INTEGER PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS projects (
    project_id    TEXT PRIMARY KEY,
    name          TEXT NOT NULL,
    owner_user_id TEXT,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS api_keys (
    key_id       TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    key_hash     TEXT NOT NULL UNIQUE,
    key_prefix   TEXT NOT NULL,
    name         TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_api_keys_project ON api_keys(project_id);

CREATE TABLE IF NOT EXISTS executions (
    execution_id        TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    parent_execution_id TEXT REFERENCES executions(execution_id),
    status              TEXT NOT NULL,
    started_at          TIMESTAMPTZ NOT NULL,
    ended_at            TIMESTAMPTZ,
    duration_ms         BIGINT,
    total_tokens_in     BIGINT,
    total_tokens_out    BIGINT,
    estimated_cost_usd  DOUBLE PRECISION,
    input_summary       TEXT,
    output_summary      TEXT,
    crash_signature     TEXT,
    sdk_version         TEXT,
    sdk_language        TEXT,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_executions_project_started ON executions(project_id, started_at DESC);
CREATE INDEX IF NOT EXISTS idx_executions_status ON executions(status);

CREATE TABLE IF NOT EXISTS events (
    event_id     TEXT PRIMARY KEY,
    execution_id TEXT NOT NULL REFERENCES executions(execution_id) ON DELETE CASCADE,
    event_type   TEXT NOT NULL,
    sequence     BIGINT NOT NULL,
    timestamp    TIMESTAMPTZ NOT NULL,
    duration_ms  BIGINT,
    payload      TEXT,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_events_execution_sequence ON events(execution_id, sequence);
CREATE INDEX IF NOT EXISTS idx_events_type_time ON events(event_type, timestamp);

INSERT INTO schema_migrations (version) VALUES (1) ON CONFLICT (version) DO NOTHING;
