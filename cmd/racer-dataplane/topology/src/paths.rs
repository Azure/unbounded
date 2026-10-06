//! Shortest-path routing over the membership graph.
//!
//! Searches run in small steps so one route never blocks a worker for long.

use crate::{Error, MAX_DEGREE, Member, Membership, hash};

use sha2::Digest;

use std::{
    cell::RefCell,
    collections::{BTreeMap, VecDeque},
    mem::size_of,
    rc::Rc,
    sync::Arc,
    task::Poll,
};

/// A route request. All positions refer to the same membership.
///
/// - `visited` must not repeat and must not contain `from` or `to`.
/// - `visited.len() + links` must be at most 255.
/// - `blocked` only stops the first hop out of `from`. Repeats are fine.
#[derive(Clone, Copy, Debug)]
pub struct PathQuery<'a> {
    /// Where the route starts.
    pub from: usize,

    /// Where the route ends.
    pub to: usize,

    /// Most hops the route may take.
    pub links: u8,

    /// Members the route must not pass through.
    pub visited: &'a [usize],

    /// Neighbors of `from` to skip as the first hop. At most 64 entries.
    /// Members that are not neighbors of `from` are ignored.
    pub blocked: &'a [usize],

    /// Picks among equally short routes. The same seed gives the same pick.
    /// At most 65,536 bytes.
    pub seed: &'a [u8],
}

/// Finds routes and caches the results.
///
/// Use one per worker thread. It does no I/O and has no timers.
pub struct Paths {
    cache_entries: usize,

    cache_bytes: usize,

    search_limit: usize,

    cache: RefCell<PathCache>,

    inflight: RefCell<BTreeMap<PathKey, Rc<RefCell<SharedSearch>>>>,
}

/// Most nodes one poll may expand before yielding.
const SEARCH_QUANTUM: usize = 32;

/// Longest seed we accept.
const MAX_SEED_BYTES: usize = 65_536;

/// Result type for routing.
type Result<T> = std::result::Result<T, Error>;

/// Cache key for a search. Weights and seed are left out on purpose, so
/// requests that differ only in those share one search.
#[derive(Clone, Eq, PartialEq, Ord, PartialOrd)]
struct PathKey {
    membership: [u8; 32],

    from: usize,

    to: usize,

    links: u8,

    visited: Vec<usize>,

    blocked: Vec<usize>,
}

impl PathKey {
    /// Check the query and sort its lists so equal queries get equal keys.
    fn new<M: Member>(membership: &Membership<M>, query: &PathQuery<'_>) -> Result<Self> {
        let count = membership.members().len();
        if query.from >= count
            || query.to >= count
            || query.visited.len() > usize::from(u8::MAX - query.links)
            || query.blocked.len() > MAX_DEGREE
            || query.seed.len() > MAX_SEED_BYTES
            || query
                .visited
                .iter()
                .any(|&v| v >= count || v == query.from || v == query.to)
            || query.blocked.iter().any(|&v| v >= count)
        {
            return Err(Error::InvalidQuery);
        }
        let mut visited = query.visited.to_vec();
        visited.sort_unstable();
        if visited.windows(2).any(|pair| pair[0] == pair[1]) {
            return Err(Error::InvalidQuery);
        }
        let mut blocked = query.blocked.to_vec();
        blocked.sort_unstable();
        blocked.dedup();
        blocked.retain(|v| {
            membership
                .neighbor_slice(query.from)
                .binary_search(v)
                .is_ok()
        });
        Ok(Self {
            membership: membership.topology_identity(),
            from: query.from,
            to: query.to,
            links: query.links,
            visited,
            blocked,
        })
    }
}

/// Candidate routes, one per usable first hop.
type Alternatives = Rc<Vec<Vec<usize>>>;

/// LRU cache of search results, limited by entry count and bytes.
#[derive(Default)]
struct PathCache {
    entries: BTreeMap<Rc<PathKey>, CacheEntry>,

    oldest: Option<Rc<PathKey>>,

    newest: Option<Rc<PathKey>>,

    bytes: usize,
}

/// One cached result plus its links to older and newer entries.
struct CacheEntry {
    alternatives: Alternatives,

    older: Option<Rc<PathKey>>,

    newer: Option<Rc<PathKey>>,

    bytes: usize,
}

impl PathCache {
    /// Estimate the bytes one entry uses.
    ///
    /// Charges a full B-tree node per entry, so it errs high. This is an
    /// estimate, since the standard library does not promise its layout.
    fn entry_bytes(key: &PathKey, alternatives: &[Vec<usize>], capacity: usize) -> usize {
        let node =
            11 * (size_of::<Rc<PathKey>>() + size_of::<CacheEntry>()) + 16 * size_of::<usize>();
        let bytes = node
            .saturating_add(size_of::<PathKey>() + 2 * size_of::<usize>())
            .saturating_add(key.visited.capacity().saturating_mul(size_of::<usize>()))
            .saturating_add(key.blocked.capacity().saturating_mul(size_of::<usize>()))
            .saturating_add(size_of::<Vec<Vec<usize>>>() + 2 * size_of::<usize>())
            .saturating_add(capacity.saturating_mul(size_of::<Vec<usize>>()));
        alternatives.iter().fold(bytes, |bytes, route| {
            bytes.saturating_add(route.capacity().saturating_mul(size_of::<usize>()))
        })
    }

    /// Remove an entry from the LRU chain by linking its neighbors together.
    fn unlink(&mut self, older: Option<&Rc<PathKey>>, newer: Option<&Rc<PathKey>>) {
        if let Some(key) = older {
            self.entries.get_mut(key).unwrap().newer = newer.cloned();
        } else {
            self.oldest = newer.cloned();
        }
        if let Some(key) = newer {
            self.entries.get_mut(key).unwrap().older = older.cloned();
        } else {
            self.newest = older.cloned();
        }
    }

    /// Return a cached result and mark it as most recently used.
    fn get(&mut self, key: &PathKey) -> Option<Alternatives> {
        let (owned, entry) = self.entries.get_key_value(key)?;
        let alternatives = Rc::clone(&entry.alternatives);
        if entry.newer.is_some() {
            let owned = Rc::clone(owned);
            let older = entry.older.clone();
            let newer = entry.newer.clone();
            self.unlink(older.as_ref(), newer.as_ref());
            if let Some(newest) = &self.newest {
                self.entries.get_mut(newest).unwrap().newer = Some(Rc::clone(&owned));
            }
            let entry = self.entries.get_mut(key).unwrap();
            entry.older = self.newest.replace(owned);
            entry.newer = None;
        }
        Some(alternatives)
    }

    /// Drop the oldest entry. The cache must not be empty.
    fn evict(&mut self) {
        let key = self.oldest.take().unwrap();
        let entry = self.entries.remove(&key).unwrap();
        self.unlink(None, entry.newer.as_ref());
        self.bytes -= entry.bytes;
        if self.entries.is_empty() {
            // Drop the empty root allocation too, keeping empty accounting zero.
            self.entries = BTreeMap::new();
        }
    }

    /// Add a result, evicting old entries to stay within limits. Skips
    /// duplicates and results too big to fit at all.
    fn store(&mut self, key: PathKey, alternatives: Alternatives, entries: usize, bytes: usize) {
        if entries == 0 || bytes == 0 || self.entries.contains_key(&key) {
            return;
        }
        let charge = Self::entry_bytes(&key, &alternatives, alternatives.capacity());
        // An oversized route must not evict useful smaller entries.
        if charge > bytes {
            return;
        }
        while self.entries.len() >= entries || self.bytes > bytes - charge {
            self.evict();
        }
        let key = Rc::new(key);
        if let Some(newest) = &self.newest {
            self.entries.get_mut(newest).unwrap().newer = Some(Rc::clone(&key));
        } else {
            self.oldest = Some(Rc::clone(&key));
        }
        let older = self.newest.replace(Rc::clone(&key));
        self.entries.insert(
            key,
            CacheEntry {
                alternatives,
                older,
                newer: None,
                bytes: charge,
            },
        );
        self.bytes += charge;
    }
}

/// A search that several callers may be waiting on.
enum SharedSearch {
    Working(Box<EqualCostSearch>),

    Done(Result<Alternatives>),
}

impl SharedSearch {
    /// Run one small step. When done, cache the result and share it with
    /// every waiter.
    fn poll(
        &mut self,
        paths: &Paths,
        key: &PathKey,
        cx: &std::task::Context<'_>,
    ) -> Poll<Result<Alternatives>> {
        let search = match self {
            Self::Working(search) => search,
            Self::Done(result) => return Poll::Ready(result.clone()),
        };
        let step = search.step(SEARCH_QUANTUM);
        debug_assert!(
            step.expansions <= SEARCH_QUANTUM && step.edges <= step.expansions * MAX_DEGREE
        );
        if !step.done {
            // Every waiter drives progress; no leader or unbounded waker list.
            cx.waker().wake_by_ref();
            return Poll::Pending;
        }
        let result = search.finish().map(Rc::new);
        if let Ok(alternatives) = &result {
            paths.store(key.clone(), Rc::clone(alternatives));
        }
        paths.inflight.borrow_mut().remove(key);
        *self = Self::Done(result.clone());
        Poll::Ready(result)
    }
}

/// One caller's hold on a running search.
struct SearchAdmission<'a> {
    paths: &'a Paths,

    key: PathKey,

    shared: Rc<RefCell<SharedSearch>>,
}

impl Drop for SearchAdmission<'_> {
    /// If this is the last caller waiting, cancel the search.
    fn drop(&mut self) {
        let mut inflight = self.paths.inflight.borrow_mut();
        if inflight
            .get(&self.key)
            .is_some_and(|search| Rc::ptr_eq(search, &self.shared) && Rc::strong_count(search) == 2)
        {
            inflight.remove(&self.key);
        }
    }
}

impl Paths {
    /// Cache up to `capacity` results and about 8 MiB. Allow 8 searches at once.
    #[must_use]
    pub fn new(capacity: usize) -> Self {
        Self::with_limits(capacity, 8 * 1024 * 1024, 8)
    }

    /// Set the cache entry limit, cache byte limit, and how many searches may
    /// run at once.
    ///
    /// - A zero cache limit turns the cache off.
    /// - Zero searches means only cache hits and self routes work.
    /// - Identical queries share one search, even with different seeds.
    ///
    /// The byte count is an estimate of cache storage only. It is not a hard
    /// memory limit. A result too big for the cache is not cached.
    #[must_use]
    pub fn with_limits(cache_entries: usize, cache_bytes: usize, active_searches: usize) -> Self {
        Self {
            cache_entries,
            cache_bytes,
            search_limit: active_searches,
            cache: RefCell::new(PathCache::default()),
            inflight: RefCell::new(BTreeMap::new()),
        }
    }

    /// Number of cached results.
    #[must_use]
    pub fn cached_entries(&self) -> usize {
        self.cache.borrow().entries.len()
    }

    /// Estimated bytes in the cache. Never above the configured limit.
    #[must_use]
    pub fn cached_bytes(&self) -> usize {
        self.cache.borrow().bytes
    }

    /// Number of searches running now. Callers sharing a search count once.
    #[must_use]
    pub fn active_searches(&self) -> usize {
        self.inflight.borrow().len()
    }

    /// Find a shortest route from `query.from` to `query.to`.
    ///
    /// Returns the positions along the route, including both ends. When
    /// several routes are equally short, the seed and the first hop's weight
    /// decide which one is returned.
    ///
    /// The search yields often, so it does not block the worker. Dropping
    /// the future cancels the search unless another caller is still waiting
    /// on it.
    ///
    /// # Errors
    /// - [`Error::InvalidQuery`] if the query breaks a [`PathQuery`] rule.
    /// - [`Error::Unreachable`] if no route fits in `links` hops.
    /// - [`Error::Overloaded`] if too many searches are already running.
    /// - [`Error::SamplingExhausted`] if weighted picking gives up (very rare).
    pub async fn route<M: Member>(
        &self,
        membership: &Membership<M>,
        query: PathQuery<'_>,
    ) -> Result<Vec<usize>> {
        let key = PathKey::new(membership, &query)?;
        if key.from == key.to {
            return Ok(vec![key.from]);
        }
        if key.links == 0 {
            return Err(Error::Unreachable);
        }
        if let Some(alternatives) = self.cache.borrow_mut().get(&key) {
            return select_route(membership, &alternatives, query.seed);
        }
        let shared = {
            let mut inflight = self.inflight.borrow_mut();
            if let Some(shared) = inflight.get(&key) {
                Rc::clone(shared)
            } else {
                if inflight.len() >= self.search_limit {
                    return Err(Error::Overloaded);
                }
                let shared = Rc::new(RefCell::new(SharedSearch::Working(Box::new(
                    EqualCostSearch::new(membership.graph(), &key),
                ))));
                inflight.insert(key.clone(), Rc::clone(&shared));
                shared
            }
        };
        let admission = SearchAdmission {
            paths: self,
            key,
            shared,
        };
        let alternatives =
            std::future::poll_fn(|cx| admission.shared.borrow_mut().poll(self, &admission.key, cx))
                .await?;
        select_route(membership, &alternatives, query.seed)
    }

    /// Cache a finished search result.
    fn store(&self, key: PathKey, alternatives: Alternatives) {
        self.cache
            .borrow_mut()
            .store(key, alternatives, self.cache_entries, self.cache_bytes);
    }
}

/// Pick one route using the seed and first-hop weights.
fn select_route<M: Member>(
    membership: &Membership<M>,
    alternatives: &[Vec<usize>],
    seed: &[u8],
) -> Result<Vec<usize>> {
    let index = if alternatives.len() == 1 {
        0
    } else {
        weighted_index(membership, alternatives, seed)?
    };
    Ok(alternatives[index].clone())
}

/// Pick a route index, weighted by first-hop weight, using a hash of the seed
/// and endpoints.
fn weighted_index<M: Member>(
    membership: &Membership<M>,
    alternatives: &[Vec<usize>],
    seed: &[u8],
) -> Result<usize> {
    let weights: Vec<_> = alternatives
        .iter()
        .map(|path| u64::from(membership.weight(path[1]).get()))
        .collect();
    let total = weights.iter().sum();
    let mut digest = hash::domain::<M>(b"/next-hop/v5\0");
    hash::bytes(&mut digest, seed);
    let path = &alternatives[0];
    hash::bytes(&mut digest, membership.id(path[0]));
    hash::bytes(&mut digest, membership.id(*path.last().unwrap()));
    sample_index(&weights, total, |counter| {
        let mut draw = digest.clone();
        draw.update(counter.to_be_bytes());
        u64::from_be_bytes(hash::finish(draw)[..8].try_into().unwrap())
    })
}

/// Try up to 64 draws. Give up rather than pick unfairly.
fn sample_index(weights: &[u64], total: u64, mut draw: impl FnMut(u32) -> u64) -> Result<usize> {
    for counter in 0u32..64 {
        if let Some(index) = weighted_draw(draw(counter), total, weights) {
            return Ok(index);
        }
    }
    Err(Error::SamplingExhausted)
}

/// Turn a random number into an index. Returns `None` for numbers that
/// would bias the result, so the caller draws again.
fn weighted_draw(sample: u64, total: u64, weights: &[u64]) -> Option<usize> {
    if sample < total.wrapping_neg() % total {
        return None;
    }
    let mut ticket = sample % total;
    for (index, weight) in weights.iter().enumerate() {
        if ticket < *weight {
            return Some(index);
        }
        ticket -= weight;
    }
    unreachable!("ticket is below the sum of positive weights")
}

/// What the search knows about one reached node.
struct Visit {
    depth: u8,

    parent: usize,

    first: u64,
}

/// One side of the search: from the source, or from the destination.
struct EqualCostWave {
    visits: BTreeMap<usize, Visit>,

    queue: VecDeque<usize>,
}

impl EqualCostWave {
    /// Start a side at its endpoint.
    fn new(root: usize) -> Self {
        Self {
            visits: BTreeMap::from([(
                root,
                Visit {
                    depth: 0,
                    parent: root,
                    first: 0,
                },
            )]),
            queue: VecDeque::from([root]),
        }
    }
}

/// Search from both ends at once, one layer at a time, until the sides meet.
///
/// It tracks which first hops lead to each node. It finishes the whole layer
/// where the sides meet, so every shortest first hop is found, not just the
/// first one by ID. It keeps one route per first hop, not every route.
struct EqualCostSearch {
    graph: Arc<Vec<Vec<usize>>>,

    key: PathKey,

    waves: [EqualCostWave; 2],

    side: usize,

    remaining: usize,

    layers: u8,

    meetings: Vec<Option<usize>>,

    done: bool,
}

/// What one search step did.
#[derive(Default)]
struct SearchStep {
    done: bool,

    expansions: usize,

    edges: usize,
}

impl EqualCostSearch {
    /// Start a search on the given graph.
    fn new(graph: Arc<Vec<Vec<usize>>>, key: &PathKey) -> Self {
        let neighbors = &graph[key.from];
        assert!(neighbors.len() <= MAX_DEGREE);
        Self {
            key: key.clone(),
            meetings: vec![None; neighbors.len()],
            graph,
            waves: [EqualCostWave::new(key.from), EqualCostWave::new(key.to)],
            side: 0,
            remaining: 1,
            layers: 0,
            done: key.from == key.to || key.links == 0,
        }
    }

    /// Expand up to `quantum` nodes. Sides switch after each full layer.
    fn step(&mut self, quantum: usize) -> SearchStep {
        let mut step = SearchStep::default();
        for _ in 0..quantum {
            if self.done {
                break;
            }
            let Some(node) = self.waves[self.side].queue.pop_front() else {
                self.done = true;
                break;
            };
            step.expansions += 1;
            step.edges += self.graph[node].len();
            let depth = self.waves[self.side].visits[&node].depth + 1;
            let first = self.waves[self.side].visits[&node].first;
            for &next in &self.graph[node] {
                if self.key.visited.binary_search(&next).is_ok()
                    || (node == self.key.from && self.key.blocked.binary_search(&next).is_ok())
                    || (next == self.key.from && self.key.blocked.binary_search(&node).is_ok())
                {
                    continue;
                }
                let bits = if self.side == 0 && node == self.key.from {
                    1u64 << self.graph[self.key.from].binary_search(&next).unwrap()
                } else {
                    first
                };
                let wave = &mut self.waves[self.side];
                let visit = wave.visits.entry(next).or_insert_with(|| {
                    wave.queue.push_back(next);
                    Visit {
                        depth,
                        parent: node,
                        first: 0,
                    }
                });
                if visit.depth != depth {
                    continue;
                }
                visit.first |= bits;
                if let Some(opposite) = self.waves[1 - self.side].visits.get(&next) {
                    // Widen sums so the full public u8 link range is safe.
                    if u16::from(depth) + u16::from(opposite.depth) != u16::from(self.layers) + 1 {
                        continue;
                    }
                    let mut mask = self.waves[0].visits[&next].first;
                    while mask != 0 {
                        let bit = mask.trailing_zeros() as usize;
                        self.meetings[bit].get_or_insert(next);
                        mask &= mask - 1;
                    }
                }
            }
            self.remaining -= 1;
            if self.remaining == 0 {
                self.layers += 1;
                self.done = self.meetings.iter().any(Option::is_some)
                    || self.layers == self.key.links
                    || self.waves[self.side].queue.is_empty();
                self.side = 1 - self.side;
                self.remaining = self.waves[self.side].queue.len();
            }
        }
        step.done = self.done;
        step
    }

    /// Build the routes found by a finished search.
    fn finish(&self) -> Result<Vec<Vec<usize>>> {
        if self.key.from == self.key.to {
            return Ok(vec![vec![self.key.from]]);
        }
        let mut routes = Vec::new();
        for (bit, meeting) in self.meetings.iter().enumerate() {
            let Some(meeting) = *meeting else {
                continue;
            };
            let mut nodes = vec![meeting];
            let mut current = meeting;
            while current != self.key.from {
                let depth = self.waves[0].visits[&current].depth;
                current = if depth == 1 {
                    self.key.from
                } else {
                    self.graph[current]
                        .iter()
                        .copied()
                        .find(|candidate| {
                            self.waves[0].visits.get(candidate).is_some_and(|v| {
                                v.depth + 1 == depth && v.first & (1u64 << bit) != 0
                            })
                        })
                        .ok_or(Error::Unreachable)?
                };
                nodes.push(current);
            }
            nodes.reverse();
            current = meeting;
            while current != self.key.to {
                current = self.waves[1].visits[&current].parent;
                nodes.push(current);
            }
            routes.push(nodes);
        }
        if routes.is_empty() {
            return Err(Error::Unreachable);
        }
        Ok(routes)
    }
}

#[cfg(test)]
/// Routing tests, checked against simple reference code.
mod tests {
    use super::*;

    use futures::{executor::block_on, task::noop_waker_ref};

    use std::{future::Future, num::NonZeroU32, task::Context};

    /// The LRU cache matches a simple reference model.
    #[test]
    fn cache_duplicate_store_and_lru_match_deterministic_model() {
        let members = membership(32);
        let keys: Vec<_> = (1..32)
            .map(|to| PathKey::new(&members, &query(0, to, 4)).unwrap())
            .collect();
        let mut cache = PathCache::default();
        let mut model = VecDeque::new();
        let alternatives = Rc::new(vec![vec![0, 1]]);
        let charge = PathCache::entry_bytes(&keys[0], &alternatives, alternatives.capacity());
        let mut state = 0xa712_81d3u64;
        for turn in 0..2048 {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            let index = usize::try_from(state % 31).unwrap();
            let key = &keys[index];
            if turn % 3 == 0 {
                let found = model.iter().position(|v| *v == index);
                assert_eq!(cache.get(key).is_some(), found.is_some());
                if let Some(position) = found {
                    model.remove(position);
                    model.push_back(index);
                }
            } else {
                let before = cache.bytes;
                cache.store(key.clone(), Rc::clone(&alternatives), 7, charge * 5);
                if model.contains(&index) {
                    // Duplicate store is idempotent, including recency and accounting.
                    assert_eq!(cache.bytes, before);
                } else {
                    if model.len() == 5 {
                        model.pop_front();
                    }
                    model.push_back(index);
                }
            }
            assert_eq!(cache.entries.len(), model.len());
            assert_eq!(cache.bytes, charge * model.len());
            assert_eq!(
                cache.bytes,
                cache
                    .entries
                    .values()
                    .map(|entry| entry.bytes)
                    .sum::<usize>()
            );
            let mut current = cache.oldest.clone();
            let mut previous: Option<Rc<PathKey>> = None;
            for &expected in &model {
                let owned = current.take().unwrap();
                assert!(owned.as_ref() == &keys[expected]);
                let entry = cache.entries.get(&owned).unwrap();
                assert!(entry.older == previous);
                current.clone_from(&entry.newer);
                previous = Some(owned);
            }
            assert!(current.is_none());
            assert!(previous == cache.newest);
        }
        let saved = cache.bytes;
        // Oversized unique result bypasses without changing existing entries.
        let mut key = keys[0].clone();
        key.links = 5;
        cache.store(key, Rc::new(vec![vec![0; 10_000]]), 7, charge * 5);
        assert_eq!(cache.bytes, saved);
        while !cache.entries.is_empty() {
            cache.evict();
        }
        assert_eq!(cache.bytes, 0);
        assert!(cache.oldest.is_none() && cache.newest.is_none());
        cache.store(keys[0].clone(), Rc::clone(&alternatives), 1, charge);
        let owned = Rc::clone(cache.entries.first_key_value().unwrap().0);
        for _ in 0..100 {
            assert!(Rc::ptr_eq(&cache.get(&keys[0]).unwrap(), &alternatives));
            assert!(Rc::ptr_eq(
                cache.entries.first_key_value().unwrap().0,
                &owned
            ));
            assert_eq!(cache.bytes, charge);
        }
        cache.store(keys[0].clone(), Rc::new(vec![vec![9; 100]]), 1, charge);
        assert!(Rc::ptr_eq(&cache.get(&keys[0]).unwrap(), &alternatives));
        assert_eq!(cache.bytes, charge);
    }

    /// Test member with a binary ID and a weight.
    #[derive(Clone)]
    struct TestMember(Vec<u8>, NonZeroU32);

    impl Member for TestMember {
        const DOMAIN: &'static str = "racer";

        /// Return the ID.
        fn id(&self) -> &[u8] {
            &self.0
        }

        /// Return the weight.
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }

    /// Build a membership of `count` members, all with weight 1.
    fn membership(n: usize) -> Membership<TestMember> {
        Membership::new(
            (0..n)
                .map(|i| {
                    TestMember(
                        format!("node-{i:06}").into_bytes(),
                        NonZeroU32::new(4).unwrap(),
                    )
                })
                .collect(),
        )
        .unwrap()
    }

    /// Build a query with no filters and an empty seed.
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

    /// Rebuild the ring graph from scratch, as a check on the real code.
    fn graph(n: usize) -> Vec<Vec<usize>> {
        let mut edges = vec![vec![]; n];
        if n < 2 {
            return edges;
        }
        for ring in 0u32..32 {
            let mut order: Vec<_> = (0..n)
                .map(|position| {
                    let id = format!("node-{position:06}");
                    let mut bytes = b"racer/overlay/sha256-rings-32/v1\0".to_vec();
                    bytes.extend_from_slice(&ring.to_be_bytes());
                    bytes.extend_from_slice(&(id.len() as u64).to_be_bytes());
                    bytes.extend_from_slice(id.as_bytes());
                    (<[u8; 32]>::from(sha2::Sha256::digest(bytes)), position)
                })
                .collect();
            order.sort_unstable();
            for index in 0..n {
                let a = order[index].1;
                let b = order[(index + 1) % n].1;
                edges[a].push(b);
                edges[b].push(a);
            }
        }
        for neighbors in &mut edges {
            neighbors.sort_unstable();
            neighbors.dedup();
        }
        edges
    }

    /// Hop count from every node to `to`, by plain BFS.
    fn distances(edges: &[Vec<usize>], key: &PathKey) -> Vec<usize> {
        let mut distances = vec![usize::MAX; edges.len()];
        distances[key.to] = 0;
        let mut queue = VecDeque::from([key.to]);
        while let Some(node) = queue.pop_front() {
            for &next in &edges[node] {
                if key.visited.contains(&next)
                    || (node == key.from && key.blocked.contains(&next))
                    || (next == key.from && key.blocked.contains(&node))
                    || distances[next] != usize::MAX
                {
                    continue;
                }
                distances[next] = distances[node] + 1;
                queue.push_back(next);
            }
        }
        distances
    }

    /// Run a search one node at a time and return all routes it finds.
    fn search(members: &Membership<TestMember>, key: &PathKey) -> Result<Vec<Vec<usize>>> {
        let mut search = EqualCostSearch::new(members.graph(), key);
        while !search.step(1).done {}
        search.finish()
    }

    /// Search finds every shortest first hop, matching plain BFS.
    #[test]
    fn all_equal_next_hops_match_independent_oracle() {
        for n in [37, 401, 1500] {
            let edges = graph(n);
            let members = membership(n);
            for source in [0, 19, n - 1] {
                for to in (0..n).step_by(17).filter(|&to| to != source) {
                    for filtered in [false, true] {
                        let mut key = PathKey::new(&members, &query(source, to, 4)).unwrap();
                        if filtered {
                            key.visited = [7, 33]
                                .into_iter()
                                .filter(|v| *v != source && *v != to)
                                .collect();
                            key.blocked = edges[source].iter().copied().step_by(2).collect();
                        }
                        let distance = distances(&edges, &key);
                        for links in [1, 2, 4, 255] {
                            key.links = links;
                            let result = search(&members, &key);
                            if distance[source] > links as usize {
                                assert_eq!(result, Err(Error::Unreachable));
                                continue;
                            }
                            let alternatives = result.unwrap();
                            let expected: Vec<_> = edges[source]
                                .iter()
                                .copied()
                                .filter(|v| {
                                    !key.blocked.contains(v)
                                        && !key.visited.contains(v)
                                        && distance[*v].checked_add(1) == Some(distance[source])
                                })
                                .collect();
                            assert_eq!(
                                alternatives.iter().map(|p| p[1]).collect::<Vec<_>>(),
                                expected
                            );
                            for path in alternatives {
                                assert_eq!(path.first(), Some(&source));
                                assert_eq!(path.last(), Some(&to));
                                assert_eq!(path.len(), distance[source] + 1);
                                for pair in path.windows(2) {
                                    assert!(edges[pair[0]].contains(&pair[1]));
                                    assert!(!key.visited.contains(&pair[1]));
                                }
                            }
                        }
                    }
                }
            }
        }
    }

    /// Route picks use the exact hash input and are redone on cache hits.
    #[test]
    fn independent_hash_vectors_cache_reselection_and_snapshot_weights() {
        let mut input = membership(1500).members().to_vec();
        for (i, member) in input.iter_mut().enumerate() {
            member.1 = NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap();
        }
        let members = Membership::new(input).unwrap();
        let cached = Paths::new(1);
        let cold = Paths::new(0);
        let alternatives = search(
            &members,
            &PathKey::new(&members, &query(0, 1499, 4)).unwrap(),
        )
        .unwrap();
        assert!(alternatives.len() > 1);
        // Independent hash byte construction, separate from production helpers.
        for attempt in [0u128, 1, 2, 127] {
            let mut seed = vec![1; 16];
            seed.extend_from_slice(&attempt.to_be_bytes());
            let query = PathQuery {
                seed: &seed,
                ..query(0, 1499, 4)
            };
            let mut bytes = b"racer/next-hop/v5\0".to_vec();
            for field in [seed.as_slice(), members.id(0), members.id(1499)] {
                bytes.extend_from_slice(&u32::try_from(field.len()).unwrap().to_be_bytes());
                bytes.extend_from_slice(field);
            }
            bytes.extend_from_slice(&0u32.to_be_bytes());
            let digest = sha2::Sha256::digest(bytes);
            let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
            let weights: Vec<_> = alternatives
                .iter()
                .map(|p| u64::from(members.weight(p[1]).get()))
                .collect();
            let total: u64 = weights.iter().sum();
            assert!(sample >= total.wrapping_neg() % total);
            let mut ticket = sample % total;
            let expected = alternatives
                .iter()
                .zip(weights)
                .find_map(|(path, weight)| {
                    if ticket < weight {
                        Some(path[1])
                    } else {
                        ticket -= weight;
                        None
                    }
                })
                .unwrap();
            let actual = block_on(cached.route(&members, query)).unwrap();
            assert_eq!(actual[1], expected);
            assert_eq!(actual, block_on(cold.route(&members, query)).unwrap());
        }
        let mut changed = members.members().to_vec();
        let favored = alternatives[0][1];
        changed[favored].1 = NonZeroU32::new(u32::MAX).unwrap();
        let changed = Membership::new(changed).unwrap();
        let mut seed = vec![1; 16];
        seed.extend_from_slice(&[2; 16]);
        let query = PathQuery {
            seed: &seed,
            ..query(0, 1499, 4)
        };
        let previous = Rc::clone(
            &cached
                .cache
                .borrow()
                .entries
                .first_key_value()
                .unwrap()
                .1
                .alternatives,
        );
        assert_eq!(block_on(cached.route(&changed, query)).unwrap()[1], favored);
        assert!(Rc::ptr_eq(
            &previous,
            &cached
                .cache
                .borrow()
                .entries
                .first_key_value()
                .unwrap()
                .1
                .alternatives
        ));
        assert_eq!(cached.cache.borrow().entries.len(), 1);
        assert_eq!(cold.cache.borrow().entries.len(), 0);
    }

    /// The 64th first hop works end to end.
    #[test]
    fn last_first_hop_bit_survives_search_and_reconstruction() {
        let n = 100_000;
        let source = 19;
        let members = membership(n);
        let neighbors = members.neighbors(source);
        assert_eq!(neighbors.len(), 64);
        let last = neighbors[63];
        let mut key = PathKey::new(&members, &query(source, last, 4)).unwrap();
        key.blocked = neighbors[..63].to_vec();
        let next = members
            .neighbors(last)
            .into_iter()
            .find(|v| *v != source && !neighbors.contains(v))
            .unwrap();
        for to in [last, next] {
            key.to = to;
            let alternatives = search(&members, &key).unwrap();
            assert_eq!(alternatives.len(), 1);
            assert_eq!(alternatives[0][1], last);
            assert_eq!(alternatives[0].last(), Some(&to));
        }
        key.blocked = neighbors;
        assert_eq!(search(&members, &key), Err(Error::Unreachable));
    }

    /// Weighted picking is fair, including at its edges.
    #[test]
    fn weighted_integer_mapping_and_probabilities() {
        assert_eq!(weighted_draw(0, 5, &[4, 1]), None);
        let mut counts = [0; 2];
        for sample in 1..=10_000 {
            counts[weighted_draw(sample, 5, &[4, 1]).unwrap()] += 1;
        }
        assert_eq!(counts, [8000, 2000]);
        let max = u64::from(u32::MAX);
        assert_eq!(weighted_draw(u64::MAX, 64 * max, &[max; 64]), Some(1));
    }

    /// Each search step stays within its work limits.
    #[test]
    fn search_work_is_bounded_per_turn() {
        let members = membership(100_000);
        let key = PathKey::new(&members, &query(0, 80_003, 4)).unwrap();
        let mut search = EqualCostSearch::new(members.graph(), &key);
        assert!(Arc::ptr_eq(&search.graph, &members.graph()));
        loop {
            let step = search.step(7);
            assert!(step.expansions <= 7);
            assert!(step.edges <= step.expansions * MAX_DEGREE);
            assert!(
                search
                    .waves
                    .iter()
                    .map(|wave| wave.visits.len())
                    .sum::<usize>()
                    <= 2 * 100_000
            );
            if step.done {
                break;
            }
        }
        let alternatives = search.finish().unwrap();
        assert!(alternatives.len() <= MAX_DEGREE);
        assert!(alternatives.iter().all(|p| p.len() <= 5));
    }

    /// Small graphs match the from-scratch ring graph.
    #[test]
    fn exhaustive_inverse_matches_definition() {
        // Retain the historical test name, now checking the ID-ring definition.
        for n in 1..=160 {
            let members = membership(n);
            let expected = graph(n);
            for (i, row) in expected.iter().enumerate() {
                assert_eq!(members.neighbor_slice(i), row, "N={n}, i={i}");
            }
        }
        assert!(membership(0).neighbors(0).is_empty());
        assert!(membership(1).neighbors(usize::MAX).is_empty());
    }

    /// Large graphs keep degree limits, symmetry, and short routes.
    #[test]
    fn hundred_thousand_nodes_bounded_symmetric_and_four_link_reachable() {
        let n = 100_000;
        let members = membership(n);
        for i in 0..n {
            let neighbors = members.neighbor_slice(i);
            assert!(neighbors.len() <= MAX_DEGREE);
            for &other in neighbors {
                assert!(members.neighbor_slice(other).binary_search(&i).is_ok());
            }
        }
        for source in [0, 1, 17, 49_999, 99_999] {
            let mut distance = vec![u8::MAX; n];
            distance[source] = 0;
            let mut queue = VecDeque::from([source]);
            while let Some(i) = queue.pop_front() {
                if distance[i] == 4 {
                    continue;
                }
                for &j in members.neighbor_slice(i) {
                    if distance[j] == u8::MAX {
                        distance[j] = distance[i] + 1;
                        queue.push_back(j);
                    }
                }
            }
            assert!(distance.iter().all(|d| *d <= 4));
        }
    }

    /// Binary IDs, bad queries, and first-hop-only blocking.
    #[test]
    fn arbitrary_binary_members_invalid_queries_and_first_hop_only_blocking() {
        /// Fixed-size binary ID that may contain zero bytes.
        struct Binary([u8; 2]);

        impl Member for Binary {
            const DOMAIN: &'static str = "binary";

            /// Return the raw ID.
            fn id(&self) -> &[u8] {
                &self.0
            }

            /// Use equal weights.
            fn weight(&self) -> NonZeroU32 {
                NonZeroU32::new(1).unwrap()
            }
        }
        let members =
            Membership::new((0u16..1500).map(|i| Binary(i.to_be_bytes())).collect()).unwrap();
        let paths = Paths::new(2);
        let basic = query(0, 1499, 255);
        let route = block_on(paths.route(&members, basic)).unwrap();
        assert_eq!(route.first(), Some(&0));
        assert_eq!(route.last(), Some(&1499));
        assert!(route.len() > 2);
        // Blocking the destination's first-hop edge does not ban reaching it later.
        assert_eq!(
            block_on(paths.route(
                &members,
                PathQuery {
                    blocked: &[1499],
                    ..basic
                }
            ))
            .unwrap(),
            route
        );
        for invalid in [
            PathQuery {
                from: usize::MAX,
                ..basic
            },
            PathQuery { to: 1500, ..basic },
            PathQuery {
                visited: &[1500],
                links: 4,
                ..basic
            },
            PathQuery {
                blocked: &[usize::MAX],
                ..basic
            },
            PathQuery {
                visited: &[0],
                links: 4,
                ..basic
            },
            PathQuery {
                visited: &[1499],
                links: 4,
                ..basic
            },
            PathQuery {
                visited: &[1, 1],
                links: 4,
                ..basic
            },
            PathQuery {
                visited: &[1],
                ..basic
            },
        ] {
            assert_eq!(
                block_on(paths.route(&members, invalid)),
                Err(Error::InvalidQuery)
            );
        }
        assert_eq!(
            block_on(paths.route(&members, query(0, 1499, 0))),
            Err(Error::Unreachable)
        );
        assert_eq!(block_on(paths.route(&members, query(0, 0, 0))), Ok(vec![0]));
        let empty = Membership::<Binary>::new(vec![]).unwrap();
        assert!(empty.neighbors(0).is_empty());
        assert!(members.neighbors(1500).is_empty());
        assert_eq!(
            block_on(paths.route(&empty, query(0, 0, 0))),
            Err(Error::InvalidQuery)
        );
    }

    /// Equal queries and weight-only snapshots share entries; different domains do not.
    #[test]
    fn canonical_cache_identity_includes_domain_but_not_weights() {
        /// Same records under a different domain.
        struct Other(TestMember);

        impl Member for Other {
            const DOMAIN: &'static str = "other";

            /// Return the wrapped ID.
            fn id(&self) -> &[u8] {
                self.0.id()
            }

            /// Return the wrapped weight.
            fn weight(&self) -> NonZeroU32 {
                self.0.weight()
            }
        }
        let members = membership(1500);
        let other =
            Membership::new(members.members().iter().cloned().map(Other).collect()).unwrap();
        let paths = Paths::new(4);
        let a = PathQuery {
            visited: &[7, 33],
            blocked: &[1, 2],
            ..query(0, 1499, 4)
        };
        let b = PathQuery {
            visited: &[33, 7],
            blocked: &[2, 1, 2],
            ..a
        };
        assert_eq!(
            block_on(paths.route(&members, a)),
            block_on(paths.route(&members, b))
        );
        assert_eq!(paths.cache.borrow().entries.len(), 1);
        let identical = membership(1500);
        assert_eq!(
            block_on(paths.route(&members, a)),
            block_on(paths.route(&identical, a))
        );
        assert_eq!(paths.cache.borrow().entries.len(), 1);
        assert_eq!(
            block_on(paths.route(&other, a)).unwrap(),
            block_on(Paths::new(0).route(&other, a)).unwrap()
        );
        assert_eq!(paths.cache.borrow().entries.len(), 2);
        let mut changed = members.members().to_vec();
        changed[0].1 = NonZeroU32::new(1).unwrap();
        block_on(paths.route(&Membership::new(changed).unwrap(), a)).unwrap();
        assert_eq!(paths.cache.borrow().entries.len(), 2);
    }

    /// Dropping a search frees its slot, with or without a cache.
    #[test]
    fn cooperative_admission_cancellation_and_cache_hits() {
        let members = membership(100_000);
        let base = query(0, 80_003, 4);
        let mut cx = Context::from_waker(noop_waker_ref());
        for capacity in [0, 1, 32] {
            let paths = Paths::with_limits(capacity, usize::MAX, 2);
            let mut pending = Vec::new();
            for to in [80_003, 80_004] {
                let mut future = Box::pin(paths.route(&members, PathQuery { to, ..base }));
                assert!(future.as_mut().poll(&mut cx).is_pending());
                pending.push(future);
            }
            assert_eq!(
                block_on(paths.route(&members, PathQuery { to: 80_005, ..base })),
                Err(Error::Overloaded)
            );
            drop(pending);
            assert_eq!(paths.active_searches(), 0);
            let expected = block_on(paths.route(&members, base)).unwrap();
            assert_eq!(paths.active_searches(), 0);
            let mut future = Box::pin(paths.route(&members, base));
            let first = future.as_mut().poll(&mut cx);
            if capacity == 0 {
                assert!(first.is_pending());
            } else {
                assert_eq!(first, Poll::Ready(Ok(expected)));
            }
        }
    }

    /// Self routes are still checked for bad input.
    #[test]
    fn self_routes_validate_before_bypassing_admission_and_cache() {
        let members = membership(1500);
        let paths = Paths::with_limits(0, 0, 0);
        assert_eq!(block_on(paths.route(&members, query(0, 0, 0))), Ok(vec![0]));
        assert_eq!(
            block_on(paths.route(&members, query(0, 1, 4))),
            Err(Error::Overloaded)
        );
        for invalid in [
            PathQuery {
                blocked: &[usize::MAX],
                ..query(0, 0, 0)
            },
            PathQuery {
                visited: &[0],
                ..query(0, 0, 0)
            },
            query(1500, 1500, 0),
        ] {
            assert_eq!(
                block_on(paths.route(&members, invalid)),
                Err(Error::InvalidQuery)
            );
        }
        assert_eq!(paths.cached_entries(), 0);
        assert_eq!(paths.active_searches(), 0);
    }

    /// Cache byte limits, LRU order, and cache misses after IDs change.
    #[test]
    fn memory_budget_lru_and_same_count_id_invalidation() {
        let members = membership(1500);
        let paths = Paths::with_limits(2, usize::MAX, 1);
        let a = query(0, 1499, 4);
        let b = query(1, 1499, 4);
        let c = query(2, 1499, 4);
        for q in [a, b, a, c] {
            block_on(paths.route(&members, q)).unwrap();
        }
        assert_eq!(paths.cached_entries(), 2);
        let cache = paths.cache.borrow();
        assert_eq!(cache.oldest.as_ref().unwrap().from, 0);
        assert_eq!(cache.newest.as_ref().unwrap().from, 2);
        drop(cache);
        let single = Paths::new(1);
        block_on(single.route(&members, a)).unwrap();
        let bytes = single.cached_bytes();
        assert!(bytes > 0);
        for budget in [0, 1, bytes - 1, bytes, bytes * 2] {
            let bounded = Paths::with_limits(1000, budget, 1);
            block_on(bounded.route(&members, a)).unwrap();
            assert_eq!(bounded.cached_entries(), usize::from(budget >= bytes));
            for q in [b, c, a, b] {
                block_on(bounded.route(&members, q)).unwrap();
                assert!(bounded.cached_bytes() <= budget);
            }
        }
        let mut values = members.members().to_vec();
        values[1499].0 = b"zz-replacement".to_vec();
        let replaced = Membership::new(values).unwrap();
        let paths = Paths::new(8);
        block_on(paths.route(&members, a)).unwrap();
        block_on(paths.route(&replaced, a)).unwrap();
        assert_eq!(paths.cached_entries(), 2);
        assert_ne!(
            paths.cache.borrow().oldest.as_ref().unwrap().membership,
            paths.cache.borrow().newest.as_ref().unwrap().membership
        );
    }

    /// Callers share a search but each gets its own pick.
    #[test]
    fn duplicate_searches_share_progress_survive_cancellation_and_reselect() {
        let members = membership(10_000);
        let base = query(0, 9999, 4);
        let paths = Paths::with_limits(0, 0, 1);
        let mut cx = Context::from_waker(noop_waker_ref());
        let mut first = Box::pin(paths.route(&members, base));
        assert!(first.as_mut().poll(&mut cx).is_pending());
        let shared = Rc::clone(paths.inflight.borrow().values().next().unwrap());
        let mut second = Box::pin(paths.route(
            &members,
            PathQuery {
                seed: b"different",
                ..base
            },
        ));
        assert!(second.as_mut().poll(&mut cx).is_pending());
        assert!(Rc::ptr_eq(
            &shared,
            paths.inflight.borrow().values().next().unwrap()
        ));
        drop(shared);
        assert_eq!(paths.active_searches(), 1);
        assert_eq!(block_on(paths.route(&members, query(0, 0, 0))), Ok(vec![0]));
        assert_eq!(
            block_on(paths.route(&members, query(1, 9999, 4))),
            Err(Error::Overloaded)
        );
        drop(first);
        assert_eq!(paths.active_searches(), 1);
        let actual = block_on(second).unwrap();
        assert_eq!(paths.active_searches(), 0);
        assert_eq!(
            actual,
            block_on(Paths::new(0).route(
                &members,
                PathQuery {
                    seed: b"different",
                    ..base
                }
            ))
            .unwrap()
        );
        let mut first = Box::pin(paths.route(&members, base));
        let mut second = Box::pin(paths.route(&members, base));
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(second.as_mut().poll(&mut cx).is_pending());
        drop(second);
        assert_eq!(paths.active_searches(), 1);
        drop(first);
        assert_eq!(paths.active_searches(), 0);
        // Completed but unpolled followers must not remove a new generation.
        let mut first = Box::pin(paths.route(&members, base));
        let mut follower = Box::pin(paths.route(&members, base));
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(follower.as_mut().poll(&mut cx).is_pending());
        block_on(first).unwrap();
        let mut next = Box::pin(paths.route(&members, base));
        assert!(next.as_mut().poll(&mut cx).is_pending());
        drop(follower);
        assert_eq!(paths.active_searches(), 1);
        drop(next);
        assert_eq!(paths.active_searches(), 0);
    }

    /// Cache settings do not change the search limit.
    #[test]
    fn cache_size_and_admission_are_independent() {
        let members = membership(10_000);
        let mut cx = Context::from_waker(noop_waker_ref());
        for entries in [0, 1, 1000] {
            for limit in [0, 1, 3] {
                let paths = Paths::with_limits(entries, 4096, limit);
                let mut waiters = Vec::new();
                for index in 0..limit {
                    let mut future = Box::pin(
                        paths.route(&members, query(0, 9999, 4 + u8::try_from(index).unwrap())),
                    );
                    assert!(future.as_mut().poll(&mut cx).is_pending());
                    waiters.push(future);
                }
                assert_eq!(paths.active_searches(), limit);
                assert_eq!(
                    block_on(paths.route(&members, query(0, 9999, 7))),
                    Err(Error::Overloaded)
                );
                drop(waiters);
                assert_eq!(paths.active_searches(), 0);
            }
        }
        assert_eq!(Paths::new(0).search_limit, Paths::new(1000).search_limit);
    }

    /// Query limits at their edges, then filtered queries.
    #[test]
    fn validation_limits_and_deterministic_fuzz_cases() {
        let members = membership(401);
        let paths = Paths::new(4);
        let visited: Vec<_> = (1..=255).collect();
        assert!(
            PathKey::new(
                &members,
                &PathQuery {
                    visited: &visited,
                    ..query(0, 400, 0)
                }
            )
            .is_ok()
        );
        for links in [1, 4, 255] {
            assert_eq!(
                block_on(paths.route(
                    &members,
                    PathQuery {
                        visited: &visited,
                        ..query(0, 400, links)
                    }
                )),
                Err(Error::InvalidQuery)
            );
        }
        let blocked = vec![1; 65];
        let seed = vec![0; MAX_SEED_BYTES + 1];
        for invalid in [
            PathQuery {
                blocked: &blocked,
                ..query(0, 400, 4)
            },
            PathQuery {
                seed: &seed,
                ..query(0, 400, 4)
            },
        ] {
            assert_eq!(
                block_on(paths.route(&members, invalid)),
                Err(Error::InvalidQuery)
            );
        }
        assert!(
            PathKey::new(
                &members,
                &PathQuery {
                    blocked: &blocked[..64],
                    seed: &seed[..MAX_SEED_BYTES],
                    ..query(0, 400, 4)
                }
            )
            .is_ok()
        );
        let non_neighbor = (1..401)
            .find(|v| !members.neighbor_slice(0).contains(v))
            .unwrap();
        let canonical = PathKey::new(
            &members,
            &PathQuery {
                blocked: &[non_neighbor, non_neighbor, 0],
                ..query(0, 400, 4)
            },
        )
        .unwrap();
        assert!(canonical.blocked.is_empty());
        check_deterministic_fuzz_cases(&members, &paths);
    }

    /// Many queries match plain BFS hop counts.
    fn check_deterministic_fuzz_cases(members: &Membership<TestMember>, paths: &Paths) {
        let edges = members.graph();
        let mut state = 0xa127_3921u64;
        for iteration in 0..128 {
            state ^= state << 13;
            state ^= state >> 7;
            state ^= state << 17;
            let from = usize::try_from(state % 401).unwrap();
            let to = usize::try_from((state >> 32) % 401).unwrap();
            let visited: Vec<_> = (0..iteration % 9)
                .map(|i| (i * 37 + iteration) % 401)
                .filter(|v| *v != from && *v != to)
                .collect();
            let blocked: Vec<_> = members
                .neighbor_slice(from)
                .iter()
                .copied()
                .step_by(3)
                .collect();
            let q = PathQuery {
                visited: &visited,
                blocked: &blocked,
                ..query(from, to, u8::try_from(iteration % 5).unwrap())
            };
            let key = PathKey::new(members, &q).unwrap();
            let distance = distances(&edges, &key)[from];
            let actual = block_on(paths.route(members, q));
            if distance > usize::from(q.links) {
                assert_eq!(actual, Err(Error::Unreachable));
            } else {
                let route = actual.unwrap();
                assert_eq!(route.len(), distance + 1);
                assert_eq!(route.first(), Some(&from));
                assert_eq!(route.last(), Some(&to));
                for pair in route.windows(2) {
                    assert!(edges[pair[0]].contains(&pair[1]));
                    assert!(!visited.contains(&pair[1]));
                }
                if route.len() > 1 {
                    assert!(!blocked.contains(&route[1]));
                }
            }
        }
    }

    /// Running out of draws gives `SamplingExhausted`, not `Unreachable`.
    #[test]
    fn rejection_retry_is_bounded_and_distinct_from_unreachable() {
        let mut calls = 0;
        assert_eq!(
            sample_index(&[4, 1], 5, |counter| {
                assert_eq!(counter, calls);
                calls += 1;
                0
            }),
            Err(Error::SamplingExhausted)
        );
        assert_eq!(calls, 64);
        assert_eq!(
            sample_index(&[4, 1], 5, |counter| if counter == 63 { 4 } else { 0 }),
            Ok(1)
        );
    }

    /// Changing a record after building the membership does not affect routing.
    #[test]
    fn selection_reads_frozen_ids_and_weights_after_interior_mutation() {
        use std::cell::Cell;

        /// Record whose ID and weight can change after use.
        struct Mutable {
            id: Vec<u8>,

            changed: Cell<bool>,

            weight: Cell<NonZeroU32>,
        }

        impl Member for Mutable {
            const DOMAIN: &'static str = "racer";

            /// Return the current ID.
            fn id(&self) -> &[u8] {
                if self.changed.get() {
                    b"changed"
                } else {
                    &self.id
                }
            }

            /// Return the current weight.
            fn weight(&self) -> NonZeroU32 {
                self.weight.get()
            }
        }
        let original = membership(1500);
        let values = original
            .members()
            .iter()
            .enumerate()
            .map(|(i, m)| Mutable {
                id: m.0.clone(),
                changed: Cell::new(false),
                weight: Cell::new(NonZeroU32::new(if i % 2 == 0 { 1 } else { 9 }).unwrap()),
            })
            .collect();
        let frozen = Membership::new(values).unwrap();
        let expected = Membership::new(
            frozen
                .members()
                .iter()
                .map(|m| TestMember(m.id.clone(), m.weight.get()))
                .collect(),
        )
        .unwrap();
        let paths = Paths::new(1);
        for m in frozen.members() {
            m.changed.set(true);
            m.weight.set(NonZeroU32::new(u32::MAX).unwrap());
        }
        for seed in [b"a".as_slice(), b"b", b"c"] {
            let q = PathQuery {
                seed,
                ..query(0, 1499, 4)
            };
            assert_eq!(
                block_on(paths.route(&frozen, q)),
                block_on(Paths::new(0).route(&expected, q))
            );
        }
        assert_eq!(paths.cached_entries(), 1);
    }

    /// Memberships with different weights share a search but pick separately.
    #[test]
    fn concurrent_weight_snapshots_share_search_but_not_selection() {
        let members = membership(10_000);
        let q = query(0, 9999, 4);
        let alternatives = search(&members, &PathKey::new(&members, &q).unwrap()).unwrap();
        assert!(alternatives.len() > 1);
        let mut values = members.members().to_vec();
        let favored = alternatives[0][1];
        values[favored].1 = NonZeroU32::new(u32::MAX).unwrap();
        let changed = Membership::new(values).unwrap();
        let paths = Paths::with_limits(0, 0, 1);
        let mut cx = Context::from_waker(noop_waker_ref());
        let mut old = Box::pin(paths.route(&members, q));
        let mut new = Box::pin(paths.route(&changed, q));
        assert!(old.as_mut().poll(&mut cx).is_pending());
        assert!(new.as_mut().poll(&mut cx).is_pending());
        assert_eq!(paths.active_searches(), 1);
        let expected_old = block_on(Paths::new(0).route(&members, q)).unwrap();
        assert_eq!(block_on(old).unwrap(), expected_old);
        assert_eq!(paths.active_searches(), 0);
        assert_eq!(block_on(new).unwrap()[1], favored);
    }

    /// Failed searches free their slot; cache hits work when slots are full.
    #[test]
    fn unreachable_releases_admission_and_cache_hits_ignore_overload() {
        let members = membership(10_000);
        let paths = Paths::with_limits(1, 1024 * 1024, 1);
        let cached = query(0, 9999, 4);
        let expected = block_on(paths.route(&members, cached)).unwrap();
        let mut pending = Box::pin(paths.route(&members, query(0, 9999, 5)));
        let mut cx = Context::from_waker(noop_waker_ref());
        assert!(pending.as_mut().poll(&mut cx).is_pending());
        let mut hit = Box::pin(paths.route(&members, cached));
        assert_eq!(hit.as_mut().poll(&mut cx), Poll::Ready(Ok(expected)));
        assert_eq!(paths.active_searches(), 1);
        drop(pending);
        assert_eq!(
            block_on(paths.route(&members, query(0, 9999, 1))),
            Err(Error::Unreachable)
        );
        assert_eq!(paths.active_searches(), 0);
        let blocked = members.neighbors(0);
        assert_eq!(
            block_on(paths.route(
                &members,
                PathQuery {
                    blocked: &blocked,
                    ..cached
                }
            )),
            Err(Error::Unreachable)
        );
        assert_eq!(paths.active_searches(), 0);
        assert_eq!(paths.cached_entries(), 1);
    }

    /// Routes work at the maximum of 255 links.
    #[test]
    fn full_u8_link_limit_reconstructs_without_overflow() {
        let members = membership(257);
        let mut graph = vec![vec![]; 257];
        for position in 1..257 {
            graph[position - 1].push(position);
            graph[position].push(position - 1);
        }
        let graph = Arc::new(graph);
        for (destination, reachable) in [(254, true), (255, true), (256, false)] {
            let key = PathKey::new(&members, &query(0, destination, 255)).unwrap();
            let mut search = EqualCostSearch::new(Arc::clone(&graph), &key);
            while !search.step(7).done {}
            if reachable {
                assert_eq!(
                    search.finish().unwrap(),
                    vec![(0..=destination).collect::<Vec<_>>()]
                );
            } else {
                assert_eq!(search.finish(), Err(Error::Unreachable));
            }
        }
    }
}
