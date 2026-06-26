package api

// Cascading-defaults resolvers for #276.a. The resolution chain
// per threshold is:
//
//   1. project-level override (if set; -1 means "inherit from org")
//   2. org-level default in organization_defaults table
//   3. hardcoded constant
//
// Each resolver also writes a config_fallback system_event when
// step 1 OR step 2 errors so the dashboard chip surfaces silent
// store-layer issues (preserves #276.d behavior).
//
// Why -1 as the inherit sentinel: every threshold here is a
// strictly-positive quantity (milliseconds, tenant count, bytes).
// -1 is unambiguous, distinguishable from "unset" (which doesn't
// exist — every project has a non-null row today), and doesn't
// collide with any meaningful threshold value. The PUT validators
// accept -1 as a "clear my override; inherit from org" sentinel.

import (
	"context"
	"encoding/json"

	"mesedi/backend/internal/store"
)

// Default-key constants — must match the strings the dashboard
// PUT request body uses. Centralized so a typo can't desync the
// store from the resolver.
const (
	OrgDefaultKeyTimeBudgetMs               = "time_budget_ms"
	OrgDefaultKeyProviderIncidentMinTenants = "provider_incident_min_tenants"
	OrgDefaultKeyToolReturnValueMaxBytes    = "tool_return_value_max_bytes"
)

// InheritSentinel signals "clear my project override and inherit
// from the org default." Used by all 3 cascading thresholds.
const InheritSentinel = -1

// Hardcoded constants — keep in sync with the historical defaults
// each threshold shipped with.
const (
	HardcodedTimeBudgetMs               = 60_000 // 60s
	HardcodedProviderIncidentMinTenants = 2
	HardcodedToolReturnValueMaxBytes    = 8192 // 8 KB
)

// ResolutionSource identifies which layer of the cascade produced
// the value. Returned alongside each resolver so callers can log
// or emit telemetry per layer.
type ResolutionSource string

const (
	SourceProject    ResolutionSource = "project"
	SourceOrgDefault ResolutionSource = "org_default"
	SourceConstant   ResolutionSource = "constant"
)

// resolveCascadingInt is the shared inner implementation for all 3
// int-valued resolvers. Returns (value, source).
func (h *Handlers) resolveCascadingInt(
	ctx context.Context,
	projectID string,
	projectValue int,
	projectErr error,
	orgDefaultKey string,
	hardcoded int,
) (int, ResolutionSource) {
	// Step 1: project layer.
	if projectErr == nil && projectValue != InheritSentinel {
		return projectValue, SourceProject
	}
	// Project read failed OR project explicitly set to inherit.
	// Emit a config_fallback only on the error path; explicit
	// inherit is a customer choice, not a degradation.
	if projectErr != nil {
		h.recordSystemEventForProject(
			ctx, projectID, "config_fallback",
			"config_fallback", "project_config", orgDefaultKey,
			map[string]any{
				"error":  projectErr.Error(),
				"layer":  "project",
				"reason": "project_read_failed_cascading_to_org",
			},
		)
	}

	// Step 2: org-default layer. Resolve project → org_id first.
	orgID := h.lookupOrgIDForProject(ctx, projectID)
	if orgID != "" {
		defs, dErr := h.Store.GetOrgDefaults(ctx, orgID)
		if dErr == nil {
			if raw, ok := defs[orgDefaultKey]; ok {
				var v int
				if jErr := json.Unmarshal([]byte(raw), &v); jErr == nil {
					return v, SourceOrgDefault
				}
			}
		} else {
			h.recordSystemEventForProject(
				ctx, projectID, "config_fallback",
				"config_fallback", "project_config", orgDefaultKey,
				map[string]any{
					"error":  dErr.Error(),
					"layer":  "org_default",
					"reason": "org_defaults_read_failed_cascading_to_constant",
				},
			)
		}
	}

	// Step 3: hardcoded constant.
	return hardcoded, SourceConstant
}

// lookupOrgIDForProject returns the org_id for projectID. Empty
// string on any error — caller treats that as "no org, skip the
// org-default lookup."
//
// Relationship: projects.owner_user_id →
// organizations.created_by_user_id → organizations.org_id.
// Done as a single store call to keep the cascading resolver
// hot path tight.
func (h *Handlers) lookupOrgIDForProject(
	ctx context.Context, projectID string,
) string {
	proj, err := h.Store.GetProject(ctx, projectID)
	if err != nil || proj == nil || proj.OwnerUserID == "" {
		return ""
	}
	// ListOrganizationsForUser walks organization_members; the
	// project owner is always a member of any org they created
	// (member row written at create-time). For multi-org users
	// (rare today) the first match wins — same posture as the
	// dashboard's existing org-context picker.
	orgs, err := h.Store.ListOrganizationsForUser(ctx, proj.OwnerUserID)
	if err != nil || len(orgs) == 0 {
		return ""
	}
	return orgs[0].OrgID
}

// ResolveTimeBudgetMs returns the effective time_budget threshold
// in milliseconds, walking the cascade described above.
func (h *Handlers) ResolveTimeBudgetMs(
	ctx context.Context, projectID string,
) (int, ResolutionSource) {
	v, err := h.Store.GetProjectTimeBudgetMs(ctx, projectID)
	return h.resolveCascadingInt(
		ctx, projectID, v, err,
		OrgDefaultKeyTimeBudgetMs,
		HardcodedTimeBudgetMs,
	)
}

// ResolveProviderIncidentMinTenants returns the effective
// minimum-tenants threshold, walking the cascade.
func (h *Handlers) ResolveProviderIncidentMinTenants(
	ctx context.Context, projectID string,
) (int, ResolutionSource) {
	v, err := h.Store.GetProjectProviderIncidentMinTenants(ctx, projectID)
	return h.resolveCascadingInt(
		ctx, projectID, v, err,
		OrgDefaultKeyProviderIncidentMinTenants,
		HardcodedProviderIncidentMinTenants,
	)
}

// ResolveToolReturnValueMaxBytes returns the effective
// max-bytes threshold, walking the cascade.
func (h *Handlers) ResolveToolReturnValueMaxBytes(
	ctx context.Context, projectID string,
) (int, ResolutionSource) {
	v, err := h.Store.GetProjectToolReturnValueMaxBytes(ctx, projectID)
	return h.resolveCascadingInt(
		ctx, projectID, v, err,
		OrgDefaultKeyToolReturnValueMaxBytes,
		HardcodedToolReturnValueMaxBytes,
	)
}

// IsValidOrgDefaultKey reports whether the given key is one of
// the 3 known org-default keys. Used by the PUT handler to reject
// arbitrary keys at the API boundary.
func IsValidOrgDefaultKey(k string) bool {
	switch k {
	case OrgDefaultKeyTimeBudgetMs,
		OrgDefaultKeyProviderIncidentMinTenants,
		OrgDefaultKeyToolReturnValueMaxBytes:
		return true
	}
	return false
}

// LookupOrgIDForProjectExported exposes lookupOrgIDForProject for
// handler-level callers that need the org_id before invoking a
// resolver (e.g. the rollup endpoint).
func (h *Handlers) LookupOrgIDForProjectExported(
	ctx context.Context, projectID string,
) string {
	return h.lookupOrgIDForProject(ctx, projectID)
}

// _ keeps store imported when build configs strip everything else.
var _ store.SystemEvent