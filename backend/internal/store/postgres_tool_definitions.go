package store

// Postgres twin of sqlite_tool_definitions.go; see that file for the
// full rationale (the mcp-pin definition-drift gap).

import (
	"context"
	"database/sql"
	"fmt"
)

// ListToolInputSchemaHashes is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) ListToolInputSchemaHashes(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ev.payload->>'input_schema_hash'
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = $1
		  AND ev.event_type = 'tool_call'
		  AND ev.payload->>'tool_name' = $2
		  AND ev.execution_id != $3
		ORDER BY ev.timestamp DESC
		LIMIT $4
	`, projectID, toolName, excludeExecutionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tool input schema hashes: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var h sql.NullString
		if err := rows.Scan(&h); err != nil {
			return nil, fmt.Errorf("scan input schema hash: %w", err)
		}
		if !h.Valid || h.String == "" {
			continue
		}
		out = append(out, h.String)
	}
	return out, rows.Err()
}
