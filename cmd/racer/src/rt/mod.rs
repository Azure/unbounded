//! Test-only thread-local environment and world selection left over from the
//! `uring-runtime` crate. Being replaced by explicit `racer_io` `WorkerCtx`,
//! `Env`, `SimClock`, and `SimOs` fixtures; delete once unused.

pub mod environment;

pub mod reactor;

/// Portable runtime failures from `racer-io`.
pub use racer_io::Error;

/// A runtime result, optionally using an application-owned error type.
pub type Result<T, E = Error> = std::result::Result<T, E>;
