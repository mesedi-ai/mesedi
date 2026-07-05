/**
 * Sandbox round-trip test for SDK gzip compression.
 *
 * Compresses a real 100-event batch and POSTs it to a configured
 * Mesedi backend, then verifies the backend accepted it (HTTP 200
 * from `/events`). The backend must speak compression (the
 * `api: accept gzip-compressed request bodies` commit or later);
 * pre-compression backends would reject the body as malformed JSON.
 *
 * Run against a local backend:
 *
 *     MESEDI_API_KEY=mesedi_sk_test_... \
 *     MESEDI_BASE_URL=http://localhost:8080 \
 *     node dist-sandbox/sandbox/compression_test.js
 *
 * Or against production with a test-mode key (do this only with a
 * project you own; the synthetic-customer project is the safe
 * choice).
 */

import { gzipSync } from "node:zlib";

const apiKey = process.env["MESEDI_API_KEY"];
if (!apiKey) {
  console.error("MESEDI_API_KEY not set");
  process.exit(1);
}
const baseUrl = (
  process.env["MESEDI_BASE_URL"] ?? "https://api.mesedi.ai"
).replace(/\/$/, "");

const executionId = `exec_sandbox_${Math.random().toString(36).slice(2, 14)}`;

interface SandboxEvent {
  event_id: string;
  execution_id: string;
  event_type: string;
  sequence: number;
  timestamp: string;
  payload: {
    prompt: string;
    response: string;
    model: string;
  };
}

const batch: SandboxEvent[] = [];
for (let i = 0; i < 100; i++) {
  batch.push({
    event_id: `evt_sandbox_${Math.random().toString(36).slice(2, 18)}`,
    execution_id: executionId,
    event_type: "llm_call",
    sequence: i,
    timestamp: "2026-06-19T00:00:00Z",
    payload: {
      prompt: "summarize the following document",
      response: "ok ".repeat(20),
      model: "claude-sonnet-4-6",
    },
  });
}

const json = JSON.stringify(batch);
const raw = new TextEncoder().encode(json);
const compressed = gzipSync(raw);
const ratio = compressed.byteLength / raw.byteLength;

console.log(
  `sandbox: raw=${raw.byteLength}, compressed=${compressed.byteLength}, ratio=${ratio.toFixed(2)}`,
);

// Step 1: create the execution (uncompressed for clarity) so the
// events endpoint has an execution to attach to.
const execResp = await fetch(`${baseUrl}/executions`, {
  method: "POST",
  headers: {
    Authorization: `Bearer ${apiKey}`,
    "Content-Type": "application/json",
    "X-Mesedi-Schema-Version": "1",
  },
  body: JSON.stringify({
    execution_id: executionId,
    agent_name: "compression_sandbox_ts",
    started_at: "2026-06-19T00:00:00Z",
  }),
});

if (execResp.status >= 400) {
  console.error(
    `sandbox: POST /executions failed: ${execResp.status} ${await execResp.text()}`,
  );
  process.exit(3);
}

// Step 2: send the compressed batch with the negotiated header.
const eventsResp = await fetch(`${baseUrl}/events`, {
  method: "POST",
  headers: {
    Authorization: `Bearer ${apiKey}`,
    "Content-Type": "application/json",
    "Content-Encoding": "gzip",
    "X-Mesedi-Schema-Version": "1",
  },
  body: compressed,
});

if (eventsResp.status >= 400) {
  console.error(
    `sandbox: POST /events failed: ${eventsResp.status} ${await eventsResp.text()}`,
  );
  process.exit(4);
}

console.log(
  `sandbox: PASS — backend accepted ${batch.length} events ` +
    `(compressed ${raw.byteLength}→${compressed.byteLength} bytes, ` +
    `ratio ${ratio.toFixed(2)}, response ${eventsResp.status})`,
);
