//! Process-global stop handler lifetime across managed bootstrap and the application.
//!
//! Signal handlers are process-wide, so this binary holds a single test that runs
//! its phases sequentially in its own process.

use racer_dataplane::app::StopSignals;
use racer_dataplane::config::Config;
use racer_dataplane::control::managed::{self, BootstrapError, Bootstrapped, ClusterControl};
use racer_dataplane::error::Error;
use std::sync::Arc;
use std::sync::atomic::{AtomicUsize, Ordering};
use std::time::{Duration, Instant};

/// Control fake that never bootstraps, as during an API outage.
struct Outage {
    attempts: AtomicUsize,
    signal: libc::c_int,
}

impl ClusterControl for Outage {
    fn bootstrap(&self, _: Duration) -> Result<Bootstrapped, BootstrapError> {
        if self.attempts.fetch_add(1, Ordering::Relaxed) == 1 {
            // SAFETY: raise delivers to this test process only; handlers are installed.
            unsafe { libc::raise(self.signal) };
        }
        Err(BootstrapError::Retry)
    }

    fn endpoints(&self) -> wire_codec::DiscoveredEndpoints {
        Default::default()
    }

    fn report(&self, _: u32, _: Vec<racer_control_wire::RailMapping>) {}
}

fn bootstrap_stops_on(signal: libc::c_int) {
    let signals = StopSignals::install().expect("install");
    assert!(!signals.requested(), "first install clears stale stop");
    let control = Arc::new(Outage {
        attempts: AtomicUsize::new(0),
        signal,
    });
    let mut config = Config::from_lookup(|_| Ok(None)).expect("managed config");
    assert!(config.managed());
    let started = Instant::now();
    let result = managed::start(&mut config, control.clone(), &|| signals.requested());
    assert_eq!(result, Err(Error::Cancelled));
    assert!(started.elapsed() < Duration::from_secs(10), "bounded stop");
    assert!(signals.requested());

    // The application takes its own guard while the entry point still holds one;
    // the pending stop must survive that transition.
    let nested = StopSignals::install().expect("nested install");
    assert!(nested.requested(), "nested install keeps pending stop");
    drop(nested);
    assert!(signals.requested());
    drop(control);
    drop(signals);
}

#[test]
fn stop_signals_span_bootstrap_and_application() {
    bootstrap_stops_on(libc::SIGTERM);
    // Last guard dropped: a fresh standalone install starts clean.
    bootstrap_stops_on(libc::SIGINT);
    let standalone = StopSignals::install().expect("standalone install");
    assert!(!standalone.requested());
}
