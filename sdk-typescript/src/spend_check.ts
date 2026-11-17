/**
 * mesedi-spend-check: compare your AI provider's bill with the spend
 * Mesedi saw in monitored runs, on your own computer. Twin of
 * sdk-python/mesedi/spend_check.py; read that module's docstring for
 * the full reasoning.
 *
 * Spend outside a monitored run, a stolen key used elsewhere included,
 * never reaches Mesedi. The provider's bill sees all of it, so the
 * difference between the two, day by day, is where it shows up. Reading
 * the bill needs the provider's admin key, which can manage the whole
 * AI account and has no read-only kind: this check reads it from an
 * environment variable and sends it only to the provider. The Mesedi
 * API key goes only to Mesedi (GET /me/spend/daily).
 *
 * Mesedi's figure is estimated from token counts at list prices, so a
 * day is flagged only when the bill exceeds it by more than both
 * tolerances. Set a spending limit at the provider as well: this check
 * finds spend after the fact, a limit stops it.
 *
 *   MESEDI_API_KEY=... ANTHROPIC_ADMIN_KEY=... npx mesedi-spend-check --provider anthropic --days 30
 *
 * Exit codes: 0 no day flagged, 1 a day flagged, 2 the check could not run.
 */

export const ANTHROPIC_COST_REPORT = "https://api.anthropic.com/v1/organizations/cost_report";
export const OPENAI_COSTS = "https://api.openai.com/v1/organization/costs";
const DEFAULT_MESEDI_URL = "https://api.mesedi.ai";
const MESEDI_MAX_DAYS = 93; // the endpoint's limit per request

export const ADMIN_KEY_ENV: Record<string, string> = {
  anthropic: "ANTHROPIC_ADMIN_KEY",
  openai: "OPENAI_ADMIN_KEY",
};

/** The slice of fetch this module uses, so tests can supply their own. */
export type FetchLike = (
  url: string,
  init: { headers: Record<string, string> },
) => Promise<{ status: number; json(): Promise<unknown>; text(): Promise<string> }>;

export class SpendCheckError extends Error {}

type Json = Record<string, unknown>;

function isoDay(d: Date): string {
  return d.toISOString().slice(0, 10);
}

function addDays(day: string, n: number): string {
  const d = new Date(`${day}T00:00:00Z`);
  d.setUTCDate(d.getUTCDate() + n);
  return isoDay(d);
}

function daysBetween(start: string, end: string): string[] {
  const out: string[] = [];
  for (let d = start; d <= end; d = addDays(d, 1)) out.push(d);
  return out;
}

async function getJson(fetchFn: FetchLike, url: string, headers: Record<string, string>, who: string): Promise<Json> {
  const r = await fetchFn(url, { headers });
  if (r.status !== 200) {
    throw new SpendCheckError(`${who}: HTTP ${r.status}: ${(await r.text()).slice(0, 300)}`);
  }
  return (await r.json()) as Json;
}

/** USD per UTC day from Anthropic's Cost Report (amounts are decimal strings in cents). */
export async function anthropicBill(fetchFn: FetchLike, adminKey: string, start: string, end: string): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  const params = new URLSearchParams({
    starting_at: `${start}T00:00:00Z`,
    ending_at: `${addDays(end, 1)}T00:00:00Z`,
    bucket_width: "1d",
    limit: "31",
  });
  const headers = { "x-api-key": adminKey, "anthropic-version": "2023-06-01", Accept: "application/json" };
  for (;;) {
    const body = await getJson(fetchFn, `${ANTHROPIC_COST_REPORT}?${params}`, headers, "Anthropic cost report");
    for (const bucket of (body["data"] as Json[] | undefined) ?? []) {
      const day = String(bucket["starting_at"] ?? "").slice(0, 10);
      let total = 0;
      for (const res of (bucket["results"] as Json[] | undefined) ?? []) {
        const currency = String(res["currency"] ?? "USD").toUpperCase();
        if (currency !== "USD") throw new SpendCheckError(`Anthropic reported a cost in ${currency}; this check compares USD only`);
        total += Number(res["amount"] ?? 0) / 100;
      }
      out[day] = (out[day] ?? 0) + total;
    }
    if (!body["has_more"] || !body["next_page"]) return out;
    params.set("page", String(body["next_page"]));
  }
}

/** USD per UTC day from OpenAI's organization Costs endpoint. */
export async function openaiBill(fetchFn: FetchLike, adminKey: string, start: string, end: string): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  const params = new URLSearchParams({
    start_time: String(Date.parse(`${start}T00:00:00Z`) / 1000),
    end_time: String(Date.parse(`${addDays(end, 1)}T00:00:00Z`) / 1000),
    bucket_width: "1d",
    limit: "180",
  });
  const headers = { Authorization: `Bearer ${adminKey}`, "Content-Type": "application/json" };
  for (;;) {
    const body = await getJson(fetchFn, `${OPENAI_COSTS}?${params}`, headers, "OpenAI costs");
    for (const bucket of (body["data"] as Json[] | undefined) ?? []) {
      const day = isoDay(new Date(Number(bucket["start_time"] ?? 0) * 1000));
      let total = 0;
      for (const res of (bucket["results"] as Json[] | undefined) ?? []) {
        const amount = (res["amount"] as Json | undefined) ?? {};
        const currency = String(amount["currency"] ?? "usd").toUpperCase();
        if (currency !== "USD") throw new SpendCheckError(`OpenAI reported a cost in ${currency}; this check compares USD only`);
        total += Number(amount["value"] ?? 0);
      }
      out[day] = (out[day] ?? 0) + total;
    }
    if (!body["has_more"] || !body["next_page"]) return out;
    params.set("page", String(body["next_page"]));
  }
}

const BILL_READERS: Record<string, typeof anthropicBill> = { anthropic: anthropicBill, openai: openaiBill };

/** USD per UTC day Mesedi saw for this provider, summed over the given projects' keys. */
export async function mesediSpend(fetchFn: FetchLike, baseUrl: string, apiKeys: string[], provider: string, start: string, end: string): Promise<Record<string, number>> {
  const out: Record<string, number> = {};
  for (const key of apiKeys) {
    for (let from = start; from <= end; ) {
      let to = addDays(from, MESEDI_MAX_DAYS - 1);
      if (to > end) to = end;
      const params = new URLSearchParams({ from, to });
      const body = await getJson(fetchFn, `${baseUrl.replace(/\/$/, "")}/me/spend/daily?${params}`,
        { Authorization: `Bearer ${key}` }, "Mesedi spend report");
      for (const d of (body["days"] as Json[] | undefined) ?? []) {
        const day = String(d["date"]);
        const usd = Number(((d["providers"] as Json | undefined) ?? {})[provider] ?? 0);
        out[day] = (out[day] ?? 0) + usd;
      }
      from = addDays(to, 1);
    }
  }
  return out;
}

export interface DayResult {
  date: string;
  bill_usd: number;
  seen_usd: number;
  unaccounted_usd: number;
  flagged: boolean;
}

const round6 = (n: number) => Math.round(n * 1e6) / 1e6;

/** Flag a day when the bill exceeds Mesedi's figure by more than both tolerances. */
export function compareSpend(bill: Record<string, number>, seen: Record<string, number>, days: string[], toleranceUsd: number, tolerancePercent: number): DayResult[] {
  return days.map((d) => {
    const b = round6(bill[d] ?? 0);
    const s = round6(seen[d] ?? 0);
    const gap = round6(b - s);
    return { date: d, bill_usd: b, seen_usd: s, unaccounted_usd: gap, flagged: gap > toleranceUsd && gap > (b * tolerancePercent) / 100 };
  });
}

export interface SpendCheckOptions {
  provider: string;
  start: string;
  end: string;
  adminKey: string;
  mesediKeys: string[];
  mesediUrl: string;
  toleranceUsd: number;
  tolerancePercent: number;
}

export async function runSpendCheck(o: SpendCheckOptions, fetchFn: FetchLike): Promise<DayResult[]> {
  const reader = BILL_READERS[o.provider];
  if (!reader) throw new SpendCheckError(`unknown provider "${o.provider}"; supported: anthropic, openai`);
  if (o.end < o.start) throw new SpendCheckError("the period ends before it starts");
  const bill = await reader(fetchFn, o.adminKey, o.start, o.end);
  const seen = await mesediSpend(fetchFn, o.mesediUrl, o.mesediKeys, o.provider, o.start, o.end);
  return compareSpend(bill, seen, daysBetween(o.start, o.end), o.toleranceUsd, o.tolerancePercent);
}

function flagValue(argv: string[], name: string): string | undefined {
  const i = argv.indexOf(name);
  return i >= 0 ? argv[i + 1] : undefined;
}

/**
 * The command: reads keys from the environment, writes results to
 * stdout and the reason it could not run to stderr. Returns the exit
 * code.
 */
export async function spendCheckMain(
  argv: string[],
  env: Record<string, string | undefined>,
  fetchFn: FetchLike,
  write: (text: string, err?: boolean) => void,
): Promise<number> {
  const provider = flagValue(argv, "--provider") ?? "";
  try {
    const adminEnv = ADMIN_KEY_ENV[provider];
    if (!adminEnv) throw new SpendCheckError("--provider must be anthropic or openai");
    const adminKey = (env[adminEnv] ?? "").trim();
    if (!adminKey) throw new SpendCheckError(`set ${adminEnv} to the provider's admin key`);
    const keys = [env["MESEDI_API_KEY"] ?? "", ...(env["MESEDI_EXTRA_API_KEYS"] ?? "").split(",")]
      .map((k) => k.trim()).filter((k) => k);
    if (!keys.length) throw new SpendCheckError("set MESEDI_API_KEY to a Mesedi project's API key");
    const yesterday = addDays(isoDay(new Date()), -1);
    const end = flagValue(argv, "--to") ?? yesterday;
    const start = flagValue(argv, "--from") ?? addDays(end, -(Number(flagValue(argv, "--days") ?? "30") - 1));
    for (const d of [start, end]) {
      if (!/^\d{4}-\d{2}-\d{2}$/.test(d)) throw new SpendCheckError(`${d} is not a date, YYYY-MM-DD`);
    }
    const results = await runSpendCheck({
      provider, start, end, adminKey, mesediKeys: keys,
      mesediUrl: flagValue(argv, "--mesedi-url") ?? env["MESEDI_BASE_URL"] ?? DEFAULT_MESEDI_URL,
      toleranceUsd: Number(flagValue(argv, "--tolerance-usd") ?? "1"),
      tolerancePercent: Number(flagValue(argv, "--tolerance-percent") ?? "10"),
    }, fetchFn);
    const flagged = results.filter((r) => r.flagged);
    if (argv.includes("--json")) {
      write(JSON.stringify({ provider, days: results, flagged_days: flagged.map((r) => r.date) }, null, 2));
    } else {
      write(`${provider} bill against spend Mesedi saw, per UTC day (USD)`);
      for (const r of results) {
        write(`${r.date}  bill ${r.bill_usd.toFixed(2)}  Mesedi saw ${r.seen_usd.toFixed(2)}  unaccounted ${r.unaccounted_usd.toFixed(2)}${r.flagged ? "  FLAG" : ""}`);
      }
      write(flagged.length
        ? `\n${flagged.length} day(s) flagged: the bill exceeds what monitored runs account for. That is an app not instrumented with Mesedi, or a key used by someone else. Check the provider's console for which key spent it, and set a spending limit at the provider.`
        : "\nNo day flagged. Mesedi's figure is an estimate at list prices, so small differences are expected.");
    }
    return flagged.length ? 1 : 0;
  } catch (err) {
    write(`mesedi-spend-check: ${err instanceof Error ? err.message : String(err)}`, true);
    return 2;
  }
}
