// Command api is the Mesedi backend service: an HTTP server that ingests
// agent execution telemetry, runs detection engines against the event
// stream, and surfaces alerts via webhook + dashboard.
//
// See the per-component READMEs in this repo for runtime configuration,
// failure-class detectors, and SDK integration patterns.
//
// Configuration (12-factor, flags or env vars; flags win):
//
//	Flag                  Env var                       Default
//	--port                MESEDI_PORT                   8080
//	--log-level           MESEDI_LOG_LEVEL              info
//	--db-url              MESEDI_DB_URL                 file:./mesedi-dev.db?_pragma=journal_mode(WAL)&_pragma=foreign_keys(on)
//	--db-url-postgres     MESEDI_DB_URL_POSTGRES        (empty; when set, Postgres backend used; SQLite fallback otherwise)
//	--dashboard-url       MESEDI_DASHBOARD_URL          (empty, falls back to request Host)
//
// When MESEDI_DB_URL_POSTGRES is set (non-empty), the backend connects
// to Postgres via the pgx driver and runs the postgres-flavored
// migrations from internal/store/migrations-postgres/. When unset,
// the backend falls back to SQLite via MESEDI_DB_URL (the v0.1
// production posture on Fly volumes). Phase 1 ships the dispatch
// + the connect/migrate path; Phase 2 ports the remaining Store
// methods (anything beyond CreateProject/GetProject/DeleteProject
// currently returns ErrPostgresNotYetPorted).
//
// MESEDI_DASHBOARD_URL is the public origin of the React dashboard
// (Cloudflare Workers in prod, e.g. https://app.mesedi.ai). When set,
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

	"mesedi/backend/internal/anthropic"
	"mesedi/backend/internal/api"
	"mesedi/backend/internal/dashboard"
	"mesedi/backend/internal/mail"
	meseditel "mesedi/backend/internal/otel"
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
	Port          int
	LogLevel      string
	DBURL         string
	DBURLPostgres string
	DashboardURL  string
	// Stripe billing config. Any of these may be empty in
	// local dev, the billing endpoints respond 503 when missing.
	StripeSecretKey         string
	// StripeSecretKeyTest (follow-on): optional test-mode API
	// secret. Pairs with StripeWebhookSecretTest so test-mode events
	// not only validate signature but also let handlers call back to
	// Stripe (e.g., charge.Get to enrich a dispute) without hitting
	// 401 on a live key against a test-mode object.
	StripeSecretKeyTest     string
	StripeWebhookSecret     string
	// StripeWebhookSecretTest: optional second webhook signing
	// secret so test-mode events from the Stripe Dashboard validate
	// against the same endpoint as live events. Empty = behavior
	// identical to the single-secret path.
	StripeWebhookSecretTest string
	// StripeTeamPriceID is the Stripe Price ID for the $99/mo Team
	// plan. Set via MESEDI_STRIPE_TEAM_PRICE_ID. The legacy
	// MESEDI_STRIPE_PRO_PRICE_ID env var is also honored at startup
	// as a fallback so an existing deploy doesn't lose billing
	// across the pricing-rename slice.
	StripeTeamPriceID string
	// Admin dashboard bearer token. When empty the /admin/*
	// routes refuse every request with 503 (fail-closed posture).
	AdminToken string
	// Transactional email via Resend. When ResendAPIKey is
	// empty the backend uses a NoopMailer that swallows every send,
	// suitable for local dev and CI.
	ResendAPIKey string
	ResendFrom   string
	// 5xx alert webhook URL. When non-empty the request-log
	// middleware POSTs an alert on every 5xx response.
	AlertWebhookURL string
	// SigninSecret guards POST /signin, the server-to-server
	// endpoint the dashboard server calls during the SSO callback +
	// magic-link verify flows. Empty means the dashboard server
	// CANNOT log existing customers in via SSO; the signup-only flow
	// still works. Set MESEDI_SIGNIN_SECRET to a high-entropy random
	// string (32+ bytes base64) on both the backend (Fly) and the
	// dashboard server (Cloudflare Workers).
	SigninSecret string
	// TOTPEncryptionKey is the AES-256-GCM key used to seal customer
	// TOTP secrets at rest. Hex-encoded, 64 chars. Empty
	// disables the 2FA endpoints (they 503). Loaded from
	// MESEDI_TOTP_ENCRYPTION_KEY.
	TOTPEncryptionKey string
}

// bootstrapDevProject creates a default "dev" project and a fixed test
// API key on first run. Idempotent, repeated runs return early when
// the project already exists. The test key value is intentionally
// non-secret and hardcoded; never use this pattern in production.
//
// Takes store.Store so it works against either SQLite or Postgres. On
// the Postgres backend during Phase 1, CreateAPIKey returns
// ErrPostgresNotYetPorted, the caller logs the warning and continues.
// Once Phase 2 ports CreateAPIKey this restriction goes away.
func bootstrapDevProject(ctx context.Context, st store.Store, logger *slog.Logger) error {
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

// bootstrapAdminProject ensures the synthetic project that holds
// admin-scope API keys exists. Idempotent: a no-op once the row is
// present. Unlike bootstrapDevProject, this runs in production too
// because the project is required for the new admin-keys surface to
// function (admin keys carry project_id=store.APIKeyAdminProjectID
// so they participate in the api_keys.project_id FK constraint).
//
// The project carries no API keys at startup; admin keys are minted
// later via POST /admin/api-keys (either by the legacy
// MESEDI_ADMIN_TOKEN holder during transition, or by an existing
// admin-scope key once one has been minted).
func bootstrapAdminProject(ctx context.Context, st store.Store, logger *slog.Logger) error {
	if _, err := st.GetProject(ctx, store.APIKeyAdminProjectID); err == nil {
		logger.Debug("admin project already exists", "project_id", store.APIKeyAdminProjectID)
		return nil
	}
	if err := st.CreateProject(ctx, &store.Project{
		ProjectID: store.APIKeyAdminProjectID,
		Name:      "Mesedi Admin (system)",
	}); err != nil {
		return fmt.Errorf("create admin project: %w", err)
	}
	logger.Info("admin project bootstrapped", "project_id", store.APIKeyAdminProjectID)
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

	// Effective DSN: prefer the Postgres URL when set, fall back to
	// the SQLite path. Logged form is redacted so passwords don't
	// reach stdout. Backend tag indicates which engine actually opened.
	effectiveDSN := cfg.DBURL
	backend := "sqlite"
	if strings.TrimSpace(cfg.DBURLPostgres) != "" {
		effectiveDSN = cfg.DBURLPostgres
		backend = "postgres"
	}
	logger.Info("mesedi backend starting",
		"port", cfg.Port,
		"log_level", cfg.LogLevel,
		"db_url", redactDSN(effectiveDSN),
		"backend_selected", backend,
	)

	// ── persistence ─────────────────────────────────────────────────
	// Dispatch by which DSN is set:
	//   MESEDI_DB_URL_POSTGRES set, non-empty -> Postgres (pgx driver)
	//   otherwise                            -> SQLite (Fly volume v0.1 posture)
	//
	// Phase 2 (shipped 2026-05-25): every Store method is ported to
	// Postgres. The remaining steps before flipping production are:
	//   1. Build + run the test suite against a local Postgres / Neon
	//   2. Run the data-migration tool to copy live SQLite -> Postgres
	//   3. Confirm parity (row counts, spot-check a few SDK round trips)
	//   4. Flip MESEDI_DB_URL_POSTGRES via fly secrets and `fly deploy`
	var st store.Store
	if backend == "postgres" {
		pst, err := store.OpenPostgres(cfg.DBURLPostgres, logger)
		if err != nil {
			logger.Error("postgres store open failed", "error", err.Error())
			os.Exit(1)
		}
		st = pst
	} else {
		sst, err := store.OpenSQLite(cfg.DBURL, logger)
		if err != nil {
			logger.Error("sqlite store open failed", "error", err.Error())
			os.Exit(1)
		}
		st = sst
	}
	defer func() { _ = st.Close() }()
	logger.Info("store ready", "backend", backend)

	// Bootstrap a dev project + API key on first run so the SDK has
	// something to authenticate against locally. Idempotent, repeated
	// runs are no-ops because the project already exists. Works
	// against both SQLite and Postgres backends (Phase 2 ported
	// CreateAPIKey on the Postgres path).
	//
	// Production guard: Fly sets FLY_APP_NAME in every running machine.
	// When that env var is present we skip the bootstrap entirely so
	// the well-known dev API key (SHA-256 of "mesedi_sk_dev_local_only")
	// never lands in a production database. This re-applies the same
	// posture that established when it manually revoked the
	// key from the original Fly+SQLite production DB, so the Postgres
	// cutover doesn't accidentally re-introduce the backdoor on a
	// fresh Neon database.
	if os.Getenv("FLY_APP_NAME") != "" {
		logger.Info("bootstrap dev project skipped (production, FLY_APP_NAME set)")
	} else if err := bootstrapDevProject(context.Background(), st, logger); err != nil {
		logger.Warn("bootstrap dev project failed (continuing)", "error", err.Error())
	}

	// Bootstrap the synthetic _admin project that holds admin-scope API
	// keys (migration 015). Runs in production as well as dev because
	// the row is required for the /admin/api-keys surface to mint
	// anything; it carries no keys until an operator explicitly mints
	// one. Idempotent across restarts.
	if err := bootstrapAdminProject(context.Background(), st, logger); err != nil {
		logger.Warn("bootstrap admin project failed (continuing)", "error", err.Error())
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
		SecretKey:         cfg.StripeSecretKey,
		SecretKeyTest:     cfg.StripeSecretKeyTest,
		WebhookSecret:     cfg.StripeWebhookSecret,
		WebhookSecretTest: cfg.StripeWebhookSecretTest,
		TeamPriceID:       cfg.StripeTeamPriceID,
	}
	logger.Info("stripe billing configured",
		"configured", stripeCfg.Configured(),
		"webhook_test_secret_set", cfg.StripeWebhookSecretTest != "",
		"api_test_key_set", cfg.StripeSecretKeyTest != "",
	)

	// 5xx alert webhook. When set, the request-log middleware
	// POSTs an alert payload to this URL on every 5xx response so the
	// operator gets paged before customers notice. Auto-detects Slack
	// and Discord webhook URL shapes. Empty disables alerting.
	api.SetAlertWebhookURL(cfg.AlertWebhookURL)
	if cfg.AlertWebhookURL != "" {
		logger.Info("alert webhook configured", "host_present", true)
	} else {
		logger.Info("alert webhook disabled", "reason", "MESEDI_ALERT_WEBHOOK_URL not set")
	}

	// Transactional email. Falls back to NoopMailer when no
	// RESEND_API_KEY is configured so signups still complete in
	// local dev and CI.
	//
	// Loud startup WARN if the From is still the Resend sandbox
	// "onboarding@resend.dev" sender. That address only delivers to
	// the Resend account owner's own email; every send to a real
	// customer or invitee gets a 403 validation_error and is silently
	// swallowed by the call-site (which logs but doesn't surface to
	// the customer). Triggered the org-invite bug 2026-05-31; the
	// WARN below makes a repeat trip-up impossible to miss on deploy.
	var mailer mail.Mailer
	if cfg.ResendAPIKey != "" {
		mailer = mail.NewResendMailer(cfg.ResendAPIKey, cfg.ResendFrom, logger)
		logger.Info("transactional email configured", "provider", "resend", "from", cfg.ResendFrom)
		if strings.Contains(cfg.ResendFrom, "@resend.dev") {
			logger.Warn(
				"MESEDI_MAIL_FROM is the Resend sandbox sender; outbound emails will only deliver to the Resend account owner. Set MESEDI_MAIL_FROM to an address on a verified domain (e.g. 'Mesedi <hello@mesedi.ai>') to fix.",
				"current_from", cfg.ResendFrom,
			)
		}
	} else {
		mailer = mail.NoopMailer{Logger: logger}
		logger.Info("transactional email disabled", "reason", "RESEND_API_KEY not set")
	}

	handlers := api.New(logger, st, cfg.DashboardURL, stripeCfg, mailer)
	// — SSO + magic-link sign-in. The dashboard server calls
	// POST /signin from its OAuth callback / magic-link verify
	// routes; empty secret leaves /signin returning 503 so a
	// misconfigured deploy fails loudly rather than letting random
	// callers mint login keys.
	handlers.SigninSecret = cfg.SigninSecret
	if cfg.SigninSecret != "" {
		logger.Info("signin endpoint enabled")
	} else {
		logger.Info("signin endpoint disabled (MESEDI_SIGNIN_SECRET unset; SSO login will 503)")
	}

	// customer-facing 2FA. The encryption key seals customer
	// TOTP secrets at rest with AES-256-GCM. An invalid key surfaces
	// as a startup fatal: a 2FA deploy with a broken key is worse
	// than a 2FA-disabled deploy because customers would think their
	// secrets are safe when they aren't.
	if cfg.TOTPEncryptionKey != "" {
		raw, perr := api.ParseTOTPEncryptionKey(cfg.TOTPEncryptionKey)
		if perr != nil {
			logger.Error("totp: encryption key invalid (2fa endpoints will 503)",
				"error", perr.Error(),
			)
		} else {
			handlers.TOTPEncryptionKey = raw
			logger.Info("2fa endpoints enabled")
		}
	} else {
		logger.Info("2fa endpoints disabled (MESEDI_TOTP_ENCRYPTION_KEY unset; /me/2fa/* will 503)")
	}

	// — OpenTelemetry parallel emission. Initialize the
	// global emitter from env (OTEL_EXPORTER_OTLP_ENDPOINT etc.).
	// When the endpoint is unset, Init returns a disabled emitter
	// that no-ops on every Emit call, so the wiring is safe to
	// leave in always-on. A construction failure is logged but
	// non-fatal: the rest of the API continues to serve without
	// OTel.
	otelEmitter, otelErr := meseditel.Init(context.Background(), logger)
	if otelErr != nil {
		logger.Warn("otel: emitter init failed (continuing without OTel emission)",
			"error", otelErr.Error(),
		)
	} else {
		handlers.OTel = otelEmitter
		// Flush pending spans on graceful shutdown. fly.io's
		// SIGTERM gives us ~30s before SIGKILL; a 5s shutdown
		// budget leaves headroom for the rest of the runtime to
		// drain cleanly.
		defer func() {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := otelEmitter.Shutdown(ctx); err != nil {
				logger.Warn("otel: shutdown returned error", "error", err.Error())
			}
		}()
	}

	// — LLM-assisted root-cause analysis. The client is
	// disabled when ANTHROPIC_API_KEY is unset; the handler returns
	// 503 in that case so the dashboard can render a "not configured"
	// state rather than a 500. No retry/backoff: a single API failure
	// surfaces to the customer; they can re-click Analyze.
	anthropicClient := anthropic.New(os.Getenv("ANTHROPIC_API_KEY"), "", "")
	handlers.Anthropic = anthropicClient
	logger.Info("anthropic client configured",
		"enabled", anthropicClient.Enabled())

	// — founder burn-rate widget on /admin. Uses the
	// Anthropic Admin API Cost Report endpoint via the
	// ANTHROPIC_ADMIN_KEY env var (sk-ant-admin-...). Disabled when
	// the env var is empty; the admin handler returns a "not
	// configured" response and the dashboard renders an empty-state.
	anthropicAdminClient := anthropic.NewAdminClient(os.Getenv("ANTHROPIC_ADMIN_KEY"), nil)
	handlers.AnthropicAdmin = anthropicAdminClient
	logger.Info("anthropic admin client configured",
		"enabled", anthropicAdminClient.Configured())

	handlers.RegisterRoutes(privateMux)
	// Request-log middleware wraps the auth chain so it can
	// read the project + key IDs the chain stamps into the context
	// AND capture the response status code on the way out. Hobby +
	// Enterprise tiers are skipped inside the middleware; only Team
	// requests land a row in the request_log table.
	requestLogger := &api.RequestLogger{
		Store:  st,
		Logger: logger,
	}
	privateHandler := requestLogger.Middleware(
		api.NewAuthChain(logger, st, handlers.Abuse)(privateMux),
	)

	// Abuse-detection background worker. Reads unresolved
	// signals every few minutes, sends the 24h-warning email, then
	// auto-suspends the project 24h later if still unresolved. Uses
	// context.Background() so the worker runs for the life of the
	// process; the OS kills it on SIGTERM along with the HTTP server.
	api.StartAbuseWorker(context.Background(), st, mailer, logger, cfg.DashboardURL)

	// failure_class aggregate worker. Runs once on startup +
	// every 24h, refreshing the current-month row so mid-month
	// account closures don't lose data.
	api.StartFailureClassAggregateWorker(context.Background(), st, logger)

	// Tenant budget-ceiling scheduler. Walks every
	// tenant_budget_ceilings row every 5 minutes, evaluates MTD burn
	// against the configured ceiling, and (on first crossing within
	// the calendar month) fires email + webhook notifications and
	// (when BreachAction == "halt") halts every active execution
	// under the tenant. Same context.Background() lifetime as the
	// abuse worker.
	budgetCeilingScheduler := &api.BudgetCeilingScheduler{
		Store:        st,
		Logger:       logger,
		HaltSubs:     handlers.HaltSubs,
		Mailer:       mailer,
		WebhookHTTP:  &http.Client{Timeout: 10 * time.Second},
		DashboardURL: cfg.DashboardURL,
	}
	budgetCeilingScheduler.Start(context.Background())

	// Data retention scheduler. Daily tick walks every project
	// with retention_days set and prunes executions outside the
	// window. Indefinite-retention projects are excluded at the
	// query level (ListProjectsForRetention). Same context lifetime
	// as the abuse + budget-ceiling workers.
	retentionScheduler := &api.RetentionScheduler{
		Store:  st,
		Logger: logger,
	}
	retentionScheduler.Start(context.Background())

	// Closed-project audit_events retention scheduler. Daily
	// tick prunes audit rows whose project_deleted_at is older than
	// the 7-year SOC 2 / financial-services retention window. Only
	// closed-project rows (project_deleted_at IS NOT NULL) are
	// eligible; live-project audit history is untouched. The default
	// retention window comes from api.DefaultAuditEventsRetention;
	// override here for stricter (e.g. EU-only) deploys.
	auditEventsRetentionScheduler := &api.AuditEventsRetentionScheduler{
		Store:  st,
		Logger: logger,
	}
	auditEventsRetentionScheduler.Start(context.Background())

	// request_log retention scheduler. Daily tick prunes rows
	// older than 90 days from the request_log table. Keeps the
	// forensic-attribution window long enough for typical compromise
	// investigations without letting the table balloon Neon storage.
	requestLogRetentionScheduler := &api.RequestLogRetentionScheduler{
		Store:  st,
		Logger: logger,
	}
	requestLogRetentionScheduler.Start(context.Background())

	// Hobby billing scheduler (). Daily tick walks every
	// Hobby-tier project whose billing period has rolled over, attempts
	// the off-session overage charge against the saved Stripe payment
	// method, and advances the period bounds on success. Retries every
	// 48 hours on failed charges; auto-detaches the saved card after
	// 5 consecutive failures. Also bootstraps NULL period bounds on
	// existing Hobby projects on first sight. No-op when Stripe is not
	// configured (CE / local dev).
	hobbyBillingScheduler := &api.HobbyBillingScheduler{
		Store:        st,
		Stripe:       handlers.Stripe,
		Mailer:       mailer,
		DashboardURL: cfg.DashboardURL,
		Logger:       logger,
	}
	hobbyBillingScheduler.Start(context.Background())

	// Public POST /signup bypasses the bearer-token auth chain (visitors
	// have no key yet) but still needs CORS so the marketing site at
	// mesedi.ai can POST cross-origin. The signup handler's
	// in-process IP rate limiter bounds abuse.
	signupMux := http.NewServeMux()
	handlers.RegisterPublicRoutes(signupMux)
	signupHandler := api.CORSMiddleware()(signupMux)

	// Founder-side admin dashboard. Two auth paths accepted by
	// AdminAuth (see internal/api/admin.go):
	//   1. Legacy: bearer matches MESEDI_ADMIN_TOKEN (Fly secret).
	//   2. Scoped: bearer is a mesedi_sk_ key with scope='admin'
	//      (migration 015), looked up via the store.
	// Fails closed with 503 when neither is configured. CORS so the
	// dashboard at app.mesedi.ai can call cross-origin.
	adminMux := http.NewServeMux()
	handlers.RegisterAdminRoutes(adminMux)
	adminHandler := api.CORSMiddleware()(api.AdminAuth(cfg.AdminToken, st)(adminMux))
	logger.Info("admin endpoints configured",
		"legacy_token", cfg.AdminToken != "",
		"scoped_keys", true,
	)

	mux := http.NewServeMux()
	mux.Handle("GET /health", publicMux)
	mux.Handle("GET /ui/", publicMux)
	mux.Handle("POST /signup", signupHandler)
	mux.Handle("OPTIONS /signup", signupHandler)
	// — POST /signin is a server-to-server endpoint guarded by
	// MESEDI_SIGNIN_SECRET, registered on the same public mux so it
	// bypasses the bearer-token auth chain (the dashboard server has
	// no customer key when calling /signin).
	mux.Handle("POST /signin", signupHandler)
	mux.Handle("OPTIONS /signin", signupHandler)
	// commit 2 — magic-link routes. /start is browser-callable
	// (rate-limited by IP, no secret). /verify is server-to-server
	// (gated inside the handler by MESEDI_SIGNIN_SECRET).
	mux.Handle("POST /magic-link/start", signupHandler)
	mux.Handle("OPTIONS /magic-link/start", signupHandler)
	mux.Handle("GET /magic-link/verify", signupHandler)
	mux.Handle("OPTIONS /magic-link/verify", signupHandler)
	// Batch 2 — POST /auth/logout destroys the caller's session
	// cookie. Intentionally on the public mux: a customer who has
	// already lost their session row (expired, kicked, key revoked)
	// should still be able to click Sign Out without a 401.
	mux.Handle("POST /auth/logout", signupHandler)
	mux.Handle("OPTIONS /auth/logout", signupHandler)
	// — server-to-server 2FA verify. Same shape as /signin:
	// shared secret in X-Mesedi-Signin-Secret, called by the
	// dashboard Worker after the customer enters their authenticator
	// code on /login/2fa. Lives on signupHandler so it bypasses the
	// bearer-token auth chain (the Worker has no customer key here).
	mux.Handle("POST /auth/2fa-verify", signupHandler)
	mux.Handle("OPTIONS /auth/2fa-verify", signupHandler)
	// email-verification confirm + resend. Public from the HTTP
	// layer (the customer may not be signed in when they click the
	// link in their welcome email). Authenticity is provided by the
	// token in the request body (confirm) or by the per-email rate
	// limit inside the handler (resend; always 202s to avoid an
	// existence oracle). Routed to signupHandler (CORSMiddleware over
	// signupMux) so the public-route set in RegisterPublicRoutes is
	// reached without the bearer-token auth chain.
	mux.Handle("POST /api/email-verify/confirm", signupHandler)
	mux.Handle("OPTIONS /api/email-verify/confirm", signupHandler)
	mux.Handle("POST /api/email-verify/resend", signupHandler)
	mux.Handle("OPTIONS /api/email-verify/resend", signupHandler)
	mux.Handle("POST /executions", privateHandler)
	mux.Handle("PATCH /executions/{id}", privateHandler)
	mux.Handle("POST /events", privateHandler)
	// Slice 1, dashboard reads the calling project's identity.
	mux.Handle("GET /project", privateHandler)
	// — customer-side project rename so SSO-created
	// projects can move off the default "Default project" name.
	// Admin scope required; enforced inside the handler.
	mux.Handle("PATCH /project/name", privateHandler)
	mux.Handle("GET /me", privateHandler)
	// email-verification status. Bearer-gated so the dashboard's
	// /verify-email interstitial can poll its own project's verified
	// flag. Listed in emailVerifyExemptPaths inside auth.go so the
	// gate itself does NOT block this route (otherwise the
	// dashboard could never learn it's been verified).
	mux.Handle("GET /me/email-verification-status", privateHandler)
	mux.Handle("OPTIONS /me/email-verification-status", privateHandler)
	// Phase 3b, read-side execution + stats surface for the dashboard.
	mux.Handle("GET /executions", privateHandler)
	mux.Handle("GET /executions/{id}", privateHandler)
	// multi-agent topology graph (auth-required).
	mux.Handle("GET /executions/{id}/topology", privateHandler)
	mux.Handle("GET /stats", privateHandler)
	// Phase 3a, failure_group read surface (auth-required).
	mux.Handle("GET /failure-groups", privateHandler)
	mux.Handle("GET /failure-groups/{id}", privateHandler)
	mux.Handle("GET /failure-groups/{id}/executions", privateHandler)
	// LLM-assisted root-cause analysis (auth-required).
	// Cached on the failure_group row; ?regenerate=1 forces a refresh.
	mux.Handle("POST /failure-groups/{id}/analyze", privateHandler)
	// failure-group-resolve wave — customer-initiated resolve /
	// unresolve. Auth-required. The inner mux's HandleResolveFailureGroup
	// / HandleUnresolveFailureGroup are unreachable from the outer
	// dispatch table without these forwards (see the top-level mux
	// dispatch lesson in MESEDI_TASK_INVENTORY.md).
	mux.Handle("POST /failure-groups/{id}/resolve", privateHandler)
	mux.Handle("POST /failure-groups/{id}/unresolve", privateHandler)
	// cost-by-tenant attribution report (auth-required).
	mux.Handle("GET /reports/cost-by-tenant", privateHandler)
	// Phase 3b sub-slice 18, API key management (auth-required).
	mux.Handle("GET /api-keys", privateHandler)
	mux.Handle("POST /api-keys", privateHandler)
	mux.Handle("DELETE /api-keys/{id}", privateHandler)
	// Audit logs v1 (auth-required; admin-role guard inside handler).
	mux.Handle("GET /audit-log", privateHandler)
	// Sub-slice 21b, SSE remote-halt channel (auth-required).
	mux.Handle("GET /executions/{id}/halt-stream", privateHandler)
	mux.Handle("POST /executions/{id}/halt", privateHandler)
	// Tier 1 Playbooks (auth-required).
	mux.Handle("GET /playbooks", privateHandler)
	// webhook escalation config + dispatcher (auth-required).
	mux.Handle("GET /webhooks", privateHandler)
	mux.Handle("POST /webhooks", privateHandler)
	mux.Handle("DELETE /webhooks/{id}", privateHandler)
	mux.Handle("POST /webhooks/{id}/test", privateHandler)
	mux.Handle("GET /webhooks/{id}/deliveries", privateHandler)
	// , Stripe billing (auth-required for everything except
	// the Stripe-server-to-server webhook receiver, which is wired
	// below alongside the signup endpoint).
	mux.Handle("GET /billing", privateHandler)
	mux.Handle("GET /billing/usage", privateHandler)
	// : AI root-cause analyses usage counter for the user dashboard.
	mux.Handle("GET /billing/ai-analyses-usage", privateHandler)
	mux.Handle("POST /billing/checkout", privateHandler)
	mux.Handle("POST /billing/portal", privateHandler)
	// Hobby Setup Intent flow: the dashboard POSTs here to get
	// a Stripe Checkout URL in setup-mode so the customer can attach
	// a card. Without this outer-mux forwarding the route was reachable
	// on the privateMux but the request never landed there (bug).
	mux.Handle("POST /billing/payment-method/setup", privateHandler)
	mux.Handle("POST /billing/payment-method/remove", privateHandler)
	mux.Handle("PUT /billing/cap", privateHandler)
	// : danger-zone flows.
	mux.Handle("POST /billing/downgrade", privateHandler)
	mux.Handle("POST /billing/close-account", privateHandler)
	mux.Handle("POST /billing/webhook", signupHandler)
	// tenant-wide org rollup.
	mux.Handle("GET /me/rollup", privateHandler)
	mux.Handle("GET /me/savings", privateHandler)
	// tenant monthly budget ceiling.
	mux.Handle("GET /me/budget-ceiling", privateHandler)
	mux.Handle("PUT /me/budget-ceiling", privateHandler)
	// per-project failure-class severity overrides.
	mux.Handle("GET /me/class-severities", privateHandler)
	mux.Handle("PUT /me/class-severities/{class}", privateHandler)
	mux.Handle("DELETE /me/class-severities/{class}", privateHandler)
	// per-project data retention configuration.
	mux.Handle("GET /me/retention", privateHandler)
	mux.Handle("PUT /me/retention", privateHandler)
	// per-project provider_incident detector threshold.
	mux.Handle("GET /me/provider-incident-config", privateHandler)
	mux.Handle("PUT /me/provider-incident-config", privateHandler)
	// per-project time_budget detector threshold (ms).
	mux.Handle("GET /me/time-budget-config", privateHandler)
	mux.Handle("PUT /me/time-budget-config", privateHandler)
	// Per-project cost_velocity detector absolute threshold (USD).
	mux.Handle("GET /me/cost-velocity-config", privateHandler)
	mux.Handle("PUT /me/cost-velocity-config", privateHandler)
	// Per-project cost_velocity rate detector configuration
	// (threshold $/min + rolling window in minutes).
	mux.Handle("GET /me/cost-velocity-rate-config", privateHandler)
	mux.Handle("PUT /me/cost-velocity-rate-config", privateHandler)
	// Backend pricing-table metadata. Returns the pricing table
	// version + supported model list so customers can verify which
	// models Mesedi prices server-side.
	mux.Handle("GET /me/pricing-info", privateHandler)
	// Playbook signatures. Returns the SHA-256 of each in-binary
	// playbook so the dashboard can detect when a cached AI
	// analysis was generated against a now-stale playbook (Wave
	// ai-analysis-staleness-tracking).
	mux.Handle("GET /me/playbook-signatures", privateHandler)
	// per-project tool_schema_drift return_value byte cap.
	mux.Handle("GET /me/tool-return-value-config", privateHandler)
	mux.Handle("PUT /me/tool-return-value-config", privateHandler)
	// per-project truncation-rate telemetry.
	mux.Handle("GET /me/tool-return-value-stats", privateHandler)
	// per-project config-fallback telemetry.
	mux.Handle("GET /me/config-fallback-stats", privateHandler)
	// Wave + .g, org-level cascading defaults + rollup.
	mux.Handle("GET /me/organization/defaults", privateHandler)
	mux.Handle("PUT /me/organization/defaults", privateHandler)
	mux.Handle("GET /me/organization/config-fallback-rollup", privateHandler)
	// per-project custom security patterns for the 3
	// security detectors (prompt_injection, data_leakage,
	// sandbox_escape). Without these explicit forwards the routes
	// registered on privateMux inside RegisterRoutes would 404 at
	// the outer mux before ever reaching the auth chain (same
	// pattern as the billing-payment-method bug noted at ).
	// Added retroactively in Allowlist.d after the foundation-audit
	// B22 check fired on the missing primitive-family forwards.
	mux.Handle("GET /me/pattern-config/{detector}", privateHandler)
	mux.Handle("POST /me/pattern-config/{detector}", privateHandler)
	mux.Handle("PATCH /me/pattern-config/{detector}/{pattern_id}", privateHandler)
	mux.Handle("DELETE /me/pattern-config/{detector}/{pattern_id}", privateHandler)
	// per-project detector thresholds for the 6
	// audit-called-out detectors (semantic_loop, token_waste,
	// tool_schema_drift, grounding_failure, drift, context_overflow).
	mux.Handle("GET /me/detector-thresholds/{detector}", privateHandler)
	mux.Handle("GET /me/detector-thresholds/{detector}/{threshold_key}", privateHandler)
	mux.Handle("PUT /me/detector-thresholds/{detector}/{threshold_key}", privateHandler)
	mux.Handle("DELETE /me/detector-thresholds/{detector}/{threshold_key}", privateHandler)
	// Allowlist.a + .d, per-project failure-suppression entries for
	// the 3 detectors that share the Allowlist primitive (crashes,
	// tool_failures, validator_failures) plus the lifetime
	// per-detector suppression-count telemetry tile.
	mux.Handle("GET /me/allowlist/{detector}", privateHandler)
	mux.Handle("POST /me/allowlist/{detector}", privateHandler)
	mux.Handle("PATCH /me/allowlist/{detector}/{allowlist_id}", privateHandler)
	mux.Handle("DELETE /me/allowlist/{detector}/{allowlist_id}", privateHandler)
	mux.Handle("GET /me/allowlist-stats", privateHandler)
	// Empty-states wave A — detector-status observability surface.
	// Generic per-detector empty-state + priming metadata the
	// dashboard reads on overview-page load. Closes the backend half
	// of semantic_loop.G2 + tool_schema_drift.G2.
	mux.Handle("GET /v1/detector-status", privateHandler)
	// — customer-facing 2FA / TOTP. All five live on
	// privateHandler because they manage the calling customer's own
	// authenticator-app enrollment and need the session cookie.
	// Without these explicit forwards the routes registered on the
	// privateMux inside RegisterRoutes would 404 at the outer mux
	// before ever reaching the auth chain (same pattern as the
	// billing-payment-method bug noted at ).
	mux.Handle("GET /me/2fa/status", privateHandler)
	mux.Handle("POST /me/2fa/setup-init", privateHandler)
	mux.Handle("POST /me/2fa/setup-verify", privateHandler)
	mux.Handle("POST /me/2fa/disable", privateHandler)
	mux.Handle("POST /me/2fa/regenerate-codes", privateHandler)
	// Team / multi-seat. Admin endpoints under
	// /me/organization/* go through privateHandler (project API key
	// auth + admin-role guard inside the handler). Public accept
	// endpoints go through signupHandler since they're token-auth
	// only and need CORS for the dashboard's /invites/{token} page.
	mux.Handle("GET /me/organization", privateHandler)
	mux.Handle("GET /me/organization/members", privateHandler)
	mux.Handle("PATCH /me/organization/members/{user}", privateHandler)
	mux.Handle("DELETE /me/organization/members/{user}", privateHandler)
	mux.Handle("GET /me/organization/invites", privateHandler)
	mux.Handle("POST /me/organization/invites", privateHandler)
	mux.Handle("DELETE /me/organization/invites/{invite}", privateHandler)
	mux.Handle("GET /invites/{token}", signupHandler)
	mux.Handle("POST /invites/{token}/accept", signupHandler)
	mux.Handle("OPTIONS /invites/{token}", signupHandler)
	mux.Handle("OPTIONS /invites/{token}/accept", signupHandler)
	// Founder-side admin dashboard. Token-gated; refuses every
	// request when MESEDI_ADMIN_TOKEN is empty. CORS preflight OPTIONS
	// is needed because the dashboard at app.mesedi.ai calls
	// cross-origin from a different host than the backend (api.mesedi.ai for the hosted service).
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
	mux.Handle("DELETE /admin/projects/{id}/failure-groups", adminHandler)
	mux.Handle("OPTIONS /admin/projects/{id}/failure-groups", adminHandler)
	mux.Handle("GET /admin/storage", adminHandler)
	mux.Handle("OPTIONS /admin/storage", adminHandler)
	mux.Handle("GET /admin/ai-analyses-by-project", adminHandler)
	mux.Handle("OPTIONS /admin/ai-analyses-by-project", adminHandler)
	// per-project failure-group breakdown for the expanded row.
	mux.Handle("GET /admin/projects/{id}/ai-analyses-detail", adminHandler)
	mux.Handle("OPTIONS /admin/projects/{id}/ai-analyses-detail", adminHandler)
	// cross-tenant flat list + lifetime totals.
	mux.Handle("GET /admin/ai-analyses", adminHandler)
	mux.Handle("OPTIONS /admin/ai-analyses", adminHandler)
	mux.Handle("GET /admin/ai-analyses-totals", adminHandler)
	mux.Handle("OPTIONS /admin/ai-analyses-totals", adminHandler)
	// founder analytics + accounting (Stripe-derived).
	mux.Handle("GET /admin/analytics-summary", adminHandler)
	mux.Handle("OPTIONS /admin/analytics-summary", adminHandler)
	mux.Handle("GET /admin/charges", adminHandler)
	mux.Handle("OPTIONS /admin/charges", adminHandler)
	mux.Handle("GET /admin/refunds", adminHandler)
	mux.Handle("OPTIONS /admin/refunds", adminHandler)
	mux.Handle("GET /admin/subscriptions-canceled", adminHandler)
	mux.Handle("OPTIONS /admin/subscriptions-canceled", adminHandler)
	// anonymized failure_class aggregates (LinkedIn trends).
	mux.Handle("GET /admin/failure-class-aggregates", adminHandler)
	mux.Handle("OPTIONS /admin/failure-class-aggregates", adminHandler)
	mux.Handle("POST /admin/failure-class-aggregates/run", adminHandler)
	mux.Handle("OPTIONS /admin/failure-class-aggregates/run", adminHandler)
	// Anthropic credit + 7-day burn rate widget on /admin.
	mux.Handle("GET /admin/anthropic-credit", adminHandler)
	mux.Handle("POST /admin/anthropic-credit", adminHandler)
	mux.Handle("OPTIONS /admin/anthropic-credit", adminHandler)
	mux.Handle("GET /admin/abuse", adminHandler)
	mux.Handle("OPTIONS /admin/abuse", adminHandler)
	mux.Handle("POST /admin/abuse/{id}/resolve", adminHandler)
	mux.Handle("OPTIONS /admin/abuse/{id}/resolve", adminHandler)
	// Stripe webhook billing-event signals: chargebacks + dunning.
	mux.Handle("GET /admin/billing-events", adminHandler)
	mux.Handle("OPTIONS /admin/billing-events", adminHandler)
	mux.Handle("POST /admin/billing-events/{id}/resolve", adminHandler)
	mux.Handle("OPTIONS /admin/billing-events/{id}/resolve", adminHandler)
	// API key management (migration 015).
	mux.Handle("GET /admin/api-keys", adminHandler)
	mux.Handle("POST /admin/api-keys", adminHandler)
	mux.Handle("OPTIONS /admin/api-keys", adminHandler)
	mux.Handle("DELETE /admin/api-keys/{id}", adminHandler)
	mux.Handle("OPTIONS /admin/api-keys/{id}", adminHandler)
	// "Mark key compromised" admin action.
	mux.Handle("POST /admin/api-keys/{id}/mark-compromised", adminHandler)
	mux.Handle("OPTIONS /admin/api-keys/{id}/mark-compromised", adminHandler)
	mux.Handle("GET /admin/whoami", adminHandler)
	mux.Handle("OPTIONS /admin/whoami", adminHandler)
	// Closed-project audit search (, migration 031). R1 + R2:
	// staff-only forensics for account-takeover + customer-support
	// response to a "I did not press Close" claim. Dashboard UI
	// ships in ; staff curl this directly today.
	mux.Handle("GET /admin/audit-events", adminHandler)
	mux.Handle("OPTIONS /admin/audit-events", adminHandler)
	// GDPR Article 17 purge endpoint. Hard-delete every audit
	// row owned by the supplied closed project. Refuses live projects.
	mux.Handle("POST /admin/projects/{id}/audit-events/purge", adminHandler)
	mux.Handle("OPTIONS /admin/projects/{id}/audit-events/purge", adminHandler)

	// Top-level middleware chain. SecurityHeaders is outermost so its
	// four hardening headers (HSTS, X-Content-Type-Options,
	// X-Frame-Options, Referrer-Policy) stamp every response —
	// including unauthenticated 401s and 404s that pre-date the auth
	// chain. NewTopChain handles panic recovery and request logging.
	root := api.SecurityHeaders(api.NewTopChain(logger)(mux))

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
		DBURLPostgres:       envString("MESEDI_DB_URL_POSTGRES", ""),
		DashboardURL:        envString("MESEDI_DASHBOARD_URL", ""),
		StripeSecretKey:         envString("MESEDI_STRIPE_SECRET_KEY", ""),
		StripeSecretKeyTest:     envString("MESEDI_STRIPE_SECRET_KEY_TEST", ""),
		StripeWebhookSecret:     envString("MESEDI_STRIPE_WEBHOOK_SECRET", ""),
		StripeWebhookSecretTest: envString("MESEDI_STRIPE_WEBHOOK_SECRET_TEST", ""),
		TOTPEncryptionKey:       envString("MESEDI_TOTP_ENCRYPTION_KEY", ""),
		// Prefer the new TEAM env var; fall back to the legacy PRO one
		// so an in-flight deploy with the old secret still works. Once
		// every deployment has migrated to MESEDI_STRIPE_TEAM_PRICE_ID
		// the legacy fallback can be removed.
		StripeTeamPriceID: envString("MESEDI_STRIPE_TEAM_PRICE_ID",
			envString("MESEDI_STRIPE_PRO_PRICE_ID", "")),
		AdminToken:      envString("MESEDI_ADMIN_TOKEN", ""),
		ResendAPIKey:    envString("RESEND_API_KEY", ""),
		ResendFrom:      envString("MESEDI_MAIL_FROM", "Mesedi <onboarding@resend.dev>"),
		AlertWebhookURL: envString("MESEDI_ALERT_WEBHOOK_URL", ""),
		SigninSecret:    envString("MESEDI_SIGNIN_SECRET", ""),
	}
	flag.IntVar(&cfg.Port, "port", cfg.Port, "TCP port for the HTTP API")
	flag.StringVar(&cfg.LogLevel, "log-level", cfg.LogLevel, "log verbosity: debug | info | warn | error")
	flag.StringVar(&cfg.DBURL, "db-url", cfg.DBURL, "SQLite DSN (used when --db-url-postgres is empty)")
	flag.StringVar(&cfg.DBURLPostgres, "db-url-postgres", cfg.DBURLPostgres, "Postgres DSN (postgres:// or postgresql://); when set, used in preference to --db-url")
	flag.StringVar(&cfg.DashboardURL, "dashboard-url", cfg.DashboardURL, "public origin of the React dashboard (e.g. https://app.mesedi.ai)")
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
