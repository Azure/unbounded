//! Immutable key leases. Closing admission drops the registry owner; the last
//! operation/completion owner zeroizes the secret without a node-wide fence.
use super::identity::SigningIdentity;
use crate::{
    control::{enrollment::LocalSigningIdentity, wire::*},
    error::{Error, Result},
    model::{
        envelope::KeyId,
        identity::{CacheId, ClusterId, NodeId},
    },
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
    roots: Arc<Vec<Vec<u8>>>,
    entries: Vec<Entry>,
    identity: Option<Arc<SigningIdentity>>,
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
        next.cache_keys[0].key.id = KeyId([4; 16]);
        next.cache_keys[0].material = [10; 32];
        next.cache_keys.push(CacheEncryptionKey {
            key: reference.clone(),
            state: CacheKeyState::Retiring,
            material: [7; 32],
        });
        keys.install(next.clone()).unwrap();
        assert!(
            keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
                .is_err()
        );
        assert_eq!(
            keys.active(&cache, KeyPurpose::Page).unwrap().id(),
            KeyId([4; 16])
        );
        next.generation = BundleGeneration(3);
        next.cache_keys.pop();
        keys.install(next).unwrap();
        assert!(
            keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                .is_err()
        );
        assert!(
            keys.lease(Some(&cache), lease.id(), KeyPurpose::Page)
                .is_err()
        );
        let secret = Arc::downgrade(&lease.secret);
        assert_eq!(lease.material(KeyPurpose::Page).unwrap(), &[7; 32]);
        assert_eq!(secret.strong_count(), 1);
        drop(lease);
        assert!(secret.upgrade().is_none());
        // A later generation may reintroduce the immutable UID/key identity.
        // No process-lifetime tombstones accumulate across projection churn.
        assert!(
            keys.install(bundle(
                4,
                (*keys.peer_trust_roots().unwrap()).clone(),
                CacheKeyState::Active
            ))
            .is_ok()
        );
        assert!(
            keys.lease(Some(&cache), reference.id, KeyPurpose::Page)
                .is_ok()
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
        assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
        assert_eq!(secret.strong_count(), 1);
        assert!(
            keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
                .is_err()
        );
        keys.install(rotated.clone()).unwrap();
        assert!(
            keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
                .is_err()
        );
        // A replay acknowledges already installed configuration without resurrecting it.
        keys.install(rotated.clone()).unwrap();
        assert!(
            keys.lease(Some(&old.cache), old.id, KeyPurpose::Page)
                .is_err()
        );
        rotated.generation = BundleGeneration(3);
        keys.install(rotated).unwrap();
        drop(held);
        assert!(secret.upgrade().is_none());
    }
    #[test]
    fn active_crypto_operation_completes_after_rotation_with_its_original_key_lease() {
        use crate::{
            memory::pool::BufferPool,
            model::{identity::*, limits::ResourceClass},
            runtime::{
                crypto::{self, CryptoClient, CryptoInput, CryptoOutput},
                deadline::RequestScope,
                worker::{CryptoRuntime, CryptoService},
            },
            security::aead::PageCryptoEngine,
        };
        let keys = keys();
        let cache = CacheId(CACHE.into());
        use crate::runtime::reactor::IoBuffer;
        let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let pool = BufferPool::new(admission.clone());
        let (io, engine) = crypto::pair(WorkerId(0), 0, std::num::NonZeroUsize::new(8).unwrap());
        let client = CryptoClient::new(io);
        let mut engine = PageCryptoEngine::new(CryptoRuntime { port: engine });
        let mut plaintext = pool
            .plaintext(
                admission
                    .reserve(Some(&cache), ResourceClass::Plaintext, 3)
                    .unwrap(),
                3,
            )
            .unwrap();
        plaintext.bytes_mut().unwrap().copy_from_slice(b"abc");
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("held"),
            },
            number: PageNumber(0),
        };
        let lease = keys.active(&cache, KeyPurpose::Page).unwrap();
        let secret = Arc::downgrade(&lease.secret);
        let scope = RequestScope::new(
            RequestId([1; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(10),
        )
        .unwrap();
        let mut operation = client.execute(
            CryptoInput::Encrypt {
                page,
                plaintext,
                ciphertext: admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                    .unwrap(),
            },
            lease,
            &scope,
        );
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        let mut next = bundle(
            2,
            (*keys.peer_trust_roots().unwrap()).clone(),
            CacheKeyState::Active,
        );
        next.cache_keys[0].key.id = KeyId([4; 16]);
        next.cache_keys[0].material = [10; 32];
        keys.install(next).unwrap();
        assert!(
            keys.lease(Some(&cache), KeyId([1; 16]), KeyPurpose::Page)
                .is_err()
        );
        assert!(secret.upgrade().is_some());
        engine.poll_budgeted(8).unwrap();
        client.poll_budgeted(8).unwrap();
        let std::task::Poll::Ready(Ok(CryptoOutput::Encrypted(verified, ciphertext))) =
            operation.as_mut().poll(&mut cx)
        else {
            panic!("held operation did not complete");
        };
        assert_eq!(verified.bytes(), b"abc");
        assert_eq!(ciphertext.envelope().key_id, KeyId([1; 16]));
        drop(operation);
        assert!(secret.upgrade().is_none());
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
    secret: Arc<Secret>,
}
struct Secret(Zeroizing<[u8; 32]>);
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
    /// Exact immutable epoch identity, independent of current admission.
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
            if let Some(old) = state.entries.iter().find(|e| e.reference == candidate.key) {
                if *old.secret.0 != candidate.material {
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
        // Removed entries need no tombstone or polling state. Accepted jobs own
        // their Arc independently, including jobs whose caller has timed out.
        state.entries.retain(|entry| {
            bundle
                .cache_keys
                .iter()
                .any(|k| k.key == entry.reference && k.state != CacheKeyState::Retiring)
        });
        for entry in &mut state.entries {
            entry.state = bundle
                .cache_keys
                .iter()
                .find(|k| k.key == entry.reference)
                .map_or(CacheKeyState::Retiring, |k| k.state);
        }
        for candidate in &bundle.cache_keys {
            if candidate.state != CacheKeyState::Retiring
                && !state.entries.iter().any(|e| e.reference == candidate.key)
            {
                state.entries.push(Entry {
                    reference: candidate.key.clone(),
                    state: candidate.state,
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
            .find(|e| e.reference == reference)
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
            })
            .ok_or(Error::MissingKey)?;
        Ok(KeyLease {
            reference: entry.reference.clone(),
            secret: entry.secret.clone(),
        })
    }
}
