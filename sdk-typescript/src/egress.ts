/**
 * Egress and environment-declaration emitters.
 *
 * Added 2026-09-14 when the egress-visibility deferral was reopened.
 * Twin of sdk-python/mesedi/egress.py.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";

/**
 * Reduce a destination to host or host:port. A full URL loses its
 * scheme, path and query on purpose: identical services must
 * cluster, and query strings are where secrets ride.
 */
function normalizeEgressDestination(destination: string): string {
  let d = destination.trim();
  const schemeIdx = d.indexOf("://");
  if (schemeIdx >= 0) d = d.slice(schemeIdx + 3);
  for (const cut of ["/", "?", "#"]) {
    const i = d.indexOf(cut);
    if (i >= 0) d = d.slice(0, i);
  }
  return d;
}

/** Options for emitEgress. */
export interface EgressOptions {
  protocol?: string;
  toolName?: string;
  bytesOut?: number;
  note?: string;
}

/**
 * Emit an `egress` event recording one outbound network contact by
 * the agent's environment.
 *
 * Report, never infer: call this from the host application or
 * sandbox that actually opened the connection. Mesedi cannot see a
 * connection nobody reports. The destination is normalized to host
 * or host:port; scheme, path and query are dropped so identical
 * services cluster and secrets in URLs never leave the process.
 *
 * Outside `wrap()`: silent no-op.
 */
export function emitEgress(destination: string, opts: EgressOptions = {}): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = {
    destination: normalizeEgressDestination(destination),
  };
  if (opts.protocol) payload["protocol"] = opts.protocol;
  if (opts.toolName) payload["tool_name"] = opts.toolName;
  if (opts.bytesOut) payload["bytes_out"] = opts.bytesOut;
  if (opts.note) payload["note"] = opts.note;

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.EGRESS,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  client.submitEvent(event);
}

/** Options for emitEnvironmentDeclaration. */
export interface EnvironmentDeclarationOptions {
  declaredBy?: string;
  note?: string;
}

/**
 * Emit an `environment_declaration` event stating what environment
 * this run is SUPPOSED to be in.
 *
 * Well-known modes are "live", "simulation" and "staging"; any other
 * string is stored verbatim and treated as not-live. Emit once, near
 * the start of the run. The backend's environment_misapprehension
 * detector compares later observations against this claim; it checks
 * the declared boundary, never the model's beliefs.
 *
 * Outside `wrap()`: silent no-op.
 */
export function emitEnvironmentDeclaration(
  mode: string,
  opts: EnvironmentDeclarationOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = {
    mode: mode.trim().toLowerCase(),
  };
  payload["declared_by"] = opts.declaredBy ?? "operator";
  if (opts.note) payload["note"] = opts.note;

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.ENVIRONMENT_DECLARATION,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    payload,
  };
  client.submitEvent(event);
}
