package anthropic

import (
	"encoding/json"
	"testing"
)

// claude-sonnet-5 rejects `temperature` with a 400. It is the model
// the hand-sold tiers use for AI root-cause analysis, so sending the
// parameter takes the entire feature down for Production and
// Enterprise customers, which is exactly what happened on
// 2026-08-24 before this guard existed.
func TestModelSupportsTemperature_Sonnet5Rejects(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"claude-sonnet-5",
		"claude-sonnet-5-20260101", // dated snapshot must match too
		"CLAUDE-SONNET-5",          // case-insensitive
		"  claude-sonnet-5  ",      // trimmed
	} {
		if ModelSupportsTemperature(id) {
			t.Errorf("%q: expected temperature UNSUPPORTED", id)
		}
	}
}

// Everything else keeps the historical behaviour. Defaulting to
// "supported" is deliberate: silently dropping temperature everywhere
// would change analysis output for the entire self-serve customer
// base, which is the majority of traffic.
func TestModelSupportsTemperature_OthersAllowIt(t *testing.T) {
	t.Parallel()
	for _, id := range []string{
		"claude-haiku-4-5",
		"claude-haiku-4-5-snapshot-20251001",
		"claude-sonnet-4",
		"claude-sonnet-4-6",
		"claude-opus-4",
		"claude-opus-4-6",
		"some-unknown-future-model",
	} {
		if !ModelSupportsTemperature(id) {
			t.Errorf("%q: expected temperature SUPPORTED", id)
		}
	}
}

// The guard has to actually keep the field out of the wire payload ,
// asserting the helper alone would not catch a call site that ignores
// it. `temperature` carries omitempty on a *float64, so "absent" means
// the key is missing entirely, not present-and-zero.
func TestRequestBodyOmitsTemperatureWhenUnsupported(t *testing.T) {
	t.Parallel()

	withTemp := 0.2
	supported := requestBody{
		Model:       "claude-haiku-4-5",
		MaxTokens:   1024,
		Temperature: &withTemp,
	}
	raw, err := json.Marshal(&supported)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["temperature"]; !present {
		t.Error("haiku: temperature should be present on the wire")
	}

	unsupported := requestBody{
		Model:     "claude-sonnet-5",
		MaxTokens: 1024,
		// Temperature deliberately nil, as the Call guard leaves it.
	}
	raw, err = json.Marshal(&unsupported)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = map[string]any{}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if _, present := got["temperature"]; present {
		t.Error("sonnet-5: temperature must be ABSENT from the wire " +
			"payload, the API 400s on it, which 502s the analysis")
	}
}
