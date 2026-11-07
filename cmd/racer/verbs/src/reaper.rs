//! Teardown calls that can block in the kernel, run off the owner thread.
//!
//! `ibv_destroy_qp`, `rdma_destroy_qp`, and `rdma_destroy_id` normally return
//! at once, but a sick device can make them wait in the kernel for good: an
//! Azure MANA VF hot-unplug left `rdma_destroy_id` in uninterruptible sleep
//! (`cm_destroy_id_wait_timeout` every 90 s). Run on the owner thread, that
//! stalls every link on the node. The owner hands such calls to a reaper
//! thread instead and polls the returned [`Ticket`]; until it completes, the
//! owner keeps every buffer, registration, and handle the QP may still use,
//! so a hung teardown leaks memory but never frees it early.
//!
//! The handoff is bounded: each reaper thread has a small queue, at most
//! [`MAX_REAPERS`] threads exist, and a full queue makes the owner retry
//! later rather than wait. A thread busy on one teardown longer than
//! [`STUCK`] is presumed stuck in the kernel and left behind; later
//! teardowns go to a fresh one.
//! Each thread runs its teardowns at most one per [`PACE`], so a burst of
//! link teardowns (an idle close of every link, a node close, or a device
//! loss) reaches the device's command channel spread out, not all at once.

use std::{
    sync::{
        Arc, Mutex, OnceLock, PoisonError,
        atomic::{AtomicU8, AtomicU64, Ordering},
        mpsc::{Receiver, SyncSender, TrySendError, sync_channel},
    },
    task::Waker,
    time::{Duration, Instant},
};

/// Teardowns one reaper thread queues before submissions go elsewhere.
pub(crate) const QUEUE: usize = 32;

/// Reaper threads at most, counting stuck ones.
pub(crate) const MAX_REAPERS: usize = 4;

/// A thread busy on one teardown this long is presumed stuck in the kernel.
pub(crate) const STUCK: Duration = Duration::from_secs(5);

/// Least time between two teardowns on one thread.
pub(crate) const PACE: Duration = Duration::from_millis(2);

const PENDING: u8 = 0;

const DONE: u8 = 1;

/// One submitted teardown's outcome, shared with the reaper thread.
struct Slot {
    state: AtomicU8,

    /// The job's result bits, valid once `state` is `DONE`.
    result: AtomicU64,

    /// Woken once the result is stored.
    waker: Option<Waker>,
}

/// The owner's handle on a submitted teardown.
pub(crate) struct Ticket(Arc<Slot>);

impl Ticket {
    /// The job's result once it ran, else `None`.
    pub(crate) fn result(&self) -> Option<u64> {
        (self.0.state.load(Ordering::Acquire) == DONE)
            .then(|| self.0.result.load(Ordering::Relaxed))
    }
}

type Job = Box<dyn FnOnce() -> u64 + Send>;

/// One reaper thread's queue and how long its current job has run.
struct Worker {
    queue: SyncSender<(Job, Arc<Slot>)>,

    /// Nanoseconds since [`Reaper::epoch`] when the running job started; 0
    /// while idle.
    busy_since: Arc<AtomicU64>,
}

/// A small pool of reaper threads. See the module docs.
pub(crate) struct Reaper {
    workers: Mutex<Vec<Worker>>,

    epoch: Instant,

    queue: usize,

    max: usize,

    stuck: Duration,

    pace: Duration,
}

impl Reaper {
    /// A reaper with explicit limits, for tests.
    pub(crate) fn new(queue: usize, max: usize, stuck: Duration, pace: Duration) -> Self {
        Self {
            workers: Mutex::new(Vec::new()),
            epoch: Instant::now(),
            queue: queue.max(1),
            max: max.max(1),
            stuck,
            pace,
        }
    }

    /// The process-wide reaper.
    pub(crate) fn global() -> &'static Self {
        static REAPER: OnceLock<Reaper> = OnceLock::new();
        REAPER.get_or_init(|| Self::new(QUEUE, MAX_REAPERS, STUCK, PACE))
    }

    fn now(&self) -> u64 {
        // At least 1: 0 means idle.
        (self.epoch.elapsed().as_nanos() as u64).max(1)
    }

    /// Run `job` on a reaper thread and wake `waker` when it finished.
    /// `None` when every thread is full or stuck and no more may start: the
    /// caller keeps everything and retries later.
    pub(crate) fn submit(
        &self,
        job: impl FnOnce() -> u64 + Send + 'static,
        waker: Option<Waker>,
    ) -> Option<Ticket> {
        let slot = Arc::new(Slot {
            state: AtomicU8::new(PENDING),
            result: AtomicU64::new(0),
            waker,
        });
        let mut job: Job = Box::new(job);
        let mut workers = self.workers.lock().unwrap_or_else(PoisonError::into_inner);
        let now = self.now();
        let stuck = self.stuck.as_nanos() as u64;
        let mut healthy = false;
        for worker in workers.iter() {
            let since = worker.busy_since.load(Ordering::Acquire);
            if since != 0 && now.saturating_sub(since) >= stuck {
                continue;
            }
            healthy = true;
            match worker.queue.try_send((job, slot.clone())) {
                Ok(()) => return Some(Ticket(slot)),
                Err(TrySendError::Full((back, _)) | TrySendError::Disconnected((back, _))) => {
                    job = back;
                }
            }
        }
        // A healthy thread that is merely full keeps the pace: retry later.
        // Only stuck threads justify another one.
        if healthy || workers.len() >= self.max {
            return None;
        }
        let (queue, receiver) = sync_channel(self.queue);
        let busy_since = Arc::new(AtomicU64::new(0));
        let thread = {
            let busy_since = busy_since.clone();
            let epoch = self.epoch;
            let pace = self.pace;
            std::thread::Builder::new()
                .name("racer-rdma-reaper".into())
                .spawn(move || run(&receiver, &busy_since, epoch, pace))
        };
        if thread.is_err() {
            return None;
        }
        // A fresh queue has room.
        queue.try_send((job, slot.clone())).ok()?;
        workers.push(Worker { queue, busy_since });
        Some(Ticket(slot))
    }
}

/// A reaper thread: run each job, publish its result, wake its owner.
fn run(
    receiver: &Receiver<(Job, Arc<Slot>)>,
    busy_since: &AtomicU64,
    epoch: Instant,
    pace: Duration,
) {
    while let Ok((job, slot)) = receiver.recv() {
        busy_since.store(
            (epoch.elapsed().as_nanos() as u64).max(1),
            Ordering::Release,
        );
        let result = job();
        busy_since.store(0, Ordering::Release);
        slot.result.store(result, Ordering::Relaxed);
        slot.state.store(DONE, Ordering::Release);
        if let Some(waker) = &slot.waker {
            waker.wake_by_ref();
        }
        if !pace.is_zero() {
            std::thread::sleep(pace);
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use std::sync::{
        Barrier,
        atomic::{AtomicBool, AtomicUsize},
    };

    fn wait(ticket: &Ticket) -> u64 {
        let deadline = Instant::now() + Duration::from_secs(10);
        loop {
            if let Some(result) = ticket.result() {
                return result;
            }
            assert!(Instant::now() < deadline, "teardown never finished");
            std::thread::sleep(Duration::from_millis(1));
        }
    }

    struct Flag(AtomicBool);

    impl std::task::Wake for Flag {
        fn wake(self: Arc<Self>) {
            self.0.store(true, Ordering::Release);
        }
    }

    #[test]
    /// A job runs off the calling thread, its result is published, and the
    /// waker fires.
    fn runs_job_and_wakes() {
        let reaper = Reaper::new(4, 2, Duration::from_secs(5), Duration::ZERO);
        let flag = Arc::new(Flag(AtomicBool::new(false)));
        let caller = std::thread::current().id();
        let ticket = reaper
            .submit(
                move || u64::from(std::thread::current().id() != caller) + 41,
                Some(Waker::from(flag.clone())),
            )
            .unwrap();
        assert_eq!(wait(&ticket), 42);
        assert!(flag.0.load(Ordering::Acquire));
    }

    #[test]
    /// A job blocked in the "kernel" never blocks the submitter: once it
    /// counts as stuck, new jobs go to a fresh thread, and once every thread
    /// is full or stuck, submission refuses instead of waiting.
    fn stuck_job_is_bypassed_and_handoff_is_bounded() {
        let reaper = Reaper::new(1, 2, Duration::from_millis(20), Duration::ZERO);
        let release = Arc::new(Barrier::new(2));
        let hung = {
            let release = release.clone();
            reaper
                .submit(
                    move || {
                        release.wait();
                        7
                    },
                    None,
                )
                .unwrap()
        };
        std::thread::sleep(Duration::from_millis(50));
        assert_eq!(hung.result(), None);
        // The first thread is stuck: this goes to a second thread.
        let ran = Arc::new(AtomicUsize::new(0));
        let next = {
            let ran = ran.clone();
            reaper
                .submit(
                    move || {
                        ran.fetch_add(1, Ordering::AcqRel);
                        1
                    },
                    None,
                )
                .unwrap()
        };
        assert_eq!(wait(&next), 1);
        // Block the second thread too, and fill its queue of one.
        let second = Arc::new(Barrier::new(2));
        let blocked = {
            let second = second.clone();
            reaper
                .submit(
                    move || {
                        second.wait();
                        2
                    },
                    None,
                )
                .unwrap()
        };
        let mut queued = None;
        let start = Instant::now();
        while queued.is_none() {
            queued = reaper.submit(|| 3, None);
            assert!(start.elapsed() < Duration::from_secs(5));
        }
        // Both threads busy (one stuck, one full): refuse, at once.
        let refused = Instant::now();
        assert!(reaper.submit(|| 4, None).is_none());
        assert!(refused.elapsed() < Duration::from_secs(1));
        second.wait();
        assert_eq!(wait(&blocked), 2);
        assert_eq!(wait(queued.as_ref().unwrap()), 3);
        release.wait();
        assert_eq!(wait(&hung), 7);
        assert_eq!(ran.load(Ordering::Acquire), 1);
    }

    #[test]
    /// Jobs on one thread run in order, at most one per pace interval.
    fn jobs_are_paced() {
        let pace = Duration::from_millis(10);
        let reaper = Reaper::new(16, 1, Duration::from_secs(5), pace);
        let start = Instant::now();
        let tickets: Vec<Ticket> = (0..5u64)
            .map(|i| reaper.submit(move || i, None).unwrap())
            .collect();
        for (i, ticket) in tickets.iter().enumerate() {
            assert_eq!(wait(ticket), i as u64);
        }
        assert!(start.elapsed() >= pace * 4);
    }
}
