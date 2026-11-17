-- Migration 067: report chaining and optional signing.
--
-- THE GAP THIS CLOSES
--
-- Anyone holding a project's key, the agent included, could send
-- invented reports, and a report could be removed or slipped into a
-- run after the fact; only gaps and duplicate sequence numbers were
-- caught. Each report now carries the fingerprint of the report sent
-- before it in the same run (attest.ReportChainHash), and a team can
-- sign reports with a key held by a reporting process the agent cannot
-- reach.
--
-- report_chain keeps, per chained report, what the server computed on
-- arrival: the fingerprint, the claimed predecessor and the SHA-256 of
-- the payload exactly as received. The last one is kept because secret
-- redaction rewrites stored payloads, so the stored bytes alone cannot
-- reproduce a fingerprint; with it, a checker can tell "redacted by
-- Mesedi on arrival" from "changed".
--
-- report_signing_keys holds Ed25519 public keys, registered only by a
-- signed-in admin, never by an API key (an agent may hold one). A key
-- is revoked, not deleted, so reports it signed before revocation stay
-- checkable.
--
-- report_chain_status records each run's verdict at completion:
-- unchained, chained, signed or broken, with the end the SDK declared
-- (the last report's fingerprint, and its signature for a signed run),
-- so a run whose last reports went missing is caught too.

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
