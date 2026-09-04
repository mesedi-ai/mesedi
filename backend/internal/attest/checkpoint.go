// Checkpoints: making omission detectable.
//
// # WHAT PROBLEM THIS SOLVES, AND WHICH ONE IT DOES NOT
//
// digest.go can summarise one execution and that root can be anchored
// in a public transparency log. That proves the execution's record has
// not changed since it was anchored.
//
// It does not stop Mesedi from choosing WHICH executions to anchor. A
// dishonest operator anchors the clean runs and skips the ugly ones.
// Every receipt that exists is cryptographically perfect; the missing
// ones leave no trace. An auditor holding fifty valid receipts cannot
// tell whether there were fifty-one executions. Per-execution
// anchoring cannot fix this, because the absence of a receipt is
// indistinguishable from the absence of an execution.
//
// A checkpoint chain fixes it the way a numbered hand receipt book
// does. Remove number 47 and the hole is the finding. Each checkpoint
// covers a fixed clock interval, names its predecessor's hash AND its
// predecessor's transparency-log entry, and carries a cumulative count.
// Skipping one leaves the next pointing at a log entry that does not
// exist. Silence becomes evidence.
//
// WHAT IT STILL DOES NOT COVER, stated here so nobody has to discover
// it in a demo: the chain bounds the SERVER side. It proves nothing was
// altered or dropped after Mesedi received it. It says nothing about
// whether the agent told the truth on the way in, and it cannot detect
// an event that was never chained in the first place — drop it at
// ingest and the chain is internally consistent and looks complete.
// That gap closes only with a signature from the agent's own key. See
// design-docs/SIGNED_INGEST_DESIGN.md.
package attest

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strconv"
	"time"
)

// CheckpointFormat identifies this construction. Any change to the
// preimage or the tree shape REQUIRES a new identifier: a verifier
// applying v1 rules to a v2 checkpoint would compute a different hash
// and report tampering that never happened.
//
// v1 -> v2 on 2026-09-04. canonicalTime now truncates to
// storagePrecision, which changes the preimage of every checkpoint
// carrying a sub-microsecond timestamp. See storagePrecision for why.
//
// v1 is deliberately NOT accepted by VerifyChain. It is not a matter of
// dropping support for something that worked: a v1 checkpoint written to
// Postgres could never be re-verified even when brand new, because the
// hash committed to nanoseconds the database did not keep. Exactly one v1
// checkpoint was ever produced, on 2026-09-04. It was an empty genesis
// checkpoint, it is unverifiable by construction, and it was abandoned
// rather than migrated. Its Rekor entry (2711966358) remains in the public
// log as an orphan, which is the honest outcome: the log is append-only,
// and pretending the entry was not made would be the one thing this
// system exists to prevent.
const (
	CheckpointFormatV1 = "mesedi.checkpoint.v1"
	CheckpointFormatV2 = "mesedi.checkpoint.v2"

	// CheckpointFormatCurrent is what BuildCheckpoint stamps and what
	// VerifyChain requires. Named separately from the versioned constants
	// so a future bump changes one line and every reference follows.
	CheckpointFormatCurrent = CheckpointFormatV2
)

// Domain-separation tags. Two different structures must never produce
// the same preimage, or a tenant leaf could be presented as a
// checkpoint.
//
// DO NOT "FIX" THE v1 IN THESE STRINGS TO MATCH CheckpointFormatV2.
//
// They look stale next to a v2 format constant and they are not. These
// tags separate STRUCTURES, not versions: their job is that a tenant leaf
// can never be mistaken for a checkpoint. Versioning is carried by the
// "format" field inside the checkpoint preimage, which is what actually
// changed in the v1 -> v2 bump.
//
// Editing either string silently changes every hash this package produces.
// Existing checkpoints would then fail read-back verification and the
// system would report tampering that never happened — which is precisely
// the incident that caused the v2 bump in the first place. The tenant-leaf
// tag is doubly load-bearing: TenantLeafHash commits to no timestamps, so
// leaf hashes are byte-identical across v1 and v2, and they should stay
// that way.
const (
	checkpointDomain = "mesedi.checkpoint.v1"
	tenantLeafDomain = "mesedi.checkpoint.tenant-leaf.v1"
)

// ZeroHash is the predecessor hash for a genesis record: 64 hex zeros.
// A fixed sentinel rather than an empty string, so "no predecessor" is
// a value that gets hashed like any other rather than a gap in the
// preimage.
const ZeroHash = "0000000000000000000000000000000000000000000000000000000000000000"

// Chain verification failures. Distinct errors on purpose: a verifier
// that can only say "false" is useless for diagnosis, and the whole
// point of this mechanism is telling an auditor WHICH property broke.
var (
	ErrChainEmpty             = errors.New("attest: no checkpoints to verify")
	ErrChainFormat            = errors.New("attest: unrecognised checkpoint format")
	ErrChainSequence          = errors.New("attest: checkpoint sequence is not consecutive")
	ErrChainPrevHash          = errors.New("attest: checkpoint does not name its predecessor's hash")
	ErrChainHashMismatch      = errors.New("attest: checkpoint hash does not match its contents")
	ErrChainInterval          = errors.New("attest: checkpoint intervals do not tile the period")
	ErrChainCountRegressed    = errors.New("attest: cumulative count decreased")
	ErrChainEmptyRootMismatch = errors.New(
		"attest: MerkleRoot must be empty exactly when TenantLeafCount is zero")
	ErrChainGenesis = errors.New("attest: genesis checkpoint is malformed")
)

// TenantLeaf is one project's contribution to an interval.
//
// One leaf per project rather than one chain per project. A chain each
// would mean one transparency-log submission per project per interval,
// so cost would scale with customer count on a path that earns nothing
// per call. As leaves under a single interval root, the cost is one
// submission per interval regardless of how many projects exist, and a
// project still gets an inclusion proof for its own leaf. What a
// project learns about others is exactly one number: how many leaves
// the interval had.
type TenantLeaf struct {
	// ProjectID is Mesedi's tenant scope.
	ProjectID string `json:"project_id"`

	// IntervalRoot is the RFC 6962 root over this project's execution
	// digests in this interval, hex. Empty when the project had no
	// executions but still needs a leaf to keep its sub-chain intact.
	IntervalRoot string `json:"interval_root"`

	// ExecutionCount is executions in THIS interval.
	ExecutionCount int `json:"execution_count"`

	// CumulativeCount is this project's total executions covered since
	// its first leaf. Monotonic; what makes truncation detectable.
	CumulativeCount uint64 `json:"cumulative_count"`

	// PrevLeafHash is this project's previous leaf hash, ZeroHash at
	// first appearance.
	//
	// This field is why a project cannot be silently dropped from one
	// interval's tree. Without it the global chain would still verify
	// with a project missing. With it, that project's next leaf points
	// at a predecessor hash that appears in no anchored tree.
	PrevLeafHash string `json:"prev_leaf_hash"`
}

// Checkpoint is a signed-elsewhere commitment to one clock interval.
//
// Anchoring is not this type's job: it carries no signature and no
// log entry of its own. It is anchored by submitting Hash, and the
// entry id that comes back becomes the NEXT checkpoint's
// PrevLogEntryID. That is what stitches the chain to a log Mesedi does
// not control, rather than only to itself.
type Checkpoint struct {
	Format string `json:"format"`

	// Seq is monotonic and starts at 1. Gaps are the primary signal.
	Seq uint64 `json:"seq"`

	// PrevCheckpointHash is checkpoint Seq-1's Hash, ZeroHash at genesis.
	PrevCheckpointHash string `json:"prev_checkpoint_hash"`

	// PrevLogEntryID is the transparency-log entry for checkpoint Seq-1,
	// empty at genesis. Inside the hash preimage, so it cannot be edited
	// after the fact to point somewhere convenient.
	PrevLogEntryID string `json:"prev_log_entry_id"`

	// The interval covered, both aligned to the interval boundary and
	// both UTC. Fixed clock boundaries, not "an hour since the last
	// one", because a drifting schedule cannot be checked for holes.
	IntervalStart time.Time `json:"interval_start"`
	IntervalEnd   time.Time `json:"interval_end"`

	// TenantLeafCount is leaves in this interval's tree.
	TenantLeafCount int `json:"tenant_leaf_count"`

	// MerkleRoot is the RFC 6962 root over the tenant leaf hashes, hex.
	//
	// EMPTY STRING for an interval with no leaves — NOT the empty-tree
	// root. rootFromLeafHashes returns SHA-256 of the empty input for an
	// empty slice, which is a real-looking 64-hex value, and digest.go's
	// header warns that such a value would silently stand in for "we
	// have no record". An interval that genuinely had no activity must
	// be distinguishable from one whose contents went missing.
	MerkleRoot string `json:"merkle_root"`

	// CumulativeCount is total executions covered since genesis across
	// all projects.
	CumulativeCount uint64 `json:"cumulative_count"`

	// CreatedAtUnattested is when Mesedi says it built this. Named to
	// admit what it is: a claim. The trustworthy timestamp is the
	// transparency log's own integration time, which Mesedi cannot set.
	CreatedAtUnattested time.Time `json:"created_at_unattested"`

	// Hash is the checkpoint's own hash, hex. Stored for convenience.
	// VerifyChain RECOMPUTES it rather than trusting it — a stored hash
	// nobody recomputes is decoration, not a check.
	Hash string `json:"hash"`
}

// appendCanonicalField writes label:length:value\n.
//
// DELIBERATELY MIRRORS the encoding inside CanonicalLeaf rather than
// sharing code with it. CanonicalLeaf's output is already anchored in a
// public log; refactoring it to extract a shared helper would risk
// changing bytes that existing proofs depend on, and the failure would
// look like tampering rather than like a refactor. The equivalence is
// pinned by a test instead.
//
// The length is what stops ("ab","c") and ("a","bc") producing
// identical bytes. Every preimage in this file is built with it, unlike
// verdifax-orchestrator's adapters.join, which is a bare "." delimiter.
func appendCanonicalField(buf []byte, label, value string) []byte {
	buf = append(buf, label...)
	buf = append(buf, byte(':'))
	buf = append(buf, strconv.Itoa(len(value))...)
	buf = append(buf, byte(':'))
	buf = append(buf, value...)
	buf = append(buf, byte('\n'))
	return buf
}

// storagePrecision is the coarsest precision any supported backend keeps,
// and therefore the only precision a hash may commit to.
//
// Postgres TIMESTAMPTZ stores microseconds. Go's time.Now() carries
// nanoseconds. SQLite keeps whatever string it is handed. So a hash taken
// over a nanosecond timestamp and then stored in Postgres can never be
// recomputed from what comes back: the database silently drops the last
// three digits, the recomputed hash differs, and the read path reports
// that the row was altered.
//
// That is not hypothetical. It happened on the first checkpoint this chain
// ever wrote, on 2026-09-04, in production. The checkpoint was built,
// stored, anchored to Rekor, and then failed its own read-back
// verification seconds later with "the row has been altered since it was
// written". Nothing had altered it. In a system whose entire purpose is
// detecting alteration, a false alteration alarm is the worst defect
// available: it is indistinguishable from the real thing, and it trains
// whoever sees it to disbelieve the alarm that matters.
//
// Truncating here rather than at the call sites is deliberate. This is the
// single point every hashed timestamp passes through, so a future field
// carrying a nanosecond time cannot reintroduce the bug by forgetting.
const storagePrecision = time.Microsecond

// canonicalTime is the one time encoding used everywhere here: RFC 3339
// UTC, truncated to storagePrecision. Normalised to UTC so the same
// instant expressed in two zones produces one preimage, and truncated so
// the preimage survives a round trip through either backend.
//
// The format string still prints nine fractional digits. The final three
// are always zero after truncation; they are kept so the encoding stays
// fixed-width and byte-comparable against CanonicalLeaf.
func canonicalTime(t time.Time) string {
	return t.UTC().Truncate(storagePrecision).Format("2006-01-02T15:04:05.000000000Z")
}

// alignedTo reports whether t sits exactly on a d-sized grid boundary.
//
// Uses Equal rather than ==. Comparing time.Time with == compares the
// struct — wall clock, monotonic reading, and location POINTER — so two
// values denoting the same instant can compare unequal. Equal compares
// the instant, which is the question being asked.
//
// Truncate rounds down relative to the zero time (1 Jan year 1,
// 00:00:00 UTC), so for an hour it lands on the hour and for 24h on
// midnight UTC. It also strips the monotonic reading, which is another
// reason not to use ==.
func alignedTo(t time.Time, d time.Duration) bool {
	u := t.UTC()
	return u.Truncate(d).Equal(u)
}

// RootOverExecutionDigests builds one project's interval root from the
// digest roots of its executions, in the order given.
//
// ORDER IS THE CALLER'S RESPONSIBILITY AND IT IS PART OF THE RESULT.
// This does not sort. The caller supplies executions in the order the
// store returned them (sealed_at, then execution_id), and that order is
// what the anchored root commits to. Sorting here would silently
// disagree with whatever order a verifier reconstructs from the store,
// and the mismatch would surface as tampering.
//
// Each input is a hex Merkle root produced by Compute. They are treated
// as ALREADY-HASHED leaves — Compute's output is a root, not raw data,
// so hashing it again would build a tree a verifier could not reproduce
// from the same inputs.
//
// Refuses an empty input rather than returning the empty-tree root.
// That value is a real 64-hex digest and would be indistinguishable
// from a genuine root over data that went missing, which is exactly the
// confusion the empty-interval sentinel exists to avoid.
func RootOverExecutionDigests(hexRoots []string) (string, error) {
	if len(hexRoots) == 0 {
		return "", errors.New(
			"attest: no execution digests; an empty interval root must be the " +
				"empty-string sentinel, not the root of an empty tree")
	}
	level := make([][]byte, 0, len(hexRoots))
	for i, h := range hexRoots {
		raw, err := hex.DecodeString(h)
		if err != nil {
			return "", fmt.Errorf("attest: execution digest %d is not hex: %w", i, err)
		}
		if len(raw) != sha256.Size {
			return "", fmt.Errorf(
				"attest: execution digest %d is %d bytes, want %d",
				i, len(raw), sha256.Size)
		}
		level = append(level, raw)
	}
	return hex.EncodeToString(rootFromLeafHashes(level)), nil
}

// TenantLeafHash returns one leaf's hash, hex.
//
// Every field of TenantLeaf is covered. A field left out could be
// edited without moving the leaf hash, which would let a project's
// counts be rewritten inside an anchored tree.
func TenantLeafHash(l TenantLeaf) string {
	var buf []byte
	buf = appendCanonicalField(buf, "domain", tenantLeafDomain)
	buf = appendCanonicalField(buf, "project_id", l.ProjectID)
	buf = appendCanonicalField(buf, "interval_root", l.IntervalRoot)
	buf = appendCanonicalField(buf, "execution_count", strconv.Itoa(l.ExecutionCount))
	buf = appendCanonicalField(buf, "cumulative_count", strconv.FormatUint(l.CumulativeCount, 10))
	buf = appendCanonicalField(buf, "prev_leaf_hash", l.PrevLeafHash)
	return hex.EncodeToString(merkleLeafHash(buf))
}

// CheckpointHash returns the checkpoint's hash, hex.
//
// Covers every field except Hash itself. Note PrevLogEntryID IS
// covered: it is the link to the public log, and a link that could be
// rewritten after anchoring would let a broken chain be repaired on
// paper.
func CheckpointHash(c Checkpoint) string {
	var buf []byte
	buf = appendCanonicalField(buf, "domain", checkpointDomain)
	buf = appendCanonicalField(buf, "format", c.Format)
	buf = appendCanonicalField(buf, "seq", strconv.FormatUint(c.Seq, 10))
	buf = appendCanonicalField(buf, "prev_checkpoint_hash", c.PrevCheckpointHash)
	buf = appendCanonicalField(buf, "prev_log_entry_id", c.PrevLogEntryID)
	buf = appendCanonicalField(buf, "interval_start", canonicalTime(c.IntervalStart))
	buf = appendCanonicalField(buf, "interval_end", canonicalTime(c.IntervalEnd))
	buf = appendCanonicalField(buf, "tenant_leaf_count", strconv.Itoa(c.TenantLeafCount))
	buf = appendCanonicalField(buf, "merkle_root", c.MerkleRoot)
	buf = appendCanonicalField(buf, "cumulative_count", strconv.FormatUint(c.CumulativeCount, 10))
	buf = appendCanonicalField(buf, "created_at_unattested", canonicalTime(c.CreatedAtUnattested))
	return hex.EncodeToString(merkleLeafHash(buf))
}

// CheckpointParams is what a caller supplies to build the next
// checkpoint. Separate from Checkpoint so the derived fields — root,
// counts, hash — cannot be supplied by a caller and must be computed.
type CheckpointParams struct {
	// Prev is the previous checkpoint, or nil for genesis.
	Prev *Checkpoint

	// PrevLogEntryID is the log entry Prev was anchored in. Required
	// when Prev is non-nil: a chain whose links do not reach the public
	// log can only be checked by asking Mesedi, which defeats it.
	PrevLogEntryID string

	IntervalStart time.Time
	IntervalEnd   time.Time

	// Interval is the configured checkpoint period, and it is REQUIRED.
	//
	// BuildCheckpoint could derive the length from the two timestamps
	// and not ask. It asks because VerifyChain rejects a checkpoint
	// whose span is not exactly the configured interval, or whose
	// boundaries are not aligned to it — and building something that
	// verification will later reject means anchoring it first, which
	// costs a real transparency-log submission before the mistake
	// surfaces. Cheaper to refuse here.
	Interval time.Duration

	// Leaves may be empty. An empty interval still produces a
	// checkpoint — that is the heartbeat, and the reason a missing
	// interval is evidence rather than ambiguity.
	Leaves []TenantLeaf

	// Now is injected rather than read from the clock, so tests are
	// deterministic. Zero value means time.Now().
	Now time.Time
}

// BuildCheckpoint computes the next checkpoint in the chain.
//
// Refuses rather than producing something subtly wrong: an interval
// that is not positive, a non-genesis checkpoint with no log entry for
// its predecessor, or leaves whose sub-chain counts contradict
// themselves.
func BuildCheckpoint(p CheckpointParams) (Checkpoint, error) {
	if p.Interval <= 0 {
		return Checkpoint{}, fmt.Errorf(
			"attest: Interval must be positive, got %s", p.Interval)
	}
	if !p.IntervalEnd.After(p.IntervalStart) {
		return Checkpoint{}, fmt.Errorf(
			"attest: interval_end %s is not after interval_start %s",
			canonicalTime(p.IntervalEnd), canonicalTime(p.IntervalStart))
	}
	if d := p.IntervalEnd.Sub(p.IntervalStart); d != p.Interval {
		return Checkpoint{}, fmt.Errorf(
			"%w: interval spans %s but the configured interval is %s",
			ErrChainInterval, d, p.Interval)
	}
	// Boundaries must sit ON the interval grid, not merely be the right
	// length apart. A chain of correctly-sized intervals that all start
	// at :37 past cannot be checked for holes against a published
	// schedule, which is the property the whole mechanism rests on.
	if !alignedTo(p.IntervalStart, p.Interval) {
		return Checkpoint{}, fmt.Errorf(
			"%w: interval_start %s is not aligned to a %s boundary",
			ErrChainInterval, canonicalTime(p.IntervalStart), p.Interval)
	}

	var (
		seq      uint64 = 1
		prevHash        = ZeroHash
		baseline uint64
	)
	if p.Prev != nil {
		// Recompute the predecessor's hash instead of trusting the field.
		// Building on a Prev whose stored Hash does not match its
		// contents would anchor a link to a hash that is not actually
		// the predecessor's, and the failure would surface later at the
		// WRONG checkpoint — pointing an auditor at the new one instead
		// of the corrupted one.
		if got := CheckpointHash(*p.Prev); got != p.Prev.Hash {
			return Checkpoint{}, fmt.Errorf(
				"%w: predecessor %d claims hash %s but hashes to %s; refusing to "+
					"extend a chain from a corrupted checkpoint",
				ErrChainHashMismatch, p.Prev.Seq, short(p.Prev.Hash), short(got))
		}
		if p.PrevLogEntryID == "" {
			return Checkpoint{}, fmt.Errorf(
				"attest: checkpoint %d needs the log entry id for checkpoint %d; "+
					"a chain that does not reach the public log can only be "+
					"verified by asking Mesedi", p.Prev.Seq+1, p.Prev.Seq)
		}
		if !p.IntervalStart.Equal(p.Prev.IntervalEnd) {
			return Checkpoint{}, fmt.Errorf(
				"%w: checkpoint %d starts at %s but %d ended at %s",
				ErrChainInterval, p.Prev.Seq+1, canonicalTime(p.IntervalStart),
				p.Prev.Seq, canonicalTime(p.Prev.IntervalEnd))
		}
		seq = p.Prev.Seq + 1
		prevHash = p.Prev.Hash
		baseline = p.Prev.CumulativeCount
	} else if p.PrevLogEntryID != "" {
		return Checkpoint{}, fmt.Errorf(
			"%w: genesis has no predecessor but a PrevLogEntryID was supplied",
			ErrChainGenesis)
	}

	// Tenant leaves, in the order given. Order is the caller's
	// responsibility and must be deterministic, because the root depends
	// on it; the scheduler increment sorts by project_id.
	var (
		leafHashes = make([][]byte, 0, len(p.Leaves))
		added      uint64
	)
	for i, l := range p.Leaves {
		if l.ProjectID == "" {
			return Checkpoint{}, fmt.Errorf("attest: leaf %d has no project_id", i)
		}
		if l.PrevLeafHash == "" {
			return Checkpoint{}, fmt.Errorf(
				"attest: leaf %d (%s) has an empty prev_leaf_hash; use ZeroHash "+
					"for a project's first leaf so the field is always hashed",
				i, l.ProjectID)
		}
		if l.ExecutionCount < 0 {
			return Checkpoint{}, fmt.Errorf(
				"attest: leaf %d (%s) has negative execution_count", i, l.ProjectID)
		}
		// A leaf claiming executions but no root, or a root but no
		// executions, is internally inconsistent and would anchor a
		// contradiction.
		if (l.ExecutionCount == 0) != (l.IntervalRoot == "") {
			return Checkpoint{}, fmt.Errorf(
				"attest: leaf %d (%s) has execution_count=%d with interval_root=%q; "+
					"a root must be present exactly when there are executions",
				i, l.ProjectID, l.ExecutionCount, l.IntervalRoot)
		}
		h, err := hex.DecodeString(TenantLeafHash(l))
		if err != nil {
			return Checkpoint{}, fmt.Errorf("attest: leaf %d hash: %w", i, err)
		}
		leafHashes = append(leafHashes, h)
		added += uint64(l.ExecutionCount)
	}

	// EMPTY INTERVAL: explicit empty root, never the empty-tree root.
	root := ""
	if len(leafHashes) > 0 {
		root = hex.EncodeToString(rootFromLeafHashes(leafHashes))
	}

	now := p.Now
	if now.IsZero() {
		now = time.Now()
	}

	c := Checkpoint{
		Format:             CheckpointFormatCurrent,
		Seq:                seq,
		PrevCheckpointHash: prevHash,
		PrevLogEntryID:     p.PrevLogEntryID,
		IntervalStart:      p.IntervalStart.UTC(),
		IntervalEnd:        p.IntervalEnd.UTC(),
		TenantLeafCount:    len(p.Leaves),
		MerkleRoot:         root,
		CumulativeCount:    baseline + added,
		// Truncated at construction as well as inside canonicalTime, so
		// the value STORED is the value HASHED. Truncating only in the
		// hash would leave the database holding a timestamp the hash does
		// not commit to, which reads as a discrepancy to anyone comparing
		// the row against the preimage by hand.
		CreatedAtUnattested: now.UTC().Truncate(storagePrecision),
	}
	c.Hash = CheckpointHash(c)
	return c, nil
}

// VerifyChain checks a consecutive run of checkpoints.
//
// This is the function an auditor runs, and it is written to need
// nothing from Mesedi: given checkpoints pulled from a transparency
// log, it decides on its own whether the chain is intact. Every check
// names the sequence number where it failed.
//
// What it CANNOT check, because the input does not contain it: whether
// each PrevLogEntryID actually resolves to an entry in the log, and
// whether the log's integration times are ordered sensibly against the
// intervals claimed. Those require the log itself. A caller doing a
// full audit must do them too, and this function's success does not
// stand in for them.
func VerifyChain(cps []Checkpoint, interval time.Duration) error {
	if len(cps) == 0 {
		return ErrChainEmpty
	}
	if interval <= 0 {
		return fmt.Errorf("attest: interval must be positive, got %s", interval)
	}

	for i, c := range cps {
		if c.Format != CheckpointFormatCurrent {
			// v1 gets its own message. "want v2, got v1" invites the
			// reader to go looking for a v1 verifier that could be
			// written; there isn't one worth writing, and saying so here
			// saves someone the afternoon.
			if c.Format == CheckpointFormatV1 {
				return fmt.Errorf("%w: checkpoint %d is format %q, which cannot be verified "+
					"at all: v1 hashed timestamps to nanoseconds that Postgres does not store, "+
					"so a v1 checkpoint fails its own read-back even when untampered. "+
					"Exactly one was ever written and it was abandoned, not migrated",
					ErrChainFormat, c.Seq, c.Format)
			}
			return fmt.Errorf("%w: checkpoint %d has format %q, want %q",
				ErrChainFormat, c.Seq, c.Format, CheckpointFormatCurrent)
		}
		// Seq starts at 1. Rejected explicitly because a Seq of 0 would
		// otherwise slip past the genesis checks below, which only fire
		// on Seq == 1, and would then look like a legitimate mid-chain
		// window start.
		if c.Seq < 1 {
			return fmt.Errorf("%w: checkpoint sequence numbers start at 1, got %d",
				ErrChainSequence, c.Seq)
		}

		// Recompute rather than trust. The stored Hash is convenience;
		// this is the check.
		if got := CheckpointHash(c); got != c.Hash {
			return fmt.Errorf("%w: checkpoint %d claims hash %s but its contents hash to %s",
				ErrChainHashMismatch, c.Seq, short(c.Hash), short(got))
		}

		if (c.TenantLeafCount == 0) != (c.MerkleRoot == "") {
			return fmt.Errorf("%w: checkpoint %d has tenant_leaf_count=%d, merkle_root=%q",
				ErrChainEmptyRootMismatch, c.Seq, c.TenantLeafCount, c.MerkleRoot)
		}

		if !c.IntervalEnd.After(c.IntervalStart) {
			return fmt.Errorf("%w: checkpoint %d ends at or before it starts",
				ErrChainInterval, c.Seq)
		}
		if d := c.IntervalEnd.Sub(c.IntervalStart); d != interval {
			return fmt.Errorf("%w: checkpoint %d covers %s, expected %s",
				ErrChainInterval, c.Seq, d, interval)
		}
		// Alignment, not just duration. A chain of correctly-sized
		// intervals that all begin at :37 past the hour cannot be
		// checked for holes against a published schedule — and being
		// checkable against a published schedule is the property the
		// whole mechanism rests on. Checked here as well as in
		// BuildCheckpoint because this is the function an auditor runs,
		// and it must not depend on the builder having been careful.
		if !alignedTo(c.IntervalStart, interval) {
			return fmt.Errorf("%w: checkpoint %d starts at %s, not on a %s boundary",
				ErrChainInterval, c.Seq, canonicalTime(c.IntervalStart), interval)
		}

		if i == 0 {
			// The first element of the slice need not be chain genesis —
			// an auditor may verify a window. Only a checkpoint claiming
			// Seq 1 must look like genesis.
			if c.Seq == 1 {
				if c.PrevCheckpointHash != ZeroHash {
					return fmt.Errorf(
						"%w: checkpoint 1 must name 64 hex zeros as its predecessor, got %s",
						ErrChainGenesis, short(c.PrevCheckpointHash))
				}
				if c.PrevLogEntryID != "" {
					return fmt.Errorf("%w: checkpoint 1 must not name a predecessor log entry",
						ErrChainGenesis)
				}
			}
			continue
		}

		prev := cps[i-1]
		if c.Seq != prev.Seq+1 {
			return fmt.Errorf("%w: %d follows %d; a checkpoint is missing",
				ErrChainSequence, c.Seq, prev.Seq)
		}
		if c.PrevCheckpointHash != prev.Hash {
			return fmt.Errorf("%w: checkpoint %d names %s but checkpoint %d hashes to %s",
				ErrChainPrevHash, c.Seq, short(c.PrevCheckpointHash), prev.Seq, short(prev.Hash))
		}
		if c.PrevLogEntryID == "" {
			return fmt.Errorf("%w: checkpoint %d names no log entry for checkpoint %d",
				ErrChainPrevHash, c.Seq, prev.Seq)
		}
		if !c.IntervalStart.Equal(prev.IntervalEnd) {
			return fmt.Errorf("%w: checkpoint %d starts at %s but %d ended at %s",
				ErrChainInterval, c.Seq, canonicalTime(c.IntervalStart),
				prev.Seq, canonicalTime(prev.IntervalEnd))
		}
		if c.CumulativeCount < prev.CumulativeCount {
			return fmt.Errorf("%w: checkpoint %d has %d, down from %d at checkpoint %d",
				ErrChainCountRegressed, c.Seq, c.CumulativeCount,
				prev.CumulativeCount, prev.Seq)
		}
	}
	return nil
}

// ErrIntervalLeaves means the supplied leaves are not the ones the
// checkpoint committed to.
var ErrIntervalLeaves = errors.New("attest: leaves do not match the checkpoint")

// ErrTenantSubChain means one project's leaves do not link up.
var ErrTenantSubChain = errors.New("attest: tenant sub-chain is broken")

// VerifyIntervalLeaves proves that a set of leaves is exactly what a
// checkpoint committed to.
//
// This is the bridge, and without it the leaves are just numbers
// somebody handed you. VerifyChain establishes that the CHECKPOINTS are
// intact; this establishes that a given set of leaves is what a
// particular checkpoint anchored. Only then is walking a project's
// sub-chain worth anything.
//
// Order matters: the root depends on it. The leaves must be presented
// in the order they were built in.
func VerifyIntervalLeaves(c Checkpoint, leaves []TenantLeaf) error {
	if len(leaves) != c.TenantLeafCount {
		return fmt.Errorf("%w: checkpoint %d committed to %d leaves, got %d",
			ErrIntervalLeaves, c.Seq, c.TenantLeafCount, len(leaves))
	}
	if len(leaves) == 0 {
		if c.MerkleRoot != "" {
			return fmt.Errorf("%w: checkpoint %d has no leaves but a non-empty root",
				ErrIntervalLeaves, c.Seq)
		}
		return nil
	}
	hashes := make([][]byte, 0, len(leaves))
	for i, l := range leaves {
		h, err := hex.DecodeString(TenantLeafHash(l))
		if err != nil {
			return fmt.Errorf("%w: leaf %d: %v", ErrIntervalLeaves, i, err)
		}
		hashes = append(hashes, h)
	}
	if got := hex.EncodeToString(rootFromLeafHashes(hashes)); got != c.MerkleRoot {
		return fmt.Errorf("%w: checkpoint %d anchored root %s, these leaves produce %s",
			ErrIntervalLeaves, c.Seq, short(c.MerkleRoot), short(got))
	}
	return nil
}

// VerifyTenantSubChain checks one project's leaves across consecutive
// appearances.
//
// Two properties, and the second is the one that makes omission
// arithmetic rather than a matter of judgement:
//
//  1. Each leaf names the previous leaf's hash, so a leaf cannot be
//     removed from the middle without the next one pointing at nothing.
//  2. CumulativeCount advances by exactly ExecutionCount. If executions
//     were dropped from an interval, the running total no longer
//     reconciles with the per-interval counts, and hiding that requires
//     rewriting every later leaf — each of which is anchored.
//
// Leaves must be in interval order, and should first be proven to be
// the anchored ones via VerifyIntervalLeaves. Intervals in which the
// project had no activity simply have no leaf, which is legitimate: the
// arithmetic still reconciles because nothing was added.
func VerifyTenantSubChain(projectID string, leaves []TenantLeaf) error {
	if len(leaves) == 0 {
		return fmt.Errorf("%w: no leaves for project %q", ErrTenantSubChain, projectID)
	}
	for i, l := range leaves {
		if l.ProjectID != projectID {
			return fmt.Errorf("%w: leaf %d belongs to project %q, not %q",
				ErrTenantSubChain, i, l.ProjectID, projectID)
		}
		if i == 0 {
			continue
		}
		prev := leaves[i-1]
		if want := TenantLeafHash(prev); l.PrevLeafHash != want {
			return fmt.Errorf("%w: leaf %d of %q names %s, but the previous leaf "+
				"hashes to %s; a leaf is missing or was replaced",
				ErrTenantSubChain, i, projectID, short(l.PrevLeafHash), short(want))
		}
		// Guarded before the uint64 conversion: these leaves may come
		// from a log rather than from BuildCheckpoint, and uint64(-1) is
		// 18 quintillion, which would make the arithmetic below pass for
		// an obviously corrupt leaf.
		if l.ExecutionCount < 0 {
			return fmt.Errorf("%w: leaf %d of %q has negative execution_count %d",
				ErrTenantSubChain, i, projectID, l.ExecutionCount)
		}
		wantCount := prev.CumulativeCount + uint64(l.ExecutionCount)
		if l.CumulativeCount != wantCount {
			return fmt.Errorf("%w: leaf %d of %q reports %d executions taking the total "+
				"from %d to %d; it should be %d. Executions were dropped or the "+
				"count was rewritten",
				ErrTenantSubChain, i, projectID, l.ExecutionCount,
				prev.CumulativeCount, l.CumulativeCount, wantCount)
		}
	}
	return nil
}

// short abbreviates a hex digest for error messages. Full digests make
// a multi-line error unreadable, and the first eight characters are
// enough to tell two apart when you have both in front of you.
func short(hexDigest string) string {
	if len(hexDigest) <= 8 {
		return hexDigest
	}
	return hexDigest[:8] + "..."
}
