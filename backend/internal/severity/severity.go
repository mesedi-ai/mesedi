// Package severity holds the failure-class-to-severity defaults and
// related types for the severity routing feature (#261).
//
// Severity is a product opinion: how loud should a given failure
// class be when it first fires? We answer with three buckets:
//
//   - critical: page somebody now. Real-money harm, data loss, or
//     wrong answers reaching production. Customers usually route
//     these to PagerDuty / Slack #incidents / phone.
//
//   - warning: get to it within a day. Something is wrong but the
//     blast radius is bounded. Customers usually route to a normal
//     Slack channel or a daily digest.
//
//   - info: log it; review during weekly drift review. Customers
//     can mute these on most webhooks.
//
// The default mapping below is opinionated but overridable per-project
// via the project_class_severities table (#261). The dispatcher reads
// the override first; if none, falls back to Default(class) here.
package severity

import "strings"

// Severity is the canonical string form persisted to project_webhooks
// (severity_filter) and project_class_severities (severity column).
type Severity string

const (
	Critical Severity = "critical"
	Warning  Severity = "warning"
	Info     Severity = "info"
)

// All returns the three canonical severities in order from loudest to
// quietest. Used by the UI to render the severity filter checkboxes
// in a consistent order.
func All() []Severity {
	return []Severity{Critical, Warning, Info}
}

// Valid returns true if s is one of the three canonical severities.
// Used by handlers to reject malformed PUT bodies.
func Valid(s string) bool {
	switch Severity(s) {
	case Critical, Warning, Info:
		return true
	}
	return false
}

// Default returns the hardcoded severity for a failure class. The
// mapping below reflects "what most customers want by default" and
// is grouped by the harm shape:
//
//   - Real-money or data harm reaching production -> critical.
//     Crashes, tool failures, validator failures, prompt injection,
//     data leakage, tool schema drift, grounding failure, cascading
//     failure, coordination deadlock, sandbox escape.
//
//   - Cost / latency / quality regression with bounded blast
//     radius -> warning. Cost velocity, time budget, step count,
//     infrastructure throttled, context overflow, token waste,
//     provider incident, HITL timeout, HITL rejection spike.
//
//   - Behavioral signals worth reviewing later -> info. The loop
//     family, drift, semantic loop.
//
// Unknown failure classes fall through to warning, the sensible
// middle ground.
func Default(failureClass string) Severity {
	switch strings.ToLower(failureClass) {
	case "crashes",
		"tool_failures",
		"validator_failures",
		"prompt_injection",
		"data_leakage",
		"tool_schema_drift",
		"grounding_failure",
		"cascading_failure",
		"coordination_deadlock",
		"sandbox_escape":
		return Critical
	case "cost_velocity",
		"time_budget",
		"step_count",
		"infrastructure_throttled",
		"context_overflow",
		"token_waste",
		"provider_incident",
		"hitl_timeout",
		"hitl_rejection_spike":
		return Warning
	case "loops",
		"identical_call_loop",
		"similar_call_loop",
		"semantic_loop",
		"drift":
		return Info
	default:
		return Warning
	}
}

// ParseFilter takes the comma-separated severity_filter column value
// and returns the set of severities it represents. Empty input
// returns nil, which callers should treat as "all severities allowed"
// (backward compatible with pre-#261 webhooks that have no filter).
//
// Whitespace and case are normalized; unknown tokens are dropped.
// "critical, warning" and " WARNING ,critical" both parse to the
// same two-element set.
func ParseFilter(raw string) map[Severity]bool {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return nil
	}
	out := make(map[Severity]bool, 3)
	for _, tok := range strings.Split(raw, ",") {
		s := strings.ToLower(strings.TrimSpace(tok))
		if Valid(s) {
			out[Severity(s)] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// Allows returns true if a webhook with the given filter should
// receive an event of the given severity. Treats nil/empty filter
// as "allow all" so legacy webhooks keep firing on everything.
func Allows(filter map[Severity]bool, eventSeverity Severity) bool {
	if len(filter) == 0 {
		return true
	}
	return filter[eventSeverity]
}

// FormatFilter is the inverse of ParseFilter: takes a set and
// returns the canonical comma-separated form for persistence. Order
// is critical, warning, info so the column value is stable.
func FormatFilter(filter map[Severity]bool) string {
	if len(filter) == 0 {
		return ""
	}
	parts := make([]string, 0, 3)
	for _, s := range All() {
		if filter[s] {
			parts = append(parts, string(s))
		}
	}
	return strings.Join(parts, ",")
}
