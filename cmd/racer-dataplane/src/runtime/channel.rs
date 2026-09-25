//! Bounded SPSC ownership handoffs, without locks or blocking sends.
use crate::error::{Error, Result};
use futures::task::AtomicWaker;
use std::{
    cell::{Cell, UnsafeCell},
    marker::PhantomData,
    mem::MaybeUninit,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
};
#[repr(align(64))]
struct Cursor(AtomicUsize);
struct Shared<T> {
    slots: Box<[UnsafeCell<MaybeUninit<T>>]>,
    head: Cursor,
    tail: Cursor,
    sender_closed: AtomicBool,
    receiver_closed: AtomicBool,
    readable: AtomicWaker,
    writable: AtomicWaker,
}
fn advance(cursor: usize, capacity: usize) -> usize {
    if cursor + 1 == capacity * 2 {
        0
    } else {
        cursor + 1
    }
}
fn distance(tail: usize, head: usize, capacity: usize) -> usize {
    if tail >= head {
        tail - head
    } else {
        capacity * 2 - head + tail
    }
}
// Only the unique non-Sync sender writes unpublished slots, and only the unique
// non-Sync receiver reads published slots. Release/acquire cursors transfer ownership.
unsafe impl<T: Send> Sync for Shared<T> {}
unsafe impl<T: Send> Send for Shared<T> {}
impl<T> Drop for Shared<T> {
    fn drop(&mut self) {
        let mut head = *self.head.0.get_mut();
        let tail = *self.tail.0.get_mut();
        while head != tail {
            unsafe {
                self.slots[head % self.slots.len()]
                    .get_mut()
                    .assume_init_drop();
            }
            head = advance(head, self.slots.len());
        }
    }
}
#[repr(align(64))]
pub struct Sender<T> {
    shared: Arc<Shared<T>>,
    local: PhantomData<Cell<()>>,
}
#[repr(align(64))]
pub struct Receiver<T> {
    shared: Arc<Shared<T>>,
    local: PhantomData<Cell<()>>,
}
pub struct SendFailure<T> {
    pub command: T,
    pub error: Error,
}
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
            local: PhantomData,
        },
        Receiver {
            shared,
            local: PhantomData,
        },
    ))
}
impl<T> Sender<T> {
    /// Reclaim orphaned messages only after the sole receiver has been destroyed.
    /// The sender is non-Sync, so no concurrent producer can publish during this.
    pub fn discard_closed(&self) -> bool {
        if !self.shared.receiver_closed.load(Ordering::Acquire) {
            return false;
        }
        let mut head = self.shared.head.0.load(Ordering::Relaxed);
        let tail = self.shared.tail.0.load(Ordering::Acquire);
        while head != tail {
            unsafe {
                (*self.shared.slots[head % self.shared.slots.len()].get()).assume_init_drop();
            }
            head = advance(head, self.shared.slots.len());
            self.shared.head.0.store(head, Ordering::Release);
        }
        true
    }
    pub fn try_send(&self, command: T) -> std::result::Result<(), SendFailure<T>> {
        if self.shared.sender_closed.load(Ordering::Acquire)
            || self.shared.receiver_closed.load(Ordering::Acquire)
        {
            return Err(SendFailure {
                command,
                error: Error::Unavailable,
            });
        }
        let tail = self.shared.tail.0.load(Ordering::Relaxed);
        let head = self.shared.head.0.load(Ordering::Acquire);
        if distance(tail, head, self.shared.slots.len()) == self.shared.slots.len() {
            return Err(SendFailure {
                command,
                error: Error::Overloaded,
            });
        }
        unsafe {
            (*self.shared.slots[tail % self.shared.slots.len()].get()).write(command);
        }
        self.shared
            .tail
            .0
            .store(advance(tail, self.shared.slots.len()), Ordering::Release);
        self.shared.readable.wake();
        Ok(())
    }
    pub fn close(&self) {
        self.shared.sender_closed.store(true, Ordering::Release);
        self.shared.readable.wake();
        self.shared.writable.wake();
    }
    pub fn poll_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        self.shared.writable.register(cx.waker());
        if self.shared.sender_closed.load(Ordering::Acquire)
            || self.shared.receiver_closed.load(Ordering::Acquire)
        {
            return Poll::Ready(Err(Error::Unavailable));
        }
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
    fn drop(&mut self) {
        self.close();
    }
}
impl<T> Receiver<T> {
    pub fn is_closed(&self) -> bool {
        self.shared.sender_closed.load(Ordering::Acquire)
    }
    pub fn register(&self, waker: &std::task::Waker) {
        self.shared.readable.register(waker);
    }
    pub fn receive(&mut self) -> Result<Option<T>> {
        let head = self.shared.head.0.load(Ordering::Relaxed);
        if head == self.shared.tail.0.load(Ordering::Acquire) {
            return Ok(None);
        }
        let item = unsafe {
            (*self.shared.slots[head % self.shared.slots.len()].get()).assume_init_read()
        };
        self.shared
            .head
            .0
            .store(advance(head, self.shared.slots.len()), Ordering::Release);
        self.shared.writable.wake();
        Ok(Some(item))
    }
    pub fn poll_receive(&mut self, cx: &mut Context<'_>) -> Poll<Result<Option<T>>> {
        self.shared.readable.register(cx.waker());
        match self.receive() {
            Ok(None) if !self.shared.sender_closed.load(Ordering::Acquire) => Poll::Pending,
            Ok(None) => Poll::Ready(self.receive()),
            result => Poll::Ready(result),
        }
    }
}
impl<T> Drop for Receiver<T> {
    fn drop(&mut self) {
        self.shared.receiver_closed.store(true, Ordering::Release);
        self.shared.writable.wake();
    }
}
#[cfg(test)]
mod tests {
    use super::*;
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
    #[test]
    fn wakeups_cover_capacity_data_and_close() {
        use std::{sync::atomic::AtomicUsize, task::Wake};
        struct Counter(AtomicUsize);
        impl Wake for Counter {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let (sender, mut receiver) = bounded(1).unwrap();
        let counter = Arc::new(Counter(AtomicUsize::new(0)));
        let waker = std::task::Waker::from(counter.clone());
        let mut cx = Context::from_waker(&waker);
        assert!(receiver.poll_receive(&mut cx).is_pending());
        assert!(sender.try_send(7).is_ok());
        assert_eq!(counter.0.load(Ordering::Relaxed), 1);
        assert!(sender.poll_ready(&mut cx).is_pending());
        assert_eq!(receiver.receive().unwrap(), Some(7));
        assert_eq!(counter.0.load(Ordering::Relaxed), 2);
        assert!(receiver.poll_receive(&mut cx).is_pending());
        sender.close();
        assert_eq!(counter.0.load(Ordering::Relaxed), 3);
    }
}
