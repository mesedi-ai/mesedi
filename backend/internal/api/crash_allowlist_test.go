package api

// The crash allowlist matches the exception type the SDK sends as
// crash_type (from 2026-11-16), and still matches the crash signature hash.
// Until then only the hash was compared, so an entry such as "ValueError",
// which the docs tell people to write, could never match (outside review
// the outside review of 2026-11-15).

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/store"
)

func crashAllowlistFixture(t *testing.T, allowKey string) (*Handlers, *store.SQLiteStore, string) {
	t.Helper()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	st, err := store.OpenSQLite(":memory:", logger)
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}
	ctx := context.Background()
	const projectID = "proj_crash_allow"
	if err := st.CreateProject(ctx, &store.Project{ProjectID: projectID, Name: "crash allowlist", Tier: "hobby", CreatedAt: time.Now().UTC()}); err != nil {
		t.Fatal(err)
	}
	if allowKey != "" {
		if err := st.CreateProjectAllowlistEntry(ctx, &store.AllowlistEntry{
			AllowlistID: "allow_1", ProjectID: projectID, Detector: "crashes",
			AllowlistKey: allowKey, CreatedAt: time.Now().UTC().Format(time.RFC3339),
		}); err != nil {
			t.Fatalf("create allowlist entry: %v", err)
		}
	}
	h := &Handlers{Logger: logger, Store: st, HaltSubs: NewHaltSubscribers()}
	t.Cleanup(func() { h.DrainDispatches(); _ = st.Close() })
	return h, st, projectID
}

func patchCrashed(t *testing.T, h *Handlers, st *store.SQLiteStore, projectID, executionID, body string) {
	t.Helper()
	if err := st.CreateExecution(context.Background(), &events.Execution{
		ExecutionID: executionID, ProjectID: projectID,
		Status: events.StatusStarted, StartedAt: time.Now().UTC().Add(-time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest("PATCH", "/executions/"+executionID, strings.NewReader(body))
	req.SetPathValue("id", executionID)
	req = req.WithContext(context.WithValue(req.Context(), ctxKeyProjectID, projectID))
	rec := httptest.NewRecorder()
	h.HandleUpdateExecution(rec, req)
	if rec.Code != 200 {
		t.Fatalf("PATCH returned %d: %s", rec.Code, rec.Body.String())
	}
}

func crashGroups(t *testing.T, st *store.SQLiteStore, projectID string) int {
	t.Helper()
	groups, err := st.ListFailureGroups(context.Background(), projectID, store.ListFailureGroupsOpts{Limit: 50})
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, g := range groups {
		if g.FailureClass == store.FailureClassCrashes {
			n++
		}
	}
	return n
}

func TestCrashAllowlistMatchesExceptionType(t *testing.T) {
	h, st, p := crashAllowlistFixture(t, "ValueError")
	patchCrashed(t, h, st, p, "exec_type",
		`{"status":"crashed","crash_signature":"0123456789abcdef","crash_type":"ValueError"}`)
	if n := crashGroups(t, st, p); n != 0 {
		t.Fatalf("an allowlisted exception type still produced %d crash group(s)", n)
	}
}

func TestCrashAllowlistStillMatchesSignatureHash(t *testing.T) {
	h, st, p := crashAllowlistFixture(t, "0123456789abcdef")
	patchCrashed(t, h, st, p, "exec_hash",
		`{"status":"crashed","crash_signature":"0123456789abcdef","crash_type":"KeyError"}`)
	if n := crashGroups(t, st, p); n != 0 {
		t.Fatalf("an allowlisted signature hash still produced %d crash group(s)", n)
	}
}

func TestCrashAllowlistOtherTypeIsGrouped(t *testing.T) {
	h, st, p := crashAllowlistFixture(t, "ValueError")
	patchCrashed(t, h, st, p, "exec_other",
		`{"status":"crashed","crash_signature":"fedcba9876543210","crash_type":"KeyError"}`)
	if n := crashGroups(t, st, p); n != 1 {
		t.Fatalf("a crash that is not allowlisted produced %d crash group(s), want 1", n)
	}
}

func TestCrashWithoutTypeFromOlderSDKIsGrouped(t *testing.T) {
	// SDKs before 2026-11-16 send no crash_type; nothing changes for them.
	h, st, p := crashAllowlistFixture(t, "ValueError")
	patchCrashed(t, h, st, p, "exec_old_sdk",
		`{"status":"crashed","crash_signature":"fedcba9876543210"}`)
	if n := crashGroups(t, st, p); n != 1 {
		t.Fatalf("an older SDK's crash produced %d crash group(s), want 1", n)
	}
}
