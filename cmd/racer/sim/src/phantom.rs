//! Memory-lean stand-ins for the per-node placement and route caches.
//!
//! [`Phantom`] and [`SharedRoutes`] plug into [`topology::Placement`] and
//! [`topology::Paths`] so every simulated node runs the real cache, CLOCK, admission,
//! and maintenance code while the scoring and search results are computed
//! once and shared across nodes.

use std::{cell::RefCell, rc::Rc};

use topology::{
    Alternatives, Error, Exhaustive, Member, Membership, PathKey, Ranking, RankingEntry, Rankings,
    Searches,
};

#[cfg(test)]
use topology::{Paths, Placement};

/// Membership identity, then slot, as in [`topology::Placement`]'s cache.
type CacheKey = ([u8; 32], u32);

/// Simulation [`Rankings`]: entries hold only a cursor and the CLOCK bit.
#[derive(Clone, Default)]
pub struct Phantom {
    memo: Rc<RefCell<std::collections::HashMap<CacheKey, PhantomResult>>>,
}

/// Memoized result for one (membership identity, slot).
struct PhantomResult {
    /// The real complete ranking.
    ranking: Ranking,

    /// Whether the real incremental update from this ranking's predecessor succeeded, keyed by
    /// predecessor identity. Usually one element.
    updates: Vec<([u8; 32], bool)>,
}

/// Phantom cache entry: progress and the CLOCK bit, no scores.
#[derive(Debug, Eq, PartialEq)]
pub struct PhantomEntry {
    /// Members scored so far.
    cursor: u32,

    /// CLOCK "recently used" bit.
    referenced: bool,
}

impl Default for PhantomEntry {
    fn default() -> Self {
        Self {
            cursor: 0,
            referenced: true,
        }
    }
}

impl RankingEntry for PhantomEntry {
    fn referenced(&mut self) -> &mut bool {
        &mut self.referenced
    }

    fn cursor(&self) -> usize {
        self.cursor as usize
    }
}

impl Phantom {
    /// Create a phantom with an empty shared memo.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Drop memoized results for every membership except `keep`.
    pub fn forget(&self, keep: &[[u8; 32]]) {
        self.memo
            .borrow_mut()
            .retain(|(identity, _), _| keep.contains(identity));
    }

    /// Number of memoized (membership, slot) results.
    #[must_use]
    pub fn memo_len(&self) -> usize {
        self.memo.borrow().len()
    }

    /// Run `body` on the memoized real ranking, computing it on first use.
    fn with<M: Member, T>(
        &self,
        membership: &Membership<M>,
        slot: u32,
        body: impl FnOnce(&mut PhantomResult) -> T,
    ) -> T {
        let mut memo = self.memo.borrow_mut();
        let result = memo
            .entry((membership.identity(), slot))
            .or_insert_with(|| {
                let mut ranking = Ranking::default();
                ranking.advance(membership, slot, usize::MAX);
                PhantomResult {
                    ranking,
                    updates: Vec::new(),
                }
            });
        body(result)
    }
}

impl Rankings for Phantom {
    type Entry = PhantomEntry;

    /// Map node share, `Rc` header, and `RefCell<PhantomEntry>`.
    const ENTRY_BYTES: usize = 128;

    fn advance<M: Member>(
        &self,
        entry: &mut PhantomEntry,
        membership: &Membership<M>,
        _: u32,
        work: usize,
    ) {
        let end = (entry.cursor as usize)
            .saturating_add(work)
            .min(membership.members().len());
        entry.cursor = u32::try_from(end).expect("membership fits u32");
    }

    fn read<M: Member, T>(
        &self,
        _: &PhantomEntry,
        membership: &Membership<M>,
        slot: u32,
        read: impl FnOnce(&Ranking) -> T,
    ) -> T {
        self.with(membership, slot, |result| read(&result.ranking))
    }

    fn updated<M: Member>(
        &self,
        _: &PhantomEntry,
        membership: &Membership<M>,
        slot: u32,
        base: [u8; 32],
    ) -> Option<PhantomEntry> {
        let known = self.with(membership, slot, |result| {
            result
                .updates
                .iter()
                .find(|(identity, _)| *identity == base)
                .map(|(_, ok)| *ok)
        });
        let ok = known.unwrap_or_else(|| {
            // The predecessor is complete, so its real ranking was memoized unless it was
            // forgotten. Treat a forgotten one as a rescore.
            let previous = self
                .memo
                .borrow()
                .get(&(base, slot))
                .and_then(|old| old.ranking.updated(membership, slot));
            let ok = previous.is_some();
            debug_assert!(
                previous.is_none_or(|next| {
                    self.with(membership, slot, |result| {
                        next.candidates() == result.ranking.candidates()
                    })
                }),
                "incremental update disagrees with full scoring"
            );
            self.with(membership, slot, |result| result.updates.push((base, ok)));
            ok
        });
        ok.then(|| PhantomEntry {
            cursor: u32::try_from(membership.members().len()).expect("membership fits u32"),
            referenced: true,
        })
    }
}

/// Simulation [`Searches`]: replay results from a shared memo.
#[derive(Clone, Default)]
pub struct SharedRoutes {
    memo: Rc<RefCell<RouteMemo>>,
}

/// Steps and result of each memoized search.
type RouteMemo = std::collections::HashMap<PathKey, (usize, Result<Alternatives, Error>)>;

/// One replayed search: steps left and the shared result.
pub struct ReplayedSearch {
    remaining: usize,

    result: Result<Alternatives, Error>,
}

impl SharedRoutes {
    /// Create an empty shared memo.
    #[must_use]
    pub fn new() -> Self {
        Self::default()
    }

    /// Number of memoized searches.
    #[must_use]
    pub fn memo_len(&self) -> usize {
        self.memo.borrow().len()
    }

    /// Drop memoized searches for every graph except `keep` (topology identities).
    pub fn forget(&self, keep: &[[u8; 32]]) {
        self.memo
            .borrow_mut()
            .retain(|key, _| keep.contains(&key.topology()));
    }
}

impl Searches for SharedRoutes {
    type Search = ReplayedSearch;

    fn start<M: Member>(&self, membership: &Membership<M>, key: &PathKey) -> ReplayedSearch {
        let mut memo = self.memo.borrow_mut();
        let (steps, result) = memo.entry(key.clone()).or_insert_with(|| {
            let mut search = Exhaustive.start(membership, key);
            let mut steps = 1;
            while !Exhaustive.step(&mut search) {
                steps += 1;
            }
            (steps, Exhaustive.finish(&search))
        });
        ReplayedSearch {
            remaining: *steps,
            result: result.clone(),
        }
    }

    fn step(&self, search: &mut ReplayedSearch) -> bool {
        search.remaining -= 1;
        search.remaining == 0
    }

    fn finish(&self, search: &ReplayedSearch) -> Result<Alternatives, Error> {
        search.result.clone()
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        num::NonZeroU32,
        task::{Context, Poll, Waker},
    };
    use topology::{PathQuery, REPLICAS, RETAINED_DEPTH};

    /// Test member with an ID and a weight.
    #[derive(Clone, Debug)]
    struct TestMember(String, NonZeroU32);

    impl Member for TestMember {
        const DOMAIN: &'static str = "phantom-tests";

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

    fn key(value: u64) -> [u8; 8] {
        value.to_be_bytes()
    }

    fn query(from: usize, to: usize, links: u8) -> PathQuery<'static> {
        PathQuery {
            from,
            to,
            links,
            visited: &[],
            blocked: &[],
            seed: &[],
        }
    }

    /// Poll a future to completion with a no-op waker.
    fn block_on<F: Future>(future: F) -> F::Output {
        let mut future = std::pin::pin!(future);
        let mut cx = Context::from_waker(Waker::noop());
        loop {
            if let Poll::Ready(value) = future.as_mut().poll(&mut cx) {
                return value;
            }
        }
    }

    /// Demoted members agree between `Scored` and `Phantom`.
    #[test]
    fn phantom_demoted_matches_scored() {
        let members = membership(1000);
        let key = b"demoted";
        let scored = Placement::new(4);
        scored.rank(&members, key);
        let demoted = scored.demoted(&members, key).unwrap();
        assert_eq!(demoted.len(), RETAINED_DEPTH - REPLICAS);
        let phantom = Placement::with_rankings(4, Phantom::new());
        phantom.rank(&members, key);
        assert_eq!(phantom.demoted(&members, key), Some(demoted));
    }

    /// Phantom and scored placements make identical decisions under a seeded workload with held
    /// requests, refreshes, maintenance, and churn.
    #[test]
    fn phantom_matches_scored_on_seeded_workloads_with_churn() {
        type Pending<'a> = std::pin::Pin<Box<dyn Future<Output = Result<Vec<usize>, Error>> + 'a>>;
        for seed in 1..=6u64 {
            let mut state = seed.wrapping_mul(0x9e37_79b9_7f4a_7c15);
            let mut draw = move || {
                state ^= state << 13;
                state ^= state >> 7;
                state ^= state << 17;
                state
            };
            let mut generations = vec![membership(300 + usize::try_from(seed).unwrap() * 50)];
            for generation in 0..8usize {
                let old = generations.last().unwrap();
                let mut values = old.members().to_vec();
                let pick = usize::try_from(draw()).unwrap() % values.len();
                if generation % 3 == 0 {
                    values.remove(pick);
                } else if generation % 3 == 1 {
                    values[pick].1 =
                        NonZeroU32::new(u32::try_from(draw() % 9).unwrap() + 1).unwrap();
                } else {
                    values.push(member(10_000 + generation, 4));
                }
                let next = Membership::new(values).unwrap().with_predecessor(old);
                generations.push(next);
            }
            let capacity = usize::try_from(seed % 3).unwrap() * 8;
            let scored = Placement::new(capacity);
            let phantom = Placement::with_rankings(capacity, Phantom::new());
            let mut held: Vec<(Pending<'_>, Pending<'_>)> = Vec::new();
            let mut cx = Context::from_waker(Waker::noop());
            let mut polls = [0usize; 2];
            let mut current = 0;
            for step in 0..3000 {
                let value = draw();
                if step % 300 == 299 && current + 1 < generations.len() {
                    current += 1;
                }
                let members = &generations[current];
                // Requests sometimes use the previous generation, like in-flight old requests do.
                let view = if value & 64 != 0 && current > 0 {
                    &generations[current - 1]
                } else {
                    members
                };
                let page = key((value >> 8) % 40);
                match value % 8 {
                    0 => assert_eq!(scored.rank(view, &page), phantom.rank(view, &page)),
                    1 => {
                        let mut a: Pending<'_> = Box::pin(scored.rank_async(view, &page));
                        let mut b: Pending<'_> = Box::pin(phantom.rank_async(view, &page));
                        let (x, y) = (a.as_mut().poll(&mut cx), b.as_mut().poll(&mut cx));
                        assert_eq!(x, y, "seed {seed} step {step}");
                        if x.is_pending() {
                            held.push((a, b));
                        }
                    }
                    2 | 3 => {
                        if !held.is_empty() {
                            let index = usize::try_from(value >> 20).unwrap() % held.len();
                            let (a, b) = &mut held[index];
                            polls[0] += 1;
                            polls[1] += 1;
                            let (x, y) = (a.as_mut().poll(&mut cx), b.as_mut().poll(&mut cx));
                            assert_eq!(x, y, "seed {seed} step {step}");
                            if x.is_ready() || value & 1024 != 0 {
                                drop(held.swap_remove(index));
                            }
                        }
                    }
                    4 => assert_eq!(scored.refresh(view, &page), phantom.refresh(view, &page)),
                    5 => assert_eq!(scored.maintain(members), phantom.maintain(members)),
                    6 => assert_eq!(scored.cached(view, &page), phantom.cached(view, &page)),
                    _ => {
                        let node = usize::try_from(value >> 32).unwrap() % 8;
                        assert_eq!(
                            scored.is_owner(view, &page, |i| i == node),
                            phantom.is_owner(view, &page, |i| i == node)
                        );
                    }
                }
                assert_eq!(
                    scored.snapshot(members),
                    phantom.snapshot(members),
                    "seed {seed} step {step}"
                );
                assert_eq!(scored.retained_bytes() > 0, phantom.retained_bytes() > 0);
            }
            assert_eq!(polls[0], polls[1]);
        }
    }

    /// Phantom entries are tiny and the memo is shared across placements.
    #[test]
    fn phantom_entries_are_small_and_share_one_memo() {
        use std::mem::size_of;
        assert!(size_of::<PhantomEntry>() <= 8);
        let map_node = 11 * size_of::<(CacheKey, Rc<RefCell<PhantomEntry>>)>()
            + 12 * size_of::<usize>()
            + 4 * size_of::<usize>();
        // B-tree nodes hold up to 11 entries; count a per-entry share.
        let per_entry = map_node / 11 + 2 * size_of::<usize>() + size_of::<RefCell<PhantomEntry>>();
        assert!(per_entry <= Phantom::ENTRY_BYTES, "{per_entry}");
        let shared = Phantom::new();
        let members = membership(500);
        let nodes: Vec<_> = (0..50)
            .map(|_| Placement::with_rankings(16, shared.clone()))
            .collect();
        for node in &nodes {
            for page in 0..10 {
                assert_eq!(
                    node.rank(&members, &key(page)),
                    Placement::new(0).rank(&members, &key(page))
                );
            }
        }
        assert_eq!(shared.memo_len(), 10);
        shared.forget(&[]);
        assert_eq!(shared.memo_len(), 0);
        // A forgotten predecessor forces a full rescore instead of a wrong answer.
        let next = membership(501).with_predecessor(&members);
        assert_eq!(
            nodes[0].rank(&next, &key(0)),
            Placement::new(0).rank(&next, &key(0))
        );
    }

    /// Replayed searches make the same decisions as real ones: results, cache contents and bytes,
    /// admission, and polls per search.
    #[test]
    fn shared_routes_match_exhaustive_on_seeded_workloads() {
        type Pending<'a> = std::pin::Pin<Box<dyn Future<Output = Result<Vec<usize>, Error>> + 'a>>;
        let generations: Vec<_> = [3000, 3001, 2900].into_iter().map(membership).collect();
        let shared = SharedRoutes::new();
        for seed in 1..=5u64 {
            let mut state = seed.wrapping_mul(0x9e37_79b9_7f4a_7c15);
            let mut draw = move || {
                state ^= state << 13;
                state ^= state >> 7;
                state ^= state << 17;
                state
            };
            let entries = usize::try_from(seed).unwrap() * 3;
            let bytes = 4096 * usize::try_from(seed).unwrap();
            let real = Paths::with_limits(entries, bytes, 3);
            let replay = Paths::with_searches(entries, bytes, 3, shared.clone());
            let seeds: Vec<[u8; 1]> = (0..4).map(|i| [i]).collect();
            let visited = [7usize, 8];
            let mut held: Vec<(Pending<'_>, Pending<'_>)> = Vec::new();
            let mut cx = Context::from_waker(Waker::noop());
            let mut pended = 0;
            for step in 0..2000 {
                let value = draw();
                let members = &generations[usize::try_from(value % 3).unwrap()];
                let count = members.members().len() as u64;
                let from = usize::try_from((value >> 8) % 4).unwrap();
                let to = usize::try_from((value >> 16) % count).unwrap();
                let query = PathQuery {
                    from,
                    to,
                    links: 1 + u8::try_from((value >> 24) % 8).unwrap(),
                    visited: if value & (1 << 40) != 0 && to > 8 {
                        &visited
                    } else {
                        &[]
                    },
                    blocked: &[],
                    seed: &seeds[usize::try_from((value >> 32) % 4).unwrap()],
                };
                if value % 4 == 0 && !held.is_empty() {
                    let index = usize::try_from(value >> 44).unwrap() % held.len();
                    let (a, b) = &mut held[index];
                    let (x, y) = (a.as_mut().poll(&mut cx), b.as_mut().poll(&mut cx));
                    assert_eq!(x, y, "seed {seed} step {step}");
                    if x.is_ready() || value & (1 << 50) != 0 {
                        drop(held.swap_remove(index));
                    }
                } else {
                    let mut a: Pending<'_> = Box::pin(real.route(members, query));
                    let mut b: Pending<'_> = Box::pin(replay.route(members, query));
                    let (x, y) = (a.as_mut().poll(&mut cx), b.as_mut().poll(&mut cx));
                    assert_eq!(x, y, "seed {seed} step {step}");
                    if x.is_pending() {
                        pended += 1;
                        held.push((a, b));
                    }
                }
                assert_eq!(real.cached_entries(), replay.cached_entries());
                assert_eq!(real.cached_bytes(), replay.cached_bytes());
                assert_eq!(real.active_searches(), replay.active_searches());
            }
            drop(held);
            assert!(pended > 0);
            assert_eq!(real.active_searches(), 0);
            assert_eq!(replay.active_searches(), 0);
        }
        assert!(shared.memo_len() > 0);
        let keep = generations[0].topology_identity();
        shared.forget(&[keep]);
        assert!(shared.memo_len() > 0);
        shared.forget(&[]);
        assert_eq!(shared.memo_len(), 0);
    }

    /// Many replaying `Paths` share one result allocation.
    #[test]
    fn shared_routes_share_results_across_nodes() {
        let members = membership(500);
        let shared = SharedRoutes::new();
        let nodes: Vec<_> = (0..20)
            .map(|_| Paths::with_searches(8, 1 << 20, 2, shared.clone()))
            .collect();
        for node in &nodes {
            assert_eq!(
                block_on(node.route(&members, query(0, 250, 8))),
                block_on(Paths::new(0).route(&members, query(0, 250, 8)))
            );
        }
        assert_eq!(shared.memo_len(), 1);
        // Every node's cache holds the memo's allocation, not a copy: the memo plus one cached
        // entry per node.
        let memo = shared.memo.borrow();
        let (_, result) = memo.values().next().unwrap();
        assert_eq!(Rc::strong_count(result.as_ref().unwrap()), 1 + nodes.len());
        drop(memo);
        assert!(std::mem::size_of::<ReplayedSearch>() <= 32);
        assert_eq!(
            block_on(nodes[0].route(&members, query(0, 250, 0))),
            Err(Error::Unreachable)
        );
    }
}
