package severity_test

import (
	"testing"

	"mesedi/backend/internal/severity"
	"mesedi/backend/internal/store"
)

func TestEveryFailureClassHasAnExplicitDefault(t *testing.T) {
	for _, c := range store.AllFailureClasses {
		if !severity.HasExplicitDefault(c) {
			t.Errorf("failure class %q has no default severity of its own; it falls through to warning", c)
		}
	}
}
