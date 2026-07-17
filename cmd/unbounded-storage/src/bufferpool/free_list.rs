// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Single-shard free page allocator with FIFO hand-off.
//!
//! When the backing is exhausted, blocking [`FreeList::alloc`] callers
//! park in FIFO order. A released page is *handed off* directly to the
//! oldest waiter rather than being returned to
//! the shared `free` pool. This is load-bearing: speculative prefetch
//! uses [`FreeList::try_alloc_spare`], which must never steal a page a
//! blocked head fetch is owed. If `release` instead pushed the page to
//! `free` and merely woke the waiter, a speculative `try_alloc_spare`
//! running before the woken head re-polled (the waiter queue is empty
//! in that window) could pop the page out from under it and deadlock
//! under free-list pressure. Direct hand-off closes that race: a
//! promised page lives in the waiter's state, never in `free`.
//!
//! The pool runs single-threaded inside its NUMA shard so the
//! underlying [`RefCell`] is sufficient; no atomics are required.

use std::cell::RefCell;
use std::collections::{HashMap, VecDeque};
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

pub(super) struct FreeList {
    inner: RefCell<Inner>,
}

struct Inner {
    free: Vec<u32>,
    /// FIFO queue of blocked [`AllocFuture`]s.
    waiters: VecDeque<Rc<Waiter>>,
    /// Pages withheld from reuse because a fixed-buffer RECV writing
    /// into them was cancelled while still in flight. A quarantined
    /// page returns to circulation only once BOTH the kernel has
    /// finished with it (`kernel_done`, set when its RECV CQE is reaped)
    /// AND the owner has handed it back (`owner_released`, set by the
    /// normal [`FreeList::release`]). Until then it appears neither in
    /// `free` nor a waiter grant, so no allocation can hand the page out
    /// while the kernel may still write into it. This makes cancelling
    /// a fixed RECV sound without blocking the dropping task.
    quarantined: HashMap<u32, QState>,
}

#[derive(Default)]
struct QState {
    kernel_done: bool,
    owner_released: bool,
}

struct Waiter {
    state: RefCell<WaiterState>,
}

enum WaiterState {
    Queued(Waker),
    Granted(u32),
    Done,
}

impl Inner {
    /// Give `page` to the oldest waiter (recording it in the waiter and
    /// returning its waker to wake), or push it onto `free` if there
    /// are no waiters. The caller must wake the returned waker after
    /// dropping the borrow.
    fn hand_off(&mut self, page: u32) -> Option<Waker> {
        if let Some(w) = self.waiters.pop_front() {
            let queued = std::mem::replace(&mut *w.state.borrow_mut(), WaiterState::Granted(page));
            match queued {
                WaiterState::Queued(waker) => Some(waker),
                WaiterState::Granted(_) | WaiterState::Done => {
                    unreachable!("only queued waiters may remain in the FIFO")
                }
            }
        } else {
            self.free.push(page);
            crate::metrics::bufferpool_free_delta(1);
            None
        }
    }
}

impl FreeList {
    pub fn new(page_count: u32) -> Self {
        let mut free = Vec::with_capacity(page_count as usize);
        // Reverse so popping gives ascending page indices, which
        // makes test assertions deterministic.
        for i in (0..page_count).rev() {
            free.push(i);
        }
        Self {
            inner: RefCell::new(Inner {
                free,
                waiters: VecDeque::new(),
                quarantined: HashMap::new(),
            }),
        }
    }

    /// Non-blocking allocation for a *head* fetch that must never park
    /// on the free list.
    ///
    /// This is the admission primitive for cross-shard (remote) reads:
    /// a coordinator on another shard pins whole stripes in this pool
    /// across a network round trip, so a remote head that parked on
    /// [`FreeList::alloc`] could sit blocked while the very coordinator
    /// owed the page is itself stalled behind another owner's pins,
    /// forming a cross-shard hold-and-wait cycle. Returning `None`
    /// instead (surfaced to the caller as a transient `Error::Busy`)
    /// keeps remote admission fail-fast: the coordinator retries later
    /// rather than holding pins while blocked.
    ///
    /// Unlike [`FreeList::try_alloc_spare`] it keeps no reserve (a head
    /// is the real consumer, not speculation, so it may legitimately
    /// take the last free page). It still yields to parked waiters: if
    /// any blocking head is queued on [`FreeList::alloc`] it returns
    /// `None` rather than jumping ahead, preserving FIFO fairness and
    /// the local-head priority the deadlock-freedom argument relies on.
    /// Because a remote head never parks, it always completes (with a
    /// page or `Busy`) in bounded time and releases its pins, so the
    /// cross-shard wait graph stays acyclic.
    pub fn try_alloc_head(&self) -> Option<u32> {
        let mut g = self.inner.borrow_mut();
        if g.waiters.is_empty() {
            let page = g.free.pop();
            if page.is_some() {
                crate::metrics::bufferpool_free_delta(-1);
            }
            page
        } else {
            None
        }
    }

    pub fn has_waiters(&self) -> bool {
        !self.inner.borrow().waiters.is_empty()
    }

    /// Non-blocking allocation for speculative (prefetch) use that
    /// yields only a *spare* page.
    ///
    /// Two guards keep speculation from ever deadlocking a head fetch:
    ///
    /// 1. It fails if any waiter is parked on [`FreeList::alloc`].
    ///    Parked waiters are head fetches with strict priority on
    ///    scarce pages. Pages already promised to a waiter live in
    ///    its state, not `free`, so they are never visible here.
    ///
    /// 2. It fails unless strictly more than `reserve` pages remain
    ///    free *after* the pop, i.e. it keeps `reserve` pages in
    ///    reserve for head fetches. The caller passes the current
    ///    active-stream count: every active stream needs at most one
    ///    backing page for its in-order head, so keeping `reserve`
    ///    pages free guarantees no head ever has to park on
    ///    [`FreeList::alloc`]. Since a deadlock cycle requires a head
    ///    blocked on `alloc` while speculation pins the pages it
    ///    needs, denying speculation that last reserve provably
    ///    prevents the cycle. Under genuine page scarcity (few pages,
    ///    many streams) this disables prefetch entirely and the reader
    ///    degrades to head-only fetching; when pages are plentiful
    ///    relative to streams (the single-large-object case we want to
    ///    accelerate, `reserve == 1`) speculation runs at full depth.
    pub fn try_alloc_spare(&self, reserve: usize) -> Option<u32> {
        let mut g = self.inner.borrow_mut();
        if g.waiters.is_empty() && g.free.len() > reserve {
            let page = g.free.pop();
            if page.is_some() {
                crate::metrics::bufferpool_free_delta(-1);
            }
            page
        } else {
            None
        }
    }

    /// Park until a free page is available, then return it.
    pub fn alloc(&self) -> AllocFuture<'_> {
        AllocFuture {
            list: self,
            waiter: None,
        }
    }

    /// Return `page_idx` to the pool, handing it directly to the
    /// oldest waiter if one is parked.
    ///
    /// If the page is in recv-quarantine (its destination was withheld
    /// by [`FreeList::quarantine_recv`] because an in-flight RECV into
    /// it was cancelled), this only marks the owner side done. The page
    /// is handed back to circulation here only if the kernel has also
    /// already finished with it; otherwise it stays withheld until
    /// [`FreeList::reclaim_recv`] observes the RECV CQE.
    pub fn release(&self, page_idx: u32) {
        let waker = {
            let mut g = self.inner.borrow_mut();
            // `Some(kernel_done)` if quarantined, `None` if not.
            let quarantined = match g.quarantined.get_mut(&page_idx) {
                Some(st) => {
                    st.owner_released = true;
                    Some(st.kernel_done)
                }
                None => None,
            };
            match quarantined {
                None => g.hand_off(page_idx),
                Some(false) => None,
                Some(true) => {
                    g.quarantined.remove(&page_idx);
                    g.hand_off(page_idx)
                }
            }
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    /// Withhold `page_idx` from reuse until its in-flight fixed-buffer
    /// RECV CQE is reaped. Called when such a RECV is cancelled on early
    /// drop while still owning `page_idx`: the kernel may still complete
    /// the RECV into the page after the dropping task returns, so the
    /// page must not be reused yet. Pairs with [`FreeList::reclaim_recv`]
    /// (kernel side) and the owner's [`FreeList::release`]; the page
    /// returns to circulation only once both have fired.
    pub fn quarantine_recv(&self, page_idx: u32) {
        let mut g = self.inner.borrow_mut();
        let st = g.quarantined.entry(page_idx).or_default();
        debug_assert!(
            !st.kernel_done && !st.owner_released,
            "page {page_idx} quarantined while already in quarantine",
        );
    }

    /// Mark the kernel finished with a quarantined `page_idx` (its RECV
    /// CQE has been reaped). If the owner has already released the page
    /// it returns to circulation now (handed to the oldest waiter if one
    /// is parked); otherwise it waits for the owner's
    /// [`FreeList::release`]. A no-op if the page is not quarantined.
    pub fn reclaim_recv(&self, page_idx: u32) {
        let waker = {
            let mut g = self.inner.borrow_mut();
            // `Some(owner_released)` if quarantined, `None` if not.
            let owner_released = match g.quarantined.get_mut(&page_idx) {
                Some(st) => {
                    st.kernel_done = true;
                    Some(st.owner_released)
                }
                None => None,
            };
            match owner_released {
                Some(true) => {
                    g.quarantined.remove(&page_idx);
                    g.hand_off(page_idx)
                }
                _ => None,
            }
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    #[allow(dead_code)]
    pub fn available(&self) -> usize {
        self.inner.borrow().free.len()
    }
}

/// Public, non-generic handle to a shard pool's free list, exposing
/// only the recv-quarantine operations keyed by byte offset into the
/// registered backing.
///
/// It is handed to the ring layer (via a backend adapter implementing
/// `ring::RecvQuarantine`) so a cancelled fixed-buffer RECV can withhold
/// its destination page until the kernel is done with it, without the
/// ring needing to know the pool's generic parameters or page geometry.
#[derive(Clone)]
pub struct RecvQuarantineHandle {
    free: Rc<FreeList>,
    page_size: usize,
}

impl RecvQuarantineHandle {
    pub(super) fn new(free: Rc<FreeList>, page_size: usize) -> Self {
        Self { free, page_size }
    }

    /// Withhold the page containing `page_byte_offset` from reuse until
    /// its RECV CQE is reaped. See [`FreeList::quarantine_recv`].
    pub fn quarantine(&self, page_byte_offset: usize) {
        self.free
            .quarantine_recv((page_byte_offset / self.page_size) as u32);
    }

    /// Mark the kernel finished with the page containing
    /// `page_byte_offset`. See [`FreeList::reclaim_recv`].
    pub fn reclaim(&self, page_byte_offset: usize) {
        self.free
            .reclaim_recv((page_byte_offset / self.page_size) as u32);
    }
}

pub(super) struct AllocFuture<'a> {
    list: &'a FreeList,
    /// Lazily allocated only when this future has to park.
    waiter: Option<Rc<Waiter>>,
}

impl<'a> Future for AllocFuture<'a> {
    type Output = u32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<u32> {
        let this = self.get_mut();
        if let Some(waiter) = &this.waiter {
            let mut state = waiter.state.borrow_mut();
            match &mut *state {
                WaiterState::Granted(page) => {
                    let page = *page;
                    *state = WaiterState::Done;
                    drop(state);
                    this.waiter = None;
                    return Poll::Ready(page);
                }
                WaiterState::Queued(waker) => {
                    let old = std::mem::replace(waker, cx.waker().clone());
                    drop(state);
                    drop(old);
                    return Poll::Pending;
                }
                WaiterState::Done => panic!("allocation future polled after completion"),
            }
        }

        let mut g = this.list.inner.borrow_mut();
        // Only jump the queue for a free page if nobody is waiting
        // ahead of us; otherwise we would starve parked heads.
        if g.waiters.is_empty() {
            if let Some(p) = g.free.pop() {
                crate::metrics::bufferpool_free_delta(-1);
                return Poll::Ready(p);
            }
        }

        let waiter = Rc::new(Waiter {
            state: RefCell::new(WaiterState::Queued(cx.waker().clone())),
        });
        g.waiters.push_back(waiter.clone());
        this.waiter = Some(waiter);
        Poll::Pending
    }
}

impl<'a> Drop for AllocFuture<'a> {
    fn drop(&mut self) {
        let Some(waiter) = self.waiter.take() else {
            return;
        };
        let state = std::mem::replace(&mut *waiter.state.borrow_mut(), WaiterState::Done);
        let (waker, discarded) = match state {
            WaiterState::Queued(waker) => {
                let mut g = self.list.inner.borrow_mut();
                let pos = g
                    .waiters
                    .iter()
                    .position(|queued| Rc::ptr_eq(queued, &waiter))
                    .expect("queued allocation waiter must be in the FIFO");
                g.waiters.remove(pos);
                (None, Some(waker))
            }
            WaiterState::Granted(page) => {
                let waker = self.list.inner.borrow_mut().hand_off(page);
                (waker, None)
            }
            WaiterState::Done => (None, None),
        };
        drop(discarded);
        if let Some(w) = waker {
            w.wake();
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::{Wake, Waker};

    use super::*;

    struct CountWake(AtomicUsize);

    impl Wake for CountWake {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::Relaxed);
        }
    }

    fn waker(counter: &Arc<CountWake>) -> Waker {
        Waker::from(counter.clone())
    }

    fn poll(future: &mut AllocFuture<'_>, waker: &Waker) -> Poll<u32> {
        let mut cx = Context::from_waker(waker);
        Pin::new(future).poll(&mut cx)
    }

    #[test]
    fn handoff_is_fifo_and_hidden_from_nonblocking_allocations() {
        let free = FreeList::new(1);
        assert_eq!(free.try_alloc_head(), Some(0));
        let count = Arc::new(CountWake(AtomicUsize::new(0)));
        let wake = waker(&count);
        let mut first = free.alloc();
        let mut second = free.alloc();
        assert!(poll(&mut first, &wake).is_pending());
        assert!(poll(&mut second, &wake).is_pending());

        free.release(0);
        assert_eq!(count.0.load(Ordering::Relaxed), 1);
        assert_eq!(free.available(), 0);
        assert_eq!(free.try_alloc_head(), None);
        assert_eq!(free.try_alloc_spare(0), None);
        assert_eq!(poll(&mut first, &wake), Poll::Ready(0));

        drop(first);
        free.release(0);
        assert_eq!(poll(&mut second, &wake), Poll::Ready(0));
    }

    #[test]
    fn dropping_queued_waiter_preserves_survivor_order() {
        let free = FreeList::new(1);
        let page = free.try_alloc_head().unwrap();
        let count = Arc::new(CountWake(AtomicUsize::new(0)));
        let wake = waker(&count);
        let mut first = free.alloc();
        let mut cancelled = free.alloc();
        let mut last = free.alloc();
        assert!(poll(&mut first, &wake).is_pending());
        assert!(poll(&mut cancelled, &wake).is_pending());
        assert!(poll(&mut last, &wake).is_pending());
        drop(cancelled);

        free.release(page);
        assert_eq!(poll(&mut first, &wake), Poll::Ready(page));
        drop(first);
        free.release(page);
        assert_eq!(poll(&mut last, &wake), Poll::Ready(page));
    }

    #[test]
    fn dropping_granted_waiter_forwards_its_page() {
        let free = FreeList::new(1);
        let page = free.try_alloc_head().unwrap();
        let count = Arc::new(CountWake(AtomicUsize::new(0)));
        let wake = waker(&count);
        let mut first = free.alloc();
        let mut second = free.alloc();
        assert!(poll(&mut first, &wake).is_pending());
        assert!(poll(&mut second, &wake).is_pending());
        free.release(page);

        drop(first);
        assert_eq!(poll(&mut second, &wake), Poll::Ready(page));
    }

    #[test]
    fn quarantine_hands_safe_page_to_oldest_waiter() {
        let free = FreeList::new(1);
        let page = free.try_alloc_head().unwrap();
        free.quarantine_recv(page);
        let count = Arc::new(CountWake(AtomicUsize::new(0)));
        let wake = waker(&count);
        let mut waiter = free.alloc();
        assert!(poll(&mut waiter, &wake).is_pending());

        free.release(page);
        assert!(poll(&mut waiter, &wake).is_pending());
        free.reclaim_recv(page);
        assert_eq!(free.available(), 0);
        assert_eq!(poll(&mut waiter, &wake), Poll::Ready(page));
    }

    #[test]
    fn repeated_poll_replaces_waker() {
        let free = FreeList::new(0);
        let old_count = Arc::new(CountWake(AtomicUsize::new(0)));
        let new_count = Arc::new(CountWake(AtomicUsize::new(0)));
        let mut waiter = free.alloc();
        assert!(poll(&mut waiter, &waker(&old_count)).is_pending());
        assert!(poll(&mut waiter, &waker(&new_count)).is_pending());

        free.release(7);
        assert_eq!(old_count.0.load(Ordering::Relaxed), 0);
        assert_eq!(new_count.0.load(Ordering::Relaxed), 1);
    }
}
