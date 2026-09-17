# Disaster recovery

How Mesedi Cloud's production data is protected, what recovery looks
like in each failure scenario, and, stated plainly, what is not
covered. Numbers marked "measured" come from timed drills against the
real system, not estimates. Self-hosted deployments are not covered
here: with self-hosting you own the database and the backups, and
this document can serve as a template for what yours should answer.

This file replaces an earlier runbook that described the original
SQLite-on-a-volume deployment. Production has run on managed Postgres
since the migration, and the posture below is the one the security
page at mesedi.ai/security describes.

## The short version

Mesedi is operated by one person. There is no on-call rotation and no
secondary responder. Everything runs in a single US region with no
multi-region failover. We would rather you knew that before signing
than found it out during an incident; it is also why the SDK is
designed so a Mesedi outage cannot break your agents.

Customer data lives in managed Postgres (Neon) with a 24-hour
point-in-time history, plus a nightly encrypted dump held at a
different vendor (Cloudflare R2), so losing the database provider
does not mean losing the data.

| Scenario | Data loss (RPO) | Time to recover (RTO) | Status |
|---|---|---|---|
| Bad migration or accidental delete | ~0 (to the second) | 39 seconds | Measured 2026-08-27 |
| Backend machine lost | 0 | Seconds to minutes | Automatic (Fly) |
| Dashboard lost | 0 | ~2 minutes | Redeploy |
| Database provider lost entirely | Up to 48 hours | 11 minutes | Measured 2026-08-28 |

## How far back a restore can reach

Data loss on recovery and reachable history are different questions.
"Can you restore us to last Tuesday?" is the second one.

| Age of the target moment | Available | Granularity |
|---|---|---|
| 0 to 24 hours | Postgres point-in-time | To the second |
| 1 to 30 days | Nightly encrypted dump | Roughly daily |
| Over 30 days | Nothing | Objects expire |

The 30-day boundary is a verified lifecycle rule, not an intention. A
corruption discovered on day 31 is not recoverable from any copy
Mesedi holds, which is why detection (monitoring plus the monthly
restore drill below) has to be faster than a month.

## The off-platform copy

A full database dump runs nightly, is encrypted with AES-256 before
it leaves the machine that produced it, and is stored with a
different vendor from the database itself. It is never written
unencrypted to durable storage.

The schedule is best-effort, and the honest consequence is stated in
the table above: worst-case loss in the provider-loss scenario is up
to 48 hours, not the 24 the schedule implies. A missed backup is
detected by a dead man's switch: the backup job pings an external
heartbeat on success, and silence past 36 hours raises an alert. It
is deliberately not a second scheduled job checking the first, since
a watchdog that shares the scheduler with the thing it watches fails
silently alongside it.

## The restore is tested, not just the backup

A backup that has never been restored is a hypothesis. An automated
drill runs monthly: it fetches a real backup, decrypts it, restores
it into a clean database, and fails loudly if the newest backup is
stale or the restored data is old. This also continuously proves the
encryption and decryption parameters still agree, the failure mode
where every new backup silently becomes undecryptable while the job
keeps reporting success.

The full recovery path for total provider loss was rehearsed by hand
on 2026-08-28: provision a fresh Postgres instance, retrieve and
decrypt the off-platform backup, restore, repoint the application,
and confirm it serving from the restored data by writing real rows,
not by a health check answering. End to end: 11 minutes, of which the
mechanical restore was under a minute; the rest was human steps this
runbook now shortens.

## What is not covered, stated plainly

Simultaneous loss of both vendors (the database provider and the
backup store) is out of scope at current scale. Nothing currently
watches TLS certificate or domain expiry; the certificate auto-renews
and a lapse would take the API down without prior warning. A full
region outage takes Mesedi down until the region returns.

## Detection

api.mesedi.ai/ready pings the database and verifies the applied
migration count on every check, returning 503 with a reason code when
either fails, and is monitored every few minutes from multiple
continents with alerting by email. The distinction from /health is
deliberate: a process that is merely alive cannot fail a liveness
check, and readiness is what once caught a database outage that three
green uptime monitors sat through.

## Retention

| Data | Retained |
|---|---|
| Executions, events, failure groups, webhook deliveries | Per tier: 7 days Hobby, 90 days Team, longer on hand-sold tiers |
| Audit events | 7 years, including after project closure |
| Nightly encrypted dumps | 30 days |

Review trigger for this document: any change to database provider,
region, backup destination, or retention policy. Otherwise annually.
