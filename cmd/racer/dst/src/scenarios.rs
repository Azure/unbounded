//! Built-in scenarios run by `make racer-dst` and the CI seed set.
use crate::{
    Scenario,
    engine::{MILLIS, SECONDS},
    fixture::lane_pair,
    invariants::{Class, all_roles_settled},
    model::{ModelSpec, Storm},
    scenario::FaultMix,
    world::Fault,
};
use serde_json::Value;

fn counter(report: &Value, name: &str) -> u64 {
    report["metrics"]["counters"][name].as_u64().unwrap_or(0)
}

/// Ported from racer-sim `concurrent_cold_readers_join_flights`
/// (`sim/tests/contention_scenarios.rs`): 64 nodes read every page of one cold object within
/// 1 ms. Every read is delivered, readers join flights, and origin traffic stays per owner.
pub fn concurrent_cold_readers() -> Scenario {
    const NODES: u32 = 64;
    let replicas = topology::REPLICAS as u64;
    let spec = ModelSpec {
        storms: vec![Storm {
            at: 0,
            object: 0,
            first: 0,
            nodes: NODES,
            window: MILLIS,
        }],
        ..ModelSpec::default()
    };
    let pages = u64::from(spec.pages_per_object);
    let spec = spec
        .check("all-reads-delivered", move |r| {
            let done = counter(r, "reads_completed");
            (done == u64::from(NODES) * pages)
                .then_some(())
                .ok_or(format!("{done} reads completed: {}", r["metrics"]))
        })
        .check("readers-join-flights", |r| {
            (counter(r, "flight_joins") > 0)
                .then_some(())
                .ok_or(format!("no flight joins: {}", r["metrics"]))
        })
        .check("origin-per-owner", move |r| {
            let origin = counter(r, "origin_requests");
            let max = r["origin_per_page"]["max_requests_per_page"]
                .as_u64()
                .unwrap_or(u64::MAX);
            (origin <= pages * replicas && max <= replicas * 2)
                .then_some(())
                .ok_or(format!("origin {origin}, max per page {max}"))
        })
        .check("origin-a-fraction-of-reads", |r| {
            let origin = counter(r, "origin_requests");
            (origin < counter(r, "reads_completed") / 8)
                .then_some(())
                .ok_or(format!("origin {origin}: {}", r["metrics"]))
        });
    Scenario::new("model-concurrent-cold-readers")
        .nodes(NODES)
        .sites(4)
        .model(spec)
        .end(4 * SECONDS)
}

/// Every built-in scenario.
pub fn catalog() -> Vec<Scenario> {
    vec![
        concurrent_cold_readers(),
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
                clock_rate: 1,
                max_rate_ppm: 200_000,
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
