/**
 * Event types and execution records, the TypeScript mirror of the Go
 * backend's wire format and the Python SDK's data model.
 *
 * Source-of-truth lives in the Go backend at
 * `backend/internal/events/types.go`. Any new EventType or Status
 * added there MUST be added here too, strict-JSON decoding on the
 * backend will reject events whose fields it doesn't recognize.
 *
 * The wire format uses RFC 3339 UTC timestamps for every time field.
 */

/**
 * RFC 3339 UTC timestamp with microsecond precision, the format the
 * Go backend's time.Time RFC3339 marshaller accepts. Example:
 * "2026-05-14T22:17:33.123456Z".
 *
 * JavaScript's `Date.prototype.toISOString()` produces millisecond
 * precision; we pad to microseconds for byte-identical compatibility
 * with the Python SDK's output (which uses Python's %f for
 * microseconds).
 */
export function utcNowRfc3339(): string {
  const d = new Date();
  // toISOString gives "...123Z" (millisecond precision). Strip the Z,
  // add three zero-padded extra digits to reach microseconds, then
  // re-attach Z. Matches the Python SDK formatter exactly.
  return d.toISOString().replace(/Z$/, "000Z");
}

/**
 * Seven event types. Must match `EventType` constants in
 * backend/internal/events/types.go exactly.
 */
export const EventType = {
  LLM_CALL: "llm_call",
  TOOL_CALL: "tool_call",
  CHECKPOINT: "checkpoint",
  EXCEPTION: "exception",
  VALIDATOR_RESULT: "validator_result",
  DRIFT_SIGNAL: "drift_signal",
  INJECTION_ALERT: "injection_alert",
} as const;
export type EventType = (typeof EventType)[keyof typeof EventType];

/**
 * Execution lifecycle states. Must match `ExecutionStatus` constants
 * in backend/internal/events/types.go exactly.
 */
export const Status = {
  STARTED: "started",
  COMPLETED: "completed",
  CRASHED: "crashed",
  HALTED: "halted",
  TIMEOUT: "timeout",
  VALIDATION_FAILED: "validation_failed",
} as const;
export type Status = (typeof Status)[keyof typeof Status];

/**
 * An agent run as the Mesedi backend records it. Mirrors the Python
 * SDK's `Execution` dataclass and the Go backend's struct.
 */
export interface Execution {
  execution_id: string;
  status: Status;
  started_at: string;
  ended_at?: string;
  duration_ms?: number;
  total_tokens_in?: number;
  total_tokens_out?: number;
  estimated_cost_usd?: number;
  sdk_language: "typescript";
  sdk_version: string;
  crash_signature?: string;
}

/**
 * A single observation within an execution. Payload is opaque JSON , 
 * the backend stores it as a raw blob; per-event-type payload shapes
 * are defined in the typed payload helpers below.
 */
export interface Event {
  event_id: string;
  execution_id: string;
  event_type: EventType;
  sequence: number;
  timestamp: string;
  duration_ms?: number;
  payload: Record<string, unknown>;
}

/**
 * Build the body for POST /executions, only the fields valid at
 * execution start.
 */
export function executionStartPayload(e: Execution): Record<string, unknown> {
  return {
    execution_id: e.execution_id,
    status: e.status,
    started_at: e.started_at,
    sdk_language: e.sdk_language,
    sdk_version: e.sdk_version,
  };
}

/**
 * Build the body for PATCH /executions/{id}, omit any undefined
 * fields so the backend's strict-decode doesn't reject them.
 */
export function executionEndPayload(e: Execution): Record<string, unknown> {
  const out: Record<string, unknown> = { status: e.status };
  if (e.ended_at !== undefined) out["ended_at"] = e.ended_at;
  if (e.duration_ms !== undefined) out["duration_ms"] = e.duration_ms;
  if (e.total_tokens_in !== undefined) out["total_tokens_in"] = e.total_tokens_in;
  if (e.total_tokens_out !== undefined) out["total_tokens_out"] = e.total_tokens_out;
  if (e.estimated_cost_usd !== undefined) out["estimated_cost_usd"] = e.estimated_cost_usd;
  if (e.crash_signature !== undefined) out["crash_signature"] = e.crash_signature;
  return out;
}

/**
 * Wire-format dict for POST /events (which accepts an array).
 */
export function eventToWire(e: Event): Record<string, unknown> {
  const out: Record<string, unknown> = {
    event_id: e.event_id,
    execution_id: e.execution_id,
    event_type: e.event_type,
    sequence: e.sequence,
    timestamp: e.timestamp,
    payload: e.payload,
  };
  if (e.duration_ms !== undefined) out["duration_ms"] = e.duration_ms;
  return out;
}
