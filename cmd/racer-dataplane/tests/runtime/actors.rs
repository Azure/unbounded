// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Cooperative overlap cell. Only the coordinator advances the production drivers.
use super::*;
use crate::simulation::{Gate, Phase, history::require};

#[derive(Clone, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub(super) struct GeneratedLifecycle {
    pub seed: u64,
    pub rounds: u8,
}

struct LifecycleRound {
    round: u8,
    crash_node: usize,
    callers: usize,
    stride: usize,
    targets: [String; 2],
    gates: [usize; 2],
    requests: [Vec<u64>; 2],
    accepted: [usize; 2],
    revisions: [u64; 2],
    healthy: [String; 2],
    healthy_requests: Vec<u64>,
    checkpoint: Option<u64>,
    cursor: u64,
    deadline: u64,
    stage: u8,
}

fn lifecycle_target(seed: u64, round: u8, label: &str, node: usize, size: usize) -> String {
    (0..1024)
        .map(|suffix| format!("/sized/{size}/lifecycle/{seed}/{round}/{label}/{suffix}?exact=%2f"))
        .find(|target| owner(target, 2) == node)
        .expect("bounded lifecycle owner search")
}

impl LifecycleRound {
    fn live_requests(&self, cluster: &Cluster) -> Vec<u64> {
        let mut live = Vec::new();
        for source in 0..2 {
            let pending = &cluster.machines[source].driver.application().pending;
            for id in &self.requests[source] {
                require(
                    pending
                        .iter()
                        .any(|p| p.id == *id && p.request.target == self.targets[source]),
                    "lifecycle.live-cohort",
                    "every accepted fault caller must remain live until the crash boundary",
                );
                live.push(*id);
            }
        }
        live
    }

    fn healthy_done(&self, cluster: &Cluster) -> bool {
        !cluster.machines.iter().any(|machine| {
            machine
                .driver
                .application()
                .pending
                .iter()
                .any(|p| self.healthy_requests.contains(&p.id))
        })
    }

    fn step(&mut self, cluster: &mut Cluster) -> bool {
        let world = cluster.world.clone();
        require(
            world.tick() < self.deadline,
            "lifecycle.progress",
            "bounded lifecycle must reach the dirty crash without timeout recovery",
        );
        for event in world.events_since(&mut self.cursor).unwrap() {
            for source in 0..2 {
                if event.node == Some(source)
                    && event.kind == "volume-accept"
                    && event.target == self.targets[source]
                {
                    self.accepted[source] += 1;
                }
            }
            if event.node == Some(self.crash_node) {
                require(
                    event.kind != "checkpoint-root-written",
                    "lifecycle.barrier-order",
                    "held data sync must prevent checkpoint root publication before crash",
                );
                if event.kind == "checkpoint-data-written" {
                    self.checkpoint.get_or_insert(event.tick);
                }
            }
        }
        let live = self.live_requests(cluster);
        match self.stage {
            0 => {
                if self.accepted != [self.callers; 2]
                    || !self.gates.iter().all(|gate| world.hits(*gate) > 0)
                {
                    return false;
                }
                for source in 0..2 {
                    world.observation(Transition::FaultEffective {
                        fault: self.gates[source],
                    });
                    world.observation(Transition::LifecycleFaultCohort {
                        round: self.round,
                        fault: self.gates[source],
                        source,
                        destination: 1 - source,
                        target: self.targets[source].clone(),
                        requests: self.requests[source].clone(),
                    });
                }
                // Submit independent local-owner traffic before publication;
                // the coordinator progresses it concurrently with activation.
                for node in 0..2 {
                    self.healthy_requests.push(cluster.admitted as u64);
                    cluster.admit(get(node, self.healthy[node].clone()));
                }
                // Same production publication path as FaultActor. Preserve the
                // namespace so the independently durable object remains addressable.
                for node in 0..2 {
                    let _scope = world.scoped_node(Some(node));
                    let machine = &mut cluster.machines[node];
                    machine.config.revision += 1;
                    machine.config.epoch += 1;
                    machine.config.volumes[0].topology.as_mut().unwrap().epoch += 1;
                    self.revisions[node] = machine.config.revision;
                    let (mut trust, _) = fixture();
                    trust.node = identity(node);
                    let published = machine
                        .driver
                        .application()
                        .volumes
                        .updates
                        .publish(Cluster::prepare_single_volume(&trust, &machine.config));
                    require(
                        published.is_ok(),
                        "lifecycle.publish",
                        "valid topology publication must be accepted",
                    );
                    world.observation(Transition::Publish {
                        revision: self.revisions[node],
                    });
                }
                self.stage = 1;
            }
            1 => {
                if !(0..2).all(|node| {
                    cluster.machines[node].driver.application().volumes.servers
                        [&address(node, false).into()]
                        .handler()
                        .current
                        ._config
                        .config
                        .revision
                        == self.revisions[node]
                }) {
                    return false;
                }
                world.observation(Transition::LifecyclePublicationOverlap {
                    round: self.round,
                    faults: self.gates.to_vec(),
                    requests: live,
                    revisions: self.revisions.to_vec(),
                });
                self.stage = 2;
            }
            2 => {
                if !self.healthy_done(cluster) {
                    return false;
                }
                // Second sight admits payload storage, as in checkpoint_crash_policy.
                self.healthy_requests.push(cluster.admitted as u64);
                cluster.admit(get(self.crash_node, self.healthy[self.crash_node].clone()));
                self.stage = 3;
            }
            3 => {
                if !self.healthy_done(cluster) {
                    return false;
                }
                let dirty = cluster.machines[self.crash_node].disk.dirty_sectors();
                if !self
                    .checkpoint
                    .is_some_and(|tick| world.tick() >= tick + 32)
                    || dirty.len() < 3
                {
                    return false;
                }
                for node in 0..2 {
                    require(
                        cluster.machines[node]
                            .driver
                            .application()
                            .outcomes
                            .get(&self.healthy[node])
                            == Some(&200),
                        "lifecycle.healthy-progress",
                        "independent traffic must finish through the strict response oracle",
                    );
                    require(
                        cluster.machines[node].driver.application().pending.len() == self.callers,
                        "lifecycle.crash-accounting",
                        "only the witnessed fault cohort may remain at process loss",
                    );
                }
                world.observation(Transition::LifecycleHealthyProgress {
                    round: self.round,
                    faults: self.gates.to_vec(),
                    requests: self.healthy_requests.clone(),
                });
                // Skip the first dirty sector and retain a seeded non-prefix subset.
                let persisted: Vec<_> =
                    dirty.iter().skip(1).step_by(self.stride).copied().collect();
                require(
                    !persisted.is_empty() && !persisted.contains(&dirty[0]),
                    "lifecycle.nonprefix-crash",
                    "crash must select a nonempty non-prefix dirty subset",
                );
                world.observation(Transition::DirtyCheckpointCrash {
                    dirty: dirty.len(),
                    persisted: persisted.clone(),
                });
                world.observation(Transition::LifecycleCrashOverlap {
                    round: self.round,
                    node: self.crash_node,
                    faults: self.gates.to_vec(),
                    requests: live,
                    dirty: dirty.len(),
                    persisted: persisted.clone(),
                });
                cluster.machines[self.crash_node]
                    .disk
                    .select_crash_sectors(persisted);
                let survivor = 1 - self.crash_node;
                let cancelled = cluster.cancelled;
                // Explicit caller retirement prevents accepting arbitrary transport
                // errors on the surviving process. No driver turn splits this cut.
                for _ in 0..self.callers {
                    cluster.action(Action::Cancel(survivor));
                }
                let before = {
                    let _scope = world.scoped_node(Some(self.crash_node));
                    world.process().incarnation
                };
                cluster.reboot(self.crash_node, false, Some(0));
                let incarnation = {
                    let _scope = world.scoped_node(Some(self.crash_node));
                    world.process().incarnation
                };
                require(
                    incarnation == before + 1 && cluster.cancelled == cancelled + 2 * self.callers,
                    "lifecycle.process-loss",
                    "crash and explicit cancellation must retire exactly the two cohorts",
                );
                {
                    let _scope = world.scoped_node(Some(self.crash_node));
                    world.observation(Transition::LifecycleRestarted {
                        round: self.round,
                        node: self.crash_node,
                        incarnation,
                        lost: self.requests[self.crash_node].clone(),
                    });
                }
                for gate in self.gates {
                    world.release(gate);
                    world.observation(Transition::FaultReleased { fault: gate });
                }
                return true;
            }
            _ => unreachable!(),
        }
        false
    }
}

pub(super) fn generated_lifecycle(cluster: &mut Cluster, config: &GeneratedLifecycle) {
    let world = cluster.world.clone();
    let expected_disk_size = (16 + 16 * u64::from(config.rounds)) * buffers::BUFFER_SIZE as u64;
    require(
        cluster
            .machines
            .iter()
            .all(|machine| machine.disk_size == expected_disk_size),
        "lifecycle.storage-budget",
        "lifecycle requires the declared round-bounded slab capacity on both nodes",
    );
    let mut random = corpus::Random(config.seed);
    for round in 0..config.rounds {
        let crash_node = random.index(2);
        let callers = 2 + random.index(2);
        let size = [257, 4095, 4096][random.index(3)];
        let first = random.index(2);
        let stride = 2 + random.index(2);
        world.observation(Transition::LifecycleRoundPlanned {
            round,
            crash_node,
            callers,
            object_bytes: size,
            first_source: first,
            persistence_stride: stride,
            disk_bytes: cluster.machines[crash_node].disk_size,
        });
        let retained = lifecycle_target(config.seed, round, "retained", crash_node, size);
        for _ in 0..2 {
            cluster.admit(get(crash_node, retained.clone()));
            cluster.drain();
        }
        cluster.action(Action::Durable(crash_node, retained.clone()));
        {
            let _scope = world.scoped_node(Some(crash_node));
            world.observation(Transition::DurabilityWitness {
                target: retained.clone(),
            });
        }
        cluster.quiesce();
        let mut actor = LifecycleRound {
            round,
            crash_node,
            callers,
            stride,
            targets: std::array::from_fn(|source| {
                lifecycle_target(config.seed, round, "held", 1 - source, size)
            }),
            gates: [0; 2],
            requests: std::array::from_fn(|_| Vec::new()),
            accepted: [0; 2],
            revisions: [0; 2],
            healthy: std::array::from_fn(|node| {
                lifecycle_target(config.seed, round, "healthy", node, size)
            }),
            healthy_requests: Vec::new(),
            checkpoint: None,
            cursor: cluster.cursor,
            deadline: world.tick() + 2500,
            stage: 0,
        };
        cluster.machines[crash_node].disk.hold_sync(true);
        for source in [first, 1 - first] {
            let target = &actor.targets[source];
            cluster.fault_targets.insert(target.clone());
            let gate = world.gate(Gate::new(
                source,
                address(1 - source, false),
                target,
                Phase::Request,
                None,
            ));
            actor.gates[source] = gate;
            world.observation(Transition::FaultArmed {
                fault: gate,
                target: target.clone(),
            });
            for caller in 0..callers {
                actor.requests[source].push(cluster.admitted as u64);
                cluster.admit_method(get(source, target.clone()), caller == callers - 1);
            }
        }
        while !actor.step(cluster) {
            cluster.turn();
        }
        require(
            cluster.machines[crash_node].disk_size == expected_disk_size,
            "lifecycle.storage-budget",
            "process restart must preserve the declared slab geometry",
        );
        cluster.quiesce();
        // Disable the restarted owner's origin for this exact-target probe.
        // quiesce checks cache/IO ownership, not every HTTP server task: released
        // old-cohort work on the OTHER node may still reach its unrelated origin.
        // Require no retained-target origin execution anywhere, plus a payload
        // disk hit on the restarted node and real splice activity.
        cluster.origin_off(crash_node);
        let hits = cluster.hits.borrow().len();
        let reads = world.counts()[30];
        let metrics = cluster.machines[crash_node]
            .driver
            .ring_mut()
            .metrics()
            .values();
        let request = cluster.admitted as u64;
        cluster.admit(get(crash_node, retained.clone()));
        cluster.drain();
        let new_hits = cluster.hits.borrow()[hits..].to_vec();
        let after_metrics = cluster.machines[crash_node]
            .driver
            .ring_mut()
            .metrics()
            .values();
        require(
            cluster.machines[crash_node]
                .driver
                .application()
                .origin
                .is_none()
                && new_hits.iter().all(|(_, target)| target != &retained)
                && after_metrics[11] > metrics[11]
                && world.counts()[30] > reads,
            "lifecycle.durable-recovery",
            format!(
                "restart must recover witnessed bytes from local disk without retained-target origin execution: seed={} round={round} node={crash_node} target={retained} request={request} status={:?} splice_before={reads} splice_after={} origin_hits={new_hits:?} cache_before={:?} cache_after={:?}",
                config.seed,
                cluster.machines[crash_node]
                    .driver
                    .application()
                    .outcomes
                    .get(&retained),
                world.counts()[30],
                &metrics[6..20],
                &after_metrics[6..20],
            ),
        );
        {
            let _scope = world.scoped_node(Some(crash_node));
            world.observation(Transition::DurableRecovery {
                target: retained.clone(),
            });
        }
        cluster.quiesce();
        // Rebind only after the old origin ACCEPT retires; keep the process and
        // its recovered cache alive for the cold cross-peer recovery probes.
        let origin = cluster.separate_origin(crash_node);
        cluster.machines[crash_node].driver.application_mut().origin = Some(origin);
        crate::workers::Wake::wake(&*cluster.machines[crash_node].driver.wake_handle());
        let mut cold_requests = Vec::new();
        let mut cold_targets = Vec::new();
        for source in 0..2 {
            let target = lifecycle_target(config.seed, round, "recovered", 1 - source, size);
            cold_requests.push(cluster.admitted as u64);
            cold_targets.push(target.clone());
            cluster.admit(get(source, target));
        }
        cluster.drain();
        for source in 0..2 {
            require(
                cluster.machines[source]
                    .driver
                    .application()
                    .outcomes
                    .get(&cold_targets[source])
                    == Some(&200)
                    && cluster
                        .hits
                        .borrow()
                        .iter()
                        .any(|(node, key)| *node == 1 - source && key == &cold_targets[source]),
                "lifecycle.cold-recovery",
                "both cold probes must reach their original owners and pass the strict byte oracle",
            );
            cluster.fault_targets.remove(&actor.targets[source]);
        }
        cluster.quiesce();
        world.observation(Transition::LifecycleRoundRecovered {
            round,
            target: retained,
            cold_requests,
        });
    }
}

#[test]
fn generated_lifecycle_overlaps_publication_dirty_crash_and_recovers() {
    for (seed, rounds) in [(19, 2), (71, 3)] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_lifecycle_storage(world, rounds);
        cluster.phase_policy = PhasePolicy::Permuted;
        generated_lifecycle(&mut cluster, &GeneratedLifecycle { seed, rounds });
        cluster.finish();
    }
}

#[test]
fn generated_lifecycle_sync_mutant_requires_barrier_order_oracle() {
    let world = World::new(19);
    let _scope = world.enter();
    let mut cluster = Cluster::with_lifecycle_storage(world.clone(), 1);
    // The durable predecessor still has to pass setup. The named barrier oracle
    // applies only after setup, when the composition deliberately holds data sync.
    world.mutant(Some(Mutant::SkipCheckpointDataSync));
    let result = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
        generated_lifecycle(
            &mut cluster,
            &GeneratedLifecycle {
                seed: 19,
                rounds: 1,
            },
        );
        cluster.finish();
    }));
    let failure = result.expect_err("checkpoint sync mutant survived lifecycle overlap");
    assert_eq!(
        failure
            .downcast_ref::<Failure>()
            .map(|failure| failure.oracle),
        Some("lifecycle.barrier-order")
    );
}

// The external caller owns its socket so FIN/RST use the simulator's actual
// directional stream policies. Listener, parsing, handler, origin and cleanup
// still run through the production drivers advanced only by Cluster::turn.
struct HttpWireCaller {
    socket: crate::simulation::Handle,
    began: u64,
}

impl HttpWireCaller {
    fn connect(cluster: &Cluster, destination: usize) -> Self {
        let world = &cluster.world;
        let _scope = world.scoped_node(Some(destination));
        let socket = world.socket();
        let endpoint = crate::socket::UnixPath::new(
            &cluster.machines[destination].config.volumes[0].cache_socket,
        )
        .unwrap();
        let raw = endpoint.sockaddr();
        // SAFETY: operation accesses this live sockaddr synchronously.
        let result = unsafe { world.operation(16, socket.id, &raw as *const _ as u64, 0, 0, 0, 0) };
        require(
            result.is_some_and(|(result, handle)| result == 0 && handle.is_none()),
            "http-stream.connect",
            "external caller must connect to the production listener",
        );
        Self {
            socket,
            began: world.tick(),
        }
    }

    fn send(&self, cluster: &mut Cluster, bytes: &[u8]) {
        let mut sent = 0;
        while sent < bytes.len() {
            http_actor_budget(cluster, self.began);
            // SAFETY: operation consumes only the supplied live byte slice.
            let result = unsafe {
                cluster.world.operation(
                    26,
                    self.socket.id,
                    bytes[sent..].as_ptr() as u64,
                    (bytes.len() - sent) as u32,
                    0,
                    0,
                    0,
                )
            };
            if let Some((count, _)) = result {
                require(count > 0, "http-stream.send", "request send must progress");
                sent += count as usize;
            }
            cluster.turn();
        }
    }

    fn response(&self, cluster: &mut Cluster, target: &str) {
        let mut bytes = Vec::new();
        loop {
            http_actor_budget(cluster, self.began);
            let mut buffer = [0u8; 4096];
            // SAFETY: operation writes synchronously into this live buffer.
            let result = unsafe {
                cluster.world.operation(
                    27,
                    self.socket.id,
                    buffer.as_mut_ptr() as u64,
                    buffer.len() as u32,
                    0,
                    0,
                    0,
                )
            };
            if let Some((count, _)) = result {
                require(
                    count >= 0,
                    "http-stream.response",
                    "half-closed caller must receive a complete successful response",
                );
                if count == 0 {
                    break;
                }
                bytes.extend_from_slice(&buffer[..count as usize]);
                require(
                    bytes.len() <= corpus::length(target) + 8192,
                    "http-stream.framing",
                    "response must remain bounded by payload and headers",
                );
            }
            cluster.turn();
        }
        let boundary = bytes.windows(4).position(|b| b == b"\r\n\r\n");
        require(
            boundary.is_some(),
            "http-stream.framing",
            "response must contain a complete HTTP header block before EOF",
        );
        let boundary = boundary.unwrap();
        let headers = std::str::from_utf8(&bytes[..boundary]);
        require(
            headers.is_ok(),
            "http-stream.framing",
            "response headers must have valid text encoding",
        );
        let headers = headers.unwrap();
        let mut lines = headers.split("\r\n");
        require(
            lines
                .next()
                .unwrap_or_default()
                .split_whitespace()
                .take(2)
                .collect::<Vec<_>>()
                == ["HTTP/1.1", "200"],
            "http-stream.status",
            "FIN must not turn the accepted GET into an error response",
        );
        let lengths: Result<Vec<_>, _> = lines
            .filter_map(|line| line.split_once(':'))
            .filter(|(name, _)| name.eq_ignore_ascii_case("content-length"))
            .map(|(_, value)| value.trim().parse::<usize>())
            .collect();
        require(
            lengths.is_ok(),
            "http-stream.framing",
            "response Content-Length must be a valid bounded integer",
        );
        let lengths = lengths.unwrap();
        require(
            lengths == [corpus::length(target)]
                && bytes[boundary + 4..] == corpus::reference(target, 0, corpus::length(target)),
            "http-stream.bytes",
            "FIN response must have exact framing and independently computed bytes",
        );
        cluster.world.trace_bytes(&bytes);
    }
}

fn http_actor_budget(cluster: &Cluster, began: u64) {
    require(
        cluster.world.tick() < began + 1000,
        "http-recovery.deadline",
        "HTTP actor must progress within its original monotonic budget",
    );
}

fn http_healthy_progress(cluster: &mut Cluster, target: &str, began: u64) {
    let completed = cluster.machines[0].driver.application().completed;
    cluster.admit(get(0, target));
    while cluster.machines[0].driver.application().completed == completed {
        http_actor_budget(cluster, began);
        cluster.turn();
    }
    require(
        cluster.machines[0]
            .driver
            .application()
            .outcomes
            .get(target)
            == Some(&200),
        "http-recovery.healthy-progress",
        "independent traffic must pass the strict response oracle while the fault is held",
    );
    cluster.world.observation(Transition::HttpHealthyProgress {
        target: target.into(),
        completed: completed + 1,
    });
}

pub(super) fn http_stream_recovery(cluster: &mut Cluster, reset: bool) {
    let world = cluster.world.clone();
    let target = cluster.buckets[1][0].clone();
    let healthy = cluster.buckets[0][0].clone();
    let ingress =
        crate::socket::Address::unix(&cluster.machines[1].config.volumes[0].cache_socket).unwrap();
    let gate = world.gate(Gate::new(
        1,
        crate::socket::Address::unix(&cluster.machines[1].config.volumes[0].origin_socket).unwrap(),
        &target,
        Phase::Request,
        None,
    ));
    world.observation(Transition::FaultArmed {
        fault: gate,
        target: target.clone(),
    });
    let mut cursor = cluster.cursor;
    let caller = HttpWireCaller::connect(cluster, 1);
    caller.send(
        cluster,
        format!("GET {target} HTTP/1.1\r\nHost: localhost\r\n\r\n").as_bytes(),
    );
    let mut accepted = 0;
    loop {
        for event in world.events_since(&mut cursor).unwrap() {
            accepted += usize::from(
                event.node == Some(1) && event.kind == "volume-accept" && event.target == target,
            );
        }
        if accepted == 1 && world.hits(gate) > 0 {
            break;
        }
        http_actor_budget(cluster, caller.began);
        cluster.turn();
    }
    require(
        cluster.machines[1].driver.application().volumes.servers[&ingress].connections() == 1
            && !cluster.hits.borrow().iter().any(|(_, key)| key == &target),
        "http-stream.in-flight",
        "one accepted HTTP task must be held before its origin request executes",
    );
    world.observation(Transition::HttpStreamInFlight {
        socket: caller.socket.id,
        target: target.clone(),
        fault: gate,
    });
    if reset {
        caller.socket.reset();
    } else {
        caller.socket.shutdown_write();
    }
    world.observation(Transition::FaultEffective { fault: gate });
    http_healthy_progress(cluster, &healthy, caller.began);
    world.release(gate);
    world.observation(Transition::FaultReleased { fault: gate });
    if !reset {
        caller.response(cluster, &target);
    }
    // Keep the reset descriptor alive until the production listener retires the
    // affected keep-alive slot; neither Connection: close nor dropping the
    // caller can provide the retirement cause. FIN similarly has to reach EOF.
    while cluster.machines[1].driver.application().volumes.servers[&ingress].connections() != 0 {
        http_actor_budget(cluster, caller.began);
        cluster.turn();
    }
    world.observation(Transition::HttpStreamRetired {
        socket: caller.socket.id,
        target: target.clone(),
    });
    let retired = caller.began;
    drop(caller);
    cluster.quiesce();
    http_actor_budget(cluster, retired);
    let completed = cluster.machines[1].driver.application().completed;
    cluster.admit(get(1, target.clone()));
    let began = world.tick();
    while cluster.machines[1].driver.application().completed == completed {
        http_actor_budget(cluster, began);
        cluster.turn();
    }
    require(
        cluster.machines[1]
            .driver
            .application()
            .outcomes
            .get(&target)
            == Some(&200),
        "http-stream.recovery",
        "a new connection must successfully serve the affected target after stream retirement",
    );
    world.observation(Transition::HttpStreamRecovered {
        policy: if reset { "reset" } else { "half-close" }.into(),
        target,
    });
}

fn signed_http_page(
    cluster: &Cluster,
    target: &str,
) -> (
    crate::http_auth::Policy,
    crate::http_auth::Pending,
    Vec<(String, Vec<u8>)>,
) {
    let _scope = cluster.world.scoped_node(Some(0));
    let (trust, _) = fixture();
    let policy = crate::http_auth::Policy {
        keys: trust.keys,
        universe: trust.universe,
        node: identity(0),
        peers: [identity(1)].into(),
    };
    let routing = crate::routing::Routing::new(
        &cluster.machines[0].config.universe,
        &cluster.machines[0].config.volumes[0],
    )
    .unwrap();
    let mut cursor = routing.start(target);
    cursor.position += 1;
    let payload = corpus::reference(target, 0, corpus::length(target));
    let mut wire = b"RF04".to_vec();
    wire.extend(5000u32.to_le_bytes());
    wire.extend(cursor.algorithm.magic());
    wire.extend(cursor.encode());
    wire.extend(b"RF05\x01");
    wire.extend(0u64.to_le_bytes());
    wire.extend((payload.len() as u64).to_le_bytes());
    wire.extend(blake3::hash(&payload).as_bytes());
    wire.extend(target.as_bytes());
    let mut headers = vec![(
        "X-Racer-Fault".into(),
        crate::cache::peer_wire::hex(&wire).into_bytes(),
    )];
    let pending = policy
        .request(identity(1), "GET", "/", &mut headers)
        .unwrap();
    (policy, pending, headers)
}

fn http_peer_exchange(cluster: &mut Cluster, headers: &[(String, Vec<u8>)]) -> client::GetExchange {
    let _scope = cluster.world.scoped_node(Some(0));
    let refs: Vec<_> = headers
        .iter()
        .map(|(n, v)| (n.as_str(), std::str::from_utf8(v).unwrap()))
        .collect();
    let fill = cluster.machines[0]
        .driver
        .ring_mut()
        .pool()
        .private_fill()
        .unwrap();
    client::Connection::new(address(1, false), "localhost")
        .unwrap()
        .get(
            client::Request::new("/", &refs).unwrap(),
            fill,
            cluster.world.now() + Duration::from_secs(5),
        )
        .unwrap()
}

fn http_peer_poll(
    cluster: &mut Cluster,
    exchange: &mut client::GetExchange,
) -> Option<client::GetResponse> {
    let _scope = cluster.world.scoped_node(Some(0));
    let progress = exchange.poll(cluster.machines[0].driver.ring_mut(), 64);
    require(
        progress.is_ok(),
        "http-auth.exchange",
        format!(
            "authenticated HTTP exchange must produce a complete framed response: {:?}",
            progress.as_ref().err()
        ),
    );
    match progress.unwrap() {
        Progress::Ready(reply) => Some(reply),
        Progress::Pending(_) => None,
    }
}

fn http_peer_response(
    cluster: &mut Cluster,
    exchange: &mut client::GetExchange,
    began: u64,
) -> client::GetResponse {
    loop {
        http_actor_budget(cluster, began);
        if let Some(reply) = http_peer_poll(cluster, exchange) {
            return reply;
        }
        cluster.turn();
    }
}

fn check_http_peer_page(
    mut reply: client::GetResponse,
    policy: &crate::http_auth::Policy,
    pending: &crate::http_auth::Pending,
    target: &str,
) {
    let length = corpus::length(target);
    require(
        reply.status() == 200 && reply.content_length() == Some(length as u64),
        "http-auth.response",
        "authenticated recovery must return exactly 200 and the expected length",
    );
    let signature = pending.verify(&policy.keys, 200, length as u64, reply.headers());
    require(
        signature.is_ok(),
        "http-auth.signature",
        format!(
            "response must carry a valid signature bound to the original request: {signature:?}"
        ),
    );
    require(
        reply.body() == corpus::reference(target, 0, length),
        "http-auth.bytes",
        "authenticated recovery must return independently computed payload bytes",
    );
}

pub(super) fn http_wall_expiry(cluster: &mut Cluster) {
    let world = cluster.world.clone();
    // A two-second margin keeps both directions outside the 60-second window
    // even if healthy overlap crosses a wall-second rounding boundary.
    for (index, offset) in [62_000, -62_000].into_iter().enumerate() {
        let target = cluster.buckets[1][index].clone();
        let healthy = cluster.buckets[0][index].clone();
        let (policy, pending, headers) = signed_http_page(cluster, &target);
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
        let began = world.tick();
        let receiver_metrics =
            cluster.machines[1].driver.ring_mut().metrics().values()[6..20].to_vec();
        let mut exchange = http_peer_exchange(cluster, &headers);
        while world.hits(gate) == 0 {
            http_actor_budget(cluster, began);
            require(
                http_peer_poll(cluster, &mut exchange).is_none(),
                "http-auth.held",
                "signed request must remain in flight at the send gate",
            );
            cluster.turn();
        }
        world.observation(Transition::HttpAuthenticationInFlight {
            target: target.clone(),
            fault: gate,
            authenticated: false,
        });
        let now = world.now();
        world.wall_offset(Some(1), offset);
        require(
            world.now() == now,
            "http-auth.monotonic",
            "wall jump must not consume or renew the request deadline",
        );
        world.observation(Transition::FaultEffective { fault: gate });
        http_healthy_progress(cluster, &healthy, began);
        world.release(gate);
        world.observation(Transition::FaultReleased { fault: gate });
        let mut reply = http_peer_response(cluster, &mut exchange, began);
        require(
            reply.status() == 400
                && reply.content_length() == Some(0)
                && reply.headers().get("x-racer-signature").is_none()
                && reply.body().is_empty(),
            "http-auth.expired",
            "expired in-flight authentication must return unsigned empty 400 before nonce/cache admission",
        );
        require(
            !cluster.hits.borrow().iter().any(|(_, key)| key == &target)
                && cluster.machines[1].driver.ring_mut().metrics().values()[6..20]
                    == receiver_metrics,
            "http-auth.no-cache-admission",
            "rejected authentication must not admit a cache fault or execute the origin request",
        );
        world.observation(Transition::HttpAuthenticationExpired {
            target: target.clone(),
            offset,
            status: 400,
        });
        drop((reply, exchange));
        world.wall_offset(Some(1), 0);
        // Reuse the exact signature and nonce: rejected authentication must not
        // poison replay admission. A fresh valid nonce alone would miss that bug.
        let began = world.tick();
        let mut exchange = http_peer_exchange(cluster, &headers);
        let reply = http_peer_response(cluster, &mut exchange, began);
        check_http_peer_page(reply, &policy, &pending, &target);
        drop(exchange);
        require(
            cluster
                .hits
                .borrow()
                .iter()
                .any(|(node, key)| *node == 1 && key == &target),
            "http-auth.recovery-origin",
            "same-nonce recovery must execute the previously rejected cold page at its owner",
        );
        world.observation(Transition::HttpAuthenticationRecovered {
            target,
            same_nonce: true,
        });
    }
    // Authentication is an admission check, not a response-time wall lease.
    // Cross the same wall boundary after actual origin admission and require the
    // already authenticated request to finish with its original signed context.
    let target = cluster.buckets[1][2].clone();
    let (policy, pending, headers) = signed_http_page(cluster, &target);
    let gate = world.gate(Gate::new(
        1,
        crate::socket::Address::unix(&cluster.machines[1].config.volumes[0].origin_socket).unwrap(),
        &target,
        Phase::Request,
        None,
    ));
    world.observation(Transition::FaultArmed {
        fault: gate,
        target: target.clone(),
    });
    let began = world.tick();
    let mut exchange = http_peer_exchange(cluster, &headers);
    while world.hits(gate) == 0 {
        http_actor_budget(cluster, began);
        require(
            http_peer_poll(cluster, &mut exchange).is_none(),
            "http-auth.admitted-flight",
            "authenticated page must be held at its real origin request",
        );
        cluster.turn();
    }
    world.observation(Transition::HttpAuthenticationInFlight {
        target: target.clone(),
        fault: gate,
        authenticated: true,
    });
    world.wall_offset(Some(1), 62_000);
    world.observation(Transition::FaultEffective { fault: gate });
    let healthy = cluster.buckets[0][2].clone();
    http_healthy_progress(cluster, &healthy, began);
    world.release(gate);
    world.observation(Transition::FaultReleased { fault: gate });
    let reply = http_peer_response(cluster, &mut exchange, began);
    check_http_peer_page(reply, &policy, &pending, &target);
    drop(exchange);
    world.observation(Transition::HttpAuthenticatedFlightCompleted {
        target,
        offset: 62_000,
    });
    world.wall_offset(Some(1), 0);
    cluster.quiesce();
    http_actor_budget(cluster, began);
}

#[test]
fn http_stream_reset_retires_inflight_request_and_recovers() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        http_stream_recovery(&mut cluster, true);
        cluster.finish();
    }
}

#[test]
fn http_stream_half_close_preserves_inflight_response_and_recovers() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        http_stream_recovery(&mut cluster, false);
        cluster.finish();
    }
}

#[test]
fn http_inflight_wall_expiry_rejects_at_authentication_and_recovers() {
    for seed in [19, 71] {
        let world = World::new(seed);
        let _scope = world.enter();
        let mut cluster = Cluster::with_rdma(world, 2, false);
        http_wall_expiry(&mut cluster);
        cluster.finish();
    }
}

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
                .get(&address(0, false).into())
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
                    machine.driver.application().volumes.servers[&address(0, false).into()]
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
                        [&address(node, false).into()]
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
        cluster.machines[node].driver.application().volumes.servers[&address(node, false).into()]
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
        crate::socket::Address::unix(&cluster.machines[1].config.volumes[0].origin_socket).unwrap(),
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
        cluster.machines[node].driver.application().volumes.servers[&address(node, false).into()]
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
                endpoint: address(1, false).into(),
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
