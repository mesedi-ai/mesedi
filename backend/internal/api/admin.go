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
	"io"
	"net/http"
	"os"
	"strings"
	"syscall"
	"time"

	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

// AdminAuth returns middleware that gates routes behind a static admin
// bearer token. The middleware is created once at server startup (in
// main.go) with the configured token; if the token is empty, the
// middleware refuses every request (fail-closed posture).
//
// Auth header shape: `Authorization: Bearer <token>`. Mismatched or
// missing headers return 401 with an opaque message; we don't echo
// the token or describe why it failed (timing-and-information leak
// hygiene).
func AdminAuth(adminToken string) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if adminToken == "" {
				// Fail closed when no token is configured, refuses to
				// expose admin endpoints on an accidentally-misconfigured
				// deploy.
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
			// Constant-time compare on equal-length slices. If lengths
			// differ, ConstantTimeCompare returns 0 in O(min(len)) time
			// without leaking which prefix matched.
			if subtle.ConstantTimeCompare([]byte(supplied), []byte(adminToken)) != 1 {
				writeError(w, http.StatusUnauthorized, "unauthorized")
				return
			}
			next.ServeHTTP(w, r)
		})
	}
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
	mux.HandleFunc("GET /admin/storage", h.HandleAdminStorage)
}

// AdminProjectDetail bundles everything the founder dashboard's
// drill-down page needs in a single round trip: the project itself
// plus its recent executions, recent failure_groups, webhooks, and
// API key prefixes (never the hash). Keeps the dashboard's network
// surface small (one fetch on page mount, not four).
type AdminProjectDetail struct {
	Project         *store.AdminProjectRow `json:"project"`
	RecentExecutions []*events.Execution    `json:"recent_executions"`
	RecentFailureGroups []*store.FailureGroup `json:"recent_failure_groups"`
	Webhooks        []*store.ProjectWebhook `json:"webhooks"`
	APIKeys         []*store.APIKey         `json:"api_keys"`
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
	SchemaVersion int                       `json:"schema_version"`
	ExportedAt    time.Time                 `json:"exported_at"`
	Project       *store.AdminProjectRow    `json:"project"`
	APIKeys       []*store.APIKey           `json:"api_keys"`
	Executions    []*ExportedExecution      `json:"executions"`
	FailureGroups []*store.FailureGroup     `json:"failure_groups"`
	Webhooks      []*store.ProjectWebhook   `json:"webhooks"`
	Deliveries    []*store.WebhookDelivery  `json:"webhook_deliveries"`
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
	switch tier {
	case TierHobby, TierPro, TierEnterprise:
		// ok
	default:
		writeError(w, http.StatusBadRequest, "tier must be hobby, pro, or enterprise")
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
