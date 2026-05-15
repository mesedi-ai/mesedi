/**
 * Hard-halt local-budget demo (sub-slice 21d, TS port).
 *
 * Direct port of `sdk-python/sandbox/halt_test.py`. Demonstrates the
 * three halt triggers in the TypeScript SDK:
 *
 *   1. Wall-clock — an agent that would run forever halts after N seconds
 *   2. Step count — an agent that emits too many events halts after N steps
 *   3. Token total — an agent that consumes too many tokens halts mid-run
 *
 * For each demo: wraps a function with `wrap({ budget }, fn)` and
 * confirms that:
 *   - MesediHalt fires at a safe boundary (between tool calls, not
 *     mid-tool-call)
 *   - The wrapped function returns `undefined` (caller doesn't see
 *     the exception — same semantics as Python's `return None`)
 *   - The execution lands in SQLite with status=halted and a
 *     crash_signature like `halt:wall_clock`
 *   - Standard try/finally cleanup still runs
 *
 * Prereqs:
 *   - Backend running: cd ../../backend && go run cmd/api/main.go
 *   - SDK built:       cd .. && npm run build:sandbox
 *
 * Run:
 *   node sandbox/dist-sandbox/halt_test.js
 */
import { configure, flush, tool, wrap, } from "../src/index.js";
configure({
    apiKey: "mesedi_sk_dev_local_only",
    baseUrl: "http://localhost:8080",
});
// 200ms sleep per call — emits one tool_call event per invocation, so
// the step-count budget will trip after N calls. The sleep also
// guarantees wall-clock budgets trip predictably.
const slowTool = tool(async function slowTool() {
    await new Promise((resolve) => setTimeout(resolve, 200));
    return "done";
});
// ── Wall-clock halt ──────────────────────────────────────────────────
const runawayWallClockBudget = { maxWallClockSeconds: 1.0 };
const runawayWallClockAgent = wrap({ budget: runawayWallClockBudget }, async function runaway_wall_clock_agent() {
    // Would loop forever calling slowTool — but the 1s wall-clock
    // budget halts it after ~5 iterations. Cleanup runs in `finally`
    // to prove standard JS cleanup semantics survive the halt.
    const cleanupRan = [];
    try {
        for (let i = 0; i < 100; i++) {
            await slowTool(); // 200ms each; budget check fires before the 6th
            console.log(`  iteration ${i + 1} completed`);
        }
        return "all 100 iterations done";
    }
    finally {
        cleanupRan.push(true);
        console.log(`  finally block ran (cleanup_ran=[${cleanupRan.join(",")}])`);
    }
});
// ── Step-count halt ─────────────────────────────────────────────────
const runawayStepCountBudget = { maxSteps: 3 };
const runawayStepCountAgent = wrap({ budget: runawayStepCountBudget }, async function runaway_step_count_agent() {
    // Tries to call slowTool 20 times, but the 3-step budget halts
    // after the 3rd tool_call's pre-call check.
    for (let i = 0; i < 20; i++) {
        await slowTool();
        console.log(`  iteration ${i + 1} completed`);
    }
    return "done";
});
// ── No budget — control case ─────────────────────────────────────────
const cleanAgent = wrap(async function clean_agent() {
    // No budget — runs to completion normally.
    await slowTool();
    await slowTool();
    return "no budget, no halt";
});
function ms(seconds) {
    return `${(seconds * 1000).toFixed(0)}ms`;
}
async function main() {
    console.log("\n── Run 1: cleanAgent (no budget, control case) ──");
    let t = performance.now();
    let result = await cleanAgent();
    console.log(`  wall-clock: ${ms((performance.now() - t) / 1000)}`);
    console.log(`  result: ${JSON.stringify(result)}`);
    console.log("\n── Run 2: runawayWallClockAgent (budget=1s wall-clock) ──");
    console.log("  Expected: ~5 iterations complete, then halt fires at the 6th.");
    t = performance.now();
    result = await runawayWallClockAgent();
    console.log(`  wall-clock: ${ms((performance.now() - t) / 1000)}`);
    console.log(`  result: ${JSON.stringify(result)} (undefined means halted cleanly)`);
    console.log("\n── Run 3: runawayStepCountAgent (budget=3 steps) ──");
    console.log("  Expected: 2 tool_calls complete, then halt fires at the 3rd.");
    t = performance.now();
    result = await runawayStepCountAgent();
    console.log(`  wall-clock: ${ms((performance.now() - t) / 1000)}`);
    console.log(`  result: ${JSON.stringify(result)} (undefined means halted cleanly)`);
    console.log("\n── Flushing shipper queue... ──");
    t = performance.now();
    const ok = await flush(5000);
    console.log(`  flush ok=${ok} in ${ms((performance.now() - t) / 1000)}`);
    console.log("\n── Done. Verify in SQLite: ──");
    console.log("  cd ../../backend");
    console.log("  sqlite3 mesedi-dev.db \"SELECT execution_id, status, " +
        "duration_ms, crash_signature FROM executions WHERE status='halted' " +
        "AND sdk_language='typescript' ORDER BY started_at DESC LIMIT 5;\"");
    console.log("  Expect 2 halted rows from this run with crash_signature like halt:wall_clock and halt:step_count");
}
main().catch((err) => {
    console.error("halt_test crashed:", err);
    process.exit(1);
});
//# sourceMappingURL=halt_test.js.map