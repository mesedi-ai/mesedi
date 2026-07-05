-- 039_execution_failure_group_membership.sql (Postgres twin)
--
-- See migrations/039_execution_failure_group_membership.sql for the
-- design rationale. This file is the Postgres-syntax twin: BOOLEAN
-- instead of INTEGER for is_primary, ON CONFLICT DO NOTHING instead
-- of INSERT OR IGNORE, and TIMESTAMP WITH TIME ZONE for the
-- classified_at column to match the rest of the Postgres schema.

CREATE TABLE execution_failure_groups (
    execution_id  TEXT NOT NULL,
    group_id      TEXT NOT NULL,
    is_primary    BOOLEAN NOT NULL DEFAULT FALSE,
    classified_at TIMESTAMP WITH TIME ZONE NOT NULL,
    PRIMARY KEY (execution_id, group_id),
    FOREIGN KEY (execution_id) REFERENCES executions(execution_id) ON DELETE CASCADE,
    FOREIGN KEY (group_id) REFERENCES failure_groups(group_id) ON DELETE CASCADE
);

CREATE INDEX idx_execution_failure_groups_execution
    ON execution_failure_groups (execution_id);

CREATE INDEX idx_execution_failure_groups_group
    ON execution_failure_groups (group_id);

INSERT INTO execution_failure_groups (
    execution_id, group_id, is_primary, classified_at
)
SELECT
    execution_id,
    failure_group_id,
    TRUE,
    COALESCE(ended_at, started_at)
FROM executions
WHERE failure_group_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (39) ON CONFLICT DO NOTHING;
