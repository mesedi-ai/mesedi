-- Migration 066: the first description and definition seen for each tool.
--
-- THE GAP THIS CLOSES
--
-- The description and definition checks compare a tool against the
-- majority of its own recent calls, which needs ten calls of history
-- and is built from traffic an attacker can shape: a description that
-- changes on call two is invisible, and once the changed text is the
-- majority it becomes the baseline. Pins (migration 062) fix this for
-- tools an operator has approved, but only for those.
--
-- THE FIX
--
-- The first description hash and the first declared-schema hash
-- Mesedi records for a (project, tool) are kept here, and any later
-- call that differs is reported from that call on. A row of kind
-- 'tool' marks that the tool has been seen at all, so the first run
-- that uses a new tool can raise one info-level notice.
--
-- Kept in a table rather than read from the oldest stored event,
-- because events expire under retention: a baseline that moves when
-- old events are deleted would let an old change become the baseline.
-- A first-seen value is never updated. To accept a changed tool, an
-- operator pins the new version, and the pin replaces this baseline.
-- Hashes only, never the description text itself.

CREATE TABLE IF NOT EXISTS tool_first_seen (
    project_id    TEXT NOT NULL,
    tool_name     TEXT NOT NULL,
    kind          TEXT NOT NULL,
    hash          TEXT NOT NULL,
    execution_id  TEXT NOT NULL,
    recorded_at   TEXT NOT NULL,
    PRIMARY KEY (project_id, tool_name, kind)
);
