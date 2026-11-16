"""Exit behaviour when the backend accepts connections but never answers.

Until 2026-11-15 a short script against such a backend (which is how
api.mesedi.ai behaved while paused) exited after 5 seconds having delivered
nothing and printed nothing (the outside review of 2026-11-15). The rule now, the
same in both SDKs: wait at most ``shutdown_timeout`` at exit, then say in one
warning how many items were not delivered and where they were going.

Each case runs a real child interpreter, because the behaviour under test is
what happens at interpreter exit.
"""

from __future__ import annotations

import http.server
import socket
import subprocess
import sys
import textwrap
import threading
import time
from pathlib import Path

SDK_ROOT = Path(__file__).resolve().parents[1]

SCRIPT = textwrap.dedent(
    """
    import time
    import mesedi
    mesedi.configure(api_key="mesedi_sk_test_exit", base_url="{url}", shutdown_timeout={wait})

    @mesedi.wrap()
    def agent():
        time.sleep({agent_s})
        return "ok"

    print("agent returned:", agent())
    """
)


def _run_child(url: str, wait: float, agent_s: float = 0.0) -> tuple[str, str, float]:
    started = time.monotonic()
    r = subprocess.run(
        [sys.executable, "-c", SCRIPT.format(url=url, wait=wait, agent_s=agent_s)],
        capture_output=True,
        text=True,
        timeout=60,
        cwd=SDK_ROOT,
    )
    return r.stdout, r.stderr, time.monotonic() - started


class _SilentServer:
    """Accepts TCP connections and never sends a byte back."""

    def __init__(self) -> None:
        self.sock = socket.socket(socket.AF_INET, socket.SOCK_STREAM)
        self.sock.bind(("127.0.0.1", 0))
        self.sock.listen(64)
        self.url = f"http://127.0.0.1:{self.sock.getsockname()[1]}"
        self.held: list = []
        self._stop = False
        threading.Thread(target=self._accept, daemon=True).start()

    def _accept(self) -> None:
        while not self._stop:
            try:
                conn, _ = self.sock.accept()
            except OSError:
                return
            self.held.append(conn)

    def close(self) -> None:
        self._stop = True
        for c in self.held:
            c.close()
        self.sock.close()


class _OkHandler(http.server.BaseHTTPRequestHandler):
    def _ok(self) -> None:
        length = int(self.headers.get("Content-Length") or 0)
        self.rfile.read(length)
        body = b'{"ok":true,"accepted":1,"rejected":0}'
        self.send_response(200)
        self.send_header("Content-Type", "application/json")
        self.send_header("Content-Length", str(len(body)))
        self.end_headers()
        self.wfile.write(body)

    do_POST = _ok
    do_PATCH = _ok

    def log_message(self, *args: object) -> None:
        pass


def test_silent_backend_exit_is_bounded_and_reported() -> None:
    server = _SilentServer()
    try:
        out, err, elapsed = _run_child(server.url, wait=1.0)
    finally:
        server.close()
    assert "agent returned: ok" in out
    # Bounded: about one second of waiting, never the 10 s request timeout
    # times four attempts.
    assert elapsed < 6.0, f"exit took {elapsed:.1f}s"
    assert "were not delivered to " + server.url in err, err
    assert "did not answer within 1.0s" in err, err
    assert err.count("were not delivered") == 1, err


def test_bounded_even_when_a_send_was_already_hanging() -> None:
    """The agent runs past the 250 ms flush interval, so the first send is
    already waiting on the silent backend when the agent returns."""
    server = _SilentServer()
    try:
        out, err, elapsed = _run_child(server.url, wait=1.0, agent_s=1.5)
    finally:
        server.close()
    assert "agent returned: ok" in out
    assert elapsed < 7.0, f"exit took {elapsed:.1f}s"
    assert "were not delivered to " + server.url in err, err


def test_shutdown_timeout_is_the_callers_choice() -> None:
    server = _SilentServer()
    try:
        _, err, elapsed = _run_child(server.url, wait=3.0)
    finally:
        server.close()
    assert elapsed >= 3.0, f"exit took {elapsed:.1f}s, wanted at least the configured 3 s"
    assert "did not answer within 3.0s" in err, err


def test_healthy_backend_exits_without_a_warning() -> None:
    httpd = http.server.ThreadingHTTPServer(("127.0.0.1", 0), _OkHandler)
    threading.Thread(target=httpd.serve_forever, daemon=True).start()
    try:
        out, err, _ = _run_child(f"http://127.0.0.1:{httpd.server_address[1]}", wait=5.0)
    finally:
        httpd.shutdown()
    assert "agent returned: ok" in out
    assert "were not delivered" not in err, err
