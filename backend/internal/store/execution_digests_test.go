package store

// Stored execution digests (migration 063), on SQLite and, when
// MESEDI_TEST_POSTGRES_DSN is set, on Postgres. The stored value must be
// impossible to make disagree with what was anchored.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// digestChain writes one checkpoint with one leaf for a fresh project and
// returns everything needed to store digests against it.
func digestChain(t *testing.T, c cascadeTarget) (attest.Checkpoint, attest.TenantLeaf, []ExecutionDigest) {
	t.Helper()
	ctx := context.Background()
	suffix := fmt.Sprintf("%d", time.Now().UnixNano())
	project := "proj-dig-" + suffix
	if err := c.st.CreateProject(ctx, &Project{ProjectID: project, Name: project}); err != nil {
		t.Fatal(err)
	}
	head, err := c.st.LatestCheckpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var prevLog string
	if head != nil {
		a, _ := c.st.GetCheckpointAnchor(ctx, head.Seq)
		if !a.Anchored {
			if err := c.st.MarkCheckpointAnchored(ctx, head.Seq, CheckpointAnchor{LogEntryID: "9" + suffix, LedgerBackend: "mock"}, time.Now()); err != nil {
				t.Fatal(err)
			}
		}
		a, _ = c.st.GetCheckpointAnchor(ctx, head.Seq)
		prevLog = a.LogEntryID
	}
	roots := []string{}
	var digests []ExecutionDigest
	for i := 0; i < 3; i++ {
		d, err := attest.Compute(fmt.Sprintf("exec-%s-%d", suffix, i), []*events.Event{{
			EventID: fmt.Sprintf("e-%s-%d", suffix, i), ExecutionID: fmt.Sprintf("exec-%s-%d", suffix, i),
			EventType: events.EventTypeLLMCall, Sequence: 1, Timestamp: time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC), Payload: []byte(`{}`),
		}})
		if err != nil {
			t.Fatal(err)
		}
		roots = append(roots, d.Root)
		digests = append(digests, ExecutionDigest{ExecutionID: d.ExecutionID, ProjectID: project, Position: i, Root: d.Root, LeafCount: d.LeafCount, Algorithm: d.Algorithm})
	}
	root, _ := attest.RootOverExecutionDigests(roots)
	leaf := attest.TenantLeaf{ProjectID: project, IntervalRoot: root, ExecutionCount: 3, CumulativeCount: 3, PrevLeafHash: attest.ZeroHash}
	var start time.Time
	if head != nil {
		start = head.IntervalEnd
	} else {
		start = time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
	}
	cp, err := attest.BuildCheckpoint(attest.CheckpointParams{Prev: head, PrevLogEntryID: prevLog, IntervalStart: start,
		IntervalEnd: start.Add(time.Hour), Interval: time.Hour, Leaves: []attest.TenantLeaf{leaf}, Now: start.Add(2 * time.Hour)})
	if err != nil {
		t.Fatal(err)
	}
	for i := range digests {
		digests[i].CheckpointSeq = cp.Seq
	}
	return cp, leaf, digests
}

func TestInsertCheckpointWithDigestsRefusesAFoldThatDisagrees(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			cp, leaf, digests := digestChain(t, c)
			bad := append([]ExecutionDigest(nil), digests...)
			bad[0].Root, bad[1].Root = bad[1].Root, bad[0].Root
			if err := c.st.InsertCheckpointWithDigests(ctx, cp, []attest.TenantLeaf{leaf}, map[string][]ExecutionDigest{leaf.ProjectID: bad}); err == nil {
				t.Fatal("digests in the wrong order were stored; they do not fold to the anchored root")
			}
			if head, _ := c.st.LatestCheckpoint(ctx); head != nil && head.Seq == cp.Seq {
				t.Fatal("the checkpoint was written although its digests were refused")
			}
			if err := c.st.InsertCheckpointWithDigests(ctx, cp, []attest.TenantLeaf{leaf}, map[string][]ExecutionDigest{leaf.ProjectID: digests}); err != nil {
				t.Fatalf("correct digests refused: %v", err)
			}
			got, err := c.st.ListExecutionDigests(ctx, leaf.ProjectID, cp.Seq)
			if err != nil || len(got) != 3 || got[2].Root != digests[2].Root {
				t.Fatalf("stored digests read back as %+v, err %v", got, err)
			}
		})
	}
}

func TestStoreHistoricalDigestsRefusesAFoldThatDisagrees(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			cp, leaf, digests := digestChain(t, c)
			if err := c.st.InsertCheckpoint(ctx, cp, []attest.TenantLeaf{leaf}); err != nil {
				t.Fatal(err)
			}
			missing, err := c.st.ListLeavesMissingDigests(ctx, 1000)
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, m := range missing {
				found = found || (m.Seq == cp.Seq && m.ProjectID == leaf.ProjectID && m.IntervalRoot == leaf.IntervalRoot)
			}
			if !found {
				t.Fatal("a leaf without digests is not listed for backfill")
			}
			if err := c.st.StoreHistoricalDigests(ctx, cp.Seq, leaf.ProjectID, digests[:2]); err == nil {
				t.Fatal("two digests were stored for a leaf that counts three")
			}
			if err := c.st.StoreHistoricalDigests(ctx, cp.Seq, leaf.ProjectID, digests); err != nil {
				t.Fatalf("matching history refused: %v", err)
			}
			if err := c.st.RecordDigestHistoryGap(ctx, cp.Seq, "other-project", HistoryGapRecordsChanged, time.Now()); err != nil {
				t.Fatal(err)
			}
			if err := c.st.RecordDigestHistoryGap(ctx, cp.Seq, "other-project", HistoryGapRecordsChanged, time.Now()); err != nil {
				t.Fatalf("recording a gap twice must be harmless: %v", err)
			}
			if reason, ok, _ := c.st.GetDigestHistoryGap(ctx, cp.Seq, "other-project"); !ok || reason != HistoryGapRecordsChanged {
				t.Fatalf("gap read back as %q, %v", reason, ok)
			}
		})
	}
}

func TestSaveEventsTruncatesAndFlagsLateEvents(t *testing.T) {
	for _, c := range cascadeTargets(t) {
		t.Run(c.name, func(t *testing.T) {
			ctx := context.Background()
			cp, leaf, digests := digestChain(t, c)
			exec := digests[0].ExecutionID
			start := time.Date(2026, 9, 3, 10, 0, 0, 0, time.UTC)
			if err := c.st.CreateExecution(ctx, &events.Execution{ExecutionID: exec, ProjectID: leaf.ProjectID, Status: "completed", StartedAt: start}); err != nil {
				t.Fatal(err)
			}
			ns := time.Date(2026, 9, 3, 10, 0, 1, 123456789, time.UTC)
			if err := c.st.SaveEvents(ctx, []events.Event{{EventID: exec + "-a", ExecutionID: exec, EventType: events.EventTypeLLMCall, Sequence: 1, Timestamp: ns, Payload: []byte(`{}`)}}); err != nil {
				t.Fatal(err)
			}
			if err := c.st.InsertCheckpointWithDigests(ctx, cp, []attest.TenantLeaf{leaf}, map[string][]ExecutionDigest{leaf.ProjectID: digests}); err != nil {
				t.Fatal(err)
			}
			if err := c.st.SaveEvents(ctx, []events.Event{{EventID: exec + "-b", ExecutionID: exec, EventType: events.EventTypeLLMCall, Sequence: 2, Timestamp: ns, Payload: []byte(`{}`)}}); err != nil {
				t.Fatal(err)
			}
			all, _ := c.st.ListEventsForExecution(ctx, exec)
			anchored, _ := c.st.ListDigestEventsForExecution(ctx, exec)
			lateBy, _ := c.st.CountEventsAfterDigest(ctx, []string{exec, "exec-never-existed"})
			late := lateBy[exec]
			if len(all) != 2 || len(anchored) != 1 || late != 1 || len(lateBy) != 1 {
				t.Fatalf("all %d, anchored %d, late %v; want 2, 1, and only %s: 1", len(all), len(anchored), lateBy, exec)
			}
			if got := anchored[0].Timestamp.UTC(); !got.Equal(ns.Truncate(time.Microsecond)) {
				t.Errorf("stored timestamp %s, want %s (truncated to microseconds, E12)", got.Format(time.RFC3339Nano), ns.Truncate(time.Microsecond).Format(time.RFC3339Nano))
			}
			exists, _ := c.st.ExistingExecutionIDs(ctx, []string{exec, "exec-never-existed"})
			if !exists[exec] || exists["exec-never-existed"] {
				t.Errorf("ExistingExecutionIDs = %v", exists)
			}

			// SQLite allows 32,766 bound values per statement, so 40,000 ids
			// with one placeholder each would fail there. The ids travel as
			// one JSON parameter instead, so a long list is one query.
			many := make([]string, 40000)
			for i := range many {
				many[i] = fmt.Sprintf("exec-absent-%d", i)
			}
			many[39999] = exec
			exists, err := c.st.ExistingExecutionIDs(ctx, many)
			if err != nil || len(exists) != 1 || !exists[exec] {
				t.Errorf("40,000 ids: %v, err %v", exists, err)
			}
			lateMany, err := c.st.CountEventsAfterDigest(ctx, many)
			if err != nil || lateMany[exec] != 1 || len(lateMany) != 1 {
				t.Errorf("40,000 ids, late counts: %v, err %v", lateMany, err)
			}
			if none, err := c.st.ExistingExecutionIDs(ctx, nil); err != nil || len(none) != 0 {
				t.Errorf("no ids: %v, err %v", none, err)
			}
		})
	}
}
