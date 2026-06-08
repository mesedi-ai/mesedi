# Pre-flight #30 Tier Constants Audit

Status: Milestone 1 (audit + canonical source)

This document is the cross-surface audit catalog for the deep tier-claims refactor that lands before the public Mesedi repo ships. It enumerates every surface that hand-codes a tier, execution count, retention window, price, or SLA today, and lays out which refactor each surface needs in Milestones 2 and 3.

## Why this exists

The pre-flight #30 audit found pricing-claim drift across at least nine customer-facing surfaces. Examples caught: terms page promised "1 business day" SLA while pricing card promised "within 48 hours"; terms page mentioned a stale "5,000 execution per month cap" from a prior pricing rewrite; pricing page CTA claimed Team becomes the obvious upgrade at 50K executions while the actual breakeven is closer to 60K and the FAQ said Team is 100K included not 50K.

The root cause is that every tier number was hand-typed as a literal string in every file that referenced it. Single source of truth removes that whole class of bug.

## Architecture

Two canonical sources, one reconciler.

1. Runtime enforcement source: `backend/internal/api/billing.go` constants like `HobbyExecutionLimit`, `HobbyOveragePriceUSD`, `TeamAIAnalysisLimit`. Backend continues to read these at runtime. No change to existing logic.
2. Customer-facing source: `web-extract/dashboard/lib/tier-constants.ts` exports a typed `TIER_CONSTANTS` record plus formatting helpers (`formatExecutions`, `formatPrice`, `formatRetention`, `formatOverageRate`, `formatCap`, `hobbyToTeamBreakevenExecutions`).
3. Drift guard: `tools/check-tier-constants.sh` runs in CI and asserts every key value appears identically in both files. Either change both together or the build fails.

## Audit catalog

Every surface that mentions a tier number, with the action needed in M2 or M3.

### Tier 1: marketing surfaces (public-facing)

| File | Current state | M2 action |
|---|---|---|
| `web-extract/dashboard/app/pricing/page.tsx` | Hand-coded TIERS array with hardcoded prices, exec counts, retention values; FAQ entries with hardcoded numbers | Refactor TIERS array to derive from TIER_CONSTANTS; FAQ entries already corrected for math but need to read constants for the numbers |
| `web-extract/dashboard/app/page.tsx` (marketing homepage) | References "10K executions" and tier names in hero/positioning copy | Replace literal numbers with `formatExecutions(TIER_CONSTANTS.hobby.executionsIncluded)` template substitutions |
| `web-extract/dashboard/app/signup/page.tsx` | Hobby + Team taglines hand-coded; Stage A copy mentions "10,000 executions per month free" and overage rate | Refactor taglines to template strings consuming TIER_CONSTANTS |
| `web-extract/dashboard/app/terms/page.tsx` | Mentions tier names, execution caps, retention windows, SLA prose in legal copy | Replace literals with TIER_CONSTANTS substitutions; keep legal-tone prose stable around the numbers |
| `web-extract/dashboard/app/privacy/page.tsx` | References data retention defaults per tier | Replace retention literals with `formatRetention()` calls |
| `web-extract/dashboard/app/security/page.tsx` | Mentions retention; possibly mentions audit logs and SSO availability per tier | Replace retention literals; conditionalize feature mentions on TIER_CONSTANTS[slug].sso / auditLogs |

### Tier 2: in-app dashboard surfaces (logged-in users)

| File | Current state | M3 action |
|---|---|---|
| `web-extract/dashboard/app/app/billing/page.tsx` | Already partially refactored in this session; still has copy mentioning "10,000 executions per month" and "$0.002 per execution" in the Add a card banner | Replace remaining literals; "Add a card" banner body should call `formatExecutions(TIER_CONSTANTS.hobby.executionsIncluded)` etc. |
| `web-extract/dashboard/app/app/page.tsx` (dashboard home) | Shows tier name / usage summary on landing | Use TIER_CONSTANTS[currentTier].name for display |
| `web-extract/dashboard/app/app/settings/page.tsx` | Surfaces retention controls with tier-cap text | Read tier cap via TIER_CONSTANTS[currentTier].retentionDays |
| `web-extract/dashboard/lib/marketing-cases.ts` | Includes retention/tier strings in case-study data structures | Replace literals; case-study prose mentioning retention should use formatter helpers |

### Tier 3: docs pages

| File | Current state | M2 action |
|---|---|---|
| `web-extract/dashboard/app/docs/page.tsx` | Docs landing references tier names | Replace any literal tier names with TIER_CONSTANTS lookups |
| `web-extract/dashboard/app/docs/quickstart/page.tsx` | Mentions "10K free executions" in onboarding copy | Replace literal with formatter call |
| `web-extract/dashboard/app/docs/self-host/page.tsx` | Likely references CE limits | Verify against TIER_CONSTANTS.self_hosted |
| `web-extract/dashboard/app/docs/concepts/page.tsx` | Concepts page may mention tier features in product overview | Audit for literals |
| `web-extract/dashboard/app/docs/hitl/page.tsx` | HITL doc may mention tier-gated availability | Audit for literals |
| `web-extract/dashboard/app/docs/api/page.tsx`, `python-sdk/page.tsx`, `typescript-sdk/page.tsx`, `multi-agent/page.tsx`, `opentelemetry/page.tsx` | SDK and API docs; check each for tier references in availability notes | Audit one by one |

### Tier 4: CE staging public README + docs

| File | Current state | M2 action |
|---|---|---|
| `mesedi-ce-staging/README.md` | Hand-coded Cloud Hobby/Team/Enterprise pricing in the "Cloud vs CE" comparison table | Hardcode numbers in markdown match TIER_CONSTANTS; no template substitution available in pure markdown, so reconciliation is manual + verified by `tools/check-tier-constants.sh` extension |
| `mesedi-ce-staging/CONTRIBUTING.md` | Likely mentions tier scope for CE contributions | Audit |
| `mesedi-ce-staging/SECURITY.md` | Vulnerability disclosure SLA | Verify SLA windows match TIER_CONSTANTS support text |
| `mesedi-ce-staging/sdk-python/README.md` | SDK doc may reference Cloud tier availability | Audit |
| `mesedi-ce-staging/sdk-typescript/README.md` | Same | Audit |

### Tier 5: backend error messages (customer-facing strings the backend emits)

| File | Current state | M3 action |
|---|---|---|
| `backend/internal/api/billing.go` | `capExceeded` returns formatted error mentioning the cap; `HandleCreateSetupCheckout` error messages mention tier names | Replace tier name literals with constants; use `fmt.Sprintf` with `TierHobby` / `TierTeam` instead of "Cloud Hobby" / "Cloud Team" literal strings |
| `backend/internal/api/handlers.go` | `HandleAnalyzeFailureGroup` returns "LLM-assisted root-cause analysis is a Cloud Team feature" literal; ingest cap-reached error mentions $X of $Y | Replace tier name literals with `tier_display_name(tier)` helper; numbers come from existing constants |
| `backend/internal/api/signup.go` | Error messages around tier limits at signup | Audit; signup is mostly free-tier so likely fine |
| `backend/internal/mail/mailer.go` | Hobby billing notification templates mention "$200 cap" and "10K free quota" hardcoded in the email body strings | Replace literals with values pulled from billing.go constants; template renderer should accept tier as input and inject values |
| `backend/internal/playbooks/content/cost_velocity/_default.md` | Customer-facing playbook content referencing dollar amounts | Audit; replace if drift-prone |

## Non-goals for this refactor

Intentionally NOT included in M2/M3:

1. Backend test fixtures (`detectors/*_test.go`): tests assert against the runtime constants which already come from `billing.go`. No drift risk.
2. Migration SQL files: schema not customer-facing prose.
3. node_modules / generated files: never edited manually.
4. CORS / RBAC / internal config files (`cors.go`, `rbac.go`, `main.go`): no customer-facing tier copy.
5. `lib/format.ts`, `lib/api.ts`: utility files that don't contain tier numbers themselves.

## Effort estimates

- M2 (Tier 1, 3, 4 surfaces): roughly 12-18 files, 3-4 hours of focused refactor work
- M3 (Tier 2, 5 surfaces): roughly 8-10 files, 2-3 hours

Total remaining work after M1: 5-7 hours across M2 + M3.

## Files written in Milestone 1

1. `web-extract/dashboard/lib/tier-constants.ts` (new) — canonical TS source
2. `tools/check-tier-constants.sh` (new) — drift guard script
3. `PREFLIGHT_30_TIER_AUDIT.md` (this file) — audit catalog

No existing files were modified. No commits taken in M1.

## Robert's review checklist before approving M1

Before approving M2 start, please verify:

1. Open `web-extract/dashboard/lib/tier-constants.ts`. Confirm every numeric value matches your intent for the public ship: 10K Hobby execs, $0.002 Hobby overage, $200 default cap, 15-day Hobby retention, 100K Team, $0.001 Team overage, 90-day Team retention, 200 LLM analyses/period Team cap, 5 Hobby billing failure ceiling.
2. Confirm the four tier slugs (`self_hosted`, `hobby`, `team`, `enterprise`) are the right vocabulary. The backend uses `TierHobby = "hobby"`, etc. These match.
3. Confirm the formatter helper outputs match your taste:
   - `formatExecutions(10_000)` → "10,000"
   - `formatRetention(15)` → "15-day"
   - `formatRetention(3650)` → "10 years"
   - `formatPrice(0, "/month")` → "$0/month"
   - `formatPrice(0, "forever")` → "Free"
   - `formatPrice(99, "/month")` → "$99/month"
   - `formatPrice(null, null)` → "Contact us"
   - `formatOverageRate(0.002)` → "$0.002"
   - `formatCap(200)` → "$200"
   - `formatCap(0)` → "uncapped (configurable)"
   - `hobbyToTeamBreakevenExecutions()` → 59500 (Hobby executions where overage cost catches Team flat $99: 10000 + ceil(99 / 0.002))
4. Run `bash tools/check-tier-constants.sh` from the mesedi repo root. Should print all "OK" lines and exit 0.

If any of those four checks surfaces a concern, flag it and I will adjust the constants file before M2 starts. Once you approve, M2 (marketing + docs + CE staging refactor) begins.
