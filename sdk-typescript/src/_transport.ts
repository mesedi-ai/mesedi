/**
 * The shipper's HTTP sender: one request whose socket never keeps the
 * host process alive.
 *
 * Why not fetch: a pending fetch holds Node's event loop open (about
 * 10.7 s for one request to an address that never answers, measured
 * 2026-11-15 on Node 22), and while it does, "beforeExit" never fires,
 * so the client cannot bound how long telemetry delays exit. Against a
 * backend that accepted connections and never answered, a one-second
 * script took 86 seconds to finish (the outside review of 2026-11-15). With
 * an unref'd socket the program exits when its own work is done, and
 * MesediClient.shutdown() decides how long to wait, holding the process
 * open with its own timer for at most `shutdownTimeoutMs`.
 *
 * Internal: not re-exported from index.ts.
 */

import { request as httpRequest, type IncomingMessage } from "node:http";
import { request as httpsRequest } from "node:https";

export interface TransportResponse {
  status: number;
  text: string;
  /** The Retry-After header, in seconds, when the server sent one. */
  retryAfterSeconds?: number;
}

export interface TransportRequest {
  method: string;
  headers: Record<string, string>;
  body: Uint8Array;
  timeoutMs: number;
  signal?: AbortSignal;
}

/** Send one request; resolve with status and body text, reject on network error or timeout. */
export function sendUnref(url: string, req: TransportRequest): Promise<TransportResponse> {
  return new Promise<TransportResponse>((resolve, reject) => {
    const target = new URL(url);
    const send = target.protocol === "https:" ? httpsRequest : httpRequest;
    const r = send(target, {
      method: req.method,
      headers: { ...req.headers, "Content-Length": String(req.body.byteLength) },
      signal: req.signal,
    });
    r.on("socket", (socket) => socket.unref());
    r.setTimeout(req.timeoutMs, () => {
      r.destroy(new Error(`no response within ${req.timeoutMs} ms`));
    });
    r.on("error", reject);
    r.on("response", (res: IncomingMessage) => {
      const chunks: Buffer[] = [];
      res.on("data", (c: Buffer) => chunks.push(c));
      res.on("end", () => {
        const ra = Number(res.headers["retry-after"]);
        resolve({
          status: res.statusCode ?? 0,
          text: Buffer.concat(chunks).toString("utf8"),
          ...(Number.isFinite(ra) && ra >= 0 ? { retryAfterSeconds: ra } : {}),
        });
      });
      res.on("error", reject);
    });
    r.end(req.body);
  });
}
