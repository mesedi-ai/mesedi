/**
 * Direct-emission helpers for events that don't fit the HOF pattern.
 *
 * `wrap()` and `tool()` wrap functions; `checkpoint()` and
 * `validatorResult()` are markers inserted at points of interest
 * inside agent code, often inside the same function `wrap()` already
 * covers. For those, a plain function call is the right API:
 *
 *     checkpoint("after_retrieval", { documents: 5, used_cache: true });
 *
 *     if (!result) {
 *       validatorResult("non-empty-response", false, {
 *         message: "LLM returned empty content",
 *         severity: "error",
 *       });
 *     }
 *
 * Both helpers no-op silently when called outside an active wrap()
 * execution context, same fail-open pattern as `tool()`.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";

const MAX_VALIDATOR_MSG = 500;

/**
 * Emit a `checkpoint` event marking a notable point in execution.
 *
 * A checkpoint is a free-form marker: a name + arbitrary metadata.
 * Typical uses: "after_retrieval", "before_synthesis", "cache_hit".
 * Useful both for Phase 3+ detector hooks (drift, cost-velocity)
 * and for ad-hoc debugging, replay UI in a future phase will
 * render checkpoints as anchored markers on the execution timeline.
 *
 * Outside `wrap()`: silent no-op.
 */
export function checkpoint(
  name: string,
  metadata: Record<string, unknown> = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  // Halt-safe boundary: checkpoint is the canonical place for users
  // to insert their own "ok to halt here" markers. Budget check runs
  // first so a halt fires before the event is emitted; the user's
  // checkpoint call effectively becomes a yield point for halt.
  ctx.checkBudget();
  if (ctx.budgetTracker) {
    ctx.budgetTracker.incrementSteps();
  }

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.CHECKPOINT,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload: { name, metadata },
  };
  client.submitEvent(event);
}

export type ValidatorSeverity = "warning" | "error" | "critical";

export interface ValidatorResultOptions {
  message?: string;
  severity?: ValidatorSeverity;
}

/**
 * Report a validator outcome as a `validator_result` event.
 *
 * Validators are checks the agent (or its framework) runs against
 * intermediate or final outputs: schema conformance, factuality,
 * relevance, safety. The result, pass or fail, becomes a discrete
 * event so Phase-3 detection can spot patterns like "validator X
 * has been failing 90% of the time on this model."
 *
 * Outside `wrap()`: silent no-op.
 */
export function validatorResult(
  name: string,
  passed: boolean,
  opts: ValidatorResultOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  let severity: ValidatorSeverity = opts.severity ?? "error";
  if (
    severity !== "warning" &&
    severity !== "error" &&
    severity !== "critical"
  ) {
    // Don't throw, the caller's agent shouldn't fail because of an
    // SDK-side validation. Coerce to the safest default.
    severity = "error";
  }

  const payload: Record<string, unknown> = { name, passed, severity };
  if (opts.message) {
    payload["message"] = opts.message.slice(0, MAX_VALIDATOR_MSG);
  }

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.VALIDATOR_RESULT,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  client.submitEvent(event);
}

/**
 * Options for emitInfrastructureEvent. Mirrors the Go
 * InfrastructureEventPayload struct in
 * backend/internal/events/types.go field-for-field, omitting `reason`
 * which is the positional first arg of the emitter.
 */
export interface InfrastructureEventOptions {
  /** Short provider identifier ("anthropic", "openai", "google"...). */
  provider?: string;
  /** Provider URL path that triggered the event. */
  endpoint?: string;
  /** HTTP status returned (429, 503...). */
  statusCode?: number;
  /** Server-suggested backoff window in ms (from Retry-After). */
  retryAfterMs?: number;
  /** Calls / tokens still available, from x-ratelimit-remaining. */
  quotaRemaining?: number;
  /** Maximum on this quota, from x-ratelimit-limit. */
  quotaLimit?: number;
  /**
   * Stable string identifier for the quota dimension that breached
   * ("tokens_per_minute", "requests_per_second", etc.). Used as a
   * signature dimension by the backend's infrastructure_throttled
   * detector.
   */
  quotaDimension?: string;
  /** How long the caller actually waited before retrying. */
  backoffAppliedMs?: number;
  /**
   * When reason="circuit_breaker", one of "open" / "half_open" /
   * "closed". Empty defaults to "open" in the backend signature
   * assembly.
   */
  circuitState?: string;
}

/**
 * Reason codes recognized by ThrottlingSignature on the backend.
 * Other strings are accepted (they cluster as "<reason>:<provider>")
 * so SDKs can emit forward-compatible reasons before the detector
 * adds explicit support.
 */
export type InfrastructureReason =
  | "rate_limit"
  | "circuit_breaker"
  | "quota_exhausted"
  | (string & {});

/**
 * Emit an `infrastructure_event` for transport-plane backpressure.
 *
 * Used when the SDK or caller observes an HTTP 429, hits a provider
 * quota header, trips a local circuit breaker, or otherwise gets a
 * signal from the network plane that the agent's logic is fine but
 * the underlying infrastructure is pushing back. The Mesedi backend's
 * `infrastructure_throttled` detector consumes these events and
 * clusters them by (reason, provider, quota_dimension) so SRE teams
 * get a single page per affected provider instead of one page per
 * request.
 *
 * Typical caller pattern (Vercel AI SDK / OpenAI / Anthropic):
 *
 *     try {
 *       const res = await fetch(endpoint, { method: "POST", body });
 *       if (res.status === 429) {
 *         emitInfrastructureEvent("rate_limit", {
 *           provider: "anthropic",
 *           endpoint: "/v1/messages",
 *           statusCode: 429,
 *           retryAfterMs: Number(
 *             res.headers.get("retry-after-ms") ?? 0,
 *           ),
 *           quotaDimension: "tokens_per_minute",
 *           quotaRemaining: Number(
 *             res.headers.get("x-ratelimit-remaining") ?? 0,
 *           ),
 *         });
 *       }
 *     } catch (err) {
 *       // ...
 *     }
 *
 * Outside `wrap()`: silent no-op. Mirrors the fail-open pattern of
 * every other observe-layer primitive. The SDK does NOT retry on
 * its own; emitting this event is purely observational.
 */
export function emitInfrastructureEvent(
  reason: InfrastructureReason,
  opts: InfrastructureEventOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = { event_type: reason };
  if (opts.provider) payload["provider"] = opts.provider;
  if (opts.endpoint) payload["endpoint"] = opts.endpoint;
  if (opts.statusCode) payload["status_code"] = Math.trunc(opts.statusCode);
  if (opts.retryAfterMs) payload["retry_after_ms"] = Math.trunc(opts.retryAfterMs);
  if (opts.quotaRemaining)
    payload["quota_remaining"] = Math.trunc(opts.quotaRemaining);
  if (opts.quotaLimit) payload["quota_limit"] = Math.trunc(opts.quotaLimit);
  if (opts.quotaDimension) payload["quota_dimension"] = opts.quotaDimension;
  if (opts.backoffAppliedMs)
    payload["backoff_applied_ms"] = Math.trunc(opts.backoffAppliedMs);
  if (opts.circuitState) payload["circuit_state"] = opts.circuitState;

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.INFRASTRUCTURE_EVENT,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  client.submitEvent(event);
}

/**
 * Options for emitMcpCall. Mirrors the Go MCPCallPayload struct in
 * backend/internal/events/types.go field-for-field, omitting
 * `serverName` and `method` which are positional args.
 */
export interface McpCallOptions {
  /** Optional server URL or stdio target for the expanded view. */
  serverUrl?: string;
  /** Method arguments (any JSON-serializable value). */
  arguments?: unknown;
  /** Successful return value; omit on error. */
  returnValue?: unknown;
  /** Wall-clock duration in milliseconds. */
  latencyMs?: number;
  /** Error message when the call failed. */
  error?: string;
  /** Failure classifier: "hard_error" / "soft_error" / "timeout" / "server_unreachable" / "method_not_found". */
  errorClass?: string;
}

/**
 * Emit an `mcp_call` event for one Model Context Protocol server
 * invocation.
 *
 * Use this when your agent talks to an MCP server (Anthropic's
 * filesystem / github servers, a customer-hosted MCP server, etc.).
 * The dashboard renders MCP calls in a distinct chip so cost
 * attribution can break down by server identity, and the existing
 * tool_failures detector picks up failed MCP calls when `error` or
 * `errorClass` is non-empty.
 *
 * Outside `wrap()`: silent no-op.
 */
export function emitMcpCall(
  serverName: string,
  method: string,
  opts: McpCallOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = {
    server_name: serverName,
    method,
  };
  if (opts.serverUrl) payload["server_url"] = opts.serverUrl;
  if (opts.arguments !== undefined) payload["arguments"] = opts.arguments;
  if (opts.returnValue !== undefined) payload["return_value"] = opts.returnValue;
  if (opts.latencyMs) payload["latency_ms"] = Math.trunc(opts.latencyMs);
  if (opts.error) payload["error"] = opts.error;
  if (opts.errorClass) payload["error_class"] = opts.errorClass;

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.MCP_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: opts.latencyMs,
    payload,
  };
  client.submitEvent(event);
}

/** Options for emitEvalScore beyond the required core fields. */
export interface EvalScoreOptions {
  /** Optional cutoff value the evaluator used. */
  threshold?: number;
  /**
   * True for faithfulness / relevance / correctness; false for
   * inverse metrics like hallucination_rate. Default true.
   */
  higherIsBetter?: boolean;
  /** Optional explanation the evaluator returned. */
  reason?: string;
  /** Optional [0, 1] self-confidence the evaluator reports. */
  confidence?: number;
}

/**
 * Emit an `eval_score` event recording one external evaluator
 * verdict on an execution's output.
 *
 * Use this when you run Ragas, Promptfoo, Vectara HHEM, or a custom
 * judge against an execution's output and want Mesedi to track the
 * score over time. Mesedi #14 (Tier 3) aggregates these into
 * grounding_failure clusters when scores trend below threshold.
 *
 * Outside `wrap()`: silent no-op.
 */
export function emitEvalScore(
  evaluatorId: string,
  metricType: string,
  score: number,
  passed: boolean,
  opts: EvalScoreOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = {
    evaluator_id: evaluatorId,
    metric_type: metricType,
    score,
    passed,
    higher_is_better: opts.higherIsBetter ?? true,
  };
  if (opts.threshold !== undefined) payload["threshold"] = opts.threshold;
  if (opts.reason) payload["reason"] = opts.reason;
  if (opts.confidence !== undefined) payload["confidence"] = opts.confidence;

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.EVAL_SCORE,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  client.submitEvent(event);
}
