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
package api

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mesedi/backend/internal/anthropic"
	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/dlp"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/mail"
	meseditel "mesedi/backend/internal/otel"
	"mesedi/backend/internal/playbooks"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/severity"
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
	WebhookClient *http.Client     // task #83, outbound dispatcher HTTP client
	// DashboardURL is the public origin of the React dashboard
	// (e.g. https://app.mesedi.ai). Used to build deep-links in
	// webhook payloads and Discord embeds. When empty, the dispatcher
	// falls back to the inbound request's scheme + host, correct for
	// local dev where the dashboard is same-origin with the API, wrong
	// in prod where the dashboard lives on a different host.
	DashboardURL string
	// Stripe carries the secret key, webhook signing secret, and Pro
	// price id used by the billing endpoints (#120). All three may
	// be empty in local dev; the billing endpoints respond 503 when
	// any of them is missing.
	Stripe StripeConfig
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
	// signed tokens, and PII (Mesedi #1 + #24). Constructed once at
	// startup with the built-in rule baseline; nil disables the scan
	// entirely (useful for local dev where you do NOT want your
	// real API keys in test payloads to flag).
	DLPScanner *dlp.Scanner
	// OTel is the OpenTelemetry parallel emitter (Mesedi #22). Set
	// at startup from main.go when OTEL_EXPORTER_OTLP_ENDPOINT is
	// configured; nil (or .Enabled()==false) disables emission so
	// the entire feature can ship behind an env var. All Emit
	// calls are nil-safe at the method receiver.
	OTel *meseditel.Emitter //nolint:revive // local alias to avoid name collision with OTel SDK
	// Anthropic is the minimal Messages API client used by the
	// AI root-cause analysis endpoint (Mesedi #27). nil or a
	// client with no API key disables analysis; the handler
	// then responds with a "not configured" message instead
	// of crashing.
	Anthropic *anthropic.Client
	// AnthropicAdmin wraps the Anthropic Admin API Cost Report
	// endpoint (Mesedi #198). nil or a client without a key
	// disables the burn-rate display on the founder admin
	// dashboard; the handler then responds with a "not
	// configured" payload rather than failing the request.
	AnthropicAdmin *anthropic.AdminClient
	// SigninSecret is the server-to-server shared secret that
	// guards POST /signin (#196). The dashboard server (Cloudflare
	// Workers) calls /signin from its OAuth callback and magic-link
	// verify routes after it has already proved email ownership; the
	// secret keeps the browser from calling /signin directly with an
	// arbitrary email. Loaded from MESEDI_SIGNIN_SECRET. Empty
	// disables the endpoint entirely (returns 503), which is the
	// safe default in local dev where SSO is not configured.
	SigninSecret string
	// TOTPEncryptionKey is the 32-byte AES-256-GCM key used to seal
	// customer TOTP secrets at rest (#252). Loaded from
	// MESEDI_TOTP_ENCRYPTION_KEY (64 hex chars). Nil/empty disables
	// the 2FA endpoints — they return 503 — so a local-dev deploy
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
	// rule baseline. A compilation failure here is fatal-by-design,
	// it indicates a malformed rule in code that should NEVER ship.
	// We log and continue with a nil scanner so the rest of the API
	// stays up, but the data_leakage detector goes dark until the
	// next deploy. The nil-check in HandleIngestEvents skips the
	// scan path cleanly.
	scanner, err := dlp.NewScanner(nil)
	if err != nil {
		logger.Error("dlp scanner construction failed (data_leakage detector disabled)",
			"error", err.Error())
		scanner = nil
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
	// Server-to-server signin endpoint (#196). Public from the HTTP
	// layer's perspective so the dashboard server (Cloudflare Workers)
	// can call it cross-origin during the SSO callback / magic-link
	// verify flows; authenticity is verified inside the handler via
	// the X-Mesedi-Signin-Secret header against the configured shared
	// secret. See signin.go's file-level doc comment for the full
	// trust model.
	mux.HandleFunc("POST /signin", h.HandleSignin)
	// #252 two-factor verify completes a paused signin after the
	// customer enters their 6-digit code on the prompt page. Same
	// shared-secret model as /signin (the dashboard Worker is the
	// only legitimate caller).
	mux.HandleFunc("POST /auth/2fa-verify", h.HandleTwoFactorVerify)
	// Magic-link sign-in (#196 commit 2). /start mints a token + emails;
	// /verify is server-to-server (dashboard calls it from its handoff
	// route after the customer clicks the email link).
	mux.HandleFunc("POST /magic-link/start", h.HandleMagicLinkStart)
	mux.HandleFunc("GET /magic-link/verify", h.HandleMagicLinkVerify)

	// Email verification (#232). The confirm + resend endpoints are
	// public — the recipient may not be signed in when they click
	// the verification link, and resend is keyed on the email itself.
	// The status endpoint is bearer-gated because the dashboard reads
	// it under the customer's existing auth.
	mux.HandleFunc("POST /api/email-verify/confirm", h.HandleEmailVerifyConfirm)
	mux.HandleFunc("POST /api/email-verify/resend", h.HandleEmailVerifyResend)
	// #213 Batch 2 — POST /auth/logout destroys the caller's session.
	// Public so a customer who has already lost their session row
	// (expired, kicked, key revoked) can still click Sign Out
	// without a 401. See auth_logout.go.
	mux.HandleFunc("POST /auth/logout", h.HandleAuthLogout)
	// Stripe webhook receiver. Public because Stripe POSTs server-
	// to-server with no bearer; authenticity is verified inside the
	// handler via the Stripe-Signature header against the configured
	// webhook secret.
	mux.HandleFunc("POST /billing/webhook", h.HandleStripeWebhook)
	// Task #263, public invite-accept endpoints. Auth IS the token:
	// the random hex string in the URL path is the authentication.
	// GET surfaces the invite info so the accept page can render
	// "you've been invited to X as Y" before the user redeems.
	mux.HandleFunc("GET /invites/{token}", h.HandleGetInviteByToken)
	mux.HandleFunc("POST /invites/{token}/accept", h.HandleAcceptInvite)
}

func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	// Phase 1 ingest surface.
	mux.HandleFunc("POST /executions", h.HandleCreateExecution)
	// #118 Slice 1, read-side project surface for the dashboard.
	mux.HandleFunc("GET /project", h.HandleGetProject)
	mux.HandleFunc("PATCH /project/name", h.HandleSetProjectName)
	mux.HandleFunc("GET /me", h.HandleGetMe)
	// #232 — dashboard polls this once per layout mount to decide
	// whether to render the email-verification interstitial.
	mux.HandleFunc("GET /me/email-verification-status", h.HandleEmailVerificationStatus)

	// #252 customer-facing TOTP / two-factor authentication.
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
	// Mesedi #10, multi-agent topology graph.
	mux.HandleFunc("GET /executions/{id}/topology", h.HandleGetExecutionTopology)
	mux.HandleFunc("GET /stats", h.HandleStats)
	// Task #259, org-level rollup across all projects owned by the
	// same user (v0.1 tenant model = owner_user_id). Resolves the
	// authenticated project's owner, sums burn across sibling projects.
	mux.HandleFunc("GET /me/rollup", h.HandleOrgRollup)
	mux.HandleFunc("GET /me/savings", h.HandleSavings)
	// Task #252, tenant monthly budget ceiling. GET fetches the
	// configured ceiling (ErrNotFound -> 404 so the UI can render
	// the "set up a ceiling" empty state). PUT upserts.
	mux.HandleFunc("GET /me/budget-ceiling", h.HandleGetBudgetCeiling)
	mux.HandleFunc("PUT /me/budget-ceiling", h.HandleUpsertBudgetCeiling)
	// Task #261, per-project failure-class severity overrides.
	// GET returns the full map (defaults + any overrides), PUT
	// upserts an override for one class, DELETE reverts to default.
	mux.HandleFunc("GET /me/class-severities", h.HandleListClassSeverities)
	mux.HandleFunc("PUT /me/class-severities/{class}", h.HandleUpsertClassSeverity)
	mux.HandleFunc("DELETE /me/class-severities/{class}", h.HandleDeleteClassSeverity)
	// Task #262, per-project data retention.
	mux.HandleFunc("GET /me/retention", h.HandleGetRetention)
	mux.HandleFunc("PUT /me/retention", h.HandleSetRetention)
	// Task #271 migration 040, per-project provider_incident
	// detector threshold (default 2, single-tenant customers
	// typically set to 1).
	mux.HandleFunc("GET /me/provider-incident-config", h.HandleGetProviderIncidentConfig)
	mux.HandleFunc("PUT /me/provider-incident-config", h.HandleSetProviderIncidentConfig)
	// Task #276, per-project time_budget detector threshold (ms).
	mux.HandleFunc("GET /me/time-budget-config", h.HandleGetTimeBudgetConfig)
	mux.HandleFunc("PUT /me/time-budget-config", h.HandleSetTimeBudgetConfig)
	mux.HandleFunc("GET /me/cost-velocity-config", h.HandleGetCostVelocityConfig)
	mux.HandleFunc("PUT /me/cost-velocity-config", h.HandleSetCostVelocityConfig)
	mux.HandleFunc("GET /me/cost-velocity-rate-config", h.HandleGetCostVelocityRateConfig)
	mux.HandleFunc("PUT /me/cost-velocity-rate-config", h.HandleSetCostVelocityRateConfig)
	mux.HandleFunc("GET /me/pricing-info", h.HandleGetPricingInfo)
	// Task #270.a, per-project tool_schema_drift return_value byte cap.
	mux.HandleFunc("GET /me/tool-return-value-config", h.HandleGetToolReturnValueConfig)
	mux.HandleFunc("PUT /me/tool-return-value-config", h.HandleSetToolReturnValueConfig)
	// Wave 2.1.a, per-project custom-pattern storage for the three
	// security detectors (prompt_injection / data_leakage /
	// sandbox_escape). Detectors read these in Wave 2.1.b; dashboard
	// editor lands in Wave 2.1.c.
	mux.HandleFunc("GET /me/pattern-config/{detector}", h.HandleListPatternConfig)
	mux.HandleFunc("POST /me/pattern-config/{detector}", h.HandleCreatePatternConfig)
	mux.HandleFunc("PATCH /me/pattern-config/{detector}/{pattern_id}", h.HandleUpdatePatternConfig)
	mux.HandleFunc("DELETE /me/pattern-config/{detector}/{pattern_id}", h.HandleDeletePatternConfig)
	// Task #270.c, per-project truncation-rate telemetry.
	mux.HandleFunc("GET /me/tool-return-value-stats", h.HandleGetToolReturnValueStats)
	// Task #276.d, per-project config-fallback telemetry.
	mux.HandleFunc("GET /me/config-fallback-stats", h.HandleGetConfigFallbackStats)
	// Task #263, Team / multi-seat. Admin-gated endpoints for
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
	// Mesedi #5, cost broken down by tenant_id over a time window.
	mux.HandleFunc("GET /reports/cost-by-tenant", h.HandleReportCostByTenant)
	// Phase 3b sub-slice 9, executions inside a failure_group.
	mux.HandleFunc("GET /failure-groups/{id}/executions", h.HandleListExecutionsInFailureGroup)
	// Mesedi #27, LLM-assisted root-cause analysis. Cached on the
	// failure_group row for 24h or until last_seen advances; force
	// regenerate with ?regenerate=1.
	mux.HandleFunc("POST /failure-groups/{id}/analyze", h.HandleAnalyzeFailureGroup)
	// Phase 3b sub-slice 18, API key management surface.
	mux.HandleFunc("GET /api-keys", h.HandleListAPIKeys)
	mux.HandleFunc("POST /api-keys", h.HandleCreateAPIKey)
	mux.HandleFunc("DELETE /api-keys/{id}", h.HandleRevokeAPIKey)
	// #207 Audit logs v1 (admin role required; enforced inside handler).
	// Backs the Team-tier "Audit logs (who did what, when)" promise.
	mux.HandleFunc("GET /audit-log", h.HandleListAuditEvents)
	// Sub-slice 21b, SSE remote-halt channel.
	mux.HandleFunc("GET /executions/{id}/halt-stream", h.HandleHaltStream)
	mux.HandleFunc("POST /executions/{id}/halt", h.HandleTriggerHalt)
	// Tier 1 Playbooks, canonical fix descriptions per failure-class
	// signature. Plain GET with query params; content is the embedded
	// markdown shipped in internal/playbooks/content/.
	mux.HandleFunc("GET /playbooks", h.HandleGetPlaybook)
	// Task #83, webhook escalation config + dispatcher.
	mux.HandleFunc("GET /webhooks", h.HandleListWebhooks)
	mux.HandleFunc("POST /webhooks", h.HandleCreateWebhook)
	mux.HandleFunc("DELETE /webhooks/{id}", h.HandleDeleteWebhook)
	// Slice 2: manual test-delivery trigger + deliveries log read.
	mux.HandleFunc("POST /webhooks/{id}/test", h.HandleTestWebhook)
	mux.HandleFunc("GET /webhooks/{id}/deliveries", h.HandleListWebhookDeliveries)
	// Task #120, Stripe billing endpoints (auth-required).
	mux.HandleFunc("GET /billing", h.HandleGetBilling)
	mux.HandleFunc("GET /billing/usage", h.HandleGetBillingUsage)
	// #197: AI root-cause analyses usage counter. Surfaced on
	// /app/billing + as a chip on the main user dashboard so Team
	// customers can see headroom before they hit the 200/period cap.
	mux.HandleFunc("GET /billing/ai-analyses-usage", h.HandleGetAIAnalysesUsage)
	mux.HandleFunc("POST /billing/checkout", h.HandleCreateCheckout)
	mux.HandleFunc("POST /billing/portal", h.HandleCreatePortal)
	mux.HandleFunc("POST /billing/payment-method/setup", h.HandleCreateSetupCheckout)
	mux.HandleFunc("POST /billing/payment-method/remove", h.HandleRemovePaymentMethod)
	// #187: customer-configurable monthly overage cap. PUT body is
	// {"cap_usd": <float>}; the value is persisted to
	// projects.billing_cap_usd and consulted by the hobby billing
	// scheduler at period close + ingest-time gate.
	mux.HandleFunc("PUT /billing/cap", h.HandleUpdateBillingCap)
	// #188 danger-zone flow on /app/settings. POST /billing/downgrade
	// cancels the Cloud Team subscription at period end (keep data).
	// POST /billing/close-account cancels immediately + cascade-deletes
	// the project (lose data, force logout on next 401).
	mux.HandleFunc("POST /billing/downgrade", h.HandleDowngradeToHobby)
	mux.HandleFunc("POST /billing/close-account", h.HandleCloseAccount)
}

// HandleGetPlaybook returns the markdown content for the playbook
// matching (failure_class, signature) query parameters. Returns:
//
//	200 + text/markdown: content
//	400: missing or empty query params
//	404: no playbook matches this (class, signature)
//
// No auth-context project check needed, playbook content is
// universal (doesn't reference any particular project's data), so
// authenticated callers in any project can read any playbook.
func (h *Handlers) HandleGetPlaybook(w http.ResponseWriter, r *http.Request) {
	failureClass := r.URL.Query().Get("failure_class")
	signature := r.URL.Query().Get("signature")
	if failureClass == "" || signature == "" {
		writeError(w, http.StatusBadRequest,
			"failure_class and signature query parameters are both required")
		return
	}

	body, err := playbooks.Load(failureClass, signature)
	if err != nil {
		if errors.Is(err, playbooks.ErrNotFound) {
			writeError(w, http.StatusNotFound,
				"no playbook for failure_class="+failureClass+" signature="+signature)
			return
		}
		h.Logger.Error("playbook load failed",
			"failure_class", failureClass,
			"signature", signature,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "playbook load failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// HandleCreateExecution accepts an Execution at the agent's entry point
// and records the start of a run. Phase 1 implementation just validates
// shape and logs; Phase 1.5 persists to Postgres.
func (h *Handlers) HandleCreateExecution(w http.ResponseWriter, r *http.Request) {
	var exec events.Execution
	if err := decodeJSON(r, &exec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if exec.ExecutionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id is required")
		return
	}

	// Auth-attached project_id is the source of truth. If the request
	// body provided one and it doesn't match, reject, this catches
	// SDK bugs where the wrong API key was used for the wrong project.
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context (auth middleware not engaged)")
		return
	}
	if exec.ProjectID != "" && exec.ProjectID != authProjectID {
		writeError(w, http.StatusForbidden,
			"project_id in body does not match authenticated project")
		return
	}
	exec.ProjectID = authProjectID

	// Stamp the authenticated API key onto the execution (#255). This
	// is the only place the column is set; the SDK never supplies it
	// (the api_key_id field on the wire is server-stamped and any
	// body-supplied value is overwritten here). Without this, the
	// Terms commitment to "share the information we have about the
	// key's recent use" on a compromise report cannot be honored
	// per-key, only per-project.
	if authKeyID, hasKey := APIKeyIDFromContext(r.Context()); hasKey && authKeyID != "" {
		k := authKeyID
		exec.APIKeyID = &k
	}

	if exec.Status == "" {
		exec.Status = events.StatusStarted
	}
	if exec.StartedAt.IsZero() {
		exec.StartedAt = time.Now().UTC()
	}

	// Billing cap enforcement (Mesedi pricing alignment).
	//
	// Before persisting the execution, check whether the project's
	// per-period paid-overage cost has already crossed billing_cap_usd.
	// When it has, silent-drop the execution with 402 + a structured
	// "billing cap reached" message so the SDK can surface it without
	// retrying. Enterprise tier is exempt (no per-execution overage).
	//
	// The cap check uses the current row, so a request arriving while
	// the meter is exactly at the cap will be accepted; the *next*
	// request after the increment is the one that gets blocked. That's
	// the desired behavior: the customer never pays past the cap,
	// modulo a single-execution slop on the boundary.
	if proj, err := h.Store.GetProject(r.Context(), authProjectID); err == nil && proj != nil {
		if blocked, capUSD, costUSD := capExceeded(proj); blocked {
			h.Logger.Info("ingest blocked: billing cap reached",
				"project_id", authProjectID,
				"tier", proj.Tier,
				"cost_usd", costUSD,
				"cap_usd", capUSD,
			)
			writeError(w, http.StatusPaymentRequired,
				fmt.Sprintf("billing cap reached ($%.2f of $%.2f). New executions are paused until the next billing period or until the cap is raised.", costUSD, capUSD))
			return
		}
	}

	if err := h.Store.CreateExecution(r.Context(), &exec); err != nil {
		h.Logger.Error("create execution failed",
			"execution_id", exec.ExecutionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	// #120, increment the per-period execution counter. Best-effort:
	// a failure here logs a warning but does not propagate to the
	// caller. Counting must never block ingest. Enforcement (Hobby
	// silent-drop, Pro overage metering) lands in a follow-up slice
	// that gates on this counter before the CreateExecution call.
	if err := h.Store.IncrementExecutionsThisPeriod(r.Context(), exec.ProjectID); err != nil {
		h.Logger.Warn("increment executions counter failed (continuing)",
			"project_id", exec.ProjectID,
			"error", err.Error(),
		)
	}

	h.Logger.Info("execution created",
		"execution_id", exec.ExecutionID,
		"project_id", exec.ProjectID,
		"status", exec.Status,
		"started_at", exec.StartedAt.Format(time.RFC3339),
		"sdk_language", exec.SDKLanguage,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"execution_id": exec.ExecutionID,
		"status":       exec.Status,
	})
}

// HandleUpdateExecution marks an existing execution as completed, crashed,
// halted, etc. Idempotent, repeated PATCH calls with the same status are
// silently accepted.
//
// Phase 3a addition: if the PATCH transitions an execution to status=crashed
// AND a crash_signature is provided, the execution is grouped into the
// appropriate failure_group via Store.GroupCrashedExecution. The grouping
// step is best-effort: if it fails, the request still returns 200 because
// the execution's primary update has already succeeded; only the
// dashboard's grouping view is degraded.
func (h *Handlers) HandleUpdateExecution(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}

	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context (auth middleware not engaged)")
		return
	}

	var patch events.Execution
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	patch.ExecutionID = executionID

	// Mesedi #18: read the current state to drive the lifecycle
	// state machine. Pause / resume transitions take a dedicated
	// code path (Store.PauseExecution / Store.ResumeExecution),
	// distinct from the normal terminal write through
	// UpdateExecution. We need the prior status to decide which.
	currentExec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found: "+executionID)
			return
		}
		h.Logger.Error("get execution for lifecycle check failed",
			"execution_id", executionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "lifecycle probe failed: "+err.Error())
		return
	}
	if currentExec.ProjectID != authProjectID {
		// Cross-tenant access returns 404 to avoid leaking that the
		// id exists on a different project, same posture as
		// HandleGetExecution.
		writeError(w, http.StatusNotFound, "execution not found: "+executionID)
		return
	}
	priorStatus := currentExec.Status

	// Validate the transition. Rejecting illegal transitions early
	// keeps the rest of the handler clean.
	if !isValidLifecycleTransition(priorStatus, patch.Status) {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"invalid lifecycle transition: %s -> %s", priorStatus, patch.Status,
		))
		return
	}

	// Branch on the four meaningful transitions (Mesedi #18).
	now := time.Now().UTC()
	switch {
	case priorStatus == events.StatusStarted && patch.Status == events.StatusAwaitingHuman:
		// Pure pause. No ended_at, no detector chain (not terminal).
		if err := h.Store.PauseExecution(r.Context(), executionID, authProjectID, now); err != nil {
			h.Logger.Error("pause execution failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
			writeError(w, http.StatusInternalServerError, "pause failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"execution_id": executionID,
			"status":       events.StatusAwaitingHuman,
		})
		return

	case priorStatus == events.StatusAwaitingHuman && patch.Status == events.StatusStarted:
		// Pure resume. No ended_at, no detector chain.
		if err := h.Store.ResumeExecution(r.Context(), executionID, authProjectID, now); err != nil {
			h.Logger.Error("resume execution failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
			writeError(w, http.StatusInternalServerError, "resume failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"execution_id": executionID,
			"status":       events.StatusStarted,
		})
		return

	case priorStatus == events.StatusAwaitingHuman && patch.Status.IsTerminal():
		// Terminal from paused. Flush accumulated paused time first
		// by resuming (which writes total_paused_ms + clears
		// paused_at), then fall through to the normal terminal
		// update path. The resume here is a synthetic internal
		// transition; from the customer's perspective the
		// execution went paused -> terminal in one PATCH.
		if err := h.Store.ResumeExecution(r.Context(), executionID, authProjectID, now); err != nil {
			h.Logger.Error("flush paused time before terminal failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
			writeError(w, http.StatusInternalServerError, "lifecycle flush failed: "+err.Error())
			return
		}
		// Fall through to UpdateExecution below.
	}

	// Default to ended_at = now for the terminal path so legacy
	// SDKs that don't supply it still get a sensible value.
	// Non-terminal transitions return above before reaching here.
	if patch.EndedAt == nil {
		patch.EndedAt = &now
	}

	// Mesedi #18: compute the effective (working-time) duration by
	// subtracting accumulated paused time from the wall-clock
	// duration the SDK supplied. Detectors that gate on
	// "the agent worked for too long" (currently only time_budget)
	// must use effectiveDurationMs so a HITL wait does not falsely
	// trip them.
	//
	// totalPausedMs reflects the FINAL accumulated paused time:
	// every closed pause cycle plus, if we just flushed the
	// terminal-from-paused branch above, the final cycle that the
	// synthetic resume just wrote to the row. We do not re-read the
	// execution from the store here; we reconstruct the same value
	// the resume call would have produced.
	totalPausedMs := currentExec.TotalPausedMs
	if priorStatus == events.StatusAwaitingHuman && patch.Status.IsTerminal() && currentExec.PausedAt != nil {
		flushMs := now.Sub(*currentExec.PausedAt).Milliseconds()
		if flushMs > 0 {
			totalPausedMs += flushMs
		}
	}
	effectiveDurationMs := patch.DurationMs - totalPausedMs
	if effectiveDurationMs < 0 {
		// Defensive: SDK clock skew or pause arithmetic drift.
		// Falling back to the raw wall-clock keeps the detector
		// chain from getting confused by a negative duration.
		effectiveDurationMs = patch.DurationMs
	}

	if err := h.Store.UpdateExecution(r.Context(), &patch); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found: "+patch.ExecutionID)
			return
		}
		h.Logger.Error("update execution failed",
			"execution_id", patch.ExecutionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	// Phase 7 v0.0.1: model-drift detector, runs FIRST in the detection
	// chain so it wins the idempotency claim over crashes when the crash
	// IS caused by a new model. The classic case: agent calls a model
	// that doesn't exist (deprecated, typo, misrouted), Anthropic
	// returns 404, the agent crashes. Without this ordering, the
	// execution lands in `crashes` with a generic stack-trace signature
	// and the customer never sees the actionable "you used a new model"
	// classification. With drift first, the same execution lands in
	// `drift / new_model:<name>`, which is the right surface for "why
	// did my agent suddenly start failing today."
	//
	// For executions that crashed WITHOUT a model change, drift's
	// condition is false (current models all in historical) → it
	// no-ops, and crashes claims normally below. So this change is
	// safe: it only diverts the model-driven crashes, leaving
	// non-model-related crashes unaffected.
	//
	// Best-effort throughout, drift query failures log and continue
	// rather than blocking the rest of the detection pipeline.
	if isTerminalStatus(patch.Status) {
		// 7-day historical window, same for both drift signals.
		cutoff := time.Now().Add(-7 * 24 * time.Hour)

		// ── Drift v1, model-mix signal ─────────────────────────────
		// Catches: this execution used a model the project hasn't seen.
		currentModels, mErr := h.Store.ListModelsForExecution(r.Context(), executionID)
		if mErr != nil {
			h.Logger.Warn("drift: list models for execution failed (skipping model-drift)",
				"execution_id", executionID,
				"error", mErr.Error(),
			)
		} else if len(currentModels) > 0 {
			historicalModels, hErr := h.Store.ListModelsForProjectSince(r.Context(), authProjectID, cutoff, executionID)
			if hErr != nil {
				h.Logger.Warn("drift: list project models failed (skipping model-drift)",
					"project_id", authProjectID,
					"error", hErr.Error(),
				)
			} else if len(historicalModels) > 0 {
				if signature, drift := detectors.DetectModelDrift(currentModels, historicalModels); drift {
					isNew, dErr := h.Store.GroupDriftSignal(r.Context(), executionID, authProjectID, signature)
					if dErr != nil {
						h.Logger.Warn("drift grouping failed (continuing)",
							"execution_id", executionID,
							"signature", signature,
							"error", dErr.Error(),
						)
					} else {
						h.Logger.Info("model drift detected",
							"execution_id", executionID,
							"signature", signature,
							"current_models", currentModels,
							"historical_models_count", len(historicalModels),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassDrift, signature, isNew, dErr)
				}
			}
		}

		// Drift v2 (lexical) moved to the tail of the detector chain ,
		// see the cost_velocity block below.
	}

	// Phase 3a: link crashed executions to their failure_group. Best-effort ,
	// a grouping failure doesn't fail the PATCH because the execution itself
	// is already correctly recorded. Runs AFTER drift, if drift already
	// claimed this execution, GroupCrashedExecution's idempotency check
	// short-circuits as a no-op.
	if patch.Status == events.StatusCrashed && patch.CrashSignature != "" {
		isNew, err := h.Store.GroupCrashedExecution(r.Context(), executionID, authProjectID, patch.CrashSignature)
		if err != nil {
			h.Logger.Warn("crash grouping failed (continuing)",
				"execution_id", executionID,
				"crash_signature", patch.CrashSignature,
				"error", err.Error(),
			)
		}
		h.maybeFireWebhook(r, authProjectID, store.FailureClassCrashes, patch.CrashSignature, isNew, err)
	}

	// Phase 3b sub-slice 11: step-count detector. Any terminal execution
	// with > 10 events gets grouped as loops/step-count. Runs after the
	// crash and time-budget checks, so it's the lowest-priority
	// classification, an execution that crashed, took too long, AND
	// emitted lots of events ends up classified as crashes (first match
	// wins via the failure_group_id short-circuit). Threshold of 10 is
	// artificially low for v0.0.1 demo visibility; production default
	// 50+ per the concept doc.
	if isTerminalStatus(patch.Status) {
		count, err := h.Store.CountEventsForExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("count events for step-count check failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if count > 10 {
			isNew, gErr := h.Store.GroupStepCountExceedance(r.Context(), executionID, authProjectID, count)
			if gErr != nil {
				h.Logger.Warn("step-count grouping failed (continuing)",
					"execution_id", executionID,
					"event_count", count,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, store.StepCountSignature(count), isNew, gErr)
		}
	}

	// Mesedi #3 + #4 — context_overflow and token_waste detectors.
	// Both consume the execution's llm_call events so we query
	// them once and feed both detectors from the result. They run
	// AFTER loops/step_count (which catch coarser patterns) and
	// BEFORE semantic_loop / tool_schema_drift (which catch finer
	// downstream patterns). context_overflow fires on cumulative
	// input_tokens vs configured model window; token_waste fires on
	// repeating user_prompt prefixes.
	if isTerminalStatus(patch.Status) {
		llmPayloads, err := h.Store.ListLLMCallPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list llm_call payloads for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(llmPayloads) > 0 {
			rawPayloads := make([]json.RawMessage, len(llmPayloads))
			for i, p := range llmPayloads {
				rawPayloads[i] = json.RawMessage(p)
			}
			// context_overflow first; fail-level overrides
			// token_waste claim if both fired on the same exec.
			if sig, fired := detectors.DetectContextOverflow(rawPayloads); fired {
				isNew, gErr := h.Store.GroupContextOverflow(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("context-overflow grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassContextOverflow, sig, isNew, gErr)
			}
			if sig, fired := detectors.DetectTokenWaste(rawPayloads); fired {
				isNew, gErr := h.Store.GroupTokenWaste(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("token-waste grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassTokenWaste, sig, isNew, gErr)
			}
		}
	}

	// Mesedi #6 — semantic_loop detector. Hashes the canonical
	// state across all checkpoint events on the execution; if any
	// hash recurs 3+ times the execution is clustered as
	// semantic_loop. Catches the "agent revisits the same logical
	// state via different surface text" pattern that step_count and
	// identical_call_loops cannot see. Runs AFTER the loops family
	// so simpler patterns (exact-call repeat, time-budget breach)
	// claim first; this detector picks up only the residue.
	if isTerminalStatus(patch.Status) {
		checkpoints, err := h.Store.ListCheckpointPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list checkpoint payloads for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(checkpoints) > 0 {
			payloads := make([]json.RawMessage, len(checkpoints))
			for i, p := range checkpoints {
				payloads[i] = json.RawMessage(p)
			}
			if sig, fired := detectors.DetectSemanticLoop(payloads); fired {
				isNew, gErr := h.Store.GroupSemanticLoop(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("semantic-loop grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassSemanticLoop, sig, isNew, gErr)
			}
		}
	}

	// Mesedi #8 — tool_schema_drift detector. For each tool the
	// execution invoked successfully, compare the LAST successful
	// return_value's shape on this execution against the project's
	// historical roll-up for the same tool. Fires when a previously-
	// stable tool returns a new shape, catching silent third-party
	// API version bumps.
	//
	// Runs AFTER semantic_loop because:
	//   - loop-class errors are about the AGENT's behavior; schema
	//     drift is about the WORLD's behavior; resolving the loop
	//     first lets SREs see the simpler root cause if both exist.
	//   - if multiple drift signals fire across multiple tools, only
	//     the first-found one claims the execution (deterministic
	//     iteration order via ListToolNamesInExecution's distinct
	//     scan).
	if isTerminalStatus(patch.Status) {
		toolNames, err := h.Store.ListToolNamesInExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list tool names for schema-drift failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else {
			// #270.a: per-project cap on return_value bytes used
			// for fingerprinting. Returns above this threshold are
			// excluded from the comparison (treated as inconclusive,
			// mirroring the SDK's "<truncated>" sentinel). Default
			// 8192 on store error so a transient DB blip can't
			// silence the detector entirely.
			maxBytes := 8192
			if mb, mbErr := h.Store.GetProjectToolReturnValueMaxBytes(r.Context(), authProjectID); mbErr == nil {
				maxBytes = mb
			} else {
				h.Logger.Warn("get tool_return_value_max_bytes failed; using default",
					"execution_id", executionID,
					"project_id", authProjectID,
					"error", mbErr.Error(),
				)
				// #276.d: durable telemetry — surfaces in dashboard.
				h.recordAuditEventForProject(
					r.Context(),
					authProjectID, "system",
					"config_fallback", "project_config", "tool_return_value_max_bytes",
					map[string]any{
						"error":          mbErr.Error(),
						"fallback_value": maxBytes,
					},
				)
			}
			for _, toolName := range toolNames {
				// Current-execution shape: query the most recent
				// successful tool_call for this tool on THIS
				// execution. The detector compares it against the
				// project's historical roll-up.
				currentReturns, err := h.Store.ListSuccessfulToolReturns(r.Context(), authProjectID, toolName, "", 1)
				if err != nil || len(currentReturns) == 0 {
					// "" excludes nothing, gets the most recent
					// project-wide which includes the current
					// execution. If it returns nothing, skip.
					continue
				}
				if len(currentReturns[0]) > maxBytes {
					// Exceeds the per-project cap; treat as
					// non-comparable rather than hashing a partial
					// or oversized structure.
					continue
				}
				currentShape := detectors.ReturnShapeHash(json.RawMessage(currentReturns[0]))
				if currentShape == "" {
					continue
				}
				history, err := h.Store.ListSuccessfulToolReturns(r.Context(), authProjectID, toolName, executionID, 100)
				if err != nil {
					h.Logger.Warn("list tool-return history failed",
						"execution_id", executionID,
						"tool_name", toolName,
						"error", err.Error(),
					)
					continue
				}
				shapeCounts := map[string]int{}
				for _, raw := range history {
					if len(raw) > maxBytes {
						// Same cap for history rows — keeps
						// fingerprint computation symmetric so a
						// customer raising/lowering the cap sees
						// consistent comparisons.
						continue
					}
					shape := detectors.ReturnShapeHash(json.RawMessage(raw))
					if shape == "" {
						continue
					}
					shapeCounts[shape]++
				}
				if sig, fired := detectors.DetectSchemaDrift(toolName, currentShape, shapeCounts); fired {
					isNew, gErr := h.Store.GroupToolSchemaDrift(r.Context(), executionID, authProjectID, sig)
					if gErr != nil {
						h.Logger.Warn("tool-schema-drift grouping failed (continuing)",
							"execution_id", executionID,
							"signature", sig,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassToolSchemaDrift, sig, isNew, gErr)
					break // one drift signal per execution is enough
				}
			}
		}
	}

	// Phase 3b sub-slice 13: tool-failures detector. If any tool_call
	// event in the execution had payload.status="failed", classify the
	// execution as tool_failures with signature=tool_name. Different
	// from crashes (where the exception escaped @wrap), tool-failures
	// catches the silent-degradation pattern where the agent recovers
	// from a tool exception and ran to completion but produced
	// degraded output. Runs after the loop detectors so an execution
	// that BOTH had a failed tool AND was a runaway loop classifies
	// as the loop (loops are higher-priority, they waste more).
	if isTerminalStatus(patch.Status) {
		toolName, err := h.Store.FindFirstFailedToolName(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find failed tool for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if toolName != "" {
			isNew, gErr := h.Store.GroupToolFailure(r.Context(), executionID, authProjectID, toolName)
			if gErr != nil {
				h.Logger.Warn("tool-failure grouping failed (continuing)",
					"execution_id", executionID,
					"tool_name", toolName,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassToolFailures, toolName, isNew, gErr)
		}
	}

	// Mesedi #2 — infrastructure_throttled detector. If any
	// infrastructure_event was recorded for this execution, classify
	// it as infrastructure_throttled with a signature derived from
	// (reason, provider, dimension). Distinguishes "your provider is
	// rate-limiting you" / "your circuit breaker tripped" / "you hit
	// your monthly quota" from generic tool_failures, so SREs get a
	// distinct alert chain with a distinct playbook (raise quota vs
	// debug code).
	//
	// Runs after tool_failures so an execution that experienced both
	// a tool failure AND a transport throttling event surfaces under
	// the throttling group (which actionably points at the underlying
	// cause; the tool failure was just the symptom).
	if isTerminalStatus(patch.Status) {
		throttleSig, err := h.Store.FindFirstThrottlingSignal(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find throttling signal for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if throttleSig != "" {
			isNew, gErr := h.Store.GroupInfrastructureThrottled(r.Context(), executionID, authProjectID, throttleSig)
			if gErr != nil {
				h.Logger.Warn("infrastructure-throttled grouping failed (continuing)",
					"execution_id", executionID,
					"signature", throttleSig,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassInfraThrottled, throttleSig, isNew, gErr)
		}
	}

	// Mesedi #17 — sandbox_escape detector. Scans every tool_call
	// payload for known sandbox-escape patterns (os.system, raw
	// sockets, /proc/self, instance-metadata endpoints, secret
	// file paths). Runs at the security tier alongside
	// data_leakage; both fire same-priority and the dashboard
	// renders both red.
	if isTerminalStatus(patch.Status) {
		toolPayloads, err := h.Store.ListAllToolCallPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list tool_call payloads for sandbox-escape failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(toolPayloads) > 0 {
			rawPayloads := make([]json.RawMessage, len(toolPayloads))
			for i, p := range toolPayloads {
				rawPayloads[i] = json.RawMessage(p)
			}
			if sig, fired := detectors.DetectSandboxEscape(rawPayloads); fired {
				isNew, gErr := h.Store.GroupSandboxEscape(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("sandbox-escape grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassSandboxEscape, sig, isNew, gErr)
			}
		}
	}

	// Mesedi #1 — data_leakage detector. If any dlp_scan_result
	// event for this execution recorded critical/high hits, cluster
	// the execution under data_leakage with the matched rule_id as
	// the signature. Runs after infrastructure_throttled so an
	// execution that experienced both transport throttling AND a
	// credential leak surfaces under the leak (which is a security
	// incident; throttling is operational); the lower-priority
	// classifier is a no-op once a higher one has claimed the
	// execution.
	if isTerminalStatus(patch.Status) {
		dlpSig, err := h.Store.FindFirstDLPSignal(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find dlp signal for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if dlpSig != "" {
			isNew, gErr := h.Store.GroupDataLeakage(r.Context(), executionID, authProjectID, dlpSig)
			if gErr != nil {
				h.Logger.Warn("data-leakage grouping failed (continuing)",
					"execution_id", executionID,
					"rule_id", dlpSig,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassDataLeakage, dlpSig, isNew, gErr)
		}
	}

	// Phase 3b sub-slice 14: validator-failures detector. If any
	// validator_result event in the execution had payload.passed=false,
	// classify the execution as validator_failures with
	// signature=validator_name. Same silent-degradation family as
	// tool-failures, the agent ran to completion but produced output
	// that a downstream quality check failed.
	if isTerminalStatus(patch.Status) {
		validatorName, err := h.Store.FindFirstFailedValidator(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find failed validator for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if validatorName != "" {
			isNew, gErr := h.Store.GroupValidatorFailure(r.Context(), executionID, authProjectID, validatorName)
			if gErr != nil {
				h.Logger.Warn("validator-failure grouping failed (continuing)",
					"execution_id", executionID,
					"validator_name", validatorName,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassValidator, validatorName, isNew, gErr)
		}
	}

	// Mesedi #14 — grounding_failure detector. Aggregates the
	// eval_score events ingested via emit_eval_score (#9). Fires
	// when any external evaluator returned passed=false, or when
	// mean score across higher_is_better evaluators fell below 0.5.
	// Runs after validator_failures because:
	//   - validator_result represents the agent's OWN self-check;
	//     eval_score represents an EXTERNAL evaluator's verdict.
	//     Both fire on quality issues but at different rigor levels.
	if isTerminalStatus(patch.Status) {
		evalPayloads, err := h.Store.ListEvalScorePayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list eval_score payloads for grounding detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(evalPayloads) > 0 {
			rawPayloads := make([]json.RawMessage, len(evalPayloads))
			for i, p := range evalPayloads {
				rawPayloads[i] = json.RawMessage(p)
			}
			if sig, fired := detectors.DetectGroundingFailure(rawPayloads); fired {
				isNew, gErr := h.Store.GroupGroundingFailure(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("grounding-failure grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassGroundingFailure, sig, isNew, gErr)
			}
		}
	}

	// Mesedi #12 — cascading_failure detector. Joins this
	// execution's agent_handoff events (#11) with the terminal
	// status of each referenced child execution and fires when a
	// handoff was followed by the child reaching a failure
	// terminal state. Runs after grounding_failure because:
	//   - grounding/eval signals are per-evaluator and orthogonal;
	//   - cascading_failure is a CROSS-execution signal that
	//     subsumes the child's own per-execution failure_group
	//     into a "this run is part of a chain" cluster, which is
	//     a more useful framing for the customer than the raw
	//     two-separate-bugs view.
	if isTerminalStatus(patch.Status) {
		handoffs, err := h.Store.ListHandoffsWithChildStatus(r.Context(), executionID, authProjectID)
		if err != nil {
			h.Logger.Warn("list handoffs with child status for cascade detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(handoffs) > 0 {
			if sig, fired := detectors.DetectCascadingFailure(handoffs); fired {
				isNew, gErr := h.Store.GroupCascadingFailure(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("cascading-failure grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassCascadingFailure, sig, isNew, gErr)
			}
		}
	}

	// Mesedi #13 — coordination_deadlock detector. Walks the
	// topology subtree rooted at this execution, collects every
	// agent_handoff edge, and fires on the first 2-cycle in the
	// agent-role graph (A→B AND B→A in the same subtree). Runs
	// after cascading_failure so that a deadlock that ALSO
	// produced a cascade gets attributed to the more specific
	// "deadlock" class (timeout-without-progress is a more
	// actionable framing than "child crashed").
	if isTerminalStatus(patch.Status) {
		edges, err := h.Store.ListHandoffEdgesInTopology(r.Context(), executionID, authProjectID, 0)
		if err != nil {
			h.Logger.Warn("list handoff edges for deadlock detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(edges) >= 2 {
			if sig, fired := detectors.DetectCoordinationDeadlock(edges); fired {
				isNew, gErr := h.Store.GroupCoordinationDeadlock(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("coordination-deadlock grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassCoordinationDeadlock, sig, isNew, gErr)
			}
		}
	}

	// Mesedi #16 — provider_incident detector. Scans this
	// execution's llm_call payloads for provider errors, then
	// asks the store how many DISTINCT tenants in the same
	// project saw the same (provider, error_class) error in the
	// recent window. Fires when the cross-tenant count meets
	// MinTenantsForProviderIncident.
	//
	// Order rationale: runs last in the failure-detection chain
	// because it is a cross-cutting signal (provider-side, not
	// agent-side) and should not preempt the agent-level
	// classes above. A provider_incident group does not preclude
	// other groupings on the same execution; the dashboard
	// surfaces all of them.
	if isTerminalStatus(patch.Status) {
		llmPayloads, err := h.Store.ListLLMCallPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list llm_call payloads for provider-incident detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(llmPayloads) > 0 {
			// Extract distinct (provider, error_class) pairs
			// emitted by THIS execution. We only check the cross-
			// tenant count for pairs we already saw locally; that
			// keeps the query budget linear in this execution's
			// own provider-error footprint, not in the project's
			// total provider diversity.
			seen := map[[2]string]struct{}{}
			for _, raw := range llmPayloads {
				var p struct {
					Provider   string `json:"provider"`
					ErrorClass string `json:"error_class"`
				}
				if jErr := json.Unmarshal(raw, &p); jErr != nil {
					continue
				}
				if p.Provider == "" || p.ErrorClass == "" {
					continue
				}
				// Drop customer-side error classes
				// (invalid_api_key, client_error, unknown) from
				// provider_incident aggregation. They get stored
				// on the llm_call event for observability but a
				// rate of bad-key errors across tenants is not a
				// provider outage — it's customer key rotation
				// failures. The PROVIDER_SIDE_ERROR_CLASSES set
				// matches what the SDK ships in mesedi/errors.py
				// and src/errors.ts.
				if !detectors.IsProviderSideErrorClass(p.ErrorClass) {
					continue
				}
				seen[[2]string{p.Provider, p.ErrorClass}] = struct{}{}
			}
			// Per-project threshold (#270/#271 migration 040).
			// Default 2 matches the historical hardcoded constant;
			// single-tenant customers set this to 1 in their
			// project settings so any provider error from a single
			// agent fires the detector.
			threshold, tErr := h.Store.GetProjectProviderIncidentMinTenants(
				r.Context(), authProjectID,
			)
			if tErr != nil {
				h.Logger.Warn("get provider_incident_min_tenants failed; using default",
					"execution_id", executionID,
					"project_id", authProjectID,
					"error", tErr.Error(),
				)
				threshold = detectors.MinTenantsForProviderIncident
				// #276.d: durable telemetry — surfaces in dashboard.
				h.recordAuditEventForProject(
					r.Context(),
					authProjectID, "system",
					"config_fallback", "project_config", "provider_incident_min_tenants",
					map[string]any{
						"error":          tErr.Error(),
						"fallback_value": threshold,
					},
				)
			}
			// Look back 15 minutes — long enough to span a
			// rolling provider blip, short enough to keep the
			// signal current. The constant is intentionally not
			// configurable at v1; a future iteration can promote
			// it to a per-project setting.
			since := time.Now().Add(-15 * time.Minute)
			for pair := range seen {
				provider, errClass := pair[0], pair[1]
				count, cErr := h.Store.CountDistinctTenantsWithProviderError(
					r.Context(), authProjectID, provider, errClass, since,
				)
				if cErr != nil {
					h.Logger.Warn("count distinct tenants with provider error failed",
						"execution_id", executionID,
						"provider", provider,
						"error_class", errClass,
						"error", cErr.Error(),
					)
					continue
				}
				if sig, fired := detectors.DetectProviderIncident(
					provider, errClass, count, threshold,
				); fired {
					isNew, gErr := h.Store.GroupProviderIncident(r.Context(), executionID, authProjectID, sig)
					if gErr != nil {
						h.Logger.Warn("provider-incident grouping failed (continuing)",
							"execution_id", executionID,
							"signature", sig,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassProviderIncident, sig, isNew, gErr)
				}
			}
		}
	}

	// Mesedi #20 — hitl_timeout detector. Aggregates the
	// human_intervention events (#19) on this execution and fires
	// when the customer's HITL SLA was breached. Two firing
	// conditions: response_kind=="timeout" (explicit) OR
	// wait_duration_ms > sla_seconds*1000 (sla_exceeded). Runs
	// after provider_incident because HITL signals are scoped to
	// THIS run; provider_incident is the cross-tenant signal
	// that subsumes per-run causes when present.
	if isTerminalStatus(patch.Status) {
		hiPayloads, err := h.Store.ListHumanInterventionPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list human_intervention payloads for hitl_timeout detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(hiPayloads) > 0 {
			raw := make([]json.RawMessage, len(hiPayloads))
			for i, p := range hiPayloads {
				raw[i] = json.RawMessage(p)
			}
			if sig, fired := detectors.DetectHITLTimeout(raw); fired {
				isNew, gErr := h.Store.GroupHITLTimeout(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("hitl-timeout grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassHITLTimeout, sig, isNew, gErr)
			}

			// Mesedi #21 — hitl_rejection_spike detector. Cross-
			// execution signal: if a high fraction of recent HITL
			// runs in this project came back rejected or edited,
			// the agent's behavior likely regressed. We only run
			// this when THIS execution itself recorded at least
			// one human_intervention event (no point asking
			// "did rejections spike" if we have no fresh HITL
			// data anyway). The 1-hour window matches the
			// provider_incident posture: recent enough to be
			// actionable, wide enough to accumulate a meaningful
			// sample.
			since := time.Now().Add(-1 * time.Hour)
			counts, sErr := h.Store.CountHITLOutcomesInWindow(r.Context(), authProjectID, since)
			if sErr != nil {
				h.Logger.Warn("count hitl outcomes in window failed",
					"execution_id", executionID,
					"error", sErr.Error(),
				)
			} else if sig, fired := detectors.DetectHITLRejectionSpike(
				counts,
				detectors.MinHITLSampleForRejectionSpike,
				detectors.RejectionSpikeRateBp,
				detectors.EditSpikeRateBp,
			); fired {
				isNew, gErr := h.Store.GroupHITLRejectionSpike(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("hitl-rejection-spike grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassHITLRejectionSpike, sig, isNew, gErr)
			}
		}
	}

	// Phase 3b sub-slices 12 + 15: events-driven post-processing. Both
	// cost computation and prompt-injection detection walk the same
	// event list, so fetch ONCE and feed both. Best-effort throughout ,
	// failures here never fail the PATCH.
	if isTerminalStatus(patch.Status) {
		evts, err := h.Store.ListEventsForExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list events for post-PATCH processing failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else {
			// Sub-slice 12: cost computation. Wave 0.3 — backend is now
			// the source of truth for known models. The SDK-shipped
			// per-event estimated_cost_usd is fallback only for models
			// missing from pricing.priceTable. This means new model
			// pricing or pricing changes ship with a backend deploy
			// without waiting for an SDK release.
			//
			// Always walks events and recomputes; the patch body's
			// EstimatedCostUSD is kept as a defensive last-resort
			// fallback when the walk produced no value (e.g. tool-only
			// executions with no llm_call events). When the walk
			// produces a value, it overrides whatever the SDK rolled
			// up — that's the inversion.
			//
			// Unknown models surface via an audit_event (one per
			// execution, not per event) so the dashboard's existing
			// config-fallback chip can show "Mesedi doesn't know how
			// to price model X — using SDK fallback" tile.
			cost, unknownModels := computeExecutionCost(evts)
			effectiveCost := cost
			if effectiveCost == 0 {
				effectiveCost = patch.EstimatedCostUSD
			}
			if effectiveCost > 0 {
				if err := h.Store.SetExecutionCost(r.Context(), executionID, effectiveCost); err != nil {
					h.Logger.Warn("set execution cost failed",
						"execution_id", executionID,
						"computed_cost_usd", effectiveCost,
						"backend_cost_usd", cost,
						"sdk_rollup_cost_usd", patch.EstimatedCostUSD,
						"error", err.Error(),
					)
				} else {
					h.Logger.Info("execution cost computed",
						"execution_id", executionID,
						"cost_usd", effectiveCost,
						"backend_cost_usd", cost,
						"unknown_model_count", len(unknownModels),
					)
				}
			}
			if len(unknownModels) > 0 {
				h.recordAuditEventForProject(
					r.Context(),
					authProjectID, "system",
					"pricing_unknown_model", "execution", executionID,
					map[string]any{
						"models":         unknownModels,
						"pricing_table_version": pricing.PricingTableVersion,
					},
				)
			}

			// Sub-slice 17: identical-call loop detector. Hashes
			// (model + user_message) per llm_call; if the same hash
			// appears 3+ times in one execution, group as
			// loops/identical_call. Runs BEFORE the injection check
			// because a runaway loop generating the same prompt
			// repeatedly is a more urgent resource-waste signal than
			// a single injection attempt embedded in the same prompt.
			if callHash, found := scanForIdenticalCalls(evts, 3); found {
				isNew, gErr := h.Store.GroupIdenticalCallLoop(r.Context(), executionID, authProjectID, callHash)
				if gErr != nil {
					h.Logger.Warn("identical-call grouping failed (continuing)",
						"execution_id", executionID,
						"call_hash", callHash,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, "identical_call_"+callHash, isNew, gErr)
			}

			// Sub-slice 81: similar-call loop detector. Catches the
			// "stuck-loop with paraphrased prompts" pattern that
			// identical_call misses, different exact text, same
			// semantic intent, ≥3 near-duplicates within one
			// execution. Runs AFTER identical_call so exact-text
			// loops win the more-specific signature; only loops with
			// varied wording reach this code path. Uses the same
			// trigram substrate as drift v2.
			similarMsgs := extractLLMUserMessages(evts)
			if len(similarMsgs) >= detectors.SimilarCallMinClusterSize {
				if callHash, found := detectors.DetectSimilarCallLoop(similarMsgs); found {
					isNew, gErr := h.Store.GroupSimilarCallLoop(r.Context(), executionID, authProjectID, callHash)
					if gErr != nil {
						h.Logger.Warn("similar-call grouping failed (continuing)",
							"execution_id", executionID,
							"call_hash", callHash,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, "similar_call_"+callHash, isNew, gErr)
				}
			}

			// Sub-slice 15: prompt-injection detection. Scan each
			// llm_call event's user_message + system_prompt for known
			// injection patterns. First match wins; the pattern name
			// becomes the failure_group signature so all executions
			// hitting the same attack pattern cluster together.
			//
			// PRIORITY NOTE: injection runs BEFORE cost-velocity (just
			// below) because a prompt-injection is a security event ,
			// "this execution was attacked" is a more important
			// classification than "this execution was expensive."
			// The failure_group_id idempotency short-circuit means an
			// injection-classified execution skips cost-velocity even
			// if it would otherwise have matched.
			if pattern, found := scanForInjection(evts); found {
				isNew, gErr := h.Store.GroupPromptInjection(r.Context(), executionID, authProjectID, pattern)
				if gErr != nil {
					h.Logger.Warn("prompt-injection grouping failed (continuing)",
						"execution_id", executionID,
						"pattern", pattern,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassInjection, pattern, isNew, gErr)
			}

			// Sub-slice 16: cost-velocity detector. Any execution whose
			// resolved cost exceeds the per-project threshold gets
			// grouped as cost_velocity with a cost-bucketed signature.
			//
			// Migration 043: threshold is per-project. Default 1.00 USD
			// (raised from the broken $0.001 v0.0.1 floor that fired
			// on every real execution). Cost-sensitive customers can
			// lower it (e.g. $0.10); batch customers can raise it.
			// Falls back to the package default on store error so a
			// transient DB blip never silences the detector, with a
			// durable audit_event so persistent failures surface in
			// the dashboard config-fallback chip.
			//
			// Migration 044 (Wave 0.2) adds the rate-based ($/min)
			// detector immediately after this block. Both can fire on
			// the same execution because they answer different
			// questions (single expensive call vs sustained burn
			// rate); idempotent failure_group writes ensure no
			// double-counting under the absolute signature.
			if effectiveCost > 0 {
				costThresholdUSD := store.DefaultCostVelocityThresholdUSD
				if cvUSD, cvErr := h.Store.GetProjectCostVelocityThresholdUSD(r.Context(), authProjectID); cvErr == nil {
					costThresholdUSD = cvUSD
				} else {
					h.Logger.Warn("get cost_velocity_threshold_usd failed; using default",
						"execution_id", executionID,
						"project_id", authProjectID,
						"error", cvErr.Error(),
					)
					h.recordAuditEventForProject(
						r.Context(),
						authProjectID, "system",
						"config_fallback", "project_config", "cost_velocity_threshold_usd",
						map[string]any{
							"error":          cvErr.Error(),
							"fallback_value": costThresholdUSD,
						},
					)
				}
				if effectiveCost >= costThresholdUSD {
					isNew, gErr := h.Store.GroupCostVelocity(r.Context(), executionID, authProjectID, effectiveCost)
					if gErr != nil {
						h.Logger.Warn("cost-velocity grouping failed (continuing)",
							"execution_id", executionID,
							"cost_usd", effectiveCost,
							"threshold_usd", costThresholdUSD,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassCostVelocity, store.CostVelocitySignature(effectiveCost), isNew, gErr)
				}
			}

			// Sub-slice 16b: cost-velocity RATE detector. Sums execution
			// costs over a per-project rolling window and fires when
			// the burn-rate ($/minute) exceeds the per-project threshold.
			// Closes the marketing-vs-implementation gap from the audit
			// (cost_velocity.G2): marketing promised "$/minute rate
			// detection" but only per-execution magnitude existed.
			//
			// Migration 044: threshold + window are per-project. Defaults
			// {5.00 USD/min, 5 min}. Same fallback-to-default-with-audit
			// pattern as the absolute block above. Independent of the
			// absolute detector — both can fire on the same execution.
			//
			// Aggregator reuses SumExecutionCostByProjectSince — it
			// already exists (org-rollup endpoint), so no new store
			// API surface and no new index requirements (the existing
			// (project_id, started_at) covers the scan).
			rateCfg := store.DefaultCostVelocityRateConfig
			if rc, rcErr := h.Store.GetProjectCostVelocityRateConfig(r.Context(), authProjectID); rcErr == nil {
				rateCfg = rc
			} else {
				h.Logger.Warn("get cost_velocity_rate_config failed; using default",
					"execution_id", executionID,
					"project_id", authProjectID,
					"error", rcErr.Error(),
				)
				h.recordAuditEventForProject(
					r.Context(),
					authProjectID, "system",
					"config_fallback", "project_config", "cost_velocity_rate_config",
					map[string]any{
						"error":                 rcErr.Error(),
						"fallback_threshold":    rateCfg.ThresholdUSDPerMin,
						"fallback_window_mins":  rateCfg.WindowMinutes,
					},
				)
			}
			windowStart := time.Now().UTC().Add(-time.Duration(rateCfg.WindowMinutes) * time.Minute)
			windowCostUSD, _, rcAggErr := h.Store.SumExecutionCostByProjectSince(r.Context(), authProjectID, windowStart)
			if rcAggErr != nil {
				// Aggregator failures must NOT break the request path.
				// Log + audit-telemetry + skip the rate fire for this
				// execution. The absolute detector above has already
				// run; rate is additive signal, not the only one.
				h.Logger.Warn("cost-velocity rate aggregator failed (continuing)",
					"execution_id", executionID,
					"project_id", authProjectID,
					"window_minutes", rateCfg.WindowMinutes,
					"error", rcAggErr.Error(),
				)
				h.recordAuditEventForProject(
					r.Context(),
					authProjectID, "system",
					"config_fallback", "project_config", "cost_velocity_rate_aggregator",
					map[string]any{
						"error": rcAggErr.Error(),
					},
				)
			} else if rateCfg.WindowMinutes > 0 {
				ratePerMin := windowCostUSD / float64(rateCfg.WindowMinutes)
				if ratePerMin >= rateCfg.ThresholdUSDPerMin {
					isNew, gErr := h.Store.GroupCostVelocityRate(r.Context(), executionID, authProjectID, ratePerMin)
					if gErr != nil {
						h.Logger.Warn("cost-velocity rate grouping failed (continuing)",
							"execution_id", executionID,
							"rate_usd_per_min", ratePerMin,
							"threshold_usd_per_min", rateCfg.ThresholdUSDPerMin,
							"window_minutes", rateCfg.WindowMinutes,
							"error", gErr.Error(),
						)
					}
					// Webhook fires under the rate signature
					// distinctly from the absolute one so SREs can
					// route them differently if they choose.
					h.maybeFireWebhook(r, authProjectID, store.FailureClassCostVelocity, store.CostVelocityRateSignature(ratePerMin), isNew, gErr)
				}
			}

			// Time-budget detector. Catch-all for executions that ran
			// long without a more specific cause. MOVED HERE from the
			// top of the chain (where it ran at line 737 with a 1s
			// threshold) because the original placement greedy-claimed
			// every execution > 1s and the failure_group_id idempotency
			// short-circuit then silently suppressed ~7 more specific
			// detectors (token_waste, semantic_loop, tool_schema_drift,
			// data_leakage, cascading_failure, prompt_injection,
			// cost_velocity). Real customer agents that take >1s of
			// wall-clock are normal — they should be classified by the
			// specific failure they experienced, not by a generic
			// "slow" bucket. Time-budget is now the second-to-last
			// detector in the chain, falling through only when no
			// specific detector claimed the execution.
			//
			// Threshold raised from 1s (v0.0.1 demo placeholder) to
			// 60s — the production default the original placeholder
			// comment intended. Real "agent stuck running" alerts
			// belong in the minute+ range, not the second range.
			//
			// Mesedi #18: subtract accumulated paused time so a HITL
			// wait does not falsely trip the time budget.
			// effectiveDurationMs reflects the agent's actual working
			// time (wall-clock minus all paused intervals).
			//
			// Mesedi #276: threshold is now per-project (migration
			// 041). Default 60_000 ms matches the historical
			// hardcoded constant; a chat-agent project can lower it
			// to 30_000, a research-agent project can raise it to
			// 300_000. Falls back to the default on store error so a
			// transient DB blip never silences the detector.
			thresholdMs := 60_000
			if tbMs, tbErr := h.Store.GetProjectTimeBudgetMs(r.Context(), authProjectID); tbErr == nil {
				thresholdMs = tbMs
			} else {
				h.Logger.Warn("get time_budget_ms failed; using default",
					"execution_id", executionID,
					"project_id", authProjectID,
					"error", tbErr.Error(),
				)
				// #276.d: durable telemetry so a silent column-drop
				// or persistent DB issue surfaces in the dashboard
				// instead of only the Warn log.
				h.recordAuditEventForProject(
					r.Context(),
					authProjectID, "system",
					"config_fallback", "project_config", "time_budget_ms",
					map[string]any{
						"error":          tbErr.Error(),
						"fallback_value": thresholdMs,
					},
				)
			}
			if isTerminalStatus(patch.Status) && effectiveDurationMs >= int64(thresholdMs) {
				isNew, err := h.Store.GroupTimeBudgetExceedance(r.Context(), executionID, authProjectID, effectiveDurationMs)
				if err != nil {
					h.Logger.Warn("time-budget grouping failed (continuing)",
						"execution_id", executionID,
						"effective_duration_ms", effectiveDurationMs,
						"wall_clock_duration_ms", patch.DurationMs,
						"total_paused_ms", totalPausedMs,
						"error", err.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, store.TimeBudgetSignature(effectiveDurationMs), isNew, err)
			}

			// Drift v2, lexical signal. Char-3-gram cosine distance
			// between current execution's user_messages and the
			// project's recent history. Runs LAST in the chain on
			// purpose: lexical drift is a SOFT behavioral signal that
			// should only surface for executions nothing else
			// classified. The idempotency short-circuit in
			// GroupDriftSignal means any execution already grouped
			// (crashes, loops, tool_failures, validator_failures,
			// prompt_injection, cost_velocity, or model-drift) skips
			// drift v2, which is the right priority: specific causal
			// classifications beat the "prompts have shifted" pattern.
			//
			// The signal still gets logged when computed but
			// not-grouped, so dashboards / detectors can be tuned
			// against real data later without changing the order.
			driftCutoff := time.Now().Add(-7 * 24 * time.Hour)
			currentMsgs, cErr := h.Store.ListLLMUserMessagesForExecution(r.Context(), executionID)
			if cErr != nil {
				h.Logger.Warn("drift: list user_messages for execution failed (skipping lexical-drift)",
					"execution_id", executionID,
					"error", cErr.Error(),
				)
			} else if len(currentMsgs) > 0 {
				historicalMsgs, hErr := h.Store.ListLLMUserMessagesForProjectSince(r.Context(), authProjectID, driftCutoff, executionID, 500)
				if hErr != nil {
					h.Logger.Warn("drift: list project user_messages failed (skipping lexical-drift)",
						"project_id", authProjectID,
						"error", hErr.Error(),
					)
				} else if len(historicalMsgs) > 0 {
					if signature, distance, drift := detectors.DetectLexicalDrift(currentMsgs, historicalMsgs); drift {
						isNew, dErr := h.Store.GroupDriftSignal(r.Context(), executionID, authProjectID, signature)
						if dErr != nil {
							h.Logger.Warn("lexical drift grouping failed (continuing)",
								"execution_id", executionID,
								"signature", signature,
								"distance", distance,
								"error", dErr.Error(),
							)
						} else {
							h.Logger.Info("lexical drift detected",
								"execution_id", executionID,
								"signature", signature,
								"distance", distance,
								"current_msgs_count", len(currentMsgs),
								"historical_msgs_count", len(historicalMsgs),
							)
						}
						h.maybeFireWebhook(r, authProjectID, store.FailureClassDrift, signature, isNew, dErr)
					}
				}
			}
		}
	}

	h.Logger.Info("execution updated",
		"execution_id", patch.ExecutionID,
		"status", patch.Status,
		"ended_at", patch.EndedAt.Format(time.RFC3339),
		"duration_ms", patch.DurationMs,
		"total_tokens_in", patch.TotalTokensIn,
		"total_tokens_out", patch.TotalTokensOut,
		"estimated_cost_usd", patch.EstimatedCostUSD,
		"crash_signature", patch.CrashSignature,
	)

	// Mesedi #22 — OpenTelemetry parallel emission. After the
	// terminal-status write + detector chain have both committed,
	// fire-and-forget a goroutine that translates this execution
	// (plus its events) into an OTel trace and ships it via OTLP
	// to the customer-configured collector. Best-effort: emission
	// failures are logged inside the emitter and never surface to
	// the customer. Reads the full event list once, separate from
	// the earlier post-PATCH loop that did per-event cost +
	// injection scanning, because the OTel write should reflect
	// the FINAL execution state (with any post-detector writes
	// already persisted, including failure_group_id set by
	// detectors above).
	if h.OTel.Enabled() && isTerminalStatus(patch.Status) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			finalExec, gErr := h.Store.GetExecution(ctx, executionID)
			if gErr != nil {
				h.Logger.Warn("otel: load execution for emission failed",
					"execution_id", executionID,
					"error", gErr.Error(),
				)
				return
			}
			evts, eErr := h.Store.ListEventsForExecution(ctx, executionID)
			if eErr != nil {
				h.Logger.Warn("otel: load events for emission failed",
					"execution_id", executionID,
					"error", eErr.Error(),
				)
				return
			}
			h.OTel.Emit(ctx, finalExec, evts)
		}()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"execution_id": patch.ExecutionID,
		"status":       patch.Status,
	})
}

// HandleListFailureGroups returns the calling project's failure groups,
// sorted by most-recent first. Supports `limit` (default 50, max 200) and
// `offset` (default 0) via query string.
func (h *Handlers) HandleListFailureGroups(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)

	groups, err := h.Store.ListFailureGroups(r.Context(), authProjectID, limit, offset)
	if err != nil {
		h.Logger.Error("list failure_groups failed",
			"project_id", authProjectID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"failure_groups": groups,
		"count":          len(groups),
		"limit":          limit,
		"offset":         offset,
	})
}

// HandleGetFailureGroup returns a single failure_group by id. Returns 404
// both when the group doesn't exist AND when the group belongs to a
// different project than the caller (don't leak group_id existence
// across tenants).
func (h *Handlers) HandleGetFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		h.Logger.Error("get failure_group failed",
			"group_id", groupID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if group.ProjectID != authProjectID {
		// Don't reveal that the group exists in another project, return
		// 404 same as a non-existent group.
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	writeJSON(w, http.StatusOK, group)
}

// HandleAnalyzeFailureGroup runs the LLM-assisted root-cause
// analyzer over a failure_group and persists the resulting Markdown
// on the row (Mesedi #27). Subsequent reads of the group surface
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
func (h *Handlers) HandleAnalyzeFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	if h.Anthropic == nil || !h.Anthropic.Enabled() {
		writeError(w, http.StatusServiceUnavailable,
			"AI analysis is not configured for this deployment (ANTHROPIC_API_KEY unset)")
		return
	}

	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if group.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	// Cache hit: return the existing analysis if it was generated
	// within the last 24 hours AND no new affected executions have
	// landed since (last_seen <= analyzed_at).
	if group.AnalyzedAt != nil && group.AnalysisMarkdown != nil {
		fresh := time.Since(*group.AnalyzedAt) < 24*time.Hour
		stable := !group.LastSeen.After(*group.AnalyzedAt)
		// The query param ?regenerate=1 forces a re-analysis even
		// when the cache is warm. Lets the customer ask for a
		// fresh take after deploying a fix.
		forced := r.URL.Query().Get("regenerate") == "1"
		if fresh && stable && !forced {
			writeJSON(w, http.StatusOK, group)
			return
		}
	}

	// Tier gate + per-period rate limit (added pre-#30 to protect
	// Verdifax LLC's Anthropic bill from being subsidized by users
	// without payment on file and from a Team customer with thousands
	// of distinct failure groups generating unbounded LLM calls).
	//
	// Order:
	//   1. Load project to get tier + period bounds.
	//   2. Hobby (#206 pay-per-use, $0.75 each, 50 / period cap):
	//      a. No Stripe card on file → refuse with 402; analyses
	//         are billed at period end and cannot be charged to a
	//         missing card. Dashboard surfaces "add a payment
	//         method on /app/billing to use AI root-cause."
	//      b. Card present + count >= HobbyAIAnalysisLimit (50)
	//         → refuse with 429 + upgrade nudge to Team.
	//      c. Otherwise allow; scheduler picks up the charge at
	//         period end (HobbyBillingScheduler combines analysis
	//         cost with execution overage in one PaymentIntent).
	//   3. Team: count analyses since current_period_start. If at
	//      or above TeamAIAnalysisLimit, refuse with 429; the
	//      dashboard renders "AI explanation rate limit reached
	//      this period" alongside the raw failure-group row.
	//   4. Enterprise: skip rate limit (contract-defined).
	//
	// Cache hits short-circuited above; only real LLM-calling
	// requests reach this block.
	proj, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"load project for tier check: "+err.Error())
		return
	}
	tier := normalizeTier(proj.Tier)
	if tier == TierHobby {
		if !proj.CardOnFile {
			writeError(w, http.StatusPaymentRequired,
				fmt.Sprintf("AI root-cause analysis on Cloud Hobby is pay-per-use at $%.2f per analysis. Add a payment method on /app/billing to enable it, or upgrade to Cloud Team for %d analyses included.",
					HobbyAIAnalysisPriceUSD, TeamAIAnalysisLimit))
			return
		}
		// Hobby is single-project tier; per-project count is the
		// canonical scope (no tenant fan-out needed here).
		since := time.Now().UTC().AddDate(0, -1, 0)
		if proj.CurrentPeriodStart != nil {
			since = *proj.CurrentPeriodStart
		}
		count, cErr := h.Store.CountAIAnalysesSincePeriodStart(
			r.Context(), authProjectID, since,
		)
		if cErr != nil {
			h.Logger.Warn("analyze: hobby count check failed",
				"project_id", authProjectID, "error", cErr.Error())
			// Fail open on the rate-limit query itself. A DB blip
			// should not block a legitimate analysis request.
		} else if count >= HobbyAIAnalysisLimit {
			h.Logger.Info("analyze: hobby rate limit reached",
				"project_id", authProjectID,
				"count", count, "limit", HobbyAIAnalysisLimit)
			writeError(w, http.StatusTooManyRequests,
				fmt.Sprintf("You have used %d of %d AI root-cause analyses this period on Cloud Hobby ($%.2f each, capped at %d to prevent surprise bills). Upgrade to Cloud Team at /app/billing for %d included per period plus unlimited projects + 90-day retention. Failure detection still works; AI explanations resume next billing period.",
					count, HobbyAIAnalysisLimit, HobbyAIAnalysisPriceUSD,
					HobbyAIAnalysisLimit, TeamAIAnalysisLimit))
			return
		}
	}
	if tier == TierTeam {
		// Use current_period_start as the rate-limit window. If it
		// is nil (race window between Stripe checkout and webhook),
		// fall back to a conservative 30-day rolling window so a
		// brand-new Team customer can still get analyses.
		since := time.Now().UTC().AddDate(0, -1, 0)
		if proj.CurrentPeriodStart != nil {
			since = *proj.CurrentPeriodStart
		}

		// Count analyses across the entire ORGANIZATION (tenant),
		// not just the calling project, because Team allows unlimited
		// projects under one org and a per-project cap would be
		// trivially bypassed by spawning more projects. Fall back to
		// per-project count only when the project has no tenant_id
		// (legacy row that escaped migration-013 backfill).
		tenantID, terr := h.Store.GetProjectTenantID(r.Context(), authProjectID)
		var count int
		var cErr error
		switch {
		case terr == nil && tenantID != nil && *tenantID != "":
			count, cErr = h.Store.CountAIAnalysesByTenantSince(
				r.Context(), *tenantID, since,
			)
		default:
			// tenant_id NULL or lookup failed: fall back to project
			// scope. This leaves an edge-case bypass for legacy
			// unbackfilled projects, accepted because (a) migration
			// 013 already ran on prod, (b) any remaining NULL rows
			// can be one-off backfilled if discovered, and (c)
			// blocking the request entirely would be worse UX than
			// occasionally over-allowing on a rare legacy edge case.
			count, cErr = h.Store.CountAIAnalysesSincePeriodStart(
				r.Context(), authProjectID, since,
			)
		}

		if cErr != nil {
			h.Logger.Warn("analyze: count check failed",
				"project_id", authProjectID, "error", cErr.Error())
			// Fail open on the rate-limit query itself. A DB blip
			// should not block a legitimate analysis request.
		} else if count >= TeamAIAnalysisLimit {
			// #209: Team that removed their card mid-cycle is
			// hard-capped at the included 200 because there's no
			// way to bill the $0.50 overage. The 402 message
			// guides them to either re-add a card or wait for
			// next period.
			if !proj.CardOnFile {
				h.Logger.Info("analyze: team no-card hard cap at included",
					"project_id", authProjectID,
					"count", count, "included", TeamAIAnalysisLimit)
				writeError(w, http.StatusPaymentRequired,
					fmt.Sprintf("You have used your included %d AI root-cause analyses for this period on Cloud Team and your card has been removed. Add a payment method on /app/billing to continue at $%.2f each, or wait for your next billing period.",
						TeamAIAnalysisLimit, TeamAIAnalysisOveragePriceUSD))
				return
			}
			// #208: Team with card past 200 → overage at
			// $0.50/each. Log for observability and continue;
			// invoice.upcoming pushes the InvoiceItem. Customers
			// who want a hard ceiling set BillingCapUSD on
			// /app/billing (same mechanism that protects them on
			// execution overage).
			h.Logger.Info("analyze: team in AI overage",
				"project_id", authProjectID,
				"tenant_id", tenantID,
				"count", count,
				"included", TeamAIAnalysisLimit,
				"overage_units", count-TeamAIAnalysisLimit+1,
				"overage_rate_usd", TeamAIAnalysisOveragePriceUSD)
		}
	}

	// Pull a small, recent sample of affected executions to give
	// the model concrete context. We cap deliberately small
	// (3 executions) so the LLM-side input bill stays bounded.
	sampleExecs, err := h.Store.ListExecutionsByFailureGroup(r.Context(), groupID, 3, 0)
	if err != nil {
		h.Logger.Warn("analyze: sample executions failed",
			"group_id", groupID, "error", err.Error())
	}

	prompt := buildFailureGroupAnalysisPrompt(group, sampleExecs)

	model := "claude-haiku-4-5"
	res, err := h.Anthropic.Call(r.Context(), anthropic.CallOptions{
		Model: model,
		System: "You are Mesedi's senior on-call engineer for AI agent reliability. " +
			"You read structured failure-group telemetry and write a concise root-cause " +
			"analysis with two concrete remediation suggestions. Output is rendered as " +
			"Markdown in a dashboard card. Be precise, opinionated, and honest about " +
			"uncertainty. Never claim a specific fix will resolve the issue; frame " +
			"recommendations as hypotheses the operator should test.",
		User:        prompt,
		MaxTokens:   1024,
		Temperature: 0.2,
	})
	if err != nil {
		h.Logger.Error("anthropic call failed",
			"group_id", groupID, "error", err.Error())
		writeError(w, http.StatusBadGateway, "AI analysis failed: "+err.Error())
		return
	}

	now := time.Now().UTC()
	if err := h.Store.SaveFailureGroupAnalysis(
		r.Context(), groupID, res.Text, model, now,
	); err != nil {
		h.Logger.Error("save failure_group analysis failed",
			"group_id", groupID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	// #199 record one ai_analyses row per call so the founder
	// accounting view sees actual model + token + cost (not just
	// the flat $0.03 estimate the dashboard quotes customers).
	// Best-effort: log and continue on failure so a transient DB
	// error on this insert never breaks the customer's analysis.
	tenantID := ""
	if group.ProjectID != "" {
		if tp, terr := h.Store.GetProjectTenantID(r.Context(), group.ProjectID); terr == nil && tp != nil {
			tenantID = *tp
		}
	}
	cost := anthropic.ComputeCostUSD(model, res.InputTokens, res.OutputTokens)
	if err := h.Store.CreateAIAnalysis(r.Context(), &store.AIAnalysis{
		AnalysisID:       newAIAnalysisID(),
		FailureGroupID:   groupID,
		ProjectID:        group.ProjectID,
		TenantID:         tenantID,
		ModelID:          model,
		InputTokens:      res.InputTokens,
		OutputTokens:     res.OutputTokens,
		CostUSD:          cost,
		GeneratedAt:      now,
		AnalysisMarkdown: res.Text,
	}); err != nil {
		h.Logger.Warn("record ai_analyses row failed (accounting only)",
			"group_id", groupID,
			"input_tokens", res.InputTokens,
			"output_tokens", res.OutputTokens,
			"error", err.Error())
	}

	// Re-read so the response matches what a subsequent GET would
	// return (with the freshly-persisted analysis populated).
	refreshed, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, refreshed)
}

// newAIAnalysisID returns a stable "ai_<32 hex>" identifier. Uses
// the same random-id pattern as the audit-event id helper so the
// PK shape is consistent across Mesedi tables.
func newAIAnalysisID() string {
	return "ai_" + newAuditEventID()[len("audit_"):]
}

// buildFailureGroupAnalysisPrompt assembles the structured prompt
// sent to the LLM. Kept small so the input bill stays bounded and
// the model's output stays focused.
func buildFailureGroupAnalysisPrompt(
	group *store.FailureGroup,
	sampleExecs []*events.Execution,
) string {
	var sb strings.Builder
	sb.WriteString("# Failure group context\n\n")
	sb.WriteString(fmt.Sprintf("- **failure_class**: %s\n", group.FailureClass))
	sb.WriteString(fmt.Sprintf("- **signature**: %s\n", group.Signature))
	sb.WriteString(fmt.Sprintf("- **first_seen**: %s\n", group.FirstSeen.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("- **last_seen**: %s\n", group.LastSeen.Format(time.RFC3339)))
	sb.WriteString(fmt.Sprintf("- **affected_executions**: %d\n", group.AffectedExecutions))
	sb.WriteString(fmt.Sprintf("- **event_count**: %d\n", group.EventCount))
	if group.CostWastedUSD != nil {
		sb.WriteString(fmt.Sprintf("- **estimated_cost_usd**: %.4f\n", *group.CostWastedUSD))
	}
	if group.SampleExecutionID != "" {
		sb.WriteString(fmt.Sprintf("- **sample_execution_id**: %s\n", group.SampleExecutionID))
	}
	if len(sampleExecs) > 0 {
		sb.WriteString("\n## Sample executions (most recent)\n\n")
		for _, exec := range sampleExecs {
			if exec == nil {
				continue
			}
			sb.WriteString(fmt.Sprintf("### %s\n\n", exec.ExecutionID))
			sb.WriteString(fmt.Sprintf("- status: %s\n", exec.Status))
			sb.WriteString(fmt.Sprintf("- duration_ms: %d\n", exec.DurationMs))
			if exec.CrashSignature != "" {
				sb.WriteString(fmt.Sprintf("- crash_signature: %s\n", exec.CrashSignature))
			}
			if exec.SDKLanguage != "" {
				sb.WriteString(fmt.Sprintf("- sdk_language: %s\n", exec.SDKLanguage))
			}
			sb.WriteString("\n")
		}
	}
	sb.WriteString("\n## Task\n\n")
	sb.WriteString("Write a short Markdown analysis with three sections:\n")
	sb.WriteString("1. **Likely cause** — one paragraph naming the most plausible root cause given the signature and the sample executions.\n")
	sb.WriteString("2. **Two remediation hypotheses** — two bullet items, each a concrete action the operator can test.\n")
	sb.WriteString("3. **Confidence** — one line: how confident you are (low / medium / high) and the single biggest unknown.\n")
	sb.WriteString("\nKeep the entire output under 250 words. Do not include a disclaimer about the analysis being non-authoritative; the dashboard already renders one.\n")
	return sb.String()
}

// MaxEventPayloadBytes caps the JSON-serialized byte count of a
// single event's Payload field at the ingest boundary (#243).
// Defense-in-depth against SDKs that did not (or could not) apply
// the same cap client-side: pre-v0.4 SDKs, direct curl/custom HTTP
// integrations, and the rare case of a customer deliberately
// disabling SDK-side truncation. Matches the SDK default
// `DEFAULT_MAX_PAYLOAD_BYTES`; the SDK should always reach this
// before the backend does, but the backend still enforces so a
// single oversized event from any source cannot bloat the
// executions / events tables.
//
// Per-event, not per-batch: a legitimate 100-event batch with
// 32 KB payloads each totals ~3.2 MB on the wire, which is fine.
const MaxEventPayloadBytes = 32 * 1024

// payloadOverCap returns true iff the event's serialized payload
// exceeds the backend's hard cap. Extracted as a tiny named helper so
// the unit test can drive it without standing up the full ingest
// handler.
func payloadOverCap(evt *events.Event) bool {
	return len(evt.Payload) > MaxEventPayloadBytes
}

// HandleIngestEvents accepts a batch of Events. Batching is required ,
// the SDK buffers events client-side and flushes in groups of ~100, so
// the ingest path is array-shaped from day one. A single-event POST is
// accepted as a 1-element array; rejecting non-array bodies catches
// SDK bugs early.
func (h *Handlers) HandleIngestEvents(w http.ResponseWriter, r *http.Request) {
	var batch []events.Event
	if err := decodeJSON(r, &batch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(batch) == 0 {
		writeError(w, http.StatusBadRequest, "empty event batch")
		return
	}

	// First pass: validate and defaulting. Reject malformed events
	// individually so a single bad event in a batch doesn't poison the
	// whole transaction.
	accepted := make([]events.Event, 0, len(batch))
	rejected := 0
	for i := range batch {
		evt := &batch[i]
		if evt.EventID == "" || evt.ExecutionID == "" || evt.EventType == "" {
			rejected++
			h.Logger.Warn("event rejected: required field missing",
				"event_index", i,
				"event_id", evt.EventID,
				"execution_id", evt.ExecutionID,
				"event_type", evt.EventType,
			)
			continue
		}
		// #243 per-event size cap. SDK-side truncation should keep
		// every payload well under this floor; reaching it implies
		// an older SDK or a custom integration that bypassed the
		// truncation helper. We reject the individual event and
		// continue so a single oversized event in a batch does not
		// poison the rest of the batch.
		if payloadOverCap(evt) {
			rejected++
			h.Logger.Warn("event rejected: payload exceeds cap",
				"event_index", i,
				"event_id", evt.EventID,
				"execution_id", evt.ExecutionID,
				"event_type", evt.EventType,
				"payload_bytes", len(evt.Payload),
				"max_bytes", MaxEventPayloadBytes,
			)
			continue
		}
		if evt.Timestamp.IsZero() {
			evt.Timestamp = time.Now().UTC()
		}
		accepted = append(accepted, *evt)
	}

	// Mesedi #1 + #24: scan + redact outbound LLM / tool payloads
	// against the DLP rule registry before persistence. Matched
	// secrets are replaced with `[REDACTED:rule_id]` tokens; high /
	// critical hits also generate a sibling dlp_scan_result event
	// that the data_leakage detector consumes downstream. Nil
	// scanner (local dev) leaves the batch unchanged.
	accepted = h.applyDLPToBatch(accepted)

	if err := h.Store.SaveEvents(r.Context(), accepted); err != nil {
		h.Logger.Error("save events failed", "error", err.Error(), "batch_size", len(accepted))
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	for _, evt := range accepted {
		h.Logger.Info("event ingested",
			"event_id", evt.EventID,
			"execution_id", evt.ExecutionID,
			"event_type", evt.EventType,
			"sequence", evt.Sequence,
			"duration_ms", evt.DurationMs,
			"payload_bytes", len(evt.Payload),
		)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accepted": len(accepted),
		"rejected": rejected,
	})
}

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

// HandleListExecutions returns the calling project's executions, sorted
// by started_at DESC. Supports `limit` (default 50, max 200) and `offset`
// (default 0).
func (h *Handlers) HandleListExecutions(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)

	execs, err := h.Store.ListExecutions(r.Context(), authProjectID, limit, offset)
	if err != nil {
		h.Logger.Error("list executions failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"executions": execs,
		"count":      len(execs),
		"limit":      limit,
		"offset":     offset,
	})
}

// HandleGetExecution returns a single execution + its events (sorted by
// sequence ASC). Cross-tenant access returns 404 to avoid leaking which
// execution IDs exist on other projects.
func (h *Handlers) HandleGetExecution(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	exec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exec.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}

	evts, err := h.Store.ListEventsForExecution(r.Context(), executionID)
	if err != nil {
		h.Logger.Warn("list events failed (returning execution without events)",
			"execution_id", executionID,
			"error", err.Error(),
		)
		evts = nil
	}

	// Server-side token aggregation: if the SDK didn't explicitly PATCH
	// total_tokens_in / total_tokens_out on the terminal-status update,
	// derive them from the llm_call event payloads. Lets adapters
	// (LangChain, CrewAI, etc.) and bare emit_llm_call() callers show
	// accurate execution-level totals without requiring every SDK to
	// thread a running counter into the @wrap exit path.
	//
	// SDK-supplied values win when present (non-zero), a future SDK
	// slice can authoritatively report totals (e.g. accumulating
	// across streaming chunks the event payloads can't see) and the
	// dashboard will trust that report.
	if exec.TotalTokensIn == 0 && exec.TotalTokensOut == 0 {
		var sumIn, sumOut int
		for _, e := range evts {
			if e == nil || e.EventType != events.EventTypeLLMCall {
				continue
			}
			if e.Payload == nil {
				continue
			}
			// Payload is a json.RawMessage; cheaply parse just the
			// two numeric fields we need.
			var p struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				sumIn += p.InputTokens
				sumOut += p.OutputTokens
			}
		}
		exec.TotalTokensIn = sumIn
		exec.TotalTokensOut = sumOut
	}

	// If this execution was clustered into a failure_group by the
	// detection pipeline, also fetch the group so the dashboard can
	// render a "Flagged by [class] / [signature]" banner with the
	// underlying reason + a deep-link back to the group's detail page.
	// Failure to load the group is non-fatal (the page still renders;
	// the banner just doesn't), so any store error is logged and the
	// response continues without the failure_group field.
	var failureGroup *store.FailureGroup
	if exec.FailureGroupID != nil && *exec.FailureGroupID != "" {
		fg, err := h.Store.GetFailureGroup(r.Context(), *exec.FailureGroupID)
		if err != nil {
			h.Logger.Warn("get failure_group failed (rendering execution without banner)",
				"execution_id", executionID,
				"group_id", *exec.FailureGroupID,
				"error", err.Error(),
			)
		} else {
			failureGroup = fg
		}
	}

	resp := map[string]any{
		"ok":        true,
		"execution": exec,
		"events":    evts,
	}
	if failureGroup != nil {
		resp["failure_group"] = failureGroup
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleGetExecutionTopology returns the parent + child tree of the
// supplied execution within the calling project (Mesedi #10). The
// response is a flat list of TopologyNode, ordered by depth ASC then
// started_at ASC, so the dashboard can render the tree without
// re-sorting. Cross-project edges are silently dropped at query
// time; an execution that the caller is not authorized to see does
// not appear in the response (and an attempt to seed the topology
// at a foreign execution_id returns an empty array, matching the
// 404-on-cross-tenant policy used by HandleGetExecution).
//
// Query param ?depth=N caps traversal in both directions (default
// 8, max 32). The cap defends against pathological parent chains
// and bounds the response size.
func (h *Handlers) HandleGetExecutionTopology(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	depth := parseIntQuery(r, "depth", 8, 1, 32)
	nodes, err := h.Store.GetExecutionTopology(r.Context(), authProjectID, executionID, depth)
	if err != nil {
		h.Logger.Error("get execution topology failed",
			"execution_id", executionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "topology query failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"nodes":      nodes,
		"count":      len(nodes),
		"depth":      depth,
		"seed_id":    executionID,
		"project_id": authProjectID,
	})
}

// HandleListExecutionsInFailureGroup returns the executions that belong
// to a given failure_group. Verifies cross-tenant access by first
// fetching the group and confirming group.project_id == auth project ,
// 404 if it doesn't match (don't leak group_id existence).
func (h *Handlers) HandleListExecutionsInFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	// Authorization: verify the group belongs to the caller's project.
	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if group.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)

	execs, err := h.Store.ListExecutionsByFailureGroup(r.Context(), groupID, limit, offset)
	if err != nil {
		h.Logger.Error("list executions by failure_group failed",
			"group_id", groupID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"executions": execs,
		"count":      len(execs),
		"limit":      limit,
		"offset":     offset,
		"group_id":   groupID,
	})
}

// HandleReportCostByTenant returns the calling project's cost
// aggregated by tenant_id over a time window. Query parameters:
//
//   - since   RFC3339 lower bound on executions.started_at (optional)
//   - until   RFC3339 upper bound on executions.started_at (optional)
//   - limit   max rows returned, 1-200, default 25
//
// Executions with NULL tenant_id collapse into a single row with
// tenant_id="" so the dashboard renders "unattributed" cost
// separately rather than silently dropping it. Rows are ordered by
// total cost descending.
//
// Mesedi #5: bridges the gap between project-scoped cost_velocity
// alerts ("something is expensive") and the SaaS host's customer
// granularity ("Customer X drove $Y of that spend"). The host
// application sets tenant_id on POST /executions; this endpoint reads
// it back.
func (h *Handlers) HandleReportCostByTenant(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	since, err := parseRFC3339Query(r, "since")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid since: "+err.Error())
		return
	}
	until, err := parseRFC3339Query(r, "until")
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid until: "+err.Error())
		return
	}
	limit := parseIntQuery(r, "limit", 25, 1, 200)

	rows, err := h.Store.GetCostByTenant(r.Context(), authProjectID, since, until, limit)
	if err != nil {
		h.Logger.Error("get cost by tenant failed",
			"project_id", authProjectID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "report failed: "+err.Error())
		return
	}

	resp := map[string]any{
		"ok":    true,
		"rows":  rows,
		"count": len(rows),
		"limit": limit,
	}
	if !since.IsZero() {
		resp["since"] = since.UTC().Format(time.RFC3339)
	}
	if !until.IsZero() {
		resp["until"] = until.UTC().Format(time.RFC3339)
	}
	writeJSON(w, http.StatusOK, resp)
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

// HandleStats returns top-line stat-card numbers for the dashboard:
// total executions, total failure groups, crashed-in-last-24h,
// average duration_ms across completed executions.
//
// Implementation is deliberately simple: a few small COUNT queries.
// When the executions table grows beyond hand-friendly size, we'll
// either cache these or migrate to time-bucketed aggregates.
func (h *Handlers) HandleStats(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	totalExecutions, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, "", time.Time{})
	if err != nil {
		// Fall back to 0 on individual count failures so the dashboard
		// degrades gracefully rather than 500-ing for one bad query.
		h.Logger.Warn("count total executions failed", "error", err.Error())
	}
	cutoff24h := time.Now().Add(-24 * time.Hour)
	crashed24h, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, string(events.StatusCrashed), cutoff24h)
	if err != nil {
		h.Logger.Warn("count crashed-24h failed", "error", err.Error())
	}
	completedAllTime, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, string(events.StatusCompleted), time.Time{})
	if err != nil {
		h.Logger.Warn("count completed failed", "error", err.Error())
	}

	groups, err := h.Store.ListFailureGroups(ctx, authProjectID, 1000, 0)
	if err != nil {
		h.Logger.Warn("list failure_groups for stats failed", "error", err.Error())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                   true,
		"total_executions":     totalExecutions,
		"completed_executions": completedAllTime,
		"crashed_24h":          crashed24h,
		"open_failure_groups":  len(groups),
	})
}

// HandleOrgRollup returns tenant-wide burn rollup across every project
// owned by the same user as the authenticated project (#259).
//
// v0.1 tenant model: a "tenant" is a single user account
// (owner_user_id). Multi-seat organizations come later. The endpoint
// resolves the authenticated project's owner, lists sibling projects,
// and aggregates SUM(estimated_cost_usd) + COUNT(executions) across
// three buckets: month-to-date, prior month, and last 24h.
//
// Auth: any valid project API key. The endpoint returns ALL projects
// belonging to the same owner_user_id, which is fine because each
// project's API key is already proof of access to that user account.
//
// Response shape (frontend renders KPI cards + per-project table):
//
//	{
//	  "owner_user_id": "...",
//	  "project_count": 3,
//	  "mtd": {"total_cost_usd": 12.34, "total_executions": 1234},
//	  "prior_month": {"total_cost_usd": 9.10, "total_executions": 987},
//	  "last_24h": {"total_cost_usd": 0.42, "total_executions": 42},
//	  "projects": [
//	    {"project_id": "...", "name": "...", "tier": "...",
//	     "mtd_cost_usd": 6.20, "mtd_executions": 600,
//	     "prior_month_cost_usd": 4.50, "prior_month_executions": 450},
//	    ...
//	  ]
//	}
func (h *Handlers) HandleOrgRollup(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	// Resolve owner_user_id from the authenticated project.
	authProject, err := h.Store.GetProject(ctx, authProjectID)
	if err != nil {
		h.Logger.Error("org rollup: get auth project failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load authenticated project")
		return
	}
	if authProject.OwnerUserID == "" {
		// Some legacy projects have no owner; still return a single-
		// project rollup rather than erroring.
		h.Logger.Warn("org rollup: project has no owner_user_id, returning single-project rollup",
			"project_id", authProjectID)
		writeOrgRollup(w, ctx, h, "", []*store.Project{authProject})
		return
	}

	siblings, err := h.Store.ListProjectsByOwner(ctx, authProject.OwnerUserID)
	if err != nil {
		h.Logger.Error("org rollup: list projects by owner failed",
			"owner_user_id", authProject.OwnerUserID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not enumerate tenant projects")
		return
	}
	if len(siblings) == 0 {
		// Defensive: the auth project should always show up under its
		// own owner_user_id, but if it doesn't, fall back to the
		// authenticated project alone.
		siblings = []*store.Project{authProject}
	}

	writeOrgRollup(w, ctx, h, authProject.OwnerUserID, siblings)
}

// writeOrgRollup aggregates burn across `projects` (assumed to all
// belong to the same owner) and serializes the response. Extracted so
// HandleOrgRollup can take both the normal path and the legacy
// no-owner fallback path with the same body.
func writeOrgRollup(
	w http.ResponseWriter,
	ctx context.Context,
	h *Handlers,
	ownerUserID string,
	projects []*store.Project,
) {
	now := time.Now().UTC()
	startOfMonth := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	startOfPriorMonth := startOfMonth.AddDate(0, -1, 0)
	cutoff24h := now.Add(-24 * time.Hour)

	type ProjectRollup struct {
		ProjectID            string  `json:"project_id"`
		Name                 string  `json:"name"`
		Tier                 string  `json:"tier"`
		MTDCostUSD           float64 `json:"mtd_cost_usd"`
		MTDExecutions        int     `json:"mtd_executions"`
		PriorMonthCostUSD    float64 `json:"prior_month_cost_usd"`
		PriorMonthExecutions int     `json:"prior_month_executions"`
		Last24hCostUSD       float64 `json:"last_24h_cost_usd"`
		Last24hExecutions    int     `json:"last_24h_executions"`
	}

	rollups := make([]ProjectRollup, 0, len(projects))
	var totalMTDCost, totalPriorCost, total24hCost float64
	var totalMTDExec, totalPriorExec, total24hExec int

	for _, p := range projects {
		pr := ProjectRollup{ProjectID: p.ProjectID, Name: p.Name, Tier: p.Tier}

		mtdCost, mtdExec, err := h.Store.SumExecutionCostByProjectSince(ctx, p.ProjectID, startOfMonth)
		if err != nil {
			h.Logger.Warn("org rollup: mtd sum failed",
				"project_id", p.ProjectID, "error", err.Error())
		} else {
			pr.MTDCostUSD = mtdCost
			pr.MTDExecutions = mtdExec
		}

		// Prior month: SUM between startOfPriorMonth and startOfMonth.
		// SumExecutionCostByProjectSince only has a `since` cutoff, so
		// we compute (since_priorMonth) - (since_thisMonth) to get the
		// prior-month bucket without a new store method.
		priorPlusCurrentCost, priorPlusCurrentExec, err := h.Store.SumExecutionCostByProjectSince(ctx, p.ProjectID, startOfPriorMonth)
		if err != nil {
			h.Logger.Warn("org rollup: prior-month sum failed",
				"project_id", p.ProjectID, "error", err.Error())
		} else {
			pr.PriorMonthCostUSD = priorPlusCurrentCost - pr.MTDCostUSD
			pr.PriorMonthExecutions = priorPlusCurrentExec - pr.MTDExecutions
			if pr.PriorMonthCostUSD < 0 {
				pr.PriorMonthCostUSD = 0
			}
			if pr.PriorMonthExecutions < 0 {
				pr.PriorMonthExecutions = 0
			}
		}

		last24hCost, last24hExec, err := h.Store.SumExecutionCostByProjectSince(ctx, p.ProjectID, cutoff24h)
		if err != nil {
			h.Logger.Warn("org rollup: 24h sum failed",
				"project_id", p.ProjectID, "error", err.Error())
		} else {
			pr.Last24hCostUSD = last24hCost
			pr.Last24hExecutions = last24hExec
		}

		totalMTDCost += pr.MTDCostUSD
		totalMTDExec += pr.MTDExecutions
		totalPriorCost += pr.PriorMonthCostUSD
		totalPriorExec += pr.PriorMonthExecutions
		total24hCost += pr.Last24hCostUSD
		total24hExec += pr.Last24hExecutions

		rollups = append(rollups, pr)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"owner_user_id": ownerUserID,
		"project_count": len(projects),
		"mtd": map[string]any{
			"total_cost_usd":   totalMTDCost,
			"total_executions": totalMTDExec,
		},
		"prior_month": map[string]any{
			"total_cost_usd":   totalPriorCost,
			"total_executions": totalPriorExec,
		},
		"last_24h": map[string]any{
			"total_cost_usd":   total24hCost,
			"total_executions": total24hExec,
		},
		"projects": rollups,
	})
}

// HandleGetBudgetCeiling returns the configured tenant budget ceiling
// for the authenticated project's owner (#252). 404 with a structured
// body when no ceiling has been configured, so the UI can render an
// empty state and prompt the user to set one up.
func (h *Handlers) HandleGetBudgetCeiling(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	authProject, err := h.Store.GetProject(ctx, authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load authenticated project")
		return
	}
	if authProject.OwnerUserID == "" {
		writeError(w, http.StatusNotFound, "no ceiling configured")
		return
	}

	c, err := h.Store.GetTenantBudgetCeiling(ctx, authProject.OwnerUserID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "no ceiling configured")
			return
		}
		h.Logger.Error("get budget ceiling failed",
			"owner_user_id", authProject.OwnerUserID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load ceiling")
		return
	}
	writeJSON(w, http.StatusOK, c)
}

// HandleUpsertBudgetCeiling configures or updates the tenant budget
// ceiling for the authenticated project's owner (#252).
//
// Body shape:
//
//	{
//	  "monthly_ceiling_usd": 1000.0,
//	  "breach_action": "warn" | "halt",
//	  "notify_email": "ops@example.com",     // optional
//	  "notify_webhook_url": "https://..."     // optional
//	}
func (h *Handlers) HandleUpsertBudgetCeiling(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	authProject, err := h.Store.GetProject(ctx, authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not load authenticated project")
		return
	}
	if authProject.OwnerUserID == "" {
		writeError(w, http.StatusForbidden, "this project has no owner; cannot configure a tenant ceiling")
		return
	}

	var body struct {
		MonthlyCeilingUSD float64 `json:"monthly_ceiling_usd"`
		BreachAction      string  `json:"breach_action"`
		NotifyEmail       string  `json:"notify_email"`
		NotifyWebhookURL  string  `json:"notify_webhook_url"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.MonthlyCeilingUSD <= 0 {
		writeError(w, http.StatusBadRequest, "monthly_ceiling_usd must be > 0")
		return
	}
	if body.BreachAction == "" {
		body.BreachAction = "warn"
	}
	if body.BreachAction != "warn" && body.BreachAction != "halt" {
		writeError(w, http.StatusBadRequest, "breach_action must be 'warn' or 'halt'")
		return
	}

	c := &store.TenantBudgetCeiling{
		OwnerUserID:       authProject.OwnerUserID,
		MonthlyCeilingUSD: body.MonthlyCeilingUSD,
		BreachAction:      body.BreachAction,
		NotifyEmail:       body.NotifyEmail,
		NotifyWebhookURL:  body.NotifyWebhookURL,
	}
	if err := h.Store.UpsertTenantBudgetCeiling(ctx, c); err != nil {
		h.Logger.Error("upsert budget ceiling failed",
			"owner_user_id", authProject.OwnerUserID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save ceiling")
		return
	}

	// Echo back the saved row (now includes server-set created_at /
	// updated_at) for the UI to re-render without a follow-up GET.
	saved, err := h.Store.GetTenantBudgetCeiling(ctx, authProject.OwnerUserID)
	if err != nil {
		// Save succeeded but read-back failed; return what we have.
		writeJSON(w, http.StatusOK, c)
		return
	}
	writeJSON(w, http.StatusOK, saved)
}

// tierRetentionCap returns the maximum retention_days a project of
// the given tier may configure, and whether the tier may set
// indefinite retention. Tier strings are normalized lowercase; an
// unknown tier falls through to Hobby semantics (conservative).
//
// Caps match the /pricing card promises (#262, updated pre-#30):
//
//	Hobby:      up to 15 days,   no indefinite
//	Team:       up to 90 days,   no indefinite
//	Enterprise: up to 3650 days, indefinite allowed
//
// Hobby was bumped down from 30 to 15 pre-#30 to create a real
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
	case TierEnterprise:
		return 3650, true
	case TierTeam:
		return TeamDefaultRetentionDays, false
	default:
		// Hobby, empty, or unknown tier.
		return HobbyDefaultRetentionDays, false
	}
}

// HandleGetRetention returns the configured retention for the
// authenticated project (#262). The response includes the tier's
// caps so the UI can disable the indefinite checkbox / cap the day
// input without an extra round-trip.
//
// Response:
//
//	{
//	  "ok": true,
//	  "retention_days": 30,        // or null when indefinite
//	  "is_indefinite": false,
//	  "tier": "team",
//	  "max_days": 30,              // tier-specific cap
//	  "allow_indefinite": false    // only true on enterprise
//	}
func (h *Handlers) HandleGetRetention(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	p, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get retention: load project failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load retention")
		return
	}
	days, err := h.Store.GetProjectRetentionDays(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("get retention failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load retention")
		return
	}
	maxDays, allowIndefinite := tierRetentionCap(p.Tier)
	resp := map[string]any{
		"ok":               true,
		"retention_days":   days,
		"is_indefinite":    days == nil,
		"tier":             p.Tier,
		"max_days":         maxDays,
		"allow_indefinite": allowIndefinite,
	}
	writeJSON(w, http.StatusOK, resp)
}

// HandleSetRetention updates the retention window for the
// authenticated project (#262). Tier-gated:
//
//	Hobby:      max 7 days,    no indefinite
//	Pro:        max 30 days,   no indefinite
//	Enterprise: max 3650 days, indefinite allowed
//
// Returns 403 with the tier's caps in the response when the customer
// asks for more than their tier permits, so the dashboard can render
// a clear "upgrade for longer retention" message instead of a generic
// validation failure.
func (h *Handlers) HandleSetRetention(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var body struct {
		RetentionDays *int `json:"retention_days"`
		IsIndefinite  bool `json:"is_indefinite"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}

	// Load project so we can enforce tier caps. Done BEFORE generic
	// validation so a Hobby customer asking for indefinite gets the
	// tier-specific 403 instead of a generic 400.
	p, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set retention: load project failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load project")
		return
	}
	maxDays, allowIndefinite := tierRetentionCap(p.Tier)

	// Tier guard: indefinite only on Enterprise.
	if body.IsIndefinite && !allowIndefinite {
		writeError(w, http.StatusForbidden,
			"indefinite retention is an Enterprise feature; current tier '"+p.Tier+
				"' is capped at "+strconv.Itoa(maxDays)+" days")
		return
	}

	// Tier guard: requested days must be within the tier's cap.
	var days *int
	if !body.IsIndefinite && body.RetentionDays != nil {
		v := *body.RetentionDays
		if v < 1 {
			writeError(w, http.StatusBadRequest,
				"retention_days must be at least 1")
			return
		}
		if v > maxDays {
			writeError(w, http.StatusForbidden,
				"current tier '"+p.Tier+"' is capped at "+strconv.Itoa(maxDays)+
					" days; upgrade for longer retention")
			return
		}
		days = &v
	}

	if err := h.Store.SetProjectRetentionDays(r.Context(), authProjectID, days); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set retention failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save retention")
		return
	}

	h.Logger.Info("retention updated",
		"project_id", authProjectID,
		"tier", p.Tier,
		"retention_days", days,
		"is_indefinite", days == nil)
	// #207 step C — retention is a data-handling control. Customers and
	// auditors want to see when it changes and by whom. days is *int
	// so we record nil as is_indefinite=true and skip retention_days.
	retentionMeta := map[string]any{
		"tier":          p.Tier,
		"is_indefinite": days == nil,
	}
	if days != nil {
		retentionMeta["retention_days"] = *days
	}
	h.recordAuditEvent(r, AuditRetentionUpdate, "project", authProjectID, retentionMeta)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"retention_days":   days,
		"is_indefinite":    days == nil,
		"tier":             p.Tier,
		"max_days":         maxDays,
		"allow_indefinite": allowIndefinite,
	})
}

// HandleGetProviderIncidentConfig returns the per-project
// provider_incident detector configuration (migration 040). Default
// 2 mirrors the historical hardcoded constant. Single-tenant
// customers should set min_tenants to 1 so any provider error
// fires the detector.
//
// Response shape matches HandleGetTimeBudgetConfig so the dashboard
// can use one shared pattern for per-project threshold tiles.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "min_tenants": 2
//	}
func (h *Handlers) HandleGetProviderIncidentConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectProviderIncidentMinTenants(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get provider_incident config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load provider_incident config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":  authProjectID,
		"min_tenants": n,
	})
}

// HandleSetProviderIncidentConfig updates the per-project
// provider_incident threshold. Body:
//
//	{"min_tenants": 1}
//
// Validation: min_tenants must be >= 1. Higher values are accepted
// without an upper cap — a 1000-tenant customer might want a
// threshold of 10 before a single provider blip pages on-call.
func (h *Handlers) HandleSetProviderIncidentConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		MinTenants int `json:"min_tenants"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.MinTenants < 1 {
		writeError(w, http.StatusBadRequest, "min_tenants must be >= 1")
		return
	}
	// Same upper-bound discipline as time_budget / tool_return_value
	// (#276.c). The dashboard tile caps at 1000; match server-side.
	const maxMinTenants = 1000
	if body.MinTenants > maxMinTenants {
		writeError(w, http.StatusBadRequest,
			"min_tenants must be <= 1000")
		return
	}
	if err := h.Store.SetProjectProviderIncidentMinTenants(
		r.Context(), authProjectID, body.MinTenants,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set provider_incident config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update provider_incident config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"min_tenants": body.MinTenants,
	})
}

// HandleGetTimeBudgetConfig returns the per-project time_budget
// detector threshold (migration 041) in milliseconds. Default 60000
// mirrors the historical hardcoded constant. Chat-agent projects
// often lower it (e.g. 30000); research-agent projects often raise
// it (e.g. 300000).
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "threshold_ms": 60000
//	}
func (h *Handlers) HandleGetTimeBudgetConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectTimeBudgetMs(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get time_budget config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load time_budget config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":   authProjectID,
		"threshold_ms": n,
	})
}

// HandleSetTimeBudgetConfig updates the per-project time_budget
// threshold. Body:
//
//	{"threshold_ms": 30000}
//
// Validation: threshold_ms must be >= 1 (zero would fire on every
// terminal execution). No upper cap — a batch-processing project
// might legitimately set hours-long thresholds.
func (h *Handlers) HandleSetTimeBudgetConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		ThresholdMs int `json:"threshold_ms"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.ThresholdMs < 1 {
		writeError(w, http.StatusBadRequest, "threshold_ms must be >= 1")
		return
	}
	// Server-side upper bound matches the dashboard tile's 24h cap
	// (86_400_000 ms). Without this, a typo of 999999999999 was
	// silently accepted by the backend and mishandled downstream
	// (#276.c).
	const maxThresholdMs = 86_400_000 // 24 hours
	if body.ThresholdMs > maxThresholdMs {
		writeError(w, http.StatusBadRequest,
			"threshold_ms must be <= 86400000 (24 hours)")
		return
	}
	if err := h.Store.SetProjectTimeBudgetMs(
		r.Context(), authProjectID, body.ThresholdMs,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set time_budget config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update time_budget config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"threshold_ms": body.ThresholdMs,
	})
}

// HandleGetCostVelocityConfig returns the per-project cost_velocity
// detector threshold (migration 043) in USD. Default 1.00 mirrors
// the post-Wave-0 sensible default, raised from the broken v0.0.1
// hardcoded $0.001 that fired on every real execution. Cost-sensitive
// customers often lower it; batch / tolerant customers often raise it.
//
// Response shape matches HandleGetTimeBudgetConfig /
// HandleGetProviderIncidentConfig so the dashboard can use one
// shared pattern for per-project threshold tiles.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "threshold_usd": 1.00
//	}
func (h *Handlers) HandleGetCostVelocityConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectCostVelocityThresholdUSD(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get cost_velocity config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load cost_velocity config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":    authProjectID,
		"threshold_usd": n,
	})
}

// HandleSetCostVelocityConfig updates the per-project cost_velocity
// threshold. Body:
//
//	{"threshold_usd": 0.50}
//
// Validation: threshold_usd must be in [0.01, 10000.00]. The floor
// prevents fires-on-every-execution storage abuse (a customer
// setting threshold near zero would create a failure_group for
// every execution). The ceiling prevents typo / float overflow.
//
// NOT tier-capped: cost_velocity threshold is the customer's alarm
// sensitivity, not a Mesedi-side cost vector. Same reasoning as
// provider_incident_min_tenants (see tier_caps.go).
func (h *Handlers) HandleSetCostVelocityConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		ThresholdUSD float64 `json:"threshold_usd"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	const (
		minThresholdUSD = 0.01
		maxThresholdUSD = 10_000.00
	)
	if body.ThresholdUSD < minThresholdUSD {
		writeError(w, http.StatusBadRequest,
			"threshold_usd must be >= 0.01 (lower values would fire on every execution and create storage abuse)")
		return
	}
	if body.ThresholdUSD > maxThresholdUSD {
		writeError(w, http.StatusBadRequest,
			"threshold_usd must be <= 10000.00")
		return
	}
	if err := h.Store.SetProjectCostVelocityThresholdUSD(
		r.Context(), authProjectID, body.ThresholdUSD,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set cost_velocity config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update cost_velocity config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"threshold_usd": body.ThresholdUSD,
	})
}

// HandleGetPricingInfo returns the backend pricing-table metadata so
// customers can see exactly which models Mesedi prices server-side
// AND when those prices were last reviewed. Wave 0.3 made the backend
// authoritative for known models; this endpoint is how customers
// verify which models that includes.
//
// Models not in `supported_models` fall back to the SDK-shipped
// per-event estimated_cost_usd at execution close. When that happens,
// a `pricing_unknown_model` audit_event is recorded; the dashboard
// surfaces it via the existing config-fallback chip.
//
// Response:
//
//	{
//	  "pricing_table_version": "2026-06-21",
//	  "supported_models":      ["claude-3-haiku", "claude-3-opus", ...]
//	}
//
// No auth scope beyond project context — the pricing table is the
// same for every project (per Wave 0.3 Decision 5, tier-agnostic).
func (h *Handlers) HandleGetPricingInfo(w http.ResponseWriter, r *http.Request) {
	if _, ok := ProjectIDFromContext(r.Context()); !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"pricing_table_version": pricing.PricingTableVersion,
		"supported_models":      pricing.SupportedModels(),
	})
}

// HandleGetCostVelocityRateConfig returns the per-project rate
// detector configuration (migration 044): the $/minute threshold and
// the rolling lookback window in minutes. Defaults {5.00, 5} —
// $300/hr sustained burn over a 5-minute window. Pairs with the
// absolute threshold (HandleGetCostVelocityConfig); both detectors
// fire independently because they answer different questions.
//
// Response:
//
//	{
//	  "project_id":             "...",
//	  "threshold_usd_per_min":  5.00,
//	  "window_minutes":         5
//	}
func (h *Handlers) HandleGetCostVelocityRateConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	cfg, err := h.Store.GetProjectCostVelocityRateConfig(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get cost_velocity_rate config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load cost_velocity_rate config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":            authProjectID,
		"threshold_usd_per_min": cfg.ThresholdUSDPerMin,
		"window_minutes":        cfg.WindowMinutes,
	})
}

// HandleSetCostVelocityRateConfig updates the per-project rate
// configuration. Body:
//
//	{"threshold_usd_per_min": 2.50, "window_minutes": 10}
//
// Validation:
//   - threshold_usd_per_min ∈ [0.10, 10000.00]. The floor prevents
//     fires-on-every-minute storage abuse; the ceiling prevents
//     typo / float overflow.
//   - window_minutes ∈ [1, 60]. The floor avoids noise from sub-minute
//     spikes; the ceiling bounds aggregator scan size.
//
// NOT tier-capped: same reasoning as the absolute threshold and
// provider_incident_min_tenants — alarm sensitivity is the customer's
// choice, not a Mesedi-side cost vector. See tier_caps.go.
func (h *Handlers) HandleSetCostVelocityRateConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		ThresholdUSDPerMin float64 `json:"threshold_usd_per_min"`
		WindowMinutes      int     `json:"window_minutes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	const (
		minThresholdUSDPerMin = 0.10
		maxThresholdUSDPerMin = 10_000.00
		minWindowMinutes      = 1
		maxWindowMinutes      = 60
	)
	if body.ThresholdUSDPerMin < minThresholdUSDPerMin {
		writeError(w, http.StatusBadRequest,
			"threshold_usd_per_min must be >= 0.10 (lower values would fire on every minute and create storage abuse)")
		return
	}
	if body.ThresholdUSDPerMin > maxThresholdUSDPerMin {
		writeError(w, http.StatusBadRequest,
			"threshold_usd_per_min must be <= 10000.00")
		return
	}
	if body.WindowMinutes < minWindowMinutes {
		writeError(w, http.StatusBadRequest,
			"window_minutes must be >= 1")
		return
	}
	if body.WindowMinutes > maxWindowMinutes {
		writeError(w, http.StatusBadRequest,
			"window_minutes must be <= 60 (longer windows make aggregator scans pathological)")
		return
	}
	cfg := store.CostVelocityRateConfig{
		ThresholdUSDPerMin: body.ThresholdUSDPerMin,
		WindowMinutes:      body.WindowMinutes,
	}
	if err := h.Store.SetProjectCostVelocityRateConfig(
		r.Context(), authProjectID, cfg,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set cost_velocity_rate config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update cost_velocity_rate config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                    true,
		"threshold_usd_per_min": cfg.ThresholdUSDPerMin,
		"window_minutes":        cfg.WindowMinutes,
	})
}

// HandleGetToolReturnValueConfig returns the per-project byte cap
// on tool_call return_value payloads used by the tool_schema_drift
// detector (migration 042 / #270.a). Default 8192 (8 KB) — covers
// typical tool returns while bounding pathological cases. The full
// event payload is still stored regardless of this cap; only the
// detector's fingerprint comparison is bounded.
//
// Response shape matches HandleGetTimeBudgetConfig /
// HandleGetProviderIncidentConfig so the dashboard can use one
// shared pattern for per-project threshold tiles.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "max_bytes": 8192
//	}
func (h *Handlers) HandleGetToolReturnValueConfig(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	n, err := h.Store.GetProjectToolReturnValueMaxBytes(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("get tool_return_value config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load tool_return_value config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id": authProjectID,
		"max_bytes":  n,
	})
}

// HandleSetToolReturnValueConfig updates the per-project cap. Body:
//
//	{"max_bytes": 16384}
//
// Validation: max_bytes must be >= 1. Server-side upper bound is
// 1 MB (matches the existing payload cap from #243) — a per-project
// fingerprint cap larger than the wire-format max is meaningless
// because the SDK can't ship more than 1 MB anyway.
func (h *Handlers) HandleSetToolReturnValueConfig(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		MaxBytes int `json:"max_bytes"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.MaxBytes < 1 {
		writeError(w, http.StatusBadRequest, "max_bytes must be >= 1")
		return
	}
	const oneMegabyte = 1 << 20
	if body.MaxBytes > oneMegabyte {
		writeError(w, http.StatusBadRequest, "max_bytes must be <= 1048576 (the wire-format payload cap)")
		return
	}
	if err := h.Store.SetProjectToolReturnValueMaxBytes(
		r.Context(), authProjectID, body.MaxBytes,
	); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		h.Logger.Error("set tool_return_value config failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not update tool_return_value config")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"max_bytes": body.MaxBytes,
	})
}

// HandleGetToolReturnValueStats returns recent-window telemetry on
// how often tool_call return_values are being clipped (#270.c).
// Two clip sources are counted:
//
//   - SDK truncation: the SDK shipped the literal "<truncated>"
//     string after the return_value JSON exceeded its on-wire cap
//     (16 KB in v0.5.0+).
//   - Backend exclusion: the return_value JSON length exceeds the
//     per-project tool_return_value_max_bytes cap; the detector
//     drops these from the schema-drift fingerprint comparison.
//
// Window defaults to the last 24 hours. A high rate means the
// customer should raise their cap (or refactor the tool to return
// less) so schema_drift has more comparable signal.
//
// Response shape:
//
//	{
//	  "project_id": "...",
//	  "window_hours": 24,
//	  "total_calls": 1500,
//	  "truncated_sentinel_count": 12,
//	  "oversized_count": 8,
//	  "max_bytes": 8192
//	}
func (h *Handlers) HandleGetToolReturnValueStats(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	maxBytes, err := h.Store.GetProjectToolReturnValueMaxBytes(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		// Fall through with default 8192 on transient errors so
		// the stats query doesn't fail outright — the customer
		// just sees stats computed against the default cap rather
		// than their configured one.
		maxBytes = 8192
	}
	stats, err := h.Store.GetToolReturnValueStats(
		r.Context(), authProjectID, 24, maxBytes,
	)
	if err != nil {
		h.Logger.Error("get tool_return_value stats failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load tool_return_value stats")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":               authProjectID,
		"window_hours":             stats.WindowHours,
		"total_calls":              stats.TotalCalls,
		"truncated_sentinel_count": stats.TruncatedCount,
		"oversized_count":          stats.OversizedCount,
		"max_bytes":                maxBytes,
	})
}

// HandleGetConfigFallbackStats returns recent-window counts of
// per-project config-read fallbacks (#276.d). The dashboard uses
// this to render a warning chip on each Settings tile when the
// corresponding config has fallen back to the default in the last
// 24 h — a signal that something operational is going wrong with
// the backend (transient DB blip, dropped column, etc.) and the
// customer's tuning is being silently ignored.
//
// Response:
//
//	{
//	  "project_id": "...",
//	  "window_hours": 24,
//	  "time_budget_count": 0,
//	  "provider_incident_min_tenants_count": 0,
//	  "tool_return_value_max_bytes_count": 0
//	}
func (h *Handlers) HandleGetConfigFallbackStats(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	stats, err := h.Store.GetConfigFallbackStats(r.Context(), authProjectID, 24)
	if err != nil {
		h.Logger.Error("get config_fallback stats failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load config_fallback stats")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"project_id":                          authProjectID,
		"window_hours":                        stats.WindowHours,
		"time_budget_count":                   stats.TimeBudgetCount,
		"provider_incident_min_tenants_count": stats.ProviderIncidentMinTenantsCount,
		"tool_return_value_max_bytes_count":   stats.ToolReturnValueMaxBytesCount,
		"class_severity_override_count":       stats.ClassSeverityOverrideCount,
	})
}

// HandleListClassSeverities returns the full map of failure classes
// to their currently-effective severity for the authenticated project
// (#261). The map includes EVERY known failure class with its current
// value, sourced from:
//
//  1. project_class_severities row, if one exists for that class
//  2. severity.Default(class), otherwise
//
// The response also carries an `is_override` flag per class so the UI
// can render "(default)" vs "(custom)" badges next to each value.
//
// Response shape:
//
//	{
//	  "classes": [
//	    {"failure_class": "crashes", "severity": "critical", "is_override": false},
//	    {"failure_class": "loops",   "severity": "warning",  "is_override": true},
//	    ...
//	  ],
//	  "valid_severities": ["critical", "warning", "info"]
//	}
func (h *Handlers) HandleListClassSeverities(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	overrides, err := h.Store.ListProjectClassSeverityOverrides(ctx, authProjectID)
	if err != nil {
		h.Logger.Error("list class severity overrides failed",
			"project_id", authProjectID, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not load overrides")
		return
	}
	overrideMap := make(map[string]string, len(overrides))
	for _, o := range overrides {
		overrideMap[o.FailureClass] = o.Severity
	}

	// The canonical class list mirrors the failure-class registry. We
	// avoid pulling it from a separate package to keep the surface
	// small; the strings here match what the detectors emit.
	//
	// Ordering is opinionated: critical-by-default first (so the most
	// dangerous classes anchor the top of the table on /app/settings),
	// then warning-by-default cost/quality signals, then info-by-default
	// behavioral signals. severity.Default() is the source of truth for
	// the initial value; this slice only controls which classes the UI
	// renders.
	classes := []string{
		// critical-by-default
		"crashes",
		"tool_failures",
		"validator_failures",
		"prompt_injection",
		"data_leakage",
		"tool_schema_drift",
		"grounding_failure",
		"cascading_failure",
		"coordination_deadlock",
		"sandbox_escape",
		// warning-by-default
		"cost_velocity",
		"time_budget",
		"step_count",
		"infrastructure_throttled",
		"context_overflow",
		"token_waste",
		"provider_incident",
		"hitl_timeout",
		"hitl_rejection_spike",
		// info-by-default
		"identical_call_loop",
		"similar_call_loop",
		"semantic_loop",
		"drift",
	}

	type classRow struct {
		FailureClass string `json:"failure_class"`
		Severity     string `json:"severity"`
		IsOverride   bool   `json:"is_override"`
	}
	out := make([]classRow, 0, len(classes))
	for _, c := range classes {
		if sev, ok := overrideMap[c]; ok {
			out = append(out, classRow{FailureClass: c, Severity: sev, IsOverride: true})
		} else {
			out = append(out, classRow{
				FailureClass: c,
				Severity:     string(severity.Default(c)),
				IsOverride:   false,
			})
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":               true,
		"classes":          out,
		"valid_severities": severity.All(),
	})
}

// HandleUpsertClassSeverity sets or updates the severity override for
// a single failure class (#261). The class name comes from the URL
// path; the body carries the new severity value.
//
//	PUT /me/class-severities/loops
//	{ "severity": "critical" }
func (h *Handlers) HandleUpsertClassSeverity(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	class := r.PathValue("class")
	if class == "" {
		writeError(w, http.StatusBadRequest, "class path parameter required")
		return
	}

	var body struct {
		Severity string `json:"severity"`
	}
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if !severity.Valid(body.Severity) {
		writeError(w, http.StatusBadRequest,
			"severity must be one of critical|warning|info")
		return
	}

	o := &store.ProjectClassSeverity{
		ProjectID:    authProjectID,
		FailureClass: class,
		Severity:     body.Severity,
	}
	if err := h.Store.UpsertProjectClassSeverity(r.Context(), o); err != nil {
		h.Logger.Error("upsert class severity failed",
			"project_id", authProjectID, "class", class, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not save override")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"failure_class": class,
		"severity":      body.Severity,
		"is_override":   true,
	})
}

// HandleDeleteClassSeverity removes a per-project severity override
// so the dispatcher reverts to severity.Default for that class (#261).
// Idempotent: 200 OK even if no override existed.
func (h *Handlers) HandleDeleteClassSeverity(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	class := r.PathValue("class")
	if class == "" {
		writeError(w, http.StatusBadRequest, "class path parameter required")
		return
	}
	if err := h.Store.DeleteProjectClassSeverity(r.Context(), authProjectID, class); err != nil {
		h.Logger.Error("delete class severity failed",
			"project_id", authProjectID, "class", class, "error", err.Error())
		writeError(w, http.StatusInternalServerError, "could not delete override")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":            true,
		"failure_class": class,
		"severity":      string(severity.Default(class)),
		"is_override":   false,
	})
}

// computeExecutionCost sums the estimated USD cost across every
// llm_call event in the slice. Each event's payload is unmarshaled into
// a small struct extracting only the four fields cost-computation needs
// (model, input_tokens, output_tokens, estimated_cost_usd); everything
// else is ignored, so changes to the payload schema (adding fields)
// don't affect this code.
//
// Wave 0.3 semantics — backend is the source of truth for known models;
// SDK-shipped per-event cost is the fallback for unknown models:
//
//   - Known model (pricing.IsKnownModel == true): use the backend
//     pricing table. SDK-shipped cost on the event is ignored. This
//     means new model pricing or pricing changes ship with a backend
//     deploy without waiting for an SDK release.
//
//   - Unknown model: use the per-event payload.estimated_cost_usd as
//     the fallback. Customer using a brand-new model the day it ships
//     doesn't see $0 costs — they see the SDK's best-effort number
//     until the next backend deploy adds the row.
//
// Returns (totalUSD, unknownModelIDs). Caller uses unknownModelIDs to
// emit a single audit_event per execution (not per event) for the
// dashboard's unknown-model surface.
//
// Events whose payload fails to unmarshal are skipped silently — a
// single malformed event shouldn't break cost computation for the
// whole execution.
func computeExecutionCost(evts []*events.Event) (totalUSD float64, unknownModels []string) {
	seenUnknown := map[string]struct{}{}
	for _, e := range evts {
		if e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			Model             string  `json:"model"`
			InputTokens       int     `json:"input_tokens"`
			OutputTokens      int     `json:"output_tokens"`
			EstimatedCostUSD  float64 `json:"estimated_cost_usd"`
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
		if pricing.IsKnownModel(p.Model) {
			totalUSD += pricing.ComputeLLMCost(p.Model, p.InputTokens, p.OutputTokens)
			continue
		}
		// Unknown to backend table — fall back to SDK-shipped cost
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

// HandleListAPIKeys returns the calling project's API keys (without
// the hash, never serialized).
func (h *Handlers) HandleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	keys, err := h.Store.ListAPIKeysForProject(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("list api_keys failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"api_keys": keys,
		"count":    len(keys),
	})
}

// HandleCreateAPIKey mints a new API key for the calling project and
// returns the RAW KEY VALUE ONCE, this is the only moment a caller
// ever sees it. The server only persists the hash. Caller must store
// the raw key immediately; a lost raw key requires a new mint.
//
// Request body (optional): {"name": "human-readable label"}.
func (h *Handlers) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var body struct {
		Name string `json:"name,omitempty"`
	}
	// Empty body is fine, name is optional. Skip the strict-decode
	// path here because the field is intentionally permissive.
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	rawKey, hash, prefix, err := MintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint key: "+err.Error())
		return
	}

	keyID := "key-" + prefix[len("mesedi_sk_"):] + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	// Tag the new key with the caller's user_id so it authenticates
	// as the same org member that created it (#263 RBAC). If the
	// caller is on a legacy key with no user_id, the new key inherits
	// the project's owner identity so the chain doesn't break.
	callerUserID, _ := UserIDFromContext(r.Context())
	if callerUserID == "" {
		if p, perr := h.Store.GetProject(r.Context(), authProjectID); perr == nil && p != nil {
			if p.OwnerUserID != "" {
				callerUserID = p.OwnerUserID
			} else {
				callerUserID = p.OwnerEmail
			}
		}
	}
	rec := &store.APIKey{
		KeyID:     keyID,
		ProjectID: authProjectID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      body.Name,
		UserID:    callerUserID,
	}
	if err := h.Store.CreateAPIKey(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, "persist key: "+err.Error())
		return
	}

	h.Logger.Info("api key minted",
		"key_id", keyID,
		"prefix", prefix,
		"project_id", authProjectID,
		"name", body.Name,
	)

	// #207 audit log: API key creation is a top-tier admin action.
	// Recorded after the row lands so a failed mint never produces
	// a false-positive audit entry.
	h.recordAuditEvent(r, AuditAPIKeyCreate, "api_key", keyID, map[string]any{
		"name":   body.Name,
		"prefix": prefix,
	})

	// Return the raw key in this ONE response. The hash never leaves.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":      true,
		"key_id":  keyID,
		"raw_key": rawKey,
		"prefix":  prefix,
		"name":    body.Name,
		"warning": "Store this raw_key now, it will never be shown again.",
	})
}

// HandleRevokeAPIKey hard-deletes an API key. Project-scoped via the
// Store method's project_id guard. Admin-only (#263 RBAC): a Read or
// Write member could otherwise revoke the admin's own key.
func (h *Handlers) HandleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "key_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	// Last-key protection (#188 Robert flagged): revoking the only
	// remaining key would leave the project unable to authenticate
	// against anything but admin endpoints. Only the close-account
	// flow is allowed to bring key count to zero, so refuse here.
	keys, lkErr := h.Store.ListAPIKeysForProject(r.Context(), authProjectID)
	if lkErr != nil {
		writeError(w, http.StatusInternalServerError,
			"could not verify remaining keys: "+lkErr.Error())
		return
	}
	if len(keys) <= 1 {
		writeError(w, http.StatusConflict,
			"cannot revoke the project's last API key; mint a new key first, or close the account from settings to remove the project entirely")
		return
	}
	// #213 Batch 2: find the target key's UserID so we can kill that
	// user's dashboard sessions after the API key is gone. Robert's
	// rule: revoking a member's key MUST also log them out of every
	// browser they have open. We pluck the UserID from the existing
	// keys slice rather than make a new Store call.
	var revokedKeyUserID string
	for _, k := range keys {
		if k.KeyID == keyID {
			revokedKeyUserID = k.UserID
			break
		}
	}
	if err := h.Store.DeleteAPIKey(r.Context(), keyID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "api key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	// #213 Batch 2: kick the affected user out of every dashboard
	// browser tab they have open. Best-effort: a delete-sessions
	// failure does NOT undo the key revocation; the audit row
	// records sessions_revoked=0 so an operator can re-run a
	// cleanup if needed.
	sessionsRevoked := 0
	if revokedKeyUserID != "" {
		if n, sErr := h.Store.DeleteSessionsByUserID(r.Context(), revokedKeyUserID); sErr == nil {
			sessionsRevoked = n
		} else {
			h.Logger.Warn("api key revoke: kill sessions failed (key still revoked)",
				"key_id", keyID, "user_id", revokedKeyUserID, "error", sErr.Error())
		}
	}
	h.Logger.Info("api key revoked",
		"key_id", keyID,
		"project_id", authProjectID,
		"sessions_revoked", sessionsRevoked,
	)
	// #207 audit log: key revocation is admin-tier. #213 Batch 2
	// adds sessions_revoked so the row records the full effect.
	h.recordAuditEvent(r, AuditAPIKeyRevoke, "api_key", keyID, map[string]any{
		"sessions_revoked": sessionsRevoked,
	})
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"key_id": keyID,
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Webhook escalation config (task #83 slice 1)
// ─────────────────────────────────────────────────────────────────────────

// validFailureClasses is the allowlist of class names accepted in the
// `enabled_classes` field on POST /webhooks. Must stay in sync with
// the FailureClass* constants in store.go so that every class the
// detector emits is also acceptable in a webhook filter. The PL2
// fix on 2026-06-13 unblocked the 13 newer classes that had drifted
// since the webhook layer was first written; the dashboard's
// /app/webhooks form no longer renders chips at all (routing for
// individual classes is configured under /app/settings, severity
// routing), so the only consumer of this list now is API-direct
// users posting to /webhooks via curl or SDK.
var validFailureClasses = map[string]struct{}{
	store.FailureClassCrashes:             {},
	store.FailureClassLoops:               {},
	store.FailureClassToolFailures:        {},
	store.FailureClassValidator:           {},
	store.FailureClassDrift:               {},
	store.FailureClassCostVelocity:        {},
	store.FailureClassInjection:           {},
	store.FailureClassInfraThrottled:      {},
	store.FailureClassDataLeakage:         {},
	store.FailureClassSemanticLoop:        {},
	store.FailureClassToolSchemaDrift:     {},
	store.FailureClassContextOverflow:     {},
	store.FailureClassTokenWaste:          {},
	store.FailureClassSandboxEscape:       {},
	store.FailureClassGroundingFailure:    {},
	store.FailureClassCascadingFailure:    {},
	store.FailureClassCoordinationDeadlock: {},
	store.FailureClassProviderIncident:    {},
	store.FailureClassHITLTimeout:         {},
	store.FailureClassHITLRejectionSpike:  {},
}

// HandleListWebhooks returns the calling project's webhooks. The
// `secret` field is never serialized, it's only ever shown once at
// creation time.
func (h *Handlers) HandleListWebhooks(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	hooks, err := h.Store.ListProjectWebhooksForProject(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("list webhooks failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"webhooks": hooks,
		"count":    len(hooks),
	})
}

// HandleCreateWebhook registers a new webhook for the calling project
// and returns the generated secret ONCE. Subsequent list responses
// omit the secret.
//
// Request body:
//
//	{
//	  "name":            "string (optional, human label)",
//	  "url":             "https://... (required, must be http(s))",
//	  "enabled_classes": ["crashes","tool_failures"] (optional; empty/missing = all),
//	  "enabled":         true (optional, default true)
//	}
//
// The dispatcher (slice 2) will only deliver to webhooks where
// enabled=true and the failure_group's class is either in
// enabled_classes OR enabled_classes is empty.
func (h *Handlers) HandleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var body struct {
		Name           string   `json:"name,omitempty"`
		URL            string   `json:"url"`
		EnabledClasses []string `json:"enabled_classes,omitempty"`
		Enabled        *bool    `json:"enabled,omitempty"`
		// SeverityFilter is a comma-separated list of severities this
		// webhook should fire on (#261). Empty/omitted = fire on every
		// severity (backward compatible). Unknown tokens are dropped
		// by severity.ParseFilter, but we explicitly validate here so
		// a typo doesn't silently produce a fire-on-nothing webhook.
		SeverityFilter string `json:"severity_filter,omitempty"`
		// RecurrenceMode picks whether (and how) the webhook fires on
		// recurrences of an existing failure group (#249). One of
		// "off" | "every_event" | "throttled". Empty/omitted defaults
		// to "off" so legacy clients see the pre-#249 behavior.
		RecurrenceMode string `json:"recurrence_mode,omitempty"`
		// RecurrenceWindowSeconds is required only when
		// RecurrenceMode is "throttled". Below the 60s floor the
		// dispatcher promotes the value to 60.
		RecurrenceWindowSeconds int `json:"recurrence_window_seconds,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	// URL validation: must be parseable, must have http/https scheme,
	// must have a host. Anything else is a misconfiguration that would
	// just generate dispatcher-side errors later.
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" {
		writeError(w, http.StatusBadRequest, "url required")
		return
	}
	parsed, err := url.Parse(body.URL)
	if err != nil || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "url is not a valid URL")
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		writeError(w, http.StatusBadRequest, "url must use http or https scheme")
		return
	}

	// Validate enabled_classes, every entry must match a known class.
	// Unknown class names would just silently never fire, which is the
	// worst failure mode for an alerting feature; reject loudly.
	for _, c := range body.EnabledClasses {
		if _, known := validFailureClasses[c]; !known {
			writeError(w, http.StatusBadRequest,
				"unknown failure_class: "+c+" (valid: crashes, loops, tool_failures, validator_failures, drift, cost_velocity, prompt_injection)")
			return
		}
	}

	// Validate severity_filter (#261). Normalize whitespace + case,
	// reject if non-empty input parsed to no valid severities (which
	// would mean every token was misspelled). Empty input is fine,
	// it means "fire on every severity".
	if strings.TrimSpace(body.SeverityFilter) != "" {
		parsed := severity.ParseFilter(body.SeverityFilter)
		if len(parsed) == 0 {
			writeError(w, http.StatusBadRequest,
				"severity_filter contains no valid severities (valid: critical, warning, info)")
			return
		}
		body.SeverityFilter = severity.FormatFilter(parsed) // canonicalize
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	// Validate recurrence_mode (#249). Empty input defaults to "off"
	// for forward-compatibility with legacy clients.
	recurrenceMode := strings.TrimSpace(body.RecurrenceMode)
	if recurrenceMode == "" {
		recurrenceMode = store.RecurrenceModeOff
	}
	switch recurrenceMode {
	case store.RecurrenceModeOff, store.RecurrenceModeEveryEvent, store.RecurrenceModeThrottled:
		// ok
	default:
		writeError(w, http.StatusBadRequest,
			"recurrence_mode must be one of: "+
				store.RecurrenceModeOff+", "+
				store.RecurrenceModeEveryEvent+", "+
				store.RecurrenceModeThrottled)
		return
	}
	recurrenceWindowSeconds := body.RecurrenceWindowSeconds
	if recurrenceMode == store.RecurrenceModeThrottled {
		if recurrenceWindowSeconds < store.RecurrenceMinWindowSeconds {
			writeError(w, http.StatusBadRequest,
				"recurrence_window_seconds must be at least "+
					strconv.Itoa(store.RecurrenceMinWindowSeconds)+
					" when recurrence_mode is \"throttled\"")
			return
		}
	} else {
		// Window is meaningless for off and every_event; zero it out
		// so storage stays clean.
		recurrenceWindowSeconds = 0
	}

	// Generate webhook_id + secret. webhook_id is a short stable
	// identifier for client-side reference; secret is 32 bytes of
	// random entropy hex-encoded (256-bit HMAC key, industry-standard
	// strength for symmetric webhook signing).
	webhookID, err := newWebhookID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate webhook_id: "+err.Error())
		return
	}
	secret, err := newWebhookSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate secret: "+err.Error())
		return
	}

	rec := &store.ProjectWebhook{
		WebhookID:               webhookID,
		ProjectID:               authProjectID,
		Name:                    body.Name,
		URL:                     body.URL,
		Secret:                  secret,
		EnabledClasses:          body.EnabledClasses,
		Enabled:                 enabled,
		SeverityFilter:          body.SeverityFilter,
		RecurrenceMode:          recurrenceMode,
		RecurrenceWindowSeconds: recurrenceWindowSeconds,
	}
	if err := h.Store.CreateProjectWebhook(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, "persist webhook: "+err.Error())
		return
	}

	h.Logger.Info("webhook created",
		"webhook_id", webhookID,
		"project_id", authProjectID,
		"url", body.URL,
		"enabled", enabled,
		"class_filter_count", len(body.EnabledClasses),
		"severity_filter", body.SeverityFilter,
		"recurrence_mode", recurrenceMode,
		"recurrence_window_seconds", recurrenceWindowSeconds,
	)
	// #207 audit log: webhook create. URL captured because rotating
	// a delivery target is a meaningful security event the customer
	// will want to verify against their own change-management.
	h.recordAuditEvent(r, AuditWebhookCreate, "webhook", webhookID, map[string]any{
		"name":                      body.Name,
		"url":                       body.URL,
		"enabled":                   enabled,
		"enabled_classes":           body.EnabledClasses,
		"severity_filter":           body.SeverityFilter,
		"recurrence_mode":           recurrenceMode,
		"recurrence_window_seconds": recurrenceWindowSeconds,
	})

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                        true,
		"webhook_id":                webhookID,
		"url":                       body.URL,
		"name":                      body.Name,
		"enabled_classes":           body.EnabledClasses,
		"enabled":                   enabled,
		"severity_filter":           body.SeverityFilter,
		"recurrence_mode":           recurrenceMode,
		"recurrence_window_seconds": recurrenceWindowSeconds,
		"secret":                    secret,
		"warning":                   "Store this secret now, it will never be shown again. Use it to verify the X-Mesedi-Signature header on inbound webhook deliveries.",
	})
}

// HandleDeleteWebhook hard-deletes a webhook. Project-scoped via the
// store method's project_id guard; cross-tenant id-guessing returns
// 404, not 403, to avoid leaking which ids exist.
func (h *Handlers) HandleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	webhookID := r.PathValue("id")
	if webhookID == "" {
		writeError(w, http.StatusBadRequest, "webhook_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	if err := h.Store.DeleteProjectWebhook(r.Context(), webhookID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.Logger.Info("webhook deleted",
		"webhook_id", webhookID,
		"project_id", authProjectID,
	)
	// #207 audit log: webhook delete is a write-tier action but a
	// silent disable of alerting can mask incidents, so it is worth
	// the audit row.
	h.recordAuditEvent(r, AuditWebhookDelete, "webhook", webhookID, nil)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"webhook_id": webhookID,
	})
}

// HandleTestWebhook fires a synthetic delivery against a webhook so an
// operator can verify the receiver is reachable and HMAC-verifying
// correctly. Blocks until the delivery resolves (delivered or failed
// after retries) so the response carries the outcome.
//
// Project-scoped: the webhook must belong to the calling project. The
// dashboard URL embedded in the payload is derived from the request's
// Host header, adequate for local-dev; a future slice will make this
// configurable via a flag/env var for production deployments.
func (h *Handlers) HandleTestWebhook(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "write") {
		return
	}
	webhookID := r.PathValue("id")
	if webhookID == "" {
		writeError(w, http.StatusBadRequest, "webhook_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	wh, err := h.Store.GetProjectWebhook(r.Context(), webhookID, authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup webhook: "+err.Error())
		return
	}

	// Dashboard base URL, configured via MESEDI_DASHBOARD_URL in prod
	// (e.g. https://app.mesedi.ai); falls back to request-derived
	// scheme + host for local-dev.
	dashboardBase := h.resolveDashboardBase(r)

	// Build a delivery_id up front so the payload echoes it back to the
	// receiver in the X-Mesedi-Event-Id header (the receiver can use it
	// for idempotency).
	rndBuf := make([]byte, 8)
	if _, err := rand.Read(rndBuf); err != nil {
		writeError(w, http.StatusInternalServerError, "generate delivery_id: "+err.Error())
		return
	}
	deliveryID := "del-" + hex.EncodeToString(rndBuf)

	payload := webhooks.BuildTestPayload(wh, dashboardBase, deliveryID)

	// Run delivery, synchronous for slice 2; slice 3's auto-fire path
	// will run this in a goroutine.
	result, attempts := webhooks.Deliver(r.Context(), h.Logger, h.WebhookClient, wh, payload)

	// Persist every attempt to the deliveries log (best-effort: a
	// persistence error here doesn't change the operator-visible
	// outcome, but does get logged).
	for i := range attempts {
		if err := h.Store.RecordWebhookDelivery(r.Context(), &attempts[i]); err != nil {
			h.Logger.Warn("record webhook delivery failed (continuing)",
				"webhook_id", webhookID,
				"attempt", attempts[i].Attempt,
				"error", err.Error(),
			)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          result.Status == "delivered",
		"webhook_id":  webhookID,
		"delivery_id": deliveryID,
		"status":      result.Status,
		"attempts":    result.Attempts,
		"http_status": result.HTTPStatus,
		"error":       result.Error,
		"duration_ms": result.DurationMs,
		"payload":     payload,
	})
}

// HandleListWebhookDeliveries returns the most recent delivery attempts
// for a webhook. Project-scoped via the webhook lookup. Default limit
// 50; capped at 200.
func (h *Handlers) HandleListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("id")
	if webhookID == "" {
		writeError(w, http.StatusBadRequest, "webhook_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	// Confirm webhook belongs to project before returning its log.
	if _, err := h.Store.GetProjectWebhook(r.Context(), webhookID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup webhook: "+err.Error())
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}

	deliveries, err := h.Store.ListDeliveriesForWebhook(r.Context(), webhookID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list deliveries: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"webhook_id": webhookID,
		"deliveries": deliveries,
		"count":      len(deliveries),
	})
}

// newWebhookID returns a short stable identifier for a webhook row.
// Format: "wh-<16-hex-chars>", readable in logs, sortable, no
// information leak about creation time. 64 bits of entropy is plenty
// for a per-project identifier space.
func newWebhookID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "wh-" + hex.EncodeToString(buf[:]), nil
}

// newWebhookSecret returns a 256-bit random secret as a 64-char hex
// string. Used as the HMAC key the dispatcher signs payloads with;
// the receiver verifies signatures using the same value.
func newWebhookSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

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
func scanForInjection(evts []*events.Event) (string, bool) {
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
		if name, found := detectors.DetectInjection(p.UserMessage); found {
			return name, true
		}
		if name, found := detectors.DetectInjection(p.SystemPrompt); found {
			return name, true
		}
	}
	return "", false
}

// isTerminalStatus returns true for any execution status that means
// "the agent run is over." Detection passes (time-budget, future
// drift / cost-velocity) only fire on terminal statuses, running
// executions don't have a final duration yet.
// isTerminalStatus delegates to events.ExecutionStatus.IsTerminal so
// `started` and `awaiting_human` (Mesedi #18) are both treated as
// non-terminal. The handler keeps its own thin wrapper so the
// detector-chain guard expressions stay readable at the call sites.
func isTerminalStatus(s events.ExecutionStatus) bool {
	return s.IsTerminal()
}

// isValidLifecycleTransition reports whether the (prior, next)
// transition is allowed by the Mesedi #18 state machine. The
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

// HandleGetProject returns the authenticated project's identity. Used
// by the dashboard to show "Project: <name>" in the topbar and welcome
// screens, and by /app/settings to display + (eventually) rename the
// project.
//
// Returns project_id, name, owner_email, created_at. Does not return
// the API key prefix or any sensitive material, the calling client
// already has the key in localStorage and any rename/revoke flows
// happen through other endpoints that already audit-log by key_id.
func (h *Handlers) HandleGetProject(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	project, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "get project: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"project_id":  project.ProjectID,
		"name":        project.Name,
		"owner_email": project.OwnerEmail,
		"created_at":  project.CreatedAt,
	})
}

// HandleSetProjectName updates the calling project's display name.
// Admin role required (anyone with read scope can call GET /project,
// but a rename is a mutation; only admins can perform it).
//
// Body: { "name": "<new name>" }
// Returns the same payload shape as HandleGetProject after the update.
//
// Validation:
//   - name is trimmed; must be 1-80 characters after trim
//   - empty / whitespace-only is rejected
//   - same 80-char cap as signup so a rename cannot bypass the
//     signup-time bound
//
// Added pre-#30 ship (#173): SSO signup defaults all projects to
// "Default project" because the OAuth flow does not collect a name.
// Customers need a way to rename without re-signing-up.
func (h *Handlers) HandleSetProjectName(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	var body struct {
		Name string `json:"name"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return
	}
	newName := strings.TrimSpace(body.Name)
	if newName == "" {
		writeError(w, http.StatusBadRequest,
			"name is required and cannot be only whitespace")
		return
	}
	if len(newName) > 80 {
		writeError(w, http.StatusBadRequest,
			"name must be 80 characters or fewer")
		return
	}
	if err := h.Store.UpdateProjectName(r.Context(), authProjectID, newName); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError,
			"update project name: "+err.Error())
		return
	}
	h.Logger.Info("project renamed",
		"project_id", authProjectID, "new_name", newName)
	// #207 step C — project rename is admin-tier and changes how the
	// project surfaces in invoices, emails, and the dashboard. Worth
	// the audit row.
	h.recordAuditEvent(r, AuditProjectRename, "project", authProjectID, map[string]any{
		"new_name": newName,
	})
	project, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"reload project after rename: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"project_id":  project.ProjectID,
		"name":        project.Name,
		"owner_email": project.OwnerEmail,
		"created_at":  project.CreatedAt,
	})
}

// HandleGetMe returns the calling user's identity + role for the
// dashboard. Used by the topbar / overview to show "Signed in as X"
// with the correct member email (not the project owner), and by every
// page to grey out mutation buttons when the role doesn't permit
// them. Without this endpoint, the dashboard renders mutation
// affordances unconditionally and the user only learns they lack
// permission after clicking.
//
// Response shape:
//
//	{
//	  "user_id":     "alice@example.com",  // who is calling
//	  "email":       "alice@example.com",  // same in v1 (email-as-user-id)
//	  "role":        "read",               // read | write | admin
//	  "project_id":  "proj_...",
//	  "project_name":"Acme Production",
//	  "owner_email": "owner@example.com"   // project creator, for context
//	}
//
// Legacy keys (no user_id) and projects without a tenant_id resolve
// to role=admin via the same fallback resolveCallerRole uses, so the
// founder's own integrations don't show "Signed in as null."
func (h *Handlers) HandleGetMe(w http.ResponseWriter, r *http.Request) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	project, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "get project: "+err.Error())
		return
	}

	// Resolve the caller's user_id from the API key (post-014 keys
	// carry it). Falls back to the project owner so legacy keys still
	// render meaningfully.
	userID, _ := UserIDFromContext(r.Context())
	if userID == "" {
		userID = project.OwnerUserID
		if userID == "" {
			userID = project.OwnerEmail
		}
	}

	role, err := h.resolveCallerRole(r)
	if err != nil {
		h.Logger.Warn("me: resolve role failed (defaulting to read)",
			"error", err.Error())
		role = "read"
	}
	if role == "" {
		role = "read"
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"user_id":      userID,
		"email":        userID,
		"role":         role,
		"project_id":   project.ProjectID,
		"project_name": project.Name,
		"owner_email":  project.OwnerEmail,
	})
}
