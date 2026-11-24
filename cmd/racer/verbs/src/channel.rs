//! Worker command queues and one-shot operation results.
//!
//! Each I/O worker gets one [`Worker`] handle. The node owner holds one
//! [`Owner`] that drains every worker's bounded command queue. Results go
//! back through per-operation one-shot cells ([`Op`] and [`Completer`]), so a
//! worker never drains results it did not ask for.
//!
//! Queues are [`futures::channel::mpsc`] channels and results are
//! [`futures::channel::oneshot`] channels. A futures mpsc channel holds
//! `buffer + senders` commands, and each worker has exactly one sender, so a
//! queue of `n` commands is built with `n - 1` buffer slots.
//!
//! The owner never polls a receiver as a stream: it drains one command at a
//! time with `try_recv` from its own pass, and an [`AtomicWaker`] shared by
//! every worker wakes it after a send or a worker drop. A worker that finds
//! its queue full waits on [`mpsc::Sender::poll_ready`]; futures wakes it
//! when the owner receives one of its commands.
//!
//! No `unsafe`. Wakers are called after the enclosing lock is released.
//!
//! Cancellation:
//! - Dropping the [`Owner`] closes every queue and drops queued commands.
//!   Commands own their [`Completer`]s, so their [`Op`]s resolve to
//!   [`Error::Canceled`]. Later sends fail with [`Error::Unavailable`].
//! - Dropping a [`Worker`] keeps its queued commands for the owner to drain.
//! - Dropping a [`Completer`] without completing resolves the [`Op`] to
//!   [`Error::Canceled`]. Dropping an [`Op`] makes [`Completer::complete`]
//!   hand the value back so the owner can clean it up.
//! - [`Op::cancel`] closes the receiver before taking a value, so a value
//!   sent while the op is being abandoned is either returned by `cancel` or
//!   handed back to [`Completer::complete`], never dropped unseen.

use std::{
    future::Future,
    pin::Pin,
    sync::{
        Arc, Mutex, MutexGuard, PoisonError,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll},
};

use futures::{
    channel::{mpsc, oneshot},
    task::AtomicWaker,
};

use crate::{Error, Result};

/// Lock ignoring poison. Critical sections here never panic midway.
fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

/// Queue depth shared by one worker and the owner.
struct Queue {
    /// Commands queued or in the middle of being sent. A send reserves a
    /// slot before the command is visible to the owner and releases it on
    /// receipt or on failure, so the count never understates the queue.
    queued: AtomicUsize,

    /// Commands the queue holds before a send fails.
    capacity: usize,

    /// The worker handle is alive.
    live: AtomicBool,
}

/// Create one owner and `workers` worker handles, each with a queue of
/// `capacity` commands.
///
/// Returns [`Error::InvalidConfiguration`] if `capacity` is zero.
pub(crate) fn channels<C>(workers: usize, capacity: usize) -> Result<(Vec<Worker<C>>, Owner<C>)> {
    if capacity == 0 {
        return Err(Error::InvalidConfiguration);
    }

    let waker = Arc::new(AtomicWaker::new());
    let mut handles = Vec::with_capacity(workers);
    let mut channels = Vec::with_capacity(workers);

    for _ in 0..workers {
        // Capacity is `buffer + senders`, and this worker is the only sender.
        let (sender, receiver) = mpsc::channel(capacity - 1);
        let queue = Arc::new(Queue {
            queued: AtomicUsize::new(0),
            capacity,
            live: AtomicBool::new(true),
        });
        handles.push(Worker {
            sender: Mutex::new(sender),
            queue: queue.clone(),
            owner: waker.clone(),
        });
        channels.push(Channel { receiver, queue });
    }

    Ok((
        handles,
        Owner {
            channels,
            waker,
            next: 0,
        },
    ))
}

/// One I/O worker's sending side.
pub(crate) struct Worker<C> {
    sender: Mutex<mpsc::Sender<C>>,
    queue: Arc<Queue>,
    /// Shared by every channel of one owner. Woken on new commands or a
    /// worker drop.
    owner: Arc<AtomicWaker>,
}

impl<C> Worker<C> {
    /// Queue `command` for the owner and wake it.
    ///
    /// On failure the command comes back with [`Error::Overloaded`] (queue
    /// full; call [`Worker::poll_ready`] to wait) or [`Error::Unavailable`]
    /// (owner gone).
    pub(crate) fn try_send(&self, command: C) -> std::result::Result<(), (Error, C)> {
        // Reserve the slot before the command can be seen by the owner.
        self.queue.queued.fetch_add(1, Ordering::AcqRel);

        let sent = {
            let mut sender = lock(&self.sender);
            sender.try_send(command)
        };

        match sent {
            Ok(()) => {
                self.owner.wake();
                Ok(())
            }
            Err(error) => {
                self.queue.queued.fetch_sub(1, Ordering::AcqRel);
                let kind = if error.is_full() {
                    Error::Overloaded
                } else {
                    Error::Unavailable
                };
                Err((kind, error.into_inner()))
            }
        }
    }

    /// Whether the queue is at capacity.
    pub(crate) fn is_full(&self) -> bool {
        self.queue.queued.load(Ordering::Acquire) >= self.queue.capacity
    }

    /// Whether the queue holds commands the owner has not drained.
    pub(crate) fn is_pending(&self) -> bool {
        self.queue.queued.load(Ordering::Acquire) > 0
    }

    /// Ready once the queue has space. Fails with [`Error::Unavailable`]
    /// once the owner is gone.
    pub(crate) fn poll_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        let mut sender = lock(&self.sender);

        match sender.poll_ready(cx) {
            Poll::Ready(Ok(())) => Poll::Ready(Ok(())),
            Poll::Ready(Err(_)) => Poll::Ready(Err(Error::Unavailable)),
            Poll::Pending => Poll::Pending,
        }
    }
}

impl<C> Drop for Worker<C> {
    /// Tell the owner no more commands come from this worker.
    fn drop(&mut self) {
        self.queue.live.store(false, Ordering::Release);
        self.owner.wake();
    }
}

/// The node owner's receiving side for one worker.
struct Channel<C> {
    receiver: mpsc::Receiver<C>,
    queue: Arc<Queue>,
}

/// The node owner's receiving side for every worker.
pub(crate) struct Owner<C> {
    channels: Vec<Channel<C>>,
    waker: Arc<AtomicWaker>,
    /// Worker to start the next drain at, for fairness.
    next: usize,
}

impl<C> Owner<C> {
    /// Hand up to `budget` queued commands to `handle` as
    /// `(worker index, command)`. Commands from one worker arrive in send
    /// order. Workers are visited round-robin, one command at a time.
    ///
    /// Returns `Ready(n)` with `n > 0` if it handled anything, otherwise
    /// registers `cx` and returns `Pending`. Workers waiting for space are
    /// woken.
    #[cfg(test)]
    pub(crate) fn poll_drain(
        &mut self,
        cx: &mut Context<'_>,
        budget: usize,
        mut handle: impl FnMut(usize, C),
    ) -> Poll<usize> {
        let handled = self.drain(budget, &mut handle);

        if handled > 0 || budget == 0 {
            return Poll::Ready(handled);
        }

        // Register, then check again so a send in between is not missed.
        self.waker.register(cx.waker());

        match self.drain(budget, &mut handle) {
            0 => Poll::Pending,
            handled => Poll::Ready(handled),
        }
    }

    /// Register `waker` for new commands and worker drops.
    pub(crate) fn register(&self, waker: &std::task::Waker) {
        self.waker.register(waker);
    }

    /// Hand up to `budget` queued commands to `handle` without registering a
    /// waker. Returns how many commands were handled.
    pub(crate) fn try_drain(&mut self, budget: usize, mut handle: impl FnMut(usize, C)) -> usize {
        self.drain(budget, &mut handle)
    }

    fn drain(&mut self, budget: usize, handle: &mut impl FnMut(usize, C)) -> usize {
        let count = self.channels.len();
        let mut handled = 0;

        while handled < budget {
            let mut progressed = false;
            let start = self.next;

            for step in 0..count {
                if handled >= budget {
                    break;
                }

                let index = (start + step) % count;
                let channel = &mut self.channels[index];

                // Receiving unparks a sender waiting on `poll_ready`.
                if let Ok(command) = channel.receiver.try_recv() {
                    channel.queue.queued.fetch_sub(1, Ordering::AcqRel);
                    handle(index, command);
                    handled += 1;
                    progressed = true;
                    // Resume after the last serviced worker, even if its
                    // queue still has commands, so the budget rotates fairly
                    // across more workers than fit in one drain.
                    self.next = (index + 1) % count;
                }
            }

            if !progressed {
                break;
            }
        }

        handled
    }

    /// True once worker `index` is dropped and its queue is empty.
    #[cfg(test)]
    pub(crate) fn is_closed(&self, index: usize) -> bool {
        self.channels.get(index).is_none_or(|channel| {
            !channel.queue.live.load(Ordering::Acquire)
                && channel.queue.queued.load(Ordering::Acquire) == 0
        })
    }
}

impl<C> Drop for Owner<C> {
    /// Close every queue, drop queued commands, and wake waiting workers.
    fn drop(&mut self) {
        // Closing first makes every later send fail before any command runs
        // its completion.
        for channel in &mut self.channels {
            channel.receiver.close();
        }

        for channel in &mut self.channels {
            while let Ok(command) = channel.receiver.try_recv() {
                channel.queue.queued.fetch_sub(1, Ordering::AcqRel);
                // Dropped outside any lock: commands may complete ops and
                // wake.
                drop(command);
            }
        }
    }
}

/// Create a pending operation and the handle that completes it.
pub(crate) fn op<T>() -> (Op<T>, Completer<T>) {
    let (sender, receiver) = oneshot::channel::<Result<T>>();

    (Op(Mutex::new(Some(receiver))), Completer(Some(sender)))
}

/// One-shot result of an owner operation.
///
/// Wraps a [`oneshot::Receiver`] because the public API polls it through
/// `&self`.
pub struct Op<T>(Mutex<Option<oneshot::Receiver<Result<T>>>>);

impl<T> Op<T> {
    /// The result, once. Polling after `Ready` returns
    /// [`Error::InvalidRequest`].
    pub fn poll(&self, cx: &mut Context<'_>) -> Poll<Result<T>> {
        let mut receiver = lock(&self.0);
        let Some(inner) = receiver.as_mut() else {
            return Poll::Ready(Err(Error::InvalidRequest));
        };

        match Pin::new(inner).poll(cx) {
            Poll::Ready(Ok(result)) => {
                *receiver = None;
                Poll::Ready(result)
            }
            Poll::Ready(Err(oneshot::Canceled)) => {
                *receiver = None;
                Poll::Ready(Err(Error::Canceled))
            }
            Poll::Pending => Poll::Pending,
        }
    }

    /// Abandon the op and return its result if it completed but was not
    /// read. Atomic with respect to the owner's completion: a result is
    /// either returned here or handed back to the [`Completer`], never lost.
    pub fn cancel(self) -> Option<Result<T>> {
        let mut receiver = self
            .0
            .into_inner()
            .unwrap_or_else(PoisonError::into_inner)?;

        // The completer may have already resolved the op.
        match receiver.try_recv() {
            Ok(Some(result)) => return Some(result),
            Err(oneshot::Canceled) => return Some(Err(Error::Canceled)),
            Ok(None) => {}
        }

        // Fail a later completion, then pick up a value that was sent while
        // closing. `Receiver::close` marks the channel complete, so
        // `Sender::send` either fails and returns the value to the owner or
        // stored it before the close, where `try_recv` takes it back.
        receiver.close();

        match receiver.try_recv() {
            Ok(Some(result)) => Some(result),
            Ok(None) | Err(oneshot::Canceled) => None,
        }
    }
}

impl<T> std::fmt::Debug for Op<T> {
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.write_str("Op(..)")
    }
}

/// The owner's side of an [`Op`]. Dropping it uncompleted cancels the op.
pub(crate) struct Completer<T>(Option<oneshot::Sender<Result<T>>>);

impl<T> Completer<T> {
    /// True once the [`Op`] is dropped or canceled.
    pub(crate) fn is_abandoned(&self) -> bool {
        self.0.as_ref().is_none_or(oneshot::Sender::is_canceled)
    }

    /// Resolve the op and wake its poller. If the [`Op`] is gone the value
    /// comes back as `Err` so the caller can clean it up.
    pub(crate) fn complete(mut self, result: Result<T>) -> std::result::Result<(), Result<T>> {
        let sender = self.0.take().expect("completer used once");

        sender.send(result)
    }
}

#[cfg(test)]
mod tests {
    use std::{
        sync::atomic::{AtomicUsize, Ordering},
        task::{Wake, Waker},
        thread,
    };

    use super::*;

    #[derive(Default)]
    struct Count(AtomicUsize);

    impl Wake for Count {
        fn wake(self: Arc<Self>) {
            self.0.fetch_add(1, Ordering::SeqCst);
        }
    }

    fn counter() -> (Arc<Count>, Waker) {
        let count = Arc::new(Count::default());
        (count.clone(), Waker::from(count.clone()))
    }

    fn wakes(count: &Count) -> usize {
        count.0.load(Ordering::SeqCst)
    }

    fn drain_all<C>(owner: &mut Owner<C>, budget: usize) -> Vec<(usize, C)> {
        let (_, waker) = counter();
        let mut cx = Context::from_waker(&waker);
        let mut out = Vec::new();

        let _ = owner.poll_drain(&mut cx, budget, |index, command| out.push((index, command)));
        out
    }

    #[test]
    fn drain_resumes_after_last_serviced_worker() {
        let workers_count = 128;
        let budget = 48;
        let (workers, mut owner) = channels::<u32>(workers_count, 4).unwrap();

        for worker in &workers {
            assert!(worker.try_send(0).is_ok());
            assert!(worker.try_send(1).is_ok());
        }

        let mut seen = vec![false; workers_count];

        for _ in 0..workers_count.div_ceil(budget) {
            let out = drain_all(&mut owner, budget);
            assert_eq!(out.len(), budget);

            for (index, _) in out {
                seen[index] = true;
            }
        }

        assert!(seen.iter().all(|&serviced| serviced));
    }

    #[test]
    fn rejects_zero_capacity() {
        assert!(matches!(
            channels::<u32>(1, 0),
            Err(Error::InvalidConfiguration)
        ));
    }

    #[test]
    fn sends_and_completes() {
        let (workers, mut owner) = channels::<Completer<u32>>(1, 4).unwrap();
        let (op, completer) = op();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);

        assert_eq!(owner.channels.len(), 1);
        assert!(op.poll(&mut cx).is_pending());
        assert!(workers[0].try_send(completer).is_ok());

        for (index, completer) in drain_all(&mut owner, 8) {
            assert_eq!(index, 0);
            assert!(!completer.is_abandoned());
            assert!(completer.complete(Ok(7)).is_ok());
        }

        assert_eq!(wakes(&count), 1);
        assert_eq!(op.poll(&mut cx), Poll::Ready(Ok(7)));
        assert_eq!(op.poll(&mut cx), Poll::Ready(Err(Error::InvalidRequest)));
    }

    #[test]
    fn full_queue_applies_backpressure() {
        let (workers, mut owner) = channels::<u32>(1, 2).unwrap();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);

        assert!(workers[0].try_send(1).is_ok());
        assert!(workers[0].try_send(2).is_ok());
        assert_eq!(workers[0].try_send(3), Err((Error::Overloaded, 3)));
        assert!(workers[0].poll_ready(&mut cx).is_pending());

        assert_eq!(drain_all(&mut owner, 1), vec![(0, 1)]);
        assert_eq!(wakes(&count), 1);
        assert_eq!(workers[0].poll_ready(&mut cx), Poll::Ready(Ok(())));
        assert!(workers[0].try_send(3).is_ok());
        assert_eq!(drain_all(&mut owner, 8), vec![(0, 2), (0, 3)]);
    }

    #[test]
    fn capacity_is_exactly_the_requested_bound() {
        let (workers, _owner) = channels::<u32>(1, 3).unwrap();

        for sequence in 0..3 {
            assert!(workers[0].try_send(sequence).is_ok());
        }

        assert!(workers[0].is_full());
        assert_eq!(workers[0].try_send(3), Err((Error::Overloaded, 3)));
    }

    #[test]
    fn a_one_slot_queue_holds_one_command() {
        let (workers, mut owner) = channels::<u32>(1, 1).unwrap();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);

        assert!(workers[0].try_send(1).is_ok());
        assert_eq!(workers[0].try_send(2), Err((Error::Overloaded, 2)));
        assert!(workers[0].poll_ready(&mut cx).is_pending());

        assert_eq!(drain_all(&mut owner, 8), vec![(0, 1)]);
        assert_eq!(wakes(&count), 1);
        assert_eq!(workers[0].poll_ready(&mut cx), Poll::Ready(Ok(())));
    }

    #[test]
    fn owner_drop_cancels_queued_and_rejects_new() {
        let (workers, owner) = channels::<Completer<u32>>(1, 4).unwrap();
        let (op, completer) = op();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);
        let marker = Arc::new(());

        let _ = op.poll(&mut cx);
        assert!(workers[0].try_send(completer).is_ok());
        drop(owner);

        assert_eq!(op.poll(&mut cx), Poll::Ready(Err(Error::Canceled)));
        assert_eq!(
            workers[0].poll_ready(&mut cx),
            Poll::Ready(Err(Error::Unavailable))
        );
        assert!(wakes(&count) >= 1);

        let (_, completer) = super::op::<Arc<()>>();
        drop(completer);

        let (workers2, owner2) = channels::<Arc<()>>(1, 1).unwrap();
        drop(owner2);
        let Err((error, back)) = workers2[0].try_send(marker.clone()) else {
            panic!("send after owner drop");
        };
        assert_eq!(error, Error::Unavailable);
        drop(back);
        assert_eq!(Arc::strong_count(&marker), 1);
    }

    #[test]
    fn owner_drop_wakes_blocked_worker() {
        let (workers, owner) = channels::<u32>(1, 1).unwrap();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);

        assert!(workers[0].try_send(1).is_ok());
        assert!(workers[0].poll_ready(&mut cx).is_pending());
        drop(owner);
        assert_eq!(wakes(&count), 1);
    }

    #[test]
    fn worker_drop_keeps_queue_then_closes() {
        let (mut workers, mut owner) = channels::<u32>(2, 4).unwrap();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);

        assert!(owner.poll_drain(&mut cx, 8, |_, _| {}).is_pending());
        drop(workers.remove(0));
        assert_eq!(wakes(&count), 1);
        assert!(owner.is_closed(0));

        assert!(owner.poll_drain(&mut cx, 8, |_, _| {}).is_pending());
        assert!(workers[0].try_send(9).is_ok());
        assert_eq!(wakes(&count), 2);
        drop(workers.remove(0));
        assert!(!owner.is_closed(1));
        assert_eq!(drain_all(&mut owner, 8), vec![(1, 9)]);
        assert!(owner.is_closed(1));
        assert!(owner.is_closed(5));
    }

    #[test]
    fn completer_drop_cancels() {
        let (op, completer) = op::<u32>();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);

        assert!(op.poll(&mut cx).is_pending());
        drop(completer);
        assert_eq!(wakes(&count), 1);
        assert_eq!(op.poll(&mut cx), Poll::Ready(Err(Error::Canceled)));
    }

    #[test]
    fn cancel_returns_an_unread_result() {
        let (op, completer) = op::<u32>();

        assert!(completer.complete(Ok(7)).is_ok());
        assert_eq!(op.cancel(), Some(Ok(7)));
    }

    #[test]
    fn cancel_before_completion_returns_value_to_owner() {
        let (op, completer) = op::<u32>();

        assert_eq!(op.cancel(), None);
        assert!(completer.is_abandoned());
        assert_eq!(completer.complete(Ok(7)), Err(Ok(7)));
    }

    #[test]
    fn cancel_after_completer_drop_reports_canceled() {
        let (op, completer) = op::<u32>();

        drop(completer);
        assert_eq!(op.cancel(), Some(Err(Error::Canceled)));
    }

    #[test]
    fn op_drop_returns_value_to_owner() {
        let marker = Arc::new(());
        let (op, completer) = op::<Arc<()>>();

        drop(op);
        assert!(completer.is_abandoned());
        let Err(Ok(back)) = completer.complete(Ok(marker.clone())) else {
            panic!("value must come back");
        };
        drop(back);
        assert_eq!(Arc::strong_count(&marker), 1);
    }

    #[test]
    fn op_drop_frees_unread_result() {
        let marker = Arc::new(());
        let (op, completer) = op::<Arc<()>>();

        assert!(completer.complete(Ok(marker.clone())).is_ok());
        drop(op);
        assert_eq!(Arc::strong_count(&marker), 1);
    }

    #[test]
    fn multiple_workers_round_robin_in_order() {
        let (workers, mut owner) = channels::<(usize, u32)>(3, 4).unwrap();

        for (index, worker) in workers.iter().enumerate() {
            for sequence in 0..3 {
                assert!(worker.try_send((index, sequence)).is_ok());
            }
        }

        let first = drain_all(&mut owner, 3);
        let mut seen: Vec<_> = first.iter().map(|(index, _)| *index).collect();
        seen.sort_unstable();
        assert_eq!(seen, vec![0, 1, 2]);

        let mut all = first;
        all.extend(drain_all(&mut owner, 100));
        assert_eq!(all.len(), 9);

        for index in 0..3 {
            let order: Vec<_> = all
                .iter()
                .filter(|(from, _)| *from == index)
                .map(|(from, (sender, sequence))| {
                    assert_eq!(from, sender);
                    *sequence
                })
                .collect();
            assert_eq!(order, vec![0, 1, 2]);
        }
    }

    #[test]
    fn zero_budget_is_ready_without_work() {
        let (workers, mut owner) = channels::<u32>(1, 1).unwrap();

        assert!(workers[0].try_send(1).is_ok());
        assert_eq!(drain_all(&mut owner, 0), Vec::new());
        assert_eq!(drain_all(&mut owner, 1), vec![(0, 1)]);
    }

    /// Threads race sends, drains, completions, and drops. Every op must
    /// resolve exactly once and nothing may leak.
    #[test]
    fn stress_threads() {
        const WORKERS: usize = 4;
        const OPS: usize = if cfg!(miri) { 20 } else { 2_000 };

        let marker = Arc::new(());
        let (workers, mut owner) = channels::<(Completer<Arc<()>>, usize)>(WORKERS, 3).unwrap();
        let owner_marker = marker.clone();
        let owner_thread = thread::spawn(move || {
            let (_, waker) = counter();
            let mut cx = Context::from_waker(&waker);
            let mut done = 0;

            while (0..WORKERS).any(|index| !owner.is_closed(index)) {
                let _ = owner.poll_drain(&mut cx, 5, |_, (completer, sequence)| {
                    // Cancel some ops by dropping the completer.
                    if sequence % 7 != 0 {
                        let _ = completer.complete(Ok(owner_marker.clone()));
                    }
                    done += 1;
                });
                thread::yield_now();
            }

            done
        });

        let senders: Vec<_> = workers
            .into_iter()
            .map(|worker| {
                thread::spawn(move || {
                    let (_, waker) = counter();
                    let mut cx = Context::from_waker(&waker);
                    let mut outcomes = (0, 0);

                    for sequence in 0..OPS {
                        let (op, completer) = op();
                        let mut command = (completer, sequence);

                        loop {
                            match worker.try_send(command) {
                                Ok(()) => break,
                                Err((Error::Overloaded, back)) => {
                                    command = back;
                                    thread::yield_now();
                                }
                                Err((error, _)) => panic!("unexpected {error}"),
                            }
                        }

                        // Abandon some ops; poll the rest to completion.
                        if sequence % 5 == 0 {
                            continue;
                        }

                        loop {
                            match op.poll(&mut cx) {
                                Poll::Pending => thread::yield_now(),
                                Poll::Ready(Ok(_)) => {
                                    outcomes.0 += 1;
                                    break;
                                }
                                Poll::Ready(Err(Error::Canceled)) => {
                                    outcomes.1 += 1;
                                    break;
                                }
                                Poll::Ready(Err(error)) => panic!("unexpected {error}"),
                            }
                        }
                    }

                    outcomes
                })
            })
            .collect();

        let polled: usize = senders
            .into_iter()
            .map(|sender| {
                let (ok, canceled) = sender.join().unwrap();
                ok + canceled
            })
            .sum();

        assert_eq!(owner_thread.join().unwrap(), WORKERS * OPS);
        assert_eq!(polled, WORKERS * (OPS - OPS.div_ceil(5)));
        assert_eq!(Arc::strong_count(&marker), 1);
    }
}
