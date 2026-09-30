package store

// Approval-time tool contract pins, Postgres implementation
// (migration 062). Twin of sqlite_tool_pins.go: same shape, same
// contract, $1/$2 placeholders instead of ?, and the struct and
// rationale live in the SQLite file.

import (
	"context"
	"fmt"
	"time"
)

// GetToolContractPins returns kind -> pinned_hash for one tool,
// empty map when nothing is pinned.
func (s *PostgresStore) GetToolContractPins(
	ctx context.Context,
	projectID, toolName string,
) (map[string]string, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT kind, pinned_hash
		 FROM tool_contract_pins
		 WHERE project_id = $1 AND tool_name = $2`,
		projectID, toolName,
	)
	if err != nil {
		return nil, fmt.Errorf("get tool_contract_pins: %w", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var kind, hash string
		if err := rows.Scan(&kind, &hash); err != nil {
			return nil, fmt.Errorf("scan tool_contract_pin: %w", err)
		}
		out[kind] = hash
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err on tool_contract_pins scan: %w", err)
	}
	return out, nil
}

// ListToolContractPins returns every pin row for the project,
// ordered by tool_name then kind.
func (s *PostgresStore) ListToolContractPins(
	ctx context.Context,
	projectID string,
) ([]*ToolContractPin, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT project_id, tool_name, kind, pinned_hash, created_at, updated_at
		 FROM tool_contract_pins
		 WHERE project_id = $1
		 ORDER BY tool_name ASC, kind ASC`,
		projectID,
	)
	if err != nil {
		return nil, fmt.Errorf("list tool_contract_pins: %w", err)
	}
	defer rows.Close()
	var out []*ToolContractPin
	for rows.Next() {
		p := &ToolContractPin{}
		if err := rows.Scan(
			&p.ProjectID, &p.ToolName, &p.Kind,
			&p.PinnedHash, &p.CreatedAt, &p.UpdatedAt,
		); err != nil {
			return nil, fmt.Errorf("scan tool_contract_pin row: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows.Err on tool_contract_pins list: %w", err)
	}
	return out, nil
}

// UpsertToolContractPin records the approved contract hash for
// (projectID, toolName, kind).
func (s *PostgresStore) UpsertToolContractPin(
	ctx context.Context,
	projectID, toolName, kind, pinnedHash string,
) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO tool_contract_pins
		   (project_id, tool_name, kind, pinned_hash, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6)
		 ON CONFLICT (project_id, tool_name, kind) DO UPDATE SET
		   pinned_hash = excluded.pinned_hash,
		   updated_at  = excluded.updated_at`,
		projectID, toolName, kind, pinnedHash, now, now,
	)
	if err != nil {
		return fmt.Errorf("upsert tool_contract_pin: %w", err)
	}
	return nil
}

// DeleteToolContractPin removes a pin. Returns ErrNotFound when no
// row matched.
func (s *PostgresStore) DeleteToolContractPin(
	ctx context.Context,
	projectID, toolName, kind string,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM tool_contract_pins
		 WHERE project_id = $1 AND tool_name = $2 AND kind = $3`,
		projectID, toolName, kind,
	)
	if err != nil {
		return fmt.Errorf("delete tool_contract_pin: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("delete tool_contract_pin rows-affected: %w", err)
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
