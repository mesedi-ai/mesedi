package attest

// OpenTimestamps receipts, tested against the reference client's own
// published proofs (testdata/OTS-SOURCES.md).

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"strings"
	"testing"
)

// Block 912095's Merkle root as block explorers display it; see
// testdata/OTS-SOURCES.md.
const block912095Root = "936131994129988e1932d8c1c6a9780fd42ae4c87204cfb0ac852be1250039ae"

func readOTSFile(t *testing.T, name string) ([]byte, []byte, *OTSTimestamp) {
	t.Helper()
	raw, err := os.ReadFile("testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	digest, ts, err := ParseOTSFile(raw)
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return raw, digest, ts
}

func reversedHex(b []byte) string {
	r := make([]byte, len(b))
	for i := range b {
		r[i] = b[len(b)-1-i]
	}
	return hex.EncodeToString(r)
}

func TestARealProofLeadsToItsBitcoinBlock(t *testing.T) {
	raw, digest, ts := readOTSFile(t, "ots-bitcoin-912095.ots")
	if hex.EncodeToString(digest) != "c70972a12908b73c2407d9cc6842ba2a02203a690f3090cd29f30c45f0cfd93d" {
		t.Fatalf("file digest %x", digest)
	}
	claims := ts.BitcoinClaims()
	if len(claims) != 1 || claims[0].Height != 912095 {
		t.Fatalf("claims %+v", claims)
	}
	if got := reversedHex(claims[0].Msg); got != block912095Root {
		t.Fatalf("the proof leads to %s, but block 912095's Merkle root is %s", got, block912095Root)
	}
	if body := raw[len(otsFileMagic)+2+sha256.Size:]; !bytes.Equal(ts.Serialize(), body) {
		t.Fatal("writing the proof back out does not reproduce the reference client's bytes")
	}
}

func TestPendingProofsNameTheirCalendars(t *testing.T) {
	for name, want := range map[string][]string{
		"ots-two-calendars.ots": {"https://alice.btc.calendar.opentimestamps.org", "https://bob.btc.calendar.opentimestamps.org"},
		"ots-pending.ots":       {"https://alice.btc.calendar.opentimestamps.org"},
	} {
		raw, _, ts := readOTSFile(t, name)
		var got []string
		for _, p := range ts.PendingClaims() {
			got = append(got, p.URI)
		}
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("%s: pending at %v, want %v", name, got, want)
		}
		if len(ts.BitcoinClaims()) != 0 {
			t.Errorf("%s: a pending proof reports a Bitcoin block", name)
		}
		if body := raw[len(otsFileMagic)+2+sha256.Size:]; !bytes.Equal(ts.Serialize(), body) {
			t.Errorf("%s: does not round-trip", name)
		}
	}
}

func TestAPathThroughRIPEMD160IsRefused(t *testing.T) {
	raw, _ := os.ReadFile("testdata/ots-ripemd160.ots")
	if _, _, err := ParseOTSFile(raw); err == nil || !strings.Contains(err.Error(), "unsupported operation 0x03") {
		t.Fatalf("err = %v, want the RIPEMD-160 operation refused", err)
	}
}

func TestGraftingAnUpgradeCompletesAPendingProof(t *testing.T) {
	_, digest, ts := readOTSFile(t, "ots-pending.ots")
	pending := ts.PendingClaims()
	if len(pending) != 1 {
		t.Fatal("fixture should be pending at one calendar")
	}
	// An upgrade as a calendar returns it: operations from the
	// commitment to a block, ending in a Bitcoin attestation.
	var height bytes.Buffer
	writeVaruint(&height, 912000)
	upgrade := &OTSTimestamp{Msg: pending[0].Commitment}
	next, _ := applyOTSOp(OTSOpAppend, []byte{1, 2, 3}, upgrade.Msg)
	leaf, _ := applyOTSOp(OTSOpSHA256, nil, next)
	upgrade.Ops = []OTSOp{{Tag: OTSOpAppend, Arg: []byte{1, 2, 3}, Next: &OTSTimestamp{Msg: next,
		Ops: []OTSOp{{Tag: OTSOpSHA256, Next: &OTSTimestamp{Msg: leaf,
			Attestations: []OTSAttestation{{Tag: OTSTagBitcoin, Payload: height.Bytes()}}}}}}}}

	wrong := &OTSTimestamp{Msg: []byte("another commitment")}
	if ts.Graft(pending[0].Commitment, wrong) {
		t.Fatal("an upgrade for another commitment was grafted")
	}
	if !ts.Graft(pending[0].Commitment, upgrade) {
		t.Fatal("the upgrade did not graft")
	}
	if len(ts.PendingClaims()) != 0 {
		t.Error("the calendar's pending attestation survived its upgrade")
	}
	claims := ts.BitcoinClaims()
	if len(claims) != 1 || claims[0].Height != 912000 || !bytes.Equal(claims[0].Msg, leaf) {
		t.Fatalf("after grafting, claims %+v", claims)
	}
	again, err := ParseOTSTimestamp(digest, ts.Serialize())
	if err != nil || len(again.BitcoinClaims()) != 1 {
		t.Fatalf("the grafted receipt does not read back: %v", err)
	}
}

func TestHostileReceiptsAreRefused(t *testing.T) {
	digest := make([]byte, 32)
	// Well formed but too deep: a long chain of hashes ending in a valid
	// attestation, so only the depth limit can refuse it.
	deep := append(bytes.Repeat([]byte{OTSOpSHA256}, otsMaxDepth+5), append(append([]byte{0x00}, OTSTagPending[:]...), 0)...)
	cases := map[string][]byte{
		"empty":              {},
		"truncated":          {OTSOpAppend, 5, 1, 2},
		"trailing bytes":     append(append([]byte{0x00}, OTSTagPending[:]...), 1, 0, 0xaa),
		"nested too deeply":  deep,
		"oversized field":    append(append([]byte{0x00}, OTSTagPending[:]...), 0xff, 0xff, 0x03),
		"result too long":    append([]byte{OTSOpAppend, 0xa0, 0x20}, make([]byte, otsMaxMsgLength)...),
		"unknown operation":  {0x67, 0x00},
		"varuint never ends": append([]byte{OTSOpAppend}, bytes.Repeat([]byte{0xff}, 11)...),
	}
	for name, data := range cases {
		if _, err := ParseOTSTimestamp(digest, data); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
