package store

import (
	"context"
	"time"

	"mesedi/backend/internal/events"
)

// Event ingest and failure detection. The largest surface by far: one
// Group method per failure class, plus the reads those classifiers run
// over an execution's events.
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

type DetectionStore interface {
	// Events (batch ingest path is the hot one; single-event ingest is for tests).
	SaveEvents(ctx context.Context, batch []events.Event) error

	// Failure groups (Phase 3a, crash detection, Phase 3b/4, loops).
	//
	// Every Group* method returns (isNew bool, error). isNew is true
	// iff this call CREATED a new failure_group row (this is the first
	// occurrence of this (project, class, signature) tuple).
	// Subsequent occurrences return isNew=false. Used by the webhook
	// escalation dispatcher to fire on first occurrence only,
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
	// FindFirstFailedTool returns the tool_name AND exception_type
	// of the first tool_call event with payload.status="failed" in
	// this execution, or empty strings if no failed tool calls
	// exist. Used by the tool-failures detector to classify
	// executions where a tool failed silently (agent caught the
	// exception, ran to completion).
	//
	// granular-sig wave: exception_type is the Python exception
	// class name the SDK captured on the failing tool_call (e.g.
	// "RuntimeError", "ConnectionError", "ValidationError"). The
	// handler concatenates it into the failure_group signature as
	// "<tool>:<exception_type>" so tools failing in N distinct ways
	// surface as N clusters instead of one.
	//
	// exception_type may be empty for legacy tool_call events that
	// pre-date the SDK's exception_type capture. In that case the
	// handler falls back to the bare "<tool>" signature shape for
	// backward compat.
	FindFirstFailedTool(ctx context.Context, executionID string) (toolName, exceptionType string, err error)
	// GroupToolFailure upserts a failure_group with
	// failure_class=tool_failures and signature=signature. Returns
	// isNew=true on first occurrence.
	GroupToolFailure(ctx context.Context, executionID, projectID, signature string) (bool, error)
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
	//
	// LEGACY: preserved for backward compat. New call sites should
	// use FindFirstDLPSignalForSeverities (data_leakage.G5 wave).
	// This method is now a thin wrapper that calls
	// FindFirstDLPSignalForSeverities with the historical default
	// ["critical", "high"].
	FindFirstDLPSignal(ctx context.Context, executionID string) (string, error)
	// FindFirstDLPSignalForSeverities returns the rule_id of the
	// highest-priority dlp_scan_result on this execution whose
	// `highest_severity` is in the customer-supplied allowed slice.
	// Empty allowed slice is rejected (caller must pass at least one
	// severity); callers reading from per-project thresholds should
	// invoke EffectiveAllowedSeverities() first to guarantee the
	// slice is well-formed. Closes data_leakage.G5: lets
	// regulated-industry projects include "medium" to fire on PII
	// patterns the default skips. Same priority ordering as the
	// legacy method (critical wins over high, etc.).
	FindFirstDLPSignalForSeverities(ctx context.Context, executionID string, allowed []string) (string, error)
	// GroupDataLeakage upserts a failure_group with
	// failure_class=data_leakage and signature=ruleID. One group per
	// rule per project so SecOps sees per-secret-type aggregation.
	GroupDataLeakage(ctx context.Context, executionID, projectID, ruleID string) (bool, error)
	// ListCheckpointPayloads returns the payloads of all checkpoint
	// events on the given execution in sequence order. Used by the
	// semantic_loop detector to feed its canonical-state hash chain.
	// The returned slice's index order matches the events' sequence.
	ListCheckpointPayloads(ctx context.Context, executionID string) ([][]byte, error)
	// CountCheckpointEventsForProject returns the total count of
	// checkpoint events across all executions for the project, plus
	// the most-recent timestamp. Used by the detector-status surface
	// to render the semantic_loop "no checkpoint data yet" empty
	// state, count=0 + lastAt=nil means the customer has never
	// instrumented mesedi.checkpoint() and the semantic_loop detector
	// is therefore invisible to them. Empty-states wave (closes the
	// backend half of semantic_loop.G2).
	CountCheckpointEventsForProject(ctx context.Context, projectID string) (count int, lastAt *time.Time, err error)
	// CountLLMCallsByProviderSince returns provider → llm_call count
	// for the project over the given window. Used by detector-status
	// () to detect Ollama-only projects and render skip-
	// reason chips on the 3 N/A detectors (provider_incident,
	// infrastructure_throttled, cost_velocity).
	CountLLMCallsByProviderSince(ctx context.Context, projectID string, since time.Time) (map[string]int, error)
	// ListToolCallCountsForProject returns the per-tool count of
	// non-failed tool_call events across all executions for the
	// project. Used by the detector-status surface to render the
	// tool_schema_drift "priming, N/min_history_calls observed"
	// state per tool, tools below min_history_calls don't yet
	// trigger drift detection by design, but customers don't see
	// that progress today. Empty-states wave (closes the backend
	// half of tool_schema_drift.G2).
	ListToolCallCountsForProject(ctx context.Context, projectID string) ([]ToolCallCount, error)
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
	// ListToolDescriptions returns up to `limit` recent
	// tool_description values from tool_call events for the
	// (project, tool) pair, ordered newest-first. Excludes the
	// calling execution so the detector compares against PRIOR runs.
	//
	// Separate from ListSuccessfulToolReturns rather than folded into
	// it, deliberately. The description and the return shape are
	// independent halves of a tool's contract and drift in each means
	// something different: a changed return shape is usually the
	// tool's author shipping a change, while a changed description is
	// the text the MODEL reads being rewritten underneath it. That is
	// the shape of CVE-2026-75130 and the MCP tool-poisoning class
	// generally. Keeping them separate also avoids rehashing existing
	// history: folding description into the existing shape hash would
	// invalidate every stored baseline and make every tool look like
	// it drifted once on deploy.
	//
	// Includes failed calls as well as successful ones: a poisoned
	// description is worth seeing even when the call it accompanied
	// blew up.
	ListToolDescriptions(
		ctx context.Context,
		projectID, toolName, excludeExecutionID string,
		limit int,
	) ([]string, error)
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
	// context_overflow and token_waste detectors.
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
	// project). Used by the cascading_failure detector which
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
	// Used by the coordination_deadlock detector to build
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
	// Used by the provider_incident detector to fire only
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
	// sequence order (/). Used by the hitl_timeout
	// detector and the hitl_rejection_spike detector.
	ListHumanInterventionPayloads(ctx context.Context, executionID string) ([][]byte, error)
	// GroupHITLTimeout upserts a failure_group with
	// failure_class=hitl_timeout and the detector-supplied
	// signature.
	GroupHITLTimeout(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// CountHITLOutcomesInWindow aggregates human_intervention
	// event verdicts across the project's recent executions
	// (). Returns counts of distinct executions that
	// asked for human input in the window plus the subsets that
	// got at least one "rejected" and "edited" response.
	CountHITLOutcomesInWindow(
		ctx context.Context,
		projectID string,
		since time.Time,
	) (HITLOutcomeCounts, error)
	// GroupHITLRejectionSpike upserts a failure_group with
	// failure_class=hitl_rejection_spike and the detector-supplied
	// signature.
	GroupHITLRejectionSpike(ctx context.Context, executionID, projectID, signature string) (bool, error)
	// GroupRecordIntegrity upserts a failure_group with
	// failure_class=record_integrity and the detector-supplied
	// signature ("record_integrity:sequence_gap" or
	// "record_integrity:duplicate_sequence").
	//
	// Note the self-referential quality of this one: the execution
	// being grouped is the same execution whose record is incomplete.
	// That is intentional, the group points at the run you would go
	// look at, but it does mean the group's own event counts are
	// drawn from the record it is telling you not to fully trust.
	GroupRecordIntegrity(ctx context.Context, executionID, projectID, signature string) (bool, error)
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
	// UpdateFailureGroupSeverityHint writes the SDK-supplied severity
	// to the severity_hint column on a freshly-created failure_group
	// row (migration 047). Used by the validator_failures detector
	// to honor the SDK's `validator_result(..., severity=...)`
	// parameter (validator_failures.G1). NULL/empty value clears the
	// hint. Only writes when the group already exists; returns
	// ErrNotFound otherwise.
	UpdateFailureGroupSeverityHint(
		ctx context.Context,
		groupID string,
		severityHint string,
	) error
	// GetFailureGroupSeverityHint reads the per-group severity hint
	// the SDK supplied at detection time. Returns ("", nil) when no
	// hint was set. Used by the severity resolution chain in
	// webhook_dispatch: per-class override > severity_hint >
	// severity.Default(failureClass).
	GetFailureGroupSeverityHint(
		ctx context.Context,
		groupID string,
	) (string, error)

	// FindFirstFailedValidator returns the name of the first
	// validator_result event with payload.passed=false in this
	// execution, or empty string if no validators failed. The "agent
	// recovered from a quality-check failure" pattern.
	// Returns (validatorName, severityHint, err). severityHint is
	// the SDK-supplied `severity` payload field on validator_result
	// events (added validator_failures.G1), one of {"warning",
	// "error", "critical"} or empty when the SDK is older than the
	// fix. Empty means "no hint"; resolution falls through to class
	// default.
	// granular-sig wave: also returns the optional `category` field
	// the SDK now lets customers attach to validator_result calls.
	// The handler concatenates it into the failure_group signature
	// as "<validatorName>:<category>" when present (forward-only;
	// callers not supplying category continue to land under the
	// bare "<validatorName>" signature shape for backward compat).
	FindFirstFailedValidator(ctx context.Context, executionID string) (validatorName, severityHint, category string, err error)
	// GroupValidatorFailure upserts a failure_group with
	// failure_class=validator_failures and signature=validatorName.
	GroupValidatorFailure(ctx context.Context, executionID, projectID, validatorName string) (bool, error)
	// GroupPromptInjection upserts a failure_group with
	// failure_class=prompt_injection and signature=patternName.
	GroupPromptInjection(ctx context.Context, executionID, projectID, patternName string) (bool, error)
	// GroupCostVelocity upserts a failure_group with
	// failure_class=cost_velocity and a cost-bucketed signature.
	GroupCostVelocity(ctx context.Context, executionID, projectID string, costUSD float64) (bool, error)
	// GroupCostVelocityRate upserts a failure_group with
	// failure_class=cost_velocity and a RATE-bucketed signature
	// (rate_$X+_per_min). Companion to GroupCostVelocity, same
	// failure_class, different signature so rate-based bursts cluster
	// distinctly from per-execution magnitude on the dashboard.
	GroupCostVelocityRate(ctx context.Context, executionID, projectID string, ratePerMinUSD float64) (bool, error)
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
	// last_seen DESC (most recent first). Opts struct carries pagination
	// (Limit + Offset), search (Q, case-insensitive substring on
	// signature + failure_class), and resolved-visibility
	// (IncludeResolved, default false hides resolved groups from the
	// dashboard's default view). Zero-value opts == legacy default
	// behavior with no q + first page of 50 + resolved hidden.
	ListFailureGroups(ctx context.Context, projectID string, opts ListFailureGroupsOpts) ([]*FailureGroup, error)
	// ResolveFailureGroup marks a failure_group as resolved (sets
	// resolved_at = now, resolved_by = actorUserID). Tenant-scoped via
	// the projectID predicate, a resolve attempt against another
	// project's group_id returns ErrNotFound, no leak. Idempotent: a
	// second resolve refreshes the timestamp. Audit emission lives at
	// the handler layer (audit_events.action = "failure_group.resolved").
	ResolveFailureGroup(ctx context.Context, groupID, projectID, actorUserID string) error
	// UnresolveFailureGroup clears resolved_at + resolved_by. Same
	// tenant-scope contract as ResolveFailureGroup. Idempotent.
	UnresolveFailureGroup(ctx context.Context, groupID, projectID string) error
	// GetFailureGroup returns a single failure_group by id. Returns
	// ErrNotFound if absent.
	GetFailureGroup(ctx context.Context, groupID string) (*FailureGroup, error)
	// GetFailureGroupByClassSignature returns a failure_group by its
	// natural key. Used by the webhook dispatcher to fetch the
	// canonical sample_execution_id for the payload at
	// first-occurrence time.
	GetFailureGroupByClassSignature(ctx context.Context, projectID, failureClass, signature string) (*FailureGroup, error)
	// SaveFailureGroupAnalysis stores the LLM-generated root-cause
	// analysis on a failure_group. Sets
	// analysis_markdown, analyzed_at, analysis_model, and
	// analysis_playbook_signature on the row. Idempotent:
	// a subsequent call overwrites with the new analysis.
	// Returns ErrNotFound when the group does not exist.
	//
	// playbookSignature is the SHA-256 hex digest of the playbook
	// content used at analysis time (migration 053,
	// ai-analysis-staleness-tracking wave). Empty string stores as
	// NULL, used when the playbook lookup failed and no signature
	// is available; the dashboard treats NULL as "outdated."
	SaveFailureGroupAnalysis(
		ctx context.Context,
		groupID, analysisMarkdown, analysisModel string,
		analyzedAt time.Time,
		playbookSignature string,
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
}
