-- Migration 026: ai_analyses table ().
--
-- Today, failure_groups.analysis_markdown caches the LATEST analysis
-- on the group row, and ListAIAnalysesUsageByProject + the
-- 200/period quota math both COUNT(*) failure_groups where
-- analyzed_at IS NOT NULL.
--
-- That model has two limitations the founder-side accounting needs
-- to fix:
--   1. Re-running an analysis OVERWRITES the previous one with no
--      cost record of either run. We need a row per call.
--   2. Cost is currently a flat $0.03 estimate. The actual cost
--      depends on model + actual token usage from the Anthropic
--      response (input_tokens, output_tokens).
--
-- ai_analyses is the per-call history table. failure_groups still
-- caches the latest analysis for fast dashboard reads; ai_analyses
-- is the source of truth for accounting + history.
--
-- Schema:
--   analysis_id       Mesedi-issued "ai_<random>" identifier
--   failure_group_id  FK to failure_groups; cascade on group delete
--   project_id        FK to projects; cascade on project delete
--   tenant_id         denormalized from projects.tenant_id at write
--                     time so the per-tenant rollup is one query
--   model_id          e.g. "claude-haiku-4-5"
--   input_tokens      from Anthropic CallResult.InputTokens
--   output_tokens     from Anthropic CallResult.OutputTokens
--   cost_usd          computed at write time from the per-model
--                     pricing registry; denormalized so historical
--                     rows don't change when we update prices
--   generated_at      when the analysis call completed
--   analysis_markdown the analysis output (kept here too for full
--                     history)
--
-- Indexes:
--   (project_id, generated_at DESC) for per-project drilldowns
--   (generated_at DESC) for the cross-tenant admin "all analyses"
--   flat view

CREATE TABLE ai_analyses (
    analysis_id TEXT PRIMARY KEY,
    failure_group_id TEXT NOT NULL REFERENCES failure_groups(group_id) ON DELETE CASCADE,
    project_id TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    tenant_id TEXT,
    model_id TEXT NOT NULL,
    input_tokens INTEGER NOT NULL DEFAULT 0,
    output_tokens INTEGER NOT NULL DEFAULT 0,
    cost_usd REAL NOT NULL DEFAULT 0,
    generated_at TIMESTAMP NOT NULL,
    analysis_markdown TEXT
);

CREATE INDEX idx_ai_analyses_project_generated_at
    ON ai_analyses (project_id, generated_at DESC);

CREATE INDEX idx_ai_analyses_generated_at
    ON ai_analyses (generated_at DESC);

INSERT INTO schema_migrations (version) VALUES (26);
