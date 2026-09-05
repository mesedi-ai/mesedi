package events

// Event types Mesedi writes itself, which a customer may not submit.
//
// WHY THIS DISTINCTION HAS TO EXIST
//
// Almost every event in an execution is the customer's claim about what
// their agent did. A few are not: Mesedi mints them during ingest to
// record something Mesedi observed. Today that is one type, the sibling
// event written when a DLP scan finds a critical or high-severity hit.
//
// Those two kinds of event must be told apart, because checks that ask
// "does this customer's record contradict itself?" have to run over the
// customer's events and not over Mesedi's annotations of them. The DLP
// sibling deliberately carries its parent's sequence number so the two
// render side by side, which the record_integrity detector then read as
// the customer having claimed one sequence twice. Every DLP hit produced
// a duplicate_sequence signal, so a customer whose agent leaked a secret
// was ALSO told their records were corrupt, by us, about an event we
// wrote ourselves.
//
// WHY IT IS ENFORCED AT INGEST AND NOT ONLY FILTERED AT READ TIME
//
// Filtering by type alone would be worse than the bug. The ingest
// endpoint validates no event types at all, so a customer could label
// their own events dlp_scan_result and have a genuine duplicate quietly
// excluded from the integrity check, then hand an auditor a Mesedi
// report stating the record is clean. Mesedi would be the party making
// the false statement, which is the exact failure this product exists to
// prevent.
//
// So the type is RESERVED: submitting one is rejected at ingest, which
// makes the read-time filter sound rather than merely convenient. The
// two halves are one mechanism and neither works alone.
//
// The chain is unaffected either way. attest.Compute folds every event
// including the siblings, and tiebreaks on EventID precisely because
// duplicate sequences were anticipated there.

// IsBackendMinted reports whether Mesedi writes this event type itself.
//
// Adding a type here means two things at once: ingest starts rejecting
// it from customers, and integrity checks stop counting it as a customer
// claim. Do not add a type here that customers legitimately send.
func IsBackendMinted(t EventType) bool {
	switch t {
	case EventTypeDLPScanResult:
		return true
	default:
		return false
	}
}
