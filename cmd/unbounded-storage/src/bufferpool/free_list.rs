// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Single-shard free page allocator. Parks awaiting wakers when the
//! backing is exhausted and wakes the oldest one on each
//! [`FreeList::release`]. The pool runs single-threaded inside its
//! NUMA shard so the underlying [`RefCell`] is sufficient; no atomics
//! are required.

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
    waiters: VecDeque<Waker>,
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
            }),
        }
    }

    /// Try to grab a free page without parking.
    #[allow(dead_code)]
    pub fn try_alloc(&self) -> Option<u32> {
        self.inner.borrow_mut().free.pop()
    }

    /// Park until a free page is available, then return it.
    pub fn alloc(&self) -> AllocFuture<'_> {
        AllocFuture { list: self }
    }

    /// Return `page_idx` to the pool and wake the oldest waiter.
    pub fn release(&self, page_idx: u32) {
        let waker = {
            let mut g = self.inner.borrow_mut();
            g.free.push(page_idx);
            g.waiters.pop_front()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    #[cfg(test)]
    pub fn available(&self) -> usize {
        self.inner.borrow().free.len()
    }
}

pub(super) struct AllocFuture<'a> {
    list: &'a FreeList,
}

impl<'a> Future for AllocFuture<'a> {
    type Output = u32;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<u32> {
        let mut g = self.list.inner.borrow_mut();
        if let Some(p) = g.free.pop() {
            Poll::Ready(p)
        } else {
            g.waiters.push_back(cx.waker().clone());
            Poll::Pending
        }
    }
}
