-- Migration 034: billing_events table (#261).
--
-- Backs the Security page commitment: "Stripe webhooks for
-- charge.dispute.created, invoice.payment_failed, and subscription
-- state changes feed into our admin dashboard so we can act on
-- fraud signals and dunning cases without polling Stripe."
--
-- Every charge.dispute.created and invoice.payment_failed Stripe
-- webhook lands a row here; the /admin/billing-events page reads
-- from it.
--
-- Schema:
--   event_id            Stripe-issued evt_xxx identifier. Natural
--                        primary key gives us idempotency for free:
--                        if Stripe redelivers the same event, the
--                        INSERT OR IGNORE on the handler turns into
--                        a no-op.
--   project_id          project the event belongs to; resolved by
--                        looking up the Stripe customer ID via the
--                        existing GetProjectByStripeCustomerID
--                        store method. Cascade-delete with the
--                        project so a GDPR purge cleans up here too.
--   stripe_customer_id  cus_xxx, kept for traceability and so the
--                        admin page can show "who" without a join.
--   kind                "stripe_dispute" or "stripe_payment_failed".
--                        Discriminator for the admin UI's per-kind
--                        icon and copy.
--   severity            "high" | "medium" | "low". High for chargebacks
--                        (potential fraud), medium for dunning, low
--                        reserved for future kinds.
--   stripe_object_id    dispute_xxx or in_xxx. Lets ops jump straight
--                        into the Stripe Dashboard via deeplink.
--   amount_cents        amount in the smallest currency unit (Stripe's
--                        native representation).
--   currency            3-letter ISO code, lowercased per Stripe's
--                        convention.
--   detail_json         free-form per-kind context (dispute reason,
--                        invoice attempt_count, next retry timestamp,
--                        etc.). Schema is owned by the handler code,
--                        not the DB, so new fields don't need a
--                        migration.
--   received_at         UTC time the webhook was received.
--   resolved_at         set when ops dismisses the signal.
--   resolved_by         admin actor (email or API key name).
--   resolution_note     human-written explanation.
--
-- Indexes:
--   (project_id, received_at DESC) — list one project's events newest first
--   (received_at) WHERE resolved_at IS NULL — unresolved-only admin scan
--     (SQLite supports partial indexes; the Postgres mirror copies
--     the WHERE clause verbatim).

CREATE TABLE billing_events (
    event_id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    stripe_customer_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    severity TEXT NOT NULL,
    stripe_object_id TEXT NOT NULL,
    amount_cents INTEGER NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT '',
    detail_json TEXT,
    received_at TIMESTAMP NOT NULL,
    resolved_at TIMESTAMP,
    resolved_by TEXT,
    resolution_note TEXT
);

CREATE INDEX idx_billing_events_project_received
    ON billing_events (project_id, received_at DESC);

CREATE INDEX idx_billing_events_unresolved
    ON billing_events (received_at)
    WHERE resolved_at IS NULL;

INSERT INTO schema_migrations (version) VALUES (34);
