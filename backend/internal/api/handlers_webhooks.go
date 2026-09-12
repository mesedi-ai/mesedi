// Webhook configuration, test delivery and delivery-log endpoints.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"mesedi/backend/internal/severity"
	"mesedi/backend/internal/store"
	"mesedi/backend/internal/webhooks"
)

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
	store.FailureClassCrashes:              {},
	store.FailureClassLoops:                {},
	store.FailureClassToolFailures:         {},
	store.FailureClassValidator:            {},
	store.FailureClassDrift:                {},
	store.FailureClassCostVelocity:         {},
	store.FailureClassInjection:            {},
	store.FailureClassInfraThrottled:       {},
	store.FailureClassDataLeakage:          {},
	store.FailureClassSemanticLoop:         {},
	store.FailureClassToolSchemaDrift:      {},
	store.FailureClassContextOverflow:      {},
	store.FailureClassTokenWaste:           {},
	store.FailureClassSandboxEscape:        {},
	store.FailureClassGroundingFailure:     {},
	store.FailureClassCascadingFailure:     {},
	store.FailureClassCoordinationDeadlock: {},
	store.FailureClassProviderIncident:     {},
	store.FailureClassHITLTimeout:          {},
	store.FailureClassHITLRejectionSpike:   {},
	store.FailureClassRecordIntegrity:      {},
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
		// webhook should fire on. Empty/omitted = fire on every
		// severity (backward compatible). Unknown tokens are dropped
		// by severity.ParseFilter, but we explicitly validate here so
		// a typo doesn't silently produce a fire-on-nothing webhook.
		SeverityFilter string `json:"severity_filter,omitempty"`
		// RecurrenceMode picks whether (and how) the webhook fires on
		// recurrences of an existing failure group. One of
		// "off" | "every_event" | "throttled". Empty/omitted defaults
		// to "off" so legacy clients see the behavior.
		RecurrenceMode string `json:"recurrence_mode,omitempty"`
		// RecurrenceWindowSeconds is required only when
		// RecurrenceMode is "throttled". Below the 60s floor the
		// dispatcher promotes the value to 60.
		RecurrenceWindowSeconds int `json:"recurrence_window_seconds,omitempty"`
		// AuthToken is a customer-provided receiver-side auth value
		// for receivers that don't use Mesedi's HMAC signing.
		// Currently required for PagerDuty (their routing_key /
		// integration key), optional for anything else. See
		// tier_change_cascade.go and adapters.go for the routing.
		AuthToken string `json:"auth_token,omitempty"`
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

	// PagerDuty enforcement: their Events API v2 authenticates via a
	// routing_key inside the request body, not via HTTP headers, so
	// the customer MUST supply their PagerDuty integration key here.
	// Without it, every delivery would silently be rejected by
	// PagerDuty with an unhelpful 400. Fail loud at create-time
	// instead. Length range guards against a fat-finger paste (real
	// keys are 32 hex-ish characters); the outer bounds are
	// deliberately loose because PagerDuty has changed their key
	// format across API versions.
	body.AuthToken = strings.TrimSpace(body.AuthToken)
	if webhooks.IsPagerDutyReceiver(body.URL) {
		if body.AuthToken == "" {
			writeError(w, http.StatusBadRequest,
				"PagerDuty webhooks require the integration key (routing_key) in the auth_token field")
			return
		}
		if len(body.AuthToken) < 20 || len(body.AuthToken) > 128 {
			writeError(w, http.StatusBadRequest,
				"auth_token length looks wrong for a PagerDuty integration key (expected ~32 chars)")
			return
		}
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

	// Validate severity_filter. Normalize whitespace + case,
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

	// Validate recurrence_mode. Empty input defaults to "off"
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
		AuthToken:               body.AuthToken,
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
	// audit log: webhook create. URL captured because rotating
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
	// audit log: webhook delete is a write-tier action but a
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
