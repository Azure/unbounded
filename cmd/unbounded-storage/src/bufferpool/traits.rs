// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(async_fn_in_trait)]

use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use crate::bufferpool::stream::ReadStream;
use crate::bufferpool::types::{Backing, BulkRef, Error, PageRef, StripeKey};

pub trait Req {
    fn key(&self) -> StripeKey;
}

/// `Stream`-like surface for a transport's bulk-get response. We
/// define this locally (rather than reuse `stream::ReadStream`,
/// which is a concrete struct exposing an `async fn next_page`,
/// not a `poll_next` trait) so transports can yield pages
/// incrementally without pulling in `futures-core`. Semantically
/// identical to a `futures::Stream<Item = Result<PageRef, Error>>`.
pub trait PageStream {
    fn poll_next(
        self: Pin<&mut Self>,
        cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, Error>>>;
}

pub trait Transport<R: Req> {
    /// Stream of pages produced by `bulk_get`. One `poll_next` may
    /// yield `Some(Ok(page))` per delivered page; `None` ends the
    /// stream successfully. Errors surface as `Some(Err(_))`.
    type Stream<'a>: PageStream + 'a
    where
        Self: 'a,
        R: 'a;

    /// Fetch the byte range described by `src` from a peer derived
    /// from `req` into each of the pages in `dsts`. Returns a
    /// stream that yields one `PageRef` per delivered page; the
    /// stream itself drives the async work via `poll_next`.
    ///
    /// The transport is constructed already aware of the pool's
    /// pinned backing and page geometry (the embedder registers
    /// the backing out-of-band, e.g. via
    /// `fabric::Fabric::register_backing`, before handing the
    /// transport to `Pool::new`). The `Pool` calls only this
    /// method at runtime.
    fn bulk_get<'a>(&'a self, req: &'a R, src: BulkRef, dsts: &'a [PageRef]) -> Self::Stream<'a>;
}

impl<R: Req, T: Transport<R> + ?Sized> Transport<R> for Arc<T> {
    type Stream<'a>
        = T::Stream<'a>
    where
        Self: 'a,
        R: 'a;

    fn bulk_get<'a>(&'a self, req: &'a R, src: BulkRef, dsts: &'a [PageRef]) -> Self::Stream<'a> {
        (**self).bulk_get(req, src, dsts)
    }
}

pub trait BlockStore {
    /// Symmetric with the transport's NUMA registration handshake
    /// (which the embedder performs out-of-band before constructing
    /// the transport via `Fabric::register_backing`): impls that
    /// don't need pre-registration (plain `pwrite` on a regular
    /// file) can no-op; impls that do (io_uring fixed buffers, NVMe
    /// DMA mapping) record their per-page handles here. Taking
    /// `&Backing` rather than raw pointers keeps the buffer pool
    /// as the single source of truth for page geometry on both the
    /// transport and the blockstore.
    fn register_pages(&self, backing: &Backing) -> Result<(), Error>;

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

/// Blanket impl so a `LocalStorage` (or any other `BlockStore`)
/// can be shared across shards by handing each `Pool` an
/// `Arc`-wrapped clone instead of an owned instance. Required by
/// the node-level engine ownership model: one engine per disk,
/// shared by every NIC shard.
impl<T: BlockStore + ?Sized> BlockStore for Arc<T> {
    fn register_pages(&self, backing: &Backing) -> Result<(), Error> {
        (**self).register_pages(backing)
    }

    async fn read_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        dst: PageRef,
    ) -> Result<bool, Error> {
        (**self).read_page(key, stripe_off, dst).await
    }

    async fn write_page(
        &self,
        key: StripeKey,
        stripe_off: u64,
        page: PageRef,
    ) -> Result<(), Error> {
        (**self).write_page(key, stripe_off, page).await
    }
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
