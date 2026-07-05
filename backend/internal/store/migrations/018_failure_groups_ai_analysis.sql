-- Migration 018: failure_groups AI-assisted root-cause analysis.
--
-- Mesedi . Adds three columns to failure_groups so a LangSmith-
-- Engine-style "explain the likely cause + propose two fixes"
-- analysis can be generated on demand, cached on the row, and
-- served back to the dashboard without re-calling the LLM on every
-- page view.
--
-- Columns:
--
--   * analysis_markdown   the LLM's response, rendered as Markdown
--                         on the dashboard. NULL when no analysis
--                         has been generated yet.
--
--   * analyzed_at         timestamp the analysis was produced.
--                         Cache-invalidation key: the dashboard
--                         shows a "Regenerate" button when this is
--                         older than 24 hours or when new affected
--                         executions have landed since.
--
--   * analysis_model      identifier of the model that produced the
--                         analysis (e.g. "claude-haiku-4-5"). Lets
--                         operators trace which model wrote which
--                         analysis, and gives the dashboard a
--                         provenance string to render.
--
-- All three columns are nullable; existing rows are untouched and
-- show up in the dashboard with the "Generate analysis" call-to-
-- action surfaced instead of a rendered analysis card.
--
-- No index needed; analyses are read by group_id (already the
-- primary key) and never queried as a separate column.

ALTER TABLE failure_groups ADD COLUMN analysis_markdown TEXT;
ALTER TABLE failure_groups ADD COLUMN analyzed_at TIMESTAMP;
ALTER TABLE failure_groups ADD COLUMN analysis_model TEXT;

INSERT INTO schema_migrations (version) VALUES (18);
