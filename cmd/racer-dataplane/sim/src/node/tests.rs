//! Scenario tests for the real node logic at small scale.

use serde_json::Value;

use super::NodeWorld;
use crate::config::Config;
use crate::engine::{MILLIS, SECONDS};
use crate::resources::OriginErrors;
use crate::workload::{Churn, Storm};

fn run(config: Config, seed: u64) -> (NodeWorld, Value) {
    let mut world = NodeWorld::new(config, seed);
    let end = world.config.duration_ns;
    world.run(end);
    let report = world.report("test", seed);
    assert_eq!(
        report["invariants"]["hard_failures"], 0,
        "{}",
        report["invariants"]
    );
    (world, report)
}

fn counter(report: &Value, name: &str) -> u64 {
    report["metrics"]["counters"][name].as_u64().unwrap_or(0)
}

fn quiet(nodes: u32) -> Config {
    Config {
        nodes,
        request_rate: 0.0,
        duration_ns: 3 * SECONDS,
        ..Config::default()
    }
}

/// One reader fetches an object, then every node reads it. Each page reaches
/// the origin at most once per ranked owner: a reader probes owners in spread
/// order, a later owner fetches after its predecessors miss, and the rank 0
/// owner always fetches itself (`CandidateWalk` with `rank = Some(0)` ends in
/// `End::Origin` at once). See Known issues in the design doc.
#[test]
fn single_reader_then_storm_reuses_copies() {
    let mut config = quiet(64);
    config.storms = vec![
        Storm {
            at_ns: 0,
            object: 3,
            nodes: 1,
            window_ns: 1,
        },
        Storm {
            at_ns: SECONDS,
            object: 3,
            nodes: 64,
            window_ns: 100 * MILLIS,
        },
    ];
    let (world, report) = run(config, 7);
    let pages = u64::from(world.config.pages_per_object);
    assert_eq!(counter(&report, "reads_completed"), pages * 65);
    let origin = counter(&report, "origin_requests");
    assert!(
        (pages..=pages * topology::REPLICAS as u64).contains(&origin),
        "{origin}"
    );
    for p in 0..pages {
        let n = world.origin_requests(3 * pages + p);
        assert!(
            (1..=topology::REPLICAS as u32).contains(&n),
            "page {p}: {n}"
        );
    }
    assert!(counter(&report, "peer_bytes") > 0);
    assert!(counter(&report, "flight_joins") + counter(&report, "peer_memory_hits") > 0);
    assert!(
        report["metrics"]["histograms"]["hops"]["count"]
            .as_u64()
            .unwrap()
            > 0
    );
}

/// Cold-start storm: origin traffic per page is bounded by the replica count.
#[test]
fn cold_storm_bounds_origin_per_page() {
    let mut config = quiet(300);
    config.duration_ns = 4 * SECONDS;
    config.storms = vec![Storm {
        at_ns: 0,
        object: 0,
        nodes: 300,
        window_ns: 10 * MILLIS,
    }];
    let (_, report) = run(config, 3);
    let max = report["origin_per_page"]["max_requests_per_page"]
        .as_u64()
        .unwrap();
    assert!(max <= topology::REPLICAS as u64, "{max}");
    assert_eq!(report["origin_per_page"]["pages"], 16);
    assert!(counter(&report, "flight_joins") > 0);
}

fn churny(nodes: u32, retained: u64) -> Config {
    let mut config = Config {
        nodes,
        duration_ns: 2 * SECONDS,
        objects: 32,
        ..Config::default()
    };
    config.node.retained_versions = retained;
    config.churn = Churn::random(
        nodes,
        nodes / 8,
        config.duration_ns,
        SECONDS / 2,
        &mut racer_core::time::SplitMix64::new(5),
    );
    config
}

/// Churn publishes new memberships, nodes adopt them with lag, placement
/// maintenance runs, and lagging receivers answer stale membership.
#[test]
fn churn_exercises_membership_adoption() {
    let (world, report) = run(churny(120, 2), 1);
    assert!(counter(&report, "membership_publications") > 1);
    assert!(counter(&report, "membership_adoptions") > 120);
    assert!(counter(&report, "placement_maintenance_steps") > 0);
    assert!(counter(&report, "stale_membership_replies") > 0, "{report}");
    assert!(world.versions.latest().number > 2);
    assert!(
        report["membership"]["placement_memo_entries"]
            .as_u64()
            .unwrap()
            > 0
    );
}

/// Without retained old versions, receivers that moved ahead answer stale
/// membership to lagging requesters, which refresh and walk again.
#[test]
fn stale_requesters_refresh_their_walk() {
    let (_, report) = run(churny(120, 0), 2);
    assert!(
        counter(&report, "walk_membership_refreshes") > 0,
        "{report}"
    );
}

/// 412 continues with the remaining walk; 403 and 5xx fail the fill.
#[test]
fn origin_errors_follow_the_fill_plan() {
    let mut config = quiet(32);
    config.request_rate = 20.0;
    config.origin_errors = OriginErrors {
        p412: 0.2,
        p403: 0.05,
        p5xx: 0.1,
    };
    let (_, report) = run(config, 4);
    assert!(counter(&report, "remaining_walks") > 0);
    assert!(counter(&report, "origin_403") > 0);
    assert!(counter(&report, "origin_5xx") > 0);
    assert!(counter(&report, "fills_failed") > 0);
    assert!(counter(&report, "error_origin_forbidden") > 0);
}

/// Tight quotas reject work per resource class.
#[test]
fn tight_quotas_reject_by_class() {
    let mut config = quiet(100);
    config.node.relay_transfers = 1;
    config.node.flights = 1;
    config.storms = vec![Storm {
        at_ns: 0,
        object: 1,
        nodes: 100,
        window_ns: MILLIS,
    }];
    let (_, report) = run(config, 9);
    // The flight table's own entry cap rejects before the Flight quota, as
    // in production `Flights::join_prepared`; relays hit the Relay quota.
    let r = &report["rejections"];
    assert!(r["relay"].as_u64().unwrap() > 0, "{r}");
    assert!(counter(&report, "flight_join_rejected") > 0);
}

#[test]
fn same_seed_replays_and_seeds_differ() {
    let a = run(churny(60, 2), 11).1;
    let b = run(churny(60, 2), 11).1;
    assert_eq!(a["trace_hash"], b["trace_hash"]);
    assert_eq!(a["metrics"], b["metrics"]);
    let c = run(churny(60, 2), 12).1;
    assert_ne!(a["trace_hash"], c["trace_hash"]);
}

#[test]
fn memory_accounting_is_per_component() {
    let (_, report) = run(churny(100, 2), 1);
    let components = report["memory"]["components"].as_object().unwrap();
    for name in [
        "placement",
        "paths",
        "memory_lru",
        "disk_index",
        "flights",
        "quotas",
        "placement_memo",
        "route_memo",
    ] {
        assert!(components.contains_key(name), "{name}");
    }
    assert!(components["placement"].as_u64().unwrap() > 0);
    let per_node = report["memory"]["accounted_bytes_per_node"]
        .as_u64()
        .unwrap();
    assert!(per_node < 640 << 10, "{per_node}");
}
