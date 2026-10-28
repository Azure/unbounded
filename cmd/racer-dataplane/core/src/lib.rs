//! Sans-IO Racer decision logic.
//!
//! The production dataplane and `racer-sim` both call this crate. Callers pass time and an explicit
//! RNG ([`time::SplitMix64`]) in; the crate never reads clocks, spawns threads, or reads OS
//! randomness (`tests/purity.rs` and `clippy.toml` enforce this).

pub mod admission;
pub mod cache;
pub mod liveness;
pub mod read;
pub mod route;
pub mod subscriptions;
pub mod time;
