// Event queries the egress-derived detectors read: the declared
// environment mode, an execution's egress destinations, and the
// cross-execution rendezvous counter for covert coordination.
// Added 2026-09-15 when the egress-visibility deferral was reopened.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// DefaultCovertCoordinationRunThreshold is how many DISTINCT recent
// executions must share one egress destination before the covert
// coordination counter fires. Three, not two: two runs sharing an
// API endpoint is routine software, three within the window is a
// pattern worth a look. Modeled on the provider-incident tenant
// counter, which this detector is nearly a copy of by design.
const DefaultCovertCoordinationRunThreshold = 3

// DefaultCovertCoordinationWindow bounds how far back the distinct
// run counter looks. A day: long enough that slow coordination shows
// up, short enough that a stable shared dependency ages out of the
// count instead of firing forever.
const DefaultCovertCoordinationWindow = 24 * time.Hour

// GetEnvironmentDeclarationMode returns the mode string of the
// FIRST environment_declaration event on the execution, or "" when
// no declaration was emitted. First, not last: the declaration is
// the operator's claim at run start, and letting a later event
// override it would let a confused run rewrite its own boundary.
func (s *SQLiteStore) GetEnvironmentDeclarationMode(
	ctx context.Context,
	executionID string,
) (string, error) {
	var mode string
	err := s.db.QueryRowContext(ctx, `
		SELECT COALESCE(json_extract(payload, '$.mode'), '')
		FROM events
		WHERE execution_id = ?
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

// ListEgressDestinations returns the DISTINCT destinations of every
// egress event on the execution, in first-seen order. Bounded by
// LIMIT 200: an execution reporting more distinct destinations than
// that is its own kind of signal, and the detectors only need the
// set, not the volume.
func (s *SQLiteStore) ListEgressDestinations(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(payload, '$.destination') AS dest
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'egress'
		  AND json_extract(payload, '$.destination') IS NOT NULL
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

// EgressRendezvous is one egress destination that more distinct
// recent executions than the threshold have contacted.
type EgressRendezvous struct {
	Destination  string
	DistinctRuns int
}

// ListCoordinatingEgressDestinations returns, for the given
// execution's own egress destinations, those contacted by at least
// `threshold` DISTINCT executions of the project since `since`,
// with the distinct-run count. One query on purpose: the per-
// destination counting happens server-side so the caller never
// loops store calls over destinations.
func (s *SQLiteStore) ListCoordinatingEgressDestinations(
	ctx context.Context,
	executionID, projectID string,
	since time.Time,
	threshold int,
) ([]EgressRendezvous, error) {
	if threshold <= 0 {
		threshold = DefaultCovertCoordinationRunThreshold
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(ev.payload, '$.destination') AS dest,
		       COUNT(DISTINCT ev.execution_id)           AS runs
		FROM events ev
		JOIN executions x ON x.execution_id = ev.execution_id
		WHERE x.project_id = ?
		  AND ev.event_type = 'egress'
		  AND x.started_at >= ?
		  AND json_extract(ev.payload, '$.destination') IN (
			SELECT DISTINCT json_extract(payload, '$.destination')
			FROM events
			WHERE execution_id = ?
			  AND event_type = 'egress'
		  )
		GROUP BY dest
		HAVING COUNT(DISTINCT ev.execution_id) >= ?
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
