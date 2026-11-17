package store

// PostgresStore side of ExecutionDigestStore. The bodies live once, in
// store_execution_digests.go, so the two databases cannot drift.

import (
	"context"
	"time"
)

func (s *PostgresStore) digests() digestRepo { return digestRepo{db: s.db, pg: true} }

func (s *PostgresStore) ListExecutionDigests(ctx context.Context, projectID string, seq uint64) ([]ExecutionDigest, error) {
	return s.digests().listExecutionDigests(ctx, projectID, seq)
}

func (s *PostgresStore) ListLeavesMissingDigests(ctx context.Context, limit int) ([]LeafMissingDigests, error) {
	return s.digests().listLeavesMissingDigests(ctx, limit)
}

func (s *PostgresStore) StoreHistoricalDigests(ctx context.Context, seq uint64, projectID string, digests []ExecutionDigest) error {
	return s.digests().storeHistoricalDigests(ctx, seq, projectID, digests)
}

func (s *PostgresStore) RecordDigestHistoryGap(ctx context.Context, seq uint64, projectID, reason string, at time.Time) error {
	return s.digests().recordDigestHistoryGap(ctx, seq, projectID, reason, at)
}

func (s *PostgresStore) GetDigestHistoryGap(ctx context.Context, seq uint64, projectID string) (string, bool, error) {
	return s.digests().getDigestHistoryGap(ctx, seq, projectID)
}

func (s *PostgresStore) ExistingExecutionIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	return s.digests().existingExecutionIDs(ctx, ids)
}

func (s *PostgresStore) CountEventsAfterDigest(ctx context.Context, executionIDs []string) (map[string]int, error) {
	return s.digests().countEventsAfterDigest(ctx, executionIDs)
}

func (s *PostgresStore) ListReportChainStatuses(ctx context.Context, executionIDs []string) (map[string]string, error) {
	return s.digests().listReportChainStatuses(ctx, executionIDs)
}
