"""
Local hard-halt budgets — Phase 10 sub-slice 21a.

A `Budget` is a per-execution policy: "halt if this run takes more
than N seconds, OR emits more than N events, OR uses more than N
tokens." When any constraint trips, the SDK raises `MesediHalt`
synchronously at the next halt-safe checkpoint — between LLM-call
boundaries, between tool-call boundaries, or wherever the user
explicitly calls `mesedi.checkpoint()`.

`MesediHalt` is a regular Python exception. That means:

  - Standard `try`/`finally` cleanup blocks run as the exception
    unwinds the stack — open files close, locks release, context
    managers exit. The SDK doesn't need a separate "cleanup hooks"
    machinery; Python's exception model already provides it.
  - The `@mesedi.wrap` decorator catches `MesediHalt` AT THE TOP of
    the wrapped function, marks the execution `status=halted` with
    a `halt_reason` field, and **does NOT re-raise**. Calling code
    sees the wrapped function "return" cleanly; the halt is logged
    on Mesedi's side as the terminal status of the run.

**This slice is pure SDK — no backend changes, no network
dependency.** A future slice (21b) adds a remote control channel so
detectors firing on the backend can signal a halt back to the SDK.
"""

from __future__ import annotations

import threading
import time
from dataclasses import dataclass
from typing import Optional


class MesediHalt(BaseException):
    """Raised when an execution's local budget is exceeded.

    Inherits from `BaseException` (not `Exception`) so user code's
    broad `except Exception:` handlers do NOT swallow it. Only an
    explicit `except MesediHalt:` (or `except BaseException:`) catches
    it. The `@mesedi.wrap` decorator has the explicit handler that
    converts the halt into a `status=halted` PATCH and returns from
    the wrapped function rather than propagating to the caller.
    """

    def __init__(self, reason: str, trigger: str):
        super().__init__(reason)
        self.reason = reason
        self.trigger = trigger  # "wall_clock" | "step_count" | "token_total" | "remote_signal"


@dataclass
class Budget:
    """A per-execution halt policy.

    All limits are optional — pass `None` for "no limit on this axis."
    `Budget()` with all defaults is a no-op (never halts).

    Attributes:
        max_wall_clock_seconds: Maximum wall-clock duration before
            halt. Checked at every safe-boundary inspection.
        max_steps: Maximum number of events (tool calls, LLM calls,
            checkpoints) emitted in this execution.
        max_tokens_in: Maximum total input tokens summed across all
            llm_call events.
        max_tokens_out: Maximum total output tokens.
    """

    max_wall_clock_seconds: Optional[float] = None
    max_steps: Optional[int] = None
    max_tokens_in: Optional[int] = None
    max_tokens_out: Optional[int] = None

    def is_unbounded(self) -> bool:
        """True if no field is set — Budget() is a no-op halt policy."""
        return (
            self.max_wall_clock_seconds is None
            and self.max_steps is None
            and self.max_tokens_in is None
            and self.max_tokens_out is None
        )


class BudgetTracker:
    """Per-execution running totals plus the halt-check primitive.

    Owned by an `ExecutionContext`. Mutated by `@tool`, the Anthropic
    monkey-patch, and `checkpoint()` as events flow through the
    execution. Checked at safe boundaries via `check_or_halt()`.

    Thread-safe — concurrent tool calls in the same execution that
    bump the counters simultaneously won't corrupt the totals.
    """

    def __init__(self, budget: Budget, started_at_monotonic: float):
        self._budget = budget
        self._started_at_monotonic = started_at_monotonic
        self._lock = threading.Lock()
        self._step_count = 0
        self._tokens_in = 0
        self._tokens_out = 0

    def increment_steps(self, n: int = 1) -> None:
        with self._lock:
            self._step_count += n

    def add_tokens(self, tokens_in: int = 0, tokens_out: int = 0) -> None:
        with self._lock:
            self._tokens_in += tokens_in
            self._tokens_out += tokens_out

    def snapshot(self) -> dict:
        """Return current totals (for inclusion in halt-reason metadata)."""
        with self._lock:
            return {
                "step_count": self._step_count,
                "tokens_in": self._tokens_in,
                "tokens_out": self._tokens_out,
                "wall_clock_seconds": time.perf_counter() - self._started_at_monotonic,
            }

    def check_or_halt(self) -> None:
        """Inspect the budget; raise MesediHalt if any limit is exceeded.

        Called at every halt-safe checkpoint — LLM-call entry, tool-
        call entry, explicit `checkpoint()`. Cheap when the budget is
        unbounded (early-return) so it's safe to call frequently.
        """
        b = self._budget
        if b.is_unbounded():
            return

        if b.max_wall_clock_seconds is not None:
            elapsed = time.perf_counter() - self._started_at_monotonic
            if elapsed >= b.max_wall_clock_seconds:
                raise MesediHalt(
                    reason=f"wall-clock budget exceeded: {elapsed:.2f}s >= {b.max_wall_clock_seconds:.2f}s",
                    trigger="wall_clock",
                )

        with self._lock:
            step_count = self._step_count
            tokens_in = self._tokens_in
            tokens_out = self._tokens_out

        if b.max_steps is not None and step_count >= b.max_steps:
            raise MesediHalt(
                reason=f"step budget exceeded: {step_count} >= {b.max_steps}",
                trigger="step_count",
            )

        if b.max_tokens_in is not None and tokens_in >= b.max_tokens_in:
            raise MesediHalt(
                reason=f"input-token budget exceeded: {tokens_in} >= {b.max_tokens_in}",
                trigger="token_total",
            )

        if b.max_tokens_out is not None and tokens_out >= b.max_tokens_out:
            raise MesediHalt(
                reason=f"output-token budget exceeded: {tokens_out} >= {b.max_tokens_out}",
                trigger="token_total",
            )
