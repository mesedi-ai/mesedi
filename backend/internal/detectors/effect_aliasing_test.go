package detectors

import (
	"testing"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func decl(dests ...events.EffectDestination) events.EffectDeclarationPayload {
	return events.EffectDeclarationPayload{Destinations: dests}
}

func TestEffectAliasing(t *testing.T) {
	repo := events.EffectDestination{Destination: "github.com/acme/app", Via: []string{"git_push"}}
	bucket := events.EffectDestination{Destination: "s3://acme-reports/*"}
	scope := NewEffectScope([]events.EffectDeclarationPayload{decl(repo, bucket)})

	cases := []struct {
		name  string
		calls []ToolEffects
		want  string
	}{
		{"the watched path", []ToolEffects{{"git_push", []string{"github.com/acme/app"}}}, ""},
		// The digline shape: same effect, another route.
		{"another route to a watched place",
			[]ToolEffects{{"git_push", []string{"github.com/acme/app"}}, {"github/create_or_update_file", []string{"github.com/acme/app"}}},
			"unlisted_path:github.com/acme/app:via:github/create_or_update_file"},
		{"a place never declared", []ToolEffects{{"sql", []string{"db:prod/customers"}}}, "outside_scope:db:prod/customers"},
		{"a prefix declaration covers paths under it", []ToolEffects{{"upload", []string{"s3://acme-reports/2026/11.csv"}}}, ""},
		{"a prefix is not a substring match", []ToolEffects{{"upload", []string{"s3://acme-reports-old/x"}}}, ""},
		{"spaces around a destination are ignored", []ToolEffects{{"git_push", []string{"  github.com/acme/app "}}}, ""},
		{"calls that report nothing", []ToolEffects{{"read", nil}, {"read", []string{""}}}, ""},
		{"first violation in call order wins",
			[]ToolEffects{{"sql", []string{"db:a"}}, {"sql", []string{"db:b"}}}, "outside_scope:db:a"},
	}
	// "s3://acme-reports-old/x" does start with "s3://acme-reports", but
	// the declared prefix is "s3://acme-reports/", so it must fire.
	cases[4].want = "outside_scope:s3://acme-reports-old/x"

	for _, c := range cases {
		sig, fired := DetectEffectAliasing(scope, c.calls)
		if sig != c.want || fired != (c.want != "") {
			t.Errorf("%s: got %q %v, want %q", c.name, sig, fired, c.want)
		}
	}
}

func TestEffectAliasingNeedsADeclaration(t *testing.T) {
	calls := []ToolEffects{{"anything", []string{"anywhere"}}}
	for name, decls := range map[string][]events.EffectDeclarationPayload{
		"no declaration":               nil,
		"empty declaration":            {decl()},
		"a bare star declares nothing": {decl(events.EffectDestination{Destination: "*"})},
	} {
		if sig, fired := DetectEffectAliasing(NewEffectScope(decls), calls); fired {
			t.Errorf("%s: fired %q; an undeclared run is not checked", name, sig)
		}
	}
}

func TestEffectDeclarationsAddUp(t *testing.T) {
	scope := NewEffectScope([]events.EffectDeclarationPayload{
		decl(events.EffectDestination{Destination: "ledger", Via: []string{"post_entry"}}),
		decl(events.EffectDestination{Destination: "ledger", Via: []string{"reverse_entry"}}),
		decl(events.EffectDestination{Destination: "inbox"}),
	})
	ok := []ToolEffects{{"post_entry", []string{"ledger"}}, {"reverse_entry", []string{"ledger"}}, {"anything", []string{"inbox"}}}
	if sig, fired := DetectEffectAliasing(scope, ok); fired {
		t.Fatalf("allowed changes fired %q", sig)
	}
	if sig, _ := DetectEffectAliasing(scope, []ToolEffects{{"sql_exec", []string{"ledger"}}}); sig != "unlisted_path:ledger:via:sql_exec" {
		t.Fatalf("got %q", sig)
	}
}
