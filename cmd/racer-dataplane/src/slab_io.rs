// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Shared logical slab I/O admission. Network and directory I/O are not charged.
use std::{
    cell::RefCell,
    env, io,
    sync::{Arc, Mutex},
    time::{Duration, Instant},
};

const SECOND: u128 = 1_000_000_000;

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
enum Mode {
    Operations,
    Bytes,
}

/// Startup-only token bucket configuration. An absent configuration is unlimited.
#[derive(Clone, Copy, Debug)]
pub struct Config {
    mode: Mode,
    rate: u64,
    burst: u64,
}
impl Config {
    pub fn from_env() -> io::Result<Option<Self>> {
        Self::parse(|name| env::var(name))
    }

    fn parse(
        mut lookup: impl FnMut(&str) -> Result<String, env::VarError>,
    ) -> io::Result<Option<Self>> {
        let mut number = |name| match lookup(name) {
            Err(env::VarError::NotPresent) => Ok(None),
            Ok(value) if !value.is_empty() && value.bytes().all(|b| b.is_ascii_digit()) => value
                .parse::<u64>()
                .ok()
                .filter(|n| *n != 0)
                .map(Some)
                .ok_or_else(|| invalid(name)),
            _ => Err(invalid(name)),
        };
        let ops = number("RACER_SLAB_IOPS")?;
        let bytes = number("RACER_SLAB_BYTES_PER_SEC")?;
        let burst = number("RACER_SLAB_IO_BURST")?;
        let (mode, rate, minimum) = match (ops, bytes) {
            (None, None) if burst.is_none() => return Ok(None),
            (Some(rate), None) => (Mode::Operations, rate, 1),
            (None, Some(rate)) => (Mode::Bytes, rate, crate::buffers::BUFFER_SIZE as u64),
            _ => {
                return Err(invalid(
                    "set exactly one slab rate when configuring a burst",
                ));
            }
        };
        let burst = burst.unwrap_or(rate.max(minimum));
        if burst < minimum {
            return Err(invalid(
                "RACER_SLAB_IO_BURST is smaller than one maximum slab operation",
            ));
        }
        Ok(Some(Self { mode, rate, burst }))
    }
}

fn invalid(message: &str) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidInput, message.to_owned())
}

struct Bucket {
    config: Config,
    state: Mutex<State>,
}
struct State {
    tokens: u128,
    updated: Instant,
    operations: u64,
    bytes: u64,
    waits: u64,
    wait_nanos: u128,
}
impl Bucket {
    fn refill(&self, state: &mut State, now: Instant) {
        let elapsed = now.saturating_duration_since(state.updated).as_nanos();
        state.tokens = state
            .tokens
            .saturating_add(elapsed.saturating_mul(self.config.rate as u128))
            .min(self.config.burst as u128 * SECOND);
        state.updated = now;
    }
}

/// Cloneable slab policy. Copies share tokens and counters across workers/inodes.
#[derive(Clone, Default)]
pub struct Io {
    bucket: Option<Arc<Bucket>>,
    stopped: Option<Arc<dyn Fn() -> bool + Send + Sync>>,
}
thread_local! {
    static SETUP: RefCell<Io> = RefCell::default();
}
impl Io {
    pub fn new(config: Option<Config>) -> Self {
        Self {
            bucket: config.map(|config| {
                Arc::new(Bucket {
                    config,
                    state: Mutex::new(State {
                        tokens: config.burst as u128 * SECOND,
                        updated: crate::environment::now(),
                        operations: 0,
                        bytes: 0,
                        waits: 0,
                        wait_nanos: 0,
                    }),
                })
            }),
            stopped: None,
        }
    }

    pub fn with_stop(mut self, stopped: impl Fn() -> bool + Send + Sync + 'static) -> Self {
        self.stopped = Some(Arc::new(stopped));
        self
    }

    /// Install a policy for slab constructors on this setup thread. Created slab
    /// capabilities retain it after the scope exits, including recovery reads.
    pub fn scope<T>(&self, f: impl FnOnce() -> T) -> T {
        struct Restore(Io);
        impl Drop for Restore {
            fn drop(&mut self) {
                SETUP.with(|s| *s.borrow_mut() = self.0.clone());
            }
        }
        let _restore = Restore(SETUP.with(|s| s.replace(self.clone())));
        f()
    }

    pub(crate) fn current() -> Self {
        SETUP.with(|s| s.borrow().clone())
    }
    pub(crate) fn limited(&self) -> bool {
        self.bucket.is_some()
    }

    /// Reserve before submission; callers must finish even failed/short I/O.
    pub(crate) fn reserve(&self, bytes: usize, now: Instant) -> Result<Charge, Instant> {
        let Some(bucket) = &self.bucket else {
            return Ok(Charge {
                bucket: None,
                bytes,
            });
        };
        let cost = match bucket.config.mode {
            Mode::Operations => 1,
            Mode::Bytes => bytes as u128,
        } * SECOND;
        let mut state = bucket.state.lock().unwrap();
        bucket.refill(&mut state, now);
        if state.tokens < cost {
            let nanos = (cost - state.tokens).div_ceil(bucket.config.rate as u128);
            // Maximum charged operation is 4 MiB, even at one byte/second.
            return Err(now + Duration::from_nanos(nanos.min(u64::MAX as u128) as u64));
        }
        state.tokens -= cost;
        state.operations = state.operations.saturating_add(1);
        Ok(Charge {
            bucket: Some(bucket.clone()),
            bytes,
        })
    }

    pub(crate) fn waited(&self, since: Instant) {
        if let Some(bucket) = &self.bucket {
            let mut state = bucket.state.lock().unwrap();
            state.waits = state.waits.saturating_add(1);
            state.wait_nanos += crate::environment::now()
                .saturating_duration_since(since)
                .as_nanos();
        }
    }

    pub(crate) fn blocking<T>(
        &self,
        bytes: usize,
        f: impl FnOnce() -> io::Result<(T, usize)>,
    ) -> io::Result<T> {
        let start = crate::environment::now();
        let mut waited = false;
        let charge = loop {
            if self.stopped.as_ref().is_some_and(|stop| stop()) {
                if waited {
                    self.waited(start);
                }
                return Err(io::Error::new(
                    io::ErrorKind::Interrupted,
                    "slab I/O setup stopped",
                ));
            }
            match self.reserve(bytes, crate::environment::now()) {
                Ok(charge) => break charge,
                Err(deadline) => {
                    waited = true;
                    std::thread::sleep(
                        deadline
                            .saturating_duration_since(crate::environment::now())
                            .min(Duration::from_millis(10)),
                    );
                }
            }
        };
        if waited {
            self.waited(start);
        }
        let result = f();
        charge.finish(result.as_ref().map_or(0, |(_, n)| *n));
        result.map(|(value, _)| value)
    }

    pub(crate) fn render(&self, out: &mut String) {
        use std::fmt::Write;
        let (operations, bytes, waits, seconds) =
            self.bucket.as_ref().map_or((0, 0, 0, 0.0), |b| {
                let s = b.state.lock().unwrap();
                (
                    s.operations,
                    s.bytes,
                    s.waits,
                    s.wait_nanos as f64 / SECOND as f64,
                )
            });
        for (name, help, value) in [
            (
                "operations_total",
                "Rate-limited logical slab operations submitted (including failures).",
                operations.to_string(),
            ),
            (
                "bytes_total",
                "Completed data bytes through the slab limiter.",
                bytes.to_string(),
            ),
            (
                "waits_total",
                "Slab operations that waited for tokens.",
                waits.to_string(),
            ),
            (
                "wait_seconds_total",
                "Aggregate slab token wait across operations.",
                seconds.to_string(),
            ),
        ] {
            writeln!(out, "# HELP racer_dataplane_slab_io_{name} {help}\n# TYPE racer_dataplane_slab_io_{name} counter\nracer_dataplane_slab_io_{name} {value}").unwrap();
        }
    }
}

pub(crate) struct Charge {
    bucket: Option<Arc<Bucket>>,
    bytes: usize,
}
impl Charge {
    pub(crate) fn finish(self, completed: usize) {
        if let Some(bucket) = self.bucket {
            let mut state = bucket.state.lock().unwrap();
            let completed = completed.min(self.bytes);
            state.bytes = state.bytes.saturating_add(completed as u64);
            if bucket.config.mode == Mode::Bytes {
                bucket.refill(&mut state, crate::environment::now());
                state.tokens = (state.tokens + (self.bytes - completed) as u128 * SECOND)
                    .min(bucket.config.burst as u128 * SECOND);
            }
        }
    }
}

#[cfg(test)]
#[path = "../tests/storage/slab_io.rs"]
mod tests;
