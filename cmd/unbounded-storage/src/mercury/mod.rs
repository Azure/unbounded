// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Mercury RPC transport for the bufferpool.
//!
//! # Two-phase RPC contract
//!
//! Every `bulk_get` is a single RPC with two RDMA stages:
//!
//! 1. The client builds a `BulkGetIn` header containing the opaque
//!    request bytes plus a Mercury bulk handle over its own
//!    pre-registered destination buffer, and issues `HG_Forward`.
//! 2. The server decodes the header, calls `BulkSource::fetch` to
//!    obtain the bytes, pushes them into the client's destination via
//!    `HG_Bulk_transfer(HG_BULK_PUSH)`, and only then issues
//!    `HG_Respond` with a `BulkGetOut { status }`. `status == 0` means
//!    the bytes are already in the destination buffer when the client's
//!    forward callback fires; any non-zero status indicates the push
//!    was skipped (short read, fetch error, ...).
//!
//! # Multi-context-per-NIC model
//!
//! One [`Nic`] owns one Mercury class per HCA. The class is shared by
//! `cfg.contexts_per_nic` [`ProgressContext`]s, each with its own
//! dedicated progress thread, [`CompletionRegistry`] (client-side
//! back-pressure), and optional [`ServerJobQueue`] (inbound RPC fan-in
//! for listening NICs). Inbound RPCs are sharded across contexts by a
//! hash of the peer address through [`CtxSelector`]; outbound calls
//! pick a context at `MercuryTransport` construction time.
//!
//! # Pre-registered destination MR
//!
//! [`Nic::register_backing`] calls `HG_Bulk_create` once over the
//! caller's backing buffer and parks the resulting [`LocalBulk`] on the
//! NIC via `ArcSwapOption`. Every client `bulk_get` reuses that handle
//! - the hot path never calls `HG_Bulk_create`, only `HG_Forward`.
//!
//! # User-facing surface
//!
//! - [`Nic`] - per-HCA Mercury class plus contexts and peer table.
//! - [`MercuryTransport`] - client-side `bufferpool::Transport` impl.
//! - [`MercuryServer`] - server-side drainer attached to a listening NIC.
//! - [`BulkSource`] - trait the application implements to serve bytes.
//! - [`PeerRouter`] - maps an outbound request to a `PeerId`.

pub mod ffi;

mod bulk;
mod config;
mod context;
mod error;
mod handle;
mod nic;
mod peer;
mod progress;
mod router;
mod rpc;
mod server;
mod transport;

#[cfg(test)]
mod tests;

pub use bulk::LocalBulk;
pub use config::{NicConfig, PeerEntry};
pub use context::ProgressContext;
pub use error::{HgError, Result};
pub use handle::Handle;
pub use nic::Nic;
pub use peer::{PeerId, PeerTable};
pub use progress::{CompletionFuture, CompletionRegistry, CompletionSlot, Oneshot, ServerJobQueue};
pub use router::{PeerRouter, StaticPeer};
pub use rpc::{BulkGetIn, BulkGetOut, CtxSelector};
pub use server::{BulkSource, MercuryServer};
pub use transport::MercuryTransport;
