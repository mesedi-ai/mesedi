/**
 * Parity smoke test: exercises every TS-SDK helper that mirrors the
 * Python SDK, checkpoint(), validatorResult(), instrumentAnthropic().
 *
 * Like sdk-python/sandbox/{observe_test.py, anthropic_test.py,
 * injection_test.py} compressed into a single script.
 *
 * Prereqs:
 *   - Backend running on localhost:8080
 *   - Package built: cd ../ && npm run build:sandbox
 *
 * Run:
 *   npm run test:parity     (added in package.json)
 * or:
 *   node dist-sandbox/sandbox/parity_test.js
 *
 * Verify the new events landed via the dashboard or SQL:
 *   sqlite3 mesedi-dev.db "
 *     SELECT event_type, sequence,
 *            json_extract(payload, '\$.name')      AS name,
 *            json_extract(payload, '\$.passed')    AS passed,
 *            json_extract(payload, '\$.model')     AS model,
 *            json_extract(payload, '\$.status')    AS status
 *     FROM events ORDER BY rowid DESC LIMIT 15;
 *   "
 */
import { checkpoint, configure, flush, instrumentAnthropic, validatorResult, wrap, } from "../src/index.js";
configure({
    apiKey: "mesedi_sk_dev_local_only",
    baseUrl: "http://localhost:8080",
});
// ── Fake Anthropic-shaped types (mirrors Python anthropic_test.py) ──
class FakeMessages {
    async create(params) {
        await new Promise((r) => setTimeout(r, 10));
        const lastUser = params.messages?.find((m) => m.role === "user")?.content ?? "";
        return {
            content: [{ text: "[fake TS response]" }],
            usage: {
                input_tokens: Math.max(1, lastUser.split(/\s+/).length),
                output_tokens: 20,
            },
        };
    }
}
class FakeAnthropicClient {
    messages = new FakeMessages();
}
// Inject FakeMessages so instrumentAnthropic patches our test class.
// instrumentAnthropic expects a class-like with prototype.create, so
// wrap FakeMessages accordingly.
await instrumentAnthropic({ prototype: FakeMessages.prototype });
// ── Agents that exercise each helper ────────────────────────────────
const agentWithObservations = wrap({ name: "agent_with_observations" }, async (query) => {
    checkpoint("started", { input_length: query.length });
    // Pretend retrieval
    await new Promise((r) => setTimeout(r, 5));
    const results = ["doc-1", "doc-2", "doc-3"];
    checkpoint("after_retrieval", {
        num_results: results.length,
        used_cache: false,
    });
    // Validator: retrieval should be non-empty
    if (results.length === 0) {
        validatorResult("non-empty-retrieval", false, {
            message: "retrieval returned 0 documents",
            severity: "critical",
        });
        return "no results";
    }
    validatorResult("non-empty-retrieval", true);
    // Pretend an LLM call (uses our patched FakeMessages)
    const anthropic = new FakeAnthropicClient();
    const resp = await anthropic.messages.create({
        model: "claude-opus-4-6",
        system: "You are a helpful research assistant.",
        messages: [{ role: "user", content: `summarize: ${query}` }],
    });
    checkpoint("after_synthesis", { answer_length: resp.content[0].text.length });
    return resp.content[0].text;
});
const agentThatFailsValidator = wrap({ name: "agent_that_fails_validator" }, async () => {
    const answer = ""; // intentionally empty
    validatorResult("non-empty-response", false, {
        message: "LLM returned empty content",
        severity: "error",
    });
    return answer;
});
function ms(t) {
    return `${t.toFixed(1)}ms`;
}
async function main() {
    console.log("\n── Run 1: agent_with_observations (3 checkpoints + 1 validator + 1 LLM call) ──");
    let t = performance.now();
    const result = await agentWithObservations("pickleball clubs in miami");
    console.log(`  wall-clock: ${ms(performance.now() - t)}`);
    console.log(`  result: ${result}`);
    console.log("\n── Run 2: agent_that_fails_validator (1 failing validator) ──");
    t = performance.now();
    await agentThatFailsValidator();
    console.log(`  wall-clock: ${ms(performance.now() - t)}`);
    console.log("\n── Run 3: helpers outside wrap() should no-op silently ──");
    checkpoint("loose_checkpoint", { note: "fired outside any wrap()" });
    validatorResult("loose_validator", true);
    console.log("  both calls returned cleanly with no event recorded");
    console.log("\n── Flushing shipper queue... ──");
    t = performance.now();
    const ok = await flush(5_000);
    console.log(`  flush ok=${ok} in ${ms(performance.now() - t)}`);
    console.log("\n── Done. Expected new events: ──");
    console.log("  3 checkpoint events (started, after_retrieval, after_synthesis)");
    console.log("  2 validator_result events (non-empty-retrieval=ok, non-empty-response=failed)");
    console.log("  1 llm_call event (model=claude-opus-4-6 from FakeMessages)");
    console.log("  validator-failures detector should also create a new failure_group");
    console.log("  for signature=non_empty_response.");
}
main().catch((err) => {
    console.error("parity_test failed:", err);
    process.exit(1);
});
//# sourceMappingURL=parity_test.js.map