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

	"mesedi/backend/internal/events"
)

// Project is one customer's top-level container for agent telemetry.
//
// Billing fields (Tier, StripeCustomerID, StripeSubscriptionID,
// CurrentPeriodStart, CurrentPeriodEnd, ExecutionsThisPeriod) were
// added in migration 006 as part of the Stripe integration slice
// (#120). For existing projects created before that migration ran,
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
}

// TenantBudgetCeiling is the persisted configuration that powers
// the Enterprise-tier monthly budget ceiling feature (#252).
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
	// (#263). Required for per-member role enforcement. Migration 014
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
}

// APIKeyScopeCustomer / APIKeyScopeAdmin are the only legal values of
// APIKey.Scope. The auth middleware compares against these constants
// to decide whether a request can reach the /admin/* surface.
const (
	APIKeyScopeCustomer = "customer"
	APIKeyScopeAdmin    = "admin"
)

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
	WebhookID      string    `json:"webhook_id"`
	ProjectID      string    `json:"project_id"`
	Name           string    `json:"name,omitempty"`
	URL            string    `json:"url"`
	Secret         string    `json:"-"` // never returned in list responses
	EnabledClasses []string  `json:"enabled_classes"`
	Enabled        bool      `json:"enabled"`
	CreatedAt      time.Time `json:"created_at"`
	// SeverityFilter is the comma-separated subset of severities this
	// webhook fires on (#261). Empty string = "fire on every severity"
	// (backward compatible with pre-#261 webhooks). The dispatcher
	// parses via severity.ParseFilter and skips delivery when the
	// event severity is not in the filter.
	SeverityFilter string `json:"severity_filter"`
}

// ProjectClassSeverity is the per-project override of the hardcoded
// failure-class-to-severity default map (#261). Absent rows fall back
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
// (#262). Carries just the project_id + retention_days the
// scheduler needs to compute the delete cutoff. Skips rows where
// retention_days IS NULL (indefinite retention).
type ProjectRetention struct {
	ProjectID     string `json:"project_id"`
	RetentionDays int    `json:"retention_days"`
}

// Organization is the multi-seat tenant unit (#263). One org has one
// or more organization_members; one project is owned by exactly one
// org via projects.tenant_id. Replaces the implicit owner_user_id
// tenant model used by #259 and #252.
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
	// agent_handoff (#11) was followed by the child execution
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
	// intervention event indicated an SLA breach (Mesedi #20).
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
	// edits over the recent window (Mesedi #21). This is a
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
	SampleExecutionID  string    `json:"sample_execution_id,omitempty"`
	// AnalysisMarkdown is the LLM-generated root-cause analysis
	// (Mesedi #27). Nil when no analysis has been generated for
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
}

// TopologyNode is one entry in a multi-agent execution topology
// (Mesedi #10). The topology is a directed tree rooted at the
// execution that has no parent in the project; every node carries
// enough metadata to render the tree in the dashboard and to feed
// downstream detectors (cascading_failure #12 + coordination_deadlock
// #13).
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
// cascading_failure detector (#12). It pairs an agent_handoff
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
// the hitl_rejection_spike detector (#21). All three counts are
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
// The coordination_deadlock detector (#13) builds the agent-role
// graph from these edges and looks for cycles.
type HandoffEdge struct {
	EmittingExecutionID string    `json:"emitting_execution_id"`
	FromAgent           string    `json:"from_agent"`
	ToAgent             string    `json:"to_agent"`
	EmittedAt           time.Time `json:"emitted_at"`
}

// TenantCostRow is one row of the cost-by-tenant report (Mesedi #5):
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

// Store is the abstract persistence interface. Phase 1.5 minimal surface;
// will grow as later phases add read-side queries (list executions,
// failure groups, aggregations, etc.).
type Store interface {
	// Projects + API keys (admin / bootstrap operations).
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, projectID string) (*Project, error)
	// UpdateProjectTier flips a project's tier without going through
	// Stripe. Founder-side admin lever (#150). Does NOT touch the
	// Stripe customer/subscription columns; if a project was
	// previously on Pro and we manually drop to Hobby, the dangling
	// Stripe subscription is the founder's problem to cancel.
	//
	// expiresAt sets tier_expires_at (nil = never expires). Lazy
	// enforcement: when expiresAt has passed, HandleGetBilling
	// treats the tier as Hobby. Pass nil to make a permanent flip.
	UpdateProjectTier(ctx context.Context, projectID, tier string, expiresAt *time.Time) error
	// UpdateProjectName changes the human-readable display name on a
	// project row. Customer-driven (#173): the rename endpoint on the
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
	// GetProjectStorageStats returns one row per project with counts
	// across the major child tables plus an EstimatedBytes total from
	// SUM(LENGTH()) over the large text columns. Used by the admin
	// dashboard's Storage page (#173) to spot heavy users before the
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
	// by the admin "reset demo project" endpoint (#270) so the next
	// detector pass re-creates each group as isNew=true and the
	// webhook dispatcher fires fresh failure_group.created events.
	// Executions and events are NOT touched; only the grouping
	// summary rows are wiped.
	DeleteFailureGroupsByProject(ctx context.Context, projectID string) (int64, error)
	// ListAllProjects returns every project in the database with
	// aggregate activity stats (last execution time, total execution
	// count) joined in. Used by the founder-side admin dashboard
	// (#150), NEVER expose this through the customer-facing API.
	// Ordered by created_at DESC so newest signups appear first.
	ListAllProjects(ctx context.Context) ([]*AdminProjectRow, error)
	// ListProjectsByOwner returns every project owned by ownerUserID,
	// ordered by created_at ASC (oldest first, which mirrors how the
	// org rollup dashboard wants to lay them out left-to-right). Used
	// by the customer-facing /me/rollup endpoint (#259) to discover
	// the set of project_ids that make up one tenant in v0.1 (where
	// "tenant" is defined as a single user account; the proper
	// organizations table comes later when multi-seat enterprises
	// onboard).
	//
	// Returns an empty slice (not ErrNotFound) when ownerUserID has no
	// projects, so callers can blanket-aggregate without special-casing.
	ListProjectsByOwner(ctx context.Context, ownerUserID string) ([]*Project, error)
	// Billing (#120, Stripe integration).
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
	// member's existing keys stop working immediately (#187). Returns
	// the number of keys deleted; the caller logs this as part of the
	// member-removed audit trail.
	DeleteAPIKeysByUserID(ctx context.Context, userID string) (int, error)

	// Project webhooks (failure-class escalation, task #83).
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

	// Webhook delivery log (slice 2 dispatcher).
	// RecordWebhookDelivery persists one delivery attempt row. The
	// caller is responsible for filling in WebhookID, ProjectID,
	// Status, Attempt, and any of the optional fields; DeliveryID
	// and CreatedAt are set here if zero.
	RecordWebhookDelivery(ctx context.Context, d *WebhookDelivery) error
	// ListDeliveriesForWebhook returns the most recent N deliveries
	// for a webhook, sorted by created_at DESC.
	ListDeliveriesForWebhook(ctx context.Context, webhookID string, limit int) ([]*WebhookDelivery, error)

	// Executions.
	CreateExecution(ctx context.Context, exec *events.Execution) error
	UpdateExecution(ctx context.Context, exec *events.Execution) error
	GetExecution(ctx context.Context, executionID string) (*events.Execution, error)
	// PauseExecution transitions a started execution into the
	// awaiting_human state (Mesedi #18). Atomically sets paused_at
	// to the supplied timestamp, increments pause_count, and sets
	// status='awaiting_human'. Returns ErrInvalidLifecycleTransition
	// if the execution is not currently in `started` state (the only
	// state from which a pause is legal). Returns ErrNotFound if
	// the execution does not exist in the project.
	PauseExecution(ctx context.Context, executionID, projectID string, pausedAt time.Time) error
	// ResumeExecution transitions an awaiting_human execution back
	// to the started state. Computes (resumedAt - paused_at) and
	// adds it to total_paused_ms, clears paused_at, sets
	// status='started'. Returns ErrInvalidLifecycleTransition if
	// the execution is not currently paused. Used both for explicit
	// resume calls AND as a pre-step when transitioning from
	// awaiting_human directly to a terminal status, so the paused
	// duration is flushed before the terminal write.
	ResumeExecution(ctx context.Context, executionID, projectID string, resumedAt time.Time) error
	// ListExecutions returns the project's executions sorted by
	// started_at DESC (most recent first). Pagination via limit/offset.
	ListExecutions(ctx context.Context, projectID string, limit, offset int) ([]*events.Execution, error)
	// ListExecutionsByFailureGroup returns executions whose
	// failure_group_id matches groupID, sorted by started_at DESC.
	// Caller should verify (group.project_id == auth_project_id) BEFORE
	// calling, this method does NOT enforce project scoping.
	ListExecutionsByFailureGroup(ctx context.Context, groupID string, limit, offset int) ([]*events.Execution, error)
	// ListEventsForExecution returns the events recorded against a
	// single execution, sorted by sequence ASC. Used by the dashboard's
	// execution-detail view (Phase 3b polish + replay UI in Phase 9+).
	ListEventsForExecution(ctx context.Context, executionID string) ([]*events.Event, error)
	// CountExecutionsByStatusSince returns a count of executions with
	// the given status that started_at >= cutoff. Used by dashboard
	// stat cards (e.g. "crashed in last 24h"). cutoff = zero-time means
	// "all-time count for that status."
	CountExecutionsByStatusSince(ctx context.Context, projectID, status string, cutoff time.Time) (int, error)
	// SumExecutionCostByProjectSince returns (totalCostUSD, totalCount)
	// across all executions of projectID that started_at >= since.
	// Used by the org-rollup endpoint (#259) for per-project burn
	// aggregation. Sums the persisted executions.estimated_cost_usd
	// column directly; this is the same column the per-execution
	// dashboard surfaces use, so the two views stay in agreement.
	//
	// since=zero-time means "all time".
	SumExecutionCostByProjectSince(ctx context.Context, projectID string, since time.Time) (totalCostUSD float64, totalCount int, err error)

	// ListActiveExecutionsByProject returns executions for projectID
	// that have not yet ended (status = "started"). Used by the
	// tenant budget-ceiling breach handler (#252) to enumerate
	// halt targets when a ceiling breach fires. Sorted by started_at
	// DESC (newest active first).
	ListActiveExecutionsByProject(ctx context.Context, projectID string) ([]*events.Execution, error)

	// Tenant budget ceilings (#252). v0.1 tenant = owner_user_id.
	GetTenantBudgetCeiling(ctx context.Context, ownerUserID string) (*TenantBudgetCeiling, error)
	// UpsertTenantBudgetCeiling inserts or updates the ceiling row.
	// Caller sets MonthlyCeilingUSD, BreachAction, NotifyEmail,
	// NotifyWebhookURL. Store sets CreatedAt on insert, always sets
	// UpdatedAt to now. LastEvaluatedAt and BreachedAt are NOT
	// touched by this method; the scheduler manages those.
	UpsertTenantBudgetCeiling(ctx context.Context, c *TenantBudgetCeiling) error
	// ListTenantBudgetCeilings returns every ceiling row. Used by
	// the scheduler at tick time. Order is unspecified.
	ListTenantBudgetCeilings(ctx context.Context) ([]*TenantBudgetCeiling, error)
	// MarkTenantCeilingEvaluated bumps last_evaluated_at to `at`.
	// Called by the scheduler after each successful evaluation.
	MarkTenantCeilingEvaluated(ctx context.Context, ownerUserID string, at time.Time) error
	// SetTenantCeilingBreached sets breached_at (nil clears it; non-nil
	// records a breach). Called by the scheduler when the burn-vs-
	// ceiling state transitions: nil -> now() on first breach,
	// non-nil -> nil on month-rollover reset.
	SetTenantCeilingBreached(ctx context.Context, ownerUserID string, breachedAt *time.Time) error

	// Per-project data retention (#262). retention_days = nil means
	// "indefinite, never prune" (the Enterprise-friendly default for
	// existing rows). retention_days = positive int means the
	// nightly retention scheduler deletes executions older than that
	// many days; FK CASCADE constraints handle events,
	// failure_groups, and webhook_deliveries.
	GetProjectRetentionDays(ctx context.Context, projectID string) (*int, error)
	// SetProjectRetentionDays writes nil for indefinite or a positive
	// int for a finite window. Handlers validate the value before
	// invoking; store accepts whatever's passed.
	SetProjectRetentionDays(ctx context.Context, projectID string, days *int) error
	// ListProjectsForRetention returns one row per project whose
	// retention_days IS NOT NULL. Used by the retention scheduler
	// (#262) at tick time. Indefinite-retention projects are
	// intentionally excluded so the scheduler never even considers
	// them for deletion.
	ListProjectsForRetention(ctx context.Context) ([]*ProjectRetention, error)
	// DeleteExecutionsOlderThan removes executions for the given
	// projectID whose started_at < cutoff. Returns the number of
	// rows deleted so the scheduler can log prune volume. The
	// FK ON DELETE CASCADE chain takes care of events,
	// failure_groups, and webhook_deliveries owned by the same
	// executions.
	DeleteExecutionsOlderThan(ctx context.Context, projectID string, cutoff time.Time) (deleted int64, err error)

	// Per-project failure-class severity overrides (#261).
	// GetProjectClassSeverity returns the override for (projectID,
	// failureClass), or ErrNotFound if no override exists (caller
	// then falls back to severity.Default).
	GetProjectClassSeverity(ctx context.Context, projectID, failureClass string) (*ProjectClassSeverity, error)
	// UpsertProjectClassSeverity inserts or updates an override row.
	// Caller validates Severity is one of "critical"|"warning"|"info"
	// before invoking; store does not enforce.
	UpsertProjectClassSeverity(ctx context.Context, override *ProjectClassSeverity) error
	// DeleteProjectClassSeverity removes the override so the
	// dispatcher reverts to severity.Default for that class.
	DeleteProjectClassSeverity(ctx context.Context, projectID, failureClass string) error
	// ListProjectClassSeverityOverrides returns every override for
	// the given project. The dashboard settings page uses this to
	// render which classes have been customized vs left at default.
	ListProjectClassSeverityOverrides(ctx context.Context, projectID string) ([]*ProjectClassSeverity, error)

	// =================================================================
	// Team / multi-seat (#263): organizations, members, invites.
	// =================================================================

	// CreateOrganization inserts a fresh row. Caller has already
	// generated org_id; this method sets created_at + updated_at to
	// now and validates required fields.
	CreateOrganization(ctx context.Context, org *Organization) error
	// GetOrganization returns the row or ErrNotFound.
	GetOrganization(ctx context.Context, orgID string) (*Organization, error)
	// UpdateOrganizationName changes the human-readable name and
	// bumps updated_at. Admin-only on the handler side.
	UpdateOrganizationName(ctx context.Context, orgID, name string) error
	// ListOrganizationsForUser returns every org the user is a member
	// of, regardless of role. Used to populate the dashboard org
	// switcher. Sorted by name ASC for stable rendering.
	ListOrganizationsForUser(ctx context.Context, userID string) ([]*Organization, error)

	// AddOrganizationMember inserts a (org_id, user_id, role) row.
	// Caller validates role; store enforces role CHECK constraint
	// at the SQL layer as a defense-in-depth.
	AddOrganizationMember(ctx context.Context, member *OrganizationMember) error
	// GetOrganizationMember returns one row or ErrNotFound. Used by
	// the role-check middleware on every protected endpoint to
	// confirm "is this user a member of this org and what's their role".
	GetOrganizationMember(ctx context.Context, orgID, userID string) (*OrganizationMember, error)
	// ListOrganizationMembers returns every member of an org, sorted
	// by added_at ASC (oldest first). Includes pending-but-not-yet-
	// signed-in members whose email was captured at invite time.
	ListOrganizationMembers(ctx context.Context, orgID string) ([]*OrganizationMember, error)
	// UpdateOrganizationMemberRole changes the role of an existing
	// member. Admin-only on the handler side; here the constraint is
	// just the CHECK on role values.
	UpdateOrganizationMemberRole(ctx context.Context, orgID, userID, newRole string) error
	// RemoveOrganizationMember deletes the row. Admin-only on the
	// handler side. Does NOT cascade-delete the user's session;
	// callers should also clear any session cookies. Returns
	// ErrNotFound if the row didn't exist.
	RemoveOrganizationMember(ctx context.Context, orgID, userID string) error

	// CreateOrganizationInvite inserts a pending invite. Caller has
	// generated invite_id + token + expires_at; store sets created_at.
	CreateOrganizationInvite(ctx context.Context, invite *OrganizationInvite) error
	// GetOrganizationInviteByToken is the lookup used by the public
	// accept endpoint. Returns ErrNotFound for unknown / expired /
	// already-accepted tokens (caller checks AcceptedAt + ExpiresAt
	// after fetch and returns appropriate user-facing errors).
	GetOrganizationInviteByToken(ctx context.Context, token string) (*OrganizationInvite, error)
	// ListOrganizationInvites returns invites for an org. When
	// pendingOnly = true, filters to invites with accepted_at IS NULL
	// AND expires_at > now (the live invite set).
	ListOrganizationInvites(ctx context.Context, orgID string, pendingOnly bool) ([]*OrganizationInvite, error)
	// MarkInviteAccepted atomically transitions an invite from
	// pending to accepted. Returns ErrAlreadyAccepted if accepted_at
	// is already set (race-safe single-use guarantee).
	MarkInviteAccepted(ctx context.Context, inviteID, acceptedByUserID string) error
	// RevokeOrganizationInvite deletes a pending invite. Admin-only
	// on the handler side. Idempotent: deleting a non-existent or
	// already-accepted invite returns ErrNotFound, callers can treat
	// as success.
	RevokeOrganizationInvite(ctx context.Context, inviteID, orgID string) error

	// GetProjectTenantID returns the tenant_id (org_id) for a
	// project. Used by the role-check middleware to map an inbound
	// project-scoped request to its owning org. ErrNotFound when
	// the project doesn't exist; (nil, nil) when the project exists
	// but tenant_id is NULL (legacy rows that escaped the
	// migration 013 backfill).
	GetProjectTenantID(ctx context.Context, projectID string) (*string, error)
	// SetProjectTenantID updates projects.tenant_id for one project.
	// Used by (a) the signup handler to link a freshly created project
	// to its auto-created org, and (b) resolveAdminContext's self-heal
	// path for projects that escaped the 013 backfill (created in the
	// brief window between deploy and migration, or projects that pre-
	// dated 013 without an owner_user_id).
	SetProjectTenantID(ctx context.Context, projectID, tenantID string) error
	// ListProjectsByTenant returns every project whose tenant_id
	// matches. Replaces ListProjectsByOwner in the #259 rollup +
	// #252 budget-ceiling code paths after they're retrofitted.
	ListProjectsByTenant(ctx context.Context, tenantID string) ([]*Project, error)

	// Events (batch ingest path is the hot one; single-event ingest is for tests).
	SaveEvents(ctx context.Context, batch []events.Event) error

	// Failure groups (Phase 3a, crash detection, Phase 3b/4, loops).
	//
	// Every Group* method returns (isNew bool, error). isNew is true
	// iff this call CREATED a new failure_group row (this is the first
	// occurrence of this (project, class, signature) tuple).
	// Subsequent occurrences return isNew=false. Used by the webhook
	// escalation dispatcher (task #83) to fire on first occurrence only,
	// not on every re-occurrence. Idempotency is unchanged, an
	// already-grouped execution is still a no-op and returns
	// (false, nil).
	GroupCrashedExecution(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// GroupTimeBudgetExceedance upserts a failure_group with
	// failure_class=loops and a duration-bucketed signature. Same
	// idempotency contract as GroupCrashedExecution.
	GroupTimeBudgetExceedance(ctx context.Context, executionID, projectID string, durationMs int64) (bool, error)
	// GroupStepCountExceedance upserts a failure_group with
	// failure_class=loops and an event-count-bucketed signature.
	GroupStepCountExceedance(ctx context.Context, executionID, projectID string, eventCount int) (bool, error)
	// CountEventsForExecution returns the number of event rows
	// recorded against a single execution. Used by the step-count
	// detector and the Phase-9 replay UI's "this run produced N
	// events" header.
	CountEventsForExecution(ctx context.Context, executionID string) (int, error)
	// SetExecutionCost writes a computed estimated_cost_usd onto an
	// execution. Called after the cost-aggregator sums LLM tokens from
	// events. No-op if the value is non-positive.
	SetExecutionCost(ctx context.Context, executionID string, cost float64) error
	// FindFirstFailedToolName returns the tool_name of the first
	// tool_call event with payload.status="failed" in this execution,
	// or empty string if no failed tool calls exist. Used by the
	// tool-failures detector to classify executions where a tool
	// failed silently (agent caught the exception, ran to completion).
	FindFirstFailedToolName(ctx context.Context, executionID string) (string, error)
	// GroupToolFailure upserts a failure_group with
	// failure_class=tool_failures and signature=toolName. Returns
	// isNew=true on first occurrence.
	GroupToolFailure(ctx context.Context, executionID, projectID, toolName string) (bool, error)
	// FindFirstThrottlingSignal returns the pre-assembled cluster
	// signature for the first infrastructure_event row on this
	// execution, or empty string if none exist. The signature is
	// produced by ThrottlingSignature from the payload's reason +
	// provider + dimension + circuit_state fields. Used by the
	// infrastructure_throttled detector.
	FindFirstThrottlingSignal(ctx context.Context, executionID string) (string, error)
	// GroupInfrastructureThrottled upserts a failure_group with
	// failure_class=infrastructure_throttled and the caller-supplied
	// signature. Returns isNew=true on first occurrence.
	GroupInfrastructureThrottled(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// FindFirstDLPSignal returns the rule_id of the highest-priority
	// dlp_scan_result on this execution, or empty string when none
	// fired. medium-severity hits never cluster and are filtered
	// out at the query level.
	FindFirstDLPSignal(ctx context.Context, executionID string) (string, error)
	// GroupDataLeakage upserts a failure_group with
	// failure_class=data_leakage and signature=ruleID. One group per
	// rule per project so SecOps sees per-secret-type aggregation.
	GroupDataLeakage(ctx context.Context, executionID, projectID, ruleID string) (bool, error)
	// ListCheckpointPayloads returns the payloads of all checkpoint
	// events on the given execution in sequence order. Used by the
	// semantic_loop detector to feed its canonical-state hash chain.
	// The returned slice's index order matches the events' sequence.
	ListCheckpointPayloads(ctx context.Context, executionID string) ([][]byte, error)
	// GroupSemanticLoop upserts a failure_group with
	// failure_class=semantic_loop and the detector-supplied signature
	// (semantic_loop:<hex8>). Returns isNew=true on first occurrence.
	GroupSemanticLoop(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListSuccessfulToolReturns returns up to `limit` recent
	// return_value payloads from successful tool_call events for the
	// (project, tool) pair, ordered newest-first. Used by the
	// tool_schema_drift detector to build the historical shape
	// rollup. Excludes the calling execution so the detector compares
	// against PRIOR runs, not its own.
	ListSuccessfulToolReturns(
		ctx context.Context,
		projectID, toolName, excludeExecutionID string,
		limit int,
	) ([][]byte, error)
	// ListToolNamesInExecution returns the distinct tool_names
	// invoked successfully in the execution. The schema-drift
	// detector walks this list and queries history per tool.
	ListToolNamesInExecution(ctx context.Context, executionID string) ([]string, error)
	// GroupToolSchemaDrift upserts a failure_group with
	// failure_class=tool_schema_drift and the detector-supplied
	// signature.
	GroupToolSchemaDrift(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListLLMCallPayloads returns the payloads of all llm_call events
	// on the given execution in sequence order. Shared by the
	// context_overflow (#3) and token_waste (#4) detectors.
	ListLLMCallPayloads(ctx context.Context, executionID string) ([][]byte, error)
	// GroupContextOverflow upserts a failure_group with
	// failure_class=context_overflow and the detector-supplied
	// signature.
	GroupContextOverflow(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// GroupTokenWaste upserts a failure_group with
	// failure_class=token_waste and the detector-supplied signature.
	GroupTokenWaste(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListAllToolCallPayloads returns every tool_call payload on
	// the execution in sequence order, including failed ones. Used
	// by the sandbox_escape detector which scans args + returns for
	// escape patterns regardless of success/failure status.
	ListAllToolCallPayloads(ctx context.Context, executionID string) ([][]byte, error)
	// GroupSandboxEscape upserts a failure_group with
	// failure_class=sandbox_escape and the detector-supplied
	// signature.
	GroupSandboxEscape(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListEvalScorePayloads returns every eval_score event payload
	// on the execution in sequence order. Used by the
	// grounding_failure detector.
	ListEvalScorePayloads(ctx context.Context, executionID string) ([][]byte, error)
	// GroupGroundingFailure upserts a failure_group with
	// failure_class=grounding_failure and the detector-supplied
	// signature.
	GroupGroundingFailure(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListHandoffsWithChildStatus returns every agent_handoff event
	// on the supplied parent execution joined with the terminal
	// status of the referenced child execution (when the SDK
	// populated child_execution_id and the child exists in the same
	// project). Used by the cascading_failure detector (#12) which
	// fires when a handoff is followed by the child crashing within
	// the cascade window.
	ListHandoffsWithChildStatus(
		ctx context.Context,
		parentExecutionID, projectID string,
	) ([]HandoffWithChildStatus, error)
	// GroupCascadingFailure upserts a failure_group with
	// failure_class=cascading_failure and the detector-supplied
	// signature.
	GroupCascadingFailure(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListHandoffEdgesInTopology returns every agent_handoff edge
	// emitted by the rootExecutionID's topology subtree (root +
	// descendants reachable via parent_execution_id, capped at
	// maxDepth). Cross-project edges are dropped at query time.
	// Used by the coordination_deadlock detector (#13) to build
	// the agent-role graph and look for cycles.
	ListHandoffEdgesInTopology(
		ctx context.Context,
		rootExecutionID, projectID string,
		maxDepth int,
	) ([]HandoffEdge, error)
	// GroupCoordinationDeadlock upserts a failure_group with
	// failure_class=coordination_deadlock and the detector-supplied
	// signature.
	GroupCoordinationDeadlock(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// CountDistinctTenantsWithProviderError returns the number of
	// distinct tenant_ids in the project that emitted at least one
	// llm_call event with the given provider + error_class since
	// the supplied time. NULL tenant_id collapses to a single
	// "unattributed" bucket and counts as one tenant when present.
	// Used by the provider_incident detector (#16) to fire only
	// when an outage spans multiple tenants (and is therefore
	// almost certainly provider-side rather than caller-side).
	CountDistinctTenantsWithProviderError(
		ctx context.Context,
		projectID, provider, errorClass string,
		since time.Time,
	) (int, error)
	// GroupProviderIncident upserts a failure_group with
	// failure_class=provider_incident and the detector-supplied
	// signature.
	GroupProviderIncident(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListHumanInterventionPayloads returns every
	// human_intervention event payload on the execution in
	// sequence order (Mesedi #19/#20). Used by the hitl_timeout
	// detector (#20) and the hitl_rejection_spike detector (#21).
	ListHumanInterventionPayloads(ctx context.Context, executionID string) ([][]byte, error)
	// GroupHITLTimeout upserts a failure_group with
	// failure_class=hitl_timeout and the detector-supplied
	// signature (Mesedi #20).
	GroupHITLTimeout(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// CountHITLOutcomesInWindow aggregates human_intervention
	// event verdicts across the project's recent executions
	// (Mesedi #21). Returns counts of distinct executions that
	// asked for human input in the window plus the subsets that
	// got at least one "rejected" and "edited" response.
	CountHITLOutcomesInWindow(
		ctx context.Context,
		projectID string,
		since time.Time,
	) (HITLOutcomeCounts, error)
	// GroupHITLRejectionSpike upserts a failure_group with
	// failure_class=hitl_rejection_spike and the detector-supplied
	// signature (Mesedi #21).
	GroupHITLRejectionSpike(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// GetExecutionTopology returns the full ancestor + descendant
	// tree for the given execution within the calling project. The
	// returned slice is ordered by depth ASC then started_at ASC so
	// callers can render the tree without re-sorting. maxDepth caps
	// traversal (defends against a pathological parent_execution_id
	// chain); 0 = use a server default. Cross-project edges are
	// silently dropped at query time so the response only contains
	// nodes the caller is authorized to see.
	GetExecutionTopology(
		ctx context.Context,
		projectID, executionID string,
		maxDepth int,
	) ([]TopologyNode, error)
	// GetCostByTenant aggregates SUM(estimated_cost_usd) and COUNT(*)
	// per tenant_id within the requested time window, ordered by
	// total cost descending. Executions with NULL tenant_id collapse
	// into a single row with TenantID="" so dashboards can render
	// unattributed cost separately. limit caps the number of rows
	// returned (0 = unlimited).
	GetCostByTenant(
		ctx context.Context,
		projectID string,
		since time.Time,
		until time.Time,
		limit int,
	) ([]TenantCostRow, error)
	// FindFirstFailedValidator returns the name of the first
	// validator_result event with payload.passed=false in this
	// execution, or empty string if no validators failed. The "agent
	// recovered from a quality-check failure" pattern.
	FindFirstFailedValidator(ctx context.Context, executionID string) (string, error)
	// GroupValidatorFailure upserts a failure_group with
	// failure_class=validator_failures and signature=validatorName.
	GroupValidatorFailure(ctx context.Context, executionID, projectID, validatorName string) (bool, error)
	// GroupPromptInjection upserts a failure_group with
	// failure_class=prompt_injection and signature=patternName.
	GroupPromptInjection(ctx context.Context, executionID, projectID, patternName string) (bool, error)
	// GroupCostVelocity upserts a failure_group with
	// failure_class=cost_velocity and a cost-bucketed signature.
	GroupCostVelocity(ctx context.Context, executionID, projectID string, costUSD float64) (bool, error)
	// GroupIdenticalCallLoop upserts a failure_group with
	// failure_class=loops and signature=identical_call_<short_hash>.
	GroupIdenticalCallLoop(ctx context.Context, executionID, projectID, callHash string) (bool, error)
	// GroupSimilarCallLoop upserts a failure_group with
	// failure_class=loops and signature=similar_call_<short_hash>.
	GroupSimilarCallLoop(ctx context.Context, executionID, projectID, callHash string) (bool, error)
	// ListModelsForExecution returns the distinct set of model names
	// extracted from this execution's llm_call events' payload.model
	// field, sorted alphabetically. Empty slice if no llm_call events
	// recorded a model.
	ListModelsForExecution(ctx context.Context, executionID string) ([]string, error)
	// ListModelsForProjectSince returns the distinct set of model names
	// seen across this project's llm_call events since cutoff,
	// EXCLUDING events linked to excludeExecutionID. Used by the drift
	// detector to compute the "historical model mix" baseline for the
	// project. Caller passes the current execution's ID in
	// excludeExecutionID so the baseline doesn't include the very
	// execution being evaluated.
	ListModelsForProjectSince(ctx context.Context, projectID string, cutoff time.Time, excludeExecutionID string) ([]string, error)
	// GroupDriftSignal upserts a failure_group with
	// failure_class=drift and the caller-supplied signature.
	GroupDriftSignal(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// ListLLMUserMessagesForExecution returns the user_message field
	// from each llm_call event in this execution, in payload-sequence
	// order. Used by the lexical drift detector to build the
	// per-execution prompt corpus. Returns empty slice if no llm_call
	// events have a non-empty user_message.
	ListLLMUserMessagesForExecution(ctx context.Context, executionID string) ([]string, error)
	// ListLLMUserMessagesForProjectSince returns user_messages from
	// every llm_call event in this project since cutoff, EXCLUDING
	// events linked to excludeExecutionID. Used to build the historical
	// baseline corpus the lexical drift detector compares against.
	// limit caps the number of messages returned (most recent first);
	// pass 0 for "no limit" but the caller is responsible for sensible
	// bounds, a 7-day window on a busy project can be thousands of
	// rows.
	ListLLMUserMessagesForProjectSince(ctx context.Context, projectID string, cutoff time.Time, excludeExecutionID string, limit int) ([]string, error)
	// ListFailureGroups returns the project's failure groups sorted by
	// last_seen DESC (most recent first). For pagination, pass limit +
	// offset; default to limit=50 in callers.
	ListFailureGroups(ctx context.Context, projectID string, limit, offset int) ([]*FailureGroup, error)
	// GetFailureGroup returns a single failure_group by id. Returns
	// ErrNotFound if absent.
	GetFailureGroup(ctx context.Context, groupID string) (*FailureGroup, error)
	// GetFailureGroupByClassSignature returns a failure_group by its
	// natural key. Used by the webhook dispatcher to fetch the
	// canonical sample_execution_id for the payload at
	// first-occurrence time.
	GetFailureGroupByClassSignature(ctx context.Context, projectID, failureClass, signature string) (*FailureGroup, error)
	// SaveFailureGroupAnalysis stores the LLM-generated root-cause
	// analysis on a failure_group (Mesedi #27). Sets
	// analysis_markdown, analyzed_at, and analysis_model on the
	// row. Idempotent: a subsequent call overwrites with the new
	// analysis. Returns ErrNotFound when the group does not exist.
	SaveFailureGroupAnalysis(
		ctx context.Context,
		groupID, analysisMarkdown, analysisModel string,
		analyzedAt time.Time,
	) error
	// CountAIAnalysesSincePeriodStart counts the number of distinct
	// failure_groups for projectID whose analyzed_at >= since.
	// Fallback used by the LLM root-cause rate limiter when a
	// project has no tenant_id (legacy row that escaped the
	// migration-013 backfill). Tenant-scoped counting via
	// CountAIAnalysesByTenantSince is preferred for any project
	// with a tenant_id, because Team customers can own multiple
	// projects under one organization and the LLM rate limit must
	// apply across all of them or the cap is trivially bypassed by
	// spawning more projects.
	CountAIAnalysesSincePeriodStart(ctx context.Context, projectID string, since time.Time) (int, error)
	// CountAIAnalysesByTenantSince counts failure_groups summed
	// across every project owned by tenantID whose analyzed_at >=
	// since. This is the canonical query for the Team-tier LLM
	// rate limit: the cap is per-organization per-period, not
	// per-project, so a Team customer creating 100 projects can
	// not multiply their LLM analysis quota by 100. Cache hits
	// and Hobby tier never reach this query.
	CountAIAnalysesByTenantSince(ctx context.Context, tenantID string, since time.Time) (int, error)

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

	// Abuse signals + project suspension (#172).
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

	// Lifecycle.
	Close() error
	Ping(ctx context.Context) error
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
