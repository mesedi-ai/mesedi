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
import { currentExecutionContext, newEventId, } from "./context.js";
import { EventType, utcNowRfc3339 } from "./events.js";
const MAX_ARG_REPR = 200;
const MAX_RESULT_REPR = 500;
const MAX_EXC_MSG = 500;
/**
 * Decorate an async function as an observable agent tool.
 */
export function tool(fnOrOpts, maybeFn) {
    let opts;
    let fn;
    if (typeof fnOrOpts === "function") {
        opts = {};
        // Cast: `typeof === "function"` narrows to the broad built-in
        // `Function` type rather than to our specific signature.
        fn = fnOrOpts;
    }
    else {
        opts = fnOrOpts;
        if (!maybeFn) {
            throw new TypeError("tool() requires a function, pass either tool(fn) or tool(options, fn).");
        }
        fn = maybeFn;
    }
    const toolName = opts.name ?? fn.name ?? "<unknown>";
    return async function inner(...args) {
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
            const event = {
                event_id: eventId,
                execution_id: ctx.executionId,
                event_type: EventType.TOOL_CALL,
                sequence,
                timestamp: utcNowRfc3339(),
                duration_ms: durationMs,
                payload: {
                    tool_name: toolName,
                    arguments: argsSummary,
                    status: "ok",
                    result_summary: truncate(safeRepr(result), MAX_RESULT_REPR),
                },
            };
            client.submitEvent(event);
            return result;
        }
        catch (err) {
            const durationMs = Math.round(performance.now() - start);
            const event = {
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
                    exception_type: err instanceof Error && err.constructor.name
                        ? err.constructor.name
                        : typeof err,
                    exception_message: truncate(err instanceof Error ? err.message : String(err), MAX_EXC_MSG),
                },
            };
            client.submitEvent(event);
            throw err;
        }
    };
}
function summarizeArgs(args) {
    return {
        args: args.map((a) => truncate(safeRepr(a), MAX_ARG_REPR)),
    };
}
/**
 * JSON.stringify-with-fallback. Falls back to `String(x)` for values
 * that JSON can't serialize (circular refs, BigInt, functions).
 */
function safeRepr(value) {
    if (value === undefined)
        return "undefined";
    if (value === null)
        return "null";
    if (typeof value === "string")
        return JSON.stringify(value);
    try {
        return JSON.stringify(value);
    }
    catch {
        return String(value);
    }
}
function truncate(s, max) {
    if (s.length <= max)
        return s;
    return s.slice(0, max - 3) + "...";
}
//# sourceMappingURL=tool.js.map