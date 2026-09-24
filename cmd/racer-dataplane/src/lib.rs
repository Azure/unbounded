// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

pub mod allocator;
pub mod breaker;
pub mod buffers;
pub mod cache;
pub mod control;
pub mod crypto;
#[cfg(feature = "dev-bench")]
#[path = "../bench/mod.rs"]
pub mod dev_bench;
mod failure_diagnostics;
pub use failure_diagnostics::initialize as initialize_failure_diagnostics;
pub mod handlers;
pub mod header_value;
pub mod http;
pub mod http_auth;
pub mod http_client;
pub mod http_server;
pub mod lifecycle;
pub mod metadata;
pub mod metrics;
pub mod negotiation;
pub mod origin_data;
pub mod outcome;
pub mod rdma;
pub mod runtime;
pub mod slab_io;
pub mod socket;
mod socket_listener;
pub mod tls;
pub mod tuning;
pub mod uring;
pub mod workers;

// Preserve the crate-root paths for helpers colocated with their owning subsystem.
pub use control::routing;
pub use negotiation::peer_identity;
pub use workers::sharding;

pub(crate) use runtime::environment;

#[cfg(test)]
include!(concat!(env!("CARGO_MANIFEST_DIR"), "/tests/contracts.rs"));

#[path = "../../../internal/racer/product.rs"]
pub mod product;
pub mod topology;

pub(crate) use uring::sys as uring_sys;
