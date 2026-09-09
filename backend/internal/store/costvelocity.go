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

import "context"

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
