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

/// racer-sim `churn_grows_the_data_plane`. The Model has no `membership_adoptions` counter;
/// a node adopts exactly what its client applied (`dst/MODEL.md`, Combined scenarios).
#[test]
fn churn_grows_the_data_plane() {
    let r = quick("full-churn", 320, 400, 1, 0.1);
    assert_clean(&r);
    assert!(
        counter(&r, "membership_publications") >= 4,
        "{}",
        r["metrics"]["counters"]
    );
    assert!(ctl_counter(&r, "ctl_pods_removed") > 0);
    assert!(counter(&r, "reads_completed") >= 1);
}

/// racer-sim `lagging_clients_skew_the_data_plane`.
#[test]
fn lagging_clients_skew_the_data_plane() {
    let r = quick("full-lagging", 300, 600, 1, 0.2);
    assert_clean(&r);
    assert!(ctl_counter(&r, "ctl_unreachable") > 0);
    assert!(counter(&r, "reads_completed") >= 1);
    let incompatible = r["metrics"]["errors"]["IncompatibleMembership"]
        .as_u64()
        .unwrap_or(0);
    assert!(
        counter(&r, "stale_membership_replies") + incompatible > 0,
        "{}",
        r["metrics"]["counters"]
    );
}

/// racer-sim `data_faults_join_the_control_plans`: the combined plans carry data-plane faults
/// by default and `"combined_faults"` turns them off. racer-sim's explicit `faults` list has no
/// Model counterpart (scenario faults are set in code).
#[test]
fn data_faults_join_the_control_plans() {
    use racer_dst::scenarios::{DataFaults, combined_with};
    let faults = DataFaults::default();
    let failover = combined_with("full-failover", 200, 0.2, Some(300), &faults);
    let spec = failover.model.as_ref().unwrap();
    assert_eq!(spec.site_cuts.len(), 1);
    assert_eq!(spec.origin_bursts.len(), 2);
    // Every 100th node of 1,000 is slowed 8x, and later restored.
    let lagging = combined_with("full-lagging", 1000, 0.2, Some(300), &faults);
    assert_eq!(lagging.faults.len(), 20);
    let mut off = faults;
    off.apply_json(&serde_json::json!({"enabled": false}))
        .unwrap();
    let quiet = combined_with("full-failover", 200, 0.2, Some(300), &off);
    let spec = quiet.model.as_ref().unwrap();
    assert!(spec.site_cuts.is_empty() && spec.origin_bursts.is_empty());
    assert!(
        off.apply_json(&serde_json::json!({"slow_factor": 0}))
            .is_err()
    );
    assert!(
        DataFaults::default()
            .apply_json(&serde_json::json!({"nope": 1}))
            .is_err()
    );

    let r_cut = quick("full-failover", 200, 300, 1, 0.2);
    assert_clean(&r_cut);
    assert!(counter(&r_cut, "origin_5xx") >= 1);
    assert!(counter(&r_cut, "link_failures") >= 1);
    let (outcome, r) = model::run(&quiet, Choices::seeded(1));
    assert_eq!(outcome.failure, None);
    assert_eq!(counter(&r, "origin_5xx"), 0);
    // Links to crashed Pods still fail; the site cut's do not.
    let (cut, quiet) = (
        counter(&r_cut, "link_failures"),
        counter(&r, "link_failures"),
    );
    assert!(
        quiet < cut,
        "{quiet} link failures without the cut, {cut} with it"
    );
}

/// racer-sim `replays_exactly`.
#[test]
fn replays_exactly() {
    let run = |name: &str, seed: u64| {
        let scenario = scenarios::combined(name, 80, 0.1, Some(120));
        model::run(&scenario, Choices::seeded(seed))
    };
    for name in [
        "full-cold-start",
        "full-failover",
        "full-churn",
        "full-lagging",
    ] {
        let ((a, ra), (b, rb)) = (run(name, 3), run(name, 3));
        assert_eq!(a.trace, b.trace, "{name}");
        assert_eq!(ra["metrics"], rb["metrics"], "{name}");
    }
    assert_ne!(run("full-churn", 3).0.trace, run("full-churn", 4).0.trace);
}

/// racer-sim `failover_drops_the_dead_member`: the Pod crashed for good at `d / 2 + 5 s` leaves
/// the membership once its Node has been not ready for the grace, so peers stop choosing it.
#[test]
fn failover_drops_the_dead_member() {
    let r = quick("full-failover", 200, 300, 1, 0.2);
    assert_clean(&r);
    assert!(counter(&r, "dead_member_links") > 0, "{}", r["metrics"]);
    // Node monitor grace 40 s, readiness grace 30 s, one 10 s pass, plus client propagation.
    let age = counter(&r, "dead_member_link_age_max_s");
    assert!(age <= 90, "{age}");
    assert!(
        ctl_counter(&r, "ctl_commits") > 0 && counter(&r, "membership_publications") >= 2,
        "{}",
        r["metrics"]["counters"]
    );
}
