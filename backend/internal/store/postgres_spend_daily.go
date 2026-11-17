package store

import (
	"context"
	"fmt"
	"time"
)

// ListLLMCallCostsInRange: Postgres twin; see the interface doc.
func (s *PostgresStore) ListLLMCallCostsInRange(ctx context.Context, projectID string, from, to time.Time) ([]LLMCallCost, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT e.timestamp,
		       COALESCE(e.payload::jsonb->>'model', ''),
		       COALESCE(e.payload::jsonb->>'provider', ''),
		       COALESCE((e.payload::jsonb->>'input_tokens')::double precision, 0),
		       COALESCE((e.payload::jsonb->>'output_tokens')::double precision, 0),
		       COALESCE((e.payload::jsonb->>'estimated_cost_usd')::double precision, 0)
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = $1
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= $2
		  AND e.timestamp < $3
	`, projectID, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("list llm call costs: %w", err)
	}
	defer rows.Close()
	var out []LLMCallCost
	for rows.Next() {
		var c LLMCallCost
		var in, outTok, est float64
		if err := rows.Scan(&c.At, &c.Model, &c.Provider, &in, &outTok, &est); err != nil {
			return nil, fmt.Errorf("scan llm call cost: %w", err)
		}
		// UTC, as SQLite returns it: the report buckets calls by UTC day.
		c.At = c.At.UTC()
		c.InputTokens, c.OutputTokens, c.EstimatedCostUSD = int(in), int(outTok), est
		out = append(out, c)
	}
	return out, rows.Err()
}
