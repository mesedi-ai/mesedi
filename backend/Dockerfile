# Mesedi backend — multi-stage Docker build for Fly.io deploys.
#
# Stage 1: build a static Go binary against Go 1.25.
# modernc.org/sqlite is pure-Go (no CGO), so we set CGO_ENABLED=0
# and get a single self-contained binary.
#
# Stage 2: copy the binary into a minimal runtime image. We use
# alpine (not scratch) to get tzdata + ca-certificates for HTTPS
# calls to Stripe + webhook destinations, and for a basic shell
# during `fly ssh console` debugging.

FROM golang:1.26-alpine AS build

WORKDIR /src

# Copy module files first so `go mod download` can be cached
# independently of source-code edits.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source.
COPY . .

# Static build. -trimpath strips local paths from the binary;
# -ldflags="-s -w" strips symbol tables to shrink the binary.
#
# GOFIPS140=certified links the most recent Go Cryptographic Module that
# holds an actual CMVP validation certificate, and enables FIPS 140-3
# mode by default in the resulting binary. At the time of writing that
# resolves to Go Cryptographic Module v1.0.0, CMVP Certificate #5247.
#
# Why the build-time variable rather than the GODEBUG option: Go's own
# guidance is that "most programs should not set this option directly,
# and should instead use GOFIPS140 at build time". The GODEBUG value
# fips140=only exists for testing and assessment and is explicitly NOT
# intended for production; it is documented to introduce crashes and
# unhandled errors by design.
#
# Two operational notes:
#   - FIPS mode runs known-answer self-tests at init and pairwise
#     consistency tests on generated keys. Expect up to a 2x slowdown on
#     some key generation paths.
#   - FIPS 140-3 mode is unsupported on OpenBSD, Wasm, AIX and 32-bit
#     Windows. This image is linux/amd64, so it is fine.
#
# The running binary reports its own status at startup via
# crypto/fips140.Enabled(), so this is verifiable rather than asserted.
# It does not log the module version: crypto/fips140.Version() requires
# go1.26 and backend/go.mod declares go1.25. Worth adding when that moves.
# BUILD_GIT_SHA is stamped into the binary and reported by GET /health, so
# CI can ask the running service which commit it is and fail the deploy when
# the answer is wrong. Without it, a deploy that silently did nothing looks
# identical to one that worked — which is how Verdifax served a stale binary
# for two weeks behind green checkmarks.
#
# Defaults to "unknown" rather than being required. A local `docker build`
# with no --build-arg is a dev build and should say so honestly; only CI
# claims to be a specific commit.
ARG BUILD_GIT_SHA=unknown

# The -X target is `main.buildGitSHA` because cmd/api is package main.
# Deliberately NOT a path into an internal package: `go build` silently
# ignores an -X flag whose symbol does not resolve — no warning, exit 0 —
# and a package-qualified path breaks invisibly the day the module is
# renamed. That happened in verdifax-orchestrator on 2026-09-04 and cost
# eleven commits of falsely-stamped artifacts before anyone noticed.
# cmd/api/health_stamp_test.go pins this target to what main.go declares.
RUN CGO_ENABLED=0 GOOS=linux GOFIPS140=certified go build \
    -trimpath \
    -ldflags="-s -w -X 'main.buildGitSHA=${BUILD_GIT_SHA}'" \
    -o /out/mesedi-api \
    ./cmd/api

# ---

FROM alpine:3.20

# ca-certificates: TLS verification for outbound HTTPS (Stripe API,
# webhook destinations, etc.).
# tzdata: lets time.LoadLocation work for non-UTC formatting.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app
COPY --from=build /out/mesedi-api /app/mesedi-api

# Default port; can be overridden via MESEDI_PORT env var.
EXPOSE 8080

ENTRYPOINT ["/app/mesedi-api"]
