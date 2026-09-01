/**
 * Google Gemini TypeScript SDK monkey-patch: auto-emit llm_call
 * events for every GenerativeModel.generateContent() call inside a
 * wrap()'d execution.
 *
 * Targets the @google/generative-ai package (the dominant non-
 * Vertex Gemini TS surface). Mirrors instrumentAnthropic /
 * instrumentOpenAI / instrumentCohere in fail-open semantics,
 * canonical error vocabulary, idempotency.
 *
 * Out of scope (filed as follow-ups):
 *   - generateContentStream / chat sessions
 *   - Vertex AI surface (@google-cloud/vertexai)
 *
 * Dependency injection: instrumentGemini accepts an optional class
 * argument so this is testable without installing the package.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import {
  classifyGeminiException,
  extractHttpStatus,
  extractRetryAfter,
} from "./errors.js";
import { EventType, utcNowRfc3339 } from "./events.js";
import { _maybeEmitThrottlingEvent } from "./observe.js";

const MAX_SYSTEM = 1000;
const MAX_USER_MSG = 1000;
const MAX_RESPONSE = 1000;
const MAX_EXC_MSG = 500;

const PROVIDER = "gemini";

const _patched = new WeakSet<object>();

export interface GenerativeModelClassLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  prototype: { generateContent: (...args: any[]) => Promise<any> };
}

type GenFn = (this: unknown, ...args: unknown[]) => Promise<unknown>;

interface GeminiPart {
  text?: string;
}
interface GeminiContent {
  role?: string;
  parts?: GeminiPart[];
}
interface GeminiResponse {
  response?: {
    text?: () => string;
    usageMetadata?: {
      promptTokenCount?: number;
      candidatesTokenCount?: number;
    };
  };
}

/** Patch GenerativeModel.generateContent to emit llm_call events. */
export async function instrumentGemini(
  modelClass?: GenerativeModelClassLike,
): Promise<boolean> {
  let cls = modelClass;
  if (!cls) {
    try {
      // eslint-disable-next-line @typescript-eslint/ban-ts-comment
      // @ts-ignore — package may not be installed; by design.
      const mod = (await import("@google/generative-ai")) as unknown as {
        GenerativeModel?: GenerativeModelClassLike;
      };
      cls = mod?.GenerativeModel;
    } catch {
      console.warn(
        "mesedi: @google/generative-ai not installed; instrumentGemini() is a no-op. " +
          "Install with `npm install @google/generative-ai` to enable.",
      );
      return false;
    }
    if (!cls) return false;
  }

  if (_patched.has(cls)) return true;

  const originalGenerate = cls.prototype.generateContent as GenFn;

  cls.prototype.generateContent = async function patchedGenerate(
    this: unknown,
    ...args: unknown[]
  ): Promise<unknown> {
    const ctx = currentExecutionContext();
    if (!ctx) return originalGenerate.apply(this, args);
    ctx.checkBudget();
    if (ctx.budgetTracker) ctx.budgetTracker.incrementSteps();

    const client = getClient();
    const sequence = ctx.nextSequence();
    const eventId = newEventId();

    const selfRecord = this as Record<string, unknown>;
    const model =
      typeof selfRecord.model === "string"
        ? selfRecord.model
        : typeof selfRecord.modelName === "string"
          ? selfRecord.modelName
          : "unknown";
    // System instruction is set when the GenerativeModel is
    // constructed (`systemInstruction` option). It surfaces on the
    // instance as `systemInstruction` (string OR Content object).
    const systemRaw = selfRecord.systemInstruction;
    const systemText = stringifyContent(systemRaw);

    const userMessage = extractUserMessage(args[0]);

    const start = performance.now();
    try {
      const response = (await originalGenerate.apply(this, args)) as GeminiResponse;
      const durationMs = Math.round(performance.now() - start);
      const { responseText, inputTokens, outputTokens } =
        extractResponseFields(response);
      if (ctx.budgetTracker) ctx.budgetTracker.addTokens(inputTokens, outputTokens);
      client.submitEvent({
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.LLM_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload: {
          provider: PROVIDER,
          surface: "chat",
          model,
          system_prompt: truncate(systemText, MAX_SYSTEM),
          user_message: truncate(userMessage, MAX_USER_MSG),
          response_text: truncate(responseText, MAX_RESPONSE),
          status: "ok",
          input_tokens: inputTokens,
          output_tokens: outputTokens,
        },
      });
      return response;
    } catch (err) {
      const durationMs = Math.round(performance.now() - start);
      const failurePayload: Record<string, unknown> = {
        provider: PROVIDER,
        surface: "chat",
        model,
        system_prompt: truncate(systemText, MAX_SYSTEM),
        user_message: truncate(userMessage, MAX_USER_MSG),
        status: "failed",
        error_class: classifyGeminiException(err),
        exception_type:
          err instanceof Error && err.constructor.name
            ? err.constructor.name
            : typeof err,
        exception_message: truncate(
          err instanceof Error ? err.message : String(err),
          MAX_EXC_MSG,
        ),
      };
      const httpStatus = extractHttpStatus(err);
      if (httpStatus !== undefined) failurePayload.http_status = httpStatus;
      const retryAfter = extractRetryAfter(err);
      if (retryAfter !== undefined) failurePayload.retry_after_seconds = retryAfter;
      client.submitEvent({
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.LLM_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload: failurePayload,
      });
      //  auto-emit infrastructure_event on throttling-class
      // exceptions so infrastructure_throttled isn't silently inactive.
      _maybeEmitThrottlingEvent({
        provider: PROVIDER,
        errorClass: failurePayload.error_class as string,
        httpStatus,
        retryAfterSeconds: retryAfter,
        endpoint: "/v1/models/generateContent",
      });
      throw err;
    }
  } as GenFn;

  _patched.add(cls);
  return true;
}

function extractUserMessage(arg: unknown): string {
  if (typeof arg === "string") return arg;
  if (Array.isArray(arg)) {
    // Array of strings OR Content objects. Walk for the last
    // role:user (or any role-less string) entry.
    for (let i = arg.length - 1; i >= 0; i--) {
      const item = arg[i];
      if (typeof item === "string") return item;
      if (item && typeof item === "object") {
        const obj = item as GeminiContent & { text?: string };
        if (obj.role && obj.role !== "user") continue;
        if (typeof obj.text === "string") return obj.text;
        if (Array.isArray(obj.parts)) {
          const parts: string[] = [];
          for (const p of obj.parts) {
            if (typeof p?.text === "string") parts.push(p.text);
          }
          return parts.join("\n");
        }
      }
    }
    return "";
  }
  if (arg && typeof arg === "object") {
    // GenerateContentRequest-like: { contents: [...] }
    const obj = arg as { contents?: unknown };
    if (obj.contents !== undefined) return extractUserMessage(obj.contents);
  }
  return "";
}

function stringifyContent(content: unknown): string {
  if (typeof content === "string") return content;
  if (!content || typeof content !== "object") return "";
  const obj = content as GeminiContent & { text?: string };
  if (typeof obj.text === "string") return obj.text;
  if (Array.isArray(obj.parts)) {
    const bits: string[] = [];
    for (const p of obj.parts) {
      if (typeof p?.text === "string") bits.push(p.text);
    }
    return bits.join("\n");
  }
  return "";
}

function extractResponseFields(response: GeminiResponse): {
  responseText: string;
  inputTokens: number;
  outputTokens: number;
} {
  let responseText = "";
  let inputTokens = 0;
  let outputTokens = 0;
  try {
    if (typeof response?.response?.text === "function") {
      const text = response.response.text();
      if (typeof text === "string") responseText = text;
    }
  } catch {
    /* best-effort */
  }
  try {
    const usage = response?.response?.usageMetadata;
    if (usage) {
      inputTokens = Number(usage.promptTokenCount ?? 0) || 0;
      outputTokens = Number(usage.candidatesTokenCount ?? 0) || 0;
    }
  } catch {
    /* best-effort */
  }
  return { responseText, inputTokens, outputTokens };
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 3) + "...";
}

// ── non-chat surface — embedContent (TS twin) ─────────────────

export interface GenerativeModelEmbedClassLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  prototype: { embedContent: (...args: any[]) => Promise<any> };
}

const _embedPatched = new WeakSet<object>();

function extractEmbedContentInput(arg: unknown): string {
  if (typeof arg === "string") return arg;
  if (arg && typeof arg === "object") {
    const obj = arg as { content?: unknown };
    if (obj.content !== undefined) {
      const c = obj.content;
      if (typeof c === "string") return c;
      return stringifyContent(c);
    }
    return stringifyContent(arg);
  }
  return "";
}

/** Patch GenerativeModel.embedContent to emit
 * surface='embeddings' llm_call events. Idempotent per class. */
export function patchGeminiEmbedContent(
  cls: GenerativeModelEmbedClassLike,
): void {
  if (_embedPatched.has(cls)) return;
  const originalEmbed = cls.prototype.embedContent as (
    this: unknown,
    ...args: unknown[]
  ) => Promise<unknown>;

  cls.prototype.embedContent = async function patchedEmbed(
    this: unknown,
    ...args: unknown[]
  ): Promise<unknown> {
    const ctx = currentExecutionContext();
    if (!ctx) return originalEmbed.apply(this, args);
    ctx.checkBudget();
    if (ctx.budgetTracker) ctx.budgetTracker.incrementSteps();

    const client = getClient();
    const sequence = ctx.nextSequence();
    const eventId = newEventId();

    const selfRecord = this as Record<string, unknown>;
    const model =
      typeof selfRecord.model === "string"
        ? selfRecord.model
        : typeof selfRecord.modelName === "string"
          ? selfRecord.modelName
          : "unknown";
    const userMessage = extractEmbedContentInput(args[0]);

    const start = performance.now();
    try {
      const response = await originalEmbed.apply(this, args);
      const durationMs = Math.round(performance.now() - start);
      client.submitEvent({
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.LLM_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload: {
          provider: PROVIDER,
          surface: "embeddings",
          model,
          user_message: truncate(userMessage, MAX_USER_MSG),
          response_text: "<embedding vectors>",
          status: "ok",
          input_tokens: 0,
          output_tokens: 0,
        },
      });
      return response;
    } catch (err) {
      const durationMs = Math.round(performance.now() - start);
      const failurePayload: Record<string, unknown> = {
        provider: PROVIDER,
        surface: "embeddings",
        model,
        user_message: truncate(userMessage, MAX_USER_MSG),
        status: "failed",
        error_class: classifyGeminiException(err),
        exception_type:
          err instanceof Error && err.constructor.name
            ? err.constructor.name
            : typeof err,
        exception_message: truncate(
          err instanceof Error ? err.message : String(err),
          MAX_EXC_MSG,
        ),
      };
      const httpStatus = extractHttpStatus(err);
      if (httpStatus !== undefined) failurePayload.http_status = httpStatus;
      const retryAfter = extractRetryAfter(err);
      if (retryAfter !== undefined) failurePayload.retry_after_seconds = retryAfter;
      client.submitEvent({
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.LLM_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload: failurePayload,
      });
      _maybeEmitThrottlingEvent({
        provider: PROVIDER,
        errorClass: failurePayload.error_class as string,
        httpStatus,
        retryAfterSeconds: retryAfter,
        endpoint: "/v1/models/embedContent",
      });
      throw err;
    }
  } as (this: unknown, ...args: unknown[]) => Promise<unknown>;

  _embedPatched.add(cls);
}
