-- 067_report_chain.sql (Postgres twin)
-- See migrations/067_report_chain.sql for the rationale.

CREATE TABLE IF NOT EXISTS report_chain (
    event_id        TEXT PRIMARY KEY,
    execution_id    TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    prev_hash       TEXT NOT NULL,
    hash            TEXT NOT NULL,
    payload_sha256  TEXT NOT NULL,
    key_id          TEXT NOT NULL DEFAULT '',
    signature       TEXT NOT NULL DEFAULT '',
    recorded_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_report_chain_execution
    ON report_chain(execution_id);

CREATE TABLE IF NOT EXISTS report_signing_keys (
    project_id  TEXT NOT NULL,
    key_id      TEXT NOT NULL,
    public_key  TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    created_by  TEXT NOT NULL DEFAULT '',
    revoked_at  TEXT NOT NULL DEFAULT '',
    PRIMARY KEY (project_id, key_id)
);

CREATE TABLE IF NOT EXISTS report_chain_status (
    execution_id  TEXT PRIMARY KEY,
    project_id    TEXT NOT NULL,
    status        TEXT NOT NULL,
    detail        TEXT NOT NULL DEFAULT '',
    end_hash      TEXT NOT NULL DEFAULT '',
    end_key_id    TEXT NOT NULL DEFAULT '',
    end_signature TEXT NOT NULL DEFAULT '',
    checked_at    TEXT NOT NULL
);
