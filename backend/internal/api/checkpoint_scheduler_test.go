package api

// Tests for CheckpointScheduler.
//
// THE ONE THAT MATTERS is TestScheduler_DoesNotBuildOnAnUnanchoredHead.
// Everything else in the chain exists so that a suppressed interval is
// detectable; if this worker can be induced to skip an interval and
// carry on, it hands an attacker exactly the alibi the mechanism was
// built to deny. A stalled chain is visible and recoverable. A chain
// with a hole in it is indistinguishable from a cover-up.
//
// Following the convention set by the other scheduler tests here: drive
// tick() directly, never the goroutine. Testing time.Ticker scheduling
// adds flake without catching real bugs.

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"testing"
	"time"

	"mesedi/backend/internal/attest"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

// ── an in-memory chain that behaves like the real store ──────────────

type fakeChainStore struct {
	store.Store

	checkpoints []attest.Checkpoint
	leaves      map[uint64][]attest.TenantLeaf
	anchors     map[uint64]store.CheckpointAnchor

	// perInterval[projectID] = execution ids, keyed by interval start.
	perInterval map[string]map[string][]string

	sealCalls int
	insertErr error
	countErr  error
}

func newFakeChain() *fakeChainStore {
	return &fakeChainStore{
		leaves:      map[uint64][]attest.TenantLeaf{},
		anchors:     map[uint64]store.CheckpointAnchor{},
		perInterval: map[string]map[string][]string{},
	}
}

func key(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// activity registers executions for a project in the interval starting
// at `start`.
func (f *fakeChainStore) activity(start time.Time, projectID string, execIDs ...string) {
	k := key(start)
	if f.perInterval[k] == nil {
		f.perInterval[k] = map[string][]string{}
	}
	f.perInterval[k][projectID] = execIDs
}

func (f *fakeChainStore) SealExecutions(_ context.Context, _ time.Time, _, _ time.Duration) (int64, error) {
	f.sealCalls++
	return 0, nil
}

func (f *fakeChainStore) CountSealedByProject(_ context.Context, from, _ time.Time) (map[string]int, error) {
	if f.countErr != nil {
		return nil, f.countErr
	}
	out := map[string]int{}
	for p, ids := range f.perInterval[key(from)] {
		out[p] = len(ids)
	}
	return out, nil
}

func (f *fakeChainStore) ListSealedExecutionIDs(_ context.Context, projectID string, from, _ time.Time) ([]string, error) {
	return f.perInterval[key(from)][projectID], nil
}

func (f *fakeChainStore) ListEventsForExecution(_ context.Context, executionID string) ([]*events.Event, error) {
	// One event is enough for attest.Compute; the digest's contents do
	// not matter here, only that it is stable per execution id.
	return []*events.Event{{
		EventID:     executionID + "-e1",
		ExecutionID: executionID,
		EventType:   events.EventTypeLLMCall,
		Sequence:    1,
		Timestamp:   time.Date(2026, 9, 3, 12, 0, 0, 0, time.UTC),
		Payload:     []byte(`{"k":"v"}`),
	}}, nil
}

func (f *fakeChainStore) LatestCheckpoint(context.Context) (*attest.Checkpoint, error) {
	if len(f.checkpoints) == 0 {
		return nil, nil
	}
	cp := f.checkpoints[len(f.checkpoints)-1]
	return &cp, nil
}

func (f *fakeChainStore) GetCheckpointAnchor(_ context.Context, seq uint64) (store.CheckpointAnchor, error) {
	return f.anchors[seq], nil
}

func (f *fakeChainStore) LatestTenantLeaf(_ context.Context, projectID string) (*attest.TenantLeaf, error) {
	for i := len(f.checkpoints) - 1; i >= 0; i-- {
		for _, l := range f.leaves[f.checkpoints[i].Seq] {
			if l.ProjectID == projectID {
				leaf := l
				return &leaf, nil
			}
		}
	}
	return nil, nil
}

func (f *fakeChainStore) InsertCheckpoint(_ context.Context, cp attest.Checkpoint, leaves []attest.TenantLeaf) error {
	if f.insertErr != nil {
		return f.insertErr
	}
	for _, existing := range f.checkpoints {
		if existing.Seq == cp.Seq {
			return errors.New("duplicate checkpoint sequence")
		}
	}
	f.checkpoints = append(f.checkpoints, cp)
	f.leaves[cp.Seq] = leaves
	f.anchors[cp.Seq] = store.CheckpointAnchor{}
	return nil
}

func (f *fakeChainStore) MarkCheckpointAnchored(
	_ context.Context, seq uint64, in store.CheckpointAnchor, at time.Time,
) error {
	a := f.anchors[seq]
	if a.Anchored {
		return errors.New("already anchored")
	}
	f.anchors[seq] = store.CheckpointAnchor{
		Anchored:      true,
		LogEntryID:    in.LogEntryID,
		LedgerBackend: in.LedgerBackend,
		LeafPreimage:  in.LeafPreimage,
		AnchoredAt:    at,
	}
	return nil
}

// ── a controllable anchorer ──────────────────────────────────────────

type fakeAnchorer struct {
	calls  int
	fail   error
	nextID int

	// prevSizes records the previousTreeSize argument of every call, so
	// a test can assert the scheduler actually looked up the previous
	// anchor rather than passing a placeholder. A hardcoded zero here
	// would compile, pass, and silently mean the log is never asked to
	// prove it only grew.
	prevSizes []int64
}

func (a *fakeAnchorer) AnchorCheckpoint(
	_ context.Context, cp attest.Checkpoint, previousTreeSize int64,
) (store.CheckpointAnchor, error) {
	a.calls++
	a.prevSizes = append(a.prevSizes, previousTreeSize)
	if a.fail != nil {
		return store.CheckpointAnchor{}, a.fail
	}
	a.nextID++
	return store.CheckpointAnchor{
		Anchored:      true,
		LogEntryID:    fmt.Sprintf("rekor-%d", a.nextID),
		LedgerBackend: "mock",
		// Shaped like a real preimage and containing this checkpoint's
		// hash, so the scheduler tests exercise the value that now has to
		// survive the trip into the store rather than a placeholder.
		LeafPreimage: "verdifax.ledger.input.v2.env." + cp.Hash + ".bind.1",
	}, nil
}

func newCPScheduler(st store.Store, an CheckpointAnchorer, now time.Time) *CheckpointScheduler {
	return &CheckpointScheduler{
		Store:    st,
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Anchorer: an,
		Now:      func() time.Time { return now },
	}
}

func hourAt(n int) time.Time { return time.Date(2026, 9, 3, n, 0, 0, 0, time.UTC) }

// ── the security property ────────────────────────────────────────────

// An unanchored head must BLOCK the next interval. Building past it
// would leave a checkpoint that names a log entry which does not exist,
// and the alternative failure, skipping the interval, is the hole
// this whole mechanism exists to make impossible.
func TestScheduler_DoesNotBuildOnAnUnanchoredHead(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	f.activity(hourAt(12), "proj-a", "x1")
	f.activity(hourAt(13), "proj-a", "x2")

	anchorer := &fakeAnchorer{fail: errors.New("rekor unreachable")}
	s := newCPScheduler(f, anchorer, hourAt(14))

	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	// Genesis got built and stored, but anchoring failed.
	if len(f.checkpoints) != 1 {
		t.Fatalf("expected exactly 1 checkpoint, got %d", len(f.checkpoints))
	}
	if f.anchors[1].Anchored {
		t.Fatal("test setup wrong: the anchor was supposed to fail")
	}

	// A second tick, with a whole further interval elapsed, must NOT
	// build checkpoint 2.
	s.Now = func() time.Time { return hourAt(15) }
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	if len(f.checkpoints) != 1 {
		t.Fatalf("built %d checkpoints on top of an unanchored head. Checkpoint 2 "+
			"would name a log entry that does not exist, and the chain would be "+
			"unverifiable from the log alone", len(f.checkpoints))
	}
	if anchorer.calls < 2 {
		t.Errorf("the scheduler stopped retrying the anchor (%d calls); a stall must "+
			"keep trying or the chain never resumes", anchorer.calls)
	}
}

func TestScheduler_ResumesAfterAnchorRecovers(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	f.activity(hourAt(12), "proj-a", "x1")
	f.activity(hourAt(13), "proj-a", "x2")

	anchorer := &fakeAnchorer{fail: errors.New("down")}
	s := newCPScheduler(f, anchorer, hourAt(14))
	_ = s.tick(context.Background())

	// Log comes back.
	anchorer.fail = nil
	s.Now = func() time.Time { return hourAt(15) }
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("tick after recovery: %v", err)
	}

	if len(f.checkpoints) < 2 {
		t.Fatalf("chain did not resume after the anchor recovered: %d checkpoints",
			len(f.checkpoints))
	}
	// And the resumed chain must be intact, not merely longer.
	if err := attest.VerifyChain(f.checkpoints, CheckpointInterval); err != nil {
		t.Errorf("the chain built across an outage does not verify: %v", err)
	}
}

// A nil anchorer must stall at genesis rather than quietly producing an
// unanchored chain that looks complete.
func TestScheduler_NilAnchorerStallsRatherThanSkips(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	f.activity(hourAt(12), "proj-a", "x1")
	f.activity(hourAt(13), "proj-a", "x2")

	s := newCPScheduler(f, nil, hourAt(15))
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(f.checkpoints) != 1 {
		t.Errorf("with no anchorer configured the chain must stall at 1 checkpoint, "+
			"got %d, an unanchored chain that keeps growing looks complete and is not",
			len(f.checkpoints))
	}
}

// ── the heartbeat ────────────────────────────────────────────────────

// An interval with no activity still produces a checkpoint. Without it,
// "no checkpoint for 13:00" is ambiguous between no traffic and
// suppression, and that ambiguity is the vulnerability.
func TestScheduler_EmptyIntervalStillProducesACheckpoint(t *testing.T) {
	t.Parallel()
	f := newFakeChain() // no activity registered at all
	s := newCPScheduler(f, &fakeAnchorer{}, hourAt(14))

	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(f.checkpoints) != 1 {
		t.Fatalf("a quiet interval produced %d checkpoints, want 1", len(f.checkpoints))
	}
	cp := f.checkpoints[0]
	if cp.TenantLeafCount != 0 {
		t.Errorf("leaf count = %d, want 0", cp.TenantLeafCount)
	}
	if cp.MerkleRoot != "" {
		t.Errorf("empty interval root = %q, want the empty-string sentinel; the "+
			"empty-TREE root is a real digest and would be indistinguishable from "+
			"a root over data that went missing", cp.MerkleRoot)
	}
}

// ── determinism ──────────────────────────────────────────────────────

// Leaf order must not come from map iteration, which Go randomises.
// Two runs over identical data must produce the identical root, or the
// second one looks like tampering.
func TestScheduler_LeafOrderIsDeterministic(t *testing.T) {
	t.Parallel()
	roots := make([]string, 0, 8)
	for run := 0; run < 8; run++ {
		f := newFakeChain()
		// Several projects, deliberately not inserted in sorted order.
		f.activity(hourAt(12), "zeta", "z1", "z2")
		f.activity(hourAt(12), "alpha", "a1")
		f.activity(hourAt(12), "mid", "m1", "m2", "m3")
		f.activity(hourAt(12), "beta", "b1")

		s := newCPScheduler(f, &fakeAnchorer{}, hourAt(14))
		if err := s.tick(context.Background()); err != nil {
			t.Fatalf("run %d tick: %v", run, err)
		}
		if len(f.checkpoints) == 0 {
			t.Fatalf("run %d produced no checkpoint", run)
		}
		roots = append(roots, f.checkpoints[0].MerkleRoot)
	}
	for i, r := range roots {
		if r != roots[0] {
			t.Fatalf("run %d produced root %s but run 0 produced %s, leaf order is "+
				"coming from map iteration, so identical data hashes differently "+
				"between runs and the second one reads as tampering", i, r, roots[0])
		}
	}
}

// ── timing ───────────────────────────────────────────────────────────

// An interval still in progress must not be closed: its window is still
// receiving seals, and a root anchored now would change afterwards.
func TestScheduler_DoesNotCloseAnIntervalStillRunning(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	f.activity(hourAt(12), "proj-a", "x1")
	s := newCPScheduler(f, &fakeAnchorer{}, hourAt(14))

	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("first tick: %v", err)
	}
	before := len(f.checkpoints)

	// Same hour, 30 minutes later. The 13:00-14:00 interval has closed
	// but 14:00-15:00 has not.
	s.Now = func() time.Time { return hourAt(14).Add(30 * time.Minute) }
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("second tick: %v", err)
	}
	for _, cp := range f.checkpoints {
		if cp.IntervalEnd.After(hourAt(14).Add(30 * time.Minute)) {
			t.Errorf("closed interval [%s, %s) which has not finished yet",
				cp.IntervalStart.Format(time.RFC3339), cp.IntervalEnd.Format(time.RFC3339))
		}
	}
	if len(f.checkpoints) < before {
		t.Error("checkpoints disappeared")
	}
}

// After downtime the scheduler must drain the backlog IN ORDER with no
// gaps, because a gap is the thing being defended against.
func TestScheduler_CatchUpClosesIntervalsInOrderWithNoGaps(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	for h := 12; h <= 17; h++ {
		f.activity(hourAt(h), "proj-a", "x")
	}

	// Establish genesis FIRST. Catch-up only means anything once a chain
	// head exists: a first-ever run does not reach back and backfill,
	// because a checkpoint covering a period before the mechanism
	// existed is a claim nobody can check. That is decision 3 in the
	// design doc, and it is why this test seeds a head rather than
	// expecting one tick at 18:00 to close six hours.
	s := newCPScheduler(f, &fakeAnchorer{}, hourAt(13))
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("genesis tick: %v", err)
	}
	if len(f.checkpoints) != 1 {
		t.Fatalf("genesis produced %d checkpoints, want exactly 1", len(f.checkpoints))
	}

	// Now five hours of downtime, then a single tick.
	s.Now = func() time.Time { return hourAt(18) }
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("catch-up tick: %v", err)
	}

	if len(f.checkpoints) != 6 {
		t.Fatalf("catch-up closed %d intervals in total, want 6 (12:00 genesis plus "+
			"13:00 through 17:00); a short count means intervals were skipped, "+
			"which is the hole this mechanism exists to prevent", len(f.checkpoints))
	}
	for i, cp := range f.checkpoints {
		if cp.Seq != uint64(i+1) {
			t.Fatalf("checkpoint at index %d has seq %d; sequences must be "+
				"consecutive from 1 or there is a gap", i, cp.Seq)
		}
		if i > 0 && !cp.IntervalStart.Equal(f.checkpoints[i-1].IntervalEnd) {
			t.Fatalf("interval %d starts at %s but %d ended at %s, the intervals "+
				"do not tile, which is a hole", cp.Seq,
				cp.IntervalStart.Format(time.RFC3339), f.checkpoints[i-1].Seq,
				f.checkpoints[i-1].IntervalEnd.Format(time.RFC3339))
		}
	}
	if err := attest.VerifyChain(f.checkpoints, CheckpointInterval); err != nil {
		t.Errorf("caught-up chain does not verify: %v", err)
	}
}

// Genesis covers the most recently COMPLETED interval and does not
// reach backwards, however much history is sitting in the database.
//
// This is design decision 3, and it is a refusal rather than a
// limitation: a checkpoint claiming to cover a period before the
// mechanism existed would be the single most misleading thing the
// chain could assert, because nobody, including Mesedi, could check
// it. The chain starts on a stated date and says so.
func TestScheduler_GenesisDoesNotBackfillHistory(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	// A full day of prior activity, all of it predating the chain.
	for h := 0; h <= 17; h++ {
		f.activity(hourAt(h), "proj-a", "x")
	}

	s := newCPScheduler(f, &fakeAnchorer{}, hourAt(18))
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}

	if len(f.checkpoints) != 1 {
		t.Fatalf("genesis produced %d checkpoints; it must cover exactly one "+
			"interval, not backfill history", len(f.checkpoints))
	}
	cp := f.checkpoints[0]
	if !cp.IntervalStart.Equal(hourAt(17)) || !cp.IntervalEnd.Equal(hourAt(18)) {
		t.Errorf("genesis covers [%s, %s); want the most recently completed "+
			"interval [%s, %s)",
			cp.IntervalStart.Format(time.RFC3339), cp.IntervalEnd.Format(time.RFC3339),
			hourAt(17).Format(time.RFC3339), hourAt(18).Format(time.RFC3339))
	}
}

// Catch-up is bounded so a long outage cannot turn one tick into an
// unbounded loop. The remaining intervals are closed by later ticks;
// nothing is skipped.
func TestScheduler_CatchUpIsBounded(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	s := newCPScheduler(f, &fakeAnchorer{}, hourAt(12).Add(200*time.Hour))
	if err := s.tick(context.Background()); err != nil {
		t.Fatalf("tick: %v", err)
	}
	if len(f.checkpoints) > maxIntervalsPerTick {
		t.Errorf("one tick closed %d intervals, above the bound of %d",
			len(f.checkpoints), maxIntervalsPerTick)
	}
	if len(f.checkpoints) == 0 {
		t.Error("a large backlog closed nothing at all")
	}
}

// ── failure propagation ──────────────────────────────────────────────

func TestScheduler_StoreErrorsSurface(t *testing.T) {
	t.Parallel()
	f := newFakeChain()
	f.activity(hourAt(12), "proj-a", "x1")
	f.countErr = errors.New("db down")

	s := newCPScheduler(f, &fakeAnchorer{}, hourAt(14))
	if err := s.tick(context.Background()); err == nil {
		t.Error("a store failure while counting must surface, not be swallowed " +
			"into a silently empty interval")
	}
	if len(f.checkpoints) != 0 {
		t.Error("a checkpoint was written despite the interval's contents being unknown")
	}
}
