# Mesedi's anchor signing keys

Since 16 November 2026, Mesedi signs every hourly checkpoint it publishes
on Sigstore's public Rekor log with its own key. These are the public
halves of its two keys. mesedi-verify trusts exactly these two, and
reports any directly anchored hour signed by another key as a failure.

| Key | Role | SHA-256 of the public key (DER) |
|---|---|---|
| 1 | Signs every hourly checkpoint | `eccad116fe144bd3ca0da53453eac1180d0981e0833ff756b18580c3648e5247` |
| 2 | Published replacement for key 1 | `f7ad3f9ce0b4e55ae144ed574944b9b1854cd8b0c7461f1941484a191a041e44` |

Check a fingerprint yourself:

    openssl pkey -pubin -in key.pem -outform DER | shasum -a 256

## Custody

Both are ECDSA P-256 keys, created on 16 November 2026 on the owner's
own machine with OpenSSL. Each private key exists only encrypted
(AES-256, with a random passphrase), stored with its passphrase in the
owner's password manager; no unencrypted copy was kept, and neither key
is in any repository. When anchoring runs, key 1 is held only as a
secret of the hosted backend.

Key 2 is published now so that losing or retiring key 1 never strands
the chain: a verifier built today already trusts the replacement.

Hours anchored before 16 November 2026 were signed by the third-party
service used then; mesedi-verify says their signer is not checked.

## Key 1

```
-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEVfEcOm4AzR+9YWvm13txE1cboCHX
+3/s/pX29Vidph2AJnjyXbgWVRbnByk0WVIdxL5GaRFH1fmLt5uYJMM9eQ==
-----END PUBLIC KEY-----
```

## Key 2

```
-----BEGIN PUBLIC KEY-----
MFkwEwYHKoZIzj0CAQYIKoZIzj0DAQcDQgAEEACCZ7nAkkogbezTSvyg1Ec3VJI/
YTnvCpiPJfa7jgbrAZxhiwI9xfDmAA8gwNeZjYizLwSqgji/+bB5VmahFQ==
-----END PUBLIC KEY-----
```
