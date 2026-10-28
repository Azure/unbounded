//! Racer control plane that runs inside every racer-dataplane Pod.
//!
//! There is no separate controller Deployment. Each dataplane Pod starts this
//! runtime before dataplane enrollment. Pods compete for a small number of
//! seat Leases (`racer-seat-0` to `racer-seat-15`). Seat holders serve the
//! control API (`/v1/bootstrap`, `/v1/snapshot`, `/v1/keyring`) and the object
//! sidecar admission webhook. One seat holder also holds the `racer-primary`
//! Lease. Only the primary watches Nodes, Pods, ClusterCaches and seat Leases,
//! builds publications, and writes the `racer-state` Secret.
//!
//! # Durable state
//!
//! The operator creates the `racer-state` Secret empty. Its UID is the cluster
//! ID. The primary stores counters, content hashes, the cache keyring, trust
//! roots, retired roots, and a writer fence in it. Publications do not fit in
//! a Secret, so seats copy the current image from the primary and accept it
//! only when its hash matches the committed record.
//!
//! # Fencing
//!
//! A resourceVersion CAS alone is not leader fencing. Each write records the
//! primary term (primary Lease UID and `leaseTransitions`) and the writer Pod
//! UID. A writer rejects state from a newer term, and never retries a conflict
//! after another writer has committed. A writer checks its Lease is still
//! fresh, by the local monotonic clock, before every commit.
//!
//! # Threads
//!
//! [`start`] spawns exactly one thread, see [`CONTROL_THREADS`]. It runs a
//! small single-threaded executor over `poll(2)`. There is no Tokio and no
//! thread pool. Connection and task counts are fixed by [`limits`].
//!
//! # Discovery annotations
//!
//! See [`annotations`] for exact names shared with manifests and clients.

#![deny(unsafe_op_in_unsafe_fn)]

pub mod annotations;
pub mod apiserver;
mod auth;
pub mod authority;
mod config;
mod control;
pub mod entropy;
mod exec;
mod http;
mod kube;
mod lease;
pub mod limits;
mod pki;
mod primary;
mod seat;
mod server;
mod shared;
pub mod step;
#[cfg(test)]
mod tests;

pub use config::{Config, Rotation, SeatPolicy};
pub use control::{Bootstrap, Diagnostics, Handle, Role, start};
pub use shared::PrimaryView;

/// Threads started by [`start`]. The dataplane counts this against its budget.
pub const CONTROL_THREADS: usize = 1;

pub use wire_codec::{DiscoveredEndpoint, DiscoveredEndpoints};

/// Payload-free runtime failures.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Error {
    /// The environment or a value is invalid.
    InvalidConfiguration,

    /// A required resource is not ready yet. Retry.
    Unavailable,

    /// A caller deadline passed.
    Timeout,

    /// The runtime stopped.
    Stopped,

    /// Durable state is invalid. The runtime fails closed.
    InvalidState,

    /// Local I/O failed.
    Io,
}

impl std::fmt::Display for Error {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(f, "{self:?}")
    }
}

impl std::error::Error for Error {}
