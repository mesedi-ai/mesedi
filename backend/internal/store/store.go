// Package store defines the persistence interface for Mesedi and
// provides concrete implementations.
//
// The Store interface lets the rest of the codebase work against an
// abstract data layer regardless of whether SQLite (local dev) or
// Postgres (eventual production) is the underlying engine. Adding the
// Postgres implementation in a future slice is a drop-in.
package store

import (
	"context"
	"time"
)

// Project is one customer's top-level container for agent telemetry.
//
// Billing fields (Tier, StripeCustomerID, StripeSubscriptionID,
// CurrentPeriodStart, CurrentPeriodEnd, ExecutionsThisPeriod) were
// added in migration 006 as part of the Stripe integration slice
// . For existing projects created before that migration ran,
// Tier defaults to "hobby" and the Stripe identifiers are empty
// until a Checkout completes.
type Project struct {
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"owner_user_id,omitempty"`
	OwnerEmail  string    `json:"owner_email,omitempty"`
	CreatedAt   time.Time `json:"created_at"`

	// Tier: "hobby" | "team" | "enterprise". Always populated;
	// migration 006 backfilled existing rows to "hobby", and migration
	// 019 renamed any "pro" rows to "team" so the database speaks the
	// post-rewrite pricing vocabulary. The API layer also calls
	// normalizeTier() defensively in case a future out-of-band SQL
	// fix re-inserts the legacy "pro" string.
	Tier string `json:"tier"`
	// Stripe identifiers, populated after a successful Checkout.
	// Empty for hobby-tier projects that never upgraded.
	StripeCustomerID     string `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID string `json:"stripe_subscription_id,omitempty"`
	// CurrentPeriodStart / CurrentPeriodEnd mirror the Stripe
	// subscription's billing period so the dashboard can render the
	// "X executions used of N this month" line without a Stripe API
	// round-trip on every page load. Updated on
	// customer.subscription.updated and invoice.paid webhook events.
	CurrentPeriodStart *time.Time `json:"current_period_start,omitempty"`
	CurrentPeriodEnd   *time.Time `json:"current_period_end,omitempty"`
	// ExecutionsThisPeriod is the rolling counter incremented on each
	// successful POST /executions. Reset to zero on each new billing
	// period (lazy reset: handlers compare CurrentPeriodEnd to now and
	// roll over before incrementing).
	ExecutionsThisPeriod int64 `json:"executions_this_period"`
	// GrantedExecutions is the admin-granted extra quota on top of the
	// tier's base allowance (migration 007). Additive across the
	// lifetime of the project, does NOT reset at period rollover.
	// May be negative if the admin revoked a previous grant; effective
	// quota math floors at zero, but the column itself is signed for
	// auditability.
	GrantedExecutions int64 `json:"granted_executions"`
	// GrantedExecutionsExpiresAt is the moment the grant stops counting
	// (migration 008). Nil means "never expires". Enforcement is lazy:
	// billing.go's read handler compares this to now and treats the
	// grant as zero when expired. The column value itself stays.
	GrantedExecutionsExpiresAt *time.Time `json:"granted_executions_expires_at,omitempty"`
	// TierExpiresAt is the moment an admin-flipped tier reverts to
	// Hobby (migration 008). Nil means the tier doesn't auto-revert
	// (the default for paid Stripe subscriptions and permanent admin
	// flips). Enforced lazily in HandleGetBilling.
	TierExpiresAt *time.Time `json:"tier_expires_at,omitempty"`
	// BillingCapUSD is the monthly hard cap on overage spend
	// (migration 019). When the computed overage cost crosses this
	// number, the ingest path silent-drops new executions with a 402
	// "billing cap reached" response. Default 200 across all tiers;
	// future slice lets customers configure it.
	//
	// Overage cost itself is NOT stored; it's computed on the fly at
	// every read site as max(0, executions_this_period - included)
	// times the tier's per-execution rate. Single source of truth.
	BillingCapUSD float64 `json:"billing_cap_usd"`

	// HobbyBillingLastAttemptAt is the timestamp of the most recent
	// charge attempt made by the HobbyBillingScheduler against this
	// project's saved payment method (migration 021). Nil when no
	// attempt has been made yet (the default for every existing
	// row). Drives the every-other-day retry cadence: the scheduler
	// only attempts a fresh charge when this is nil or > 48 hours
	// in the past.
	HobbyBillingLastAttemptAt *time.Time `json:"hobby_billing_last_attempt_at,omitempty"`

	// HobbyBillingConsecutiveFailures counts how many charge
	// attempts in a row have failed against the saved payment
	// method (migration 021). Increments on each failed Stripe
	// PaymentIntent, resets to zero on a successful charge. When
	// it crosses 5 the scheduler auto-detaches the saved card
	// (clears StripeCustomerID), reverting the project to the
	// hard-capped "no card" state until the customer attaches a
	// new card via Stripe Checkout.
	HobbyBillingConsecutiveFailures int `json:"hobby_billing_consecutive_failures"`

	// CardOnFile distinguishes "this project has a Stripe customer
	// record" from "this project has a card we can charge"
	// (migration 022, ). Before this column the two concepts
	// were conflated via StripeCustomerID == "" which broke Team's
	// customer-initiated card removal: we want to keep the customer
	// linkage so the active subscription stays addressable, but
	// signal "no card" to the hard-cap path.
	//
	// True  → the saved card can be charged off-session. Default
	//         for any project that has ever had a card attached.
	// False → no card to charge. Ingest path hard-caps at the
	//         included quota (capExceeded), AI analysis path
	//         refuses overage requests, Hobby scheduler skips
	//         charge attempts. Default for brand-new projects
	//         until the first Setup Intent succeeds.
	CardOnFile bool `json:"card_on_file"`
}

// TenantBudgetCeiling is the persisted configuration that powers
// the Enterprise-tier monthly budget ceiling feature.
//
// v0.1 tenant = owner_user_id (single-user account). The scheduler
// evaluates each ceiling row every 5 minutes by summing
// estimated_cost_usd across all projects owned by the same
// owner_user_id since the start of the current month, and compares
// the sum to MonthlyCeilingUSD.
//
// BreachAction is "warn" (notify only) or "halt" (notify + auto-halt
// across the tenant's active executions). v0.1 only emits
// notifications regardless of value; v1.1 wires in the halt fan-out.
//
// BreachedAt is set by the scheduler the first time burn crosses
// the ceiling within a calendar month, and cleared at month rollover.
// Notification dispatch reads this column to dedupe: we send one
// email + one webhook per breach event, not one every 5 minutes for
// the rest of the month.
type TenantBudgetCeiling struct {
	OwnerUserID       string     `json:"owner_user_id"`
	MonthlyCeilingUSD float64    `json:"monthly_ceiling_usd"`
	BreachAction      string     `json:"breach_action"`                // "warn" | "halt"
	NotifyEmail       string     `json:"notify_email,omitempty"`       // optional override; falls back to project owner_email
	NotifyWebhookURL  string     `json:"notify_webhook_url,omitempty"` // optional; if set, dispatcher posts a JSON payload
	CreatedAt         time.Time  `json:"created_at"`
	UpdatedAt         time.Time  `json:"updated_at"`
	LastEvaluatedAt   *time.Time `json:"last_evaluated_at,omitempty"`
	BreachedAt        *time.Time `json:"breached_at,omitempty"`
}

// AdminProjectRow is one row in the founder-side admin dashboard's
// project list. Extends the bare Project with activity aggregates
// (last_activity_at, total_executions) computed via LEFT JOIN to the
// executions table. Returned only via the admin-token-gated
// /admin/projects endpoint; never reachable from the customer dashboard.
type AdminProjectRow struct {
	// Core project identity (same fields as Project).
	ProjectID                  string     `json:"project_id"`
	Name                       string     `json:"name"`
	OwnerEmail                 string     `json:"owner_email,omitempty"`
	CreatedAt                  time.Time  `json:"created_at"`
	Tier                       string     `json:"tier"`
	StripeCustomerID           string     `json:"stripe_customer_id,omitempty"`
	StripeSubscriptionID       string     `json:"stripe_subscription_id,omitempty"`
	CurrentPeriodStart         *time.Time `json:"current_period_start,omitempty"`
	CurrentPeriodEnd           *time.Time `json:"current_period_end,omitempty"`
	ExecutionsThisPeriod       int64      `json:"executions_this_period"`
	GrantedExecutions          int64      `json:"granted_executions"`
	GrantedExecutionsExpiresAt *time.Time `json:"granted_executions_expires_at,omitempty"`
	TierExpiresAt              *time.Time `json:"tier_expires_at,omitempty"`
	// Activity aggregates joined from executions table. Nil/zero when
	// the project has never produced an execution (e.g., signed up but
	// never integrated the SDK).
	LastActivityAt  *time.Time `json:"last_activity_at,omitempty"`
	TotalExecutions int64      `json:"total_executions"`
}

// ProjectStorage is one row of the admin storage view's per-project
// breakdown. EstimatedBytes is computed from SUM(LENGTH()) over the
// large text columns (events.payload, executions.input_summary,
// executions.output_summary, executions.crash_signature), close
// enough to disk usage at our scale, doesn't require dbstat.
type ProjectStorage struct {
	ProjectID         string `json:"project_id"`
	Name              string `json:"name"`
	OwnerEmail        string `json:"owner_email,omitempty"`
	Tier              string `json:"tier"`
	Executions        int64  `json:"executions"`
	Events            int64  `json:"events"`
	FailureGroups     int64  `json:"failure_groups"`
	WebhookDeliveries int64  `json:"webhook_deliveries"`
	EstimatedBytes    int64  `json:"estimated_bytes"`
}

// AIAnalysesByProjectRow is one row of the admin AI-analyses
// breakdown view. Aggregates failure_groups.analyzed_at
// counts per project so the founder dashboard can rank tenants by
// AI usage in the current window. The view is computed live (no
// rollup table) since AI analyses are sparse and the join is small.
type AIAnalysesByProjectRow struct {
	ProjectID  string `json:"project_id"`
	Name       string `json:"name"`
	OwnerEmail string `json:"owner_email,omitempty"`
	Tier       string `json:"tier"`
	TenantID   string `json:"tenant_id,omitempty"`
	Count      int    `json:"count"`
	// FailureClasses is the distinct list of failure_class slugs the
	// project ran analyses against in this window (filter
	// chips). Order is whatever the DB returned; the dashboard sorts
	// alphabetically before rendering. Empty when no analyses ran.
	FailureClasses []string `json:"failure_classes,omitempty"`
}

// DailyExecutionCount is one bucket of an execution-usage time series.
// Used by the billing page's usage chart. The Date is in UTC, midnight,
// inclusive (so a row with Date=2026-05-23 covers all executions where
// started_at falls between 2026-05-23T00:00:00Z and 2026-05-24T00:00:00Z).
type DailyExecutionCount struct {
	Date  time.Time `json:"date"`
	Count int64     `json:"count"`
}

// APIKey is an authentication credential bound to a project. The raw
// key is never persisted, only the SHA-256 hash. The prefix is a
// non-secret display string for the developer to identify the key.
type APIKey struct {
	KeyID      string     `json:"key_id"`
	ProjectID  string     `json:"project_id"`
	KeyHash    string     `json:"-"` // never serialized to clients
	KeyPrefix  string     `json:"key_prefix"`
	Name       string     `json:"name,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
	// UserID is the org-member identity this key authenticates as
	//. Required for per-member role enforcement. Migration 014
	// added the column nullable so pre-014 keys still work; auth
	// middleware falls back to project.owner_user_id when UserID is
	// empty so existing customer integrations don't break overnight.
	UserID string `json:"user_id,omitempty"`
	// Scope is either "customer" (default, project-scoped) or
	// "admin" (privileged, gates the /admin/* surface). Added by
	// migration 015. Admin keys carry project_id=APIKeyAdminProjectID
	// ("_admin") so they participate in the same FK constraint as
	// customer keys without needing a separate table.
	Scope string `json:"scope,omitempty"`
	// ExpiresAt is the optional cutoff. Empty == never expires.
	// Non-empty values are RFC3339Nano UTC. The auth middleware
	// rejects an arriving request past ExpiresAt identically to a
	// revoked / missing key.
	ExpiresAt string `json:"expires_at,omitempty"`
	// Source records HOW this key was minted. Added by migration 028
	// to discriminate long-lived "customer-visible" credentials from
	// short-lived "session-grade" credentials minted by SSO login and
	// magic-link sign-in. Listings of customer-facing keys
	// filter session-grade rows OUT so the /admin/api-keys page only
	// surfaces keys the user consciously created.
	Source string `json:"source,omitempty"`
	// Role is the explicit per-key authorization override. When set
	// (admin/write/read), the api_key_role_resolver returns this
	// value verbatim instead of falling back to the minting user's
	// org role. Added by migration 056 so an admin can mint scoped
	// credentials (e.g. a read-only key for a monitoring script or
	// a write key for a CI pipeline) without having to invite a new
	// user to hold the reduced role. Empty preserves legacy
	// behavior, every pre-056 key resolves via user role.
	Role string `json:"role,omitempty"`
}

// APIKeyScopeCustomer / APIKeyScopeAdmin are the only legal values of
// APIKey.Scope. The auth middleware compares against these constants
// to decide whether a request can reach the /admin/* surface.
const (
	APIKeyScopeCustomer = "customer"
	APIKeyScopeAdmin    = "admin"
)

// APIKeySource* enumerate the four legal values of APIKey.Source. The
// "manual" default covers keys that predate the source column
// (every backfilled row is treated as long-lived); "signup" tags the
// first key minted on POST /signup; "sso_login" and "magic_link" tag
// short-lived session credentials. The session-grade values are
// filtered out of customer-facing listings (see
// IsAPIKeySourceSessionGrade) and out of admin listings by default.
const (
	APIKeySourceManual    = "manual"
	APIKeySourceSignup    = "signup"
	APIKeySourceSSOLogin  = "sso_login"
	APIKeySourceMagicLink = "magic_link"
)

// IsAPIKeySourceSessionGrade reports whether the given source value
// identifies a key that should be HIDDEN from customer-facing key
// lists. Session-grade keys are minted by /signin (SSO callback) and
// magic-link verify; they expire after a short window (7 days from
// mint) and exist purely to keep the dashboard signed in. They must
// never surface in the /admin/api-keys UI because (a) the customer
// did not consciously create them and (b) clicking "revoke" on one
// would silently log them out, which is a confusing UX.
func IsAPIKeySourceSessionGrade(source string) bool {
	return source == APIKeySourceSSOLogin || source == APIKeySourceMagicLink
}

// APIKeyLoginExpiryDays is the lifetime (in days) of session-grade
// keys minted by SSO login and magic-link verify. Chosen short so a
// leaked localStorage entry stops being a valid bearer quickly; long
// enough that a customer signing in on Monday does not get bounced
// out before Friday. Aligned with industry-standard SaaS dashboard
// session lifetimes.
const APIKeyLoginExpiryDays = 7

// WebhookDeliveryListLimitMax caps the result-set size for
// ListDeliveriesForWebhook on both SQLite and Postgres implementations.
// User-controllable `limit` is clamped inside the function before any
// allocation, so a request with `?limit=999999999` cannot drive an
// unbounded slice grow. CodeQL's go/uncontrolled-allocation-size query
// (alerts ) needs to see the constant ceiling at the
// make() site, hence the explicit constant used in both stores.
const WebhookDeliveryListLimitMax = 500

// APIKeyAdminProjectID is the project_id all admin-scope keys share.
// Auto-bootstrapped at startup so the projects FK on api_keys still
// holds for admin keys without needing to relax the constraint.
const APIKeyAdminProjectID = "_admin"

// ProjectWebhook is a per-project webhook configuration for failure-class
// escalation. When a failure_group fires (slice 2 dispatcher), Mesedi
// looks up every enabled webhook for the project, filters by
// EnabledClasses, and POSTs a signed payload to each matching URL.
//
// Secret is a shared symmetric HMAC key returned to the caller ONCE at
// creation time; the dispatcher uses it to sign outbound payloads with
// an X-Mesedi-Signature header the receiver verifies. Stored
// plaintext for local-dev; production deployments would encrypt the
// column at rest with KMS.
//
// EnabledClasses is a JSON-encoded array of failure-class names. Empty
// or nil means "all classes", the common case. Validation that
// supplied class names match the FailureClass* constants happens at
// the handler layer.
type ProjectWebhook struct {
	WebhookID string `json:"webhook_id"`
	ProjectID string `json:"project_id"`
	Name      string `json:"name,omitempty"`
	URL       string `json:"url"`
	Secret    string `json:"-"` // never returned in list responses
	// AuthToken is a customer-provided receiver-side auth value used
	// when the receiver has its own auth scheme instead of Mesedi's
	// HMAC signature. Currently used only for PagerDuty (the value
	// is their integration key / routing_key, ~32 chars). Never
	// returned in list responses. Empty for Slack / Discord / generic
	// receivers; the dispatcher falls back to HMAC signing in those
	// cases. Added by migration 055.
	AuthToken      string    `json:"-"`
	EnabledClasses []string  `json:"enabled_classes"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	// SeverityFilter is the comma-separated subset of severities this
	// webhook fires on. Empty string = "fire on every severity"
	// (backward compatible with webhooks). The dispatcher
	// parses via severity.ParseFilter and skips delivery when the
	// event severity is not in the filter.
	SeverityFilter string `json:"severity_filter"`
	// RecurrenceMode controls whether the webhook fires on recurrences
	// of an existing failure group. One of:
	//   "off"        , only fire on new failure groups (default; matches
	//                   behavior for every legacy webhook).
	//   "every_event", fire on every recurrence with no throttling.
	//   "throttled"  , fire on the first recurrence in each rolling
	//                   window of RecurrenceWindowSeconds, suppress
	//                   further recurrences until the window elapses.
	// The dispatcher treats unknown values as "off" so a malformed
	// row never spams or silently breaks a customer's pipeline.
	RecurrenceMode string `json:"recurrence_mode"`
	// RecurrenceWindowSeconds is non-zero only when RecurrenceMode is
	// "throttled". The dispatcher floors any value below 60 to 60 so
	// a misconfigured row cannot turn into an every_event firehose by
	// accident.
	RecurrenceWindowSeconds int `json:"recurrence_window_seconds"`
}

// WebhookRecurrenceConsts captures the allowed values of
// ProjectWebhook.RecurrenceMode in one place so handler validation
// and dispatcher branch logic stay in sync. The default for any
// new or legacy row is RecurrenceModeOff.
const (
	RecurrenceModeOff        = "off"
	RecurrenceModeEveryEvent = "every_event"
	RecurrenceModeThrottled  = "throttled"

	// RecurrenceMinWindowSeconds is the floor the dispatcher applies
	// when a "throttled" row carries a window below this value. Below
	// 60s, "throttled" approaches "every_event" but with extra DB
	// round-trips per fire, so we collapse the two semantically.
	RecurrenceMinWindowSeconds = 60
)

// WebhookRecurrenceState is one row of the webhook_recurrence_state
// table, the last time a given webhook fired for a specific failure
// group. The dispatcher reads this row to decide whether the
// throttled-mode window has elapsed; upserts it on every successful
// fire so the next decision has the right baseline.
//
// Rows are scoped to (webhook_id, group_id) and cascade-deleted with
// the parent webhook. There is no separate retention job; rows are
// cheap (one timestamp) and bounded by the lifetime of the parent
// webhook's group population.
type WebhookRecurrenceState struct {
	WebhookID   string    `json:"webhook_id"`
	GroupID     string    `json:"group_id"`
	LastFiredAt time.Time `json:"last_fired_at"`
}

// ProjectClassSeverity is the per-project override of the hardcoded
// failure-class-to-severity default map. Absent rows fall back
// to severity.Default(failureClass) in Go code. Customers set
// overrides via PUT /me/class-severities/{class} on the dashboard
// settings page.
type ProjectClassSeverity struct {
	ProjectID    string    `json:"project_id"`
	FailureClass string    `json:"failure_class"`
	Severity     string    `json:"severity"` // "critical" | "warning" | "info"
	UpdatedAt    time.Time `json:"updated_at"`
}

// ProjectRetention is one row returned by ListProjectsForRetention
// . Carries just the project_id + retention_days the
// scheduler needs to compute the delete cutoff. Skips rows where
// retention_days IS NULL (indefinite retention).
type ProjectRetention struct {
	ProjectID     string `json:"project_id"`
	RetentionDays int    `json:"retention_days"`
}

// Organization is the multi-seat tenant unit. One org has one
// or more organization_members; one project is owned by exactly one
// org via projects.tenant_id. Replaces the implicit owner_user_id
// tenant model used by and .
type Organization struct {
	OrgID           string    `json:"org_id"`
	Name            string    `json:"name"`
	CreatedByUserID string    `json:"created_by_user_id"`
	CreatedAt       time.Time `json:"created_at"`
	UpdatedAt       time.Time `json:"updated_at"`
}

// OrganizationMember is the (org, user, role) join row. Role is one
// of "admin", "write", "read"; backend handlers consult the role to
// authorize sensitive actions (admin-only: invite, remove member,
// billing changes; write or admin: create webhooks, set retention;
// any role: read).
//
// Email is captured at member-add time so the dashboard members
// list can render meaningfully BEFORE the invited user has signed
// in for the first time. Refreshed to the user's current email
// whenever they next authenticate.
type OrganizationMember struct {
	OrgID         string    `json:"org_id"`
	UserID        string    `json:"user_id"`
	Role          string    `json:"role"`
	Email         string    `json:"email,omitempty"`
	AddedByUserID string    `json:"added_by_user_id,omitempty"`
	AddedAt       time.Time `json:"added_at"`
}

// OrganizationInvite is one outstanding invitation. Token-based; the
// invitee receives a link in their email that carries the token, the
// public /invites/accept/{token} endpoint consumes it.
//
// Single-use: AcceptedAt + AcceptedByUserID flip from nil to set
// values on first successful accept. Subsequent accept attempts on
// the same token fail with ErrAlreadyAccepted.
type OrganizationInvite struct {
	InviteID         string     `json:"invite_id"`
	OrgID            string     `json:"org_id"`
	Email            string     `json:"email"`
	Role             string     `json:"role"`
	Token            string     `json:"token,omitempty"` // not returned in list responses, only on create
	InvitedByUserID  string     `json:"invited_by_user_id"`
	CreatedAt        time.Time  `json:"created_at"`
	ExpiresAt        time.Time  `json:"expires_at"`
	AcceptedAt       *time.Time `json:"accepted_at,omitempty"`
	AcceptedByUserID string     `json:"accepted_by_user_id,omitempty"`
}

// WebhookDelivery is one attempted POST to a registered webhook URL.
// One row per attempt (including retries); a single failure-group
// escalation may produce up to 3 rows under the default retry policy.
//
// Status values: "pending" | "delivered" | "failed".
type WebhookDelivery struct {
	DeliveryID   string    `json:"delivery_id"`
	WebhookID    string    `json:"webhook_id"`
	ProjectID    string    `json:"project_id"`
	FailureClass string    `json:"failure_class,omitempty"`
	Signature    string    `json:"signature,omitempty"`
	GroupID      string    `json:"group_id,omitempty"`
	Attempt      int       `json:"attempt"`
	Status       string    `json:"status"`
	HTTPStatus   *int      `json:"http_status,omitempty"`
	Error        string    `json:"error,omitempty"`
	ResponseBody string    `json:"response_body,omitempty"`
	DurationMs   int64     `json:"duration_ms"`
	CreatedAt    time.Time `json:"created_at"`
}

// Failure-class constants. One value per detector that produces a
// failure_group. Crashes is the only class wired into the backend
// detector today; loops / tool_failures / etc. come online as their
// Phase-3+ detectors land. Keep this list in sync with the SDK side
// (mesedi-python events.EventType) when adding new classes.
const (
	FailureClassCrashes      = "crashes"
	FailureClassLoops        = "loops"
	FailureClassToolFailures = "tool_failures"
	FailureClassValidator    = "validator_failures"
	FailureClassDrift        = "drift"
	FailureClassCostVelocity = "cost_velocity"
	FailureClassInjection    = "prompt_injection"
	// FailureClassInfraThrottled groups executions whose underlying
	// provider transport hit a rate-limit, quota exhaustion, or local
	// circuit-breaker trip. Distinct from cost_velocity (different
	// fix: raise quota vs reduce calls) and from tool_failures (the
	// failure is in the transport plane, not the developer's tool).
	// Signature pieces are assembled by ThrottlingSignature.
	FailureClassInfraThrottled = "infrastructure_throttled"
	// FailureClassDataLeakage groups executions whose outbound LLM
	// prompts or tool-call arguments contained credentials, signed
	// tokens, or PII matched by a DLP rule. Signature is the rule_id
	// (e.g. "aws_access_key" or "stripe_live_secret_key"), one
	// failure_group per detected secret type per project so SecOps
	// can see "we have 12 runs leaking AWS keys" vs "47 runs leaking
	// Stripe keys" without alert flood.
	FailureClassDataLeakage = "data_leakage"
	// FailureClassSemanticLoop groups executions where an agent's
	// canonical-state hash repeated 3+ times across checkpoints.
	// Captures the "different surface text, same logical state"
	// loop pattern that the existing step_count / identical_call
	// detectors miss. Signature is "semantic_loop:<hex8>", a stable
	// fingerprint of the looping state.
	FailureClassSemanticLoop = "semantic_loop"
	// FailureClassToolSchemaDrift groups executions where a tool's
	// return-value SHAPE changed vs the project's stable historical
	// baseline. Signature is "<tool_name>:<hex8>" of the new shape
	// so each (tool, new shape) gets its own group: SREs see a
	// single alert when a tool rolls over to a new shape, not one
	// alert per agent run after the rollover.
	FailureClassToolSchemaDrift = "tool_schema_drift"
	// FailureClassContextOverflow groups executions where an
	// llm_call's input_tokens crossed 90% (warn) or 100% (fail) of
	// the configured model's context window. Signature is
	// "context_overflow:<level>:<model>" so the dashboard surfaces
	// (model, severity) pairs distinctly.
	FailureClassContextOverflow = "context_overflow"
	// FailureClassTokenWaste groups executions whose user_prompts
	// share a leading-prefix hash that repeats 3+ times. Catches
	// the context-accumulation loop described in the marketing
	// page's $500/mo → $847k/mo case. Signature is
	// "token_waste:<hex8>".
	FailureClassTokenWaste = "token_waste"
	// FailureClassSandboxEscape groups executions whose tool_call
	// arguments or return_values matched a sandbox-escape pattern
	// (os.system, raw socket, /proc/self, instance metadata
	// endpoints, etc.). Signature is "sandbox_escape:<pattern_id>"
	// so each escape vector clusters separately for security
	// triage.
	FailureClassSandboxEscape = "sandbox_escape"
	// FailureClassGroundingFailure groups executions whose
	// eval_score events showed the agent's output diverged from
	// retrieved context. Signature is
	// "grounding_failure:<evaluator_id>:<metric_type>".
	FailureClassGroundingFailure = "grounding_failure"
	// FailureClassCascadingFailure groups executions where an
	// agent_handoff was followed by the child execution
	// crashing within a short cascade window. The signature is
	// "cascading_failure:<from_agent>:<to_agent>:<child_status>"
	// so that repeated cascades along the same agent edge
	// dedupe into one group regardless of the specific
	// execution_id pair involved.
	FailureClassCascadingFailure = "cascading_failure"
	// FailureClassCoordinationDeadlock groups executions where a
	// cycle was detected in the agent-handoff graph within the
	// topology subtree. The signature is
	// "coordination_deadlock:<agent_a>:<agent_b>" for the canonical
	// 2-cycle case (alphabetized so A↔B and B↔A collapse to one
	// group), and "coordination_deadlock:cycle:<sorted_agents>"
	// for longer cycles. Maps to the "circular wait" Coffman
	// condition; the rest of Coffman's conditions are implicit in
	// the agent-handoff semantics (mutual exclusion = one agent at
	// a time per role, hold and wait = synchronous handoff, no
	// preemption = SDK does not unwind handoffs).
	FailureClassCoordinationDeadlock = "coordination_deadlock"
	// FailureClassProviderIncident groups executions that
	// experienced an LLM-provider-side outage (Anthropic, OpenAI,
	// Gemini, ...) detected by a cross-tenant signal: at least N
	// distinct tenants in the project saw provider errors with
	// the same provider name within the recent window. The
	// signature is "provider_incident:<provider>:<error_class>"
	// so a single outage produces one group per (provider,
	// error_class) pair rather than one per affected tenant.
	FailureClassProviderIncident = "provider_incident"
	// FailureClassHITLTimeout groups executions where a human
	// intervention event indicated an SLA breach.
	// Two firing conditions:
	//   1. response_kind == "timeout" - the host application
	//      gave up waiting before a human responded.
	//   2. wait_duration_ms > sla_seconds * 1000 (when
	//      sla_seconds is set) - a human responded but
	//      missed the customer-declared SLA.
	// Signature is "hitl_timeout:<reason>" where reason is
	// "explicit" (case 1) or "sla_exceeded" (case 2).
	FailureClassHITLTimeout = "hitl_timeout"
	// FailureClassHITLRejectionSpike groups executions that
	// participated in a project-wide spike in human rejections or
	// edits over the recent window. This is a
	// cross-execution signal that detects agent quality
	// regressions: if many humans across the project independently
	// said "no" or "edit this" to recent agent outputs, the agent's
	// behavior likely regressed.
	//
	// Two firing variants, distinguished by signature:
	//   "hitl_rejection_spike:rejected" - humans are saying NO
	//      (response_kind="rejected"). Strong signal that the agent
	//      is producing wrong / unsafe / unwanted outputs.
	//   "hitl_rejection_spike:edited"   - humans are MODIFYING the
	//      output before approving (response_kind="edited").
	//      Signal that the agent's output is close but consistently
	//      requires correction.
	FailureClassHITLRejectionSpike = "hitl_rejection_spike"
	// FailureClassRecordIntegrity groups executions whose own event
	// record is internally inconsistent. Every other class in this
	// list describes something the AGENT did; this one describes
	// something wrong with the EVIDENCE of what the agent did.
	//
	// Two firing variants, distinguished by signature:
	//   "record_integrity:sequence_gap"        - the event stream is
	//      missing at least one sequence number between its lowest
	//      and highest observed value. Events were produced that this
	//      record does not contain.
	//   "record_integrity:duplicate_sequence"  - two or more events
	//      claim the same sequence number, so one position in the
	//      record was written more than once.
	//
	// It reports incompleteness, NOT tampering. A gap is far more
	// often a dropped request or an SDK killed mid-flush than anyone
	// deleting anything, and the detector documentation says so
	// plainly, an integrity signal that overclaims gets ignored.
	// Proving a record was not altered after the fact requires it to
	// have been signed when written, which this service does not do.
	FailureClassRecordIntegrity = "record_integrity"
)

// FailureGroup is a deduplicated cluster of failures sharing the same
// signature within a project + failure_class. The first crashed
// execution that matches an unseen signature creates a new group; every
// subsequent identical crash bumps the counters and updates last_seen.
//
// group_id is derived deterministically from (project_id, failure_class,
// signature), so the same signature always maps to the same group_id
// across runs and restarts, no UUID coordination required.
type FailureGroup struct {
	GroupID            string    `json:"group_id"`
	ProjectID          string    `json:"project_id"`
	FailureClass       string    `json:"failure_class"`
	Signature          string    `json:"signature"`
	FirstSeen          time.Time `json:"first_seen"`
	LastSeen           time.Time `json:"last_seen"`
	EventCount         int       `json:"event_count"`
	AffectedExecutions int       `json:"affected_executions"`
	CostWastedUSD      *float64  `json:"cost_wasted_usd,omitempty"`
	// TotalTokensIn / TotalTokensOut / TotalTokens are computed live
	// as SUM(executions.total_tokens_in / total_tokens_out) across all
	// executions linked to the group (same rollup pattern as
	// CostWastedUSD). TotalTokens is the sum of the two. Populated
	// only when the summed value is positive, nil means "no LLM
	// call in this group's executions had recorded token usage."
	// The dashboard's per-failure-class metric policy decides which
	// failure_class surfaces these fields prominently (Tier 1, token
	// classes), as secondary context (Tier 2, most classes), or hides
	// them on the group header entirely (Tier 3, HITL / tool /
	// coordination classes where tokens are non-diagnostic). See
	// mesedi-web/dashboard/lib/failureClass.ts for the policy.
	TotalTokensIn     *int64 `json:"total_tokens_in,omitempty"`
	TotalTokensOut    *int64 `json:"total_tokens_out,omitempty"`
	TotalTokens       *int64 `json:"total_tokens,omitempty"`
	SampleExecutionID string `json:"sample_execution_id,omitempty"`
	// AnalysisMarkdown is the LLM-generated root-cause analysis
	// (). Nil when no analysis has been generated for
	// this group yet. Rendered as Markdown on the dashboard.
	AnalysisMarkdown *string `json:"analysis_markdown,omitempty"`
	// AnalyzedAt is the timestamp the analysis was produced. Nil
	// when no analysis exists. Used as a cache-invalidation key:
	// the dashboard offers a "Regenerate" button when analyzed_at
	// is older than 24 hours or new affected executions have
	// landed since.
	AnalyzedAt *time.Time `json:"analyzed_at,omitempty"`
	// AnalysisModel is the model id that produced the analysis
	// (e.g. "claude-haiku-4-5"). Lets the dashboard render
	// provenance ("Analyzed by claude-haiku-4-5 14m ago").
	AnalysisModel *string `json:"analysis_model,omitempty"`
	// AnalysisPlaybookSignature is the SHA-256 hex digest of the
	// playbook content used at the time AnalysisMarkdown was
	// generated (migration 053, ai-analysis-staleness-tracking
	// wave). The dashboard compares this to the in-binary
	// playbooks.Signature() for this failure_class; when they
	// differ, a "Re-analyze to refresh" badge surfaces because
	// the cached analysis was anchored on an older playbook.
	// NULL on rows generated before this column existed; the
	// dashboard treats NULL as "outdated, recommend re-analyze."
	AnalysisPlaybookSignature *string `json:"analysis_playbook_signature,omitempty"`
	// SeverityHint is the SDK-supplied severity at the time the
	// failure_group was created (migration 047). Today only
	// validator_failures populates it (via the SDK
	// `validator_result(..., severity=...)` parameter). NULL means
	// no hint was supplied, severity resolution falls through to
	// the class default. Severity precedence (+ validator
	// failures.G1):
	//   1. project_class_severities override
	//   2. severity_hint (this field)
	//   3. severity.Default(failureClass)
	SeverityHint *string `json:"severity_hint,omitempty"`
	// ResolvedAt + ResolvedBy populate when a customer marks the
	// failure_group resolved via the Resolve action (migration 052,
	// failure-group-resolve wave). Both NULL = unresolved (default).
	// Resolved groups are hidden from the default list view; the
	// dashboard exposes a "Show resolved" toggle that flips
	// ListFailureGroupsOpts.IncludeResolved to surface them again.
	// Sentry-style semantic: new events for a resolved group's
	// signature still cluster into it and update last_seen but do
	// NOT auto-reopen, only an explicit Unresolve action does that.
	ResolvedAt *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy *string    `json:"resolved_by,omitempty"`
}

// ListFailureGroupsOpts is the options struct for
// ListFailureGroups. Introduced in the failure-group-resolve wave
// to replace the positional (q, limit, offset) signature that was
// already growing parameter creep after the list-search-paginate
// wave. New filter dimensions get added as fields here without
// rippling every caller.
type ListFailureGroupsOpts struct {
	// Q is a case-insensitive substring filter on signature +
	// failure_class. Empty means no filter.
	Q string
	// IncludeResolved=true returns resolved groups alongside open
	// ones. Default false hides resolved groups so the dashboard
	// stays clean.
	IncludeResolved bool
	// Limit caps the number of rows returned. Hard ceiling 200 at
	// the handler layer.
	Limit int
	// Offset paginates. Cap 1_000_000 at the handler.
	Offset int
}

// TopologyNode is one entry in a multi-agent execution topology
// (). The topology is a directed tree rooted at the
// execution that has no parent in the project; every node carries
// enough metadata to render the tree in the dashboard and to feed
// downstream detectors (cascading_failure + coordination_deadlock
// ).
//
// Depth=0 is the root; each subsequent depth level is a generation
// of children. Nodes within a level are ordered by started_at ASC.
// Cycles are impossible in the current schema (parent_execution_id
// points BACKWARD in time only), but the traversal guard depth still
// enforces a hard cap so a malformed self-pointing row can't loop.
type TopologyNode struct {
	ExecutionID       string     `json:"execution_id"`
	ParentExecutionID *string    `json:"parent_execution_id,omitempty"`
	Status            string     `json:"status"`
	StartedAt         time.Time  `json:"started_at"`
	EndedAt           *time.Time `json:"ended_at,omitempty"`
	DurationMs        int64      `json:"duration_ms,omitempty"`
	SDKLanguage       string     `json:"sdk_language,omitempty"`
	// Depth is the number of edges from the root to this node;
	// root nodes have depth=0.
	Depth int `json:"depth"`
	// FailureGroupID surfaces the existing executions.failure_group_id
	// column so the dashboard can color-code nodes by their
	// failure-class without a separate per-node lookup.
	FailureGroupID *string `json:"failure_group_id,omitempty"`
}

// HandoffWithChildStatus is one row of the join used by the
// cascading_failure detector. It pairs an agent_handoff
// event payload (parsed into its identifying fields) with the
// terminal status of the child execution referenced by
// child_execution_id. ChildExists is false when the SDK did not
// resolve a child id, or when the referenced child belongs to a
// different project (cross-project ids are dropped at query
// time to preserve tenant isolation).
type HandoffWithChildStatus struct {
	FromAgent        string     `json:"from_agent"`
	ToAgent          string     `json:"to_agent"`
	HandoffKind      string     `json:"handoff_kind,omitempty"`
	ChildExecutionID string     `json:"child_execution_id,omitempty"`
	ChildExists      bool       `json:"child_exists"`
	ChildStatus      string     `json:"child_status,omitempty"`
	ChildEndedAt     *time.Time `json:"child_ended_at,omitempty"`
	HandoffEmittedAt time.Time  `json:"handoff_emitted_at"`
}

// HITLOutcomeCounts is the project-window aggregate consumed by
// the hitl_rejection_spike detector. All three counts are
// over distinct executions (one execution that asked for human
// input three times and got rejected each time still counts once
// in ExecutionsWithRejection).
type HITLOutcomeCounts struct {
	TotalExecutionsWithHITL int `json:"total_executions_with_hitl"`
	ExecutionsWithRejection int `json:"executions_with_rejection"`
	ExecutionsWithEdit      int `json:"executions_with_edit"`
}

// HandoffEdge is a directed (from_agent → to_agent) edge in the
// agent-role graph, attributed to the execution that emitted it.
// The coordination_deadlock detector builds the agent-role
// graph from these edges and looks for cycles.
type HandoffEdge struct {
	EmittingExecutionID string    `json:"emitting_execution_id"`
	FromAgent           string    `json:"from_agent"`
	ToAgent             string    `json:"to_agent"`
	EmittedAt           time.Time `json:"emitted_at"`
}

// TenantCostRow is one row of the cost-by-tenant report:
// a single tenant's aggregated cost + execution count within the
// requested time window. Executions without a tenant_id are reported
// as a single row with TenantID="" so dashboards can distinguish
// "no tenant ever supplied" from "tenant supplied as empty string"
// (callers can suppress this row if they only want explicitly
// attributed cost).
type TenantCostRow struct {
	TenantID       string  `json:"tenant_id"`
	TotalCostUSD   float64 `json:"total_cost_usd"`
	ExecutionCount int     `json:"execution_count"`
	TotalTokensIn  int64   `json:"total_tokens_in"`
	TotalTokensOut int64   `json:"total_tokens_out"`
}

// CheckpointAnchor is where a checkpoint reached the transparency log.
//
// Anchored is false before submission succeeds, and that state is
// legitimate rather than an error: a checkpoint exists before it is
// anchored, and the gap between building one and getting an entry back
// is exactly where a crash can land. The design's answer is to anchor
// late rather than abandon the interval, so "built but not yet
// anchored" has to be a resumable state the scheduler can observe.
//
// Kept OUT of attest.Checkpoint deliberately. These fields are not
// hash-committed: they are learned after the checkpoint is sealed. A
// checkpoint whose hash depended on where it was anchored could not be
// built before it was anchored, which is the wrong way round.
type CheckpointAnchor struct {
	Anchored      bool
	LogEntryID    string
	LedgerBackend string
	AnchoredAt    time.Time

	// LeafPreimage is the exact string the ledger hashed to produce the
	// entry at LogEntryID.
	//
	// It is what makes the anchor checkable. The log does NOT record the
	// checkpoint hash; it records sha256 of a canonical leaf built from a
	// domain tag, an envelope id, the checkpoint hash, a binding hash and
	// a per-request nonce. A verifier hashes this string, compares the
	// result against what the log holds, and separately confirms the
	// checkpoint's own hash appears inside it. Without it there is no
	// path from "this checkpoint" to "that log entry" at all.
	//
	// Empty on anchors recorded before 2026-09-04, and unrecoverable:
	// Verdifax generated the nonce at request time and discarded it, so
	// nothing can reconstruct those preimages. Empty means "this anchor
	// cannot be verified", never "not applicable".
	LeafPreimage string

	// AnchorProofJSON is the transparency log's own proof that the entry
	// at LogEntryID is committed under a root the log signed. It is what
	// makes a checkpoint verifiable WITHOUT contacting the log.
	//
	// A JSON envelope with three members, log_id, entry_body and
	// inclusion_proof, carried as opaque bytes rather than parsed into
	// typed fields, because it is Verdifax's and Sigstore's evidence
	// passing through Mesedi, not Mesedi's own data. Re-encoding it here
	// would silently drop anything either of them adds later.
	//
	// Empty means offline verification is not possible for this
	// checkpoint. That is weaker than an empty LeafPreimage, which means
	// no verification is possible at all: an anchor with a preimage and
	// no proof can still be checked by fetching the entry from the log.
	// Readers must not collapse the two into "unverified".
	AnchorProofJSON string
}

// Store is the abstract persistence interface. Phase 1.5 minimal surface;
// will grow as later phases add read-side queries (list executions,
// failure groups, aggregations, etc.).
type Store interface {
	// Composed of the interfaces below, each in its own file. Split on
	// 2026-09-04, when this one interface reached 1,570 lines.
	//
	// Store remains a single interface to every caller and every
	// implementation: embedding is a compile-time sum, so a type that
	// satisfied the old Store satisfies this one and nothing had to
	// change. The compiler also refuses a method declared in two
	// embedded interfaces, so the split cannot silently duplicate one.
	//
	// What the compiler CANNOT catch is a method DROPPED during the
	// split: the implementations would still have it and the interface
	// would simply stop requiring it, silently. So the split was done
	// by slicing exact line ranges and asserting the method-name set
	// was unchanged (259 methods), not by retyping.
	IdentityStore
	ProjectStore
	ExecutionStore
	DetectionStore
	BillingLifecycleStore
	CheckpointStore

	// Lifecycle.
	Close() error
	Ping(ctx context.Context) error

	// SchemaStatus reports how many migrations this binary embeds
	// versus how many the connected database has applied. Used by the
	// /ready endpoint. See readiness.go for why a Ping alone is not
	// sufficient.
	SchemaStatus(ctx context.Context) (SchemaStatus, error)
}

// AbuseSignal is one detected abuse event. Schema mirrors
// migrations/009_abuse_signals.sql one-to-one.
type AbuseSignal struct {
	SignalID       string     `json:"signal_id"`
	ProjectID      string     `json:"project_id"`
	Kind           string     `json:"kind"`
	Severity       string     `json:"severity"`
	Detail         string     `json:"detail,omitempty"` // JSON-encoded
	DetectedAt     time.Time  `json:"detected_at"`
	NotifiedAt     *time.Time `json:"notified_at,omitempty"`
	SuspendedAt    *time.Time `json:"suspended_at,omitempty"`
	ResolvedAt     *time.Time `json:"resolved_at,omitempty"`
	ResolvedBy     string     `json:"resolved_by,omitempty"`
	ResolutionNote string     `json:"resolution_note,omitempty"`
}
