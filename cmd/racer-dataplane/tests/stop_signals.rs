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

/// Control fake that never bootstraps, as during an API outage: like the
/// embedded handle it blocks for the whole requested wait, then times out.
struct Outage {
    attempts: AtomicUsize,
    longest: std::sync::Mutex<Duration>,
    signal: libc::c_int,
}

impl ClusterControl for Outage {
    fn bootstrap(&self, timeout: Duration) -> Result<Bootstrapped, BootstrapError> {
        {
            let mut longest = self.longest.lock().unwrap();
            *longest = (*longest).max(timeout);
        }
        if self.attempts.fetch_add(1, Ordering::Relaxed) == 1 {
            // SAFETY: raise delivers to this test process only; handlers are installed.
            unsafe { libc::raise(self.signal) };
        }
        std::thread::sleep(timeout);
        Err(BootstrapError::Pending)
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
        longest: std::sync::Mutex::new(Duration::ZERO),
        signal,
    });
    let mut config = Config::from_lookup(|_| Ok(None)).expect("managed config");
    assert!(config.managed());
    let started = Instant::now();
    let result = managed::start(&mut config, control.clone(), &|| signals.requested());
    assert_eq!(result, Err(Error::Cancelled));
    // The stop arrives inside a blocking wait; it must end well before one
    // full 30 s bootstrap wait, within a couple of short polls.
    assert!(
        started.elapsed() < Duration::from_secs(2),
        "bounded stop: {:?}",
        started.elapsed()
    );
    assert!(*control.longest.lock().unwrap() <= Duration::from_millis(100));
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

/// Real binary in managed mode against an unreachable API server: a stop signal
/// during the bootstrap wait exits cleanly well inside the 30 s grace period.
#[test]
fn binary_exits_cleanly_on_signal_during_bootstrap() {
    for signal in [libc::SIGTERM, libc::SIGINT] {
        let root = std::path::PathBuf::from(env!("CARGO_TARGET_TMPDIR"))
            .join(format!("stop-signals-{}-{signal}", std::process::id()));
        let _ = std::fs::remove_dir_all(&root);
        std::fs::create_dir_all(&root).unwrap();
        let mut params = rcgen::CertificateParams::new(Vec::<String>::new()).unwrap();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        let key = rcgen::KeyPair::generate().unwrap();
        let ca = params.self_signed(&key).unwrap();
        std::fs::write(root.join("ca.crt"), ca.pem()).unwrap();
        std::fs::write(root.join("token"), "token").unwrap();
        // Held as the API server: its first accepted connection shows startup
        // passed handler installation and embedded control is running.
        let api_listener = std::net::TcpListener::bind("127.0.0.1:0").unwrap();
        api_listener.set_nonblocking(true).unwrap();
        let api = api_listener.local_addr().unwrap();
        let mut command = std::process::Command::new(env!("CARGO_BIN_EXE_racer-dataplane"));
        for (key, _) in std::env::vars_os() {
            let key_bytes = key.as_encoded_bytes();
            if key_bytes.starts_with(b"RACER_") || key_bytes.starts_with(b"KUBERNETES_") {
                command.env_remove(key);
            }
        }
        command
            .envs([
                ("RACER_NAMESPACE", "racer"),
                ("RACER_POD_NAME", "racer-0"),
                ("RACER_POD_UID", "00000000-0000-4000-8000-000000000001"),
                ("RACER_NODE_NAME", "node-0"),
                ("RACER_POD_IP", "127.0.0.1"),
                ("RACER_CONTROL_LISTEN", "127.0.0.1:0"),
                ("RACER_WEBHOOK_LISTEN", "127.0.0.1:0"),
                ("KUBERNETES_SERVICE_HOST", "127.0.0.1"),
            ])
            .env("KUBERNETES_SERVICE_PORT", api.port().to_string())
            .env("RACER_API_CA", root.join("ca.crt"))
            .env("RACER_API_TOKEN", root.join("token"))
            .env("RACER_CONTROL_TOKEN", root.join("token"))
            .env("RACER_IDENTITY_DIRECTORY", root.join("identity"))
            .env("RACER_SLAB_DIRECTORY", root.join("slabs"))
            .stdout(std::process::Stdio::null())
            .stderr(std::process::Stdio::piped());
        let mut child = command.spawn().unwrap();
        let waiting = Instant::now();
        loop {
            match api_listener.accept() {
                Ok((connection, _)) => {
                    drop(connection);
                    break;
                }
                Err(error) if error.kind() == std::io::ErrorKind::WouldBlock => {}
                Err(error) => panic!("accept: {error}"),
            }
            if let Some(status) = child.try_wait().unwrap() {
                panic!("exited before contacting the API: {status:?}");
            }
            if waiting.elapsed() > Duration::from_secs(20) {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("no API connection");
            }
            std::thread::sleep(Duration::from_millis(10));
        }
        // Later attempts are refused, keeping bootstrap unavailable.
        drop(api_listener);
        assert!(child.try_wait().unwrap().is_none(), "still bootstrapping");
        // SAFETY: signal only the spawned child.
        unsafe { libc::kill(child.id() as libc::pid_t, signal) };
        let signaled = Instant::now();
        let status = loop {
            if let Some(status) = child.try_wait().unwrap() {
                break status;
            }
            if signaled.elapsed() > Duration::from_secs(20) {
                child.kill().unwrap();
                child.wait().unwrap();
                panic!("no exit after signal {signal}");
            }
            std::thread::sleep(Duration::from_millis(20));
        };
        let mut stderr = String::new();
        std::io::Read::read_to_string(&mut child.stderr.take().unwrap(), &mut stderr).unwrap();
        assert!(status.success(), "signal {signal}: {status:?}\n{stderr}");
        assert!(stderr.contains("stopped by signal"), "{stderr}");
        assert!(
            signaled.elapsed() < Duration::from_secs(15),
            "{:?}",
            signaled.elapsed()
        );
        let _ = std::fs::remove_dir_all(&root);
    }
}
