//! Deterministic simulation backend for [`uring_runtime`].
//!
//! Work in progress: the seeded clock and entropy stream.

pub mod clock;
pub mod entropy;

pub use clock::{DEFAULT_WALL_EPOCH, SimClock};
pub use entropy::{DEFAULT_ENTROPY_DOMAIN, SimEntropy};
