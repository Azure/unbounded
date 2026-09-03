// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Bounded work queue feeding the RPC server's persistent worker pool.
//!
//! The recv-completion handler runs on the fabric progress thread; it
//! cannot block serving a request without stalling completion
//! progress. Instead it builds a type-erased [`Job`] and hands it to
//! this queue, where one of the pool's long-lived worker threads picks
//! it up. The queue is the only synchronization point between the
//! progress thread (producer) and the workers (consumers).
//!
//! Jobs are erased to `Box<dyn FnOnce() + Send>` so the queue and the
//! [`RpcServerHandle`](super::rpc::RpcServerHandle) stay free of the
//! request/handler type parameters; the completion handler captures
//! those in the closure it pushes.
//!
//! Shutdown is cooperative: [`JobQueue::close`] wakes every blocked
//! worker so an idle pool drains promptly, while already-queued jobs
//! are still handed out (each observes the server's shutdown flag and
//! finishes fast) so no enqueued request is silently dropped.

use std::collections::VecDeque;
use std::sync::{Condvar, Mutex};

/// A unit of work erased of its request/handler types. Running it
/// serves one RPC request to completion.
pub(crate) type Job = Box<dyn FnOnce() + Send + 'static>;

/// MPSC job queue with a single `Condvar` for blocking consumers.
pub(crate) struct JobQueue {
    inner: Mutex<Inner>,
    not_empty: Condvar,
}

struct Inner {
    jobs: VecDeque<Job>,
    closed: bool,
}

impl JobQueue {
    pub(crate) fn new() -> Self {
        Self {
            inner: Mutex::new(Inner {
                jobs: VecDeque::new(),
                closed: false,
            }),
            not_empty: Condvar::new(),
        }
    }

    /// Enqueue `job`. Returns `Err(job)` if the queue is closed so the
    /// caller can drop the work and its captured resources.
    pub(crate) fn push(&self, job: Job) -> Result<(), Job> {
        let mut inner = self.inner.lock().expect("job queue poisoned");
        if inner.closed {
            return Err(job);
        }
        inner.jobs.push_back(job);
        drop(inner);
        self.not_empty.notify_one();
        Ok(())
    }

    /// Block until a job is available, returning `None` only once the
    /// queue is both closed and drained. Closing drains in FIFO order
    /// so queued requests are still served (and can observe shutdown)
    /// rather than discarded.
    pub(crate) fn pop_blocking(&self) -> Option<Job> {
        let mut inner = self.inner.lock().expect("job queue poisoned");
        loop {
            if let Some(job) = inner.jobs.pop_front() {
                return Some(job);
            }
            if inner.closed {
                return None;
            }
            inner = self.not_empty.wait(inner).expect("job queue poisoned");
        }
    }

    /// Mark the queue closed and wake every blocked worker. Idempotent.
    pub(crate) fn close(&self) {
        let mut inner = self.inner.lock().expect("job queue poisoned");
        inner.closed = true;
        drop(inner);
        self.not_empty.notify_all();
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::Arc;
    use std::sync::atomic::{AtomicU32, Ordering};

    #[test]
    fn pop_runs_pushed_job() {
        let q = JobQueue::new();
        let ran = Arc::new(AtomicU32::new(0));
        let r = ran.clone();
        assert!(
            q.push(Box::new(move || {
                r.fetch_add(1, Ordering::SeqCst);
            }))
            .is_ok(),
            "push on open queue"
        );

        let job = q.pop_blocking().expect("job available");
        job();
        assert_eq!(ran.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn push_after_close_is_rejected() {
        let q = JobQueue::new();
        q.close();
        let rejected = q.push(Box::new(|| {}));
        assert!(rejected.is_err(), "closed queue must reject pushes");
    }

    #[test]
    fn rejected_job_drops_captured_resources() {
        struct DropCount(Arc<AtomicU32>);

        impl Drop for DropCount {
            fn drop(&mut self) {
                self.0.fetch_add(1, Ordering::SeqCst);
            }
        }

        let q = JobQueue::new();
        let drops = Arc::new(AtomicU32::new(0));
        let guard = DropCount(drops.clone());
        q.close();

        drop(q.push(Box::new(move || drop(guard))));
        assert_eq!(drops.load(Ordering::SeqCst), 1);
    }

    #[test]
    fn close_drains_remaining_then_returns_none() {
        let q = JobQueue::new();
        assert!(q.push(Box::new(|| {})).is_ok());
        assert!(q.push(Box::new(|| {})).is_ok());
        q.close();

        assert!(q.pop_blocking().is_some(), "first queued job drains");
        assert!(q.pop_blocking().is_some(), "second queued job drains");
        assert!(
            q.pop_blocking().is_none(),
            "drained closed queue signals exit"
        );
    }

    #[test]
    fn blocked_pop_wakes_on_close() {
        let q = Arc::new(JobQueue::new());
        let consumer = q.clone();
        let handle = std::thread::spawn(move || consumer.pop_blocking().is_none());

        // Give the consumer time to block on the condvar, then close.
        std::thread::sleep(std::time::Duration::from_millis(20));
        q.close();

        assert!(
            handle.join().expect("consumer thread"),
            "a blocked consumer must wake with None when the queue closes"
        );
    }

    #[test]
    fn producer_consumer_drains_every_job() {
        const N: u32 = 1000;
        let q = Arc::new(JobQueue::new());
        let ran = Arc::new(AtomicU32::new(0));

        let consumer_q = q.clone();
        let consumer_ran = ran.clone();
        let handle = std::thread::spawn(move || {
            while let Some(job) = consumer_q.pop_blocking() {
                job();
            }
            consumer_ran.load(Ordering::SeqCst)
        });

        for _ in 0..N {
            let r = ran.clone();
            assert!(
                q.push(Box::new(move || {
                    r.fetch_add(1, Ordering::SeqCst);
                }))
                .is_ok(),
                "push on open queue"
            );
        }
        q.close();

        let observed = handle.join().expect("consumer thread");
        assert_eq!(
            observed, N,
            "every pushed job must run before the consumer exits"
        );
    }
}
