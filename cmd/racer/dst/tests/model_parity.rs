//! Model ports of racer-sim scenarios: each runs with its racer-sim invariants (the scenario
//! checks) and prints its report; `dst/MODEL.md` compares the numbers with racer-sim.
use racer_dst::{Choices, Scenario, model, scenarios};

fn run(scenario: Scenario, seed: u64) -> serde_json::Value {
    let (outcome, report) = model::run(&scenario, Choices::seeded(seed));
    let mut brief = report.clone();
    brief["coverage"] = serde_json::Value::Null;
    println!("{brief:#}");
    assert_eq!(outcome.failure, None, "{}", report["metrics"]);
    assert!(outcome.quiescent);
    report
}

#[test]
fn origin_forbidden_fails_only_the_leader() {
    let report = run(scenarios::origin_forbidden(), 11);
    let counters = &report["metrics"]["counters"];
    // Each 403 fails only its leader and re-elects a waiter (racer-sim `flight_reelected`). A
    // leader whose walk fails otherwise (shed by every owner) re-elects a waiter too.
    let reelected = counters["flights_reelected"].as_u64().unwrap();
    let forbidden = counters["origin_403"].as_u64().unwrap();
    assert!(forbidden > 0 && reelected >= forbidden, "{counters}");
    assert_eq!(
        report["metrics"]["errors"]["OriginForbidden"].as_u64(),
        Some(forbidden),
        "{}",
        report["metrics"]
    );
}

#[test]
fn hot_page_storm_fans_out_through_helpers() {
    run(scenarios::hot_page_storm(512), 5);
}

#[test]
fn reads_recover_after_partition_heals() {
    let report = run(scenarios::partition_heals(), 6);
    let counters = &report["metrics"]["counters"];
    assert!(
        counters["link_failures"].as_u64().unwrap() > 0,
        "{counters}"
    );
}

#[test]
fn hung_peers_are_routed_around() {
    let report = run(scenarios::hung_peers(), 1);
    let counters = &report["metrics"]["counters"];
    assert!(
        counters["silence_pings"].as_u64().unwrap() > 0,
        "{counters}"
    );
}

#[test]
fn node_faults_end_every_read() {
    let report = run(scenarios::node_faults(), 2);
    let counters = &report["metrics"]["counters"];
    assert!(counters["crashes"].as_u64().unwrap() > 0, "{counters}");
    assert!(counters["replies_lost"].as_u64().unwrap() > 0, "{counters}");
}

#[test]
fn cold_start_400() {
    let report = run(scenarios::cold_start(400), 8);
    let counters = &report["metrics"]["counters"];
    // Probes cross the mesh: most owners are not neighbors, so requests are relayed, and every
    // relayed page crosses more than one link.
    let relays = counters["relays"].as_u64().unwrap();
    assert!(relays > 0, "{counters}");
    let (bytes, pages) = (
        counters["peer_bytes"].as_u64().unwrap(),
        counters["peer_pages"].as_u64().unwrap(),
    );
    assert!(
        bytes > pages * model::ModelSpec::default().page_bytes,
        "{counters}"
    );
}

/// Full-size cold start; a few seconds in release builds (`cargo test --release`).
#[test]
#[ignore = "large: run with --release --ignored"]
fn cold_start_2000() {
    run(scenarios::cold_start(2000), 8);
}

#[test]
fn range_readers_deliver_every_page() {
    let report = run(scenarios::range_readers(), 3);
    println!("{:#}", report["coverage"]);
}

#[test]
fn metadata_readers_coalesce_refreshes() {
    let report = run(scenarios::metadata_readers(), 3);
    println!("{:#}", report["coverage"]);
}

#[test]
fn faults_end_every_read() {
    let report = run(
        scenarios::faults(),
        std::env::var("SEED").map_or(3, |s| s.parse().unwrap()),
    );
    println!("{:#}", report["coverage"]);
}

/// Peak RSS per node of a Model cold start at `MODEL_NODES` nodes (default 10,000); compare with
/// racer-sim's ~103 KB/node at 5x10^4 nodes. Run alone:
/// `MODEL_NODES=50000 cargo test --release -p racer-dst --test model_parity model_memory -- --ignored --nocapture`.
#[test]
#[ignore = "large: run alone with --release --ignored"]
fn model_memory_per_node() {
    let nodes: u32 = std::env::var("MODEL_NODES").map_or(10_000, |n| n.parse().unwrap());
    // `MODEL_ATTEMPT_MS`: per-attempt cap; unset is production's `model::ATTEMPT_TIMEOUT`, 0 is
    // no cap.
    let attempt = std::env::var("MODEL_ATTEMPT_MS").map_or(model::ATTEMPT_TIMEOUT, |ms| {
        match ms.parse::<u64>().unwrap() {
            0 => model::NO_ATTEMPT_CAP,
            ms => ms * 1_000_000,
        }
    });
    let mut scenario = scenarios::cold_start_with(nodes, attempt);
    // `MODEL_CRYPTO_QUEUE`, `MODEL_RELAY_TRANSFERS`: override the production defaults.
    if let Some(spec) = scenario.model.as_mut() {
        let spec = std::rc::Rc::get_mut(spec).unwrap();
        if let Ok(q) = std::env::var("MODEL_CRYPTO_QUEUE") {
            spec.crypto_queue = q.parse().unwrap();
        }
        if let Ok(r) = std::env::var("MODEL_RELAY_TRANSFERS") {
            spec.relay_transfers = r.parse().unwrap();
        }
        // `MODEL_CONNECTIONS`: `off` holds no connection slots; `legacy` is the listener
        // before peer ingress was sized apart and refused when full (45 slots per worker, accept
        // queue); `evict` adds idle eviction to production. Unset is production.
        if let Ok(mode) = std::env::var("MODEL_CONNECTIONS") {
            let mut config = model::ConnectionConfig::production(spec.workers);
            config.evict_idle = mode.contains("evict");
            if mode.contains("legacy") {
                config = model::ConnectionConfig::legacy(spec.workers);
            }
            spec.connections = if mode == "off" {
                model::PeerConnections::Off
            } else {
                model::PeerConnections::Custom(config)
            };
        }
    }
    // `MODEL_SEED`: the cold start's seed, 8 by default.
    let seed = std::env::var("MODEL_SEED").map_or(8, |s| s.parse().unwrap());
    let (outcome, report) = model::run(&scenario, Choices::seeded(seed));
    let status = std::fs::read_to_string("/proc/self/status").unwrap();
    let hwm_kib: u64 = status
        .lines()
        .find_map(|l| l.strip_prefix("VmHWM:"))
        .and_then(|v| v.trim().trim_end_matches("kB").trim().parse().ok())
        .unwrap();
    println!(
        "nodes {nodes} peak_rss {} B per_node {} B steps {} failure {:?} counters {} latency {} attempt_errors {}",
        hwm_kib * 1024,
        hwm_kib * 1024 / u64::from(nodes),
        outcome.steps,
        outcome.failure.as_ref().map(|f| &f.name),
        report["metrics"]["counters"],
        report["metrics"]["histograms"]["read_latency_ns"],
        report["metrics"]["attempt_errors"]
    );
}

/// One line per parity scenario with the numbers `dst/MODEL.md` tabulates.
#[test]
#[ignore = "summary: run with --release --ignored --nocapture"]
fn parity_summary() {
    let cases = [
        (scenarios::concurrent_cold_readers(), 2),
        (scenarios::cold_start(400), 8),
        (scenarios::cold_start(2000), 8),
        (scenarios::hot_page_storm(512), 5),
        (scenarios::origin_forbidden(), 11),
    ];
    for (scenario, seed) in cases {
        let (outcome, r) = model::run(&scenario, Choices::seeded(seed));
        let c = &r["metrics"]["counters"];
        let l = &r["metrics"]["histograms"]["read_latency_ns"];
        let ms = |k: &str| l[k].as_u64().unwrap_or(0) / 1_000_000;
        println!(
            "{} failure={:?} completed={} failed={} errors={} origin={} per_page={} joins={} \
             reelected={} sheds={} helper_probes={} helper_copies={} retries={} latency={}/{}/{}",
            scenario.name,
            outcome.failure.as_ref().map(|f| &f.name),
            c["reads_completed"],
            c["reads_failed"],
            r["metrics"]["errors"],
            c["origin_requests"],
            r["origin_per_page"]["max_requests_per_page"],
            c["flight_joins"],
            c["flights_reelected"],
            c["sheds"],
            c["helper_probes"],
            c["helper_copies"],
            c["read_retries"],
            ms("p50"),
            ms("p99"),
            ms("max"),
        );
    }
}

/// Diagnostic report of one scenario (`MODEL_SCENARIO`, `MODEL_SEED`).
#[test]
#[ignore = "diagnostic: run with --ignored --nocapture"]
fn scenario_report() {
    let name = std::env::var("MODEL_SCENARIO").unwrap_or_else(|_| "model-cold-start-400".into());
    let seed = std::env::var("MODEL_SEED").map_or(1, |s| s.parse().unwrap());
    let scenario = scenarios::find(&name).expect("known scenario");
    let (outcome, r) = model::run(&scenario, Choices::seeded(seed));
    println!(
        "failure {:?} max_per_page {} counters {} attempt_errors {}",
        outcome.failure.as_ref().map(|f| &f.name),
        r["origin_per_page"]["max_requests_per_page"],
        r["metrics"]["counters"],
        r["metrics"]["attempt_errors"]
    );
}

#[test]
fn membership_churn_refreshes_falls_back_and_waits() {
    let report = run(scenarios::membership_churn(), 1);
    let counters = &report["metrics"]["counters"];
    let n = |k: &str| counters[k].as_u64().unwrap();
    // Lagging destinations answer `StaleMembership`; the requester gets the unused links back,
    // and readers that cannot fall back to the previous membership wait.
    assert!(n("stale_membership_replies") > 0, "{counters}");
    assert!(n("stale_links_refunded") > 0, "{counters}");
    assert!(n("stale_waits") > 0, "{counters}");
    assert!(n("reads_skipped_not_member") > 0, "{counters}");
    let coverage = &report["coverage"];
    for name in ["Acquisition::Refresh", "Acquisition::Sleep"] {
        assert!(
            coverage[name].as_u64().unwrap_or(0) > 0,
            "{name}: {coverage}"
        );
    }
}

/// Peer connection slots on the 400-node cold start (`racer_core::connections`). With the
/// legacy limits (`ConnectionConfig::legacy`: 45 ingress slots per worker, a full listener
/// leaves connections in its accept queue) hot nodes' ingress slots stay held by idle
/// connections their peers keep pooled (30 s on both sides), so later connections wait in the
/// accept queue; evicting the longest-idle inbound connection cuts the waits that outlive their
/// attempt and never fails more reads. Production (2,048 peer slots per worker, refusal when full)
/// never queues. See `dst/MODEL.md`, "Peer connection slots".
#[test]
fn idle_connections_pin_ingress_slots() {
    let counters = |connections: fn(u32) -> model::ConnectionConfig, evict: bool| {
        let mut scenario = scenarios::cold_start(400);
        let spec = std::rc::Rc::get_mut(scenario.model.as_mut().unwrap()).unwrap();
        let mut config = connections(spec.workers);
        config.evict_idle = evict;
        spec.connections = model::PeerConnections::Custom(config);
        let (_, report) = model::run(&scenario, Choices::seeded(8));
        report["metrics"]["counters"].clone()
    };
    let count = |c: &serde_json::Value, k: &str| c[k].as_u64().unwrap();
    let legacy = counters(model::ConnectionConfig::legacy, false);
    let evicting = counters(model::ConnectionConfig::legacy, true);
    let production = counters(model::ConnectionConfig::production, false);
    assert!(count(&legacy, "conn_queued") > 0, "{legacy}");
    assert!(count(&legacy, "conn_reused") > 0, "{legacy}");
    assert_eq!(count(&legacy, "conn_idle_evicted"), 0, "{legacy}");
    assert!(count(&evicting, "conn_idle_evicted") > 0, "{evicting}");
    // Waits that outlive the requester's attempt, and the reads they fail.
    assert!(
        count(&evicting, "conn_queue_abandoned") * 2 < count(&legacy, "conn_queue_abandoned"),
        "{legacy} {evicting}"
    );
    assert!(
        count(&evicting, "reads_failed") <= count(&legacy, "reads_failed"),
        "{legacy} {evicting}"
    );
    for key in ["conn_queued", "conn_dropped", "conn_idle_evicted"] {
        assert_eq!(count(&production, key), 0, "{key}: {production}");
    }
    assert!(count(&production, "conn_reused") > 0, "{production}");
    assert!(
        count(&production, "reads_failed") <= count(&legacy, "reads_failed"),
        "{legacy} {production}"
    );
}

#[test]
fn range_deadline_discards_held_pages() {
    let report = run(scenarios::range_deadline(), 1);
    assert!(
        report["coverage"].to_string().contains("Window::Discard"),
        "{}",
        report["coverage"]
    );
}

/// Gap closed by link circuits (`dst/MODEL.md`, Combined scenarios, Parity): `full-failover`
/// cuts sites 0 and 1. Production opens a worker's circuit to a peer whose connect failed
/// (`src/peer.rs` `request_network`, `src/topology.rs` `LinkHealth`) and routes around it
/// (`Paths::blocked`). Without circuits the Model failed 86 reads at seed 8 against racer-sim's
/// 12; with them it fails 13.
#[test]
fn site_cut_opens_link_circuits_and_routes_around() {
    let measure = |circuits: bool| {
        let mut scenario = scenarios::combined("full-failover", 200, 0.2, Some(300));
        let spec = std::rc::Rc::get_mut(scenario.model.as_mut().unwrap()).unwrap();
        spec.circuits = circuits;
        let (outcome, report) = model::run(&scenario, Choices::seeded(8));
        assert_eq!(outcome.failure, None, "{}", report["metrics"]);
        let counters = report["metrics"]["counters"].clone();
        let n = |k: &str| counters[k].as_u64().unwrap();
        (n("reads_failed"), n("link_failures"))
    };
    let (failed, links) = measure(true);
    let (failed_off, links_off) = measure(false);
    assert!(
        links < links_off,
        "{links} links failed, {links_off} without circuits"
    );
    assert!(
        failed < failed_off,
        "{failed} reads failed, {failed_off} without circuits"
    );
    // racer-sim fails 12 reads at this seed; the scale tolerance is 2x.
    assert!(failed <= 24, "{failed} reads failed");
}

/// A storm-only Model scenario: `storms` on `nodes` nodes until `end`.
fn storms(name: &str, nodes: u32, end: u64, spec: model::ModelSpec) -> Scenario {
    Scenario::new(name)
        .nodes(nodes)
        .model(spec)
        .end(end * racer_dst::engine::SECONDS)
}

fn storm(at: u64, object: u64, nodes: u32, window: u64) -> model::Storm {
    model::Storm {
        at,
        object,
        first: 0,
        nodes,
        window,
        range: None,
    }
}

fn counter(r: &serde_json::Value, k: &str) -> u64 {
    r["metrics"]["counters"][k].as_u64().unwrap_or(0)
}

/// racer-sim `single_reader_then_storm_reuses_copies` (seed 7): one reader fetches an object,
/// then every node reads it; the origin sees each page once.
#[test]
fn single_reader_then_storm_reuses_copies() {
    use racer_dst::engine::{MILLIS, SECONDS};
    let spec = model::ModelSpec {
        storms: vec![storm(0, 3, 1, 1), storm(SECONDS, 3, 64, 100 * MILLIS)],
        ..model::ModelSpec::default()
    };
    let pages = u64::from(spec.pages_per_object);
    let r = run(storms("single-reader-then-storm", 64, 3, spec), 7);
    assert_eq!(counter(&r, "reads_completed"), pages * 65);
    assert_eq!(counter(&r, "origin_requests"), pages);
    assert!(counter(&r, "peer_bytes") > 0);
    assert!(counter(&r, "flight_joins") + counter(&r, "local_hits") > 0);
    assert!(counter(&r, "relays") + counter(&r, "peer_pages") > 0);
}

/// racer-sim `cold_storm_bounds_origin_per_page` (seed 3): a 300-node cold storm reaches the
/// origin at most `REPLICAS` times per page.
#[test]
fn cold_storm_bounds_origin_per_page() {
    use racer_dst::engine::MILLIS;
    let spec = model::ModelSpec {
        storms: vec![storm(0, 0, 300, 10 * MILLIS)],
        ..model::ModelSpec::default()
    };
    let r = run(storms("cold-storm", 300, 4, spec), 3);
    let max = r["origin_per_page"]["max_requests_per_page"]
        .as_u64()
        .unwrap();
    assert!(max <= topology::REPLICAS as u64, "{max}");
    assert_eq!(r["origin_per_page"]["pages"], 16);
    assert!(counter(&r, "flight_joins") > 0);
}

/// racer-sim `origin_errors_follow_the_fill_plan` (seed 4): 403 and 5xx fail the fill
/// (`fills_failed`), and 403 fails reads `OriginForbidden`. racer-sim's `remaining_walks`
/// (412 continuing the walk) has no Model counter.
#[test]
fn origin_errors_follow_the_fill_plan() {
    use racer_dst::{engine::SECONDS, resources::OriginErrors};
    let spec = model::ModelSpec {
        storms: (0..8)
            .map(|i| storm(i * SECONDS / 4, i, 32, SECONDS / 8))
            .collect(),
        origin_errors: OriginErrors {
            p412: 0.2,
            p403: 0.05,
            p5xx: 0.1,
        },
        ..model::ModelSpec::default()
    };
    let (outcome, r) = model::run(&storms("origin-errors", 32, 30, spec), Choices::seeded(4));
    assert!(outcome.quiescent, "{}", r["invariants"]);
    assert!(counter(&r, "origin_403") > 0);
    assert!(counter(&r, "origin_5xx") > 0);
    assert!(counter(&r, "origin_412") > 0);
    assert!(counter(&r, "fills_failed") > 0);
    assert!(r["metrics"]["errors"]["OriginForbidden"].as_u64() > Some(0));
}
