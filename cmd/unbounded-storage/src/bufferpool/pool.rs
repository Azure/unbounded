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

    /// Inspection accessor for the free list size. Used by in-tree
    /// unit tests and the DST harness under `tests/`; not part of
    /// the production-facing API contract.
    pub fn free_pages(&self) -> usize {
        self.inner.free.available()
    }

    /// Inspection accessor for the inflight map size. Same audience
    /// as [`Pool::free_pages`].
    pub fn inflight_entries(&self) -> usize {
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
        // `PageGuard::drop` calls this. It is logically equivalent
        // to dropping a `ConsumerHold`: decrement `consumer_holds`
        // and recycle if the slot is terminal with no other
        // holders. We keep the function-call form here because
        // `PageGuard` is dyn-erased (`Rc<dyn StreamSrc>`) and does
        // not see the typed `ConsumerHold`.
        let slot = match self.fetch.borrow().pages.get(&page_no).cloned() {
            Some(s) => s,
            None => return,
        };
        let prev = slot.consumer_holds.get();
        slot.consumer_holds.set(prev.saturating_sub(1));
        recycle_if_terminal(&self.inner, self.key, &self.fetch, &slot, page_no);
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
// ConsumerHold: RAII for `PageSlot::consumer_holds`.
// ---------------------------------------------------------------------------

/// RAII bump of `slot.consumer_holds`. Construction increments;
/// drop decrements and runs `recycle_if_terminal`. This is the
/// single mechanism every participant in a slot's lifecycle (the
/// leader during I/O+tee, parked subscribers, fast-path Ready
/// callers) uses to express "I still care about this slot".
///
/// On the success path the hold is transferred to the caller of
/// [`fetch_page`] via [`ConsumerHold::forget`], which suppresses
/// the drop decrement. The transferred bump is then balanced by
/// [`StreamSrc::release_guard`] when the resulting `PageGuard`
/// drops.
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

    /// Transfer ownership of the bump out of this guard. The bump
    /// remains on the slot; the caller is responsible for
    /// balancing it (typically via the `PageGuard` returned to the
    /// stream consumer). Drop becomes a no-op.
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

// ---------------------------------------------------------------------------
// Cleanup helpers.
// ---------------------------------------------------------------------------

/// Returns `slot`'s page to the free list and removes the slot
/// (and possibly the entire `StripeFetch`) if it is in a terminal
/// state and no one is holding it. Invoked from `ConsumerHold::drop`
/// and from `StreamSrc::release_guard`; the cleanup invariant lives
/// in one place.
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
///
/// Lifetime of `consumer_holds`. Every party that is or will be a
/// consumer of the slot's bytes holds a [`ConsumerHold`] for the
/// duration of its interest:
///
/// * The leader takes a `ConsumerHold` on entering `Action::Lead`
///   and keeps it across I/O and the tee write. On error the hold
///   drops and (if no parked subscribers are still around) runs
///   `recycle_if_terminal`, returning the page to the free list
///   and unblocking [`FreeList::alloc`] waiters elsewhere. On
///   success the hold is `forget`'d (transferred to the caller's
///   `PageGuard`).
/// * Parked subscribers take a `ConsumerHold` on their first
///   [`ParkOnSlot::poll`] (so that the leader's `PageGuard` drop
///   cannot recycle the slot between the wake and the re-poll).
///   The hold is transferred forward on `ParkOutcome::Ready`,
///   dropped on `Error`/`Retry`/cancellation.
/// * Fast-path `Action::Ready` callers construct a transient
///   `ConsumerHold` and immediately `forget` it for the caller.
async fn fetch_page<T, S, R>(
    inner: Rc<PoolInner<T, S, R>>,
    fetch: Rc<RefCell<StripeFetch>>,
    key: StripeKey,
    req: &R,
    page_no: u64,
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
                // Take the caller's hold via `ConsumerHold` and
                // forget it; the returned `PageGuard` will balance
                // the bump via `StreamSrc::release_guard`.
                ConsumerHold::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no)
                    .forget();
                return Ok(pi);
            }
            Action::Error(e) => return Err(e),
            Action::Park => {
                // `ParkOnSlot` pre-bumps `consumer_holds` for this
                // subscriber on first registration via a
                // `ConsumerHold`. The hold is transferred to us on
                // `Ready`, dropped on `Error`/`Retry`/cancellation.
                //
                // This closes the recycle race that would otherwise
                // let the leader free the page between "subscriber
                // woken" and "subscriber re-polled" while we still
                // hold an `Rc<PageSlot>` whose `page_idx` aliases a
                // page that's already been reallocated.
                let park = ParkOnSlot::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no);
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
                // phase plus the tee. On any path out of this arm:
                //
                // * Error: `leader_guard.completed = true`, state
                //   set to `Error`, wakers woken, leader_hold drops
                //   and recycles if there are no other holders.
                // * Cancellation in I/O: `leader_guard` resets to
                //   `Idle` and wakes wakers; leader_hold then drops
                //   (state is `Idle`, not terminal, so no recycle).
                //   A parked subscriber takes over via `Retry`.
                // * Cancellation in tee: `tee_pending` is cleared
                //   by `TeePendingGuard`; leader_hold drops, state
                //   is `Ready` and terminal, so the page recycles
                //   if no other holders remain.
                // * Success: leader_hold is `forget`'d and the
                //   page_idx is returned.
                //
                // Drop order on cancel is (locals declared first
                // drop last): `leader_guard` flips state and wakes
                // first, then `leader_hold` drops to release the
                // bump and possibly recycle.
                let leader_hold =
                    ConsumerHold::new(inner.clone(), fetch.clone(), slot.clone(), key, page_no);
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
                        // leader_hold drops at the `return`. Its
                        // drop runs `recycle_if_terminal`: state is
                        // now `Error` (terminal). If parked
                        // subscribers are still holding their
                        // pre-bumped `ConsumerHold`s the count
                        // stays non-zero and the recycle is a
                        // no-op; the last subscriber to observe
                        // `Error` runs it. If no subscribers are
                        // parked the page returns to the free list
                        // immediately, waking any `FreeList::alloc`
                        // waiter.
                        return Err(e);
                    }
                };

                // Phase 2: mark Ready and wake parked subscribers
                // BEFORE running the tee, so non-leader subscribers
                // can consume bytes concurrently with the leader's
                // `write_page` (matches designs/bufferpool.md
                // "Pull-through with tee"). The page stays pinned
                // across the tee via `tee_pending`.
                //
                // Each parked subscriber has pre-bumped its own
                // `consumer_holds` via its `ConsumerHold`; the
                // leader's `ConsumerHold` covers itself.
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

                // Phase 3: leader drives the tee. The leader's
                // `ConsumerHold` keeps the page pinned across the
                // tee even if no other subscriber is around. If the
                // leader's future is dropped here, `TeePendingGuard`
                // clears `tee_pending` first, then `leader_hold`
                // drops and runs the same recycle path as
                // `release_guard`. `write_page` errors are treated
                // as best-effort for v1 (see designs/bufferpool.md
                // TODO(partial-failure)).
                if need_tee {
                    let _tee_pending_guard = TeePendingGuard { slot: &slot };
                    let _ = inner.blockstore.write_page(key, stripe_off, dst).await;
                    // `_tee_pending_guard` drops at end of block,
                    // clearing `tee_pending` before we forget the
                    // leader's hold so a subsequent `PageGuard`
                    // drop can recycle if appropriate.
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

/// RAII state-reset guard for the leader. If the leader's future
/// is dropped before it transitions the slot to `Ready` or
/// `Error`, this resets the slot to `Idle` and wakes parked
/// subscribers so the next one can take over leadership; the
/// `page_idx` is preserved for reuse. Consumer-hold accounting is
/// handled separately by [`ConsumerHold`].
struct LeaderGuard<'a> {
    slot: &'a Rc<PageSlot>,
    completed: bool,
}

impl<'a> Drop for LeaderGuard<'a> {
    fn drop(&mut self) {
        if self.completed {
            return;
        }
        let wakers = take_loading_wakers(self.slot);
        *self.slot.state.borrow_mut() = SlotState::Idle;
        for w in wakers {
            w.wake();
        }
    }
}

/// Clears `slot.tee_pending` on drop. Drop order in
/// [`fetch_page`]'s tee block ensures this runs before the
/// leader's `ConsumerHold` drops, so any subsequent recycle sees
/// `tee_pending == false`.
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
    /// `consumer_holds` via a `ConsumerHold`. The hold is taken
    /// out on a terminal outcome: transferred to the caller on
    /// `Ready` (via `ParkOutcome::Ready(hold)`), dropped on
    /// `Error`/`Retry`. If the future is cancelled before reaching
    /// a terminal outcome the `Option` drops with the hold inside,
    /// releasing the bump.
    hold: Option<ConsumerHold<T, S, R>>,
}

/// Outcome of awaiting [`ParkOnSlot`].
enum ParkOutcome<T, S, R>
where
    T: Transport<R>,
    S: BlockStore,
    R: Req,
{
    /// Slot reached `Ready`. The pre-bumped `ConsumerHold` is
    /// handed back to the caller, which must `forget()` it so the
    /// bump survives to be balanced by the consumer's `PageGuard`.
    Ready(ConsumerHold<T, S, R>),
    Error(Error),
    /// Slot left `Loading` via leader-future drop (back to `Idle`
    /// or re-entering `Loading`). The pre-bumped hold has already
    /// been released; the caller re-enters the state machine.
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

        // First poll: register our waker and pre-bump
        // `consumer_holds` via a `ConsumerHold`.
        //
        // `Action::Park` constructs a `ParkOnSlot` after
        // synchronously observing `Loading`. Between that
        // observation and this poll, the executor may have run
        // other tasks (in particular the leader can complete the
        // I/O), so the state can already have moved past `Loading`
        // by the time we get here. We still bump the hold
        // unconditionally so the caller's "outcome path always
        // consumes the transferred hold" accounting holds, and
        // fall through to the match below to deliver whatever
        // outcome the state currently dictates.
        if this.hold.is_none() {
            {
                let mut st = this.slot.state.borrow_mut();
                if let SlotState::Loading(wakers) = &mut *st {
                    wakers.push(cx.waker().clone());
                }
            }
            this.hold = Some(ConsumerHold::new(
                this.inner.clone(),
                this.fetch.clone(),
                this.slot.clone(),
                this.key,
                this.page_no,
            ));
        }

        // Subsequent polls: state may or may not have transitioned.
        // We've already registered our waker once; spinning drivers
        // (e.g. the in-tree `block_on_two` with a noop waker) may
        // re-poll us while still `Loading`. Don't re-push: the
        // existing waker entry will be drained by the leader. This
        // bounds the wakers vec at one entry per parked subscriber.
        let outcome = {
            let st = this.slot.state.borrow();
            match &*st {
                SlotState::Loading(_) => return Poll::Pending,
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
                // Drop the hold to release the pre-bump and run
                // recycle_if_terminal; state is terminal so if we
                // are the last holder the slot is reclaimed here.
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
/// the `slot.state` borrow scope minimal (we observe the state,
/// then drop the borrow before mutating `self.hold`).
enum Outcome {
    Ready,
    Error(Error),
    Retry,
}
