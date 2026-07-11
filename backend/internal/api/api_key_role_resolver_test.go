// Unit tests for the API-key role resolver and last-admin-key guard.
//
// Coverage:
//   - resolveKeyRoles maps user_id → role via GetOrganizationMember,
//     caches per-user lookups so N keys owned by 1 user cost 1 DB call,
//     and falls back to admin for pre-migration-014 rows with no
//     user_id (mirroring the rbac.go legacy path).
//   - resolveKeyRoles returns "unknown" ("") when the member row is
//     missing — the key still authenticates but we can't display a
//     role badge, and the guard MUST treat unknown as not-admin so
//     revoking such a key doesn't accidentally satisfy the check.
//   - wouldStrandProjectWithoutAdminKey correctly refuses only when
//     the target is admin AND is the last remaining admin among the
//     project's keys. Non-admin targets are always allowed (they don't
//     affect admin coverage). Admin targets are allowed when at least
//     one other admin remains.
//
// These are pure unit tests against a stub store; the postgres +
// sqlite CRUD paths for GetOrganizationMember + GetProjectTenantID are
// covered elsewhere (integration tests).
package api

import (
	"context"
	"testing"

	"mesedi/backend/internal/store"
)

// stubRoleStore embeds store.Store to satisfy the interface without
// listing every method. Only the two lookups the resolver uses are
// implemented. Every call is counted so we can assert the caching
// path (per-user role lookups memoized within one resolveKeyRoles
// invocation).
type stubRoleStore struct {
	store.Store

	// Tenant returned for the project. nil pointer or empty string
	// exercises the legacy "no tenant => admin" fallback.
	tenantPtr *string
	// Optional canned error from GetProjectTenantID.
	tenantErr error

	// Members keyed by userID. Missing entries return (nil, nil),
	// exercising the unknown-role path.
	membersByUserID map[string]*store.OrganizationMember
	// Optional canned error from GetOrganizationMember (per call).
	memberErr error

	// Call counters. tenantCalls asserts we only look up the
	// project's tenant once per resolveKeyRoles invocation. Per-user
	// memberCalls asserts we only look up each unique user_id once
	// (multiple keys owned by the same user share a cached role).
	tenantCalls  int
	memberCalls  map[string]int
}

func (s *stubRoleStore) GetProjectTenantID(_ context.Context, _ string) (*string, error) {
	s.tenantCalls++
	if s.tenantErr != nil {
		return nil, s.tenantErr
	}
	return s.tenantPtr, nil
}

func (s *stubRoleStore) GetOrganizationMember(
	_ context.Context, _ string, userID string,
) (*store.OrganizationMember, error) {
	if s.memberCalls == nil {
		s.memberCalls = map[string]int{}
	}
	s.memberCalls[userID]++
	if s.memberErr != nil {
		return nil, s.memberErr
	}
	m, ok := s.membersByUserID[userID]
	if !ok {
		return nil, nil
	}
	return m, nil
}

// strPtr is a helper so table entries can be written inline.
func strPtr(s string) *string { return &s }

func TestResolveKeyRoles(t *testing.T) {
	t.Parallel()

	type check struct {
		keyID string
		want  string
	}
	cases := []struct {
		name              string
		tenantPtr         *string
		members           map[string]*store.OrganizationMember
		keys              []*store.APIKey
		checks            []check
		wantTenantCalls   int
		wantMemberCalls   map[string]int
	}{
		{
			name:      "empty key set returns empty map, no DB calls",
			tenantPtr: strPtr("t1"),
			keys:      nil,
			checks:    nil,
			// resolveKeyRoles short-circuits before the tenant lookup
			// when there are no keys — no need to burn a round trip.
			wantTenantCalls: 0,
			wantMemberCalls: nil,
		},
		{
			name:      "legacy project with no tenant falls back to admin for every key",
			tenantPtr: nil,
			keys: []*store.APIKey{
				{KeyID: "k1", UserID: "u1"},
				{KeyID: "k2", UserID: "u2"},
				{KeyID: "k3"}, // legacy key
			},
			checks: []check{
				{"k1", apiKeyRoleAdmin},
				{"k2", apiKeyRoleAdmin},
				{"k3", apiKeyRoleAdmin},
			},
			wantTenantCalls: 1,
			// Legacy path skips the per-key member lookup entirely,
			// so we expect ZERO GetOrganizationMember calls.
			wantMemberCalls: nil,
		},
		{
			name:      "pre-migration-014 key (empty UserID) resolves to admin",
			tenantPtr: strPtr("t1"),
			members: map[string]*store.OrganizationMember{
				"u1": {UserID: "u1", Role: "read"},
			},
			keys: []*store.APIKey{
				{KeyID: "k1", UserID: "u1"},
				{KeyID: "legacy", UserID: ""}, // pre-014 shape
			},
			checks: []check{
				{"k1", apiKeyRoleRead},
				{"legacy", apiKeyRoleAdmin},
			},
			wantTenantCalls: 1,
			// Only "u1" gets looked up — "legacy" skips the DB.
			wantMemberCalls: map[string]int{"u1": 1},
		},
		{
			name:      "member with missing row resolves to unknown ('')",
			tenantPtr: strPtr("t1"),
			members: map[string]*store.OrganizationMember{
				"u1": {UserID: "u1", Role: "admin"},
			},
			keys: []*store.APIKey{
				{KeyID: "k1", UserID: "u1"},
				{KeyID: "orphan", UserID: "u-removed"},
			},
			checks: []check{
				{"k1", apiKeyRoleAdmin},
				{"orphan", apiKeyRoleUnknown},
			},
			wantTenantCalls: 1,
			wantMemberCalls: map[string]int{"u1": 1, "u-removed": 1},
		},
		{
			name:      "multiple keys owned by the same user share ONE member lookup",
			tenantPtr: strPtr("t1"),
			members: map[string]*store.OrganizationMember{
				"u1": {UserID: "u1", Role: "admin"},
				"u2": {UserID: "u2", Role: "write"},
			},
			keys: []*store.APIKey{
				{KeyID: "k1", UserID: "u1"},
				{KeyID: "k2", UserID: "u1"}, // same owner as k1
				{KeyID: "k3", UserID: "u1"}, // same owner
				{KeyID: "k4", UserID: "u2"},
			},
			checks: []check{
				{"k1", apiKeyRoleAdmin},
				{"k2", apiKeyRoleAdmin},
				{"k3", apiKeyRoleAdmin},
				{"k4", apiKeyRoleWrite},
			},
			wantTenantCalls: 1,
			// u1 counted once despite owning 3 keys — this is the
			// N-keys/1-owner caching correctness assertion.
			wantMemberCalls: map[string]int{"u1": 1, "u2": 1},
		},
		{
			// Migration 056: an admin can mint a scoped read/write key
			// under their own user_id. When the per-key Role column is
			// set, the resolver returns it verbatim WITHOUT looking up
			// the owner's org role — that's the whole point.
			name:      "explicit per-key Role overrides user-role AND skips member lookup",
			tenantPtr: strPtr("t1"),
			members: map[string]*store.OrganizationMember{
				"u-admin": {UserID: "u-admin", Role: "admin"},
			},
			keys: []*store.APIKey{
				// Key with explicit Role="read" owned by an admin —
				// resolver returns "read", not "admin".
				{KeyID: "kScoped", UserID: "u-admin", Role: "read"},
				// Same owner, no explicit role → falls through to
				// admin via the user-role lookup.
				{KeyID: "kInherit", UserID: "u-admin"},
			},
			checks: []check{
				{"kScoped", apiKeyRoleRead},
				{"kInherit", apiKeyRoleAdmin},
			},
			wantTenantCalls: 1,
			// Only kInherit triggers the DB lookup; kScoped short-
			// circuits on Role. u-admin counted once.
			wantMemberCalls: map[string]int{"u-admin": 1},
		},
		{
			// Belt-and-suspenders: explicit Role wins even when the key
			// has no UserID (which would otherwise fall through to the
			// legacy-admin path).
			name:      "explicit per-key Role beats the legacy no-user-id admin fallback",
			tenantPtr: strPtr("t1"),
			keys: []*store.APIKey{
				{KeyID: "kLegacyScoped", UserID: "", Role: "write"},
			},
			checks: []check{
				{"kLegacyScoped", apiKeyRoleWrite},
			},
			wantTenantCalls: 1,
			wantMemberCalls: nil,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &stubRoleStore{
				tenantPtr:       tc.tenantPtr,
				membersByUserID: tc.members,
			}
			h := &Handlers{Store: s}
			got := h.resolveKeyRoles(context.Background(), "p1", tc.keys)

			for _, c := range tc.checks {
				if got[c.keyID] != c.want {
					t.Errorf("keyID=%s got role=%q want %q",
						c.keyID, got[c.keyID], c.want)
				}
			}
			if s.tenantCalls != tc.wantTenantCalls {
				t.Errorf("GetProjectTenantID calls: got %d want %d",
					s.tenantCalls, tc.wantTenantCalls)
			}
			if len(tc.wantMemberCalls) == 0 && len(s.memberCalls) != 0 {
				t.Errorf("expected no member calls, got %v", s.memberCalls)
			}
			for userID, want := range tc.wantMemberCalls {
				if got := s.memberCalls[userID]; got != want {
					t.Errorf("GetOrganizationMember(%s) calls: got %d want %d",
						userID, got, want)
				}
			}
		})
	}
}

func TestWouldStrandProjectWithoutAdminKey(t *testing.T) {
	t.Parallel()

	// Fixed setup: tenant exists, u-admin is admin, u-write is write,
	// u-read is read. Each test case varies which keys exist and
	// which one we're targeting for revoke.
	baseMembers := map[string]*store.OrganizationMember{
		"u-admin1": {UserID: "u-admin1", Role: "admin"},
		"u-admin2": {UserID: "u-admin2", Role: "admin"},
		"u-write":  {UserID: "u-write", Role: "write"},
		"u-read":   {UserID: "u-read", Role: "read"},
	}

	cases := []struct {
		name        string
		keys        []*store.APIKey
		targetKeyID string
		wantStrand  bool
	}{
		{
			name: "revoking non-admin key is always allowed even if it's the only key",
			// Solo write key. Guard is not about total-count-1 (that's
			// the separate last-key guard in HandleRevokeAPIKey); this
			// guard is strictly about admin-role coverage.
			keys: []*store.APIKey{
				{KeyID: "kW", UserID: "u-write"},
			},
			targetKeyID: "kW",
			wantStrand:  false,
		},
		{
			name: "revoking read key when admin keys remain is allowed",
			keys: []*store.APIKey{
				{KeyID: "kA", UserID: "u-admin1"},
				{KeyID: "kR", UserID: "u-read"},
			},
			targetKeyID: "kR",
			wantStrand:  false,
		},
		{
			name: "revoking the ONLY admin key strands the project",
			keys: []*store.APIKey{
				{KeyID: "kA", UserID: "u-admin1"},
				{KeyID: "kW", UserID: "u-write"},
				{KeyID: "kR", UserID: "u-read"},
			},
			targetKeyID: "kA",
			wantStrand:  true,
		},
		{
			name: "revoking one of TWO admin keys is allowed",
			keys: []*store.APIKey{
				{KeyID: "kA1", UserID: "u-admin1"},
				{KeyID: "kA2", UserID: "u-admin2"},
			},
			targetKeyID: "kA1",
			wantStrand:  false,
		},
		{
			name: "unknown-role target does NOT satisfy the admin check",
			// If the target key's owner was removed from the org
			// (unknown role), and there's only one true admin remaining,
			// revoking the unknown key must NOT be blocked — it's not
			// admin. The remaining true admin (kA) stays intact.
			keys: []*store.APIKey{
				{KeyID: "kA", UserID: "u-admin1"},
				{KeyID: "orphan", UserID: "u-removed"},
			},
			targetKeyID: "orphan",
			wantStrand:  false,
		},
		{
			name: "legacy no-user-id keys count as admin: guard blocks revoking the last one",
			// Two keys: one legacy (auto-admin), one write. Revoking
			// the legacy key would leave zero admin coverage → block.
			keys: []*store.APIKey{
				{KeyID: "kLegacy"}, // no UserID → admin per legacy path
				{KeyID: "kW", UserID: "u-write"},
			},
			targetKeyID: "kLegacy",
			wantStrand:  true,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := &stubRoleStore{
				tenantPtr:       strPtr("t1"),
				membersByUserID: baseMembers,
			}
			h := &Handlers{Store: s}
			got := h.wouldStrandProjectWithoutAdminKey(
				context.Background(), "p1", tc.targetKeyID, tc.keys,
			)
			if got != tc.wantStrand {
				t.Errorf("wouldStrandProjectWithoutAdminKey got=%v want=%v",
					got, tc.wantStrand)
			}
		})
	}
}
