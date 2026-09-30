-- Migration 062: approval-time tool contract pins.
--
-- THE GAP THIS CLOSES
--
-- The tool_schema_drift detector's description and definition checks
-- compare a tool's current contract hash against a majority of its
-- own recent history. That baseline needs ten calls before it can
-- form an opinion, and it is built from the very traffic an attacker
-- controls: a tool whose description flips to carry injected
-- instructions on call three is invisible at call three, and from
-- roughly call four onward the poisoned text IS the majority the
-- detector defends. The 2026-09-15 incident radar named this the
-- sharpest open finding and it survived two radars unfixed.
--
-- THE FIX
--
-- A pin is the operator's statement of the approved contract: for a
-- (project, tool) they record the expected hash of the declared
-- input schema (kind 'definition') and/or the normalized description
-- (kind 'description'). When a pin exists, detection for that kind
-- compares against the pin instead of history: no call floor, no
-- majority, fires from the first deviating call. History-based
-- detection continues unchanged for unpinned tools, so the pin is
-- opt-in hardening, not a behavior change for existing customers.
--
-- Hashes only, never the contract text itself: the SDKs already
-- compute description and RFC 8785 canonicalized schema hashes on
-- the wire, so the backend can pin what it already sees without
-- storing customer tool definitions.

CREATE TABLE IF NOT EXISTS tool_contract_pins (
    project_id   TEXT NOT NULL,
    tool_name    TEXT NOT NULL,
    kind         TEXT NOT NULL,
    pinned_hash  TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (project_id, tool_name, kind)
);

-- The detector's hot path looks up both kinds for one tool at once;
-- the primary key's leading columns serve it. This second index
-- serves the dashboard's per-project pin listing.
CREATE INDEX IF NOT EXISTS idx_tool_contract_pins_project
    ON tool_contract_pins(project_id);

INSERT OR IGNORE INTO schema_migrations (version) VALUES (62);
