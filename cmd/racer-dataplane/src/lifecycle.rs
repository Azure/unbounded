//! Process lifecycle policy. The watchdog never frees worker-owned resources.
use std::{
    io,
    sync::{
        Arc, OnceLock,
        atomic::{AtomicBool, AtomicU64, Ordering},
    },
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};

pub const HEARTBEAT: Duration = Duration::from_millis(250);

#[derive(Clone, Copy)]
pub struct Config {
    pub startup: Duration,
    pub stall: Duration,
    pub drain: Duration,
    pub quiesce: Duration,
}
impl Default for Config {
    fn default() -> Self {
        Self {
            startup: Duration::from_secs(90),
            stall: Duration::from_secs(5),
            drain: Duration::from_secs(20),
            quiesce: Duration::from_secs(5),
        }
    }
}
impl Config {
    pub fn from_env() -> io::Result<Self> {
        Self::from_lookup(|name| std::env::var(name))
    }
    fn from_lookup(
        mut lookup: impl FnMut(&str) -> Result<String, std::env::VarError>,
    ) -> io::Result<Self> {
        let mut seconds = |name: &str, default: u64| -> io::Result<Duration> {
            let value = match lookup(name) {
                Ok(v) => v.parse::<u64>().ok(),
                Err(std::env::VarError::NotPresent) => Some(default),
                Err(_) => None,
            }
            .filter(|v| (1..=3600).contains(v))
            .ok_or_else(|| {
                io::Error::new(
                    io::ErrorKind::InvalidInput,
                    format!("{name} must be 1..=3600 seconds"),
                )
            })?;
            Ok(Duration::from_secs(value))
        };
        Ok(Self {
            startup: seconds("RACER_STARTUP_SECONDS", 90)?,
            stall: seconds("RACER_STALL_SECONDS", 5)?,
            drain: seconds("RACER_DRAIN_SECONDS", 20)?,
            quiesce: seconds("RACER_QUIESCE_SECONDS", 5)?,
        })
    }
}

pub struct Lifecycle {
    config: Config,
    began: Instant,
    workers: OnceLock<Vec<AtomicU64>>,
    started: AtomicBool,
    stopping: OnceLock<Instant>,
}
impl Lifecycle {
    pub fn new(config: Config) -> Self {
        Self {
            config,
            began: Instant::now(),
            workers: OnceLock::new(),
            started: AtomicBool::new(false),
            stopping: OnceLock::new(),
        }
    }
    pub fn configure_workers(&self, count: usize) {
        assert!(count > 0);
        assert!(
            self.workers
                .set((0..count).map(|_| AtomicU64::new(0)).collect())
                .is_ok()
        );
    }
    pub fn progress(&self, worker: usize) {
        self.progress_at(worker, Instant::now());
    }
    fn progress_at(&self, worker: usize, now: Instant) {
        self.workers.get().expect("worker health configured")[worker]
            .store(self.tick(now), Ordering::Release);
    }
    fn tick(&self, now: Instant) -> u64 {
        now.saturating_duration_since(self.began).as_millis() as u64 + 1
    }
    fn fresh_at(&self, now: Instant) -> bool {
        self.workers.get().is_some_and(|workers| {
            workers.iter().all(|worker| {
                let last = worker.load(Ordering::Acquire);
                last != 0
                    && self.tick(now).saturating_sub(last) < self.config.stall.as_millis() as u64
            })
        })
    }
    pub fn healthy(&self) -> bool {
        !self.draining() && self.fresh_at(Instant::now())
    }
    pub fn draining(&self) -> bool {
        self.stopping.get().is_some()
    }
    pub fn begin_shutdown(&self) {
        let now = Instant::now();
        if !self.draining() && self.fresh_at(now) {
            self.started.store(true, Ordering::Release);
        }
        self.begin_at(now);
    }
    fn begin_at(&self, now: Instant) {
        self.stopping.get_or_init(|| now);
    }
    fn deadlines(&self, now: Instant) -> (bool, bool) {
        if !self.draining() && self.fresh_at(now) {
            self.started.store(true, Ordering::Release);
        }
        if (!self.started.load(Ordering::Acquire)
            && now.duration_since(self.began) >= self.config.startup)
            || (self.started.load(Ordering::Acquire) && !self.fresh_at(now))
        {
            self.begin_at(now);
        }
        self.stopping.get().map_or((false, false), |start| {
            let elapsed = now.saturating_duration_since(*start);
            let drain = if self.started.load(Ordering::Acquire) {
                self.config.drain
            } else {
                Duration::ZERO
            };
            (elapsed >= drain, elapsed >= drain + self.config.quiesce)
        })
    }
}

/// Install before filesystem, provider, compute or worker setup. A blocked Rust
/// thread cannot be safely cancelled. At the hard deadline exit_group via _exit
/// skips destructors; the kernel owns process/device teardown. This is not proof
/// of device quiescence, nor a bound on an uninterruptible kernel/failed host.
pub struct Monitor {
    done: Arc<AtomicBool>,
    thread: Option<JoinHandle<()>>,
}
impl Monitor {
    pub fn start(
        life: Arc<Lifecycle>,
        stop: crate::workers::StopHandle,
        signal: &'static AtomicBool,
    ) -> io::Result<Self> {
        let done = Arc::new(AtomicBool::new(false));
        let finished = done.clone();
        let thread = thread::Builder::new()
            .name("racer-lifecycle".into())
            .spawn(move || {
                while !finished.load(Ordering::Acquire) {
                    if signal.load(Ordering::Relaxed) || stop.is_stopping() {
                        life.begin_shutdown();
                    }
                    let (cancel, exit) = life.deadlines(Instant::now());
                    if exit {
                        // SAFETY: process termination deliberately performs no user cleanup.
                        unsafe { libc::_exit(124) }
                    }
                    if cancel {
                        stop.request_stop();
                    }
                    thread::park_timeout(Duration::from_millis(25));
                }
            })?;
        Ok(Self {
            done,
            thread: Some(thread),
        })
    }
}
impl Drop for Monitor {
    fn drop(&mut self) {
        self.done.store(true, Ordering::Release);
        if let Some(thread) = self.thread.take() {
            thread.thread().unpark();
            let _ = thread.join();
        }
    }
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/execution/lifecycle.rs"
));
