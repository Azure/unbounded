// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Workload model, proptest strategy, and the `run_workload` driver
//! that exercises recursive routing on a synthetic [`SimCluster`].

use std::cell::RefCell;
use std::rc::Rc;

use proptest::collection::vec;
use proptest::prelude::*;
use unbounded_storage::bufferpool::StripeKey;
use unbounded_storage::p2p::{NodeId, PeerEntry, RingId, TopologyTags, splitmix64, stripe_to_ring};

use crate::framework::executor::{Executor, RunError};
use crate::p2p::mocks::{P2pSimCfg, SimCluster};

// ---------------------------------------------------------------------------
// Workload model.
// ---------------------------------------------------------------------------

#[derive(Clone, Debug)]
pub struct Workload {
    pub peer_count: usize,
    pub topology_groups: usize,
    pub tag_depth: usize,
    pub k: u32,
    pub max_hop_delay: u32,
    pub hop_fault_rate: u32,
    pub key_count: usize,
    pub clients: Vec<ClientSpec>,
}

#[derive(Clone, Debug)]
pub struct ClientSpec {
    pub start_node_idx: usize,
    pub key_idx: usize,
}

// ---------------------------------------------------------------------------
// Outputs.
// ---------------------------------------------------------------------------

#[derive(Clone, Debug)]
#[allow(dead_code)]
pub struct ClientOutcome {
    pub start: NodeId,
    pub target: RingId,
    pub key: StripeKey,
    pub terminal: NodeId,
    pub hops: u32,
    pub completed: bool,
    pub path: Vec<NodeId>,
    /// Reference successor of `target` recorded at spawn time from
    /// the same cluster view the client routes over. Stored on the
    /// outcome rather than looked up later so invariants compare
    /// against what the run actually computed and stay correct
    /// regardless of the executor's task completion order.
    pub naive_destination: NodeId,
}

#[derive(Debug)]
#[allow(dead_code)]
pub struct RunReport {
    pub outcomes: Vec<ClientOutcome>,
    pub naive_destinations: Vec<NodeId>,
    pub steps: u64,
    pub peer_count: usize,
}

// ---------------------------------------------------------------------------
// Strategy.
// ---------------------------------------------------------------------------

pub fn workload_strategy() -> impl Strategy<Value = Workload> {
    // Bias `peer_count` toward larger populations so that
    // `log_k(peer_count)` is genuinely >= 2 for most cases, making
    // multi-hop routing the norm rather than the exception. A
    // (peer_count=32, k=4) cell, for example, gives log_4(32) = 2.5
    // expected hops; (peer_count=64, k=4) gives 3.
    let peer_count = prop_oneof![
        1 => 8usize..=16usize,
        2 => 17usize..=32usize,
        3 => 33usize..=48usize,
        2 => 49usize..=64usize,
    ];
    let topology_groups = 1usize..=4usize;
    let tag_depth = 1usize..=4usize;
    // Narrow `k` toward the small end so the per-node arc count is
    // low and the closest-preceding-finger chain has to take real
    // forward steps. Drop the k=32 outlier: with the new peer_count
    // floor of 8 it would frequently make log_k(N) < 2 again.
    let k = prop_oneof![
        4 => Just(4u32),
        3 => Just(8u32),
        2 => Just(16u32),
    ];
    let max_hop_delay = 0u32..=4u32;
    let hop_fault_rate = prop_oneof![
        8 => Just(0u32),
        2 => 1u32..=100u32,
    ];
    let key_count = 1usize..=8usize;
    let clients = vec(client_strategy(), 1..=6);

    (
        peer_count,
        topology_groups,
        tag_depth,
        k,
        max_hop_delay,
        hop_fault_rate,
        key_count,
        clients,
    )
        .prop_map(
            |(
                peer_count,
                topology_groups,
                tag_depth,
                k,
                max_hop_delay,
                hop_fault_rate,
                key_count,
                clients,
            )| Workload {
                peer_count,
                topology_groups,
                tag_depth,
                k,
                max_hop_delay,
                hop_fault_rate,
                key_count,
                clients,
            },
        )
}

fn client_strategy() -> impl Strategy<Value = ClientSpec> {
    (0usize..=255usize, 0usize..=255usize).prop_map(|(start_node_idx, key_idx)| ClientSpec {
        start_node_idx,
        key_idx,
    })
}

// ---------------------------------------------------------------------------
// Driver.
// ---------------------------------------------------------------------------

/// Build the initial peer list deterministically. Ring ids are
/// produced by `splitmix64(i)` so neighbours on the ring are not
/// adjacent in the input vector, exercising the finger table's arc
/// math instead of trivial linear walks.
pub fn build_peers(w: &Workload) -> Vec<PeerEntry> {
    let groups = w.topology_groups.max(1);
    let depth = w.tag_depth.max(1);
    let mut out = Vec::with_capacity(w.peer_count);
    for i in 0..w.peer_count {
        let ring = splitmix64(i as u64);
        let group = i % groups;
        let parts: Vec<String> = (0..depth)
            .map(|slot| match slot {
                0 => format!("g{group}"),
                1 => format!("z{group}-{}", i % 3),
                2 => format!("r{group}-{}", (i / 3) % 3),
                _ => format!("k{group}-{i}"),
            })
            .collect();
        out.push(PeerEntry {
            node: NodeId(i as u64),
            ring: RingId(ring),
            tags: TopologyTags(parts),
        });
    }
    out
}

fn stripe_key_for(idx: usize) -> StripeKey {
    // Use splitmix64 to spread keys across the ring; copy the
    // mixed bytes into the leading 8 of a 32-byte key. The trailing
    // bytes hold the index so that distinct logical keys never
    // collide even when splitmix64 hashes overlap on the leading 8.
    let mixed = splitmix64((idx as u64).wrapping_add(0x9E37_79B9_7F4A_7C15));
    let mut out = [0u8; 32];
    out[..8].copy_from_slice(&mixed.to_le_bytes());
    out[8..16].copy_from_slice(&(idx as u64).to_le_bytes());
    StripeKey(out)
}

pub fn run_workload(seed: u64, w: Workload) -> Result<RunReport, RunError> {
    let peers = build_peers(&w);
    if peers.is_empty() {
        return Ok(RunReport {
            outcomes: Vec::new(),
            naive_destinations: Vec::new(),
            steps: 0,
            peer_count: 0,
        });
    }

    let cfg = P2pSimCfg::new();
    cfg.max_hop_delay.set(w.max_hop_delay);
    cfg.hop_fault_rate.set(w.hop_fault_rate);

    let cluster = Rc::new(SimCluster::new(peers.clone(), w.k, cfg.clone()));

    let outcomes: Rc<RefCell<Vec<ClientOutcome>>> = Rc::new(RefCell::new(Vec::new()));

    let mut exec = Executor::new(seed);

    // Hop budget for a single client: bounded forward walk plus a
    // small slack for fault-induced no-progress hops. Per-client
    // max hops is `peer_count` (a clean walk can't visit more
    // distinct nodes than exist).
    let per_client_hop_cap = (w.peer_count as u32).saturating_add(8);
    let fault_retry_cap = if w.hop_fault_rate > 0 {
        per_client_hop_cap.saturating_mul(4)
    } else {
        per_client_hop_cap
    };

    let mut naive_destinations = Vec::with_capacity(w.clients.len());

    for c in &w.clients {
        let start = peers[c.start_node_idx % peers.len()].node;
        let key = stripe_key_for(c.key_idx % w.key_count.max(1));
        let target = stripe_to_ring(key);
        let naive_destination = cluster.naive_destination(target);
        naive_destinations.push(naive_destination);
        let cluster_t = cluster.clone();
        let outcomes_t = outcomes.clone();

        exec.spawn(async move {
            let mut current = start;
            let mut hops: u32 = 0;
            let mut path = vec![start];
            let mut completed = false;
            while hops < fault_retry_cap {
                match cluster_t.forward_once(current, target).await {
                    Some(next) if next == current => {
                        // Fault: no progress. Loop and try again;
                        // the outer cap bounds total retries.
                        hops += 1;
                        continue;
                    }
                    Some(next) => {
                        // Cycle guard: do not revisit, just terminate
                        // here (the test invariants then catch it).
                        if path.contains(&next) {
                            // Treat as stalled at `next`: still
                            // record it on the path for diagnostics.
                            path.push(next);
                            current = next;
                            hops += 1;
                            break;
                        }
                        path.push(next);
                        current = next;
                        hops += 1;
                        if path.len() as u32 > per_client_hop_cap {
                            break;
                        }
                    }
                    None => {
                        // Destination (or stall with no progress
                        // arc). Treat `current` as the destination.
                        completed = true;
                        break;
                    }
                }
            }

            outcomes_t.borrow_mut().push(ClientOutcome {
                start,
                target,
                key,
                terminal: current,
                hops,
                completed,
                path,
                naive_destination,
            });
        });
    }

    // Step budget: 64 base + clients * (delay + slack) * log_k * 16 * fault_slack.
    let pages_per_client = 1u64;
    let fault_slack = if w.hop_fault_rate > 0 { 4u64 } else { 1u64 };
    let log_k = ((w.peer_count.max(2)) as f64)
        .log((w.k.max(2)) as f64)
        .ceil() as u64
        + 1;
    let budget = 64
        + (w.clients.len() as u64)
            * pages_per_client
            * (w.max_hop_delay as u64 + 4)
            * (log_k + 4)
            * 16
            * fault_slack;

    exec.run(budget)?;

    let outcomes = Rc::try_unwrap(outcomes)
        .map_err(|_| RunError::Deadlock)
        .expect("all tasks completed; outcomes Rc must be unique")
        .into_inner();

    Ok(RunReport {
        outcomes,
        naive_destinations,
        steps: exec.last_steps(),
        peer_count: w.peer_count,
    })
}
