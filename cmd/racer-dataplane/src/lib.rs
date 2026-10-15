//! Racer's worker-owned dataplane and its composition boundaries.
//!
//! Constructors assemble dependencies without operational side effects. Worker
//! lifecycle methods activate reactor-owned I/O, authenticated control and peer
//! transports, encrypted storage, and bounded read coordination.

#![deny(unsafe_op_in_unsafe_fn)]

pub mod admission;
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
pub mod profiling;
pub mod rdma;
pub mod read;
pub mod retention;
pub mod runtime;
pub mod security;
pub mod store;
pub mod telemetry;
pub mod topology;
pub mod worker;

#[cfg(feature = "subscription-interop")]
mod subscription_interop;

// The integration-test launcher needs one opt-in entry point, not a public fixture module.
#[cfg(feature = "subscription-interop")]
#[doc(hidden)]
pub use subscription_interop::go_sdk_subscription_server as run_subscription_interop_fixture;

#[cfg(test)]
mod contention;

#[cfg(test)]
pub(crate) mod test_support;
