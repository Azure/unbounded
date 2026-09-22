// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Cooperative overlap cell. Only the coordinator advances the production drivers.
use super::*;
use crate::simulation::{Gate, Phase, history::require};

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

pub(super) fn checkpoint_crash(cluster: &mut Cluster) {
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
            cluster.world.observation(Transition::DirtyCheckpointCrash {
                dirty: dirty.len(),
                persisted: persisted.clone(),
            });
            cluster.machines[0].disk.select_crash_sectors(persisted);
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
