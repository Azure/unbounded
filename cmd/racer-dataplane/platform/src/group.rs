//! Pinned thread groups, CPU discovery, and Racer's placement policy.
//!
//! The lifecycle engine and topology discovery are the library's
//! ([`uring::group`] and [`uring::affinity`], re-exported here). The library
//! deliberately chooses no CPUs; [`affinity::place`] and
//! [`affinity::place_exact`] are Racer's lane/helper placement policy.

pub use uring::group::*;

/// Linux CPU discovery and calling-thread affinity, plus Racer placement.
pub mod affinity {
    pub use uring::affinity::*;
}
