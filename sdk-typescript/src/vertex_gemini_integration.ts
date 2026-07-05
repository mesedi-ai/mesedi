/**
 * Vertex AI Gemini TypeScript SDK monkey-patch — auto-emit llm_call
 * events for every GenerativeModel.generateContent() call inside a
 * wrap()'d execution against the Vertex AI surface
 * (@google-cloud/vertexai package).
 *
 * Same provider tag ("gemini") and same canonical error_class map
 * as instrumentGemini. Customers running both surfaces call both
 * instrumentGemini() and instrumentVertexGemini() — each patches a
 * different package's class. Provider_incident detection sees one
 * signal regardless of which surface was hit.
 *
 * Dependency injection: instrumentVertexGemini accepts an optional
 * class argument so this code is testable without installing
 * @google-cloud/vertexai.
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
const ENDPOINT =
  "/v1/projects/*/locations/*/publishers/google/models/*:generateContent";

const _patched = new WeakSet<object>();

export interface VertexGenerativeModelClassLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  prototype: { generateContent: (...args: any[]) => Promise<any> };
}

type GenFn = (this: unknown, ...args: unknown[]) => Promise<unknown>;

interface VertexResponse {
  response?: {
    candidates?: Array<{
      content?: { parts?: Array<{ text?: string }> };
    }>;
    usageMetadata?: {
      promptTokenCount?: number;
      candidatesTokenCount?: number;
    };
  };
}

/** Patch Vertex AI's GenerativeModel.generateContent to emit
 * llm_call events. Returns true if patched OR was a no-op re-patch.
 * Returns false if @google-cloud/vertexai isn't installed AND no
 * class was provided. */
export async function instrumentVertexGemini(
  modelClass?: VertexGenerativeModelClassLike,
): Promise<boolean> {
  let cls = modelClass;
  if (!cls) {
    try {
      // eslint-disable-next-line @typescript-eslint/ban-ts-comment
      // @ts-ignore — package may not be installed; by design.
      const mod = (await import("@google-cloud/vertexai")) as unknown as {
        GenerativeModel?: VertexGenerativeModelClassLike;
      };
      cls = mod?.GenerativeModel;
    } catch {
      console.warn(
        "mesedi: @google-cloud/vertexai not installed; " +
          "instrumentVertexGemini() is a no-op. " +
          "Install with `npm install @google-cloud/vertexai` to enable.",
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
    // Vertex stores the model identifier on different attrs across
    // SDK versions; walk the candidates defensively.
    const model =
      (typeof selfRecord.model === "string" && selfRecord.model) ||
      (typeof selfRecord.modelName === "string" && selfRecord.modelName) ||
      (typeof selfRecord._publisherModel === "string" &&
        selfRecord._publisherModel) ||
      "unknown";
    const systemRaw =
      selfRecord.systemInstruction ?? selfRecord._systemInstruction;
    const systemText = stringifySystemInstruction(systemRaw);

    const userMessage = extractUserMessage(args[0]);

    const start = performance.now();
    try {
      const response = (await originalGenerate.apply(this, args)) as VertexResponse;
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
      _maybeEmitThrottlingEvent({
        provider: PROVIDER,
        errorClass: failurePayload.error_class as string,
        httpStatus,
        retryAfterSeconds: retryAfter,
        endpoint: ENDPOINT,
      });
      throw err;
    }
  } as GenFn;

  _patched.add(cls);
  return true;
}

interface VertexPart {
  text?: string;
}
interface VertexContent {
  role?: string;
  parts?: VertexPart[];
}

function extractUserMessage(arg: unknown): string {
  if (typeof arg === "string") return arg;
  if (Array.isArray(arg)) {
    // Walk backward looking for the last role:user (or role-less).
    for (let i = arg.length - 1; i >= 0; i--) {
      const item = arg[i];
      if (typeof item === "string") return item;
      if (item && typeof item === "object") {
        const obj = item as VertexContent;
        if (obj.role && obj.role !== "user") continue;
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

function stringifySystemInstruction(content: unknown): string {
  if (typeof content === "string") return content;
  if (!content || typeof content !== "object") return "";
  const obj = content as VertexContent;
  if (Array.isArray(obj.parts)) {
    const bits: string[] = [];
    for (const p of obj.parts) {
      if (typeof p?.text === "string") bits.push(p.text);
    }
    return bits.join("\n");
  }
  return "";
}

function extractResponseFields(response: VertexResponse): {
  responseText: string;
  inputTokens: number;
  outputTokens: number;
} {
  let responseText = "";
  let inputTokens = 0;
  let outputTokens = 0;
  try {
    const candidates = response?.response?.candidates ?? [];
    const parts = candidates[0]?.content?.parts ?? [];
    const bits: string[] = [];
    for (const p of parts) {
      if (typeof p?.text === "string") bits.push(p.text);
    }
    responseText = bits.join("\n");
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
