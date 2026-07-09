// Copyright (c) Microsoft Corporation.
// Licensed under the MIT License.

//! Per-engine mutator queue and reply primitives.
//!
//! Replaces the engine's old `mutator_gate: AsyncMutex` with a
//! single-consumer batched task. Writers and the eviction path
//! enqueue [`MutatorReq`] items and await a per-request
//! [`MutatorReply`]. The engine's `run_mutator` loop drains up to
//! [`crate::storage::EngineConfig::commit_batch_max`] items per
//! tick and folds them into one `BTreeIndex::apply_batch` call,
//! preserving the invariant that the btree mutation commits
//! before any allocator slot is released.
//!
//! The queue is intentionally tiny: producers and the consumer
//! share an `std::sync::Mutex` so the surface is `Send + Sync`
//! and matches the rest of the engine's `Mutex`-guarded state.
//! A single waker is stored for the consumer; producers wake it
//! after every push.

use std::collections::VecDeque;
use std::future::Future;
use std::pin::Pin;
use std::sync::{Arc, Mutex};
use std::task::{Context, Poll, Waker};

use crate::storage::btree::LeafEntry;
use crate::storage::types::PageKey;

/// A single submission to the mutator. The submitter has already
/// allocated the LBA run (recorded in `entry.lba`) and written the
/// page to the device; the mutator only owns the btree mutation.
/// Each variant carries the shared [`MutatorReply`] the submitter
/// awaits.
pub(crate) enum MutatorReq {
    Insert {
        key: PageKey,
        entry: LeafEntry,
        done: Arc<MutatorReply>,
    },
    Delete {
        keys: Vec<PageKey>,
        done: Arc<MutatorReply>,
    },
}

/// What the mutator observed when it processed a request.
#[derive(Clone, Debug)]
pub(crate) enum MutatorOutcome {
    /// `apply_batch` committed; `prior` is the full LeafEntry the
    /// btree mapped `key` to immediately before this batch, or
    /// `None` if the key was unmapped. The entry's `byte_len` lets
    /// the submitter free the entire prior contiguous LBA range.
    InsertCommitted { prior: Option<LeafEntry> },
    /// `apply_batch` committed the delete set.
    DeleteCommitted,
    /// `apply_batch` returned an error. The submitter must clean
    /// up the LBA range it allocated (insert) or leave eviction
    /// state untouched (delete).
    Failed,
}

/// Shared reply slot: filled by the mutator, awaited by the
/// submitter. Cheap to construct (`Arc<Self>`) so the engine can
/// allocate one per request.
pub(crate) struct MutatorReply {
    inner: Mutex<ReplyInner>,
}

struct ReplyInner {
    result: Option<MutatorOutcome>,
    waker: Option<Waker>,
}

impl MutatorReply {
    pub(crate) fn new() -> Arc<Self> {
        Arc::new(Self {
            inner: Mutex::new(ReplyInner {
                result: None,
                waker: None,
            }),
        })
    }

    /// Called by the mutator once the batch this request was part
    /// of has been processed. Wakes the submitter if it has
    /// already polled.
    pub(crate) fn set(&self, outcome: MutatorOutcome) {
        let waker = {
            let mut g = self.inner.lock().unwrap();
            g.result = Some(outcome);
            g.waker.take()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    pub(crate) fn wait(self: Arc<Self>) -> ReplyWait {
        ReplyWait { inner: self }
    }
}

pub(crate) struct ReplyWait {
    inner: Arc<MutatorReply>,
}

impl Future for ReplyWait {
    type Output = MutatorOutcome;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<Self::Output> {
        let mut g = self.inner.inner.lock().unwrap();
        if let Some(o) = g.result.take() {
            Poll::Ready(o)
        } else {
            if !g.waker.as_ref().is_some_and(|w| w.will_wake(cx.waker())) {
                g.waker = Some(cx.waker().clone());
            }
            Poll::Pending
        }
    }
}

/// MPSC-style queue between writer/eviction tasks (producers)
/// and the engine's `run_mutator` loop (single consumer). All
/// operations take `&self` so an `Arc<MutatorQueue>` can be
/// shared between every producer and the consumer.
pub(crate) struct MutatorQueue {
    inner: Mutex<QueueInner>,
}

struct QueueInner {
    items: VecDeque<MutatorReq>,
    consumer_waker: Option<Waker>,
    closed: bool,
}

impl MutatorQueue {
    pub(crate) fn new() -> Self {
        Self {
            inner: Mutex::new(QueueInner {
                items: VecDeque::new(),
                consumer_waker: None,
                closed: false,
            }),
        }
    }

    /// Enqueue a request and wake the consumer. Pushes on a
    /// closed queue are silently dropped after the submitter is
    /// notified via `Failed`; closure only happens at shutdown,
    /// and during shutdown no new producer should be running.
    pub(crate) fn push(&self, req: MutatorReq) {
        let waker = {
            let mut g = self.inner.lock().unwrap();
            if g.closed {
                // Fail the request inline so the submitter
                // doesn't deadlock.
                drop(g);
                match req {
                    MutatorReq::Insert { done, .. } | MutatorReq::Delete { done, .. } => {
                        done.set(MutatorOutcome::Failed);
                    }
                }
                return;
            }
            g.items.push_back(req);
            g.consumer_waker.take()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    /// Mark the queue as closed and wake the consumer so it can
    /// exit. Items still in the queue are processed normally;
    /// only future pushes fail.
    pub(crate) fn close(&self) {
        let waker = {
            let mut g = self.inner.lock().unwrap();
            g.closed = true;
            g.consumer_waker.take()
        };
        if let Some(w) = waker {
            w.wake();
        }
    }

    /// True once the queue is closed and drained: the consumer's
    /// exit condition.
    pub(crate) fn is_closed_and_empty(&self) -> bool {
        let g = self.inner.lock().unwrap();
        g.closed && g.items.is_empty()
    }

    /// True once the queue has been closed via [`Self::close`].
    /// The coalescing drain consults this so it stops spending its
    /// yield budget on a queue that can no longer receive pushes,
    /// letting a draining shutdown commit and exit promptly.
    pub(crate) fn is_closed(&self) -> bool {
        self.inner.lock().unwrap().closed
    }

    /// Number of items currently buffered. Used by shutdown
    /// invariants to verify the mutator drained all submitted
    /// requests before exiting.
    pub(crate) fn pending_len(&self) -> usize {
        self.inner.lock().unwrap().items.len()
    }

    /// Pop up to `max` items. Returns an empty vec when nothing
    /// is queued.
    pub(crate) fn try_drain_up_to(&self, max: usize) -> Vec<MutatorReq> {
        let mut g = self.inner.lock().unwrap();
        let n = g.items.len().min(max);
        let mut out = Vec::with_capacity(n);
        for _ in 0..n {
            if let Some(r) = g.items.pop_front() {
                out.push(r);
            }
        }
        out
    }

    /// Drain a coalesced batch of up to `max` items, spending at
    /// most `ticks` cooperative yields to let in-flight producers
    /// enqueue more before the consumer commits. Returns as soon
    /// as the batch reaches `max`, the queue is closed, or the
    /// yield budget is exhausted.
    ///
    /// The DST harness forbids reading elapsed time, so coalescing is
    /// expressed as a budget of `ticks` logical yields (see
    /// [`crate::storage::EngineConfig::commit_batch_ticks`]). Each
    /// [`yield_once`] gives the executor a chance to
    /// interleave producers that are about to push, so batches
    /// grow under load while a drained or closed queue never
    /// stalls. `ticks == 0` disables coalescing (a single drain).
    ///
    /// Callers gate on [`Self::wait_nonempty`] first, so the
    /// returned batch is non-empty in practice; it can only be
    /// empty if the caller raced an empty, non-closed queue.
    pub(crate) async fn drain_batch(&self, max: usize, ticks: u32) -> Vec<MutatorReq> {
        let mut batch = self.try_drain_up_to(max);
        let mut spent = 0u32;
        while batch.len() < max && spent < ticks {
            // A closed queue will receive no further pushes, so
            // stop waiting and let the consumer commit what it has
            // and observe the close on its next loop.
            if self.is_closed() {
                break;
            }
            yield_once().await;
            spent += 1;
            let more = self.try_drain_up_to(max - batch.len());
            batch.extend(more);
        }
        batch
    }

    /// Park the consumer until something is queued or the queue
    /// is closed. Resolves to `true` when there is at least one
    /// item ready to drain, `false` when the queue is closed AND
    /// empty (consumer should exit).
    pub(crate) fn wait_nonempty(self: &Arc<Self>) -> WaitNonEmpty {
        WaitNonEmpty {
            queue: self.clone(),
        }
    }
}

pub(crate) struct WaitNonEmpty {
    queue: Arc<MutatorQueue>,
}

impl Future for WaitNonEmpty {
    type Output = bool;
    fn poll(self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<bool> {
        let mut g = self.queue.inner.lock().unwrap();
        if !g.items.is_empty() {
            return Poll::Ready(true);
        }
        if g.closed {
            return Poll::Ready(false);
        }
        if !g
            .consumer_waker
            .as_ref()
            .is_some_and(|w| w.will_wake(cx.waker()))
        {
            g.consumer_waker = Some(cx.waker().clone());
        }
        Poll::Pending
    }
}

/// Yield exactly once, registering the current waker to be woken
/// immediately so the executor can interleave other tasks before
/// re-polling. This is the single-tick building block the mutator
/// uses to coalesce batches without reaching for wall-clock time,
/// which the DST harness forbids: [`MutatorQueue::drain_batch`]
/// spends a bounded number of these yields (see
/// [`crate::storage::EngineConfig::commit_batch_ticks`]) to invite
/// in-flight producers to enqueue into the current batch before it
/// commits.
pub(crate) fn yield_once() -> YieldOnce {
    YieldOnce { yielded: false }
}

pub(crate) struct YieldOnce {
    yielded: bool,
}

impl Future for YieldOnce {
    type Output = ();
    fn poll(mut self: Pin<&mut Self>, cx: &mut Context<'_>) -> Poll<()> {
        if self.yielded {
            Poll::Ready(())
        } else {
            self.yielded = true;
            cx.waker().wake_by_ref();
            Poll::Pending
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::runtime::noop_waker;
    use crate::storage::btree::LeafEntry;
    use crate::storage::types::{Checksum, Lba};
    use proptest::collection::vec;
    use proptest::prelude::*;
    use std::future::Future;
    use std::pin::pin;
    use std::sync::atomic::{AtomicUsize, Ordering};
    use std::task::{Context, Poll};
    use std::task::{Wake, Waker};

    fn block_on<F: Future>(f: F) -> F::Output {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut f = pin!(f);
        let mut spins = 0u64;
        loop {
            match f.as_mut().poll(&mut cx) {
                Poll::Ready(v) => return v,
                Poll::Pending => {
                    spins += 1;
                    assert!(spins < 1_000_000, "stuck");
                }
            }
        }
    }

    #[test]
    fn reply_resolves_after_set() {
        let r = MutatorReply::new();
        r.set(MutatorOutcome::DeleteCommitted);
        let out = block_on(r.wait());
        assert!(matches!(out, MutatorOutcome::DeleteCommitted));
    }

    #[test]
    fn queue_drains_in_order() {
        let q = Arc::new(MutatorQueue::new());
        let r1 = MutatorReply::new();
        let r2 = MutatorReply::new();
        q.push(MutatorReq::Delete {
            keys: vec![],
            done: r1.clone(),
        });
        q.push(MutatorReq::Delete {
            keys: vec![],
            done: r2.clone(),
        });
        let drained = q.try_drain_up_to(10);
        assert_eq!(drained.len(), 2);
    }

    #[test]
    fn close_then_push_fails_inline() {
        let q = Arc::new(MutatorQueue::new());
        q.close();
        let r = MutatorReply::new();
        q.push(MutatorReq::Delete {
            keys: vec![],
            done: r.clone(),
        });
        let out = block_on(r.wait());
        assert!(matches!(out, MutatorOutcome::Failed));
    }

    #[test]
    fn wait_nonempty_returns_false_when_closed_empty() {
        let q = Arc::new(MutatorQueue::new());
        q.close();
        let ok = block_on(q.wait_nonempty());
        assert!(!ok);
    }

    fn push_n(q: &Arc<MutatorQueue>, n: usize) {
        for _ in 0..n {
            q.push(MutatorReq::Delete {
                keys: vec![],
                done: MutatorReply::new(),
            });
        }
    }

    /// Drive a `drain_batch` future to completion under a noop
    /// waker, invoking `on_pending` before each re-poll. Returns
    /// the drained batch and the number of `Poll::Pending`s
    /// observed, i.e. the number of yields the drain actually
    /// spent.
    fn drive_drain(
        q: &Arc<MutatorQueue>,
        max: usize,
        ticks: u32,
        mut on_pending: impl FnMut(),
    ) -> (Vec<MutatorReq>, u32) {
        let w = noop_waker();
        let mut cx = Context::from_waker(&w);
        let mut fut = pin!(q.drain_batch(max, ticks));
        let mut pendings = 0u32;
        loop {
            match fut.as_mut().poll(&mut cx) {
                Poll::Ready(b) => return (b, pendings),
                Poll::Pending => {
                    on_pending();
                    pendings += 1;
                    assert!(pendings < 1_000, "drain_batch did not converge");
                }
            }
        }
    }

    #[test]
    fn drain_batch_coalesces_toward_max() {
        let q = Arc::new(MutatorQueue::new());
        push_n(&q, 2);
        // Two more producers enqueue on every yield, so the batch
        // climbs to `max` within the tick budget.
        let (batch, _) = drive_drain(&q, 8, 8, || push_n(&q, 2));
        assert_eq!(batch.len(), 8);
    }

    #[test]
    fn drain_batch_stops_at_tick_budget_when_starved() {
        let q = Arc::new(MutatorQueue::new());
        push_n(&q, 1);
        // No producer enqueues, so the drain spends its whole
        // budget and returns the partial batch rather than
        // stalling forever.
        let (batch, pendings) = drive_drain(&q, 8, 3, || {});
        assert_eq!(pendings, 3);
        assert_eq!(batch.len(), 1);
    }

    #[test]
    fn drain_batch_on_closed_queue_commits_without_yielding() {
        let q = Arc::new(MutatorQueue::new());
        push_n(&q, 2);
        q.close();
        // A closed queue must not spend any of the yield budget so
        // a draining shutdown terminates promptly.
        let (batch, pendings) = drive_drain(&q, 8, 8, || panic!("closed queue must not yield"));
        assert_eq!(pendings, 0);
        assert_eq!(batch.len(), 2);
    }

    struct CountWaker {
        count: Arc<AtomicUsize>,
    }

    impl Wake for CountWaker {
        fn wake(self: Arc<Self>) {
            self.wake_by_ref();
        }

        fn wake_by_ref(self: &Arc<Self>) {
            self.count.fetch_add(1, Ordering::SeqCst);
        }
    }

    #[derive(Default)]
    struct ProducerState {
        wait: Option<Pin<Box<ReplyWait>>>,
        wake_count: Arc<AtomicUsize>,
        accepted: bool,
        completed: bool,
    }

    struct ConsumerState {
        wait: Pin<Box<WaitNonEmpty>>,
        wake_count: Arc<AtomicUsize>,
        pending_before_wake: Option<usize>,
        exited: bool,
    }

    impl ConsumerState {
        fn new(queue: &Arc<MutatorQueue>) -> Self {
            Self {
                wait: Box::pin(queue.wait_nonempty()),
                wake_count: Arc::new(AtomicUsize::new(0)),
                pending_before_wake: None,
                exited: false,
            }
        }
    }

    fn count_waker(count: Arc<AtomicUsize>) -> Waker {
        Arc::new(CountWaker { count }).into()
    }

    fn page_key(i: u32) -> PageKey {
        PageKey::new([i as u8; 32], i)
    }

    fn leaf_entry(i: u64) -> LeafEntry {
        LeafEntry {
            lba: Lba(i),
            data_checksum: Checksum(0),
            byte_len: 4096,
        }
    }

    fn poll_reply(producer: &mut ProducerState) {
        let Some(wait) = producer.wait.as_mut() else {
            return;
        };
        let waker = count_waker(producer.wake_count.clone());
        let mut cx = Context::from_waker(&waker);
        match wait.as_mut().poll(&mut cx) {
            Poll::Ready(_) => {
                producer.completed = true;
                producer.wait = None;
            }
            Poll::Pending => {}
        }
    }

    fn push_request(
        queue: &Arc<MutatorQueue>,
        producer: &mut ProducerState,
        idx: usize,
        variant: u8,
    ) -> bool {
        if producer.wait.is_some() || producer.completed {
            return false;
        }

        let reply = MutatorReply::new();
        let req = if variant % 2 == 0 {
            MutatorReq::Insert {
                key: page_key(idx as u32),
                entry: leaf_entry(idx as u64 + 10),
                done: reply.clone(),
            }
        } else {
            MutatorReq::Delete {
                keys: vec![page_key(idx as u32)],
                done: reply.clone(),
            }
        };
        queue.push(req);
        producer.accepted = !queue.is_closed();
        producer.wait = Some(Box::pin(reply.wait()));
        true
    }

    fn poll_consumer(queue: &Arc<MutatorQueue>, consumer: &mut ConsumerState) {
        if consumer.exited {
            return;
        }

        let waker = count_waker(consumer.wake_count.clone());
        let mut cx = Context::from_waker(&waker);
        match consumer.wait.as_mut().poll(&mut cx) {
            Poll::Pending => {
                consumer.pending_before_wake = Some(consumer.wake_count.load(Ordering::SeqCst));
            }
            Poll::Ready(true) => {
                if let Some(before) = consumer.pending_before_wake.take() {
                    assert!(
                        consumer.wake_count.load(Ordering::SeqCst) > before,
                        "producer push did not wake parked mutator consumer",
                    );
                }

                let batch = queue.try_drain_up_to(4);
                assert!(!batch.is_empty(), "ready consumer drained no requests");
                for req in batch {
                    match req {
                        MutatorReq::Insert { done, .. } => {
                            done.set(MutatorOutcome::InsertCommitted { prior: None })
                        }
                        MutatorReq::Delete { done, .. } => {
                            done.set(MutatorOutcome::DeleteCommitted)
                        }
                    }
                }
                consumer.wait = Box::pin(queue.wait_nonempty());
            }
            Poll::Ready(false) => {
                assert!(queue.is_closed_and_empty());
                consumer.exited = true;
            }
        }
    }

    proptest! {
        #![proptest_config(ProptestConfig {
            cases: 128,
            ..ProptestConfig::default()
        })]

        #[test]
        fn randomized_queue_wakeup_close_and_reply_interleavings(
            ops in vec((0usize..8, 0u8..5), 1..200),
        ) {
            let queue = Arc::new(MutatorQueue::new());
            let mut producers: Vec<ProducerState> = (0..8).map(|_| ProducerState::default()).collect();
            let mut consumer = ConsumerState::new(&queue);
            let mut closed = false;

            for (actor, op) in ops {
                match op {
                    0 | 1 => {
                        let pushed = push_request(&queue, &mut producers[actor], actor, op);
                        if closed && pushed {
                            poll_reply(&mut producers[actor]);
                            prop_assert!(
                                producers[actor].completed,
                                "push after close did not fail reply inline",
                            );
                        }
                    }
                    2 => poll_reply(&mut producers[actor]),
                    3 => poll_consumer(&queue, &mut consumer),
                    4 => {
                        queue.close();
                        closed = true;
                        poll_consumer(&queue, &mut consumer);
                    }
                    _ => unreachable!(),
                }
            }

            queue.close();
            for _ in 0..64 {
                poll_consumer(&queue, &mut consumer);
                for producer in &mut producers {
                    poll_reply(producer);
                }
                if consumer.exited {
                    break;
                }
            }

            prop_assert!(consumer.exited, "consumer did not observe closed-empty queue");
            prop_assert_eq!(queue.pending_len(), 0, "mutator queue leaked pending requests");
            for (idx, producer) in producers.iter().enumerate() {
                prop_assert!(
                    producer.completed || !producer.accepted,
                    "accepted producer {} did not receive exactly one reply",
                    idx,
                );
            }
        }
    }
}
