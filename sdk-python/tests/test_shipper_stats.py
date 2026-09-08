"""TelemetryStats: the shipper must be able to say what was lost.

Written from a real session on 2026-09-08. A synthetic-customer run
with a bad API key had every submission refused with 401; the SDK
logged each refusal and carried on, fail-open by design, and flush()
truthfully reported the queue drained. The session summary concluded
everything succeeded. Nothing in the SDK could contradict it, because
rejections were logged once and counted nowhere.

These tests drive a real EventShipper through a fake httpx transport
for each of the four outcomes: accepted, permanently refused, retried
into exhaustion, and never sent at all. No network, no sleeps beyond
the shipper's own cadence.
"""

from __future__ import annotations

import httpx
import pytest

from mesedi._shipper import EventShipper, TelemetryStats
from mesedi.events import Event


def _shipper_with(handler, **kw):
    transport = httpx.MockTransport(handler)
    http = httpx.Client(transport=transport, base_url="http://fake")
    defaults = dict(flush_interval_ms=10, max_retries=1)
    defaults.update(kw)
    return EventShipper(http, **defaults)


def _event() -> Event:
    return Event(
        event_id="evt-stats-test-1",
        execution_id="exec-stats-test",
        event_type="llm_call",
        sequence=1,
        payload={"note": "stats fixture"},
    )


def test_accepted_submissions_count_as_delivered():
    def ok(request):
        return httpx.Response(200, json={"ok": True})

    s = _shipper_with(ok)
    s.submit_event(_event())
    assert s.flush(timeout=5.0)
    st = s.stats()
    assert st.delivered >= 1
    assert st.lost == 0
    assert st.complete
    s.shutdown()


def test_a_401_is_counted_not_just_logged():
    def refuse(request):
        return httpx.Response(401, json={"ok": False, "error": "API key not recognized"})

    s = _shipper_with(refuse)
    s.submit_event(_event())
    assert s.flush(timeout=5.0), "fail-open: a refused queue still drains"
    st = s.stats()
    assert st.rejected >= 1, (
        "a permanent 4xx was logged and counted nowhere; that is the exact "
        "defect this type exists to close"
    )
    assert not st.complete
    s.shutdown()


def test_retries_exhausted_is_its_own_number():
    def always_500(request):
        return httpx.Response(500, text="boom")

    s = _shipper_with(always_500, max_retries=0)
    s.submit_event(_event())
    assert s.flush(timeout=10.0)
    st = s.stats()
    assert st.retries_exhausted >= 1
    assert st.rejected == 0, (
        "a transient failure that ran out of retries is not a rejection; "
        "conflating them tells an operator to fix the wrong thing"
    )
    assert not st.complete
    s.shutdown()


def test_flush_true_must_not_be_read_as_delivery():
    """The pair of answers that started this: drained yes, arrived no."""

    def refuse(request):
        return httpx.Response(401, json={"ok": False})

    s = _shipper_with(refuse)
    s.submit_event(_event())
    drained = s.flush(timeout=5.0)
    st = s.stats()
    assert drained and st.lost > 0, (
        "this combination IS the bug scenario: flush says True while "
        "everything was refused. stats() existing is what makes the "
        "combination detectable by a caller"
    )
    s.shutdown()


def test_stats_is_a_frozen_snapshot():
    st = TelemetryStats(delivered=3, rejected=1, retries_exhausted=1,
                        dropped_queue_full=2)
    assert st.lost == 4
    assert not st.complete
    with pytest.raises(Exception):
        st.delivered = 99  # type: ignore[misc]
