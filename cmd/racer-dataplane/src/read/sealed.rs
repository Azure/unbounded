//! Sealed pages: ciphertext a GPU authenticates and decrypts itself.
//!
//! A `gpu-read` asks its range stream for sealed pages
//! ([`super::range_stream::RangeStream::set_sealed`]). Pages that are verified
//! plaintext in memory still arrive as [`Fetched::Page`], and so do pages that
//! the CPU had to produce anyway (origin fills, which encrypt) or that another
//! reader authenticated first. Every other page arrives as a [`SealedPage`]:
//! the envelope, the AAD, the expected CRC, the XAES-derived page key, and the
//! ciphertext in host memory or already in GPU staging.
//!
//! The derived key is wiped on drop and is never logged: [`SealedPage`] has no
//! `Debug` implementation.

use crate::error::Error;
use crate::error::Result;
use crate::gpu::GpuHelper;
use crate::gpu::StagedCiphertext;
use crate::memory::PageResult;
use crate::memory::UnverifiedPage;
use crate::model::ObjectMetadata;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::store::ReadToken;
use std::sync::Arc;

/// Where a GPU should read a sealed page from.
#[derive(Clone)]
pub struct SealedTarget {
    pub(crate) helper: Arc<GpuHelper>,

    /// The GPU that holds the request's allocation.
    pub(crate) ordinal: i32,
}

impl SealedTarget {
    /// The GPU that holds the request's allocation.
    pub fn ordinal(&self) -> i32 {
        self.ordinal
    }
}

/// One fetched window page in sealed mode.
#[allow(clippy::large_enum_variant)] // Pages move unboxed through the mailbox, as `Value::Page` does.
pub enum Fetched {
    /// Verified plaintext.
    Page(PageResult),

    /// Ciphertext for the GPU to verify and decrypt.
    Sealed(Box<SealedPage>),
}

/// Where a sealed page's ciphertext is.
#[allow(clippy::large_enum_variant)] // Always inside a boxed SealedPage.
pub enum SealedSource {
    /// Ciphertext and tag in host memory: retained, pending, read from disk
    /// through the slab, or received from a peer.
    Host(UnverifiedPage),

    /// Ciphertext already in the target GPU's staging memory, read from a
    /// local v2 disk record with cuFile.
    Staged {
        staged: StagedCiphertext,

        /// The disk mapping the bytes came from.
        token: ReadToken,
    },
}

/// A page that only the GPU will authenticate. Nothing here is proof of
/// authenticity: the GPU checks the CRC and the AEAD tag before it writes.
pub struct SealedPage {
    pub metadata: ObjectMetadata,

    pub envelope: PageEnvelope,

    /// `security::page_aad(&envelope)`.
    pub aad: Vec<u8>,

    /// The XAES-derived AES-256-GCM key and nonce. Wiped on drop.
    pub key: racer_crypto::DerivedPageKey,

    /// The stored CRC-64 of ciphertext and tag, when known.
    pub expected_crc: Option<u64>,

    pub source: SealedSource,
}

/// What a page's owning worker needs to act on the GPU's verdict.
#[derive(Clone)]
pub struct SealedReport {
    pub(crate) page: PageId,

    /// The host copy, to drop from memory on failure or persist on success.
    pub(crate) copy: Option<UnverifiedPage>,

    /// The disk mapping a staged copy came from, to drop on failure.
    pub(crate) token: Option<ReadToken>,
}

impl SealedPage {
    /// Owner-side facts about where the bytes came from. Cheap: it shares
    /// the ciphertext and copies no secrets.
    pub fn report(&self) -> SealedReport {
        let (copy, token) = match &self.source {
            SealedSource::Host(copy) => (Some(copy.clone()), None),
            SealedSource::Staged { token, .. } => (None, Some(token.clone())),
        };
        SealedReport {
            page: self.page().clone(),
            copy,
            token,
        }
    }

    /// The page identity from the envelope.
    pub fn page(&self) -> &PageId {
        &self.envelope.page
    }

    /// Plaintext bytes in the whole page.
    pub fn plaintext_length(&self) -> u32 {
        self.envelope.plaintext_length
    }

    /// Structural checks only: the identity, lengths and AAD bound agree.
    pub fn validate_for(&self, page: &PageId) -> Result<()> {
        self.metadata.validate()?;
        self.envelope.validate()?;
        if self.page() != page
            || self.metadata.version != page.version
            || self.metadata.immutable().page_length(page)? != self.envelope.plaintext_length
            || self.aad.len() > racer_gpu::MAX_AAD
        {
            return Err(Error::CorruptRecord);
        }
        if let SealedSource::Host(copy) = &self.source {
            let ciphertext = &copy.copy.ciphertext;
            if ciphertext.envelope() != &self.envelope
                || ciphertext.bytes().len() != self.envelope.ciphertext_length as usize
            {
                return Err(Error::CorruptRecord);
            }
        }
        Ok(())
    }
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::memory::CiphertextCopy;

    /// A host-sourced sealed page for `result`, with a fixed test key.
    pub(crate) fn sealed(result: &PageResult) -> SealedPage {
        let envelope = result.ciphertext.envelope().clone();
        SealedPage {
            metadata: result.metadata.clone(),
            aad: crate::security::page_aad(&envelope).unwrap_or_default(),
            key: racer_crypto::xaes_derive(&[7; 32], &envelope.nonce.0),
            expected_crc: Some(result.ciphertext.checksum()),
            envelope,
            source: SealedSource::Host(UnverifiedPage {
                copy: CiphertextCopy {
                    metadata: result.metadata.clone(),
                    ciphertext: result.ciphertext.clone(),
                },
                disk_token: None,
            }),
        }
    }

    #[test]
    fn validate_checks_identity_lengths_and_aad_bound() {
        use crate::model::*;
        let admission = flow_control::Quotas::new(crate::admission::AdmissionPolicy::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let metadata = ObjectMetadata {
            content_type: None,
            version: ObjectVersion {
                object: ObjectId {
                    cache: racer_control_wire::CacheId(
                        "00000000-0000-4000-8000-000000000001".into(),
                    ),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            length: 37,
            expires_at: ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap(),
        };
        let result = crate::read::range_stream::tests::page_result(&admission, &metadata, 0);
        let page = result.plaintext.page().clone();
        let mut sealed = sealed(&result);
        sealed.validate_for(&page).unwrap();
        assert_eq!(sealed.plaintext_length(), 37);

        let mut other = page.clone();
        other.number = crate::model::PageNumber(1);
        assert_eq!(sealed.validate_for(&other), Err(Error::CorruptRecord));

        sealed.aad = vec![0; racer_gpu::MAX_AAD + 1];
        assert_eq!(sealed.validate_for(&page), Err(Error::CorruptRecord));

        let mut sealed = self::sealed(&result);
        sealed.envelope.plaintext_length += 1;
        sealed.envelope.ciphertext_length += 1;
        assert!(sealed.validate_for(&page).is_err());
    }
}
