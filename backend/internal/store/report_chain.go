package store

import "errors"

// Report chaining and signing (migration 067). Shared types.

// Report chain statuses recorded per run at completion.
const (
	ReportChainUnchained = "unchained"
	ReportChainChained   = "chained"
	ReportChainSigned    = "signed"
	ReportChainBroken    = "broken"
)

// ReportChainRow is one chained report as the server received it.
type ReportChainRow struct {
	EventID       string
	ExecutionID   string
	ProjectID     string
	PrevHash      string
	Hash          string
	PayloadSHA256 string
	KeyID         string
	Signature     string
}

// ReportChainStatusRow is a run's chain verdict and the end its SDK
// declared when the run finished ("" when none was).
type ReportChainStatusRow struct {
	ExecutionID  string
	ProjectID    string
	Status       string
	Detail       string
	EndHash      string
	EndKeyID     string
	EndSignature string
}

// ReportSigningKey is a registered Ed25519 public key (base64, 32
// bytes). RevokedAt is "" while the key is in use.
type ReportSigningKey struct {
	KeyID     string `json:"key_id"`
	PublicKey string `json:"public_key"`
	CreatedAt string `json:"created_at"`
	CreatedBy string `json:"created_by,omitempty"`
	RevokedAt string `json:"revoked_at,omitempty"`
}

// ErrReportSigningKeyExists is returned when a key_id is registered a
// second time. Keys are immutable: a changed key gets a new key_id, so
// a signature can always be traced to exactly one public key.
var ErrReportSigningKeyExists = errors.New("report signing key id already registered")
