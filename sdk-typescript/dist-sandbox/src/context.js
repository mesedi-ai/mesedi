/**
 * Execution-context tracking using `AsyncLocalStorage` — Node's
 * equivalent of Python's `contextvars`.
 *
 * When `wrap()` is entered, it pushes a fresh `ExecutionContext`
 * onto an AsyncLocalStorage instance. Inside the wrapped function
 * (anywhere in the call tree, including across `await` boundaries),
 * `tool()` and the (future) `checkpoint()` / `validatorResult()`
 * helpers read the context to learn which execution to attach to.
 *
 * Why AsyncLocalStorage: it tracks "logical call context" across
 * promise boundaries automatically — passing context through every
 * await would be invasive and error-prone. AsyncLocalStorage is the
 * Node-native way to do what Python contextvars does.
 */
import { AsyncLocalStorage } from "node:async_hooks";
import { randomUUID } from "node:crypto";
export class ExecutionContext {
    executionId;
    /**
     * Optional budget tracker. Set when `wrap()` was invoked with a
     * `budget` option; absent otherwise. The halt-safe boundary
     * primitives (`tool()`, the Anthropic patch, `checkpoint()`) call
     * `checkBudget()` at entry, which is a no-op when this is undefined.
     */
    budgetTracker;
    sequence = 0;
    constructor(executionId, budgetTracker) {
        this.executionId = executionId;
        this.budgetTracker = budgetTracker;
    }
    nextSequence() {
        this.sequence += 1;
        return this.sequence;
    }
    /**
     * Halt-safe boundary check. Throws MesediHalt if a budget is
     * exceeded; no-op if no budget is configured. Call this at the
     * entry of any code path that's a natural halt boundary (start of
     * a tool call, start of an LLM call, on a checkpoint).
     */
    checkBudget() {
        if (this.budgetTracker) {
            this.budgetTracker.check();
        }
    }
}
const storage = new AsyncLocalStorage();
/**
 * Returns the currently-active execution context, or null if called
 * outside any `wrap()`-decorated function.
 */
export function currentExecutionContext() {
    return storage.getStore() ?? null;
}
/**
 * Runs `fn` inside a fresh execution context. The new context is
 * automatically propagated through awaits inside `fn`. When `fn`
 * resolves (or rejects), the previous context (which may be null)
 * is restored.
 *
 * This is the workhorse `wrap()` calls internally.
 */
export function runInExecutionContext(executionId, fn, budgetTracker) {
    const ctx = new ExecutionContext(executionId, budgetTracker);
    return storage.run(ctx, fn);
}
/**
 * Generate a fresh execution_id matching the Python SDK's format:
 * `exec-<12 hex chars>`. UUID4-derived; the short-prefix variant is
 * easier on the eyes in logs than a full UUID and collision-resistant
 * at any plausible single-tenant scale.
 */
export function newExecutionId() {
    return "exec-" + randomUUID().replace(/-/g, "").slice(0, 12);
}
/**
 * Generate a fresh event_id matching the Python SDK's format:
 * `evt-<12 hex chars>`.
 */
export function newEventId() {
    return "evt-" + randomUUID().replace(/-/g, "").slice(0, 12);
}
//# sourceMappingURL=context.js.map