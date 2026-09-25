//! Immutable page encryption descriptors shared by memory, peers, and storage.
//!
//! Disk padding is outside the authenticated ciphertext length. HTTP signatures
//! authenticate this descriptor; XChaCha20-Poly1305 authenticates page bytes.

use super::identity::PageId;

#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct KeyId(pub [u8; 16]);
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Nonce(pub [u8; 24]);

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PageEnvelope {
    pub page: PageId,
    pub key_id: KeyId,
    pub nonce: Nonce,
    pub plaintext_length: u32,
    /// Includes the AEAD tag, excludes disk alignment padding and record headers.
    pub ciphertext_length: u32,
}

#[cfg(test)]
mod tests {
    // Cover descriptor bounds, tag length, wrong identity, and final short pages.
}
