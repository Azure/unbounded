// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Direct libfabric transport. Phase 1 laid types, error plumbing, and
//! FFI declarations; Phase 3 brings up the libfabric class lifecycle
//! plus one pinned progress thread per CQ; Phase 4 adds connection
//! CRUD, MR registration, and tagged ping/pong; Phase 5a adds the
//! streaming RPC server (`rpc`), the server-side `Handler` trait
//! (`handler`), and the client-side `Transport` impl (`transport`).

mod backing;
mod completion;
mod config;
mod connection;
mod error;
mod fabric;
mod ffi;
mod handler;
mod ping;
mod pool_handler;
mod progress;
mod rpc;
mod rpc_queue;
mod transport;
mod types;

#[cfg(test)]
mod tests;

pub use backing::MrHandle;
pub use completion::{CompletionFuture, CompletionInfo, CompletionRegistry, CompletionSlot};
pub use config::{FabricConfig, Provider, defaults_for};
pub use error::{FabricError, Result, check};
pub use fabric::Fabric;
pub use handler::{Handler, HandlerStream};
pub use ping::{PING_TAG, PONG_TAG};
pub use pool_handler::{PoolHandler, PoolHandlerError, PoolHandlerStream};
pub use progress::ProgressThread;
pub use rpc::{
    MAX_HOPS, PageWritePlan, RequestHeader, RequestPlan, RpcServer, RpcServerHandle,
    plan_page_write,
};
pub use transport::{
    FabricTransport, PeerRouter, StaticPeer, ensure_launch_fits_registry, required_completion_slots,
};
pub use types::{ConnectionId, ConnectionSpec, Page, PeerId};
