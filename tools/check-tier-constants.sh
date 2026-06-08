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
#   - dashboard TypeScript: web-extract/dashboard/lib/tier-constants.ts
#
# Both repos must be checked out side by side at the conventional
# locations for this script to find the dashboard file. The script
# only reads files; it never edits them.
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
TS_CONSTANTS="$MESEDI_ROOT/web-extract/dashboard/lib/tier-constants.ts"

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

echo "=== Tier constant drift check ==="
echo "Go billing:    $GO_BILLING"
echo "Go handlers:   $GO_HANDLERS"
echo "TS constants:  $TS_CONSTANTS"
echo ""

# Hobby execution limit: 10,000
assert_pattern "Hobby executions included = 10000 (Go)" \
  "$GO_BILLING" "HobbyExecutionLimit[[:space:]]*=[[:space:]]*10000" || true
assert_pattern "Hobby executions included = 10_000 (TS)" \
  "$TS_CONSTANTS" "executionsIncluded:[[:space:]]*10_000" || true

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

# Hobby billing failure ceiling: 5
assert_pattern "Hobby billing failure ceiling = 5 (Go)" \
  "$GO_BILLING" "HobbyBillingFailureCeiling[[:space:]]*=[[:space:]]*5" || true
assert_pattern "Hobby billing failure ceiling = 5 (TS)" \
  "$TS_CONSTANTS" "HOBBY_BILLING_FAILURE_CEILING[[:space:]]*=[[:space:]]*5" || true

# Hobby retention cap: 15 days (lives in tierRetentionCap in handlers.go)
assert_pattern "Hobby retention cap = 15 (Go)" \
  "$GO_HANDLERS" "return 15," || true
assert_pattern "Hobby retention days = 15 (TS)" \
  "$TS_CONSTANTS" "retentionDays:[[:space:]]*15" || true

# Team retention cap: 90 days
assert_pattern "Team retention cap = 90 (Go)" \
  "$GO_HANDLERS" "return 90," || true
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
