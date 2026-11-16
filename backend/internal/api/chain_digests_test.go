package api

// Stored execution digests (migration 063, 2026-11-16), end to end on a
// real SQLite store, the real scheduler and the real export.
//
// Until then exports recomputed every digest from current records, so one
// late event (outside review finding E3) or a retention purge (E4) made an
// honest export report tampering. Robert's decisions: store the anchored
// digest; keep late events but flag them; keep retention, keep the digest;
// backfill history only where it still folds; label the rest.

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

type digestTestAnchorer struct{ n int }

func (a *digestTestAnchorer) AnchorCheckpoint(_ context.Context, _ attest.Checkpoint, _ int64) (store.CheckpointAnchor, error) {
	a.n++
	return store.CheckpointAnchor{Anchored: true, LogEntryID: "8000" + string(rune('0'+a.n)), LedgerBackend: "mock"}, nil
}

// preDigestStore drops the digests, as the code before migration 063 did,
// to build history the backfill must handle.
type preDigestStore struct{ *store.SQLiteStore }

func (p preDigestStore) InsertCheckpointWithDigests(ctx context.Context, cp attest.Checkpoint, leaves []attest.TenantLeaf, _ map[string][]store.ExecutionDigest) error {
	return p.InsertCheckpoint(ctx, cp, leaves)
}

type chainFixture struct {
	t   *testing.T
	ctx context.Context
	st  *store.SQLiteStore
	h   *Handlers
	sch *CheckpointScheduler
	t0  time.Time
	now time.Time
}

func newChainFixture(t *testing.T, preDigest bool) *chainFixture {
	t.Helper()
	ctx := context.Background()
	lg := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite("file:"+filepath.Join(t.TempDir(), "chain.db"), lg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.CreateProject(ctx, &store.Project{ProjectID: "proj", Name: "p", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	f := &chainFixture{t: t, ctx: ctx, st: st, h: &Handlers{Store: st, Logger: lg, HaltSubs: NewHaltSubscribers()},
		t0: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)}
	var s store.Store = st
	if preDigest {
		s = preDigestStore{st}
	}
	f.sch = &CheckpointScheduler{Store: s, Logger: lg, Anchorer: &digestTestAnchorer{}, Now: func() time.Time { return f.now }}
	t.Cleanup(func() { f.h.DrainDispatches() })
	return f
}

func (f *chainFixture) ev(exec, id string, seq int) events.Event {
	return events.Event{EventID: id, ExecutionID: exec, EventType: events.EventTypeLLMCall, Sequence: seq,
		Timestamp: f.t0.Add(time.Duration(seq) * time.Second), Payload: []byte(`{}`)}
}

// runOne creates a finished execution with one event in the 10:00 hour and
// ticks the scheduler until that hour is sealed, built and anchored.
func (f *chainFixture) runOne(exec string) {
	f.t.Helper()
	end := f.t0.Add(time.Minute)
	if err := f.st.CreateExecution(f.ctx, &events.Execution{ExecutionID: exec, ProjectID: "proj", Status: "completed", StartedAt: f.t0, EndedAt: &end}); err != nil {
		f.t.Fatal(err)
	}
	if err := f.st.SaveEvents(f.ctx, []events.Event{f.ev(exec, exec+"-e1", 1)}); err != nil {
		f.t.Fatal(err)
	}
	f.now = f.t0.Add(30 * time.Minute)
	if err := f.sch.tick(f.ctx); err != nil {
		f.t.Fatal(err)
	}
	f.now = f.t0.Add(2*time.Hour + 5*time.Minute)
	if err := f.sch.tick(f.ctx); err != nil {
		f.t.Fatal(err)
	}
}

func (f *chainFixture) export() (attest.ChainExport, attest.ExportVerification) {
	f.t.Helper()
	head, err := f.st.LatestCheckpoint(f.ctx)
	if err != nil || head == nil {
		f.t.Fatalf("no checkpoint: %v", err)
	}
	exp, err := f.h.buildChainExport(f.ctx, "proj", 1, head.Seq)
	if err != nil {
		f.t.Fatal(err)
	}
	return exp, attest.VerifyChainExport(exp)
}

// leafSeq returns the checkpoint holding the project's leaf.
func (f *chainFixture) leafSeq() uint64 {
	f.t.Helper()
	exp, _ := f.export()
	for _, iv := range exp.Intervals {
		if iv.Leaf != nil {
			return iv.Checkpoint.Seq
		}
	}
	f.t.Fatal("no checkpoint holds the project's leaf")
	return 0
}

func failures(v attest.ExportVerification) string {
	var out []string
	for _, c := range v.Checks {
		if !c.OK {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return strings.Join(out, "; ")
}

func TestStoredDigestsSurviveALateEvent(t *testing.T) {
	f := newChainFixture(t, false)
	f.runOne("exec-1")
	if _, v := f.export(); !v.OK {
		t.Fatalf("export failed before the late event: %s", failures(v))
	}

	if err := f.st.SaveEvents(f.ctx, []events.Event{f.ev("exec-1", "late", 2)}); err != nil {
		t.Fatalf("a late event was refused; it must be kept: %v", err)
	}
	exp, v := f.export()
	if !v.OK {
		t.Fatalf("one late event made an honest export fail: %s", failures(v))
	}
	if exp.Format != attest.ChainExportFormatV2 {
		t.Errorf("export format %q, want %q", exp.Format, attest.ChainExportFormatV2)
	}
	if late, _ := f.st.CountEventsAfterDigest(f.ctx, []string{"exec-1", "exec-unknown"}); late["exec-1"] != 1 || len(late) != 1 {
		t.Errorf("late events counted %v, want exec-1: 1 and nothing else", late)
	}
	all, _ := f.st.ListEventsForExecution(f.ctx, "exec-1")
	anchored, _ := f.st.ListDigestEventsForExecution(f.ctx, "exec-1")
	if len(all) != 2 || len(anchored) != 1 {
		t.Errorf("all events %d (want 2, the late one kept), anchored events %d (want 1)", len(all), len(anchored))
	}
}

func TestLateEventRaisesARecordIntegritySignal(t *testing.T) {
	f := newChainFixture(t, false)
	f.runOne("exec-1")
	late := []events.Event{f.ev("exec-1", "late", 2)}
	if err := f.st.SaveEvents(f.ctx, late); err != nil {
		t.Fatal(err)
	}
	f.h.signalEventsAfterDigest(httptest.NewRequest("POST", "/events", nil), "proj", late)
	groups, err := f.st.ListFailureGroups(f.ctx, "proj", store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range groups {
		if g.FailureClass == store.FailureClassRecordIntegrity && g.Signature == RecordIntegritySigAfterDigest {
			return
		}
	}
	t.Fatalf("no %s group after an event arrived for an anchored execution", RecordIntegritySigAfterDigest)
}

func TestRetentionKeepsTheChainVerifiable(t *testing.T) {
	f := newChainFixture(t, false)
	f.runOne("exec-1")
	if n, err := f.st.DeleteExecutionsOlderThan(f.ctx, "proj", f.t0.Add(time.Hour)); err != nil || n != 1 {
		t.Fatalf("retention deleted %d executions, err %v", n, err)
	}
	exp, v := f.export()
	if !v.OK {
		t.Fatalf("retention made an honest export fail: %s", failures(v))
	}
	var deleted bool
	for _, iv := range exp.Intervals {
		for _, x := range iv.Executions {
			deleted = deleted || x.RecordsDeleted
		}
	}
	if !deleted {
		t.Error("the purged execution is not marked records_deleted")
	}
	if !strings.Contains(strings.Join(v.Unverified, " "), "deleted under retention") {
		t.Errorf("the report does not say records were deleted under retention: %v", v.Unverified)
	}
}

func TestBackfillStoresHistoryThatStillFolds(t *testing.T) {
	f := newChainFixture(t, true)
	f.runOne("exec-1")
	seq := f.leafSeq()
	if d, _ := f.st.ListExecutionDigests(f.ctx, "proj", seq); len(d) != 0 {
		t.Fatal("the pre-063 fixture stored digests; it must not")
	}
	sch := &CheckpointScheduler{Store: f.st, Logger: f.sch.Logger, Now: func() time.Time { return f.now }}
	stored, gaps, err := sch.BackfillDigestHistory(f.ctx)
	if err != nil || stored != 1 || gaps != 0 {
		t.Fatalf("backfill stored %d, labelled %d, err %v; want 1, 0", stored, gaps, err)
	}
	if d, _ := f.st.ListExecutionDigests(f.ctx, "proj", seq); len(d) != 1 {
		t.Fatalf("backfill left %d stored digests for checkpoint %d, want 1", len(d), seq)
	}
	// Now protected: a late event no longer breaks the export.
	if err := f.st.SaveEvents(f.ctx, []events.Event{f.ev("exec-1", "late", 2)}); err != nil {
		t.Fatal(err)
	}
	if _, v := f.export(); !v.OK {
		t.Fatalf("backfilled history failed after a late event: %s", failures(v))
	}
	if s2, g2, _ := sch.BackfillDigestHistory(f.ctx); s2+g2 != 0 {
		t.Errorf("backfill is not idempotent: second run stored %d, labelled %d", s2, g2)
	}
}

func TestBackfillLabelsHistoryThatChanged(t *testing.T) {
	f := newChainFixture(t, true)
	f.runOne("exec-1")
	// Records purged before digests were stored: the hour cannot be proven.
	if _, err := f.st.DeleteExecutionsOlderThan(f.ctx, "proj", f.t0.Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	sch := &CheckpointScheduler{Store: f.st, Logger: f.sch.Logger, Now: func() time.Time { return f.now }}
	stored, gaps, err := sch.BackfillDigestHistory(f.ctx)
	if err != nil || stored != 0 || gaps != 1 {
		t.Fatalf("backfill stored %d, labelled %d, err %v; want 0, 1", stored, gaps, err)
	}
	exp, v := f.export()
	if !v.OK {
		t.Fatalf("an unprovable hour was reported as a failure; it must be unverified: %s", failures(v))
	}
	var labelled bool
	for _, iv := range exp.Intervals {
		labelled = labelled || iv.DigestsUnavailable == store.HistoryGapRecordsChanged
	}
	if !labelled {
		t.Error("the export does not label the hour whose history changed")
	}
	if !strings.Contains(strings.Join(v.Unverified, " "), "before Mesedi stored each run's fingerprint") {
		t.Errorf("the report does not explain the unprovable hour: %v", v.Unverified)
	}
}

func TestUnpublishedHeadIsLeftOutOfTheExport(t *testing.T) {
	f := newChainFixture(t, false)
	f.runOne("exec-1")
	// Stall anchoring and close one more hour: the head is now unpublished.
	f.sch.Anchorer = failingAnchorer{}
	f.now = f.t0.Add(3*time.Hour + 5*time.Minute)
	_ = f.sch.tick(f.ctx)
	head, _ := f.st.LatestCheckpoint(f.ctx)
	anchor, _ := f.st.GetCheckpointAnchor(f.ctx, head.Seq)
	if anchor.Anchored {
		t.Fatal("fixture: the head should be unpublished")
	}
	exp, v := f.export()
	if !v.OK {
		t.Fatalf("a pending anchor made the export fail: %s", failures(v))
	}
	if last := exp.Intervals[len(exp.Intervals)-1].Checkpoint.Seq; last == head.Seq {
		t.Errorf("the unpublished head %d is still in the export", head.Seq)
	}
}

type failingAnchorer struct{}

func (failingAnchorer) AnchorCheckpoint(context.Context, attest.Checkpoint, int64) (store.CheckpointAnchor, error) {
	return store.CheckpointAnchor{}, errAnchorUnavailable
}

var errAnchorUnavailable = errFake("anchor unavailable")

type errFake string

func (e errFake) Error() string { return string(e) }
