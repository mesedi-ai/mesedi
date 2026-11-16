-- 064: record which path anchored each checkpoint, and hold its
-- OpenTimestamps receipts.
--
-- anchor_kind: 'verdifax-attest' for every checkpoint anchored before
-- 2026-11-16, when anchoring went through a third-party service and the
-- log entry was signed by that service's key; 'mesedi-direct' from then
-- on, signed by Mesedi's own key. The verifier checks the signer of
-- direct anchors and states that earlier anchors' signer is not checked.
-- Labelled once here; the anchorer sets it on every new anchor.
ALTER TABLE checkpoints ADD COLUMN anchor_kind TEXT NOT NULL DEFAULT '';
UPDATE checkpoints SET anchor_kind = 'verdifax-attest'
 WHERE log_entry_id <> '' AND anchor_kind = '';

-- checkpoint_timestamps: one OpenTimestamps receipt per checkpoint per
-- calendar server, a second public anchor independent of Sigstore (owner
-- decision D, 21 October 2026). A receipt starts pending and is upgraded
-- in place once the calendar has committed it to a Bitcoin block, so
-- rows may be updated but never deleted: like checkpoints, they are
-- evidence. Kept apart from checkpoints so a slow calendar can never
-- hold up the hourly chain.
CREATE TABLE IF NOT EXISTS checkpoint_timestamps (
    checkpoint_seq  INTEGER   NOT NULL REFERENCES checkpoints(seq),
    calendar_url    TEXT      NOT NULL,
    receipt         BLOB      NOT NULL,
    complete        INTEGER   NOT NULL DEFAULT 0,
    submitted_at    TIMESTAMP NOT NULL,
    upgraded_at     TIMESTAMP,
    PRIMARY KEY (checkpoint_seq, calendar_url)
);

CREATE INDEX IF NOT EXISTS idx_checkpoint_timestamps_pending
    ON checkpoint_timestamps(complete, checkpoint_seq);

CREATE TRIGGER IF NOT EXISTS checkpoint_timestamps_are_permanent
BEFORE DELETE ON checkpoint_timestamps
BEGIN
    SELECT RAISE(ABORT,
        'checkpoint timestamps are permanent evidence, deletion is refused (migration 064)');
END;
