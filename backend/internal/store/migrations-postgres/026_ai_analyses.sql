-- Migration 026: ai_analyses table (#199).
--
-- Postgres twin of migrations/026_ai_analyses.sql. See the SQLite
-- file for design rationale.

CREATE TABLE ai_analyses (
    analysis_id TEXT PRIMARY KEY,
    failure_group_id TEXT NOT NULL REFERENCES failure_groups(group_id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    tenant_id TEXT,
    model_id TEXT NOT NULL,
    input_tokens BIGINT NOT NULL DEFAULT 0,
    output_tokens BIGINT NOT NULL DEFAULT 0,
    cost_usd DOUBLE PRECISION NOT NULL DEFAULT 0,
    generated_at TIMESTAMPTZ NOT NULL,
    analysis_markdown TEXT
);

CREATE INDEX idx_ai_analyses_project_generated_at
    ON ai_analyses (project_id, generated_at DESC);

CREATE INDEX idx_ai_analyses_generated_at
    ON ai_analyses (generated_at DESC);

INSERT INTO schema_migrations (version) VALUES (26);
