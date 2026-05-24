/**
 * MesediClient, HTTP client + async event shipper for Node.
 *
 * Zero runtime dependencies: uses Node 18+ native `fetch` for HTTP
 * and the event loop for async dispatch. No background threads (Node
 * doesn't have them in the Python sense); instead a `setInterval`
 * timer drains the queue periodically, and a `process.on("exit")`
 * handler flushes any remaining items at shutdown.
 *
 * Auth: bearer token (`mesedi_sk_...`). Wire-format version pinned
 * at "1" today; future breaking changes bump SCHEMA_VERSION and the
 * backend tightens enforcement.
 *
 * Fail-open: every observation HTTP call wraps its promise in a
 * try/catch, backend errors are logged via console.warn (configurable
 * via a logger injection in a future polish slice) but NEVER bubble
 * back to the wrapped agent code.
 */
import { eventToWire, executionEndPayload, executionStartPayload, } from "./events.js";
export const DEFAULT_BASE_URL = "http://localhost:8080";
export const DEFAULT_TIMEOUT_MS = 10_000;
export const SCHEMA_VERSION = "1";
export class MesediClient {
    apiKey;
    baseUrl;
    timeoutMs;
    flushIntervalMs;
    batchSize;
    maxQueue;
    queue = [];
    pendingEvents = [];
    lastFlush = Date.now();
    droppedCount = 0;
    stopped = false;
    timer = null;
    inflight = Promise.resolve();
    constructor(opts) {
        const apiKey = opts.apiKey ?? process.env["MESEDI_API_KEY"];
        if (!apiKey) {
            throw new Error("Mesedi API key not provided. Pass { apiKey: '...' } or set MESEDI_API_KEY in the environment.");
        }
        if (!apiKey.startsWith("mesedi_sk_")) {
            // Mirror the backend's auth-middleware check, fail loudly on
            // the SDK side rather than letting the backend return 401 on
            // every call.
            throw new Error("api_key must start with 'mesedi_sk_' (received an obviously-malformed key)");
        }
        this.apiKey = apiKey;
        this.baseUrl = (opts.baseUrl ?? process.env["MESEDI_BASE_URL"] ?? DEFAULT_BASE_URL).replace(/\/$/, "");
        this.timeoutMs = opts.timeoutMs ?? DEFAULT_TIMEOUT_MS;
        this.flushIntervalMs = opts.flushIntervalMs ?? 250;
        this.batchSize = opts.batchSize ?? 100;
        this.maxQueue = opts.maxQueue ?? 10_000;
        // Start the periodic flush timer. unref() so it doesn't keep the
        // event loop alive on its own, the process can exit when the
        // user's code is done, and our atexit handler drains anything left.
        this.timer = setInterval(() => this.tick(), this.flushIntervalMs);
        this.timer.unref();
        // Atexit-equivalent: drain on graceful shutdown.
        process.once("beforeExit", () => this.shutdown());
    }
    // ── producer-side API ────────────────────────────────────────────
    submitExecutionStart(execution) {
        this.enqueue({ kind: "create_execution", body: execution });
    }
    submitExecutionEnd(execution) {
        this.enqueue({ kind: "update_execution", body: execution });
    }
    submitEvent(event) {
        this.enqueue({ kind: "event", body: event });
    }
    /**
     * Block-style: returns a promise that resolves when everything
     * submitted SO FAR has been sent. Doesn't wait for items submitted
     * after this call. Useful in tests and end-of-script sync.
     */
    flush(timeoutMs = 5_000) {
        return new Promise((resolve) => {
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
     * Stop the shipper. Drains queue with a short blocking sleep; any
     * items not sent in time are lost.
     */
    async shutdown(timeoutMs = 5_000) {
        if (this.stopped)
            return;
        this.stopped = true;
        if (this.timer) {
            clearInterval(this.timer);
            this.timer = null;
        }
        await this.drainAll(timeoutMs);
    }
    // ── internals ────────────────────────────────────────────────────
    enqueue(item) {
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
                console.warn(`mesedi: shipper queue full, dropped ${this.droppedCount} items so far`);
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
    async tick() {
        if (this.queue.length === 0 && this.pendingEvents.length === 0)
            return;
        // Chain onto the previous tick's promise so we serialize HTTP
        // calls, preserves ordering (POST /executions before PATCH for
        // the same execution).
        this.inflight = this.inflight.then(() => this.processOnce());
        await this.inflight;
    }
    async processOnce() {
        while (this.queue.length > 0) {
            const item = this.queue.shift();
            if (item.kind === "event") {
                this.pendingEvents.push(item.body);
                if (this.pendingEvents.length >= this.batchSize) {
                    await this.flushPendingEvents();
                }
            }
            else {
                // Non-event item, flush pending events first so the PATCH
                // doesn't arrive before its preceding /events POST.
                if (this.pendingEvents.length > 0) {
                    await this.flushPendingEvents();
                }
                if (item.kind === "create_execution") {
                    await this.sendCreateExecution(item.body);
                }
                else if (item.kind === "update_execution") {
                    await this.sendUpdateExecution(item.body);
                }
                else if (item.kind === "barrier") {
                    item.body();
                }
            }
        }
        // Time-based flush: if events have been waiting > flush interval,
        // send them even if the batch isn't full.
        if (this.pendingEvents.length > 0 &&
            Date.now() - this.lastFlush >= this.flushIntervalMs) {
            await this.flushPendingEvents();
        }
    }
    async flushPendingEvents() {
        if (this.pendingEvents.length === 0)
            return;
        const batch = this.pendingEvents.map(eventToWire);
        this.pendingEvents = [];
        this.lastFlush = Date.now();
        await this.sendWithRetry("POST", "/events", batch, `send_events batch of ${batch.length}`);
    }
    async sendCreateExecution(e) {
        await this.sendWithRetry("POST", "/executions", executionStartPayload(e), `create_execution ${e.execution_id}`);
    }
    async sendUpdateExecution(e) {
        await this.sendWithRetry("PATCH", `/executions/${e.execution_id}`, executionEndPayload(e), `update_execution ${e.execution_id}`);
    }
    /**
     * One HTTP call with up to 3 retries on transient failure
     * (network errors, 5xx). 4xx responses drop with a warning, the
     * request is malformed and retrying won't help.
     */
    async sendWithRetry(method, path, body, description) {
        const backoffs = [100, 500, 2000];
        let lastErr = null;
        for (let attempt = 0; attempt <= backoffs.length; attempt++) {
            try {
                const controller = new AbortController();
                const timer = setTimeout(() => controller.abort(), this.timeoutMs);
                const resp = await fetch(this.baseUrl + path, {
                    method,
                    headers: {
                        Authorization: `Bearer ${this.apiKey}`,
                        "Content-Type": "application/json",
                        "X-Mesedi-Schema-Version": SCHEMA_VERSION,
                    },
                    body: JSON.stringify(body),
                    signal: controller.signal,
                });
                clearTimeout(timer);
                if (resp.status < 400)
                    return; // success
                if (resp.status >= 400 && resp.status < 500) {
                    // Permanent failure, retrying won't help.
                    const text = await resp.text().catch(() => "");
                    console.warn(`mesedi: ${description} rejected with ${resp.status}: ${text.slice(0, 200)}`);
                    return;
                }
                lastErr = `HTTP ${resp.status}`;
            }
            catch (err) {
                lastErr = err;
            }
            if (attempt < backoffs.length) {
                await new Promise((r) => setTimeout(r, backoffs[attempt]));
            }
        }
        console.warn(`mesedi: ${description} failed after ${backoffs.length + 1} attempts: ${String(lastErr)}`);
    }
    /**
     * Drain everything currently queued with a deadline. Called from
     * shutdown(). Returns once queue is empty or timeout elapses.
     */
    async drainAll(timeoutMs) {
        const deadline = Date.now() + timeoutMs;
        while ((this.queue.length > 0 || this.pendingEvents.length > 0) &&
            Date.now() < deadline) {
            await this.processOnce();
        }
    }
}
// ── module-level singleton (mirrors Python's mesedi.configure) ──────
let _defaultClient = null;
export function configure(opts = {}) {
    if (_defaultClient) {
        // shutdown previous client cleanly before replacing
        _defaultClient.shutdown().catch(() => undefined);
    }
    _defaultClient = new MesediClient(opts);
    return _defaultClient;
}
export function getClient() {
    if (_defaultClient)
        return _defaultClient;
    // Auto-configure from env if MESEDI_API_KEY is set.
    if (process.env["MESEDI_API_KEY"]) {
        return configure();
    }
    throw new Error("Mesedi is not configured. Call configure({ apiKey: '...' }) or set MESEDI_API_KEY before using wrap()/tool().");
}
export async function flush(timeoutMs = 5_000) {
    return getClient().flush(timeoutMs);
}
//# sourceMappingURL=client.js.map