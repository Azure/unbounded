//! Small cryptographic primitives with caller-supplied keys, nonces, and buffers.
//! No entropy source, protocol framing, identity policy, or key derivation policy.

#![forbid(unsafe_code)]

pub mod aead;
pub mod ed25519;

use sha2::{Digest, Sha256};
use zeroize::Zeroizing;

/// Opaque failure of a cryptographic operation or input validation.
#[derive(Debug)]
pub struct Error(());

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("cryptographic operation failed")
    }
}

impl std::error::Error for Error {}

/// HMAC-SHA256 with a fixed 32-byte key. Domain separation belongs to the caller.
pub fn hmac_sha256(key: &[u8; 32], msg: &[u8]) -> [u8; 32] {
    let mut inner = Zeroizing::new([0x36; 64]);
    let mut outer = Zeroizing::new([0x5c; 64]);
    for i in 0..32 {
        inner[i] ^= key[i];
        outer[i] ^= key[i];
    }
    let mut hash = Sha256::new();
    hash.update(&*inner);
    hash.update(msg);
    let digest = hash.finalize();
    let mut hash = Sha256::new();
    hash.update(&*outer);
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
