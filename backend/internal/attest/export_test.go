package attest

import (
	"fmt"
	"strings"
	"testing"
	"time"
)

// These tests build a REAL chain, leaves that actually fold to their
// roots, checkpoints that actually name their predecessors, and then
// break one thing at a time. A test that hand-writes a plausible-looking
// export and asserts it passes proves only that the verifier is lenient.

const exportProject = "proj-agency"

// digestRoot returns a distinct, well-formed 64-hex digest root.
func digestRoot(n int) string {
	return fmt.Sprintf("%064x", n*2654435761)
}

// buildExport constructs a valid export whose intervals have the given
// execution counts. A count of 0 means the project had no activity that
// hour, which produces a checkpoint with no leaf for this project.
func buildExport(t *testing.T, counts []int) ChainExport {
	t.Helper()

	var (
		intervals  []ExportedInterval
		prevCP     *Checkpoint
		prevLeaf   *TenantLeaf
		cumulative uint64
		execN      int
	)

	for i, n := range counts {
		var (
			leaves []TenantLeaf
			leaf   *TenantLeaf
			execs  []ExportedExecution
		)

		if n > 0 {
			roots := make([]string, 0, n)
			for range n {
				execN++
				r := digestRoot(execN)
				roots = append(roots, r)
				execs = append(execs, ExportedExecution{
					ExecutionID: fmt.Sprintf("exec-%04d", execN),
					DigestRoot:  r,
				})
			}
			root, err := RootOverExecutionDigests(roots)
			if err != nil {
				t.Fatalf("RootOverExecutionDigests: %v", err)
			}

			prevHash := ZeroHash
			if prevLeaf != nil {
				prevHash = TenantLeafHash(*prevLeaf)
			}
			cumulative += uint64(n)

			l := TenantLeaf{
				ProjectID:       exportProject,
				IntervalRoot:    root,
				ExecutionCount:  n,
				CumulativeCount: cumulative,
				PrevLeafHash:    prevHash,
			}
			leaves = append(leaves, l)
			leaf = &l
		}

		prevLogEntry := ""
		if prevCP != nil {
			prevLogEntry = fmt.Sprintf("27124678%02d", i)
		}
		cp, err := BuildCheckpoint(CheckpointParams{
			Prev:           prevCP,
			PrevLogEntryID: prevLogEntry,
			IntervalStart:  hour(i),
			IntervalEnd:    hour(i + 1),
			Interval:       testInterval,
			Leaves:         leaves,
			Now:            hour(i + 1).Add(time.Minute),
		})
		if err != nil {
			t.Fatalf("BuildCheckpoint %d: %v", i, err)
		}

		iv := ExportedInterval{
			Checkpoint: cp,
			LogEntryID: fmt.Sprintf("27124679%02d", i),
			AnchoredAt: hour(i + 1).Add(2 * time.Minute),
			Executions: execs,
		}
		if leaf != nil {
			p, err := ProveTenantLeaf(cp, leaves, exportProject)
			if err != nil {
				t.Fatalf("ProveTenantLeaf %d: %v", i, err)
			}
			iv.Leaf = leaf
			iv.Proof = &p
			prevLeaf = leaf
		}
		intervals = append(intervals, iv)

		cpCopy := cp
		prevCP = &cpCopy
	}

	return ChainExport{
		Format:          ChainExportFormatV1,
		GeneratedAt:     hour(len(counts)),
		ProjectID:       exportProject,
		IntervalSeconds: int(testInterval / time.Second),
		Intervals:       intervals,
	}
}

func failedChecks(v ExportVerification) []string {
	var out []string
	for _, c := range v.Checks {
		if !c.OK {
			out = append(out, c.Name+": "+c.Detail)
		}
	}
	return out
}

func TestVerifyChainExportHappyPath(t *testing.T) {
	e := buildExport(t, []int{3, 1, 4})
	v := VerifyChainExport(e)
	if !v.OK {
		t.Fatalf("a valid export failed verification: %v", failedChecks(v))
	}
	if len(v.Unverified) == 0 {
		t.Error("a passing report must still state its limits, or the reader " +
			"assumes everything was checked")
	}
}

// An hour where this project ran nothing is normal and must not read as a
// gap. An agency that sees "FAILED: interval 2" for an hour they were
// asleep will stop believing the report.
func TestVerifyChainExportTreatsQuietHoursAsNormal(t *testing.T) {
	e := buildExport(t, []int{2, 0, 0, 3})
	v := VerifyChainExport(e)
	if !v.OK {
		t.Fatalf("intervals with no activity failed verification: %v", failedChecks(v))
	}
	found := false
	for _, c := range v.Checks {
		if strings.Contains(c.Name, "quiet hours") && c.OK {
			found = true
		}
	}
	if !found {
		t.Error("quiet intervals should be reported explicitly, not silently skipped; " +
			"a reader counting intervals will otherwise think some are missing")
	}
}

func TestVerifyChainExportDetectsTampering(t *testing.T) {
	cases := []struct {
		name    string
		mutate  func(*ChainExport)
		wantHit string
	}{
		{
			name:    "an execution is removed from an interval",
			mutate:  func(e *ChainExport) { e.Intervals[0].Executions = e.Intervals[0].Executions[1:] },
			wantHit: "combined fingerprint",
		},
		{
			name: "an execution digest is swapped",
			mutate: func(e *ChainExport) {
				e.Intervals[2].Executions[0].DigestRoot = digestRoot(9999)
			},
			wantHit: "combined fingerprint",
		},
		{
			name: "the executions are reordered",
			mutate: func(e *ChainExport) {
				x := e.Intervals[0].Executions
				x[0], x[2] = x[2], x[0]
			},
			wantHit: "combined fingerprint",
		},
		{
			name: "a leaf's execution count is inflated",
			mutate: func(e *ChainExport) {
				l := *e.Intervals[0].Leaf
				l.ExecutionCount = 99
				e.Intervals[0].Leaf = &l
			},
			wantHit: "inclusion",
		},
		{
			name:    "a whole checkpoint is dropped from the middle",
			mutate:  func(e *ChainExport) { e.Intervals = append(e.Intervals[:1], e.Intervals[2:]...) },
			wantHit: "records link up",
		},
		{
			name: "a checkpoint's stored hash is rewritten",
			mutate: func(e *ChainExport) {
				e.Intervals[1].Checkpoint.Hash = strings.Repeat("b", 64)
			},
			wantHit: "records link up",
		},
		{
			name:    "a checkpoint was never anchored",
			mutate:  func(e *ChainExport) { e.Intervals[1].LogEntryID = "" },
			wantHit: "published",
		},
		{
			name: "the inclusion proof is replaced with another interval's",
			mutate: func(e *ChainExport) {
				p := *e.Intervals[2].Proof
				e.Intervals[0].Proof = &p
			},
			wantHit: "inclusion",
		},
		{
			name: "a leaf from another tenant is substituted",
			mutate: func(e *ChainExport) {
				l := *e.Intervals[0].Leaf
				l.ProjectID = "proj-someone-else"
				e.Intervals[0].Leaf = &l
			},
			wantHit: "ownership",
		},
		{
			name: "a leaf is dropped from the MIDDLE of the tenant sub-chain",
			mutate: func(e *ChainExport) {
				e.Intervals[1].Leaf = nil
				e.Intervals[1].Proof = nil
				e.Intervals[1].Executions = nil
			},
			wantHit: "sub-chain",
		},
		{
			name:    "the export declares a format this verifier does not know",
			mutate:  func(e *ChainExport) { e.Format = "mesedi.chain-export.v9" },
			wantHit: "format",
		},
		{
			name:    "the cadence is misdeclared to hide a missing hour",
			mutate:  func(e *ChainExport) { e.IntervalSeconds = 7200 },
			wantHit: "records link up",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			e := buildExport(t, []int{3, 1, 4})
			tc.mutate(&e)

			v := VerifyChainExport(e)
			if v.OK {
				t.Fatalf("tampering was not detected")
			}
			hit := false
			for _, c := range v.Checks {
				if !c.OK && (strings.Contains(c.Name, tc.wantHit) ||
					strings.Contains(c.Detail, tc.wantHit)) {
					hit = true
				}
			}
			if !hit {
				t.Errorf("detected a problem but not the expected one (%q).\nfailures: %v",
					tc.wantHit, failedChecks(v))
			}
		})
	}
}

// The export claims to be for one project. If it carries a leaf that is
// not ours, that is either a serious bug or a deliberate attempt to pass
// off someone else's activity, and either way it must not verify.
func TestVerifyChainExportRefusesAnEmptyOrMalformedExport(t *testing.T) {
	for _, tc := range []struct {
		name string
		e    ChainExport
	}{
		{"no project", ChainExport{Format: ChainExportFormatV1, IntervalSeconds: 3600}},
		{"no intervals", ChainExport{
			Format: ChainExportFormatV1, ProjectID: exportProject, IntervalSeconds: 3600}},
		{"zero cadence", ChainExport{
			Format:    ChainExportFormatV1,
			ProjectID: exportProject,
			Intervals: []ExportedInterval{{}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if v := VerifyChainExport(tc.e); v.OK {
				t.Error("a malformed export verified clean")
			}
		})
	}
}

// The hole this closes: dropping a project's EARLIEST leaves is invisible
// to VerifyTenantSubChain, because it cannot check the predecessor of its
// first element. Presenting those hours as "no activity" chains perfectly.
//
// The requirement is not that this fails, a partial export is legitimate
// when an auditor asks for one month of a year. The requirement is that
// the reader is TOLD, with a number, so "complete from your first
// execution" is never confused with "starts partway through".
func TestVerifyChainExportReportsWhenHistoryIsMissingFromTheStart(t *testing.T) {
	full := buildExport(t, []int{3, 1, 4})
	if v := VerifyChainExport(full); !v.OK {
		t.Fatalf("baseline export failed: %v", failedChecks(v))
	} else if !hasCheck(v, "coverage", "first published activity") {
		t.Error("a genesis-complete export should say so explicitly")
	}

	// Same chain, earliest leaf quietly withheld.
	truncated := buildExport(t, []int{3, 1, 4})
	truncated.Intervals[0].Leaf = nil
	truncated.Intervals[0].Proof = nil
	truncated.Intervals[0].Executions = nil

	v := VerifyChainExport(truncated)
	if !hasCheck(v, "coverage", "starts partway through") {
		t.Errorf("withholding the earliest leaves was not reported as a partial "+
			"export.\nchecks: %v", v.Checks)
	}
	// The three withheld executions must be named, not merely implied.
	if !hasUnverified(v, "3 earlier agent runs") {
		t.Errorf("the count of withheld executions was not surfaced.\nunverified: %v",
			v.Unverified)
	}
	// And it must be distinguishable from the complete export above.
	if hasCheck(v, "coverage", "first published activity") {
		t.Error("a truncated export claimed to begin at the project's first activity")
	}
}

// A leaf that claims to be the project's first while its own running total
// says otherwise is self-contradictory, and must fail rather than be
// reported as merely partial.
func TestVerifyChainExportRejectsAFalsifiedGenesisLeaf(t *testing.T) {
	e := buildExport(t, []int{3, 1, 4})
	l := *e.Intervals[0].Leaf
	l.PrevLeafHash = ZeroHash // already true; now inflate the running total
	l.CumulativeCount = 500
	e.Intervals[0].Leaf = &l

	if v := VerifyChainExport(e); v.OK {
		t.Error("a leaf claiming to be first while reporting 497 prior executions verified clean")
	}
}

func hasCheck(v ExportVerification, nameFragment, detailFragment string) bool {
	for _, c := range v.Checks {
		if strings.Contains(c.Name, nameFragment) && strings.Contains(c.Detail, detailFragment) {
			return true
		}
	}
	return false
}

func hasUnverified(v ExportVerification, fragment string) bool {
	for _, u := range v.Unverified {
		if strings.Contains(u, fragment) {
			return true
		}
	}
	return false
}

// A proof or executions present with no leaf means the export contradicts
// itself. Cheap to check and it catches a class of assembly bug that would
// otherwise pass silently, since the leaf-less path skips everything else.
func TestVerifyChainExportCatchesAContradictoryInterval(t *testing.T) {
	e := buildExport(t, []int{3, 1, 4})
	e.Intervals[1].Leaf = nil // proof and executions deliberately left behind

	v := VerifyChainExport(e)
	if v.OK {
		t.Fatal("an interval with no leaf but a leftover proof verified clean")
	}
}
