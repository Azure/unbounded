//! Determinism double-run and failure shrinking.
//!
//! Every CI seed runs twice with the same choice stream; the trace digests,
//! step counts and draw counts must match. A mismatch means some behavior
//! depended on something outside [`crate::Choices`]: a `RandomState` hash map
//! iterated on a behavior path, a host clock or entropy read, a process global,
//! or a pointer-address ordering.
use crate::{
    choices::{Choices, Source, shrink},
    scenario::Scenario,
    world::Outcome,
};

/// Two runs of the same stream disagreed.
#[derive(Clone, Debug, PartialEq, Eq)]
pub struct Divergence {
    /// First run.
    pub first: Outcome,
    /// Second run.
    pub second: Outcome,
}

impl std::fmt::Display for Divergence {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(
            f,
            "nondeterministic: trace {:016x} vs {:016x}, steps {} vs {}, draws {} vs {}",
            self.first.trace,
            self.second.trace,
            self.first.steps,
            self.second.steps,
            self.first.draws,
            self.second.draws
        )
    }
}

/// Run `scenario` twice over `source` and require identical outcomes.
pub fn run_twice(scenario: &Scenario, source: &Source) -> Result<Outcome, Box<Divergence>> {
    let first = scenario.run(source.choices());
    let second = scenario.run(source.choices());
    if first == second {
        Ok(first)
    } else {
        Err(Box::new(Divergence { first, second }))
    }
}

/// Record the choice stream of a failing run and shrink it to a smaller
/// stream that still fails with the same invariant. Returns the shrunk raw
/// draws (check them in as a `words=` seed entry), or `None` if `source` does
/// not fail.
pub fn shrink_failure(scenario: &Scenario, source: &Source, budget: usize) -> Option<Vec<u64>> {
    let choices = source.choices().recording();
    let outcome = scenario.run(choices.clone());
    let name = outcome.failure?.name;
    let words = choices.recorded().unwrap_or_default();
    Some(shrink(&words, budget, |candidate| {
        scenario
            .run(Choices::replay(candidate))
            .failure
            .is_some_and(|f| f.name == name)
    }))
}
