package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"mesedi/backend/internal/attest"
)

// Verified against REAL Sigstore data, not a fixture we invented.
//
// Everything below comes from the 5 September 2026 production run:
// checkpoint 24 landed in a tree of 2,603,896,653 entries and
// checkpoint 27 in a tree of 2,605,579,383, and Rekor served a 29-hash
// consistency proof between them. The two roots are read out of the
// signed checkpoints in that export.
//
// A synthetic fixture would only prove this code agrees with itself.
// The failure mode that matters here is an off-by-one in the RFC 6962
// walk that happens to be self-consistent, and only real data from a
// tree of two and a half billion entries exercises the branch structure
// that would expose it.

const (
	live24Size = 2603896653
	live27Size = 2605579383
	live24Root = "ea430359e8f7e6804110a4fc9cb2f381c6bbee9dc2645c12631ea2b1c888629a"
	live27Root = "83214d791b355fb5b6277b5c3d16a9638c03c43e459b55029033f3405b3b7928"
)

// The proof path Rekor returned for firstSize=2603896653,
// lastSize=2605579383, treeID=1193050959916656506.
var live24to27Proof = []string{
	"abf8a54eea89b777116e1629402eabf525f17b5e31ca2c5ea10d51bad77b4bc8",
	"978598b5db90c47b7c6c1c6ff88b8d086f7043443eacd96a9180a7770234236f",
	"debb3d9c802de9790562a67f4d0502b86c10280330c5217e41a4d4189de19684",
	"44535110393ea68f0424f3d96693bcf6b80e5630913551b398995895d9d1536a",
	"cb1729241858b36564fece79420016c5d4b50b3df81d3b1376d1de909ee2a654",
	"44d6b50bf637337a01d4e08576f5804560ab56912c0b25cd3b60491d5f80f7e9",
	"1376295ee02260dcbfd76a7a56a3ed4571e98a18603ed4a42d1e7ec45c75c6ea",
	"37724d2283671f05a48cb6dd07e0f03886fdf60f98e70a6b78cbd204af74a7c7",
	"58f1cb901dfabe47b7baad2f777f9baa98a660970d6b91c1174efd68d38d9a69",
	"b7b2c0e6f02b0d56344d0a662b54baf1d9445fbf451b7018fa14aa5a588dc805",
	"f8d3fc9bf300dc5bf4c7d87d0a95029e3a1099bdedc23625a18f027ed21f1b92",
	"26d5bcf1183b296fab0dd724beba8adb3d2a667bad5b85a29e82f1a5a0c7b46b",
	"97ca17f846f1e2bf8e9676f7784d9f74c182bb4d5bf669d5516baa9f44a86fa0",
	"cfdd9a720bef2964df91f4728a27ab721a50b1e90c3ddd0c0040e4334412b249",
	"8071ecf70c2af9fcfa988384a0609732b32d82e0f1202e94696a3e79cdf7acae",
	"d7ff3684caa6edc450611a9b700b23f95d6ccbac54148d9fc3751da178055692",
	"3fe8356ffb5d6a1a4322f6f3944161371d5b213161cdd2bb77d92a62bda47684",
	"cfbf4fae934bcbeab4e9f80e82c8eafb333fc8e0ddbef9103cd4824bb3cddf92",
	"9c6740b58d782b9c6bfeb093d2fbc5708ee217eb6d99d9dafb3c32c73050ed7c",
	"96ca19fb452cbcb1ad2eac504678dc473fc8b439cd89b96bf254533e10ed29ca",
	"efa6042f9cd5df95935230653678b0c31afa406dcf49644f1f409e4dc84246c9",
	"73f496c93d22f4981a6a6699e588926eac9be8401f43455f25b50d78c8dcd217",
	"8c4aee8fd04593fa782f1f8dab46a4c84891c6e41335a2cdb44801be7d3ddc78",
	"9116a4e95590de1396eccd5a5985a255ba110d0a724418a812b59e9019a8c4ce",
	"b5b159672bc734e04fe44ea519d788a6d9e9dc86747f207a388eccd2532566ae",
	"7e73ebbededa7c4b9394885a31cac651dbbdfa17809a4e23389dfe06bf3f07c3",
	"79748bacba8a4b14538b148ffe4e188a6ae207696e7daa3ab6ba93aecc719728",
	"4a775b30a56d7137a7024c22d8905f1b30fe9a17b1a75a896d1218f80d494485",
	"c47fc30ac78b1ebf5e2a8613f2ab0e4592bbcd5744198587b95b6c56b0fde706",
}

func mustHex(t *testing.T, s string) []byte {
	t.Helper()
	b, err := hex.DecodeString(s)
	if err != nil {
		t.Fatalf("bad hex fixture: %v", err)
	}
	return b
}

func livePath(t *testing.T) [][]byte {
	t.Helper()
	p, err := decodeProofPath(live24to27Proof)
	if err != nil {
		t.Fatalf("decodeProofPath: %v", err)
	}
	return p
}

func TestTheRealSigstoreProofVerifies(t *testing.T) {
	if err := verifyConsistency(live24Size, mustHex(t, live24Root),
		live27Size, mustHex(t, live27Root), livePath(t)); err != nil {
		t.Fatalf("a genuine Sigstore consistency proof between two real production "+
			"tree heads was rejected: %v", err)
	}
}

// The half that stops the test above passing for a trivial reason. If
// verifyConsistency returned nil for everything it would still pass, so
// the same proof must be REJECTED against a root that is one bit
// different. That is the case a log rewriting its history produces.
func TestAProofAgainstTheWrongLaterRootIsRejected(t *testing.T) {
	wrong := mustHex(t, live27Root)
	wrong[0] ^= 0x01

	err := verifyConsistency(live24Size, mustHex(t, live24Root),
		live27Size, wrong, livePath(t))
	if err == nil {
		t.Fatal("a proof was accepted against a later root it does not rebuild. " +
			"A log that rebuilt its tree would pass this check")
	}
	if !strings.Contains(err.Error(), "did not simply grow") {
		t.Errorf("the failure does not explain what it means: %v", err)
	}
}

func TestAProofAgainstTheWrongEarlierRootIsRejected(t *testing.T) {
	wrong := mustHex(t, live24Root)
	wrong[31] ^= 0x80

	if err := verifyConsistency(live24Size, wrong,
		live27Size, mustHex(t, live27Root), livePath(t)); err == nil {
		t.Fatal("a proof was accepted against an earlier root it does not rebuild, " +
			"so it would pass for a tree this record was never published in")
	}
}

// A truncated path must fail rather than accidentally landing on a root.
func TestATruncatedProofIsRejected(t *testing.T) {
	full := livePath(t)
	if err := verifyConsistency(live24Size, mustHex(t, live24Root),
		live27Size, mustHex(t, live27Root), full[:len(full)-1]); err == nil {
		t.Fatal("a proof missing its last step was accepted")
	}
}

// A log that shrank is a finding, and must be named as one rather than
// reported as a generic mismatch.
func TestAShrinkingTreeIsNamed(t *testing.T) {
	err := verifyConsistency(live27Size, mustHex(t, live27Root),
		live24Size, mustHex(t, live24Root), livePath(t))
	if err == nil {
		t.Fatal("a later tree smaller than the earlier one was accepted")
	}
	if !strings.Contains(err.Error(), "shrank") {
		t.Errorf("the error does not say the log shrank: %v", err)
	}
}

// Two anchors can land in the same tree. That needs no path, and two
// different roots at the same size is the log serving two trees at once.
func TestEqualSizesRequireEqualRootsAndNoPath(t *testing.T) {
	root := mustHex(t, live27Root)
	if err := verifyConsistency(live27Size, root, live27Size, root, nil); err != nil {
		t.Errorf("equal size and equal root was rejected: %v", err)
	}
	if err := verifyConsistency(live27Size, root, live27Size, root, livePath(t)); err == nil {
		t.Error("a proof path between a tree and itself was accepted")
	}
	other := mustHex(t, live24Root)
	if err := verifyConsistency(live27Size, root, live27Size, other, nil); err == nil {
		t.Error("two different roots at the same tree size were accepted, which is " +
			"the log having served two different trees")
	}
}

func TestMissingPathBetweenDifferentSizesIsRejected(t *testing.T) {
	if err := verifyConsistency(live24Size, mustHex(t, live24Root),
		live27Size, mustHex(t, live27Root), nil); err == nil {
		t.Fatal("an absent proof path between two different tree sizes was accepted, " +
			"which would report the log as proven to have only grown on no evidence")
	}
}

// A short hash in the path must be refused at decode rather than fed
// into the walk, where it would produce a wrong root and read to an
// auditor as tampering.
// ── the walk across an export ────────────────────────────────────────

func intervalWith(t *testing.T, seq uint64, size int64, root, consistency string) attest.ExportedInterval {
	t.Helper()
	env := fmt.Sprintf(
		`{"log_id":"aa","entry_body":"bb","inclusion_proof":{"TreeSize":%d,"RootHash":%q}%s}`,
		size, root, consistency)
	return attest.ExportedInterval{
		Checkpoint:  attest.Checkpoint{Seq: seq},
		AnchorProof: json.RawMessage(env),
	}
}

func proofField(t *testing.T, first, last int64, hashes []string) string {
	t.Helper()
	b, err := json.Marshal(consistencyProofWire{FirstSize: first, LastSize: last, Hashes: hashes})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return `,"consistency_proof":` + string(b)
}

// The shape of every export produced before today. Absence must count
// as unproven and must NOT count as a failure: nobody asked the log
// anything, which is a gap in our evidence, not a finding about Rekor.
func TestAnExportWithNoGrowthProofsReportsThemUnprovenNotFailed(t *testing.T) {
	res := checkLogGrowth([]attest.ExportedInterval{
		intervalWith(t, 24, live24Size, live24Root, ""),
		intervalWith(t, 25, live24Size+1, live27Root, ""),
		intervalWith(t, 26, live27Size, live27Root, ""),
	})
	if len(res.Failures) != 0 {
		t.Errorf("a missing growth proof was reported as a failure: %v", res.Failures)
	}
	if res.Proven != 0 || res.Unproven != 2 {
		t.Errorf("proven=%d unproven=%d, want 0 and 2", res.Proven, res.Unproven)
	}
	if s := growthSummary(res); s != "" {
		t.Errorf("a run that proved nothing produced a summary claiming something: %q", s)
	}
}

func TestARealGrowthProofAcrossTwoHoursIsCounted(t *testing.T) {
	res := checkLogGrowth([]attest.ExportedInterval{
		intervalWith(t, 24, live24Size, live24Root, ""),
		intervalWith(t, 25, live27Size, live27Root,
			proofField(t, live24Size, live27Size, live24to27Proof)),
	})
	if len(res.Failures) != 0 {
		t.Fatalf("a genuine Sigstore growth proof was reported as a failure: %v", res.Failures)
	}
	if res.Proven != 1 || res.Unproven != 0 {
		t.Fatalf("proven=%d unproven=%d, want 1 and 0", res.Proven, res.Unproven)
	}
	if !strings.Contains(growthSummary(res), "only been added to") {
		t.Errorf("the summary does not say what was established: %q", growthSummary(res))
	}
}

// A proof can verify perfectly and still be about two other trees. That
// is the same trap as an inclusion proof for an entry that is not
// yours, and it must be a finding rather than a pass.
func TestAGrowthProofAboutOtherTreesIsAFinding(t *testing.T) {
	res := checkLogGrowth([]attest.ExportedInterval{
		intervalWith(t, 24, live24Size, live24Root, ""),
		intervalWith(t, 25, live27Size, live27Root,
			proofField(t, live24Size-1, live27Size, live24to27Proof)),
	})
	if len(res.Failures) != 1 {
		t.Fatalf("a proof about the wrong tree sizes was accepted: %+v", res)
	}
	if res.Proven != 0 {
		t.Errorf("it was also counted as proven")
	}
	if !strings.Contains(growthSummary(res), "did NOT only grow") {
		t.Errorf("the summary softens a finding: %q", growthSummary(res))
	}
}

// A stored proof that does not hold is a statement about the log, and
// the report must fail rather than file it as a limitation.
func TestABrokenGrowthProofIsAFailureNotACaveat(t *testing.T) {
	bad := make([]string, len(live24to27Proof))
	copy(bad, live24to27Proof)
	bad[0] = strings.Repeat("00", 32)

	res := checkLogGrowth([]attest.ExportedInterval{
		intervalWith(t, 24, live24Size, live24Root, ""),
		intervalWith(t, 25, live27Size, live27Root,
			proofField(t, live24Size, live27Size, bad)),
	})
	if len(res.Failures) != 1 {
		t.Fatalf("a growth proof that does not rebuild the roots was accepted: %+v", res)
	}
	if res.Unproven != 0 {
		t.Error("a broken proof was also counted as unproven, which would let a real " +
			"finding be read as a gap in our evidence")
	}
}

// The caveat that must change when this check starts passing.
//
// "It does not show the log has only ever been added to" is true today
// and becomes FALSE the moment every step is proven. A false sentence
// inside the section whose entire job is to prevent overclaiming is the
// worst place in the report for one, and it is the exact defect closed
// as task 36. This asserts the substitution is wired, and that the
// replacement still names the split view as the remaining gap.
func TestTheHistoryCaveatIsNarrowedOnceGrowthIsProven(t *testing.T) {
	before := "Sigstore's signature was checked, but not the log's history. The proof " +
		"shows this record is covered by one summary Sigstore signed."
	after := replacePrefix([]string{before},
		"Sigstore's signature was checked, but not the log's history",
		"Sigstore's signature and its growth were both checked, but not whether it "+
			"showed the same log to everyone.")

	if len(after) != 1 || after[0] == before {
		t.Fatalf("replacePrefix did not substitute the history caveat: %v", after)
	}
	if strings.Contains(after[0], "not the log's history") {
		t.Error("the replacement still claims the log's history was not checked")
	}
	if !strings.Contains(after[0], "same log to everyone") {
		t.Error("the replacement drops the split view, which is the gap that actually " +
			"remains once growth is proven")
	}
}

func TestAMalformedProofPathIsRefusedBeforeWalking(t *testing.T) {
	for name, path := range map[string][]string{
		"not hex":   {"zzzz"},
		"too short": {hex.EncodeToString(make([]byte, sha256.Size-1))},
		"too long":  {hex.EncodeToString(make([]byte, sha256.Size+1))},
	} {
		if _, err := decodeProofPath(path); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
