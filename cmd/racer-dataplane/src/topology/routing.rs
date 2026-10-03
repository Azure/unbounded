//! Authenticated routing budgets, worker-local health, and runtime cancellation.
//! Graph/search/weighted selection live in the runtime-independent topology crate.
use super::{
    health::LinkHealth,
    membership::{Membership, MembershipLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::{AttemptId, NodeId, RequestId},
    runtime::deadline::{Deadline, RequestScope},
};
use std::{future::Future, rc::Rc, sync::Arc, task::Poll, time::Instant};

/// Compatibility wrapper for callers holding an authenticated membership lease.
pub struct Graph {
    membership: MembershipLease,
}
impl Graph {
    pub fn new(membership: MembershipLease) -> Self {
        Self { membership }
    }
    pub fn neighbors(&self, node: &NodeId) -> Result<Vec<NodeId>> {
        let position = self.membership.position(node)?;
        Ok(self
            .membership
            .inner
            .neighbors(position)
            .into_iter()
            .map(|index| self.membership.members()[index].node.clone())
            .collect())
    }
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
impl RouteBudget {
    /// `visited` contains prior senders, excluding the current recipient.
    pub fn forwarded(&self, from: &NodeId, next: &NodeId) -> Result<Self> {
        self.validate_at(from, uring_runtime::environment::now())?;
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
        forwarded.validate_at(next, uring_runtime::environment::now())
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

pub struct Paths {
    pub(crate) peer_admission: Option<Arc<crate::peer::adaptive::AdaptivePeers>>,
    health: Rc<LinkHealth>,
    inner: ::topology::Paths,
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
            inner: ::topology::Paths::new(capacity),
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

    /// Runtime checks surround every cooperative algorithm poll, including cache hits.
    pub fn shortest_async<'a>(
        &'a self,
        membership: MembershipLease,
        from: &'a NodeId,
        budget: &'a RouteBudget,
        scope: &'a RequestScope,
    ) -> Operation<'a, Route> {
        Box::pin(async move {
            scope.check()?;
            budget.validate_at(from, uring_runtime::environment::now())?;
            if membership.version != budget.membership {
                return Err(Error::IncompatibleMembership);
            }
            let source = membership.position(from)?;
            let to = membership.position(&budget.destination)?;
            if source != to && budget.remaining_links == 0 {
                return Err(Error::HopBudgetExhausted);
            }
            let visited = budget
                .visited
                .iter()
                .map(|node| membership.position(node))
                .collect::<Result<Vec<_>>>()?;
            let blocked = self.blocked(&membership, source)?;
            let mut seed = [0; 32];
            seed[..16].copy_from_slice(&budget.request.0);
            seed[16..].copy_from_slice(&budget.attempt.0);
            let nodes = {
                let query = ::topology::PathQuery {
                    from: source,
                    to,
                    links: budget.remaining_links,
                    visited: &visited,
                    blocked: &blocked,
                    seed: &seed,
                };
                let mut operation = std::pin::pin!(self.inner.route(&membership.inner, query));
                std::future::poll_fn(|cx| {
                    if let Err(error) = scope
                        .check()
                        .and_then(|()| budget.validate_at(from, uring_runtime::environment::now()))
                    {
                        return Poll::Ready(Err(error));
                    }
                    operation
                        .as_mut()
                        .poll(cx)
                        .map(|result| result.map_err(map_error))
                })
                .await
            };
            scope.check()?;
            budget.validate_at(from, uring_runtime::environment::now())?;
            // Health/admission may change while yielded. Retry under the same budget.
            if self.blocked(&membership, source)? != blocked {
                return Err(Error::Unavailable);
            }
            let nodes = nodes?
                .into_iter()
                .map(|index| membership.members()[index].node.clone())
                .collect();
            Ok(Route { membership, nodes })
        })
    }

    fn blocked(&self, membership: &Membership, source: usize) -> Result<Vec<usize>> {
        let positions = membership.inner.neighbors(source);
        let neighbors: Vec<_> = positions
            .iter()
            .map(|&i| membership.members()[i].node.clone())
            .collect();
        self.health.retain_neighbors(&neighbors);
        let mut blocked = Vec::new();
        for (position, node) in positions.into_iter().zip(&neighbors) {
            if !self.health.available(node)?
                || self
                    .peer_admission
                    .as_ref()
                    .is_some_and(|admission| !admission.available(node))
            {
                blocked.push(position);
            }
        }
        Ok(blocked)
    }
}
fn map_error(error: ::topology::Error) -> Error {
    match error {
        ::topology::Error::InvalidQuery => Error::InvalidRequest,
        ::topology::Error::Overloaded => Error::Overloaded,
        ::topology::Error::Unreachable => Error::Unavailable,
        _ => Error::Internal,
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
        // Private eviction assertions moved to topology::paths tests.
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
        // A fresh cold search succeeds after dropping the previous operation.
        assert!(cold.shortest(members.clone(), source, &request).is_ok());
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

    #[test]
    fn yielded_search_rechecks_health_and_retains_only_current_neighbors() {
        let members = membership(100_000);
        let source = &members.members()[0].node;
        let health = Rc::new(LinkHealth);
        let paths = Paths::new(health.clone(), 1);
        let request = budget(&members, 80_003, 4);
        health
            .observe(&NodeId("retired".into()), LinkOutcome::Timeout)
            .unwrap();
        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
        let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        assert_eq!(health.tracked_links(), 0);
        let neighbor = Graph::new(members.clone()).neighbors(source).unwrap()[0].clone();
        health
            .observe_at(
                &neighbor,
                LinkOutcome::Timeout,
                Instant::now() + Duration::from_secs(60),
            )
            .unwrap();
        assert_eq!(
            futures::executor::block_on(operation).unwrap_err(),
            Error::Unavailable
        );
        let retried = paths.shortest(members.clone(), source, &request).unwrap();
        assert_ne!(retried.nodes[1], neighbor);
    }

    #[test]
    fn yielded_search_rechecks_adaptive_peer_admission() {
        use crate::peer::adaptive::{AdaptivePeers, Config, Outcome};
        let clock = uring_runtime::environment::SimulationClock::new(110);
        let _env = clock.environment(0).enter();
        let members = membership(100_000);
        let source = &members.members()[0].node;
        let admission = AdaptivePeers::new(Config::default(), Default::default()).unwrap();
        let paths = Paths::new(Rc::new(LinkHealth), 1).with_peer_admission(admission.clone());
        let mut request = budget(&members, 80_003, 4);
        request.deadline = Deadline(uring_runtime::environment::now() + Duration::from_secs(60));
        let scope = RequestScope::new(request.request, request.deadline.0).unwrap();
        let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        let neighbor = Graph::new(members.clone()).neighbors(source).unwrap()[0].clone();
        let permit = admission.acquire(&neighbor).unwrap();
        permit.observe(Outcome::PeerFailure);
        drop(permit);
        assert_eq!(
            futures::executor::block_on(operation).unwrap_err(),
            Error::Unavailable
        );
        assert_ne!(
            paths
                .shortest(members.clone(), source, &request)
                .unwrap()
                .nodes[1],
            neighbor
        );
        assert_eq!(paths.link_health().tracked_links(), 0);
    }

    #[test]
    fn yielded_and_cached_searches_enforce_scope_budget_and_membership() {
        let clock = uring_runtime::environment::SimulationClock::new(109);
        let _env = clock.environment(0).enter();
        let members = membership(100_000);
        let source = &members.members()[0].node;
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        for case in 0..3 {
            let paths = Paths::new(Rc::new(LinkHealth), 1);
            let mut request = budget(&members, 80_003, 4);
            let now = uring_runtime::environment::now();
            request.deadline = Deadline(now + Duration::from_secs(if case == 1 { 1 } else { 60 }));
            let scope = RequestScope::new(
                request.request,
                now + Duration::from_secs(if case == 2 { 1 } else { 60 }),
            )
            .unwrap();
            let mut operation = paths.shortest_async(members.clone(), source, &request, &scope);
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            if case == 0 {
                scope.cancel().unwrap();
            } else {
                clock.advance(Duration::from_secs(2));
            }
            let expected = if case == 0 {
                Error::Cancelled
            } else {
                Error::DeadlineExceeded
            };
            assert_eq!(
                futures::executor::block_on(operation).unwrap_err(),
                expected
            );
            // Failure drops the algorithm admission before a new request starts.
            let mut fresh = request.clone();
            fresh.deadline = Deadline(uring_runtime::environment::now() + Duration::from_secs(60));
            paths.shortest(members.clone(), source, &fresh).unwrap();
            assert_eq!(
                futures::executor::block_on(paths.shortest_async(
                    members.clone(),
                    source,
                    &request,
                    &scope
                ))
                .unwrap_err(),
                expected
            );
            fresh.membership = crate::model::MembershipVersion(2);
            assert_eq!(
                paths.shortest(members.clone(), source, &fresh).unwrap_err(),
                Error::IncompatibleMembership
            );
        }
    }
}
