# racer-crypto

Minimal, synchronous adapters for XChaCha20-Poly1305, fixed-key HMAC-SHA256,
byte equality, Ed25519, and CRC-64/XZ. This crate contains no Racer policy or
types: no key epochs, identities, certificate trust, framing, domain separation,
admission, workers, or I/O.

- `aead::seal` writes ciphertext followed by a 16-byte tag. Output must be
  exactly plaintext length plus 16. `aead::open` requires exactly ciphertext
  length minus 16 and leaves output untouched on every error. Both use upstream
  detached inout operations without allocating or staging plaintext.
- Keys are 32 bytes and XChaCha nonces are 24 bytes. **Callers supply entropy and
  ensure nonce uniqueness per key.** There is no entropy source in this crate.
- `hmac_sha256` takes a 32-byte key. Callers choose domain separation.
  `ct_eq` folds all equal-length bytes without data-dependent early exits;
  differing lengths return immediately and lengths are not treated as secret.
- `ed25519::SigningKey` imports caller-supplied seeds or PKCS#8 DER, signs, and
  explicitly exports zeroizing PKCS#8 bytes. It is neither Debug nor Clone.
  Upstream secret-key zeroization is retained. Callers must protect and erase
  their own input seeds, key buffers, and imported DER.
- `ed25519::VerifyingKey` parses public keys and exposes strict verification;
  parsing does not establish identity or trust.
- `crc64` is CRC-64/XZ for accidental corruption detection, **not authentication**.

Failures use one opaque `Error` implementing `Display` and `std::error::Error`.
No application-specific error classification or size policy is imposed.
The explicit `cipher/zeroize` and `poly1305/zeroize` dependency feature pins
are required in addition to `chacha20poly1305/zeroize`.

Run `cargo test -p racer-crypto` from the parent workspace. The two ignored CRC
hardware tests must be selected individually on supported x86 or AArch64 hosts;
they deliberately fail on unsupported hardware rather than silently skip.

Coverage starts with two small public-API scenarios in `tests/workflows.rs`:
exchanging records in reusable caller-owned buffer slices (including empty
records and rejection followed by retry), and authenticating shared-key messages
(including empty and multi-block messages, changed messages/keys/tags, and
truncated tags). `tests/primitives.rs` also restores a persisted PKCS#8 signing
key and verifies a new signature using only exported public-key bytes.

Independent standard vectors, exhaustive tamper and malformed-length checks,
strict Ed25519 rejection cases, malformed PKCS#8 documents, and CRC reference
comparisons remain separate regression coverage. These tests do not establish
timing guarantees or secret zeroization; protocol trust, replay, key rotation,
and admitted-buffer lifecycle tests belong to the parent dataplane.
