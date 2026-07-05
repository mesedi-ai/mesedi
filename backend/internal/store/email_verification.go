// Email-verification storage methods (pre-launch).
//
// Two tables back this feature:
//   verified_emails              — one row per email that has ever
//                                  proved ownership (any method)
//   email_verification_tokens    — one row per outstanding token in
//                                  flight (issued during raw-email
//                                  signup, consumed by the confirm
//                                  endpoint)
//
// IsEmailVerified is the read every customer request makes; it needs
// to be cheap. The PRIMARY KEY on verified_emails.email gives an O(1)
// lookup. Email comparison is case-insensitive: callers should pass
// the same LOWER(TRIM(...)) form that the signup handler stores.

package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
)

// EmailVerificationToken is one in-flight verification request. The
// raw token shipped in the email is hashed at neither end — it's a
// 32-byte url-safe random string, single-use, lives at most 24 hours.
// (Magic-link tokens DO hash for a different reason: those are reused
// as session tokens in the cookie store. Verification tokens are
// fire-and-forget so the simpler shape is fine.)
type EmailVerificationToken struct {
	Token     string
	Email     string
	ProjectID string
	CreatedAt time.Time
	ExpiresAt time.Time
	UsedAt    *time.Time
}

// ErrEmailTokenNotFound + ErrEmailTokenExpired + ErrEmailTokenAlreadyUsed
// are the three negative cases the confirm handler maps to a single
// user-facing "this link is no longer valid" message. Distinct
// sentinels so tests + logs can tell them apart without sniffing
// strings.
var (
	ErrEmailTokenNotFound    = errors.New("email verification token not found")
	ErrEmailTokenExpired     = errors.New("email verification token expired")
	ErrEmailTokenAlreadyUsed = errors.New("email verification token already used")
)

// normalizeEmail mirrors what signup.go does to owner_email so the
// comparison succeeds across paths. Centralize here so future callers
// don't have to remember.
func normalizeEmail(email string) string {
	return strings.ToLower(strings.TrimSpace(email))
}

// --- SQLite -------------------------------------------------------

func (s *SQLiteStore) IsEmailVerified(ctx context.Context, email string) (bool, error) {
	normalized := normalizeEmail(email)
	if normalized == "" {
		return false, nil
	}
	var verifiedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT verified_at FROM verified_emails WHERE email = ?
	`, normalized).Scan(&verifiedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read verified_emails (sqlite): %w", err)
	}
	return true, nil
}

func (s *SQLiteStore) GetEmailVerificationMethod(ctx context.Context, email string) (string, error) {
	normalized := normalizeEmail(email)
	if normalized == "" {
		return "", nil
	}
	var method string
	err := s.db.QueryRowContext(ctx, `
		SELECT method FROM verified_emails WHERE email = ?
	`, normalized).Scan(&method)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read verified_emails method (sqlite): %w", err)
	}
	return method, nil
}

func (s *SQLiteStore) MarkEmailVerified(ctx context.Context, email, method string) error {
	normalized := normalizeEmail(email)
	if normalized == "" {
		return errors.New("MarkEmailVerified: empty email")
	}
	if method == "" {
		return errors.New("MarkEmailVerified: empty method")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO verified_emails (email, verified_at, method)
		VALUES (?, ?, ?)
		ON CONFLICT(email) DO UPDATE SET
			verified_at = excluded.verified_at,
			method      = excluded.method
	`, normalized, time.Now().UTC(), method)
	if err != nil {
		return fmt.Errorf("upsert verified_emails (sqlite): %w", err)
	}
	return nil
}

func (s *SQLiteStore) CreateEmailVerificationToken(
	ctx context.Context, t *EmailVerificationToken,
) error {
	if t == nil || t.Token == "" || t.Email == "" || t.ProjectID == "" {
		return errors.New("CreateEmailVerificationToken: missing required field")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = t.CreatedAt.Add(24 * time.Hour)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO email_verification_tokens
			(token, email, project_id, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?)
	`, t.Token, normalizeEmail(t.Email), t.ProjectID,
		t.CreatedAt.UTC(), t.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert email_verification_tokens (sqlite): %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetEmailVerificationToken(
	ctx context.Context, token string,
) (*EmailVerificationToken, error) {
	if token == "" {
		return nil, ErrEmailTokenNotFound
	}
	var t EmailVerificationToken
	var usedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT token, email, project_id, created_at, expires_at, used_at
		FROM email_verification_tokens
		WHERE token = ?
	`, token).Scan(&t.Token, &t.Email, &t.ProjectID,
		&t.CreatedAt, &t.ExpiresAt, &usedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrEmailTokenNotFound
		}
		return nil, fmt.Errorf("read email_verification_tokens (sqlite): %w", err)
	}
	if usedAt.Valid {
		t.UsedAt = &usedAt.Time
	}
	return &t, nil
}

func (s *SQLiteStore) MarkEmailVerificationTokenUsed(
	ctx context.Context, token string,
) error {
	if token == "" {
		return ErrEmailTokenNotFound
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE email_verification_tokens
		SET used_at = ?
		WHERE token = ? AND used_at IS NULL
	`, time.Now().UTC(), token)
	if err != nil {
		return fmt.Errorf("update email_verification_tokens (sqlite): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil // best-effort; the unique constraint already guarantees idempotency
	}
	if n == 0 {
		// Either the token does not exist or it was already used.
		// Disambiguate so the caller can surface a precise message.
		t, getErr := s.GetEmailVerificationToken(ctx, token)
		if getErr != nil {
			return getErr
		}
		if t.UsedAt != nil {
			return ErrEmailTokenAlreadyUsed
		}
		return ErrEmailTokenNotFound
	}
	return nil
}

// --- Postgres -----------------------------------------------------

func (s *PostgresStore) IsEmailVerified(ctx context.Context, email string) (bool, error) {
	normalized := normalizeEmail(email)
	if normalized == "" {
		return false, nil
	}
	var verifiedAt time.Time
	err := s.db.QueryRowContext(ctx, `
		SELECT verified_at FROM verified_emails WHERE email = $1
	`, normalized).Scan(&verifiedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, fmt.Errorf("read verified_emails (postgres): %w", err)
	}
	return true, nil
}

func (s *PostgresStore) GetEmailVerificationMethod(ctx context.Context, email string) (string, error) {
	normalized := normalizeEmail(email)
	if normalized == "" {
		return "", nil
	}
	var method string
	err := s.db.QueryRowContext(ctx, `
		SELECT method FROM verified_emails WHERE email = $1
	`, normalized).Scan(&method)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("read verified_emails method (postgres): %w", err)
	}
	return method, nil
}

func (s *PostgresStore) MarkEmailVerified(ctx context.Context, email, method string) error {
	normalized := normalizeEmail(email)
	if normalized == "" {
		return errors.New("MarkEmailVerified: empty email")
	}
	if method == "" {
		return errors.New("MarkEmailVerified: empty method")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO verified_emails (email, verified_at, method)
		VALUES ($1, $2, $3)
		ON CONFLICT (email) DO UPDATE SET
			verified_at = EXCLUDED.verified_at,
			method      = EXCLUDED.method
	`, normalized, time.Now().UTC(), method)
	if err != nil {
		return fmt.Errorf("upsert verified_emails (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) CreateEmailVerificationToken(
	ctx context.Context, t *EmailVerificationToken,
) error {
	if t == nil || t.Token == "" || t.Email == "" || t.ProjectID == "" {
		return errors.New("CreateEmailVerificationToken: missing required field")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = t.CreatedAt.Add(24 * time.Hour)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO email_verification_tokens
			(token, email, project_id, created_at, expires_at)
		VALUES ($1, $2, $3, $4, $5)
	`, t.Token, normalizeEmail(t.Email), t.ProjectID,
		t.CreatedAt.UTC(), t.ExpiresAt.UTC())
	if err != nil {
		return fmt.Errorf("insert email_verification_tokens (postgres): %w", err)
	}
	return nil
}

func (s *PostgresStore) GetEmailVerificationToken(
	ctx context.Context, token string,
) (*EmailVerificationToken, error) {
	if token == "" {
		return nil, ErrEmailTokenNotFound
	}
	var t EmailVerificationToken
	var usedAt sql.NullTime
	err := s.db.QueryRowContext(ctx, `
		SELECT token, email, project_id, created_at, expires_at, used_at
		FROM email_verification_tokens
		WHERE token = $1
	`, token).Scan(&t.Token, &t.Email, &t.ProjectID,
		&t.CreatedAt, &t.ExpiresAt, &usedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, ErrEmailTokenNotFound
		}
		return nil, fmt.Errorf("read email_verification_tokens (postgres): %w", err)
	}
	if usedAt.Valid {
		t.UsedAt = &usedAt.Time
	}
	return &t, nil
}

func (s *PostgresStore) MarkEmailVerificationTokenUsed(
	ctx context.Context, token string,
) error {
	if token == "" {
		return ErrEmailTokenNotFound
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE email_verification_tokens
		SET used_at = $1
		WHERE token = $2 AND used_at IS NULL
	`, time.Now().UTC(), token)
	if err != nil {
		return fmt.Errorf("update email_verification_tokens (postgres): %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil
	}
	if n == 0 {
		t, getErr := s.GetEmailVerificationToken(ctx, token)
		if getErr != nil {
			return getErr
		}
		if t.UsedAt != nil {
			return ErrEmailTokenAlreadyUsed
		}
		return ErrEmailTokenNotFound
	}
	return nil
}
