package store

// RequestLog + store methods that back the persisted HTTP request
// audit table (#256). See migrations/036_request_log.sql for the
// schema rationale.
//
// One row per authenticated request from a Team-tier project. Hobby
// and Enterprise traffic is not logged here (see migration comment
// for the cost + product rationale). The forensic query for a
// compromise report ("what did key X do between time A and B")
// reads from this table; the nightly retention scheduler prunes
// rows older than 90 days.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// RequestLog is one row of the request_log table.
//
// IPAddress is the client IP as seen by Fly's proxy. Stored as a
// string to accommodate both IPv4 dotted and IPv6 colon forms.
//
// Path is the URL path only; query strings are intentionally not
// captured because they sometimes carry secrets and we do not want
// secrets sitting in the audit log.
type RequestLog struct {
	LogID       int64     `json:"log_id"`
	ProjectID   string    `json:"project_id"`
	APIKeyID    string    `json:"api_key_id"`
	IPAddress   string    `json:"ip_address,omitempty"`
	Method      string    `json:"method"`
	Path        string    `json:"path"`
	StatusCode  int       `json:"status_code"`
	ReceivedAt  time.Time `json:"received_at"`
}

// RequestLogFilter parameterizes ListRequestLog for the #257 admin
// "key recent use" report. APIKeyID is required (the report is
// always per-key). T1 / T2 bound the time window; zero T2 means
// "now".
type RequestLogFilter struct {
	APIKeyID string
	T1       time.Time
	T2       time.Time
	Limit    int
}

// --- SQLite impl --------------------------------------------------

// CreateRequestLog inserts one row. Called from the request-log
// middleware after each authenticated Team-tier request. The
// middleware passes a non-blocking timeout context; the insert is
// expected to complete in single-digit milliseconds.
func (s *SQLiteStore) CreateRequestLog(ctx context.Context, r *RequestLog) error {
	if r == nil {
		return errors.New("nil request log")
	}
	if r.ProjectID == "" || r.APIKeyID == "" || r.Method == "" || r.Path == "" {
		return errors.New("request log missing required field (project_id, api_key_id, method, path)")
	}
	if r.ReceivedAt.IsZero() {
		r.ReceivedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_log (
			project_id, api_key_id, ip_address,
			method, path, status_code, received_at
		)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`,
		r.ProjectID, r.APIKeyID, nullString(r.IPAddress),
		r.Method, r.Path, r.StatusCode, r.ReceivedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert request log: %w", err)
	}
	return nil
}

// ListRequestLog reads request_log rows newest-first for the
// supplied API key + time window. Backs the #257 admin "share
// recent use" report and the customer-side self-serve forensic
// query.
func (s *SQLiteStore) ListRequestLog(
	ctx context.Context, filter RequestLogFilter,
) ([]*RequestLog, error) {
	if filter.APIKeyID == "" {
		return nil, errors.New("ListRequestLog: api_key_id required")
	}
	if filter.Limit <= 0 {
		filter.Limit = 1000
	}
	args := []any{filter.APIKeyID}
	clauses := []string{"api_key_id = ?"}
	if !filter.T1.IsZero() {
		clauses = append(clauses, "received_at >= ?")
		args = append(args, filter.T1.UTC())
	}
	if !filter.T2.IsZero() {
		clauses = append(clauses, "received_at <= ?")
		args = append(args, filter.T2.UTC())
	}
	args = append(args, filter.Limit)
	rows, err := s.db.QueryContext(ctx, `
		SELECT log_id, project_id, api_key_id, ip_address,
		       method, path, status_code, received_at
		FROM request_log
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY received_at DESC
		LIMIT ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("list request log: %w", err)
	}
	defer rows.Close()
	return scanRequestLogRows(rows)
}

// DeleteRequestLogOlderThan purges request_log rows older than the
// cutoff. Called by the daily request_log_retention_scheduler.
// Returns the number of rows deleted.
func (s *SQLiteStore) DeleteRequestLogOlderThan(
	ctx context.Context, cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM request_log
		WHERE received_at < ?
	`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete request log (sqlite): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}

// scanRequestLogRows is the shared row-loop used by every reader.
func scanRequestLogRows(rows *sql.Rows) ([]*RequestLog, error) {
	out := make([]*RequestLog, 0, 16)
	for rows.Next() {
		r := &RequestLog{}
		var ip sql.NullString
		if err := rows.Scan(
			&r.LogID, &r.ProjectID, &r.APIKeyID, &ip,
			&r.Method, &r.Path, &r.StatusCode, &r.ReceivedAt,
		); err != nil {
			return nil, fmt.Errorf("scan request log: %w", err)
		}
		if ip.Valid {
			r.IPAddress = ip.String
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// --- Postgres impl ------------------------------------------------

// CreateRequestLog is the Postgres twin of the SQLite method.
func (s *PostgresStore) CreateRequestLog(ctx context.Context, r *RequestLog) error {
	if r == nil {
		return errors.New("nil request log")
	}
	if r.ProjectID == "" || r.APIKeyID == "" || r.Method == "" || r.Path == "" {
		return errors.New("request log missing required field (project_id, api_key_id, method, path)")
	}
	if r.ReceivedAt.IsZero() {
		r.ReceivedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO request_log (
			project_id, api_key_id, ip_address,
			method, path, status_code, received_at
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
	`,
		r.ProjectID, r.APIKeyID, nullString(r.IPAddress),
		r.Method, r.Path, r.StatusCode, r.ReceivedAt.UTC(),
	)
	if err != nil {
		return fmt.Errorf("insert request log (postgres): %w", err)
	}
	return nil
}

// ListRequestLog is the Postgres twin of the SQLite method.
func (s *PostgresStore) ListRequestLog(
	ctx context.Context, filter RequestLogFilter,
) ([]*RequestLog, error) {
	if filter.APIKeyID == "" {
		return nil, errors.New("ListRequestLog: api_key_id required")
	}
	if filter.Limit <= 0 {
		filter.Limit = 1000
	}
	args := []any{filter.APIKeyID}
	clauses := []string{"api_key_id = $1"}
	if !filter.T1.IsZero() {
		clauses = append(clauses,
			fmt.Sprintf("received_at >= $%d", len(args)+1))
		args = append(args, filter.T1.UTC())
	}
	if !filter.T2.IsZero() {
		clauses = append(clauses,
			fmt.Sprintf("received_at <= $%d", len(args)+1))
		args = append(args, filter.T2.UTC())
	}
	args = append(args, filter.Limit)
	limitPlaceholder := fmt.Sprintf("$%d", len(args))
	rows, err := s.db.QueryContext(ctx, `
		SELECT log_id, project_id, api_key_id, ip_address,
		       method, path, status_code, received_at
		FROM request_log
		WHERE `+strings.Join(clauses, " AND ")+`
		ORDER BY received_at DESC
		LIMIT `+limitPlaceholder, args...)
	if err != nil {
		return nil, fmt.Errorf("list request log (postgres): %w", err)
	}
	defer rows.Close()
	return scanRequestLogRows(rows)
}

// DeleteRequestLogOlderThan is the Postgres twin of the SQLite method.
func (s *PostgresStore) DeleteRequestLogOlderThan(
	ctx context.Context, cutoff time.Time,
) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM request_log
		WHERE received_at < $1
	`, cutoff.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete request log (postgres): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil
	}
	return n, nil
}
