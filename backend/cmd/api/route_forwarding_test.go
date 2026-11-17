package main

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"
)

// Every route the API registers must be forwarded by this binary's
// top-level router, or it answers 404 in production with no log line.
// That trap was hit at least twice (email verification, 2FA) and a third
// time on 17 November 2026 by the spend report, caught only by the
// integration suite. A Go test that builds its own router cannot see
// it; this one reads both route lists.
func TestEveryRegisteredRouteIsForwarded(t *testing.T) {
	route := regexp.MustCompile(`mux\.HandleFunc\("([A-Z]+ [^"]+)"`)
	files, err := filepath.Glob(filepath.Join("..", "..", "internal", "api", "*.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("cannot find the API's route registrations: %v", err)
	}
	registered := map[string]bool{}
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, m := range route.FindAllStringSubmatch(string(b), -1) {
			registered[m[1]] = true
		}
	}
	if len(registered) < 50 {
		t.Fatalf("found only %d registered routes; this guard has stopped reading them", len(registered))
	}
	main, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	forwarded := map[string]bool{}
	for _, m := range regexp.MustCompile(`mux\.Handle(?:Func)?\("([A-Z]+ [^"]+)"`).FindAllStringSubmatch(string(main), -1) {
		forwarded[m[1]] = true
	}
	var missing []string
	for r := range registered {
		if !forwarded[r] {
			missing = append(missing, r)
		}
	}
	sort.Strings(missing)
	if len(missing) > 0 {
		t.Fatalf("registered by the API but not forwarded in cmd/api/main.go, so they answer "+
			"404 in production: %v", missing)
	}
}
