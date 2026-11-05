//! `racer-dst` runner: every seed entry runs twice and must match.
//!
//! Environment: `DST_SEEDS` (a seed file path, or a count `N` to run seeds
//! `1..=N` of every scenario; default `seeds/ci.txt` then
//! `seeds/regressions.txt`), `DST_STEPS` (step budget per run) and
//! `DST_FILTER` (scenario name substring).
//!
//! With arguments it runs one scenario and writes its JSON report instead, with racer-sim's
//! command line (`--scenario`, `--nodes`, `--seed`, `--duration`, `--config`, `--out`; see
//! [`racer_dst::cli`]). It exits 1 when the run fails a check or invariant (the report is
//! still written) and 2 on a usage error.
use std::process::ExitCode;

use racer_dst::{
    choices::{SeedEntry, Source, parse_seeds},
    cli,
    determinism::{run_twice, shrink_failure},
    model,
    scenario::Fidelity,
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

/// Plan section 9.2: the Model entries of the seed set together exercise every core effect and
/// serve outcome variant ([`model::coverage`]). Returns 1 on a gap, 0 otherwise.
fn coverage_parity(entries: &[SeedEntry]) -> u32 {
    let mut seen = std::collections::BTreeSet::new();
    for entry in entries {
        let Some(scenario) = scenarios::find(&entry.scenario) else {
            continue;
        };
        if scenario.fidelity != Fidelity::Model {
            continue;
        }
        let (_, report) = model::run(&scenario, entry.source.choices());
        if let Some(coverage) = report["coverage"].as_object() {
            seen.extend(coverage.keys().cloned());
        }
    }
    let (missing, stale) = model::coverage::check(&seen);
    if missing.is_empty() && stale.is_empty() {
        println!("ok   coverage parity: {} variants seen", seen.len());
        return 0;
    }
    eprintln!("FAIL coverage parity: missing {missing:?}, exercised but exempt {stale:?}");
    1
}

/// Run one scenario from the command line and write its report.
fn report(args: &[String]) -> ExitCode {
    let options = match cli::Options::parse(args) {
        Ok(options) => options,
        Err(err) => {
            eprintln!("racer-dst: {err}\n{}", cli::USAGE);
            return ExitCode::from(2);
        }
    };
    let (report, failed) = match cli::run(&options) {
        Ok(result) => result,
        Err(err) => {
            eprintln!("racer-dst: {err}");
            return ExitCode::from(2);
        }
    };
    let text = serde_json::to_string_pretty(&report).expect("report serializes");
    match &options.out {
        Some(path) => {
            if let Err(err) = std::fs::write(path, text + "\n") {
                eprintln!("racer-dst: write {path}: {err}");
                return ExitCode::FAILURE;
            }
        }
        None => println!("{text}"),
    }
    if failed {
        eprintln!("racer-dst: {}", report["failure"]);
        return ExitCode::FAILURE;
    }
    ExitCode::SUCCESS
}

fn main() -> ExitCode {
    let args: Vec<String> = std::env::args().skip(1).collect();
    if !args.is_empty() {
        return report(&args);
    }
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
    let entries_for_coverage = entries.clone();
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
    if filter.is_empty() && var("DST_SEEDS").is_none() {
        failed += coverage_parity(&entries_for_coverage);
    }
    println!("racer-dst: {ran} entries, {failed} failed");
    if failed == 0 && ran > 0 {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}
