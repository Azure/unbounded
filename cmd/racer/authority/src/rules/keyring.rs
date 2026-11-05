//! Durable keyring: cache keys, their rotation, and peer trust roots.
//!
//! Each cache has one active page key and one active origin credentials key.
//! A rotation first adds prepared keys, then activates them after
//! `prepare_for`. On activation the old active page key becomes a `decrypt`
//! key, so pages sealed under it stay readable; a cache keeps at most
//! [`Policy::retained_page_keys`]` - 1` decrypt keys and drops the oldest.
//! Old origin credentials keys are dropped on activation. Caches are never
//! evicted to make room. Every change gets a new bundle generation; exhausted
//! generations fail.
//!
//! Each seat offers an issuer root, reused across claims by the same Pod. A
//! root is kept while a seat offers it,
//! and for one Lease duration plus one certificate lifetime after that, capped
//! at its NotAfter. A removed root is listed as retired until its NotAfter so a
//! stale Lease annotation can never add it back.

use crate::entropy::Entropy;
use base64::{Engine, engine::general_purpose::STANDARD};
use racer_control_wire::{
    self as wire, BundleGeneration, CacheDefinition, CacheEncryptionKey, CacheId, CacheKeyPurpose,
    CacheKeyRef, CacheKeyState, ClusterId, KeyId, KeyringBundle,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, BTreeSet},
    time::Duration,
};

/// Most trust roots in one bundle.
pub const MAX_ROOTS: usize = wire::MAX_BUNDLE_ROOTS;

/// Largest root DER accepted, and reserved per root in catalog capacity.
pub const MAX_ROOT_DER_BYTES: usize = wire::MAX_ROOT_DER_BYTES;

/// Readable page keys per cache (active plus decrypt) by default.
pub const DEFAULT_RETAINED_PAGE_KEYS: usize = 7;

/// Upper bound of the bundle envelope without roots and keys.
const ENVELOPE: usize = 512;

/// Bytes reserved in the state record for everything but keys and roots:
/// the record fields and the retired root list.
const STATE_RESERVE: usize = 64 * 1024;

/// Upper bound of the state record fields of one root besides its DER.
const ROOT_RECORD_OVERHEAD: usize = 256;

/// Clock skew allowance.
pub const SKEW: Duration = Duration::from_secs(60);

/// Keyring failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum KeyringError {
    Invalid,
    /// Generations are exhausted.
    Unavailable,
    /// Admitted caches exceed capacity. Nothing is evicted.
    TooLarge,
    Entropy,
}

/// Cache keys stored in the state record, in canonical order (cache, purpose,
/// key ID). Serialized as the wire bundle's compact key table in base64, so
/// durable and wire key IDs share one encoding. Debug output omits material.
#[derive(Clone, Default, Eq, PartialEq)]
pub struct Keys(pub Vec<CacheEncryptionKey>);

impl std::ops::Deref for Keys {
    type Target = Vec<CacheEncryptionKey>;

    fn deref(&self) -> &Self::Target {
        &self.0
    }
}

impl std::ops::DerefMut for Keys {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut self.0
    }
}

impl std::fmt::Debug for Keys {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_list()
            .entries(self.0.iter().map(|k| {
                format!(
                    "Key({}/{:?}/{}/{:?})",
                    k.key.cache.0,
                    k.key.purpose,
                    hex(&k.key.id.0),
                    k.state
                )
            }))
            .finish()
    }
}

impl Serialize for Keys {
    fn serialize<S: serde::Serializer>(&self, serializer: S) -> Result<S::Ok, S::Error> {
        let encoded = wire::encode_cache_keys_base64(&self.0)
            .map_err(|_| serde::ser::Error::custom("invalid cache keys"))?;
        serializer.serialize_str(&encoded)
    }
}

impl<'de> Deserialize<'de> for Keys {
    fn deserialize<D: serde::Deserializer<'de>>(deserializer: D) -> Result<Self, D::Error> {
        let encoded = zeroize::Zeroizing::new(String::deserialize(deserializer)?);
        // Generations are checked by `Durable::validate`.
        wire::decode_cache_keys_base64(&encoded, u64::MAX)
            .map(Keys)
            .map_err(|_| serde::de::Error::custom("invalid cache keys"))
    }
}

fn hex(b: &[u8]) -> String {
    b.iter().map(|b| format!("{b:02x}")).collect()
}

/// One trust root.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Root {
    /// Base64 DER.
    pub der: String,
    pub seat: u8,
    /// Pod UID that offered it.
    pub holder: String,
    pub not_after: u64,
    /// Unix seconds after which the root is removed.
    pub retain_until: u64,
}

/// A removed root, by SHA-256 of its DER.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Retired {
    pub sha256: String,
    pub until: u64,
}

/// Keyring stored in the state record.
#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Durable {
    pub generation: u64,
    pub keys: Keys,
    /// Unix seconds of the next preparation.
    pub next_rotation: u64,
    /// Unix seconds to activate prepared keys, or 0.
    pub activate_at: u64,
    pub roots: Vec<Root>,
    pub retired: Vec<Retired>,
}

/// A root a seat offers through its Lease.
#[derive(Clone, Debug)]
pub struct Offer {
    pub der: Vec<u8>,
    pub seat: u8,
    pub holder: String,
}

/// Rotation timing and retention. An authority setting.
#[derive(Clone, Copy, Debug)]
pub struct Policy {
    pub interval: Duration,
    pub prepare_for: Duration,
    /// Longest certificate any seat issues.
    pub certificate_lifetime: Duration,
    pub lease_duration: Duration,
    /// Readable page keys per cache: the active key plus up to this many
    /// minus one decrypt keys. In `1..=`[`wire::MAX_RETAINED_PAGE_KEYS`].
    pub retained_page_keys: usize,
}

impl Policy {
    /// `retained_page_keys` clamped to its valid range.
    pub fn retained(&self) -> usize {
        self.retained_page_keys
            .clamp(1, wire::MAX_RETAINED_PAGE_KEYS)
    }
}

impl From<crate::config::Rotation> for Policy {
    fn from(r: crate::config::Rotation) -> Self {
        Self {
            interval: r.interval,
            prepare_for: r.prepare_for,
            certificate_lifetime: wire::CERTIFICATE_LIFETIME,
            lease_duration: crate::limits::LEASE_DURATION,
            retained_page_keys: r.retained_page_keys,
        }
    }
}

/// SHA-256 hex.
pub fn sha256(b: &[u8]) -> String {
    format!("{:x}", Sha256::digest(b))
}

const PURPOSES: [CacheKeyPurpose; 2] = [CacheKeyPurpose::Page, CacheKeyPurpose::OriginCredentials];

fn purpose_rank(p: CacheKeyPurpose) -> u8 {
    match p {
        CacheKeyPurpose::Page => 0,
        CacheKeyPurpose::OriginCredentials => 1,
    }
}

/// Canonical key order, matching the compact key table.
fn sort_keys(keys: &mut [CacheEncryptionKey]) {
    keys.sort_by(|a, b| {
        (&a.key.cache, purpose_rank(a.key.purpose), a.key.id.0).cmp(&(
            &b.key.cache,
            purpose_rank(b.key.purpose),
            b.key.id.0,
        ))
    });
}

/// Facts of a root certificate needed here.
pub struct RootInfo {
    pub not_after: u64,
}

/// Check a root: CA, Ed25519, self-signed, valid now, at most
/// [`MAX_ROOT_DER_BYTES`] so catalog capacity always holds.
pub fn inspect_root(der: &[u8], now: u64) -> Result<RootInfo, KeyringError> {
    if der.is_empty() || der.len() > MAX_ROOT_DER_BYTES {
        return Err(KeyringError::Invalid);
    }
    let (rest, cert) =
        x509_parser::parse_x509_certificate(der).map_err(|_| KeyringError::Invalid)?;
    if !rest.is_empty()
        || !cert.is_ca()
        || cert.public_key().algorithm.algorithm.to_id_string() != "1.3.101.112"
        || cert.verify_signature(None).is_err()
    {
        return Err(KeyringError::Invalid);
    }
    let not_before = cert.validity().not_before.timestamp();
    let not_after = cert.validity().not_after.timestamp();
    if not_before > now as i64 + SKEW.as_secs() as i64 || not_after <= now as i64 {
        return Err(KeyringError::Invalid);
    }
    Ok(RootInfo {
        not_after: not_after as u64,
    })
}

impl Durable {
    /// Structural validation of a decoded keyring.
    pub fn validate(&self) -> Result<(), KeyringError> {
        wire::validate_cache_keys(&self.keys, self.generation)
            .map_err(|_| KeyringError::Invalid)?;
        if self.roots.len() > MAX_ROOTS {
            return Err(KeyringError::Invalid);
        }
        let mut hashes = std::collections::HashSet::new();
        for r in &self.roots {
            let der = STANDARD.decode(&r.der).map_err(|_| KeyringError::Invalid)?;
            if der.len() > MAX_ROOT_DER_BYTES
                || !hashes.insert(sha256(&der))
                || r.seat as usize >= crate::annotations::MAX_SEATS
            {
                return Err(KeyringError::Invalid);
            }
        }
        if self.retired.iter().any(|r| hashes.contains(&r.sha256)) {
            return Err(KeyringError::Invalid);
        }
        Ok(())
    }

    /// Whether `der` is a committed root.
    pub fn root_committed(&self, der: &[u8]) -> bool {
        let encoded = STANDARD.encode(der);
        self.roots.iter().any(|r| r.der == encoded)
    }

    /// DER of every root.
    pub fn root_ders(&self) -> Vec<Vec<u8>> {
        self.roots
            .iter()
            .filter_map(|r| STANDARD.decode(&r.der).ok())
            .collect()
    }

    /// Caches with both active keys. This is the durable admission record.
    pub fn keyed_caches(&self) -> BTreeMap<String, bool> {
        let mut n: BTreeMap<String, usize> = BTreeMap::new();
        for k in self
            .keys
            .iter()
            .filter(|k| k.state == CacheKeyState::Active)
        {
            *n.entry(k.key.cache.0.clone()).or_default() += 1;
        }
        n.into_iter().map(|(k, v)| (k, v == 2)).collect()
    }

    /// Wire bundle for clients. Needs at least one root.
    pub fn bundle(&self, cluster: &str) -> Result<KeyringBundle, KeyringError> {
        let b = KeyringBundle {
            schema_version: wire::KEYRING_SCHEMA_VERSION,
            cluster: ClusterId(cluster.into()),
            generation: BundleGeneration(self.generation),
            peer_trust_roots: self.root_ders(),
            cache_keys: self.keys.0.clone(),
        };
        wire::encode_bundle(&b).map_err(|_| KeyringError::Invalid)?;
        Ok(b)
    }
}

/// Keys one cache holds at most with `retained` readable page keys: those,
/// one prepared page key, and an active and a prepared origin key.
pub fn keys_per_cache(retained: usize) -> usize {
    retained.clamp(1, wire::MAX_RETAINED_PAGE_KEYS) + 3
}

/// Number of caches that always fit, with every cache holding
/// [`keys_per_cache`] keys and [`MAX_ROOTS`] roots of [`MAX_ROOT_DER_BYTES`],
/// under both the bundle byte limit and the state record limit. Computed
/// from the compact key table encoding.
pub fn catalog_capacity(retained: usize) -> usize {
    let per_cache =
        wire::KEY_GROUP_HEADER_BYTES + keys_per_cache(retained) * wire::KEY_RECORD_BYTES;
    // Base64 plus the JSON quotes and comma.
    let root_cost = MAX_ROOT_DER_BYTES.div_ceil(3) * 4 + 3;
    let bundle = (wire::MAX_BUNDLE_BYTES - ENVELOPE - MAX_ROOTS * root_cost) / 4 * 3;
    let state =
        (super::state::MAX_BYTES - STATE_RESERVE - MAX_ROOTS * (root_cost + ROOT_RECORD_OVERHEAD))
            / 4
            * 3;
    bundle.min(state) / per_cache
}

/// Keep keyed caches, then fill free slots in UID order. Never evicts.
pub fn admit_catalog(
    catalog: &[CacheDefinition],
    durable: &Durable,
    retained: usize,
) -> Result<Vec<CacheDefinition>, KeyringError> {
    let capacity = catalog_capacity(retained);
    let keyed = durable.keyed_caches();
    let existing = catalog
        .iter()
        .filter(|c| keyed.get(&c.id.0) == Some(&true))
        .count();
    if existing > capacity {
        return Err(KeyringError::TooLarge);
    }
    let mut free = capacity - existing;
    let mut out = Vec::new();
    for c in catalog {
        if keyed.get(&c.id.0) == Some(&true) {
            out.push(c.clone());
        } else if free > 0 {
            free -= 1;
            out.push(c.clone());
        }
    }
    Ok(out)
}

fn new_key(
    rng: &mut dyn Entropy,
    cache: &str,
    purpose: CacheKeyPurpose,
    state: CacheKeyState,
    generation: u64,
) -> Result<CacheEncryptionKey, KeyringError> {
    let mut suffix = [0u8; 4];
    rng.fill(&mut suffix).map_err(|_| KeyringError::Entropy)?;
    let id = KeyId::from_generation(generation, u32::from_be_bytes(suffix))
        .map_err(|_| KeyringError::Unavailable)?;
    let mut material = zeroize::Zeroizing::new([0u8; 32]);
    rng.fill(&mut *material)
        .map_err(|_| KeyringError::Entropy)?;
    Ok(CacheEncryptionKey::new(
        CacheKeyRef {
            cache: CacheId(cache.into()),
            id,
            purpose,
        },
        state,
        material,
    ))
}

/// Keep the newest `retained - 1` decrypt page keys of each cache.
fn prune_decrypt(keys: &mut Vec<CacheEncryptionKey>, retained: usize) {
    let keep = retained.clamp(1, wire::MAX_RETAINED_PAGE_KEYS) - 1;
    let mut by_cache: BTreeMap<String, Vec<(u64, [u8; 16])>> = BTreeMap::new();
    for k in keys.iter().filter(|k| k.state == CacheKeyState::Decrypt) {
        by_cache
            .entry(k.key.cache.0.clone())
            .or_default()
            .push((k.key.id.generation().unwrap_or(0), k.key.id.0));
    }
    let mut drop = BTreeSet::new();
    for (cache, mut ids) in by_cache {
        // Newest first.
        ids.sort_unstable_by(|a, b| b.cmp(a));
        for (_, id) in ids.into_iter().skip(keep) {
            drop.insert((cache.clone(), id));
        }
    }
    if !drop.is_empty() {
        keys.retain(|k| {
            k.state != CacheKeyState::Decrypt
                || !drop.contains(&(k.key.cache.0.clone(), k.key.id.0))
        });
    }
}

/// Plan output.
pub struct Plan {
    pub durable: Durable,
    pub changed: bool,
    /// Offers refused because [`MAX_ROOTS`] was reached.
    pub refused_roots: usize,
}

/// Compute the next keyring. `catalog` must already be admitted.
#[cfg(test)]
pub fn plan(
    policy: &Policy,
    current: &Durable,
    catalog: &[CacheDefinition],
    offers: &[Offer],
    now: u64,
) -> Result<Plan, KeyringError> {
    plan_with(
        policy,
        current,
        Some(catalog),
        offers,
        now,
        &mut crate::entropy::OsEntropy,
    )
}

/// Like [`plan`], but `None` means the catalog is unknown, for example before
/// the ClusterCache watch synced or when it failed to build. Cache keys are
/// then neither added nor removed: an unknown catalog is not an empty one,
/// and dropping keys would give existing caches new keys later.
pub fn plan_with(
    policy: &Policy,
    current: &Durable,
    catalog: Option<&[CacheDefinition]>,
    offers: &[Offer],
    now: u64,
    rng: &mut dyn Entropy,
) -> Result<Plan, KeyringError> {
    let mut d = current.clone();
    // A key created now belongs to the next generation.
    let next = current.generation.checked_add(1);
    let creation = next.unwrap_or(0);

    // Remove keys of deleted caches.
    if let Some(catalog) = catalog {
        let wanted: BTreeSet<&str> = catalog.iter().map(|c| c.id.0.as_str()).collect();
        d.keys.retain(|k| wanted.contains(k.key.cache.0.as_str()));
    }

    // Add missing keys.
    for c in catalog.unwrap_or_default() {
        for p in PURPOSES {
            if !d
                .keys
                .iter()
                .any(|k| k.key.cache.0 == c.id.0 && k.key.purpose == p)
            {
                if creation == 0 {
                    return Err(KeyringError::Unavailable);
                }
                d.keys
                    .push(new_key(rng, &c.id.0, p, CacheKeyState::Active, creation)?);
            }
        }
    }

    if d.next_rotation == 0 {
        d.next_rotation = now + (policy.interval - policy.prepare_for).as_secs();
    }
    if d.activate_at != 0 && now >= d.activate_at {
        let prepared: BTreeSet<(String, u8)> = d
            .keys
            .iter()
            .filter(|k| k.state == CacheKeyState::Prepared)
            .map(|k| (k.key.cache.0.clone(), purpose_rank(k.key.purpose)))
            .collect();
        let replaced = |k: &CacheEncryptionKey| {
            k.state == CacheKeyState::Active
                && prepared.contains(&(k.key.cache.0.clone(), purpose_rank(k.key.purpose)))
        };
        // Old origin keys go; old page keys stay readable.
        d.keys
            .retain(|k| !(replaced(k) && k.key.purpose == CacheKeyPurpose::OriginCredentials));
        for k in d.keys.iter_mut() {
            if replaced(k) {
                k.state = CacheKeyState::Decrypt;
            }
        }
        for k in d.keys.iter_mut() {
            if k.state == CacheKeyState::Prepared {
                k.state = CacheKeyState::Active;
            }
        }
        d.activate_at = 0;
        d.next_rotation = now + (policy.interval - policy.prepare_for).as_secs();
    } else if d.activate_at == 0 && now >= d.next_rotation && !d.keys.is_empty() {
        if creation == 0 {
            return Err(KeyringError::Unavailable);
        }
        let mut prepared = Vec::new();
        for k in d.keys.iter().filter(|k| k.state == CacheKeyState::Active) {
            prepared.push(new_key(
                rng,
                &k.key.cache.0,
                k.key.purpose,
                CacheKeyState::Prepared,
                creation,
            )?);
        }
        d.keys.extend(prepared);
        d.activate_at = now + policy.prepare_for.as_secs();
    }
    prune_decrypt(&mut d.keys, policy.retained());
    sort_keys(&mut d.keys);

    // Roots.
    let keep_after_offer = (policy.lease_duration + policy.certificate_lifetime + SKEW).as_secs();
    let min_life = (policy.certificate_lifetime + SKEW).as_secs();
    d.retired.retain(|r| r.until > now);
    let mut refused = 0;
    for offer in offers {
        let encoded = STANDARD.encode(&offer.der);
        if let Some(r) = d.roots.iter_mut().find(|r| r.der == encoded) {
            // Extend with hysteresis so idle passes do not rewrite the
            // deadline: only when less than keep_after_offer remains, and
            // then by twice that, so a root is always retained at least
            // keep_after_offer past the last pass that saw it offered.
            if r.retain_until < (now + keep_after_offer).min(r.not_after) {
                r.retain_until = (now + 2 * keep_after_offer).min(r.not_after);
            }
            continue;
        }
        let hash = sha256(&offer.der);
        if d.retired.iter().any(|r| r.sha256 == hash)
            || (offer.seat as usize) >= crate::annotations::MAX_SEATS
        {
            continue;
        }
        let Ok(info) = inspect_root(&offer.der, now) else {
            continue;
        };
        if info.not_after < now + min_life {
            continue;
        }
        if d.roots.len() >= MAX_ROOTS {
            refused += 1;
            continue;
        }
        d.roots.push(Root {
            der: encoded,
            seat: offer.seat,
            holder: offer.holder.clone(),
            not_after: info.not_after,
            retain_until: (now + 2 * keep_after_offer).min(info.not_after),
        });
    }
    let mut kept = Vec::new();
    for r in std::mem::take(&mut d.roots) {
        if now >= r.retain_until || now >= r.not_after {
            let der = STANDARD.decode(&r.der).unwrap_or_default();
            if r.not_after > now {
                d.retired.push(Retired {
                    sha256: sha256(&der),
                    until: r.not_after,
                });
            }
        } else {
            kept.push(r);
        }
    }
    d.roots = kept;

    // Only these fields count as a change. Retain deadline extensions are
    // persisted but do not bump the bundle generation.
    let material_change = |a: &Durable, b: &Durable| {
        a.keys != b.keys
            || a.roots
                .iter()
                .map(|r| &r.der)
                .ne(b.roots.iter().map(|r| &r.der))
            || a.activate_at != b.activate_at
            || a.next_rotation != b.next_rotation
            || a.retired != b.retired
    };
    let changed = material_change(current, &d);
    if changed {
        let bundle_changed = current.keys != d.keys
            || current
                .roots
                .iter()
                .map(|r| &r.der)
                .ne(d.roots.iter().map(|r| &r.der));
        if bundle_changed {
            d.generation = next.ok_or(KeyringError::Unavailable)?;
        }
    }
    let changed = changed || current.roots != d.roots;
    Ok(Plan {
        durable: d,
        changed,
        refused_roots: refused,
    })
}

#[cfg(test)]
pub(crate) mod tests {
    use super::*;
    use crate::entropy::{Failing, OsEntropy};
    use racer_core::time::SplitMix64;

    const CLUSTER: &str = "11111111-1111-4111-8111-111111111111";
    const DAY: u64 = 86400;

    pub fn policy() -> Policy {
        Policy {
            interval: Duration::from_secs(DAY),
            prepare_for: Duration::from_secs(3600),
            certificate_lifetime: Duration::from_secs(DAY),
            lease_duration: Duration::from_secs(30),
            retained_page_keys: DEFAULT_RETAINED_PAGE_KEYS,
        }
    }

    pub fn root(days: u64) -> Vec<u8> {
        let key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut p = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        p.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Constrained(0));
        let now = std::time::SystemTime::now();
        p.not_before = (now - Duration::from_secs(60)).into();
        p.not_after = (now + Duration::from_secs(days * DAY)).into();
        p.self_signed(&key).unwrap().der().to_vec()
    }

    fn now() -> u64 {
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_secs()
    }

    fn cache(n: u8) -> CacheDefinition {
        let id = format!("{n:08x}-1111-4111-8111-111111111111");
        let (client, origin) = wire::canonical_socket_paths(&format!("c{n}")).unwrap();
        CacheDefinition {
            id: CacheId(id),
            name: format!("c{n}"),
            client_socket: client,
            priority: wire::DEFAULT_PRIORITY,
            mode: wire::CacheMode::Cached,
            origin: None,
            origin_socket: origin,
        }
    }

    #[test]
    fn initial_keys_rotation_and_activation() {
        let t = now();
        let offer = Offer {
            der: root(7),
            seat: 0,
            holder: "pod".into(),
        };
        let p = plan(
            &policy(),
            &Durable::default(),
            &[cache(1)],
            std::slice::from_ref(&offer),
            t,
        )
        .unwrap();
        assert!(p.changed);
        let d = p.durable;
        assert_eq!(d.generation, 1);
        assert_eq!(d.keys.len(), 2);
        d.validate().unwrap();
        assert!(d.root_committed(&offer.der));
        let b = d.bundle(CLUSTER).unwrap();
        assert_eq!(b.cache_keys.len(), 2);

        // Steady state: nothing changes.
        let p2 = plan(
            &policy(),
            &d,
            &[cache(1)],
            std::slice::from_ref(&offer),
            t + 10,
        )
        .unwrap();
        assert_eq!(p2.durable.generation, 1);
        assert_eq!(p2.durable.keys, d.keys);

        // Prepare.
        let t2 = d.next_rotation;
        let p3 = plan(
            &policy(),
            &p2.durable,
            &[cache(1)],
            std::slice::from_ref(&offer),
            t2,
        )
        .unwrap();
        assert_eq!(p3.durable.generation, 2);
        assert_eq!(p3.durable.keys.len(), 4);
        assert!(p3.durable.activate_at > 0);
        p3.durable.validate().unwrap();
        p3.durable.bundle(CLUSTER).unwrap();

        // Activate.
        let t3 = p3.durable.activate_at;
        let p4 = plan(&policy(), &p3.durable, &[cache(1)], &[offer], t3).unwrap();
        assert_eq!(p4.durable.generation, 3);
        // New active page and origin keys, and the old page key kept for reads.
        assert_eq!(p4.durable.keys.len(), 3);
        p4.durable.validate().unwrap();
        let old: Vec<_> = d.keys.iter().map(|k| k.key.id).collect();
        for k in p4.durable.keys.iter() {
            if old.contains(&k.key.id) {
                assert_eq!(k.state, CacheKeyState::Decrypt);
                assert_eq!(k.key.purpose, CacheKeyPurpose::Page);
            } else {
                assert_eq!(k.state, CacheKeyState::Active);
            }
        }
        let b = p4.durable.bundle(CLUSTER).unwrap();
        let encoded = wire::encode_bundle(&b).unwrap();
        assert!(wire::decode_bundle(&encoded).unwrap().cache_keys == b.cache_keys);
    }

    #[test]
    fn deleted_cache_drops_keys_and_generation_exhaustion_fails() {
        let t = now();
        let offer = Offer {
            der: root(7),
            seat: 0,
            holder: "p".into(),
        };
        let d = plan(
            &policy(),
            &Durable::default(),
            &[cache(1), cache(2)],
            std::slice::from_ref(&offer),
            t,
        )
        .unwrap()
        .durable;
        let d2 = plan(&policy(), &d, &[cache(2)], std::slice::from_ref(&offer), t)
            .unwrap()
            .durable;
        assert_eq!(d2.keys.len(), 2);
        assert_eq!(d2.generation, 2);
        let mut max = d2.clone();
        max.generation = u64::MAX;
        assert_eq!(
            plan(
                &policy(),
                &max,
                &[cache(2), cache(3)],
                std::slice::from_ref(&offer),
                t
            )
            .err(),
            Some(KeyringError::Unavailable)
        );
        // No change at exhaustion is fine.
        assert!(
            !plan(&policy(), &max, &[cache(2)], &[offer], t)
                .unwrap()
                .changed
        );
    }

    #[test]
    fn unknown_catalog_keeps_cache_keys() {
        // A new primary whose ClusterCache watch has not synced must not drop
        // keys, or existing caches would silently get new keys later.
        let t = now();
        let d = plan(
            &policy(),
            &Durable::default(),
            &[cache(1), cache(2)],
            &[],
            t,
        )
        .unwrap()
        .durable;
        let p = plan_with(&policy(), &d, None, &[], t, &mut OsEntropy).unwrap();
        assert_eq!(p.durable.keys, d.keys);
        assert_eq!(p.durable.generation, d.generation);
        assert!(!p.changed);
        // Rotation still proceeds on the existing keys.
        let p = plan_with(&policy(), &d, None, &[], d.next_rotation, &mut OsEntropy).unwrap();
        assert_eq!(p.durable.keys.len(), 8);
        assert!(d.keys.iter().all(|k| p.durable.keys.contains(k)));
        // An empty known catalog does drop them.
        assert!(
            plan_with(&policy(), &d, Some(&[]), &[], t, &mut OsEntropy)
                .unwrap()
                .durable
                .keys
                .is_empty()
        );
    }

    #[test]
    fn root_retention_and_retirement() {
        let t = now();
        let a = Offer {
            der: root(7),
            seat: 0,
            holder: "a".into(),
        };
        let b = Offer {
            der: root(7),
            seat: 1,
            holder: "b".into(),
        };
        let d = plan(
            &policy(),
            &Durable::default(),
            &[],
            &[a.clone(), b.clone()],
            t,
        )
        .unwrap()
        .durable;
        assert_eq!(d.roots.len(), 2);
        // b stops offering. It stays at least a lease plus a certificate
        // lifetime (new roots get twice that, see hysteresis in plan_with).
        let d = plan(&policy(), &d, &[], std::slice::from_ref(&a), t + 100)
            .unwrap()
            .durable;
        assert_eq!(d.roots.len(), 2);
        let later = t + 2 * (30 + DAY + 60) + 1;
        let p = plan(&policy(), &d, &[], std::slice::from_ref(&a), later).unwrap();
        assert!(p.changed);
        assert_eq!(p.durable.roots.len(), 1);
        assert_eq!(p.durable.retired.len(), 1);
        // A stale Lease cannot add it back.
        let again = plan(
            &policy(),
            &p.durable,
            &[],
            &[a.clone(), b.clone()],
            later + 1,
        )
        .unwrap()
        .durable;
        assert_eq!(again.roots.len(), 1);
        again.validate().unwrap();
    }

    #[test]
    fn idle_passes_do_not_change_and_retention_stays_ahead() {
        let t = now();
        let a = Offer {
            der: root(7),
            seat: 0,
            holder: "a".into(),
        };
        let pol = policy();
        let keep = (pol.lease_duration + pol.certificate_lifetime + SKEW).as_secs();
        let offers = std::slice::from_ref(&a);
        let mut d = plan(&pol, &Durable::default(), &[], offers, t)
            .unwrap()
            .durable;
        // (a) A second pass one second later is idle.
        let p = plan(&pol, &d, &[], offers, t + 1).unwrap();
        assert!(!p.changed);
        // (b) Retention never drops below keep_after_offer while offered,
        // and changes are rare.
        let mut changes = 0;
        let mut now_ = t;
        while now_ < t + 3 * keep {
            now_ += 10;
            let p = plan(&pol, &d, &[], offers, now_).unwrap();
            assert!(p.durable.roots[0].retain_until >= now_ + keep);
            if p.changed {
                changes += 1;
            }
            d = p.durable;
        }
        assert!(changes <= 4, "changes={changes}");
        // (c) Stops being offered: kept for keep_after_offer, then retired.
        let last = now_;
        let p = plan(&pol, &d, &[], &[], last + keep - 1).unwrap();
        assert_eq!(p.durable.roots.len(), 1);
        let p = plan(&pol, &p.durable, &[], &[], last + 2 * keep + 1).unwrap();
        assert!(p.changed);
        assert!(p.durable.roots.is_empty());
        assert_eq!(p.durable.retired.len(), 1);
    }

    #[test]
    fn short_or_invalid_roots_are_ignored_and_limit_refuses() {
        let t = now();
        let short = Offer {
            der: root(0),
            seat: 0,
            holder: "a".into(),
        };
        let junk = Offer {
            der: vec![1, 2, 3],
            seat: 0,
            holder: "a".into(),
        };
        let d = plan(&policy(), &Durable::default(), &[], &[short, junk], t).unwrap();
        assert!(d.durable.roots.is_empty());
        let offers: Vec<_> = (0..MAX_ROOTS + 2)
            .map(|i| Offer {
                der: root(7),
                seat: (i % 16) as u8,
                holder: "h".into(),
            })
            .collect();
        let p = plan(&policy(), &Durable::default(), &[], &offers, t).unwrap();
        assert_eq!(p.durable.roots.len(), MAX_ROOTS);
        assert_eq!(p.refused_roots, 2);
    }

    #[test]
    fn capacity_never_evicts() {
        let cap = catalog_capacity(DEFAULT_RETAINED_PAGE_KEYS);
        assert!(cap >= 900, "{cap}");
        assert!(catalog_capacity(1) > cap);
        let catalog: Vec<_> = (0..3).map(cache).collect();
        let mut d = Durable::default();
        assert_eq!(
            admit_catalog(&catalog, &d, DEFAULT_RETAINED_PAGE_KEYS)
                .unwrap()
                .len(),
            3
        );
        d.generation = 1;
        for c in &catalog {
            for p in PURPOSES {
                d.keys
                    .push(new_key(&mut OsEntropy, &c.id.0, p, CacheKeyState::Active, 1).unwrap());
            }
        }
        assert_eq!(d.keyed_caches().len(), 3);
    }

    /// At maximum catalog, with every cache holding all retained page keys
    /// and a prepared rotation, and every root slot used, the bundle and the
    /// state record fit and decode.
    #[test]
    fn maximum_catalog_fits_bundle_and_state_record() {
        let retained = DEFAULT_RETAINED_PAGE_KEYS;
        let cap = catalog_capacity(retained);
        let t = now();
        let mut big = Durable {
            generation: 20,
            ..Default::default()
        };
        let mut rng = SplitMix64::new(1);
        for i in 0..cap {
            let id = format!("{i:08x}-2222-4222-8222-222222222222");
            for g in 0..retained as u64 {
                let state = if g == 0 {
                    CacheKeyState::Active
                } else {
                    CacheKeyState::Decrypt
                };
                big.keys
                    .push(new_key(&mut rng, &id, CacheKeyPurpose::Page, state, 10 - g).unwrap());
            }
            big.keys.push(
                new_key(
                    &mut rng,
                    &id,
                    CacheKeyPurpose::Page,
                    CacheKeyState::Prepared,
                    20,
                )
                .unwrap(),
            );
            big.keys.push(
                new_key(
                    &mut rng,
                    &id,
                    CacheKeyPurpose::OriginCredentials,
                    CacheKeyState::Active,
                    10,
                )
                .unwrap(),
            );
            big.keys.push(
                new_key(
                    &mut rng,
                    &id,
                    CacheKeyPurpose::OriginCredentials,
                    CacheKeyState::Prepared,
                    20,
                )
                .unwrap(),
            );
        }
        assert_eq!(big.keys.len(), cap * keys_per_cache(retained));
        sort_keys(&mut big.keys);
        for i in 0..MAX_ROOTS {
            // Pad to the largest accepted root so the reservation is exercised.
            let mut der = root(7);
            der.resize(MAX_ROOT_DER_BYTES, 0);
            big.roots.push(Root {
                der: STANDARD.encode(&der),
                seat: (i % 16) as u8,
                holder: "11111111-1111-4111-8111-111111111111".into(),
                not_after: t + DAY * 7,
                retain_until: t + DAY,
            });
        }
        big.validate().unwrap();
        // Padded DER is not a certificate, so encode the wire key table and
        // envelope with real roots and account the padding separately.
        let mut wire_bundle = big.clone();
        for r in &mut wire_bundle.roots {
            r.der = STANDARD.encode(root(7));
        }
        let b = wire_bundle.bundle(CLUSTER).unwrap();
        let encoded = wire::encode_bundle(&b).unwrap();
        let real_roots: usize = wire_bundle.roots.iter().map(|r| r.der.len()).sum();
        let padded_roots: usize = big.roots.iter().map(|r| r.der.len()).sum();
        assert!(
            encoded.len() - real_roots + padded_roots <= wire::MAX_BUNDLE_BYTES,
            "{}",
            encoded.len()
        );
        let decoded = wire::decode_bundle(&encoded).unwrap();
        assert_eq!(decoded.cache_keys.len(), big.keys.len());
        let record = super::super::state::StateRecord {
            version: super::super::state::VERSION,
            cluster: CLUSTER.into(),
            term: super::super::state::Term {
                lease_uid: "l".into(),
                transitions: 1,
                holder: "h".into(),
            },
            writer_pod_uid: "p".into(),
            keyring: big.clone(),
            ..Default::default()
        };
        let state = super::super::state::encode(&record).unwrap();
        assert!(
            state.len() < super::super::state::MAX_BYTES,
            "{}",
            state.len()
        );
        let back = super::super::state::decode(CLUSTER, true, Some(&state))
            .unwrap()
            .unwrap();
        assert_eq!(back.keyring, big);
    }

    /// Pages stay readable for N - 1 rotations: each activation turns the old
    /// page key into a decrypt key and drops the oldest beyond N - 1.
    #[test]
    fn rotation_retains_page_keys() {
        let t = now();
        let pol = Policy {
            retained_page_keys: 3,
            ..policy()
        };
        let mut d = plan(&pol, &Durable::default(), &[cache(1)], &[], t)
            .unwrap()
            .durable;
        let first = d
            .keys
            .iter()
            .find(|k| k.key.purpose == CacheKeyPurpose::Page)
            .unwrap()
            .key
            .id;
        let readable = |d: &Durable, id: KeyId| d.keys.iter().any(|k| k.key.id == id);
        for rotation in 1..=3 {
            d = plan(&pol, &d, &[cache(1)], &[], d.next_rotation)
                .unwrap()
                .durable;
            d = plan(&pol, &d, &[cache(1)], &[], d.activate_at)
                .unwrap()
                .durable;
            d.validate().unwrap();
            let decrypt = d
                .keys
                .iter()
                .filter(|k| k.state == CacheKeyState::Decrypt)
                .count();
            assert_eq!(decrypt, rotation.min(2), "rotation {rotation}");
            assert_eq!(readable(&d, first), rotation < 3, "rotation {rotation}");
            assert_eq!(
                d.keys
                    .iter()
                    .filter(|k| k.key.purpose == CacheKeyPurpose::OriginCredentials)
                    .count(),
                1
            );
        }
        // Lowering N prunes on the next pass.
        let one = Policy {
            retained_page_keys: 1,
            ..policy()
        };
        let p = plan(&one, &d, &[cache(1)], &[], t).unwrap();
        assert!(p.changed);
        assert!(
            p.durable
                .keys
                .iter()
                .all(|k| k.state == CacheKeyState::Active)
        );
    }

    #[test]
    fn oversized_root_is_refused() {
        let t = now();
        let mut der = root(7);
        der.resize(MAX_ROOT_DER_BYTES + 1, 0);
        assert!(inspect_root(&der, t).is_err());
    }

    #[test]
    fn validate_rejects_bad_state() {
        const C: &str = "11111111-1111-4111-8111-111111111111";
        let key = |state, generation| {
            new_key(&mut OsEntropy, C, CacheKeyPurpose::Page, state, generation).unwrap()
        };
        let mut d = Durable {
            generation: 1,
            ..Default::default()
        };
        d.keys.push(key(CacheKeyState::Active, 1));
        d.validate().unwrap();
        let mut two = d.clone();
        two.keys.push(key(CacheKeyState::Active, 1));
        assert!(two.validate().is_err());
        let mut future = d.clone();
        future.keys[0] = key(CacheKeyState::Active, 5);
        assert!(future.validate().is_err());
        let mut origin_decrypt = d.clone();
        origin_decrypt.keys.push(
            new_key(
                &mut OsEntropy,
                C,
                CacheKeyPurpose::OriginCredentials,
                CacheKeyState::Active,
                1,
            )
            .unwrap(),
        );
        origin_decrypt.keys.push(
            new_key(
                &mut OsEntropy,
                C,
                CacheKeyPurpose::OriginCredentials,
                CacheKeyState::Decrypt,
                1,
            )
            .unwrap(),
        );
        assert!(origin_decrypt.validate().is_err());
        // Debug never shows material, and the record round-trips.
        let debug = format!("{d:?}");
        assert!(debug.contains("Active") && !debug.contains("material"));
        let json = serde_json::to_string(&d).unwrap();
        assert_eq!(serde_json::from_str::<Durable>(&json).unwrap(), d);
    }

    #[test]
    fn plan_uses_injected_entropy() {
        let catalog: Vec<_> = (0..2).map(cache).collect();
        let d = Durable::default();
        let t = now();
        let a = plan_with(
            &policy(),
            &d,
            Some(&catalog),
            &[],
            t,
            &mut SplitMix64::new(5),
        )
        .unwrap();
        let b = plan_with(
            &policy(),
            &d,
            Some(&catalog),
            &[],
            t,
            &mut SplitMix64::new(5),
        )
        .unwrap();
        assert!(a.changed);
        assert_eq!(
            serde_json::to_vec(&a.durable).unwrap(),
            serde_json::to_vec(&b.durable).unwrap()
        );
        let c = plan_with(
            &policy(),
            &d,
            Some(&catalog),
            &[],
            t,
            &mut SplitMix64::new(6),
        )
        .unwrap();
        assert_ne!(
            serde_json::to_vec(&a.durable).unwrap(),
            serde_json::to_vec(&c.durable).unwrap()
        );
        assert_eq!(
            plan_with(&policy(), &d, Some(&catalog), &[], t, &mut Failing).err(),
            Some(KeyringError::Entropy)
        );
    }
}
