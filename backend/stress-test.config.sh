#!/usr/bin/env bash
# stress-test.config.sh for the Mesedi backend (Go service on Fly.io).
#
# Run via:
#     ~/stress-test/stress-test.sh ~/mesedi/backend
#
# The Python SDK (~/mesedi/sdk-python) and TypeScript SDK
# (~/mesedi/sdk-typescript) live outside this PROJECT_DIR. If you want
# pip-audit / npm-audit coverage on them, run the harness against
# those directories separately with their own config files.

# ── Project identity ──────────────────────────────────────────────────────
PROJECT_NAME="mesedi-backend"
PROJECT_LANGUAGE="go"

# ── MCD source scan ───────────────────────────────────────────────────────
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

# ── Dependency vulnerability scan ────────────────────────────────────────
DEPS_GO="go.mod"
DEPS_PYTHON=""
DEPS_NODE=""

# ── Static analysis ──────────────────────────────────────────────────────
STATIC_TOOLS="AUTO"   # picks golangci-lint for go projects

# ── Live API tests (against production) ──────────────────────────────────
API_BASE_URL="https://mesedi-api.fly.dev"
API_HEALTH_PATH="/health"

# Mesedi uses standard Bearer auth. The header name is literally
# "Authorization" and the env var value MUST include the "Bearer "
# prefix so the assembled header line reads:
#     Authorization: Bearer mesedi_sk_...
# Export with the prefix included:
#     export MESEDI_API_KEY="Bearer mesedi_sk_<your_test_key>"
API_AUTH_HEADER="Authorization"
API_AUTH_ENV="MESEDI_API_KEY"

# Mesedi does not have a customer-vs-admin SCOPE split inside its key
# system — admin endpoints sit behind a separately-configured
# adminToken (a Fly secret), not a derived scope on the regular key.
# Reusing MESEDI_API_KEY here verifies that a normal mesedi_sk_ token
# cannot reach /admin/* (it should hit AdminAuth and 401).
API_CUSTOMER_AUTH_ENV="MESEDI_API_KEY"

# Endpoints to fuzz with malformed payloads.
# Format: METHOD|PATH|REQUIRES_AUTH(true|false)|SAMPLE_PAYLOAD_FILE
FUZZ_ENDPOINTS=(
  "POST|/executions|true|"
  "POST|/events|true|"
  "GET|/executions|true|"
  "GET|/stats|true|"
)

# Auth boundary tests.
# Customer-scope token must receive 401/403 against admin endpoints.
AUTH_BOUNDARIES=(
  "customer|/admin/projects"
  "customer|/admin/storage"
  "customer|/admin/abuse"
)

# ── TLS posture ──────────────────────────────────────────────────────────
TLS_HOSTNAME="mesedi-api.fly.dev"
TLS_PORT=443

# ── Load / soak ──────────────────────────────────────────────────────────
LOAD_DURATION="20s"
LOAD_RPS=10
LOAD_TARGET_PATH="/health"

# ── Advanced (Tier 1+2+3+missed) categories ──────────────────────────────

# Cat. 23 horizontal BOLA: Customer-B trying to read Customer-A's resources.
# Requires two customer-scope keys minted via /signup (each gets its own
# project_id). Configure both, then point BOLA_PROBES at a Customer-A
# resource Customer-B should NOT be able to read.
API_CUSTOMER_AUTH_ENV_2="MESEDI_CUSTOMER_KEY_2"
BOLA_PROBES=(
  # exec-000eb09a839b is owned by Default project. Synthetic Org's
  # customer key (sent via MESEDI_CUSTOMER_KEY_2) should get 401/403/404
  # against this path. If it gets 200, Mesedi has a cross-tenant leak.
  "GET|/executions/exec-000eb09a839b"
)

# Cat. 24 Slowloris: conservative defaults so the test doesn't DoS prod.
SLOWLORIS_N=30
SLOWLORIS_HOLD_S=8

# Cat. 27 HPP target: cheap read-only endpoint.
HPP_TARGET_PATH="/health"

# Cat. 31 container image scan (Trivy). After building locally with
# `docker build -t mesedi-api:stress-test ~/mesedi/backend`, point at
# the local tag (Fly's registry doesn't allow pulls of deploy tags).
CONTAINER_IMAGE="mesedi-api:stress-test"

# Cat. 34 second-order SQLi marker (light). Tests the admin api-keys
# round-trip: name field is stored on POST, then read back on GET. If
# the marker crashes the read, the read path is constructing SQL via
# string concat against a user-controlled stored field. Requires the
# bearer in MESEDI_API_KEY to be admin-scope.
SECOND_ORDER_SQLI_PROBES=(
  "POST|/api-keys|name|GET|/api-keys"
)

# Cat. 38 subdomain takeover scan: Mesedi-owned hosts.
SUBDOMAIN_TAKEOVER_TARGETS=(
  "mesedi.ai"
  "app.mesedi.ai"
  "api.mesedi.ai"
)

# Cat. 40 WebSocket upgrade: Mesedi doesn't expose WebSockets, expect 404.
WEBSOCKET_PATH="/ws"
