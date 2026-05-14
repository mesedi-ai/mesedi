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
type Project struct {
	ProjectID   string    `json:"project_id"`
	Name        string    `json:"name"`
	OwnerUserID string    `json:"owner_user_id,omitempty"`
	CreatedAt   time.Time `json:"created_at"`
}

// APIKey is an authentication credential bound to a project. The raw
// key is never persisted — only the SHA-256 hash. The prefix is a
// non-secret display string for the developer to identify the key.
type APIKey struct {
	KeyID      string     `json:"key_id"`
	ProjectID  string     `json:"project_id"`
	KeyHash    string     `json:"-"` // never serialized to clients
	KeyPrefix  string     `json:"key_prefix"`
	Name       string     `json:"name,omitempty"`
	CreatedAt  time.Time  `json:"created_at"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
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
)

// FailureGroup is a deduplicated cluster of failures sharing the same
// signature within a project + failure_class. The first crashed
// execution that matches an unseen signature creates a new group; every
// subsequent identical crash bumps the counters and updates last_seen.
//
// group_id is derived deterministically from (project_id, failure_class,
// signature), so the same signature always maps to the same group_id
// across runs and restarts — no UUID coordination required.
type FailureGroup struct {
	GroupID            string     `json:"group_id"`
	ProjectID          string     `json:"project_id"`
	FailureClass       string     `json:"failure_class"`
	Signature          string     `json:"signature"`
	FirstSeen          time.Time  `json:"first_seen"`
	LastSeen           time.Time  `json:"last_seen"`
	EventCount         int        `json:"event_count"`
	AffectedExecutions int        `json:"affected_executions"`
	CostWastedUSD      *float64   `json:"cost_wasted_usd,omitempty"`
	SampleExecutionID  string     `json:"sample_execution_id,omitempty"`
}

// Store is the abstract persistence interface. Phase 1.5 minimal surface;
// will grow as later phases add read-side queries (list executions,
// failure groups, aggregations, etc.).
type Store interface {
	// Projects + API keys (admin / bootstrap operations).
	CreateProject(ctx context.Context, p *Project) error
	GetProject(ctx context.Context, projectID string) (*Project, error)
	CreateAPIKey(ctx context.Context, k *APIKey) error
	GetAPIKeyByHash(ctx context.Context, keyHash string) (*APIKey, error)
	TouchAPIKey(ctx context.Context, keyID string) error

	// Executions.
	CreateExecution(ctx context.Context, exec *events.Execution) error
	UpdateExecution(ctx context.Context, exec *events.Execution) error
	GetExecution(ctx context.Context, executionID string) (*events.Execution, error)
	// ListExecutions returns the project's executions sorted by
	// started_at DESC (most recent first). Pagination via limit/offset.
	ListExecutions(ctx context.Context, projectID string, limit, offset int) ([]*events.Execution, error)
	// ListExecutionsByFailureGroup returns executions whose
	// failure_group_id matches groupID, sorted by started_at DESC.
	// Caller should verify (group.project_id == auth_project_id) BEFORE
	// calling — this method does NOT enforce project scoping.
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

	// Events (batch ingest path is the hot one; single-event ingest is for tests).
	SaveEvents(ctx context.Context, batch []events.Event) error

	// Failure groups (Phase 3a — crash detection, Phase 3b/4 — loops).
	// GroupCrashedExecution upserts a failure_group for the (project,
	// failure_class=crashes, signature) tuple and links the execution.
	// Idempotent: re-calling with an already-grouped execution is a no-op.
	GroupCrashedExecution(ctx context.Context, executionID, projectID, signature string) error
	// GroupTimeBudgetExceedance upserts a failure_group with
	// failure_class=loops and a duration-bucketed signature. Same
	// idempotency contract as GroupCrashedExecution — an execution
	// already linked to a group (e.g., already grouped as a crash) is
	// a no-op; crash classification wins over time-budget overlap.
	GroupTimeBudgetExceedance(ctx context.Context, executionID, projectID string, durationMs int64) error
	// GroupStepCountExceedance upserts a failure_group with
	// failure_class=loops and an event-count-bucketed signature. Runs
	// after the time-budget check; the same idempotency short-circuit
	// means each execution lands in at most one group.
	GroupStepCountExceedance(ctx context.Context, executionID, projectID string, eventCount int) error
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
	// failure_class=tool_failures and signature=toolName. Same
	// idempotency contract as the other groupers.
	GroupToolFailure(ctx context.Context, executionID, projectID, toolName string) error
	// FindFirstFailedValidator returns the name of the first
	// validator_result event with payload.passed=false in this
	// execution, or empty string if no validators failed. The "agent
	// recovered from a quality-check failure" pattern.
	FindFirstFailedValidator(ctx context.Context, executionID string) (string, error)
	// GroupValidatorFailure upserts a failure_group with
	// failure_class=validator_failures and signature=validatorName.
	GroupValidatorFailure(ctx context.Context, executionID, projectID, validatorName string) error
	// ListFailureGroups returns the project's failure groups sorted by
	// last_seen DESC (most recent first). For pagination, pass limit +
	// offset; default to limit=50 in callers.
	ListFailureGroups(ctx context.Context, projectID string, limit, offset int) ([]*FailureGroup, error)
	// GetFailureGroup returns a single failure_group by id. Returns
	// ErrNotFound if absent.
	GetFailureGroup(ctx context.Context, groupID string) (*FailureGroup, error)

	// Lifecycle.
	Close() error
	Ping(ctx context.Context) error
}
