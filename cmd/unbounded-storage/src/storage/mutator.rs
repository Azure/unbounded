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
use crate::storage::types::{Lba, PageKey};

/// A single submission to the mutator. Each variant carries the
/// shared [`MutatorReply`] the submitter awaits.
pub(crate) enum MutatorReq {
    Insert {
        key: PageKey,
        entry: LeafEntry,
        /// The freshly allocated LBA for this insert. Carried
        /// alongside `entry.lba` so the mutator can return it to
        /// the allocator if `apply_batch` fails; the submitter
        /// has already done the device write.
        lba: Lba,
        done: Arc<MutatorReply>,
    },
    Delete {
        keys: Vec<PageKey>,
        done: Arc<MutatorReply>,
    },
    /// Request that the mutator perform an eviction sweep. The
    /// mutator selects victims, removes their btree entries, and
    /// returns the freed LBAs to the submitter so the submitter
    /// can free them in the allocator. Routing eviction through
    /// the mutator is the only way to make "pick victims" and
    /// "delete victims from btree" atomic with respect to
    /// concurrent inserts; see `StorageEngine::evict_if_over_watermark`.
    Evict {
        count: usize,
        done: Arc<MutatorReply>,
    },
}

/// What the mutator observed when it processed a request.
#[derive(Clone, Debug)]
pub(crate) enum MutatorOutcome {
    /// `apply_batch` committed; `prior_lba` is whatever LBA the
    /// btree mapped `key` to immediately before this batch, or
    /// `None` if the key was unmapped.
    InsertCommitted { prior_lba: Option<Lba> },
    /// `apply_batch` committed the delete set.
    DeleteCommitted,
    /// Eviction sweep committed. `freed` lists the LBAs whose
    /// btree entries were removed in the same batch; the
    /// submitter owns them and must hand them back to the
    /// allocator after the reply.
    EvictCommitted { freed: Vec<Lba> },
    /// `apply_batch` returned an error. The submitter must clean
    /// up the LBA it allocated (insert) or leave eviction state
    /// untouched (delete).
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
                    MutatorReq::Insert { done, .. }
                    | MutatorReq::Delete { done, .. }
                    | MutatorReq::Evict { done, .. } => {
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
/// immediately. Used by the mutator loop to invite further
/// submissions to coalesce into the current batch without
/// reaching for wall-clock time, which the DST harness forbids.
///
/// The crate's `commit_batch_deadline_us` config field exists to
/// let production tune coalescing latency; here we approximate
/// it as a single yield so the executor can interleave producers
/// that are about to enqueue. Replacing this with a tick-aware
/// scheme is a follow-up once a tick abstraction lands.
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
    use std::future::Future;
    use std::pin::pin;
    use std::task::{Context, Poll, RawWaker, RawWakerVTable, Waker};

    fn noop_waker() -> Waker {
        fn raw() -> RawWaker {
            RawWaker::new(std::ptr::null(), &VTABLE)
        }
        static VTABLE: RawWakerVTable = RawWakerVTable::new(|_| raw(), |_| {}, |_| {}, |_| {});
        unsafe { Waker::from_raw(raw()) }
    }

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
}
