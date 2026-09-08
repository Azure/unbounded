// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Single-shard free page allocator with FIFO hand-off.
//!
//! When the backing is exhausted, blocking [`FreeList::alloc`] callers
//! park in FIFO order. A released page is *handed off* directly to the
//! oldest waiter (recorded in `granted`) rather than being returned to
//! the shared `free` pool. This is load-bearing: speculative prefetch
//! uses [`FreeList::try_alloc_spare`], which must never steal a page a
//! blocked head fetch is owed. If `release` instead pushed the page to
//! `free` and merely woke the waiter, a speculative `try_alloc_spare`
//! running before the woken head re-polled (the waiter queue is empty
//! in that window) could pop the page out from under it and deadlock
//! under free-list pressure. Direct hand-off closes that race: a
//! promised page lives in `granted`, never in `free`.
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
    /// FIFO queue of blocked [`AllocFuture`]s, identified by id.
    waiters: VecDeque<Waiter>,
    /// Pages handed off to a specific waiter, awaiting its next poll.
    /// Reserved here so speculation cannot reclaim them.
    granted: Vec<(u64, u32)>,
    /// Pages withheld from reuse because a fixed-buffer RECV writing
    /// into them was canceled while still in flight. A quarantined
    /// page returns to circulation only once BOTH the kernel has
    /// finished with it (`kernel_done`, set when its RECV CQE is reaped)
    /// AND the owner has handed it back (`owner_released`, set by the
    /// normal [`FreeList::release`]). Until then it appears neither in
    /// `free` nor `granted`, so no allocation can hand the page out
    /// while the kernel may still write into it. This makes canceling
    /// a fixed RECV sound without blocking the dropping task.
    quarantined: HashMap<u32, QState>,
    next_id: u64,
}

#[derive(Default)]
struct QState {
    kernel_done: bool,
    owner_released: bool,
}

struct Waiter {
    id: u64,
    waker: Waker,
}

impl Inner {
    /// Give `page` to the oldest waiter (recording it in `granted` and
    /// returning its waker to wake), or push it onto `free` if there
    /// are no waiters. The caller must wake the returned waker after
    /// dropping the borrow.
    fn hand_off(&mut self, page: u32) -> Option<Waker> {
        if let Some(w) = self.waiters.pop_front() {
            self.granted.push((w.id, page));
            Some(w.waker)
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
                granted: Vec::new(),
                quarantined: HashMap::new(),
                next_id: 0,
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
    ///    scarce pages. (Pages already promised to a waiter live in
    ///    `granted`, not `free`, so they are never visible here.)
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
            id: None,
        }
    }

    /// Return `page_idx` to the pool, handing it directly to the
    /// oldest waiter if one is parked.
    ///
    /// If the page is in recv-quarantine (its destination was withheld
    /// by [`FreeList::quarantine_recv`] because an in-flight RECV into
    /// it was canceled), this only marks the owner side done. The page
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
    /// RECV CQE is reaped. Called when such a RECV is canceled on early
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
/// `ring::RecvQuarantine`) so a canceled fixed-buffer RECV can withhold
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
    /// Lazily assigned when this future first has to park, so it can
    /// receive a hand-off and clean up on drop.
    id: Option<u64>,
}

impl<'a> Future for AllocFuture<'a> {
    type Output = u32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<u32> {
        let this = self.get_mut();
        let mut g = this.list.inner.borrow_mut();

        // A page handed to us by `release`/drop-reclaim takes priority.
        if let Some(id) = this.id {
            if let Some(pos) = g.granted.iter().position(|(wid, _)| *wid == id) {
                let (_, page) = g.granted.remove(pos);
                return Poll::Ready(page);
            }
        }

        // Only jump the queue for a free page if nobody is waiting
        // ahead of us; otherwise we would starve parked heads.
        if this.id.is_none() && g.waiters.is_empty() {
            if let Some(p) = g.free.pop() {
                crate::metrics::bufferpool_free_delta(-1);
                return Poll::Ready(p);
            }
        }

        // Park (or refresh our parked waker).
        let id = match this.id {
            Some(id) => id,
            None => {
                let id = g.next_id;
                g.next_id += 1;
                this.id = Some(id);
                id
            }
        };
        if let Some(w) = g.waiters.iter_mut().find(|w| w.id == id) {
            w.waker = cx.waker().clone();
        } else {
            g.waiters.push_back(Waiter {
                id,
                waker: cx.waker().clone(),
            });
        }
        Poll::Pending
    }
}

impl<'a> Drop for AllocFuture<'a> {
    fn drop(&mut self) {
        let Some(id) = self.id else {
            return;
        };
        let waker = {
            let mut g = self.list.inner.borrow_mut();
            g.waiters.retain(|w| w.id != id);
            // If we were handed a page but never polled to take it,
            // re-offer it so it is not leaked.
            if let Some(pos) = g.granted.iter().position(|(wid, _)| *wid == id) {
                let (_, page) = g.granted.remove(pos);
                g.hand_off(page)
            } else {
                None
            }
        };
        if let Some(w) = waker {
            w.wake();
        }
    }
}
