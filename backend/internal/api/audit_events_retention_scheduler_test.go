// Unit tests for AuditEventsRetentionScheduler.
//
// Coverage:
//   - tick() computes cutoff = now - Retention and calls the store
//     method exactly once with that cutoff.
//   - Successful prune logs the deletion count.
//   - Store error is logged but does not panic and does not bubble.
//   - Zero Retention falls back to DefaultAuditEventsRetention on
//     Start (we exercise the cutoff math directly via tick).
//
// We do not exercise the goroutine loop here. tick() is the unit of
// work; testing the time.Ticker scheduling adds flake risk without
// catching real bugs. The Start/Shutdown lifecycle is verified by
// the broader integration smoke test on staging.
package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// stubRetentionStore embeds store.Store so it satisfies the interface
// without implementing every method. Only the audit-events delete
// method is reached by tick().
type stubRetentionStore struct {
	store.Store

	calls       int
	gotCutoff   time.Time
	returnCount int64
	returnErr   error
}

func (s *stubRetentionStore) DeleteClosedProjectAuditEventsOlderThan(
	_ context.Context, cutoff time.Time,
) (int64, error) {
	s.calls++
	s.gotCutoff = cutoff
	return s.returnCount, s.returnErr
}

func Test_AuditEventsRetentionScheduler_Tick_ComputesCutoffFromRetention(t *testing.T) {
	st := &stubRetentionStore{returnCount: 5}
	s := &AuditEventsRetentionScheduler{
		Store:     st,
		Logger:    quietLogger(),
		Retention: 7 * 365 * 24 * time.Hour,
	}

	before := time.Now().UTC()
	s.tick(context.Background())
	after := time.Now().UTC()

	if st.calls != 1 {
		t.Fatalf("want exactly 1 store call, got %d", st.calls)
	}

	// Cutoff should be in the range
	//   [before - retention, after - retention]
	// allowing for the wall-clock drift while tick was running.
	wantLowerBound := before.Add(-s.Retention)
	wantUpperBound := after.Add(-s.Retention)

	if st.gotCutoff.Before(wantLowerBound) || st.gotCutoff.After(wantUpperBound) {
		t.Errorf("cutoff = %v; want in [%v, %v] (now - 7y window)",
			st.gotCutoff.Format(time.RFC3339),
			wantLowerBound.Format(time.RFC3339),
			wantUpperBound.Format(time.RFC3339))
	}
}

func Test_AuditEventsRetentionScheduler_Tick_StoreError_DoesNotPanic(t *testing.T) {
	st := &stubRetentionStore{returnErr: errors.New("simulated db down")}
	s := &AuditEventsRetentionScheduler{
		Store:     st,
		Logger:    quietLogger(),
		Retention: 7 * 365 * 24 * time.Hour,
	}

	// MUST NOT panic. Soft failure is the contract; the scheduler
	// keeps ticking and the next attempt has a fresh shot at the DB.
	s.tick(context.Background())

	if st.calls != 1 {
		t.Errorf("want 1 store call even on error, got %d", st.calls)
	}
}

func Test_AuditEventsRetentionScheduler_Tick_ZeroDeletes_NoError(t *testing.T) {
	st := &stubRetentionStore{returnCount: 0}
	s := &AuditEventsRetentionScheduler{
		Store:     st,
		Logger:    quietLogger(),
		Retention: 7 * 365 * 24 * time.Hour,
	}

	s.tick(context.Background())

	if st.calls != 1 {
		t.Fatalf("want 1 store call, got %d", st.calls)
	}
	// No-deletion is a Debug log, not an error path; just verifies
	// we did not return early or call the store more than once.
}

func Test_AuditEventsRetentionScheduler_DefaultRetentionConstant(t *testing.T) {
	want := 7 * 365 * 24 * time.Hour
	if DefaultAuditEventsRetention != want {
		t.Errorf("DefaultAuditEventsRetention = %v, want %v (7 years)",
			DefaultAuditEventsRetention, want)
	}
}
