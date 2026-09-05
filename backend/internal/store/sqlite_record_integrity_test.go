package store

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"mesedi/backend/internal/events"
)

// GroupRecordIntegrity is a thin wrapper over groupExecutionInternal,
// and thin wrappers are exactly where a copy-paste slip hides: the
// wrong FailureClass constant produces a group that looks correct in
// every way except which bucket it lands in, and nothing downstream
// would complain. This test pins the class and the refusal.
//
// The Postgres twin is not tested here. Per the project's documented
// B18 exemption in .git/foundation_audit.conf, no store sidecar test
// has a Postgres twin yet, the SQLite in-memory pattern is the
// established style and the Postgres CI path is tracked as its own
// task rather than being re-litigated in each detector's commit.
func TestGroupRecordIntegrityUsesTheRecordIntegrityClass(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := OpenSQLite(filepath.Join(dir, "record_integrity.db"), logger)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	const (
		projectID   = "proj-record-integrity"
		executionID = "exec-record-integrity-1"
		signature   = "record_integrity:sequence_gap"
	)

	if err := s.CreateProject(ctx, &Project{
		ProjectID:  projectID,
		Name:       "record integrity",
		OwnerEmail: "ri@example.com",
		CreatedAt:  time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateProject: %v", err)
	}
	// Note the status: an execution with a hole in its event record can
	// have completed perfectly well. Seeding it as completed rather
	// than crashed keeps the test honest about what this class means ,
	// the record is incomplete, not the run.
	if err := s.CreateExecution(ctx, &events.Execution{
		ExecutionID: executionID,
		ProjectID:   projectID,
		Status:      events.StatusCompleted,
		StartedAt:   time.Now().UTC(),
	}); err != nil {
		t.Fatalf("CreateExecution: %v", err)
	}

	isNew, err := s.GroupRecordIntegrity(ctx, executionID, projectID, signature)
	if err != nil {
		t.Fatalf("GroupRecordIntegrity: %v", err)
	}
	if !isNew {
		t.Error("first occurrence of a signature must report isNew=true")
	}

	g, err := s.GetFailureGroupByClassSignature(
		ctx, projectID, FailureClassRecordIntegrity, signature,
	)
	if err != nil {
		t.Fatalf("GetFailureGroupByClassSignature: %v", err)
	}
	if g == nil {
		t.Fatal("no group created under failure_class=record_integrity, " +
			"the wrapper is grouping under the wrong class")
	}
	if g.FailureClass != FailureClassRecordIntegrity {
		t.Errorf("failure_class = %q, want %q", g.FailureClass,
			FailureClassRecordIntegrity)
	}
	if g.Signature != signature {
		t.Errorf("signature = %q, want %q", g.Signature, signature)
	}

	// Second occurrence of the same signature is a recurrence, not a
	// new group. Guards against a wrapper that accidentally creates a
	// fresh group per call and inflates the customer's group list.
	isNew, err = s.GroupRecordIntegrity(ctx, executionID, projectID, signature)
	if err != nil {
		t.Fatalf("GroupRecordIntegrity (recurrence): %v", err)
	}
	if isNew {
		t.Error("a repeat of an existing signature must report isNew=false")
	}
}

// An empty signature must be refused rather than written. A group keyed
// on the empty string would silently collect every future integrity
// failure whose signature construction went wrong, which is worse than
// an error because it looks like data.
func TestGroupRecordIntegrityRefusesAnEmptySignature(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	dir := t.TempDir()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	s, err := OpenSQLite(filepath.Join(dir, "record_integrity_empty.db"), logger)
	if err != nil {
		t.Fatalf("OpenSQLite: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })

	if _, err := s.GroupRecordIntegrity(ctx, "exec-x", "proj-x", ""); err == nil {
		t.Fatal("empty signature must be refused")
	}
}
