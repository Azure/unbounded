//! Worker-local idle verified pages and original ciphertext; independent of disk clock.
use super::{
    page::{CiphertextCopy, PageResult},
    pool::BufferPool,
};
use crate::{
    error::{Error, Result},
    model::{
        envelope::KeyId,
        identity::{CacheId, ObjectVersion, PageId},
        limits::ResourceClass,
        metadata::VersionMetadata,
    },
};
use std::{cell::RefCell, collections::VecDeque, rc::Rc, sync::Arc};
pub struct MemoryCache {
    pool: Rc<BufferPool>,
    entries: RefCell<VecDeque<PageResult>>,
    availability: Option<Rc<crate::control::availability::Availability>>,
}
impl MemoryCache {
    pub fn new(pool: Rc<BufferPool>) -> Self {
        Self {
            pool,
            entries: RefCell::new(VecDeque::new()),
            availability: None,
        }
    }
    pub fn with_availability(
        mut self,
        availability: Rc<crate::control::availability::Availability>,
    ) -> Self {
        self.availability = Some(availability);
        self
    }
    fn available(&self, page: &PageResult) -> bool {
        self.availability.as_ref().is_none_or(|a| {
            a.page(
                &page.metadata.version.object.cache,
                page.ciphertext.envelope().key_id,
            )
        })
    }
    pub fn get(&self, page: &PageId) -> Result<Option<PageResult>> {
        let mut entries = self.entries.borrow_mut();
        let Some(index) = entries
            .iter()
            .position(|entry| entry.plaintext.page() == page)
        else {
            return Ok(None);
        };
        let entry = entries.remove(index).expect("located entry");
        if !self.available(&entry) {
            return Ok(None);
        }
        let result = entry.clone();
        entries.push_back(entry);
        Ok(Some(result))
    }
    pub fn ciphertext(&self, page: &PageId) -> Result<Option<CiphertextCopy>> {
        Ok(self.get(page)?.map(|entry| entry.copy()))
    }
    /// Validate matching identities and full-page bounds before retaining the bundle.
    pub fn publish(&self, page: PageResult) -> Result<()> {
        self.pool.validate_page(&page)?;
        let id = page.plaintext.page();
        if self
            .availability
            .as_ref()
            .is_some_and(|a| !a.cache(&id.version.object.cache))
        {
            return Err(Error::Unavailable);
        }
        if !self.available(&page) {
            return Err(Error::MissingKey);
        }
        let mut entries = self.entries.borrow_mut();
        for entry in entries.iter() {
            if entry.metadata.version == page.metadata.version
                && entry.metadata.length != page.metadata.length
            {
                return Err(Error::CorruptRecord);
            }
            if entry.plaintext.page() == id {
                if entry.plaintext.bytes() != page.plaintext.bytes() {
                    return Err(Error::CorruptRecord);
                }
                // A duplicate fill must not replace the original nonce/ciphertext
                // or turn its historical deadline into renewed freshness.
                return Ok(());
            }
        }
        if entries.len() >= self.pool.entry_limit() {
            let index = entries.iter().position(idle).ok_or(Error::Overloaded)?;
            entries.remove(index);
        }
        entries.try_reserve(1).map_err(|_| Error::Overloaded)?;
        entries.push_back(page);
        Ok(())
    }
    pub fn metadata(&self, version: &ObjectVersion) -> Result<Option<VersionMetadata>> {
        Ok(self
            .entries
            .borrow()
            .iter()
            .find(|entry| &entry.metadata.version == version && self.available(entry))
            .map(|entry| entry.metadata.immutable()))
    }
    /// Return released admission bytes, including any reserved final-page slack.
    /// Busy plaintext OR ciphertext protects the complete retained bundle.
    pub fn evict_idle(&self, bytes: usize) -> Result<usize> {
        let mut released = 0usize;
        self.entries.borrow_mut().retain(|entry| {
            if released >= bytes || !idle(entry) {
                return true;
            }
            released = released
                .saturating_add(entry.plaintext.inner.reservation.amount())
                .saturating_add(entry.ciphertext.inner.reservation.amount());
            false
        });
        Ok(released)
    }
    /// One bounded LRU pass, counting only the exhausted class. The callback may
    /// release a queued writer's sole extra ciphertext reference, never a reader.
    pub(crate) fn reclaim_idle(
        &self,
        class: ResourceClass,
        cache: Option<&CacheId>,
        bytes: usize,
        mut release_queued: impl FnMut(&PageResult) -> usize,
    ) -> usize {
        if !matches!(class, ResourceClass::Plaintext | ResourceClass::Ciphertext) {
            return 0;
        }
        let mut released = 0usize;
        self.entries.borrow_mut().retain(|entry| {
            if released >= bytes
                || cache.is_some_and(|cache| cache != &entry.metadata.version.object.cache)
                || Arc::strong_count(&entry.plaintext.inner) != 1
            {
                return true;
            }
            let staging = release_queued(entry);
            if matches!(class, ResourceClass::Ciphertext) {
                released = released.saturating_add(staging);
            }
            if released >= bytes {
                return true;
            }
            if !idle(entry) {
                return true;
            }
            released = released.saturating_add(match class {
                ResourceClass::Plaintext => entry.plaintext.inner.reservation.amount(),
                _ => entry.ciphertext.inner.reservation.amount(),
            });
            false
        });
        released
    }
    /// Evict lookup references. Existing owners keep
    /// their charges until their completion fences release them.
    pub fn retire_key(&self, cache: &CacheId, key: KeyId) -> Result<usize> {
        let mut entries = self.entries.borrow_mut();
        let before = entries.len();
        entries.retain(|entry| {
            &entry.metadata.version.object.cache != cache
                || entry.ciphertext.envelope().key_id != key
        });
        Ok(before - entries.len())
    }
    pub fn remove_cache(&self, cache: &CacheId) -> Result<()> {
        self.entries
            .borrow_mut()
            .retain(|entry| &entry.metadata.version.object.cache != cache);
        Ok(())
    }
}
fn idle(entry: &PageResult) -> bool {
    Arc::strong_count(&entry.plaintext.inner) == 1
        && Arc::strong_count(&entry.ciphertext.inner) == 1
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        memory::pool::tests::{admission, bundle, bundle_for},
        model::{limits::ResourceClass, metadata::ExpiresAt},
    };

    #[test]
    fn busy_leases_protect_both_allocations_and_eviction_releases_idle_bytes() {
        let admission = admission(8);
        let cache = MemoryCache::new(Rc::new(BufferPool::new(admission.clone())));
        let page = bundle(&admission, "v1");
        let id = page.plaintext.page().clone();
        cache.publish(page).unwrap();
        let copy = cache.ciphertext(&id).unwrap().unwrap();
        assert_eq!(cache.evict_idle(usize::MAX), Ok(0));
        drop(copy);
        let plaintext = cache.get(&id).unwrap().unwrap().plaintext;
        assert_eq!(cache.evict_idle(usize::MAX), Ok(0));
        drop(plaintext);
        assert_eq!(cache.evict_idle(0), Ok(0));
        assert_eq!(cache.evict_idle(1), Ok(22));
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
        assert!(cache.get(&id).unwrap().is_none());
        assert!(cache.metadata(&id.version).unwrap().is_none());
    }
    #[test]
    fn duplicate_preserves_original_ciphertext_metadata_and_expired_deadline() {
        let admission = admission(8);
        let cache = MemoryCache::new(Rc::new(BufferPool::new(admission.clone())));
        let page = bundle(&admission, "v1");
        let id = page.plaintext.page().clone();
        let pointer = page.ciphertext.bytes().as_ptr();
        let original = page.ciphertext.envelope().clone();
        cache.publish(page).unwrap();
        let mut duplicate = bundle(&admission, "v1");
        duplicate.metadata.expires_at = ExpiresAt::from_unix_millis(123456).unwrap();
        Arc::get_mut(&mut duplicate.ciphertext.inner)
            .unwrap()
            .envelope
            .nonce
            .0[0] ^= 1;
        cache.publish(duplicate).unwrap();
        let copy = cache.ciphertext(&id).unwrap().unwrap();
        assert_eq!(copy.ciphertext.bytes().as_ptr(), pointer);
        assert_eq!(copy.ciphertext.envelope(), &original);
        assert_eq!(copy.ciphertext.bytes(), &[2; 19]);
        assert_eq!(copy.metadata.expires_at, ExpiresAt(std::time::UNIX_EPOCH));
        assert_eq!(
            cache.metadata(&id.version).unwrap(),
            Some(copy.metadata.immutable())
        );
        assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
    }
    #[test]
    fn capacity_evicts_least_recent_idle_entry_and_rejects_all_busy() {
        let admission = admission(2);
        let cache = MemoryCache::new(Rc::new(BufferPool::new(admission.clone())));
        let first = bundle(&admission, "v1");
        let first_id = first.plaintext.page().clone();
        let second = bundle(&admission, "v2");
        let second_id = second.plaintext.page().clone();
        cache.publish(first).unwrap();
        cache.publish(second).unwrap();
        let busy = cache.get(&first_id).unwrap().unwrap();
        let also_busy = cache.get(&second_id).unwrap().unwrap();
        assert_eq!(
            cache.publish(bundle(&admission, "v3")),
            Err(Error::Overloaded)
        );
        drop(also_busy);
        cache.publish(bundle(&admission, "v3")).unwrap();
        assert!(cache.get(&second_id).unwrap().is_none());
        assert!(cache.get(&first_id).unwrap().is_some());
        drop(busy);
        cache.publish(bundle(&admission, "v4")).unwrap();
        assert!(cache.get(&first_id).unwrap().is_some());
        assert_eq!(cache.entries.borrow().len(), 2);
    }
    #[test]
    fn publication_rejects_foreign_undercharged_and_conflicting_bundles() {
        let admission = admission(8);
        let cache = MemoryCache::new(Rc::new(BufferPool::new(admission.clone())));
        assert_eq!(
            cache.publish(bundle(&self::admission(8), "v1")),
            Err(Error::InvalidConfiguration)
        );
        let mut malformed = bundle(&admission, "v1");
        Arc::get_mut(&mut malformed.ciphertext.inner)
            .unwrap()
            .bytes
            .pop();
        assert_eq!(cache.publish(malformed), Err(Error::CorruptRecord));
        let mut undercharged = bundle(&admission, "v1");
        let wrong = admission
            .reserve(
                Some(&undercharged.metadata.version.object.cache),
                ResourceClass::Ciphertext,
                3,
            )
            .unwrap();
        Arc::get_mut(&mut undercharged.plaintext.inner)
            .unwrap()
            .reservation = wrong;
        assert_eq!(
            cache.publish(undercharged),
            Err(Error::InvalidConfiguration)
        );
        cache.publish(bundle(&admission, "v1")).unwrap();
        let mut conflicting = bundle(&admission, "v1");
        Arc::get_mut(&mut conflicting.plaintext.inner)
            .unwrap()
            .bytes[0] ^= 1;
        assert_eq!(cache.publish(conflicting), Err(Error::CorruptRecord));
        let mut inconsistent = bundle(&admission, "v2");
        inconsistent.metadata.version.etag = crate::model::identity::StrongEtag::test_value("v1");
        assert_eq!(cache.publish(inconsistent), Err(Error::CorruptRecord));
        assert_eq!(cache.entries.borrow().len(), 1);
    }
    #[test]
    fn eviction_is_cache_scoped_and_preserves_live_leases() {
        let admission = admission(8);
        let cache = MemoryCache::new(Rc::new(BufferPool::new(admission.clone())));
        let page = bundle(&admission, "v1");
        let id = page.plaintext.page().clone();
        let mut other_descriptor = page.metadata.immutable();
        other_descriptor.version.object.cache = CacheId("other".into());
        let other = bundle_for(&admission, other_descriptor.clone());
        let other_id = other.plaintext.page().clone();
        cache.publish(page.clone()).unwrap();
        cache.publish(other).unwrap();
        assert_eq!(
            cache.retire_key(&id.version.object.cache, KeyId([1; 16])),
            Ok(1)
        );
        assert!(cache.get(&id).unwrap().is_none());
        assert!(cache.get(&other_id).unwrap().is_some());
        assert_eq!(page.plaintext.bytes(), &[1; 3]);
        assert_eq!(admission.used(ResourceClass::Plaintext), 6);
        drop(page);
        assert_eq!(admission.used(ResourceClass::Plaintext), 3);
        let lease = cache.ciphertext(&other_id).unwrap().unwrap();
        cache.remove_cache(&other_id.version.object.cache).unwrap();
        assert!(cache.get(&other_id).unwrap().is_none());
        assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
        drop(lease);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }
    #[test]
    fn eviction_is_idempotent_and_churn_needs_no_tombstones() {
        let admission = admission(1);
        let cache = MemoryCache::new(Rc::new(BufferPool::new(admission)));
        let id = CacheId("cache".into());
        assert_eq!(cache.retire_key(&id, KeyId([1; 16])), Ok(0));
        assert_eq!(cache.retire_key(&id, KeyId([1; 16])), Ok(0));
        assert_eq!(cache.retire_key(&id, KeyId([2; 16])), Ok(0));
        assert_eq!(cache.remove_cache(&id), Ok(()));
        assert_eq!(cache.remove_cache(&id), Ok(()));
        assert_eq!(cache.remove_cache(&CacheId("other".into())), Ok(()));
        for n in 0..10_000 {
            cache.remove_cache(&CacheId(n.to_string())).unwrap();
        }
    }
    #[test]
    fn empty_stable_catalog_rotates_without_consuming_page_metadata_capacity() {
        use crate::{
            control::{availability::for_caches, wire::CacheKeyPurpose},
            security::keyring::tests::rotation_bundle,
        };
        let admission = admission(1024);
        let keys = Rc::new(crate::security::keyring::tests::keys());
        let roots = (*keys.peer_trust_roots().unwrap()).clone();
        let caches: Vec<_> = (0..356)
            .map(|cache| CacheId(format!("{cache:08x}-0000-4000-8000-000000000000")))
            .collect();
        let memory = MemoryCache::new(Rc::new(BufferPool::new(admission)))
            .with_availability(for_caches(keys.clone(), caches.clone()));
        let mut previous: Vec<crate::control::wire::CacheKeyRef> = Vec::new();
        for generation in 2u64..=6 {
            let mut next = rotation_bundle(generation, roots.clone());
            let templates = std::mem::take(&mut next.cache_keys);
            for cache in 0u64..356 {
                for template in &templates {
                    let mut key = template.clone();
                    key.key.cache = CacheId(format!("{cache:08x}-0000-4000-8000-000000000000"));
                    key.material[8..16].copy_from_slice(&cache.to_be_bytes());
                    next.cache_keys.push(key);
                }
            }
            keys.install(next.clone()).unwrap();
            for key in previous {
                if key.purpose == CacheKeyPurpose::Page {
                    assert_eq!(memory.retire_key(&key.cache, key.id), Ok(0));
                    assert!(
                        !memory
                            .availability
                            .as_ref()
                            .unwrap()
                            .page(&key.cache, key.id)
                    );
                }
            }
            previous = next.cache_keys.iter().map(|key| key.key.clone()).collect();
        }
        assert!(memory.entries.borrow().is_empty());
    }
}
