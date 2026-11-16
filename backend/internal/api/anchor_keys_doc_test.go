package api

// backend/docs/ANCHOR-SIGNING-KEYS.md publishes Mesedi's two anchor keys
// with their fingerprints. Each fingerprint must be the hash of the key
// printed beside it, so the document cannot state one key and print
// another.

import (
	"crypto/ecdsa"
	"crypto/x509"
	"encoding/pem"
	"errors"
	"os"
	"regexp"
	"strings"
	"testing"
)

func TestPublishedAnchorKeysMatchTheirFingerprints(t *testing.T) {
	raw, err := os.ReadFile("../../docs/ANCHOR-SIGNING-KEYS.md")
	if err != nil {
		t.Fatal(err)
	}
	doc := string(raw)
	pems := regexp.MustCompile(`(?s)-----BEGIN PUBLIC KEY-----.*?-----END PUBLIC KEY-----`).FindAllString(doc, -1)
	want := []string{
		"eccad116fe144bd3ca0da53453eac1180d0981e0833ff756b18580c3648e5247",
		"f7ad3f9ce0b4e55ae144ed574944b9b1854cd8b0c7461f1941484a191a041e44",
	}
	if len(pems) != len(want) {
		t.Fatalf("%d keys printed, want %d", len(pems), len(want))
	}
	for i, p := range pems {
		pub, err := parsePublicKey([]byte(p))
		if err != nil {
			t.Fatalf("key %d: %v", i+1, err)
		}
		if got := AnchorKeyFingerprint(pub); got != want[i] {
			t.Errorf("key %d hashes to %s, want %s", i+1, got, want[i])
		}
		if !strings.Contains(doc, "`"+want[i]+"`") {
			t.Errorf("fingerprint %s is not stated in the table", want[i])
		}
	}
}

func parsePublicKey(p []byte) (*ecdsa.PublicKey, error) {
	block, _ := pem.Decode(p)
	if block == nil {
		return nil, errors.New("not PEM")
	}
	k, err := x509.ParsePKIXPublicKey(block.Bytes)
	if err != nil {
		return nil, err
	}
	pub, ok := k.(*ecdsa.PublicKey)
	if !ok {
		return nil, errors.New("not an ECDSA key")
	}
	return pub, nil
}
