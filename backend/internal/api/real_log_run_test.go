package api

// A run against the REAL public log and calendars. Skipped unless
// MESEDI_REAL_LOG_RUN=1, so CI and ordinary test runs never publish.
//
// Owner decision, 16 November 2026: before release, and again before
// restart, anchor a few hours on Sigstore's real Rekor log and the real
// OpenTimestamps calendars, signed by a throwaway key, then check them
// with mesedi-verify (MESEDI-DIRECT-ANCHORING-DESIGN-2026-10-21, order of
// work, step 5). Every entry it makes is public and permanent; each holds
// only a hash of a test checkpoint.
//
//	MESEDI_REAL_LOG_RUN=1 MESEDI_REAL_LOG_DIR=/some/dir \
//	  go test -count=1 -run TestRealLogRun -v ./internal/api/
//
// Writes, into MESEDI_REAL_LOG_DIR: chain.db (keep it), export.json and
// test-key.pub.pem. Verify with
//
//	mesedi-verify --trusted-key test-key.pub.pem export.json
//	mesedi-verify --offline --trusted-key test-key.pub.pem export.json
//
// Hours later, once the calendars have reached Bitcoin, the same command
// with MESEDI_REAL_LOG_UPGRADE=1 upgrades the receipts in chain.db and
// writes a fresh export.json.

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

func TestRealLogRun(t *testing.T) {
	if os.Getenv("MESEDI_REAL_LOG_RUN") != "1" {
		t.Skip("publishes to the real public log; set MESEDI_REAL_LOG_RUN=1 to run")
	}
	dir := os.Getenv("MESEDI_REAL_LOG_DIR")
	if dir == "" {
		t.Fatal("set MESEDI_REAL_LOG_DIR to a folder to keep chain.db and export.json in")
	}
	ctx := context.Background()
	lg := slog.New(slog.NewTextHandler(os.Stderr, nil))
	st, err := store.OpenSQLite("file:"+filepath.Join(dir, "chain.db"), lg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	calendars, err := ParseOTSCalendars(os.Getenv("MESEDI_OTS_CALENDARS"))
	if err != nil {
		t.Fatal(err)
	}
	ts := &CheckpointTimestamper{Store: st, Calendars: calendars, Logger: lg}
	h := &Handlers{Store: st, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), HaltSubs: NewHaltSubscribers()}
	t.Cleanup(func() { h.DrainDispatches() })

	if os.Getenv("MESEDI_REAL_LOG_UPGRADE") == "1" {
		ts.Run(ctx)
		writeRealLogExport(t, ctx, h, dir)
		return
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	der, _ := x509.MarshalPKCS8PrivateKey(key)
	anchorer, err := NewDirectRekorAnchorer(os.Getenv("MESEDI_REKOR_URL"),
		pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), lg)
	if err != nil {
		t.Fatal(err)
	}
	pubPEM, _ := AnchorPublicKeyPEM(&key.PublicKey)
	if err := os.WriteFile(filepath.Join(dir, "test-key.pub.pem"), pubPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("throwaway key %s", AnchorKeyFingerprint(&key.PublicKey))

	if err := st.CreateProject(ctx, &store.Project{ProjectID: "proj-real-log", Name: "real log run", CreatedAt: time.Now()}); err != nil {
		t.Fatal(err)
	}
	var clock time.Time
	sch := &CheckpointScheduler{Store: st, Logger: lg, Anchorer: anchorer, Timestamper: ts,
		Now: func() time.Time { return clock }}

	// One agent run in each of the three hours before this one. Ticking
	// half an hour into each seals that hour's run; the next tick closes
	// and anchors the hour. The first tick also anchors an empty genesis
	// hour.
	now := time.Now().UTC()
	top := now.Truncate(time.Hour)
	for i := 3; i >= 1; i-- {
		hourStart := top.Add(-time.Duration(i) * time.Hour)
		exec := "exec-real-log-" + hourStart.Format("1504")
		end := hourStart.Add(5 * time.Minute)
		if err := st.CreateExecution(ctx, &events.Execution{ExecutionID: exec, ProjectID: "proj-real-log",
			Status: "completed", StartedAt: hourStart.Add(time.Minute), EndedAt: &end}); err != nil {
			t.Fatal(err)
		}
		if err := st.SaveEvents(ctx, []events.Event{{EventID: exec + "-e1", ExecutionID: exec,
			EventType: events.EventTypeLLMCall, Sequence: 1, Timestamp: hourStart.Add(2 * time.Minute),
			Payload: []byte(`{"note":"mesedi real-log test run"}`)}}); err != nil {
			t.Fatal(err)
		}
		clock = hourStart.Add(30 * time.Minute)
		if err := sch.tick(ctx); err != nil {
			t.Fatal(err)
		}
	}
	clock = now
	if err := sch.tick(ctx); err != nil {
		t.Fatal(err)
	}
	head, err := st.LatestCheckpoint(ctx)
	if err != nil || head == nil {
		t.Fatalf("no checkpoint: %v", err)
	}
	for seq := uint64(1); seq <= head.Seq; seq++ {
		a, err := st.GetCheckpointAnchor(ctx, seq)
		if err != nil || !a.Anchored {
			t.Fatalf("checkpoint %d not anchored: %v", seq, err)
		}
		t.Logf("checkpoint %d anchored at log index %s", seq, a.LogEntryID)
	}
	writeRealLogExport(t, ctx, h, dir)
}

func writeRealLogExport(t *testing.T, ctx context.Context, h *Handlers, dir string) {
	t.Helper()
	head, err := h.Store.LatestCheckpoint(ctx)
	if err != nil || head == nil {
		t.Fatalf("no checkpoint: %v", err)
	}
	exp, err := h.buildChainExport(ctx, "proj-real-log", 1, head.Seq)
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.MarshalIndent(exp, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "export.json"), raw, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Logf("wrote %s (%d checkpoints)", filepath.Join(dir, "export.json"), len(exp.Intervals))
}
