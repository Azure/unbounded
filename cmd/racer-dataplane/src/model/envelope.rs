//! Immutable page encryption descriptors shared by memory, peers, and storage.
//!
//! Disk padding is outside the authenticated ciphertext length. HTTP signatures
//! authenticate this descriptor; XChaCha20-Poly1305 authenticates page bytes.

use super::{identity::PageId, range::PAGE_BYTES};
use crate::error::{Error, Result};

pub const AEAD_TAG_BYTES: u32 = 16;

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

impl PageEnvelope {
    /// Structural bounds only. Identity and final-page length require the version
    /// descriptor; authenticity requires successful AEAD verification.
    pub fn validate(&self) -> Result<()> {
        if self.plaintext_length == 0
            || u64::from(self.plaintext_length) > PAGE_BYTES
            || self.plaintext_length.checked_add(AEAD_TAG_BYTES) != Some(self.ciphertext_length)
        {
            return Err(Error::CorruptRecord);
        }
        Ok(())
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::model::identity::{
        CacheId, CacheKey, ObjectId, ObjectVersion, PageNumber, StrongEtag,
    };

    #[test]
    fn envelope_enforces_nonempty_bounded_plaintext_and_exact_tag_length() {
        let mut envelope = PageEnvelope {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId("cache".into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::parse(b"\"v1\"").unwrap(),
                },
                number: PageNumber(0),
            },
            key_id: KeyId([0; 16]),
            nonce: Nonce([0; 24]),
            plaintext_length: 1,
            ciphertext_length: 17,
        };
        for length in [1, PAGE_BYTES as u32] {
            envelope.plaintext_length = length;
            envelope.ciphertext_length = length + AEAD_TAG_BYTES;
            assert_eq!(envelope.validate(), Ok(()));
            envelope.ciphertext_length += 1;
            assert_eq!(envelope.validate(), Err(Error::CorruptRecord));
        }
        for (plaintext, ciphertext) in [
            (0, 16),
            (PAGE_BYTES as u32 + 1, PAGE_BYTES as u32 + 17),
            (u32::MAX, 15),
        ] {
            envelope.plaintext_length = plaintext;
            envelope.ciphertext_length = ciphertext;
            assert_eq!(envelope.validate(), Err(Error::CorruptRecord));
        }
    }
}
