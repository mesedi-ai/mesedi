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
	"strconv"
	"strings"
	"syscall"
	"time"

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
	mux.HandleFunc("GET /admin/storage", h.HandleAdminStorage)
	mux.HandleFunc("GET /admin/abuse", h.HandleAdminListAbuseSignals)
	mux.HandleFunc("POST /admin/abuse/{id}/resolve", h.HandleAdminResolveAbuseSignal)
	// API keys management (migration 015).
	mux.HandleFunc("GET /admin/api-keys", h.HandleAdminListAPIKeys)
	mux.HandleFunc("POST /admin/api-keys", h.HandleAdminCreateAPIKey)
	mux.HandleFunc("DELETE /admin/api-keys/{id}", h.HandleAdminRevokeAPIKey)
	mux.HandleFunc("GET /admin/whoami", h.HandleAdminWhoami)
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
	if err := h.Store.UpdateProjectTier(context.Background(), projectID, tier, expiresAt); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "update tier: "+err.Error())
		return
	}
	// The admin audit trail is just structured log lines today ,
	// rotate to a real audit_events table when traffic justifies it.
	h.Logger.Info("admin: tier set",
		"project_id", projectID,
		"new_tier", tier,
		"expires_days", req.ExpiresDays,
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
