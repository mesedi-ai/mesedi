package store

// Tool DEFINITION observations for the definition-drift detector,
// the mcp-pin gap: the incident radar's crawl found seventeen tool
// definitions whose input schema changed while the description
// stayed byte-identical, and Mesedi caught none, because neither
// payload carried the declared schema. SDKs now send
// input_schema_hash, a SHA-256 of the RFC 8785 canonicalized
// declared input schema, on tool_call and mcp_call events where a
// declared schema exists (MCP tools and framework adapters; a plain
// function has no schema to declare and sends nothing).
//
// Same event-extraction pattern as ListToolDescriptions: no new
// table, the hash rides the payload and is read back with
// json_extract. The empty-string lesson from descriptions applies
// identically and is inherited deliberately: rows without the field
// are skipped, never counted, so pre-upgrade traffic cannot form a
// baseline that makes every upgraded client look like drift.

import (
	"context"
	"database/sql"
	"fmt"
)

// ListToolInputSchemaHashes returns recent input_schema_hash values
// from tool_call events for a (project, tool), most recent first,
// excluding the given execution so a baseline is prior runs.
func (s *SQLiteStore) ListToolInputSchemaHashes(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(ev.payload, '$.input_schema_hash')
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'tool_call'
		  AND json_extract(ev.payload, '$.tool_name') = ?
		  AND ev.execution_id != ?
		ORDER BY ev.timestamp DESC
		LIMIT ?
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
