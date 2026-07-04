/**
 * Mastra integration for Mesedi (Wave 4.B).
 *
 * Ships as a Mastra Observability exporter. Consumes Mastra's built-in
 * tracing events and translates them into Mesedi's execution / event
 * schema, then hands off to the standard SDK shipper.
 *
 * Usage:
 *
 *     import { configure } from "mesedi";
 *     import { MesediExporter } from "mesedi/integrations/mastra";
 *     import { Mastra } from "@mastra/core";
 *     import { Observability } from "@mastra/observability";
 *
 *     // Once, on Mesedi setup.
 *     configure({ apiKey: process.env.MESEDI_API_KEY });
 *
 *     // Then wire the exporter into Mastra. Mastra 1.x requires the
 *     // `configs` shape below — the `exporters: {...}` root shape from
 *     // pre-1.x is silently ignored and no spans reach the exporter.
 *     const mastra = new Mastra({
 *       observability: new Observability({
 *         configs: {
 *           mesedi: {
 *             serviceName: "my-app",
 *             exporters: [new MesediExporter()],
 *             sampling: { type: "always" },
 *           },
 *         },
 *       }),
 *       // ...your agents / workflows
 *     });
 *
 * Architectural notes.
 *
 * Mastra is OpenTelemetry-first: every agent run, workflow run, LLM
 * call, and tool call emits an OTel-shaped span, and the sanctioned
 * observability integration is to implement an Observability exporter
 * that consumes those spans. Unlike the other Mesedi integrations
 * (LangGraph, OpenAI Agents, Vercel AI SDK), the customer does NOT
 * need to wrap their entry function with `mesedi.wrap()` — Mastra's
 * AGENT_RUN / WORKFLOW_RUN root span is the execution boundary. The
 * exporter opens a Mesedi execution on root-span-start and closes it
 * on root-span-end. All descendant spans on the same trace flow into
 * that execution via an internal trace-id → execution-id map.
 *
 * What the exporter emits:
 *
 *   - AGENT_RUN / WORKFLOW_RUN → open Mesedi execution + emit
 *     checkpoint events at entry and exit.
 *   - MODEL_GENERATION → llm_call event (model, provider, tokens).
 *   - TOOL_CALL / MCP_TOOL_CALL / CLIENT_TOOL_CALL → tool_call event.
 *   - WORKFLOW_STEP → checkpoint event carrying the step name.
 *   - Other span types (RAG_*, MEMORY_OPERATION, WORKSPACE_ACTION,
 *     SCORER_*, MAPPING, PROCESSOR_RUN, MODEL_STEP / MODEL_INFERENCE /
 *     MODEL_CHUNK) are silently ignored in v1. MODEL_GENERATION is
 *     the aggregating span for a full model call — the finer-grained
 *     step / inference / chunk spans are noise for Mesedi's detectors
 *     and are already rolled up by Mastra.
 *
 * Fail-open posture matches the rest of the SDK: any exception inside
 * the exporter is swallowed and logged via console.warn. Never blocks
 * or breaks the customer's Mastra pipeline.
 */

import { getClient } from "../client.js";
import { newEventId, newExecutionId } from "../context.js";
import {
  classifyByProviderName,
  ErrorClass,
} from "../errors.js";
import {
  Event,
  EventType,
  Execution,
  Status,
  utcNowRfc3339,
} from "../events.js";

// ── Peer-dep imports ─────────────────────────────────────────────────
//
// @mastra/observability's BaseExporter is a runtime dep of this
// module. @mastra/core provides the TracingEvent shape and SpanType
// discriminator strings. Both are declared as OPTIONAL peer
// dependencies in package.json — customers who never import this
// module never pay the install cost.

import { BaseExporter } from "@mastra/observability";
import type {
  AnyExportedSpan,
  TracingEvent,
} from "@mastra/core/observability";

// ── Span-type literals ───────────────────────────────────────────────
//
// Mastra's SpanType is a runtime enum. We use its VALUES (which are
// stable string literals like "agent_run", "tool_call") in switch
// cases below rather than importing the enum object itself. This
// keeps the enum a compile-time-only concern and avoids pulling the
// enum's runtime bindings into our output. Matches Mastra's stable
// wire strings; if Mastra ever renames one, tsc catches it because
// AnyExportedSpan['type'] is a union of the same string literals.

const SPAN_AGENT_RUN = "agent_run";
const SPAN_WORKFLOW_RUN = "workflow_run";
const SPAN_MODEL_GENERATION = "model_generation";
const SPAN_TOOL_CALL = "tool_call";
const SPAN_MCP_TOOL_CALL = "mcp_tool_call";
const SPAN_CLIENT_TOOL_CALL = "client_tool_call";
const SPAN_WORKFLOW_STEP = "workflow_step";

// Truncation budgets, matching the OpenAI Agents integration.
const MAX_STATE_REPR = 1000;
const MAX_TOOL_INPUT_REPR = 200;
const MAX_TOOL_OUTPUT_REPR = 500;
const MAX_EXC_MSG = 500;

/**
 * Configuration accepted by `new MesediExporter(...)`. Kept tiny on
 * purpose — the SDK's `configure()` already owns apiKey / baseUrl /
 * shipper knobs globally; passing them per-exporter would produce two
 * shippers if the customer ever instantiates two Mastra apps in the
 * same process.
 */
export interface MesediExporterConfig {
  /**
   * Optional Mastra tenant identifier attached to every Mesedi
   * execution the exporter opens. Surfaces on the dashboard's
   * cost-by-tenant report and enables provider_incident's cross-
   * tenant clustering. Absent means "unattributed."
   */
  tenantId?: string;
}

/**
 * Mesedi observability exporter for Mastra. Attach an instance via
 * `Observability({ exporters: { mesedi: new MesediExporter() } })` on
 * your `new Mastra(...)` call.
 */
export class MesediExporter extends BaseExporter {
  name = "mesedi";

  private readonly tenantId?: string;

  /**
   * Maps Mastra traceId → Mesedi executionId. Populated when a root
   * span (AGENT_RUN or WORKFLOW_RUN) starts; removed when it ends.
   * Descendant spans on the same trace look up their execution here.
   */
  private readonly traceToExecution: Map<string, string> = new Map();

  /**
   * Sequence counter per execution, ticked once per emitted event.
   * Mesedi's backend relies on monotonic sequences to reconstruct
   * event order at ingest.
   */
  private readonly sequences: Map<string, number> = new Map();

  /**
   * Tracks execution start time in ms epoch so we can compute
   * duration_ms on execution end. Removed when execution closes.
   */
  private readonly executionStarts: Map<string, number> = new Map();

  constructor(config: MesediExporterConfig = {}) {
    super({});
    this.tenantId = config.tenantId;
  }

  /**
   * Called by @mastra/observability's ObservabilityBus for every
   * span lifecycle event. Base class routes through
   * `applySpanFormatter` first if the customer supplied one.
   */
  protected async _exportTracingEvent(event: TracingEvent): Promise<void> {
    try {
      const span = event.exportedSpan;
      switch (event.type) {
        case "span_started":
          this.handleSpanStarted(span);
          break;
        case "span_ended":
          this.handleSpanEnded(span);
          break;
        case "span_updated":
          // Updates are ignored in v1. The relevant fields (tokens,
          // status, output) are re-emitted on SPAN_ENDED, which is
          // when we snapshot state into Mesedi's event schema.
          break;
      }
    } catch (err) {
      // Fail-open: never break the customer's Mastra pipeline.
      console.warn("mesedi mastra exporter: swallowed error", err);
    }
  }

  // ── Span-start ─────────────────────────────────────────────────────

  private handleSpanStarted(span: AnyExportedSpan): void {
    if (!isRootSpan(span)) return;
    // Root span. Open a Mesedi execution.
    const traceId = extractTraceId(span);
    if (!traceId) return;
    if (this.traceToExecution.has(traceId)) return;

    const executionId = newExecutionId();
    this.traceToExecution.set(traceId, executionId);
    this.sequences.set(executionId, 0);
    this.executionStarts.set(executionId, Date.now());

    const execution: Execution = {
      execution_id: executionId,
      status: Status.STARTED,
      started_at: utcNowRfc3339(),
      sdk_language: "typescript",
      sdk_version: "0.7.0",
    };
    if (this.tenantId !== undefined) execution.tenant_id = this.tenantId;
    const client = getClient();
    client.submitExecutionStart(execution);

    // Also emit an enter checkpoint so semantic_loop / drift see the
    // execution boundary.
    this.emitEvent(executionId, EventType.CHECKPOINT, {
      name: `mastra.${span.type}.enter`,
      agent: extractAgentName(span),
      agent_event: "enter",
      state_repr: summarize(inputRepr(span)),
    });
  }

  // ── Span-end ───────────────────────────────────────────────────────

  private handleSpanEnded(span: AnyExportedSpan): void {
    const traceId = extractTraceId(span);
    if (!traceId) return;
    const executionId = this.traceToExecution.get(traceId);
    if (!executionId) return;

    switch (span.type) {
      case SPAN_AGENT_RUN:
      case SPAN_WORKFLOW_RUN:
        this.emitEvent(executionId, EventType.CHECKPOINT, {
          name: `mastra.${span.type}.exit`,
          agent: extractAgentName(span),
          agent_event: "exit",
          state_repr: summarize(outputRepr(span)),
        });
        this.closeExecution(traceId, executionId, span);
        break;

      case SPAN_MODEL_GENERATION:
        this.emitEvent(executionId, EventType.LLM_CALL, modelPayload(span));
        break;

      case SPAN_TOOL_CALL:
      case SPAN_MCP_TOOL_CALL:
      case SPAN_CLIENT_TOOL_CALL:
        this.emitEvent(
          executionId,
          span.type === SPAN_MCP_TOOL_CALL
            ? EventType.MCP_CALL
            : EventType.TOOL_CALL,
          toolPayload(span),
        );
        break;

      case SPAN_WORKFLOW_STEP: {
        // SW#280-3.g.b: backend semantic_loop.G3 detector groups
        // checkpoints by canonical hash of `payload.metadata`
        // (semantic_loop.go extractState reads `metadata`, not
        // `state_repr`). Direct SDK's checkpoint() ships {name,
        // metadata} too — matching that shape unblocks Mastra
        // customers using workflows with 3+ identical-return steps.
        // state_repr stays for dashboard display but the detector-
        // relevant field is metadata (raw structured object).
        const rawOutput = outputRepr(span);
        const payload: Record<string, unknown> = {
          name: `mastra.workflow_step.${extractName(span) ?? "unknown"}`,
          state_repr: summarize(rawOutput),
        };
        if (rawOutput && typeof rawOutput === "object") {
          payload["metadata"] = rawOutput;
        }
        this.emitEvent(executionId, EventType.CHECKPOINT, payload);
        break;
      }

      // All other span types are ignored in v1. See the file header
      // comment for rationale.
      default:
        break;
    }
  }

  private closeExecution(
    traceId: string,
    executionId: string,
    span: AnyExportedSpan,
  ): void {
    const startedAtMs = this.executionStarts.get(executionId);
    const durationMs =
      startedAtMs != null ? Math.max(0, Date.now() - startedAtMs) : undefined;
    const execution: Execution = {
      execution_id: executionId,
      status: mapStatus(span),
      started_at: utcNowRfc3339(),
      sdk_language: "typescript",
      sdk_version: "0.7.0",
      ended_at: utcNowRfc3339(),
    };
    if (durationMs !== undefined) execution.duration_ms = durationMs;
    getClient().submitExecutionEnd(execution);

    this.traceToExecution.delete(traceId);
    this.sequences.delete(executionId);
    this.executionStarts.delete(executionId);
  }

  private emitEvent(
    executionId: string,
    eventType: EventType,
    payload: Record<string, unknown>,
  ): void {
    const nextSeq = (this.sequences.get(executionId) ?? 0) + 1;
    this.sequences.set(executionId, nextSeq);
    const event: Event = {
      event_id: newEventId(),
      execution_id: executionId,
      event_type: eventType,
      sequence: nextSeq,
      timestamp: utcNowRfc3339(),
      payload,
    };
    getClient().submitEvent(event);
  }

  // ── Lifecycle ──────────────────────────────────────────────────────
  //
  // BaseExporter provides no-op defaults for flush() and shutdown().
  // We don't override them: the Mesedi client owns its own shipper
  // lifecycle (setInterval + beforeExit hook in client.ts), so
  // exporter-level flush/shutdown would just duplicate that work.
}

// ── Helpers ──────────────────────────────────────────────────────────

function isRootSpan(span: AnyExportedSpan): boolean {
  // A root span has no parent. Mastra encodes this as either
  // parentSpanId being undefined or explicitly null. We check for
  // both. AGENT_RUN and WORKFLOW_RUN with no parent are treated as
  // the execution boundary.
  if (span.type !== SPAN_AGENT_RUN && span.type !== SPAN_WORKFLOW_RUN) {
    return false;
  }
  const s = span as unknown as { parentSpanId?: string | null };
  return s.parentSpanId == null;
}

function extractTraceId(span: AnyExportedSpan): string | undefined {
  const s = span as unknown as { traceId?: string };
  return typeof s.traceId === "string" && s.traceId.length > 0
    ? s.traceId
    : undefined;
}

function extractAgentName(span: AnyExportedSpan): string {
  const s = span as unknown as {
    attributes?: { name?: string; agentName?: string };
    name?: string;
  };
  if (typeof s.attributes?.agentName === "string" && s.attributes.agentName) {
    return s.attributes.agentName;
  }
  if (typeof s.attributes?.name === "string" && s.attributes.name) {
    return s.attributes.name;
  }
  if (typeof s.name === "string" && s.name) return s.name;
  return "unknown";
}

function extractName(span: AnyExportedSpan): string | undefined {
  const s = span as unknown as {
    name?: string;
    attributes?: { name?: string; stepId?: string };
  };
  if (typeof s.attributes?.stepId === "string" && s.attributes.stepId) {
    return s.attributes.stepId;
  }
  if (typeof s.attributes?.name === "string" && s.attributes.name) {
    return s.attributes.name;
  }
  if (typeof s.name === "string" && s.name) return s.name;
  return undefined;
}

function inputRepr(span: AnyExportedSpan): unknown {
  const s = span as unknown as {
    input?: unknown;
    attributes?: { prompt?: unknown; input?: unknown };
  };
  return s.attributes?.prompt ?? s.attributes?.input ?? s.input ?? "";
}

function outputRepr(span: AnyExportedSpan): unknown {
  const s = span as unknown as {
    output?: unknown;
    attributes?: { output?: unknown; result?: unknown };
  };
  return s.attributes?.output ?? s.attributes?.result ?? s.output ?? "";
}

/**
 * SW#280-3.g: extract the last user-role message's text from a
 * Mastra MODEL_GENERATION span's `input`, unwrapping the ai-sdk
 * message-array + content-parts + providerOptions timestamp
 * envelope. Falls back to the raw input for non-Mastra shapes so
 * existing tests keep working.
 *
 * Mastra shape observed live (SW#280-3.g probe):
 *   input = {
 *     messages: [
 *       {role: "system", content: "..."},
 *       {role: "user", content: [
 *         {type: "text", text: "actual prompt",
 *          providerOptions: {mastra: {createdAt: 1783...}}}
 *       ]}
 *     ]
 *   }
 *
 * The `providerOptions.mastra.createdAt` differs per call and, when
 * we ship the whole envelope as user_message, defeats
 * identical_call / drift lexical / token_waste signature equality.
 * Extracting just the human-visible text is what those detectors
 * need.
 */
function extractUserMessage(span: AnyExportedSpan): unknown {
  const s = span as unknown as {
    input?: unknown;
    attributes?: { prompt?: unknown; input?: unknown };
  };
  const raw =
    s.attributes?.prompt ?? s.attributes?.input ?? s.input ?? "";
  if (typeof raw === "string") return raw;
  const messages = (raw as { messages?: unknown[] })?.messages;
  if (!Array.isArray(messages)) return raw;
  // Walk backwards; use the last user-role message.
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i] as { role?: string; content?: unknown };
    if (m?.role !== "user") continue;
    if (typeof m.content === "string") return m.content;
    if (Array.isArray(m.content)) {
      const parts: string[] = [];
      for (const p of m.content) {
        if (typeof p === "string") {
          parts.push(p);
        } else if (p && typeof p === "object") {
          const pt = p as { type?: string; text?: string };
          if (pt.type === "text" && typeof pt.text === "string") {
            parts.push(pt.text);
          }
        }
      }
      return parts.join(" ");
    }
    return m.content ?? "";
  }
  return raw;
}

/**
 * SW#280-3.g: extract the response text from a Mastra
 * MODEL_GENERATION span's `output`, unwrapping the
 * `{files, reasoning, sources, text, warnings}` envelope. Falls
 * back to the raw output for non-Mastra shapes so existing tests
 * keep working.
 */
function extractResponseText(span: AnyExportedSpan): unknown {
  const s = span as unknown as {
    output?: unknown;
    attributes?: { output?: unknown; result?: unknown };
  };
  const raw =
    s.attributes?.output ?? s.attributes?.result ?? s.output ?? "";
  if (typeof raw === "string") return raw;
  const text = (raw as { text?: unknown })?.text;
  if (typeof text === "string") return text;
  return raw;
}

function modelPayload(span: AnyExportedSpan): Record<string, unknown> {
  const s = span as unknown as {
    attributes?: {
      model?: string;
      provider?: string;
      // SW#280-3.g: Mastra 1.x MODEL_GENERATION spans carry per-call
      // token counts in `attributes.usage`. `attributes.internalUsage`
      // is only for INTERNAL descendant spans rolled up to their
      // exported ancestor (e.g. AGENT_RUN with hidden MODEL_GENERATION
      // children) — it stays empty on the customer-facing
      // model_generation span. Read `usage` first, fall back to
      // `internalUsage` for the ancestor-rollup case.
      usage?: {
        inputTokens?: number;
        outputTokens?: number;
      };
      internalUsage?: {
        inputTokens?: number;
        outputTokens?: number;
      };
    };
    errorInfo?: {
      message?: string;
      name?: string;
      details?: Record<string, unknown>;
    };
  };
  const model = s.attributes?.model ?? "unknown";
  const provider = s.attributes?.provider ?? "unknown";
  const errorInfo = s.errorInfo;
  const hasError = !!errorInfo && (!!errorInfo.name || !!errorInfo.message);

  // Success path stays byte-identical with the pre-SW#280-3.f shape
  // to avoid churning consumers that already handle Mastra llm_calls.
  // SW#280-3.g: Mastra 1.x model_generation spans populate
  // `span.input` with the full ai-sdk messages array (system + user
  // messages, with `providerOptions.mastra.createdAt` timestamps
  // baked in) and `span.output` with `{files, reasoning, sources,
  // text, warnings}` where `text` is the response. Extracting the
  // last user message's text (not JSON-stringifying the whole
  // envelope) is what identical_call / drift lexical / token_waste
  // need — the raw envelope's per-call timestamp noise defeats
  // signature equality.
  const payload: Record<string, unknown> = {
    model,
    provider,
    user_message: summarize(extractUserMessage(span), MAX_STATE_REPR),
    response_text: summarize(extractResponseText(span), MAX_STATE_REPR),
    status: hasError ? "failed" : "ok",
  };
  if (!hasError) {
    // SW#280-3.g: prefer per-call `usage` over ancestor-rollup
    // `internalUsage` — see attribute-shape comment above.
    const tokensIn =
      s.attributes?.usage?.inputTokens ??
      s.attributes?.internalUsage?.inputTokens;
    const tokensOut =
      s.attributes?.usage?.outputTokens ??
      s.attributes?.internalUsage?.outputTokens;
    if (tokensIn !== undefined) payload["input_tokens"] = tokensIn;
    if (tokensOut !== undefined) payload["output_tokens"] = tokensOut;
    return payload;
  }

  // SW#280-3.f: failed-path fields matching the LangChain adapter's
  // SW#280-3.a contract so the provider_incident detector's canonical
  // (provider, error_class) clustering works cross-adapter. Mastra
  // gives us SpanErrorInfo.name (exception class name as a bare
  // string, not an Error object) — classifyByProviderName wraps it in
  // a shim so the per-provider classifiers score it correctly.
  const excType = errorInfo?.name;
  const excMessage = errorInfo?.message;
  payload["error_class"] = classifyByProviderName(provider, excType) ?? ErrorClass.UNKNOWN;
  if (excType) payload["exception_type"] = excType;
  if (excMessage) {
    payload["exception_message"] = summarize(excMessage, MAX_EXC_MSG);
  }
  const httpStatus = extractStatusCode(errorInfo?.details);
  if (httpStatus !== undefined) payload["http_status"] = httpStatus;
  const retryAfter = extractRetryAfterSeconds(errorInfo?.details);
  if (retryAfter !== undefined) payload["retry_after"] = retryAfter;
  return payload;
}

/** Read an HTTP status code from SpanErrorInfo.details, tolerating
 * the common variants (status, statusCode, http_status). Returns
 * undefined when absent or not a finite integer. */
function extractStatusCode(
  details: Record<string, unknown> | undefined,
): number | undefined {
  if (!details) return undefined;
  for (const key of ["status", "statusCode", "http_status"]) {
    const v = details[key];
    if (typeof v === "number" && Number.isFinite(v) && Number.isInteger(v)) {
      return v;
    }
  }
  return undefined;
}

/** Read a retry_after (seconds) from SpanErrorInfo.details, tolerating
 * the common variants (retryAfter, retry_after). Returns undefined
 * when absent or not a finite non-negative number. */
function extractRetryAfterSeconds(
  details: Record<string, unknown> | undefined,
): number | undefined {
  if (!details) return undefined;
  for (const key of ["retryAfter", "retry_after"]) {
    const v = details[key];
    if (typeof v === "number" && Number.isFinite(v) && v >= 0) {
      return Math.floor(v);
    }
  }
  return undefined;
}

function toolPayload(span: AnyExportedSpan): Record<string, unknown> {
  const name = extractName(span) ?? "unknown";
  const s = span as unknown as {
    attributes?: { input?: unknown; output?: unknown; error?: unknown };
  };
  const status = s.attributes?.error ? "error" : "ok";
  return {
    tool_name: name,
    arguments: summarize(s.attributes?.input, MAX_TOOL_INPUT_REPR),
    return_value: summarize(s.attributes?.output, MAX_TOOL_OUTPUT_REPR),
    status,
  };
}

function mapStatus(span: AnyExportedSpan): Status {
  const s = span as unknown as {
    attributes?: { tripwireAbort?: unknown; error?: unknown };
  };
  if (s.attributes?.tripwireAbort) return Status.HALTED;
  if (s.attributes?.error) return Status.CRASHED;
  return Status.COMPLETED;
}

function summarize(value: unknown, maxLen: number = MAX_STATE_REPR): string {
  if (value == null) return "";
  let r: string;
  try {
    r = typeof value === "string" ? value : JSON.stringify(value);
  } catch {
    r = String(value);
  }
  if (r.length > maxLen) r = r.slice(0, maxLen - 3) + "...";
  return r;
}
