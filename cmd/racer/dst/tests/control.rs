//! Model control scenarios: racer-sim's control plans at small sizes, and a
//! report dump for parity runs against racer-sim.
use racer_dst::{Choices, engine::SECONDS, model, scenarios};

/// racer-sim's `control_scenarios_run_from_the_cli`: every plan at 40 Pods
/// for 60 s runs without a hard invariant failure.
#[test]
fn control_plans_run_clean_at_40_pods() {
    for plan in ["enrollment-storm", "failover", "ctl-churn", "lagging"] {
        let mut scenario = scenarios::control(plan, 40).end(60 * SECONDS);
        // The plan checks assume the default durations.
        if let Some(spec) = scenario.model.as_mut() {
            std::rc::Rc::get_mut(spec).unwrap().checks.clear();
        }
        let (outcome, report) = model::run(&scenario, Choices::seeded(1));
        assert_eq!(outcome.failure, None, "{plan}");
        assert_eq!(report["invariants"]["hard_failures"], 0, "{plan}");
        assert!(report["nodes"].as_u64().unwrap() > 0);
    }
}

/// Write the report of `CTL_PLAN` (racer-sim plan name) at `CTL_NODES` Pods
/// and `CTL_SEED` to `CTL_OUT` (stdout if unset), for the parity table in
/// `dst/MODEL.md`.
#[test]
#[ignore = "parity: run with --ignored --nocapture"]
fn control_report() {
    let var = |k: &str| std::env::var(k).ok();
    let plan: &'static str =
        var("CTL_PLAN").map_or("enrollment-storm", |p| Box::leak(p.into_boxed_str()));
    let nodes = var("CTL_NODES").map_or(200, |n| n.parse().unwrap());
    let seed = var("CTL_SEED").map_or(1, |s| s.parse().unwrap());
    let mut scenario = scenarios::control(plan, nodes);
    if let Some(end) = var("CTL_SECS") {
        scenario = scenario.end(end.parse::<u64>().unwrap() * SECONDS);
    }
    let (outcome, report) = model::run(&scenario, Choices::seeded(seed));
    let text = serde_json::to_string_pretty(&report).unwrap();
    match var("CTL_OUT") {
        Some(path) => std::fs::write(path, text).unwrap(),
        None => println!("{text}"),
    }
    println!("failure {:?}", outcome.failure);
}

/// Write the report of combined scenario `CTL_PLAN` (`full-*`) at
/// `CTL_NODES`, `CTL_RATE` reads per node per second, `CTL_SECS` and
/// `CTL_SEED` to `CTL_OUT` (stdout if unset).
#[test]
#[ignore = "parity: run with --ignored --nocapture"]
fn combined_report() {
    let var = |k: &str| std::env::var(k).ok();
    let name = var("CTL_PLAN").unwrap_or_else(|| "full-cold-start".into());
    let nodes = var("CTL_NODES").map_or(200, |n| n.parse().unwrap());
    let seed = var("CTL_SEED").map_or(1, |s| s.parse().unwrap());
    let rate = var("CTL_RATE").map_or(0.05, |s| s.parse().unwrap());
    let secs = var("CTL_SECS").map(|s| s.parse().unwrap());
    let mut scenario = scenarios::combined(&name, nodes, rate, secs);
    // `CTL_NO_CUT`, `CTL_NO_BURSTS`: drop the site cut or origin bursts (diagnostics).
    if let Some(spec) = scenario.model.as_mut() {
        let spec = std::rc::Rc::get_mut(spec).unwrap();
        if var("CTL_NO_CUT").is_some() {
            spec.site_cuts.clear();
        }
        if var("CTL_NO_BURSTS").is_some() {
            spec.origin_bursts.clear();
        }
    }
    let started = std::time::Instant::now();
    let (outcome, report) = model::run(&scenario, Choices::seeded(seed));
    let text = serde_json::to_string_pretty(&report).unwrap();
    match var("CTL_OUT") {
        Some(path) => std::fs::write(path, text).unwrap(),
        None => println!("{text}"),
    }
    println!("failure {:?} wall {:?}", outcome.failure, started.elapsed());
}
