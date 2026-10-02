//! Integer weighted rendezvous placement with bounded cooperative cache work.
use crate::{Error, Member, Membership, hash};
use sha2::Digest;
use std::{
    cell::RefCell,
    collections::{BTreeMap, VecDeque},
    future::Future,
    rc::Rc,
    task::Poll,
};

const WORK_QUANTUM: usize = 256;

pub struct Placement {
    capacity: usize,
    cache: RefCell<RankingCache>,
    maintenance: RefCell<Maintenance>,
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{num::NonZeroU32, task::Context};

    #[derive(Clone, Debug)]
    struct TestMember(String, NonZeroU32);
    impl Member for TestMember {
        const DOMAIN: &'static str = "racer";
        fn id(&self) -> &[u8] {
            self.0.as_bytes()
        }
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }
    fn member(index: usize, weight: u32) -> TestMember {
        TestMember(format!("node-{index:06}"), NonZeroU32::new(weight).unwrap())
    }
    fn membership(count: usize) -> Membership<TestMember> {
        Membership::new((0..count).map(|i| member(i, 4)).collect()).unwrap()
    }
    fn key(page: u64) -> Vec<u8> {
        let mut key = 7u32.to_be_bytes().to_vec();
        key.extend_from_slice(b"cache-a");
        key.extend_from_slice(&[0x42; 32]);
        key.extend_from_slice(&page.to_be_bytes());
        key
    }

    #[test]
    fn golden_slot_and_weighted_ranking_vectors() {
        let members = Membership::new(
            [1, 3, 6, 4]
                .into_iter()
                .enumerate()
                .map(|(i, w)| member(i, w))
                .collect(),
        )
        .unwrap();
        let placement = Placement::new(3);
        for (page, expected_slot, expected_order) in [
            (0, 887_651, [1, 3, 2]),
            (1, 348_931, [3, 2, 1]),
            (u64::MAX, 665_200, [2, 1, 3]),
        ] {
            assert_eq!(slot::<TestMember>(&key(page)), expected_slot);
            assert_eq!(
                placement.rank(&members, &key(page)).unwrap(),
                expected_order
            );
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
    fn weighted_distribution_without_share_expansion() {
        let members = Membership::new(vec![member(0, 1), member(1, 3), member(2, 6)]).unwrap();
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
        let huge = Membership::new(vec![member(0, u32::MAX), member(1, 1)]).unwrap();
        assert_eq!(Placement::new(1).rank(&huge, &key(0)).unwrap()[0], 0);
    }

    #[test]
    fn cooperative_coalescing_bounded_cache_and_cancellation() {
        let placement = Placement::new(1);
        let members = membership(1000);
        let mut first = Box::pin(placement.rank_async(&members, &key(0)));
        let mut second = Box::pin(placement.rank_async(&members, &key(0)));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(first.as_mut().poll(&mut cx).is_pending());
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
            256
        );
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
        assert_eq!(placement.rank(&members, &key(1)), Err(Error::Overloaded));
        drop(first);
        let result = futures::executor::block_on(second).unwrap();
        assert_eq!(result, placement.rank(&members, &key(0)).unwrap());
        placement.rank(&members, &key(1)).unwrap();
        assert_eq!(placement.cache.borrow().entries.len(), 1);
        let uncached = Placement::new(0);
        uncached.rank(&members, &key(0)).unwrap();
        assert!(uncached.cache.borrow().entries.is_empty());
    }

    #[test]
    fn generic_domain_isolates_identity_slot_and_cached_scores() {
        struct Other(TestMember);
        impl Member for Other {
            const DOMAIN: &'static str = "object-store";
            fn id(&self) -> &[u8] {
                self.0.id()
            }
            fn weight(&self) -> NonZeroU32 {
                self.0.weight()
            }
        }
        let a = membership(30);
        let b = Membership::new(a.members().iter().cloned().map(Other).collect()).unwrap();
        assert_ne!(a.identity(), b.identity());
        assert_ne!(
            Membership::<TestMember>::new(vec![]).unwrap().identity(),
            Membership::<Other>::new(vec![]).unwrap().identity()
        );
        assert_ne!(
            slot::<TestMember>(b"opaque\0key"),
            slot::<Other>(b"opaque\0key")
        );
        let placement = Placement::new(4);
        placement.rank(&a, b"opaque\0key").unwrap();
        assert_eq!(
            placement.rank(&b, b"opaque\0key").unwrap(),
            Placement::new(0).rank(&b, b"opaque\0key").unwrap()
        );
        assert_eq!(placement.cache.borrow().entries.len(), 2);
        for count in 0..=4 {
            assert_eq!(
                Placement::new(0)
                    .rank(&membership(count), b"")
                    .unwrap()
                    .len(),
                count.min(3)
            );
        }
    }

    #[test]
    fn predecessor_maintenance_and_churn_match_cold_oracle() {
        let placement = Placement::new(512);
        let oracle = Placement::new(0);
        let mut old = membership(80);
        for generation in 2..42 {
            for page in 0..40 {
                placement.rank(&old, &key(page)).unwrap();
            }
            let mut members = old.members().to_vec();
            members.remove(generation % members.len());
            members.push(member(100 + generation, 4));
            members[generation % 10].1 = NonZeroU32::new((generation % 7 + 1) as u32).unwrap();
            let next = Membership::new(members).unwrap().with_predecessor(&old);
            for _ in 0..100 {
                placement.maintain(&next).unwrap();
            }
            for page in 0..40 {
                assert_eq!(
                    placement.rank(&next, &key(page)).unwrap(),
                    oracle.rank(&next, &key(page)).unwrap(),
                    "generation {generation} page {page}"
                );
            }
            old = next;
        }
    }
}

#[derive(Default)]
struct Maintenance {
    identity: Option<[u8; 32]>,
    cursor: Option<CacheKey>,
    active: Option<u32>,
    finished: bool,
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

fn slot<M: Member>(key: &[u8]) -> u32 {
    let mut digest = hash::domain::<M>(b"/slot/v1\0");
    digest.update(key);
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
    fn score<M: Member>(membership: &Membership<M>, slot: u32, index: usize) -> Score {
        let member = &membership.members()[index];
        let mut digest = hash::domain::<M>(b"/hrw/v1\0");
        digest.update(slot.to_be_bytes());
        hash::bytes(&mut digest, member.id());
        let digest = hash::finish(digest);
        Score {
            node: index,
            cost: exponential_cost(u64::from_be_bytes(digest[..8].try_into().unwrap())),
            shares: member.weight().get(),
        }
    }
    fn insert(&mut self, score: Score) {
        let position = self.best.partition_point(|old| old.compare(&score).is_lt());
        if position < 3 {
            self.best.insert(position, score);
            self.best.truncate(3);
        }
    }
    fn updated<M: Member>(&self, membership: &Membership<M>, slot: u32) -> Option<Self> {
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
            if let Some(index) = new
                && !next.best.iter().any(|score| score.node == *index)
            {
                next.insert(Self::score(membership, slot, *index));
            }
        }
        Some(next)
    }
    fn advance<M: Member>(&mut self, membership: &Membership<M>, slot: u32, work: usize) {
        let end = self
            .cursor
            .saturating_add(work)
            .min(membership.members().len());
        for index in self.cursor..end {
            self.insert(Self::score(membership, slot, index));
        }
        self.cursor = end;
    }

    fn candidates(&self) -> Vec<usize> {
        self.best.iter().map(|score| score.node).collect()
    }
}

impl Placement {
    /// Conservative allocation charge including scores, cache containers,
    /// reference counts, allocator overhead, and container slack.
    pub const ENTRY_BYTES: usize = 512;

    pub fn new(capacity: usize) -> Self {
        Self {
            capacity,
            cache: RefCell::new(RankingCache::default()),
            maintenance: RefCell::new(Maintenance::default()),
        }
    }

    fn ranking<M: Member>(
        &self,
        membership: &Membership<M>,
        slot: u32,
    ) -> Result<Rc<RefCell<Ranking>>, Error> {
        let key = (membership.identity(), slot);
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

    /// Rank up to three member indices for an application-encoded key.
    pub fn rank<M: Member>(
        &self,
        membership: &Membership<M>,
        key: &[u8],
    ) -> Result<Vec<usize>, Error> {
        let slot = slot::<M>(key);
        let ranking = self.ranking(membership, slot)?;
        let mut ranking = ranking.borrow_mut();
        ranking.advance(membership, slot, usize::MAX);
        Ok(ranking.candidates())
    }

    /// Concurrent requests for a resident slot share progress. Each poll scores
    /// at most 256 cold members, holding no borrow across a yield. Dropping the
    /// future releases its cache admission; no runtime or cancellation is required.
    pub fn rank_async<'a, M: Member>(
        &'a self,
        membership: &'a Membership<M>,
        key: &[u8],
    ) -> impl Future<Output = Result<Vec<usize>, Error>> + 'a + use<'a, M> {
        let slot = slot::<M>(key);
        async move {
            let ranking = self.ranking(membership, slot)?;
            std::future::poll_fn(|cx| {
                let mut ranking = ranking.borrow_mut();
                ranking.advance(membership, slot, WORK_QUANTUM);
                if ranking.cursor == membership.members().len() {
                    Poll::Ready(Ok(ranking.candidates()))
                } else {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await
        }
    }

    /// Warm only already-demanded predecessor slots. Each turn hashes at most
    /// one cold quantum or visits one retained key, with no full-cache sweep.
    pub fn maintain<M: Member>(&self, membership: &Membership<M>) -> Result<(), Error> {
        use std::ops::Bound::{Excluded, Unbounded};
        let mut work = self.maintenance.borrow_mut();
        let identity = membership.identity();
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
