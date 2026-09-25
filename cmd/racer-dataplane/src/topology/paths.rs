//! Deterministic bounded shortest-path search; no all-pairs table.
use super::{
    graph::neighbor_positions,
    health::LinkHealth,
    membership::{Membership, MembershipLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::identity::{AttemptId, NodeId, RequestId},
    runtime::deadline::Deadline,
};
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

impl RouteBudget {
    /// `visited` contains prior senders, excluding the current recipient.
    pub fn forwarded(&self, from: &NodeId, next: &NodeId) -> Result<Self> {
        self.validate_at(from, Instant::now())?;
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
        forwarded.validate_at(next, Instant::now())
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
    entries: BTreeMap<PathKey, (Weak<Membership>, Vec<usize>)>,
    fifo: VecDeque<PathKey>,
}

pub struct Paths {
    health: Rc<LinkHealth>,
    capacity: usize,
    search_work: usize,
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
    pub fn new(health: Rc<LinkHealth>, capacity: usize, search_work: usize) -> Self {
        Self {
            health,
            capacity,
            search_work,
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
        if let Some(route) = self.cached(&membership, &key) {
            return Ok(route);
        }
        let _admission = self.admit_search()?;
        let mut search = Search::new(membership.members().len(), &key, self.search_work);
        while !search.step(256, budget.deadline)? {}
        let nodes = search.finish()?;
        self.store(&membership, key, &nodes);
        Ok(route(membership, &nodes))
    }

    /// Same result as `shortest`, yielding after at most 256 vertex expansions
    /// or candidate comparisons. Each expansion examines at most 36 edges.
    pub fn shortest_async<'a>(
        &'a self,
        membership: MembershipLease,
        from: &'a NodeId,
        budget: &'a RouteBudget,
    ) -> Operation<'a, Route> {
        Box::pin(async move {
            let key = self.key(&membership, from, budget)?;
            if let Some(route) = self.cached(&membership, &key) {
                return Ok(route);
            }
            let _admission = self.admit_search()?;
            let mut search = Search::new(membership.members().len(), &key, self.search_work);
            std::future::poll_fn(|cx| match search.step(256, budget.deadline) {
                Ok(true) => Poll::Ready(Ok(())),
                Err(error) => Poll::Ready(Err(error)),
                Ok(false) => {
                    cx.waker().wake_by_ref();
                    Poll::Pending
                }
            })
            .await?;
            let nodes = search.finish()?;
            // Health may have changed while yielded: retry under the same budget.
            if self.key(&membership, from, budget)? != key {
                return Err(Error::Unavailable);
            }
            self.store(&membership, key, &nodes);
            Ok(route(membership, &nodes))
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
        budget.validate_at(from, Instant::now())?;
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

    fn cached(&self, membership: &MembershipLease, key: &PathKey) -> Option<Route> {
        let cache = self.cache.borrow();
        let (weak, nodes) = cache.entries.get(key)?;
        weak.upgrade()?;
        Some(route(membership.clone(), nodes))
    }

    fn store(&self, membership: &MembershipLease, key: PathKey, nodes: &[usize]) {
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

fn route(membership: MembershipLease, positions: &[usize]) -> Route {
    let nodes = positions
        .iter()
        .map(|&index| membership.members()[index].node.clone())
        .collect();
    Route { membership, nodes }
}

struct Wave {
    distances: Vec<u8>,
    parents: Vec<usize>,
    queue: VecDeque<usize>,
    radius: u8,
}

impl Wave {
    fn new(count: usize, source: usize, radius: u8) -> Self {
        let mut distances = vec![u8::MAX; count];
        distances[source] = 0;
        Self {
            distances,
            parents: vec![usize::MAX; count],
            queue: VecDeque::from([source]),
            radius,
        }
    }
}

/// Bidirectional BFS explores balls of radius floor(L/2), ceil(L/2).
/// Memory is O(N); work counts edge examinations plus meeting-node comparisons.
struct Search {
    key: PathKey,
    waves: [Wave; 2],
    side: usize,
    scan: usize,
    work: usize,
    best: Option<Vec<usize>>,
}

impl Search {
    fn new(count: usize, key: &PathKey, work: usize) -> Self {
        Self {
            key: key.clone(),
            waves: [
                Wave::new(count, key.from, key.links / 2),
                Wave::new(count, key.to, key.links - key.links / 2),
            ],
            side: 0,
            scan: 0,
            work,
            best: None,
        }
    }

    fn spend(&mut self) -> Result<()> {
        self.work = self.work.checked_sub(1).ok_or(Error::Overloaded)?;
        Ok(())
    }

    fn step(&mut self, quantum: usize, deadline: Deadline) -> Result<bool> {
        if Instant::now() >= deadline.0 {
            return Err(Error::DeadlineExceeded);
        }
        if self.key.from == self.key.to {
            self.best = Some(vec![self.key.from]);
            return Ok(true);
        }
        for _ in 0..quantum {
            if self.side < 2 {
                let Some(node) = self.waves[self.side].queue.pop_front() else {
                    self.side += 1;
                    continue;
                };
                let depth = self.waves[self.side].distances[node];
                if depth == self.waves[self.side].radius {
                    continue;
                }
                for neighbor in neighbor_positions(self.waves[0].distances.len(), node) {
                    self.spend()?;
                    if self.key.visited.binary_search(&neighbor).is_ok()
                        || (node == self.key.from
                            && self.key.failed.binary_search(&neighbor).is_ok())
                        || (neighbor == self.key.from
                            && self.key.failed.binary_search(&node).is_ok())
                    {
                        continue;
                    }
                    let wave = &mut self.waves[self.side];
                    if wave.distances[neighbor] == u8::MAX {
                        wave.distances[neighbor] = depth + 1;
                        wave.parents[neighbor] = node;
                        wave.queue.push_back(neighbor);
                    } else if self.side == 1
                        && wave.distances[neighbor] == depth + 1
                        && node < wave.parents[neighbor]
                    {
                        // Reverse suffix tie: smallest next hop toward destination.
                        wave.parents[neighbor] = node;
                    }
                }
            } else {
                if self.scan == self.waves[0].distances.len() {
                    return Ok(true);
                }
                self.spend()?;
                let meeting = self.scan;
                self.scan += 1;
                let left = self.waves[0].distances[meeting];
                let right = self.waves[1].distances[meeting];
                if left == u8::MAX || right == u8::MAX || left + right > self.key.links {
                    continue;
                }
                if self
                    .best
                    .as_ref()
                    .is_some_and(|best| best.len() < usize::from(left + right) + 1)
                {
                    continue;
                }
                let mut path = vec![meeting];
                let mut current = meeting;
                while current != self.key.from {
                    current = self.waves[0].parents[current];
                    path.push(current);
                }
                path.reverse();
                current = meeting;
                while current != self.key.to {
                    current = self.waves[1].parents[current];
                    path.push(current);
                }
                if self
                    .best
                    .as_ref()
                    .is_none_or(|best| (path.len(), &path) < (best.len(), best))
                {
                    self.best = Some(path);
                }
            }
        }
        Ok(false)
    }

    fn finish(self) -> Result<Vec<usize>> {
        self.best.ok_or(Error::Unavailable)
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

    fn oracle(
        n: usize,
        source: usize,
        to: usize,
        links: u8,
        forbidden: &[usize],
        failed: &[usize],
    ) -> Option<Vec<usize>> {
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
            for next in neighbor_positions(n, current) {
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

    #[test]
    fn exhaustive_small_shortest_paths_and_lexicographic_ties() {
        for n in 1..=42 {
            let members = membership(n);
            let paths = Paths::new(Rc::new(LinkHealth), 16, 100_000);
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
                    assert_eq!(
                        actual.nodes,
                        expected
                            .iter()
                            .map(|&i| members.members()[i].node.clone())
                            .collect::<Vec<_>>(),
                        "n={n}, from={from}, to={to}"
                    );
                }
            }
            assert!(paths.cached_paths() <= 16);
        }
        // Non-complete graphs exercise multi-hop suffix/prefix ties.
        for n in [63, 100, 321, 1000] {
            let members = membership(n);
            let paths = Paths::new(Rc::new(LinkHealth), 0, 100_000);
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
                    assert_eq!(
                        actual.nodes,
                        expected
                            .iter()
                            .map(|&i| members.members()[i].node.clone())
                            .collect::<Vec<_>>()
                    );
                }
            }
        }
    }

    #[test]
    fn failures_are_local_edges_not_global_node_exclusions() {
        let members = membership(1000);
        let health = Rc::new(LinkHealth::new(36));
        let paths = Paths::new(health.clone(), 4, 100_000);
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
        let isolated = Paths::new(Rc::new(LinkHealth), 1, 100_000);
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
        let paths = Paths::new(Rc::new(LinkHealth), 2, 100_000);
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
        let paths = Paths::new(Rc::new(LinkHealth), 2, 150_000);
        for (from, to) in [(0, 99_999), (50_000, 17), (17, 80_003)] {
            let budget = budget(&members, to, 4);
            let source = &members.members()[from].node;
            let mut operation = paths.shortest_async(members.clone(), source, &budget);
            let mut context = Context::from_waker(futures::task::noop_waker_ref());
            assert!(operation.as_mut().poll(&mut context).is_pending());
            let route = futures::executor::block_on(operation).unwrap();
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
        let bounded = Paths::new(Rc::new(LinkHealth), 1, 1);
        assert_eq!(
            bounded
                .shortest(
                    members.clone(),
                    &members.members()[0].node,
                    &budget(&members, 99_999, 4)
                )
                .unwrap_err(),
            Error::Overloaded
        );
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
            let paths = Paths::new(health, 2, 100_000);
            for destination in (0..n).step_by(7).filter(|to| *to != source) {
                let forbidden: Vec<_> = [5, 18, 309]
                    .into_iter()
                    .filter(|i| *i != source && *i != destination)
                    .collect();
                for links in [1, 2, 4] {
                    let mut request = budget(&members, destination, links);
                    request.visited = forbidden
                        .iter()
                        .map(|&i| members.members()[i].node.clone())
                        .collect();
                    let actual =
                        paths.shortest(members.clone(), &members.members()[source].node, &request);
                    match oracle(n, source, destination, links, &forbidden, &failed) {
                        Some(expected) => assert_eq!(
                            actual.unwrap().nodes,
                            expected
                                .iter()
                                .map(|&i| members.members()[i].node.clone())
                                .collect::<Vec<_>>()
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
        let paths = Paths::new(Rc::new(LinkHealth), 1, 150_000);
        let source = &members.members()[0].node;
        let request = budget(&members, 99_999, 4);
        let mut operation = paths.shortest_async(members.clone(), source, &request);
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
        assert!(paths.shortest(members.clone(), source, &request).is_ok());
        let key = paths.key(&members, source, &request).unwrap();
        let mut search = Search::new(members.members().len(), &key, 150_000);
        assert!(!search.step(1, request.deadline).unwrap());
        assert_eq!(
            search.step(1, Deadline(Instant::now())),
            Err(Error::DeadlineExceeded)
        );
    }
}
