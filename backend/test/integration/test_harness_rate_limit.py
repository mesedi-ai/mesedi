"""The suite's own handling of rate limits (conftest.py): a direct HTTP
call that gets a 429 waits as Retry-After asks and tries again, and any
other answer is returned at once."""

from __future__ import annotations

from typing import Any, List

import conftest


class _Resp:
    def __init__(self, code: int, retry_after: str = "0") -> None:
        self.status_code = code
        self.headers = {"Retry-After": retry_after}


def test_request_waiting_out_rate_limits(monkeypatch: Any) -> None:
    answers: List[_Resp] = [_Resp(429), _Resp(429), _Resp(200)]
    calls: List[str] = []

    def fake(self: Any, method: str, url: str, *a: Any, **k: Any) -> _Resp:
        calls.append(url)
        return answers.pop(0)

    monkeypatch.setattr(conftest, "_original_session_request", fake)
    monkeypatch.setattr(conftest._time, "sleep", lambda s: None)
    resp = conftest._request_waiting_out_rate_limits(None, "GET", "http://x/")
    assert resp.status_code == 200 and len(calls) == 3

    answers[:] = [_Resp(404)]
    calls.clear()
    assert conftest._request_waiting_out_rate_limits(None, "GET", "http://x/").status_code == 404
    assert len(calls) == 1
