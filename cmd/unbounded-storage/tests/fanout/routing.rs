// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! NUMA-local fetch-routing DST (Phase 4).
//!
//! This is pure placement math: given a synthetic multi-NUMA topology
//! (a serving-shard set with per-shard NUMA nodes plus a drive -> NUMA
//! table), route many random stripe keys through [`numa_local_shard`]
//! and assert the NUMA-locality invariants and the documented fallback.
//!
//! Unlike the fetch/pin-lifetime workload in this module's `workload.rs`,
//! routing has no async I/O to interleave, so there are no tasks and no
//! `Executor::run`. The executor is used only as the sanctioned source
//! of seeded randomness ([`Executor::sim_mut`]) so a `(seed, workload)`
//! pair still fully determines the run, matching the rest of the DST
//! harness.
//!
//! [`numa_local_shard`]: unbounded_storage::fanout::numa_local_shard
//! [`Executor::sim_mut`]: crate::framework::executor::Executor::sim_mut

use proptest::collection::vec;
use proptest::prelude::*;
use rand::RngCore;
use unbounded_storage::bufferpool::StripeKey;
use unbounded_storage::fanout::{NumaShardTable, ShardPick, numa_local_shard, owner_shard};
use unbounded_storage::storage::disk_for;

use crate::framework::executor::Executor;

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

/// A synthetic fan-out topology plus a probe count. The shard set and
/// the drive table are modeled independently so the strategy can
/// reliably produce drives whose NUMA node has no serving shard (which
/// forces the cross-NUMA fallback) as well as fully NUMA-local layouts.
#[derive(Clone, Debug)]
pub struct RoutingWorkload {
    /// NUMA node of each serving shard. The length is the shard count
    /// (always >= 1). `Some(n)` pins the shard to node `n`; `None`
    /// models a shard whose node is unknown, which therefore belongs to
    /// no node bucket but is still reachable via the spread fallback.
    pub shard_nodes: Vec<Option<u16>>,
    /// Drive -> NUMA node table, aligned with the path-sorted channel
    /// vector exactly as [`DiskChannelDirectory::drive_numa`] publishes
    /// it. Empty models a box with no open disks (or none pinned to a
    /// node), which routes through the topology-free spread. `Some(n)`
    /// is the node of the drive's storage core; `None` is an unpinned
    /// drive.
    ///
    /// [`DiskChannelDirectory::drive_numa`]: unbounded_storage::storage::disks::DiskChannelDirectory
    pub drive_numa: Vec<Option<u16>>,
    /// Number of random `(key, stripe_off)` pairs to route.
    pub num_probes: usize,
}

impl RoutingWorkload {
    fn shard_count(&self) -> usize {
        self.shard_nodes.len().max(1)
    }

    /// Rebuild the routing table the driver feeds to `numa_local_shard`.
    fn table(&self) -> NumaShardTable {
        NumaShardTable::from_shards(self.shard_nodes.iter().copied().enumerate())
    }
}

// ---------------------------------------------------------------------------
// Proptest strategy.
// ---------------------------------------------------------------------------

/// Shard nodes are drawn from `0..=3`; drive nodes from `0..=4`. Node
/// `4` therefore never owns a serving shard, guaranteeing the strategy
/// exercises the "drive on a node with no shard" fallback. `None`
/// entries on both sides exercise the unpinned-shard and unpinned-drive
/// paths, and an empty `drive_numa` exercises the no-topology spread.
pub fn routing_workload_strategy() -> impl Strategy<Value = RoutingWorkload> {
    let shard_node = prop_oneof![
        4 => (0u16..=3).prop_map(Some),
        1 => Just(None::<u16>),
    ];
    let drive_node = prop_oneof![
        4 => (0u16..=4).prop_map(Some),
        1 => Just(None::<u16>),
    ];

    let shard_nodes = vec(shard_node, 1..=8);
    let drive_numa = vec(drive_node, 0..=6);
    let num_probes = 1usize..=64;

    (shard_nodes, drive_numa, num_probes).prop_map(|(shard_nodes, drive_numa, num_probes)| {
        RoutingWorkload {
            shard_nodes,
            drive_numa,
            num_probes,
        }
    })
}

// ---------------------------------------------------------------------------
// Outcomes and report.
// ---------------------------------------------------------------------------

/// One routed probe: the random inputs and the resulting pick.
#[derive(Clone, Debug)]
pub struct Probe {
    pub key: [u8; 32],
    pub stripe_off: u64,
    pub pick: ShardPick,
}

/// What the driver reports back to the property tests. Carries the
/// topology so each invariant can recompute the expected placement
/// from first principles rather than trusting the driver.
#[derive(Debug)]
pub struct RoutingReport {
    pub shard_nodes: Vec<Option<u16>>,
    pub drive_numa: Vec<Option<u16>>,
    pub shard_count: usize,
    pub probes: Vec<Probe>,
    /// Probes whose pick stayed on the drive's NUMA node.
    pub local_count: usize,
    /// Probes whose pick had to leave the drive's NUMA node (the
    /// fallback spread). Reported so a test can confirm the fallback was
    /// actually exercised across the sweep.
    pub cross_numa_count: usize,
}

// ---------------------------------------------------------------------------
// Driver.
// ---------------------------------------------------------------------------

/// Route `w.num_probes` random stripes under `seed` and collect the
/// picks. Randomness is drawn from the framework executor's seeded PRNG
/// so the run is fully determined by `(seed, w)`.
pub fn run_routing(seed: u64, w: RoutingWorkload) -> RoutingReport {
    let shard_count = w.shard_count();
    let table = w.table();

    let exec = Executor::new(seed);
    let mut probes = Vec::with_capacity(w.num_probes);
    let mut local_count = 0usize;
    let mut cross_numa_count = 0usize;

    {
        let mut sim = exec.sim_mut();
        for _ in 0..w.num_probes {
            let mut key = [0u8; 32];
            sim.rng.fill_bytes(&mut key);
            let mut off_bytes = [0u8; 8];
            sim.rng.fill_bytes(&mut off_bytes);
            let stripe_off = u64::from_le_bytes(off_bytes);

            let pick = numa_local_shard(&StripeKey(key), stripe_off, &w.drive_numa, &table);
            if pick.cross_numa {
                cross_numa_count += 1;
            } else {
                local_count += 1;
            }

            probes.push(Probe {
                key,
                stripe_off,
                pick,
            });
        }
    }

    RoutingReport {
        shard_nodes: w.shard_nodes,
        drive_numa: w.drive_numa,
        shard_count,
        probes,
        local_count,
        cross_numa_count,
    }
}

// ---------------------------------------------------------------------------
// Property tests.
// ---------------------------------------------------------------------------

proptest! {
    #![proptest_config(ProptestConfig {
        cases: 256,
        ..ProptestConfig::default()
    })]

    #[test]
    fn routing_invariants(seed in any::<u64>(), w in routing_workload_strategy()) {
        let report = run_routing(seed, w);
        assert_pick_in_range(&report)?;
        assert_empty_topology_spreads(&report)?;
        assert_numa_local_when_node_has_shard(&report)?;
        assert_fallback_when_no_local_shard(&report)?;
        assert_deterministic(&report)?;
        assert_counts_consistent(&report)?;
    }
}

/// Invariant: every pick is a valid shard index.
///
/// Whether NUMA-local or fallen back, the chosen shard must be in
/// `0..shard_count`; the coordinator indexes its peer table with it.
fn assert_pick_in_range(report: &RoutingReport) -> Result<(), TestCaseError> {
    for (i, p) in report.probes.iter().enumerate() {
        prop_assert!(
            p.pick.shard < report.shard_count,
            "probe {} picked shard {} out of {} shards",
            i,
            p.pick.shard,
            report.shard_count,
        );
    }
    Ok(())
}

/// Invariant: with no drive topology, routing is the plain spread.
///
/// An empty `drive_numa` (no open disks, or none pinned to a node) has
/// nothing to be local to, so every pick must equal `owner_shard` over
/// all shards and must not be flagged cross-NUMA (there was no NUMA node
/// to miss).
fn assert_empty_topology_spreads(report: &RoutingReport) -> Result<(), TestCaseError> {
    if !report.drive_numa.is_empty() {
        return Ok(());
    }
    for (i, p) in report.probes.iter().enumerate() {
        let expected = owner_shard(&StripeKey(p.key), report.shard_count);
        prop_assert_eq!(
            p.pick.shard,
            expected,
            "probe {} on empty topology did not spread via owner_shard",
            i,
        );
        prop_assert!(
            !p.pick.cross_numa,
            "probe {} on empty topology flagged cross-NUMA",
            i,
        );
    }
    Ok(())
}

/// Invariant: when the backing drive's node has a serving shard, the
/// pick lands on that node and is not cross-NUMA.
///
/// Recompute the backing drive the local store would read
/// (`disk_for` over the same `drive_numa.len()`), read its node, and if
/// that node owns at least one shard, the pick must be on-node
/// (`shard_nodes[pick] == Some(node)`) and `cross_numa` must be false.
/// This is the core Phase 4 guarantee: disk read, checksum, and
/// `SEND_ZC` backing all stay on one NUMA node.
fn assert_numa_local_when_node_has_shard(report: &RoutingReport) -> Result<(), TestCaseError> {
    if report.drive_numa.is_empty() {
        return Ok(());
    }
    for (i, p) in report.probes.iter().enumerate() {
        let drive = disk_for(&StripeKey(p.key), p.stripe_off, report.drive_numa.len());
        let Some(node) = report.drive_numa[drive] else {
            continue;
        };
        if !node_has_shard(&report.shard_nodes, node) {
            continue;
        }
        prop_assert!(
            !p.pick.cross_numa,
            "probe {} on node {} (which has a shard) flagged cross-NUMA",
            i,
            node,
        );
        prop_assert_eq!(
            report.shard_nodes[p.pick.shard],
            Some(node),
            "probe {} picked shard {} which is not on the drive's node {}",
            i,
            p.pick.shard,
            node,
        );
    }
    Ok(())
}

/// Invariant: when locality cannot be honored, fall back to the spread
/// and flag the cross-NUMA hop.
///
/// If the backing drive is unpinned (`None`) or its node owns no serving
/// shard, the pick must equal `owner_shard` over all shards and
/// `cross_numa` must be true, so the cross-NUMA fetch metric fires.
fn assert_fallback_when_no_local_shard(report: &RoutingReport) -> Result<(), TestCaseError> {
    if report.drive_numa.is_empty() {
        return Ok(());
    }
    for (i, p) in report.probes.iter().enumerate() {
        let drive = disk_for(&StripeKey(p.key), p.stripe_off, report.drive_numa.len());
        let target = report.drive_numa[drive];
        let has_local = target.is_some_and(|node| node_has_shard(&report.shard_nodes, node));
        if has_local {
            continue;
        }
        prop_assert!(
            p.pick.cross_numa,
            "probe {} with no local shard did not flag cross-NUMA",
            i,
        );
        let expected = owner_shard(&StripeKey(p.key), report.shard_count);
        prop_assert_eq!(
            p.pick.shard,
            expected,
            "probe {} fallback did not spread via owner_shard",
            i,
        );
    }
    Ok(())
}

/// Invariant: routing is a pure function of its inputs.
///
/// Re-routing each probe's recorded `(key, stripe_off)` against the same
/// topology must reproduce the recorded pick exactly. Guards against any
/// hidden state in the routing path.
fn assert_deterministic(report: &RoutingReport) -> Result<(), TestCaseError> {
    let table = NumaShardTable::from_shards(report.shard_nodes.iter().copied().enumerate());
    for (i, p) in report.probes.iter().enumerate() {
        let again = numa_local_shard(&StripeKey(p.key), p.stripe_off, &report.drive_numa, &table);
        prop_assert_eq!(again, p.pick, "probe {} re-route disagreed", i);
    }
    Ok(())
}

/// Invariant: the local and cross-NUMA tallies partition the probes.
fn assert_counts_consistent(report: &RoutingReport) -> Result<(), TestCaseError> {
    prop_assert_eq!(
        report.local_count + report.cross_numa_count,
        report.probes.len(),
        "local + cross-NUMA counts do not sum to the probe count",
    );
    Ok(())
}

/// Whether any serving shard sits on node `n`. Mirrors the private
/// `NumaShardTable::shards_on` membership without reaching into it.
fn node_has_shard(shard_nodes: &[Option<u16>], n: u16) -> bool {
    shard_nodes.iter().any(|node| *node == Some(n))
}
