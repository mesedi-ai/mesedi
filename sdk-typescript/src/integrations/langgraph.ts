/**
 * LangGraph integration for Mesedi.
 *
 * Usage (one-liner):
 *
 *     import { wrap } from "mesedi";
 *     import { instrumentLangGraph } from "mesedi/integrations/langgraph";
 *     import { StateGraph } from "@langchain/langgraph";
 *
 *     let graph = buildMyGraph().compile();
 *     graph = instrumentLangGraph(graph);
 *
 *     export const runAgent = wrap(async (question: string) => {
 *       const result = await graph.invoke({ question });
 *       return result.answer;
 *     });
 *
 * What the integration emits:
 *
 *   - `llm_call` events for every chat-model invocation inside the
 *     graph (via the inherited LangChain dispatcher).
 *   - `tool_call` events for every tool invocation.
 *   - `checkpoint` events at every node entry and exit, with the
 *     node name and a truncated state repr. Feeds the semantic_loop
 *     detector which hashes canonical state.
 *   - `agent_handoff` events when the graph invokes a compiled
 *     sub-graph. Feeds cascading_failure and
 *     coordination_deadlock.
 *
 * Why this exists at the TS layer:
 *
 * The Python integration ships in `mesedi.integrations.langgraph`.
 * The TypeScript LangChain / LangGraph stack uses the same callback
 * handler surface (`BaseCallbackHandler` from
 * `@langchain/core/callbacks/base`), so this port keeps wire-format
 * parity with the Python emitter while reading idiomatically in
 * TypeScript.
 *
 * Out of scope for v1:
 *
 *   - Streaming hooks (`graph.streamEvents`). Covered in a follow-up.
 *   - LangGraph's persistence (checkpointer) — Mesedi emits
 *     telemetry parallel to it without touching the stored state.
 *   - LangGraph `interrupt()` auto-bridging — use the existing HITL
 *     helpers (`pauseForHuman`, `requestHumanIntervention`) for now.
 */

import { getClient } from "../client.js";
import { currentExecutionContext, newEventId } from "../context.js";
import { Event, EventType, utcNowRfc3339 } from "../events.js";

// Truncation budget for checkpoint state repr. Wide enough for
// semantic_loop hashing to be useful; narrow enough to keep
// individual event payloads bounded on graphs with large state.
const MAX_NODE_STATE_REPR = 1000;

/**
 * Mesedi LangGraph callback handler. Duck-typed against
 * `BaseCallbackHandler` from `@langchain/core/callbacks/base`.
 *
 * We do NOT extend the LangChain class directly because that would
 * make `@langchain/core` a required runtime dependency. The handler
 * implements the methods the LangGraph dispatcher calls and is
 * picked up structurally.
 */
export class MesediLangGraphCallbackHandler {
  // The LangChain dispatcher reads this field to decide whether the
  // handler is OK to share across runs. We allow it.
  readonly name = "mesedi-langgraph";
  readonly raiseError = false;

  // run_id → { nodeName, startedAtMs }. Used to pair node
  // start/end so we can compute node duration and emit it on the
  // exit checkpoint.
  private nodeStarts: Map<string, { nodeName: string; startedAtMs: number }> =
    new Map();

  // run_id → { fromAgent, toAgent }. Used to pair sub-graph
  // start/end. We only emit the handoff request at start; the
  // sub-graph's own execution (if it ran as a nested wrap) carries
  // its terminal status independently.
  private handoffStarts: Map<string, { fromAgent: string; toAgent: string }> =
    new Map();

  // ── Node-level chain events ─────────────────────────────────────

  onChainStart(
    serialized: Record<string, unknown> | undefined,
    inputs: Record<string, unknown> | undefined,
    runId: string,
    _parentRunId?: string,
    tags?: string[],
    metadata?: Record<string, unknown>,
  ): void {
    const nodeName = extractLangGraphNodeName(serialized, tags, metadata);
    if (nodeName) {
      this.nodeStarts.set(runId, {
        nodeName,
        startedAtMs: Date.now(),
      });
      emitCheckpoint(nodeName, inputs ?? {}, "enter", 0);
    }

    const subgraphName = extractSubgraphName(serialized, tags, metadata);
    if (subgraphName) {
      const fromAgent = this.mostRecentNode() ?? "graph";
      this.handoffStarts.set(runId, { fromAgent, toAgent: subgraphName });
      emitHandoffRequest(fromAgent, subgraphName, summarizeInputs(inputs ?? {}));
    }
  }

  onChainEnd(
    outputs: Record<string, unknown> | undefined,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
    _kwargs?: Record<string, unknown>,
  ): void {
    const entry = this.nodeStarts.get(runId);
    if (entry) {
      this.nodeStarts.delete(runId);
      const durationMs = Math.max(0, Date.now() - entry.startedAtMs);
      emitCheckpoint(entry.nodeName, outputs ?? {}, "exit", durationMs);
    }
    this.handoffStarts.delete(runId);
  }

  onChainError(
    error: unknown,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
    _kwargs?: Record<string, unknown>,
  ): void {
    const entry = this.nodeStarts.get(runId);
    if (entry) {
      this.nodeStarts.delete(runId);
      const durationMs = Math.max(0, Date.now() - entry.startedAtMs);
      const errorName =
        error && typeof error === "object" && "name" in error
          ? String((error as { name: unknown }).name)
          : typeof error === "string"
            ? error
            : "Error";
      emitCheckpoint(
        entry.nodeName,
        { error_type: errorName },
        "error",
        durationMs,
      );
    }
    this.handoffStarts.delete(runId);
  }

  // ── LLM passthrough ─────────────────────────────────────────────
  //
  // LangChain dispatches LLM and tool callbacks to this handler too.
  // The Python integration inherits the LLM/tool handling from the
  // existing MesediCallbackHandler; the TypeScript SDK does not ship
  // a separate LangChain handler yet, so we provide thin
  // implementations here.

  onChatModelStart(
    serialized: Record<string, unknown> | undefined,
    messages: Array<Array<unknown>>,
    runId: string,
    _parentRunId?: string,
    extraParams?: Record<string, unknown>,
  ): void {
    const model = extractModel(serialized, extraParams);
    const last = messages[messages.length - 1] ?? [];
    const { userMessage, systemPrompt } = extractRoleMessages(last);
    this.llmStarts.set(runId, {
      model,
      userMessage,
      systemPrompt,
      startedAtMs: Date.now(),
    });
  }

  onLLMStart(
    serialized: Record<string, unknown> | undefined,
    prompts: string[],
    runId: string,
    _parentRunId?: string,
    extraParams?: Record<string, unknown>,
  ): void {
    const model = extractModel(serialized, extraParams);
    this.llmStarts.set(runId, {
      model,
      userMessage: prompts[prompts.length - 1] ?? "",
      systemPrompt: "",
      startedAtMs: Date.now(),
    });
  }

  onLLMEnd(output: unknown, runId: string): void {
    const ctx = this.llmStarts.get(runId);
    if (!ctx) return;
    this.llmStarts.delete(runId);
    const durationMs = Math.max(0, Date.now() - ctx.startedAtMs);
    const responseText = extractResponseText(output);
    const { inputTokens, outputTokens } = extractTokenUsage(output);
    emitLLMCallEvent({
      model: ctx.model,
      userMessage: ctx.userMessage,
      systemPrompt: ctx.systemPrompt,
      responseText,
      inputTokens,
      outputTokens,
      durationMs,
      status: "ok",
    });
  }

  onLLMError(_error: unknown, runId: string): void {
    const ctx = this.llmStarts.get(runId);
    if (!ctx) return;
    this.llmStarts.delete(runId);
    const durationMs = Math.max(0, Date.now() - ctx.startedAtMs);
    emitLLMCallEvent({
      model: ctx.model,
      userMessage: ctx.userMessage,
      systemPrompt: ctx.systemPrompt,
      responseText: "",
      inputTokens: 0,
      outputTokens: 0,
      durationMs,
      status: "failed",
    });
  }

  onToolStart(
    serialized: Record<string, unknown> | undefined,
    inputStr: string | Record<string, unknown>,
    runId: string,
  ): void {
    // Pull the tool name off the serialized payload if present.
    // The conditional has to be a plain ternary (not the
    // && && ?? chain a previous revision tried) because `&&`
    // short-circuits to `false` when an intermediate predicate is
    // false, and `??` does not coalesce `false`.
    const toolName =
      serialized && typeof serialized.name === "string" && serialized.name
        ? serialized.name
        : "tool";
    const input =
      typeof inputStr === "string" ? inputStr : JSON.stringify(inputStr);
    this.toolStarts.set(runId, {
      name: toolName,
      inputStr: input,
      startedAtMs: Date.now(),
    });
  }

  onToolEnd(output: unknown, runId: string): void {
    const ctx = this.toolStarts.get(runId);
    if (!ctx) return;
    this.toolStarts.delete(runId);
    const durationMs = Math.max(0, Date.now() - ctx.startedAtMs);
    emitToolCallEvent({
      toolName: ctx.name,
      inputStr: ctx.inputStr,
      resultSummary:
        output == null
          ? ""
          : typeof output === "string"
            ? output
            : JSON.stringify(output),
      durationMs,
      status: "ok",
    });
  }

  onToolError(error: unknown, runId: string): void {
    const ctx = this.toolStarts.get(runId);
    if (!ctx) return;
    this.toolStarts.delete(runId);
    const durationMs = Math.max(0, Date.now() - ctx.startedAtMs);
    const errorName =
      error && typeof error === "object" && "name" in error
        ? String((error as { name: unknown }).name)
        : "Error";
    const errorMessage =
      error && typeof error === "object" && "message" in error
        ? String((error as { message: unknown }).message)
        : "";
    emitToolCallEvent({
      toolName: ctx.name,
      inputStr: ctx.inputStr,
      resultSummary: "",
      durationMs,
      status: "failed",
      exceptionType: errorName,
      exceptionMessage: errorMessage,
    });
  }

  // Internal pairing tables for LLM and tool events.
  private llmStarts: Map<
    string,
    {
      model: string;
      userMessage: string;
      systemPrompt: string;
      startedAtMs: number;
    }
  > = new Map();
  private toolStarts: Map<
    string,
    { name: string; inputStr: string; startedAtMs: number }
  > = new Map();

  // Internal helper to look up the most recent in-flight node so
  // sub-graph handoffs can name a plausible from_agent even when
  // the dispatcher does not surface an explicit parent.
  private mostRecentNode(): string | null {
    if (this.nodeStarts.size === 0) return null;
    // JS Map preserves insertion order.
    let last: string | null = null;
    for (const v of this.nodeStarts.values()) last = v.nodeName;
    return last;
  }
}

/**
 * Attach Mesedi telemetry to a compiled LangGraph.
 *
 * Wraps the supplied compiled graph so every call to `invoke`,
 * `stream`, or their async variants automatically attaches a
 * `MesediLangGraphCallbackHandler` to the LangChain callback config.
 * The wrap is non-destructive: it adds the handler to whatever
 * callbacks the caller passed in via `config={ callbacks: [...] }`,
 * so existing instrumentation keeps working.
 *
 * Returns the same `graph` object with patched methods, so callers
 * can re-assign in place.
 *
 * Outside a `wrap()` execution context, the handler's emissions
 * silently no-op, so it is safe to instrument the graph once at
 * module load and let individual invocations decide whether they
 * want to be observed.
 */
export function instrumentLangGraph<G extends LangGraphLike>(graph: G): G {
  const handler = new MesediLangGraphCallbackHandler();

  const ensureHandler = (
    config: Record<string, unknown> | undefined,
  ): Record<string, unknown> => {
    const cfg = { ...(config ?? {}) };
    const cbs = Array.isArray(cfg.callbacks) ? [...(cfg.callbacks as unknown[])] : [];
    for (const cb of cbs) {
      if (cb instanceof MesediLangGraphCallbackHandler) return cfg;
    }
    cbs.push(handler);
    cfg.callbacks = cbs;
    return cfg;
  };

  const origInvoke = graph.invoke?.bind(graph);
  if (origInvoke) {
    graph.invoke = async function patchedInvoke(
      input: unknown,
      config?: Record<string, unknown>,
      ...rest: unknown[]
    ): Promise<unknown> {
      return origInvoke(input, ensureHandler(config), ...rest);
    } as G["invoke"];
  }

  const origStream = graph.stream?.bind(graph);
  if (origStream) {
    graph.stream = async function patchedStream(
      input: unknown,
      config?: Record<string, unknown>,
      ...rest: unknown[]
    ): Promise<unknown> {
      return origStream(input, ensureHandler(config), ...rest);
    } as G["stream"];
  }

  return graph;
}

// ── Internal helpers ───────────────────────────────────────────────

interface LangGraphLike {
  invoke?: (
    input: unknown,
    config?: Record<string, unknown>,
    ...rest: unknown[]
  ) => Promise<unknown>;
  stream?: (
    input: unknown,
    config?: Record<string, unknown>,
    ...rest: unknown[]
  ) => Promise<unknown> | AsyncIterable<unknown>;
}

function extractLangGraphNodeName(
  _serialized: Record<string, unknown> | undefined,
  tags: string[] | undefined,
  metadata: Record<string, unknown> | undefined,
): string | null {
  if (metadata && typeof metadata === "object") {
    const fromMeta =
      (typeof metadata.langgraph_node === "string" && metadata.langgraph_node) ||
      (typeof metadata.node === "string" && metadata.node);
    if (fromMeta) return fromMeta;
  }
  if (Array.isArray(tags)) {
    for (const t of tags) {
      if (typeof t === "string" && t.startsWith("langgraph:node:")) {
        return t.split(":", 3)[2] ?? null;
      }
    }
  }
  return null;
}

function extractSubgraphName(
  _serialized: Record<string, unknown> | undefined,
  tags: string[] | undefined,
  metadata: Record<string, unknown> | undefined,
): string | null {
  if (metadata && typeof metadata === "object") {
    const fromMeta =
      (typeof metadata.langgraph_subgraph === "string" &&
        metadata.langgraph_subgraph) ||
      (typeof metadata.subgraph === "string" && metadata.subgraph);
    if (fromMeta) return fromMeta;
  }
  if (Array.isArray(tags)) {
    for (const t of tags) {
      if (typeof t === "string" && t.startsWith("langgraph:subgraph:")) {
        return t.split(":", 3)[2] ?? null;
      }
    }
  }
  return null;
}

function summarizeInputs(inputs: Record<string, unknown>): string {
  let r: string;
  try {
    r = JSON.stringify(inputs);
  } catch {
    r = String(inputs);
  }
  if (r.length > MAX_NODE_STATE_REPR) {
    r = r.slice(0, MAX_NODE_STATE_REPR - 3) + "...";
  }
  return r;
}

function emitCheckpoint(
  nodeName: string,
  state: unknown,
  nodeEvent: "enter" | "exit" | "error",
  durationMs: number,
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  let stateRepr: string;
  try {
    stateRepr = JSON.stringify(state);
  } catch {
    stateRepr = String(state);
  }
  if (stateRepr.length > MAX_NODE_STATE_REPR) {
    stateRepr = stateRepr.slice(0, MAX_NODE_STATE_REPR - 3) + "...";
  }

  const payload: Record<string, unknown> = {
    name: `langgraph.${nodeEvent}.${nodeName}`,
    node: nodeName,
    node_event: nodeEvent,
    state_repr: stateRepr,
  };
  if (durationMs > 0) payload["duration_ms"] = Math.trunc(durationMs);

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.CHECKPOINT,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: durationMs > 0 ? Math.trunc(durationMs) : undefined,
    payload,
  };
  client.submitEvent(event);
}

function emitHandoffRequest(
  fromAgent: string,
  toAgent: string,
  taskSummary: string,
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const payload: Record<string, unknown> = {
    from_agent: fromAgent,
    to_agent: toAgent,
    handoff_kind: "delegate",
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

function emitLLMCallEvent(args: {
  model: string;
  userMessage: string;
  systemPrompt: string;
  responseText: string;
  inputTokens: number;
  outputTokens: number;
  durationMs: number;
  status: "ok" | "failed";
}): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const payload: Record<string, unknown> = {
    model: args.model,
    user_prompt: args.userMessage,
    system_prompt: args.systemPrompt,
    response: args.responseText,
    input_tokens: args.inputTokens,
    output_tokens: args.outputTokens,
    latency_ms: Math.trunc(args.durationMs),
    status: args.status,
  };
  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.LLM_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: Math.trunc(args.durationMs),
    payload,
  };
  client.submitEvent(event);
}

function emitToolCallEvent(args: {
  toolName: string;
  inputStr: string;
  resultSummary: string;
  durationMs: number;
  status: "ok" | "failed";
  exceptionType?: string;
  exceptionMessage?: string;
}): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const payload: Record<string, unknown> = {
    tool_name: args.toolName,
    arguments: args.inputStr,
    return_value: args.resultSummary,
    latency_ms: Math.trunc(args.durationMs),
    status: args.status,
  };
  if (args.exceptionType) payload["error_class"] = args.exceptionType;
  if (args.exceptionMessage) payload["error"] = args.exceptionMessage;
  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.TOOL_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: Math.trunc(args.durationMs),
    payload,
  };
  client.submitEvent(event);
}

function extractModel(
  serialized: Record<string, unknown> | undefined,
  kwargs: Record<string, unknown> | undefined,
): string {
  const inv =
    kwargs && typeof kwargs === "object"
      ? ((kwargs.invocation_params as Record<string, unknown>) ?? {})
      : {};
  for (const key of ["model", "model_name", "deployment_name"]) {
    const v = inv[key];
    if (typeof v === "string" && v) return v;
  }
  if (serialized && typeof serialized === "object") {
    const sk = (serialized.kwargs as Record<string, unknown>) ?? {};
    for (const key of ["model", "model_name", "deployment_name"]) {
      const v = sk[key];
      if (typeof v === "string" && v) return v;
    }
    const id = serialized.id;
    if (Array.isArray(id) && id.length > 0) {
      const last = id[id.length - 1];
      if (typeof last === "string") return last;
    }
    if (typeof serialized.name === "string" && serialized.name) {
      return serialized.name;
    }
  }
  return "unknown";
}

function extractRoleMessages(conversation: unknown[]): {
  userMessage: string;
  systemPrompt: string;
} {
  let userMessage = "";
  let systemPrompt = "";
  for (const msg of conversation) {
    if (!msg || typeof msg !== "object") continue;
    const m = msg as Record<string, unknown>;
    const type =
      typeof m.type === "string"
        ? m.type.toLowerCase()
        : typeof m._getType === "function"
          ? String((m._getType as => unknown)())
          : String(m.constructor?.name ?? "").toLowerCase();
    let content = m.content;
    if (Array.isArray(content)) {
      const parts: string[] = [];
      for (const block of content) {
        if (block && typeof block === "object") {
          const b = block as Record<string, unknown>;
          if (b.type === "text" && typeof b.text === "string") {
            parts.push(b.text);
          }
        } else if (typeof block === "string") {
          parts.push(block);
        }
      }
      content = parts.join(" ");
    }
    const text = typeof content === "string" ? content : String(content ?? "");
    if (type.includes("system")) systemPrompt = text;
    else if (type.includes("human") || type.includes("user")) userMessage = text;
  }
  return { userMessage, systemPrompt };
}

function extractResponseText(output: unknown): string {
  if (!output || typeof output !== "object") return "";
  const o = output as Record<string, unknown>;
  // LangChain's LLMResult shape: { generations: [[{ text }]] }
  const gens = o.generations;
  if (Array.isArray(gens) && gens.length > 0) {
    const inner = gens[0];
    if (Array.isArray(inner) && inner.length > 0) {
      const g = inner[0] as Record<string, unknown>;
      if (typeof g.text === "string") return g.text;
      if (g.message && typeof g.message === "object") {
        const msg = g.message as Record<string, unknown>;
        if (typeof msg.content === "string") return msg.content;
      }
    }
  }
  if (typeof o.text === "string") return o.text;
  return "";
}

function extractTokenUsage(output: unknown): {
  inputTokens: number;
  outputTokens: number;
} {
  if (!output || typeof output !== "object") {
    return { inputTokens: 0, outputTokens: 0 };
  }
  const o = output as Record<string, unknown>;
  // LangChain shapes: llmOutput.tokenUsage / usage_metadata / usage.
  const containers = [o.llmOutput, o.usage_metadata, o.usage].filter(
    (c): c is Record<string, unknown> =>
      c !== null && typeof c === "object" && !Array.isArray(c),
  );
  for (const c of containers) {
    const usage =
      c.tokenUsage && typeof c.tokenUsage === "object"
        ? (c.tokenUsage as Record<string, unknown>)
        : c;
    const inputCandidates = [
      usage.input_tokens,
      usage.inputTokens,
      usage.prompt_tokens,
      usage.promptTokens,
    ];
    const outputCandidates = [
      usage.output_tokens,
      usage.outputTokens,
      usage.completion_tokens,
      usage.completionTokens,
    ];
    const inputTokens = inputCandidates.find(
      (v) => typeof v === "number",
    ) as number | undefined;
    const outputTokens = outputCandidates.find(
      (v) => typeof v === "number",
    ) as number | undefined;
    if (inputTokens != null || outputTokens != null) {
      return {
        inputTokens: Math.trunc(inputTokens ?? 0),
        outputTokens: Math.trunc(outputTokens ?? 0),
      };
    }
  }
  return { inputTokens: 0, outputTokens: 0 };
}
