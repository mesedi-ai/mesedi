/**
 * mesedi-spend-check against stand-in endpoints answering in the
 * documented formats. Twin of sdk-python/tests/test_spend_check.py.
 */

import { describe, expect, it } from "vitest";

import { anthropicBill, compareSpend, mesediSpend, openaiBill, runSpendCheck, spendCheckMain, type FetchLike } from "./spend_check.js";

const SEEN: Record<string, Record<string, number>> = {
  "2026-11-10": { anthropic: 4, openai: 1 },
  "2026-11-11": { anthropic: 2 },
};

function stand(calls: Array<{ url: string; headers: Record<string, string> }>, anthropicPages: unknown[], openaiBody: unknown = {}): FetchLike {
  const pages = [...anthropicPages];
  return async (url, init) => {
    calls.push({ url, headers: init.headers });
    const u = new URL(url);
    let body: unknown;
    if (u.host === "api.anthropic.com") body = pages.shift();
    else if (u.host === "api.openai.com") body = openaiBody;
    else if (u.pathname === "/me/spend/daily") {
      body = { days: ["2026-11-10", "2026-11-11", "2026-11-12"].map((d) => ({ date: d, providers: SEEN[d] ?? {} })) };
    }
    return { status: body === undefined ? 404 : 200, json: async () => body, text: async () => JSON.stringify(body) };
  };
}

const ANTHROPIC_PAGES = [
  { data: [{ starting_at: "2026-11-10T00:00:00Z", results: [{ amount: "400.00", currency: "USD" }, { amount: "25.5", currency: "USD" }] }],
    has_more: true, next_page: "page_2" },
  { data: [{ starting_at: "2026-11-11T00:00:00Z", results: [{ amount: "5200", currency: "USD" }] },
           { starting_at: "2026-11-12T00:00:00Z", results: [] }], has_more: false, next_page: null },
];

const base = { start: "2026-11-10", end: "2026-11-12", mesediKeys: ["mesedi_sk_a"], mesediUrl: "https://mesedi.test", toleranceUsd: 1, tolerancePercent: 10 };

describe("spend check", () => {
  it("compares the Anthropic bill with what Mesedi saw, and keeps the admin key to the provider", async () => {
    const calls: Array<{ url: string; headers: Record<string, string> }> = [];
    const r = await runSpendCheck({ ...base, provider: "anthropic", adminKey: "ADMINKEY" }, stand(calls, ANTHROPIC_PAGES));
    const byDay = Object.fromEntries(r.map((d) => [d.date, d]));
    expect(byDay["2026-11-10"]!.flagged).toBe(false);
    expect(byDay["2026-11-11"]!.flagged).toBe(true);
    expect(byDay["2026-11-11"]!.unaccounted_usd).toBe(50);
    const anthropic = calls.filter((c) => c.url.includes("api.anthropic.com"));
    expect(anthropic).toHaveLength(2);
    expect(new URL(anthropic[1]!.url).searchParams.get("page")).toBe("page_2");
    expect(new URL(anthropic[0]!.url).searchParams.get("ending_at")).toBe("2026-11-13T00:00:00Z");
    for (const c of calls) {
      if (c.url.includes("api.anthropic.com")) expect(c.headers["x-api-key"]).toBe("ADMINKEY");
      else expect(JSON.stringify(c)).not.toContain("ADMINKEY");
    }
  });

  it("reads the OpenAI costs endpoint", async () => {
    const body = { object: "page", data: [{ object: "bucket", start_time: 1794268800, end_time: 1794355200,
      results: [{ object: "organization.costs.result", amount: { value: 30, currency: "usd" } }] }], has_more: false, next_page: null };
    const calls: Array<{ url: string; headers: Record<string, string> }> = [];
    const r = await runSpendCheck({ ...base, end: "2026-11-10", provider: "openai", adminKey: "OPENAIADMIN" }, stand(calls, [], body));
    expect(r[0]).toMatchObject({ bill_usd: 30, seen_usd: 1, flagged: true });
    const oc = calls.find((c) => c.url.includes("api.openai.com"))!;
    expect(oc.headers["Authorization"]).toBe("Bearer OPENAIADMIN");
    expect(new URL(oc.url).searchParams.get("start_time")).toBe("1794268800");
  });

  it("needs both tolerances exceeded", () => {
    const d = ["2026-11-10"];
    expect(compareSpend({ "2026-11-10": 1000 }, { "2026-11-10": 995 }, d, 1, 10)[0]!.flagged).toBe(false);
    expect(compareSpend({ "2026-11-10": 1 }, { "2026-11-10": 0.2 }, d, 1, 10)[0]!.flagged).toBe(false);
    expect(compareSpend({ "2026-11-10": 20 }, {}, d, 1, 10)[0]!.flagged).toBe(true);
  });

  it("exits 0, 1 or 2", async () => {
    const out: string[] = [];
    const write = (t: string) => void out.push(t);
    expect(await spendCheckMain(["--provider", "anthropic"], { MESEDI_API_KEY: "k" }, stand([], []), write)).toBe(2);
    expect(out.join("\n")).toContain("ANTHROPIC_ADMIN_KEY");
    out.length = 0;
    const code = await spendCheckMain(
      ["--provider", "anthropic", "--from", "2026-11-10", "--to", "2026-11-12", "--mesedi-url", "https://mesedi.test", "--json"],
      { MESEDI_API_KEY: "k", ANTHROPIC_ADMIN_KEY: "ADMINKEY" }, stand([], ANTHROPIC_PAGES.map((p) => ({ ...p }))), write);
    expect(code).toBe(1);
    expect(JSON.parse(out.join("\n")).flagged_days).toEqual(["2026-11-11"]);
    const failing: FetchLike = async () => ({ status: 401, json: async () => ({}), text: async () => "bad key" });
    out.length = 0;
    expect(await spendCheckMain(["--provider", "anthropic", "--days", "2"], { MESEDI_API_KEY: "k", ANTHROPIC_ADMIN_KEY: "x" }, failing, write)).toBe(2);
    expect(out.join("\n")).toContain("HTTP 401");
  });
});

describe("the three readers, directly", () => {
  it("anthropicBill, openaiBill and mesediSpend each return USD per day", async () => {
    const a = await anthropicBill(stand([], ANTHROPIC_PAGES.map((p) => ({ ...p }))), "k", "2026-11-10", "2026-11-12");
    expect(a["2026-11-11"]).toBe(52);
    const o = await openaiBill(stand([], [], { data: [{ start_time: 1794268800, results: [{ amount: { value: 2.5, currency: "usd" } }] }] }), "k", "2026-11-10", "2026-11-10");
    expect(o["2026-11-10"]).toBe(2.5);
    const m = await mesediSpend(stand([], []), "https://mesedi.test", ["k1", "k2"], "anthropic", "2026-11-10", "2026-11-11");
    expect(m).toEqual({ "2026-11-10": 8, "2026-11-11": 4, "2026-11-12": 0 });
  });
});
