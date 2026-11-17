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
export function normalizeEgressDestination(
  destination: string,
  addressesNeverContainPasswords = false,
): string {
  let d = destination.trim();
  const schemeIdx = d.indexOf("://");
  if (schemeIdx >= 0) d = d.slice(schemeIdx + 3);
  const cuts = ["/", "?", "#"].map((c) => d.indexOf(c)).filter((i) => i >= 0);
  const cut = cuts.length ? Math.min(...cuts) : -1;
  const authority = cut < 0 ? d : d.slice(0, cut);
  // A password may itself contain "/", "?" or "#", which ends the host
  // part early: "alice:12/34@db.example.com" used to be sent as
  // "alice:12" (the outside review's round 2, 17 November 2026). When the
  // part before the first cut has a ":" and an "@" follows the cut, not at
  // the start of a path segment, everything up to that "@" is possible
  // credentials and the host after it is kept; with no host after it, the
  // destination is "unreadable" rather than a guess. Owner decision, 17 November 2026:
  // never send a possible password.
  if (cut >= 0 && !authority.includes("@") && authority.includes(":")) {
    // The first "@" after the cut that does not start a path segment:
    // "/@types/node" is a path, not a credential.
    let at = d.indexOf("@", cut + 1);
    while (at >= 0 && d[at - 1] === "/") at = d.indexOf("@", at + 1);
    // Round 3 (finding M2): a host and all-digit port before a later
    // "@" ("host:8080/a@b", once recorded as "b") cannot be told apart
    // from a user and a numeric password ("alice:1234/x@db"). Recorded
    // as "unreadable" unless the team says its addresses never carry
    // passwords (owner decision, 17 November 2026).
    if (at >= 0 && hostAndNumericPort(authority) === authority) {
      return addressesNeverContainPasswords ? authority : UNREADABLE_DESTINATION;
    }
    if (at >= 0) {
      let rest = d.slice(at + 1);
      for (const c of ["/", "?", "#"]) {
        const i = rest.indexOf(c);
        if (i >= 0) rest = rest.slice(0, i);
      }
      const host = hostAndNumericPort(rest);
      return host ? host : UNREADABLE_DESTINATION;
    }
  }
  d = authority;
  // Everything up to the last @ is userinfo (user, or user:password).
  // Until 2026-11-15 it was kept and sent (the outside review of 2026-11-15).
  const at = d.lastIndexOf("@");
  if (at >= 0) d = d.slice(at + 1);
  return hostAndNumericPort(d);
}

/** The configured client's setting; false when none is configured. */
function addressesNeverContainPasswords(): boolean {
  try {
    return getClient().addressesNeverContainPasswords;
  } catch {
    return false;
  }
}

/**
 * Recorded when an address carried possible credentials and no host can
 * be read safely after them.
 */
export const UNREADABLE_DESTINATION = "unreadable";

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
    destination: normalizeEgressDestination(destination, addressesNeverContainPasswords()),
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
