package store

import (
	"context"
	"time"

	"mesedi/backend/internal/events"
)

// Executions and the per-project configuration that governs them: budget
// ceilings, time and cost thresholds, patterns, detector thresholds, the
// allowlist, retention, severity overrides, organizations and tenants.
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

type ExecutionStore interface {
	// Executions.
	CreateExecution(ctx context.Context, exec *events.Execution) error
	UpdateExecution(ctx context.Context, exec *events.Execution) error
	GetExecution(ctx context.Context, executionID string) (*events.Execution, error)
	// PauseExecution transitions a started execution into the
	// awaiting_human state. Atomically sets paused_at
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
	// When q is non-empty, results are filtered to rows where
	// execution_id OR crash_signature contains q (case-insensitive
	// substring). Server-side search powers the dashboard's
	// list-search-paginate wave; pass "" for the unfiltered list.
	ListExecutions(ctx context.Context, projectID string, q string, limit, offset int) ([]*events.Execution, error)
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
	// Used by the org-rollup endpoint for per-project burn
	// aggregation. Sums the persisted executions.estimated_cost_usd
	// column directly; this is the same column the per-execution
	// dashboard surfaces use, so the two views stay in agreement.
	//
	// since=zero-time means "all time".
	SumExecutionCostByProjectSince(ctx context.Context, projectID string, since time.Time) (totalCostUSD float64, totalCount int, err error)

	// ListActiveExecutionsByProject returns executions for projectID
	// that have not yet ended (status = "started"). Used by the
	// tenant budget-ceiling breach handler to enumerate
	// halt targets when a ceiling breach fires. Sorted by started_at
	// DESC (newest active first).
	ListActiveExecutionsByProject(ctx context.Context, projectID string) ([]*events.Execution, error)

	// Tenant budget ceilings. v0.1 tenant = owner_user_id.
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

	// Per-project data retention. retention_days = nil means
	// "indefinite, never prune" (the Enterprise-friendly default for
	// existing rows). retention_days = positive int means the
	// nightly retention scheduler deletes executions older than that
	// many days; FK CASCADE constraints handle events,
	// failure_groups, and webhook_deliveries.
	// GetProjectProviderIncidentMinTenants returns the per-project
	// minimum-tenants threshold for the provider_incident detector.
	// Migration 040 added the column with default 2 (the historical
	// hardcoded constant), so projects predating the migration also
	// resolve cleanly through this read. A single-tenant customer
	// can set this to 1 so any provider error from a single agent
	// fires the detector.
	GetProjectProviderIncidentMinTenants(ctx context.Context, projectID string) (int, error)
	// SetProjectProviderIncidentMinTenants writes the threshold.
	// Handler validates >= 1 before invoking; store accepts what's
	// passed.
	SetProjectProviderIncidentMinTenants(ctx context.Context, projectID string, minTenants int) error

	// GetProjectTimeBudgetMs returns the per-project time_budget
	// detector threshold in milliseconds. Migration 041 added the
	// column with default 60_000 (60s), matching the historical
	// hardcoded constant. Chat-agent projects often lower it (e.g.
	// 30_000); research-agent projects often raise it (e.g.
	// 300_000).
	GetProjectTimeBudgetMs(ctx context.Context, projectID string) (int, error)
	// SetProjectTimeBudgetMs writes the threshold in ms. Handler
	// validates >= 1 before invoking; store accepts what's passed.
	SetProjectTimeBudgetMs(ctx context.Context, projectID string, thresholdMs int) error

	// GetProjectCostVelocityThresholdUSD returns the per-project
	// cost_velocity detector threshold in USD. Migration 043 added
	// the column with default 1.00 (one dollar per execution),
	// raised from the broken v0.0.1 hardcoded floor of $0.001 that
	// fired on every real agent call. Cost-sensitive customers can
	// lower it (e.g. $0.10); batch-processing customers can raise
	// it (e.g. $100). The handler enforces a global floor of $0.01
	// to prevent fires-on-every-execution abuse and a ceiling of
	// $10,000 to prevent typo / overflow. NOT tier-capped — alarm
	// sensitivity is not a Mesedi-side cost vector (see tier_caps.go
	// for the principle and provider_incident_min_tenants for the
	// precedent).
	GetProjectCostVelocityThresholdUSD(ctx context.Context, projectID string) (float64, error)
	// SetProjectCostVelocityThresholdUSD writes the threshold in
	// USD. Handler validates [0.01, 10000.00] before invoking;
	// store accepts what's passed.
	SetProjectCostVelocityThresholdUSD(ctx context.Context, projectID string, thresholdUSD float64) error

	// GetProjectCostVelocityRateConfig returns the per-project rate
	// detector configuration: the $/minute threshold and the rolling
	// lookback window in minutes. Migration 044 added the columns
	// with defaults {5.00 USD/min, 5 min}. Rate-based detection
	// answers a different question than the absolute-magnitude
	// detector (sustained burn vs single expensive call) — both can
	// fire on the same execution. NOT tier-capped (same reasoning as
	// the absolute threshold and provider_incident_min_tenants).
	GetProjectCostVelocityRateConfig(ctx context.Context, projectID string) (CostVelocityRateConfig, error)
	// SetProjectCostVelocityRateConfig writes both rate-config fields
	// in one statement. Handler validates threshold ∈ [0.10, 10000.00]
	// $/min and window ∈ [1, 60] minutes before invoking.
	SetProjectCostVelocityRateConfig(ctx context.Context, projectID string, cfg CostVelocityRateConfig) error

	// GetProjectToolReturnValueMaxBytes returns the per-project cap
	// on tool_call return_value size (bytes) for the
	// tool_schema_drift detector's fingerprint computation.
	// Migration 042 added the column with default 8192 (8 KB).
	// Returns above this threshold are excluded from the detector's
	// comparison (treated as inconclusive, mirroring the SDK's
	// "<truncated>" sentinel).
	GetProjectToolReturnValueMaxBytes(ctx context.Context, projectID string) (int, error)
	// SetProjectToolReturnValueMaxBytes writes the cap. Handler
	// validates >= 1 before invoking.
	SetProjectToolReturnValueMaxBytes(ctx context.Context, projectID string, maxBytes int) error

	// GetToolReturnValueStats aggregates tool_call events in the
	// last windowHours for projectID, counting how many returned
	// the SDK's "<truncated>" sentinel and how many would be
	// excluded by the per-project byte cap. Surface in the
	// dashboard tile so customers can see when their cap is too
	// tight to capture useful drift signal.
	GetToolReturnValueStats(
		ctx context.Context,
		projectID string,
		windowHours int,
		maxBytes int,
	) (ToolReturnValueStats, error)

	// ListProjectPatterns returns the customer-defined custom
	// patterns for the given (projectID, detector) pair. Detector
	// must be one of the three security detectors that share the
	// pattern-config primitive (prompt_injection, data_leakage,
	// sandbox_escape). The detector hot path (wired in )
	// reads with enabledOnly=true; the dashboard editor reads with
	// enabledOnly=false so customers can see disabled rules too.
	ListProjectPatterns(
		ctx context.Context,
		projectID, detector string,
		enabledOnly bool,
	) ([]*ProjectPattern, error)
	// CreateProjectPattern inserts a new pattern row. Caller must
	// have RE2-validated p.Pattern and enforced the
	// PROJECT_PATTERN_MAX cap. p.PatternID is server-generated by
	// the caller (e.g. uuid).
	CreateProjectPattern(ctx context.Context, p *ProjectPattern) error
	// UpdateProjectPattern overwrites the mutable fields of an
	// existing pattern row (pattern text, severity, description,
	// enabled). project_id + pattern_id must match. match_count and
	// created_at are NOT touched.
	UpdateProjectPattern(ctx context.Context, p *ProjectPattern) error
	// DeleteProjectPattern removes a pattern row. Returns
	// ErrNotFound when (projectID, patternID) doesn't exist or
	// belongs to a different project (no information leak).
	DeleteProjectPattern(ctx context.Context, projectID, patternID string) error
	// IncrementPatternMatchCount adds delta to match_count for the
	// given pattern (must belong to projectID). Used by the
	// detector hot path in for per-pattern telemetry.
	IncrementPatternMatchCount(
		ctx context.Context,
		projectID, patternID string,
		delta int,
	) error
	// CountProjectPatterns returns the row count for
	// (projectID, detector). Used by the handler to enforce
	// PROJECT_PATTERN_MAX before INSERT.
	CountProjectPatterns(
		ctx context.Context,
		projectID, detector string,
	) (int, error)

	// GetProjectDetectorThreshold reads the per-project override for
	// the given (projectID, detector, threshold_key). Returns
	// ErrNotFound when no override row exists — the caller (B.b
	// hot-path resolver, or the API handler returning the "current
	// value or default" view) falls back to the validators-registry
	// default. Migration 048. See backend/internal/api/
	// detector_thresholds_validators.go for the registry shape.
	GetProjectDetectorThreshold(
		ctx context.Context,
		projectID, detector, thresholdKey string,
	) (*DetectorThreshold, error)
	// ListProjectDetectorThresholds returns every override row for
	// the (projectID, detector) pair, ordered by threshold_key. The
	// hot path in B.b uses this to fetch all of a detector's
	// overrides in a single indexed query at execution-close time.
	ListProjectDetectorThresholds(
		ctx context.Context,
		projectID, detector string,
	) ([]*DetectorThreshold, error)
	// SetProjectDetectorThreshold upserts an override row. Caller
	// has already validated (detector, threshold_key) against the
	// registry, bounds-checked valueJSON via the registry's
	// per-spec validate function, and enforced the tier cap
	// (Hobby/Team/Enterprise). The store accepts whatever value
	// the handler hands it.
	SetProjectDetectorThreshold(
		ctx context.Context,
		projectID, detector, thresholdKey, valueJSON string,
	) error
	// DeleteProjectDetectorThreshold removes an override row,
	// reverting the detector to the registry default for that
	// (project, detector, threshold_key). Returns ErrNotFound when
	// no override row matched.
	DeleteProjectDetectorThreshold(
		ctx context.Context,
		projectID, detector, thresholdKey string,
	) error

	// ListProjectAllowlist returns the customer-defined allowlist
	// entries for the (projectID, detector) pair. Caller is
	// responsible for validating detector ∈ {crashes,
	// tool_failures, validator_failures} at the API edge — the
	// store accepts any string so a future fourth
	// allowlist-supporting detector drops in with no store change.
	// (llowlist.a — migration 049.)
	ListProjectAllowlist(
		ctx context.Context,
		projectID, detector string,
	) ([]*AllowlistEntry, error)
	// GetProjectAllowlistEntry fetches a single row by PK. Returns
	// ErrNotFound when (projectID, allowlistID) doesn't exist or
	// belongs to a different project (no information leak).
	GetProjectAllowlistEntry(
		ctx context.Context,
		projectID, allowlistID string,
	) (*AllowlistEntry, error)
	// CreateProjectAllowlistEntry inserts a new allowlist row.
	// Caller has validated input + assigned a server-generated
	// allowlist_id; created_at is overwritten in the store layer
	// to ensure monotonic order.
	CreateProjectAllowlistEntry(ctx context.Context, e *AllowlistEntry) error
	// UpdateProjectAllowlistEntry overwrites the mutable fields
	// (allowlist_key, reason). Created_at / created_by /
	// match_count are NOT touched. Returns ErrNotFound when no
	// row matched.
	UpdateProjectAllowlistEntry(ctx context.Context, e *AllowlistEntry) error
	// DeleteProjectAllowlistEntry removes a row by PK. Returns
	// ErrNotFound when (projectID, allowlistID) doesn't exist.
	DeleteProjectAllowlistEntry(
		ctx context.Context,
		projectID, allowlistID string,
	) error
	// CountProjectAllowlistEntries returns the row count for
	// (projectID, detector). Used by the handler to enforce
	// PROJECT_ALLOWLIST_MAX before INSERT.
	CountProjectAllowlistEntries(
		ctx context.Context,
		projectID, detector string,
	) (int, error)
	// CheckAllowlistMatch returns true iff a row exists with
	// (project_id, detector, allowlist_key = signature). Used by
	// the Allowlist.b detector hot path to skip failure_group
	// creation on allowlisted signatures. Single indexed query.
	CheckAllowlistMatch(
		ctx context.Context,
		projectID, detector, signature string,
	) (bool, error)
	// IncrementAllowlistMatchCount adds delta to match_count for
	// the (project_id, detector, allowlist_key) row(s). Used by
	// the Allowlist.b detector hot path to keep per-entry
	// telemetry visible in the dashboard editor.
	IncrementAllowlistMatchCount(
		ctx context.Context,
		projectID, detector, allowlistKey string,
		delta int,
	) error
	// GetAllowlistStats returns lifetime per-detector aggregate
	// telemetry for the project's allowlist entries: entry count,
	// total match count (= total failures suppressed), and dormant
	// entry count (entries with match_count = 0). Used by the
	// dashboard suppressions tile (Allowlist.d) to surface
	// "Mesedi suppressed N failures for you" at a glance.
	//
	// One indexed scan of project_detector_allowlist filtered by
	// project_id, GROUP BY detector. Cheap even at the 200-entry
	// PROJECT_ALLOWLIST_MAX cap × 3 detectors = 600-row worst case.
	GetAllowlistStats(
		ctx context.Context,
		projectID string,
	) ([]AllowlistDetectorStats, error)

	// GetConfigFallbackStats counts audit_events rows where the
	// backend's per-project config read failed and the handler
	// fell back to the hardcoded default. Surfaces in the dashboard
	// so a bad migration / column drop doesn't silently
	// ignore every customer's config without anyone noticing.
	GetConfigFallbackStats(
		ctx context.Context,
		projectID string,
		windowHours int,
	) (ConfigFallbackStats, error)

	GetProjectRetentionDays(ctx context.Context, projectID string) (*int, error)
	// SetProjectRetentionDays writes nil for indefinite or a positive
	// int for a finite window. Handlers validate the value before
	// invoking; store accepts whatever's passed.
	SetProjectRetentionDays(ctx context.Context, projectID string, days *int) error
	// ListProjectsForRetention returns one row per project whose
	// retention_days IS NOT NULL. Used by the retention scheduler
	// at tick time. Indefinite-retention projects are
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
	// DeleteFailureGroupsOlderThan removes failure_groups for the
	// project whose LAST_SEEN precedes the cutoff, and returns the
	// count removed.
	//
	// Retention did not cover this table until 2026-08-27. The
	// scheduler deleted executions and relied on "FK CASCADE handles
	// events, failure_groups, webhook_deliveries", a comment present in
	// both store twins and wrong in two of its three claims:
	// failure_groups has a foreign key to PROJECTS, not executions, so
	// nothing cascaded to it. Verified against pg_constraint rather
	// than read off the comment. Result in production: executions and
	// events were 3 days old while failure_groups was 88 days old,
	// against a documented 7-day Hobby window.
	//
	// LAST_SEEN, not first_seen, is the correct cutoff. A group first
	// seen 80 days ago but recurring yesterday is live, and pruning it
	// by first_seen would silently delete a customer's active alert
	// history.
	//
	// ai_analyses and execution_failure_groups both cascade from
	// failure_groups, so deleting here cleans them up too.
	DeleteFailureGroupsOlderThan(ctx context.Context, projectID string, cutoff time.Time) (deleted int64, err error)
	// DeleteWebhookDeliveriesOlderThan removes webhook_deliveries for
	// the project older than the cutoff. Same 2026-08-27 gap: this
	// table also keys on project_id rather than execution_id, so it was
	// never reached by the executions cascade and held 88-day-old rows.
	DeleteWebhookDeliveriesOlderThan(ctx context.Context, projectID string, cutoff time.Time) (deleted int64, err error)

	// Per-project failure-class severity overrides.
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
	// Team / multi-seat: organizations, members, invites.
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
	// matches. Replaces ListProjectsByOwner in the rollup +
	// budget-ceiling code paths after they're retrofitted.
	ListProjectsByTenant(ctx context.Context, tenantID string) ([]*Project, error)
}
