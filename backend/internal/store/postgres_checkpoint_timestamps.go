package store

import (
	"context"
	"time"
)

func (s *PostgresStore) RecordCheckpointTimestamp(ctx context.Context, t CheckpointTimestamp) error {
	return s.digests().recordCheckpointTimestamp(ctx, t)
}

func (s *PostgresStore) CompleteCheckpointTimestamp(ctx context.Context, seq uint64, calendarURL string,
	receipt []byte, at time.Time) error {
	return s.digests().completeCheckpointTimestamp(ctx, seq, calendarURL, receipt, at)
}

func (s *PostgresStore) ListPendingTimestamps(ctx context.Context, limit int) ([]CheckpointTimestamp, error) {
	return s.digests().listPendingTimestamps(ctx, limit)
}

func (s *PostgresStore) ListUntimestampedCheckpoints(ctx context.Context, limit int) ([]UntimestampedCheckpoint, error) {
	return s.digests().listUntimestampedCheckpoints(ctx, limit)
}

func (s *PostgresStore) ListCheckpointTimestamps(ctx context.Context, fromSeq, toSeq uint64) (map[uint64][]CheckpointTimestamp, error) {
	return s.digests().listCheckpointTimestamps(ctx, fromSeq, toSeq)
}
