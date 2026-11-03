//! Publication counters, encoding, deltas, and follower verification.
//!
//! Counter rules match the old controller: changed content takes the next
//! sequence, and changed membership also takes the next membership version.
//! The same counters never name different content, so a new primary whose
//! rebuilt content differs from the committed hash always takes new counters.

use super::pools::{self, PoolPlan};
use racer_control_wire::{
    self as wire, CacheDefinition, ClusterId, Member, MembershipVersion, Publication,
    PublicationSequence,
};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use std::{collections::BTreeMap, sync::Arc};

/// Maximum delta size, as in the old controller.
pub const MAX_DELTA_BYTES: usize = 4 * 1024 * 1024;

/// Committed counters and hashes.
#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct Record {
    pub sequence: u64,
    pub membership_version: u64,
    pub content_hash: String,
    pub membership_hash: String,
}

/// A candidate publication.
pub struct Prepared {
    pub record: Record,
    pub publication: Publication,
    pub encoded: Vec<u8>,
}

/// Content and membership hashes.
pub fn hashes(p: &Publication) -> Result<(String, String), wire::Error> {
    let (content, membership) = wire::canonical_content(p)?;
    Ok((
        format!("{:x}", Sha256::digest(content)),
        format!("{:x}", Sha256::digest(membership)),
    ))
}

/// Build the next publication from `prev` counters. `pools` name member
/// Nodes by UID; non-members are left out of each pool.
pub fn prepare(
    prev: &Record,
    cluster: &str,
    members: impl IntoIterator<Item = Member>,
    caches: Vec<CacheDefinition>,
    pools: &[PoolPlan],
) -> Result<Prepared, wire::Error> {
    let mut p = Publication {
        schema_version: wire::SCHEMA_VERSION,
        cluster: ClusterId(cluster.into()),
        sequence: PublicationSequence(1),
        membership_version: MembershipVersion(1),
        members: members.into_iter().collect(),
        caches,
        pools: Vec::new(),
    };
    p.members.sort_by(|a, b| a.node.cmp(&b.node));
    let nodes: Vec<&str> = p.members.iter().map(|m| m.node.0.as_str()).collect();
    p.pools = pools::resolve(pools, &nodes);
    let (content, membership) = hashes(&p)?;
    let mut record = prev.clone();
    if content != prev.content_hash {
        record.sequence = prev
            .sequence
            .checked_add(1)
            .ok_or(wire::Error::Overloaded)?;
    }
    if membership != prev.membership_hash {
        record.membership_version = prev
            .membership_version
            .checked_add(1)
            .ok_or(wire::Error::Overloaded)?;
    }
    record.content_hash = content;
    record.membership_hash = membership;
    p.sequence = PublicationSequence(record.sequence);
    p.membership_version = MembershipVersion(record.membership_version);
    let encoded = wire::encode_publication(&p)?;
    Ok(Prepared {
        record,
        publication: p,
        encoded,
    })
}

/// Members by Node UID, and the cache and pool lists, as encoded.
type Parts = (Value, Value, BTreeMap<String, Value>);

fn members_json(p: &Publication) -> Result<Parts, wire::Error> {
    let mut v: Value = serde_json::from_slice(&wire::encode_publication(p)?)
        .map_err(|_| wire::Error::InvalidRequest)?;
    let members = match v["members"].take() {
        Value::Array(a) => a,
        _ => return Err(wire::Error::InvalidRequest),
    };
    let map = members
        .into_iter()
        .map(|m| (m["node"].as_str().unwrap_or_default().to_owned(), m))
        .collect();
    Ok((v["caches"].take(), v["pools"].take(), map))
}

/// Delta from `base` to `next`, in the old controller's format.
pub fn encode_delta(base: &Publication, next: &Publication) -> Result<Vec<u8>, wire::Error> {
    let (_, _, old) = members_json(base)?;
    let (caches, pools, new) = members_json(next)?;
    let upsert: Vec<&Value> = new
        .iter()
        .filter(|(k, v)| old.get(*k) != Some(v))
        .map(|(_, v)| v)
        .collect();
    let remove: Vec<&String> = old.keys().filter(|k| !new.contains_key(*k)).collect();
    let d = json!({
        "delta_version": wire::DELTA_VERSION,
        "cluster": next.cluster.0,
        "base_sequence": base.sequence.0.to_string(),
        "base_hash": wire::content_hash(base)?,
        "sequence": next.sequence.0.to_string(),
        "membership_version": next.membership_version.0.to_string(),
        "content_hash": wire::content_hash(next)?,
        "upsert_members": upsert,
        "remove_members": remove,
        "caches": caches,
        "pools": pools,
    });
    let b = serde_json::to_vec(&d).map_err(|_| wire::Error::InvalidRequest)?;
    if b.len() > MAX_DELTA_BYTES {
        return Err(wire::Error::Overloaded);
    }
    Ok(b)
}

/// An immutable committed image with an optional delta from the previous one.
#[derive(Clone)]
pub struct Image {
    pub record: Record,
    pub publication: Arc<Publication>,
    pub encoded: Arc<Vec<u8>>,
    /// `(base sequence, base content hash, delta bytes)`.
    pub delta: Option<(u64, String, Arc<Vec<u8>>)>,
}

impl Image {
    /// Build an image, with a delta from `previous` when it is smaller.
    pub fn new(
        record: Record,
        publication: Publication,
        encoded: Vec<u8>,
        previous: Option<&Image>,
    ) -> Self {
        let delta = previous
            .filter(|p| p.record.sequence < record.sequence)
            .and_then(|p| {
                let d = encode_delta(&p.publication, &publication).ok()?;
                (d.len() < encoded.len()).then(|| {
                    (
                        p.record.sequence,
                        p.record.content_hash.clone(),
                        Arc::new(d),
                    )
                })
            });
        Self {
            record,
            publication: Arc::new(publication),
            encoded: Arc::new(encoded),
            delta,
        }
    }

    /// Delta for the exact base, else the full image.
    pub fn for_base(&self, after: Option<u64>, base_hash: Option<&str>) -> Arc<Vec<u8>> {
        use crate::step::admission::{Body, body_for};
        let base = self.delta.as_ref().map(|(s, h, _)| (*s, h.as_str()));
        match (body_for(base, after, base_hash), &self.delta) {
            (Body::Delta, Some((_, _, d))) => d.clone(),
            _ => self.encoded.clone(),
        }
    }
}

/// Verify an image copied from the primary against the committed record.
pub fn verify_image(
    bytes: &[u8],
    cluster: &str,
    record: &Record,
) -> Result<Publication, wire::Error> {
    let p = wire::decode_publication(bytes)?;
    let (content, membership) = hashes(&p)?;
    if p.cluster.0 != cluster
        || p.sequence.0 != record.sequence
        || p.membership_version.0 != record.membership_version
        || content != record.content_hash
        || membership != record.membership_hash
    {
        return Err(wire::Error::Replay);
    }
    Ok(p)
}

/// Member map of a publication, keyed by Node UID.
pub fn history(p: &Publication) -> BTreeMap<String, Member> {
    p.members
        .iter()
        .map(|m| (m.node.0.clone(), m.clone()))
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::num::NonZeroU32;

    const C: &str = "11111111-1111-4111-8111-111111111111";

    fn member(n: u8, shares: u32) -> Member {
        Member {
            node: wire::NodeId(format!("{n:08x}-0000-4000-8000-000000000000")),
            shares: NonZeroU32::new(shares).unwrap(),
            peer_endpoint: format!("10.0.0.{n}:8082"),
            rails: vec![],
            site: String::new(),
        }
    }

    /// Prepare with every cache in the default pool.
    fn prep(
        prev: &Record,
        cluster: &str,
        members: impl IntoIterator<Item = Member>,
        caches: Vec<CacheDefinition>,
    ) -> Result<Prepared, wire::Error> {
        let plans = pools::plan(&caches, &Default::default(), []);
        prepare(prev, cluster, members, caches, &plans)
    }

    fn cache() -> CacheDefinition {
        let (c, o) = wire::canonical_socket_paths("a").unwrap();
        CacheDefinition {
            id: wire::CacheId(C.into()),
            name: "a".into(),
            client_socket: c,
            origin_socket: o,
            priority: wire::DEFAULT_PRIORITY,
            mode: wire::CacheMode::Cached,
        }
    }

    #[test]
    fn counters() {
        let p1 = prep(&Record::default(), C, [member(1, 4)], vec![]).unwrap();
        assert_eq!((p1.record.sequence, p1.record.membership_version), (1, 1));
        // Unchanged content keeps counters and bytes.
        let same = prep(&p1.record, C, [member(1, 4)], vec![]).unwrap();
        assert_eq!(same.record, p1.record);
        assert_eq!(same.encoded, p1.encoded);
        // A new cache joins a pool, so membership changes too.
        let p2 = prep(&p1.record, C, [member(1, 4)], vec![cache()]).unwrap();
        assert_eq!((p2.record.sequence, p2.record.membership_version), (2, 2));
        let p3 = prep(&p2.record, C, [member(1, 4), member(2, 4)], vec![cache()]).unwrap();
        assert_eq!((p3.record.sequence, p3.record.membership_version), (3, 3));
        let mut max = p3.record.clone();
        max.sequence = u64::MAX;
        assert_eq!(
            prep(&max, C, [member(1, 4)], vec![]).err(),
            Some(wire::Error::Overloaded)
        );
    }

    #[test]
    fn priority_change_republishes_with_new_pool_rank() {
        let p1 = prep(&Record::default(), C, [member(1, 4)], vec![cache()]).unwrap();
        let mut edited = cache();
        edited.priority = 900;
        let p2 = prep(&p1.record, C, [member(1, 4)], vec![edited]).unwrap();
        assert_eq!(p2.record.sequence, p1.record.sequence + 1);
        assert_ne!(p2.record.content_hash, p1.record.content_hash);
        // The default pool's rank is its highest cache priority.
        assert_eq!(
            p2.record.membership_version,
            p1.record.membership_version + 1
        );
        assert_ne!(p2.record.membership_hash, p1.record.membership_hash);
        assert_eq!(p2.publication.caches[0].priority, 900);
        assert_eq!(p2.publication.pools[0].rank, 900);
    }

    #[test]
    fn mode_change_republishes_without_membership_change() {
        let mut record = prep(&Record::default(), C, [member(1, 4)], vec![cache()])
            .unwrap()
            .record;
        let mut hashes = vec![record.content_hash.clone()];
        // Into each passthrough route, between them, and back to cached.
        for mode in [
            wire::CacheMode::PassthroughPeers,
            wire::CacheMode::PassthroughLocal,
            wire::CacheMode::Cached,
        ] {
            let mut edited = cache();
            edited.mode = mode;
            let next = prep(&record, C, [member(1, 4)], vec![edited]).unwrap();
            assert_eq!(next.record.sequence, record.sequence + 1);
            assert_ne!(next.record.content_hash, record.content_hash);
            assert_eq!(next.record.membership_version, record.membership_version);
            assert_eq!(next.record.membership_hash, record.membership_hash);
            assert_eq!(next.publication.caches[0].mode, mode);
            hashes.push(next.record.content_hash.clone());
            record = next.record;
        }
        // Returning to cached restores the original canonical content.
        assert_eq!(hashes[0], hashes[3]);
    }

    #[test]
    fn failover_never_reuses_counters() {
        let committed = prep(&Record::default(), C, [member(1, 4)], vec![])
            .unwrap()
            .record;
        // A new primary builds different content from the committed counters.
        let rebuilt = prep(&committed, C, [member(1, 5)], vec![]).unwrap();
        assert_eq!(rebuilt.record.sequence, 2);
        assert_eq!(rebuilt.record.membership_version, 2);
    }

    #[test]
    fn delta_roundtrip_and_selection() {
        let p1 = prep(
            &Record::default(),
            C,
            (1..50).map(|n| member(n, 4)),
            vec![cache()],
        )
        .unwrap();
        let i1 = Image::new(
            p1.record.clone(),
            p1.publication.clone(),
            p1.encoded.clone(),
            None,
        );
        let p2 = prep(
            &p1.record,
            C,
            (2..51).map(|n| member(n, if n == 7 { 9 } else { 4 })),
            vec![cache()],
        )
        .unwrap();
        let i2 = Image::new(
            p2.record.clone(),
            p2.publication.clone(),
            p2.encoded.clone(),
            Some(&i1),
        );
        let (seq, hash, d) = i2.delta.clone().unwrap();
        assert_eq!(seq, 1);
        let applied = wire::apply_delta(&p1.publication, &d).unwrap();
        assert_eq!(wire::encode_publication(&applied).unwrap(), p2.encoded);
        assert_eq!(*i2.for_base(Some(1), Some(&hash)), *d);
        assert_eq!(*i2.for_base(Some(1), Some("x")), p2.encoded);
        assert_eq!(*i2.for_base(None, Some(&hash)), p2.encoded);
        assert_eq!(*i2.for_base(Some(0), Some(&hash)), p2.encoded);
    }

    #[test]
    fn follower_verification() {
        let p = prep(&Record::default(), C, [member(1, 4)], vec![]).unwrap();
        assert!(verify_image(&p.encoded, C, &p.record).is_ok());
        let mut other = p.record.clone();
        other.content_hash = "0".repeat(64);
        assert_eq!(
            verify_image(&p.encoded, C, &other).err(),
            Some(wire::Error::Replay)
        );
        let mut seq = p.record.clone();
        seq.sequence = 2;
        assert!(verify_image(&p.encoded, C, &seq).is_err());
        assert!(
            verify_image(
                &p.encoded,
                "22222222-2222-4222-8222-222222222222",
                &p.record
            )
            .is_err()
        );
        assert!(verify_image(b"{}", C, &p.record).is_err());
        assert_eq!(history(&p.publication).len(), 1);
    }

    fn gpu_pool() -> Vec<PoolPlan> {
        let selector = pools::Selector::parse(Some(&json!({"matchLabels":{"gpu":"yes"}})))
            .unwrap()
            .unwrap();
        let selectors = BTreeMap::from([(cache().id, selector)]);
        let gpu = BTreeMap::from([("gpu".to_owned(), "yes".to_owned())]);
        let none = BTreeMap::new();
        let n = |i: u8| member(i, 4).node.0;
        let (n1, n2, n3) = (n(1), n(2), n(3));
        pools::plan(
            &[cache()],
            &selectors,
            [
                (n1.as_str(), &none),
                (n2.as_str(), &gpu),
                (n3.as_str(), &gpu),
            ],
        )
    }

    #[test]
    fn pools_resolve_to_member_indexes_and_bump_membership() {
        let plans = gpu_pool();
        let members = || [member(3, 4), member(1, 4), member(2, 4)];
        let p1 = prepare(&Record::default(), C, members(), vec![cache()], &plans).unwrap();
        assert_eq!(p1.publication.pools.len(), 1);
        assert_eq!(p1.publication.pools[0].members, Some(vec![1, 2]));
        assert_eq!(p1.publication.pools[0].rank, (1 << 32) | 500);
        // Same inputs in another order, as on another primary: same bytes.
        let again = prepare(
            &Record::default(),
            C,
            [member(2, 4), member(3, 4), member(1, 4)],
            vec![cache()],
            &plans,
        )
        .unwrap();
        assert_eq!(again.encoded, p1.encoded);
        assert_eq!(again.record, p1.record);
        // A node leaving the pool changes only membership-relevant pools.
        let mut moved = plans.clone();
        moved[0]
            .nodes
            .as_mut()
            .unwrap()
            .remove(&member(3, 4).node.0);
        let p2 = prepare(&p1.record, C, members(), vec![cache()], &moved).unwrap();
        assert_eq!(p2.publication.pools[0].members, Some(vec![1]));
        assert_eq!(p2.record.sequence, p1.record.sequence + 1);
        assert_eq!(
            p2.record.membership_version,
            p1.record.membership_version + 1
        );
        // Moving the cache to the default pool also bumps membership.
        let p3 = prep(&p2.record, C, members(), vec![cache()]).unwrap();
        assert_eq!(p3.publication.pools[0].members, None);
        assert_eq!(
            p3.record.membership_version,
            p2.record.membership_version + 1
        );
        // Pools naming non-members leave them out.
        let p4 = prepare(&p3.record, C, [member(1, 4)], vec![cache()], &plans).unwrap();
        assert_eq!(p4.publication.pools[0].members, Some(vec![]));
        // Deltas carry pools.
        let i3 = Image::new(
            p3.record.clone(),
            p3.publication.clone(),
            p3.encoded.clone(),
            None,
        );
        let d = encode_delta(&i3.publication, &p4.publication).unwrap();
        let applied = wire::apply_delta(&p3.publication, &d).unwrap();
        assert_eq!(wire::encode_publication(&applied).unwrap(), p4.encoded);
        // Uncovered caches are rejected.
        assert!(prepare(&Record::default(), C, members(), vec![cache()], &[]).is_err());
    }
}
