package events

import "testing"

// Every declared status must fall into exactly one lifecycle class:
// actively running, paused for a human, or terminal. IsTerminal and
// IsPaused are the two predicates detectors and budget enforcers key
// on, so a status that satisfies both, or neither while not being
// `started`, means the lifecycle grew without the predicates keeping
// up, which is the exact drift their doc comments promise to prevent.
func TestStatusLifecycleClassesPartitionTheStatusSet(t *testing.T) {
	all := []ExecutionStatus{
		StatusStarted,
		StatusAwaitingHuman,
		StatusCompleted,
		StatusCrashed,
		StatusHalted,
		StatusTimeout,
		StatusValidationFailed,
	}
	terminal := map[ExecutionStatus]bool{
		StatusCompleted:        true,
		StatusCrashed:          true,
		StatusHalted:           true,
		StatusTimeout:          true,
		StatusValidationFailed: true,
	}
	for _, s := range all {
		if got, want := s.IsTerminal(), terminal[s]; got != want {
			t.Errorf("%q.IsTerminal() = %v, want %v", s, got, want)
		}
		if got, want := s.IsPaused(), s == StatusAwaitingHuman; got != want {
			t.Errorf("%q.IsPaused() = %v, want %v", s, got, want)
		}
		if s.IsTerminal() && s.IsPaused() {
			t.Errorf("%q claims to be both terminal and paused", s)
		}
		if !s.IsTerminal() && !s.IsPaused() && s != StatusStarted {
			t.Errorf("%q is neither terminal, paused, nor started", s)
		}
	}
}
