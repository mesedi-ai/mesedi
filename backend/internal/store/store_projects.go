package store

import (
	"context"
	"time"
)

// Project lifecycle and the things attached to a project: AI analyses,
// storage stats, billing state, API keys and webhooks.
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

type ProjectStore interface {
	// ListAnalyzedFailureGroupsByProject powers the per-project
	// failure-group breakdown on the admin AI analyses page.
	// Pass limit=0 for the default cap (200 rows).
	ListAnalyzedFailureGroupsByProject(
		ctx context.Context, projectID string, since time.Time, limit int,
	) ([]*FailureGroup, error)

	// CreateAnthropicCreditSnapshot + GetLatestAnthropicCreditSnapshot
	// back the manual-entry remaining-credit-balance surface.
	// See store/anthropic_credit.go for contracts. GetLatest returns
	// ErrNotFound when no snapshot has ever been recorded so the
	// admin endpoint can render an empty-state.
	CreateAnthropicCreditSnapshot(ctx context.Context, snap *AnthropicCreditSnapshot) error
	GetLatestAnthropicCreditSnapshot(ctx context.Context) (*AnthropicCreditSnapshot, error)

	// CreateAIAnalysis + ListAIAnalyses + GetAIAnalysesTotals power
	// the per-call accounting surface. One ai_analyses row
	// is written per Anthropic call (NOT per failure_group), so
	// re-runs preserve cost history. See store/ai_analyses.go for
	// contracts.
	CreateAIAnalysis(ctx context.Context, a *AIAnalysis) error
	ListAIAnalyses(ctx context.Context, limit, offset int) ([]*AIAnalysis, error)
	GetAIAnalysesTotals(ctx context.Context) (*AIAnalysesTotals, error)

	// AggregateFailureClassesForMonth + ListFailureClassAggregates
	// back the LinkedIn-trend anonymized counts. The
	// aggregate table survives account closure so historical
	// trend reports remain publishable without retaining
	// customer-identifying data. kAnonymity > 0 on List drops
	// rows where distinct_tenants_count < k.
	AggregateFailureClassesForMonth(
		ctx context.Context,
		period string,
		startInclusive time.Time,
		endExclusive time.Time,
	) (int, error)
	ListFailureClassAggregates(
		ctx context.Context, kAnonymity, limit int,
	) ([]*FailureClassAggregateRow, error)

	// ListAIAnalysesUsageByProject returns one row per project with
	// at least one AI root-cause analysis since the supplied time
	// (admin breakdown). Sorted by Count descending so the
	// heaviest users land at the top. Used by the founder dashboard
	// to spot heavy AI users for billing reconciliation and abuse
	// detection. Empty slice when no projects analyzed in the window.
	ListAIAnalysesUsageByProject(
		ctx context.Context, since time.Time,
	) ([]*AIAnalysesByProjectRow, error)

	// GetProjectStorageStats returns one row per project with counts
	// across the major child tables plus an EstimatedBytes total from
	// SUM(LENGTH()) over the large text columns. Used by the admin
	// dashboard's Storage page to spot heavy users before the
	// SQLite volume fills up. Founder-only, never expose this through
	// the customer API.
	GetProjectStorageStats(ctx context.Context) ([]*ProjectStorage, error)
	// DeleteProject permanently removes a project and (via the FK
	// ON DELETE CASCADE on every child table) all of its api keys,
	// executions, events, failure_groups, webhooks, and webhook
	// deliveries. Used by the admin DELETE endpoint to honor the
	// Privacy Policy's customer-data-deletion right.
	//
	// The caller (admin handler) is responsible for refusing the
	// deletion if a Stripe subscription is still active, the store
	// has no Stripe-awareness and will happily wipe a paying
	// customer.
	DeleteProject(ctx context.Context, projectID string) error
	// DeleteFailureGroupsByProject removes every failure_group row
	// owned by projectID and returns the number of rows deleted. Used
	// by the admin "reset demo project" endpoint so the next
	// detector pass re-creates each group as isNew=true and the
	// webhook dispatcher fires fresh failure_group.created events.
	// Executions and events are NOT touched; only the grouping
	// summary rows are wiped.
	DeleteFailureGroupsByProject(ctx context.Context, projectID string) (int64, error)
	// ListAllProjects returns every project in the database with
	// aggregate activity stats (last execution time, total execution
	// count) joined in. Used by the founder-side admin dashboard
	// NEVER expose this through the customer-facing API.
	// Ordered by created_at DESC so newest signups appear first.
	ListAllProjects(ctx context.Context) ([]*AdminProjectRow, error)
	// ListProjectsByOwner returns every project owned by ownerUserID,
	// ordered by created_at ASC (oldest first, which mirrors how the
	// org rollup dashboard wants to lay them out left-to-right). Used
	// by the customer-facing /me/rollup endpoint to discover
	// the set of project_ids that make up one tenant in v0.1 (where
	// "tenant" is defined as a single user account; the proper
	// organizations table comes later when multi-seat enterprises
	// onboard).
	//
	// Returns an empty slice (not ErrNotFound) when ownerUserID has no
	// projects, so callers can blanket-aggregate without special-casing.
	ListProjectsByOwner(ctx context.Context, ownerUserID string) ([]*Project, error)
	// Billing (, Stripe integration).
	// UpdateProjectBilling sets the tier, Stripe identifiers, and
	// current period bounds in one call. Called from the Stripe
	// webhook handler after checkout.session.completed and from
	// customer.subscription.updated. Period start/end may be nil to
	// clear (e.g., on subscription cancellation).
	UpdateProjectBilling(ctx context.Context, projectID, tier, stripeCustomerID, stripeSubscriptionID string, periodStart, periodEnd *time.Time) error
	// GetProjectByStripeCustomerID resolves a Stripe customer id back
	// to the owning project for webhook event handling. Returns
	// ErrNotFound if no project is associated with that customer.
	GetProjectByStripeCustomerID(ctx context.Context, stripeCustomerID string) (*Project, error)
	// GetMostRecentProjectByOwnerEmail resolves a verified email
	// address back to the customer's most recently-created project.
	// Used by the /signin handler after SSO or magic-link
	// proves email ownership; the dashboard server hands us the
	// email, we hand back the project the new session-grade key will
	// belong to. Returns ErrNotFound when the email has never signed
	// up (callers surface a "no account for that email" UX).
	//
	// "Most recent" matters because the v0.1 schema does not enforce
	// email uniqueness on projects (a user could call POST /signup
	// twice with the same email and get two projects). Picking the
	// latest matches the customer's mental model of "log me into my
	// dashboard" -- the most recent project is the one they almost
	// certainly mean. When session-cookie auth ships in this
	// becomes "list all projects, let the user pick", but for the
	// API-key-as-session expedient one project per signin is fine.
	GetMostRecentProjectByOwnerEmail(ctx context.Context, email string) (*Project, error)
	// UpdateProjectBillingCap sets the project's monthly overage spend
	// cap. Customers configure this from /app/billing to bound their
	// monthly exposure on Hobby + Team tiers. Pass 0 to clear
	// the cap back to the constants-default ($200 for Hobby today;
	// the hobby billing scheduler still respects the project value if
	// it's nonzero, so 0 effectively means "use default constant").
	UpdateProjectBillingCap(ctx context.Context, projectID string, capUSD float64) error
	// DeleteProjectCascade hard-deletes a project and every dependent
	// row. Wired up for the customer-facing "Close account" danger-zone
	// flow on /app/settings. Deletion order respects FK
	// constraints: events -> executions -> failure_groups -> webhooks
	// (+ deliveries) -> api_keys -> project_settings -> billing_grant
	// + class_severities -> webhook_severity_configs ... ending with
	// projects. Implementations may run this in a single transaction
	// where the store supports it. Returns ErrNotFound if the project
	// doesn't exist.
	DeleteProjectCascade(ctx context.Context, projectID string) error
	// IncrementExecutionsThisPeriod atomically increments the per-
	// period execution counter on a project. Called from
	// HandleCreateExecution on each successful POST /executions. Best-
	// effort: a failure here logs a warning but does not fail the
	// ingest path.
	IncrementExecutionsThisPeriod(ctx context.Context, projectID string) error
	// ResetExecutionsThisPeriod zeroes the counter and updates the
	// period bounds. Called when a new billing period starts (lazy
	// reset, triggered by webhook or by a counter-read handler
	// noticing the current period has ended).
	ResetExecutionsThisPeriod(ctx context.Context, projectID string, periodStart, periodEnd time.Time) error
	// GetDailyExecutionCounts returns one row per UTC day of executions
	// in the given project over the given window, in ascending date
	// order. Days with zero executions are NOT included in the result
	// (the dashboard fills gaps client-side). Used by the billing
	// page's usage chart.
	GetDailyExecutionCounts(ctx context.Context, projectID string, since, until time.Time) ([]DailyExecutionCount, error)
	CreateAPIKey(ctx context.Context, k *APIKey) error
	GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error)
	TouchAPIKey(ctx context.Context, keyID string) error
	// ListAPIKeysForProject returns all keys (minus the hash) for a
	// project, sorted by created_at DESC. Used by the dashboard's
	// settings → API keys page.
	ListAPIKeysForProject(ctx context.Context, projectID string) ([]*APIKey, error)
	// ListAllAPIKeys returns every API key in the system (minus the
	// hash), sorted by created_at DESC. Admin-only: used by the
	// /admin/api-keys page to surface keys across all projects
	// (including the synthetic _admin project that holds admin-scope
	// keys).
	ListAllAPIKeys(ctx context.Context) ([]*APIKey, error)
	// DeleteAPIKey revokes (hard-deletes) an API key by id, but ONLY
	// if it belongs to the given project. Returns ErrNotFound if the
	// key doesn't exist or belongs to a different project, protects
	// against cross-tenant deletion via id-guessing.
	DeleteAPIKey(ctx context.Context, keyID, projectID string) error
	// DeleteAPIKeyByID hard-deletes any API key with no project_id
	// guard. Admin-only: used by /admin/api-keys to revoke any key
	// (admin or customer scope, any project).
	DeleteAPIKeyByID(ctx context.Context, keyID string) error
	// DeleteAPIKeysByUserID hard-deletes every API key whose user_id
	// matches. Used when an org admin removes a member so the removed
	// member's existing keys stop working immediately. Returns
	// the number of keys deleted; the caller logs this as part of the
	// member-removed audit trail.
	DeleteAPIKeysByUserID(ctx context.Context, userID string) (int, error)

	// Project webhooks (failure-class escalation, ).
	// CreateProjectWebhook persists a new webhook configuration. The
	// caller is responsible for generating WebhookID + Secret. CreatedAt
	// is set if zero.
	CreateProjectWebhook(ctx context.Context, wh *ProjectWebhook) error
	// ListProjectWebhooksForProject returns every webhook (enabled and
	// disabled) for a project, sorted by CreatedAt DESC. The Secret
	// field is intentionally cleared on the returned values, the
	// secret is shown ONLY at creation time.
	ListProjectWebhooksForProject(ctx context.Context, projectID string) ([]*ProjectWebhook, error)
	// ListEnabledProjectWebhooks returns only the enabled webhooks for
	// a project, WITH the Secret populated. Used by the slice-2
	// dispatcher to sign outbound payloads. Never call this from a
	// handler that returns the result to a client.
	ListEnabledProjectWebhooks(ctx context.Context, projectID string) ([]*ProjectWebhook, error)
	// DeleteProjectWebhook hard-deletes a webhook by id, but ONLY if
	// it belongs to the given project. Returns ErrNotFound if the
	// webhook doesn't exist or belongs to a different project.
	DeleteProjectWebhook(ctx context.Context, webhookID, projectID string) error
	// GetProjectWebhook returns one webhook by id WITH the Secret
	// populated. Used by the test-trigger handler to look up the
	// secret before dispatching. Returns ErrNotFound if absent or if
	// the webhook belongs to a different project.
	GetProjectWebhook(ctx context.Context, webhookID, projectID string) (*ProjectWebhook, error)

	// Webhook recurrence state.
	// GetWebhookRecurrenceLastFired returns the last-fired timestamp
	// for (webhook, group). Returns ErrNotFound when no row exists yet
	// for the pair, which the dispatcher treats as "window elapsed" so
	// the first recurrence ping always goes out. Used only by the
	// throttled recurrence path; off / every_event paths skip this
	// lookup entirely.
	GetWebhookRecurrenceLastFired(ctx context.Context, webhookID, groupID string) (time.Time, error)
	// UpsertWebhookRecurrenceLastFired records the timestamp of the
	// most recent fire for (webhook, group). Called from the dispatcher
	// on every successful fire so the next throttle check observes the
	// right baseline.
	UpsertWebhookRecurrenceLastFired(ctx context.Context, webhookID, groupID string, t time.Time) error

	// Webhook delivery log (slice 2 dispatcher).
	// RecordWebhookDelivery persists one delivery attempt row. The
	// caller is responsible for filling in WebhookID, ProjectID,
	// Status, Attempt, and any of the optional fields; DeliveryID
	// and CreatedAt are set here if zero.
	RecordWebhookDelivery(ctx context.Context, d *WebhookDelivery) error
	// ListDeliveriesForWebhook returns the most recent N deliveries
	// for a webhook, sorted by created_at DESC.
	ListDeliveriesForWebhook(ctx context.Context, webhookID string, limit int) ([]*WebhookDelivery, error)
}
