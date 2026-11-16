-- 063: store each execution's digest when its checkpoint is built.
--
-- Until 2026-11-16 the scheduler computed every execution's digest to
-- build the hour's tenant leaf and then threw it away; exports recomputed
-- it from whatever events existed at export time. One event arriving
-- after sealing (outside review finding E3), or retention deleting the
-- sealed execution (E4), made an honest export report tampering.
--
-- execution_digests holds the anchored value. It deliberately has NO
-- foreign key to executions: retention deletes the execution and its
-- events on schedule, and the digest (hashes, an event count and the
-- customer's execution id) stays, so the chain can vouch for history.
-- Account closure deletes it with the rest of the project (Robert's
-- decision, 2026-11-16). position is the execution's place in the
-- project's interval fold, so the export no longer depends on query
-- order or database collation (E28).
CREATE TABLE IF NOT EXISTS execution_digests (
    execution_id    TEXT    PRIMARY KEY,
    project_id      TEXT    NOT NULL,
    checkpoint_seq  INTEGER NOT NULL REFERENCES checkpoints(seq),
    position        INTEGER NOT NULL,
    root            TEXT    NOT NULL,
    leaf_count      INTEGER NOT NULL,
    algorithm       TEXT    NOT NULL,
    created_at      TIMESTAMP NOT NULL
);

CREATE UNIQUE INDEX IF NOT EXISTS idx_execution_digests_fold
    ON execution_digests(project_id, checkpoint_seq, position);

-- Hours sealed before digests were stored whose recomputed digests no
-- longer fold to the anchored value (records purged or changed since).
-- They are labelled, never re-anchored (Robert's decision, 2026-11-16).
CREATE TABLE IF NOT EXISTS digest_history_gaps (
    checkpoint_seq  INTEGER NOT NULL REFERENCES checkpoints(seq),
    project_id      TEXT    NOT NULL,
    reason          TEXT    NOT NULL,
    detected_at     TIMESTAMP NOT NULL,
    PRIMARY KEY (checkpoint_seq, project_id)
);

-- Events that arrive after their execution's digest was stored are kept
-- but sit outside the anchored record (Robert's decision, 2026-11-16).
ALTER TABLE events ADD COLUMN after_digest INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (63);
