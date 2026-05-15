/**
 * Direct-emission helpers for events that don't fit the HOF pattern.
 *
 * `wrap()` and `tool()` wrap functions; `checkpoint()` and
 * `validatorResult()` are markers inserted at points of interest
 * inside agent code, often inside the same function `wrap()` already
 * covers. For those, a plain function call is the right API:
 *
 *     checkpoint("after_retrieval", { documents: 5, used_cache: true });
 *
 *     if (!result) {
 *       validatorResult("non-empty-response", false, {
 *         message: "LLM returned empty content",
 *         severity: "error",
 *       });
 *     }
 *
 * Both helpers no-op silently when called outside an active wrap()
 * execution context — same fail-open pattern as `tool()`.
 */
import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { EventType, utcNowRfc3339 } from "./events.js";
const MAX_VALIDATOR_MSG = 500;
/**
 * Emit a `checkpoint` event marking a notable point in execution.
 *
 * A checkpoint is a free-form marker: a name + arbitrary metadata.
 * Typical uses: "after_retrieval", "before_synthesis", "cache_hit".
 * Useful both for Phase 3+ detector hooks (drift, cost-velocity)
 * and for ad-hoc debugging — replay UI in a future phase will
 * render checkpoints as anchored markers on the execution timeline.
 *
 * Outside `wrap()`: silent no-op.
 */
export function checkpoint(name, metadata = {}) {
    const ctx = currentExecutionContext();
    if (!ctx)
        return;
    const client = getClient();
    const event = {
        event_id: newEventId(),
        execution_id: ctx.executionId,
        event_type: EventType.CHECKPOINT,
        sequence: ctx.nextSequence(),
        timestamp: utcNowRfc3339(),
        payload: { name, metadata },
    };
    client.submitEvent(event);
}
/**
 * Report a validator outcome as a `validator_result` event.
 *
 * Validators are checks the agent (or its framework) runs against
 * intermediate or final outputs: schema conformance, factuality,
 * relevance, safety. The result — pass or fail — becomes a discrete
 * event so Phase-3 detection can spot patterns like "validator X
 * has been failing 90% of the time on this model."
 *
 * Outside `wrap()`: silent no-op.
 */
export function validatorResult(name, passed, opts = {}) {
    const ctx = currentExecutionContext();
    if (!ctx)
        return;
    let severity = opts.severity ?? "error";
    if (severity !== "warning" &&
        severity !== "error" &&
        severity !== "critical") {
        // Don't throw — the caller's agent shouldn't fail because of an
        // SDK-side validation. Coerce to the safest default.
        severity = "error";
    }
    const payload = { name, passed, severity };
    if (opts.message) {
        payload["message"] = opts.message.slice(0, MAX_VALIDATOR_MSG);
    }
    const client = getClient();
    const event = {
        event_id: newEventId(),
        execution_id: ctx.executionId,
        event_type: EventType.VALIDATOR_RESULT,
        sequence: ctx.nextSequence(),
        timestamp: utcNowRfc3339(),
        payload,
    };
    client.submitEvent(event);
}
//# sourceMappingURL=observe.js.map