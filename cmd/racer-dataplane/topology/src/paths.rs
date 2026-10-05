//! Bounded-degree graph and cooperative equal-cost shortest-path routing.
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

const SEARCH_QUANTUM: usize = 32;
const MAX_SEED_BYTES: usize = 65_536;
type Result<T> = std::result::Result<T, Error>;

/// Positions in one membership. `visited` excludes both endpoints and contains
/// no duplicates; its length plus `links` may not exceed `u8::MAX`. `blocked`
/// excludes only edges out of `from`, not these members at subsequent hops.
/// Unordered sets are canonicalized; duplicate blocked positions are accepted.
#[derive(Clone, Copy, Debug)]
pub struct PathQuery<'a> {
    /// Source position in the supplied membership.
    pub from: usize,
    /// Destination position in the supplied membership.
    pub to: usize,
    /// Maximum number of edges in the returned route.
    pub links: u8,
    /// Previously visited positions, distinct and excluding both endpoints.
    pub visited: &'a [usize],
    /// At most 64 positions (including duplicates). Only source edges are blocked;
    /// valid positions that are not source neighbors are ignored canonically.
    pub blocked: &'a [usize],
    /// Opaque selection seed, at most 65,536 bytes. The v5 selection schema
    /// length-prefixes seed, source ID, and destination ID separately.
    pub seed: &'a [u8],
}

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

type Alternatives = Rc<Vec<Vec<usize>>>;
#[derive(Default)]
struct PathCache {
    entries: BTreeMap<Rc<PathKey>, CacheEntry>,
    oldest: Option<Rc<PathKey>>,
    newest: Option<Rc<PathKey>>,
    bytes: usize,
}
struct CacheEntry {
    alternatives: Alternatives,
    older: Option<Rc<PathKey>>,
    newer: Option<Rc<PathKey>>,
    bytes: usize,
}
impl PathCache {
    // Charge a full current-std B-tree internal node for every entry, rather
    // than relying on occupancy: 11 key/value slots, 12 child pointers, and a
    // generously rounded header. This deliberately overcounts sparse nodes.
    // The standard library does not promise node layout: this is an estimate,
    // not a hard allocator bound. Shared keys are allocated/counted only once.
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

enum SharedSearch {
    Working(Option<Box<EqualCostSearch>>),
    Done(Result<Alternatives>),
}
impl SharedSearch {
    fn poll(
        &mut self,
        paths: &Paths,
        key: &PathKey,
        cx: &std::task::Context<'_>,
    ) -> Poll<Result<Alternatives>> {
        if let Self::Working(search) = self {
            let step = search.as_mut().unwrap().step(SEARCH_QUANTUM);
            debug_assert!(
                step.expansions <= SEARCH_QUANTUM && step.edges <= step.expansions * MAX_DEGREE
            );
            if !step.done {
                // Every waiter drives progress; no leader or unbounded waker list.
                cx.waker().wake_by_ref();
                return Poll::Pending;
            }
            let result = (*search.take().unwrap()).finish().map(Rc::new);
            if let Ok(alternatives) = &result {
                paths.store(key.clone(), Rc::clone(alternatives));
            }
            paths.inflight.borrow_mut().remove(key);
            *self = Self::Done(result);
        }
        let Self::Done(result) = self else {
            unreachable!()
        };
        Poll::Ready(result.clone())
    }
}

/// Worker-local bounded cache of eligible alternatives, reselected for every seed.
/// No executor, clocks, I/O, health state, or application membership leases.
pub struct Paths {
    cache_entries: usize,
    cache_bytes: usize,
    search_limit: usize,
    cache: RefCell<PathCache>,
    inflight: RefCell<BTreeMap<PathKey, Rc<RefCell<SharedSearch>>>>,
}
struct SearchAdmission<'a> {
    paths: &'a Paths,
    key: PathKey,
    shared: Rc<RefCell<SharedSearch>>,
}
impl Drop for SearchAdmission<'_> {
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
    /// Cache at most `capacity` queries and 8 MiB of accounted storage.
    /// Independently admit eight distinct cold searches, even with caching off.
    #[must_use]
    pub fn new(capacity: usize) -> Self {
        Self::with_limits(capacity, 8 * 1024 * 1024, 8)
    }

    /// Set independent cache entry, accounted byte, and distinct search limits.
    /// Either zero cache limit disables storage; zero active searches permits
    /// only cache hits and validated self routes. Identical in-flight queries
    /// share one admission slot, irrespective of seeds or frozen weights.
    ///
    /// Accounting includes key/result buffer capacities, `Rc` headers, and a
    /// conservative full B-tree node allowance per entry (11 key/value slots,
    /// 12 child pointers, rounded header). It excludes allocator overhead, this
    /// object, membership graphs, and active scratch/caller-retained results.
    /// Standard-library node layout is not guaranteed; this is an accounting
    /// budget, not a hard allocator limit. Oversized entries bypass the cache.
    /// Hits refresh LRU links in O(log entries), without cache allocations.
    /// Insertion takes O((evicted + 1) log entries), with no route rescanning.
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

    /// Number of currently cached query alternatives.
    #[must_use]
    pub fn cached_entries(&self) -> usize {
        self.cache.borrow().entries.len()
    }
    /// Constant-time accounted cache bytes; never exceeds its configured limit.
    /// See [`Self::with_limits`] for included and excluded storage.
    #[must_use]
    pub fn cached_bytes(&self) -> usize {
        self.cache.borrow().bytes
    }
    /// Number of distinct unfinished searches, not the number of waiting callers.
    #[must_use]
    pub fn active_searches(&self) -> usize {
        self.inflight.borrow().len()
    }

    /// Yield after at most 32 vertex expansions, each examining at most 64 edges.
    /// Reconstruction is bounded by 64 alternatives of at most 255 edges. Cache
    /// insertion may evict entries to meet the independently configured limits.
    /// Dropping the last waiter releases admission and scratch state; dropping
    /// one duplicate waiter leaves the shared search available to the others.
    ///
    /// # Errors
    /// Returns [`Error::InvalidQuery`] for invalid positions, repeated visited
    /// positions, visited endpoints, visited length plus links above 255, more
    /// than 64 blocked inputs, or a seed exceeding 65,536 bytes. Validation also
    /// applies to self routes. Returns [`Error::Unreachable`] when no route fits,
    /// [`Error::Overloaded`] when a distinct cold search cannot be admitted, or
    /// [`Error::SamplingExhausted`] after 64 rejected unbiased selection draws.
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
                let shared = Rc::new(RefCell::new(SharedSearch::Working(Some(Box::new(
                    EqualCostSearch::new(membership.graph(), &key),
                )))));
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

    fn store(&self, key: PathKey, alternatives: Alternatives) {
        self.cache
            .borrow_mut()
            .store(key, alternatives, self.cache_entries, self.cache_bytes);
    }
}

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

/// Integer rejection sampling avoids modulo bias and platform variance. At most
/// 64 u32 weights fit in u64; the retry cap bounds work without a biased fallback.
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
fn sample_index(weights: &[u64], total: u64, mut draw: impl FnMut(u32) -> u64) -> Result<usize> {
    for counter in 0u32..64 {
        if let Some(index) = weighted_draw(draw(counter), total, weights) {
            return Ok(index);
        }
    }
    Err(Error::SamplingExhausted)
}
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

struct Visit {
    depth: u8,
    parent: usize,
    first: u64,
}
struct EqualCostWave {
    visits: BTreeMap<usize, Visit>,
    queue: VecDeque<usize>,
}
impl EqualCostWave {
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

/// Complete the meeting layer, carrying source first-hop bitsets, not all paths.
/// A complete preceding source layer propagates every equal-depth first-hop bit
/// before its children expand. Complete the intersecting layer too: stopping at
/// its first intersection would bias selection toward sorted IDs. Before that
/// layer the two balls are disjoint, so every meeting has minimum total distance.
/// Store one canonical witness per first hop (at most 64), not per full path.
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
/// Per-turn observability in production and tests, without retained counters.
#[derive(Default)]
struct SearchStep {
    done: bool,
    expansions: usize,
    edges: usize,
}
impl EqualCostSearch {
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
    fn finish(self) -> Result<Vec<Vec<usize>>> {
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
mod tests;
