//! Process entry point. Configuration and application lifecycle own all resources.

use racer_dataplane::app::Application;
use racer_dataplane::config::Config;

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

fn run() -> std::result::Result<(), (&'static str, racer_dataplane::error::Error)> {
    let config = Config::from_env().map_err(|error| ("config", error))?;
    let application = Application::assemble(config).map_err(|error| ("assemble", error))?;
    application.run().map_err(|error| ("run", error))
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
