package store

// Cost-velocity signatures and grouping, gathered here from
// sqlite.go during #48 (attribution) because both store files are
// over the size ratchet and this is the cluster #48 changes.
//
// #48's finding, from the 2026-09-07 incident radar: the absolute
// detector's signature carried a dollar magnitude and nothing else,
// so spend from a credential or tenant never seen before was
// indistinguishable from routine spend of the same size; a new actor
// folded into the existing cost_$10+ group as a "recurrence". The
// fix makes the actor part of the signature: a never-seen identity
// crossing the threshold creates a NEW failure group, and the
// existing new-group webhook escalation fires structurally. The RATE
// detector stays project-wide on purpose, burn rate is a property of
// the project's aggregate spend, not of one actor.
//
// Baselining (spend accelerating past learned normal) remains open
// and is a larger, separate piece of #48.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CostVelocityIdentity resolves the actor dimension for attribution:
// the customer-supplied tenant when present (their own end-user
// attribution, matching how the cost-by-tenant report thinks), else
// the API key that ran the execution, else a fixed marker so
// unattributed rows still cluster deterministically.
func CostVelocityIdentity(tenantID, apiKeyID string) string {
	switch {
	case tenantID != "":
		return "tenant:" + tenantID
	case apiKeyID != "":
		return "key:" + apiKeyID
	default:
		return "unattributed"
	}
}

// CostVelocityAttributedSignature is the absolute detector's group
// signature since #48: magnitude bucket plus actor, separated by a
// pipe. "cost_$10+|tenant:acme-prod" and "cost_$10+|key:key_ab12"
// are distinct groups, which is the entire point.
func CostVelocityAttributedSignature(costUSD float64, identity string) string {
	return CostVelocitySignature(costUSD) + "|" + identity
}

// Baseline (learned-normal) form of the rate detector. The fixed
// thresholds above require an operator to pick a number higher than
// their own busiest legitimate day, which means an attacker sitting
// just under that number spends indefinitely. This form learns the
// project's own normal from its trailing week and fires on the
// MULTIPLE of normal instead. Owner-decided defaults, 2026-09-09:
// ten times normal fires; a project is silent until it is three days
// old with fifty runs in the trailing week (no crying wolf while
// learning; the fixed thresholds still protect it); and nothing
// fires below half a dollar a minute, because a near-idle project
// jumping fifty-fold to five cents a minute is not an incident.
// Per-project configuration is a deliberate later step, when a
// customer asks.
const (
	CostVelocityBaselineMultiplier     = 10.0
	CostVelocityBaselineWindowDays     = 7
	CostVelocityBaselineMinAgeHours    = 72
	CostVelocityBaselineMinRuns        = 50
	CostVelocityBaselineFloorUSDPerMin = 0.50
)

// CostVelocityBaselineExceeded is the pure decision: given the
// current burn rate, the learned baseline rate, and the project's
// maturity, report the multiple of normal and whether to fire. A
// zero or negative baseline never fires; acceleration past an
// unknown normal is the fixed thresholds' job.
func CostVelocityBaselineExceeded(ratePerMin, baselineRatePerMin float64, runsInWindow int, projectAgeHours float64) (multiple float64, fired bool) {
	if baselineRatePerMin <= 0 {
		return 0, false
	}
	multiple = ratePerMin / baselineRatePerMin
	if projectAgeHours < CostVelocityBaselineMinAgeHours || runsInWindow < CostVelocityBaselineMinRuns {
		return multiple, false
	}
	if ratePerMin < CostVelocityBaselineFloorUSDPerMin {
		return multiple, false
	}
	return multiple, multiple >= CostVelocityBaselineMultiplier
}

// CostVelocityBaselineSignature buckets the multiple-of-normal into
// order-of-magnitude signatures, so ten-times-normal and a
// thousand-times-normal read differently at a glance.
func CostVelocityBaselineSignature(multiple float64) string {
	switch {
	case multiple < 100:
		return "baseline_x10+"
	case multiple < 1000:
		return "baseline_x100+"
	default:
		return "baseline_x1000+"
	}
}

// GroupCostVelocityBaseline upserts a failure_group with
// failure_class=cost_velocity and a multiple-of-normal signature.
// Same idempotency contract as the sibling grouping methods; the
// handler owns the decision (CostVelocityBaselineExceeded), the
// store owns signature and storage.
func (s *SQLiteStore) GroupCostVelocityBaseline(
	ctx context.Context,
	executionID, projectID string,
	multiple float64,
) (isNew bool, err error) {
	signature := CostVelocityBaselineSignature(multiple)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// CostVelocitySignature buckets execution cost into order-of-magnitude
// signatures so high-cost runs cluster sensibly. Independent of the
// per-project threshold: a customer who lowers their threshold to
// $0.05 will see "cost_$0.01+" signatures fire; a customer at the
// default $1.00 will see "cost_$1+" and "cost_$10+" most often.
func CostVelocitySignature(costUSD float64) string {
	switch {
	case costUSD < 0.01:
		return "cost_$0.001+"
	case costUSD < 0.10:
		return "cost_$0.01+"
	case costUSD < 1.00:
		return "cost_$0.10+"
	case costUSD < 10.00:
		return "cost_$1+"
	default:
		return "cost_$10+"
	}
}

// CostVelocityRateSignature buckets observed burn-rate ($/minute) into
// order-of-magnitude signatures, mirroring CostVelocitySignature.
// Independent of the per-project rate threshold: a customer at the
// default $5/min threshold who hits $50/min sees "rate_$10+_per_min",
// distinct from someone barely tripping at $5/min ("rate_$5+_per_min").
// Buckets escalate so the dashboard naturally separates "barely
// anomalous" from "runaway" without manual triage.
func CostVelocityRateSignature(ratePerMinUSD float64) string {
	switch {
	case ratePerMinUSD < 1.00:
		return "rate_$0.10+_per_min" // matches the per-project floor
	case ratePerMinUSD < 5.00:
		return "rate_$1+_per_min"
	case ratePerMinUSD < 10.00:
		return "rate_$5+_per_min" // matches default threshold
	case ratePerMinUSD < 100.00:
		return "rate_$10+_per_min"
	case ratePerMinUSD < 1000.00:
		return "rate_$100+_per_min"
	default:
		return "rate_$1000+_per_min"
	}
}

// GroupCostVelocity upserts a failure_group with
// failure_class=cost_velocity and an attributed, cost-bucketed
// signature (#48). identity comes from CostVelocityIdentity. Same
// idempotency contract as the other grouping methods: if the
// execution is already in a higher-priority group (crash, loop,
// tool/validator failure), this is a no-op.
//
// The caller (HandleUpdateExecution) is responsible for the
// threshold check using the per-project value from
// GetProjectCostVelocityThresholdUSD. The store layer no longer
// enforces a threshold, the policy lives in the handler, the
// storage in the store. Mirrors the time_budget pattern.
func (s *SQLiteStore) GroupCostVelocity(
	ctx context.Context,
	executionID, projectID string,
	costUSD float64,
	identity string,
) (isNew bool, err error) {
	signature := CostVelocityAttributedSignature(costUSD, identity)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// GroupCostVelocityRate upserts a failure_group with
// failure_class=cost_velocity and a RATE-bucketed signature
// (rate_$X+_per_min). Companion to GroupCostVelocity, same class,
// different signature so the dashboard renders rate-based bursts as
// a distinct cluster from per-execution magnitude. Handler is
// responsible for the threshold check using the per-project value
// from GetProjectCostVelocityRateConfig. Deliberately NOT
// attributed: the rate is the project's aggregate burn.
//
// Why a separate store method instead of overloading GroupCostVelocity
// with a "rate" boolean: keeps the signature-computation responsibility
// in the store (single source of truth) AND makes the call site
// self-documenting at the handler. The cost and the rate are
// different quantities measured in different units; conflating them
// behind one parameter would invite future signature bugs.
func (s *SQLiteStore) GroupCostVelocityRate(
	ctx context.Context,
	executionID, projectID string,
	ratePerMinUSD float64,
) (isNew bool, err error) {
	signature := CostVelocityRateSignature(ratePerMinUSD)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCostVelocity, signature)
}

// EarliestExecutionStart returns the project's first execution's
// started_at (zero time when none exist). MIN over the existing
// (project_id, started_at) index; no new index needed.
func (s *SQLiteStore) EarliestExecutionStart(ctx context.Context, projectID string) (time.Time, error) {
	// Scan as text and parse by hand: MIN() erases the column's
	// declared type, so the driver's own time conversion does not
	// apply to the aggregate the way it does to a plain column read.
	// Two layouts because pre-canonicalization development databases
	// still hold rows in Go's Time.String() form.
	var raw sql.NullString
	err := s.db.QueryRowContext(ctx,
		`SELECT MIN(started_at) FROM executions WHERE project_id = ?`,
		projectID,
	).Scan(&raw)
	if err != nil {
		return time.Time{}, fmt.Errorf("earliest execution start: %w", err)
	}
	if !raw.Valid || raw.String == "" {
		return time.Time{}, nil
	}
	for _, layout := range []string{
		"2006-01-02 15:04:05.999999999-07:00",     // canonical since the _time_format fix
		"2006-01-02 15:04:05.999999999 -0700 MST", // legacy Time.String() rows
	} {
		if t, perr := time.Parse(layout, raw.String); perr == nil {
			return t.UTC(), nil
		}
	}
	return time.Time{}, fmt.Errorf("earliest execution start: unparseable started_at %q", raw.String)
}
