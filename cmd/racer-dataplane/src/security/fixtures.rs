//! Application-local fixtures, never dependency cfg(test) exports.
use super::{identity::*, test_support};
use crate::{
    control::wire::*,
    model::{CacheId, ClusterId, NodeId},
};
use std::sync::Arc;
pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";
pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";
pub(crate) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
    let (ca, ca_key) = test_support::ca();
    let (pending, chain) = test_support::issue(
        &ca,
        &ca_key,
        &ClusterId(CLUSTER.into()),
        &NodeId(NODE.into()),
        |_| {},
    );
    (pending, chain, vec![ca.der().to_vec()])
}
pub(crate) fn keys() -> Keyring {
    keys_for(&[CacheId(CACHE.into())])
}
pub(crate) fn assert_page_key(lease: &KeyLease, expected: &[u8; 32]) {
    let mut sealed = [0; 19];
    lease
        .seal_page(lease.cache(), &[1; 24], b"retained", b"abc", &mut sealed)
        .unwrap();
    let mut opened = [0; 3];
    racer_crypto::aead::open(expected, &[1; 24], b"retained", &sealed, &mut opened).unwrap();
    assert_eq!(&opened, b"abc");
}
pub(crate) fn keys_for(caches: &[CacheId]) -> Keyring {
    let (_, _, roots) = issued();
    let keys = Keyring::new(
        ClusterId(CLUSTER.into()),
        NodeId(NODE.into()),
        Arc::new(KeyEpochs::default()),
    );
    let mut initial = rotation_bundle(1, roots);
    // keys() historically used the unrotated seed bundle; rotation_bundle(1)
    // intentionally has different suffixes and material just like later epochs.
    for (i, record) in initial.cache_keys.iter_mut().enumerate() {
        record.key.id = crate::model::key_id_from_generation(1, i as u32 + 1).unwrap();
        *record = CacheEncryptionKey::new(
            record.key.clone(),
            record.state,
            zeroize::Zeroizing::new([7 + i as u8; 32]),
        );
    }
    let templates = std::mem::take(&mut initial.cache_keys);
    for (i, cache) in caches.iter().enumerate() {
        for template in &templates {
            let (mut key, state, mut material) = template.clone().into_installation();
            key.cache = cache.clone();
            if i != 0 {
                material[..8].copy_from_slice(&(i as u64).to_be_bytes());
            }
            initial
                .cache_keys
                .push(CacheEncryptionKey::new(key, state, material));
        }
    }
    keys.install(initial).unwrap();
    keys
}
pub(crate) fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
    KeyringBundle {
        schema_version: SCHEMA_VERSION,
        cluster: ClusterId(CLUSTER.into()),
        generation: BundleGeneration(generation),
        peer_trust_roots: roots,
        cache_keys: [CacheKeyPurpose::Page, CacheKeyPurpose::OriginCredentials]
            .into_iter()
            .enumerate()
            .map(|(i, purpose)| {
                let mut material = zeroize::Zeroizing::new([7 + i as u8; 32]);
                material[..8].copy_from_slice(&generation.to_be_bytes());
                CacheEncryptionKey::new(
                    CacheKeyRef {
                        cache: CacheId(CACHE.into()),
                        id: crate::model::key_id_from_generation(generation, i as u32).unwrap(),
                        purpose,
                    },
                    CacheKeyState::Active,
                    material,
                )
            })
            .collect(),
    }
}
