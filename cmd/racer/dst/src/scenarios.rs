//! Built-in scenarios run by `make racer-dst` and the CI seed set.
use crate::{
    Scenario,
    engine::{MILLIS, SECONDS},
    fixture::lane_pair,
    invariants::{Class, all_roles_settled},
    scenario::FaultMix,
    world::Fault,
};

/// Every built-in scenario.
pub fn catalog() -> Vec<Scenario> {
    vec![
        Scenario::new("lane-pair")
            .roles(|env| lane_pair(env, 8))
            .with(all_roles_settled())
            .end(10 * SECONDS),
        Scenario::new("lane-pair-faults")
            .nodes(3)
            .sites(2)
            .roles(|env| lane_pair(env, 6))
            .fault(
                3 * MILLIS,
                Fault::ClockSkew {
                    node: 2,
                    offset_ns: -250 * MILLIS as i64,
                },
            )
            .fault(7 * MILLIS, Fault::Crash { node: 1 })
            .fault(12 * MILLIS, Fault::Restart { node: 1 })
            .fault_mix(FaultMix {
                max_faults: 4,
                crash_restart: 3,
                partition: 1,
                slow_peer: 1,
                clock_skew: 1,
                max_downtime: 20 * MILLIS,
                max_skew_ns: SECONDS,
            })
            .with(all_roles_settled())
            .invariant("bounded-steps-per-node", Class::Safety, |probe| {
                // Every node runs at most two roles.
                let mut per = [0u32; 3];
                for r in probe.roles {
                    per[r.node as usize] += 1;
                }
                if per.iter().all(|n| *n <= 2) {
                    Ok(())
                } else {
                    Err(format!("roles per node {per:?}"))
                }
            })
            .end(10 * SECONDS),
    ]
}

/// The scenario named `name`.
pub fn find(name: &str) -> Option<Scenario> {
    catalog().into_iter().find(|s| s.name == name)
}
