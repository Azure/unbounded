//! Harness behavior: scheduling, faults, invariants, determinism.
use std::{
    cell::{Cell, RefCell},
    future::pending,
    path::Path,
    rc::Rc,
};

use racer_dst::{
    Choices, Scenario,
    choices::Source,
    determinism::{run_twice, shrink_failure},
    engine::{MILLIS, SECONDS},
    fixture::lane_pair,
    invariants::{Class, all_roles_settled},
    scenario::{FaultMix, Fidelity},
    scenarios,
    world::{DoneFlag, Fault, LaneRole, SimRole, wall_nanos},
};
use racer_io::{exec::Lane, exec::Next, sim::SimOs};

fn counter(os: &SimOs) -> u64 {
    let bytes = os.read_file(Path::new("/state/counter")).unwrap();
    std::str::from_utf8(&bytes).unwrap().parse().unwrap()
}

#[test]
fn lane_pair_runs_to_quiescence_deterministically() {
    let os = Rc::new(RefCell::new(None));
    let seen = os.clone();
    let s = Scenario::new("t")
        .roles(move |env| {
            *seen.borrow_mut() = Some(env.os.clone());
            lane_pair(env, 5)
        })
        .with(all_roles_settled());
    let a = run_twice(&s, &Source::Seed(11)).unwrap();
    assert_eq!(a.failure, None);
    assert!(a.quiescent);
    // Five rounds of at least 1 ms of timer sleep each.
    assert!(a.end >= 5 * MILLIS, "{a:?}");
    assert_eq!(counter(os.borrow().as_ref().unwrap()), 5);
    let b = s.run(Choices::seeded(12));
    assert_ne!(a.trace, b.trace, "seed changes the schedule");
}

#[test]
fn crash_and_restart_resume_from_durable_state() {
    let incarnations = Rc::new(RefCell::new(Vec::new()));
    let os = Rc::new(RefCell::new(None));
    let (seen, inc) = (os.clone(), incarnations.clone());
    let s = Scenario::new("t")
        .roles(move |env| {
            inc.borrow_mut().push(env.incarnation);
            *seen.borrow_mut() = Some(env.os.clone());
            lane_pair(env, 6)
        })
        .fault(SECONDS, Fault::Crash { node: 0 })
        .fault(SECONDS + MILLIS, Fault::Restart { node: 0 })
        .with(all_roles_settled());
    let out = s.run(Choices::seeded(3));
    assert_eq!(out.failure, None);
    assert_eq!(*incarnations.borrow(), vec![0, 1]);
    // Six durable writes before the crash, six more after recovery.
    assert_eq!(counter(os.borrow().as_ref().unwrap()), 12);
}

#[test]
fn crash_mid_run_still_settles() {
    let s = Scenario::new("t")
        .nodes(2)
        .roles(|env| lane_pair(env, 20))
        .fault(5 * MILLIS, Fault::Crash { node: 1 })
        .fault(6 * MILLIS, Fault::Restart { node: 1 })
        .with(all_roles_settled());
    for seed in 0..10 {
        let out = run_twice(&s, &Source::Seed(seed)).unwrap();
        assert_eq!(out.failure, None, "seed {seed}");
    }
}

#[test]
fn clock_skew_moves_only_wall_time() {
    let clocks = Rc::new(RefCell::new(Vec::new()));
    let seen = clocks.clone();
    let s = Scenario::new("t")
        .nodes(2)
        .roles(move |env| {
            seen.borrow_mut().push(env.clock.clone());
            lane_pair(env, 1)
        })
        .fault(
            0,
            Fault::ClockSkew {
                node: 1,
                offset_ns: -(SECONDS as i64),
            },
        );
    assert_eq!(s.run(Choices::seeded(1)).failure, None);
    let clocks = clocks.borrow();
    assert_eq!(clocks[0].now(), clocks[1].now());
    assert_eq!(
        wall_nanos(&clocks[0]) - wall_nanos(&clocks[1]),
        u128::from(SECONDS)
    );
}

#[test]
fn clock_rate_and_jump_skew_monotonic_time() {
    let clocks = Rc::new(RefCell::new(Vec::new()));
    let seen = clocks.clone();
    let s = Scenario::new("t")
        .nodes(3)
        .roles(move |env| {
            seen.borrow_mut().push(env.clock.clone());
            lane_pair(env, 3)
        })
        .fault(
            0,
            Fault::ClockRate {
                node: 1,
                ppm: 500_000,
            },
        )
        .fault(
            0,
            Fault::ClockJump {
                node: 2,
                ns: SECONDS,
            },
        )
        .with(all_roles_settled());
    let out = run_twice(&s, &Source::Seed(2)).unwrap();
    assert_eq!(out.failure, None);
    assert!(out.quiescent);
    let clocks = clocks.borrow();
    let base = clocks[0].elapsed();
    assert_eq!(clocks[0].reference_elapsed(), base);
    // The fast node saw 1.5x the world time, the stepped one a second more.
    assert_eq!(clocks[1].reference_elapsed(), base);
    assert_eq!(clocks[1].elapsed(), base * 3 / 2);
    assert_eq!(
        clocks[2].elapsed(),
        base + std::time::Duration::from_secs(1)
    );
    let bad = Scenario::new("t").fault(
        0,
        Fault::ClockRate {
            node: 0,
            ppm: -1_000_000,
        },
    );
    assert_eq!(
        bad.run(Choices::seeded(1)).failure.unwrap().name,
        "clock-rate"
    );
}

#[test]
fn os_operations_are_charged_from_the_cost_table() {
    let base = Scenario::new("t")
        .roles(|env| lane_pair(env, 2))
        .with(all_roles_settled());
    let mut costs = base.costs;
    costs.disk_latency_ns = SECONDS;
    let fast = run_twice(&base, &Source::Seed(5)).unwrap();
    let slow_disk = run_twice(&base.clone().costs(costs), &Source::Seed(5)).unwrap();
    assert_eq!((&fast.failure, &slow_disk.failure), (&None, &None));
    // Each round's atomic write and read-back touch the disk several times.
    assert!(
        slow_disk.end >= fast.end + 4 * SECONDS,
        "{fast:?} {slow_disk:?}"
    );
    let slow_peer = run_twice(
        &base.clone().fault(
            0,
            Fault::SlowPeer {
                node: 0,
                factor: 200,
            },
        ),
        &Source::Seed(5),
    )
    .unwrap();
    assert_eq!(slow_peer.failure, None);
    assert!(slow_peer.end > fast.end, "{fast:?} {slow_peer:?}");
}

#[test]
fn pending_work_without_wake_is_a_liveness_failure() {
    let s = Scenario::new("t").roles(|env| {
        let mut roles: racer_dst::world::Roles = Vec::new();
        let lane = Lane::new(env.reactor(1, 8)?, DoneFlag::default());
        lane.executor.spawn_detached(pending::<()>());
        roles.push(("stuck".into(), Box::new(LaneRole(lane))));
        Ok(roles)
    });
    let out = s.run(Choices::seeded(1));
    let failure = out.failure.unwrap();
    assert_eq!(failure.class, Class::Liveness);
    assert_eq!(failure.name, "pending-without-wake");
    assert!(failure.message.contains("node 0 role stuck"), "{failure}");
    let shrunk = shrink_failure(&s, &Source::Seed(1), 100).unwrap();
    assert!(
        shrunk.is_empty(),
        "fails with the all-zero stream: {shrunk:?}"
    );
    assert!(shrink_failure(&Scenario::new("ok"), &Source::Seed(1), 10).is_none());
}

/// Always ready, never finishing.
struct Spin;

impl SimRole for Spin {
    fn step(&mut self) -> racer_io::Result<Next> {
        Ok(Next::Ready)
    }

    fn woken(&self) -> bool {
        false
    }

    fn pending(&self) -> bool {
        true
    }
}

#[test]
fn busy_loop_and_step_budget() {
    let s = Scenario::new("t")
        .roles(|_| Ok(vec![("spin".into(), Box::new(Spin) as Box<dyn SimRole>)]))
        .busy_limit(50);
    let failure = s.run(Choices::seeded(1)).failure.unwrap();
    assert_eq!(
        (failure.class, failure.name.as_str()),
        (Class::Liveness, "busy-loop")
    );
    assert_eq!(failure.step, 51);
    let out = s
        .clone()
        .busy_limit(1000)
        .max_steps(10)
        .run(Choices::seeded(1));
    assert_eq!((out.failure, out.steps, out.quiescent), (None, 10, false));
}

#[test]
fn safety_invariants_and_role_reports_fail_the_run() {
    let s = Scenario::new("t").roles(|env| lane_pair(env, 3)).invariant(
        "at-most-ten-steps",
        Class::Safety,
        |p| {
            if p.steps <= 10 {
                Ok(())
            } else {
                Err(format!("{} steps", p.steps))
            }
        },
    );
    let failure = s.run(Choices::seeded(1)).failure.unwrap();
    assert_eq!((failure.class, failure.step), (Class::Safety, 11));
    let s = Scenario::new("t").roles(|env| {
        let report = env.report.clone();
        let lane = Lane::new(env.reactor(1, 8)?, DoneFlag::default());
        lane.executor
            .spawn_detached(async move { report.violation("bytes differ") });
        Ok(vec![(
            "r".into(),
            Box::new(LaneRole(lane)) as Box<dyn SimRole>,
        )])
    });
    let failure = s.run(Choices::seeded(1)).failure.unwrap();
    assert_eq!(failure.name, "role-assert");
    assert_eq!(failure.message, "bytes differ");
}

#[test]
fn resource_invariants_run_only_at_quiescence() {
    let ran = Rc::new(Cell::new(0));
    let seen = ran.clone();
    let s = Scenario::new("t").roles(|env| lane_pair(env, 2)).invariant(
        "count",
        Class::Resource,
        move |p| {
            assert!(p.quiescent);
            seen.set(seen.get() + 1);
            Ok(())
        },
    );
    assert_eq!(s.run(Choices::seeded(1)).failure, None);
    assert_eq!(ran.get(), 1);
    // Ending on the time bound is not quiescence.
    let out = s.clone().end(MILLIS).run(Choices::seeded(1));
    assert!(!out.quiescent);
    assert_eq!(ran.get(), 1);
}

#[test]
fn fault_mix_is_drawn_from_choices_and_shrinks_to_nothing() {
    let s = Scenario::new("t")
        .nodes(4)
        .end(SECONDS)
        .fault_mix(FaultMix {
            max_faults: 8,
            crash_restart: 1,
            partition: 1,
            slow_peer: 1,
            clock_skew: 1,
            max_downtime: 10 * MILLIS,
            max_skew_ns: SECONDS,
            clock_rate: 1,
            max_rate_ppm: 1000,
            first_node: 0,
        });
    assert!(s.fault_schedule(&Choices::replay(&[])).is_empty());
    let a = s.fault_schedule(&Choices::seeded(4));
    assert_eq!(a, s.fault_schedule(&Choices::seeded(4)));
    let all: Vec<_> = (0..50)
        .flat_map(|i| s.fault_schedule(&Choices::seeded(i)))
        .collect();
    for (at, fault) in &all {
        assert!(*at < SECONDS + 10 * MILLIS + 1, "{fault:?}");
    }
    let crashes = all
        .iter()
        .filter(|(_, f)| matches!(f, Fault::Crash { .. }))
        .count();
    let restarts = all
        .iter()
        .filter(|(_, f)| matches!(f, Fault::Restart { .. }))
        .count();
    assert!(crashes > 0);
    assert_eq!(crashes, restarts, "every crash is paired with a restart");
    assert!(
        all.iter()
            .any(|(_, f)| matches!(f, Fault::ClockSkew { .. }))
    );
    assert!(
        all.iter()
            .any(|(_, f)| matches!(f, Fault::ClockRate { ppm, .. } if ppm.unsigned_abs() <= 1000))
    );
}

#[test]
fn faults_naming_missing_nodes_and_model_fidelity_without_a_spec_are_rejected() {
    let s = Scenario::new("t").fault(0, Fault::Crash { node: 9 });
    assert_eq!(s.run(Choices::seeded(1)).failure.unwrap().name, "fault");
    let s = Scenario::new("t").fidelity(Fidelity::Model);
    assert_eq!(s.run(Choices::seeded(1)).failure.unwrap().name, "fidelity");
}

#[test]
fn catalog_ci_seeds_pass_twice() {
    for entry in racer_dst::seeds::default_set().unwrap() {
        let s = scenarios::find(&entry.scenario).unwrap();
        let out = run_twice(&s, &entry.source).unwrap_or_else(|d| panic!("{entry}: {d}"));
        assert_eq!(out.failure, None, "{entry}");
    }
}

#[test]
fn default_seed_set_covers_every_model_variant() {
    let coverage = racer_dst::seeds::coverage(&racer_dst::seeds::default_set().unwrap());
    assert!(
        coverage.missing.is_empty() && coverage.stale.is_empty(),
        "missing {:?}, exercised but exempt {:?}",
        coverage.missing,
        coverage.stale
    );
}

/// Regression for a racer-io bug found by this harness: with `SimOs::seeded`,
/// an operation whose random submission delay counted down to zero was no
/// longer `runnable()`, so the lane reported `Idle` with the operation still
/// in flight and nothing woke it.
#[test]
fn seeded_sim_os_delay_does_not_stall() {
    let s = Scenario::new("t")
        .seeded_os(true)
        .roles(|env| lane_pair(env, 3))
        .with(all_roles_settled());
    for seed in 0..20 {
        assert_eq!(s.run(Choices::seeded(seed)).failure, None, "seed {seed}");
    }
}

/// A shared network gives every node the same OS, partitions are applied
/// and counted, the board is per run, and a crash leaves the shared disk
/// alone.
#[test]
fn shared_network_partitions_and_board() {
    use racer_io::ring::SocketAddress;
    #[derive(Default)]
    struct Seen {
        last_fault: Cell<u64>,
        starts: Cell<u32>,
    }
    let s = Scenario::new("shared")
        .nodes(3)
        .shared_network(|n| SocketAddress::Inet(format!("10.9.0.{}:1", n + 1).parse().unwrap()))
        .roles(|env| {
            env.board
                .get::<Seen>()
                .starts
                .set(env.board.get::<Seen>().starts.get() + 1);
            if env.index == 0 && env.incarnation == 0 {
                env.os.write_file(Path::new("/shared"), b"x").unwrap();
            }
            // Every node sees node 0's file: one disk.
            assert_eq!(env.os.read_file(Path::new("/shared")).unwrap(), b"x");
            Ok(Vec::new())
        })
        .fault(MILLIS, Fault::Partition { a: 1, b: 0 })
        .fault(2 * MILLIS, Fault::Partition { a: 2, b: 2 })
        .fault(3 * MILLIS, Fault::Crash { node: 0 })
        .fault(4 * MILLIS, Fault::Restart { node: 0 })
        .fault(5 * MILLIS, Fault::Heal { a: 0, b: 1 })
        .invariant("observe", Class::Liveness, |probe| {
            let seen = probe.board.get::<Seen>();
            seen.last_fault.set(probe.last_fault.unwrap_or(0));
            if probe.partitions != 0 {
                return Err(format!("{} partitions left", probe.partitions));
            }
            Ok(())
        })
        .invariant("partitioned", Class::Liveness, |probe| {
            let seen = probe.board.get::<Seen>();
            // Three starts and one restart, in this run only.
            if seen.starts.get() != 4 || seen.last_fault.get() != 5 * MILLIS {
                return Err(format!(
                    "starts {} last fault {}",
                    seen.starts.get(),
                    seen.last_fault.get()
                ));
            }
            Ok(())
        })
        .end(10 * MILLIS);
    let out = run_twice(&s, &Source::Seed(3)).unwrap();
    assert_eq!(out.failure, None);
}
