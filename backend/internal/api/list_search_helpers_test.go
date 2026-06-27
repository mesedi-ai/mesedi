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
