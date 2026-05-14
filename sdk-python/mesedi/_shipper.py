"""
Background event shipper — async, ordered, retry-on-transient-failure.

The shipper owns a single daemon thread that pulls items from a thread-
safe queue and dispatches them to the Mesedi backend. Items come in
three flavors:

  - ``create_execution``: POST /executions
  - ``update_execution``: PATCH /executions/{id}
  - ``event``:            buffered and POSTed as a JSON array when the
                          batch fills or a flush interval elapses

**Ordering guarantee.** Items are processed in FIFO order. To preserve
the invariant that PATCH /executions/{id} never arrives before its
corresponding POST /executions (which would 404 on the backend), any
events pending in the batch are flushed whenever a non-event item is
about to be dispatched. This means a worst-case "fire 100 events then
a PATCH" produces 2 HTTP calls (one batched events, one PATCH); a
best-case "fire 100 events" produces 1 HTTP call.

**Retry.** Transient failures (network errors, 5xx) retry up to 3 times
with exponential backoff (0.1s, 0.5s, 2.0s). Permanent failures (4xx)
drop the item with a warning log — retrying a malformed request never
helps.

**Shutdown.** ``atexit`` registers a shutdown handler that flushes the
queue with a timeout (default 5s). Daemon thread would otherwise die
with the interpreter if the shutdown handler doesn't run (e.g., kill -9,
unhandled crash before atexit can fire) — at that point unflushed events
are lost. In normal operation, queue-to-flush latency is single-digit
milliseconds, so the loss window is small.

**Backpressure.** The queue is bounded (default 10,000 items). When
full, new submissions silently drop with a periodic warning log. Better
to drop a few events than to leak unbounded memory on a backend outage.
"""

from __future__ import annotations

import atexit
import logging
import queue
import threading
import time
from dataclasses import dataclass
from typing import Any, List, Optional, Union

import httpx

from mesedi.events import Event, Execution

logger = logging.getLogger("mesedi.shipper")


@dataclass
class _Item:
    """Internal envelope flowing through the shipper queue.

    The ``body`` field type depends on ``kind``:
      - ``create_execution`` / ``update_execution`` → Execution
      - ``event`` → Event
      - ``barrier`` → threading.Event (signaled when the shipper has
        drained everything queued before this item; used by flush())
    """

    kind: str
    body: Any  # Execution | Event | threading.Event


class EventShipper:
    """Background async shipper for backend events and executions.

    One shipper per MesediClient. Owns one daemon thread. Thread-safe
    for concurrent ``submit_*()`` calls from any number of producer
    threads.
    """

    def __init__(
        self,
        http: httpx.Client,
        flush_interval_ms: int = 250,
        batch_size: int = 100,
        max_queue: int = 10_000,
        max_retries: int = 3,
    ):
        self._http = http
        self._flush_interval = flush_interval_ms / 1000.0
        self._batch_size = batch_size
        self._max_queue = max_queue
        self._max_retries = max_retries

        self._queue: "queue.Queue[_Item]" = queue.Queue(maxsize=max_queue)
        self._stop = threading.Event()
        self._dropped_count = 0  # for diagnostics

        self._thread = threading.Thread(
            target=self._run,
            name="mesedi-shipper",
            daemon=True,
        )
        self._thread.start()
        atexit.register(self.shutdown)

    # ── producer-side API ────────────────────────────────────────────

    def submit_execution_start(self, execution: Execution) -> None:
        self._submit(_Item(kind="create_execution", body=execution))

    def submit_execution_end(self, execution: Execution) -> None:
        self._submit(_Item(kind="update_execution", body=execution))

    def submit_event(self, event: Event) -> None:
        self._submit(_Item(kind="event", body=event))

    def _submit(self, item: _Item) -> None:
        try:
            self._queue.put_nowait(item)
        except queue.Full:
            self._dropped_count += 1
            # Log only the first drop and every 100th drop afterward, to
            # avoid log-spam under sustained pressure.
            if self._dropped_count == 1 or self._dropped_count % 100 == 0:
                logger.warning(
                    "mesedi: shipper queue full, dropped %d items so far",
                    self._dropped_count,
                )

    def flush(self, timeout: float = 5.0) -> bool:
        """Block until the shipper has drained everything queued so far.

        Inserts a barrier item; returns True if the barrier was reached
        within ``timeout`` seconds, False otherwise. Useful in tests and
        before-exit synchronization where you want to ensure events have
        actually landed at the backend before the caller proceeds.
        """
        signal = threading.Event()
        try:
            self._queue.put_nowait(_Item(kind="barrier", body=signal))
        except queue.Full:
            return False
        return signal.wait(timeout=timeout)

    # ── shutdown ──────────────────────────────────────────────────────

    def shutdown(self, timeout: float = 5.0) -> None:
        """Stop the worker thread; flush remaining items with timeout.

        Safe to call multiple times. Registered with ``atexit`` at
        construction time, so an explicit call is normally unnecessary —
        but explicit ``client.close()`` will trigger this immediately
        rather than waiting for interpreter shutdown.
        """
        if self._stop.is_set():
            return
        self._stop.set()
        # The worker may be blocked on queue.get() with a timeout up to
        # flush_interval; just join() with a generous timeout and let
        # the daemon-thread fallback handle the unlikely case where the
        # worker is stuck mid-retry on a slow backend.
        self._thread.join(timeout=timeout)

    # ── worker thread ─────────────────────────────────────────────────

    def _run(self) -> None:
        """Main loop: drain queue, batch events, dispatch."""
        pending_events: List[Event] = []
        last_flush = time.monotonic()

        while True:
            # Termination condition: stop flag is set AND queue is empty.
            # We do NOT stop the instant stop_set is true — we keep
            # draining whatever is already queued, so an explicit
            # shutdown() doesn't drop in-flight work.
            if self._stop.is_set() and self._queue.empty():
                break

            timeout = max(
                0.0,
                self._flush_interval - (time.monotonic() - last_flush),
            )
            try:
                item = self._queue.get(timeout=timeout)
            except queue.Empty:
                # Time-based flush window elapsed; flush any pending events.
                if pending_events:
                    self._send_events(pending_events)
                    pending_events = []
                last_flush = time.monotonic()
                continue

            if item.kind == "event":
                assert isinstance(item.body, Event)
                pending_events.append(item.body)
                if len(pending_events) >= self._batch_size:
                    self._send_events(pending_events)
                    pending_events = []
                    last_flush = time.monotonic()
            elif item.kind == "barrier":
                # Flush any pending events FIRST so the barrier represents
                # "everything submitted before me is done".
                if pending_events:
                    self._send_events(pending_events)
                    pending_events = []
                    last_flush = time.monotonic()
                assert isinstance(item.body, threading.Event)
                item.body.set()
            else:
                # Non-event item — flush pending events FIRST to preserve
                # ordering (a PATCH must not arrive before its preceding
                # events for the same execution).
                if pending_events:
                    self._send_events(pending_events)
                    pending_events = []
                    last_flush = time.monotonic()

                assert isinstance(item.body, Execution)
                if item.kind == "create_execution":
                    self._send_create_execution(item.body)
                elif item.kind == "update_execution":
                    self._send_update_execution(item.body)
                else:
                    logger.warning("mesedi: unknown item kind %r", item.kind)

        # Final flush on shutdown.
        if pending_events:
            self._send_events(pending_events)

    # ── dispatchers with retry ────────────────────────────────────────

    def _send_create_execution(self, execution: Execution) -> None:
        self._send_with_retry(
            method="POST",
            url="/executions",
            json_body=execution.start_payload(),
            description=f"create_execution {execution.execution_id}",
        )

    def _send_update_execution(self, execution: Execution) -> None:
        self._send_with_retry(
            method="PATCH",
            url=f"/executions/{execution.execution_id}",
            json_body=execution.end_payload(),
            description=f"update_execution {execution.execution_id}",
        )

    def _send_events(self, events: List[Event]) -> None:
        body = [e.to_dict() for e in events]
        self._send_with_retry(
            method="POST",
            url="/events",
            json_body=body,
            description=f"send_events batch of {len(events)}",
        )

    def _send_with_retry(
        self,
        method: str,
        url: str,
        json_body: Any,
        description: str,
    ) -> None:
        """Issue one HTTP call with up-to-3 retries on transient failure.

        Returns silently on success or on permanent failure (4xx); logs
        a warning on permanent failure or exhausted retries.
        """
        backoffs = [0.1, 0.5, 2.0]
        last_err: Any = None
        for attempt in range(self._max_retries + 1):
            try:
                r = self._http.request(method, url, json=json_body)
                if r.status_code < 400:
                    return  # success
                if 400 <= r.status_code < 500:
                    # Permanent failure: retrying won't help.
                    logger.warning(
                        "mesedi: %s rejected with %d: %s",
                        description,
                        r.status_code,
                        r.text[:200],
                    )
                    return
                # 5xx — transient, retry.
                last_err = f"HTTP {r.status_code}"
            except Exception as exc:
                last_err = exc

            if attempt < self._max_retries:
                time.sleep(backoffs[attempt])

        logger.warning(
            "mesedi: %s failed after %d attempts: %s",
            description,
            self._max_retries + 1,
            last_err,
        )
