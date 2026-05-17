/**
 * Vercel AI SDK integration — wrap `generateText` to emit Mesedi
 * telemetry.
 *
 * Usage:
 *
 *     import { wrap } from "mesedi";
 *     import { wrapGenerateText } from "mesedi/integrations/vercel_ai";
 *     import { generateText } from "ai";
 *     import { openai } from "@ai-sdk/openai";
 *
 *     const generateTextM = wrapGenerateText(generateText);
 *
 *     export const runAgent = wrap(
 *       { agentName: "support-triage" },
 *       async (question: string) => {
 *         const result = await generateTextM({
 *           model: openai("gpt-4o"),
 *           prompt: question,
 *           tools: { lookup, search },
 *           maxSteps: 5,
 *         });
 *         return result.text;
 *       },
 *     );
 *
 * Design:
 *
 * `wrapGenerateText` is a higher-order function that takes the real
 * Vercel `generateText` and returns a drop-in replacement with the
 * same signature plus telemetry side effects. Customers don't have
 * to refactor their agent code — they just swap the import.
 *
 * Multi-step (ReAct) generation is the common case for agent
 * workflows: `generateText({ maxSteps: 5, tools: { ... } })` makes
 * up to 5 LLM calls, with tool calls between each. Vercel exposes
 * the per-step record on `result.steps`. We iterate that array and
 * emit one `llm_call` event per step plus one `tool_call` event per
 * tool invocation within the step. Detectors (drift, identical /
 * similar-call loops, tool-failures, cost-velocity,
 * prompt-injection) see the same wire format as a hand-written
 * Mesedi instrumentation produces.
 *
 * Out of scope for this slice:
 *   - `streamText` / `streamObject` — emit-on-stream-end variant
 *     would need to drain the stream first or instrument the
 *     stream's transform. Deferred to a later slice.
 *   - `generateObject` — structured output. The shape is similar to
 *     `generateText`'s but the response field is `.object` not
 *     `.text`. Will compose well with the same wrapper pattern.
 *   - OpenTelemetry-based instrumentation via `experimental_telemetry`.
 *     Heavier integration, requires @opentelemetry/api as a peer
 *     dependency. Vercel's docs note it's experimental, so we use
 *     the wrapper pattern instead for now.
 */

import { getClient } from "../client.js";
import { currentExecutionContext, newEventId } from "../context.js";
import { Event, EventType, utcNowRfc3339 } from "../events.js";

// Truncation budgets — kept in sync with the Python emit_llm_call and
// the TS Anthropic patch so wire-format payloads from this adapter and
// from hand-written code are byte-indistinguishable.
const MAX_SYSTEM = 1000;
const MAX_USER_MSG = 1000;
const MAX_RESPONSE = 1000;
const MAX_TOOL_INPUT_REPR = 200;
const MAX_TOOL_OUTPUT_REPR = 500;
const MAX_EXC_MSG = 500;

/**
 * Loose duck-typed shape for the Vercel `generateText` options. We
 * only read the fields we need; everything else passes through to
 * the underlying function untouched.
 */
interface GenerateTextOptionsLike {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  model?: any;
  prompt?: string;
  system?: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  messages?: any[];
  // Other fields (tools, maxSteps, temperature, etc.) pass through.
  [key: string]: unknown;
}

/**
 * Loose duck-typed shape for the Vercel `generateText` result. We
 * read .text, .usage, .toolCalls, .toolResults, .steps. All fields
 * are optional — older versions, custom providers, or streaming
 * adapters may omit them.
 */
interface GenerateTextResultLike {
  text?: string;
  usage?: {
    promptTokens?: number;
    completionTokens?: number;
    inputTokens?: number;
    outputTokens?: number;
    totalTokens?: number;
  };
  toolCalls?: Array<ToolCallLike>;
  toolResults?: Array<ToolResultLike>;
  steps?: Array<StepLike>;
  finishReason?: string;
}

interface ToolCallLike {
  toolCallId?: string;
  toolName?: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  args?: any;
}

interface ToolResultLike {
  toolCallId?: string;
  toolName?: string;
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  result?: any;
}

interface StepLike {
  text?: string;
  toolCalls?: Array<ToolCallLike>;
  toolResults?: Array<ToolResultLike>;
  usage?: GenerateTextResultLike["usage"];
}

/**
 * Function signature for the Vercel `generateText`. Loose enough to
 * accommodate the (intentionally-broad) typing the Vercel SDK uses
 * and to allow wrapping test doubles.
 */
export type GenerateTextLike = (
  opts: GenerateTextOptionsLike,
) => Promise<GenerateTextResultLike>;

/**
 * Wrap a Vercel `generateText` (or any function with that shape) so
 * each invocation emits Mesedi telemetry. Returns a function with
 * the same signature.
 *
 * Outside a `wrap()` execution, the events silently no-op — same
 * fail-open pattern as every other Mesedi primitive.
 */
export function wrapGenerateText<F extends GenerateTextLike>(
  generateTextFn: F,
): F {
  const wrapped = async (
    options: GenerateTextOptionsLike,
  ): Promise<GenerateTextResultLike> => {
    const startedAt = Date.now();
    const modelId = extractModelId(options.model);
    const userMessage = extractLastUserMessage(options);
    const systemPrompt =
      typeof options.system === "string" ? options.system : "";

    let result: GenerateTextResultLike;
    try {
      result = await generateTextFn(options);
    } catch (err) {
      const durationMs = Date.now() - startedAt;
      emitLlmCallEvent({
        model: modelId,
        userMessage,
        systemPrompt,
        responseText: "",
        inputTokens: 0,
        outputTokens: 0,
        durationMs,
        status: "failed",
      });
      throw err;
    }

    const durationMs = Date.now() - startedAt;
    const ctx = currentExecutionContext();
    if (!ctx) {
      // No active execution — nothing to attach events to. Return
      // the result unchanged.
      return result;
    }

    // If the result has multiple steps (ReAct loop), iterate them
    // and emit one llm_call per step + each step's tool calls.
    // Otherwise treat the whole result as a single step.
    const steps: StepLike[] =
      result.steps && result.steps.length > 0
        ? result.steps
        : [
            {
              text: result.text,
              toolCalls: result.toolCalls,
              toolResults: result.toolResults,
              usage: result.usage,
            },
          ];

    for (let i = 0; i < steps.length; i++) {
      const step = steps[i] as StepLike;
      const isLastStep = i === steps.length - 1;
      const [inputTokens, outputTokens] = extractStepTokens(step);

      emitLlmCallEvent({
        model: modelId,
        userMessage,
        systemPrompt,
        responseText: step.text ?? "",
        inputTokens,
        outputTokens,
        // Only the last step gets the full duration; intermediate
        // steps are attributed to the same total wall-clock for
        // now (Vercel doesn't expose per-step durations).
        durationMs: isLastStep ? durationMs : 0,
        status: "ok",
      });

      const toolCalls = step.toolCalls ?? [];
      const toolResults = step.toolResults ?? [];
      for (const tc of toolCalls) {
        const tr = toolResults.find(
          (r) =>
            r.toolCallId !== undefined && r.toolCallId === tc.toolCallId,
        );
        emitToolCallEvent(tc, tr);
      }
    }

    return result;
  };
  return wrapped as F;
}

/**
 * Inline emit_llm_call helper — direct equivalent of the Python
 * `mesedi.observe.emit_llm_call`. Drift / similar-call /
 * identical-call / cost-velocity / prompt-injection detectors all
 * read from the resulting event payload, so a manually-emitted
 * llm_call event from this adapter is detector-complete the same
 * way an auto-instrumented Anthropic-patch one is.
 */
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

  // Halt-safe boundary — matches @mesedi.tool, the Anthropic patch,
  // and the Python emit_llm_call.
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

/**
 * Emit a tool_call event matching the wire format of `@mesedi.tool`.
 * Tool failures: Vercel exposes the result; if a tool errored, the
 * result entry will typically be undefined OR an object with an
 * `error` field. We detect failure mode duck-typed.
 */
function emitToolCallEvent(
  tc: ToolCallLike,
  tr: ToolResultLike | undefined,
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  ctx.checkBudget();
  if (ctx.budgetTracker) {
    ctx.budgetTracker.incrementSteps();
  }

  const toolName = tc.toolName || "unknown_tool";

  // Vercel tool args are typically a flat object; mimic the
  // @mesedi.tool wire shape ({args: [...], kwargs: {...}}). The
  // structured-args object goes into kwargs; args[] stays empty.
  const kwargs: Record<string, string> = {};
  if (tc.args && typeof tc.args === "object" && !Array.isArray(tc.args)) {
    for (const [k, v] of Object.entries(tc.args as Record<string, unknown>)) {
      kwargs[k] = truncate(reprValue(v), MAX_TOOL_INPUT_REPR);
    }
  } else if (tc.args !== undefined) {
    kwargs["input"] = truncate(reprValue(tc.args), MAX_TOOL_INPUT_REPR);
  }

  const argumentsObj: Record<string, unknown> = {
    args: [],
    kwargs,
  };

  // Determine status from the matched tool result. Vercel surfaces
  // tool errors as either a missing result OR a result with an
  // `error` property; we treat both as failed.
  let status: "ok" | "failed" = "ok";
  let resultSummary = "";
  let exceptionType = "";
  let exceptionMessage = "";

  if (tr === undefined) {
    status = "failed";
    exceptionType = "ToolExecutionError";
    exceptionMessage = "tool did not return a result";
  } else if (
    tr.result &&
    typeof tr.result === "object" &&
    tr.result !== null &&
    "error" in (tr.result as Record<string, unknown>)
  ) {
    status = "failed";
    const errObj = (tr.result as Record<string, unknown>).error;
    exceptionType =
      errObj && typeof errObj === "object" && "name" in errObj
        ? String((errObj as Record<string, unknown>).name)
        : "ToolExecutionError";
    exceptionMessage = truncate(reprValue(errObj), MAX_EXC_MSG);
  } else {
    resultSummary = truncate(reprValue(tr.result), MAX_TOOL_OUTPUT_REPR);
  }

  const payload: Record<string, unknown> = {
    tool_name: toolName,
    arguments: argumentsObj,
    status,
  };
  if (status === "ok") {
    payload["result_summary"] = resultSummary;
  } else {
    payload["exception_type"] = exceptionType;
    payload["exception_message"] = exceptionMessage;
  }

  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.TOOL_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  getClient().submitEvent(event);
}

/**
 * Extract a stable model identifier from a Vercel model object.
 *
 * Vercel provider models expose either `.modelId` (newer) or
 * `.id` (older). For unknown shapes we try toString() — most
 * providers stringify to a human-readable label. Last resort:
 * "unknown".
 */
function extractModelId(model: unknown): string {
  if (typeof model === "string") return model;
  if (model && typeof model === "object") {
    const m = model as Record<string, unknown>;
    if (typeof m["modelId"] === "string") return m["modelId"] as string;
    if (typeof m["id"] === "string") return m["id"] as string;
    if (typeof m["name"] === "string") return m["name"] as string;
    try {
      const s = String(model);
      if (s && s !== "[object Object]") return s;
    } catch {
      // fall through
    }
  }
  return "unknown";
}

/**
 * Pull the last user message out of a Vercel generateText options
 * object. Vercel accepts either a flat `prompt: string` or a
 * `messages: Array<{role, content}>` (chat-style); we handle both.
 */
function extractLastUserMessage(opts: GenerateTextOptionsLike): string {
  if (typeof opts.prompt === "string" && opts.prompt) {
    return opts.prompt;
  }
  if (Array.isArray(opts.messages)) {
    for (let i = opts.messages.length - 1; i >= 0; i--) {
      const msg = opts.messages[i] as Record<string, unknown> | undefined;
      if (!msg) continue;
      if (msg["role"] !== "user") continue;
      const content = msg["content"];
      if (typeof content === "string") return content;
      if (Array.isArray(content)) {
        const parts: string[] = [];
        for (const block of content) {
          if (typeof block === "string") parts.push(block);
          else if (
            block &&
            typeof block === "object" &&
            (block as Record<string, unknown>)["type"] === "text" &&
            typeof (block as Record<string, unknown>)["text"] === "string"
          ) {
            parts.push((block as Record<string, unknown>)["text"] as string);
          }
        }
        return parts.join(" ");
      }
    }
  }
  return "";
}

/**
 * Extract (input_tokens, output_tokens) from a step's usage block.
 * Vercel renamed `promptTokens` → `inputTokens` and
 * `completionTokens` → `outputTokens` between SDK versions; try both.
 */
function extractStepTokens(step: StepLike): [number, number] {
  const usage = step.usage;
  if (!usage) return [0, 0];
  const inTokens =
    (typeof usage.inputTokens === "number" ? usage.inputTokens : 0) ||
    (typeof usage.promptTokens === "number" ? usage.promptTokens : 0) ||
    0;
  const outTokens =
    (typeof usage.outputTokens === "number" ? usage.outputTokens : 0) ||
    (typeof usage.completionTokens === "number" ? usage.completionTokens : 0) ||
    0;
  return [inTokens, outTokens];
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 3) + "...";
}

/**
 * Safe `repr` for arbitrary values. Strings pass through;
 * everything else gets JSON.stringify with a fallback to String().
 */
function reprValue(v: unknown): string {
  if (typeof v === "string") return v;
  if (v === undefined || v === null) return String(v);
  try {
    return JSON.stringify(v);
  } catch {
    try {
      return String(v);
    } catch {
      return "<unrepresentable>";
    }
  }
}
