//! Sans-IO Racer decision logic.
//!
//! The production dataplane and `racer-dst` both call this crate. Callers pass time and an explicit
//! RNG ([`time::SplitMix64`]) in; the crate never reads clocks, spawns threads, or reads OS
//! randomness (`tests/purity.rs` and `clippy.toml` enforce this).

pub mod admission;
pub mod cache;
pub mod connections;
/// Pure flow-control mechanisms (re-exported from `racer-flow-core`). The former core -> wire ->
/// http1 -> flow-control edge is gone now that core depends on the pure `racer-wire`, so the
/// crate can fold into this module.
pub use racer_flow_core as flow;
pub mod liveness;
pub mod prefetch;
pub mod read;
pub mod route;
pub mod subscriptions;
pub mod time;
