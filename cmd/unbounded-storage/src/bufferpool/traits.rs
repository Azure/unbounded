// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(async_fn_in_trait)]

use crate::bufferpool::stream::ReadStream;
use crate::bufferpool::types::{BulkRef, Error, PageRef, StripeKey};

pub trait Req {
    fn key(&self) -> StripeKey;
}

pub trait Transport<R: Req> {
    /// Called once at pool init with the full pinned backing. The
    /// impl records whatever it needs (e.g. an RDMA MR) so it can
    /// translate `page_idx` into a wire-side handle later.
    fn register_pages(
        &self,
        base: *mut u8,
        page_size: usize,
        page_count: usize,
    ) -> Result<(), Error>;

    /// Fetch the byte range described by `src` from a peer derived
    /// from `req` into the page identified by `dst.page_idx`.
    /// Resolves when the data has landed in `dst`.
    async fn bulk_get(&self, req: &R, src: BulkRef, dst: PageRef) -> Result<(), Error>;
    // TODO(jordan): Are both src and dst actually needed here?
}

pub trait BlockStore {
    /// Symmetric with [`Transport::register_pages`]. Impls that don't
    /// need pre-registration (plain `pwrite` on a regular file) can
    /// no-op; impls that do (io_uring fixed buffers, NVMe DMA
    /// mapping) record their per-page handles here.
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
