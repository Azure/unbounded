//! Racer's worker-owned dataplane and its composition boundaries.
//!
//! Constructors assemble dependencies only. Operational entry points return
//! `Error::Unimplemented` until their contracts are implemented. In particular,
//! no scaffold operation authenticates, decrypts, publishes, or reports readiness.

// Ownership fields document the final graph before operations consume them.
#![allow(dead_code)]
#![deny(unsafe_op_in_unsafe_fn)]

pub mod app;
pub mod client;
pub mod config;
pub mod control;
pub mod error;
pub mod http;
pub mod memory;
pub mod model;
pub mod origin;
pub mod peer;
pub mod rdma;
pub mod read;
pub mod runtime;
pub mod security;
pub mod store;
pub mod telemetry;
pub mod topology;

#[cfg(test)]
pub(crate) mod test_support;
