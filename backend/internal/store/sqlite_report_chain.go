package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// SaveReportChainRows: see the interface doc.
func (s *SQLiteStore) SaveReportChainRows(ctx context.Context, rows []ReportChainRow) error {
	if len(rows) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin report_chain save: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	for _, r := range rows {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO report_chain
			   (event_id, execution_id, project_id, prev_hash, hash,
			    payload_sha256, key_id, signature, recorded_at)
			 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
			 ON CONFLICT(event_id) DO NOTHING`,
			r.EventID, r.ExecutionID, r.ProjectID, r.PrevHash, r.Hash,
			r.PayloadSHA256, r.KeyID, r.Signature, now); err != nil {
			return fmt.Errorf("insert report_chain %s: %w", r.EventID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit report_chain save: %w", err)
	}
	return nil
}

// ListReportChainRows: see the interface doc.
func (s *SQLiteStore) ListReportChainRows(ctx context.Context, executionID string) ([]ReportChainRow, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT event_id, execution_id, project_id, prev_hash, hash,
		        payload_sha256, key_id, signature
		 FROM report_chain WHERE execution_id = ? ORDER BY event_id`,
		executionID)
	if err != nil {
		return nil, fmt.Errorf("list report_chain: %w", err)
	}
	defer rows.Close()
	var out []ReportChainRow
	for rows.Next() {
		var r ReportChainRow
		if err := rows.Scan(&r.EventID, &r.ExecutionID, &r.ProjectID, &r.PrevHash,
			&r.Hash, &r.PayloadSHA256, &r.KeyID, &r.Signature); err != nil {
			return nil, fmt.Errorf("scan report_chain: %w", err)
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ExecutionHasSignedReports: see the interface doc.
func (s *SQLiteStore) ExecutionHasSignedReports(ctx context.Context, executionID string) (bool, error) {
	var n int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM report_chain WHERE execution_id = ? AND signature <> ''`,
		executionID).Scan(&n)
	if err != nil {
		return false, fmt.Errorf("count signed reports: %w", err)
	}
	return n > 0, nil
}

// GetReportSigningKey: see the interface doc.
func (s *SQLiteStore) GetReportSigningKey(ctx context.Context, projectID, keyID string) (*ReportSigningKey, error) {
	var k ReportSigningKey
	err := s.db.QueryRowContext(ctx,
		`SELECT key_id, public_key, created_at, created_by, revoked_at
		 FROM report_signing_keys WHERE project_id = ? AND key_id = ?`,
		projectID, keyID).Scan(&k.KeyID, &k.PublicKey, &k.CreatedAt, &k.CreatedBy, &k.RevokedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("get report signing key: %w", err)
	}
	return &k, nil
}

// ListReportSigningKeys: see the interface doc.
func (s *SQLiteStore) ListReportSigningKeys(ctx context.Context, projectID string) ([]*ReportSigningKey, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key_id, public_key, created_at, created_by, revoked_at
		 FROM report_signing_keys WHERE project_id = ? ORDER BY key_id`,
		projectID)
	if err != nil {
		return nil, fmt.Errorf("list report signing keys: %w", err)
	}
	defer rows.Close()
	out := []*ReportSigningKey{}
	for rows.Next() {
		var k ReportSigningKey
		if err := rows.Scan(&k.KeyID, &k.PublicKey, &k.CreatedAt, &k.CreatedBy, &k.RevokedAt); err != nil {
			return nil, fmt.Errorf("scan report signing key: %w", err)
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// InsertReportSigningKey: see the interface doc.
func (s *SQLiteStore) InsertReportSigningKey(ctx context.Context, projectID, keyID, publicKey, createdBy string) error {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO report_signing_keys
		   (project_id, key_id, public_key, created_at, created_by, revoked_at)
		 VALUES (?, ?, ?, ?, ?, '')
		 ON CONFLICT(project_id, key_id) DO NOTHING`,
		projectID, keyID, publicKey, time.Now().UTC().Format(time.RFC3339), createdBy)
	if err != nil {
		return fmt.Errorf("insert report signing key: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("rows affected on report signing key: %w", err)
	} else if n == 0 {
		return ErrReportSigningKeyExists
	}
	return nil
}

// RevokeReportSigningKey: see the interface doc.
func (s *SQLiteStore) RevokeReportSigningKey(ctx context.Context, projectID, keyID string) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE report_signing_keys SET revoked_at = ?
		 WHERE project_id = ? AND key_id = ? AND revoked_at = ''`,
		time.Now().UTC().Format(time.RFC3339), projectID, keyID)
	if err != nil {
		return fmt.Errorf("revoke report signing key: %w", err)
	}
	if n, err := res.RowsAffected(); err != nil {
		return fmt.Errorf("rows affected on revoke: %w", err)
	} else if n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetReportChainStatus: see the interface doc.
func (s *SQLiteStore) SetReportChainStatus(ctx context.Context, row ReportChainStatusRow) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO report_chain_status
		   (execution_id, project_id, status, detail, end_hash, end_key_id, end_signature, checked_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(execution_id) DO UPDATE SET
		   status = excluded.status, detail = excluded.detail,
		   end_hash = excluded.end_hash, end_key_id = excluded.end_key_id,
		   end_signature = excluded.end_signature, checked_at = excluded.checked_at`,
		row.ExecutionID, row.ProjectID, row.Status, strings.TrimSpace(row.Detail),
		row.EndHash, row.EndKeyID, row.EndSignature, time.Now().UTC().Format(time.RFC3339))
	if err != nil {
		return fmt.Errorf("set report chain status: %w", err)
	}
	return nil
}

// GetReportChainStatus: see the interface doc.
func (s *SQLiteStore) GetReportChainStatus(ctx context.Context, executionID string) (ReportChainStatusRow, error) {
	row := ReportChainStatusRow{ExecutionID: executionID}
	err := s.db.QueryRowContext(ctx,
		`SELECT project_id, status, detail, end_hash, end_key_id, end_signature
		 FROM report_chain_status WHERE execution_id = ?`,
		executionID).Scan(&row.ProjectID, &row.Status, &row.Detail, &row.EndHash, &row.EndKeyID, &row.EndSignature)
	if errors.Is(err, sql.ErrNoRows) {
		return ReportChainStatusRow{}, ErrNotFound
	}
	if err != nil {
		return ReportChainStatusRow{}, fmt.Errorf("get report chain status: %w", err)
	}
	return row, nil
}
