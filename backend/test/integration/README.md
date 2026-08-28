# Backend integration tests

End-to-end test suite that runs the real `mesedi` Python SDK against
the real `mesedi-api` Go binary and asserts each backend detector
fires on textbook input.

## What it catches that unit tests don't

The unit tests for detectors use synthetic JSON payloads handcrafted
to match what each detector reads. The unit tests for the SDK don't
run a real backend. The mismatch only shows up when real SDK output
meets real detector input. This harness is the missing third layer.

Bugs this suite was built to surface:

- **Field-name drift between SDK and detectors.** Example we just
  hit: `DetectTokenWaste` reads `user_prompt` but the SDK ships
  `user_message`, so the detector silently no-ops on every real
  customer execution.
- **Outer-mux routing gaps in `cmd/api/main.go`.** Routes registered
  in `RegisterRoutes` / `RegisterPublicRoutes` that are never
  forwarded by the top-level mux 404 in production with no log line.
  This trap has bitten us at least twice (email-verify, 2FA). A Go
  test that builds its own mux cannot catch this; running the real
  binary can.
- **Detector ordering or greedy-claim regressions.** Example: when
  `time_budget` ran early in the chain with a 1-second threshold, it
  silently suppressed seven more specific detectors. A regression
  guard that asserts the specific detectors still fire would have
  caught it.

## Running

Prereqs: Go toolchain, Python 3.10+, `ANTHROPIC_API_KEY` set in your
shell (skipped tests if absent).

```
cd backend/test/integration
pip install -r requirements.txt
pytest -v
```

The fixture in `conftest.py` builds the backend binary once per
session, spawns it on a random localhost port with an in-memory
SQLite store, mints a test project + API key via the public signup
endpoint, and tears down after the suite finishes.

Each test in `test_detectors.py` is independent, fires one SDK
scenario per test, polls `GET /failure-groups` until the expected
class+signature appears within a 10-second timeout, asserts the
result. A failing assertion means the SDK + backend pair has drifted
and you have a real customer-visible bug to fix.
