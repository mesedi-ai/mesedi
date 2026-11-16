-- 064: record which path anchored each checkpoint, and hold its
-- OpenTimestamps receipts. See the SQLite twin for the reasoning.
ALTER TABLE checkpoints ADD COLUMN IF NOT EXISTS anchor_kind TEXT NOT NULL DEFAULT '';
UPDATE checkpoints SET anchor_kind = 'verdifax-attest'
 WHERE log_entry_id <> '' AND anchor_kind = '';

CREATE TABLE IF NOT EXISTS checkpoint_timestamps (
    checkpoint_seq  BIGINT      NOT NULL REFERENCES checkpoints(seq),
    calendar_url    TEXT        NOT NULL,
    receipt         BYTEA       NOT NULL,
    complete        BOOLEAN     NOT NULL DEFAULT FALSE,
    submitted_at    TIMESTAMPTZ NOT NULL,
    upgraded_at     TIMESTAMPTZ,
    PRIMARY KEY (checkpoint_seq, calendar_url)
);

CREATE INDEX IF NOT EXISTS idx_checkpoint_timestamps_pending
    ON checkpoint_timestamps(complete, checkpoint_seq);

DROP TRIGGER IF EXISTS checkpoint_timestamps_are_permanent ON checkpoint_timestamps;
CREATE TRIGGER checkpoint_timestamps_are_permanent
    BEFORE DELETE ON checkpoint_timestamps
    FOR EACH ROW EXECUTE FUNCTION refuse_checkpoint_delete();
