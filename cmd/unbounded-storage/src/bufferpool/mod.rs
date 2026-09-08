// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

mod free_list;
mod inflight;
mod null;
mod pipeline;
pub mod pool;
pub mod stream;
mod traits;
mod types;
mod window;

#[cfg(test)]
mod owned_future_tests;
#[cfg(test)]
mod tests;

pub use free_list::RecvQuarantineHandle;
pub use null::NullBlockStore;
pub use pipeline::{PipelinedRead, StripePlan};
pub use pool::Pool;
pub use stream::{OwnedPageFuture, PageGuard, ReadStream};
pub use traits::{BlockStore, BufferPool, PageCachePolicy, PageStream, Req, Transport};
pub use types::{BulkRef, Error, PageRef, PoolConfig, StripeKey};
pub use window::WindowedRead;
