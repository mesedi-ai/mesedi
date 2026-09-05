package main

// PORTED VERBATIM from verdifax-orchestrator/internal/rekorverify on
// 2026-09-05. Only the package clause changed; the code and its
// comments above are unmodified.
//
// COPIED RATHER THAN IMPORTED, for three reasons in order of weight: it
// lives under internal/ in a different Go module and is not importable
// at all; this binary is MIT and must not acquire a dependency on a
// private repository; and its worth rests on an auditor being able to
// read the whole thing, which a cross-repo dependency defeats.
//
// THE TWO COPIES MUST STAY IN STEP AND NO COMPILER WILL ENFORCE IT. If
// the Rekor proof format or the checkpoint signature scheme changes,
// both move together. The test file came across with the code for
// exactly that reason: it is the record of what these checks are meant
// to catch.

// RekorPublicKeyPEM is the production Sigstore Rekor public key used
// to verify signed log checkpoints. This value MUST be populated
// before the verifier can perform any offline Rekor proof
// verification, leaving it empty causes loadRekorPublicKey to return
// a clear "populate this file" error rather than silently accepting
// any key.
//
// HOW TO POPULATE
// ────────────────
//
// Sigstore's Rekor instance publishes its public key at:
//
//	https://rekor.sigstore.dev/api/v1/log/publicKey
//
// Fetch it once during deployment provisioning:
//
//	curl -sS https://rekor.sigstore.dev/api/v1/log/publicKey
//
// The response is a PEM block of the form:
//
//	-----BEGIN PUBLIC KEY-----
//	MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE...
//	...
//	-----END PUBLIC KEY-----
//
// Copy it verbatim (including the BEGIN/END lines and trailing
// newline) into the constant below. Commit. Rebuild the verifier.
//
// HOW TO ROTATE
// ─────────────
//
// Sigstore rotates its keys infrequently (the current key has been
// stable for years). Rotation is announced via:
//
//   - the Sigstore TUF root: https://tuf-repo-cdn.sigstore.dev/
//   - the public sigstore-keyring: https://github.com/sigstore/root-signing
//   - the Sigstore Slack and mailing list
//
// When rotation happens, rebuild the verifier with the new key. Old
// audit bundles signed under the old key will no longer verify
// against the new key, operators may want to keep both keys
// available during a transition window. The simplest extension is to
// turn this constant into a slice and have loadRekorPublicKey try
// each key in turn until one verifies.
//
// HOW TO VERIFY YOU HAVE THE RIGHT KEY
// ────────────────────────────────────
//
// The Rekor LogID is the hex SHA-256 of the DER-encoded public key.
// Compute it locally:
//
//	echo -n "$(cat rekor_public_key.pem)" | \
//	  openssl pkey -pubin -outform DER | \
//	  sha256sum
//
// Compare against the LogID embedded in any recent Rekor entry
// (visible at https://search.sigstore.dev/?logIndex=<any-recent-index>).
// They must match. If they don't, the key is wrong, do not commit it.
//
// SECURITY NOTE
// ─────────────
//
// This is a TRUSTED ROOT. Any compromise of this constant means an
// attacker can forge anchored entries. Treat changes to this file
// with the same scrutiny as changes to /etc/ssl/certs.
// Populated 2026-05-29 from https://rekor.sigstore.dev/api/v1/log/publicKey
// (production Sigstore Rekor instance). ECDSA P-256 public key used by
// Rekor to sign log checkpoints. Re-verifying any Rekor inclusion proof
// against the public log requires this key.
const RekorPublicKeyPEM = `-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAE2G2Y+2tabdTV5BcGiBIx0a9fAFwr
kBbmLSGtks4L3qX6yYY0zufBnhC8Ur/iy55GhWP/9A/bY2LhC30M9+RYtw==
-----END PUBLIC KEY-----
`
