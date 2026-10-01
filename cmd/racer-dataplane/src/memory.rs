//! Distinct accounting and lifetimes for plaintext, ciphertext, pipes, and kernel I/O.
//! Cache lookups retain the original encrypted page and immutable version metadata.
//! Eviction releases idle leases; retirement hides entries without revoking owners.
pub mod cache;
pub mod delivery;
/// Credential-free page results shared by fills, memory, and flight completion.
pub mod page {
    use super::pool::{CiphertextPage, VerifiedPage};
    use crate::model::ObjectMetadata;

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

    /// Structurally validated acquisition output, not proof of page AEAD. Only a
    /// plaintext consumer may promote this to PageResult after authenticating bytes.
    #[derive(Clone)]
    pub struct UnverifiedPage {
        pub copy: CiphertextCopy,
        pub(crate) disk_token: Option<crate::store::ReadToken>,
    }

    #[derive(Clone)]
    pub enum AcquiredPage {
        Ciphertext(UnverifiedPage),
        Plaintext(PageResult),
    }
    impl AcquiredPage {
        #[cfg(test)]
        pub(crate) fn verified(self) -> PageResult {
            match self {
                Self::Plaintext(page) => page,
                Self::Ciphertext(_) => panic!("expected verified page"),
            }
        }
        pub fn copy(&self) -> CiphertextCopy {
            match self {
                Self::Ciphertext(page) => page.copy.clone(),
                Self::Plaintext(page) => page.copy(),
            }
        }
        pub fn validate_for(&self, page: &crate::model::PageId) -> crate::error::Result<()> {
            if let Self::Plaintext(result) = self {
                return result.validate_for(page);
            }
            let copy = self.copy();
            if &copy.ciphertext.envelope().page != page {
                return Err(crate::error::Error::CorruptRecord);
            }
            copy.validate_metadata()
        }
    }
    impl From<PageResult> for AcquiredPage {
        fn from(page: PageResult) -> Self {
            Self::Plaintext(page)
        }
    }

    impl PageResult {
        /// An internally consistent bundle still must match the requested flight page.
        /// This is structural validation, not a substitute for authentication.
        pub fn validate_for(&self, page: &crate::model::PageId) -> crate::error::Result<()> {
            if self.plaintext.page() != page {
                return Err(crate::error::Error::CorruptRecord);
            }
            self.validate_metadata()
        }

        /// Structural agreement only. Authentication remains the fill/crypto boundary.
        pub fn validate_metadata(&self) -> crate::error::Result<()> {
            validate_ciphertext(&self.metadata, &self.ciphertext)?;
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

    impl CiphertextCopy {
        pub fn validate_metadata(&self) -> crate::error::Result<()> {
            validate_ciphertext(&self.metadata, &self.ciphertext)
        }
    }

    fn validate_ciphertext(
        metadata: &ObjectMetadata,
        ciphertext: &CiphertextPage,
    ) -> crate::error::Result<()> {
        metadata.validate()?;
        metadata.immutable().validate_page(ciphertext.envelope())?;
        if ciphertext.bytes().len() != ciphertext.envelope().ciphertext_length as usize {
            return Err(crate::error::Error::CorruptRecord);
        }
        Ok(())
    }

    fn validate_association(
        metadata: &ObjectMetadata,
        plaintext_page: &crate::model::PageId,
        plaintext_length: u64,
        envelope: &crate::model::PageEnvelope,
    ) -> crate::error::Result<()> {
        metadata.immutable().validate_page(envelope)?;
        if plaintext_page != &envelope.page
            || plaintext_length != u64::from(envelope.plaintext_length)
        {
            return Err(crate::error::Error::CorruptRecord);
        }
        Ok(())
    }

    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::error::Error;
        #[test]
        fn shared_results_reject_truncated_or_padded_ciphertext() {
            let admission = crate::memory::pool::tests::admission(8);
            for length in [0, 18, 19, 20] {
                let mut page = crate::memory::pool::tests::bundle(&admission, "v1");
                std::sync::Arc::get_mut(&mut page.ciphertext.inner)
                    .unwrap()
                    .bytes
                    .resize(length, 0);
                let expected = if length == 19 {
                    Ok(())
                } else {
                    Err(Error::CorruptRecord)
                };
                assert_eq!(page.validate_metadata(), expected);
                assert_eq!(page.copy().validate_metadata(), expected);
            }
        }
        #[test]
        fn shared_result_rejects_mixed_metadata_plaintext_and_ciphertext() {
            let admission = crate::memory::pool::tests::admission(8);
            let bundle = crate::memory::pool::tests::bundle(&admission, "v1");
            let metadata = &bundle.metadata;
            let page = bundle.plaintext.page();
            let envelope = bundle.ciphertext.envelope();
            assert_eq!(validate_association(metadata, page, 3, envelope), Ok(()));
            for case in 0..4 {
                let mut metadata = metadata.clone();
                let mut page = page.clone();
                let mut length = 3;
                match case {
                    0 => length = 2,
                    1 => page.version.etag = crate::model::StrongEtag::test_value("v2"),
                    2 => metadata.version.object.key.0[0] ^= 1,
                    _ => metadata.length = 4,
                }
                assert_eq!(
                    validate_association(&metadata, &page, length, envelope),
                    Err(Error::CorruptRecord)
                );
            }
        }
    }
}
pub mod pipe;
pub mod pool;
