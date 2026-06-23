// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! On-demand CPU profiling for the daemon.
//!
//! This module is compiled only under the `profiling` cargo feature so
//! that standard builds stay dependency-free. When enabled, the daemon
//! installs a SIGUSR1 handler; on each SIGUSR1 it samples on-CPU time
//! for a fixed window using `pprof-rs` and writes a pprof protobuf file
//! to a configured directory. The file can be inspected with
//! `go tool pprof` or `pprof -http`.
//!
//! Sampling is on-CPU only (pprof-rs drives it from SIGPROF / an
//! interval timer), which matches the daemon's busy-poll shard and
//! fabric threads where the dominant cost lives.

use std::path::PathBuf;
use std::sync::atomic::{AtomicBool, Ordering};
use std::time::{Duration, SystemTime, UNIX_EPOCH};

/// Directory the pprof file is written to. Defaults to the current
/// working directory.
pub const ENV_DIR: &str = "UNBOUNDED_STORAGE_PROFILE_DIR";
/// Sampling frequency in Hz. Defaults to [`DEFAULT_FREQUENCY_HZ`].
pub const ENV_HZ: &str = "UNBOUNDED_STORAGE_PROFILE_HZ";
/// Length of each capture window in seconds. Defaults to
/// [`DEFAULT_WINDOW_SECONDS`].
pub const ENV_SECONDS: &str = "UNBOUNDED_STORAGE_PROFILE_SECONDS";

const DEFAULT_FREQUENCY_HZ: i32 = 99;
const DEFAULT_WINDOW_SECONDS: u64 = 30;

/// Frames matched against this prefix list are folded out of reports so
/// the runtime/libc scaffolding does not dominate the flamegraph.
const BLOCKLIST: &[&str] = &["libc", "libgcc", "pthread", "vdso"];

/// Set by the SIGUSR1 handler; consumed by the profiling thread.
static REQUESTED: AtomicBool = AtomicBool::new(false);

/// Startup-fixed profiling settings, parsed once from the environment.
#[derive(Debug, Clone, PartialEq, Eq)]
pub struct ProfilingConfig {
    dir: PathBuf,
    frequency_hz: i32,
    window: Duration,
}

impl ProfilingConfig {
    /// Build the config from process environment variables. Missing
    /// variables fall back to defaults; malformed values are an error.
    pub fn from_env() -> Result<Self, String> {
        Self::from_parts(
            std::env::var(ENV_DIR).ok(),
            std::env::var(ENV_HZ).ok(),
            std::env::var(ENV_SECONDS).ok(),
        )
    }

    /// Pure constructor used by [`from_env`](Self::from_env) and by the
    /// unit tests. Kept free of `std::env` access so config parsing is
    /// deterministically testable without mutating global process
    /// state.
    fn from_parts(
        dir: Option<String>,
        hz: Option<String>,
        seconds: Option<String>,
    ) -> Result<Self, String> {
        let dir = PathBuf::from(
            dir.filter(|s| !s.is_empty())
                .unwrap_or_else(|| ".".to_string()),
        );

        let frequency_hz = match hz {
            None => DEFAULT_FREQUENCY_HZ,
            Some(s) => {
                let v: i32 = s
                    .trim()
                    .parse()
                    .map_err(|_| format!("{ENV_HZ}: not an integer: {s:?}"))?;
                if v <= 0 {
                    return Err(format!("{ENV_HZ}: must be positive, got {v}"));
                }
                v
            }
        };

        let window_secs = match seconds {
            None => DEFAULT_WINDOW_SECONDS,
            Some(s) => {
                let v: u64 = s
                    .trim()
                    .parse()
                    .map_err(|_| format!("{ENV_SECONDS}: not an integer: {s:?}"))?;
                if v == 0 {
                    return Err(format!("{ENV_SECONDS}: must be positive, got {v}"));
                }
                v
            }
        };

        Ok(Self {
            dir,
            frequency_hz,
            window: Duration::from_secs(window_secs),
        })
    }
}

/// Install the SIGUSR1 handler that requests a capture. The handler is
/// async-signal-safe (only a relaxed atomic store); the profiling
/// thread observes the flag via its poll loop. SIGUSR1 is distinct from
/// the SIGINT/SIGTERM shutdown signals handled in `main`, so there is no
/// conflict.
pub fn install_signal_handler() {
    unsafe extern "C" fn handler(_sig: libc::c_int) {
        // SAFETY: AtomicBool::store with Relaxed compiles to a single
        // machine store on every supported arch; it is
        // async-signal-safe.
        REQUESTED.store(true, Ordering::Relaxed);
    }
    // SAFETY: sigaction is invoked once at startup with a
    // zero-initialized sigaction whose handler does not touch any
    // non-async-signal-safe state.
    unsafe {
        let mut sa: libc::sigaction = std::mem::zeroed();
        sa.sa_sigaction = handler as *const () as usize;
        libc::sigemptyset(&mut sa.sa_mask);
        sa.sa_flags = 0;
        if libc::sigaction(libc::SIGUSR1, &sa, std::ptr::null_mut()) != 0 {
            let e = std::io::Error::last_os_error();
            eprintln!("profiling: failed to install SIGUSR1 handler: {e}");
        }
    }
}

/// Spawn the background profiling thread. It is a normal, unpinned
/// `std::thread` (not a hot shard core): it sleeps in 100ms increments,
/// runs a capture whenever SIGUSR1 has set the request flag, and exits
/// once `shutdown` returns true. `shutdown` is polled from the daemon's
/// process-wide shutdown flag.
pub fn spawn<F>(cfg: ProfilingConfig, shutdown: F)
where
    F: Fn() -> bool + Send + 'static,
{
    let spawned = std::thread::Builder::new()
        .name("profiling".to_string())
        .spawn(move || {
            eprintln!(
                "profiling: armed; send SIGUSR1 to pid {} to capture a {}s CPU profile at {}Hz into {}",
                std::process::id(),
                cfg.window.as_secs(),
                cfg.frequency_hz,
                cfg.dir.display(),
            );
            while !shutdown() {
                if REQUESTED.swap(false, Ordering::Relaxed) {
                    match capture(&cfg) {
                        Ok(path) => eprintln!("profiling: wrote {path}"),
                        Err(e) => eprintln!("profiling: capture failed: {e}"),
                    }
                    continue;
                }
                std::thread::sleep(Duration::from_millis(100));
            }
        });

    if let Err(e) = spawned {
        eprintln!("profiling: failed to spawn profiling thread: {e}");
    }
}

/// Run one capture: sample for `cfg.window`, encode a pprof protobuf,
/// and write it to `cfg.dir`. Returns the path written on success.
fn capture(cfg: &ProfilingConfig) -> Result<String, String> {
    use pprof::protos::Message;

    let guard = pprof::ProfilerGuardBuilder::default()
        .frequency(cfg.frequency_hz)
        .blocklist(BLOCKLIST)
        .build()
        .map_err(|e| format!("start profiler: {e}"))?;

    std::thread::sleep(cfg.window);

    let report = guard
        .report()
        .build()
        .map_err(|e| format!("build report: {e}"))?;
    let profile = report.pprof().map_err(|e| format!("encode pprof: {e}"))?;

    let buf = profile
        .write_to_bytes()
        .map_err(|e| format!("serialize pprof: {e}"))?;

    std::fs::create_dir_all(&cfg.dir).map_err(|e| format!("create {}: {e}", cfg.dir.display()))?;

    let millis = SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or(0);
    let path = cfg.dir.join(format!(
        "unbounded-storage-{}-{millis}.pb",
        std::process::id()
    ));

    std::fs::write(&path, &buf).map_err(|e| format!("write {}: {e}", path.display()))?;

    Ok(path.display().to_string())
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn defaults_apply_when_unset() {
        let cfg = ProfilingConfig::from_parts(None, None, None).unwrap();
        assert_eq!(cfg.dir, PathBuf::from("."));
        assert_eq!(cfg.frequency_hz, DEFAULT_FREQUENCY_HZ);
        assert_eq!(cfg.window, Duration::from_secs(DEFAULT_WINDOW_SECONDS));
    }

    #[test]
    fn empty_dir_falls_back_to_cwd() {
        let cfg = ProfilingConfig::from_parts(Some(String::new()), None, None).unwrap();
        assert_eq!(cfg.dir, PathBuf::from("."));
    }

    #[test]
    fn overrides_are_parsed() {
        let cfg = ProfilingConfig::from_parts(
            Some("/var/profiles".to_string()),
            Some("250".to_string()),
            Some("5".to_string()),
        )
        .unwrap();
        assert_eq!(cfg.dir, PathBuf::from("/var/profiles"));
        assert_eq!(cfg.frequency_hz, 250);
        assert_eq!(cfg.window, Duration::from_secs(5));
    }

    #[test]
    fn whitespace_is_trimmed() {
        let cfg =
            ProfilingConfig::from_parts(None, Some(" 100 ".to_string()), Some(" 7 ".to_string()))
                .unwrap();
        assert_eq!(cfg.frequency_hz, 100);
        assert_eq!(cfg.window, Duration::from_secs(7));
    }

    #[test]
    fn non_integer_hz_is_rejected() {
        let err = ProfilingConfig::from_parts(None, Some("fast".to_string()), None).unwrap_err();
        assert!(err.contains(ENV_HZ), "{err}");
    }

    #[test]
    fn non_positive_hz_is_rejected() {
        assert!(ProfilingConfig::from_parts(None, Some("0".to_string()), None).is_err());
        assert!(ProfilingConfig::from_parts(None, Some("-5".to_string()), None).is_err());
    }

    #[test]
    fn non_integer_seconds_is_rejected() {
        let err = ProfilingConfig::from_parts(None, None, Some("soon".to_string())).unwrap_err();
        assert!(err.contains(ENV_SECONDS), "{err}");
    }

    #[test]
    fn zero_seconds_is_rejected() {
        assert!(ProfilingConfig::from_parts(None, None, Some("0".to_string())).is_err());
    }
}
