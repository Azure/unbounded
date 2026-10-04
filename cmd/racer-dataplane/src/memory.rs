//! Distinct accounting and lifetimes for plaintext, ciphertext, pipes, and kernel I/O.
//! Cache lookups retain the original encrypted page and immutable version metadata.
//! Eviction releases idle leases; retirement hides entries without revoking owners.
pub mod cache;
pub mod delivery;
use crate::error::Error;
use crate::error::Operation;
use crate::error::Result;
use crate::model::CacheId;
use crate::model::PAGE_BYTES;
use crate::model::PageEnvelope;
use crate::model::PageId;
use crate::model::ResourceClass;

use crate::runtime::admission::AdmissionPolicy;
use crate::runtime::deadline::RequestScope;
use flow_control::Quotas;
use flow_control::pipe::PipeLease;
use flow_control::pipe::PipePool;
use std::rc::Rc;
use std::sync::Arc;
use std::task::Waker;

#[derive(Clone)]
pub struct BufferPool {
    admission: Rc<flow_control::Quotas<AdmissionPolicy>>,
}
/// Mutable staging buffer, not proof of authentication and not client-deliverable.
/// Fixed-size owned backing stays at the same address when this owner moves.
pub struct PlaintextBuffer {
    bytes: Vec<u8>,
    reservation: Option<flow_control::Charge<AdmissionPolicy>>,
}
impl PlaintextBuffer {
    pub(crate) fn into_parts(mut self) -> (Box<[u8]>, flow_control::Charge<AdmissionPolicy>) {
        (
            std::mem::take(&mut self.bytes).into_boxed_slice(),
            self.reservation.take().expect("owned reservation"),
        )
    }
    pub(crate) fn reservation(&self) -> &flow_control::Charge<AdmissionPolicy> {
        self.reservation.as_ref().expect("owned reservation")
    }
}
impl Drop for PlaintextBuffer {
    fn drop(&mut self) {
        if let Some(reservation) = &mut self.reservation {
            reservation.recycle(std::mem::take(&mut self.bytes));
        }
    }
}
// SAFETY: private fixed backing and reservation remain exclusively owned.
unsafe impl uring_runtime::reactor::IoBuffer for PlaintextBuffer {
    type Error = Error;
    fn bytes(&self) -> Result<&[u8]> {
        Ok(&self.bytes)
    }
    fn bytes_mut(&mut self) -> Result<&mut [u8]> {
        Ok(&mut self.bytes)
    }
}
/// Only page authentication/origin validation may create this publishable type.
#[derive(Clone)]
pub struct VerifiedPage {
    pub(crate) inner: Arc<VerifiedBytes>,
}
pub(crate) struct VerifiedBytes {
    pub page: PageId,
    pub bytes: Vec<u8>,
    pub reservation: flow_control::Charge<AdmissionPolicy>,
}
impl Drop for VerifiedBytes {
    fn drop(&mut self) {
        self.reservation.recycle(std::mem::take(&mut self.bytes));
    }
}
#[derive(Clone)]
pub struct CiphertextPage {
    pub(crate) inner: Arc<CiphertextBytes>,
    pub(crate) provenance: Option<crate::telemetry::PeerProvenance>,
}
pub(crate) struct CiphertextBytes {
    pub checksum: std::sync::OnceLock<u64>,
    pub envelope: PageEnvelope,
    pub bytes: Vec<u8>,
    pub reservation: flow_control::Charge<AdmissionPolicy>,
}
impl Drop for CiphertextBytes {
    fn drop(&mut self) {
        self.reservation.recycle(std::mem::take(&mut self.bytes));
    }
}
impl BufferPool {
    pub fn new(admission: Rc<flow_control::Quotas<AdmissionPolicy>>) -> Self {
        Self { admission }
    }
    pub fn plaintext(
        &self,
        mut reservation: flow_control::Charge<AdmissionPolicy>,
        length: usize,
    ) -> Result<PlaintextBuffer> {
        if length == 0 || length > PAGE_BYTES as usize {
            return Err(Error::InvalidConfiguration);
        }
        self.validate_reservation(
            &reservation,
            ResourceClass::Plaintext,
            length,
            reservation.key(),
        )?;
        let bytes = reservation.buffer(length)?;
        // Keep exact charged capacity, but no Box while reactor pointers are live.
        let bytes = bytes.into_boxed_slice().into_vec();
        reservation.shrink(bytes.len())?;
        Ok(PlaintextBuffer {
            bytes,
            reservation: Some(reservation),
        })
    }
    pub fn ciphertext(
        &self,
        mut reservation: flow_control::Charge<AdmissionPolicy>,
        envelope: PageEnvelope,
        bytes: Vec<u8>,
    ) -> Result<CiphertextPage> {
        envelope.validate()?;
        if bytes.len() != envelope.ciphertext_length as usize {
            return Err(Error::CorruptRecord);
        }
        self.validate_reservation(
            &reservation,
            ResourceClass::Ciphertext,
            bytes.capacity(),
            Some(&envelope.page.version.object.cache),
        )?;
        reservation.shrink(bytes.capacity())?;
        Ok(CiphertextPage {
            provenance: None,
            inner: Arc::new(CiphertextBytes {
                checksum: std::sync::OnceLock::new(),
                envelope,
                bytes,
                reservation,
            }),
        })
    }
    fn entry_limit(&self) -> usize {
        self.admission.policy().limits().metadata_entries.get()
    }
    fn reclaim_buffers(&self) {
        self.admission.reclaim_buffers();
    }
    fn validate_reservation(
        &self,
        reservation: &flow_control::Charge<AdmissionPolicy>,
        class: ResourceClass,
        capacity: usize,
        cache: Option<&CacheId>,
    ) -> Result<()> {
        if !self.admission.owns(reservation) || cache.is_none() || reservation.key() != cache {
            return Err(Error::InvalidConfiguration);
        }
        reservation.validate(class, capacity).map_err(Into::into)
    }
    fn validate_page(&self, page: &page::PageResult) -> Result<()> {
        page.validate_metadata()?;
        let cache = Some(&page.plaintext.page().version.object.cache);
        self.validate_reservation(
            &page.plaintext.inner.reservation,
            ResourceClass::Plaintext,
            page.plaintext.inner.bytes.capacity(),
            cache,
        )?;
        self.validate_reservation(
            &page.ciphertext.inner.reservation,
            ResourceClass::Ciphertext,
            page.ciphertext.inner.bytes.capacity(),
            cache,
        )
    }
    fn validate_ciphertext(&self, copy: &page::CiphertextCopy) -> Result<()> {
        copy.validate_metadata()?;
        self.validate_reservation(
            &copy.ciphertext.inner.reservation,
            ResourceClass::Ciphertext,
            copy.ciphertext.inner.bytes.capacity(),
            Some(&copy.metadata.version.object.cache),
        )
    }
}
impl VerifiedPage {
    pub fn page(&self) -> &PageId {
        &self.inner.page
    }
    pub fn bytes(&self) -> &[u8] {
        &self.inner.bytes
    }
}
impl CiphertextPage {
    /// Rehome a completed receive before retaining it in another worker's cache.
    /// Admit the new charge first. Shared transport owners keep their original
    /// allocation and charge until fenced; only exclusive bytes can move in place.
    pub(crate) fn rehome(
        mut self,
        reservation: flow_control::Charge<AdmissionPolicy>,
    ) -> Result<Self> {
        reservation.validate(ResourceClass::Ciphertext, self.inner.bytes.capacity())?;
        if reservation.key() != Some(&self.envelope().page.version.object.cache) {
            return Err(Error::InvalidConfiguration);
        }
        if let Some(inner) = Arc::get_mut(&mut self.inner) {
            inner.reservation = reservation;
            return Ok(self);
        }
        let mut bytes = reservation.buffer(self.bytes().len())?;
        bytes.copy_from_slice(self.bytes());
        Ok(Self {
            inner: Arc::new(CiphertextBytes {
                checksum: self.inner.checksum.clone(),
                envelope: self.envelope().clone(),
                bytes,
                reservation,
            }),
            provenance: self.provenance,
        })
    }
    pub fn checksum(&self) -> u64 {
        *self
            .inner
            .checksum
            .get_or_init(|| racer_crypto::crc64(self.bytes()))
    }
    /// Never initializes a checksum or scans bytes on the I/O thread.
    pub(crate) fn cached_checksum(&self) -> Option<u64> {
        self.inner.checksum.get().copied()
    }
    pub(crate) fn verify_checksum(&self) -> Result<()> {
        let actual = racer_crypto::crc64(self.bytes());
        if self
            .inner
            .checksum
            .get()
            .is_some_and(|expected| *expected != actual)
        {
            return Err(Error::CorruptRecord);
        }
        let _ = self.inner.checksum.set(actual);
        Ok(())
    }
    pub(crate) fn expected_checksum(&self, checksum: u64) -> Result<()> {
        self.inner
            .checksum
            .set(checksum)
            .map_err(|_| Error::CorruptRecord)
    }
    pub fn envelope(&self) -> &PageEnvelope {
        &self.inner.envelope
    }
    pub fn bytes(&self) -> &[u8] {
        &self.inner.bytes
    }
}
// SAFETY: shared ciphertext backing is immutable and retained by the owner.
unsafe impl uring_runtime::reactor::SendBuffer for CiphertextPage {
    type Error = Error;
    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(self.bytes())
    }
}
#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::model::Nonce;
    use crate::model::PageNumber;
    use crate::model::VersionMetadata;

    pub(in crate::memory) fn admission(
        entries: usize,
    ) -> Rc<flow_control::Quotas<AdmissionPolicy>> {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.metadata_entries = std::num::NonZeroUsize::new(entries).unwrap();
        Rc::new(flow_control::Quotas::new(AdmissionPolicy::new(limits)))
    }
    pub(in crate::memory) fn bundle(
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        version: &str,
    ) -> super::page::PageResult {
        use crate::model::CacheKey;
        use crate::model::ObjectId;
        use crate::model::ObjectVersion;
        use crate::model::StrongEtag;
        bundle_for(
            admission,
            VersionMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(crate::security::test_support::CACHE.into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value(version),
                },
                length: 3,
            },
        )
    }
    pub(crate) fn bundle_for(
        admission: &Rc<flow_control::Quotas<AdmissionPolicy>>,
        metadata: VersionMetadata,
    ) -> super::page::PageResult {
        let page = PageId {
            version: metadata.version.clone(),
            number: PageNumber(0),
        };
        let cache = &page.version.object.cache;
        let plaintext = VerifiedPage {
            inner: Arc::new(VerifiedBytes {
                page: page.clone(),
                bytes: vec![1; 3],
                reservation: admission
                    .reserve(Some(cache), ResourceClass::Plaintext, 3)
                    .unwrap(),
            }),
        };
        let ciphertext = BufferPool::new(admission.clone())
            .ciphertext(
                admission
                    .reserve(Some(cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
                PageEnvelope {
                    page,
                    key_id: crate::model::key_id_from_generation(1, 1).unwrap(),
                    nonce: Nonce([2; 24]),
                    plaintext_length: 3,
                    ciphertext_length: 19,
                },
                vec![2; 19],
            )
            .unwrap();
        super::page::PageResult {
            metadata: metadata.for_pin(),
            plaintext,
            ciphertext,
        }
    }
    #[test]
    fn rehome_moves_exclusive_bytes_but_preserves_shared_transport_owners() {
        for shared in [false, true] {
            let source = admission(8);
            let target = admission(8);
            let mut page = bundle(&source, "rehome");
            let provenance = crate::telemetry::PeerProvenance {
                request: crate::model::RequestId([3; 16]),
                attempt: crate::model::AttemptId([4; 16]),
                supplier: [b'a'; 36],
                remote: [b'b'; 36],
            };
            page.ciphertext.provenance = Some(provenance);
            let checksum = page.ciphertext.checksum();
            let pointer = page.ciphertext.bytes().as_ptr();
            let retained = shared.then(|| page.ciphertext.clone());
            let reservation = target
                .reserve(
                    Some(&page.metadata.version.object.cache),
                    ResourceClass::Ciphertext,
                    19,
                )
                .unwrap();
            let moved = page.ciphertext.rehome(reservation).unwrap();
            assert_eq!(moved.bytes().as_ptr() == pointer, !shared);
            assert_eq!(moved.checksum(), checksum);
            assert_eq!(moved.provenance, Some(provenance));
            if let Some(retained) = &retained {
                assert_eq!(retained.provenance, Some(provenance));
            }
            assert!(target.owns(&moved.inner.reservation));
            assert_eq!(
                source.used(ResourceClass::Ciphertext),
                if shared { 19 } else { 0 }
            );
            assert_eq!(target.used(ResourceClass::Ciphertext), 19);
            drop(retained);
            assert_eq!(source.used(ResourceClass::Ciphertext), 0);
            drop(moved);
            assert_eq!(target.used(ResourceClass::Ciphertext), 0);
        }
    }

    #[test]
    fn rehome_rejects_wrong_cache_class_and_insufficient_charge() {
        let source = admission(8);
        let target = admission(8);
        let page = bundle(&source, "rehome");
        let cache = &page.metadata.version.object.cache;
        let wrong = CacheId("wrong".into());
        for (cache, class, amount) in [
            (&wrong, ResourceClass::Ciphertext, 19),
            (cache, ResourceClass::Plaintext, 19),
            (cache, ResourceClass::Ciphertext, 18),
        ] {
            assert!(matches!(
                page.ciphertext
                    .clone()
                    .rehome(target.reserve(Some(cache), class, amount).unwrap(),),
                Err(Error::InvalidConfiguration)
            ));
        }
        assert_eq!(source.used(ResourceClass::Ciphertext), 19);
        assert_eq!(target.used(ResourceClass::Ciphertext), 0);
        assert_eq!(target.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn persisted_crc_rejects_payload_corruption_independently_of_structural_validation() {
        let admission = admission(8);
        let mut page = bundle(&admission, "crc");
        let checksum = page.ciphertext.checksum();
        assert_eq!(page.ciphertext.verify_checksum(), Ok(()));
        Arc::get_mut(&mut page.ciphertext.inner).unwrap().bytes[0] ^= 1;
        assert!(page.validate_metadata().is_ok());
        assert_eq!(page.ciphertext.verify_checksum(), Err(Error::CorruptRecord));
        assert_eq!(*page.ciphertext.inner.checksum.get().unwrap(), checksum);
    }
    #[test]
    fn aead_fingerprints_reuse_crc_and_separate_body_from_identity() {
        use crate::model::RequestId;
        use crate::security::aead::capture_aead_failure;
        let admission = admission(8);
        let mut page = bundle(&admission, "sensitive-etag");
        let request = RequestId([9; 16]);
        let capture = |p: &CiphertextPage| {
            // Use a small stand-in AAD to isolate diagnostic fingerprinting.
            // Production passes the already validated canonical page_aad bytes.
            let mut aad = p.envelope().nonce.0.to_vec();
            aad.extend_from_slice(p.envelope().page.version.etag.as_bytes());
            capture_aead_failure(p, &aad, request)
        };
        assert_eq!(page.ciphertext.cached_checksum(), None);
        assert_eq!(capture(&page.ciphertext).crc, None);
        assert_eq!(capture(&page.ciphertext).peer, None);
        assert_eq!(
            page.ciphertext.cached_checksum(),
            None,
            "capture must not hash payload"
        );
        let provenance = crate::telemetry::PeerProvenance {
            request: RequestId([3; 16]),
            attempt: crate::model::AttemptId([4; 16]),
            supplier: [b'a'; 36],
            remote: [b'b'; 36],
        };
        page.ciphertext.provenance = Some(provenance);
        page.ciphertext.verify_checksum().unwrap();
        let first = capture(&page.ciphertext);
        let repeat = capture(&page.ciphertext);
        // Frozen SHA-256 vectors include the domain, u64 field lengths, exact
        // quoted ETag and page number. The AAD fingerprint hashes exact bytes.
        let hex = |bytes: &[u8]| {
            bytes
                .iter()
                .map(|byte| format!("{byte:02x}"))
                .collect::<String>()
        };
        assert_eq!(
            hex(&first.page),
            "c862fae4132569c58e99bb681cc7e9907ea20a693ea6760bf1ea5fcf7224243e"
        );
        assert_eq!(
            hex(&first.aad),
            "611dfd441fbad13fcb7e6e98a61a2704e30651ca1f2353acdc32906dd8ae3122"
        );
        assert_eq!(first.request, request);
        assert_eq!(first.peer, Some(provenance));
        assert_eq!(first.number, 0);
        assert_eq!(first.key, page.ciphertext.envelope().key_id.0);
        assert_eq!(first.nonce, [2; 24]);
        assert_eq!((first.plaintext, first.ciphertext), (3, 19));
        assert_eq!(
            first.crc,
            Some(racer_crypto::crc64(page.ciphertext.bytes()))
        );
        assert_eq!(first.page, repeat.page);
        assert_eq!(first.aad, repeat.aad);
        assert_eq!(first.crc, repeat.crc);
        let inner = Arc::get_mut(&mut page.ciphertext.inner).unwrap();
        inner.bytes[0] ^= 1;
        assert_eq!(page.ciphertext.verify_checksum(), Err(Error::CorruptRecord));
        assert_eq!(capture(&page.ciphertext).crc, first.crc);
        assert_eq!(page.ciphertext.cached_checksum(), first.crc);
        Arc::get_mut(&mut page.ciphertext.inner).unwrap().checksum = std::sync::OnceLock::new();
        page.ciphertext.verify_checksum().unwrap();
        let changed = capture(&page.ciphertext);
        assert_ne!(first.crc, changed.crc);
        assert_eq!(first.page, changed.page);
        assert_eq!(first.aad, changed.aad);
        Arc::get_mut(&mut page.ciphertext.inner)
            .unwrap()
            .envelope
            .nonce
            .0[0] ^= 1;
        let changed_aad = capture(&page.ciphertext);
        assert_eq!(changed.crc, changed_aad.crc);
        assert_eq!(changed.page, changed_aad.page);
        assert_ne!(changed.aad, changed_aad.aad);
        let failures = crate::telemetry::Failures::default();
        failures.observer(crate::model::WorkerId(0)).record_aead(
            crate::runtime::crypto::CryptoId {
                worker: crate::model::WorkerId(0),
                generation: 0,
                sequence: 1,
            },
            first,
        );
        let mut text = String::new();
        failures.write_aead(&mut text).unwrap();
        assert!(!text.contains("sensitive-etag"));
        assert!(!text.contains(&page.ciphertext.envelope().page.version.object.key.to_hex()));
        drop(page);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        let mut retained = String::new();
        failures.write_aead(&mut retained).unwrap();
        assert_eq!(retained, text, "diagnostic facts do not retain page owners");
    }
    #[test]
    fn plaintext_is_stable_zeroed_and_reservation_backed() {
        let admission = admission(8);
        let pool = BufferPool::new(admission.clone());
        let cache = CacheId("cache".into());
        let reservation = admission
            .reserve(Some(&cache), ResourceClass::Plaintext, 8)
            .unwrap();
        let mut buffer = pool.plaintext(reservation, 3).unwrap();
        assert_eq!(buffer.bytes().unwrap(), &[0; 3]);
        let pointer = buffer.bytes().unwrap().as_ptr();
        buffer.bytes_mut().unwrap().copy_from_slice(b"abc");
        let (bytes, reservation) = buffer.into_parts();
        assert_eq!(bytes.as_ptr(), pointer);
        assert_eq!(&*bytes, b"abc");
        assert_eq!(reservation.amount(), 3);
        assert_eq!(admission.used(ResourceClass::Plaintext), 3);
        drop((bytes, reservation));
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }
    #[test]
    fn allocations_reject_wrong_provenance_class_cache_and_bounds() {
        let admission = admission(8);
        let pool = BufferPool::new(admission.clone());
        let other = self::admission(8);
        let cache = CacheId("cache".into());
        for (owner, class, cache, amount, length) in [
            (&other, ResourceClass::Plaintext, Some(&cache), 3, 3),
            (&admission, ResourceClass::Ciphertext, Some(&cache), 3, 3),
            (&admission, ResourceClass::Plaintext, None, 3, 3),
            (&admission, ResourceClass::Plaintext, Some(&cache), 2, 3),
            (&admission, ResourceClass::Plaintext, Some(&cache), 3, 0),
            (
                &admission,
                ResourceClass::Plaintext,
                Some(&cache),
                3,
                PAGE_BYTES as usize + 1,
            ),
        ] {
            assert!(matches!(
                pool.plaintext(owner.reserve(cache, class, amount).unwrap(), length),
                Err(Error::InvalidConfiguration)
            ));
        }
        let page = bundle(&admission, "v1");
        let envelope = page.ciphertext.envelope().clone();
        let wrong = CacheId("other".into());
        for (owner, class, cache, amount) in [
            (&other, ResourceClass::Ciphertext, &cache, 19),
            (&admission, ResourceClass::Plaintext, &cache, 19),
            (&admission, ResourceClass::Ciphertext, &wrong, 19),
            (&admission, ResourceClass::Ciphertext, &cache, 18),
        ] {
            assert!(matches!(
                pool.ciphertext(
                    owner.reserve(Some(cache), class, amount).unwrap(),
                    envelope.clone(),
                    vec![0; 19]
                ),
                Err(Error::InvalidConfiguration)
            ));
        }
        for length in [0, 18, 20] {
            assert!(matches!(
                pool.ciphertext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Ciphertext, 20)
                        .unwrap(),
                    envelope.clone(),
                    vec![0; length]
                ),
                Err(Error::CorruptRecord)
            ));
        }
        let mut excess_capacity = Vec::with_capacity(100);
        excess_capacity.resize(19, 0);
        assert!(matches!(
            pool.ciphertext(
                admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
                envelope.clone(),
                excess_capacity
            ),
            Err(Error::InvalidConfiguration)
        ));
        let mut invalid = envelope;
        invalid.plaintext_length = 0;
        assert!(matches!(
            pool.ciphertext(
                admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
                invalid,
                vec![0; 19]
            ),
            Err(Error::CorruptRecord)
        ));
    }
    #[test]
    fn immutable_clones_charge_once_until_last_cross_thread_owner() {
        let admission = admission(8);
        let page = bundle(&admission, "v1");
        let plain = page.plaintext.clone();
        let cipher = page.ciphertext.clone();
        let pointer = cipher.bytes().as_ptr();
        assert_eq!(page.ciphertext.bytes().as_ptr(), pointer);
        drop(page);
        assert_eq!(admission.used(ResourceClass::Plaintext), 3);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
        std::thread::spawn(move || drop((plain, cipher)))
            .join()
            .unwrap();
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
}
#[cfg(test)]
use uring_runtime::reactor::IoBuffer;

/// Credential-free page results shared by fills, memory, and flight completion.
pub mod page {
    use super::CiphertextPage;
    use super::VerifiedPage;
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
            self.metadata.validate()?;
            validate_ciphertext_length(&self.ciphertext)?;
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
        validate_ciphertext_length(ciphertext)
    }
    fn validate_ciphertext_length(ciphertext: &CiphertextPage) -> crate::error::Result<()> {
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
            let admission = crate::memory::tests::admission(8);
            for length in [0, 18, 19, 20] {
                let mut page = crate::memory::tests::bundle(&admission, "v1");
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
            let admission = crate::memory::tests::admission(8);
            let bundle = crate::memory::tests::bundle(&admission, "v1");
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

pub fn new_pipe_pool(admission: Rc<Quotas<AdmissionPolicy>>) -> PipePool<AdmissionPolicy> {
    let waiter_limit = admission.policy().limits().queue_entries.get();
    PipePool::new(
        admission,
        ResourceClass::Pipe,
        ResourceClass::RequestContext,
        waiter_limit,
    )
}

pub(crate) fn acquire_wait<'a>(
    pool: &'a PipePool<AdmissionPolicy>,
    scope: &'a RequestScope,
) -> Operation<'a, PipeLease<AdmissionPolicy>> {
    Box::pin(pool.acquire_wait(
        || scope.check(),
        || {
            let cancellation = scope.cancellation.subscribe()?;
            Ok(move |waker: &Waker| cancellation.register(waker))
        },
    ))
}

#[cfg(test)]
mod pipe_tests {
    use super::*;
    use crate::error::Error;
    use crate::model::Limits;

    pub(in crate::memory) fn admission(pipes: usize) -> Rc<Quotas<AdmissionPolicy>> {
        let small = std::num::NonZeroUsize::new(8).unwrap();
        let bytes = std::num::NonZeroUsize::new(32 * 1024 * 1024).unwrap();
        Rc::new(Quotas::new(AdmissionPolicy::new(Limits {
            plaintext_bytes: bytes,
            ciphertext_bytes: bytes,
            dirty_bytes: bytes,
            registered_bytes: bytes,
            request_context_bytes: bytes,
            flights: small,
            waiters_per_flight: small,
            queue_entries: small,
            connections_per_neighbor: small,
            client_connections: small,
            pipes: std::num::NonZeroUsize::new(pipes).unwrap(),
            range_window_pages: small,
            header_bytes: small,
            cached_rankings: small,
            cached_paths: small,
            retained_snapshots: small,
            metadata_entries: small,
            relay_transfers: small,
        })))
    }

    #[test]
    fn immediate_acquisition_does_not_subscribe_to_cancellation() {
        use crate::model::RequestId;
        use std::task::Context;
        use std::task::Poll;
        use std::time::Duration;
        use std::time::Instant;
        let admission = admission(1);
        let pool = new_pipe_pool(admission.clone());
        let scope =
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let mut registrations = Vec::new();
        loop {
            match scope.cancellation.subscribe() {
                Ok(registration) => registrations.push(registration),
                Err(Error::Overloaded) => break,
                Err(error) => panic!("unexpected registration failure: {error:?}"),
            }
            assert!(registrations.len() <= 1024);
        }
        let mut cx = Context::from_waker(Waker::noop());
        let Poll::Ready(Ok(held)) = acquire_wait(&pool, &scope).as_mut().poll(&mut cx) else {
            panic!("immediate acquisition unnecessarily subscribed")
        };
        assert!(matches!(
            acquire_wait(&pool, &scope).as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Overloaded))
        ));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
        assert_eq!(admission.used(ResourceClass::Pipe), 1);
        drop(registrations);
        let mut wait = acquire_wait(&pool, &scope);
        assert!(wait.as_mut().poll(&mut cx).is_pending());
        drop(held);
        assert!(matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Ok(_))));
        assert_eq!(admission.used(ResourceClass::RequestContext), 0);
    }

    #[test]
    fn scheduled_wait_cancellation_deadline_stop_and_abandonment_release_admission() {
        use crate::model::RequestId;
        use crate::test_support::WakeCounter;
        use std::sync::Arc;
        use std::task::Context;
        use std::task::Poll;
        use std::time::Duration;
        use std::time::Instant;
        for failure in [
            Some(Error::Cancelled),
            Some(Error::DeadlineExceeded),
            Some(Error::Unavailable),
            None,
        ] {
            let admission = admission(1);
            let pool = new_pipe_pool(admission.clone());
            let held = pool.acquire().unwrap();
            let scope = RequestScope::new(
                RequestId([0; 16]),
                Instant::now()
                    + if failure == Some(Error::DeadlineExceeded) {
                        Duration::from_millis(10)
                    } else {
                        Duration::from_secs(5)
                    },
            )
            .unwrap();
            let count = Arc::new(WakeCounter::default());
            let waker = Waker::from(count.clone());
            let mut cx = Context::from_waker(&waker);
            let mut wait = acquire_wait(&pool, &scope);
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert_eq!(
                admission.used(ResourceClass::RequestContext),
                pool.waiter_bytes()
            );
            match failure {
                Some(Error::Cancelled) => {
                    scope.cancel().unwrap();
                    assert!(count.count() > 0);
                }
                Some(Error::DeadlineExceeded) => std::thread::sleep(Duration::from_millis(20)),
                Some(Error::Unavailable) => admission.stop(),
                _ => {}
            }
            if let Some(expected) = failure {
                assert!(
                    matches!(wait.as_mut().poll(&mut cx), Poll::Ready(Err(error)) if error == expected)
                );
            }
            drop(wait);
            assert_eq!(admission.used(ResourceClass::RequestContext), 0);
            assert_eq!(admission.used(ResourceClass::Pipe), 1);
            drop(held);
            if failure != Some(Error::Unavailable) {
                // No stale FIFO entry may prevent the next caller's progress.
                let fresh =
                    RequestScope::new(RequestId([1; 16]), Instant::now() + Duration::from_secs(5))
                        .unwrap();
                assert!(matches!(
                    acquire_wait(&pool, &fresh).as_mut().poll(&mut cx),
                    Poll::Ready(Ok(_))
                ));
            }
            drop(pool);
            assert_eq!(admission.used(ResourceClass::Pipe), 0);
        }
    }
}
