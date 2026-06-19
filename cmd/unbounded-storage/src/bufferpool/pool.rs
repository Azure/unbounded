// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::future::Future;
use std::marker::PhantomData;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use crate::bufferpool::free_list::{FreeList, RecvQuarantineHandle};
use crate::bufferpool::inflight::{PageSlot, SlotState, StripeFetch};
use crate::bufferpool::pipeline::{PipelinedRead, StripePlan};
use crate::bufferpool::stream::{LocalBoxFuture, ReadStream, StaticBoxFuture, StreamSrc};
use crate::bufferpool::traits::{BlockStore, BufferPool, Req, Transport};
use crate::bufferpool::types::{BulkRef, Error, PageRef, PoolConfig, StripeKey};
use crate::bufferpool::window::WindowedRead;
use crate::memory::Backing;

/// One per shard. Per the design's runtime model, a single `Pool`
/// runs single-threaded inside its NUMA shard; the embedder pins
/// the executor and is expected to construct the pool off-thread
/// before handing it to the pinned executor for service.
pub struct Pool<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    inner: Rc<PoolInner<T, S, R>>,
}

pub(super) struct PoolInner<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    pub(super) cfg: PoolConfig,
    pub(super) backing: Backing,
    pub(super) page_size: usize,
    pub(super) free: Rc<FreeList>,
    pub(super) inflight: RefCell<HashMap<StripeKey, Rc<RefCell<StripeFetch>>>>,
    pub(super) stream_count: Cell<usize>,
    /// Global speculative-prefetch budget shared by every
    /// [`WindowedRead`]. Counts pages fetched strictly ahead of some
    /// stream's cursor that are not yet consumed. Capped by
    /// `cfg.max_inflight_pages`; the head page never counts.
    pub(super) inflight_prefetch_pages: Cell<usize>,
    pub(super) transport: T,
    pub(super) blockstore: S,
    _r: PhantomData<R>,
}

impl<T, S, R> Pool<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    /// One per NUMA shard. Carves `backing` into pages and calls
    /// `blockstore.register_pages(...)` exactly once. The
    /// `Transport` is expected to have been bound to the same
    /// `backing` out-of-band by the embedder before this call (e.g.
    /// via `fabric::Fabric::register_backing`); the pool no longer
    /// drives that handshake. No async I/O happens here; on a real
    /// RDMA `Transport` the synchronous `ibv_reg_mr` is the
    /// dominant cost (see the design's "Page registration"
    /// section). Embedders should run `Pool::new` off the pinned
    /// executor thread before handing the constructed `Pool` to the
    /// per-shard executor for service.
    pub fn new(
        cfg: PoolConfig,
        backing: Backing,
        transport: T,
        blockstore: S,
    ) -> Result<Self, Error> {
        if backing.page_size == 0 || !backing.page_size.is_power_of_two() {
            return Err(Error::BadConfig("page_size must be a power of two"));
        }
        if backing.page_count == 0 {
            return Err(Error::BadConfig("page_count must be > 0"));
        }
        if backing.page_count > u32::MAX as usize {
            return Err(Error::BadConfig("page_count exceeds u32 (see design)"));
        }
        if backing.base.is_null() {
            return Err(Error::BadConfig("backing.base is null"));
        }

        blockstore.register_pages(&backing)?;

        let page_count_u32 = backing.page_count as u32;
        let page_size = backing.page_size;
        let inner = Rc::new(PoolInner {
            cfg,
            backing,
            page_size,
            free: Rc::new(FreeList::new(page_count_u32)),
            inflight: RefCell::new(HashMap::new()),
            stream_count: Cell::new(0),
            inflight_prefetch_pages: Cell::new(0),
            transport,
            blockstore,
            _r: PhantomData,
        });
        crate::metrics::bufferpool_pages_added(page_count_u32 as i64);
        Ok(Self { inner })
    }

    /// Test-only accessor for the free list size. Also used by the
    /// out-of-tree DST harness under `tests/`.
    pub fn free_pages(&self) -> usize {
        self.inner.free.available()
    }

    /// Build a [`RecvQuarantineHandle`] over this pool's free list, for
    /// installing on the shard's `NetworkRing` (see
    /// `backend::install_recv_quarantine`). It lets a cancelled
    /// fixed-buffer RECV withhold its destination page from reuse until
    /// the kernel is done writing into it, keeping cancellation sound
    /// without blocking the dropping task.
    pub fn recv_quarantine_handle(&self) -> RecvQuarantineHandle {
        RecvQuarantineHandle::new(self.inner.free.clone(), self.inner.page_size)
    }

    /// Test-only accessor for the inflight map size. Also used by
    /// the out-of-tree DST harness under `tests/`.
    pub fn inflight_entries(&self) -> usize {
        self.inner.inflight.borrow().len()
    }

    /// Test-only accessor for the global speculative-prefetch budget
    /// currently reserved across all [`WindowedRead`]s. Used by the
    /// out-of-tree DST harness to assert the budget returns to zero
    /// at quiescence (every reservation is balanced on consume or
    /// drop).
    pub fn prefetch_inflight(&self) -> usize {
        self.inner.inflight_prefetch_pages.get()
    }
}

impl<T, S, R> BufferPool for Pool<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Clone + 'static,
{
    type Req = R;

    async fn read<'p>(
        &'p self,
        req: &'p Self::Req,
        offset: u64,
        len: u64,
    ) -> Result<ReadStream<'p>, Error> {
        let (src, end) = self.admit_stream(req, offset, len, false)?;
        Ok(ReadStream::new(src, offset, end, self.inner.page_size))
    }

    /// Windowed counterpart to [`BufferPool::read`]. Returns a
    /// [`WindowedRead`] that keeps up to `window` `fetch_page`
    /// futures outstanding ahead of its consumer cursor (within
    /// this single stripe) while still delivering `PageGuard`s
    /// strictly in cursor order, one at a time. Speculative
    /// prefetch is bounded by the pool's global
    /// `max_inflight_pages` budget; the head page is always fetched
    /// and never counts against it. `window` is clamped to
    /// `[1, max_inflight_pages + 1]` (head plus the full budget).
    fn read_windowed<'p>(
        &'p self,
        req: &'p R,
        offset: u64,
        len: u64,
        window: usize,
    ) -> Result<WindowedRead<'p>, Error> {
        let (src, end) = self.admit_stream(req, offset, len, false)?;
        let max_inflight = self.inner.cfg.max_inflight_pages;
        let window = window.clamp(1, max_inflight.saturating_add(1));
        Ok(WindowedRead::new(
            src,
            offset,
            end,
            self.inner.page_size,
            window,
            max_inflight,
        ))
    }

    /// Cross-stripe pipelined read. Lazily admits one stream per
    /// slice (bounded to roughly `window` concurrently-admitted
    /// streams via [`PipelinedRead`]'s eager slice release), so a
    /// large multi-stripe object does not exhaust
    /// `max_concurrent_streams` up front. Zero-length slices are
    /// dropped so the global page geometry is exact.
    fn read_pipelined<'p>(
        &'p self,
        stripes: Vec<StripePlan<R>>,
        window: usize,
    ) -> Result<PipelinedRead<'p>, Error> {
        let page_size = self.inner.page_size as u64;
        let max_inflight = self.inner.cfg.max_inflight_pages;
        let window = window.clamp(1, max_inflight.saturating_add(1));

        // Two index-aligned vectors: `slices` feeds `PipelinedRead`'s
        // geometry, `admit_inputs` feeds the lazy admission closure.
        // Both skip empty slices so indices stay in lockstep.
        let mut slices: Vec<(u64, u64)> = Vec::with_capacity(stripes.len());
        let mut admit_inputs: Vec<(R, u64, u64)> = Vec::with_capacity(stripes.len());
        for sp in stripes {
            if sp.intra_len == 0 {
                continue;
            }
            slices.push((sp.intra_offset, sp.intra_len));
            admit_inputs.push((sp.req, sp.intra_offset, sp.intra_len));
        }

        let admit = move |s: usize| -> Result<Rc<dyn StreamSrc + 'p>, Error> {
            let (req, offset, len) = &admit_inputs[s];
            let (src, _end) = self.admit_stream(req, *offset, *len, false)?;
            Ok(src)
        };

        Ok(PipelinedRead::new(
            Box::new(admit),
            slices,
            page_size,
            window,
            max_inflight,
        ))
    }
}

impl<T, S, R> Pool<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + Clone + 'static,
{
    /// Shared admission path for [`BufferPool::read`] and
    /// [`BufferPool::read_windowed`]: enforces `max_concurrent_streams`,
    /// bumps the stream count, gets or creates the stripe's
    /// `StripeFetch`, and builds the type-erased `StreamSrc` that
    /// owns an `Rc<R>` clone of the request. Returns the erased
    /// source and the exclusive end offset.
    fn admit_stream(
        &self,
        req: &R,
        offset: u64,
        len: u64,
        nonblocking: bool,
    ) -> Result<(Rc<dyn StreamSrc + 'static>, u64), Error> {
        let cur = self.inner.stream_count.get();
        if cur >= self.inner.cfg.max_concurrent_streams {
            return Err(Error::StreamLimit);
        }
        self.inner.stream_count.set(cur + 1);

        let key = req.key();
        let fetch = {
            let mut inflight = self.inner.inflight.borrow_mut();
            inflight
                .entry(key)
                .or_insert_with(|| Rc::new(RefCell::new(StripeFetch::new())))
                .clone()
        };
        fetch.borrow_mut().stream_refcount += 1;

        let end = offset.saturating_add(len);
        let src: Rc<dyn StreamSrc + 'static> = Rc::new(StreamSrcImpl::new(
            self.inner.clone(),
            Rc::new(req.clone()),
            key,
            fetch,
            nonblocking,
        ));
        Ok((src, end))
    }

    /// Owned-guard read path for cross-shard fan-out. Unlike
    /// [`BufferPool::read`], the returned [`ReadStream`] is `'static`
    /// and yields `PageGuard<'static>` via
    /// [`ReadStream::next_page_owned`], so a stripe's owner shard can
    /// pin the pages it fetches and hold them past this stream's own
    /// lifetime while the coordinator shard streams them to the
    /// client over the network, releasing each pin only once its
    /// zero-copy send completes.
    ///
    /// Because those pins are held across a cross-shard network round
    /// trip, this path allocates its head page non-blockingly: if no
    /// backing page is free without parking it returns [`Error::Busy`]
    /// rather than queueing on the free list. A parked owned head
    /// could otherwise pin a stripe while the remote coordinator owed
    /// the page is itself blocked behind another owner, forming a
    /// cross-shard hold-and-wait deadlock. `Busy` is transient: the
    /// coordinator re-dispatches the fetch after yielding.
    pub fn read_owned(&self, req: &R, offset: u64, len: u64) -> Result<ReadStream<'static>, Error> {
        let (src, end) = self.admit_stream(req, offset, len, true)?;
        Ok(ReadStream::new(src, offset, end, self.inner.page_size))
    }
}

// ---------------------------------------------------------------------------
// StreamSrc: type-erased per-stream view onto the typed PoolInner.
// ---------------------------------------------------------------------------

pub(super) struct StreamSrcImpl<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    inner: Rc<PoolInner<T, S, R>>,
    /// Owned (`Rc`) clone of the caller's request. Owned rather than
    /// borrowed so [`StreamSrc::fetch_page_owned`] can hand out a
    /// `'static` fetch future that a `WindowedRead` parks alongside
    /// this source.
    req: Rc<R>,
    key: StripeKey,
    fetch: Rc<RefCell<StripeFetch>>,
    /// Tracks whether `decrement_stream` has already run (it must
    /// happen exactly once per `ReadStream`). Drop ordering of
    /// `Rc<StreamSrcImpl>` is "last `PageGuard` or last
    /// `ReadStream` reference"; the cleanup belongs to the stream,
    /// not to outstanding guards, so we do it eagerly via
    /// `decrement_stream`.
    stream_decremented: Cell<bool>,
    /// When set, this stream's head fetch must not park on the free
    /// list: it allocates via `FreeList::try_alloc_head` and fails
    /// fast with [`Error::Busy`] if no page is free. Set only by
    /// [`Pool::read_owned`] (the cross-shard owned-guard path), where
    /// a parked head could otherwise pin a stripe across a network
    /// round trip and form a cross-shard hold-and-wait cycle. Local
    /// (`read`/`read_windowed`/`read_pipelined`) streams leave it
    /// false and park blocking as before.
    nonblocking: bool,
}

impl<T, S, R> StreamSrcImpl<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    pub(super) fn new(
        inner: Rc<PoolInner<T, S, R>>,
        req: Rc<R>,
        key: StripeKey,
        fetch: Rc<RefCell<StripeFetch>>,
        nonblocking: bool,
    ) -> Self {
        Self {
            inner,
            req,
            key,
            fetch,
            stream_decremented: Cell::new(false),
            nonblocking,
        }
    }
}

impl<T, S, R> StreamSrc for StreamSrcImpl<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    fn page_size(&self) -> usize {
        self.inner.page_size
    }

    fn base(&self) -> *mut u8 {
        self.inner.backing.base
    }

    fn key(&self) -> StripeKey {
        self.key
    }

    fn fetch_page<'a>(&'a self, page_no: u64) -> LocalBoxFuture<'a, Result<u32, Error>> {
        // The owned future is already `'static`; coerce it to the
        // borrowed `'a` the trait asks for. Single source of truth
        // for the fetch machine. The borrowed (ReadStream) path
        // never prefetches, so it is never speculative: its leader
        // allocates blocking.
        self.fetch_page_owned(page_no, false)
    }

    fn fetch_page_owned(
        &self,
        page_no: u64,
        speculative: bool,
    ) -> StaticBoxFuture<Result<u32, Error>> {
        Box::pin(fetch_page(
            self.inner.clone(),
            self.fetch.clone(),
            self.key,
            self.req.clone(),
            page_no,
            speculative,
            self.nonblocking,
        ))
    }

    fn release_guard(&self, page_no: u64) {
        release_guard(&self.inner, self.key, &self.fetch, page_no);
    }

    fn decrement_stream(&self) {
        if self.stream_decremented.replace(true) {
            return;
        }
        decrement_stream(&self.inner, self.key, &self.fetch);
        let prev = self.inner.stream_count.get();
        self.inner.stream_count.set(prev.saturating_sub(1));
    }

    fn try_reserve_prefetch(&self, max: usize) -> bool {
        // Reserve one backing page per active stream for its in-order
        // head, and only let speculation use pages beyond that. The
        // global budget therefore cannot exceed `page_count -
        // stream_count`. Without this bound a stream can pin pages it
        // prefetched ahead in its `ready` buffer while another
        // stream's head starves on the free list, a circular
        // hold-and-wait deadlock under page pressure (the head can
        // never get a backing page, so it can never advance to
        // release the page that would unblock its peer). Capping the
        // speculative footprint at `page_count - stream_count`
        // guarantees at least `stream_count` pages remain reachable
        // by heads, so with the FIFO hand-off free list every blocked
        // head eventually allocates.
        let head_reserve = self
            .inner
            .backing
            .page_count
            .saturating_sub(self.inner.stream_count.get());
        let effective = max.min(head_reserve);
        let cur = self.inner.inflight_prefetch_pages.get();
        if cur < effective {
            self.inner.inflight_prefetch_pages.set(cur + 1);
            crate::metrics::bufferpool_prefetch_delta(1);
            true
        } else {
            false
        }
    }

    fn release_prefetch(&self) {
        let cur = self.inner.inflight_prefetch_pages.get();
        self.inner
            .inflight_prefetch_pages
            .set(cur.saturating_sub(1));
        if cur > 0 {
            crate::metrics::bufferpool_prefetch_delta(-1);
        }
    }
}

// ---------------------------------------------------------------------------
// Cleanup helpers.
// ---------------------------------------------------------------------------

fn release_guard<T, S, R>(
    inner: &Rc<PoolInner<T, S, R>>,
    key: StripeKey,
    fetch: &Rc<RefCell<StripeFetch>>,
    page_no: u64,
) where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    let slot = match fetch.borrow().pages.get(&page_no).cloned() {
        Some(s) => s,
        None => return,
    };
    let prev = slot.consumer_holds.get();
    slot.consumer_holds.set(prev.saturating_sub(1));
    recycle_if_terminal(inner, key, fetch, &slot, page_no);
}

// ---------------------------------------------------------------------------
// ConsumerHold: RAII for `PageSlot::consumer_holds`.
// ---------------------------------------------------------------------------

/// RAII bump of `slot.consumer_holds`. Construction increments;
/// drop decrements and runs `recycle_if_terminal`. Used by parked
/// subscribers and the leader during I/O+tee to keep the slot
/// pinned across `.await` points. On success the hold is
/// transferred to the caller via [`ConsumerHold::forget`] and
/// balanced later by [`StreamSrc::release_guard`] when the
/// resulting `PageGuard` drops.
struct ConsumerHold<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    inner: Rc<PoolInner<T, S, R>>,
    fetch: Rc<RefCell<StripeFetch>>,
    slot: Rc<PageSlot>,
    key: StripeKey,
    page_no: u64,
    active: bool,
}

impl<T, S, R> ConsumerHold<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    fn new(
        inner: Rc<PoolInner<T, S, R>>,
        fetch: Rc<RefCell<StripeFetch>>,
        slot: Rc<PageSlot>,
        key: StripeKey,
        page_no: u64,
    ) -> Self {
        slot.consumer_holds.set(slot.consumer_holds.get() + 1);
        Self {
            inner,
            fetch,
            slot,
            key,
            page_no,
            active: true,
        }
    }

    /// Transfer ownership of the bump out of this guard. The caller
    /// is responsible for balancing it (typically via the
    /// `PageGuard` returned to the stream consumer).
    fn forget(mut self) {
        self.active = false;
    }
}

impl<T, S, R> Drop for ConsumerHold<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    fn drop(&mut self) {
        if !self.active {
            return;
        }
        let prev = self.slot.consumer_holds.get();
        self.slot.consumer_holds.set(prev.saturating_sub(1));
        recycle_if_terminal(&self.inner, self.key, &self.fetch, &self.slot, self.page_no);
    }
}

/// Returns `slot`'s page to the free list and removes the slot
/// (and possibly the entire `StripeFetch`) if it is in a terminal
/// state and no one is holding it. Shared between [`release_guard`]
/// and [`ConsumerHold::drop`] so the cleanup invariant lives in
/// one place.
fn recycle_if_terminal<T, S, R>(
    inner: &Rc<PoolInner<T, S, R>>,
    key: StripeKey,
    fetch: &Rc<RefCell<StripeFetch>>,
    slot: &Rc<PageSlot>,
    page_no: u64,
) where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    if !(slot.is_recyclable() && slot_is_terminal(slot)) {
        return;
    }
    let release_pi = {
        let mut f = fetch.borrow_mut();
        // Re-check under the borrow to guard against a concurrent
        // (re-entrant) update; in single-threaded use this is just
        // defensive but it is cheap.
        match f.pages.get(&page_no) {
            Some(s) if Rc::ptr_eq(s, slot) && s.is_recyclable() && slot_is_terminal(s) => {
                f.pages.remove(&page_no).and_then(|s| s.page_idx.get())
            }
            _ => None,
        }
    };
    if let Some(pi) = release_pi {
        inner.free.release(pi);
    }
    let should_remove = {
        let f = fetch.borrow();
        f.stream_refcount == 0 && f.pages.is_empty()
    };
    if should_remove {
        inner.inflight.borrow_mut().remove(&key);
    }
}

fn decrement_stream<T, S, R>(
    inner: &Rc<PoolInner<T, S, R>>,
    key: StripeKey,
    fetch: &Rc<RefCell<StripeFetch>>,
) where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    let to_release: Vec<u32> = {
        let mut f = fetch.borrow_mut();
        f.stream_refcount = f.stream_refcount.saturating_sub(1);
        let mut released = Vec::new();
        if f.stream_refcount == 0 {
            // Last stream on this stripe is gone, so no future can
            // still be mid-flight against any of its slots (each
            // stream drops its pending fetch futures before
            // decrementing). Release every recyclable slot's page,
            // not just terminal ones: a speculative `WindowedRead`
            // prefetch that became leader and allocated a page, then
            // was dropped mid-flight, leaves its slot in `Idle` with
            // `page_idx` set. With no stream left to take over
            // leadership and reuse it, that page must be reclaimed
            // here or it leaks. `is_recyclable()` still guards
            // against tearing down a slot with an outstanding hold.
            f.pages.retain(|_, slot| {
                if slot.is_recyclable() {
                    if let Some(pi) = slot.page_idx.get() {
                        released.push(pi);
                    }
                    false
                } else {
                    true
                }
            });
        }
        released
    };

    for pi in to_release {
        inner.free.release(pi);
    }

    let should_remove = {
        let f = fetch.borrow();
        f.stream_refcount == 0 && f.pages.is_empty()
    };
    if should_remove {
        inner.inflight.borrow_mut().remove(&key);
    }
}

fn slot_is_terminal(slot: &PageSlot) -> bool {
    matches!(*slot.state.borrow(), SlotState::Ready | SlotState::Error(_))
}

// ---------------------------------------------------------------------------
// Single-flight fetch.
// ---------------------------------------------------------------------------

/// Drives one logical page's I/O through the single-flight state
/// machine. Cooperative leadership: the first poller to find the
/// slot in `Idle` becomes the leader and runs the I/O; later
/// pollers park in `Loading`. If the leader's future drops before
/// completing, the [`LeaderGuard`] flips state back to `Idle`,
/// wakes parked subscribers, and the next one takes over.
async fn fetch_page<T, S, R>(
    inner: Rc<PoolInner<T, S, R>>,
    fetch: Rc<RefCell<StripeFetch>>,
    key: StripeKey,
    req: Rc<R>,
    page_no: u64,
    speculative: bool,
    nonblocking: bool,
) -> Result<u32, Error>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    let slot: Rc<PageSlot> = {
        let mut f = fetch.borrow_mut();
        f.pages
            .entry(page_no)
            .or_insert_with(|| Rc::new(PageSlot::new(page_no)))
            .clone()
    };

    loop {
        let action = {
            let mut st = slot.state.borrow_mut();
            match &mut *st {
                SlotState::Ready => Action::Ready,
                SlotState::Error(e) => Action::Error(e.clone()),
                SlotState::Loading(_) => Action::Park,
                SlotState::Idle => {
                    *st = SlotState::Loading(Vec::new());
                    Action::Lead
                }
            }
        };

        match action {
            Action::Ready => {
                let pi = slot
                    .page_idx
                    .get()
                    .expect("page_idx must be set when slot is Ready");
                // Bump and immediately transfer to the caller; the
                // returned `PageGuard` balances it via
                // `StreamSrc::release_guard`.
                ConsumerHold::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no)
                    .forget();
                return Ok(pi);
            }
            Action::Error(e) => return Err(e),
            Action::Park => {
                crate::metrics::bufferpool_coalesced();
                // `ParkOnSlot` pre-bumps `consumer_holds` so the
                // leader's `PageGuard` drop cannot recycle the slot
                // between wake and re-poll. The hold is transferred
                // to us on `Ready`, dropped on `Error`/`Retry`.
                let park =
                    ParkOnSlot::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no);
                match park.await {
                    ParkOutcome::Ready(hold) => {
                        let pi = slot
                            .page_idx
                            .get()
                            .expect("page_idx must be set when slot is Ready");
                        hold.forget();
                        return Ok(pi);
                    }
                    ParkOutcome::Error(e) => return Err(e),
                    ParkOutcome::Retry => continue,
                }
            }
            Action::Lead => {
                // Leader-owned `ConsumerHold` covers the entire I/O
                // phase plus the tee. Drop order on cancel: locals
                // declared first drop last, so `leader_guard` flips
                // state and wakes parked subscribers before
                // `leader_hold` drops and possibly recycles.
                let leader_hold =
                    ConsumerHold::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no);
                let mut leader_guard = LeaderGuard {
                    slot: &slot,
                    free: &inner.free,
                    completed: false,
                };
                if slot.page_idx.get().is_none() {
                    // Allocate the backing page lazily, here on the
                    // leader's first poll, so a fetch future that is
                    // launched but never polled holds no page. A
                    // speculative (prefetch) fetch must never park on
                    // the free list: it tries non-blockingly and, if
                    // no page is free, backs off so the head keeps
                    // priority on scarce pages. Returning here drops
                    // `leader_guard` (resetting the slot to `Idle` and
                    // waking parked subscribers) and `leader_hold`, so
                    // the slot is left clean for a later retry. A
                    // non-speculative (head) fetch blocks until a page
                    // frees; it is the only path that parks on the
                    // free list, which is what keeps prefetch from
                    // starving any stream's head and deadlocking. The
                    // one exception is a `nonblocking` head (the
                    // cross-shard owned-guard path), which also refuses
                    // to park and fails fast with `Busy`; see below.
                    let pi = if speculative {
                        // Keep one reserve page free per active stream
                        // so every stream's head can always allocate
                        // without parking. This is what makes the
                        // prefetch design deadlock-free: a starvation
                        // cycle requires some head blocked on
                        // `free.alloc` while speculation pins the pages
                        // it needs; refusing speculation that last
                        // reserve guarantees no head ever parks.
                        let reserve = inner.stream_count.get();
                        match inner.free.try_alloc_spare(reserve) {
                            Some(pi) => pi,
                            None => return Err(Error::PrefetchBackoff),
                        }
                    } else if nonblocking {
                        // Cross-shard (owned-guard) head: must never
                        // park on the free list, or it could pin this
                        // stripe across a network round trip while the
                        // owed coordinator is itself stalled behind
                        // another owner's pins, a cross-shard
                        // hold-and-wait cycle. Fail fast with `Busy`
                        // (the coordinator retries) instead. Still
                        // yields to parked local heads via
                        // `try_alloc_head`, so it cannot starve them.
                        match inner.free.try_alloc_head() {
                            Some(pi) => pi,
                            None => return Err(Error::Busy),
                        }
                    } else {
                        inner.free.alloc().await
                    };
                    slot.page_idx.set(Some(pi));
                }
                let pi = slot.page_idx.get().expect("page_idx set above");
                let dst = PageRef {
                    page_idx: pi,
                    offset: 0,
                    len: inner.page_size as u32,
                };
                let stripe_off = page_no
                    .checked_mul(inner.page_size as u64)
                    .ok_or(Error::OffsetOutOfRange)?;

                // Phase 1: get bytes into the page (blocking the
                // leader; parked subscribers wait). On error,
                // transition to `Error` and propagate.
                let fetch_result: Result<bool, Error> = async {
                    let hit = inner.blockstore.read_page(key, stripe_off, dst).await?;
                    if !hit {
                        let bulk = BulkRef {
                            stripe: key,
                            offset: stripe_off,
                            len: inner.page_size as u32,
                        };
                        inner.transport.fetch_one(req.as_ref(), bulk, dst).await?;
                    }
                    Ok(hit)
                }
                .await;

                let hit = match fetch_result {
                    Ok(h) => {
                        crate::metrics::bufferpool_request(if h {
                            crate::metrics::Lookup::Hit
                        } else {
                            crate::metrics::Lookup::Miss
                        });
                        // A blockstore hit satisfied the RAM miss from
                        // local disk. The peer/origin sources are
                        // recorded inside `Transport::bulk_get` on the
                        // miss path, where that distinction is known.
                        if h {
                            crate::metrics::bufferpool_miss_source(
                                crate::metrics::MissSource::Disk,
                            );
                        }
                        h
                    }
                    Err(e) => {
                        let wakers = take_loading_wakers(&slot);
                        *slot.state.borrow_mut() = SlotState::Error(e.clone());
                        leader_guard.completed = true;
                        drop(leader_guard);
                        for w in wakers {
                            w.wake();
                        }
                        // `leader_hold` drops at the `return`,
                        // releasing the bump and (if no parked
                        // subscribers are still holding their own
                        // bumps) recycling the page via
                        // `recycle_if_terminal`.
                        return Err(e);
                    }
                };

                // Phase 2: mark Ready and wake parked subscribers
                // BEFORE running the tee, so non-leader subscribers
                // can consume bytes concurrently with the leader's
                // `write_page` (matches designs/bufferpool.md
                // "Pull-through with tee"). The page stays pinned
                // across the tee via `tee_pending` plus the
                // leader's `ConsumerHold`.
                let need_tee = !hit;
                if need_tee {
                    slot.tee_pending.set(true);
                }
                let wakers = take_loading_wakers(&slot);
                *slot.state.borrow_mut() = SlotState::Ready;
                leader_guard.completed = true;
                drop(leader_guard);
                for w in wakers {
                    w.wake();
                }

                // Phase 3: leader drives the tee. If the leader's
                // future is dropped here, `TeePendingGuard` clears
                // `tee_pending` first, then `leader_hold` drops and
                // runs the same recycle path as `release_guard`.
                // `write_page` errors are best-effort for v1 (see
                // designs/bufferpool.md TODO(partial-failure)).
                if need_tee {
                    let _tee_pending_guard = TeePendingGuard { slot: &slot };
                    let _ = inner.blockstore.write_page(key, stripe_off, dst).await;
                }

                leader_hold.forget();
                return Ok(pi);
            }
        }
    }
}

enum Action {
    Ready,
    Error(Error),
    Park,
    Lead,
}

struct LeaderGuard<'a> {
    slot: &'a Rc<PageSlot>,
    /// Free list, so a leader cancelled mid-flight with no subscriber
    /// to take over can return its page instead of orphaning it.
    free: &'a FreeList,
    completed: bool,
}

impl<'a> Drop for LeaderGuard<'a> {
    fn drop(&mut self) {
        if self.completed {
            return;
        }
        // Leader future was dropped before reaching Ready (during
        // read_page or bulk_get). Reset to Idle so a parked
        // subscriber takes over leadership.
        let wakers = take_loading_wakers(self.slot);
        // If a subscriber is parked, leave `page_idx` set so whichever
        // one re-leads reuses the already-allocated page (their
        // pre-bumped `consumer_holds` keep the slot pinned across the
        // handoff). If there is no subscriber to take over, the page
        // would be orphaned in an `Idle` slot that nobody is driving,
        // holding a backing page that no fetch will ever recycle. That
        // can deadlock other streams blocked on the free list, so
        // return it here instead. A later fetch of this page finds the
        // slot `Idle` with `page_idx == None` and allocates afresh.
        if wakers.is_empty() {
            if let Some(pi) = self.slot.page_idx.take() {
                self.free.release(pi);
            }
        }
        *self.slot.state.borrow_mut() = SlotState::Idle;
        for w in wakers {
            w.wake();
        }
    }
}

/// Clears `slot.tee_pending` on drop. Drop order in [`fetch_page`]
/// ensures this runs before the leader's `ConsumerHold` drops, so
/// any subsequent recycle sees `tee_pending == false`.
struct TeePendingGuard<'a> {
    slot: &'a Rc<PageSlot>,
}

impl<'a> Drop for TeePendingGuard<'a> {
    fn drop(&mut self) {
        self.slot.tee_pending.set(false);
    }
}

/// Drains the parked-waker list out of a slot in `Loading`. Returns
/// an empty `Vec` if the slot has already moved on.
fn take_loading_wakers(slot: &Rc<PageSlot>) -> Vec<std::task::Waker> {
    let mut st = slot.state.borrow_mut();
    match &mut *st {
        SlotState::Loading(w) => std::mem::take(w),
        _ => Vec::new(),
    }
}

struct ParkOnSlot<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    inner: Rc<PoolInner<T, S, R>>,
    fetch: Rc<RefCell<StripeFetch>>,
    slot: Rc<PageSlot>,
    key: StripeKey,
    page_no: u64,
    /// `Some` once we have registered our waker and pre-bumped
    /// `consumer_holds`. Taken out and either transferred to the
    /// caller on `Ready` or dropped on `Error`/`Retry`/cancellation.
    hold: Option<ConsumerHold<T, S, R>>,
}

/// Outcome of awaiting [`ParkOnSlot`].
enum ParkOutcome<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    /// Slot reached `Ready`. The caller must `forget()` the hold so
    /// the bump survives to be balanced by the consumer's
    /// `PageGuard`.
    Ready(ConsumerHold<T, S, R>),
    Error(Error),
    /// Slot left `Loading` via leader-future drop. The caller
    /// re-enters the state machine.
    Retry,
}

impl<T, S, R> ParkOnSlot<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    fn new(
        inner: Rc<PoolInner<T, S, R>>,
        fetch: Rc<RefCell<StripeFetch>>,
        slot: Rc<PageSlot>,
        key: StripeKey,
        page_no: u64,
    ) -> Self {
        Self {
            inner,
            fetch,
            slot,
            key,
            page_no,
            hold: None,
        }
    }
}

impl<T, S, R> Future for ParkOnSlot<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    type Output = ParkOutcome<T, S, R>;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // `ParkOnSlot` holds only `Rc`/`Option`/`Cell` fields; it
        // is `Unpin`.
        let this = self.get_mut();

        // Pre-bump `consumer_holds` once. `Action::Park` observed
        // `Loading` synchronously, but the leader may have completed
        // before we get here; bump unconditionally so the outcome
        // paths below always have a hold to consume.
        if this.hold.is_none() {
            this.hold = Some(ConsumerHold::new(
                this.inner.clone(),
                this.fetch.clone(),
                this.slot.clone(),
                this.key,
                this.page_no,
            ));
        }

        // Register our waker into the *current* `Loading` waker list
        // on every poll, not just the first. A slot can cycle
        // `Loading -> Idle -> Loading` while we stay parked: when a
        // leader future is dropped pre-`Ready` (e.g. a windowed reader
        // cancels its speculative leader), `LeaderGuard::drop` drains
        // the waker list and resets the slot to `Idle`, and a woken
        // subscriber then re-leads with a fresh, empty waker list. A
        // subscriber that registered only on its first poll would no
        // longer be in that new list and would never be woken when the
        // new leader completes, deadlocking. The executor hands out a
        // fresh waker per poll, so re-registration appends; the list
        // is bounded per `Loading` episode (drained on every
        // transition) and the executor de-duplicates ready task ids.
        let outcome = {
            let mut st = this.slot.state.borrow_mut();
            match &mut *st {
                SlotState::Loading(wakers) => {
                    wakers.push(cx.waker().clone());
                    return Poll::Pending;
                }
                SlotState::Ready => Outcome::Ready,
                SlotState::Error(e) => Outcome::Error(e.clone()),
                SlotState::Idle => Outcome::Retry,
            }
        };
        match outcome {
            Outcome::Ready => {
                let hold = this.hold.take().expect("hold registered above");
                Poll::Ready(ParkOutcome::Ready(hold))
            }
            Outcome::Error(e) => {
                let _ = this.hold.take();
                Poll::Ready(ParkOutcome::Error(e))
            }
            Outcome::Retry => {
                let _ = this.hold.take();
                Poll::Ready(ParkOutcome::Retry)
            }
        }
    }
}

/// Internal outcome enum used inside `ParkOnSlot::poll` to keep
/// the `slot.state` borrow scope minimal.
enum Outcome {
    Ready,
    Error(Error),
    Retry,
}
