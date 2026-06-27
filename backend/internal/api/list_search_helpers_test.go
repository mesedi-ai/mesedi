// Tests for normalizeListQuery — the input sanitizer for the
// ?q= query parameter on GET /failure-groups + GET /executions
// (list-search-paginate wave). SQL parameterization is the
// injection defense; this helper handles whitespace, control
// chars, and bounded length so the response stays sane.

package api

import (
	"strings"
	"testing"
)

func TestNormalizeListQuery(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \t  ", ""},
		{"trim outer whitespace", "  hello  ", "hello"},
		{"preserve inner whitespace", "foo bar", "foo bar"},
		{"strip null byte", "ab\x00cd", "abcd"},
		{"strip control chars", "a\x01b\x1fc\x7fd", "abcd"},
		{"keep printable unicode", "héllo wörld", "héllo wörld"},
		{"keep digits and punctuation", "ValueError:foo_bar(123)", "ValueError:foo_bar(123)"},
		{"cap at 256 bytes", strings.Repeat("a", 300), strings.Repeat("a", 256)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeListQuery(tc.in)
			if got != tc.want {
				t.Errorf("normalizeListQuery(%q) = %q (len %d), want %q (len %d)",
					tc.in, got, len(got), tc.want, len(tc.want))
			}
		})
	}
}

// TestNormalizeResolveReason covers the optional customer-supplied
// reason on a resolve / unresolve action (failure-group-resolve-
// context wave). Same posture as normalizeListQuery but with a 512
// cap (reasons can be a sentence or two) and preserves tab/newline
// so multi-line reasons render correctly in the audit log Detail
// column.
func TestNormalizeResolveReason(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"empty", "", ""},
		{"whitespace only", "   \n  ", ""},
		{"trim outer whitespace", "  fixed in v0.5.1  ", "fixed in v0.5.1"},
		{"preserve newlines", "line one\nline two", "line one\nline two"},
		{"preserve tabs", "col1\tcol2", "col1\tcol2"},
		{"strip null byte", "ab\x00cd", "abcd"},
		{"strip control chars", "a\x01b\x1fc\x7fd", "abcd"},
		{"keep printable unicode", "résolu — voir #1234", "résolu — voir #1234"},
		{"cap at 512 bytes", strings.Repeat("a", 600), strings.Repeat("a", 512)},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := normalizeResolveReason(tc.in)
			if got != tc.want {
				t.Errorf("normalizeResolveReason(%q) = %q (len %d), want %q (len %d)",
					tc.in, got, len(got), tc.want, len(tc.want))
			}
		})
	}
}
