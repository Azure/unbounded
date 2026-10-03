use super::tests::{CACHE, CLUSTER, NODE};
use super::*;
pub(crate) fn keys() -> Keyring {
    keys_for(&[CacheId(CACHE.into())])
}
pub(crate) fn keys_for(caches: &[CacheId]) -> Keyring {
    let (_, _, roots) = super::tests::issued();
    let keys = Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(NODE.into()),
        Arc::new(KeyEpochs::default()),
    );
    let mut initial = bundle(1, roots, CacheKeyState::Active);
    let templates = std::mem::take(&mut initial.cache_keys);
    for (i, cache) in caches.iter().enumerate() {
        for template in &templates {
            let mut key = template.clone();
            key.key.cache = cache.clone();
            if i != 0 {
                key.material[..8].copy_from_slice(&(i as u64).to_be_bytes());
            }
            initial.cache_keys.push(key);
        }
    }
    keys.install_inner(&initial).unwrap();
    keys
}
pub(crate) fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
    let mut next = bundle(generation, roots, CacheKeyState::Active);
    for (i, key) in next.cache_keys.iter_mut().enumerate() {
        key.key.id.0[..4].copy_from_slice(b"RKG1");
        key.key.id.0[4..12].copy_from_slice(&generation.to_be_bytes());
        key.key.id.0[12..].copy_from_slice(&(i as u32).to_be_bytes());
        key.material[..8].copy_from_slice(&generation.to_be_bytes());
    }
    next
}
#[test]
fn generation_bound_ids_reject_resurrection_skips_future_and_zero_epochs() {
    let keys = keys();
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let first = rotation_bundle(2, roots.clone());
    keys.install_inner(&first).unwrap();
    let lease = keys
        .active(&CacheId(CACHE.into()), KeyPurpose::Page)
        .unwrap();
    let secret = Arc::downgrade(&lease.secret);
    // Skipped bundle generations are normal after projected-secret delays.
    let next = rotation_bundle(100, roots.clone());
    keys.install_inner(&next).unwrap();
    let held = first.cache_keys[0].key.clone();
    assert_eq!(secret.strong_count(), 1);
    drop(lease);
    assert!(secret.upgrade().is_none());
    assert!(
        keys.lease(Some(&held.cache), held.id, KeyPurpose::Page)
            .is_err()
    );
    for generation in [0, 2, 99, 100, 102] {
        let mut bad = rotation_bundle(generation, roots.clone());
        bad.generation = BundleGeneration(101);
        bad.cache_keys[0].key.id.0[15] ^= 128;
        assert_eq!(keys.install_inner(&bad), Err(Error::InvalidConfiguration));
    }
    let mut resurrected = first.clone();
    resurrected.generation = BundleGeneration(101);
    assert_eq!(
        keys.install_inner(&resurrected),
        Err(Error::InvalidConfiguration)
    );
    assert_eq!(keys.install_inner(&first), Err(Error::InvalidConfiguration));
    assert_eq!(keys.install_inner(&next), Ok(BundleGeneration(100)));
    assert_eq!(keys.epochs.state.lock().unwrap().entries.len(), 2);
    let mut overlap = rotation_bundle(101, roots.clone());
    let removed = rotation_bundle(100, roots).cache_keys;
    keys.install_inner(&overlap).unwrap();
    let old = &removed[0].key;
    assert_eq!(keys.install_inner(&overlap), Ok(BundleGeneration(101)));
    assert!(
        keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
            .is_err()
    );
    // Later bundles and exact replays continue to omit removed epochs.
    overlap.generation = BundleGeneration(102);
    assert_eq!(keys.install_inner(&overlap), Ok(BundleGeneration(102)));
    for admission in [CacheKeyState::Active, CacheKeyState::Prepared] {
        let mut resurrected = overlap.clone();
        resurrected.generation = BundleGeneration(103);
        let mut old = removed[0].clone();
        old.state = admission;
        if admission == CacheKeyState::Active {
            resurrected.cache_keys.remove(0);
        }
        resurrected.cache_keys.push(old);
        assert_eq!(
            keys.install_inner(&resurrected),
            Err(Error::InvalidConfiguration)
        );
    }
    assert_eq!(keys.epochs.state.lock().unwrap().entries.len(), 2);
}

#[test]
fn every_install_rejects_opaque_zero_and_future_key_generations() {
    let keys = keys();
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    for generation in [1, 2] {
        for id in [
            KeyId([1; 16]),
            KeyId(*b"RKG1\0\0\0\0\0\0\0\0\0\0\0\0"),
            KeyId::from_generation(3, 1).unwrap(),
        ] {
            let mut bad = bundle(generation, roots.clone(), CacheKeyState::Active);
            bad.cache_keys[0].key.id = id;
            assert_eq!(keys.install_inner(&bad), Err(Error::InvalidConfiguration));
            assert_eq!(keys.generation().unwrap(), Some(1));
        }
    }
}
fn bundle(generation: u64, roots: Vec<Vec<u8>>, state: CacheKeyState) -> KeyringBundle {
    KeyringBundle {
        schema_version: SCHEMA_VERSION,
        cluster: ClusterId(CLUSTER.into()),
        generation: BundleGeneration(generation),
        peer_trust_roots: roots,
        cache_keys: vec![
            CacheEncryptionKey {
                key: CacheKeyRef {
                    cache: CacheId(CACHE.into()),
                    id: KeyId::from_generation(1, 1).unwrap(),
                    purpose: CacheKeyPurpose::Page,
                },
                state,
                material: Zeroizing::new([7; 32]),
            },
            CacheEncryptionKey {
                key: CacheKeyRef {
                    cache: CacheId(CACHE.into()),
                    id: KeyId::from_generation(1, 2).unwrap(),
                    purpose: CacheKeyPurpose::OriginCredentials,
                },
                state,
                material: Zeroizing::new([8; 32]),
            },
        ],
    }
}
#[test]
fn rotation_rejects_rollback_rebinding_and_cross_purpose_use() {
    let keys = keys();
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let cache = CacheId(CACHE.into());
    let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
    assert!(lease.material(KeyPurpose::OriginCredentials).is_err());
    assert!(
        keys.install_inner(&bundle(1, roots.clone(), CacheKeyState::Active))
            .is_ok()
    );
    let mut bad = bundle(2, roots.clone(), CacheKeyState::Active);
    bad.cache_keys[0].material = Zeroizing::new([9; 32]);
    assert!(keys.install_inner(&bad).is_err());
    let mut conflict = bundle(1, roots.clone(), CacheKeyState::Active);
    conflict.cache_keys[0].material = Zeroizing::new([9; 32]);
    assert!(keys.install_inner(&conflict).is_err());
    assert!(
        keys.install_inner(&bundle(0, roots.clone(), CacheKeyState::Active))
            .is_err()
    );
    assert!(
        keys.install_inner(&bundle(2, roots.clone(), CacheKeyState::Prepared))
            .is_err()
    );
    let mut rotated = bundle(2, roots.clone(), CacheKeyState::Active);
    rotated.cache_keys.push(CacheEncryptionKey {
        key: CacheKeyRef {
            cache: cache.clone(),
            id: KeyId::from_generation(2, 3).unwrap(),
            purpose: CacheKeyPurpose::Page,
        },
        state: CacheKeyState::Prepared,
        material: Zeroizing::new([9; 32]),
    });
    keys.install_inner(&rotated).unwrap();
    assert!(
        keys.install_inner(&bundle(1, roots, CacheKeyState::Active))
            .is_err()
    );
    assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
    assert!(
        keys.lease(
            Some(&cache),
            KeyId::from_generation(2, 3).unwrap(),
            KeyPurpose::Page
        )
        .is_ok()
    );
    assert!(
        keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
            .is_ok()
    );
    assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
    assert_eq!(
        keys.active(&cache, KeyPurpose::Page).unwrap().id(),
        lease.id()
    );
    let prepared = keys
        .lease(
            Some(&cache),
            KeyId::from_generation(2, 3).unwrap(),
            KeyPurpose::Page,
        )
        .unwrap();
    assert_eq!(prepared.material(KeyPurpose::Page).unwrap(), &[9; 32]);
    // Activation retains the prepared material and drops the replaced key.
    rotated.generation = BundleGeneration(3);
    rotated.cache_keys.remove(0);
    rotated.cache_keys[1].state = CacheKeyState::Active;
    keys.install_inner(&rotated).unwrap();
    let active = keys.active(&cache, KeyPurpose::Page).unwrap();
    assert_eq!(active.id(), prepared.id());
    assert!(Arc::ptr_eq(&active.secret, &prepared.secret));
    assert!(
        keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
            .is_err()
    );
    assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
}
#[test]
fn retirement_closes_admission_and_last_lease_owns_secret() {
    let keys = keys();
    let cache = CacheId(CACHE.into());
    let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
    let reference = lease.reference.clone();
    let mut next = bundle(
        2,
        (*keys.peer_trust_roots().unwrap()).clone(),
        CacheKeyState::Active,
    );
    next.cache_keys[0].key.id = KeyId::from_generation(2, 4).unwrap();
    next.cache_keys[0].material = Zeroizing::new([10; 32]);
    keys.install_inner(&next).unwrap();
    assert!(
        keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
            .is_err()
    );
    assert_eq!(
        keys.active(&cache, KeyPurpose::Page).unwrap().id(),
        KeyId::from_generation(2, 4).unwrap()
    );
    next.generation = BundleGeneration(3);
    keys.install_inner(&next).unwrap();
    assert!(
        keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
            .is_err()
    );
    let secret = Arc::downgrade(&lease.secret);
    assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
    assert_eq!(secret.strong_count(), 1);
    drop(lease);
    assert!(secret.upgrade().is_none());
    // A removed epoch cannot be resurrected in a later publication.
    assert!(
        keys.install_inner(&bundle(
            4,
            (*keys.peer_trust_roots().unwrap()).clone(),
            CacheKeyState::Active
        ))
        .is_err()
    );
    assert!(
        keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
            .is_err()
    );
}
#[test]
fn explicit_retirement_blocks_published_keys_without_external_fences() {
    let keys = keys();
    let held = keys
        .active(&CacheId(CACHE.into()), KeyPurpose::Page)
        .unwrap();
    let secret = Arc::downgrade(&held.secret);
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let mut rotated = bundle(2, roots, CacheKeyState::Active);
    let old = rotated.cache_keys[0].key.clone();
    rotated.cache_keys[0] = CacheEncryptionKey {
        key: CacheKeyRef {
            id: KeyId::from_generation(2, 4).unwrap(),
            ..old.clone()
        },
        state: CacheKeyState::Active,
        material: Zeroizing::new([10; 32]),
    };
    keys.install_inner(&rotated).unwrap();
    assert_eq!(keys.generation().unwrap(), Some(2));
    assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
    assert_eq!(secret.strong_count(), 1);
    assert!(
        keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
            .is_err()
    );
    keys.install_inner(&rotated).unwrap();
    assert!(
        keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
            .is_err()
    );
    // A replay acknowledges already installed configuration without resurrecting it.
    keys.install_inner(&rotated).unwrap();
    assert!(
        keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
            .is_err()
    );
    rotated.generation = BundleGeneration(3);
    keys.install_inner(&rotated).unwrap();
    assert_eq!(keys.generation().unwrap(), Some(3));
    drop(held);
    assert!(secret.upgrade().is_none());
}
#[test]
fn removal_of_all_keys_preserves_leases_and_rejects_resurrection() {
    let keys = keys();
    let cache = CacheId(CACHE.into());
    let held = keys.active(&cache, KeyPurpose::Page).unwrap();
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let mut empty = bundle(2, roots.clone(), CacheKeyState::Active);
    empty.cache_keys.clear();
    keys.install_inner(&empty).unwrap();
    keys.install_inner(&empty).unwrap();
    assert!(keys.epochs.state.lock().unwrap().entries.is_empty());
    assert!(keys.active(&cache, KeyPurpose::Page).is_err());
    assert!(
        keys.lease(Some(&cache), held.id(), KeyPurpose::Page)
            .is_err()
    );
    assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
    let mut resurrected = rotation_bundle(3, roots.clone());
    let mut old = bundle(1, roots.clone(), CacheKeyState::Prepared).cache_keys;
    resurrected.cache_keys.append(&mut old);
    assert_eq!(
        keys.install_inner(&resurrected),
        Err(Error::InvalidConfiguration)
    );
    assert_eq!(keys.generation().unwrap(), Some(2));
    keys.install_inner(&rotation_bundle(3, roots)).unwrap();
    assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
    assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
}

#[test]
fn capacity_is_bounded_by_current_bundle_not_outstanding_leases() {
    let keys = keys();
    let held = keys
        .active(&CacheId(CACHE.into()), KeyPurpose::Page)
        .unwrap();
    let roots = (*keys.peer_trust_roots().unwrap()).clone();
    let mut full = rotation_bundle(2, roots.clone());
    let template = full.cache_keys[0].clone();
    full.cache_keys = (0..4096u32)
        .map(|i| {
            let mut key = template.clone();
            key.key.cache = CacheId(format!("{i:08x}-1111-4111-8111-111111111111"));
            key.key.id = KeyId::from_generation(2, i).unwrap();
            key.material[8..12].copy_from_slice(&i.to_be_bytes());
            key
        })
        .collect();
    keys.install_inner(&full).unwrap();
    assert_eq!(keys.epochs.state.lock().unwrap().entries.len(), 4096);
    full.generation = BundleGeneration(3);
    full.cache_keys.push(template);
    assert_eq!(keys.install_inner(&full), Err(Error::InvalidConfiguration));
    assert_eq!(keys.generation().unwrap(), Some(2));
    assert_eq!(keys.epochs.state.lock().unwrap().entries.len(), 4096);
    keys.install_inner(&rotation_bundle(3, roots)).unwrap();
    assert_eq!(keys.epochs.state.lock().unwrap().entries.len(), 2);
    assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
}

#[test]
fn identity_installation_and_leases_follow_current_trust() {
    use super::tests::{CLUSTER, NODE};
    let (pending, chain, roots) = super::tests::issued();
    let identity = Arc::new(
        pending
            .accept(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                chain,
                &roots,
            )
            .unwrap(),
    );
    let keys = Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(NODE.into()),
        Arc::new(KeyEpochs::default()),
    );
    keys.install_inner(&bundle(1, roots, CacheKeyState::Active))
        .unwrap();
    keys.install_signing_identity(identity.clone()).unwrap();
    let leased = keys.signing_identity().unwrap();
    assert_eq!(
        leased.sign(b"admitted operation").unwrap(),
        identity.sign(b"admitted operation").unwrap()
    );
    let (_, _, replacement_roots) = super::tests::issued();
    keys.install_inner(&bundle(2, replacement_roots, CacheKeyState::Active))
        .unwrap();
    assert!(keys.signing_identity().is_err());
    assert!(keys.install_signing_identity(identity).is_err());
    // Already admitted owners remain memory-safe during trust replacement.
    assert!(leased.sign(b"admitted operation").is_ok());
}
