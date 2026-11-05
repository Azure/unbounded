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
fn cold_start_400() {
    run(scenarios::cold_start(400), 8);
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
    let scenario = scenarios::cold_start_with(nodes, attempt);
    let (outcome, report) = model::run(&scenario, Choices::seeded(8));
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
