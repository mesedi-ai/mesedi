package store

import (
	"context"
	"io"
	"log/slog"
	"math"
	"testing"
)

func TestSeqRangeArgs(t *testing.T) {
	from, to, err := seqRangeArgs(3, 9)
	if err != nil || from != 3 || to != 9 {
		t.Fatalf("seqRangeArgs(3, 9) = %d, %d, %v", from, to, err)
	}
	if _, _, err := seqRangeArgs(0, math.MaxInt64); err != nil {
		t.Fatalf("the largest signed value must be accepted: %v", err)
	}
	if _, _, err := seqRangeArgs(0, math.MaxInt64+1); err == nil {
		t.Fatal("a sequence above math.MaxInt64 must be refused, not wrapped negative")
	}
	if _, _, err := seqRangeArgs(math.MaxUint64, 1); err == nil {
		t.Fatal("a from value above math.MaxInt64 must be refused")
	}
}

func TestCheckpointRangeRefusesUnrepresentableSeq(t *testing.T) {
	st, err := OpenSQLite(":memory:", slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	// A narrow range, so ValidateCheckpointRange's span limit passes and
	// only the signed-64-bit guard can refuse it.
	ctx := context.Background()
	if _, err := st.ListCheckpointRange(ctx, math.MaxInt64+1, math.MaxInt64+2); err == nil {
		t.Error("ListCheckpointRange accepted a range it cannot represent")
	}
	if _, err := st.ListCheckpointLeavesRange(ctx, math.MaxInt64+1, math.MaxInt64+2); err == nil {
		t.Error("ListCheckpointLeavesRange accepted a range it cannot represent")
	}
}
