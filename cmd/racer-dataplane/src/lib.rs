// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

pub mod allocator;
pub mod buffers;
pub mod cache;
pub mod control;
pub mod crypto;
pub mod handlers;
pub mod http_auth;
pub mod http_client;
pub mod http_server;
pub mod lifecycle;
mod listener_policy;
pub mod metadata;
pub mod metrics;
pub mod negotiation;
pub mod rdma;
pub mod runtime;
pub mod slab_io;
pub mod tls;
pub mod uring;
pub mod workers;

// Preserve the crate-root paths for helpers colocated with their owning subsystem.
pub use control::routing;
pub use http_client::{breaker, http};
pub use negotiation::peer_identity;
pub use workers::sharding;

pub(crate) use runtime::environment;
#[cfg(test)]
#[path = "../tests/support/simulation.rs"]
pub(crate) mod simulation;

#[cfg(test)]
include!(concat!(env!("CARGO_MANIFEST_DIR"), "/tests/contracts.rs"));

pub mod topology;

pub(crate) use uring::sys as uring_sys;
