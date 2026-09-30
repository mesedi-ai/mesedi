package detectors

import "testing"

func TestDetectPinnedContractDrift(t *testing.T) {
	const (
		pinned  = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
		current = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	)
	cases := []struct {
		name       string
		tool, kind string
		cur, pin   string
		wantSig    string
		wantFired  bool
	}{
		{"matching hash never fires", "t", PinKindDefinition, pinned, pinned, "", false},
		{"empty current is inconclusive, not a violation", "t", PinKindDefinition, "", pinned, "", false},
		{"empty pin means nothing approved", "t", PinKindDefinition, current, "", "", false},
		{"empty tool name declines", "", PinKindDefinition, current, pinned, "", false},
		{"definition violation carries pin:def and the current hash prefix",
			"crm_lookup", PinKindDefinition, current, pinned, "crm_lookup:pin:def:bbbbbbbb", true},
		{"description violation carries pin:desc",
			"crm_lookup", PinKindDescription, current, pinned, "crm_lookup:pin:desc:bbbbbbbb", true},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			sig, fired := DetectPinnedContractDrift(c.tool, c.kind, c.cur, c.pin)
			if fired != c.wantFired || sig != c.wantSig {
				t.Errorf("DetectPinnedContractDrift(%q,%q,%q,%q) = (%q,%v), want (%q,%v)",
					c.tool, c.kind, c.cur, c.pin, sig, fired, c.wantSig, c.wantFired)
			}
		})
	}
}
