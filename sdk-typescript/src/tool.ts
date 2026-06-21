/**
 * tool(), wraps an async function as an instrumented agent tool.
 *
 * Mirrors the Python `@mesedi.tool` decorator, but as a higher-order
 * function (TS doesn't have stable decorators yet). When the
 * wrapped function is called from INSIDE a `wrap()`'d execution,
 * each invocation emits a `tool_call` event with:
 *
 *   - tool_name (from options or the function's .name)
 *   - arguments (sanitized + truncated)
 *   - status ("ok" or "failed")
 *   - result_summary (truncated) on success
 *   - exception_type / exception_message on failure
 *   - duration_ms
 *
 * Outside a wrap() context: runs unobserved (same fail-open as Python).
 * Exceptions propagate to the caller unchanged.
 */

import { getClient } from "./client.js";
import {
  currentExecutionContext,
  newEventId,
} from "./context.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";

const MAX_ARG_REPR = 200;
const MAX_RESULT_REPR = 500;
const MAX_EXC_MSG = 500;
/** Cap for the JSON-serialized return_value field. Larger than
 * MAX_RESULT_REPR because the detector fingerprints structure,
 * not value text — preserving structure is more valuable than a
 * tight cap. 2048 chars covers typical tool responses (objects
 * with a dozen keys) while still bounding pathological returns. */
const MAX_RETURN_VALUE_JSON = 2048;

export interface ToolOptions {
  /** Override the tool name (defaults to fn.name). */
  name?: string;
}

/**
 * Decorate an async function as an observable agent tool.
 */
export function tool<TArgs extends unknown[], TResult>(
  fnOrOpts: ToolOptions | ((...args: TArgs) => Promise<TResult>),
  maybeFn?: (...args: TArgs) => Promise<TResult>,
): (...args: TArgs) => Promise<TResult> {
  let opts: ToolOptions;
  let fn: (...args: TArgs) => Promise<TResult>;
  if (typeof fnOrOpts === "function") {
    opts = {};
    // Cast: `typeof === "function"` narrows to the broad built-in
    // `Function` type rather than to our specific signature.
    fn = fnOrOpts as (...args: TArgs) => Promise<TResult>;
  } else {
    opts = fnOrOpts;
    if (!maybeFn) {
      throw new TypeError(
        "tool() requires a function, pass either tool(fn) or tool(options, fn).",
      );
    }
    fn = maybeFn;
  }
  const toolName = opts.name ?? fn.name ?? "<unknown>";

  return async function inner(...args: TArgs): Promise<TResult> {
    const ctx = currentExecutionContext();
    if (!ctx) {
      // No active execution, run unobserved.
      return fn(...args);
    }

    // Halt-safe boundary: check the budget BEFORE doing any work.
    // If a budget exists and is exceeded, this throws MesediHalt
    // which propagates up to wrap()'s catch block. The user's tool
    // code never runs, guarantees halt fires at the boundary, not
    // mid-tool. wrap() also incrementing steps post-check matches
    // the Python pattern (check, then count).
    ctx.checkBudget();
    if (ctx.budgetTracker) {
      ctx.budgetTracker.incrementSteps();
    }

    const client = getClient();
    const sequence = ctx.nextSequence();
    const eventId = newEventId();
    const argsSummary = summarizeArgs(args);
    const start = performance.now();

    try {
      const result = await fn(...args);
      const durationMs = Math.round(performance.now() - start);
      const payload: Record<string, unknown> = {
        tool_name: toolName,
        arguments: argsSummary,
        status: "ok",
        // Human-readable repr for dashboard display. Kept
        // alongside structured return_value below so the UI can
        // render whatever shape the customer's tool returns.
        result_summary: truncate(safeRepr(result), MAX_RESULT_REPR),
      };
      // Structured JSON-native form for backend detectors
      // (tool_schema_drift fingerprints the return shape). Only
      // present when the result is JSON-serializable AND under
      // the size cap; absent otherwise so the detector gracefully
      // no-ops on this call instead of mis-fingerprinting.
      const structured = structuredReturnValue(result);
      if (structured !== undefined) {
        payload.return_value = structured;
      }
      const event: Event = {
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.TOOL_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload,
      };
      client.submitEvent(event);
      return result;
    } catch (err) {
      const durationMs = Math.round(performance.now() - start);
      const event: Event = {
        event_id: eventId,
        execution_id: ctx.executionId,
        event_type: EventType.TOOL_CALL,
        sequence,
        timestamp: utcNowRfc3339(),
        duration_ms: durationMs,
        payload: {
          tool_name: toolName,
          arguments: argsSummary,
          status: "failed",
          exception_type:
            err instanceof Error && err.constructor.name
              ? err.constructor.name
              : typeof err,
          exception_message: truncate(
            err instanceof Error ? err.message : String(err),
            MAX_EXC_MSG,
          ),
        },
      };
      client.submitEvent(event);
      throw err;
    }
  };
}

function summarizeArgs(args: unknown[]): Record<string, unknown> {
  return {
    args: args.map((a) => truncate(safeRepr(a), MAX_ARG_REPR)),
  };
}

/**
 * Returns ``result`` as a JSON-native value (string / number / boolean
 * / null / array / object) suitable for embedding in the
 * ``return_value`` payload field, or ``undefined`` when the result
 * cannot be safely serialized.
 *
 * The backend's tool_schema_drift detector fingerprints the
 * structure (sorted keys + value types) from this field. We need
 * valid JSON structure, NOT a repr string.
 *
 * Implementation mirrors the Python `_structured_return_value`:
 *   1. Serialize with a replacer that coerces unserializable values
 *      (BigInt, functions, undefined, symbols) to strings rather
 *      than dropping them silently.
 *   2. Check the serialized size; if it exceeds the cap, return
 *      the sentinel "<truncated>" so the detector treats this call
 *      as non-comparable instead of mis-fingerprinting a partial
 *      structure.
 *   3. Round-trip through JSON.parse so the returned value is
 *      guaranteed JSON-native (the shipper later JSON.stringifies
 *      the whole payload with no replacer and would silently drop
 *      raw BigInt / undefined values).
 */
function structuredReturnValue(result: unknown): unknown {
  let encoded: string;
  try {
    encoded = JSON.stringify(result, (_k, v) => {
      if (typeof v === "bigint") return v.toString();
      if (typeof v === "function") return `<function:${v.name || "anonymous"}>`;
      if (typeof v === "symbol") return v.toString();
      if (typeof v === "undefined") return "<undefined>";
      return v;
    });
  } catch {
    // Circular reference or other JSON.stringify failure.
    return undefined;
  }
  // JSON.stringify can legitimately return undefined for inputs
  // like a top-level function or a top-level undefined; treat as
  // unserializable rather than crashing the payload.
  if (encoded === undefined) return undefined;
  if (encoded.length > MAX_RETURN_VALUE_JSON) {
    return "<truncated>";
  }
  try {
    return JSON.parse(encoded);
  } catch {
    return undefined;
  }
}

/**
 * JSON.stringify-with-fallback. Falls back to `String(x)` for values
 * that JSON can't serialize (circular refs, BigInt, functions).
 */
function safeRepr(value: unknown): string {
  if (value === undefined) return "undefined";
  if (value === null) return "null";
  if (typeof value === "string") return JSON.stringify(value);
  try {
    return JSON.stringify(value);
  } catch {
    return String(value);
  }
}

function truncate(s: string, max: number): string {
  if (s.length <= max) return s;
  return s.slice(0, max - 3) + "...";
}
