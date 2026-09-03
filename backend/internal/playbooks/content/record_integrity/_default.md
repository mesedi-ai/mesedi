# Record integrity

The event record for this execution contradicts itself. Every other failure class in Mesedi tells you something about what your agent did. This one tells you something about the evidence — that the record you are reading is not complete, and therefore that any conclusion drawn from it may be drawn from a partial picture.

Two signatures, and both can fire on the same execution:

- **`record_integrity:sequence_gap`** — the events carry sequence numbers with at least one value missing between the lowest and highest observed. Events were produced that this record does not contain.
- **`record_integrity:duplicate_sequence`** — two or more events claim the same sequence number. One position in the record was written more than once, so at most one of them is the original.

Ordering is deterministic: gap appears before duplicate when both fire.

## Read this before you act on it

**This is not a tampering alert, and treating it as one will waste your time.** A missing sequence number is far more often a dropped HTTP request, an SDK process killed before its buffer flushed, or a retry that landed twice than it is anybody removing anything.

There is a harder reason too, and it is structural rather than statistical: **the detector's only input is supplied by the caller.** Sequence numbers arrive inside the event body, and Mesedi's ingest endpoint authenticates the project key, not the content. Anyone able to post events chooses the sequence numbers — so an actor concealing something does not leave a gap. They post a dense stream, and this detector reports a clean record.

The scope is therefore: **it sees records that were damaged, not records that were authored.** Loss, crash and retry it catches. Fabrication it does not, and no detector reading the same unsigned stream could — that would be asking the data whether the data is trustworthy.

What the detector establishes is narrower and still worth having: *this record is not complete, and here is the exact position that is missing or doubled.* Whether that is infrastructure or intent is a question this data cannot answer.

Note also what it **cannot** see. The gap check measures from the lowest sequence number actually present, not from a fixed starting number, because Mesedi must not assume where your SDK begins counting. The consequence is that if the *first* events of a run never arrived, the lowest survivor silently becomes the new floor and the record looks clean. Detecting a truncated head needs a start marker the event stream alone does not carry.

## What's usually happening

Ranked roughly by how often each turns out to be the cause:

**Transport loss.** An event POST failed and was never retried. Check your SDK's error logs for the window covering the execution. This is the single most common explanation and the easiest to confirm.

**Process death mid-flush.** The agent was killed — OOM, container eviction, deploy rollout, timeout kill — while events were buffered and not yet sent. Look for the execution ending in `crashed`, `timeout`, or with no terminal status at all. If the gap sits at the *end* of the surviving range, this is usually why.

**Duplicate delivery.** A retry succeeded after the original also succeeded, and both were written. Produces `duplicate_sequence` without a gap. Usually harmless to the agent's actual behaviour, but it means anything counting events for this execution is counting one too many.

**Concurrent writers sharing a sequence counter.** Two goroutines, threads or workers emitting under the same execution id without coordinating their numbering. Produces duplicates, and often duplicates at several positions rather than one. If you see many duplicated values in one execution, look here first.

**Clock or ordering assumptions in your own instrumentation.** If you assign sequence numbers yourself rather than letting the SDK do it, an off-by-one or a reset on reconnect will show up here immediately.

## How to investigate

1. **Open the execution and look at where the gap sits.** A gap at the end points at process death. A gap in the middle points at transport loss. Duplicates scattered throughout point at concurrent writers.

2. **Check the execution's terminal status.** An execution that never received its terminal PATCH is a strong signal the agent process did not exit cleanly, which explains both the missing tail and any missing terminal event.

3. **Correlate with your own infrastructure for that window.** Deploy events, OOM kills, network incidents and provider outages all leave marks elsewhere. If a `provider_incident` or `infrastructure_throttled` group covers the same window, you likely have your answer.

4. **Count how many executions are affected.** One execution is an anomaly. A steady rate across a project means your telemetry is systematically lossy, and every dashboard number you look at is quietly low.

## How to fix

**If it is transport loss:** enable or lengthen SDK retry with backoff, and make sure event submission failures are logged loudly rather than swallowed. Silent drop is what turns this from a fixable bug into a slow erosion of your data.

**If it is process death:** flush events on shutdown. Install a signal handler for SIGTERM that drains the buffer before exiting, and give your container a termination grace period long enough for that drain to complete. Most orchestrators default to a grace period shorter than a slow flush needs.

**If it is duplicate delivery:** make event submission idempotent on `event_id` so a retry that lands twice is written once.

**If it is concurrent writers:** serialise sequence assignment behind a single counter per execution, or give each worker its own execution id and link them with `parent_execution_id`. Sharing an execution id across uncoordinated writers is the root problem; sharing a counter is only the symptom.

## Why this class exists at all

If nothing in your stack is watching for holes, a lossy pipeline looks exactly like a healthy one. The events that arrive render fine. The dashboard totals up whatever it has. A missing event and an event that never happened are indistinguishable — unless something is checking that the record accounts for itself.

That matters more the more weight the record carries. Telemetry used to tune a prompt can absorb some loss. A record used to demonstrate what a system did, to an auditor, a regulator, or a customer disputing an outcome, cannot — and there the question stops being "is this complete" and becomes "can you prove it was not changed." That question needs the record to have been signed and chained when it was written, which is a different mechanism than this one.
