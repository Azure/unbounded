use super::*;
use crate::model::ExpiresAt;

#[test]
fn ciphertext_residency_is_distinct_bounded_and_conditionally_invalidated() {
    let admission = admission(2);
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(),
    );
    let first = bundle(&admission, "cipher");
    let id = first.plaintext.page().clone();
    let unverified = UnverifiedPage {
        copy: first.copy(),
        disk_token: None,
    };
    cache.publish_ciphertext(unverified.clone()).unwrap();
    assert!(cache.get(&id).unwrap().is_none());
    assert_eq!(
        cache.ciphertext(&id).unwrap().unwrap().ciphertext.bytes(),
        first.ciphertext.bytes()
    );
    let replacement = bundle(&admission, "cipher");
    cache.invalidate_ciphertext(&UnverifiedPage {
        copy: replacement.copy(),
        disk_token: None,
    });
    assert!(
        cache.unverified(&id).unwrap().is_some(),
        "different allocation must not invalidate"
    );
    cache.invalidate_ciphertext(&unverified);
    assert!(cache.unverified(&id).unwrap().is_none());
    cache.publish_ciphertext(unverified).unwrap();
    cache.publish(first).unwrap();
    assert!(cache.unverified(&id).unwrap().is_none());
    assert!(cache.get(&id).unwrap().is_some());
}
#[test]
fn bounded_reclamation_advances_past_a_busy_prefix() {
    let admission = admission(300);
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(),
    );
    let mut busy = Vec::new();
    let mut ids = Vec::new();
    for n in 0..300 {
        let page = bundle(&admission, &format!("v{n}"));
        ids.push(page.plaintext.page().clone());
        if n < 256 {
            busy.push(page.clone());
        }
        cache.publish(page).unwrap();
    }
    assert_eq!(cache.evict_idle(1), Ok(0));
    assert_eq!(cache.evict_idle(1), Ok(22));
    assert!(cache.get(&ids[256]).unwrap().is_none());
    assert!(cache.get(&ids[257]).unwrap().is_some());
    for reader in &busy {
        assert_eq!(reader.plaintext.bytes(), &[1; 3]);
        assert!(cache.get(reader.plaintext.page()).unwrap().is_some());
    }
    assert_eq!(admission.used(ResourceClass::Plaintext), 299 * 3);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 299 * 19);
    drop(busy);
    for _ in 0..3 {
        cache.evict_idle(usize::MAX).unwrap();
    }
    for id in ids {
        assert!(cache.get(&id).unwrap().is_none());
    }
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
}
#[test]
fn busy_leases_protect_both_allocations_and_eviction_releases_idle_bytes() {
    let admission = admission(8);
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(),
    );
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
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(),
    );
    let page = bundle(&admission, "v1");
    let id = page.plaintext.page().clone();
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
    assert_eq!(copy.ciphertext.envelope(), &original);
    assert_eq!(copy.ciphertext.bytes(), &[2; 19]);
    assert_eq!(
        copy.metadata.expires_at,
        ExpiresAt::from_system_time(std::time::UNIX_EPOCH).unwrap()
    );
    assert_eq!(
        cache.metadata(&id.version).unwrap(),
        Some(copy.metadata.immutable())
    );
    assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
}
#[test]
fn capacity_evicts_least_recent_idle_entry_and_rejects_all_busy() {
    let admission = admission(2);
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(),
    );
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
    let third = bundle(&admission, "v3");
    let third_id = third.plaintext.page().clone();
    cache.publish(third).unwrap();
    assert!(cache.get(&second_id).unwrap().is_none());
    assert!(cache.get(&first_id).unwrap().is_some());
    drop(busy);
    let fourth = bundle(&admission, "v4");
    let fourth_id = fourth.plaintext.page().clone();
    cache.publish(fourth).unwrap();
    assert!(cache.get(&first_id).unwrap().is_some());
    assert!(cache.get(&third_id).unwrap().is_none());
    assert!(cache.get(&fourth_id).unwrap().is_some());
    assert_eq!(admission.used(ResourceClass::Plaintext), 6);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 38);
}
#[test]
fn publication_rejects_foreign_undercharged_and_conflicting_bundles() {
    let admission = admission(8);
    let cache = MemoryCache::new(
        BufferPool::new(admission.clone()),
        crate::test_support::availability(),
    );
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
    let original = bundle(&admission, "v1");
    let id = original.plaintext.page().clone();
    cache.publish(original).unwrap();
    let mut conflicting = bundle(&admission, "v1");
    Arc::get_mut(&mut conflicting.plaintext.inner)
        .unwrap()
        .bytes[0] ^= 1;
    assert_eq!(cache.publish(conflicting), Err(Error::CorruptRecord));
    let mut inconsistent = bundle(&admission, "v2");
    let rejected_id = inconsistent.plaintext.page().clone();
    inconsistent.metadata.version.etag = crate::model::StrongEtag::test_value("v1");
    assert_eq!(cache.publish(inconsistent), Err(Error::CorruptRecord));
    assert_eq!(cache.get(&id).unwrap().unwrap().plaintext.bytes(), &[1; 3]);
    assert!(cache.get(&rejected_id).unwrap().is_none());
    assert_eq!(admission.used(ResourceClass::Plaintext), 3);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 19);
}
#[test]
fn eviction_is_cache_scoped_and_preserves_live_leases() {
    let admission = admission(8);
    let other_cache = CacheId("44444444-4444-4444-8444-444444444444".into());
    let availability = crate::test_support::availability_for(vec![
        CacheId(crate::test_support::security::CACHE.into()),
        other_cache.clone(),
    ]);
    let cache = MemoryCache::new(BufferPool::new(admission.clone()), availability);
    let page = bundle(&admission, "v1");
    let id = page.plaintext.page().clone();
    let mut other_descriptor = page.metadata.immutable();
    other_descriptor.version.object.cache = other_cache;
    let other = bundle_for(&admission, other_descriptor.clone());
    let other_id = other.plaintext.page().clone();
    cache.publish(page.clone()).unwrap();
    cache.publish(other).unwrap();
    cache.remove_cache(&id.version.object.cache).unwrap();
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
    let cache = MemoryCache::new(
        BufferPool::new(admission),
        crate::test_support::availability(),
    );
    let id = CacheId("cache".into());
    assert_eq!(cache.remove_cache(&id), Ok(()));
    assert_eq!(cache.remove_cache(&id), Ok(()));
    assert_eq!(cache.remove_cache(&CacheId("other".into())), Ok(()));
    for n in 0..10_000 {
        cache.remove_cache(&CacheId(n.to_string())).unwrap();
    }
}
#[test]
fn empty_stable_catalog_rotates_without_consuming_page_metadata_capacity() {
    use crate::control::for_caches;
    use crate::test_support::security::rotation_bundle;
    let admission = admission(1024);
    let keys = Rc::new(crate::test_support::security::keys());
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let caches: Vec<_> = (0..356)
        .map(|cache| CacheId(format!("{cache:08x}-0000-4000-8000-000000000000")))
        .collect();
    let memory = MemoryCache::new(
        BufferPool::new(admission.clone()),
        for_caches(keys.clone(), caches.clone()),
    );
    use racer_control_wire::CacheKeyPurpose;
    let mut previous: Vec<racer_control_wire::CacheKeyRef> = Vec::new();
    for generation in 2u64..=6 {
        let mut next = rotation_bundle(generation, roots.clone());
        let templates = std::mem::take(&mut next.cache_keys);
        for cache in 0u64..356 {
            for template in &templates {
                let mut key = template.clone();
                key.key.cache = CacheId(format!("{cache:08x}-0000-4000-8000-000000000000"));
                let (reference, state, mut material) = key.clone().into_installation();
                material[8..16].copy_from_slice(&cache.to_be_bytes());
                key = racer_control_wire::CacheEncryptionKey::new(reference, state, material);
                next.cache_keys.push(key);
            }
        }
        keys.install(next.clone()).unwrap();
        for key in previous {
            if key.purpose == CacheKeyPurpose::Page {
                assert!(!memory.availability.page(&key.cache, key.id));
            }
        }
        previous = next.cache_keys.iter().map(|key| key.key.clone()).collect();
    }
    assert_eq!(memory.evict_idle(usize::MAX), Ok(0));
    assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
}
