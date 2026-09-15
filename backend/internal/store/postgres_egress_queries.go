// Postgres twin of sqlite_egress_queries.go: the event queries the
// egress-derived detectors read, in Postgres dialect ($N
// placeholders, payload::jsonb->>'k' extraction).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// GetEnvironmentDeclarationMode: see the SQLite twin for semantics
// (FIRST declaration wins; "" when none was emitted).
func (s *PostgresStore) GetEnvironmentDeclarationMode(
	ctx context.Context,
	executionID string,
) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(payload::jsonb->>'mode', '')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'environment_declaration'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&mode)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get environment declaration mode: %w", err)
	}
	return mode, nil
}

// ListEgressDestinations: see the SQLite twin for semantics
// (DISTINCT destinations, first-seen order, bounded at 200).
func (s *PostgresStore) ListEgressDestinations(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload::jsonb->>'destination' AS dest
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'egress'
		  AND payload::jsonb->>'destination' IS NOT NULL
		GROUP BY dest
		ORDER BY MIN(sequence) ASC
		LIMIT 200
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list egress destinations: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d string
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scan egress destination: %w", err)
		}
		if d != "" {
			out = append(out, d)
		}
	}
	return out, rows.Err()
}

// ListCoordinatingEgressDestinations: see the SQLite twin for
// semantics; one server-side query so the caller never loops store
// calls over destinations.
func (s *PostgresStore) ListCoordinatingEgressDestinations(
	ctx context.Context,
	executionID, projectID string,
	since time.Time,
	threshold int,
) ([]EgressRendezvous, error) {
	if threshold <= 0 {
		threshold = DefaultCovertCoordinationRunThreshold
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT ev.payload::jsonb->>'destination'  AS dest,
		       COUNT(DISTINCT ev.execution_id)    AS runs
		FROM events ev
		JOIN executions x ON x.execution_id = ev.execution_id
		WHERE x.project_id = $1
		  AND ev.event_type = 'egress'
		  AND x.started_at >= $2
		  AND ev.payload::jsonb->>'destination' IN (
			SELECT DISTINCT payload::jsonb->>'destination'
			FROM events
			WHERE execution_id = $3
			  AND event_type = 'egress'
		  )
		GROUP BY dest
		HAVING COUNT(DISTINCT ev.execution_id) >= $4
		ORDER BY runs DESC
		LIMIT 50
	`, projectID, since, executionID, threshold)
	if err != nil {
		return nil, fmt.Errorf("list coordinating egress destinations: %w", err)
	}
	defer rows.Close()
	out := []EgressRendezvous{}
	for rows.Next() {
		var r EgressRendezvous
		if err := rows.Scan(&r.Destination, &r.DistinctRuns); err != nil {
			return nil, fmt.Errorf("scan coordinating destination: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
