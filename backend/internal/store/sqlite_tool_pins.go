package store

// Approval-time tool contract pins, SQLite implementation
// (migration 062). Matches postgres_tool_pins.go field-for-field.
//
// A pin replaces the history-derived baseline for one half of a
// tool's contract (kind "definition" for the declared input schema
// hash, kind "description" for the normalized description hash).
// The detector compares the current call's hash against the pin
// directly: no call floor, no majority vote, so a contract that
// flips on call three fires on call three instead of becoming its
// own baseline by call four. History-based detection is untouched
// for unpinned (tool, kind) pairs.
//
// The caller (API handler) is responsible for validating kind
// against the two known values and pinned_hash against the expected
// 64-hex-character format; the store accepts any strings so a
// future third kind drops in without a store change.

import (
	"context"
	"fmt"
	"time"
)

// ToolContractPin is one row of the tool_contract_pins table.
type ToolContractPin struct {
	ProjectID  string `json:"-"`
	ToolName   string `json:"tool_name"`
	Kind       string `json:"kind"`
	PinnedHash string `json:"pinned_hash"`
	CreatedAt  string `json:"created_at"`
	UpdatedAt  string `json:"updated_at"`
}

// GetToolContractPins returns kind -> pinned_hash for one tool,
// empty map when nothing is pinned. Single query on the primary
// key's leading columns; the drift detector calls this once per
// tool per execution close.
func (s *SQLiteStore) GetToolContractPins(
	ctx context.Context,
	projectID, toolName string,
) (map[string]string, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT kind, pinned_hash
		 FROM tool_contract_pins
		 WHERE project_id = ? AND tool_name = ?`,
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
// ordered by tool_name then kind, for the dashboard listing.
func (s *SQLiteStore) ListToolContractPins(
	ctx context.Context,
	projectID string,
) ([]*ToolContractPin, error) {
	rows, err := s.db.QueryContext(
		ctx,
		`SELECT project_id, tool_name, kind, pinned_hash, created_at, updated_at
		 FROM tool_contract_pins
		 WHERE project_id = ?
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
// (projectID, toolName, kind), replacing any existing pin for the
// same triple.
func (s *SQLiteStore) UpsertToolContractPin(
	ctx context.Context,
	projectID, toolName, kind, pinnedHash string,
) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := s.db.ExecContext(
		ctx,
		`INSERT INTO tool_contract_pins
		   (project_id, tool_name, kind, pinned_hash, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?)
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
// row matched, so a caller cannot distinguish another project's pin
// from an absent one.
func (s *SQLiteStore) DeleteToolContractPin(
	ctx context.Context,
	projectID, toolName, kind string,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM tool_contract_pins
		 WHERE project_id = ? AND tool_name = ? AND kind = ?`,
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
