//! Trust roots, certificate validation, and locally generated Ed25519 identities.
//! Immutable key leases outlive admission; their last completion owner zeroizes
//! the secret. Secret export is explicit and zeroizing.
//!
//! # Ownership and admission
//!
//! Wire bundles move into zeroizing staging before validation. Installation
//! publishes a complete epoch atomically; removed epochs remain usable only by
//! previously admitted owners. Leases expose fixed-purpose page/credential AEAD
//! and request MAC operations, never raw secrets. Inputs are borrowed and outputs
//! caller-owned. Purpose, cache, and key ID checks do not consult current admission.
//!
//! Callers own nonce uniqueness, canonical AAD/messages, quotas, cancellation,
//! CRC checks, telemetry, and completion ownership. PKCS8 persistence export is
//! explicit and zeroizing. Errors contain no input data. Certificate caches are
//! worker-local and non-Send; verified peers cannot be constructed by callers.
//! This module has no dependency on the dataplane worker or service graph.
//!
//! # Validation and fixtures
//!
//! Certificate and epoch state-space tests live here; cross-component page-engine
//! and decode/BundleInstaller scenarios live in the application's integration tests.
//! Run these gates from `cmd/racer-dataplane`:
//!
//! ```sh
//! timeout --signal=TERM --kill-after=10s 300s cargo test --locked -p racer-crypto
//! timeout --signal=TERM --kill-after=10s 300s cargo clippy --locked -p racer-crypto --all-targets --all-features --no-deps -- -D warnings
//! timeout --signal=TERM --kill-after=10s 300s cargo test --locked -p racer-dataplane --test identity_integration
//! ```
//!
//! The opt-in `test-util` feature exposes Ed25519 CA and node-certificate fixtures
//! with customizable parameters. `test_util::issue_pending` retains an existing
//! key without consuming additional entropy. Production does not enable it.
use crate::{SigningKey, VerifyingKey};
#[cfg(test)]
use racer_control_wire::CacheKeyPurpose;
pub use racer_control_wire::valid_uuid as canonical_uuid;
use racer_control_wire::{
    BundleGeneration, CacheId, CacheKeyRef, CacheKeyState, ClusterId, KeyId, NodeId, SCHEMA_VERSION,
};
use rustls::{
    RootCertStore,
    pki_types::{CertificateDer, PrivatePkcs8KeyDer},
};
use sha2::{Digest, Sha256};
use std::{
    cell::RefCell,
    collections::VecDeque,
    rc::Rc,
    sync::{Arc, Mutex},
};
use uring_runtime::environment;
use x509_parser::{extensions::GeneralName, parse_x509_certificate};
use zeroize::Zeroizing;

/// Payload-free component failures, explicitly mapped by the application.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The requested operation has invalid input.
    InvalidRequest,
    /// The proposed key configuration or epoch transition is invalid.
    InvalidConfiguration,
    /// Identity, trust, or authentication validation failed.
    Unauthorized,
    /// A required resource or coherent publication is temporarily unavailable.
    Unavailable,
    /// No admitted key matches the requested cache, ID, and purpose.
    MissingKey,
    /// A page record or its cryptographic encoding is invalid.
    CorruptRecord,
}

/// A component result that never includes secrets in its error payload.
pub type Result<T> = std::result::Result<T, Error>;

/// A locally owned Ed25519 key awaiting an authenticated certificate chain.
pub struct PendingIdentity {
    key: SigningKey,
}

/// An accepted node certificate paired with its locally held signing key.
pub struct SigningIdentity {
    cluster: ClusterId,

    node: NodeId,

    key: SigningKey,

    chain: Vec<Vec<u8>>,

    expires: u64,

    valid_from: u64,
}

/// Shared publication lock for complete epochs and their signing identity.
#[derive(Default)]
pub struct KeyEpochs {
    state: Mutex<State>,
}

/// A cluster/node view of shared epochs, responsible for admission and rotation.
pub struct Keyring {
    cluster: ClusterId,

    node: NodeId,

    epochs: Arc<KeyEpochs>,
}

/// An immutable secret owner, not a raw-key export capability.
/// ```compile_fail
/// fn raw(key: &racer_crypto::identity::KeyLease) {
///     key.material(racer_crypto::identity::KeyPurpose::Page);
/// }
/// ```
/// ```compile_fail
/// fn diagnostic(key: racer_crypto::identity::KeyLease) { println!("{key:?}"); }
/// ```
pub struct KeyLease {
    reference: CacheKeyRef,

    secret: Arc<Secret>,
}

pub use racer_control_wire::CacheKeyPurpose as KeyPurpose;

/// Worker-local verifier caching exact chains against the current trust roots.
/// ```compile_fail
/// fn send<T: Send>() {}
/// send::<racer_crypto::identity::Certificates>();
/// ```
pub struct Certificates {
    cluster: ClusterId,

    keys: Rc<Keyring>,

    cache: RefCell<VecDeque<CachedPeer>>,
}

/// Only successful certificate validation can construct a verified peer.
/// ```compile_fail
/// let peer = racer_crypto::identity::VerifiedPeer { node: racer_control_wire::NodeId("forged".into()) };
/// ```
pub struct VerifiedPeer {
    node: NodeId,
}

/// Private validation staging owns zeroizing transfers on every rejection exit.
#[derive(Clone)]
struct CacheEncryptionKey {
    key: CacheKeyRef,

    state: CacheKeyState,

    material: Zeroizing<[u8; 32]>,
}
/// An owned wire transfer with secrets that wipe on every exit path.
#[derive(Clone)]
struct KeyringBundle {
    schema_version: u32,

    cluster: ClusterId,

    generation: BundleGeneration,

    peer_trust_roots: Vec<Vec<u8>>,

    cache_keys: Vec<CacheEncryptionKey>,
}
impl From<racer_control_wire::KeyringBundle> for KeyringBundle {
    /// Move transferred material into private, zeroizing validation staging.
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

impl PendingIdentity {
    /// Generate a key from the scoped runtime entropy source.
    pub fn generate() -> Result<Self> {
        let mut seed = Zeroizing::new([0u8; 32]);
        environment::fill_random(&mut *seed).map_err(|_| Error::Unavailable)?;
        Ok(Self {
            key: SigningKey::from_seed(&seed),
        })
    }
    /// Recover a bounded Ed25519 PKCS8 key without accepting a certificate.
    pub fn recover(pkcs8: &[u8]) -> Result<Self> {
        if pkcs8.len() > 4096 {
            return Err(Error::InvalidRequest);
        }
        Ok(Self {
            key: SigningKey::from_pkcs8_der(pkcs8).map_err(|_| Error::Unauthorized)?,
        })
    }
    /// Explicitly export a zeroizing PKCS8 copy for durable storage.
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
    /// Accept only a trusted node chain whose leaf matches this private key.
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
    /// Return the conservative expiry of the accepted chain and all roots.
    pub fn expires_at_seconds(&self) -> u64 {
        self.expires
    }
    /// Recover a persisted key and validate its certificate and node binding.
    pub fn from_pkcs8(
        cluster: ClusterId,
        node: NodeId,
        pkcs8: &[u8],
        chain: Vec<Vec<u8>>,
        roots: &[Vec<u8>],
    ) -> Result<Self> {
        PendingIdentity::recover(pkcs8)?.accept(cluster, node, chain, roots)
    }
    /// Return the cluster authenticated by the certificate.
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    /// Return the node authenticated by the certificate.
    pub fn node(&self) -> &NodeId {
        &self.node
    }
    /// Borrow the exact accepted certificate chain, leaf first.
    pub fn certificate_chain(&self) -> &[Vec<u8>] {
        &self.chain
    }
    /// Sign the exact message only within the accepted validity interval.
    pub fn sign(&self, message: &[u8]) -> Result<Vec<u8>> {
        let now = unix_time().as_secs();
        if now < self.valid_from || now >= self.expires {
            return Err(Error::Unauthorized);
        }
        Ok(self.key.sign(message).to_vec())
    }
}

/// One exact chain validated against a specific immutable root snapshot.
struct CachedPeer {
    roots: Arc<Vec<Vec<u8>>>,

    chain: Vec<Vec<u8>>,

    node: NodeId,

    key: VerifyingKey,

    until: u64,

    checked: u64,
}
impl VerifiedPeer {
    /// Return the authenticated peer node.
    pub fn node(&self) -> &NodeId {
        &self.node
    }
}
impl Certificates {
    /// Create a worker-local verifier using current keyring trust roots.
    pub fn new(cluster: ClusterId, keys: Rc<Keyring>) -> Self {
        Self {
            cluster,
            keys,
            cache: RefCell::new(VecDeque::new()),
        }
    }
    /// Reuse exact-chain validation only while roots and time remain valid.
    fn key(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifyingKey> {
        let roots = self.keys.peer_trust_roots()?;
        let now = unix_time().as_secs();
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
    /// Exercise certificate acceptance without a message in component tests.
    #[cfg(test)]
    fn verify(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifiedPeer> {
        if &self.cluster != self.keys.cluster() {
            return Err(Error::Unauthorized);
        }
        self.key(chain, expected)?;
        Ok(VerifiedPeer {
            node: expected.clone(),
        })
    }
    /// Authenticate both the expected node and its signature over this message.
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

/// Empty or coherent publication, with an independently installable identity.
#[derive(Default)]
struct State {
    published: Option<PublishedBundle>,

    identity: Option<InstalledIdentity>,
}
/// A complete publication; an uninstalled keyring has none of these fields.
struct PublishedBundle {
    cluster: ClusterId,

    generation: BundleGeneration,

    fingerprint: [u8; 32],

    roots: Arc<Vec<Vec<u8>>>,

    entries: Vec<Entry>,
}
/// An accepted signing identity and the trust snapshot that validated it.
struct InstalledIdentity {
    identity: Arc<SigningIdentity>,

    roots: Arc<Vec<Vec<u8>>>,

    checked: u64,

    until: u64,
}
/// Validated, zeroizing input ready for a locked generation transition.
struct ValidatedBundle<'a> {
    bundle: &'a KeyringBundle,

    fingerprint: [u8; 32],
}
/// Current admission metadata sharing immutable material with outstanding leases.
struct Entry {
    reference: CacheKeyRef,

    state: CacheKeyState,

    secret: Arc<Secret>,
}
/// Wipes key material when its last admission or completion owner releases it.
struct Secret(Zeroizing<[u8; 32]>);
impl KeyLease {
    /// Validate purpose before application admission and integrity checks.
    pub fn require_purpose(&self, purpose: KeyPurpose) -> Result<()> {
        self.material(purpose).map(|_| ())
    }
    /// Enforce the immutable cache, key ID, and purpose before using the secret.
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
        crate::seal(
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
        crate::open(
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
        crate::seal(
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
        crate::open(
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
        let derived = Zeroizing::new(crate::hmac_sha256(material, &domain));
        *out = crate::hmac_sha256(&derived, message);
        Ok(())
    }
    /// Authenticate a request MAC in constant time after checking its binding.
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
        if !crate::ct_eq(&expected, tag) {
            return Err(Error::Unauthorized);
        }
        Ok(())
    }
    /// Return the immutable generation-bound key ID.
    pub fn id(&self) -> KeyId {
        self.reference.id
    }
    /// Return the cache authorized to use this secret.
    pub fn cache(&self) -> &CacheId {
        &self.reference.cache
    }
    /// Exact immutable epoch identity, independent of current admission.
    pub fn reference(&self) -> &CacheKeyRef {
        &self.reference
    }
    /// Borrow private material only for the lease's fixed purpose.
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
            .published
            .as_ref()
            .map(|bundle| bundle.generation.0))
    }
    /// Create a node-bound view without implicitly installing any keys.
    pub fn new(cluster: ClusterId, node: NodeId, epochs: Arc<KeyEpochs>) -> Self {
        Self {
            cluster,
            node,
            epochs,
        }
    }
    /// Return the cluster this view admits.
    pub fn cluster(&self) -> &ClusterId {
        &self.cluster
    }
    /// Return the node this view admits.
    pub fn node(&self) -> &NodeId {
        &self.node
    }
    /// Lease the current trust snapshot, preserving pointer identity on replay.
    pub fn peer_trust_roots(&self) -> Result<Arc<Vec<Vec<u8>>>> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        let published = state.published.as_ref().ok_or(Error::MissingKey)?;
        if published.cluster != self.cluster {
            return Err(Error::MissingKey);
        }
        Ok(published.roots.clone())
    }
    /// Validate a wire transfer and atomically publish a complete key epoch.
    pub fn install(&self, bundle: racer_control_wire::KeyringBundle) -> Result<BundleGeneration> {
        // Bound private staging before allocating it, including direct callers
        // that do not pass through the wire decoder's byte limit.
        if bundle.cache_keys.len() > 4096 {
            return Err(Error::InvalidConfiguration);
        }
        self.install_inner(&bundle.into())
    }
    /// Prepare outside the lock, then validate the transition and publish once.
    fn install_inner(&self, bundle: &KeyringBundle) -> Result<BundleGeneration> {
        let validated = ValidatedBundle::prepare(bundle, &self.cluster, &self.node)?;
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        let published = validated.transition(state.published.as_ref())?;
        if let Some(published) = published {
            state.published = Some(published);
        }
        Ok(bundle.generation)
    }
}

impl<'a> ValidatedBundle<'a> {
    /// Validate bounded input and fingerprint its original, order-sensitive form.
    fn prepare(bundle: &'a KeyringBundle, cluster: &ClusterId, node: &NodeId) -> Result<Self> {
        if bundle.schema_version != SCHEMA_VERSION
            || &bundle.cluster != cluster
            || bundle.cache_keys.len() > 4096
            || bundle.generation.0 == 0
            || !canonical_uuid(&cluster.0)
            || !canonical_uuid(&node.0)
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
        Ok(Self {
            bundle,
            fingerprint,
        })
    }

    /// Check the current epoch and construct a replacement without mutating it.
    /// Exact replay returns no replacement, preserving all shared owner identities.
    fn transition(self, current: Option<&PublishedBundle>) -> Result<Option<PublishedBundle>> {
        let Self {
            bundle,
            fingerprint,
        } = self;
        if let Some(current) = current {
            if current.cluster != bundle.cluster || bundle.generation < current.generation {
                return Err(Error::InvalidConfiguration);
            }
            if current.generation == bundle.generation {
                return if current.fingerprint == fingerprint {
                    Ok(None)
                } else {
                    Err(Error::InvalidConfiguration)
                };
            }
            for candidate in &bundle.cache_keys {
                let generation = candidate
                    .key
                    .id
                    .generation()
                    .ok_or(Error::InvalidConfiguration)?;
                // Removed IDs cannot reenter admission, even as prepared keys.
                if generation <= current.generation.0
                    && !current.entries.iter().any(|e| e.reference == candidate.key)
                {
                    return Err(Error::InvalidConfiguration);
                }
                if let Some(old) = current
                    .entries
                    .iter()
                    .find(|e| e.reference == candidate.key)
                    && *old.secret.0 != *candidate.material
                {
                    return Err(Error::InvalidConfiguration);
                }
                if current
                    .entries
                    .iter()
                    .any(|e| e.reference != candidate.key && *e.secret.0 == *candidate.material)
                {
                    return Err(Error::InvalidConfiguration);
                }
            }
        }
        // Removed entries need no tombstone or polling state. Accepted jobs own
        // their Arc independently, including jobs whose caller has timed out.
        let mut entries: Vec<_> = bundle
            .cache_keys
            .iter()
            .map(|candidate| {
                let retained = current.and_then(|current| {
                    current
                        .entries
                        .iter()
                        .find(|e| e.reference == candidate.key)
                });
                Entry {
                    reference: candidate.key.clone(),
                    state: candidate.state,
                    secret: retained.map_or_else(
                        || Arc::new(Secret(candidate.material.clone())),
                        |entry| entry.secret.clone(),
                    ),
                }
            })
            .collect();
        entries.sort_by(|a, b| {
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
        let roots = current
            .filter(|current| *current.roots == bundle.peer_trust_roots)
            .map_or_else(
                || Arc::new(bundle.peer_trust_roots.clone()),
                |current| current.roots.clone(),
            );
        Ok(Some(PublishedBundle {
            cluster: bundle.cluster.clone(),
            generation: bundle.generation,
            fingerprint,
            roots,
            entries,
        }))
    }
}

impl Keyring {
    /// Accept a matching signing identity against roots unchanged by validation.
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
        let checked = unix_time().as_secs();
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if !state
            .published
            .as_ref()
            .is_some_and(|bundle| Arc::ptr_eq(&roots, &bundle.roots))
        {
            return Err(Error::Unavailable);
        }
        state.identity = Some(InstalledIdentity {
            identity,
            roots,
            checked,
            until,
        });
        Ok(())
    }
    /// Lease the identity after checking current trust and clock continuity.
    pub fn signing_identity(&self) -> Result<Arc<SigningIdentity>> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        let installed = state.identity.as_ref().ok_or(Error::MissingKey)?;
        let identity = &installed.identity;
        if identity.node() != &self.node || identity.cluster() != &self.cluster {
            return Err(Error::Unauthorized);
        }
        let now = unix_time().as_secs();
        let identity = identity.clone();
        let published = state.published.as_ref().ok_or(Error::MissingKey)?;
        if Arc::ptr_eq(&installed.roots, &published.roots)
            && now >= installed.checked
            && now < installed.until
        {
            return Ok(identity);
        }
        let roots = published.roots.clone();
        drop(state);
        verify_chain(
            &roots,
            identity.certificate_chain(),
            &self.cluster,
            &self.node,
        )?;
        let (_, until) = validity(identity.certificate_chain().iter().chain(roots.iter()))?;
        let mut state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        if !state
            .published
            .as_ref()
            .is_some_and(|bundle| Arc::ptr_eq(&roots, &bundle.roots))
            || !state
                .identity
                .as_ref()
                .is_some_and(|current| Arc::ptr_eq(&current.identity, &identity))
        {
            return Err(Error::Unavailable);
        }
        state.identity = Some(InstalledIdentity {
            identity: identity.clone(),
            roots,
            checked: now,
            until,
        });
        Ok(identity)
    }
    /// Admit a specific active or prepared epoch for its cache and purpose.
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
        let published = state.published.as_ref().ok_or(Error::MissingKey)?;
        if published.cluster != self.cluster {
            return Err(Error::MissingKey);
        }
        let position = published
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
        let entry = &published.entries[position];
        Ok(KeyLease {
            reference,
            secret: entry.secret.clone(),
        })
    }
    /// Admit the unique active key for this cache and purpose.
    pub fn active(&self, cache: &CacheId, purpose: KeyPurpose) -> Result<KeyLease> {
        let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
        let published = state.published.as_ref().ok_or(Error::MissingKey)?;
        if published.cluster != self.cluster {
            return Err(Error::MissingKey);
        }
        let first = published.entries.partition_point(|entry| {
            (&entry.reference.cache, entry.reference.purpose as u8) < (cache, purpose as u8)
        });
        let entry = published.entries[first..]
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

impl std::fmt::Display for Error {
    /// Render the payload-free variant name.
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for Error {}

/// Scoped wall time for rustls, clamped to the Unix epoch for pre-epoch clocks.
/// This retains rustls's whole-second truncation and does not set certificate policy.
pub fn unix_time() -> rustls::pki_types::UnixTime {
    rustls::pki_types::UnixTime::since_unix_epoch(
        environment::wall_now()
            .duration_since(std::time::SystemTime::UNIX_EPOCH)
            .unwrap_or_default(),
    )
}

/// Verify bounded client-auth certificates, canonical SAN, and Ed25519 leaf key.
fn verify_chain(
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
        .verify_client_cert(&leaf, &intermediates, unix_time())
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

/// Parse bounded CA roots and require every configured root to be valid now.
fn root_store(roots: &[Vec<u8>]) -> Result<RootCertStore> {
    if roots.is_empty() || roots.len() > 32 {
        return Err(Error::Unauthorized);
    }
    let mut store = RootCertStore::empty();
    for root in roots {
        if root.is_empty() || root.len() > 16384 {
            return Err(Error::Unauthorized);
        }
        let (rest, cert) = parse_x509_certificate(root).map_err(|_| Error::Unauthorized)?;
        let now = unix_time().as_secs();
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

/// Conservative validity intersection includes all configured trust anchors.
fn validity<'a>(certificates: impl Iterator<Item = &'a Vec<u8>>) -> Result<(u64, u64)> {
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

/// Build the single canonical URI permitted for this cluster and node.
fn spiffe(cluster: &ClusterId, node: &NodeId) -> Result<String> {
    if !canonical_uuid(&cluster.0) || !canonical_uuid(&node.0) {
        return Err(Error::Unauthorized);
    }
    Ok(format!("spiffe://{}/node/{}", cluster.0, node.0))
}

/// Opt-in Ed25519 certificate fixtures with explicit parameter customization.
#[cfg(any(test, feature = "test-util"))]
pub mod test_util {
    use super::*;

    /// Generate an unconstrained signing CA using the fixture defaults.
    pub fn ca() -> (rcgen::Certificate, rcgen::KeyPair) {
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        params.key_usages = vec![
            rcgen::KeyUsagePurpose::KeyCertSign,
            rcgen::KeyUsagePurpose::CrlSign,
        ];
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        (params.self_signed(&key).unwrap(), key)
    }

    /// Generate a node key and issue a customizable client-auth certificate.
    pub fn issue(
        ca: &rcgen::Certificate,
        ca_key: &rcgen::KeyPair,
        cluster: &ClusterId,
        node: &NodeId,
        customize: impl FnOnce(&mut rcgen::CertificateParams),
    ) -> (PendingIdentity, Vec<Vec<u8>>) {
        let pending = PendingIdentity::generate().unwrap();
        issue_pending(pending, ca, ca_key, cluster, node, customize)
    }

    /// Issue for an existing key without regenerating it or changing defaults.
    pub fn issue_pending(
        pending: PendingIdentity,
        ca: &rcgen::Certificate,
        ca_key: &rcgen::KeyPair,
        cluster: &ClusterId,
        node: &NodeId,
        customize: impl FnOnce(&mut rcgen::CertificateParams),
    ) -> (PendingIdentity, Vec<Vec<u8>>) {
        let secret = pending.export_pkcs8_for_persistence().unwrap();
        let key = rcgen::KeyPair::from_pkcs8_der_and_sign_algo(
            &PrivatePkcs8KeyDer::from(secret.as_slice()),
            &rcgen::PKCS_ED25519,
        )
        .unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.subject_alt_names = vec![rcgen::SanType::URI(
            format!("spiffe://{}/node/{}", cluster.0, node.0)
                .try_into()
                .unwrap(),
        )];
        params.key_usages = vec![rcgen::KeyUsagePurpose::DigitalSignature];
        params.extended_key_usages = vec![rcgen::ExtendedKeyUsagePurpose::ClientAuth];
        customize(&mut params);
        let cert = params.signed_by(&key, ca, ca_key).unwrap();
        (pending, vec![cert.der().to_vec()])
    }
}

/// Fixture and scoped-time contracts shared with parent integration tests.
#[cfg(test)]
mod fixture_tests {
    use super::test_util::*;
    use super::*;
    use std::time::{Duration, UNIX_EPOCH};

    /// TLS time follows the current scope and clamps/truncates epoch offsets.
    #[test]
    fn tls_time_uses_current_scoped_wall_clock_clamps_and_truncates() {
        let clock = uring_runtime::environment::SimulationClock::new(71);
        let _environment = clock.environment(0).enter();
        for (wall, seconds) in [
            (UNIX_EPOCH - Duration::from_nanos(1), 0),
            (UNIX_EPOCH, 0),
            (UNIX_EPOCH + Duration::from_millis(1999), 1),
            (UNIX_EPOCH + Duration::from_secs(10), 10),
        ] {
            clock.set_wall_time(wall);
            assert_eq!(unix_time().as_secs(), seconds);
        }
    }

    /// Issuing for an existing key keeps its bytes and caller-specified dates.
    #[test]
    fn existing_key_and_customized_certificate_contract_are_preserved() {
        let (ca, ca_key) = ca();
        let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
        let node = NodeId("22222222-2222-4222-8222-222222222222".into());
        let pending = PendingIdentity::generate().unwrap();
        let original = pending.export_pkcs8_for_persistence().unwrap();
        let (pending, chain) = issue_pending(pending, &ca, &ca_key, &cluster, &node, |params| {
            params.not_before = rcgen::date_time_ymd(2020, 1, 1);
            params.not_after = rcgen::date_time_ymd(2030, 1, 1);
        });
        assert_eq!(*pending.export_pkcs8_for_persistence().unwrap(), *original);
        let (_, cert) = parse_x509_certificate(&chain[0]).unwrap();
        assert_eq!(cert.validity().not_before.timestamp(), 1_577_836_800);
        assert_eq!(cert.validity().not_after.timestamp(), 1_893_456_000);
        pending
            .accept(cluster, node, chain, &[ca.der().to_vec()])
            .unwrap();
    }
}

/// Certificate rejection and cache invalidation state-space tests.
#[cfg(test)]
mod certificate_tests {
    use super::tests::{CLUSTER, NODE, issued, issued_with};
    use super::*;
    /// Cached chains still require current roots, time, exact bytes, and signatures.
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
    /// Verify canonical node chains and reject malformed or altered signatures.
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
    /// Exercise usage, validity, CA-leaf, and ambiguous URI rejection.
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
    /// Reject alternate spellings that could alias a canonical node identity.
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
            assert_eq!(
                spiffe(&ClusterId(value.into()), &NodeId(NODE.into())),
                Err(Error::Unauthorized)
            );
            assert_eq!(
                spiffe(&ClusterId(CLUSTER.into()), &NodeId(value.into())),
                Err(Error::Unauthorized)
            );
        }
    }
}
/// Epoch transition tests cover rejection, replay, and retained ownership.
#[cfg(test)]
mod keyring_tests {
    use super::tests::{CACHE, CLUSTER, NODE};
    use super::*;

    /// Install the default cache's page and credential keys.
    pub(super) fn keys() -> Keyring {
        keys_for(&[CacheId(CACHE.into())])
    }

    /// Install distinct material for every requested cache.
    fn keys_for(caches: &[CacheId]) -> Keyring {
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

    /// Create fresh generation-bound IDs and material for a rotation.
    pub(super) fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
        let mut next = bundle(generation, roots, CacheKeyState::Active);
        for (i, key) in next.cache_keys.iter_mut().enumerate() {
            key.key.id.0[..4].copy_from_slice(b"RKG1");
            key.key.id.0[4..12].copy_from_slice(&generation.to_be_bytes());
            key.key.id.0[12..].copy_from_slice(&(i as u32).to_be_bytes());
            key.material[..8].copy_from_slice(&generation.to_be_bytes());
        }
        next
    }

    /// Inspect current admission without counting outstanding lease owners.
    fn admitted_count(keys: &Keyring) -> usize {
        keys.epochs
            .state
            .lock()
            .unwrap()
            .published
            .as_ref()
            .unwrap()
            .entries
            .len()
    }

    /// Replay is order-sensitive; failures preserve roots and retained secret owners.
    #[test]
    fn publication_is_coherent_and_replay_preserves_shared_owners() {
        let (_, _, mut roots) = super::tests::issued();
        roots.extend(super::tests::issued().2);
        let keys = Keyring::new(
            ClusterId(CLUSTER.into()),
            NodeId(NODE.into()),
            Arc::new(KeyEpochs::default()),
        );
        let mut initial = bundle(1, roots, CacheKeyState::Active);
        let mut invalid = initial.clone();
        invalid.cache_keys[1].material = invalid.cache_keys[0].material.clone();
        assert_eq!(
            keys.install_inner(&invalid),
            Err(Error::InvalidConfiguration)
        );
        assert!(keys.epochs.state.lock().unwrap().published.is_none());
        assert_eq!(keys.generation().unwrap(), None);
        assert!(keys.peer_trust_roots().is_err());

        keys.install_inner(&initial).unwrap();
        let roots = keys.peer_trust_roots().unwrap();
        let held = keys
            .active(&CacheId(CACHE.into()), KeyPurpose::Page)
            .unwrap();
        keys.install_inner(&initial).unwrap();
        for reorder_roots in [false, true] {
            let mut changed = initial.clone();
            if reorder_roots {
                changed.peer_trust_roots.reverse();
            } else {
                changed.cache_keys.reverse();
            }
            assert_eq!(
                keys.install_inner(&changed),
                Err(Error::InvalidConfiguration)
            );
            assert_eq!(keys.generation().unwrap(), Some(1));
            assert!(Arc::ptr_eq(&roots, &keys.peer_trust_roots().unwrap()));
            let admitted = keys.active(held.cache(), KeyPurpose::Page).unwrap();
            assert!(Arc::ptr_eq(&held.secret, &admitted.secret));
        }

        initial.generation = BundleGeneration(2);
        keys.install_inner(&initial).unwrap();
        assert_eq!(keys.generation().unwrap(), Some(2));
        assert!(Arc::ptr_eq(&roots, &keys.peer_trust_roots().unwrap()));
        assert!(Arc::ptr_eq(
            &held.secret,
            &keys.active(held.cache(), KeyPurpose::Page).unwrap().secret
        ));
    }

    /// Removed epochs cannot return even after skipped bundle generations.
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
        assert_eq!(admitted_count(&keys), 2);
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
        assert_eq!(admitted_count(&keys), 2);
    }

    /// Both replay and rotation must validate every key's encoded generation.
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

    /// Public wire installation retains state and error mapping on rejection.
    #[test]
    fn public_install_preserves_generation_errors_and_installed_state() {
        let keys = keys();
        let roots = (*keys.peer_trust_roots().unwrap()).clone();
        for id in [
            KeyId([1; 16]),
            KeyId(*b"RKG1\0\0\0\0\0\0\0\0\0\0\0\0"),
            KeyId::from_generation(3, 0).unwrap(),
        ] {
            let bundle = racer_control_wire::KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: ClusterId(CLUSTER.into()),
                generation: BundleGeneration(2),
                peer_trust_roots: roots.clone(),
                cache_keys: vec![racer_control_wire::CacheEncryptionKey::new(
                    CacheKeyRef {
                        cache: CacheId(CACHE.into()),
                        id,
                        purpose: CacheKeyPurpose::Page,
                    },
                    CacheKeyState::Active,
                    Zeroizing::new([9; 32]),
                )],
            };
            assert_eq!(keys.install(bundle), Err(Error::InvalidConfiguration));
            assert_eq!(keys.generation().unwrap(), Some(1));
        }
    }

    /// Build a default pair with generation-one key IDs and configurable admission.
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

    /// Rotation preserves prepared secret ownership and rejects invalid rebinding.
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

    /// Retiring an epoch closes admission but its final lease still owns its bytes.
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

    /// Replays and newer publications cannot undo completed retirement.
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

    /// Empty admission preserves held leases and still forbids resurrection.
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
        assert_eq!(admitted_count(&keys), 0);
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

    /// Capacity counts only current admission, not previously issued leases.
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
        assert_eq!(admitted_count(&keys), 4096);
        full.generation = BundleGeneration(3);
        full.cache_keys.push(template);
        assert_eq!(keys.install_inner(&full), Err(Error::InvalidConfiguration));
        assert_eq!(keys.generation().unwrap(), Some(2));
        assert_eq!(admitted_count(&keys), 4096);
        keys.install_inner(&rotation_bundle(3, roots)).unwrap();
        assert_eq!(admitted_count(&keys), 2);
        assert_eq!(held.material(KeyPurpose::Page).unwrap(), &[7; 32]);
    }

    /// New identity admission follows current roots without invalidating held Arcs.
    #[test]
    fn identity_installation_and_leases_follow_current_trust() {
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
}
/// Borrowed-output operations enforce immutable lease bindings after retirement.
#[cfg(test)]
mod purpose_operations {
    use super::*;

    /// Page and credential operations enforce bindings without touching failed output.
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

    /// Request MAC derivation is domain-separated and enforces the credential epoch.
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
        let derived = Zeroizing::new(crate::hmac_sha256(&[8; 32], &domain));
        assert_eq!(tag, crate::hmac_sha256(&derived, b"message"));
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
/// Shared issuance fixtures and persisted-key acceptance tests.
#[cfg(test)]
mod tests {
    use super::*;
    pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
    pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";
    pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";
    /// Issue the default node certificate and its root.
    pub(super) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
        issued_with(|_| {})
    }
    /// Issue a customized node certificate for validation edge cases.
    pub(super) fn issued_with(
        customize: impl FnOnce(&mut rcgen::CertificateParams),
    ) -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
        let (ca, ca_key) = test_util::ca();
        let (pending, chain) = test_util::issue(
            &ca,
            &ca_key,
            &ClusterId(CLUSTER.into()),
            &NodeId(NODE.into()),
            customize,
        );
        (pending, chain, vec![ca.der().to_vec()])
    }
    /// Recovery, CSR generation, and TLS pairing use the same accepted key.
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
        let accepted_key = identity.key.to_pkcs8_der().unwrap();
        let private = rustls::pki_types::PrivateKeyDer::Pkcs8(PrivatePkcs8KeyDer::from(
            accepted_key.as_slice(),
        ));
        let tls_key = rustls::crypto::ring::sign::any_supported_type(&private).unwrap();
        let certified = rustls::sign::CertifiedKey::new(
            identity
                .chain
                .iter()
                .cloned()
                .map(CertificateDer::from)
                .collect(),
            tls_key,
        );
        assert!(certified.keys_match().is_ok());
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
