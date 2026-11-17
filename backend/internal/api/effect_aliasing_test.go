package api

import (
	"testing"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

// effectRun stores one run: an optional declaration first, then the
// calls, through the first-seen harness's store path, and completes it.
func effectRun(f *firstSeenHarness, decl map[string]any, calls ...map[string]any) string {
	f.t.Helper()
	execID := f.store(calls...)
	if decl != nil {
		ev := payloadEvent(f.t, execID+"_decl", events.EventTypeEffectDeclaration, 0, decl)
		ev.ExecutionID = execID
		ev.Timestamp = f.clock
		if err := f.st.SaveEvents(t0(), []events.Event{ev}); err != nil {
			f.t.Fatal(err)
		}
	}
	f.complete(execID)
	return execID
}

func TestEffectAliasingEndToEnd(t *testing.T) {
	decl := map[string]any{"destinations": []map[string]any{
		{"destination": "github.com/acme/app", "via": []string{"git_push"}},
		{"destination": "s3://acme-reports/*"},
	}}
	cases := []struct {
		name  string
		decl  map[string]any
		calls []map[string]any
		want  string
	}{
		{"the declared route", decl, []map[string]any{
			{"tool_name": "git_push", "effects": []string{"github.com/acme/app"}},
			{"tool_name": "upload", "effects": []string{"s3://acme-reports/q4.csv"}},
		}, ""},
		{"a second route, through MCP", decl, []map[string]any{
			{"tool_name": "git_push", "effects": []string{"github.com/acme/app"}},
			{"server_name": "github", "method": "create_or_update_file", "effects": []string{"github.com/acme/app"}},
		}, "unlisted_path:github.com/acme/app:via:github/create_or_update_file"},
		{"a place never declared", decl, []map[string]any{
			{"tool_name": "sql_exec", "effects": []string{"db:prod/customers"}},
		}, "outside_scope:db:prod/customers"},
		{"no declaration, not checked", nil, []map[string]any{
			{"tool_name": "sql_exec", "effects": []string{"db:prod/customers"}},
		}, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := newFirstSeenHarness(t)
			effectRun(f, c.decl, c.calls...)
			got := f.groups(store.FailureClassEffectAliasing)
			if c.want == "" {
				if len(got) != 0 {
					t.Fatalf("groups = %v; want none", got)
				}
				return
			}
			if len(got) != 1 || got[c.want] != 1 {
				t.Fatalf("groups = %v; want %q", got, c.want)
			}
		})
	}
}

// The declaration is the customer's to send, so ingest must accept it.
func TestEffectDeclarationIsACustomerEvent(t *testing.T) {
	logger, _ := capturingLogger()
	accepted, rejected := validateIngestBatch([]events.Event{okEvent("d", events.EventTypeEffectDeclaration)}, logger)
	if rejected != 0 || len(accepted) != 1 {
		t.Fatalf("an effect declaration was refused at ingest (%d rejected)", rejected)
	}
}
