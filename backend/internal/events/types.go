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
)

// ExecutionStatus is the lifecycle state of an Execution. Exactly one
// terminal status (anything other than "started") is recorded per
// execution; the SDK transitions an execution from "started" to its
// terminal state on completion, crash, or halt.
type ExecutionStatus string

const (
	StatusStarted          ExecutionStatus = "started"
	StatusCompleted        ExecutionStatus = "completed"
	StatusCrashed          ExecutionStatus = "crashed"
	StatusHalted           ExecutionStatus = "halted"
	StatusTimeout          ExecutionStatus = "timeout"
	StatusValidationFailed ExecutionStatus = "validation_failed"
)

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
	Provider     string `json:"provider"`                // "anthropic" | "openai" | ...
	Model        string `json:"model"`                   // e.g., "claude-opus-4-6"
	SystemPrompt string `json:"system_prompt,omitempty"` // SHA-256 acceptable for redaction mode
	UserPrompt   string `json:"user_prompt,omitempty"`
	Response     string `json:"response,omitempty"`
	InputTokens  int    `json:"input_tokens,omitempty"`
	OutputTokens int    `json:"output_tokens,omitempty"`
	LatencyMs    int64  `json:"latency_ms,omitempty"`
	CostUSD      float64 `json:"cost_usd,omitempty"`
	FinishReason string `json:"finish_reason,omitempty"` // "stop" | "length" | "tool_use" | ...
	Temperature  *float64 `json:"temperature,omitempty"`
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
	State         json.RawMessage `json:"state"`
	StepNumber    int             `json:"step_number"`
	Note          string          `json:"note,omitempty"`
}

// ExceptionPayload is the recorded crash that propagated out of the agent
// entry point.
type ExceptionPayload struct {
	ExceptionType    string `json:"exception_type"`
	Message          string `json:"message"`
	StackTrace       string `json:"stack_trace"`
	StackSignature   string `json:"stack_signature,omitempty"` // first-5-frames hash for grouping
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
	CompositeScore        float64 `json:"composite_score"` // 0..1
	SemanticDistance      float64 `json:"semantic_distance,omitempty"`
	PathwayEditDistance   int     `json:"pathway_edit_distance,omitempty"`
	ToolSequenceDistance  int     `json:"tool_sequence_distance,omitempty"`
	JudgeStatus           string  `json:"judge_status,omitempty"` // "on_track" | "drifting"
	JudgeReason           string  `json:"judge_reason,omitempty"`
	Confidence            float64 `json:"confidence,omitempty"` // 0..1
}

// InjectionAlertPayload is the outcome of one prompt-injection / boundary-
// violation scan, fired by the input-scan, tool-return-scan, or output-
// scan layer of §4.7 detection.
type InjectionAlertPayload struct {
	ScanLayer       string  `json:"scan_layer"` // "input" | "tool_return" | "output"
	SignatureMatch  string  `json:"signature_match,omitempty"`
	ClassifierScore float64 `json:"classifier_score,omitempty"`
	Confidence      float64 `json:"confidence"`
	Action          string  `json:"action"` // "alerted" | "stripped" | "wrapped" | "halted"
	Excerpt         string  `json:"excerpt,omitempty"` // redacted/truncated content
}
