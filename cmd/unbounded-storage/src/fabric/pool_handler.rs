// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Production [`Handler`] that serves locally-resident stripe pages
//! to remote peers.
//!
//! # Why this does not touch the shard `Pool`
//!
//! [`crate::fabric::rpc`] drives requests on a fixed worker pool,
//! so the [`Handler`] trait requires
//! `Send + Sync + 'static`. The per-shard
//! [`crate::bufferpool::Pool`] is `Rc`-based and `!Send`: it lives
//! on, and may only be touched from, its own shard thread. Reaching
//! into the pool's `Rc<RefCell<...>>` free list or inflight map from
//! the RPC worker thread would be unsound, so this handler does not.
//!
//! Instead it owns a small **dedicated scratch [`Backing`]**, separate
//! from the shard's bufferpool backing, that only the RPC workers ever
//! touch. The scratch backing is registered as its own fabric MR (used
//! as the `local_mr` source for `fi_write`) and as a `BlockStore`
//! extra buffer (so the disk io_uring path can DMA into it). A tiny
//! free stack hands out one scratch page per in-flight request; the
//! page is reclaimed when the response stream drops, after the RPC
//! layer has finished `fi_write`ing it to the
//! peer (the worker blocks on the write completion before re-polling
//! the stream).
//!
//! # Local read semantics
//!
//! On each request the handler asks the shard's [`BlockStore`]
//! (`LiveShardLocalStore` in production) for the requested page via
//! [`BlockStore::read_page`]. A hit (`Ok(true)`) is yielded as a
//! [`PageRef`] into the scratch backing. A miss (`Ok(false)`) is
//! surfaced as [`PoolHandlerError::NotResident`] so the RPC layer
//! sends an `ERROR_ACK` and the requesting peer treats it as a miss
//! rather than receiving fabricated zero bytes. The handler never
//! recurses back into the transport: it serves only what is already
//! resident on this node.

use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll};

use crate::bufferpool::{BlockStore, BulkRef, PageRef, Req};
use crate::memory::Backing;

use super::handler::{FabricPage, Handler, HandlerStream};
use super::scratch::ScratchBacking;

/// Error surfaced by [`PoolHandler`]'s response stream.
#[derive(Debug)]
pub enum PoolHandlerError {
    /// The requested stripe page is not resident on this node's
    /// local store. Reported to the peer as a miss; it must not be
    /// confused with a successful zero-filled page.
    NotResident,
    /// The handler's scratch backing is fully checked out. The peer
    /// should retry; this is a transient resource limit, not a data
    /// error.
    NoScratchPage,
    /// The underlying [`BlockStore::read_page`] returned an error.
    BlockStore(crate::bufferpool::Error),
}

impl std::fmt::Display for PoolHandlerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            PoolHandlerError::NotResident => {
                write!(f, "stripe not resident on local store")
            }
            PoolHandlerError::NoScratchPage => {
                write!(f, "no scratch page available")
            }
            PoolHandlerError::BlockStore(e) => write!(f, "block store: {e}"),
        }
    }
}

impl std::error::Error for PoolHandlerError {}

/// Production handler serving locally-resident stripe pages.
///
/// `S` is the shard's [`BlockStore`] (e.g.
/// `LiveShardLocalStore`); it must be
/// `Send + Sync` because the handler runs on RPC worker threads.
pub struct PoolHandler<S: BlockStore + Send + Sync + 'static> {
    store: Arc<S>,
    scratch: Arc<ScratchBacking>,
}

impl<S: BlockStore + Send + Sync + 'static> PoolHandler<S> {
    /// Build a handler over `store`, using `scratch` as the dedicated
    /// backing whose pages are filled and yielded. `scratch` MUST be
    /// the same `Backing` the caller registered as the RPC server's
    /// `local_mr` (so the `fi_write` source addresses resolve) and as
    /// a `BlockStore` extra buffer (so the disk read can DMA into it).
    ///
    /// `scratch_pages` caps how many pages of `scratch` the handler
    /// will hand out concurrently; it must be `<= scratch.page_count`.
    pub fn new(store: Arc<S>, scratch: Backing, scratch_pages: u32) -> Self {
        let usable = scratch_pages.min(scratch.page_count as u32);

        Self {
            store,
            scratch: Arc::new(ScratchBacking::new(scratch, usable)),
        }
    }
}

impl<R, S> Handler<R> for PoolHandler<S>
where
    R: Req + Sync,
    S: BlockStore + Send + Sync + 'static,
{
    type Error = PoolHandlerError;
    type Stream<'a>
        = PoolHandlerStream<'a, R, S>
    where
        Self: 'a,
        R: 'a;

    fn handle<'a>(&'a self, req: &'a R, src: BulkRef, _hops_remaining: u32) -> Self::Stream<'a> {
        PoolHandlerStream {
            store: self.store.clone(),
            scratch: self.scratch.clone(),
            req,
            src,
            state: StreamState::Pending,
            page_idx: None,
        }
    }
}

#[derive(Copy, Clone, PartialEq, Eq)]
enum StreamState {
    /// Have not yet attempted the local read.
    Pending,
    /// Yielded the page; the next poll ends the stream.
    Done,
    /// Already ended (page yielded, error emitted, or miss reported).
    Ended,
}

/// Response stream for one request. Yields exactly one page on a
/// local hit, then ends; on a miss or error it yields the error then
/// ends. The scratch page (if any) is returned to the allocator on
/// drop, which happens after the RPC layer has finished `fi_write`ing
/// it to the peer.
pub struct PoolHandlerStream<'a, R: Req, S: BlockStore + Send + Sync + 'static> {
    store: Arc<S>,
    scratch: Arc<ScratchBacking>,
    req: &'a R,
    src: BulkRef,
    state: StreamState,
    page_idx: Option<u32>,
}

impl<R: Req, S: BlockStore + Send + Sync + 'static> HandlerStream for PoolHandlerStream<'_, R, S> {
    type Error = PoolHandlerError;

    fn poll_next(
        mut self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<FabricPage, PoolHandlerError>>> {
        match self.state {
            StreamState::Done | StreamState::Ended => {
                self.state = StreamState::Ended;
                Poll::Ready(None)
            }
            StreamState::Pending => {
                let result = self.serve_local_page();
                match result {
                    Ok(page) => {
                        self.state = StreamState::Done;
                        Poll::Ready(Some(Ok(page.into())))
                    }
                    Err(e) => {
                        self.state = StreamState::Ended;
                        Poll::Ready(Some(Err(e)))
                    }
                }
            }
        }
    }
}

impl<R: Req, S: BlockStore + Send + Sync + 'static> PoolHandlerStream<'_, R, S> {
    /// Reserve a scratch page, fill it from the local store, and turn
    /// it into a `PageRef`. The `BlockStore::read_page` future is
    /// created and driven to completion entirely within this call so
    /// the stream itself holds no future and is trivially `Send`.
    /// Blocking here is acceptable: the RPC worker pool is separate
    /// from shard threads (see [`crate::fabric::rpc`]) and disk progress
    /// runs concurrently on the per-disk io_uring threads.
    fn serve_local_page(&mut self) -> Result<PageRef, PoolHandlerError> {
        let page_size = self.scratch.page_size();
        let idx = self
            .scratch
            .take_zeroed()
            .ok_or(PoolHandlerError::NoScratchPage)?;
        self.page_idx = Some(idx);

        let dst = PageRef {
            page_idx: idx,
            offset: 0,
            len: page_size as u32,
        };

        let hit = block_on_local(self.store.read_page(self.req, self.src.offset, dst))
            .map_err(PoolHandlerError::BlockStore)?;
        if !hit {
            return Err(PoolHandlerError::NotResident);
        }

        // Honor the requested intra-stripe window. `BulkRef.len` is
        // bounded by `page_size`; clamp defensively so a malformed
        // request can never make the RPC layer read past the page.
        let len = (self.src.len as usize).min(page_size) as u32;
        Ok(PageRef {
            page_idx: idx,
            offset: 0,
            len,
        })
    }
}

impl<R: Req, S: BlockStore + Send + Sync + 'static> Drop for PoolHandlerStream<'_, R, S> {
    fn drop(&mut self) {
        if let Some(idx) = self.page_idx.take() {
            self.scratch.give(idx);
        }
    }
}

/// Drive a non-`Send`-bounded future to completion on the current
/// thread with a noop waker. Mirrors the spin-poller in
/// [`crate::fabric::rpc`]; the underlying disk I/O makes progress on
/// its own io_uring threads while this spins.
fn block_on_local<F: std::future::Future>(fut: F) -> F::Output {
    crate::runtime::block_on_cooperative(fut, || {
        std::thread::sleep(std::time::Duration::from_micros(50))
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::bufferpool::{Error, StripeKey};
    use crate::runtime::noop_waker;
    use std::sync::atomic::{AtomicUsize, Ordering};

    /// In-memory `BlockStore` mock: holds page bytes keyed by stripe.
    /// `read_page` copies the recorded bytes into the destination
    /// scratch page when present, returning `Ok(true)`; otherwise it
    /// returns `Ok(false)` (a miss) or, when the key is poisoned, an
    /// error.
    struct MemStore {
        base: *mut u8,
        page_size: usize,
        present: std::collections::HashMap<[u8; 32], u8>,
        poison: std::collections::HashSet<[u8; 32]>,
        reads: AtomicUsize,
    }

    // SAFETY: the test drives the mock from a single thread; the raw
    // base pointer refers to a leaked heap allocation that outlives
    // the test.
    unsafe impl Send for MemStore {}
    unsafe impl Sync for MemStore {}

    impl BlockStore for MemStore {
        fn register_pages(&self, _backing: &Backing) -> Result<(), Error> {
            Ok(())
        }

        async fn read_page<R: Req + ?Sized>(
            &self,
            req: &R,
            _stripe_off: u64,
            dst: PageRef,
        ) -> Result<bool, Error> {
            let key = req.key();
            self.reads.fetch_add(1, Ordering::Relaxed);
            if self.poison.contains(&key.0) {
                return Err(Error::Io(libc::EIO));
            }
            match self.present.get(&key.0) {
                Some(&fill) => {
                    // SAFETY: dst.page_idx is within the scratch
                    // backing the test allocated; base + idx*page_size
                    // is a valid writable page.
                    unsafe {
                        let p = self.base.add(dst.page_idx as usize * self.page_size);
                        std::ptr::write_bytes(p, fill, self.page_size);
                    }
                    Ok(true)
                }
                None => Ok(false),
            }
        }

        async fn write_page<R: Req + ?Sized>(
            &self,
            _req: &R,
            _stripe_off: u64,
            _page: PageRef,
        ) -> Result<(), Error> {
            Ok(())
        }
    }

    struct KeyReq(StripeKey);

    impl Req for KeyReq {
        fn key(&self) -> StripeKey {
            self.0
        }
    }

    const PAGE: usize = 4096;
    const SCRATCH_PAGES: u32 = 2;

    fn scratch_backing() -> (Backing, *mut u8) {
        let buf = vec![0u8; PAGE * SCRATCH_PAGES as usize].into_boxed_slice();
        let base = Box::leak(buf).as_mut_ptr();
        let backing = Backing {
            base,
            page_size: PAGE,
            page_count: SCRATCH_PAGES as usize,
            keepalive: std::sync::Arc::new(()),
        };
        (backing, base)
    }

    fn store(base: *mut u8, present: &[([u8; 32], u8)], poison: &[[u8; 32]]) -> Arc<MemStore> {
        Arc::new(MemStore {
            base,
            page_size: PAGE,
            present: present.iter().copied().collect(),
            poison: poison.iter().copied().collect(),
            reads: AtomicUsize::new(0),
        })
    }

    fn drain<S: BlockStore + Send + Sync + 'static>(
        handler: &PoolHandler<S>,
        key: [u8; 32],
        len: u32,
    ) -> Vec<Result<crate::fabric::FabricPage, PoolHandlerError>> {
        let req = KeyReq(StripeKey(key));
        let src = BulkRef {
            stripe: StripeKey(key),
            offset: 0,
            len,
        };
        let mut stream = handler.handle(&req, src, crate::fabric::MAX_HOPS);
        // SAFETY: stream is pinned to this stack frame and not moved.
        let mut stream = unsafe { Pin::new_unchecked(&mut stream) };
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut out = Vec::new();
        let mut spins = 0u64;
        loop {
            match stream.as_mut().poll_next(&mut cx) {
                Poll::Ready(None) => return out,
                Poll::Ready(Some(item)) => out.push(item),
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "stuck");
                }
            }
        }
    }

    #[test]
    fn resident_stripe_yields_one_page() {
        let (scratch, base) = scratch_backing();
        let key = [7u8; 32];
        let st = store(base, &[(key, 0xAB)], &[]);
        let handler = PoolHandler::new(st.clone(), scratch, SCRATCH_PAGES);

        let out = drain(&handler, key, PAGE as u32);
        assert_eq!(out.len(), 1, "expected exactly one page yielded");
        let page = out[0].as_ref().expect("resident page must be Ok");
        assert_eq!(page.page.len, PAGE as u32);
        // The yielded page index points into the filled scratch page.
        // SAFETY: page_idx is within the scratch backing.
        let byte = unsafe { *base.add(page.page.page_idx as usize * PAGE) };
        assert_eq!(byte, 0xAB, "scratch page was not filled from the store");
        assert_eq!(st.reads.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn non_resident_stripe_errors_not_resident() {
        let (scratch, base) = scratch_backing();
        let key = [9u8; 32];
        // store has no entry for `key` -> read_page returns Ok(false).
        let st = store(base, &[], &[]);
        let handler = PoolHandler::new(st, scratch, SCRATCH_PAGES);

        let out = drain(&handler, key, PAGE as u32);
        assert_eq!(out.len(), 1, "miss must surface exactly one error item");
        match out[0].as_ref() {
            Err(PoolHandlerError::NotResident) => {}
            other => panic!("expected NotResident, got {other:?}"),
        }
    }

    #[test]
    fn block_store_error_propagates() {
        let (scratch, base) = scratch_backing();
        let key = [3u8; 32];
        let st = store(base, &[], &[key]);
        let handler = PoolHandler::new(st, scratch, SCRATCH_PAGES);

        let out = drain(&handler, key, PAGE as u32);
        assert_eq!(out.len(), 1);
        match out[0].as_ref() {
            Err(PoolHandlerError::BlockStore(_)) => {}
            other => panic!("expected BlockStore error, got {other:?}"),
        }
    }

    #[test]
    fn intra_page_window_is_honored() {
        let (scratch, base) = scratch_backing();
        let key = [1u8; 32];
        let st = store(base, &[(key, 0x01)], &[]);
        let handler = PoolHandler::new(st, scratch, SCRATCH_PAGES);

        let out = drain(&handler, key, 1024);
        let page = out[0].as_ref().expect("ok");
        assert_eq!(page.page.len, 1024, "handler must honor BulkRef.len window");
    }

    #[test]
    fn scratch_page_is_recycled_across_requests() {
        // Only SCRATCH_PAGES pages exist; serving many sequential
        // requests must keep working because each stream returns its
        // page on drop.
        let (scratch, base) = scratch_backing();
        let key = [5u8; 32];
        let st = store(base, &[(key, 0x22)], &[]);
        let handler = PoolHandler::new(st, scratch, SCRATCH_PAGES);

        for _ in 0..(SCRATCH_PAGES as usize + 4) {
            let out = drain(&handler, key, PAGE as u32);
            assert!(out[0].is_ok(), "page should be reusable after drop");
        }
    }

    #[test]
    fn exhausted_scratch_pool_reports_no_scratch_page() {
        // Hold streams open (do not drop) until the scratch allocator drains,
        // then the next handle()+poll must report NoScratchPage.
        let (scratch, base) = scratch_backing();
        let key = [6u8; 32];
        let st = store(base, &[(key, 0x33)], &[]);
        let handler = PoolHandler::new(st, scratch, 1);

        let req = KeyReq(StripeKey(key));
        let src = BulkRef {
            stripe: StripeKey(key),
            offset: 0,
            len: PAGE as u32,
        };
        // First stream: serve a page and KEEP it (do not drop yet).
        let mut held = handler.handle(&req, src, crate::fabric::MAX_HOPS);
        // SAFETY: pinned on stack, not moved.
        let mut held_pin = unsafe { Pin::new_unchecked(&mut held) };
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let first = held_pin.as_mut().poll_next(&mut cx);
        assert!(matches!(first, Poll::Ready(Some(Ok(_)))));

        // Second request while the first page is still checked out.
        let out = drain(&handler, key, PAGE as u32);
        match out[0].as_ref() {
            Err(PoolHandlerError::NoScratchPage) => {}
            other => panic!("expected NoScratchPage, got {other:?}"),
        }
        let _ = base; // base used only via the store mock.
        drop(held);
    }
}
