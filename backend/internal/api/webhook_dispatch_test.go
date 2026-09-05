package api

import (
	"context"
	"errors"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// fakeRecurrenceReader implements recurrenceLastFiredReader for the
// recurrenceShouldFire unit tests. Returns either a fixed timestamp
// or a synthetic error depending on which test calls it.
type fakeRecurrenceReader struct {
	lastFired time.Time
	err       error
}

func (f *fakeRecurrenceReader) GetWebhookRecurrenceLastFired(
	_ context.Context,
	_, _ string,
) (time.Time, error) {
	return f.lastFired, f.err
}

// noopWarnLogger discards all log lines. Tests don't care about log
// output, only the return value of recurrenceShouldFire.
type noopWarnLogger struct{}

func (noopWarnLogger) Warn(string, ...any) {}

func ptr(s string) *string { return &s }

func TestRecurrenceShouldFire_OffNeverFires(t *testing.T) {
	st := &fakeRecurrenceReader{err: store.ErrNotFound}
	mode := store.RecurrenceModeOff
	if recurrenceShouldFire(context.Background(), st, &mode, "wh_1", "grp_1", 0, noopWarnLogger{}) {
		t.Fatal("off mode must not fire")
	}
}

func TestRecurrenceShouldFire_EveryEventAlwaysFires(t *testing.T) {
	st := &fakeRecurrenceReader{err: store.ErrNotFound}
	mode := store.RecurrenceModeEveryEvent
	if !recurrenceShouldFire(context.Background(), st, &mode, "wh_1", "grp_1", 0, noopWarnLogger{}) {
		t.Fatal("every_event mode must fire")
	}
}

func TestRecurrenceShouldFire_ThrottledFiresOnFirstOccurrence(t *testing.T) {
	// No row yet, store returns ErrNotFound. Dispatcher must treat
	// that as "window elapsed" so the customer sees the first ping.
	st := &fakeRecurrenceReader{err: store.ErrNotFound}
	mode := store.RecurrenceModeThrottled
	if !recurrenceShouldFire(context.Background(), st, &mode, "wh_1", "grp_1", 3600, noopWarnLogger{}) {
		t.Fatal("throttled mode must fire on the first recurrence when no row exists")
	}
}

func TestRecurrenceShouldFire_ThrottledSuppressesWithinWindow(t *testing.T) {
	// Fired 5 minutes ago, window is 1 hour. Should suppress.
	st := &fakeRecurrenceReader{lastFired: time.Now().Add(-5 * time.Minute)}
	mode := store.RecurrenceModeThrottled
	if recurrenceShouldFire(context.Background(), st, &mode, "wh_1", "grp_1", 3600, noopWarnLogger{}) {
		t.Fatal("throttled mode must suppress when last fire was within the window")
	}
}

func TestRecurrenceShouldFire_ThrottledFiresAfterWindow(t *testing.T) {
	// Fired 2 hours ago, window is 1 hour. Should fire.
	st := &fakeRecurrenceReader{lastFired: time.Now().Add(-2 * time.Hour)}
	mode := store.RecurrenceModeThrottled
	if !recurrenceShouldFire(context.Background(), st, &mode, "wh_1", "grp_1", 3600, noopWarnLogger{}) {
		t.Fatal("throttled mode must fire after the window has elapsed")
	}
}

func TestRecurrenceShouldFire_ThrottledFloorsWindowToMin(t *testing.T) {
	// A misconfigured 5-second window must be promoted to the
	// RecurrenceMinWindowSeconds floor; fired 10 seconds ago should
	// therefore SUPPRESS even though 5s passed.
	st := &fakeRecurrenceReader{lastFired: time.Now().Add(-10 * time.Second)}
	mode := store.RecurrenceModeThrottled
	if recurrenceShouldFire(context.Background(), st, &mode, "wh_1", "grp_1", 5, noopWarnLogger{}) {
		t.Fatalf("throttled mode must enforce the %ds floor on the window",
			store.RecurrenceMinWindowSeconds)
	}
}

func TestRecurrenceShouldFire_ThrottledFiresOnDBError(t *testing.T) {
	// Non-ErrNotFound DB error must fail toward visibility (fire)
	// so a transient lookup hiccup doesn't silence the customer.
	st := &fakeRecurrenceReader{err: errors.New("transient db error")}
	mode := store.RecurrenceModeThrottled
	if !recurrenceShouldFire(context.Background(), st, &mode, "wh_1", "grp_1", 3600, noopWarnLogger{}) {
		t.Fatal("throttled mode must fire when last_fired lookup errors (err toward visibility)")
	}
}

func TestRecurrenceShouldFire_UnknownModeNeverFires(t *testing.T) {
	// Any unknown mode value is treated as "off" so a malformed row
	// can never spam the customer.
	st := &fakeRecurrenceReader{err: store.ErrNotFound}
	if recurrenceShouldFire(context.Background(), st, ptr("garbage"), "wh_1", "grp_1", 3600, noopWarnLogger{}) {
		t.Fatal("unknown recurrence mode must default to suppress")
	}
}

func TestRecurrenceShouldFire_EmptyModeNeverFires(t *testing.T) {
	// Empty string also treated as "off" (legacy row default).
	st := &fakeRecurrenceReader{err: store.ErrNotFound}
	if recurrenceShouldFire(context.Background(), st, ptr(""), "wh_1", "grp_1", 3600, noopWarnLogger{}) {
		t.Fatal("empty recurrence mode must default to suppress")
	}
}
