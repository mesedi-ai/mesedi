#!/usr/bin/env bash
#
# tools/check-tier-constants.sh
#
# Drift guard for tier constants. Asserts that every numeric tier
# value appears identically in:
#
#   - backend Go source: backend/internal/api/billing.go and
#     backend/internal/api/handlers.go (for the tierRetentionCap
#     function)
#   - dashboard TypeScript: ../mesedi-web/dashboard/lib/tier-constants.ts
#
# Both repos must be checked out side by side (i.e. as sibling
# top-level directories `mesedi/` and `mesedi-web/`) for this
# script to find the dashboard file. The script only reads files;
# it never edits them.
#
# Pre-this script pointed at mesedi/web-extract/ — a locally-
# maintained stale snapshot of the mesedi-web repo. .F moved
# it to the actual sibling repo path so the drift check runs against
# the live customer-facing file, not a snapshot that had to be
# manually kept in sync.
#
# Exit codes:
#   0 = all constants in sync
#   1 = at least one drift detected
#   2 = a referenced file is missing
#
# Run in CI before any merge to main, and locally via
#   bash tools/check-tier-constants.sh
# before pushing changes that touch pricing copy or billing logic.

set -euo pipefail

# Anchor the script's notion of paths to its own location so it
# works from any working directory.
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
MESEDI_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"

GO_BILLING="$MESEDI_ROOT/backend/internal/api/billing.go"
GO_HANDLERS="$MESEDI_ROOT/backend/internal/api/handlers.go"
# .F: point at the sibling mesedi-web checkout rather than a
# locally-maintained snapshot inside mesedi/web-extract/. The
# customer-facing dashboard is in mesedi-web; the drift check must
# read from the actual deployed source, not a stale copy.
TS_CONSTANTS="$MESEDI_ROOT/../mesedi-web/dashboard/lib/tier-constants.ts"

for f in "$GO_BILLING" "$GO_HANDLERS" "$TS_CONSTANTS"; do
  if [ ! -f "$f" ]; then
    echo "ERROR: required file not found: $f"
    exit 2
  fi
done

drift=0

# assert_pattern <description> <file> <pattern>
#
# Returns success if the pattern is found in the file. On failure
# prints a one-line drift report and bumps the drift counter via
# the calling scope.
assert_pattern() {
  local description="$1"
  local file="$2"
  local pattern="$3"
  if grep -qE "$pattern" "$file"; then
    echo "OK: $description"
    return 0
  fi
  echo "DRIFT: $description not found in $(basename "$file")"
  echo "       expected pattern: $pattern"
  drift=1
  return 1
}

# assert_tier_field <description> <file> <tier_slug> <pattern>
#
# Extracts the named tier's block from tier-constants.ts using awk,
# then greps for <pattern> WITHIN that block. Use when the field
# and its value span multiple lines (e.g. sso arrays, auditLogs
# comments) so a single-line regex like `hobby:.*sso:.*"google"`
# cannot match. Bumps the drift counter on miss.
assert_tier_field() {
  local description="$1"
  local file="$2"
  local tier="$3"
  local pattern="$4"
  local block
  block=$(awk -v tier="$tier" '
    $0 ~ "^  "tier": \\{" { in_block=1; next }
    in_block && /^  \},?[[:space:]]*$/ { in_block=0; next }
    in_block { print }
  ' "$file")
  if echo "$block" | grep -qE "$pattern"; then
    echo "OK: $description"
    return 0
  fi
  echo "DRIFT: $description not found in $(basename "$file")"
  echo "       tier: $tier | pattern: $pattern"
  drift=1
  return 1
}

echo "=== Tier constant drift check ==="
echo "Go billing:    $GO_BILLING"
echo "Go handlers:   $GO_HANDLERS"
echo "TS constants:  $TS_CONSTANTS"
echo ""

# Hobby execution limit: 5,000 (.F cut from 10K, closes )
assert_pattern "Hobby executions included = 5000 (Go)" \
  "$GO_BILLING" "HobbyExecutionLimit[[:space:]]*=[[:space:]]*5000" || true
assert_pattern "Hobby executions included = 5_000 (TS)" \
  "$TS_CONSTANTS" "executionsIncluded:[[:space:]]*5_000" || true

# Hobby overage rate: $0.002 USD/exec
assert_pattern "Hobby overage rate = 0.002 (Go)" \
  "$GO_BILLING" "HobbyOveragePriceUSD[[:space:]]*=[[:space:]]*0\\.002" || true
assert_pattern "Hobby overage rate = 0.002 (TS)" \
  "$TS_CONSTANTS" "overagePerExecutionUSD:[[:space:]]*0\\.002" || true

# Team execution included: 100,000
assert_pattern "Team executions included = 100000 (Go)" \
  "$GO_BILLING" "TeamExecutionIncluded[[:space:]]*=[[:space:]]*100000" || true
assert_pattern "Team executions included = 100_000 (TS)" \
  "$TS_CONSTANTS" "executionsIncluded:[[:space:]]*100_000" || true

# Team overage rate: $0.001 USD/exec
assert_pattern "Team overage rate = 0.001 (Go)" \
  "$GO_BILLING" "TeamOveragePriceUSD[[:space:]]*=[[:space:]]*0\\.001" || true
assert_pattern "Team overage rate = 0.001 (TS)" \
  "$TS_CONSTANTS" "overagePerExecutionUSD:[[:space:]]*0\\.001" || true

# LLM analysis per-org cap: 200 (Team)
assert_pattern "Team LLM analysis cap = 200 (Go)" \
  "$GO_BILLING" "TeamAIAnalysisLimit[[:space:]]*=[[:space:]]*200" || true
assert_pattern "Team LLM analysis cap = 200 (TS)" \
  "$TS_CONSTANTS" "llmRootCausePerPeriod:[[:space:]]*200" || true

# Hobby LLM analysis per-period cap: 25 (.F cut from 50, closes )
assert_pattern "Hobby LLM analysis cap = 25 (Go)" \
  "$GO_BILLING" "HobbyAIAnalysisLimit[[:space:]]*=[[:space:]]*25" || true
assert_pattern "Hobby LLM analysis cap = 25 (TS)" \
  "$TS_CONSTANTS" "llmRootCausePerPeriod:[[:space:]]*25" || true

# Hobby per-analysis price: \$0.75 (pay-per-use)
assert_pattern "Hobby AI analysis price = 0.75 (Go)" \
  "$GO_BILLING" "HobbyAIAnalysisPriceUSD[[:space:]]*=[[:space:]]*0\\.75" || true
assert_pattern "Hobby AI analysis price = 0.75 (TS)" \
  "$TS_CONSTANTS" "aiAnalysisPriceUSD:[[:space:]]*0\\.75" || true

# Team AI analysis overage price: \$0.50 (no hard cap)
assert_pattern "Team AI analysis overage = 0.50 (Go)" \
  "$GO_BILLING" "TeamAIAnalysisOveragePriceUSD[[:space:]]*=[[:space:]]*0\\.50" || true
assert_pattern "Team AI analysis overage = 0.50 (TS)" \
  "$TS_CONSTANTS" "aiAnalysisPriceUSD:[[:space:]]*0\\.50" || true

# Hobby billing failure ceiling: 5
assert_pattern "Hobby billing failure ceiling = 5 (Go)" \
  "$GO_BILLING" "HobbyBillingFailureCeiling[[:space:]]*=[[:space:]]*5" || true
assert_pattern "Hobby billing failure ceiling = 5 (TS)" \
  "$TS_CONSTANTS" "HOBBY_BILLING_FAILURE_CEILING[[:space:]]*=[[:space:]]*5" || true

# Hobby retention default: 7 days (.F cut from 15, closes
# — PostHog-free benchmark). Constant in billing.go used by
# both signup default and tierRetentionCap in handlers.go.
assert_pattern "Hobby retention default = 7 (Go)" \
  "$GO_BILLING" "HobbyDefaultRetentionDays[[:space:]]*=[[:space:]]*7" || true
assert_pattern "Hobby retention days = 7 (TS)" \
  "$TS_CONSTANTS" "retentionDays:[[:space:]]*7" || true

# Team retention default: 90 days
assert_pattern "Team retention default = 90 (Go)" \
  "$GO_BILLING" "TeamDefaultRetentionDays[[:space:]]*=[[:space:]]*90" || true
assert_pattern "Team retention days = 90 (TS)" \
  "$TS_CONSTANTS" "retentionDays:[[:space:]]*90" || true

# Hobby price: $0 / month
assert_pattern "Hobby price = 0 (TS)" \
  "$TS_CONSTANTS" "pricePerMonthUSD:[[:space:]]*0" || true

# Team price: $99 / month (only enforced in TS; backend doesn't
# hard-code the Stripe price, it uses MESEDI_STRIPE_TEAM_PRICE_ID
# env var so this lives on the marketing surface only)
assert_pattern "Team price = 99 (TS)" \
  "$TS_CONSTANTS" "pricePerMonthUSD:[[:space:]]*99" || true

# Hobby default cap: $200
assert_pattern "Hobby default cap = 200 (TS)" \
  "$TS_CONSTANTS" "defaultCapUSD:[[:space:]]*200" || true

# ─── Per-feature claim drift guards (.F, closes ) ────
#
# Pre-this script only asserted pricing scalars. That let
# hobby.sso=[] and hobby.auditLogs=false ship as customer-facing
# lies for weeks — the backend had no tier gate on either, but
# the CI script had no way to notice the mismatch.
#
# Below asserts every per-feature claim in tier-constants.ts
# against the actual backend gating site (or the documented
# absence of one). If someone flips a claim without also updating
# the backend, or ships a backend tier gate without updating the
# claim, this script fails the build.
#
# Doctrine: if the claim says "this tier does NOT have feature X"
# (empty array, false), the backend MUST have a code path that
# enforces the restriction. If no enforcement exists, the claim
# MUST say the tier HAS the feature (matches production reality).

# Hobby SSO — production allows Google + GitHub (no tier check
# in signin.go). Claim MUST include both providers.
assert_tier_field "Hobby SSO includes Google (TS)" \
  "$TS_CONSTANTS" "hobby" '"google"' || true
assert_tier_field "Hobby SSO includes GitHub (TS)" \
  "$TS_CONSTANTS" "hobby" '"github"' || true
# Backend side — signin.go must NOT gate SSO by tier. If a tier
# check appears here in the future, the Hobby claim above needs
# to change to match. This grep looks for the ABSENCE of tier
# discrimination in the SSO handler; if a tier check ships, the
# assertion fails and forces the constants file update.
if grep -q "APIKeySourceSSOLogin.*Tier\|Tier.*APIKeySourceSSOLogin" "$GO_BILLING" 2>/dev/null; then
  echo "DRIFT: signin.go added a tier check on SSO login, but"
  echo "       tier-constants.ts still claims Hobby has SSO."
  echo "       Update hobby.sso to match the new restriction."
  drift=1
else
  echo "OK: signin.go still has no tier gate on SSO (matches hobby.sso claim)"
fi

# Hobby audit logs — .F (decision) moved to Team-only.
# Backend enforcement in audit.go:HandleListAuditEvents rejects
# Hobby (via allowlist: only TierTeam / TierEnterprise pass).
# Constants file MUST declare auditLogs: false to match reality.
assert_tier_field "Hobby auditLogs = false (TS)" \
  "$TS_CONSTANTS" "hobby" 'auditLogs:[[:space:]]*false' || true
# Backend side — HandleListAuditEvents MUST tier-gate. Looks for
# the allowlist pattern (`tier != TierTeam && tier != TierEnterprise`)
# or the explicit TierTeam string in the handler. If both go
# missing without flipping the claim, this assertion fails.
if grep -A 30 "func .*HandleListAuditEvents" \
    "$MESEDI_ROOT/backend/internal/api/audit.go" 2>/dev/null \
    | grep -qE "TierTeam|TierHobby"; then
  echo "OK: HandleListAuditEvents has tier gate (matches hobby.auditLogs=false)"
else
  echo "DRIFT: HandleListAuditEvents no longer tier-gates Hobby, but"
  echo "       tier-constants.ts still claims Hobby lacks auditLogs."
  echo "       Either restore the tier gate OR set hobby.auditLogs=true."
  drift=1
fi

# Team SSO — production ships Google + GitHub. Claim MUST include
# both providers. (Same OAuth callback code path as Hobby.)
assert_tier_field "Team SSO includes Google (TS)" \
  "$TS_CONSTANTS" "team" '"google"' || true
assert_tier_field "Team SSO includes GitHub (TS)" \
  "$TS_CONSTANTS" "team" '"github"' || true

# Enterprise SSO — Google + GitHub ship; microsoft/saml/oidc/okta
# are contract-negotiated build-on-request items. The pricing card
# menu shows all six; contracts finalize the actual per-customer
# list before onboarding. Claim MUST include google + github and
# MAY include the aspirational four (with the inline comment
# documenting per-contract status).
assert_tier_field "Enterprise SSO includes Google (TS)" \
  "$TS_CONSTANTS" "enterprise" '"google"' || true
assert_tier_field "Enterprise SSO includes GitHub (TS)" \
  "$TS_CONSTANTS" "enterprise" '"github"' || true

# Self-hosted SSO — only Google + GitHub ship in the MIT source.
# Any additional provider would need customer-side code (fork
# with new OAuth callback). Claim MUST match shipped source.
assert_tier_field "Self-hosted SSO includes Google (TS)" \
  "$TS_CONSTANTS" "self_hosted" '"google"' || true
assert_tier_field "Self-hosted SSO includes GitHub (TS)" \
  "$TS_CONSTANTS" "self_hosted" '"github"' || true

# Hobby admins cap — production refuses team invites on Hobby
# (handlers_team.go:456). Claim MUST be 1.
assert_tier_field "Hobby admins = 1 (TS)" \
  "$TS_CONSTANTS" "hobby" 'admins:[[:space:]]*1' || true
assert_pattern "Hobby team-invite refuses tier (Go)" \
  "$MESEDI_ROOT/backend/internal/api/handlers_team.go" \
  "TierHobby" || true

echo ""
if [ "$drift" -ne 0 ]; then
  echo "=== TIER CONSTANTS DRIFT detected ==="
  echo "Update both Go and TS sources to match. Both must be"
  echo "edited together; the runtime enforcer (Go) and the customer-"
  echo "facing copy (TS) cannot diverge without breaking trust."
  exit 1
fi
echo "=== All tier constants in sync between Go and TS ==="
exit 0
