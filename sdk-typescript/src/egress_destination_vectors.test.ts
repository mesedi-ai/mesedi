/**
 * Runs the shared egress destination vectors
 * (spec/egress_destination_vectors.json). The Python SDK runs the same
 * file in tests/test_egress_destination_vectors.py, so the two
 * normalizers cannot drift apart. Added 2026-11-15 after the outside
 * review found both SDKs sending user:password@ to the backend
 * (the outside review of 2026-11-15).
 */

import { readFileSync } from "node:fs";

import { describe, expect, it } from "vitest";

import { normalizeEgressDestination, UNREADABLE_DESTINATION } from "./egress.js";

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

// The outside review's round 2, 17 November 2026: a password with "/", "?"
// or "#" ended the host early and its first part was sent as "alice:12".
// The host after the credentials is kept instead, and with no host the
// destination is recorded as unreadable, never guessed.
describe("a password containing a cut character never leaves", () => {
  it("keeps the host after the credentials", () => {
    expect(normalizeEgressDestination("https://alice:12/34@db.example.com/x")).toBe("db.example.com");
    expect(normalizeEgressDestination("https://alice:9876?x@db.example.com")).toBe("db.example.com");
  });
  it("records an address with no host after the credentials as unreadable", () => {
    expect(normalizeEgressDestination("https://u:pw/x@")).toBe(UNREADABLE_DESTINATION);
  });
  it("does not treat an @ starting a path segment as a credential", () => {
    expect(normalizeEgressDestination("https://registry.npmjs.org:443/@types/node")).toBe("registry.npmjs.org:443");
  });
});
