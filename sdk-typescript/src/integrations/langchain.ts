/**
 * LangChain.js integration for Mesedi (.C).
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
 * events, `llm_call` and `tool_call`, that Mesedi's detectors
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
import {
  ErrorClassValue,
  classifyByProvider,
  extractHttpStatus,
  extractRetryAfter,
} from "../errors.js";
import { Event, EventType, utcNowRfc3339 } from "../events.js";
import { structuredReturnValue } from "../tool.js";

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
//  user_message cap raised from 1000 to 8192 so backend
// detectors that score on the raw user_message content
// (token_waste requires a 2048+ char shared prefix, drift lexical
// uses the full text for similarity scoring) actually see the
// customer's prompt instead of a defensive truncation that defeated
// them. Wire payload size is bounded elsewhere via payload
// truncation on the outer envelope.
const MAX_USER_MSG = 8192;
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
  /** Stable lowercase provider identifier ("anthropic", "openai",
   * "cohere", "gemini", "ollama", "unknown"). Captured at start so
   * handleLLMEnd / handleLLMError emit consistent (provider,
   * error_class) tuples, matching the provider_incident detector's
   * cross-tenant clustering contract. */
  provider: string;
  userMessage: string;
  systemPrompt: string;
  startedAt: number;
}

interface ToolStartContext {
  name: string;
  inputStr: string;
  /** Raw input passed to the tool BEFORE truncation, so handleToolEnd
   * can emit a structured `arguments` shape when the input parses as
   * JSON (tool_schema_drift fingerprints the argument shape too). */
  rawInput: unknown;
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
 * detector chain: drift, identical / similar-call loops,
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
      const serialized = llm as unknown as Record<string, unknown>;
      const model = extractModel(serialized, extraParams);
      const provider = extractProvider(serialized);
      const userMessage =
        prompts.length > 0 ? (prompts[prompts.length - 1] ?? "") : "";
      this.llmStarts.set(runId, {
        model,
        provider,
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
      const serialized = llm as unknown as Record<string, unknown>;
      const model = extractModel(serialized, extraParams);
      const provider = extractProvider(serialized);
      const lastConversation: unknown[] =
        messages.length > 0 ? (messages[messages.length - 1] ?? []) : [];
      const { userMessage, systemPrompt } =
        extractRoleMessages(lastConversation);
      this.llmStarts.set(runId, {
        model,
        provider,
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
        provider: ctx.provider,
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
    err: unknown,
    runId: string,
    _parentRunId?: string,
    _tags?: string[],
  ): void {
    try {
      const ctx = this.llmStarts.get(runId);
      if (!ctx) return;
      this.llmStarts.delete(runId);
      const durationMs = Math.max(0, Date.now() - ctx.startedAt);
      const rawExc = unwrapProviderException(err);
      const [excType, excMessage] = extractError(rawExc);
      const errorClass = classifyByProvider(ctx.provider, rawExc);
      const httpStatus = extractHttpStatus(rawExc);
      const retryAfter = extractRetryAfter(rawExc);
      emitLlmCallEvent({
        model: ctx.model,
        provider: ctx.provider,
        userMessage: ctx.userMessage,
        systemPrompt: ctx.systemPrompt,
        responseText: "",
        inputTokens: 0,
        outputTokens: 0,
        durationMs,
        status: "failed",
        errorClass,
        exceptionType: excType,
        exceptionMessage: excMessage,
        httpStatus,
        retryAfter,
      });
    } catch (e) {
      console.warn("mesedi langchain handler: handleLLMError error", e);
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
      // LangChain always hands us `input` as a string. Try to parse
      // it as JSON so `arguments` on the emitted event carries a
      // structured shape when the tool takes a JSON payload (the
      // common LangChain tool pattern). Falls through to the raw
      // string when parsing fails.
      let rawInput: unknown = input;
      if (typeof input === "string") {
        rawInput = tryParseJson(input);
      }
      this.toolStarts.set(runId, {
        name,
        inputStr,
        rawInput,
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
      // LangChain tools frequently return a string that happens to be
      // JSON (e.g. from a StructuredTool). Try parsing so the
      // tool_schema_drift detector sees the underlying shape rather
      // than a truncated string it can't fingerprint.
      let rawOutput: unknown = output;
      if (typeof output === "string") {
        rawOutput = tryParseJson(output);
      }
      emitToolCallEvent({
        toolName: ctx.name,
        inputStr: ctx.inputStr,
        rawInput: ctx.rawInput,
        resultSummary,
        rawOutput,
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
        rawInput: ctx.rawInput,
        resultSummary: "",
        rawOutput: undefined,
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
  provider: string;
  userMessage: string;
  systemPrompt: string;
  responseText: string;
  inputTokens: number;
  outputTokens: number;
  durationMs: number;
  status: "ok" | "failed";
  errorClass?: ErrorClassValue;
  exceptionType?: string;
  exceptionMessage?: string;
  httpStatus?: number;
  retryAfter?: number;
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
    // Stable lowercase provider identifier. Always emitted (even on
    // "unknown") so the backend's provider_incident detector can
    // cluster cross-tenant signals on (provider, error_class)
    // regardless of which framework the customer wraps.
    provider: args.provider,
    system_prompt: truncate(args.systemPrompt, MAX_SYSTEM),
    user_message: truncate(args.userMessage, MAX_USER_MSG),
    status: args.status,
  };
  if (args.status === "ok") {
    payload["response_text"] = truncate(args.responseText, MAX_RESPONSE);
    payload["input_tokens"] = args.inputTokens;
    payload["output_tokens"] = args.outputTokens;
  } else {
    // Canonical closed-vocabulary error class. Backend groups
    // multi-provider signals on this bucket; free-form strings would
    // defeat cross-provider clustering.
    if (args.errorClass) payload["error_class"] = args.errorClass;
    if (args.exceptionType) payload["exception_type"] = args.exceptionType;
    if (args.exceptionMessage) {
      payload["exception_message"] = truncate(
        args.exceptionMessage,
        MAX_EXC_MSG,
      );
    }
    if (typeof args.httpStatus === "number") {
      payload["http_status"] = args.httpStatus;
    }
    if (typeof args.retryAfter === "number") {
      payload["retry_after"] = args.retryAfter;
    }
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
  rawInput: unknown;
  resultSummary: string;
  rawOutput: unknown;
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

  // Assemble `arguments` in the @mesedi.tool wire shape:
  //   { args: [...], kwargs: {...} }
  //
  // Prefer a structured (schema-preserving) representation ONLY when
  // the raw input actually parsed into a non-string container
  // (object / array / number / bool). tool_schema_drift fingerprints
  // this shape. For plain strings we keep the truncated string
  // branch — the wire budget MAX_TOOL_INPUT_REPR still applies.
  const rawInputIsStructured =
    args.rawInput !== null &&
    args.rawInput !== undefined &&
    typeof args.rawInput !== "string";
  const structuredArgs = rawInputIsStructured
    ? structuredReturnValue(args.rawInput)
    : undefined;
  const argsSlot: unknown =
    structuredArgs !== undefined
      ? structuredArgs
      : truncate(args.inputStr, MAX_TOOL_INPUT_REPR);
  const payload: Record<string, unknown> = {
    tool_name: args.toolName,
    arguments: {
      args: [argsSlot],
      kwargs: {},
    },
    status: args.status,
  };
  if (args.status === "ok") {
    payload["result_summary"] = truncate(
      args.resultSummary,
      MAX_TOOL_OUTPUT_REPR,
    );
    // Structured JSON-native form for backend detectors. Matches
    // @mesedi.tool's return_value contract: the walker
    // preserves type via typed sentinels, so
    // tool_schema_drift can fingerprint the shape. Absent when the
    // output isn't safely serializable (functions, circular refs).
    const structuredReturn = structuredReturnValue(args.rawOutput);
    if (structuredReturn !== undefined) {
      payload["return_value"] = structuredReturn;
    }
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

/**
 * Return a stable lowercase provider identifier for a LangChain
 * serialized LLM object, defaulting to "unknown" when it can't be
 * derived. Matches the shipped `provider` values used by the direct
 * instrumentation modules (anthropic_integration, openai_integration,
 * etc.) so the provider_incident detector clusters cross-tenant
 * signals on ONE bucket regardless of whether the customer wraps
 * their LLM directly or via LangChain.
 *
 * LangChain's `serialized.id` is an array like
 *   ["langchain", "chat_models", "anthropic", "ChatAnthropic"]
 *   ["@langchain/anthropic", "chat_models", "ChatAnthropic"]
 *   ["langchain_anthropic", "chat_models", "ChatAnthropic"]
 * across versions. We walk the array looking for the first known
 * provider substring; if none matches, we probe the tail element's
 * class name for a "Chat<Provider>" prefix.
 */
function extractProvider(
  serialized: Record<string, unknown> | undefined,
): string {
  if (!serialized || typeof serialized !== "object") return "unknown";
  const ident = serialized["id"];
  if (Array.isArray(ident)) {
    for (const segRaw of ident) {
      if (typeof segRaw !== "string") continue;
      const seg = segRaw.toLowerCase();
      for (const known of KNOWN_PROVIDERS) {
        if (seg.includes(known)) return normalizeProvider(known);
      }
    }
    // Fallback: parse class name at the tail
    // (e.g. "ChatAnthropic" -> "anthropic").
    const tail = ident[ident.length - 1];
    if (typeof tail === "string") {
      const cls = tail.toLowerCase();
      for (const known of KNOWN_PROVIDERS) {
        if (cls.includes(known)) return normalizeProvider(known);
      }
    }
  }
  // Last-resort probe on serialized.name.
  const name = serialized["name"];
  if (typeof name === "string") {
    const cls = name.toLowerCase();
    for (const known of KNOWN_PROVIDERS) {
      if (cls.includes(known)) return normalizeProvider(known);
    }
  }
  return "unknown";
}

/** Known provider substrings, ordered so the more specific match
 * wins (e.g. "vertexai" before "gemini" because Vertex-hosted Gemini
 * calls also contain "gemini" in some serialized paths, but the
 * canonical bucket is "vertexai"). */
const KNOWN_PROVIDERS: readonly string[] = [
  "anthropic",
  "openai",
  "cohere",
  "vertexai",
  "vertex",
  "gemini",
  "googlegenai",
  "ollama",
  "mistral",
  "groq",
  "bedrock",
];

/** Collapse the KNOWN_PROVIDERS variants to the canonical bucket the
 * backend expects. Kept aligned with the values shipped by the
 * direct instrumentation modules. */
function normalizeProvider(known: string): string {
  if (known === "vertexai" || known === "vertex") return "vertexai";
  if (known === "googlegenai") return "gemini";
  if (known === "bedrock") return "bedrock";
  return known;
}

/**
 * LangChain often wraps the provider SDK exception in its own
 * envelope (`LangChainError`, `RetryError`, etc.) with the real
 * exception dangling off `.cause`. Walk one hop to reach the raw
 * provider exception so the class-name-based classifier scores it
 * correctly. Falls back to the original object when no cause chain
 * exists.
 */
function unwrapProviderException(err: unknown): unknown {
  if (!err || typeof err !== "object") return err;
  const cause = (err as { cause?: unknown }).cause;
  if (cause instanceof Error) return cause;
  return err;
}

/**
 * Try to JSON.parse a string; return the parsed value on success,
 * the original string on failure. Used to derive a structured shape
 * for `arguments` / `return_value` from LangChain's stringly-typed
 * tool I/O so tool_schema_drift can fingerprint the underlying
 * shape.
 */
function tryParseJson(s: string): unknown {
  if (typeof s !== "string") return s;
  const trimmed = s.trim();
  if (!trimmed) return s;
  const first = trimmed[0] ?? "";
  // Cheap-guard: only try JSON.parse on strings that could plausibly
  // be JSON. Avoids a try/catch cycle for every plain-text output.
  if (
    first !== "{" &&
    first !== "[" &&
    first !== '"' &&
    !(first >= "0" && first <= "9") &&
    first !== "-" &&
    trimmed !== "true" &&
    trimmed !== "false" &&
    trimmed !== "null"
  ) {
    return s;
  }
  try {
    return JSON.parse(trimmed);
  } catch {
    return s;
  }
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
