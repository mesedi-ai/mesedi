package main

import (
	"strings"
	"testing"
)

// The report instructs its reader to rebuild this binary and reproduce
// the verdict. These tests exist because that instruction is only
// meaningful if the document names the source, and because the case
// that matters most, a build with uncommitted changes, is the one a
// developer sees every day and would stop noticing.

func TestFormatVersionNamesTheSource(t *testing.T) {
	const rev = "a1b2c3d4e5f60718293a"

	cases := []struct {
		name     string
		stamped  string
		rev      string
		modified string
		want     string
	}{
		{
			name:    "a release wins over everything",
			stamped: "v1.4.0", rev: rev, modified: "true",
			want: "v1.4.0",
		},
		{
			name:    "a clean checkout reports its commit",
			stamped: "unknown", rev: rev, modified: "false",
			want: "a1b2c3d4e5f6",
		},
		{
			name:    "an empty stamp is treated as unstamped",
			stamped: "", rev: rev, modified: "false",
			want: "a1b2c3d4e5f6",
		},
		{
			name:    "a dirty checkout says so",
			stamped: "unknown", rev: rev, modified: "true",
			want: "a1b2c3d4e5f6 + UNCOMMITTED CHANGES",
		},
		{
			name:    "no vcs information at all",
			stamped: "unknown", rev: "", modified: "",
			want: "unknown (built outside a source checkout)",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := formatVersion(tc.stamped, tc.rev, tc.modified); got != tc.want {
				t.Errorf("formatVersion(%q, %q, %q) = %q, want %q",
					tc.stamped, tc.rev, tc.modified, got, tc.want)
			}
		})
	}
}

// The predicate decides whether the report prints a caveat about its own
// reproducibility, so getting it backwards would silently remove a
// disclosure rather than break anything visible.
func TestVersionIsReproducible(t *testing.T) {
	reproducible := []string{"v1.4.0", "a1b2c3d4e5f6"}
	for _, v := range reproducible {
		if !versionIsReproducible(v) {
			t.Errorf("%q names obtainable source but was reported as not reproducible", v)
		}
	}

	not := []string{
		"a1b2c3d4e5f6 + UNCOMMITTED CHANGES",
		"unknown (built outside a source checkout)",
		"unknown",
	}
	for _, v := range not {
		if versionIsReproducible(v) {
			t.Errorf("%q does not name obtainable source but passed as reproducible", v)
		}
	}
}

// resolveVersion runs against whatever built the test binary. It cannot
// assert a specific value, but it can assert the thing that would
// actually hurt: silently returning nothing, so the PDF's Verifier line
// comes out blank.
func TestResolveVersionIsNeverEmpty(t *testing.T) {
	got := resolveVersion()
	if strings.TrimSpace(got) == "" {
		t.Fatal("resolveVersion returned nothing; the report would name no verifier at all")
	}
	t.Logf("this test binary reports version %q", got)
}
