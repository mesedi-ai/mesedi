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
 * scheme, path, query, fragment and any user:password@ part on
 * purpose: identical services must cluster, and query strings and
 * userinfo are where secrets ride. A port survives only if it is all
 * digits, so a malformed URL cannot smuggle part of a password out as
 * a "port". Cases: spec/egress_destination_vectors.json (shared with
 * the Python SDK). Exported only for that test; not re-exported from
 * index.ts, so it is not part of the public API.
 */
export function normalizeEgressDestination(destination: string): string {
  let d = destination.trim();
  const schemeIdx = d.indexOf("://");
  if (schemeIdx >= 0) d = d.slice(schemeIdx + 3);
  for (const cut of ["/", "?", "#"]) {
    const i = d.indexOf(cut);
    if (i >= 0) d = d.slice(0, i);
  }
  // Everything up to the last @ is userinfo (user, or user:password).
  // Until 2026-11-15 it was kept and sent (the outside review of 2026-11-15).
  const at = d.lastIndexOf("@");
  if (at >= 0) d = d.slice(at + 1);
  return hostAndNumericPort(d);
}

/**
 * Keep host, plus :port only when the port is all ASCII digits.
 * Bracketed IPv6 hosts keep their brackets.
 */
function hostAndNumericPort(d: string): string {
  const digits = /^[0-9]+$/;
  if (d.startsWith("[")) {
    const end = d.indexOf("]");
    if (end === -1) return d;
    const host = d.slice(0, end + 1);
    const rest = d.slice(end + 1);
    const port = rest.startsWith(":") ? rest.slice(1) : "";
    return digits.test(port) ? host + rest : host;
  }
  const colon = d.indexOf(":");
  if (colon >= 0) {
    return digits.test(d.slice(colon + 1)) ? d : d.slice(0, colon);
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
 * or host:port; scheme, path, query, fragment and user:password@ are
 * dropped so identical services cluster and secrets in URLs never
 * leave the process. Versions before 0.10.0 kept the user:password@
 * part.
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
