//! Memory cache recency indexes, generic over the cached value.

use std::{
    collections::{BTreeMap, HashMap, hash_map::RandomState},
    hash::{BuildHasher, Hash},
    ops::Bound::{Excluded, Included, Unbounded},
};

/// Most entries one reclamation pass examines, per lane or overall.
pub const RECLAIM_WINDOW: usize = 256;

/// Most lanes one global pressure pass examines. The window rotates, so every lane with entries is
/// checked within `ceil(lanes / PRESSURE_WINDOW)` full passes.
pub const PRESSURE_WINDOW: usize = 8;

/// Assigns each key to a lane (for the dataplane, its cache) so one lane's
/// entries can be reclaimed without scanning the others.
pub trait Grouping<K> {
    /// Lane identifier.
    type Id: Ord + Clone;

    /// The key's lane, or `None` to track no lanes at all.
    fn group(key: &K) -> Option<Self::Id>;
}

/// No lanes: every operation skips lane bookkeeping.
#[derive(Clone, Copy, Debug, Default)]
pub struct Ungrouped;

impl<K> Grouping<K> for Ungrouped {
    type Id = ();

    fn group(_: &K) -> Option<()> {
        None
    }
}

/// One lane's keys in tick order, with its own resumable reclaim cursor.
pub struct Lane<K> {
    /// Keys by tick, oldest first.
    pub order: BTreeMap<u64, K>,

    /// Tick where the last pass over this lane stopped.
    pub after: u64,
}

impl<K> Default for Lane<K> {
    fn default() -> Self {
        Self {
            order: BTreeMap::new(),
            after: 0,
        }
    }
}

/// Per-lane tick order. Each insert, touch, and remove costs O(log lanes + log entries); nothing
/// here walks all lanes or entries. Empty lanes are dropped.
pub struct Lanes<G, K>(pub BTreeMap<G, Lane<K>>);

impl<G, K> Default for Lanes<G, K> {
    fn default() -> Self {
        Self(BTreeMap::new())
    }
}

impl<G: Ord + Clone, K: Clone> Lanes<G, K> {
    /// Track `id` at `tick` in lane `group`.
    pub fn insert(&mut self, group: Option<G>, tick: u64, id: &K) {
        if let Some(group) = group {
            self.0
                .entry(group)
                .or_default()
                .order
                .insert(tick, id.clone());
        }
    }

    /// Forget tick `tick` in lane `group`.
    pub fn remove(&mut self, group: Option<G>, tick: u64) {
        let Some(group) = group else {
            return;
        };
        let Some(lane) = self.0.get_mut(&group) else {
            return;
        };
        lane.order.remove(&tick);
        if lane.order.is_empty() {
            self.0.remove(&group);
        }
    }

    /// Up to [`RECLAIM_WINDOW`] of one lane's entries, oldest first after its cursor, wrapping.
    #[must_use]
    pub fn candidates(&self, group: &G) -> Vec<(u64, K)> {
        let Some(lane) = self.0.get(group) else {
            return Vec::new();
        };
        lane.order
            .range((Excluded(lane.after), Unbounded))
            .chain(lane.order.range(..=lane.after))
            .take(RECLAIM_WINDOW)
            .map(|(tick, id)| (*tick, id.clone()))
            .collect()
    }

    /// Move lane `group`'s cursor to `tick`.
    pub fn advance(&mut self, group: &G, tick: u64) {
        if let Some(lane) = self.0.get_mut(group) {
            lane.after = tick;
        }
    }

    /// Up to [`PRESSURE_WINDOW`] lanes after `cursor`, wrapping.
    #[must_use]
    pub fn window(&self, cursor: Option<&G>) -> Vec<G> {
        let keys: Box<dyn Iterator<Item = &G>> = match cursor {
            Some(cursor) => Box::new(
                self.0
                    .range::<G, _>((Excluded(cursor), Unbounded))
                    .chain(self.0.range::<G, _>(..=cursor))
                    .map(|(group, _)| group),
            ),
            None => Box::new(self.0.keys()),
        };
        keys.take(PRESSURE_WINDOW).cloned().collect()
    }
}

/// Verified entries keyed by `K`, each with a unique recency tick.
pub struct TickLru<K, V, H = RandomState, G: Grouping<K> = Ungrouped> {
    /// Entries and their current tick.
    pub pages: HashMap<K, (u64, V), H>,

    /// Keys by tick, oldest first.
    pub lru: BTreeMap<u64, K>,

    /// Last tick handed out.
    pub clock: u64,

    /// Tick where the last reclamation pass stopped.
    pub reclaim_after: u64,

    /// Keys by lane, then tick.
    pub lanes: Lanes<G::Id, K>,
}

impl<K, V, H: Default, G: Grouping<K>> Default for TickLru<K, V, H, G> {
    fn default() -> Self {
        Self {
            pages: HashMap::with_hasher(H::default()),
            lru: BTreeMap::new(),
            clock: 0,
            reclaim_after: 0,
            lanes: Lanes::default(),
        }
    }
}

/// What a reclamation visitor wants done with one entry.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Visit {
    /// Keep the entry and look at the next one.
    Skip,

    /// Remove the entry and look at the next one.
    Remove,

    /// Keep the entry and stop. The cursor moves to it.
    Last,

    /// Stop before this entry. The cursor does not move to it.
    Stop,
}

/// Tick exhaustion. The caller maps it to its own overload error.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Exhausted;

impl<K: Eq + Hash + Clone, V, H: BuildHasher, G: Grouping<K>> TickLru<K, V, H, G> {
    /// Number of entries.
    #[must_use]
    pub fn len(&self) -> usize {
        self.pages.len()
    }

    /// Whether there are no entries.
    #[must_use]
    pub fn is_empty(&self) -> bool {
        self.pages.is_empty()
    }

    /// Remove an entry and its tick.
    pub fn remove(&mut self, id: &K) -> Option<V> {
        let (tick, value) = self.pages.remove(id)?;
        self.lru.remove(&tick);
        self.lanes.remove(G::group(id), tick);
        Some(value)
    }

    /// Add an entry at the newest tick. The key must not be present.
    pub fn insert(&mut self, id: K, value: V) -> Result<(), Exhausted> {
        self.clock = self.clock.checked_add(1).ok_or(Exhausted)?;
        self.lru.insert(self.clock, id.clone());
        self.lanes.insert(G::group(&id), self.clock, &id);
        self.pages.insert(id, (self.clock, value));
        Ok(())
    }

    /// Move `id` to the newest tick. Missing keys are ignored.
    pub fn touch(&mut self, id: &K) -> Result<(), Exhausted> {
        let Some((old, _)) = self.pages.get(id) else {
            return Ok(());
        };
        let old = *old;
        self.clock = self.clock.checked_add(1).ok_or(Exhausted)?;
        let tick = self.clock;
        self.lru.remove(&old);
        self.lru.insert(tick, id.clone());
        let group = G::group(id);
        self.lanes.remove(group.clone(), old);
        self.lanes.insert(group, tick, id);
        self.pages.get_mut(id).unwrap().0 = tick;
        Ok(())
    }

    /// Up to [`RECLAIM_WINDOW`] entries after the cursor, wrapping around.
    #[must_use]
    pub fn candidates(&self) -> Vec<(u64, K)> {
        self.lru
            .range((Excluded(self.reclaim_after), Unbounded))
            .chain(self.lru.range(..=self.reclaim_after))
            .take(RECLAIM_WINDOW)
            .map(|(tick, id)| (*tick, id.clone()))
            .collect()
    }

    /// Admission victim: the first candidate `idle` accepts, moving the cursor
    /// past every candidate examined. The victim is not removed.
    pub fn victim(&mut self, mut idle: impl FnMut(&V) -> bool) -> Option<K> {
        self.candidates().into_iter().find_map(|(tick, id)| {
            self.reclaim_after = tick;
            idle(&self.pages[&id].1).then_some(id)
        })
    }

    /// One bounded reclamation pass. `visit` decides each candidate in order. The cursor moves to
    /// every candidate decided, except one that returns [`Visit::Stop`].
    pub fn reclaim(&mut self, mut visit: impl FnMut(&K, &V) -> Visit) {
        for (tick, id) in self.candidates() {
            let decision = visit(&id, &self.pages[&id].1);
            if decision == Visit::Stop {
                return;
            }
            self.reclaim_after = tick;
            match decision {
                Visit::Remove => {
                    self.remove(&id);
                }
                Visit::Last => return,
                Visit::Skip | Visit::Stop => {}
            }
        }
    }

    /// At most `batch` entries with ticks in `(after, cutoff]`, oldest first,
    /// and the cursor for the next batch (`None` when this batch was short).
    #[must_use]
    pub fn expiring(&self, after: u64, cutoff: u64, batch: usize) -> (Vec<(u64, K)>, Option<u64>) {
        expiring(&self.lru, after, cutoff, batch)
    }
}

/// Shared idle-expiry selection over a tick index.
fn expiring<K: Clone>(
    order: &BTreeMap<u64, K>,
    after: u64,
    cutoff: u64,
    batch: usize,
) -> (Vec<(u64, K)>, Option<u64>) {
    let selected: Vec<_> = order
        .range((Excluded(after), Included(cutoff)))
        .take(batch)
        .map(|(tick, id)| (*tick, id.clone()))
        .collect();
    let next = (selected.len() == batch)
        .then(|| selected.last().map(|(tick, _)| *tick))
        .flatten();
    (selected, next)
}

/// Unverified entries ordered by key, with a recency tick per entry for idle expiry. Every mutation
/// goes through these methods so the tick index never drifts from the entries.
pub struct OrderedLru<K, V, H = RandomState, G: Grouping<K> = Ungrouped> {
    /// Entries by key.
    pub pages: BTreeMap<K, V>,

    /// Current tick of each entry.
    pub ticks: HashMap<K, u64, H>,

    /// Keys by tick, oldest first.
    pub order: BTreeMap<u64, K>,

    /// Last tick handed out.
    pub clock: u64,

    /// Keys by lane, then tick.
    pub lanes: Lanes<G::Id, K>,
}

impl<K, V, H: Default, G: Grouping<K>> Default for OrderedLru<K, V, H, G> {
    fn default() -> Self {
        Self {
            pages: BTreeMap::new(),
            ticks: HashMap::with_hasher(H::default()),
            order: BTreeMap::new(),
            clock: 0,
            lanes: Lanes::default(),
        }
    }
}

impl<K, V, H, G: Grouping<K>> std::ops::Deref for OrderedLru<K, V, H, G> {
    type Target = BTreeMap<K, V>;

    fn deref(&self) -> &Self::Target {
        &self.pages
    }
}

impl<K: Ord + Eq + Hash + Clone, V, H: BuildHasher, G: Grouping<K>> OrderedLru<K, V, H, G> {
    /// Move `id` to the newest tick. Missing keys are ignored.
    pub fn touch(&mut self, id: &K) {
        if !self.pages.contains_key(id) {
            return;
        }
        // Wrapping is unreachable in practice (2^64 touches); saturating keeps
        // the index consistent even then, at worst making order ambiguous.
        self.clock = self.clock.saturating_add(1);
        let group = G::group(id);
        if let Some(old) = self.ticks.insert(id.clone(), self.clock) {
            self.order.remove(&old);
            self.lanes.remove(group.clone(), old);
        }
        self.order.insert(self.clock, id.clone());
        self.lanes.insert(group, self.clock, id);
    }

    /// Add or replace an entry at the newest tick.
    pub fn insert(&mut self, id: K, page: V) {
        self.pages.insert(id.clone(), page);
        self.touch(&id);
    }

    /// Remove an entry and its tick.
    pub fn remove(&mut self, id: &K) -> Option<V> {
        let page = self.pages.remove(id)?;
        if let Some(tick) = self.ticks.remove(id) {
            self.order.remove(&tick);
            self.lanes.remove(G::group(id), tick);
        }
        Some(page)
    }

    /// Remove every entry `keep` rejects.
    pub fn retain(&mut self, mut keep: impl FnMut(&K) -> bool) {
        let removed: Vec<_> = self.pages.keys().filter(|id| !keep(id)).cloned().collect();
        for id in removed {
            self.remove(&id);
        }
    }

    /// Up to [`RECLAIM_WINDOW`] keys after `cursor` in key order, wrapping.
    #[must_use]
    pub fn candidates(&self, cursor: Option<&K>) -> Vec<K> {
        self.pages
            .range::<K, _>((cursor.map_or(Unbounded, Excluded), Unbounded))
            .chain(
                self.pages
                    .iter()
                    .take_while(|(id, _)| cursor.is_some_and(|cursor| *id <= cursor)),
            )
            .take(RECLAIM_WINDOW)
            .map(|(id, _)| id.clone())
            .collect()
    }

    /// Same contract as [`TickLru::expiring`].
    #[must_use]
    pub fn expiring(&self, after: u64, cutoff: u64, batch: usize) -> (Vec<(u64, K)>, Option<u64>) {
        expiring(&self.order, after, cutoff, batch)
    }
}

/// Entries nobody inserted or hit for this long are released by the dataplane memory cache, so a
/// worker that served a burst does not keep its cached pages (and their pooled buffers) forever.
pub const IDLE_TTL: std::time::Duration = std::time::Duration::from_secs(30);
/// Minimum spacing of the clock checkpoints that map wall time to LRU ticks.
pub const EXPIRY_CHECKPOINT: std::time::Duration = std::time::Duration::from_secs(1);
/// Upper bound of entries examined per class by one idle expiry call.
pub const EXPIRY_BATCH: usize = 32;

/// Time-to-tick checkpoints and resumable expiry cursors. Between checkpoints
/// with no pass in progress, idle expiry does constant work.
#[derive(Default)]
pub struct Expiry {
    /// `(time, verified clock, unverified clock)`, oldest first. The front is
    /// kept as the newest checkpoint at least `IDLE_TTL` old.
    pub checkpoints: std::collections::VecDeque<(std::time::Instant, u64, u64)>,
    /// Last examined tick of an in-progress pass; `None` when no pass is due.
    pub verified: Option<u64>,
    pub ciphertext: Option<u64>,
}
impl Expiry {
    /// Record a checkpoint at most once per interval and return the tick
    /// cutoffs, if any checkpoint is at least `IDLE_TTL` old.
    pub fn advance(
        &mut self,
        now: std::time::Instant,
        verified: u64,
        ciphertext: u64,
    ) -> Option<(u64, u64)> {
        if self
            .checkpoints
            .back()
            .is_none_or(|(at, _, _)| now.saturating_duration_since(*at) >= EXPIRY_CHECKPOINT)
        {
            self.checkpoints.push_back((now, verified, ciphertext));
            // Each new checkpoint may move the cutoff: start a fresh pass that also revisits
            // entries that were busy last time. An unfinished pass keeps its cursor so a long busy
            // prefix cannot starve later entries; adopting the newer cutoff is safe because a
            // touched entry moves to a tick above every aged checkpoint.
            self.verified.get_or_insert(0);
            self.ciphertext.get_or_insert(0);
        }
        let old = |at: &std::time::Instant| now.saturating_duration_since(*at) >= IDLE_TTL;
        while self.checkpoints.get(1).is_some_and(|(at, _, _)| old(at)) {
            self.checkpoints.pop_front();
        }
        self.checkpoints
            .front()
            .filter(|(at, _, _)| old(at))
            .map(|(_, verified, ciphertext)| (*verified, *ciphertext))
    }
}

#[cfg(test)]
mod lru_tests {
    use super::*;

    #[test]
    fn tick_lru_orders_touches_and_wraps_cursor() {
        let mut lru = TickLru::<u32, bool>::default();
        assert!(lru.is_empty());
        for id in 0..4 {
            lru.insert(id, id % 2 == 1).unwrap();
        }
        lru.touch(&0).unwrap();
        lru.touch(&99).unwrap();
        assert_eq!(lru.candidates(), vec![(2, 1), (3, 2), (4, 3), (5, 0)]);
        assert_eq!(lru.victim(|idle| *idle), Some(1));
        assert_eq!(lru.reclaim_after, 2);
        assert_eq!(lru.remove(&1), Some(true));
        assert_eq!(lru.remove(&1), None);
        assert_eq!(lru.candidates(), vec![(3, 2), (4, 3), (5, 0)]);
        lru.reclaim_after = 4;
        assert_eq!(lru.candidates(), vec![(5, 0), (3, 2), (4, 3)]);
        assert_eq!(lru.victim(|_| false), None);
        assert_eq!(lru.reclaim_after, 4);
        let mut seen = Vec::new();
        lru.reclaim(|id, _| {
            seen.push(*id);
            if *id == 2 { Visit::Stop } else { Visit::Remove }
        });
        assert_eq!(seen, vec![0, 2]);
        assert_eq!(lru.reclaim_after, 5);
        assert_eq!(lru.len(), 2);
        lru.reclaim(|id, _| if *id == 2 { Visit::Last } else { Visit::Skip });
        assert_eq!(lru.reclaim_after, 3);
        assert_eq!(lru.len(), 2);
    }

    #[test]
    fn tick_lru_reports_exhaustion_without_change() {
        let mut lru = TickLru::<u32, ()>::default();
        lru.insert(1, ()).unwrap();
        lru.clock = u64::MAX;
        assert_eq!(lru.insert(2, ()), Err(Exhausted));
        assert_eq!(lru.touch(&1), Err(Exhausted));
        assert_eq!(lru.len(), 1);
        assert_eq!(lru.candidates(), vec![(1, 1)]);
    }

    #[test]
    fn expiry_batches_resume_and_finish() {
        let mut lru = TickLru::<u32, ()>::default();
        for id in 0..5 {
            lru.insert(id, ()).unwrap();
        }
        assert_eq!(lru.expiring(0, 4, 2), (vec![(1, 0), (2, 1)], Some(2)));
        assert_eq!(lru.expiring(2, 4, 2), (vec![(3, 2), (4, 3)], Some(4)));
        assert_eq!(lru.expiring(4, 4, 2), (vec![], None));
        assert_eq!(lru.expiring(0, 1, 2), (vec![(1, 0)], None));
    }

    /// Lanes by key parity.
    struct Parity;
    impl Grouping<u32> for Parity {
        type Id = u32;
        fn group(key: &u32) -> Option<u32> {
            Some(key % 2)
        }
    }

    fn assert_lanes<V>(lru: &TickLru<u32, V, RandomState, Parity>) {
        let mut total = 0;
        for (group, lane) in &lru.lanes.0 {
            assert!(!lane.order.is_empty(), "empty lane {group} kept");
            for (tick, id) in &lane.order {
                assert_eq!(id % 2, *group);
                assert_eq!(lru.pages[id].0, *tick);
                assert_eq!(lru.lru[tick], *id);
            }
            total += lane.order.len();
        }
        assert_eq!(total, lru.len());
    }

    #[test]
    fn lanes_track_insert_touch_remove_and_reclaim() {
        let mut lru = TickLru::<u32, (), RandomState, Parity>::default();
        for id in 0..6 {
            lru.insert(id, ()).unwrap();
        }
        assert_lanes(&lru);
        lru.touch(&0).unwrap();
        assert_lanes(&lru);
        assert_eq!(lru.lanes.candidates(&0), vec![(3, 2), (5, 4), (7, 0)]);
        lru.lanes.advance(&0, 3);
        assert_eq!(lru.lanes.candidates(&0), vec![(5, 4), (7, 0), (3, 2)]);
        assert_eq!(lru.lanes.candidates(&9), vec![]);
        lru.reclaim(|id, ()| {
            if id % 2 == 1 {
                Visit::Remove
            } else {
                Visit::Skip
            }
        });
        assert_lanes(&lru);
        assert!(!lru.lanes.0.contains_key(&1));
        for id in [0, 2, 4] {
            lru.remove(&id);
        }
        assert_lanes(&lru);
        assert!(lru.lanes.0.is_empty());
        // Ungrouped indexes track no lanes.
        let mut plain = TickLru::<u32, ()>::default();
        plain.insert(1, ()).unwrap();
        plain.touch(&1).unwrap();
        assert!(plain.lanes.0.is_empty());
    }

    #[test]
    fn ordered_lru_lanes_follow_ticks() {
        let mut lru = OrderedLru::<u32, (), RandomState, Parity>::default();
        for id in 0..4 {
            lru.insert(id, ());
        }
        lru.touch(&1);
        lru.insert(2, ());
        assert_eq!(
            lru.lanes.0[&1].order.values().copied().collect::<Vec<_>>(),
            vec![3, 1]
        );
        assert_eq!(
            lru.lanes.0[&0].order.values().copied().collect::<Vec<_>>(),
            vec![0, 2]
        );
        lru.retain(|id| id % 2 == 0);
        assert!(!lru.lanes.0.contains_key(&1));
        lru.remove(&0);
        lru.remove(&2);
        assert!(lru.lanes.0.is_empty());
    }

    #[test]
    fn lane_window_rotates_and_wraps() {
        let mut lanes = Lanes::<u32, u32>::default();
        for group in 0..(PRESSURE_WINDOW as u32 + 3) {
            lanes.insert(Some(group), u64::from(group), &group);
        }
        let first = lanes.window(None);
        assert_eq!(first, (0..PRESSURE_WINDOW as u32).collect::<Vec<_>>());
        let next = lanes.window(first.last());
        assert_eq!(next, vec![8, 9, 10, 0, 1, 2, 3, 4]);
        lanes.remove(Some(9), 9);
        assert_eq!(lanes.window(Some(&8)), vec![10, 0, 1, 2, 3, 4, 5, 6]);
        lanes.remove(None, 0);
        lanes.remove(Some(77), 0);
        assert_eq!(lanes.0.len(), PRESSURE_WINDOW + 2);
    }

    #[test]
    fn ordered_lru_keeps_ticks_consistent() {
        let mut lru = OrderedLru::<u32, &str>::default();
        lru.insert(3, "c");
        lru.insert(1, "a");
        lru.insert(2, "b");
        lru.touch(&3);
        lru.touch(&9);
        assert_eq!(lru.get(&1), Some(&"a"));
        assert_eq!(lru.candidates(None), vec![1, 2, 3]);
        assert_eq!(lru.candidates(Some(&1)), vec![2, 3, 1]);
        assert_eq!(lru.expiring(0, 10, 10).0, vec![(2, 1), (3, 2), (4, 3)]);
        lru.insert(1, "z");
        assert_eq!(lru.order.len(), 3);
        lru.retain(|id| *id != 2);
        assert_eq!(lru.remove(&2), None);
        assert_eq!(lru.ticks.len(), 2);
        assert_eq!(lru.order.len(), 2);
        assert_eq!(lru.remove(&1), Some("z"));
        assert_eq!(lru.order.values().copied().collect::<Vec<_>>(), vec![3]);
    }
}

// Disk segment value used to order CLOCK eviction candidates.

/// Most mappings inspected when scoring one segment.
pub const SCAN_BUDGET: usize = 256;

/// Highest retention score of one page: heat 0 to 3 plus 1 for ownership.
pub const MAX_SCORE: u8 = 4;
/// Highest value [`class_score`] returns.
pub const MAX_PAGE_VALUE: u8 = 2 * MAX_SCORE + 1;

/// Value charged for each mapping beyond [`SCAN_BUDGET`]: a full page at the
/// highest retention score.
#[must_use]
pub const fn unseen_value(page_bytes: u64) -> u64 {
    unseen_value_for(page_bytes, false)
}

/// [`unseen_value`] while some cache may be over its fair share: unseen
/// mappings are charged at the highest value [`class_score`] can give.
#[must_use]
pub const fn unseen_value_for(page_bytes: u64, any_over_share: bool) -> u64 {
    page_bytes.saturating_mul(if any_over_share {
        MAX_PAGE_VALUE as u64
    } else {
        MAX_SCORE as u64
    })
}

/// Per-byte value of one page given its retention `score` (at most [`MAX_SCORE`]). While any cache
/// is over share, every under-share page outvalues every over-share page; otherwise this is the
/// plain retention score.
#[must_use]
pub const fn class_score(score: u8, any_over_share: bool, page_over_share: bool) -> u8 {
    if !any_over_share || page_over_share {
        score
    } else {
        score + MAX_SCORE + 1
    }
}

/// Value of one resident page: payload bytes times retention score. A page
/// whose length cannot be derived counts as a full page.
#[must_use]
pub fn page_value(bytes: Option<u32>, page_bytes: u32, score: u8) -> u64 {
    u64::from(bytes.unwrap_or(page_bytes)) * u64::from(score)
}

#[cfg(test)]
mod segment_tests {
    use super::*;

    #[test]
    fn page_value_uses_length_or_full_page() {
        assert_eq!(page_value(Some(10), 100, 3), 30);
        assert_eq!(page_value(None, 100, 3), 300);
        assert_eq!(page_value(Some(10), 100, 0), 0);
        assert_eq!(
            page_value(Some(u32::MAX), 1, u8::MAX),
            u64::from(u32::MAX) * 255
        );
    }

    #[test]
    fn class_score_puts_under_share_pages_above_over_share_pages() {
        for score in 0..=MAX_SCORE {
            assert_eq!(class_score(score, false, false), score);
            assert_eq!(class_score(score, false, true), score);
            assert_eq!(class_score(score, true, true), score);
            assert!(class_score(0, true, false) > class_score(MAX_SCORE, true, true));
            assert!(class_score(score, true, false) <= MAX_PAGE_VALUE);
        }
        assert_eq!(unseen_value(10), 40);
        assert_eq!(unseen_value_for(10, false), 40);
        assert_eq!(unseen_value_for(10, true), 90);
    }
}
