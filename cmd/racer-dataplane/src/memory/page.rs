//! Credential-free page results shared by fills, memory, and flight completion.
use super::pool::{CiphertextPage, VerifiedPage};
use crate::model::metadata::ObjectMetadata;

/// Retain all three values for the same version until the last reader releases
/// them. Freshness-pointer eviction must not strand a cached page without length.
#[derive(Clone)]
pub struct PageResult {
    pub metadata: ObjectMetadata,
    pub plaintext: VerifiedPage,
    pub ciphertext: CiphertextPage,
}

/// Pending and completed peer copies retain metadata alongside original ciphertext.
/// The deadline is historical, not permission to advance the current-version pointer.
#[derive(Clone)]
pub struct CiphertextCopy {
    pub metadata: ObjectMetadata,
    pub ciphertext: CiphertextPage,
}

impl PageResult {
    /// An internally consistent bundle still must match the requested flight page.
    /// This is structural validation, not a substitute for authentication.
    pub fn validate_for(&self, page: &crate::model::identity::PageId) -> crate::error::Result<()> {
        if self.plaintext.page() != page {
            return Err(crate::error::Error::CorruptRecord);
        }
        self.validate_metadata()
    }

    /// Structural agreement only. Authentication remains the fill/crypto boundary.
    pub fn validate_metadata(&self) -> crate::error::Result<()> {
        validate_association(
            &self.metadata,
            self.plaintext.page(),
            self.plaintext.bytes().len() as u64,
            self.ciphertext.envelope(),
        )
    }
    pub fn copy(&self) -> CiphertextCopy {
        CiphertextCopy {
            metadata: self.metadata.clone(),
            ciphertext: self.ciphertext.clone(),
        }
    }
}

fn validate_association(
    metadata: &ObjectMetadata,
    plaintext_page: &crate::model::identity::PageId,
    plaintext_length: u64,
    envelope: &crate::model::envelope::PageEnvelope,
) -> crate::error::Result<()> {
    metadata.immutable().validate_page(envelope)?;
    if plaintext_page != &envelope.page || plaintext_length != u64::from(envelope.plaintext_length)
    {
        return Err(crate::error::Error::CorruptRecord);
    }
    Ok(())
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        error::Error,
        model::{
            envelope::{KeyId, Nonce, PageEnvelope},
            identity::{
                CacheId, CacheKey, ObjectId, ObjectVersion, PageId, PageNumber, StrongEtag,
            },
            metadata::ExpiresAt,
        },
    };

    #[test]
    fn shared_result_rejects_mixed_metadata_plaintext_and_ciphertext() {
        let metadata = ObjectMetadata {
            version: ObjectVersion {
                object: ObjectId {
                    cache: CacheId("cache".into()),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: 3,
            expires_at: ExpiresAt(std::time::UNIX_EPOCH),
        };
        let page = PageId {
            version: metadata.version.clone(),
            number: PageNumber(0),
        };
        let envelope = PageEnvelope {
            page: page.clone(),
            key_id: KeyId([0; 16]),
            nonce: Nonce([0; 24]),
            plaintext_length: 3,
            ciphertext_length: 19,
        };
        assert_eq!(validate_association(&metadata, &page, 3, &envelope), Ok(()));
        assert_eq!(
            validate_association(&metadata, &page, 2, &envelope),
            Err(Error::CorruptRecord)
        );
        let mut wrong = page.clone();
        wrong.version.etag = StrongEtag::test_value("v2");
        assert_eq!(
            validate_association(&metadata, &wrong, 3, &envelope),
            Err(Error::CorruptRecord)
        );
        let mut wrong_metadata = metadata.clone();
        wrong_metadata.version.object.key = CacheKey([1; 32]);
        assert_eq!(
            validate_association(&wrong_metadata, &page, 3, &envelope),
            Err(Error::CorruptRecord)
        );
        wrong_metadata = metadata.clone();
        wrong_metadata.length = 4;
        assert_eq!(
            validate_association(&wrong_metadata, &page, 3, &envelope),
            Err(Error::CorruptRecord)
        );
    }
}
