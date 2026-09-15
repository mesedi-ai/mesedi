package detectors

import "testing"

// The live/not-live judgment IS the detector; every boundary here is
// a false-positive or false-negative in production. Conservative in
// both directions by design: a well-built simulation full of stubs
// must not fire, and a declared simulation touching the public
// internet must.
func TestIsLiveDestinationJudgment(t *testing.T) {
	notLive := []string{
		"", "localhost", "localhost:8080",
		"127.0.0.1", "127.0.0.1:443", "0.0.0.0",
		"10.2.3.4", "10.2.3.4:8443", "192.168.1.10", "172.16.0.9", "172.31.255.1",
		"169.254.169.254", // instance metadata is link-local, not the internet
		"db", "redis:6379", "api-stub",
		"vault.internal", "printer.local", "sut.test", "demo.example", "x.invalid",
		"[::1]:443",
	}
	for _, d := range notLive {
		if isLiveDestination(d) {
			t.Errorf("isLiveDestination(%q) = true, want false", d)
		}
	}
	live := []string{
		"api.example-corp.com", "api.example-corp.com:443",
		"8.8.8.8", "8.8.8.8:53", "172.32.0.1", // just past the RFC1918 172 range
		"payments.stripe.com",
	}
	for _, d := range live {
		if !isLiveDestination(d) {
			t.Errorf("isLiveDestination(%q) = false, want true", d)
		}
	}
}

func TestDetectEnvironmentMismatchFiresOnlyForDeclaredNonLive(t *testing.T) {
	liveDest := []string{"api.example-corp.com"}
	stubDest := []string{"stub.internal", "db:5432"}

	if sig, fired := DetectEnvironmentMismatch("simulation", liveDest); !fired || sig != "env_mismatch:simulation" {
		t.Fatalf("declared simulation reaching live = (%q, %v), want fire", sig, fired)
	}
	if _, fired := DetectEnvironmentMismatch("Simulation", liveDest); !fired {
		t.Fatal("mode comparison must be case-insensitive")
	}
	if _, fired := DetectEnvironmentMismatch("simulation", stubDest); fired {
		t.Fatal("a simulation touching only stubs must not fire")
	}
	if _, fired := DetectEnvironmentMismatch("live", liveDest); fired {
		t.Fatal("a declared-live run can never fire, whatever it touches")
	}
	if _, fired := DetectEnvironmentMismatch("", liveDest); fired {
		t.Fatal("no declaration means no boundary to violate")
	}
	if _, fired := DetectEnvironmentMismatch("simulation", nil); fired {
		t.Fatal("no egress means nothing crossed the boundary")
	}
}

func TestDetectCovertCoordinationThreshold(t *testing.T) {
	if sig, fired := DetectCovertCoordination("drop.example-corp.com", 3, 3); !fired ||
		sig != "covert_coordination:drop.example-corp.com" {
		t.Fatalf("at threshold = (%q, %v), want fire", sig, fired)
	}
	if _, fired := DetectCovertCoordination("drop.example-corp.com", 2, 3); fired {
		t.Fatal("below threshold must not fire")
	}
	if _, fired := DetectCovertCoordination("", 10, 3); fired {
		t.Fatal("empty destination must not fire")
	}
	if _, fired := DetectCovertCoordination("drop.example-corp.com", 10, 0); fired {
		t.Fatal("non-positive threshold means the store default was bypassed; never fire")
	}
}
