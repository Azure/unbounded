//! Immutable placement, bidirectional routing, and end-to-end rail selection.
pub mod health;
pub mod membership;
pub mod placement;
pub mod rails;
pub mod routing;

mod hash {
    use crate::model::{ObjectId, PageNumber};
    use sha2::{Digest, Sha256};

    pub(super) fn domain(name: &[u8]) -> Sha256 {
        let mut hash = Sha256::new();
        hash.update(name);
        hash
    }
    pub(super) fn bytes(hash: &mut Sha256, value: &[u8]) {
        hash.update((value.len() as u32).to_be_bytes());
        hash.update(value);
    }
    pub(super) fn object(hash: &mut Sha256, object: &ObjectId, page: PageNumber) {
        bytes(hash, object.cache.0.as_bytes());
        hash.update(object.key.0);
        hash.update(page.0.to_be_bytes());
    }
    pub(super) fn finish(hash: Sha256) -> [u8; 32] {
        hash.finalize().into()
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::topology::fixtures;
        #[test]
        fn full_domain_separated_digest_vectors() {
            for (name, kind, expected) in [
                (
                    b"racer/slot/v1\0".as_slice(),
                    0,
                    "d8b632a58acf4dc92ccc3abe711290968975c95221982118f58393fbdead4781",
                ),
                (
                    b"racer/hrw/v1\0",
                    1,
                    "e41095812e885f6f0ae7e3c1a93d8ec04999df01dbb5820765c7e972b2c07a9c",
                ),
                (
                    b"racer/rail/v1\0",
                    2,
                    "500304ace38caf7cc36f8f97ae12bdfc89b92f7eb64c8bb060d1daf49f76314c",
                ),
                (
                    b"racer/rail/v2\0",
                    0,
                    "9ac860a9df3ce1ac648cde8350aeec1d1efc604eeae5cceb70d67dd55f4443d1",
                ),
            ] {
                let mut hash = domain(name);
                if kind == 1 {
                    hash.update(887651u32.to_be_bytes());
                    bytes(&mut hash, b"node-000000");
                } else {
                    object(&mut hash, &fixtures::object(), PageNumber(0));
                    if kind == 2 {
                        bytes(&mut hash, b"\"v1\"");
                    }
                }
                let actual: String = finish(hash).iter().map(|b| format!("{b:02x}")).collect();
                assert_eq!(actual, expected);
            }
        }
    }
}

/// Algorithm changes require a new version and new interoperability vectors.
/// Latest supported contract; topology changes require coordinated rollout.
pub const ALGORITHM_VERSION: u32 = 5;

pub const RADIX: usize = 32;

/// Shared capacity bound for every supported topology, including first-hop masks.
pub const MAX_DEGREE: usize = 2 * RADIX;
const _: () = assert!(MAX_DEGREE <= u64::BITS as usize);

#[cfg(test)]
mod fixtures {
    use super::membership::{Member, Membership, MembershipLease};
    use crate::model::*;
    use std::{num::NonZeroU32, sync::Arc};

    // Independent Python hashlib + outgoing-edge BFS vectors. N=1500, source=0,
    // destination=1499, request=[1;16], shares=1 at multiples of 3 and 4 elsewhere.
    // Attempts are big-endian u128; V5 intentionally retains the V4 hash domain.
    pub(super) const V5_NEXT_HOPS: [(u128, usize); 4] = [(0, 1312), (1, 937), (2, 703), (127, 937)];
    pub(super) fn member(index: usize, shares: u32) -> Member {
        Member {
            node: NodeId(format!("node-{index:06}")),
            shares: NonZeroU32::new(shares).unwrap(),
            peer_endpoint: "127.0.0.1:8080".into(),
            rails: vec![],
            alignment_enabled: true,
            site: String::new(),
        }
    }
    pub(super) fn membership(count: usize) -> MembershipLease {
        Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..count).map(|i| member(i, 4)).collect(),
            )
            .unwrap(),
        )
    }
    pub(super) fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("cache-a".into()),
            key: CacheKey([0x42; 32]),
        }
    }
}
