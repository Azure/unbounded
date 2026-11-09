//! `racer-dst` runner: every seed entry runs twice and must match.
//!
//! Environment: `DST_SEEDS` (a seed file path, or a count `N` to run seeds
//! `1..=N` of every non-app scenario, then `seeds/regressions.txt`; default `seeds/ci.txt` then
//! `seeds/regressions.txt`), `DST_STEPS` (step budget per run) and
//! `DST_FILTER` (scenario name substring). Without a filter, the default and
//! count sets end with the Model variant coverage check over the default set.
//!
//! With arguments it runs one scenario and writes its JSON report instead, using the
//! `racer-dst` command line (`--scenario`, `--nodes`, `--seed`, `--duration`, `--config`,
//! `--out`; see [`racer_dst::cli`]). It exits 1 when the run fails a check or invariant (the report is
//! still written) and 2 on a usage error.
use std::process::ExitCode;

use racer_dst::{
    choices::{SeedEntry, Source},
    cli,
    determinism::{run_twice, shrink_failure},
    scenarios, seeds,
};

fn var(name: &str) -> Option<String> {
    std::env::var(name).ok().filter(|v| !v.trim().is_empty())
}

/// The entries to run and whether to run the coverage check after them.
fn entries() -> Result<(Vec<SeedEntry>, bool), String> {
    match var("DST_SEEDS") {
        Some(spec) => match spec.trim().parse::<u64>() {
            Ok(count) => Ok((seeds::counted(count)?, true)),
            Err(_) => Ok((seeds::read(spec.trim())?, false)),
        },
        None => Ok((seeds::default_set()?, true)),
    }
}

/// Coverage check over the default seed set (see [`seeds::coverage`]).
/// Returns 1 on a gap, 0 otherwise.
fn coverage_check() -> u32 {
    let entries = match seeds::default_set() {
        Ok(entries) => entries,
        Err(e) => {
            eprintln!("FAIL coverage: {e}");
            return 1;
        }
    };
    let coverage = seeds::coverage(&entries);
    if coverage.missing.is_empty() && coverage.stale.is_empty() {
        println!("ok   coverage: {} variants seen", coverage.seen.len());
        return 0;
    }
    eprintln!(
        "FAIL coverage: missing {:?}, exercised but exempt {:?}",
        coverage.missing, coverage.stale
    );
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
    let (entries, check_coverage) = match entries() {
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
    if filter.is_empty() && check_coverage {
        failed += coverage_check();
    }
    println!("racer-dst: {ran} entries, {failed} failed");
    if failed == 0 && ran > 0 {
        ExitCode::SUCCESS
    } else {
        ExitCode::FAILURE
    }
}
