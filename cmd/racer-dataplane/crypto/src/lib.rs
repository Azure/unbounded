#![forbid(unsafe_code)]

pub mod enrollment;

use chacha20poly1305::{
    KeyInit, Tag, XChaCha20Poly1305,
    aead::{AeadInOut, inout::InOutBuf},
};
use ed25519_dalek::{
    Signature, Signer,
    pkcs8::{DecodePrivateKey, EncodePrivateKey},
};
use sha2::{Digest, Sha256};
use subtle::ConstantTimeEq;
use zeroize::Zeroizing;

#[derive(Debug)]
pub struct Error(());

pub struct SigningKey(ed25519_dalek::SigningKey);

#[derive(Clone, Copy, PartialEq, Eq, Debug)]
pub struct VerifyingKey(ed25519_dalek::VerifyingKey);

pub const TAG_LEN: usize = 16;

impl SigningKey {
    pub fn from_seed(seed: &[u8; 32]) -> Self {
        Self(ed25519_dalek::SigningKey::from_bytes(seed))
    }

    pub fn from_pkcs8_der(der: &[u8]) -> Result<Self, Error> {
        ed25519_dalek::SigningKey::from_pkcs8_der(der)
            .map(Self)
            .map_err(|_| Error(()))
    }

    pub fn to_pkcs8_der(&self) -> Result<Zeroizing<Vec<u8>>, Error> {
        let document = self.0.to_pkcs8_der().map_err(|_| Error(()))?;
        Ok(Zeroizing::new(document.as_bytes().to_vec()))
    }

    pub fn verifying_key(&self) -> VerifyingKey {
        VerifyingKey(self.0.verifying_key())
    }

    pub fn sign(&self, msg: &[u8]) -> [u8; 64] {
        self.0.sign(msg).to_bytes()
    }
}

impl VerifyingKey {
    pub fn from_bytes(bytes: &[u8; 32]) -> Result<Self, Error> {
        ed25519_dalek::VerifyingKey::from_bytes(bytes)
            .map(Self)
            .map_err(|_| Error(()))
    }

    pub fn as_bytes(&self) -> &[u8; 32] {
        self.0.as_bytes()
    }

    pub fn verify_strict(&self, msg: &[u8], sig: &[u8]) -> Result<(), Error> {
        let signature = Signature::from_slice(sig).map_err(|_| Error(()))?;
        self.0.verify_strict(msg, &signature).map_err(|_| Error(()))
    }
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("cryptographic operation failed")
    }
}

impl std::error::Error for Error {}

pub fn seal(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    plaintext: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    if plaintext.len().checked_add(TAG_LEN) != Some(out.len()) {
        return Err(Error(()));
    }
    let (ciphertext, tag_out) = out.split_at_mut(plaintext.len());
    let buffer = InOutBuf::new(plaintext, ciphertext).map_err(|_| Error(()))?;
    let tag = XChaCha20Poly1305::new(key.into())
        .encrypt_inout_detached(nonce.into(), aad, buffer)
        .map_err(|_| Error(()))?;
    tag_out.copy_from_slice(&tag);
    Ok(())
}

pub fn open(
    key: &[u8; 32],
    nonce: &[u8; 24],
    aad: &[u8],
    sealed: &[u8],
    out: &mut [u8],
) -> Result<(), Error> {
    let length = sealed.len().checked_sub(TAG_LEN).ok_or(Error(()))?;
    if out.len() != length {
        return Err(Error(()));
    }
    let (ciphertext, tag) = sealed.split_at(length);
    let tag = Tag::try_from(tag).map_err(|_| Error(()))?;
    let buffer = InOutBuf::new(ciphertext, out).map_err(|_| Error(()))?;
    // Upstream detached decryption validates length and authenticates the input
    // before applying the keystream to output. Keep that ordering on upgrades.
    XChaCha20Poly1305::new(key.into())
        .decrypt_inout_detached(nonce.into(), aad, buffer, &tag)
        .map_err(|_| Error(()))
}

pub fn hmac_sha256(key: &[u8; 32], msg: &[u8]) -> [u8; 32] {
    let mut inner = Zeroizing::new([0x36; 64]);
    let mut outer = Zeroizing::new([0x5c; 64]);
    for i in 0..32 {
        inner[i] ^= key[i];
        outer[i] ^= key[i];
    }
    let mut hash = Sha256::new();
    hash.update(inner.as_slice());
    hash.update(msg);
    // sha2 0.10 cannot wipe its private hash state; wipe our digest buffer.
    let digest = Zeroizing::new(<[u8; 32]>::from(hash.finalize()));
    let mut hash = Sha256::new();
    hash.update(outer.as_slice());
    hash.update(digest.as_slice());
    hash.finalize().into()
}

pub fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    bool::from(a.ct_eq(b))
}

pub fn crc64(bytes: &[u8]) -> u64 {
    let mut digest = crc64fast::Digest::new();
    digest.write(bytes);
    digest.sum64()
}

pub mod identity {
    use crate::{SigningKey, VerifyingKey};
    #[cfg(test)]
    use racer_control_wire::CacheKeyPurpose;

    pub use racer_control_wire::valid_uuid as canonical_uuid;
    use racer_control_wire::{
        BundleGeneration, CacheId, CacheKeyRef, CacheKeyState, ClusterId, KeyId, NodeId,
        SCHEMA_VERSION,
    };
    use rustls::{RootCertStore, pki_types::CertificateDer};
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

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum Error {
        InvalidRequest,

        InvalidConfiguration,

        Unauthorized,

        Unavailable,

        MissingKey,

        CorruptRecord,
    }

    pub type Result<T> = std::result::Result<T, Error>;

    pub struct PendingIdentity {
        key: SigningKey,
    }

    struct RemoteSigningKey {
        key: ed25519_dalek::SigningKey,

        public: [u8; 32],
    }

    impl rcgen::RemoteKeyPair for RemoteSigningKey {
        fn public_key(&self) -> &[u8] {
            &self.public
        }

        fn sign(&self, msg: &[u8]) -> std::result::Result<Vec<u8>, rcgen::Error> {
            use ed25519_dalek::Signer;
            Ok(self.key.sign(msg).to_bytes().to_vec())
        }

        fn algorithm(&self) -> &'static rcgen::SignatureAlgorithm {
            &rcgen::PKCS_ED25519
        }
    }

    pub struct SigningIdentity {
        cluster: ClusterId,

        node: NodeId,

        key: SigningKey,

        chain: Vec<Vec<u8>>,

        expires: u64,

        valid_from: u64,
    }

    #[derive(Default)]
    pub struct KeyEpochs {
        state: Mutex<State>,
    }

    pub struct Keyring {
        cluster: ClusterId,

        node: NodeId,

        epochs: Arc<KeyEpochs>,
    }

    pub struct BundleInstaller {
        state: RefCell<BundleDelivery>,
    }

    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum BundleError {
        Replay,

        Wire(racer_control_wire::Error),

        Identity(Error),
    }

    struct BundleDelivery {
        keys: Rc<Keyring>,

        accepted: Option<AcceptedBundle>,
    }

    struct AcceptedBundle {
        generation: BundleGeneration,

        hash: [u8; 32],
    }

    impl BundleInstaller {
        pub fn new(keys: Rc<Keyring>) -> Self {
            Self {
                state: RefCell::new(BundleDelivery {
                    keys,
                    accepted: None,
                }),
            }
        }

        pub fn generation(&self) -> Option<BundleGeneration> {
            self.state
                .borrow()
                .accepted
                .as_ref()
                .map(|accepted| accepted.generation)
        }

        pub fn bind_keyring(&self, keys: Rc<Keyring>) {
            *self.state.borrow_mut() = BundleDelivery {
                keys,
                accepted: None,
            };
        }

        pub fn install(
            &self,
            mut bundle: racer_control_wire::KeyringBundle,
        ) -> std::result::Result<(BundleGeneration, Vec<Vec<u8>>), BundleError> {
            bundle.peer_trust_roots.sort();
            // Avoid stable sort's heap scratch copies of inline secret material.
            bundle
                .cache_keys
                .sort_unstable_by(|a, b| reference_order(&a.key).cmp(&reference_order(&b.key)));
            let encoded = racer_control_wire::encode_bundle(&bundle).map_err(BundleError::Wire)?;
            let hash: [u8; 32] = Sha256::digest(&*encoded).into();
            let mut state = self.state.borrow_mut();
            if let Some(old) = state.accepted.as_ref()
                && (bundle.generation < old.generation
                    || bundle.generation == old.generation && hash != old.hash)
            {
                return Err(BundleError::Replay);
            }
            // Even exact cached delivery must consult current shared epochs.
            // Another keyring view may have advanced or retired this generation.
            let roots = bundle.peer_trust_roots.clone();
            let generation = state.keys.install(bundle).map_err(BundleError::Identity)?;
            state.accepted = Some(AcceptedBundle { generation, hash });
            Ok((generation, roots))
        }
    }

    pub struct KeyLease {
        reference: CacheKeyRef,

        secret: Arc<Secret>,
    }

    pub use racer_control_wire::CacheKeyPurpose as KeyPurpose;

    pub struct Certificates {
        cluster: ClusterId,

        keys: Rc<Keyring>,

        cache: RefCell<VecDeque<CachedPeer>>,

        #[cfg(test)]
        before_key_return: RefCell<Option<Box<dyn FnOnce()>>>,
    }

    pub struct VerifiedPeer {
        node: NodeId,
    }

    pub(super) struct ValidatedChain {
        pub(super) key: VerifyingKey,

        pub(super) valid_from: u64,

        pub(super) expires: u64,
    }

    #[cfg_attr(test, derive(Clone))]
    struct CacheEncryptionKey {
        key: CacheKeyRef,

        state: CacheKeyState,

        material: Zeroizing<[u8; 32]>,
    }

    #[cfg_attr(test, derive(Clone))]
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

        pub fn csr_der(&self) -> Result<Vec<u8>> {
            let key = self.rcgen_key_pair()?;
            let params = rcgen::CertificateParams::new(Vec::<String>::new())
                .map_err(|_| Error::Unavailable)?;
            Ok(params
                .serialize_request(&key)
                .map_err(|_| Error::Unavailable)?
                .der()
                .to_vec())
        }

        pub(crate) fn rcgen_key_pair(&self) -> Result<rcgen::KeyPair> {
            rcgen::KeyPair::from_remote(Box::new(RemoteSigningKey {
                key: self.key.0.clone(),
                public: *self.key.verifying_key().as_bytes(),
            }))
            .map_err(|_| Error::Unavailable)
        }

        pub fn accept(
            self,
            cluster: ClusterId,
            node: NodeId,
            chain: Vec<Vec<u8>>,
            roots: &[Vec<u8>],
        ) -> Result<SigningIdentity> {
            let verified = verify_chain(roots, &chain, &cluster, &node)?;
            if verified.key != self.key.verifying_key() {
                return Err(Error::Unauthorized);
            }
            Ok(SigningIdentity {
                cluster,
                node,
                key: self.key,
                expires: verified.expires,
                valid_from: verified.valid_from,
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
            let now = unix_time().as_secs();
            if now < self.valid_from || now >= self.expires {
                return Err(Error::Unauthorized);
            }
            Ok(self.key.sign(message).to_vec())
        }
    }

    struct CachedPeer {
        roots: Arc<Vec<Vec<u8>>>,

        chain: Vec<Vec<u8>>,

        node: NodeId,

        key: VerifyingKey,

        until: u64,

        checked: u64,
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
                #[cfg(test)]
                before_key_return: RefCell::new(None),
            }
        }

        fn key(&self, chain: &[Vec<u8>], expected: &NodeId) -> Result<VerifyingKey> {
            let roots = self.keys.peer_trust_roots()?;
            let key = self.key_for_roots(chain, expected, roots.clone())?;
            #[cfg(test)]
            if let Some(before_return) = self.before_key_return.borrow_mut().take() {
                before_return();
            }
            if !Arc::ptr_eq(&roots, &self.keys.peer_trust_roots()?) {
                return Err(Error::Unavailable);
            }
            Ok(key)
        }

        fn key_for_roots(
            &self,
            chain: &[Vec<u8>],
            expected: &NodeId,
            roots: Arc<Vec<Vec<u8>>>,
        ) -> Result<VerifyingKey> {
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
            let verified = verify_chain(&roots, chain, &self.cluster, expected)?;
            let mut cache = self.cache.borrow_mut();
            // At most 64 exact, wire-bounded chains (4 MiB of chain bytes).
            if cache.len() == 64 {
                cache.pop_front();
            }
            cache.push_back(CachedPeer {
                roots,
                chain: chain.to_vec(),
                node: expected.clone(),
                key: verified.key,
                until: verified.expires,
                checked: now,
            });
            Ok(verified.key)
        }

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

    #[derive(Default)]
    struct State {
        published: Option<PublishedBundle>,

        identity: Option<InstalledIdentity>,
    }

    struct PublishedBundle {
        cluster: ClusterId,

        generation: BundleGeneration,

        fingerprint: [u8; 32],

        roots: Arc<Vec<Vec<u8>>>,

        entries: Vec<Entry>,
    }

    impl PublishedBundle {
        fn entry(&self, reference: &CacheKeyRef) -> Option<&Entry> {
            self.entries
                .binary_search_by(|entry| {
                    reference_order(&entry.reference).cmp(&reference_order(reference))
                })
                .ok()
                .map(|position| &self.entries[position])
        }
    }

    struct InstalledIdentity {
        identity: Arc<SigningIdentity>,

        roots: Arc<Vec<Vec<u8>>>,

        checked: u64,

        until: u64,
    }

    struct ValidatedBundle<'a> {
        bundle: &'a KeyringBundle,

        fingerprint: [u8; 32],
    }

    struct Entry {
        reference: CacheKeyRef,

        state: CacheKeyState,

        secret: Arc<Secret>,
    }

    struct Secret(Zeroizing<[u8; 32]>);

    impl KeyLease {
        pub fn require_purpose(&self, purpose: KeyPurpose) -> Result<()> {
            self.material(purpose).map(|_| ())
        }

        fn bound_material(
            &self,
            cache: &CacheId,
            id: KeyId,
            purpose: KeyPurpose,
        ) -> Result<&[u8; 32]> {
            let material = self.material(purpose)?;
            if self.cache() != cache || self.id() != id {
                return Err(Error::MissingKey);
            }
            Ok(material)
        }

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

        pub fn request_mac(
            &self,
            cache: &CacheId,
            message: &[u8],
            out: &mut [u8; 32],
        ) -> Result<()> {
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

        pub fn id(&self) -> KeyId {
            self.reference.id
        }

        pub fn cache(&self) -> &CacheId {
            &self.reference.cache
        }

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
            let published = state.published.as_ref().ok_or(Error::MissingKey)?;
            if published.cluster != self.cluster {
                return Err(Error::MissingKey);
            }
            Ok(published.roots.clone())
        }

        pub fn install(
            &self,
            bundle: racer_control_wire::KeyringBundle,
        ) -> Result<BundleGeneration> {
            // Bound private staging before allocating it, including direct callers
            // that do not pass through the wire decoder's byte limit.
            if bundle.cache_keys.len() > 4096 {
                return Err(Error::InvalidConfiguration);
            }
            self.install_inner(&bundle.into())
        }

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
            // Installation admits future and expired anchors; verification filters time.
            check_roots(&bundle.peer_trust_roots)?;
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
                    let retained = current.entry(&candidate.key);
                    if generation <= current.generation.0 && retained.is_none() {
                        return Err(Error::InvalidConfiguration);
                    }
                    if let Some(old) = retained
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
                    let retained = current.and_then(|current| current.entry(&candidate.key));
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
            entries
                .sort_by(|a, b| reference_order(&a.reference).cmp(&reference_order(&b.reference)));
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
        pub fn install_signing_identity(&self, identity: Arc<SigningIdentity>) -> Result<()> {
            if identity.node() != &self.node || identity.cluster() != &self.cluster {
                return Err(Error::Unauthorized);
            }
            let roots = self.peer_trust_roots()?;
            let verified = verify_chain(
                &roots,
                identity.certificate_chain(),
                &self.cluster,
                &self.node,
            )?;
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
                until: verified.expires,
            });
            Ok(())
        }

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
            let verified = verify_chain(
                &roots,
                identity.certificate_chain(),
                &self.cluster,
                &self.node,
            )?;
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
                until: verified.expires,
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
            let published = state.published.as_ref().ok_or(Error::MissingKey)?;
            if published.cluster != self.cluster {
                return Err(Error::MissingKey);
            }
            let entry = published.entry(&reference).ok_or(Error::MissingKey)?;
            Ok(KeyLease {
                reference,
                secret: entry.secret.clone(),
            })
        }

        pub fn active(&self, cache: &CacheId, purpose: KeyPurpose) -> Result<KeyLease> {
            let state = self.epochs.state.lock().map_err(|_| Error::Unavailable)?;
            let published = state.published.as_ref().ok_or(Error::MissingKey)?;
            if published.cluster != self.cluster {
                return Err(Error::MissingKey);
            }
            let group = (cache, purpose as u8);
            let first = published
                .entries
                .partition_point(|entry| reference_group(&entry.reference) < group);
            let entry = published.entries[first..]
                .iter()
                .take_while(|entry| reference_group(&entry.reference) == group)
                .find(|entry| entry.state == CacheKeyState::Active)
                .ok_or(Error::MissingKey)?;
            Ok(KeyLease {
                reference: entry.reference.clone(),
                secret: entry.secret.clone(),
            })
        }
    }

    impl std::fmt::Display for Error {
        fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
            write!(f, "{self:?}")
        }
    }

    impl std::error::Error for Error {}

    fn reference_group(reference: &CacheKeyRef) -> (&CacheId, u8) {
        (&reference.cache, reference.purpose as u8)
    }

    fn reference_order(reference: &CacheKeyRef) -> ((&CacheId, u8), [u8; 16]) {
        (reference_group(reference), reference.id.0)
    }

    pub fn unix_time() -> rustls::pki_types::UnixTime {
        rustls::pki_types::UnixTime::since_unix_epoch(
            environment::wall_now()
                .duration_since(std::time::SystemTime::UNIX_EPOCH)
                .unwrap_or_default(),
        )
    }

    pub(super) fn verify_chain(
        roots: &[Vec<u8>],
        chain: &[Vec<u8>],
        cluster: &ClusterId,
        node: &NodeId,
    ) -> Result<ValidatedChain> {
        if chain.is_empty()
            || chain.len() > 8
            || chain.iter().any(|c| c.is_empty() || c.len() > 16384)
            || chain.iter().map(Vec::len).sum::<usize>() > 65536
        {
            return Err(Error::Unauthorized);
        }
        let now = unix_time();
        let (store, root_bounds) = root_store_with_validity(roots, now)?;
        let leaf = CertificateDer::from(chain[0].as_slice());
        let intermediates: Vec<_> = chain[1..]
            .iter()
            .map(|c| CertificateDer::from(c.as_slice()))
            .collect();
        let (chain_start, chain_end) = validity(chain.iter())?;
        // rustls does not expose the chosen anchor. Validate each separately so
        // unrelated roots cannot shorten a held identity's lifetime.
        let mut bounds = None;
        for (anchor, (start, end)) in store.roots.into_iter().zip(root_bounds) {
            let valid_from = chain_start.max(start);
            let expires = chain_end.min(end);
            if now.as_secs() < valid_from || now.as_secs() >= expires {
                continue;
            }
            let verifier = rustls::server::WebPkiClientVerifier::builder_with_provider(
                Arc::new(RootCertStore {
                    roots: vec![anchor],
                }),
                Arc::new(rustls::crypto::ring::default_provider()),
            )
            .build()
            .map_err(|_| Error::Unauthorized)?;
            if verifier
                .verify_client_cert(&leaf, &intermediates, now)
                .is_ok()
            {
                bounds = Some((valid_from, expires));
                break;
            }
        }
        let (valid_from, expires) = bounds.ok_or(Error::Unauthorized)?;
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
        // Match the authority's exact digitalSignature and clientAuth usage sets.
        if usage.value.flags != 1
            || !extended.value.client_auth
            || extended.value.any
            || extended.value.server_auth
            || extended.value.code_signing
            || extended.value.email_protection
            || extended.value.time_stamping
            || extended.value.ocsp_signing
            || !extended.value.other.is_empty()
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
        let key = VerifyingKey::from_bytes(key).map_err(|_| Error::Unauthorized)?;
        Ok(ValidatedChain {
            key,
            valid_from,
            expires,
        })
    }

    #[cfg(test)]
    pub(super) fn root_store(roots: &[Vec<u8>]) -> Result<RootCertStore> {
        root_store_with_validity(roots, unix_time()).map(|(store, _)| store)
    }

    /// Check root structure without filtering by time, as keyring installation does.
    pub(super) fn check_roots(roots: &[Vec<u8>]) -> Result<()> {
        build_root_store(roots, None).map(|_| ())
    }

    fn root_store_with_validity(
        roots: &[Vec<u8>],
        now: rustls::pki_types::UnixTime,
    ) -> Result<(RootCertStore, Vec<(u64, u64)>)> {
        build_root_store(roots, Some(now))
    }

    fn build_root_store(
        roots: &[Vec<u8>],
        now: Option<rustls::pki_types::UnixTime>,
    ) -> Result<(RootCertStore, Vec<(u64, u64)>)> {
        if roots.is_empty() || roots.len() > 32 {
            return Err(Error::Unauthorized);
        }
        let mut store = RootCertStore::empty();
        let mut bounds = Vec::new();
        for root in roots {
            if root.is_empty() || root.len() > 16384 {
                return Err(Error::Unauthorized);
            }
            let (rest, cert) = parse_x509_certificate(root).map_err(|_| Error::Unauthorized)?;
            if !rest.is_empty() || !cert.is_ca() {
                return Err(Error::Unauthorized);
            }
            let start = cert.validity().not_before.timestamp();
            let end = cert.validity().not_after.timestamp();
            store
                .add(CertificateDer::from(root.clone()))
                .map_err(|_| Error::Unauthorized)?;
            if let Some(now) = now {
                let seconds = i64::try_from(now.as_secs()).map_err(|_| Error::Unauthorized)?;
                if seconds < start || seconds >= end {
                    store.roots.pop();
                    continue;
                }
            }
            bounds.push((start.max(0) as u64, end.max(0) as u64));
        }
        if store.is_empty() {
            return Err(Error::Unauthorized);
        }
        Ok((store, bounds))
    }

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

    fn spiffe(cluster: &ClusterId, node: &NodeId) -> Result<String> {
        if !canonical_uuid(&cluster.0) || !canonical_uuid(&node.0) {
            return Err(Error::Unauthorized);
        }
        Ok(format!("spiffe://{}/node/{}", cluster.0, node.0))
    }

    #[cfg(any(test, feature = "test-util"))]
    pub mod test_util {
        use super::*;

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

        pub fn issue_pending(
            pending: PendingIdentity,
            ca: &rcgen::Certificate,
            ca_key: &rcgen::KeyPair,
            cluster: &ClusterId,
            node: &NodeId,
            customize: impl FnOnce(&mut rcgen::CertificateParams),
        ) -> (PendingIdentity, Vec<Vec<u8>>) {
            let key = pending.rcgen_key_pair().unwrap();
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

    #[cfg(test)]
    mod bundle_tests {
        use super::test_util::*;
        use super::*;

        #[test]
        fn installation_accepts_time_invalid_roots_but_verification_does_not() {
            use std::time::Duration;

            let clock = environment::SimulationClock::new(111);
            let _environment = clock.environment(0).enter();
            let now = environment::wall_now();
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let node = NodeId("22222222-2222-4222-8222-222222222222".into());
            let (_, ca_key) = ca();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
            params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
            params.not_before = (now + Duration::from_secs(60)).into();
            params.not_after = (now + Duration::from_secs(120)).into();
            let future = params.clone().self_signed(&ca_key).unwrap();
            params.not_before = (now - Duration::from_secs(120)).into();
            params.not_after = now.into();
            let expired = params.self_signed(&ca_key).unwrap();
            let (_, chain) = issue(&future, &ca_key, &cluster, &node, |_| {});

            for roots in [
                vec![future.der().to_vec()],
                vec![expired.der().to_vec()],
                vec![expired.der().to_vec(), future.der().to_vec()],
            ] {
                let keys = Rc::new(Keyring::new(cluster.clone(), node.clone(), Arc::default()));
                let installer = BundleInstaller::new(keys.clone());
                let mut bundle = racer_control_wire::KeyringBundle {
                    schema_version: SCHEMA_VERSION,
                    cluster: cluster.clone(),
                    generation: BundleGeneration(1),
                    peer_trust_roots: roots.clone(),
                    cache_keys: vec![],
                };
                installer.install(bundle.clone()).unwrap();
                installer.install(bundle.clone()).unwrap();
                assert!(verify_chain(&roots, &chain, &cluster, &node).is_err());
                clock.set_wall_time(now + Duration::from_secs(60));
                assert_eq!(
                    verify_chain(&roots, &chain, &cluster, &node).is_ok(),
                    roots.contains(&future.der().to_vec())
                );
                clock.set_wall_time(now + Duration::from_secs(120));
                assert!(verify_chain(&roots, &chain, &cluster, &node).is_err());
                bundle.generation = BundleGeneration(2);
                installer.install(bundle.clone()).unwrap();
                bundle.generation = BundleGeneration(3);
                for invalid in [vec![], vec![vec![0; 128]], chain.clone()] {
                    bundle.peer_trust_roots = invalid;
                    assert!(installer.install(bundle.clone()).is_err());
                    assert!(keys.install(bundle.clone()).is_err());
                    assert_eq!(keys.generation().unwrap(), Some(2));
                    assert_eq!(installer.generation(), Some(BundleGeneration(2)));
                }
                clock.set_wall_time(now);
            }
        }

        #[test]
        fn cached_delivery_revalidates_shared_keyring_epoch() {
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let node = NodeId("22222222-2222-4222-8222-222222222222".into());
            let epochs = Arc::new(KeyEpochs::default());
            let keys = Rc::new(Keyring::new(cluster.clone(), node.clone(), epochs.clone()));
            let other = Keyring::new(cluster.clone(), node, epochs);
            let installer = BundleInstaller::new(keys.clone());
            let (old_ca, _) = ca();
            let (new_ca, _) = ca();
            let old = racer_control_wire::KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster,
                generation: BundleGeneration(2),
                peer_trust_roots: vec![old_ca.der().to_vec()],
                cache_keys: vec![],
            };
            let accepted = installer.install(old.clone()).unwrap();
            let original_roots = keys.peer_trust_roots().unwrap();
            assert_eq!(installer.install(old.clone()).unwrap(), accepted);
            assert!(Arc::ptr_eq(
                &keys.peer_trust_roots().unwrap(),
                &original_roots
            ));
            let mut newer = old.clone();
            newer.generation = BundleGeneration(3);
            newer.peer_trust_roots = vec![new_ca.der().to_vec()];
            other.install(newer.clone()).unwrap();
            assert_eq!(keys.generation().unwrap(), Some(3));
            assert_eq!(
                installer.install(old),
                Err(BundleError::Identity(Error::InvalidConfiguration))
            );
            assert_eq!(*keys.peer_trust_roots().unwrap(), newer.peer_trust_roots);
            assert_eq!(installer.generation(), Some(BundleGeneration(2)));
            assert_eq!(
                installer.install(newer.clone()).unwrap(),
                (BundleGeneration(3), newer.peer_trust_roots)
            );
            assert_eq!(installer.generation(), Some(BundleGeneration(3)));
        }

        #[test]
        fn bundle_delivery_canonical_replay_and_rebinding() {
            let cluster = ClusterId("11111111-1111-4111-8111-111111111111".into());
            let node = NodeId("22222222-2222-4222-8222-222222222222".into());
            let (first, first_key) = ca();
            let (second, _) = ca();
            let keys = || {
                Rc::new(Keyring::new(
                    cluster.clone(),
                    node.clone(),
                    Arc::new(KeyEpochs::default()),
                ))
            };
            let installer = BundleInstaller::new(keys());
            assert_eq!(installer.generation(), None);
            let mut bundle = racer_control_wire::KeyringBundle {
                schema_version: 1,
                cluster,
                generation: BundleGeneration(2),
                peer_trust_roots: vec![first.der().to_vec(), second.der().to_vec()],
                cache_keys: vec![],
            };
            let accepted = installer.install(bundle.clone()).unwrap();
            bundle.peer_trust_roots.reverse();
            assert_eq!(installer.install(bundle.clone()).unwrap(), accepted);
            let mut conflict = bundle.clone();
            conflict.peer_trust_roots.pop();
            assert_eq!(installer.install(conflict), Err(BundleError::Replay));
            bundle.generation = BundleGeneration(1);
            assert_eq!(installer.install(bundle.clone()), Err(BundleError::Replay));
            assert_eq!(installer.generation(), Some(BundleGeneration(2)));
            let replacement = Rc::new(Keyring::new(
                bundle.cluster.clone(),
                node,
                Arc::new(KeyEpochs::default()),
            ));
            installer.bind_keyring(replacement);
            assert_eq!(installer.generation(), None);
            assert_eq!(
                installer.install(bundle.clone()).unwrap().0,
                BundleGeneration(1)
            );
            bundle.generation = BundleGeneration(3);
            bundle.peer_trust_roots = vec![vec![0; 128]];
            assert_eq!(
                installer.install(bundle.clone()),
                Err(BundleError::Wire(racer_control_wire::Error::InvalidRequest))
            );
            let (_, chain) = issue(
                &first,
                &first_key,
                &bundle.cluster,
                &NodeId("22222222-2222-4222-8222-222222222222".into()),
                |_| {},
            );
            bundle.peer_trust_roots = chain;
            assert_eq!(
                installer.install(bundle),
                Err(BundleError::Identity(Error::Unauthorized))
            );
            assert_eq!(installer.generation(), Some(BundleGeneration(1)));
        }
    }

    #[cfg(test)]
    mod certificate_tests {
        use super::tests::{CLUSTER, NODE, issued};
        use super::*;

        #[test]
        fn root_time_filtering_preserves_only_valid_signing_paths() {
            use std::time::Duration;

            for short_is_issuer in [false, true] {
                let clock = environment::SimulationClock::new(107);
                let _environment = clock.environment(0).enter();
                let now = environment::wall_now();
                let cluster = ClusterId(CLUSTER.into());
                let node = NodeId(NODE.into());
                let (long, long_key) = test_util::ca();
                let (_, short_key) = test_util::ca();
                let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
                params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
                params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
                params.not_before = now.into();
                params.not_after = (now + Duration::from_secs(60)).into();
                let short = params.self_signed(&short_key).unwrap();
                let (issuer, key) = if short_is_issuer {
                    (&short, &short_key)
                } else {
                    (&long, &long_key)
                };
                let (pending, chain) = test_util::issue(issuer, key, &cluster, &node, |_| {});
                // Try the unrelated anchor first, including matching default CA names.
                let mut roots = vec![short.der().to_vec(), long.der().to_vec()];
                if short_is_issuer {
                    roots.reverse();
                }
                let identity = Arc::new(
                    pending
                        .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
                        .unwrap(),
                );
                let (_, expected_end) =
                    validity(chain.iter().chain(std::iter::once(&roots[1]))).unwrap();
                assert_eq!(identity.expires_at_seconds(), expected_end);
                let keys = Rc::new(Keyring::new(cluster.clone(), node.clone(), Arc::default()));
                let bundle = |generation| racer_control_wire::KeyringBundle {
                    schema_version: SCHEMA_VERSION,
                    cluster: cluster.clone(),
                    generation: BundleGeneration(generation),
                    peer_trust_roots: roots.clone(),
                    cache_keys: vec![],
                };
                keys.install(bundle(1)).unwrap();
                keys.install_signing_identity(identity.clone()).unwrap();
                let certs = Certificates::new(cluster.clone(), keys.clone());
                let signature = identity.sign(b"root lifetime").unwrap();
                certs
                    .verify_signed(&chain, &node, b"root lifetime", &signature)
                    .unwrap();
                clock.set_wall_time(now + Duration::from_secs(59));
                assert!(identity.sign(b"before expiry").is_ok());
                for elapsed in [60, 120] {
                    clock.set_wall_time(now + Duration::from_secs(elapsed));
                    assert_eq!(identity.sign(b"after expiry").is_ok(), !short_is_issuer);
                    assert_eq!(keys.signing_identity().is_ok(), !short_is_issuer);
                    assert_eq!(
                        certs
                            .verify_signed(&chain, &node, b"root lifetime", &signature)
                            .is_ok(),
                        !short_is_issuer
                    );
                    let fresh = Certificates::new(cluster.clone(), keys.clone());
                    assert_eq!(
                        fresh
                            .verify_signed(&chain, &node, b"root lifetime", &signature)
                            .is_ok(),
                        !short_is_issuer
                    );
                    assert_eq!(
                        verify_chain(&roots, &chain, &cluster, &node).is_ok(),
                        !short_is_issuer
                    );
                    keys.install(bundle(elapsed)).unwrap();
                }
                clock.set_wall_time(now - Duration::from_secs(1));
                assert_eq!(identity.sign(b"rollback").is_ok(), !short_is_issuer);
                assert_eq!(keys.signing_identity().is_ok(), !short_is_issuer);
                assert_eq!(certs.verify(&chain, &node).is_ok(), !short_is_issuer);
            }
        }

        #[test]
        fn intermediate_expiry_blocks_held_signing_and_cached_verification() {
            use std::time::Duration;

            let clock = environment::SimulationClock::new(110);
            let _environment = clock.environment(0).enter();
            let now = environment::wall_now();
            let cluster = ClusterId(CLUSTER.into());
            let node = NodeId(NODE.into());
            let (root, root_key) = test_util::ca();
            let (_, issuer_key) = test_util::ca();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
            params.key_usages = vec![rcgen::KeyUsagePurpose::KeyCertSign];
            params
                .distinguished_name
                .push(rcgen::DnType::CommonName, "intermediate");
            params.not_before = now.into();
            params.not_after = (now + Duration::from_secs(60)).into();
            let issuer = params.signed_by(&issuer_key, &root, &root_key).unwrap();
            let (pending, mut chain) =
                test_util::issue(&issuer, &issuer_key, &cluster, &node, |_| {});
            chain.push(issuer.der().to_vec());
            let roots = vec![root.der().to_vec()];
            let identity = Arc::new(
                pending
                    .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
                    .unwrap(),
            );
            assert_eq!(identity.expires_at_seconds(), unix_time().as_secs() + 60);
            let keys = Rc::new(Keyring::new(cluster.clone(), node.clone(), Arc::default()));
            keys.install(racer_control_wire::KeyringBundle {
                schema_version: SCHEMA_VERSION,
                cluster: cluster.clone(),
                generation: BundleGeneration(1),
                peer_trust_roots: roots,
                cache_keys: vec![],
            })
            .unwrap();
            keys.install_signing_identity(identity.clone()).unwrap();
            let certs = Certificates::new(cluster.clone(), keys.clone());
            let signature = identity.sign(b"intermediate lifetime").unwrap();
            certs
                .verify_signed(&chain, &node, b"intermediate lifetime", &signature)
                .unwrap();
            clock.set_wall_time(now + Duration::from_secs(60));
            assert!(identity.sign(b"expired issuer").is_err());
            assert!(keys.signing_identity().is_err());
            assert!(
                certs
                    .verify_signed(&chain, &node, b"intermediate lifetime", &signature)
                    .is_err()
            );
            assert!(
                Certificates::new(cluster, keys)
                    .verify(&chain, &node)
                    .is_err()
            );
        }

        #[test]
        fn root_store_filters_time_invalid_roots_and_rejects_all_invalid_sets() {
            use std::time::Duration;

            let clock = environment::SimulationClock::new(108);
            let _environment = clock.environment(0).enter();
            let now = environment::wall_now();
            let (pending, chain, good) = issued();
            let (_, key) = test_util::ca();
            let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
            params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
            params.not_before = (now + Duration::from_secs(60)).into();
            params.not_after = (now + Duration::from_secs(120)).into();
            let future = params.clone().self_signed(&key).unwrap();
            params.not_before = (now - Duration::from_secs(120)).into();
            params.not_after = now.into();
            let expired = params.self_signed(&key).unwrap();
            let invalid = vec![expired.der().to_vec(), future.der().to_vec()];
            assert!(root_store(&invalid).is_err());
            assert!(root_store(&invalid[..1]).is_err());
            assert!(root_store(&invalid[1..]).is_err());
            let mut roots = invalid;
            roots.extend(good);
            assert_eq!(root_store(&roots).unwrap().len(), 1);
            let cluster = ClusterId(CLUSTER.into());
            let node = NodeId(NODE.into());
            let identity = pending
                .accept(cluster.clone(), node.clone(), chain, &roots)
                .unwrap();
            assert!(identity.sign(b"healthy root").is_ok());
            let (_, future_chain) = test_util::issue(&future, &key, &cluster, &node, |_| {});
            assert!(verify_chain(&roots, &future_chain, &cluster, &node).is_err());
            clock.set_wall_time(now + Duration::from_secs(60));
            assert!(verify_chain(&roots, &future_chain, &cluster, &node).is_ok());
            roots.push(vec![0; 128]);
            assert!(root_store(&roots).is_err());
        }

        #[test]
        fn peer_key_rechecks_roots_after_cached_and_fresh_validation() {
            for cached in [false, true] {
                for replace_roots in [false, true] {
                    let (pending, chain, roots) = issued();
                    let epochs = Arc::new(KeyEpochs::default());
                    let cluster = ClusterId(CLUSTER.into());
                    let node = NodeId(NODE.into());
                    let keys = Rc::new(Keyring::new(cluster.clone(), node.clone(), epochs.clone()));
                    let other = Keyring::new(cluster.clone(), node.clone(), epochs);
                    let bundle = racer_control_wire::KeyringBundle {
                        schema_version: SCHEMA_VERSION,
                        cluster: cluster.clone(),
                        generation: BundleGeneration(1),
                        peer_trust_roots: roots.clone(),
                        cache_keys: vec![],
                    };
                    keys.install(bundle.clone()).unwrap();
                    let identity = pending
                        .accept(cluster.clone(), node.clone(), chain.clone(), &roots)
                        .unwrap();
                    let signature = identity.sign(b"message").unwrap();
                    let certs = Certificates::new(cluster, keys.clone());
                    if cached {
                        certs
                            .verify_signed(&chain, &node, b"message", &signature)
                            .unwrap();
                        assert_eq!(certs.cache.borrow().len(), 1);
                    } else {
                        assert!(certs.cache.borrow().is_empty());
                    }
                    let mut next = bundle;
                    next.generation = BundleGeneration(2);
                    if replace_roots {
                        next.peer_trust_roots = issued().2;
                    }
                    *certs.before_key_return.borrow_mut() = Some(Box::new(move || {
                        other.install(next).unwrap();
                    }));
                    let result = certs
                        .verify_signed(&chain, &node, b"message", &signature)
                        .map(|peer| peer.node().clone());
                    assert_eq!(keys.generation().unwrap(), Some(2));
                    if replace_roots {
                        assert_eq!(result, Err(Error::Unavailable), "cached={cached}");
                        assert_eq!(certs.key(&chain, &node), Err(Error::Unauthorized));
                    } else {
                        assert_eq!(result, Ok(node.clone()), "cached={cached}");
                        assert!(certs.key(&chain, &node).is_ok());
                    }
                }
            }
        }

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

    #[cfg(test)]
    mod keyring_tests {
        use super::tests::{CACHE, CLUSTER, NODE};
        use super::*;

        fn keys() -> Keyring {
            let (_, _, roots) = super::tests::issued();
            let keys = Keyring::new(
                ClusterId(CLUSTER.into()),
                NodeId(NODE.into()),
                Arc::new(KeyEpochs::default()),
            );
            keys.install_inner(&bundle(1, roots, CacheKeyState::Active))
                .unwrap();
            keys
        }

        fn rotation_bundle(generation: u64, roots: Vec<Vec<u8>>) -> KeyringBundle {
            let mut next = bundle(generation, roots, CacheKeyState::Active);
            for (i, key) in next.cache_keys.iter_mut().enumerate() {
                key.key.id.0[..4].copy_from_slice(b"RKG1");
                key.key.id.0[4..12].copy_from_slice(&generation.to_be_bytes());
                key.key.id.0[12..].copy_from_slice(&(i as u32).to_be_bytes());
                key.material[..8].copy_from_slice(&generation.to_be_bytes());
            }
            next
        }

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
            // Direct callers may reorder retained references in a newer generation.
            initial.cache_keys.reverse();
            keys.install_inner(&initial).unwrap();
            assert_eq!(keys.generation().unwrap(), Some(2));
            assert!(Arc::ptr_eq(&roots, &keys.peer_trust_roots().unwrap()));
            assert!(Arc::ptr_eq(
                &held.secret,
                &keys.active(held.cache(), KeyPurpose::Page).unwrap().secret
            ));
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

    #[cfg(test)]
    mod tests {
        use super::*;

        #[test]
        fn rcgen_signer_is_remote_and_uses_zeroizing_dalek_key() {
            fn requires_zeroize_on_drop(_: &impl zeroize::ZeroizeOnDrop) {}

            let pending = PendingIdentity {
                key: SigningKey::from_seed(&[7; 32]),
            };
            requires_zeroize_on_drop(&pending.key.0);
            let public = pending.key.verifying_key();
            let key = pending.rcgen_key_pair().unwrap();
            drop(pending);
            let remote = key.as_remote().expect("no rcgen private-key import");
            assert_eq!(remote.public_key(), public.as_bytes());
            assert_eq!(remote.algorithm(), &rcgen::PKCS_ED25519);
            for message in [b"".as_slice(), b"CSR signing"] {
                let signature = remote.sign(message).unwrap();
                assert_eq!(signature.len(), 64);
                public.verify_strict(message, &signature).unwrap();
                assert!(public.verify_strict(b"changed", &signature).is_err());
            }
        }

        #[test]
        fn pending_csr_public_key_and_signature_match_identity() {
            use x509_parser::{certification_request::X509CertificationRequest, prelude::FromDer};

            let pending = PendingIdentity {
                key: SigningKey::from_seed(&[7; 32]),
            };
            let mut der = pending.csr_der().unwrap();
            let (rest, csr) = X509CertificationRequest::from_der(&der).unwrap();
            assert!(rest.is_empty());
            let info = &csr.certification_request_info;
            assert_eq!(
                info.subject_pki.subject_public_key.data.as_ref(),
                pending.key.verifying_key().as_bytes()
            );
            assert_eq!(
                csr.signature_algorithm.algorithm.to_id_string(),
                "1.3.101.112"
            );
            assert_eq!(
                info.subject
                    .iter_common_name()
                    .next()
                    .unwrap()
                    .as_str()
                    .unwrap(),
                "rcgen self signed cert"
            );
            csr.verify_signature().unwrap();
            *der.last_mut().unwrap() ^= 1;
            let (_, corrupted) = X509CertificationRequest::from_der(&der).unwrap();
            assert!(corrupted.verify_signature().is_err());
        }

        pub(crate) const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";

        pub(crate) const NODE: &str = "22222222-2222-4222-8222-222222222222";

        pub(crate) const CACHE: &str = "33333333-3333-4333-8333-333333333333";

        pub(super) fn issued() -> (PendingIdentity, Vec<Vec<u8>>, Vec<Vec<u8>>) {
            let (ca, ca_key) = test_util::ca();
            let (pending, chain) = test_util::issue(
                &ca,
                &ca_key,
                &ClusterId(CLUSTER.into()),
                &NodeId(NODE.into()),
                |_| {},
            );
            (pending, chain, vec![ca.der().to_vec()])
        }
    }
}
