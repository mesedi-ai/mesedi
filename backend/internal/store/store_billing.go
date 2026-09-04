package store

import (
	"context"
	"time"
)

// The Hobby billing tick and abuse signals. Grouped together because
// both are scheduler-driven lifecycle actions taken ON a project rather
// than requests made on its behalf.
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

type BillingLifecycleStore interface {
	// ListProjectsForHobbyBillingTick returns every Hobby-tier
	// project the HobbyBillingScheduler needs to consider this tick:
	// projects whose current_period_end <= now (period rolled over,
	// candidate for charging + advancing bounds) AND projects whose
	// current_period_start IS NULL (legacy unbootstrapped rows that
	// need their first billing window assigned). Team and
	// Enterprise tiers are excluded because their billing runs
	// through Stripe subscriptions, not this scheduler.
	ListProjectsForHobbyBillingTick(ctx context.Context, now time.Time) ([]*Project, error)

	// UpdateHobbyBillingState records the result of a Hobby billing
	// charge attempt. On success: sets hobby_billing_last_attempt_at
	// to attemptAt AND resets hobby_billing_consecutive_failures to 0.
	// On failure: sets hobby_billing_last_attempt_at to attemptAt AND
	// increments hobby_billing_consecutive_failures by 1. The
	// scheduler reads these columns next tick to enforce the every-
	// other-day retry cadence and to trigger auto-downgrade after
	// the configured failure ceiling.
	UpdateHobbyBillingState(ctx context.Context, projectID string, attemptAt time.Time, success bool) error

	// DetachHobbyCardForBillingFailure clears the project's
	// stripe_customer_id, hobby_billing_consecutive_failures, and
	// hobby_billing_last_attempt_at in one atomic write. Called by
	// the HobbyBillingScheduler when consecutive failures cross the
	// configured ceiling: the saved payment method is treated as
	// dead, the project reverts to the hard-capped "no card on
	// file" state, and the customer must attach a new card via
	// Stripe Checkout to resume billable usage.
	DetachHobbyCardForBillingFailure(ctx context.Context, projectID string) error

	// MarkCardDetached is the customer-initiated card-removal store
	// method. It sets card_on_file = FALSE on the project
	// WITHOUT nulling stripe_customer_id. Used by the
	// /billing/payment-method/remove handler so:
	//   - Hobby projects: keep the Stripe customer record for a
	//     future re-add (no duplicate customers).
	//   - Team projects: keep the Stripe customer record so the
	//     active subscription stays addressable and the
	//     customer.subscription.deleted webhook can still find the
	//     project at period-end auto-downgrade.
	// Hobby billing scheduler counters (consecutive_failures,
	// last_attempt_at) are NOT reset here; those reset on success
	// or on the auto-detach path. ErrNotFound if no row matches.
	MarkCardDetached(ctx context.Context, projectID string) error

	// MarkCardAttached is the inverse: set card_on_file = TRUE.
	// Called from handleSetupIntentSucceeded when a new payment
	// method is attached to the customer (covers both first-time
	// attach and re-attach after a prior removal). ErrNotFound if
	// no row matches.
	MarkCardAttached(ctx context.Context, projectID string) error

	// Abuse signals + project suspension.
	//
	// CreateAbuseSignal inserts a new row. detected_at is set by the
	// caller; the worker controls notified_at / suspended_at /
	// resolved_at lifecycle transitions via the Mark... methods below.
	CreateAbuseSignal(ctx context.Context, sig *AbuseSignal) error
	// ListAbuseSignals returns signals sorted by detected_at DESC.
	// When unresolvedOnly is true the resolved column must be NULL.
	// limit caps results (0 means no cap, but callers should bound).
	ListAbuseSignals(ctx context.Context, unresolvedOnly bool, limit int) ([]*AbuseSignal, error)
	// GetAbuseSignal fetches one by id; ErrNotFound when absent.
	GetAbuseSignal(ctx context.Context, signalID string) (*AbuseSignal, error)
	// MarkAbuseSignalNotified records the time the 24h-warning email
	// was sent. The background worker calls this immediately after a
	// successful email dispatch so notification idempotency holds
	// across worker restarts.
	MarkAbuseSignalNotified(ctx context.Context, signalID string, notifiedAt time.Time) error
	// MarkAbuseSignalSuspended records the auto-suspension transition
	// (notified_at + 24h with no human resolution). The same call
	// MUST also flip projects.suspended_at; implementations should
	// either run both updates in a single transaction or document the
	// best-effort ordering. SQLite implementation runs them in a
	// transaction.
	MarkAbuseSignalSuspended(ctx context.Context, signalID, projectID, reason string, suspendedAt time.Time) error
	// ResolveAbuseSignal closes out a signal with operator metadata.
	// If the project was suspended due to this signal, the caller is
	// responsible for also calling UnsuspendProject to reactivate.
	ResolveAbuseSignal(ctx context.Context, signalID, resolvedBy, note string, resolvedAt time.Time) error
	// UnsuspendProject clears suspended_at + suspension_reason. Used
	// when an operator resolves a signal that triggered suspension and
	// chooses to reactivate the project.
	UnsuspendProject(ctx context.Context, projectID string) error
	// IsProjectSuspended is a fast read for the auth middleware to
	// reject authenticated requests on suspended projects. Returns
	// (false, "", nil) for active projects, (true, reason, nil) for
	// suspended ones.
	IsProjectSuspended(ctx context.Context, projectID string) (bool, string, error)
}
