// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Lightweight, dependency-free observability for network request
//! logging.
//!
//! The crate is runtime-agnostic and deliberately avoids pulling in a
//! logging framework (`log`/`tracing`), so this module provides just
//! enough machinery to emit one structured line per network operation:
//! incoming/outgoing fabric RPCs, frontend client requests, and backend
//! origin fetches.
//!
//! A line looks like:
//!
//! ```text
//! 1717795200123 INFO  backend.http op="GET" backend="b" path="/o" off=0 len=4096 pages=1 outcome=ok dur_us=812
//! ```
//!
//! The global level is controlled by the `UNBOUNDED_STORAGE_LOG`
//! environment variable (see [`init_from_env`]). Field formatting is
//! skipped entirely when the relevant level is disabled, so logging is
//! close to free on the hot path when turned off.

use std::fmt::Display;
use std::fmt::Write as _;
use std::future::Future;
use std::io::Write as _;
use std::sync::atomic::AtomicU8;
use std::sync::atomic::Ordering;
use std::time::Instant;
use std::time::SystemTime;
use std::time::UNIX_EPOCH;

/// Severity levels, ordered from least to most verbose. `Off` disables
/// all output.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord, Debug)]
#[repr(u8)]
pub enum Level {
    Off = 0,
    Error = 1,
    Warn = 2,
    Info = 3,
    Debug = 4,
    Trace = 5,
}

impl Level {
    pub fn as_str(self) -> &'static str {
        match self {
            Level::Off => "OFF",
            Level::Error => "ERROR",
            Level::Warn => "WARN",
            Level::Info => "INFO",
            Level::Debug => "DEBUG",
            Level::Trace => "TRACE",
        }
    }

    fn from_u8(value: u8) -> Level {
        match value {
            0 => Level::Off,
            1 => Level::Error,
            2 => Level::Warn,
            3 => Level::Info,
            4 => Level::Debug,
            _ => Level::Trace,
        }
    }

    /// Parse a case-insensitive level name. Returns `None` for unknown
    /// values so callers can decide how to report a bad configuration.
    pub fn parse(text: &str) -> Option<Level> {
        match text.trim().to_ascii_lowercase().as_str() {
            "off" | "none" => Some(Level::Off),
            "error" => Some(Level::Error),
            "warn" | "warning" => Some(Level::Warn),
            "info" => Some(Level::Info),
            "debug" => Some(Level::Debug),
            "trace" => Some(Level::Trace),
            _ => None,
        }
    }
}

/// Environment variable that selects the global log level.
pub const ENV_VAR: &str = "UNBOUNDED_STORAGE_LOG";

/// Number of leading bytes of a 32-byte stripe key rendered by
/// [`ReqLog::hexkey`]. Eight bytes is enough to disambiguate keys in a
/// log without bloating each line.
const HEXKEY_BYTES: usize = 8;

static LEVEL: AtomicU8 = AtomicU8::new(Level::Info as u8);

/// Set the global log level. Visible to all threads.
pub fn set_level(level: Level) {
    LEVEL.store(level as u8, Ordering::Relaxed);
}

/// Current global log level.
pub fn level() -> Level {
    Level::from_u8(LEVEL.load(Ordering::Relaxed))
}

/// Whether a message at `level` would be emitted under the current
/// global level. `Off` messages are never enabled.
pub fn enabled(level: Level) -> bool {
    let want = level as u8;
    want != 0 && want <= LEVEL.load(Ordering::Relaxed)
}

/// Initialize the global level from the `UNBOUNDED_STORAGE_LOG`
/// environment variable. Unset or unparseable values leave the default
/// (`Info`) in place; an unparseable value is reported once to stderr.
pub fn init_from_env() {
    let raw = match std::env::var(ENV_VAR) {
        Ok(v) => v,
        Err(_) => return,
    };
    match Level::parse(&raw) {
        Some(level) => set_level(level),
        None => {
            eprintln!(
                "{ENV_VAR}: unrecognized log level {raw:?}; keeping {}",
                level().as_str()
            );
        }
    }
}

/// Emit a one-off event at `level` with preformatted `fields` (each
/// field is expected to already include its leading space, matching
/// [`ReqLog`] accumulation). Does nothing when the level is disabled.
pub fn event(level: Level, target: &str, fields: &str) {
    if enabled(level) {
        emit(level, target, fields);
    }
}

fn render(level: Level, target: &str, fields: &str) -> String {
    format!("{:<5} {}{}", level.as_str(), target, fields)
}

fn emit(level: Level, target: &str, fields: &str) {
    let line = format!("{} {}\n", unix_millis(), render(level, target, fields));
    let stderr = std::io::stderr();
    let mut lock = stderr.lock();
    let _ = lock.write_all(line.as_bytes());
}

fn unix_millis() -> u128 {
    SystemTime::now()
        .duration_since(UNIX_EPOCH)
        .map(|d| d.as_millis())
        .unwrap_or(0)
}

/// A request-scoped log builder. Accumulate fields over the lifetime of
/// a single network operation, then call [`ReqLog::finish_ok`] or
/// [`ReqLog::finish_err`] to emit one line including the elapsed
/// duration and outcome.
///
/// When logging is disabled (`active == false`) field methods are
/// no-ops and no allocation occurs, so an instrumented hot path pays
/// almost nothing when logging is off.
pub struct ReqLog {
    target: &'static str,
    start: Instant,
    fields: String,
    active: bool,
}

impl ReqLog {
    /// Start a request log for `target` (e.g. `"backend.http"`). The
    /// log is active when at least `Warn` is enabled, because both the
    /// `ok` (`Info`) and `err` (`Warn`) outcomes are gated at or below
    /// that level; finishing re-checks the specific outcome level.
    pub fn new(target: &'static str) -> ReqLog {
        ReqLog {
            target,
            start: Instant::now(),
            fields: String::new(),
            active: enabled(Level::Warn),
        }
    }

    /// Append ` key=value` using `value`'s `Display` formatting.
    pub fn field<T: Display>(&mut self, key: &str, value: T) -> &mut ReqLog {
        if self.active {
            let _ = write!(self.fields, " {key}={value}");
        }

        self
    }

    /// Append ` key="value"` (quoted) for free-form string values.
    pub fn str_field(&mut self, key: &str, value: &str) -> &mut ReqLog {
        if self.active {
            let _ = write!(self.fields, " {key}={value:?}");
        }

        self
    }

    /// Append ` key=<hex>` for the first [`HEXKEY_BYTES`] bytes of a
    /// binary key such as a stripe key.
    pub fn hexkey(&mut self, key: &str, bytes: &[u8]) -> &mut ReqLog {
        if self.active {
            let _ = write!(self.fields, " {key}=");
            for b in bytes.iter().take(HEXKEY_BYTES) {
                let _ = write!(self.fields, "{b:02x}");
            }
        }

        self
    }

    /// Emit a successful-completion line at `Info`. Idempotent: a
    /// second call is a no-op so a stream that finishes explicitly and
    /// is then dropped does not log twice.
    pub fn finish_ok(&mut self) {
        self.finish::<&str>(Level::Info, None);
    }

    /// Emit a failed-completion line at `Warn`, including the error.
    /// Idempotent (see [`ReqLog::finish_ok`]).
    pub fn finish_err<E: Display>(&mut self, err: E) {
        self.finish(Level::Warn, Some(err));
    }

    fn finish<E: Display>(&mut self, level: Level, err: Option<E>) {
        if !self.active {
            return;
        }

        // Prevent a second emission (e.g. explicit finish then Drop).
        self.active = false;

        if !enabled(level) {
            return;
        }

        let dur_us = self.start.elapsed().as_micros();
        let outcome = if err.is_some() { "err" } else { "ok" };
        let _ = write!(self.fields, " outcome={outcome} dur_us={dur_us}");

        if let Some(err) = err {
            let _ = write!(self.fields, " error={:?}", err.to_string());
        }

        emit(level, self.target, &self.fields);
    }
}

/// Drive `fut` to completion, emitting `log` with the appropriate
/// outcome. Convenience wrapper for the common case where a network
/// operation is a single `async fn` returning `Result`.
pub async fn instrument<T, E, F>(mut log: ReqLog, fut: F) -> Result<T, E>
where
    E: Display,
    F: Future<Output = Result<T, E>>,
{
    match fut.await {
        Ok(value) => {
            log.finish_ok();
            Ok(value)
        }
        Err(err) => {
            log.finish_err(&err);
            Err(err)
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn level_parse_is_case_insensitive_and_aliased() {
        assert_eq!(Level::parse("ERROR"), Some(Level::Error));
        assert_eq!(Level::parse("warn"), Some(Level::Warn));
        assert_eq!(Level::parse("WARNING"), Some(Level::Warn));
        assert_eq!(Level::parse(" info "), Some(Level::Info));
        assert_eq!(Level::parse("off"), Some(Level::Off));
        assert_eq!(Level::parse("none"), Some(Level::Off));
        assert_eq!(Level::parse("trace"), Some(Level::Trace));
        assert_eq!(Level::parse("bogus"), None);
    }

    #[test]
    fn enabled_respects_threshold_and_off() {
        set_level(Level::Off);
        assert!(!enabled(Level::Error));
        assert!(!enabled(Level::Trace));

        set_level(Level::Info);
        assert!(enabled(Level::Error));
        assert!(enabled(Level::Warn));
        assert!(enabled(Level::Info));
        assert!(!enabled(Level::Debug));
        assert!(!enabled(Level::Trace));

        set_level(Level::Trace);
        assert!(enabled(Level::Trace));
    }

    #[test]
    fn render_pads_level_and_appends_fields() {
        assert_eq!(
            render(Level::Info, "frontend.http", " status=200"),
            "INFO  frontend.http status=200"
        );
        assert_eq!(
            render(Level::Warn, "fabric.serve", ""),
            "WARN  fabric.serve"
        );
    }

    #[test]
    fn reqlog_fields_accumulate_when_active() {
        set_level(Level::Info);
        let mut log = ReqLog::new("backend.http");
        log.field("len", 4096u64)
            .str_field("path", "/obj")
            .hexkey("stripe", &[0xde, 0xad, 0xbe, 0xef]);
        assert_eq!(log.fields, " len=4096 path=\"/obj\" stripe=deadbeef");
    }

    #[test]
    fn reqlog_hexkey_truncates_to_eight_bytes() {
        set_level(Level::Info);
        let mut log = ReqLog::new("fabric.serve");
        let key: [u8; 32] = [0xab; 32];
        log.hexkey("stripe", &key);
        assert_eq!(log.fields, " stripe=abababababababab");
        assert_eq!(log.fields.len(), " stripe=".len() + HEXKEY_BYTES * 2);
    }

    #[test]
    fn reqlog_is_inert_when_disabled() {
        set_level(Level::Off);
        let mut log = ReqLog::new("backend.s3");
        log.field("len", 1u64).str_field("path", "/x");
        assert!(log.fields.is_empty());
        assert!(!log.active);
    }
}
