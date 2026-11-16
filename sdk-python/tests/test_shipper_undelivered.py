"""Direct tests of the helpers added for findings D13 and R22 (2026-11-15):
EventShipper.undelivered(), which counts what an exit leaves behind, and
egress._host_and_numeric_port(), which keeps a port only when it is digits."""

from __future__ import annotations

import logging
import time

import httpx

from mesedi._shipper import EventShipper
from mesedi.egress import _host_and_numeric_port
from mesedi.events import Event, EventType, utcnow_rfc3339


def _event(n: int) -> Event:
    return Event(
        event_id=f"evt-undelivered-{n}",
        execution_id="exec-undelivered",
        event_type=EventType.LLM_CALL,
        sequence=n,
        timestamp=utcnow_rfc3339(),
        payload={"n": n},
    )


def test_undelivered_counts_waiting_items_and_exit_reports_them(caplog) -> None:
    # A long flush interval keeps events waiting in the batch, unsent.
    http = httpx.Client(base_url="http://127.0.0.1:9", timeout=1.0)
    shipper = EventShipper(http=http, flush_interval_ms=60_000, shutdown_timeout=0.0,
                           endpoint="http://127.0.0.1:9")
    for n in range(3):
        shipper.submit_event(_event(n))
    # Let the worker move them off the queue into the batch it is
    # building, so the count below covers that batch, not the queue.
    deadline = time.monotonic() + 2.0
    while not shipper._queue.empty() and time.monotonic() < deadline:
        time.sleep(0.01)
    assert shipper._queue.empty(), "worker never picked the events up"
    assert len(shipper._pending_events) == 3
    assert shipper.undelivered() == 3
    with caplog.at_level(logging.WARNING, logger="mesedi.shipper"):
        shipper.shutdown()
    warned = [r.getMessage() for r in caplog.records if "were not delivered" in r.getMessage()]
    assert len(warned) == 1
    assert "3 item(s) were not delivered to http://127.0.0.1:9" in warned[0]
    http.close()


def test_host_and_numeric_port() -> None:
    assert _host_and_numeric_port("h.example.com:5432") == "h.example.com:5432"
    assert _host_and_numeric_port("h.example.com:SECRET") == "h.example.com"
    assert _host_and_numeric_port("h.example.com:") == "h.example.com"
    assert _host_and_numeric_port("[::1]:8080") == "[::1]:8080"
    assert _host_and_numeric_port("[::1]:x") == "[::1]"
    assert _host_and_numeric_port("h.example.com") == "h.example.com"
