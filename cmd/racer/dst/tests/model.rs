//! Model fidelity: a scenario, its report, and determinism.
use racer_dst::{
    Choices, Scenario,
    choices::Source,
    determinism::run_twice,
    model::{self, ModelSpec, Storm},
    scenario::Fidelity,
    scenarios,
};

#[test]
fn concurrent_cold_readers_report_holds_invariants() {
    let scenario = scenarios::concurrent_cold_readers();
    assert_eq!(scenario.fidelity, Fidelity::Model);
    let (outcome, report) = model::run(&scenario, Choices::seeded(2));
    println!("{report:#}");
    assert_eq!(outcome.failure, None, "{report:#}");
    assert!(outcome.quiescent);
    assert_eq!(report["invariants"]["hard_failures"], 0);
    let counters = &report["metrics"]["counters"];
    assert_eq!(counters["reads_completed"], 64 * 16);
    assert_eq!(counters["reads_failed"], 0);
    let max = report["origin_per_page"]["max_requests_per_page"]
        .as_u64()
        .unwrap();
    assert!((1..=topology::REPLICAS as u64 * 2).contains(&max), "{max}");
    assert!(counters["peer_pages"].as_u64().unwrap() > 0);
    assert!(
        report["metrics"]["histograms"]["read_latency_ns"]["max"]
            .as_u64()
            .unwrap()
            > 0
    );
}

#[test]
fn model_runs_are_deterministic_and_seed_dependent() {
    let scenario = scenarios::concurrent_cold_readers();
    let a = run_twice(&scenario, &Source::Seed(1)).unwrap();
    let b = run_twice(&scenario, &Source::Seed(2)).unwrap();
    assert_ne!(a.trace, b.trace);
}

#[test]
fn model_scenario_checks_fail_the_run() {
    let spec = ModelSpec {
        storms: vec![Storm {
            at: 0,
            object: 0,
            first: 0,
            nodes: 4,
            window: 1,
            range: None,
        }],
        ..ModelSpec::default()
    }
    .check("never", |_| Err("by design".into()));
    let scenario = Scenario::new("t").nodes(4).model(spec);
    let failure = scenario.run(Choices::seeded(1)).failure.unwrap();
    assert_eq!(failure.name, "never");
}

/// A crypto queue far below production's sheds request verifications at the owners with the
/// unsigned overload head: every read
/// ends and origin traffic stays bounded.
#[test]
fn verification_sheds_end_every_read() {
    let spec = ModelSpec {
        storms: vec![Storm {
            at: 0,
            object: 0,
            first: 0,
            nodes: 400,
            window: 10_000_000,
            range: None,
        }],
        crypto_queue: 4,
        ..ModelSpec::default()
    };
    let total = 400 * u64::from(spec.pages_per_object);
    let scenario = Scenario::new("verify-sheds").nodes(400).model(spec);
    let (outcome, report) = model::run(&scenario, Choices::seeded(8));
    assert_eq!(outcome.failure, None, "{report:#}");
    let counters = &report["metrics"]["counters"];
    let ended =
        counters["reads_completed"].as_u64().unwrap() + counters["reads_failed"].as_u64().unwrap();
    assert_eq!(ended, total);
    assert!(counters["verify_sheds"].as_u64().unwrap() > 0, "{counters}");
    let max = report["origin_per_page"]["max_requests_per_page"]
        .as_u64()
        .unwrap();
    assert!((1..=topology::REPLICAS as u64 * 2).contains(&max), "{max}");
}
