package attest

// Tests for the checkpoint chain.
//
// The four that carry the security property:
//
//   TestVerifyChain_MissingCheckpointIsDetected      — the whole point
//   TestVerifyChain_RecomputesRatherThanTrusts       — a stored hash
//                                                      nobody recomputes
//                                                      is decoration
//   TestBuildCheckpoint_EmptyIntervalUsesEmptyRoot   — an empty interval
//                                                      must not look
//                                                      like a missing one
//   TestVerifyChain_TenantDroppedFromTreeIsDetected  — per-tenant
//                                                      sub-chain
//
// The golden hashes below were computed by an INDEPENDENT Python
// implementation of the same preimage rules, not by running this code
// and recording its output. A golden value taken from the
// implementation it is meant to check proves only that the code is
// consistent with itself.

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"
)

const testInterval = time.Hour

func hour(n int) time.Time {
	return time.Date(2026, 9, 3, 12+n, 0, 0, 0, time.UTC)
}

func leaf(project string, execCount int, cumulative uint64, prev string) TenantLeaf {
	root := ""
	if execCount > 0 {
		root = strings.Repeat("a", 64)
	}
	return TenantLeaf{
		ProjectID:       project,
		IntervalRoot:    root,
		ExecutionCount:  execCount,
		CumulativeCount: cumulative,
		PrevLeafHash:    prev,
	}
}

// buildChain returns n consecutive checkpoints, each covering one hour
// and each carrying one leaf for "proj-alpha".
func buildChain(t *testing.T, n int) []Checkpoint {
	t.Helper()
	var (
		out      []Checkpoint
		prev     *Checkpoint
		prevLeaf = ZeroHash
		cum      uint64
	)
	for i := 0; i < n; i++ {
		cum += 2
		l := leaf("proj-alpha", 2, cum, prevLeaf)
		entryID := ""
		if prev != nil {
			entryID = "rekor-" + strings.Repeat("0", 4) + string(rune('a'+i))
		}
		c, err := BuildCheckpoint(CheckpointParams{
			Prev:           prev,
			PrevLogEntryID: entryID,
			IntervalStart:  hour(i),
			IntervalEnd:    hour(i + 1),
			Interval:       testInterval,
			Leaves:         []TenantLeaf{l},
			Now:            hour(i + 1).Add(5 * time.Second),
		})
		if err != nil {
			t.Fatalf("BuildCheckpoint %d: %v", i+1, err)
		}
		out = append(out, c)
		prevLeaf = TenantLeafHash(l)
		prev = &out[len(out)-1]
	}
	return out
}

// ── golden values, from an independent implementation ────────────────

func TestCanonicalField_EncodingIsUnambiguous(t *testing.T) {
	if got := string(appendCanonicalField(nil, "a", "bc")); got != "a:2:bc\n" {
		t.Errorf("appendCanonicalField = %q, want %q", got, "a:2:bc\n")
	}
	// The property length-prefixing exists for. Without the length,
	// these two would concatenate to the same bytes.
	x := string(appendCanonicalField(nil, "ab", "c"))
	y := string(appendCanonicalField(nil, "a", "bc"))
	if x == y {
		t.Errorf("(\"ab\",\"c\") and (\"a\",\"bc\") encode identically to %q — "+
			"the length prefix is not doing its job", x)
	}
}

func TestTenantLeafHash_MatchesIndependentImplementation(t *testing.T) {
	l := TenantLeaf{
		ProjectID:       "proj-alpha",
		IntervalRoot:    strings.Repeat("a", 64),
		ExecutionCount:  3,
		CumulativeCount: 3,
		PrevLeafHash:    ZeroHash,
	}
	const want = "7de6e244efe10184c5bde3eaccfafd72d83aad0c405696ee0f47d1b4819ab1ac"
	if got := TenantLeafHash(l); got != want {
		t.Errorf("TenantLeafHash drifted from the pinned construction.\n got:  %s\n want: %s\n"+
			"If this change was deliberate, CheckpointFormatV1 must be bumped: a "+
			"verifier applying v1 rules would otherwise compute a different hash "+
			"and report tampering that never happened.", got, want)
	}
}

func TestCheckpointHash_MatchesIndependentImplementation(t *testing.T) {
	l := TenantLeaf{
		ProjectID:       "proj-alpha",
		IntervalRoot:    strings.Repeat("a", 64),
		ExecutionCount:  3,
		CumulativeCount: 3,
		PrevLeafHash:    ZeroHash,
	}
	c, err := BuildCheckpoint(CheckpointParams{
		IntervalStart: hour(0),
		IntervalEnd:   hour(1),
		Interval:      testInterval,
		Leaves:        []TenantLeaf{l},
		Now:           hour(1).Add(5 * time.Second),
	})
	if err != nil {
		t.Fatalf("BuildCheckpoint: %v", err)
	}
	const wantRoot = "7de6e244efe10184c5bde3eaccfafd72d83aad0c405696ee0f47d1b4819ab1ac"
	const wantHash = "b4f78e06bf18917b7453e094e791d9bcb5f892bcbf3332f0cd5b732c8840bf5d"
	if c.MerkleRoot != wantRoot {
		t.Errorf("single-leaf root = %s, want %s", c.MerkleRoot, wantRoot)
	}
	if c.Hash != wantHash {
		t.Errorf("CheckpointHash drifted.\n got:  %s\n want: %s", c.Hash, wantHash)
	}
}

// ── RootOverExecutionDigests ─────────────────────────────────────────

// Golden values from an independent Python implementation of RFC 6962
// node hashing, not from running this code and recording its output.
func TestRootOverExecutionDigests_MatchesIndependentImplementation(t *testing.T) {
	a := strings.Repeat("aa", 32)
	b := strings.Repeat("bb", 32)
	c := strings.Repeat("cc", 32)

	got, err := RootOverExecutionDigests([]string{a, b})
	if err != nil {
		t.Fatalf("two leaves: %v", err)
	}
	const wantAB = "2f65cc0c7abfdb0c535cb7f942d65ae1fb04c9a3ad3ea5a62057aa8ac934a93a"
	if got != wantAB {
		t.Errorf("root over [a,b] = %s, want %s", got, wantAB)
	}

	got3, err := RootOverExecutionDigests([]string{a, b, c})
	if err != nil {
		t.Fatalf("three leaves: %v", err)
	}
	const wantABC = "9633b0ce0937fab8c998ffa595193755199f36aa16faab36fc024c80a50531e7"
	if got3 != wantABC {
		t.Errorf("root over [a,b,c] = %s, want %s (odd-leaf promotion)", got3, wantABC)
	}

	// One execution: the root IS that execution's digest. Pinned because
	// it is the case where a tree does no work, and a future change that
	// wrapped it in an extra hash would break every single-execution
	// interval already anchored.
	got1, err := RootOverExecutionDigests([]string{a})
	if err != nil {
		t.Fatalf("one leaf: %v", err)
	}
	if got1 != a {
		t.Errorf("root over a single digest = %s, want the digest itself %s", got1, a)
	}
}

// ORDER IS PART OF THE RESULT. The function must not sort: the caller
// supplies executions in the store's order, and that order is what the
// anchored root commits to.
func TestRootOverExecutionDigests_OrderChangesTheRoot(t *testing.T) {
	a := strings.Repeat("aa", 32)
	b := strings.Repeat("bb", 32)

	ab, err := RootOverExecutionDigests([]string{a, b})
	if err != nil {
		t.Fatal(err)
	}
	ba, err := RootOverExecutionDigests([]string{b, a})
	if err != nil {
		t.Fatal(err)
	}
	if ab == ba {
		t.Error("reordering the executions did not change the root; either the " +
			"function is sorting, which would disagree with the store's order, or " +
			"order is not being committed to at all")
	}
}

// An empty input must REFUSE, not return the empty-tree root. That
// value is a real 64-hex digest and would be indistinguishable from a
// genuine root over data that went missing — the exact confusion the
// empty-interval sentinel exists to avoid.
func TestRootOverExecutionDigests_RefusesEmptyRatherThanReturningTheEmptyTreeRoot(t *testing.T) {
	got, err := RootOverExecutionDigests(nil)
	if err == nil {
		t.Fatalf("empty input returned %q instead of refusing", got)
	}
	if got == emptyTreeRootHex {
		t.Errorf("returned the empty-TREE root, which would look like a real root " +
			"over vanished executions")
	}
	if got != "" {
		t.Errorf("a refusal must not also return a value, got %q", got)
	}
}

const emptyTreeRootHex = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func TestRootOverExecutionDigests_RejectsMalformedDigests(t *testing.T) {
	good := strings.Repeat("aa", 32)
	cases := []struct {
		name  string
		input []string
	}{
		{"not hex", []string{strings.Repeat("zz", 32)}},
		{"too short", []string{strings.Repeat("aa", 31)}},
		{"too long", []string{strings.Repeat("aa", 33)}},
		{"one bad among good", []string{good, "nope", good}},
		{"empty string element", []string{good, ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got, err := RootOverExecutionDigests(tc.input); err == nil {
				t.Errorf("accepted a malformed digest and produced %s", got)
			}
		})
	}
}

// ── the security property ────────────────────────────────────────────

func TestVerifyChain_HappyPath(t *testing.T) {
	if err := VerifyChain(buildChain(t, 4), testInterval); err != nil {
		t.Fatalf("an intact 4-checkpoint chain failed to verify: %v", err)
	}
}

// THE test. Suppressing one interval must be detectable.
func TestVerifyChain_MissingCheckpointIsDetected(t *testing.T) {
	full := buildChain(t, 4)

	// Drop checkpoint 3, exactly as an operator hiding an interval would.
	gapped := []Checkpoint{full[0], full[1], full[3]}

	err := VerifyChain(gapped, testInterval)
	if err == nil {
		t.Fatal("a chain with checkpoint 3 removed VERIFIED — omission is undetectable, " +
			"which is the one thing this mechanism exists to prevent")
	}
	if !errors.Is(err, ErrChainSequence) {
		t.Errorf("want ErrChainSequence, got %v", err)
	}
	// The error must name where, or an auditor cannot act on it.
	if !strings.Contains(err.Error(), "4 follows 2") {
		t.Errorf("error does not identify the gap: %v", err)
	}
}

// A stored hash that nobody recomputes is decoration.
func TestVerifyChain_RecomputesRatherThanTrusts(t *testing.T) {
	chain := buildChain(t, 3)

	// Rewrite the contents and leave the stored Hash alone — what
	// someone editing the database would do.
	chain[1].CumulativeCount = 9999

	err := VerifyChain(chain, testInterval)
	if err == nil {
		t.Fatal("edited contents with a stale Hash verified — VerifyChain is trusting " +
			"the stored hash instead of recomputing it")
	}
	if !errors.Is(err, ErrChainHashMismatch) {
		t.Errorf("want ErrChainHashMismatch, got %v", err)
	}
}

// Repairing the chain on paper: recompute the tampered checkpoint's own
// hash so it is internally consistent. The LINK to its predecessor must
// still fail.
func TestVerifyChain_ReHashedTamperStillBreaksTheLink(t *testing.T) {
	chain := buildChain(t, 3)
	chain[1].CumulativeCount = 9999
	chain[1].Hash = CheckpointHash(chain[1]) // internally consistent now

	err := VerifyChain(chain, testInterval)
	if err == nil {
		t.Fatal("a re-hashed tampered checkpoint verified; rewriting history is free")
	}
	// checkpoint 3 named checkpoint 2's ORIGINAL hash.
	if !errors.Is(err, ErrChainPrevHash) {
		t.Errorf("want ErrChainPrevHash, got %v", err)
	}
}

// An interval with no activity must be distinguishable from one whose
// contents went missing.
func TestBuildCheckpoint_EmptyIntervalUsesEmptyRoot(t *testing.T) {
	c, err := BuildCheckpoint(CheckpointParams{
		IntervalStart: hour(0),
		IntervalEnd:   hour(1),
		Interval:      testInterval,
		Leaves:        nil,
		Now:           hour(1),
	})
	if err != nil {
		t.Fatalf("an empty interval must still produce a checkpoint: %v", err)
	}
	if c.MerkleRoot != "" {
		t.Errorf("empty interval root = %q, want empty string", c.MerkleRoot)
	}
	if c.TenantLeafCount != 0 || c.CumulativeCount != 0 {
		t.Errorf("empty interval has leaves=%d count=%d", c.TenantLeafCount, c.CumulativeCount)
	}

	// The trap digest.go warns about: rootFromLeafHashes over nothing
	// returns SHA-256 of the empty input, a real-looking 64-hex value.
	emptyTree := hex.EncodeToString(rootFromLeafHashes(nil))
	sum := sha256.Sum256(nil)
	if emptyTree != hex.EncodeToString(sum[:]) {
		t.Fatalf("assumption about rootFromLeafHashes(nil) is wrong: %s", emptyTree)
	}
	if c.MerkleRoot == emptyTree {
		t.Errorf("empty interval used the empty-TREE root %s; that value would "+
			"silently stand in for a missing record", emptyTree)
	}

	if err := VerifyChain([]Checkpoint{c}, testInterval); err != nil {
		t.Errorf("a legitimately empty checkpoint failed to verify: %v", err)
	}
}

// Forging emptiness: claim zero leaves while carrying a root.
func TestVerifyChain_EmptyRootInvariantIsEnforced(t *testing.T) {
	cases := []struct {
		name string
		mut  func(*Checkpoint)
	}{
		{"leaves but no root", func(c *Checkpoint) { c.MerkleRoot = "" }},
		{"no leaves but a root", func(c *Checkpoint) {
			c.TenantLeafCount = 0
			c.MerkleRoot = hex.EncodeToString(rootFromLeafHashes(nil))
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := buildChain(t, 1)[0]
			tc.mut(&c)
			c.Hash = CheckpointHash(c) // make it internally consistent
			err := VerifyChain([]Checkpoint{c}, testInterval)
			if !errors.Is(err, ErrChainEmptyRootMismatch) {
				t.Errorf("want ErrChainEmptyRootMismatch, got %v", err)
			}
		})
	}
}

// Per-tenant sub-chain: a project silently omitted from one interval's
// tree leaves its next leaf pointing at a predecessor that is in no
// anchored tree.
func TestVerifyChain_TenantDroppedFromTreeIsDetected(t *testing.T) {
	// Interval 1: two projects.
	a1 := leaf("proj-alpha", 2, 2, ZeroHash)
	b1 := leaf("proj-beta", 3, 3, ZeroHash)
	cp1, err := BuildCheckpoint(CheckpointParams{
		IntervalStart: hour(0), IntervalEnd: hour(1), Interval: testInterval,
		Leaves: []TenantLeaf{a1, b1}, Now: hour(1),
	})
	if err != nil {
		t.Fatalf("cp1: %v", err)
	}

	// Interval 2: beta is DROPPED. Alpha continues normally.
	a2 := leaf("proj-alpha", 1, 3, TenantLeafHash(a1))
	cp2, err := BuildCheckpoint(CheckpointParams{
		Prev: &cp1, PrevLogEntryID: "rekor-1",
		IntervalStart: hour(1), IntervalEnd: hour(2), Interval: testInterval,
		Leaves: []TenantLeaf{a2}, Now: hour(2),
	})
	if err != nil {
		t.Fatalf("cp2: %v", err)
	}

	// The global chain still verifies — this is the honest limitation,
	// and it is why the per-tenant PrevLeafHash exists.
	if err := VerifyChain([]Checkpoint{cp1, cp2}, testInterval); err != nil {
		t.Fatalf("global chain should still verify: %v", err)
	}

	// Prove the leaves we hold are the ones each checkpoint anchored.
	// Without this step the leaves are just numbers somebody handed us.
	if err := VerifyIntervalLeaves(cp1, []TenantLeaf{a1, b1}); err != nil {
		t.Fatalf("cp1 leaves: %v", err)
	}
	if err := VerifyIntervalLeaves(cp2, []TenantLeaf{a2}); err != nil {
		t.Fatalf("cp2 leaves: %v", err)
	}

	// Beta really ran 5 executions in interval 2 and 1 in interval 3, so
	// its honest running total is 3 -> 8 -> 9. With the interval-2 leaf
	// removed after the fact, an auditor walking beta's sub-chain holds
	// only leaf 1 (total 3) and leaf 3 (total 9, +1 this interval).
	b3 := TenantLeaf{
		ProjectID: "proj-beta", IntervalRoot: strings.Repeat("c", 64),
		ExecutionCount: 1, CumulativeCount: 9, PrevLeafHash: TenantLeafHash(b1),
	}

	err = VerifyTenantSubChain("proj-beta", []TenantLeaf{b1, b3})
	if err == nil {
		t.Fatal("beta's sub-chain verified with an interval removed; the counts " +
			"reconciled when they should not have")
	}
	if !errors.Is(err, ErrTenantSubChain) {
		t.Errorf("want ErrTenantSubChain, got %v", err)
	}
	// 3 + 1 = 4, not 9. The arithmetic is what catches it.
	if !strings.Contains(err.Error(), "it should be 4") {
		t.Errorf("error does not show the arithmetic: %v", err)
	}

	// The honest limitation, asserted so nobody mistakes the scope: if
	// beta genuinely had NO activity in interval 2, having no leaf there
	// is correct and the sub-chain reconciles.
	quiet := TenantLeaf{
		ProjectID: "proj-beta", IntervalRoot: strings.Repeat("c", 64),
		ExecutionCount: 1, CumulativeCount: 4, PrevLeafHash: TenantLeafHash(b1),
	}
	if err := VerifyTenantSubChain("proj-beta", []TenantLeaf{b1, quiet}); err != nil {
		t.Errorf("a genuinely quiet interval must not look like tampering: %v", err)
	}
}

func TestVerifyTenantSubChain_Refusals(t *testing.T) {
	a1 := leaf("proj-alpha", 2, 2, ZeroHash)
	a2 := leaf("proj-alpha", 1, 3, TenantLeafHash(a1))

	if err := VerifyTenantSubChain("proj-alpha", []TenantLeaf{a1, a2}); err != nil {
		t.Fatalf("an intact sub-chain must verify: %v", err)
	}
	if err := VerifyTenantSubChain("proj-alpha", nil); !errors.Is(err, ErrTenantSubChain) {
		t.Errorf("empty: want ErrTenantSubChain, got %v", err)
	}
	// A leaf from another project must not be accepted into this chain.
	foreign := leaf("proj-beta", 1, 3, TenantLeafHash(a1))
	if err := VerifyTenantSubChain("proj-alpha", []TenantLeaf{a1, foreign}); !errors.Is(err, ErrTenantSubChain) {
		t.Errorf("foreign leaf: want ErrTenantSubChain, got %v", err)
	}
	// Broken link.
	relinked := a2
	relinked.PrevLeafHash = strings.Repeat("d", 64)
	if err := VerifyTenantSubChain("proj-alpha", []TenantLeaf{a1, relinked}); !errors.Is(err, ErrTenantSubChain) {
		t.Errorf("broken link: want ErrTenantSubChain, got %v", err)
	}
	// Negative count, guarded before the uint64 conversion that would
	// otherwise turn -1 into 18 quintillion and make the sum "work".
	neg := a2
	neg.ExecutionCount = -1
	neg.PrevLeafHash = TenantLeafHash(a1)
	if err := VerifyTenantSubChain("proj-alpha", []TenantLeaf{a1, neg}); !errors.Is(err, ErrTenantSubChain) {
		t.Errorf("negative count: want ErrTenantSubChain, got %v", err)
	}
}

func TestVerifyIntervalLeaves_DetectsSubstitution(t *testing.T) {
	a1 := leaf("proj-alpha", 2, 2, ZeroHash)
	b1 := leaf("proj-beta", 3, 3, ZeroHash)
	cp, err := BuildCheckpoint(CheckpointParams{
		IntervalStart: hour(0), IntervalEnd: hour(1), Interval: testInterval,
		Leaves: []TenantLeaf{a1, b1}, Now: hour(1),
	})
	if err != nil {
		t.Fatalf("BuildCheckpoint: %v", err)
	}

	if err := VerifyIntervalLeaves(cp, []TenantLeaf{a1, b1}); err != nil {
		t.Fatalf("the anchored leaves must verify: %v", err)
	}
	// Wrong count.
	if err := VerifyIntervalLeaves(cp, []TenantLeaf{a1}); !errors.Is(err, ErrIntervalLeaves) {
		t.Errorf("short list: want ErrIntervalLeaves, got %v", err)
	}
	// Right count, altered contents.
	tampered := b1
	tampered.CumulativeCount = 99
	if err := VerifyIntervalLeaves(cp, []TenantLeaf{a1, tampered}); !errors.Is(err, ErrIntervalLeaves) {
		t.Errorf("substituted leaf: want ErrIntervalLeaves, got %v", err)
	}
	// Order matters: the root depends on it.
	if err := VerifyIntervalLeaves(cp, []TenantLeaf{b1, a1}); !errors.Is(err, ErrIntervalLeaves) {
		t.Errorf("reordered leaves: want ErrIntervalLeaves, got %v", err)
	}
}

func TestVerifyChain_CountRegressionIsDetected(t *testing.T) {
	chain := buildChain(t, 3)
	chain[2].CumulativeCount = 1
	chain[2].Hash = CheckpointHash(chain[2])
	if err := VerifyChain(chain, testInterval); !errors.Is(err, ErrChainCountRegressed) {
		t.Errorf("want ErrChainCountRegressed, got %v", err)
	}
}

// PrevLogEntryID must be inside the preimage, or a broken chain could
// be repaired on paper by repointing the link.
func TestCheckpointHash_CoversPrevLogEntryID(t *testing.T) {
	chain := buildChain(t, 2)
	before := chain[1].Hash
	chain[1].PrevLogEntryID = "rekor-somewhere-else"
	if CheckpointHash(chain[1]) == before {
		t.Error("rewriting prev_log_entry_id did not change the hash; the link to " +
			"the public log is editable after anchoring")
	}
}

// ── build-time refusals ──────────────────────────────────────────────

func TestBuildCheckpoint_RefusesCorruptedPredecessor(t *testing.T) {
	chain := buildChain(t, 1)
	bad := chain[0]
	bad.CumulativeCount = 500 // stored Hash no longer matches

	_, err := BuildCheckpoint(CheckpointParams{
		Prev: &bad, PrevLogEntryID: "rekor-1",
		IntervalStart: hour(1), IntervalEnd: hour(2), Interval: testInterval,
		Leaves: []TenantLeaf{leaf("proj-alpha", 1, 3, ZeroHash)}, Now: hour(2),
	})
	if !errors.Is(err, ErrChainHashMismatch) {
		t.Errorf("extending a corrupted predecessor should refuse, got %v", err)
	}
}

func TestBuildCheckpoint_Refusals(t *testing.T) {
	okLeaf := leaf("proj-alpha", 1, 1, ZeroHash)
	base := func() CheckpointParams {
		return CheckpointParams{
			IntervalStart: hour(0), IntervalEnd: hour(1), Interval: testInterval,
			Leaves: []TenantLeaf{okLeaf}, Now: hour(1),
		}
	}
	cases := []struct {
		name string
		mut  func(*CheckpointParams)
	}{
		{"no interval", func(p *CheckpointParams) { p.Interval = 0 }},
		{"end before start", func(p *CheckpointParams) { p.IntervalEnd = hour(-1) }},
		{"span not equal to interval", func(p *CheckpointParams) { p.IntervalEnd = hour(2) }},
		{"start not on the grid", func(p *CheckpointParams) {
			p.IntervalStart = hour(0).Add(37 * time.Minute)
			p.IntervalEnd = hour(1).Add(37 * time.Minute)
		}},
		{"genesis with a log entry", func(p *CheckpointParams) { p.PrevLogEntryID = "rekor-1" }},
		{"leaf with no project", func(p *CheckpointParams) {
			l := okLeaf
			l.ProjectID = ""
			p.Leaves = []TenantLeaf{l}
		}},
		{"leaf with empty prev hash", func(p *CheckpointParams) {
			l := okLeaf
			l.PrevLeafHash = ""
			p.Leaves = []TenantLeaf{l}
		}},
		{"leaf with executions but no root", func(p *CheckpointParams) {
			l := okLeaf
			l.IntervalRoot = ""
			p.Leaves = []TenantLeaf{l}
		}},
		{"leaf with a root but no executions", func(p *CheckpointParams) {
			l := okLeaf
			l.ExecutionCount = 0
			p.Leaves = []TenantLeaf{l}
		}},
		{"negative execution count", func(p *CheckpointParams) {
			l := okLeaf
			l.ExecutionCount = -1
			p.Leaves = []TenantLeaf{l}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := base()
			tc.mut(&p)
			if _, err := BuildCheckpoint(p); err == nil {
				t.Error("expected a refusal, got none")
			}
		})
	}
}

func TestBuildCheckpoint_NonGenesisNeedsALogEntry(t *testing.T) {
	chain := buildChain(t, 1)
	_, err := BuildCheckpoint(CheckpointParams{
		Prev: &chain[0], PrevLogEntryID: "",
		IntervalStart: hour(1), IntervalEnd: hour(2), Interval: testInterval,
		Leaves: []TenantLeaf{leaf("proj-alpha", 1, 3, ZeroHash)}, Now: hour(2),
	})
	if err == nil {
		t.Fatal("a non-genesis checkpoint with no predecessor log entry was accepted; " +
			"the chain would only be verifiable by asking Mesedi")
	}
	if !strings.Contains(err.Error(), "log entry id") {
		t.Errorf("unhelpful error: %v", err)
	}
}

// ── verifier input validation ────────────────────────────────────────

func TestVerifyChain_InputValidation(t *testing.T) {
	good := buildChain(t, 1)[0]

	if err := VerifyChain(nil, testInterval); !errors.Is(err, ErrChainEmpty) {
		t.Errorf("empty slice: want ErrChainEmpty, got %v", err)
	}
	if err := VerifyChain([]Checkpoint{good}, 0); err == nil {
		t.Error("non-positive interval should be refused")
	}

	badFormat := good
	badFormat.Format = "mesedi.checkpoint.v2"
	badFormat.Hash = CheckpointHash(badFormat)
	if err := VerifyChain([]Checkpoint{badFormat}, testInterval); !errors.Is(err, ErrChainFormat) {
		t.Errorf("want ErrChainFormat, got %v", err)
	}

	zeroSeq := good
	zeroSeq.Seq = 0
	zeroSeq.Hash = CheckpointHash(zeroSeq)
	if err := VerifyChain([]Checkpoint{zeroSeq}, testInterval); !errors.Is(err, ErrChainSequence) {
		t.Errorf("seq 0: want ErrChainSequence, got %v", err)
	}

	// Genesis must not name a predecessor.
	fakeGenesis := good
	fakeGenesis.PrevCheckpointHash = strings.Repeat("b", 64)
	fakeGenesis.Hash = CheckpointHash(fakeGenesis)
	if err := VerifyChain([]Checkpoint{fakeGenesis}, testInterval); !errors.Is(err, ErrChainGenesis) {
		t.Errorf("want ErrChainGenesis, got %v", err)
	}

	// A window starting mid-chain is legitimate and must NOT be treated
	// as malformed genesis.
	window := buildChain(t, 3)[1:]
	if err := VerifyChain(window, testInterval); err != nil {
		t.Errorf("verifying a mid-chain window should succeed: %v", err)
	}
}

// Alignment is checked by the verifier too, not only the builder — the
// verifier is what an auditor runs and must not assume the builder was
// careful.
func TestVerifyChain_MisalignedIntervalIsDetected(t *testing.T) {
	c := buildChain(t, 1)[0]
	c.IntervalStart = c.IntervalStart.Add(37 * time.Minute)
	c.IntervalEnd = c.IntervalEnd.Add(37 * time.Minute)
	c.Hash = CheckpointHash(c)
	if err := VerifyChain([]Checkpoint{c}, testInterval); !errors.Is(err, ErrChainInterval) {
		t.Errorf("want ErrChainInterval for a :37-past chain, got %v", err)
	}
}

// Times denoting the same instant in different locations must be
// treated as equal. This is why alignedTo uses Equal rather than ==.
func TestAlignedTo_IgnoresLocation(t *testing.T) {
	utc := time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC)
	if !alignedTo(utc, time.Hour) {
		t.Fatal("12:00:00Z is not aligned to the hour")
	}
	// Same instant, different location.
	east := utc.In(time.FixedZone("UTC+2", 2*3600))
	if !alignedTo(east, time.Hour) {
		t.Error("the same instant expressed in UTC+2 was reported misaligned; " +
			"alignedTo is comparing struct fields rather than the instant")
	}
	if alignedTo(utc.Add(time.Minute), time.Hour) {
		t.Error("12:01:00Z reported as aligned to the hour")
	}
}
