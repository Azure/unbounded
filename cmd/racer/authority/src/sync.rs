//! Waiting primitives for control tasks: a broadcast [`Signal`] and a
//! counting semaphore [`Slots`] with a bounded FIFO wait.
//!
//! Both are single-threaded and independent of the reactor except that
//! [`Slots::acquire`] waits on a reactor deadline.

use crate::rt::deadline;
use std::{
    cell::{Cell, RefCell},
    future::Future,
    rc::Rc,
    task::{Poll, Waker},
    time::Instant,
};

/// Edge-free broadcast: waiters wake on every [`Signal::notify`].
///
/// Each [`Signal::changed`] future owns one entry in a slab of wakers, so
/// registering, re-registering, and dropping a waiter are O(1) and a notify
/// with W waiters is O(W).
#[derive(Clone, Default)]
pub struct Signal(Rc<SignalInner>);

#[derive(Default)]
struct SignalInner {
    generation: Cell<u64>,
    wakers: RefCell<WakerSlab>,
}

#[derive(Default)]
struct WakerSlab {
    entries: Vec<Option<Waker>>,
    free: Vec<usize>,
    live: usize,
}

impl WakerSlab {
    fn insert(&mut self, waker: Waker) -> usize {
        self.live += 1;
        match self.free.pop() {
            Some(i) => {
                self.entries[i] = Some(waker);
                i
            }
            None => {
                self.entries.push(Some(waker));
                self.entries.len() - 1
            }
        }
    }

    fn remove(&mut self, i: usize) {
        if let Some(e) = self.entries.get_mut(i)
            && e.take().is_some()
        {
            self.live -= 1;
            self.free.push(i);
        }
    }
}

impl Signal {
    /// Wake all current waiters.
    pub fn notify(&self) {
        self.0.generation.set(self.0.generation.get() + 1);
        // Take the slab first: a waker may run code that registers again.
        let slab = std::mem::take(&mut *self.0.wakers.borrow_mut());
        for w in slab.entries.into_iter().flatten() {
            w.wake();
        }
    }

    /// Current generation, to pass to [`Signal::changed`].
    pub fn generation(&self) -> u64 {
        self.0.generation.get()
    }

    /// Wakers registered and not yet woken or dropped.
    #[cfg(test)]
    pub fn waiters(&self) -> usize {
        self.0.wakers.borrow().live
    }

    /// Slab entries, live or free.
    #[cfg(test)]
    pub fn slab_len(&self) -> usize {
        self.0.wakers.borrow().entries.len()
    }

    /// Wait until the generation differs from `seen`.
    pub fn changed(&self, seen: u64) -> Changed<'_> {
        Changed {
            signal: self,
            seen,
            slot: None,
        }
    }
}

/// Future of [`Signal::changed`].
pub struct Changed<'a> {
    signal: &'a Signal,
    seen: u64,
    /// Slab index, valid while the generation still equals `seen` (a
    /// notify empties the slab and bumps the generation together).
    slot: Option<usize>,
}

impl Future for Changed<'_> {
    type Output = ();

    fn poll(mut self: std::pin::Pin<&mut Self>, cx: &mut std::task::Context<'_>) -> Poll<()> {
        let signal = self.signal;
        let inner = &signal.0;
        if inner.generation.get() != self.seen {
            self.slot = None;
            return Poll::Ready(());
        }
        let mut slab = inner.wakers.borrow_mut();
        match self.slot {
            Some(i) => {
                if let Some(Some(w)) = slab.entries.get_mut(i)
                    && !w.will_wake(cx.waker())
                {
                    w.clone_from(cx.waker());
                }
            }
            None => {
                let i = slab.insert(cx.waker().clone());
                drop(slab);
                self.slot = Some(i);
            }
        }
        Poll::Pending
    }
}

impl Drop for Changed<'_> {
    fn drop(&mut self) {
        if let Some(i) = self.slot
            && self.signal.0.generation.get() == self.seen
        {
            self.signal.0.wakers.borrow_mut().remove(i);
        }
    }
}

/// A counting semaphore with an optional bounded FIFO wait.
/// [`Slots::try_acquire`] fails when full; [`Slots::acquire`] queues until a
/// deadline. The rules are [`crate::rules::admission::WriteQueue`], shared
/// with the simulator.
#[derive(Clone)]
pub struct Slots(Rc<RefCell<SlotQueue>>);

type SlotQueue = crate::rules::admission::WriteQueue<Rc<SlotWaiter>>;

/// A queued [`Slots::acquire`].
#[derive(Default)]
pub struct SlotWaiter {
    /// The slot was handed to this waiter.
    granted: Cell<bool>,
    /// The waiter gave up; skip it.
    gone: Cell<bool>,
    waker: RefCell<Option<Waker>>,
}

fn waiting(w: &Rc<SlotWaiter>) -> bool {
    !w.gone.get() && !w.granted.get()
}

/// A held slot.
pub struct Slot(Rc<RefCell<SlotQueue>>);

fn release(queue: &Rc<RefCell<SlotQueue>>) {
    let next = queue.borrow_mut().release(waiting);
    if let Some(w) = next {
        w.granted.set(true);
        if let Some(waker) = w.waker.borrow_mut().take() {
            waker.wake();
        }
    }
}

impl Drop for Slot {
    fn drop(&mut self) {
        release(&self.0);
    }
}

impl Slots {
    /// `capacity` concurrent holders; nobody may wait.
    pub fn new(capacity: usize) -> Self {
        Self::queued(capacity, 0)
    }

    /// `capacity` concurrent holders and up to `max_waiting` queued
    /// [`Slots::acquire`] calls.
    pub fn queued(capacity: usize, max_waiting: usize) -> Self {
        Self(Rc::new(RefCell::new(SlotQueue::new(capacity, max_waiting))))
    }

    /// Take a slot now. Fails when full or when others are queued.
    pub fn try_acquire(&self) -> Option<Slot> {
        self.0
            .borrow_mut()
            .try_take(waiting)
            .then(|| Slot(self.0.clone()))
    }

    /// Slots held.
    #[cfg(test)]
    pub fn used(&self) -> usize {
        self.0.borrow().used
    }

    /// Take a slot, waiting in FIFO order until `at`. `None` when the queue
    /// is full or the deadline passes first.
    pub async fn acquire(&self, at: Instant) -> Option<Slot> {
        let waiter = Rc::new(SlotWaiter::default());
        let admit = self.0.borrow_mut().admit(waiter.clone(), waiting);
        match admit {
            crate::rules::admission::Admit::Take => return Some(Slot(self.0.clone())),
            crate::rules::admission::Admit::Full => return None,
            crate::rules::admission::Admit::Wait => {}
        }
        // From here the waiter is queued: `Pending` marks it gone on any
        // exit, and hands a granted but unclaimed slot to the next waiter.
        let pending = Pending {
            queue: self.0.clone(),
            waiter,
        };
        let granted = std::future::poll_fn(|cx| {
            if pending.waiter.granted.get() {
                return Poll::Ready(());
            }
            *pending.waiter.waker.borrow_mut() = Some(cx.waker().clone());
            Poll::Pending
        });
        if deadline(at, granted).await.is_err() && !pending.waiter.granted.get() {
            return None;
        }
        pending.waiter.gone.set(true);
        Some(Slot(self.0.clone()))
    }

    /// Queued entries, including ones that gave up and are not yet trimmed.
    #[cfg(test)]
    pub fn waiting(&self) -> usize {
        self.0.borrow().waiting()
    }
}

struct Pending {
    queue: Rc<RefCell<SlotQueue>>,
    waiter: Rc<SlotWaiter>,
}

impl Drop for Pending {
    fn drop(&mut self) {
        if self.waiter.gone.replace(true) {
            // Claimed: the slot moved into a `Slot`.
            return;
        }
        if self.waiter.granted.get() {
            // Granted but never claimed: pass it on.
            release(&self.queue);
        }
    }
}
