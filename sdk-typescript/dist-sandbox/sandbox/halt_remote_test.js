/**
 * Remote-halt demo, sub-slice 21e (TS port of halt_remote_test.py).
 *
 * Demonstrates the SSE remote-halt channel end-to-end inside a single
 * Node process:
 *
 *   1. wrap({ budget }, fn) enters, spawning a background SSE reader
 *      subscribed to /executions/{id}/halt-stream on the backend.
 *   2. Inside the wrapped function, we schedule a setTimeout that
 *      fires after 2 seconds and POSTs to /halt for our own
 *      execution_id. Stands in for the dashboard / a detector
 *      triggering the halt remotely.
 *   3. The reader receives the SSE event and calls
 *      tracker.signalRemoteHalt(reason).
 *   4. The next halt-safe boundary check inside the agent, at a
 *      tool() entry, throws MesediHalt with trigger='remote_signal'.
 *   5. wrap()'s catch block converts the halt to status=halted with
 *      crash_signature='halt:remote_signal' and returns undefined.
 *
 * Cross-language wire-format parity check: the resulting SQLite row
 * should have crash_signature='halt:remote_signal' identical to what
 * the Python halt_remote_test.py produces. Both rows aggregate into
 * the SAME failure_group on the backend (deterministic group_id).
 *
 * Prereqs:
 *   - Backend running with sub-slice 21b.1 deployed.
 *   - SDK built: cd .. && npm run build:sandbox
 *
 * Run:
 *   node dist-sandbox/sandbox/halt_remote_test.js
 */
import { configure, flush, tool, wrap, } from "../src/index.js";
const BASE_URL = "http://localhost:8080";
const API_KEY = "mesedi_sk_dev_local_only";
configure({ apiKey: API_KEY, baseUrl: BASE_URL });
const slowTool = tool(async function slowTool() {
    await new Promise((resolve) => setTimeout(resolve, 300));
    return "done";
});
async function triggerHaltAfter(executionId, delayMs) {
    await new Promise((resolve) => setTimeout(resolve, delayMs));
    try {
        const r = await fetch(`${BASE_URL}/executions/${encodeURIComponent(executionId)}/halt`, {
            method: "POST",
            headers: {
                Authorization: `Bearer ${API_KEY}`,
                "X-Mesedi-Schema-Version": "1",
                "Content-Type": "application/json",
            },
            body: JSON.stringify({ reason: "remote halt from halt_remote_test.ts" }),
        });
        const body = await r.text();
        console.log(`  [trigger] POST /halt → status=${r.status} body=${body}`);
    }
    catch (err) {
        console.log(`  [trigger] POST /halt failed: ${String(err)}`);
    }
}
// Inline import of the context primitive so the agent can grab its
// own execution_id at runtime. The TS SDK doesn't currently export
// currentExecutionContext, read from internal module path.
import { currentExecutionContext } from "../src/context.js";
const budget = { maxWallClockSeconds: 60.0 };
const runawayRemoteHaltAgent = wrap({ budget }, async function runaway_remote_halt_agent() {
    const ctx = currentExecutionContext();
    if (!ctx)
        throw new Error("must be called inside wrap()");
    console.log(`  [agent] started, execution_id=${ctx.executionId}`);
    // Schedule the halt 2 seconds from now. setTimeout handle is
    // ref-counted so it keeps the event loop alive; agent halts
    // before the timer would otherwise fire, but this is defensive.
    // Wrap in a void to make the floating-promise lint happy.
    void triggerHaltAfter(ctx.executionId, 2000);
    const cleanupRan = [];
    try {
        for (let i = 0; i < 100; i++) {
            await slowTool();
            console.log(`  [agent] iteration ${i + 1} completed`);
        }
        return "all 100 iterations done";
    }
    finally {
        cleanupRan.push(true);
        console.log(`\n  [agent] finally block ran (cleanup_ran=[${cleanupRan.join(",")}])`);
    }
});
function ms(seconds) {
    return `${(seconds * 1000).toFixed(0)}ms`;
}
async function main() {
    console.log("\n── Remote-halt demo (TS) ──");
    console.log("  Expected: agent runs ~6-7 iterations, halt arrives via SSE,");
    console.log("            agent halts at next tool() entry, finally runs.");
    console.log("            Result is undefined (controlled stop, no exception).");
    console.log();
    const t = performance.now();
    const result = await runawayRemoteHaltAgent();
    console.log(`\n  wall-clock: ${ms((performance.now() - t) / 1000)}`);
    console.log(`  result: ${JSON.stringify(result)}  (undefined means halted cleanly)`);
    console.log("\n── Flushing shipper queue... ──");
    const flushStart = performance.now();
    const ok = await flush(5000);
    console.log(`  flush ok=${ok} in ${ms((performance.now() - flushStart) / 1000)}`);
    console.log("\n── Done. Verify cross-language parity in SQLite: ──");
    console.log("  cd ../../backend");
    console.log("  sqlite3 mesedi-dev.db \"SELECT execution_id, status, " +
        "sdk_language, crash_signature FROM executions WHERE " +
        "crash_signature='halt:remote_signal' ORDER BY started_at DESC LIMIT 5;\"");
    console.log("  Expect at least one row with sdk_language='typescript' AND " +
        "crash_signature='halt:remote_signal', proving the TS halt landed " +
        "with the same wire format as Python.");
}
main().catch((err) => {
    console.error("halt_remote_test (TS) crashed:", err);
    process.exit(1);
});
//# sourceMappingURL=halt_remote_test.js.map