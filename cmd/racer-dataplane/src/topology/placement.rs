//! Canonical slot encoding and weighted rendezvous ranking of up to three nodes.
//!
//! Freeze hash, integer arithmetic, and tie vectors before interoperability work.
//! Coalesce O(N) cold rankings under a CPU budget and cache by placement identity.
use super::{
    hash,
    membership::{Membership, MembershipLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::{NodeId, ObjectId, PageNumber},
};
use sha2::Digest;
use std::{
    cell::RefCell,
    collections::{BTreeMap, VecDeque},
    rc::Rc,
    task::Poll,
};

pub const SLOT_COUNT: u32 = 1 << 20;
const WORK_QUANTUM: usize = 256;
/// Conservative allocation charge: ranking, four scores, Rc/RefCell, BTree
/// entry and FIFO key, including container slack and allocator overhead.
pub const RANKING_BYTES: usize = 512;

pub struct Placement {
    capacity: usize,
    cache: RefCell<RankingCache>,
    maintenance: RefCell<Maintenance>,
}
#[derive(Default)]
struct Maintenance {
    identity: Option<[u8; 32]>,
    cursor: Option<CacheKey>,
    active: Option<u32>,
    finished: bool,
}
#[derive(Clone, Debug)]
pub struct Candidates {
    pub membership: MembershipLease,
    pub ordered: Vec<NodeId>,
}

type CacheKey = ([u8; 32], u32);
#[derive(Default)]
struct RankingCache {
    entries: BTreeMap<CacheKey, Rc<RefCell<Ranking>>>,
    fifo: VecDeque<CacheKey>,
}

struct Ranking {
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
    fn score(membership: &Membership, slot: u32, index: usize) -> Score {
        let member = &membership.members()[index];
        let mut digest = hash::domain(b"racer/hrw/v1\0");
        digest.update(slot.to_be_bytes());
        hash::bytes(&mut digest, member.node.0.as_bytes());
        let digest = hash::finish(digest);
        Score {
            node: index,
            cost: exponential_cost(u64::from_be_bytes(digest[..8].try_into().unwrap())),
            shares: member.shares.get(),
        }
    }
    fn insert(&mut self, score: Score) {
        let position = self.best.partition_point(|old| old.compare(&score).is_lt());
        if position < 3 {
            self.best.insert(position, score);
            self.best.truncate(3);
        }
    }
    fn updated(&self, membership: &Membership, slot: u32) -> Option<Self> {
        let delta = membership.placement_delta.as_ref()?;
        if self.cursor != delta.old_count {
            return None;
        }
        let mut next = Self {
            cursor: membership.members().len(),
            best: Vec::with_capacity(4),
        };
        for old in &self.best {
            if let Some((_, new)) = delta
                .changes
                .iter()
                .find(|(index, _)| *index == Some(old.node))
            {
                let new = (*new)?;
                let score = Self::score(membership, slot, new);
                // A worse retained winner may expose an unretained fourth node.
                if score.shares < old.shares {
                    return None;
                }
                next.insert(score);
            } else {
                let removed = delta
                    .changes
                    .iter()
                    .filter(|(a, b)| b.is_none() && a.is_some_and(|a| a < old.node))
                    .count();
                let base = old.node - removed;
                let mut node = base;
                for (_, added) in delta.changes.iter().filter(|(a, _)| a.is_none()) {
                    if added.is_some_and(|added| added <= node) {
                        node += 1;
                    }
                }
                next.insert(Score { node, ..*old });
            }
        }
        for (_, new) in &delta.changes {
            if let Some(index) = new {
                if !next.best.iter().any(|score| score.node == *index) {
                    next.insert(Self::score(membership, slot, *index));
                }
            }
        }
        Some(next)
    }
    fn advance(&mut self, membership: &Membership, slot: u32, work: usize) {
        let end = self
            .cursor
            .saturating_add(work)
            .min(membership.members().len());
        for index in self.cursor..end {
            self.insert(Self::score(membership, slot, index));
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
            maintenance: RefCell::new(Maintenance::default()),
        }
    }

    pub fn with_memory_budget(bytes: usize) -> Self {
        Self::new(bytes / RANKING_BYTES)
    }

    fn ranking(&self, membership: &MembershipLease, slot: u32) -> Result<Rc<RefCell<Ranking>>> {
        let key = (membership.placement_identity(), slot);
        let mut cache = self.cache.borrow_mut();
        if let Some(ranking) = cache.entries.get(&key) {
            return Ok(ranking.clone());
        }
        let updated = membership.placement_delta.as_ref().and_then(|delta| {
            cache
                .entries
                .get(&(delta.base, slot))?
                .borrow()
                .updated(membership, slot)
        });
        let ranking = Rc::new(RefCell::new(updated.unwrap_or_else(|| Ranking {
            cursor: 0,
            best: Vec::with_capacity(4),
        })));
        if self.capacity == 0 {
            return Ok(ranking);
        }
        if cache.entries.len() == self.capacity {
            // Rotate busy entries instead of scanning the entire working set.
            for _ in 0..cache.fifo.len().min(64) {
                let key = cache.fifo.pop_front().unwrap();
                if Rc::strong_count(&cache.entries[&key]) == 1 {
                    cache.entries.remove(&key);
                    break;
                }
                cache.fifo.push_back(key);
            }
            if cache.entries.len() == self.capacity {
                return Err(Error::Overloaded);
            }
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
        self.rank_scoped(membership, object, page, None)
    }

    pub fn rank_scoped<'a>(
        &'a self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
        scope: Option<&'a crate::runtime::deadline::RequestScope>,
    ) -> Operation<'a, Candidates> {
        let slot = slot(object, page);
        Box::pin(async move {
            if let Some(scope) = scope {
                scope.check()?;
            }
            let cancellation = scope
                .map(|scope| scope.cancellation.subscribe())
                .transpose()?;
            let ranking = self.ranking(&membership, slot)?;
            std::future::poll_fn(|cx| {
                if let Some(scope) = scope {
                    if let Some(cancellation) = &cancellation {
                        cancellation.register(cx.waker());
                    }
                    scope.check()?;
                }
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
    /// Warm only already-demanded predecessor slots. Each turn hashes at most
    /// one cold quantum or visits one retained key, with no full-cache sweep.
    pub fn maintain(&self, membership: &MembershipLease) -> Result<()> {
        use std::ops::Bound::{Excluded, Unbounded};
        let mut work = self.maintenance.borrow_mut();
        let identity = membership.placement_identity();
        if work.identity != Some(identity) {
            *work = Maintenance {
                identity: Some(identity),
                ..Maintenance::default()
            };
        }
        if work.finished {
            return Ok(());
        }
        if let Some(slot) = work.active {
            let ranking = self.ranking(membership, slot)?;
            let mut ranking = ranking.borrow_mut();
            ranking.advance(membership, slot, WORK_QUANTUM);
            if ranking.cursor == membership.members().len() {
                work.active = None;
            }
            return Ok(());
        }
        let Some(delta) = &membership.placement_delta else {
            work.finished = true;
            return Ok(());
        };
        let next = {
            let cache = self.cache.borrow();
            match work.cursor {
                Some(key) => cache.entries.range((Excluded(key), Unbounded)).next(),
                None => cache.entries.range((delta.base, 0)..).next(),
            }
            .map(|(key, _)| *key)
        };
        match next {
            Some(key) if key.0 == delta.base => {
                work.cursor = Some(key);
                work.active = Some(key.1);
            }
            _ => work.finished = true,
        }
        Ok(())
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
        for (sample, cost) in [
            (0xe41095812e885f6f, 715_971_622),
            (0xc4251196ce419070, 1_650_232_626),
            (0x9322018f0806e768, 3_431_784_333),
            (0xb379deaba20d903a, 2_200_536_977),
        ] {
            assert_eq!(exponential_cost(sample), cost);
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
            node.alignment_enabled = false;
        }
        let current = Arc::new(Membership::validate(MembershipVersion(2), nodes).unwrap());
        assert_eq!(old.placement_identity(), current.placement_identity());
        let mut future = placement.rank_async(current.clone(), &object(), PageNumber(0));
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let Poll::Ready(Ok(actual)) = future.as_mut().poll(&mut cx) else {
            panic!("endpoint-only change must not perform another cold ranking");
        };
        assert_eq!(actual.ordered, expected.ordered);
        assert!(Arc::ptr_eq(&actual.membership, &current));
        assert_eq!(placement.cached_rankings(), 1);
        drop(old);
        drop(expected);
        assert_eq!(placement.cached_rankings(), 1);
    }

    #[test]
    fn scoped_cold_rank_cancels_between_bounded_quanta() {
        use crate::{model::RequestId, runtime::deadline::RequestScope};
        let placement = Placement::with_memory_budget(4 * RANKING_BYTES);
        let members = membership(100_000);
        let scope = RequestScope::new(
            RequestId([1; 16]),
            crate::runtime::environment::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        let mut future = placement.rank_scoped(members, &object(), PageNumber(0), Some(&scope));
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.cancel().unwrap();
        assert!(matches!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
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
            WORK_QUANTUM
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
