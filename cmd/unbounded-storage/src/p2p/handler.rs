// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Server-side recursive [`Handler`] for stripe reads.
//!
//! Where [`crate::fabric::PoolHandler`] serves only locally-resident
//! pages and reports a miss otherwise, this handler *resolves* every
//! request: it either owns the stripe (serve from the local
//! [`BlockStore`], falling back to the topology-unaware origin
//! [`Backend`] on a miss) or it is an intermediate hop (forward to the
//! next hop over the fabric, landing the downstream page directly in
//! this node's scratch via RDMA, then relay it upstream).
//!
//! # Routing
//!
//! On each request the handler computes `stripe_to_ring(key)` and asks
//! its [`FingerTable`] for the next hop:
//!
//! * `next_hop == None` - this node owns the stripe. Read it from the
//!   local store; on a miss, fetch from the origin `Backend`.
//! * `next_hop == Some(_)` and `hops_remaining > 0` - forward to the
//!   next hop with a decremented TTL via the scratch-bound
//!   [`FabricTransport`]. The downstream hop RDMA-writes its page into
//!   this node's scratch page; the handler then yields that page so
//!   the RPC layer relays it to the original requester.
//! * `next_hop == Some(_)` and `hops_remaining == 0` - the hop budget
//!   is exhausted; surface [`RecursiveHandlerError::HopLimitExceeded`].
//!
//! # Scratch backing
//!
//! Like [`crate::fabric::PoolHandler`], this handler owns a dedicated
//! scratch [`Backing`] registered as its own fabric MR and as a
//! [`BlockStore`] extra buffer. A `Mutex`-guarded free list hands out
//! one scratch page per in-flight request; the page is reclaimed when
//! the response stream drops, after the RPC layer has finished
//! `fi_write`ing it to the peer. The free list helper is duplicated
//! here (rather than shared with `pool_handler.rs`) to keep the two
//! handlers decoupled.

use std::collections::HashMap;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll};

use serde::Serialize;
use serde::de::DeserializeOwned;

use crate::backend::Backend;
use crate::bufferpool::{BlockStore, BulkRef, PageRef, PageStream, Req, StripeKey};
use crate::fabric::Result as FabResult;
use crate::fabric::{Fabric, FabricTransport, Handler, HandlerStream, MrHandle, PeerId};
use crate::memory::Backing;
use crate::p2p::{
    ChainFingerRouter, FingerTable, NodeId, RingId, RouteTableHandle, RoutingHandle, stripe_to_ring,
};
use crate::runtime::{block_on_cooperative, noop_waker};

/// Error surfaced by [`RecursiveHandler`]'s response stream.
#[derive(Debug)]
pub enum RecursiveHandlerError {
    /// The handler's scratch backing is fully checked out. The peer
    /// should retry; this is a transient resource limit, not a data
    /// error.
    NoScratchPage,
    /// This node is an intermediate hop but the request's hop budget
    /// is exhausted. The recursion is cut short to bound forwarding.
    HopLimitExceeded,
    /// The local [`BlockStore::read_page`] returned an error.
    BlockStore(crate::bufferpool::Error),
    /// The origin [`Backend`] (consulted on an owner-side miss)
    /// returned an error.
    Backend(crate::bufferpool::Error),
    /// Forwarding to the next hop over the fabric failed (downstream
    /// server error or fabric transport error).
    Forward(crate::bufferpool::Error),
}

impl std::fmt::Display for RecursiveHandlerError {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            RecursiveHandlerError::NoScratchPage => {
                write!(f, "no scratch page available")
            }
            RecursiveHandlerError::HopLimitExceeded => {
                write!(f, "recursive forward hop limit exceeded")
            }
            RecursiveHandlerError::BlockStore(e) => write!(f, "block store: {e}"),
            RecursiveHandlerError::Backend(e) => write!(f, "origin backend: {e}"),
            RecursiveHandlerError::Forward(e) => write!(f, "forward to next hop: {e}"),
        }
    }
}

impl std::error::Error for RecursiveHandlerError {}

/// Recursive handler resolving stripe reads by routing through the
/// finger table.
///
/// `S` is the shard's [`BlockStore`]; `B` is the topology-unaware
/// origin [`Backend`]. Both must be `Send + Sync` because the handler
/// runs on RPC worker threads. `forward` is a client transport bound
/// to the scratch MR, used to forward to the next hop and land the
/// downstream page directly in scratch.
pub struct RecursiveHandler<S, B>
where
    S: BlockStore + Send + Sync + 'static,
    B: Backend,
{
    store: Arc<S>,
    scratch: Arc<ScratchBacking>,
    routes: RouteTableHandle,
    forward: FabricTransport<B::Req, ChainFingerRouter>,
    backend: B,
}

impl<S, B> RecursiveHandler<S, B>
where
    S: BlockStore + Send + Sync + 'static,
    B: Backend + 'static,
    B::Req: Req + Serialize + DeserializeOwned + 'static,
{
    /// Build a recursive handler over a freshly-seeded routing surface.
    ///
    /// `scratch` is the dedicated backing whose pages are filled and
    /// yielded; `scratch_mr` MUST be the same backing registered as
    /// the forward transport's destination MR (so downstream
    /// `fi_write`s land in scratch) and as a [`BlockStore`] extra
    /// buffer (so the local disk read can DMA into it). `scratch_pages`
    /// caps concurrent checkouts and must be `<= scratch.page_count`.
    ///
    /// The chosen signature threads the scratch backing, its MR, and
    /// the page geometry separately because the backing (CPU view) and
    /// the MR handle (NIC view) are distinct objects the embedder
    /// registers out-of-band, mirroring `PoolHandler::new` plus
    /// `FabricTransport::new`. Use [`Self::with_routing`] to share a
    /// [`RoutingHandle`] with other consumers for live reload.
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        store: Arc<S>,
        scratch: Backing,
        scratch_pages: u32,
        fingers: Arc<FingerTable>,
        node_to_peer: Arc<HashMap<NodeId, PeerId>>,
        fabric: Arc<Fabric>,
        scratch_mr: MrHandle,
        page_size: usize,
        backend: B,
    ) -> FabResult<Self> {
        let mut routes = HashMap::new();
        routes.insert(
            RouteTableHandle::LEGACY_ROUTE_ID.to_string(),
            crate::p2p::RoutingSnapshot {
                fingers,
                node_to_peer,
            },
        );
        Self::with_routes(
            store,
            scratch,
            scratch_pages,
            RouteTableHandle::new(routes),
            fabric,
            scratch_mr,
            page_size,
            backend,
        )
    }

    /// Build a recursive handler that shares `routing` with other
    /// consumers. The forwarding `FabricTransport` is built with a
    /// `FingerRouter` over a clone of the same handle, so a republish
    /// through any clone reloads this handler's classify and forward
    /// paths together.
    #[allow(clippy::too_many_arguments)]
    pub fn with_routing(
        store: Arc<S>,
        scratch: Backing,
        scratch_pages: u32,
        routing: RoutingHandle,
        fabric: Arc<Fabric>,
        scratch_mr: MrHandle,
        page_size: usize,
        backend: B,
    ) -> FabResult<Self> {
        let snap = routing.snapshot();
        let mut routes = HashMap::new();
        routes.insert(
            RouteTableHandle::LEGACY_ROUTE_ID.to_string(),
            crate::p2p::RoutingSnapshot {
                fingers: snap.fingers.clone(),
                node_to_peer: snap.node_to_peer.clone(),
            },
        );
        Self::with_routes(
            store,
            scratch,
            scratch_pages,
            RouteTableHandle::new(routes),
            fabric,
            scratch_mr,
            page_size,
            backend,
        )
    }

    #[allow(clippy::too_many_arguments)]
    pub fn with_routes(
        store: Arc<S>,
        scratch: Backing,
        scratch_pages: u32,
        routes: RouteTableHandle,
        fabric: Arc<Fabric>,
        scratch_mr: MrHandle,
        page_size: usize,
        backend: B,
    ) -> FabResult<Self> {
        let usable = scratch_pages.min(scratch.page_count as u32);
        let free: Vec<u32> = (0..usable).collect();
        let scratch = Arc::new(ScratchBacking {
            backing: scratch,
            free: Mutex::new(free),
        });
        let router = ChainFingerRouter::new(routes.clone());
        let forward = FabricTransport::new(fabric, scratch_mr, router, page_size)?;
        Ok(Self {
            store,
            scratch,
            routes,
            forward,
            backend,
        })
    }
}

impl<R, S, B> Handler<R> for RecursiveHandler<S, B>
where
    R: Req + Serialize + DeserializeOwned + Send + Sync + 'static,
    S: BlockStore + Send + Sync + 'static,
    B: Backend<Req = R> + 'static,
{
    type Error = RecursiveHandlerError;
    type Stream<'a>
        = RecursiveHandlerStream<'a, R, S, B>
    where
        Self: 'a,
        R: 'a;

    fn handle<'a>(&'a self, req: &'a R, src: BulkRef, hops_remaining: u32) -> Self::Stream<'a> {
        RecursiveHandlerStream {
            store: self.store.clone(),
            scratch: self.scratch.clone(),
            fingers: self.routes.route_for_req(req).map(|route| route.fingers),
            forward: &self.forward,
            backend: &self.backend,
            req,
            key: req.key(),
            src,
            hops_remaining,
            state: StreamState::Pending,
            page_idx: None,
        }
    }
}

/// Scratch backing plus its free list of available page indices.
/// Reads and writes to the underlying pages only ever happen from
/// RPC worker threads; the free list is the only shared mutable
/// state, guarded by a `Mutex`. The `backing` field is read directly
/// to derive page base pointers when a checked-out slot must be
/// zeroed before it is filled.
struct ScratchBacking {
    backing: Backing,
    free: Mutex<Vec<u32>>,
}

impl ScratchBacking {
    /// Pop a free page index, zeroing the full extent of that scratch
    /// slot before returning it. Every data-bearing request path must
    /// check out through this method so a recycled page can never
    /// carry residual bytes from a prior request into a new response;
    /// the relayed length is derived from remote-supplied input and a
    /// short fill would otherwise expose stale in-page bytes.
    fn take_zeroed(&self) -> Option<u32> {
        let idx = self.take()?;
        let page_size = self.backing.page_size;
        // SAFETY: `idx` came off the free list, so this request owns
        // the slot exclusively until the matching `give`. The region
        // `[base + idx*page_size, +page_size)` lies within the
        // registered backing because `idx < page_count` (the free list
        // is seeded with indices `0..usable <= page_count`).
        unsafe {
            let slot = self.backing.base.add(idx as usize * page_size);
            std::ptr::write_bytes(slot, 0, page_size);
        }
        Some(idx)
    }

    fn take(&self) -> Option<u32> {
        self.free.lock().expect("scratch free list poisoned").pop()
    }

    fn give(&self, idx: u32) {
        self.free
            .lock()
            .expect("scratch free list poisoned")
            .push(idx);
    }
}

#[derive(Copy, Clone, PartialEq, Eq)]
enum StreamState {
    /// Have not yet attempted to resolve the request.
    Pending,
    /// Yielded the page; the next poll ends the stream.
    Done,
    /// Already ended (page yielded or error emitted).
    Ended,
}

/// Where a request resolves on this node.
#[derive(Copy, Clone, PartialEq, Eq, Debug)]
enum Route {
    /// This node owns the stripe; serve from store/backend.
    Owner,
    /// This node is an intermediate hop; forward downstream.
    Forward,
    /// Intermediate hop with no remaining hop budget.
    HopLimit,
}

/// Classify a request by finger-table ownership and hop budget. Pure
/// over `(fingers, target, hops_remaining)` so the routing decision is
/// unit-testable without a live fabric.
fn classify(fingers: &FingerTable, target: RingId, hops_remaining: u32) -> Route {
    match fingers.next_hop(target) {
        None => Route::Owner,
        Some(_) if hops_remaining == 0 => Route::HopLimit,
        Some(_) => Route::Forward,
    }
}

/// Response stream for one request. Resolves on first poll, yields
/// exactly one page on success then ends, or yields the error then
/// ends. The scratch page (if reserved) is returned to the free list
/// on drop, which happens after the RPC layer has finished
/// `fi_write`ing it to the peer.
pub struct RecursiveHandlerStream<'a, R, S, B>
where
    S: BlockStore + Send + Sync + 'static,
    B: Backend<Req = R>,
{
    store: Arc<S>,
    scratch: Arc<ScratchBacking>,
    fingers: Option<Arc<FingerTable>>,
    forward: &'a FabricTransport<R, ChainFingerRouter>,
    backend: &'a B,
    req: &'a R,
    key: StripeKey,
    src: BulkRef,
    hops_remaining: u32,
    state: StreamState,
    page_idx: Option<u32>,
}

impl<'a, R, S, B> HandlerStream for RecursiveHandlerStream<'a, R, S, B>
where
    R: Req + Serialize + DeserializeOwned + Send + Sync + 'static,
    S: BlockStore + Send + Sync + 'static,
    B: Backend<Req = R>,
{
    type Error = RecursiveHandlerError;

    fn poll_next(
        mut self: Pin<&mut Self>,
        _cx: &mut Context<'_>,
    ) -> Poll<Option<Result<PageRef, RecursiveHandlerError>>> {
        match self.state {
            StreamState::Done | StreamState::Ended => {
                self.state = StreamState::Ended;
                Poll::Ready(None)
            }
            StreamState::Pending => match self.serve() {
                Ok(page) => {
                    self.state = StreamState::Done;
                    Poll::Ready(Some(Ok(page)))
                }
                Err(e) => {
                    self.state = StreamState::Ended;
                    Poll::Ready(Some(Err(e)))
                }
            },
        }
    }
}

impl<'a, R, S, B> RecursiveHandlerStream<'a, R, S, B>
where
    R: Req + Serialize + DeserializeOwned + Send + Sync + 'static,
    S: BlockStore + Send + Sync + 'static,
    B: Backend<Req = R>,
{
    /// Resolve the request: own it (store/backend) or forward it.
    /// Reserves one scratch page for the data-bearing paths and
    /// records it in `page_idx` so `Drop` reclaims it. The inner async
    /// work (`read_page`, the backend stream, the forward stream) is
    /// driven to completion within this call so the stream holds no
    /// future and is trivially `Send`; blocking here is acceptable
    /// because each request has its own RPC worker thread.
    fn serve(&mut self) -> Result<PageRef, RecursiveHandlerError> {
        let page_size = self.scratch.backing.page_size;
        let target = stripe_to_ring(self.key);
        let route = self
            .fingers
            .as_deref()
            .map(|fingers| classify(fingers, target, self.hops_remaining))
            .unwrap_or(Route::Owner);
        match route {
            Route::HopLimit => {
                crate::metrics::p2p_hop_limit_exceeded();
                Err(RecursiveHandlerError::HopLimitExceeded)
            }
            Route::Owner => {
                crate::metrics::p2p_request(crate::metrics::Disposition::Local);
                let idx = self
                    .scratch
                    .take_zeroed()
                    .ok_or(RecursiveHandlerError::NoScratchPage)?;
                self.page_idx = Some(idx);
                let dst = scratch_page(idx, page_size);
                serve_owned(&self.store, self.backend, self.req, self.src, dst)?;
                Ok(self.clamped_page(idx, page_size))
            }
            Route::Forward => {
                crate::metrics::p2p_request(crate::metrics::Disposition::Forward);
                let idx = self
                    .scratch
                    .take_zeroed()
                    .ok_or(RecursiveHandlerError::NoScratchPage)?;
                self.page_idx = Some(idx);
                let dst = scratch_page(idx, page_size);
                serve_forward(
                    self.forward,
                    self.req,
                    self.src,
                    self.hops_remaining - 1,
                    idx,
                    page_size,
                    dst,
                )?;
                Ok(self.clamped_page(idx, page_size))
            }
        }
    }

    /// The yielded page, clamped to the requested intra-stripe window.
    /// `BulkRef.len` is bounded by `page_size`; clamp defensively so a
    /// malformed request can never make the RPC layer read past the
    /// page.
    fn clamped_page(&self, idx: u32, page_size: usize) -> PageRef {
        let len = (self.src.len as usize).min(page_size) as u32;
        PageRef {
            page_idx: idx,
            offset: 0,
            len,
        }
    }
}

impl<'a, R, S, B> Drop for RecursiveHandlerStream<'a, R, S, B>
where
    S: BlockStore + Send + Sync + 'static,
    B: Backend<Req = R>,
{
    fn drop(&mut self) {
        if let Some(idx) = self.page_idx.take() {
            self.scratch.give(idx);
        }
    }
}

/// Full-page scratch destination for index `idx`.
fn scratch_page(idx: u32, page_size: usize) -> PageRef {
    PageRef {
        page_idx: idx,
        offset: 0,
        len: page_size as u32,
    }
}

/// Owner path: read the page from the local store, falling back to the
/// origin backend on a miss. The page lands in `dst` (a scratch page).
/// Factored out as a free function so it can be unit-tested without a
/// live fabric (it never touches `forward`).
fn serve_owned<R, S, B>(
    store: &Arc<S>,
    backend: &B,
    req: &R,
    src: BulkRef,
    dst: PageRef,
) -> Result<(), RecursiveHandlerError>
where
    R: Req,
    S: BlockStore + Send + Sync + 'static,
    B: Backend<Req = R>,
{
    let hit = block_on_local(store.read_page(req, src.offset, dst))
        .map_err(RecursiveHandlerError::BlockStore)?;
    if hit {
        return Ok(());
    }
    let stream = backend.bulk_get(req, src, std::slice::from_ref(&dst));
    drive_page_stream(stream).map_err(RecursiveHandlerError::Backend)
}

/// Forward path: hand the request to the next hop with TTL `ttl`,
/// landing the downstream page in scratch slot `dst_idx` via RDMA.
///
/// The downstream hop RMA-writes the single response page at
/// `dest_mr_base + ordinal * page_size`; since this relay reserved
/// scratch slot `dst_idx` and later reads that exact slot back to
/// relay upstream, the forward must shift the destination base by
/// `dst_idx * page_size`. Passing only the full-page `dst` (ordinal 0)
/// would land the page in slot 0 and the relay would ship stale bytes
/// from slot `dst_idx`.
fn serve_forward<R>(
    forward: &FabricTransport<R, ChainFingerRouter>,
    req: &R,
    src: BulkRef,
    ttl: u32,
    dst_idx: u32,
    page_size: usize,
    dst: PageRef,
) -> Result<(), RecursiveHandlerError>
where
    R: Req + Serialize + DeserializeOwned + 'static,
{
    let dest_offset = (dst_idx as usize).saturating_mul(page_size);
    let stream = forward.bulk_get_forward(req, src, std::slice::from_ref(&dst), ttl, dest_offset);
    drive_page_stream(stream).map_err(RecursiveHandlerError::Forward)
}

/// Drive a [`PageStream`] to completion with a noop waker on the
/// current thread, returning the first error if any. Page payloads
/// land in the caller-provided `dst` via the stream's side effects, so
/// the yielded `PageRef`s themselves are not inspected here.
fn drive_page_stream<P: PageStream>(stream: P) -> Result<(), crate::bufferpool::Error> {
    let mut stream = std::pin::pin!(stream);
    let waker = noop_waker();
    let mut cx = Context::from_waker(&waker);
    loop {
        match stream.as_mut().poll_next(&mut cx) {
            Poll::Ready(None) => return Ok(()),
            Poll::Ready(Some(Ok(_))) => {}
            Poll::Ready(Some(Err(e))) => return Err(e),
            Poll::Pending => std::thread::sleep(std::time::Duration::from_micros(50)),
        }
    }
}

/// Drive a non-`Send`-bounded future to completion on the current
/// thread with a noop waker. Mirrors [`crate::fabric::PoolHandler`]'s
/// spin-poller; the underlying disk I/O makes progress on its own
/// io_uring threads while this spins.
fn block_on_local<F: std::future::Future>(fut: F) -> F::Output {
    block_on_cooperative(fut, || {
        std::thread::sleep(std::time::Duration::from_micros(50))
    })
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::backend::NullBackend;
    use crate::bufferpool::Error;
    use crate::p2p::{FingerTableConfig, PeerEntry, TopologyTags, node_to_ring};
    use std::sync::atomic::{AtomicUsize, Ordering};

    const PAGE: usize = 4096;
    const SCRATCH_PAGES: usize = 2;

    struct TestReq(StripeKey);

    impl Req for TestReq {
        fn key(&self) -> StripeKey {
            self.0
        }
    }

    /// In-memory `BlockStore` mock: holds page bytes keyed by stripe.
    /// `read_page` copies the recorded fill byte into the destination
    /// scratch page when present (`Ok(true)`); otherwise returns a
    /// miss (`Ok(false)`) or, for a poisoned key, an error.
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
                    // SAFETY: dst.page_idx is within the backing the
                    // test allocated.
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

    /// Origin backend that fills the destination page with a known
    /// byte then yields it once, mimicking a real origin fetch.
    struct FillBackend {
        base: *mut u8,
        page_size: usize,
        fill: u8,
    }

    // SAFETY: single-threaded test; base outlives the test.
    unsafe impl Send for FillBackend {}
    unsafe impl Sync for FillBackend {}

    struct FillStream {
        page: Option<PageRef>,
    }

    impl PageStream for FillStream {
        fn poll_next(
            mut self: Pin<&mut Self>,
            _cx: &mut Context<'_>,
        ) -> Poll<Option<Result<PageRef, Error>>> {
            match self.page.take() {
                Some(p) => Poll::Ready(Some(Ok(p))),
                None => Poll::Ready(None),
            }
        }
    }

    impl Backend for FillBackend {
        type Req = TestReq;
        type Stream<'a> = FillStream;

        fn bulk_get<'a>(
            &'a self,
            _req: &'a Self::Req,
            _src: BulkRef,
            dsts: &'a [PageRef],
        ) -> Self::Stream<'a> {
            let dst = dsts[0];
            // SAFETY: dst.page_idx is within the test backing.
            unsafe {
                let p = self.base.add(dst.page_idx as usize * self.page_size);
                std::ptr::write_bytes(p, self.fill, self.page_size);
            }
            FillStream { page: Some(dst) }
        }
    }

    fn scratch_backing() -> (Backing, *mut u8) {
        let buf = vec![0u8; PAGE * SCRATCH_PAGES].into_boxed_slice();
        let base = Box::leak(buf).as_mut_ptr();
        let backing = Backing {
            base,
            page_size: PAGE,
            page_count: SCRATCH_PAGES,
            keepalive: std::sync::Arc::new(()),
        };
        (backing, base)
    }

    fn mem_store(base: *mut u8, present: &[([u8; 32], u8)], poison: &[[u8; 32]]) -> Arc<MemStore> {
        Arc::new(MemStore {
            base,
            page_size: PAGE,
            present: present.iter().copied().collect(),
            poison: poison.iter().copied().collect(),
            reads: AtomicUsize::new(0),
        })
    }

    fn page_byte(base: *mut u8, idx: u32) -> u8 {
        // SAFETY: idx is within the test backing.
        unsafe { *base.add(idx as usize * PAGE) }
    }

    fn peer(node: u64) -> PeerEntry {
        PeerEntry {
            node: NodeId(node),
            ring: node_to_ring(NodeId(node)),
            tags: TopologyTags(vec!["r".to_string()]),
        }
    }

    fn key_for_ring(ring: u64) -> StripeKey {
        let mut k = [0u8; 32];
        k[..8].copy_from_slice(&ring.to_le_bytes());
        StripeKey(k)
    }

    fn src_for(key: StripeKey) -> BulkRef {
        BulkRef {
            stripe: key,
            offset: 0,
            len: PAGE as u32,
        }
    }

    // --- classify (routing decision, no fabric) ---

    #[test]
    fn classify_single_node_owns_everything() {
        let local = peer(1);
        let fingers = FingerTable::build(local.clone(), &[], FingerTableConfig { k: 8 });
        for target in [0u64, 1, 999, u64::MAX] {
            let r = classify(
                &fingers,
                stripe_to_ring(key_for_ring(target)),
                crate::fabric::MAX_HOPS,
            );
            assert_eq!(r, Route::Owner, "target {target}");
        }
    }

    #[test]
    fn classify_peer_owned_forwards_when_budget_remains() {
        let local = peer(1);
        let other = peer(2);
        let fingers = FingerTable::build(
            local,
            std::slice::from_ref(&other),
            FingerTableConfig { k: 8 },
        );
        let target = stripe_to_ring(key_for_ring(other.ring.0));
        assert_eq!(classify(&fingers, target, 5), Route::Forward);
    }

    #[test]
    fn classify_peer_owned_hits_hop_limit_at_zero() {
        let local = peer(1);
        let other = peer(2);
        let fingers = FingerTable::build(
            local,
            std::slice::from_ref(&other),
            FingerTableConfig { k: 8 },
        );
        let target = stripe_to_ring(key_for_ring(other.ring.0));
        assert_eq!(classify(&fingers, target, 0), Route::HopLimit);
    }

    // --- serve_owned (owner path, no fabric) ---

    #[test]
    fn owner_resident_stripe_yields_store_page() {
        let (_backing, base) = scratch_backing();
        let key = key_for_ring(42);
        let store = mem_store(base, &[(key.0, 0xAB)], &[]);
        let backend = NullBackend::<TestReq>::new();
        let req = TestReq(key);
        let dst = scratch_page(0, PAGE);

        serve_owned(&store, &backend, &req, src_for(key), dst).expect("resident read must succeed");
        assert_eq!(page_byte(base, 0), 0xAB, "store did not fill the page");
        assert_eq!(store.reads.load(Ordering::Relaxed), 1);
    }

    #[test]
    fn owner_miss_falls_back_to_backend_fill() {
        let (_backing, base) = scratch_backing();
        let key = key_for_ring(7);
        // store has no entry -> read_page returns Ok(false) (miss).
        let store = mem_store(base, &[], &[]);
        let backend = FillBackend {
            base,
            page_size: PAGE,
            fill: 0xCD,
        };
        let req = TestReq(key);
        let dst = scratch_page(1, PAGE);

        serve_owned(&store, &backend, &req, src_for(key), dst)
            .expect("backend fallback must succeed");
        assert_eq!(page_byte(base, 1), 0xCD, "backend did not fill the page");
    }

    #[test]
    fn owner_miss_with_null_backend_errors() {
        let (_backing, base) = scratch_backing();
        let key = key_for_ring(9);
        let store = mem_store(base, &[], &[]);
        let backend = NullBackend::<TestReq>::new();
        let req = TestReq(key);
        let dst = scratch_page(0, PAGE);

        match serve_owned(&store, &backend, &req, src_for(key), dst) {
            Err(RecursiveHandlerError::Backend(_)) => {}
            other => panic!("expected Backend error, got {other:?}"),
        }
    }

    #[test]
    fn owner_block_store_error_propagates() {
        let (_backing, base) = scratch_backing();
        let key = key_for_ring(3);
        let store = mem_store(base, &[], &[key.0]);
        let backend = NullBackend::<TestReq>::new();
        let req = TestReq(key);
        let dst = scratch_page(0, PAGE);

        match serve_owned(&store, &backend, &req, src_for(key), dst) {
            Err(RecursiveHandlerError::BlockStore(_)) => {}
            other => panic!("expected BlockStore error, got {other:?}"),
        }
    }

    // --- scratch recycling (cross-request leak guard) ---

    fn fill_page(base: *mut u8, idx: u32, byte: u8) {
        // SAFETY: idx is within the test backing.
        unsafe {
            std::ptr::write_bytes(base.add(idx as usize * PAGE), byte, PAGE);
        }
    }

    fn page_all(base: *mut u8, idx: u32) -> Vec<u8> {
        // SAFETY: idx is within the test backing.
        unsafe { std::slice::from_raw_parts(base.add(idx as usize * PAGE), PAGE).to_vec() }
    }

    #[test]
    fn recycled_scratch_page_is_zeroed_on_checkout() {
        let (backing, base) = scratch_backing();
        let scratch = ScratchBacking {
            backing,
            free: Mutex::new(vec![0]),
        };

        // A prior request leaves a recognizable non-zero pattern across
        // the full extent of the slot, then returns it to the free list.
        let idx = scratch.take().expect("slot available");
        fill_page(base, idx, 0xEE);
        scratch.give(idx);

        // Re-acquiring through the zeroing checkout must hand back a
        // clean page with no residual bytes from the prior request.
        let idx = scratch.take_zeroed().expect("slot available");
        assert!(
            page_all(base, idx).iter().all(|&b| b == 0),
            "recycled scratch page leaked prior request bytes"
        );
    }

    #[test]
    fn short_fill_does_not_relay_prior_bytes() {
        // The store fill writes the whole page here, but the guarantee
        // we exercise is that the unwritten tail of a recycled page is
        // zero: dirty the slot, then drive an owner read whose fill we
        // restrict to the page head, and assert the tail is zero rather
        // than the prior pattern.
        let (backing, base) = scratch_backing();
        let scratch = ScratchBacking {
            backing,
            free: Mutex::new(vec![0]),
        };

        // Prior request dirties the entire slot.
        let idx = scratch.take().expect("slot available");
        fill_page(base, idx, 0xEE);
        scratch.give(idx);

        // New checkout zeroes the slot; a short fill (head only) leaves
        // the tail untouched, so the tail must read as zero rather than
        // the prior 0xEE pattern.
        let idx = scratch.take_zeroed().expect("slot available");
        fill_page_head(base, idx, 0x11, 64);

        let page = page_all(base, idx);
        assert!(page[..64].iter().all(|&b| b == 0x11), "head fill missing");
        assert!(
            page[64..].iter().all(|&b| b == 0),
            "tail leaked prior request bytes instead of zero"
        );
    }

    fn fill_page_head(base: *mut u8, idx: u32, byte: u8, n: usize) {
        // SAFETY: idx is within the test backing and n <= PAGE.
        unsafe {
            std::ptr::write_bytes(base.add(idx as usize * PAGE), byte, n);
        }
    }
}
