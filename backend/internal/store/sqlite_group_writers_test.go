package store

// Coverage-debt batch four: every Group* writer that had no test
// naming it, exercised table-driven against the contract they all
// share through groupExecutionInternal: the first call with a
// signature creates a group of exactly the right failure class and
// reports isNew, the second call with the same signature is a
// recurrence and must not, and an empty signature is refused. The
// class mapping IS the contract: a writer filing under the wrong
// class would render every dashboard and webhook filter wrong while
// every other test stayed green. Notably, the time-budget,
// step-count, and both call-loop writers all deliberately file under
// the loops class with distinguishing signature prefixes; this test
// pins that consolidation so a future "cleanup" cannot silently
// split them into classes no dashboard filter knows about.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func TestEveryGroupWriterFilesItsOwnClass(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.CreateProject(ctx, &Project{
		ProjectID: "proj_gw", Name: "group writers", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	type writer func(ctx context.Context, executionID, projectID, signature string) (bool, error)
	cases := []struct {
		name  string
		class string
		// The numeric-input writers compute their own signatures,
		// so an empty string cannot reach the store through them.
		skipEmptyCheck bool
		fn             writer
	}{
		{"GroupCrashedExecution", FailureClassCrashes, false, st.GroupCrashedExecution},
		{"GroupTimeBudgetExceedance", FailureClassLoops, true,
			func(ctx context.Context, executionID, projectID, _ string) (bool, error) {
				return st.GroupTimeBudgetExceedance(ctx, executionID, projectID, 15_000)
			}},
		{"GroupStepCountExceedance", FailureClassLoops, true,
			func(ctx context.Context, executionID, projectID, _ string) (bool, error) {
				return st.GroupStepCountExceedance(ctx, executionID, projectID, 60)
			}},
		{"GroupInfrastructureThrottled", FailureClassInfraThrottled, false, st.GroupInfrastructureThrottled},
		{"GroupDataLeakage", FailureClassDataLeakage, false, st.GroupDataLeakage},
		{"GroupSemanticLoop", FailureClassSemanticLoop, false, st.GroupSemanticLoop},
		{"GroupToolSchemaDrift", FailureClassToolSchemaDrift, false, st.GroupToolSchemaDrift},
		{"GroupContextOverflow", FailureClassContextOverflow, false, st.GroupContextOverflow},
		{"GroupTokenWaste", FailureClassTokenWaste, false, st.GroupTokenWaste},
		{"GroupSandboxEscape", FailureClassSandboxEscape, false, st.GroupSandboxEscape},
		{"GroupGroundingFailure", FailureClassGroundingFailure, false, st.GroupGroundingFailure},
		{"GroupCascadingFailure", FailureClassCascadingFailure, false, st.GroupCascadingFailure},
		{"GroupCoordinationDeadlock", FailureClassCoordinationDeadlock, false, st.GroupCoordinationDeadlock},
		{"GroupProviderIncident", FailureClassProviderIncident, false, st.GroupProviderIncident},
		{"GroupHITLTimeout", FailureClassHITLTimeout, false, st.GroupHITLTimeout},
		{"GroupHITLRejectionSpike", FailureClassHITLRejectionSpike, false, st.GroupHITLRejectionSpike},
		{"GroupValidatorFailure", FailureClassValidator, false, st.GroupValidatorFailure},
		{"GroupPromptInjection", FailureClassInjection, false, st.GroupPromptInjection},
		{"GroupIdenticalCallLoop", FailureClassLoops, false, st.GroupIdenticalCallLoop},
		{"GroupSimilarCallLoop", FailureClassLoops, false, st.GroupSimilarCallLoop},
		{"GroupDriftSignal", FailureClassDrift, false, st.GroupDriftSignal},
	}

	wantByClass := map[string]int{}
	for i, tc := range cases {
		wantByClass[tc.class]++
		execID := "exec_gw_" + tc.name
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: execID, ProjectID: "proj_gw",
			Status: events.StatusStarted, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("[%d] create execution: %v", i, err)
		}
		sig := tc.name + "-sig"
		isNew, err := tc.fn(ctx, execID, "proj_gw", sig)
		if err != nil || !isNew {
			t.Errorf("%s first call = (%v, %v), want a new group", tc.name, isNew, err)
			continue
		}
		isNew, err = tc.fn(ctx, execID, "proj_gw", sig)
		if err != nil || isNew {
			t.Errorf("%s second call = (%v, %v), want a recurrence, not new", tc.name, isNew, err)
		}
		if !tc.skipEmptyCheck {
			if _, err := tc.fn(ctx, execID, "proj_gw", ""); err == nil {
				t.Errorf("%s accepted an empty signature", tc.name)
			}
		}
	}

	groups, err := st.ListFailureGroups(ctx, "proj_gw", ListFailureGroupsOpts{Limit: 100})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	gotByClass := map[string]int{}
	for _, g := range groups {
		gotByClass[g.FailureClass]++
	}
	for class, want := range wantByClass {
		if gotByClass[class] != want {
			t.Errorf("class %s has %d groups, want %d: a writer filed under the wrong class",
				class, gotByClass[class], want)
		}
	}
	for class, got := range gotByClass {
		if wantByClass[class] == 0 {
			t.Errorf("unexpected class %s with %d groups", class, got)
		}
	}
}
