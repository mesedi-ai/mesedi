package store

import (
	"fmt"
	"math"
)

// seqRangeArgs converts a checkpoint sequence range to the int64 values the
// database drivers bind. Sequence numbers are uint64 in Go and BIGINT
// (signed) in both databases, so a value above math.MaxInt64 used to wrap
// to a negative number and silently select the wrong range. Code scanning
// flagged all twelve conversions (go/incorrect-integer-conversion); since
// 2026-11-16 they go through this one checked function.
func seqRangeArgs(fromSeq, toSeq uint64) (int64, int64, error) {
	if fromSeq > math.MaxInt64 || toSeq > math.MaxInt64 {
		return 0, 0, fmt.Errorf("checkpoint sequence range [%d, %d] exceeds the database's signed 64-bit limit", fromSeq, toSeq)
	}
	return int64(fromSeq), int64(toSeq), nil
}

// checkedSeq converts one checkpoint sequence number the same way.
func checkedSeq(seq uint64) (int64, error) {
	if seq > math.MaxInt64 {
		return 0, fmt.Errorf("checkpoint sequence %d exceeds the database's signed 64-bit limit", seq)
	}
	return int64(seq), nil
}
