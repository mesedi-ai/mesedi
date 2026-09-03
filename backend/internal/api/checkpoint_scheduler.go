package api

// CheckpointScheduler closes one hourly interval at a time, builds the
// checkpoint over it, and anchors it in a public transparency log.
//
// THE ONE FAILURE THIS FILE EXISTS TO PREVENT
//
// A missing checkpoint. Everything else in the chain — the hashes, the
// per-tenant sub-chains, the cumulative counts — exists so that a
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
// NOT WIRED INTO main YET, deliberately. Once the Verdifax caller is
// injected this worker spends a real transparency-log submission every
// interval, and a scheduler that starts spending the moment it merges
// is not a decision anyone made.

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"sync"
	"time"

	"mesedi/backend/internal/attest"
	"mesedi/backend/internal/store"
)

// CheckpointAnchorer submits a checkpoint hash to a transparency log
// and returns where it landed.
//
// An interface rather than a direct Verdifax call for two reasons. The
// caller does not exist yet, and this worker must be testable without
// spending money — every accepted submission in rekor mode is a real
// Sigstore entry with a real cost.
type CheckpointAnchorer interface {
	// AnchorCheckpoint submits cp.Hash and returns the log entry id and
	// backend name. An error means NOT anchored; the caller must not
	// treat a partial result as success.
	AnchorCheckpoint(ctx context.Context, cp attest.Checkpoint) (logEntryID, ledgerBackend string, err error)
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
	// but never anchored, so the chain stalls after the first one —
	// visibly, at a known sequence. That is the correct behaviour for a
	// deployment with no transparency log configured: it must not
	// silently produce an unanchored chain that looks complete.
	Anchorer CheckpointAnchorer

	// TickInterval governs how often the worker wakes. Default 5
	// minutes when zero — more often than the checkpoint interval so a
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

	leaves, err := s.buildLeaves(ctx, start, end)
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

	if err := s.Store.InsertCheckpoint(ctx, cp, leaves); err != nil {
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
	// the "anchor late, never abandon" rule — a late checkpoint is
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
		// NOT a backfill of history — a checkpoint claiming to cover a
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
) ([]attest.TenantLeaf, error) {
	counts, err := s.Store.CountSealedByProject(ctx, start, end)
	if err != nil {
		return nil, fmt.Errorf("count sealed by project: %w", err)
	}
	if len(counts) == 0 {
		// Empty interval. Returning nil, not an error: the checkpoint
		// still gets built, because an interval with no checkpoint is
		// indistinguishable from a suppressed one.
		return nil, nil
	}

	// SORTED, never map order. Go randomises map iteration, so ranging
	// over `counts` directly would order the leaves differently on every
	// run and produce a different root for identical data — and the
	// second run would look like tampering.
	projects := make([]string, 0, len(counts))
	for p := range counts {
		projects = append(projects, p)
	}
	sort.Strings(projects)

	leaves := make([]attest.TenantLeaf, 0, len(projects))
	for _, projectID := range projects {
		root, n, err := s.projectIntervalRoot(ctx, projectID, start, end)
		if err != nil {
			return nil, err
		}
		if n == 0 {
			// Counted but produced no executions: the two queries
			// disagree, which means the window shifted underneath us.
			// Refuse rather than anchor a leaf that does not describe
			// its own interval.
			return nil, fmt.Errorf(
				"project %s was counted in [%s, %s) but produced no executions; "+
					"refusing to anchor a leaf that contradicts its own count",
				projectID, start.Format(time.RFC3339), end.Format(time.RFC3339))
		}

		prevHash := attest.ZeroHash
		var prevCumulative uint64
		prev, err := s.Store.LatestTenantLeaf(ctx, projectID)
		if err != nil {
			return nil, fmt.Errorf("latest leaf for %s: %w", projectID, err)
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
	return leaves, nil
}

// projectIntervalRoot computes one project's Merkle root over the
// digests of its executions sealed in the interval.
//
// Order comes from the store, which sorts by sealed_at then
// execution_id. That ordering is part of the contract: the root depends
// on it, and a differently-ordered rebuild would not match.
func (s *CheckpointScheduler) projectIntervalRoot(
	ctx context.Context, projectID string, start, end time.Time,
) (string, int, error) {
	ids, err := s.Store.ListSealedExecutionIDs(ctx, projectID, start, end)
	if err != nil {
		return "", 0, fmt.Errorf("list sealed executions for %s: %w", projectID, err)
	}
	if len(ids) == 0 {
		return "", 0, nil
	}

	roots := make([]string, 0, len(ids))
	for _, execID := range ids {
		evts, err := s.Store.ListEventsForExecution(ctx, execID)
		if err != nil {
			return "", 0, fmt.Errorf("events for %s: %w", execID, err)
		}
		d, err := attest.Compute(execID, evts)
		if err != nil {
			// An execution with no events cannot be digested. Refuse the
			// whole interval rather than silently dropping it: a leaf
			// whose count includes an execution the root does not cover
			// is a lie the chain would then anchor.
			return "", 0, fmt.Errorf("digest execution %s: %w", execID, err)
		}
		roots = append(roots, d.Root)
	}
	root, err := attest.RootOverExecutionDigests(roots)
	if err != nil {
		return "", 0, fmt.Errorf("interval root for %s: %w", projectID, err)
	}
	return root, len(ids), nil
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
	entryID, backend, err := s.Anchorer.AnchorCheckpoint(ctx, cp)
	if err != nil {
		s.Logger.Warn("checkpoint_scheduler: anchor failed, chain stalled",
			"seq", cp.Seq, "error", err.Error())
		return err
	}
	if entryID == "" {
		s.Logger.Warn("checkpoint_scheduler: anchorer returned no log entry id",
			"seq", cp.Seq)
		return fmt.Errorf("anchorer returned an empty log entry id for checkpoint %d", cp.Seq)
	}
	if err := s.Store.MarkCheckpointAnchored(ctx, cp.Seq, entryID, backend, s.now()); err != nil {
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
