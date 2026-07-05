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

FROM golang:1.25-alpine AS build

WORKDIR /src

# Copy module files first so `go mod download` can be cached
# independently of source-code edits.
COPY go.mod go.sum ./
RUN go mod download

# Copy the rest of the source.
COPY . .

# Static build. -trimpath strips local paths from the binary;
# -ldflags="-s -w" strips symbol tables to shrink the binary.
RUN CGO_ENABLED=0 GOOS=linux go build \
    -trimpath \
    -ldflags="-s -w" \
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
