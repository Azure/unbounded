//! Bounded-degree graph and cooperative equal-cost shortest-path routing.
use crate::{Error, MAX_DEGREE, Member, Membership, RADIX, hash};
use sha2::Digest;
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    task::Poll,
};

const SEARCH_QUANTUM: usize = 32;
type Result<T> = std::result::Result<T, Error>;

impl<M: Member> Membership<M> {
    /// Sorted union of incoming/outgoing radix edges, excluding self and duplicates.
    /// An invalid position (including in empty membership) returns an empty list.
    pub fn neighbors(&self, position: usize) -> Vec<usize> {
        neighbor_positions_for(self.members().len(), position)
    }
}

/// Incoming intervals are disjoint lifts of `node` modulo N. Their quotient
/// by the radix gives every predecessor without scanning any other member.
fn neighbor_positions_for(count: usize, node: usize) -> Vec<usize> {
    if node >= count {
        return vec![];
    }
    // Widen before multiplication: the public algorithm has no application size cap.
    let count = count as u128;
    let node = node as u128;
    let radix = RADIX as u128;
    let mut neighbors = Vec::with_capacity(MAX_DEGREE);
    for digit in 0..radix {
        neighbors.push(((radix * node + digit) % count) as usize);
        neighbors.push(((node + digit * count) / radix) as usize);
    }
    neighbors.sort_unstable();
    neighbors.dedup();
    neighbors.retain(|other| *other != node as usize);
    neighbors
}

/// Positions in one membership. `visited` excludes both endpoints and contains
/// no duplicates; its length plus `links` may not exceed u8::MAX. `blocked`
/// excludes only edges out of `from`, not these members at subsequent hops.
/// Unordered sets are canonicalized; duplicate blocked positions are accepted.
#[derive(Clone, Copy, Debug)]
pub struct PathQuery<'a> {
    pub from: usize,
    pub to: usize,
    pub links: u8,
    pub visited: &'a [usize],
    pub blocked: &'a [usize],
    /// Opaque selection seed, hashed before length-prefixed source/destination IDs.
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
        Ok(Self {
            membership: membership.identity(),
            from: query.from,
            to: query.to,
            links: query.links,
            visited,
            blocked,
        })
    }
}

#[derive(Default)]
struct PathCache {
    entries: BTreeMap<PathKey, Vec<Vec<usize>>>,
    fifo: VecDeque<PathKey>,
}

/// Worker-local bounded cache of eligible alternatives, reselected for every seed.
/// No executor, clocks, I/O, health state, or application membership leases.
pub struct Paths {
    capacity: usize,
    cache: RefCell<PathCache>,
    active_searches: Cell<usize>,
}
struct SearchAdmission<'a>(&'a Cell<usize>);
impl Drop for SearchAdmission<'_> {
    fn drop(&mut self) {
        self.0.set(self.0.get() - 1);
    }
}
impl Paths {
    pub fn new(capacity: usize) -> Self {
        Self {
            capacity,
            cache: RefCell::new(PathCache::default()),
            active_searches: Cell::new(0),
        }
    }

    /// Yield after at most 32 vertex expansions, each examining at most 64 edges.
    /// Dropping the future releases search admission and all scratch state.
    pub async fn route<'a, M: Member>(
        &'a self,
        membership: &'a Membership<M>,
        query: PathQuery<'a>,
    ) -> Result<Vec<usize>> {
        let key = PathKey::new(membership, &query)?;
        if key.from != key.to && key.links == 0 {
            return Err(Error::Unreachable);
        }
        if let Some(alternatives) = self.cache.borrow().entries.get(&key) {
            return select_route(membership, alternatives, query.seed);
        }
        // A disabled cache still permits one cold computation at a time.
        if self.active_searches.get() >= self.capacity.clamp(1, 8) {
            return Err(Error::Overloaded);
        }
        self.active_searches.set(self.active_searches.get() + 1);
        let _admission = SearchAdmission(&self.active_searches);
        let mut search = EqualCostSearch::new(membership.members().len(), &key);
        std::future::poll_fn(|cx| {
            if search.step(SEARCH_QUANTUM) {
                Poll::Ready(())
            } else {
                cx.waker().wake_by_ref();
                Poll::Pending
            }
        })
        .await;
        let alternatives = search.finish()?;
        let selected = select_route(membership, &alternatives, query.seed);
        self.store(key, alternatives);
        selected
    }

    fn store(&self, key: PathKey, alternatives: Vec<Vec<usize>>) {
        if self.capacity == 0 {
            return;
        }
        let mut cache = self.cache.borrow_mut();
        if cache.entries.contains_key(&key) {
            return;
        }
        if cache.entries.len() == self.capacity {
            let victim = cache.fifo.pop_front().unwrap();
            cache.entries.remove(&victim);
        }
        cache.fifo.push_back(key.clone());
        cache.entries.insert(key, alternatives);
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

/// Positive weights apply only to eligible next hops. Integer rejection sampling
/// avoids modulo bias, floats, and platform variance. At most 64 u32 weights fit
/// in u64; the retry cap bounds adversarial work without a biased fallback.
fn weighted_index<M: Member>(
    membership: &Membership<M>,
    alternatives: &[Vec<usize>],
    seed: &[u8],
) -> Result<usize> {
    let weights: Vec<_> = alternatives
        .iter()
        .map(|path| u64::from(membership.members()[path[1]].weight().get()))
        .collect();
    let total: u64 = weights.iter().sum();
    let mut digest = hash::domain::<M>(b"/next-hop/v4\0");
    digest.update(seed);
    let path = &alternatives[0];
    hash::bytes(&mut digest, membership.members()[path[0]].id());
    hash::bytes(
        &mut digest,
        membership.members()[*path.last().unwrap()].id(),
    );
    for counter in 0u32..64 {
        let mut draw = digest.clone();
        draw.update(counter.to_be_bytes());
        let sample = u64::from_be_bytes(hash::finish(draw)[..8].try_into().unwrap());
        if let Some(index) = weighted_draw(sample, total, &weights) {
            return Ok(index);
        }
    }
    Err(Error::Unreachable)
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
    count: usize,
    key: PathKey,
    neighbors: Vec<usize>,
    waves: [EqualCostWave; 2],
    side: usize,
    remaining: usize,
    layers: u8,
    meetings: Vec<Option<usize>>,
    done: bool,
    #[cfg(test)]
    expansions: usize,
    #[cfg(test)]
    edges: usize,
}
impl EqualCostSearch {
    #[cfg(test)]
    fn visited_entries(&self) -> usize {
        self.waves.iter().map(|wave| wave.visits.len()).sum()
    }

    fn new(count: usize, key: &PathKey) -> Self {
        let neighbors = neighbor_positions_for(count, key.from);
        assert!(neighbors.len() <= MAX_DEGREE);
        Self {
            count,
            key: key.clone(),
            meetings: vec![None; neighbors.len()],
            neighbors,
            waves: [EqualCostWave::new(key.from), EqualCostWave::new(key.to)],
            side: 0,
            remaining: 1,
            layers: 0,
            done: key.from == key.to || key.links == 0,
            #[cfg(test)]
            expansions: 0,
            #[cfg(test)]
            edges: 0,
        }
    }
    fn step(&mut self, quantum: usize) -> bool {
        for _ in 0..quantum {
            if self.done {
                return true;
            }
            let Some(node) = self.waves[self.side].queue.pop_front() else {
                self.done = true;
                return true;
            };
            #[cfg(test)]
            {
                self.expansions += 1;
            }
            let depth = self.waves[self.side].visits[&node].depth + 1;
            let first = self.waves[self.side].visits[&node].first;
            for next in neighbor_positions_for(self.count, node) {
                #[cfg(test)]
                {
                    self.edges += 1;
                }
                if self.key.visited.binary_search(&next).is_ok()
                    || (node == self.key.from && self.key.blocked.binary_search(&next).is_ok())
                    || (next == self.key.from && self.key.blocked.binary_search(&node).is_ok())
                {
                    continue;
                }
                let bits = if self.side == 0 && node == self.key.from {
                    1u64 << self.neighbors.binary_search(&next).unwrap()
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
        self.done
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
                    neighbor_positions_for(self.count, current)
                        .into_iter()
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
mod tests {
    use super::*;
    use futures::{executor::block_on, task::noop_waker_ref};
    use std::{future::Future, num::NonZeroU32, task::Context};

    #[derive(Clone)]
    struct TestMember(Vec<u8>, NonZeroU32);
    impl Member for TestMember {
        const DOMAIN: &'static str = "racer";
        fn id(&self) -> &[u8] {
            &self.0
        }
        fn weight(&self) -> NonZeroU32 {
            self.1
        }
    }
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
    // Independent outgoing-edge definition, not the production inverse algorithm.
    fn graph(n: usize) -> Vec<Vec<usize>> {
        let mut edges = vec![vec![]; n];
        for i in 0..n {
            for digit in 0..32 {
                let j = (32 * i + digit) % n;
                if i != j {
                    edges[i].push(j);
                    edges[j].push(i);
                }
            }
        }
        for neighbors in &mut edges {
            neighbors.sort_unstable();
            neighbors.dedup();
        }
        edges
    }
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
    fn search(n: usize, key: &PathKey) -> Result<Vec<Vec<usize>>> {
        let mut search = EqualCostSearch::new(n, key);
        while !search.step(1) {}
        search.finish()
    }
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
                            let result = search(n, &key);
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
    #[test]
    fn independent_hash_vectors_cache_reselection_and_snapshot_weights() {
        let mut input = membership(1500).members().to_vec();
        for (i, member) in input.iter_mut().enumerate() {
            member.1 = NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap();
        }
        let members = Membership::new(input).unwrap();
        let cached = Paths::new(1);
        let cold = Paths::new(0);
        // Independent Python hashlib + outgoing-edge BFS vectors, also asserted
        // through the Racer adapter to verify request||attempt seed construction.
        for (attempt, expected) in [(0u128, 1312), (1, 937), (2, 703), (127, 937)] {
            let mut seed = vec![1; 16];
            seed.extend_from_slice(&attempt.to_be_bytes());
            let query = PathQuery {
                seed: &seed,
                ..query(0, 1499, 4)
            };
            let actual = block_on(cached.route(&members, query)).unwrap();
            assert_eq!(actual[1], expected);
            assert_eq!(actual, block_on(cold.route(&members, query)).unwrap());
        }
        let mut changed = members.members().to_vec();
        changed[1312].1 = NonZeroU32::new(u32::MAX).unwrap();
        let changed = Membership::new(changed).unwrap();
        let mut seed = vec![1; 16];
        seed.extend_from_slice(&[2; 16]);
        let query = PathQuery {
            seed: &seed,
            ..query(0, 1499, 4)
        };
        assert_eq!(block_on(cached.route(&changed, query)).unwrap()[1], 1312);
        assert_eq!(cached.cache.borrow().entries.len(), 1);
        assert_eq!(cached.cache.borrow().fifo.len(), 1);
        assert_eq!(cold.cache.borrow().entries.len(), 0);
    }
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
            let alternatives = search(n, &key).unwrap();
            assert_eq!(alternatives.len(), 1);
            assert_eq!(alternatives[0][1], last);
            assert_eq!(alternatives[0].last(), Some(&to));
        }
        key.blocked = neighbors;
        assert_eq!(search(n, &key), Err(Error::Unreachable));
    }
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
    #[test]
    fn search_work_is_bounded_per_turn() {
        let members = membership(100_000);
        let key = PathKey::new(&members, &query(0, 80_003, 4)).unwrap();
        let mut search = EqualCostSearch::new(100_000, &key);
        loop {
            let before = search.expansions;
            let done = search.step(7);
            assert!(search.expansions - before <= 7);
            assert!(search.edges <= search.expansions * MAX_DEGREE);
            assert!(search.visited_entries() <= 2 * 100_000);
            if done {
                break;
            }
        }
        let alternatives = search.finish().unwrap();
        assert!(alternatives.len() <= MAX_DEGREE);
        assert!(alternatives.iter().all(|p| p.len() <= 5));
    }
    #[test]
    fn exhaustive_inverse_matches_definition() {
        let radix = RADIX;
        for n in 1..=160 {
            for i in 0..n {
                let expected: Vec<_> = (0..n)
                    .filter(|&k| {
                        k != i
                            && (0..radix)
                                .any(|j| (radix * i + j) % n == k || (radix * k + j) % n == i)
                    })
                    .collect();
                assert_eq!(neighbor_positions_for(n, i), expected, "N={n}, i={i}");
            }
        }
        assert!(neighbor_positions_for(0, 0).is_empty());
        assert!(neighbor_positions_for(1, usize::MAX).is_empty());
        assert!(
            neighbor_positions_for(usize::MAX, usize::MAX - 1)
                .iter()
                .all(|&v| v < usize::MAX)
        );
    }
    #[test]
    fn hundred_thousand_nodes_bounded_symmetric_and_four_link_reachable() {
        let n = 100_000;
        for i in 0..n {
            let neighbors = neighbor_positions_for(n, i);
            assert!(neighbors.len() <= MAX_DEGREE);
            for &other in &neighbors {
                assert!(neighbor_positions_for(n, other).binary_search(&i).is_ok());
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
                for j in neighbor_positions_for(n, i) {
                    if distance[j] == u8::MAX {
                        distance[j] = distance[i] + 1;
                        queue.push_back(j);
                    }
                }
            }
            assert!(distance.iter().all(|d| *d <= 4));
        }
    }
    #[test]
    fn arbitrary_binary_members_invalid_queries_and_first_hop_only_blocking() {
        struct Binary([u8; 2]);
        impl Member for Binary {
            const DOMAIN: &'static str = "binary";
            fn id(&self) -> &[u8] {
                &self.0
            }
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
    #[test]
    fn canonical_cache_identity_includes_domain_and_weights() {
        struct Other(TestMember);
        impl Member for Other {
            const DOMAIN: &'static str = "other";
            fn id(&self) -> &[u8] {
                self.0.id()
            }
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
        assert_eq!(paths.cache.borrow().entries.len(), 3);
    }
    #[test]
    fn cooperative_admission_cancellation_and_cache_hits() {
        let members = membership(100_000);
        let query = query(0, 80_003, 4);
        let mut cx = Context::from_waker(noop_waker_ref());
        for capacity in [0, 1, 32] {
            let paths = Paths::new(capacity);
            let mut pending = Vec::new();
            for _ in 0..capacity.clamp(1, 8) {
                let mut future = Box::pin(paths.route(&members, query));
                assert!(future.as_mut().poll(&mut cx).is_pending());
                pending.push(future);
            }
            assert_eq!(
                block_on(paths.route(&members, query)),
                Err(Error::Overloaded)
            );
            drop(pending);
            assert_eq!(paths.active_searches.get(), 0);
            let expected = block_on(paths.route(&members, query)).unwrap();
            assert_eq!(paths.active_searches.get(), 0);
            let mut future = Box::pin(paths.route(&members, query));
            let first = future.as_mut().poll(&mut cx);
            if capacity == 0 {
                assert!(first.is_pending());
            } else {
                assert_eq!(first, Poll::Ready(Ok(expected)));
            }
        }
    }
}
