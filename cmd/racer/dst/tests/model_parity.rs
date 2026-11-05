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
    run(scenarios::origin_forbidden(), 11);
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
