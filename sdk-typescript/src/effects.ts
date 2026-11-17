/**
 * Effect declarations and per-call effects, for the backend's
 * effect_aliasing check: two paths reach the same side effect and only
 * one is watched. Twin of sdk-python/mesedi/effects.py.
 *
 * The class and its one-line spec come from a thread in the LangChain
 * community i-made-this channel and from Alessandro Prandini's writeup
 * "Four ways an agent walked past my command hook" (digline.dev).
 *
 * A run states where it may make changes, optionally with the tools
 * allowed to make each one (`declareEffects`). Tool and MCP calls report
 * what they changed (`recordEffects` inside a `tool()` function, or
 * `effects` on `emitMcpCall`). The backend flags a change to a place
 * not declared, or by a tool not listed for it. A run with no
 * declaration is not checked. Name the underlying resource, not the
 * route: two names for one place look like two places.
 */

import { AsyncLocalStorage } from "node:async_hooks";

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";

/** The list a tool() call collects recordEffects() into. */
export const effectsStorage = new AsyncLocalStorage<string[]>();

/**
 * Report the places the current `tool()` call changed. The
 * destinations ride on the tool's own `tool_call` event as `effects`.
 * Outside a `tool()` call: no-op.
 */
export function recordEffects(...destinations: string[]): void {
  const bucket = effectsStorage.getStore();
  if (!bucket) return;
  for (const d of destinations) {
    if (typeof d === "string" && d.trim()) bucket.push(d.trim());
  }
}

/** One place a run may change; `via` lists the only tools allowed to. */
export interface EffectDestination {
  destination: string;
  via?: string[];
}

export interface EffectDeclarationOptions {
  declaredBy?: string;
  note?: string;
}

/**
 * Emit an `effect_declaration` event: where this run may make changes.
 * A string allows any tool; an object with `via` names the only tools
 * allowed (MCP tools as "server/method"). A destination ending in `*`
 * covers everything that starts with the text before it. Declarations
 * in one run add up. Outside `wrap()`: no-op.
 */
export function declareEffects(
  destinations: Array<string | EffectDestination>,
  opts: EffectDeclarationOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;
  const out: Array<Record<string, unknown>> = [];
  for (const d of destinations) {
    if (typeof d === "string") {
      if (d.trim()) out.push({ destination: d.trim() });
      continue;
    }
    const dest = String(d.destination ?? "").trim();
    if (!dest) continue;
    const entry: Record<string, unknown> = { destination: dest };
    const via = (d.via ?? []).map((v) => String(v).trim()).filter((v) => v);
    if (via.length) entry["via"] = via;
    out.push(entry);
  }
  const payload: Record<string, unknown> = { destinations: out };
  const declaredBy = opts.declaredBy ?? "operator";
  if (declaredBy) payload["declared_by"] = declaredBy;
  if (opts.note) payload["note"] = opts.note;
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.EFFECT_DECLARATION,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  getClient().submitEvent(event);
}

/** Trimmed, non-empty destination strings, order kept. */
export function cleanEffects(effects: string[] | undefined): string[] {
  if (!effects) return [];
  return effects.filter((e) => typeof e === "string" && e.trim()).map((e) => e.trim());
}
