package main

import (
	"encoding/json"
	"net/http/httptest"
	"os"
	"regexp"
	"testing"
)

// Why this test exists.
//
// buildGitSHA is set at link time with `go build -ldflags -X`. The linker
// SILENTLY IGNORES an -X flag whose symbol does not resolve: no warning, no
// error, exit 0. The image builds, the container starts, /health answers,
// and the field is the dev default.
//
// That is not hypothetical. In verdifax-orchestrator on 2026-09-04 the
// module was renamed and the Dockerfile kept stamping the old symbol path.
// Eleven commits shipped claiming unknown provenance before a human noticed
// by reading /health by hand.
//
// This repo is now one commit into the same arrangement, so the invariant is
// pinned before it has a chance to rot: the Dockerfile's -X target must name
// a variable that actually exists in this package, and /health must actually
// report it.

var ldflagXPattern = regexp.MustCompile(`-X\s+'([^'=]+)=`)

func TestDockerfileStampsAVariableThatExists(t *testing.T) {
	// cmd/api/ -> backend/
	raw, err := os.ReadFile("../../Dockerfile")
	if err != nil {
		t.Fatalf("reading Dockerfile: %v", err)
	}

	matches := ldflagXPattern.FindAllStringSubmatch(string(raw), -1)
	if len(matches) == 0 {
		t.Fatal("Dockerfile no longer stamps any -X value; /health would always " +
			"report 'unknown' and the deploy verification step could never pass")
	}

	found := false
	for _, m := range matches {
		symbol := m[1]
		if symbol == "main.buildGitSHA" {
			found = true
			continue
		}
		// Anything else must at least be package main, since that is the
		// only package this Dockerfile builds. A path into an internal
		// package would resolve at build time only by luck and would break
		// silently on any module rename.
		if len(symbol) < 5 || symbol[:5] != "main." {
			t.Errorf("-X target %q is not in package main. cmd/api is the only thing "+
				"this Dockerfile builds, and the linker ignores unresolvable -X targets "+
				"without erroring, so this would ship a binary that silently reports "+
				"the dev default.", symbol)
		}
	}
	if !found {
		t.Errorf("Dockerfile does not stamp main.buildGitSHA. Targets found: %v", matches)
	}
}

// The stamp is worthless if the field never reaches the response. This also
// pins the JSON key: the deploy workflow parses "git_sha", and renaming it
// would turn every future deploy verification into a false failure.
func TestHealthReportsGitSHA(t *testing.T) {
	rec := httptest.NewRecorder()
	handleHealth(quietLogger())(rec, httptest.NewRequest("GET", "/health", nil))

	if rec.Code != 200 {
		t.Fatalf("/health returned %d", rec.Code)
	}

	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("/health is not valid JSON: %v\nbody=%s", err, rec.Body.String())
	}

	got, present := body["git_sha"]
	if !present {
		t.Fatal("/health has no git_sha field; the deploy verification step parses " +
			"this key and would fail every deploy")
	}
	// In a test binary nothing is stamped, so "unknown" is the correct and
	// expected value. Asserting on it deliberately: a build that fabricated
	// a plausible-looking SHA here would be worse than one that admits it
	// does not know.
	if got != "unknown" {
		t.Errorf("unstamped build should report git_sha \"unknown\", got %q", got)
	}
}
