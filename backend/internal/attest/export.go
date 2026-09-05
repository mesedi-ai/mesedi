package attest

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// The chain export: everything one project needs to verify its own
// activity was anchored, and nothing about anyone else's.
//
// The governing rule for this file is that the export is DATA, not a
// claim. Mesedi produces it, so nothing in it may be believed on Mesedi's
// say-so. Every field is either recomputed here from other fields, or
// checked against the public log by the caller. A field that is neither
// does not belong in the export, because its only possible role is to be
// trusted, and trust is the thing this product exists to remove.
//
// That rule is why the export carries execution DIGESTS rather than
// execution counts alone: a count is a claim, a list of digests can be
// folded into a root and compared against what was anchored.

// ChainExportFormatV1 identifies this export shape. Bumped for the same
// reason CheckpointFormat is: a verifier applying v1 rules to a v2 export
// would report failures that are really format drift.
const ChainExportFormatV1 = "mesedi.chain-export.v1"

// ExportedExecution is one sealed execution's identity and digest.
//
// The digest root, not the events. An auditor can fold these roots and
// confirm they produce the anchored interval root, which proves the set of
// executions is complete and unaltered. It does NOT let them confirm that
// a given digest describes what their agent actually did, that needs the
// events, and is a separate, far larger retrieval. Stated in Unverified
// rather than glossed over.
type ExportedExecution struct {
	ExecutionID string `json:"execution_id"`
	DigestRoot  string `json:"digest_root"`
}

// ExportedInterval is one hour: the checkpoint, where it was anchored,
// and this project's place in it.
type ExportedInterval struct {
	Checkpoint Checkpoint `json:"checkpoint"`

	// LogEntryID and AnchoredAt describe where the checkpoint hash was
	// published. Both are unverifiable from inside the export by
	// construction, confirming them is exactly what requires leaving the
	// export and asking the log.
	LogEntryID string    `json:"log_entry_id"`
	AnchoredAt time.Time `json:"anchored_at"`

	// LedgerBackend names which ledger produced LogEntryID: "rekor" for
	// the public transparency log, "mock" for a deterministic local
	// stand-in used in development.
	//
	// Carried because a verifier that assumed every entry id was a real
	// Rekor index would report a mock-anchored checkpoint as a FAILURE,
	// when the truth is that no public-log claim was ever made for it.
	// Those are different findings and the difference is the reader's to
	// know. Mesedi's own mock ids are additionally spelled "rekor-..."
	// today, which makes guessing from the id itself actively unsafe.
	LedgerBackend string `json:"ledger_backend,omitempty"`

	// LeafPreimage is the exact string the ledger hashed to produce the
	// entry at LogEntryID. It is the ONLY thing that connects a
	// checkpoint to its log entry.
	//
	// The log does not record the checkpoint hash. It records sha256 of a
	// canonical leaf that contains it, so a verifier comparing the log's
	// value against Checkpoint.Hash finds a mismatch every time and is
	// right to. With the preimage the reader can hash it, compare against
	// what the log holds, and confirm the checkpoint's own hash appears
	// inside it, none of which requires trusting Mesedi.
	//
	// Empty on checkpoints anchored before 2026-09-04. Those are
	// permanently unverifiable, because the nonce inside their preimages
	// was generated in Verdifax's handler and discarded. A verifier must
	// report them as UNVERIFIABLE and never as tampering: the record is
	// not known to be wrong, it is merely no longer checkable, and
	// conflating the two would be a falsehood in the opposite direction.
	LeafPreimage string `json:"leaf_preimage,omitempty"`

	// AnchorProof is the log's own proof that the entry at LogEntryID is
	// committed under a root the log signed: a JSON object holding
	// log_id, entry_body and inclusion_proof.
	//
	// It is what turns the sentence above, "confirming them is exactly
	// what requires leaving the export and asking the log", from true
	// into false. With it, the export is self-contained: a verifier
	// recomputes the Merkle root from the sibling path, checks the log's
	// signature over that root against a pinned public key, and confirms
	// log_id names the log that key belongs to. No network, which means
	// an auditor on a closed network can verify, and a checkpoint stays
	// verifiable even if the log itself is later retired (task #22).
	//
	// Raw JSON, passed through untouched from the anchoring receipt. It
	// is the log's evidence and Verdifax's, not Mesedi's, and re-encoding
	// it here would drop whatever either of them adds next.
	//
	// Empty means offline verification is not possible for this
	// checkpoint. That is NOT the same as an empty LeafPreimage: an
	// anchor with a preimage and no proof is still fully checkable by
	// fetching the entry from the log. A verifier must keep the two
	// findings apart.
	AnchorProof json.RawMessage `json:"anchor_proof,omitempty"`

	// Leaf and Proof are nil when this project had no sealed executions
	// in the interval. That is a normal, expected state and must not read
	// as a gap: the checkpoint still exists, still anchors, and still
	// chains. An agency that ran nothing at 3am should not be shown a
	// verification failure for 3am.
	Leaf  *TenantLeaf     `json:"leaf,omitempty"`
	Proof *InclusionProof `json:"proof,omitempty"`

	// Executions is in the order the leaf's IntervalRoot was folded over.
	// Order is load-bearing: RootOverExecutionDigests is order-dependent,
	// so a re-sorted list produces a different root and reads as tampering.
	Executions []ExportedExecution `json:"executions,omitempty"`
}

// ChainExport is the whole document handed to an auditor.
type ChainExport struct {
	Format      string    `json:"format"`
	GeneratedAt time.Time `json:"generated_at"`

	// ProjectID is whose export this is. The verifier checks every leaf
	// belongs to this project, so an export that mixes tenants, whether
	// by bug or by malice, is rejected rather than silently reported as
	// someone else's activity.
	ProjectID string `json:"project_id"`

	// Interval the chain runs on, carried so a verifier can check tiling
	// without being told out of band. Named in the export because a
	// verifier that assumes one hour would silently accept a chain that
	// had quietly changed cadence.
	IntervalSeconds int `json:"interval_seconds"`

	Intervals []ExportedInterval `json:"intervals"`
}

// plural renders a count with the right noun.
//
// Small, and worth having: this report is read by auditors, contracting
// officers and commanders, and "1 hourly entries" reads as carelessness
// in a document whose whole argument is that it was careful.
func plural(n int, one, many string) string {
	if n == 1 {
		return fmt.Sprintf("%d %s", n, one)
	}
	return fmt.Sprintf("%d %s", n, many)
}

// CheckResult is one named finding. Carries prose because the destination
// is a PDF read by someone who is not a cryptographer, and "false" on its
// own tells them nothing they can act on.
type CheckResult struct {
	Name   string `json:"name"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// ExportVerification is the full result of checking an export.
type ExportVerification struct {
	Checks []CheckResult `json:"checks"`

	// Unverified is the load-bearing field of this whole package.
	//
	// A verification report that lists only what passed invites the reader
	// to assume everything was checked. Every limit, offline mode, events
	// not retrieved, log entries not resolved, goes here, and the PDF
	// prints it as prominently as the passes. An evaluator who discovers a
	// limit themselves concludes it was concealed; one who is handed it
	// concludes the rest is straight.
	Unverified []string `json:"unverified"`

	OK bool `json:"ok"`
}

func (v *ExportVerification) add(name string, ok bool, format string, args ...any) {
	v.Checks = append(v.Checks, CheckResult{
		Name: name, OK: ok, Detail: fmt.Sprintf(format, args...),
	})
	if !ok {
		v.OK = false
	}
}

// VerifyChainExport checks everything checkable without a network.
//
// It deliberately does NOT contact the transparency log, and says so in
// Unverified. Anchoring is what makes the chain evidence rather than
// bookkeeping, so the caller must resolve the log entries separately and
// append its own results. Splitting it this way keeps this function pure
// and testable, and makes the offline limitation impossible to forget:
// the only way to clear that Unverified line is to actually do the work.
func VerifyChainExport(e ChainExport) ExportVerification {
	v := ExportVerification{OK: true}

	if e.Format != ChainExportFormatV1 {
		v.add("file format", false,
			"export claims format %q, this verifier understands %q",
			e.Format, ChainExportFormatV1)
		return v
	}
	v.add("file format", true, "%s", e.Format)

	if e.ProjectID == "" {
		v.add("project identity", false, "export names no project")
		return v
	}
	if len(e.Intervals) == 0 {
		v.add("intervals present", false,
			"export contains no intervals, so there is nothing to verify")
		return v
	}
	if e.IntervalSeconds <= 0 {
		v.add("interval cadence", false,
			"export declares an interval of %d seconds", e.IntervalSeconds)
		return v
	}
	interval := time.Duration(e.IntervalSeconds) * time.Second

	// 1. The chain itself: continuity, hash recomputation, interval tiling,
	// count monotonicity. VerifyChain owns all of that; duplicating any of
	// it here would create a second opinion that could disagree.
	cps := make([]Checkpoint, 0, len(e.Intervals))
	for _, iv := range e.Intervals {
		cps = append(cps, iv.Checkpoint)
	}
	if err := VerifyChain(cps, interval); err != nil {
		v.add("records link up", false, "%v", err)
	} else {
		v.add("records link up", true,
			"%d checkpoints in an unbroken sequence, each naming the one before it. "+
				"Every fingerprint was recalculated here, not taken on trust", len(cps))
	}

	// 2. Every anchored checkpoint must name where it was anchored. A
	// checkpoint with no log entry was never published, and an unpublished
	// checkpoint proves nothing no matter how well it chains.
	var unanchored []uint64
	for _, iv := range e.Intervals {
		if iv.LogEntryID == "" {
			unanchored = append(unanchored, iv.Checkpoint.Seq)
		}
	}
	if len(unanchored) > 0 {
		v.add("all checkpoints published", false,
			"checkpoints %v do not say where they were published",
			unanchored)
	} else {
		v.add("all checkpoints published", true,
			"every checkpoint says where it was published")
	}

	// 2b. Publication was never skipped for an hour.
	//
	// Each checkpoint carries PrevLogEntryID, the public-log position of
	// the checkpoint before it. VerifyChain only asserts that field is
	// NON-EMPTY, which is the weakest possible reading of it: any string
	// passes. Nothing until now compared it against the position the
	// previous checkpoint was actually published at.
	//
	// That comparison is what makes "the chain cannot advance without
	// publishing" checkable by a reader instead of a claim about our
	// scheduler that only someone reading our source can evaluate. It is
	// the property that separates this design from one where anchoring is
	// a setting somebody can turn off, and it was the last major claim in
	// the report that rested on trusting us.
	//
	// It bites because it COMPOSES with the transparency-log check. On its
	// own, a fabricated chain could carry internally consistent made-up
	// positions. But every position named here is separately proven to
	// hold the leaf committing to that specific checkpoint. So the two
	// together say: hour N+1 names a public-log position, that position
	// really contains hour N, and hour N really contains hour N-1. A
	// missing hour has nowhere to hide, because the hour after it would
	// have to name a published position for a checkpoint that was never
	// published.
	//
	// A ranged export cannot check its own first element: the checkpoint
	// before it is outside the file. Reported rather than skipped
	// silently, because an unchecked link presented as a checked one is
	// the failure this whole document exists to prevent.
	var (
		linkedToPrev   int
		badLinks       []string
		firstUnchecked bool
	)
	for i, iv := range e.Intervals {
		cp := iv.Checkpoint
		if i == 0 {
			// Genesis has no predecessor by construction, which
			// VerifyChain already enforces. Anything else is a window
			// start whose predecessor we do not hold.
			if cp.Seq != 1 {
				firstUnchecked = true
			}
			continue
		}
		prev := e.Intervals[i-1]
		// Only adjacent checkpoints can be compared. If the sequence is
		// broken, VerifyChain has already reported it and comparing
		// non-adjacent entries would add a second, confusing failure.
		if cp.Seq != prev.Checkpoint.Seq+1 {
			continue
		}
		if cp.PrevLogEntryID != prev.LogEntryID {
			badLinks = append(badLinks, fmt.Sprintf(
				"hour %d says the hour before it was published at position %q, but hour %d "+
					"was published at %q", cp.Seq, cp.PrevLogEntryID,
				prev.Checkpoint.Seq, prev.LogEntryID))
			continue
		}
		linkedToPrev++
	}
	switch {
	case len(badLinks) > 0:
		v.add("publication never skipped", false,
			"%s. An hour was recorded without the hour before it being published, "+
				"or the record of where it was published has been altered",
			strings.Join(badLinks, "; "))
	case linkedToPrev == 0:
		v.add("publication never skipped", true,
			"only one hour is covered here, so there is no earlier hour to link it to")
	default:
		detail := fmt.Sprintf("each of %s names the exact public-log position of the "+
			"hour before it, and every one matched. Publishing even one hour late or not "+
			"at all would break this",
			plural(linkedToPrev, "hour", "hours"))
		if firstUnchecked {
			detail += ". The earliest hour in this file has no earlier hour here to link " +
				"to, so that one link is not checked"
		}
		v.add("publication never skipped", true, "%s", detail)
	}

	// 3. This project's leaves, per interval.
	var (
		leaves       []TenantLeaf
		withActivity int
		emptyForUs   int
	)
	for _, iv := range e.Intervals {
		seq := iv.Checkpoint.Seq

		if iv.Leaf == nil {
			if iv.Proof != nil || len(iv.Executions) > 0 {
				v.add(fmt.Sprintf("hour %d consistency", seq), false,
					"no leaf for this project, but the export still carries a proof "+
						"or executions for it")
			}
			emptyForUs++
			continue
		}
		withActivity++

		if iv.Leaf.ProjectID != e.ProjectID {
			v.add(fmt.Sprintf("hour %d ownership", seq), false,
				"export is for project %q but this leaf belongs to %q",
				e.ProjectID, iv.Leaf.ProjectID)
			continue
		}
		leaves = append(leaves, *iv.Leaf)

		if iv.Proof == nil {
			v.add(fmt.Sprintf("hour %d inclusion", seq), false,
				"leaf present but no inclusion proof, so it cannot be tied to the "+
					"anchored root")
			continue
		}
		if err := VerifyTenantLeafInclusion(iv.Checkpoint, *iv.Leaf, *iv.Proof); err != nil {
			v.add(fmt.Sprintf("hour %d inclusion", seq), false, "%v", err)
			continue
		}

		// 4. The leaf's own root must be the fold over the executions.
		// This is what connects "my executions" to "the anchored root":
		// without it, the leaf is just a number Mesedi wrote down.
		roots := make([]string, 0, len(iv.Executions))
		for _, x := range iv.Executions {
			roots = append(roots, x.DigestRoot)
		}
		got, err := RootOverExecutionDigests(roots)
		if err != nil {
			v.add(fmt.Sprintf("hour %d combined fingerprint", seq), false,
				"cannot fold %d execution digests: %v", len(roots), err)
			continue
		}
		if got != iv.Leaf.IntervalRoot {
			v.add(fmt.Sprintf("hour %d combined fingerprint", seq), false,
				"the %d executions listed fold to %s, but the anchored leaf "+
					"committed to %s", len(roots), short(got), short(iv.Leaf.IntervalRoot))
			continue
		}
		if iv.Leaf.ExecutionCount != len(iv.Executions) {
			v.add(fmt.Sprintf("hour %d run count", seq), false,
				"leaf claims %d executions, export lists %d",
				iv.Leaf.ExecutionCount, len(iv.Executions))
			continue
		}

		v.add(fmt.Sprintf("hour %d", seq), true,
			"%d agent runs, whose combined fingerprint matches this project's entry "+
				"for the hour, inside the published checkpoint", len(iv.Executions))
	}

	// 5. The tenant sub-chain. Each of this project's leaves names the
	// previous one, so a leaf removed from an earlier interval breaks the
	// link even though the interval tree for that hour still verifies.
	if len(leaves) > 0 {
		if err := VerifyTenantSubChain(e.ProjectID, leaves); err != nil {
			v.add("this project's entries", false, "%v", err)
		} else {
			v.add("this project's entries", true,
				"%s for this project, each naming the one before it, with running "+
					"totals that add up", plural(len(leaves), "hourly entry", "hourly entries"))
		}

		// 6. WHERE THE EXPORT BEGINS. This check exists because its
		// absence was a hole, and the hole was the exact attack the chain
		// is built to defeat.
		//
		// VerifyTenantSubChain cannot check the predecessor of its FIRST
		// leaf, there is nothing before it to compare against. So an
		// export that simply omits a project's earliest leaves, presenting
		// those hours as "no activity", chains perfectly and verifies
		// clean. Every later link is intact; the removed history leaves no
		// mark. That is selective omission, dressed as a quiet night.
		//
		// The leaf carries its own refutation. CumulativeCount is the
		// project's running total, so on a genuine first leaf it equals
		// ExecutionCount. Anything larger means executions happened before
		// this export begins, whether or not the export admits it.
		//
		// A partial export is perfectly legitimate, an auditor asking for
		// last month does not want a year, so this is not a failure. It
		// is a fact that must be REPORTED, because the difference between
		// "complete from your first execution" and "starts partway, with
		// 4,812 executions before it" is the difference the reader needs.
		first := leaves[0]
		priorExecutions := first.CumulativeCount - uint64(first.ExecutionCount)
		atGenesis := first.PrevLeafHash == ZeroHash

		switch {
		case atGenesis && priorExecutions == 0:
			v.add("coverage", true,
				"starts at this project's first published activity, so nothing "+
					"earlier is missing")

		case atGenesis && priorExecutions > 0:
			// Self-contradictory: claims to be the first leaf while its own
			// running total says otherwise. One of the two was edited.
			v.add("coverage", false,
				"the earliest leaf claims to be this project's first, but its "+
					"cumulative total of %d exceeds its own count of %d, so %d "+
					"executions preceded it",
				first.CumulativeCount, first.ExecutionCount, priorExecutions)

		default:
			v.add("coverage", true,
				"starts partway through: %d earlier agent runs were published but "+
					"are not covered here",
				priorExecutions)
			v.Unverified = append(v.Unverified, fmt.Sprintf(
				"This file starts partway through the project's history. %d earlier "+
					"agent runs were published and are NOT covered here. Ask for the "+
					"full range to check them.",
				priorExecutions))
		}
	}

	if emptyForUs > 0 {
		v.add("quiet hours", true,
			"%d of %d hours held no agent runs for this project. That is normal and "+
				"not a gap: the hourly records still exist and still link up",
			emptyForUs, len(e.Intervals))
	}

	// The limits, always, whether or not anything failed.
	v.Unverified = append(v.Unverified,
		"Transparency log entries were NOT resolved. This check confirms the export "+
			"is internally consistent; it does not confirm the checkpoints were "+
			"published. Re-run with network access to check the log.",
		"The agent runs themselves were not opened. This shows the set of runs is "+
			"complete and unaltered. It does not show what any individual run did, "+
			"which needs the underlying records.",
		"Nothing here judges whether the AI was right. A passing report means the "+
			"record is intact, not that the decisions in it were good.",
	)

	return v
}
