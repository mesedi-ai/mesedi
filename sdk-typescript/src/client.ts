/**
 * MesediClient, HTTP client + async event shipper for Node.
 *
 * Zero runtime dependencies: uses Node's `http`/`https` for the
 * shipper (see _transport.ts for why not fetch), `AsyncLocalStorage`
 * for async context (in wrap()/tool()), and
 * `node:zlib` for opt-in gzip compression of request bodies above 1
 * KB. Below the threshold requests ship uncompressed exactly as
 * v0.2.0 did. No background threads (Node doesn't have them in the
 * Python sense); instead a `setInterval` timer drains the queue
 * periodically, and a `process.once("beforeExit")` handler flushes any
 * remaining items at shutdown, waiting at most `shutdownTimeoutMs`.
 *
 * Why gzip (and why the Python SDK uses zstd): JavaScript zstd
 * encoders are heavy (WASM, ~750 KB, async init) and would break
 * this SDK's tiny-install posture. Node ships gzip synchronously in
 * its standard library, so gzip lets us compress with no new
 * dependency and zero install-size cost. The backend accepts both
 * encodings through the same middleware path.
 *
 * Auth: bearer token (`mesedi_sk_...`). Wire-format version pinned
 * at "1" today; future breaking changes bump SCHEMA_VERSION and the
 * backend tightens enforcement.
 *
 * Fail-open: every observation HTTP call wraps its promise in a
 * try/catch, backend errors are logged via console.warn (configurable
 * via a logger injection in a future polish slice) but NEVER bubble
 * back to the wrapped agent code. Compression also fails open: if
 * the encoder throws, the body ships uncompressed instead.
 */

import { sendUnref } from "./_transport.js";
import { maybeCompress } from "./compress.js";
import { DEFAULT_MAX_PAYLOAD_BYTES, maybeTruncate } from "./truncate.js";
import {
  Event,
  Execution,
  eventToWire,
  executionEndPayload,
  executionStartPayload,
} from "./events.js";

export const DEFAULT_BASE_URL = "https://api.mesedi.ai";
export const DEFAULT_TIMEOUT_MS = 10_000;
/**
 * How long the process waits at exit for undelivered telemetry. Bounded
 * so a Mesedi outage never holds the host program open; configurable so
 * a caller who prefers delivery to a fast exit can wait longer.
 */
export const DEFAULT_SHUTDOWN_TIMEOUT_MS = 5_000;
export const SCHEMA_VERSION = "1";

/**
 * A snapshot of what happened to everything a client submitted.
 * Mirrors the Python SDK's TelemetryStats. Units are HTTP submissions,
 * not individual events; a rejected batch of 7 events counts once.
 */
export interface TelemetryStats {
  /** Submissions the backend accepted. */
  delivered: number;
  /** Refused with a 4xx; never retried. */
  rejected: number;
  /** Gave up after transient failures. */
  retriesExhausted: number;
  /** Never left this process (queue full at submit time). */
  droppedQueueFull: number;
  /** Everything that did not reach the backend, for any reason. */
  lost: number;
  /** True when nothing submitted so far has been lost. */
  complete: boolean;
}

/** Configuration accepted by `configure()`. */
export interface ConfigureOptions {
  /**
   * Bearer token (`mesedi_sk_...`). If omitted, the module reads
   * `MESEDI_API_KEY` from `process.env`.
   */
  apiKey?: string;
  /**
   * Backend URL. Defaults to `MESEDI_BASE_URL` env var, then to
   * "https://api.mesedi.ai" (production). Override to localhost for local dev.
   */
  baseUrl?: string;
  /** Per-request timeout in milliseconds. Default 10s. */
  timeoutMs?: number;
  /**
   * How long the process waits at exit for telemetry not yet delivered,
   * in milliseconds. Default 5s. Telemetry never holds your program
   * open longer than this; anything still undelivered is reported in
   * one console.warn naming the count and the backend.
   */
  shutdownTimeoutMs?: number;
  /** How often the shipper drains the queue. Default 250ms. */
  flushIntervalMs?: number;
  /** Max items per /events POST batch. Default 100. */
  batchSize?: number;
  /** Max queue depth before drop-on-full kicks in. Default 10_000. */
  maxQueue?: number;
  /**
   * Per-event payload cap in bytes. Events whose serialized
   * payload exceeds this size have their longest string fields
   * smart-truncated to fit, with marker fields stamped so downstream
   * readers know what happened. Default 32 KB; pass 0 (or negative)
   * to disable truncation entirely.
   */
  maxPayloadBytes?: number;
}

/**
 * Internal envelope flowing through the shipper queue. Mirrors the
 * Python SDK's `_Item` dataclass.
 */
type ShipItem =
  | { kind: "create_execution"; body: Execution }
  | { kind: "update_execution"; body: Execution }
  | { kind: "event"; body: Event }
  | { kind: "barrier"; body: () => void };

export class MesediClient {
  readonly apiKey: string;
  readonly baseUrl: string;
  readonly timeoutMs: number;
  readonly shutdownTimeoutMs: number;
  private readonly flushIntervalMs: number;
  private readonly batchSize: number;
  private readonly maxQueue: number;
  private readonly maxPayloadBytes: number;

  private queue: ShipItem[] = [];
  private pendingEvents: Event[] = [];
  private lastFlush: number = Date.now();

  // Delivery accounting, mirroring the Python SDK's TelemetryStats.
  // Until 2026-09-08 only queue-full drops were counted; a permanent
  // 4xx rejection and a retry-exhausted send were each logged once and
  // then counted nowhere, so no caller could learn telemetry was lost.
  // flush() answers "did the queue drain", which is true even when
  // every submission was refused. Units are HTTP submissions, not
  // events: a rejected batch of 7 counts once.
  private droppedCount = 0;
  private deliveredCount = 0;
  private rejectedCount = 0;
  private exhaustedCount = 0;
  private stopped = false;
  private timer: NodeJS.Timeout | null = null;
  private inflight: Promise<void> = Promise.resolve();
  // Items of the send in progress, so an exit can count what it leaves
  // behind; and the controllers that let shutdown() cut sends short.
  private inflightItems = 0;
  private abandoned = false;
  private readonly controllers = new Set<AbortController>();

  constructor(opts: ConfigureOptions) {
    const apiKey = opts.apiKey ?? process.env["MESEDI_API_KEY"];
    if (!apiKey) {
      throw new Error(
        "Mesedi API key not provided. Pass { apiKey: '...' } or set MESEDI_API_KEY in the environment.",
      );
    }
    if (!apiKey.startsWith("mesedi_sk_")) {
      // Mirror the backend's auth-middleware check, fail loudly on
      // the SDK side rather than letting the backend return 401 on
      // every call.
      throw new Error(
        "api_key must start with 'mesedi_sk_' (received an obviously-malformed key)",
      );
    }
    this.apiKey = apiKey;
    this.baseUrl = (opts.baseUrl ?? process.env["MESEDI_BASE_URL"] ?? DEFAULT_BASE_URL).replace(/\/$/, "");
    this.timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
    this.shutdownTimeoutMs = opts.shutdownTimeoutMs ?? DEFAULT_SHUTDOWN_TIMEOUT_MS;
    this.flushIntervalMs = opts.flushIntervalMs ?? 250;
    this.batchSize = opts.batchSize ?? 100;
    this.maxQueue = opts.maxQueue ?? 10_000;
    this.maxPayloadBytes = opts.maxPayloadBytes ?? DEFAULT_MAX_PAYLOAD_BYTES;

    // Start the periodic flush timer. unref() so it doesn't keep the
    // event loop alive on its own, the process can exit when the
    // user's code is done, and our atexit handler drains anything left.
    this.timer = setInterval(() => this.tick(), this.flushIntervalMs);
    this.timer.unref();

    // Atexit-equivalent: drain on graceful shutdown.
    process.once("beforeExit", () => this.shutdown());
  }

  // ── producer-side API ────────────────────────────────────────────

  submitExecutionStart(execution: Execution): void {
    this.enqueue({ kind: "create_execution", body: execution });
  }

  submitExecutionEnd(execution: Execution): void {
    this.enqueue({ kind: "update_execution", body: execution });
  }

  submitEvent(event: Event): void {
    // Apply the per-event payload cap BEFORE the event enters
    // the queue. Truncating here keeps the queue's memory footprint
    // bounded even when a customer accidentally captures a multi-MB
    // blob; downstream retries reuse the already-trimmed payload
    // instead of re-running truncation on every attempt.
    if (this.maxPayloadBytes > 0) {
      event.payload = maybeTruncate(event.payload, this.maxPayloadBytes);
    }
    this.enqueue({ kind: "event", body: event });
  }

  /**
   * What happened to everything this client submitted so far.
   *
   * flush() answers "did the queue drain", and that is true even when
   * every submission was refused: this SDK is fail-open by design, so
   * a rejected batch is logged once and the agent carries on. Right
   * for the agent, wrong to mistake for delivery. Read this after
   * flushing when it matters whether the record actually arrived:
   *
   *     await mesedi.flush();
   *     if (!mesedi.stats().complete) { ...the record is incomplete }
   */
  stats(): TelemetryStats {
    const lost =
      this.rejectedCount + this.exhaustedCount + this.droppedCount;
    return {
      delivered: this.deliveredCount,
      rejected: this.rejectedCount,
      retriesExhausted: this.exhaustedCount,
      droppedQueueFull: this.droppedCount,
      lost,
      complete: lost === 0,
    };
  }

  /**
   * Block-style: returns a promise that resolves when everything
   * submitted SO FAR has been sent. Doesn't wait for items submitted
   * after this call. Useful in tests and end-of-script sync.
   */
  flush(timeoutMs = 5_000): Promise<boolean> {
    return new Promise<boolean>((resolve) => {
      const timer = setTimeout(() => resolve(false), timeoutMs);
      this.enqueue({
        kind: "barrier",
        body: () => {
          clearTimeout(timer);
          resolve(true);
        },
      });
    });
  }

  /**
   * Stop the shipper. Drains the queue for at most `timeoutMs`
   * (default: the configured `shutdownTimeoutMs`, 5s). Items not sent
   * by then are lost, and one console.warn says how many and where
   * they were going. Until 2026-11-15 a backend that accepted
   * connections but never answered held a one-second script open for
   * 86 seconds (outside review finding R22).
   */
  async shutdown(timeoutMs = this.shutdownTimeoutMs): Promise<void> {
    if (this.stopped) return;
    this.stopped = true;
    if (this.timer) {
      clearInterval(this.timer);
      this.timer = null;
    }
    let deadlineHit = false;
    let deadlineTimer: NodeJS.Timeout | undefined;
    // Deliberately not unref'd: this timer is what keeps the process
    // alive while it waits, and it is the only thing that does, so the
    // wait can never exceed timeoutMs.
    const deadline = new Promise<void>((resolve) => {
      deadlineTimer = setTimeout(() => {
        deadlineHit = true;
        resolve();
      }, timeoutMs);
    });
    const drain = this.inflight.then(() => this.drainAll(timeoutMs));
    await Promise.race([drain, deadline]);
    clearTimeout(deadlineTimer);
    if (!deadlineHit) return;
    const left = this.undelivered();
    this.abandoned = true;
    for (const c of this.controllers) c.abort();
    if (left > 0) {
      console.warn(
        `mesedi: ${left} item(s) were not delivered to ${this.baseUrl} before exit: ` +
          `the backend did not answer within ${(timeoutMs / 1000).toFixed(1)}s. ` +
          `Pass a larger shutdownTimeoutMs to configure() to wait longer.`,
      );
    }
  }

  /**
   * Items submitted but not yet delivered, rejected or given up on:
   * still queued, waiting in the events batch, or in the send in
   * progress. Barriers are not items.
   */
  private undelivered(): number {
    return (
      this.queue.filter((i) => i.kind !== "barrier").length +
      this.pendingEvents.length +
      this.inflightItems
    );
  }

  // ── internals ────────────────────────────────────────────────────

  private enqueue(item: ShipItem): void {
    if (this.stopped) {
      // Even after shutdown is invoked, the user's main code might
      // continue running for a short tick. Best-effort: drop silently
      // rather than throw, we're already past the contractual
      // observation window.
      return;
    }
    if (this.queue.length >= this.maxQueue) {
      this.droppedCount++;
      if (this.droppedCount === 1 || this.droppedCount % 100 === 0) {
        console.warn(
          `mesedi: shipper queue full, dropped ${this.droppedCount} items so far`,
        );
      }
      return;
    }
    this.queue.push(item);
  }

  /**
   * Single tick of the shipper loop, called by setInterval. Drains
   * the queue, dispatches non-event items, and flushes the pending-
   * events batch if it's full OR the flush interval has elapsed.
   */
  private async tick(): Promise<void> {
    if (this.queue.length === 0 && this.pendingEvents.length === 0) return;
    // Chain onto the previous tick's promise so we serialize HTTP
    // calls, preserves ordering (POST /executions before PATCH for
    // the same execution).
    this.inflight = this.inflight.then(() => this.processOnce());
    await this.inflight;
  }

  private async processOnce(): Promise<void> {
    while (this.queue.length > 0) {
      const item = this.queue.shift()!;
      if (item.kind === "event") {
        this.pendingEvents.push(item.body);
        if (this.pendingEvents.length >= this.batchSize) {
          await this.flushPendingEvents();
        }
      } else {
        // Non-event item, flush pending events first so the PATCH
        // doesn't arrive before its preceding /events POST.
        if (this.pendingEvents.length > 0) {
          await this.flushPendingEvents();
        }
        if (item.kind === "create_execution") {
          await this.sendCreateExecution(item.body);
        } else if (item.kind === "update_execution") {
          await this.sendUpdateExecution(item.body);
        } else if (item.kind === "barrier") {
          item.body();
        }
      }
    }
    // Time-based flush: if events have been waiting > flush interval,
    // send them even if the batch isn't full.
    if (
      this.pendingEvents.length > 0 &&
      Date.now() - this.lastFlush >= this.flushIntervalMs
    ) {
      await this.flushPendingEvents();
    }
  }

  private async flushPendingEvents(): Promise<void> {
    if (this.pendingEvents.length === 0) return;
    const batch = this.pendingEvents.map(eventToWire);
    this.pendingEvents = [];
    this.lastFlush = Date.now();
    this.inflightItems = batch.length;
    try {
      await this.sendWithRetry("POST", "/events", batch, `send_events batch of ${batch.length}`);
    } finally {
      this.inflightItems = 0;
    }
  }

  private async sendCreateExecution(e: Execution): Promise<void> {
    this.inflightItems = 1;
    try {
      await this.sendWithRetry(
        "POST",
        "/executions",
        executionStartPayload(e),
        `create_execution ${e.execution_id}`,
      );
    } finally {
      this.inflightItems = 0;
    }
  }

  private async sendUpdateExecution(e: Execution): Promise<void> {
    this.inflightItems = 1;
    try {
      await this.sendWithRetry(
        "PATCH",
        `/executions/${e.execution_id}`,
        executionEndPayload(e),
        `update_execution ${e.execution_id}`,
      );
    } finally {
      this.inflightItems = 0;
    }
  }

  /**
   * One HTTP call with up to 3 retries on transient failure
   * (network errors, 5xx). 4xx responses drop with a warning, the
   * request is malformed and retrying won't help.
   */
  private async sendWithRetry(
    method: string,
    path: string,
    body: unknown,
    description: string,
  ): Promise<void> {
    // Serialize once, outside the retry loop, so retries do not
    // repeat the JSON.stringify or compression cost.
    const rawBody = new TextEncoder().encode(JSON.stringify(body));
    const { body: finalBody, extraHeaders } = maybeCompress(rawBody);

    const backoffs = [100, 500, 2000];
    let lastErr: unknown = null;
    for (let attempt = 0; attempt <= backoffs.length; attempt++) {
      // After shutdown gave up waiting, stop: its warning already
      // counted this send among the undelivered.
      if (this.abandoned) return;
      const controller = new AbortController();
      this.controllers.add(controller);
      try {
        const resp = await sendUnref(this.baseUrl + path, {
          method,
          headers: {
            Authorization: `Bearer ${this.apiKey}`,
            "Content-Type": "application/json",
            "X-Mesedi-Schema-Version": SCHEMA_VERSION,
            ...extraHeaders,
          },
          body: finalBody,
          timeoutMs: this.timeoutMs,
          signal: controller.signal,
        });
        if (resp.status < 400) {
          this.deliveredCount++;
          return; // success
        }
        if (resp.status >= 400 && resp.status < 500) {
          // Permanent failure, retrying won't help.
          this.rejectedCount++;
          console.warn(
            `mesedi: ${description} rejected with ${resp.status}: ${resp.text.slice(0, 200)}`,
          );
          return;
        }
        lastErr = `HTTP ${resp.status}`;
      } catch (err) {
        lastErr = err;
      } finally {
        this.controllers.delete(controller);
      }
      if (this.abandoned) return;
      if (attempt < backoffs.length) {
        // unref'd, like the socket: a retry pause never holds the
        // process open on its own.
        await new Promise<void>((r) => setTimeout(r, backoffs[attempt]).unref());
      }
    }
    this.exhaustedCount++;
    console.warn(
      `mesedi: ${description} failed after ${backoffs.length + 1} attempts: ${String(lastErr)}`,
    );
  }

  /**
   * Drain everything currently queued with a deadline. Called from
   * shutdown(). Returns once queue is empty or timeout elapses.
   */
  private async drainAll(timeoutMs: number): Promise<void> {
    const deadline = Date.now() + timeoutMs;
    while (
      (this.queue.length > 0 || this.pendingEvents.length > 0) &&
      Date.now() < deadline
    ) {
      await this.processOnce();
    }
  }
}

// ── module-level singleton (mirrors Python's mesedi.configure) ──────

let _defaultClient: MesediClient | null = null;

export function configure(opts: ConfigureOptions = {}): MesediClient {
  if (_defaultClient) {
    // shutdown previous client cleanly before replacing
    _defaultClient.shutdown().catch(() => undefined);
  }
  _defaultClient = new MesediClient(opts);
  return _defaultClient;
}

export function getClient(): MesediClient {
  if (_defaultClient) return _defaultClient;
  // Auto-configure from env if MESEDI_API_KEY is set.
  if (process.env["MESEDI_API_KEY"]) {
    return configure();
  }
  throw new Error(
    "Mesedi is not configured. Call configure({ apiKey: '...' }) or set MESEDI_API_KEY before using wrap()/tool().",
  );
}

export async function flush(timeoutMs = 5_000): Promise<boolean> {
  return getClient().flush(timeoutMs);
}

/**
 * Module-level helper: delivery accounting for the default client.
 * The end-of-script pair is flush() then stats().complete.
 */
export function stats(): TelemetryStats {
  return getClient().stats();
}
