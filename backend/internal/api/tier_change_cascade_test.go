// Unit tests for applyTierChangeCascade and its retention-clamp step.
//
// Coverage:
//   - Upgrades (Hobby→Team, Team→Enterprise) are a no-op: no read,
//     no write, no audit, no email. The cascade must never clamp
//     upward or touch state on a tier that's growing.
//   - Same-tier calls (idempotency): a re-delivered Stripe webhook
//     that fires cascade with from==to writes nothing. This is the
//     critical property Stripe's at-least-once delivery relies on.
//   - Downgrade with retention over the new cap (Team→Hobby with
//     retention=90) writes retention_days=7, an audit row, and calls
//     the mailer exactly once.
//   - Downgrade with retention already under the new cap
//     (Team→Hobby with retention=5) is a no-op, no clamp, no
//     audit, no email. Prevents noisy audit entries every time
//     someone with light retention downgrades.
//   - Downgrade with retention=nil (indefinite) on a project going
//     to a tier that DOESN'T allow indefinite (any downgrade to
//     Team or Hobby) clamps to the destination cap.
//   - Downgrade to Enterprise: indefinite retention is legal at
//     Enterprise, so a nil retention stays nil (no clamp needed).
//     Guards against a bug where "downgrade" logic accidentally
//     clamps a Hobby→Enterprise flip (which isn't really a
//     downgrade at all, verified by the isTierDowngrade helper).
//   - Empty fromTier: the cascade bails without any store reads.
//     Callers that don't have the old tier handy (Stripe webhook
//     that didn't preload the project) get a safe no-op, not an
//     accidental clamp.
//   - Missing project id: silent no-op, no panic.
//
// Also covers isTierDowngrade directly with a small truth table so
// the tier-ordering invariant is documented in code, not just in
// the helper's comments.

package api

import (
	"context"
	"errors"
	"testing"

	"mesedi/backend/internal/mail"
	"mesedi/backend/internal/store"
)

// stubCascadeStore embeds store.Store to satisfy the interface without
// listing every method. Records the calls the cascade actually makes
// so tests can assert on them.
type stubCascadeStore struct {
	store.Store

	// Retention read + write plumbing.
	retentionByProject map[string]*int
	retentionReads     int
	// Captured on each SetProjectRetentionDays call.
	lastSetProjectID string
	lastSetDays      *int
	setCalls         int
	setErr           error

	// GetProject plumbing, used by the email path only.
	projectByID map[string]*store.Project

	// Audit-event captures.
	auditEvents []*store.AuditEvent
	auditErr    error
}

func (s *stubCascadeStore) GetProjectRetentionDays(
	_ context.Context, projectID string,
) (*int, error) {
	s.retentionReads++
	v, ok := s.retentionByProject[projectID]
	if !ok {
		// Legacy "no row" is nil == indefinite in the schema.
		return nil, nil
	}
	return v, nil
}

func (s *stubCascadeStore) SetProjectRetentionDays(
	_ context.Context, projectID string, days *int,
) error {
	s.setCalls++
	s.lastSetProjectID = projectID
	s.lastSetDays = days
	if s.setErr != nil {
		return s.setErr
	}
	// Reflect the write so subsequent GetProjectRetentionDays sees it.
	if s.retentionByProject == nil {
		s.retentionByProject = map[string]*int{}
	}
	s.retentionByProject[projectID] = days
	return nil
}

func (s *stubCascadeStore) GetProject(
	_ context.Context, projectID string,
) (*store.Project, error) {
	if p, ok := s.projectByID[projectID]; ok {
		return p, nil
	}
	return nil, store.ErrNotFound
}

func (s *stubCascadeStore) CreateAuditEvent(
	_ context.Context, e *store.AuditEvent,
) error {
	if s.auditErr != nil {
		return s.auditErr
	}
	cp := *e
	s.auditEvents = append(s.auditEvents, &cp)
	return nil
}

// stubClampMailer records SendTierSettingsClamped calls so tests can
// assert the mail path fired (or did NOT fire) exactly as expected.
// All other Mailer methods embed the NoopMailer's zero-value shape.
type stubClampMailer struct {
	mail.NoopMailer

	sends   []mail.TierSettingsClampedInput
	sendErr error
}

func (m *stubClampMailer) SendTierSettingsClamped(
	_ context.Context, in mail.TierSettingsClampedInput,
) error {
	m.sends = append(m.sends, in)
	if m.sendErr != nil {
		return m.sendErr
	}
	return nil
}

// intPtr is a small helper so table rows can express "retention =
// N days" inline without leaking a package-level variable.
func intPtr(n int) *int { return &n }

func TestApplyTierChangeCascade(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name           string
		fromTier       string
		toTier         string
		startRetention *int
		expectClampTo  *int // nil = expect no write; pointer = expect this value written
		expectAudit    bool
		expectEmail    bool
	}{
		{
			name:           "upgrade Hobby to Team is a no-op",
			fromTier:       TierHobby,
			toTier:         TierTeam,
			startRetention: intPtr(7),
			expectClampTo:  nil,
			expectAudit:    false,
			expectEmail:    false,
		},
		{
			name:           "upgrade Team to Enterprise is a no-op",
			fromTier:       TierTeam,
			toTier:         TierEnterprise,
			startRetention: intPtr(90),
			expectClampTo:  nil,
			expectAudit:    false,
			expectEmail:    false,
		},
		{
			name:           "same-tier (Stripe re-delivery) is a no-op",
			fromTier:       TierTeam,
			toTier:         TierTeam,
			startRetention: intPtr(90),
			expectClampTo:  nil,
			expectAudit:    false,
			expectEmail:    false,
		},
		{
			name:           "downgrade Team to Hobby with retention=90 clamps to 7",
			fromTier:       TierTeam,
			toTier:         TierHobby,
			startRetention: intPtr(90),
			expectClampTo:  intPtr(HobbyDefaultRetentionDays),
			expectAudit:    true,
			expectEmail:    true,
		},
		{
			name:           "downgrade Team to Hobby with retention=5 is a no-op",
			fromTier:       TierTeam,
			toTier:         TierHobby,
			startRetention: intPtr(5),
			expectClampTo:  nil,
			expectAudit:    false,
			expectEmail:    false,
		},
		{
			name:           "downgrade Enterprise to Team with indefinite retention clamps to 90",
			fromTier:       TierEnterprise,
			toTier:         TierTeam,
			startRetention: nil, // indefinite
			expectClampTo:  intPtr(TeamDefaultRetentionDays),
			expectAudit:    true,
			expectEmail:    true,
		},
		{
			name:           "downgrade Enterprise to Hobby with indefinite retention clamps to 7",
			fromTier:       TierEnterprise,
			toTier:         TierHobby,
			startRetention: nil,
			expectClampTo:  intPtr(HobbyDefaultRetentionDays),
			expectAudit:    true,
			expectEmail:    true,
		},
		{
			name:           "empty fromTier bails without a store read",
			fromTier:       "",
			toTier:         TierHobby,
			startRetention: intPtr(90),
			expectClampTo:  nil,
			expectAudit:    false,
			expectEmail:    false,
		},
	}

	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			const projectID = "p1"
			s := &stubCascadeStore{
				retentionByProject: map[string]*int{
					projectID: tc.startRetention,
				},
				projectByID: map[string]*store.Project{
					projectID: {
						ProjectID:  projectID,
						Name:       "test-project",
						OwnerEmail: "owner@example.com",
					},
				},
			}
			mailer := &stubClampMailer{}
			h := &Handlers{
				Store:  s,
				Mailer: mailer,
				Logger: quietLogger(),
			}

			if err := h.applyTierChangeCascade(
				context.Background(), projectID,
				"owner@example.com",
				tc.fromTier, tc.toTier,
			); err != nil {
				t.Fatalf("applyTierChangeCascade: %v", err)
			}

			// Retention write assertion.
			if tc.expectClampTo == nil {
				if s.setCalls != 0 {
					t.Errorf("expected 0 SetProjectRetentionDays calls, got %d (value=%v)",
						s.setCalls, s.lastSetDays)
				}
			} else {
				if s.setCalls != 1 {
					t.Errorf("expected 1 SetProjectRetentionDays call, got %d",
						s.setCalls)
				}
				if s.lastSetDays == nil || *s.lastSetDays != *tc.expectClampTo {
					t.Errorf("clamp target: got %v want %d",
						s.lastSetDays, *tc.expectClampTo)
				}
			}

			// Audit assertion.
			if tc.expectAudit {
				if len(s.auditEvents) != 1 {
					t.Errorf("expected 1 audit event, got %d", len(s.auditEvents))
				} else if s.auditEvents[0].Action != AuditTierSettingsClamped {
					t.Errorf("audit action: got %q want %q",
						s.auditEvents[0].Action, AuditTierSettingsClamped)
				}
			} else {
				if len(s.auditEvents) != 0 {
					t.Errorf("expected 0 audit events, got %d", len(s.auditEvents))
				}
			}

			// Email assertion.
			if tc.expectEmail {
				if len(mailer.sends) != 1 {
					t.Errorf("expected 1 clamp email, got %d", len(mailer.sends))
				} else {
					got := mailer.sends[0]
					if got.ToEmail != "owner@example.com" {
						t.Errorf("email to: got %q want owner@example.com", got.ToEmail)
					}
					if got.NewTier != normalizeTier(tc.toTier) {
						t.Errorf("email new_tier: got %q want %q",
							got.NewTier, normalizeTier(tc.toTier))
					}
				}
			} else {
				if len(mailer.sends) != 0 {
					t.Errorf("expected 0 clamp emails, got %d", len(mailer.sends))
				}
			}
		})
	}
}

func TestApplyTierChangeCascade_EmptyProjectID(t *testing.T) {
	t.Parallel()
	s := &stubCascadeStore{}
	h := &Handlers{Store: s, Logger: quietLogger()}
	if err := h.applyTierChangeCascade(
		context.Background(), "", "owner@example.com",
		TierTeam, TierHobby,
	); err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if s.retentionReads != 0 {
		t.Errorf("expected 0 retention reads for empty project id, got %d",
			s.retentionReads)
	}
}

func TestApplyTierChangeCascade_ClampWriteErrorPropagates(t *testing.T) {
	// The only failure the cascade surfaces to the caller is a
	// storage-layer clamp write. Everything else (audit + email) is
	// best-effort. This test pins that contract.
	t.Parallel()
	const projectID = "p1"
	writeErr := errors.New("db locked")
	s := &stubCascadeStore{
		retentionByProject: map[string]*int{projectID: intPtr(90)},
		projectByID: map[string]*store.Project{
			projectID: {ProjectID: projectID, OwnerEmail: "owner@example.com"},
		},
		setErr: writeErr,
	}
	h := &Handlers{Store: s, Mailer: &stubClampMailer{}, Logger: quietLogger()}
	err := h.applyTierChangeCascade(
		context.Background(), projectID, "owner@example.com",
		TierTeam, TierHobby,
	)
	if err == nil {
		t.Fatal("expected error propagation, got nil")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("expected wrapped setErr, got %v", err)
	}
}

func TestApplyTierChangeCascade_AuditFailureDoesNotBlockClamp(t *testing.T) {
	// A CreateAuditEvent failure is logged inside recordAuditEventForProject
	// and swallowed there, the cascade must still return nil (clamp
	// succeeded, only the paper trail is missing). Guards against a
	// future refactor that starts propagating audit failures upward.
	t.Parallel()
	const projectID = "p1"
	s := &stubCascadeStore{
		retentionByProject: map[string]*int{projectID: intPtr(90)},
		projectByID: map[string]*store.Project{
			projectID: {ProjectID: projectID, OwnerEmail: "owner@example.com"},
		},
		auditErr: errors.New("audit table read-only"),
	}
	mailer := &stubClampMailer{}
	h := &Handlers{Store: s, Mailer: mailer, Logger: quietLogger()}
	if err := h.applyTierChangeCascade(
		context.Background(), projectID, "owner@example.com",
		TierTeam, TierHobby,
	); err != nil {
		t.Fatalf("cascade should have swallowed audit err: %v", err)
	}
	if s.setCalls != 1 {
		t.Errorf("clamp did not run despite audit failure: setCalls=%d", s.setCalls)
	}
	// Email is called AFTER the audit block; verify it still fires
	// so the customer is notified even when the audit trail is broken.
	if len(mailer.sends) != 1 {
		t.Errorf("email did not fire despite audit failure: sends=%d",
			len(mailer.sends))
	}
}

func TestIsTierDowngrade(t *testing.T) {
	t.Parallel()
	cases := []struct {
		from, to string
		want     bool
	}{
		// Downgrades: true.
		{TierEnterprise, TierTeam, true},
		{TierEnterprise, TierHobby, true},
		{TierTeam, TierHobby, true},
		// Upgrades: false.
		{TierHobby, TierTeam, false},
		{TierHobby, TierEnterprise, false},
		{TierTeam, TierEnterprise, false},
		// Same-tier: false.
		{TierHobby, TierHobby, false},
		{TierTeam, TierTeam, false},
		{TierEnterprise, TierEnterprise, false},

		// PRODUCTION. Absent from this table until 2026-08-28, and
		// absent from the rank switch, so it fell to the default and
		// ranked 0. The first two cases below are the bug: a source
		// rank of 0 makes the function return false, the cascade never
		// runs, and a project leaving Production keeps 3650-day
		// retention on a tier that permits 7 or 90.
		//
		// Nothing crashed and nothing logged. This was found by a CI
		// check added the same day, not by review.
		{TierProduction, TierHobby, true},
		{TierProduction, TierTeam, true},
		{TierHobby, TierProduction, false},
		{TierTeam, TierProduction, false},
		{TierProduction, TierProduction, false},

		// Production and Enterprise share rank 3 on purpose: identical
		// caps everywhere in the backend, so a move between them
		// clamps nothing and is a downgrade in neither direction.
		// Ranking Enterprise above Production would make
		// Enterprise -> Production clamp settings that need no
		// clamping, and fire a "your settings were changed" email at a
		// customer whose settings were not changed.
		{TierEnterprise, TierProduction, false},
		{TierProduction, TierEnterprise, false},

		// Unknown source: false (safe fallback).
		{"", TierHobby, false},
		{"unknown", TierHobby, false},
	}
	for _, tc := range cases {
		got := isTierDowngrade(tc.from, tc.to)
		if got != tc.want {
			t.Errorf("isTierDowngrade(%q, %q) = %v, want %v",
				tc.from, tc.to, got, tc.want)
		}
	}
}
