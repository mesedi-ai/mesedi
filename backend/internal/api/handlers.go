// Package api wires HTTP handlers for the Mesedi backend service.
//
// Phase 1 scope: ingest endpoints accept JSON, validate shape, log to
// stdout. Storage (Postgres) and detection (loops, drift, etc.) come
// online in Phase 1.5 and Phases 3+.
//
// Handlers do not authenticate yet, Phase 1.5 adds bearer-token auth
// via middleware. For local dev today, any caller can post to /events
// and /executions; that is intentional and matches the "ship phase
// acceptance, iterate after" principle of the development checklist.
//
// The handler bodies live in topical sidecars since the 2026-09-12
// split: handlers_ingest, handlers_execution_reads,
// handlers_failure_groups, handlers_analysis, handlers_reports,
// handlers_retention_budget_config, handlers_detector_config,
// handlers_class_severities, handlers_api_keys, handlers_webhooks
// and handlers_project. This file keeps the Handlers struct, the
// route tables, and the helpers shared across them.
package api

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/anthropic"
	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/dlp"
	"mesedi/backend/internal/mail"
	meseditel "mesedi/backend/internal/otel"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/store"
	"mesedi/backend/internal/webhooks"
)

// Handlers carries dependencies needed by HTTP handlers. As more
// subsystems come online (storage, detectors, etc.) they get attached
// here rather than passed through each handler signature.
type Handlers struct {
	Logger        *slog.Logger
	Store         store.Store
	HaltSubs      *HaltSubscribers // sub-slice 21b, SSE halt-channel registry
	WebhookClient *http.Client     // outbound dispatcher HTTP client
	// dispatchWG tracks spawn-and-forget webhook dispatch goroutines
	// so shutdown and tests can drain them; see DrainDispatches.
	dispatchWG sync.WaitGroup
	// DashboardURL is the public origin of the React dashboard
	// (e.g. https://app.mesedi.ai). Used to build deep-links in
	// webhook payloads and Discord embeds. When empty, the dispatcher
	// falls back to the inbound request's scheme + host, correct for
	// local dev where the dashboard is same-origin with the API, wrong
	// in prod where the dashboard lives on a different host.
	DashboardURL string
	// Stripe carries the secret key, webhook signing secret, and Pro
	// price id used by the billing endpoints. All three may
	// be empty in local dev; the billing endpoints respond 503 when
	// any of them is missing.
	Stripe StripeConfig
	// HobbyBillingScheduler is the same instance started at process
	// boot; a pointer is stashed on Handlers so the smoke-harness admin
	// trigger endpoint (POST /admin/projects/{id}/trigger-hobby-
	// billing-run) can invoke the scheduler's processProject step
	// on demand. Nil in unit tests + local dev that don't spin up
	// the scheduler; the admin handler 503s on nil.
	HobbyBillingScheduler *HobbyBillingScheduler
	// Mailer ships transactional email (welcome on signup today;
	// day-1 / day-3 nudges later). Local-dev runs without
	// RESEND_API_KEY use a NoopMailer that silently swallows sends.
	Mailer mail.Mailer
	// DocsURL is the public origin of the docs site, used inside
	// transactional email templates. Falls back to DashboardURL +
	// "/docs" if empty.
	DocsURL string
	// Abuse is the process-wide detector. Carried on Handlers so the
	// rate-limit middleware, auth middleware (key-leak detector),
	// ingest middleware (oversized payload), and signup handler all
	// share the same in-memory rolling counters.
	Abuse *AbuseDetector
	// DLPScanner is the compiled regex set used by HandleIngestEvents
	// to scan outbound llm_call / tool_call payloads for credentials,
	// signed tokens, and PII. Constructed once at
	// startup with the built-in rule baseline; nil disables the scan
	// entirely (useful for local dev where you do NOT want your
	// real API keys in test payloads to flag).
	DLPScanner *dlp.Scanner
	// OTel is the OpenTelemetry parallel emitter. Set
	// at startup from main.go when OTEL_EXPORTER_OTLP_ENDPOINT is
	// configured; nil (or .Enabled()==false) disables emission so
	// the entire feature can ship behind an env var. All Emit
	// calls are nil-safe at the method receiver.
	OTel *meseditel.Emitter //nolint:revive // local alias to avoid name collision with OTel SDK
	// Anthropic is the minimal Messages API client used by the
	// AI root-cause analysis endpoint. nil or a
	// client with no API key disables analysis; the handler
	// then responds with a "not configured" message instead
	// of crashing.
	Anthropic *anthropic.Client
	// AnthropicAdmin wraps the Anthropic Admin API Cost Report
	// endpoint. nil or a client without a key
	// disables the burn-rate display on the founder admin
	// dashboard; the handler then responds with a "not
	// configured" payload rather than failing the request.
	AnthropicAdmin *anthropic.AdminClient
	// SigninSecret is the server-to-server shared secret that
	// guards POST /signin. The dashboard server (Cloudflare
	// Workers) calls /signin from its OAuth callback and magic-link
	// verify routes after it has already proved email ownership; the
	// secret keeps the browser from calling /signin directly with an
	// arbitrary email. Loaded from MESEDI_SIGNIN_SECRET. Empty
	// disables the endpoint entirely (returns 503), which is the
	// safe default in local dev where SSO is not configured.
	SigninSecret string
	// TOTPEncryptionKey is the 32-byte AES-256-GCM key used to seal
	// customer TOTP secrets at rest. Loaded from
	// MESEDI_TOTP_ENCRYPTION_KEY (64 hex chars). Nil/empty disables
	// the 2FA endpoints, they return 503, so a local-dev deploy
	// without the key set degrades gracefully rather than crashing.
	// See totp_crypto.go for the encrypt/decrypt helpers.
	TOTPEncryptionKey []byte
}

// New constructs the Handlers value. Done as a constructor (rather than
// a literal) so the dependencies become explicit as the surface grows.
//
// dashboardURL is the public origin of the React dashboard (no trailing
// slash, no path). Pass "" in local dev to derive from the request host.
//
// stripeCfg carries Stripe-specific identifiers; pass a zero-value
// StripeConfig in local dev to leave billing endpoints disabled.
//
// mailer is the transactional email sender. Pass mail.NoopMailer in
// local dev or test runs to silently swallow sends.
func New(logger *slog.Logger, s store.Store, dashboardURL string, stripeCfg StripeConfig, mailer mail.Mailer) *Handlers {
	if mailer == nil {
		mailer = mail.NoopMailer{Logger: logger}
	}
	// Compile the DLP scanner once at startup against the built-in
	// rule baseline. A compilation failure here means a malformed
	// built-in rule in code that should NEVER ship, it's a build
	// bug, not a runtime config issue. Closes data_leakage.G3:
	// previously we logged the error and continued with a nil
	// scanner, leaving the entire data_leakage detector DARK in
	// production with only a buried startup log to signal it.
	// Customers thought they had DLP coverage; in reality their
	// secrets were flowing through unredacted. Same defensive
	// posture as sandbox_escape's mustCompilePatterns: panic at
	// init so ops sees the failure immediately and the bad build
	// can never reach production silently.
	scanner, err := dlp.NewScanner(nil)
	if err != nil {
		panic("dlp scanner construction failed at startup; built-in rule is malformed and must be fixed before deploy: " + err.Error())
	}
	return &Handlers{
		Logger:        logger,
		Store:         s,
		HaltSubs:      NewHaltSubscribers(),
		WebhookClient: webhooks.DefaultHTTPClient(),
		DashboardURL:  strings.TrimRight(dashboardURL, "/"),
		Stripe:        stripeCfg,
		Mailer:        mailer,
		Abuse:         NewAbuseDetector(logger, s),
		DLPScanner:    scanner,
	}
}

// RegisterRoutes attaches every protected route to the provided ServeMux.
// Keep this list short and explicit, it doubles as the API surface
// inventory for documentation.
// RegisterPublicRoutes attaches handlers that intentionally bypass
// bearer-token auth. /signup is public because a browser visiting it
// has no API key yet; abuse is bounded by signup.go's in-process IP
// rate limiter.
func (h *Handlers) RegisterPublicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /signup", h.HandleSignup)
	// Server-to-server signin endpoint. Public from the HTTP
	// layer's perspective so the dashboard server (Cloudflare Workers)
	// can call it cross-origin during the SSO callback / magic-link
	// verify flows; authenticity is verified inside the handler via
	// the X-Mesedi-Signin-Secret header against the configured shared
	// secret. See signin.go's file-level doc comment for the full
	// trust model.
	mux.HandleFunc("POST /signin", h.HandleSignin)
	// two-factor verify completes a paused signin after the
	// customer enters their 6-digit code on the prompt page. Same
	// shared-secret model as /signin (the dashboard Worker is the
	// only legitimate caller).
	mux.HandleFunc("POST /auth/2fa-verify", h.HandleTwoFactorVerify)
	// Magic-link sign-in (commit 2). /start mints a token + emails;
	// /verify is server-to-server (dashboard calls it from its handoff
	// route after the customer clicks the email link).
	mux.HandleFunc("POST /magic-link/start", h.HandleMagicLinkStart)
	mux.HandleFunc("GET /magic-link/verify", h.HandleMagicLinkVerify)

	// Email verification. The confirm + resend endpoints are
	// public, the recipient may not be signed in when they click
	// the verification link, and resend is keyed on the email itself.
	// The status endpoint is bearer-gated because the dashboard reads
	// it under the customer's existing auth.
	mux.HandleFunc("POST /api/email-verify/confirm", h.HandleEmailVerifyConfirm)
	mux.HandleFunc("POST /api/email-verify/resend", h.HandleEmailVerifyResend)
	// Batch 2, POST /auth/logout destroys the caller's session.
	// Public so a customer who has already lost their session row
	// (expired, kicked, key revoked) can still click Sign Out
	// without a 401. See auth_logout.go.
	mux.HandleFunc("POST /auth/logout", h.HandleAuthLogout)
	// Stripe webhook receiver. Public because Stripe POSTs server-
	// to-server with no bearer; authenticity is verified inside the
	// handler via the Stripe-Signature header against the configured
	// webhook secret.
	mux.HandleFunc("POST /billing/webhook", h.HandleStripeWebhook)
	// public invite-accept endpoints. Auth IS the token:
	// the random hex string in the URL path is the authentication.
	// GET surfaces the invite info so the accept page can render
	// "you've been invited to X as Y" before the user redeems.
	mux.HandleFunc("GET /invites/{token}", h.HandleGetInviteByToken)
	mux.HandleFunc("POST /invites/{token}/accept", h.HandleAcceptInvite)
}

func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	// Phase 1 ingest surface.
	mux.HandleFunc("POST /executions", h.HandleCreateExecution)
	// Slice 1, read-side project surface for the dashboard.
	mux.HandleFunc("GET /project", h.HandleGetProject)
	mux.HandleFunc("PATCH /project/name", h.HandleSetProjectName)
	mux.HandleFunc("GET /me", h.HandleGetMe)
	//, dashboard polls this once per layout mount to decide
	// whether to render the email-verification interstitial.
	mux.HandleFunc("GET /me/email-verification-status", h.HandleEmailVerificationStatus)

	// customer-facing TOTP / two-factor authentication.
	mux.HandleFunc("GET /me/2fa/status", h.HandleTOTPStatus)
	mux.HandleFunc("POST /me/2fa/setup-init", h.HandleTOTPSetupInit)
	mux.HandleFunc("POST /me/2fa/setup-verify", h.HandleTOTPSetupVerify)
	mux.HandleFunc("POST /me/2fa/disable", h.HandleTOTPDisable)
	mux.HandleFunc("POST /me/2fa/regenerate-codes", h.HandleTOTPRegenerateBackupCodes)
	mux.HandleFunc("PATCH /executions/{id}", h.HandleUpdateExecution)
	mux.HandleFunc("POST /events", h.HandleIngestEvents)
	// Phase 3b, read-side execution surface for the dashboard.
	mux.HandleFunc("GET /executions", h.HandleListExecutions)
	mux.HandleFunc("GET /executions/{id}", h.HandleGetExecution)
	mux.HandleFunc("GET /executions/{id}/digest", h.HandleGetExecutionDigest)
	// multi-agent topology graph.
	mux.HandleFunc("GET /executions/{id}/topology", h.HandleGetExecutionTopology)
	mux.HandleFunc("GET /stats", h.HandleStats)
	// org-level rollup across all projects owned by the
	// same user (v0.1 tenant model = owner_user_id). Resolves the
	// authenticated project's owner, sums burn across sibling projects.
	mux.HandleFunc("GET /me/rollup", h.HandleOrgRollup)
	mux.HandleFunc("GET /me/savings", h.HandleSavings)
	// tenant monthly budget ceiling. GET fetches the
	// configured ceiling (ErrNotFound -> 404 so the UI can render
	// the "set up a ceiling" empty state). PUT upserts.
	mux.HandleFunc("GET /me/budget-ceiling", h.HandleGetBudgetCeiling)
	mux.HandleFunc("PUT /me/budget-ceiling", h.HandleUpsertBudgetCeiling)
	// per-project failure-class severity overrides.
	// GET returns the full map (defaults + any overrides), PUT
	// upserts an override for one class, DELETE reverts to default.
	mux.HandleFunc("GET /me/class-severities", h.HandleListClassSeverities)
	mux.HandleFunc("PUT /me/class-severities/{class}", h.HandleUpsertClassSeverity)
	mux.HandleFunc("DELETE /me/class-severities/{class}", h.HandleDeleteClassSeverity)
	// per-project data retention.
	mux.HandleFunc("GET /me/retention", h.HandleGetRetention)
	mux.HandleFunc("PUT /me/retention", h.HandleSetRetention)
	// migration 040, per-project provider_incident
	// detector threshold (default 2, single-tenant customers
	// typically set to 1).
	mux.HandleFunc("GET /me/provider-incident-config", h.HandleGetProviderIncidentConfig)
	mux.HandleFunc("PUT /me/provider-incident-config", h.HandleSetProviderIncidentConfig)
	// per-project time_budget detector threshold (ms).
	mux.HandleFunc("GET /me/time-budget-config", h.HandleGetTimeBudgetConfig)
	mux.HandleFunc("PUT /me/time-budget-config", h.HandleSetTimeBudgetConfig)
	mux.HandleFunc("GET /me/cost-velocity-config", h.HandleGetCostVelocityConfig)
	mux.HandleFunc("PUT /me/cost-velocity-config", h.HandleSetCostVelocityConfig)
	mux.HandleFunc("GET /me/cost-velocity-rate-config", h.HandleGetCostVelocityRateConfig)
	mux.HandleFunc("PUT /me/cost-velocity-rate-config", h.HandleSetCostVelocityRateConfig)
	mux.HandleFunc("GET /me/pricing-info", h.HandleGetPricingInfo)
	// Wave ai-analysis-staleness-tracking: dashboard fetches the
	// current playbook content signatures so it can compare against
	// the stored signature on each cached AI analysis and surface a
	// "re-analyze to refresh" badge when they differ.
	mux.HandleFunc("GET /me/playbook-signatures", h.HandleGetPlaybookSignatures)
	// per-project tool_schema_drift return_value byte cap.
	mux.HandleFunc("GET /me/tool-return-value-config", h.HandleGetToolReturnValueConfig)
	mux.HandleFunc("PUT /me/tool-return-value-config", h.HandleSetToolReturnValueConfig)
	// per-project custom-pattern storage for the three
	// security detectors (prompt_injection / data_leakage /
	// sandbox_escape). Detectors read these in ; dashboard
	// editor lands in .
	mux.HandleFunc("GET /me/pattern-config/{detector}", h.HandleListPatternConfig)
	mux.HandleFunc("POST /me/pattern-config/{detector}", h.HandleCreatePatternConfig)
	mux.HandleFunc("PATCH /me/pattern-config/{detector}/{pattern_id}", h.HandleUpdatePatternConfig)
	mux.HandleFunc("DELETE /me/pattern-config/{detector}/{pattern_id}", h.HandleDeletePatternConfig)
	// per-project tunable thresholds for the six detectors
	// the audit called out as having hardcoded thresholds
	// (semantic_loop / token_waste / tool_schema_drift /
	// grounding_failure / drift / context_overflow). Detectors read
	// these in B.b; dashboard editor lands in B.c.
	mux.HandleFunc("GET /me/detector-thresholds/{detector}", h.HandleListDetectorThresholds)
	mux.HandleFunc("GET /me/detector-thresholds/{detector}/{threshold_key}", h.HandleGetDetectorThreshold)
	mux.HandleFunc("PUT /me/detector-thresholds/{detector}/{threshold_key}", h.HandleSetDetectorThreshold)
	mux.HandleFunc("DELETE /me/detector-thresholds/{detector}/{threshold_key}", h.HandleDeleteDetectorThreshold)
	// Allowlist.a, per-project allowlist entries for the three
	// detectors that share the Allowlist primitive (crashes,
	// tool_failures, validator_failures). Detector hot-path check
	// wired in Allowlist.b; dashboard editor lands in Allowlist.c.
	mux.HandleFunc("GET /me/allowlist/{detector}", h.HandleListAllowlist)

	// The auditor's export. Under /me/ because the project comes from the
	// API key and there is deliberately nowhere to name a different one.
	//
	// RateLimit posture: the shared per-project limiter on this private
	// chain, plus two hard caps in the handler, MaxCheckpointRange and
	// MaxExportExecutions, both of which refuse rather than truncate.
	// This is the most expensive read the API serves, so it is bounded by
	// cost rather than by call count.
	//
	// Available to every tier, deliberately. An agency's ability to verify
	// records it is legally required to retain must not depend on what it
	// pays us. Tighten the caps if the work becomes a problem; do not gate
	// who is allowed to check their own evidence. Full reasoning in
	// chain_export.go.
	mux.HandleFunc("GET /me/chain/export", h.HandleChainExport)
	mux.HandleFunc("POST /me/allowlist/{detector}", h.HandleCreateAllowlist)
	mux.HandleFunc("PATCH /me/allowlist/{detector}/{allowlist_id}", h.HandleUpdateAllowlist)
	mux.HandleFunc("DELETE /me/allowlist/{detector}/{allowlist_id}", h.HandleDeleteAllowlist)
	// Allowlist.d, lifetime per-detector suppression telemetry for
	// the main-dashboard suppressions tile. One indexed scan,
	// GROUP BY detector.
	mux.HandleFunc("GET /me/allowlist-stats", h.HandleGetAllowlistStats)
	// per-project truncation-rate telemetry.
	mux.HandleFunc("GET /me/tool-return-value-stats", h.HandleGetToolReturnValueStats)
	// Empty-states wave A, detector-status surface. Generic per-
	// detector observability metadata the dashboard uses to render
	// "you haven't instrumented X yet" / "drift detection priming"
	// empty states. Closes the backend half of semantic_loop.G2 +
	// tool_schema_drift.G2. Extensible to future detectors via new
	// response fields, not new endpoints.
	mux.HandleFunc("GET /v1/detector-status", h.HandleGetDetectorStatus)
	// per-project config-fallback telemetry.
	mux.HandleFunc("GET /me/config-fallback-stats", h.HandleGetConfigFallbackStats)
	// Wave + .g, org-level cascading defaults + rollup.
	mux.HandleFunc("GET /me/organization/defaults", h.HandleGetOrgDefaults)
	mux.HandleFunc("PUT /me/organization/defaults", h.HandlePutOrgDefault)
	mux.HandleFunc("GET /me/organization/config-fallback-rollup", h.HandleGetOrgConfigFallbackRollup)
	// Team / multi-seat. Admin-gated endpoints for
	// managing the org + members + invites under the auth project's
	// tenant_id. resolveAdminContext() guards each handler.
	mux.HandleFunc("GET /me/organization", h.HandleGetOrganization)
	mux.HandleFunc("GET /me/organization/members", h.HandleListMembers)
	mux.HandleFunc("PATCH /me/organization/members/{user}", h.HandleUpdateMemberRole)
	mux.HandleFunc("DELETE /me/organization/members/{user}", h.HandleRemoveMember)
	mux.HandleFunc("GET /me/organization/invites", h.HandleListInvites)
	mux.HandleFunc("POST /me/organization/invites", h.HandleCreateInvite)
	mux.HandleFunc("DELETE /me/organization/invites/{invite}", h.HandleRevokeInvite)
	// Phase 3a, read-side failure_group surface for the dashboard.
	mux.HandleFunc("GET /failure-groups", h.HandleListFailureGroups)
	mux.HandleFunc("GET /failure-groups/{id}", h.HandleGetFailureGroup)
	// cost broken down by tenant_id over a time window.
	mux.HandleFunc("GET /reports/cost-by-tenant", h.HandleReportCostByTenant)
	// Phase 3b sub-slice 9, executions inside a failure_group.
	mux.HandleFunc("GET /failure-groups/{id}/executions", h.HandleListExecutionsInFailureGroup)
	// LLM-assisted root-cause analysis. Cached on the
	// failure_group row for 24h or until last_seen advances; force
	// regenerate with ?regenerate=1.
	mux.HandleFunc("POST /failure-groups/{id}/analyze", h.HandleAnalyzeFailureGroup)
	// failure-group-resolve wave: customer-facing resolve / unresolve
	// actions. Both Bearer-gated, both emit audit_events. POST chosen
	// over PATCH for browser-form compatibility, Next.js fetch from
	// the dashboard can POST without preflight CORS overhead.
	mux.HandleFunc("POST /failure-groups/{id}/resolve", h.HandleResolveFailureGroup)
	mux.HandleFunc("POST /failure-groups/{id}/unresolve", h.HandleUnresolveFailureGroup)
	// Phase 3b sub-slice 18, API key management surface.
	mux.HandleFunc("GET /api-keys", h.HandleListAPIKeys)
	mux.HandleFunc("POST /api-keys", h.HandleCreateAPIKey)
	mux.HandleFunc("DELETE /api-keys/{id}", h.HandleRevokeAPIKey)
	// Audit logs v1 (admin role required; enforced inside handler).
	// Backs the Team-tier "Audit logs (who did what, when)" promise.
	mux.HandleFunc("GET /audit-log", h.HandleListAuditEvents)
	// Sub-slice 21b, SSE remote-halt channel.
	mux.HandleFunc("GET /executions/{id}/halt-stream", h.HandleHaltStream)
	mux.HandleFunc("POST /executions/{id}/halt", h.HandleTriggerHalt)
	// Tier 1 Playbooks, canonical fix descriptions per failure-class
	// signature. Plain GET with query params; content is the embedded
	// markdown shipped in internal/playbooks/content/.
	mux.HandleFunc("GET /playbooks", h.HandleGetPlaybook)
	// webhook escalation config + dispatcher.
	mux.HandleFunc("GET /webhooks", h.HandleListWebhooks)
	mux.HandleFunc("POST /webhooks", h.HandleCreateWebhook)
	mux.HandleFunc("DELETE /webhooks/{id}", h.HandleDeleteWebhook)
	// Slice 2: manual test-delivery trigger + deliveries log read.
	mux.HandleFunc("POST /webhooks/{id}/test", h.HandleTestWebhook)
	mux.HandleFunc("GET /webhooks/{id}/deliveries", h.HandleListWebhookDeliveries)
	// Stripe billing endpoints (auth-required).
	mux.HandleFunc("GET /billing", h.HandleGetBilling)
	mux.HandleFunc("GET /billing/usage", h.HandleGetBillingUsage)
	// : AI root-cause analyses usage counter. Surfaced on
	// /app/billing + as a chip on the main user dashboard so Team
	// customers can see headroom before they hit the 200/period cap.
	mux.HandleFunc("GET /billing/ai-analyses-usage", h.HandleGetAIAnalysesUsage)
	mux.HandleFunc("POST /billing/checkout", h.HandleCreateCheckout)
	mux.HandleFunc("POST /billing/portal", h.HandleCreatePortal)
	mux.HandleFunc("POST /billing/payment-method/setup", h.HandleCreateSetupCheckout)
	mux.HandleFunc("POST /billing/payment-method/remove", h.HandleRemovePaymentMethod)
	// : customer-configurable monthly overage cap. PUT body is
	// {"cap_usd": <float>}; the value is persisted to
	// projects.billing_cap_usd and consulted by the hobby billing
	// scheduler at period close + ingest-time gate.
	mux.HandleFunc("PUT /billing/cap", h.HandleUpdateBillingCap)
	// danger-zone flow on /app/settings. POST /billing/downgrade
	// cancels the Cloud Team subscription at period end (keep data).
	// POST /billing/close-account cancels immediately + cascade-deletes
	// the project (lose data, force logout on next 401).
	mux.HandleFunc("POST /billing/downgrade", h.HandleDowngradeToHobby)
	mux.HandleFunc("POST /billing/close-account", h.HandleCloseAccount)
}

// normalizeListQuery moved to list_query.go during the split.

// HandleAnalyzeFailureGroup runs the LLM-assisted root-cause
// analyzer over a failure_group and persists the resulting Markdown
// on the row. Subsequent reads of the group surface
// the analysis on the same response shape, so the dashboard does
// not need a separate fetch.
//
// Rate posture for v1: no per-project rate limit beyond the cache.
// Repeated calls within 24 hours short-circuit by returning the
// cached analysis without re-calling the LLM, so the cost ceiling
// is effectively one LLM call per failure_group per day no matter
// how many times the dashboard re-renders the card.
//
// When ANTHROPIC_API_KEY is unset the handler returns 503 with a
// "not configured" message rather than a 500, so the dashboard can
// surface a friendly "AI analysis is not enabled on this
// deployment" state.

// ─────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────

// decodeJSON enforces strict decoding, unknown JSON fields cause a 400.
// Strict decoding catches schema drift early during SDK development;
// once the schema is stable post-Phase 4 we may relax to forward-compat.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// writeJSON writes a JSON response with the given status code. Errors
// during write are logged-and-ignored: there's nothing useful to do at
// that point and the client has already received the status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Cannot send another response now (header is committed); just log.
		fmt.Fprintf(w, `{"ok":false,"error":"response encode failed: %s"}`, err.Error())
	}
}

// writeError is a convenience wrapper for the standard error response shape.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": message,
	})
}

// parseRFC3339Query parses an optional RFC3339 timestamp from the
// named query parameter. Returns the zero time.Time when the param
// is absent or empty (which the store layer treats as "no bound").
// Invalid timestamps return an error so callers can 400 cleanly.
func parseRFC3339Query(r *http.Request, name string) (time.Time, error) {
	v := r.URL.Query().Get(name)
	if v == "" {
		return time.Time{}, nil
	}
	return time.Parse(time.RFC3339, v)
}

// tierRetentionCap returns the maximum retention_days a project of
// the given tier may configure, and whether the tier may set
// indefinite retention. Tier strings are normalized lowercase; an
// unknown tier falls through to Hobby semantics (conservative).
//
// Caps match the /pricing card promises (, updated ):
//
//	Hobby:      up to 15 days,   no indefinite
//	Team:       up to 90 days,   no indefinite
//	Enterprise: up to 3650 days, indefinite allowed
//
// Hobby was bumped down from 30 to 15 to create a real
// retention spread vs Team (15 vs 90 days). The new 15-day value
// matches Arize AX Free and is one day above LangSmith / Braintrust
// free tiers; well within industry norms for free observability
// tiers.
//
// The 3650-day max on Enterprise is a sanity ceiling matching the
// global validation in HandleSetRetention; anyone wanting longer is
// expected to flip to indefinite, which is what "audit history"
// customers really want anyway.
func tierRetentionCap(tier string) (maxDays int, allowIndefinite bool) {
	// normalizeTier maps legacy "pro" -> "team" so a stale row
	// returning the old label still gets the same retention cap as
	// the renamed tier.
	switch normalizeTier(strings.ToLower(tier)) {
	case TierProduction, TierEnterprise:
		// Retention is contract-negotiated on both hand-sold tiers.
		return 3650, true
	case TierTeam:
		return TeamDefaultRetentionDays, false
	default:
		// Hobby, empty, or unknown tier.
		return HobbyDefaultRetentionDays, false
	}
}

// computeExecutionCost sums the estimated USD cost across every
// llm_call event in the slice. Each event's payload is unmarshaled into
// a small struct extracting only the four fields cost-computation needs
// (model, input_tokens, output_tokens, estimated_cost_usd); everything
// else is ignored, so changes to the payload schema (adding fields)
// don't affect this code.
//
// semantics, backend is the source of truth for known models;
// SDK-shipped per-event cost is the fallback for unknown models:
//
//   - Known model (pricing.IsKnownModel == true): use the backend
//     pricing table. SDK-shipped cost on the event is ignored. This
//     means new model pricing or pricing changes ship with a backend
//     deploy without waiting for an SDK release.
//
//   - Unknown model: use the per-event payload.estimated_cost_usd as
//     the fallback. Customer using a brand-new model the day it ships
//     doesn't see $0 costs, they see the SDK's best-effort number
//     until the next backend deploy adds the row.
//
// Returns (totalUSD, unknownModelIDs). Caller uses unknownModelIDs to
// emit a single audit_event per execution (not per event) for the
// dashboard's unknown-model surface.
//
// Events whose payload fails to unmarshal are skipped silently, a
// single malformed event shouldn't break cost computation for the
// whole execution.
func computeExecutionCost(
	evts []*events.Event,
	customPricing map[string]pricing.ModelPriceOverride,
) (totalUSD float64, unknownModels []string) {
	seenUnknown := map[string]struct{}{}
	for _, e := range evts {
		if e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			Model            string  `json:"model"`
			InputTokens      int     `json:"input_tokens"`
			OutputTokens     int     `json:"output_tokens"`
			EstimatedCostUSD float64 `json:"estimated_cost_usd"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		// Defensive: negative token counts coerced to 0. SDK never
		// ships negatives but bug-class protection.
		if p.InputTokens < 0 {
			p.InputTokens = 0
		}
		if p.OutputTokens < 0 {
			p.OutputTokens = 0
		}
		//, per-project override wins outright, even
		// when the model is otherwise unknown to the canonical
		// priceTable. This is the path that lets a customer running
		// a fine-tuned variant of a model Mesedi never shipped
		// pricing for (e.g. "my-llama-3-fine-tune") declare a real
		// rate and have cost_velocity work correctly for it.
		if _, hasOverride := customPricing[p.Model]; hasOverride {
			totalUSD += pricing.ComputeLLMCostWithOverrides(
				p.Model, p.InputTokens, p.OutputTokens, customPricing,
			)
			continue
		}
		if pricing.IsKnownModel(p.Model) {
			totalUSD += pricing.ComputeLLMCost(p.Model, p.InputTokens, p.OutputTokens)
			continue
		}
		// Unknown to backend table, fall back to SDK-shipped cost
		// on this event. Treat negative or NaN as 0 defensively.
		if p.EstimatedCostUSD > 0 {
			totalUSD += p.EstimatedCostUSD
		}
		if p.Model != "" {
			if _, dup := seenUnknown[p.Model]; !dup {
				seenUnknown[p.Model] = struct{}{}
				unknownModels = append(unknownModels, p.Model)
			}
		}
	}
	return totalUSD, unknownModels
}

// extractLLMUserMessages walks the event list and returns the
// user_message field from every llm_call event, in sequence order.
// Used by the similar-call loop detector to assemble the corpus for
// pairwise cosine-distance clustering. Skips events whose payload is
// missing or malformed, the detector handles empty slices.
//
// Mirrors computeExecutionCost's payload-shape-tolerant approach:
// unmarshal into a tiny struct that extracts only the field we need,
// ignore the rest. Survives any future payload schema additions.
func extractLLMUserMessages(evts []*events.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		if e == nil || e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			UserMessage string `json:"user_message"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.UserMessage != "" {
			out = append(out, p.UserMessage)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// API key management (sub-slice 18)
// ─────────────────────────────────────────────────────────────────────────

// ─────────────────────────────────────────────────────────────────────────
// Webhook escalation config (slice 1)
// ─────────────────────────────────────────────────────────────────────────

// scanForIdenticalCalls returns the short-hex hash of an LLM call
// (model + user_message) that appears at least `threshold` times in
// the event slice, plus true. If no call repeats that many times,
// returns ("", false). The hash is the SHA-256 of model+user_message
// truncated to 8 hex chars, readable, collision-resistant at scale,
// and acts as the failure_group signature so distinct repeated
// prompts cluster into distinct groups.
//
// Detection fires on the FIRST event that pushes a hash to the
// threshold, earlier events of the same hash are already counted but
// haven't yet crossed the line. This makes the function cheap (O(n)
// with early return) without needing to scan the entire event list
// twice.
func scanForIdenticalCalls(evts []*events.Event, threshold int) (string, bool) {
	counts := make(map[string]int, 8)
	for _, e := range evts {
		if e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			Model       string `json:"model"`
			UserMessage string `json:"user_message"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		sum := sha256.Sum256([]byte(p.Model + "\x00" + p.UserMessage))
		short := hex.EncodeToString(sum[:4])
		counts[short]++
		if counts[short] >= threshold {
			return short, true
		}
	}
	return "", false
}

// scanForInjection walks llm_call events looking for known prompt-
// injection signatures in the user_message and system_prompt fields.
// Returns the first matching pattern's name plus true; ("", false) if
// nothing matched. The scan is ordered by event sequence so the first
// injection chronologically wins.
//
// Both user_message and system_prompt are scanned because injections
// can come from either side, a compromised system prompt is rarer
// but more dangerous, so we want it caught.
func scanForInjection(
	evts []*events.Event,
	custom []*detectors.CustomPattern,
) (signature, matchedPatternID string, found bool) {
	for _, e := range evts {
		if e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			UserMessage  string `json:"user_message"`
			SystemPrompt string `json:"system_prompt"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if sig, pid, fired := detectors.DetectInjectionWithCustom(
			p.UserMessage, custom,
		); fired {
			return sig, pid, true
		}
		if sig, pid, fired := detectors.DetectInjectionWithCustom(
			p.SystemPrompt, custom,
		); fired {
			return sig, pid, true
		}
	}
	return "", "", false
}

// isTerminalStatus returns true for any execution status that means
// "the agent run is over." Detection passes (time-budget, future
// drift / cost-velocity) only fire on terminal statuses, running
// executions don't have a final duration yet.
// isTerminalStatus delegates to events.ExecutionStatus.IsTerminal so
// `started` and `awaiting_human` () are both treated as
// non-terminal. The handler keeps its own thin wrapper so the
// detector-chain guard expressions stay readable at the call sites.
func isTerminalStatus(s events.ExecutionStatus) bool {
	return s.IsTerminal()
}

// isValidLifecycleTransition reports whether the (prior, next)
// transition is allowed by the state machine. The
// matrix:
//
//	started        -> awaiting_human    (pause)
//	started        -> <any terminal>    (normal exit)
//	awaiting_human -> started           (resume)
//	awaiting_human -> <any terminal>    (HITL timeout, halt)
//	<terminal>     -> <same terminal>   (idempotent re-PATCH)
//
// All other transitions are rejected with HTTP 409 so an SDK bug
// or an out-of-order PATCH does not silently corrupt the lifecycle.
func isValidLifecycleTransition(prior, next events.ExecutionStatus) bool {
	if prior == next {
		return true // idempotent
	}
	switch prior {
	case events.StatusStarted:
		return next == events.StatusAwaitingHuman || next.IsTerminal()
	case events.StatusAwaitingHuman:
		return next == events.StatusStarted || next.IsTerminal()
	default:
		// Terminal states are immutable. The only legal "transition"
		// from a terminal state is the idempotent prior == next
		// case handled above.
		return false
	}
}

// parseIntQuery returns the integer value of a URL query parameter,
// falling back to defaultVal if missing/invalid. Clamps the result to
// [min, max]. Used by list endpoints for limit/offset.
func parseIntQuery(r *http.Request, key string, defaultVal, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return defaultVal
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}
