package detectors

import (
	"reflect"
	"testing"
)

// The clean cases matter as much as the firing ones. A detector that
// cannot stay quiet is a detector customers disable, and the whole
// value of this one rests on it being trustworthy when it speaks.
func TestRecordIntegrityStaysQuietOnCleanRecords(t *testing.T) {
	cases := []struct {
		name string
		seqs []int
	}{
		{"dense from one", []int{1, 2, 3, 4, 5}},
		{"dense from zero", []int{0, 1, 2, 3}},
		{"dense from an arbitrary base", []int{900, 901, 902}},
		{"out of arrival order but complete", []int{3, 1, 4, 2}},
		{"two adjacent events", []int{7, 8}},
		{"single event", []int{1}},
		{"no events", nil},
		{"empty slice", []int{}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DetectRecordIntegrityAllMatches(tc.seqs); got != nil {
				t.Errorf("expected silence, got %v", got)
			}
		})
	}
}

func TestRecordIntegrityDetectsGap(t *testing.T) {
	cases := []struct {
		name string
		seqs []int
	}{
		{"one missing in the middle", []int{1, 2, 4, 5}},
		{"three missing in the middle", []int{1, 2, 6, 7}},
		{"gap despite out-of-order arrival", []int{5, 1, 2}},
		{"gap at a non-one base", []int{100, 101, 104}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := DetectRecordIntegrityAllMatches(tc.seqs)
			if !contains(got, RecordIntegritySequenceGap) {
				t.Errorf("expected %s, got %v", RecordIntegritySequenceGap, got)
			}
		})
	}
}

func TestRecordIntegrityDetectsDuplicate(t *testing.T) {
	got := DetectRecordIntegrityAllMatches([]int{1, 2, 2, 3})
	if !contains(got, RecordIntegrityDuplicateSequence) {
		t.Errorf("expected %s, got %v", RecordIntegrityDuplicateSequence, got)
	}
	if contains(got, RecordIntegritySequenceGap) {
		t.Errorf("1,2,2,3 spans 1..3 with 3 distinct values and is dense; "+
			"gap must not fire. got %v", got)
	}
}

// The two conditions are orthogonal and a record can carry both.
// Reporting only the first one found would hide half the problem —
// this is the same defect hitl_timeout.G3 fixed when its first-match
// -wins scan suppressed the second signature.
func TestRecordIntegrityReportsBothConditionsWhenBothPresent(t *testing.T) {
	// 1,2,2,5: distinct {1,2,5} = 3 values spanning 1..5 = 5 slots,
	// so 3 and 4 are missing AND 2 is doubled.
	got := DetectRecordIntegrityAllMatches([]int{1, 2, 2, 5})
	want := []string{
		RecordIntegritySequenceGap,
		RecordIntegrityDuplicateSequence,
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("expected both signatures in gap-then-duplicate order.\n got %v\nwant %v", got, want)
	}
}

// Ordering is part of the contract, not an accident of implementation.
// Anything downstream that renders or diffs these lists needs them
// stable across runs.
func TestRecordIntegrityOrderingIsDeterministic(t *testing.T) {
	in := []int{9, 2, 2, 5, 1}
	first := DetectRecordIntegrityAllMatches(in)
	for i := 0; i < 50; i++ {
		if got := DetectRecordIntegrityAllMatches(in); !reflect.DeepEqual(got, first) {
			t.Fatalf("run %d differed: got %v, first %v", i, got, first)
		}
	}
}

func TestMissingSequences(t *testing.T) {
	cases := []struct {
		name string
		seqs []int
		want []int
	}{
		{"dense record has nothing missing", []int{1, 2, 3}, nil},
		{"single hole", []int{1, 2, 4}, []int{3}},
		{"multiple holes", []int{1, 4, 6}, []int{2, 3, 5}},
		{"ascending regardless of input order", []int{6, 1, 4}, []int{2, 3, 5}},
		{"duplicates do not create phantom holes", []int{1, 1, 2, 3}, nil},
		{"too few events", []int{5}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := MissingSequences(tc.seqs); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

func TestDuplicatedSequences(t *testing.T) {
	cases := []struct {
		name string
		seqs []int
		want []int
	}{
		{"no duplicates", []int{1, 2, 3}, nil},
		{"one duplicate", []int{1, 2, 2, 3}, []int{2}},
		{"several duplicates returned ascending", []int{5, 1, 5, 1, 3}, []int{1, 5}},
		{"triplicate counts once", []int{4, 4, 4}, []int{4}},
		{"too few events", []int{9}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := DuplicatedSequences(tc.seqs); !reflect.DeepEqual(got, tc.want) {
				t.Errorf("got %v, want %v", got, tc.want)
			}
		})
	}
}

// Go randomises map iteration order. DuplicatedSequences sorts for
// exactly that reason, and this test exists so the sort cannot be
// removed as "unnecessary" by someone who runs the suite once and
// sees it pass.
func TestDuplicatedSequencesIsStableAcrossRuns(t *testing.T) {
	in := []int{7, 3, 7, 3, 11, 11, 2}
	want := []int{3, 7, 11}
	for i := 0; i < 100; i++ {
		if got := DuplicatedSequences(in); !reflect.DeepEqual(got, want) {
			t.Fatalf("run %d: got %v, want %v", i, got, want)
		}
	}
}

// A truncated head is invisible to this detector by design. Pinning
// the limitation in a test means it is a documented decision rather
// than an undiscovered bug, and anyone who later makes head-truncation
// detectable has to come here and delete this deliberately.
func TestRecordIntegrityCannotSeeATruncatedHead(t *testing.T) {
	// Events 1 and 2 never arrived. What survives is dense.
	if got := DetectRecordIntegrityAllMatches([]int{3, 4, 5}); got != nil {
		t.Errorf("documented limitation changed: expected silence on a "+
			"truncated head, got %v. If head truncation is now detectable, "+
			"update the package comment and remove this test.", got)
	}
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

// A two-event record can declare a two-billion-wide span, because
// Event.Sequence arrives in a client-supplied payload. Enumerating it
// would allocate gigabytes inside a request handler. This test is the
// guard on that bound; it was added because the post-task audit's E12
// check (long-lived in-memory state has a size cap) caught the
// unbounded version.
func TestMissingSequencesIsHardCapped(t *testing.T) {
	// Span of two billion from two events.
	got := MissingSequences([]int{1, 2000000000})
	if len(got) != MaxReportedMissing {
		t.Fatalf("expected exactly %d reported missing, got %d",
			MaxReportedMissing, len(got))
	}
	// Truncated, but still pointing at the right end of the problem.
	if got[0] != 2 {
		t.Errorf("truncated result must still start at the first missing "+
			"value; got[0] = %d, want 2", got[0])
	}
	for i := 1; i < len(got); i++ {
		if got[i] <= got[i-1] {
			t.Fatalf("result must stay ascending after truncation: "+
				"got[%d]=%d <= got[%d]=%d", i, got[i], i-1, got[i-1])
		}
	}
}

// Truncating the detail list must never suppress the signal itself.
// If a wide span stopped firing the gap signature, the cap would have
// traded away detection for memory, which is the wrong trade.
func TestWideSpanStillFiresTheGapSignature(t *testing.T) {
	got := DetectRecordIntegrityAllMatches([]int{1, 2000000000})
	if !contains(got, RecordIntegritySequenceGap) {
		t.Errorf("a two-billion-wide gap must still fire %s; got %v",
			RecordIntegritySequenceGap, got)
	}
}

// A gap smaller than the cap must be reported in full — the cap must
// not silently truncate ordinary results.
func TestMissingSequencesBelowCapIsComplete(t *testing.T) {
	got := MissingSequences([]int{1, 12})
	want := 10 // 2..11
	if len(got) != want {
		t.Errorf("expected all %d missing values below the cap, got %d: %v",
			want, len(got), got)
	}
}
