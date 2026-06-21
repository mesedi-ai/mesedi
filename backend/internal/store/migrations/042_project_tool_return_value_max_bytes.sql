-- 042_project_tool_return_value_max_bytes.sql
--
-- Per-project maximum-bytes cap on tool_call return_value payloads
-- that the tool_schema_drift detector will fingerprint. Replaces
-- the original v0.5.0 hardcoded 2048-byte cap in the Python +
-- TypeScript SDKs (#270.a).
--
-- Architecture note: the SDK ships up to ~16 KB (its generous
-- default, raised from the original 2 KB in v0.5.0). The backend
-- applies the per-project cap at FINGERPRINTING time — return
-- values larger than this threshold are excluded from the
-- detector's comparison (treated as inconclusive, same as the
-- SDK's "<truncated>" sentinel). The full event payload is still
-- stored in the events table for dashboard display, bounded only
-- by the 1 MB payload cap from #243.
--
-- Default 8192 bytes (8 KB) covers typical tool returns
-- (objects with ~20 keys, short string values) while still
-- bounding pathological cases. Customers with deeply nested
-- structured returns can raise to 16 KB (= SDK ship cap); customers
-- on the smallest tier may be capped lower in a future #276.b-style
-- tier-aware sweep.

ALTER TABLE projects
    ADD COLUMN tool_return_value_max_bytes INTEGER NOT NULL DEFAULT 8192;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (42);
