//! XChaCha20-Poly1305 with separate borrowed input and caller-owned output.

use crate::Error;
use chacha20poly1305::{
    KeyInit, Tag, XChaCha20Poly1305,
    aead::{AeadInOut, inout::InOutBuf},
};

pub const KEY_LEN: usize = 32;
pub const NONCE_LEN: usize = 24;
pub const TAG_LEN: usize = 16;

/// Seal into exactly `plaintext.len() + TAG_LEN` bytes as ciphertext followed by tag.
/// The caller must never reuse a nonce with the same key.
/// No allocation or plaintext staging copy is performed.
pub fn seal(
    key: &[u8; KEY_LEN],
    nonce: &[u8; NONCE_LEN],
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
    key: &[u8; KEY_LEN],
    nonce: &[u8; NONCE_LEN],
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
