//! Versioned graph neighbors and deterministic bounded shortest-path routing.
//! Search caches eligible alternatives, then selects a next hop per request.
use super::{
    MAX_DEGREE, RADIX, hash,
    health::LinkHealth,
    membership::{Membership, MembershipLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::{AttemptId, NodeId, RequestId},
    runtime::deadline::{Deadline, RequestScope},
};
use sha2::Digest;
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    rc::Rc,
    sync::{Arc, Weak},
    task::Poll,
    time::Instant,
};

/// Versioned union of incoming/outgoing (radix*i+j)%N edges, excluding self and duplicates.
pub struct Graph {
    membership: MembershipLease,
}
impl Graph {
    pub fn new(membership: MembershipLease) -> Self {
        Self { membership }
    }
    pub fn neighbors(&self, node: &NodeId) -> Result<Vec<NodeId>> {
        let position = self.membership.position(node)?;
        Ok(
            neighbor_positions_for(self.membership.members().len(), position)
                .into_iter()
                .map(|index| self.membership.members()[index].node.clone())
                .collect(),
        )
    }
}

/// The incoming intervals are disjoint lifts of `node` modulo N. Their quotient
/// by the radix gives every predecessor without scanning any other member.
fn neighbor_positions_for(count: usize, node: usize) -> Vec<usize> {
    debug_assert!(node < count);
    let radix = RADIX;
    let mut neighbors = Vec::with_capacity(MAX_DEGREE);
    for digit in 0..radix {
        neighbors.push((radix * node + digit) % count);
        neighbors.push((node + digit * count) / radix);
    }
    neighbors.sort_unstable();
    neighbors.dedup();
    neighbors.retain(|other| *other != node);
    neighbors
}

#[derive(Clone, Debug)]
pub struct Route {
    pub membership: MembershipLease,
    pub nodes: Vec<NodeId>,
}
/// Signed forwarding state. Retries preserve deadline and consumed link budget.
#[derive(Clone, Debug)]
pub struct RouteBudget {
    pub membership: crate::model::MembershipVersion,
    pub request: RequestId,
    pub attempt: AttemptId,
    pub destination: NodeId,
    pub visited: Vec<NodeId>,
    pub remaining_links: u8,
    /// Acquisition credits transferred from the caller, never replenished by a hop.
    pub remaining_attempts: u32,
    pub deadline: Deadline,
}

pub const NORMAL_LINKS: u8 = 4;
pub const FAILURE_LINKS: u8 = 8;
const SEARCH_QUANTUM: usize = 32;

impl RouteBudget {
    /// `visited` contains prior senders, excluding the current recipient.
    pub fn forwarded(&self, from: &NodeId, next: &NodeId) -> Result<Self> {
        self.validate_at(from, crate::runtime::environment::now())?;
        if self.remaining_links == 0 {
            return Err(Error::HopBudgetExhausted);
        }
        if next == from || self.visited.contains(next) {
            return Err(Error::InvalidRequest);
        }
        let mut forwarded = self.clone();
        forwarded.visited.push(from.clone());
        forwarded.remaining_links -= 1;
        Ok(forwarded)
    }

    /// Authentication verifies this monotonic relationship between signed hops.
    pub fn validate_forwarded(&self, forwarded: &Self, from: &NodeId, next: &NodeId) -> Result<()> {
        let expected = self.forwarded(from, next)?;
        if forwarded.membership != expected.membership
            || forwarded.request != expected.request
            || forwarded.attempt != expected.attempt
            || forwarded.destination != expected.destination
            || forwarded.visited != expected.visited
            || forwarded.remaining_links != expected.remaining_links
            || forwarded.remaining_attempts > expected.remaining_attempts
            || forwarded.deadline.0 > expected.deadline.0
        {
            return Err(Error::InvalidRequest);
        }
        forwarded.validate_at(next, crate::runtime::environment::now())
    }

    fn validate_at(&self, from: &NodeId, now: Instant) -> Result<()> {
        if now >= self.deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        if self.visited.len() > usize::from(FAILURE_LINKS)
            || self.visited.len() + usize::from(self.remaining_links) > usize::from(FAILURE_LINKS)
        {
            return Err(Error::HopBudgetExhausted);
        }
        if self.visited.contains(from)
            || self.visited.contains(&self.destination)
            || self
                .visited
                .iter()
                .enumerate()
                .any(|(i, node)| self.visited[..i].contains(node))
        {
            return Err(Error::InvalidRequest);
        }
        Ok(())
    }
}

#[derive(Clone, Eq, PartialEq, Ord, PartialOrd)]
struct PathKey {
    membership: usize,
    from: usize,
    to: usize,
    links: u8,
    visited: Vec<usize>,
    failed: Vec<usize>,
}

#[derive(Default)]
struct PathCache {
    entries: BTreeMap<PathKey, (Weak<Membership>, Vec<Vec<usize>>)>,
    fifo: VecDeque<PathKey>,
}

pub struct Paths {
    pub(crate) peer_admission: Option<Arc<crate::peer::adaptive::AdaptivePeers>>,
    health: Rc<LinkHealth>,
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
    pub(crate) fn with_peer_admission(
        mut self,
        admission: Arc<crate::peer::adaptive::AdaptivePeers>,
    ) -> Self {
        self.peer_admission = Some(admission);
        self
    }
    pub fn link_health(&self) -> Rc<LinkHealth> {
        self.health.clone()
    }
    pub fn new(health: Rc<LinkHealth>, capacity: usize) -> Self {
        Self {
            peer_admission: None,
            health,
            capacity,
            cache: RefCell::new(PathCache::default()),
            active_searches: Cell::new(0),
        }
    }
    #[cfg(test)]
    pub fn shortest(
        &self,
        membership: MembershipLease,
        from: &NodeId,
        budget: &RouteBudget,
    ) -> Result<Route> {
        let scope = RequestScope::new(budget.request, budget.deadline.0)?;
        futures::executor::block_on(self.shortest_async(membership, from, budget, &scope))
    }

    /// Yield after at most 32 vertex expansions.
    /// Each expansion examines at most 64 edges. Dropping the future cancels it.
    pub fn shortest_async<'a>(
        &'a self,
        membership: MembershipLease,
        from: &'a NodeId,
        budget: &'a RouteBudget,
        scope: &'a RequestScope,
    ) -> Operation<'a, Route> {
        Box::pin(async move {
            scope.check()?;
            let key = self.key(&membership, from, budget)?;
            if let Some(route) = self.cached(&membership, &key, budget) {
                return route;
            }
            let _admission = self.admit_search()?;
            let mut search = EqualCostSearch::new(membership.members().len(), &key);
            let deadline = Deadline(budget.deadline.0.min(scope.deadline.0));
            std::future::poll_fn(|cx| {
                match scope
                    .check()
                    .and_then(|()| search.step(SEARCH_QUANTUM, deadline))
                {
                    Ok(true) => Poll::Ready(Ok(())),
                    Err(error) => Poll::Ready(Err(error)),
                    Ok(false) => {
                        cx.waker().wake_by_ref();
                        Poll::Pending
                    }
                }
            })
            .await?;
            scope.check()?;
            let nodes = search.finish()?;
            // Health may have changed while yielded: retry under the same budget.
            if self.key(&membership, from, budget)? != key {
                return Err(Error::Unavailable);
            }
            self.store(&membership, key, &nodes);
            select_route(membership, &nodes, budget)
        })
    }

    fn admit_search(&self) -> Result<SearchAdmission<'_>> {
        // Bound aggregate scratch memory as well as work per operation. Even a
        // disabled route cache permits one cold computation at a time.
        if self.active_searches.get() >= self.capacity.clamp(1, 8) {
            return Err(Error::Overloaded);
        }
        self.active_searches.set(self.active_searches.get() + 1);
        Ok(SearchAdmission(&self.active_searches))
    }

    fn key(
        &self,
        membership: &MembershipLease,
        from: &NodeId,
        budget: &RouteBudget,
    ) -> Result<PathKey> {
        budget.validate_at(from, crate::runtime::environment::now())?;
        if membership.version != budget.membership {
            return Err(Error::IncompatibleMembership);
        }
        let source = membership.position(from)?;
        let to = membership.position(&budget.destination)?;
        if source != to && budget.remaining_links == 0 {
            return Err(Error::HopBudgetExhausted);
        }
        let mut visited = budget
            .visited
            .iter()
            .map(|node| membership.position(node))
            .collect::<Result<Vec<_>>>()?;
        visited.sort_unstable();
        let mut failed = Vec::new();
        let neighbors: Vec<_> = neighbor_positions_for(membership.members().len(), source)
            .into_iter()
            .map(|position| membership.members()[position].node.clone())
            .collect();
        self.health.retain_neighbors(&neighbors);
        for neighbor in neighbor_positions_for(membership.members().len(), source) {
            if !self
                .health
                .available(&membership.members()[neighbor].node)?
                || self
                    .peer_admission
                    .as_ref()
                    .is_some_and(|a| !a.available(&membership.members()[neighbor].node))
            {
                failed.push(neighbor);
            }
        }
        Ok(PathKey {
            membership: Arc::as_ptr(membership) as usize,
            from: source,
            to,
            links: budget.remaining_links,
            visited,
            failed,
        })
    }

    fn cached(
        &self,
        membership: &MembershipLease,
        key: &PathKey,
        budget: &RouteBudget,
    ) -> Option<Result<Route>> {
        let cache = self.cache.borrow();
        let (weak, nodes) = cache.entries.get(key)?;
        weak.upgrade()?;
        Some(select_route(membership.clone(), nodes, budget))
    }

    fn store(&self, membership: &MembershipLease, key: PathKey, nodes: &[Vec<usize>]) {
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
        cache
            .entries
            .insert(key, (Arc::downgrade(membership), nodes.to_vec()));
    }

    #[cfg(test)]
    fn cached_paths(&self) -> usize {
        self.cache.borrow().entries.len()
    }
}

fn select_route(
    membership: MembershipLease,
    alternatives: &[Vec<usize>],
    budget: &RouteBudget,
) -> Result<Route> {
    let index = if alternatives.len() == 1 {
        0
    } else {
        weighted_index(&membership, alternatives, budget)?
    };
    Ok(route(membership, &alternatives[index]))
}

/// Positive authenticated membership shares weight only the eligible next hop.
/// Integer rejection sampling avoids modulo bias, floats, and platform variance.
/// At most 64 u32 weights fit in u64. The retry cap bounds adversarial work; a
/// rejected draw never falls back to a biased choice. See the v4 contract.
fn weighted_index(
    membership: &Membership,
    alternatives: &[Vec<usize>],
    budget: &RouteBudget,
) -> Result<usize> {
    let weights: Vec<_> = alternatives
        .iter()
        .map(|path| u64::from(membership.members()[path[1]].shares.get()))
        .collect();
    let total: u64 = weights.iter().sum();
    let mut digest = hash::domain(b"racer/next-hop/v4\0");
    digest.update(budget.request.0);
    digest.update(budget.attempt.0);
    hash::bytes(
        &mut digest,
        membership.members()[alternatives[0][0]].node.0.as_bytes(),
    );
    hash::bytes(&mut digest, budget.destination.0.as_bytes());
    for counter in 0u32..64 {
        if crate::runtime::environment::now() >= budget.deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        let mut draw = digest.clone();
        draw.update(counter.to_be_bytes());
        let sample = u64::from_be_bytes(hash::finish(draw)[..8].try_into().unwrap());
        if let Some(index) = weighted_draw(sample, total, &weights) {
            return Ok(index);
        }
    }
    Err(Error::Unavailable)
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

fn route(membership: MembershipLease, positions: &[usize]) -> Route {
    let nodes = positions
        .iter()
        .map(|&index| membership.members()[index].node.clone())
        .collect();
    Route { membership, nodes }
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
///
/// A complete preceding source layer has already propagated every equal-depth
/// first-hop bit before its children expand. Complete the intersecting layer too:
/// stopping at its first intersection would retain v2's sorted-UID bias. Before
/// that layer the two balls are disjoint, so each intersection has minimum total
/// distance. Store one meeting per first hop (at most 64), not one per full path.
/// Reconstruct one canonical witness per bit. Relays reselect at their own hop.
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
            done: key.from == key.to,
            #[cfg(test)]
            expansions: 0,
            #[cfg(test)]
            edges: 0,
        }
    }

    fn step(&mut self, quantum: usize, deadline: Deadline) -> Result<bool> {
        for _ in 0..quantum {
            if crate::runtime::environment::now() >= deadline.0 {
                return Err(Error::DeadlineExceeded);
            }
            if self.done {
                return Ok(true);
            }
            let Some(node) = self.waves[self.side].queue.pop_front() else {
                self.done = true;
                return Ok(true);
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
                    || (node == self.key.from && self.key.failed.binary_search(&next).is_ok())
                    || (next == self.key.from && self.key.failed.binary_search(&node).is_ok())
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
                    if depth + opposite.depth != self.layers + 1 {
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
        Ok(self.done)
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
                        .ok_or(Error::Internal)?
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
            return Err(Error::Unavailable);
        }
        Ok(routes)
    }
}

#[cfg(test)]
mod scenarios {
    use super::*;
    use crate::topology::{fixtures::membership, health::LinkOutcome};
    use std::time::Duration;
    fn budget(members: &MembershipLease, to: usize, links: u8) -> RouteBudget {
        RouteBudget {
            membership: members.version,
            request: RequestId([1; 16]),
            attempt: AttemptId([2; 16]),
            destination: members.members()[to].node.clone(),
            visited: vec![],
            remaining_links: links,
            remaining_attempts: 3,
            deadline: Deadline(Instant::now() + Duration::from_secs(60)),
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
                    || (node == key.from && key.failed.contains(&next))
                    || (next == key.from && key.failed.contains(&node))
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
    fn search(n: usize, key: &PathKey, deadline: Deadline) -> Result<Vec<Vec<usize>>> {
        let mut search = EqualCostSearch::new(n, key);
        while !search.step(1, deadline)? {}
        search.finish()
    }
    #[test]
    fn all_equal_next_hops_match_independent_oracle() {
        for n in [37, 401, 1500] {
            let edges = graph(n);
            let members = membership(n);
            let paths = Paths::new(Rc::new(LinkHealth), 4);
            for source in [0, 19, n - 1] {
                for to in (0..n).step_by(17).filter(|&to| to != source) {
                    let request = budget(&members, to, NORMAL_LINKS);
                    for filtered in [false, true] {
                        let mut key = paths
                            .key(&members, &members.members()[source].node, &request)
                            .unwrap();
                        if filtered {
                            key.visited = [7, 33]
                                .into_iter()
                                .filter(|v| *v != source && *v != to)
                                .collect();
                            key.failed = edges[source].iter().copied().step_by(2).collect();
                        }
                        let distance = distances(&edges, &key);
                        for links in [1, 2, 4] {
                            key.links = links;
                            let result = search(n, &key, request.deadline);
                            if distance[source] > links as usize {
                                assert_eq!(result, Err(Error::Unavailable));
                                continue;
                            }
                            let alternatives = result.unwrap();
                            let expected: Vec<_> = edges[source]
                                .iter()
                                .copied()
                                .filter(|v| {
                                    !key.failed.contains(v)
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
            member.shares = std::num::NonZeroU32::new(if i % 3 == 0 { 1 } else { 4 }).unwrap();
        }
        let members =
            Arc::new(Membership::validate(crate::model::MembershipVersion(1), input).unwrap());
        let cached = Paths::new(Rc::new(LinkHealth), 1);
        let cold = Paths::new(Rc::new(LinkHealth), 0);
        for (attempt, expected) in super::super::fixtures::V5_NEXT_HOPS {
            let mut request = budget(&members, 1499, NORMAL_LINKS);
            request.attempt = AttemptId(attempt.to_be_bytes());
            let from = &members.members()[0].node;
            let actual = cached.shortest(members.clone(), from, &request).unwrap();
            assert_eq!(actual.nodes[1], members.members()[expected].node);
            let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
            assert_eq!(
                actual.nodes,
                futures::executor::block_on(cold.shortest_async(
                    members.clone(),
                    from,
                    &request,
                    &scope
                ))
                .unwrap()
                .nodes
            );
        }
        let mut changed = members.members().to_vec();
        changed[1312].shares = std::num::NonZeroU32::new(u32::MAX).unwrap();
        let changed =
            Arc::new(Membership::validate(crate::model::MembershipVersion(2), changed).unwrap());
        let route = cached
            .shortest(
                changed.clone(),
                &changed.members()[0].node,
                &budget(&changed, 1499, 4),
            )
            .unwrap();
        assert_eq!(route.nodes[1], changed.members()[1312].node);
        assert_eq!(cached.cached_paths(), 1);
    }
    #[test]
    fn last_first_hop_bit_survives_search_and_reconstruction() {
        let n = 100_000;
        let members = membership(n);
        let source = 19;
        let neighbors = neighbor_positions_for(n, source);
        assert_eq!(neighbors.len(), 64);
        let last = neighbors[63];
        let paths = Paths::new(Rc::new(LinkHealth), 0);
        let request = budget(&members, last, NORMAL_LINKS);
        let mut key = paths
            .key(&members, &members.members()[source].node, &request)
            .unwrap();
        key.failed = neighbors[..63].to_vec();
        let next = neighbor_positions_for(n, last)
            .into_iter()
            .find(|v| *v != source && !neighbors.contains(v))
            .unwrap();
        for to in [last, next] {
            key.to = to;
            let alternatives = search(n, &key, request.deadline).unwrap();
            assert_eq!(alternatives.len(), 1);
            assert_eq!(alternatives[0][1], last);
            assert_eq!(alternatives[0].last(), Some(&to));
        }
        key.failed = neighbors;
        assert_eq!(search(n, &key, request.deadline), Err(Error::Unavailable));
    }
    #[test]
    fn health_eviction_budget_deadline_and_cancellation() {
        let members = membership(1500);
        let source = &members.members()[0].node;
        let health = Rc::new(LinkHealth);
        let paths = Paths::new(health.clone(), 1);
        let request = budget(&members, 1499, NORMAL_LINKS);
        let original = paths.shortest(members.clone(), source, &request).unwrap();
        let mut blocked = request.clone();
        blocked.visited.push(original.nodes[1].clone());
        let alternate = paths.shortest(members.clone(), source, &blocked).unwrap();
        assert_eq!(alternate.nodes.len(), original.nodes.len());
        assert!(!alternate.nodes.contains(&original.nodes[1]));
        assert_eq!(paths.cached_paths(), 1);
        health
            .observe_at(
                &original.nodes[1],
                LinkOutcome::Timeout,
                Instant::now() + Duration::from_secs(60),
            )
            .unwrap();
        assert_ne!(
            paths
                .shortest(members.clone(), source, &request)
                .unwrap()
                .nodes[1],
            original.nodes[1]
        );
        for neighbor in Graph::new(members.clone()).neighbors(source).unwrap() {
            health
                .observe_at(
                    &neighbor,
                    LinkOutcome::Timeout,
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap();
        }
        for (case, expected) in [
            (0, Error::Unavailable),
            (1, Error::HopBudgetExhausted),
            (2, Error::InvalidRequest),
            (3, Error::DeadlineExceeded),
        ] {
            let mut blocked = request.clone();
            match case {
                1 => blocked.remaining_links = 0,
                2 => blocked.visited.push(source.clone()),
                3 => blocked.deadline = Deadline(Instant::now()),
                _ => {}
            }
            assert_eq!(
                paths
                    .shortest(members.clone(), source, &blocked)
                    .unwrap_err(),
                expected
            );
        }
        let cold = Paths::new(Rc::new(LinkHealth), 0);
        assert_eq!(
            cold.shortest(members.clone(), source, &budget(&members, 0, 0))
                .unwrap()
                .nodes,
            vec![source.clone()]
        );
        let request = budget(&members, 1499, 4);
        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
        let mut operation = cold.shortest_async(members.clone(), source, &request, &scope);
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let _ = operation.as_mut().poll(&mut cx);
        drop(operation);
        assert_eq!(cold.active_searches.get(), 0);
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
        let paths = Paths::new(Rc::new(LinkHealth), 0);
        let request = budget(&members, 80_003, 4);
        let key = paths
            .key(&members, &members.members()[0].node, &request)
            .unwrap();
        let mut search = EqualCostSearch::new(100_000, &key);
        loop {
            let before = search.expansions;
            let done = search.step(7, request.deadline).unwrap();
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
    fn forwarded_budget_is_monotonic_and_rejects_cycles() {
        let members = membership(100);
        let request = budget(&members, 99, 4);
        let from = &members.members()[0].node;
        let next = &members.members()[1].node;
        let forwarded = request.forwarded(from, next).unwrap();
        request.validate_forwarded(&forwarded, from, next).unwrap();
        assert_eq!(forwarded.remaining_links, 3);
        assert_eq!(forwarded.remaining_attempts, request.remaining_attempts);
        assert!(forwarded.forwarded(next, from).is_err());
        let mut invalid = forwarded.clone();
        invalid.remaining_attempts += 1;
        assert!(request.validate_forwarded(&invalid, from, next).is_err());
        invalid = forwarded;
        invalid.deadline.0 += Duration::from_secs(1);
        assert!(request.validate_forwarded(&invalid, from, next).is_err());
    }
}

#[cfg(test)]
mod graph_tests {
    use super::*;
    use crate::topology::fixtures::membership;
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
            let mut queue = std::collections::VecDeque::from([source]);
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
    fn rejects_unknown_and_empty_membership() {
        let graph = Graph::new(membership(0));
        assert!(graph.neighbors(&NodeId("unknown".into())).is_err());
        let one = membership(1);
        assert!(
            Graph::new(one.clone())
                .neighbors(&one.members()[0].node)
                .unwrap()
                .is_empty()
        );
    }
}
