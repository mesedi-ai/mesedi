#!/usr/bin/env bash
# stress-test-159/backend-staging.config.sh
#
# STAGING-TARGET OVERRIDE for the #159 pre-launch stress test run.
# Points every live-API category at mesedi-api-staging.fly.dev instead
# of production so we can hammer the backend without disturbing the
# real deploy.
#
# Usage (from ~/mesedi/backend/, mesedi backend repo root):
#
#     # 1. Swap the public config aside and drop this one in
#     cp stress-test.config.sh stress-test.config.sh.prod-bak
#     cp ~/mesedi/internal-extract/stress-test-159/backend-staging.config.sh \
#        stress-test.config.sh
#
#     # 2. Run the harness (uses MESEDI_STAGING_API_KEY env var below)
#     export MESEDI_STAGING_API_KEY="Bearer mesedi_sk_<your_staging_test_key>"
#     ~/stress-test/back-end/stress-test.sh ~/mesedi/backend
#
#     # 3. When done, revert the public config
#     mv stress-test.config.sh.prod-bak stress-test.config.sh
#
# NEVER commit this staging override into the public backend repo. This
# file lives in `internal-extract/` so its `mesedi-api-staging.fly.dev`
# hostname stays out of the public tree.

# ── Project identity ──────────────────────────────────────────────────────
PROJECT_NAME="mesedi-backend-staging"
PROJECT_LANGUAGE="go"

# ── MCD source scan (unchanged from prod config) ─────────────────────────
SCAN_DIRS=("internal" "cmd")
SCAN_EXCLUDE=(
  "vendor"
  "node_modules"
  ".git"
  ".venv"
  "dist"
  "build"
  "out"
  "testdata"
  "internal/api/home_export"
)

# ── Dependency vulnerability scan (unchanged) ────────────────────────────
DEPS_GO="go.mod"
DEPS_PYTHON=""
DEPS_NODE=""

# ── Static analysis (unchanged) ──────────────────────────────────────────
STATIC_TOOLS="AUTO"

# ── Live API tests (STAGING) ─────────────────────────────────────────────
API_BASE_URL="https://mesedi-api-staging.fly.dev"
API_HEALTH_PATH="/health"

# The staging harness needs its own bearer token — do NOT reuse the prod
# MESEDI_API_KEY. Mint a synthetic-customer key on staging and export as
# MESEDI_STAGING_API_KEY="Bearer mesedi_sk_..." before running.
API_AUTH_HEADER="Authorization"
API_AUTH_ENV="MESEDI_STAGING_API_KEY"

# Same value — staging has no scope split either.
API_CUSTOMER_AUTH_ENV="MESEDI_STAGING_API_KEY"

# Fuzz targets (unchanged from prod config).
FUZZ_ENDPOINTS=(
  "POST|/executions|true|"
  "POST|/events|true|"
  "GET|/executions|true|"
  "GET|/stats|true|"
)

# Auth boundary tests (unchanged).
AUTH_BOUNDARIES=(
  "customer|/admin/projects"
  "customer|/admin/storage"
  "customer|/admin/abuse"
)

# ── TLS posture (staging hostname) ───────────────────────────────────────
TLS_HOSTNAME="mesedi-api-staging.fly.dev"
TLS_PORT=443

# ── Load / soak (unchanged — 20s @ 10 RPS is safe on Fly ord region) ────
LOAD_DURATION="20s"
LOAD_RPS=10
LOAD_TARGET_PATH="/health"

# ── Advanced categories ──────────────────────────────────────────────────

# Cat. 23 horizontal BOLA — mint a SECOND staging customer key before
# running. If unset the BOLA test SKIPs cleanly rather than failing.
API_CUSTOMER_AUTH_ENV_2="MESEDI_STAGING_CUSTOMER_KEY_2"
BOLA_PROBES=(
  # Placeholder — swap in a known staging execution id from the
  # synthetic-customer project. If left as-is the probe returns 404
  # (still verifies cross-tenant isolation, just not on a real row).
  "GET|/executions/exec-staging-placeholder"
)

# Cat. 24 Slowloris — conservative defaults.
SLOWLORIS_N=30
SLOWLORIS_HOLD_S=8

# Cat. 27 HPP target.
HPP_TARGET_PATH="/health"

# Cat. 31 container image scan (unchanged — local Trivy against the
# locally-built image, no Fly registry pull).
CONTAINER_IMAGE="mesedi-api:stress-test"

# Cat. 34 second-order SQLi (unchanged — hits /api-keys round-trip).
SECOND_ORDER_SQLI_PROBES=(
  "POST|/api-keys|name|GET|/api-keys"
)

# Cat. 38 subdomain takeover scan — staging-relevant hosts.
SUBDOMAIN_TAKEOVER_TARGETS=(
  "mesedi.ai"
  "app.mesedi.ai"
  "api.mesedi.ai"
  "mesedi-api-staging.fly.dev"
)

# Cat. 40 WebSocket upgrade (staging matches prod: no WS surface).
WEBSOCKET_PATH="/ws"
