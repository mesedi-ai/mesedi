/**
 * Anthropic SDK monkey-patch, auto-emit llm_call events for every
 * messages.create() call inside a wrap()'d execution.
 *
 * Activation is opt-in (call instrumentAnthropic() once at startup),
 * matching the Python SDK and the Datadog/Sentry/OpenTelemetry
 * pattern for observability instrumentation.
 *
 * What gets captured per call:
 *   - model (e.g. "claude-opus-4-6")
 *   - system_prompt, truncated to 1000 chars
 *   - user_message, the LAST user-role message, truncated to 1000
 *   - response_text, concatenated text-block content, truncated
 *   - input_tokens / output_tokens, from response.usage
 *   - duration_ms, wall-clock of the API call
 *   - status, "ok" / "failed"
 *   - exception_type + exception_message on failure
 *
 * Dependency injection: instrumentAnthropic accepts an optional
 * `messagesClass` argument so this code path is testable without
 * installing the actual `@anthropic-ai/sdk` package. Production
 * callers pass the real Messages class; tests pass a hand-rolled
 * fake.
 */
import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { EventType, utcNowRfc3339 } from "./events.js";
const MAX_SYSTEM = 1000;
const MAX_USER_MSG = 1000;
const MAX_RESPONSE = 1000;
const MAX_EXC_MSG = 500;
/** Already-patched classes, keyed by class identity to make
 * instrumentAnthropic() idempotent and to allow distinct fake classes
 * to be patched in tests without falsely tripping the check. */
const _patched = new WeakSet();
/**
 * Patch the given Messages class's `create()` method to emit
 * llm_call events. Returns true on success or no-op (already
 * patched). Returns false if no class was supplied and the
 * @anthropic-ai/sdk package isn't installed (best-effort dynamic
 * import, kept optional so the SDK stays dependency-free at install
 * time).
 */
export async function instrumentAnthropic(messagesClass) {
    let cls = messagesClass;
    if (!cls) {
        try {
            // Dynamic import so the SDK doesn't take a hard runtime
            // dependency on @anthropic-ai/sdk. If the user has it
            // installed, we auto-locate Messages; otherwise instrumentation
            // is a no-op.
            // eslint-disable-next-line @typescript-eslint/ban-ts-comment
            // @ts-ignore, the package may not be installed; this is by design
            const mod = (await import("@anthropic-ai/sdk"));
            cls = mod?.Anthropic?.Messages;
        }
        catch {
            console.warn("mesedi: @anthropic-ai/sdk not installed; instrumentAnthropic() is a no-op. " +
                "Install with `npm install @anthropic-ai/sdk` to enable, or pass the Messages class explicitly.");
            return false;
        }
        if (!cls) {
            console.warn("mesedi: located @anthropic-ai/sdk but couldn't find Anthropic.Messages, SDK version mismatch?");
            return false;
        }
    }
    if (_patched.has(cls))
        return true;
    const originalCreate = cls.prototype.create;
    cls.prototype.create = async function patchedCreate(...args) {
        const ctx = currentExecutionContext();
        if (!ctx) {
            // Outside wrap(), pass through unobserved.
            return originalCreate.apply(this, args);
        }
        // Halt-safe boundary: check the budget BEFORE the actual LLM
        // call. If a budget exists and is exceeded, this throws
        // MesediHalt which propagates up to wrap()'s catch block. We
        // count this as a step now (after the check passes) so the
        // counter advances even though the LLM call hasn't run yet , 
        // matches the Python SDK's pattern (check, then count, then act).
        ctx.checkBudget();
        if (ctx.budgetTracker) {
            ctx.budgetTracker.incrementSteps();
        }
        const client = getClient();
        const sequence = ctx.nextSequence();
        const eventId = newEventId();
        const firstArg = (args[0] ?? {});
        const model = firstArg.model ?? "unknown";
        const system = firstArg.system ?? "";
        const messages = Array.isArray(firstArg.messages) ? firstArg.messages : [];
        const systemText = typeof system === "string" ? system : safeStringify(system);
        const userMessage = extractLastUserMessage(messages);
        const start = performance.now();
        try {
            const response = await originalCreate.apply(this, args);
            const durationMs = Math.round(performance.now() - start);
            const { responseText, inputTokens, outputTokens } = extractResponseFields(response);
            // Token-budget accounting: feeds into BudgetTracker so future
            // halt-safe boundary checks know how many tokens this execution
            // has consumed. Tokens from FAILED LLM calls don't get
            // accounted (we don't reach this code path on the error
            // branch), that matches the Python SDK behavior.
            if (ctx.budgetTracker) {
                ctx.budgetTracker.addTokens(inputTokens, outputTokens);
            }
            const event = {
                event_id: eventId,
                execution_id: ctx.executionId,
                event_type: EventType.LLM_CALL,
                sequence,
                timestamp: utcNowRfc3339(),
                duration_ms: durationMs,
                payload: {
                    model,
                    system_prompt: truncate(systemText, MAX_SYSTEM),
                    user_message: truncate(userMessage, MAX_USER_MSG),
                    response_text: truncate(responseText, MAX_RESPONSE),
                    status: "ok",
                    input_tokens: inputTokens,
                    output_tokens: outputTokens,
                },
            };
            client.submitEvent(event);
            return response;
        }
        catch (err) {
            const durationMs = Math.round(performance.now() - start);
            const event = {
                event_id: eventId,
                execution_id: ctx.executionId,
                event_type: EventType.LLM_CALL,
                sequence,
                timestamp: utcNowRfc3339(),
                duration_ms: durationMs,
                payload: {
                    model,
                    system_prompt: truncate(systemText, MAX_SYSTEM),
                    user_message: truncate(userMessage, MAX_USER_MSG),
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
    _patched.add(cls);
    return true;
}
function extractLastUserMessage(messages) {
    if (!messages || messages.length === 0)
        return "";
    // Walk backwards to find the most recent user-role message.
    for (let i = messages.length - 1; i >= 0; i--) {
        const m = messages[i];
        if (!m || m.role !== "user")
            continue;
        const content = m.content;
        if (typeof content === "string")
            return content;
        if (Array.isArray(content)) {
            const parts = [];
            for (const block of content) {
                if (block?.type === "text" && typeof block.text === "string") {
                    parts.push(block.text);
                }
            }
            return parts.join("\n");
        }
        return safeStringify(content);
    }
    return "";
}
function extractResponseFields(response) {
    let responseText = "";
    let inputTokens = 0;
    let outputTokens = 0;
    try {
        if (Array.isArray(response.content)) {
            const parts = [];
            for (const block of response.content) {
                if (block && typeof block.text === "string")
                    parts.push(block.text);
            }
            responseText = parts.join("\n");
        }
    }
    catch {
        // best effort, leave responseText empty
    }
    try {
        if (response.usage) {
            inputTokens = Number(response.usage.input_tokens ?? 0) || 0;
            outputTokens = Number(response.usage.output_tokens ?? 0) || 0;
        }
    }
    catch {
        // best effort, leave token counts at 0
    }
    return { responseText, inputTokens, outputTokens };
}
function safeStringify(v) {
    if (v === undefined)
        return "";
    if (v === null)
        return "";
    if (typeof v === "string")
        return v;
    try {
        return JSON.stringify(v);
    }
    catch {
        return String(v);
    }
}
function truncate(s, max) {
    if (s.length <= max)
        return s;
    return s.slice(0, max - 3) + "...";
}
//# sourceMappingURL=anthropic_integration.js.map