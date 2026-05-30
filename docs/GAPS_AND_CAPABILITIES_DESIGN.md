# Mesedi: gap closure and capabilities design

> Design document for the three honest-disclosure gaps in Mesedi's
> current coverage, surfaced during analysis of the Crypto Briefing
> $500M uncapped-Claude incident (May 28, 2026).
>
> Status: draft. Author: founder. Reviewed against task #246.
> Source incident: https://cryptobriefing.com/client-loses-500m-claude-uncapped-ai-usage/

---

## Context

On May 28, 2026, an enterprise client was reported to have billed $500M
on Anthropic's Claude API in a single 30-day window after employees were
given unrestricted access with no spending caps, no usage limits, and
no observability dashboards. Microsoft has since capped internal Claude
Code licenses at the $500-$2,000 per-engineer band. Uber burned through
its entire 2026 AI budget by April.

Mesedi's seven failure-class detectors plus the hard-halt SDK catch
most of what the article describes when an agent's traffic actually
flows through the instrumented Mesedi SDK. The honest answer for what
Mesedi does NOT catch today breaks into three gaps. This document
designs the capabilities that close each gap, scopes the work, and
recommends a ship order.

---

## Gap 1: SDK bypass (direct API calls)

### What Mesedi misses today

Mesedi observes and gates traffic that flows through the instrumented
Python or TypeScript SDK. Any execution path that talks to Anthropic
(or any model provider) directly, for example:

```bash
curl -X POST https://api.anthropic.com/v1/messages \
  -H "x-api-key: sk-ant-..." \
  -d '{"model": "claude-opus-4-6", ...}'
```

bypasses Mesedi entirely. Cost-velocity detector cannot fire, hard-halt
cannot trigger, and the Mesedi dashboard never sees the spend. In the
$500M incident, thousands of employees with raw API keys would have
produced exactly this bypass pattern.

### Design: Gateway mode

Mesedi ships a reverse-proxy gateway that sits between the
organization's egress and the model provider's API endpoint. Customers
issue per-employee Mesedi-scoped API tokens, which the gateway swaps
for the underlying provider key before forwarding the request.

```
employee curl -> mesedi gateway -> Anthropic API
                        |
                        v
              Mesedi observability + budget gate
```

#### Architecture

- A single new service: `mesedi-gateway`, deployed as a Go HTTP server
  (reuses the orchestrator's existing transport-canonicalization code
  from the failure_groups path).
- One endpoint per upstream provider:
  - `POST /v1/anthropic/messages` -> `https://api.anthropic.com/v1/messages`
  - `POST /v1/openai/chat/completions` -> `https://api.openai.com/v1/chat/completions`
  - `POST /v1/gemini/generateContent` -> `https://generativelanguage.googleapis.com/...`
- Header swap: the gateway accepts `x-mesedi-key: msdi_...` and replaces it
  server-side with the customer's stored upstream key (encrypted at rest
  via AWS KMS, same posture as Verdifax's CRES).
- Pre-flight gate: checks per-token rate, per-token monthly budget,
  per-tenant aggregate budget, model-allowlist, and any other policy
  fired by the existing detectors.
- Streaming responses: forwarded byte-for-byte to preserve the SSE
  contract the upstream defines.

#### Why customers would use it

Procurement teams already need a single egress point for AI provider
keys for compliance reasons (key rotation, audit logging, leak
containment). Mesedi gateway gives them that for free, with budget
gating and observability on top. The bypass-prevention argument is the
hook, the procurement-simplification argument is what gets it deployed.

#### Trade-offs and risks

- Adds Mesedi to the critical-path latency budget. Target: under 5ms
  added p50 latency over direct provider call. Connection pooling and
  early-byte streaming mitigate most of it.
- Supply-chain concern: customers now trust Mesedi with their upstream
  API keys. Mitigations: KMS encryption at rest, audit log on every
  key access, optional customer-managed-key mode where Mesedi never
  sees the raw key (proxy-only with bring-your-own-vault integration).
- Cost: gateway is a compute line for Mesedi. Fly.io machine plus
  scaling. Estimated $30-100/month at modest enterprise volume.

#### Scope estimate

- v0.1 Anthropic-only gateway: 1 week of focused build (gateway
  service, key vault, basic policy enforcement, dashboard view).
- v0.2 OpenAI + Gemini support: +3 days each.
- v0.3 streaming SSE fidelity, retry semantics: +1 week.

Total: 3-4 weeks for a complete v1 gateway. Could ship a v0.1 Anthropic-
only beta in one focused week. The beta is enough for the Enterprise
tier marketing claim.

#### Alternative: shell-alias helper script (lighter, cheaper)

If a full gateway is too heavy for the current sprint, ship a small
shell helper:

```bash
# install
curl -fsSL https://mesedi.ai/install/curl-shim | sh

# usage (replaces raw curl -> Anthropic with budget-checked version)
mesedi-curl https://api.anthropic.com/v1/messages -d '{...}'
```

The shim is a 200-line bash script that wraps curl, calls a Mesedi
endpoint to log the request and check the budget, then forwards to
Anthropic. Catches the casual-engineer bypass case without the
infrastructure cost. Does NOT catch hostile bypass (someone determined
to evade) but is sufficient for the "nobody thought to instrument"
case in the article.

Ship cost: 1-2 days. Recommended as a v0 stopgap that ships before
the full gateway.

---

## Gap 2: Org-level rollup (per-project burn caught, aggregate not)

### What Mesedi misses today

Mesedi tracks burn per-project. The cost-velocity detector fires when
a single project's burn rate exceeds the configured threshold. The
dashboard shows per-project totals. The storage view (admin) breaks
DB size down per-project. But there is no single screen that says:

> Across all 47 projects on this Mesedi tenant, total spend in the
> last 30 days is $X. Top 5 projects are A ($a), B ($b), C ($c),
> D ($d), E ($e). Trend over last 90 days: up 35% month over month.

A $500M incident at the article's enterprise would have shown up as
"thousands of small per-project alerts that nobody aggregated" rather
than "one screaming aggregate alert."

### Design: Tenant rollup view + aggregate detectors

#### Dashboard surface: /app/admin/tenant or /app/org

A new top-level dashboard view, gated by tenant-admin role, with:

- Headline: "Tenant total burn (last 30d)" + delta vs previous 30d.
- Top-5 projects by burn, with sparklines.
- Project count + active-project count (active = at least one
  execution in last 7d).
- Provider breakdown: spend by Anthropic vs OpenAI vs Gemini, with
  model breakdown (Claude Opus vs Sonnet vs Haiku, etc.).
- New project hot-list: projects created in the last 7 days, sorted
  by burn (catches "the new agent someone spun up that's burning hot").

#### New detectors that fire on tenant-aggregate behavior

- `tenant_burn_velocity`: tenant-wide $/hour exceeds threshold.
- `tenant_project_creation_spike`: more than N new projects created
  per hour (catches "someone is mass-provisioning agents").
- `tenant_concurrent_active_projects`: more than M concurrently
  executing projects (catches the "thousands of employees firing
  agents simultaneously" pattern from the article).
- `tenant_monthly_budget_ceiling`: cumulative spend exceeds the
  tenant's declared monthly ceiling.

These behave like the existing seven failure classes: they fire
failure_group events, dispatch webhook escalations, can trigger the
hard-halt across all projects in the tenant, and are surfaced in the
dashboard.

#### Hard-halt at tenant level

A new escalation rung above project-level hard-halt:
`tenant.halt` halts every project under the tenant simultaneously,
with an operator override workflow (require N-of-M approver signoff
to resume, configurable). This is the org-level kill switch that
would have stopped the $500M bleed within seconds of the first
aggregate threshold breach.

#### Scope estimate

- Backend tenant-aggregate queries (rollup endpoint on the API): 2-3 days.
- Dashboard view: 2-3 days.
- New aggregate detectors: 1-2 days (mostly extending existing
  detector framework).
- Tenant-level hard-halt + approver workflow: 3-5 days.

Total: 1.5-2 weeks for v1.

---

## Gap 3: Governance ownership (nobody watching the dashboard)

### What Mesedi misses today

The article's actual root cause is human, not technical: nobody at
the enterprise owned AI spend. Mesedi catches the runaway, fires
alerts to webhooks, shows the burn on the dashboard, can hard-halt
with a button click. None of that helps if the configured webhook
goes to a Slack channel nobody reads, no dashboard is in anyone's
morning routine, and the org-level monthly ceiling was never set.

### Design: Active governance push (CFO-style digest)

Make Mesedi push to the responsible humans on a schedule even when
nothing has gone wrong yet. The capability is opt-in policy, not
required vigilance.

#### Scheduled digest webhooks

A new webhook event type `digest.weekly` and `digest.monthly`
(eventually `digest.daily` for high-stakes tenants). Fires on cron
regardless of failure activity, with payload:

```json
{
  "event": "digest.weekly",
  "tenant_id": "...",
  "period_start": "2026-05-26",
  "period_end": "2026-06-01",
  "total_burn_usd": 12450.00,
  "delta_vs_previous_period_pct": 18.4,
  "top_projects": [...],
  "active_projects_count": 47,
  "halts_triggered": 3,
  "tenant_budget_remaining_usd": 17550.00,
  "tenant_budget_consumed_pct": 41.5
}
```

Customer routes this to a CFO inbox, an FP&A Slack channel, an
exec-stakeholder email distribution list, etc. The information now
reaches whoever owns the budget without anyone having to remember to
look at a dashboard.

#### Email digest as a first-class delivery target

Webhooks are great for systems-integrated customers. For
not-yet-Slack-integrated customers (which is most early Mesedi
adopters), ship an HTML-email digest as the primary delivery surface.
Routes through Resend, same outbound infrastructure as the welcome
email sequence.

#### Tenant monthly budget ceiling with auto-halt

A simple knob: tenant admin sets a monthly USD ceiling. When the
tenant-aggregate detector fires at the ceiling, every project halts
automatically. Override requires an explicit "raise ceiling" action
in the dashboard. Combined with the digest, this means a CFO who
reads the weekly email can see the ceiling slipping before it hits,
not after.

#### Scope estimate

- Scheduled cron + digest payload assembly: 2-3 days.
- HTML email template via Resend: 1-2 days.
- Tenant monthly ceiling + auto-halt UI: 2 days.
- Webhook event type registration + docs: 1 day.

Total: about 1 week of focused work.

---

## Recommendation: ship order

Recommended sequence, balancing impact vs effort vs how soon each
capability earns a credible Enterprise-tier line item on the pricing
page:

### Sprint 1 (ship in 1 week)

1. **Shell-alias bypass shim** (Gap 1 lite). 200-line bash script that
   wraps curl. Catches the casual bypass case. Documented as the
   "free path for engineers who haven't integrated the SDK yet."
2. **Scheduled weekly + monthly digest webhooks** (Gap 3 part 1).
   Active push of summary to the responsible human. Includes email
   delivery via Resend.
3. **Tenant monthly budget ceiling + auto-halt** (Gap 3 part 2).
   Knob in dashboard, halt action wired into the existing
   hard-halt mechanism.

After Sprint 1, the Enterprise tier on the pricing page can credibly
list: org-level budget ceiling, scheduled CFO digest, full hard-halt
on aggregate breach.

### Sprint 2 (ship in 2 weeks)

4. **Tenant rollup dashboard view** (Gap 2 part 1). The screen that
   shows total burn, top projects, active project count, provider
   breakdown.
5. **Aggregate detectors** (Gap 2 part 2). The four new tenant-level
   detectors that fire failure_groups on aggregate behavior.

After Sprint 2, Enterprise tier adds: org-level rollup view, four
new aggregate detectors, tenant-level alerting.

### Sprint 3 (ship in 4 weeks)

6. **Gateway mode v0.1, Anthropic only** (Gap 1 full). One-week build
   plus a week of soaking in beta. Beta customers ride the gateway
   for free until v1 ships.

After Sprint 3, Enterprise tier adds the bypass-prevention story
that maps directly to the article's failure mode.

### Sprint 4 (ship in 6 weeks)

7. **Gateway: OpenAI and Gemini support**.
8. **Approver workflow for tenant-halt override**.

---

## Cross-references

### Pricing page (task #247)

After Sprint 1, the Enterprise tier on /pricing can ship with these
capabilities listed as differentiators. The $25K-$50K/year asking
price is justified against the $500M incident: any Mesedi enterprise
subscription, even at the high end, would have saved tens of millions.

Suggested Enterprise tier feature bullets after Sprint 1:
- Tenant-level monthly budget ceiling with automatic hard-halt on breach.
- Scheduled weekly and monthly burn digests delivered to CFO / FP&A
  via email or webhook.
- Curl-bypass shim for non-SDK-instrumented teams.
- VPC option, SSO, SLA, dedicated CSM (existing Enterprise tier copy).

After Sprint 2:
- Org-level burn rollup dashboard (tenant total, top projects, provider
  and model breakdown, active-project count, new-project hotlist).
- Four new aggregate detectors firing failure_groups on tenant-level
  behavior.

After Sprint 3:
- Mesedi Gateway for Anthropic API: bypass-prevention reverse proxy
  with budget gating before any traffic hits the upstream provider.

### Marketing copy (task #245)

The "what Mesedi would have caught" table on the marketing site and in
the Show HN draft lists the seven existing detectors against the article's
failure modes. The honest "would NOT have caught" list footnotes:
- "We see what's instrumented. The Gateway mode (Enterprise, shipping
  Sprint 3) closes the SDK bypass case."
- "Per-project today, tenant-aggregate on the Enterprise tier (Sprint 2)."
- "Scheduled digests and tenant budget ceilings ship in Sprint 1."

Each footnote ties the gap to a specific shipping date the marketing
team can update as deliverables land.

---

## Notes for review

- All scope estimates assume one engineer (the founder) working full-time.
- Sprint 1 is the highest-impact, lowest-effort sequence. If only one
  sprint can ship before Show HN launch, Sprint 1 is the right one
  because it earns the marketing claim and the Enterprise tier
  pricing anchor with minimal new infrastructure.
- The Gateway (Sprint 3) is the technically most ambitious item and
  the one that maps most directly to the article's narrative. It's
  worth shipping even if no customer pays for it on Day 1, because
  it converts "Mesedi catches what flows through our SDK" into "Mesedi
  catches what flows through your AI egress, instrumented or not."

## Open questions

- Should the tenant-level hard-halt require N-of-M approver signoff,
  or can a single tenant admin trigger it? The $500M incident argues
  for "single admin can halt everything immediately," but the
  enterprise customer probably wants more nuance once they're past
  the initial firefighting phase.
- Pricing of execution-equivalent for gateway traffic: does each
  gateway-proxied request count toward the Pro tier's 100K/month
  cap, or is gateway-only traffic billed separately at a per-request
  rate? Recommendation: keep one execution accounting model; gateway
  requests count as executions on whichever project they're tagged to.
- Should the curl-shim be open-source MIT under the existing Mesedi
  license? Recommendation: yes, no reason to gate it. Free
  distribution maximizes the bypass-prevention surface.
