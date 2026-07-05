import { describe, expect, test } from "vitest";

import {
  DEFAULT_MAX_PAYLOAD_BYTES,
  MARKER_ORIGINAL_BYTES,
  MARKER_TRUNCATED,
  MARKER_TRUNCATED_FIELDS,
  MIN_KEEP_CHARS,
  TRUNCATION_SUFFIX,
  maybeTruncate,
} from "./truncate.js";

function byteSize(payload: unknown): number {
  return new TextEncoder().encode(JSON.stringify(payload)).byteLength;
}

describe("maybeTruncate", () => {
  test("below cap passes through unchanged", () => {
    const payload = { prompt: "hello", response: "world" };
    const out = maybeTruncate(payload);
    expect(out).toBe(payload);
    expect(out[MARKER_TRUNCATED]).toBeUndefined();
    expect(out[MARKER_ORIGINAL_BYTES]).toBeUndefined();
    expect(out[MARKER_TRUNCATED_FIELDS]).toBeUndefined();
  });

  test("oversized string field gets truncated; small fields preserved", () => {
    const payload = {
      prompt: "summarize the doc",
      response: "x".repeat(DEFAULT_MAX_PAYLOAD_BYTES * 2),
    };
    const originalBytes = byteSize(payload);
    const out = maybeTruncate(payload);
    expect(out[MARKER_TRUNCATED]).toBe(true);
    expect(out[MARKER_ORIGINAL_BYTES]).toBe(originalBytes);
    expect(out[MARKER_TRUNCATED_FIELDS]).toContain("response");
    expect(out[MARKER_TRUNCATED_FIELDS]).not.toContain("prompt");
    expect(out["prompt"]).toBe("summarize the doc");
    expect(String(out["response"])).toContain(TRUNCATION_SUFFIX);
    expect(byteSize(out)).toBeLessThanOrEqual(DEFAULT_MAX_PAYLOAD_BYTES + 200);
  });

  test("multiple oversized fields all get truncated", () => {
    const payload = {
      prompt: "a".repeat(DEFAULT_MAX_PAYLOAD_BYTES / 2),
      response: "b".repeat(DEFAULT_MAX_PAYLOAD_BYTES / 2),
      stack_trace: "c".repeat(DEFAULT_MAX_PAYLOAD_BYTES / 2),
    };
    const out = maybeTruncate(payload);
    expect(out[MARKER_TRUNCATED]).toBe(true);
    const truncated = out[MARKER_TRUNCATED_FIELDS] as string[];
    expect(truncated.length).toBeGreaterThanOrEqual(2);
    expect(byteSize(out)).toBeLessThanOrEqual(DEFAULT_MAX_PAYLOAD_BYTES + 200);
  });

  test("custom cap drives truncation", () => {
    const payload = { response: "x".repeat(5000) };
    const out = maybeTruncate(payload, 1024);
    expect(out[MARKER_TRUNCATED]).toBe(true);
    expect(Number(out[MARKER_ORIGINAL_BYTES])).toBeGreaterThan(1024);
    expect(String(out["response"])).toContain(TRUNCATION_SUFFIX);
  });

  test("short strings are preserved", () => {
    const payload = {
      execution_id: "exec_abc123",
      prompt: "x".repeat(DEFAULT_MAX_PAYLOAD_BYTES * 2),
    };
    const out = maybeTruncate(payload);
    expect(out["execution_id"]).toBe("exec_abc123");
    const truncated = out[MARKER_TRUNCATED_FIELDS] as string[];
    expect(truncated).not.toContain("execution_id");
  });

  test("non-object payload passes through unchanged", () => {
    // Defensive: customer might supply a non-object even though the
    // type hint says Record<string, unknown>. Should bail without
    // throwing.
    const listLike = [1, 2, 3] as unknown as Record<string, unknown>;
    expect(maybeTruncate(listLike)).toBe(listLike);
  });

  test("truncated field keeps at least MIN_KEEP_CHARS of original", () => {
    const payload = { response: "x".repeat(DEFAULT_MAX_PAYLOAD_BYTES * 100) };
    const out = maybeTruncate(payload);
    const finalLen =
      String(out["response"]).length - TRUNCATION_SUFFIX.length;
    expect(finalLen).toBeGreaterThanOrEqual(MIN_KEEP_CHARS);
  });

  test("realistic LLM event under cap", () => {
    // Typical LLM-call event with prompt + response + tokens fits
    // comfortably under the default cap. Sanity check that the 99%
    // case is not accidentally marked truncated.
    const payload = {
      model: "claude-sonnet-4-6",
      prompt:
        "Summarize the following document in three sentences.".repeat(5),
      response: "The document discusses...".repeat(50),
      prompt_tokens: 245,
      completion_tokens: 87,
      latency_ms: 1240,
    };
    const out = maybeTruncate(payload);
    expect(out).toBe(payload);
  });
});
