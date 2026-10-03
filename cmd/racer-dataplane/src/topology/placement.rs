//! Canonical slot encoding and weighted rendezvous ranking of up to three nodes.
//!
//! Freeze hash, integer arithmetic, and tie vectors before interoperability work.
//! Coalesce O(N) cold rankings under a CPU budget and cache by placement identity.
use super::{hash, membership::MembershipLease};
use crate::{
    error::{Error, Operation, Result},
    model::{NodeId, ObjectId, PageNumber},
};
use std::future::Future;
#[cfg(test)]
use std::task::Poll;

pub const SLOT_COUNT: u32 = 1 << 20;
#[cfg(test)]
use super::membership::scored_members;
/// Conservative allocation charge: ranking, four scores, Rc/RefCell, BTree
/// entry and FIFO key, including container slack and allocator overhead.
pub const RANKING_BYTES: usize = ::topology::Placement::ENTRY_BYTES;

pub struct Placement {
    inner: ::topology::Placement,
}
#[derive(Clone, Debug)]
pub struct Candidates {
    pub membership: MembershipLease,
    pub ordered: Vec<NodeId>,
}

fn encoded_key(object: &ObjectId, page: PageNumber) -> Vec<u8> {
    let mut key = Vec::with_capacity(4 + object.cache.0.len() + 32 + 8);
    key.extend_from_slice(&(object.cache.0.len() as u32).to_be_bytes());
    key.extend_from_slice(object.cache.0.as_bytes());
    key.extend_from_slice(&object.key.0);
    key.extend_from_slice(&page.0.to_be_bytes());
    key
}

/// Fixed slot independent of object version and membership. Metadata passes page 0.
pub fn slot(object: &ObjectId, page: PageNumber) -> u32 {
    let mut digest = hash::domain(b"racer/slot/v1\0");
    hash::object(&mut digest, object, page);
    let digest = hash::finish(digest);
    u32::from_be_bytes(digest[..4].try_into().unwrap()) >> 12
}

fn candidates(membership: MembershipLease, ranked: Vec<usize>) -> Candidates {
    let ordered = ranked
        .into_iter()
        .map(|i| membership.members()[i].node.clone())
        .collect();
    Candidates {
        membership,
        ordered,
    }
}

fn placement_error(error: ::topology::Error) -> Error {
    match error {
        ::topology::Error::Overloaded => Error::Overloaded,
        _ => Error::InvalidConfiguration,
    }
}

impl Placement {
    pub fn new(capacity: usize) -> Self {
        Self {
            inner: ::topology::Placement::new(capacity),
        }
    }

    pub fn with_memory_budget(bytes: usize) -> Self {
        Self::new(bytes / RANKING_BYTES)
    }

    pub fn rank(
        &self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Result<Candidates> {
        let ranked = self
            .inner
            .rank(&membership.inner, &encoded_key(object, page))
            .map_err(placement_error)?;
        Ok(candidates(membership, ranked))
    }

    /// Reactor-friendly cold ranking. Concurrent requests for a resident slot
    /// share progress; each poll hashes at most 256 members, with no held borrow
    /// across the yield. Dropped operations release their active-cache admission.
    pub fn rank_async<'a>(
        &'a self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Operation<'a, Candidates> {
        self.rank_scoped(membership, object, page, None)
    }

    pub fn rank_scoped<'a>(
        &'a self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
        scope: Option<&'a crate::runtime::deadline::RequestScope>,
    ) -> Operation<'a, Candidates> {
        let key = encoded_key(object, page);
        Box::pin(async move {
            if let Some(scope) = scope {
                scope.check()?;
            }
            let cancellation = scope
                .map(|scope| scope.cancellation.subscribe())
                .transpose()?;
            let mut ranking = std::pin::pin!(self.inner.rank_async(&membership.inner, &key));
            let ranked = std::future::poll_fn(|cx| {
                if let Some(scope) = scope {
                    if let Some(cancellation) = &cancellation {
                        cancellation.register(cx.waker());
                    }
                    scope.check()?;
                }
                let result = ranking
                    .as_mut()
                    .poll(cx)
                    .map(|result| result.map_err(placement_error));
                if let Some(scope) = scope {
                    scope.check()?;
                }
                result
            })
            .await?;
            Ok(candidates(membership.clone(), ranked))
        })
    }

    /// Warm only already-demanded predecessor slots. Each turn hashes at most
    /// one cold quantum or visits one retained key, with no full-cache sweep.
    pub fn maintain(&self, membership: &MembershipLease) -> Result<()> {
        self.inner
            .maintain(&membership.inner)
            .map_err(placement_error)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::MembershipVersion,
        topology::{
            fixtures::{member, membership, object},
            membership::Membership,
        },
    };
    use std::sync::Arc;

    #[test]
    fn golden_slot_and_weighted_ranking_vectors() {
        let members = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [1, 3, 6, 4]
                    .into_iter()
                    .enumerate()
                    .map(|(i, weight)| member(i, weight))
                    .collect(),
            )
            .unwrap(),
        );
        let placement = Placement::new(3);
        for (page, expected_slot, expected_order) in [
            (0, 887_651, [1, 3, 2]),
            (1, 348_931, [3, 2, 1]),
            (u64::MAX, 665_200, [2, 1, 3]),
        ] {
            assert_eq!(slot(&object(), PageNumber(page)), expected_slot);
            let ranking = placement
                .rank(members.clone(), &object(), PageNumber(page))
                .unwrap();
            assert_eq!(ranking.ordered, expected_order.map(|i| member(i, 1).node));
        }
    }

    #[test]
    fn addition_removal_preserve_survivor_order_and_old_snapshot_rankings() {
        let old = membership(12);
        let mut added = old.members().to_vec();
        added.push(member(12, 4));
        let added = Arc::new(Membership::validate(MembershipVersion(2), added).unwrap());
        let removed = Arc::new(
            Membership::validate(MembershipVersion(3), old.members()[1..].to_vec()).unwrap(),
        );
        let placement = Placement::new(6);
        for page in 0..100 {
            let original = placement
                .rank(old.clone(), &object(), PageNumber(page))
                .unwrap()
                .ordered;
            let extended = placement
                .rank(added.clone(), &object(), PageNumber(page))
                .unwrap()
                .ordered;
            let survivors: Vec<_> = extended
                .iter()
                .filter(|node| **node != member(12, 1).node)
                .cloned()
                .collect();
            assert_eq!(&original[..survivors.len()], survivors);
            let reduced = placement
                .rank(removed.clone(), &object(), PageNumber(page))
                .unwrap()
                .ordered;
            let survivors: Vec<_> = original
                .iter()
                .filter(|node| **node != member(0, 1).node)
                .cloned()
                .collect();
            assert_eq!(&reduced[..survivors.len()], survivors);
            assert_eq!(
                placement
                    .rank(old.clone(), &object(), PageNumber(page))
                    .unwrap()
                    .ordered,
                original
            );
        }
    }

    #[test]
    fn small_clusters_order_invariance_and_nonplacement_changes() {
        let placement = Placement::new(10);
        for count in 0..=4 {
            assert_eq!(
                placement
                    .rank(membership(count), &object(), PageNumber(0))
                    .unwrap()
                    .ordered
                    .len(),
                count.min(3)
            );
        }
        let original = membership(30);
        let mut changed = original.members().to_vec();
        changed.reverse();
        for node in &mut changed {
            node.peer_endpoint = "[::1]:9090".into();
            node.rails.clear();
        }
        let changed = Arc::new(Membership::validate(MembershipVersion(2), changed).unwrap());
        for page in 0..100 {
            assert_eq!(
                placement
                    .rank(original.clone(), &object(), PageNumber(page))
                    .unwrap()
                    .ordered,
                placement
                    .rank(changed.clone(), &object(), PageNumber(page))
                    .unwrap()
                    .ordered
            );
        }
    }

    #[test]
    fn incremental_demand_maintenance_matches_cold_oracle_under_churn() {
        let placement = Placement::with_memory_budget(512 * RANKING_BYTES);
        let oracle = Placement::new(0);
        let mut old = membership(80);
        for generation in 2..42 {
            for page in 0..40 {
                placement
                    .rank(old.clone(), &object(), PageNumber(page))
                    .unwrap();
            }
            let mut members = old.members().to_vec();
            members.remove(generation as usize % members.len());
            members.push(member(100 + generation as usize, 4));
            members[generation as usize % 10].shares =
                std::num::NonZeroU32::new(generation % 7 + 1).unwrap();
            let next = Arc::new(
                Membership::validate(MembershipVersion(generation as u64), members)
                    .unwrap()
                    .with_predecessor(&old),
            );
            for page in 0..40 {
                assert_eq!(
                    placement
                        .rank(next.clone(), &object(), PageNumber(page))
                        .unwrap()
                        .ordered,
                    oracle
                        .rank(next.clone(), &object(), PageNumber(page))
                        .unwrap()
                        .ordered,
                    "generation {generation} page {page}"
                );
            }
            old = next;
        }
    }

    #[test]
    fn endpoint_epoch_reuses_completed_rank_and_returns_current_routing() {
        let placement = Placement::with_memory_budget(RANKING_BYTES);
        let old = membership(100_000);
        let expected = placement
            .rank(old.clone(), &object(), PageNumber(0))
            .unwrap();
        let mut nodes = old.members().to_vec();
        for node in &mut nodes {
            node.peer_endpoint = "[::1]:9090".into();
            node.rails.clear();
        }
        let current = Arc::new(Membership::validate(MembershipVersion(2), nodes).unwrap());
        assert_eq!(old.placement_identity(), current.placement_identity());
        let mut future = placement.rank_async(current.clone(), &object(), PageNumber(0));
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let before = scored_members();
        let Poll::Ready(Ok(actual)) = future.as_mut().poll(&mut cx) else {
            panic!("endpoint-only change must not perform another cold ranking");
        };
        assert_eq!(actual.ordered, expected.ordered);
        assert!(Arc::ptr_eq(&actual.membership, &current));
        assert_eq!(scored_members(), before);
        drop(old);
        drop(expected);
        placement.rank(current, &object(), PageNumber(0)).unwrap();
        assert_eq!(scored_members(), before);
    }

    #[test]
    fn scoped_cold_rank_cancels_between_bounded_quanta() {
        use crate::{model::RequestId, runtime::deadline::RequestScope};
        let placement = Placement::with_memory_budget(4 * RANKING_BYTES);
        let members = membership(100_000);
        let scope = RequestScope::new(
            RequestId([1; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        let mut future = placement.rank_scoped(members, &object(), PageNumber(0), Some(&scope));
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let before = scored_members();
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.cancel().unwrap();
        assert!(matches!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(scored_members() - before, 256);
    }

    #[test]
    fn cooperative_coalescing_bounded_cache_and_cancellation() {
        use std::task::Context;
        let placement = Placement::new(1);
        let members = membership(100_000);
        let mut first = placement.rank_async(members.clone(), &object(), PageNumber(0));
        let mut second = placement.rank_async(members.clone(), &object(), PageNumber(0));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let before = scored_members();
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(second.as_mut().poll(&mut cx).is_pending());
        assert_eq!(scored_members() - before, 512);
        assert_eq!(
            placement
                .rank(members.clone(), &object(), PageNumber(1))
                .unwrap_err(),
            Error::Overloaded
        );
        drop(first);
        let ranked = futures::executor::block_on(second).unwrap();
        assert_eq!(
            ranked.ordered,
            placement
                .rank(members.clone(), &object(), PageNumber(0))
                .unwrap()
                .ordered
        );
        placement
            .rank(members.clone(), &object(), PageNumber(1))
            .unwrap();
        let before = scored_members();
        placement
            .rank(members.clone(), &object(), PageNumber(1))
            .unwrap();
        assert_eq!(scored_members(), before);
        assert_eq!(Arc::strong_count(&members), 2); // caller and returned Candidates only
        let uncached = Placement::new(0);
        uncached
            .rank(members.clone(), &object(), PageNumber(0))
            .unwrap();
        let before = scored_members();
        uncached.rank(members, &object(), PageNumber(0)).unwrap();
        assert_eq!(scored_members() - before, 100_000);
    }
}
