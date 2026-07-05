package store

// User TOTP, backup codes, and pending 2FA tokens for the customer-
// facing two-factor authentication feature. See
// migrations/038_user_totp.sql for the schema rationale.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// UserTOTP is one row of the user_totp table. SecretEncrypted is the
// AES-256-GCM ciphertext of the TOTP shared secret; the encryption
// key lives in MESEDI_TOTP_ENCRYPTION_KEY (Fly secret) and is
// applied at the API layer, NOT the store layer. The store sees
// opaque bytes.
type UserTOTP struct {
	UserID           string
	SecretEncrypted  []byte
	CreatedAt        time.Time
	LastUsedAt       time.Time // zero if never used
}

// BackupCode is one row of the user_backup_codes table. CodeHash is
// the SHA-256 hex of the raw backup code; raw codes are shown to the
// customer exactly once at enrollment / regenerate.
type BackupCode struct {
	CodeHash   string
	UserID     string
	CreatedAt  time.Time
	UsedAt     time.Time // zero if unused
}

// Pending2FAToken is one row of the pending_2fa_tokens table.
// Minted by HandleSignin when 2FA is enabled, redeemed by
// HandleTwoFactorVerify. TokenHash is the SHA-256 hex of the raw
// token (which is returned exactly once in the signin response).
type Pending2FAToken struct {
	TokenHash  string
	UserID     string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	UsedAt     time.Time // zero if unused
}

// ─────────────────────────────────────────────────────────────────────
// SQLite impl
// ─────────────────────────────────────────────────────────────────────

// UpsertUserTOTP inserts or replaces the TOTP secret for a user.
// Used at enrollment (HandleTOTPSetupVerify) and at any future
// rotation. Idempotent on user_id.
func (s *SQLiteStore) UpsertUserTOTP(ctx context.Context, t *UserTOTP) error {
	if t == nil || t.UserID == "" || len(t.SecretEncrypted) == 0 {
		return errors.New("user_totp missing required field")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_totp (user_id, secret_encrypted, created_at, last_used_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(user_id) DO UPDATE SET
			secret_encrypted = excluded.secret_encrypted,
			created_at       = excluded.created_at,
			last_used_at     = NULL
	`, t.UserID, t.SecretEncrypted, t.CreatedAt.UTC(), nullTimePtr(t.LastUsedAt))
	if err != nil {
		return fmt.Errorf("upsert user_totp: %w", err)
	}
	return nil
}

// GetUserTOTP returns the row for userID or ErrNotFound. Used by the
// auth middleware on every dashboard request (to decide whether to
// enforce 2FA) and by the verify handler.
func (s *SQLiteStore) GetUserTOTP(ctx context.Context, userID string) (*UserTOTP, error) {
	if userID == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, secret_encrypted, created_at, last_used_at
		FROM user_totp
		WHERE user_id = ?
	`, userID)
	t := &UserTOTP{}
	var lastUsed sql.NullTime
	if err := row.Scan(&t.UserID, &t.SecretEncrypted, &t.CreatedAt, &lastUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get user_totp: %w", err)
	}
	if lastUsed.Valid {
		t.LastUsedAt = lastUsed.Time
	}
	return t, nil
}

// DeleteUserTOTP removes the row. Idempotent. Used by
// HandleTOTPDisable. The handler also calls DeleteBackupCodesForUser
// so disabling 2FA leaves no orphan rows.
func (s *SQLiteStore) DeleteUserTOTP(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_totp WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete user_totp: %w", err)
	}
	return nil
}

// TouchUserTOTP updates last_used_at on a successful verify. The
// admin "share recent use" report can later surface this to confirm
// the customer's 2FA is actually being used.
func (s *SQLiteStore) TouchUserTOTP(ctx context.Context, userID string, lastUsedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE user_totp SET last_used_at = ? WHERE user_id = ?
	`, lastUsedAt.UTC(), userID)
	if err != nil {
		return fmt.Errorf("touch user_totp: %w", err)
	}
	return nil
}

// CreateBackupCodes inserts a batch of hashed backup codes. The
// caller generates the raw codes, hashes each with SHA-256, returns
// the raw codes to the customer ONCE, and persists the hashes here.
func (s *SQLiteStore) CreateBackupCodes(ctx context.Context, codes []*BackupCode) error {
	if len(codes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO user_backup_codes (code_hash, user_id, created_at, used_at)
		VALUES (?, ?, ?, NULL)
	`)
	if err != nil {
		return fmt.Errorf("prepare insert backup code: %w", err)
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for _, c := range codes {
		if c.UserID == "" || c.CodeHash == "" {
			return errors.New("backup code missing required field")
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = now
		}
		if _, err := stmt.ExecContext(ctx, c.CodeHash, c.UserID, c.CreatedAt.UTC()); err != nil {
			return fmt.Errorf("insert backup code: %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backup codes: %w", err)
	}
	return nil
}

// ConsumeBackupCode marks one row used. Returns ErrNotFound if the
// (userID, codeHash) pair doesn't exist or has already been used.
// Used as a fallback path when the customer cannot reach their
// authenticator app.
func (s *SQLiteStore) ConsumeBackupCode(ctx context.Context, userID, codeHash string, usedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_backup_codes
		SET used_at = ?
		WHERE code_hash = ? AND user_id = ? AND used_at IS NULL
	`, usedAt.UTC(), codeHash, userID)
	if err != nil {
		return fmt.Errorf("consume backup code: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteBackupCodesForUser wipes every row for one user. Called from
// HandleTOTPDisable (clean teardown) and from
// HandleTOTPRegenerateBackupCodes (replace the old set in one shot).
func (s *SQLiteStore) DeleteBackupCodesForUser(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_backup_codes WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("delete backup codes: %w", err)
	}
	return nil
}

// CountUnusedBackupCodes returns how many backup codes remain
// available for a user. Surfaced in the dashboard's 2FA section so
// the customer sees "you have 7 backup codes left" without us
// revealing the codes themselves.
func (s *SQLiteStore) CountUnusedBackupCodes(ctx context.Context, userID string) (int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_backup_codes WHERE user_id = ? AND used_at IS NULL
	`, userID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count backup codes: %w", err)
	}
	return n, nil
}

// CreatePending2FAToken persists a fresh pending token. The caller
// generates the raw token, hashes with SHA-256, returns the raw token
// to the dashboard ONCE in the signin response, and persists the
// hash here.
func (s *SQLiteStore) CreatePending2FAToken(ctx context.Context, t *Pending2FAToken) error {
	if t == nil || t.TokenHash == "" || t.UserID == "" {
		return errors.New("pending_2fa_token missing required field")
	}
	if t.ExpiresAt.IsZero() {
		return errors.New("pending_2fa_token expires_at required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_2fa_tokens (token_hash, user_id, created_at, expires_at, used_at)
		VALUES (?, ?, ?, ?, NULL)
	`, t.TokenHash, t.UserID, t.CreatedAt.UTC(), t.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert pending_2fa_token: %w", err)
	}
	return nil
}

// GetPending2FAToken returns the row for tokenHash or ErrNotFound.
// Caller MUST verify (used_at IS NULL) AND (expires_at > now) before
// trusting; we return the row including used_at so the caller can
// distinguish "expired" from "already used" from "valid" in its
// response.
func (s *SQLiteStore) GetPending2FAToken(ctx context.Context, tokenHash string) (*Pending2FAToken, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, created_at, expires_at, used_at
		FROM pending_2fa_tokens
		WHERE token_hash = ?
	`, tokenHash)
	t := &Pending2FAToken{}
	var used sql.NullTime
	if err := row.Scan(&t.TokenHash, &t.UserID, &t.CreatedAt, &t.ExpiresAt, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get pending_2fa_token: %w", err)
	}
	if used.Valid {
		t.UsedAt = used.Time
	}
	return t, nil
}

// MarkPending2FATokenUsed flips used_at on the row. Returns
// ErrNotFound if the token is gone or already used.
func (s *SQLiteStore) MarkPending2FATokenUsed(ctx context.Context, tokenHash string, usedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE pending_2fa_tokens SET used_at = ?
		WHERE token_hash = ? AND used_at IS NULL
	`, usedAt.UTC(), tokenHash)
	if err != nil {
		return fmt.Errorf("mark pending_2fa_token used: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

// DeleteExpiredPending2FATokens sweeps rows whose expires_at is
// before asOf. Wired into the retention scheduler later; not blocking
// for commit 1a.
func (s *SQLiteStore) DeleteExpiredPending2FATokens(ctx context.Context, asOf time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM pending_2fa_tokens WHERE expires_at < ?
	`, asOf.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired pending_2fa_tokens: %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

// SetSessionPassed2FA flips sessions.passed_2fa = 1 atomically.
// Called by HandleTOTPSetupVerify so a customer who enables 2FA does
// not get immediately kicked out by their own enrollment, and by
// HandleTwoFactorVerify to upgrade the freshly-minted session.
func (s *SQLiteStore) SetSessionPassed2FA(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET passed_2fa = 1 WHERE token_hash = ?
	`, tokenHash)
	if err != nil {
		return fmt.Errorf("set session passed_2fa: %w", err)
	}
	return nil
}

// nullTimePtr returns a sql.NullTime that's invalid when t is zero,
// otherwise valid. Used in INSERTs so we can write NULL into
// last_used_at on first enrollment.
func nullTimePtr(t time.Time) any {
	if t.IsZero() {
		return nil
	}
	return t.UTC()
}

// ─────────────────────────────────────────────────────────────────────
// Postgres impl
// ─────────────────────────────────────────────────────────────────────

func (s *PostgresStore) UpsertUserTOTP(ctx context.Context, t *UserTOTP) error {
	if t == nil || t.UserID == "" || len(t.SecretEncrypted) == 0 {
		return errors.New("user_totp missing required field")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO user_totp (user_id, secret_encrypted, created_at, last_used_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT (user_id) DO UPDATE SET
			secret_encrypted = EXCLUDED.secret_encrypted,
			created_at       = EXCLUDED.created_at,
			last_used_at     = NULL
	`, t.UserID, t.SecretEncrypted, t.CreatedAt.UTC(), nullTimePtr(t.LastUsedAt))
	if err != nil {
		return fmt.Errorf("upsert user_totp (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) GetUserTOTP(ctx context.Context, userID string) (*UserTOTP, error) {
	if userID == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT user_id, secret_encrypted, created_at, last_used_at
		FROM user_totp
		WHERE user_id = $1
	`, userID)
	t := &UserTOTP{}
	var lastUsed sql.NullTime
	if err := row.Scan(&t.UserID, &t.SecretEncrypted, &t.CreatedAt, &lastUsed); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get user_totp (postgres): %w", err)
	}
	if lastUsed.Valid {
		t.LastUsedAt = lastUsed.Time
	}
	return t, nil
}

func (s *PostgresStore) DeleteUserTOTP(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_totp WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete user_totp (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) TouchUserTOTP(ctx context.Context, userID string, lastUsedAt time.Time) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE user_totp SET last_used_at = $1 WHERE user_id = $2
	`, lastUsedAt.UTC(), userID)
	if err != nil {
		return fmt.Errorf("touch user_totp (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) CreateBackupCodes(ctx context.Context, codes []*BackupCode) error {
	if len(codes) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx (postgres): %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO user_backup_codes (code_hash, user_id, created_at, used_at)
		VALUES ($1, $2, $3, NULL)
	`)
	if err != nil {
		return fmt.Errorf("prepare insert backup code (postgres): %w", err)
	}
	defer stmt.Close()
	now := time.Now().UTC()
	for _, c := range codes {
		if c.UserID == "" || c.CodeHash == "" {
			return errors.New("backup code missing required field")
		}
		if c.CreatedAt.IsZero() {
			c.CreatedAt = now
		}
		if _, err := stmt.ExecContext(ctx, c.CodeHash, c.UserID, c.CreatedAt.UTC()); err != nil {
			return fmt.Errorf("insert backup code (postgres): %w", err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit backup codes (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) ConsumeBackupCode(ctx context.Context, userID, codeHash string, usedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE user_backup_codes
		SET used_at = $1
		WHERE code_hash = $2 AND user_id = $3 AND used_at IS NULL
	`, usedAt.UTC(), codeHash, userID)
	if err != nil {
		return fmt.Errorf("consume backup code (postgres): %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) DeleteBackupCodesForUser(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM user_backup_codes WHERE user_id = $1`, userID)
	if err != nil {
		return fmt.Errorf("delete backup codes (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) CountUnusedBackupCodes(ctx context.Context, userID string) (int, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT COUNT(*) FROM user_backup_codes WHERE user_id = $1 AND used_at IS NULL
	`, userID)
	var n int
	if err := row.Scan(&n); err != nil {
		return 0, fmt.Errorf("count backup codes (postgres): %w", err)
	}
	return n, nil
}

func (s *PostgresStore) CreatePending2FAToken(ctx context.Context, t *Pending2FAToken) error {
	if t == nil || t.TokenHash == "" || t.UserID == "" {
		return errors.New("pending_2fa_token missing required field")
	}
	if t.ExpiresAt.IsZero() {
		return errors.New("pending_2fa_token expires_at required")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO pending_2fa_tokens (token_hash, user_id, created_at, expires_at, used_at)
		VALUES ($1, $2, $3, $4, NULL)
	`, t.TokenHash, t.UserID, t.CreatedAt.UTC(), t.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert pending_2fa_token (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) GetPending2FAToken(ctx context.Context, tokenHash string) (*Pending2FAToken, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT token_hash, user_id, created_at, expires_at, used_at
		FROM pending_2fa_tokens
		WHERE token_hash = $1
	`, tokenHash)
	t := &Pending2FAToken{}
	var used sql.NullTime
	if err := row.Scan(&t.TokenHash, &t.UserID, &t.CreatedAt, &t.ExpiresAt, &used); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrNotFound
		}
		return nil, fmt.Errorf("get pending_2fa_token (postgres): %w", err)
	}
	if used.Valid {
		t.UsedAt = used.Time
	}
	return t, nil
}

func (s *PostgresStore) MarkPending2FATokenUsed(ctx context.Context, tokenHash string, usedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE pending_2fa_tokens SET used_at = $1
		WHERE token_hash = $2 AND used_at IS NULL
	`, usedAt.UTC(), tokenHash)
	if err != nil {
		return fmt.Errorf("mark pending_2fa_token used (postgres): %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *PostgresStore) DeleteExpiredPending2FATokens(ctx context.Context, asOf time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `
		DELETE FROM pending_2fa_tokens WHERE expires_at < $1
	`, asOf.UTC())
	if err != nil {
		return 0, fmt.Errorf("delete expired pending_2fa_tokens (postgres): %w", err)
	}
	n, _ := res.RowsAffected()
	return int(n), nil
}

func (s *PostgresStore) SetSessionPassed2FA(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `
		UPDATE sessions SET passed_2fa = TRUE WHERE token_hash = $1
	`, tokenHash)
	if err != nil {
		return fmt.Errorf("set session passed_2fa (postgres): %w", err)
	}
	return nil
}
