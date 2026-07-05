/**
 * OpenAI Agents SDK integration for Mesedi.
 *
 * Usage:
 *
 *     import { wrap } from "mesedi";
 *     import { MesediRunHooks } from "mesedi/integrations/openai_agents";
 *     import { Agent, Runner } from "@openai/agents";
 *
 *     const triage = new Agent({ name: "triage", ...});
 *     const planner = new Agent({ name: "planner", ...});
 *
 *     export const runUserRequest = wrap(async (question: string) => {
 *       const result = await Runner.run(
 *         triage,
 *         question,
 *         { hooks: new MesediRunHooks() },
 *       );
 *       return result.finalOutput;
 *     });
 *
 * What the hooks emit:
 *
 *   - `checkpoint` events at every agent start AND end with the
 *     agent name and a truncated input/output repr. Feeds the
 *     semantic_loop detector.
 *   - `agent_handoff` events on every handoff between agents.
 *     from_agent / to_agent come straight from the hook arguments,
 *     handoff_kind defaults to `"transfer"`. Feeds cascading_failure
 * and coordination_deadlock.
 *   - `tool_call` events for every tool invocation, mirroring
 *     `mesedi.tool()` wire format.
 *
 * Out of scope for v1:
 *
 *   - `llm_call` events. The OAI Agents SDK does not expose
 *     per-LLM-call hooks in its public RunHooks surface as of the
 *     version this integration was written against. For
 *     Anthropic-backed runs use the existing
 *     `instrumentAnthropic()` patch; an OpenAI-backed equivalent
 *     is not currently provided.
 *   - Streaming hooks (`Runner.runStreamed`). Not currently
 *     instrumented.
 */

import { getClient } from "../client.js";
import { currentExecutionContext, newEventId } from "../context.js";
import { Event, EventType, utcNowRfc3339 } from "../events.js";

// Truncation budget for checkpoint state repr and tool args /
// return values. Wide enough to be useful for semantic_loop
// hashing; narrow enough to keep individual event payloads
// bounded.
const MAX_STATE_REPR = 1000;
const MAX_TOOL_INPUT_REPR = 200;
const MAX_TOOL_OUTPUT_REPR = 500;

/**
 * Mesedi-emitting implementation of the `@openai/agents` RunHooks
 * interface. Pass an instance to `Runner.run(..., { hooks: ... })`.
 *
 * All emissions go through Mesedi's standard event submitter, which
 * is non-blocking (events are batched on a background shipper). The
 * OAI Agents SDK's async runner is therefore never blocked on
 * Mesedi I/O.
 *
 * Outside a `wrap()` execution context, all emissions silently
 * no-op (matching the rest of Mesedi's observe layer).
 */
export class MesediRunHooks {
  // The OAI Agents SDK reads this to identify the hooks instance
  // in trace output. Required field on some versions of the
  // dispatcher; ignored by older ones.
  readonly name = "mesedi-openai-agents";

  // tool key → start time (ms epoch). Used to pair onToolStart
  // with onToolEnd so we can compute duration. We key on the
  // tool name because the SDK does not guarantee stable
  // object identity across the start/end pair in all versions.
  private toolStarts: Map<string, number> = new Map();

  // ── Agent lifecycle ─────────────────────────────────────────────

  async onAgentStart(_context: unknown, agent: unknown): Promise<void> {
    const agentName = extractAgentName(agent);
    emitCheckpoint(agentName, extractContextState(_context), "enter");
  }

  async onAgentEnd(
    _context: unknown,
    agent: unknown,
    output: unknown,
  ): Promise<void> {
    const agentName = extractAgentName(agent);
    emitCheckpoint(agentName, output, "exit");
  }

  // ── Handoffs ────────────────────────────────────────────────────

  async onHandoff(
    context: unknown,
    fromAgent: unknown,
    toAgent: unknown,
  ): Promise<void> {
    emitHandoff(
      extractAgentName(fromAgent),
      extractAgentName(toAgent),
      "transfer",
      summarize(extractContextState(context)),
    );
  }

  // ── Tools ───────────────────────────────────────────────────────

  async onToolStart(
    _context: unknown,
    _agent: unknown,
    tool: unknown,
  ): Promise<void> {
    const key = extractToolName(tool);
    this.toolStarts.set(key, Date.now());
  }

  async onToolEnd(
    _context: unknown,
    _agent: unknown,
    tool: unknown,
    result: unknown,
  ): Promise<void> {
    const key = extractToolName(tool);
    const started = this.toolStarts.get(key);
    this.toolStarts.delete(key);
    const durationMs = started != null ? Math.max(0, Date.now() - started) : 0;
    emitToolCall(
      key,
      summarizeToolInput(tool),
      summarizeToolOutput(result),
      durationMs,
      "ok",
    );
  }
}

// ── Helpers ─────────────────────────────────────────────────────────

function extractAgentName(agent: unknown): string {
  if (agent == null) return "unknown";
  if (typeof agent === "object") {
    const a = agent as Record<string, unknown>;
    if (typeof a.name === "string" && a.name) return a.name;
    const ctor = (a.constructor as { name?: unknown } | undefined)?.name;
    if (typeof ctor === "string" && ctor) return ctor;
  }
  return String(agent);
}

function extractToolName(tool: unknown): string {
  if (tool == null) return "unknown";
  if (typeof tool === "object") {
    const t = tool as Record<string, unknown>;
    if (typeof t.name === "string" && t.name) return t.name;
    if (typeof t.toolName === "string" && t.toolName) return t.toolName;
    const ctor = (t.constructor as { name?: unknown } | undefined)?.name;
    if (typeof ctor === "string" && ctor) return ctor;
  }
  return String(tool);
}

function extractContextState(context: unknown): unknown {
  // OAI Agents wraps the user-supplied context in a wrapper; the
  // real context is typically at `.context`. Fall back to the
  // wrapper itself when the attribute is missing.
  if (context && typeof context === "object") {
    const c = context as Record<string, unknown>;
    if ("context" in c && c.context != null) return c.context;
  }
  return context;
}

function summarize(obj: unknown): string {
  if (obj == null) return "";
  let r: string;
  try {
    r = typeof obj === "string" ? obj : JSON.stringify(obj);
  } catch {
    r = String(obj);
  }
  if (r.length > MAX_STATE_REPR) {
    r = r.slice(0, MAX_STATE_REPR - 3) + "...";
  }
  return r;
}

function summarizeToolInput(tool: unknown): string {
  if (tool && typeof tool === "object") {
    const t = tool as Record<string, unknown>;
    for (const attr of ["arguments", "input", "params"] as const) {
      if (t[attr] != null) return summarize(t[attr]);
    }
  }
  return summarize(extractToolName(tool));
}

function summarizeToolOutput(result: unknown): string {
  if (result == null) return "";
  if (typeof result === "object") {
    const r = result as Record<string, unknown>;
    if ("output" in r && r.output != null) return summarize(r.output);
  }
  return summarize(result);
}

function emitCheckpoint(
  agentName: string,
  state: unknown,
  agentEvent: "enter" | "exit",
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = {
    name: `openai_agents.${agentEvent}.${agentName}`,
    agent: agentName,
    agent_event: agentEvent,
    state_repr: summarize(state),
  };
  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.CHECKPOINT,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  client.submitEvent(event);
}

function emitHandoff(
  fromAgent: string,
  toAgent: string,
  handoffKind: string,
  taskSummary: string,
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const payload: Record<string, unknown> = {
    from_agent: fromAgent,
    to_agent: toAgent,
    handoff_kind: handoffKind,
    task_summary: taskSummary,
  };
  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.AGENT_HANDOFF,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  client.submitEvent(event);
}

function emitToolCall(
  toolName: string,
  argumentsRepr: string,
  returnValueRepr: string,
  durationMs: number,
  status: "ok" | "failed",
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const payload: Record<string, unknown> = {
    tool_name: toolName,
    arguments: argumentsRepr.slice(0, MAX_TOOL_INPUT_REPR),
    return_value: returnValueRepr.slice(0, MAX_TOOL_OUTPUT_REPR),
    latency_ms: Math.trunc(durationMs),
    status,
  };
  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.TOOL_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: Math.trunc(durationMs),
    payload,
  };
  client.submitEvent(event);
}
