/**
 * Cohere TypeScript SDK monkey-patch — auto-emit llm_call events
 * for every chat call inside a wrap()'d execution.
 *
 * Mirrors instrumentAnthropic / instrumentOpenAI in fail-open
 * semantics, canonical error vocabulary, idempotency. Patches:
 *
 *   - cohere-ai CohereClient.chat (v1 surface, `message=` shape)
 *   - cohere-ai CohereClientV2.chat (v7+ OpenAI-style `messages=`)
 *
 * Both write the same canonical llm_call payload
 * (provider="cohere") so the backend doesn't need to know which
 * API surface fired.
 *
 * Dependency injection: instrumentCohere accepts optional
 * v1Class + v2Class arguments so this code path is testable
 * without installing cohere-ai.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import {
  classifyCohereException,
  extractHttpStatus,
  extractRetryAfter,
} from "./errors.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";
import { _maybeEmitThrottlingEvent } from "./observe.js";

const MAX_SYSTEM = 1000;
const MAX_USER_MSG = 1000;
const MAX_RESPONSE = 1000;
const MAX_EXC_MSG = 500;

const PROVIDER = "cohere";

const _patched = new WeakSet<object>();

export interface CohereClientLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  prototype: { chat: (...args: any[]) => Promise<any> };
}

type ChatFn = (this: unknown, ...args: unknown[]) => Promise<unknown>;

interface ChatMessage {
  role?: string;
  content?: string | Array<{ type?: string; text?: string }>;
}

/** Patch the Cohere SDK chat surfaces (v1 + v2). Returns true if at
 * least one was patched. False if neither could be located AND the
 * cohere-ai package isn't installed. */
export async function instrumentCohere(
  v1Class?: CohereClientLike,
  v2Class?: CohereClientLike,
): Promise<boolean> {
  let patchedAny = false;
  let c1 = v1Class;
  let c2 = v2Class;

  if (!c1 || !c2) {
    try {
      // eslint-disable-next-line @typescript-eslint/ban-ts-comment
      // @ts-ignore — package may not be installed; by design.
      const mod = (await import("cohere-ai")) as unknown as {
        CohereClient?: CohereClientLike;
        CohereClientV2?: CohereClientLike;
      };
      if (!c1) c1 = mod?.CohereClient;
      if (!c2) c2 = mod?.CohereClientV2;
    } catch {
      console.warn(
        "mesedi: cohere-ai not installed; instrumentCohere() is a no-op. " +
          "Install with `npm install cohere-ai` to enable.",
      );
      return false;
    }
  }

  if (c1) {
    patchV1(c1);
    patchedAny = true;
  }
  if (c2) {
    patchV2(c2);
    patchedAny = true;
  }
  return patchedAny;
}

function patchV1(cls: CohereClientLike): void {
  if (_patched.has(cls)) return;
  const originalChat = cls.prototype.chat as ChatFn;

  cls.prototype.chat = async function patchedChat(
    this: unknown,
    ...args: unknown[]
  ): Promise<unknown> {
    const ctx = currentExecutionContext();
    if (!ctx) return originalChat.apply(this, args);
    ctx.checkBudget();
    if (ctx.budgetTracker) ctx.budgetTracker.incrementSteps();

    const client = getClient();
    const sequence = ctx.nextSequence();
    const eventId = newEventId();

    const firstArg = (args[0] ?? {}) as {
      model?: string;
      message?: string;
      preamble?: string;
    };
    const model = firstArg.model ?? "unknown";
    const userMessage = typeof firstArg.message === "string" ? firstArg.message : "";
    const systemText = typeof firstArg.preamble === "string" ? firstArg.preamble : "";

    const start = performance.now();
    try {
      const response = (await originalChat.apply(this, args)) as {
        text?: string;
        meta?: { tokens?: { input_tokens?: number; output_tokens?: number } };
      };
      const durationMs = Math.round(performance.now() - start);
      const responseText = typeof response?.text === "string" ? response.text : "";
      const inputTokens = Number(response?.meta?.tokens?.input_tokens ?? 0) || 0;
      const outputTokens = Number(response?.meta?.tokens?.output_tokens ?? 0) || 0;
      if (ctx.budgetTracker) ctx.budgetTracker.addTokens(inputTokens, outputTokens);
      client.submitEvent(
        successEvent({
          eventId,
          executionId: ctx.executionId,
          sequence,
          durationMs,
          model,
          systemText,
          userMessage,
          responseText,
          inputTokens,
          outputTokens,
        }),
      );
      return response;
    } catch (err) {
      const durationMs = Math.round(performance.now() - start);
      client.submitEvent(
        failureEvent({
          eventId,
          executionId: ctx.executionId,
          sequence,
          durationMs,
          model,
          systemText,
          userMessage,
          err,
        }),
      );
      // Wave 1.4: auto-emit infrastructure_event on throttling.
      _maybeEmitThrottlingEvent({
        provider: PROVIDER,
        errorClass: classifyCohereException(err),
        httpStatus: extractHttpStatus(err),
        retryAfterSeconds: extractRetryAfter(err),
        endpoint: "/v1/chat",
      });
      throw err;
    }
  } as ChatFn;
  _patched.add(cls);
}

function patchV2(cls: CohereClientLike): void {
  if (_patched.has(cls)) return;
  const originalChat = cls.prototype.chat as ChatFn;

  cls.prototype.chat = async function patchedChat(
    this: unknown,
    ...args: unknown[]
  ): Promise<unknown> {
    const ctx = currentExecutionContext();
    if (!ctx) return originalChat.apply(this, args);
    ctx.checkBudget();
    if (ctx.budgetTracker) ctx.budgetTracker.incrementSteps();

    const client = getClient();
    const sequence = ctx.nextSequence();
    const eventId = newEventId();

    const firstArg = (args[0] ?? {}) as {
      model?: string;
      messages?: ChatMessage[];
    };
    const model = firstArg.model ?? "unknown";
    const messages = Array.isArray(firstArg.messages) ? firstArg.messages : [];
    const systemText = extractFirstSystemMessage(messages);
    const userMessage = extractLastUserMessage(messages);

    const start = performance.now();
    try {
      const response = (await originalChat.apply(this, args)) as {
        message?: { content?: Array<{ text?: string }> };
        usage?: { tokens?: { input_tokens?: number; output_tokens?: number } };
      };
      const durationMs = Math.round(performance.now() - start);
      const parts: string[] = [];
      for (const block of response?.message?.content ?? []) {
        if (typeof block?.text === "string") parts.push(block.text);
      }
      const responseText = parts.join("\n");
      const inputTokens = Number(response?.usage?.tokens?.input_tokens ?? 0) || 0;
      const outputTokens = Number(response?.usage?.tokens?.output_tokens ?? 0) || 0;
      if (ctx.budgetTracker) ctx.budgetTracker.addTokens(inputTokens, outputTokens);
      client.submitEvent(
        successEvent({
          eventId,
          executionId: ctx.executionId,
          sequence,
          durationMs,
          model,
          systemText,
          userMessage,
          responseText,
          inputTokens,
          outputTokens,
        }),
      );
      return response;
    } catch (err) {
      const durationMs = Math.round(performance.now() - start);
      client.submitEvent(
        failureEvent({
          eventId,
          executionId: ctx.executionId,
          sequence,
          durationMs,
          model,
          systemText,
          userMessage,
          err,
        }),
      );
      // Wave 1.4: auto-emit infrastructure_event on throttling.
      _maybeEmitThrottlingEvent({
        provider: PROVIDER,
        errorClass: classifyCohereException(err),
        httpStatus: extractHttpStatus(err),
        retryAfterSeconds: extractRetryAfter(err),
        endpoint: "/v2/chat",
      });
      throw err;
    }
  } as ChatFn;
  _patched.add(cls);
}

interface EventArgsCommon {
  eventId: string;
  executionId: string;
  sequence: number;
  durationMs: number;
  model: string;
  systemText: string;
  userMessage: string;
}

interface SuccessEventArgs extends EventArgsCommon {
  responseText: string;
  inputTokens: number;
  outputTokens: number;
}

function successEvent(args: SuccessEventArgs): Event {
  return {
    event_id: args.eventId,
    execution_id: args.executionId,
    event_type: EventType.LLM_CALL,
    sequence: args.sequence,
    timestamp: utcNowRfc3339(),
    duration_ms: args.durationMs,
    payload: {
      provider: PROVIDER,
      model: args.model,
      system_prompt: truncate(args.systemText, MAX_SYSTEM),
      user_message: truncate(args.userMessage, MAX_USER_MSG),
      response_text: truncate(args.responseText, MAX_RESPONSE),
      status: "ok",
      input_tokens: args.inputTokens,
      output_tokens: args.outputTokens,
    },
  };
}

interface FailureEventArgs extends EventArgsCommon {
  err: unknown;
}

function failureEvent(args: FailureEventArgs): Event {
  const payload: Record<string, unknown> = {
    provider: PROVIDER,
    model: args.model,
    system_prompt: truncate(args.systemText, MAX_SYSTEM),
    user_message: truncate(args.userMessage, MAX_USER_MSG),
    status: "failed",
    error_class: classifyCohereException(args.err),
    exception_type:
      args.err instanceof Error && args.err.constructor.name
        ? args.err.constructor.name
        : typeof args.err,
    exception_message: truncate(
      args.err instanceof Error ? args.err.message : String(args.err),
      MAX_EXC_MSG,
    ),
  };
  const httpStatus = extractHttpStatus(args.err);
  if (httpStatus !== undefined) payload.http_status = httpStatus;
  const retryAfter = extractRetryAfter(args.err);
  if (retryAfter !== undefined) payload.retry_after_seconds = retryAfter;
  return {
    event_id: args.eventId,
    execution_id: args.executionId,
    event_type: EventType.LLM_CALL,
    sequence: args.sequence,
    timestamp: utcNowRfc3339(),
    duration_ms: args.durationMs,
    payload,
  };
}

function extractFirstSystemMessage(messages: ChatMessage[]): string {
  for (const m of messages) {
    if (m?.role !== "system") continue;
    return contentToString(m.content);
  }
  return "";
}

function extractLastUserMessage(messages: ChatMessage[]): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const m = messages[i];
    if (!m || m.role !== "user") continue;
    return contentToString(m.content);
  }
  return "";
}

function contentToString(content: ChatMessage["content"]): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    const parts: string[] = [];
    for (const b of content) {
      if (b?.type === "text" && typeof b.text === "string") parts.push(b.text);
    }
    return parts.join("\n");
  }
  return "";
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 3) + "...";
}
