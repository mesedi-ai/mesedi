package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// ListLLMCallCostsInRange: see the interface doc.
func (s *SQLiteStore) ListLLMCallCostsInRange(ctx context.Context, projectID string, from, to time.Time) ([]LLMCallCost, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.timestamp,
		       COALESCE(json_extract(e.payload, '$.model'), ''),
		       COALESCE(json_extract(e.payload, '$.provider'), ''),
		       COALESCE(json_extract(e.payload, '$.input_tokens'), 0),
		       COALESCE(json_extract(e.payload, '$.output_tokens'), 0),
		       COALESCE(json_extract(e.payload, '$.estimated_cost_usd'), 0)
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = ?
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= ?
		  AND e.timestamp < ?
	`, projectID, from.UTC(), to.UTC()) // time.Time bounds, never Format(...)
	if err != nil {
		return nil, fmt.Errorf("list llm call costs: %w", err)
	}
	defer rows.Close()
	var out []LLMCallCost
	for rows.Next() {
		var c LLMCallCost
		var ts string
		var in, outTok sql.NullFloat64
		var est sql.NullFloat64
		if err := rows.Scan(&ts, &c.Model, &c.Provider, &in, &outTok, &est); err != nil {
			return nil, fmt.Errorf("scan llm call cost: %w", err)
		}
		c.At, _ = time.Parse(time.RFC3339, ts)
		c.InputTokens, c.OutputTokens, c.EstimatedCostUSD = int(in.Float64), int(outTok.Float64), est.Float64
		out = append(out, c)
	}
	return out, rows.Err()
}
