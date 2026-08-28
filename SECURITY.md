# Security Policy

Mesedi detects security-relevant failures in AI agents: prompt
injection, data leakage, sandbox escape. A tool that makes security
claims has to be reportable-against, so this document exists to give
you a private channel and a straight answer about what happens next.

## Reporting a vulnerability

**Do not open a public issue for a security vulnerability.** A public
issue discloses the problem to everyone at the same moment, including
people running Mesedi in production who have not had a chance to
patch.

Two private channels, either is fine:

1. **GitHub private vulnerability reporting**: the "Report a
   vulnerability" button under this repository's **Security** tab.
   Preferred, because the whole exchange stays attached to the repo.
2. **Email <security@mesedi.ai>**: if you would rather not use
   GitHub, or the report involves the hosted service rather than the
   source.

## What to include

The more of this you can provide, the faster it gets fixed:

- What the vulnerability lets an attacker do, stated plainly
- Steps to reproduce, or a proof-of-concept
- Affected component (backend, Python SDK, TypeScript SDK, dashboard)
  and version or commit
- Whether it affects self-hosted deployments, the hosted service, or
  both
- Any conditions required: specific tier, configuration, or
  authentication state

A detector **bypass** is in scope and genuinely useful to us: if you
can construct a prompt injection, a leaked credential, or a sandbox
escape that Mesedi fails to flag, that is a real finding even though
nothing is "exploited" in the traditional sense. It is the product
failing at its stated job.

## What to expect

Mesedi is currently maintained by one person. These are honest
targets rather than contractual guarantees, and they are deliberately
not dressed up as an SLA:

- **Acknowledgement within 48 hours.** If you have not heard back in
  that window, assume the mail went astray and send a follow-up.
- **An initial assessment within 5 business days**, whether it
  reproduces, rough severity, and intended fix timeline.
- **Progress updates** at least every 10 days while the issue is open.
- **Credit in the release notes** when the fix ships, unless you would
  rather stay anonymous. Say which you prefer.

There is **no paid bug bounty** at this time. Saying so upfront is
fairer than letting you spend a weekend on something expecting a
payout.

## Coordinated disclosure

Please give us **90 days** from acknowledgement before publishing.
If a fix ships sooner, publish as soon as it is released. There is no
reason to sit on it. If 90 days pass without a fix, publish anyway;
an unfixed vulnerability that stays quiet only protects the vendor.

If a vulnerability is being actively exploited, tell us that in the
first message and we will treat the timeline as irrelevant.

## Scope

**In scope:**

- The backend (`backend/`), including auth, tenancy isolation, the
  webhook dispatcher, and the DLP rules
- Both SDKs (`sdk-python/`, `sdk-typescript/`)
- The hosted service at `app.mesedi.ai` and `api.mesedi.ai`
- Detector bypasses, as described above
- Anything that lets one project read, modify, or infer another
  project's data

**Out of scope:**

- Denial of service through sheer volume, including against the
  hosted service
- Social engineering of Mesedi or its users
- Missing security headers or cookie flags on the marketing pages
  where no session data is involved
- Vulnerabilities in a self-hosted deployment that stem from that
  operator's own configuration: an exposed database port, a missing
  reverse-proxy auth layer, secrets committed to their fork
- Findings from automated scanners submitted without a working
  reproduction
- Reports whose substance is that a third-party dependency has a
  published CVE, with no demonstration that Mesedi is reachable
  through it. Dependency alerts are tracked separately via Dependabot.

## Safe harbour

If you make a good-faith effort to follow this policy, we will not
pursue legal action against you, and we will say so publicly if
anyone else tries to. Good faith means: you did not access, modify,
or delete data belonging to anyone else; you did not degrade the
service for other users; you stopped as soon as you had enough to
write a report; and you gave us a private channel before going
public.

If you are unsure whether something crosses a line, ask first at
<security@mesedi.ai>. Asking is never held against you.

## Supported versions

Mesedi is pre-1.0. Security fixes land on `main` and ship in the next
release; there are no long-term support branches yet. Self-hosters
should track the latest tag.

| Version | Supported          |
| ------- | ------------------ |
| 0.1.x   | :white_check_mark: |
| < 0.1   | :x:                |

This table will get more interesting once there is more than one
release line. Pretending otherwise would be theatre.
