package store

import (
	"context"
	"time"
)

func (s *SQLiteStore) RecordCheckpointTimestamp(ctx context.Context, t CheckpointTimestamp) error {
	return s.digests().recordCheckpointTimestamp(ctx, t)
}

func (s *SQLiteStore) CompleteCheckpointTimestamp(ctx context.Context, seq uint64, calendarURL string,
	receipt []byte, at time.Time) error {
	return s.digests().completeCheckpointTimestamp(ctx, seq, calendarURL, receipt, at)
}

func (s *SQLiteStore) ListPendingTimestamps(ctx context.Context, limit int) ([]CheckpointTimestamp, error) {
	return s.digests().listPendingTimestamps(ctx, limit)
}

func (s *SQLiteStore) ListUntimestampedCheckpoints(ctx context.Context, limit int) ([]UntimestampedCheckpoint, error) {
	return s.digests().listUntimestampedCheckpoints(ctx, limit)
}

func (s *SQLiteStore) ListCheckpointTimestamps(ctx context.Context, fromSeq, toSeq uint64) (map[uint64][]CheckpointTimestamp, error) {
	return s.digests().listCheckpointTimestamps(ctx, fromSeq, toSeq)
}
