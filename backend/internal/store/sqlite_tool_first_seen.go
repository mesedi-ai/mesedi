package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetToolFirstSeen: see the interface doc.
func (s *SQLiteStore) GetToolFirstSeen(
	ctx context.Context,
	projectID, toolName string,
) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT kind, hash FROM tool_first_seen
		 WHERE project_id = ? AND tool_name = ?`,
		projectID, toolName)
	if err != nil {
		return nil, fmt.Errorf("get tool_first_seen: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var kind, hash string
		if err := rows.Scan(&kind, &hash); err != nil {
			return nil, fmt.Errorf("scan tool_first_seen: %w", err)
		}
		out[kind] = hash
	}
	return out, rows.Err()
}

// RecordToolFirstSeen: see the interface doc. Never updates a row.
func (s *SQLiteStore) RecordToolFirstSeen(
	ctx context.Context,
	projectID, toolName, kind, hash, executionID string,
) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO tool_first_seen
		   (project_id, tool_name, kind, hash, execution_id, recorded_at)
		 VALUES (?, ?, ?, ?, ?, ?)
		 ON CONFLICT(project_id, tool_name, kind) DO NOTHING`,
		projectID, toolName, kind, hash, executionID,
		time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return false, fmt.Errorf("record tool_first_seen: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected on tool_first_seen: %w", err)
	}
	return n == 1, nil
}

// EarliestToolContract: see the interface doc.
func (s *SQLiteStore) EarliestToolContract(
	ctx context.Context,
	projectID, toolName string,
) (ToolContract, error) {
	var out ToolContract
	first := func(field string, value, execID *string) error {
		where := ""
		if field != "" {
			where = ` AND COALESCE(json_extract(ev.payload, '$.` + field + `'), '') <> ''`
		}
		col := "''"
		if field != "" {
			col = `json_extract(ev.payload, '$.` + field + `')`
		}
		err := s.db.QueryRowContext(ctx, `
			SELECT ev.execution_id, `+col+`
			FROM tool_events ev
			JOIN executions ex ON ex.execution_id = ev.execution_id
			WHERE ex.project_id = ?
			  AND json_extract(ev.payload, '$.tool_name') = ?`+where+`
			ORDER BY ev.timestamp ASC, ev.sequence ASC
			LIMIT 1`, projectID, toolName).Scan(execID, value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	var ignored string
	if err := first("", &ignored, &out.ExecutionID); err != nil {
		return ToolContract{}, fmt.Errorf("earliest tool call: %w", err)
	}
	if err := first("tool_description", &out.Description, &out.DescriptionExecutionID); err != nil {
		return ToolContract{}, fmt.Errorf("earliest tool description: %w", err)
	}
	if err := first("input_schema_hash", &out.SchemaHash, &out.SchemaExecutionID); err != nil {
		return ToolContract{}, fmt.Errorf("earliest tool schema hash: %w", err)
	}
	return out, nil
}

// LatestToolContractInExecution: see the interface doc.
func (s *SQLiteStore) LatestToolContractInExecution(
	ctx context.Context,
	executionID, toolName string,
) (ToolContract, error) {
	out := ToolContract{DescriptionExecutionID: executionID, SchemaExecutionID: executionID}
	last := func(field string, value *string) error {
		err := s.db.QueryRowContext(ctx, `
			SELECT json_extract(payload, '$.`+field+`')
			FROM tool_events
			WHERE execution_id = ?
			  AND json_extract(payload, '$.tool_name') = ?
			  AND COALESCE(json_extract(payload, '$.`+field+`'), '') <> ''
			ORDER BY sequence DESC
			LIMIT 1`, executionID, toolName).Scan(value)
		if errors.Is(err, sql.ErrNoRows) {
			return nil
		}
		return err
	}
	if err := last("tool_description", &out.Description); err != nil {
		return ToolContract{}, fmt.Errorf("latest tool description: %w", err)
	}
	if err := last("input_schema_hash", &out.SchemaHash); err != nil {
		return ToolContract{}, fmt.Errorf("latest tool schema hash: %w", err)
	}
	return out, nil
}
