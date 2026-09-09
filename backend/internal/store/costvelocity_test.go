package store

// Tests for the #48 attribution pieces: identity resolution, the
// attributed signature, and the property the whole change exists
// for, that two different actors crossing the same cost bucket land
// in two different failure groups, while the same actor recurring
// lands in one.

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

func TestCostVelocityIdentity_Resolution(t *testing.T) {
	cases := []struct {
		tenant, apiKey, want string
	}{
		{"acme-prod", "key_ab12", "tenant:acme-prod"}, // tenant wins when both present
		{"acme-prod", "", "tenant:acme-prod"},
		{"", "key_ab12", "key:key_ab12"},
		{"", "", "unattributed"},
	}
	for _, c := range cases {
		if got := CostVelocityIdentity(c.tenant, c.apiKey); got != c.want {
			t.Errorf("CostVelocityIdentity(%q, %q) = %q, want %q",
				c.tenant, c.apiKey, got, c.want)
		}
	}
}

func TestCostVelocityAttributedSignature_Shape(t *testing.T) {
	got := CostVelocityAttributedSignature(50.0, "tenant:acme-prod")
	if got != "cost_$10+|tenant:acme-prod" {
		t.Errorf("attributed signature = %q, want cost_$10+|tenant:acme-prod", got)
	}
}

func TestGroupCostVelocity_DistinctIdentitiesDistinctGroups(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	const projectID = "proj_cv_attr"
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "attribution test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	mk := func(id string) {
		t.Helper()
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: id, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create execution %s: %v", id, err)
		}
	}
	mk("exec_actor_a_1")
	mk("exec_actor_a_2")
	mk("exec_actor_b_1")

	// Actor A crosses the threshold: NEW group.
	isNew, err := st.GroupCostVelocity(ctx, "exec_actor_a_1", projectID, 15.0, "tenant:actor-a")
	if err != nil {
		t.Fatalf("group a1: %v", err)
	}
	if !isNew {
		t.Errorf("first spend from actor-a should create a new group")
	}
	// Actor A again, same bucket: recurrence, NOT new.
	isNew, err = st.GroupCostVelocity(ctx, "exec_actor_a_2", projectID, 20.0, "tenant:actor-a")
	if err != nil {
		t.Fatalf("group a2: %v", err)
	}
	if isNew {
		t.Errorf("second spend from actor-a in the same bucket should be a recurrence")
	}
	// Actor B, same bucket, never seen: NEW group. This is the #48
	// property itself; pre-attribution both actors shared one
	// cost_$10+ group and actor B looked like routine recurrence.
	isNew, err = st.GroupCostVelocity(ctx, "exec_actor_b_1", projectID, 15.0, "tenant:actor-b")
	if err != nil {
		t.Fatalf("group b1: %v", err)
	}
	if !isNew {
		t.Errorf("first spend from never-seen actor-b must create a NEW group, " +
			"not fold into actor-a's; indistinguishability is the #48 defect")
	}

	groups, err := st.ListFailureGroups(ctx, projectID, ListFailureGroupsOpts{Limit: 10})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	sigs := map[string]bool{}
	for _, g := range groups {
		if g.FailureClass == FailureClassCostVelocity {
			sigs[g.Signature] = true
		}
	}
	if !sigs["cost_$10+|tenant:actor-a"] || !sigs["cost_$10+|tenant:actor-b"] {
		t.Errorf("want one group per actor (cost_$10+|tenant:actor-a and "+
			"cost_$10+|tenant:actor-b), got signatures %v", sigs)
	}
}

func TestGroupCostVelocityRate_GroupsUnderRateSignature(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	ctx := context.Background()
	const projectID = "proj_cv_rate"
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "rate grouping test", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}
	for _, id := range []string{"exec_rate_1", "exec_rate_2"} {
		if err := st.CreateExecution(ctx, &events.Execution{
			ExecutionID: id, ProjectID: projectID,
			Status: events.StatusStarted, StartedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("create execution %s: %v", id, err)
		}
	}

	isNew, err := st.GroupCostVelocityRate(ctx, "exec_rate_1", projectID, 12.0)
	if err != nil {
		t.Fatalf("rate group 1: %v", err)
	}
	if !isNew {
		t.Errorf("first rate trip should create a new group")
	}
	isNew, err = st.GroupCostVelocityRate(ctx, "exec_rate_2", projectID, 15.0)
	if err != nil {
		t.Fatalf("rate group 2: %v", err)
	}
	if isNew {
		t.Errorf("second trip in the same rate bucket should be a recurrence")
	}

	groups, err := st.ListFailureGroups(ctx, projectID, ListFailureGroupsOpts{Limit: 10})
	if err != nil {
		t.Fatalf("list groups: %v", err)
	}
	want := CostVelocityRateSignature(12.0) // rate_$10+_per_min, deliberately unattributed
	found := false
	for _, g := range groups {
		if g.FailureClass == FailureClassCostVelocity && g.Signature == want {
			found = true
		}
	}
	if !found {
		t.Errorf("no rate group with signature %q; the rate detector stays "+
			"project-wide by design and its signature must carry no identity", want)
	}
}
