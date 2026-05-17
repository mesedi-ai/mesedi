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
// or nil means "all classes" — the common case. Validation that
// supplied class names match the FailureClass* constants happens at
// the handler layer.
type ProjectWebhook struct {
	WebhookID       string    `json:"webhook_id"`
	ProjectID       string    `json:"project_id"`
	Name            string    `json:"name,omitempty"`
	URL             string    `json:"url"`
	Secret          string    `json:"-"` // never returned in list responses
	EnabledClasses  []string  `json:"enabled_classes"`
	Enabled         bool      `json:"enabled"`
	CreatedAt       time.Time `json:"created_at"`
}

// WebhookDelivery is one attempted POST to a registered webhook URL.
// One row per attempt (including retries); a single failure-group
// escalation may produce up to 3 rows under the default retry policy.
//
// Status values: "pending" | "delivered" | "failed".
type WebhookDelivery struct {
	DeliveryID    string    `json:"delivery_id"`
	WebhookID     string    `json:"webhook_id"`
	ProjectID     string    `json:"project_id"`
	FailureClass  string    `json:"failure_class,omitempty"`
	Signature     string    `json:"signature,omitempty"`
	GroupID       string    `json:"group_id,omitempty"`
	Attempt       int       `json:"attempt"`
	Status        string    `json:"status"`
	HTTPStatus    *int      `json:"http_status,omitempty"`
	Error         string    `json:"error,omitempty"`
	ResponseBody  string    `json:"response_body,omitempty"`
	DurationMs    int64     `json:"duration_ms"`
	CreatedAt     time.Time `json:"created_at"`
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
	// ListAPIKeysForProject returns all keys (minus the hash) for a
	// project, sorted by created_at DESC. Used by the dashboard's
	// settings → API keys page.
	ListAPIKeysForProject(ctx context.Context, projectID string) ([]*APIKey, error)
	// DeleteAPIKey revokes (hard-deletes) an API key by id, but ONLY
	// if it belongs to the given project. Returns ErrNotFound if the
	// key doesn't exist or belongs to a different project — protects
	// against cross-tenant deletion via id-guessing.
	DeleteAPIKey(ctx context.Context, keyID, projectID string) error

	// Project webhooks (failure-class escalation, task #83).
	// CreateProjectWebhook persists a new webhook configuration. The
	// caller is responsible for generating WebhookID + Secret. CreatedAt
	// is set if zero.
	CreateProjectWebhook(ctx context.Context, wh *ProjectWebhook) error
	// ListProjectWebhooksForProject returns every webhook (enabled and
	// disabled) for a project, sorted by CreatedAt DESC. The Secret
	// field is intentionally cleared on the returned values — the
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
	// GroupPromptInjection upserts a failure_group with
	// failure_class=prompt_injection and signature=patternName
	// (e.g. "ignore_instructions", "role_override"). Detection logic
	// lives in internal/detectors/injection.go — this method just
	// records the result.
	GroupPromptInjection(ctx context.Context, executionID, projectID, patternName string) error
	// GroupCostVelocity upserts a failure_group with
	// failure_class=cost_velocity and a cost-bucketed signature
	// (cost_$0.001+, cost_$0.01+, cost_$0.10+, cost_$1+, cost_$10+).
	// V0.0.1 absolute-threshold version; Phase-5+ will swap to a
	// proper baseline-relative detector.
	GroupCostVelocity(ctx context.Context, executionID, projectID string, costUSD float64) error
	// GroupIdenticalCallLoop upserts a failure_group with
	// failure_class=loops and signature=identical_call_<short_hash>
	// when an execution made the same LLM call (same model + user
	// message) at least N times. Catches exact-text loops where the
	// agent is regenerating the same prompt verbatim.
	GroupIdenticalCallLoop(ctx context.Context, executionID, projectID, callHash string) error
	// GroupSimilarCallLoop upserts a failure_group with
	// failure_class=loops and signature=similar_call_<short_hash>
	// when an execution made N+ LLM calls whose user_messages cluster
	// semantically (pairwise cosine distance below threshold) even
	// though no two are exactly identical. Catches the "stuck-loop
	// with paraphrased prompts" pattern that identical_call misses.
	// 4th and final Phase-4 loop sub-detector — runs after
	// identical_call so exact-text loops win that signature.
	GroupSimilarCallLoop(ctx context.Context, executionID, projectID, callHash string) error
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
	// failure_class=drift and the caller-supplied signature
	// (e.g. "new_model:claude-opus-4-6"). v0.0.1 signal is model-mix
	// drift; later versions may introduce lexical or embedding-based
	// signatures within the same failure class.
	GroupDriftSignal(ctx context.Context, executionID, projectID, signature string) error
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
	// bounds — a 7-day window on a busy project can be thousands of
	// rows.
	ListLLMUserMessagesForProjectSince(ctx context.Context, projectID string, cutoff time.Time, excludeExecutionID string, limit int) ([]string, error)
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
