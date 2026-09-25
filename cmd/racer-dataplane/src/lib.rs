//! Racer's worker-owned dataplane and its composition boundaries.
//!
//! Constructors assemble dependencies without operational side effects. Worker
//! lifecycle methods activate reactor-owned I/O, authenticated control and peer
//! transports, encrypted storage, and bounded read coordination.

// Component APIs also expose lifecycle and diagnostic hooks to embedders.
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
