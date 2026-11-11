//! Built-in scenarios run by `make racer-dst` and the CI seed set.
use crate::{
    Scenario,
    authority::{self, ApiFaults},
    engine::{MILLIS, Nanos, SECONDS},
    fixture::lane_pair,
    invariants::{Class, all_roles_settled},
    model::{
        ModelSpec, OriginBurst, Outage, RandomChurn, RangeRead, SiteCut, Storm, Workload,
        control::ControlSpec,
    },
    resources::OriginErrors,
    scenario::FaultMix,
    world::Fault,
};
use serde_json::Value;

fn counter(report: &Value, name: &str) -> u64 {
    report["metrics"]["counters"][name].as_u64().unwrap_or(0)
}

/// 64 nodes read every page of one cold object within
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

/// Every node cold-starts
/// on one object within 10 ms. Every read ends; reads fail only with `Overloaded`, `FlightFull` or
/// `Unavailable`, at most 1 in 100; each page reaches origin at most once per owner; pages are
/// published. The Model does
/// not bound per-node memory (see MODEL.md).
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

/// One node reads a one-page object, then 5 s later
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

/// 64 nodes read one object while half of the origin
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

/// Range reads through `ReadWindow`: 32 nodes read one
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

/// 32 nodes in 4 sites; sites 0 and 1 cannot
/// reach each other for the first 2 s while every node reads object 0, and every node reads
/// object 1 at 4 s, after the heal. At least one object's worth of reads
/// completes, every read ends, and every second-storm read completes.
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

/// Hung peers, with silence tracking on: 200 nodes, every 20th node's peer server hangs until 40 s (it accepts requests, as owner or
/// relay, and never answers), past both storms, and 100 readers read one 64-page object twice, 30 s apart.
/// Readers find the hung peers (silence timeouts and pings) and route around them: at most 2
/// reads fail in the first storm and at most 1 in the second.
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
        // reads over seeds 1 to 8 fail on search sheds (`search_sheds`).
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

/// 300 nodes, 48 readers of one 64-page object
/// from an 8 MiB/s origin, so rank 0's origin requests queue past the owners' 2 s attempt
/// timeout; 16 `Relay` transfers per worker and room for 128 waiters per flight. Owners whose
/// probes of rank 0 time out or are shed by a relay leave the fetch to rank 0: no page is asked
/// of origin twice, and pings find rank 0 busy, not hung.
///
/// Owners above rank 0 that probe it copy-only wait on its running fill (production
/// `Fill::copy_only_joining`) rather than fetching the page themselves.
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

/// Membership churn: a tenth of
/// the nodes leave at a drawn time and rejoin a quarter of the run later. Changes publish every
/// 250 ms and each node adopts a version up to 100 ms late, so requests meet destinations that
/// are behind (`StaleMembership`) and readers refresh, fall back to the previous membership or
/// wait. 200 nodes read a new object every 250 ms.
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

/// The control scenario `plan` ([`crate::model::control::plan`]) at
/// `nodes` Pods for its default duration, in Model fidelity: real seat,
/// election, admission and authorization rules, and primary passes driven
/// by the real `Shell`. Checks: every live client enrolled, and the plan's
/// own effect (failover, growth and shrink, partitioned clients) happened.
pub fn control(plan: &'static str, nodes: u32) -> Scenario {
    let end = match plan {
        "enrollment-storm" => 600 * SECONDS,
        "ctl-churn" => 1200 * SECONDS,
        _ => 900 * SECONDS,
    };
    let mut spec = ModelSpec {
        control: Some(ControlSpec {
            plan,
            overrides: None,
            combined: false,
        }),
        ..ModelSpec::default()
    }
    .check("everyone-enrolled", |r| {
        let c = &r["control"];
        (c["enrolled"] == c["live_clients"] && c["enrolled"].as_u64() > Some(0))
            .then_some(())
            .ok_or(format!(
                "enrolled {} of {}",
                c["enrolled"], c["live_clients"]
            ))
    })
    .check("published", |r| {
        (counter(r, "ctl_commits") > 0 && r["control"]["ready_seats"].as_u64() > Some(0))
            .then_some(())
            .ok_or("nothing committed or no ready seat".into())
    })
    .check("converges-checked", |r| {
        let checks = r["invariants"]["invariants"]
            .as_array()
            .into_iter()
            .flatten()
            .find(|i| i["name"] == "ctl_client_converges")
            .and_then(|i| i["checks"].as_u64())
            .unwrap_or(0);
        (checks > 0)
            .then_some(())
            .ok_or("convergence never checked".into())
    });
    spec = match plan {
        "failover" => spec.check("failover", |r| {
            let n = r["metrics"]["histograms"]["ctl_failover_ns"]["count"]
                .as_u64()
                .unwrap_or(0);
            (n >= 2 && counter(r, "ctl_primary_terms") >= 3)
                .then_some(())
                .ok_or(format!("{n} failovers"))
        }),
        "ctl-churn" => spec.check("churn", |r| {
            (counter(r, "ctl_pods_added") > 0 && counter(r, "ctl_pods_removed") > 0)
                .then_some(())
                .ok_or("no growth or shrink".into())
        }),
        "lagging" => spec.check("lagging", |r| {
            (counter(r, "ctl_unreachable") > 0 && counter(r, "ctl_crashes") > 0)
                .then_some(())
                .ok_or("no partitioned client or crash".into())
        }),
        _ => spec,
    };
    let short = plan.trim_start_matches("ctl-");
    Scenario::new(format!("model-ctl-{short}-{nodes}"))
        .nodes(nodes)
        .model(spec)
        .end(end)
}

/// The combined scenario `name`: the control
/// plan it names plus the Model read path on one timeline, at `nodes` nodes
/// over 4 sites with a Zipf read load of `rate` reads per node per second for
/// `secs` (400 s to 1200 s by scenario when `None`), and the
/// default data-plane faults ([`DataFaults::default`]): slow nodes, origin error
/// bursts, a site cut, and in `full-cold-start` a storm on object 0 by every
/// node at three quarters of the run.
pub fn combined(name: &str, nodes: u32, rate: f64, secs: Option<u64>) -> Scenario {
    combined_with(name, nodes, rate, secs, &DataFaults::default())
}

/// Data-plane faults of the combined scenarios (`"combined_faults"` in the `--config` file).
#[derive(Clone, Copy, Debug, PartialEq)]
pub struct DataFaults {
    /// No data-plane faults at all when false.
    pub enabled: bool,
    /// Slow nodes per thousand (`full-lagging`, `full-cold-start`).
    pub slow_permille: u32,
    /// Slowdown factor of a slow node.
    pub slow_factor: u8,
    /// Origin 5xx probability during a burst (`full-failover`).
    pub p5xx: f64,
    /// Origin 412 probability during a burst (`full-churn`).
    pub p412: f64,
    /// Length of each origin error burst.
    pub burst_ns: Nanos,
    /// Sites 0 and 1 are cut apart (`full-failover`).
    pub partition: bool,
}

impl Default for DataFaults {
    /// Every fault on, at the default rates.
    fn default() -> Self {
        Self {
            enabled: true,
            slow_permille: 10,
            slow_factor: 8,
            p5xx: 0.3,
            p412: 0.1,
            burst_ns: 30 * SECONDS,
            partition: true,
        }
    }
}

impl DataFaults {
    /// Apply a `"combined_faults"` JSON object.
    pub fn apply_json(&mut self, value: &serde_json::Value) -> Result<(), String> {
        let obj = value
            .as_object()
            .ok_or_else(|| format!("combined_faults: expected an object, got {value}"))?;
        for (key, v) in obj {
            let bad = || format!("combined_faults.{key}: invalid value {v}");
            let probability = || {
                v.as_f64()
                    .filter(|p| (0.0..=1.0).contains(p))
                    .ok_or_else(bad)
            };
            match key.as_str() {
                "enabled" => self.enabled = v.as_bool().ok_or_else(bad)?,
                "partition" => self.partition = v.as_bool().ok_or_else(bad)?,
                "slow_permille" => {
                    self.slow_permille = v
                        .as_u64()
                        .filter(|n| *n <= 1000)
                        .and_then(|n| u32::try_from(n).ok())
                        .ok_or_else(bad)?;
                }
                "slow_factor" => {
                    self.slow_factor = v
                        .as_u64()
                        .and_then(|n| u8::try_from(n).ok())
                        .filter(|n| *n >= 1)
                        .ok_or_else(bad)?;
                }
                "p5xx" => self.p5xx = probability()?,
                "p412" => self.p412 = probability()?,
                "burst_ns" => self.burst_ns = v.as_u64().ok_or_else(bad)?,
                _ => return Err(format!("unknown key combined_faults.{key}")),
            }
        }
        Ok(())
    }
}

/// [`combined`] with the data-plane faults `faults`.
pub fn combined_with(
    name: &str,
    nodes: u32,
    rate: f64,
    secs: Option<u64>,
    faults: &DataFaults,
) -> Scenario {
    let (plan, default) = match name {
        "full-cold-start" => ("enrollment-storm", 400),
        "full-failover" => ("failover", 900),
        "full-churn" => ("ctl-churn", 1200),
        _ => ("lagging", 900),
    };
    let d = secs.unwrap_or(default) * SECONDS;
    let mut spec = ModelSpec {
        control: Some(ControlSpec {
            plan,
            overrides: None,
            combined: true,
        }),
        workload: Some(Workload {
            objects: 4096,
            zipf_s: 1.0,
            rate,
            range_fraction: 0.1,
            range_pages: 8,
        }),
        ..ModelSpec::default()
    };
    let mut slow = Vec::new();
    // `DataFaults::slow`: every `1000 / slow_permille`-th node (by default every 100th), slowed
    // by `slow_factor` (8).
    let mut slowed = |from: Nanos, until: Nanos| {
        if !faults.enabled || faults.slow_permille == 0 || faults.slow_factor <= 1 {
            return;
        }
        let stride = (1000 / faults.slow_permille).max(1);
        for node in (stride / 2..nodes).step_by(stride as usize) {
            let factor = faults.slow_factor;
            slow.push((from, Fault::SlowPeer { node, factor }));
            slow.push((until, Fault::SlowPeer { node, factor: 1 }));
        }
    };
    let burst = |from: Nanos, errors: OriginErrors| OriginBurst {
        from,
        until: from + faults.burst_ns,
        errors,
    };
    // `DataFaults::origin`: no burst without errors or length.
    let bursts = |at: &[Nanos], errors: OriginErrors| {
        if !faults.enabled || errors.p412 + errors.p5xx <= 0.0 || faults.burst_ns == 0 {
            return Vec::new();
        }
        at.iter().map(|from| burst(*from, errors)).collect()
    };
    match name {
        "full-cold-start" => {
            slowed(d * 3 / 4 - d / 40, d * 3 / 4 + d / 20);
            // `cold_start_window_ns`: twice the owner egress the storm needs.
            let egress = u128::from(nodes) * u128::from(spec.page_bytes) * u128::from(SECONDS)
                / (topology::REPLICAS as u128 * 1_250_000_000);
            let window = (2 * egress).clamp(u128::from(SECONDS), u128::from(Nanos::MAX)) as Nanos;
            spec.storms.push(storm(d * 3 / 4, 0, nodes, window));
        }
        "full-failover" => {
            if faults.enabled && faults.partition {
                spec.site_cuts.push(SiteCut {
                    from: d * 2 / 5,
                    until: d / 2,
                    a: 0,
                    b: 1,
                });
            }
            let errors = OriginErrors {
                p5xx: faults.p5xx,
                ..OriginErrors::default()
            };
            spec.origin_bursts = bursts(&[d / 5, d * 3 / 5], errors);
        }
        "full-churn" => {
            let errors = OriginErrors {
                p412: faults.p412,
                ..OriginErrors::default()
            };
            spec.origin_bursts = bursts(&[d / 2], errors);
        }
        _ => slowed(d / 10, d * 9 / 10),
    }
    spec = spec
        .check("everyone-enrolled", |r| {
            let c = &r["combined"]["control"];
            (c["enrolled"] == c["live_clients"] && c["enrolled"].as_u64() > Some(0))
                .then_some(())
                .ok_or(format!(
                    "enrolled {} of {}",
                    c["enrolled"], c["live_clients"]
                ))
        })
        .check("reads-complete", |r| {
            (counter(r, "reads_completed") > 0 && counter(r, "membership_publications") > 0)
                .then_some(())
                .ok_or(format!("counters {}", r["metrics"]["counters"]))
        });
    // A 50,000-node run takes on the order of 10^9 read-path events.
    let mut scenario = Scenario::new(format!("model-{name}-{nodes}"))
        .nodes(nodes)
        .sites(4)
        .model(spec)
        .end(d)
        .max_steps(u64::MAX);
    for (at, fault) in slow {
        scenario = scenario.fault(at, fault);
    }
    scenario
}

/// Every built-in scenario.
pub fn catalog() -> Vec<Scenario> {
    vec![
        // Small sizes so the debug CI run stays fast; `tests/control.rs` can run larger ones.
        combined("full-cold-start", 200, 0.05, Some(120)),
        combined("full-failover", 120, 0.05, Some(300)),
        combined("full-churn", 160, 0.02, Some(400)),
        combined("full-lagging", 150, 0.05, Some(300)),
        control("enrollment-storm", 200),
        control("failover", 200),
        control("ctl-churn", 320),
        control("lagging", 200),
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
        slow_origin(),
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
            .expect_quiescent()
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
            .expect_quiescent()
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
    catalog()
        .into_iter()
        .chain(crate::app::catalog())
        .find(|s| s.name == name)
}
