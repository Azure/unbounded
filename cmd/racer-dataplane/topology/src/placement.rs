//! Picks which members own a key, using weighted rendezvous hashing.

use crate::{Error, MAX_INCREMENTAL_CHANGES, Member, MemberChange, Membership, hash};

use sha2::Digest;

use std::{cell::RefCell, collections::BTreeMap, future::Future, rc::Rc, task::Poll};

/// Most owners returned for one key.
pub const REPLICAS: usize = 3;

/// Number of hash bits used to pick a key's slot.
pub const SLOT_BITS: u32 = 20;

/// Number of slots. Keys in the same slot get the same owners.
pub const SLOT_COUNT: u32 = 1 << SLOT_BITS;

/// Most members scored in one poll or one maintenance step.
const WORK_QUANTUM: usize = 256;

/// Result of one [`Placement::maintain`] step.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Maintenance {
    /// Nothing left to do.
    Idle,

    /// Some work was done. Call again.
    Progress,

    /// The next old entry is in use by a request. Try again later.
    Blocked,
}

/// Picks owners for keys and caches the results.
///
/// Results are member positions. Requests for the same slot share one cache
/// entry and its work. The cache uses CLOCK eviction.
///
/// Not thread-safe. Give each worker its own `Placement`.
pub struct Placement {
    /// Most cache entries kept. Zero turns caching off.
    capacity: usize,

    /// Cached rankings.
    cache: RefCell<RankingCache>,

    /// Progress of [`Placement::maintain`], kept between calls.
    maintenance: RefCell<MaintenanceWork>,
}

/// Progress of moving cached entries from the old membership to the new one.
#[derive(Default)]
struct MaintenanceWork {
    /// The new membership. Changing it restarts maintenance.
    identity: Option<[u8; 32]>,

    /// Last old entry moved. Kept after the scan finishes.
    cursor: Option<CacheKey>,

    /// A slot being fully rescored. Held so requests cannot evict it, and kept
    /// when the scan restarts.
    active: Option<(u32, Rc<RefCell<Ranking>>)>,

    /// True when the scan is done. Cleared when an old entry is added or the
    /// membership changes.
    finished: bool,
}

/// Membership identity, then slot. Sorting this way keeps each membership's
/// entries together.
type CacheKey = ([u8; 32], u32);

/// CLOCK cache of rankings.
#[derive(Default)]
struct RankingCache {
    /// Rankings, sorted by membership and then slot.
    entries: BTreeMap<CacheKey, Rc<RefCell<Ranking>>>,

    /// Where the CLOCK hand last stopped. That entry may be gone.
    hand: Option<CacheKey>,

    /// The old membership that maintenance is moving away from.
    predecessor: Option<[u8; 32]>,

    /// Set when an entry is added for `predecessor`, so maintenance rescans.
    predecessor_dirty: bool,
}

impl RankingCache {
    /// Remove an entry. The CLOCK hand does not move.
    fn remove(&mut self, key: &CacheKey) {
        self.entries.remove(key);
    }

    /// Evict one entry that no request is using. Recently used entries get a
    /// second chance first. Returns false if every entry is in use.
    fn evict(&mut self) -> bool {
        // CLOCK gives referenced entries a second chance. Two full rotations
        // find any unpinned entry, including one beyond 64 busy entries.
        use std::ops::Bound::{Excluded, Unbounded};
        for _ in 0..self.entries.len().saturating_mul(2) {
            let next = self
                .hand
                .and_then(|key| self.entries.range((Excluded(key), Unbounded)).next())
                .or_else(|| self.entries.first_key_value());
            let (&key, ranking) = next.unwrap();
            self.hand = Some(key);
            let pinned = Rc::strong_count(ranking) != 1;
            let referenced = std::mem::replace(&mut ranking.borrow_mut().referenced, false);
            if !pinned && !referenced {
                self.entries.remove(&key);
                return true;
            }
        }
        false
    }
}

/// The best three scores for one slot, finished or still in progress.
/// Requests for the same slot share it.
struct Ranking {
    /// Next member to score. Equals the member count when done.
    cursor: usize,

    /// Up to three best scores, plus room for one more so inserts never
    /// reallocate.
    best: Vec<Score>,

    /// CLOCK "recently used" bit. Set whenever a request finds this entry.
    referenced: bool,

    /// Test only: how many scores were computed, including cheap updates.
    #[cfg(test)]
    scored: usize,

    /// Test only: true if this was built cheaply from the old membership.
    #[cfg(test)]
    incremental: bool,
}

impl Default for Ranking {
    /// Start empty, with room for one extra score so inserts never reallocate.
    fn default() -> Self {
        Self {
            cursor: 0,
            best: Vec::with_capacity(REPLICAS + 1),
            referenced: true,
            #[cfg(test)]
            scored: 0,
            #[cfg(test)]
            incremental: false,
        }
    }
}

/// One member's score for one slot. Lower is better.
#[derive(Clone, Copy)]
struct Score {
    /// Member position. Also breaks ties.
    node: usize,

    /// Random cost from the hash, before dividing by the weight.
    cost: u64,

    /// Member weight.
    shares: u32,
}

impl Score {
    /// Compare cost divided by weight, exactly, using integer math. Ties go to
    /// the lower position, which is the lower ID.
    fn compare(&self, other: &Self) -> std::cmp::Ordering {
        (u128::from(self.cost) * u128::from(other.shares))
            .cmp(&(u128::from(other.cost) * u128::from(self.shares)))
            .then(self.node.cmp(&other.node))
    }
}

/// Return the slot for a key, in `0..SLOT_COUNT`.
///
/// The key can be any bytes. Use the same [`Member`] type as the membership
/// you rank with. This function does not check [`Member::DOMAIN`];
/// [`Membership::new`] does.
#[must_use]
pub fn slot<M: Member>(key: &[u8]) -> u32 {
    let mut digest = hash::domain::<M>(b"/slot/v1\0");
    digest.update(key);
    let digest = hash::finish(digest);
    u32::from_be_bytes([digest[0], digest[1], digest[2], digest[3]]) >> (u32::BITS - SLOT_BITS)
}

/// Return -log2((sample + 1) / 2^64) as a Q32.32 fixed-point number.
/// It uses only integer math, so every platform gets the same answer.
fn exponential_cost(sample: u64) -> u64 {
    let value = u128::from(sample) + 1;
    let exponent = value.ilog2();
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
    /// Score one member for a slot.
    #[cfg_attr(
        not(test),
        allow(
            clippy::unused_self,
            reason = "test builds count scoring work on this ranking"
        )
    )]
    fn score<M: Member>(&mut self, membership: &Membership<M>, slot: u32, index: usize) -> Score {
        #[cfg(test)]
        {
            self.scored += 1;
        }
        let mut digest = hash::domain::<M>(b"/hrw/v1\0");
        digest.update(slot.to_be_bytes());
        hash::bytes(&mut digest, membership.id(index));
        let digest = hash::finish(digest);
        Score {
            node: index,
            cost: exponential_cost(u64::from_be_bytes(digest[..8].try_into().unwrap())),
            shares: membership.weight(index).get(),
        }
    }

    /// Add a score, keeping only the best three.
    fn insert(&mut self, score: Score) {
        let position = self.best.partition_point(|old| old.compare(&score).is_lt());
        if position < REPLICAS {
            self.best.insert(position, score);
            self.best.truncate(REPLICAS);
        }
    }

    /// Build this slot's ranking for the new membership from the old one,
    /// scoring only the changed members.
    ///
    /// Returns `None` when that could give a wrong answer and a full rescore is
    /// needed: there is no change list, the old ranking is unfinished, or a
    /// winner left or lost weight (an unseen fourth member could move up).
    fn updated<M: Member>(&self, membership: &Membership<M>, slot: u32) -> Option<Self> {
        let delta = membership.delta.as_ref()?;
        debug_assert!(delta.changes.len() <= MAX_INCREMENTAL_CHANGES);
        if self.cursor != delta.old_count {
            return None;
        }
        let mut next = Self {
            cursor: membership.members().len(),
            ..Self::default()
        };
        for old in &self.best {
            if let Some(change) = delta
                .changes
                .iter()
                .find(|change| change.old_position() == Some(old.node))
            {
                let new = change.new_position()?;
                // A worse retained winner may expose an unretained fourth node.
                if membership.weight(new).get() < old.shares {
                    return None;
                }
                let score = next.score(membership, slot, new);
                next.insert(score);
            } else {
                let removed = delta
                    .changes
                    .iter()
                    .filter(|change| matches!(change, MemberChange::Removed { old: index } if *index < old.node))
                    .count();
                let mut node = old.node - removed;
                for change in &delta.changes {
                    if matches!(change, MemberChange::Added { new } if *new <= node) {
                        node += 1;
                    }
                }
                next.insert(Score { node, ..*old });
            }
        }
        for change in &delta.changes {
            if let Some(index) = change.new_position()
                && !next.best.iter().any(|score| score.node == index)
            {
                let score = next.score(membership, slot, index);
                next.insert(score);
            }
        }
        #[cfg(test)]
        {
            next.incremental = true;
        }
        Some(next)
    }

    /// Score up to `work` more members, continuing where the last call stopped.
    fn advance<M: Member>(&mut self, membership: &Membership<M>, slot: u32, work: usize) {
        let end = self
            .cursor
            .saturating_add(work)
            .min(membership.members().len());
        for index in self.cursor..end {
            let score = self.score(membership, slot, index);
            self.insert(score);
        }
        self.cursor = end;
    }

    /// Return the ranked member positions, best first.
    fn candidates(&self) -> Vec<usize> {
        self.best.iter().map(|score| score.node).collect()
    }
}

impl Placement {
    /// Estimated bytes per cache entry.
    ///
    /// This is a generous estimate of the map node, `Rc`, `RefCell`, and score
    /// vector. It does not cover allocator overhead or fragmentation, which
    /// vary by platform. Tests check that the real structures fit.
    pub const ENTRY_BYTES: usize = 1024;

    /// Create a placement that caches up to `capacity` entries.
    ///
    /// Zero turns caching off. Then concurrent [`Self::rank_async`] calls do
    /// not share work, so callers must limit how many run at once.
    #[must_use]
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity,
            cache: RefCell::default(),
            maintenance: RefCell::default(),
        }
    }

    /// Estimated bytes used, including this `Placement` itself. Does not count
    /// returned vectors or uncached requests.
    #[must_use]
    pub fn retained_bytes(&self) -> usize {
        let cache = self.cache.borrow();
        std::mem::size_of::<Self>()
            .saturating_add(cache.entries.len().saturating_mul(Self::ENTRY_BYTES))
    }

    /// Find the cache entry for a slot, or add one. Never evicts an entry that
    /// a request is using.
    fn ranking<M: Member>(
        &self,
        membership: &Membership<M>,
        slot: u32,
    ) -> Result<Rc<RefCell<Ranking>>, Error> {
        let key = (membership.identity(), slot);
        let mut cache = self.cache.borrow_mut();
        if let Some(ranking) = cache.entries.get(&key) {
            ranking.borrow_mut().referenced = true;
            return Ok(ranking.clone());
        }
        let predecessor = membership.delta.as_ref().map(|delta| (delta.base, slot));
        let old = predecessor.and_then(|key| cache.entries.get(&key));
        let updated = old.and_then(|ranking| ranking.borrow().updated(membership, slot));
        let replace = old.is_some_and(|ranking| Rc::strong_count(ranking) == 1);
        // Replace this slot, never some other populated predecessor slot. An
        // in-flight old request owns its immutable generation until it finishes.
        if replace {
            cache.remove(&predecessor.unwrap());
        } else if self.capacity != 0
            && cache.entries.len() == self.capacity
            && (old.is_some() || !cache.evict())
        {
            return Err(Error::Overloaded);
        }
        let ranking = Rc::new(RefCell::new(updated.unwrap_or_default()));
        if self.capacity != 0 {
            cache.entries.insert(key, ranking.clone());
            if cache.predecessor == Some(key.0) {
                cache.predecessor_dirty = true;
            }
        }
        Ok(ranking)
    }

    /// Return up to [`REPLICAS`] owners for `key`, best first.
    ///
    /// Does all the work before returning. If the cache is full of entries in
    /// use, it skips the cache instead of failing. An empty membership returns
    /// an empty list.
    ///
    /// # Errors
    ///
    /// Never fails today. The `Result` is kept for API compatibility.
    pub fn rank<M: Member>(
        &self,
        membership: &Membership<M>,
        key: &[u8],
    ) -> Result<Vec<usize>, Error> {
        let slot = slot::<M>(key);
        let ranking = match self.ranking(membership, slot) {
            Ok(ranking) => ranking,
            Err(Error::Overloaded) => Rc::new(RefCell::new(Ranking::default())),
            Err(error) => return Err(error),
        };
        let mut ranking = ranking.borrow_mut();
        ranking.advance(membership, slot, usize::MAX);
        Ok(ranking.candidates())
    }

    /// Like [`Self::rank`], but yields so it does not block the executor.
    ///
    /// Each poll scores at most 256 members. Adding a cache entry may also do a
    /// small update from the old membership and may scan the whole cache to
    /// evict something. Requests for the same slot share work. Dropping the
    /// future releases its hold on the entry but keeps the work done so far.
    ///
    /// # Errors
    ///
    /// Returns [`Error::Overloaded`] if the cache is full and cannot take this
    /// request. That happens when every entry is in use, or when this slot's
    /// old entry is in use and there is no room for a new one.
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

    /// Do one step of moving cached entries from the old membership to
    /// `membership`, so requests after a change still hit the cache.
    ///
    /// Each step either moves one old entry or scores up to 256 members for an
    /// entry that needs a full rescore. Call it repeatedly:
    ///
    /// - [`Maintenance::Progress`]: work was done. Call again.
    /// - [`Maintenance::Blocked`]: the next old entry is in use. Try later.
    /// - [`Maintenance::Idle`]: done. Stays idle until a request adds another
    ///   old entry, which restarts the scan.
    ///
    /// Partial rescore work is held between steps so requests cannot evict it,
    /// and a restarted scan keeps it. Passing a different membership starts
    /// over and drops that work.
    ///
    /// # Errors
    ///
    /// Never fails today. An entry in use returns [`Maintenance::Blocked`]
    /// instead. The `Result` is kept for API compatibility.
    pub fn maintain<M: Member>(&self, membership: &Membership<M>) -> Result<Maintenance, Error> {
        use std::ops::Bound::{Excluded, Unbounded};
        let mut work = self.maintenance.borrow_mut();
        let identity = membership.identity();
        {
            let mut cache = self.cache.borrow_mut();
            let predecessor = membership.delta.as_ref().map(|delta| delta.base);
            if work.identity != Some(identity) || cache.predecessor != predecessor {
                *work = MaintenanceWork {
                    identity: Some(identity),
                    ..MaintenanceWork::default()
                };
                cache.predecessor = predecessor;
                cache.predecessor_dirty = false;
            } else if std::mem::take(&mut cache.predecessor_dirty) {
                // Keep active work pinned; only restart the predecessor scan.
                work.cursor = None;
                work.finished = false;
            }
        }
        if work.finished {
            return Ok(Maintenance::Idle);
        }
        if let Some((slot, ranking)) = &work.active {
            let done = {
                let mut ranking = ranking.borrow_mut();
                ranking.advance(membership, *slot, WORK_QUANTUM);
                ranking.cursor == membership.members().len()
            };
            if done {
                work.active = None;
            }
            return Ok(Maintenance::Progress);
        }
        let Some(delta) = &membership.delta else {
            work.finished = true;
            return Ok(Maintenance::Idle);
        };
        let next = {
            let cache = self.cache.borrow();
            match work.cursor {
                Some(key) => cache.entries.range((Excluded(key), Unbounded)).next(),
                None => cache.entries.range((delta.base, 0)..).next(),
            }
            .map(|(key, ranking)| (*key, Rc::strong_count(ranking) != 1))
        };
        let Some((key, pinned)) = next.filter(|(key, _)| key.0 == delta.base) else {
            work.finished = true;
            return Ok(Maintenance::Idle);
        };
        if pinned {
            return Ok(Maintenance::Blocked);
        }
        let ranking = self.ranking(membership, key.1)?;
        // If a foreground request already installed the new generation in spare
        // capacity, remove its now-unpinned predecessor as part of this migration.
        self.cache.borrow_mut().remove(&key);
        work.cursor = Some(key);
        if ranking.borrow().cursor != membership.members().len() {
            work.active = Some((key.1, ranking));
        }
        Ok(Maintenance::Progress)
    }
}

/// Tests for scoring, caching, async progress, and membership changes.
#[cfg(test)]
mod tests {
    use super::*;

    use std::{num::NonZeroU32, task::Context};

    /// Test member with a string ID and a weight.
    #[derive(Clone, Debug)]
    struct TestMember(String, NonZeroU32);

    impl Member for TestMember {
        const DOMAIN: &'static str = "placement-tests";

        /// Return the ID.
        fn id(&self) -> &[u8] {
            self.0.as_bytes()
        }

        /// Return the weight.
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }

    /// Build a member whose ID sorts in index order.
    fn member(index: usize, weight: u32) -> TestMember {
        TestMember(format!("node-{index:06}"), NonZeroU32::new(weight).unwrap())
    }

    /// Build `count` members with equal weights.
    fn membership(count: usize) -> Membership<TestMember> {
        Membership::new((0..count).map(|i| member(i, 4)).collect()).unwrap()
    }

    /// Turn a number into a key.
    fn key(value: u64) -> [u8; 8] {
        value.to_be_bytes()
    }

    /// Slots, rankings, and costs match known fixed values.
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
            (0, 325_512, [0, 2, 1]),
            (1, 733_552, [2, 1, 3]),
            (u64::MAX, 922_998, [0, 3, 2]),
        ] {
            assert_eq!(slot::<TestMember>(&key(page)), expected_slot);
            assert_eq!(
                placement.rank(&members, &key(page)).unwrap(),
                expected_order
            );
        }
        for (sample, cost) in [
            (0xe410_9581_2e88_5f6f, 715_971_622),
            (0xc425_1196_ce41_9070, 1_650_232_626),
            (0x9322_018f_0806_e768, 3_431_784_333),
            (0xb379_deab_a20d_903a, 2_200_536_977),
        ] {
            assert_eq!(exponential_cost(sample), cost);
        }
    }

    /// Cost math is right at the edges, and ties go to the lower ID.
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

    /// First owners are spread in proportion to weight.
    #[test]
    fn weighted_distribution_without_share_expansion() {
        let members = Membership::new(vec![member(0, 1), member(1, 3), member(2, 6)]).unwrap();
        let mut counts = [0usize; 3];
        for slot in 0..20_000 {
            let mut ranking = Ranking {
                cursor: 0,
                best: vec![],
                ..Ranking::default()
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

    /// Async requests share work, the cache stays bounded, and dropping a
    /// request releases its entry.
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
        assert_eq!(
            placement.rank(&members, &key(1)),
            Placement::new(0).rank(&members, &key(1))
        );
        assert_eq!(
            futures::executor::block_on(placement.rank_async(&members, &key(1))),
            Err(Error::Overloaded)
        );
        drop(first);
        let result = futures::executor::block_on(second).unwrap();
        assert_eq!(result, placement.rank(&members, &key(0)).unwrap());
        placement.rank(&members, &key(1)).unwrap();
        assert_eq!(placement.cache.borrow().entries.len(), 1);
        let uncached = Placement::new(0);
        uncached.rank(&members, &key(0)).unwrap();
        assert!(uncached.cache.borrow().entries.is_empty());
    }

    /// Different domains never share slots or cached results.
    #[test]
    fn generic_domain_isolates_identity_slot_and_cached_scores() {
        /// Same member data under another domain.
        struct Other(TestMember);

        impl Member for Other {
            const DOMAIN: &'static str = "object-store";

            /// Return the wrapped ID.
            fn id(&self) -> &[u8] {
                self.0.id()
            }

            /// Return the wrapped weight.
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

    /// After many joins, leaves, and weight changes, cached results still
    /// match a fresh computation.
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
            members[generation % 10].1 =
                NonZeroU32::new(u32::try_from(generation % 7 + 1).unwrap()).unwrap();
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

    /// Run maintenance until idle. Fail if it blocks or takes too many steps.
    fn warm_to_idle(placement: &Placement, members: &Membership<TestMember>, limit: usize) {
        for _ in 0..limit {
            match placement.maintain(members).unwrap() {
                Maintenance::Idle => return,
                Maintenance::Progress => {}
                Maintenance::Blocked => panic!("unexpected pinned predecessor"),
            }
        }
        panic!("maintenance did not finish within bounded turns");
    }

    /// Maintenance keeps every cached slot, using both cheap updates and full
    /// rescores.
    #[test]
    fn full_cache_warm_migration_retains_every_populated_slot() {
        for cold in [false, true] {
            let placement = Placement::new(32);
            let old = membership(600);
            for page in 0..32 {
                placement.rank(&old, &key(page)).unwrap();
            }
            let slots: Vec<_> = placement
                .cache
                .borrow()
                .entries
                .keys()
                .map(|key| key.1)
                .collect();
            assert_eq!(slots.len(), 32);
            let mut values = old.members().to_vec();
            if cold {
                // A removed winner exposes an unretained candidate.
                let winner = placement.rank(&old, &key(0)).unwrap()[0];
                values.remove(winner);
            } else {
                values.push(member(9999, 4));
            }
            let next = Membership::new(values).unwrap().with_predecessor(&old);
            warm_to_idle(&placement, &next, 200);
            let cache = placement.cache.borrow();
            assert_eq!(cache.entries.len(), slots.len());
            let mut cold_slots = 0;
            for slot in slots {
                let ranking = cache
                    .entries
                    .get(&(next.identity(), slot))
                    .expect("lost populated slot")
                    .borrow();
                assert_eq!(ranking.cursor, next.members().len());
                let mut oracle = Ranking::default();
                oracle.advance(&next, slot, usize::MAX);
                assert_eq!(ranking.candidates(), oracle.candidates());
                if ranking.incremental {
                    assert!(ranking.scored <= MAX_INCREMENTAL_CHANGES + REPLICAS);
                } else {
                    cold_slots += 1;
                    assert_eq!(ranking.scored, next.members().len());
                }
            }
            assert_eq!(cold_slots > 0, cold);
            assert!(!cache.entries.keys().any(|key| key.0 == old.identity()));
        }
    }

    /// A finished scan restarts only when an old entry is added.
    #[test]
    fn late_predecessor_admission_reopens_completed_scan_only_when_relevant() {
        let placement = Placement::new(8);
        let old = membership(20);
        let next = membership(21).with_predecessor(&old);
        placement.rank(&old, &key(0)).unwrap();
        warm_to_idle(&placement, &next, 10);
        let completed_cursor = placement.maintenance.borrow().cursor;
        assert!(completed_cursor.is_some());
        for value in 0..3 {
            placement.rank(&next, &key(value)).unwrap();
            assert!(!placement.cache.borrow().predecessor_dirty);
            assert_eq!(placement.maintain(&next), Ok(Maintenance::Idle));
            assert_eq!(placement.maintenance.borrow().cursor, completed_cursor);
        }
        let late_slot = slot::<TestMember>(&key(3));
        placement.rank(&old, &key(3)).unwrap();
        assert!(placement.cache.borrow().predecessor_dirty);
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
        warm_to_idle(&placement, &next, 10);
        let cache = placement.cache.borrow();
        assert!(!cache.entries.contains_key(&(old.identity(), late_slot)));
        let warmed = cache.entries[&(next.identity(), late_slot)].borrow();
        assert!(warmed.incremental);
        assert_eq!(
            warmed.candidates(),
            Placement::new(0).rank(&next, &key(3)).unwrap()
        );
        drop(warmed);
        drop(cache);
        let completed_cursor = placement.maintenance.borrow().cursor;
        for _ in 0..3 {
            assert_eq!(placement.maintain(&next), Ok(Maintenance::Idle));
            assert_eq!(placement.maintenance.borrow().cursor, completed_cursor);
        }
    }

    /// Restarting the scan keeps partial work and waits for old requests that
    /// are still running.
    #[test]
    fn late_predecessor_behind_cursor_preserves_active_work_and_grace_pins() {
        let placement = Placement::new(4);
        let old = membership(1000);
        let mut keys = [key(0), key(1)];
        keys.sort_by_key(|key| slot::<TestMember>(key));
        let [late_key, active_key] = keys;
        let late_slot = slot::<TestMember>(&late_key);
        let active_slot = slot::<TestMember>(&active_key);
        assert!(late_slot < active_slot);
        let winner = placement.rank(&old, &active_key).unwrap()[0];
        let mut values = old.members().to_vec();
        values.remove(winner);
        let next = Membership::new(values).unwrap().with_predecessor(&old);
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
        assert_eq!(
            placement.maintenance.borrow().cursor,
            Some((old.identity(), active_slot))
        );
        let active = placement.cache.borrow().entries[&(next.identity(), active_slot)].clone();
        assert_eq!(active.borrow().cursor, WORK_QUANTUM);
        let mut grace = Box::pin(placement.rank_async(&old, &late_key));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(grace.as_mut().poll(&mut cx).is_pending());
        assert!(placement.cache.borrow().predecessor_dirty);
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
        assert!(placement.maintenance.borrow().cursor.is_none());
        assert_eq!(active.borrow().cursor, 2 * WORK_QUANTUM);
        assert!(Rc::ptr_eq(
            &active,
            &placement.maintenance.borrow().active.as_ref().unwrap().1
        ));
        for _ in 0..2 {
            assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
        }
        assert_eq!(active.borrow().scored, next.members().len());
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));
        assert_eq!(
            futures::executor::block_on(grace),
            Placement::new(0).rank(&old, &late_key)
        );
        warm_to_idle(&placement, &next, 10);
        let cache = placement.cache.borrow();
        assert!(!cache.entries.contains_key(&(old.identity(), late_slot)));
        assert_eq!(
            cache.entries[&(next.identity(), late_slot)]
                .borrow()
                .candidates(),
            Placement::new(0).rank(&next, &late_key).unwrap()
        );
    }

    /// An old entry in use blocks maintenance without losing its place.
    #[test]
    fn pinned_predecessor_blocks_without_losing_cursor_or_old_request() {
        let placement = Placement::new(1);
        let old = membership(1000);
        let mut pending = Box::pin(placement.rank_async(&old, &key(0)));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(pending.as_mut().poll(&mut cx).is_pending());
        let mut values = old.members().to_vec();
        values.push(member(9999, 4));
        let next = Membership::new(values).unwrap().with_predecessor(&old);
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));
        assert!(placement.maintenance.borrow().cursor.is_none());
        assert_eq!(
            placement.rank(&next, &key(0)),
            Placement::new(0).rank(&next, &key(0))
        );
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Blocked));
        assert_eq!(
            futures::executor::block_on(pending),
            Placement::new(0).rank(&old, &key(0))
        );
        warm_to_idle(&placement, &next, 10);
        assert!(
            placement
                .cache
                .borrow()
                .entries
                .contains_key(&(next.identity(), slot::<TestMember>(&key(0))))
        );
    }

    /// Requests share maintenance's partial work and cannot evict it.
    #[test]
    fn cold_maintenance_progress_is_pinned_and_coalesces_with_requests() {
        let placement = Placement::new(1);
        let old = membership(1000);
        let winner = placement.rank(&old, &key(0)).unwrap()[0];
        let mut values = old.members().to_vec();
        values.remove(winner);
        let next = Membership::new(values).unwrap().with_predecessor(&old);
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
        assert_eq!(placement.maintain(&next), Ok(Maintenance::Progress));
        let cache_key = (next.identity(), slot::<TestMember>(&key(0)));
        assert_eq!(
            placement.cache.borrow().entries[&cache_key].borrow().cursor,
            WORK_QUANTUM
        );
        let mut pending = Box::pin(placement.rank_async(&next, &key(0)));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(pending.as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            placement.cache.borrow().entries[&cache_key].borrow().cursor,
            2 * WORK_QUANTUM
        );
        assert_eq!(
            futures::executor::block_on(placement.rank_async(&next, &key(1))),
            Err(Error::Overloaded)
        );
        assert_eq!(
            placement.rank(&next, &key(1)),
            Placement::new(0).rank(&next, &key(1))
        );
        drop(pending);
        warm_to_idle(&placement, &next, 10);
        assert_eq!(
            placement.cache.borrow().entries[&cache_key].borrow().scored,
            next.members().len()
        );
    }

    /// CLOCK skips long runs of busy entries and gives recently used entries a
    /// second chance.
    #[test]
    fn clock_rotation_inspects_more_than_64_busy_entries_and_refreshes_hits() {
        let placement = Placement::new(80);
        let members = membership(1);
        let mut pinned = Vec::new();
        for slot in 0..79 {
            pinned.push(placement.ranking(&members, slot).unwrap());
        }
        drop(placement.ranking(&members, 79).unwrap());
        drop(placement.ranking(&members, 80).unwrap());
        assert!(
            !placement
                .cache
                .borrow()
                .entries
                .contains_key(&(members.identity(), 79))
        );
        assert!(
            placement
                .cache
                .borrow()
                .entries
                .contains_key(&(members.identity(), 80))
        );
        assert_eq!(placement.cache.borrow().entries.len(), 80);
        drop(pinned);
        let placement = Placement::new(3);
        for slot in 0..3 {
            drop(placement.ranking(&members, slot).unwrap());
        }
        drop(placement.ranking(&members, 3).unwrap()); // clears references, evicts 0
        drop(placement.ranking(&members, 1).unwrap()); // refreshes reference on hit
        drop(placement.ranking(&members, 4).unwrap()); // second chance saves 1
        let cache = placement.cache.borrow();
        assert!(cache.entries.contains_key(&(members.identity(), 1)));
        assert!(!cache.entries.contains_key(&(members.identity(), 2)));
    }

    /// Changing a record after it is frozen does not change results.
    #[test]
    fn scoring_uses_frozen_snapshots_not_live_member_accessors() {
        use std::cell::Cell;

        /// Record whose ID and weight can change.
        struct Mutable {
            id: Cell<&'static [u8]>,

            weight: Cell<NonZeroU32>,
        }

        impl Member for Mutable {
            const DOMAIN: &'static str = "mutable-placement";

            /// Return the current ID.
            fn id(&self) -> &[u8] {
                self.id.get()
            }

            /// Return the current weight.
            fn weight(&self) -> NonZeroU32 {
                self.weight.get()
            }
        }
        let members = Membership::new(
            (0..4)
                .map(|index| Mutable {
                    id: Cell::new([b"a", b"b", b"c", b"d"][index].as_slice()),
                    weight: Cell::new(NonZeroU32::new(u32::try_from(index).unwrap() + 1).unwrap()),
                })
                .collect(),
        )
        .unwrap();
        let expected: Vec<_> = (0..20)
            .map(|page| Placement::new(0).rank(&members, &key(page)).unwrap())
            .collect();
        for value in members.members() {
            value.id.set(b"changed");
            value.weight.set(NonZeroU32::new(u32::MAX).unwrap());
        }
        for (page, expected) in expected.into_iter().enumerate() {
            assert_eq!(
                Placement::new(0)
                    .rank(&members, &key(u64::try_from(page).unwrap()))
                    .unwrap(),
                expected
            );
        }
    }

    /// Cheap updates and full rescores both match a plain sort of all scores.
    #[test]
    fn randomized_incremental_diff_matches_full_sort_oracle() {
        let mut state = 0x1365_a739_2135_bcedu64;
        let mut draw = || {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            state
        };
        let mut fast = 0;
        let mut cold = 0;
        for _ in 0..120 {
            let mut before = Vec::new();
            let mut after = Vec::new();
            for index in 0..40 {
                let sample = draw();
                if sample % 3 != 0 {
                    before.push(member(
                        index,
                        u32::try_from((sample >> 32) % 20).unwrap() + 1,
                    ));
                }
                if sample % 5 != 0 {
                    after.push(member(
                        index,
                        u32::try_from(((sample >> 16) & u64::from(u32::MAX)) % 20).unwrap() + 1,
                    ));
                }
            }
            let old = Membership::new(before).unwrap();
            let next = Membership::new(after).unwrap().with_predecessor(&old);
            for slot in 0..16 {
                let mut previous = Ranking::default();
                previous.advance(&old, slot, usize::MAX);
                let mut updated = previous.updated(&next, slot).unwrap_or_default();
                if updated.incremental {
                    fast += 1;
                } else {
                    cold += 1;
                }
                updated.advance(&next, slot, usize::MAX);
                let mut scoring = Ranking::default();
                let mut all: Vec<_> = (0..next.members().len())
                    .map(|index| scoring.score(&next, slot, index))
                    .collect();
                all.sort_by(Score::compare);
                let expected: Vec<_> = all.iter().take(REPLICAS).map(|score| score.node).collect();
                assert_eq!(updated.candidates(), expected);
            }
        }
        assert!(fast > 0 && cold > 0, "fast={fast}, cold={cold}");
    }

    /// Cost never goes up as the sample goes up.
    #[test]
    fn exponential_is_monotonic_including_extrema_and_power_boundaries() {
        let mut samples = vec![0, 1, u64::MAX - 1, u64::MAX];
        for bit in 0..64 {
            let pivot = 1u64 << bit;
            samples.extend([pivot - 1, pivot, pivot.saturating_add(1)]);
        }
        for index in 0..20_000 {
            samples.push((u64::MAX / 20_000) * index);
        }
        samples.sort_unstable();
        for pair in samples.windows(2) {
            assert!(
                exponential_cost(pair[0]) >= exponential_cost(pair[1]),
                "{pair:?}"
            );
        }
        assert_eq!(exponential_cost(0), 64u64 << 32);
        assert_eq!(exponential_cost(u64::MAX), 1);
        assert_eq!(SLOT_COUNT, 1_048_576);
    }

    /// `ENTRY_BYTES` covers the real per-entry structures.
    #[test]
    fn entry_budget_covers_structures_and_dynamic_score_capacity() {
        use std::mem::size_of;
        let mut ranking = Ranking::default();
        ranking.advance(&membership(100), 0, usize::MAX);
        assert_eq!(ranking.best.capacity(), REPLICAS + 1);
        // Explicit standard-library layout assumptions, not an allocator contract.
        let map_node = 11 * size_of::<(CacheKey, Rc<RefCell<Ranking>>)>()
            + 12 * size_of::<usize>()
            + 4 * size_of::<usize>();
        let rc = 2 * size_of::<usize>() + size_of::<RefCell<Ranking>>();
        let dynamic_scores = ranking.best.capacity() * size_of::<Score>();
        let clock = size_of::<RankingCache>();
        assert!(map_node + rc + dynamic_scores + clock <= Placement::ENTRY_BYTES);
        let placement = Placement::new(1);
        let empty_bytes = placement.retained_bytes();
        placement.rank(&membership(4), b"key").unwrap();
        assert!(placement.retained_bytes() >= empty_bytes + Placement::ENTRY_BYTES);
    }
}
