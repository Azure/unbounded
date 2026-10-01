//! Versioned graph neighbors and deterministic bounded shortest-path routing.
//! Search caches eligible alternatives, then selects a next hop per request.
use super::{
    MAX_DEGREE, RoutingAlgorithm, hash,
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
    algorithm: RoutingAlgorithm,
}
impl Graph {
    pub fn new(membership: MembershipLease) -> Self {
        Self::with_algorithm(membership, RoutingAlgorithm::default())
    }
    pub fn with_algorithm(membership: MembershipLease, algorithm: RoutingAlgorithm) -> Self {
        Self {
            membership,
            algorithm,
        }
    }
    pub fn neighbors(&self, node: &NodeId) -> Result<Vec<NodeId>> {
        let position = self.membership.position(node)?;
        Ok(
            neighbor_positions_for(self.membership.members().len(), position, self.algorithm)
                .into_iter()
                .map(|index| self.membership.members()[index].node.clone())
                .collect(),
        )
    }
}

/// The incoming intervals are disjoint lifts of `node` modulo N. Their quotient
/// by the radix gives every predecessor without scanning any other member.
fn neighbor_positions_for(count: usize, node: usize, algorithm: RoutingAlgorithm) -> Vec<usize> {
    debug_assert!(node < count);
    let radix = algorithm.radix();
    let mut neighbors = Vec::with_capacity(algorithm.max_degree());
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
    algorithm: RoutingAlgorithm,
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
        Self::with_algorithm(health, capacity, RoutingAlgorithm::default())
    }
    pub fn with_algorithm(
        health: Rc<LinkHealth>,
        capacity: usize,
        algorithm: RoutingAlgorithm,
    ) -> Self {
        Self {
            algorithm,
            peer_admission: None,
            health,
            capacity,
            cache: RefCell::new(PathCache::default()),
            active_searches: Cell::new(0),
        }
    }
    pub fn shortest(
        &self,
        membership: MembershipLease,
        from: &NodeId,
        budget: &RouteBudget,
    ) -> Result<Route> {
        let key = self.key(&membership, from, budget)?;
        if let Some(route) = self.cached(&membership, &key, budget) {
            return route;
        }
        let _admission = self.admit_search()?;
        let mut search = RouteSearch::new(membership.members().len(), &key, self.algorithm);
        while !search.step(SEARCH_QUANTUM, budget.deadline)? {}
        let nodes = search.finish()?;
        self.store(&membership, key, &nodes);
        select_route(membership, &nodes, budget, self.algorithm)
    }

    /// Same result as `shortest`, yielding after at most 32 vertex expansions.
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
            let mut search = RouteSearch::new(membership.members().len(), &key, self.algorithm);
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
            select_route(membership, &nodes, budget, self.algorithm)
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
        let neighbors: Vec<_> =
            neighbor_positions_for(membership.members().len(), source, self.algorithm)
                .into_iter()
                .map(|position| membership.members()[position].node.clone())
                .collect();
        self.health.retain_neighbors(&neighbors);
        for neighbor in neighbor_positions_for(membership.members().len(), source, self.algorithm) {
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
        Some(select_route(
            membership.clone(),
            nodes,
            budget,
            self.algorithm,
        ))
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
    _algorithm: RoutingAlgorithm,
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

type RouteSearch = EqualCostSearch;

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
    algorithm: RoutingAlgorithm,
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

    fn new(count: usize, key: &PathKey, algorithm: RoutingAlgorithm) -> Self {
        let neighbors = neighbor_positions_for(count, key.from, algorithm);
        assert!(neighbors.len() <= MAX_DEGREE);
        Self {
            count,
            algorithm,
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
            for next in neighbor_positions_for(self.count, node, self.algorithm) {
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
                    neighbor_positions_for(self.count, current, self.algorithm)
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
mod scenarios;

#[cfg(test)]
mod graph_tests;
