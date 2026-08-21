package api

// Audit-log helpers used by the customer-facing admin endpoints
// (v1). One central recordAuditEvent function so handlers
// stay terse — they call recordAuditEvent(ctx, "api_key.create",
// "api_key", keyID, meta) after their state-change succeeds.
//
// Best-effort posture: a failure to write the audit row logs a
// warning but never fails the underlying action. Customers losing
// the ability to revoke a key because the audit log is full is a
// worse outcome than a missing audit row.

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"time"

	"mesedi/backend/internal/store"
)

// Stable audit action slugs. Centralized so the dashboard's UI map
// stays in sync with what the handlers actually emit.
const (
	AuditAPIKeyCreate            = "api_key.create"
	AuditAPIKeyRevoke            = "api_key.revoke"
	AuditWebhookCreate           = "webhook.create"
	AuditWebhookDelete           = "webhook.delete"
	AuditBillingCapUpdate        = "billing.cap_update"
	AuditBillingDowngrade        = "billing.downgrade_scheduled"
	AuditBillingAccountClose     = "billing.account_closed"
	AuditBillingPaymentMethodAdd = "billing.payment_method_added"
	AuditBillingPaymentMethodRm  = "billing.payment_method_removed"
	// step C — additional v1.5 capture points.
	AuditProjectRename   = "project.rename"
	AuditRetentionUpdate = "project.retention_update"
	// Team-management actions. Recorded against the calling admin's
	// project (the project they were looking at when they made the
	// change); a multi-project org sees the row only on that one
	// project's audit log. team.invite_accept is intentionally not
	// captured here because the accept endpoint is public + token-only
	// and has no caller project context.
	AuditTeamInviteCreate = "team.invite_create"
	AuditTeamInviteRevoke = "team.invite_revoke"
	AuditTeamMemberRemove = "team.member_remove"
	AuditTeamRoleUpdate   = "team.role_update"
	// Platform-admin actions (Mesedi staff modifying a customer
	// project from the admin dashboard). The actor email is a
	// synthetic sentinel so the customer sees that a Mesedi staff
	// member made the change without leaking which staff account.
	AuditTierChangeByPlatformAdmin = "tier.change_by_platform_admin"
	// — GDPR Article 17 "right to be forgotten" purge of a
	// closed project's audit history. Recorded against the _admin
	// system project so the meta-paper-trail of the deletion
	// survives the deletion itself. Metadata field carries
	// rows_purged + reason.
	AuditAuditGDPRPurge = "audit.gdpr_purge"
	// failure-group-resolve wave — customer-initiated resolve /
	// unresolve actions on failure groups. Target type
	// "failure_group", target id = group_id. Metadata empty by
	// default; future additions (resolution reason, customer notes)
	// land here without a new action slug.
	AuditFailureGroupResolve   = "failure_group.resolved"
	AuditFailureGroupUnresolve = "failure_group.unresolved"
)

// AuditActorPlatformAdmin is the synthetic actor_email written by
// platform-admin capture points (Mesedi staff acting on a customer
// project). Customers see this in their audit log's ACTOR column;
// the dashboard renders it verbatim and does not need to know it is
// not a real address. Centralized so other platform-admin captures
// (grant, suspend, etc.) write the same string and the dashboard's
// special-casing has one source of truth.
const AuditActorPlatformAdmin = "Mesedi platform admin"

// recordAuditEvent inserts one audit row for the request's project.
// Reads actor identity from the request context (set by AuthMiddleware).
// Metadata is JSON-encoded into the metadata_json column; pass nil to
// omit. Errors are logged at WARN and swallowed so the calling
// handler's success path is not affected.
func (h *Handlers) recordAuditEvent(
	r *http.Request,
	action, targetType, targetID string,
	metadata map[string]any,
) {
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		// No project context means this was called from a code
		// path we don't audit (e.g., unauthenticated). Silently
		// no-op rather than scream.
		return
	}

	var metaJSON string
	if metadata != nil {
		if b, err := json.Marshal(metadata); err == nil {
			metaJSON = string(b)
		}
	}

	// Capture actor identity from context. All values are optional;
	// any missing piece just leaves that field NULL in the row.
	keyID, _ := APIKeyIDFromContext(r.Context())
	keyName := ""
	// Use legacy admin-key-name when available (admin endpoints
	// already populate this); otherwise leave empty.
	if name, ok := AdminKeyNameFromContext(r.Context()); ok {
		keyName = name
	}
	// Actor email is the api_keys.user_id of the bearer token,
	// which signin.go and signup.go both initialize to the owner's
	// email address. The auth middleware copies it onto the request
	// context under ctxKeyUserID. Without this, every non-signin
	// audit row shows the key id in the dashboard's ACTOR column
	// instead of the human-readable email.
	actorEmail, _ := UserIDFromContext(r.Context())

	event := &store.AuditEvent{
		EventID:      newAuditEventID(),
		ProjectID:    projectID,
		ActorKeyID:   keyID,
		ActorKeyName: keyName,
		ActorEmail:   actorEmail,
		Action:       action,
		TargetType:   targetType,
		TargetID:     targetID,
		MetadataJSON: metaJSON,
		CreatedAt:    time.Now().UTC(),
	}

	if err := h.Store.CreateAuditEvent(r.Context(), event); err != nil {
		// Best-effort: log and continue. An audit-write failure
		// MUST NOT break the customer's underlying admin action.
		h.Logger.Warn("audit event write failed",
			"project_id", projectID,
			"action", action,
			"error", err.Error())
	}
}

// recordAuditEventForProject is the variant of recordAuditEvent for
// code paths that don't have an *http.Request to read context from.
// Stripe webhook handlers are the canonical case: the request comes
// from Stripe's servers, the API key context machinery doesn't apply,
// and there's no caller user_id to extract. Callers pass projectID
// and actorEmail directly (typically project.OwnerEmail from a
// GetProjectByStripeCustomerID lookup the handler has already done).
//
// Same best-effort posture as recordAuditEvent: a CreateAuditEvent
// failure is logged at WARN and swallowed so the calling business
// logic (e.g., MarkCardAttached) is not affected. Empty projectID is
// a silent no-op rather than a panic.
func (h *Handlers) recordAuditEventForProject(
	ctx context.Context,
	projectID, actorEmail string,
	action, targetType, targetID string,
	metadata map[string]any,
) {
	if projectID == "" {
		return
	}

	var metaJSON string
	if metadata != nil {
		if b, err := json.Marshal(metadata); err == nil {
			metaJSON = string(b)
		}
	}

	event := &store.AuditEvent{
		EventID:      newAuditEventID(),
		ProjectID:    projectID,
		ActorEmail:   actorEmail,
		Action:       action,
		TargetType:   targetType,
		TargetID:     targetID,
		MetadataJSON: metaJSON,
		CreatedAt:    time.Now().UTC(),
	}

	if err := h.Store.CreateAuditEvent(ctx, event); err != nil {
		h.Logger.Warn("audit event write failed (no-request variant)",
			"project_id", projectID,
			"action", action,
			"error", err.Error())
	}
}

// recordSystemEventForProject is the system_events counterpart of
// recordAuditEventForProject. Used by code paths that need
// to record operational telemetry — config_fallback, future
// system-actor events — without polluting the customer-visible
// audit_events trail. Mirrors the best-effort posture: a write
// failure is logged at WARN and swallowed so the calling business
// logic (e.g., a detector firing) is not affected. Empty projectID
// is a silent no-op rather than a panic.
//
// actor is the subsystem name ("config_fallback" for now); action,
// targetType, targetID, metadata follow the same convention the
// audit-trail helpers use so the on-disk shape stays comparable.
func (h *Handlers) recordSystemEventForProject(
	ctx context.Context,
	projectID, actor string,
	action, targetType, targetID string,
	metadata map[string]any,
) {
	if projectID == "" {
		return
	}

	var payloadJSON string
	if metadata != nil {
		if b, err := json.Marshal(metadata); err == nil {
			payloadJSON = string(b)
		}
	}

	event := &store.SystemEvent{
		EventID:     newAuditEventID(),
		ProjectID:   projectID,
		Actor:       actor,
		Action:      action,
		TargetType:  targetType,
		TargetID:    targetID,
		PayloadJSON: payloadJSON,
		CreatedAt:   time.Now().UTC(),
	}

	if err := h.Store.InsertSystemEvent(ctx, event); err != nil {
		h.Logger.Warn("system event write failed",
			"project_id", projectID,
			"actor", actor,
			"action", action,
			"error", err.Error())
	}
}

// newAuditEventID produces a "audit_<32 hex chars>" identifier.
// 128 bits of entropy is overkill for an audit row id but matches
// the prefix-+-random pattern used elsewhere in Mesedi (api keys,
// failure groups, etc.) so the format is consistent.
func newAuditEventID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Reader failures are exceptionally rare on Linux/macOS.
		// Fall back to a timestamp-only ID rather than panic; an
		// unlikely collision is better than blocking the audit
		// log. Length still 32 hex chars to keep parsers happy.
		ts := time.Now().UTC().UnixNano()
		return "audit_" + hexFromInt64(ts)
	}
	return "audit_" + hex.EncodeToString(b[:])
}

// hexFromInt64 renders an int64 as a 32-character hex string with
// zero padding. Used only by the RNG-failure fallback in
// newAuditEventID; not exported.
func hexFromInt64(v int64) string {
	const hexDigits = "0123456789abcdef"
	var out [32]byte
	for i := 31; i >= 0; i-- {
		out[i] = hexDigits[v&0xf]
		v >>= 4
	}
	return string(out[:])
}

// ListAuditEventsResponse is the body shape returned by
// GET /audit-log. Wrapping in {events: [...]} matches the pattern
// other list endpoints follow (api keys, webhooks) and leaves room
// for pagination metadata if we ever ship it.
type ListAuditEventsResponse struct {
	OK     bool                `json:"ok"`
	Events []*store.AuditEvent `json:"events"`
}

// HandleListAuditEvents returns the last 100 audit events for the
// calling project. Admin-role AND Team-or-higher tier only. Returned
// in created_at DESC order so the UI can render newest first without
// re-sorting.
//
// .F (closes / ): audit logs moved from Hobby-included
// to Team-only. Rationale: audit logs on a single-admin Hobby project
// are useless (all events are "you did it"); the real product value
// is multi-admin "who did what," which requires the collaboration
// story that only Team+ supports. Hobby customers hitting this
// endpoint get 402 with an upgrade nudge, mirroring the Hobby AI-
// analysis pay-per-use paywall.
func (h *Handlers) HandleListAuditEvents(w http.ResponseWriter, r *http.Request) {
	if !h.requireRole(w, r, "admin") {
		return
	}
	projectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	// Tier gate — Team or Enterprise only (.F). Allowlist
	// pattern rather than denylist so an empty / unknown tier
	// falls to the strictest cap (refuse), matching the doctrine
	// applied in tier_caps.go. Empty tier is a legitimate legacy
	// state (pre-tier-labeling projects) and MUST NOT accidentally
	// bypass the paywall.
	proj, err := h.Store.GetProject(r.Context(), projectID)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"load project for tier check: "+err.Error())
		return
	}
	tier := normalizeTier(proj.Tier)
	if tier != TierTeam && tier != TierProduction && tier != TierEnterprise {
		writeError(w, http.StatusPaymentRequired,
			"Audit logs are a Cloud Team feature. On a single-admin Hobby project every event is 'you did it', so the log has no accountability value. Upgrade to Cloud Team at /app/billing to enable multi-admin audit trail.")
		return
	}
	events, err := h.Store.ListAuditEventsByProject(r.Context(), projectID, 100)
	if err != nil {
		writeError(w, http.StatusInternalServerError,
			"list audit events: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, ListAuditEventsResponse{
		OK:     true,
		Events: events,
	})
}
