package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"mesedi/backend/internal/attest"
	"mesedi/backend/internal/store"
)

// rawJSONOrNil carries a stored JSON string into the export without
// re-encoding it.
//
// nil, not an empty json.RawMessage: an empty RawMessage marshals to
// nothing and produces invalid JSON in the enclosing document, whereas
// nil combined with omitempty makes the field disappear, which is the
// honest rendering, since absent means "no offline proof was captured".
//
// Malformed stored JSON is dropped rather than emitted. A verifier
// handed a broken proof would report a verification failure, and the
// only true statement about a proof that will not parse is that there
// isn't one.
func rawJSONOrNil(s string) json.RawMessage {
	if s == "" || !json.Valid([]byte(s)) {
		return nil
	}
	return json.RawMessage(s)
}

// GET /me/chain/export, the file an auditor takes away.
//
// The governing rule, from attest/export.go: this emits DATA, not a claim.
// Mesedi assembles it, so nothing in it may be believed on Mesedi's
// say-so. Every field is either recomputable by the reader or checkable
// against the public transparency log. That is why the response carries
// execution digests rather than a count, and an inclusion proof rather
// than an assurance that the leaf was included.
//
// It also carries nothing about any other tenant. The proof exposes about
// log2(n) opaque sibling hashes; the leaves of other projects are read
// server-side to build it and never leave this process.

// MaxExportExecutions caps how many executions one export will digest.
//
// Digesting is the expensive part: each execution needs its events read
// and folded, which the checkpoint scheduler also does hourly. 20,000 is
// generous for a month of one project's activity and still bounded.
//
// REFUSES rather than truncating, for the reason that governs every cap
// in this system: a silently shortened export is indistinguishable from
// an export of a chain with executions missing, and telling those two
// apart is the entire product.
const MaxExportExecutions = 20000

// buildChainExport assembles one project's export over a sequence range.
//
// Kept separate from the handler so it can be tested against a store
// without an HTTP round trip, and so the handler stays about parsing and
// status codes rather than about evidence.
func (h *Handlers) buildChainExport(
	ctx context.Context, projectID string, fromSeq, toSeq uint64,
) (attest.ChainExport, error) {
	cps, err := h.Store.ListCheckpointRange(ctx, fromSeq, toSeq)
	if err != nil {
		return attest.ChainExport{}, err
	}
	if len(cps) == 0 {
		return attest.ChainExport{}, fmt.Errorf(
			"no checkpoints in range [%d, %d]", fromSeq, toSeq)
	}

	// All tenants' leaves, because an inclusion proof needs the whole
	// level. This map does not leave the process.
	leavesBySeq, err := h.Store.ListCheckpointLeavesRange(ctx, fromSeq, toSeq)
	if err != nil {
		return attest.ChainExport{}, err
	}

	// The cadence is measured from the data rather than assumed to be an
	// hour. A verifier told the wrong cadence would accept a chain that
	// had quietly changed it, and hardcoding CheckpointInterval here would
	// make this export lie about a chain built under a different one.
	interval := cps[0].Checkpoint.IntervalEnd.Sub(cps[0].Checkpoint.IntervalStart)
	if interval <= 0 {
		return attest.ChainExport{}, fmt.Errorf(
			"checkpoint %d has a non-positive interval", cps[0].Checkpoint.Seq)
	}

	out := attest.ChainExport{
		Format:          attest.ChainExportFormatV1,
		GeneratedAt:     time.Now().UTC(),
		ProjectID:       projectID,
		IntervalSeconds: int(interval.Seconds()),
	}

	totalExecutions := 0
	for _, ac := range cps {
		cp := ac.Checkpoint
		iv := attest.ExportedInterval{
			Checkpoint: cp,
			LogEntryID: ac.Anchor.LogEntryID,
			AnchoredAt: ac.Anchor.AnchoredAt,

			// Both carried verbatim, including when empty. An anchor with
			// no preimage cannot be checked against the log, and the
			// auditor has to be told that rather than left to infer it
			// from a mismatch that looks like tampering.
			LedgerBackend: ac.Anchor.LedgerBackend,
			LeafPreimage:  ac.Anchor.LeafPreimage,
			AnchorProof:   rawJSONOrNil(ac.Anchor.AnchorProofJSON),
		}

		all := leavesBySeq[cp.Seq]
		var mine *attest.TenantLeaf
		for i := range all {
			if all[i].ProjectID == projectID {
				mine = &all[i]
				break
			}
		}

		// No leaf means this project ran nothing in that hour. The
		// checkpoint is still exported, it is part of the chain and its
		// absence would be a gap, but with no leaf, proof or executions.
		if mine == nil {
			out.Intervals = append(out.Intervals, iv)
			continue
		}

		proof, err := attest.ProveTenantLeaf(cp, all, projectID)
		if err != nil {
			return attest.ChainExport{}, fmt.Errorf(
				"prove leaf for %s in checkpoint %d: %w", projectID, cp.Seq, err)
		}

		execs, err := h.exportedExecutions(
			ctx, projectID, cp.IntervalStart, cp.IntervalEnd)
		if err != nil {
			return attest.ChainExport{}, fmt.Errorf(
				"executions for checkpoint %d: %w", cp.Seq, err)
		}

		totalExecutions += len(execs)
		if totalExecutions > MaxExportExecutions {
			return attest.ChainExport{}, fmt.Errorf(
				"this range covers more than %d executions. Request a shorter "+
					"range: a truncated export cannot be told apart from one with "+
					"executions missing", MaxExportExecutions)
		}

		leafCopy := *mine
		iv.Leaf = &leafCopy
		iv.Proof = &proof
		iv.Executions = execs
		out.Intervals = append(out.Intervals, iv)
	}

	return out, nil
}

// exportedExecutions recomputes one project's execution digests for an
// interval, in the same order the leaf's root was folded over.
//
// Order is load-bearing and comes from ListSealedExecutionIDs, which is
// the same call and the same ordering the scheduler used when it built
// the leaf. Re-sorting here would produce a different fold and the export
// would accuse itself of tampering.
//
// Recomputed rather than stored. The scheduler computes these roots to
// build the leaf and then discards them, so this repeats that work. A
// deliberate trade: storing them would be a schema change and a second
// copy of a value that must agree with the chain, and a stored copy that
// silently disagreed would be worse than redoing the arithmetic. Revisit
// if exports become slow.
func (h *Handlers) exportedExecutions(
	ctx context.Context, projectID string, start, end time.Time,
) ([]attest.ExportedExecution, error) {
	ids, err := h.Store.ListSealedExecutionIDs(ctx, projectID, start, end)
	if err != nil {
		return nil, fmt.Errorf("list sealed executions: %w", err)
	}

	out := make([]attest.ExportedExecution, 0, len(ids))
	for _, execID := range ids {
		evts, err := h.Store.ListEventsForExecution(ctx, execID)
		if err != nil {
			return nil, fmt.Errorf("events for %s: %w", execID, err)
		}
		// ComputeForChain, not Compute: an execution with no events must
		// still appear here. Compute refuses an empty event list by
		// design, and this path used the same call as the scheduler, so
		// the stall that stopped checkpoint construction on 2026-09-04
		// would have reappeared in the export the moment an interval
		// containing an event-less execution was requested.
		d, err := attest.ComputeForChain(execID, evts)
		if err != nil {
			// Refuse the export rather than omit the execution. An export
			// missing an execution the leaf's count includes is exactly
			// the omission this system exists to make visible, and
			// producing one here would be doing it to ourselves.
			return nil, fmt.Errorf("digest execution %s: %w", execID, err)
		}
		out = append(out, attest.ExportedExecution{
			ExecutionID: execID,
			DigestRoot:  d.Root,
		})
	}
	return out, nil
}

func parseSeq(raw string) (uint64, error) {
	n, err := strconv.ParseUint(raw, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("must be a positive whole number")
	}
	return n, nil
}

// HandleChainExport serves the calling project's chain export.
//
//	GET /me/chain/export?from=<seq>&to=<seq>
//	→ 200 application/json, an attest.ChainExport
//	→ 400 on a bad or oversized range
//	→ 404 when the range holds no checkpoints
//
// The project comes from the API key, never from a parameter. There is no
// way to ask for someone else's export because there is nowhere to say
// whose export you want.
//
// RATE LIMIT POSTURE: all tiers, bounded by cost rather than by count.
//
// This is the most expensive read the API serves. A full range digests up
// to MaxExportExecutions executions, each requiring its events read and
// folded, so a caller looping it could do more work than the hourly
// checkpoint scheduler does. It is bounded three ways: the shared
// per-project rate limiter on the private chain, MaxCheckpointRange at
// 744 intervals, and MaxExportExecutions at 20,000 executions, both
// refusing rather than truncating.
//
// Deliberately NOT gated to a paid tier. An agency's ability to verify
// the records it is legally required to retain must not depend on what it
// pays us, that is the one thing in this product that would be indefensible
// to charge for. Revisit the limits if the work becomes a problem; do not
// revisit who is allowed to check their own evidence.
func (h *Handlers) HandleChainExport(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	q := r.URL.Query()
	fromSeq, err := parseSeq(q.Get("from"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "from: "+err.Error())
		return
	}
	toSeq, err := parseSeq(q.Get("to"))
	if err != nil {
		writeError(w, http.StatusBadRequest, "to: "+err.Error())
		return
	}

	// Checked here as well as in the store, using the store's own
	// validator rather than a second copy of the rules. Rejecting at the
	// boundary means an obviously bad range never reaches the database,
	// and the caller gets the same message either way.
	if err := store.ValidateCheckpointRange(fromSeq, toSeq); err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	export, err := h.buildChainExport(r.Context(), authProjectID, fromSeq, toSeq)
	if err != nil {
		// The range errors are the caller's to fix and say so plainly;
		// anything else is ours and must not leak internals.
		h.Logger.Warn("chain export failed",
			"project_id", authProjectID, "from", fromSeq, "to", toSeq,
			"error", err.Error())
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	writeJSON(w, http.StatusOK, export)
}
