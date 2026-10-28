//! Sans-IO Racer decision logic.
//!
//! This crate holds the parts of Racer that decide what to do, without doing
//! any I/O. The production dataplane and the `racer-sim` simulator both call
//! it. Callers pass time in and pass randomness in as an explicit RNG (see
//! [`time::SplitMix64`]). The crate never reads clocks,
//! spawns threads, or reads OS randomness. `tests/purity.rs` and
//! `clippy.toml` enforce this.

pub mod admission;
pub mod cache;
pub mod liveness;
pub mod read;
pub mod route;
pub mod subscriptions;
pub mod time;
