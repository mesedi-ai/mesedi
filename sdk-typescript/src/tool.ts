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
/** Cap for the JSON-serialized return_value field on the wire.
 * The SDK uses this as a generous upper bound; the backend applies
 * its OWN per-project cap (default 8 KB) at fingerprint time so
 * customers tune storage / fingerprint policy from the dashboard
 * without an SDK redeploy (see , migration 042).
 *
 * Raised from the v0.5.0 hardcoded 2048 to 16384 (16 KB) so the
 * SDK no longer aggressively truncates returns that the backend
 * would happily fingerprint. */
const MAX_RETURN_VALUE_JSON = 16384;

/**
 * Cap on the tool description sent with each tool_call.
 *
 * Tool descriptions are what the MODEL reads when deciding whether and
 * how to call a tool. Under MCP they come from a third-party server and
 * are not sanitized, which is the attack behind CVE-2026-75130
 * (Context7, published 2026-08-18): a compromised server puts
 * instructions in what looks like help text and the agent follows them.
 *
 * Before this the SDK sent only the return shape, so a poisoned
 * description with an unchanged return shape produced no signal at all.
 * Verified against production 2026-08-27: 50 failure groups before,
 * 50 after, zero fired.
 *
 * 2000 matches the Python SDK. Truncation is marked inline so a hash
 * change caused by truncation cannot be mistaken for a real edit.
 */
const MAX_TOOL_DESCRIPTION = 2000;

export interface ToolOptions {
  /** Override the tool name (defaults to fn.name). */
  name?: string;
  /**
   * The description the model sees for this tool.
   *
   * TypeScript has no docstring equivalent the runtime can read, so
   * unlike the Python SDK (which reads __doc__) this has to be passed
   * explicitly. Pass the same string you give your agent framework as
   * the tool description, otherwise the backend is fingerprinting
   * something the model never saw.
   */
  description?: string;
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
  const toolDescription = truncateDescription(opts.description);

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
      if (toolDescription) {
        payload.tool_description = toolDescription;
      }
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
          ...(toolDescription ? { tool_description: toolDescription } : {}),
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
 * Returns ``result`` as a JSON-native, schema-preserving value
 * suitable for embedding in the ``return_value`` payload field, or
 * ``undefined`` when the result cannot be safely serialized.
 *
 * The backend's tool_schema_drift detector fingerprints the
 * structure (sorted keys + value types) from this field. The
 * implementation used a JSON.stringify replacer that
 * coerced BigInt / Date / function / etc. to plain strings, which
 * silently collapsed ``{created_at: Date}`` and
 * ``{created_at: "string"}`` to an identical fingerprint and
 * masked real schema drift.
 *
 * Fix: type-tagged sentinels matching the Python sibling. A Date
 * becomes ``{"__type__": "datetime", "value": "..."}``; BigInt
 * becomes ``{"__type__": "bigint", ...}``; etc. The structural
 * fingerprint now sees a distinct shape for typed-vs-plain-string
 * fields so the detector catches drift between them.
 *
 * The ``__type__`` tag vocabulary is closed and matches Python's
 * so cross-SDK consumers (e.g. customer running Python tools that
 * call a TypeScript backend) see consistent fingerprints.
 */
export function structuredReturnValue(result: unknown): unknown {
  let walked: unknown;
  try {
    walked = walkForShape(result, new WeakSet<object>());
  } catch {
    // Circular ref or walker failure — omit the field cleanly.
    return undefined;
  }
  let encoded: string | undefined;
  try {
    encoded = JSON.stringify(walked);
  } catch {
    return undefined;
  }
  if (encoded === undefined) return undefined;
  if (encoded.length > MAX_RETURN_VALUE_JSON) return "<truncated>";
  return walked;
}

/** Recursively coerce ``value`` to a JSON-native form that
 * preserves type information via typed sentinels. ``visited``
 * tracks objects on the current path so circular refs throw rather
 * than infinite-loop. */
function walkForShape(value: unknown, visited: WeakSet<object>): unknown {
  // Fast path: JSON-native primitives.
  if (value === null) return null;
  const t = typeof value;
  if (t === "string" || t === "boolean") return value;
  if (t === "number") {
    const n = value as number;
    if (!Number.isFinite(n)) {
      return {
        __type__: "object",
        class: "number",
        value: String(n),
      };
    }
    return n;
  }
  // undefined inside a container needs a typed sentinel so the
  // shape preserves "this field is intentionally undefined" vs
  // "this field is a string." Top-level undefined is handled by
  // the caller (returns undefined-the-payload-absence).
  if (t === "undefined") {
    return { __type__: "undefined" };
  }
  if (t === "bigint") {
    return { __type__: "bigint", value: (value as bigint).toString() };
  }
  if (t === "function") {
    return {
      __type__: "function",
      name: (value as { name?: string }).name || "anonymous",
    };
  }
  if (t === "symbol") {
    return { __type__: "symbol", value: (value as symbol).toString() };
  }
  // Container types: recurse, guarding against cycles.
  if (Array.isArray(value)) {
    if (visited.has(value)) {
      throw new Error("circular array reference");
    }
    visited.add(value);
    try {
      return value.map((v) => walkForShape(v, visited));
    } finally {
      visited.delete(value);
    }
  }
  if (value instanceof Date) {
    return { __type__: "datetime", value: value.toISOString() };
  }
  if (value instanceof Map) {
    return {
      __type__: "map",
      value: Array.from(value.entries()).map(([k, v]) => [
        walkForShape(k, visited),
        walkForShape(v, visited),
      ]),
    };
  }
  if (value instanceof Set) {
    // Sets aren't JSON-native; tag and sort so order-insensitive
    // nature is reflected in the shape.
    const members = Array.from(value).map((v) => walkForShape(v, visited));
    members.sort((a, b) => {
      const sa = JSON.stringify(a) ?? "";
      const sb = JSON.stringify(b) ?? "";
      return sa.localeCompare(sb);
    });
    return { __type__: "set", value: members };
  }
  if (value instanceof RegExp) {
    return { __type__: "regexp", value: value.toString() };
  }
  if (value instanceof Error) {
    return {
      __type__: "object",
      class: value.constructor?.name || "Error",
      value: value.message,
    };
  }
  if (typeof value === "object") {
    if (visited.has(value as object)) {
      throw new Error("circular object reference");
    }
    visited.add(value as object);
    try {
      const obj = value as Record<string, unknown>;
      const out: Record<string, unknown> = {};
      for (const [k, v] of Object.entries(obj)) {
        out[k] = walkForShape(v, visited);
      }
      // Tag the class name when it's not a plain Object — preserves
      // "returns User vs returns AdminUser" drift even when both
      // have the same fields.
      const className = (value as object).constructor?.name;
      if (className && className !== "Object") {
        out.__type__ = "object";
        out.class = className;
      }
      return out;
    } finally {
      visited.delete(value as object);
    }
  }
  // Should be unreachable; defensive fallback.
  return { __type__: "object", class: typeof value, value: String(value) };
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

/**
 * Normalise a tool description for transport.
 *
 * Returns undefined when absent or blank, so the caller omits the
 * field entirely rather than sending "". The backend needs to tell
 * "no description" apart from "description was emptied": those are
 * different events, and only one of them is drift.
 */
function truncateDescription(desc: string | undefined): string | undefined {
  if (typeof desc !== "string") return undefined;
  const trimmed = desc.trim();
  if (!trimmed) return undefined;
  if (trimmed.length > MAX_TOOL_DESCRIPTION) {
    return trimmed.slice(0, MAX_TOOL_DESCRIPTION) + "...[truncated]";
  }
  return trimmed;
}
