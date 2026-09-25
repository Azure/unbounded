//! Canonical slot encoding and weighted rendezvous ranking of up to three nodes.
//!
//! Freeze hash, integer arithmetic, and tie vectors before interoperability work.
//! Coalesce O(N) cold rankings under a CPU budget and cache by membership version.
use super::{
    hash,
    membership::{Membership, MembershipLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::identity::{NodeId, ObjectId, PageNumber},
};
use sha2::Digest;
use std::{
    cell::RefCell,
    collections::{BTreeMap, VecDeque},
    rc::Rc,
    sync::{Arc, Weak},
    task::Poll,
};

pub const SLOT_COUNT: u32 = 1 << 20;
const WORK_QUANTUM: usize = 256;

pub struct Placement {
    capacity: usize,
    cache: RefCell<RankingCache>,
}
#[derive(Clone, Debug)]
pub struct Candidates {
    pub membership: MembershipLease,
    pub ordered: Vec<NodeId>,
}

type CacheKey = (usize, u32);
#[derive(Default)]
struct RankingCache {
    entries: BTreeMap<CacheKey, Rc<RefCell<Ranking>>>,
    fifo: VecDeque<CacheKey>,
}

struct Ranking {
    membership: Weak<Membership>,
    cursor: usize,
    best: Vec<Score>,
}

#[derive(Clone, Copy)]
struct Score {
    node: usize,
    cost: u64,
    shares: u32,
}

impl Score {
    fn compare(&self, other: &Self) -> std::cmp::Ordering {
        (u128::from(self.cost) * u128::from(other.shares))
            .cmp(&(u128::from(other.cost) * u128::from(self.shares)))
            .then(self.node.cmp(&other.node))
    }
}

/// Fixed slot independent of object version and membership. Metadata passes page 0.
pub fn slot(object: &ObjectId, page: PageNumber) -> u32 {
    let mut digest = hash::domain(b"racer/slot/v1\0");
    hash::object(&mut digest, object, page);
    let digest = hash::finish(digest);
    u32::from_be_bytes(digest[..4].try_into().unwrap()) >> 12
}

/// Q32.32 approximation of -log2((sample+1)/2^64). Binary logarithm
/// via repeated squaring is fully specified integer arithmetic on all targets.
fn exponential_cost(sample: u64) -> u64 {
    let value = u128::from(sample) + 1;
    let exponent = 127 - value.leading_zeros();
    if exponent == 64 {
        return 1;
    }
    let mut normalized = value << (63 - exponent);
    let mut fraction = 0u64;
    for bit in (0..32).rev() {
        normalized = (normalized * normalized) >> 63;
        if normalized >= (1u128 << 64) {
            normalized >>= 1;
            fraction |= 1u64 << bit;
        }
    }
    ((64 - u64::from(exponent)) << 32) - fraction
}

impl Ranking {
    fn advance(&mut self, membership: &Membership, slot: u32, work: usize) {
        let end = self
            .cursor
            .saturating_add(work)
            .min(membership.members().len());
        for index in self.cursor..end {
            let member = &membership.members()[index];
            let mut digest = hash::domain(b"racer/hrw/v1\0");
            digest.update(slot.to_be_bytes());
            hash::bytes(&mut digest, member.node.0.as_bytes());
            let digest = hash::finish(digest);
            let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
            let score = Score {
                node: index,
                cost: exponential_cost(sample),
                shares: member.shares.get(),
            };
            let position = self.best.partition_point(|old| old.compare(&score).is_lt());
            if position < 3 {
                self.best.insert(position, score);
                self.best.truncate(3);
            }
        }
        self.cursor = end;
    }

    fn candidates(&self, membership: MembershipLease) -> Candidates {
        let ordered = self
            .best
            .iter()
            .map(|score| membership.members()[score.node].node.clone())
            .collect();
        Candidates {
            membership,
            ordered,
        }
    }
}

impl Placement {
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity,
            cache: RefCell::new(RankingCache::default()),
        }
    }

    fn ranking(&self, membership: &MembershipLease, slot: u32) -> Result<Rc<RefCell<Ranking>>> {
        let key = (Arc::as_ptr(membership) as usize, slot);
        let mut cache = self.cache.borrow_mut();
        if let Some(ranking) = cache.entries.get(&key) {
            // Weak ownership prevents pointer reuse, and never pins old snapshots.
            if ranking.borrow().membership.upgrade().is_some() {
                return Ok(ranking.clone());
            }
        }
        let ranking = Rc::new(RefCell::new(Ranking {
            membership: Arc::downgrade(membership),
            cursor: 0,
            best: Vec::with_capacity(4),
        }));
        if self.capacity == 0 {
            return Ok(ranking);
        }
        if cache.entries.len() == self.capacity {
            let victim = cache
                .fifo
                .iter()
                .position(|key| Rc::strong_count(&cache.entries[key]) == 1)
                .ok_or(Error::Overloaded)?;
            let key = cache.fifo.remove(victim).unwrap();
            cache.entries.remove(&key);
        }
        cache.fifo.push_back(key);
        cache.entries.insert(key, ranking.clone());
        Ok(ranking)
    }
    pub fn rank(
        &self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Result<Candidates> {
        let slot = slot(object, page);
        let ranking = self.ranking(&membership, slot)?;
        let mut ranking = ranking.borrow_mut();
        ranking.advance(&membership, slot, usize::MAX);
        Ok(ranking.candidates(membership))
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
        let slot = slot(object, page);
        Box::pin(async move {
            let ranking = self.ranking(&membership, slot)?;
            std::future::poll_fn(|cx| {
                let mut ranking = ranking.borrow_mut();
                ranking.advance(&membership, slot, WORK_QUANTUM);
                if ranking.cursor == membership.members().len() {
                    Poll::Ready(Ok(ranking.candidates(membership.clone())))
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await
        })
    }

    pub fn cached_rankings(&self) -> usize {
        self.cache.borrow().entries.len()
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::{
        model::identity::MembershipVersion,
        topology::{
            fixtures::{member, membership, object},
            membership::Membership,
        },
    };

    #[test]
    fn integer_log_edges_and_ties() {
        assert_eq!(exponential_cost(0), 64 << 32);
        assert_eq!(exponential_cost(u64::MAX), 1);
        for exponent in 0..64 {
            assert_eq!(
                exponential_cost((1u64 << exponent) - 1),
                (64 - exponent) << 32
            );
        }
        let a = Score {
            node: 0,
            cost: 12,
            shares: 4,
        };
        let b = Score {
            node: 1,
            cost: 3,
            shares: 1,
        };
        assert!(a.compare(&b).is_lt());
        assert!(
            Score {
                cost: u64::MAX,
                shares: u32::MAX,
                ..a
            }
            .compare(&Score {
                cost: u64::MAX,
                shares: 1,
                ..b
            })
            .is_lt()
        );
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
            node.alignment_enabled = false;
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
    fn weighted_distribution_without_share_expansion() {
        let members = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                vec![member(0, 1), member(1, 3), member(2, 6)],
            )
            .unwrap(),
        );
        let mut counts = [0usize; 3];
        for slot in 0..20_000 {
            let mut ranking = Ranking {
                membership: Arc::downgrade(&members),
                cursor: 0,
                best: vec![],
            };
            ranking.advance(&members, slot, usize::MAX);
            counts[ranking.best[0].node] += 1;
            assert_eq!(ranking.best.len(), 3);
        }
        for (actual, expected) in counts.into_iter().zip([2000, 6000, 12000]) {
            assert!(actual.abs_diff(expected) < 400, "{counts:?}");
        }
        let huge = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                vec![member(0, u32::MAX), member(1, 1)],
            )
            .unwrap(),
        );
        assert_eq!(
            Placement::new(1)
                .rank(huge, &object(), PageNumber(0))
                .unwrap()
                .ordered[0],
            member(0, 1).node
        );
    }

    #[test]
    fn cooperative_coalescing_bounded_cache_and_cancellation() {
        use std::task::Context;
        let placement = Placement::new(1);
        let members = membership(100_000);
        let mut first = placement.rank_async(members.clone(), &object(), PageNumber(0));
        let mut second = placement.rank_async(members.clone(), &object(), PageNumber(0));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(second.as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            placement
                .cache
                .borrow()
                .entries
                .values()
                .next()
                .unwrap()
                .borrow()
                .cursor,
            512
        );
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
        assert_eq!(placement.cached_rankings(), 1);
        assert_eq!(Arc::strong_count(&members), 2); // caller and returned Candidates only
        let uncached = Placement::new(0);
        uncached.rank(members, &object(), PageNumber(0)).unwrap();
        assert_eq!(uncached.cached_rankings(), 0);
    }
}
