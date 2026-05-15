/**
 * wrap() — Mesedi's primary observation primitive for TypeScript.
 *
 * TypeScript doesn't have stable decorators in regular JS, so wrap()
 * is a HIGHER-ORDER FUNCTION rather than a decorator. The shape:
 *
 *     const myAgent = wrap(
 *       { name: "ticket_handler" },     // optional config
 *       async (ticket: Ticket) => {     // the actual agent function
 *         // ... agent logic ...
 *         return result;
 *       },
 *     );
 *
 *     await myAgent(someTicket);
 *
 * Behavior matches the Python `@mesedi.wrap` decorator exactly:
 *   - On entry: enqueue POST /executions (status=started). Returns
 *     immediately; HTTP happens in the background shipper.
 *   - On normal return: enqueue PATCH /executions/{id}
 *     (status=completed, duration_ms, ended_at).
 *   - On rejection: enqueue PATCH /executions/{id}
 *     (status=crashed, crash_signature), then re-throw the original
 *     error with its original stack trace.
 *
 * **Async-by-default**: Submission to the shipper is fire-and-forget;
 * actual HTTP happens in the background. Latency added to the
 * wrapped call is the time to construct an Execution + push a single
 * item onto an in-memory queue. Sub-millisecond.
 *
 * **Fail-open**: Mesedi outages NEVER block the wrapped agent.
 * Observation failures are logged via console.warn but the wrapped
 * function's behavior is preserved unchanged.
 *
 * **Nested wraps**: A wrap()'d function calling another wrap()'d
 * function produces two distinct executions. The inner one becomes
 * "current" while it runs; when it returns, the outer execution's
 * context is automatically restored via AsyncLocalStorage.
 */
import { createHash } from "node:crypto";
import { getClient } from "./client.js";
import { newExecutionId, runInExecutionContext, } from "./context.js";
import { Status, utcNowRfc3339, } from "./events.js";
/**
 * Wrap an async function so each call is recorded as an agent
 * execution by Mesedi.
 */
export function wrap(fnOrOpts, maybeFn) {
    // Support both call shapes:
    //   wrap(fn) — no options
    //   wrap({...opts}, fn) — with options
    let opts;
    let fn;
    if (typeof fnOrOpts === "function") {
        opts = {};
        // Cast: `typeof === "function"` narrows to the broad built-in
        // `Function` type rather than to our specific signature, but at
        // runtime it IS our signature — the call site guarantees that
        // shape since the function position is statically typed.
        fn = fnOrOpts;
    }
    else {
        opts = fnOrOpts;
        if (!maybeFn) {
            throw new TypeError("wrap() requires a function — pass either wrap(fn) or wrap(options, fn).");
        }
        fn = maybeFn;
    }
    // `opts.name` is reserved for future use; touch it so the noUnusedLocals
    // compiler check doesn't complain in this slice.
    void opts.name;
    return async function inner(...args) {
        const client = getClient();
        const executionId = newExecutionId();
        const execution = {
            execution_id: executionId,
            status: Status.STARTED,
            started_at: utcNowRfc3339(),
            sdk_language: "typescript",
            sdk_version: "0.0.1",
        };
        client.submitExecutionStart(execution);
        const startWall = performance.now();
        try {
            const result = await runInExecutionContext(executionId, () => fn(...args));
            execution.status = Status.COMPLETED;
            execution.duration_ms = Math.round(performance.now() - startWall);
            execution.ended_at = utcNowRfc3339();
            client.submitExecutionEnd(execution);
            return result;
        }
        catch (err) {
            execution.status = Status.CRASHED;
            execution.duration_ms = Math.round(performance.now() - startWall);
            execution.ended_at = utcNowRfc3339();
            execution.crash_signature = crashSignature(err);
            client.submitExecutionEnd(execution);
            throw err; // re-throw with original stack
        }
    };
}
/**
 * Stable 16-hex-char signature for grouping identical crashes.
 *
 * Matches the Python SDK formula: SHA-256 of (error class name +
 * the first 5 lines of the formatted error). Truncates to 16 hex
 * chars — collision-resistant at scale, easy to display in
 * dashboards.
 *
 * The backend's Phase-3a crash grouper uses the same input shape, so
 * Python-emitted and TS-emitted crashes with the same root cause
 * cluster into the same failure_group.
 */
function crashSignature(err) {
    const name = err instanceof Error && err.constructor.name
        ? err.constructor.name
        : typeof err;
    const stack = err instanceof Error && err.stack ? err.stack : String(err);
    const top5 = stack.split("\n").slice(0, 5).join("\n");
    const input = `${name}\n${top5}`;
    return createHash("sha256").update(input, "utf8").digest("hex").slice(0, 16);
}
//# sourceMappingURL=wrap.js.map