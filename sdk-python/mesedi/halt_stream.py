"""
SSE halt-stream reader, sub-slice 21b.2.

When `@mesedi.wrap` is entered with a `budget=Budget(...)` set, the
wrap layer spawns a `HaltStreamReader` daemon thread for the lifetime
of the execution. The thread opens an HTTP stream to
`GET /executions/{id}/halt-stream` (a Server-Sent Events endpoint
shipped in sub-slice 21b.1). When the backend pushes an
`event: halt` frame, the reader parses the reason and calls
`tracker.signal_remote_halt(reason)`. The next halt-safe-boundary
check inside the wrapped agent code then raises `MesediHalt` with
`trigger="remote_signal"`.

Design points:

  - **Daemon thread.** The reader is `daemon=True` so it can't prevent
    process shutdown. When `@wrap` exits (normally or via halt /
    crash), it calls `reader.stop()` which sets a stop event and
    closes the streaming HTTP connection, the reader thread sees
    the close and exits cleanly.

  - **Fail-open posture.** If the SSE subscription fails (backend
    unreachable, 4xx/5xx response, network blip), the reader logs and
    returns. It does NOT crash the wrapped agent. Remote-halt is a
    nice-to-have on top of the local-budget primitive, if it doesn't
    work, the wrap()'d agent continues to run with whatever local
    budget it was given.

  - **One-shot semantics.** After the reader fires
    `tracker.signal_remote_halt()`, it returns. The backend closes
    the SSE connection after the same event; both sides agree the
    halt is a single signal per subscription.

  - **No SDK API surface.** `HaltStreamReader` is internal, customers
    don't construct it directly. It's exposed only to `wrap.py`'s
    halt-safe setup code.
"""

from __future__ import annotations

import json
import logging
import threading
from typing import Callable, Optional

import httpx

logger = logging.getLogger("mesedi.halt_stream")

# Connect timeout for the SSE stream. We don't want to hang the
# wrapped agent's startup if the backend is unreachable, fail-fast,
# fall back to local-budget-only operation.
_CONNECT_TIMEOUT_SECONDS = 5.0


class HaltStreamReader:
    """Background thread that subscribes to the backend's SSE
    halt-stream for one execution and signals remote halts.

    Owned by the `@wrap` invocation that created it. Started after
    the execution-id is allocated and before the wrapped function
    runs; stopped after the wrapped function returns (or halts, or
    crashes).
    """

    def __init__(
        self,
        execution_id: str,
        base_url: str,
        api_key: str,
        on_halt: Callable[[str], None],
        schema_version: str = "1",
    ):
        self._execution_id = execution_id
        self._base_url = base_url.rstrip("/")
        self._api_key = api_key
        self._on_halt = on_halt
        self._schema_version = schema_version
        self._stop = threading.Event()
        self._thread: Optional[threading.Thread] = None
        # Hold the open httpx Response so .stop() can close it
        # promptly, otherwise the reader thread blocks in
        # iter_lines() until the keepalive triggers or the max-
        # lifetime times out.
        self._response_lock = threading.Lock()
        self._open_response: Optional[httpx.Response] = None

    def start(self) -> None:
        """Spawn the reader thread. Idempotent, second call is a no-op."""
        if self._thread is not None:
            return
        self._thread = threading.Thread(
            target=self._run,
            daemon=True,
            name=f"mesedi-halt-stream-{self._execution_id[:12]}",
        )
        self._thread.start()

    def stop(self) -> None:
        """Signal the reader to exit and close any open HTTP stream.

        Safe to call multiple times. Does NOT join the thread , 
        daemon=True means it dies with the process; we just unblock
        it so it exits gracefully.
        """
        self._stop.set()
        with self._response_lock:
            resp = self._open_response
            self._open_response = None
        if resp is not None:
            try:
                resp.close()
            except Exception:  # noqa: BLE001, defensive cleanup
                pass

    # ── Internal ────────────────────────────────────────────────

    def _run(self) -> None:
        url = f"{self._base_url}/executions/{self._execution_id}/halt-stream"
        headers = {
            "Authorization": f"Bearer {self._api_key}",
            "X-Mesedi-Schema-Version": self._schema_version,
            "Accept": "text/event-stream",
        }
        # Connect timeout caps how long we'll wait to establish the
        # subscription. Read timeout=None lets the stream stay open
        # indefinitely, the backend's `: keepalive` comments every
        # 15s keep the TCP connection warm; without that we'd hit a
        # read timeout. write/pool timeouts are conservative because
        # neither path matters for a single GET stream.
        timeout = httpx.Timeout(
            connect=_CONNECT_TIMEOUT_SECONDS,
            read=None,
            write=10.0,
            pool=10.0,
        )
        try:
            with httpx.stream("GET", url, headers=headers, timeout=timeout) as resp:
                if resp.status_code != 200:
                    logger.debug(
                        "halt-stream subscription rejected by backend",
                        extra={
                            "execution_id": self._execution_id,
                            "status_code": resp.status_code,
                        },
                    )
                    return
                with self._response_lock:
                    self._open_response = resp

                # SSE parser. We track the current event-type
                # (defaults to "message") and accumulate data lines
                # until a blank line dispatches the event. The
                # backend only ever sends one event type we care
                # about, "halt", so the parser stays tiny.
                current_event = "message"
                data_buf: list[str] = []
                for line in resp.iter_lines():
                    if self._stop.is_set():
                        return
                    # iter_lines yields raw text, no trailing newline.
                    if not line:
                        # Blank line dispatches the accumulated event.
                        if current_event == "halt" and data_buf:
                            self._dispatch_halt("\n".join(data_buf))
                            return  # one-shot, done
                        current_event = "message"
                        data_buf = []
                        continue
                    if line.startswith(":"):
                        # Comment / keepalive, ignore.
                        continue
                    if line.startswith("event:"):
                        current_event = line[len("event:"):].strip() or "message"
                    elif line.startswith("data:"):
                        # `data:` may have a leading space per spec.
                        chunk = line[len("data:"):]
                        if chunk.startswith(" "):
                            chunk = chunk[1:]
                        data_buf.append(chunk)
        except httpx.HTTPError as exc:
            logger.debug(
                "halt-stream connection failed (fail-open)",
                extra={"execution_id": self._execution_id, "error": str(exc)},
            )
        except Exception as exc:  # noqa: BLE001, defensive
            logger.debug(
                "halt-stream reader crashed (fail-open)",
                extra={"execution_id": self._execution_id, "error": str(exc)},
            )
        finally:
            with self._response_lock:
                self._open_response = None

    def _dispatch_halt(self, raw_data: str) -> None:
        """Parse the data payload and signal the halt back to the wrap.

        Defensive: malformed JSON falls back to a generic reason
        string. The reader's job is to fire the halt, we never want
        a parse glitch to silently swallow the signal.
        """
        reason = "remote halt"
        try:
            data = json.loads(raw_data)
            if isinstance(data, dict) and isinstance(data.get("reason"), str):
                reason = data["reason"]
        except json.JSONDecodeError:
            logger.debug(
                "halt-stream data was not JSON, using generic reason",
                extra={"execution_id": self._execution_id, "raw": raw_data[:200]},
            )
        try:
            self._on_halt(reason)
        except Exception as exc:  # noqa: BLE001, defensive
            logger.warning(
                "halt-stream on_halt callback raised, halt may not have fired",
                extra={"execution_id": self._execution_id, "error": str(exc)},
            )
