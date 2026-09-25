use super::membership::{Member, Membership, MembershipLease};
use crate::model::identity::*;
use std::{num::NonZeroU32, sync::Arc};

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
