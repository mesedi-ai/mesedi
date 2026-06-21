/**
 * OpenAI SDK monkey-patch — auto-emit llm_call events for every
 * chat.completions.create() and responses.create() call inside a
 * wrap()'d execution.
 *
 * Mirrors instrumentAnthropic shape (same fail-open semantics, same
 * canonical error vocabulary, same idempotency guarantee), with
 * OpenAI-shape adjustments:
 *
 *   - System prompt lives inside `messages` (role: "system"), not
 *     a separate `system=` parameter like Anthropic.
 *   - Response text is `choices[0].message.content` for chat
 *     completions, OR `output_text` for the Responses API.
 *   - Token names: `prompt_tokens` / `completion_tokens` for chat,
 *     `input_tokens` / `output_tokens` for Responses — both
 *     normalized to the canonical input_tokens/output_tokens on
 *     payload.
 *
 * Patches two surfaces:
 *   - openai.OpenAI.Chat.Completions.prototype.create
 *   - openai.OpenAI.Responses.prototype.create (openai>=4.50)
 *
 * Out of scope for v1 (filed as #271 follow-ups):
 *   - AsyncOpenAI patching gap shared with instrumentAnthropic
 *   - Streaming responses
 *   - Embeddings / image generation / audio endpoints
 *
 * Dependency injection: instrumentOpenAI accepts optional
 * completionsClass + responsesClass arguments so this code path
 * is testable without installing the actual openai package.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import {
  classifyOpenAIException,
  extractHttpStatus,
  extractRetryAfter,
} from "./errors.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";

const MAX_SYSTEM = 1000;
const MAX_USER_MSG = 1000;
const MAX_RESPONSE = 1000;
const MAX_EXC_MSG = 500;

/** Stable lowercase provider identifier. Backend detectors cluster
 * cross-tenant signals on (provider, error_class), so this string
 * must NOT change between SDK versions without a coordinated
 * backend change. */
const PROVIDER = "openai";

const _patched = new WeakSet<object>();

export interface CompletionsClassLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  prototype: { create: (...args: any[]) => Promise<any> };
}
export interface ResponsesClassLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  prototype: { create: (...args: any[]) => Promise<any> };
}

type CreateFn = (this: unknown, ...args: unknown[]) => Promise<unknown>;

interface ChatMessage {
  role?: string;
  content?: string | Array<{ type?: string; text?: string }>;
}
interface ChatCompletionArgs {
  model?: string;
  messages?: ChatMessage[];
}
interface ChatCompletionResponse {
  choices?: Array<{ message?: { content?: string } }>;
  usage?: { prompt_tokens?: number; completion_tokens?: number };
}
interface ResponsesCreateArgs {
  model?: string;
  instructions?: string;
  input?: string | ChatMessage[];
}
interface ResponsesResponse {
  output_text?: string;
  output?: Array<{ content?: Array<{ text?: string }> }>;
  usage?: { input_tokens?: number; output_tokens?: number };
}

/**
 * Patch the OpenAI SDK's chat.completions.create and
 * responses.create to emit llm_call events. Returns true if at
 * least one surface was patched (or already patched on a prior
 * call). Returns false if neither could be located AND the openai
 * package isn't installed.
 */
export async function instrumentOpenAI(
  completionsClass?: CompletionsClassLike,
  responsesClass?: ResponsesClassLike,
): Promise<boolean> {
  let patchedAny = false;

  let completions = completionsClass;
  let responses = responsesClass;

  if (!completions || !responses) {
    try {
      // Dynamic import so the mesedi SDK has no hard runtime
      // dependency on `openai`. Both Completions and Responses live
      // off OpenAI.Chat / OpenAI.Responses on the default export.
      // eslint-disable-next-line @typescript-eslint/ban-ts-comment
      // @ts-ignore — package may not be installed; this is by design.
      const mod = (await import("openai")) as unknown as {
        OpenAI?: {
          Chat?: { Completions?: CompletionsClassLike };
          Responses?: ResponsesClassLike;
        };
      };
      const openai = mod?.OpenAI;
      if (!completions) completions = openai?.Chat?.Completions;
      if (!responses) responses = openai?.Responses;
    } catch {
      console.warn(
        "mesedi: openai package not installed; instrumentOpenAI() is a no-op. " +
          "Install with `npm install openai` to enable, or pass class arguments explicitly.",
      );
      return false;
    }
  }

  if (completions) {
    patchChatCompletions(completions);
    patchedAny = true;
  }
  if (responses) {
    patchResponses(responses);
    patchedAny = true;
  }
  return patchedAny;
}

function patchChatCompletions(cls: CompletionsClassLike): void {
  if (_patched.has(cls)) return;
  const originalCreate = cls.prototype.create as CreateFn;

  cls.prototype.create = async function patchedCreate(
    this: unknown,
    ...args: unknown[]
  ): Promise<unknown> {
    const ctx = currentExecutionContext();
    if (!ctx) return originalCreate.apply(this, args);
    ctx.checkBudget();
    if (ctx.budgetTracker) ctx.budgetTracker.incrementSteps();

    const client = getClient();
    const sequence = ctx.nextSequence();
    const eventId = newEventId();

    const firstArg = (args[0] ?? {}) as ChatCompletionArgs;
    const model = firstArg.model ?? "unknown";
    const messages = Array.isArray(firstArg.messages) ? firstArg.messages : [];
    const systemText = extractFirstSystemMessage(messages);
    const userMessage = extractLastUserMessage(messages);

    const start = performance.now();
    try {
      const response = (await originalCreate.apply(this, args)) as ChatCompletionResponse;
      const durationMs = Math.round(performance.now() - start);
      const { responseText, inputTokens, outputTokens } =
        extractChatResponseFields(response);
      if (ctx.budgetTracker) {
        ctx.budgetTracker.addTokens(inputTokens, outputTokens);
      }
      client.submitEvent({
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.LLM_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload: {
          provider: PROVIDER,
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
      client.submitEvent(
        buildFailureEvent({
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
      throw err;
    }
  } as CreateFn;

  _patched.add(cls);
}

function patchResponses(cls: ResponsesClassLike): void {
  if (_patched.has(cls)) return;
  const originalCreate = cls.prototype.create as CreateFn;

  cls.prototype.create = async function patchedCreate(
    this: unknown,
    ...args: unknown[]
  ): Promise<unknown> {
    const ctx = currentExecutionContext();
    if (!ctx) return originalCreate.apply(this, args);
    ctx.checkBudget();
    if (ctx.budgetTracker) ctx.budgetTracker.incrementSteps();

    const client = getClient();
    const sequence = ctx.nextSequence();
    const eventId = newEventId();

    const firstArg = (args[0] ?? {}) as ResponsesCreateArgs;
    const model = firstArg.model ?? "unknown";
    const instructions = firstArg.instructions ?? "";
    const { userMessage, systemFromInput } = extractResponsesInput(firstArg.input);
    // `instructions` is the canonical system slot; if both are
    // present it wins over a role:system inside the input.
    const systemText = instructions || systemFromInput;

    const start = performance.now();
    try {
      const response = (await originalCreate.apply(this, args)) as ResponsesResponse;
      const durationMs = Math.round(performance.now() - start);
      const { responseText, inputTokens, outputTokens } =
        extractResponsesFields(response);
      if (ctx.budgetTracker) {
        ctx.budgetTracker.addTokens(inputTokens, outputTokens);
      }
      client.submitEvent({
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.LLM_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload: {
          provider: PROVIDER,
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
      client.submitEvent(
        buildFailureEvent({
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
      throw err;
    }
  } as CreateFn;

  _patched.add(cls);
}

interface FailureEventArgs {
  eventId: string;
  executionId: string;
  sequence: number;
  durationMs: number;
  model: string;
  systemText: string;
  userMessage: string;
  err: unknown;
}

function buildFailureEvent(args: FailureEventArgs): Event {
  const failurePayload: Record<string, unknown> = {
    provider: PROVIDER,
    model: args.model,
    system_prompt: truncate(args.systemText, MAX_SYSTEM),
    user_message: truncate(args.userMessage, MAX_USER_MSG),
    status: "failed",
    error_class: classifyOpenAIException(args.err),
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
  if (httpStatus !== undefined) failurePayload.http_status = httpStatus;
  const retryAfter = extractRetryAfter(args.err);
  if (retryAfter !== undefined) failurePayload.retry_after_seconds = retryAfter;
  return {
    event_id: args.eventId,
    execution_id: args.executionId,
    event_type: EventType.LLM_CALL,
    sequence: args.sequence,
    timestamp: utcNowRfc3339(),
    duration_ms: args.durationMs,
    payload: failurePayload,
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

function extractResponsesInput(input: ResponsesCreateArgs["input"]): {
  userMessage: string;
  systemFromInput: string;
} {
  if (typeof input === "string") {
    return { userMessage: input, systemFromInput: "" };
  }
  if (Array.isArray(input)) {
    return {
      userMessage: extractLastUserMessage(input),
      systemFromInput: extractFirstSystemMessage(input),
    };
  }
  return { userMessage: "", systemFromInput: "" };
}

function contentToString(content: ChatMessage["content"]): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    const parts: string[] = [];
    for (const block of content) {
      if (block && block.type === "text" && typeof block.text === "string") {
        parts.push(block.text);
      }
    }
    return parts.join("\n");
  }
  return "";
}

function extractChatResponseFields(response: ChatCompletionResponse): {
  responseText: string;
  inputTokens: number;
  outputTokens: number;
} {
  let responseText = "";
  let inputTokens = 0;
  let outputTokens = 0;
  try {
    const text = response?.choices?.[0]?.message?.content;
    if (typeof text === "string") responseText = text;
  } catch {
    /* best-effort; leave empty */
  }
  try {
    if (response?.usage) {
      inputTokens = Number(response.usage.prompt_tokens ?? 0) || 0;
      outputTokens = Number(response.usage.completion_tokens ?? 0) || 0;
    }
  } catch {
    /* best-effort; leave zero */
  }
  return { responseText, inputTokens, outputTokens };
}

function extractResponsesFields(response: ResponsesResponse): {
  responseText: string;
  inputTokens: number;
  outputTokens: number;
} {
  let responseText = "";
  let inputTokens = 0;
  let outputTokens = 0;
  try {
    if (typeof response?.output_text === "string") {
      responseText = response.output_text;
    } else if (Array.isArray(response?.output)) {
      const parts: string[] = [];
      for (const block of response.output) {
        for (const cb of block?.content ?? []) {
          if (typeof cb?.text === "string") parts.push(cb.text);
        }
      }
      responseText = parts.join("\n");
    }
  } catch {
    /* best-effort */
  }
  try {
    if (response?.usage) {
      inputTokens = Number(response.usage.input_tokens ?? 0) || 0;
      outputTokens = Number(response.usage.output_tokens ?? 0) || 0;
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
