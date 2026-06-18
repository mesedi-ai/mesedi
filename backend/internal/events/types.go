// Package events defines the canonical event types Mesedi ingests from
// instrumented agents and the execution-level metadata that frames them.
//
// The schema mirrors §6 of the detailed concept document
// (mesedi/concept idea/DETAILED_CONCEPT.md §6, Data model). Each event
// is bound to an Execution via execution_id; an Execution is a tree of
// time-ordered events terminated by exactly one terminal event.
//
// For Phase 1, these types are used by:
//   - the HTTP handlers (POST /executions, POST /events) for request
//     validation
//   - the in-memory logging path (no Postgres persistence yet)
//
// For Phase 1.5+, the same structs become the source of truth for the
// Postgres schema (one table per top-level type) and the row-mapping
// layer in internal/store.
package events

import (
	"encoding/json"
	"time"
)

// EventType is the discriminator for the polymorphic Event.Payload field.
// Each value maps to a typed payload struct (LLMCallPayload, ToolCallPayload,
// etc.) that the handler can unmarshal once the type is known.
type EventType string

const (
	EventTypeLLMCall         EventType = "llm_call"
	EventTypeToolCall        EventType = "tool_call"
	EventTypeCheckpoint      EventType = "checkpoint"
	EventTypeException       EventType = "exception"
	EventTypeValidatorResult EventType = "validator_result"
	EventTypeDriftSignal     EventType = "drift_signal"
	EventTypeInjectionAlert  EventType = "injection_alert"
	EventTypeInfrastructure  EventType = "infrastructure_event"
	EventTypeDLPScanResult   EventType = "dlp_scan_result"
	EventTypeMCPCall         EventType = "mcp_call"
	EventTypeEvalScore       EventType = "eval_score"
	EventTypeMemoryOperation EventType = "memory_operation"
	// EventTypeAgentHandoff (Mesedi #11). Emitted when one agent
	// delegates a task to another agent (sub-agent invocation,
	// supervisor/worker handoff, plan/execute split). The payload
	// carries the source agent identity, destination agent identity,
	// the optional child execution_id (resolved when the SDK opens a
	// nested @wrap for the handoff target), and a short task summary.
	// Downstream detectors join handoff events back to the topology
	// graph (#10) to surface cascading_failure (#12) and
	// coordination_deadlock (#13).
	EventTypeAgentHandoff EventType = "agent_handoff"
	// EventTypeHumanIntervention (Mesedi #19). Captures one full
	// human-in-the-loop ask/answer cycle as a single event,
	// emitted at the moment the human's response lands and the
	// agent is about to be unblocked. The downstream HITL
	// detectors (#20 hitl_timeout, #21 hitl_rejection_spike)
	// aggregate these events to surface SLA breaches and
	// quality regressions detected via human rejections.
	EventTypeHumanIntervention EventType = "human_intervention"
)

// ExecutionStatus is the lifecycle state of an Execution. There are
// three categories:
//
//  1. Running: `started`. The agent is actively working.
//  2. Paused: `awaiting_human` (Mesedi #18). The agent is suspended,
//     typically because it called the HITL request helper, and is
//     waiting on a human decision before it can continue. Paused
//     executions are NOT terminal: detectors do not fire on them
//     (a paused run is not a failed run), and wall-clock budget
//     checks subtract accumulated paused time so a long HITL wait
//     does not falsely trip a time_budget alert.
//  3. Terminal: `completed`, `crashed`, `halted`, `timeout`,
//     `validation_failed`. Exactly one terminal status is recorded
//     per execution; the SDK transitions the execution from
//     `started` (or `awaiting_human`) to its terminal state at the
//     exit boundary.
//
// Valid state transitions:
//
//	started        ──pause──►  awaiting_human
//	awaiting_human ──resume─►  started
//	started        ──exit──►   <any terminal>
//	awaiting_human ──exit──►   <any terminal>  (HITL timeout, halt)
type ExecutionStatus string

const (
	StatusStarted          ExecutionStatus = "started"
	StatusAwaitingHuman    ExecutionStatus = "awaiting_human"
	StatusCompleted        ExecutionStatus = "completed"
	StatusCrashed          ExecutionStatus = "crashed"
	StatusHalted           ExecutionStatus = "halted"
	StatusTimeout          ExecutionStatus = "timeout"
	StatusValidationFailed ExecutionStatus = "validation_failed"
)

// IsTerminal reports whether the supplied status is a terminal
// lifecycle state. `started` and `awaiting_human` are non-terminal;
// every other status terminates the execution. Centralized here so
// detectors and budget enforcers stay in sync as the lifecycle
// grows (Mesedi #18 added `awaiting_human`).
func (s ExecutionStatus) IsTerminal() bool {
	switch s {
	case StatusCompleted, StatusCrashed, StatusHalted, StatusTimeout, StatusValidationFailed:
		return true
	default:
		return false
	}
}

// IsPaused reports whether the supplied status represents a paused
// (non-terminal) execution. Distinct from `started` because the
// detector chain treats paused runs as "not in a position to be
// evaluated yet" rather than "actively making progress" (Mesedi #18).
func (s ExecutionStatus) IsPaused() bool {
	return s == StatusAwaitingHuman
}

// Execution is the root record for one agent invocation. The SDK posts
// an Execution to POST /executions at the agent's entry point and PATCHes
// the same execution_id with a terminal status at the exit boundary.
//
// Concept-doc reference: §6.1 executions table.
type Execution struct {
	ExecutionID       string          `json:"execution_id"`
	ProjectID         string          `json:"project_id"`
	ParentExecutionID *string         `json:"parent_execution_id,omitempty"`
	Status            ExecutionStatus `json:"status"`
	StartedAt         time.Time       `json:"started_at"`
	EndedAt           *time.Time      `json:"ended_at,omitempty"`
	DurationMs        int64           `json:"duration_ms,omitempty"`
	TotalTokensIn     int             `json:"total_tokens_in,omitempty"`
	TotalTokensOut    int             `json:"total_tokens_out,omitempty"`
	EstimatedCostUSD  float64         `json:"estimated_cost_usd,omitempty"`
	InputSummary      string          `json:"input_summary,omitempty"`
	OutputSummary     string          `json:"output_summary,omitempty"`
	CrashSignature    string          `json:"crash_signature,omitempty"`
	SDKVersion        string          `json:"sdk_version,omitempty"`
	SDKLanguage       string          `json:"sdk_language,omitempty"` // "python" | "typescript"
	// FailureGroupID is populated when this execution was clustered into
	// a failure_group by the detection pipeline. The dashboard uses this
	// to render a "Flagged by [class] / [signature]" banner on the
	// execution detail page and link back to the group. nil for clean
	// executions and for executions that haven't yet been processed by
	// the detection pipeline.
	FailureGroupID *string `json:"failure_group_id,omitempty"`
	// TenantID is the caller-supplied end-user / customer identifier
	// in the host SaaS application. Optional; absent for single-tenant
	// projects. Used by the cost-by-tenant report (Mesedi #5) to break
	// down a project's cost across the customers driving it. nil =
	// "not supplied"; non-nil pointer to "" = "supplied as empty
	// string" (treated as a deliberate, distinct value).
	TenantID *string `json:"tenant_id,omitempty"`
	// APIKeyID records which API key authenticated the request that
	// created this execution (#255). Stamped server-side from the auth
	// middleware context; any value the SDK sends in the request body
	// is overwritten in HandleCreateExecution. Surfaced on reads so the
	// admin "key recent use" report (#257) can scope a forensic audit
	// to a single key after a compromise. nil for executions written
	// before this column shipped (cannot reconstruct attribution
	// historically; see migration 035 design notes).
	APIKeyID *string `json:"api_key_id,omitempty"`
	// PausedAt is the timestamp at which the execution entered the
	// currently-active human-in-the-loop pause cycle (Mesedi #18).
	// nil when the execution is not paused. Cleared on resume.
	PausedAt *time.Time `json:"paused_at,omitempty"`
	// TotalPausedMs is the accumulated wall-clock duration the
	// execution has spent paused, summed across every pause/resume
	// cycle. Wall-clock budget enforcement subtracts this from the
	// raw elapsed duration so a long HITL wait does not falsely
	// trip a time_budget alert. The HITL SLA detectors (#20 and
	// #21) read this to compute wait duration and pause-cycle
	// frequency.
	TotalPausedMs int64 `json:"total_paused_ms,omitempty"`
	// PauseCount is the number of times the execution has paused.
	// Cumulative; not reset on resume. Used by hitl_rejection_spike
	// (#21) which fires when a single execution accumulates an
	// abnormal number of human-intervention cycles within its run.
	PauseCount int `json:"pause_count,omitempty"`
}

// Event is the polymorphic envelope for every recorded step in an execution.
// Payload's interpretation is determined by EventType, handlers may
// unmarshal Payload into the corresponding typed struct (LLMCallPayload,
// ToolCallPayload, etc.) using json.Unmarshal.
//
// Concept-doc reference: §6.2 events table.
type Event struct {
	EventID     string          `json:"event_id"`
	ExecutionID string          `json:"execution_id"`
	EventType   EventType       `json:"event_type"`
	Sequence    int             `json:"sequence"`
	Timestamp   time.Time       `json:"timestamp"`
	DurationMs  int64           `json:"duration_ms,omitempty"`
	Payload     json.RawMessage `json:"payload"`
}

// ─────────────────────────────────────────────────────────────────────────
// Typed payloads (decoded from Event.Payload based on EventType)
//
// Each payload corresponds to one EventType value above. Keeping payloads
// as separate types, rather than fields on a single Event struct, lets
// the schema evolve per event class without breaking the wire format.
// ─────────────────────────────────────────────────────────────────────────

// LLMCallPayload is the recorded shape of a single foundation-model API
// call made by the agent (Anthropic, OpenAI, Cursor, etc.).
type LLMCallPayload struct {
	Provider     string   `json:"provider"`                // "anthropic" | "openai" | ...
	Model        string   `json:"model"`                   // e.g., "claude-opus-4-6"
	SystemPrompt string   `json:"system_prompt,omitempty"` // SHA-256 acceptable for redaction mode
	UserPrompt   string   `json:"user_prompt,omitempty"`
	Response     string   `json:"response,omitempty"`
	InputTokens  int      `json:"input_tokens,omitempty"`
	OutputTokens int      `json:"output_tokens,omitempty"`
	LatencyMs    int64    `json:"latency_ms,omitempty"`
	CostUSD      float64  `json:"cost_usd,omitempty"`
	FinishReason string   `json:"finish_reason,omitempty"` // "stop" | "length" | "tool_use" | ...
	Temperature  *float64 `json:"temperature,omitempty"`
}

// MemoryOperationPayload is one read / write / search against an
// external memory store (vector DB, key-value cache, document store
// holding agent state). Captures enough to drive cost attribution
// per store and to feed downstream detectors (grounding_failure
// already reads eval_score; future versions may correlate retrieval
// confidence here with output quality there).
//
// Operation values: "read" / "write" / "search" / "delete".
// StoreType / StoreName let customers cost-attribute by backend
// (Pinecone, Weaviate, pgvector, Mem0) and by named collection.
type MemoryOperationPayload struct {
	Operation     string  `json:"operation"`                // "read" | "write" | "search" | "delete"
	StoreType     string  `json:"store_type,omitempty"`     // "pinecone" | "weaviate" | "pgvector" | "qdrant" | "mem0" | ...
	StoreName     string  `json:"store_name,omitempty"`     // collection / index name
	Query         string  `json:"query,omitempty"`          // semantic search query (truncated)
	DocumentCount int     `json:"document_count,omitempty"` // for search: how many docs returned
	TokenCount    int     `json:"token_count,omitempty"`    // total tokens in the retrieved/written payload
	TopScore      float64 `json:"top_score,omitempty"`      // semantic confidence of the highest-scoring result
	LatencyMs     int64   `json:"latency_ms,omitempty"`     // wall-clock duration of the operation
	CacheHit      bool    `json:"cache_hit,omitempty"`      // true if the call was satisfied from a local cache
	Error         string  `json:"error,omitempty"`          // when the operation failed
	ErrorClass    string  `json:"error_class,omitempty"`    // "timeout" | "auth_error" | "rate_limited" | ...
}

// EvalScorePayload is one external evaluator's verdict on an
// execution's output. Mesedi does not run the evaluation itself;
// customers compute scores via Ragas, Promptfoo, Vectara HHEM, an
// LLM-judge, or their own custom evaluator and emit this event with
// the result. Mesedi #14 (grounding_failure, Tier 3) aggregates
// these events over time windows to fire alerts when scores trend
// below threshold.
//
// First-pass design notes:
//
//  1. The event is purely ingestion-only at v1. There is no detector
//     that fires on a single eval_score event. Downstream detectors
//     subscribe to aggregations (mean score over N runs for a given
//     evaluator_id) which is where the actual signal lives.
//
//  2. metric_type is intentionally an open string. We ship a list of
//     well-known values ("faithfulness", "relevance",
//     "hallucination_rate", "answer_correctness") in the SDK docs,
//     but the backend doesn't enforce them. Customers running custom
//     evaluators can pick their own names without coordinating with
//     us.
//
//  3. score is a float in [0, 1]. Higher = better when applicable
//     (faithfulness, relevance); for inverse metrics like
//     hallucination_rate, lower = better. The threshold field lets
//     evaluators communicate which direction "pass" is.
type EvalScorePayload struct {
	EvaluatorID    string  `json:"evaluator_id"`         // stable id, e.g. "ragas/faithfulness" | "vectara-hhem/v1" | "custom:my-judge"
	MetricType     string  `json:"metric_type"`          // "faithfulness" | "relevance" | "hallucination_rate" | "answer_correctness" | ...
	Score          float64 `json:"score"`                // numeric value the evaluator produced
	Passed         bool    `json:"passed"`               // evaluator's pass/fail verdict per its own threshold
	Threshold      float64 `json:"threshold,omitempty"`  // optional, the cutoff the evaluator used
	HigherIsBetter bool    `json:"higher_is_better"`     // true for faithfulness; false for hallucination_rate
	Reason         string  `json:"reason,omitempty"`     // optional, the evaluator's explanation
	Confidence     float64 `json:"confidence,omitempty"` // 0..1 if the evaluator reports its own confidence
}

// HumanInterventionPayload (Mesedi #19). One full HITL ask/answer
// cycle: the agent paused with a question, a human eventually
// responded, the agent resumed. Emitted at completion so the
// event row contains both halves and downstream detectors can
// query it as a single record.
//
// Well-known response_kind values:
//
//   - "approved"  human said yes, agent proceeds with original plan
//   - "rejected"  human said no, agent must abandon or replan
//   - "edited"    human modified the proposed output before approving
//   - "timeout"   SLA expired before a human responded
//   - "cancelled" operator killed the wait without a decision
//
// Customers may use other strings; the backend does not enforce
// the enumeration (mirrors the open-string posture of
// EvalScorePayload.metric_type). The five values above are the
// ones #20 and #21 detectors recognize as well-known signals.
//
// request_id is a SDK-generated UUID that correlates the ask side
// with the answer side in any host system that needs to plumb the
// question through a queue, a database row, or a websocket. The
// event itself is emitted once on completion; request_id is just
// the customer-facing handle.
//
// wait_duration_ms is decided_at minus requested_at, computed by
// the SDK and stored verbatim so the dashboard can render
// "approved after 14 minutes" without redoing the arithmetic.
type HumanInterventionPayload struct {
	RequestID       string                 `json:"request_id"`
	Question        string                 `json:"question"`
	SLASeconds      int64                  `json:"sla_seconds,omitempty"`
	RequestedAt     string                 `json:"requested_at"`
	ResponseKind    string                 `json:"response_kind"`
	ResponsePayload map[string]interface{} `json:"response_payload,omitempty"`
	DecidedBy       string                 `json:"decided_by,omitempty"`
	DecidedAt       string                 `json:"decided_at"`
	WaitDurationMs  int64                  `json:"wait_duration_ms"`
	Metadata        map[string]interface{} `json:"metadata,omitempty"`
}

// AgentHandoffPayload (Mesedi #11). Captures the moment one agent
// delegates a task to another agent. The from/to identities are
// stable agent role names ("planner", "researcher", "qa", or any
// app-specific label). When the SDK opens a nested @wrap for the
// handoff target, ChildExecutionID is populated so downstream
// detectors can correlate the handoff edge with the topology
// graph (#10). The handoff_kind discriminates the common shapes:
//
//   - "delegate"  one-shot, expects a return value
//   - "spawn"     fire-and-forget background sub-agent
//   - "transfer"  control transferred (no return)
//   - "consult"   short Q&A, return text only
//
// Customers pick a value; we ship the four above as well-known
// strings but do not enforce them at the backend, mirroring the
// open-string posture of EvalScorePayload.metric_type.
//
// Latency is the wall-clock cost the SOURCE agent paid waiting on
// the handoff to return (zero for "spawn" / "transfer"). The
// detector for cascading_failure (#12) reads this alongside the
// child execution's terminal status to fire alerts when a
// handoff's child crashes within a short window after the
// handoff event.
type AgentHandoffPayload struct {
	FromAgent        string `json:"from_agent"`                   // calling agent role/name
	ToAgent          string `json:"to_agent"`                     // target agent role/name
	HandoffKind      string `json:"handoff_kind,omitempty"`       // "delegate" | "spawn" | "transfer" | "consult"
	TaskSummary      string `json:"task_summary,omitempty"`       // short description of the delegated task
	ChildExecutionID string `json:"child_execution_id,omitempty"` // populated when the SDK resolved the nested execution id
	LatencyMs        int64  `json:"latency_ms,omitempty"`         // wall-clock duration the source agent waited on the handoff
	Error            string `json:"error,omitempty"`              // if the handoff itself failed (not the child's work)
	ErrorClass       string `json:"error_class,omitempty"`        // "timeout" | "unreachable" | "rejected" | ...
}

// MCPCallPayload is one invocation of a Model Context Protocol
// server's method. Distinct from ToolCallPayload because MCP routes
// through a separate server identity (e.g. an Anthropic-provided
// `filesystem` server, a customer's internal `crm-mcp` server),
// which matters for cost attribution and provider-incident
// correlation. The dashboard renders the server identity prominently
// so SREs can see "we spent $X on Anthropic MCP servers vs $Y on
// third-party servers" without inspecting tool args.
//
// Per-call structure mirrors ToolCallPayload so the existing
// tool_failures detector can pick up failed MCP calls without code
// changes: ServerName + "." + Method is the cluster signature when
// Error / ErrorClass is set.
type MCPCallPayload struct {
	ServerName  string          `json:"server_name"`          // e.g. "filesystem", "github", "crm-mcp"
	ServerURL   string          `json:"server_url,omitempty"` // e.g. "stdio:./filesystem-mcp" or "https://mcp.example.com"
	Method      string          `json:"method"`               // e.g. "read_file", "list_resources"
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	ReturnValue json.RawMessage `json:"return_value,omitempty"`
	LatencyMs   int64           `json:"latency_ms,omitempty"`
	Error       string          `json:"error,omitempty"`
	ErrorClass  string          `json:"error_class,omitempty"` // "hard_error" | "soft_error" | "timeout" | "server_unreachable" | "method_not_found"
}

// ToolCallPayload is one invocation of a developer-registered tool.
type ToolCallPayload struct {
	ToolName    string          `json:"tool_name"`
	Arguments   json.RawMessage `json:"arguments,omitempty"`
	ReturnValue json.RawMessage `json:"return_value,omitempty"`
	LatencyMs   int64           `json:"latency_ms,omitempty"`
	Error       string          `json:"error,omitempty"`
	ErrorClass  string          `json:"error_class,omitempty"` // "hard_error" | "soft_error" | "timeout" | "hallucinated_name" | "malformed_args"
}

// CheckpointPayload captures the agent's working state at a step boundary.
// Emitted automatically at each LLM-call boundary, or manually via
// argusly.checkpoint() / mesedi.checkpoint() in the SDK.
type CheckpointPayload struct {
	State      json.RawMessage `json:"state"`
	StepNumber int             `json:"step_number"`
	Note       string          `json:"note,omitempty"`
}

// ExceptionPayload is the recorded crash that propagated out of the agent
// entry point.
type ExceptionPayload struct {
	ExceptionType  string `json:"exception_type"`
	Message        string `json:"message"`
	StackTrace     string `json:"stack_trace"`
	StackSignature string `json:"stack_signature,omitempty"` // first-5-frames hash for grouping
}

// ValidatorResultPayload is the outcome of one developer-defined output
// validator running against an agent's terminal output.
type ValidatorResultPayload struct {
	ValidatorName string `json:"validator_name"`
	ValidatorType string `json:"validator_type"` // "schema" | "regex" | "length" | "reference_check" | "source_attribution" | "llm_judge" | "custom"
	Passed        bool   `json:"passed"`
	Reason        string `json:"reason,omitempty"`
}

// DriftSignalPayload is the outcome of one drift-detection pass, emitted
// periodically (at step boundaries or on judge-invocation cadence) when
// the composite drift score crosses configured thresholds.
type DriftSignalPayload struct {
	CompositeScore       float64 `json:"composite_score"` // 0..1
	SemanticDistance     float64 `json:"semantic_distance,omitempty"`
	PathwayEditDistance  int     `json:"pathway_edit_distance,omitempty"`
	ToolSequenceDistance int     `json:"tool_sequence_distance,omitempty"`
	JudgeStatus          string  `json:"judge_status,omitempty"` // "on_track" | "drifting"
	JudgeReason          string  `json:"judge_reason,omitempty"`
	Confidence           float64 `json:"confidence,omitempty"` // 0..1
}

// InfrastructureEventPayload is the recorded shape of one
// infrastructure-layer backpressure signal: a provider rate-limit
// (HTTP 429), a token-bucket exhaustion, or a local circuit-breaker
// trip. Distinct from tool_call.error because the failure isn't in
// the agent's logic or the developer's tool, it's in the underlying
// transport / quota plane. The infrastructure_throttled detector
// consumes these events to differentiate "your agent is buggy" from
// "your quota is undersized."
//
// EventType discriminates between the three sub-cases:
//
//   - "rate_limit"     a provider returned HTTP 429 or signalled
//     x-ratelimit-remaining=0 in headers
//   - "circuit_breaker" the SDK's local circuit-breaker tripped open
//     and stopped sending requests to a provider
//   - "quota_exhausted" the upstream provider returned a hard quota
//     error (different from 429: hard means you've
//     consumed your monthly cap, not your per-minute)
//
// Signature pieces for clustering live in (Provider, Endpoint, Reason);
// see store.ThrottlingSignature for the canonical assembly.
type InfrastructureEventPayload struct {
	EventType        string `json:"event_type"`                   // "rate_limit" | "circuit_breaker" | "quota_exhausted"
	Provider         string `json:"provider,omitempty"`           // "anthropic" | "openai" | ...
	Endpoint         string `json:"endpoint,omitempty"`           // e.g. "/v1/messages"
	StatusCode       int    `json:"status_code,omitempty"`        // HTTP status (429 etc.)
	RetryAfterMs     int64  `json:"retry_after_ms,omitempty"`     // server-suggested backoff
	QuotaRemaining   int    `json:"quota_remaining,omitempty"`    // from x-ratelimit-remaining
	QuotaLimit       int    `json:"quota_limit,omitempty"`        // from x-ratelimit-limit
	QuotaDimension   string `json:"quota_dimension,omitempty"`    // "tokens_per_minute" | "requests_per_second" | ...
	BackoffAppliedMs int64  `json:"backoff_applied_ms,omitempty"` // how long the SDK actually waited
	CircuitState     string `json:"circuit_state,omitempty"`      // "open" | "half_open" | "closed"
}

// DLPScanResultHit is one rule's roll-up inside DLPScanResultPayload.
// Mirrors dlp.HitSummary exactly so the package can JSON-marshal the
// slice straight into the payload field.
type DLPScanResultHit struct {
	RuleID   string `json:"rule_id"`
	Label    string `json:"label"`
	Severity string `json:"severity"` // "critical" | "high" | "medium"
	Count    int    `json:"count"`
}

// DLPScanResultPayload is the recorded outcome of one Data Loss
// Prevention scan against an outbound payload (LLM prompt or tool
// arguments). Mirrors prompt_injection's structure but on the
// outbound side: the scan layer indicates which event field was
// matched ("llm_prompt" | "tool_arguments"), the parent_event_id
// links back to the redacted event the scan was triggered by, and
// Hits is the per-rule rollup of matches.
//
// The data_leakage detector consumes these events: every hit with
// SeverityCritical fires the cluster, every hit with SeverityHigh
// records a warning-tier cluster, and SeverityMedium hits record but
// don't fire (under threshold).
type DLPScanResultPayload struct {
	ScanLayer       string             `json:"scan_layer"`                  // "llm_prompt" | "tool_arguments" | "tool_return"
	ParentEventID   string             `json:"parent_event_id,omitempty"`   // the redacted event this scan was derived from
	ParentEventType string             `json:"parent_event_type,omitempty"` // "llm_call" | "tool_call"
	HighestSeverity string             `json:"highest_severity"`            // for fast filtering
	HitCount        int                `json:"hit_count"`                   // sum of Count across Hits
	Hits            []DLPScanResultHit `json:"hits"`                        // per-rule rollup
	Action          string             `json:"action"`                      // "redacted" | "alerted" (medium-only)
}

// InjectionAlertPayload is the outcome of one prompt-injection / boundary-
// violation scan, fired by the input-scan, tool-return-scan, or output-
// scan layer of §4.7 detection.
type InjectionAlertPayload struct {
	ScanLayer       string  `json:"scan_layer"` // "input" | "tool_return" | "output"
	SignatureMatch  string  `json:"signature_match,omitempty"`
	ClassifierScore float64 `json:"classifier_score,omitempty"`
	Confidence      float64 `json:"confidence"`
	Action          string  `json:"action"`            // "alerted" | "stripped" | "wrapped" | "halted"
	Excerpt         string  `json:"excerpt,omitempty"` // redacted/truncated content
}
