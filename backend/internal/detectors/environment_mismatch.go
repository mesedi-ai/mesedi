// Environment-misapprehension detector, the declared-boundary half.
//
// The operator declares what environment a run is SUPPOSED to be in
// (an environment_declaration event, mode "live" | "simulation" |
// "staging" | free-form). This detector compares that claim against
// the run's observed egress destinations and fires when a run
// declared NOT-live reached a live-looking destination. That is the
// direction that does damage: the January 2026 incident in
// Anthropic's September report was an eval that breached a real
// third-party machine.
//
// Honest scope, stated everywhere this class appears: this checks
// the DECLARED boundary against observed traffic. It says nothing
// about the model's beliefs, which live in reasoning text Mesedi
// deliberately does not ingest. A run declared "live" can never
// fire here, whatever the model thought it was doing.
package detectors

import (
	"net"
	"strings"
)

// DetectEnvironmentMismatch fires when declaredMode is non-empty,
// not "live", and any destination looks like the live internet.
// Signature shape: "env_mismatch:<mode>", one cluster per declared
// mode, so every simulation that leaks clusters together and the
// operator sees the boundary itself failing rather than a scatter
// of destinations.
func DetectEnvironmentMismatch(declaredMode string, destinations []string) (string, bool) {
	mode := strings.ToLower(strings.TrimSpace(declaredMode))
	if mode == "" || mode == "live" {
		return "", false
	}
	for _, d := range destinations {
		if isLiveDestination(d) {
			return "env_mismatch:" + mode, true
		}
	}
	return "", false
}

// isLiveDestination reports whether an egress destination (host or
// host:port, as the SDKs normalize) looks like the live internet
// rather than test or internal infrastructure.
//
// The judgment is deliberately conservative in BOTH directions:
// loopback, RFC 1918 and link-local addresses, .local/.internal/
// .test/.example/.localhost names, and bare single-label hostnames
// (compose services, cluster shortnames) all read as not-live, so a
// well-built simulation full of stubs cannot fire this detector.
// Everything else, a public IP or a dotted public hostname, reads
// as live.
func isLiveDestination(dest string) bool {
	d := strings.ToLower(strings.TrimSpace(dest))
	if d == "" {
		return false
	}
	// Strip a port if present. SplitHostPort demands one, so fall
	// back to the raw string when there is none. Bracketed IPv6
	// with a port is handled by SplitHostPort; bare IPv6 is left
	// as-is for the IP parse below.
	if host, _, err := net.SplitHostPort(d); err == nil {
		d = host
	}
	d = strings.Trim(d, "[]")

	if ip := net.ParseIP(d); ip != nil {
		return !(ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() ||
			ip.IsLinkLocalMulticast() || ip.IsUnspecified())
	}

	if d == "localhost" {
		return false
	}
	for _, suffix := range []string{".local", ".internal", ".test", ".example", ".invalid", ".localhost"} {
		if strings.HasSuffix(d, suffix) {
			return false
		}
	}
	// A bare single-label hostname ("db", "redis", "api-stub") is
	// internal naming, not the internet.
	if !strings.Contains(d, ".") {
		return false
	}
	return true
}
