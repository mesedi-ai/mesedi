/**
 * LangChain.js integration for Mesedi (Wave 4.C).
 *
 * Ships as a LangChain callback handler that emits Mesedi telemetry.
 * The customer wraps their entry function with `mesedi.wrap()` (which
 * owns the execution boundary) and attaches an instance of this
 * handler to LangChain's `callbacks:` slot (which owns intra-execution
 * events).
 *
 * Usage:
 *
 *     import { wrap } from "mesedi";
 *     import { MesediLangChainCallbackHandler } from "mesedi/integrations/langchain";
 *     import { ChatOpenAI } from "@langchain/openai";
 *
 *     export const runAgent = wrap(
 *       { agentName: "support-triage" },
 *       async (question: string) => {
 *         const llm = new ChatOpenAI({ model: "gpt-4o" });
 *         const result = await llm.invoke(
 *           [{ role: "user", content: question }],
 *           { callbacks: [new MesediLangChainCallbackHandler()] },
 *         );
 *         return String(result.content);
 *       },
 *     );
 *
 * Design.
 *
 * `mesedi.wrap()` manages the execution boundary (started / completed /
 * crash signature). The callback handler emits the intra-execution
 * events — `llm_call` and `tool_call` — that Mesedi's detectors
 * consume. Splitting responsibility this way lets the customer adopt
 * Mesedi without reshaping their LangChain code: `wrap` goes around
 * the entry point, the handler gets attached at the existing
 * `callbacks:` slot LangChain already accepts.
 *
 * Mirrors the Python `mesedi.integrations.langchain.MesediCallbackHandler`
 * surface. Wire format is byte-identical so backend detectors see one
 * unified event stream whether the customer writes Python or TypeScript.
 *
 * Fail-open posture matches the rest of the SDK: any exception inside
 * a handler method is swallowed and logged via `console.warn`. Never
 * blocks or breaks the customer's LangChain pipeline.
 *
 * Out of scope for v1:
 *   - Streaming responses (`handleLLMNewToken`). Receivers see only
 *     the final assembled response. Streaming attribution is a v2
 *     concern that needs an event-payload schema change.
 *   - Per-chain depth tracking. Every chain start fires but we ignore
 *     it because `wrap` already owns the execution boundary. A
 *     chain-as-execution mode (no `wrap` required) is a later
 *     iteration.
 *   - Multi-modal content blocks (images in messages). We extract the
 *     text parts and ignore the rest.
 */

import { getClient } from "../client.js";
import { currentExecutionContext, newEventId } from "../context.js";
import { Event, EventType, utcNowRfc3339 } from "../events.js";

// ── Peer-dep imports ─────────────────────────────────────────────────
//
// `@langchain/core` is declared as an OPTIONAL peer dependency in
// package.json. Customers who never import this module never pay the
// install cost. `tsc` resolves `BaseCallbackHandler` against the
// devDependency version at build time; at runtime the customer's
// installed `@langchain/core` provides the actual class.

import { BaseCallbackHandler } from "@langchain/core/callbacks/base";
import type { Serialized } from "@langchain/core/load/serializable";
import type { BaseMessage } from "@langchain/core/messages";

// Truncation budgets. Kept in sync with `vercel_ai.ts` / `mastra.ts` /
// the Python `emit_llm_call` so wire-format payloads from this
// adapter and from hand-written code are byte-indistinguishable.
const MAX_SYSTEM = 1000;
const MAX_USER_MSG = 1000;
const MAX_RESPONSE = 1000;
const MAX_TOOL_INPUT_REPR = 200;
const MAX_TOOL_OUTPUT_REPR = 500;
const MAX_EXC_MSG = 500;

// ── In-flight state ──────────────────────────────────────────────────
//
// LangChain assigns each LLM / tool invocation a `runId` string on
// start; the matching `end` (or `error`) callback echoes it back. We
// use this to pair start + end and compute duration.

interface LLMStartContext {
  model: string;
  userMessage: string;
  systemPrompt: string;
  startedAt: number;
}

interface ToolStartContext {
  name: string;
  inputStr: string;
  startedAt: number;
}

/**
 * LangChain callback handler that emits Mesedi events. Attach an
 * instance to any LangChain runnable via the standard `callbacks:`
 * config slot:
 *
 *     runnable.invoke(input, { callbacks: [new MesediLangChainCallbackHandler()] });
 *
 * Emits one `llm_call` event per LLM invocation (matching the wire
 * format from `emit_llm_call` and the Anthropic patch) and one
 * `tool_call` event per tool invocation (matching the wire format
 * from `@mesedi.tool`). Both event types feed the standard Mesedi
 * detector chain — drift, identical / similar-call loops,
 * tool-failures, cost-velocity, prompt-injection.
 *
 * Outside a `wrap()` execution context, all emissions silently no-op
 * (matches the rest of Mesedi's observe layer).
 */
export class MesediLangChainCallbackHandler extends BaseCallbackHandler {
  name = "mesedi-langchain";

  /**
   * Tell LangChain's dispatcher not to reraise handler exceptions.
   * We already swallow inside each method; this is belt-and-suspenders
   * for any future LangChain version that changes the default.
   */
  raiseError = false;

  private readonly llmStarts: Map<string, LLMStartContext> = new Map();
  private readonly toolStarts: Map<string, ToolStartContext> = new Map();

  // ── LLM events ──────────────────────────────────────────────────

  handleLLMStart(
    llm: Serialized,
    prompts: string[],
    runId: string,
    _parentRunId?: string,
    extraParams?: Record<string, unknown>,
    _tags?: string[],
    _metadata?: Record<string, unknown>,
    _runName?: string,
  ): void {
    try {
      const model = extractModel(llm as unknown as Record<string, unknown>, extraParams);
      const userMessage =
        prompts.length > 0 ? (prompts[prompts.length - 1] ?? "") : "";
      this.llmStarts.set(runId, {
        model,
        userMessage,
        systemPrompt: "",
        startedAt: Date.now(),
      });
    } catch (err) {
      console.warn("mesedi langchain handler: handleLLMStart error", err);
    }
  }

  handleChatModelStart(
    llm: Serialized,
    messages: BaseMessage[][],
    runId: string,
    _parentRunId?: string,
    extraParams?: Record<string, unknown>,
    _tags?: string[],
    _metadata?: Record<string, unknown>,
    _runName?: string,
  ): void {
    try {
      const model = extractModel(llm as unknown as Record<string, unknown>, extraParams);
      const lastConversation: unknown[] =
        messages.length > 0 ? (messages[messages.length - 1] ?? []) : [];
      const { userMessage, systemPrompt } =
        extractRoleMessages(lastConversation);
      this.llmStarts.set(runId, {
        model,
        userMessage,
        systemPrompt,
        startedAt: Date.now(),
      });
    } catch (err) {
      console.warn(
        "mesedi langchain handler: handleChatModelStart error",
        err,
      );
    }
  }

  handleLLMEnd(
    output: unknown,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
  ): void {
    try {
      const ctx = this.llmStarts.get(runId);
      if (!ctx) return;
      this.llmStarts.delete(runId);
      const responseText = extractResponseText(output);
      const [inputTokens, outputTokens] = extractTokenUsage(output);
      const durationMs = Math.max(0, Date.now() - ctx.startedAt);
      emitLlmCallEvent({
        model: ctx.model,
        userMessage: ctx.userMessage,
        systemPrompt: ctx.systemPrompt,
        responseText,
        inputTokens,
        outputTokens,
        durationMs,
        status: "ok",
      });
    } catch (err) {
      console.warn("mesedi langchain handler: handleLLMEnd error", err);
    }
  }

  handleLLMError(
    _err: unknown,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
  ): void {
    try {
      const ctx = this.llmStarts.get(runId);
      if (!ctx) return;
      this.llmStarts.delete(runId);
      const durationMs = Math.max(0, Date.now() - ctx.startedAt);
      emitLlmCallEvent({
        model: ctx.model,
        userMessage: ctx.userMessage,
        systemPrompt: ctx.systemPrompt,
        responseText: "",
        inputTokens: 0,
        outputTokens: 0,
        durationMs,
        status: "failed",
      });
    } catch (err) {
      console.warn("mesedi langchain handler: handleLLMError error", err);
    }
  }

  // ── Tool events ─────────────────────────────────────────────────

  handleToolStart(
    tool: Serialized,
    input: string,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
    _metadata?: Record<string, unknown>,
    _name?: string,
  ): void {
    try {
      const name = extractToolName(tool as unknown as Record<string, unknown>);
      const inputStr = typeof input === "string" ? input : reprValue(input);
      this.toolStarts.set(runId, {
        name,
        inputStr,
        startedAt: Date.now(),
      });
    } catch (err) {
      console.warn("mesedi langchain handler: handleToolStart error", err);
    }
  }

  handleToolEnd(
    output: unknown,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
  ): void {
    try {
      const ctx = this.toolStarts.get(runId);
      if (!ctx) return;
      this.toolStarts.delete(runId);
      const durationMs = Math.max(0, Date.now() - ctx.startedAt);
      const resultSummary =
        output == null
          ? ""
          : typeof output === "string"
            ? output
            : reprValue(output);
      emitToolCallEvent({
        toolName: ctx.name,
        inputStr: ctx.inputStr,
        resultSummary,
        durationMs,
        status: "ok",
      });
    } catch (err) {
      console.warn("mesedi langchain handler: handleToolEnd error", err);
    }
  }

  handleToolError(
    err: unknown,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
  ): void {
    try {
      const ctx = this.toolStarts.get(runId);
      if (!ctx) return;
      this.toolStarts.delete(runId);
      const durationMs = Math.max(0, Date.now() - ctx.startedAt);
      const [excType, excMessage] = extractError(err);
      emitToolCallEvent({
        toolName: ctx.name,
        inputStr: ctx.inputStr,
        resultSummary: "",
        durationMs,
        status: "failed",
        exceptionType: excType,
        exceptionMessage: excMessage,
      });
    } catch (e) {
      console.warn("mesedi langchain handler: handleToolError error", e);
    }
  }
}

// ── Emitters ────────────────────────────────────────────────────────

function emitLlmCallEvent(args: {
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

  // Halt-safe boundary at LLM call. Matches @mesedi.tool, the
  // Anthropic patch, and vercel_ai's emitLlmCallEvent.
  ctx.checkBudget();
  if (ctx.budgetTracker) {
    ctx.budgetTracker.incrementSteps();
    if (args.inputTokens > 0 || args.outputTokens > 0) {
      ctx.budgetTracker.addTokens(args.inputTokens, args.outputTokens);
    }
  }

  const payload: Record<string, unknown> = {
    model: args.model,
    system_prompt: truncate(args.systemPrompt, MAX_SYSTEM),
    user_message: truncate(args.userMessage, MAX_USER_MSG),
    status: args.status,
  };
  if (args.status === "ok") {
    payload["response_text"] = truncate(args.responseText, MAX_RESPONSE);
    payload["input_tokens"] = args.inputTokens;
    payload["output_tokens"] = args.outputTokens;
  }

  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.LLM_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: args.durationMs,
    payload,
  };
  getClient().submitEvent(event);
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

  // Halt-safe boundary at tool call, matches @mesedi.tool.
  ctx.checkBudget();
  if (ctx.budgetTracker) {
    ctx.budgetTracker.incrementSteps();
  }

  const payload: Record<string, unknown> = {
    tool_name: args.toolName,
    // LangChain tools take a single input string. Mimic the
    // @mesedi.tool wire shape ({args: [...], kwargs: {...}}) by
    // dropping the input into args[0]; kwargs stays empty.
    arguments: {
      args: [truncate(args.inputStr, MAX_TOOL_INPUT_REPR)],
      kwargs: {},
    },
    status: args.status,
  };
  if (args.status === "ok") {
    payload["result_summary"] = truncate(
      args.resultSummary,
      MAX_TOOL_OUTPUT_REPR,
    );
  } else {
    if (args.exceptionType) payload["exception_type"] = args.exceptionType;
    if (args.exceptionMessage) {
      payload["exception_message"] = truncate(
        args.exceptionMessage,
        MAX_EXC_MSG,
      );
    }
  }

  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.TOOL_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: args.durationMs,
    payload,
  };
  getClient().submitEvent(event);
}

// ── Extractors ──────────────────────────────────────────────────────
//
// LangChain's serialized-object shape and message-class hierarchy have
// shifted across versions. Each extractor below tries the known shapes
// in order and falls through to a safe default so the adapter still
// emits a useful event on a version we haven't seen.

function extractModel(
  serialized: Record<string, unknown> | undefined,
  extraParams: Record<string, unknown> | undefined,
): string {
  const inv =
    (extraParams?.["invocation_params"] as Record<string, unknown> | undefined) ??
    {};
  for (const key of ["model", "model_name", "deployment_name"]) {
    const v = inv[key];
    if (typeof v === "string" && v) return v;
  }
  if (serialized && typeof serialized === "object") {
    const kw =
      (serialized["kwargs"] as Record<string, unknown> | undefined) ?? {};
    for (const key of ["model", "model_name", "deployment_name"]) {
      const v = kw[key];
      if (typeof v === "string" && v) return v;
    }
    const ident = serialized["id"];
    if (Array.isArray(ident) && ident.length > 0) {
      const last = ident[ident.length - 1];
      if (typeof last === "string" && last) return last;
    }
    const name = serialized["name"];
    if (typeof name === "string" && name) return name;
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
    const m = msg as {
      type?: string;
      _getType?: () => string;
      content?: unknown;
      constructor?: { name?: string };
    };
    let msgType = "";
    if (typeof m.type === "string" && m.type) {
      msgType = m.type;
    } else if (typeof m._getType === "function") {
      try {
        msgType = m._getType();
      } catch {
        // ignore, fall through to constructor name
      }
    }
    if (!msgType && m.constructor?.name) {
      msgType = m.constructor.name;
    }
    let content: unknown = m.content;
    if (Array.isArray(content)) {
      const parts: string[] = [];
      for (const block of content) {
        if (typeof block === "string") {
          parts.push(block);
        } else if (block && typeof block === "object") {
          const b = block as { type?: string; text?: string };
          if (b.type === "text" && typeof b.text === "string") {
            parts.push(b.text);
          }
        }
      }
      content = parts.join(" ");
    }
    const contentStr =
      typeof content === "string" ? content : content == null ? "" : String(content);
    const t = msgType.toLowerCase();
    if (t === "system" || t === "systemmessage") {
      systemPrompt = contentStr;
    } else if (
      t === "human" ||
      t === "humanmessage" ||
      t === "user" ||
      t === "usermessage"
    ) {
      userMessage = contentStr;
    }
  }
  return { userMessage, systemPrompt };
}

function extractResponseText(response: unknown): string {
  try {
    if (!response || typeof response !== "object") return "";
    const r = response as { generations?: unknown[][] };
    const generations = r.generations;
    if (!Array.isArray(generations) || generations.length === 0) return "";
    const firstBatch = generations[0];
    if (!Array.isArray(firstBatch) || firstBatch.length === 0) return "";
    const gen = firstBatch[0] as {
      text?: string;
      message?: { content?: unknown };
    };
    if (typeof gen?.text === "string" && gen.text) return gen.text;
    const message = gen?.message;
    if (message && typeof message === "object") {
      const content = message.content;
      if (typeof content === "string") return content;
      if (Array.isArray(content)) {
        const parts: string[] = [];
        for (const block of content) {
          if (typeof block === "string") {
            parts.push(block);
          } else if (block && typeof block === "object") {
            const b = block as { type?: string; text?: string };
            if (b.type === "text" && typeof b.text === "string") {
              parts.push(b.text);
            }
          }
        }
        return parts.join(" ");
      }
    }
  } catch {
    // fall through to empty
  }
  return "";
}

function extractTokenUsage(response: unknown): [number, number] {
  try {
    if (!response || typeof response !== "object") return [0, 0];
    const r = response as {
      llmOutput?: Record<string, unknown>;
      llm_output?: Record<string, unknown>;
      generations?: unknown[][];
    };
    const llmOutput = r.llmOutput ?? r.llm_output ?? {};
    const usageAny =
      llmOutput["tokenUsage"] ??
      llmOutput["token_usage"] ??
      llmOutput["usage"] ??
      {};
    const usage =
      typeof usageAny === "object" && usageAny !== null
        ? (usageAny as Record<string, unknown>)
        : {};
    const inputTokens =
      toInt(usage["promptTokens"]) ||
      toInt(usage["prompt_tokens"]) ||
      toInt(usage["inputTokens"]) ||
      toInt(usage["input_tokens"]);
    const outputTokens =
      toInt(usage["completionTokens"]) ||
      toInt(usage["completion_tokens"]) ||
      toInt(usage["outputTokens"]) ||
      toInt(usage["output_tokens"]);
    if (inputTokens > 0 || outputTokens > 0) {
      return [inputTokens, outputTokens];
    }
    // Newer LangChain: `usage_metadata` on the generation itself.
    const generations = r.generations;
    if (Array.isArray(generations) && generations.length > 0) {
      const firstBatch = generations[0];
      if (Array.isArray(firstBatch) && firstBatch.length > 0) {
        const gen = firstBatch[0] as {
          message?: {
            usage_metadata?: Record<string, unknown>;
            usageMetadata?: Record<string, unknown>;
          };
        };
        const meta =
          gen?.message?.usage_metadata ?? gen?.message?.usageMetadata ?? {};
        if (meta && typeof meta === "object") {
          return [
            toInt(meta["input_tokens"]) || toInt(meta["inputTokens"]),
            toInt(meta["output_tokens"]) || toInt(meta["outputTokens"]),
          ];
        }
      }
    }
  } catch {
    // fall through
  }
  return [0, 0];
}

function extractToolName(
  serialized: Record<string, unknown> | undefined,
): string {
  if (!serialized || typeof serialized !== "object") return "unknown_tool";
  const name = serialized["name"];
  if (typeof name === "string" && name) return name;
  const ident = serialized["id"];
  if (Array.isArray(ident) && ident.length > 0) {
    const last = ident[ident.length - 1];
    if (typeof last === "string" && last) return last;
  }
  return "unknown_tool";
}

function extractError(err: unknown): [string, string] {
  if (err instanceof Error) {
    return [err.constructor.name || "Error", err.message];
  }
  if (err && typeof err === "object") {
    const e = err as { name?: string; message?: string };
    return [
      typeof e.name === "string" && e.name ? e.name : "Error",
      typeof e.message === "string" ? e.message : "",
    ];
  }
  return ["Error", typeof err === "string" ? err : ""];
}

// ── Helpers ────────────────────────────────────────────────────────

function truncate(s: string, maxLen: number): string {
  if (!s) return "";
  if (s.length <= maxLen) return s;
  return s.slice(0, maxLen - 3) + "...";
}

function toInt(v: unknown): number {
  if (typeof v === "number" && Number.isFinite(v)) {
    return Math.max(0, Math.floor(v));
  }
  if (typeof v === "string") {
    const n = parseInt(v, 10);
    return Number.isFinite(n) ? Math.max(0, n) : 0;
  }
  return 0;
}

function reprValue(v: unknown): string {
  if (v == null) return "";
  if (typeof v === "string") return v;
  try {
    return JSON.stringify(v);
  } catch {
    return String(v);
  }
}
