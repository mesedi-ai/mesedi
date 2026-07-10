package store

// MagicLinkToken + persistence helpers (commit 2). See
// migrations/029_magic_link_tokens.sql for schema rationale.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"
)

// MagicLinkToken is one row of the magic_link_tokens table.
type MagicLinkToken struct {
	TokenID   string    `json:"token_id"`
	TokenHash string    `json:"-"` // never serialized
	Email     string    `json:"email"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	UsedAt    time.Time `json:"used_at,omitempty"`
	RequestIP string    `json:"request_ip,omitempty"`
}

// MagicLinkTTL is how long a magic-link token stays valid from mint.
// 15 minutes is the industry-standard window: long enough to handle
// "I'll go grab my laptop" UX, short enough that a leaked email gives
// an attacker a tight window to act.
const MagicLinkTTL = 15 * time.Minute

// MagicLinkPersistor is the optional store surface for the magic-link
// feature. Kept on its own interface so tests can stub it without
// implementing the full Store.
type MagicLinkPersistor interface {
	CreateMagicLinkToken(ctx context.Context, t *MagicLinkToken) error
	GetMagicLinkTokenByHash(ctx context.Context, tokenHash string) (*MagicLinkToken, error)
	MarkMagicLinkTokenUsed(ctx context.Context, tokenID string) error
}

// --- SQLite impl --------------------------------------------------

func (s *SQLiteStore) CreateMagicLinkToken(ctx context.Context, t *MagicLinkToken) error {
	if t == nil {
		return errors.New("nil magic-link token")
	}
	if t.TokenID == "" || t.TokenHash == "" || t.Email == "" {
		return errors.New("magic-link token missing required field")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = t.CreatedAt.Add(MagicLinkTTL)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO magic_link_tokens (token_id, token_hash, email, created_at, expires_at, request_ip)
		VALUES (?, ?, ?, ?, ?, ?)
	`,
		t.TokenID,
		t.TokenHash,
		t.Email,
		t.CreatedAt.UTC().Format(time.RFC3339Nano),
		t.ExpiresAt.UTC().Format(time.RFC3339Nano),
		t.RequestIP,
	)
	if err != nil {
		return fmt.Errorf("insert magic_link_token: %w", err)
	}
	return nil
}

func (s *SQLiteStore) GetMagicLinkTokenByHash(ctx context.Context, tokenHash string) (*MagicLinkToken, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	t := &MagicLinkToken{}
	var createdAt, expiresAt, usedAt, requestIP string
	err := s.db.QueryRowContext(ctx, `
		SELECT token_id, token_hash, email, created_at, expires_at, used_at, request_ip
		FROM magic_link_tokens
		WHERE token_hash = ?
	`, tokenHash).Scan(
		&t.TokenID, &t.TokenHash, &t.Email,
		&createdAt, &expiresAt, &usedAt, &requestIP,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = parseFlexTime(createdAt)
	t.ExpiresAt = parseFlexTime(expiresAt)
	if usedAt != "" {
		t.UsedAt = parseFlexTime(usedAt)
	}
	t.RequestIP = requestIP
	return t, nil
}

func (s *SQLiteStore) MarkMagicLinkTokenUsed(ctx context.Context, tokenID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE magic_link_tokens
		SET used_at = ?
		WHERE token_id = ? AND used_at = ''
	`,
		time.Now().UTC().Format(time.RFC3339Nano),
		tokenID,
	)
	if err != nil {
		return fmt.Errorf("update magic_link_token used_at: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Token either does not exist or was already used. Either
		// way the caller's burn-the-token operation has nothing
		// further to do; we surface ErrNotFound so the verify
		// handler can distinguish "burned" from "found and burned
		// now" without a separate read.
		return ErrNotFound
	}
	return nil
}

// --- Postgres impl ------------------------------------------------

func (s *PostgresStore) CreateMagicLinkToken(ctx context.Context, t *MagicLinkToken) error {
	if t == nil {
		return errors.New("nil magic-link token")
	}
	if t.TokenID == "" || t.TokenHash == "" || t.Email == "" {
		return errors.New("magic-link token missing required field")
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	if t.ExpiresAt.IsZero() {
		t.ExpiresAt = t.CreatedAt.Add(MagicLinkTTL)
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO magic_link_tokens (token_id, token_hash, email, created_at, expires_at, request_ip)
		VALUES ($1, $2, $3, $4, $5, $6)
	`,
		t.TokenID,
		t.TokenHash,
		t.Email,
		t.CreatedAt.UTC().Format(time.RFC3339Nano),
		t.ExpiresAt.UTC().Format(time.RFC3339Nano),
		t.RequestIP,
	)
	if err != nil {
		return fmt.Errorf("insert magic_link_token: %w", err)
	}
	return nil
}

func (s *PostgresStore) GetMagicLinkTokenByHash(ctx context.Context, tokenHash string) (*MagicLinkToken, error) {
	if tokenHash == "" {
		return nil, ErrNotFound
	}
	t := &MagicLinkToken{}
	var createdAt, expiresAt, usedAt, requestIP string
	err := s.db.QueryRowContext(ctx, `
		SELECT token_id, token_hash, email, created_at, expires_at, used_at, request_ip
		FROM magic_link_tokens
		WHERE token_hash = $1
	`, tokenHash).Scan(
		&t.TokenID, &t.TokenHash, &t.Email,
		&createdAt, &expiresAt, &usedAt, &requestIP,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	t.CreatedAt = parseFlexTime(createdAt)
	t.ExpiresAt = parseFlexTime(expiresAt)
	if usedAt != "" {
		t.UsedAt = parseFlexTime(usedAt)
	}
	t.RequestIP = requestIP
	return t, nil
}

func (s *PostgresStore) MarkMagicLinkTokenUsed(ctx context.Context, tokenID string) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE magic_link_tokens
		SET used_at = $1
		WHERE token_id = $2 AND used_at = ''
	`,
		time.Now().UTC().Format(time.RFC3339Nano),
		tokenID,
	)
	if err != nil {
		return fmt.Errorf("update magic_link_token used_at: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return ErrNotFound
	}
	return nil
}
