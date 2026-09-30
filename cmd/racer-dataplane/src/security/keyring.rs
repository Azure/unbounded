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
    identity_trust: Option<(Arc<Vec<Vec<u8>>>, u64, u64)>,
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
    /// Locally installed coherent bundle, not a controller acknowledgment.
    pub fn generation(&self) -> Result<Option<u64>> {
        Ok(self
            .epochs
            .state
            .lock()
            .map_err(|_| Error::Unavailable)?
            .generation
            .map(|g| g.0))
    }
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
        // Validate the downloaded epoch before acquiring the shared publication
        // lock. Existing immutable secret leases remain usable during validation.
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
        }
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if state.cluster.as_ref().is_some_and(|c| c != &self.cluster)
            || state.generation.is_some_and(|g| bundle.generation < g)
        {
            return Err(Error::InvalidConfiguration);
        }
        for candidate in &bundle.cache_keys {
            if state.generation == Some(bundle.generation) {
                continue;
            }
            if let Some(generation) = candidate.key.id.generation() {
                if generation == 0
                    || generation > bundle.generation.0
                    // Retiring declarations never admit leases or retain secrets.
                    // They may persist across many controller overlap bundles.
                    || (candidate.state != CacheKeyState::Retiring
                        && state.generation.is_some_and(|g| generation <= g.0)
                        && !state.entries.iter().any(|e| e.reference == candidate.key))
                {
                    return Err(Error::InvalidConfiguration);
                }
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
        state.entries.sort_by(|a, b| {
            (
                &a.reference.cache,
                a.reference.purpose as u8,
                a.reference.id.0,
            )
                .cmp(&(
                    &b.reference.cache,
                    b.reference.purpose as u8,
                    b.reference.id.0,
                ))
        });
        if *state.roots != bundle.peer_trust_roots {
            state.roots = Arc::new(bundle.peer_trust_roots.clone());
        }
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
        let roots = self.peer_trust_roots()?;
        super::certificates::verify_chain(
            &roots,
            identity.certificate_chain(),
            &self.cluster,
            &self.node,
        )?;
        let (_, until) =
            super::certificates::validity(identity.certificate_chain().iter().chain(roots.iter()))?;
        let checked = crate::runtime::environment::unix_time().as_secs();
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if !Arc::ptr_eq(&roots, &state.roots) {
            return Err(Error::Unavailable);
        }
        state.identity_trust = Some((roots, checked, until));
        state.identity = Some(identity);
        Ok(())
    }
    pub fn signing_identity(&self) -> Result<Arc<SigningIdentity>> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        let identity = state.identity.as_ref().ok_or(Error::MissingKey)?;
        if identity.node() != &self.node || identity.cluster() != &self.cluster {
            return Err(Error::Unauthorized);
        }
        let now = crate::runtime::environment::unix_time().as_secs();
        let identity = identity.clone();
        if state
            .identity_trust
            .as_ref()
            .is_some_and(|(roots, checked, until)| {
                Arc::ptr_eq(roots, &state.roots) && now >= *checked && now < *until
            })
        {
            return Ok(identity);
        }
        let roots = state.roots.clone();
        drop(state);
        super::certificates::verify_chain(
            &roots,
            identity.certificate_chain(),
            &self.cluster,
            &self.node,
        )?;
        let (_, until) =
            super::certificates::validity(identity.certificate_chain().iter().chain(roots.iter()))?;
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if !Arc::ptr_eq(&roots, &state.roots)
            || !state
                .identity
                .as_ref()
                .is_some_and(|current| Arc::ptr_eq(current, &identity))
        {
            return Err(Error::Unavailable);
        }
        state.identity_trust = Some((roots, now, until));
        Ok(identity)
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
        let position = state
            .entries
            .binary_search_by(|entry| {
                (
                    &entry.reference.cache,
                    entry.reference.purpose as u8,
                    entry.reference.id.0,
                )
                    .cmp(&(
                        &reference.cache,
                        reference.purpose as u8,
                        reference.id.0,
                    ))
            })
            .map_err(|_| Error::MissingKey)?;
        let entry = &state.entries[position];
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
        let first = state.entries.partition_point(|entry| {
            (&entry.reference.cache, entry.reference.purpose as u8) < (cache, purpose as u8)
        });
        let entry = state.entries[first..]
            .iter()
            .take_while(|entry| {
                &entry.reference.cache == cache && entry.reference.purpose == purpose
            })
            .find(|entry| entry.state == CacheKeyState::Active)
            .ok_or(Error::MissingKey)?;
        Ok(KeyLease {
            reference: entry.reference.clone(),
            secret: entry.secret.clone(),
        })
    }
}

#[cfg(test)]
#[path = "keyring_tests.rs"]
pub(crate) mod tests;
