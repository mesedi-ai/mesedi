// TOTP secret encryption helper.
//
// Customer TOTP secrets at rest are encrypted with AES-256-GCM. The
// key lives in the `MESEDI_TOTP_ENCRYPTION_KEY` Fly secret as a
// 64-character hex string (32 raw bytes). Without that key, the
// stored `user_totp.secret_encrypted` ciphertext is useless even if
// the database is exfiltrated, an attacker cannot replay a
// customer's TOTP without it.
//
// Why GCM, not CBC: GCM gives authenticated encryption (the
// ciphertext is tamper-evident) so we don't have to add a separate
// HMAC. The nonce is generated fresh per encryption and stored
// alongside the ciphertext.
//
// Storage shape: [nonce (12 bytes) | ciphertext | tag (16 bytes)]
// concatenated into one byte slice. encryptTOTPSecret returns this
// layout; decryptTOTPSecret expects it.
//
// Rotation: not handled in this commit. When we rotate the key, every
// stored row needs to be re-encrypted. Filed as a post-launch
// infrastructure task (lineage). For now the key lives in Fly
// secrets like everything else.
package api

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
)

// totpKeyLen is the required raw byte length of the encryption key
// (AES-256 takes a 256-bit / 32-byte key).
const totpKeyLen = 32

// totpNonceLen is the GCM standard nonce length (96 bits / 12 bytes).
const totpNonceLen = 12

// ParseTOTPEncryptionKey decodes the hex-encoded key from the Fly
// secret into raw bytes. Returns a clear error if the key is the
// wrong length or contains non-hex characters so the operator sees a
// boot-time misconfiguration loudly instead of silently running with
// a broken encryption path.
func ParseTOTPEncryptionKey(hexKey string) ([]byte, error) {
	if hexKey == "" {
		return nil, errors.New("totp encryption key is empty (MESEDI_TOTP_ENCRYPTION_KEY)")
	}
	raw, err := hex.DecodeString(hexKey)
	if err != nil {
		return nil, fmt.Errorf("totp encryption key is not valid hex: %w", err)
	}
	if len(raw) != totpKeyLen {
		return nil, fmt.Errorf("totp encryption key must be %d bytes (%d hex chars), got %d", totpKeyLen, totpKeyLen*2, len(raw))
	}
	return raw, nil
}

// encryptTOTPSecret seals a TOTP secret with AES-256-GCM. Returns
// the layout `[nonce | ciphertext | tag]` ready for persistence in
// the `user_totp.secret_encrypted` column.
func encryptTOTPSecret(key []byte, plaintext []byte) ([]byte, error) {
	if len(key) != totpKeyLen {
		return nil, fmt.Errorf("encrypt: key must be %d bytes, got %d", totpKeyLen, len(key))
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("encrypt: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("encrypt: cipher.NewGCM: %w", err)
	}
	nonce := make([]byte, totpNonceLen)
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, fmt.Errorf("encrypt: read random nonce: %w", err)
	}
	// gcm.Seal appends ciphertext+tag to the supplied dst (nonce here)
	// so the returned slice is the full [nonce | ciphertext | tag]
	// layout the persistence path expects.
	return gcm.Seal(nonce, nonce, plaintext, nil), nil
}

// decryptTOTPSecret inverts encryptTOTPSecret. Returns an error on
// any tampering (GCM tag mismatch) or on a payload too short to
// carry a nonce.
func decryptTOTPSecret(key []byte, payload []byte) ([]byte, error) {
	if len(key) != totpKeyLen {
		return nil, fmt.Errorf("decrypt: key must be %d bytes, got %d", totpKeyLen, len(key))
	}
	if len(payload) < totpNonceLen {
		return nil, fmt.Errorf("decrypt: payload too short (%d bytes) to carry a %d-byte nonce", len(payload), totpNonceLen)
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, fmt.Errorf("decrypt: aes.NewCipher: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("decrypt: cipher.NewGCM: %w", err)
	}
	nonce, ciphertext := payload[:totpNonceLen], payload[totpNonceLen:]
	plaintext, err := gcm.Open(nil, nonce, ciphertext, nil)
	if err != nil {
		return nil, fmt.Errorf("decrypt: gcm.Open: %w", err)
	}
	return plaintext, nil
}
