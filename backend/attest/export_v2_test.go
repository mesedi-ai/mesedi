package attest

// Chain export v2 and the report fixes of 2026-11-16 (outside review
// the outside review of 2026-11-15).

import (
	"strings"
	"testing"
	"time"
)

func TestV2ExportsAreAcceptedAndUnknownFormatsRefused(t *testing.T) {
	e := buildExport(t, []int{1, 2})
	e.Format = ChainExportFormatV2
	if v := VerifyChainExport(e); !v.OK {
		t.Fatalf("a valid v2 export failed: %v", failedChecks(v))
	}
	e.Format = "mesedi.chain-export.v9"
	if v := VerifyChainExport(e); v.OK {
		t.Fatal("an unknown export format verified")
	}
}

func TestAnHourWithoutDigestsIsUnverifiedNotFailed(t *testing.T) {
	e := buildExport(t, []int{1, 2})
	e.Format = ChainExportFormatV2
	e.Intervals[1].Executions = nil
	e.Intervals[1].DigestsUnavailable = "records changed before digests were stored"
	v := VerifyChainExport(e)
	if !v.OK {
		t.Fatalf("an hour whose history could not be stored was reported as a failure: %v", failedChecks(v))
	}
	if !hasUnverified(v, "before Mesedi stored each run's fingerprint") {
		t.Errorf("the report does not explain the hour without digests: %v", v.Unverified)
	}
	if !hasCheck(v, "checkpoint 2", "cannot be listed: records changed before digests were stored") {
		t.Errorf("the hour's own line does not say why its runs cannot be listed: %+v", v.Checks)
	}
}

func TestAnHourMarkedWithoutDigestsMustNotListAny(t *testing.T) {
	e := buildExport(t, []int{1, 2})
	e.Format = ChainExportFormatV2
	e.Intervals[1].DigestsUnavailable = "records changed before digests were stored"
	if v := VerifyChainExport(e); v.OK {
		t.Fatal("an hour marked as having no digests while listing digests verified")
	}
}

func TestRetentionDeletedRunsVerifyAndAreReported(t *testing.T) {
	e := buildExport(t, []int{2})
	e.Format = ChainExportFormatV2
	e.Intervals[0].Executions[0].RecordsDeleted = true
	v := VerifyChainExport(e)
	if !v.OK {
		t.Fatalf("a run whose records retention deleted failed verification: %v", failedChecks(v))
	}
	if !hasCheck(v, "retention", "1 of 2 agent runs listed had their records deleted") {
		t.Errorf("no retention line: %+v", v.Checks)
	}
	if !hasUnverified(v, "deleted under retention") {
		t.Errorf("no retention limit stated: %v", v.Unverified)
	}
}

func TestTrailingQuietHoursCannotBeConfirmed(t *testing.T) {
	v := VerifyChainExport(buildExport(t, []int{1, 0, 2, 0, 0}))
	if !v.OK {
		t.Fatalf("export failed: %v", failedChecks(v))
	}
	if !hasCheck(v, "quiet hours", "1 hour before this project's last entry") {
		t.Errorf("the quiet hour before the last entry is not reported as covered: %+v", v.Checks)
	}
	if !hasUnverified(v, "The last 2 hours in this file show no agent runs") {
		t.Errorf("the trailing quiet hours are not stated as unconfirmed: %v", v.Unverified)
	}
	for _, c := range v.Checks {
		if strings.Contains(c.Detail, "normal and not a gap") {
			t.Errorf("the old claim that every quiet hour is normal is still made: %s", c.Detail)
		}
	}
}

func TestCoverageRefusesARunningTotalBelowItsCount(t *testing.T) {
	e := buildExport(t, []int{3, 1})
	e.Intervals[0].Leaf.CumulativeCount = 1 // below its own count of 3
	v := VerifyChainExport(e)
	if !hasCheck(v, "coverage", "smaller than its own count of 3") {
		t.Fatalf("a running total below the leaf's own count was not refused: %+v", v.Checks)
	}
	for _, c := range v.Checks {
		if strings.Contains(c.Detail, "18446744073709551615") {
			t.Fatalf("the coverage count wrapped around: %s", c.Detail)
		}
	}
}

func TestPublicationLineNoLongerClaimsLateness(t *testing.T) {
	v := VerifyChainExport(buildExport(t, []int{1, 1, 1}))
	for _, c := range v.Checks {
		if strings.Contains(c.Detail, "one hour late") {
			t.Fatalf("the report still claims late publishing would break the chain: %s", c.Detail)
		}
	}
	if !hasCheck(v, "publication never skipped", "so no hour was skipped") {
		t.Errorf("publication line missing: %+v", v.Checks)
	}
}

func TestReportUsesCheckpointLabelsAndPlurals(t *testing.T) {
	v := VerifyChainExport(buildExport(t, []int{1}))
	if !hasCheck(v, "checkpoint 1 (", "1 agent run,") {
		t.Errorf("want a dated checkpoint label and the singular: %+v", v.Checks)
	}
	cp := Checkpoint{Seq: 7, IntervalStart: time.Date(2026, 9, 3, 14, 0, 0, 0, time.UTC)}
	if got := checkLabel(cp); got != "checkpoint 7 (2026-09-03 14:00 UTC)" {
		t.Errorf("checkLabel = %q", got)
	}
}

func TestVerifyChainRefusesNegativeCountAndUnlinkedWindowStart(t *testing.T) {
	e := buildExport(t, []int{1, 1, 1})
	cps := []Checkpoint{e.Intervals[1].Checkpoint, e.Intervals[2].Checkpoint}
	if err := VerifyChain(cps, time.Hour); err != nil {
		t.Fatalf("a valid window failed: %v", err)
	}
	unlinked := cps[0]
	unlinked.PrevLogEntryID = ""
	unlinked.Hash = CheckpointHash(unlinked)
	if err := VerifyChain([]Checkpoint{unlinked}, time.Hour); err == nil {
		t.Error("a window starting with a checkpoint that names no published predecessor verified")
	}
	neg := e.Intervals[0].Checkpoint
	neg.TenantLeafCount = -1
	neg.Hash = CheckpointHash(neg)
	if err := VerifyChain([]Checkpoint{neg}, time.Hour); err == nil {
		t.Error("a checkpoint with a negative leaf count verified")
	}
}
