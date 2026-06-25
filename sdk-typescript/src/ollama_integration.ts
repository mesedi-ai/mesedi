/**
 * Ollama SDK monkey-patch — auto-emit llm_call events for every
 * `Ollama.chat()` call inside a wrap()'d execution.
 *
 * Mirrors instrumentOpenAI / instrumentAnthropic shape (same
 * fail-open semantics, same payload contract, same idempotency
 * guarantee), with Ollama-shape adjustments:
 *
 *   - Token field names: prompt_eval_count / eval_count — translated
 *     to canonical input_tokens / output_tokens on payload.
 *   - Response text is `response.message.content` (not `choices[0]...`).
 *   - No API-key auth; customers point at localhost or a remote
 *     Ollama host. No throttling auto-emit in this sub-wave — for a
 *     local runtime there is no upstream rate-limit signal worth
 *     surfacing.
 *
 * Why a dedicated integration when Ollama exposes an OpenAI-
 * compatible endpoint? Customers who chose Ollama for local
 * inference also chose the native `ollama` npm package for its
 * ergonomics. Forcing them to switch to the openai client just to
 * be observable defeats the point.
 *
 * Patches one surface in this sub-wave:
 *   - ollama.Ollama.prototype.chat
 *
 * Out of scope (filed as follow-ups in the Ollama arc):
 *   - Streaming (stream: true returns AsyncIterable). Aggregation
 *     wrapper deferred to a follow-up sub-wave, same way OpenAI
 *     streaming landed in its own #271.i sub-wave. Until that wave
 *     ships, streaming calls **emit no llm_call event at all** (Wave
 *     2.5.1.a correction) — the prior placeholder zero-token event
 *     polluted cost_velocity / token_waste telemetry with misleading
 *     data. The customer's stream still works; only the Mesedi
 *     observation is deferred. A one-time console.info explains why
 *     the streaming calls aren't appearing in the dashboard.
 *   - Embeddings (ollama.embed)
 *   - Generate API (ollama.generate)
 *
 * Dependency injection: instrumentOllama accepts an optional
 * ollamaClass parameter so this code path is testable without
 * installing the actual `ollama` package.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";

const MAX_SYSTEM = 1000;
const MAX_USER_MSG = 1000;
const MAX_RESPONSE = 1000;
const MAX_EXC_MSG = 500;

/** Stable lowercase provider identifier shipped on every llm_call
 * event. Backend's provider_incident detector clusters cross-tenant
 * signals on (provider, error_class) — though for local runtimes the
 * cross-tenant signal is necessarily quiet. */
const PROVIDER = "ollama";

const _patched = new WeakSet<object>();

/** Wave 2.5.1.a — one-time guard for streaming calls. The
 * chunk-aggregating wrapper for streaming responses ships in a
 * follow-up sub-wave; until then, calls with stream: true emit NO
 * llm_call event AND log a single console.info so customers
 * understand why their streaming calls don't appear in the Mesedi
 * dashboard. */
let _streamingWarningEmitted = false;

function maybeWarnStreamingUnsupported(): void {
  if (_streamingWarningEmitted) return;
  _streamingWarningEmitted = true;
  console.info(
    "mesedi: instrumentOllama observed a chat({ stream: true }) call; " +
      "the chunk-aggregating wrapper for streaming responses ships in " +
      "a follow-up sub-wave, so no llm_call event will be recorded for " +
      "streaming calls until then. Non-streaming calls are unaffected. " +
      "Your stream still works; only Mesedi observation is deferred.",
  );
}

export interface OllamaClassLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  prototype: { chat: (...args: any[]) => Promise<any> };
}

type ChatFn = (this: unknown, ...args: unknown[]) => Promise<unknown>;

interface OllamaMessage {
  role?: string;
  content?: string | Array<{ type?: string; text?: string }>;
}
interface OllamaChatArgs {
  model?: string;
  messages?: OllamaMessage[];
  stream?: boolean;
}
interface OllamaChatResponse {
  model?: string;
  message?: { role?: string; content?: string };
  prompt_eval_count?: number;
  eval_count?: number;
  done?: boolean;
}

/**
 * Patch the Ollama SDK's chat method to emit llm_call events.
 * Returns true if the class was patched (or already patched).
 * Returns false if the class wasn't provided AND the ollama
 * package isn't installed.
 */
export async function instrumentOllama(
  ollamaClass?: OllamaClassLike,
): Promise<boolean> {
  let cls = ollamaClass;

  if (!cls) {
    try {
      // Dynamic import so the mesedi SDK has no hard runtime
      // dependency on `ollama`. The class lives at module.Ollama
      // on the default export.
      // eslint-disable-next-line @typescript-eslint/ban-ts-comment
      // @ts-ignore — package may not be installed; this is by design.
      const mod = (await import("ollama")) as unknown as {
        Ollama?: OllamaClassLike;
        default?: { Ollama?: OllamaClassLike };
      };
      cls = mod?.Ollama ?? mod?.default?.Ollama;
    } catch {
      console.warn(
        "mesedi: ollama package not installed; instrumentOllama() is a no-op. " +
          "Install with `npm install ollama` to enable, or pass the Ollama class explicitly.",
      );
      return false;
    }
  }

  if (!cls) return false;
  patchChat(cls);
  return true;
}

function patchChat(cls: OllamaClassLike): void {
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

    const firstArg = (args[0] ?? {}) as OllamaChatArgs;
    const model = firstArg.model ?? "unknown";
    const messages = Array.isArray(firstArg.messages) ? firstArg.messages : [];
    const systemText = extractFirstSystemMessage(messages);
    const userMessage = extractLastUserMessage(messages);
    // Wave 2.5.1.a — streaming calls skip event emission entirely.
    // See module docstring for the rationale.
    const isStream = firstArg.stream === true;

    const start = performance.now();
    try {
      const response = (await originalChat.apply(this, args)) as OllamaChatResponse;

      if (isStream) {
        maybeWarnStreamingUnsupported();
        return response;
      }

      const durationMs = Math.round(performance.now() - start);
      const { responseText, inputTokens, outputTokens } =
        extractResponseFields(response);
      if (ctx.budgetTracker) {
        ctx.budgetTracker.addTokens(inputTokens, outputTokens);
      }
      client.submitEvent(
        buildOkEvent({
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
  } as ChatFn;

  _patched.add(cls);
}

interface OkEventArgs {
  eventId: string;
  executionId: string;
  sequence: number;
  durationMs: number;
  model: string;
  systemText: string;
  userMessage: string;
  responseText: string;
  inputTokens: number;
  outputTokens: number;
}

function buildOkEvent(a: OkEventArgs): Event {
  return {
    event_id: a.eventId,
    execution_id: a.executionId,
    event_type: EventType.LLM_CALL,
    sequence: a.sequence,
    timestamp: utcNowRfc3339(),
    duration_ms: a.durationMs,
    payload: {
      provider: PROVIDER,
      model: a.model,
      system_prompt: truncate(a.systemText, MAX_SYSTEM),
      user_message: truncate(a.userMessage, MAX_USER_MSG),
      response_text: truncate(a.responseText, MAX_RESPONSE),
      status: "ok",
      input_tokens: a.inputTokens,
      output_tokens: a.outputTokens,
    },
  };
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

function buildFailureEvent(a: FailureEventArgs): Event {
  const errObj = a.err as { name?: string; message?: string; status_code?: number };
  const payload: Record<string, unknown> = {
    provider: PROVIDER,
    model: a.model,
    system_prompt: truncate(a.systemText, MAX_SYSTEM),
    user_message: truncate(a.userMessage, MAX_USER_MSG),
    status: "failed",
    // Wave 2.5.2 will ship classifyOllamaException and swap this
    // hardcoded UNKNOWN for the canonical error class.
    error_class: "unknown",
    exception_type: errObj?.name ?? "Error",
    exception_message: truncate(String(errObj?.message ?? a.err), MAX_EXC_MSG),
  };
  if (typeof errObj?.status_code === "number") {
    payload.http_status = errObj.status_code;
  }
  return {
    event_id: a.eventId,
    execution_id: a.executionId,
    event_type: EventType.LLM_CALL,
    sequence: a.sequence,
    timestamp: utcNowRfc3339(),
    duration_ms: a.durationMs,
    payload,
  };
}

function extractFirstSystemMessage(messages: OllamaMessage[]): string {
  for (const msg of messages) {
    if (msg?.role !== "system") continue;
    return contentToText(msg.content);
  }
  return "";
}

function extractLastUserMessage(messages: OllamaMessage[]): string {
  for (let i = messages.length - 1; i >= 0; i--) {
    const msg = messages[i];
    if (msg?.role !== "user") continue;
    return contentToText(msg.content);
  }
  return "";
}

function contentToText(
  content: string | Array<{ type?: string; text?: string }> | undefined,
): string {
  if (typeof content === "string") return content;
  if (Array.isArray(content)) {
    return content
      .map((b) => (typeof b?.text === "string" ? b.text : ""))
      .join("\n");
  }
  return "";
}

function extractResponseFields(response: OllamaChatResponse): {
  responseText: string;
  inputTokens: number;
  outputTokens: number;
} {
  const responseText = response?.message?.content ?? "";
  const inputTokens = numberOrZero(response?.prompt_eval_count);
  const outputTokens = numberOrZero(response?.eval_count);
  return { responseText, inputTokens, outputTokens };
}

function numberOrZero(value: unknown): number {
  if (typeof value === "number" && Number.isFinite(value)) return value;
  if (typeof value === "string") {
    const parsed = Number(value);
    return Number.isFinite(parsed) ? parsed : 0;
  }
  return 0;
}

function truncate(s: string, limit: number): string {
  if (!s) return "";
  if (s.length <= limit) return s;
  return s.slice(0, limit) + "...[truncated]";
}

// Exported helpers for unit tests. Not part of the public API; tests
// import these to verify field translation and message extraction.
export const _testing = {
  PROVIDER,
  extractFirstSystemMessage,
  extractLastUserMessage,
  contentToText,
  extractResponseFields,
  numberOrZero,
  truncate,
  /** Resets the one-time streaming-warning flag so tests can assert
   * the warning fires on first stream call after a reset. Not part
   * of the public API. */
  resetStreamingWarningGuard: (): void => {
    _streamingWarningEmitted = false;
  },
  /** The streaming-warning function itself, exposed so unit tests
   * can verify the once-per-process contract without spinning up a
   * full wrap() + chat() integration scaffold. Not part of the
   * public API. */
  maybeWarnStreamingUnsupported,
};
