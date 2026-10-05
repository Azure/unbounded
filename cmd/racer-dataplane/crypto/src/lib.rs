//! Small cryptographic primitives with caller-supplied keys, nonces, and buffers.
//! No entropy source, protocol framing, identity policy, or key derivation policy.
//!
//! Callers supply entropy, ensure nonce uniqueness for each key, and choose domain
//! separation. AEAD operations borrow input and write directly to caller-owned
//! output without allocating or staging plaintext. Public-key parsing does not
//! establish identity or trust. CRC detects accidental corruption, not forgery.
//!
//! Callers must protect and erase their input seeds, key buffers, and imported
//! DER. Secret-key ownership and explicit PKCS#8 export retain upstream
//! zeroization. The explicit `cipher/zeroize` and `poly1305/zeroize` dependency
//! features are required in addition to `chacha20poly1305/zeroize`.
//!
//! Integration tests cover reusable buffers, shared-key messages, standard
//! vectors, rejection cases, persisted signing keys, and an independent CRC
//! reference. They do not establish timing or secret-erasure guarantees. Protocol
//! trust, replay, rotation, and admitted-buffer lifetimes belong to callers.
//! Run `cargo test -p racer-crypto` from the parent workspace. Select ignored CRC
//! hardware tests individually on supported x86 or AArch64 hosts; they fail on
//! unsupported hardware rather than silently skipping.

#![forbid(unsafe_code)]
#![deny(missing_docs)]

use chacha20poly1305::{
    KeyInit, Tag, XChaCha20Poly1305,
    aead::{AeadInOut, inout::InOutBuf},
};
use ed25519_dalek::{
    Signature, Signer,
    pkcs8::{DecodePrivateKey, EncodePrivateKey},
};
use sha2::{Digest, Sha256};
use zeroize::Zeroizing;

/// Opaque failure of a cryptographic operation or input validation.
/// No application-specific error classification or size policy is imposed.
#[derive(Debug)]
pub struct Error(());

/// Secret signing key. Intentionally does not implement Debug or Clone.
/// The upstream key zeroizes its secret on drop; callers own their seed's lifetime.
pub struct SigningKey(ed25519_dalek::SigningKey);

/// Public verification key. Parsing does not establish trust or identity.
#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct VerifyingKey(ed25519_dalek::VerifyingKey);

/// Number of authentication-tag bytes appended to XChaCha20-Poly1305 ciphertext.
pub const TAG_LEN: usize = 16;

impl SigningKey {
    /// Import a caller-supplied secret seed. No randomness is generated here.
    pub fn from_seed(seed: &[u8; 32]) -> Self {
        Self(ed25519_dalek::SigningKey::from_bytes(seed))
    }

    /// Import an Ed25519 PKCS#8 DER document, rejecting malformed key material.
    pub fn from_pkcs8_der(der: &[u8]) -> Result<Self, Error> {
        ed25519_dalek::SigningKey::from_pkcs8_der(der)
            .map(Self)
            .map_err(|_| Error(()))
    }

    /// Explicit secret export. Both the temporary DER document and returned
    /// bytes are zeroized on drop.
    pub fn to_pkcs8_der(&self) -> Result<Zeroizing<Vec<u8>>, Error> {
        let document = self.0.to_pkcs8_der().map_err(|_| Error(()))?;
        Ok(Zeroizing::new(document.as_bytes().to_vec()))
    }

    /// Return the public verification key corresponding to this signing key.
    pub fn verifying_key(&self) -> VerifyingKey {
        VerifyingKey(self.0.verifying_key())
    }

    /// Sign the exact message bytes without adding framing or domain separation.
    pub fn sign(&self, msg: &[u8]) -> [u8; 64] {
        self.0.sign(msg).to_bytes()
    }
}

impl VerifyingKey {
    /// Parse an encoded public key without establishing trust in its owner.
    pub fn from_bytes(bytes: &[u8; 32]) -> Result<Self, Error> {
        ed25519_dalek::VerifyingKey::from_bytes(bytes)
            .map(Self)
            .map_err(|_| Error(()))
    }

    /// Borrow the public key's 32-byte encoding.
    pub fn as_bytes(&self) -> &[u8; 32] {
        self.0.as_bytes()
    }

    /// Reject malformed signatures and use upstream strict verification,
    /// including its small-order point and scalar malleability checks.
    pub fn verify_strict(&self, msg: &[u8], sig: &[u8]) -> Result<(), Error> {
        let signature = Signature::from_slice(sig).map_err(|_| Error(()))?;
        self.0.verify_strict(msg, &signature).map_err(|_| Error(()))
    }
}

impl std::fmt::Display for Error {
    /// Describe the failure without exposing inputs or cryptographic details.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("cryptographic operation failed")
    }
}

impl std::error::Error for Error {}

/// Seal into exactly `plaintext.len() + TAG_LEN` bytes as ciphertext followed by tag.
/// The caller must never reuse a nonce with the same key.
/// No allocation or plaintext staging copy is performed.
pub fn seal(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    plaintext: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    if plaintext.len().checked_add(TAG_LEN) != Some(out.len()) {
        return Err(Error(()));
    }
    let (ciphertext, tag_out) = out.split_at_mut(plaintext.len());
    let buffer = InOutBuf::new(plaintext, ciphertext).map_err(|_| Error(()))?;
    let tag = XChaCha20Poly1305::new(key.into())
        .encrypt_inout_detached(nonce.into(), aad, buffer)
        .map_err(|_| Error(()))?;
    tag_out.copy_from_slice(&tag);
    Ok(())
}

/// Open ciphertext followed by tag into exactly `sealed.len() - TAG_LEN` bytes.
/// Every error leaves output untouched, including malformed input/output lengths.
/// Authentication precedes decryption; no allocation or plaintext staging copy is performed.
pub fn open(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    sealed: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    let length = sealed.len().checked_sub(TAG_LEN).ok_or(Error(()))?;
    if out.len() != length {
        return Err(Error(()));
    }
    let (ciphertext, tag) = sealed.split_at(length);
    let tag = Tag::try_from(tag).map_err(|_| Error(()))?;
    let buffer = InOutBuf::new(ciphertext, out).map_err(|_| Error(()))?;
    // Upstream detached decryption validates length and authenticates the input
    // before applying the keystream to output. Keep that ordering on upgrades.
    XChaCha20Poly1305::new(key.into())
        .decrypt_inout_detached(nonce.into(), aad, buffer, &tag)
        .map_err(|_| Error(()))
}

/// HMAC-SHA256 with a fixed 32-byte key. Domain separation belongs to the caller.
pub fn hmac_sha256(key: &[u8; 32], msg: &[u8]) -> [u8; 32] {
    let mut inner = Zeroizing::new([0x36; 64]);
    let mut outer = Zeroizing::new([0x5c; 64]);
    for i in 0..32 {
        inner[i] ^= key[i];
        outer[i] ^= key[i];
    }
    let mut hash = Sha256::new();
    hash.update(inner.as_slice());
    hash.update(msg);
    let digest = hash.finalize();
    let mut hash = Sha256::new();
    hash.update(outer.as_slice());
    hash.update(digest);
    hash.finalize().into()
}

/// Compare equal-length slices without data-dependent early exits.
/// Lengths are not secret: unequal lengths return immediately.
pub fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    a.iter().zip(b).fold(0u8, |diff, (a, b)| diff | (a ^ b)) == 0
}

/// CRC-64/XZ checksum. Detects accidental corruption, not malicious changes.
pub fn crc64(bytes: &[u8]) -> u64 {
    let mut digest = crc64fast::Digest::new();
    digest.write(bytes);
    digest.sum64()
}
