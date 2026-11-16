package api

import (
	"strings"
	"testing"

	"mesedi/backend/internal/store"
)

// Every failure class a detector can emit must be subscribable by webhook.
// Until 2026-11-16 the filter missed environment_misapprehension and
// covert_coordination (the outside review of 2026-11-15).
func TestWebhookFilterAcceptsEveryFailureClass(t *testing.T) {
	for _, c := range store.AllFailureClasses {
		if _, ok := validFailureClasses[c]; !ok {
			t.Errorf("webhooks reject failure class %q", c)
		}
	}
	if len(validFailureClasses) != len(store.AllFailureClasses) {
		t.Errorf("webhook filter has %d classes, the store declares %d", len(validFailureClasses), len(store.AllFailureClasses))
	}
	for _, c := range []string{store.FailureClassEnvironmentMisapprehension, store.FailureClassCovertCoordination} {
		if !strings.Contains(strings.Join(store.AllFailureClasses, ","), c) {
			t.Errorf("%s missing", c)
		}
	}
}
