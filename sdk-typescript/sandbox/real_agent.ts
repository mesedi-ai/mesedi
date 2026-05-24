/**
 * End-to-end smoke test for the Mesedi TypeScript SDK.
 *
 * Mirrors sdk-python/sandbox/real_agent.py: a clean run, a crashing
 * run, and a tool-using run, all against the local backend's
 * mesedi_sk_dev_local_only bootstrap key.
 *
 * Prereqs:
 *   - Backend running: cd ../../backend && go run cmd/api/main.go
 *   - This package built: cd ../ && npm run build
 *
 * Run:
 *   node sandbox/real_agent.js   (after build)
 *
 * Verify in SQLite (in another terminal):
 *   cd ../../backend
 *   sqlite3 mesedi-dev.db "SELECT execution_id, status, duration_ms, sdk_language FROM executions ORDER BY started_at DESC LIMIT 5;"
 *
 * Expect: rows with sdk_language = 'typescript'.
 */

import {
  configure,
  flush,
  tool,
  wrap,
} from "../src/index.js";

configure({
  apiKey: "mesedi_sk_dev_local_only",
  baseUrl: "http://localhost:8080",
});

// ── @tool examples ─────────────────────────────────────────────────

const searchWeb = tool({ name: "search_web" }, async (query: string) => {
  await new Promise((r) => setTimeout(r, 10));
  return [`result for ${query} #1`, `result for ${query} #2`, `result for ${query} #3`];
});

const calculator = tool(
  { name: "calculator" },
  async (a: number, b: number, op: "+" | "*" = "+") => {
    if (op === "+") return a + b;
    if (op === "*") return a * b;
    throw new Error(`unsupported op: ${op}`);
  },
);

// ── @wrap examples ─────────────────────────────────────────────────

const agentWithTools = wrap(
  { name: "agent_with_tools" },
  async (query: string) => {
    const results = await searchWeb(query);
    const product = await calculator(results.length, 10, "*");
    return `answer for ${query}: ${product} results scored`;
  },
);

const agentThatCrashes = wrap(
  { name: "agent_that_crashes" },
  async (query: string): Promise<string> => {
    await searchWeb(query);
    throw new Error(`agent gave up on ${query}`);
  },
);

const instantAgent = wrap({ name: "instant_agent" }, async () => {
  return "done";
});

function ms(t: number): string {
  return `${t.toFixed(1)}ms`;
}

async function main(): Promise<void> {
  console.log("\n── Run 1: instant_agent (no work, showing wrap() overhead) ──");
  let t = performance.now();
  await instantAgent();
  console.log(`  wall-clock: ${ms(performance.now() - t)} (target: < 5ms)`);

  console.log("\n── Run 2: agent_with_tools (2 tool calls inside) ──");
  t = performance.now();
  const result = await agentWithTools("pickleball clubs in miami");
  console.log(`  wall-clock: ${ms(performance.now() - t)}`);
  console.log(`  result: ${result}`);

  console.log("\n── Run 3: agent_that_crashes (1 tool call, then crash) ──");
  t = performance.now();
  try {
    await agentThatCrashes("invalid input");
  } catch (err) {
    console.log(`  wall-clock: ${ms(performance.now() - t)}`);
    console.log(`  caught (re-thrown by wrap, as expected): ${(err as Error).message}`);
  }

  console.log("\n── Flushing shipper queue... ──");
  t = performance.now();
  const ok = await flush(5_000);
  console.log(`  flush ok=${ok} in ${ms(performance.now() - t)}`);

  console.log("\n── Done. Verify in SQLite: ──");
  console.log("  cd ../../backend");
  console.log(
    "  sqlite3 mesedi-dev.db \"SELECT execution_id, status, duration_ms, sdk_language FROM executions ORDER BY started_at DESC LIMIT 5;\"",
  );
  console.log("  (rows should have sdk_language = 'typescript')");
}

main().catch((err) => {
  console.error("real_agent failed:", err);
  process.exit(1);
});
