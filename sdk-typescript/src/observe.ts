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

/** Options for emitMemoryOperation. */
export interface MemoryOperationOptions {
  storeType?: string;
  storeName?: string;
  query?: string;
  documentCount?: number;
  tokenCount?: number;
  topScore?: number;
  latencyMs?: number;
  cacheHit?: boolean;
  error?: string;
  errorClass?: string;
}

/**
 * Emit a `memory_operation` event for one external memory store
 * read / write / search.
 */
export function emitMemoryOperation(
  operation: "read" | "write" | "search" | "delete" | (string & {}),
  opts: MemoryOperationOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = { operation };
  if (opts.storeType) payload["store_type"] = opts.storeType;
  if (opts.storeName) payload["store_name"] = opts.storeName;
  if (opts.query) payload["query"] = opts.query.slice(0, 1000);
  if (opts.documentCount) payload["document_count"] = Math.trunc(opts.documentCount);
  if (opts.tokenCount) payload["token_count"] = Math.trunc(opts.tokenCount);
  if (opts.topScore) payload["top_score"] = opts.topScore;
  if (opts.latencyMs) payload["latency_ms"] = Math.trunc(opts.latencyMs);
  if (opts.cacheHit) payload["cache_hit"] = true;
  if (opts.error) payload["error"] = opts.error;
  if (opts.errorClass) payload["error_class"] = opts.errorClass;

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.MEMORY_OPERATION,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: opts.latencyMs,
    payload,
  };
  client.submitEvent(event);
}

/**
 * Options for {@link emitAgentHandoff}.
 *
 * Mesedi #11. Use this at the moment one agent invokes another
 * (supervisor/worker, plan/execute, role-based router). The
 * downstream cascading_failure detector (#12) joins this event
 * back to the topology graph (#10) so that a handoff whose child
 * execution crashes within a short window can be surfaced as
 * one logical failure rather than two unrelated ones.
 *
 * Well-known handoffKind values:
 *
 *  - "delegate"  one-shot, expects a return value
 *  - "spawn"     fire-and-forget background sub-agent
 *  - "transfer"  control transferred (no return)
 *  - "consult"   short Q&A, return text only
 */
export interface AgentHandoffOptions {
  handoffKind?: "delegate" | "spawn" | "transfer" | "consult" | (string & {});
  taskSummary?: string;
  childExecutionId?: string;
  latencyMs?: number;
  error?: string;
  errorClass?: string;
}

/**
 * Emit an `agent_handoff` event marking that the current agent
 * delegated work to another agent.
 *
 * Outside `@wrap` / `withExecution`: no-op.
 */
export function emitAgentHandoff(
  fromAgent: string,
  toAgent: string,
  opts: AgentHandoffOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = {
    from_agent: fromAgent,
    to_agent: toAgent,
  };
  if (opts.handoffKind) payload["handoff_kind"] = opts.handoffKind;
  if (opts.taskSummary) payload["task_summary"] = opts.taskSummary.slice(0, 1000);
  if (opts.childExecutionId) payload["child_execution_id"] = opts.childExecutionId;
  if (opts.latencyMs) payload["latency_ms"] = Math.trunc(opts.latencyMs);
  if (opts.error) payload["error"] = opts.error;
  if (opts.errorClass) payload["error_class"] = opts.errorClass;

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.AGENT_HANDOFF,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: opts.latencyMs,
    payload,
  };
  client.submitEvent(event);
}

/**
 * Synchronously transition the current execution to
 * `awaiting_human` (Mesedi #18).
 *
 * Call this from inside a `wrap()`-ed function at the moment the
 * agent reaches a decision that requires a human in the loop. The
 * backend records the pause timestamp, increments pause_count, and
 * starts the HITL clock. A subsequent {@link resumeForAgent} call
 * adds the elapsed time to total_paused_ms so a long HITL wait
 * does not falsely trip the agent's time budget.
 *
 * The host application is responsible for actually blocking the
 * agent until the human responds (typically the wrap()-ed function
 * body itself awaits a queue / websocket / database row). This
 * helper only updates Mesedi's lifecycle state; it does not block.
 *
 * Outside `wrap()`: no-op.
 *
 * Important: this is a SYNCHRONOUS PATCH (await) rather than the
 * asynchronous shipper path that events take. The pause / resume
 * transitions must be committed before the host application
 * suspends the agent, so async-only delivery would race against
 * the human responding.
 */
export async function pauseForHuman(): Promise<void> {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const client = getClient();
  // Drain pending shipper traffic first so the POST /executions
  // that opened this wrap() has landed on the backend. Without
  // this, the synchronous PATCH races the async POST and
  // returns 404 when pause is called immediately after the
  // agent starts.
  await client.flush(5_000);
  const res = await fetch(`${client.baseUrl}/executions/${ctx.executionId}`, {
    method: "PATCH",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${client.apiKey}`,
    },
    body: JSON.stringify({ status: "awaiting_human" }),
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`pauseForHuman: PATCH ${res.status} ${body}`);
  }
}

/**
 * Synchronously transition the current execution from
 * `awaiting_human` back to `started` (Mesedi #18).
 *
 * Call this from the host application's HITL response handler the
 * moment the human's decision lands and the agent is about to be
 * unblocked. The backend computes the wait duration and adds it
 * to total_paused_ms so the time-budget detector reads only the
 * agent's actual working time.
 *
 * Outside `wrap()`: no-op.
 */
export async function resumeForAgent(): Promise<void> {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const client = getClient();
  const res = await fetch(`${client.baseUrl}/executions/${ctx.executionId}`, {
    method: "PATCH",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${client.apiKey}`,
    },
    body: JSON.stringify({ status: "started" }),
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`resumeForAgent: PATCH ${res.status} ${body}`);
  }
}

/**
 * Serializable handle to an in-flight HITL request (Mesedi #19).
 *
 * Returned by {@link requestHumanIntervention}. The shape is plain
 * JSON so the host application can stash it in Redis, a database
 * row, a websocket session, or a queue payload while waiting for
 * the human response, then reconstruct a working handle from the
 * stored data via {@link rehydrateHumanInterventionHandle}.
 */
export interface HumanInterventionHandleData {
  executionId: string;
  requestId: string;
  question: string;
  slaSeconds: number;
  requestedAt: string;
  metadata?: Record<string, unknown>;
}

/**
 * Options for completing a HITL request via
 * {@link completeHumanIntervention}.
 *
 * Well-known {@link responseKind} values: `"approved"`, `"rejected"`,
 * `"edited"`, `"timeout"`, `"cancelled"`. Custom strings are
 * accepted but downstream HITL detectors (#20, #21) only recognize
 * the five well-known values.
 */
export interface CompleteHumanInterventionOptions {
  responseKind: string;
  responsePayload?: Record<string, unknown>;
  decidedBy?: string;
}

/**
 * Pause the current execution and return a handle (Mesedi #19).
 *
 * Synchronously transitions the execution into `awaiting_human`
 * and returns a {@link HumanInterventionHandleData} carrying the
 * correlation data needed to complete the cycle later. The host
 * application is responsible for actually waiting on the human
 * response (queue, websocket, DB poll) and then calling
 * {@link completeHumanIntervention} with the answer.
 *
 * Outside `wrap()`: no-op, returns `null`.
 *
 * `slaSeconds` is optional metadata describing the customer's own
 * SLA expectation. The hitl_timeout detector (#20) reads this to
 * fire when the actual wait exceeds the configured SLA. Customers
 * without an explicit SLA can omit it and #20 will fall back to a
 * project-level default.
 */
export async function requestHumanIntervention(
  question: string,
  options: { slaSeconds?: number; metadata?: Record<string, unknown> } = {},
): Promise<HumanInterventionHandleData | null> {
  const ctx = currentExecutionContext();
  if (!ctx) return null;
  const client = getClient();
  const requestId = `hitl-${cryptoRandomId()}`;
  const requestedAt = utcNowRfc3339();
  // Drain pending shipper traffic so the POST /executions for
  // this wrap() has landed. Without this, calling
  // requestHumanIntervention immediately after the agent starts
  // races the async POST and returns 404.
  await client.flush(5_000);
  const res = await fetch(`${client.baseUrl}/executions/${ctx.executionId}`, {
    method: "PATCH",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${client.apiKey}`,
    },
    body: JSON.stringify({ status: "awaiting_human" }),
  });
  if (!res.ok) {
    const body = await res.text().catch(() => "");
    throw new Error(`requestHumanIntervention: PATCH ${res.status} ${body}`);
  }
  return {
    executionId: ctx.executionId,
    requestId,
    question,
    slaSeconds: options.slaSeconds ?? 0,
    requestedAt,
    metadata: options.metadata,
  };
}

/**
 * Complete a HITL request (Mesedi #19).
 *
 * Emits the `human_intervention` event with the full ask/answer
 * payload, then synchronously transitions the execution from
 * `awaiting_human` back to `started` so the agent code can
 * continue. Idempotent only insofar as a re-call from `started`
 * is a no-op resume (the backend rejects illegal transitions).
 */
export async function completeHumanIntervention(
  handle: HumanInterventionHandleData,
  opts: CompleteHumanInterventionOptions,
): Promise<void> {
  const client = getClient();
  const decidedAtDate = new Date();
  const decidedAt = decidedAtDate.toISOString();
  let waitDurationMs = 0;
  try {
    const requestedAtDate = new Date(handle.requestedAt);
    waitDurationMs = Math.max(0, decidedAtDate.getTime() - requestedAtDate.getTime());
  } catch {
    waitDurationMs = 0;
  }

  const payload: Record<string, unknown> = {
    request_id: handle.requestId,
    question: handle.question,
    requested_at: handle.requestedAt,
    response_kind: opts.responseKind,
    decided_at: decidedAt,
    wait_duration_ms: waitDurationMs,
  };
  if (handle.slaSeconds > 0) payload["sla_seconds"] = Math.trunc(handle.slaSeconds);
  if (opts.responsePayload) payload["response_payload"] = opts.responsePayload;
  if (opts.decidedBy) payload["decided_by"] = opts.decidedBy;
  if (handle.metadata) payload["metadata"] = handle.metadata;

  // Send the human_intervention event synchronously so it lands
  // before (or at worst at the same time as) the resume PATCH.
  // The async shipper would risk losing it if the process exits
  // immediately after resume.
  const event: Event = {
    event_id: newEventId(),
    execution_id: handle.executionId,
    event_type: EventType.HUMAN_INTERVENTION,
    sequence: 0,
    timestamp: decidedAt,
    duration_ms: waitDurationMs,
    payload,
  };
  const eventsRes = await fetch(`${client.baseUrl}/events`, {
    method: "POST",
    headers: {
      "Content-Type": "application/json",
      Authorization: `Bearer ${client.apiKey}`,
    },
    body: JSON.stringify([event]),
  });
  if (!eventsRes.ok) {
    const body = await eventsRes.text().catch(() => "");
    throw new Error(`completeHumanIntervention event POST ${eventsRes.status} ${body}`);
  }

  // Resume the lifecycle.
  const patchRes = await fetch(
    `${client.baseUrl}/executions/${handle.executionId}`,
    {
      method: "PATCH",
      headers: {
        "Content-Type": "application/json",
        Authorization: `Bearer ${client.apiKey}`,
      },
      body: JSON.stringify({ status: "started" }),
    },
  );
  if (!patchRes.ok) {
    const body = await patchRes.text().catch(() => "");
    throw new Error(`completeHumanIntervention PATCH ${patchRes.status} ${body}`);
  }
}

function cryptoRandomId(): string {
  // Small helper for generating a 12-char hex correlation id. Uses
  // Node's webcrypto when available; falls back to Math.random for
  // older runtimes (still good enough for correlation, NOT for
  // anything security-sensitive). Typed loose because the SDK
  // targets both browser and Node runtimes and the tsconfig
  // doesn't pull in the DOM lib.
  const c = (globalThis as {
    crypto?: { getRandomValues?: (a: Uint8Array) => Uint8Array };
  }).crypto;
  if (c && typeof c.getRandomValues === "function") {
    const arr = new Uint8Array(6);
    c.getRandomValues(arr);
    return Array.from(arr).map((b) => b.toString(16).padStart(2, "0")).join("");
  }
  return Math.random().toString(16).slice(2, 14).padEnd(12, "0");
}
