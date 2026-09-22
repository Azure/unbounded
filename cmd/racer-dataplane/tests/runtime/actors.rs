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
