package store

import (
	"context"
	"time"

	"mesedi/backend/internal/events"
)

// Projects, audit events, billing events, request logs, magic links,
// email verification, sessions and 2FA. Everything that answers "who is
// this and are they allowed in", plus the records kept about it.
//
// Split out of store.go on 2026-09-04. The Store interface had grown to
// 1,570 lines inside a 2,463-line file, which tripped the audit's
// 1000-line limit as a BLOCKING failure and made every store change
// carry it.
//
// This is a pure move. The declarations below are byte-identical to what
// they were in store.go and Store now embeds this interface, so every
// implementation and every caller is unchanged. Go does not care which
// file in a package a declaration lives in, which is what makes a split
// like this verifiable by compiling.

type IdentityStore interface {
	// Projects + API keys (admin / bootstrap operations).
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, projectID string) (*Project, error)
	// UpdateProjectTier flips a project's tier without going through
	// Stripe. Founder-side admin lever. Does NOT touch the
	// Stripe customer/subscription columns; if a project was
	// previously on Pro and we manually drop to Hobby, the dangling
	// Stripe subscription is the founder's problem to cancel.
	//
	// expiresAt sets tier_expires_at (nil = never expires). Lazy
	// enforcement: when expiresAt has passed, HandleGetBilling
	// treats the tier as Hobby. Pass nil to make a permanent flip.
	UpdateProjectTier(ctx context.Context, projectID, tier string, expiresAt *time.Time) error
	// UpdateProjectName changes the human-readable display name on a
	// project row. Customer-driven: the rename endpoint on the
	// dashboard sends through here so SSO signups whose project
	// defaulted to "Default project" can pick something meaningful.
	// Trimming + length-bound (1-80 chars) is the API layer's job;
	// the store layer just writes whatever name it gets. Returns
	// ErrNotFound if the project_id does not exist.
	UpdateProjectName(ctx context.Context, projectID, name string) error
	// AddGrantedExecutions adds delta to the granted_executions
	// column atomically. Positive delta grants quota; negative delta
	// revokes a previous grant. Used for the early-customer 100K
	// promo and for goodwill credits.
	//
	// expiresAt overwrites granted_executions_expires_at (nil = never).
	// Single-expiration-per-project model: each call replaces the
	// existing expiration regardless of whether delta is positive or
	// negative.
	AddGrantedExecutions(ctx context.Context, projectID string, delta int64, expiresAt *time.Time) error

	// CreateAuditEvent + ListAuditEventsByProject power the Cloud
	// Team audit-log feature (v1). See store/audit_events.go
	// for contracts.
	//
	// SnapshotAuditEventsForClosedProject preserves the audit
	// history past project close (migration 031). Called from
	// HandleCloseAccount BEFORE DeleteProjectCascade.
	//
	// SearchClosedProjectAuditEvents serves admin-side forensic
	// lookups (R1 takeover, R2 customer support). At least one
	// of email or project_id is required in the filter.
	//
	// DeleteClosedProjectAuditEventsOlderThan purges closed-project
	// audit rows whose project_deleted_at < cutoff (SOC 2 /
	// financial-compliance 7-year retention cron). Only rows that
	// are tagged as belonging to a closed project are eligible
	// (project_deleted_at IS NOT NULL); live-project audit history
	// is untouched. Returns the number of rows deleted so the
	// scheduler can log prune volume.
	CreateAuditEvent(ctx context.Context, e *AuditEvent) error
	ListAuditEventsByProject(ctx context.Context, projectID string, limit int) ([]*AuditEvent, error)
	SnapshotAuditEventsForClosedProject(ctx context.Context, projectID, projectName string) error
	SearchClosedProjectAuditEvents(ctx context.Context, filter ClosedProjectAuditFilter) ([]*AuditEvent, error)
	DeleteClosedProjectAuditEventsOlderThan(ctx context.Context, cutoff time.Time) (deleted int64, err error)

	// InsertSystemEvent inserts one row into the system_events table
	// (, migration 050). Used by handlers.go to record
	// operational events (config_fallback, etc.) WITHOUT polluting
	// the customer-visible audit_events trail. Best-effort: callers
	// log on failure rather than fail the underlying business action.
	InsertSystemEvent(ctx context.Context, e *SystemEvent) error

	// Organization-level defaults (, migration 051). One row
	// per (org_id, default_key). The handler's resolver reads the
	// project-level value first, then the org default, then a
	// hardcoded constant. Known default_key values are validated at
	// the API layer; the store accepts any string.
	GetOrgDefaults(ctx context.Context, orgID string) (map[string]string, error)
	SetOrgDefault(ctx context.Context, orgID, defaultKey, valueJSON string) error

	// GetOrgConfigFallbackRollup aggregates system_events
	// action="config_fallback" rows across every project owned by
	// orgID over the recent window.
	GetOrgConfigFallbackRollup(ctx context.Context, orgID string, windowHours int) (OrgConfigFallbackRollup, error)
	// PurgeAuditEventsForClosedProject hard-deletes every audit row
	// owned by projectID (GDPR Article 17 right-to-be-forgotten).
	//
	// Refuses to operate on a project that still has LIVE audit rows
	// (project_deleted_at IS NULL on any row) by returning
	// ErrProjectStillActive. The handler maps that to HTTP 422 so an
	// operator who pasted the wrong project_id gets stopped instead
	// of accidentally wiping a paying customer's audit history.
	//
	// Returns the number of rows hard-deleted on success. Callers
	// are expected to record a meta-audit-event (action=
	// AuditAuditGDPRPurge) on the _admin system project recording
	// who fired the purge, when, and the original target project_id.
	PurgeAuditEventsForClosedProject(ctx context.Context, projectID string) (deleted int64, err error)

	// CreateBillingEvent + ListBillingEvents + GetBillingEvent +
	// ResolveBillingEvent back the Stripe webhook fraud/dunning
	// signal table. See store/billing_events.go for
	// contracts. The handler in api/billing.go inserts a row when
	// a charge.dispute.created or invoice.payment_failed webhook
	// arrives; the /admin/billing-events page reads from the same
	// table and stamps resolved_at when ops clears a signal.
	CreateBillingEvent(ctx context.Context, e *BillingEvent) error
	ListBillingEvents(ctx context.Context, filter BillingEventFilter) ([]*BillingEvent, error)
	GetBillingEvent(ctx context.Context, eventID string) (*BillingEvent, error)
	ResolveBillingEvent(ctx context.Context, eventID, resolvedBy, note string) error

	// CreateRequestLog + ListRequestLog + DeleteRequestLogOlderThan
	// back the persisted HTTP request audit table. One row
	// per authenticated Team-tier request. The request-log middleware
	// writes via CreateRequestLog after each authenticated request.
	// The admin "share recent use" report reads via ListRequestLog.
	// The daily request_log_retention_scheduler prunes via
	// DeleteRequestLogOlderThan to keep the table from growing
	// without bound (90-day window by default).
	CreateRequestLog(ctx context.Context, r *RequestLog) error
	ListRequestLog(ctx context.Context, filter RequestLogFilter) ([]*RequestLog, error)
	DeleteRequestLogOlderThan(ctx context.Context, cutoff time.Time) (int64, error)

	// GetAPIKeyByID + ListExecutionsByAPIKey serve the admin "Mark
	// key compromised" action. The endpoint checks scope via
	// GetAPIKeyByID before suspending the project; the recent-use
	// report combines ListExecutionsByAPIKey with ListRequestLog so
	// the customer sees both run-creation activity and arbitrary
	// URL traffic from the leaked key.
	GetAPIKeyByID(ctx context.Context, keyID string) (*APIKey, error)
	ListExecutionsByAPIKey(ctx context.Context, keyID string, t1, t2 time.Time, limit int) ([]*events.Execution, error)

	// CreateMagicLinkToken + GetMagicLinkTokenByHash + MarkMagicLinkTokenUsed
	// back the magic-link sign-in feature (commit 2). See
	// store/magic_link_tokens.go for contracts.
	CreateMagicLinkToken(ctx context.Context, t *MagicLinkToken) error
	GetMagicLinkTokenByHash(ctx context.Context, tokenHash string) (*MagicLinkToken, error)
	MarkMagicLinkTokenUsed(ctx context.Context, tokenID string) error

	// Email verification (pre-launch). IsEmailVerified is the gate
	// the customer-facing auth middleware checks on every request — if
	// it returns false the dashboard renders an "verify your email"
	// interstitial instead of customer pages. MarkEmailVerified is
	// called by (a) the email-link confirm handler when a raw-email
	// signup clicks the link in their welcome email, (b) the OAuth
	// callbacks where the IdP has already attested the email, and (c)
	// the migration backfill that grandfathers pre-launch accounts.
	// The verification-token methods power the one-click confirm
	// flow shipped in the welcome email. See store/email_verification.go.
	IsEmailVerified(ctx context.Context, email string) (bool, error)
	// GetEmailVerificationMethod returns the method label stored on
	// the verified_emails row, e.g. "email_link", "magic_link",
	// "sso_google", "sso_github", "grandfathered". Empty string +
	// nil error when the email has no verified_emails row (i.e. not
	// yet verified). The /me/email-verification-status endpoint
	// surfaces this so the dashboard can suppress the "VERIFIED"
	// chip on the settings page for SSO-attested accounts where the
	// label would be redundant.
	GetEmailVerificationMethod(ctx context.Context, email string) (string, error)
	MarkEmailVerified(ctx context.Context, email, method string) error
	CreateEmailVerificationToken(ctx context.Context, t *EmailVerificationToken) error
	GetEmailVerificationToken(ctx context.Context, token string) (*EmailVerificationToken, error)
	MarkEmailVerificationTokenUsed(ctx context.Context, token string) error

	// Session CRUD backs the cookie-based dashboard auth flow.
	// See store/sessions.go for contracts. The auth middleware calls
	// GetSessionByTokenHash + TouchSession on every dashboard request;
	// HandleRevokeAPIKey and HandleRemoveMember call
	// DeleteSessionsByUserID so revoking a key or kicking a member
	// immediately logs them out of every active browser tab.
	CreateSession(ctx context.Context, s *Session) error
	GetSessionByTokenHash(ctx context.Context, tokenHash string) (*Session, error)
	TouchSession(ctx context.Context, tokenHash string, lastUsedAt, newExpiresAt time.Time) error
	DeleteSession(ctx context.Context, tokenHash string) error
	DeleteSessionsByUserID(ctx context.Context, userID string) (int, error)
	ListSessionsForUser(ctx context.Context, userID string, now time.Time) ([]*Session, error)
	UpdateSessionProjectID(ctx context.Context, tokenHash, newProjectID string) error
	DeleteExpiredSessions(ctx context.Context, asOf time.Time) (int, error)

	// User TOTP, backup codes, pending 2FA tokens, and session
	// passed_2fa flag — back the customer-facing two-factor
	// authentication feature. See store/user_totp.go for the
	// per-method contracts and migrations/038_user_totp.sql for the
	// schema rationale. UpsertUserTOTP runs at enrollment;
	// GetUserTOTP runs on every dashboard auth check to determine
	// whether 2FA is enforced for the user; DeleteUserTOTP +
	// DeleteBackupCodesForUser run together on disable; backup-code
	// methods power the lost-phone fallback path. SetSessionPassed2FA
	// upgrades the session flag atomically at enrollment-time and at
	// successful /auth/2fa-verify so the customer is not kicked out
	// by their own action.
	UpsertUserTOTP(ctx context.Context, t *UserTOTP) error
	GetUserTOTP(ctx context.Context, userID string) (*UserTOTP, error)
	DeleteUserTOTP(ctx context.Context, userID string) error
	TouchUserTOTP(ctx context.Context, userID string, lastUsedAt time.Time) error
	CreateBackupCodes(ctx context.Context, codes []*BackupCode) error
	ConsumeBackupCode(ctx context.Context, userID, codeHash string, usedAt time.Time) error
	DeleteBackupCodesForUser(ctx context.Context, userID string) error
	CountUnusedBackupCodes(ctx context.Context, userID string) (int, error)
	CreatePending2FAToken(ctx context.Context, t *Pending2FAToken) error
	GetPending2FAToken(ctx context.Context, tokenHash string) (*Pending2FAToken, error)
	MarkPending2FATokenUsed(ctx context.Context, tokenHash string, usedAt time.Time) error
	DeleteExpiredPending2FATokens(ctx context.Context, asOf time.Time) (int, error)
	SetSessionPassed2FA(ctx context.Context, tokenHash string) error
}
