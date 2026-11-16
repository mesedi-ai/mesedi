package severity

import "testing"

// The SDK vocabulary maps onto the backend's three tiers. "info" was
// added 2026-11-16: the docs told people to send it, but it mapped to
// Warning here and both SDKs turned it into "error" (critical) before it
// arrived (the outside review of 2026-11-15).
func TestFromSDKValue(t *testing.T) {
	cases := map[string]Severity{
		"info": Info, "INFO": Info, " info ": Info,
		"warning":  Warning,
		"error":    Critical,
		"critical": Critical,
		"":         Warning,
		"fatal":    Warning,
	}
	for in, want := range cases {
		if got := FromSDKValue(in); got != want {
			t.Errorf("FromSDKValue(%q) = %v, want %v", in, got, want)
		}
	}
}
