// The egress-derived detector queries in sqlite_egress_queries.go
// and postgres_egress_queries.go match events by SQL string literal
// ('egress', 'environment_declaration'), not by the Go constants in
// backend/attest/events. Nothing else references those constants on
// the read path, so changing a constant's value would compile clean,
// pass most tests, and silently orphan both detectors: writers would
// emit the new value while every query keeps matching the old one.
// These pins turn that silent break into a loud one. If one of these
// assertions fails, the SQL literals in both query files must change
// in the same commit as the constant.
package store

import (
	"testing"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func TestEventWireValuesMatchTheSQLLiterals(t *testing.T) {
	pins := []struct {
		name     string
		constant events.EventType
		sqlValue string
	}{
		{"egress", events.EventTypeEgress, "egress"},
		{"environment declaration", events.EventTypeEnvironmentDeclaration, "environment_declaration"},
	}
	for _, pin := range pins {
		if string(pin.constant) != pin.sqlValue {
			t.Errorf("%s: constant is %q but the egress query files match the literal %q; update the SQL in sqlite_egress_queries.go and postgres_egress_queries.go together with the constant",
				pin.name, pin.constant, pin.sqlValue)
		}
	}
}
