package api

// Tier-change cascade helper.
//
// Purpose: when a project's tier flips (customer downgrades via
// Stripe, subscription is deleted, or platform staff overrides tier
// from the admin console), per-project settings that were legal under
// the old tier may exceed the new tier's caps. The retention_days
// column is the canonical case: a Team-tier project keeps 90-day
// retention until it cancels, at which point the row still says
// retention_days=90 but the tier is now Hobby (cap 7). The nightly
// retention scheduler then silently keeps 90 days of data for a
// project that pays for 7 — customer looks like they're getting a
// free ride, and the marketing pricing card looks like it's lying.
//
// This helper closes that loophole by running on every tier-change
// event. It reads the current retention, clamps to the new tier's
// cap if over, writes an audit_events row so the customer sees why
// their retention changed (Team+ selling point: full auditability),
// and fires an email if retention was reduced (so the customer isn't
// surprised when their old data disappears at the next scheduler tick).
//
// Design principles (kept deliberately narrow for this slice):
//
//   1. One function, called from every tier-change site. The store
//      layer stays pure CRUD; business rules live in the API layer.
//      A CI check in tools/check-tier-constants.sh enforces that
//      every UpdateProjectTier / UpdateProjectBilling call site in
//      the api package has applyTierChangeCascade within 20 lines.
//
//   2. Best-effort side effects. Audit + email failures are logged
//      at WARN and swallowed; the retention clamp itself is the only
//      operation whose failure propagates to the caller. Rationale:
//      the tier flip has already happened by the time this runs,
//      and rolling back would leave the customer in a worse state
//      (paid for something, then couldn't cancel).
//
//   3. Upgrades are a no-op. Team→Enterprise doesn't need to clamp
//      anything; retention_days=90 is still valid at Enterprise.
//      Only downgrades enter the clamp path.
//
//   4. Idempotent. Called twice with the same (from, to) pair, the
//      second call sees retention already at or below the cap and
//      writes nothing. Stripe webhooks are re-delivered on ambiguous
//      2xx responses; this must not double-clamp or send two emails.
//
// Extension path: when the next tier-capped setting lands
// (custom-pattern count, model-window count, seat count, etc.), it
// gets added inside applyTierChangeCascade as an independent step.
// Each step reads its own current value, compares to the destination
// tier's cap, clamps, and logs its own audit row. Steps are
// independent — a failure in one step doesn't block the others.

import (
	"context"
	"fmt"

	"mesedi/backend/internal/mail"
)

// Audit + actor sentinels for the cascade path. These are exported
// so tests can assert against the same strings the runtime writes.
const (
	// AuditActorBillingSystem is the synthetic actor written when an
	// automated Stripe webhook fires the cascade. Customers see this
	// in their audit log's ACTOR column and can distinguish it from
	// their own dashboard actions or from AuditActorPlatformAdmin.
	AuditActorBillingSystem = "Mesedi billing system"
	// AuditTierSettingsClamped is the audit action written each time
	// the cascade reduces a per-project setting to fit a new tier's
	// cap. Metadata carries {setting, from_value, to_value, from_tier,
	// to_tier} so the audit log page can render a human-readable row.
	AuditTierSettingsClamped = "project.tier_settings_clamped"
)

// applyTierChangeCascade runs the settings-clamp cascade for a tier
// change on projectID. Called from every site that flips a project's
// tier (admin manual override, Stripe subscription updated, Stripe
// subscription deleted).
//
// Parameters:
//   - fromTier: the tier the project WAS on before the flip. Callers
//     that don't have the old tier handy (subscription webhook that
//     didn't load the project first) can pass "" — the cascade still
//     runs based on toTier's cap alone; the audit metadata will just
//     have an empty from_tier.
//   - toTier: the tier the project IS on now (post-flip).
//   - actorEmail: the identity to record in the audit row. Use the
//     customer's own OwnerEmail when the flip was customer-initiated
//     (Stripe cancel), AuditActorPlatformAdmin for admin overrides,
//     or AuditActorBillingSystem when neither is available.
//
// Return value: only errors from the STORAGE-LAYER clamp write
// propagate. Audit + email failures are logged at WARN and swallowed.
// A nil-return from an upgrade path (from lower to higher tier) is
// also normal — that path skips the clamp entirely.
func (h *Handlers) applyTierChangeCascade(
	ctx context.Context,
	projectID, actorEmail, fromTier, toTier string,
) error {
	fromNorm := normalizeTier(fromTier)
	toNorm := normalizeTier(toTier)

	// No-op guards.
	if projectID == "" {
		return nil
	}
	if fromNorm == toNorm {
		// Idempotency: same-tier "change" writes nothing. Also covers
		// the case where Stripe re-delivers a webhook after we already
		// processed it.
		return nil
	}
	if !isTierDowngrade(fromNorm, toNorm) {
		// Upgrades (Hobby→Team, Hobby→Enterprise, Team→Enterprise)
		// never need a clamp — the destination tier's caps are always
		// looser. This branch also protects the empty-fromTier case:
		// isTierDowngrade returns false when fromNorm is empty, so a
		// caller that didn't preload the old tier gets a safe no-op
		// rather than an accidental clamp based on incomplete info.
		return nil
	}

	// --- Step 1: retention_days ---
	if err := h.clampRetentionForTier(ctx, projectID, actorEmail, fromNorm, toNorm); err != nil {
		// Clamp write failure is the ONLY thing we surface. Everything
		// else (audit row, email) is best-effort.
		return fmt.Errorf("cascade: clamp retention: %w", err)
	}

	// Future settings clamps land here as additional steps. Each
	// step is independent; a failure in one doesn't skip the next.
	// See doc comment at file top for the extension pattern.

	return nil
}

// clampRetentionForTier reads the project's current retention_days,
// compares it to the destination tier's cap, and clamps if needed.
// Writes an audit row and fires an email on any clamp. Extracted from
// applyTierChangeCascade so future settings can follow the same shape.
func (h *Handlers) clampRetentionForTier(
	ctx context.Context,
	projectID, actorEmail, fromTier, toTier string,
) error {
	cap, allowIndefinite := tierRetentionCap(toTier)
	current, err := h.Store.GetProjectRetentionDays(ctx, projectID)
	if err != nil {
		return fmt.Errorf("read retention: %w", err)
	}

	// Compute the new value. Cases:
	//   - current is nil (indefinite): only Enterprise supports this;
	//     downgrading to anything else clamps to that tier's cap.
	//   - current > cap: clamp to cap.
	//   - current <= cap: no-op.
	var oldForAudit string
	var newForAudit string
	var shouldClamp bool
	newDays := cap
	switch {
	case current == nil:
		if allowIndefinite {
			return nil
		}
		oldForAudit = "indefinite"
		newForAudit = fmt.Sprintf("%d days", cap)
		shouldClamp = true
	case *current > cap:
		oldForAudit = fmt.Sprintf("%d days", *current)
		newForAudit = fmt.Sprintf("%d days", cap)
		shouldClamp = true
	default:
		// Already at or below the cap — nothing to do.
		return nil
	}
	if !shouldClamp {
		return nil
	}

	// Persist the clamp. This is the only operation whose failure
	// aborts the cascade — a partial state (settings-clamp-half-
	// applied) is worse than the caller retrying the whole cascade.
	if err := h.Store.SetProjectRetentionDays(ctx, projectID, &newDays); err != nil {
		return fmt.Errorf("write retention: %w", err)
	}

	// Best-effort audit trail. A write failure here is logged but does
	// NOT roll back the clamp — the customer's data-retention posture
	// is now correct even if we didn't manage to record the reason.
	h.recordAuditEventForProject(
		ctx, projectID, actorEmail,
		AuditTierSettingsClamped,
		"retention_days", projectID,
		map[string]any{
			"setting":    "retention_days",
			"from_value": oldForAudit,
			"to_value":   newForAudit,
			"from_tier":  fromTier,
			"to_tier":    toTier,
		},
	)

	// Best-effort customer email. Skipped when we don't have a project
	// owner email (the caller either didn't load the project or the
	// row genuinely has no owner). The email is a courtesy, not a
	// legal requirement — the audit row above is the record of truth.
	h.sendTierClampEmail(ctx, projectID, oldForAudit, newForAudit, toTier)

	return nil
}

// sendTierClampEmail looks up the project owner's email and fires a
// SendTierSettingsClamped email through the configured Mailer. Best-
// effort: any error (project load fails, mailer disabled, Resend
// returns 500) is logged at WARN and swallowed. Never blocks the
// cascade.
func (h *Handlers) sendTierClampEmail(
	ctx context.Context,
	projectID, oldRetention, newRetention, toTier string,
) {
	if h.Mailer == nil {
		return
	}
	proj, err := h.Store.GetProject(ctx, projectID)
	if err != nil || proj == nil || proj.OwnerEmail == "" {
		if h.Logger != nil {
			h.Logger.Warn("tier-cascade: skipping clamp email, no owner email",
				"project_id", projectID)
		}
		return
	}
	in := mail.TierSettingsClampedInput{
		ToEmail:      proj.OwnerEmail,
		ProjectName:  proj.Name,
		Setting:      "retention",
		OldValue:     oldRetention,
		NewValue:     newRetention,
		NewTier:      toTier,
		DashboardURL: h.DashboardURL,
	}
	if err := h.Mailer.SendTierSettingsClamped(ctx, in); err != nil && h.Logger != nil {
		h.Logger.Warn("tier-cascade: clamp email send failed",
			"project_id", projectID,
			"error", err.Error())
	}
}

// isTierDowngrade returns true when toNorm is strictly lower than
// fromNorm in the tier ordering Hobby < Team < Enterprise. Empty
// fromNorm returns false (safe fallback: no clamp when we can't
// establish a direction).
func isTierDowngrade(fromNorm, toNorm string) bool {
	rank := func(t string) int {
		switch t {
		case TierEnterprise:
			return 3
		case TierTeam:
			return 2
		case TierHobby:
			return 1
		default:
			return 0
		}
	}
	f, to := rank(fromNorm), rank(toNorm)
	if f == 0 {
		// Unknown source tier — bail rather than guess.
		return false
	}
	return to < f
}
