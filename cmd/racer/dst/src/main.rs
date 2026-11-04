//! `racer-dst` runner: every seed entry runs twice and must match.
//!
//! Environment: `DST_SEEDS` (a seed file path, or a count `N` to run seeds
//! `1..=N` of every scenario; default `seeds/ci.txt` then
//! `seeds/regressions.txt`), `DST_STEPS` (step budget per run) and
//! `DST_FILTER` (scenario name substring).
use std::process::ExitCode;

use racer_dst::{
    choices::{SeedEntry, Source, parse_seeds},
    determinism::{run_twice, shrink_failure},
    scenarios,
};

const SEED_DIR: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/seeds");

fn var(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.trim().is_empty())
}

fn entries() -> Result<Vec<SeedEntry>, String> {
    let read = |path: &str| {
        std::fs::read_to_string(path)
            .map_err(|e| format!("{path}: {e}"))
            .and_then(|text| parse_seeds(&text).map_err(|e| format!("{path}: {e}")))
    };
    match var("DST_SEEDS") {
        Some(spec) => match spec.trim().parse::<u64>() {
            Ok(count) => Ok(scenarios::catalog()
                .iter()
                .flat_map(|s| {
                    (1..=count).map(|seed| SeedEntry {
                        scenario: s.name.clone(),
                        source: Source::Seed(seed),
                    })
                })
                .collect()),
            Err(_) => read(spec.trim()),
        },
        None => {
            let mut all = read(&format!("{SEED_DIR}/ci.txt"))?;
            all.extend(read(&format!("{SEED_DIR}/regressions.txt"))?);
            Ok(all)
        }
    }
}

fn main() -> ExitCode {
    let entries = match entries() {
        Ok(entries) => entries,
        Err(e) => {
            eprintln!("racer-dst: {e}");
            return ExitCode::FAILURE;
        }
    };
    let steps = match var("DST_STEPS").map(|s| s.trim().parse::<u64>()) {
        None => None,
        Some(Ok(steps)) => Some(steps),
        Some(Err(_)) => {
            eprintln!("racer-dst: DST_STEPS must be an unsigned integer");
            return ExitCode::FAILURE;
        }
    };
    let filter = var("DST_FILTER").unwrap_or_default();
    let (mut ran, mut failed) = (0u32, 0u32);
    for entry in entries {
        if !entry.scenario.contains(&filter) {
            continue;
        }
        let Some(mut scenario) = scenarios::find(&entry.scenario) else {
            eprintln!("racer-dst: unknown scenario in {entry}");
            failed += 1;
            continue;
        };
        if let Some(steps) = steps {
            scenario = scenario.max_steps(steps);
        }
        ran += 1;
        match run_twice(&scenario, &entry.source) {
            Err(divergence) => {
                failed += 1;
                eprintln!("FAIL {entry}: {divergence}");
            }
            Ok(outcome) => match &outcome.failure {
                None => println!(
                    "ok   {entry}: trace {:016x} steps {} end {} ns",
                    outcome.trace, outcome.steps, outcome.end
                ),
                Some(failure) => {
                    failed += 1;
                    eprintln!("FAIL {entry}: {failure}");
                    if let Some(words) = shrink_failure(&scenario, &entry.source, 2000) {
                        let shrunk = SeedEntry {
                            scenario: entry.scenario.clone(),
                            source: Source::Words(words),
                        };
                        eprintln!("     shrunk: {shrunk}");
                    }
                }
            },
        }
    }
    println!("racer-dst: {ran} entries, {failed} failed");
    if failed == 0 && ran > 0 {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}
