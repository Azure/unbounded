// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

use std::cell::{Cell, RefCell};
use std::collections::HashMap;
use std::future::Future;
use std::marker::PhantomData;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll};

use crate::bufferpool::free_list::FreeList;
use crate::bufferpool::inflight::{PageSlot, SlotState, StripeFetch};
use crate::bufferpool::stream::{LocalBoxFuture, ReadStream, StreamSrc};
use crate::bufferpool::traits::{BlockStore, BufferPool, Req, Transport};
use crate::bufferpool::types::{Backing, BulkRef, Error, PageRef, PoolConfig, StripeKey};

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
    pub(super) free: FreeList,
    pub(super) inflight: RefCell<HashMap<StripeKey, Rc<RefCell<StripeFetch>>>>,
    pub(super) stream_count: Cell<usize>,
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
    /// One per NUMA shard. Carves `backing` into pages, calls
    /// `transport.register_pages(...)` and
    /// `blockstore.register_pages(...)` exactly once. No async I/O
    /// happens here; on a real RDMA `Transport` the synchronous
    /// `ibv_reg_mr` is the dominant cost (see the design's
    /// "Page registration" section). Embedders should run
    /// `Pool::new` off the pinned executor thread before handing
    /// the constructed `Pool` to the per-shard executor for
    /// service.
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

        transport.register_pages(backing.base, backing.page_size, backing.page_count)?;
        blockstore.register_pages(backing.base, backing.page_size, backing.page_count)?;

        let page_count_u32 = backing.page_count as u32;
        let page_size = backing.page_size;
        let inner = Rc::new(PoolInner {
            cfg,
            backing,
            page_size,
            free: FreeList::new(page_count_u32),
            inflight: RefCell::new(HashMap::new()),
            stream_count: Cell::new(0),
            transport,
            blockstore,
            _r: PhantomData,
        });
        Ok(Self { inner })
    }

    /// Test-only accessor for the free list size.
    #[cfg(test)]
    pub(super) fn free_pages(&self) -> usize {
        self.inner.free.available()
    }

    /// Test-only accessor for the inflight map size.
    #[cfg(test)]
    pub(super) fn inflight_entries(&self) -> usize {
        self.inner.inflight.borrow().len()
    }
}

impl<T, S, R> BufferPool for Pool<T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    type Req = R;

    async fn read<'p>(
        &'p self,
        req: &'p Self::Req,
        offset: u64,
        len: u64,
    ) -> Result<ReadStream<'p>, Error> {
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
        let src: Rc<dyn StreamSrc + 'p> =
            Rc::new(StreamSrcImpl::new(self.inner.clone(), req, key, fetch));
        Ok(ReadStream::new(src, offset, end, self.inner.page_size))
    }
}

// ---------------------------------------------------------------------------
// StreamSrc: type-erased per-stream view onto the typed PoolInner.
// ---------------------------------------------------------------------------

pub(super) struct StreamSrcImpl<'p, T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    inner: Rc<PoolInner<T, S, R>>,
    req: &'p R,
    key: StripeKey,
    fetch: Rc<RefCell<StripeFetch>>,
    /// Tracks whether `decrement_stream` has already run (it must
    /// happen exactly once per `ReadStream`). Drop ordering of
    /// `Rc<StreamSrcImpl>` is "last `PageGuard` or last
    /// `ReadStream` reference"; the cleanup belongs to the stream,
    /// not to outstanding guards, so we do it eagerly via
    /// `decrement_stream`.
    stream_decremented: Cell<bool>,
}

impl<'p, T, S, R> StreamSrcImpl<'p, T, S, R>
where
    T: Transport<R> + 'static,
    S: BlockStore + 'static,
    R: Req + 'static,
{
    pub(super) fn new(
        inner: Rc<PoolInner<T, S, R>>,
        req: &'p R,
        key: StripeKey,
        fetch: Rc<RefCell<StripeFetch>>,
    ) -> Self {
        Self {
            inner,
            req,
            key,
            fetch,
            stream_decremented: Cell::new(false),
        }
    }
}

impl<'p, T, S, R> StreamSrc for StreamSrcImpl<'p, T, S, R>
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
        Box::pin(fetch_page(
            self.inner.clone(),
            self.fetch.clone(),
            self.key,
            self.req,
            page_no,
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

/// Returns `slot`'s page to the free list and removes the slot
/// (and possibly the entire `StripeFetch`) if it is in a terminal
/// state and no one is holding it. Shared between [`release_guard`]
/// and [`TeeGuard::drop`] so the cleanup invariant lives in one
/// place.
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
            f.pages.retain(|_, slot| {
                if slot.is_recyclable() && slot_is_terminal(slot) {
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
    req: &R,
    page_no: u64,
) -> Result<u32, Error>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
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
                slot.consumer_holds.set(slot.consumer_holds.get() + 1);
                return Ok(pi);
            }
            Action::Error(e) => return Err(e),
            Action::Park => {
                ParkOnSlot { slot: &slot }.await;
                continue;
            }
            Action::Lead => {
                let mut leader_guard = LeaderGuard {
                    slot: &slot,
                    completed: false,
                };

                if slot.page_idx.get().is_none() {
                    let pi = inner.free.alloc().await;
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
                        inner.transport.bulk_get(req, bulk, dst).await?;
                    }
                    Ok(hit)
                }
                .await;

                let hit = match fetch_result {
                    Ok(h) => h,
                    Err(e) => {
                        let wakers = take_loading_wakers(&slot);
                        *slot.state.borrow_mut() = SlotState::Error(e.clone());
                        leader_guard.completed = true;
                        drop(leader_guard);
                        for w in wakers {
                            w.wake();
                        }
                        return Err(e);
                    }
                };

                // Phase 2: mark Ready and wake parked subscribers
                // BEFORE running the tee, so non-leader subscribers
                // can consume bytes concurrently with the leader's
                // `write_page` (matches designs/bufferpool.md
                // "Pull-through with tee"). The page stays pinned
                // across the tee via `tee_pending`.
                let need_tee = !hit;
                if need_tee {
                    slot.tee_pending.set(true);
                }
                let wakers = take_loading_wakers(&slot);
                *slot.state.borrow_mut() = SlotState::Ready;
                slot.consumer_holds.set(slot.consumer_holds.get() + 1);
                leader_guard.completed = true;
                drop(leader_guard);
                for w in wakers {
                    w.wake();
                }

                // Phase 3: leader drives the tee. The leader keeps
                // `consumer_holds` bumped above so the page stays
                // pinned across the tee even if no other subscriber
                // is around. If the leader's future is dropped here
                // the `TeeGuard` clears `tee_pending` *and* releases
                // the leader's pre-emptive consumer hold so the page
                // can be recycled (the tee write is best-effort:
                // data is still in the pool, only the on-disk cache
                // fill is skipped).  `write_page` errors are also
                // treated as best-effort for v1 (see
                // designs/bufferpool.md TODO(partial-failure)).
                if need_tee {
                    let mut tee_guard = TeeGuard {
                        slot: &slot,
                        fetch: &fetch,
                        inner: &inner,
                        key,
                        page_no,
                        completed: false,
                    };
                    let _ = inner.blockstore.write_page(key, stripe_off, dst).await;
                    slot.tee_pending.set(false);
                    tee_guard.completed = true;
                }

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
    completed: bool,
}

impl<'a> Drop for LeaderGuard<'a> {
    fn drop(&mut self) {
        if self.completed {
            return;
        }
        // Leader future was dropped before reaching Ready (during
        // read_page or bulk_get). Reset to Idle so the next
        // subscriber takes over leadership; preserve `page_idx` for
        // reuse. v1 drain relaxation: see mod.rs.
        let wakers = take_loading_wakers(self.slot);
        *self.slot.state.borrow_mut() = SlotState::Idle;
        for w in wakers {
            w.wake();
        }
    }
}

/// RAII guard for the tee phase. If the leader's future is dropped
/// while `BlockStore::write_page` is in flight, releases the
/// leader's pre-emptive consumer hold and clears `tee_pending` so
/// the page can be recycled. The bytes are already in the pool and
/// any non-leader subscribers that observed Ready keep their copy;
/// the on-disk cache fill is simply skipped.
struct TeeGuard<'a, T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    slot: &'a Rc<PageSlot>,
    fetch: &'a Rc<RefCell<StripeFetch>>,
    inner: &'a Rc<PoolInner<T, S, R>>,
    key: StripeKey,
    page_no: u64,
    completed: bool,
}

impl<'a, T, S, R> Drop for TeeGuard<'a, T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    fn drop(&mut self) {
        if self.completed {
            return;
        }
        // Drop the leader's pre-emptive consumer hold (it never got
        // wrapped in a PageGuard) and clear `tee_pending`. Then run
        // the same recycle path that `release_guard` uses so the
        // page returns to the free list and the slot/inflight entry
        // are cleaned up if appropriate.
        let prev = self.slot.consumer_holds.get();
        self.slot.consumer_holds.set(prev.saturating_sub(1));
        self.slot.tee_pending.set(false);
        recycle_if_terminal(self.inner, self.key, self.fetch, self.slot, self.page_no);
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

struct ParkOnSlot<'a> {
    slot: &'a Rc<PageSlot>,
}

impl<'a> Future for ParkOnSlot<'a> {
    type Output = ();
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        let mut st = self.slot.state.borrow_mut();
        match &mut *st {
            SlotState::Loading(wakers) => {
                wakers.push(cx.waker().clone());
                Poll::Pending
            }
            _ => Poll::Ready(()),
        }
    }
}
