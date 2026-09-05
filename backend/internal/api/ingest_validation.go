package api

import (
	"log/slog"
	"time"

	"mesedi/backend/internal/events"
)

// validateIngestBatch is the first pass over a submitted event batch:
// it drops events that cannot be stored and defaults the ones that can.
//
// Returns the accepted events and how many were dropped. Rejection is
// PER EVENT, never per batch, so one malformed event does not cost a
// customer the other ninety-nine in the same request. Every rejection is
// logged with the index and ids, because an event silently vanishing
// from an agent's record is the worst outcome available here — the
// record would look complete and be short.
//
// Extracted from HandleIngestEvents so these rules can be tested. They
// had no test of any kind: there is no Go test for HandleIngestEvents at
// all, and building one needs a store fake large enough that the rules
// themselves would still not be what was being exercised. Four rules
// that decide whether a customer's data is kept deserve better than
// being reachable only through a handler nobody can construct.
func validateIngestBatch(
	batch []events.Event, logger *slog.Logger,
) (accepted []events.Event, rejected int) {
	accepted = make([]events.Event, 0, len(batch))

	for i := range batch {
		evt := &batch[i]

		if evt.EventID == "" || evt.ExecutionID == "" || evt.EventType == "" {
			rejected++
			logger.Warn("event rejected: required field missing",
				"event_index", i,
				"event_id", evt.EventID,
				"execution_id", evt.ExecutionID,
				"event_type", evt.EventType,
			)
			continue
		}

		// Reserved types. Mesedi writes these itself during ingest, so a
		// customer submitting one is either confused or arranging for
		// their own events to be treated as ours.
		//
		// This rejection is what makes the integrity filter downstream
		// SOUND rather than merely convenient. That filter excludes
		// backend-minted events from the customer's sequence set; if a
		// customer could mint them, they could exclude a genuine
		// duplicate from their own integrity check and then be handed a
		// Mesedi report stating the record is clean. Mesedi would be the
		// party making the false statement. See events.IsBackendMinted.
		if events.IsBackendMinted(evt.EventType) {
			rejected++
			logger.Warn("event rejected: reserved event type",
				"event_index", i,
				"event_id", evt.EventID,
				"execution_id", evt.ExecutionID,
				"event_type", evt.EventType,
				"detail", "Mesedi writes this type itself; it cannot be submitted",
			)
			continue
		}

		// Per-event size cap. SDK-side truncation should keep every
		// payload well under this floor; reaching it implies an older SDK
		// or a custom integration that bypassed the truncation helper.
		// The individual event is rejected so one oversized payload does
		// not poison the rest of the batch.
		if payloadOverCap(evt) {
			rejected++
			logger.Warn("event rejected: payload exceeds cap",
				"event_index", i,
				"event_id", evt.EventID,
				"execution_id", evt.ExecutionID,
				"event_type", evt.EventType,
				"payload_bytes", len(evt.Payload),
				"max_bytes", MaxEventPayloadBytes,
			)
			continue
		}

		if evt.Timestamp.IsZero() {
			evt.Timestamp = time.Now().UTC()
		}
		accepted = append(accepted, *evt)
	}
	return accepted, rejected
}
