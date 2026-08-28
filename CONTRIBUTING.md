# Contributing to Mesedi

Thanks for being here. This document is deliberately specific about
what helps right now, because vague "contributions welcome!" files
waste people's weekends.

Mesedi is pre-1.0 and maintained by one person. That shapes
everything below.

## The most valuable thing you can contribute

**Tell me about a failure class I'm missing.**

Mesedi ships 20 named failure classes. That list came from reading
incident write-ups and from my own agents breaking. It is certainly
incomplete. If you have run agents in production and hit something
that isn't `semantic_loop`, `token_waste`, `cost_velocity`,
`prompt_injection`, `data_leakage`, `sandbox_escape`,
`tool_failures`, `tool_schema_drift`, `grounding_failure`,
`context_overflow`, `coordination_deadlock`, `cascading_failure`,
`provider_incident`, `infrastructure_throttled`, `hitl_timeout`,
`hitl_rejection_spike`, `validator_failures`, `drift`, or a loop
variant: I want to hear it.

Open an issue describing what happened, what it looked like in your
logs, and how you eventually noticed. Even a rough description is
useful. This is the single highest-leverage thing anyone outside the
project can give me.

**Tell me when a detector misses.**

If you can construct an input that should fire a detector and
doesn't, that's a real bug even though nothing crashed. It is the
product failing at its advertised job.

For detector misses with security implications, a prompt injection
or credential leak that slips past, please use the private channel
in [SECURITY.md](SECURITY.md) rather than a public issue.

## Bug reports

Open an issue with:

- What you expected, what happened instead
- Component and version (backend commit, `mesedi` PyPI version, or
  npm version)
- Self-hosted or hosted
- A minimal reproduction if you have one; if not, file it anyway

There is no issue template yet. Prose is fine.

## Pull requests

**Small PRs: yes, please.** Typos, broken links, a doc example that
doesn't run, an obviously wrong error message, a failing edge case
with a test. Send them straight in.

**Large feature PRs: not yet.** Please open an issue first and let's
talk before you write code.

This isn't gatekeeping, and it isn't about code quality. Three
honest reasons:

1. **The detector taxonomy is still moving.** The failure classes and
   their signatures are the core of what Mesedi is. I'm still
   changing how they cluster and how signatures are derived. A new
   detector written against today's conventions may need rewriting
   next month, and I'd rather that cost be mine than yours.

2. **I'm the only reviewer.** A substantial PR deserves a careful
   review, and I can't reliably give one this week. An unreviewed PR
   sitting open for a month is worse for you than an upfront "not
   yet."

3. **Two stores, one behaviour.** The backend carries separate
   hand-written SQL for Postgres (hosted) and SQLite (self-hosted).
   A change applied to one and not the other is invisible to tests
   that only exercise the other: that exact mistake caused a
   production outage here once. Any change touching storage needs
   both paths and tests for both, which is easy to miss without
   context I haven't written down yet.

This will loosen. Ask me again after 1.0, or open an issue and make
the case: if something obviously belongs in Mesedi and you want to
build it, I'd rather find a way to say yes than lose you.

## Running things locally

**Backend** (Go, version pinned in `backend/go.mod`):

```bash
cd backend
go build ./...
go vet ./...
go test ./...
```

Some store tests spin up a real `postgres:16-alpine` container via
testcontainers. They skip automatically when Docker isn't running,
and execute for real in CI. If you're touching storage, run them with
Docker up.

**Python SDK:**

```bash
cd sdk-python
pip install -e ".[dev]"
python -m pytest -q
```

**TypeScript SDK:**

```bash
cd sdk-typescript
npm ci
npm run build
npx vitest run --exclude '**/*.integration.test.ts'
```

Note the exclude: `npm test` on its own also runs the integration
suite, which makes live provider calls and needs API keys in your
environment. CI skips those for the same reason. Run the plain
`npm test` only when you've set the keys and actually want to spend
the tokens.

CI runs all three on every push. A PR that reddens CI won't be
merged, but a failing test is a conversation, not a rejection: say
so in the PR and I'll help.

## Style

No linter config to fight with. Match the surrounding code. Comments
in this codebase tend to explain *why* rather than *what*, often
including the incident that motivated the code. That convention is
deliberate and worth continuing, because the reasoning is what
survives.

## Licence

Mesedi's core is MIT. By contributing you agree your contribution
ships under the same licence. There's no CLA.

## Code of conduct

There isn't a formal one. The informal version: be decent, assume
good faith, and remember there's one person on the other end who is
probably reading your issue between other things. Behaviour that
makes this project worse to work on gets you blocked, and I don't
plan to write a document to justify that.
