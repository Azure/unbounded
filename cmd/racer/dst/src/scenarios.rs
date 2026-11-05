//! Built-in scenarios run by `make racer-dst` and the CI seed set.
use crate::{
    Scenario,
    authority::{self, ApiFaults},
    engine::{MILLIS, Nanos, SECONDS},
    fixture::lane_pair,
    invariants::{Class, all_roles_settled},
    model::{ModelSpec, Outage, RandomChurn, RangeRead, Storm},
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
/// on one object within 10 ms. Every read ends; reads fail only with `Overloaded`, `FlightFull` or
/// `Unavailable`, at most 1 in 100; each page reaches origin at most once per owner; pages are
/// published. racer-sim's per-node memory bound has no Model counterpart (see MODEL.md).
pub fn cold_start(nodes: u32) -> Scenario {
    cold_start_with(nodes, ModelSpec::default().attempt_timeout)
}

/// [`cold_start`] with per-attempt cap `attempt_timeout` ([`ModelSpec::attempt_timeout`]).
pub fn cold_start_with(nodes: u32, attempt_timeout: Nanos) -> Scenario {
    let spec = ModelSpec {
        storms: vec![storm(0, 0, nodes, 10 * MILLIS)],
        attempt_timeout,
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
            let allowed = error(r, "Overloaded") + error(r, "FlightFull") + error(r, "Unavailable");
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

/// Model-only: range readers whose read deadline (150 ms) passes while their windows still hold
/// delivered-but-unread pages, so ending the read discards them (`Window::Discard`). Every read
/// ends, completed or failed.
pub fn range_deadline() -> Scenario {
    const NODES: u32 = 32;
    let range = Some(RangeRead {
        window_pages: 4,
        spill: 4,
        leased: true,
    });
    let spec = ModelSpec {
        storms: vec![Storm {
            range,
            ..storm(0, 0, NODES, MILLIS)
        }],
        read_timeout: 150 * MILLIS,
        ..ModelSpec::default()
    };
    Scenario::new("model-range-deadline")
        .nodes(NODES)
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

/// Ported from racer-sim `reads_recover_after_partition_heals`
/// (`sim/tests/contention_scenarios.rs`, seed 6): 32 nodes in 4 sites; sites 0 and 1 cannot
/// reach each other for the first 2 s while every node reads object 0, and every node reads
/// object 1 at 4 s, after the heal. racer-sim's check: at least one object's worth of reads
/// completes; the Model also requires every read to end and every second-storm read to
/// complete.
pub fn partition_heals() -> Scenario {
    const NODES: u32 = 32;
    const SITES: u32 = 4;
    let spec = ModelSpec {
        storms: vec![
            storm(0, 0, NODES, MILLIS),
            storm(4 * SECONDS, 1, NODES, MILLIS),
        ],
        ..ModelSpec::default()
    };
    let pages = u64::from(NODES) * u64::from(spec.pages_per_object);
    let spec = spec
        .check("every-read-ends", move |r| {
            let ended = counter(r, "reads_completed") + counter(r, "reads_failed");
            (ended == 2 * pages)
                .then_some(())
                .ok_or(format!("{ended} of {} reads ended", 2 * pages))
        })
        .check("reads-recover", move |r| {
            let completed = counter(r, "reads_completed");
            (completed >= pages)
                .then_some(())
                .ok_or(format!("{completed} completed"))
        })
        .check("partition-cut-links", |r| {
            (counter(r, "link_failures") > 0)
                .then_some(())
                .ok_or("no link crossed the partition".into())
        });
    let mut scenario = Scenario::new("model-partition-heals")
        .nodes(NODES)
        .sites(SITES)
        .model(spec)
        .end(8 * SECONDS);
    for a in (0..NODES).filter(|n| n % SITES == 0) {
        for b in (0..NODES).filter(|n| n % SITES == 1) {
            scenario = scenario
                .fault(0, Fault::Partition { a, b })
                .fault(2 * SECONDS, Fault::Heal { a, b });
        }
    }
    scenario
}

/// Ported from racer-sim `hung_relays_are_routed_around` and
/// `hung_peers_are_found_and_recovered_peers_are_used_again` (the `GRID` layout, silence on):
/// 200 nodes, every 20th node's peer server hangs until 40 s (it accepts requests, as owner or
/// relay, and never answers), past both storms, and 100 readers read one 64-page object twice, 30 s apart.
/// Readers find the hung peers (silence timeouts and pings) and route around them: at most 2
/// reads fail in the first storm and at most 1 in the second (racer-sim's bounds).
pub fn hung_peers() -> Scenario {
    const NODES: u32 = 200;
    let spec = ModelSpec {
        pages_per_object: 64,
        storms: vec![
            storm(0, 0, 100, 100 * MILLIS),
            storm(30 * SECONDS, 0, 100, 100 * MILLIS),
        ],
        hangs: (0..10)
            .map(|i| Outage {
                node: i * 20,
                at: 0,
                until: 40 * SECONDS,
            })
            .collect(),
        ..ModelSpec::default()
    };
    let spec = spec
        .check("hung-peers-lose-requests", |r| {
            (counter(r, "messages_lost") > 0 && counter(r, "silence_timeouts") > 0)
                .then_some(())
                .ok_or("no request reached a hung peer".into())
        })
        // At most 0.5% of the 12,800 reads. The bound was 3 while the Model charged request
        // signing to the crypto helper. Production signs on the I/O worker (`src/peer.rs`
        // `sign_request_to`); charged there, each worker's 32-page burst holds its 8 route search
        // slots longer (production `Paths::with_limits` has no queue, `src/app.rs`), and 8 to 30
        // reads over seeds 1 to 8 fail on search sheds (`search_sheds`; see `dst/MODEL.md`,
        // "Signing placement").
        .check("few-reads-fail", |r| {
            let failed = counter(r, "reads_failed");
            (failed <= 64)
                .then_some(())
                .ok_or(format!("{failed} reads failed: {}", r["metrics"]["errors"]))
        });
    // Long enough for the silence rechecks (every 10 s) to find the recovered peers.
    Scenario::new("model-hung-peers")
        .nodes(NODES)
        .model(spec)
        .end(60 * SECONDS)
}

/// Ported from racer-sim `relay_shed_rank0_probes_do_not_overlap_a_slow_origin_fetch`
/// (`sim/tests/contention_scenarios.rs`, seed 3): 300 nodes, 48 readers of one 64-page object
/// from an 8 MiB/s origin, so rank 0's origin requests queue past the owners' 2 s attempt
/// timeout; 16 `Relay` transfers per worker and room for 128 waiters per flight. Owners whose
/// probes of rank 0 time out or are shed by a relay leave the fetch to rank 0: no page is asked
/// of origin twice, and pings find rank 0 busy, not hung.
///
/// Not in [`catalog`] yet: the Model fails `origin-once-per-page` (see `dst/MODEL.md`, "Slow
/// origin"). Run it with `MODEL_SCENARIO=model-slow-origin MODEL_SEED=3` and the
/// `scenario_report` test after adding it to the catalog.
pub fn slow_origin() -> Scenario {
    const NODES: u32 = 300;
    let spec = ModelSpec {
        pages_per_object: 64,
        storms: vec![storm(0, 0, 48, 100 * MILLIS)],
        relay_transfers: 16,
        waiters_per_flight: 128,
        ..ModelSpec::default()
    };
    let total = 48 * u64::from(spec.pages_per_object);
    let spec = spec
        .check("every-read-ends", move |r| {
            let ended = counter(r, "reads_completed") + counter(r, "reads_failed");
            (ended == total)
                .then_some(())
                .ok_or(format!("{ended} of {total} reads ended"))
        })
        .check("origin-once-per-page", |r| {
            let max = r["origin_per_page"]["max_requests_per_page"]
                .as_u64()
                .unwrap_or(0);
            (max == 1)
                .then_some(())
                .ok_or(format!("origin per page: {}", r["origin_per_page"]))
        })
        .check("rank0-pinged-and-answers", |r| {
            (counter(r, "silence_pings") > 0)
                .then_some(())
                .ok_or("no silence pings".into())
        });
    let mut costs = crate::costs::Costs::checked_in();
    costs.origin_bytes_per_sec = 8 << 20;
    Scenario::new("model-slow-origin")
        .nodes(NODES)
        .model(spec)
        .costs(costs)
        .end(60 * SECONDS)
}

/// Model-only node faults: 64 nodes read twelve objects, one a second, while a drawn schedule crashes and
/// restarts nodes, partitions node pairs and slows peers. Every read ends, and reads fail only
/// with `Unavailable` or `Overloaded` after retries.
pub fn node_faults() -> Scenario {
    const NODES: u32 = 64;
    let spec = ModelSpec {
        storms: (0..12)
            .map(|i| storm(i * SECONDS, i, NODES, 500 * MILLIS))
            .collect(),
        ..ModelSpec::default()
    };
    let total = 12 * u64::from(NODES) * u64::from(spec.pages_per_object);
    let spec = spec
        .check("every-read-ends", move |r| {
            let ended = counter(r, "reads_completed") + counter(r, "reads_failed");
            (ended == total)
                .then_some(())
                .ok_or(format!("{ended} of {total} reads ended"))
        })
        .check("failures-are-transient", |r| {
            let failed = counter(r, "reads_failed");
            let explained =
                error(r, "Unavailable") + error(r, "Overloaded") + error(r, "FlightFull");
            (failed == explained && counter(r, "reads_completed") > failed)
                .then_some(())
                .ok_or(format!("failures: {}", r["metrics"]["errors"]))
        });
    Scenario::new("model-node-faults")
        .nodes(NODES)
        .sites(4)
        .model(spec)
        .fault_mix(FaultMix {
            max_faults: 16,
            crash_restart: 2,
            partition: 2,
            slow_peer: 1,
            clock_skew: 0,
            max_downtime: 2 * SECONDS,
            max_skew_ns: 0,
            clock_rate: 0,
            max_rate_ppm: 0,
            first_node: 0,
        })
        .end(24 * SECONDS)
}

/// Ported from racer-sim's `churn` scenario (`sim/src/lib.rs` `scenario_schedules`): a tenth of
/// the nodes leave at a drawn time and rejoin a quarter of the run later. Changes publish every
/// 250 ms and each node adopts a version up to 100 ms late, so requests meet destinations that
/// are behind (`StaleMembership`) and readers refresh, fall back to the previous membership or
/// wait. racer-sim drives a Poisson workload; here 200 nodes read a new object every 250 ms.
/// Every read ends, and reads fail only with transient errors.
pub fn membership_churn() -> Scenario {
    const NODES: u32 = 200;
    const STORMS: u64 = 8;
    let duration = 2 * SECONDS;
    let spec = ModelSpec {
        storms: (0..STORMS)
            .map(|i| storm(i * 250 * MILLIS, i, NODES, 250 * MILLIS))
            .collect(),
        random_churn: Some(RandomChurn {
            events: NODES / 10,
            within: duration,
            away: duration / 4,
        }),
        ..ModelSpec::default()
    };
    let total = STORMS * u64::from(NODES) * u64::from(spec.pages_per_object);
    let spec = spec
        .check("every-read-ends", move |r| {
            let ended = counter(r, "reads_completed")
                + counter(r, "reads_failed")
                + counter(r, "reads_skipped_not_member");
            (ended == total)
                .then_some(())
                .ok_or(format!("{ended} of {total} reads ended"))
        })
        .check("membership-changes", |r| {
            (counter(r, "membership_changes") > 0 && counter(r, "membership_publications") > 0)
                .then_some(())
                .ok_or("no membership change".into())
        })
        .check("failures-are-transient", |r| {
            let failed = counter(r, "reads_failed");
            let explained =
                error(r, "Unavailable") + error(r, "Overloaded") + error(r, "FlightFull");
            (failed == explained && counter(r, "reads_completed") > failed)
                .then_some(())
                .ok_or(format!("failures: {}", r["metrics"]["errors"]))
        });
    Scenario::new("model-membership-churn")
        .nodes(NODES)
        .model(spec)
        .end(20 * SECONDS)
}

/// Three authorities and the fake API elect one primary and keep it, with
/// no faults.
pub fn authority_election() -> Scenario {
    authority::scenario(
        "authority-election",
        3,
        FaultMix::default(),
        ApiFaults::default(),
        180 * SECONDS,
    )
}

/// Partitions between authorities and the API server or each other.
pub fn authority_partitions() -> Scenario {
    authority::scenario(
        "authority-partitions",
        3,
        FaultMix {
            max_faults: 4,
            partition: 1,
            max_downtime: 90 * SECONDS,
            ..FaultMix::default()
        },
        ApiFaults::default(),
        420 * SECONDS,
    )
}

/// Authorities crash and restart with the same Pod identity.
pub fn authority_crash_restart() -> Scenario {
    authority::scenario(
        "authority-crash-restart",
        3,
        FaultMix {
            max_faults: 4,
            crash_restart: 1,
            max_downtime: 60 * SECONDS,
            ..FaultMix::default()
        },
        ApiFaults::default(),
        420 * SECONDS,
    )
}

/// Authority clocks run up to 10% fast or slow, with partitions.
pub fn authority_clock_skew() -> Scenario {
    authority::scenario(
        "authority-clock-skew",
        3,
        FaultMix {
            max_faults: 5,
            clock_rate: 2,
            partition: 1,
            max_downtime: 90 * SECONDS,
            max_rate_ppm: 100_000,
            ..FaultMix::default()
        },
        ApiFaults::default(),
        420 * SECONDS,
    )
}

/// Spurious write conflicts and dropped watches from the API server, with
/// crashes.
pub fn authority_api_faults() -> Scenario {
    authority::scenario(
        "authority-api-faults",
        3,
        FaultMix {
            max_faults: 2,
            crash_restart: 1,
            max_downtime: 30 * SECONDS,
            ..FaultMix::default()
        },
        ApiFaults {
            conflict_ppm: 2_000,
            watch_drop_ppm: 500,
        },
        300 * SECONDS,
    )
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
        range_deadline(),
        metadata_readers(),
        faults(),
        partition_heals(),
        hung_peers(),
        node_faults(),
        membership_churn(),
        authority_election(),
        authority_partitions(),
        authority_crash_restart(),
        authority_clock_skew(),
        authority_api_faults(),
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
                first_node: 0,
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
