// API key persistence: minting, lookup by hash, listing, deletion.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *PostgresStore) CreateAPIKey(ctx context.Context, k *APIKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	scope := k.Scope
	if scope == "" {
		scope = APIKeyScopeCustomer
	}
	source := k.Source
	if source == "" {
		source = APIKeySourceManual
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (key_id, project_id, key_hash, key_prefix, name, created_at, user_id, scope, expires_at, source, role)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`, k.KeyID, k.ProjectID, k.KeyHash, k.KeyPrefix, nullString(k.Name), k.CreatedAt, nullString(k.UserID), scope, k.ExpiresAt, source, nullString(k.Role))
	if err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	k := &APIKey{}
	var name, userID, role sql.NullString
	var lastUsed sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT key_id, project_id, key_hash, key_prefix, name, created_at, last_used_at, user_id, scope, expires_at, source, role
		FROM api_keys WHERE key_hash = $1
	`, keyHash).Scan(&k.KeyID, &k.ProjectID, &k.KeyHash, &k.KeyPrefix, &name, &k.CreatedAt, &lastUsed, &userID, &k.Scope, &k.ExpiresAt, &k.Source, &role)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if name.Valid {
		k.Name = name.String
	}
	if userID.Valid {
		k.UserID = userID.String
	}
	if role.Valid {
		k.Role = role.String
	}
	if lastUsed.Valid {
		t := lastUsed.Time
		k.LastUsedAt = &t
	}
	return k, nil
}

func (s *PostgresStore) TouchAPIKey(ctx context.Context, keyID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET last_used_at = $1 WHERE key_id = $2",
		time.Now().UTC(), keyID,
	)
	return err
}

func (s *PostgresStore) ListAPIKeysForProject(ctx context.Context, projectID string) ([]*APIKey, error) {
	// Filter session-grade keys (sso_login, magic_link) out of the
	// customer-facing listing. See sqlite.go counterpart.
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, project_id, key_prefix, name, created_at, last_used_at, scope, expires_at, source, role
		FROM api_keys
		WHERE project_id = $1
		  AND source NOT IN ('sso_login', 'magic_link')
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()
	return scanPostgresAPIKeyList(rows)
}

// ListAllAPIKeys returns every API key in the system, NEWEST first.
// Admin-only. See sqlite.go counterpart for full docs (session-grade
// keys are filtered out for the same reason there).
func (s *PostgresStore) ListAllAPIKeys(ctx context.Context) ([]*APIKey, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, project_id, key_prefix, name, created_at, last_used_at, scope, expires_at, source, role
		FROM api_keys
		WHERE source NOT IN ('sso_login', 'magic_link')
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, fmt.Errorf("query api_keys (all): %w", err)
	}
	defer rows.Close()
	return scanPostgresAPIKeyList(rows)
}

// scanPostgresAPIKeyList is the postgres-side counterpart of
// scanAPIKeyList in sqlite.go. Centralized so list-by-project and
// list-all share identical scan semantics.
func scanPostgresAPIKeyList(rows *sql.Rows) ([]*APIKey, error) {
	var out []*APIKey
	for rows.Next() {
		var (
			k          APIKey
			lastUsedAt sql.NullTime
			name       sql.NullString
			role       sql.NullString
		)
		if err := rows.Scan(
			&k.KeyID, &k.ProjectID, &k.KeyPrefix,
			&name, &k.CreatedAt, &lastUsedAt, &k.Scope, &k.ExpiresAt, &k.Source, &role,
		); err != nil {
			return nil, err
		}
		if name.Valid {
			k.Name = name.String
		}
		if role.Valid {
			k.Role = role.String
		}
		if lastUsedAt.Valid {
			t := lastUsedAt.Time
			k.LastUsedAt = &t
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// DeleteAPIKeysByUserID hard-deletes every API key whose user_id
// matches. Postgres counterpart to the SQLiteStore method; called by
// HandleRemoveMember to revoke a removed team member's credentials
// . Returns the number of rows deleted.
func (s *PostgresStore) DeleteAPIKeysByUserID(
	ctx context.Context,
	userID string,
) (int, error) {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE user_id = $1`,
		userID,
	)
	if err != nil {
		return 0, fmt.Errorf("delete api_keys by user_id: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

func (s *PostgresStore) DeleteAPIKey(ctx context.Context, keyID, projectID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_keys WHERE key_id = $1 AND project_id = $2`,
		keyID, projectID,
	)
	if err != nil {
		return fmt.Errorf("delete api_key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteAPIKeyByID hard-deletes any API key with no project_id guard.
// Admin-only. See sqlite.go counterpart for full docs.
func (s *PostgresStore) DeleteAPIKeyByID(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM api_keys WHERE key_id = $1`,
		keyID,
	)
	if err != nil {
		return fmt.Errorf("delete api_key: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
