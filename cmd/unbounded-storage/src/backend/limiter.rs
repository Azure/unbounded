// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Cooperative concurrency limiter for origin fetches.
//!
//! Each origin backend bounds how many fetches it may have in flight at
//! once to the configured `http_concurrency`. The fabric and frontend
//! tiers run a single cooperative task per shard, so the limiter never
//! blocks a thread: it parks the fetch future and wakes it when a
//! permit frees up.
//!
//! The limiter is shard-local. Its handles may outlive a backend registry
//! entry through in-flight fetch futures, but they never cross threads.

use std::cell::RefCell;
use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};

/// A clonable shard-local handle to an origin fetch permit pool.
///
/// `limit` permits are available at construction. `acquire().await`
/// yields a [`FetchPermit`] once a permit is free; dropping the permit
/// returns it to the pool and wakes the next waiter. A `limit` of zero
/// is treated as one so a backend always makes progress.
#[derive(Clone)]
pub struct FetchLimiter {
    inner: Rc<RefCell<LimiterInner>>,
}

struct LimiterInner {
    available: usize,
    /// FIFO queue of parked acquirers. Each entry is `(id, waker)` where
    /// `id` uniquely identifies one `Acquire` future. Keying on a unique
    /// id (rather than on the `Waker` value) lets a cancelled acquirer
    /// remove exactly its own entry on drop, and lets a re-poll refresh
    /// its own waker in place, without disturbing other futures that may
    /// share the same `Waker` (e.g. several fetches driven by one task).
    waiters: VecDeque<(u64, Waker)>,
    /// Monotonic source of waiter ids.
    next_id: u64,
}

impl FetchLimiter {
    pub fn new(limit: usize) -> Self {
        Self {
            inner: Rc::new(RefCell::new(LimiterInner {
                available: limit.max(1),
                waiters: VecDeque::new(),
                next_id: 0,
            })),
        }
    }

    /// Acquire a permit, parking until one is free.
    pub fn acquire(&self) -> Acquire {
        Acquire {
            inner: Rc::clone(&self.inner),
            id: None,
        }
    }

    /// Permits currently free. Exposed for tests.
    #[cfg(test)]
    pub fn available(&self) -> usize {
        self.inner.borrow().available
    }
}

/// Future returned by [`FetchLimiter::acquire`]. Owns an `Rc` clone of
/// the pool so it can live inside a `'static` fetch future.
///
/// While parked it holds a uniquely-identified slot in the pool's waiter
/// queue. The slot is removed when the future resolves *or* is dropped
/// (cancelled), so a cancelled acquirer can never leave an orphaned
/// waker behind to swallow a later wake-up meant for a live waiter.
pub struct Acquire {
    inner: Rc<RefCell<LimiterInner>>,
    /// Id of this future's slot in `waiters`, assigned the first time it
    /// parks. `None` until then (and irrelevant once resolved).
    id: Option<u64>,
}

impl Future for Acquire {
    type Output = FetchPermit;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // `Acquire` is `Unpin` (it holds only an `Rc` and an `Option`),
        // so taking a `&mut Self` out of the pin is sound.
        let this = self.get_mut();
        let mut inner = this.inner.borrow_mut();
        if inner.available > 0 {
            inner.available -= 1;
            // We are about to resolve: drop any slot we registered so a
            // later release does not waste a wake on us.
            if let Some(id) = this.id.take() {
                inner.waiters.retain(|(i, _)| *i != id);
            }
            drop(inner);
            return Poll::Ready(FetchPermit {
                inner: Rc::clone(&this.inner),
            });
        }
        // No permit free: register (or refresh) our waker under our own
        // id and park. Refreshing in place (rather than appending) keeps
        // at most one slot per `Acquire` even across re-polls.
        let id = match this.id {
            Some(id) => id,
            None => {
                let id = inner.next_id;
                inner.next_id += 1;
                this.id = Some(id);
                id
            }
        };
        if let Some(slot) = inner.waiters.iter_mut().find(|(i, _)| *i == id) {
            slot.1 = cx.waker().clone();
        } else {
            inner.waiters.push_back((id, cx.waker().clone()));
        }
        Poll::Pending
    }
}

impl Drop for Acquire {
    /// Remove our parked slot, if any, so a cancelled acquire cannot
    /// leave an orphaned waker that a subsequent permit release would
    /// pop and wake to no effect, stranding a live waiter behind it.
    ///
    /// A permit release ([`FetchPermit::drop`]) frees a permit *and*
    /// pops one waiter to wake it, removing that waiter from the queue
    /// before it has re-polled to claim the permit. If that woken
    /// acquirer is then cancelled before its next poll, the wake the
    /// queue was owed dies with it. So when we drop with a free permit
    /// visible, re-arm the next front waiter to hand the wake along.
    /// This covers the cancel-*after*-wake case that simply removing our
    /// own slot does not. Firing only when `available > 0` keeps the
    /// uncontended path untouched, and a spurious wake is harmless (the
    /// re-polled acquirer just re-parks).
    fn drop(&mut self) {
        if let Some(id) = self.id {
            let waker = {
                let mut inner = self.inner.borrow_mut();
                inner.waiters.retain(|(i, _)| *i != id);
                if inner.available > 0 {
                    inner.waiters.front().map(|(_, w)| w.clone())
                } else {
                    None
                }
            };
            // Wake outside the borrow so an eager re-poll can re-enter.
            if let Some(waker) = waker {
                waker.wake();
            }
        }
    }
}

/// An in-flight permit. Returns itself to the pool on drop and wakes
/// the next parked waiter, if any.
pub struct FetchPermit {
    inner: Rc<RefCell<LimiterInner>>,
}

impl Drop for FetchPermit {
    fn drop(&mut self) {
        let waker = {
            let mut inner = self.inner.borrow_mut();
            inner.available += 1;
            inner.waiters.pop_front().map(|(_, w)| w)
        };
        // Wake outside the borrow so an eager re-poll can re-enter.
        if let Some(waker) = waker {
            waker.wake();
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::task::{RawWaker, RawWakerVTable, Wake};

    struct CountWaker(std::sync::atomic::AtomicUsize);

    impl Wake for CountWaker {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
        }
    }

    fn noop_waker() -> Waker {
        fn no_op(_: *const ()) {}
        fn clone(_: *const ()) -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, no_op, no_op, no_op);
        // SAFETY: the vtable functions are all no-ops over a null data
        // pointer, which is the canonical noop waker construction.
        unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) }
    }

    #[test]
    fn acquires_up_to_limit_then_parks() {
        let limiter = FetchLimiter::new(2);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);

        let mut a = Box::pin(limiter.acquire());
        let mut b = Box::pin(limiter.acquire());
        let mut c = Box::pin(limiter.acquire());

        let pa = match a.as_mut().poll(&mut cx) {
            Poll::Ready(p) => p,
            Poll::Pending => panic!("first acquire should be ready"),
        };
        let _pb = match b.as_mut().poll(&mut cx) {
            Poll::Ready(p) => p,
            Poll::Pending => panic!("second acquire should be ready"),
        };
        assert_eq!(limiter.available(), 0);
        assert!(matches!(c.as_mut().poll(&mut cx), Poll::Pending));

        // Releasing one permit must wake the parked waiter and let `c`
        // make progress on its next poll.
        drop(pa);
        assert!(matches!(c.as_mut().poll(&mut cx), Poll::Ready(_)));
    }

    #[test]
    fn zero_limit_is_treated_as_one() {
        let limiter = FetchLimiter::new(0);
        let waker = noop_waker();
        let mut cx = Context::from_waker(&waker);
        let mut a = Box::pin(limiter.acquire());
        assert!(matches!(a.as_mut().poll(&mut cx), Poll::Ready(_)));
    }

    #[test]
    fn drop_wakes_one_parked_waiter() {
        let limiter = FetchLimiter::new(1);
        let count = Arc::new(CountWaker(std::sync::atomic::AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut cx = Context::from_waker(&waker);

        let mut a = Box::pin(limiter.acquire());
        let pa = match a.as_mut().poll(&mut cx) {
            Poll::Ready(p) => p,
            Poll::Pending => panic!("first acquire ready"),
        };
        let mut b = Box::pin(limiter.acquire());
        assert!(matches!(b.as_mut().poll(&mut cx), Poll::Pending));
        drop(pa);
        assert_eq!(count.0.load(std::sync::atomic::Ordering::SeqCst), 1);
    }

    /// A cancelled (dropped-while-parked) acquirer must not swallow the
    /// wake a later release owes to a live waiter behind it. This is the
    /// cross-task case the limiter actually faces: concurrent shard-local
    /// fetches can carry distinct wakers. Regression for the orphaned-waker
    /// lost wakeup.
    #[test]
    fn cancelled_waiter_does_not_strand_live_waiter() {
        let limiter = FetchLimiter::new(1);

        // Holder occupies the single permit.
        let holder = {
            let waker = noop_waker();
            let mut cx = Context::from_waker(&waker);
            let mut a = Box::pin(limiter.acquire());
            match a.as_mut().poll(&mut cx) {
                Poll::Ready(p) => p,
                Poll::Pending => panic!("first acquire should be ready"),
            }
        };

        // B parks first (distinct waker), then C parks behind it. B is
        // ahead of C in the FIFO queue.
        let b_count = Arc::new(CountWaker(std::sync::atomic::AtomicUsize::new(0)));
        let b_waker = Waker::from(b_count.clone());
        let mut b_cx = Context::from_waker(&b_waker);
        let mut b = Box::pin(limiter.acquire());
        assert!(matches!(b.as_mut().poll(&mut b_cx), Poll::Pending));

        let c_count = Arc::new(CountWaker(std::sync::atomic::AtomicUsize::new(0)));
        let c_waker = Waker::from(c_count.clone());
        let mut c_cx = Context::from_waker(&c_waker);
        let mut c = Box::pin(limiter.acquire());
        assert!(matches!(c.as_mut().poll(&mut c_cx), Poll::Pending));

        // B is cancelled while parked; its slot must be removed.
        drop(b);

        // Releasing the permit must wake C, not waste the wake on the
        // departed B.
        drop(holder);
        assert_eq!(c_count.0.load(std::sync::atomic::Ordering::SeqCst), 1);
        assert_eq!(b_count.0.load(std::sync::atomic::Ordering::SeqCst), 0);
        assert!(matches!(c.as_mut().poll(&mut c_cx), Poll::Ready(_)));
    }

    /// A release pops one waiter off the queue to wake it *before* that
    /// waiter has re-polled to claim the permit. If the woken acquirer is
    /// then cancelled in that window, the wake the queue was owed must be
    /// handed to the next live waiter, not lost - otherwise it parks
    /// forever while a permit sits free. Regression for the
    /// cancel-*after*-wake lost wakeup (the reverse ordering of
    /// `cancelled_waiter_does_not_strand_live_waiter`).
    #[test]
    fn waiter_cancelled_after_being_woken_does_not_strand_next() {
        let limiter = FetchLimiter::new(1);

        // Holder occupies the single permit.
        let holder = {
            let waker = noop_waker();
            let mut cx = Context::from_waker(&waker);
            let mut a = Box::pin(limiter.acquire());
            match a.as_mut().poll(&mut cx) {
                Poll::Ready(p) => p,
                Poll::Pending => panic!("first acquire should be ready"),
            }
        };

        // B parks first, then C parks behind it (distinct wakers).
        let b_count = Arc::new(CountWaker(std::sync::atomic::AtomicUsize::new(0)));
        let b_waker = Waker::from(b_count.clone());
        let mut b_cx = Context::from_waker(&b_waker);
        let mut b = Box::pin(limiter.acquire());
        assert!(matches!(b.as_mut().poll(&mut b_cx), Poll::Pending));

        let c_count = Arc::new(CountWaker(std::sync::atomic::AtomicUsize::new(0)));
        let c_waker = Waker::from(c_count.clone());
        let mut c_cx = Context::from_waker(&c_waker);
        let mut c = Box::pin(limiter.acquire());
        assert!(matches!(c.as_mut().poll(&mut c_cx), Poll::Pending));

        // Release wakes B (front of the queue) and frees the permit; B is
        // removed from the queue but has not yet re-polled to claim it.
        drop(holder);
        assert_eq!(b_count.0.load(std::sync::atomic::Ordering::SeqCst), 1);
        assert_eq!(limiter.available(), 1);

        // B is cancelled before re-polling. The permit is still free, so C
        // must be re-armed rather than left parked behind the departed B.
        drop(b);
        assert_eq!(c_count.0.load(std::sync::atomic::Ordering::SeqCst), 1);
        assert!(matches!(c.as_mut().poll(&mut c_cx), Poll::Ready(_)));
    }
}
