//! Bounded immutable page leases. No buffers contain request credentials.
use crate::{
    error::{Error, Result},
    model::{
        envelope::PageEnvelope,
        identity::{CacheId, PageId},
        limits::ResourceClass,
        range::PAGE_BYTES,
    },
    runtime::admission::{Admission, Reservation},
};
use std::{rc::Rc, sync::Arc};

pub struct BufferPool {
    admission: Rc<Admission>,
}
/// Mutable staging buffer, not proof of authentication and not client-deliverable.
/// Fixed-size owned backing stays at the same address when this owner moves.
pub struct PlaintextBuffer {
    bytes: Box<[u8]>,
    reservation: Option<Reservation>,
}
impl PlaintextBuffer {
    pub(crate) fn into_parts(mut self) -> (Box<[u8]>, Reservation) {
        (
            std::mem::take(&mut self.bytes),
            self.reservation.take().expect("owned reservation"),
        )
    }
    pub(crate) fn reservation(&self) -> &Reservation {
        self.reservation.as_ref().expect("owned reservation")
    }
}
impl Drop for PlaintextBuffer {
    fn drop(&mut self) {
        if let Some(reservation) = &mut self.reservation {
            reservation.recycle(std::mem::take(&mut self.bytes).into_vec());
        }
    }
}
impl crate::runtime::reactor::sealed::Sealed for PlaintextBuffer {}
impl crate::runtime::reactor::IoBuffer for PlaintextBuffer {
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
    pub reservation: Reservation,
}
impl Drop for VerifiedBytes {
    fn drop(&mut self) {
        self.reservation.recycle(std::mem::take(&mut self.bytes));
    }
}
#[derive(Clone)]
pub struct CiphertextPage {
    pub(crate) inner: Arc<CiphertextBytes>,
}
pub(crate) struct CiphertextBytes {
    pub checksum: std::sync::OnceLock<u64>,
    pub envelope: PageEnvelope,
    pub bytes: Vec<u8>,
    pub reservation: Reservation,
}
impl Drop for CiphertextBytes {
    fn drop(&mut self) {
        self.reservation.recycle(std::mem::take(&mut self.bytes));
    }
}
impl BufferPool {
    pub fn new(admission: Rc<Admission>) -> Self {
        Self { admission }
    }
    pub fn plaintext(
        &self,
        mut reservation: Reservation,
        length: usize,
    ) -> Result<PlaintextBuffer> {
        if length == 0 || length > PAGE_BYTES as usize {
            return Err(Error::InvalidConfiguration);
        }
        self.validate_reservation(
            &reservation,
            ResourceClass::Plaintext,
            length,
            reservation.cache(),
        )?;
        let bytes = reservation.buffer(length)?;
        let bytes = bytes.into_boxed_slice();
        reservation.shrink(bytes.len())?;
        Ok(PlaintextBuffer {
            bytes,
            reservation: Some(reservation),
        })
    }
    pub fn ciphertext(
        &self,
        mut reservation: Reservation,
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
        reservation: &Reservation,
        class: ResourceClass,
        capacity: usize,
        cache: Option<&CacheId>,
    ) -> Result<()> {
        if !self.admission.owns(reservation) || cache.is_none() || reservation.cache() != cache {
            return Err(Error::InvalidConfiguration);
        }
        reservation.validate(class, capacity)
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
    pub fn checksum(&self) -> u64 {
        *self
            .inner
            .checksum
            .get_or_init(|| crate::security::crc64::checksum(self.bytes()))
    }
    pub(crate) fn verify_checksum(&self) -> Result<()> {
        let actual = crate::security::crc64::checksum(self.bytes());
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
impl crate::runtime::reactor::sealed::Sealed for CiphertextPage {}
impl crate::runtime::reactor::SendBuffer for CiphertextPage {
    fn send_bytes(&self) -> Result<&[u8]> {
        Ok(self.bytes())
    }
}
#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::{
        model::{
            envelope::{KeyId, Nonce},
            identity::PageNumber,
            metadata::VersionMetadata,
        },
        runtime::reactor::IoBuffer,
    };

    pub(in crate::memory) fn admission(entries: usize) -> Rc<Admission> {
        let mut limits = crate::test_support::cluster::config(false).limits;
        limits.metadata_entries = std::num::NonZeroUsize::new(entries).unwrap();
        Rc::new(Admission::new(limits))
    }
    pub(in crate::memory) fn bundle(
        admission: &Rc<Admission>,
        version: &str,
    ) -> super::super::page::PageResult {
        use crate::model::identity::{CacheKey, ObjectId, ObjectVersion, StrongEtag};
        bundle_for(
            admission,
            VersionMetadata {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: CacheId("cache".into()),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value(version),
                },
                length: 3,
            },
        )
    }
    pub(crate) fn bundle_for(
        admission: &Rc<Admission>,
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
                    key_id: KeyId([1; 16]),
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
