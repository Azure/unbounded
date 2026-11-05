//! Racer's worker-owned dataplane and its composition boundaries.
//!
//! Constructors assemble dependencies without operational side effects. Worker
//! lifecycle methods activate reactor-owned I/O, authenticated control and peer
//! transports, encrypted storage, and bounded read coordination.

#![deny(unsafe_op_in_unsafe_fn)]

// REST loopback fixtures are shared by unit and integration tests and name `racer::`.
#[cfg(test)]
extern crate self as racer;

pub mod admission;
pub mod affinity;
pub mod app;
pub mod client;
pub mod config;
pub mod control;
pub mod enrollment;
pub mod error;
pub mod fd_limit;
pub mod gpu;
pub mod gpu_landing;
pub mod gpu_slot;
pub mod gpu_telemetry;
pub mod hardware;
pub mod http;
pub mod lifecycle;
pub mod memory;
pub mod model;
pub mod origin;
pub mod peer;
pub mod prefetch;
pub mod priority;
pub mod profiling;
pub mod rdma;
pub mod read;
pub mod remote;
pub mod retention;
#[doc(hidden)]
pub mod rt;
pub mod runtime;
pub mod security;
pub mod store;
pub mod telemetry;
pub mod topology;
pub mod uds_endpoint;
pub mod worker;

#[cfg(feature = "subscription-interop")]
mod subscription_interop;

// The integration-test launcher needs one opt-in entry point, not a public fixture module.
#[cfg(feature = "subscription-interop")]
#[doc(hidden)]
pub use subscription_interop::go_sdk_subscription_server as run_subscription_interop_fixture;

pub(crate) mod pressure;

#[cfg(test)]
pub(crate) mod test_support;
