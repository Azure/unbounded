//! An explicitly driven deadline queue.
//!
//! [`Timers`] keeps pending deadlines ordered by `(deadline, registration
//! order)`. [`Timers::sleep_until`] returns a [`Sleep`] future that becomes
//! ready once the owner calls [`Timers::poll`] with a time at or after its
//! deadline. `Timers` never reads a clock: the owner passes the current time,
//! usually from the backend's [`Clock`](crate::Clock), so the same code runs
//! under real and simulated time.
//!
//! A typical loop expires due timers each turn and bounds its blocking wait
//! with [`Timers::next_deadline`]. See the [`task`](crate::task) module for a
//! complete loop.
//!
//! ```
//! use std::future::Future;
//! use std::pin::pin;
//! use std::task::{Context, Poll, Waker};
//! use std::time::{Duration, Instant};
//! use uring_runtime::time::Timers;
//!
//! let timers = Timers::new();
//! let start = Instant::now();
//! let mut sleep = pin!(timers.sleep_until(start + Duration::from_millis(5)));
//! let mut cx = Context::from_waker(Waker::noop());
//!
//! assert!(sleep.as_mut().poll(&mut cx).is_pending());
//! assert_eq!(timers.next_deadline(), Some(start + Duration::from_millis(5)));
//!
//! assert_eq!(timers.poll(start, 16), 0); // not due yet
//! assert_eq!(timers.poll(start + Duration::from_millis(5), 16), 1);
//! assert_eq!(sleep.as_mut().poll(&mut cx), Poll::Ready(()));
//! assert!(timers.is_empty());
//! ```

use std::cell::{Cell, RefCell};
use std::collections::BTreeMap;
use std::fmt;
use std::future::Future;
use std::pin::Pin;
use std::rc::Rc;
use std::task::{Context, Poll, Waker};
use std::time::Instant;

use crate::{Error, ErrorKind, Result};

type Key = (Instant, u64);

#[derive(Default)]
struct Inner {
    next_id: Cell<u64>,
    /// Pending deadlines and the waker of the task waiting on each, if any.
    pending: RefCell<BTreeMap<Key, Option<Waker>>>,
}

/// A single-thread queue of deadlines, driven by [`Timers::poll`].
///
/// `Timers` is a cheap `Rc` handle: clones share the same queue. Each pending
/// [`Sleep`] holds one entry, removed when it fires or is dropped, so the
/// queue never grows beyond the number of live sleeps.
///
/// ```compile_fail
/// fn require_send<T: Send>() {}
/// require_send::<uring_runtime::time::Timers>();
/// ```
#[derive(Clone, Default)]
pub struct Timers {
    inner: Rc<Inner>,
}

impl Timers {
    /// Create an empty queue.
    pub fn new() -> Self {
        Self::default()
    }

    /// A future that completes once [`poll`](Self::poll) is called with a
    /// time at or after `deadline`.
    ///
    /// The deadline is registered immediately, not on first poll, so it counts
    /// toward [`len`](Self::len) and [`next_deadline`](Self::next_deadline)
    /// right away. Deadlines that are equal fire in registration order.
    pub fn sleep_until(&self, deadline: Instant) -> Sleep {
        let id = self.inner.next_id.get();
        // 2^64 registrations cannot happen in practice; wrapping keeps this
        // infallible.
        self.inner.next_id.set(id.wrapping_add(1));
        let key = (deadline, id);
        self.inner.pending.borrow_mut().insert(key, None);
        Sleep {
            timers: self.inner.clone(),
            key,
        }
    }

    /// Run `future` until it completes or `deadline` passes, whichever is
    /// observed first. If both are ready in the same poll, the future's output
    /// wins.
    pub fn timeout<F: Future>(&self, deadline: Instant, future: F) -> Timeout<F> {
        Timeout {
            future,
            sleep: self.sleep_until(deadline),
        }
    }

    /// Fire at most `budget` entries whose deadline is at or before `now`,
    /// earliest first, and return how many fired.
    ///
    /// Fired entries are removed before their wakers run, and wakers run after
    /// the queue is released, so a waker may register or drop sleeps.
    /// Entries left over by a small budget stay due and are reported by
    /// [`next_deadline`](Self::next_deadline).
    pub fn poll(&self, now: Instant, budget: usize) -> usize {
        let mut wakers = Vec::new();
        let mut fired = 0;
        {
            let mut pending = self.inner.pending.borrow_mut();
            while fired < budget {
                let Some(entry) = pending.first_entry() else {
                    break;
                };
                if entry.key().0 > now {
                    break;
                }
                wakers.extend(entry.remove());
                fired += 1;
            }
        }
        for waker in wakers {
            waker.wake();
        }
        fired
    }

    /// The earliest pending deadline, including entries already due but not
    /// yet fired.
    pub fn next_deadline(&self) -> Option<Instant> {
        self.inner
            .pending
            .borrow()
            .first_key_value()
            .map(|(key, _)| key.0)
    }

    /// The number of pending entries.
    pub fn len(&self) -> usize {
        self.inner.pending.borrow().len()
    }

    /// Whether no entries are pending.
    pub fn is_empty(&self) -> bool {
        self.inner.pending.borrow().is_empty()
    }
}

impl fmt::Debug for Timers {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Timers")
            .field("len", &self.len())
            .field("next_deadline", &self.next_deadline())
            .finish()
    }
}

/// A future that completes when its [`Timers`] queue passes its deadline.
///
/// Created by [`Timers::sleep_until`]. Only the waker from the most recent
/// poll is woken. Dropping a `Sleep` removes its entry. Once fired, it stays
/// ready.
#[must_use = "futures do nothing unless polled"]
pub struct Sleep {
    timers: Rc<Inner>,
    key: Key,
}

impl Sleep {
    /// The deadline this sleep waits for.
    pub fn deadline(&self) -> Instant {
        self.key.0
    }

    /// Whether the deadline has fired, that is, polling would return `Ready`.
    pub fn is_elapsed(&self) -> bool {
        !self.timers.pending.borrow().contains_key(&self.key)
    }
}

impl Future for Sleep {
    type Output = ();

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        // Clone before borrowing: a waker clone may re-enter the queue.
        let waker = cx.waker().clone();
        let mut pending = self.timers.pending.borrow_mut();
        let Some(slot) = pending.get_mut(&self.key) else {
            // Ids are never reused, so only firing removes a live entry.
            return Poll::Ready(());
        };
        let old = slot.replace(waker);
        drop(pending);
        drop(old);
        Poll::Pending
    }
}

impl Drop for Sleep {
    fn drop(&mut self) {
        // Drop the waker outside the borrow: its destructor may re-enter.
        let removed = self.timers.pending.borrow_mut().remove(&self.key);
        drop(removed);
    }
}

impl fmt::Debug for Sleep {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("Sleep")
            .field("deadline", &self.deadline())
            .field("elapsed", &self.is_elapsed())
            .finish()
    }
}

/// A future with a deadline, created by [`Timers::timeout`].
///
/// Resolves to `Ok(output)`, or to a
/// [`DeadlineExceeded`](ErrorKind::DeadlineExceeded) error once the deadline
/// fired. On timeout the inner future is not dropped until the `Timeout` is.
#[must_use = "futures do nothing unless polled"]
#[derive(Debug)]
pub struct Timeout<F> {
    future: F,
    sleep: Sleep,
}

impl<F> Timeout<F> {
    /// The inner future.
    pub fn get_ref(&self) -> &F {
        &self.future
    }

    /// Consume the timeout, returning the inner future.
    pub fn into_inner(self) -> F {
        self.future
    }
}

impl<F: Future> Future for Timeout<F> {
    type Output = Result<F::Output>;

    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        // SAFETY: `future` is structurally pinned: this is the only place
        // that projects it, `Timeout` has no `Drop` impl, and `into_inner`
        // needs `self` by value, which a pinned `!Unpin` value cannot give.
        // `sleep` is `Unpin` and is not treated as pinned.
        let this = unsafe { self.get_unchecked_mut() };
        // SAFETY: see above.
        let future = unsafe { Pin::new_unchecked(&mut this.future) };
        if let Poll::Ready(output) = future.poll(cx) {
            return Poll::Ready(Ok(output));
        }
        match Pin::new(&mut this.sleep).poll(cx) {
            Poll::Ready(()) => Poll::Ready(Err(Error::new(ErrorKind::DeadlineExceeded))),
            Poll::Pending => Poll::Pending,
        }
    }
}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::Wake;
    use std::time::Duration;

    use super::*;

    #[derive(Default)]
    struct WakeCount(AtomicUsize);

    impl Wake for WakeCount {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn poll_with(sleep: &mut Sleep, waker: &Waker) -> Poll<()> {
        Pin::new(sleep).poll(&mut Context::from_waker(waker))
    }

    fn poll_now(sleep: &mut Sleep) -> Poll<()> {
        poll_with(sleep, Waker::noop())
    }

    #[test]
    fn equal_deadlines_fire_in_registration_order() {
        let timers = Timers::new();
        let now = Instant::now();
        let mut late = timers.sleep_until(now + Duration::from_secs(1));
        let mut first = timers.sleep_until(now);
        let mut second = timers.sleep_until(now);
        assert_eq!(timers.len(), 3);
        assert_eq!(timers.poll(now, 1), 1);
        assert!(poll_now(&mut first).is_ready());
        assert!(first.is_elapsed());
        assert!(poll_now(&mut second).is_pending());
        assert!(poll_now(&mut late).is_pending());
        assert_eq!(timers.poll(now, 8), 1);
        assert!(poll_now(&mut second).is_ready());
        assert!(poll_now(&mut late).is_pending());
        assert_eq!(late.deadline(), now + Duration::from_secs(1));
    }

    #[test]
    fn budget_and_drop_preserve_accounting() {
        let timers = Timers::new();
        let now = Instant::now();
        let mut first = timers.sleep_until(now);
        let second = timers.sleep_until(now);
        assert_eq!(timers.poll(now, 0), 0);
        assert_eq!(timers.poll(now - Duration::from_nanos(1), 8), 0);
        assert_eq!(timers.poll(now, 1), 1);
        assert!(poll_now(&mut first).is_ready());
        assert!(poll_now(&mut first).is_ready(), "a fired sleep stays ready");
        drop(second);
        assert!(timers.is_empty());
        assert_eq!(timers.poll(now, 8), 0);
    }

    #[test]
    fn next_deadline_tracks_drops_and_partial_expiration() {
        let timers = Timers::new();
        let now = Instant::now();
        let later = now + Duration::from_secs(1);
        assert_eq!(timers.next_deadline(), None);
        let mut late = timers.sleep_until(later);
        assert_eq!(timers.next_deadline(), Some(later));
        let first = timers.sleep_until(now);
        let second = timers.sleep_until(now);
        assert_eq!(timers.next_deadline(), Some(now));
        assert_eq!(timers.poll(now, 1), 1);
        assert_eq!(
            timers.next_deadline(),
            Some(now),
            "due entry left by budget"
        );
        drop(second);
        assert_eq!(timers.next_deadline(), Some(later));
        drop(first);
        assert_eq!(timers.poll(later, 1), 1);
        assert!(poll_now(&mut late).is_ready());
        assert_eq!(timers.next_deadline(), None);
        drop(timers.sleep_until(now));
        assert_eq!(timers.next_deadline(), None);
    }

    #[test]
    fn only_latest_waker_is_woken_and_drop_releases_it() {
        let timers = Timers::new();
        let now = Instant::now();
        let mut sleep = timers.sleep_until(now);
        let old = Arc::new(WakeCount::default());
        let latest = Arc::new(WakeCount::default());
        assert!(poll_with(&mut sleep, &Waker::from(old.clone())).is_pending());
        assert!(poll_with(&mut sleep, &Waker::from(latest.clone())).is_pending());
        assert_eq!(timers.poll(now, 1), 1);
        assert_eq!(old.0.load(Ordering::SeqCst), 0);
        assert_eq!(latest.0.load(Ordering::SeqCst), 1);

        let mut sleep = timers.sleep_until(now);
        let weak = Arc::downgrade(&latest);
        assert!(poll_with(&mut sleep, &Waker::from(latest)).is_pending());
        assert!(weak.upgrade().is_some());
        drop(sleep);
        assert!(weak.upgrade().is_none());
    }

    thread_local! {
        static TIMERS: RefCell<Option<Timers>> = const { RefCell::new(None) };
    }

    /// Registers and drops a nested sleep from wake and drop callbacks.
    struct Reentrant;

    impl Reentrant {
        fn access() {
            TIMERS.with(|slot| {
                let timers = slot.borrow().clone().unwrap();
                let before = timers.len();
                let nested = timers.sleep_until(Instant::now());
                assert_eq!(timers.len(), before + 1);
                drop(nested);
                let _ = timers.next_deadline();
            });
        }
    }

    impl Wake for Reentrant {
        fn wake(self: Arc<Self>) {
            Self::access();
        }
    }

    impl Drop for Reentrant {
        fn drop(&mut self) {
            Self::access();
        }
    }

    #[test]
    fn wake_and_drop_callbacks_may_reenter() {
        let timers = Timers::new();
        TIMERS.with(|slot| slot.replace(Some(timers.clone())));
        let now = Instant::now();
        let mut sleep = timers.sleep_until(now);
        assert!(poll_with(&mut sleep, &Waker::from(Arc::new(Reentrant))).is_pending());
        // Replacing the waker drops the old one outside the borrow.
        assert!(poll_with(&mut sleep, &Waker::from(Arc::new(Reentrant))).is_pending());
        assert_eq!(timers.poll(now, 1), 1);
        let mut sleep = timers.sleep_until(now);
        assert!(poll_with(&mut sleep, &Waker::from(Arc::new(Reentrant))).is_pending());
        drop(sleep);
        assert!(timers.is_empty());
        TIMERS.with(|slot| slot.take());
    }

    #[test]
    fn waker_clone_may_reenter_during_poll() {
        use std::task::{RawWaker, RawWakerVTable};
        unsafe fn clone(_: *const ()) -> RawWaker {
            Reentrant::access();
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        unsafe fn noop(_: *const ()) {}
        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, noop, noop, noop);

        let timers = Timers::new();
        TIMERS.with(|slot| slot.replace(Some(timers.clone())));
        let mut sleep = timers.sleep_until(Instant::now());
        // SAFETY: the vtable ignores its data pointer and owns no allocation.
        let waker = unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) };
        assert!(poll_with(&mut sleep, &waker).is_pending());
        drop(sleep);
        TIMERS.with(|slot| slot.take());
    }

    #[test]
    fn timeout_prefers_output_and_reports_deadline() {
        let timers = Timers::new();
        let now = Instant::now();
        let mut cx = Context::from_waker(Waker::noop());

        let mut ready = timers.timeout(now, std::future::ready(4));
        timers.poll(now, 8);
        assert_eq!(
            Pin::new(&mut ready).poll(&mut cx).map(Result::unwrap),
            Poll::Ready(4)
        );

        let mut pending = Box::pin(timers.timeout(now, std::future::pending::<()>()));
        assert!(pending.as_mut().poll(&mut cx).is_pending());
        timers.poll(now, 8);
        let Poll::Ready(Err(error)) = pending.as_mut().poll(&mut cx) else {
            panic!("timeout must fire");
        };
        assert!(error.is_deadline_exceeded());
        drop(pending);
        drop(ready);
        assert!(timers.is_empty());
    }

    #[test]
    fn sleep_wakes_executor_task() {
        use crate::task::LocalExecutor;
        let executor = LocalExecutor::new(1);
        let timers = Timers::new();
        let now = Instant::now();
        let sleep = timers.sleep_until(now + Duration::from_millis(1));
        let mut handle = executor.spawn(sleep).unwrap();
        assert_eq!(executor.run_ready(8), 1);
        assert_eq!(executor.run_ready(8), 0);
        timers.poll(now + Duration::from_millis(1), 8);
        assert_eq!(executor.run_ready(8), 1);
        let mut cx = Context::from_waker(Waker::noop());
        assert!(matches!(
            Pin::new(&mut handle).poll(&mut cx),
            Poll::Ready(Ok(()))
        ));
    }
}
