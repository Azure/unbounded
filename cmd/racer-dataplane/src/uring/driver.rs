// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Composite scheduling and external-source arm/recheck/sleep ordering.
use super::*;
/// Outcome of a bounded application/source batch. `runnable` includes budget
/// exhaustion; deadlines use the monotonic clock and are combined by minimum.
#[derive(Clone, Copy, Debug, Default)]
pub struct Work {
    pub runnable: bool,
    pub deadline: Option<Instant>,
}
impl Work {
    pub fn merge(&mut self, other: Self) {
        self.runnable |= other.runnable;
        self.deadline = match (self.deadline, other.deadline) {
            (Some(a), Some(b)) => Some(a.min(b)),
            (a, b) => a.or(b),
        };
    }
}

/// Application scheduler on the pinned worker. Poll ready tasks, inspect typed
/// tickets, and queue I/O up to `budget`; report runnable on budget exhaustion.
/// Task wakers can be made with `std::task::Waker::from(ring.wake_handle())`.
pub trait Application {
    fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work>;
    fn begin_drain(&mut self) {}
    fn drained(&self) -> bool {
        true
    }
    /// Stop admission and drop/cancel application tickets. The driver subsequently
    /// drains the ring; application-owned raw I/O needs its own safe teardown.
    fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()>;
}

/// An independently owned external completion source, e.g. an RDMA RNIC.
///
/// For rdma-core, `poll` must fairly poll all owned CQs, drain nonblocking
/// completion-channel events and acknowledge each event, and service async
/// events. `arm` calls ibv_req_notify_cq and installs a one-shot [`Ring::poll_fd`]
/// on an owned/duplicated channel descriptor. The driver then polls again before
/// sleeping. FD readiness is only a prompt to poll the CQ; it is not a work
/// completion. Keep CQ notification arming separate from FD poll rearming.
/// Unsignaled remote writes need an explicit protocol notification to wake a CPU.
///
/// One owner per CQ/channel; multiple sources/RNICs per worker are supported.
/// This is a safe scheduling trait, not a memory-safety contract. Implementations
/// must retain each MR's MemoryLease and each in-flight Fill/Buffer until the NIC
/// is proven quiescent, even if shutdown is skipped, fails, or panics. The ring's
/// cancellation and deregistration cannot prove NIC quiescence.
pub trait CompletionSource {
    fn poll(&mut self, ring: &mut Ring, budget: usize) -> io::Result<Work>;
    fn arm(&mut self, ring: &mut Ring) -> io::Result<()>;
    fn shutdown(&mut self, ring: &mut Ring) -> io::Result<()>;
}

/// Composite worker driver: bounded round-robin sources, application scheduling,
/// notification arm/recheck, then exactly one io_uring sleep decision.
///
/// ```no_run
/// use racer_dataplane::{buffers, uring, workers};
/// use std::{io, num::NonZeroUsize, sync::Arc};
/// struct App;
/// impl uring::Application for App {
///     fn poll(&mut self, _: &mut uring::Ring, _: usize) -> io::Result<uring::Work> {
///         Ok(uring::Work::default())
///     }
///     fn shutdown(&mut self, _: &mut uring::Ring) -> io::Result<()> { Ok(()) }
/// }
/// # fn start() -> io::Result<()> {
/// let pools = Arc::new(buffers::Pools::new(buffers::Config::new(
///     NonZeroUsize::new(32).unwrap(),
/// )));
/// let workers = workers::Workers::start(workers::Config::default(), move |placement| {
///     let pool = pools.for_worker(placement)?;
///     let ring = uring::Ring::new(placement, pool, uring::Config::default())?;
///     uring::Driver::new(ring, App, 128)
/// })?;
/// # drop(workers);
/// # Ok(()) }
/// ```
pub struct Driver<A: Application> {
    lifecycle: Option<(Arc<crate::lifecycle::Lifecycle>, usize)>,
    metrics_deadline: Option<Instant>,
    ring: Ring,
    application: A,
    sources: Vec<Box<dyn CompletionSource>>,
    first: usize,
    budget: usize,
    stopped: bool,
    quiesced: bool,
}
impl<A: Application> Driver<A> {
    pub fn new(ring: Ring, application: A, budget: usize) -> io::Result<Self> {
        if budget == 0 {
            return Err(invalid("driver budget must be nonzero"));
        }
        Ok(Self {
            lifecycle: None,
            ring,
            metrics_deadline: None,
            application,
            sources: Vec::new(),
            first: 0,
            budget,
            stopped: false,
            quiesced: false,
        })
    }
    /// Setup-time registration on the owning worker.
    pub fn with_lifecycle(mut self, life: Arc<crate::lifecycle::Lifecycle>, worker: usize) -> Self {
        self.lifecycle = Some((life, worker));
        self
    }
    pub fn add_source(&mut self, source: impl CompletionSource + 'static) {
        self.sources.push(Box::new(source));
    }
    fn poll(&mut self) -> io::Result<Work> {
        let mut work = Work {
            runnable: self.ring.progress()?,
            deadline: None,
        };
        for offset in 0..self.sources.len() {
            let index = (self.first + offset) % self.sources.len();
            work.merge(self.sources[index].poll(&mut self.ring, self.budget)?);
        }
        work.merge(self.application.poll(&mut self.ring, self.budget)?);
        work.merge(Work {
            runnable: false,
            deadline: self.ring.slab_deadline(),
        });
        work.merge(self.ring.metrics.poll(&mut self.metrics_deadline));
        if let Some((life, worker)) = &self.lifecycle {
            life.progress(*worker);
            work.merge(Work {
                runnable: false,
                deadline: Some(crate::environment::now() + crate::lifecycle::HEARTBEAT),
            });
        }
        Ok(work)
    }
}
impl<A: Application> workers::Driver for Driver<A> {
    type Wake = Wake;
    fn begin_drain(&mut self) {
        self.application.begin_drain();
    }
    fn drained(&self) -> bool {
        self.application.drained()
    }
    fn wake_handle(&self) -> Arc<Wake> {
        self.ring.wake_handle()
    }
    fn turn(&mut self) -> io::Result<()> {
        if self.stopped {
            return Err(invalid("driver stopped"));
        }
        let mut work = self.poll()?;
        if !self.sources.is_empty() {
            self.first = (self.first + 1) % self.sources.len();
        }
        if work.runnable {
            return Ok(());
        }
        for source in &mut self.sources {
            source.arm(&mut self.ring)?;
        }
        work.merge(self.poll()?);
        if !work.runnable {
            self.ring.wait(work.deadline)?;
        }
        Ok(())
    }
    fn shutdown(&mut self) -> io::Result<()> {
        if self.quiesced {
            return Ok(());
        }
        self.stopped = true;
        let mut error = self.application.shutdown(&mut self.ring).err();
        for source in &mut self.sources {
            if let Err(e) = source.shutdown(&mut self.ring) {
                error.get_or_insert(e);
            }
        }
        if let Err(e) = self.ring.shutdown() {
            error.get_or_insert(e);
        }
        self.ring.metrics.publish();
        match error {
            Some(e) => Err(e),
            None => {
                self.quiesced = true;
                Ok(())
            }
        }
    }
}

#[cfg(test)]
impl<A: Application> Driver<A> {
    pub(crate) fn parts_mut(&mut self) -> (&mut A, &mut Ring) {
        (&mut self.application, &mut self.ring)
    }
    pub(crate) fn application(&self) -> &A {
        &self.application
    }
}
