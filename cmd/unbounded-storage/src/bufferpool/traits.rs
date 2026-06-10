// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

#![allow(async_fn_in_trait)]

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use crate::bufferpool::stream::ReadStream;
use crate::bufferpool::types::{BulkRef, Error, PageRef, StripeKey};
use crate::bufferpool::window::WindowedRead;
use crate::bufferpool::pipeline::{PipelinedRead, StripePlan};
use crate::memory::Backing;

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

    /// Fetch exactly one page from a peer into `dst`, collapsing the
    /// single-page `bulk_get` stream into a flat async completion.
    ///
    /// This is the single-page consumer surface that is symmetric
    /// with [`BlockStore::read_page`]: both deliver one page into a
    /// `PageRef` via a flat `async fn`, so the buffer pool can drive
    /// a local-disk read and a remote fetch through identical shapes
    /// with no stream adapter at the call site. Multi-page consumers
    /// (server handlers, object-range backends) keep using
    /// [`Transport::bulk_get`] directly.
    ///
    /// Resolves `Ok(())` on the first delivered page, propagates a
    /// stream error, and surfaces a premature end-of-stream as an
    /// `Error`.
    async fn fetch_one(&self, req: &R, src: BulkRef, dst: PageRef) -> Result<(), Error> {
        let dsts = [dst];
        DriveSinglePage {
            stream: self.bulk_get(req, src, &dsts),
        }
        .await
    }
}

/// Adapter that drives a single-page `bulk_get` stream to its first
/// delivered page. Used by [`Transport::fetch_one`].
struct DriveSinglePage<S: PageStream> {
    stream: S,
}

impl<S: PageStream> Future for DriveSinglePage<S> {
    type Output = Result<(), Error>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // SAFETY: standard structural pin projection through the
        // single `stream` field; we never move out of `self`.
        let stream = unsafe { self.map_unchecked_mut(|s| &mut s.stream) };
        match stream.poll_next(cx) {
            Poll::Pending => Poll::Pending,
            Poll::Ready(Some(Ok(_page))) => Poll::Ready(Ok(())),
            Poll::Ready(Some(Err(e))) => Poll::Ready(Err(e)),
            Poll::Ready(None) => Poll::Ready(Err(Error::from(
                "transport stream ended without delivering page",
            ))),
        }
    }
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

    /// Windowed counterpart to [`BufferPool::read`]. Returns a
    /// [`WindowedRead`] that keeps up to `window` `fetch_page`
    /// futures outstanding ahead of its consumer cursor (within a
    /// single stripe) while still delivering pages strictly in
    /// cursor order, one at a time. This is the high-throughput
    /// read path: it overlaps fabric fetches of pages ahead of the
    /// cursor with the in-order consumption of the current page, so
    /// a single large read keeps many RDMA transfers in flight.
    /// `window` is clamped by the implementation to the pool's
    /// configured prefetch budget.
    fn read_windowed<'p>(
        &'p self,
        req: &'p Self::Req,
        offset: u64,
        len: u64,
        window: usize,
    ) -> Result<WindowedRead<'p>, Error>;

    /// Cross-stripe counterpart to [`BufferPool::read_windowed`].
    /// Takes the ordered list of per-stripe slices that make up a
    /// byte range and returns a [`PipelinedRead`] that keeps up to
    /// `window` `fetch_page` futures outstanding ahead of the
    /// consumer cursor *across stripe boundaries*, while still
    /// delivering pages strictly in order, one at a time. This is the
    /// throughput path for multi-stripe reads: it overlaps origin and
    /// peer fetches of pages many stripes ahead with the in-order
    /// consumption (and slow client send) of the current page, so a
    /// single large GET keeps the whole `max_inflight_pages` budget
    /// saturated instead of collapsing to one stripe's depth. `window`
    /// is clamped by the implementation to the pool's configured
    /// prefetch budget.
    fn read_pipelined<'p>(
        &'p self,
        stripes: Vec<StripePlan<Self::Req>>,
        window: usize,
    ) -> Result<PipelinedRead<'p>, Error>;
}
