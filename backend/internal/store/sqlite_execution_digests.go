package store

// SQLiteStore side of ExecutionDigestStore. The bodies live once, in
// store_execution_digests.go, so the two databases cannot drift.

import (
	"context"
	"time"
)

func (s *SQLiteStore) digests() digestRepo { return digestRepo{db: s.db, pg: false} }

func (s *SQLiteStore) ListExecutionDigests(ctx context.Context, projectID string, seq uint64) ([]ExecutionDigest, error) {
	return s.digests().listExecutionDigests(ctx, projectID, seq)
}

func (s *SQLiteStore) ListLeavesMissingDigests(ctx context.Context, limit int) ([]LeafMissingDigests, error) {
	return s.digests().listLeavesMissingDigests(ctx, limit)
}

func (s *SQLiteStore) StoreHistoricalDigests(ctx context.Context, seq uint64, projectID string, digests []ExecutionDigest) error {
	return s.digests().storeHistoricalDigests(ctx, seq, projectID, digests)
}

func (s *SQLiteStore) RecordDigestHistoryGap(ctx context.Context, seq uint64, projectID, reason string, at time.Time) error {
	return s.digests().recordDigestHistoryGap(ctx, seq, projectID, reason, at)
}

func (s *SQLiteStore) GetDigestHistoryGap(ctx context.Context, seq uint64, projectID string) (string, bool, error) {
	return s.digests().getDigestHistoryGap(ctx, seq, projectID)
}

func (s *SQLiteStore) ExistingExecutionIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	return s.digests().existingExecutionIDs(ctx, ids)
}

func (s *SQLiteStore) CountEventsAfterDigest(ctx context.Context, executionIDs []string) (map[string]int, error) {
	return s.digests().countEventsAfterDigest(ctx, executionIDs)
}
