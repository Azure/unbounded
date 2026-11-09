//! Durable keyring: cache keys, their rotation, and peer trust roots.
//!
//! Cache keys follow the removed Go controller:
//! each cache has one active page key and one active origin credentials key.
//! A rotation first adds prepared keys, then activates them after
//! `prepare_for`. Caches are never evicted to make room. Every change gets a
//! new bundle generation; exhausted generations fail.
//!
//! Each seat offers an issuer root, reused across claims by the same process.
//! A root is kept while a seat offers it, and for one Lease duration plus one
//! certificate lifetime after that, capped at its NotAfter. A root removed
//! that way is listed as retired until its NotAfter so a stale Lease
//! annotation can never add it back.
//!
//! Every process start offers a new root, so restarts can fill all
//! [`MAX_ROOTS`] slots with roots no seat offers any more. A new offer then
//! evicts the unoffered root with the earliest retain deadline, which is the
//! one offered longest ago. Leaves that root issued stop verifying before
//! they expire: a live seat outranks them. Evicted roots are not retired,
//! because the seat that offered one may still hold its issuer and offer it
//! again. Eviction waits for a pass that read every seat Lease, since an
//! unread seat may still offer its root.

use crate::entropy::Entropy;
use base64::{Engine, engine::general_purpose::STANDARD};
use racer_control_wire::{
    self as wire, BundleGeneration, CacheDefinition, CacheEncryptionKey, CacheId, CacheKeyPurpose,
    CacheKeyRef, CacheKeyState, ClusterId, KeyId, KeyringBundle,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use std::{collections::BTreeMap, time::Duration};
use zeroize::Zeroize;

/// Most trust roots in one bundle.
pub const MAX_ROOTS: usize = 32;

/// DER bytes reserved per root when computing catalog capacity.
pub const RESERVED_ROOT_BYTES: usize = 1024;

/// Upper bound of one encoded cache key in a bundle.
const KEY_COST: usize = 192;

/// Upper bound of the bundle envelope without roots and keys.
const ENVELOPE: usize = 512;

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

/// One stored cache key.
#[derive(Clone, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Key {
    pub cache: String,
    /// Hex of the 16 byte ID.
    pub id: String,
    /// `page` or `origin_credentials`.
    pub purpose: String,
    /// `prepared` or `active`.
    pub state: String,
    /// Base64 of 32 bytes.
    pub material: String,
}

impl std::fmt::Debug for Key {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "Key({}/{}/{}/{})",
            self.cache, self.purpose, self.id, self.state
        )
    }
}

impl Drop for Key {
    fn drop(&mut self) {
        self.material.zeroize();
    }
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
    pub keys: Vec<Key>,
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

/// Rotation timing.
#[derive(Clone, Copy, Debug)]
pub struct Policy {
    pub interval: Duration,
    pub prepare_for: Duration,
    /// Longest certificate any seat issues.
    pub certificate_lifetime: Duration,
    pub lease_duration: Duration,
}

impl From<crate::config::Rotation> for Policy {
    fn from(r: crate::config::Rotation) -> Self {
        Self {
            interval: r.interval,
            prepare_for: r.prepare_for,
            certificate_lifetime: wire::CERTIFICATE_LIFETIME,
            lease_duration: crate::limits::LEASE_DURATION,
        }
    }
}

/// SHA-256 hex.
pub fn sha256(b: &[u8]) -> String {
    format!("{:x}", Sha256::digest(b))
}

fn purpose(p: &str) -> Result<CacheKeyPurpose, KeyringError> {
    match p {
        "page" => Ok(CacheKeyPurpose::Page),
        "origin_credentials" => Ok(CacheKeyPurpose::OriginCredentials),
        _ => Err(KeyringError::Invalid),
    }
}

const PURPOSES: [&str; 2] = ["page", "origin_credentials"];

fn decode_hex16(s: &str) -> Result<[u8; 16], KeyringError> {
    if s.len() != 32 {
        return Err(KeyringError::Invalid);
    }
    let mut out = [0u8; 16];
    for (i, o) in out.iter_mut().enumerate() {
        *o = u8::from_str_radix(&s[2 * i..2 * i + 2], 16).map_err(|_| KeyringError::Invalid)?;
    }
    Ok(out)
}

/// Facts of a root certificate needed here.
pub struct RootInfo {
    pub not_after: u64,
}

/// Check a root: CA, Ed25519, self-signed, valid now.
pub fn inspect_root(der: &[u8], now: u64) -> Result<RootInfo, KeyringError> {
    if der.is_empty() || der.len() > 16 * 1024 {
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
        let mut active: BTreeMap<(String, String), usize> = BTreeMap::new();
        let mut ids = std::collections::HashSet::new();
        for k in &self.keys {
            if !wire::valid_uuid(&k.cache) || !ids.insert(k.id.clone()) {
                return Err(KeyringError::Invalid);
            }
            purpose(&k.purpose)?;
            let id = KeyId(decode_hex16(&k.id)?);
            if id.generation().is_none_or(|g| g > self.generation) {
                return Err(KeyringError::Invalid);
            }
            let m = STANDARD
                .decode(&k.material)
                .map_err(|_| KeyringError::Invalid)?;
            if m.len() != 32 {
                return Err(KeyringError::Invalid);
            }
            match k.state.as_str() {
                "active" => {
                    *active
                        .entry((k.cache.clone(), k.purpose.clone()))
                        .or_default() += 1
                }
                "prepared" => {
                    active
                        .entry((k.cache.clone(), k.purpose.clone()))
                        .or_default();
                }
                _ => return Err(KeyringError::Invalid),
            }
        }
        if active.values().any(|n| *n != 1) || self.roots.len() > MAX_ROOTS {
            return Err(KeyringError::Invalid);
        }
        let mut hashes = std::collections::HashSet::new();
        for r in &self.roots {
            let der = STANDARD.decode(&r.der).map_err(|_| KeyringError::Invalid)?;
            if !hashes.insert(sha256(&der)) || r.seat as usize >= crate::annotations::MAX_SEATS {
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
        for k in self.keys.iter().filter(|k| k.state == "active") {
            *n.entry(k.cache.clone()).or_default() += 1;
        }
        n.into_iter().map(|(k, v)| (k, v == 2)).collect()
    }

    /// Wire bundle for clients. Needs at least one root.
    pub fn bundle(&self, cluster: &str) -> Result<KeyringBundle, KeyringError> {
        let mut keys = Vec::with_capacity(self.keys.len());
        for k in &self.keys {
            let mut m = zeroize::Zeroizing::new([0u8; 32]);
            let decoded = zeroize::Zeroizing::new(
                STANDARD
                    .decode(&k.material)
                    .map_err(|_| KeyringError::Invalid)?,
            );
            if decoded.len() != 32 {
                return Err(KeyringError::Invalid);
            }
            m.copy_from_slice(&decoded);
            keys.push(CacheEncryptionKey::new(
                CacheKeyRef {
                    cache: CacheId(k.cache.clone()),
                    id: KeyId(decode_hex16(&k.id)?),
                    purpose: purpose(&k.purpose)?,
                },
                if k.state == "active" {
                    CacheKeyState::Active
                } else {
                    CacheKeyState::Prepared
                },
                m,
            ));
        }
        let b = KeyringBundle {
            schema_version: wire::SCHEMA_VERSION,
            cluster: ClusterId(cluster.into()),
            generation: BundleGeneration(self.generation),
            peer_trust_roots: self.root_ders(),
            cache_keys: keys,
        };
        wire::encode_bundle(&b).map_err(|_| KeyringError::Invalid)?;
        Ok(b)
    }
}

/// Number of caches that always fit, with every cache in prepared overlap and
/// [`MAX_ROOTS`] roots, under the bundle byte limit.
pub fn catalog_capacity() -> usize {
    let root_cost = RESERVED_ROOT_BYTES.div_ceil(3) * 4 + 3;
    let available = wire::MAX_BUNDLE_BYTES - ENVELOPE - MAX_ROOTS * root_cost;
    // Two purposes, each active plus prepared.
    available / (4 * KEY_COST)
}

/// Keep keyed caches, then fill free slots in UID order. Never evicts.
pub fn admit_catalog(
    catalog: &[CacheDefinition],
    durable: &Durable,
) -> Result<Vec<CacheDefinition>, KeyringError> {
    let capacity = catalog_capacity();
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
    purpose: &str,
    state: &str,
    generation: u64,
) -> Result<Key, KeyringError> {
    let mut suffix = [0u8; 4];
    rng.fill(&mut suffix).map_err(|_| KeyringError::Entropy)?;
    let id = KeyId::from_generation(generation, u32::from_be_bytes(suffix))
        .map_err(|_| KeyringError::Unavailable)?;
    let mut material = zeroize::Zeroizing::new([0u8; 32]);
    rng.fill(&mut *material)
        .map_err(|_| KeyringError::Entropy)?;
    Ok(Key {
        cache: cache.into(),
        id: id.0.iter().map(|b| format!("{b:02x}")).collect(),
        purpose: purpose.into(),
        state: state.into(),
        material: STANDARD.encode(*material),
    })
}

/// Plan output.
pub struct Plan {
    pub durable: Durable,
    pub changed: bool,
    /// Offers refused because [`MAX_ROOTS`] was reached by offered roots.
    pub refused_roots: usize,
    /// Unoffered roots removed early to admit an offer when slots were full.
    pub evicted_roots: usize,
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
        true,
        now,
        &mut crate::entropy::OsEntropy,
    )
}

/// Removes roots past their retain deadline or NotAfter, except those `skip`
/// keeps, and retires each one until its NotAfter.
fn expire_roots(d: &mut Durable, now: u64, skip: impl Fn(&Root) -> bool) {
    let mut kept = Vec::new();
    for r in std::mem::take(&mut d.roots) {
        if !skip(&r) && (now >= r.retain_until || now >= r.not_after) {
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
}

/// Like [`plan`], but `None` means the catalog is unknown, for example before
/// the ClusterCache watch synced or when it failed to build. Cache keys are
/// then neither added nor removed: an unknown catalog is not an empty one,
/// and dropping keys would give existing caches new keys later.
///
/// `evict` is false when `offers` may be incomplete, for example after a
/// failed seat Lease read: a root missing from `offers` may then belong to a
/// live seat, so full slots refuse new offers instead of evicting.
pub fn plan_with(
    policy: &Policy,
    current: &Durable,
    catalog: Option<&[CacheDefinition]>,
    offers: &[Offer],
    evict: bool,
    now: u64,
    rng: &mut dyn Entropy,
) -> Result<Plan, KeyringError> {
    let mut d = current.clone();
    // A key created now belongs to the next generation.
    let next = current.generation.checked_add(1);
    let creation = next.unwrap_or(0);

    // Remove keys of deleted caches.
    if let Some(catalog) = catalog {
        let wanted: std::collections::BTreeSet<&str> =
            catalog.iter().map(|c| c.id.0.as_str()).collect();
        d.keys.retain(|k| wanted.contains(k.cache.as_str()));
    }

    // Add missing keys.
    for c in catalog.unwrap_or_default() {
        for p in PURPOSES {
            if !d.keys.iter().any(|k| k.cache == c.id.0 && k.purpose == p) {
                if creation == 0 {
                    return Err(KeyringError::Unavailable);
                }
                d.keys.push(new_key(rng, &c.id.0, p, "active", creation)?);
            }
        }
    }

    if d.next_rotation == 0 {
        d.next_rotation = now + (policy.interval - policy.prepare_for).as_secs();
    }
    if d.activate_at != 0 && now >= d.activate_at {
        let prepared: std::collections::BTreeSet<(String, String)> = d
            .keys
            .iter()
            .filter(|k| k.state == "prepared")
            .map(|k| (k.cache.clone(), k.purpose.clone()))
            .collect();
        d.keys.retain(|k| {
            !(k.state == "active" && prepared.contains(&(k.cache.clone(), k.purpose.clone())))
        });
        for k in &mut d.keys {
            if k.state == "prepared" {
                k.state = "active".into();
            }
        }
        d.activate_at = 0;
        d.next_rotation = now + (policy.interval - policy.prepare_for).as_secs();
    } else if d.activate_at == 0 && now >= d.next_rotation && !d.keys.is_empty() {
        if creation == 0 {
            return Err(KeyringError::Unavailable);
        }
        let mut prepared = Vec::new();
        for k in d.keys.iter().filter(|k| k.state == "active") {
            prepared.push(new_key(rng, &k.cache, &k.purpose, "prepared", creation)?);
        }
        d.keys.extend(prepared);
        d.activate_at = now + policy.prepare_for.as_secs();
    }

    // Roots.
    let keep_after_offer = (policy.lease_duration + policy.certificate_lifetime + SKEW).as_secs();
    let min_life = (policy.certificate_lifetime + SKEW).as_secs();
    d.retired.retain(|r| r.until > now);
    let offered: std::collections::HashSet<String> =
        offers.iter().map(|o| STANDARD.encode(&o.der)).collect();
    // Expire unoffered roots first so eviction only happens when expiry
    // alone does not free a slot.
    expire_roots(&mut d, now, |r| offered.contains(&r.der));
    let mut refused = 0;
    let mut evicted = 0;
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
            // A live seat outranks a root no seat offers any more. Without
            // this, a crash loop that claims seats from new processes, each
            // with a new issuer, fills every slot with dead roots and leaves
            // all seats unready until those roots expire. Evict the unoffered
            // root closest to its deadline. It is not retired, so a holder
            // that offers it again is admitted like a new one.
            let victim = evict
                .then(|| {
                    d.roots
                        .iter()
                        .enumerate()
                        .filter(|(_, r)| !offered.contains(&r.der))
                        .min_by_key(|(_, r)| (r.retain_until, r.not_after))
                        .map(|(i, _)| i)
                })
                .flatten();
            let Some(victim) = victim else {
                refused += 1;
                continue;
            };
            d.roots.remove(victim);
            evicted += 1;
        }
        d.roots.push(Root {
            der: encoded,
            seat: offer.seat,
            holder: offer.holder.clone(),
            not_after: info.not_after,
            retain_until: (now + 2 * keep_after_offer).min(info.not_after),
        });
    }
    expire_roots(&mut d, now, |_| false);

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
        evicted_roots: evicted,
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
        assert_eq!(p4.durable.keys.len(), 2);
        assert!(p4.durable.keys.iter().all(|k| k.state == "active"));
        let old: Vec<_> = d.keys.iter().map(|k| k.id.clone()).collect();
        assert!(p4.durable.keys.iter().all(|k| !old.contains(&k.id)));
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
        let p = plan_with(&policy(), &d, None, &[], true, t, &mut OsEntropy).unwrap();
        assert_eq!(p.durable.keys, d.keys);
        assert_eq!(p.durable.generation, d.generation);
        assert!(!p.changed);
        // Rotation still proceeds on the existing keys.
        let p = plan_with(
            &policy(),
            &d,
            None,
            &[],
            true,
            d.next_rotation,
            &mut OsEntropy,
        )
        .unwrap();
        assert_eq!(p.durable.keys.len(), 8);
        assert!(d.keys.iter().all(|k| p.durable.keys.contains(k)));
        // An empty known catalog does drop them.
        assert!(
            plan_with(&policy(), &d, Some(&[]), &[], true, t, &mut OsEntropy)
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

    /// Fill every root slot with roots that pods which have since exited
    /// offered: `retain_until` rises with the index.
    fn full_of_departed_roots(t: u64) -> Durable {
        let mut d = Durable::default();
        for i in 0..MAX_ROOTS {
            d.roots.push(Root {
                der: STANDARD.encode(root(7)),
                seat: (i % crate::annotations::MAX_SEATS) as u8,
                holder: format!("departed-{i}"),
                not_after: t + 7 * DAY,
                retain_until: t + DAY + i as u64,
            });
        }
        d.validate().unwrap();
        d
    }

    fn offers(n: usize) -> Vec<Offer> {
        (0..n)
            .map(|i| Offer {
                der: root(7),
                seat: i as u8,
                holder: format!("live-{i}"),
            })
            .collect()
    }

    #[test]
    fn full_slots_evict_unoffered_roots_for_live_offers() {
        let t = now();
        let d = full_of_departed_roots(t);
        let live = offers(3);
        let p = plan(&policy(), &d, &[], &live, t).unwrap();
        assert_eq!(p.refused_roots, 0);
        assert_eq!(p.evicted_roots, 3);
        assert!(p.changed);
        assert_eq!(p.durable.generation, d.generation + 1);
        assert_eq!(p.durable.roots.len(), MAX_ROOTS);
        p.durable.validate().unwrap();
        for o in &live {
            assert!(p.durable.root_committed(&o.der));
        }
        // The roots closest to their deadline went first, and are not retired.
        for r in &d.roots[..3] {
            assert!(!p.durable.roots.contains(r));
        }
        assert!(p.durable.roots.iter().any(|r| r == &d.roots[3]));
        assert!(p.durable.retired.is_empty());

        // Admitted roots stay; a later pass with the same offers is idle.
        let again = plan(&policy(), &p.durable, &[], &live, t + 60).unwrap();
        assert_eq!(again.evicted_roots, 0);
        assert_eq!(again.refused_roots, 0);
        assert!(!again.changed);
    }

    #[test]
    fn expired_roots_free_slots_before_eviction() {
        let t = now();
        let mut d = full_of_departed_roots(t);
        d.roots[10].retain_until = t;
        d.roots[20].retain_until = t - 1;
        let live = offers(3);
        let p = plan(&policy(), &d, &[], &live, t).unwrap();
        // Expiry frees two slots, so only one root is evicted.
        assert_eq!(p.evicted_roots, 1);
        assert_eq!(p.refused_roots, 0);
        assert_eq!(p.durable.roots.len(), MAX_ROOTS);
        assert_eq!(p.durable.retired.len(), 2);
        p.durable.validate().unwrap();
        for o in &live {
            assert!(p.durable.root_committed(&o.der));
        }
        assert!(!p.durable.roots.contains(&d.roots[0]));
        assert!(p.durable.roots.contains(&d.roots[1]));

        // Expiry alone is enough when it frees a slot per offer.
        let p = plan(&policy(), &d, &[], &live[..2], t).unwrap();
        assert_eq!(p.evicted_roots, 0);
        assert!(p.durable.roots.contains(&d.roots[0]));
    }

    #[test]
    fn incomplete_seat_reads_defer_eviction() {
        let t = now();
        let d = full_of_departed_roots(t);
        let live = offers(2);
        let p = plan_with(&policy(), &d, Some(&[]), &live, false, t, &mut OsEntropy).unwrap();
        assert_eq!(p.evicted_roots, 0);
        assert_eq!(p.refused_roots, 2);
        assert_eq!(p.durable.roots, d.roots);
        for o in &live {
            assert!(!p.durable.root_committed(&o.der));
        }
    }

    #[test]
    fn offered_roots_are_never_evicted() {
        let t = now();
        let mut d = full_of_departed_roots(t);
        // Every slot is offered again; one more offer does not fit.
        let mut live: Vec<_> = d
            .roots
            .iter()
            .map(|r| Offer {
                der: STANDARD.decode(&r.der).unwrap(),
                seat: r.seat,
                holder: r.holder.clone(),
            })
            .collect();
        live.extend(offers(1));
        let p = plan(&policy(), &d, &[], &live, t).unwrap();
        assert_eq!(p.evicted_roots, 0);
        assert_eq!(p.refused_roots, 1);
        assert!(!p.durable.root_committed(&live[MAX_ROOTS].der));

        // Only the unoffered root is evicted, even though it is not the
        // one closest to its deadline.
        d.roots[MAX_ROOTS - 1].retain_until = t + 2 * DAY;
        let newest = STANDARD.decode(&d.roots[MAX_ROOTS - 1].der).unwrap();
        live.remove(MAX_ROOTS - 1);
        let p = plan(&policy(), &d, &[], &live, t).unwrap();
        assert_eq!(p.evicted_roots, 1);
        assert_eq!(p.refused_roots, 0);
        assert!(!p.durable.root_committed(&newest));
        assert!(p.durable.root_committed(&live[MAX_ROOTS - 1].der));
    }

    #[test]
    fn evicted_root_is_admitted_when_offered_again() {
        let t = now();
        let d = full_of_departed_roots(t);
        let evicted = STANDARD.decode(&d.roots[0].der).unwrap();
        let p = plan(&policy(), &d, &[], &offers(1), t).unwrap();
        assert_eq!(p.evicted_roots, 1);
        assert!(!p.durable.root_committed(&evicted));
        let back = Offer {
            der: evicted.clone(),
            seat: 0,
            holder: "departed-0".into(),
        };
        let p = plan(&policy(), &p.durable, &[], &[back], t + 60).unwrap();
        assert_eq!(p.evicted_roots, 1);
        assert_eq!(p.refused_roots, 0);
        assert!(p.durable.root_committed(&evicted));
    }

    #[test]
    fn capacity_never_evicts() {
        let cap = catalog_capacity();
        assert!(cap > 100);
        let catalog: Vec<_> = (0..3).map(cache).collect();
        let mut d = Durable::default();
        assert_eq!(admit_catalog(&catalog, &d).unwrap().len(), 3);
        d.generation = 1;
        for c in &catalog {
            for p in PURPOSES {
                d.keys
                    .push(new_key(&mut OsEntropy, &c.id.0, p, "active", 1).unwrap());
            }
        }
        assert_eq!(d.keyed_caches().len(), 3);
        // A full bundle with every cache in overlap still encodes.
        let t = now();
        let mut big = Durable {
            generation: 2,
            ..Default::default()
        };
        for i in 0..cap {
            let id = format!("{i:08x}-2222-4222-8222-222222222222");
            for p in PURPOSES {
                big.keys
                    .push(new_key(&mut OsEntropy, &id, p, "active", 1).unwrap());
                big.keys
                    .push(new_key(&mut OsEntropy, &id, p, "prepared", 2).unwrap());
            }
        }
        for i in 0..MAX_ROOTS {
            let der = root(7);
            big.roots.push(Root {
                der: STANDARD.encode(&der),
                seat: (i % 16) as u8,
                holder: "h".into(),
                not_after: t + DAY * 7,
                retain_until: t + DAY,
            });
        }
        big.validate().unwrap();
        big.bundle(CLUSTER).unwrap();
        let state = serde_json::to_vec(&big).unwrap();
        assert!(
            state.len() < super::super::state::MAX_BYTES,
            "{}",
            state.len()
        );
    }

    #[test]
    fn validate_rejects_bad_state() {
        let mut d = Durable {
            generation: 1,
            ..Default::default()
        };
        d.keys.push(
            new_key(
                &mut OsEntropy,
                "11111111-1111-4111-8111-111111111111",
                "page",
                "active",
                1,
            )
            .unwrap(),
        );
        d.validate().unwrap();
        let mut two = d.clone();
        two.keys.push(
            new_key(
                &mut OsEntropy,
                "11111111-1111-4111-8111-111111111111",
                "page",
                "active",
                1,
            )
            .unwrap(),
        );
        assert!(two.validate().is_err());
        let mut future = d.clone();
        future.keys[0] = new_key(
            &mut OsEntropy,
            "11111111-1111-4111-8111-111111111111",
            "page",
            "active",
            5,
        )
        .unwrap();
        assert!(future.validate().is_err());
        assert!(
            format!("{:?}", d.keys[0])
                .find(&d.keys[0].material)
                .is_none()
        );
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
            true,
            t,
            &mut SplitMix64::new(5),
        )
        .unwrap();
        let b = plan_with(
            &policy(),
            &d,
            Some(&catalog),
            &[],
            true,
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
            true,
            t,
            &mut SplitMix64::new(6),
        )
        .unwrap();
        assert_ne!(
            serde_json::to_vec(&a.durable).unwrap(),
            serde_json::to_vec(&c.durable).unwrap()
        );
        assert_eq!(
            plan_with(&policy(), &d, Some(&catalog), &[], true, t, &mut Failing).err(),
            Some(KeyringError::Entropy)
        );
    }
}
