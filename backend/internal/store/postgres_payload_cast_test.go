package store

// Every Postgres query that reads into an event payload must cast it to
// jsonb first: the payload column is TEXT, and "payload->>'x'" on TEXT
// is an error at query time, not at build time. Found 17 November 2026:
// the tool-definition query had been written without the cast, so the
// "tool definition changed" check failed on every call on Postgres, the
// production database, and no test ran it there.

import (
	"os"
	"path/filepath"
	"regexp"
	"testing"
)

var uncastPayloadRead = regexp.MustCompile(`\bpayload\s*->>?\s*'`)

func TestPostgresPayloadReadsCastToJSONB(t *testing.T) {
	files, err := filepath.Glob("postgres_*.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range files {
		if filepath.Ext(f) != ".go" || len(f) > 8 && f[len(f)-8:] == "_test.go" {
			continue
		}
		raw, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, loc := range uncastPayloadRead.FindAllIndex(raw, -1) {
			line := 1
			for _, b := range raw[:loc[0]] {
				if b == '\n' {
					line++
				}
			}
			t.Errorf("%s:%d reads payload as JSON without ::jsonb; the column is TEXT, so this fails on Postgres", f, line)
		}
	}
}
