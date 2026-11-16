/**
 * Runs the shared egress destination vectors
 * (spec/egress_destination_vectors.json). The Python SDK runs the same
 * file in tests/test_egress_destination_vectors.py, so the two
 * normalizers cannot drift apart. Added 2026-11-15 after the outside
 * review found both SDKs sending user:password@ to the backend
 * (finding D13).
 */

import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { normalizeEgressDestination } from "./egress.js";

interface VectorCase {
  in: string;
  out: string;
  why: string;
}

const cases: VectorCase[] = JSON.parse(
  readFileSync(new URL("../../spec/egress_destination_vectors.json", import.meta.url), "utf8"),
).cases;

describe("shared egress destination vectors", () => {
  it("has cases, so it cannot pass vacuously", () => {
    expect(cases.length).toBeGreaterThan(0);
  });

  it("normalizes every case as specified", () => {
    for (const c of cases) {
      expect(normalizeEgressDestination(c.in), `${c.in} (${c.why})`).toBe(c.out);
    }
  });

  it("never lets a secret survive", () => {
    for (const c of cases.filter((c) => c.in.includes("SECRET"))) {
      expect(normalizeEgressDestination(c.in)).not.toContain("SECRET");
    }
  });

  it("keeps the credentials case that was broken", () => {
    expect(cases.some((c) => c.in.includes(":SECRET@"))).toBe(true);
  });
});
