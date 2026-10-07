//! Bounded SPSC ownership handoffs, without locks or blocking sends.
//!
//! Each endpoint has exactly one polling waiter, even on a single thread. Prefer
//! [`Sender::ready`] for an exclusively borrowed readiness wait. Raw polling and
//! [`Receiver::register`] replace the previous waker, not broadcast to waiters.
//!
//! Endpoints can move between threads when the payload is `Send`, including
//! payloads that are not `Sync`. They cannot be shared between threads.
//!
//! ```
//! use std::cell::Cell;
//! use uring_runtime::channel::{Sender, Receiver};
//! fn assert_send<T: Send>() {}
//! assert_send::<Sender<Cell<u8>>>();
//! assert_send::<Receiver<Cell<u8>>>();
//! ```
//!
//! ```compile_fail
//! use uring_runtime::channel::Sender;
//! fn assert_sync<T: Sync>() {}
//! assert_sync::<Sender<u8>>();
//! ```
//!
//! ```compile_fail
//! use uring_runtime::channel::Receiver;
//! fn assert_sync<T: Sync>() {}
//! assert_sync::<Receiver<u8>>();
//! ```
//!
//! ```compile_fail
//! use std::rc::Rc;
//! use uring_runtime::channel::Sender;
//! fn assert_send<T: Send>() {}
//! assert_send::<Sender<Rc<u8>>>();
//! ```
//!
//! ```compile_fail
//! use std::rc::Rc;
//! use uring_runtime::channel::Receiver;
//! fn assert_send<T: Send>() {}
//! assert_send::<Receiver<Rc<u8>>>();
//! ```
use crate::{Error, Result};
use futures::task::AtomicWaker;
use std::{
    cell::{Cell, UnsafeCell},
    mem::MaybeUninit,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
};

/// The unique producer. Non-Sync also prevents concurrent calls through `&self`.
pub struct Sender<T> {
    shared: Arc<Shared<T>>,

    // A stale head only overestimates occupancy. Refresh at cached full, before
    // tail could move more than capacity ahead of this acquired head.
    cached_head: Cell<usize>,
}

/// The unique consumer. Neither endpoint is cloneable or shareable across threads.
pub struct Receiver<T> {
    shared: Arc<Shared<T>>,

    // A stale tail only underestimates availability. Refresh at cached empty.
    cached_tail: Cell<usize>,
}

/// Rejected publication, returning the original value to its caller.
pub struct SendFailure<T> {
    /// Value whose ownership was not transferred.
    pub command: T,

    /// Saturation or closure that prevented publication.
    pub error: Error,
}

/// Allocate a fixed-capacity queue with one non-cloneable endpoint per direction.
/// Zero or overflowing capacities are invalid; allocation failure is overload.
pub fn bounded<T>(capacity: usize) -> Result<(Sender<T>, Receiver<T>)> {
    if capacity == 0 || capacity > isize::MAX as usize / std::mem::size_of::<T>().max(1) {
        return Err(Error::InvalidConfiguration);
    }
    let mut slots = Vec::new();
    slots
        .try_reserve_exact(capacity)
        .map_err(|_| Error::Overloaded)?;
    slots.resize_with(capacity, || UnsafeCell::new(MaybeUninit::uninit()));
    let shared = Arc::new(Shared {
        slots: slots.into_boxed_slice(),
        head: Cursor(AtomicUsize::new(0)),
        tail: Cursor(AtomicUsize::new(0)),
        sender_closed: AtomicBool::new(false),
        receiver_closed: AtomicBool::new(false),
        readable: AtomicWaker::new(),
        writable: AtomicWaker::new(),
    });
    Ok((
        Sender {
            shared: shared.clone(),
            cached_head: Cell::new(0),
        },
        Receiver {
            shared,
            cached_tail: Cell::new(0),
        },
    ))
}

impl<T> Sender<T> {
    /// Reclaim orphaned messages only after the sole receiver has been destroyed.
    /// Returns false without dropping anything while the receiver is alive.
    ///
    /// Destructors may reenter this method. A panicking destructor has already
    /// been removed; a later call (or final endpoint drop) reclaims the remainder.
    pub fn discard_closed(&self) -> bool {
        if !self.shared.receiver_closed.load(Ordering::Acquire) {
            return false;
        }
        let tail = self.shared.tail.0.load(Ordering::Acquire);
        loop {
            // receiver_closed is monotonic: all subsequent sends fail, including
            // reentrant sends. The unique non-Sync sender cannot be publishing
            // concurrently, so tail is now fixed. Acquiring receiver_closed also
            // observes its last head update; the receiver will never touch slots
            // again. Head, unlike tail, MUST be reloaded after each destructor:
            // recursive discard_closed may have drained any or all of the rest.
            let head = self.shared.head.0.load(Ordering::Relaxed);
            if head == tail {
                self.cached_head.set(head);
                break;
            }
            // SAFETY: head != fixed tail identifies an initialized, exclusively
            // owned slot. No user code runs between the move and head publication.
            let item = unsafe {
                (*self.shared.slots[slot_index(head, self.shared.slots.len())].get())
                    .assume_init_read()
            };
            let next = advance(head, self.shared.slots.len());
            self.shared.head.0.store(next, Ordering::Release);
            self.cached_head.set(next);
            // Publish before arbitrary code: both reentry and unwind see this
            // item as removed. Never restore a cached head after this point.
            drop(item);
        }
        true
    }

    /// Transfer ownership without waiting, or return the original value on error.
    /// A send racing receiver destruction may succeed; such an orphan is reclaimed
    /// by [`Self::discard_closed`] or by final endpoint destruction.
    pub fn try_send(&self, command: T) -> std::result::Result<(), SendFailure<T>> {
        if self.shared.publication_closed() {
            return Err(SendFailure {
                command,
                error: Error::Unavailable,
            });
        }
        let tail = self.shared.tail.0.load(Ordering::Relaxed);
        let capacity = self.shared.slots.len();
        let mut head = self.cached_head.get();
        debug_assert!(distance(tail, head, capacity) <= capacity);
        if is_full(tail, head, capacity) {
            head = self.shared.head.0.load(Ordering::Acquire);
            self.cached_head.set(head);
            if is_full(tail, head, capacity) {
                return Err(SendFailure {
                    command,
                    error: Error::Overloaded,
                });
            }
        }
        // SAFETY: acquiring head proves this slot is no longer initialized or
        // being read. Only this endpoint writes tail, and there is no user code
        // until publication completes. A racing receiver drop cannot drain slots.
        unsafe {
            (*self.shared.slots[slot_index(tail, capacity)].get()).write(command);
        }
        self.shared
            .tail
            .0
            .store(advance(tail, capacity), Ordering::Release);
        self.shared.readable.wake();
        Ok(())
    }

    /// Stop new sends and wake both sides. Already published values remain readable.
    pub fn close(&self) {
        self.shared.sender_closed.store(true, Ordering::Release);
        self.shared.readable.wake();
        self.shared.writable.wake();
    }

    /// Wait for capacity or closure with exclusive access to this endpoint.
    ///
    /// Readiness is not a reservation; use [`Self::try_send`] to transfer the value
    /// and handle receiver closure. Canceling this future neither sends nor takes
    /// capacity. A canceled wait may leave a stale waker until the next wait/wake.
    /// The exclusive borrow prevents two outstanding readiness futures:
    ///
    /// ```compile_fail
    /// use uring_runtime::channel::bounded;
    /// let (mut sender, _receiver) = bounded::<u8>(1).unwrap();
    /// let first = sender.ready();
    /// let second = sender.ready();
    /// drop((first, second));
    /// ```
    ///
    /// The future can move between threads when the payload is `Send`, even if
    /// the payload and endpoint are not `Sync`:
    ///
    /// ```
    /// use std::cell::Cell;
    /// use uring_runtime::channel::{bounded, Sender};
    /// fn assert_send(_: impl Send) {}
    /// let (mut sender, _receiver): (Sender<Cell<u8>>, _) = bounded(1).unwrap();
    /// assert_send(sender.ready());
    /// ```
    pub fn ready(&mut self) -> impl std::future::Future<Output = Result<()>> + '_ {
        std::future::poll_fn(move |cx| self.poll_ready(cx))
    }

    /// Low-level readiness polling, for a caller managing one logical waiter.
    ///
    /// Only the most recently registered waker is notified. Do not interleave
    /// outstanding waits from different tasks, even on the same thread: replacing
    /// a pending task's waker can strand it. Like [`Receiver::poll_receive`], this
    /// contract spans polls, not just the duration of this call. Prefer
    /// [`Self::ready`] to enforce exclusive waiting with the borrow checker.
    /// Readiness is advisory and does not reserve a slot.
    pub fn poll_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        // Register BEFORE checking state. A transition before registration is
        // observed below; one during/after registration wakes the registered task.
        self.shared.writable.register(cx.waker());
        if self.shared.publication_closed() {
            return Poll::Ready(Err(Error::Unavailable));
        }
        // Advisory checks use authoritative cursors without changing cached_head.
        // In particular, registration can reenter and advance either endpoint.
        if distance(
            self.shared.tail.0.load(Ordering::Acquire),
            self.shared.head.0.load(Ordering::Acquire),
            self.shared.slots.len(),
        ) < self.shared.slots.len()
        {
            Poll::Ready(Ok(()))
        } else {
            Poll::Pending
        }
    }
}

impl<T> Drop for Sender<T> {
    /// Close publication before releasing this endpoint's shared storage owner.
    fn drop(&mut self) {
        self.close();
    }
}

impl<T> Receiver<T> {
    /// Whether the producer closed publication; queued values may remain readable.
    pub fn is_closed(&self) -> bool {
        self.shared.sender_closed.load(Ordering::Acquire)
    }

    /// Register the sole readable waiter before checking data/closure.
    /// Replaces any previous waker, including one from [`Self::poll_receive`].
    /// A caller must not interleave this with another task's outstanding wait.
    pub fn register(&self, waker: &std::task::Waker) {
        self.shared.readable.register(waker);
    }

    /// Take one value without waiting; `None` means currently empty, not necessarily EOF.
    pub fn receive(&mut self) -> Result<Option<T>> {
        self.receive_shared()
    }

    /// Receive for a local facade that cannot expose an exclusive endpoint borrow.
    /// Non-Sync access and publication before callbacks keep reentry ownership-safe.
    pub(crate) fn receive_shared(&self) -> Result<Option<T>> {
        let head = self.shared.head.0.load(Ordering::Relaxed);
        let capacity = self.shared.slots.len();
        let mut tail = self.cached_tail.get();
        debug_assert!(distance(tail, head, capacity) <= capacity);
        if head == tail {
            tail = self.shared.tail.0.load(Ordering::Acquire);
            self.cached_tail.set(tail);
            if head == tail {
                return Ok(None);
            }
        }
        // SAFETY: acquiring tail observes initialization, and the unique receiver
        // owns this slot until it publishes head. No user code can reenter in
        // between the move and publication, including for zero-sized T.
        let item =
            unsafe { (*self.shared.slots[slot_index(head, capacity)].get()).assume_init_read() };
        self.shared
            .head
            .0
            .store(advance(head, capacity), Ordering::Release);
        self.shared.writable.wake();
        Ok(Some(item))
    }

    /// Poll the sole readable waiter. Returns `Ready(Ok(None))` only once closed
    /// and drained. Only the last registered waker is notified; the caller must
    /// not alternate outstanding waits from different tasks between polls.
    pub fn poll_receive(&mut self, cx: &mut Context<'_>) -> Poll<Result<Option<T>>> {
        self.poll_receive_shared(cx)
    }

    /// Poll for a local facade obeying the same single-waiter contract as the endpoint.
    pub(crate) fn poll_receive_shared(&self, cx: &mut Context<'_>) -> Poll<Result<Option<T>>> {
        // Internal shared callers must obey the same single-waiter contract.
        self.shared.readable.register(cx.waker());
        match self.receive_shared() {
            Ok(None) if !self.shared.sender_closed.load(Ordering::Acquire) => Poll::Pending,
            // The first empty check may precede the final send. Acquiring close
            // observes its tail publication, so check again before reporting EOF.
            Ok(None) => Poll::Ready(self.receive_shared()),
            result => Poll::Ready(result),
        }
    }
}

impl<T> Drop for Receiver<T> {
    /// Transfer orphan ownership to the sender before notifying its waiter.
    fn drop(&mut self) {
        // Publish the final head before transferring orphan ownership to sender.
        // Do not touch slots after this store: wake may synchronously reenter it.
        self.shared.receiver_closed.store(true, Ordering::Release);
        self.shared.writable.wake();
    }
}

/// Cache-line-separated ownership cursor, independent of endpoint layout.
#[repr(align(64))]
struct Cursor(AtomicUsize);

/// Ring storage whose initialized slots belong to exactly one endpoint at a time.
struct Shared<T> {
    slots: Box<[UnsafeCell<MaybeUninit<T>>]>,

    head: Cursor,

    tail: Cursor,

    sender_closed: AtomicBool,

    receiver_closed: AtomicBool,

    readable: AtomicWaker,

    writable: AtomicWaker,
}

impl<T> Shared<T> {
    /// Observe either permanent publication barrier without touching peer caches.
    fn publication_closed(&self) -> bool {
        self.sender_closed.load(Ordering::Acquire) || self.receiver_closed.load(Ordering::Acquire)
    }
}

// SAFETY: endpoints are unique (not Clone), non-Sync, and expose no slot references.
// Only the sender writes unpublished slots; only the receiver moves out published
// slots until receiver_closed is acquired. Release/acquire tail publication makes
// initialization visible to the receiver; release/acquire head publication makes
// completed reads visible before slot reuse. Cursor distance is at most capacity.
// Modulo 2*capacity distinguishes empty from full, including non-power-of-two
// capacities. bounded checks also ensure cursor arithmetic cannot overflow.
//
// Once receiver_closed is acquired, only the sender may drain the remaining slots.
// Endpoint methods invoke arbitrary code (wakers or destructors) only outside the
// slot access/cursor publication interval. Reentrant calls therefore observe the
// authoritative owner cursors and already-updated conservative peer caches, never
// an outstanding reference or partially moved value. No cache is restored after
// arbitrary code: nested operations may have advanced it through a complete wrap.
// T: Send suffices because ownership, not a shared reference to T, crosses threads.
unsafe impl<T: Send> Sync for Shared<T> {}

// SAFETY: same ownership-transfer invariant as above. Final Arc destruction has
// exclusive access to Shared, with no surviving endpoints or in-flight operations.
unsafe impl<T: Send> Send for Shared<T> {}

impl<T> Drop for Shared<T> {
    /// Destroy remaining initialized slots after both endpoints have gone away.
    fn drop(&mut self) {
        let mut head = *self.head.0.get_mut();
        let tail = *self.tail.0.get_mut();
        while head != tail {
            // SAFETY: the last Arc owns all slots in [head, tail). No endpoint can
            // reenter this Shared, and each initialized slot is visited once. If a
            // destructor panics, remaining MaybeUninit slots may leak, not redrop.
            unsafe {
                self.slots[slot_index(head, self.slots.len())]
                    .get_mut()
                    .assume_init_drop();
            }
            head = advance(head, self.slots.len());
        }
    }
}

/// Advance through two laps so equal slots can distinguish full from empty.
fn advance(cursor: usize, capacity: usize) -> usize {
    if cursor + 1 == capacity * 2 {
        0
    } else {
        cursor + 1
    }
}

/// Count occupied slots using two-lap cursors and a conservative peer observation.
fn distance(tail: usize, head: usize, capacity: usize) -> usize {
    if tail >= head {
        tail - head
    } else {
        capacity * 2 - head + tail
    }
}

/// Map either cursor lap to its physical slot without requiring power-of-two capacity.
fn slot_index(cursor: usize, capacity: usize) -> usize {
    debug_assert!(cursor < capacity * 2);
    if cursor >= capacity {
        cursor - capacity
    } else {
        cursor
    }
}

/// Recognize cursors exactly one capacity apart, excluding the empty equal case.
fn is_full(tail: usize, head: usize, capacity: usize) -> bool {
    tail != head && slot_index(tail, capacity) == slot_index(head, capacity)
}

/// Ownership, cursor, callback, and wake-registration state-space regressions.
#[cfg(test)]
mod tests {
    use super::*;
    use crate::test_util::WakeCounter;
    use std::{
        cell::RefCell,
        rc::Rc,
        sync::Barrier,
        task::{RawWaker, RawWakerVTable, Waker},
    };

    /// Record each payload's identity when ownership is finally released.
    struct Tracked(usize, Rc<RefCell<Vec<usize>>>);

    impl Drop for Tracked {
        /// Append this payload to the destruction log.
        fn drop(&mut self) {
            self.1.borrow_mut().push(self.0);
        }
    }

    /// Keep local endpoints compact while isolating cross-thread cursor writes.
    #[test]
    fn endpoint_layout_is_compact_but_shared_cursors_remain_isolated() {
        use std::mem::{align_of, offset_of, size_of};
        assert_eq!(align_of::<Sender<u8>>(), align_of::<usize>());
        assert_eq!(align_of::<Receiver<u8>>(), align_of::<usize>());
        assert_eq!(size_of::<Sender<u8>>(), 2 * size_of::<usize>());
        assert_eq!(size_of::<Receiver<u8>>(), 2 * size_of::<usize>());
        assert_eq!(align_of::<Cursor>(), 64);
        assert_eq!(size_of::<Cursor>(), 64);
        let head = offset_of!(Shared<u8>, head);
        let tail = offset_of!(Shared<u8>, tail);
        assert_eq!(head % 64, 0);
        assert_eq!(tail % 64, 0);
        assert!(head.abs_diff(tail) >= 64);
    }

    /// Check cursor arithmetic across capacities, occupancy levels, and both laps.
    #[test]
    fn cursor_mapping_and_distance_cover_both_laps_and_maximum_capacity() {
        for capacity in [1, 2, 3, 7, 256] {
            for head in 0..2 * capacity {
                assert_eq!(slot_index(head, capacity), head % capacity);
                assert_eq!(advance(head, capacity), (head + 1) % (2 * capacity));
                for occupied in 0..=capacity {
                    let tail = (head + occupied) % (2 * capacity);
                    assert_eq!(distance(tail, head, capacity), occupied);
                    assert_eq!(is_full(tail, head, capacity), occupied == capacity);
                }
            }
        }
        let capacity = isize::MAX as usize;
        assert_eq!(advance(2 * capacity - 1, capacity), 0);
        assert_eq!(slot_index(2 * capacity - 1, capacity), capacity - 1);
        assert_eq!(distance(capacity - 1, 2 * capacity - 1, capacity), capacity);
        assert!(is_full(capacity - 1, 2 * capacity - 1, capacity));
        assert!(!is_full(2 * capacity - 1, 2 * capacity - 1, capacity));
    }

    /// Advisory polling must not invalidate either endpoint's conservative cache.
    #[test]
    fn peer_caches_remain_conservative_across_wrap_and_advisory_polling() {
        for capacity in [1, 2, 3, 7, 256] {
            let (sender, receiver) = bounded(capacity).unwrap();
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            for round in 0..8 {
                for item in 0..capacity {
                    assert!(sender.try_send((round, item)).is_ok());
                    let cached = sender.cached_head.get();
                    let tail = sender.shared.tail.0.load(Ordering::Relaxed);
                    let head = sender.shared.head.0.load(Ordering::Relaxed);
                    assert!(distance(tail, cached, capacity) <= capacity);
                    assert!(distance(tail, cached, capacity) >= distance(tail, head, capacity));
                    let _ = sender.poll_ready(&mut cx);
                    assert_eq!(sender.cached_head.get(), cached);
                }
                assert_eq!(
                    sender.try_send((0, 0)).err().unwrap().error,
                    Error::Overloaded
                );
                for item in 0..capacity {
                    assert_eq!(receiver.receive_shared().unwrap(), Some((round, item)));
                    let cached = receiver.cached_tail.get();
                    let tail = receiver.shared.tail.0.load(Ordering::Relaxed);
                    let head = receiver.shared.head.0.load(Ordering::Relaxed);
                    assert!(distance(cached, head, capacity) <= distance(tail, head, capacity));
                    let sender_cache = sender.cached_head.get();
                    assert_eq!(sender.poll_ready(&mut cx), Poll::Ready(Ok(())));
                    assert_eq!(sender.cached_head.get(), sender_cache);
                }
                assert_eq!(receiver.receive_shared().unwrap(), None);
            }
        }
    }

    /// One raw-waker event and the thread-local callback it should trigger.
    type Callback = (u8, Box<dyn FnOnce()>);

    thread_local! {
        static CALLBACK: RefCell<Option<Callback>> = const { RefCell::new(None) };
    }

    /// Take and run the matching hook without retaining its thread-local borrow.
    fn callback(event: u8) {
        let hook = CALLBACK.with(|slot| {
            let mut slot = slot.borrow_mut();
            if slot
                .as_ref()
                .is_some_and(|(expected, _)| *expected == event)
            {
                slot.take()
            } else {
                None
            }
        });
        if let Some((_, hook)) = hook {
            hook();
        }
    }

    /// Remove an unconsumed callback when a test scope ends.
    struct CallbackGuard;

    impl Drop for CallbackGuard {
        /// Release any remaining hook outside its thread-local borrow.
        fn drop(&mut self) {
            let old = CALLBACK.with(|slot| slot.borrow_mut().take());
            drop(old);
        }
    }

    /// Install one hook for cloning, waking, or dropping the test waker.
    fn on_callback(event: u8, hook: impl FnOnce() + 'static) -> CallbackGuard {
        CALLBACK.with(|slot| *slot.borrow_mut() = Some((event, Box::new(hook))));
        CallbackGuard
    }

    /// Build a thread-safe raw waker that consults only the executing thread's hook.
    fn callback_waker() -> Waker {
        /// Run the clone hook and return another stateless raw waker.
        unsafe fn clone(_: *const ()) -> RawWaker {
            callback(0);
            RawWaker::new(std::ptr::null(), &VTABLE)
        }

        /// Run the wake hook without consuming any pointer-backed ownership.
        unsafe fn wake(_: *const ()) {
            callback(1);
        }

        /// Run the destruction hook for this stateless waker.
        unsafe fn drop_raw(_: *const ()) {
            callback(2);
        }

        static VTABLE: RawWakerVTable = RawWakerVTable::new(clone, wake, wake, drop_raw);
        // SAFETY: no pointer or ownership is carried in the waker. Each callback
        // only accesses the current thread's hook, even when moved across threads.
        unsafe { Waker::from_raw(RawWaker::new(std::ptr::null(), &VTABLE)) }
    }

    /// Nested sends may advance through wraps without an outer cache overwrite.
    #[test]
    fn send_wake_reentry_refreshes_caches_without_outer_restore() {
        let (sender, receiver) = bounded(3).unwrap();
        let (sender, receiver) = (Rc::new(sender), Rc::new(receiver));
        for item in 0..3 {
            assert!(sender.try_send(item).is_ok());
        }
        for item in 0..3 {
            assert_eq!(receiver.receive_shared().unwrap(), Some(item));
        }
        let waker = callback_waker();
        receiver.register(&waker);
        let (tx, rx) = (sender.clone(), receiver.clone());
        let final_cache = Rc::new(Cell::new(usize::MAX));
        let observed = final_cache.clone();
        let _hook = on_callback(1, move || {
            assert_eq!(rx.receive_shared().unwrap(), Some(3));
            for item in 4..14 {
                assert!(tx.try_send(item).is_ok());
                assert_eq!(rx.receive_shared().unwrap(), Some(item));
            }
            assert!(tx.try_send(14).is_ok());
            observed.set(tx.cached_head.get());
        });
        assert!(sender.try_send(3).is_ok());
        assert_ne!(final_cache.get(), usize::MAX, "wake hook ran");
        assert_eq!(sender.cached_head.get(), final_cache.get());
        assert_eq!(receiver.receive_shared().unwrap(), Some(14));
        assert_eq!(receiver.receive_shared().unwrap(), None);
    }

    /// Nested receives may advance through wraps without an outer cache overwrite.
    #[test]
    fn receive_wake_reentry_refreshes_caches_without_outer_restore() {
        let (sender, receiver) = bounded(3).unwrap();
        let (sender, receiver) = (Rc::new(sender), Rc::new(receiver));
        for item in 0..3 {
            assert!(sender.try_send(item).is_ok());
        }
        let waker = callback_waker();
        assert!(
            sender
                .poll_ready(&mut Context::from_waker(&waker))
                .is_pending()
        );
        let (tx, rx) = (sender.clone(), receiver.clone());
        let final_cache = Rc::new(Cell::new(usize::MAX));
        let observed = final_cache.clone();
        let _hook = on_callback(1, move || {
            assert_eq!(rx.receive_shared().unwrap(), Some(1));
            assert_eq!(rx.receive_shared().unwrap(), Some(2));
            for item in 3..13 {
                assert!(tx.try_send(item).is_ok());
                assert_eq!(rx.receive_shared().unwrap(), Some(item));
            }
            assert!(tx.try_send(13).is_ok());
            observed.set(rx.cached_tail.get());
        });
        assert_eq!(receiver.receive_shared().unwrap(), Some(0));
        assert_ne!(final_cache.get(), usize::MAX, "wake hook ran");
        assert_eq!(receiver.cached_tail.get(), final_cache.get());
        assert_eq!(receiver.receive_shared().unwrap(), Some(13));
        assert_eq!(receiver.receive_shared().unwrap(), None);
    }

    /// Clone and drop hooks may mutate either endpoint during waiter registration.
    #[test]
    fn registration_clone_and_drop_reentry_can_wrap_cached_cursors() {
        for event in [0, 2] {
            for writable in [false, true] {
                let (sender, receiver) = bounded(3).unwrap();
                let (sender, receiver) = (Rc::new(sender), Rc::new(receiver));
                let waker = callback_waker();
                let poll = |waker: &Waker| {
                    let mut cx = Context::from_waker(waker);
                    if writable {
                        assert_eq!(sender.poll_ready(&mut cx), Poll::Ready(Ok(())));
                    } else {
                        assert_eq!(receiver.poll_receive_shared(&mut cx), Poll::Pending);
                    }
                };
                if event == 2 {
                    poll(&waker);
                }
                let (tx, rx) = (sender.clone(), receiver.clone());
                let called = Rc::new(Cell::new(false));
                let observed = called.clone();
                let _hook = on_callback(event, move || {
                    for item in 0..14 {
                        assert!(tx.try_send(item).is_ok());
                        assert_eq!(rx.receive_shared().unwrap(), Some(item));
                    }
                    observed.set(true);
                });
                poll(if event == 0 {
                    &waker
                } else {
                    futures::task::noop_waker_ref()
                });
                assert!(called.get());
                assert!(sender.try_send(99).is_ok());
                assert_eq!(receiver.receive_shared().unwrap(), Some(99));
                assert_eq!(receiver.receive_shared().unwrap(), None);
            }
        }
    }

    /// Both endpoint destruction orders preserve exactly-once payload ownership.
    #[test]
    fn endpoint_drop_orders_reclaim_each_value_once() {
        for sender_first in [false, true] {
            let drops = Rc::new(RefCell::new(Vec::new()));
            let (sender, mut receiver) = bounded(3).unwrap();
            for id in 0..3 {
                assert!(sender.try_send(Tracked(id, drops.clone())).is_ok());
            }
            let rejected = sender.try_send(Tracked(3, drops.clone())).err().unwrap();
            assert_eq!(rejected.error, Error::Overloaded);
            assert!(
                drops.borrow().is_empty(),
                "failure retains caller ownership"
            );
            drop(rejected.command);
            drop(receiver.receive().unwrap());
            assert!(
                !sender.discard_closed(),
                "a live receiver owns queued values"
            );
            assert_eq!(*drops.borrow(), [3, 0]);
            if sender_first {
                drop(sender);
                assert_eq!(*drops.borrow(), [3, 0]);
                drop(receiver);
            } else {
                drop(receiver);
                assert_eq!(*drops.borrow(), [3, 0]);
                drop(sender);
            }
            assert_eq!(*drops.borrow(), [3, 0, 1, 2]);
        }
    }

    /// Nested panicking cleanup leaves the authoritative head ready to resume.
    #[test]
    fn reentrant_cleanup_after_wrap_and_panic_resumes_from_authoritative_head() {
        use std::rc::Weak;

        /// Payload that checks publication ordering and injects nested cleanup failure.
        struct Item {
            id: usize,

            sender: Weak<Sender<Item>>,

            drops: Rc<RefCell<Vec<usize>>>,
        }

        impl Drop for Item {
            /// Record removal before reentering cleanup or raising the injected panic.
            fn drop(&mut self) {
                self.drops.borrow_mut().push(self.id);
                let sender = self.sender.upgrade().unwrap();
                if sender.shared.receiver_closed.load(Ordering::Acquire) {
                    assert_eq!(
                        sender.cached_head.get(),
                        sender.shared.head.0.load(Ordering::Relaxed),
                        "cleanup publishes its cache before invoking a destructor"
                    );
                }
                if self.id == 3 {
                    // The nested cleanup removes 4 before its destructor panics.
                    // The outer cleanup must never put either 3 or 4 back.
                    self.sender.upgrade().unwrap().discard_closed();
                } else if self.id == 4 {
                    panic!("nested destructor failure");
                }
            }
        }
        let drops = Rc::new(RefCell::new(Vec::new()));
        let (sender, mut receiver) = bounded(3).unwrap();
        let sender = Rc::new(sender);
        for id in 0..6 {
            assert!(
                sender
                    .try_send(Item {
                        id,
                        sender: Rc::downgrade(&sender),
                        drops: drops.clone(),
                    })
                    .is_ok()
            );
            if id < 3 {
                drop(receiver.receive().unwrap());
            }
        }
        drop(receiver);
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| { sender.discard_closed() }))
                .is_err()
        );
        assert_eq!(*drops.borrow(), [0, 1, 2, 3, 4]);
        assert_eq!(
            sender.cached_head.get(),
            sender.shared.head.0.load(Ordering::Relaxed),
            "unwind must not restore an outer cleanup's stale cache"
        );
        assert!(sender.discard_closed());
        assert!(sender.discard_closed());
        drop(sender);
        assert_eq!(*drops.borrow(), [0, 1, 2, 3, 4, 5]);
    }

    /// Zero-sized payloads retain distinct ownership despite sharing storage addresses.
    #[test]
    fn zero_sized_values_still_have_exactly_once_destructors() {
        static DROPS: AtomicUsize = AtomicUsize::new(0);

        /// Zero-sized payload with observable destruction.
        struct Zst;

        impl Drop for Zst {
            /// Count the release of one logical payload.
            fn drop(&mut self) {
                DROPS.fetch_add(1, Ordering::Relaxed);
            }
        }
        assert_eq!(std::mem::size_of::<Zst>(), 0);
        let (sender, mut receiver) = bounded(3).unwrap();
        for _ in 0..12 {
            assert!(sender.try_send(Zst).is_ok());
            drop(receiver.receive().unwrap());
        }
        for _ in 0..3 {
            assert!(sender.try_send(Zst).is_ok());
        }
        drop(sender.try_send(Zst).err().unwrap().command);
        drop(receiver);
        assert!(sender.discard_closed());
        drop(sender);
        assert_eq!(DROPS.load(Ordering::Relaxed), 16);
        let (sender, receiver) = bounded(1).unwrap();
        assert!(sender.try_send(Zst).is_ok());
        drop(sender);
        drop(receiver);
        assert_eq!(DROPS.load(Ordering::Relaxed), 17);
    }

    /// Canceled readiness waits release their borrow without reserving queue capacity.
    #[test]
    fn readiness_future_cancellation_and_closure() {
        use std::future::Future;
        let (mut sender, mut receiver) = bounded(1).unwrap();
        assert!(sender.try_send(7).is_ok());
        let old = Arc::new(WakeCounter::default());
        let old_waker = Waker::from(old.clone());
        {
            let mut wait = std::pin::pin!(sender.ready());
            assert!(
                wait.as_mut()
                    .poll(&mut Context::from_waker(&old_waker))
                    .is_pending()
            );
        }
        let current = Arc::new(WakeCounter::default());
        let waker = Waker::from(current.clone());
        let mut cx = Context::from_waker(&waker);
        {
            let mut wait = std::pin::pin!(sender.ready());
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            assert_eq!(receiver.receive().unwrap(), Some(7));
            assert_eq!(old.count(), 0, "canceled waiter was replaced");
            assert_eq!(current.count(), 1);
            assert_eq!(wait.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        }
        assert!(sender.try_send(8).is_ok());
        {
            let mut wait = std::pin::pin!(sender.ready());
            assert!(wait.as_mut().poll(&mut cx).is_pending());
            drop(receiver);
            assert_eq!(current.count(), 2);
            assert_eq!(
                wait.as_mut().poll(&mut cx),
                Poll::Ready(Err(Error::Unavailable))
            );
        }
        let (mut sender, _receiver) = bounded::<u8>(1).unwrap();
        sender.close();
        let mut wait = std::pin::pin!(sender.ready());
        assert_eq!(
            wait.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Unavailable))
        );
    }

    /// Racing publication and receiver destruction cannot leak or duplicate an owner.
    #[test]
    fn receiver_drop_racing_send_preserves_ownership() {
        /// Cross-thread payload with an atomic destruction count.
        struct Counted(Arc<AtomicUsize>);

        impl Drop for Counted {
            /// Record this payload's final release.
            fn drop(&mut self) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        for _ in 0..64 {
            let drops = Arc::new(AtomicUsize::new(0));
            let (sender, receiver) = bounded(1).unwrap();
            let barrier = Arc::new(Barrier::new(2));
            let peer = barrier.clone();
            let worker = std::thread::spawn(move || {
                peer.wait();
                drop(receiver);
            });
            barrier.wait();
            if let Err(failed) = sender.try_send(Counted(drops.clone())) {
                assert_eq!(failed.error, Error::Unavailable);
                drop(failed.command);
            }
            worker.join().unwrap();
            assert!(sender.discard_closed());
            drop(sender);
            assert_eq!(drops.load(Ordering::Relaxed), 1);
        }
    }

    /// Closing publication must not hide the final value from a racing consumer.
    #[test]
    fn sender_close_racing_receive_never_reports_eof_before_final_value() {
        for _ in 0..64 {
            let (sender, mut receiver) = bounded(1).unwrap();
            let barrier = Arc::new(Barrier::new(2));
            let peer = barrier.clone();
            let worker = std::thread::spawn(move || {
                peer.wait();
                assert!(sender.try_send(42).is_ok());
                sender.close();
            });
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            barrier.wait();
            let first = receiver.poll_receive(&mut cx);
            worker.join().unwrap();
            match first {
                Poll::Pending => {
                    assert_eq!(receiver.poll_receive(&mut cx), Poll::Ready(Ok(Some(42))))
                }
                Poll::Ready(Ok(Some(42))) => (),
                other => panic!("premature EOF or lost value: {other:?}"),
            }
            assert_eq!(receiver.poll_receive(&mut cx), Poll::Ready(Ok(None)));
        }
    }

    /// Block in raw-waker cloning to force a transition during registration.
    /// Callbacks touch only atomics and a shared barrier, so the waker is thread-safe.
    struct RegistrationWake {
        barrier: Arc<Barrier>,

        first_clone: AtomicBool,

        wakes: AtomicUsize,
    }

    impl RegistrationWake {
        const VTABLE: RawWakerVTable = RawWakerVTable::new(
            Self::clone_raw,
            Self::wake_raw,
            Self::wake_by_ref_raw,
            Self::drop_raw,
        );

        /// Transfer one shared owner into a raw waker.
        fn raw(this: Arc<Self>) -> RawWaker {
            RawWaker::new(Arc::into_raw(this).cast(), &Self::VTABLE)
        }

        /// Clone the shared owner, pausing the first clone at the test barrier.
        unsafe fn clone_raw(data: *const ()) -> RawWaker {
            // SAFETY: each raw waker owns an Arc count. Borrow it without consuming
            // that count; clone creates exactly one count for the returned waker.
            let this = unsafe { &*data.cast::<Self>() };
            unsafe { Arc::increment_strong_count(data.cast::<Self>()) };
            if this.first_clone.swap(false, Ordering::Relaxed) {
                this.barrier.wait();
                this.barrier.wait();
            }
            RawWaker::new(data, &Self::VTABLE)
        }

        /// Count an owned wake and release its shared owner.
        unsafe fn wake_raw(data: *const ()) {
            // SAFETY: wake consumes the raw waker's owned Arc count exactly once.
            let this = unsafe { Arc::from_raw(data.cast::<Self>()) };
            this.wakes.fetch_add(1, Ordering::Relaxed);
        }

        /// Count a borrowed wake without releasing its shared owner.
        unsafe fn wake_by_ref_raw(data: *const ()) {
            // SAFETY: the calling waker keeps its Arc alive throughout this borrow.
            let this = unsafe { &*data.cast::<Self>() };
            this.wakes.fetch_add(1, Ordering::Relaxed);
        }

        /// Release the raw waker's shared owner without waking.
        unsafe fn drop_raw(data: *const ()) {
            // SAFETY: drop consumes the raw waker's owned Arc count exactly once.
            drop(unsafe { Arc::from_raw(data.cast::<Self>()) });
        }
    }

    /// Exercise both endpoints, closure and capacity changes, and registration timing.
    #[test]
    fn transitions_before_during_and_after_waker_registration_are_not_lost() {
        // Cover both sides and both transitions: publication/close for readable,
        // capacity/close for writable. No timing/sleep assumptions are needed.
        for writable in [false, true] {
            for close in [false, true] {
                for timing in 0..3 {
                    let (sender, mut receiver) = bounded(1).unwrap();
                    if writable {
                        assert!(sender.try_send(7).is_ok());
                    }
                    let barrier = Arc::new(Barrier::new(2));
                    let state = Arc::new(RegistrationWake {
                        barrier: barrier.clone(),
                        first_clone: AtomicBool::new(timing == 1),
                        wakes: AtomicUsize::new(0),
                    });
                    // SAFETY: raw owns an Arc and all callbacks implement the
                    // RawWaker ownership contract, with thread-safe shared state.
                    let waker = unsafe { Waker::from_raw(RegistrationWake::raw(state.clone())) };
                    let mut cx = Context::from_waker(&waker);

                    /// Normalize readable and writable checks for the timing matrix.
                    type Poller = Box<dyn FnMut(&mut Context<'_>) -> Poll<Result<()>>>;

                    /// Change peer state and retain its endpoint until checks finish.
                    type Action = Box<dyn FnOnce() -> Box<dyn Send> + Send>;
                    let (mut poll, action): (Poller, Action) = if writable {
                        (
                            Box::new(move |cx| sender.poll_ready(cx)),
                            Box::new(move || {
                                if close {
                                    drop(receiver);
                                    Box::new(())
                                } else {
                                    assert_eq!(receiver.receive().unwrap(), Some(7));
                                    // Keep the receiver alive until after readiness
                                    // was checked, so freeing capacity is not closure.
                                    Box::new(receiver)
                                }
                            }),
                        )
                    } else {
                        (
                            Box::new(move |cx| {
                                receiver.poll_receive(cx).map(|result| {
                                    result.map(|item| {
                                        assert_eq!(item, if close { None } else { Some(7) })
                                    })
                                })
                            }),
                            Box::new(move || {
                                if close {
                                    sender.close();
                                } else {
                                    assert!(sender.try_send(7).is_ok());
                                }
                                Box::new(sender)
                            }),
                        )
                    };
                    let sync = state.barrier.clone();
                    let worker = std::thread::spawn(move || {
                        sync.wait();
                        let endpoint = action();
                        sync.wait();
                        sync.wait();
                        drop(endpoint);
                    });
                    if timing == 0 {
                        state.barrier.wait();
                        state.barrier.wait();
                    }
                    let first = poll(&mut cx);
                    if timing == 2 {
                        assert!(first.is_pending());
                        state.barrier.wait();
                        state.barrier.wait();
                    }
                    let expected = if writable && close {
                        Err(Error::Unavailable)
                    } else {
                        Ok(())
                    };
                    if first.is_pending() {
                        assert!(
                            state.wakes.load(Ordering::Relaxed) > 0,
                            "pending transition must wake"
                        );
                        assert_eq!(poll(&mut cx), Poll::Ready(expected));
                    } else {
                        assert_eq!(first, Poll::Ready(expected));
                    }
                    state.barrier.wait();
                    worker.join().unwrap();
                }
            }
        }
    }

    /// Every orphan is removed before its destructor can recursively drain the rest.
    #[test]
    fn orphan_destructor_can_reenter_cleanup() {
        use std::rc::{Rc, Weak};

        /// Orphan payload that recursively requests cleanup when destroyed.
        struct Item(Weak<Sender<Item>>, Rc<Cell<usize>>);

        impl Drop for Item {
            /// Count this removal and request cleanup of any remaining orphans.
            fn drop(&mut self) {
                self.1.set(self.1.get() + 1);
                self.0.upgrade().unwrap().discard_closed();
            }
        }
        let (sender, receiver) = bounded(3).unwrap();
        let sender = Rc::new(sender);
        let count = Rc::new(Cell::new(0));
        for _ in 0..3 {
            assert!(
                sender
                    .try_send(Item(Rc::downgrade(&sender), count.clone()))
                    .is_ok()
            );
        }
        drop(receiver);
        assert!(sender.discard_closed());
        assert_eq!(count.get(), 3);
    }

    /// Reject empty and unrepresentable rings before attempting allocation.
    #[test]
    fn invalid_capacity_is_rejected_before_allocation() {
        assert!(matches!(bounded::<u8>(0), Err(Error::InvalidConfiguration)));
        assert!(matches!(
            bounded::<u8>(usize::MAX),
            Err(Error::InvalidConfiguration)
        ));
        assert!(matches!(
            bounded::<()>(usize::MAX),
            Err(Error::InvalidConfiguration)
        ));
    }

    /// Saturation returns the original payload and closure preserves queued values.
    #[test]
    fn saturation_returns_owner_and_close_drains() {
        let (sender, mut receiver) = bounded(1).unwrap();
        assert!(sender.try_send(String::from("first")).is_ok());
        let failed = sender.try_send(String::from("second")).err().unwrap();
        assert_eq!(failed.command, "second");
        assert_eq!(failed.error, Error::Overloaded);
        sender.close();
        assert_eq!(receiver.receive().unwrap().as_deref(), Some("first"));
        assert!(matches!(
            receiver.poll_receive(&mut Context::from_waker(futures::task::noop_waker_ref())),
            Poll::Ready(Ok(None))
        ));
    }

    /// Transfer a long FIFO stream between the unique producer and consumer threads.
    #[test]
    fn transfers_fifo_between_threads() {
        let (sender, mut receiver) = bounded(4).unwrap();
        let producer = std::thread::spawn(move || {
            for n in 0..10000 {
                let mut value = n;
                loop {
                    match sender.try_send(value) {
                        Ok(()) => break,
                        Err(error) => {
                            value = error.command;
                            std::thread::yield_now();
                        }
                    }
                }
            }
        });
        for n in 0..10000 {
            loop {
                if let Some(value) = receiver.receive().unwrap() {
                    assert_eq!(value, n);
                    break;
                }
                std::thread::yield_now();
            }
        }
        producer.join().unwrap();
    }

    /// Repeated full-ring wraps work without power-of-two capacity assumptions.
    #[test]
    fn non_power_of_two_capacity_wraps_without_overwriting() {
        let (sender, mut receiver) = bounded(3).unwrap();
        for round in 0..100 {
            for offset in 0..3 {
                assert!(sender.try_send(round * 3 + offset).is_ok());
            }
            assert!(sender.try_send(0).is_err());
            for offset in 0..3 {
                assert_eq!(receiver.receive().unwrap(), Some(round * 3 + offset));
            }
        }
    }

    /// Publication, consumption, and closure each notify their waiting endpoint.
    #[test]
    fn wakeups_cover_capacity_data_and_close() {
        use crate::test_util::WakeCounter;
        let (sender, mut receiver) = bounded(1).unwrap();
        let counter = Arc::new(WakeCounter::default());
        let waker = std::task::Waker::from(counter.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(receiver.poll_receive(&mut cx).is_pending());
        assert!(sender.try_send(7).is_ok());
        assert_eq!(counter.count(), 1);
        assert!(sender.poll_ready(&mut cx).is_pending());
        assert_eq!(receiver.receive().unwrap(), Some(7));
        assert_eq!(counter.count(), 2);
        assert!(receiver.poll_receive(&mut cx).is_pending());
        sender.close();
        assert_eq!(counter.count(), 3);
    }

    /// Unwinding an orphan destructor cannot leave its slot initialized twice.
    #[test]
    fn orphan_cleanup_advances_before_panicking_destructor() {
        use std::sync::atomic::AtomicUsize;

        /// Payload that panics on its first destruction attempt.
        struct Panics(Arc<AtomicUsize>);

        impl Drop for Panics {
            /// Count destruction and inject a failure on the first call.
            fn drop(&mut self) {
                if self.0.fetch_add(1, Ordering::Relaxed) == 0 {
                    panic!("injected destructor failure");
                }
            }
        }
        let count = Arc::new(AtomicUsize::new(0));
        let (sender, receiver) = bounded(1).unwrap();
        assert!(sender.try_send(Panics(count.clone())).is_ok());
        drop(receiver);
        assert!(
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| sender.discard_closed()))
                .is_err()
        );
        drop(sender);
        assert_eq!(
            count.load(Ordering::Relaxed),
            1,
            "orphan cannot be dropped twice"
        );
    }
}
