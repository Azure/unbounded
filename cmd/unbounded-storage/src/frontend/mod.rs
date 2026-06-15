// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(async_fn_in_trait)]

//! Client-facing tier (an HTTP server) that streams bufferpool pages
//! out to workloads.
//!
//! This module is the symmetric twin of [`crate::backend`]: where a
//! `Backend` *fetches* bytes from an origin into bufferpool pages, a
//! frontend *serves* bufferpool pages out to a workload.
//!
//! A concrete frontend ([`HttpFrontend`], [`S3Frontend`]) is built once
//! per node from a [`FrontendSpec`](crate::config::FrontendSpec) via its
//! inherent `from_spec`, then bound per shard with `bind_listener` (using
//! `SO_REUSEPORT`, so every shard accepts the same port). The per-shard
//! [`HttpDriver`]/[`S3Driver`] it produces is advanced by the shard
//! [`ShardLoop`](crate::runtime::ShardLoop) via a tick hook
//! (`loop.add_tick_hook(move || driver.progress())`). A tick hook rather
//! than a single long-lived future is the right seam because a frontend
//! multiplexes *many* connection futures whose lifetimes do not line up
//! with one `poll`; the driver owns that connection set internally and
//! the loop just keeps poking it.
//!
//! The live, hot-reloadable set of per-shard frontend drivers is owned by
//! `main.rs`'s `FrontendRegistry`, which implements
//! [`config::reconcile::FrontendReconcileTarget`](crate::config::reconcile)
//! so the config reconcile loop can add/remove frontends in place. This
//! module only provides the concrete frontend/driver types and their
//! shared error and range/XML helpers.

mod range;
mod s3_xml;

#[cfg(target_os = "linux")]
mod http_serve;

#[cfg(target_os = "linux")]
mod s3_serve;

#[cfg(target_os = "linux")]
mod serve_metrics;

pub use range::{ByteRange, RangeError, ResolvedRange, StripeSlice, full_object, stripe_set};
pub use s3_xml::{S3ErrorCode, error_xml, xml_escape};

#[cfg(target_os = "linux")]
pub use http_serve::{HttpDriver, HttpFrontend};

#[cfg(target_os = "linux")]
pub use s3_serve::{S3Driver, S3Frontend};

/// Errors building or starting a frontend. Mirrors the
/// `&'static str`-flavored, cheaply-cloneable style of
/// [`crate::bufferpool::Error`] so it threads through the reconcile
/// path without boxing.
#[derive(Clone, Debug, PartialEq, Eq)]
pub enum FrontendError {
    /// The [`FrontendSpec`](crate::config::FrontendSpec) referenced a
    /// [`FrontendKind`](crate::config::FrontendKind) this build does not
    /// support (e.g. an HTTP frontend on a non-Linux target, where the
    /// socket ring does not exist).
    UnsupportedKind(&'static str),
    /// The listener address in the spec could not be parsed/bound.
    BadBind(String),
}

impl std::fmt::Display for FrontendError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            FrontendError::UnsupportedKind(k) => write!(f, "unsupported frontend kind: {k}"),
            FrontendError::BadBind(b) => write!(f, "bad frontend bind address: {b}"),
        }
    }
}

impl std::error::Error for FrontendError {}
