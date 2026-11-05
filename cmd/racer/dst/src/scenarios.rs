//! Built-in scenarios run by `make racer-dst` and the CI seed set.
use crate::{
    Scenario,
    engine::{MILLIS, Nanos, SECONDS},
    fixture::lane_pair,
    invariants::{Class, all_roles_settled},
    model::{ModelSpec, Outage, RangeRead, Storm},
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
            range: None,
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

fn error(report: &Value, name: &str) -> u64 {
    report["metrics"]["errors"][name].as_u64().unwrap_or(0)
}

fn storm(at: Nanos, object: u64, nodes: u32, window: Nanos) -> Storm {
    Storm {
        at,
        object,
        first: 0,
        nodes,
        window,
        range: None,
    }
}

/// Ported from racer-sim `cold_start_completes_with_bounded_origin_traffic` and
/// `two_thousand_nodes_cold_start` (`sim/tests/contention_scenarios.rs`): every node cold-starts
/// on one object within 10 ms. Every read ends; reads fail only with `Overloaded` or
/// `Unavailable`, at most 1 in 100; each page reaches origin at most once per owner; pages are
/// published. racer-sim's per-node memory bound has no Model counterpart (see MODEL.md).
pub fn cold_start(nodes: u32) -> Scenario {
    let spec = ModelSpec {
        storms: vec![storm(0, 0, nodes, 10 * MILLIS)],
        ..ModelSpec::default()
    };
    let total = u64::from(nodes) * u64::from(spec.pages_per_object);
    let replicas = topology::REPLICAS as u64;
    let spec = spec
        .check("every-read-ends", move |r| {
            let ended = counter(r, "reads_completed") + counter(r, "reads_failed");
            (ended == total)
                .then_some(())
                .ok_or(format!("{ended} of {total} reads ended"))
        })
        .check("only-admission-or-unavailable-failures", move |r| {
            let failed = counter(r, "reads_failed");
            let allowed = error(r, "Overloaded") + error(r, "Unavailable");
            (error(r, "HopBudgetExhausted") == 0 && failed == allowed && failed * 100 <= total)
                .then_some(())
                .ok_or(format!("failures: {}", r["metrics"]["errors"]))
        })
        .check("origin-at-most-once-per-owner", move |r| {
            let max = r["origin_per_page"]["max_requests_per_page"]
                .as_u64()
                .unwrap_or(0);
            (1..=replicas)
                .contains(&max)
                .then_some(())
                .ok_or(format!("max origin requests per page {max}"))
        })
        .check("pages-published", |r| {
            (counter(r, "disk_writes") > 0)
                .then_some(())
                .ok_or("no disk writes".into())
        });
    Scenario::new(format!("model-cold-start-{nodes}"))
        .nodes(nodes)
        .sites(4)
        .model(spec)
        .end(30 * SECONDS)
        .max_steps(u64::MAX)
}

/// Ported from racer-sim `hot_page_storm` with `hot_page_readers_fan_out_through_helpers` and
/// `mesh_hot_page_readers_fan_out_through_column_helpers`
/// (`sim/tests/contention_scenarios.rs`): one node reads a one-page object, then 5 s later
/// `nodes` nodes read it within 1 ms over 32 MiB/s NICs. Owners streaming their hot-page body
/// limit shed readers to helpers; every read completes, helpers serve copies and the page
/// reaches origin once.
pub fn hot_page_storm(nodes: u32) -> Scenario {
    let spec = ModelSpec {
        pages_per_object: 1,
        storms: vec![storm(0, 0, 1, 1), storm(5 * SECONDS, 0, nodes, MILLIS)],
        ..ModelSpec::default()
    };
    let reads = u64::from(nodes) + 1;
    let spec = spec
        .check("every-read-completes", move |r| {
            let (done, failed) = (counter(r, "reads_completed"), counter(r, "reads_failed"));
            (done == reads && failed == 0)
                .then_some(())
                .ok_or(format!("{done} completed, {failed} failed of {reads}"))
        })
        .check("owners-shed-to-helpers", |r| {
            (counter(r, "sheds") > 0 && counter(r, "helper_copies") > 0)
                .then_some(())
                .ok_or(format!("no helper fan-out: {}", r["metrics"]["counters"]))
        })
        .check("origin-once", |r| {
            let origin = counter(r, "origin_requests");
            (origin == 1)
                .then_some(())
                .ok_or(format!("{origin} origin requests"))
        });
    let mut costs = crate::costs::Costs::checked_in();
    costs.nic_bytes_per_sec = 32 << 20;
    Scenario::new(format!("model-hot-page-storm-{nodes}"))
        .nodes(nodes)
        .sites(4)
        .costs(costs)
        .model(spec)
        .end(40 * SECONDS)
        .max_steps(u64::MAX)
}

/// Ported from racer-sim `origin_forbidden_reelects_a_waiter`
/// (`sim/tests/contention_scenarios.rs`): 64 nodes read one object while half of the origin
/// fetches answer 403. A 403 fails only the leader that received it, so some reads fail with
/// `OriginForbidden` and the rest complete through re-elected waiters.
pub fn origin_forbidden() -> Scenario {
    let spec = ModelSpec {
        storms: vec![storm(0, 1, 64, MILLIS)],
        origin_errors: crate::resources::OriginErrors {
            p403: 0.5,
            ..Default::default()
        },
        ..ModelSpec::default()
    };
    let total = 64 * u64::from(spec.pages_per_object);
    let spec = spec
        .check("forbidden-fails-reads", |r| {
            (counter(r, "origin_403") > 0 && error(r, "OriginForbidden") > 0)
                .then_some(())
                .ok_or(format!("no 403 failures: {}", r["metrics"]))
        })
        .check("waiters-recover", move |r| {
            let done = counter(r, "reads_completed");
            let ended = done + counter(r, "reads_failed");
            (ended == total && done > counter(r, "reads_failed"))
                .then_some(())
                .ok_or(format!("{done} of {total} reads completed"))
        });
    Scenario::new("model-origin-forbidden")
        .nodes(64)
        .sites(4)
        .model(spec)
        .end(3 * SECONDS)
}

/// Model-only (racer-sim's range reads do not run through `ReadWindow`): 32 nodes read one
/// cold object as a range through windows of 4 pages with 4 spill pages, then read it again
/// 1 s later from their local copies. Every page is delivered in order, origin traffic stays
/// per owner, and the second pass reads from disk.
pub fn range_readers() -> Scenario {
    const NODES: u32 = 32;
    let range = Some(RangeRead {
        window_pages: 4,
        spill: 4,
        leased: true,
    });
    let spec = ModelSpec {
        storms: vec![
            Storm {
                range,
                ..storm(0, 0, NODES, MILLIS)
            },
            Storm {
                range,
                ..storm(SECONDS, 0, NODES, MILLIS)
            },
        ],
        ..ModelSpec::default()
    };
    let pages = u64::from(spec.pages_per_object);
    let replicas = topology::REPLICAS as u64;
    let spec = spec
        .check("every-page-delivered", move |r| {
            let done = counter(r, "reads_completed");
            (done == 2 * u64::from(NODES) * pages && counter(r, "reads_failed") == 0)
                .then_some(())
                .ok_or(format!("{done} pages delivered: {}", r["metrics"]))
        })
        .check("origin-per-owner", move |r| {
            let origin = counter(r, "origin_requests");
            (origin <= pages * replicas)
                .then_some(())
                .ok_or(format!("origin {origin}"))
        })
        .check("second-pass-reads-disk", move |r| {
            (counter(r, "disk_reads") >= u64::from(NODES) * pages)
                .then_some(())
                .ok_or(format!("disk reads: {}", r["metrics"]["counters"]))
        });
    Scenario::new("model-range-readers")
        .nodes(NODES)
        .sites(4)
        .model(spec)
        .end(4 * SECONDS)
}

/// Model-only: [`concurrent_cold_readers`] where every page read first refreshes its object's
/// metadata through the worker's `MetadataCohort`, with 10% of origin requests failing with a
/// 5xx. Refreshes coalesce per cohort, and failures surface only as `Unavailable` after the
/// client's retries.
pub fn metadata_readers() -> Scenario {
    const NODES: u32 = 32;
    let spec = ModelSpec {
        storms: vec![storm(0, 0, NODES, MILLIS)],
        metadata: true,
        origin_errors: crate::resources::OriginErrors {
            p5xx: 0.1,
            ..Default::default()
        },
        ..ModelSpec::default()
    };
    let total = u64::from(NODES) * u64::from(spec.pages_per_object);
    let spec = spec
        .check("every-read-ends", move |r| {
            let ended = counter(r, "reads_completed") + counter(r, "reads_failed");
            let failed = counter(r, "reads_failed");
            (ended == total && failed == error(r, "Unavailable"))
                .then_some(())
                .ok_or(format!("{ended} of {total} ended: {}", r["metrics"]))
        })
        .check("refreshes-coalesce", move |r| {
            let refreshes = counter(r, "metadata_requests");
            (refreshes > 0 && refreshes < total / 4)
                .then_some(())
                .ok_or(format!("{refreshes} metadata requests for {total} reads"))
        });
    Scenario::new("model-metadata-readers")
        .nodes(NODES)
        .sites(4)
        .model(spec)
        .end(4 * SECONDS)
}

/// Model-only fault mix: 32 nodes read one object page by page and another as ranges while
/// 20% of origin fetches answer 403 and 5% answer 5xx, and four nodes lose their caches for the
/// first 300 ms (they answer peers with `Miss`). Every read ends; reads fail only with
/// `OriginForbidden` (not retried) or `Unavailable` (after retries); a failed range read
/// discards its buffered pages.
pub fn faults() -> Scenario {
    const NODES: u32 = 32;
    let range = Some(RangeRead {
        window_pages: 4,
        spill: 0,
        leased: false,
    });
    let spec = ModelSpec {
        storms: vec![
            storm(0, 0, NODES, MILLIS),
            Storm {
                range,
                ..storm(0, 1, NODES, MILLIS)
            },
        ],
        origin_errors: crate::resources::OriginErrors {
            p403: 0.2,
            p5xx: 0.05,
            ..Default::default()
        },
        outages: (0..4)
            .map(|node| Outage {
                node: node * 7,
                at: 0,
                until: 300 * MILLIS,
            })
            .collect(),
        ..ModelSpec::default()
    };
    let total = 2 * u64::from(NODES) * u64::from(spec.pages_per_object);
    let spec = spec
        .check("every-read-ends", move |r| {
            let ended = counter(r, "reads_completed") + counter(r, "reads_failed");
            (ended == total)
                .then_some(())
                .ok_or(format!("{ended} of {total} reads ended"))
        })
        .check("failures-are-origin-errors", |r| {
            let failed = counter(r, "reads_failed");
            let explained = error(r, "OriginForbidden") + error(r, "Unavailable");
            (failed == explained && counter(r, "reads_completed") > failed)
                .then_some(())
                .ok_or(format!("failures: {}", r["metrics"]["errors"]))
        })
        .check("outages-answer-miss", |r| {
            (r["coverage"]["Served::Miss"].as_u64().unwrap_or(0) > 0)
                .then_some(())
                .ok_or("no outage misses".into())
        });
    Scenario::new("model-faults")
        .nodes(NODES)
        .sites(4)
        .model(spec)
        .end(10 * SECONDS)
}

/// Every built-in scenario.
pub fn catalog() -> Vec<Scenario> {
    vec![
        concurrent_cold_readers(),
        cold_start(400),
        cold_start(2000),
        hot_page_storm(512),
        origin_forbidden(),
        range_readers(),
        metadata_readers(),
        faults(),
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
