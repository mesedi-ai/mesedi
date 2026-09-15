package store

// Coverage-debt batch six, the final store batch: the nine admin and
// billing odds and ends the split surfaced with zero test references.
// One seeded world, walked in dependency order: owner listing, quota
// grants, severity hints, analysis save plus the admin usage rollup
// that reads it back, storage stats, the active-execution enumeration
// the budget-ceiling halt fans out over, and finally the per-project
// group delete, which runs last because it destroys the fixtures the
// earlier subtests read.

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func TestAdminAndBillingOddsAndEnds(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	mkProject := func(id, owner string) {
		t.Helper()
		if err := st.CreateProject(ctx, &Project{
			ProjectID: id, Name: "odds " + id, Tier: "hobby",
			OwnerUserID: owner, CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create project %s: %v", id, err)
		}
	}
	mkProject("proj_odd_a", "user_a")
	mkProject("proj_odd_b", "user_a")
	mkProject("proj_odd_c", "user_b")

	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: "exec_odd_1", ProjectID: "proj_odd_a",
		Status: events.StatusStarted, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create execution: %v", err)
	}
	if err := st.CreateExecution(ctx, &events.Execution{
		ExecutionID: "exec_odd_2", ProjectID: "proj_odd_a",
		Status: events.StatusCompleted, StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create completed execution: %v", err)
	}
	if isNew, err := st.GroupCrashedExecution(ctx, "exec_odd_1", "proj_odd_a", "odds-sig"); err != nil || !isNew {
		t.Fatalf("seed failure group: (%v, %v)", isNew, err)
	}
	groupID := DeriveFailureGroupID("proj_odd_a", FailureClassCrashes, "odds-sig")

	t.Run("ListProjectsByOwner", func(t *testing.T) {
		got, err := st.ListProjectsByOwner(ctx, "user_a")
		if err != nil || len(got) != 2 {
			t.Fatalf("= %d projects (%v), want user_a's two", len(got), err)
		}
		if got[0].ProjectID != "proj_odd_a" || got[1].ProjectID != "proj_odd_b" {
			t.Fatalf("order = %s, %s; want creation order", got[0].ProjectID, got[1].ProjectID)
		}
		if got, err := st.ListProjectsByOwner(ctx, ""); err != nil || len(got) != 0 {
			t.Fatalf("empty owner = %d projects (%v), want an empty answer, not everything", len(got), err)
		}
	})

	t.Run("AddGrantedExecutions", func(t *testing.T) {
		expiry := time.Now().UTC().Add(24 * time.Hour)
		if err := st.AddGrantedExecutions(ctx, "proj_odd_a", 100_000, &expiry); err != nil {
			t.Fatalf("grant: %v", err)
		}
		p, err := st.GetProject(ctx, "proj_odd_a")
		if err != nil || p.GrantedExecutions != 100_000 {
			t.Fatalf("after grant = %d (%v), want the full grant", p.GrantedExecutions, err)
		}
		if p.GrantedExecutionsExpiresAt == nil || p.GrantedExecutionsExpiresAt.Unix() != expiry.Unix() {
			t.Fatalf("expiry = %v, want the granted expiry", p.GrantedExecutionsExpiresAt)
		}
		// A revocation larger than the grant leaves the signed column
		// negative for auditability, and a nil expiry clears the old
		// one rather than preserving it.
		if err := st.AddGrantedExecutions(ctx, "proj_odd_a", -200_000, nil); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		p, err = st.GetProject(ctx, "proj_odd_a")
		if err != nil || p.GrantedExecutions != -100_000 {
			t.Fatalf("after revoke = %d (%v), want the signed negative balance", p.GrantedExecutions, err)
		}
		if p.GrantedExecutionsExpiresAt != nil {
			t.Fatalf("expiry = %v, want it cleared by the nil-expiry update", p.GrantedExecutionsExpiresAt)
		}
		if err := st.AddGrantedExecutions(ctx, "proj_missing", 1, nil); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown project = %v, want ErrNotFound", err)
		}
	})

	t.Run("SeverityHint", func(t *testing.T) {
		// The read side answers empty for an unknown group while the
		// write side refuses it: readers tolerate absence, writers
		// must not invent rows.
		if hint, err := st.GetFailureGroupSeverityHint(ctx, "grp-missing"); err != nil || hint != "" {
			t.Fatalf("unknown group read = (%q, %v), want quiet empty", hint, err)
		}
		if err := st.UpdateFailureGroupSeverityHint(ctx, "grp-missing", "high"); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown group write = %v, want ErrNotFound", err)
		}
		if err := st.UpdateFailureGroupSeverityHint(ctx, groupID, "high"); err != nil {
			t.Fatalf("set hint: %v", err)
		}
		if hint, err := st.GetFailureGroupSeverityHint(ctx, groupID); err != nil || hint != "high" {
			t.Fatalf("read back = (%q, %v), want the hint just set", hint, err)
		}
		if err := st.UpdateFailureGroupSeverityHint(ctx, groupID, ""); err != nil {
			t.Fatalf("clear hint: %v", err)
		}
		if hint, err := st.GetFailureGroupSeverityHint(ctx, groupID); err != nil || hint != "" {
			t.Fatalf("after clear = (%q, %v), want empty again", hint, err)
		}
	})

	t.Run("SaveAnalysisAndUsageRollup", func(t *testing.T) {
		if err := st.SaveFailureGroupAnalysis(ctx, "grp-missing", "md", "model", time.Now().UTC(), ""); !errors.Is(err, ErrNotFound) {
			t.Fatalf("unknown group = %v, want ErrNotFound", err)
		}
		if err := st.SaveFailureGroupAnalysis(ctx, groupID,
			"analysis body", "claude-opus-4-6", time.Now().UTC(), "playbook-sig"); err != nil {
			t.Fatalf("save analysis: %v", err)
		}
		rows, err := st.ListAIAnalysesUsageByProject(ctx, time.Now().UTC().Add(-time.Hour))
		if err != nil || len(rows) != 1 {
			t.Fatalf("= %d rows (%v), want the one analyzed project", len(rows), err)
		}
		r := rows[0]
		if r.ProjectID != "proj_odd_a" || r.Count != 1 {
			t.Fatalf("row = %+v, want one analysis on the seeded project", r)
		}
		if len(r.FailureClasses) != 1 || r.FailureClasses[0] != FailureClassCrashes {
			t.Fatalf("classes = %v, want the crashes filter chip", r.FailureClasses)
		}
	})

	t.Run("GetProjectStorageStats", func(t *testing.T) {
		stats, err := st.GetProjectStorageStats(ctx)
		if err != nil || len(stats) != 3 {
			t.Fatalf("= %d rows (%v), want every project", len(stats), err)
		}
		byID := map[string]*ProjectStorage{}
		for _, row := range stats {
			byID[row.ProjectID] = row
		}
		a := byID["proj_odd_a"]
		if a == nil || a.Executions != 2 || a.FailureGroups != 1 {
			t.Fatalf("proj_odd_a = %+v, want its two executions and one group counted", a)
		}
		if b := byID["proj_odd_b"]; b == nil || b.Executions != 0 {
			t.Fatalf("proj_odd_b = %+v, want an empty project still listed", b)
		}
	})

	t.Run("ListActiveExecutionsByProject", func(t *testing.T) {
		active, err := st.ListActiveExecutionsByProject(ctx, "proj_odd_a")
		if err != nil || len(active) != 1 || active[0].ExecutionID != "exec_odd_1" {
			t.Fatalf("= %d (%v), want only the started execution, not the completed one", len(active), err)
		}
	})

	t.Run("DeleteFailureGroupsByProject", func(t *testing.T) {
		// Seed a second project's group to prove the delete is scoped.
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: "exec_odd_b1", ProjectID: "proj_odd_b",
			Status: events.StatusStarted, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create execution: %v", err)
		}
		if _, err := st.GroupCrashedExecution(ctx, "exec_odd_b1", "proj_odd_b", "odds-sig-b"); err != nil {
			t.Fatalf("seed second group: %v", err)
		}
		n, err := st.DeleteFailureGroupsByProject(ctx, "proj_odd_a")
		if err != nil || n != 1 {
			t.Fatalf("= (%d, %v), want exactly the one group deleted", n, err)
		}
		n, err = st.DeleteFailureGroupsByProject(ctx, "proj_odd_a")
		if err != nil || n != 0 {
			t.Fatalf("second delete = (%d, %v), want nothing left to delete", n, err)
		}
		remaining, err := st.ListFailureGroups(ctx, "proj_odd_b", ListFailureGroupsOpts{Limit: 10})
		if err != nil || len(remaining) != 1 {
			t.Fatalf("= %d groups (%v), want the other project's group untouched", len(remaining), err)
		}
	})
}
