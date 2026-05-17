# Continuous synthetic-org traffic

Sets up Mesedi's synthetic-org so it runs automatically every hour without operator intervention. The Mesedi dashboard stays continuously populated with fresh-shape failure-group data; any regression in the detector chain, SDK, or shipper surfaces within an hour rather than waiting until you happen to run synthetic-org manually.

Zero API spend by design — runs in dry-run mode where every LLM call returns a mock response via `emit_llm_call`. All seven detectors still exercise correctly because the SDK still emits the underlying event payloads. The only thing the dry-run path skips is the actual round-trip to Anthropic.

## How it works

Three pieces fit together:

The **wrapper script** `scripts/continuous_traffic.sh` is what runs each batch. It sets `MESEDI_SYNTHETIC_ORG_DRY_RUN=1` in the environment, cd's into the synthetic-org directory, invokes `python3 runner.py --agent all --iterations 10 --pace-seconds 0.5`, and appends stdout/stderr to a rotating log file at `synthetic-org/continuous_traffic.log`. The log auto-rotates once it crosses 5 MB so disk usage stays bounded indefinitely.

The **launchd plist** at `scripts/ai.mesedi.synthetic-org.plist` schedules the wrapper script via macOS's native job scheduler. Fires every 3600 seconds (one hour). `RunAtLoad=false` so the first batch waits a full hour after install — avoids generating traffic at early-boot before the backend may have come up. `ThrottleInterval=60` prevents the scheduler from firing more than once per minute under any condition. `ProcessType=Background` tells macOS this is a non-interactive background job (no GUI affinity, no foreground process priority).

The **rotating log** at `continuous_traffic.log` (in the synthetic-org directory, gitignored) captures every batch's runner output for diagnostic purposes. When the file exceeds 5 MB, the wrapper script renames it to `.log.1` and starts a fresh log. Only the most recent two log files are kept; older history is implicitly purged.

## Install

One time, from the repo root:

```bash
cp synthetic-org/scripts/ai.mesedi.synthetic-org.plist ~/Library/LaunchAgents/
launchctl unload ~/Library/LaunchAgents/ai.mesedi.synthetic-org.plist 2>/dev/null
launchctl load   ~/Library/LaunchAgents/ai.mesedi.synthetic-org.plist
```

The `unload 2>/dev/null` is harmless if the plist isn't already loaded — it just clears any previous registration so re-installing after edits is idempotent.

Confirm it's registered:

```bash
launchctl list | grep mesedi
# Should print: -  0  ai.mesedi.synthetic-org
# (The "-" in column one means "not currently running"; the "0" is the
# last exit code, 0 = success. After the first hour you'll see actual
# timestamps in the launchctl output.)
```

## Trigger an immediate batch (testing)

If you don't want to wait the full hour for the first batch — useful for verifying the install worked — fire it manually:

```bash
launchctl start ai.mesedi.synthetic-org
```

The wrapper script runs once with the same configuration the scheduler would use. Watch the log:

```bash
tail -f /Users/robertcanario/mesedi/synthetic-org/continuous_traffic.log
```

You should see ten iterations across the five agents (support / clinical / financial / contract / incident, two each), each ending in `→ ok`, followed by the session summary. The Mesedi dashboard at `http://localhost:8080/ui/` should show the new executions in the Recent table within a few seconds of the batch completing.

## Inspect status

Check the last exit code and PID of the scheduled job:

```bash
launchctl list | grep mesedi
```

Read the most recent batch output:

```bash
tail -100 /Users/robertcanario/mesedi/synthetic-org/continuous_traffic.log
```

If the dashboard shows no new executions for several hours, the most likely causes are:

- **Backend isn't running.** The wrapper script attempts the runner regardless; runner.py will print connection-refused errors and exit with a circuit-breaker bailout after 5 consecutive errors. The log will show this clearly. Start the backend (`cd backend && go run cmd/api/main.go`) and the next hourly batch will succeed.
- **Plist not loaded.** `launchctl list | grep mesedi` returns nothing. Re-run the install commands above.
- **Path issue inside the wrapper.** `python3` not found, `/Users/robertcanario/mesedi/synthetic-org/scripts/continuous_traffic.sh` not executable. Check the log for the specific error.

## Uninstall

```bash
launchctl unload ~/Library/LaunchAgents/ai.mesedi.synthetic-org.plist
rm ~/Library/LaunchAgents/ai.mesedi.synthetic-org.plist
```

The wrapper script and plist in the repo are untouched; this just removes the active scheduler registration. To re-install, run the install block above again.

## Cost estimate

Zero. Every batch is `MESEDI_SYNTHETIC_ORG_DRY_RUN=1`, which short-circuits every LLM call to a synthetic event emitted via `mesedi.emit_llm_call(...)`. No tokens are consumed against the Anthropic account. The only resource costs are SQLite disk (each batch adds ~10 execution rows + their events, on the order of tens of KB) and the wrapper script's CPU/memory during the ~30-second batch run.

If you ever want to run continuous traffic against real Anthropic for some reason — say, to dogfood with real LLM responses for a demo — edit the wrapper script to unset `MESEDI_SYNTHETIC_ORG_DRY_RUN` and add `--max-spend-usd 0.50` to bound the per-batch spend. Don't leave that running indefinitely without supervision.

## Why not just leave a terminal open with `while true; do; sleep 3600; done`?

It would work, but launchd is genuinely better for this use case: it survives logout / shutdown / login cycles, it doesn't require a terminal to be open, it logs to a known location automatically, and it has built-in throttling against runaway re-scheduling. Once installed, it's invisible until you specifically look for it. For continuous background traffic on a Mac, this is the standard pattern.
