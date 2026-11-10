//! The seed sets `make racer-dst` runs and the Model variant coverage check over them.
use std::collections::BTreeSet;

use crate::{
    choices::{SeedEntry, Source, parse_seeds},
    model,
    scenario::Fidelity,
    scenarios,
};

/// Directory holding `ci.txt` and `regressions.txt`.
pub const DIR: &str = concat!(env!("CARGO_MANIFEST_DIR"), "/seeds");

/// Parse the seed file at `path`.
pub fn read(path: &str) -> Result<Vec<SeedEntry>, String> {
    std::fs::read_to_string(path)
        .map_err(|e| format!("{path}: {e}"))
        .and_then(|text| parse_seeds(&text).map_err(|e| format!("{path}: {e}")))
}

/// Shrunk failures checked in after a fix; every seed set replays them.
pub fn regressions() -> Result<Vec<SeedEntry>, String> {
    read(&format!("{DIR}/regressions.txt"))
}

/// The default set: `ci.txt` then `regressions.txt`.
pub fn default_set() -> Result<Vec<SeedEntry>, String> {
    let mut all = read(&format!("{DIR}/ci.txt"))?;
    all.extend(regressions()?);
    Ok(all)
}

/// Seeds `1..=count` of every [`scenarios::catalog`] scenario (not the
/// `app-*` scenarios, which run from seed files), then `regressions.txt`.
pub fn counted(count: u64) -> Result<Vec<SeedEntry>, String> {
    let mut all: Vec<SeedEntry> = scenarios::catalog()
        .iter()
        .flat_map(|s| {
            (1..=count).map(|seed| SeedEntry {
                scenario: s.name.clone(),
                source: Source::Seed(seed),
            })
        })
        .collect();
    all.extend(regressions()?);
    Ok(all)
}

/// Result of [`coverage`].
#[derive(Debug, Default)]
pub struct Coverage {
    /// Variants exercised by the Model entries.
    pub seen: BTreeSet<String>,
    /// Required variants no entry exercised.
    pub missing: Vec<&'static str>,
    /// Exempt variants that some entry exercised (stale exemptions).
    pub stale: Vec<&'static str>,
}

/// Run the Model entries of `entries` once and check that together they
/// exercise every core effect and serve outcome variant ([`model::coverage`]).
pub fn coverage(entries: &[SeedEntry]) -> Coverage {
    let mut seen = BTreeSet::new();
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
    Coverage {
        seen,
        missing,
        stale,
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn counted_runs_every_seed_then_regressions() {
        let catalog = scenarios::catalog();
        let regressions = regressions().unwrap();
        let entries = counted(3).unwrap();
        assert_eq!(entries.len(), 3 * catalog.len() + regressions.len());
        let first = &catalog[0].name;
        assert_eq!(entries.iter().filter(|e| &e.scenario == first).count(), 3);
        assert_eq!(
            entries[entries.len() - regressions.len()..],
            regressions[..]
        );
        assert_eq!(counted(0).unwrap(), regressions);
    }

    #[test]
    fn default_set_includes_regressions() {
        let all = default_set().unwrap();
        let regressions = regressions().unwrap();
        assert!(all.len() > regressions.len());
        assert_eq!(all[all.len() - regressions.len()..], regressions[..]);
    }

    #[test]
    fn missing_seed_file_is_an_error() {
        assert!(read(&format!("{DIR}/no-such.txt")).is_err());
    }
}
