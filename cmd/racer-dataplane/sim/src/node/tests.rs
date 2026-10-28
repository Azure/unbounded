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

/// One reader fetches an object, then every node reads it.
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
    let per_page: Vec<u32> = (0..pages)
        .map(|p| world.origin_requests(3 * pages + p))
        .collect();
    // Before owners delegated to rank 0 this was 33.
    assert_eq!(origin, pages, "{per_page:?}");
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

/// Churn publishes new memberships, nodes adopt them with lag, placement maintenance runs, and
/// lagging receivers answer stale membership.
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

/// Without retained old versions, receivers that moved ahead answer stale membership to lagging
/// requesters, which refresh and walk again.
#[test]
fn stale_requesters_refresh_their_walk() {
    let (_, report) = run(churny(120, 0), 2);
    assert!(
        counter(&report, "walk_membership_refreshes") > 0,
        "{report}"
    );
}

/// Requesters that adopted a publication before their destination get `StaleMembership` with
/// nothing newer to refresh to.
#[test]
fn requesters_ahead_of_their_destination_skip_it() {
    for (retained, seed) in [(0, 2), (2, 1)] {
        let (_, report) = run(churny(120, retained), seed);
        assert!(counter(&report, "stale_destination_skips") > 0, "{report}");
        assert_eq!(
            counter(&report, "error_incompatible_membership"),
            0,
            "{report}"
        );
        assert_eq!(
            counter(&report, "error_hop_budget_exhausted"),
            0,
            "{report}"
        );
    }
}

/// With a long adoption lag, requesters often adopt a version before the page's owners.
#[test]
fn stale_owners_send_non_owners_to_the_previous_membership() {
    let with = |fallback: bool, refund: bool| {
        let mut config = churny(120, 2);
        config.request_rate = 40.0;
        config.node.membership_lag_ns = config.node.membership_batch_ns;
        config.node.stale_fallback = fallback;
        // Measure the fallback alone: without it, readers would wait for the stale owners instead.
        config.node.stale_wait = false;
        // The links a stale answer returns let a walk reach the other owners even without the
        // fallback, so most of the failed reads it saved are measured with that refund off.
        config.node.stale_link_refund = refund;
        run(config, 3).1
    };
    let off = with(false, false);
    let on = with(true, false);
    assert_eq!(counter(&off, "stale_fallbacks"), 0);
    assert!(counter(&on, "stale_fallbacks") > 0, "{on}");
    let exhausted = |r: &Value| counter(r, "walk_exhausted_stale");
    assert!(exhausted(&off) > 0, "{off}");
    assert!(
        exhausted(&on) * 10 <= exhausted(&off),
        "{} vs {}",
        exhausted(&on),
        exhausted(&off)
    );
    // Seed 3, refund off: 1,028 such walks and 40 failed reads without the fallback, 6 and 3 with
    // it.
    assert!(counter(&on, "reads_failed") * 4 <= counter(&off, "reads_failed"));
    // With the refund (the default) the fallback still ends far fewer walks exhausted (597 to 1)
    // and fails no more reads (2 and 2).
    let (off, on) = (with(false, true), with(true, true));
    assert!(
        exhausted(&on) * 10 <= exhausted(&off),
        "{} vs {}",
        exhausted(&on),
        exhausted(&off)
    );
    assert!(counter(&on, "reads_failed") <= counter(&off, "reads_failed"));
}

/// A node that rejoins in version N is not a member of N - 1, so it cannot walk the previous
/// membership when the page's owners are behind.
#[test]
fn readers_that_cannot_fall_back_wait_for_owners_that_are_behind() {
    let with = |wait: bool| {
        let mut config = churny(120, 2);
        config.request_rate = 40.0;
        config.node.membership_lag_ns = config.node.membership_batch_ns;
        config.node.stale_wait = wait;
        run(config, 3).1
    };
    let off = with(false);
    let on = with(true);
    assert_eq!(counter(&off, "stale_waits"), 0);
    assert!(counter(&on, "stale_waits") > 0, "{on}");
    let exhausted = |r: &Value| counter(r, "walk_exhausted_stale");
    eprintln!(
        "exhausted {} -> {}, failed {} -> {}, waits {}, not member {}",
        exhausted(&off),
        exhausted(&on),
        counter(&off, "reads_failed"),
        counter(&on, "reads_failed"),
        counter(&on, "stale_waits"),
        counter(&off, "stale_fallback_not_member"),
    );
    assert!(counter(&off, "stale_fallback_not_member") > 0, "{off}");
    assert!(exhausted(&off) > 0, "{off}");
    assert!(exhausted(&on) < exhausted(&off));
    assert!(counter(&on, "reads_failed") <= counter(&off, "reads_failed"));
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

/// A 403 fails only the leader that received it; a waiter on the same flight is re-elected and
/// fills with its own budget (matching production `Flights::fail(OriginForbidden)`).
#[test]
fn origin_forbidden_reelects_a_waiter() {
    let mut config = quiet(64);
    config.storms = vec![Storm {
        at_ns: 0,
        object: 1,
        nodes: 64,
        window_ns: MILLIS,
    }];
    config.origin_errors = OriginErrors {
        p412: 0.0,
        p403: 0.5,
        p5xx: 0.0,
    };
    let (_, report) = run(config, 11);
    assert!(counter(&report, "origin_403") > 0);
    assert!(counter(&report, "flight_reelected") > 0);
    assert_eq!(counter(&report, "flight_unexpected_lead"), 0);
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
    // The flight table's own entry cap rejects before the Flight quota, as in production
    // `Flights::join_prepared`; relays hit the Relay quota.
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

/// Retention observes every client read, including memory hits: a non-owner persists a page on its
/// second read (second sight), not at publication.
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

/// Rankings and route searches yield between CPU slices and hold their placement pin or search slot
/// meanwhile, so production's concurrency limits reject concurrent work with `Overloaded` (without
/// a search queue).
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

/// With the production search queue, searches beyond the slot limit wait (parked, no CPU) and are
/// woken when a slot frees, so the same storm rejects far fewer searches, and none at the default
/// limits.
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

/// Runs the hedge scenario: every ranked owner of object 4 warms its pages, then each rank 0 owner
/// becomes 255 times slower and non-owner readers (never slow) read every page.
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

/// Production default (no slots) never races; with slots, a slow rank 0 owner loses to the
/// copy-only duplicate at the second owner: lower peer read latency, duplicate bytes counted per
/// started hedge, and the split budgets never grow (checked by `budget_never_grows`).
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

/// A duplicate publish of a resident page does not refresh its recency, as in production
/// `MemoryCache::publish`: inserting 0, 1, 0, 2 into a two-page worker cache evicts 0, the oldest
/// insert.
#[test]
fn duplicate_memory_publish_does_not_touch() {
    let mut config = quiet(4);
    config.memory_pages = 2 * u32::from(config.workers);
    let mut world = NodeWorld::new(config, 1);
    for page in [0, 1, 0, 2] {
        world.memory_insert(0, 0, page);
    }
    assert!(!world.in_memory(0, 0, 0));
    assert!(world.in_memory(0, 0, 1));
    assert!(world.in_memory(0, 0, 2));
}

/// Ownership moves when nodes join: an owner that adopted the new membership asks the page's
/// previous owners copy-only before origin, so a page read before the join is not fetched again
/// after it.
#[test]
fn joined_owners_copy_from_previous_owners() {
    let mut config = quiet(64);
    config.duration_ns = 4 * SECONDS;
    let mut churn = Vec::new();
    for node in 48..64 {
        churn.push(Churn {
            at_ns: MILLIS,
            node,
            join: false,
        });
        churn.push(Churn {
            at_ns: SECONDS + SECONDS / 2,
            node,
            join: true,
        });
    }
    config.churn = churn;
    config.storms = vec![
        Storm {
            at_ns: SECONDS,
            object: 3,
            nodes: 1,
            window_ns: 1,
        },
        Storm {
            at_ns: 3 * SECONDS,
            object: 3,
            nodes: 48,
            window_ns: 100 * MILLIS,
        },
    ];
    let (world, report) = run(config, 7);
    let pages = u64::from(world.config.pages_per_object);
    let per_page: Vec<u32> = (0..pages)
        .map(|p| world.origin_requests(3 * pages + p))
        .collect();
    // Without handoff probes this was 19 (three pages fetched twice).
    assert!(counter(&report, "handoff_probes") > 0, "{report}");
    assert_eq!(counter(&report, "origin_requests"), pages, "{per_page:?}");
    assert_eq!(all_repeats(&report), 0, "{}", report["origin_per_page"]);
}

fn repeats(report: &Value, kind: &str) -> u64 {
    report["origin_per_page"]["repeats"][kind].as_u64().unwrap()
}

/// Repeated origin requests over every class (`NodeWorld::classify_repeat`).
fn all_repeats(report: &Value) -> u64 {
    [
        "after_owner_move",
        "after_failure",
        "concurrent",
        "copy_lost",
        "copy_held",
    ]
    .iter()
    .map(|kind| repeats(report, kind))
    .sum()
}

/// `copy_held` repeats with cause `cause` (`origin_repeats_held_*`).
fn held(report: &Value, cause: &str) -> u64 {
    report["origin_per_page"]["repeats"]["held"][cause]
        .as_u64()
        .unwrap()
}

/// Under churn, an owner whose predecessor has not adopted the current membership yet used to get
/// `StaleMembership`, record it as missing, and fetch the page itself while the predecessor
/// (usually rank 0) held it.
#[test]
fn owners_ask_lagging_predecessors_under_the_previous_membership() {
    for (nodes, seed) in [(120, 1), (120, 2), (300, 1)] {
        let (_, report) = run(churny(nodes, 2), seed);
        assert!(
            counter(&report, "previous_membership_probes") > 0,
            "{report}"
        );
        assert!(
            counter(&report, "owner_stale_destination_skips") <= 4,
            "{nodes} {seed}: {}",
            report["metrics"]["counters"]
        );
        assert!(
            repeats(&report, "copy_held") <= 8,
            "{nodes} {seed}: {}",
            report["origin_per_page"]
        );
    }
    // Without retained versions peers cannot resolve the previous membership, so owners never use
    // it (production: no grace retention).
    let (_, report) = run(churny(120, 0), 2);
    assert_eq!(counter(&report, "previous_membership_probes"), 0);
}

/// Repeated origin fetches of a page are bounded.
#[test]
fn repeat_origin_fetches_are_classified_and_bounded() {
    for (nodes, seed) in [(120, 1), (300, 1), (300, 2)] {
        let (_, report) = run(churny(nodes, 2), seed);
        let origin = counter(&report, "origin_requests");
        let pages = report["origin_per_page"]["pages"].as_u64().unwrap();
        let held = repeats(&report, "copy_held");
        assert_eq!(
            all_repeats(&report),
            origin - pages,
            "{}",
            report["origin_per_page"]
        );
        let causes: u64 = [
            "raced",
            "unreachable",
            "busy",
            "unfunded",
            "missed",
            "unasked",
        ]
        .iter()
        .map(|cause| self::held(&report, cause))
        .sum();
        assert_eq!(causes, held, "{}", report["origin_per_page"]);
        assert!(
            held * 50 <= origin,
            "{nodes} {seed}: {}",
            report["origin_per_page"]
        );
    }
}

/// With origin slower than the read deadline, leaders expire while their fills wait on origin.
#[test]
fn expired_leaders_drain_before_a_new_fill() {
    let mut config = quiet(64);
    // Without handover every expired leader's fill drains or is canceled.
    config.node.flight_handover = false;
    config.origin_bps = 1 << 20;
    config.duration_ns = 12 * SECONDS;
    config.storms = (0..4)
        .map(|i| Storm {
            at_ns: i * 3 * SECONDS,
            object: 1,
            nodes: 64,
            window_ns: 100 * MILLIS,
        })
        .collect();
    let (_, report) = run(config, 5);
    assert!(counter(&report, "flight_stale_finishes") > 0, "{report}");
    assert_eq!(counter(&report, "flight_unexpected_lead"), 0);
}

/// Production cancels an expired fill's origin exchange at the leader's deadline, which frees the
/// origin connection and bandwidth.
#[test]
fn expired_fills_cancel_their_origin_request() {
    let mut config = quiet(64);
    // Without handover every expired leader's fill drains or is canceled.
    config.node.flight_handover = false;
    config.origin_bps = 1 << 20;
    config.duration_ns = 12 * SECONDS;
    config.storms = (0..4)
        .map(|i| Storm {
            at_ns: i * 3 * SECONDS,
            object: 1,
            nodes: 64,
            window_ns: 100 * MILLIS,
        })
        .collect();
    let (_, report) = run(config, 5);
    assert!(counter(&report, "flight_stale_finishes") > 0, "{report}");
    assert!(counter(&report, "origin_canceled") > 0, "{report}");
    assert_eq!(counter(&report, "flight_unexpected_lead"), 0);
}

/// With origin slower than the read deadline and readers arriving throughout, an expired leader's
/// fill continues for a later waiter instead of being canceled and restarted.
#[test]
fn expired_leaders_hand_their_fill_to_later_waiters() {
    let report = |handover: bool| {
        let mut config = quiet(64);
        config.origin_bps = 1 << 20;
        config.duration_ns = 16 * SECONDS;
        config.node.flight_handover = handover;
        config.storms = (0..12)
            .map(|i| Storm {
                at_ns: i * SECONDS,
                object: 1,
                nodes: 16,
                window_ns: 500 * MILLIS,
            })
            .collect();
        run(config, 7).1
    };
    let (before, after) = (report(false), report(true));
    assert_eq!(counter(&before, "flight_handovers"), 0);
    assert!(counter(&after, "flight_handovers") > 0, "{after}");
    assert!(
        counter(&after, "origin_canceled") < counter(&before, "origin_canceled"),
        "before {} after {}",
        before["metrics"]["counters"],
        after["metrics"]["counters"]
    );
    assert!(
        counter(&after, "origin_requests") < counter(&before, "origin_requests"),
        "before {} after {}",
        before["metrics"]["counters"],
        after["metrics"]["counters"]
    );
    let max = |r: &Value| {
        r["origin_per_page"]["max_requests_per_page"]
            .as_u64()
            .unwrap()
    };
    assert!(
        max(&after) <= max(&before),
        "{} {}",
        max(&before),
        max(&after)
    );
    assert_eq!(counter(&after, "flight_unexpected_lead"), 0);
}

/// A reader that owned the page under the previous membership may not walk it (no fallback), and
/// ranks it there before it skips a destination that is behind, so it waits for that destination
/// like a reader that joined in the current membership.
#[test]
fn previous_owners_wait_instead_of_skipping_owners_that_are_behind() {
    let mut config = churny(120, 2);
    config.request_rate = 40.0;
    config.node.membership_lag_ns = config.node.membership_batch_ns;
    let r = run(config, 3).1;
    assert!(counter(&r, "stale_fallback_denied") > 0, "{r}");
    assert_eq!(
        counter(&r, "stale_waits_denied"),
        counter(&r, "stale_fallback_denied"),
        "{r}"
    );
    assert_eq!(counter(&r, "walk_exhausted_stale"), 0, "{r}");
}

/// A verified `StaleMembership` returns the links past its answerer, so a reader that waited for,
/// or fell back past, a stale owner can still fund its later owners.
#[test]
fn stale_answers_return_unused_links() {
    let config = |refund: bool| {
        let mut config = churny(120, 2);
        config.request_rate = 40.0;
        config.node.membership_lag_ns = config.node.membership_batch_ns;
        config.node.stale_link_refund = refund;
        config
    };
    let on = run(config(true), 3).1;
    let off = run(config(false), 3).1;
    assert!(counter(&on, "stale_links_refunded") > 0, "{on}");
    assert_eq!(counter(&off, "stale_links_refunded"), 0, "{off}");
    assert!(counter(&on, "walk_exhausted") <= counter(&off, "walk_exhausted"));
    assert!(counter(&on, "reads_failed") <= counter(&off, "reads_failed"));
}
