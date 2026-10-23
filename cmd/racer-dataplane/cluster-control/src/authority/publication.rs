//! Publication counters, encoding, deltas, and follower verification.
//!
//! Counter rules match the old controller: changed content takes the next
//! sequence, and changed membership also takes the next membership version.
//! The same counters never name different content, so a new primary whose
//! rebuilt content differs from the committed hash always takes new counters.

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

/// Build the next publication from `prev` counters.
pub fn prepare(
    prev: &Record,
    cluster: &str,
    members: impl IntoIterator<Item = Member>,
    caches: Vec<CacheDefinition>,
) -> Result<Prepared, wire::Error> {
    let mut p = Publication {
        schema_version: wire::SCHEMA_VERSION,
        cluster: ClusterId(cluster.into()),
        sequence: PublicationSequence(1),
        membership_version: MembershipVersion(1),
        members: members.into_iter().collect(),
        caches,
    };
    p.members.sort_by(|a, b| a.node.cmp(&b.node));
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

fn members_json(p: &Publication) -> Result<(Value, BTreeMap<String, Value>), wire::Error> {
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
    Ok((v["caches"].take(), map))
}

/// Delta from `base` to `next`, in the old controller's format.
pub fn encode_delta(base: &Publication, next: &Publication) -> Result<Vec<u8>, wire::Error> {
    let (_, old) = members_json(base)?;
    let (caches, new) = members_json(next)?;
    let upsert: Vec<&Value> = new
        .iter()
        .filter(|(k, v)| old.get(*k) != Some(v))
        .map(|(_, v)| v)
        .collect();
    let remove: Vec<&String> = old.keys().filter(|k| !new.contains_key(*k)).collect();
    let d = json!({
        "delta_version": 1,
        "cluster": next.cluster.0,
        "base_sequence": base.sequence.0.to_string(),
        "base_hash": wire::content_hash(base)?,
        "sequence": next.sequence.0.to_string(),
        "membership_version": next.membership_version.0.to_string(),
        "content_hash": wire::content_hash(next)?,
        "upsert_members": upsert,
        "remove_members": remove,
        "caches": caches,
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
        match (&self.delta, after, base_hash) {
            (Some((seq, hash, d)), Some(a), Some(h)) if *seq == a && hash == h => d.clone(),
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

    fn cache() -> CacheDefinition {
        let (c, o) = wire::canonical_socket_paths("a").unwrap();
        CacheDefinition {
            id: wire::CacheId(C.into()),
            name: "a".into(),
            client_socket: c,
            origin_socket: o,
        }
    }

    #[test]
    fn counters() {
        let p1 = prepare(&Record::default(), C, [member(1, 4)], vec![]).unwrap();
        assert_eq!((p1.record.sequence, p1.record.membership_version), (1, 1));
        // Unchanged content keeps counters and bytes.
        let same = prepare(&p1.record, C, [member(1, 4)], vec![]).unwrap();
        assert_eq!(same.record, p1.record);
        assert_eq!(same.encoded, p1.encoded);
        // Cache change bumps only the sequence.
        let p2 = prepare(&p1.record, C, [member(1, 4)], vec![cache()]).unwrap();
        assert_eq!((p2.record.sequence, p2.record.membership_version), (2, 1));
        let p3 = prepare(&p2.record, C, [member(1, 4), member(2, 4)], vec![cache()]).unwrap();
        assert_eq!((p3.record.sequence, p3.record.membership_version), (3, 2));
        let mut max = p3.record.clone();
        max.sequence = u64::MAX;
        assert_eq!(
            prepare(&max, C, [member(1, 4)], vec![]).err(),
            Some(wire::Error::Overloaded)
        );
    }

    #[test]
    fn failover_never_reuses_counters() {
        let committed = prepare(&Record::default(), C, [member(1, 4)], vec![])
            .unwrap()
            .record;
        // A new primary builds different content from the committed counters.
        let rebuilt = prepare(&committed, C, [member(1, 5)], vec![]).unwrap();
        assert_eq!(rebuilt.record.sequence, 2);
        assert_eq!(rebuilt.record.membership_version, 2);
    }

    #[test]
    fn delta_roundtrip_and_selection() {
        let p1 = prepare(
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
        let p2 = prepare(
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
        let p = prepare(&Record::default(), C, [member(1, 4)], vec![]).unwrap();
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
}
