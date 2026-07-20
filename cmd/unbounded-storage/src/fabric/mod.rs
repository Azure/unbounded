// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Direct libfabric transport over connection-managed `FI_EP_MSG`
//! endpoints. The connection manager (`cm`) dials and accepts peer
//! connections; each connection self-reposts a `RecvPool` whose
//! completions are demultiplexed by the inbound `dispatch` using an
//! 8-byte message header (`wire`). A per-NUMA `ProgressGroup`
//! (`progress`) drives the CQs. The streaming RPC server (`rpc`) and
//! the server-side `Handler` trait (`handler`) answer requests; the
//! client-side `Transport` impl (`transport`) issues them.

mod backing;
mod cm;
mod completion;
mod config;
mod connection;
mod dispatch;
mod error;
mod fabric;
mod ffi;
mod handler;
mod pool_handler;
mod progress;
mod recvpool;
mod rpc;
mod rpc_admission;
mod rpc_queue;
pub(crate) mod scratch;
mod sendpool;
mod transport;
mod types;
mod wire;

#[cfg(test)]
mod tests;

pub use backing::MrHandle;
pub use completion::{CompletionFuture, CompletionInfo, CompletionRegistry, CompletionSlot};
pub use config::{FabricConfig, Provider, apply_tcp_env_defaults, defaults_for};
pub use error::{FabricError, Result, check};
pub use fabric::{Fabric, provider_available};
pub use handler::{FabricPage, Handler, HandlerStream, PageRelease, PageSource};
pub use pool_handler::{PoolHandler, PoolHandlerError, PoolHandlerStream};
pub use rpc::{
    MAX_HOPS, PageWritePlan, RequestHeader, RequestPlan, RpcServer, RpcServerHandle,
    plan_page_write,
};
pub use transport::{
    FabricTransport, PeerPathSelection, PeerRouter, StaticPeer, ensure_launch_fits_registry,
    required_completion_slots,
};
pub use types::{ConnectionId, ConnectionSpec, FabricAddress, Page, PeerId};
