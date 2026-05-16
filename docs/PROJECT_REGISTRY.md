# Mesedi — project registry

Single source of truth for Mesedi's external-facing assets: domain, email, GitHub org, package registries, hosting accounts. Every artifact gets one entry with provenance, current state, and the post-LOI launch action.

Authoritative as of 2026-05-15. Update in place whenever a new account is created, an existing one transitions state, or a credential is rotated.

## Domain

`mesedi.ai` — registered at Cloudflare Registrar on 2026-05-13, two-year registration ($160 USD), WHOIS privacy enabled, registrant listed under founder personal name. Currently **parked** — no DNS records published, no MX, no web surface. Cloudflare nameservers control the zone but the zone is empty by design. The deliberate posture during the Verdifax outreach window is zero public Mesedi DNS footprint. Post-LOI launch action: stand up A/AAAA records for `app.mesedi.ai` (production dashboard), `api.mesedi.ai` (ingest backend), `docs.mesedi.ai` (Next.js docs site), MX records for transactional mail, DMARC/SPF/DKIM hardening on send-from domain.

## Email

`mesediai@gmail.com` — registered 2026-05-15. Dedicated identity for Mesedi-side operations (GitHub org owner, package-registry account owner, future SaaS-provider accounts). Separate from any Verdifax email account to maintain strict-isolation posture between the two projects. Pre-launch hygiene checklist:

- Enable two-factor authentication (TOTP via authenticator app, not SMS). Confirm before any external account uses this address as recovery.
- Add a non-trivial recovery phone number distinct from the Verdifax recovery numbers.
- Configure recovery email to a personal account the founder controls but does not advertise.
- Set the Gmail signature to a neutral placeholder; do not include Mesedi branding until the public launch.
- Review forwarding rules monthly during the outreach window.

Post-LOI launch: this address remains the org owner and admin-contact for every Mesedi-side account, but day-to-day operational mail moves to `robert@mesedi.ai` (or equivalent on the mesedi.ai zone) once MX records are live.

## GitHub organization

GitHub org for Mesedi exists as of 2026-05-15 — owned under the `mesediai@gmail.com` identity above, distinct from the Verdifax GitHub org and its personal-account collaborators. Exact org handle to be filled in once confirmed (`Mesedi`, `mesedi`, `mesediai`, or another variant — record the canonical handle here when known).

Current posture: org exists, no public repos. The Mesedi monorepo at `/Users/robertcanario/mesedi/` remains local-only on the founder's machine and is **not pushed to the new org during the Verdifax outreach window** to avoid acquirer-discoverable Mesedi artifacts. When ready to push, the right move is to create one private repo on the org (named `mesedi-monorepo` or just `mesedi`), push the existing local history, and leave it private until the post-LOI public launch flip.

Pre-launch hygiene checklist (mirroring what was done for the Verdifax org under tasks #9 through #33):

- Two-factor authentication required for all org members (currently just the founder).
- Branch protection defaults configured on `main` (require pull request for non-founder contributors, require status checks once CI lands).
- Org-level secrets and Actions variables empty until first CI workflow is added.
- Org-level webhooks empty.
- Org profile metadata: short description, logo placeholder, location, public-facing URL pointing to `mesedi.ai` (once that zone has a landing page).
- Domain verification (`mesedi.ai`) on the GitHub org once the DNS zone is live.
- Audit setting "members can create public repos" disabled until the public launch flip.

Repos planned for the post-LOI public launch (matching the current monorepo subdirectories): `mesedi-backend` (Go), `mesedi-sdk-python`, `mesedi-sdk-typescript`, `mesedi-dashboard` (Next.js production surface, not the local-dev HTML), `mesedi-docs` (docs site), `mesedi-synthetic-org` (dogfood substrate — possibly public to serve as a customer demo asset).

## Package registries

**PyPI:** account not yet created. Deferred until the post-LOI public launch flip. When created, register the project name `mesedi` and enable PEP 740 Trusted Publishing from the GitHub org so package releases never need long-lived API tokens. Account owner: `mesediai@gmail.com`.

**npm:** account not yet created. Deferred until the post-LOI public launch flip. When created, register the package name `mesedi` and enable npm provenance attestation tied to the GitHub org. Account owner: `mesediai@gmail.com`.

## SaaS / hosting accounts

All deferred until the post-LOI public launch. The local-only posture means none of these exist yet; the production backend runs on `/Users/robertcanario/mesedi/backend/` against a SQLite database on disk. Planned providers (matching what was scoped in `DEVELOPMENT_CHECKLIST.md`):

- **Fly.io** — Go backend hosting. Account under `mesediai@gmail.com`.
- **Neon** — managed Postgres. Free tier sufficient for v1 launch.
- **Upstash** — Redis (for rate limiting and the eventual subscriber registry once multi-instance).
- **Vercel** — Next.js dashboard hosting.
- **Clerk** — auth.
- **Stripe** — subscriptions (Phase 13 deliverable).
- **Resend** — transactional mail.
- **Sentry** — error reporting (until Mesedi can dogfood itself in v2).
- **Anthropic API** — production LLM integration for any Mesedi-side AI features (e.g., the optional Haiku-graded validator helpers).
- **Cloudflare** — DNS and edge for `mesedi.ai`.

When standing each one up post-LOI: enable 2FA, use the `mesediai@gmail.com` owner identity, document the account ID and recovery configuration here.

## Local development state

For completeness, the on-disk Mesedi state lives at `/Users/robertcanario/mesedi/` on the founder's MacBook Air. The directory is a single local-only git repo with no remote configured. SQLite dev database lives at `backend/mesedi-dev.db` and is in `.gitignore`. The Anthropic API key required for synthetic-org runs (only when not in dry-run mode) is sourced from the founder's existing `ANTHROPIC_API_KEY` env var, set in the shell profile rather than checked into the repo.

## How to use this file

Treat as a living single-source-of-truth document. Whenever a new external Mesedi-facing account is created, modified, or rotated, update the relevant section before the work is considered done. Whenever the local-only posture changes (first push to GitHub, first DNS record on `mesedi.ai`, first PyPI publish), add a dated note here documenting the transition. An acquirer or a future engineer should be able to read this file and understand the complete public footprint of the project in five minutes.
