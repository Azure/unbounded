//! Process entry point. Configuration and application lifecycle own all resources.
//!
//! In managed deployments the embedded cluster-control service starts first, on
//! its own bounded thread, and the dataplane waits for its bootstrap before any
//! worker is assembled. Standalone fixtures set a static endpoint instead.

use racer_dataplane::app::{Application, StopSignals};
use racer_dataplane::config::Config;
use racer_dataplane::control::managed::{BootstrapError, Bootstrapped, ClusterControl};
use racer_dataplane::error::Error;
use std::sync::Arc;
use std::time::Duration;

fn main() -> std::process::ExitCode {
    tune_allocator();

    match run() {
        Ok(()) => std::process::ExitCode::SUCCESS,
        Err((stage, error)) => {
            // Workers log their own failing stage first; this names the lifecycle phase.
            eprintln!("racer-dataplane: fatal stage={stage} error={error}");
            std::process::ExitCode::FAILURE
        }
    }
}

/// Cluster-control handle adapted to the dataplane startup contract.
struct Embedded(racer_cluster_control::Handle);

impl ClusterControl for Embedded {
    fn bootstrap(&self, timeout: Duration) -> Result<Bootstrapped, BootstrapError> {
        match self.0.bootstrap(timeout) {
            Ok(bootstrap) => Ok(Bootstrapped {
                cluster: bootstrap.cluster,
                block_devices: bootstrap.block_devices,
            }),
            Err(racer_cluster_control::Error::Unavailable) => Err(BootstrapError::Retry),
            Err(racer_cluster_control::Error::Timeout) => Err(BootstrapError::Pending),
            Err(error) => {
                eprintln!("racer-dataplane: stage=cluster-bootstrap control error={error}");
                Err(BootstrapError::Fatal)
            }
        }
    }

    fn endpoints(&self) -> wire_codec::DiscoveredEndpoints {
        self.0.endpoints()
    }

    fn report(&self, shares: u32, rails: Vec<racer_control_wire::RailMapping>) {
        // Reports are retried by control; a refused enqueue is logged, never fatal.
        if let Err(error) = self.0.report(shares, rails) {
            eprintln!("racer-dataplane: stage=cluster-report error={error}");
        }
    }
}

fn run() -> std::result::Result<(), (&'static str, Error)> {
    let mut config = Config::from_env().map_err(|error| ("config", error))?;
    // Held across cluster bootstrap and the application so SIGTERM/SIGINT stop a
    // managed bootstrap wait; the application shares, never resets, this state.
    let signals = StopSignals::install().map_err(|error| ("signals", error))?;
    let control = racer_cluster_control::Config::from_env().map_err(|error| {
        eprintln!("racer-dataplane: stage=cluster-control-config error={error}");
        ("cluster-control-config", Error::InvalidConfiguration)
    })?;
    // Retained until the application stops; dropping it stops control boundedly.
    // The concrete handle is kept beside the trait object for fatal diagnostics.
    let control = match (control, config.managed()) {
        (None, false) => None,
        (Some(control), true) => {
            let handle = racer_cluster_control::start(control).map_err(|error| {
                eprintln!("racer-dataplane: stage=cluster-control-start error={error}");
                ("cluster-control-start", Error::Unavailable)
            })?;
            let concrete = Arc::new(Embedded(handle));
            let embedded: Arc<dyn ClusterControl> = concrete.clone();
            match racer_dataplane::control::managed::start(&mut config, embedded, &|| {
                signals.requested()
            }) {
                Ok(()) => {}
                // A requested stop during bootstrap is a clean exit; dropping the
                // handle stops embedded control boundedly.
                Err(Error::Cancelled) if signals.requested() => {
                    eprintln!("racer-dataplane: stage=cluster-bootstrap stopped by signal");
                    return Ok(());
                }
                Err(error) => return Err(fatal(&concrete, ("cluster-bootstrap", error))),
            }
            Some(concrete)
        }
        (Some(_), false) => {
            eprintln!(
                "racer-dataplane: RACER_CLUSTER_ID and RACER_CONTROL_ENDPOINT must be unset when RACER_NAMESPACE selects managed control"
            );
            return Err(("config", Error::InvalidConfiguration));
        }
        (None, true) => {
            eprintln!(
                "racer-dataplane: set RACER_NAMESPACE for managed control, or both RACER_CLUSTER_ID and RACER_CONTROL_ENDPOINT for standalone control"
            );
            return Err(("config", Error::InvalidConfiguration));
        }
    };
    let report = |failure| match &control {
        Some(control) => fatal(control, failure),
        None => failure,
    };
    let application = Application::assemble(config).map_err(|error| report(("assemble", error)))?;
    let result = application.run().map_err(|error| report(("run", error)));
    drop(signals);
    result
}

/// Log payload-free embedded control state for a fatal lifecycle failure.
fn fatal(control: &Embedded, failure: (&'static str, Error)) -> (&'static str, Error) {
    let state = control.0.diagnostics();
    let last_error = state
        .last_error
        .map_or_else(|| "none".to_owned(), |error| error.to_string());
    eprintln!(
        "racer-dataplane: stage={} control role={:?} sequence={} ready={} tasks={} last_error={last_error}",
        failure.0, state.role, state.sequence, state.ready, state.tasks
    );
    failure
}

/// Environment variable glibc reads at startup to fix the mmap threshold. When an operator sets
/// it, glibc has already applied that value and this process leaves it alone.
#[cfg(all(target_os = "linux", target_env = "gnu"))]
const MALLOC_MMAP_THRESHOLD_ENV: &str = "MALLOC_MMAP_THRESHOLD_";

/// Fixed glibc mmap threshold applied at startup: 1 MiB.
///
/// Recycled page buffers (16 MiB) are allocated through the global allocator. glibc raises its
/// dynamic mmap threshold after the first large free (up to 32 MiB on 64-bit), so later page
/// buffers would come from heap arenas and freeing them may not return memory to the kernel.
/// Setting the threshold explicitly disables that dynamic adjustment, so every allocation of
/// 1 MiB or more is a private mapping that is unmapped on free.
#[cfg(all(target_os = "linux", target_env = "gnu"))]
const MMAP_THRESHOLD_BYTES: libc::c_int = 1 << 20;

/// Returns whether the process should set the glibc mmap threshold, given the value of
/// `MALLOC_MMAP_THRESHOLD_` from the environment. An operator override always wins.
#[cfg_attr(not(all(target_os = "linux", target_env = "gnu")), allow(dead_code))]
fn should_set_mmap_threshold(env: Option<&std::ffi::OsStr>) -> bool {
    env.is_none()
}

/// Applies glibc allocator tuning. Runs first in `main`, before any worker thread starts.
#[cfg(all(target_os = "linux", target_env = "gnu"))]
fn tune_allocator() {
    let env = std::env::var_os(MALLOC_MMAP_THRESHOLD_ENV);
    if !should_set_mmap_threshold(env.as_deref()) {
        eprintln!("racer-dataplane: malloc mmap threshold left to {MALLOC_MMAP_THRESHOLD_ENV}");
        return;
    }

    // SAFETY: mallopt only adjusts allocator parameters and is called before other threads exist.
    let applied = unsafe { libc::mallopt(libc::M_MMAP_THRESHOLD, MMAP_THRESHOLD_BYTES) };
    if applied == 1 {
        eprintln!("racer-dataplane: malloc mmap threshold set bytes={MMAP_THRESHOLD_BYTES}");
    } else {
        eprintln!(
            "racer-dataplane: malloc mmap threshold not applied bytes={MMAP_THRESHOLD_BYTES}"
        );
    }
}

#[cfg(not(all(target_os = "linux", target_env = "gnu")))]
fn tune_allocator() {}

#[cfg(test)]
mod tests {
    use super::should_set_mmap_threshold;
    use std::ffi::OsStr;

    #[test]
    fn sets_mmap_threshold_when_env_unset() {
        assert!(should_set_mmap_threshold(None));
    }

    #[test]
    fn respects_operator_mmap_threshold_override() {
        assert!(!should_set_mmap_threshold(Some(OsStr::new("4194304"))));
    }

    #[test]
    fn respects_empty_operator_mmap_threshold_override() {
        assert!(!should_set_mmap_threshold(Some(OsStr::new(""))));
    }
}
