// Founder-side admin dashboard endpoints (#150).
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

package api

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"mesedi/backend/internal/anthropic"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

// AdminAuth returns middleware that gates routes behind admin
// credentials. Two auth paths are accepted:
//
//  1. Legacy static token: the bearer value matches MESEDI_ADMIN_TOKEN
//     (constant-time compared). Retained for backward compatibility
//     during the migration to scoped API keys (#150 follow-up). Will
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
	mux.HandleFunc("GET /admin/projects/{id}/export", h.HandleAdminExportProject)
	mux.HandleFunc("DELETE /admin/projects/{id}", h.HandleAdminDeleteProject)
	mux.HandleFunc("DELETE /admin/projects/{id}/failure-groups", h.HandleAdminResetFailureGroups)
	mux.HandleFunc("GET /admin/ai-analyses-by-project", h.HandleAdminAIAnalysesByProject)
	// #211 per-project breakdown of WHICH failure groups generated the
	// count, surfaced when the admin expands a project row.
	mux.HandleFunc("GET /admin/projects/{id}/ai-analyses-detail", h.HandleAdminProjectAIAnalysesDetail)
	// #199 flat list of every analysis ever run + lifetime/month
	// totals. Powers the founder cost-attribution surface.
	mux.HandleFunc("GET /admin/ai-analyses", h.HandleAdminListAIAnalyses)
	mux.HandleFunc("GET /admin/ai-analyses-totals", h.HandleAdminGetAIAnalysesTotals)
	// #202 Stripe-side analytics + accounting (MRR/ARR, charges,
	// refunds, churn).
	mux.HandleFunc("GET /admin/analytics-summary", h.HandleAdminGetAnalyticsSummary)
	mux.HandleFunc("GET /admin/charges", h.HandleAdminListCharges)
	mux.HandleFunc("GET /admin/refunds", h.HandleAdminListRefunds)
	mux.HandleFunc("GET /admin/subscriptions-canceled", h.HandleAdminListCanceledSubscriptions)
	// #212 anonymized failure_class aggregates for LinkedIn trend
	// reports. GET returns publishable rows (k-anonymity gated),
	// POST re-runs aggregation for a given month.
	mux.HandleFunc("GET /admin/failure-class-aggregates", h.HandleAdminListFailureClassAggregates)
	mux.HandleFunc("POST /admin/failure-class-aggregates/run", h.HandleAdminRunFailureClassAggregation)
	// #198 Anthropic credit balance + 7-day burn rate. GET returns
	// the latest manually-entered balance + programmatic burn rate;
	// POST accepts a new manually-entered balance snapshot.
	mux.HandleFunc("GET /admin/anthropic-credit", h.HandleAdminGetAnthropicCredit)
	mux.HandleFunc("POST /admin/anthropic-credit", h.HandleAdminCreateAnthropicCreditSnapshot)
	mux.HandleFunc("GET /admin/storage", h.HandleAdminStorage)
	mux.HandleFunc("GET /admin/abuse", h.HandleAdminListAbuseSignals)
	mux.HandleFunc("POST /admin/abuse/{id}/resolve", h.HandleAdminResolveAbuseSignal)
	// API keys management (migration 015).
	mux.HandleFunc("GET /admin/api-keys", h.HandleAdminListAPIKeys)
	mux.HandleFunc("POST /admin/api-keys", h.HandleAdminCreateAPIKey)
	mux.HandleFunc("DELETE /admin/api-keys/{id}", h.HandleAdminRevokeAPIKey)
	mux.HandleFunc("GET /admin/whoami", h.HandleAdminWhoami)
	// #221 closed-project audit search. R1 takeover forensics + R2
	// customer-support response. Read-only; rows are written by the
	// snapshot call inside HandleCloseAccount (migration 031).
	mux.HandleFunc("GET /admin/audit-events", h.HandleAdminSearchClosedProjectAudit)
}

// AdminProjectDetail bundles everything the founder dashboard's
// drill-down page needs in a single round trip: the project itself
// plus its recent executions, recent failure_groups, webhooks, and
// API key prefixes (never the hash). Keeps the dashboard's network
// surface small (one fetch on page mount, not four).
type AdminProjectDetail struct {
	Project             *store.AdminProjectRow  `json:"project"`
	RecentExecutions    []*events.Execution     `json:"recent_executions"`
	RecentFailureGroups []*store.FailureGroup   `json:"recent_failure_groups"`
	Webhooks            []*store.ProjectWebhook `json:"webhooks"`
	APIKeys             []*store.APIKey         `json:"api_keys"`
}

// HandleAdminGetProjectDetail returns the bundled drill-down payload
// for a single project. Composes 5 store calls (the AdminProjectRow
// list filtered to the requested id, plus four per-project list
// methods); the dashboard renders whichever sections have data.
//
// 404 when the project doesn't exist; 500 for downstream errors.
// Each sub-list is capped at 20 rows, enough to see "is this customer
// active" without dragging the whole table into memory.
func (h *Handlers) HandleAdminGetProjectDetail(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}
	ctx := r.Context()

	// Resolve the project. We re-use ListAllProjects + filter so the
	// payload's `project` field includes the same activity aggregates
	// (last_activity_at, total_executions) the list page shows.
	all, err := h.Store.ListAllProjects(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list projects: "+err.Error())
		return
	}
	var project *store.AdminProjectRow
	for _, p := range all {
		if p.ProjectID == projectID {
			project = p
			break
		}
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	// The four sub-lists are independent reads; on any single one
	// failing we log + continue with an empty slice rather than 500-ing
	// the whole detail page. Founder needs to see SOMETHING even if
	// one source is unhappy.
	executions, err := h.Store.ListExecutions(ctx, projectID, 20, 0)
	if err != nil {
		h.Logger.Warn("admin: list executions failed", "project_id", projectID, "error", err.Error())
		executions = nil
	}
	failureGroups, err := h.Store.ListFailureGroups(ctx, projectID, 20, 0)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		h.Logger.Warn("admin: list failure groups failed", "project_id", projectID, "error", err.Error())
		failureGroups = nil
	}
	webhooks, err := h.Store.ListProjectWebhooksForProject(ctx, projectID)
	if err != nil {
		h.Logger.Warn("admin: list webhooks failed", "project_id", projectID, "error", err.Error())
		webhooks = nil
	}
	apiKeys, err := h.Store.ListAPIKeysForProject(ctx, projectID)
	if err != nil {
		h.Logger.Warn("admin: list api keys failed", "project_id", projectID, "error", err.Error())
		apiKeys = nil
	}

	writeJSON(w, http.StatusOK, AdminProjectDetail{
		Project:             project,
		RecentExecutions:    executions,
		RecentFailureGroups: failureGroups,
		Webhooks:            webhooks,
		APIKeys:             apiKeys,
	})
}

// AdminProjectExport is the full data archive shape returned by the
// export endpoint. Customers requesting their data under the Privacy
// Policy get this exact JSON. Schema-version field lets future
// consumers detect format changes without guessing.
type AdminProjectExport struct {
	SchemaVersion int                      `json:"schema_version"`
	ExportedAt    time.Time                `json:"exported_at"`
	Project       *store.AdminProjectRow   `json:"project"`
	APIKeys       []*store.APIKey          `json:"api_keys"`
	Executions    []*ExportedExecution     `json:"executions"`
	FailureGroups []*store.FailureGroup    `json:"failure_groups"`
	Webhooks      []*store.ProjectWebhook  `json:"webhooks"`
	Deliveries    []*store.WebhookDelivery `json:"webhook_deliveries"`
}

// ExportedExecution is one execution with its events inlined. Saves
// the consumer from cross-referencing two arrays by execution_id.
type ExportedExecution struct {
	Execution *events.Execution `json:"execution"`
	Events    []*events.Event   `json:"events"`
}

// HandleAdminExportProject returns a JSON archive of every row
// associated with a project. Used to honor the Privacy Policy's
// data-export right. Response is served as a file download via the
// Content-Disposition header so a browser GET hits the file picker
// rather than dumping JSON into a tab.
//
// No pagination, exports are infrequent and the founder controls
// when they run. Memory budget is the natural ceiling: a project
// with 100M events shouldn't run this from the dashboard. If that
// becomes a real constraint, switch to streaming JSON Lines and
// chunk it.
func (h *Handlers) HandleAdminExportProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}
	ctx := r.Context()

	// Reuse ListAllProjects + filter (same pattern as the detail
	// handler). Gives the export consistent activity stats.
	all, err := h.Store.ListAllProjects(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list projects: "+err.Error())
		return
	}
	var project *store.AdminProjectRow
	for _, p := range all {
		if p.ProjectID == projectID {
			project = p
			break
		}
	}
	if project == nil {
		writeError(w, http.StatusNotFound, "project not found")
		return
	}

	apiKeys, err := h.Store.ListAPIKeysForProject(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list api keys: "+err.Error())
		return
	}

	// 1,000,000 is "all" for any project at our current scale. If
	// real customers ever exceed this we'll add streaming.
	executions, err := h.Store.ListExecutions(ctx, projectID, 1_000_000, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list executions: "+err.Error())
		return
	}
	exportedExecutions := make([]*ExportedExecution, 0, len(executions))
	for _, e := range executions {
		evts, err := h.Store.ListEventsForExecution(ctx, e.ExecutionID)
		if err != nil {
			// Log but keep going, a missing events list shouldn't
			// fail the whole export.
			h.Logger.Warn("admin export: list events failed",
				"execution_id", e.ExecutionID, "error", err.Error())
			evts = nil
		}
		exportedExecutions = append(exportedExecutions, &ExportedExecution{
			Execution: e,
			Events:    evts,
		})
	}

	failureGroups, err := h.Store.ListFailureGroups(ctx, projectID, 1_000_000, 0)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError, "list failure groups: "+err.Error())
		return
	}

	webhooks, err := h.Store.ListProjectWebhooksForProject(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list webhooks: "+err.Error())
		return
	}

	// Aggregate deliveries across every webhook so the export shows
	// the full delivery history.
	deliveries := []*store.WebhookDelivery{}
	for _, wh := range webhooks {
		ds, err := h.Store.ListDeliveriesForWebhook(ctx, wh.WebhookID, 100_000)
		if err != nil {
			h.Logger.Warn("admin export: list deliveries failed",
				"webhook_id", wh.WebhookID, "error", err.Error())
			continue
		}
		deliveries = append(deliveries, ds...)
	}

	archive := AdminProjectExport{
		SchemaVersion: 1,
		ExportedAt:    time.Now().UTC(),
		Project:       project,
		APIKeys:       apiKeys,
		Executions:    exportedExecutions,
		FailureGroups: failureGroups,
		Webhooks:      webhooks,
		Deliveries:    deliveries,
	}

	h.Logger.Info("admin: project exported",
		"project_id", projectID,
		"executions", len(exportedExecutions),
		"failure_groups", len(failureGroups),
		"webhooks", len(webhooks),
	)

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set(
		"Content-Disposition",
		`attachment; filename="mesedi-export-`+sanitizeFilename(projectID)+`.json"`,
	)
	w.WriteHeader(http.StatusOK)
	if err := json.NewEncoder(w).Encode(archive); err != nil {
		h.Logger.Error("admin export: encode failed", "error", err.Error())
	}
}

// sanitizeFilename strips anything that isn't alphanumeric or
// underscore/dash so the Content-Disposition header can never
// inject a quote that breaks the header or a path traversal.
func sanitizeFilename(s string) string {
	out := make([]byte, 0, len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '_', r == '-':
			out = append(out, byte(r))
		default:
			out = append(out, '_')
		}
	}
	if len(out) == 0 {
		return "project"
	}
	return string(out)
}

// HandleAdminDeleteProject permanently deletes a project and every
// row tied to it. Used to honor the Privacy Policy's data-deletion
// right. Refuses the delete when the project still has an active
// Stripe subscription, the admin must cancel via the Stripe
// Dashboard first, partly to avoid silently dropping a paying
// customer and partly because Stripe records are retained for
// accounting regardless of our local state.
//
// Required confirmation: ?confirm=<project_name> must exactly match
// the project's name. This is the same typed-confirmation idiom
// GitHub and Linear use for destructive ops. Spelling matters.
func (h *Handlers) HandleAdminDeleteProject(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}
	ctx := r.Context()

	p, err := h.Store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}

	confirm := r.URL.Query().Get("confirm")
	if confirm == "" || confirm != p.Name {
		writeError(
			w,
			http.StatusBadRequest,
			"confirmation mismatch, pass ?confirm=<exact project name>",
		)
		return
	}

	// Refuse if a Stripe subscription is still tied to this project.
	// The admin should cancel via Stripe Dashboard first so the
	// customer doesn't get charged after we wipe local state.
	if p.StripeSubscriptionID != "" {
		writeError(
			w,
			http.StatusConflict,
			"project has an active Stripe subscription ("+p.StripeSubscriptionID+
				"). Cancel it via the Stripe Dashboard before deleting.",
		)
		return
	}

	if err := h.Store.DeleteProject(ctx, projectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "delete project: "+err.Error())
		return
	}
	h.Logger.Info("admin: project deleted",
		"project_id", projectID,
		"project_name", p.Name,
		"owner_email", p.OwnerEmail,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"project_id": projectID,
	})
}

// HandleAdminResetFailureGroups wipes every failure_group row for a
// single project so the next detector pass re-creates each one with
// isNew=true, which in turn triggers the webhook dispatcher to fire
// fresh failure_group.created events to any configured receivers
// (Discord, Slack, etc.). Used to reset a demo project between
// recording sessions or after a long-running synthetic-org build has
// already populated all the canonical signatures.
//
// Executions, events, and webhook_deliveries are NOT touched, only
// the grouping summary rows. The action is therefore reversible in
// the sense that the underlying execution history remains intact,
// the next round of detectors will regenerate identical groups.
//
// Required confirmation: ?confirm=<project_name> must exactly match
// the project's name, same idiom as HandleAdminDeleteProject. This
// is destructive enough (a paying customer loses their alert history
// summary view) that a typed confirmation is appropriate.
func (h *Handlers) HandleAdminResetFailureGroups(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}
	ctx := r.Context()

	p, err := h.Store.GetProject(ctx, projectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "load project: "+err.Error())
		return
	}

	confirm := r.URL.Query().Get("confirm")
	if confirm == "" || confirm != p.Name {
		writeError(
			w,
			http.StatusBadRequest,
			"confirmation mismatch, pass ?confirm=<exact project name>",
		)
		return
	}

	deleted, err := h.Store.DeleteFailureGroupsByProject(ctx, projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "reset failure_groups: "+err.Error())
		return
	}
	h.Logger.Info("admin: failure_groups reset",
		"project_id", projectID,
		"project_name", p.Name,
		"deleted", deleted,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"project_id":   projectID,
		"project_name": p.Name,
		"deleted":      deleted,
	})
}

// adminSetTierRequest is the JSON body for POST /admin/projects/{id}/tier.
// ExpiresDays > 0 means "this tier reverts to Hobby after N days from
// now"; 0 or negative means "permanent". Always re-applied on every
// call, pass 0 explicitly to clear a previously-set expiration.
type adminSetTierRequest struct {
	Tier        string `json:"tier"`
	ExpiresDays int    `json:"expires_days,omitempty"`
}

// HandleAdminSetTier manually flips a project's tier. Bypasses Stripe
// entirely, does NOT cancel an existing subscription if dropping
// Pro to Hobby. The founder is expected to handle Stripe cleanup
// separately (manually in the Stripe Dashboard, or via the customer's
// own Manage subscription link). This is a deliberate "founder knows
// what they're doing" design choice; an automated subscription cancel
// here would be too easy to misfire.
func (h *Handlers) HandleAdminSetTier(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req adminSetTierRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	tier := strings.ToLower(strings.TrimSpace(req.Tier))
	// Accept the legacy "pro" alias from older admin scripts but
	// normalize to "team" before writing. The dashboard's tier-flip
	// dropdown sends the new name; the legacy fallback is defensive.
	tier = normalizeTier(tier)
	switch tier {
	case TierHobby, TierTeam, TierEnterprise:
		// ok
	default:
		writeError(w, http.StatusBadRequest, "tier must be hobby, team, or enterprise")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresDays > 0 {
		t := time.Now().UTC().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
		expiresAt = &t
	}
	// Load the project BEFORE the update so the audit row can record
	// the from_tier value. Best-effort: a load failure is logged but
	// does not block the tier change; the audit row will then carry
	// from_tier="" and the customer still sees the change happened.
	var fromTier string
	if p, gerr := h.Store.GetProject(context.Background(), projectID); gerr == nil && p != nil {
		fromTier = p.Tier
	}
	if err := h.Store.UpdateProjectTier(context.Background(), projectID, tier, expiresAt); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "update tier: "+err.Error())
		return
	}
	h.Logger.Info("admin: tier set",
		"project_id", projectID,
		"new_tier", tier,
		"expires_days", req.ExpiresDays,
	)
	// #207 step C / PL4 — surface platform-admin tier changes in the
	// customer's own audit log so they can attribute a sudden tier
	// flip back to a Mesedi-side action rather than silently to
	// "someone in your org." Actor is the synthetic
	// AuditActorPlatformAdmin sentinel; the specific staff identity
	// is intentionally not exposed to the customer.
	tierMeta := map[string]any{
		"from_tier":    fromTier,
		"to_tier":      tier,
		"expires_days": req.ExpiresDays,
	}
	if expiresAt != nil {
		tierMeta["expires_at"] = expiresAt.UTC().Format(time.RFC3339)
	}
	h.recordAuditEventForProject(
		context.Background(),
		projectID,
		AuditActorPlatformAdmin,
		AuditTierChangeByPlatformAdmin,
		"project",
		projectID,
		tierMeta,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"project_id": projectID,
		"tier":       tier,
		"expires_at": expiresAt,
	})
}

// adminGrantRequest is the JSON body for POST /admin/projects/{id}/grant.
// Executions is signed so the admin can revoke a grant by passing a
// negative value through the same endpoint. ExpiresDays > 0 sets a
// grant expiration N days from now; 0 means "never expires". Each
// call overwrites the previous expiration (single-grant model).
type adminGrantRequest struct {
	Executions  int64 `json:"executions"`
	ExpiresDays int   `json:"expires_days,omitempty"`
}

// HandleAdminGrantExecutions adjusts a project's granted_executions
// column. Positive Executions grants quota (e.g., 100,000 to promote
// an early signup); negative Executions revokes a prior grant.
//
// Guardrails: cap the absolute delta at 10,000,000 to make finger-
// flubs cheaper. A single grant > 10M is almost certainly a typo;
// the admin can issue multiple grants if they really need more.
func (h *Handlers) HandleAdminGrantExecutions(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "missing project id")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if err != nil {
		writeError(w, http.StatusBadRequest, "read body: "+err.Error())
		return
	}
	var req adminGrantRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}
	// Executions == 0 is allowed ONLY if the caller is updating the
	// expiration of an existing grant. Without that exception we'd
	// reject a no-op-delta request the admin uses to extend (or
	// clear) the expiration of a previously-granted bonus.
	if req.Executions == 0 && req.ExpiresDays == 0 {
		writeError(w, http.StatusBadRequest, "executions must be non-zero, or expires_days must be set")
		return
	}
	const maxAbs = int64(10_000_000)
	if req.Executions > maxAbs || req.Executions < -maxAbs {
		writeError(w, http.StatusBadRequest, "executions delta exceeds 10M guardrail")
		return
	}
	var expiresAt *time.Time
	if req.ExpiresDays > 0 {
		t := time.Now().UTC().Add(time.Duration(req.ExpiresDays) * 24 * time.Hour)
		expiresAt = &t
	}
	if err := h.Store.AddGrantedExecutions(context.Background(), projectID, req.Executions, expiresAt); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "grant executions: "+err.Error())
		return
	}
	h.Logger.Info("admin: executions granted",
		"project_id", projectID,
		"delta", req.Executions,
		"expires_days", req.ExpiresDays,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"project_id": projectID,
		"delta":      req.Executions,
		"expires_at": expiresAt,
	})
}

// AdminStorageResponse is the JSON payload for GET /admin/storage.
// Bundles disk-level numbers (DB file size, volume capacity) with
// per-project breakdown so the founder can spot heavy users before
// the SQLite volume fills.
type AdminStorageResponse struct {
	// DatabaseFileBytes is the size of /data/mesedi.db on disk.
	DatabaseFileBytes int64 `json:"database_file_bytes"`
	// VolumeTotalBytes is the size of the mounted volume.
	VolumeTotalBytes int64 `json:"volume_total_bytes"`
	// VolumeFreeBytes is the available space on the mounted volume.
	VolumeFreeBytes int64 `json:"volume_free_bytes"`
	// VolumeMountPath is the path we measured. Empty when not
	// running on a real volume mount (local dev).
	VolumeMountPath string `json:"volume_mount_path,omitempty"`
	// Projects is the per-project breakdown sorted by estimated
	// bytes descending, biggest consumers first.
	Projects []*store.ProjectStorage `json:"projects"`
}

// HandleAdminStorage returns DB file size, volume capacity, and a
// per-project storage breakdown. The founder uses this to decide
// when to extend the Fly volume, when to dunning the heaviest
// users, or whether a single customer is hot-spotting disk.
//
// Volume measurement uses syscall.Statfs against the parent
// directory of the DB file. On local dev (when /data doesn't
// exist) this returns the host filesystem stats, useful enough
// for debugging without special-casing.
func (h *Handlers) HandleAdminStorage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()

	// Per-project breakdown via SQL.
	projects, err := h.Store.GetProjectStorageStats(ctx)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "storage stats: "+err.Error())
		return
	}

	// DB file size: stat the main file. Fly volume is mounted at
	// /data; in dev the DB lives under the working directory. Try
	// both and use whichever resolves.
	resp := AdminStorageResponse{Projects: projects}
	for _, candidate := range []string{
		"/data/mesedi.db",
		"./mesedi-dev.db",
	} {
		info, err := os.Stat(candidate)
		if err == nil {
			resp.DatabaseFileBytes = info.Size()
			resp.VolumeMountPath = candidate
			break
		}
	}

	// Volume capacity: statfs on the directory holding the DB. If
	// we couldn't locate the DB above, fall back to /data so we
	// still report the volume even if the file is missing.
	statfsTarget := "/data"
	if resp.VolumeMountPath != "" {
		// strip the filename, Statfs wants a directory
		if idx := strings.LastIndex(resp.VolumeMountPath, "/"); idx > 0 {
			statfsTarget = resp.VolumeMountPath[:idx]
		}
	}
	var stat syscall.Statfs_t
	if err := syscall.Statfs(statfsTarget, &stat); err == nil {
		// nolint:gosec, Bsize and Blocks are platform-dependent
		// signed/unsigned; the cast is safe at our volume sizes.
		resp.VolumeTotalBytes = int64(stat.Bsize) * int64(stat.Blocks)
		resp.VolumeFreeBytes = int64(stat.Bsize) * int64(stat.Bavail)
	} else {
		h.Logger.Warn("admin storage: statfs failed",
			"path", statfsTarget, "error", err.Error())
	}

	writeJSON(w, http.StatusOK, resp)
}

// HandleAdminListProjects returns every project in the database with
// activity aggregates. Used by the founder dashboard's /admin landing
// page. The shape mirrors store.AdminProjectRow exactly; the dashboard
// renders the fields it cares about (table) and ignores the rest.
//
// No pagination yet. At founder-traffic scale (low thousands of
// signups) the full list fits in a single response comfortably.
// Pagination + filtering becomes a slice 3+ concern.
func (h *Handlers) HandleAdminListProjects(w http.ResponseWriter, r *http.Request) {
	rows, err := h.Store.ListAllProjects(r.Context())
	if err != nil {
		h.Logger.Error("admin: list all projects failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list projects: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"projects": rows,
		"count":    len(rows),
	})
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

// adminCreateKeyRequest is the POST body for /admin/api-keys.
// Mirrors Verdifax's shape (see Verdifax orchestrator's createKeyRequest)
// so the dashboard JS can be written once with one mental model.
type adminCreateKeyRequest struct {
	// Name is the human-chosen label. Required, max 200 chars.
	// For customer-scope keys the handler does NOT add the
	// "customer:" prefix Verdifax uses; Mesedi separates scope into a
	// dedicated column so a marker prefix in the name is redundant.
	Name string `json:"name"`
	// Scope: "customer" (default, project-scoped) or "admin"
	// (privileged). Empty / missing defaults to "customer" so an old
	// client cannot silently escalate.
	Scope string `json:"scope,omitempty"`
	// ConfirmAdminScope MUST be true when Scope=="admin". A typo in
	// the scope field is not enough to mint a privileged credential;
	// the dashboard explicitly opts in via a confirmation checkbox.
	ConfirmAdminScope bool `json:"confirm_admin_scope,omitempty"`
	// ProjectID is required when Scope=="customer" (the project the
	// new key authenticates as). Ignored when Scope=="admin"; the
	// handler always assigns admin keys to store.APIKeyAdminProjectID.
	ProjectID string `json:"project_id,omitempty"`
	// ExpiresAt is optional. Accepts either RFC3339Nano
	// ("2026-12-31T23:59:59Z") or YYYY-MM-DD (parsed as end-of-day
	// UTC). Empty == never expires. Past timestamps are rejected.
	ExpiresAt string `json:"expires_at,omitempty"`
}

// HandleAdminCreateAPIKey mints a new API key. See adminCreateKeyRequest.
// Returns the raw secret ONCE; the operator stores it immediately or
// has to revoke + remint.
func (h *Handlers) HandleAdminCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var req adminCreateKeyRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	name := strings.TrimSpace(req.Name)
	if name == "" {
		writeError(w, http.StatusBadRequest, "name required")
		return
	}
	if len(name) > 200 {
		writeError(w, http.StatusBadRequest, "name too long (max 200 chars)")
		return
	}
	scope := strings.ToLower(strings.TrimSpace(req.Scope))
	if scope == "" {
		scope = store.APIKeyScopeCustomer
	}
	if scope != store.APIKeyScopeCustomer && scope != store.APIKeyScopeAdmin {
		writeError(w, http.StatusBadRequest, `scope must be "customer" or "admin"`)
		return
	}
	if scope == store.APIKeyScopeAdmin && !req.ConfirmAdminScope {
		writeError(w, http.StatusBadRequest,
			"admin scope requires confirm_admin_scope:true")
		return
	}

	// Resolve project_id by scope.
	var projectID string
	if scope == store.APIKeyScopeAdmin {
		projectID = store.APIKeyAdminProjectID
	} else {
		projectID = strings.TrimSpace(req.ProjectID)
		if projectID == "" {
			writeError(w, http.StatusBadRequest, "project_id required for customer-scope keys")
			return
		}
		if projectID == store.APIKeyAdminProjectID {
			writeError(w, http.StatusBadRequest,
				`project_id "_admin" is reserved for admin-scope keys`)
			return
		}
		// Verify the project exists so we fail at 400 (operator typo)
		// rather than 500 (FK constraint violation deep in store.go).
		if _, err := h.Store.GetProject(r.Context(), projectID); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				writeError(w, http.StatusNotFound, "project not found: "+projectID)
				return
			}
			writeError(w, http.StatusInternalServerError, "project lookup failed: "+err.Error())
			return
		}
	}

	expiresAt, err := parseAdminKeyExpiresAt(req.ExpiresAt)
	if err != nil {
		writeError(w, http.StatusBadRequest, "expires_at: "+err.Error())
		return
	}

	rawKey, hash, prefix, err := MintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint key: "+err.Error())
		return
	}
	now := time.Now().UTC()
	keyID := "key-" + prefix[len("mesedi_sk_"):] + "-" + strconv.FormatInt(now.UnixNano(), 10)

	k := &store.APIKey{
		KeyID:     keyID,
		ProjectID: projectID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      name,
		CreatedAt: now,
		Scope:     scope,
		ExpiresAt: expiresAt,
	}
	if err := h.Store.CreateAPIKey(r.Context(), k); err != nil {
		writeError(w, http.StatusInternalServerError, "create key: "+err.Error())
		return
	}

	resp := map[string]any{
		"ok":         true,
		"key_id":     keyID,
		"project_id": projectID,
		"name":       name,
		"scope":      scope,
		"key_prefix": prefix,
		"secret":     rawKey,
		"warning":    "Store this secret now. It will never be shown again.",
		"created_at": now.Format(time.RFC3339Nano),
	}
	if expiresAt != "" {
		resp["expires_at"] = expiresAt
	}
	// Attribution log line (the audit trail until we add a proper table).
	method, _ := AdminAuthMethodFromContext(r.Context())
	actorKeyID, _ := AdminKeyIDFromContext(r.Context())
	h.Logger.Info("admin: api key minted",
		"key_id", keyID,
		"scope", scope,
		"project_id", projectID,
		"actor_method", method,
		"actor_key_id", actorKeyID,
	)
	writeJSON(w, http.StatusCreated, resp)
}

// HandleAdminListAPIKeys returns every API key in the system, NEWEST
// first. key_hash is never serialized. Admin-only.
func (h *Handlers) HandleAdminListAPIKeys(w http.ResponseWriter, r *http.Request) {
	keys, err := h.Store.ListAllAPIKeys(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list api keys: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"api_keys": keys,
		"count":    len(keys),
	})
}

// HandleAdminRevokeAPIKey hard-deletes any API key by key_id. Admin-
// only. The dashboard layers an admin-key-paste confirmation on top
// of this for admin-scope deletions (Verdifax pattern, task #2).
func (h *Handlers) HandleAdminRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "missing key id")
		return
	}
	// Self-revoke guard: if this revoke target is the same key the
	// operator is currently authenticated with, refuse so the operator
	// can't accidentally lock themselves out. The dashboard already
	// warns about this; this is the server-side fail-safe.
	if actorKeyID, ok := AdminKeyIDFromContext(r.Context()); ok && actorKeyID == keyID {
		writeError(w, http.StatusBadRequest,
			"refusing to revoke the key authenticating this request; "+
				"mint a replacement admin key first, then revoke this one with the new key")
		return
	}
	if err := h.Store.DeleteAPIKeyByID(r.Context(), keyID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "key not found: "+keyID)
			return
		}
		writeError(w, http.StatusInternalServerError, "revoke key: "+err.Error())
		return
	}
	method, _ := AdminAuthMethodFromContext(r.Context())
	actorKeyID, _ := AdminKeyIDFromContext(r.Context())
	h.Logger.Info("admin: api key revoked",
		"key_id", keyID,
		"actor_method", method,
		"actor_key_id", actorKeyID,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"key_id": keyID,
	})
}

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

// parseAdminKeyExpiresAt accepts either RFC3339Nano / RFC3339 or
// YYYY-MM-DD and returns the canonical RFC3339Nano UTC string that
// gets stored in api_keys.expires_at. Empty input is allowed and
// returns "" (never expires).
//
// Date-only inputs are interpreted as end-of-day UTC (23:59:59.999...)
// so "expires today" works as the operator's intuition expects.
// Past timestamps are rejected; minting an already-dead credential is
// never what the operator meant.
func parseAdminKeyExpiresAt(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", nil
	}
	// Try date-only first since that's the common case from a
	// <input type="date"> picker.
	if t, err := time.ParseInLocation("2006-01-02", s, time.UTC); err == nil {
		t = t.Add(24*time.Hour - time.Nanosecond)
		if !time.Now().UTC().Before(t) {
			return "", fmt.Errorf("date is in the past")
		}
		return t.Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse(time.RFC3339Nano, s); err == nil {
		if !time.Now().UTC().Before(t) {
			return "", fmt.Errorf("timestamp is in the past")
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		if !time.Now().UTC().Before(t) {
			return "", fmt.Errorf("timestamp is in the past")
		}
		return t.UTC().Format(time.RFC3339Nano), nil
	}
	return "", fmt.Errorf("expected YYYY-MM-DD or RFC3339")
}

// adminHaikuCostPerAnalysisUSD is the per-analysis Anthropic cost
// surfaced on the admin breakdown. Matches the Cap-math number in
// billing.go's TeamAIAnalysisLimit comment (~$0.03 Haiku 4.5
// input+output for a typical failure group). Used to compute the
// estimated cost-to-Verdifax column so the founder can spot heavy
// users whose Anthropic burn outpaces their subscription revenue.
const adminHaikuCostPerAnalysisUSD = 0.03

// AdminAIAnalysesByProjectRow is the response payload row for
// GET /admin/ai-analyses-by-project. Wraps the store row with the
// estimated Anthropic cost so the dashboard can render a single
// table without extra math.
type AdminAIAnalysesByProjectRow struct {
	ProjectID        string   `json:"project_id"`
	Name             string   `json:"name"`
	OwnerEmail       string   `json:"owner_email,omitempty"`
	Tier             string   `json:"tier"`
	TenantID         string   `json:"tenant_id,omitempty"`
	Count            int      `json:"count"`
	EstimatedCostUSD float64  `json:"estimated_cost_usd"`
	// FailureClasses powers the per-row chip filter on the admin
	// dashboard (#211). Distinct failure_class slugs the project
	// ran analyses against during this window. Empty omitted.
	FailureClasses []string `json:"failure_classes,omitempty"`
}

// AdminAIAnalysesByProjectResponse is the JSON body of
// GET /admin/ai-analyses-by-project. Surfaces the per-project
// breakdown plus aggregate totals so the dashboard can render the
// summary chip without a second round trip.
type AdminAIAnalysesByProjectResponse struct {
	Since                 string                       `json:"since"`
	TotalCount            int                          `json:"total_count"`
	TotalEstimatedCostUSD float64                      `json:"total_estimated_cost_usd"`
	Projects              []AdminAIAnalysesByProjectRow `json:"projects"`
}

// HandleAdminAIAnalysesByProject returns the per-project AI
// root-cause analysis breakdown (#197). Default window is the
// start of the current calendar month UTC; override via ?since=
// query param as RFC3339. Used by the founder dashboard to spot
// heavy AI users for billing reconciliation and abuse detection.
func (h *Handlers) HandleAdminAIAnalysesByProject(w http.ResponseWriter, r *http.Request) {
	since := startOfCurrentMonthUTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				"since must be RFC3339 (e.g., 2026-06-01T00:00:00Z): "+err.Error())
			return
		}
		since = t.UTC()
	}

	rows, err := h.Store.ListAIAnalysesUsageByProject(r.Context(), since)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list ai analyses by project: "+err.Error())
		return
	}

	out := AdminAIAnalysesByProjectResponse{
		Since:    since.Format(time.RFC3339),
		Projects: make([]AdminAIAnalysesByProjectRow, 0, len(rows)),
	}
	for _, r := range rows {
		est := float64(r.Count) * adminHaikuCostPerAnalysisUSD
		out.Projects = append(out.Projects, AdminAIAnalysesByProjectRow{
			ProjectID:        r.ProjectID,
			Name:             r.Name,
			OwnerEmail:       r.OwnerEmail,
			Tier:             r.Tier,
			TenantID:         r.TenantID,
			Count:            r.Count,
			EstimatedCostUSD: est,
			FailureClasses:   r.FailureClasses,
		})
		out.TotalCount += r.Count
		out.TotalEstimatedCostUSD += est
	}
	writeJSON(w, http.StatusOK, out)
}

// startOfCurrentMonthUTC returns the first instant of the current
// UTC calendar month. Used as the default window for the admin
// AI-analyses breakdown so the dashboard "this month" view matches
// what most billing reconciliation flows expect.
func startOfCurrentMonthUTC() time.Time {
	now := time.Now().UTC()
	return time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
}

// --- #198 Anthropic credit + burn rate ----------------------------

// AdminAnthropicCreditResponse is the JSON body of
// GET /admin/anthropic-credit. Fields are pointer-typed when they
// might legitimately be absent so the dashboard can branch on
// "not configured" vs "no balance recorded yet" without inspecting
// string fields.
//
// Auto-decrement model (#198 redesign): the founder records a
// balance ONCE per top-up. From that snapshot onward, the
// CURRENT balance the dashboard shows is computed as
//
//	current_balance = recorded_balance − spend_since_snapshot
//
// where spend_since_snapshot is the sum of Anthropic Cost Report
// buckets between the snapshot timestamp and now. The recorded
// raw value is still surfaced (CreditBalanceRecordedUSD) for
// transparency and a "as of" disclosure on the UI.
type AdminAnthropicCreditResponse struct {
	// AdminAPIConfigured reflects whether ANTHROPIC_ADMIN_KEY is set.
	// When false, burn-rate + auto-decrement fields are nil because
	// we can't compute them without calling the Cost Report endpoint.
	AdminAPIConfigured bool `json:"admin_api_configured"`

	// CreditBalanceRecordedUSD is the most recently entered raw
	// balance (manual entry; Anthropic does not expose this via API).
	// Nil when no snapshot has been recorded yet.
	CreditBalanceRecordedUSD *float64 `json:"credit_balance_recorded_usd,omitempty"`
	// CreditSnapshotAt is when the founder pasted the recorded
	// balance above. Nil when no snapshot recorded.
	CreditSnapshotAt *string `json:"credit_snapshot_at,omitempty"`
	// CreditActorEmail records who recorded the snapshot. Nil when
	// no snapshot or when actor identity was not captured.
	CreditActorEmail *string `json:"credit_actor_email,omitempty"`
	// CreditNote is the optional free-text reason ("after top-up").
	CreditNote *string `json:"credit_note,omitempty"`

	// CreditSpentSinceSnapshotUSD is the Anthropic-side spend
	// between CreditSnapshotAt and now, pulled from the Cost Report
	// API. Nil when admin API isn't configured or no snapshot exists.
	CreditSpentSinceSnapshotUSD *float64 `json:"credit_spent_since_snapshot_usd,omitempty"`
	// CreditBalanceCurrentUSD is the live displayed balance:
	// CreditBalanceRecordedUSD − CreditSpentSinceSnapshotUSD.
	// Clamped at zero so we never render negative dollars. Nil
	// when either input is missing.
	CreditBalanceCurrentUSD *float64 `json:"credit_balance_current_usd,omitempty"`

	// BurnRateUSDPerDay is the 7-day rolling burn computed from the
	// Anthropic Cost Report endpoint. Nil when admin API isn't
	// configured. Zero is a valid value (no burn in the window).
	BurnRateUSDPerDay *float64 `json:"burn_rate_usd_per_day,omitempty"`
	// TotalSpentLast7DUSD is the raw total over the 7-day window
	// used to compute BurnRateUSDPerDay; surfaced so the dashboard
	// can show "spent $X.XX in the last 7 days" alongside the daily
	// rate.
	TotalSpentLast7DUSD *float64 `json:"total_spent_last_7d_usd,omitempty"`

	// RunwayDays is CreditBalanceCurrentUSD / BurnRateUSDPerDay,
	// computed here so the dashboard renders one number. Nil when
	// either input is missing OR when burn rate is zero (avoid
	// div-by-zero and "infinity days of runway" surprises).
	RunwayDays *float64 `json:"runway_days,omitempty"`

	// DailySpend is the last-14-days per-day spend in USD, oldest
	// first. Powers the inline bar chart on the admin card so the
	// founder can spot a spike at a glance. Empty (omitted)
	// when admin API isn't configured.
	DailySpend []AdminDailySpendBucket `json:"daily_spend,omitempty"`
}

// AdminDailySpendBucket is one bar of the 14-day spend chart.
type AdminDailySpendBucket struct {
	Date string  `json:"date"`     // YYYY-MM-DD UTC
	USD  float64 `json:"usd"`      // total spend that day
}

// HandleAdminGetAnthropicCredit assembles the founder burn-rate
// widget payload (#198). Three pieces:
//   1. Latest recorded balance snapshot (manual entry, infrequent).
//   2. Spend SINCE that snapshot (Anthropic Cost Report). Auto-
//      decrements the displayed balance so the founder rarely has
//      to re-enter anything.
//   3. Last 14 days of daily spend (chart + 7-day burn rate).
// Cost Report failures degrade gracefully: the snapshot fields are
// always returned even if Anthropic's API is down, and the
// derived numbers are simply omitted with a logged warning.
func (h *Handlers) HandleAdminGetAnthropicCredit(w http.ResponseWriter, r *http.Request) {
	out := AdminAnthropicCreditResponse{
		AdminAPIConfigured: h.AnthropicAdmin != nil && h.AnthropicAdmin.Configured(),
	}

	// 1. Latest credit-balance snapshot (manual entry).
	snap, err := h.Store.GetLatestAnthropicCreditSnapshot(r.Context())
	if err == nil && snap != nil {
		bal := snap.BalanceUSD
		out.CreditBalanceRecordedUSD = &bal
		ts := snap.SnapshottedAt.UTC().Format(time.RFC3339)
		out.CreditSnapshotAt = &ts
		if snap.ActorEmail != "" {
			email := snap.ActorEmail
			out.CreditActorEmail = &email
		}
		if snap.Note != "" {
			note := snap.Note
			out.CreditNote = &note
		}
	} else if err != nil && !errors.Is(err, store.ErrNotFound) {
		writeError(w, http.StatusInternalServerError,
			"load credit snapshot: "+err.Error())
		return
	}

	if !out.AdminAPIConfigured {
		writeJSON(w, http.StatusOK, out)
		return
	}

	now := time.Now().UTC()

	// 2. Spend since snapshot -> auto-decremented current balance.
	if snap != nil {
		report, cerr := h.AnthropicAdmin.GetCostReport(r.Context(),
			snap.SnapshottedAt.UTC(), now)
		if cerr != nil {
			h.Logger.Warn("anthropic cost report (since-snapshot) failed; current balance omitted",
				"error", cerr.Error())
		} else {
			spent := report.TotalUSD
			out.CreditSpentSinceSnapshotUSD = &spent
			current := snap.BalanceUSD - spent
			if current < 0 {
				current = 0 // clamp; recorded balance is now stale.
			}
			out.CreditBalanceCurrentUSD = &current
		}
	}

	// 3. Last 14 days: chart + 7-day burn rate.
	startingAt := now.AddDate(0, 0, -14)
	report14, cerr := h.AnthropicAdmin.GetCostReport(r.Context(), startingAt, now)
	if cerr != nil {
		h.Logger.Warn("anthropic cost report (14-day) failed; chart + burn rate omitted",
			"error", cerr.Error())
	} else {
		// Sort buckets oldest-first so the chart renders left-to-right
		// in chronological order even if Anthropic reorders them.
		buckets := append([]anthropic.DailyCostBucket(nil), report14.DailyBuckets...)
		sort.Slice(buckets, func(i, j int) bool {
			return buckets[i].Date.Before(buckets[j].Date)
		})

		out.DailySpend = make([]AdminDailySpendBucket, 0, len(buckets))
		for _, b := range buckets {
			out.DailySpend = append(out.DailySpend, AdminDailySpendBucket{
				Date: b.Date.Format("2006-01-02"),
				USD:  b.USD,
			})
		}

		// 7-day burn rate from the LAST 7 buckets (newest end). If
		// the response had fewer than 7 buckets (e.g., a brand new
		// org), divide by however many we got so we don't under-
		// estimate the rate.
		windowSize := 7
		if len(buckets) < windowSize {
			windowSize = len(buckets)
		}
		var last7Total float64
		if windowSize > 0 {
			last7 := buckets[len(buckets)-windowSize:]
			for _, b := range last7 {
				last7Total += b.USD
			}
			total := last7Total
			out.TotalSpentLast7DUSD = &total
			rate := last7Total / float64(windowSize)
			out.BurnRateUSDPerDay = &rate
			// Runway: use the auto-decremented current balance when
			// available, otherwise fall back to the recorded value.
			var bal float64
			var have bool
			switch {
			case out.CreditBalanceCurrentUSD != nil:
				bal = *out.CreditBalanceCurrentUSD
				have = true
			case out.CreditBalanceRecordedUSD != nil:
				bal = *out.CreditBalanceRecordedUSD
				have = true
			}
			if have && rate > 0 {
				days := bal / rate
				out.RunwayDays = &days
			}
		}
	}

	writeJSON(w, http.StatusOK, out)
}

// adminCreateCreditSnapshotRequest is the POST body shape.
type adminCreateCreditSnapshotRequest struct {
	// BalanceUSD is the dollar amount the founder pasted from the
	// Anthropic Console sidebar. Required. Must be >= 0.
	BalanceUSD float64 `json:"balance_usd"`
	// ActorEmail is who recorded the snapshot. Optional but
	// recommended so the audit trail is human-readable.
	ActorEmail string `json:"actor_email,omitempty"`
	// Note is an optional reason ("after $100 top-up"). Free-form.
	Note string `json:"note,omitempty"`
}

// HandleAdminCreateAnthropicCreditSnapshot inserts a new manual
// credit-balance snapshot (#198). One row per call; the GET endpoint
// always reads the most recent. History is preserved so a future
// "snapshot history" page can chart balance over time.
func (h *Handlers) HandleAdminCreateAnthropicCreditSnapshot(w http.ResponseWriter, r *http.Request) {
	var body adminCreateCreditSnapshotRequest
	if err := decodeJSON(r, &body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if body.BalanceUSD < 0 {
		writeError(w, http.StatusBadRequest, "balance_usd must be >= 0")
		return
	}
	// Sanity ceiling: nobody is going to manually enter $1M+, and a
	// typo there would scramble runway math.
	const maxBalance = 100_000.0
	if body.BalanceUSD > maxBalance {
		writeError(w, http.StatusBadRequest,
			"balance_usd above $100,000 looks like a typo; double-check the value")
		return
	}

	snap := &store.AnthropicCreditSnapshot{
		SnapshotID:    newCreditSnapshotID(),
		BalanceUSD:    body.BalanceUSD,
		SnapshottedAt: time.Now().UTC(),
		ActorEmail:    strings.TrimSpace(body.ActorEmail),
		Note:          strings.TrimSpace(body.Note),
	}
	if err := h.Store.CreateAnthropicCreditSnapshot(r.Context(), snap); err != nil {
		writeError(w, http.StatusInternalServerError,
			"persist credit snapshot: "+err.Error())
		return
	}
	h.Logger.Info("anthropic credit snapshot recorded",
		"snapshot_id", snap.SnapshotID,
		"balance_usd", snap.BalanceUSD,
		"actor_email", snap.ActorEmail)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"snapshot_id": snap.SnapshotID,
		"balance_usd": snap.BalanceUSD,
	})
}

// newCreditSnapshotID produces a "credit_<32 hex>" identifier
// matching the prefix-plus-random pattern used elsewhere in Mesedi.
// crypto/rand fallback to nanosecond hex is safe here because
// snapshots are low-volume admin-only writes.
func newCreditSnapshotID() string {
	return "credit_" + newAuditEventID()[len("audit_"):]
}

// --- #199 AI analyses flat list + lifetime totals -----------------

// AdminListAIAnalysesResponse is the JSON body of
// GET /admin/ai-analyses. One row per Anthropic call ever made by
// the analyze handler. Sorted recent-first; paginated via limit +
// offset query params.
type AdminListAIAnalysesResponse struct {
	OK       bool                 `json:"ok"`
	Analyses []*store.AIAnalysis  `json:"analyses"`
	Limit    int                  `json:"limit"`
	Offset   int                  `json:"offset"`
}

// HandleAdminListAIAnalyses returns a flat cross-tenant list of
// every analysis, newest first. Default limit 100 with a hard
// ceiling of 500 so a runaway pagination loop on the frontend
// can't pull a giant payload.
func (h *Handlers) HandleAdminListAIAnalyses(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 500 {
		limit = 500
	}
	offset := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("offset")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n >= 0 {
			offset = n
		}
	}
	analyses, err := h.Store.ListAIAnalyses(r.Context(), limit, offset)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list ai analyses: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, AdminListAIAnalysesResponse{
		OK:       true,
		Analyses: analyses,
		Limit:    limit,
		Offset:   offset,
	})
}

// HandleAdminGetAIAnalysesTotals returns lifetime + this-month
// summary stats for the founder tile at the top of /admin/ai-analyses.
func (h *Handlers) HandleAdminGetAIAnalysesTotals(w http.ResponseWriter, r *http.Request) {
	totals, err := h.Store.GetAIAnalysesTotals(r.Context())
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"ai analyses totals: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, totals)
}

// AdminProjectAIAnalysesDetailRow is one analyzed failure group on
// the per-project breakdown surface (#211). Trims store.FailureGroup
// down to the columns the admin actually needs to spot heavy users
// and reconcile Anthropic spend.
type AdminProjectAIAnalysesDetailRow struct {
	GroupID            string  `json:"group_id"`
	FailureClass       string  `json:"failure_class"`
	Signature          string  `json:"signature"`
	EventCount         int     `json:"event_count"`
	AffectedExecutions int     `json:"affected_executions"`
	AnalyzedAt         string  `json:"analyzed_at"`
	AnalysisModel      string  `json:"analysis_model,omitempty"`
	EstimatedCostUSD   float64 `json:"estimated_cost_usd"`
	LastSeen           string  `json:"last_seen"`
}

// AdminProjectAIAnalysesDetailResponse is the JSON body of
// GET /admin/projects/{id}/ai-analyses-detail. Surfaces the per-group
// breakdown plus this-project's aggregate totals so the dashboard
// can render an inset summary inside the expanded row without a
// second round trip.
type AdminProjectAIAnalysesDetailResponse struct {
	ProjectID             string                            `json:"project_id"`
	Since                 string                            `json:"since"`
	TotalCount            int                               `json:"total_count"`
	TotalEstimatedCostUSD float64                           `json:"total_estimated_cost_usd"`
	Groups                []AdminProjectAIAnalysesDetailRow `json:"groups"`
}

// HandleAdminProjectAIAnalysesDetail returns the analyzed failure
// groups for one project since the given window (#211). The default
// window matches the parent admin AI-analyses page (start of current
// UTC month) so the expanded row's totals add up to the table row.
func (h *Handlers) HandleAdminProjectAIAnalysesDetail(w http.ResponseWriter, r *http.Request) {
	projectID := r.PathValue("id")
	if projectID == "" {
		writeError(w, http.StatusBadRequest, "project_id path parameter required")
		return
	}
	since := startOfCurrentMonthUTC()
	if raw := strings.TrimSpace(r.URL.Query().Get("since")); raw != "" {
		t, err := time.Parse(time.RFC3339, raw)
		if err != nil {
			writeError(w, http.StatusBadRequest,
				"since must be RFC3339 (e.g., 2026-06-01T00:00:00Z): "+err.Error())
			return
		}
		since = t.UTC()
	}

	groups, err := h.Store.ListAnalyzedFailureGroupsByProject(r.Context(), projectID, since, 0)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list analyzed failure groups: "+err.Error())
		return
	}

	out := AdminProjectAIAnalysesDetailResponse{
		ProjectID: projectID,
		Since:     since.Format(time.RFC3339),
		Groups:    make([]AdminProjectAIAnalysesDetailRow, 0, len(groups)),
	}
	for _, g := range groups {
		row := AdminProjectAIAnalysesDetailRow{
			GroupID:            g.GroupID,
			FailureClass:       g.FailureClass,
			Signature:          g.Signature,
			EventCount:         g.EventCount,
			AffectedExecutions: g.AffectedExecutions,
			EstimatedCostUSD:   adminHaikuCostPerAnalysisUSD,
			LastSeen:           g.LastSeen.UTC().Format(time.RFC3339),
		}
		if g.AnalyzedAt != nil {
			row.AnalyzedAt = g.AnalyzedAt.UTC().Format(time.RFC3339)
		}
		if g.AnalysisModel != nil {
			row.AnalysisModel = *g.AnalysisModel
		}
		out.Groups = append(out.Groups, row)
		out.TotalCount++
		out.TotalEstimatedCostUSD += adminHaikuCostPerAnalysisUSD
	}
	writeJSON(w, http.StatusOK, out)
}

// AdminClosedProjectAuditRow is the wire shape returned by the
// closed-project audit search endpoint. We project AuditEvent into
// a flat shape so the caller does not have to know about
// sql.NullString / sql.NullTime serialization quirks.
type AdminClosedProjectAuditRow struct {
	EventID             string         `json:"event_id"`
	ProjectID           string         `json:"project_id"`
	ProjectNameSnapshot string         `json:"project_name_snapshot,omitempty"`
	ProjectDeletedAt    string         `json:"project_deleted_at,omitempty"`
	ActorKeyID          string         `json:"actor_key_id,omitempty"`
	ActorKeyName        string         `json:"actor_key_name,omitempty"`
	ActorEmail          string         `json:"actor_email,omitempty"`
	Action              string         `json:"action"`
	TargetType          string         `json:"target_type,omitempty"`
	TargetID            string         `json:"target_id,omitempty"`
	Metadata            map[string]any `json:"metadata,omitempty"`
	CreatedAt           string         `json:"created_at"`
}

// AdminClosedProjectAuditResponse wraps the search results with the
// echoed filter so a caller paging through results can confirm what
// the server actually applied (e.g. limit fell back to the default).
type AdminClosedProjectAuditResponse struct {
	Email     string                       `json:"email,omitempty"`
	ProjectID string                       `json:"project_id,omitempty"`
	Limit     int                          `json:"limit"`
	Count     int                          `json:"count"`
	Rows      []AdminClosedProjectAuditRow `json:"rows"`
}

// HandleAdminSearchClosedProjectAudit serves R1 + R2 lookups against
// closed-project audit history. Migration 031 added the survival
// columns (project_name_snapshot, project_deleted_at); this endpoint
// is the read path.
//
// Auth: AdminAuth (legacy token OR admin-scope API key). Customer
// keys do not reach this route.
//
// Query params:
//
//	email      — search every closed project where actor_email = X
//	             (powers R1 account-takeover forensics: "show me
//	              every Close action this victim's email ever fired").
//	project_id — search every audit row for a specific closed project
//	             (powers R2 customer-support response: "user X says
//	              they did not close project Y; show me the close
//	              event and who pressed it").
//	limit      — cap rows (default 100, store-side enforced).
//
// At least one of email or project_id must be present; both empty
// returns 400 (the store would also refuse, but we fail fast at the
// edge with a clearer message).
//
// Response shape: AdminClosedProjectAuditResponse. Rows are ordered
// by created_at DESC (store contract). metadata_json is parsed into
// a typed map; on parse failure we drop the field rather than 500
// the whole row (an unreadable metadata blob shouldn't hide the
// audit row itself from the operator).
//
// Follow-up: task #220 ships the dashboard UI on top of this
// endpoint. For now staff curl it directly.
func (h *Handlers) HandleAdminSearchClosedProjectAudit(w http.ResponseWriter, r *http.Request) {
	email := strings.TrimSpace(r.URL.Query().Get("email"))
	projectID := strings.TrimSpace(r.URL.Query().Get("project_id"))
	if email == "" && projectID == "" {
		writeError(w, http.StatusBadRequest,
			"at least one of email or project_id is required")
		return
	}

	limit := 0
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		n, err := strconv.Atoi(raw)
		if err != nil || n < 0 {
			writeError(w, http.StatusBadRequest,
				"limit must be a non-negative integer")
			return
		}
		limit = n
	}

	// NOTE: do not name this slice "events" -- the package imports
	// mesedi/backend/internal/events and the local name would shadow
	// it for the rest of the function. Pick something distinct.
	auditRows, err := h.Store.SearchClosedProjectAuditEvents(
		r.Context(), store.ClosedProjectAuditFilter{
			Email:     email,
			ProjectID: projectID,
			Limit:     limit,
		},
	)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"search closed project audit: "+err.Error())
		return
	}

	rows := make([]AdminClosedProjectAuditRow, 0, len(auditRows))
	for _, ev := range auditRows {
		row := AdminClosedProjectAuditRow{
			EventID:             ev.EventID,
			ProjectID:           ev.ProjectID,
			ProjectNameSnapshot: ev.ProjectNameSnapshot,
			ActorKeyID:          ev.ActorKeyID,
			ActorKeyName:        ev.ActorKeyName,
			ActorEmail:          ev.ActorEmail,
			Action:              ev.Action,
			TargetType:          ev.TargetType,
			TargetID:            ev.TargetID,
			CreatedAt:           ev.CreatedAt.UTC().Format(time.RFC3339),
		}
		if ev.ProjectDeletedAt != nil {
			row.ProjectDeletedAt = ev.ProjectDeletedAt.UTC().Format(time.RFC3339)
		}
		if ev.MetadataJSON != "" {
			var md map[string]any
			if jerr := json.Unmarshal([]byte(ev.MetadataJSON), &md); jerr == nil {
				row.Metadata = md
			}
		}
		rows = append(rows, row)
	}

	effectiveLimit := limit
	if effectiveLimit == 0 {
		effectiveLimit = 100
	}
	writeJSON(w, http.StatusOK, AdminClosedProjectAuditResponse{
		Email:     email,
		ProjectID: projectID,
		Limit:     effectiveLimit,
		Count:     len(rows),
		Rows:      rows,
	})
}
