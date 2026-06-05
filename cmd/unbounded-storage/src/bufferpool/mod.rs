// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

mod free_list;
mod group;
mod inflight;
mod null;
pub mod pool;
pub mod stream;
mod traits;
mod types;
mod window;

#[cfg(test)]
mod tests;

pub use free_list::RecvQuarantineHandle;
pub use group::{PoolGroup, ShardDescriptor, ShardRouter};
pub use null::NullBlockStore;
pub use pool::Pool;
pub use stream::{PageGuard, ReadStream};
pub use traits::{BlockStore, BufferPool, PageStream, Req, Transport};
pub use types::{BulkRef, Error, PageRef, PoolConfig, StripeKey, TraceCtx};
pub use window::WindowedRead;
