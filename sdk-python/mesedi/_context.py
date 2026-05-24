"""
Execution-context tracking via ``contextvars``.

When ``@mesedi.wrap`` decorates a function, it sets a context variable
to identify the currently-executing run. Inside that function, any
``@mesedi.tool``-decorated tool call reads the same variable to learn
which execution it belongs to, so tool_call events can attach to the
right Execution at the backend.

Why ``contextvars`` and not threading.local or a global dict:

  - ``contextvars`` is the standard library primitive for "ambient
    state that's logical-call-stack-scoped, not thread-scoped." It
    behaves correctly in async code (each Task gets its own copy) and
    in nested calls (set/reset returns a token, so popping a nested
    context restores the outer one automatically).

  - threading.local works for sync code but fails in async; we want
    @wrap and @tool to work in both. asyncio support comes in a
    later slice but the foundation is already async-friendly.

  - A module-level dict keyed by thread_id is hand-rolled state with
    none of the above guarantees.

Sequence numbering: events within an execution are numbered 1, 2, 3, …
The context object holds a monotonic counter so concurrent tool calls
within the same execution can request distinct sequence numbers
without coordinating.
"""

from __future__ import annotations

import threading
import time
from contextvars import ContextVar, Token
from dataclasses import dataclass, field
from typing import Optional

from mesedi.halt import Budget, BudgetTracker


@dataclass
class ExecutionContext:
    """Per-execution scratch state shared between @wrap, @tool,
    @anthropic, @checkpoint, and validator_result.

    Holds the execution_id (so events know which Execution to attach
    to), a sequence counter (so each emitted event gets a monotonically
    increasing sequence number within the execution), and an optional
    BudgetTracker for hard-halt enforcement.
    """

    execution_id: str
    _seq_lock: threading.Lock = field(default_factory=threading.Lock, repr=False)
    _seq: int = 0
    budget_tracker: Optional[BudgetTracker] = None

    def next_sequence(self) -> int:
        """Return the next sequence number for this execution.

        Thread-safe, two tool calls fired concurrently from different
        threads in the same execution still get distinct sequence
        numbers.
        """
        with self._seq_lock:
            self._seq += 1
            return self._seq

    def check_budget(self) -> None:
        """Inspect the budget; raise MesediHalt if any limit is exceeded.

        Cheap no-op if no budget was configured (or all limits are
        unbounded). Called from @tool/@checkpoint/anthropic-patch at
        the function-call boundary BEFORE the inner work runs, so a
        halt fires at a safe checkpoint rather than mid-tool.
        """
        if self.budget_tracker is not None:
            self.budget_tracker.check_or_halt()


_current: ContextVar[Optional[ExecutionContext]] = ContextVar(
    "mesedi_current_execution",
    default=None,
)


def current_execution_context() -> Optional[ExecutionContext]:
    """Return the currently-active execution context, or None if outside @wrap."""
    return _current.get()


def push_execution_context(
    execution_id: str,
    budget: Optional[Budget] = None,
) -> Token[Optional[ExecutionContext]]:
    """Set the current execution context.

    Returns a token that must be passed to ``pop_execution_context()``
    to restore the previous value. Nested wraps (a @wrap function
    calling another @wrap function) work naturally: each call pushes,
    each return pops, the outer context is restored automatically.

    If `budget` is provided, a fresh BudgetTracker is attached for
    halt enforcement within this execution.
    """
    tracker: Optional[BudgetTracker] = None
    if budget is not None and not budget.is_unbounded():
        tracker = BudgetTracker(budget=budget, started_at_monotonic=time.perf_counter())
    ctx = ExecutionContext(execution_id=execution_id, budget_tracker=tracker)
    return _current.set(ctx)


def pop_execution_context(token: Token[Optional[ExecutionContext]]) -> None:
    """Restore the execution context to its prior value."""
    _current.reset(token)
