// Founder-side admin dashboard endpoints.
//
// These routes are NOT reachable from a customer's API key. Auth is a
// bearer token compared in constant time against MESEDI_ADMIN_TOKEN
// (set as a Fly secret). The token has full read access to every
// project's metadata + activity stats, plus (in later slices) the
// ability to manually flip tier and grant credits. No per-action audit
// trail in slice 1, log lines are the audit trail until traffic
// justifies a proper audit table.
//
// Security posture: a leaked admin token compromises the entire
// dataset. Treat it like a root password. Rotate by changing the env
// var (no DB state to invalidate). The middleware is constant-time
// against timing attacks via subtle.ConstantTimeCompare; it is NOT
// proof against an attacker who can read process memory or env vars.
//
// Why /admin/* and not /api/admin/* or /internal/*, the route prefix
// is just a routing convention; the actual gate is the middleware.
// Naming /admin is honest about purpose and makes log lines easier
// to grep.
//
// The handler bodies live in topical sidecars since the 2026-09-12
// split: admin_projects, admin_api_keys, admin_ai_analyses and
// admin_gdpr_audit. This file keeps the auth middleware and the
// route table.

package api

import (
	"context"
	"crypto/subtle"
	"errors"
	"net/http"
	"strings"
	"time"

	"mesedi/backend/internal/store"
)

// AdminAuth returns middleware that gates routes behind admin
// credentials. Two auth paths are accepted:
//
//  1. Legacy static token: the bearer value matches MESEDI_ADMIN_TOKEN
//     (constant-time compared). Retained for backward compatibility
//     during the migration to scoped API keys (follow-up). Will
//     be removed in a follow-up once every operator has minted an
//     admin-scope key for themselves.
//
//  2. Admin-scope API key: the bearer is a `mesedi_sk_...` token
//     whose api_keys row has scope='admin' and is not past its
//     expires_at. Looked up via store.GetAPIKeyByHash, same path as
//     customer auth. Lets us mint / revoke / expire admin credentials
//     without a redeploy + carries an audit identity (key_id, name).
//
// At least one of (adminToken != "", st != nil) must be set; if both
// are zero values the middleware fails closed with 503 so an
// accidentally-misconfigured deploy can't silently expose /admin/*.
//
// On success the request context is stamped with:
//   - ctxKeyAdminAuthMethod: "legacy_token" or "api_key"
//   - ctxKeyAdminKeyID:     key_id (api_key path only)
//   - ctxKeyAdminKeyName:   name   (api_key path only)
//
// Auth header shape: `Authorization: Bearer <token>`. Failures return
// 401 with an opaque message; we don't echo the token or describe
// which path failed (timing-and-information leak hygiene).
func AdminAuth(adminToken string, st store.Store) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" && st == nil {
				writeError(w, http.StatusServiceUnavailable, "admin not configured")
				return
			}
			hdr := r.Header.Get("Authorization")
			const prefix = "Bearer "
			if !strings.HasPrefix(hdr, prefix) {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			supplied := strings.TrimSpace(hdr[len(prefix):])

			// Path 1: legacy static token. Only attempted when the
			// supplied bearer does NOT look like a mesedi_sk_ key, so
			// we don't burn a (constant-time) comparison against the
			// legacy token on every customer-flavored request. The
			// prefix check is itself non-secret.
			if adminToken != "" && !strings.HasPrefix(supplied, "mesedi_sk_") {
				if subtle.ConstantTimeCompare([]byte(supplied), []byte(adminToken)) == 1 {
					ctx := context.WithValue(r.Context(), ctxKeyAdminAuthMethod, AdminAuthMethodLegacyToken)
					next.ServeHTTP(w, r.WithContext(ctx))
					return
				}
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}

			// Path 2: admin-scope API key. Hash the bearer, look up
			// the row, verify scope and expiry. Refuses identically
			// for missing key, wrong scope, or expired key so an
			// attacker can't distinguish the three failure modes.
			if st == nil {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			hash := HashAPIKey(supplied)
			key, err := st.GetAPIKeyByHash(r.Context(), hash)
			if err != nil {
				if errors.Is(err, store.ErrNotFound) {
					writeError(w, http.StatusUnauthorized, "unauthorized")
					return
				}
				writeError(w, http.StatusInternalServerError, "auth lookup failed")
				return
			}
			if key.Scope != store.APIKeyScopeAdmin {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			if !adminKeyExpiryOK(key.ExpiresAt) {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}

			// Touch last_used_at asynchronously, fire-and-forget so a
			// slow DB write doesn't add latency. Same pattern as the
			// customer authMiddleware.
			go func(keyID string) {
				_ = st.TouchAPIKey(context.Background(), keyID)
			}(key.KeyID)

			ctx := context.WithValue(r.Context(), ctxKeyAdminAuthMethod, AdminAuthMethodAPIKey)
			ctx = context.WithValue(ctx, ctxKeyAdminKeyID, key.KeyID)
			ctx = context.WithValue(ctx, ctxKeyAdminKeyName, key.Name)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// adminKeyExpiryOK returns true if the supplied expires_at column is
// empty (never expires) or parses to a time in the future. Any parse
// failure is treated as expired (fail closed) so a malformed value
// can't accidentally extend a credential's lifetime.
func adminKeyExpiryOK(expiresAt string) bool {
	expiresAt = strings.TrimSpace(expiresAt)
	if expiresAt == "" {
		return true
	}
	t, err := time.Parse(time.RFC3339Nano, expiresAt)
	if err != nil {
		// Try RFC3339 without sub-second precision before giving up.
		t, err = time.Parse(time.RFC3339, expiresAt)
		if err != nil {
			return false
		}
	}
	return time.Now().UTC().Before(t)
}

// RegisterAdminRoutes attaches the founder-side admin endpoints to the
// supplied mux. The caller is responsible for wrapping the mux with
// AdminAuth, RegisterAdminRoutes itself is unauthenticated so unit
// tests can hit the handlers directly without faking the token.
func (h *Handlers) RegisterAdminRoutes(mux *http.ServeMux) {
	mux.HandleFunc("GET /admin/projects", h.HandleAdminListProjects)
	mux.HandleFunc("GET /admin/projects/{id}", h.HandleAdminGetProjectDetail)
	mux.HandleFunc("POST /admin/projects/{id}/tier", h.HandleAdminSetTier)
	mux.HandleFunc("POST /admin/projects/{id}/grant", h.HandleAdminGrantExecutions)
	// Smoke-harness admin trigger endpoints for billing schedulers.
	// See admin_billing_triggers.go for scope. Test-mode overrides
	// (billing_test_overrides.go) make these safe to call against
	// tiny synthetic quotas on staging.
	mux.HandleFunc("POST /admin/projects/{id}/trigger-hobby-billing-run", h.HandleAdminTriggerHobbyBillingRun)
	mux.HandleFunc("POST /admin/projects/{id}/trigger-team-billing-run", h.HandleAdminTriggerTeamBillingRun)
	// Reset the period counter for a project (harness state hygiene
	// between smoke runs). See admin_billing_triggers.go for scope.
	mux.HandleFunc("POST /admin/projects/{id}/reset-period-counter", h.HandleAdminResetPeriodCounter)
	mux.HandleFunc("GET /admin/projects/{id}/export", h.HandleAdminExportProject)
	mux.HandleFunc("DELETE /admin/projects/{id}", h.HandleAdminDeleteProject)
	mux.HandleFunc("DELETE /admin/projects/{id}/failure-groups", h.HandleAdminResetFailureGroups)
	mux.HandleFunc("GET /admin/ai-analyses-by-project", h.HandleAdminAIAnalysesByProject)
	// per-project breakdown of WHICH failure groups generated the
	// count, surfaced when the admin expands a project row.
	mux.HandleFunc("GET /admin/projects/{id}/ai-analyses-detail", h.HandleAdminProjectAIAnalysesDetail)
	// flat list of every analysis ever run + lifetime/month
	// totals. Powers the founder cost-attribution surface.
	mux.HandleFunc("GET /admin/ai-analyses", h.HandleAdminListAIAnalyses)
	mux.HandleFunc("GET /admin/ai-analyses-totals", h.HandleAdminGetAIAnalysesTotals)
	// Stripe-side analytics + accounting (MRR/ARR, charges,
	// refunds, churn).
	mux.HandleFunc("GET /admin/analytics-summary", h.HandleAdminGetAnalyticsSummary)
	mux.HandleFunc("GET /admin/charges", h.HandleAdminListCharges)
	mux.HandleFunc("GET /admin/refunds", h.HandleAdminListRefunds)
	mux.HandleFunc("GET /admin/subscriptions-canceled", h.HandleAdminListCanceledSubscriptions)
	// anonymized failure_class aggregates for LinkedIn trend
	// reports. GET returns publishable rows (k-anonymity gated),
	// POST re-runs aggregation for a given month.
	mux.HandleFunc("GET /admin/failure-class-aggregates", h.HandleAdminListFailureClassAggregates)
	mux.HandleFunc("POST /admin/failure-class-aggregates/run", h.HandleAdminRunFailureClassAggregation)
	// Anthropic credit balance + 7-day burn rate. GET returns
	// the latest manually-entered balance + programmatic burn rate;
	// POST accepts a new manually-entered balance snapshot.
	mux.HandleFunc("GET /admin/anthropic-credit", h.HandleAdminGetAnthropicCredit)
	mux.HandleFunc("POST /admin/anthropic-credit", h.HandleAdminCreateAnthropicCreditSnapshot)
	mux.HandleFunc("GET /admin/storage", h.HandleAdminStorage)
	mux.HandleFunc("GET /admin/abuse", h.HandleAdminListAbuseSignals)
	mux.HandleFunc("POST /admin/abuse/{id}/resolve", h.HandleAdminResolveAbuseSignal)
	// Stripe webhook billing-event signals (chargebacks +
	// dunning). Read-only list + resolve-with-note. Backs the
	// /security page commitment of fraud/dunning surfacing in
	// admin without polling Stripe.
	mux.HandleFunc("GET /admin/billing-events", h.HandleAdminListBillingEvents)
	mux.HandleFunc("POST /admin/billing-events/{id}/resolve", h.HandleAdminResolveBillingEvent)
	// API keys management (migration 015).
	mux.HandleFunc("GET /admin/api-keys", h.HandleAdminListAPIKeys)
	mux.HandleFunc("POST /admin/api-keys", h.HandleAdminCreateAPIKey)
	mux.HandleFunc("DELETE /admin/api-keys/{id}", h.HandleAdminRevokeAPIKey)
	// "Mark key compromised" admin action. Admin-scope keys only;
	// records abuse signal, suspends project, revokes key, returns
	// recent-use report for the operator to email to the customer.
	mux.HandleFunc("POST /admin/api-keys/{id}/mark-compromised", h.HandleAdminMarkKeyCompromised)
	mux.HandleFunc("GET /admin/whoami", h.HandleAdminWhoami)
	// closed-project audit search. R1 takeover forensics + R2
	// customer-support response. Read-only; rows are written by the
	// snapshot call inside HandleCloseAccount (migration 031).
	mux.HandleFunc("GET /admin/audit-events", h.HandleAdminSearchClosedProjectAudit)
	// GDPR Article 17 purge endpoint. Hard-deletes every audit
	// row owned by the supplied projectID; writes a meta-audit-event
	// against the _admin system project so we keep a paper trail of
	// the deletion. Refuses to operate on live projects (422).
	mux.HandleFunc("POST /admin/projects/{id}/audit-events/purge", h.HandleAdminGDPRPurgeClosedProjectAudit)
}

// ─────────────────────────────────────────────────────────────────────────
// Admin API key management (migration 015).
//
// The /admin/api-keys surface lets an admin-authenticated operator
// mint, list, and revoke any API key in the system. Two key scopes:
//
//   - customer: project-scoped, identical to keys minted from the
//     customer-facing /api-keys page. The operator picks which project
//     the key belongs to. Useful for support-side reissuing a key on
//     behalf of a project.
//
//   - admin: privileged, scope='admin', project_id='_admin'. Bearer
//     value passes AdminAuth and reaches /admin/*. Operators mint
//     these for themselves to replace the legacy MESEDI_ADMIN_TOKEN
//     Fly secret.
//
// Mint flow returns the raw secret ONCE in the response; subsequent
// list calls only ever see the prefix. Revoke is a hard delete.
// ─────────────────────────────────────────────────────────────────────────

// HandleAdminWhoami returns the identity the request authenticated as.
// Used by the dashboard to (a) confirm admin auth succeeded and (b)
// show "you are revoking the key you are currently using" warnings.
func (h *Handlers) HandleAdminWhoami(w http.ResponseWriter, r *http.Request) {
	method, _ := AdminAuthMethodFromContext(r.Context())
	resp := map[string]any{
		"auth_method": method,
		"is_admin":    true,
	}
	if keyID, ok := AdminKeyIDFromContext(r.Context()); ok {
		resp["key_id"] = keyID
	}
	if name, ok := AdminKeyNameFromContext(r.Context()); ok && name != "" {
		resp["name"] = name
	}
	writeJSON(w, http.StatusOK, resp)
}

// --- Anthropic credit + burn rate ----------------------------

// --- AI analyses flat list + lifetime totals -----------------
