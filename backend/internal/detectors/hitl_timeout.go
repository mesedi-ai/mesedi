// HITL-timeout detector (Mesedi #20).
//
// Fires when a human_intervention event (#19) on this execution
// indicates an SLA breach. Two firing conditions:
//
//  1. Explicit timeout. response_kind == "timeout" means the host
//     application gave up waiting before a human responded. This
//     is the customer-declared "we waited too long" signal and
//     the most actionable framing for an operator: it tells you
//     the human side of your loop is dropping requests.
//
//  2. SLA exceeded. The human DID respond, but
//     wait_duration_ms > sla_seconds * 1000. The agent proceeded
//     with the answer, so the execution itself might still
//     succeed, but the SLA was breached and a quality / response-
//     time review is warranted.
//
// First-match priority: explicit timeout wins over sla_exceeded.
// If an execution has multiple human_intervention events (e.g. an
// agent that asked for human input several times during a single
// run), the first event that meets either condition determines
// the cluster.
//
// Signature shape: "hitl_timeout:<reason>" where reason is either
// "explicit" or "sla_exceeded". Customers running both patterns
// see two distinct failure_groups; customers running only one
// see a single group. The dashboard surfaces the wait_duration
// and sla on the execution detail page so drilling in shows the
// specific breach magnitudes.
package detectors

import (
	"encoding/json"
)

// DetectHITLTimeoutAllMatches returns BOTH signatures when both
// firing conditions are present in the execution's human_intervention
// payloads (explicit timeout + SLA exceeded). Closes hitl_timeout.G3:
// the legacy first-match-only variant suppressed the SLA-exceeded
// cluster when an explicit timeout was also present; customers
// running multi-intervention executions where both kinds occur now
// see both clusters.
//
// At most 2 signatures returned (explicit + sla_exceeded). Ordering
// is deterministic: explicit before sla_exceeded when both fire.
func DetectHITLTimeoutAllMatches(payloads []json.RawMessage) []string {
	if len(payloads) == 0 {
		return nil
	}
	var sigs []string
	// Explicit timeout: any payload with response_kind=timeout.
	for _, raw := range payloads {
		var p struct {
			ResponseKind string `json:"response_kind"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.ResponseKind == "timeout" {
			sigs = append(sigs, "hitl_timeout:explicit")
			break
		}
	}
	// SLA exceeded: any payload whose wait_duration_ms exceeds
	// sla_seconds*1000. Only fires when sla_seconds > 0; without
	// a customer-declared SLA we cannot detect a breach.
	for _, raw := range payloads {
		var p struct {
			SLASeconds     int64 `json:"sla_seconds"`
			WaitDurationMs int64 `json:"wait_duration_ms"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.SLASeconds <= 0 {
			continue
		}
		if p.WaitDurationMs > p.SLASeconds*1000 {
			sigs = append(sigs, "hitl_timeout:sla_exceeded")
			break
		}
	}
	return sigs
}

// DetectHITLTimeout scans the supplied human_intervention payloads
// and returns the first failure signature. Returns ("", false)
// when no payload represents an SLA breach.
//
// LEGACY first-match-wins API kept for backward-compat with existing
// tests. The handler now uses DetectHITLTimeoutAllMatches per the
// all-matches-recorded wave (hitl_timeout.G3).
func DetectHITLTimeout(payloads []json.RawMessage) (signature string, detected bool) {
	if len(payloads) == 0 {
		return "", false
	}
	// First pass: explicit timeout. response_kind == "timeout" is
	// the strongest signal because it is the customer's own
	// declaration, not our heuristic.
	for _, raw := range payloads {
		var p struct {
			ResponseKind string `json:"response_kind"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.ResponseKind == "timeout" {
			return "hitl_timeout:explicit", true
		}
	}
	// Second pass: SLA exceeded. Only fires when sla_seconds is
	// set (we cannot detect a breach without a customer-declared
	// SLA). The wait_duration_ms is computed by the SDK at
	// response time and stored on the event.
	for _, raw := range payloads {
		var p struct {
			SLASeconds     int64 `json:"sla_seconds"`
			WaitDurationMs int64 `json:"wait_duration_ms"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.SLASeconds <= 0 {
			continue
		}
		if p.WaitDurationMs > p.SLASeconds*1000 {
			return "hitl_timeout:sla_exceeded", true
		}
	}
	return "", false
}
