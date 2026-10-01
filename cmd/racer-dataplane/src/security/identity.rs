//! Trust roots, certificate validation, and locally generated Ed25519 identities.
//! Immutable key leases outlive admission; their last completion owner zeroizes
//! the secret. Secret export is explicit and zeroizing.
use crate::{
    control::wire::*,
    error::{Error, Result},
    model::{CacheId, ClusterId, KeyId, NodeId},
};
use ed25519_dalek::{
    Signature, Signer, SigningKey, VerifyingKey,
    pkcs8::{DecodePrivateKey, EncodePrivateKey},
};
use rustls::{
    RootCertStore,
    pki_types::{CertificateDer, PrivateKeyDer, PrivatePkcs8KeyDer},
};
use sha2::{Digest, Sha256};
use std::{
    cell::RefCell,
    collections::VecDeque,
    rc::Rc,
    sync::{Arc, Mutex},
};
use x509_parser::{extensions::GeneralName, parse_x509_certificate};
use zeroize::{Zeroize, Zeroizing};

pub struct PendingIdentity {
    key: SigningKey,
}
pub struct SigningIdentity {
    cluster: ClusterId,
    node: NodeId,
    key: SigningKey,
    chain: Vec<Vec<u8>>,
    expires: u64,
    valid_from: u64,
}
impl PendingIdentity {
    pub fn generate() -> Result<Self> {
        let mut seed = Zeroizing::new([0u8; 32]);
        crate::runtime::environment::fill_random(&mut *seed).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            key: SigningKey::from_bytes(&seed),
        })
    }
    pub fn recover(pkcs8: &[u8]) -> Result<Self> {
        if pkcs8.len() > 4096 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            key: SigningKey::from_pkcs8_der(pkcs8).map_err(|_| Error::Unauthorized)?,
        })
    }
    pub fn export_pkcs8_for_persistence(&self) -> Result<Zeroizing<Vec<u8>>> {
        Ok(Zeroizing::new(
            self.key
                .to_pkcs8_der()
                .map_err(|_| Error::Unavailable)?
                .as_bytes()
                .to_vec(),
        ))
    }
    /// The control server assigns the node SAN from authenticated enrollment.
    pub fn csr_der(&self) -> Result<Vec<u8>> {
        let bytes = self.export_pkcs8_for_persistence()?;
        let der = PrivatePkcs8KeyDer::from(bytes.as_slice());
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(&der, &rcgen::PKCS_ED25519)
            .map_err(|_| Error::Unavailable)?;
        let params =
            rcgen::CertificateParams::new(Vec::<String>::new()).map_err(|_| Error::Unavailable)?;
        Ok(params
            .serialize_request(&key)
            .map_err(|_| Error::Unavailable)?
            .der()
            .to_vec())
    }
    pub fn accept(
        self,
        cluster: ClusterId,
        node: NodeId,
        chain: Vec<Vec<u8>>,
        roots: &[Vec<u8>],
    ) -> Result<SigningIdentity> {
        let public = verify_chain(roots, &chain, &cluster, &node)?;
        if public != self.key.verifying_key() {
            return Err(Error::Unauthorized);
        }
        let (valid_from, expires) = validity(chain.iter().chain(roots.iter()))?;
        Ok(SigningIdentity {
            cluster,
            node,
            key: self.key,
            expires,
            valid_from,
            chain,
        })
    }
}
impl SigningIdentity {
    pub fn expires_at_seconds(&self) -> u64 {
        self.expires
    }
    pub fn from_pkcs8(
        cluster: ClusterId,
        node: NodeId,
        pkcs8: &[u8],
        chain: Vec<Vec<u8>>,
        roots: &[Vec<u8>],
    ) -> Result<Self> {
        PendingIdentity::recover(pkcs8)?.accept(cluster, node, chain, roots)
    }
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    pub fn node(&self) -> &NodeId {
        &self.node
    }
    pub fn certificate_chain(&self) -> &[Vec<u8>] {
        &self.chain
    }
    pub fn sign(&self, message: &[u8]) -> Result<Vec<u8>> {
        let now = crate::runtime::environment::unix_time().as_secs();
        if now < self.valid_from || now >= self.expires {
            return Err(Error::Unauthorized);
        }
        Ok(self.key.sign(message).to_bytes().to_vec())
    }
    pub fn export_pkcs8_for_persistence(&self) -> Result<Zeroizing<Vec<u8>>> {
        Ok(Zeroizing::new(
            self.key
                .to_pkcs8_der()
                .map_err(|_| Error::Unavailable)?
                .as_bytes()
                .to_vec(),
        ))
    }
    pub fn tls_certified_key(&self) -> Result<rustls::sign::CertifiedKey> {
        let bytes = self.export_pkcs8_for_persistence()?;
        let private = PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(bytes.as_slice()));
        let key = rustls::crypto::ring::sign::any_supported_type(&private)
            .map_err(|_| Error::Unauthorized)?;
        let certified = rustls::sign::CertifiedKey::new(
            self.chain
                .iter()
                .cloned()
                .map(CertificateDer::from)
                .collect(),
            key,
        );
        certified.keys_match().map_err(|_| Error::Unauthorized)?;
        Ok(certified)
    }
}

/// Worker-local verifier caching exact chains against the current trust roots.
pub struct Certificates {
    cluster: ClusterId,
    keys: Rc<Keyring>,
    cache: RefCell<VecDeque<CachedPeer>>,
}
struct CachedPeer {
    roots: Arc<Vec<Vec<u8>>>,
    chain: Vec<Vec<u8>>,
    node: NodeId,
    key: VerifyingKey,
    until: u64,
    checked: u64,
}
pub struct VerifiedPeer {
    node: NodeId,
}
impl VerifiedPeer {
    pub fn node(&self) -> &NodeId {
        &self.node
    }
}
impl Certificates {
    pub fn new(cluster: ClusterId, keys: Rc<Keyring>) -> Self {
        Self {
            cluster,
            keys,
            cache: RefCell::new(VecDeque::new()),
        }
    }
    fn key(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifyingKey> {
        let roots = self.keys.peer_trust_roots()?;
        let now = crate::runtime::environment::unix_time().as_secs();
        let mut cache = self.cache.borrow_mut();
        cache.retain(|entry| {
            Arc::ptr_eq(&entry.roots, &roots) && now >= entry.checked && now < entry.until
        });
        if let Some(entry) = cache
            .iter()
            .find(|entry| &entry.node == expected && entry.chain == chain)
        {
            return Ok(entry.key);
        }
        drop(cache);
        let key = verify_chain(&roots, chain, &self.cluster, expected)?;
        let (_, until) = validity(chain.iter().chain(roots.iter()))?;
        let mut cache = self.cache.borrow_mut();
        // At most 64 exact, wire-bounded chains (4 MiB of chain bytes).
        if cache.len() == 64 {
            cache.pop_front();
        }
        cache.push_back(CachedPeer {
            roots,
            chain: chain.to_vec(),
            node: expected.clone(),
            key,
            until,
            checked: now,
        });
        Ok(key)
    }
    pub fn verify(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifiedPeer> {
        if &self.cluster != self.keys.cluster() {
            return Err(Error::Unauthorized);
        }
        self.key(chain, expected)?;
        Ok(VerifiedPeer {
            node: expected.clone(),
        })
    }
    pub fn verify_signed(
        &self,
        chain: &[Vec<u8>],
        expected: &NodeId,
        message: &[u8],
        signature: &[u8],
    ) -> Result<VerifiedPeer> {
        if &self.cluster != self.keys.cluster() {
            return Err(Error::Unauthorized);
        }
        let key = self.key(chain, expected)?;
        let signature = Signature::from_slice(signature).map_err(|_| Error::Unauthorized)?;
        key.verify_strict(message, &signature)
            .map_err(|_| Error::Unauthorized)?;
        Ok(VerifiedPeer {
            node: expected.clone(),
        })
    }
}

/// Conservative validity intersection includes all configured trust anchors.
pub(crate) fn validity<'a>(certificates: impl Iterator<Item = &'a Vec<u8>>) -> Result<(u64, u64)> {
    let mut start = 0;
    let mut end = u64::MAX;
    for der in certificates {
        let (_, cert) = parse_x509_certificate(der).map_err(|_| Error::Unauthorized)?;
        start = start.max(cert.validity().not_before.timestamp().max(0) as u64);
        end = end.min(
            u64::try_from(cert.validity().not_after.timestamp())
                .map_err(|_| Error::Unauthorized)?,
        );
    }
    Ok((start, end))
}
pub(crate) fn root_store(roots: &[Vec<u8>]) -> Result<RootCertStore> {
    if roots.is_empty() || roots.len() > 32 {
        return Err(Error::Unauthorized);
    }
    let mut store = RootCertStore::empty();
    for root in roots {
        if root.is_empty() || root.len() > 16384 {
            return Err(Error::Unauthorized);
        }
        let (rest, cert) = parse_x509_certificate(root).map_err(|_| Error::Unauthorized)?;
        let now = crate::runtime::environment::unix_time().as_secs();
        let now = i64::try_from(now).map_err(|_| Error::Unauthorized)?;
        let now =
            x509_parser::time::ASN1Time::from_timestamp(now).map_err(|_| Error::Unauthorized)?;
        if !rest.is_empty() || !cert.is_ca() || !cert.validity().is_valid_at(now) {
            return Err(Error::Unauthorized);
        }
        store
            .add(CertificateDer::from(root.clone()))
            .map_err(|_| Error::Unauthorized)?;
    }
    Ok(store)
}
pub(crate) fn spiffe(cluster: &ClusterId, node: &NodeId) -> Result<String> {
    if !canonical_uuid(&cluster.0) || !canonical_uuid(&node.0) {
        return Err(Error::Unauthorized);
    }
    Ok(format!("spiffe://{}/node/{}", cluster.0, node.0))
}
pub(crate) fn canonical_uuid(value: &str) -> bool {
    value.len() == 36
        && value.bytes().enumerate().all(|(i, b)| {
            if matches!(i, 8 | 13 | 18 | 23) {
                b == b'-'
            } else {
                b.is_ascii_digit() || (b'a'..=b'f').contains(&b)
            }
        })
}
pub(crate) fn verify_chain(
    roots: &[Vec<u8>],
    chain: &[Vec<u8>],
    cluster: &ClusterId,
    node: &NodeId,
) -> Result<VerifyingKey> {
    if chain.is_empty()
        || chain.len() > 8
        || chain.iter().any(|c| c.is_empty() || c.len() > 16384)
        || chain.iter().map(Vec::len).sum::<usize>() > 65536
    {
        return Err(Error::Unauthorized);
    }
    let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
        Arc::new(root_store(roots)?),
        Arc::new(rustls::crypto::ring::default_provider()),
    )
    .build()
    .map_err(|_| Error::Unauthorized)?;
    let leaf = CertificateDer::from(chain[0].as_slice());
    let intermediates: Vec<_> = chain[1..]
        .iter()
        .map(|c| CertificateDer::from(c.as_slice()))
        .collect();
    verifier
        .verify_client_cert(
            &leaf,
            &intermediates,
            crate::runtime::environment::unix_time(),
        )
        .map_err(|_| Error::Unauthorized)?;
    let (rest, cert) = parse_x509_certificate(&chain[0]).map_err(|_| Error::Unauthorized)?;
    if !rest.is_empty()
        || cert.is_ca()
        || cert.public_key().algorithm.algorithm.to_id_string() != "1.3.101.112"
        || cert.public_key().algorithm.parameters.is_some()
    {
        return Err(Error::Unauthorized);
    }
    let san = cert
        .subject_alternative_name()
        .map_err(|_| Error::Unauthorized)?
        .ok_or(Error::Unauthorized)?;
    let usage = cert
        .key_usage()
        .map_err(|_| Error::Unauthorized)?
        .ok_or(Error::Unauthorized)?;
    let extended = cert
        .extended_key_usage()
        .map_err(|_| Error::Unauthorized)?
        .ok_or(Error::Unauthorized)?;
    if !usage.value.digital_signature()
        || usage.value.key_cert_sign()
        || !extended.value.client_auth
    {
        return Err(Error::Unauthorized);
    }
    let expected = spiffe(cluster, node)?;
    let uris: Vec<_> = san
        .value
        .general_names
        .iter()
        .filter_map(|n| {
            if let GeneralName::URI(s) = n {
                Some(*s)
            } else {
                None
            }
        })
        .collect();
    if uris != [expected.as_str()] {
        return Err(Error::Unauthorized);
    }
    let key: &[u8; 32] = cert
        .public_key()
        .subject_public_key
        .data
        .as_ref()
        .try_into()
        .map_err(|_| Error::Unauthorized)?;
    VerifyingKey::from_bytes(key).map_err(|_| Error::Unauthorized)
}

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
pub use crate::control::wire::CacheKeyPurpose as KeyPurpose;
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
        if self.reference.purpose != purpose {
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
            || !canonical_uuid(&self.cluster.0)
            || !canonical_uuid(&self.node.0)
        {
            return Err(Error::InvalidConfiguration);
        }
        root_store(&bundle.peer_trust_roots)?;
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
            let generation = candidate
                .key
                .id
                .generation()
                .ok_or(Error::InvalidConfiguration)?;
            if generation > bundle.generation.0 {
                return Err(Error::InvalidConfiguration);
            }
            if !canonical_uuid(&candidate.key.cache.0) {
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
            let generation = candidate
                .key
                .id
                .generation()
                .ok_or(Error::InvalidConfiguration)?;
            // Removed IDs cannot reenter admission, even as prepared keys.
            if state.generation.is_some_and(|g| generation <= g.0)
                && !state.entries.iter().any(|e| e.reference == candidate.key)
            {
                return Err(Error::InvalidConfiguration);
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
        state.entries.retain_mut(|entry| {
            if let Some(key) = bundle.cache_keys.iter().find(|k| k.key == entry.reference) {
                entry.state = key.state;
                true
            } else {
                false
            }
        });
        for candidate in &bundle.cache_keys {
            if !state.entries.iter().any(|e| e.reference == candidate.key) {
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
    pub fn install_signing_identity(&self, identity: Arc<SigningIdentity>) -> Result<()> {
        if identity.node() != &self.node || identity.cluster() != &self.cluster {
            return Err(Error::Unauthorized);
        }
        let roots = self.peer_trust_roots()?;
        verify_chain(
            &roots,
            identity.certificate_chain(),
            &self.cluster,
            &self.node,
        )?;
        let (_, until) = validity(identity.certificate_chain().iter().chain(roots.iter()))?;
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
        verify_chain(
            &roots,
            identity.certificate_chain(),
            &self.cluster,
            &self.node,
        )?;
        let (_, until) = validity(identity.certificate_chain().iter().chain(roots.iter()))?;
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
            purpose,
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
mod certificate_tests;
#[cfg(test)]
pub(crate) mod keyring_tests;
#[cfg(test)]
pub(crate) mod tests;
