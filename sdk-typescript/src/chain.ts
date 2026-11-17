/**
 * Report chaining and optional signing (mesedi-report-chain-v1). Twin
 * of sdk-python/mesedi/_chain.py.
 *
 * Every report the shipper sends carries `chain_prev`: the fingerprint
 * of the report it sent before it in the same run, or the run's genesis
 * value for the first. The backend fingerprints each report exactly as
 * it arrives and checks that a run's reports form one unbroken path, so
 * a report removed, changed or slipped in after sending is caught.
 *
 * The fingerprint covers the payload as the exact bytes sent: the
 * shipper sends `JSON.stringify(batch)`, which nests each payload as
 * `JSON.stringify(payload)`, UTF-8 encoded. Field list and test
 * vectors: spec/report_chain_vectors.json, shared with the backend and
 * the Python SDK.
 *
 * Signing is optional: `configure({ reportSigner })` takes a function
 * that receives the 32-byte fingerprint and returns `{ keyId,
 * signature }`, an Ed25519 signature by a key registered to the
 * project. It is meant for a reporting process the agent cannot reach;
 * signing proves which process sent a report, not that it is true, and
 * whoever holds every key can still invent reports.
 */

import { createHash, createPrivateKey, sign as cryptoSign } from "node:crypto";

import type { Event } from "./events.js";

export const REPORT_CHAIN_ALGORITHM = "mesedi-report-chain-v1";

// Runs whose last fingerprint is remembered; see the Python twin.
const MAX_TRACKED_RUNS = 10_000;

export interface ReportSignature {
  keyId: string;
  signature: Uint8Array;
}

/** Signs a report's 32-byte fingerprint. May return a promise. */
export type ReportSigner = (fingerprint: Uint8Array) => ReportSignature | Promise<ReportSignature>;

function sha256Hex(data: string | Uint8Array): string {
  return createHash("sha256").update(data).digest("hex");
}

/** The chain_prev of a run's first report. */
export function reportChainGenesis(executionId: string): string {
  return sha256Hex(`${REPORT_CHAIN_ALGORITHM} genesis\n${executionId}`);
}

/**
 * UTC with exactly six fractional digits, as the backend formats the
 * instant it parsed. Throws on an unparseable value.
 */
export function canonicalTimestamp(ts: string): string {
  const m = /^(\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2})(?:\.(\d+))?(Z|z|[+-]\d{2}:\d{2})$/.exec(ts.trim());
  if (!m) throw new Error(`unparseable timestamp ${JSON.stringify(ts)}`);
  const [, base, frac = "", zone] = m;
  const micros = (frac + "000000").slice(0, 6);
  const offset = zone === "Z" || zone === "z" ? "Z" : zone;
  const millis = Date.parse(`${base}.000${offset}`);
  if (Number.isNaN(millis)) throw new Error(`unparseable timestamp ${JSON.stringify(ts)}`);
  const utc = new Date(millis).toISOString(); // YYYY-MM-DDTHH:MM:SS.000Z
  return `${utc.slice(0, 19)}.${micros}Z`;
}

/**
 * The 32 bytes a signer signs to declare a run's last report. Not the
 * fingerprint itself, so a report's own signature cannot be replayed as
 * an end. Vectors: spec/report_chain_vectors.json "end".
 */
export function reportChainEndMessage(executionId: string, lastFingerprint: string): Uint8Array {
  return Uint8Array.from(
    createHash("sha256").update(`${REPORT_CHAIN_ALGORITHM} end\n${executionId}\n${lastFingerprint}`).digest(),
  );
}

/** The payload exactly as the shipper puts it on the wire. */
export function payloadBytes(payload: unknown): Uint8Array {
  return new TextEncoder().encode(JSON.stringify(payload));
}

export interface FingerprintInput {
  eventId: string;
  executionId: string;
  eventType: string;
  sequence: number;
  timestamp: string;
  durationMs?: number;
  payload: Uint8Array;
  chainPrev: string;
}

export function reportFingerprint(f: FingerprintInput): string {
  const fields: Array<[string, string]> = [
    ["algorithm", REPORT_CHAIN_ALGORITHM],
    ["event_id", f.eventId],
    ["execution_id", f.executionId],
    ["event_type", f.eventType],
    ["sequence", String(Math.trunc(f.sequence))],
    ["timestamp", canonicalTimestamp(f.timestamp)],
    ["duration_ms", String(Math.trunc(f.durationMs ?? 0))],
    ["payload_sha256", sha256Hex(f.payload)],
    ["chain_prev", f.chainPrev],
  ];
  const enc = new TextEncoder();
  const parts: Uint8Array[] = [];
  for (const [label, value] of fields) {
    const v = enc.encode(value);
    parts.push(enc.encode(`${label}:${v.length}:`), v, enc.encode("\n"));
  }
  const hash = createHash("sha256");
  for (const p of parts) hash.update(p);
  return hash.digest("hex");
}

function hexToBytes(hex: string): Uint8Array {
  return Uint8Array.from(Buffer.from(hex, "hex"));
}

/**
 * Chains events in the order the shipper sends them. Used only from the
 * shipper's serialized processing loop.
 */
export class ReportChainer {
  private readonly last = new Map<string, string>();

  constructor(private readonly signer?: ReportSigner) {}

  /**
   * Set chain_prev (and signature when signing) and advance the run's
   * chain. An event that cannot be encoded is left unchained; the
   * backend accepts it and marks the run's chain broken, the truth.
   */
  async chain(event: Event): Promise<void> {
    const prev = this.last.get(event.execution_id) ?? reportChainGenesis(event.execution_id);
    let fp: string;
    try {
      fp = reportFingerprint({
        eventId: event.event_id,
        executionId: event.execution_id,
        eventType: event.event_type,
        sequence: event.sequence,
        timestamp: event.timestamp,
        durationMs: event.duration_ms,
        payload: payloadBytes(event.payload),
        chainPrev: prev,
      });
    } catch (err) {
      console.warn(`mesedi: report ${event.event_id} left unchained: ${String(err)}`);
      return;
    }
    // The link is recorded before any await, so the chain follows the
    // order chain() is called in even if a slow signer lets another
    // call start meanwhile.
    event.chain_prev = prev;
    this.last.delete(event.execution_id);
    this.last.set(event.execution_id, fp);
    while (this.last.size > MAX_TRACKED_RUNS) {
      const oldest = this.last.keys().next().value as string;
      this.last.delete(oldest);
    }
    if (this.signer) {
      try {
        const { keyId, signature } = await this.signer(hexToBytes(fp));
        event.signature = { key_id: keyId, sig: Buffer.from(signature).toString("base64") };
      } catch (err) {
        console.warn(`mesedi: report ${event.event_id} could not be signed: ${String(err)}`);
      }
    }
  }

  /**
   * The run's declared end for its run-finished message: the last
   * fingerprint sent, signed (over reportChainEndMessage) when signing.
   * Undefined when no report of the run was chained.
   */
  async end(executionId: string): Promise<{ hash: string; signature?: { key_id: string; sig: string } } | undefined> {
    const last = this.last.get(executionId);
    if (last === undefined) return undefined;
    const out: { hash: string; signature?: { key_id: string; sig: string } } = { hash: last };
    if (this.signer) {
      try {
        const { keyId, signature } = await this.signer(reportChainEndMessage(executionId, last));
        out.signature = { key_id: keyId, sig: Buffer.from(signature).toString("base64") };
      } catch (err) {
        console.warn(`mesedi: the end of run ${executionId} could not be signed: ${String(err)}`);
      }
    }
    return out;
  }
}

/**
 * A report signer from a raw 32-byte Ed25519 private key (the seed),
 * for a reporting process kept apart from the agent. Register the
 * matching public key in the dashboard under the same keyId; only a
 * signed-in admin can.
 */
export function ed25519Signer(keyId: string, privateKeySeed: Uint8Array): ReportSigner {
  if (privateKeySeed.length !== 32) {
    throw new Error("an Ed25519 private key seed is 32 bytes");
  }
  // PKCS#8 wrapping of a raw Ed25519 seed (RFC 8410).
  const der = Buffer.concat([Buffer.from("302e020100300506032b657004220420", "hex"), Buffer.from(privateKeySeed)]);
  const key = createPrivateKey({ key: der, format: "der", type: "pkcs8" });
  return (fingerprint: Uint8Array) => ({
    keyId,
    signature: Uint8Array.from(cryptoSign(null, Buffer.from(fingerprint), key)),
  });
}
