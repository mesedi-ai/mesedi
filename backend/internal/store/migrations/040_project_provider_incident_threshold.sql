-- 040_project_provider_incident_threshold.sql
--
-- Per-project minimum-tenants threshold for the provider_incident
-- detector. Was hardcoded at 2 in detectors/provider_incident.go,
-- which made the detector unreachable for 1-tenant customers no
-- matter how many provider errors they hit. This column lets each
-- project tune the threshold to its workload — single-tenant
-- customers set it to 1 (any provider error triggers a group),
-- multi-tenant customers keep the default 2.
--
-- Default value matches the historical hardcoded constant so
-- existing projects see no behavior change on this migration alone.

ALTER TABLE projects
    ADD COLUMN provider_incident_min_tenants INTEGER NOT NULL DEFAULT 2;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (40);
