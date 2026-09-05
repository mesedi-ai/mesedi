package events

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The DLP sibling is the reason this exists; it shares its parent's
// sequence and was being counted as a customer claim.
func TestDLPScanResultIsBackendMinted(t *testing.T) {
	if !IsBackendMinted(EventTypeDLPScanResult) {
		t.Error("dlp_scan_result is written by Mesedi during ingest. If it is not " +
			"marked backend-minted, ingest will accept forged ones and the " +
			"record_integrity detector will count Mesedi's own annotation as the " +
			"customer having claimed a sequence twice")
	}
}

// The far more dangerous direction. Marking a type backend-minted makes
// ingest REJECT it, so a type customers legitimately send would stop
// being accepted at all — a silent, total data-loss bug for that type.
func TestCustomerEventTypesAreNotBackendMinted(t *testing.T) {
	customerTypes := []EventType{
		EventTypeLLMCall, EventTypeToolCall, EventTypeCheckpoint,
		EventTypeException, EventTypeValidatorResult, EventTypeDriftSignal,
		EventTypeInjectionAlert, EventTypeInfrastructure, EventTypeMCPCall,
		EventTypeEvalScore, EventTypeMemoryOperation, EventTypeAgentHandoff,
		EventTypeHumanIntervention,
	}
	for _, tt := range customerTypes {
		if IsBackendMinted(tt) {
			t.Errorf("%q is marked backend-minted, so ingest now REJECTS it. If "+
				"customers send this type, every such event is being dropped and "+
				"the only symptom is missing data", tt)
		}
	}
}

// An unknown type must not be treated as ours. Defaulting the other way
// would reject every event type added by a future SDK before the
// backend learned about it.
func TestUnknownTypesAreTreatedAsTheCustomers(t *testing.T) {
	if IsBackendMinted(EventType("something_a_newer_sdk_sends")) {
		t.Error("an unrecognised event type was treated as backend-minted, which " +
			"would reject it at ingest; a newer SDK's events would vanish")
	}
	if IsBackendMinted(EventType("")) {
		t.Error("the empty event type was treated as backend-minted")
	}
}

// The list here and the code that actually mints events must agree.
// They live in different packages with nothing connecting them, so a
// second backend-minted type added in api/ would silently keep being
// counted as a customer claim — the original bug, again, for a
// different type.
//
// Reads the sibling-minting source rather than importing it, because
// internal/api imports this package and the reverse would be a cycle.
func TestEveryTypeMintedInTheAPIPackageIsListedHere(t *testing.T) {
	root := filepath.Join("..", "api")
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Skipf("cannot read %s: %v", root, err)
	}

	// Look for `EventType:   events.EventTypeX,` inside composite
	// literals — how the backend constructs an event it is writing.
	found := map[string]string{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".go") ||
			strings.HasSuffix(e.Name(), "_test.go") {
			continue
		}
		b, err := os.ReadFile(filepath.Join(root, e.Name()))
		if err != nil {
			t.Fatalf("read %s: %v", e.Name(), err)
		}
		for _, line := range strings.Split(string(b), "\n") {
			trimmed := strings.TrimSpace(line)
			if !strings.HasPrefix(trimmed, "EventType:") {
				continue
			}
			const marker = "events.EventType"
			i := strings.Index(trimmed, marker)
			if i < 0 {
				continue
			}
			name := strings.TrimRight(trimmed[i+len("events."):], ",")
			found[name] = e.Name()
		}
	}

	if len(found) == 0 {
		t.Fatal("found no event-construction sites in internal/api; this test has " +
			"stopped checking anything and must be repaired, not deleted")
	}

	known := map[string]bool{"EventTypeDLPScanResult": true}
	for name, file := range found {
		if !known[name] {
			t.Errorf("internal/api/%s constructs an event of type %s, but that type "+
				"is not listed in IsBackendMinted. Either it is customer data being "+
				"written by the backend, or it is a backend-minted type that ingest "+
				"will accept forged copies of and that integrity checks will count "+
				"as a customer claim", file, name)
		}
	}
}
