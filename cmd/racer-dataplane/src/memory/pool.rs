//! Bounded immutable page leases. No buffers contain request credentials.
use crate::{
    error::{Error, Result},
    model::{CacheId, PAGE_BYTES, PageEnvelope, PageId, ResourceClass},
    runtime::admission::{AdmissionExt, AdmissionPolicy},
};
use std::{rc::Rc, sync::Arc};

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
unsafe impl crate::runtime::reactor::IoBuffer for PlaintextBuffer {
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
    pub(crate) provenance: Option<crate::telemetry::failures::PeerProvenance>,
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
    pub(super) fn entry_limit(&self) -> usize {
        self.admission.limits().metadata_entries.get()
    }
    pub(super) fn reclaim_buffers(&self) {
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
    pub(super) fn validate_page(&self, page: &super::page::PageResult) -> Result<()> {
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
    pub(super) fn validate_ciphertext(&self, copy: &super::page::CiphertextCopy) -> Result<()> {
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
unsafe impl crate::runtime::reactor::SendBuffer for CiphertextPage {
    type Error = Error;
    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(self.bytes())
    }
}
#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::{
        model::{Nonce, PageNumber, VersionMetadata},
        runtime::reactor::IoBuffer,
    };

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
    ) -> super::super::page::PageResult {
        use crate::model::{CacheKey, ObjectId, ObjectVersion, StrongEtag};
        bundle_for(
            admission,
            VersionMetadata {
                content_type: None,
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId(crate::security::identity::tests::CACHE.into()),
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
    ) -> super::super::page::PageResult {
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
        super::super::page::PageResult {
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
            let provenance = crate::telemetry::failures::PeerProvenance {
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
        use crate::{model::RequestId, security::aead::capture_aead_failure};
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
        let provenance = crate::telemetry::failures::PeerProvenance {
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
        let failures = crate::telemetry::failures::Failures::default();
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
