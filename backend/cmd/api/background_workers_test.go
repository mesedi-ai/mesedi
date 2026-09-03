package main

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// safeBuffer is a bytes.Buffer that survives concurrent writes. The
// scheduler logs from its own goroutine while the test reads, so an
// unguarded buffer here is a data race that -race would (correctly) fail.
type safeBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *safeBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *safeBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

// stubChainStore satisfies store.Store without implementing anything.
// The embedded nil interface panics on any call, which is the point: these
// tests assert that the scheduler either does not start, or starts and is
// cancelled before its first tick. Either way it must not touch the store,
// and a panic is a much clearer failure than a silently tolerated call.
type stubChainStore struct {
	store.Store
}

// waitForLog polls until want appears in the log or the deadline passes.
// The scheduler logs "checkpoint_scheduler: started" from a goroutine, so
// the enabled case is inherently asynchronous. Polling rather than sleeping
// a fixed duration keeps the test fast when it passes and still gives a
// loaded CI machine room before it fails.
func waitForLog(logs *safeBuffer, want string, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if strings.Contains(logs.String(), want) {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return strings.Contains(logs.String(), want)
}

func runChain(t *testing.T, baseURL, apiKey string) *safeBuffer {
	t.Helper()
	t.Setenv("VERDIFAX_BASE_URL", baseURL)
	t.Setenv("VERDIFAX_API_KEY", apiKey)

	logs := &safeBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Cancelled at the end of the test rather than immediately, so the
	// enabled case really does start a goroutine we can observe. The
	// scheduler waits 30 seconds before its first tick, so a test that
	// finishes in milliseconds never reaches any store call.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	startCheckpointChain(ctx, &stubChainStore{}, logger)
	return logs
}

func TestCheckpointChainDisabledWhenUnconfigured(t *testing.T) {
	cases := []struct {
		name    string
		baseURL string
		apiKey  string
	}{
		{"neither set", "", ""},
		{"base URL only", "https://api.verdifax.com", ""},
		{"api key only", "", "vfx_live_whatever"},
		{"whitespace only", "   ", "\t"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := runChain(t, tc.baseURL, tc.apiKey)

			out := logs.String()
			if !strings.Contains(out, "checkpoint chain disabled") {
				t.Errorf("expected the disabled log line, got:\n%s", out)
			}
			if strings.Contains(out, "checkpoint chain enabled") {
				t.Errorf("chain reported itself enabled with incomplete config:\n%s", out)
			}

			// The load-bearing assertion. NewVerdifaxAnchorer returns a
			// typed nil *VerdifaxAnchorer, and CheckpointScheduler.Anchorer
			// is an interface. Assigning a typed nil pointer to an interface
			// produces a NON-NIL interface, so a refactor that stores the
			// result first and nil-checks the interface would start the
			// scheduler with an anchorer that panics on first use — 30
			// seconds after boot, in production, with nothing else wrong.
			// startCheckpointChain nil-checks the concrete pointer before
			// the assignment precisely to avoid that. This asserts the
			// goroutine never came up at all.
			if waitForLog(logs, "checkpoint_scheduler: started", 200*time.Millisecond) {
				t.Errorf("scheduler goroutine started despite incomplete config:\n%s", logs.String())
			}
		})
	}
}

func TestCheckpointChainEnabledWhenFullyConfigured(t *testing.T) {
	logs := runChain(t, "https://api.verdifax.com", "vfx_live_secret_value")

	if !waitForLog(logs, "checkpoint_scheduler: started", 2*time.Second) {
		t.Fatalf("scheduler goroutine never started:\n%s", logs.String())
	}

	out := logs.String()
	if !strings.Contains(out, "checkpoint chain enabled") {
		t.Errorf("expected the enabled log line, got:\n%s", out)
	}
	if strings.Contains(out, "checkpoint chain disabled") {
		t.Errorf("chain reported itself disabled with complete config:\n%s", out)
	}
	if !strings.Contains(out, "https://api.verdifax.com") {
		t.Errorf("enabled line should name which Verdifax it points at, got:\n%s", out)
	}
	if !strings.Contains(out, "anchorer_configured=true") {
		t.Errorf("scheduler should see a non-nil anchorer, got:\n%s", out)
	}
}

// The API key is a credential. Startup logs are the most widely shipped,
// least access-controlled output the process produces — they land in Fly's
// log stream and in whatever aggregator is attached. A key that appears
// there is disclosed, and rotating it is the only remedy.
func TestCheckpointChainNeverLogsTheAPIKey(t *testing.T) {
	const secret = "vfx_live_this_must_never_appear_in_logs"

	logs := runChain(t, "https://api.verdifax.com", secret)
	if !waitForLog(logs, "checkpoint_scheduler: started", 2*time.Second) {
		t.Fatalf("scheduler goroutine never started:\n%s", logs.String())
	}

	if strings.Contains(logs.String(), secret) {
		t.Errorf("the Verdifax API key was written to the log:\n%s", logs.String())
	}
}
