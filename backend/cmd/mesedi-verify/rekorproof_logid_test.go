package main

import "testing"

// Guards the trusted root.
//
// Deliberately in its own file rather than added to rekorproof_test.go,
// which is a verbatim copy of the upstream test file and must stay
// diffable against verdifax-orchestrator/internal/rekorverify when the
// two copies are re-synced. Local additions go here.
//
// WHY THIS TEST IS WORTH MORE THAN IT LOOKS
//
// rekorproof_pubkey.go embeds Sigstore's Rekor public key and its own
// comment calls it a TRUSTED ROOT: "Any compromise of this constant
// means an attacker can forge anchored entries." A wrong key does not
// fail loudly. It produces a verifier that walks a Merkle path, checks
// a signature against the wrong key, and reports whatever that
// comparison happens to yield, the worst outcome this binary has,
// because it looks exactly like a working verifier.
//
// So the key is checked against a value SIGSTORE ITSELF PRODUCED,
// rather than against a hash of the key computed from the key, which
// would be circular and would pass with any key at all.

// rekorProductionLogID is the log identifier Sigstore stamps on entries
// in its production Rekor instance. It is the hex SHA-256 of the DER
// encoding of the log's public key, so it is a fingerprint of the very
// constant this test is guarding.
//
// OBSERVED, not looked up in documentation: read from Rekor entry
// 2718374165 on 2026-09-05, which is the entry Mesedi checkpoint 11 was
// anchored to. Reproduce it with:
//
//	curl -sS "https://rekor.sigstore.dev/api/v1/log/entries?logIndex=2718374165" \
//	  | python3 -c "import sys,json;print(list(json.load(sys.stdin).values())[0]['logID'])"
//
// Any recent index works; the log id is a property of the log, not of
// the entry.
const rekorProductionLogID = "c0d23d6ad406973f9559f3ba2d1ca01f84147d8ffc5b8445c224f98b9591801d"

func TestEmbeddedKeyIsTheProductionRekorKey(t *testing.T) {
	got, err := EmbeddedLogID()
	if err != nil {
		t.Fatalf("EmbeddedLogID: %v, the embedded key does not parse, so no "+
			"offline verification is possible at all", err)
	}
	if got != rekorProductionLogID {
		t.Fatalf(
			"the embedded Rekor public key is NOT the production key.\n"+
				"  embedded key fingerprints to %s\n"+
				"  production Rekor stamps       %s\n\n"+
				"Every offline verification this binary performs is worthless until "+
				"this matches: signatures would be checked against the wrong key and "+
				"the result reported as if it meant something. Do not 'fix' this by "+
				"updating the constant below, confirm against a live entry first "+
				"(see the comment on rekorProductionLogID), because if Sigstore has "+
				"rotated its key then previously anchored records need the OLD key "+
				"to verify and this file needs both.",
			got, rekorProductionLogID)
	}
}

// A malformed or absent key must fail loudly rather than silently
// disabling verification. rekor_pubkey.go's own documentation promises
// "a clear 'populate this file' error rather than silently accepting
// any key", and that promise is only worth something if it is tested.
func TestEmbeddedLogIDIsDerivedNotHardcoded(t *testing.T) {
	got, err := EmbeddedLogID()
	if err != nil {
		t.Fatalf("EmbeddedLogID: %v", err)
	}
	if len(got) != 64 {
		t.Errorf("log id %q is not a 64-character hex SHA-256; it is being "+
			"returned from somewhere other than a hash of the key", got)
	}
	for _, c := range got {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			t.Fatalf("log id %q contains a non-lowercase-hex character %q", got, c)
		}
	}
}
