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
 *   - Streaming (stream: true returns AsyncIterable): observed via
 *     wrapStreamingResponse. Chunks pass through to the
 *     customer's `for await` loop unchanged; the llm_call event ships
 *     at stream-end with streaming=true in the payload. (The 2.5.1.a
 *     maybeWarnStreamingUnsupported helper is kept as dead code with
 *     a deprecation note for a future cleanup wave.)
 *   - Embeddings (ollama.embed)
 *   - Generate API (ollama.generate)
 *
 * Dependency injection: instrumentOllama accepts an optional
 * ollamaClass parameter so this code path is testable without
 * installing the actual `ollama` package.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { classifyOllamaException } from "./errors.js";
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

/** — one-time guard for streaming calls. The
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
    // — chunk-aggregating wrapper for streaming calls
    // (replaces the 2.5.1.a no-op). When stream:true, response is
    // an AsyncIterable<OllamaChatChunk>; we wrap it so chunks pass
    // through to the customer while accumulating response_text +
    // token counts, then emit the llm_call event at stream-end.
    const isStream = firstArg.stream === true;

    const start = performance.now();
    try {
      const response = await originalChat.apply(this, args);

      if (isStream) {
        return wrapStreamingResponse({
          inner: response as AsyncIterable<OllamaChatChunk>,
          client,
          executionId: ctx.executionId,
          eventId,
          sequence,
          model,
          systemText,
          userMessage,
          start,
          budgetTracker: ctx.budgetTracker ?? null,
        });
      }

      const durationMs = Math.round(performance.now() - start);
      const { responseText, inputTokens, outputTokens } =
        extractResponseFields(response as OllamaChatResponse);
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
      // — intentional omission of the 
      // _maybeEmitThrottlingEvent auto-emit call. The other four
      // instrumentation modules call it here; instrumentOllama does
      // not because Ollama is a local runtime — no per-minute rate
      // limiting, no quota exhaustion. shipped a
      // regression-guard test asserting classifyOllamaException
      // NEVER returns RATE_LIMITED or QUOTA_EXHAUSTED, so this call
      // would be guaranteed-dead code. instrument_throttling.test.ts
      // contains a paired negative assertion that fails loudly if a
      // future refactor adds the import without removing the guard.
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
      surface: "chat",
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
    surface: "chat",
    model: a.model,
    system_prompt: truncate(a.systemText, MAX_SYSTEM),
    user_message: truncate(a.userMessage, MAX_USER_MSG),
    status: "failed",
    error_class: classifyOllamaException(a.err),
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

// ──────────────────────────────────────────────────────────────────────
// — streaming chunk-aggregation
// ──────────────────────────────────────────────────────────────────────

/** One chunk yielded by the ollama client's streaming generator.
 * Partial text on .message.content for non-final chunks; final
 * chunk carries .prompt_eval_count + .eval_count + .done=true. */
interface OllamaChatChunk {
  model?: string;
  message?: { role?: string; content?: string };
  done?: boolean;
  prompt_eval_count?: number;
  eval_count?: number;
}

interface StreamingWrapArgs {
  inner: AsyncIterable<OllamaChatChunk>;
  client: ReturnType<typeof getClient>;
  executionId: string;
  eventId: string;
  sequence: number;
  model: string;
  systemText: string;
  userMessage: string;
  start: number;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  budgetTracker: any;
}

/** Wraps an Ollama AsyncIterable streaming response so chunks pass
 * through to the customer while we accumulate response_text + tokens
 * for the final llm_call event emission. The returned object is an
 * AsyncIterable (Symbol.asyncIterator) the customer can `for await`
 * over identically to the original response. */
function wrapStreamingResponse(args: StreamingWrapArgs): AsyncIterable<OllamaChatChunk> {
  const state = { textParts: [] as string[], inputTokens: 0, outputTokens: 0 };
  let emitted = false;

  const emitSuccess = () => {
    if (emitted) return;
    emitted = true;
    const durationMs = Math.round(performance.now() - args.start);
    const responseText = state.textParts.join("");
    if (args.budgetTracker) {
      args.budgetTracker.addTokens(state.inputTokens, state.outputTokens);
    }
    args.client.submitEvent({
      event_id: args.eventId,
      execution_id: args.executionId,
      event_type: EventType.LLM_CALL,
      sequence: args.sequence,
      timestamp: utcNowRfc3339(),
      duration_ms: durationMs,
      payload: {
        provider: PROVIDER,
        surface: "chat",
        model: args.model,
        system_prompt: truncate(args.systemText, MAX_SYSTEM),
        user_message: truncate(args.userMessage, MAX_USER_MSG),
        response_text: truncate(responseText, MAX_RESPONSE),
        status: "ok",
        input_tokens: state.inputTokens,
        output_tokens: state.outputTokens,
        streaming: true,
      },
    });
  };

  const emitFailure = (err: unknown) => {
    if (emitted) return;
    emitted = true;
    const durationMs = Math.round(performance.now() - args.start);
    args.client.submitEvent(
      buildFailureEvent({
        eventId: args.eventId,
        executionId: args.executionId,
        sequence: args.sequence,
        durationMs,
        model: args.model,
        systemText: args.systemText,
        userMessage: args.userMessage,
        err,
      }),
    );
  };

  return {
    [Symbol.asyncIterator](): AsyncIterator<OllamaChatChunk> {
      const inner = args.inner[Symbol.asyncIterator]();
      return {
        async next(): Promise<IteratorResult<OllamaChatChunk>> {
          try {
            const result = await inner.next();
            if (result.done) {
              emitSuccess();
              return result;
            }
            accumulateChunk(result.value, state);
            return result;
          } catch (err) {
            emitFailure(err);
            throw err;
          }
        },
      };
    },
  };
}

/** Apply one streaming chunk to the wrapper's state. Defensive: any
 * extraction failure degrades silently so a single malformed chunk
 * does not break stream-level aggregation. */
function accumulateChunk(
  chunk: OllamaChatChunk,
  state: { textParts: string[]; inputTokens: number; outputTokens: number },
): void {
  try {
    const content = chunk?.message?.content;
    if (typeof content === "string" && content.length > 0) {
      state.textParts.push(content);
    }
    if (chunk?.done) {
      state.inputTokens = numberOrZero(chunk.prompt_eval_count);
      state.outputTokens = numberOrZero(chunk.eval_count);
    }
  } catch {
    // Swallow — one bad chunk shouldn't break stream-level aggregation.
  }
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
  /** — exposed for unit tests of the streaming chunk-
   * aggregation logic. Not part of the public API. */
  accumulateChunk,
};
