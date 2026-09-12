// API key persistence: minting, lookup by hash, listing, deletion.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

func (s *SQLiteStore) CreateAPIKey(ctx context.Context, k *APIKey) error {
	if k.CreatedAt.IsZero() {
		k.CreatedAt = time.Now().UTC()
	}
	scope := k.Scope
	if scope == "" {
		scope = APIKeyScopeCustomer
	}
	// source defaults to "manual" (long-lived, customer-visible) to
	// match the migration 028 default. Callers that need a different
	// classification (signup flow, /signin from OAuth callback, magic
	// link verify) set k.Source explicitly before calling this.
	source := k.Source
	if source == "" {
		source = APIKeySourceManual
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO api_keys (key_id, project_id, key_hash, key_prefix, name, created_at, user_id, scope, expires_at, source, role)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`, k.KeyID, k.ProjectID, k.KeyHash, k.KeyPrefix, nullString(k.Name), k.CreatedAt, nullString(k.UserID), scope, k.ExpiresAt, source, nullString(k.Role))
	if err != nil {
		return fmt.Errorf("insert api_key: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error) {
	k := &APIKey{}
	var name, userID sql.NullString
	var lastUsed sql.NullTime
	var role sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT key_id, project_id, key_hash, key_prefix, name, created_at, last_used_at, user_id, scope, expires_at, source, role
		FROM api_keys WHERE key_hash = ?
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

func (s *SQLiteStore) TouchAPIKey(ctx context.Context, keyID string) error {
	_, err := s.db.ExecContext(ctx,
		"UPDATE api_keys SET last_used_at = ? WHERE key_id = ?",
		time.Now().UTC(), keyID,
	)
	return err
}

// ListAPIKeysForProject returns every API key bound to the given
// project, NEWEST first. key_hash is intentionally omitted from the
// returned structs, that field is never serialized to clients or
// callers; only the hash on the server's authoritative copy ever
// touches the auth path.
func (s *SQLiteStore) ListAPIKeysForProject(
	ctx context.Context,
	projectID string,
) ([]*APIKey, error) {
	// Filter session-grade keys (sso_login, magic_link) out of the
	// customer-facing listing. They are minted invisibly by the SSO
	// callback / magic-link verify routes and the customer never
	// consciously created them; surfacing them in /admin/api-keys
	// would clutter the list and the "revoke" affordance would
	// silently log the customer out.
	rows, err := s.db.QueryContext(ctx, `
		SELECT key_id, project_id, key_prefix, name, created_at, last_used_at, scope, expires_at, source, role
		FROM api_keys
		WHERE project_id = ?
		  AND source NOT IN ('sso_login', 'magic_link')
		ORDER BY created_at DESC
	`, projectID)
	if err != nil {
		return nil, fmt.Errorf("query api_keys: %w", err)
	}
	defer rows.Close()
	return scanAPIKeyList(rows)
}

// ListAllAPIKeys returns every API key in the system, NEWEST first.
// Admin-only: used by the /admin/api-keys page to surface keys across
// every project (including the synthetic _admin project that holds
// admin-scope keys). key_hash is intentionally not selected.
//
// Session-grade keys (sso_login, magic_link) are excluded from this
// listing for the same reason ListAPIKeysForProject excludes them:
// they are invisible session credentials, not credentials the
// operator should be reasoning about in the admin UI.
func (s *SQLiteStore) ListAllAPIKeys(ctx context.Context) ([]*APIKey, error) {
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
	return scanAPIKeyList(rows)
}

// scanAPIKeyList consumes a *sql.Rows produced by one of the
// list-api_keys queries and returns the materialized slice. Centralized
// so list-by-project and list-all share identical scan semantics
// (column order MUST match the SELECT lists above).
func scanAPIKeyList(rows *sql.Rows) ([]*APIKey, error) {
	var out []*APIKey
	for rows.Next() {
		var (
			k          APIKey
			createdAt  string
			lastUsedAt sql.NullString
			name       sql.NullString
			role       sql.NullString
		)
		if err := rows.Scan(
			&k.KeyID, &k.ProjectID, &k.KeyPrefix,
			&name, &createdAt, &lastUsedAt, &k.Scope, &k.ExpiresAt, &k.Source, &role,
		); err != nil {
			return nil, err
		}
		if name.Valid {
			k.Name = name.String
		}
		if role.Valid {
			k.Role = role.String
		}
		k.CreatedAt = parseFlexTime(createdAt)
		if lastUsedAt.Valid {
			t := parseFlexTime(lastUsedAt.String)
			if !t.IsZero() {
				k.LastUsedAt = &t
			}
		}
		out = append(out, &k)
	}
	return out, rows.Err()
}

// DeleteAPIKeyByID hard-deletes any API key by its key_id, with no
// project_id guard. Admin-only. Used by the /admin/api-keys page to
// revoke keys across every project (including admin-scope keys in
// project _admin). Returns ErrNotFound if the key does not exist.
func (s *SQLiteStore) DeleteAPIKeyByID(ctx context.Context, keyID string) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE key_id = ?`,
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

// DeleteAPIKeysByUserID hard-deletes every API key whose user_id
// matches. Called from HandleRemoveMember so a removed team member's
// existing credentials stop working immediately. Returns the
// number of rows deleted; never returns ErrNotFound (0 deletions is a
// valid outcome when the removed user never minted a key).
func (s *SQLiteStore) DeleteAPIKeysByUserID(
	ctx context.Context,
	userID string,
) (int, error) {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE user_id = ?`,
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

// DeleteAPIKey hard-deletes an API key, but ONLY if the key belongs
// to the given project. Returns ErrNotFound if the key doesn't exist
// OR if it belongs to a different project (don't leak existence
// across tenants). After deletion the key's hash is gone, re-minting
// requires a new random key.
func (s *SQLiteStore) DeleteAPIKey(
	ctx context.Context,
	keyID, projectID string,
) error {
	res, err := s.db.ExecContext(
		ctx,
		`DELETE FROM api_keys WHERE key_id = ? AND project_id = ?`,
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
