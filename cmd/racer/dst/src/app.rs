//! Full fidelity Racer application worlds.
//!
//! Each app scenario runs one [`racer::app::dst`] harness as a single role:
//! real `WorkerApplication` graphs assembled through
//! `WorkerApplication::assemble` on `Driver::Sim`, with stepped page crypto,
//! a simulated RDMA fabric, fake UDS origins and clients whose responses are
//! checked against an origin oracle. The harness keeps its own `SimOs` and
//! `SimClock` (it moves time itself, 1 ms per turn), so the world sees one
//! role that finishes in one step; the run's replay digest is folded into
//! the world trace, so the determinism double-run compares the whole app
//! replay. Harness assertions (oracle bytes, 412/502 contract, resource
//! bounds, release at quiescence, pending work without a wake source) become
//! world violations.
use racer_io::exec::Next;

use crate::{
    engine::SECONDS,
    scenario::Scenario,
    world::{NodeEnv, Reporter, SimRole, Trace},
};

/// Trace tag for an app replay digest.
const TAG_APP_DIGEST: u64 = 0xa99;

/// Scenario name prefix; the rest is a [`racer::app::dst::SCENARIOS`] name.
pub const PREFIX: &str = "app-";

struct AppRole {
    name: &'static str,
    seed: u64,
    trace: Trace,
    report: Reporter,
    done: bool,
}

impl SimRole for AppRole {
    fn step(&mut self) -> racer_io::Result<Next> {
        if !self.done {
            self.done = true;
            match racer::app::dst::run(self.name, self.seed, 0) {
                Ok(run) => self.trace.record_bytes(TAG_APP_DIGEST, &run.digest),
                Err(message) => self
                    .report
                    .violation(format!("app {} seed={}: {message}", self.name, self.seed)),
            }
        }
        Ok(Next::Done)
    }

    fn woken(&self) -> bool {
        false
    }

    fn pending(&self) -> bool {
        !self.done
    }
}

fn role(name: &'static str, env: &NodeEnv) -> AppRole {
    AppRole {
        name,
        // Fixed regressions ignore the seed; generated runs replay it.
        seed: env.choices.below(1 << 32),
        trace: env.trace.clone(),
        report: env.report.clone(),
        done: false,
    }
}

/// One app scenario per [`racer::app::dst::SCENARIOS`] entry, named
/// `app-<name>`.
pub fn catalog() -> Vec<Scenario> {
    racer::app::dst::SCENARIOS
        .iter()
        .map(|&name| {
            Scenario::new(format!("{PREFIX}{name}"))
                .roles(move |env| Ok(vec![("app".into(), Box::new(role(name, env)) as _)]))
                .end(SECONDS)
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn catalog_names_every_app_scenario() {
        let names: Vec<_> = catalog().into_iter().map(|s| s.name).collect();
        assert_eq!(names.len(), racer::app::dst::SCENARIOS.len());
        assert!(names.contains(&"app-generated".to_string()));
    }

    #[test]
    fn unknown_app_scenario_is_an_error() {
        assert!(racer::app::dst::run("no-such", 1, 1).is_err());
    }
}
