// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Proptest invariants and a fixed-seed smoke scenario for the p2p
//! subsystem under the DST executor. Non-DST coverage of routing
//! lives in module unit/integration tests under `src/p2p/`.

use std::collections::HashSet;

use proptest::prelude::*;
use unbounded_storage::p2p::{NodeId, RingId, splitmix64};

use crate::p2p::mocks::{P2pSimCfg, RouteOutcome, SimCluster};
use crate::p2p::workload::{
    ClientSpec, RunReport, Workload, build_peers, run_workload, workload_strategy,
};

// ---------------------------------------------------------------------------
// Invariants.
// ---------------------------------------------------------------------------

/// Strict identity check: every completed client's terminal must
/// equal the reference successor of `target` on the ring (the
/// `naive_destination` the workload recorded when it spawned the
/// client). After the Phase 2 routing fix, `next_hop` implements
/// Chord `find_successor` correctly and the closest-preceding-finger
/// chain always terminates at the actual successor of the target,
/// regardless of finger sparsity or arc collisions. This invariant
/// pins that down: no exceptions, no waivers. Any divergence between
/// `terminal` and the recorded successor is a routing bug.
///
/// We source the reference successor from
/// `report.outcomes[i].naive_destination` (recorded at spawn time
/// from the same cluster view the client routed over) rather than
/// recomputing it here, so the assertion compares against what the
/// run actually saw.
///
/// Faulted runs (`hop_fault_rate > 0`) may legitimately produce
/// `completed: false` outcomes through retry exhaustion; those are
/// skipped here and bounded separately by
/// `assert_completed_under_no_faults` and
/// `assert_incomplete_fraction_bounded`.
fn assert_progress_or_destination(report: &RunReport) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        if !o.completed {
            continue;
        }
        prop_assert_eq!(
            o.terminal,
            o.naive_destination,
            "client {} from {:?} targeting {:?} terminated at {:?}, expected successor {:?}",
            i,
            o.start,
            o.target,
            o.terminal,
            o.naive_destination,
        );
    }
    Ok(())
}

/// With faults disabled every client must reach a terminator.
/// Pins the strict identity check above to a regime where retry
/// exhaustion cannot mask bugs by turning a misroute into an
/// incomplete outcome.
fn assert_completed_under_no_faults(report: &RunReport, w: &Workload) -> Result<(), TestCaseError> {
    if w.hop_fault_rate > 0 {
        return Ok(());
    }
    for (i, o) in report.outcomes.iter().enumerate() {
        prop_assert!(
            o.completed,
            "client {} from {:?} targeting {:?} failed to complete with no faults",
            i,
            o.start,
            o.target,
        );
    }
    Ok(())
}

/// Sanity bound on the incomplete fraction under faults. Catches a
/// fully-broken router that fails every lookup; does NOT enforce a
/// tight reliability target.
///
/// Reasoning: the workload retries up to `4 * (peer_count + 8)`
/// total hops per client. The maximum strategy fault rate is
/// `100/1000 = 10%`; for the largest peer counts (~64) and worst
/// fault rate, retries amply cover the real path of `<= peer_count
/// + 8 <= 72` hops in expectation. Realistically the great majority
/// of clients complete. We allow up to half the clients to fail as
/// headroom for path-length variance and unlucky fault streaks; a
/// router that drops EVERY lookup will still trip this bound when
/// the client population is at least 2.
fn assert_incomplete_fraction_bounded(
    report: &RunReport,
    w: &Workload,
) -> Result<(), TestCaseError> {
    if w.hop_fault_rate == 0 {
        // Strict zero-fault coverage is handled by
        // assert_completed_under_no_faults.
        return Ok(());
    }
    let total = report.outcomes.len();
    if total == 0 {
        return Ok(());
    }
    let incomplete = report.outcomes.iter().filter(|o| !o.completed).count();
    let bound = (total / 2).max(1);
    prop_assert!(
        incomplete <= bound,
        "incomplete clients {} exceeded sanity bound {} (total={}, fault_rate={}/1000)",
        incomplete,
        bound,
        total,
        w.hop_fault_rate,
    );
    Ok(())
}

/// No client revisits a node. With the cycle guard in the workload
/// this is recorded as a path where every entry is unique.
fn assert_no_cycle(report: &RunReport) -> Result<(), TestCaseError> {
    for (i, o) in report.outcomes.iter().enumerate() {
        let mut seen: HashSet<NodeId> = HashSet::new();
        for n in &o.path {
            prop_assert!(
                seen.insert(*n),
                "client {} revisited {:?} on path {:?}",
                i,
                n,
                o.path,
            );
        }
    }
    Ok(())
}

/// A generous upper bound on hop counts. With faults disabled the
/// workload can take at most one hop per distinct peer; with
/// faults we allow a healthy multiplier because a fault stalls
/// progress without advancing the path.
fn assert_hops_bounded(report: &RunReport, faults_enabled: bool) -> Result<(), TestCaseError> {
    let multiplier = if faults_enabled { 5 } else { 1 };
    let cap = (report.peer_count as u32 + 8) * multiplier;
    for (i, o) in report.outcomes.iter().enumerate() {
        prop_assert!(
            o.hops <= cap,
            "client {} hops={} exceeded cap={} (peers={})",
            i,
            o.hops,
            cap,
            report.peer_count,
        );
    }
    Ok(())
}

// ---------------------------------------------------------------------------
// Proptest entry.
// ---------------------------------------------------------------------------

/// Reduce a [`RouteOutcome`] to a comparable key (variant tag,
/// terminal node, hop count) so two clusters' routing decisions can
/// be compared for exact equality.
fn route_key(outcome: &RouteOutcome) -> (u8, NodeId, u32) {
    match outcome {
        RouteOutcome::Reached { hops, terminal } => (0, *terminal, *hops),
        RouteOutcome::Stalled { hops, terminal } => (1, *terminal, *hops),
        RouteOutcome::OutOfBudget { hops, terminal } => (2, *terminal, *hops),
    }
}
proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn invariant_suite(seed in any::<u64>(), w in workload_strategy()) {
        let faults_enabled = w.hop_fault_rate > 0;
        let w_for_assert = w.clone();
        let report = run_workload(seed, w).expect("run completed");
        assert_progress_or_destination(&report)?;
        assert_completed_under_no_faults(&report, &w_for_assert)?;
        assert_incomplete_fraction_bounded(&report, &w_for_assert)?;
        assert_no_cycle(&report)?;
        assert_hops_bounded(&report, faults_enabled)?;
    }

    /// Disjoint-discovery same-path guard. A cluster where every node
    /// is configured with ONLY its selected neighbors (via
    /// `FingerTable::from_explicit`) must route every lookup along the
    /// byte-identical path as the globally-built cluster. We assert
    /// this directly: for every (start node, target) pair the
    /// fault-free reference walk produces the same terminal and the
    /// same hop count under both constructions. This is the core
    /// correctness claim of disjoint peer discovery - same path
    /// through the p2p topology without global knowledge.
    #[test]
    fn disjoint_routing_matches_global(w in workload_strategy()) {
        let peers = build_peers(&w);
        prop_assume!(!peers.is_empty());

        let global = SimCluster::new(peers.clone(), w.k, P2pSimCfg::new());
        let disjoint = SimCluster::new_disjoint(peers.clone(), w.k, P2pSimCfg::new());
        let cap = w.peer_count as u32 + 8;

        // Sweep a spread of ring targets (each peer's own ring id plus
        // splitmix-derived positions between them) from every start.
        let mut targets: Vec<RingId> = peers.iter().map(|p| p.ring).collect();
        for i in 0..(w.peer_count as u64 * 4) {
            targets.push(RingId(splitmix64(i.wrapping_mul(0x100_0001))));
        }

        for start in peers.iter().map(|p| p.node) {
            for &target in &targets {
                let g = route_key(&global.route(start, target, cap));
                let d = route_key(&disjoint.route(start, target, cap));
                prop_assert_eq!(
                    g,
                    d,
                    "routing diverged from {:?} to {:?}: global={:?} disjoint={:?}",
                    start,
                    target,
                    g,
                    d,
                );
            }
        }
    }
}

// ---------------------------------------------------------------------------
// Smoke scenario.
// ---------------------------------------------------------------------------

/// Fixed-seed run that gives a fast pass/fail signal without
/// paying the full proptest cost. The strict per-client routing
/// invariants are enforced by `invariant_suite` across the workload
/// space; here we only assert the basics that prove the executor
/// wired the clients through to completion.
#[test]
fn smoke() {
    let w = Workload {
        peer_count: 8,
        topology_groups: 2,
        tag_depth: 2,
        k: 4,
        max_hop_delay: 1,
        hop_fault_rate: 0,
        key_count: 2,
        clients: vec![
            ClientSpec {
                start_node_idx: 0,
                key_idx: 0,
            },
            ClientSpec {
                start_node_idx: 1,
                key_idx: 1,
            },
            ClientSpec {
                start_node_idx: 2,
                key_idx: 0,
            },
        ],
    };
    let report = run_workload(0xC0FFEE, w).expect("smoke run");
    assert_eq!(report.outcomes.len(), 3);
    for o in &report.outcomes {
        assert!(o.completed, "expected completion with no faults: {o:?}");
    }
}
