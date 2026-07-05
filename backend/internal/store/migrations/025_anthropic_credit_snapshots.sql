-- Migration 025: anthropic_credit_snapshots table ().
--
-- Anthropic's documented Admin API exposes per-period spend via the
-- Cost Report endpoint but does NOT expose the remaining credit
-- balance shown in the Console sidebar. To surface that number on
-- Mesedi's admin dashboard we accept manual entry: the founder
-- pastes the current balance from the Console; we store one row
-- per snapshot with a timestamp; the UI shows "remaining $X.XX
-- (as of $TIME)" alongside the burn rate we compute programmatically
-- from the Cost Report API.
--
-- Schema:
--   snapshot_id     unique id, format "credit_<random>"
--   balance_usd     the dollar amount the founder pasted
--   snapshotted_at  when the snapshot was recorded
--   actor_email     who recorded it (admin actor identity for audit)
--   note            optional free-text reason (e.g. "after top-up
--                    on Jun 11")
--
-- Performance: the only common query is "give me the most recent
-- snapshot" so the (snapshotted_at DESC) ordering on every read is
-- cheap without an index on a single-digit-row-count table.

CREATE TABLE anthropic_credit_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    balance_usd REAL NOT NULL,
    snapshotted_at TIMESTAMP NOT NULL,
    actor_email TEXT,
    note TEXT
);

CREATE INDEX idx_anthropic_credit_snapshotted_at
    ON anthropic_credit_snapshots (snapshotted_at DESC);

INSERT INTO schema_migrations (version) VALUES (25);
