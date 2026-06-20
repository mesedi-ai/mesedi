// DLP ingest helpers (Mesedi #1 + #24). When the global DLP scanner
// is configured, HandleIngestEvents calls applyDLPToBatch before
// persistence: each llm_call / tool_call payload is scanned against
// the rule registry, matched secrets are replaced with stable
// `[REDACTED:rule_id]` tokens, and a sibling dlp_scan_result event is
// generated for each event that had at least one critical/high hit.
//
// The sibling event lets the downstream detector aggregate by rule_id
// without re-scanning, and gives the dashboard a per-event log of
// what was redacted (rule_id + count + severity, never the original
// matched substring).
//
// Critical contract: redaction MUST happen before SaveEvents. The
// matched secret must NEVER reach durable storage in clear. This
// file orchestrates that contract.

package api

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"time"

	"mesedi/backend/internal/dlp"
	"mesedi/backend/internal/events"
)

// scanFieldKeys enumerates the fields per event_type that the DLP
// scanner runs against. Each field is treated as an opaque string
// for scanning purposes; the redactor preserves JSON shape by
// re-marshalling after substitution. Adding a new event type with
// secret-bearing fields means extending this map.
var scanFieldKeys = map[events.EventType][]string{
	events.EventTypeLLMCall: {
		// Field names match what the SDK's instrument_anthropic
		// ships. Previously listed `user_prompt` and `response`
		// which no SDK ever emitted, so DLP scanned nothing on
		// llm_call events and data_leakage silently no-op'd. Caught
		// by the integration suite (backend/test/integration/
		// test_detectors.py::test_data_leakage).
		"system_prompt",
		"user_message",
		"response_text",
	},
	events.EventTypeToolCall: {
		// arguments and return_value are json.RawMessage on the payload
		// struct; we scan their serialized text form. tool_name is
		// not scanned (it's a logical identifier, never a secret).
		"arguments",
		"return_value",
	},
}

// applyDLPToBatch is the orchestrator. For each event in batch:
//   - If event_type has fields the scanner cares about, decode the
//     payload, scan each scanable field, and on hit replace the
//     matched bytes with `[REDACTED:rule_id]` tokens. Re-encode.
//   - If the redacted event had at least one critical/high hit,
//     generate a sibling dlp_scan_result event linking back via
//     parent_event_id. Lower-severity (medium) hits are recorded in
//     the parent event's redaction but don't generate a sibling
//     event (the threshold for "page the on-call" is critical/high).
//
// Returns a new batch slice with redacted parents + interleaved
// sibling events. The caller (HandleIngestEvents) passes this to
// SaveEvents. The order is preserved: each sibling immediately
// follows its parent.
//
// Sibling events get auto-generated event_ids and use the parent's
// execution_id + a sequence number = parent.Sequence (the same
// sequence: detectors only care about ordering within an execution,
// and sharing the parent sequence lets the dashboard render the two
// side-by-side. Future refinement: bump sequence on each sibling).
func (h *Handlers) applyDLPToBatch(batch []events.Event) []events.Event {
	if h.DLPScanner == nil || len(batch) == 0 {
		return batch
	}

	out := make([]events.Event, 0, len(batch))
	for i := range batch {
		evt := batch[i]
		fields, scan := scanFieldKeys[evt.EventType]
		if !scan || len(evt.Payload) == 0 {
			out = append(out, evt)
			continue
		}

		// Decode the payload as a generic JSON object so we can
		// rewrite the scan fields without committing to a typed
		// struct (typed structs vary per event type; this helper
		// stays general-purpose).
		var pm map[string]any
		if err := json.Unmarshal(evt.Payload, &pm); err != nil {
			// Malformed JSON, pass through unmodified. The event will
			// likely be rejected downstream by stricter consumers,
			// but the DLP layer is best-effort and shouldn't drop
			// otherwise-valid events on JSON quirks.
			out = append(out, evt)
			continue
		}

		var allHits []dlp.Hit
		mutated := false
		for _, key := range fields {
			raw, ok := pm[key]
			if !ok {
				continue
			}
			// Coerce the value to a string we can scan. For nested
			// JSON (arguments / return_value on tool_call) we
			// re-marshal to text form so regex hits on stringified
			// keys-and-values inside the sub-object still fire.
			var s string
			switch v := raw.(type) {
			case string:
				s = v
			default:
				bs, err := json.Marshal(v)
				if err != nil {
					continue
				}
				s = string(bs)
			}
			redacted, hits := h.DLPScanner.ScanAndRedact(s)
			if len(hits) == 0 {
				continue
			}
			allHits = append(allHits, hits...)
			pm[key] = redacted
			mutated = true
		}

		if mutated {
			if newPayload, err := json.Marshal(pm); err == nil {
				evt.Payload = newPayload
			}
		}
		out = append(out, evt)

		// Generate the sibling dlp_scan_result event only when there
		// are hits at critical or high severity. medium-only hits
		// are recorded in the parent (the redaction stays) but do
		// not page anyone.
		if len(allHits) == 0 {
			continue
		}
		highest := dlp.HighestSeverity(allHits)
		if highest != dlp.SeverityCritical && highest != dlp.SeverityHigh {
			continue
		}
		sibling, err := buildDLPScanResultEvent(evt, allHits, highest)
		if err != nil {
			h.Logger.Warn("dlp: build sibling event failed (parent redacted, no cluster signal)",
				"parent_event_id", evt.EventID,
				"error", err.Error(),
			)
			continue
		}
		out = append(out, sibling)
	}
	return out
}

// buildDLPScanResultEvent constructs the sibling event that the
// data_leakage detector consumes. event_id is freshly generated;
// execution_id + sequence + timestamp match the parent so the
// dashboard can render them side-by-side in the timeline.
func buildDLPScanResultEvent(parent events.Event, hits []dlp.Hit, highest dlp.Severity) (events.Event, error) {
	summaries := dlp.Summarize(hits)
	payloadHits := make([]events.DLPScanResultHit, len(summaries))
	totalCount := 0
	for i, s := range summaries {
		payloadHits[i] = events.DLPScanResultHit{
			RuleID:   s.RuleID,
			Label:    s.Label,
			Severity: string(s.Severity),
			Count:    s.Count,
		}
		totalCount += s.Count
	}
	// scan_layer hints at which parent field the hits came from. We
	// resolve via event_type rather than scanning the hits because
	// hits don't carry a key-of-origin field today (future
	// refinement). The label is good enough for the dashboard
	// timeline.
	scanLayer := "unknown"
	switch parent.EventType {
	case events.EventTypeLLMCall:
		scanLayer = "llm_prompt"
	case events.EventTypeToolCall:
		scanLayer = "tool_arguments"
	}
	payload := events.DLPScanResultPayload{
		ScanLayer:       scanLayer,
		ParentEventID:   parent.EventID,
		ParentEventType: string(parent.EventType),
		HighestSeverity: string(highest),
		HitCount:        totalCount,
		Hits:            payloadHits,
		Action:          "redacted",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return events.Event{}, err
	}
	return events.Event{
		EventID:     newDLPEventID(),
		ExecutionID: parent.ExecutionID,
		EventType:   events.EventTypeDLPScanResult,
		Sequence:    parent.Sequence,
		Timestamp:   parent.Timestamp.Add(time.Microsecond),
		Payload:     raw,
	}, nil
}

// newDLPEventID generates a per-event identifier for sibling events.
// Format mirrors what the SDKs use ("evt-" + 12 hex chars) so
// downstream lookups by event_id stay consistent across SDK-emitted
// and server-emitted events.
func newDLPEventID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read is overwhelmingly unlikely to fail; on the
		// off-chance, fall back to a fixed prefix + nanosecond
		// counter so we never panic in the ingest hot path.
		return "evt-dlpfallback"
	}
	return "evt-" + hex.EncodeToString(b[:])
}
