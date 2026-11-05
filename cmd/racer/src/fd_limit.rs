//! Startup check that the process may open every descriptor its budgets admit.
//!
//! Connection and pipe budgets are admission limits, not descriptor reservations:
//! a node that admits more sockets than `RLIMIT_NOFILE` allows fails with
//! `EMFILE` under load instead of answering `Overloaded`. Startup therefore
//! computes the descriptor budget from the final node-wide limits, raises the
//! soft limit toward the hard limit when needed, and refuses to start when the
//! hard limit is too low.

use crate::config::Limits;
use crate::error::{Error, Result};

/// Descriptors outside the per-worker budgets: listeners, the store and its
/// files, control, diagnostics, helper threads, and the standard streams.
pub const BASE_DESCRIPTORS: u64 = 1024;

/// Per-worker descriptors outside the connection and pipe budgets: the
/// io_uring instance, wakeup eventfds, per-worker listeners, and cache files.
pub const WORKER_DESCRIPTORS: u64 = 64;

/// Variables whose values set the descriptor budget, named in errors.
pub const VARIABLES: &str = "RACER_CLIENT_CONNECTIONS(_PER_WORKER), RACER_PEER_CONNECTIONS(_PER_WORKER), RACER_OUTBOUND_CONNECTIONS(_PER_WORKER), RACER_PIPES(_PER_WORKER), RACER_IO_WORKERS";

/// Descriptors needed for node-wide (not partitioned) `limits` across `workers` I/O workers.
/// Each pipe holds two descriptors. Saturates instead of overflowing.
pub fn required(limits: &Limits, workers: usize) -> u64 {
    let count = |n: std::num::NonZeroUsize| n.get() as u64;
    BASE_DESCRIPTORS
        .saturating_add(count(limits.client_connections))
        .saturating_add(count(limits.peer_connections))
        .saturating_add(count(limits.outbound_connections))
        .saturating_add(count(limits.pipes).saturating_mul(2))
        .saturating_add((workers as u64).saturating_mul(WORKER_DESCRIPTORS))
}

/// The soft limit to set, `None` when the current soft limit already suffices,
/// or the operator-facing error when the hard limit is below `required`.
pub fn plan(required: u64, soft: u64, hard: u64) -> std::result::Result<Option<u64>, String> {
    if soft >= required {
        Ok(None)
    } else if hard >= required {
        Ok(Some(required))
    } else {
        Err(message(required, hard))
    }
}

/// Explain an insufficient hard `RLIMIT_NOFILE` and how to fix it.
pub fn message(required: u64, hard: u64) -> String {
    format!(
        "RLIMIT_NOFILE hard limit {hard} is below the {required} file descriptors required by the configured budgets; raise the hard limit (for example the container runtime's nofile ulimit) or lower {VARIABLES}"
    )
}

/// Ensure the soft `RLIMIT_NOFILE` covers `limits` across `workers` workers.
///
/// # Errors
/// Returns [`Error::InvalidConfiguration`] after logging when the hard limit is
/// too low, or [`Error::Unavailable`] when the limit cannot be read or set.
pub fn ensure(limits: &Limits, workers: usize) -> Result<()> {
    let required = required(limits, workers);
    let mut current = libc::rlimit {
        rlim_cur: 0,
        rlim_max: 0,
    };
    // SAFETY: `current` is a valid, writable rlimit for the call's duration.
    if unsafe { libc::getrlimit(libc::RLIMIT_NOFILE, &mut current) } != 0 {
        eprintln!(
            "racer: stage=fd-limit error={}",
            std::io::Error::last_os_error()
        );
        return Err(Error::Unavailable);
    }
    match plan(required, current.rlim_cur, current.rlim_max) {
        Ok(None) => Ok(()),
        Ok(Some(soft)) => {
            let raised = libc::rlimit {
                rlim_cur: soft,
                rlim_max: current.rlim_max,
            };
            // SAFETY: `raised` is a valid rlimit; lowering nothing below the hard limit.
            if unsafe { libc::setrlimit(libc::RLIMIT_NOFILE, &raised) } != 0 {
                eprintln!(
                    "racer: stage=fd-limit error={}",
                    std::io::Error::last_os_error()
                );
                return Err(Error::Unavailable);
            }
            eprintln!(
                "racer: stage=fd-limit raised soft RLIMIT_NOFILE from {} to {soft}",
                current.rlim_cur
            );
            Ok(())
        }
        Err(message) => {
            eprintln!("racer: stage=fd-limit error={message}");
            Err(Error::InvalidConfiguration)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::num::NonZeroUsize;

    fn limits(client: usize, peer: usize, outbound: usize, pipes: usize) -> Limits {
        let n = |v| NonZeroUsize::new(v).unwrap();
        Limits {
            client_connections: n(client),
            peer_connections: n(peer),
            outbound_connections: n(outbound),
            pipes: n(pipes),
            ..Limits::default()
        }
    }

    #[test]
    fn sixteen_default_workers_need_about_thirty_seven_thousand() {
        let workers = 16;
        let l = limits(64 * workers, 2048 * workers, 64 * workers, 16 * workers);
        assert_eq!(
            required(&l, workers),
            1024 + 1024 + 32_768 + 1024 + 512 + 1024
        );
    }

    #[test]
    fn pipes_count_twice_and_workers_add_overhead() {
        let base = required(&limits(1, 1, 1, 1), 0);
        assert_eq!(base, BASE_DESCRIPTORS + 3 + 2);
        assert_eq!(required(&limits(1, 1, 1, 2), 0), base + 2);
        assert_eq!(
            required(&limits(1, 1, 1, 1), 3),
            base + 3 * WORKER_DESCRIPTORS
        );
    }

    #[test]
    fn required_saturates() {
        assert_eq!(required(&limits(1, 1, 1, 1), usize::MAX), u64::MAX);
    }

    #[test]
    fn plan_keeps_a_sufficient_soft_limit() {
        assert_eq!(plan(100, 100, 100), Ok(None));
        assert_eq!(plan(100, 200, 50), Ok(None));
    }

    #[test]
    fn plan_raises_soft_limit_to_required() {
        assert_eq!(plan(100, 10, 100), Ok(Some(100)));
        assert_eq!(plan(100, 10, u64::MAX), Ok(Some(100)));
    }

    #[test]
    fn plan_rejects_low_hard_limit_naming_the_budgets() {
        let error = plan(40_000, 1024, 4096).unwrap_err();
        assert_eq!(error, message(40_000, 4096));
        assert!(error.contains("hard limit 4096"), "{error}");
        assert!(error.contains("40000 file descriptors"), "{error}");
        for name in [
            "RACER_CLIENT_CONNECTIONS",
            "RACER_PEER_CONNECTIONS",
            "RACER_OUTBOUND_CONNECTIONS",
            "RACER_PIPES",
            "RACER_IO_WORKERS",
        ] {
            assert!(error.contains(name), "{name} missing from {error}");
        }
    }

    #[test]
    fn ensure_accepts_a_tiny_budget() {
        ensure(&limits(1, 1, 1, 1), 1).unwrap();
    }
}
