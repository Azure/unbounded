//! Object protocol values plus application-owned page encryption descriptors.

use crate::error::{Error, Result};
use racer_control_wire::KeyId;
pub use racer_object_wire::model::*;

/// Authentication tag size in the application's page encryption format.
pub const AEAD_TAG_BYTES: u32 = 16;

/// Application configuration constructor, distinct from wire syntax errors.
pub fn key_id_from_generation(generation: u64, suffix: u32) -> Result<KeyId> {
    KeyId::from_generation(generation, suffix).map_err(|_| Error::InvalidConfiguration)
}

/// Application-owned XChaCha20 nonce.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Nonce(pub [u8; 24]);

/// Immutable encryption descriptor shared by memory, peers, and storage.
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
    /// Check structural bounds, not identity, final-page length, or authentication.
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

impl PageDescriptor for PageEnvelope {
    fn validate_structure(&self) -> racer_object_wire::Result<()> {
        self.validate()
            .map_err(|_| racer_object_wire::Error::CorruptRecord)
    }

    fn page(&self) -> &PageId {
        &self.page
    }

    fn plaintext_length(&self) -> u32 {
        self.plaintext_length
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use racer_control_wire::CacheId;

    fn descriptor(etag: &str, length: u64) -> VersionMetadata {
        VersionMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value(etag),
            },
            length,
        }
    }

    #[test]
    fn page_bounds_use_total_version_length_and_reject_empty_overflow_and_mismatch() {
        let descriptor = descriptor("v1", PAGE_BYTES + 3);
        let mut envelope = PageEnvelope {
            page: PageId {
                version: descriptor.version.clone(),
                number: PageNumber(1),
            },
            key_id: KeyId([0; 16]),
            nonce: Nonce([0; 24]),
            plaintext_length: 3,
            ciphertext_length: 19,
        };
        let corrupt = Err(racer_object_wire::Error::CorruptRecord);
        assert_eq!(descriptor.validate_page(&envelope), Ok(()));
        envelope.ciphertext_length = 18;
        assert_eq!(descriptor.validate_page(&envelope), corrupt);
        envelope.ciphertext_length = 19;
        envelope.plaintext_length = 4;
        assert_eq!(descriptor.validate_page(&envelope), corrupt);
        envelope.plaintext_length = 3;
        for number in [2, u64::MAX] {
            envelope.page.number = PageNumber(number);
            assert_eq!(descriptor.validate_page(&envelope), corrupt);
        }
        envelope.page.number = PageNumber(0);
        let empty = VersionMetadata {
            length: 0,
            ..descriptor.clone()
        };
        assert_eq!(empty.validate_page(&envelope), corrupt);
        envelope.page.version.etag = StrongEtag::test_value("other");
        assert_eq!(descriptor.validate_page(&envelope), corrupt);
    }

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
