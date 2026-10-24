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

fn client_read(node: u32, page: u64, at: crate::engine::Nanos) -> super::world::Event {
    super::world::Event::Read(super::world::ClientRead {
        node,
        incarnation: 0,
        page,
        start: at,
        attempt: 0,
        range: 0,
        slot: 0,
    })
}

/// Retention observes every client read, including memory hits: a non-owner
/// persists a page on its second read (second sight), not at publication.
#[test]
fn retention_observes_client_reads_including_memory_hits() {
    let config = quiet(64);
    let mut world = NodeWorld::new(config, 5);
    let pages = u64::from(world.config.pages_per_object);
    let reader = 9;
    let foreign: Vec<u64> = (3 * pages..4 * pages)
        .filter(|p| !world.owners(*p).contains(&reader))
        .collect();
    assert!(!foreign.is_empty());
    for p in &foreign {
        world.engine.at(0, client_read(reader, *p, 0));
    }
    world.run(SECONDS);
    let after_first = world.metrics.get("retention_observations");
    assert_eq!(after_first, foreign.len() as u64);
    for p in &foreign {
        assert!(
            !world.on_disk(reader, *p),
            "page {p} persisted on first sight"
        );
    }
    let hits = world.metrics.get("memory_hits");
    for p in &foreign {
        world.engine.at(SECONDS, client_read(reader, *p, SECONDS));
    }
    world.run(2 * SECONDS);
    assert_eq!(
        world.metrics.get("memory_hits") - hits,
        foreign.len() as u64
    );
    assert_eq!(
        world.metrics.get("retention_observations"),
        2 * foreign.len() as u64
    );
    for p in &foreign {
        assert!(
            world.on_disk(reader, *p),
            "page {p} not persisted on second sight"
        );
    }
    assert_eq!(world.invariants.to_json()["hard_failures"], 0);
}

/// Rankings and route searches yield between CPU slices and hold their
/// placement pin or search slot meanwhile, so production's concurrency
/// limits reject concurrent work with `Overloaded` (without a search queue).
#[test]
fn yielding_searches_hit_production_concurrency_limits() {
    let mut config = quiet(600);
    config.duration_ns = 2 * SECONDS;
    config.node.placement_entries = 2;
    config.node.active_searches = 1;
    config.node.search_queue = 0;
    config.storms = vec![Storm {
        at_ns: 0,
        object: 2,
        nodes: 600,
        window_ns: MILLIS,
    }];
    let (_, report) = run(config.clone(), 3);
    assert!(counter(&report, "ranking_overloaded") > 0, "{report}");
    assert!(counter(&report, "route_search_overloaded") > 0, "{report}");
    // 600 members take three 256-member chunks per cold ranking.
    assert!(counter(&report, "ranking_polls") > counter(&report, "fills"));
    // Default limits: the same storm sees no placement overload.
    let mut roomy = config;
    roomy.node.placement_entries = 256;
    roomy.node.active_searches = 8;
    let (_, report) = run(roomy, 3);
    assert_eq!(counter(&report, "ranking_overloaded"), 0, "{report}");
}

/// Known issue 10: with the production search queue, searches beyond the
/// slot limit wait (parked, no CPU) and are woken when a slot frees, so the
/// same storm rejects far fewer searches, and none at the default limits.
#[test]
fn queued_searches_wait_instead_of_failing() {
    let mut config = quiet(600);
    config.duration_ns = 2 * SECONDS;
    config.node.placement_entries = 2;
    config.node.active_searches = 1;
    config.storms = vec![Storm {
        at_ns: 0,
        object: 2,
        nodes: 600,
        window_ns: MILLIS,
    }];
    let mut rejecting = config.clone();
    rejecting.node.search_queue = 0;
    let (_, before) = run(rejecting, 3);
    let (world, after) = run(config.clone(), 3);
    let rejected = counter(&before, "route_search_overloaded");
    assert!(rejected > 0, "{before}");
    assert!(counter(&after, "route_search_queued") > 0, "{after}");
    assert!(counter(&after, "route_search_queue_wakes") > 0, "{after}");
    assert!(
        counter(&after, "route_search_overloaded") * 4 < rejected,
        "{after}"
    );
    assert_eq!(world.invariants.to_json()["hard_failures"], 0);
    let mut roomy = config;
    roomy.node.active_searches = 8;
    let (_, report) = run(roomy, 3);
    assert_eq!(counter(&report, "route_search_overloaded"), 0, "{report}");
}

/// Runs the hedge scenario: every ranked owner of object 4 warms its pages,
/// then each rank 0 owner becomes 255 times slower and non-owner readers
/// (never slow) read every page.
fn hedge_scenario(slots: u32) -> (NodeWorld, Value) {
    use crate::faults::Fault;
    let mut config = quiet(64);
    config.node.hedge_slots = slots;
    config.node.hedge_delay_ns = 5 * MILLIS;
    config.node.hedge_bytes = u64::from(slots.max(1)) * super::world::DUPLICATE_BYTES as u64;
    config.duration_ns = 8 * SECONDS;
    let probe = NodeWorld::new(config.clone(), 21);
    let pages = u64::from(probe.config.pages_per_object);
    let range: Vec<u64> = (4 * pages..5 * pages).collect();
    let owners: Vec<Vec<u32>> = range.iter().map(|p| probe.owners(*p)).collect();
    let slow: std::collections::BTreeSet<u32> = owners.iter().map(|o| o[0]).collect();
    for node in &slow {
        config.faults.push(Fault::Slow {
            node: *node,
            factor: 255,
            from_ns: SECONDS,
            until_ns: 3 * SECONDS,
        });
    }
    let mut world = NodeWorld::new(config, 21);
    for (p, o) in range.iter().zip(&owners) {
        for node in o {
            world.engine.at(0, client_read(*node, *p, 0));
        }
        let readers = (0..64u32)
            .filter(|n| !o.contains(n) && !slow.contains(n))
            .take(4);
        for (i, node) in readers.enumerate() {
            let at = SECONDS + 100 * MILLIS + i as u64 * MILLIS;
            world.engine.at(at, client_read(node, *p, at));
        }
    }
    world.run(8 * SECONDS);
    let report = world.report("hedge", 21);
    assert_eq!(
        report["invariants"]["hard_failures"], 0,
        "{}",
        report["invariants"]
    );
    (world, report)
}

/// Production default (no slots) never races; with slots, a slow rank 0
/// owner loses to the copy-only duplicate at the second owner: lower peer
/// read latency, duplicate bytes counted per started hedge, and the split
/// budgets never grow (checked by `budget_never_grows`).
#[test]
fn hedging_beats_a_slow_primary() {
    let (_, off) = hedge_scenario(0);
    let (_, on) = hedge_scenario(4);
    assert_eq!(counter(&off, "hedge_races"), 0);
    assert!(counter(&off, "hedge_eligible") > 0, "{off}");
    assert!(counter(&on, "hedge_races") > 0, "{on}");
    let started = counter(&on, "hedge_started");
    assert!(started > 0, "{on}");
    // The duplicate at the fast second owner wins most races.
    assert!(2 * counter(&on, "hedge_won") > started, "{on}");
    assert!(counter(&on, "hedge_losers_canceled") > 0, "{on}");
    assert_eq!(
        counter(&on, "hedge_duplicate_bytes"),
        started * super::world::DUPLICATE_BYTES as u64
    );
    assert_eq!(
        counter(&on, "reads_completed"),
        counter(&off, "reads_completed"),
        "on {}\noff {}",
        on["metrics"]["counters"],
        off["metrics"]["counters"]
    );
    let mean = |r: &Value| {
        r["metrics"]["histograms"]["read_ns"]["mean"]
            .as_u64()
            .unwrap()
    };
    assert!(
        mean(&on) < mean(&off),
        "on {} off {}",
        mean(&on),
        mean(&off)
    );
}
