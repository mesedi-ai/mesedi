package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/attest"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/store"
)

// The test that matters here is TestChainExportRoundTripsThroughItsOwnVerifier.
//
// An export that assembles without error but does not verify is worse
// than one that fails outright: it hands an auditor a document that
// accuses Mesedi of tampering it did not do. So the assembly is checked
// by the same function an auditor will run, not by asserting field by
// field that it looks about right.
//
// The fixtures build REAL digests from REAL events and derive the leaf
// roots from them, rather than pairing invented roots with invented
// counts. A fixture that could not verify would make every test below
// vacuous.

const (
	exportProjA = "proj-agency-a"
	exportProjB = "proj-other-tenant"
)

type exportStubStore struct {
	store.Store

	checkpoints []store.AnchoredCheckpoint
	leaves      map[uint64][]attest.TenantLeaf

	// sealed and eventsFor are keyed to model the two store reads
	// exportedExecutions makes.
	sealed    map[string][]string // projectID|intervalStart -> execution ids
	eventsFor map[string][]*events.Event

	rangeCalls int
}

func (s *exportStubStore) ListCheckpointRange(
	_ context.Context, fromSeq, toSeq uint64,
) ([]store.AnchoredCheckpoint, error) {
	s.rangeCalls++
	var out []store.AnchoredCheckpoint
	for _, c := range s.checkpoints {
		if c.Checkpoint.Seq >= fromSeq && c.Checkpoint.Seq <= toSeq {
			out = append(out, c)
		}
	}
	return out, nil
}

func (s *exportStubStore) ListCheckpointLeavesRange(
	_ context.Context, fromSeq, toSeq uint64,
) (map[uint64][]attest.TenantLeaf, error) {
	out := make(map[uint64][]attest.TenantLeaf)
	for seq, l := range s.leaves {
		if seq >= fromSeq && seq <= toSeq {
			out[seq] = l
		}
	}
	return out, nil
}

func (s *exportStubStore) ListSealedExecutionIDs(
	_ context.Context, projectID string, from, _ time.Time,
) ([]string, error) {
	return s.sealed[projectID+"|"+from.Format(time.RFC3339)], nil
}

func (s *exportStubStore) ListEventsForExecution(
	_ context.Context, executionID string,
) ([]*events.Event, error) {
	return s.eventsFor[executionID], nil
}

func exportEvents(execID string, n int, base time.Time) []*events.Event {
	out := make([]*events.Event, 0, n)
	for i := range n {
		out = append(out, &events.Event{
			EventID:     fmt.Sprintf("%s-evt-%03d", execID, i),
			ExecutionID: execID,
			EventType:   events.EventTypeCheckpoint,
			Sequence:    i + 1,
			Timestamp:   base.Add(time.Duration(i) * time.Second),
			Payload:     json.RawMessage(fmt.Sprintf(`{"i":%d}`, i)),
		})
	}
	return out
}

func exportHour(n int) time.Time {
	return time.Date(2026, 9, 4, 12+n, 0, 0, 0, time.UTC)
}

// newExportFixture builds a store holding a genuinely valid chain.
//
// perInterval gives project A's execution count per hour; project B is
// present in every interval so the interval tree has more than one leaf
// and the inclusion proofs are non-trivial.
func newExportFixture(t *testing.T, perInterval []int) *exportStubStore {
	t.Helper()

	st := &exportStubStore{
		leaves:    map[uint64][]attest.TenantLeaf{},
		sealed:    map[string][]string{},
		eventsFor: map[string][]*events.Event{},
	}

	var (
		prevCP                   *attest.Checkpoint
		prevA, prevB             *attest.TenantLeaf
		cumulativeA, cumulativeB uint64
		execN                    int
	)

	for i, nA := range perInterval {
		start, end := exportHour(i), exportHour(i+1)

		// Build each project's executions, events and interval root.
		mkLeaf := func(project string, count int, cumulative *uint64,
			prev *attest.TenantLeaf) *attest.TenantLeaf {
			if count == 0 {
				return nil
			}
			var (
				ids   []string
				roots []string
			)
			for range count {
				execN++
				id := fmt.Sprintf("exec-%04d", execN)
				evs := exportEvents(id, 3, start)
				st.eventsFor[id] = evs
				d, err := attest.Compute(id, evs)
				if err != nil {
					t.Fatalf("Compute %s: %v", id, err)
				}
				ids = append(ids, id)
				roots = append(roots, d.Root)
			}
			st.sealed[project+"|"+start.Format(time.RFC3339)] = ids

			root, err := attest.RootOverExecutionDigests(roots)
			if err != nil {
				t.Fatalf("RootOverExecutionDigests: %v", err)
			}
			prevHash := attest.ZeroHash
			if prev != nil {
				prevHash = attest.TenantLeafHash(*prev)
			}
			*cumulative += uint64(count)
			l := attest.TenantLeaf{
				ProjectID:       project,
				IntervalRoot:    root,
				ExecutionCount:  count,
				CumulativeCount: *cumulative,
				PrevLeafHash:    prevHash,
			}
			return &l
		}

		leafA := mkLeaf(exportProjA, nA, &cumulativeA, prevA)
		leafB := mkLeaf(exportProjB, 2, &cumulativeB, prevB)

		// Committed order is sorted by project id, which is what the
		// scheduler does and what the proofs are indexed against.
		var leaves []attest.TenantLeaf
		if leafA != nil {
			leaves = append(leaves, *leafA)
			prevA = leafA
		}
		if leafB != nil {
			leaves = append(leaves, *leafB)
			prevB = leafB
		}

		prevLogEntry := ""
		if prevCP != nil {
			prevLogEntry = fmt.Sprintf("100000%02d", i-1)
		}
		cp, err := attest.BuildCheckpoint(attest.CheckpointParams{
			Prev:           prevCP,
			PrevLogEntryID: prevLogEntry,
			IntervalStart:  start,
			IntervalEnd:    end,
			Interval:       time.Hour,
			Leaves:         leaves,
			Now:            end.Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("BuildCheckpoint %d: %v", i, err)
		}

		st.leaves[cp.Seq] = leaves
		st.checkpoints = append(st.checkpoints, store.AnchoredCheckpoint{
			Checkpoint: cp,
			Anchor: store.CheckpointAnchor{
				Anchored:      true,
				LogEntryID:    fmt.Sprintf("100000%02d", i),
				LedgerBackend: "rekor",
				AnchoredAt:    end.Add(2 * time.Minute),
			},
		})
		cpCopy := cp
		prevCP = &cpCopy
	}
	return st
}

func TestChainExportRoundTripsThroughItsOwnVerifier(t *testing.T) {
	st := newExportFixture(t, []int{3, 1, 4})
	h := &Handlers{Store: st, Logger: quietLogger()}

	export, err := h.buildChainExport(context.Background(), exportProjA, 1, 3)
	if err != nil {
		t.Fatalf("buildChainExport: %v", err)
	}

	v := attest.VerifyChainExport(export)
	if !v.OK {
		for _, c := range v.Checks {
			if !c.OK {
				t.Errorf("FAILED %s: %s", c.Name, c.Detail)
			}
		}
		t.Fatal("an export this service produced does not pass the verifier an " +
			"auditor will run against it")
	}
	if len(v.Unverified) == 0 {
		t.Error("the export's own verifier reported no limits")
	}
}

// The privacy property, asserted rather than assumed: nothing identifying
// another tenant may appear anywhere in the bytes an auditor receives.
func TestChainExportLeaksNothingAboutOtherTenants(t *testing.T) {
	st := newExportFixture(t, []int{3, 1, 4})
	h := &Handlers{Store: st, Logger: quietLogger()}

	export, err := h.buildChainExport(context.Background(), exportProjA, 1, 3)
	if err != nil {
		t.Fatalf("buildChainExport: %v", err)
	}

	raw, err := json.Marshal(export)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if strings.Contains(string(raw), exportProjB) {
		t.Error("the other tenant's project id appears in the export")
	}
	// Their execution ids must not appear either. Project B's executions
	// are the even-numbered ones in the fixture, so check a few directly.
	for _, ids := range st.sealed {
		for _, id := range ids {
			isMine := false
			for _, iv := range export.Intervals {
				for _, x := range iv.Executions {
					if x.ExecutionID == id {
						isMine = true
					}
				}
			}
			if !isMine && strings.Contains(string(raw), id) {
				t.Errorf("execution id %q belongs to another tenant but appears "+
					"in the export", id)
			}
		}
	}
}

// An hour in which this project ran nothing still exports its checkpoint.
// Omitting it would leave a hole in the sequence, and a hole is exactly
// what a reader is meant to treat as suspicious.
func TestChainExportKeepsCheckpointsForQuietHours(t *testing.T) {
	st := newExportFixture(t, []int{2, 0, 3})
	h := &Handlers{Store: st, Logger: quietLogger()}

	export, err := h.buildChainExport(context.Background(), exportProjA, 1, 3)
	if err != nil {
		t.Fatalf("buildChainExport: %v", err)
	}
	if len(export.Intervals) != 3 {
		t.Fatalf("want 3 intervals including the quiet one, got %d", len(export.Intervals))
	}
	quiet := export.Intervals[1]
	if quiet.Leaf != nil || quiet.Proof != nil || len(quiet.Executions) != 0 {
		t.Error("the quiet interval carries a leaf, proof or executions for a " +
			"project that ran nothing")
	}
	if quiet.Checkpoint.Seq == 0 {
		t.Error("the quiet interval dropped its checkpoint, leaving a hole in the chain")
	}
	if v := attest.VerifyChainExport(export); !v.OK {
		t.Errorf("an export containing a quiet hour failed verification: %v", v.Checks)
	}
}

func TestChainExportEndpointRefusesWithoutAProject(t *testing.T) {
	h := &Handlers{Store: newExportFixture(t, []int{1}), Logger: quietLogger()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me/chain/export", h.HandleChainExport)

	w := httptest.NewRecorder()
	mux.ServeHTTP(w, httptest.NewRequest(http.MethodGet,
		"/me/chain/export?from=1&to=1", nil))

	if w.Code != http.StatusUnauthorized {
		t.Errorf("an unauthenticated export request returned %d, not 401", w.Code)
	}
}

func TestChainExportEndpointRejectsBadRanges(t *testing.T) {
	h := &Handlers{Store: newExportFixture(t, []int{1, 1}), Logger: quietLogger()}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /me/chain/export", h.HandleChainExport)

	for _, q := range []string{
		"", "?from=1", "?to=2", "?from=abc&to=2", "?from=0&to=2", "?from=5&to=1",
	} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodGet, "/me/chain/export"+q, nil)
		r = r.WithContext(context.WithValue(r.Context(), ctxKeyProjectID, exportProjA))
		mux.ServeHTTP(w, r)

		if w.Code != http.StatusBadRequest {
			t.Errorf("query %q returned %d, want 400", q, w.Code)
		}
	}
}
