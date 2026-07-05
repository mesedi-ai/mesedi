package store

// Session + store methods that back the cookie-based dashboard auth
// flow. See migrations/030_sessions.sql for the schema
// rationale.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// Session is one row of the sessions table. The TokenHash is the
// SHA-256 of the raw cookie value (which never lives in the DB).
// Lookups happen by computing the hash of the incoming cookie and
// querying on TokenHash.
type Session struct {
	TokenHash       string    `json:"-"`
	UserID          string    `json:"user_id"`
	ProjectID       string    `json:"project_id"`
	CreatedAt       time.Time `json:"created_at"`
	ExpiresAt       time.Time `json:"expires_at"`
	LastUsedAt      time.Time `json:"last_used_at"`
	UserAgent       string    `json:"user_agent,omitempty"`
	IPAddress       string    `json:"ip_address,omitempty"`
	// PassedTwoFactor is true iff the session was minted via
	// the /auth/2fa-verify path OR was upgraded by HandleTOTPSetupVerify
	// at enrollment time. Auth middleware compares this against the
	// user's TOTP enrollment state: if the user has TOTP enabled but
	// this is false, the request is rejected and the customer is
	// redirected to the 2FA prompt. Pre-sessions default to false
	// (migration 038 sets passed_2fa = 0); users who don't have TOTP
	// enabled are not affected.
	PassedTwoFactor bool `json:"passed_2fa"`
}

// SessionLister is the optional Store interface for session CRUD.
// Kept on its own interface so test stubs can opt in via embedding
// only when relevant, mirroring AuditEventLister.
type SessionLister interface {
	// CreateSession inserts a fresh row. Caller fills in TokenHash,
	// UserID, ProjectID, CreatedAt, ExpiresAt, LastUsedAt at minimum;
	// UserAgent and IPAddress are optional and default to empty.
	CreateSession(ctx context.Context, s *Session) error

	// GetSessionByTokenHash returns the row whose token_hash matches
	// or ErrNotFound. Does NOT check expiry; the caller (auth
	// middleware) compares ExpiresAt against time.Now() so we can
	// distinguish "no such session" from "expired session" in the
	// response.
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)

	// TouchSession updates last_used_at and expires_at on every
	// authenticated request. The sliding-window refresh lets active
	// customers stay logged in indefinitely; an idle browser tab
	// will expire and force a re-login.
	TouchSession(ctx context.Context, tokenHash string, lastUsedAt, newExpiresAt time.Time) error

	// DeleteSession removes one row by token_hash. Used by the
	// logout endpoint. No error if the row already does not exist
	// (logout should be idempotent).
	DeleteSession(ctx context.Context, tokenHash string) error

	// DeleteSessionsByUserID removes every session for one user_id
	// and returns the count revoked. Wired into HandleRevokeAPIKey
	// and HandleRemoveMember so revoking a member's key or kicking
	// them from the org immediately logs them out of every browser
	// they had open (step 1 hardening per Robert).
	DeleteSessionsByUserID(ctx context.Context, userID string) (int, error)

	// ListSessionsForUser returns every non-expired session for
	// userID, sorted by last_used_at DESC. Backs the /me/sessions
	// surface in /app/settings (Batch 4 UI). Expired rows are
	// excluded so the customer is not surprised by a stale entry.
	ListSessionsForUser(ctx context.Context, userID string, now time.Time) ([]*Session, error)

	// UpdateSessionProjectID switches a session's active project.
	// Used by the multi-project Team customer flow: a customer with
	// projects A and B has ONE cookie; the active project travels
	// in the sessions row and gets flipped via this method when the
	// customer picks a different project from the switcher.
	UpdateSessionProjectID(ctx context.Context, tokenHash, newProjectID string) error

	// DeleteExpiredSessions sweeps rows whose expires_at is before
	// asOf and returns the count removed. Wired into the retention
	// scheduler so stale rows do not accumulate forever.
	DeleteExpiredSessions(ctx context.Context, asOf time.Time) (int, error)
}

// --- SQLite impl --------------------------------------------------

// CreateSession persists one session row. The caller is responsible
// for hashing the raw token before passing it in.
func (s *SQLiteStore) CreateSession(ctx context.Context, sess *Session) error {
	if sess == nil {
		return errors.New("nil session")
	}
	if sess.TokenHash == "" || sess.UserID == "" || sess.ProjectID == "" {
		return errors.New("session missing required field (token_hash, user_id, project_id)")
	}
	now := time.Now().UTC()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.LastUsedAt.IsZero() {
		sess.LastUsedAt = now
	}
	if sess.ExpiresAt.IsZero() {
		return errors.New("session expires_at must be set by caller (no implicit TTL at the store layer)")
	}
	passed := 0
	if sess.PassedTwoFactor {
		passed = 1
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			token_hash, user_id, project_id,
			created_at, expires_at, last_used_at,
			user_agent, ip_address, passed_2fa
		)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		sess.TokenHash, sess.UserID, sess.ProjectID,
		sess.CreatedAt.UTC(), sess.ExpiresAt.UTC(), sess.LastUsedAt.UTC(),
		sess.UserAgent, sess.IPAddress, passed,
	)
	if err != nil {
		return fmt.Errorf("insert session: %w", err)
	}
	return nil
}

// GetSessionByTokenHash returns the row or ErrNotFound.
func (s *SQLiteStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, project_id,
		       created_at, expires_at, last_used_at,
		       user_agent, ip_address, passed_2fa
		FROM sessions
		WHERE token_hash = ?
	`, tokenHash)
	sess := &Session{}
	var ua, ip sql.NullString
	var passed int
	if err := row.Scan(
		&sess.TokenHash, &sess.UserID, &sess.ProjectID,
		&sess.CreatedAt, &sess.ExpiresAt, &sess.LastUsedAt,
		&ua, &ip, &passed,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get session: %w", err)
	}
	if ua.Valid {
		sess.UserAgent = ua.String
	}
	if ip.Valid {
		sess.IPAddress = ip.String
	}
	sess.PassedTwoFactor = passed != 0
	return sess, nil
}

// TouchSession updates last_used_at and expires_at.
func (s *SQLiteStore) TouchSession(ctx context.Context, tokenHash string, lastUsedAt, newExpiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET last_used_at = ?, expires_at = ?
		WHERE token_hash = ?
	`, lastUsedAt.UTC(), newExpiresAt.UTC(), tokenHash)
	if err != nil {
		return fmt.Errorf("touch session: %w", err)
	}
	return nil
}

// DeleteSession removes one row by token_hash. Idempotent.
func (s *SQLiteStore) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete session: %w", err)
	}
	return nil
}

// DeleteSessionsByUserID removes every session for one user.
func (s *SQLiteStore) DeleteSessionsByUserID(ctx context.Context, userID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions by user_id: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListSessionsForUser returns non-expired sessions for userID.
func (s *SQLiteStore) ListSessionsForUser(ctx context.Context, userID string, now time.Time) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT token_hash, user_id, project_id,
		       created_at, expires_at, last_used_at,
		       user_agent, ip_address, passed_2fa
		FROM sessions
		WHERE user_id = ? AND expires_at > ?
		ORDER BY last_used_at DESC
	`, userID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list sessions: %w", err)
	}
	defer rows.Close()

	out := make([]*Session, 0, 4)
	for rows.Next() {
		sess := &Session{}
		var ua, ip sql.NullString
		var passed int
		if err := rows.Scan(
			&sess.TokenHash, &sess.UserID, &sess.ProjectID,
			&sess.CreatedAt, &sess.ExpiresAt, &sess.LastUsedAt,
			&ua, &ip, &passed,
		); err != nil {
			return nil, fmt.Errorf("scan session: %w", err)
		}
		if ua.Valid {
			sess.UserAgent = ua.String
		}
		if ip.Valid {
			sess.IPAddress = ip.String
		}
		sess.PassedTwoFactor = passed != 0
		out = append(out, sess)
	}
	return out, rows.Err()
}

// UpdateSessionProjectID switches a session's active project.
func (s *SQLiteStore) UpdateSessionProjectID(ctx context.Context, tokenHash, newProjectID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET project_id = ? WHERE token_hash = ?
	`, newProjectID, tokenHash)
	if err != nil {
		return fmt.Errorf("update session project_id: %w", err)
	}
	return nil
}

// DeleteExpiredSessions sweeps rows whose expires_at is before asOf.
func (s *SQLiteStore) DeleteExpiredSessions(ctx context.Context, asOf time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, asOf.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// --- Postgres impl ------------------------------------------------

// CreateSession is the Postgres twin.
func (s *PostgresStore) CreateSession(ctx context.Context, sess *Session) error {
	if sess == nil {
		return errors.New("nil session")
	}
	if sess.TokenHash == "" || sess.UserID == "" || sess.ProjectID == "" {
		return errors.New("session missing required field (token_hash, user_id, project_id)")
	}
	now := time.Now().UTC()
	if sess.CreatedAt.IsZero() {
		sess.CreatedAt = now
	}
	if sess.LastUsedAt.IsZero() {
		sess.LastUsedAt = now
	}
	if sess.ExpiresAt.IsZero() {
		return errors.New("session expires_at must be set by caller (no implicit TTL at the store layer)")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO sessions (
			token_hash, user_id, project_id,
			created_at, expires_at, last_used_at,
			user_agent, ip_address, passed_2fa
		)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`,
		sess.TokenHash, sess.UserID, sess.ProjectID,
		sess.CreatedAt.UTC(), sess.ExpiresAt.UTC(), sess.LastUsedAt.UTC(),
		sess.UserAgent, sess.IPAddress, sess.PassedTwoFactor,
	)
	if err != nil {
		return fmt.Errorf("insert session (postgres): %w", err)
	}
	return nil
}

// GetSessionByTokenHash is the Postgres twin.
func (s *PostgresStore) GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, project_id,
		       created_at, expires_at, last_used_at,
		       user_agent, ip_address, passed_2fa
		FROM sessions
		WHERE token_hash = $1
	`, tokenHash)
	sess := &Session{}
	var ua, ip sql.NullString
	if err := row.Scan(
		&sess.TokenHash, &sess.UserID, &sess.ProjectID,
		&sess.CreatedAt, &sess.ExpiresAt, &sess.LastUsedAt,
		&ua, &ip, &sess.PassedTwoFactor,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get session (postgres): %w", err)
	}
	if ua.Valid {
		sess.UserAgent = ua.String
	}
	if ip.Valid {
		sess.IPAddress = ip.String
	}
	return sess, nil
}

// TouchSession is the Postgres twin.
func (s *PostgresStore) TouchSession(ctx context.Context, tokenHash string, lastUsedAt, newExpiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions
		SET last_used_at = $1, expires_at = $2
		WHERE token_hash = $3
	`, lastUsedAt.UTC(), newExpiresAt.UTC(), tokenHash)
	if err != nil {
		return fmt.Errorf("touch session (postgres): %w", err)
	}
	return nil
}

// DeleteSession is the Postgres twin.
func (s *PostgresStore) DeleteSession(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE token_hash = $1`, tokenHash)
	if err != nil {
		return fmt.Errorf("delete session (postgres): %w", err)
	}
	return nil
}

// DeleteSessionsByUserID is the Postgres twin.
func (s *PostgresStore) DeleteSessionsByUserID(ctx context.Context, userID string) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
	if err != nil {
		return 0, fmt.Errorf("delete sessions by user_id (postgres): %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// ListSessionsForUser is the Postgres twin.
func (s *PostgresStore) ListSessionsForUser(ctx context.Context, userID string, now time.Time) ([]*Session, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT token_hash, user_id, project_id,
		       created_at, expires_at, last_used_at,
		       user_agent, ip_address, passed_2fa
		FROM sessions
		WHERE user_id = $1 AND expires_at > $2
		ORDER BY last_used_at DESC
	`, userID, now.UTC())
	if err != nil {
		return nil, fmt.Errorf("list sessions (postgres): %w", err)
	}
	defer rows.Close()

	out := make([]*Session, 0, 4)
	for rows.Next() {
		sess := &Session{}
		var ua, ip sql.NullString
		if err := rows.Scan(
			&sess.TokenHash, &sess.UserID, &sess.ProjectID,
			&sess.CreatedAt, &sess.ExpiresAt, &sess.LastUsedAt,
			&ua, &ip, &sess.PassedTwoFactor,
		); err != nil {
			return nil, fmt.Errorf("scan session (postgres): %w", err)
		}
		if ua.Valid {
			sess.UserAgent = ua.String
		}
		if ip.Valid {
			sess.IPAddress = ip.String
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// UpdateSessionProjectID is the Postgres twin.
func (s *PostgresStore) UpdateSessionProjectID(ctx context.Context, tokenHash, newProjectID string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET project_id = $1 WHERE token_hash = $2
	`, newProjectID, tokenHash)
	if err != nil {
		return fmt.Errorf("update session project_id (postgres): %w", err)
	}
	return nil
}

// DeleteExpiredSessions is the Postgres twin.
func (s *PostgresStore) DeleteExpiredSessions(ctx context.Context, asOf time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < $1`, asOf.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired sessions (postgres): %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}
