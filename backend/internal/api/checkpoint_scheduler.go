package api

// CheckpointScheduler closes one hourly interval at a time, builds the
// checkpoint over it, and anchors it in a public transparency log.
//
// THE ONE FAILURE THIS FILE EXISTS TO PREVENT
//
// A missing checkpoint. Everything else in the chain, the hashes, the
// per-tenant sub-chains, the cumulative counts, exists so that a
// suppressed interval is detectable. If this worker can be induced to
// SKIP an interval and carry on, it hands an attacker the alibi the
// whole mechanism was built to deny.
//
// So every failure path here stalls rather than skips. Concretely:
//
//   - If the head checkpoint is not yet anchored, the next one is NOT
//     built. It cannot be: checkpoint N+1 names N's log entry, and that
//     entry does not exist yet. The scheduler retries the anchor
//     instead. A sustained outage therefore stalls the chain at a known
//     sequence, which is visible and recoverable, rather than leaving a
//     hole, which is indistinguishable from suppression.
//
//   - An interval with no activity still produces a checkpoint. An
//     interval with no checkpoint would be ambiguous between "nothing
//     happened" and "something was hidden", and that ambiguity is the
//     vulnerability.
//
//   - Leaf order comes from the store's deterministic ordering, never
//     from ranging over a map. Go randomises map iteration, so a
//     map-ordered tree would produce a different root on every run for
//     identical data, and the second run would look like tampering.
//
// Wired into main in cmd/api/background_workers.go. It builds and stores
// checkpoints whenever it runs, and publishes them only when an anchorer
// is configured, so starting the backend never submits to a public log
// by itself.

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"mesedi/backend/internal/store"
)

// CheckpointAnchorer submits a checkpoint hash to a transparency log
// and returns where it landed.
//
// An interface rather than a direct Verdifax call for two reasons. The
// caller does not exist yet, and this worker must be testable without
// spending money, every accepted submission in rekor mode is a real
// Sigstore entry with a real cost.
type CheckpointAnchorer interface {
	// AnchorCheckpoint submits cp.Hash and returns the log entry id and
	// backend name. An error means NOT anchored; the caller must not
	// treat a partial result as success.
	// previousTreeSize is the log's tree size when checkpoint Seq-1 was
	// anchored, or zero when that is unknown or does not exist. Passing
	// it lets the anchorer ask the log to prove it only grew, which is
	// the one question no inclusion proof can answer.
	AnchorCheckpoint(ctx context.Context, cp attest.Checkpoint,
		previousTreeSize int64) (store.CheckpointAnchor, error)
}

// Defaults. Interval is deliberately NOT configurable at runtime: a
// value an operator can change is a value an operator can change to
// hide a gap, and the whole point is that the schedule is published and
// checkable.
const (
	CheckpointInterval = time.Hour

	// defaultSettle is how long an execution must sit finished before
	// it is sealed. Events can still arrive after an execution reports
	// it ended; sealing immediately would anchor a digest that later
	// changes, and a changed root reads as tampering.
	defaultSettle = 15 * time.Minute

	// defaultTimeout seals an execution that never ended. Without it
	// such a run stays outside the chain forever, which is an omission
	// nobody can see.
	defaultTimeout = 6 * time.Hour

	// maxIntervalsPerTick bounds catch-up after downtime so a long
	// outage cannot turn one tick into an unbounded loop. Remaining
	// intervals are picked up by the next tick; the chain is never
	// skipped, only drained more slowly.
	maxIntervalsPerTick = 24
)

// CheckpointScheduler is the hourly worker.
type CheckpointScheduler struct {
	Store  store.Store
	Logger *slog.Logger

	// Anchorer may be nil. Nil means checkpoints are built and stored
	// but never anchored, so the chain stalls after the first one ,
	// visibly, at a known sequence. That is the correct behaviour for a
	// deployment with no transparency log configured: it must not
	// silently produce an unanchored chain that looks complete.
	Anchorer CheckpointAnchorer

	// TickInterval governs how often the worker wakes. Default 5
	// minutes when zero, more often than the checkpoint interval so a
	// missed wakeup does not delay an interval by a whole hour. Tests
	// call tick directly and never set this.
	TickInterval time.Duration

	// Settle and Timeout override the sealing defaults. Zero uses them.
	Settle  time.Duration
	Timeout time.Duration

	// Now is injected so tests are deterministic. Nil uses time.Now.
	Now func() time.Time

	once   sync.Once
	cancel context.CancelFunc
}

// Start launches the worker goroutine. Idempotent.
func (s *CheckpointScheduler) Start(ctx context.Context) {
	s.once.Do(func() {
		if s.TickInterval == 0 {
			s.TickInterval = 5 * time.Minute
		}
		runCtx, cancel := context.WithCancel(ctx)
		s.cancel = cancel
		go s.run(runCtx)
	})
}

// Shutdown cancels the worker. Safe to call multiple times.
func (s *CheckpointScheduler) Shutdown() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *CheckpointScheduler) run(ctx context.Context) {
	s.Logger.Info("checkpoint_scheduler: started",
		"tick_interval", s.TickInterval.String(),
		"checkpoint_interval", CheckpointInterval.String(),
		"anchorer_configured", s.Anchorer != nil,
	)
	// Delay the first tick so a fresh boot finishes migrations before
	// the worker starts writing. Missing the first tick costs nothing:
	// intervals are fixed to the clock and are picked up on the next
	// pass, so a late start delays a checkpoint rather than losing one.
	select {
	case <-ctx.Done():
		return
	case <-time.After(30 * time.Second):
	}
	// Before the first tick: store digests for hours sealed before
	// migration 063, while their records can still prove them. Failure
	// is logged and the chain carries on; the next start retries the
	// leaves still unhandled.
	if stored, gaps, err := s.BackfillDigestHistory(ctx); err != nil {
		s.Logger.Error("checkpoint_scheduler: digest history backfill failed",
			"error", err.Error(), "stored", stored, "gaps", gaps)
	} else if stored+gaps > 0 {
		s.Logger.Info("checkpoint_scheduler: digest history backfilled",
			"stored", stored, "labelled_records_changed", gaps)
	}
	if err := s.tick(ctx); err != nil {
		s.Logger.Error("checkpoint_scheduler: first tick failed", "error", err.Error())
	}

	t := time.NewTicker(s.TickInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			s.Logger.Info("checkpoint_scheduler: shutting down")
			return
		case <-t.C:
			if err := s.tick(ctx); err != nil {
				s.Logger.Error("checkpoint_scheduler: tick failed", "error", err.Error())
			}
		}
	}
}

func (s *CheckpointScheduler) now() time.Time {
	if s.Now != nil {
		return s.Now().UTC()
	}
	return time.Now().UTC()
}

func (s *CheckpointScheduler) settle() time.Duration {
	if s.Settle > 0 {
		return s.Settle
	}
	return defaultSettle
}

func (s *CheckpointScheduler) timeout() time.Duration {
	if s.Timeout > 0 {
		return s.Timeout
	}
	return defaultTimeout
}

// tick performs one pass: seal what is eligible, then close as many
// fully-elapsed intervals as possible.
func (s *CheckpointScheduler) tick(ctx context.Context) error {
	now := s.now()

	sealed, err := s.Store.SealExecutions(ctx, now, s.settle(), s.timeout())
	if err != nil {
		return fmt.Errorf("seal executions: %w", err)
	}
	if sealed > 0 {
		s.Logger.Info("checkpoint_scheduler: sealed executions", "count", sealed)
	}

	for i := 0; i < maxIntervalsPerTick; i++ {
		done, err := s.closeNextInterval(ctx, now)
		if err != nil {
			return err
		}
		if !done {
			return nil
		}
	}
	// Hit the per-tick bound. Not an error: the remaining intervals are
	// closed by the next tick. Logged because a scheduler that is
	// permanently at its bound is behind, and that is worth seeing.
	s.Logger.Warn("checkpoint_scheduler: hit the per-tick interval bound",
		"max", maxIntervalsPerTick)
	return nil
}

// closeNextInterval closes at most one interval. Returns true when it
// closed one, so the caller can drain a backlog.
func (s *CheckpointScheduler) closeNextInterval(ctx context.Context, now time.Time) (bool, error) {
	head, err := s.Store.LatestCheckpoint(ctx)
	if err != nil {
		return false, fmt.Errorf("latest checkpoint: %w", err)
	}

	// ANCHOR BEFORE BUILDING. Checkpoint N+1 names N's log entry, so an
	// unanchored head blocks the chain by construction. Retrying here,
	// rather than skipping ahead, is what turns an anchoring outage into
	// a visible stall instead of a hole.
	var prevLogEntryID string
	if head != nil {
		anchor, err := s.Store.GetCheckpointAnchor(ctx, head.Seq)
		if err != nil {
			return false, fmt.Errorf("checkpoint %d anchor state: %w", head.Seq, err)
		}
		if !anchor.Anchored {
			if err := s.anchor(ctx, *head); err != nil {
				// Stall, deliberately. Returning nil rather than an error
				// keeps this out of the error log every five minutes
				// while a log outage lasts; the Warn inside anchor() is
				// the operational signal.
				return false, nil
			}
			// Re-read rather than assume: the anchor write must be
			// durable before anything is built on top of it.
			anchor, err = s.Store.GetCheckpointAnchor(ctx, head.Seq)
			if err != nil {
				return false, fmt.Errorf("checkpoint %d anchor re-read: %w", head.Seq, err)
			}
			if !anchor.Anchored {
				return false, fmt.Errorf(
					"checkpoint %d reports unanchored immediately after a successful "+
						"anchor; refusing to build on it", head.Seq)
			}
		}
		prevLogEntryID = anchor.LogEntryID
	}

	start, end := s.nextInterval(head, now)
	if start.IsZero() {
		return false, nil // nothing to close yet
	}

	leaves, digests, err := s.buildLeaves(ctx, start, end)
	if err != nil {
		return false, err
	}

	cp, err := attest.BuildCheckpoint(attest.CheckpointParams{
		Prev:           head,
		PrevLogEntryID: prevLogEntryID,
		IntervalStart:  start,
		IntervalEnd:    end,
		Interval:       CheckpointInterval,
		Leaves:         leaves,
		Now:            now,
	})
	if err != nil {
		return false, fmt.Errorf("build checkpoint for [%s, %s): %w",
			start.Format(time.RFC3339), end.Format(time.RFC3339), err)
	}

	// Store every execution's digest with its checkpoint, so exports read
	// the anchored value instead of recomputing it from records that may
	// since have changed or been purged (findings E3, E4).
	for project := range digests {
		for i := range digests[project] {
			digests[project][i].CheckpointSeq = cp.Seq
		}
	}
	if err := s.Store.InsertCheckpointWithDigests(ctx, cp, leaves, digests); err != nil {
		return false, fmt.Errorf("insert checkpoint %d: %w", cp.Seq, err)
	}
	s.Logger.Info("checkpoint_scheduler: interval closed",
		"seq", cp.Seq,
		"interval_start", start.Format(time.RFC3339),
		"interval_end", end.Format(time.RFC3339),
		"tenant_leaves", cp.TenantLeafCount,
		"cumulative_count", cp.CumulativeCount,
	)

	// Anchor immediately. Failure is not fatal: the checkpoint is
	// stored, and the next pass finds it unanchored and retries. That is
	// the "anchor late, never abandon" rule, a late checkpoint is
	// checkable, a missing one is not.
	_ = s.anchor(ctx, cp)
	return true, nil
}

// nextInterval returns the next fully-elapsed interval to close, or a
// zero start when there is nothing to do.
//
// An interval is only closed once it has fully elapsed. Closing one
// early would anchor a root over a window still receiving seals, and
// the root would change afterwards.
func (s *CheckpointScheduler) nextInterval(head *attest.Checkpoint, now time.Time) (time.Time, time.Time) {
	var start time.Time
	if head == nil {
		// Genesis: the most recently completed interval. Deliberately
		// NOT a backfill of history, a checkpoint claiming to cover a
		// period before this mechanism existed is a claim nobody can
		// check.
		start = now.Truncate(CheckpointInterval).Add(-CheckpointInterval)
	} else {
		start = head.IntervalEnd
	}
	end := start.Add(CheckpointInterval)
	if now.Before(end) {
		return time.Time{}, time.Time{}
	}
	return start, end
}

// buildLeaves produces one leaf per project with activity in the
// interval, in a deterministic order.
func (s *CheckpointScheduler) buildLeaves(
	ctx context.Context, start, end time.Time,
) ([]attest.TenantLeaf, map[string][]store.ExecutionDigest, error) {
	counts, err := s.Store.CountSealedByProject(ctx, start, end)
	if err != nil {
		return nil, nil, fmt.Errorf("count sealed by project: %w", err)
	}
	if len(counts) == 0 {
		// Empty interval. Returning nil, not an error: the checkpoint
		// still gets built, because an interval with no checkpoint is
		// indistinguishable from a suppressed one.
		return nil, map[string][]store.ExecutionDigest{}, nil
	}

	// SORTED, never map order. Go randomises map iteration, so ranging
	// over `counts` directly would order the leaves differently on every
	// run and produce a different root for identical data, and the
	// second run would look like tampering.
	projects := make([]string, 0, len(counts))
	for p := range counts {
		projects = append(projects, p)
	}
	sort.Strings(projects)

	leaves := make([]attest.TenantLeaf, 0, len(projects))
	digests := make(map[string][]store.ExecutionDigest, len(projects)) // max: one entry per project sealed this hour
	for _, projectID := range projects {
		root, projectDigests, err := s.projectIntervalRoot(ctx, projectID, start, end)
		if err != nil {
			return nil, nil, err
		}
		n := len(projectDigests)
		digests[projectID] = projectDigests
		if n == 0 {
			// Counted but produced no executions: the two queries
			// disagree, which means the window shifted underneath us.
			// Refuse rather than anchor a leaf that does not describe
			// its own interval.
			return nil, nil, fmt.Errorf(
				"project %s was counted in [%s, %s) but produced no executions; "+
					"refusing to anchor a leaf that contradicts its own count",
				projectID, start.Format(time.RFC3339), end.Format(time.RFC3339))
		}

		prevHash := attest.ZeroHash
		var prevCumulative uint64
		prev, err := s.Store.LatestTenantLeaf(ctx, projectID)
		if err != nil {
			return nil, nil, fmt.Errorf("latest leaf for %s: %w", projectID, err)
		}
		if prev != nil {
			prevHash = attest.TenantLeafHash(*prev)
			prevCumulative = prev.CumulativeCount
		}

		leaves = append(leaves, attest.TenantLeaf{
			ProjectID:       projectID,
			IntervalRoot:    root,
			ExecutionCount:  n,
			CumulativeCount: prevCumulative + uint64(n),
			PrevLeafHash:    prevHash,
		})
	}
	return leaves, digests, nil
}

// projectIntervalRoot computes one project's Merkle root over the
// digests of its executions sealed in the interval.
//
// Order comes from the store, which sorts by sealed_at then
// execution_id. That ordering is part of the contract: the root depends
// on it, and a differently-ordered rebuild would not match.
func (s *CheckpointScheduler) projectIntervalRoot(
	ctx context.Context, projectID string, start, end time.Time,
) (string, []store.ExecutionDigest, error) {
	ids, err := s.Store.ListSealedExecutionIDs(ctx, projectID, start, end)
	if err != nil {
		return "", nil, fmt.Errorf("list sealed executions for %s: %w", projectID, err)
	}
	return s.digestFold(ctx, projectID, ids)
}

// digestFold digests each execution in fold order and returns the
// interval root with the per-execution digests. Shared by the hourly build
// and BackfillDigestHistory, so history is computed exactly one way.
//
// ComputeForChain, not Compute: an execution with no events gets an
// honest no-events digest rather than stalling the chain (2026-09-04).
// ListDigestEventsForExecution, not ListEventsForExecution: events that
// arrived after a digest was stored are outside the anchored record.
func (s *CheckpointScheduler) digestFold(
	ctx context.Context, projectID string, ids []string,
) (string, []store.ExecutionDigest, error) {
	if len(ids) == 0 {
		return "", nil, nil
	}
	out := make([]store.ExecutionDigest, 0, len(ids))
	roots := make([]string, 0, len(ids))
	for i, execID := range ids {
		evts, err := s.Store.ListDigestEventsForExecution(ctx, execID)
		if err != nil {
			return "", nil, fmt.Errorf("events for %s: %w", execID, err)
		}
		d, err := attest.ComputeForChain(execID, evts)
		if err != nil {
			return "", nil, fmt.Errorf("digest execution %s: %w", execID, err)
		}
		roots = append(roots, d.Root)
		out = append(out, store.ExecutionDigest{
			ExecutionID: execID, ProjectID: projectID, Position: i,
			Root: d.Root, LeafCount: d.LeafCount, Algorithm: d.Algorithm,
		})
	}
	root, err := attest.RootOverExecutionDigests(roots)
	if err != nil {
		return "", nil, fmt.Errorf("interval root for %s: %w", projectID, err)
	}
	return root, out, nil
}

// BackfillDigestHistory stores digests for leaves sealed before
// migration 063 (2026-11-16), wherever they can still be proven.
//
// For each such leaf it recomputes the project's digests from today's
// records, in the order the leaf was built. If they fold exactly to the
// anchored IntervalRoot they are stored, and that hour is protected from
// then on. If not (records purged by retention or changed since), the
// hour is labelled "records changed before digests were stored" and is
// never re-anchored or silently repaired (Robert's decision, 2026-11-16).
// Idempotent: a handled leaf is never visited again.
func (s *CheckpointScheduler) BackfillDigestHistory(ctx context.Context) (stored, gaps int, err error) {
	for {
		batch, err := s.Store.ListLeavesMissingDigests(ctx, 200)
		if err != nil {
			return stored, gaps, err
		}
		if len(batch) == 0 {
			return stored, gaps, nil
		}
		for _, l := range batch {
			ids, err := s.Store.ListSealedExecutionIDs(ctx, l.ProjectID, l.IntervalStart, l.IntervalEnd)
			if err != nil {
				return stored, gaps, fmt.Errorf("backfill checkpoint %d: %w", l.Seq, err)
			}
			root, digests, ferr := s.digestFold(ctx, l.ProjectID, ids)
			if ferr == nil && len(digests) == l.ExecutionCount && root == l.IntervalRoot {
				for i := range digests {
					digests[i].CheckpointSeq = l.Seq
				}
				if err := s.Store.StoreHistoricalDigests(ctx, l.Seq, l.ProjectID, digests); err != nil {
					return stored, gaps, err
				}
				stored++
				continue
			}
			if err := s.Store.RecordDigestHistoryGap(ctx, l.Seq, l.ProjectID, store.HistoryGapRecordsChanged, s.now()); err != nil {
				return stored, gaps, err
			}
			gaps++
		}
	}
}

// previousTreeSize reports how large the log's tree was when checkpoint
// seq-1 was anchored, so this anchor can ask the log to prove it only
// grew since.
//
// Returns 0, meaning "do not ask", for the first checkpoint and for any
// predecessor whose anchor predates the proof material being captured.
// NEVER returns an error: a missing previous size must not stall the
// chain. The chain stalling is reserved for an anchor that did not
// happen, and this is a strictly additional question about one that
// did. A zero here costs one unproven link in the report, which the
// verifier names; an error here would cost the interval.
func (s *CheckpointScheduler) previousTreeSize(ctx context.Context, seq uint64) int64 {
	if seq <= 1 {
		return 0
	}
	prev, err := s.Store.GetCheckpointAnchor(ctx, seq-1)
	if err != nil || prev.AnchorProofJSON == "" {
		return 0
	}
	// TreeSize carries NO json tag, deliberately, and for the same
	// reason wireInclusionProof in the standalone verifier's offline.go
	// (github.com/mesedi-ai/mesedi-verify) carries
	// none: Verdifax's inclusion proof struct has no tags either, so on
	// the wire its keys are whatever Go's default marshalling produces,
	// which is PascalCase. Leaving the tag off lets encoding/json fall
	// back to case-insensitive matching, so this keeps working if that
	// struct ever gains explicit lower-case tags.
	//
	// A pinned tag of "TreeSize" would also work today and is what this
	// had first, but it is not snake_case and the audit is right to
	// reject it: it would read as a field we designed rather than a
	// field we are reading from somebody else's serialiser.
	var env struct {
		InclusionProof struct {
			TreeSize int64
		} `json:"inclusion_proof"`
	}
	if err := json.Unmarshal([]byte(prev.AnchorProofJSON), &env); err != nil {
		s.Logger.Warn("checkpoint_scheduler: previous anchor proof would not parse, so "+
			"this checkpoint cannot ask the log to prove it only grew",
			"seq", seq, "previous_seq", seq-1, "error", err.Error())
		return 0
	}
	return env.InclusionProof.TreeSize
}

// anchor submits a checkpoint and records where it landed.
func (s *CheckpointScheduler) anchor(ctx context.Context, cp attest.Checkpoint) error {
	if s.Anchorer == nil {
		s.Logger.Warn("checkpoint_scheduler: no anchorer configured, chain will stall",
			"seq", cp.Seq,
			"detail", "checkpoints are being built and stored but not anchored; "+
				"the next interval cannot be built until this one reaches a log")
		return fmt.Errorf("no anchorer configured")
	}
	anchor, err := s.Anchorer.AnchorCheckpoint(ctx, cp, s.previousTreeSize(ctx, cp.Seq))
	if err != nil {
		s.Logger.Warn("checkpoint_scheduler: anchor failed, chain stalled",
			"seq", cp.Seq, "error", err.Error())
		return err
	}
	if anchor.LogEntryID == "" {
		s.Logger.Warn("checkpoint_scheduler: anchorer returned no log entry id",
			"seq", cp.Seq)
		return fmt.Errorf("anchorer returned an empty log entry id for checkpoint %d", cp.Seq)
	}
	// Deliberately NOT rejected here when the preimage is empty. The
	// anchorer is the party that talked to the log and is the only one
	// positioned to judge its receipt; duplicating that judgement here
	// would mean two places to keep in step, and an anchorer that
	// legitimately cannot supply a preimage would stall the chain
	// forever rather than record a weaker but real anchor. The export
	// reports an empty preimage as unverifiable, which is where a
	// reader needs to see it.
	entryID, backend := anchor.LogEntryID, anchor.LedgerBackend
	if err := s.Store.MarkCheckpointAnchored(ctx, cp.Seq, anchor, s.now()); err != nil {
		// Anchored in the log but not recorded here. The next pass sees
		// it as unanchored and submits again, producing a second entry.
		// Wasteful, not incorrect: two entries for one checkpoint is
		// visible in the log, whereas recording an entry we failed to
		// write would be a claim with nothing behind it.
		s.Logger.Error("checkpoint_scheduler: anchored but not recorded",
			"seq", cp.Seq, "log_entry_id", entryID, "error", err.Error())
		return err
	}
	s.Logger.Info("checkpoint_scheduler: anchored",
		"seq", cp.Seq, "log_entry_id", entryID, "ledger_backend", backend)
	return nil
}
