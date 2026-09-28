//! Deterministic bounded shortest-path search; no all-pairs table.
use super::{
    RoutingAlgorithm,
    equal_cost::EqualCostSearch,
    graph::neighbor_positions,
    hash,
    health::LinkHealth,
    membership::{Membership, MembershipLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::identity::{AttemptId, NodeId, RequestId},
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

#[derive(Clone, Debug)]
pub struct Route {
    pub membership: MembershipLease,
    pub nodes: Vec<NodeId>,
}
/// Signed forwarding state. Retries preserve deadline and consumed link budget.
#[derive(Clone, Debug)]
pub struct RouteBudget {
    pub membership: crate::model::identity::MembershipVersion,
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
pub(super) struct PathKey {
    membership: usize,
    pub(super) from: usize,
    pub(super) to: usize,
    pub(super) links: u8,
    pub(super) visited: Vec<usize>,
    pub(super) failed: Vec<usize>,
}

#[derive(Default)]
struct PathCache {
    entries: BTreeMap<PathKey, (Weak<Membership>, Vec<Vec<usize>>)>,
    fifo: VecDeque<PathKey>,
}

pub struct Paths {
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
    pub fn link_health(&self) -> Rc<LinkHealth> {
        self.health.clone()
    }
    pub fn new(health: Rc<LinkHealth>, capacity: usize) -> Self {
        Self::with_algorithm(health, capacity, RoutingAlgorithm::V2)
    }
    pub fn with_algorithm(
        health: Rc<LinkHealth>,
        capacity: usize,
        algorithm: RoutingAlgorithm,
    ) -> Self {
        Self {
            algorithm,
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
            return Ok(route);
        }
        let _admission = self.admit_search()?;
        let mut search = RouteSearch::new(membership.members().len(), &key, self.algorithm);
        while !search.step(SEARCH_QUANTUM, budget.deadline)? {}
        let nodes = search.finish()?;
        self.store(&membership, key, &nodes);
        Ok(select_route(membership, &nodes, budget))
    }

    /// Same result as `shortest`, yielding after at most 32 vertex expansions.
    /// Each expansion examines at most 36 edges. Dropping the future cancels it.
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
                return Ok(route);
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
            Ok(select_route(membership, &nodes, budget))
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
        let neighbors: Vec<_> = neighbor_positions(membership.members().len(), source)
            .into_iter()
            .map(|position| membership.members()[position].node.clone())
            .collect();
        self.health.retain_neighbors(&neighbors);
        for neighbor in neighbor_positions(membership.members().len(), source) {
            if !self
                .health
                .available(&membership.members()[neighbor].node)?
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
    ) -> Option<Route> {
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

    pub fn cached_paths(&self) -> usize {
        self.cache.borrow().entries.len()
    }
}

fn select_route(
    membership: MembershipLease,
    alternatives: &[Vec<usize>],
    budget: &RouteBudget,
) -> Route {
    let index = if alternatives.len() == 1 {
        0
    } else {
        let mut digest = hash::domain(b"racer/next-hop/v3\0");
        digest.update(budget.request.0);
        digest.update(budget.attempt.0);
        hash::bytes(
            &mut digest,
            membership.members()[alternatives[0][0]].node.0.as_bytes(),
        );
        hash::bytes(&mut digest, budget.destination.0.as_bytes());
        let sample = u64::from_be_bytes(hash::finish(digest)[..8].try_into().unwrap());
        (sample % alternatives.len() as u64) as usize
    };
    route(membership, &alternatives[index])
}

enum RouteSearch {
    Legacy(Search),
    EqualCost(EqualCostSearch),
}
impl RouteSearch {
    fn new(count: usize, key: &PathKey, algorithm: RoutingAlgorithm) -> Self {
        match algorithm {
            RoutingAlgorithm::V2 => Self::Legacy(Search::new(count, key)),
            RoutingAlgorithm::V3 => Self::EqualCost(EqualCostSearch::new(count, key)),
        }
    }
    fn step(&mut self, quantum: usize, deadline: Deadline) -> Result<bool> {
        match self {
            Self::Legacy(search) => search.step(quantum, deadline),
            Self::EqualCost(search) => search.step(quantum, deadline),
        }
    }
    fn finish(self) -> Result<Vec<Vec<usize>>> {
        match self {
            Self::Legacy(search) => search.finish().map(|route| vec![route]),
            Self::EqualCost(search) => search.finish(),
        }
    }
}

fn route(membership: MembershipLease, positions: &[usize]) -> Route {
    let nodes = positions
        .iter()
        .map(|&index| membership.members()[index].node.clone())
        .collect();
    Route { membership, nodes }
}

struct Wave {
    parents: BTreeMap<usize, usize>,
    queue: VecDeque<usize>,
}

impl Wave {
    fn new(source: usize) -> Self {
        Self {
            parents: BTreeMap::from([(source, source)]),
            queue: VecDeque::from([source]),
        }
    }
}

/// Alternate complete BFS layers, source first. Before each layer the two
/// visited balls are disjoint, so the first intersection has shortest distance
/// (the sum of the prior radii plus one). Sorted neighbors and FIFO discovery
/// choose a deterministic tie without enumerating equivalent paths.
/// Each side visits a node once, to radius ceil(L/2) / floor(L/2), respectively.
/// Scratch storage is O(visited), at most O(N), with no membership-wide scan or
/// initialization. Each expanded vertex examines at most 36 edges.
struct Search {
    count: usize,
    key: PathKey,
    waves: [Wave; 2],
    side: usize,
    layer_remaining: usize,
    layers: u8,
    meeting: Option<usize>,
    #[cfg(test)]
    expansions: usize,
    #[cfg(test)]
    edges: usize,
}

impl Search {
    fn new(count: usize, key: &PathKey) -> Self {
        Self {
            count,
            key: key.clone(),
            waves: [Wave::new(key.from), Wave::new(key.to)],
            side: 0,
            layer_remaining: 1,
            layers: 0,
            meeting: (key.from == key.to).then_some(key.from),
            #[cfg(test)]
            expansions: 0,
            #[cfg(test)]
            edges: 0,
        }
    }

    fn step(&mut self, quantum: usize, deadline: Deadline) -> Result<bool> {
        if crate::runtime::environment::now() >= deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        if self.meeting.is_some() || self.layers == self.key.links {
            return Ok(true);
        }
        for _ in 0..quantum {
            if crate::runtime::environment::now() >= deadline.0 {
                return Err(Error::DeadlineExceeded);
            }
            let Some(node) = self.waves[self.side].queue.pop_front() else {
                return Ok(true);
            };
            #[cfg(test)]
            {
                self.expansions += 1;
            }
            for neighbor in neighbor_positions(self.count, node) {
                #[cfg(test)]
                {
                    self.edges += 1;
                }
                if self.key.visited.binary_search(&neighbor).is_ok()
                    || (node == self.key.from && self.key.failed.binary_search(&neighbor).is_ok())
                    || (neighbor == self.key.from && self.key.failed.binary_search(&node).is_ok())
                {
                    continue;
                }
                let wave = &mut self.waves[self.side];
                if let std::collections::btree_map::Entry::Vacant(entry) =
                    wave.parents.entry(neighbor)
                {
                    entry.insert(node);
                    wave.queue.push_back(neighbor);
                    if self.waves[1 - self.side].parents.contains_key(&neighbor) {
                        self.meeting = Some(neighbor);
                        return Ok(true);
                    }
                }
            }
            self.layer_remaining -= 1;
            if self.layer_remaining == 0 {
                self.layers += 1;
                if self.layers == self.key.links || self.waves[self.side].queue.is_empty() {
                    return Ok(true);
                }
                self.side = 1 - self.side;
                self.layer_remaining = self.waves[self.side].queue.len();
            }
        }
        Ok(false)
    }

    fn finish(self) -> Result<Vec<usize>> {
        let meeting = self.meeting.ok_or(Error::Unavailable)?;
        let mut path = vec![meeting];
        let mut current = meeting;
        while current != self.key.from {
            current = self.waves[0].parents[&current];
            path.push(current);
        }
        path.reverse();
        current = meeting;
        while current != self.key.to {
            current = self.waves[1].parents[&current];
            path.push(current);
        }
        Ok(path)
    }
}
#[cfg(test)]
mod tests {
    use super::*;
    use crate::topology::{fixtures::membership, health::LinkOutcome};
    use std::time::Duration;

    fn budget(membership: &MembershipLease, to: usize, links: u8) -> RouteBudget {
        RouteBudget {
            membership: membership.version,
            request: RequestId([1; 16]),
            attempt: AttemptId([2; 16]),
            destination: membership.members()[to].node.clone(),
            visited: vec![],
            remaining_links: links,
            remaining_attempts: 0,
            deadline: Deadline(Instant::now() + Duration::from_secs(60)),
        }
    }

    #[test]
    fn v3_all_equal_next_hops_match_independent_oracle() {
        for n in [37, 401, 1500] {
            let members = membership(n);
            for source in [0, 19, n - 1] {
                let health = Rc::new(LinkHealth::new(36));
                let paths = Paths::with_algorithm(health, 4, RoutingAlgorithm::V3);
                for to in (0..n).step_by(17).filter(|&to| to != source) {
                    for filtered in [false, true] {
                        let mut request = budget(&members, to, 4);
                        let mut key = paths
                            .key(&members, &members.members()[source].node, &request)
                            .unwrap();
                        if filtered {
                            key.visited = [7, 33]
                                .into_iter()
                                .filter(|&v| v != source && v != to)
                                .collect();
                            key.failed = neighbor_positions(n, source)
                                .into_iter()
                                .step_by(2)
                                .collect();
                            request.visited = key
                                .visited
                                .iter()
                                .map(|&v| members.members()[v].node.clone())
                                .collect();
                        }
                        for links in [1, 2, 4] {
                            key.links = links;
                            let expected = oracle(n, source, to, links, &key.visited, &key.failed);
                            let mut search = EqualCostSearch::new(n, &key);
                            while !search.step(1, request.deadline).unwrap() {}
                            let alternatives = search.finish();
                            let Some(expected) = expected else {
                                assert_eq!(alternatives, Err(Error::Unavailable));
                                continue;
                            };
                            let alternatives = alternatives.unwrap();
                            let firsts: Vec<_> = alternatives.iter().map(|p| p[1]).collect();
                            let mut forbidden = key.visited.clone();
                            forbidden.push(source);
                            let expected_firsts: Vec<_> = neighbor_positions(n, source)
                                .into_iter()
                                .filter(|next| {
                                    !key.failed.contains(next)
                                        && !key.visited.contains(next)
                                        && oracle(n, *next, to, links - 1, &forbidden, &[])
                                            .is_some_and(|p| p.len() + 1 == expected.len())
                                })
                                .collect();
                            assert_eq!(firsts, expected_firsts, "n={n} source={source} to={to}");
                            for positions in &alternatives {
                                assert_route(
                                    &members,
                                    &route(members.clone(), positions).nodes,
                                    &expected,
                                    &key.visited,
                                    &key.failed,
                                );
                            }
                        }
                    }
                }
            }
        }
    }

    #[test]
    fn v3_cache_reselects_identity_and_recomputes_deterministically() {
        assert_eq!(crate::topology::ALGORITHM_VERSION, 3);
        let members = membership(1500);
        let source = &members.members()[0].node;
        let paths = Paths::with_algorithm(Rc::new(LinkHealth), 2, RoutingAlgorithm::V3);
        let cold = Paths::with_algorithm(Rc::new(LinkHealth), 0, RoutingAlgorithm::V3);
        let mut request = budget(&members, 1499, 4);
        let mut nexts = std::collections::BTreeSet::new();
        for id in 0u128..128 {
            request.attempt = AttemptId(id.to_be_bytes());
            let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
            let actual = futures::executor::block_on(paths.shortest_async(
                members.clone(),
                source,
                &request,
                &scope,
            ))
            .unwrap();
            assert_eq!(
                actual.nodes,
                cold.shortest(members.clone(), source, &request)
                    .unwrap()
                    .nodes
            );
            nexts.insert(actual.nodes[1].clone());
            // Independently calculated SHA-256 interoperability vectors.
            let expected = match id {
                0 => Some(833),
                1 => Some(83),
                2 => Some(1166),
                127 => Some(83),
                _ => None,
            };
            if let Some(expected) = expected {
                assert_eq!(actual.nodes[1], members.members()[expected].node);
            }
        }
        assert!(nexts.len() > 1);
        assert_eq!(paths.cached_paths(), 1);
        assert_eq!(Arc::strong_count(&members), 1);
    }

    #[test]
    fn v3_health_cache_eviction_budget_and_deadline_regressions() {
        let members = membership(1500);
        let source = &members.members()[0].node;
        let health = Rc::new(LinkHealth::new(36));
        let paths = Paths::with_algorithm(health.clone(), 1, RoutingAlgorithm::V3);
        let request = budget(&members, 1499, NORMAL_LINKS);
        let original = paths.shortest(members.clone(), source, &request).unwrap();
        let mut blocked = request.clone();
        blocked.visited.push(original.nodes[1].clone());
        let alternate = paths.shortest(members.clone(), source, &blocked).unwrap();
        assert_eq!(alternate.nodes.len(), original.nodes.len());
        assert!(!alternate.nodes.contains(&original.nodes[1]));
        assert_eq!(paths.cached_paths(), 1);
        assert_eq!(
            paths
                .shortest(members.clone(), source, &request)
                .unwrap()
                .nodes,
            original.nodes
        );
        health
            .observe_at(
                &original.nodes[1],
                LinkOutcome::Timeout,
                Instant::now() + Duration::from_secs(60),
            )
            .unwrap();
        let rerouted = paths.shortest(members.clone(), source, &request).unwrap();
        assert_ne!(rerouted.nodes[1], original.nodes[1]);
        assert_eq!(rerouted.nodes.len(), original.nodes.len());
        for neighbor in neighbor_positions(1500, 0) {
            health
                .observe_at(
                    &members.members()[neighbor].node,
                    LinkOutcome::Timeout,
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap();
        }
        assert_eq!(
            paths
                .shortest(members.clone(), source, &request)
                .unwrap_err(),
            Error::Unavailable
        );
        let mut exhausted = request.clone();
        exhausted.remaining_links = 0;
        assert_eq!(
            paths
                .shortest(members.clone(), source, &exhausted)
                .unwrap_err(),
            Error::HopBudgetExhausted
        );
        exhausted = request.clone();
        exhausted.visited.push(source.clone());
        assert_eq!(
            paths
                .shortest(members.clone(), source, &exhausted)
                .unwrap_err(),
            Error::InvalidRequest
        );
        exhausted = request;
        exhausted.deadline = Deadline(Instant::now());
        assert_eq!(
            paths
                .shortest(members.clone(), source, &exhausted)
                .unwrap_err(),
            Error::DeadlineExceeded
        );
        let cold = Paths::with_algorithm(Rc::new(LinkHealth), 0, RoutingAlgorithm::V3);
        assert_eq!(
            cold.shortest(members.clone(), source, &budget(&members, 0, 0))
                .unwrap()
                .nodes,
            vec![source.clone()]
        );
    }

    #[test]
    fn v3_healthy_search_work_is_independent_of_membership_size() {
        let members = membership(100_000);
        let paths = Paths::with_algorithm(Rc::new(LinkHealth), 1, RoutingAlgorithm::V3);
        let request = budget(&members, 80_003, 4);
        let key = paths
            .key(&members, &members.members()[0].node, &request)
            .unwrap();
        let mut search = EqualCostSearch::new(100_000, &key);
        loop {
            let before = search.expansions;
            let done = search.step(7, request.deadline).unwrap();
            assert!(search.expansions - before <= 7);
            assert!(search.expansions <= 74);
            if done {
                break;
            }
        }
        let alternatives = search.finish().unwrap();
        assert!(alternatives.len() <= 36);
        assert!(alternatives.iter().all(|p| p.len() == 5));
        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
        let mut operation = paths.shortest_async(
            members.clone(),
            &members.members()[0].node,
            &request,
            &scope,
        );
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(
            paths
                .shortest(members.clone(), &members.members()[0].node, &request)
                .unwrap_err(),
            Error::Overloaded
        );
        scope.cancel().unwrap();
        assert!(matches!(
            operation.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(operation);
        assert_eq!(paths.active_searches.get(), 0);
        assert_eq!(paths.cached_paths(), 0);
    }

    #[test]
    fn v3_balanced_1500_uid_demand_reduces_peak_nic_without_extra_hops() {
        use super::super::{fixtures::member, membership::Membership};
        let n = 1500;
        let members = Arc::new(
            Membership::validate(
                crate::model::identity::MembershipVersion(1),
                (0..n)
                    .map(|i| {
                        let mut member = member(i, 4);
                        let digest = sha2::Sha256::digest((i as u64).to_be_bytes());
                        let hex: String = digest[..16].iter().map(|b| format!("{b:02x}")).collect();
                        member.node = NodeId(format!(
                            "{}-{}-4{}-8{}-{}",
                            &hex[..8],
                            &hex[8..12],
                            &hex[13..16],
                            &hex[17..20],
                            &hex[20..32]
                        ));
                        member
                    })
                    .collect(),
            )
            .unwrap(),
        );
        let adjacency = oracle_graph(n);
        let mut loads = [
            [vec![0usize; n], vec![0usize; n]],
            [vec![0usize; n], vec![0usize; n]],
        ];
        let mut links = [0usize; 2];
        let started = Instant::now();
        for source in 0..n {
            let mut distance = vec![u8::MAX; n];
            distance[source] = 0;
            let mut queue = VecDeque::from([source]);
            while let Some(node) = queue.pop_front() {
                for &next in &adjacency[node] {
                    if distance[next] == u8::MAX {
                        distance[next] = distance[node] + 1;
                        queue.push_back(next);
                    }
                }
            }
            // Coprime stride: each source and each owner receives exactly 96
            // equal-sized transfers. IDs are UID-like, not numeric fixture names.
            for sample in 1..=96 {
                let destination = (source + sample * 739) % n;
                for (version, algorithm) in [RoutingAlgorithm::V2, RoutingAlgorithm::V3]
                    .into_iter()
                    .enumerate()
                {
                    let paths = Paths::with_algorithm(Rc::new(LinkHealth), 4, algorithm);
                    let mut request = budget(&members, destination, NORMAL_LINKS);
                    request.request = RequestId(((source * n + destination) as u128).to_be_bytes());
                    request.attempt = AttemptId((sample as u128).to_be_bytes());
                    let mut current = source;
                    let mut length = 0;
                    while current != destination {
                        let from = &members.members()[current].node;
                        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
                        let route = futures::executor::block_on(paths.shortest_async(
                            members.clone(),
                            from,
                            &request,
                            &scope,
                        ))
                        .unwrap();
                        let next = members.position(&route.nodes[1]).unwrap();
                        // Payload returns toward requester: next transmits,
                        // current receives. Endpoints count toward NIC load.
                        loads[version][0][next] += 1;
                        loads[version][1][current] += 1;
                        request = request.forwarded(from, &route.nodes[1]).unwrap();
                        current = next;
                        length += 1;
                    }
                    assert_eq!(length, distance[destination] as usize);
                    assert!(length <= 4);
                    links[version] += length;
                }
            }
        }
        assert_eq!(links[0], links[1]);
        for direction in 0..2 {
            let peak = |version: usize| {
                let load = &loads[version][direction];
                *load.iter().max().unwrap() as f64 / (load.iter().sum::<usize>() as f64 / n as f64)
            };
            eprintln!(
                "1500 UID nodes, 144000 balanced flows, direction={direction}: v2 peak/mean={:.4}, v3={:.4}, mean links={:.6}, elapsed={:?}",
                peak(0),
                peak(1),
                links[1] as f64 / 144000.0,
                started.elapsed()
            );
            assert!(peak(0) > 3.5);
            assert!(peak(1) < 1.4);
        }
    }

    fn oracle(
        n: usize,
        source: usize,
        to: usize,
        links: u8,
        forbidden: &[usize],
        failed: &[usize],
    ) -> Option<Vec<usize>> {
        // Independent edge construction from the graph definition, not the
        // production inverse-neighbor enumeration or bidirectional search.
        let adjacency = oracle_graph(n);
        let mut queue = VecDeque::from([vec![source]]);
        let mut seen = vec![false; n];
        seen[source] = true;
        while let Some(path) = queue.pop_front() {
            let current = *path.last().unwrap();
            if current == to {
                return Some(path);
            }
            if path.len() > usize::from(links) {
                continue;
            }
            for &next in &adjacency[current] {
                if seen[next]
                    || forbidden.contains(&next)
                    || (current == source && failed.contains(&next))
                {
                    continue;
                }
                seen[next] = true;
                let mut extension = path.clone();
                extension.push(next);
                queue.push_back(extension);
            }
        }
        None
    }

    fn oracle_graph(n: usize) -> Vec<Vec<usize>> {
        let mut adjacency = vec![vec![]; n];
        for node in 0..n {
            for digit in 0..18 {
                let next = (18 * node + digit) % n;
                if next != node {
                    adjacency[node].push(next);
                    adjacency[next].push(node);
                }
            }
        }
        for neighbors in &mut adjacency {
            neighbors.sort_unstable();
            neighbors.dedup();
        }
        adjacency
    }

    fn assert_route(
        members: &MembershipLease,
        nodes: &[NodeId],
        expected: &[usize],
        forbidden: &[usize],
        failed: &[usize],
    ) {
        let positions: Vec<_> = nodes
            .iter()
            .map(|node| members.position(node).unwrap())
            .collect();
        assert_eq!(positions.len(), expected.len());
        assert_eq!(positions.first(), expected.first());
        assert_eq!(positions.last(), expected.last());
        for (i, node) in positions.iter().enumerate() {
            assert!(!forbidden.contains(node));
            assert!(!positions[..i].contains(node));
        }
        for pair in positions.windows(2) {
            assert!((0..18).any(|digit| {
                (18 * pair[0] + digit) % members.members().len() == pair[1]
                    || (18 * pair[1] + digit) % members.members().len() == pair[0]
            }));
            assert!(!(pair[0] == positions[0] && failed.contains(&pair[1])));
            assert!(!(pair[1] == positions[0] && failed.contains(&pair[0])));
        }
    }

    #[test]
    fn exhaustive_small_shortest_paths_and_deterministic_ties() {
        for n in 1..=42 {
            let members = membership(n);
            let paths = Paths::new(Rc::new(LinkHealth), 16);
            for from in 0..n {
                for to in 0..n {
                    let expected = oracle(n, from, to, 4, &[], &[]).unwrap();
                    let actual = paths
                        .shortest(
                            members.clone(),
                            &members.members()[from].node,
                            &budget(&members, to, 4),
                        )
                        .unwrap();
                    assert_route(&members, &actual.nodes, &expected, &[], &[]);
                }
            }
            assert!(paths.cached_paths() <= 16);
        }
        // Non-complete graphs exercise multi-hop suffix/prefix ties.
        for n in [63, 100, 321, 1000] {
            let members = membership(n);
            let paths = Paths::new(Rc::new(LinkHealth), 0);
            for from in [0, n / 2, n - 1] {
                for to in 0..n {
                    let expected = oracle(n, from, to, 4, &[], &[]).unwrap();
                    let actual = paths
                        .shortest(
                            members.clone(),
                            &members.members()[from].node,
                            &budget(&members, to, 4),
                        )
                        .unwrap();
                    assert_route(&members, &actual.nodes, &expected, &[], &[]);
                    let request = budget(&members, to, 4);
                    let key = paths
                        .key(&members, &members.members()[from].node, &request)
                        .unwrap();
                    let mut search = Search::new(n, &key);
                    while !search.step(1, request.deadline).unwrap() {}
                    assert_eq!(
                        actual.nodes,
                        route(members.clone(), &search.finish().unwrap()).nodes
                    );
                }
            }
        }
    }

    #[test]
    fn algorithm_v2_route_vectors() {
        for (n, from, to, expected) in [
            (1000, 0, 999, vec![0, 55, 999]),
            (100_000, 0, 80_003, vec![0, 13, 246, 4444, 80_003]),
            (401, 37, 309, vec![37, 358, 309]),
        ] {
            let members = membership(n);
            let paths = Paths::new(Rc::new(LinkHealth), 0);
            let request = budget(&members, to, NORMAL_LINKS);
            let key = paths
                .key(&members, &members.members()[from].node, &request)
                .unwrap();
            for quantum in [1, SEARCH_QUANTUM, 256] {
                let mut search = Search::new(n, &key);
                while !search.step(quantum, request.deadline).unwrap() {}
                assert_eq!(search.finish().unwrap(), expected);
            }
        }
    }

    #[test]
    fn failures_are_local_edges_not_global_node_exclusions() {
        let members = membership(1000);
        let health = Rc::new(LinkHealth::new(36));
        let paths = Paths::new(health.clone(), 4);
        let from = &members.members()[19].node;
        let neighbors = neighbor_positions(1000, 19);
        let to = neighbors[0];
        let request = budget(&members, to, 8);
        let direct = paths.shortest(members.clone(), from, &request).unwrap();
        assert_eq!(direct.nodes.len(), 2);
        health
            .observe(&members.members()[to].node, LinkOutcome::Timeout)
            .unwrap();
        let alternate = paths.shortest(members.clone(), from, &request).unwrap();
        assert!(alternate.nodes.len() > 2);
        assert_eq!(alternate.nodes.last(), Some(&members.members()[to].node));
        for neighbor in neighbors {
            health
                .observe(&members.members()[neighbor].node, LinkOutcome::Timeout)
                .unwrap();
        }
        assert_eq!(
            paths.shortest(members.clone(), from, &request).unwrap_err(),
            Error::Unavailable
        );
        let isolated = Paths::new(Rc::new(LinkHealth), 1);
        assert_eq!(
            isolated
                .shortest(members.clone(), from, &request)
                .unwrap()
                .nodes,
            direct.nodes
        );
    }

    #[test]
    fn inherited_budget_loops_deadlines_and_memberships() {
        let members = membership(50);
        let paths = Paths::new(Rc::new(LinkHealth), 2);
        let from = &members.members()[0].node;
        let next = &members.members()[1].node;
        let original = budget(&members, 30, 4);
        let mut hop = original.forwarded(from, next).unwrap();
        original.validate_forwarded(&hop, from, next).unwrap();
        assert_eq!(hop.remaining_links, 3);
        assert_eq!(hop.deadline.0, original.deadline.0);
        hop.remaining_links = 4;
        assert_eq!(
            original.validate_forwarded(&hop, from, next),
            Err(Error::InvalidRequest)
        );
        hop.remaining_links = 3;
        hop.deadline.0 += Duration::from_secs(1);
        assert_eq!(
            original.validate_forwarded(&hop, from, next),
            Err(Error::InvalidRequest)
        );
        assert_eq!(
            original.forwarded(from, from).unwrap_err(),
            Error::InvalidRequest
        );
        let mut bad = original.clone();
        bad.visited = vec![from.clone()];
        assert_eq!(
            paths.shortest(members.clone(), from, &bad).unwrap_err(),
            Error::InvalidRequest
        );
        bad = original.clone();
        bad.membership.0 += 1;
        assert_eq!(
            paths.shortest(members.clone(), from, &bad).unwrap_err(),
            Error::IncompatibleMembership
        );
        bad = original.clone();
        bad.remaining_links = 0;
        assert_eq!(
            paths.shortest(members.clone(), from, &bad).unwrap_err(),
            Error::HopBudgetExhausted
        );
        bad = original.clone();
        bad.remaining_links = 9;
        assert_eq!(
            paths.shortest(members.clone(), from, &bad).unwrap_err(),
            Error::HopBudgetExhausted
        );
        bad = original.clone();
        bad.deadline = Deadline(Instant::now());
        assert_eq!(
            paths.shortest(members.clone(), from, &bad).unwrap_err(),
            Error::DeadlineExceeded
        );
        let mut remaining = original.forwarded(from, next).unwrap();
        remaining.visited.push(members.members()[2].node.clone());
        remaining.remaining_links -= 1;
        let route = paths.shortest(members.clone(), next, &remaining).unwrap();
        assert!(!route.nodes.contains(from));
        assert!(!route.nodes.contains(&members.members()[2].node));
        assert!(route.nodes.len() <= usize::from(remaining.remaining_links) + 1);
        assert_eq!(remaining.deadline.0, original.deadline.0);
    }

    #[test]
    fn forwarding_preserves_or_spends_attempt_credits_without_refill() {
        let members = membership(4);
        let from = &members.members()[0].node;
        let next = &members.members()[1].node;
        let mut original = budget(&members, 3, 4);
        original.remaining_attempts = u32::MAX;
        let mut forwarded = original.forwarded(from, next).unwrap();
        assert_eq!(forwarded.remaining_attempts, u32::MAX);
        original.validate_forwarded(&forwarded, from, next).unwrap();
        forwarded.remaining_attempts = 2;
        original.validate_forwarded(&forwarded, from, next).unwrap();
        original.remaining_attempts = 1;
        assert_eq!(
            original.validate_forwarded(&forwarded, from, next),
            Err(Error::InvalidRequest)
        );
        original.remaining_attempts = 0;
        forwarded.remaining_attempts = 0;
        original.validate_forwarded(&forwarded, from, next).unwrap();
        forwarded.remaining_attempts = 1;
        assert_eq!(
            original.validate_forwarded(&forwarded, from, next),
            Err(Error::InvalidRequest)
        );
    }

    #[test]
    fn hundred_thousand_nodes_work_bound_yield_and_cache_leases() {
        use std::task::Context;
        let members = membership(100_000);
        let paths = Paths::new(Rc::new(LinkHealth), 2);
        for (from, to) in [(0, 99_999), (50_000, 17), (17, 80_003)] {
            let budget = budget(&members, to, 4);
            let scope = RequestScope::new(budget.request, budget.deadline.0).unwrap();
            let source = &members.members()[from].node;
            let mut operation = paths.shortest_async(members.clone(), source, &budget, &scope);
            let mut context = Context::from_waker(futures::task::noop_waker_ref());
            let route = match operation.as_mut().poll(&mut context) {
                Poll::Ready(result) => result.unwrap(),
                Poll::Pending => futures::executor::block_on(operation).unwrap(),
            };
            assert!(route.nodes.len() <= 5);
            for pair in route.nodes.windows(2) {
                let left = members.position(&pair[0]).unwrap();
                let right = members.position(&pair[1]).unwrap();
                assert!(neighbor_positions(100_000, left).contains(&right));
            }
            assert_eq!(
                route.nodes,
                paths
                    .shortest(members.clone(), source, &budget)
                    .unwrap()
                    .nodes
            );
        }
        assert_eq!(paths.cached_paths(), 2);
        assert_eq!(Arc::strong_count(&members), 1);
        // One cold search is admitted even with caching disabled. Work is
        // bounded by explored vertices rather than a total-work allowance.
        let bounded = Paths::new(Rc::new(LinkHealth), 0);
        assert!(
            bounded
                .shortest(
                    members.clone(),
                    &members.members()[0].node,
                    &budget(&members, 99_999, 4)
                )
                .is_ok()
        );
        assert_eq!(bounded.cached_paths(), 0);
    }

    #[test]
    fn filtered_paths_match_oracle_and_bound_no_path_results() {
        let n = 401;
        let members = membership(n);
        for source in [0, 37, 211, 400] {
            let health = Rc::new(LinkHealth::new(36));
            let failed: Vec<_> = neighbor_positions(n, source)
                .into_iter()
                .step_by(2)
                .collect();
            for &neighbor in &failed {
                health
                    .observe_at(
                        &members.members()[neighbor].node,
                        LinkOutcome::Timeout,
                        Instant::now() + Duration::from_secs(60),
                    )
                    .unwrap();
            }
            let paths = Paths::new(health, 2);
            for destination in (0..n).step_by(7).filter(|to| *to != source) {
                let forbidden: Vec<_> = [5, 18, 309]
                    .into_iter()
                    .filter(|i| *i != source && *i != destination)
                    .collect();
                for links in [1, 2, 3, 4, 5] {
                    let mut request = budget(&members, destination, links);
                    request.visited = forbidden
                        .iter()
                        .map(|&i| members.members()[i].node.clone())
                        .collect();
                    let actual =
                        paths.shortest(members.clone(), &members.members()[source].node, &request);
                    match oracle(n, source, destination, links, &forbidden, &failed) {
                        Some(expected) => assert_route(
                            &members,
                            &actual.unwrap().nodes,
                            &expected,
                            &forbidden,
                            &failed,
                        ),
                        None => assert_eq!(actual.unwrap_err(), Error::Unavailable),
                    }
                }
            }
        }
    }

    #[test]
    fn cooperative_search_admission_cancellation_and_deadline() {
        use std::task::Context;
        let members = membership(100_000);
        let health = Rc::new(LinkHealth::new(36));
        for neighbor in neighbor_positions(100_000, 0)
            .into_iter()
            .filter(|&n| n != 1)
        {
            health
                .observe_at(
                    &members.members()[neighbor].node,
                    LinkOutcome::Timeout,
                    Instant::now() + Duration::from_secs(60),
                )
                .unwrap();
        }
        let paths = Paths::new(health, 1);
        let source = &members.members()[0].node;
        let request = budget(&members, 80_003, 8);
        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
        let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
        let mut context = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut context).is_pending());
        assert_eq!(
            paths
                .shortest(members.clone(), source, &request)
                .unwrap_err(),
            Error::Overloaded
        );
        drop(operation);
        assert_eq!(paths.active_searches.get(), 0);
        let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
        assert!(operation.as_mut().poll(&mut context).is_pending());
        scope.cancel().unwrap();
        assert!(matches!(
            operation.as_mut().poll(&mut context),
            Poll::Ready(Err(Error::Cancelled))
        ));
        drop(operation);
        assert_eq!(paths.active_searches.get(), 0);
        assert_eq!(paths.cached_paths(), 0);

        let clock = crate::runtime::environment::SimulationClock::new(1);
        let environment = clock.environment(0);
        let _guard = environment.enter();
        let scope = RequestScope::new(
            request.request,
            crate::runtime::environment::now() + Duration::from_secs(1),
        )
        .unwrap();
        let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
        assert!(operation.as_mut().poll(&mut context).is_pending());
        clock.advance(Duration::from_secs(1));
        assert!(matches!(
            operation.as_mut().poll(&mut context),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        drop(operation);
        assert_eq!(paths.active_searches.get(), 0);
        assert_eq!(paths.cached_paths(), 0);
        drop(_guard);
        assert!(paths.shortest(members.clone(), source, &request).is_ok());
        let key = paths.key(&members, source, &request).unwrap();
        let mut search = Search::new(members.members().len(), &key);
        assert!(!search.step(1, request.deadline).unwrap());
        assert_eq!(
            search.step(1, Deadline(Instant::now())),
            Err(Error::DeadlineExceeded)
        );
    }

    #[test]
    fn default_hundred_thousand_member_routes_at_every_distance_are_bounded() {
        let config = crate::config::Config::from_lookup_with_fabric_ports(|name| {
            Ok(match name {
                "RACER_CLUSTER_ID" => Some("00000000-0000-4000-8000-000000000001".into()),
                "RACER_CONTROL_ENDPOINT" => Some("https://control.example:8443".into()),
                _ => None,
            })
        })
        .unwrap()
        .0;
        let n = 100_000;
        let members = membership(n);
        let paths = Paths::new(Rc::new(LinkHealth), config.limits.cached_paths.get());
        let adjacency = oracle_graph(n);
        for source in [0, 1, 17, 49_999, 99_999] {
            let mut distances = vec![u8::MAX; n];
            distances[source] = 0;
            let mut queue = VecDeque::from([source]);
            while let Some(node) = queue.pop_front() {
                for &next in &adjacency[node] {
                    if distances[next] == u8::MAX {
                        distances[next] = distances[node] + 1;
                        queue.push_back(next);
                    }
                }
            }
            for distance in 0..=NORMAL_LINKS {
                // Both ends of each distance bucket catch position-order effects.
                let first = distances.iter().position(|&d| d == distance).unwrap();
                let last = distances.iter().rposition(|&d| d == distance).unwrap();
                for destination in [first, last] {
                    for links in [NORMAL_LINKS, FAILURE_LINKS] {
                        let request = budget(&members, destination, links);
                        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
                        let from = &members.members()[source].node;
                        let key = paths.key(&members, from, &request).unwrap();
                        let mut search = Search::new(n, &key);
                        assert_eq!(
                            search.waves.iter().map(|w| w.parents.len()).sum::<usize>(),
                            2
                        );
                        loop {
                            let before = search.expansions;
                            let done = search.step(7, request.deadline).unwrap();
                            assert!(search.expansions - before <= 7);
                            assert!(search.edges <= 36 * search.expansions);
                            // Healthy distance <=4: only the two radius-one
                            // balls can be expanded; radius-two nodes are stored.
                            assert!(search.expansions <= 2 * (1 + 36));
                            for wave in &search.waves {
                                assert!(wave.parents.len() <= 1 + 36 + 36 * 35);
                                assert!(wave.queue.len() <= wave.parents.len());
                            }
                            if done {
                                break;
                            }
                        }
                        let expected = search.finish().unwrap();
                        assert_eq!(expected.len(), usize::from(distance) + 1);
                        let actual = futures::executor::block_on(paths.shortest_async(
                            members.clone(),
                            from,
                            &request,
                            &scope,
                        ))
                        .unwrap();
                        assert_route(&members, &actual.nodes, &expected, &[], &[]);
                        assert_eq!(actual.nodes, route(members.clone(), &expected).nodes);
                    }
                }
            }
        }
        assert!(paths.cached_paths() <= config.limits.cached_paths.get());
        assert_eq!(paths.active_searches.get(), 0);
        assert_eq!(Arc::strong_count(&members), 1);
    }

    #[test]
    fn unreachable_searches_yield_and_bound_saturation_and_cancellation() {
        use std::task::Context;
        let n = 100_000;
        let members = membership(n);
        // Isolate the destination with failed nodes in an internal fixture.
        // Exhausting either component must stop even with eight links left.
        let paths = Paths::new(Rc::new(LinkHealth), 128);
        let source = &members.members()[80_003].node;
        let request = budget(&members, 0, FAILURE_LINKS);
        let mut key = paths.key(&members, source, &request).unwrap();
        key.visited = neighbor_positions(n, 0);
        let mut search = Search::new(n, &key);
        while !search.step(1, request.deadline).unwrap() {
            assert!(search.expansions <= 2 * n);
            assert!(search.edges <= 36 * search.expansions);
            assert!(search.waves.iter().all(|w| w.parents.len() <= n));
        }
        assert_eq!(search.finish(), Err(Error::Unavailable));

        // A three-link hop bound forces a full unsuccessful search for a
        // distance-four pair. Use quantum one directly to check interruption.
        let mut request = budget(&members, 80_003, 3);
        let source = &members.members()[0].node;
        let key = paths.key(&members, source, &request).unwrap();
        let mut search = Search::new(n, &key);
        assert!(!search.step(1, request.deadline).unwrap());
        assert_eq!(
            search.step(1, Deadline(Instant::now())),
            Err(Error::DeadlineExceeded)
        );
        while !search.step(1, request.deadline).unwrap() {}
        assert_eq!(search.finish(), Err(Error::Unavailable));

        // Saturate the maximum admission, even with a larger cache capacity.
        let mut context = Context::from_waker(futures::task::noop_waker_ref());
        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
        let mut operations: Vec<_> = (0..8)
            .map(|_| paths.shortest_async(members.clone(), source, &request, &scope))
            .collect();
        for operation in &mut operations {
            assert!(operation.as_mut().poll(&mut context).is_pending());
        }
        assert_eq!(paths.active_searches.get(), 8);
        let mut denied = paths.shortest_async(members.clone(), source, &request, &scope);
        assert!(matches!(
            denied.as_mut().poll(&mut context),
            Poll::Ready(Err(Error::Overloaded))
        ));
        drop(denied);
        drop(operations);
        assert_eq!(paths.active_searches.get(), 0);
        request.remaining_links = NORMAL_LINKS;
        assert!(
            futures::executor::block_on(paths.shortest_async(
                members.clone(),
                source,
                &request,
                &scope
            ))
            .is_ok()
        );
    }
}
