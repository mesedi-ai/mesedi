package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/pem"
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

func runChain(t *testing.T, keyPEM, rekorURL string) *safeBuffer {
	t.Helper()
	t.Setenv("MESEDI_ANCHOR_SIGNING_KEY", keyPEM)
	t.Setenv("MESEDI_REKOR_URL", rekorURL)

	logs := &safeBuffer{}
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// Cancelled at the end of the test rather than immediately, so the
	// enabled case really does start a goroutine we can observe. The
	// scheduler waits 30 seconds before its first tick, so a test that
	// finishes in milliseconds never reaches any store call or the log.
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	startCheckpointChain(ctx, &stubChainStore{}, logger)
	return logs
}

func testKeyPEM(t *testing.T) string {
	t.Helper()
	k, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, err := x509.MarshalPKCS8PrivateKey(k)
	if err != nil {
		t.Fatal(err)
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}))
}

func TestCheckpointChainDisabledWhenUnconfiguredOrMisconfigured(t *testing.T) {
	cases := []struct {
		name, key, wantReason string
	}{
		{"no key", "", "MESEDI_ANCHOR_SIGNING_KEY is not set"},
		{"whitespace only", " \t\n", "MESEDI_ANCHOR_SIGNING_KEY is not set"},
		{"not a key", "-----BEGIN PRIVATE KEY-----\nnope\n-----END PRIVATE KEY-----", "signing key"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			logs := runChain(t, tc.key, "")
			out := logs.String()
			if !strings.Contains(out, "checkpoint chain disabled") || !strings.Contains(out, tc.wantReason) {
				t.Errorf("expected the disabled line naming %q, got:\n%s", tc.wantReason, out)
			}
			if strings.Contains(out, "checkpoint chain enabled") {
				t.Errorf("chain reported itself enabled without a usable key:\n%s", out)
			}
			// The scheduler goroutine must never come up without an anchorer.
			if waitForLog(logs, "checkpoint_scheduler: started", 200*time.Millisecond) {
				t.Errorf("scheduler goroutine started without a usable key:\n%s", out)
			}
		})
	}
}

func TestCheckpointChainEnabledWithASigningKey(t *testing.T) {
	logs := runChain(t, testKeyPEM(t), "https://rekor.example.test")
	if !waitForLog(logs, "checkpoint_scheduler: started", 2*time.Second) {
		t.Fatalf("scheduler goroutine never started:\n%s", logs.String())
	}
	out := logs.String()
	for _, want := range []string{"checkpoint chain enabled", "https://rekor.example.test", "signing_key_sha256=", "anchorer_configured=true"} {
		if !strings.Contains(out, want) {
			t.Errorf("enabled log lacks %q:\n%s", want, out)
		}
	}
}

// The private key is the one secret that makes an anchor Mesedi's. Startup
// logs land in the host's log stream and any aggregator attached to it; a
// key that appears there is disclosed, and only rotating it repairs that.
func TestCheckpointChainNeverLogsTheSigningKey(t *testing.T) {
	key := testKeyPEM(t)
	logs := runChain(t, key, "")
	if !waitForLog(logs, "checkpoint_scheduler: started", 2*time.Second) {
		t.Fatalf("scheduler goroutine never started:\n%s", logs.String())
	}
	body := strings.Split(key, "\n")[1]
	if strings.Contains(logs.String(), body) {
		t.Errorf("the signing key was written to the log:\n%s", logs.String())
	}
	broken := strings.Replace(key, body, body[:20]+"!!"+body[22:], 1)
	logs = runChain(t, broken, "")
	if strings.Contains(logs.String(), body[:20]) {
		t.Errorf("a rejected key was echoed in the log:\n%s", logs.String())
	}
}
