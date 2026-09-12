package attest

import (
	"strings"
	"testing"
	"time"
)

// These tests exist because of a production failure on 2026-09-04.
//
// The first checkpoint this chain ever wrote was built, stored in Postgres,
// anchored to Rekor, and then failed its own read-back verification seconds
// later with "the row has been altered since it was written". Nothing had
// altered it. canonicalTime hashed nanoseconds; TIMESTAMPTZ keeps
// microseconds; the database silently dropped three digits and the
// recomputed hash no longer matched.
//
// The existing tests all passed, because every one of them either used a
// zero-nanosecond timestamp or never round-tripped through storage. The
// hash was correct, the storage was correct, and the pair was broken. That
// is the gap these close: not "is the hash right" but "does the hash
// survive the trip".

// pgRoundTrip models what Postgres TIMESTAMPTZ does to a timestamp: keeps
// microseconds, discards the rest. Deliberately a local model rather than a
// live database, so the invariant is asserted in a fast unit test that runs
// on every commit rather than only where a Postgres instance exists.
func pgRoundTrip(t time.Time) time.Time { return t.Truncate(time.Microsecond) }

func TestCheckpointHashSurvivesPostgresRoundTrip(t *testing.T) {
	// 123456789ns: microseconds AND a non-zero nanosecond remainder, so a
	// truncation that is silently skipped shows up. A timestamp ending in
	// zeros would pass whether or not the fix is present, which is exactly
	// why the original tests missed this.
	built := time.Date(2026, 9, 4, 13, 46, 45, 123456789, time.UTC)

	c := Checkpoint{
		Format:              CheckpointFormatCurrent,
		Seq:                 1,
		PrevCheckpointHash:  ZeroHash,
		PrevLogEntryID:      "",
		IntervalStart:       time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		IntervalEnd:         time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC),
		TenantLeafCount:     0,
		MerkleRoot:          "",
		CumulativeCount:     0,
		CreatedAtUnattested: built,
	}

	atWrite := CheckpointHash(c)

	// What the row looks like coming back out of Postgres.
	c.CreatedAtUnattested = pgRoundTrip(c.CreatedAtUnattested)
	atRead := CheckpointHash(c)

	if atWrite != atRead {
		t.Fatalf("checkpoint hash does not survive storage:\n  written:  %s\n  read back: %s\n"+
			"A checkpoint that cannot be re-verified from its own stored columns reports "+
			"tampering that never happened.", atWrite, atRead)
	}
}

// Sub-microsecond differences must not change the hash at all, in either
// direction. If they did, two processes building the same logical
// checkpoint microseconds apart would disagree.
func TestCheckpointHashIgnoresSubMicrosecondNoise(t *testing.T) {
	base := time.Date(2026, 9, 4, 13, 46, 45, 123456000, time.UTC)

	mk := func(ns int) Checkpoint {
		return Checkpoint{
			Format:              CheckpointFormatCurrent,
			Seq:                 7,
			PrevCheckpointHash:  strings.Repeat("a", 64),
			PrevLogEntryID:      "2711966358",
			IntervalStart:       time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
			IntervalEnd:         time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC),
			TenantLeafCount:     3,
			MerkleRoot:          strings.Repeat("b", 64),
			CumulativeCount:     42,
			CreatedAtUnattested: base.Add(time.Duration(ns)),
		}
	}

	want := CheckpointHash(mk(0))
	for _, ns := range []int{1, 17, 500, 999} {
		if got := CheckpointHash(mk(ns)); got != want {
			t.Errorf("+%dns changed the hash; sub-microsecond precision is not storable "+
				"and must not be committed to", ns)
		}
	}

	// The guard against over-truncating: a WHOLE microsecond is a real,
	// storable difference and must still change the hash. Without this,
	// a careless fix that truncated to the second would pass every other
	// test here while quietly destroying resolution.
	if got := CheckpointHash(mk(1000)); got == want {
		t.Error("a full microsecond difference must change the hash; " +
			"truncation has gone coarser than storage precision")
	}
}

// canonicalTime is the single choke point, so assert the property there
// directly rather than only through CheckpointHash. A future field that
// hashes a timestamp gets this for free, and this test says so.
func TestCanonicalTimeTruncatesToStoragePrecision(t *testing.T) {
	ts := time.Date(2026, 9, 4, 13, 46, 45, 123456789, time.UTC)

	got := canonicalTime(ts)
	const want = "2026-09-04T13:46:45.123456000Z"
	if got != want {
		t.Errorf("canonicalTime(%v) = %q, want %q", ts, got, want)
	}
	if got != canonicalTime(pgRoundTrip(ts)) {
		t.Error("canonicalTime disagrees before and after a storage round trip")
	}

	// Fixed width matters: the encoding is length-prefixed and compared
	// bytewise against CanonicalLeaf. Dropping the always-zero trailing
	// digits would be a silent preimage change.
	if len(got) != len(want) {
		t.Errorf("encoding width changed: %d vs %d", len(got), len(want))
	}
}

// The v1 format must be rejected with an explanation, not silently
// re-verified under v2 rules. Anyone holding the one v1 checkpoint ever
// written deserves to be told why it cannot check out, rather than being
// shown a hash mismatch that looks like tampering.
func TestVerifyChainRejectsV1WithAnExplanation(t *testing.T) {
	c := Checkpoint{
		Format:              CheckpointFormatV1,
		Seq:                 1,
		PrevCheckpointHash:  ZeroHash,
		IntervalStart:       time.Date(2026, 9, 4, 12, 0, 0, 0, time.UTC),
		IntervalEnd:         time.Date(2026, 9, 4, 13, 0, 0, 0, time.UTC),
		CreatedAtUnattested: time.Date(2026, 9, 4, 13, 46, 45, 0, time.UTC),
	}
	c.Hash = CheckpointHash(c)

	err := VerifyChain([]Checkpoint{c}, time.Hour)
	if err == nil {
		t.Fatal("VerifyChain accepted a v1 checkpoint")
	}
	if !strings.Contains(err.Error(), "cannot be verified") {
		t.Errorf("v1 rejection should explain why, got: %v", err)
	}
}
