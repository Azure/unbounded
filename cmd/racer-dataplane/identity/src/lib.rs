//! Trust roots, certificate validation, and locally generated Ed25519 identities.
//! Immutable key leases outlive admission; their last completion owner zeroizes
//! the secret. Secret export is explicit and zeroizing.
#[cfg(test)]
use racer_control_wire::CacheKeyPurpose;
use racer_control_wire::{
    BundleGeneration, CacheId, CacheKeyRef, CacheKeyState, ClusterId, KeyId, NodeId, SCHEMA_VERSION,
};
mod environment {
    pub use uring_runtime::environment::*;
    pub fn unix_time() -> rustls::pki_types::UnixTime {
        rustls::pki_types::UnixTime::since_unix_epoch(
            wall_now()
                .duration_since(std::time::SystemTime::UNIX_EPOCH)
                .unwrap_or_default(),
        )
    }
}
mod error;
#[cfg(test)]
mod test_support;
pub use error::{Error, Result};

fn key_generation(id: KeyId) -> Option<u64> {
    (id.0[..4] == *b"RKG1")
        .then(|| u64::from_be_bytes(id.0[4..12].try_into().expect("generation bytes")))
        .filter(|generation| *generation != 0)
}
use racer_crypto::ed25519::{SigningKey, VerifyingKey};
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
use zeroize::Zeroizing;

// Private validation staging owns zeroizing transfers on every rejection exit.
#[derive(Clone)]
struct CacheEncryptionKey {
    key: CacheKeyRef,
    state: CacheKeyState,
    material: Zeroizing<[u8; 32]>,
}
#[derive(Clone)]
struct KeyringBundle {
    schema_version: u32,
    cluster: ClusterId,
    generation: BundleGeneration,
    peer_trust_roots: Vec<Vec<u8>>,
    cache_keys: Vec<CacheEncryptionKey>,
}
impl From<racer_control_wire::KeyringBundle> for KeyringBundle {
    fn from(bundle: racer_control_wire::KeyringBundle) -> Self {
        Self {
            schema_version: bundle.schema_version,
            cluster: bundle.cluster,
            generation: bundle.generation,
            peer_trust_roots: bundle.peer_trust_roots,
            cache_keys: bundle
                .cache_keys
                .into_iter()
                .map(|record| {
                    let (key, state, material) = record.into_installation();
                    CacheEncryptionKey {
                        key,
                        state,
                        material,
                    }
                })
                .collect(),
        }
    }
}

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
        environment::fill_random(&mut *seed).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            key: SigningKey::from_seed(&seed),
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
        self.key.to_pkcs8_der().map_err(|_| Error::Unavailable)
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
        let now = environment::unix_time().as_secs();
        if now < self.valid_from || now >= self.expires {
            return Err(Error::Unauthorized);
        }
        Ok(self.key.sign(message).to_vec())
    }
    pub fn export_pkcs8_for_persistence(&self) -> Result<Zeroizing<Vec<u8>>> {
        self.key.to_pkcs8_der().map_err(|_| Error::Unavailable)
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
/// ```compile_fail
/// fn send<T: Send>() {}
/// send::<racer_identity::Certificates>();
/// ```
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
/// Only successful certificate validation can construct a verified peer.
/// ```compile_fail
/// let peer = racer_identity::VerifiedPeer { node: racer_control_wire::NodeId("forged".into()) };
/// ```
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
        let now = environment::unix_time().as_secs();
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
        key.verify_strict(message, signature)
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
        let now = environment::unix_time().as_secs();
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
pub fn canonical_uuid(value: &str) -> bool {
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
        .verify_client_cert(&leaf, &intermediates, environment::unix_time())
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
    identity_trust: Option<IdentityTrust>,
}
struct IdentityTrust {
    roots: Arc<Vec<Vec<u8>>>,
    checked: u64,
    until: u64,
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
/// An immutable secret owner, not a raw-key export capability.
/// ```compile_fail
/// fn raw(key: &racer_identity::KeyLease) {
///     key.material(racer_identity::KeyPurpose::Page);
/// }
/// ```
/// ```compile_fail
/// fn diagnostic(key: racer_identity::KeyLease) { println!("{key:?}"); }
/// ```
pub struct KeyLease {
    reference: CacheKeyRef,
    secret: Arc<Secret>,
}
pub use racer_control_wire::CacheKeyPurpose as KeyPurpose;
impl KeyLease {
    /// Validate purpose before application admission and integrity checks.
    pub fn require_purpose(&self, purpose: KeyPurpose) -> Result<()> {
        self.material(purpose).map(|_| ())
    }
    fn bound_material(&self, cache: &CacheId, id: KeyId, purpose: KeyPurpose) -> Result<&[u8; 32]> {
        let material = self.material(purpose)?;
        if self.cache() != cache || self.id() != id {
            return Err(Error::MissingKey);
        }
        Ok(material)
    }
    /// Seal a page without copying input or consulting current epoch admission.
    /// Caller supplies canonical AAD and a fresh nonce for this key.
    pub fn seal_page(
        &self,
        cache: &CacheId,
        nonce: &[u8; 24],
        aad: &[u8],
        input: &[u8],
        out: &mut [u8],
    ) -> Result<()> {
        racer_crypto::aead::seal(
            self.bound_material(cache, self.id(), KeyPurpose::Page)?,
            nonce,
            aad,
            input,
            out,
        )
        .map_err(|_| Error::CorruptRecord)
    }
    /// Authenticate before writing caller output; an error leaves output untouched.
    // Keep borrowed buffer and expected-identity arguments explicit at this boundary.
    #[allow(clippy::too_many_arguments)]
    pub fn open_page(
        &self,
        cache: &CacheId,
        id: KeyId,
        nonce: &[u8; 24],
        aad: &[u8],
        input: &[u8],
        out: &mut [u8],
    ) -> Result<()> {
        racer_crypto::aead::open(
            self.bound_material(cache, id, KeyPurpose::Page)?,
            nonce,
            aad,
            input,
            out,
        )
        .map_err(|_| Error::CorruptRecord)
    }
    /// Seal credentials using only a credential epoch. Caller owns nonce and AAD.
    pub fn seal_credentials(
        &self,
        cache: &CacheId,
        nonce: &[u8; 24],
        aad: &[u8],
        input: &[u8],
        out: &mut [u8],
    ) -> Result<()> {
        racer_crypto::aead::seal(
            self.bound_material(cache, self.id(), KeyPurpose::OriginCredentials)?,
            nonce,
            aad,
            input,
            out,
        )
        .map_err(|_| Error::Unauthorized)
    }
    /// Authenticate credentials into borrowed output without staging plaintext.
    // Keep borrowed buffer and expected-identity arguments explicit at this boundary.
    #[allow(clippy::too_many_arguments)]
    pub fn open_credentials(
        &self,
        cache: &CacheId,
        id: KeyId,
        nonce: &[u8; 24],
        aad: &[u8],
        input: &[u8],
        out: &mut [u8],
    ) -> Result<()> {
        racer_crypto::aead::open(
            self.bound_material(cache, id, KeyPurpose::OriginCredentials)?,
            nonce,
            aad,
            input,
            out,
        )
        .map_err(|_| Error::Unauthorized)
    }
    /// Derive and use the request-only MAC key internally; never export it.
    pub fn request_mac(&self, cache: &CacheId, message: &[u8], out: &mut [u8; 32]) -> Result<()> {
        let material = self.bound_material(cache, self.id(), KeyPurpose::OriginCredentials)?;
        let mut domain = b"racer/request-mac/key/v1\0".to_vec();
        let length = u32::try_from(cache.0.len()).map_err(|_| Error::InvalidRequest)?;
        domain.extend_from_slice(&length.to_be_bytes());
        domain.extend_from_slice(cache.0.as_bytes());
        domain.extend_from_slice(&self.id().0);
        let derived = Zeroizing::new(racer_crypto::hmac_sha256(material, &domain));
        *out = racer_crypto::hmac_sha256(&derived, message);
        Ok(())
    }
    pub fn verify_request_mac(
        &self,
        cache: &CacheId,
        id: KeyId,
        message: &[u8],
        tag: &[u8],
    ) -> Result<()> {
        self.bound_material(cache, id, KeyPurpose::OriginCredentials)?;
        let mut expected = [0; 32];
        self.request_mac(cache, message, &mut expected)?;
        if !racer_crypto::ct_eq(&expected, tag) {
            return Err(Error::Unauthorized);
        }
        Ok(())
    }
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
    fn material(&self, purpose: KeyPurpose) -> Result<&[u8; 32]> {
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
    pub fn install(&self, bundle: racer_control_wire::KeyringBundle) -> Result<BundleGeneration> {
        // Bound private staging before allocating it, including direct callers
        // that do not pass through the wire decoder's byte limit.
        if bundle.cache_keys.len() > 4096 {
            return Err(Error::InvalidConfiguration);
        }
        self.install_inner(&bundle.into())
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
            hash.update(key.material.as_slice());
        }
        let fingerprint: [u8; 32] = hash.finalize().into();
        // Validate the downloaded epoch before acquiring the shared publication
        // lock. Existing immutable secret leases remain usable during validation.
        for (i, candidate) in bundle.cache_keys.iter().enumerate() {
            let generation = key_generation(candidate.key.id).ok_or(Error::InvalidConfiguration)?;
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
            let generation = key_generation(candidate.key.id).ok_or(Error::InvalidConfiguration)?;
            // Removed IDs cannot reenter admission, even as prepared keys.
            if state.generation.is_some_and(|g| generation <= g.0)
                && !state.entries.iter().any(|e| e.reference == candidate.key)
            {
                return Err(Error::InvalidConfiguration);
            }
            if let Some(old) = state.entries.iter().find(|e| e.reference == candidate.key)
                && *old.secret.0 != *candidate.material
            {
                return Err(Error::InvalidConfiguration);
            }
            if state
                .entries
                .iter()
                .any(|e| e.reference != candidate.key && *e.secret.0 == *candidate.material)
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
                    secret: Arc::new(Secret(candidate.material.clone())),
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
        let checked = environment::unix_time().as_secs();
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if !Arc::ptr_eq(&roots, &state.roots) {
            return Err(Error::Unavailable);
        }
        state.identity_trust = Some(IdentityTrust {
            roots,
            checked,
            until,
        });
        state.identity = Some(identity);
        Ok(())
    }
    pub fn signing_identity(&self) -> Result<Arc<SigningIdentity>> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        let identity = state.identity.as_ref().ok_or(Error::MissingKey)?;
        if identity.node() != &self.node || identity.cluster() != &self.cluster {
            return Err(Error::Unauthorized);
        }
        let now = environment::unix_time().as_secs();
        let identity = identity.clone();
        if state.identity_trust.as_ref().is_some_and(|trust| {
            Arc::ptr_eq(&trust.roots, &state.roots) && now >= trust.checked && now < trust.until
        }) {
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
        state.identity_trust = Some(IdentityTrust {
            roots,
            checked: now,
            until,
        });
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
mod certificate_tests {
    use super::tests::{CLUSTER, NODE, issued, issued_with};
    use super::*;
    #[test]
    fn cache_requires_exact_chain_current_trust_time_and_fresh_signature() {
        let clock = environment::SimulationClock::new(93);
        let _environment = clock.environment(0).enter();
        let (pending, chain, roots) = issued();
        let keys = Rc::new(Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::default()),
        ));
        let bundle = |generation, roots| racer_control_wire::KeyringBundle {
            schema_version: 1,
            cluster: ClusterId(CLUSTER.into()),
            generation: BundleGeneration(generation),
            peer_trust_roots: roots,
            cache_keys: vec![],
        };
        keys.install(bundle(1, roots.clone())).unwrap();
        let identity = pending
            .accept(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                chain.clone(),
                &roots,
            )
            .unwrap();
        let certs = Certificates::new(ClusterId(CLUSTER.into()), keys.clone());
        let node = NodeId(NODE.into());
        let signature = identity.sign(b"message").unwrap();
        for _ in 0..2 {
            certs
                .verify_signed(&chain, &node, b"message", &signature)
                .unwrap();
        }
        assert!(
            certs
                .verify_signed(&chain, &node, b"tampered", &signature)
                .is_err()
        );
        let mut changed = chain.clone();
        changed[0].push(0);
        assert!(certs.verify(&changed, &node).is_err());
        let original = environment::wall_now();
        let (_, until) = validity(chain.iter().chain(roots.iter())).unwrap();
        clock.set_wall_time(
            std::time::SystemTime::UNIX_EPOCH + std::time::Duration::from_secs(until + 1),
        );
        assert!(certs.verify(&chain, &node).is_err());
        clock.set_wall_time(original);
        certs.verify(&chain, &node).unwrap();
        let (_, _, replacement) = issued();
        keys.install(bundle(2, replacement)).unwrap();
        assert!(certs.verify(&chain, &node).is_err());
    }
    #[test]
    fn validates_chain_node_and_strict_signature() {
        let (pending, chain, roots) = issued();
        let cluster = ClusterId(CLUSTER.into());
        let node = NodeId(NODE.into());
        let key = verify_chain(&roots, &chain, &cluster, &node).unwrap();
        let identity = pending
            .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
            .unwrap();
        let signature = identity.sign(b"message").unwrap();
        assert!(key.verify_strict(b"message", &signature).is_ok());
        assert!(key.verify_strict(b"changed", &signature).is_err());
        assert!(key.verify_strict(b"message", &signature[..63]).is_err());
        let mut oversized = signature.clone();
        oversized.push(0);
        assert!(key.verify_strict(b"message", &oversized).is_err());
        assert!(verify_chain(&roots, &chain, &cluster, &NodeId("other".into())).is_err());
        assert!(verify_chain(&roots, &chain, &ClusterId("other".into()), &node).is_err());
        let (_, _, foreign_roots) = issued();
        assert!(verify_chain(&foreign_roots, &chain, &cluster, &node).is_err());
        let mut bad = chain.clone();
        bad[0].push(0);
        assert!(verify_chain(&roots, &bad, &cluster, &node).is_err());
        assert!(verify_chain(&roots, &vec![chain[0].clone(); 9], &cluster, &node).is_err());
    }
    #[test]
    fn rejects_missing_usage_ca_expiration_and_ambiguous_identity() {
        for case in 0..8 {
            let (_, chain, roots) = issued_with(|params| match case {
                0 => params.key_usages.clear(),
                1 => params.key_usages = vec![rcgen::KeyUsagePurpose::KeyEncipherment],
                2 => params.extended_key_usages.clear(),
                3 => params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ServerAuth],
                4 => params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained),
                5 => {
                    params.not_before = rcgen::date_time_ymd(2000, 1, 1);
                    params.not_after = rcgen::date_time_ymd(2001, 1, 1);
                }
                6 => params
                    .subject_alt_names
                    .push(params.subject_alt_names[0].clone()),
                _ => {
                    params.subject_alt_names = vec![rcgen::SanType::URI(
                        format!("spiffe://{CLUSTER}/node/{NODE}/extra")
                            .try_into()
                            .unwrap(),
                    )]
                }
            });
            assert!(
                verify_chain(
                    &roots,
                    &chain,
                    &ClusterId(CLUSTER.into()),
                    &NodeId(NODE.into())
                )
                .is_err(),
                "case {case}"
            );
        }
    }
    #[test]
    fn canonical_identity_rejects_aliases() {
        assert!(canonical_uuid(CLUSTER));
        for value in [
            "abc",
            "AAAAAAAA-1111-4111-8111-111111111111",
            "11111111111141118111111111111111",
            "11111111-1111-4111-8111-11111111111/",
        ] {
            assert!(!canonical_uuid(value));
        }
    }
}
#[cfg(test)]
pub(crate) mod keyring_tests;
#[cfg(test)]
mod purpose_operations {
    use super::*;

    #[test]
    fn borrowed_outputs_enforce_purpose_cache_id_and_retained_epoch() {
        let keys = keyring_tests::keys();
        let cache = CacheId(tests::CACHE.into());
        let page = keys.active(&cache, KeyPurpose::Page).unwrap();
        let credentials = keys.active(&cache, KeyPurpose::OriginCredentials).unwrap();
        let mut sealed = [0; 19];
        page.seal_page(&cache, &[1; 24], b"aad", b"abc", &mut sealed)
            .unwrap();
        let mut out = [42; 3];
        page.open_page(&cache, page.id(), &[1; 24], b"aad", &sealed, &mut out)
            .unwrap();
        assert_eq!(&out, b"abc");
        out.fill(42);
        assert_eq!(
            page.open_page(
                &cache,
                credentials.id(),
                &[1; 24],
                b"aad",
                &sealed,
                &mut out
            ),
            Err(Error::MissingKey)
        );
        assert_eq!(
            page.open_page(
                &CacheId("wrong".into()),
                page.id(),
                &[1; 24],
                b"aad",
                &sealed,
                &mut out
            ),
            Err(Error::MissingKey)
        );
        assert_eq!(
            page.open_page(&cache, page.id(), &[1; 24], b"bad", &sealed, &mut out),
            Err(Error::CorruptRecord)
        );
        assert_eq!(out, [42; 3]);
        assert_eq!(
            credentials.seal_page(&cache, &[1; 24], b"aad", b"abc", &mut sealed),
            Err(Error::MissingKey)
        );
        assert_eq!(
            page.seal_credentials(&cache, &[1; 24], b"aad", b"abc", &mut sealed),
            Err(Error::MissingKey)
        );
        credentials
            .seal_credentials(&cache, &[2; 24], b"aad", b"abc", &mut sealed)
            .unwrap();
        credentials
            .open_credentials(
                &cache,
                credentials.id(),
                &[2; 24],
                b"aad",
                &sealed,
                &mut out,
            )
            .unwrap();
        assert_eq!(&out, b"abc");
        assert_eq!(
            credentials.open_credentials(
                &cache,
                credentials.id(),
                &[2; 24],
                b"bad",
                &sealed,
                &mut out
            ),
            Err(Error::Unauthorized)
        );
        let roots = (*keys.peer_trust_roots().unwrap()).clone();
        keys.install_inner(&keyring_tests::rotation_bundle(2, roots))
            .unwrap();
        assert!(
            keys.lease(Some(&cache), page.id(), KeyPurpose::Page)
                .is_err()
        );
        page.seal_page(&cache, &[3; 24], b"aad", b"abc", &mut sealed)
            .unwrap();
        page.open_page(&cache, page.id(), &[3; 24], b"aad", &sealed, &mut out)
            .unwrap();
        assert_eq!(&out, b"abc");
    }

    #[test]
    fn request_mac_derives_internally_and_rejects_wrong_purpose_or_message() {
        let keys = keyring_tests::keys();
        let cache = CacheId(tests::CACHE.into());
        let page = keys.active(&cache, KeyPurpose::Page).unwrap();
        let key = keys.active(&cache, KeyPurpose::OriginCredentials).unwrap();
        let mut tag = [0; 32];
        assert_eq!(
            page.request_mac(&cache, b"message", &mut tag),
            Err(Error::MissingKey)
        );
        key.request_mac(&cache, b"message", &mut tag).unwrap();
        let mut domain = b"racer/request-mac/key/v1\0".to_vec();
        domain.extend_from_slice(&(cache.0.len() as u32).to_be_bytes());
        domain.extend_from_slice(cache.0.as_bytes());
        domain.extend_from_slice(&key.id().0);
        let derived = Zeroizing::new(racer_crypto::hmac_sha256(&[8; 32], &domain));
        assert_eq!(tag, racer_crypto::hmac_sha256(&derived, b"message"));
        key.verify_request_mac(&cache, key.id(), b"message", &tag)
            .unwrap();
        assert_eq!(
            key.verify_request_mac(&cache, key.id(), b"changed", &tag),
            Err(Error::Unauthorized)
        );
        assert_eq!(
            key.verify_request_mac(&cache, page.id(), b"message", &tag),
            Err(Error::MissingKey)
        );
    }
}
#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
    pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";
    pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";
    pub(crate) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
        issued_with(|_| {})
    }
    pub(crate) fn issued_with(
        customize: impl FnOnce(&mut rcgen::CertificateParams),
    ) -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
        let (ca, ca_key) = crate::test_support::ca();
        let (pending, chain) = crate::test_support::issue(
            &ca,
            &ca_key,
            &ClusterId(CLUSTER.into()),
            &NodeId(NODE.into()),
            customize,
        );
        (pending, chain, vec![ca.der().to_vec()])
    }
    #[test]
    fn identity_recovery_csr_tls_and_key_pairing() {
        let (pending, chain, roots) = issued();
        assert!(!pending.csr_der().unwrap().is_empty());
        let bytes = pending.export_pkcs8_for_persistence().unwrap();
        let identity = pending
            .accept(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                chain.clone(),
                &roots,
            )
            .unwrap();
        assert!(identity.tls_certified_key().is_ok());
        let recovered = SigningIdentity::from_pkcs8(
            identity.cluster.clone(),
            identity.node.clone(),
            &bytes,
            chain.clone(),
            &roots,
        )
        .unwrap();
        assert_eq!(
            identity.sign(b"exact message").unwrap(),
            recovered.sign(b"exact message").unwrap()
        );
        assert!(
            PendingIdentity::generate()
                .unwrap()
                .accept(
                    identity.cluster.clone(),
                    identity.node.clone(),
                    chain.clone(),
                    &roots
                )
                .is_err()
        );
        assert!(
            SigningIdentity::from_pkcs8(
                identity.cluster.clone(),
                NodeId("other".into()),
                &bytes,
                chain,
                &roots
            )
            .is_err()
        );
    }
}
