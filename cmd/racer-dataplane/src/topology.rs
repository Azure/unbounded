//! Immutable placement, bidirectional routing, and end-to-end rail selection.
pub mod health {
    //! Worker-local link circuits, distinct from process readiness and placement.
    use super::hash;
    use crate::{
        error::{Error, Result},
        model::NodeId,
    };
    use sha2::Digest;
    use std::{
        cell::RefCell,
        collections::{BTreeMap, BTreeSet},
        time::{Duration, Instant},
    };
    pub struct LinkHealth {
        capacity: usize,
        states: RefCell<BTreeMap<NodeId, Circuit>>,
        probes: RefCell<BTreeSet<NodeId>>,
    }
    pub struct LinkProbe<'a> {
        health: &'a LinkHealth,
        node: Option<NodeId>,
    }
    impl Drop for LinkProbe<'_> {
        fn drop(&mut self) {
            if let Some(node) = &self.node {
                self.health.probes.borrow_mut().remove(node);
            }
        }
    }
    #[allow(non_upper_case_globals, clippy::declare_interior_mutable_const)]
    pub const LinkHealth: LinkHealth = LinkHealth::new(super::MAX_DEGREE);
    struct Circuit {
        failures: u32,
        retry_at: Instant,
        probe_until: Option<Instant>,
    }
    #[derive(Clone, Copy, Debug)]
    pub enum LinkOutcome {
        Success,
        Timeout,
        Refused,
        ProtocolFailure,
    }
    impl LinkHealth {
        pub fn acquire(&self, node: &NodeId) -> Result<LinkProbe<'_>> {
            if !self.try_acquire(node)? {
                return Err(Error::Unavailable);
            }
            let probe = self.states.borrow().contains_key(node);
            if probe {
                if self.probes.borrow().len() >= self.capacity {
                    return Err(Error::Overloaded);
                }
                self.probes.borrow_mut().insert(node.clone());
            }
            Ok(LinkProbe {
                health: self,
                node: probe.then(|| node.clone()),
            })
        }
        /// Application misses and credential rejection do not open transport circuits.
        pub async fn run<T>(
            &self,
            endpoint: &NodeId,
            operation: impl std::future::Future<Output = Result<T>>,
        ) -> Result<T> {
            let _probe = self.acquire(endpoint)?;
            let result = operation.await;
            let outcome = match &result {
                Err(Error::Io | Error::Unavailable) => Some(LinkOutcome::Refused),
                Err(Error::DeadlineExceeded) => Some(LinkOutcome::Timeout),
                Err(Error::BadGateway | Error::CorruptRecord) => Some(LinkOutcome::ProtocolFailure),
                Err(Error::Cancelled | Error::Overloaded) => None,
                _ => Some(LinkOutcome::Success),
            };
            if let Some(outcome) = outcome {
                let _ = self.observe(endpoint, outcome);
            }
            result
        }
        pub const fn new(capacity: usize) -> Self {
            Self {
                capacity,
                states: RefCell::new(BTreeMap::new()),
                probes: RefCell::new(BTreeSet::new()),
            }
        }
        pub fn observe(&self, neighbor: &NodeId, outcome: LinkOutcome) -> Result<()> {
            self.observe_at(neighbor, outcome, crate::runtime::environment::now())
        }
        pub fn observe_at(
            &self,
            neighbor: &NodeId,
            outcome: LinkOutcome,
            now: Instant,
        ) -> Result<()> {
            let mut states = self.states.borrow_mut();
            if matches!(outcome, LinkOutcome::Success) {
                states.remove(neighbor);
                return Ok(());
            }
            if !states.contains_key(neighbor) && states.len() >= self.capacity {
                return Err(Error::Overloaded);
            }
            let state = states.entry(neighbor.clone()).or_insert(Circuit {
                failures: 0,
                retry_at: now,
                probe_until: None,
            });
            state.failures = state.failures.saturating_add(1);
            state.retry_at = now + backoff(neighbor, state.failures);
            state.probe_until = None;
            Ok(())
        }
        /// Routing hint only; actual sends acquire an exclusive half-open probe.
        pub fn available(&self, neighbor: &NodeId) -> Result<bool> {
            self.available_at(neighbor, crate::runtime::environment::now())
        }
        pub fn available_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
            if self.probes.borrow().contains(neighbor) {
                return Ok(false);
            }
            Ok(self.states.borrow().get(neighbor).is_none_or(|s| {
                now >= s.retry_at && s.probe_until.is_none_or(|until| now >= until)
            }))
        }
        pub fn try_acquire(&self, neighbor: &NodeId) -> Result<bool> {
            self.try_acquire_at(neighbor, crate::runtime::environment::now())
        }
        pub fn try_acquire_at(&self, neighbor: &NodeId, now: Instant) -> Result<bool> {
            if self.probes.borrow().contains(neighbor) {
                return Ok(false);
            }
            let mut states = self.states.borrow_mut();
            let Some(state) = states.get_mut(neighbor) else {
                return Ok(true);
            };
            if now < state.retry_at || state.probe_until.is_some_and(|until| now < until) {
                return Ok(false);
            }
            // A dropped or hung probe releases eligibility only after this timeout.
            state.probe_until = Some(now + Duration::from_secs(1));
            Ok(true)
        }
        pub fn retain_neighbors(&self, neighbors: &[NodeId]) {
            self.states
                .borrow_mut()
                .retain(|node, _| neighbors.contains(node));
        }
        pub fn tracked_links(&self) -> usize {
            self.states.borrow().len()
        }
    }
    fn backoff(node: &NodeId, failures: u32) -> Duration {
        let base = (100u64 << failures.saturating_sub(1).min(8)).min(20_000);
        let mut digest = hash::domain(b"racer/link-backoff/v1\0");
        hash::bytes(&mut digest, node.0.as_bytes());
        digest.update(failures.to_be_bytes());
        let digest = hash::finish(digest);
        let jitter = u64::from(u16::from_be_bytes([digest[0], digest[1]])) % (base / 4 + 1);
        Duration::from_millis(base + jitter)
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::{
            model::PageNumber,
            topology::{
                fixtures::{membership, object},
                placement::Placement,
            },
        };
        #[test]
        fn owned_half_open_probe_remains_exclusive_until_completion_or_drop() {
            let health = LinkHealth::new(1);
            let node = NodeId("peer".into());
            health
                .observe_at(
                    &node,
                    LinkOutcome::Timeout,
                    crate::runtime::environment::now() - Duration::from_secs(60),
                )
                .unwrap();
            let probe = health.acquire(&node).unwrap();
            assert!(
                !health
                    .try_acquire_at(
                        &node,
                        crate::runtime::environment::now() + Duration::from_secs(60)
                    )
                    .unwrap()
            );
            drop(probe);
            assert!(
                health
                    .try_acquire_at(
                        &node,
                        crate::runtime::environment::now() + Duration::from_secs(60)
                    )
                    .unwrap()
            );
        }
        #[test]
        fn endpoint_application_errors_do_not_open_circuits_and_probes_are_bounded() {
            let health = LinkHealth::new(2);
            let endpoint = NodeId("origin".into());
            for error in [
                Error::Unauthorized,
                Error::NotFound,
                Error::VersionUnavailable,
            ] {
                assert_eq!(
                    futures::executor::block_on(
                        health.run(&endpoint, async { Err::<(), _>(error) })
                    ),
                    Err(error)
                );
                assert_eq!(health.tracked_links(), 0);
            }
            assert_eq!(
                futures::executor::block_on(
                    health.run(&endpoint, async { Err::<(), _>(Error::Io) })
                ),
                Err(Error::Io)
            );
            assert_eq!(
                futures::executor::block_on(health.run(&endpoint, async {
                    panic!("open circuit attempted I/O");
                    #[allow(unreachable_code)]
                    Ok(())
                })),
                Err(Error::Unavailable)
            );
        }
        #[test]
        fn circuit_backoff_probes_success_and_isolation() {
            let first = LinkHealth::new(2);
            let second = LinkHealth::new(2);
            let node = NodeId("a".into());
            let now = Instant::now();
            assert!(first.try_acquire_at(&node, now).unwrap());
            first.observe_at(&node, LinkOutcome::Timeout, now).unwrap();
            assert!(!first.available_at(&node, now).unwrap());
            assert!(second.available_at(&node, now).unwrap());
            let retry = now + backoff(&node, 1);
            assert!(
                !first
                    .available_at(&node, retry - Duration::from_nanos(1))
                    .unwrap()
            );
            assert!(first.available_at(&node, retry).unwrap());
            assert!(first.try_acquire_at(&node, retry).unwrap());
            assert!(!first.try_acquire_at(&node, retry).unwrap());
            assert!(
                first
                    .try_acquire_at(&node, retry + Duration::from_secs(1))
                    .unwrap()
            );
            first
                .observe_at(&node, LinkOutcome::Refused, retry)
                .unwrap();
            assert!(backoff(&node, 2) > backoff(&node, 1));
            first
                .observe_at(&node, LinkOutcome::Success, retry)
                .unwrap();
            assert!(first.available_at(&node, retry).unwrap());
            assert_eq!(first.tracked_links(), 0);
            for failures in [1, 2, 10, u32::MAX] {
                assert!(backoff(&node, failures) <= Duration::from_secs(25));
            }
            assert_ne!(backoff(&node, 3), backoff(&NodeId("b".into()), 3));
        }
        #[test]
        fn capacity_and_placement_are_independent() {
            let health = LinkHealth::new(1);
            let members = membership(3);
            let placement = Placement::new(1);
            let before = placement
                .rank(members.clone(), &object(), PageNumber(0))
                .unwrap()
                .ordered;
            health
                .observe(&before[0], LinkOutcome::ProtocolFailure)
                .unwrap();
            assert_eq!(
                health.observe(&before[1], LinkOutcome::Timeout),
                Err(Error::Overloaded)
            );
            assert_eq!(
                before,
                placement
                    .rank(members, &object(), PageNumber(0))
                    .unwrap()
                    .ordered
            );
            health.retain_neighbors(&[]);
            assert_eq!(health.tracked_links(), 0);
        }
        #[test]
        fn default_tracks_all_64_neighbors_without_eviction() {
            let health = LinkHealth;
            let now = Instant::now();
            for i in 0..64 {
                health
                    .observe_at(&NodeId(format!("peer-{i}")), LinkOutcome::Timeout, now)
                    .unwrap();
            }
            assert_eq!(health.tracked_links(), 64);
            for i in 0..64 {
                assert!(
                    !health
                        .available_at(&NodeId(format!("peer-{i}")), now)
                        .unwrap()
                );
            }
            assert_eq!(
                health.observe_at(&NodeId("overflow".into()), LinkOutcome::Timeout, now),
                Err(Error::Overloaded)
            );
        }
    }
}
pub mod membership;
pub mod placement;
pub mod rails {
    //! RDMA requires compatible authenticated mappings; discovery can only veto.
    use super::{
        hash,
        routing::{FAILURE_LINKS, Route},
    };
    use crate::{
        error::{Error, Result},
        model::PageId,
    };
    #[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
    pub struct RailId(pub u16);
    #[derive(Clone, Debug, Eq, PartialEq)]
    pub struct RailMapping {
        pub rail: RailId,
        pub device: String,
        pub port: u8,
        pub gid: Option<[u8; 16]>,
        pub numa_node: Option<usize>,
    }
    #[derive(Clone, Copy, Debug, Eq, PartialEq)]
    pub enum TransportPlan {
        Http,
        Rdma { rail: RailId },
    }
    /// Conservative route summary; actual transport admission uses select_hop.
    pub fn select(route: &Route, page: &PageId) -> Result<TransportPlan> {
        validate_route(route)?;
        let members = route
            .nodes
            .iter()
            .map(|node| route.membership.member(node))
            .collect::<Result<Vec<_>>>()?;
        select_members(&route.membership, &members, page)
    }
    pub fn select_hop(
        route: &Route,
        page: &PageId,
        local: &crate::model::NodeId,
        peer: &crate::model::NodeId,
    ) -> Result<TransportPlan> {
        validate_route(route)?;
        if !route.nodes.windows(2).any(|pair| {
            (&pair[0] == local && &pair[1] == peer) || (&pair[1] == local && &pair[0] == peer)
        }) {
            return Err(Error::IncompatibleMembership);
        }
        select_members(
            &route.membership,
            &[
                route.membership.member(local)?,
                route.membership.member(peer)?,
            ],
            page,
        )
    }
    fn validate_route(route: &Route) -> Result<()> {
        if route.nodes.is_empty()
            || route.nodes.len() > usize::from(FAILURE_LINKS) + 1
            || route
                .nodes
                .iter()
                .enumerate()
                .any(|(i, node)| route.nodes[..i].contains(node))
        {
            return Err(Error::InvalidRequest);
        }
        for node in &route.nodes {
            route.membership.member(node)?;
        }
        Ok(())
    }
    fn select_members(
        membership: &super::membership::Membership,
        members: &[&super::membership::Member],
        page: &PageId,
    ) -> Result<TransportPlan> {
        if members
            .iter()
            .any(|m| m.site.is_empty() || m.site != members[0].site || m.rails.is_empty())
        {
            return Ok(TransportPlan::Http);
        }
        // Site is an admission boundary, not a new rail domain or hash scheme.
        let domain = membership.rail_domain();
        if domain.is_empty() {
            return Ok(TransportPlan::Http);
        }
        let mut digest = hash::domain(b"racer/rail/v2\0");
        hash::object(&mut digest, &page.version.object, page.number);
        let digest = hash::finish(digest);
        let sample = u64::from_be_bytes(digest[..8].try_into().unwrap());
        let rail = domain[(sample % domain.len() as u64) as usize];
        if !members
            .iter()
            .all(|m| m.rails.iter().any(|m| m.rail == rail))
        {
            return Ok(TransportPlan::Http);
        }
        Ok(TransportPlan::Rdma { rail })
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::{
            model::*,
            topology::{
                fixtures::{member, object},
                membership::Membership,
            },
        };
        use std::sync::Arc;
        fn mappings() -> Vec<RailMapping> {
            vec![
                RailMapping {
                    rail: RailId(7),
                    device: "a".into(),
                    port: 1,
                    gid: None,
                    numa_node: Some(0),
                },
                RailMapping {
                    rail: RailId(2),
                    device: "b".into(),
                    port: 1,
                    gid: None,
                    numa_node: Some(1),
                },
            ]
        }
        fn page(number: u64) -> PageId {
            PageId {
                version: ObjectVersion {
                    object: object(),
                    etag: StrongEtag::test_value("\"v1\""),
                },
                number: PageNumber(number),
            }
        }
        // Fixture-side hardware veto for summary tests. Real sessions additionally
        // require device activation; this helper never grants transport admission.
        fn local_compatible(
            route: &Route,
            plan: &TransportPlan,
            local: &NodeId,
            discovered: &[RailMapping],
        ) -> Result<bool> {
            if !route.nodes.contains(local) {
                return Err(Error::IncompatibleMembership);
            }
            let member = route.membership.member(local)?;
            let TransportPlan::Rdma { rail } = plan else {
                return Ok(true);
            };
            let Some(published) = member.rails.iter().find(|m| m.rail == *rail) else {
                return Ok(false);
            };
            let mut matching = discovered.iter().filter(|m| m.rail == *rail);
            Ok(matching.next().is_some_and(|hardware| {
                hardware.device == published.device
                    && hardware.port == published.port
                    && published
                        .numa_node
                        .is_none_or(|numa| hardware.numa_node == Some(numa))
            }) && matching.next().is_none())
        }
        fn select_with_local(
            route: &Route,
            page: &PageId,
            local: &NodeId,
            discovered: &[RailMapping],
        ) -> Result<TransportPlan> {
            let plan = select(route, page)?;
            Ok(if local_compatible(route, &plan, local, discovered)? {
                plan
            } else {
                TransportPlan::Http
            })
        }
        fn route(change: impl FnOnce(&mut Vec<super::super::membership::Member>)) -> Route {
            let mut members: Vec<_> = (0..3)
                .map(|i| {
                    let mut member = member(i, 4);
                    member.site = "site1".into();
                    member.rails = mappings();
                    member
                })
                .collect();
            change(&mut members);
            let membership = Arc::new(Membership::validate(MembershipVersion(1), members).unwrap());
            Route {
                nodes: membership
                    .members()
                    .iter()
                    .map(|m| m.node.clone())
                    .collect(),
                membership,
            }
        }
        #[test]
        fn mixed_site_hops_preserve_global_rail_mapping_and_hardware_vetoes() {
            let mixed = route(|m| m[2].site = "site2".into());
            let a = &mixed.nodes[0];
            let b = &mixed.nodes[1];
            let c = &mixed.nodes[2];
            assert_eq!(select(&mixed, &page(0)).unwrap(), TransportPlan::Http);
            for (from, to, expected) in [
                (a, b, TransportPlan::Rdma { rail: RailId(2) }),
                (b, a, TransportPlan::Rdma { rail: RailId(2) }),
                (b, c, TransportPlan::Http),
                (c, b, TransportPlan::Http),
            ] {
                assert_eq!(select_hop(&mixed, &page(0), from, to).unwrap(), expected);
            }
            for other in [c, a, &NodeId("unknown".into())] {
                assert!(select_hop(&mixed, &page(0), a, other).is_err());
            }
            for local in [0, 1] {
                let missing = route(|m| m[local].site.clear());
                assert_eq!(
                    select_hop(&missing, &page(0), a, b).unwrap(),
                    TransportPlan::Http
                );
            }
            for changed in [
                route(|m| m[1].site.clear()),
                route(|m| m[1].rails.clear()),
                route(|m| {
                    m[1].rails
                        .iter_mut()
                        .for_each(|r| r.rail = RailId(r.rail.0 + 1))
                }),
            ] {
                assert_eq!(
                    select_hop(&changed, &page(0), a, b).unwrap(),
                    TransportPlan::Http
                );
            }
            let plan = select_hop(&mixed, &page(0), a, b).unwrap();
            assert!(local_compatible(&mixed, &plan, a, &mappings()).unwrap());
            assert!(!local_compatible(&mixed, &plan, a, &[]).unwrap());
        }
        #[test]
        fn golden_page_to_rail_vectors() {
            let route = route(|_| {});
            for (number, rail) in [(0, 2), (1, 7), (u64::MAX, 2)] {
                assert_eq!(
                    select(&route, &page(number)).unwrap(),
                    TransportPlan::Rdma { rail: RailId(rail) }
                );
            }
        }
        #[test]
        fn repeated_rails_and_different_physical_names_do_not_change_remote_eligibility() {
            let original = route(|_| {});
            let repeated = route(|members| {
                for (i, member) in members.iter_mut().enumerate() {
                    let mut extra = member.rails[0].clone();
                    extra.device = format!("extra-{i}");
                    member.rails.push(extra);
                    member.rails[0].device = format!("local-{i}");
                }
            });
            assert_eq!(
                original.membership.rail_domain(),
                repeated.membership.rail_domain()
            );
            for number in 0..100 {
                assert_eq!(
                    select(&original, &page(number)),
                    select(&repeated, &page(number))
                );
            }
        }
        #[test]
        fn intersection_over_all_hops_and_http_fallback() {
            for route in [
                route(|m| m[1].site.clear()),
                route(|m| m[1].rails.clear()),
                route(|m| {
                    for rail in &mut m[1].rails {
                        rail.rail = RailId(rail.rail.0 + 1);
                    }
                }),
            ] {
                assert_eq!(select(&route, &page(0)).unwrap(), TransportPlan::Http);
            }
            let full = route(|_| {});
            let partial = route(|m| m[1].rails.retain(|rail| rail.rail == RailId(7)));
            for number in 0..100 {
                let expected = match select(&full, &page(number)).unwrap() {
                    TransportPlan::Rdma { rail: RailId(7) } => {
                        TransportPlan::Rdma { rail: RailId(7) }
                    }
                    _ => TransportPlan::Http,
                };
                assert_eq!(select(&partial, &page(number)).unwrap(), expected);
                let mut alternate = partial.clone();
                alternate.nodes.remove(1);
                assert_eq!(
                    select(&alternate, &page(number)).unwrap(),
                    select(&full, &page(number)).unwrap()
                );
                let mut version = page(number);
                version.version.etag = StrongEtag::test_value("\"v2\"");
                assert_eq!(
                    select(&full, &version).unwrap(),
                    select(&full, &page(number)).unwrap()
                );
            }
        }
        #[test]
        fn deterministic_reverse_path_order_and_local_hardware() {
            let route = route(|m| {
                m[1].rails.reverse();
                for rail in &mut m[1].rails {
                    rail.numa_node = Some(99);
                }
            });
            let mut reverse = route.clone();
            reverse.nodes.reverse();
            let mut selected = std::collections::BTreeSet::new();
            for number in 0..100 {
                let plan = select(&route, &page(number)).unwrap();
                assert_eq!(plan, select(&reverse, &page(number)).unwrap());
                let TransportPlan::Rdma { rail } = plan else {
                    panic!("expected RDMA");
                };
                selected.insert(rail);
                for (local, hardware, expected) in [
                    (0, mappings(), plan),
                    (1, mappings(), TransportPlan::Http),
                    (0, vec![], TransportPlan::Http),
                ] {
                    assert_eq!(
                        select_with_local(&route, &page(number), &route.nodes[local], &hardware)
                            .unwrap(),
                        expected
                    );
                }
            }
            assert_eq!(selected.len(), 2);
            let mut invalid = route.clone();
            invalid.nodes.push(invalid.nodes[0].clone());
            assert_eq!(select(&invalid, &page(0)), Err(Error::InvalidRequest));
        }
    }
}
pub mod routing;

mod hash {
    use crate::model::{ObjectId, PageNumber};
    use sha2::{Digest, Sha256};

    pub(super) fn domain(name: &[u8]) -> Sha256 {
        let mut hash = Sha256::new();
        hash.update(name);
        hash
    }
    pub(super) fn bytes(hash: &mut Sha256, value: &[u8]) {
        hash.update((value.len() as u32).to_be_bytes());
        hash.update(value);
    }
    pub(super) fn object(hash: &mut Sha256, object: &ObjectId, page: PageNumber) {
        bytes(hash, object.cache.0.as_bytes());
        hash.update(object.key.0);
        hash.update(page.0.to_be_bytes());
    }
    pub(super) fn finish(hash: Sha256) -> [u8; 32] {
        hash.finalize().into()
    }
    #[cfg(test)]
    mod tests {
        use super::*;
        use crate::topology::fixtures;
        #[test]
        fn full_domain_separated_digest_vectors() {
            for (name, kind, expected) in [
                (
                    b"racer/slot/v1\0".as_slice(),
                    0,
                    "d8b632a58acf4dc92ccc3abe711290968975c95221982118f58393fbdead4781",
                ),
                (
                    b"racer/hrw/v1\0",
                    1,
                    "e41095812e885f6f0ae7e3c1a93d8ec04999df01dbb5820765c7e972b2c07a9c",
                ),
                (
                    b"racer/rail/v1\0",
                    2,
                    "500304ace38caf7cc36f8f97ae12bdfc89b92f7eb64c8bb060d1daf49f76314c",
                ),
                (
                    b"racer/rail/v2\0",
                    0,
                    "9ac860a9df3ce1ac648cde8350aeec1d1efc604eeae5cceb70d67dd55f4443d1",
                ),
            ] {
                let mut hash = domain(name);
                if kind == 1 {
                    hash.update(887651u32.to_be_bytes());
                    bytes(&mut hash, b"node-000000");
                } else {
                    object(&mut hash, &fixtures::object(), PageNumber(0));
                    if kind == 2 {
                        bytes(&mut hash, b"\"v1\"");
                    }
                }
                let actual: String = finish(hash).iter().map(|b| format!("{b:02x}")).collect();
                assert_eq!(actual, expected);
            }
        }
    }
}

/// Algorithm changes require a new version and new interoperability vectors.
/// Latest supported contract; topology changes require coordinated rollout.
pub const ALGORITHM_VERSION: u32 = 5;

pub const RADIX: usize = 32;

/// Shared capacity bound for every supported topology, including first-hop masks.
pub const MAX_DEGREE: usize = 2 * RADIX;
const _: () = assert!(MAX_DEGREE <= u64::BITS as usize);

#[cfg(test)]
mod fixtures {
    use super::membership::{Member, Membership, MembershipLease};
    use crate::model::*;
    use std::{num::NonZeroU32, sync::Arc};

    // Independent Python hashlib + outgoing-edge BFS vectors. N=1500, source=0,
    // destination=1499, request=[1;16], shares=1 at multiples of 3 and 4 elsewhere.
    // Attempts are big-endian u128; V5 intentionally retains the V4 hash domain.
    pub(super) const V5_NEXT_HOPS: [(u128, usize); 4] = [(0, 1312), (1, 937), (2, 703), (127, 937)];
    pub(super) fn member(index: usize, shares: u32) -> Member {
        Member {
            node: NodeId(format!("node-{index:06}")),
            shares: NonZeroU32::new(shares).unwrap(),
            peer_endpoint: "127.0.0.1:8080".into(),
            rails: vec![],
            site: String::new(),
        }
    }
    pub(super) fn membership(count: usize) -> MembershipLease {
        Arc::new(
            Membership::validate(
                MembershipVersion(1),
                (0..count).map(|i| member(i, 4)).collect(),
            )
            .unwrap(),
        )
    }
    pub(super) fn object() -> ObjectId {
        ObjectId {
            cache: CacheId("cache-a".into()),
            key: CacheKey([0x42; 32]),
        }
    }
}
