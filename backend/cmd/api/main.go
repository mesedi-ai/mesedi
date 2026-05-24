// Command api is the Mesedi backend service: an HTTP server that ingests
// agent execution telemetry, runs detection engines against the event
// stream, and surfaces alerts via webhook + dashboard.
//
// See the per-component READMEs in this repo for runtime configuration,
// failure-class detectors, and SDK integration patterns.
//
// Configuration (12-factor, flags or env vars; flags win):
//
//	Flag             Env var                 Default
//	--port           MESEDI_PORT             8080
//	--log-level      MESEDI_LOG_LEVEL        info
//	--db-url         MESEDI_DB_URL           file:./mesedi-dev.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)
//	--dashboard-url  MESEDI_DASHBOARD_URL    (empty, falls back to request Host)
//
// MESEDI_DASHBOARD_URL is the public origin of the React dashboard
// (Vercel-hosted in prod, e.g. https://mesedi.vercel.app). When set,
// webhook payloads and embed deep-links use this base; otherwise the
// scheme+host of the inbound request is used (correct for same-origin
// dev setups, wrong when the backend and dashboard live on different
// hosts).
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"mesedi/backend/internal/api"
	"mesedi/backend/internal/dashboard"
	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// defaultDBURL is the SQLite DSN used when no MESEDI_DB_URL is provided.
// WAL journal mode enables concurrent readers while a writer holds the
// lock; foreign_keys=on enforces ON DELETE CASCADE behavior in the schema.
const defaultDBURL = "file:./mesedi-dev.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)"

const (
	serviceName    = "mesedi-backend"
	serviceVersion = "0.0.1"
)

type runtimeConfig struct {
	Port         int
	LogLevel     string
	DBURL        string
	DashboardURL string
	// Stripe billing config (#120). Any of these may be empty in
	// local dev, the billing endpoints respond 503 when missing.
	StripeSecretKey     string
	StripeWebhookSecret string
	StripeProPriceID    string
	// Admin dashboard bearer token (#150). When empty the /admin/*
	// routes refuse every request with 503 (fail-closed posture).
	AdminToken string
	// Transactional email via Resend (#127). When ResendAPIKey is
	// empty the backend uses a NoopMailer that swallows every send,
	// suitable for local dev and CI.
	ResendAPIKey string
	ResendFrom   string
}

// bootstrapDevProject creates a default "dev" project and a fixed test
// API key on first run. Idempotent, repeated runs return early when
// the project already exists. The test key value is intentionally
// non-secret and hardcoded; never use this pattern in production.
func bootstrapDevProject(ctx context.Context, st *store.SQLiteStore, logger *slog.Logger) error {
	const devProjectID = "proj-dev"
	const devKeyID = "key-dev"
	// SHA-256 of the literal string "mesedi_sk_dev_local_only", fixed so
	// SDK smoke tests can authenticate without per-run key minting during
	// local dev. Verify via: echo -n "mesedi_sk_dev_local_only" | sha256sum
	const devKeyHash = "63aee0bafbf5a68577021746b028842f70d922c2809776e1a1de0ecf6fc7fb33"

	if _, err := st.GetProject(ctx, devProjectID); err == nil {
		logger.Debug("dev project already exists", "project_id", devProjectID)
		return nil
	}

	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: devProjectID,
		Name:      "Local Dev",
	}); err != nil {
		return fmt.Errorf("create dev project: %w", err)
	}
	if err := st.CreateAPIKey(ctx, &store.APIKey{
		KeyID:     devKeyID,
		ProjectID: devProjectID,
		KeyHash:   devKeyHash,
		KeyPrefix: "mesedi_sk_dev",
		Name:      "Local dev key (no auth enforced in Phase 1.5)",
	}); err != nil {
		return fmt.Errorf("create dev api key: %w", err)
	}
	logger.Info("dev project bootstrapped", "project_id", devProjectID, "key_id", devKeyID)
	return nil
}

// redactDSN strips query-string credentials from a DSN before logging.
// For SQLite (file:./...) this is a no-op; for Postgres (postgres://user:pass@host/db)
// it returns just the scheme + host so passwords don't end up in stdout.
func redactDSN(dsn string) string {
	if !strings.HasPrefix(dsn, "postgres://") && !strings.HasPrefix(dsn, "postgresql://") {
		return dsn
	}
	at := strings.LastIndex(dsn, "@")
	if at < 0 {
		return dsn
	}
	scheme := strings.SplitN(dsn, "://", 2)
	if len(scheme) != 2 {
		return dsn
	}
	return scheme[0] + "://[redacted]@" + dsn[at+1:]
}

func main() {
	cfg := loadConfig()
	logger := newLogger(cfg.LogLevel)

	logger.Info("mesedi backend starting",
		"port", cfg.Port,
		"log_level", cfg.LogLevel,
		"db_url", redactDSN(cfg.DBURL),
	)

	// ── persistence ─────────────────────────────────────────────────
	// SQLite for local dev; Postgres implementation comes online when
	// MESEDI_DB_URL points at a postgres:// URL in a later slice.
	st, err := store.OpenSQLite(cfg.DBURL, logger)
	if err != nil {
		logger.Error("store open failed", "error", err.Error())
		os.Exit(1)
	}
	defer func() { _ = st.Close() }()
	logger.Info("store ready", "backend", "sqlite")

	// Bootstrap a dev project + API key on first run so the SDK has
	// something to authenticate against locally. Idempotent, repeated
	// runs are no-ops because the project already exists.
	if err := bootstrapDevProject(context.Background(), st, logger); err != nil {
		logger.Warn("bootstrap dev project failed (continuing)", "error", err.Error())
	}

	// Build the routing tree in three layers:
	//   1. `public`: routes that bypass auth (only /health today).
	//   2. `private`, routes that require a valid bearer token. Auth
	//                  middleware wraps these.
	//   3. `mux`: top-level router that fans out to public or
	//                  private as appropriate.
	//
	// Top-level middleware (recover, request log) wraps everything; auth
	// is scoped to the private subtree so the load-balancer probe at
	// /health is never blocked by an auth failure.
	publicMux := http.NewServeMux()
	publicMux.HandleFunc("GET /health", handleHealth(logger))
	// Local-dev dashboard: served from embedded files in the backend
	// binary itself, so same-origin (no CORS gymnastics) and no
	// separate web server needed. NOT the production dashboard. See
	// internal/dashboard/dashboard.go for the posture statement.
	publicMux.Handle("GET /ui/", dashboard.Handler())

	privateMux := http.NewServeMux()
	stripeCfg := api.StripeConfig{
		SecretKey:     cfg.StripeSecretKey,
		WebhookSecret: cfg.StripeWebhookSecret,
		ProPriceID:    cfg.StripeProPriceID,
	}
	logger.Info("stripe billing configured", "configured", stripeCfg.Configured())

	// Transactional email (#127). Falls back to NoopMailer when no
	// RESEND_API_KEY is configured so signups still complete in
	// local dev and CI.
	var mailer mail.Mailer
	if cfg.ResendAPIKey != "" {
		mailer = mail.NewResendMailer(cfg.ResendAPIKey, cfg.ResendFrom, logger)
		logger.Info("transactional email configured", "provider", "resend", "from", cfg.ResendFrom)
	} else {
		mailer = mail.NoopMailer{Logger: logger}
		logger.Info("transactional email disabled", "reason", "RESEND_API_KEY not set")
	}

	handlers := api.New(logger, st, cfg.DashboardURL, stripeCfg, mailer)
	handlers.RegisterRoutes(privateMux)
	privateHandler := api.NewAuthChain(logger, st)(privateMux)

	// Public POST /signup bypasses the bearer-token auth chain (visitors
	// have no key yet) but still needs CORS so the marketing site at
	// mesedi.vercel.app can POST cross-origin. The signup handler's
	// in-process IP rate limiter bounds abuse.
	signupMux := http.NewServeMux()
	handlers.RegisterPublicRoutes(signupMux)
	signupHandler := api.CORSMiddleware()(signupMux)

	// Founder-side admin dashboard (#150). Gated by MESEDI_ADMIN_TOKEN
	// bearer; refuses every request when the env var is empty so an
	// accidentally-misconfigured deploy can't leak project listings.
	// CORS so the dashboard at mesedi.vercel.app can call cross-origin.
	adminMux := http.NewServeMux()
	handlers.RegisterAdminRoutes(adminMux)
	adminHandler := api.CORSMiddleware()(api.AdminAuth(cfg.AdminToken)(adminMux))
	logger.Info("admin endpoints configured", "configured", cfg.AdminToken != "")

	mux := http.NewServeMux()
	mux.Handle("GET /health", publicMux)
	mux.Handle("GET /ui/", publicMux)
	mux.Handle("POST /signup", signupHandler)
	mux.Handle("OPTIONS /signup", signupHandler)
	mux.Handle("POST /executions", privateHandler)
	mux.Handle("PATCH /executions/{id}", privateHandler)
	mux.Handle("POST /events", privateHandler)
	// #118 Slice 1, dashboard reads the calling project's identity.
	mux.Handle("GET /project", privateHandler)
	// Phase 3b, read-side execution + stats surface for the dashboard.
	mux.Handle("GET /executions", privateHandler)
	mux.Handle("GET /executions/{id}", privateHandler)
	mux.Handle("GET /stats", privateHandler)
	// Phase 3a, failure_group read surface (auth-required).
	mux.Handle("GET /failure-groups", privateHandler)
	mux.Handle("GET /failure-groups/{id}", privateHandler)
	mux.Handle("GET /failure-groups/{id}/executions", privateHandler)
	// Phase 3b sub-slice 18, API key management (auth-required).
	mux.Handle("GET /api-keys", privateHandler)
	mux.Handle("POST /api-keys", privateHandler)
	mux.Handle("DELETE /api-keys/{id}", privateHandler)
	// Sub-slice 21b, SSE remote-halt channel (auth-required).
	mux.Handle("GET /executions/{id}/halt-stream", privateHandler)
	mux.Handle("POST /executions/{id}/halt", privateHandler)
	// Tier 1 Playbooks (auth-required).
	mux.Handle("GET /playbooks", privateHandler)
	// Task #83, webhook escalation config + dispatcher (auth-required).
	mux.Handle("GET /webhooks", privateHandler)
	mux.Handle("POST /webhooks", privateHandler)
	mux.Handle("DELETE /webhooks/{id}", privateHandler)
	mux.Handle("POST /webhooks/{id}/test", privateHandler)
	mux.Handle("GET /webhooks/{id}/deliveries", privateHandler)
	// #120, Stripe billing (auth-required for everything except
	// the Stripe-server-to-server webhook receiver, which is wired
	// below alongside the signup endpoint).
	mux.Handle("GET /billing", privateHandler)
	mux.Handle("GET /billing/usage", privateHandler)
	mux.Handle("POST /billing/checkout", privateHandler)
	mux.Handle("POST /billing/portal", privateHandler)
	mux.Handle("POST /billing/webhook", signupHandler)
	// Founder-side admin dashboard (#150). Token-gated; refuses every
	// request when MESEDI_ADMIN_TOKEN is empty. CORS preflight OPTIONS
	// is needed because the dashboard at mesedi.vercel.app calls
	// cross-origin from a different host than mesedi-api.fly.dev.
	mux.Handle("GET /admin/projects", adminHandler)
	mux.Handle("OPTIONS /admin/projects", adminHandler)
	mux.Handle("GET /admin/projects/{id}", adminHandler)
	mux.Handle("OPTIONS /admin/projects/{id}", adminHandler)
	mux.Handle("POST /admin/projects/{id}/tier", adminHandler)
	mux.Handle("OPTIONS /admin/projects/{id}/tier", adminHandler)
	mux.Handle("POST /admin/projects/{id}/grant", adminHandler)
	mux.Handle("OPTIONS /admin/projects/{id}/grant", adminHandler)
	mux.Handle("GET /admin/projects/{id}/export", adminHandler)
	mux.Handle("OPTIONS /admin/projects/{id}/export", adminHandler)
	mux.Handle("DELETE /admin/projects/{id}", adminHandler)
	mux.Handle("GET /admin/storage", adminHandler)
	mux.Handle("OPTIONS /admin/storage", adminHandler)

	// Top-level middleware: recover from panics, log every request.
	root := api.NewTopChain(logger)(mux)

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           root,
		ReadHeaderTimeout: 5 * time.Second,
	}

	// Graceful shutdown on SIGINT / SIGTERM.
	go func() {
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, os.Interrupt, syscall.SIGTERM)
		<-sigCh
		logger.Info("shutdown signal received")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := srv.Shutdown(ctx); err != nil {
			logger.Error("graceful shutdown failed", "error", err.Error())
		}
	}()

	logger.Info("http server listening", "addr", srv.Addr)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		logger.Error("http server died", "error", err.Error())
		os.Exit(1)
	}
	logger.Info("http server stopped cleanly")
}

// handleHealth returns the standard health-check endpoint with the
// conventional shape (ok, service, version, time). Adds git_sha once we
// have a build pipeline injecting it via -ldflags.
func handleHealth(logger *slog.Logger) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"ok":true,"service":%q,"version":%q,"time":%q}`,
			serviceName,
			serviceVersion,
			time.Now().UTC().Format(time.RFC3339),
		)
	}
}

func loadConfig() runtimeConfig {
	cfg := runtimeConfig{
		Port:                envInt("MESEDI_PORT", 8080),
		LogLevel:            envString("MESEDI_LOG_LEVEL", "info"),
		DBURL:               envString("MESEDI_DB_URL", defaultDBURL),
		DashboardURL:        envString("MESEDI_DASHBOARD_URL", ""),
		StripeSecretKey:     envString("MESEDI_STRIPE_SECRET_KEY", ""),
		StripeWebhookSecret: envString("MESEDI_STRIPE_WEBHOOK_SECRET", ""),
		StripeProPriceID:    envString("MESEDI_STRIPE_PRO_PRICE_ID", ""),
		AdminToken:          envString("MESEDI_ADMIN_TOKEN", ""),
		ResendAPIKey:        envString("RESEND_API_KEY", ""),
		ResendFrom:          envString("MESEDI_MAIL_FROM", "Mesedi <onboarding@resend.dev>"),
	}
	flag.IntVar(&cfg.Port, "port", cfg.Port, "TCP port for the HTTP API")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log verbosity: debug | info | warn | error")
	flag.StringVar(&cfg.DBURL, "db-url", cfg.DBURL, "Postgres connection string (required for Phase 1.5+)")
	flag.StringVar(&cfg.DashboardURL, "dashboard-url", cfg.DashboardURL, "public origin of the React dashboard (e.g. https://mesedi.vercel.app)")
	flag.Parse()
	return cfg
}

func newLogger(levelName string) *slog.Logger {
	level := slog.LevelInfo
	switch strings.ToLower(strings.TrimSpace(levelName)) {
	case "debug":
		level = slog.LevelDebug
	case "info":
		level = slog.LevelInfo
	case "warn", "warning":
		level = slog.LevelWarn
	case "error":
		level = slog.LevelError
	}
	handler := slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level})
	return slog.New(handler).With(
		"service", serviceName,
		"version", serviceVersion,
	)
}

func envString(key, fallback string) string {
	if v, ok := os.LookupEnv(key); ok && v != "" {
		return v
	}
	return fallback
}

func envInt(key string, fallback int) int {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return fallback
	}
	var parsed int
	if _, err := fmt.Sscanf(v, "%d", &parsed); err != nil {
		return fallback
	}
	return parsed
}
