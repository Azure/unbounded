// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Production multi-node cluster adapter and deterministic workload campaigns.
use super::*;
use crate::{
    allocator,
    buffers::{self, Key},
    control::{proto, tests::fixture},
    http_client as client,
    simulation::{
        Disk, World,
        corpus::{self, *},
    },
    workers::{Driver as _, NumaNodeId},
};
use std::{
    collections::{BTreeSet, VecDeque},
    num::NonZeroUsize,
};

const NODES: usize = 1024;
const DISK: u64 = 32 * 1024 * 1024;
const MAX_TURNS: usize = 20_000;
const POOL_SLOTS: usize = 48;
const RDMA_QPS: usize = 32;
const RDMA_DEPTH: usize = 2;
const RING_SLOTS: u32 = 256;
#[path = "actors.rs"]
mod actors;
#[path = "oracles.rs"]
pub(super) mod oracles;
use crate::simulation::history::{Failure, Mutant, Transition};
use oracles::{Capabilities, Oracle};

#[derive(serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
struct ArtifactInput {
    seeds: crate::simulation::journal::Seeds,
    nodes: usize,
    rdma: bool,
    actions: Vec<Action>,
    #[serde(default)]
    overlap: bool,
    #[serde(default)]
    namespace_overlap: bool,
    #[serde(default)]
    checkpoint_overlap: bool,
    #[serde(default)]
    checkpoint_versions: bool,
    #[serde(default)]
    flight_cancellation: bool,
    #[serde(default)]
    local_attribution: bool,
    #[serde(default)]
    confirmation_admission: bool,
    #[serde(default)]
    zc_retirement: bool,
    #[serde(default)]
    rdma_recovery: bool,
    #[serde(default)]
    confirmation_reload: bool,
    #[serde(default)]
    shared_workers: bool,
    #[serde(default)]
    wall_authentication: bool,
    #[serde(default)]
    mutant: Option<Mutant>,
    #[serde(default)]
    socket_capacity: Option<usize>,
    #[serde(default)]
    phase_policy: PhasePolicy,
    #[serde(default)]
    peer_failure_delay: u64,
}

impl ArtifactInput {
    fn validate_composition(&self) -> Result<(), &'static str> {
        if self.actor_count() > 1 {
            return Err("invalid scenario: multiple actors require an explicit composition");
        }
        if self.checkpoint_versions && !self.checkpoint_overlap {
            return Err("invalid scenario: checkpoint versions require checkpoint actor");
        }
        Ok(())
    }

    fn actor_count(&self) -> usize {
        [
            self.overlap,
            self.namespace_overlap,
            self.checkpoint_overlap,
            self.flight_cancellation,
            self.local_attribution,
            self.confirmation_admission,
            self.zc_retirement,
            self.rdma_recovery,
            self.confirmation_reload,
            self.shared_workers,
            self.wall_authentication,
        ]
        .into_iter()
        .filter(|enabled| *enabled)
        .count()
    }
}

#[test]
fn artifact_campaign() {
    use crate::simulation::journal::{Journal, Seeds};
    use serde_json::json;
    let input_path = std::env::var("RACER_DST_INPUT").ok();
    let input: ArtifactInput = if let Some(path) = input_path
        .as_ref()
        .filter(|p| std::path::Path::new(p).exists())
    {
        serde_json::from_reader(std::fs::File::open(path).expect("infrastructure: input file"))
            .expect("invalid scenario: input JSON")
    } else {
        assert_ne!(std::env::var("RACER_DST_MODE").as_deref(), Ok("exact"));
        let seed = std::env::var("RACER_DST_SEED")
            .ok()
            .map(|s| s.parse().unwrap())
            .unwrap_or(19);
        let seeds = Seeds::from_seed(seed);
        let mut input = ArtifactInput {
            seeds,
            nodes: 2,
            rdma: false,
            actions: random_requests(seeds.workload, 2, 6),
            overlap: std::env::var("RACER_DST_SCENARIO").as_deref()
                == Ok("overlap-reconfigure-restart"),
            namespace_overlap: std::env::var("RACER_DST_SCENARIO").as_deref()
                == Ok("overlap-namespace"),
            checkpoint_overlap: std::env::var("RACER_DST_SCENARIO").as_deref()
                == Ok("overlap-checkpoint-crash"),
            checkpoint_versions: false,
            mutant: None,
            flight_cancellation: false,
            local_attribution: false,
            confirmation_admission: false,
            zc_retirement: false,
            rdma_recovery: false,
            confirmation_reload: false,
            shared_workers: false,
            wall_authentication: false,
            socket_capacity: None,
            phase_policy: PhasePolicy::Fixed,
            peer_failure_delay: 0,
        };
        if input.actor_count() != 0 {
            input.actions.clear();
        }
        if let Some(path) = &input_path {
            std::fs::write(path, serde_json::to_vec_pretty(&input).unwrap()).unwrap();
        }
        input
    };
    input.validate_composition().unwrap();
    assert!(
        !input.shared_workers || (input.nodes == 2 && !input.rdma),
        "invalid scenario: shared workers require two HTTP nodes"
    );
    assert!(
        (2..=8).contains(&input.nodes) && input.actions.len() <= 4096,
        "invalid scenario: resource bounds"
    );
    assert!(
        corpus::valid(&input.actions, input.nodes),
        "invalid scenario"
    );
    assert!(
        !(input.overlap || input.namespace_overlap || input.checkpoint_overlap) || input.nodes == 2,
        "invalid scenario: overlap nodes"
    );
    for action in &input.actions {
        if let Action::CrashSectors(_, sectors) = action {
            assert!(
                sectors.iter().all(|sector| *sector < DISK / 512)
                    && sectors.iter().collect::<BTreeSet<_>>().len() == sectors.len(),
                "invalid scenario: crash sectors"
            );
        }
    }
    assert!(
        !input.rdma_recovery || (input.rdma && input.nodes == 8),
        "invalid scenario: RDMA recovery requires eight RDMA nodes"
    );
    assert!(
        !input.confirmation_reload || (input.rdma && input.nodes == 2),
        "invalid scenario: confirmation reload requires two RDMA nodes"
    );
    assert!(
        input.peer_failure_delay <= 1000,
        "invalid scenario: peer failure delay"
    );
    let world = World::new(input.seeds.scheduler);
    let _scope = world.enter();
    world.enable_scheduler();
    world.seeds(input.seeds);
    world.mutant(input.mutant);
    if let Some(capacity) = input.socket_capacity {
        assert!(
            (1..=16 * 1024 * 1024).contains(&capacity),
            "invalid scenario: socket capacity"
        );
        world.socket_capacity(capacity);
    }
    world.limits(100_000, 20_000_000, 64);
    let journal = std::env::var("RACER_DST_JOURNAL").ok();
    if let Some(path) = &journal {
        world.journal(Journal::open(
            std::path::Path::new(path),
            std::env::var("RACER_DST_MODE").as_deref() == Ok("exact"),
            serde_json::to_value(&input).unwrap(),
        ));
    }
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        let mut cluster = Cluster::with_rdma(world.clone(), input.nodes, input.rdma);
        cluster.phase_policy = input.phase_policy;
        cluster.peer_failure_delay = input.peer_failure_delay;
        if input.rdma && !input.confirmation_reload {
            cluster.warm(&corpus::covering_edges(input.nodes));
        }
        if input.wall_authentication {
            crate::http_auth::test_wall_authentication(&world);
        } else if input.shared_workers {
            actors::shared_workers(&mut cluster);
        } else if input.confirmation_reload {
            actors::confirmation_reload(&mut cluster);
        } else if input.rdma_recovery {
            actors::rdma_recovery(&mut cluster);
        } else if input.zc_retirement {
            let _node = world.scoped_node(Some(0));
            crate::uring::test_zc_retirement();
        } else if input.confirmation_admission {
            let _node = world.scoped_node(Some(0));
            crate::negotiation::test_confirmation_admission();
        } else if input.local_attribution {
            actors::local_attribution(&mut cluster);
        } else if input.flight_cancellation {
            actors::flight_cancellation(&mut cluster);
        } else if input.checkpoint_overlap {
            actors::checkpoint_crash_policy(&mut cluster, input.checkpoint_versions);
        } else if input.namespace_overlap {
            actors::namespace(&mut cluster);
        } else if input.overlap {
            actors::run(&mut cluster, input.seeds.faults);
        }
        // Actions are a follow-up workload, including when an actor was selected.
        for (index, action) in input.actions.iter().enumerate() {
            cluster.action(action.clone());
            world.observation(Transition::ActionExecuted { index });
        }
        cluster.finish()
    }));
    let outcome = match result {
        Ok((digest, disks)) => json!({"status": "pass", "digest": digest, "disks": disks}),
        Err(payload) => {
            if let Some(failure) = payload.downcast_ref::<Failure>() {
                json!({"status": "product_failure", "failure": failure})
            } else {
                let message = payload
                    .downcast_ref::<String>()
                    .map(String::as_str)
                    .or_else(|| payload.downcast_ref::<&str>().copied())
                    .unwrap_or("non-string panic");
                json!({"status": "simulator_failure", "failure": message.split("\nrecent=").next().unwrap()})
            }
        }
    };
    if journal.is_some() {
        world.finish_journal(outcome.clone());
    }
    if let Ok(path) = std::env::var("RACER_DST_RESULT") {
        std::fs::write(path, serde_json::to_vec(&outcome).unwrap()).unwrap();
    }
    assert_eq!(outcome["status"], "pass", "{outcome}");
}

#[test]
fn artifact_composition_rejects_conflicts_and_unknown_fields() {
    use serde_json::json;
    let base = json!({
        "seeds": crate::simulation::journal::Seeds::from_seed(19),
        "nodes": 2, "rdma": false, "actions": []
    });
    let actors = [
        "overlap",
        "namespace_overlap",
        "checkpoint_overlap",
        "flight_cancellation",
        "local_attribution",
        "confirmation_admission",
        "zc_retirement",
        "rdma_recovery",
        "confirmation_reload",
        "shared_workers",
        "wall_authentication",
    ];
    for (i, first) in actors.iter().enumerate() {
        let mut input = base.clone();
        input[first] = json!(true);
        assert!(
            serde_json::from_value::<ArtifactInput>(input.clone())
                .unwrap()
                .validate_composition()
                .is_ok()
        );
        for second in &actors[i + 1..] {
            let mut pair = input.clone();
            pair[second] = json!(true);
            assert!(
                serde_json::from_value::<ArtifactInput>(pair)
                    .unwrap()
                    .validate_composition()
                    .is_err()
            );
        }
    }
    let mut versions = base.clone();
    versions["checkpoint_versions"] = json!(true);
    assert!(
        serde_json::from_value::<ArtifactInput>(versions.clone())
            .unwrap()
            .validate_composition()
            .is_err()
    );
    versions["checkpoint_overlap"] = json!(true);
    assert!(
        serde_json::from_value::<ArtifactInput>(versions)
            .unwrap()
            .validate_composition()
            .is_ok()
    );
    let mut unknown = base;
    unknown["namespace_overalp"] = json!(true);
    assert!(serde_json::from_value::<ArtifactInput>(unknown).is_err());
}

#[test]
fn delayed_peer_notifications_wait_and_fence_restarted_processes() {
    for restart in [false, true] {
        let world = World::new(19);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, true);
        cluster.warm(&[(0, 1)]);
        let (_, _, local, remote) = cluster
            .pairs
            .iter()
            .find(|(a, b, _, _)| *a == 0 && *b == 1)
            .unwrap()
            .clone();
        cluster.peer_failure_delay = 20;
        cluster.schedule_peer_failure(0, local.clone());
        cluster.schedule_peer_failure(0, local.clone());
        assert_eq!(
            cluster.peer_notifications.len(),
            1,
            "same session notification is deduplicated"
        );
        let due = cluster.peer_notifications[0].due;
        if restart {
            cluster.reboot(0, false, Some(0));
        }
        while cluster.world.tick() + 1 < due {
            cluster.turn();
        }
        assert!(cluster.peer_notifications.iter().any(|n| n.due == due));
        if !restart {
            assert!(
                local.pairs_with(&remote),
                "failure must not take effect early"
            );
        }
        cluster.turn();
        assert!(!cluster.peer_notifications.iter().any(|n| n.due == due));
        if !restart {
            assert!(!local.pairs_with(&remote));
        }
        cluster.finish();
    }
}
#[test]
fn permuted_phases_preserve_rdma_effect_and_completion_ownership() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 4, true);
        cluster.phase_policy = PhasePolicy::Permuted;
        cluster.warm(&corpus::covering_edges(4));
        for (source, destination) in corpus::covering_edges(4) {
            cluster.action(Action::Get(Request {
                node: source,
                target: cluster.buckets[destination][0].clone(),
                range: None,
            }));
        }
        cluster.drain();
        assert!(
            cluster.reads > 0,
            "permuted phases must exercise RDMA reads"
        );
        cluster.finish();
    }
}
#[test]
fn bounded_streams_wall_steps_and_nonprefix_restart() {
    let world = World::new(19);
    let _scope = world.enter();
    world.enable_scheduler();
    world.socket_capacity(4096);
    let mut cluster = Cluster::with_rdma(world, 2, false);
    let target = cluster.buckets[0][0].clone();
    cluster.action(Action::Get(get(0, target.clone())));
    cluster.action(Action::WallOffset(0, -3000));
    cluster.action(Action::Durable(0, target.clone()));
    cluster.action(Action::CrashSectors(0, vec![3, 9, 15]));
    cluster.action(Action::WallOffset(0, 3000));
    cluster.action(Action::Get(get(0, target)));
    cluster.finish();
}
enum ClientExchange {
    Get(client::GetExchange),
    Head(client::HeadExchange),
}
struct ClientReply {
    status: u16,
    length: Option<u64>,
    bytes: Vec<u8>,
    head: bool,
}
impl ClientReply {
    fn status(&self) -> u16 {
        self.status
    }
    fn content_length(&self) -> Option<u64> {
        self.length
    }
}
impl ClientExchange {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<Progress<ClientReply>> {
        Ok(match self {
            Self::Get(exchange) => match exchange.poll(ring, budget)? {
                Progress::Pending(work) => Progress::Pending(work),
                Progress::Ready(mut reply) => {
                    if reply.status() == 206 {
                        let range =
                            std::str::from_utf8(reply.headers().get("content-range").unwrap())
                                .unwrap();
                        let (bounds, total) = range
                            .strip_prefix("bytes ")
                            .unwrap()
                            .split_once('/')
                            .unwrap();
                        let (start, end) = bounds.split_once('-').unwrap();
                        let (start, end, total): (u64, u64, u64) = (
                            start.parse().unwrap(),
                            end.parse().unwrap(),
                            total.parse().unwrap(),
                        );
                        assert!(start <= end && end < total);
                        assert_eq!(reply.content_length(), Some(end - start + 1));
                    }
                    Progress::Ready(ClientReply {
                        status: reply.status(),
                        length: reply.content_length(),
                        bytes: reply.body().to_vec(),
                        head: false,
                    })
                }
            },
            Self::Head(exchange) => match exchange.poll(ring, budget)? {
                Progress::Pending(work) => Progress::Pending(work),
                Progress::Ready(reply) => {
                    assert!(
                        crate::metadata::Checksum::from_etag(
                            std::str::from_utf8(reply.headers().get("etag").unwrap()).unwrap()
                        )
                        .is_ok()
                    );
                    Progress::Ready(ClientReply {
                        status: reply.status(),
                        length: reply.content_length(),
                        bytes: Vec::new(),
                        head: true,
                    })
                }
            },
        })
    }
    fn cancel(self, ring: &mut uring::Ring) -> io::Result<()> {
        match self {
            Self::Get(e) => e.cancel(ring),
            Self::Head(e) => e.cancel(ring),
        }
    }
}
impl Cluster {
    pub(crate) fn separate_origin(&mut self, node: usize) -> http::Server<Origin> {
        self.origin_off(node);
        // Retire the old listener's pending ACCEPT before rebinding its address.
        for _ in 0..100 {
            self.turn();
        }
        let _scope = self.world.scoped_node(Some(node));
        http::Server::new(
            http::Listener::bind(
                self.machines[node].config.volumes[0]
                    .origin_address
                    .parse()
                    .unwrap(),
                std::num::NonZeroU32::new(16).unwrap(),
            )
            .unwrap(),
            Origin {
                scenario: self.scenario.is_some(),
                node,
                hits: self.hits.clone(),
            },
            http::Config::default(),
        )
    }
    pub(super) fn origin_off(&mut self, node: usize) {
        let _scope = self.world.scoped_node(Some(node));
        let (app, ring) = self.machines[node].driver.parts_mut();
        if let Some(mut origin) = app.origin.take() {
            origin.shutdown(ring).unwrap();
        }
    }
    pub(super) fn reboot(&mut self, node: usize, format: bool, torn: Option<usize>) {
        let mut notify = Vec::new();
        for (a, b, local, remote) in &self.pairs {
            let counterpart = if *a == node {
                Some((*b, remote))
            } else if *b == node {
                Some((*a, local))
            } else {
                None
            };
            if let Some((peer, qp)) = counterpart {
                notify.push((peer, qp.clone()));
            }
        }
        for (peer, qp) in notify {
            self.schedule_peer_failure(peer, qp);
        }
        let _scope = self.world.scoped_node(Some(node));
        let mut old = self.machines.remove(node);
        self.retired_completed += old.driver.application().completed;
        for pending in &old.driver.application().pending {
            self.world.observation(Transition::ProcessLost {
                request: pending.id,
            });
        }
        self.cancelled += old.driver.application().pending.len();
        self.world
            .trace_bytes(old.driver.application().bytes.clone().finalize().as_bytes());
        for transport in &old.transports {
            transport.shutdown().unwrap();
        }
        old.driver.simulated_crash();
        self.world.restart_node(Some(node));
        if let Some(torn) = torn {
            old.disk.crash(torn);
        }
        let (disk, config, rdma) = (
            old.disk.clone(),
            old.config.clone(),
            !old.transports.is_empty(),
        );
        drop(old);
        self.completions.retain(|(n, _, _)| *n != node);
        self.pairs.retain(|(a, b, _, _)| *a != node && *b != node);
        self.live_sessions = usize::MAX;
        self.machines.insert(
            node,
            Self::boot_machine(
                node,
                config,
                disk,
                format,
                rdma,
                &self.hits,
                self.scenario,
                None,
            ),
        );
    }
    pub(super) fn add_worker(&mut self, config: proto::Snapshot, ring: uring::Ring) {
        let node = self.machines.len();
        let _scope = self.world.scoped_node(Some(node));
        self.machines.push(Self::boot_machine(
            node,
            config,
            Disk::new(DISK),
            true,
            false,
            &self.hits,
            self.scenario,
            Some(ring),
        ));
        self.pinned_slots.push(0);
        self.initiated.push(0);
        self.served.push(0);
        self.turn();
    }
    fn prepare_single_volume(trust: &crate::control::Trust, config: &proto::Snapshot) -> Prepared {
        let prepared = crate::control::tests::prepare_cluster_snapshot(trust, config.clone());
        assert_eq!(prepared.volumes.len(), 1);
        let volume = &prepared.volumes[0];
        assert_eq!(
            volume.config.peer_endpoints.as_ref().unwrap().peers.len(),
            config.peers.len()
        );
        assert_eq!(volume.peers.len(), config.peers.len());
        for peer in &config.peers {
            let id = peer.id.parse().unwrap();
            assert!(
                prepared
                    .eligible_node_for_volume(&volume.config.id, id)
                    .is_some()
            );
            assert_eq!(
                volume.peers[&peer.id].address(),
                peer.http_address.parse::<SocketAddr>().unwrap()
            );
        }
        for peer in &volume.config.peers {
            assert!(
                prepared
                    .eligible_peer_for_volume(&volume.config.id, peer)
                    .is_some()
            );
        }
        assert_eq!(
            prepared
                .eligible_peers_for_volume(&volume.config.id)
                .count(),
            volume.config.peers.len()
        );
        prepared
    }
    fn capture_campaign(
        seed: u64,
        count: usize,
        rdma: bool,
        actions: &[Action],
        replay: Option<Vec<crate::simulation::Choice>>,
    ) -> (
        Result<([u8; 32], Vec<[u8; 32]>), String>,
        Vec<crate::simulation::Choice>,
    ) {
        let world = World::new(seed);
        let _scope = world.enter();
        world.enable_scheduler();
        world.limits(100_000, 20_000_000, 65536);
        let expected_choices = replay.as_ref().map(Vec::len);
        if let Some(prefix) = replay {
            world.replay(prefix);
        }
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let mut cluster = Self::with_rdma(world.clone(), count, rdma);
            if rdma {
                cluster.warm(&corpus::covering_edges(count));
            }
            for action in actions {
                cluster.action(action.clone());
            }
            cluster.finish()
        }))
        .map_err(|payload| {
            let message = payload
                .downcast_ref::<String>()
                .map(String::as_str)
                .or_else(|| payload.downcast_ref::<&str>().copied())
                .unwrap_or("non-string panic");
            message
                .split("\nrecent=")
                .next()
                .unwrap_or(message)
                .to_owned()
        });
        if result.is_err() {
            world.assert_replay_consumed();
        }
        if let Some(expected) = expected_choices {
            assert_eq!(
                world.choice_count(),
                expected as u64,
                "causal replay diverged beyond recording"
            );
        }
        (result, world.choices())
    }
    fn campaign(seed: u64, scheduler: u64, count: usize, rdma: bool, steps: usize) {
        let actions = random_requests(seed, count, steps);
        assert!(corpus::valid(&actions, count));
        let (first, decisions) = Self::capture_campaign(scheduler, count, rdma, &actions, None);
        if let Err(signature) = first {
            eprintln!(
                "seed={seed} scheduler={scheduler} nodes={count} rdma={rdma} steps={steps}\nactions={actions:?}\ndecisions={decisions:?}"
            );
            assert_eq!(
                decisions.first().map(|c| c.index),
                Some(0),
                "recording truncated"
            );
            assert_eq!(
                Self::capture_campaign(scheduler, count, rdma, &actions, Some(decisions)).0,
                Err(signature.clone()),
                "failure must strictly replay"
            );
            panic!("seed={seed} scheduler={scheduler} failure={signature}");
        }
    }
    fn ready_order(&self) -> Vec<usize> {
        let mut nodes = Vec::new();
        let mut keys = vec![0; self.machines.len()];
        for (node, machine) in self.machines.iter().enumerate() {
            if machine.driver.ready() {
                let _scope = self.world.scoped_node(Some(node));
                keys[node] = (self.world.process().incarnation << 32) | node as u64;
                nodes.push(node);
            }
        }
        if self.loaded_pair_diagnostic {
            return nodes;
        }
        let order =
            corpus::ready_permutation(&self.world, nodes.iter().map(|n| (*n, keys[*n])).collect());
        debug_assert_eq!(order.len(), nodes.len());
        debug_assert_eq!(
            order.iter().copied().collect::<BTreeSet<_>>(),
            nodes.iter().copied().collect()
        );
        order
    }
    fn fabric_snapshot(&mut self) -> Vec<(usize, rdma::TestQp)> {
        let qps = rdma::test_qps();
        self.registry_owners.resize(qps.len(), 0);
        let mut hint = 0;
        let machines = &self.machines;
        let owners = &mut self.registry_owners;
        let misses = &mut self.registry_misses;
        qps.into_iter()
            .enumerate()
            .filter_map(|(index, qp)| {
                let belongs =
                    |node: usize| machines[node].transports.iter().any(|t| qp.belongs_to(t));
                let cached = owners[index];
                let node = if belongs(cached) {
                    Some(cached)
                } else {
                    *misses += 1;
                    (0..machines.len())
                        .map(|offset| (hint + offset) % machines.len())
                        .find(|n| belongs(*n))
                }?;
                hint = node;
                owners[index] = node;
                Some((node, qp))
            })
            .collect()
    }
    fn refresh_pairs(&mut self) {
        let qps = self.fabric_snapshot();
        let mut by_node = vec![Vec::new(); self.machines.len()];
        for (node, qp) in &qps {
            by_node[*node].push(qp);
        }
        let mut cached = BTreeMap::new();
        for (node, peer, local, remote) in std::mem::take(&mut self.pairs) {
            cached
                .entry(node)
                .or_insert_with(Vec::new)
                .push((peer, local, remote));
        }
        for (node, qp) in qps.iter() {
            let known = cached.get(node).and_then(|pairs| {
                pairs
                    .iter()
                    .find(|(_, local, remote)| local.same(qp) && local.pairs_with(remote))
            });
            if let Some((peer, _, remote)) = known {
                self.pairs.push((*node, *peer, qp.clone(), remote.clone()));
                continue;
            }
            for &peer in &self.machines[*node].neighbors {
                if let Some(remote) = by_node[peer].iter().find(|remote| qp.pairs_with(remote)) {
                    self.pairs
                        .push((*node, peer, qp.clone(), (*remote).clone()));
                    break;
                }
            }
        }
    }
    fn order_completions(&self, delivery: &mut [(usize, rdma::TestQp, rdma::TestPost)]) {
        let mut groups = BTreeMap::new();
        for (node, _, post) in delivery.iter() {
            let hash = groups.entry(*node).or_insert_with(|| {
                let _scope = self.world.scoped_node(Some(*node));
                let mut hash = blake3::Hasher::new();
                hash.update(&(*node as u64).to_le_bytes());
                hash.update(&self.world.process().incarnation.to_le_bytes());
                hash
            });
            hash.update(&post.id.to_le_bytes());
            hash.update(&post.opcode.to_le_bytes());
            hash.update(&post.value);
        }
        if groups.is_empty() {
            return;
        }
        let keys: Vec<_> = groups
            .values()
            .map(|h| u64::from_le_bytes(h.clone().finalize().as_bytes()[..8].try_into().unwrap()))
            .collect();
        let selected = self.world.choose_enabled("cluster-cq-group", &keys);
        let first = *groups.keys().nth(selected).unwrap();
        delivery
            .sort_by_key(|(node, _, _)| (node + self.machines.len() - first) % self.machines.len());
    }
    fn admit_head(&mut self, node: usize, target: &str) {
        self.admit_method(get(node, target), true);
    }
    fn large_matrix(&mut self, edges: &[(usize, usize)], pairs: usize, latency: Option<u64>) {
        let width = buffers::BUFFER_SIZE;
        let count = self.machines.len();
        // Explicit link latency; deadlines and resource budgets stay production.
        self.world.link_profile(65536, latency);
        eprintln!(
            "DST fullpage profile short=65536 effect_and_cqe_ms={latency:?} (None=seeded1..3)"
        );
        for pair in 0..pairs {
            self.quiesce();
            let len = [width - 1, width, width + 17][pair % 3];
            let target = format!("/sized/{len}/{pair}?exact=%2f&version={VERSION}");
            let destination = owner(&target, count);
            let source = edges
                .iter()
                .find(|(_, next)| *next == destination)
                .unwrap()
                .0;
            self.admit_head(source, &target);
            self.admit_head(destination, &target);
            self.drain();
            let (began, counts) = (self.world.tick(), self.world.counts());
            let range = (len > width).then_some((0, width - 1));
            for node in [source, destination] {
                self.admit(Request {
                    node,
                    target: target.clone(),
                    range,
                });
            }
            self.drain();
            let after = self.world.counts();
            assert!(
                after[27] - counts[27] >= 64,
                "full page must cross HTTP, not only a 25-byte slice"
            );
            eprintln!(
                "DST fullpage pair={pair} length={len} elapsed_ms={} recv={} send={}",
                self.world.tick() - began,
                after[27] - counts[27],
                after[26] + after[47] - counts[26] - counts[47]
            );
            // Cross-page and clipped EOF, followed by an unaligned full-width
            // window. Together the two responses cover all bytes of +17 objects.
            for (start, end) in [(width - 8, width + 64), (17, len + 63)] {
                for node in [source, destination] {
                    self.admit(Request {
                        node,
                        target: target.clone(),
                        range: Some((start, end)),
                    });
                }
                self.drain();
            }
        }
    }
    fn loaded_large_pairs(&mut self, edges: &[(usize, usize)]) {
        let count = self.machines.len();
        self.world.link_profile(65536, None);
        eprintln!(
            "DST original loaded paired-range diagnostic: retained burst/cold/relay history, seeded1..3ms, strict success"
        );
        for pair in 0..16 {
            self.quiesce();
            let target = format!("/large/{pair}?exact=%2f&version={VERSION}");
            let destination = owner(&target, count);
            let source = edges.iter().find(|(_, b)| *b == destination).unwrap().0;
            eprintln!(
                "DST loaded pair={pair} tick={} source={source} owner={destination}",
                self.world.tick()
            );
            for node in [source, destination] {
                self.admit(Request {
                    node,
                    target: target.clone(),
                    range: Some((buffers::BUFFER_SIZE - 8, buffers::BUFFER_SIZE + 16)),
                });
            }
            self.drain();
        }
    }
}
#[test]
fn fullpage_fast_link_and_slow_deadline() {
    let world = World::new(1024);
    let _scope = world.enter();
    world.enable_scheduler();
    let mut cluster = Cluster::with_rdma(world.clone(), 32, true);
    let edges = corpus::covering_edges(32);
    cluster.warm(&edges);
    cluster.large_matrix(&edges, 3, Some(1));
    cluster.finish();
    // A separate slow profile must exhaust an explicit caller deadline, never
    // reinterpret a partial body as success. This is not a healthy-503 allowance.
    world.link_profile(65536, Some(20));
    let _scope = world.scoped_node(Some(32));
    struct Probe {
        origin: http::Server<Origin>,
        exchange: client::GetExchange,
        end: Instant,
        expired: bool,
    }
    impl uring::Application for Probe {
        fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
            let mut work = self.origin.poll(ring, budget)?;
            if !self.expired {
                match self.exchange.poll(ring, budget) {
                    Err(error) => {
                        assert_eq!(error.kind(), io::ErrorKind::TimedOut);
                        assert!(crate::environment::now() >= self.end);
                        eprintln!(
                            "DST slow-link deadline expired after real receive progress: {error}"
                        );
                        self.expired = true;
                    }
                    Ok(Progress::Ready(_)) => {
                        panic!("slow full-page transfer beat its physical lower bound")
                    }
                    Ok(Progress::Pending(w)) => work.merge(w),
                }
            }
            Ok(work)
        }
        fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
            self.origin.shutdown(ring)
        }
    }
    let ring = uring::Ring::http_test_ring(
        buffers::test_pool(
            buffers::Config::new(NonZeroUsize::new(4).unwrap()),
            NumaNodeId(32),
            true,
        ),
        uring::Config::default(),
    )
    .unwrap();
    let origin = http::Server::new(
        http::Listener::bind(address(32, true), NonZeroU32::new(8).unwrap()).unwrap(),
        Origin {
            scenario: false,
            node: 32,
            hits: Rc::new(RefCell::new(Vec::new())),
        },
        http::Config::default(),
    );
    let target = "/sized/4194304/slow-budget";
    let fill = ring.pool().stage(Key::new([231; 32])).unwrap();
    let end = world.now() + Duration::from_millis(500);
    let exchange = client::Connection::new(address(32, true), "localhost")
        .unwrap()
        .get(
            client::Request::new(target, &[("Range", "bytes=0-4194303")]).unwrap(),
            fill,
            end,
        )
        .unwrap();
    let mut driver = uring::Driver::new(
        ring,
        Probe {
            origin,
            exchange,
            end,
            expired: false,
        },
        64,
    )
    .unwrap();
    let before = world.counts();
    while !driver.application().expired {
        world.service_tick();
        if driver.ready() {
            driver.turn().unwrap();
        }
        assert!(world.now() < end + Duration::from_secs(1));
    }
    assert!(
        world.counts()[27] >= before[27] + 4,
        "slow case must actually receive bytes"
    );
    let pool = driver.ring_mut().pool().clone();
    driver.shutdown().unwrap();
    drop(driver);
    pool.assert_recovered();
    world.assert_clean();
}
#[test]
fn ready_pool_is_linear_fair_and_strictly_replayed() {
    for count in [0, 1, 2, 32, 1024] {
        let nodes: Vec<_> = (0..count)
            .map(|node| (node, (7u64 << 32) | node as u64))
            .collect();
        let world = World::new(71);
        world.enable_scheduler();
        let order = corpus::ready_permutation(&world, nodes.clone());
        assert_eq!(world.choice_count(), count.saturating_sub(1) as u64);
        assert_eq!(
            order.iter().copied().collect::<BTreeSet<_>>(),
            (0..count).collect()
        );
        assert_eq!(order.len(), count);
        let replay = World::new(71);
        replay.enable_scheduler();
        replay.replay(world.choices());
        assert_eq!(order, corpus::ready_permutation(&replay, nodes));
        replay.assert_replay_consumed();
    }
}
#[test]
fn fullpage_seeded_latency_stress() {
    for rdma in [false, true] {
        let world = World::new(1024);
        let _scope = world.enter();
        world.enable_scheduler();
        let mut cluster = Cluster::with_rdma(world.clone(), 32, rdma);
        let edges = corpus::covering_edges(32);
        if rdma {
            cluster.warm(&edges);
        }
        world.link_profile(65536, None);
        cluster.loaded_large_pairs(&edges);
        cluster.finish();
    }
}
#[test]
fn b01_zero_ttl_metadata_across_peers() {
    for rdma in [false, true] {
        for policy in ["missing", "zero", "nostore", "positive"] {
            let run = |replay: Option<Vec<crate::simulation::Choice>>| {
                let world = World::new(431);
                let _scope = world.enter();
                world.enable_scheduler();
                if let Some(replay) = replay {
                    world.replay(replay);
                }
                let mut cluster = Cluster::with_rdma(world.clone(), 8, rdma);
                let route = [(0, 1), (1, 3), (3, 7)];
                if rdma {
                    cluster.warm(&route);
                }
                cluster.edges.clear();
                let target = (0..1024)
                    .map(|n| format!("/ttl/{policy}/{n}?exact=%2f"))
                    .find(|t| owner(t, 8) == 7)
                    .unwrap();
                let hits =
                    |c: &Cluster| c.hits.borrow().iter().filter(|(_, t)| *t == target).count();
                let gate = world.gate(crate::simulation::Gate::new(
                    7,
                    address(7, true),
                    &target,
                    crate::simulation::Phase::Request,
                    None,
                ));
                cluster.admit(get(0, &target));
                for _ in 0..500 {
                    if world.hits(gate) > 0 {
                        break;
                    }
                    cluster.turn();
                }
                assert!(world.hits(gate) > 0, "must hold the real origin HEAD");
                let coalesced = cluster.machines[0].driver.ring_mut().metrics().values()[9];
                cluster.admit(get(0, &target));
                cluster.admit_head(0, &target);
                for _ in 0..100 {
                    if cluster.machines[0].driver.ring_mut().metrics().values()[9] >= coalesced + 2
                    {
                        break;
                    }
                    cluster.turn();
                }
                assert_eq!(
                    cluster.machines[0].driver.ring_mut().metrics().values()[9],
                    coalesced + 2,
                    "both callers must join before publication"
                );
                assert_eq!(hits(&cluster), 0);
                world.release(gate);
                cluster.drain();
                cluster.quiesce();
                assert_eq!(hits(&cluster), 2, "one HEAD and one shared page fill");
                if rdma {
                    assert!(
                        route.iter().all(|e| cluster.edges.contains(e)),
                        "typed metadata uses peer HTTP"
                    );
                    for (source, destination) in route {
                        assert!(cluster.initiated[source] >= 1);
                        assert!(cluster.served[destination] >= 1);
                    }
                } else {
                    assert!(route.iter().all(|e| cluster.edges.contains(e)));
                }
                // Independent lookups must not reuse a completed zero-TTL
                // buffer at ingress, relay, or owner. Payload identity is stable.
                let zero = usize::from(policy != "positive");
                cluster.admit_head(0, &target);
                cluster.drain();
                assert_eq!(hits(&cluster), 2 + zero);
                cluster.admit(Request {
                    node: 0,
                    target: target.clone(),
                    range: Some((3, 19)),
                });
                cluster.drain();
                assert_eq!(hits(&cluster), 2 + 2 * zero);
                if policy == "positive" {
                    cluster.quiesce();
                    world.advance(Duration::from_secs(3));
                    cluster.admit_head(0, &target);
                    cluster.drain();
                    assert_eq!(hits(&cluster), 3, "expired cached records must revalidate");
                }
                if rdma && zero == 1 {
                    // A pending payload corruption cannot affect typed metadata:
                    // HEAD performs no RDMA READ and still revalidates zero TTL.
                    cluster.quiesce();
                    let before = cluster.corruptions;
                    cluster.corrupt = true;
                    cluster.admit_head(0, &target);
                    cluster.drain();
                    assert_eq!(cluster.corruptions, before);
                    cluster.corrupt = false;
                    assert!(!cluster.edges.is_empty());
                    assert!(hits(&cluster) > 2 + 2 * zero);
                }
                assert!(cluster.candidates.is_empty());
                let result = cluster.finish();
                let choices = world.choices();
                assert_eq!(
                    choices.first().unwrap().index,
                    0,
                    "complete replay recording"
                );
                assert_eq!(choices.len() as u64, world.choice_count());
                (result, choices)
            };
            eprintln!("B01 rdma={rdma} policy={policy}");
            let (result, choices) = run(None);
            assert_eq!(result, run(Some(choices)).0, "rdma={rdma} policy={policy}");
        }
    }
}

struct Pending {
    id: u64,
    request: Request,
    exchange: ClientExchange,
    began: Instant,
    refusal: Option<(usize, crate::simulation::Phase)>,
}

#[test]
fn coordinated_retirement_under_continuous_http_load() {
    for phase in [3, 4] {
        let world = World::new(5034);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world.clone(), 2, false);
        let mut old = Vec::new();
        let mut updates = Vec::new();
        for (node, machine) in cluster.machines.iter_mut().enumerate() {
            let volumes = &machine.driver.application().volumes;
            old.push(
                volumes.servers[&address(node, false)]
                    .handler()
                    .current
                    .clone(),
            );
            updates.push(volumes.updates.clone());
            machine.config.revision = 5;
            machine.config.epoch = 5;
            machine.config.volumes[0].topology.as_mut().unwrap().epoch = 5;
            let (mut trust, _) = fixture();
            trust.node = identity(node);
            updates[node]
                .command(Cluster::prepare_single_volume(&trust, &machine.config), 2)
                .unwrap();
        }
        cluster.turn();
        for (node, machine) in cluster.machines.iter().enumerate() {
            assert_eq!(updates[node].status()["receiveReadyWorkers"], 1);
            assert_eq!(updates[node].status()["activeRevision"], 1);
            let (mut trust, _) = fixture();
            trust.node = identity(node);
            updates[node]
                .command(
                    Cluster::prepare_single_volume(&trust, &machine.config),
                    phase,
                )
                .unwrap();
        }
        cluster.turn();
        for update in &updates {
            assert_eq!(update.applied_epoch(), 5);
            assert_eq!(update.status()["retiredWorkers"], 0);
        }
        let deadlines: Vec<_> = old.iter().map(|g| g.drain.get().unwrap()).collect();
        let end = *deadlines.iter().max().unwrap() + Duration::from_secs(1);
        let mut completed_at_retirement = [None; 2];
        while world.now() < end {
            // Replenish bounded overlapping GETs before every driver turn. No
            // drain/quiesce call or generation-deadline override assists retirement.
            for node in 0..2 {
                while cluster.machines[node].driver.application().pending.len() < 2 {
                    let target = cluster.buckets[1 - node][0].clone();
                    cluster.admit(Request {
                        node,
                        target,
                        range: Some((0, 0)),
                    });
                }
            }
            world.advance(Duration::from_millis(10));
            cluster.turn();
            for node in 0..2 {
                assert_eq!(old[node].drain.get(), Some(deadlines[node]));
                let retired = updates[node].status()["retiredWorkers"] == 1;
                if world.now() < deadlines[node] {
                    assert!(!retired, "old receive authority must retain its lease");
                }
                if retired && completed_at_retirement[node].is_none() {
                    let app = cluster.machines[node].driver.application();
                    assert!(old[node].expired.get());
                    assert!(!app.pending.is_empty(), "retirement must overlap HTTP load");
                    assert!(app.completed > 100, "exercise sustained successful traffic");
                    completed_at_retirement[node] = Some(app.completed);
                }
            }
        }
        for (node, machine) in cluster.machines.iter().enumerate() {
            let app = machine.driver.application();
            let completed = completed_at_retirement[node].expect("retirement stalled under load");
            assert!(
                app.completed > completed,
                "HTTP must continue after retirement"
            );
            assert_eq!(updates[node].status()["phase"], phase);
            assert_eq!(updates[node].status()["retiredWorkers"], 1);
            if phase == 3 {
                let (mut trust, _) = fixture();
                trust.node = identity(node);
                updates[node]
                    .command(Cluster::prepare_single_volume(&trust, &machine.config), 4)
                    .unwrap();
                assert_eq!(updates[node].status()["phase"], 4);
                assert_eq!(updates[node].status()["retiredWorkers"], 1);
            }
        }
        drop(old);
        cluster.finish();
    }
}

pub(super) struct App {
    pub(super) volumes: Volumes,
    pub(super) origin: Option<http::Server<Origin>>,
    pending: VecDeque<Pending>,
    completed: usize,
    bytes: blake3::Hasher,
    outcomes: BTreeMap<String, u16>,
}
impl uring::Application for App {
    fn poll(&mut self, ring: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        let mut work = match &mut self.origin {
            Some(origin) => origin.poll(ring, budget)?,
            None => uring::Work::default(),
        };
        work.merge(self.volumes.poll(ring, budget)?);
        let count = self.pending.len().min(budget);
        for _ in 0..count {
            let mut pending = self.pending.pop_front().unwrap();
            match pending.exchange.poll(ring, budget)? {
                Progress::Pending(w) => {
                    work.merge(w);
                    self.pending.push_back(pending);
                }
                Progress::Ready(mut response) => {
                    crate::simulation::current()
                        .unwrap()
                        .observation(Transition::Response {
                            request: pending.id,
                            status: response.status(),
                        });
                    let status = response.status();
                    let head = response.head;
                    if status >= 500 {
                        eprintln!(
                            "DST response status={status} elapsed={:?} pool={:?} allocator_idle={}",
                            crate::environment::now().duration_since(pending.began),
                            ring.pool().invariant_snapshot(),
                            crate::cache::tests::idle(&self.volumes.cache.borrow())
                        );
                    }
                    let reply = corpus::Reply {
                        request: pending.request,
                        status,
                        length: response.content_length(),
                        bytes: std::mem::take(&mut response.bytes),
                        refusal: pending.refusal,
                        elapsed: crate::environment::now().duration_since(pending.began),
                    };
                    if head {
                        assert_eq!(status, 200);
                        assert_eq!(
                            reply.length,
                            Some(corpus::length(&reply.request.target) as u64)
                        );
                        assert!(reply.bytes.is_empty());
                    } else {
                        corpus::check_reply(&crate::simulation::current().unwrap(), &reply);
                    }
                    self.bytes.update(reply.request.target.as_bytes());
                    self.bytes.update(&reply.bytes);
                    self.bytes.update(&status.to_le_bytes());
                    self.outcomes.insert(reply.request.target, status);
                    self.completed += 1;
                }
            }
        }
        work.runnable |= self.pending.len() > count;
        Ok(work)
    }
    fn shutdown(&mut self, ring: &mut uring::Ring) -> io::Result<()> {
        self.pending.clear();
        self.volumes.shutdown(ring)?;
        if let Some(origin) = &mut self.origin {
            origin.shutdown(ring)?;
        }
        Ok(())
    }
}
pub(super) struct Machine {
    pub(super) driver: uring::Driver<App>,
    pub(super) disk: Disk,
    live: bool,
    transports: Vec<rdma::Transport>,
    pub(super) config: proto::Snapshot,
    neighbors: Vec<usize>,
}
#[derive(Clone, Copy)]
pub(super) struct Scenario {
    pub(super) oracles: Capabilities,
    pub(super) algorithm: Option<u32>,
    pub(super) slots: usize,
    pub(super) multi_rdma: bool,
}
// Shared harness oracle map (scenario assertions remain at their call sites):
// bytes/identity -> App + exact_identity_version_bounds_and_peer_validation;
// routing/attribution -> turn + dst_final_hop_timeout_phases_*;
// flight dependency -> turn + dst_canonical_convergent_flights_and_shared_failure;
// deadlines/liveness -> App + generated_consumer_lifecycle;
// ownership/resources -> finish + dst_step7_* + publication_authority_*;
// persistence/generations -> dst_crash_cancel_disk_fault_campaign + dst_algorithm_rollover_*;
// deterministic replay -> campaign failure replay + semantic/ready-prefix tests.
#[derive(Clone, Copy, Default, serde::Serialize, serde::Deserialize)]
enum PhasePolicy {
    #[default]
    Fixed,
    Permuted,
}

pub(crate) struct Cluster {
    hold_confirmation: bool,
    held_confirmations: usize,
    phase_policy: PhasePolicy,
    peer_failure_delay: u64,
    peer_notifications: Vec<PeerNotification>,
    next_peer_notification: u64,
    profile: crate::metrics::dst::Profile,
    pub(crate) world: World,
    pub(super) machines: Vec<Machine>,
    pub(crate) hits: Rc<RefCell<Vec<(usize, String)>>>,
    // Scenario fixtures supply topology/failure assertions at each transition;
    // generated campaigns additionally enforce the independent canonical graph.
    scenario: Option<Scenario>,
    oracles: Capabilities,
    buckets: Vec<Vec<String>>,
    admitted: usize,
    peak_admitted: usize,
    pinned_slots: Vec<usize>,
    peak_pinned_slots: usize,
    turns: usize,
    peer_failures: usize,
    cursor: u64,
    edges: BTreeSet<(usize, usize)>,
    http_exchanges: BTreeMap<(usize, usize, bool), usize>,
    dependencies: corpus::Dependencies,
    distance: Vec<Vec<u8>>,
    completions: Vec<(usize, rdma::TestQp, rdma::TestPost)>,
    pub(crate) reads: usize,
    initiated: Vec<usize>,
    served: Vec<usize>,
    retired_completed: usize,
    cancelled: usize,
    corrupt: bool,
    corruptions: usize,
    fault_targets: BTreeSet<String>,
    candidates: BTreeSet<String>,
    gate: Option<usize>,
    gate_target: Option<(String, bool)>,
    gate_source: usize,
    actions: BTreeSet<&'static str>,
    pairs: Vec<(usize, usize, rdma::TestQp, rdma::TestQp)>,
    corrupted_edge: Option<(usize, usize)>,
    live_sessions: usize,
    registry_owners: Vec<usize>,
    registry_misses: usize,
    loaded_pair_diagnostic: bool,
    gate_phase: crate::simulation::Phase,
}
struct PeerNotification {
    id: u64,
    due: u64,
    process: crate::simulation::Process,
    qp: rdma::TestQp,
}
impl Cluster {
    fn with_rdma(world: World, count: usize, rdma: bool) -> Self {
        Self::build(world, count, rdma, None)
    }
    pub(super) fn build(
        world: World,
        count: usize,
        rdma: bool,
        scenario: Option<Scenario>,
    ) -> Self {
        world.enable_scheduler();
        assert!((2..=NODES).contains(&count));
        let oracles = scenario.map_or(Capabilities::CANONICAL, |s| s.oracles);
        oracles.declare(&world);
        let topology =
            crate::topology::Topology::new(count as u32, crate::topology::Epoch::new(1)).unwrap();
        let degree = topology.degree() as usize;
        if count == NODES {
            assert_eq!(degree, 11);
        }
        let (incoming, distance) = corpus::graph(count);
        let hits = Rc::new(RefCell::new(Vec::new()));
        let mut machines = Vec::with_capacity(count);
        let (base_trust, base_config) = fixture();
        for node in 0..count {
            let _scope = world.scoped_node(Some(node));
            let mut config = base_config.clone();
            assert_eq!(config.volumes.len(), 1, "cluster corpus is single-volume");
            config.epoch = 1;
            let trust = crate::control::Trust {
                node: identity(node),
                universe: base_trust.universe,
                keys: base_trust.keys.clone(),
            };
            config.node = trust.node.to_vec();
            config.fabric = "invariant-dst".into();
            config.peers.clear();
            let neighbors: BTreeSet<_> = (0..degree)
                .map(|digit| (degree * node + digit) % count)
                .filter(|n| *n != node)
                .collect();
            let id = |n| NodeId::from_bytes(&identity(n)).unwrap().to_string();
            for &peer in neighbors.union(&incoming[node]).filter(|n| **n != node) {
                config.peers.push(proto::Peer {
                    id: id(peer),
                    http_address: address(peer, false).to_string(),
                    fabric: config.fabric.clone(),
                });
            }
            let volume = &mut config.volumes[0];
            volume.listen = address(node, false).to_string();
            volume.origin_address = address(node, true).to_string();
            volume.peers = neighbors.iter().map(|n| id(*n)).collect();
            volume.peer_endpoints = Some(proto::VolumePeerEndpoints {
                peers: config
                    .peers
                    .iter()
                    .map(|peer| proto::VolumePeerEndpoint {
                        peer: peer.id.clone(),
                        http_address: peer.http_address.clone(),
                    })
                    .collect(),
            });
            volume.topology = Some(proto::Topology {
                routing_algorithm: None,
                epoch: 1,
                slot_count: count as u32,
                local_slots: vec![node as u32],
                neighbors: neighbors
                    .iter()
                    .map(|n| proto::SlotPeer {
                        slot: *n as u32,
                        peer: id(*n),
                    })
                    .collect(),
            });
            if let Some(scenario) = scenario {
                let addresses: Vec<_> = (0..count).map(|n| address(n, false)).collect();
                config = crate::control::tests::cluster_config(
                    node,
                    &addresses,
                    format!("127.0.0.1:{}", 11000 + node).parse().unwrap(),
                    scenario.algorithm,
                    "dst",
                );
            }
            machines.push(Self::boot_machine(
                node,
                config,
                Disk::new(DISK),
                true,
                rdma,
                &hits,
                scenario,
                None,
            ));
        }
        let buckets = corpus::buckets(count);
        let mut s = Self {
            hold_confirmation: false,
            held_confirmations: 0,
            phase_policy: PhasePolicy::Fixed,
            peer_failure_delay: 0,
            peer_notifications: Vec::new(),
            next_peer_notification: 0,
            profile: crate::metrics::dst::Profile::default(),
            world,
            machines,
            hits,
            scenario,
            buckets,
            oracles,
            admitted: 0,
            peak_admitted: 0,
            pinned_slots: vec![0; count],
            peak_pinned_slots: 0,
            turns: 0,
            peer_failures: 0,
            cursor: 0,
            edges: BTreeSet::new(),
            http_exchanges: BTreeMap::new(),
            dependencies: corpus::Dependencies::default(),
            distance,
            completions: Vec::new(),
            reads: 0,
            initiated: vec![0; count],
            served: vec![0; count],
            retired_completed: 0,
            cancelled: 0,
            corrupt: false,
            corruptions: 0,
            fault_targets: BTreeSet::new(),
            candidates: BTreeSet::new(),
            gate: None,
            actions: BTreeSet::new(),
            gate_target: None,
            gate_source: 0,
            pairs: Vec::new(),
            corrupted_edge: None,
            live_sessions: 0,
            registry_owners: Vec::new(),
            registry_misses: 0,
            loaded_pair_diagnostic: std::env::var_os("RACER_DST_LOADED_PAIR_DIAGNOSTIC").is_some(),
            gate_phase: crate::simulation::Phase::Request,
        };
        s.turn();
        for (n, m) in s.machines.iter_mut().enumerate() {
            let v = &m.driver.application_mut().volumes;
            assert_eq!(
                v.servers[&address(n, false)]
                    .handler()
                    .current
                    ._config
                    .local_node()
                    .bytes(),
                if scenario.is_some() {
                    [n as u8 + 10; 32]
                } else {
                    identity(n)
                }
            );
        }
        s
    }
    pub(super) fn boot_machine(
        node: usize,
        config: proto::Snapshot,
        disk: Disk,
        format: bool,
        rdma: bool,
        hits: &Rc<RefCell<Vec<(usize, String)>>>,
        scenario: Option<Scenario>,
        ring: Option<uring::Ring>,
    ) -> Machine {
        let ring = ring.unwrap_or_else(|| {
            let pool = buffers::test_pool(
                buffers::Config::new(
                    NonZeroUsize::new(scenario.map_or(POOL_SLOTS, |s| s.slots)).unwrap(),
                ),
                NumaNodeId(node),
                true,
            );
            uring::Ring::http_test_ring(
                pool,
                uring::Config {
                    entries: RING_SLOTS,
                    requests: RING_SLOTS,
                    fixed_files: 128,
                    completion_budget: 64,
                    ..Default::default()
                },
            )
            .unwrap()
        });
        let mut slab = allocator::Slab::simulated(disk.clone(), DISK, 1, format).unwrap();
        let mut cache =
            crate::cache::tests::cache_from_slab(&mut slab, 1, allocator::Config::default());
        cache.set_metrics(ring.metrics().clone());
        let (mut trust, _) = fixture();
        trust.node = config.node.as_slice().try_into().unwrap();
        let updates = Arc::new(Updates::default());
        updates.subscribe(ring.wake_handle());
        updates
            .publish(if scenario.is_some() {
                crate::control::tests::prepare_cluster_snapshot(&trust, config.clone())
            } else {
                Self::prepare_single_volume(&trust, &config)
            })
            .unwrap();
        let crypto = Arc::new(crate::crypto::Pool::test_pool(ring.pool()));
        // Mixed scenarios exhaust one QP; multi-edge scenarios isolate renewal
        // on alternate rails using the explicit worker index below.
        let rails = if scenario.is_some_and(|s| s.multi_rdma) {
            2
        } else {
            1
        };
        let transports: Vec<_> = (0..if rdma { rails } else { 0 })
            .map(|_| match scenario {
                Some(Scenario {
                    multi_rdma: false, ..
                }) => rdma::test_transport(ring.pool()),
                Some(_) => rdma::test_transport_multi(ring.pool()),
                None => rdma::test_transport_config(ring.pool(), RDMA_QPS, RDMA_DEPTH),
            })
            .collect();
        let mut volumes = Volumes::new(
            cache,
            updates,
            crypto,
            if scenario.is_some() { node } else { 0 },
        );
        if !transports.is_empty() {
            volumes = volumes.with_rdma(Some(
                negotiation::Rails::new(
                    transports.iter().cloned().map(Some).collect(),
                    transports.len(),
                )
                .unwrap(),
            ));
        }
        let origin = Some(http::Server::new(
            http::Listener::bind(
                if scenario.is_some() {
                    format!("127.0.0.1:{}", 11000 + node).parse().unwrap()
                } else {
                    address(node, true)
                },
                NonZeroU32::new(128).unwrap(),
            )
            .unwrap(),
            Origin {
                node,
                hits: hits.clone(),
                scenario: scenario.is_some(),
            },
            http::Config::default(),
        ));
        let app = App {
            volumes,
            origin,
            pending: VecDeque::new(),
            completed: 0,
            bytes: blake3::Hasher::new(),
            outcomes: BTreeMap::new(),
        };
        let mut driver = uring::Driver::new(ring, app, 64).unwrap();
        for transport in &transports {
            driver.add_source(RdmaSource::new(transport.test_source()));
        }
        Machine {
            neighbors: config
                .peers
                .iter()
                .map(|peer| {
                    let id: NodeId = peer.id.parse().unwrap();
                    if scenario.is_some() {
                        (id.bytes()[0] - 10) as usize
                    } else {
                        u64::from_le_bytes(id.bytes()[..8].try_into().unwrap()) as usize
                    }
                })
                .collect(),
            driver,
            disk,
            live: true,
            transports,
            config,
        }
    }
    fn admit(&mut self, request: Request) {
        self.admit_method(request, false);
    }
    fn admit_method(&mut self, request: Request, head: bool) {
        let _scope = self.world.scoped_node(Some(request.node));
        let driver = &mut self.machines[request.node].driver;
        let (app, ring) = driver.parts_mut();
        let range = request.range.map(|(a, b)| format!("bytes={a}-{b}"));
        let headers: Vec<_> = range
            .as_ref()
            .map(|r| vec![("Range", r.as_str())])
            .unwrap_or_default();
        let began = self.world.now();
        let connection =
            client::Connection::new(address(request.node, false), "localhost").unwrap();
        let wire = client::Request::new(&request.target, &headers).unwrap();
        let deadline = began + Duration::from_secs(15);
        let exchange = if head {
            ClientExchange::Head(connection.head(wire, deadline).unwrap())
        } else {
            let fill = ring
                .pool()
                .private_fill()
                .expect("bounded response admission");
            ClientExchange::Get(connection.get(wire, fill, deadline).unwrap())
        };
        self.world.observation(Transition::Invoke {
            request: self.admitted as u64,
            target: request.target.clone(),
            head,
        });
        app.pending.push_back(Pending {
            id: self.admitted as u64,
            refusal: self
                .gate_target
                .as_ref()
                .filter(|(target, refused)| *refused && *target == request.target)
                .and(self.gate)
                .map(|gate| (gate, self.gate_phase)),
            request,
            exchange,
            began,
        });
        crate::workers::Wake::wake(&*driver.wake_handle());
        self.admitted += 1;
        let active = self
            .machines
            .iter()
            .map(|m| m.driver.application().pending.len())
            .sum();
        self.peak_admitted = self.peak_admitted.max(active);
    }
    fn action(&mut self, action: Action) {
        use crate::simulation::Gate;
        let refuse = matches!(&action, Action::Refuse(..));
        let topology_only = matches!(&action, Action::Topology(_));
        match action {
            Action::WallOffset(node, millis) => self.world.wall_offset(Some(node), millis),
            Action::CrashSectors(node, sectors) => {
                self.machines[node].disk.select_crash_sectors(sectors);
                self.reboot(node, false, Some(0));
            }
            Action::Get(request) => self.admit(request),
            Action::Head(request) => self.admit_method(request, true),
            Action::Turn(count) => {
                for _ in 0..count {
                    self.turn();
                }
            }
            Action::Drain => self.drain(),
            Action::Settle => self.settle(),
            Action::ReloadAll => {
                self.settle();
                for node in 0..self.machines.len() {
                    self.action(Action::Reload(node));
                }
                self.settle();
            }
            Action::Cancel(node) => {
                let _scope = self.world.scoped_node(Some(node));
                let (app, ring) = self.machines[node].driver.parts_mut();
                let pending = app
                    .pending
                    .pop_front()
                    .expect("cancel must find a live caller");
                self.world.observation(Transition::Cancel {
                    request: pending.id,
                });
                pending.exchange.cancel(ring).unwrap();
                self.cancelled += 1;
                self.actions.insert("cancel");
                crate::workers::Wake::wake(&*ring.wake_handle());
            }
            Action::Reload(node) | Action::Topology(node) => {
                let _scope = self.world.scoped_node(Some(node));
                let machine = &mut self.machines[node];
                let old = machine.driver.application().volumes.servers[&address(node, false)]
                    .handler()
                    .current
                    .clone();
                machine.config.revision += 1;
                machine.config.epoch += 1;
                if !topology_only {
                    machine.config.volumes[0].cache_generation += 1;
                }
                machine.config.volumes[0].topology.as_mut().unwrap().epoch += 1;
                let revision = machine.config.revision;
                let (mut trust, _) = fixture();
                trust.node = identity(node);
                let config = machine.config.clone();
                machine
                    .driver
                    .application_mut()
                    .volumes
                    .updates
                    .publish(Self::prepare_single_volume(&trust, &config))
                    .unwrap();
                for _ in 0..100 {
                    self.turn();
                    if self.machines[node].driver.application().volumes.servers
                        [&address(node, false)]
                        .handler()
                        .current
                        ._config
                        .config
                        .revision
                        == revision
                    {
                        break;
                    }
                }
                let current = self.machines[node].driver.application().volumes.servers
                    [&address(node, false)]
                    .handler()
                    .current
                    .clone();
                assert!(!old.active.get());
                assert!(current.active.get());
                assert_eq!(
                    current._config.config.revision, revision,
                    "reload must activate, not merely stage"
                );
                assert!(!Rc::ptr_eq(&old, &current));
                self.actions.insert("reload");
            }
            Action::Restart(node) => {
                self.reboot(node, false, Some(0));
                self.turn();
                self.actions.insert("restart");
            }
            Action::OriginOff(node) => {
                self.origin_off(node);
            }
            Action::Durable(node, target) => {
                self.drain();
                let mut ready = false;
                for _ in 0..2000 {
                    self.turn();
                    let cache = &self.machines[node].driver.application().volumes.cache;
                    if crate::cache::tests::durable_ready(&mut cache.borrow_mut(), &target) {
                        ready = true;
                        break;
                    }
                }
                assert!(
                    ready,
                    "both metadata and payload must be durably checkpointed: {target}"
                );
                self.actions.insert("durable");
            }
            Action::CorruptRead => {
                self.corrupt = true;
            }
            Action::Refuse(source, destination, target)
            | Action::Hold(source, destination, target) => {
                let errno = if refuse {
                    Some(libc::ECONNREFUSED)
                } else {
                    None
                };
                self.fault_targets.insert(target.clone());
                let gate = self.world.gate(Gate::new(
                    source,
                    address(destination, false),
                    &target,
                    self.gate_phase,
                    errno,
                ));
                self.gate = Some(gate);
                self.gate_target = Some((target, refuse));
                self.gate_source = source;
            }
            Action::Release => {
                let gate = self.gate.take().expect("gate required");
                assert!(
                    self.world.hits(gate) > 0,
                    "fault injection must hit actual IO"
                );
                self.world.release(gate);
                let (target, refused) = self.gate_target.take().unwrap();
                if refused {
                    if matches!(
                        self.gate_phase,
                        crate::simulation::Phase::Connect | crate::simulation::Phase::Request
                    ) {
                        assert!(
                            self.candidates.contains(&target),
                            "initiated final-hop refusal must advance the candidate"
                        );
                    }
                    let status = self
                        .machines
                        .iter()
                        .find_map(|m| m.driver.application().outcomes.get(&target))
                        .copied()
                        .expect("refused request must have a terminal response");
                    let successor = (owner(&target, self.machines.len()) + 1) % self.machines.len();
                    if status == 200 {
                        assert!(self.candidates.contains(&target));
                        assert!(
                            self.hits
                                .borrow()
                                .iter()
                                .any(|(node, t)| *node == successor && t == &target),
                            "successor must actually serve the fallback"
                        );
                    } else {
                        assert!(matches!(status, 502 | 503));
                    }
                }
                self.actions.insert("gate");
                self.heal(&target);
            }
            Action::AwaitGate => {
                let gate = self.gate.expect("gate required");
                for _ in 0..500 {
                    if self.world.hits(gate) > 0 {
                        break;
                    }
                    self.turn();
                }
                assert!(
                    self.world.hits(gate) > 0,
                    "gate failed to intercept a real attempt"
                );
            }
        }
    }
    pub(super) fn turn(&mut self) {
        let began = self.profile.start();
        let _scheduler = self.world.scoped_node(None);
        self.turns += 1;
        self.world.service_tick();
        // RC reports a failed peer even when that peer can no longer send an
        // ACK. Only previously authenticated reciprocal QPs qualify here.
        let mut notify = Vec::new();
        let failures = &mut self.peer_failures;
        self.pairs.retain(|(source, destination, local, remote)| {
            let healthy = local.pairs_with(remote);
            if !healthy {
                for (node, qp) in [(*source, local), (*destination, remote)] {
                    notify.push((node, qp.clone()));
                }
                *failures += 1;
            }
            healthy
        });
        for (node, qp) in notify {
            self.schedule_peer_failure(node, qp);
        }
        self.deliver_peer_failures();
        // CQ delivery is distinct from SQ effects. Only real Driver
        // completion sources consume CQEs and invoke protocol callbacks.
        let mut delivery = std::mem::take(&mut self.completions);
        let mut queued: BTreeSet<_> = delivery.iter().map(|(node, _, p)| (*node, *p)).collect();
        self.profile.stop(0, began);
        let mut phases = vec![0u64, 1, 2];
        while !phases.is_empty() {
            let selected = match self.phase_policy {
                PhasePolicy::Fixed => 0,
                PhasePolicy::Permuted => self.world.choose_enabled("cluster-phase", &phases),
            };
            let phase = phases.remove(selected);
            if matches!(self.phase_policy, PhasePolicy::Permuted) {
                self.world.observation(Transition::SchedulerPhase { phase });
            }
            let began = self.profile.start();
            match phase {
                0 => self.deliver_completions(&mut delivery),
                1 => self.apply_rdma_effects(&mut queued),
                2 => self.run_ready_workers(),
                _ => unreachable!(),
            }
            if phase != 2 {
                self.profile.stop(0, began);
            }
        }
        self.observe_turn();
    }
    fn schedule_peer_failure(&mut self, node: usize, qp: rdma::TestQp) {
        let _scope = self.world.scoped_node(Some(node));
        if self.peer_failure_delay == 0 {
            qp.disconnect().unwrap();
            crate::workers::Wake::wake(&*self.machines[node].driver.wake_handle());
            return;
        }
        let process = self.world.process();
        if self
            .peer_notifications
            .iter()
            .any(|n| n.process == process && n.qp.same(&qp))
        {
            return;
        }
        let id = self.next_peer_notification;
        self.next_peer_notification += 1;
        let due = self.world.tick() + self.peer_failure_delay;
        self.world.observation(Transition::PeerFailureScheduled {
            notification: id,
            due,
        });
        self.peer_notifications.push(PeerNotification {
            id,
            due,
            process,
            qp,
        });
    }
    fn deliver_peer_failures(&mut self) {
        let mut pending = Vec::new();
        for notification in std::mem::take(&mut self.peer_notifications) {
            if notification.due > self.world.tick() {
                pending.push(notification);
                continue;
            }
            let node = notification.process.node.unwrap();
            let _scope = self.world.scoped_node(Some(node));
            let stale = self.world.process() != notification.process;
            if !stale {
                notification.qp.disconnect().unwrap();
                crate::workers::Wake::wake(&*self.machines[node].driver.wake_handle());
            }
            self.world.observation(Transition::PeerFailureDelivered {
                notification: notification.id,
                due: notification.due,
                stale,
            });
        }
        self.peer_notifications = pending;
    }
    fn deliver_completions(&mut self, delivery: &mut Vec<(usize, rdma::TestQp, rdma::TestPost)>) {
        self.order_completions(delivery);
        for (node, qp, post) in delivery.drain(..) {
            let _scope = self.world.scoped_node(Some(node));
            if !qp.complete(post, 0).unwrap()
                && (qp.posts().contains(&post) || qp.receives().contains(&post))
            {
                self.completions.push((node, qp, post));
            }
        }
    }
    fn apply_rdma_effects(&mut self, queued: &mut BTreeSet<(usize, rdma::TestPost)>) {
        let negotiating = self.machines.iter().any(|m| {
            !m.transports.is_empty()
                && m.driver.application().volumes.servers.values().any(|s| {
                    s.handler().current.manager.as_ref().is_some_and(|manager| {
                        let manager = manager.borrow();
                        manager.outbound.iter().any(|path| path.client.is_some())
                            || !manager.inbound.is_empty()
                    })
                })
        });
        let live_sessions: usize = self
            .machines
            .iter()
            .flat_map(|m| m.driver.application().volumes.servers.values())
            .filter_map(|s| s.handler().current.manager.as_ref())
            .map(|m| m.borrow().live.len())
            .sum();
        if negotiating || live_sessions != self.live_sessions {
            self.live_sessions = live_sessions;
            self.refresh_pairs();
        }
        // Includes negotiated QPs not yet installed in manager.live.
        for (node, peer_node, qp, peer) in &self.pairs {
            if qp.pairs_with(peer) {
                for post in qp.posts() {
                    let _scope = self.world.scoped_node(Some(*node));
                    if self.hold_confirmation && post.opcode == 1 && matches!(post.kind, 5 | 6) {
                        self.held_confirmations += 1;
                        self.world.observation(Transition::ConfirmationHeld {
                            source: *node,
                            destination: *peer_node,
                            kind: post.kind,
                        });
                        break;
                    }
                    if let Some(close) =
                        negotiation::gate(post, Some((*node, address(*peer_node, false))))
                    {
                        if close {
                            qp.disconnect().unwrap();
                            peer.disconnect().unwrap();
                        }
                        break;
                    }
                    if post.opcode == 1 && post.kind == 1 {
                        if let Some(target) = self.world.request_target(&post.value) {
                            if self.oracles.canonical(Oracle::RdmaRank)
                                && !self.fault_targets.contains(&target)
                            {
                                let owner = owner(&target, self.machines.len());
                                assert_eq!(
                                    self.distance[owner][*node],
                                    self.distance[owner][*peer_node] + 1,
                                    "RDMA request must descend the independently computed owner rank"
                                );
                            }
                        }
                    }
                    let corrupt = self.corrupt && post.opcode == 3;
                    if qp.effect(peer, post, corrupt).unwrap() {
                        if corrupt {
                            self.corrupt = false;
                            self.corruptions += 1;
                            self.corrupted_edge = Some((*node, *peer_node));
                            self.world.observation(Transition::RdmaCorruption {
                                source: *node,
                                destination: *peer_node,
                            });
                        }
                        self.reads += usize::from(post.opcode == 3);
                        if post.opcode == 3 {
                            self.initiated[*node] += 1;
                            self.served[*peer_node] += 1;
                            if matches!(self.phase_policy, PhasePolicy::Permuted) {
                                self.world.observation(Transition::RdmaReadEffect {
                                    source: *node,
                                    destination: *peer_node,
                                });
                            }
                        }
                        self.completions.push((*node, qp.clone(), post));
                        queued.insert((*node, post));
                        for receive in peer.receives() {
                            if queued.insert((*peer_node, receive)) {
                                self.completions.push((*peer_node, peer.clone(), receive));
                            }
                        }
                        break;
                    }
                }
            }
        }
    }
    fn run_ready_workers(&mut self) {
        let began = self.profile.start();
        let mut ready = std::collections::VecDeque::from(self.ready_order());
        self.profile.stop(4, began);
        while !ready.is_empty() {
            let selected = if self.loaded_pair_diagnostic
                || matches!(self.phase_policy, PhasePolicy::Permuted)
            {
                let keys: Vec<_> = ready
                    .iter()
                    .map(|node| {
                        let _scope = self.world.scoped_node(Some(*node));
                        (self.world.process().incarnation << 32) | *node as u64
                    })
                    .collect();
                self.world.choose_enabled("cluster-ready-worker", &keys)
            } else {
                0
            };
            let n = ready.remove(selected).unwrap();
            let _scope = self.world.scoped_node(Some(n));
            let driver = &mut self.machines[n].driver;
            let began = self.profile.start();
            if driver.ready() {
                driver.turn().unwrap();
            }
            self.profile.stop(1, began);
            let began = self.profile.start();
            // Observer only: never manufactures readiness or polls a
            // parked application to make a liveness assertion pass.
            let pool = driver.ring_mut().pool().invariant_snapshot();
            self.pinned_slots[n] = pool.refs.iter().filter(|refs| **refs != 0).count();
            for transport in &self.machines[n].transports {
                transport.test_invariants();
            }
            self.profile.stop(2, began);
        }
        self.peak_pinned_slots = self.peak_pinned_slots.max(self.pinned_slots.iter().sum());
    }
    fn observe_turn(&mut self) {
        // Consume observations each turn, before the bounded trace wraps.
        let began = self.profile.start();
        for event in self.world.events_since(&mut self.cursor).unwrap() {
            if matches!(
                event.kind,
                "http-metadata-exchange" | "http-payload-exchange"
            ) {
                let endpoint: SocketAddr = event
                    .detail
                    .strip_prefix("endpoint=")
                    .unwrap()
                    .parse()
                    .unwrap();
                *self
                    .http_exchanges
                    .entry((
                        event.node.unwrap(),
                        endpoint.port() as usize - 10000,
                        event.kind == "http-payload-exchange",
                    ))
                    .or_default() += 1;
            }
            if self.oracles.canonical(Oracle::HealthyRecovery)
                && matches!(event.kind, "http-timeout" | "candidate")
            {
                assert!(
                    self.fault_targets.contains(&event.target),
                    "healthy request required recovery: {event:?}"
                );
                if event.kind == "candidate" {
                    self.candidates.insert(event.target.clone());
                }
            }
            self.dependencies.observe(&event);
            if event.kind == "transport-http"
                && (self.oracles.canonical(Oracle::HttpGraph)
                    || self.oracles.canonical(Oracle::HttpRank))
            {
                let source = event.node.unwrap();
                let endpoint: SocketAddr = event
                    .detail
                    .strip_prefix("endpoint=")
                    .unwrap()
                    .parse()
                    .unwrap();
                let destination = endpoint.port() as usize - 10000;
                let count = self.machines.len();
                let mut degree = 1;
                while degree * degree * degree < count {
                    degree += 1;
                }
                if self.oracles.canonical(Oracle::HttpGraph) {
                    assert!(
                        (0..degree).any(|digit| (source * degree + digit) % count == destination),
                        "off-graph transport: {event:?}"
                    );
                }
                let owner = owner(&event.target, count);
                if self.oracles.canonical(Oracle::HttpRank)
                    && !self.fault_targets.contains(&event.target)
                {
                    assert_eq!(
                        self.distance[owner][source],
                        self.distance[owner][destination] + 1,
                        "transport must strictly reduce independent owner rank: {event:?}"
                    );
                }
                self.edges.insert((source, destination));
            }
        }
        self.profile.stop(3, began);
    }
    fn drain(&mut self) {
        for _ in 0..MAX_TURNS {
            if self
                .machines
                .iter_mut()
                .all(|m| m.driver.application_mut().pending.is_empty())
            {
                return;
            }
            self.turn();
        }
        panic!(
            "cluster stalled: admitted={} tick={}",
            self.admitted,
            self.world.tick()
        );
    }
    fn settle(&mut self) {
        assert!(self.gate.is_none(), "release a held effect before settling");
        self.drain();
        self.quiesce();
        // Cooldown starts after the cancelled/failing attempt has retired.
        // Maintenance may run during this interval; it does not reset health.
        for _ in 0..1100 {
            self.turn();
        }
        self.quiesce();
    }
    fn quiesce(&mut self) {
        for _ in 0..5000 {
            self.turn();
            let began = self.profile.start();
            let quiet = self.machines.iter_mut().all(|machine| {
                let dma = machine
                    .transports
                    .iter()
                    .map(|t| t.test_invariants().2)
                    .sum::<usize>();
                let (app, ring) = machine.driver.parts_mut();
                let pool = ring.pool().invariant_snapshot();
                dma == 0
                    && pool.flights == 0
                    && pool.loading == 0
                    && pool.refs.iter().all(|refs| *refs == 0)
                    && crate::cache::tests::idle(&app.volumes.cache.borrow())
            });
            self.profile.stop(2, began);
            if quiet && self.completions.is_empty() && self.peer_notifications.is_empty() {
                return;
            }
        }
        self.diagnostics();
        panic!("cancelled work or transport ownership did not quiesce before cooldown");
    }
    fn diagnostics(&mut self) {
        eprintln!(
            "DST tick={} turns={} admitted={} peak={} reads={} peer_failures={} queued_cqes={}",
            self.world.tick(),
            self.turns,
            self.admitted,
            self.peak_admitted,
            self.reads,
            self.peer_failures,
            self.completions.len()
        );
        for (node, _, post) in self.completions.iter().take(16) {
            eprintln!("undelivered node={node} post={post:?}");
        }
        for (node, machine) in self.machines.iter_mut().enumerate() {
            let pool = machine.driver.ring_mut().pool().invariant_snapshot();
            let transport: Vec<_> = machine
                .transports
                .iter()
                .map(|t| t.test_invariants())
                .collect();
            if pool.refs.iter().any(|n| *n != 0)
                || pool.flights != 0
                || transport.iter().any(|(_, _, dma)| *dma != 0)
            {
                eprintln!(
                    "node={node} pool={pool:?} transport(total,free,dma)={transport:?} parked={} deadline={:?}",
                    machine.driver.parked(),
                    machine.driver.deadline()
                );
                for transport in &machine.transports {
                    for qp in rdma::test_qps()
                        .iter()
                        .filter(|qp| qp.belongs_to(transport))
                    {
                        eprintln!(
                            "node={node} posts={:?} receives={:?} cq_ready={}",
                            qp.posts(),
                            qp.receives(),
                            qp.source_ready()
                        );
                    }
                }
            }
        }
    }
    fn heal(&mut self, target: &str) {
        self.settle();
        self.fault_targets.remove(target);
        let node = self.gate_source;
        // A cached retry alone cannot prove that the owner/transport probe closed.
        let count = self.machines.len();
        let destination = owner(target, count);
        let cold = (0..count * 128)
            .map(|n| format!("/healed/{}/{n}?exact=%2f", self.admitted))
            .find(|t| owner(t, count) == destination)
            .expect("bounded heal bucket");
        self.admit(get(node, cold.clone()));
        self.drain();
        assert!(
            self.hits
                .borrow()
                .iter()
                .any(|(n, t)| *n == destination && *t == cold),
            "healed retry must reach the original owner, not only a cached successor"
        );
        self.admit(get(node, target));
        self.drain();
        self.actions.insert("heal");
    }
    fn warm(&mut self, edges: &[(usize, usize)]) {
        self.trigger_edges(edges);
        self.wait_warm(edges);
    }
    fn trigger_edges(&mut self, edges: &[(usize, usize)]) {
        for &(source, destination) in edges {
            let _scope = self.world.scoped_node(Some(source));
            let driver = &mut self.machines[source].driver;
            let app = driver.application_mut();
            let generation = &app.volumes.servers[&address(source, false)]
                .handler()
                .current;
            let peer = NodeId::from_bytes(&identity(destination))
                .unwrap()
                .to_string();
            generation
                .manager
                .as_ref()
                .unwrap()
                .borrow_mut()
                .trigger(&peer, &self.buckets[destination][0]);
            crate::workers::Wake::wake(&*driver.wake_handle());
        }
    }
    fn wait_warm(&mut self, edges: &[(usize, usize)]) {
        for _ in 0..2000 {
            self.turn();
            if edges.iter().all(|&(source, destination)| {
                [(source, destination, true), (destination, source, false)]
                    .into_iter()
                    .all(|(node, peer, outbound)| {
                        let generation = &self.machines[node].driver.application().volumes.servers
                            [&address(node, false)]
                            .handler()
                            .current;
                        generation
                            .manager
                            .as_ref()
                            .unwrap()
                            .borrow()
                            .live
                            .iter()
                            .any(|live| {
                                live.outbound.is_some() == outbound
                                    && live.peer.bytes() == identity(peer)
                                    && live.connection.is_healthy()
                            })
                    })
            }) {
                return;
            }
        }
        panic!("negotiation did not install all requested edges");
    }
    pub(super) fn finish(mut self) -> ([u8; 32], Vec<[u8; 32]>) {
        self.drain();
        self.quiesce();
        let count = self.machines.len();
        assert_eq!(
            self.machines
                .iter_mut()
                .map(|m| m.driver.application_mut().completed)
                .sum::<usize>()
                + self.retired_completed
                + self.cancelled,
            self.admitted
        );
        for (node, target) in self
            .hits
            .borrow()
            .iter()
            .filter(|_| self.oracles.canonical(Oracle::OriginPlacement))
        {
            let owner = owner(target, count);
            assert!(
                *node == owner
                    || (self.candidates.contains(target) && *node == (owner + 1) % count),
                "wrong origin for exact target {target}"
            );
        }
        let mut disks = Vec::new();
        for (node, machine) in self.machines.iter_mut().enumerate() {
            let _scope = self.world.scoped_node(Some(node));
            self.world.trace_bytes(
                machine
                    .driver
                    .application_mut()
                    .bytes
                    .clone()
                    .finalize()
                    .as_bytes(),
            );
            machine.driver.shutdown().unwrap();
            machine.live = false;
            self.world.restart_node(Some(node));
            machine.driver.ring_mut().pool().assert_recovered();
            disks.push(machine.disk.digest());
        }
        assert!(
            self.machines.iter_mut().all(|m| m
                .driver
                .ring_mut()
                .pool()
                .invariant_snapshot()
                .flights
                == 0)
        );
        self.machines.clear();
        self.completions.clear();
        self.pairs.clear();
        for _ in 0..10 {
            self.world.service_tick();
        }
        self.world.assert_clean();
        self.world.assert_replay_consumed();
        (self.world.digest(), disks)
    }
}
impl Drop for Cluster {
    fn drop(&mut self) {
        // A failed oracle must not leave old compute callbacks or DMA
        // ownership alive across the failure replay's next fresh execution.
        for (node, machine) in self.machines.iter_mut().enumerate() {
            if !machine.live {
                continue;
            }
            let _scope = self.world.scoped_node(Some(node));
            machine.driver.simulated_crash();
            self.world.restart_node(Some(node));
        }
        self.completions.clear();
        self.pairs.clear();
    }
}

#[test]
fn semantic_event_prefix_replays() {
    for count in 2..=4 {
        let run = |prefix: Option<Vec<crate::simulation::Choice>>| {
            let world = World::new(313);
            let _scope = world.enter();
            world.enable_scheduler();
            if let Some(prefix) = prefix {
                world.replay(prefix);
            }
            let mut cluster = Cluster::with_rdma(world.clone(), count, true);
            let edges = corpus::covering_edges(count);
            cluster.warm(&edges);
            for &(node, destination) in &edges {
                cluster.admit(get(node, cluster.buckets[destination][0].clone()));
            }
            for _ in 0..16 {
                cluster.turn();
            }
            let choices: Vec<_> = world.choices().into_iter().take(4096).collect();
            assert_eq!(
                choices.first().unwrap().index,
                0,
                "prefix must not be a retained suffix"
            );
            (cluster.finish(), choices)
        };
        let (result, prefix) = run(None);
        assert_eq!(result, run(Some(prefix)).0, "nodes={count}");
    }
}
#[test]
fn generated_request_lifecycle() {
    let number = |name, default| {
        std::env::var(name)
            .map(|v| v.parse::<u64>().expect("unsigned campaign parameter"))
            .unwrap_or(default)
    };
    let steps = number("RACER_DST_STEPS", 6) as usize;
    assert!(steps >= 6, "campaign requires its six boundary cases");
    for seed in [3, 19, 71] {
        let seed = number("RACER_DST_SEED", seed);
        for (count, rdma) in [(2, false), (4, true)] {
            Cluster::campaign(
                seed,
                number("RACER_DST_SCHEDULER_SEED", seed ^ 313),
                count,
                rdma,
                steps,
            );
        }
        if std::env::var_os("RACER_DST_SEED").is_some() {
            break;
        }
    }
}
#[test]
fn cold_requests_remain_strict_after_converged_reload() {
    let world = World::new(19);
    let _scope = world.enter();
    world.enable_scheduler();
    let mut cluster = Cluster::with_rdma(world, 4, false);
    for owner in 0..4 {
        cluster.admit(get((owner + 1) % 4, cluster.buckets[owner][0].clone()));
    }
    cluster.action(Action::ReloadAll);
    for owner in 0..4 {
        cluster.admit(get((owner + 1) % 4, cluster.buckets[owner][1].clone()));
    }
    cluster.finish();
}
#[test]
fn enabled_prefix_tree_two_to_four_nodes() {
    const DEPTH: usize = 3;
    for count in 2..=4 {
        let setup = {
            let world = World::new(211);
            let _scope = world.enter();
            world.enable_scheduler();
            let cluster = Cluster::with_rdma(world.clone(), count, false);
            let prefix = world
                .choices()
                .iter()
                .map(|c| c.selected)
                .collect::<Vec<_>>();
            cluster.finish();
            prefix
        };
        let visited = crate::metrics::dst::explore(
            setup,
            DEPTH,
            crate::metrics::dst::prefix_workers(),
            |prefix| {
                let world = World::new(211);
                let _scope = world.enter();
                world.enable_scheduler();
                world.script(prefix.to_vec());
                let mut cluster = Cluster::with_rdma(world.clone(), count, false);
                let target = cluster.buckets[count - 1][0].clone();
                for node in 0..count {
                    cluster.admit(Request {
                        node,
                        target: target.clone(),
                        range: None,
                    });
                }
                let result = cluster.finish();
                (result, world.choices())
            },
        );
        assert!(visited >= count, "enumeration must branch");
    }
}
#[test]
fn action_corpus_cancel_failure_reload_and_durable_restart() {
    for seed in [17, 53] {
        let run = || {
            let world = World::new(seed);
            let _scope = world.enter();
            world.enable_scheduler();
            let mut cluster = Cluster::with_rdma(world, 4, false);
            let local = cluster.buckets[0][0].clone();
            let request = |node, target: &String| Action::Get(get(node, target.clone()));
            cluster.action(request(0, &local));
            cluster.action(Action::Drain);
            cluster.action(request(0, &local));
            cluster.action(Action::Durable(0, local.clone()));
            let durable = cluster.machines[0].disk.digest();
            cluster.action(Action::Restart(0));
            cluster.action(Action::OriginOff(0));
            assert_eq!(
                cluster.machines[0].disk.digest(),
                durable,
                "crash(0) cannot alter the durable sector image"
            );
            let hits = cluster.hits.borrow().len();
            let reads = cluster.world.counts()[30];
            cluster.action(request(0, &local));
            cluster.action(Action::Drain);
            assert_eq!(
                cluster.hits.borrow().len(),
                hits,
                "exact recovery must succeed without origin"
            );
            assert!(
                cluster.world.counts()[30] > reads,
                "recovered bytes must traverse file-backed splice"
            );
            cluster.action(Action::Topology(0));
            cluster.action(request(0, &local));
            cluster.action(Action::Drain);
            assert_eq!(
                cluster.hits.borrow().len(),
                hits,
                "topology-only publication must reuse the cache namespace"
            );
            cluster.action(Action::Restart(0));
            cluster.action(Action::Reload(0));
            cluster.action(request(0, &local));
            cluster.action(Action::Drain);
            assert!(
                cluster.hits.borrow().len() > hits,
                "new generation must not reuse old namespace"
            );
            // Reload all peers before testing remote routes.
            for node in 1..4 {
                cluster.action(Action::Topology(node));
                cluster.action(Action::Reload(node));
            }
            let target = cluster.buckets[1][1].clone();
            for action in corpus::gated_request(0, 1, target, false) {
                cluster.action(action);
            }
            let cancelled_target = cluster.buckets[1][3].clone();
            cluster.action(Action::Hold(0, 1, cancelled_target.clone()));
            cluster.action(request(0, &cancelled_target));
            cluster.action(Action::AwaitGate);
            let process = {
                let _scope = cluster.world.scoped_node(Some(0));
                cluster.world.process()
            };
            cluster.action(Action::Restart(0));
            assert!(
                !cluster.world.is_current(process),
                "old process incarnation must be fenced"
            );
            cluster.action(Action::Release);
            assert_eq!(
                cluster.cancelled, 2,
                "cancel and process death retire exactly two callers"
            );
            let failure = cluster.buckets[1][2].clone();
            for action in corpus::gated_request(0, 1, failure, true) {
                cluster.action(action);
            }
            assert_eq!(
                cluster.actions,
                BTreeSet::from(["cancel", "durable", "gate", "heal", "reload", "restart"])
            );
            let counts = cluster.world.counts();
            assert!(
                [4, 5, 3, 13, 16, 47].iter().all(|i| counts[*i] > 0),
                "file/socket IO seams exercised: {counts:?}"
            );
            cluster.finish()
        };
        assert_eq!(run(), run(), "action seed={seed}");
    }
}
#[test]
fn transport_phase_cancel_and_refusal_table() {
    use crate::simulation::Phase;
    for phase in [
        Phase::Connect,
        Phase::Request,
        Phase::Headers,
        Phase::PartialBody,
    ] {
        for refuse in [false, true] {
            let world = World::new(251);
            let _scope = world.enter();
            world.enable_scheduler();
            world.short_transfers(31);
            let mut cluster = Cluster::with_rdma(world, 4, false);
            cluster.gate_phase = phase;
            let target = cluster.buckets[1][0].clone();
            for action in corpus::gated_request(0, 1, target, refuse) {
                cluster.action(action);
            }
            cluster.finish();
        }
    }
}
#[test]
fn managed_rdma_uses_real_sources_and_reads() {
    let world = World::new(173);
    let _scope = world.enter();
    world.enable_scheduler();
    world.limits(100_000, 20_000_000, 65536);
    let mut cluster = Cluster::with_rdma(world, 8, true);
    cluster.warm(&[(0, 1), (1, 3), (3, 7)]);
    let before = cluster.reads;
    let target = cluster.buckets[7][1].clone();
    cluster.admit(get(0, target));
    cluster.drain();
    assert!(
        cluster.reads > before,
        "fresh data must use negotiated READs, not only HTTP"
    );
    cluster.action(Action::CorruptRead);
    cluster.edges.clear();
    let target = cluster.buckets[7][2].clone();
    cluster.action(Action::Get(get(0, target.clone())));
    cluster.action(Action::Drain);
    assert_eq!(
        cluster.corruptions, 1,
        "corruption must hit an actual READ destination"
    );
    assert!(
        !cluster.candidates.contains(&target),
        "RDMA corruption must fall back on the same HTTP peer"
    );
    assert!(
        cluster.edges.contains(&cluster.corrupted_edge.unwrap()),
        "corrupt READ must cause actual same-edge HTTP fallback"
    );
    cluster.quiesce();
    assert!(
        cluster.peer_failures > 0,
        "fabric must notify the counterpart of a disconnected authenticated QP"
    );
    cluster.finish();
}
#[test]
#[ignore = "large cluster: run explicitly in a memory-limited, swap-disabled process"]
fn active_1024_synchronous_wave_and_large_pairs() {
    let world = World::new(crate::metrics::dst::scale_seed(1024));
    let _scope = world.enter();
    world.enable_scheduler();
    world.limits(100_000, 1_000_000_000, 65536);
    let count = std::env::var("RACER_DST_NODES").map_or(NODES, |value| {
        value.parse().expect("RACER_DST_NODES must be an integer")
    });
    assert!(
        (2..=NODES).contains(&count),
        "RACER_DST_NODES must be in 2..=1024"
    );
    let edges = corpus::covering_edges(count);
    eprintln!(
        "DST scale nodes={count} target={NODES} pool_slots={POOL_SLOTS} buffer_bytes={} ring_slots={RING_SLOTS} rails=1 qps={RDMA_QPS} depth={RDMA_DEPTH} control_bytes={} payload_virtual_bytes={}",
        buffers::BUFFER_SIZE,
        count * 4 * RDMA_QPS * RDMA_DEPTH * 4096,
        count as u64 * POOL_SLOTS as u64 * buffers::BUFFER_SIZE as u64
    );
    eprintln!(
        "DST scale edge_cover={} degree={} initial_wave={} cold_round_limit=8 minimum_reads_per_role=8",
        edges.len(),
        corpus::degree(count),
        count * 4
    );
    let mut cluster = Cluster::with_rdma(world.clone(), count, true);
    assert!(
        cluster
            .machines
            .iter()
            .all(|m| m.transports[0].test_invariants().0 == 4 * RDMA_QPS * RDMA_DEPTH)
    );
    for batch in edges.chunks(256) {
        cluster.warm(batch);
    }
    let live: usize = cluster
        .machines
        .iter()
        .flat_map(|m| m.driver.application().volumes.servers.values())
        .filter_map(|s| s.handler().current.manager.as_ref())
        .map(|m| m.borrow().live.len())
        .sum();
    assert_eq!(
        live,
        edges.len() * 2,
        "every warmed edge requires both authenticated endpoints"
    );
    eprintln!(
        "DST scale activated_nodes={} authenticated_qp_ends={live} control_slots_per_node={} rpc_depth_per_qp={RDMA_DEPTH}",
        cluster.machines.len(),
        4 * RDMA_QPS * RDMA_DEPTH
    );
    let tick = world.tick();
    let phase = cluster.profile.start();
    // One synchronous wave, with all selected machines coexisting.
    for node in 0..count {
        for slot in 0..4 {
            let owner = edges[node].1;
            cluster.admit(Request {
                node,
                target: cluster.buckets[owner][slot].clone(),
                range: None,
            });
        }
    }
    assert_eq!(cluster.admitted, count * 4);
    assert_eq!(cluster.peak_admitted, count * 4);
    assert_eq!(world.tick(), tick);
    cluster.drain();
    cluster.profile.stop(5, phase);
    if edges.len() == count {
        assert_eq!(
            cluster
                .hits
                .borrow()
                .iter()
                .map(|(node, _)| *node)
                .collect::<BTreeSet<_>>()
                .len(),
            count,
            "every origin must serve the initial wave"
        );
    }
    let baseline = (cluster.initiated.clone(), cluster.served.clone());
    corpus::read_coverage(
        "burst",
        &baseline.0,
        &baseline.1,
        &(vec![0; count], vec![0; count]),
        8,
    );
    cluster.settle();
    let phase = cluster.profile.start();
    let mut covered = false;
    for round in 0..8 {
        for (wave, edges) in corpus::edge_waves(&edges).iter().enumerate() {
            cluster.warm(edges);
            let targets = corpus::cold_targets(count, round * count + wave);
            for &(source, destination) in edges {
                cluster.admit(get(source, targets[destination].clone()));
            }
            cluster.drain();
            cluster.quiesce();
        }
        covered = corpus::read_coverage(
            &format!("cold round={round}"),
            &cluster.initiated,
            &cluster.served,
            &baseline,
            8,
        );
        if covered {
            break;
        }
        cluster.settle();
    }
    assert!(
        covered,
        "bounded cold waves must add >=8 initiated AND served READs at every node"
    );
    cluster.profile.stop(6, phase);
    let phase = cluster.profile.start();
    let paths = corpus::relay_paths(count, &cluster.distance);
    assert!(
        count <= 3 || !paths.is_empty(),
        "scale must exercise real relay routes"
    );
    for (index, path) in paths.iter().enumerate() {
        cluster.settle();
        let route: Vec<_> = path.windows(2).map(|p| (p[0], p[1])).collect();
        cluster.warm(&route);
        let before = (cluster.initiated.clone(), cluster.served.clone());
        let targets = corpus::cold_targets(count, 100_000 + index);
        cluster.edges.clear();
        cluster.http_exchanges.clear();
        cluster.admit(get(path[0], targets[*path.last().unwrap()].clone()));
        cluster.drain();
        cluster.quiesce();
        eprintln!(
            "DST relay path={path:?} HTTP={:?} initiated={:?} served={:?}",
            cluster.edges,
            route
                .iter()
                .map(|(a, _)| (*a, cluster.initiated[*a] - before.0[*a]))
                .collect::<Vec<_>>(),
            route
                .iter()
                .map(|(_, b)| (*b, cluster.served[*b] - before.1[*b]))
                .collect::<Vec<_>>()
        );
        assert_eq!(
            cluster.edges,
            route.iter().copied().collect(),
            "cold metadata must traverse exactly the independent relay route"
        );
        let expected_http: BTreeMap<_, _> = route
            .iter()
            .map(|&(source, destination)| ((source, destination, false), 1))
            .collect();
        assert_eq!(
            cluster.http_exchanges, expected_http,
            "each relay hop must exchange metadata once over HTTP, with no payload fallback"
        );
        for &(source, destination) in &route {
            assert!(
                cluster.initiated[source] >= before.0[source] + 1
                    && cluster.served[destination] >= before.1[destination] + 1,
                "relay {source}->{destination} missing payload READ: initiated={:?} served={:?}",
                cluster.initiated,
                cluster.served
            );
        }
    }
    let before_large = cluster.admitted;
    cluster.profile.stop(7, phase);
    let phase = cluster.profile.start();
    let diagnostic = cluster.loaded_pair_diagnostic;
    if diagnostic {
        cluster.loaded_large_pairs(&edges);
    } else {
        cluster.large_matrix(&edges, 16, Some(1));
    }
    assert_eq!(
        cluster.admitted,
        before_large + if diagnostic { 32 } else { 16 * 8 }
    );
    cluster.profile.stop(8, phase);
    cluster.profile.report(count, cluster.turns);
    eprintln!(
        "DST scale complete nodes={count} admitted={} peak={} turns={} reads={} initiated={:?} served={:?}",
        cluster.admitted,
        cluster.peak_admitted,
        cluster.turns,
        cluster.reads,
        cluster.initiated,
        cluster.served
    );
    eprintln!(
        "DST registry_owner_misses={} loaded_pair_diagnostic={diagnostic}",
        cluster.registry_misses
    );
    eprintln!(
        "DST scheduling branching_choices={} turns={} choices_per_turn={:.2}",
        world.choice_count(),
        cluster.turns,
        world.choice_count() as f64 / cluster.turns.max(1) as f64
    );
    eprintln!(
        "DST scale peak_pinned_slots={} pinned_capacity_bytes={} (capacity, not measured RSS)",
        cluster.peak_pinned_slots,
        cluster.peak_pinned_slots as u64 * buffers::BUFFER_SIZE as u64
    );
    cluster.finish();
}
#[test]
#[ignore = "higher-cardinality failures: explicit serial capped verification"]
fn scale_failure_recovery_acceptance() {
    let count =
        std::env::var("RACER_DST_NODES").map_or(NODES, |v| v.parse().expect("integer node count"));
    assert!((4..=NODES).contains(&count));
    let world = World::new(crate::metrics::dst::scale_seed(1024));
    let _scope = world.enter();
    world.enable_scheduler();
    let mut cluster = Cluster::with_rdma(world.clone(), count, true);
    let edges = corpus::covering_edges(count);
    let samples: BTreeSet<_> = [0, count / 2, count - 1].into_iter().collect();
    let selected: Vec<_> = samples.iter().map(|node| edges[*node]).collect();
    for (source, destination) in selected {
        cluster.warm(&[(source, destination)]);
        let before = cluster.reads;
        cluster.action(Action::CorruptRead);
        cluster.admit(get(source, cluster.buckets[destination][0].clone()));
        cluster.drain();
        assert!(cluster.reads > before && !cluster.corrupt);
        cluster.quiesce();
        for transport in &cluster.machines[source].transports {
            transport.shutdown().unwrap();
        }
        for action in corpus::gated_request(
            source,
            destination,
            cluster.buckets[destination][1].clone(),
            true,
        ) {
            cluster.action(action);
        }
        let local = cluster.buckets[source][3].clone();
        cluster.admit(get(source, local.clone()));
        cluster.action(Action::Durable(source, local.clone()));
        let image = cluster.machines[source].disk.digest();
        cluster.action(Action::Restart(source));
        cluster.action(Action::OriginOff(source));
        let hits = cluster.hits.borrow().len();
        cluster.admit(get(source, local));
        cluster.drain();
        assert_eq!(cluster.hits.borrow().len(), hits);
        assert_eq!(cluster.machines[source].disk.digest(), image);
        cluster.action(Action::Restart(source));
        cluster.warm(&[(source, destination)]);
        let reads = cluster.initiated[source];
        cluster.admit(get(source, cluster.buckets[destination][2].clone()));
        cluster.drain();
        assert!(
            cluster.initiated[source] > reads,
            "restart must restore actual RDMA READ service"
        );
    }
    assert_eq!(cluster.corruptions, samples.len());
    assert!(cluster.actions.contains("heal") && cluster.actions.contains("durable"));
    cluster.finish();
}
