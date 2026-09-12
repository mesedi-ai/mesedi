package store

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"
)

// The abuse path is customer-facing enforcement: a suspended project
// is locked out by the auth middleware on the strength of what these
// methods persist. Until the 2026-09-12 store split surfaced it, not
// one of them was referenced by any test. This walks the full
// lifecycle: signal created, listed, notified, project suspended,
// signal resolved, project unsuspended, with the middleware's
// hot-path check consulted at each stage.
func TestAbuseSignalLifecycleDrivesProjectSuspension(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	const projectID = "proj_abuse_test"
	if err := st.CreateProject(ctx, &Project{
		ProjectID: projectID, Name: "abuse lifecycle", Tier: "hobby",
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("create project: %v", err)
	}

	if ok, _, err := st.IsProjectSuspended(ctx, projectID); err != nil || ok {
		t.Fatalf("fresh project reads suspended=%v err=%v; want active", ok, err)
	}

	detected := time.Now().UTC().Truncate(time.Second)
	sig := &AbuseSignal{
		SignalID: "sig-1", ProjectID: projectID, Kind: "event_flood",
		Severity: "high", Detail: `{"events_per_min":100000}`, DetectedAt: detected,
	}
	if err := st.CreateAbuseSignal(ctx, sig); err != nil {
		t.Fatalf("create signal: %v", err)
	}

	unresolved, err := st.ListAbuseSignals(ctx, true, 10)
	if err != nil || len(unresolved) != 1 || unresolved[0].SignalID != "sig-1" {
		t.Fatalf("unresolved list = %v (err %v), want exactly sig-1", unresolved, err)
	}

	if err := st.MarkAbuseSignalNotified(ctx, "sig-1", detected.Add(time.Minute)); err != nil {
		t.Fatalf("mark notified: %v", err)
	}

	const reason = "event flood, 100k events/min"
	if err := st.MarkAbuseSignalSuspended(ctx, "sig-1", projectID, reason,
		detected.Add(2*time.Minute)); err != nil {
		t.Fatalf("mark suspended: %v", err)
	}
	ok, got, err := st.IsProjectSuspended(ctx, projectID)
	if err != nil || !ok || got != reason {
		t.Fatalf("after suspension: ok=%v reason=%q err=%v; want true with the stored reason",
			ok, got, err)
	}

	if err := st.ResolveAbuseSignal(ctx, "sig-1", "robert", "false positive",
		detected.Add(3*time.Minute)); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	fetched, err := st.GetAbuseSignal(ctx, "sig-1")
	if err != nil {
		t.Fatalf("get signal: %v", err)
	}
	if fetched.ResolvedAt == nil || fetched.ResolvedBy != "robert" ||
		fetched.NotifiedAt == nil || fetched.SuspendedAt == nil {
		t.Fatalf("resolved signal did not retain its lifecycle timestamps: %+v", fetched)
	}
	if remaining, err := st.ListAbuseSignals(ctx, true, 10); err != nil || len(remaining) != 0 {
		t.Fatalf("resolved signal still listed as unresolved: %v (err %v)", remaining, err)
	}

	// Resolution does not implicitly unsuspend; that is a separate,
	// deliberate act, and the order matters for the middleware.
	if ok, _, _ := st.IsProjectSuspended(ctx, projectID); !ok {
		t.Fatal("resolving the signal silently unsuspended the project")
	}
	if err := st.UnsuspendProject(ctx, projectID); err != nil {
		t.Fatalf("unsuspend: %v", err)
	}
	if ok, reason, err := st.IsProjectSuspended(ctx, projectID); err != nil || ok || reason != "" {
		t.Fatalf("after unsuspend: ok=%v reason=%q err=%v; want active with no reason",
			ok, reason, err)
	}

	// Unknown project must read as active, never as an error: the
	// middleware treats an error here as a 500 on every request.
	if ok, _, err := st.IsProjectSuspended(ctx, "proj_never_existed"); err != nil || ok {
		t.Fatalf("unknown project: ok=%v err=%v; want (false, nil)", ok, err)
	}
}
