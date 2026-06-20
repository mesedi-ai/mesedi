package api

import (
	"bytes"
	"encoding/hex"
	"strings"
	"testing"
)

// validKey is a 32-byte / 64-hex-character key used across tests.
const validKey = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestParseTOTPEncryptionKey_Valid(t *testing.T) {
	raw, err := parseTOTPEncryptionKey(validKey)
	if err != nil {
		t.Fatalf("expected valid key to parse, got: %v", err)
	}
	if len(raw) != totpKeyLen {
		t.Fatalf("expected %d-byte key, got %d", totpKeyLen, len(raw))
	}
}

func TestParseTOTPEncryptionKey_EmptyRejected(t *testing.T) {
	if _, err := parseTOTPEncryptionKey(""); err == nil {
		t.Fatal("expected empty key to be rejected; operator must see misconfiguration loudly")
	}
}

func TestParseTOTPEncryptionKey_WrongLengthRejected(t *testing.T) {
	// 32 hex chars = 16 bytes, half what AES-256 needs.
	short := "0123456789abcdef0123456789abcdef"
	if _, err := parseTOTPEncryptionKey(short); err == nil {
		t.Fatal("expected short key to be rejected")
	}
}

func TestParseTOTPEncryptionKey_NonHexRejected(t *testing.T) {
	bogus := strings.Repeat("z", 64) // 64 chars but not valid hex
	if _, err := parseTOTPEncryptionKey(bogus); err == nil {
		t.Fatal("expected non-hex key to be rejected")
	}
}

func TestEncryptDecryptTOTPSecret_RoundTrip(t *testing.T) {
	key, err := parseTOTPEncryptionKey(validKey)
	if err != nil {
		t.Fatalf("parse key: %v", err)
	}
	plaintext := []byte("JBSWY3DPEHPK3PXP") // example TOTP base32 secret
	ciphertext, err := encryptTOTPSecret(key, plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if bytes.Equal(ciphertext, plaintext) {
		t.Fatal("encryption produced cleartext output")
	}
	if len(ciphertext) <= totpNonceLen {
		t.Fatal("ciphertext too short; missing nonce or payload")
	}
	got, err := decryptTOTPSecret(key, ciphertext)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if !bytes.Equal(got, plaintext) {
		t.Fatalf("round trip mismatch: got %q, want %q", got, plaintext)
	}
}

func TestEncryptTOTPSecret_DifferentNoncesPerCall(t *testing.T) {
	// Two encryptions of the same plaintext under the same key MUST
	// produce different ciphertexts (each call mints a fresh nonce).
	// Otherwise an observer who sees two encrypted rows for the same
	// secret could correlate them.
	key, _ := parseTOTPEncryptionKey(validKey)
	plaintext := []byte("JBSWY3DPEHPK3PXP")
	a, _ := encryptTOTPSecret(key, plaintext)
	b, _ := encryptTOTPSecret(key, plaintext)
	if bytes.Equal(a, b) {
		t.Fatal("encrypt should mint a fresh nonce per call; identical ciphertexts is a bug")
	}
}

func TestDecryptTOTPSecret_TamperingRejected(t *testing.T) {
	// Flipping one bit in the ciphertext MUST surface as a decrypt
	// error (GCM tag check). This is what makes "encrypted at rest"
	// meaningful — an attacker with DB access cannot silently swap
	// in a different TOTP secret.
	key, _ := parseTOTPEncryptionKey(validKey)
	plaintext := []byte("JBSWY3DPEHPK3PXP")
	ciphertext, _ := encryptTOTPSecret(key, plaintext)
	ciphertext[totpNonceLen] ^= 0x01 // flip first byte AFTER the nonce
	if _, err := decryptTOTPSecret(key, ciphertext); err == nil {
		t.Fatal("expected tampered ciphertext to fail GCM tag check")
	}
}

func TestDecryptTOTPSecret_WrongKeyRejected(t *testing.T) {
	key1, _ := parseTOTPEncryptionKey(validKey)
	otherHex := strings.Repeat("ab", 32)
	key2, _ := parseTOTPEncryptionKey(otherHex)
	plaintext := []byte("JBSWY3DPEHPK3PXP")
	ciphertext, _ := encryptTOTPSecret(key1, plaintext)
	if _, err := decryptTOTPSecret(key2, ciphertext); err == nil {
		t.Fatal("decrypting with a different key must fail")
	}
}

func TestDecryptTOTPSecret_TooShortPayloadRejected(t *testing.T) {
	key, _ := parseTOTPEncryptionKey(validKey)
	short, _ := hex.DecodeString("aabb")
	if _, err := decryptTOTPSecret(key, short); err == nil {
		t.Fatal("expected too-short payload to be rejected (cannot carry a nonce)")
	}
}
