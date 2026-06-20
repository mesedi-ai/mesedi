"""
conftest.py — pytest fixtures that boot the real mesedi-api binary
and mint a test project + API key for the suite to drive.

The whole suite shares one binary instance (scope="session") to keep
the test loop fast: spinning the binary up costs about a second, and
each test resets its own state by using a fresh execution_id rather
than tearing the backend down.
"""

from __future__ import annotations

import os
import socket
import subprocess
import sys
import time
from dataclasses import dataclass
from pathlib import Path
from typing import Iterator

import pytest
import requests


# Repository roots, derived from this file's location. The integration
# tests live at backend/test/integration/, so backend/ is two parents
# up, and the repo root is three parents up.
HERE = Path(__file__).resolve().parent
BACKEND_DIR = HERE.parent.parent
REPO_ROOT = BACKEND_DIR.parent

# Binary build cache. Built once per pytest session.
BUILT_BINARY = HERE / ".build" / "mesedi-api-test"

# How long to wait for the binary's /health to start responding before
# we give up. The cold-start should be under a second; 10s is a
# generous ceiling that still fails fast on a broken build.
BACKEND_READY_TIMEOUT_SECS = 10.0


@dataclass
class Backend:
    """Live backend handle the tests use. base_url + api_key are
    everything an SDK or HTTP client needs; project_id is exposed so
    tests can scope direct DB inspections if they need to."""

    base_url: str
    api_key: str
    project_id: str


def _build_binary() -> Path:
    """Compile mesedi-api once per session into a known location.

    We pin the output path so subsequent sessions reuse the existing
    binary unless its mtime is older than the source — `go build` does
    this caching internally, so we just always call it.
    """
    BUILT_BINARY.parent.mkdir(exist_ok=True)
    subprocess.run(
        ["go", "build", "-o", str(BUILT_BINARY), "./cmd/api"],
        cwd=BACKEND_DIR,
        check=True,
    )
    return BUILT_BINARY


def _pick_free_port() -> int:
    """Bind a socket to port 0 to let the OS allocate a free
    ephemeral port, then close it. The binary will reopen the same
    port a few milliseconds later — a benign race in practice."""
    with socket.socket(socket.AF_INET, socket.SOCK_STREAM) as s:
        s.bind(("127.0.0.1", 0))
        return s.getsockname()[1]


def _wait_for_health(base_url: str, deadline: float) -> None:
    """Poll /health until 200 or deadline."""
    last_err: Exception | None = None
    while time.monotonic() < deadline:
        try:
            r = requests.get(f"{base_url}/health", timeout=1.0)
            if r.status_code == 200:
                return
        except requests.RequestException as exc:
            last_err = exc
        time.sleep(0.1)
    raise RuntimeError(
        f"backend at {base_url} did not become healthy within "
        f"{BACKEND_READY_TIMEOUT_SECS}s (last error: {last_err!r})"
    )


def _signup_test_project(base_url: str) -> tuple[str, str]:
    """Create a project + mint an API key via /signup.

    Returns (project_id, api_key). The key is the raw mesedi_sk_...
    bearer the SDK will use; the backend hashes + stores it during
    signup and we never see the hash here.
    """
    email = f"inttest+{int(time.time())}@example.com"
    resp = requests.post(
        f"{base_url}/signup",
        json={"email": email, "project_name": "inttest"},
        timeout=5.0,
    )
    # Signup returns 201 on creation; accept either to be robust against
    # a future tightening to strict-201.
    if resp.status_code not in (200, 201):
        raise RuntimeError(
            f"signup failed: status={resp.status_code} body={resp.text}"
        )
    body = resp.json()
    if not body.get("ok") or not body.get("api_key"):
        raise RuntimeError(f"signup returned no api_key: {body}")
    return body["project_id"], body["api_key"]


@pytest.fixture(scope="session")
def backend() -> Iterator[Backend]:
    """Boot a real mesedi-api binary and yield a Backend handle.

    Lifecycle:
        - Build the binary (cached by `go build`'s own incremental
          cache after the first run).
        - Allocate an ephemeral localhost port.
        - Spawn the binary with an in-memory SQLite DB and the env
          vars it needs to come up cleanly. We deliberately leave
          MESEDI_TOTP_ENCRYPTION_KEY unset — none of the integration
          tests touch 2FA, and the unset key just logs "2fa
          disabled" without blocking boot.
        - Wait for /health.
        - Mint a test project + API key via /signup.
        - Yield to the test session.
        - On teardown: terminate the subprocess.
    """
    binary = _build_binary()
    port = _pick_free_port()
    base_url = f"http://127.0.0.1:{port}"

    env = os.environ.copy()
    env.update(
        {
            # In-memory SQLite shared across goroutines via the
            # cache=shared connection-string fragment.
            "MESEDI_DB_URL": "file::memory:?cache=shared",
            "MESEDI_PORT": str(port),
            # Bypass the #232 email-verified gate for the test
            # session. Without this, every SDK call against a
            # freshly-signed-up project would 403 because the
            # welcome-email link was never clicked. Documented in
            # auth.go's requireEmailVerified helper; never set in
            # production.
            "MESEDI_DISABLE_EMAIL_VERIFY_GATE": "1",
            # Signup is open by default; no MESEDI_SIGNIN_SECRET set
            # means /signin server-to-server endpoints are disabled
            # (we don't need them).
            # MESEDI_ADMIN_TOKEN unset = /admin/* is gated to
            # admin-scope keys, which we don't mint; integration
            # tests don't touch /admin.
        }
    )

    proc = subprocess.Popen(
        [str(binary)],
        env=env,
        cwd=BACKEND_DIR,
        # Capture stderr so test failures can surface backend logs
        # in the pytest output. stdout is normally empty for this
        # binary (logs go to stderr).
        stderr=subprocess.PIPE,
        stdout=subprocess.DEVNULL,
    )

    try:
        deadline = time.monotonic() + BACKEND_READY_TIMEOUT_SECS
        _wait_for_health(base_url, deadline)
        project_id, api_key = _signup_test_project(base_url)
        yield Backend(base_url=base_url, api_key=api_key, project_id=project_id)
    finally:
        proc.terminate()
        try:
            proc.wait(timeout=5.0)
        except subprocess.TimeoutExpired:
            proc.kill()
            proc.wait()
        # If the backend died mid-test, pipe its stderr to ours so
        # the operator can see why.
        if proc.returncode not in (0, -15):  # 0 = clean exit, -15 = SIGTERM
            stderr = proc.stderr.read() if proc.stderr else b""
            sys.stderr.write(
                f"\n[inttest] backend exited with returncode={proc.returncode}\n"
                f"[inttest] stderr:\n{stderr.decode('utf-8', errors='replace')}\n"
            )


@pytest.fixture(scope="session")
def configured_sdk(backend: Backend):
    """Configure the mesedi SDK against the test backend exactly once
    per session. Returns the imported module so tests can use
    mesedi.wrap, mesedi.checkpoint, etc.

    Note: mesedi.configure() sets a module-global client. Any test
    that re-configures will affect every subsequent test. None do
    today, but if a future test needs an isolated SDK client it
    should use mesedi.MesediClient directly.
    """
    import mesedi

    mesedi.configure(api_key=backend.api_key, base_url=backend.base_url)
    mesedi.instrument_anthropic()
    return mesedi


def await_failure_group(
    backend: Backend,
    *,
    failure_class: str,
    signature_prefix: str = "",
    timeout_secs: float = 10.0,
) -> dict:
    """Poll GET /failure-groups until a group matching the given
    failure_class (and optional signature prefix) appears, or the
    timeout elapses.

    Returns the matching group dict on success. Raises
    AssertionError with a useful message on timeout.

    We poll the public REST surface rather than reading the SQLite
    file directly because (a) it's the same surface the dashboard
    uses, so the test reflects real customer-visible behavior, and
    (b) avoiding direct DB reads means the test doesn't need to
    know the schema.
    """
    deadline = time.monotonic() + timeout_secs
    last_seen: list[dict] = []
    while time.monotonic() < deadline:
        resp = requests.get(
            f"{backend.base_url}/failure-groups",
            headers={"Authorization": f"Bearer {backend.api_key}"},
            timeout=2.0,
        )
        if resp.status_code == 200:
            body = resp.json()
            last_seen = body.get("groups") or body.get("failure_groups") or []
            for g in last_seen:
                if g.get("failure_class") != failure_class:
                    continue
                sig = g.get("signature", "")
                if signature_prefix and not sig.startswith(signature_prefix):
                    continue
                return g
        time.sleep(0.25)

    raise AssertionError(
        f"no failure_group with class={failure_class!r} "
        f"signature_prefix={signature_prefix!r} appeared within "
        f"{timeout_secs}s. last groups seen: "
        + ", ".join(
            f"{g.get('failure_class')}/{g.get('signature')}"
            for g in last_seen
        )
    )
