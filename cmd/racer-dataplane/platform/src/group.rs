//! Pinned thread groups, CPU discovery, and Racer's placement policy.
//!
//! The lifecycle engine is the library's [`uring::group`], re-exported here.
//! [`affinity`] adds Racer's lane/helper placement on top of the library's
//! topology discovery, which deliberately chooses no CPUs.

pub use uring::group::*;

pub mod affinity;
