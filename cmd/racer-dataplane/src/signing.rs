//! Detached Ed25519 authentication. Bodies remain plaintext.
use ed25519_dalek::{Signature, Signer, SigningKey, VerifyingKey};
use sha2::{Digest, Sha256};
use std::{
    collections::BTreeMap,
    io::{self, Read},
    path::Path,
    sync::{Arc, RwLock},
};

pub type KeyId = [u8; 32];
pub const SIGNATURE_LEN: usize = 96;

/// Control-envelope authentication uses the same detached signature boundary as
/// peer traffic; remote snapshots always require a signature.
pub(crate) fn decode_configuration(
    envelope: crate::control::proto::Configuration,
    remote: bool,
    verifier: &Keys,
    universe: [u8; 32],
    node: [u8; 32],
) -> io::Result<crate::control::proto::Snapshot> {
    use crate::control::proto::{self, configuration::Contents};
    use prost::Message;
    let snapshot = match envelope
        .contents
        .ok_or_else(|| invalid("missing configuration"))?
    {
        Contents::Snapshot(s) if !remote => s,
        Contents::Snapshot(_) => return Err(invalid("HTTP configuration requires a signature")),
        Contents::Signed(e) => {
            verifier.verify(b"racer/config/v2", &[&e.snapshot], &e.signature)?;
            proto::Snapshot::decode(e.snapshot.as_slice())
                .map_err(|e| io::Error::new(io::ErrorKind::InvalidData, e))?
        }
    };
    let id = |bytes: &[u8]| {
        <[u8; 32]>::try_from(bytes).map_err(|_| invalid("identity must be 32 bytes"))
    };
    if id(&snapshot.universe)? != universe || id(&snapshot.node)? != node || snapshot.revision == 0
    {
        return Err(invalid("configuration identity or revision mismatch"));
    }
    Ok(snapshot)
}

fn invalid(message: &'static str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message)
}
fn key_id(key: &VerifyingKey) -> KeyId {
    let mut hash = Sha256::new();
    hash.update(b"racer/public-key/v2");
    hash.update(key.as_bytes());
    hash.finalize().into()
}

#[derive(Clone, Default)]
pub struct Keys {
    signing: Option<Arc<SigningKey>>,
    verifying: Arc<BTreeMap<KeyId, VerifyingKey>>,
    live: Option<Arc<RwLock<Keys>>>,
    generation: u64,
    active: Option<KeyId>,
    fingerprint: [u8; 32],
}
impl Keys {
    /// Capture one coherent signer/verifier pair for an operation or handshake.
    pub fn pinned(&self) -> Self {
        match &self.live {
            Some(live) => live.read().unwrap().clone(),
            None => self.clone(),
        }
    }

    pub fn live(self) -> Self {
        if self.live.is_some() {
            return self;
        }
        Self {
            live: Some(Arc::new(RwLock::new(self))),
            ..Self::default()
        }
    }

    pub fn generation(&self) -> u64 {
        self.pinned().generation
    }

    pub fn active_id(&self) -> Option<KeyId> {
        self.pinned().active
    }

    /// Read a single fixed-name consumer bundle from one projected version.
    pub fn bundle(path: &Path, peer: bool) -> io::Result<Self> {
        #[derive(serde::Deserialize)]
        #[serde(deny_unknown_fields)]
        struct Bundle {
            version: u32,
            generation: u64,
            active: String,
            seed: Option<String>,
            public: Vec<String>,
        }
        impl Drop for Bundle {
            fn drop(&mut self) {
                if let Some(seed) = &mut self.seed {
                    zeroize::Zeroize::zeroize(seed);
                }
            }
        }
        fn hex(s: &str) -> io::Result<[u8; 32]> {
            if s.len() != 64
                || !s
                    .bytes()
                    .all(|b| b.is_ascii_digit() || (b'a'..=b'f').contains(&b))
            {
                return Err(invalid("key must be canonical lowercase hex32"));
            }
            let mut out = [0; 32];
            for (i, byte) in out.iter_mut().enumerate() {
                *byte = u8::from_str_radix(&s[i * 2..i * 2 + 2], 16).unwrap();
            }
            Ok(out)
        }
        let root = || {
            if path.join("..data").is_symlink() {
                path.join("..data").canonicalize()
            } else {
                path.canonicalize()
            }
        };
        let version = root()?;
        let read = || -> io::Result<zeroize::Zeroizing<Vec<u8>>> {
            let mut bytes = zeroize::Zeroizing::new(Vec::new());
            std::fs::File::open(version.join("bundle.json"))?
                .take(4097)
                .read_to_end(&mut bytes)?;
            if bytes.len() > 4096 {
                return Err(invalid("signing bundle too large"));
            }
            Ok(bytes)
        };
        let bytes = read()?;
        let bundle: Bundle =
            serde_json::from_slice(&bytes).map_err(|_| invalid("invalid signing bundle"))?;
        if root()? != version || *bytes != *read()? {
            return Err(invalid("signing bundle changed while loading"));
        }
        if bundle.version != 1
            || bundle.generation == 0
            || !(1..=3).contains(&bundle.public.len())
            || peer != bundle.seed.is_some()
        {
            return Err(invalid("invalid signing bundle schema or capabilities"));
        }
        let active = hex(&bundle.active)?;
        let seed = bundle.seed.as_deref().map(hex).transpose()?;
        let mut keys = Self::new(
            seed,
            bundle
                .public
                .iter()
                .map(|s| hex(s))
                .collect::<io::Result<_>>()?,
        )?;
        let active =
            VerifyingKey::from_bytes(&active).map_err(|_| invalid("invalid active key"))?;
        if !keys.trusts(&key_id(&active)) || (peer && keys.signing_id()? != key_id(&active)) {
            return Err(invalid("active key does not match bundle"));
        }
        keys.generation = bundle.generation;
        keys.active = Some(key_id(&active));
        keys.fingerprint = Sha256::digest(&*bytes).into();
        Ok(keys)
    }

    pub fn reload_bundle(&mut self, path: &Path, peer: bool) -> io::Result<bool> {
        let next = Self::bundle(path, peer)?;
        let replace = |old: &mut Self| -> io::Result<bool> {
            if next.generation < old.generation
                || (next.generation == old.generation && next.fingerprint != old.fingerprint)
            {
                return Err(invalid("signing bundle rollback or equivocation"));
            }
            let changed =
                next.digest() != old.digest() || next.signing_id().ok() != old.signing_id().ok();
            *old = next;
            Ok(changed)
        };
        match &self.live {
            Some(live) => replace(&mut live.write().unwrap()),
            None => replace(self),
        }
    }

    pub fn digest(&self) -> String {
        if self.live.is_some() {
            return self.pinned().digest();
        }
        let mut hash = Sha256::new();
        for id in self.verifying.keys() {
            hash.update(id);
        }
        hash.finalize().iter().map(|b| format!("{b:02x}")).collect()
    }
    pub fn new(seed: Option<[u8; 32]>, public_keys: Vec<[u8; 32]>) -> io::Result<Self> {
        let mut verifying = BTreeMap::new();
        for bytes in public_keys {
            let key = VerifyingKey::from_bytes(&bytes)
                .map_err(|_| invalid("invalid Ed25519 public key"))?;
            if key.is_weak() || verifying.insert(key_id(&key), key).is_some() {
                return Err(invalid("weak or duplicate Ed25519 public key"));
            }
        }
        let signing = seed.map(|mut seed| {
            let key = Arc::new(SigningKey::from_bytes(&seed));
            zeroize::Zeroize::zeroize(&mut seed);
            key
        });
        Ok(Self {
            signing,
            verifying: Arc::new(verifying),
            ..Self::default()
        })
    }
    /// Managed peer bundles use a shared live provider.
    pub fn from_env() -> io::Result<Self> {
        if let Some(path) = std::env::var_os("RACER_PEER_KEYS_DIR") {
            return Ok(Self::bundle(Path::new(&path), true)?.live());
        }
        Err(invalid("RACER_PEER_KEYS_DIR is required"))
    }
    pub fn can_sign(&self) -> bool {
        if self.live.is_some() {
            return self.pinned().can_sign();
        }
        self.signing.is_some()
    }
    pub fn requires_verification(&self) -> bool {
        if self.live.is_some() {
            return self.pinned().requires_verification();
        }
        !self.verifying.is_empty()
    }
    pub fn can_authenticate(&self) -> bool {
        self.can_sign() && self.requires_verification()
    }
    pub fn signing_id(&self) -> io::Result<KeyId> {
        if self.live.is_some() {
            return self.pinned().signing_id();
        }
        self.signing
            .as_ref()
            .map(|key| key_id(&key.verifying_key()))
            .ok_or_else(|| invalid("signing is disabled"))
    }
    pub fn trusts(&self, id: &KeyId) -> bool {
        if self.live.is_some() {
            return self.pinned().trusts(id);
        }
        self.verifying.contains_key(id)
    }
    pub fn sign(&self, domain: &[u8], fields: &[&[u8]]) -> io::Result<[u8; SIGNATURE_LEN]> {
        if self.live.is_some() {
            return self.pinned().sign(domain, fields);
        }
        let key = self
            .signing
            .as_ref()
            .ok_or_else(|| invalid("signing is disabled"))?;
        let mut out = [0; SIGNATURE_LEN];
        out[..32].copy_from_slice(&key_id(&key.verifying_key()));
        out[32..].copy_from_slice(&key.sign(&canonical(domain, fields)).to_bytes());
        Ok(out)
    }
    pub fn verify(&self, domain: &[u8], fields: &[&[u8]], signature: &[u8]) -> io::Result<KeyId> {
        if self.live.is_some() {
            return self.pinned().verify(domain, fields, signature);
        }
        if signature.len() != SIGNATURE_LEN {
            return Err(invalid("invalid detached signature length"));
        }
        let id: KeyId = signature[..32].try_into().unwrap();
        let key = self
            .verifying
            .get(&id)
            .ok_or_else(|| invalid("unknown verification key"))?;
        let signature =
            Signature::from_slice(&signature[32..]).map_err(|_| invalid("invalid signature"))?;
        key.verify_strict(&canonical(domain, fields), &signature)
            .map_err(|_| invalid("signature verification failed"))?;
        Ok(id)
    }
}

/// Length-delimited fields prevent ambiguous encodings and cross-protocol use.
pub fn canonical(domain: &[u8], fields: &[&[u8]]) -> Vec<u8> {
    let mut out = b"RACERSIG2".to_vec();
    for field in std::iter::once(&domain).chain(fields.iter()) {
        out.extend_from_slice(&(field.len() as u64).to_be_bytes());
        out.extend_from_slice(field);
    }
    out
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/signing.rs"
));
