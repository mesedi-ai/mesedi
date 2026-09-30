-- 062_tool_contract_pins.sql (Postgres twin)
-- See migrations/062_tool_contract_pins.sql for the rationale.

CREATE TABLE IF NOT EXISTS tool_contract_pins (
    project_id   TEXT NOT NULL,
    tool_name    TEXT NOT NULL,
    kind         TEXT NOT NULL,
    pinned_hash  TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (project_id, tool_name, kind)
);

CREATE INDEX IF NOT EXISTS idx_tool_contract_pins_project
    ON tool_contract_pins(project_id);

INSERT INTO schema_migrations (version) VALUES (62) ON CONFLICT DO NOTHING;
