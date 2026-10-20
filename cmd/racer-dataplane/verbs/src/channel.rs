//! Worker command channels and one-shot operation results.
//!
//! Each I/O worker gets one [`Worker`] handle. The node owner holds one
//! [`Owner`] that drains every worker's bounded command queue. Results go
//! back through per-operation one-shot cells ([`Op`] and [`Completer`]), so a
//! worker never drains results it did not ask for.
//!
//! No `unsafe`: queues and cells are `Mutex`es held only for short,
//! non-panicking critical sections, and wakes use `AtomicWaker` like the rest
//! of the crate. Wakers are always called after the lock is released.
//!
//! Cancellation:
//! - Dropping the [`Owner`] closes every queue and drops queued commands.
//!   Commands own their [`Completer`]s, so their [`Op`]s resolve to
//!   [`Error::Cancelled`]. Later sends fail with [`Error::Unavailable`].
//! - Dropping a [`Worker`] keeps its queued commands for the owner, then the
//!   owner sees the worker as gone through [`Owner::is_closed`].
//! - Dropping a [`Completer`] without completing resolves the [`Op`] to
//!   [`Error::Cancelled`]. Dropping an [`Op`] makes [`Completer::complete`]
//!   hand the value back so the owner can clean it up.

// Later stages (`Client` and `Owner` in `lib.rs`) use this module. Until
// then only the tests do.
#![cfg_attr(not(test), allow(dead_code))]

use std::{
    collections::VecDeque,
    sync::{Arc, Mutex, MutexGuard, PoisonError},
    task::{Context, Poll},
};

use futures::task::AtomicWaker;

use crate::{Error, Result};

/// Lock ignoring poison. Critical sections here never panic midway.
fn lock<T>(mutex: &Mutex<T>) -> MutexGuard<'_, T> {
    mutex.lock().unwrap_or_else(PoisonError::into_inner)
}

struct Queue<C> {
    commands: VecDeque<C>,
    /// The owner is gone; no more commands are accepted.
    owner_gone: bool,
    /// The worker is gone; no more commands will arrive.
    worker_gone: bool,
}

/// State shared by one worker and the owner.
struct Channel<C> {
    queue: Mutex<Queue<C>>,
    capacity: usize,
    /// Woken when the owner frees space or goes away.
    worker: AtomicWaker,
    /// Shared by every channel of one owner. Woken on new commands or a
    /// worker drop.
    owner: Arc<AtomicWaker>,
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
    let shared: Vec<_> = (0..workers)
        .map(|_| {
            Arc::new(Channel {
                queue: Mutex::new(Queue {
                    commands: VecDeque::with_capacity(capacity),
                    owner_gone: false,
                    worker_gone: false,
                }),
                capacity,
                worker: AtomicWaker::new(),
                owner: waker.clone(),
            })
        })
        .collect();
    let handles = shared
        .iter()
        .map(|channel| Worker {
            channel: channel.clone(),
        })
        .collect();

    Ok((
        handles,
        Owner {
            channels: shared,
            waker,
            next: 0,
        },
    ))
}

/// One I/O worker's sending side.
pub(crate) struct Worker<C> {
    channel: Arc<Channel<C>>,
}

impl<C> Worker<C> {
    /// Queue `command` for the owner and wake it.
    ///
    /// On failure the command comes back with [`Error::Overloaded`] (queue
    /// full; call [`Worker::poll_ready`] to wait) or [`Error::Unavailable`]
    /// (owner gone).
    pub(crate) fn try_send(&self, command: C) -> std::result::Result<(), (Error, C)> {
        {
            let mut queue = lock(&self.channel.queue);

            if queue.owner_gone {
                return Err((Error::Unavailable, command));
            }

            if queue.commands.len() >= self.channel.capacity {
                return Err((Error::Overloaded, command));
            }

            queue.commands.push_back(command);
        }

        self.channel.owner.wake();
        Ok(())
    }

    /// Ready once the queue has space. Fails with [`Error::Unavailable`]
    /// once the owner is gone.
    pub(crate) fn poll_ready(&self, cx: &mut Context<'_>) -> Poll<Result<()>> {
        // Register first so a drain between the check and the return wakes us.
        self.channel.worker.register(cx.waker());

        let queue = lock(&self.channel.queue);

        if queue.owner_gone {
            Poll::Ready(Err(Error::Unavailable))
        } else if queue.commands.len() < self.channel.capacity {
            Poll::Ready(Ok(()))
        } else {
            Poll::Pending
        }
    }
}

impl<C> Drop for Worker<C> {
    /// Tell the owner no more commands come from this worker.
    fn drop(&mut self) {
        lock(&self.channel.queue).worker_gone = true;
        self.channel.owner.wake();
    }
}

/// The node owner's receiving side for every worker.
pub(crate) struct Owner<C> {
    channels: Vec<Arc<Channel<C>>>,
    waker: Arc<AtomicWaker>,
    /// Worker to start the next drain at, for fairness.
    next: usize,
}

impl<C> Owner<C> {
    /// Number of workers.
    pub(crate) fn workers(&self) -> usize {
        self.channels.len()
    }

    /// Hand up to `budget` queued commands to `handle` as
    /// `(worker index, command)`. Commands from one worker arrive in send
    /// order. Workers are visited round-robin, one command at a time.
    ///
    /// Returns `Ready(n)` with `n > 0` if it handled anything, otherwise
    /// registers `cx` and returns `Pending`. Workers waiting for space are
    /// woken.
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

    fn drain(&mut self, budget: usize, handle: &mut impl FnMut(usize, C)) -> usize {
        let count = self.channels.len();
        let mut handled = 0;

        while handled < budget {
            let mut progressed = false;

            for step in 0..count {
                if handled >= budget {
                    break;
                }

                let index = (self.next + step) % count;
                let channel = &self.channels[index];
                let command = lock(&channel.queue).commands.pop_front();

                if let Some(command) = command {
                    channel.worker.wake();
                    handle(index, command);
                    handled += 1;
                    progressed = true;
                }
            }

            if count > 0 {
                self.next = (self.next + 1) % count;
            }

            if !progressed {
                break;
            }
        }

        handled
    }

    /// True once worker `index` is dropped and its queue is empty.
    pub(crate) fn is_closed(&self, index: usize) -> bool {
        self.channels.get(index).is_none_or(|channel| {
            let queue = lock(&channel.queue);
            queue.worker_gone && queue.commands.is_empty()
        })
    }
}

impl<C> Drop for Owner<C> {
    /// Close every queue, drop queued commands, and wake waiting workers.
    fn drop(&mut self) {
        for channel in &self.channels {
            let commands = {
                let mut queue = lock(&channel.queue);
                queue.owner_gone = true;
                std::mem::take(&mut queue.commands)
            };

            // Dropped outside the lock: commands may complete ops and wake.
            drop(commands);
            channel.worker.wake();
        }
    }
}

enum State<T> {
    Pending,
    Done(Result<T>),
    /// The result was taken by `Op::poll`.
    Taken,
}

struct OpCell<T> {
    state: Mutex<State<T>>,
    /// Cleared when the `Op` drops.
    waiting: Mutex<bool>,
    waker: AtomicWaker,
}

/// Create a pending operation and the handle that completes it.
pub(crate) fn op<T>() -> (Op<T>, Completer<T>) {
    let cell = Arc::new(OpCell {
        state: Mutex::new(State::Pending),
        waiting: Mutex::new(true),
        waker: AtomicWaker::new(),
    });

    (Op(cell.clone()), Completer(Some(cell)))
}

/// One-shot result of an owner operation.
pub struct Op<T>(Arc<OpCell<T>>);

impl<T> Op<T> {
    /// The result, once. Polling after `Ready` returns
    /// [`Error::InvalidRequest`].
    pub fn poll(&self, cx: &mut Context<'_>) -> Poll<Result<T>> {
        self.0.waker.register(cx.waker());

        let mut state = lock(&self.0.state);

        match std::mem::replace(&mut *state, State::Taken) {
            State::Pending => {
                *state = State::Pending;
                Poll::Pending
            }
            State::Done(result) => Poll::Ready(result),
            State::Taken => Poll::Ready(Err(Error::InvalidRequest)),
        }
    }
}

impl<T> Drop for Op<T> {
    /// Mark the op abandoned and drop any unread result.
    fn drop(&mut self) {
        *lock(&self.0.waiting) = false;

        let state = std::mem::replace(&mut *lock(&self.0.state), State::Taken);
        drop(state);
    }
}

/// The owner's side of an [`Op`]. Dropping it uncompleted cancels the op.
pub(crate) struct Completer<T>(Option<Arc<OpCell<T>>>);

impl<T> Completer<T> {
    /// True once the [`Op`] is dropped.
    pub(crate) fn is_abandoned(&self) -> bool {
        self.0.as_ref().is_none_or(|cell| !*lock(&cell.waiting))
    }

    /// Resolve the op and wake its poller. If the [`Op`] is gone the value
    /// comes back as `Err` so the caller can clean it up.
    pub(crate) fn complete(mut self, result: Result<T>) -> std::result::Result<(), Result<T>> {
        let cell = self.0.take().expect("completer used once");

        Self::resolve(&cell, result)
    }

    fn resolve(cell: &OpCell<T>, result: Result<T>) -> std::result::Result<(), Result<T>> {
        {
            // Lock order: waiting, then state. `Op::drop` takes them one at a
            // time, so holding `waiting` makes the check and store atomic
            // with respect to the drop.
            let waiting = lock(&cell.waiting);

            if !*waiting {
                return Err(result);
            }

            *lock(&cell.state) = State::Done(result);
        }

        cell.waker.wake();
        Ok(())
    }
}

impl<T> Drop for Completer<T> {
    /// Cancel an uncompleted op.
    fn drop(&mut self) {
        if let Some(cell) = self.0.take() {
            let _ = Self::resolve(&cell, Err(Error::Cancelled));
        }
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

        assert_eq!(owner.workers(), 1);
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
    fn owner_drop_cancels_queued_and_rejects_new() {
        let (workers, owner) = channels::<Completer<u32>>(1, 4).unwrap();
        let (op, completer) = op();
        let (count, waker) = counter();
        let mut cx = Context::from_waker(&waker);
        let marker = Arc::new(());

        let _ = op.poll(&mut cx);
        assert!(workers[0].try_send(completer).is_ok());
        drop(owner);

        assert_eq!(op.poll(&mut cx), Poll::Ready(Err(Error::Cancelled)));
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
        assert_eq!(op.poll(&mut cx), Poll::Ready(Err(Error::Cancelled)));
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
                                Poll::Ready(Err(Error::Cancelled)) => {
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
                let (ok, cancelled) = sender.join().unwrap();
                ok + cancelled
            })
            .sum();

        assert_eq!(owner_thread.join().unwrap(), WORKERS * OPS);
        assert_eq!(polled, WORKERS * (OPS - OPS.div_ceil(5)));
        assert_eq!(Arc::strong_count(&marker), 1);
    }
}
