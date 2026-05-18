// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(async_fn_in_trait)]

use crate::bufferpool::stream::ReadStream;
use crate::bufferpool::types::{BulkRef, Error, PageRef, StripeKey};

pub trait Req {
    fn key(&self) -> StripeKey;
}

pub trait Transport<R: Req> {
    /// Fetch the byte range described by `src` from a peer derived
    /// from `req` into the page identified by `dst.page_idx`.
    /// Resolves when the data has landed in `dst`.
    ///
    /// The transport is constructed already aware of the pool's
    /// pinned backing and page geometry: the embedder registers the
    /// backing with whatever wire-side resource is needed (e.g. an
    /// RDMA MR via `Class::register_backing`) before handing the
    /// transport to `Pool::new`. The `Pool` calls only this method
    /// at runtime.
    async fn bulk_get(&self, req: &R, src: BulkRef, dst: PageRef) -> Result<(), Error>;
    // TODO(jordan): Are both src and dst actually needed here?
}

pub trait BlockStore {
    /// Symmetric with the transport's NUMA registration handshake
    /// (which the embedder performs out-of-band before constructing
    /// the transport): impls that don't need pre-registration
    /// (plain `pwrite` on a regular file) can no-op; impls that do
    /// (io_uring fixed buffers, NVMe DMA mapping) record their
    /// per-page handles here.
    fn register_pages(
        &self,
        base: *mut u8,
        page_size: usize,
        page_count: usize,
    ) -> Result<(), Error>;

    /// Local-disk lookup. `Ok(true)` if `dst` was filled from disk;
    /// `Ok(false)` if the key is not present (pool then falls back
    /// to `Transport::bulk_get`).
    async fn read_page(&self, key: StripeKey, stripe_off: u64, dst: PageRef)
    -> Result<bool, Error>;

    /// Persist `page` for `(key, stripe_off)`. Used as the tee on a
    /// miss after the peer fetch lands.
    async fn write_page(&self, key: StripeKey, stripe_off: u64, page: PageRef)
    -> Result<(), Error>;
}

pub trait BufferPool {
    type Req: Req;

    async fn read<'p>(
        &'p self,
        req: &'p Self::Req,
        offset: u64,
        len: u64,
    ) -> Result<ReadStream<'p>, Error>;
}
