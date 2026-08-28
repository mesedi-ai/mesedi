# Mesedi Enterprise Edition

This directory contains Mesedi Enterprise Edition (EE) source code.

**License:** proprietary. See [`LICENSE.md`](LICENSE.md).

Everything **outside** this directory is MIT-licensed. See the top-level
[`LICENSE`](../LICENSE). You can self-host, modify, and use the MIT-licensed
core of Mesedi in production at no cost, with no artificial usage caps.

## What lives here

The `ee/` directory is where enterprise-only features go. As of the initial
`ee/` scaffold commit this directory is intentionally empty, Mesedi's
current feature set is entirely MIT-licensed and available in the free
self-host build. As enterprise features ship, they will land here and be
gated behind a Mesedi Enterprise subscription.

Planned enterprise features:

- **SAML / OIDC SSO** for enterprise identity providers (Okta, Azure AD,
  Auth0, etc.). Google + GitHub OAuth stay in the MIT core.
- **Advanced RBAC** beyond the current admin / member split.
- **Seat management** and license-key enforcement.
- **SLA reporting** and support-tier gating.
- **Long-term retention** beyond the default retention window.
- **Compliance packs** (SOC 2 evidence generation, HIPAA reporting).
- Anything else that primarily serves enterprise procurement,
  compliance, or governance rather than product function.

The list above is directional, not a commitment. Mesedi reserves the
right to move features between free and paid as the product evolves.

## Why the split

Mesedi ships MIT-licensed core so that anyone can self-host, learn from
the code, and contribute back. The enterprise tier funds the ongoing
maintenance of the MIT-licensed core plus a hosted Cloud product for
teams who don't want to run infrastructure themselves.

Concretely:

- **Free self-host**: full failure-class detector coverage, full SDK
  surface, full dashboard, no seat caps, no retention caps, no artificial
  usage limits. MIT-licensed. See the top-level [`LICENSE`](../LICENSE).
- **Mesedi Cloud**: the same code as free self-host, but hosted and
  operated by Mesedi. Tiered pricing (Hobby / Team / Enterprise).
- **Mesedi Enterprise Edition**: additional features gated to
  enterprise procurement needs, licensed under this directory's
  [`LICENSE.md`](LICENSE.md). Available both as an add-on to Cloud and
  as a self-hostable add-on for teams running their own Mesedi
  deployment.

## Development

You can freely read, modify, and run EE code in development and testing
environments: see section 3 of [`LICENSE.md`](LICENSE.md). Production use
requires a Mesedi Enterprise subscription.

## Contributing

Contributions to the EE directory follow the same PR workflow as the rest
of the repository, but by contributing you agree that Mesedi retains
copyright over your contributions to the EE codebase. See section 5 of
[`LICENSE.md`](LICENSE.md). Contributions to the MIT-licensed core stay
under MIT.
