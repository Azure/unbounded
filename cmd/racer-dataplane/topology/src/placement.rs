//! Integer weighted rendezvous placement with bounded cooperative scoring.
use crate::{Error, Member, Membership, hash, membership::MAX_INCREMENTAL_CHANGES};
use sha2::Digest;
use std::{cell::RefCell, collections::BTreeMap, future::Future, rc::Rc, task::Poll};

/// Maximum number of distinct members returned by placement, in preference order.
pub const REPLICAS: usize = 3;
/// Number of high digest bits used to select a placement slot.
pub const SLOT_BITS: u32 = 20;
/// Number of placement slots; keys in the same slot share a ranking.
pub const SLOT_COUNT: u32 = 1 << SLOT_BITS;
const WORK_QUANTUM: usize = 256;

/// Outcome of one cooperative maintenance turn.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Maintenance {
    /// No more resident predecessor slots need work.
    Idle,
    /// A slot was migrated or a cold scoring quantum completed.
    Progress,
    /// A predecessor is pinned by a request; retry after requests progress.
    Blocked,
}

/// Worker-local weighted rendezvous rankings over immutable memberships.
///
/// A bounded CLOCK cache coalesces resident work. Rankings contain positions in
/// the supplied membership's ID-sorted slice, not application-owned records.
/// This type is not thread-safe; use a separate instance for each worker.
pub struct Placement {
    capacity: usize,
    cache: RefCell<RankingCache>,
    maintenance: RefCell<MaintenanceWork>,
}

#[derive(Default)]
struct MaintenanceWork {
    identity: Option<[u8; 32]>,
    cursor: Option<CacheKey>,
    active: Option<(u32, Rc<RefCell<Ranking>>)>,
    finished: bool,
}

type CacheKey = ([u8; 32], u32);
#[derive(Default)]
struct RankingCache {
    entries: BTreeMap<CacheKey, Rc<RefCell<Ranking>>>,
    hand: Option<CacheKey>,
    // Only admissions to the current predecessor can invalidate the scan.
    // A latch coalesces arbitrary insertions without an unbounded dirty queue.
    predecessor: Option<[u8; 32]>,
    predecessor_dirty: bool,
}

impl RankingCache {
    fn remove(&mut self, key: &CacheKey) {
        self.entries.remove(key);
    }

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

struct Ranking {
    cursor: usize,
    best: Vec<Score>,
    referenced: bool,
    #[cfg(test)]
    scored: usize,
    #[cfg(test)]
    incremental: bool,
}

impl Default for Ranking {
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

/// Map an application-encoded key to its domain-separated placement slot.
///
/// Returns a value in `0..SLOT_COUNT`. Keys are opaque bytes with no prescribed
/// schema. Use the same [`Member::DOMAIN`] as the membership being ranked;
/// membership construction validates the domain, but this helper does not.
#[must_use]
pub fn slot<M: Member>(key: &[u8]) -> u32 {
    let mut digest = hash::domain::<M>(b"/slot/v1\0");
    digest.update(key);
    let digest = hash::finish(digest);
    u32::from_be_bytes([digest[0], digest[1], digest[2], digest[3]]) >> (u32::BITS - SLOT_BITS)
}

/// Q32.32 approximation of -log2((sample+1)/2^64). Binary logarithm
/// via repeated squaring is fully specified integer arithmetic on all targets.
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

    fn insert(&mut self, score: Score) {
        let position = self.best.partition_point(|old| old.compare(&score).is_lt());
        if position < REPLICAS {
            self.best.insert(position, score);
            self.best.truncate(REPLICAS);
        }
    }

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
            if let Some((_, new)) = delta
                .changes
                .iter()
                .find(|(index, _)| *index == Some(old.node))
            {
                let new = (*new)?;
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
                    .filter(|(a, b)| b.is_none() && a.is_some_and(|a| a < old.node))
                    .count();
                let mut node = old.node - removed;
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
                let score = next.score(membership, slot, *index);
                next.insert(score);
            }
        }
        #[cfg(test)]
        {
            next.incremental = true;
        }
        Some(next)
    }

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

    fn candidates(&self) -> Vec<usize> {
        self.best.iter().map(|score| score.node).collect()
    }
}

impl Placement {
    /// Conservative structural budget per resident entry: map occupancy/slack,
    /// `Rc`/`RefCell`, and the four-score vector allocation. This is
    /// not an allocator hard bound: allocator metadata, size classes, and process
    /// fragmentation are platform-dependent. Regression tests check the modeled
    /// structures and actual vector capacities against this budget.
    pub const ENTRY_BYTES: usize = 1024;

    /// Create an empty cache retaining at most `capacity` slot/generation pairs.
    ///
    /// Zero capacity disables caching. Concurrent uncached asynchronous requests
    /// do not coalesce; callers must bound their total number independently.
    #[must_use]
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity,
            cache: RefCell::default(),
            maintenance: RefCell::default(),
        }
    }

    /// Conservative resident storage estimate, including the inline cache and
    /// CLOCK cursor. Excludes caller-owned result vectors and uncached requests.
    #[must_use]
    pub fn retained_bytes(&self) -> usize {
        let cache = self.cache.borrow();
        std::mem::size_of::<Self>()
            .saturating_add(cache.entries.len().saturating_mul(Self::ENTRY_BYTES))
    }

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

    /// Rank up to [`REPLICAS`] member indices. Cache pressure never rejects a
    /// synchronous request: if every eligible entry is pinned, score uncached.
    /// Empty memberships return an empty vector. Cold work runs to completion.
    ///
    /// # Errors
    ///
    /// Currently always succeeds for a constructed membership. The `Result`
    /// return type is retained for compatibility with fallible placement callers.
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

    /// Rank member indices cooperatively, coalescing resident requests.
    /// Each poll scores at most 256 cold
    /// members (plus a bounded membership delta on admission). CLOCK admission
    /// may inspect the whole bounded cache; no borrow is held across a yield.
    /// Dropping the future releases its admission without discarding resident
    /// progress. Results have the same ordering as [`Self::rank`].
    ///
    /// # Errors
    ///
    /// Returns [`Error::Overloaded`] when a nonzero-capacity cache cannot admit
    /// the request: all eviction candidates are pinned, or its own predecessor
    /// is pinned and there is no spare capacity for another generation.
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

    /// Migrate one resident predecessor slot or advance one cold quantum. A
    /// pinned predecessor returns [`Maintenance::Blocked`] without advancing the scan cursor.
    /// Active cold work is pinned across turns so requests cannot evict progress.
    /// [`Maintenance::Idle`] means the current scan is complete; switching to a
    /// different membership resets the scan and releases any old active work.
    /// A newly admitted predecessor slot reopens the scan on the next turn,
    /// including grace requests behind the cursor or after completion. Such
    /// admissions never discard active cold progress or its pin. Without new
    /// predecessor admissions, a completed scan stays idle without rescanning.
    ///
    /// # Errors
    ///
    /// Currently succeeds for constructed memberships: pinned entries report
    /// [`Maintenance::Blocked`], not cache overload. The `Result` return type is
    /// retained for compatibility with fallible maintenance callers.
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

#[cfg(test)]
#[path = "placement/tests.rs"]
mod tests;
