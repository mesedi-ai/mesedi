package attest

import (
	"bytes"
	"testing"
)

// A Bitcoin attestation whose payload is not a block height is a bad
// receipt, not a pending one (the outside review's round 2, 17 November
// 2026). BitcoinClaims leaves it out; InvalidBitcoinAttestations counts it.
func TestACorruptBitcoinAttestationIsCountedNotHidden(t *testing.T) {
	_, _, ts := readOTSFile(t, "ots-bitcoin-912095.ots")
	if n := ts.InvalidBitcoinAttestations(); n != 0 {
		t.Fatalf("a real receipt has %d invalid attestations", n)
	}
	if len(ts.BitcoinClaims()) == 0 {
		t.Fatal("fixture should carry a Bitcoin claim")
	}
	corrupted := 0
	ts.walk(func(n *OTSTimestamp) {
		for i := range n.Attestations {
			if n.Attestations[i].Tag == OTSTagBitcoin {
				// A height followed by stray bytes does not read.
				n.Attestations[i].Payload = append(bytes.Clone(n.Attestations[i].Payload), 0xff)
				corrupted++
			}
		}
	})
	if corrupted == 0 {
		t.Fatal("no Bitcoin attestation to corrupt")
	}
	if got := ts.InvalidBitcoinAttestations(); got != corrupted {
		t.Fatalf("%d invalid attestations counted, want %d", got, corrupted)
	}
	if len(ts.BitcoinClaims()) != 0 {
		t.Fatal("a corrupted attestation still reads as a claim")
	}
}
