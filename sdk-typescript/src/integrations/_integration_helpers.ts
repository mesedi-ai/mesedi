/**
 * Shared helpers for framework-adapter integration tests
 * (mastra.integration.test.ts, langchain.integration.test.ts).
 *
 * Design mirrors backend/test/integration/conftest.py: same wire
 * protocol against the real Mesedi backend, same fresh-per-session
 * project mint, same `awaitFailureGroup` polling contract.
 *
 * NOT a public SDK surface. Only imported by the .integration.test.ts
 * files in this directory. Kept as a module (rather than duplicated
 * in each test file) so the assertion contract can't drift between
 * adapters.
 */

// ── Env-gates ────────────────────────────────────────────────────────

/**
 * Master gate for the integration test suite. Every test in the
 * .integration.test.ts files starts with a guard:
 *   if (!INTEGRATION_ENABLED) return test.skip(...)
 *
 * Set `RUN_INTEGRATION_TESTS=1` to opt in. Default `npm test` skips
 * everything in this file: unit tests stay fast.
 */
export const INTEGRATION_ENABLED =
  process.env["RUN_INTEGRATION_TESTS"] === "1";

/**
 * Secondary gate for tests that make real LLM calls (Anthropic).
 * These add real cost: ~$0.01-$0.20 per test depending on payload
 * size. Set `ANTHROPIC_API_KEY` in env to unlock. Absent → skip with
 * a clear reason.
 */
export const NEEDS_ANTHROPIC = !!process.env["ANTHROPIC_API_KEY"];

/**
 * Extra gate for the single expensive test (context_overflow ≈ $0.18
 * per run against claude-haiku-4-5's 200K window). Mirrors the Python
 * suite's RUN_EXPENSIVE_TESTS flag (/ ).
 */
export const EXPENSIVE_ENABLED =
  process.env["RUN_EXPENSIVE_TESTS"] === "1";

/**
 * Base URL for the Mesedi backend under test. Defaults to production
 * so a well-configured Mac only has to set `RUN_INTEGRATION_TESTS=1`
 * to run against prod. Override for staging or a local binary via
 * `MESEDI_BASE_URL=http://localhost:8080`.
 */
export const MESEDI_BASE_URL =
  process.env["MESEDI_BASE_URL"] || "https://api.mesedi.ai";

// ── Anthropic-polite spacing ────────────────────────────────────────
//
//  sleepMs(ms) — insert small waits between real-LLM
// calls in the tight-loop tests (drift, cost_velocity, near-dup
// loops, token_waste). Without spacing the harness bursts >50
// requests through Anthropic in a few seconds, trips its TPM cap,
// and the SDK correctly emits `infrastructure_throttled` events —
// which then pollute the failure_group list the OTHER tests poll for.
//
// The right product framing: real customers pace their LLM calls
// naturally (users think between requests, downstream processing
// runs). Modeling that pacing in the harness lets the DOWNSTREAM
// detectors under test see the LLM responses they need. If Anthropic
// still rate-limits us at this pace, infrastructure_throttled still
// fires — we're being polite, not hiding the signal.
export async function sleepMs(ms: number): Promise<void> {
  if (!Number.isFinite(ms) || ms <= 0) return;
  return new Promise((resolve) => setTimeout(resolve, ms));
}

// ── Types + signup ───────────────────────────────────────────────────

export interface TestBackend {
  baseUrl: string;
  apiKey: string;
  projectId: string;
}

/**
 * Mint a fresh Mesedi test project + API key via /signup. Mirrors
 * conftest.py `_signup_test_project`. Each test suite runs in its
 * own project so cross-suite pollution (e.g. rate-window aggregate
 * detectors) is impossible.
 */
export async function signupTestProject(
  baseUrl: string,
): Promise<TestBackend> {
  const email = `inttest+ts+${Date.now()}@example.com`;
  const resp = await fetch(`${baseUrl}/signup`, {
    method: "POST",
    headers: { "Content-Type": "application/json" },
    body: JSON.stringify({ email, project_name: "inttest-ts" }),
  });
  if (resp.status !== 200 && resp.status !== 201) {
    throw new Error(
      `signup failed: status=${resp.status} body=${await resp.text()}`,
    );
  }
  const body = (await resp.json()) as {
    ok?: boolean;
    api_key?: string;
    project_id?: string;
  };
  if (!body.ok || !body.api_key || !body.project_id) {
    throw new Error(
      `signup returned no api_key / project_id: ${JSON.stringify(body)}`,
    );
  }
  return {
    baseUrl,
    apiKey: body.api_key,
    projectId: body.project_id,
  };
}

// ── awaitFailureGroup ────────────────────────────────────────────────

export interface AwaitFailureGroupOptions {
  failureClass: string;
  signaturePrefix?: string;
  /**  accept any one of a list of alternative (failure_class,
   * signaturePrefix?) pairs in addition to the primary match. Useful
   * when a signal has multiple valid backend clustering shapes: e.g.
   * near-duplicate LLM calls fire as EITHER `loops/similar_call_*` OR
   * `drift/lexical_drift_*` depending on detector sensitivity. Both
   * are correct product signals; the test succeeds if ANY of the
   * (primary + alternatives) matches. */
  alternatives?: Array<{
    failureClass: string;
    signaturePrefix?: string;
  }>;
  /** Default: 30s. Some detectors need aggregation time. */
  timeoutSec?: number;
}

interface FailureGroupRow {
  failure_class?: string;
  signature?: string;
  [key: string]: unknown;
}

/**
 * Poll `GET /failure-groups` on the real backend until a group with
 * the given failure_class (and optional signature prefix) appears, or
 * the timeout elapses. Mirrors conftest.py `await_failure_group`
 * exactly on the HTTP surface (same headers, same match logic, same
 * "last seen" diagnostic on timeout).
 *
 * Returns the matching row on success. Throws on timeout with a
 * useful "last groups seen" list so the failure mode is diagnosable
 * from the pytest output.
 */
export async function awaitFailureGroup(
  backend: TestBackend,
  opts: AwaitFailureGroupOptions,
): Promise<FailureGroupRow> {
  const timeoutSec = opts.timeoutSec ?? 30;
  const deadline = Date.now() + timeoutSec * 1000;
  let lastSeen: FailureGroupRow[] = [];

  while (Date.now() < deadline) {
    const resp = await fetch(`${backend.baseUrl}/failure-groups`, {
      headers: { Authorization: `Bearer ${backend.apiKey}` },
    });
    if (resp.status === 200) {
      const body = (await resp.json()) as {
        groups?: FailureGroupRow[];
        failure_groups?: FailureGroupRow[];
      };
      lastSeen = body.groups ?? body.failure_groups ?? [];
      const matchers: Array<{
        failureClass: string;
        signaturePrefix?: string;
      }> = [
        { failureClass: opts.failureClass, signaturePrefix: opts.signaturePrefix },
        ...(opts.alternatives ?? []),
      ];
      for (const g of lastSeen) {
        const sig = g.signature ?? "";
        for (const m of matchers) {
          if (g.failure_class !== m.failureClass) continue;
          if (m.signaturePrefix && !sig.startsWith(m.signaturePrefix)) {
            continue;
          }
          return g;
        }
      }
    }
    await new Promise((r) => setTimeout(r, 250));
  }

  const seenSummary = lastSeen
    .map((g) => `${g.failure_class}/${g.signature}`)
    .join(", ");
  throw new Error(
    `no failure_group with class=${JSON.stringify(opts.failureClass)} ` +
      `signature_prefix=${JSON.stringify(opts.signaturePrefix ?? "")} ` +
      `appeared within ${timeoutSec}s. last groups seen: ${seenSummary}`,
  );
}

// ── Skip helpers ─────────────────────────────────────────────────────

/**
 * Print a one-line reason to stdout when a test is skipped. Vitest's
 * default skip UX doesn't show the reason; forcing it makes the "why"
 * legible in the run output. Mirrors pytest's `skip(reason=...)`
 * behavior which prints the reason inline.
 */
export function skipReason(reason: string): string {
  // eslint-disable-next-line no-console
  console.log(`  [SKIP] ${reason}`);
  return reason;
}
