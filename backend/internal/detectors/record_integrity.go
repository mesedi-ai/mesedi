// Record-integrity detector.
//
// Every other detector in this package asks "did the agent do
// something wrong?". This one asks a different question: "is the
// record of what the agent did internally consistent?"
//
// That distinction is the whole point. Twenty detectors watch the
// agent. None watched the evidence. An execution can be flawless and
// still arrive with a hole in its event stream, and until now nothing
// would say so, the dashboard would render the surviving events and
// look clean, because a missing event and an event that never happened
// are indistinguishable to every other detector here.
//
// WHAT IT FIRES ON
//
//	record_integrity:sequence_gap
//	    The execution's events carry sequence numbers with at least
//	    one value missing between the lowest and highest seen. Events
//	    were produced that this record does not contain.
//
//	record_integrity:duplicate_sequence
//	    Two or more events claim the same sequence number. One
//	    position in the record has been written more than once, so at
//	    most one of them can be the original.
//
// # WHAT IT DOES NOT DO, AND WHY THAT MATTERS
//
// It does not prove tampering, and nothing in this file should be read
// as claiming otherwise. A sequence gap is far more often a dropped
// HTTP request, an SDK crash mid-flush, or a process killed before its
// buffer drained than it is anyone deleting anything. Overclaiming here
// would be worse than not shipping: an integrity control that cries
// tampering at packet loss teaches its operator to ignore it.
//
// But the load-bearing reason is sharper than "gaps are usually
// mundane", and it is structural:
//
//	THIS DETECTOR'S ONLY INPUT IS ATTACKER-CONTROLLED.
//
// Event.Sequence arrives inside the POST /events body. That endpoint
// is authenticated by bearer project key and NOTHING SIGNS AN INBOUND
// EVENT, every HMAC in this backend is outbound webhook signing.
// Anyone holding a project key can post whatever sequence numbers they
// like, so an actor wanting to conceal something does not produce a
// gap for this detector to find. They post a dense, well-ordered
// stream and this file reports a clean record.
//
// So the honest statement of scope is: this detects records that were
// damaged, not records that were authored. Loss, crash and retry it
// sees. Fabrication it cannot see, and no detector downstream of an
// unsigned ingest boundary can, such a detector would be asking the
// attacker's own data whether the attacker's data is trustworthy.
//
// What it establishes is therefore narrow and still worth having:
// THIS RECORD IS NOT COMPLETE, and here is the specific position that
// is missing or doubled. Closing the fabrication half is not a detector
// problem. It requires events to be signed at the moment they are
// written, by a key the emitting agent holds and the ingest path
// verifies, a different mechanism, and a different product surface.
//
// backend/test/integration/test_forged_event_stream.py demonstrates the
// gap against a running backend rather than asserting it in prose.
//
// # DELIBERATELY NOT IN THIS WAVE
//
// Two further signals were designed and cut: timestamp_regression
// (event N+1 timestamped before event N) and event_outside_window
// (an event timestamped outside its execution's own start/end).
// Both are real. Both are also clock-dependent, and any customer
// running agents across more than one machine has skew measured in
// milliseconds to seconds. Without a per-project skew tolerance they
// would fire constantly on healthy systems.
//
// The two signals that shipped are pure integer analysis on sequence
// numbers. No clock touches them. They cannot be perturbed by skew,
// NTP correction, timezone handling, or a customer's container drifting
// , which is exactly why they are the ones that ship first.
package detectors

import "sort"

// Signature strings emitted by this detector. Fixed vocabulary, no
// interpolated magnitudes: failure_groups cluster by signature, so
// embedding the gap size would shatter one recurring problem into a
// new group per gap width. Magnitude belongs on the execution detail,
// not in the grouping key. Same reasoning as hitl_timeout's
// "explicit" / "sla_exceeded".
const (
	RecordIntegritySequenceGap       = "record_integrity:sequence_gap"
	RecordIntegrityDuplicateSequence = "record_integrity:duplicate_sequence"
)

// MaxReportedMissing caps how many missing sequence numbers
// MissingSequences will enumerate.
//
// THIS IS A MEMORY BOUND, NOT A DISPLAY PREFERENCE. Event.Sequence
// arrives in a client-supplied payload. An execution carrying just two
// events numbered 1 and 2000000000 has a span of two billion, and
// enumerating it would allocate gigabytes inside a request handler ,
// from a two-event POST. A caller-side "truncate for display" contract
// does not help, because the allocation happens here, before any
// caller sees it.
//
// The gap SIGNATURE still fires correctly on such a record; only the
// human-readable detail list is truncated. Detection is never traded
// away for the bound.
const MaxReportedMissing = 100

// DetectRecordIntegrityAllMatches examines the sequence numbers of one
// execution's events and returns every integrity signature that fires.
//
// Input is the raw sequence values in whatever order they were read.
// ORDER IS IRRELEVANT AND THAT IS INTENTIONAL: events legitimately
// arrive out of order under concurrency, retry and batching, so
// arrival order carries no information about completeness. Only the
// SET of values does.
//
// Returns nil when the record is internally consistent. At most two
// signatures, ordered deterministically: gap before duplicate.
//
// Fewer than two events returns nil. A single event cannot be missing
// a neighbour we know about, and zero events is an execution that
// recorded nothing, a real condition, but not this detector's
// (an empty stream has no internal contradiction to find).
func DetectRecordIntegrityAllMatches(sequences []int) []string {
	if len(sequences) < 2 {
		return nil
	}

	seen := make(map[int]int, len(sequences))
	for _, s := range sequences {
		seen[s]++
	}

	var sigs []string

	// Gap: the distinct values do not densely fill the range they
	// span. Deliberately measured from the LOWEST OBSERVED value
	// rather than from zero or one, because this detector must not
	// assume where a customer's SDK starts numbering. Anchoring to
	// a constant would report a gap on every execution from any SDK
	// that happens to start at a different base, a false positive
	// caused entirely by our own assumption.
	//
	// The cost of that choice, stated plainly: a run whose FIRST
	// events are missing looks clean here, because the lowest
	// surviving value silently becomes the new floor. Detecting a
	// truncated head needs an independently known start marker,
	// which the event stream alone does not carry.
	lo, hi := sequences[0], sequences[0]
	for _, s := range sequences {
		if s < lo {
			lo = s
		}
		if s > hi {
			hi = s
		}
	}
	// hi-lo+1 is the count the range would hold if densely filled.
	// Computed in int; an execution cannot hold enough events for
	// this to overflow on any platform where int is 64-bit, and the
	// ingest path bounds sequence values well below that.
	if span := hi - lo + 1; span > len(seen) {
		sigs = append(sigs, RecordIntegritySequenceGap)
	}

	// Duplicate: any sequence number claimed more than once.
	// Checked independently of the gap test because the two are
	// orthogonal, 1,2,2,4 is both (4 is unreachable from 3 events)
	// and a customer needs to see both facts, not whichever the
	// code happened to test first.
	for _, n := range seen {
		if n > 1 {
			sigs = append(sigs, RecordIntegrityDuplicateSequence)
			break
		}
	}

	return sigs
}

// MissingSequences returns the specific sequence numbers absent from
// the record, ascending.
//
// Not used for grouping, the signature stays coarse on purpose. This
// is for the execution detail view and for the AI analysis prompt,
// where "events 4, 5 and 6 are missing" is the difference between an
// operator who can go look at the right window of their logs and one
// who has been told only that something is wrong.
//
// Returns nil when the record is dense. The result is HARD-CAPPED at
// MaxReportedMissing entries, see that constant for why the cap is a
// safety bound rather than a display choice. A truncated result is
// still ascending and still begins at the first missing value, so the
// operator is pointed at the right end of the problem.
//
// The `seen` map is bounded by len(sequences), which the ingest path
// already bounds per execution, so it needs no cap of its own. The
// enumeration loop is the part that could run away, and that is what
// the cap guards.
func MissingSequences(sequences []int) []int {
	if len(sequences) < 2 {
		return nil
	}
	seen := make(map[int]struct{}, len(sequences))
	lo, hi := sequences[0], sequences[0]
	for _, s := range sequences {
		seen[s] = struct{}{}
		if s < lo {
			lo = s
		}
		if s > hi {
			hi = s
		}
	}
	var missing []int
	for v := lo; v <= hi; v++ {
		if _, ok := seen[v]; !ok {
			missing = append(missing, v)
			if len(missing) == MaxReportedMissing {
				// Stop enumerating. Deliberately breaks out of the
				// loop rather than continuing to count, because the
				// only reason to keep walking a two-billion-wide span
				// is to produce a number nobody will read.
				break
			}
		}
	}
	return missing
}

// DuplicatedSequences returns the sequence numbers claimed more than
// once, ascending. Companion to MissingSequences with the same
// purpose: detail and analysis context, never grouping.
func DuplicatedSequences(sequences []int) []int {
	if len(sequences) < 2 {
		return nil
	}
	counts := make(map[int]int, len(sequences))
	for _, s := range sequences {
		counts[s]++
	}
	var dupes []int
	for v, n := range counts {
		if n > 1 {
			dupes = append(dupes, v)
		}
	}
	// Map iteration order is randomised in Go. Sorting is not
	// cosmetic here: without it the same input would produce a
	// different slice on every call, which would make any test
	// asserting on this flaky and any UI built on it unstable.
	sort.Ints(dupes)
	return dupes
}
