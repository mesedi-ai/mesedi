package api

// rawJSONOrNil (chain_export.go) keeps a stored proof that will not parse
// out of an export rather than passing it on broken.

import "testing"

func TestRawJSONOrNilKeepsOnlyUsableProofs(t *testing.T) {
	if got := rawJSONOrNil(""); got != nil {
		t.Errorf("empty produced %q, want nil so the field is omitted entirely", got)
	}
	if got := rawJSONOrNil(`{"log_id":`); got != nil {
		t.Errorf("malformed JSON was passed through as %q. It would either corrupt "+
			"the enclosing export or reach a verifier as a broken proof, and the "+
			"only true statement about a proof that will not parse is that there "+
			"is not one", got)
	}
	if got := rawJSONOrNil(`{"log_id":"x"}`); string(got) != `{"log_id":"x"}` {
		t.Errorf("valid JSON was altered in transit: %s", got)
	}
}
