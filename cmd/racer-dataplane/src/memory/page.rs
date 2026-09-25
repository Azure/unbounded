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
    /// Structural agreement only. Authentication remains the fill/crypto boundary.
    pub fn validate_metadata(&self) -> crate::error::Result<()> {
        let descriptor = self.metadata.immutable();
        descriptor.validate_page(self.ciphertext.envelope())?;
        if self.plaintext.page() != &self.ciphertext.envelope().page
            || self.plaintext.bytes().len() as u64
                != u64::from(self.ciphertext.envelope().plaintext_length)
        {
            return Err(crate::error::Error::CorruptRecord);
        }
        Ok(())
    }
    pub fn copy(&self) -> CiphertextCopy {
        CiphertextCopy {
            metadata: self.metadata.clone(),
            ciphertext: self.ciphertext.clone(),
        }
    }
}
