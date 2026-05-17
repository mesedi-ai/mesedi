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
import {
  newExecutionId,
  runInExecutionContext,
} from "./context.js";
import {
  Execution,
  Status,
  utcNowRfc3339,
} from "./events.js";
import { Budget, BudgetTracker, isMesediHalt } from "./halt.js";
import { HaltStreamReader } from "./halt_stream.js";

/** Options accepted by `wrap()`. All optional. */
export interface WrapOptions {
  /**
   * Optional human-readable name. Not yet rendered on the dashboard
   * but reserved for the future replay-UI; helps users distinguish
   * "this execution was the ticket handler" from "this execution
   * was the summarizer."
   */
  name?: string;
  /**
   * Optional per-execution budget. When set, the wrapped function
   * runs with halt-safe boundary checks at every `tool()` /
   * `checkpoint()` / Anthropic LLM-call entry point. If any limit is
   * exceeded between calls, a MesediHalt is thrown internally —
   * wrap() catches it, marks the execution status=halted (with
   * crash_signature=`halt:<trigger>`), and returns undefined.
   *
   * The wrapped function's normal try/finally cleanup runs as
   * expected — halt is raised AT a safe boundary, never mid-tool /
   * mid-LLM-call, so user resources release cleanly.
   */
  budget?: Budget;
}

/**
 * Wrap an async function so each call is recorded as an agent
 * execution by Mesedi.
 */
export function wrap<TArgs extends unknown[], TResult>(
  fnOrOpts: WrapOptions | ((...args: TArgs) => Promise<TResult>),
  maybeFn?: (...args: TArgs) => Promise<TResult>,
): (...args: TArgs) => Promise<TResult | undefined> {
  // Support both call shapes:
  //   wrap(fn) — no options
  //   wrap({...opts}, fn) — with options
  let opts: WrapOptions;
  let fn: (...args: TArgs) => Promise<TResult>;
  if (typeof fnOrOpts === "function") {
    opts = {};
    // Cast: `typeof === "function"` narrows to the broad built-in
    // `Function` type rather than to our specific signature, but at
    // runtime it IS our signature — the call site guarantees that
    // shape since the function position is statically typed.
    fn = fnOrOpts as (...args: TArgs) => Promise<TResult>;
  } else {
    opts = fnOrOpts;
    if (!maybeFn) {
      throw new TypeError(
        "wrap() requires a function — pass either wrap(fn) or wrap(options, fn).",
      );
    }
    fn = maybeFn;
  }
  // `opts.name` is reserved for future use; touch it so the noUnusedLocals
  // compiler check doesn't complain in this slice.
  void opts.name;

  return async function inner(...args: TArgs): Promise<TResult | undefined> {
    const client = getClient();
    const executionId = newExecutionId();
    const execution: Execution = {
      execution_id: executionId,
      status: Status.STARTED,
      started_at: utcNowRfc3339(),
      sdk_language: "typescript",
      sdk_version: "0.0.4",
    };

    // Construct a budget tracker iff a budget was supplied. Stays
    // undefined for un-budgeted wraps — checkBudget() then no-ops.
    const tracker = opts.budget ? new BudgetTracker(opts.budget) : undefined;

    // Sub-slice 21e: if a budget is configured, spawn an SSE reader
    // subscribed to /executions/{id}/halt-stream. When the backend
    // publishes a halt (e.g. via the dashboard's operator Halt
    // button), the reader calls tracker.signalRemoteHalt(reason);
    // the next halt-safe boundary check in tool()/checkpoint()/the
    // Anthropic patch then throws MesediHalt(trigger='remote_signal').
    // Fail-open: subscription failures log at debug and the agent
    // continues with local-budget-only operation.
    let haltReader: HaltStreamReader | undefined;
    if (tracker !== undefined) {
      haltReader = new HaltStreamReader({
        executionId,
        baseUrl: client.baseUrl,
        apiKey: client.apiKey,
        onHalt: (reason: string) => tracker.signalRemoteHalt(reason),
      });
      haltReader.start();
    }

    client.submitExecutionStart(execution);
    const startWall = performance.now();

    try {
      const result = await runInExecutionContext(
        executionId,
        () => fn(...args),
        tracker,
      );
      execution.status = Status.COMPLETED;
      execution.duration_ms = Math.round(performance.now() - startWall);
      execution.ended_at = utcNowRfc3339();
      client.submitExecutionEnd(execution);
      return result;
    } catch (err) {
      execution.duration_ms = Math.round(performance.now() - startWall);
      execution.ended_at = utcNowRfc3339();

      if (isMesediHalt(err)) {
        // Controlled halt — record as halted, NOT crashed. The
        // crash_signature carries the trigger so the dashboard can
        // group halts by cause (`halt:wall_clock` etc.) — same wire
        // format the Python SDK emits.
        execution.status = Status.HALTED;
        // err is narrowed to MesediHalt here by isMesediHalt's type
        // guard, so the trigger access is type-safe.
        execution.crash_signature = `halt:${err.trigger}`;
        client.submitExecutionEnd(execution);
        // Return undefined — DON'T re-throw. The halt is a
        // controlled stop; the caller's downstream code should NOT
        // see an exception. This matches Python's `return None`
        // behavior in wrap.py.
        return undefined;
      }

      execution.status = Status.CRASHED;
      execution.crash_signature = crashSignature(err);
      client.submitExecutionEnd(execution);
      throw err; // re-throw real crashes with original stack
    } finally {
      // Stop the halt-stream reader on every exit path so the
      // in-flight fetch aborts and the background task exits. Safe
      // to call multiple times.
      haltReader?.stop();
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
function crashSignature(err: unknown): string {
  const name =
    err instanceof Error && err.constructor.name
      ? err.constructor.name
      : typeof err;
  const stack = err instanceof Error && err.stack ? err.stack : String(err);
  const top5 = stack.split("\n").slice(0, 5).join("\n");
  const input = `${name}\n${top5}`;
  return createHash("sha256").update(input, "utf8").digest("hex").slice(0, 16);
}
