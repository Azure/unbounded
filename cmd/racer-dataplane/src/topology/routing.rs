//! Canonical slot encoding, placement, authenticated routing, and cancellation.
//! Graph/search/weighted selection live in the runtime-independent topology crate.
use super::{
    hash,
    health::LinkHealth,
    membership::{Membership, MembershipLease},
};
use crate::{
    error::{Error, Operation, Result},
    model::{AttemptId, NodeId, ObjectId, PageNumber, RequestId},
    runtime::deadline::{Deadline, RequestScope},
};
use std::{future::Future, rc::Rc, sync::Arc, task::Poll, time::Instant};

pub const SLOT_COUNT: u32 = 1 << 20;
/// Conservative allocation charge: ranking, four scores, Rc/RefCell, BTree
/// entry and FIFO key, including container slack and allocator overhead.
pub const RANKING_BYTES: usize = ::topology::Placement::ENTRY_BYTES;

pub struct Placement {
    inner: ::topology::Placement,
}
#[derive(Clone, Debug)]
pub struct Candidates {
    pub membership: MembershipLease,
    pub ordered: Vec<NodeId>,
}

fn encoded_key(object: &ObjectId, page: PageNumber) -> Vec<u8> {
    let mut key = Vec::with_capacity(4 + object.cache.0.len() + 32 + 8);
    key.extend_from_slice(&(object.cache.0.len() as u32).to_be_bytes());
    key.extend_from_slice(object.cache.0.as_bytes());
    key.extend_from_slice(&object.key.0);
    key.extend_from_slice(&page.0.to_be_bytes());
    key
}

/// Fixed slot independent of object version and membership. Metadata passes page 0.
pub fn slot(object: &ObjectId, page: PageNumber) -> u32 {
    let mut digest = hash::domain(b"racer/slot/v1\0");
    hash::object(&mut digest, object, page);
    let digest = hash::finish(digest);
    u32::from_be_bytes(digest[..4].try_into().unwrap()) >> 12
}

fn candidates(membership: MembershipLease, ranked: Vec<usize>) -> Candidates {
    let ordered = ranked
        .into_iter()
        .map(|i| membership.members()[i].node.clone())
        .collect();
    Candidates {
        membership,
        ordered,
    }
}

fn placement_error(error: ::topology::Error) -> Error {
    match error {
        ::topology::Error::Overloaded => Error::Overloaded,
        _ => Error::InvalidConfiguration,
    }
}

impl Placement {
    pub fn new(capacity: usize) -> Self {
        Self {
            inner: ::topology::Placement::new(capacity),
        }
    }

    pub fn with_memory_budget(bytes: usize) -> Self {
        Self::new(bytes / RANKING_BYTES)
    }

    pub fn rank(
        &self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Result<Candidates> {
        let ranked = self
            .inner
            .rank(&membership.inner, &encoded_key(object, page))
            .map_err(placement_error)?;
        Ok(candidates(membership, ranked))
    }

    /// Reactor-friendly cold ranking. Concurrent requests for a resident slot
    /// share progress; each poll hashes at most 256 members, with no held borrow
    /// across the yield. Dropped operations release their active-cache admission.
    pub fn rank_async<'a>(
        &'a self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
    ) -> Operation<'a, Candidates> {
        self.rank_scoped(membership, object, page, None)
    }

    pub fn rank_scoped<'a>(
        &'a self,
        membership: MembershipLease,
        object: &ObjectId,
        page: PageNumber,
        scope: Option<&'a crate::runtime::deadline::RequestScope>,
    ) -> Operation<'a, Candidates> {
        let key = encoded_key(object, page);
        Box::pin(async move {
            if let Some(scope) = scope {
                scope.check()?;
            }
            let cancellation = scope
                .map(|scope| scope.cancellation.subscribe())
                .transpose()?;
            let mut ranking = std::pin::pin!(self.inner.rank_async(&membership.inner, &key));
            let ranked = std::future::poll_fn(|cx| {
                if let Some(scope) = scope {
                    if let Some(cancellation) = &cancellation {
                        cancellation.register(cx.waker());
                    }
                    scope.check()?;
                }
                let result = ranking
                    .as_mut()
                    .poll(cx)
                    .map(|result| result.map_err(placement_error));
                if let Some(scope) = scope {
                    scope.check()?;
                }
                result
            })
            .await?;
            Ok(candidates(membership.clone(), ranked))
        })
    }

    /// Warm only already-demanded predecessor slots. Each turn hashes at most
    /// one cold quantum or visits one retained key, with no full-cache sweep.
    pub fn maintain(&self, membership: &MembershipLease) -> Result<()> {
        self.inner
            .maintain(&membership.inner)
            .map_err(placement_error)
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
mod tests {
    use super::*;
    use crate::{
        model::MembershipVersion,
        topology::{
            fixtures::{member, membership, object},
            membership::{Membership, scored_members},
        },
    };
    use std::sync::Arc;

    #[test]
    fn golden_slot_and_weighted_ranking_vectors() {
        let members = Arc::new(
            Membership::validate(
                MembershipVersion(1),
                [1, 3, 6, 4]
                    .into_iter()
                    .enumerate()
                    .map(|(i, weight)| member(i, weight))
                    .collect(),
            )
            .unwrap(),
        );
        let placement = Placement::new(3);
        for (page, expected_slot, expected_order) in [
            (0, 887_651, [1, 3, 2]),
            (1, 348_931, [3, 2, 1]),
            (u64::MAX, 665_200, [2, 1, 3]),
        ] {
            assert_eq!(slot(&object(), PageNumber(page)), expected_slot);
            let ranking = placement
                .rank(members.clone(), &object(), PageNumber(page))
                .unwrap();
            assert_eq!(ranking.ordered, expected_order.map(|i| member(i, 1).node));
        }
    }

    #[test]
    fn addition_removal_preserve_survivor_order_and_old_snapshot_rankings() {
        let old = membership(12);
        let mut added = old.members().to_vec();
        added.push(member(12, 4));
        let added = Arc::new(Membership::validate(MembershipVersion(2), added).unwrap());
        let removed = Arc::new(
            Membership::validate(MembershipVersion(3), old.members()[1..].to_vec()).unwrap(),
        );
        let placement = Placement::new(6);
        for page in 0..100 {
            let original = placement
                .rank(old.clone(), &object(), PageNumber(page))
                .unwrap()
                .ordered;
            let extended = placement
                .rank(added.clone(), &object(), PageNumber(page))
                .unwrap()
                .ordered;
            let survivors: Vec<_> = extended
                .iter()
                .filter(|node| **node != member(12, 1).node)
                .cloned()
                .collect();
            assert_eq!(&original[..survivors.len()], survivors);
            let reduced = placement
                .rank(removed.clone(), &object(), PageNumber(page))
                .unwrap()
                .ordered;
            let survivors: Vec<_> = original
                .iter()
                .filter(|node| **node != member(0, 1).node)
                .cloned()
                .collect();
            assert_eq!(&reduced[..survivors.len()], survivors);
            assert_eq!(
                placement
                    .rank(old.clone(), &object(), PageNumber(page))
                    .unwrap()
                    .ordered,
                original
            );
        }
    }

    #[test]
    fn small_clusters_order_invariance_and_nonplacement_changes() {
        let placement = Placement::new(10);
        for count in 0..=4 {
            assert_eq!(
                placement
                    .rank(membership(count), &object(), PageNumber(0))
                    .unwrap()
                    .ordered
                    .len(),
                count.min(3)
            );
        }
        let original = membership(30);
        let mut changed = original.members().to_vec();
        changed.reverse();
        for node in &mut changed {
            node.peer_endpoint = "[::1]:9090".into();
            node.rails.clear();
        }
        let changed = Arc::new(Membership::validate(MembershipVersion(2), changed).unwrap());
        for page in 0..100 {
            assert_eq!(
                placement
                    .rank(original.clone(), &object(), PageNumber(page))
                    .unwrap()
                    .ordered,
                placement
                    .rank(changed.clone(), &object(), PageNumber(page))
                    .unwrap()
                    .ordered
            );
        }
    }

    #[test]
    fn incremental_demand_maintenance_matches_cold_oracle_under_churn() {
        let placement = Placement::with_memory_budget(512 * RANKING_BYTES);
        let oracle = Placement::new(0);
        let mut old = membership(80);
        for generation in 2..42 {
            for page in 0..40 {
                placement
                    .rank(old.clone(), &object(), PageNumber(page))
                    .unwrap();
            }
            let mut members = old.members().to_vec();
            members.remove(generation as usize % members.len());
            members.push(member(100 + generation as usize, 4));
            members[generation as usize % 10].shares =
                std::num::NonZeroU32::new(generation % 7 + 1).unwrap();
            let next = Arc::new(
                Membership::validate(MembershipVersion(generation as u64), members)
                    .unwrap()
                    .with_predecessor(&old),
            );
            for page in 0..40 {
                assert_eq!(
                    placement
                        .rank(next.clone(), &object(), PageNumber(page))
                        .unwrap()
                        .ordered,
                    oracle
                        .rank(next.clone(), &object(), PageNumber(page))
                        .unwrap()
                        .ordered,
                    "generation {generation} page {page}"
                );
            }
            old = next;
        }
    }

    #[test]
    fn endpoint_epoch_reuses_completed_rank_and_returns_current_routing() {
        let placement = Placement::with_memory_budget(RANKING_BYTES);
        let old = membership(100_000);
        let expected = placement
            .rank(old.clone(), &object(), PageNumber(0))
            .unwrap();
        let mut nodes = old.members().to_vec();
        for node in &mut nodes {
            node.peer_endpoint = "[::1]:9090".into();
            node.rails.clear();
        }
        let current = Arc::new(Membership::validate(MembershipVersion(2), nodes).unwrap());
        assert_eq!(old.placement_identity(), current.placement_identity());
        let mut future = placement.rank_async(current.clone(), &object(), PageNumber(0));
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let before = scored_members();
        let Poll::Ready(Ok(actual)) = future.as_mut().poll(&mut cx) else {
            panic!("endpoint-only change must not perform another cold ranking");
        };
        assert_eq!(actual.ordered, expected.ordered);
        assert!(Arc::ptr_eq(&actual.membership, &current));
        assert_eq!(scored_members(), before);
        drop(old);
        drop(expected);
        placement.rank(current, &object(), PageNumber(0)).unwrap();
        assert_eq!(scored_members(), before);
    }

    #[test]
    fn scoped_cold_rank_cancels_between_bounded_quanta() {
        use crate::{model::RequestId, runtime::deadline::RequestScope};
        let placement = Placement::with_memory_budget(4 * RANKING_BYTES);
        let members = membership(100_000);
        let scope = RequestScope::new(
            RequestId([1; 16]),
            uring_runtime::environment::now() + std::time::Duration::from_secs(30),
        )
        .unwrap();
        let mut future = placement.rank_scoped(members, &object(), PageNumber(0), Some(&scope));
        let mut cx = std::task::Context::from_waker(futures::task::noop_waker_ref());
        let before = scored_members();
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.cancel().unwrap();
        assert!(matches!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(scored_members() - before, 256);
    }

    #[test]
    fn cooperative_coalescing_bounded_cache_and_cancellation() {
        use std::task::Context;
        let placement = Placement::new(1);
        let members = membership(100_000);
        let mut first = placement.rank_async(members.clone(), &object(), PageNumber(0));
        let mut second = placement.rank_async(members.clone(), &object(), PageNumber(0));
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let before = scored_members();
        assert!(first.as_mut().poll(&mut cx).is_pending());
        assert!(second.as_mut().poll(&mut cx).is_pending());
        assert_eq!(scored_members() - before, 512);
        assert_eq!(
            placement
                .rank(members.clone(), &object(), PageNumber(1))
                .unwrap_err(),
            Error::Overloaded
        );
        drop(first);
        let ranked = futures::executor::block_on(second).unwrap();
        assert_eq!(
            ranked.ordered,
            placement
                .rank(members.clone(), &object(), PageNumber(0))
                .unwrap()
                .ordered
        );
        placement
            .rank(members.clone(), &object(), PageNumber(1))
            .unwrap();
        let before = scored_members();
        placement
            .rank(members.clone(), &object(), PageNumber(1))
            .unwrap();
        assert_eq!(scored_members(), before);
        assert_eq!(Arc::strong_count(&members), 2); // caller and returned Candidates only
        let uncached = Placement::new(0);
        uncached
            .rank(members.clone(), &object(), PageNumber(0))
            .unwrap();
        let before = scored_members();
        uncached.rank(members, &object(), PageNumber(0)).unwrap();
        assert_eq!(scored_members() - before, 100_000);
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
        for neighbor in members.neighbors(source).unwrap() {
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
        let empty = membership(0);
        assert!(empty.neighbors(&NodeId("unknown".into())).is_err());
        let one = membership(1);
        assert!(one.neighbors(&one.members()[0].node).unwrap().is_empty());
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
        let neighbor = members.neighbors(source).unwrap()[0].clone();
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
        let neighbor = members.neighbors(source).unwrap()[0].clone();
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
