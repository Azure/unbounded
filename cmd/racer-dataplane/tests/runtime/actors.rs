// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Cooperative overlap cell. Only the coordinator advances the production drivers.
use super::*;
use crate::simulation::{Gate, Phase, history::require};

pub(super) fn shared_workers(cluster: &mut Cluster) {
    shared_workers_policy(cluster, false);
}

fn boot_shared_worker(cluster: &mut Cluster, disk: Disk, format: bool) -> Machine {
    let world = cluster.world.clone();
    {
        let _scope = world.scoped_worker(Some(0), 1);
        let ring = uring::Ring::http_test_ring(
            cluster.machines[0]
                .driver
                .ring_mut()
                .pool()
                .test_other_worker(),
            uring::Config {
                entries: RING_SLOTS,
                requests: RING_SLOTS,
                fixed_files: 128,
                completion_budget: 64,
                ..Default::default()
            },
        )
        .unwrap();
        let mut machine = Cluster::boot_machine(
            0,
            cluster.machines[0].config.clone(),
            disk,
            format,
            false,
            &cluster.hits,
            None,
            Some(ring),
        );
        let shared = &cluster.machines[0].driver.application().volumes;
        let wake = machine.driver.wake_handle();
        let app = machine.driver.application_mut();
        app.origin = None;
        app.volumes.updates = shared.updates.clone();
        app.volumes.updates.subscribe(wake);
        app.volumes.crypto = shared.crypto.clone();
        app.volumes.worker = 1;
        machine.driver.turn().unwrap();
        machine
    }
}

pub(super) fn shared_workers_policy(cluster: &mut Cluster, crash: bool) {
    let world = cluster.world.clone();
    let mut other = boot_shared_worker(cluster, Disk::new(DISK), true);
    let target = cluster.buckets[1][0].clone();
    let gate = world.gate(Gate::new(
        0,
        address(1, false),
        &target,
        Phase::Request,
        None,
    ));
    world.observation(Transition::FaultArmed {
        fault: gate,
        target: target.clone(),
    });
    let mut cursor = cluster.cursor;
    for _ in 0..8 {
        cluster.admit(get(0, target.clone()));
    }
    let mut accepted = BTreeSet::new();
    let mut accepted_count = 0;
    let mut joined = false;
    let deadline = world.tick() + 1000;
    loop {
        {
            let _scope = world.scoped_worker(Some(0), 1);
            if other.driver.ready() {
                other.driver.turn().unwrap();
            }
            other.driver.ring_mut().pool().invariant_snapshot();
        }
        for event in world.events_since(&mut cursor).unwrap() {
            if event.node == Some(0) && event.incarnation == 0 {
                if event.kind == "volume-accept" && event.target == target {
                    accepted_count += 1;
                    if accepted.insert(event.worker) {
                        world.observation(Transition::SharedWorkerAccepted {
                            worker: event.worker,
                        });
                    }
                }
                joined |= event.kind == "network-join";
            }
        }
        if accepted.len() == 2 && accepted_count == 8 && joined && world.hits(gate) > 0 {
            break;
        }
        require(
            world.tick() < deadline,
            "workers.shared-flight",
            "both reuse-port workers must accept and join the held shared flight",
        );
        cluster.turn();
    }
    world.observation(Transition::SharedWorkerJoined {
        target: target.clone(),
    });
    world.observation(Transition::FaultEffective { fault: gate });
    {
        let _scope = world.scoped_node(Some(0));
        let machine = &mut cluster.machines[0];
        machine.config.revision += 1;
        machine.config.epoch += 1;
        let (mut trust, _) = fixture();
        trust.node = identity(0);
        machine
            .driver
            .application()
            .volumes
            .updates
            .publish(Cluster::prepare_single_volume(&trust, &machine.config))
            .unwrap();
        world.observation(Transition::Publish { revision: 2 });
    }
    loop {
        {
            let _scope = world.scoped_worker(Some(0), 1);
            if other.driver.ready() {
                other.driver.turn().unwrap();
            }
        }
        cluster.turn();
        if other.driver.application().volumes.revision == 2
            && cluster.machines[0].driver.application().volumes.revision == 2
        {
            break;
        }
        require(
            world.tick() < deadline,
            "workers.shared-publication",
            "both workers must activate the process publication while callers are held",
        );
    }
    require(
        cluster.machines[0].driver.application().pending.len() == 8,
        "workers.live-publication",
        "held shared callers must overlap both worker activations",
    );
    world.observation(Transition::SharedWorkersActivated { revision: 2 });
    if crash {
        let retired_pool = other.driver.ring_mut().pool().test_other_worker();
        let disk = other.disk.clone();
        let incarnation;
        {
            let _scope = world.scoped_worker(Some(0), 1);
            incarnation = world.process().incarnation;
            other.driver.simulated_crash();
            other.disk.crash(0);
            drop(other);
            world.observation(Transition::SharedWorkerCrashed { worker: 1 });
        }
        cluster.reboot(0, false, Some(0));
        retired_pool.assert_recovered();
        world.observation(Transition::SharedWorkerCrashed { worker: 0 });
        require(
            cluster.cancelled == 8,
            "workers.process-retirement",
            "the process crash must retire all eight held callers",
        );
        world.release(gate);
        world.observation(Transition::FaultReleased { fault: gate });
        cluster.admit(get(0, cluster.buckets[0][1].clone()));
        cluster.drain();
        require(
            cluster.machines[0].driver.application().completed == 1,
            "workers.process-recovery",
            "the new process listener must serve an independently checked response",
        );
        reconstruct_shared_workers(cluster, disk, incarnation);
        retired_pool.assert_recovered();
        {
            let _scope = world.scoped_node(Some(0));
            world.observation(Transition::SharedProcessRecovered { retired: 8 });
        }
        restart_shared_process_for_followups(cluster);
        return;
    }
    world.release(gate);
    world.observation(Transition::FaultReleased { fault: gate });
    while !cluster.machines[0].driver.application().pending.is_empty() {
        require(
            world.tick() < deadline,
            "workers.progress",
            "all shared-flight callers must complete",
        );
        {
            let _scope = world.scoped_worker(Some(0), 1);
            if other.driver.ready() {
                other.driver.turn().unwrap();
            }
        }
        cluster.turn();
    }
    require(
        cluster.machines[0].driver.application().completed == 8,
        "workers.responses",
        "every joined caller must pass the independent response oracle",
    );
    require(
        cluster
            .hits
            .borrow()
            .iter()
            .filter(|(node, key)| *node == 1 && *key == target)
            .count()
            == 2,
        "workers.single-producer",
        "the shared callers must fetch one origin metadata response and one page across workers",
    );
    {
        let _scope = world.scoped_worker(Some(0), 1);
        other.driver.shutdown().unwrap();
        drop(other);
        world.observation(Transition::SharedWorkerRetired { worker: 1 });
    }
    cluster.admit(get(0, cluster.buckets[0][1].clone()));
    cluster.drain();
    require(
        cluster.machines[0].driver.application().completed == 9,
        "workers.listener-survival",
        "closing one worker must retain the process listener group",
    );
    restart_shared_process_for_followups(cluster);
}

fn restart_shared_process_for_followups(cluster: &mut Cluster) {
    // Updates membership lasts for the process lifetime. Worker 1's listener
    // retirement above is not an unsubscribe. Cross a real process boundary
    // before returning to the single-worker cluster's arbitrary follow-up actions.
    cluster.quiesce();
    let world = cluster.world.clone();
    let incarnation = {
        let _scope = world.scoped_node(Some(0));
        world.process().incarnation
    };
    let revision = cluster.machines[0].config.revision;
    let cancelled = cluster.cancelled;
    let completed = cluster.retired_completed + cluster.machines[0].driver.application().completed;
    let retired_pool = cluster.machines[0]
        .driver
        .ring_mut()
        .pool()
        .test_other_worker();
    cluster.reboot(0, false, Some(0));
    retired_pool.assert_recovered();
    let _scope = world.scoped_node(Some(0));
    require(
        world.process().incarnation == incarnation + 1
            && cluster.cancelled == cancelled
            && cluster.retired_completed == completed,
        "workers.followup-process-boundary",
        "follow-up handoff must restart the process without losing callers or completion accounting",
    );
    let deadline = world.tick() + 1000;
    loop {
        cluster.turn();
        let volumes = &cluster.machines[0].driver.application().volumes;
        let status = volumes.updates.status();
        if status["workers"] == 1
            && status["activatedWorkers"] == 1
            && status["activeRevision"] == revision
            && volumes
                .servers
                .get(&address(0, false))
                .is_some_and(|server| server.handler().current._config.config.revision == revision)
        {
            break;
        }
        require(
            world.tick() < deadline,
            "workers.followup-authority",
            "fresh single-worker process must activate its boot publication before follow-up actions",
        );
    }
    world.observation(Transition::SharedProcessFollowupReady { revision });
}

fn reconstruct_shared_workers(cluster: &mut Cluster, disk: Disk, retired_incarnation: u64) {
    let world = cluster.world.clone();
    let mut other = boot_shared_worker(cluster, disk, false);
    let incarnation = {
        let _scope = world.scoped_node(Some(0));
        world.process().incarnation
    };
    require(
        incarnation == retired_incarnation + 1,
        "workers.reconstructed-incarnation",
        "both reconstructed workers must belong to the next process incarnation",
    );
    for worker in [0, 1] {
        let _scope = world.scoped_worker(Some(0), worker);
        world.observation(Transition::SharedWorkerReconstructed { worker });
    }
    // First hold callers across a new publication; then prove fresh callers on
    // both listener members select that publication rather than the boot revision.
    for (revision, bucket) in [(2, 1), (3, 2)] {
        let target = cluster.buckets[1][bucket].clone();
        let gate = world.gate(Gate::new(
            0,
            address(1, false),
            &target,
            Phase::Request,
            None,
        ));
        world.observation(Transition::FaultArmed {
            fault: gate,
            target: target.clone(),
        });
        let completed = cluster.machines[0].driver.application().completed;
        let mut cursor = cluster.cursor;
        for _ in 0..8 {
            cluster.admit(get(0, target.clone()));
        }
        let mut accepted = [0usize; 2];
        let mut joined = false;
        let deadline = world.tick() + 1000;
        loop {
            {
                let _scope = world.scoped_worker(Some(0), 1);
                if other.driver.ready() {
                    other.driver.turn().unwrap();
                }
                other.driver.ring_mut().pool().invariant_snapshot();
            }
            cluster.turn();
            for event in world.events_since(&mut cursor).unwrap() {
                if event.node != Some(0) {
                    continue;
                }
                if event.kind == "volume-accept" && event.target == target {
                    require(
                        event.incarnation == incarnation
                            && event.worker < 2
                            && event.detail == format!("revision={revision}"),
                        "workers.reconstructed-authority",
                        "fresh shared-listener requests must select the current incarnation and revision",
                    );
                    accepted[event.worker as usize] += 1;
                }
                joined |= event.incarnation == incarnation && event.kind == "network-join";
            }
            if accepted.iter().all(|count| *count > 0)
                && accepted.iter().sum::<usize>() == 8
                && joined
                && world.hits(gate) > 0
            {
                break;
            }
            require(
                world.tick() < deadline,
                "workers.reconstructed-shared-flight",
                "both reconstructed workers must accept all eight callers and join the held flight",
            );
        }
        world.observation(Transition::FaultEffective { fault: gate });
        if revision == 2 {
            {
                let _scope = world.scoped_node(Some(0));
                let machine = &mut cluster.machines[0];
                machine.config.revision += 1;
                machine.config.epoch += 1;
                let (mut trust, _) = fixture();
                trust.node = identity(0);
                machine
                    .driver
                    .application()
                    .volumes
                    .updates
                    .publish(Cluster::prepare_single_volume(&trust, &machine.config))
                    .unwrap();
                world.observation(Transition::Publish { revision: 3 });
            }
            loop {
                {
                    let _scope = world.scoped_worker(Some(0), 1);
                    if other.driver.ready() {
                        other.driver.turn().unwrap();
                    }
                    other.driver.ring_mut().pool().invariant_snapshot();
                }
                cluster.turn();
                if [&cluster.machines[0], &other].into_iter().all(|machine| {
                    machine.driver.application().volumes.servers[&address(0, false)]
                        .handler()
                        .current
                        ._config
                        .config
                        .revision
                        == 3
                }) {
                    break;
                }
                require(
                    world.tick() < deadline,
                    "workers.reconstructed-publication",
                    "both reconstructed listener handlers must activate the new publication",
                );
            }
            let _scope = world.scoped_node(Some(0));
            let app = cluster.machines[0].driver.application();
            let status = app.volumes.updates.status();
            require(
                app.pending.len() == 8
                    && status["workers"] == 2
                    && status["activatedWorkers"] == 2
                    && status["activeRevision"] == 3,
                "workers.reconstructed-live-publication",
                "the new process must acknowledge both workers with all eight callers still held",
            );
            world.observation(Transition::SharedWorkersReactivated { revision: 3 });
        }
        world.release(gate);
        world.observation(Transition::FaultReleased { fault: gate });
        while !cluster.machines[0].driver.application().pending.is_empty() {
            require(
                world.tick() < deadline,
                "workers.reconstructed-progress",
                "all callers accepted by the reconstructed workers must complete",
            );
            {
                let _scope = world.scoped_worker(Some(0), 1);
                if other.driver.ready() {
                    other.driver.turn().unwrap();
                }
                other.driver.ring_mut().pool().invariant_snapshot();
            }
            cluster.turn();
        }
        require(
            cluster.machines[0].driver.application().completed == completed + 8,
            "workers.reconstructed-responses",
            "all eight reconstructed-worker responses must pass the independent response oracle",
        );
        require(
            cluster
                .hits
                .borrow()
                .iter()
                .filter(|(node, key)| *node == 1 && *key == target)
                .count()
                == 2,
            "workers.reconstructed-single-producer",
            "reconstructed workers must share one origin metadata response and one page",
        );
        for (worker, requests) in accepted.into_iter().enumerate() {
            let worker = worker as u32;
            let _scope = world.scoped_worker(Some(0), worker);
            world.observation(Transition::SharedWorkerRecovered {
                worker,
                revision,
                requests,
            });
        }
    }
    {
        let _scope = world.scoped_worker(Some(0), 1);
        other.driver.shutdown().unwrap();
        world.trace_bytes(&other.disk.digest());
        drop(other);
        world.observation(Transition::SharedWorkerRetired { worker: 1 });
    }
    cluster.admit(get(0, cluster.buckets[0][2].clone()));
    cluster.drain();
    require(
        cluster.machines[0].driver.application().completed == 18,
        "workers.reconstructed-listener-survival",
        "retiring the reconstructed second worker must preserve the new process listener",
    );
}

#[test]
fn shared_process_workers_join_and_retire_without_losing_listener() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        shared_workers(&mut cluster);
        cluster.action(Action::Reload(0));
        cluster.finish();
    }
}

#[test]
fn shared_process_crash_retires_both_workers_with_live_callers() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        shared_workers_policy(&mut cluster, true);
        cluster.action(Action::Reload(0));
        let mut cursor = cluster.cursor;
        let target = "/shared-process-follow-up?exact=%2f";
        cluster.admit(get(0, target));
        let mut accepted = Vec::new();
        let deadline = cluster.world.tick() + 1000;
        while !cluster.machines[0].driver.application().pending.is_empty() {
            cluster.turn();
            accepted.extend(
                cluster
                    .world
                    .events_since(&mut cursor)
                    .unwrap()
                    .into_iter()
                    .filter(|event| {
                        event.node == Some(0)
                            && event.kind == "volume-accept"
                            && event.target == target
                    }),
            );
            require(
                cluster.world.tick() < deadline,
                "workers.followup-progress",
                "fresh request must complete after follow-up reload",
            );
        }
        let status = cluster.machines[0]
            .driver
            .application()
            .volumes
            .updates
            .status();
        require(
            accepted.len() == 1
                && accepted[0].worker == 0
                && accepted[0].incarnation == 2
                && accepted[0].detail == "revision=4"
                && status["workers"] == 1
                && status["activatedWorkers"] == 1
                && status["activeRevision"] == 4
                && cluster.machines[0].driver.application().completed == 1
                && cluster.retired_completed == 18
                && cluster.cancelled == 8,
            "workers.followup-reload",
            "follow-up reload must activate and serve strict-oracle traffic in the new single-worker process",
        );
        cluster.finish();
    }
}

pub(super) fn rdma_recovery(cluster: &mut Cluster) {
    let old = cluster.pairs.clone();
    let target = cluster.buckets[7][2].clone();
    cluster.edges.clear();
    cluster.action(Action::CorruptRead);
    cluster.admit(get(0, target.clone()));
    // Admit independent traffic before the coordinator advances either request.
    cluster.admit(get(4, cluster.buckets[4][3].clone()));
    cluster.drain();
    require(
        cluster.corruptions == 1,
        "rdma.corruption-effect",
        "one actual READ must be corrupted",
    );
    let (source, destination) = cluster.corrupted_edge.unwrap();
    require(
        !cluster.candidates.contains(&target) && cluster.edges.contains(&(source, destination)),
        "rdma.same-edge-fallback",
        "corrupt READ must fall back to HTTP on the same peer without candidate advancement",
    );
    cluster.world.observation(Transition::SameEdgeHttpFallback {
        source,
        destination,
        target,
    });
    cluster.settle();
    require(
        cluster.peer_failures > 0,
        "rdma.peer-retirement",
        "failed authenticated QP must notify its counterpart",
    );
    cluster.warm(&[(source, destination)]);
    let replacements: Vec<_> = cluster
        .pairs
        .iter()
        .filter(|(a, b, _, _)| *a == source && *b == destination)
        .collect();
    require(
        replacements
            .iter()
            .any(|(_, _, qp, _)| !old.iter().any(|(_, _, prior, _)| qp.same(prior))),
        "rdma.session-replacement",
        "recovery must authenticate a new QP on the failed edge",
    );
    let reads = (cluster.initiated[source], cluster.served[destination]);
    cluster.admit(get(source, cluster.buckets[destination][1].clone()));
    cluster.drain();
    require(
        cluster.initiated[source] > reads.0 && cluster.served[destination] > reads.1,
        "rdma.replacement-read",
        "fresh traffic must return to RDMA on the replaced edge",
    );
    cluster.world.observation(Transition::RdmaReplacementRead {
        source,
        destination,
    });
}

#[test]
fn corrupt_read_falls_back_and_replaces_authenticated_session() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 8, true);
        cluster.phase_policy = PhasePolicy::Permuted;
        cluster.warm(&corpus::covering_edges(8));
        rdma_recovery(&mut cluster);
        cluster.finish();
    }
}

struct FaultActor {
    gates: Vec<usize>,
    stage: u8,
    published: bool,
    deadline: u64,
}
impl FaultActor {
    fn setup(cluster: &mut Cluster, seed: u64) -> Self {
        let mut gates = Vec::new();
        for source in 0..2 {
            let target = cluster.buckets[1 - source][0].clone();
            cluster.fault_targets.insert(target.clone());
            let gate = cluster.world.gate(Gate::new(
                source,
                address(1 - source, false),
                &target,
                Phase::Request,
                None,
            ));
            cluster.world.observation(Transition::FaultArmed {
                fault: gate,
                target: target.clone(),
            });
            gates.push(gate);
            cluster.admit(get(source, target));
        }
        Self {
            gates,
            stage: 0,
            published: false,
            deadline: cluster.world.tick() + 1000 + seed % 31,
        }
    }
    fn step(&mut self, cluster: &mut Cluster) -> bool {
        require(
            cluster.world.tick() < self.deadline,
            "overlap.fault-budget",
            "fault actor exceeded its completion budget",
        );
        match self.stage {
            0 => {
                if !self.gates.iter().all(|gate| cluster.world.hits(*gate) > 0) {
                    return false;
                }
                for &gate in &self.gates {
                    cluster
                        .world
                        .observation(Transition::FaultEffective { fault: gate });
                }
                // Publish both topology revisions without turning or draining here.
                for node in 0..2 {
                    let _scope = cluster.world.scoped_node(Some(node));
                    let machine = &mut cluster.machines[node];
                    machine.config.revision += 1;
                    machine.config.epoch += 1;
                    machine.config.volumes[0].topology.as_mut().unwrap().epoch += 1;
                    let (mut trust, _) = fixture();
                    trust.node = identity(node);
                    machine
                        .driver
                        .application()
                        .volumes
                        .updates
                        .publish(Cluster::prepare_single_volume(&trust, &machine.config))
                        .unwrap();
                    cluster.world.observation(Transition::Publish {
                        revision: machine.config.revision,
                    });
                }
                self.stage = 1;
            }
            1 => {
                if !(0..2).all(|node| {
                    cluster.machines[node].driver.application().volumes.servers
                        [&address(node, false)]
                        .handler()
                        .current
                        ._config
                        .config
                        .revision
                        == 2
                }) {
                    return false;
                }
                require(
                    cluster
                        .machines
                        .iter()
                        .all(|m| !m.driver.application().pending.is_empty()),
                    "overlap.live-publication",
                    "publication must activate while both gated callers are live",
                );
                self.published = true;
                cluster.action(Action::Cancel(0));
                // No action helper turn: crash is one actor step, then the coordinator turns.
                cluster.reboot(1, false, Some(0));
                self.stop(cluster);
                self.stage = 2;
            }
            _ => return true,
        }
        false
    }
    fn stop(&self, cluster: &mut Cluster) {
        for &gate in &self.gates {
            cluster.world.release(gate);
            cluster
                .world
                .observation(Transition::FaultReleased { fault: gate });
        }
    }
    fn check(&self, cluster: &Cluster) {
        require(
            self.stage == 2 && self.published && cluster.cancelled == 2,
            "overlap.witnesses",
            "missing publication, cancellation, or crash witness",
        );
    }
}

struct TrafficActor {
    admitted: usize,
    stopped: bool,
}
impl TrafficActor {
    fn step(&mut self, cluster: &mut Cluster) {
        if !self.stopped
            && self.admitted < 8
            && cluster.machines[0].driver.application().pending.len() < 3
        {
            cluster.admit(get(0, cluster.buckets[0][1].clone()));
            self.admitted += 1;
        }
    }
    fn stop(&mut self) {
        self.stopped = true;
    }
    fn check(&self, cluster: &Cluster) {
        require(
            self.admitted > 0
                && cluster.machines[0].driver.application().completed >= self.admitted,
            "overlap.healthy-progress",
            "healthy independent traffic must complete",
        );
    }
}

pub(super) fn run(cluster: &mut Cluster, seed: u64) {
    assert_eq!(cluster.machines.len(), 2, "overlap cell requires two nodes");
    let mut faults = FaultActor::setup(cluster, seed);
    let mut traffic = TrafficActor {
        admitted: 0,
        stopped: false,
    };
    while !faults.step(cluster) {
        traffic.step(cluster);
        cluster.turn();
    }
    traffic.stop();
    faults.check(cluster);
    cluster.drain();
    traffic.check(cluster);
    let before = cluster
        .machines
        .iter()
        .map(|m| m.driver.application().completed)
        .sum::<usize>();
    for source in 0..2 {
        cluster.admit(get(source, cluster.buckets[1 - source][2].clone()));
    }
    cluster.drain();
    require(
        cluster
            .machines
            .iter()
            .map(|m| m.driver.application().completed)
            .sum::<usize>()
            == before + 2,
        "overlap.cold-recovery",
        "both cold probes must complete after release and restart",
    );
}

#[test]
fn overlapping_faults_publication_restart_and_healthy_traffic() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        run(&mut cluster, seed);
        cluster.finish();
    }
}

#[test]
fn successful_status_mutant_requires_named_response_oracle() {
    for mutant in [None, Some(Mutant::SuccessfulGetStatus)] {
        let world = World::new(19);
        let _scope = world.enter();
        world.mutant(mutant);
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let mut cluster = Cluster::with_rdma(world, 2, false);
            cluster.admit(get(0, cluster.buckets[0][0].clone()));
            cluster.finish();
        }));
        if mutant.is_some() {
            let payload = result.expect_err("mutant survived");
            let failure = payload
                .downcast_ref::<Failure>()
                .expect("unrelated panic is not detection");
            assert_eq!(failure.oracle, "response.status");
        } else {
            assert!(result.is_ok());
        }
    }
}

pub(super) fn confirmation_reload(cluster: &mut Cluster) {
    cluster.hold_confirmation = true;
    cluster.trigger_edges(&[(0, 1)]);
    let end = cluster.world.tick() + 1000;
    while cluster.held_confirmations == 0 {
        cluster.turn();
        require(
            cluster.world.tick() < end,
            "confirmation.hold",
            "real confirmation must reach the link hold",
        );
    }
    require(
        cluster.reads == 0,
        "confirmation.pre-admission",
        "unconfirmed session must not issue application reads",
    );
    for node in 0..2 {
        let _scope = cluster.world.scoped_node(Some(node));
        let machine = &mut cluster.machines[node];
        machine.config.revision += 1;
        machine.config.epoch += 1;
        machine.config.volumes[0].cache_generation += 1;
        machine.config.volumes[0].topology.as_mut().unwrap().epoch += 1;
        let (mut trust, _) = fixture();
        trust.node = identity(node);
        machine
            .driver
            .application()
            .volumes
            .updates
            .publish(Cluster::prepare_single_volume(&trust, &machine.config))
            .unwrap();
        cluster
            .world
            .observation(Transition::Publish { revision: 2 });
    }
    while !(0..2).all(|node| {
        cluster.machines[node].driver.application().volumes.servers[&address(node, false)]
            .handler()
            .current
            ._config
            .config
            .revision
            == 2
    }) {
        cluster.turn();
        require(
            cluster.world.tick() < end,
            "confirmation.reload",
            "new configuration must activate while confirmation is held",
        );
    }
    cluster
        .world
        .observation(Transition::ReloadDuringConfirmation { revision: 2 });
    cluster.hold_confirmation = false;
    cluster.warm(&[(0, 1)]);
    let before = cluster.reads;
    cluster.admit(get(0, cluster.buckets[1][1].clone()));
    cluster.drain();
    require(
        cluster.reads > before,
        "confirmation.reload-read",
        "new generation must complete a real RDMA read after confirmation release",
    );
    cluster
        .world
        .observation(Transition::ConfirmationReloadRecovered {
            reads: cluster.reads - before,
        });
}

#[test]
fn held_confirmation_overlaps_namespace_reload_and_recovers() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, true);
        cluster.phase_policy = PhasePolicy::Permuted;
        confirmation_reload(&mut cluster);
        cluster.finish();
    }
}

pub(super) fn namespace(cluster: &mut Cluster) {
    let target = cluster.buckets[1][0].clone();
    let gate = cluster.world.gate(Gate::new(
        1,
        address(1, true),
        &target,
        Phase::Request,
        None,
    ));
    cluster.world.observation(Transition::FaultArmed {
        fault: gate,
        target: target.clone(),
    });
    let mut cursor = 0;
    // All callers use one pool and the same cold representation.
    for head in [false, false, false, true] {
        cluster.admit_method(get(1, target.clone()), head);
    }
    let end = cluster.world.tick() + 1000;
    let mut joined = false;
    let mut accepted = 0;
    loop {
        cluster.turn();
        for event in cluster.world.events_since(&mut cursor).unwrap() {
            joined |= event.node == Some(1) && event.kind == "network-join";
            accepted += usize::from(
                event.node == Some(1)
                    && event.kind == "volume-accept"
                    && event.target == target
                    && event.detail == "revision=1",
            );
        }
        if joined && accepted == 4 && cluster.world.hits(gate) > 0 {
            break;
        }
        require(
            cluster.world.tick() < end,
            "namespace.join",
            "shared callers must join a gated flight",
        );
    }
    cluster.world.observation(Transition::JoinedFlight {
        target: target.clone(),
    });
    cluster
        .world
        .observation(Transition::FaultEffective { fault: gate });
    let began: Vec<_> = cluster.machines[1]
        .driver
        .application()
        .pending
        .iter()
        .map(|p| (p.id, p.began))
        .collect();
    require(
        began.len() == 4,
        "namespace.live-callers",
        "GET and HEAD must remain live at publication",
    );
    for node in 0..2 {
        let _scope = cluster.world.scoped_node(Some(node));
        let machine = &mut cluster.machines[node];
        machine.config.revision += 1;
        machine.config.epoch += 1;
        machine.config.volumes[0].cache_generation += 1;
        machine.config.volumes[0].topology.as_mut().unwrap().epoch += 1;
        let (mut trust, _) = fixture();
        trust.node = identity(node);
        machine
            .driver
            .application()
            .volumes
            .updates
            .publish(Cluster::prepare_single_volume(&trust, &machine.config))
            .unwrap();
        cluster.world.observation(Transition::Publish {
            revision: machine.config.revision,
        });
    }
    while !(0..2).all(|node| {
        cluster.machines[node].driver.application().volumes.servers[&address(node, false)]
            .handler()
            .current
            ._config
            .config
            .revision
            == 2
    }) {
        cluster.turn();
        require(
            cluster.world.tick() < end,
            "namespace.activation",
            "namespace must activate before gate release",
        );
    }
    cluster.world.observation(Transition::NamespaceActivated {
        generation: cluster.machines[0].config.volumes[0].cache_generation,
    });
    cluster.action(Action::Cancel(1));
    require(
        cluster.machines[1]
            .driver
            .application()
            .pending
            .iter()
            .all(|p| began.contains(&(p.id, p.began))),
        "namespace.deadlines",
        "surviving callers must retain their original admission times",
    );
    cluster.world.release(gate);
    cluster
        .world
        .observation(Transition::FaultReleased { fault: gate });
    cluster.drain();
    require(
        cluster.machines[1].driver.application().completed == 3,
        "namespace.survivors",
        "all three uncanceled callers must complete successfully",
    );
    let hits = cluster.hits.borrow().len();
    cursor = cluster.cursor;
    cluster.admit(get(1, target.clone()));
    let mut accepted = Vec::new();
    let deadline = cluster.world.tick() + 1000;
    while !cluster.machines[1].driver.application().pending.is_empty() {
        cluster.turn();
        accepted.extend(
            cluster
                .world
                .events_since(&mut cursor)
                .unwrap()
                .into_iter()
                .filter(|event| {
                    event.node == Some(1) && event.kind == "volume-accept" && event.target == target
                }),
        );
        require(
            cluster.world.tick() < deadline,
            "namespace.fresh-progress",
            "fresh request must complete after activation",
        );
    }
    require(
        accepted.len() == 1 && accepted[0].detail == "revision=2",
        "namespace.authority",
        "fresh unrouted request must select the activated generation",
    );
    require(
        cluster.hits.borrow().len() > hits,
        "namespace.isolation",
        "new namespace must not reuse the old flight's cached representation",
    );
    cluster
        .world
        .observation(Transition::NamespaceColdFetch { target });
}

#[test]
fn namespace_activation_with_joined_get_head_and_cancellation() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        namespace(&mut cluster);
        cluster.finish();
    }
}

#[test]
fn stale_namespace_mutant_requires_authority_oracle() {
    for mutant in [None, Some(Mutant::StaleNamespaceSelection)] {
        let world = World::new(19);
        let _scope = world.enter();
        world.mutant(mutant);
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let mut cluster = Cluster::with_rdma(world.clone(), 2, false);
            namespace(&mut cluster);
            cluster.finish();
        }));
        if mutant.is_some() {
            let failure = result.expect_err("stale namespace mutant survived");
            assert_eq!(
                failure.downcast_ref::<Failure>().map(|f| f.oracle),
                Some("namespace.authority"),
                "only the intended authority violation counts as detection"
            );
        } else {
            assert!(result.is_ok());
        }
    }
}

#[test]
fn canceled_flight_accounting_mutant_requires_lease_oracle() {
    for mutant in [None, Some(Mutant::SkipCanceledFlightAccounting)] {
        let world = World::new(19);
        let _scope = world.enter();
        world.mutant(mutant);
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            let mut cluster = Cluster::with_rdma(world.clone(), 2, false);
            flight_cancellation(&mut cluster);
            cluster.finish();
        }));
        if mutant.is_some() {
            let failure = result.expect_err("cancellation accounting mutant survived");
            assert_eq!(
                failure.downcast_ref::<Failure>().map(|f| f.oracle),
                Some("ownership.flight-leases")
            );
        } else {
            assert!(result.is_ok());
        }
    }
}

pub(super) fn flight_cancellation(cluster: &mut Cluster) {
    use crate::buffers::{NetworkDependency, NetworkFlightKey, NetworkProgress};
    let _scope = cluster.world.scoped_node(Some(0));
    let pool = cluster.machines[0].driver.ring_mut().pool();
    let key = NetworkFlightKey {
        value: [7; 32],
        routing: [9; 32],
        version: 1,
        destination: 0,
        dependency: NetworkDependency::Canonical { slot: 0 },
    };
    let mut producer = pool.network_flight(key.clone()).unwrap();
    let mut survivor = pool.network_flight(key).unwrap();
    assert!(matches!(
        producer.poll(std::task::Waker::noop()),
        NetworkProgress::Produce
    ));
    assert!(matches!(
        survivor.poll(std::task::Waker::noop()),
        NetworkProgress::Pending
    ));
    pool.invariant_snapshot();
    cluster.world.observation(Transition::JoinedFlight {
        target: "component-flight".into(),
    });
    drop(producer);
    cluster
        .world
        .observation(Transition::FlightProducerCanceled { consumers: 2 });
    pool.invariant_snapshot();
    require(
        matches!(
            survivor.poll(std::task::Waker::noop()),
            NetworkProgress::Produce
        ),
        "cancellation.takeover",
        "survivor must acquire producer authority after cancellation",
    );
    drop(survivor);
    require(
        pool.invariant_snapshot().flights == 0,
        "cancellation.retirement",
        "all flight leases must retire",
    );
}

pub(super) fn checkpoint_crash(cluster: &mut Cluster) {
    checkpoint_crash_policy(cluster, false);
}

pub(super) fn checkpoint_crash_policy(cluster: &mut Cluster, versions: bool) {
    let retained = cluster.buckets[0][0].clone();
    // Second sight admits the payload. Ordinary successful GET is not durability.
    for _ in 0..2 {
        cluster.admit(get(0, retained.clone()));
        cluster.drain();
    }
    cluster.action(Action::Durable(0, retained.clone()));
    cluster.world.observation(Transition::DurabilityWitness {
        target: retained.clone(),
    });
    if versions {
        cluster.machines[0].disk.track_versions(65536).unwrap();
    }
    cluster.machines[0].disk.hold_sync(true);
    let mut cursor = 0;
    cluster.world.events_since(&mut cursor).unwrap();
    let new = cluster.buckets[0][1].clone();
    cluster.admit(get(0, new.clone()));
    cluster.drain();
    cluster.admit(get(0, new));
    let deadline = cluster.world.tick() + 2000;
    let mut checkpoint = false;
    let mut checkpoint_tick = None;
    loop {
        cluster.turn();
        for event in cluster.world.events_since(&mut cursor).unwrap() {
            checkpoint |= event.node == Some(0) && event.kind == "checkpoint-data-written";
            require(
                event.node != Some(0) || event.kind != "checkpoint-root-written",
                "durability.barrier-order",
                "checkpoint root must not be written while its data sync is held",
            );
        }
        if checkpoint {
            checkpoint_tick.get_or_insert(cluster.world.tick());
        }
        let dirty = cluster.machines[0].disk.dirty_sectors();
        if checkpoint_tick.is_some_and(|tick| cluster.world.tick() >= tick + 32) && dirty.len() >= 3
        {
            // Exclude the lowest dirty sector and persist later, separated sectors.
            // This is observably different from every address-ordered prefix.
            let persisted: Vec<_> = dirty.iter().skip(1).step_by(2).copied().collect();
            if versions {
                let pending = cluster.machines[0].disk.pending_versions();
                let selection: Vec<_> = pending
                    .iter()
                    .enumerate()
                    .map(|(index, (sector, count))| {
                        (
                            *sector,
                            if index % 2 == 0 {
                                0
                            } else {
                                (*count).div_ceil(2)
                            },
                        )
                    })
                    .collect();
                require(
                    selection.iter().any(|(_, version)| *version > 0),
                    "durability.version-window",
                    "crash must select at least one pending sector version",
                );
                cluster.world.observation(Transition::SectorVersionCrash {
                    selection: selection.clone(),
                    pending: pending.iter().map(|(_, count)| count).sum(),
                });
                cluster.machines[0]
                    .disk
                    .select_crash_versions(selection)
                    .unwrap();
            } else {
                cluster.world.observation(Transition::DirtyCheckpointCrash {
                    dirty: dirty.len(),
                    persisted: persisted.clone(),
                });
                cluster.machines[0].disk.select_crash_sectors(persisted);
            }
            cluster.reboot(0, false, Some(0));
            break;
        }
        require(
            cluster.world.tick() < deadline,
            "durability.crash-window",
            "crash must intersect dirty checkpoint data before its sync",
        );
    }
    for node in 0..2 {
        cluster.origin_off(node);
    }
    let hits = cluster.hits.borrow().len();
    let reads = cluster.world.counts()[30];
    cluster.admit(get(0, retained.clone()));
    cluster.drain();
    require(
        cluster.hits.borrow().len() == hits && cluster.world.counts()[30] > reads,
        "durability.recovery",
        "durably witnessed bytes must recover through disk without origin",
    );
    cluster
        .world
        .observation(Transition::DurableRecovery { target: retained });
}

#[test]
fn dirty_checkpoint_nonprefix_crash_preserves_durable_object() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        checkpoint_crash(&mut cluster);
        cluster.finish();
    }
}

#[test]
fn checkpoint_sector_versions_preserve_durable_object() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        checkpoint_crash_policy(&mut cluster, true);
        cluster.finish();
    }
}

#[test]
fn skipped_checkpoint_sync_requires_barrier_order_oracle() {
    for mutant in [None, Some(Mutant::SkipCheckpointDataSync)] {
        let world = World::new(19);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world.clone(), 2, false);
        world.mutant(mutant);
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            checkpoint_crash(&mut cluster);
            cluster.finish();
        }));
        if mutant.is_some() {
            let failure = result.unwrap_err().downcast::<Failure>().unwrap();
            assert_eq!(failure.oracle, "durability.barrier-order");
        } else {
            assert!(result.is_ok());
        }
    }
}

pub(super) fn local_attribution(cluster: &mut Cluster) {
    use crate::http_client::{Origin, attempt, breaker::CircuitBreaker};
    use attempt::{Cause, Phase, Transport};
    let _node = cluster.world.scoped_node(Some(0));
    for (cause, initiated) in [
        (Cause::LocalPressure, false),
        (Cause::CallerDeadline, true),
        (Cause::Cancelled, true),
        (Cause::BreakerRejected, false),
        (Cause::Connection, false),
    ] {
        let breaker = CircuitBreaker::new(Duration::from_secs(1));
        let error = |cause, initiated| {
            crate::cache::Error::from(std::io::Error::other(attempt::Failure {
                endpoint: address(1, false),
                transport: Transport::Http,
                phase: if initiated {
                    Phase::Headers
                } else {
                    Phase::LocalAdmission
                },
                cause,
                initiated,
                kind: std::io::ErrorKind::Other,
                message: "attribution negative control".into(),
            }))
        };
        cluster
            .world
            .observation(Transition::LocalFailureSubmitted {
                cause: format!("{cause:?}"),
                initiated,
            });
        Origin::error(
            breaker.try_acquire().unwrap(),
            &error(cause, initiated),
            true,
        );
        let next = breaker.try_acquire();
        require(
            next.is_ok(),
            "attribution.local-health",
            format!("{cause:?} initiated={initiated} must not reject the next peer request"),
        );
        // The same adapter must still record real initiated connection failures.
        cluster
            .world
            .observation(Transition::RemoteFailureSubmitted {
                cause: "Connection".into(),
            });
        Origin::error(next.unwrap(), &error(Cause::Connection, true), true);
        require(
            breaker.try_acquire().is_err() && breaker.active() == 0,
            "attribution.remote-health",
            "initiated connection failure must open the breaker and retire its permit",
        );
    }
}

#[test]
fn local_attribution_mutant_requires_health_oracle() {
    for mutant in [None, Some(Mutant::LocalFailureAsRemote)] {
        let world = World::new(19);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world.clone(), 2, false);
        world.mutant(mutant);
        let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
            local_attribution(&mut cluster);
            cluster.finish();
        }));
        if mutant.is_some() {
            let failure = result.unwrap_err().downcast::<Failure>().unwrap();
            assert_eq!(failure.oracle, "attribution.local-health");
        } else {
            assert!(result.is_ok());
        }
    }
}
