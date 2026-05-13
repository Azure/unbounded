// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

mod free_list;
mod inflight;
pub mod pool;
pub mod stream;
mod traits;
mod types;

#[cfg(test)]
mod tests;

pub use pool::Pool;
pub use stream::{PageGuard, ReadStream};
pub use traits::{BlockStore, BufferPool, Req, Transport};
pub use types::{
    Backing, BulkRef, Error, NodeId, PageRef, PeerId, PoolConfig, StripeKey, TraceCtx,
};
