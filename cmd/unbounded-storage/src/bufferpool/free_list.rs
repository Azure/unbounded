// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

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
use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
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
    next_id: u64,
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
                next_id: 0,
            }),
        }
    }

    /// Try to grab a free page without parking.
    #[allow(dead_code)]
    pub fn try_alloc(&self) -> Option<u32> {
        self.inner.borrow_mut().free.pop()
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
            g.free.pop()
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
    pub fn release(&self, page_idx: u32) {
        let waker = self.inner.borrow_mut().hand_off(page_idx);
        if let Some(w) = waker {
            w.wake();
        }
    }

    #[allow(dead_code)]
    pub fn available(&self) -> usize {
        self.inner.borrow().free.len()
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
