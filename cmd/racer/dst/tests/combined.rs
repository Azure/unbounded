//! Ports of racer-sim's combined scenario tests (`sim/src/combined.rs` `tests`) to the Model's
//! combined scenarios (`scenarios::combined`); `dst/MODEL.md` (racer-sim inventory) lists what
//! is ported and what is not.
use racer_dst::{Choices, model, scenarios};
use serde_json::Value;

/// racer-sim `quick`: `scenario` at `nodes`, `secs` and `rate` reads per node per second.
fn quick(scenario: &str, nodes: u32, secs: u64, seed: u64, rate: f64) -> Value {
    let scenario = scenarios::combined(scenario, nodes, rate, Some(secs));
    let (outcome, report) = model::run(&scenario, Choices::seeded(seed));
    assert_eq!(outcome.failure, None, "{}", report["invariants"]);
    report
}

fn counter(r: &Value, k: &str) -> u64 {
    r["metrics"]["counters"][k].as_u64().unwrap_or(0)
}

fn ctl_counter(r: &Value, k: &str) -> u64 {
    r["combined"]["control_metrics"]["counters"][k]
        .as_u64()
        .unwrap_or(0)
}

fn assert_clean(r: &Value) {
    assert_eq!(
        r["invariants"]["hard_failures"], 0,
        "{}\n{}",
        r["invariants"], r["combined"]["control_invariants"]
    );
}

/// racer-sim `cold_start_reads_after_enrollment`.
#[test]
fn cold_start_reads_after_enrollment() {
    let r = quick("full-cold-start", 200, 120, 1, 0.05);
    assert_clean(&r);
    assert_eq!(r["combined"]["control"]["enrolled"], 200);
    assert!(counter(&r, "reads_skipped_not_enrolled") >= 1);
    assert!(counter(&r, "reads_completed") >= 1);
    assert!(counter(&r, "flight_joins") + counter(&r, "peer_requests_acquire") > 0);
    assert!(counter(&r, "membership_publications") >= 1);
    let max = r["origin_per_page"]["max_requests_per_page"]
        .as_u64()
        .unwrap();
    assert!(max <= topology::REPLICAS as u64 * 2, "{max}");
}

/// racer-sim `failover_crashes_data_nodes_too`.
#[test]
fn failover_crashes_data_nodes_too() {
    let r = quick("full-failover", 200, 300, 1, 0.2);
    assert_clean(&r);
    assert!(ctl_counter(&r, "ctl_primary_terms") >= 3);
    assert!(counter(&r, "crashes") >= 3, "{}", r["metrics"]["counters"]);
    assert!(counter(&r, "reads_completed") >= 1);
}

/// racer-sim `failover_repeats_are_skips_or_their_orphans`: in a failover run every repeated
/// origin request is classified, and every repeat while the previous fetcher still held the
/// page is a skip (an owner above rank 0 that fetched after evidence against rank 0) or the
/// rank 0 refetch such a skip leaves behind (an orphan).
#[test]
fn failover_repeats_are_skips_or_their_orphans() {
    for (nodes, secs, seed) in [(200, 300, 1), (400, 300, 2)] {
        let r = quick("full-failover", nodes, secs, seed, 0.2);
        assert_clean(&r);
        let o = &r["origin_per_page"];
        let n = |v: &Value| v.as_u64().unwrap();
        let sum = |v: &Value| v.as_object().unwrap().values().map(n).sum::<u64>();
        let repeats = &o["repeats"];
        let classes: u64 = [
            "after_owner_move",
            "after_failure",
            "concurrent",
            "copy_lost",
            "copy_held",
        ]
        .iter()
        .map(|k| n(&repeats[k]))
        .sum();
        assert_eq!(classes, n(&o["requests"]) - n(&o["pages"]), "{o}");
        let held = n(&repeats["copy_held"]);
        assert_eq!(sum(&repeats["held"]), held, "{o}");
        assert_eq!(n(&repeats["unexplained"]), 0, "{o}");
        let (skips, orphans) = (sum(&o["skips"]), sum(&repeats["orphan"]));
        assert!(skips > 0, "{o}");
        assert!(orphans <= skips, "{o}");
        assert!(held <= skips + orphans, "{o}");
        // The Model never cancels an origin request (racer-sim `origin_canceled`).
        let failures =
            counter(&r, "origin_5xx") + counter(&r, "origin_412") + counter(&r, "origin_403");
        assert!(n(&repeats["after_failure"]) <= failures, "{o}");
        let without = n(&o["max_requests_per_page_excluding_failure_retries"]);
        assert!(without <= topology::REPLICAS as u64, "{o}");
        assert!(
            n(&o["max_requests_per_page"]) <= without + n(&o["max_failure_retries_per_page"]),
            "{o}"
        );
        assert_eq!(n(&repeats["held"]["missed"]), 0, "{o}");
    }
}
