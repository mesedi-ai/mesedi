-- 066_tool_first_seen.sql (Postgres twin)
-- See migrations/066_tool_first_seen.sql for the rationale.

CREATE TABLE IF NOT EXISTS tool_first_seen (
    project_id    TEXT NOT NULL,
    tool_name     TEXT NOT NULL,
    kind          TEXT NOT NULL,
    hash          TEXT NOT NULL,
    execution_id  TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    PRIMARY KEY (project_id, tool_name, kind)
);
