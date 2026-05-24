/**
 * Hard-halt primitives, TypeScript port of the Python `mesedi/halt.py`.
 *
 * Three exports:
 *
 *   - `Budget`    , interface describing the per-execution limits
 *                    (wall-clock seconds, step count, input/output
 *                    tokens). Any subset of fields can be set; unset
 *                    fields are "no limit on that axis."
 *
 *   - `MesediHalt`, Error subclass that the runtime throws when a
 *                    `Budget` is exceeded. Carries a `reason` and a
 *                    `trigger` so callers / dashboards can tell which
 *                    axis blew the budget. Marked with an internal
 *                    `Symbol` property so `wrap()` can reliably detect
 *                    a halt-throw even if the user's agent code did
 *                    `try { ... } catch (err) { ... }` and re-threw
 *                    something else.
 *
 *   - `BudgetTracker`, runtime counter struct. One per execution.
 *                    Tracks wall-clock start (via `performance.now()`
 *                   , JS's monotonic clock equivalent of Python's
 *                    `time.monotonic()`), step count, token totals.
 *                    `check()` throws MesediHalt if any limit is
 *                    exceeded; the wrap()'d entry points call it as
 *                    a halt-safe boundary BEFORE doing work, so the
 *                    halt always fires between tool/llm/checkpoint
 *                    calls, never mid-call.
 *
 * Cross-language wire-format parity:
 *
 *   - `crash_signature` on the execution = `halt:<trigger>`, exact
 *     same shape as the Python SDK emits, so TS-halted executions
 *     cluster into the same failure_group as Python-halted ones with
 *     the same trigger.
 *
 *   - `status = "halted"` matches Status.HALTED in events.ts.
 *
 *   - The wrapped function returns `undefined` when halted (no
 *     re-throw), mirroring Python's "return None" behavior. The halt
 *     is recorded as a controlled stop, not as an error the caller
 *     needs to handle.
 *
 * Why a marker Symbol on MesediHalt:
 *
 *   In Python, `MesediHalt(BaseException)` evades `except Exception:`
 *   handlers, broad except blocks miss it. JavaScript has no
 *   equivalent of BaseException; `catch (err)` always catches
 *   everything. So we mark MesediHalt with a hidden Symbol-keyed
 *   property and `wrap()` looks for that symbol, not just an
 *   `instanceof` check. Even if the user's agent does
 *   `try { ... } catch (err) { throw new Error("wrapped: " + err) }`
 *   and accidentally re-wraps the halt as a plain Error, our `wrap()`
 *   can still detect the halt by walking the error chain looking for
 *   the symbol. Belt-and-suspenders.
 */
/**
 * Symbol-keyed marker that identifies a MesediHalt even after
 * unintended re-wrapping. Keep this private to the module so user
 * code can't forge it.
 */
const MESEDI_HALT_MARKER = Symbol.for("mesedi.halt.marker");
/**
 * Error thrown when a Budget is exceeded. Extends Error so standard
 * stack traces work; carries a marker Symbol so `wrap()` can detect a
 * halt even if the user's catch block re-wraps it.
 */
export class MesediHalt extends Error {
    reason;
    trigger;
    // Symbol-keyed marker, invisible to JSON.stringify, hard for user
    // code to spoof.
    [MESEDI_HALT_MARKER] = true;
    constructor(reason, trigger) {
        super(reason);
        this.name = "MesediHalt";
        this.reason = reason;
        this.trigger = trigger;
        // Make the stack point at the throw site, not at the constructor.
        Error.captureStackTrace?.(this, MesediHalt);
    }
}
/**
 * Type guard: was this thrown by Mesedi's halt machinery?
 *
 * Walks the error chain via `.cause` (and a couple of common
 * re-wrap shapes) looking for the symbol marker. This is the function
 * `wrap()` uses to decide "treat as controlled halt → status=halted,
 * return undefined" vs "treat as crash → re-throw, status=crashed."
 */
export function isMesediHalt(err) {
    if (!err || typeof err !== "object")
        return false;
    // Direct marker.
    if (err[MESEDI_HALT_MARKER]) {
        return true;
    }
    // Walk Error.cause chain, ES2022 standard for wrapped errors.
    let cursor = err.cause;
    let safety = 0;
    while (cursor && safety < 10) {
        if (typeof cursor === "object" &&
            cursor[MESEDI_HALT_MARKER]) {
            return true;
        }
        cursor = cursor.cause;
        safety += 1;
    }
    return false;
}
/**
 * Runtime counter struct. One instance per execution, lives on the
 * ExecutionContext. Thread-safety isn't a concern in Node, the
 * single-threaded event loop guarantees serial access, but the
 * counter writes still need to be coherent across async boundaries,
 * which AsyncLocalStorage gives us for free.
 *
 * `check()` is the halt-safe boundary. Callers (tool, anthropic
 * patch, checkpoint) call it FIRST, then increment counters. If any
 * limit is already exceeded at the time of check, a MesediHalt is
 * thrown, the caller's work is never started.
 */
export class BudgetTracker {
    budget;
    startMs;
    steps = 0;
    tokensIn = 0;
    tokensOut = 0;
    // Sub-slice 21e: when the SSE halt-stream reader receives a halt
    // event for this execution, it sets this field via
    // signalRemoteHalt(). The next check() call reads it FIRST (before
    // any budget-axis check) and throws MesediHalt with
    // trigger='remote_signal'. undefined means no remote halt pending.
    remoteHaltReason;
    constructor(budget) {
        this.budget = budget;
        // performance.now() returns a monotonic millisecond timer that
        // doesn't jump backward on NTP adjustments, the JS equivalent
        // of Python's time.monotonic().
        this.startMs = performance.now();
    }
    /**
     * Mark this execution as having a pending remote halt.
     *
     * Called by the SSE halt-stream reader (see halt_stream.ts) when
     * the backend publishes a halt event for this execution. The next
     * `check()` call will throw `MesediHalt(trigger='remote_signal')`
     * with the supplied reason.
     *
     * Idempotent, multiple signals overwrite the reason (last one
     * wins). Concurrency is fine: Node's single-threaded event loop
     * means the reader thread's write and the agent thread's read
     * can't interleave at the JS level.
     */
    signalRemoteHalt(reason) {
        this.remoteHaltReason = reason || "remote halt";
    }
    /**
     * Halt-safe boundary check. Call before doing work; if any budget
     * is already exceeded, this throws MesediHalt and the caller's
     * work never runs.
     *
     * Priority: remote halt FIRST. If the dashboard or a detector
     * explicitly told us to halt, operator intent beats any budget
     * axis. Local budgets only trip when no remote halt is pending.
     */
    check() {
        // Remote halt check, runs even when the budget is unbounded,
        // so a wrap()'d agent without explicit local limits can still
        // be remote-halted (dashboard panic-stop semantics).
        if (this.remoteHaltReason !== undefined) {
            const reason = this.remoteHaltReason;
            // Clear so successive check() calls don't repeat-raise after
            // the first MesediHalt escapes to wrap(). wrap() catches and
            // returns immediately, so this shouldn't matter in practice,
            // but it costs nothing.
            this.remoteHaltReason = undefined;
            throw new MesediHalt(reason, "remote_signal");
        }
        // Wall-clock, checked first among the local axes because it's
        // the most common trigger in practice. A runaway agent burns
        // time before it burns steps or tokens.
        if (this.budget.maxWallClockSeconds !== undefined) {
            const elapsedSec = (performance.now() - this.startMs) / 1000;
            if (elapsedSec >= this.budget.maxWallClockSeconds) {
                throw new MesediHalt(`wall-clock budget exceeded (${elapsedSec.toFixed(2)}s ≥ ${this.budget.maxWallClockSeconds}s)`, "wall_clock");
            }
        }
        if (this.budget.maxSteps !== undefined &&
            this.steps >= this.budget.maxSteps) {
            throw new MesediHalt(`step-count budget exceeded (${this.steps} ≥ ${this.budget.maxSteps})`, "step_count");
        }
        if (this.budget.maxTokensIn !== undefined) {
            if (this.tokensIn >= this.budget.maxTokensIn) {
                throw new MesediHalt(`input-token budget exceeded (${this.tokensIn} ≥ ${this.budget.maxTokensIn})`, "token_total");
            }
        }
        if (this.budget.maxTokensOut !== undefined) {
            if (this.tokensOut >= this.budget.maxTokensOut) {
                throw new MesediHalt(`output-token budget exceeded (${this.tokensOut} ≥ ${this.budget.maxTokensOut})`, "token_total");
            }
        }
    }
    /** Increment the step counter. Call AFTER `check()`. */
    incrementSteps() {
        this.steps += 1;
    }
    /** Add token usage from an LLM-call response. */
    addTokens(tokensIn, tokensOut) {
        if (tokensIn > 0)
            this.tokensIn += tokensIn;
        if (tokensOut > 0)
            this.tokensOut += tokensOut;
    }
    // Read-only accessors for testing / observability.
    get currentSteps() {
        return this.steps;
    }
    get currentTokensIn() {
        return this.tokensIn;
    }
    get currentTokensOut() {
        return this.tokensOut;
    }
    get elapsedMs() {
        return performance.now() - this.startMs;
    }
}
//# sourceMappingURL=halt.js.map