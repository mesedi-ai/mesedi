/**
 * SSE halt-stream reader — sub-slice 21e (TS port of 21b.2).
 *
 * When `wrap()` is entered with a `budget` configured, the wrap layer
 * spawns a HaltStreamReader that opens an HTTP stream against
 * `GET /executions/{id}/halt-stream` (the SSE endpoint shipped in
 * sub-slice 21b.1). When the backend pushes an `event: halt` frame,
 * the reader parses the reason and calls `tracker.signalRemoteHalt(reason)`.
 * The next halt-safe boundary check inside the wrapped agent then
 * throws `MesediHalt` with `trigger='remote_signal'`.
 *
 * Implementation notes vs the Python version:
 *
 *   - **No thread.** JavaScript is single-threaded; the reader is
 *     just an async function returning a Promise that we kick off
 *     without awaiting. The fetch's underlying I/O runs on Node's
 *     libuv pool — same effective concurrency as a Python daemon
 *     thread.
 *
 *   - **AbortController for stop().** We can't "interrupt" an async
 *     iteration the way Python's `_stop` event lets us bail out of
 *     `iter_lines()`. Instead the controller's signal is wired into
 *     fetch; calling `.stop()` aborts the underlying HTTP connection,
 *     the response stream throws on its next read, and the reader
 *     exits via the catch block.
 *
 *   - **Native fetch + ReadableStream.** Node 18+ ships fetch in
 *     stdlib. We pull bytes off `response.body.getReader()`, decode
 *     UTF-8 chunks via TextDecoder, accumulate into a buffer, and
 *     split on newlines to get SSE lines. The SSE spec uses `\n` or
 *     `\r\n` or `\r` as line separators — we normalize all three.
 *
 *   - **Fail-open posture.** If subscription fails (connect error,
 *     non-200 response, parse error), the reader logs at debug-level
 *     via `console.debug` and exits. The wrapped agent continues
 *     running on whatever local budget it has. Remote halt is a
 *     nice-to-have on top of local budgets, never a hard requirement.
 *
 *   - **One-shot semantics.** After dispatching a halt event, the
 *     reader exits. The backend closes the SSE connection at the
 *     same moment — both sides agree halt is one signal per
 *     subscription.
 *
 * No public API surface: customers don't construct HaltStreamReader.
 * It's used only by wrap.ts.
 */

const CONNECT_TIMEOUT_MS = 5000;

export interface HaltStreamReaderOptions {
  executionId: string;
  baseUrl: string;
  apiKey: string;
  onHalt: (reason: string) => void;
  schemaVersion?: string;
}

export class HaltStreamReader {
  private readonly executionId: string;
  private readonly baseUrl: string;
  private readonly apiKey: string;
  private readonly onHalt: (reason: string) => void;
  private readonly schemaVersion: string;
  private readonly controller: AbortController;
  private started = false;
  private stopped = false;

  constructor(opts: HaltStreamReaderOptions) {
    this.executionId = opts.executionId;
    this.baseUrl = opts.baseUrl.replace(/\/+$/, "");
    this.apiKey = opts.apiKey;
    this.onHalt = opts.onHalt;
    this.schemaVersion = opts.schemaVersion ?? "1";
    this.controller = new AbortController();
  }

  /**
   * Kick off the async read loop. Idempotent — second call is a no-op.
   * Does NOT await — the wrap layer fires this and continues.
   */
  start(): void {
    if (this.started) return;
    this.started = true;
    // Intentionally not awaited; the Promise runs in the background.
    // Catch is inside _run() so we never produce an unhandled rejection.
    void this._run();
  }

  /**
   * Signal the reader to stop and abort any in-flight fetch.
   * Safe to call multiple times.
   */
  stop(): void {
    if (this.stopped) return;
    this.stopped = true;
    try {
      this.controller.abort();
    } catch {
      // ignore — abort can throw on some Node versions if already aborted
    }
  }

  private async _run(): Promise<void> {
    const url = `${this.baseUrl}/executions/${encodeURIComponent(this.executionId)}/halt-stream`;
    // Plain Record rather than DOM-lib HeadersInit so the type
    // resolves under Node typings without pulling in the DOM lib.
    // fetch() accepts a plain string-keyed object for headers.
    const headers: Record<string, string> = {
      Authorization: `Bearer ${this.apiKey}`,
      "X-Mesedi-Schema-Version": this.schemaVersion,
      Accept: "text/event-stream",
    };

    // Connect-timeout is a separate signal so it doesn't kill the
    // long-lived read after connect succeeds. AbortSignal.timeout()
    // is Node 17.3+ / native; AbortSignal.any() is Node 20+ — we
    // compose them manually for broader compatibility (Node 18 LTS).
    const connectTimeout = setTimeout(() => this.controller.abort(), CONNECT_TIMEOUT_MS);
    let resp: Response;
    try {
      resp = await fetch(url, { headers, signal: this.controller.signal });
      clearTimeout(connectTimeout);
    } catch (err) {
      clearTimeout(connectTimeout);
      if (!this.stopped) {
        // Connect failed (network error, DNS, timeout). Fail-open.
        console.debug(
          "[mesedi.halt_stream] connect failed (fail-open)",
          { executionId: this.executionId, error: String(err) },
        );
      }
      return;
    }

    if (resp.status !== 200) {
      console.debug(
        "[mesedi.halt_stream] subscription rejected by backend",
        { executionId: this.executionId, status: resp.status },
      );
      // Drain the body so the connection can close cleanly.
      try { await resp.text(); } catch { /* ignore */ }
      return;
    }
    if (!resp.body) {
      console.debug(
        "[mesedi.halt_stream] response has no body",
        { executionId: this.executionId },
      );
      return;
    }

    const reader = resp.body.getReader();
    const decoder = new TextDecoder("utf-8");
    let buffer = "";
    let currentEvent = "message";
    let dataLines: string[] = [];

    try {
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        if (this.stopped) break;

        buffer += decoder.decode(value, { stream: true });
        // SSE lines are separated by \n, \r\n, or \r. Normalize CRLF
        // to LF, treat remaining \r as line separators, then split
        // on \n.
        buffer = buffer.replace(/\r\n/g, "\n").replace(/\r/g, "\n");
        const lines = buffer.split("\n");
        // Last element is either '' (clean line end) or a partial
        // line that we hold back for the next read.
        buffer = lines.pop() ?? "";

        for (const line of lines) {
          if (this.stopped) break;
          if (line === "") {
            // Blank line dispatches the accumulated event.
            if (currentEvent === "halt" && dataLines.length > 0) {
              this._dispatchHalt(dataLines.join("\n"));
              return; // one-shot — done
            }
            currentEvent = "message";
            dataLines = [];
            continue;
          }
          if (line.startsWith(":")) {
            // Comment frame (`: keepalive`) — ignore.
            continue;
          }
          if (line.startsWith("event:")) {
            currentEvent = line.slice("event:".length).trim() || "message";
          } else if (line.startsWith("data:")) {
            let chunk = line.slice("data:".length);
            if (chunk.startsWith(" ")) chunk = chunk.slice(1);
            dataLines.push(chunk);
          }
        }
      }
    } catch (err) {
      if (!this.stopped) {
        console.debug(
          "[mesedi.halt_stream] reader crashed (fail-open)",
          { executionId: this.executionId, error: String(err) },
        );
      }
    } finally {
      try { reader.releaseLock(); } catch { /* ignore */ }
    }
  }

  private _dispatchHalt(rawData: string): void {
    let reason = "remote halt";
    try {
      const data = JSON.parse(rawData);
      if (data && typeof data === "object" && typeof data.reason === "string") {
        reason = data.reason;
      }
    } catch {
      console.debug(
        "[mesedi.halt_stream] data was not JSON, using generic reason",
        { executionId: this.executionId, raw: rawData.slice(0, 200) },
      );
    }
    try {
      this.onHalt(reason);
    } catch (err) {
      console.warn(
        "[mesedi.halt_stream] onHalt callback threw — halt may not have fired",
        { executionId: this.executionId, error: String(err) },
      );
    }
  }
}
