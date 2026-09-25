//! Immutable leased key epochs with explicit, fail-closed retirement fences.
use super::identity::SigningIdentity;
use crate::{
    control::{enrollment::LocalSigningIdentity, wire::*},
    error::{Error, Operation, Result},
    model::{
        envelope::KeyId,
        identity::{CacheId, ClusterId, NodeId},
    },
    runtime::deadline::RequestScope,
};
use sha2::{Digest, Sha256};
use std::sync::{Arc, Mutex};
use zeroize::{Zeroize, Zeroizing};

#[derive(Default)]
pub struct KeyEpochs {
    state: Mutex<State>,
}
#[derive(Default)]
struct State {
    cluster: Option<ClusterId>,
    generation: Option<BundleGeneration>,
    fingerprint: Option<[u8; 32]>,
    retired: Vec<CacheKeyRef>,
    roots: Arc<Vec<Vec<u8>>>,
    entries: Vec<Entry>,
    identity: Option<Arc<SigningIdentity>>,
    barriers: Option<Arc<dyn RetirementBarriers>>,
}
#[cfg(test)]
pub(crate) mod tests {
    use super::super::identity::tests::{CACHE, CLUSTER, NODE};
    use super::*;
    pub(crate) fn keys() -> Keyring {
        let (_, _, roots) = super::super::identity::tests::issued();
        let keys = Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::default()),
        );
        keys.install(bundle(1, roots, CacheKeyState::Active))
            .unwrap();
        keys
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
                        id: KeyId([1; 16]),
                        purpose: CacheKeyPurpose::Page,
                    },
                    state,
                    material: [7; 32],
                },
                CacheEncryptionKey {
                    key: CacheKeyRef {
                        cache: CacheId(CACHE.into()),
                        id: KeyId([2; 16]),
                        purpose: CacheKeyPurpose::OriginCredentials,
                    },
                    state,
                    material: [8; 32],
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
            keys.install(bundle(1, roots.clone(), CacheKeyState::Active))
                .is_ok()
        );
        let mut bad = bundle(2, roots.clone(), CacheKeyState::Active);
        bad.cache_keys[0].material = [9; 32];
        assert!(keys.install(bad).is_err());
        let mut conflict = bundle(1, roots.clone(), CacheKeyState::Active);
        conflict.cache_keys[0].material = [9; 32];
        assert!(keys.install(conflict).is_err());
        assert!(
            keys.install(bundle(0, roots.clone(), CacheKeyState::Active))
                .is_err()
        );
        assert!(
            keys.install(bundle(2, roots.clone(), CacheKeyState::Prepared))
                .is_err()
        );
        let mut rotated = bundle(2, roots.clone(), CacheKeyState::Active);
        rotated.cache_keys.push(CacheEncryptionKey {
            key: CacheKeyRef {
                cache: cache.clone(),
                id: KeyId([3; 16]),
                purpose: CacheKeyPurpose::Page,
            },
            state: CacheKeyState::Prepared,
            material: [9; 32],
        });
        keys.install(rotated).unwrap();
        assert!(
            keys.install(bundle(1, roots, CacheKeyState::Active))
                .is_err()
        );
        assert!(keys.active(&cache, KeyPurpose::Page).is_ok());
        assert!(
            keys.lease(Some(&cache), KeyId([3; 16]), KeyPurpose::Page)
                .is_ok()
        );
        assert!(
            keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                .is_ok()
        );
        assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
    }
    #[test]
    fn retirement_requires_barriers_and_last_lease() {
        let keys = keys();
        let cache = CacheId(CACHE.into());
        let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
        let reference = lease.reference.clone();
        let mut next = bundle(
            2,
            (*keys.peer_trust_roots().unwrap()).clone(),
            CacheKeyState::Active,
        );
        next.cache_keys[0].key.id = KeyId([4; 16]);
        next.cache_keys[0].material = [10; 32];
        next.cache_keys.push(CacheEncryptionKey {
            key: reference.clone(),
            state: CacheKeyState::Retiring,
            material: [7; 32],
        });
        keys.install(next.clone()).unwrap();
        assert!(keys.pending_retirements().unwrap().is_empty());
        assert!(
            keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
                .is_ok()
        );
        assert_eq!(
            keys.active(&cache, KeyPurpose::Page).unwrap().id(),
            KeyId([4; 16])
        );
        next.generation = BundleGeneration(3);
        next.cache_keys.pop();
        keys.install(next).unwrap();
        assert_eq!(keys.pending_retirements().unwrap(), vec![reference.clone()]);
        assert!(
            keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                .is_err()
        );
        let scope = RequestScope::new(
            crate::model::identity::RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        assert!(futures::executor::block_on(keys.retire(&reference, &scope)).is_err());
        assert!(
            keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                .is_err()
        );
        struct Fence;
        impl RetirementBarriers for Fence {
            fn fence(&self, _: &CacheKeyRef) -> Result<bool> {
                Ok(true)
            }
        }
        keys.register_retirement_barriers(Arc::new(Fence)).unwrap();
        assert!(futures::executor::block_on(keys.retire(&reference, &scope)).is_err());
        drop(lease);
        futures::executor::block_on(keys.retire(&reference, &scope)).unwrap();
        assert!(
            keys.install(bundle(
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
    fn explicit_retirement_blocks_published_keys_before_fences_complete() {
        use std::sync::atomic::{AtomicBool, Ordering};
        struct Fence(AtomicBool);
        impl RetirementBarriers for Fence {
            fn fence(&self, _: &CacheKeyRef) -> Result<bool> {
                Ok(self.0.load(Ordering::Acquire))
            }
        }
        let keys = keys();
        let roots = (*keys.peer_trust_roots().unwrap()).clone();
        let mut rotated = bundle(2, roots, CacheKeyState::Active);
        let old = rotated.cache_keys[0].key.clone();
        rotated.cache_keys[0].state = CacheKeyState::Retiring;
        rotated.cache_keys.push(CacheEncryptionKey {
            key: CacheKeyRef {
                id: KeyId([4; 16]),
                ..old.clone()
            },
            state: CacheKeyState::Active,
            material: [10; 32],
        });
        keys.install(rotated.clone()).unwrap();
        let fence = Arc::new(Fence(AtomicBool::new(false)));
        keys.register_retirement_barriers(fence.clone()).unwrap();
        let scope = RequestScope::new(
            crate::model::identity::RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        assert!(futures::executor::block_on(keys.retire(&old, &scope)).is_err());
        assert_eq!(keys.pending_retirements().unwrap(), vec![old.clone()]);
        assert!(
            keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
                .is_err()
        );
        keys.install(rotated.clone()).unwrap();
        assert!(
            keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
                .is_err()
        );
        fence.0.store(true, Ordering::Release);
        futures::executor::block_on(keys.retire(&old, &scope)).unwrap();
        // A replay acknowledges already installed configuration without resurrecting it.
        keys.install(rotated.clone()).unwrap();
        assert!(keys.pending_retirements().unwrap().is_empty());
        assert!(
            keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
                .is_err()
        );
        rotated.generation = BundleGeneration(3);
        assert!(keys.install(rotated).is_err());
    }
    #[test]
    fn identity_installation_and_leases_follow_current_trust() {
        use super::super::identity::tests::{CLUSTER, NODE};
        let (pending, chain, roots) = super::super::identity::tests::issued();
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
        keys.install(bundle(1, roots, CacheKeyState::Active))
            .unwrap();
        keys.install_signing_identity(identity.clone()).unwrap();
        let leased = keys.signing_identity().unwrap();
        assert_eq!(
            leased.sign(b"admitted operation").unwrap(),
            identity.sign(b"admitted operation").unwrap()
        );
        let (_, _, replacement_roots) = super::super::identity::tests::issued();
        keys.install(bundle(2, replacement_roots, CacheKeyState::Active))
            .unwrap();
        assert!(keys.signing_identity().is_err());
        assert!(keys.install_signing_identity(identity).is_err());
        // Already admitted owners remain memory-safe during trust replacement.
        assert!(leased.sign(b"admitted operation").is_ok());
    }
}
struct Entry {
    reference: CacheKeyRef,
    state: CacheKeyState,
    blocked: bool,
    secret: Arc<Secret>,
}
struct Secret(Zeroizing<[u8; 32]>);
/// Implementations must fence storage, checkpoints, memory, transport, and late
/// writes for this exact epoch. `true` is an irrevocable fence, not a snapshot.
pub trait RetirementBarriers: Send + Sync {
    fn fence(&self, key: &CacheKeyRef) -> Result<bool>;
}
pub struct Keyring {
    cluster: ClusterId,
    node: NodeId,
    epochs: Arc<KeyEpochs>,
}
pub struct KeyLease {
    reference: CacheKeyRef,
    secret: Arc<Secret>,
}
#[derive(Clone, Copy, Eq, PartialEq)]
pub enum KeyPurpose {
    Page,
    OriginCredentials,
    NodeSigning,
    PeerVerification,
}
impl KeyPurpose {
    fn cache(self) -> Result<CacheKeyPurpose> {
        match self {
            Self::Page => Ok(CacheKeyPurpose::Page),
            Self::OriginCredentials => Ok(CacheKeyPurpose::OriginCredentials),
            _ => Err(Error::MissingKey),
        }
    }
}
impl KeyLease {
    pub fn id(&self) -> KeyId {
        self.reference.id
    }
    pub fn cache(&self) -> &CacheId {
        &self.reference.cache
    }
    /// Exact immutable epoch identity for storage/transport retirement fences.
    pub fn reference(&self) -> &CacheKeyRef {
        &self.reference
    }
    pub(crate) fn material(&self, purpose: KeyPurpose) -> Result<&[u8; 32]> {
        if self.reference.purpose != purpose.cache()? {
            return Err(Error::MissingKey);
        }
        Ok(&self.secret.0)
    }
}
impl Keyring {
    pub fn new(cluster: ClusterId, node: NodeId, epochs: Arc<KeyEpochs>) -> Self {
        Self {
            cluster,
            node,
            epochs,
        }
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn node(&self) -> &NodeId {
        &self.node
    }
    pub fn peer_trust_roots(&self) -> Result<Arc<Vec<Vec<u8>>>> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.cluster.as_ref() != Some(&self.cluster) || state.roots.is_empty() {
            return Err(Error::MissingKey);
        }
        Ok(state.roots.clone())
    }
    pub fn install(&self, mut bundle: KeyringBundle) -> Result<BundleGeneration> {
        // Wipe DTO secrets on every validation outcome, including rejected bundles.
        let result = self.install_inner(&bundle);
        for key in &mut bundle.cache_keys {
            key.material.zeroize();
        }
        result
    }
    fn install_inner(&self, bundle: &KeyringBundle) -> Result<BundleGeneration> {
        if bundle.schema_version != SCHEMA_VERSION
            || bundle.cluster != self.cluster
            || bundle.cache_keys.len() > 4096
            || bundle.generation.0 == 0
            || !super::certificates::canonical_uuid(&self.cluster.0)
            || !super::certificates::canonical_uuid(&self.node.0)
        {
            return Err(Error::InvalidConfiguration);
        }
        super::certificates::root_store(&bundle.peer_trust_roots)?;
        let mut hash = Sha256::new();
        hash.update(b"racer/keyring/bundle/v1\0");
        hash.update((bundle.peer_trust_roots.len() as u64).to_be_bytes());
        for root in &bundle.peer_trust_roots {
            hash.update((root.len() as u64).to_be_bytes());
            hash.update(root);
        }
        hash.update((bundle.cache_keys.len() as u64).to_be_bytes());
        for key in &bundle.cache_keys {
            hash.update((key.key.cache.0.len() as u64).to_be_bytes());
            hash.update(key.key.cache.0.as_bytes());
            hash.update(key.key.id.0);
            hash.update([key.key.purpose as u8, key.state as u8]);
            hash.update(key.material);
        }
        let fingerprint: [u8; 32] = hash.finalize().into();
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.cluster.as_ref().is_some_and(|c| c != &self.cluster)
            || state.generation.is_some_and(|g| bundle.generation < g)
        {
            return Err(Error::InvalidConfiguration);
        }
        for (i, candidate) in bundle.cache_keys.iter().enumerate() {
            if !super::certificates::canonical_uuid(&candidate.key.cache.0) {
                return Err(Error::InvalidConfiguration);
            }
            if bundle
                .cache_keys
                .iter()
                .filter(|k| {
                    k.key.cache == candidate.key.cache
                        && k.key.purpose == candidate.key.purpose
                        && k.state == CacheKeyState::Active
                })
                .count()
                != 1
            {
                return Err(Error::InvalidConfiguration);
            }
            for other in &bundle.cache_keys[..i] {
                if candidate.key == other.key
                    || candidate.material == other.material
                    || (candidate.state == CacheKeyState::Active
                        && other.state == CacheKeyState::Active
                        && candidate.key.cache == other.key.cache
                        && candidate.key.purpose == other.key.purpose)
                {
                    return Err(Error::InvalidConfiguration);
                }
            }
            if state.generation == Some(bundle.generation) {
                continue;
            }
            if state.retired.contains(&candidate.key) {
                return Err(Error::InvalidConfiguration);
            }
            if let Some(old) = state.entries.iter().find(|e| e.reference == candidate.key) {
                if *old.secret.0 != candidate.material
                    || (old.blocked && candidate.state != CacheKeyState::Retiring)
                    || (old.state == CacheKeyState::Retiring
                        && candidate.state != CacheKeyState::Retiring)
                {
                    return Err(Error::InvalidConfiguration);
                }
            }
            if state
                .entries
                .iter()
                .any(|e| e.reference != candidate.key && *e.secret.0 == candidate.material)
            {
                return Err(Error::InvalidConfiguration);
            }
        }
        if state.generation == Some(bundle.generation) {
            return if state.fingerprint == Some(fingerprint) {
                Ok(bundle.generation)
            } else {
                Err(Error::InvalidConfiguration)
            };
        }
        if state
            .entries
            .len()
            .checked_add(
                bundle
                    .cache_keys
                    .iter()
                    .filter(|k| !state.entries.iter().any(|e| e.reference == k.key))
                    .count(),
            )
            .is_none_or(|n| n > 8192)
        {
            return Err(Error::Overloaded);
        }
        for entry in &mut state.entries {
            entry.state = bundle
                .cache_keys
                .iter()
                .find(|k| k.key == entry.reference)
                .map_or(CacheKeyState::Retiring, |k| k.state);
            entry.blocked |= !bundle.cache_keys.iter().any(|k| k.key == entry.reference);
        }
        for candidate in &bundle.cache_keys {
            if !state.entries.iter().any(|e| e.reference == candidate.key) {
                state.entries.push(Entry {
                    reference: candidate.key.clone(),
                    state: candidate.state,
                    blocked: false,
                    secret: Arc::new(Secret(Zeroizing::new(candidate.material))),
                });
            }
        }
        state.roots = Arc::new(bundle.peer_trust_roots.clone());
        state.cluster = Some(self.cluster.clone());
        state.generation = Some(bundle.generation);
        state.fingerprint = Some(fingerprint);
        Ok(bundle.generation)
    }
    /// Convert control's private persistence owner using the current peer roots.
    pub fn install_identity(&self, identity: LocalSigningIdentity) -> Result<()> {
        let roots = self.peer_trust_roots()?;
        self.install_signing_identity(identity.signing_identity(&roots)?)
    }
    pub fn pending_retirements(&self) -> Result<Vec<CacheKeyRef>> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.cluster.as_ref() != Some(&self.cluster) {
            return Err(Error::MissingKey);
        }
        Ok(state
            .entries
            .iter()
            .filter(|e| e.blocked)
            .map(|e| e.reference.clone())
            .collect())
    }
    pub fn install_signing_identity(&self, identity: Arc<SigningIdentity>) -> Result<()> {
        if identity.node() != &self.node || identity.cluster() != &self.cluster {
            return Err(Error::Unauthorized);
        }
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.cluster.as_ref() != Some(&self.cluster) {
            return Err(Error::MissingKey);
        }
        super::certificates::verify_chain(
            &state.roots,
            identity.certificate_chain(),
            &self.cluster,
            &self.node,
        )?;
        state.identity = Some(identity);
        Ok(())
    }
    pub fn signing_identity(&self) -> Result<Arc<SigningIdentity>> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        let identity = state.identity.as_ref().ok_or(Error::MissingKey)?;
        if identity.node() != &self.node || identity.cluster() != &self.cluster {
            return Err(Error::Unauthorized);
        }
        super::certificates::verify_chain(
            &state.roots,
            identity.certificate_chain(),
            &self.cluster,
            &self.node,
        )?;
        Ok(identity.clone())
    }
    pub fn lease(
        &self,
        cache: Option<&CacheId>,
        id: KeyId,
        purpose: KeyPurpose,
    ) -> Result<KeyLease> {
        let reference = CacheKeyRef {
            cache: cache.ok_or(Error::MissingKey)?.clone(),
            id,
            purpose: purpose.cache()?,
        };
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.cluster.as_ref() != Some(&self.cluster) {
            return Err(Error::MissingKey);
        }
        let entry = state
            .entries
            .iter()
            .find(|e| e.reference == reference && !e.blocked)
            .ok_or(Error::MissingKey)?;
        Ok(KeyLease {
            reference,
            secret: entry.secret.clone(),
        })
    }
    pub fn active(&self, cache: &CacheId, purpose: KeyPurpose) -> Result<KeyLease> {
        let purpose = purpose.cache()?;
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.cluster.as_ref() != Some(&self.cluster) {
            return Err(Error::MissingKey);
        }
        let entry = state
            .entries
            .iter()
            .find(|e| {
                &e.reference.cache == cache
                    && e.reference.purpose == purpose
                    && e.state == CacheKeyState::Active
                    && !e.blocked
            })
            .ok_or(Error::MissingKey)?;
        Ok(KeyLease {
            reference: entry.reference.clone(),
            secret: entry.secret.clone(),
        })
    }
    pub fn register_retirement_barriers(
        &self,
        barriers: Arc<dyn RetirementBarriers>,
    ) -> Result<()> {
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.barriers.is_some() {
            return Err(Error::InvalidConfiguration);
        }
        state.barriers = Some(barriers);
        Ok(())
    }
    pub fn retire<'a>(
        &'a self,
        key: &'a CacheKeyRef,
        scope: &'a RequestScope,
    ) -> Operation<'a, ()> {
        Box::pin(async move {
            scope.check()?;
            let barriers = {
                let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
                if state.cluster.as_ref() != Some(&self.cluster) {
                    return Err(Error::MissingKey);
                }
                let entry = state
                    .entries
                    .iter_mut()
                    .find(|e| &e.reference == key)
                    .ok_or(Error::MissingKey)?;
                if entry.state != CacheKeyState::Retiring {
                    return Err(Error::InvalidRequest);
                }
                entry.blocked = true;
                state.barriers.clone().ok_or(Error::Unavailable)?
            };
            if !barriers.fence(key)? {
                return Err(Error::Unavailable);
            }
            scope.check()?;
            let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
            let index = state
                .entries
                .iter()
                .position(|e| &e.reference == key)
                .ok_or(Error::MissingKey)?;
            if Arc::strong_count(&state.entries[index].secret) != 1 {
                return Err(Error::Unavailable);
            }
            if state.retired.len() >= 8192 {
                return Err(Error::Overloaded);
            }
            state.retired.push(key.clone());
            state.entries.remove(index);
            Ok(())
        })
    }
}
