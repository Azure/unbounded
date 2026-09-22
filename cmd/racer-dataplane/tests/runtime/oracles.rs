// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Routing capabilities only. Byte, dependency, deadline, and resource checks
//! cannot be disabled through this declaration.
use std::collections::{BTreeMap, BTreeSet};
use std::net::SocketAddr;

/// Test-owned placement, independent of Routing, Cursor, and Topology. Physical
/// IDs and observation-node IDs are separate: an extra worker is not a slot.
pub(in crate::runtime) struct Placement {
    slots: u32,
    local: BTreeMap<usize, BTreeSet<u32>>,
    neighbors: BTreeMap<usize, BTreeMap<u32, usize>>,
    workers: BTreeMap<usize, usize>,
    endpoints: BTreeMap<SocketAddr, usize>,
}

#[derive(Debug)]
pub(in crate::runtime) struct ModelRoute {
    source: u32,
    owner: u32,
    attempt: u32,
    pub(in crate::runtime) path: Vec<u32>,
    // Physical receiver, unnormalized wire position, ingress flag.
    arrivals: Vec<(usize, usize, bool)>,
    pub(in crate::runtime) hops: Vec<(usize, usize)>,
    pub(in crate::runtime) final_peer: bool,
    pub(in crate::runtime) origin: usize,
}

fn degree(slots: u32) -> u64 {
    assert!(slots > 0);
    let mut d = 1u64;
    while d.pow(3) < u64::from(slots) {
        d += 1;
    }
    d
}

/// Enumerate words by length, then lexicographic digit order. This intentionally
/// does not use the production reach interval, route iterator, or cursor.
fn digit_path(slots: u32, source: u32, destination: u32) -> Vec<u32> {
    assert!(source < slots && destination < slots);
    let d = degree(slots);
    for length in 0..=3 {
        for word in 0..d.pow(length) {
            let mut path = [source; 4];
            let mut used = 1;
            let mut remainder = word;
            for position in (0..length).rev() {
                let place = d.pow(position);
                let digit = remainder / place;
                remainder %= place;
                path[used] = ((u64::from(path[used - 1]) * d + digit) % u64::from(slots)) as u32;
                used += 1;
            }
            if path[used - 1] == destination {
                return path[..used].to_vec();
            }
        }
    }
    panic!("digit graph must reach every slot within three edges");
}

impl Placement {
    /// A complete partition for this request's publication view. Other live
    /// publications may differ, as in the isolated all-local admission fixture.
    pub(in crate::runtime) fn new(slots: u32, owners: Vec<(usize, Vec<u32>)>) -> Self {
        let d = degree(slots);
        let mut physical = BTreeMap::new();
        let mut local = BTreeMap::new();
        for (node, assigned) in owners {
            assert!(!assigned.is_empty());
            for &slot in &assigned {
                assert!(slot < slots);
                assert!(physical.insert(slot, node).is_none(), "duplicate slot");
            }
            assert!(
                local
                    .insert(node, assigned.into_iter().collect::<BTreeSet<_>>())
                    .is_none()
            );
        }
        assert_eq!(physical.len(), slots as usize, "incomplete placement");
        let neighbors = local
            .iter()
            .map(|(&node, assigned)| {
                let edges = assigned
                    .iter()
                    .flat_map(|&slot| {
                        (0..d).map(move |digit| {
                            ((u64::from(slot) * d + digit) % u64::from(slots)) as u32
                        })
                    })
                    .filter(|slot| !assigned.contains(slot))
                    .map(|slot| (slot, physical[&slot]))
                    .collect();
                (node, edges)
            })
            .collect();
        let workers = local.keys().map(|&node| (node, node)).collect();
        let endpoints = local
            .keys()
            .map(|&node| (format!("127.0.0.1:{}", 10000 + node).parse().unwrap(), node))
            .collect();
        Self {
            slots,
            local,
            neighbors,
            workers,
            endpoints,
        }
    }

    pub(in crate::runtime) fn alias(
        &mut self,
        worker: usize,
        physical: usize,
        endpoint: SocketAddr,
    ) {
        assert!(self.local.contains_key(&physical));
        assert!(self.workers.insert(worker, physical).is_none());
        assert!(self.endpoints.insert(endpoint, physical).is_none());
    }

    /// Compare raw fixture publications with the independently derived graph.
    pub(in crate::runtime) fn assert_neighbors(&self, node: usize, edges: BTreeMap<u32, usize>) {
        assert_eq!(self.neighbors[&node], edges);
    }

    pub(in crate::runtime) fn owner(&self, target: &str) -> u32 {
        (u64::from_le_bytes(
            blake3::hash(target.as_bytes()).as_bytes()[..8]
                .try_into()
                .unwrap(),
        ) % u64::from(self.slots)) as u32
    }

    pub(in crate::runtime) fn route(
        &self,
        worker: usize,
        target: &str,
        attempt: u32,
    ) -> ModelRoute {
        assert!(attempt < self.slots);
        let mut node = self.workers[&worker];
        let source = *self.local[&node].first().unwrap();
        let owner = self.owner(target);
        let candidate = ((u64::from(owner) + u64::from(attempt)) % u64::from(self.slots)) as u32;
        let path = digit_path(self.slots, source, candidate);
        let mut arrivals = vec![(node, 0, true)];
        let mut hops = Vec::new();
        let mut position = 0;
        let mut final_peer = false;
        loop {
            assert!(self.local[&node].contains(&path[position]));
            // Last later local slot, not just consecutive local slots.
            let effective = (position..path.len())
                .rev()
                .find(|&i| self.local[&node].contains(&path[i]))
                .unwrap();
            if effective + 1 == path.len() {
                break;
            }
            let next = self.neighbors[&node][&path[effective + 1]];
            let rank = digit_path(self.slots, path[effective], candidate).len() - 1;
            let received_rank = digit_path(self.slots, path[effective + 1], candidate).len() - 1;
            assert_eq!(rank, received_rank + 1);
            let receiving_effective = (effective + 1..path.len())
                .rev()
                .find(|&i| self.local[&next].contains(&path[i]))
                .unwrap();
            let normalized_rank =
                digit_path(self.slots, path[receiving_effective], candidate).len() - 1;
            assert!(normalized_rank <= received_rank && normalized_rank < rank);
            if hops.is_empty() {
                final_peer = self.neighbors[&node].get(&candidate) == Some(&next);
            }
            hops.push((node, next));
            position = effective + 1;
            node = next;
            arrivals.push((node, position, false));
        }
        ModelRoute {
            source,
            owner,
            attempt,
            path,
            arrivals,
            hops,
            final_peer,
            origin: node,
        }
    }
}

#[derive(Clone, Copy)]
pub(in crate::runtime) enum RouteOutcome {
    /// A cold success must witness origin, every hop, and every logical arrival
    /// of this candidate. Coverage is scope-wide, not per concurrent request.
    Cold(u32),
    /// No upstream work is required for a completed cache hit.
    Cached(u32),
    /// This scope must not access any origin.
    Failed,
}

/// A marker makes bounded-history loss fail closed. Each scope is finalized
/// before another scope for the same target begins; no scheduler work is added.
/// Transport observations lack cursor IDs, so they are checked against the
/// declared route set, not paired with the most recently observed cursor.
#[must_use = "finish the scoped independent routing check"]
pub(in crate::runtime) struct RouteScope {
    target: String,
    hits: usize,
}
impl RouteScope {
    pub(in crate::runtime) fn begin(
        world: &crate::simulation::World,
        target: &str,
        hits: usize,
    ) -> Self {
        world.event("routing-oracle-scope", target, "begin");
        Self {
            target: target.into(),
            hits,
        }
    }

    pub(in crate::runtime) fn check(
        self,
        placement: &Placement,
        ingress: usize,
        attempts: &[u32],
        outcome: RouteOutcome,
        world: &crate::simulation::World,
        hits: &[(usize, String)],
    ) {
        let events = world.events();
        let start = events
            .iter()
            .rposition(|e| e.kind == "routing-oracle-scope" && e.target == self.target)
            .expect("routing oracle scope fell out of retained history");
        self.check_observations(
            placement,
            ingress,
            attempts,
            outcome,
            &events[start + 1..],
            hits,
        );
    }

    fn check_observations(
        &self,
        placement: &Placement,
        ingress: usize,
        attempts: &[u32],
        outcome: RouteOutcome,
        events: &[crate::simulation::Event],
        hits: &[(usize, String)],
    ) {
        assert!(!attempts.is_empty());
        assert!(attempts.windows(2).all(|a| a[0] < a[1]));
        // These scoped fixtures all retain the production default logical cap.
        assert!(attempts.iter().all(|&a| a < placement.slots.min(3)));
        let routes: Vec<_> = attempts
            .iter()
            .map(|&a| placement.route(ingress, &self.target, a))
            .collect();
        let mut seen_routes = 0;
        let mut seen_arrivals = BTreeSet::new();
        // Each ingress route starts a candidate chain, including repeated
        // attempt-zero requests in a HEAD/GET or concurrent-worker scope. A
        // transition consumes one predecessor and creates one successor. The
        // observations have no request ID: this is existential chain matching
        // within one observation node/worker/incarnation, not causal pairing.
        let mut reachable: BTreeMap<(usize, u32, u64, u32), usize> = BTreeMap::new();
        let mut seen_hops = BTreeSet::new();
        for event in events.iter().filter(|e| e.target == self.target) {
            match event.kind {
                "route" => {
                    let node = placement.workers[&event.node.unwrap()];
                    let (route, arrival) = routes
                        .iter()
                        .find_map(|r| {
                            r.arrivals.iter().find(|&&(n, position, origin)| {
                                n == node && event.detail == format!(
                                    "source={} owner={} attempt={} position={position} origin={origin}",
                                    r.source, r.owner, r.attempt)
                            }).map(|&arrival| (r, arrival))
                        })
                        .unwrap_or_else(|| {
                            panic!("unexpected logical route: {event:?}; expected {routes:?}")
                        });
                    seen_arrivals.insert((route.attempt, arrival));
                    if arrival.2 {
                        *reachable
                            .entry((
                                event.node.unwrap(),
                                event.worker,
                                event.incarnation,
                                route.attempt,
                            ))
                            .or_default() += 1;
                    }
                    seen_routes += 1;
                }
                "transport-http" | "transport-rdma" => {
                    let source = placement.workers[&event.node.unwrap()];
                    let endpoint: SocketAddr = event
                        .detail
                        .strip_prefix("endpoint=")
                        .unwrap()
                        .parse()
                        .unwrap();
                    let destination = placement.endpoints[&endpoint];
                    assert!(
                        routes
                            .iter()
                            .any(|r| r.hops.contains(&(source, destination))),
                        "off-path transport: {event:?}; expected {routes:?}"
                    );
                    seen_hops.insert((source, destination));
                }
                "candidate" => {
                    assert_eq!(
                        placement.workers[&event.node.unwrap()],
                        placement.workers[&ingress]
                    );
                    let (previous, next) = routes
                        .iter()
                        .find_map(|next| {
                            routes
                                .iter()
                                .find(|previous| {
                                    previous.attempt < next.attempt
                                        && event.detail
                                            == format!(
                                                "owner={} next={} attempt={}",
                                                (previous.owner + previous.attempt)
                                                    % placement.slots,
                                                (next.owner + next.attempt) % placement.slots,
                                                next.attempt
                                            )
                                })
                                .map(|previous| (previous, next))
                        })
                        .unwrap_or_else(|| panic!("unauthorized candidate: {event:?}"));
                    let key = |attempt| {
                        (
                            event.node.unwrap(),
                            event.worker,
                            event.incarnation,
                            attempt,
                        )
                    };
                    let predecessor = reachable
                        .get_mut(&key(previous.attempt))
                        .filter(|count| **count > 0)
                        .unwrap_or_else(|| {
                            panic!("candidate predecessor was not reachable: {event:?}")
                        });
                    *predecessor -= 1;
                    *reachable.entry(key(next.attempt)).or_default() += 1;
                    // Fallback activates the ingress locally without another
                    // route event. This witnesses only its ingress arrival;
                    // every remote receiver must still emit its own route.
                    seen_arrivals.insert((next.attempt, next.arrivals[0]));
                }
                _ => {}
            }
        }
        assert!(
            seen_routes > 0,
            "scope did not witness routing for {}",
            self.target
        );
        let actual: Vec<_> = hits[self.hits..]
            .iter()
            .filter(|(_, t)| *t == self.target)
            .collect();
        match outcome {
            RouteOutcome::Failed => assert!(actual.is_empty(), "failed scope accessed origin"),
            RouteOutcome::Cold(attempt) | RouteOutcome::Cached(attempt) => {
                let route = routes.iter().find(|r| r.attempt == attempt).unwrap();
                assert!(
                    actual
                        .iter()
                        .all(|(node, _)| placement.workers[node] == route.origin),
                    "wrong origin for {}: {actual:?}",
                    self.target
                );
                if matches!(outcome, RouteOutcome::Cold(_)) {
                    assert!(
                        route
                            .arrivals
                            .iter()
                            .all(|arrival| seen_arrivals.contains(&(attempt, *arrival))),
                        "cold scope omitted expected logical arrivals: {route:?}"
                    );
                    assert!(!actual.is_empty(), "cold scope has no origin witness");
                    assert!(
                        route.hops.iter().all(|edge| seen_hops.contains(edge)),
                        "cold scope omitted expected physical hops: {route:?}"
                    );
                } else {
                    assert!(actual.is_empty(), "cached scope accessed origin");
                    assert!(seen_hops.is_empty(), "cached scope used peer transport");
                }
            }
        }
    }
}

#[test]
fn independent_placement_digit_words_and_noncontiguous_shortcuts() {
    assert_eq!(digit_path(8, 4, 3), [4, 1, 3]);
    assert_eq!(digit_path(8, 0, 7), [0, 1, 3, 7]);
    assert_eq!(digit_path(8, 0, 0), [0]);
    // Non-power geometry exercises the lexicographic shortest-word tie-break.
    assert_eq!(digit_path(5, 4, 1), [4, 3, 1]);
    assert_eq!(digit_path(131072, 0, 131071), [0, 50, 2570, 131071]);
    let placement = Placement::new(
        8,
        vec![(0, vec![0, 3]), (1, vec![1, 2, 4, 5, 6]), (7, vec![7])],
    );
    let target = model_target(&placement, 7);
    let route = placement.route(0, &target, 0);
    assert_eq!(route.path, [0, 1, 3, 7]);
    assert_eq!(route.arrivals, [(0, 0, true), (7, 3, false)]);
    assert_eq!(route.hops, [(0, 7)]);
    assert!(route.final_peer);
    assert_eq!(route.origin, 7);
}

fn model_target(placement: &Placement, owner: u32) -> String {
    (0..10000)
        .map(|i| format!("/independent-oracle/{i}"))
        .find(|t| placement.owner(t) == owner)
        .unwrap()
}

#[test]
fn independent_placement_colocation_aliases_and_logical_cap() {
    let mut placement = Placement::new(8, vec![(0, (0..4).collect()), (4, (4..8).collect())]);
    placement.alias(8, 4, "127.0.0.1:12000".parse().unwrap());
    let target = model_target(&placement, 3);
    let route = placement.route(8, &target, 0);
    assert_eq!(route.source, 4);
    assert_eq!(route.owner, 3);
    assert_eq!(route.path, [4, 1, 3]);
    assert_eq!(route.arrivals, [(4, 0, true), (0, 1, false)]);
    assert_eq!(route.hops, [(4, 0)]);
    assert!(route.final_peer);
    assert_eq!(route.origin, 0);
    let fallback = placement.route(8, &target, 1);
    assert!(fallback.hops.is_empty());
    assert_eq!(fallback.origin, 4);
    let target = model_target(&placement, 1);
    for attempt in 0..3 {
        assert_eq!(placement.route(8, &target, attempt).origin, 0);
    }
    assert_eq!(placement.route(8, &target, 3).origin, 4);
    let wrap = model_target(&placement, 7);
    assert_eq!(*placement.route(8, &wrap, 1).path.last().unwrap(), 0);
    let all_local = Placement::new(8, vec![(3, (0..8).collect())]);
    let target = model_target(&all_local, 7);
    let route = all_local.route(3, &target, 0);
    assert_eq!(route.source, 0);
    assert_eq!(route.path, [0, 1, 3, 7]);
    assert!(route.hops.is_empty());
    assert_eq!(route.origin, 3);
}

#[test]
fn independent_scope_rejects_wrong_routes_transports_origins_and_missing_witnesses() {
    use crate::simulation::Event;
    let placement = Placement::new(8, vec![(0, (0..4).collect()), (4, (4..8).collect())]);
    let target = model_target(&placement, 3);
    let scope = RouteScope {
        target: target.clone(),
        hits: 0,
    };
    let event = |node, kind, detail: &str| Event {
        tick: 0,
        node: Some(node),
        worker: 0,
        incarnation: 0,
        kind,
        target: target.clone(),
        detail: detail.into(),
        flight: None,
        depends_on: None,
        key: None,
    };
    let events = vec![
        event(
            4,
            "route",
            "source=4 owner=3 attempt=0 position=0 origin=true",
        ),
        event(4, "transport-http", "endpoint=127.0.0.1:10000"),
        event(
            0,
            "route",
            "source=4 owner=3 attempt=0 position=1 origin=false",
        ),
        event(4, "transport-rdma", "endpoint=127.0.0.1:10000"),
    ];
    let hits = vec![(0, target.clone())];
    let check = |events: &[Event], hits: &[(usize, String)], attempts: &[u32]| {
        scope.check_observations(&placement, 4, attempts, RouteOutcome::Cold(0), events, hits);
    };
    check(&events, &hits, &[0]);
    let rejects = |events: &[Event], hits: &[(usize, String)], attempts: &[u32]| {
        assert!(std::panic::catch_unwind(|| check(events, hits, attempts)).is_err());
    };
    for detail in [
        "source=4 owner=2 attempt=0 position=1 origin=false",
        "source=4 owner=3 attempt=0 position=2 origin=false",
        "source=4 owner=3 attempt=1 position=1 origin=false",
    ] {
        let mut bad = events.clone();
        bad[2].detail = detail.into();
        rejects(&bad, &hits, &[0]);
    }
    for index in [1, 3] {
        let mut bad = events.clone();
        bad[index].detail = "endpoint=127.0.0.1:10004".into();
        rejects(&bad, &hits, &[0]);
    }
    rejects(&events, &[(4, target.clone())], &[0]);
    rejects(&events, &[], &[0]);
    rejects(&[], &hits, &[0]);
    rejects(&events, &hits, &[0, 3]);
    let mut bad = events.clone();
    bad.push(event(4, "candidate", "owner=3 next=4 attempt=1"));
    rejects(&bad, &hits, &[0]);
    let no_hops: Vec<_> = events
        .iter()
        .filter(|e| e.kind == "route")
        .cloned()
        .collect();
    rejects(&no_hops, &hits, &[0]);
    let no_receiver: Vec<_> = events
        .iter()
        .enumerate()
        .filter(|(index, _)| *index != 2)
        .map(|(_, event)| event.clone())
        .collect();
    rejects(&no_receiver, &hits, &[0]);
    rejects(&events[1..], &hits, &[0]);
    scope.check_observations(&placement, 4, &[0], RouteOutcome::Failed, &events, &[]);
    assert!(
        std::panic::catch_unwind(|| {
            scope.check_observations(&placement, 4, &[0], RouteOutcome::Failed, &events, &hits);
        })
        .is_err()
    );
    let fallback = vec![
        events[0].clone(),
        event(4, "candidate", "owner=3 next=4 attempt=1"),
    ];
    scope.check_observations(
        &placement,
        4,
        &[0, 1],
        RouteOutcome::Cold(1),
        &fallback,
        &[(4, target.clone())],
    );
    let candidate_chain = |events: &[Event]| {
        scope.check_observations(&placement, 4, &[0, 1, 2], RouteOutcome::Failed, events, &[]);
    };
    let advance_one = event(4, "candidate", "owner=3 next=4 attempt=1");
    let advance_two = event(4, "candidate", "owner=4 next=5 attempt=2");
    let skip_one = event(4, "candidate", "owner=3 next=5 attempt=2");
    // A blocked intermediate candidate need not emit a separate transition.
    candidate_chain(&[events[0].clone(), skip_one.clone()]);
    candidate_chain(&[events[0].clone(), advance_one.clone(), advance_two.clone()]);
    // Repeated ingress events explicitly authorize independent chains. Their
    // interleaving is checked existentially, never by a latest-cursor guess.
    candidate_chain(&[
        events[0].clone(),
        events[0].clone(),
        advance_one.clone(),
        advance_two.clone(),
        skip_one.clone(),
    ]);
    candidate_chain(&[
        events[0].clone(),
        advance_one.clone(),
        advance_two.clone(),
        events[0].clone(),
        skip_one.clone(),
    ]);
    // Admission can begin at a nonzero attempt when earlier slots are blocked.
    candidate_chain(&[
        event(
            4,
            "route",
            "source=4 owner=3 attempt=1 position=0 origin=true",
        ),
        advance_two.clone(),
    ]);
    for bad in [
        // Declaring attempt 1 is not evidence that its candidate was reached.
        vec![events[0].clone(), advance_two.clone()],
        // A later observation cannot retroactively authorize its predecessor.
        vec![events[0].clone(), advance_two.clone(), advance_one.clone()],
        // Once consumed, attempt 0 cannot advance again without a new ingress.
        vec![events[0].clone(), advance_one.clone(), skip_one.clone()],
        vec![events[0].clone(), skip_one, advance_one.clone()],
        vec![
            events[0].clone(),
            advance_one,
            event(4, "candidate", "owner=4 next=3 attempt=0"),
        ],
        // A receiver route cannot seed an ingress candidate chain.
        vec![
            events[2].clone(),
            event(4, "candidate", "owner=3 next=4 attempt=1"),
        ],
    ] {
        assert!(std::panic::catch_unwind(|| candidate_chain(&bad)).is_err());
    }
    for (worker, incarnation) in [(1, 0), (0, 1)] {
        let mut foreign = event(4, "candidate", "owner=3 next=4 attempt=1");
        foreign.worker = worker;
        foreign.incarnation = incarnation;
        assert!(
            std::panic::catch_unwind(|| {
                candidate_chain(&[events[0].clone(), foreign.clone()]);
            })
            .is_err()
        );
    }
    scope.check_observations(
        &placement,
        4,
        &[0],
        RouteOutcome::Cached(0),
        &events[..1],
        &[],
    );
    assert!(
        std::panic::catch_unwind(|| {
            scope.check_observations(&placement, 4, &[0], RouteOutcome::Cached(0), &events, &[]);
        })
        .is_err()
    );
}

#[test]
fn independent_placement_rejects_incomplete_and_conflicting_publications() {
    for owners in [vec![(0, vec![0])], vec![(0, vec![0]), (1, vec![0, 1])]] {
        assert!(std::panic::catch_unwind(|| Placement::new(2, owners)).is_err());
    }
    let placement = Placement::new(2, vec![(0, vec![0]), (1, vec![1])]);
    placement.assert_neighbors(0, [(1, 1)].into());
    assert!(std::panic::catch_unwind(|| placement.assert_neighbors(0, [(1, 0)].into())).is_err());
}

#[derive(Clone, Copy, Debug, serde::Serialize)]
pub(super) enum Oracle {
    HealthyRecovery,
    HttpGraph,
    HttpRank,
    RdmaRank,
    OriginPlacement,
}

const ALL: [Oracle; 5] = [
    Oracle::HealthyRecovery,
    Oracle::HttpGraph,
    Oracle::HttpRank,
    Oracle::RdmaRank,
    Oracle::OriginPlacement,
];

#[derive(Clone, Copy, serde::Serialize)]
pub(in crate::runtime) enum Check {
    IndependentCanonical,
    FixtureReplacement {
        reason: &'static str,
        check: &'static str,
        scope: &'static str,
        independence: &'static str,
    },
}

#[derive(Clone, Copy, serde::Serialize)]
pub(in crate::runtime) struct Capabilities(pub(in crate::runtime) [Check; 5]);

impl Capabilities {
    pub(super) const CANONICAL: Self = Self([Check::IndependentCanonical; 5]);

    pub(super) fn validate(self) {
        for check in self.0 {
            if let Check::FixtureReplacement {
                reason,
                check,
                scope,
                independence,
            } = check
            {
                assert!(
                    [reason, check, scope, independence]
                        .iter()
                        .all(|field| !field.trim().is_empty()),
                    "oracle replacement requires reason, check, scope, and independence"
                );
            }
        }
    }

    pub(super) fn canonical(self, oracle: Oracle) -> bool {
        matches!(self.0[oracle as usize], Check::IndependentCanonical)
    }

    pub(super) fn declare(self, world: &crate::simulation::World) {
        self.validate();
        for (oracle, check) in ALL.into_iter().zip(self.0) {
            world.observation(crate::simulation::history::Transition::OracleCapability {
                oracle: format!("{oracle:?}"),
                declaration: serde_json::to_value(check).unwrap(),
            });
        }
    }
}

#[test]
fn replacements_are_scoped_per_oracle_and_require_explanations() {
    let mut capabilities = Capabilities::CANONICAL;
    capabilities.0[Oracle::HttpRank as usize] = Check::FixtureReplacement {
        reason: "colocated logical slots",
        check: "logical cursor transition assertions",
        scope: "B03 routed requests",
        independence: "fixture expectations plus production cursor validation",
    };
    capabilities.validate();
    assert!(!capabilities.canonical(Oracle::HttpRank));
    for oracle in [
        Oracle::HealthyRecovery,
        Oracle::HttpGraph,
        Oracle::RdmaRank,
        Oracle::OriginPlacement,
    ] {
        assert!(capabilities.canonical(oracle));
    }
    capabilities.0[0] = Check::FixtureReplacement {
        reason: "",
        check: "",
        scope: "",
        independence: "",
    };
    assert!(std::panic::catch_unwind(|| capabilities.validate()).is_err());
}
