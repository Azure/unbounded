// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Single-shard free page allocator. Parks awaiting wakers when the
//! backing is exhausted and wakes the oldest one on each
//! [`FreeList::release`]. The pool runs single-threaded inside its
//! NUMA shard so the underlying [`RefCell`] is sufficient; no atomics
//! are required.

use std::cell::RefCell;
use std::collections::BTreeMap;
use std::future::Future;
use std::pin::Pin;
use std::task::{Context, Poll, Waker};

pub(super) struct FreeList {
    inner: RefCell<Inner>,
}

struct Inner {
    free: Vec<u32>,
    // Parked wakers keyed by waiter id. `next_waiter_id` is
    // monotonic, so ascending key order is arrival (FIFO) order and
    // the head waiter is always the smallest key. Keying this way
    // keeps lookup, insert, and remove at O(log n) and avoids the
    // linear scans a queue would force on every poll and drop.
    waiters: BTreeMap<u64, Waker>,
    next_waiter_id: u64,
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
                waiters: BTreeMap::new(),
                next_waiter_id: 0,
            }),
        }
    }

    /// Try to grab a free page without parking.
    #[allow(dead_code)]
    pub fn try_alloc(&self) -> Option<u32> {
        let mut g = self.inner.borrow_mut();
        if g.waiters.is_empty() {
            g.free.pop()
        } else {
            None
        }
    }

    /// Park until a free page is available, then return it.
    pub fn alloc(&self) -> AllocFuture<'_> {
        AllocFuture {
            list: self,
            waiter_id: None,
        }
    }

    /// Return `page_idx` to the pool and wake the oldest waiter.
    pub fn release(&self, page_idx: u32) {
        let waker = {
            let mut g = self.inner.borrow_mut();
            g.free.push(page_idx);
            g.waiters.values().next().cloned()
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

pub(super) struct AllocFuture<'a> {
    list: &'a FreeList,
    waiter_id: Option<u64>,
}

impl<'a> Future for AllocFuture<'a> {
    type Output = u32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<u32> {
        let this = self.get_mut();
        let mut g = this.list.inner.borrow_mut();

        if let Some(id) = this.waiter_id {
            if g.waiters.contains_key(&id) {
                // Refresh the stored waker (cancel-safe across polls).
                g.waiters.insert(id, cx.waker().clone());
                // Only the head (smallest id) may consume a page; this
                // preserves strict FIFO service order.
                if g.waiters.keys().next() != Some(&id) {
                    return Poll::Pending;
                }
                if let Some(p) = g.free.pop() {
                    g.waiters.remove(&id);
                    let next_waker = if g.free.is_empty() {
                        None
                    } else {
                        g.waiters.values().next().cloned()
                    };
                    this.waiter_id = None;
                    drop(g);
                    if let Some(w) = next_waker {
                        w.wake();
                    }
                    return Poll::Ready(p);
                }
                return Poll::Pending;
            }
            // Our entry is gone (currently unreachable, but a stray
            // wake must never orphan the future). Re-park from scratch
            // below so a Pending return always leaves a live waker.
            this.waiter_id = None;
        }

        if g.waiters.is_empty() {
            if let Some(p) = g.free.pop() {
                return Poll::Ready(p);
            }
        }

        let id = g.next_waiter_id;
        g.next_waiter_id = g.next_waiter_id.wrapping_add(1);
        g.waiters.insert(id, cx.waker().clone());
        this.waiter_id = Some(id);
        Poll::Pending
    }
}

impl Drop for AllocFuture<'_> {
    fn drop(&mut self) {
        let Some(id) = self.waiter_id.take() else {
            return;
        };
        let waker = {
            let mut g = self.list.inner.borrow_mut();
            // Hand the baton to the next waiter only if we were the
            // head and a page is sitting free, matching the wake path
            // in `poll`.
            let was_head = g.waiters.keys().next() == Some(&id);
            if g.waiters.remove(&id).is_none() {
                return;
            }
            if was_head && !g.free.is_empty() {
                g.waiters.values().next().cloned()
            } else {
                None
            }
        };
        if let Some(w) = waker {
            w.wake();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::FreeList;
    use std::future::Future;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        unsafe { Waker::from_raw(raw()) }
    }

    #[test]
    fn fresh_alloc_does_not_overtake_waiter() {
        let list = FreeList::new(1);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        let mut first = Box::pin(list.alloc());
        let Poll::Ready(page) = first.as_mut().poll(&mut cx) else {
            panic!("first allocation should be ready");
        };
        drop(first);

        let mut waiter = Box::pin(list.alloc());
        assert!(matches!(waiter.as_mut().poll(&mut cx), Poll::Pending));
        list.release(page);

        let mut fresh = Box::pin(list.alloc());
        assert!(matches!(fresh.as_mut().poll(&mut cx), Poll::Pending));
        assert!(matches!(waiter.as_mut().poll(&mut cx), Poll::Ready(0)));
        assert!(matches!(fresh.as_mut().poll(&mut cx), Poll::Pending));

        list.release(0);
        assert!(matches!(fresh.as_mut().poll(&mut cx), Poll::Ready(0)));
    }

    #[test]
    fn dropping_head_waiter_unblocks_next_waiter() {
        let list = FreeList::new(1);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        let mut first = Box::pin(list.alloc());
        let Poll::Ready(page) = first.as_mut().poll(&mut cx) else {
            panic!("first allocation should be ready");
        };
        drop(first);

        let mut waiter1 = Box::pin(list.alloc());
        assert!(matches!(waiter1.as_mut().poll(&mut cx), Poll::Pending));
        let mut waiter2 = Box::pin(list.alloc());
        assert!(matches!(waiter2.as_mut().poll(&mut cx), Poll::Pending));

        list.release(page);
        drop(waiter1);

        assert!(matches!(waiter2.as_mut().poll(&mut cx), Poll::Ready(0)));
    }

    #[test]
    fn poll_after_entry_removed_reparks_with_live_waker() {
        let list = FreeList::new(1);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        let mut first = Box::pin(list.alloc());
        let Poll::Ready(page) = first.as_mut().poll(&mut cx) else {
            panic!("first allocation should be ready");
        };
        drop(first);

        let mut waiter = Box::pin(list.alloc());
        assert!(matches!(waiter.as_mut().poll(&mut cx), Poll::Pending));

        // Simulate the entry vanishing out from under the future. A
        // re-poll must re-park rather than orphan itself, leaving a
        // live waker registered.
        list.inner.borrow_mut().waiters.clear();
        assert!(matches!(waiter.as_mut().poll(&mut cx), Poll::Pending));
        assert_eq!(list.inner.borrow().waiters.len(), 1);

        list.release(page);
        assert!(matches!(waiter.as_mut().poll(&mut cx), Poll::Ready(0)));
    }
}
