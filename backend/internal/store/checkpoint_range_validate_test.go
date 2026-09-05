package store

import (
	"strings"
	"testing"
)

// ValidateCheckpointRange is exported so the API layer can reject a bad
// range at the boundary rather than letting it reach the database. That
// makes it a shared contract between two packages, and the point of these
// tests is that the boundary and the store can never disagree about what
// a valid range is, there is one implementation and both call it.
func TestValidateCheckpointRange(t *testing.T) {
	cases := []struct {
		name      string
		from, to  uint64
		wantErr   bool
		wantMatch string
	}{
		{name: "single checkpoint", from: 1, to: 1},
		{name: "ordinary range", from: 10, to: 20},
		{name: "exactly at the cap", from: 1, to: MaxCheckpointRange},

		{
			name: "sequence zero", from: 0, to: 5,
			wantErr: true, wantMatch: "start at 1",
		},
		{
			name: "both zero", from: 0, to: 0,
			wantErr: true, wantMatch: "start at 1",
		},
		{
			name: "reversed", from: 9, to: 3,
			wantErr: true, wantMatch: "before from",
		},
		{
			name: "one past the cap", from: 1, to: MaxCheckpointRange + 1,
			wantErr: true, wantMatch: "smaller ranges",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidateCheckpointRange(tc.from, tc.to)
			if tc.wantErr && err == nil {
				t.Fatalf("range [%d, %d] was accepted", tc.from, tc.to)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("range [%d, %d] was refused: %v", tc.from, tc.to, err)
			}
			if tc.wantErr && !strings.Contains(err.Error(), tc.wantMatch) {
				t.Errorf("error should explain the problem (%q), got: %v",
					tc.wantMatch, err)
			}
		})
	}
}

// The cap must refuse rather than silently shorten. A truncated export
// cannot be told apart from an export of a chain with a hole in it, and
// telling those two apart is the whole product, so the error must say so
// rather than reading as a generic limit.
func TestValidateCheckpointRangeRefusesRatherThanImplyingTruncation(t *testing.T) {
	err := ValidateCheckpointRange(1, MaxCheckpointRange+500)
	if err == nil {
		t.Fatal("an oversized range was accepted")
	}
	for _, want := range []string{"hole", "smaller ranges"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should explain why truncation is not an option "+
				"(missing %q), got: %v", want, err)
		}
	}
}

// Guards against a maximum expressed as a count being confused with one
// expressed as a difference. from=1,to=744 is 744 checkpoints, not 745.
func TestValidateCheckpointRangeCountsInclusively(t *testing.T) {
	if err := ValidateCheckpointRange(1, MaxCheckpointRange); err != nil {
		t.Errorf("a range of exactly MaxCheckpointRange checkpoints was refused: %v", err)
	}
	if err := ValidateCheckpointRange(1, MaxCheckpointRange+1); err == nil {
		t.Error("a range of MaxCheckpointRange+1 checkpoints was accepted")
	}
	// The same width starting elsewhere must behave identically.
	if err := ValidateCheckpointRange(1000, 1000+MaxCheckpointRange-1); err != nil {
		t.Errorf("the cap should depend on width, not on where the range starts: %v", err)
	}
}
