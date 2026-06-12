-- Migration 025: anthropic_credit_snapshots table (#198).
--
-- Postgres twin of migrations/025_anthropic_credit_snapshots.sql.
-- See SQLite file for the design rationale.

CREATE TABLE anthropic_credit_snapshots (
    snapshot_id TEXT PRIMARY KEY,
    balance_usd DOUBLE PRECISION NOT NULL,
    snapshotted_at TIMESTAMPTZ NOT NULL,
    actor_email TEXT,
    note TEXT
);

CREATE INDEX idx_anthropic_credit_snapshotted_at
    ON anthropic_credit_snapshots (snapshotted_at DESC);

INSERT INTO schema_migrations (version) VALUES (25);
