"""
MesediClient — HTTP client + event shipper for the Mesedi backend.

The client owns two things:

  1. An ``httpx.Client`` for the underlying HTTP connection (with the
     bearer token and schema-version header pre-configured).
  2. An ``EventShipper`` background thread that buffers and dispatches
     executions and events asynchronously.

Two API tiers are exposed:

  - **Async (default for @wrap):** ``submit_*()`` methods enqueue work
    on the shipper and return immediately. The shipper handles batching,
    retries, and graceful shutdown. Mesedi outages NEVER block the
    caller.
  - **Sync (advanced):** ``create_execution()`` / ``update_execution()``
    / ``send_events()`` make the HTTP call inline and return the
    parsed response body. Use these only when you need the backend's
    response (e.g., to inspect the ``accepted`` / ``rejected`` count
    from /events) or when running in a context where the shipper is
    impractical (CLI tools, scripts).

Authentication: bearer token (``mesedi_sk_...``). Wire-format version
is fixed at "1" today; future breaking changes bump SCHEMA_VERSION and
the backend tightens enforcement.
"""

from __future__ import annotations

import os
from typing import Any, Dict, List, Optional

import httpx

from mesedi._shipper import EventShipper
from mesedi.events import Event, Execution

DEFAULT_BASE_URL = "http://localhost:8080"
DEFAULT_TIMEOUT = 10.0
SCHEMA_VERSION = "1"

# Module-level singleton client. Populated by mesedi.configure() and
# consumed by the @wrap decorator. Most callers need exactly one client
# per process — Mesedi is meant to observe agent code, not to be
# instantiated per-call.
_default_client: Optional["MesediClient"] = None


class MesediClient:
    """HTTP client + background shipper for the Mesedi backend.

    Thread-safety: the underlying httpx.Client is thread-safe for
    concurrent use, and the shipper's queue is thread-safe by design,
    so a single MesediClient can be safely shared across threads.
    """

    def __init__(
        self,
        api_key: str,
        base_url: str = DEFAULT_BASE_URL,
        timeout: float = DEFAULT_TIMEOUT,
        flush_interval_ms: int = 250,
        batch_size: int = 100,
        max_queue: int = 10_000,
    ):
        if not api_key:
            raise ValueError("api_key is required")
        if not api_key.startswith("mesedi_sk_"):
            # Match the backend's auth-middleware check — fail loudly on
            # the SDK side rather than letting the backend return 401 on
            # every call.
            raise ValueError(
                "api_key must start with 'mesedi_sk_' "
                "(received an obviously-malformed key)"
            )

        self.api_key = api_key
        self.base_url = base_url.rstrip("/")
        self._http = httpx.Client(
            base_url=self.base_url,
            headers={
                "Authorization": f"Bearer {api_key}",
                "Content-Type": "application/json",
                "X-Mesedi-Schema-Version": SCHEMA_VERSION,
            },
            timeout=timeout,
        )
        self._shipper = EventShipper(
            http=self._http,
            flush_interval_ms=flush_interval_ms,
            batch_size=batch_size,
            max_queue=max_queue,
        )

    # ── context-manager sugar ─────────────────────────────────────────

    def __enter__(self) -> "MesediClient":
        return self

    def __exit__(self, *exc: Any) -> None:
        self.close()

    def close(self) -> None:
        """Shut down the shipper and close the underlying HTTP client.

        Safe to call multiple times. The shipper drains its queue with
        a timeout; any items not yet sent at the timeout are lost.
        """
        self._shipper.shutdown()
        self._http.close()

    def flush(self, timeout: float = 5.0) -> bool:
        """Block until everything submitted so far has been sent.

        Returns True if drained within ``timeout``, False otherwise.
        Primarily useful in tests and end-of-script synchronization;
        production code should rely on the atexit-registered shutdown.
        """
        return self._shipper.flush(timeout=timeout)

    # ── async submit (used by @wrap; default path) ────────────────────

    def submit_execution_start(self, execution: Execution) -> None:
        """Enqueue POST /executions for asynchronous dispatch."""
        self._shipper.submit_execution_start(execution)

    def submit_execution_end(self, execution: Execution) -> None:
        """Enqueue PATCH /executions/{id} for asynchronous dispatch."""
        self._shipper.submit_execution_end(execution)

    def submit_event(self, event: Event) -> None:
        """Enqueue a single Event for batched asynchronous dispatch."""
        self._shipper.submit_event(event)

    # ── sync (advanced) ───────────────────────────────────────────────

    def create_execution(self, execution: Execution) -> Dict[str, Any]:
        """POST /executions synchronously. Returns parsed JSON response.

        Bypasses the shipper. Useful for one-off scripts or when you
        need the backend's response (e.g., to inspect ``ok``). Production
        agent code should prefer ``submit_execution_start()``.
        """
        r = self._http.post("/executions", json=execution.start_payload())
        r.raise_for_status()
        return r.json()  # type: ignore[no-any-return]

    def update_execution(self, execution: Execution) -> Dict[str, Any]:
        """PATCH /executions/{id} synchronously. Returns parsed JSON."""
        r = self._http.patch(
            f"/executions/{execution.execution_id}",
            json=execution.end_payload(),
        )
        r.raise_for_status()
        return r.json()  # type: ignore[no-any-return]

    def send_events(self, events: List[Event]) -> Dict[str, Any]:
        """POST /events synchronously as a JSON array. Empty list is a no-op."""
        if not events:
            return {"ok": True, "accepted": 0, "rejected": 0}
        payload = [e.to_dict() for e in events]
        r = self._http.post("/events", json=payload)
        r.raise_for_status()
        return r.json()  # type: ignore[no-any-return]


def configure(
    api_key: Optional[str] = None,
    base_url: Optional[str] = None,
    timeout: float = DEFAULT_TIMEOUT,
    flush_interval_ms: int = 250,
    batch_size: int = 100,
) -> MesediClient:
    """Configure the module-level default client.

    Environment-variable fallbacks:
      - api_key  ← MESEDI_API_KEY
      - base_url ← MESEDI_BASE_URL (defaults to ``http://localhost:8080``)

    Calling configure() again replaces the previous default client; the
    previous client's shipper is shut down (with flush) before
    replacement.
    """
    global _default_client

    if api_key is None:
        api_key = os.environ.get("MESEDI_API_KEY")
    if not api_key:
        raise RuntimeError(
            "Mesedi API key not provided. Pass api_key=... to "
            "mesedi.configure() or set MESEDI_API_KEY in the environment."
        )

    if base_url is None:
        base_url = os.environ.get("MESEDI_BASE_URL", DEFAULT_BASE_URL)

    # Cleanly close any previous default client before replacing it.
    if _default_client is not None:
        try:
            _default_client.close()
        except Exception:
            pass

    _default_client = MesediClient(
        api_key=api_key,
        base_url=base_url,
        timeout=timeout,
        flush_interval_ms=flush_interval_ms,
        batch_size=batch_size,
    )
    return _default_client


def get_client() -> MesediClient:
    """Return the module-level default client.

    If not yet configured, tries to auto-configure from MESEDI_API_KEY in
    the environment. Raises RuntimeError if no api_key is available.
    """
    if _default_client is None:
        env_key = os.environ.get("MESEDI_API_KEY")
        if env_key:
            return configure()
        raise RuntimeError(
            "Mesedi is not configured. Call mesedi.configure(api_key=...) "
            "or set MESEDI_API_KEY in the environment before using @mesedi.wrap."
        )
    return _default_client


def flush(timeout: float = 5.0) -> bool:
    """Module-level helper: flush the default client.

    Convenience wrapper for ``mesedi.get_client().flush(timeout)``. Useful
    when you want a one-liner at the end of a script to ensure all
    observations have landed before exit.
    """
    return get_client().flush(timeout=timeout)
