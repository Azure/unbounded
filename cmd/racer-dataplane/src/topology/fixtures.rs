use super::membership::{Member, Membership, MembershipLease};
use crate::model::identity::*;
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
