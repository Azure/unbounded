//! Owned page-AEAD handoff between one I/O shard and its crypto engine endpoint.
//! Multiple endpoints can share a NUMA-local crypto execution thread.
//!
//! Queue ownership and completion admission are independent of waiting futures.
//! Admission stays on I/O. Reserve a job slot AND its eventual completion slot
//! before enqueueing. The permit follows the job through completion consumption,
//! so cancellation, an abandoned future, or shutdown cannot reclaim its capacity,
//! buffers, or key early. Completion publication never waits for new admission.
//!
//! All polling must register the waker and recheck state before returning Pending.
//! Enqueue wakes crypto; completion wakes I/O; consumption wakes capacity waiters;
//! close wakes both sides. Neither side spins or blocks awaiting the other. This
//! is required even when both OS threads share one allowed CPU.
//!
//! Queue timing measures accepted submission to crypto dequeue, excluding permit
//! waits and execution. I/O records each measured execution once on completion
//! reap, including failed, canceled, or abandoned work reaching that path. Queued
//! or in-flight work and engine loss without completion are not yet counted.
//! The racer_crypto_{encrypt,decrypt}_queue_nanoseconds_{sum,count} counters yield
//! mean wait in milliseconds as rate(sum) / rate(count) / 1e6. For a fleet mean,
//! sum each rate across instances before dividing. Zero count rate has no defined
//! mean; these counters provide neither percentiles nor current queue age or total
//! request latency.

#[cfg(test)]
#[path = "crypto_measurement.rs"]
mod measurement;

use super::{
    admission::Reservation,
    channel::{self, Receiver, SendFailure, Sender},
    deadline::RequestScope,
};
use crate::{
    error::{Error, Operation, Result},
    memory::pool::{CiphertextPage, PlaintextBuffer, VerifiedPage},
    model::{PageId, WorkerId},
    security::keyring::KeyLease,
};
use std::{
    cell::{Cell, RefCell},
    collections::{BTreeMap, VecDeque},
    num::NonZeroUsize,
    sync::{
        Arc,
        atomic::{AtomicBool, AtomicUsize, Ordering},
    },
    task::{Context, Poll, Waker},
};

/// I/O-generated identity, independent of flights. Never reuse a sequence within
/// a pair generation; restart increments the generation and rejects late results.
#[derive(Clone, Copy, Debug, Eq, Hash, PartialEq)]
pub struct CryptoId {
    pub worker: WorkerId,
    pub generation: u64,
    pub sequence: u64,
}
impl Ord for CryptoId {
    fn cmp(&self, other: &Self) -> std::cmp::Ordering {
        (self.worker.0, self.generation, self.sequence).cmp(&(
            other.worker.0,
            other.generation,
            other.sequence,
        ))
    }
}
impl PartialOrd for CryptoId {
    fn partial_cmp(&self, other: &Self) -> Option<std::cmp::Ordering> {
        Some(self.cmp(other))
    }
}

/// Output capacity was admitted on I/O. The engine cannot reach Admission,
/// flights, metadata catalogs, storage, credentials, or any Rc service graph.
pub enum CryptoInput {
    Decrypt {
        ciphertext: CiphertextPage,
        plaintext: Reservation,
    },
    Encrypt {
        page: PageId,
        plaintext: PlaintextBuffer,
        ciphertext: Reservation,
    },
}

pub enum CryptoOutput {
    /// Retain original ciphertext through completion too; I/O decides whether to
    /// retain it for peer copies/persistence after consuming the completion.
    Decrypted(VerifiedPage, CiphertextPage),
    Encrypted(VerifiedPage, CiphertextPage),
}

/// Composition descriptor shared only by the two endpoints and their permits.
/// A future backend must bound queued + executing + unconsumed completions by
/// capacity, not merely bound the number of jobs waiting to execute.
struct Handoff {
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
    outstanding: AtomicUsize,
    closed: AtomicBool,
    capacity_waker: futures::task::AtomicWaker,
    engine_waker: futures::task::AtomicWaker,
    io_waker: futures::task::AtomicWaker,
}

/// Non-cloneable, pair-bound reservation of both submission and completion space.
/// Only the I/O endpoint can mint it. Failed submission returns the entire job.
/// A capacity reservation cannot be duplicated:
/// ```compile_fail
/// use racer_dataplane::runtime::crypto::CryptoPermit;
/// fn duplicate(permit: CryptoPermit) { let _second = permit.clone(); }
/// ```
pub struct CryptoPermit {
    handoff: Arc<Handoff>,
    id: CryptoId,
    measurement: Measurement,
}

#[derive(Default)]
struct Measurement {
    // Benchmark control only. No production field, configuration, or runtime knob.
    #[cfg(test)]
    disabled: bool,
    decrypt: bool,
    bytes: u64,
    submitted: Option<std::time::Instant>,
    queue_ns: Option<u64>,
    execution_ns: Option<u64>,
}

fn elapsed_ns(start: std::time::Instant) -> u64 {
    super::environment::now()
        .saturating_duration_since(start)
        .as_nanos()
        .min(u128::from(u64::MAX)) as u64
}

impl CryptoPermit {
    fn measurement_enabled(&self) -> bool {
        #[cfg(test)]
        return !self.measurement.disabled;
        #[cfg(not(test))]
        true
    }

    pub(crate) fn execution_start(&self) -> Option<std::time::Instant> {
        self.measurement_enabled().then(super::environment::now)
    }

    pub(crate) fn executed(&mut self, start: std::time::Instant) {
        self.measurement.execution_ns = Some(elapsed_ns(start));
    }
}

impl CryptoCompletion {
    /// I/O is the only counter writer. Aggregate once on dequeue, even when the
    /// waiter is abandoned. Started means execution observed through completion;
    /// in-flight work and engine loss without a completion are not yet counted.
    fn record(&self, metrics: &crate::telemetry::metrics::Metrics) {
        use crate::telemetry::metrics::Event::*;
        let m = &self.permit.measurement;
        let Some(execution) = m.execution_ns else {
            return;
        };
        let events = if m.decrypt {
            [
                CryptoDecryptStarted,
                CryptoDecryptSuccess,
                CryptoDecryptFailure,
                CryptoDecryptBytes,
                CryptoDecryptExecutionCount,
                CryptoDecryptExecutionNs,
                CryptoDecryptQueueCount,
                CryptoDecryptQueueNs,
            ]
        } else {
            [
                CryptoEncryptStarted,
                CryptoEncryptSuccess,
                CryptoEncryptFailure,
                CryptoEncryptBytes,
                CryptoEncryptExecutionCount,
                CryptoEncryptExecutionNs,
                CryptoEncryptQueueCount,
                CryptoEncryptQueueNs,
            ]
        };
        let success = matches!(self.outcome, CryptoOutcome::Completed(_));
        let amounts = [
            1,
            u64::from(success),
            u64::from(!success),
            if success { m.bytes } else { 0 },
            1,
            execution,
            u64::from(m.queue_ns.is_some()),
            m.queue_ns.unwrap_or(0),
        ];
        for (event, amount) in events.into_iter().zip(amounts) {
            let _ = metrics.record(event, amount);
        }
    }
}
impl Drop for CryptoPermit {
    fn drop(&mut self) {
        self.handoff.outstanding.fetch_sub(1, Ordering::AcqRel);
        self.handoff.capacity_waker.wake();
    }
}

pub struct CryptoJob {
    pub(crate) permit: CryptoPermit,
    pub(crate) input: CryptoInput,
    pub(crate) key: KeyLease,
    pub(crate) scope: RequestScope,
}

impl CryptoPermit {
    pub fn job(mut self, input: CryptoInput, key: KeyLease, scope: RequestScope) -> CryptoJob {
        use super::reactor::IoBuffer;
        if self.measurement_enabled() {
            self.measurement.decrypt = matches!(input, CryptoInput::Decrypt { .. });
            self.measurement.bytes = match &input {
                CryptoInput::Encrypt { plaintext, .. } => {
                    plaintext.bytes().map_or(0, |b| b.len() as u64)
                }
                CryptoInput::Decrypt { ciphertext, .. } => {
                    u64::from(ciphertext.envelope().plaintext_length)
                }
            };
        }
        CryptoJob {
            permit: self,
            input,
            key,
            scope,
        }
    }
}

impl CryptoJob {
    pub fn id(&self) -> CryptoId {
        self.permit.id
    }
}

/// Failed/canceled work returns its input allocations and reservations intact.
/// Successful work transfers those reservations to the output page leases.
pub enum CryptoOutcome {
    Completed(CryptoOutput),
    Failed { input: CryptoInput, error: Error },
}

/// Only the engine may create a completion after it has stopped accessing input.
/// The key and permit survive success AND failure until I/O consumes the result.
pub struct CryptoCompletion {
    pub(crate) permit: CryptoPermit,
    pub(crate) outcome: CryptoOutcome,
    pub(crate) key: KeyLease,
    pub(crate) scope: RequestScope,
}

impl CryptoCompletion {
    pub fn id(&self) -> CryptoId {
        self.permit.id
    }
}

/// Endpoints move to their respective threads before constructing local services.
/// They are not Clone: there is exactly one producer/consumer in each direction.
pub struct IoCryptoPort {
    #[cfg(test)]
    attribution_disabled: bool,
    handoff: Arc<Handoff>,
    jobs: Sender<CryptoJob>,
    completions: RefCell<Receiver<CryptoCompletion>>,
    last_sequence: Cell<Option<u64>>,
}
pub struct CryptoPort {
    handoff: Arc<Handoff>,
    jobs: Option<Receiver<CryptoJob>>,
    completions: Sender<CryptoCompletion>,
}

/// Allocate fixed-capacity handoffs, without starting either service.
pub fn pair(
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
) -> (IoCryptoPort, CryptoPort) {
    try_pair(worker, generation, capacity).expect("validated crypto queue capacity")
}

/// Fallible allocation for operational startup and untrusted configuration.
pub fn try_pair(
    worker: WorkerId,
    generation: u64,
    capacity: NonZeroUsize,
) -> Result<(IoCryptoPort, CryptoPort)> {
    let (jobs_tx, jobs_rx) = channel::bounded(capacity.get())?;
    let (results_tx, results_rx) = channel::bounded(capacity.get())?;
    let handoff = Arc::new(Handoff {
        worker,
        generation,
        capacity,
        outstanding: AtomicUsize::new(0),
        closed: AtomicBool::new(false),
        capacity_waker: futures::task::AtomicWaker::new(),
        engine_waker: futures::task::AtomicWaker::new(),
        io_waker: futures::task::AtomicWaker::new(),
    });
    Ok((
        IoCryptoPort {
            #[cfg(test)]
            attribution_disabled: false,
            handoff: handoff.clone(),
            jobs: jobs_tx,
            completions: RefCell::new(results_rx),
            last_sequence: Cell::new(None),
        },
        CryptoPort {
            handoff,
            jobs: Some(jobs_rx),
            completions: results_tx,
        },
    ))
}

impl IoCryptoPort {
    /// Atomically reserve both directions. Saturation parks with a wakeup rather
    /// than waiting while holding a job-only slot. Reject wrong-worker/stale IDs.
    pub fn poll_reserve(&self, cx: &mut Context<'_>, id: CryptoId) -> Poll<Result<CryptoPermit>> {
        self.handoff.capacity_waker.register(cx.waker());
        if self.handoff.closed.load(Ordering::Acquire) {
            return Poll::Ready(Err(Error::Unavailable));
        }
        if id.worker != self.handoff.worker
            || id.generation != self.handoff.generation
            || self
                .last_sequence
                .get()
                .is_some_and(|last| id.sequence <= last)
        {
            return Poll::Ready(Err(Error::StaleFlight));
        }
        if self
            .handoff
            .outstanding
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |n| {
                (n < self.handoff.capacity.get()).then_some(n + 1)
            })
            .is_err()
        {
            return Poll::Pending;
        }
        self.last_sequence.set(Some(id.sequence));
        Poll::Ready(Ok(CryptoPermit {
            handoff: self.handoff.clone(),
            id,
            measurement: Measurement {
                #[cfg(test)]
                disabled: self.attribution_disabled,
                ..Measurement::default()
            },
        }))
    }

    /// Validate the permit belongs to this pair. Closed or rejected submission
    /// returns all ownership; an accepted job lives independently of its waiter.
    pub fn try_submit(
        &self,
        mut job: CryptoJob,
    ) -> std::result::Result<(), SendFailure<CryptoJob>> {
        if !Arc::ptr_eq(&self.handoff, &job.permit.handoff)
            || self.handoff.closed.load(Ordering::Acquire)
        {
            return Err(SendFailure {
                command: job,
                error: Error::Unavailable,
            });
        }
        // Capture at publication attempt, not permit reservation/admission. A
        // rejected attempt never reaches dequeue and is overwritten on retry.
        if job.permit.measurement_enabled() {
            job.permit.measurement.submitted = Some(super::environment::now());
        }
        self.jobs.try_send(job)?;
        self.handoff.engine_waker.wake();
        Ok(())
    }

    /// Drain even abandoned/stale completions before returning their credits.
    pub fn poll_completion(&self, cx: &mut Context<'_>) -> Poll<Result<Option<CryptoCompletion>>> {
        self.completions.borrow_mut().poll_receive(cx)
    }

    /// Refuse new reservations/submissions, but keep completions available.
    pub fn close_submissions(&self) -> Result<()> {
        self.handoff.closed.store(true, Ordering::Release);
        self.jobs.close();
        self.handoff.engine_waker.wake();
        self.handoff.capacity_waker.wake();
        Ok(())
    }
}

impl CryptoPort {
    /// Worker-level wake registration independent of per-operation polling.
    pub fn register_driver(&self, waker: &Waker) {
        self.handoff.engine_waker.register(waker);
    }
    /// None means closed and all accepted jobs consumed, not temporarily empty.
    pub fn poll_job(&mut self, cx: &mut Context<'_>) -> Poll<Result<Option<CryptoJob>>> {
        let result = self
            .jobs
            .as_mut()
            .expect("live engine endpoint")
            .poll_receive(cx);
        match result {
            Poll::Ready(Ok(Some(mut job))) => {
                if job.permit.measurement_enabled() {
                    job.permit.measurement.queue_ns =
                        job.permit.measurement.submitted.map(elapsed_ns);
                }
                Poll::Ready(Ok(Some(job)))
            }
            other => other,
        }
    }

    /// Uses the job's reserved completion slot, including during drain. A backend
    /// failure returns ownership to the engine for retry/fenced teardown.
    pub fn complete(
        &mut self,
        completion: CryptoCompletion,
    ) -> std::result::Result<(), SendFailure<CryptoCompletion>> {
        if !Arc::ptr_eq(&self.handoff, &completion.permit.handoff) {
            return Err(SendFailure {
                command: completion,
                error: Error::StaleFlight,
            });
        }
        self.completions.try_send(completion)?;
        self.handoff.io_waker.wake();
        Ok(())
    }
}
impl Drop for CryptoPort {
    fn drop(&mut self) {
        // No engine accesses queued jobs. Closing prevents further submissions;
        // drain queued ownership on this unique consumer before publishing EOF.
        self.handoff.closed.store(true, Ordering::Release);
        if let Some(mut jobs) = self.jobs.take() {
            while let Ok(Some(job)) = jobs.receive() {
                drop(job);
            }
        }
        self.completions.close();
        self.handoff.io_waker.wake();
        self.handoff.capacity_waker.wake();
    }
}

/// Local submission facade, driven by WorkerService, not by the waiting future.
/// The bounded waiter table outlives canceled futures. Future drop abandons only
/// delivery; the engine/queue retains resources until I/O reaps the completion.
/// I/O checks generation/sequence before delivery and never publishes stale work.
pub struct CryptoClient {
    port: IoCryptoPort,
    metrics: RefCell<Option<crate::telemetry::metrics::Metrics>>,
    waiters: RefCell<BTreeMap<CryptoId, Waiter>>,
    sequence: Cell<u64>,
    pending: RefCell<VecDeque<(CryptoId, Waker, RequestScope)>>,
    deadline_cursor: Cell<Option<CryptoId>>,
    pending_cursor: Cell<usize>,
    drain_waiter: RefCell<Option<(RequestScope, Waker)>>,
}
struct Waiter {
    waker: Waker,
    abandoned: bool,
    result: Option<CryptoCompletion>,
    scope: RequestScope,
}
struct Registration<'a> {
    client: &'a CryptoClient,
    id: CryptoId,
}
struct CapacityWaiter<'a> {
    client: &'a CryptoClient,
    id: CryptoId,
}
impl Drop for CapacityWaiter<'_> {
    fn drop(&mut self) {
        let wake = {
            let mut pending = self.client.pending.borrow_mut();
            pending.retain(|(id, _, _)| *id != self.id);
            pending.front().map(|(_, waker, _)| waker.clone())
        };
        if let Some(waker) = wake {
            waker.wake();
        }
    }
}
impl Drop for Registration<'_> {
    fn drop(&mut self) {
        let mut waiters = self.client.waiters.borrow_mut();
        if let Some(waiter) = waiters.get_mut(&self.id) {
            if waiter.result.is_some() {
                waiters.remove(&self.id);
            } else {
                waiter.abandoned = true;
            }
        }
    }
}

impl CryptoClient {
    pub fn new(port: IoCryptoPort) -> Self {
        Self {
            port,
            metrics: RefCell::new(None),
            waiters: RefCell::new(BTreeMap::new()),
            sequence: Cell::new(0),
            pending: RefCell::new(VecDeque::new()),
            deadline_cursor: Cell::new(None),
            pending_cursor: Cell::new(0),
            drain_waiter: RefCell::new(None),
        }
    }

    /// Install the I/O writer before admitting work; crypto never writes this shard.
    pub(crate) fn set_metrics(&self, metrics: crate::telemetry::metrics::Metrics) {
        *self.metrics.borrow_mut() = Some(metrics);
    }

    /// Allocate a unique ID in this pair's generation, reserve both queue slots,
    /// clone the original scope into the owned job, then submit. On rejection,
    /// retain ownership for a wakeable retry or release locally before acceptance.
    /// Record the waiter before enqueue so even immediate completion cannot race
    /// registration. Sequence overflow must drain/restart, never wrap in place.
    pub fn execute<'a>(
        &'a self,
        input: CryptoInput,
        key: KeyLease,
        scope: &'a RequestScope,
    ) -> Operation<'a, CryptoOutput> {
        Box::pin(async move {
            scope.check()?;
            let cancellation = scope.cancellation.subscribe()?;
            if self.pending.borrow().len() >= self.port.handoff.capacity.get() {
                return Err(Error::Overloaded);
            }
            let sequence = self
                .sequence
                .get()
                .checked_add(1)
                .ok_or(Error::Unavailable)?;
            self.sequence.set(sequence);
            let id = CryptoId {
                worker: self.port.handoff.worker,
                generation: self.port.handoff.generation,
                sequence,
            };
            let capacity_waiter = CapacityWaiter { client: self, id };
            let permit = futures::future::poll_fn(|cx| {
                cancellation.register(cx.waker());
                scope.check()?;
                let mut pending = self.pending.borrow_mut();
                if let Some((_, waker, _)) = pending
                    .iter_mut()
                    .find(|(candidate, _, _)| *candidate == id)
                {
                    *waker = cx.waker().clone();
                } else {
                    pending.push_back((id, cx.waker().clone(), scope.clone()));
                }
                if pending
                    .front()
                    .is_some_and(|(candidate, _, _)| *candidate != id)
                {
                    return Poll::Pending;
                }
                drop(pending);
                self.port.poll_reserve(cx, id)
            })
            .await?;
            drop(capacity_waiter);
            let mut job = Some(permit.job(input, key, scope.clone()));
            futures::future::poll_fn(|cx| {
                self.waiters.borrow_mut().insert(
                    id,
                    Waiter {
                        waker: cx.waker().clone(),
                        abandoned: false,
                        result: None,
                        scope: scope.clone(),
                    },
                );
                Poll::Ready(())
            })
            .await;
            let registration = Registration { client: self, id };
            if let Err(failure) = self.port.try_submit(job.take().unwrap()) {
                self.waiters.borrow_mut().remove(&id);
                return Err(failure.error);
            }
            let result = futures::future::poll_fn(|cx| {
                // After acceptance, returning is itself a completion fence for
                // the retained read driver. Cancellation changes delivery, not
                // the point at which a replacement acquisition may be elected.
                let mut waiters = self.waiters.borrow_mut();
                let Some(waiter) = waiters.get_mut(&id) else {
                    return Poll::Ready(Err(Error::Cancelled));
                };
                waiter.waker = cx.waker().clone();
                if let Some(completion) = waiter.result.take() {
                    let cancelled = waiter.abandoned;
                    waiters.remove(&id);
                    if cancelled {
                        return Poll::Ready(Err(Error::Cancelled));
                    }
                    if let Err(error) = scope.check() {
                        return Poll::Ready(Err(error));
                    }
                    Poll::Ready(match completion.outcome {
                        CryptoOutcome::Completed(output) => Ok(output),
                        CryptoOutcome::Failed { error, .. } => Err(error),
                    })
                } else {
                    if self.port.completions.borrow().is_closed() && self.outstanding() == 0 {
                        Poll::Ready(Err(Error::Unavailable))
                    } else {
                        Poll::Pending
                    }
                }
            })
            .await;
            drop(registration);
            result
        })
    }

    /// Called by I/O even when no user futures remain, before admitting more work.
    pub fn poll_budgeted(&self, work_budget: usize) -> Result<()> {
        for _ in 0..work_budget {
            let completion = self.port.completions.borrow_mut().receive()?;
            let Some(completion) = completion else {
                break;
            };
            if completion.permit.measurement_enabled() {
                if let Some(metrics) = self.metrics.borrow().as_ref() {
                    completion.record(metrics);
                }
            }
            let id = completion.id();
            let wake = {
                let mut waiters = self.waiters.borrow_mut();
                if let Some(waiter) = waiters.get_mut(&id) {
                    if waiter.abandoned {
                        waiters.remove(&id).map(|waiter| waiter.waker)
                    } else {
                        let wake = waiter.waker.clone();
                        waiter.result = Some(completion);
                        Some(wake)
                    }
                } else {
                    None
                }
            };
            if let Some(wake) = wake {
                wake.wake();
            }
        }
        if self.port.completions.borrow().is_closed() {
            self.port.jobs.discard_closed();
            if self.outstanding() == 0 {
                let abandoned: Vec<_> = self
                    .waiters
                    .borrow()
                    .iter()
                    .filter(|(_, waiter)| waiter.abandoned)
                    .take(work_budget)
                    .map(|(id, _)| *id)
                    .collect();
                for id in abandoned {
                    self.waiters.borrow_mut().remove(&id);
                }
            }
        }
        // All waits have bounded original deadlines. The worker drives this even
        // without external I/O, allowing expired/canceled futures to detach.
        let wakes: Vec<_> = {
            use std::ops::Bound::{Excluded, Unbounded};
            let waiters = self.waiters.borrow();
            let start = self.deadline_cursor.get().map_or(Unbounded, Excluded);
            let mut wakes = Vec::new();
            for (id, waiter) in waiters
                .range((start, Unbounded))
                .chain(waiters.iter())
                .take(work_budget.min(waiters.len()))
            {
                self.deadline_cursor.set(Some(*id));
                if !waiter.abandoned && self.port.completions.borrow().is_closed() {
                    wakes.push(waiter.waker.clone());
                }
            }
            wakes
        };
        for waker in wakes {
            waker.wake();
        }
        let expired: Vec<_> = {
            let pending = self.pending.borrow();
            let mut wakes = Vec::new();
            for _ in 0..work_budget.min(pending.len()) {
                let index = self.pending_cursor.get() % pending.len();
                self.pending_cursor.set(index + 1);
                let (_, waker, scope) = &pending[index];
                if scope.check().is_err() || self.port.handoff.closed.load(Ordering::Acquire) {
                    wakes.push(waker.clone());
                }
            }
            wakes
        };
        for waker in expired {
            waker.wake();
        }
        let drain_wake = self
            .drain_waiter
            .borrow()
            .as_ref()
            .filter(|(scope, _)| scope.check().is_err())
            .map(|(_, waker)| waker.clone());
        if let Some(waker) = drain_wake {
            waker.wake();
        }
        Ok(())
    }

    pub fn outstanding(&self) -> usize {
        self.port.handoff.outstanding.load(Ordering::Acquire)
    }
    pub fn register_driver(&self, waker: &Waker) {
        self.port.handoff.io_waker.register(waker);
    }

    pub fn close_submissions(&self) -> Result<()> {
        self.port.close_submissions()
    }

    /// Deadline cancels delivery, not the ownership fence. Keep polling until all
    /// accepted jobs complete; a timeout cannot authorize dropping live buffers.
    pub fn drain<'a>(&'a self, scope: &'a RequestScope) -> Operation<'a, ()> {
        Box::pin(async move {
            let cancellation = scope.cancellation.subscribe()?;
            struct DrainGuard<'a>(&'a CryptoClient);
            impl Drop for DrainGuard<'_> {
                fn drop(&mut self) {
                    self.0.drain_waiter.borrow_mut().take();
                }
            }
            let _guard = DrainGuard(self);
            futures::future::poll_fn(move |cx| {
                if scope.check().is_ok() {
                    cancellation.register(cx.waker());
                }
                *self.drain_waiter.borrow_mut() = Some((scope.clone(), cx.waker().clone()));
                self.register_driver(cx.waker());
                self.port.handoff.capacity_waker.register(cx.waker());
                self.poll_budgeted(self.port.handoff.capacity.get())?;
                if scope.check().is_err() {
                    let wakes: Vec<_> = self
                        .waiters
                        .borrow()
                        .values()
                        .filter(|waiter| waiter.result.is_some())
                        .map(|waiter| waiter.waker.clone())
                        .collect();
                    for waiter in self.waiters.borrow_mut().values_mut() {
                        waiter.abandoned = true;
                    }
                    self.waiters
                        .borrow_mut()
                        .retain(|_, waiter| waiter.result.is_none());
                    for waker in wakes {
                        waker.wake();
                    }
                }
                if self.outstanding() == 0 {
                    Poll::Ready(Ok(()))
                } else {
                    Poll::Pending
                }
            })
            .await
        })
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn measurements_account_once_at_reap_even_for_cancel_and_abandon() {
        use crate::{
            runtime::environment::{self, SimulationClock},
            telemetry::metrics::{Event::*, Metrics},
        };
        use std::time::Duration;
        let clock = SimulationClock::new(42);
        let env = clock.environment(0);
        let _env = env.enter();
        let _strict = environment::require_simulated();
        let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let keys = keyring();
        for decrypt in [false, true] {
            for mode in ["success", "failure", "cancel", "abandon"] {
                let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                let client = CryptoClient::new(io);
                let metrics = Metrics::default();
                client.set_metrics(metrics.clone());
                let scope = RequestScope::new(
                    crate::model::RequestId([0; 16]),
                    environment::now() + Duration::from_secs(10),
                )
                .unwrap();
                let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
                let lease = || {
                    keys.active(&cache, crate::security::keyring::KeyPurpose::Page)
                        .unwrap()
                };
                let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                let mut data = input(&admission);
                if decrypt {
                    let (setup, _setup_engine) =
                        pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
                    let Poll::Ready(Ok(permit)) = setup.poll_reserve(
                        &mut cx,
                        CryptoId {
                            worker: WorkerId(0),
                            generation: 0,
                            sequence: 0,
                        },
                    ) else {
                        panic!("permit")
                    };
                    let result = crate::security::aead::PageCryptoEngine::process(permit.job(
                        data,
                        lease(),
                        scope.clone(),
                    ));
                    let CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, mut ciphertext)) =
                        result.outcome
                    else {
                        panic!("encrypted")
                    };
                    drop(plain);
                    if mode == "failure" {
                        Arc::get_mut(&mut ciphertext.inner).unwrap().bytes[0] ^= 1;
                    }
                    data = CryptoInput::Decrypt {
                        ciphertext,
                        plaintext: admission
                            .reserve(Some(&cache), crate::model::ResourceClass::Plaintext, 1)
                            .unwrap(),
                    };
                } else if mode == "failure" {
                    if let CryptoInput::Encrypt { page, .. } = &mut data {
                        page.version.object.cache.0 = "wrong".into();
                    }
                }
                // Admission delay must not leak into residence.
                clock.advance(Duration::from_millis(100));
                let mut future = client.execute(data, lease(), &scope);
                assert!(future.as_mut().poll(&mut cx).is_pending());
                if mode == "cancel" {
                    scope.cancel().unwrap();
                }
                clock.advance(Duration::from_millis(3));
                let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                    panic!("job")
                };
                let mut completion = crate::security::aead::PageCryptoEngine::process(job);
                // The engine uses virtual time (zero cost in DST). Explicitly
                // advance a measured interval to exercise the exact sum separately.
                assert_eq!(completion.permit.measurement.execution_ns, Some(0));
                let start = environment::now();
                clock.advance(Duration::from_millis(2));
                completion.permit.executed(start);
                let (_, mut wrong) = pair(WorkerId(1), 0, NonZeroUsize::new(1).unwrap());
                completion = wrong.complete(completion).err().unwrap().command;
                assert!(engine.complete(completion).is_ok());
                let events = if decrypt {
                    [
                        CryptoDecryptStarted,
                        CryptoDecryptSuccess,
                        CryptoDecryptFailure,
                        CryptoDecryptBytes,
                        CryptoDecryptExecutionCount,
                        CryptoDecryptExecutionNs,
                        CryptoDecryptQueueCount,
                        CryptoDecryptQueueNs,
                    ]
                } else {
                    [
                        CryptoEncryptStarted,
                        CryptoEncryptSuccess,
                        CryptoEncryptFailure,
                        CryptoEncryptBytes,
                        CryptoEncryptExecutionCount,
                        CryptoEncryptExecutionNs,
                        CryptoEncryptQueueCount,
                        CryptoEncryptQueueNs,
                    ]
                };
                assert_eq!(metrics.count(events[0]), 0);
                if mode == "abandon" {
                    drop(future);
                } else {
                    client.poll_budgeted(1).unwrap();
                    assert!(future.as_mut().poll(&mut cx).is_ready());
                }
                client.poll_budgeted(1).unwrap();
                client.poll_budgeted(1).unwrap();
                let success = u64::from(mode == "success" || mode == "abandon");
                for (event, expected) in events.into_iter().zip([
                    1,
                    success,
                    1 - success,
                    success,
                    1,
                    2_000_000,
                    1,
                    3_000_000,
                ]) {
                    assert_eq!(metrics.count(event), expected, "{mode} {event:?}");
                }
                assert_eq!(client.outstanding(), 0);
            }
        }
    }

    #[test]
    fn shared_worker_encrypt_queue_measurements() {
        shared_worker_queue_measurements(false);
    }

    #[test]
    fn shared_worker_decrypt_queue_measurements() {
        shared_worker_queue_measurements(true);
    }

    fn shared_worker_queue_measurements(decrypt: bool) {
        use crate::{
            runtime::environment::{self, SimulationClock},
            security::aead::PageCryptoEngine,
            telemetry::metrics::{Event::*, Metrics},
        };
        use std::time::Duration;

        let clock = SimulationClock::new(43);
        let env = clock.environment(0);
        let _env = env.enter();
        let _strict = environment::require_simulated();
        let admission = std::rc::Rc::new(crate::runtime::admission::Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let keys = keyring();
        let cache = crate::model::CacheId("00000000-0000-4000-8000-000000000003".into());
        let lease = || {
            keys.active(&cache, crate::security::keyring::KeyPurpose::Page)
                .unwrap()
        };
        let events = if decrypt {
            [
                CryptoDecryptStarted,
                CryptoDecryptSuccess,
                CryptoDecryptFailure,
                CryptoDecryptBytes,
                CryptoDecryptExecutionCount,
                CryptoDecryptExecutionNs,
                CryptoDecryptQueueCount,
                CryptoDecryptQueueNs,
            ]
        } else {
            [
                CryptoEncryptStarted,
                CryptoEncryptSuccess,
                CryptoEncryptFailure,
                CryptoEncryptBytes,
                CryptoEncryptExecutionCount,
                CryptoEncryptExecutionNs,
                CryptoEncryptQueueCount,
                CryptoEncryptQueueNs,
            ]
        };
        let opposite_queue = if decrypt {
            [CryptoEncryptQueueCount, CryptoEncryptQueueNs]
        } else {
            [CryptoDecryptQueueCount, CryptoDecryptQueueNs]
        };

        for mode in ["success", "failure", "cancel", "abandon"] {
            let mut cx = Context::from_waker(futures::task::noop_waker_ref());
            let scopes = [0, 1].map(|id| {
                RequestScope::new(
                    crate::model::RequestId([id; 16]),
                    environment::now() + Duration::from_secs(10),
                )
                .unwrap()
            });
            let make_input = |scope: &RequestScope, corrupt: bool| {
                let mut data = input(&admission);
                if decrypt {
                    let (setup, _setup_engine) =
                        pair(WorkerId(2), 0, NonZeroUsize::new(1).unwrap());
                    let mut cx = Context::from_waker(futures::task::noop_waker_ref());
                    let Poll::Ready(Ok(permit)) = setup.poll_reserve(
                        &mut cx,
                        CryptoId {
                            worker: WorkerId(2),
                            generation: 0,
                            sequence: 0,
                        },
                    ) else {
                        panic!("setup permit")
                    };
                    let result =
                        PageCryptoEngine::process(permit.job(data, lease(), scope.clone()));
                    let CryptoOutcome::Completed(CryptoOutput::Encrypted(plain, mut ciphertext)) =
                        result.outcome
                    else {
                        panic!("setup encryption")
                    };
                    drop(plain);
                    if corrupt {
                        Arc::get_mut(&mut ciphertext.inner).unwrap().bytes[0] ^= 1;
                    }
                    CryptoInput::Decrypt {
                        ciphertext,
                        plaintext: admission
                            .reserve(Some(&cache), crate::model::ResourceClass::Plaintext, 1)
                            .unwrap(),
                    }
                } else {
                    if corrupt {
                        if let CryptoInput::Encrypt { page, .. } = &mut data {
                            page.version.object.cache.0 = "wrong".into();
                        }
                    }
                    data
                }
            };
            let (other_io, mut other_engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
            let (waiting_io, mut waiting_engine) =
                pair(WorkerId(1), 0, NonZeroUsize::new(1).unwrap());
            // Occupy capacity without submitting: the target really parks before
            // admission, rather than merely being created after a clock advance.
            let Poll::Ready(Ok(blocker)) = waiting_io.poll_reserve(
                &mut cx,
                CryptoId {
                    worker: WorkerId(1),
                    generation: 0,
                    sequence: 0,
                },
            ) else {
                panic!("capacity blocker")
            };
            let other = CryptoClient::new(other_io);
            let waiting = CryptoClient::new(waiting_io);
            let other_metrics = Metrics::default();
            let waiting_metrics = Metrics::default();
            other.set_metrics(other_metrics.clone());
            waiting.set_metrics(waiting_metrics.clone());
            let mut future = Some(waiting.execute(
                make_input(&scopes[1], mode == "failure"),
                lease(),
                &scopes[1],
            ));
            assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            assert_eq!(waiting.pending.borrow().len(), 1);
            assert!(waiting.waiters.borrow().is_empty());
            clock.advance(Duration::from_millis(100));
            assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            assert!(waiting_engine.poll_job(&mut cx).is_pending());
            drop(blocker);
            assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            assert!(waiting.pending.borrow().is_empty());
            assert_eq!(waiting.waiters.borrow().len(), 1);

            let mut other_future =
                other.execute(make_input(&scopes[0], false), lease(), &scopes[0]);
            assert!(other_future.as_mut().poll(&mut cx).is_pending());
            if mode == "cancel" {
                scopes[1].cancel().unwrap();
                assert!(future.as_mut().unwrap().as_mut().poll(&mut cx).is_pending());
            } else if mode == "abandon" {
                drop(future.take());
            }

            // One execution service thread, manually interleaved: service the
            // other shard for 7ms before dequeuing the already-submitted target.
            // Real AEAD has zero virtual cost; set its measured service interval
            // explicitly, following the single-pair measurement test above.
            for (engine, worker, service_ms) in [
                (&mut other_engine, WorkerId(0), 7),
                (&mut waiting_engine, WorkerId(1), 5),
            ] {
                let Poll::Ready(Ok(Some(job))) = engine.poll_job(&mut cx) else {
                    panic!("submitted job")
                };
                assert_eq!(job.id().worker, worker);
                let mut completion = PageCryptoEngine::process(job);
                assert_eq!(completion.permit.measurement.execution_ns, Some(0));
                let start = environment::now();
                clock.advance(Duration::from_millis(service_ms));
                completion.permit.executed(start);
                assert!(engine.complete(completion).is_ok());
            }
            // Neither the target's own 5ms execution nor this 11ms delay before
            // I/O reaping belongs in its submit-to-dequeue queue measurement.
            clock.advance(Duration::from_millis(11));
            for event in events {
                assert_eq!(other_metrics.count(event), 0, "before reap {event:?}");
                assert_eq!(waiting_metrics.count(event), 0, "before reap {event:?}");
            }
            other.poll_budgeted(1).unwrap();
            for event in events {
                assert_eq!(
                    waiting_metrics.count(event),
                    0,
                    "other shard reaped {event:?}"
                );
            }
            waiting.poll_budgeted(1).unwrap();
            assert!(matches!(
                other_future.as_mut().poll(&mut cx),
                Poll::Ready(Ok(_))
            ));
            if let Some(mut future) = future {
                match (mode, future.as_mut().poll(&mut cx)) {
                    ("success", Poll::Ready(Ok(_)))
                    | ("failure", Poll::Ready(Err(_)))
                    | ("cancel", Poll::Ready(Err(Error::Cancelled))) => {}
                    _ => panic!("unexpected {mode} result"),
                }
            }
            let success = u64::from(mode == "success" || mode == "abandon");
            // Repeated drains must not duplicate either shard's observations,
            // even when delivery was canceled or abandoned before execution.
            for _ in 0..3 {
                other.poll_budgeted(1).unwrap();
                waiting.poll_budgeted(1).unwrap();
                for ((event, other_expected), waiting_expected) in events
                    .into_iter()
                    .zip([1, 1, 0, 1, 1, 7_000_000, 1, 0])
                    .zip([1, success, 1 - success, success, 1, 5_000_000, 1, 7_000_000])
                {
                    assert_eq!(
                        other_metrics.count(event),
                        other_expected,
                        "other {mode} {event:?}"
                    );
                    assert_eq!(
                        waiting_metrics.count(event),
                        waiting_expected,
                        "waiting {mode} {event:?}"
                    );
                }
                for event in opposite_queue {
                    assert_eq!(other_metrics.count(event), 0);
                    assert_eq!(waiting_metrics.count(event), 0);
                }
                assert_eq!(other.outstanding(), 0);
                assert_eq!(waiting.outstanding(), 0);
            }
        }
    }

    #[test]
    fn duration_saturates_without_host_time_in_dst() {
        use crate::runtime::environment::{self, SimulationClock};
        let clock = SimulationClock::new(19);
        let env = clock.environment(0);
        let _env = env.enter();
        let _strict = environment::require_simulated();
        let start = environment::now();
        clock.advance(std::time::Duration::from_secs(u64::MAX / 1_000_000_000 + 1));
        assert_eq!(elapsed_ns(start), u64::MAX);
    }

    fn input(admission: &std::rc::Rc<crate::runtime::admission::Admission>) -> CryptoInput {
        use crate::{
            memory::pool::BufferPool,
            model::{ResourceClass, *},
        };
        let cache = CacheId("00000000-0000-4000-8000-000000000003".into());
        CryptoInput::Encrypt {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: cache.clone(),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            },
            plaintext: BufferPool::new(admission.clone())
                .plaintext(
                    admission
                        .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                        .unwrap(),
                    1,
                )
                .unwrap(),
            ciphertext: admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
                .unwrap(),
        }
    }

    #[test]
    fn engine_loss_reclaims_queued_owners_and_unblocks_drain() {
        use crate::{
            model::{RequestId, ResourceClass},
            runtime::admission::Admission,
        };
        let admission = std::rc::Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (io, engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope = RequestScope::new(
            RequestId([0; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        let mut future = client.execute(input(&admission), key(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        drop(engine);
        client.poll_budgeted(1).unwrap();
        assert_eq!(client.outstanding(), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert!(matches!(future.as_mut().poll(&mut cx), Poll::Ready(Err(_))));
        assert!(client.drain(&scope).as_mut().poll(&mut cx).is_ready());
    }

    #[test]
    fn accepted_cancellation_cannot_return_before_completion_consumption() {
        use crate::{
            model::{RequestId, ResourceClass},
            runtime::admission::Admission,
        };
        let admission = std::rc::Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope = RequestScope::new(
            RequestId([0; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        let mut future = client.execute(input(&admission), key(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.cancel().unwrap();
        for _ in 0..4 {
            client.poll_budgeted(1).unwrap();
            assert!(future.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(client.outstanding(), 1);
        assert_eq!(admission.used(ResourceClass::Plaintext), 1);
        let job = match engine.poll_job(&mut cx) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("job"),
        };
        let completion = crate::security::aead::PageCryptoEngine::process(job);
        assert!(engine.complete(completion).is_ok());
        assert!(
            future.as_mut().poll(&mut cx).is_pending(),
            "queued completion is not consumed"
        );
        client.poll_budgeted(1).unwrap();
        assert!(matches!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(client.outstanding(), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn accepted_cancellation_waits_for_consumed_completion_not_notification() {
        use crate::{
            model::{RequestId, ResourceClass},
            runtime::admission::Admission,
        };
        let admission = std::rc::Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope = RequestScope::new(
            RequestId([0; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        let mut future = client.execute(input(&admission), key(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        scope.cancel().unwrap();
        for _ in 0..4 {
            client.poll_budgeted(1).unwrap();
            assert!(future.as_mut().poll(&mut cx).is_pending());
        }
        assert_eq!(client.outstanding(), 1);
        assert_eq!(admission.used(ResourceClass::Plaintext), 1);
        let job = match engine.poll_job(&mut cx) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("job"),
        };
        let CryptoJob {
            permit,
            input,
            key,
            scope: job_scope,
        } = job;
        assert!(
            engine
                .complete(CryptoCompletion {
                    permit,
                    outcome: CryptoOutcome::Failed {
                        input,
                        error: Error::Cancelled
                    },
                    key,
                    scope: job_scope
                })
                .is_ok()
        );
        assert!(
            future.as_mut().poll(&mut cx).is_pending(),
            "publication alone is not consumption"
        );
        client.poll_budgeted(1).unwrap();
        assert!(matches!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
        assert_eq!(client.outstanding(), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
    }

    #[test]
    fn accepted_deadline_expiry_waits_for_engine_completion() {
        use crate::{model::RequestId, runtime::admission::Admission};
        let admission = std::rc::Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let key = key();
        let scope = RequestScope::new(
            RequestId([0; 16]),
            std::time::Instant::now() + std::time::Duration::from_millis(20),
        )
        .unwrap();
        let mut future = client.execute(input(&admission), key, &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        std::thread::sleep(std::time::Duration::from_millis(25));
        assert!(future.as_mut().poll(&mut cx).is_pending());
        let job = match engine.poll_job(&mut cx) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("job"),
        };
        let completion = crate::security::aead::PageCryptoEngine::process(job);
        assert!(engine.complete(completion).is_ok());
        client.poll_budgeted(1).unwrap();
        assert!(matches!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::DeadlineExceeded))
        ));
        assert_eq!(client.outstanding(), 0);
    }

    #[test]
    fn abandoned_task_cannot_replace_worker_completion_wake() {
        use crate::{model::RequestId, runtime::admission::Admission};
        struct Count(AtomicUsize);
        impl std::task::Wake for Count {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let admission = std::rc::Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope = RequestScope::new(
            RequestId([0; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        let driver = Arc::new(Count(AtomicUsize::new(0)));
        client.register_driver(&Waker::from(driver.clone()));
        let mut future = client.execute(input(&admission), key(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        drop(future);
        let job = match engine.poll_job(&mut cx) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("job"),
        };
        let CryptoJob {
            permit,
            input,
            key,
            scope,
        } = job;
        assert!(
            engine
                .complete(CryptoCompletion {
                    permit,
                    outcome: CryptoOutcome::Failed {
                        input,
                        error: Error::Cancelled
                    },
                    key,
                    scope
                })
                .is_ok()
        );
        assert_eq!(driver.0.load(Ordering::Relaxed), 1);
        client.poll_budgeted(1).unwrap();
        assert_eq!(client.outstanding(), 0);
    }

    #[test]
    fn drain_scope_cancellation_wakes_even_with_unconsumed_result() {
        use crate::{model::RequestId, runtime::admission::Admission};
        struct Count(AtomicUsize);
        impl std::task::Wake for Count {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let admission = std::rc::Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope = RequestScope::new(
            RequestId([0; 16]),
            std::time::Instant::now() + std::time::Duration::from_secs(5),
        )
        .unwrap();
        let mut future = client.execute(input(&admission), key(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(future.as_mut().poll(&mut cx).is_pending());
        let job = match engine.poll_job(&mut cx) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("job"),
        };
        let CryptoJob {
            permit,
            input,
            key,
            scope: job_scope,
        } = job;
        assert!(
            engine
                .complete(CryptoCompletion {
                    permit,
                    outcome: CryptoOutcome::Failed {
                        input,
                        error: Error::Cancelled
                    },
                    key,
                    scope: job_scope
                })
                .is_ok()
        );
        client.poll_budgeted(1).unwrap();
        let drain_scope = RequestScope::new(RequestId([1; 16]), scope.deadline.0).unwrap();
        let count = Arc::new(Count(AtomicUsize::new(0)));
        let waker = Waker::from(count.clone());
        let mut drain_cx = Context::from_waker(&waker);
        let mut drain = client.drain(&drain_scope);
        assert!(drain.as_mut().poll(&mut drain_cx).is_pending());
        drain_scope.cancel().unwrap();
        assert!(count.0.load(Ordering::Relaxed) > 0);
        assert!(matches!(
            drain.as_mut().poll(&mut drain_cx),
            Poll::Ready(Ok(()))
        ));
        assert!(matches!(
            future.as_mut().poll(&mut cx),
            Poll::Ready(Err(Error::Cancelled))
        ));
    }

    #[test]
    fn capacity_tracks_reserved_owners_and_rejects_reused_identity() {
        let (io, _engine) = pair(WorkerId(1), 4, NonZeroUsize::new(1).unwrap());
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        let first = CryptoId {
            worker: WorkerId(1),
            generation: 4,
            sequence: 1,
        };
        let permit = match io.poll_reserve(&mut cx, first) {
            Poll::Ready(Ok(permit)) => permit,
            _ => panic!("first permit"),
        };
        let second = CryptoId {
            sequence: 2,
            ..first
        };
        assert!(io.poll_reserve(&mut cx, second).is_pending());
        assert!(matches!(
            io.poll_reserve(&mut cx, first),
            Poll::Ready(Err(Error::StaleFlight))
        ));
        drop(permit);
        assert!(matches!(
            io.poll_reserve(&mut cx, second),
            Poll::Ready(Ok(_))
        ));
        io.close_submissions().unwrap();
        assert!(matches!(
            io.poll_reserve(
                &mut cx,
                CryptoId {
                    sequence: 3,
                    ..first
                }
            ),
            Poll::Ready(Err(Error::Unavailable))
        ));
    }

    #[test]
    fn wrong_pair_generation_never_debits_capacity() {
        let (io, _engine) = pair(WorkerId(1), 4, NonZeroUsize::new(1).unwrap());
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        for (worker, generation) in [(WorkerId(2), 4), (WorkerId(1), 3)] {
            assert!(matches!(
                io.poll_reserve(
                    &mut cx,
                    CryptoId {
                        worker,
                        generation,
                        sequence: 1
                    }
                ),
                Poll::Ready(Err(Error::StaleFlight))
            ));
        }
        assert_eq!(io.handoff.outstanding.load(Ordering::Acquire), 0);
    }

    #[test]
    fn invalid_queue_capacity_is_a_startup_error_not_a_panic() {
        assert!(matches!(
            try_pair(WorkerId(0), 0, NonZeroUsize::new(usize::MAX).unwrap()),
            Err(Error::InvalidConfiguration)
        ));
    }

    pub(super) fn key() -> KeyLease {
        keyring()
            .active(
                &crate::model::CacheId("00000000-0000-4000-8000-000000000003".into()),
                crate::security::keyring::KeyPurpose::Page,
            )
            .unwrap()
    }

    pub(super) fn keyring() -> crate::security::keyring::Keyring {
        use crate::{
            control::wire::*,
            model::KeyId,
            model::{CacheId, ClusterId, NodeId},
            security::keyring::{KeyEpochs, Keyring},
        };
        let ca_key = rcgen::KeyPair::generate_for(&rcgen::PKCS_ED25519).unwrap();
        let mut params = rcgen::CertificateParams::default();
        params.is_ca = rcgen::IsCa::Ca(rcgen::BasicConstraints::Unconstrained);
        let ca = params.self_signed(&ca_key).unwrap();
        let keys = Keyring::new(
            ClusterId("00000000-0000-4000-8000-000000000001".into()),
            NodeId("00000000-0000-4000-8000-000000000002".into()),
            Arc::new(KeyEpochs::default()),
        );
        let cache = CacheId("00000000-0000-4000-8000-000000000003".into());
        keys.install(KeyringBundle {
            schema_version: 1,
            cluster: ClusterId("00000000-0000-4000-8000-000000000001".into()),
            generation: BundleGeneration(1),
            peer_trust_roots: vec![ca.der().to_vec()],
            cache_keys: vec![CacheEncryptionKey {
                key: CacheKeyRef {
                    cache: cache.clone(),
                    id: KeyId([1; 16]),
                    purpose: CacheKeyPurpose::Page,
                },
                state: CacheKeyState::Active,
                material: [7; 32],
            }],
        })
        .unwrap();
        keys
    }

    #[test]
    fn abandoned_future_retains_buffers_key_and_permit_until_reaped() {
        use crate::{
            memory::pool::BufferPool,
            model::{ResourceClass, *},
            runtime::admission::Admission,
        };
        use std::{
            rc::Rc,
            time::{Duration, Instant},
        };
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let cache = CacheId("cache".into());
        let pool = BufferPool::new(admission.clone());
        let plaintext = pool
            .plaintext(
                admission
                    .reserve(Some(&cache), ResourceClass::Plaintext, 3)
                    .unwrap(),
                3,
            )
            .unwrap();
        let input = CryptoInput::Encrypt {
            page: PageId {
                version: ObjectVersion {
                    object: ObjectId {
                        cache: cache.clone(),
                        key: CacheKey([0; 32]),
                    },
                    etag: StrongEtag::test_value("v1"),
                },
                number: PageNumber(0),
            },
            plaintext,
            ciphertext: admission
                .reserve(Some(&cache), ResourceClass::Ciphertext, 19)
                .unwrap(),
        };
        let (io, mut engine) = pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope =
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let mut operation = client.execute(input, key(), &scope);
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        struct Count(std::sync::atomic::AtomicUsize);
        impl std::task::Wake for Count {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, Ordering::Relaxed);
            }
        }
        let count = Arc::new(Count(std::sync::atomic::AtomicUsize::new(0)));
        let driver = Waker::from(count.clone());
        client.register_driver(&driver);
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        drop(operation);
        assert_eq!(client.outstanding(), 1);
        assert_eq!(admission.used(ResourceClass::Plaintext), 3);
        let job = match engine.poll_job(&mut cx) {
            Poll::Ready(Ok(Some(job))) => job,
            _ => panic!("accepted job"),
        };
        let CryptoJob {
            permit,
            input,
            key,
            scope,
        } = job;
        assert!(
            engine
                .complete(CryptoCompletion {
                    permit,
                    outcome: CryptoOutcome::Failed {
                        input,
                        error: Error::Cancelled
                    },
                    key,
                    scope
                })
                .is_ok()
        );
        assert_eq!(client.outstanding(), 1);
        assert_eq!(admission.used(ResourceClass::Plaintext), 3);
        client.poll_budgeted(1).unwrap();
        assert!(
            count.0.load(Ordering::Relaxed) > 0,
            "abandoned operation must not steal driver wake"
        );
        assert_eq!(client.outstanding(), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert_eq!(admission.used(ResourceClass::Ciphertext), 0);
    }

    #[test]
    fn engine_drop_reclaims_queued_jobs_and_drain_finishes() {
        use crate::{
            memory::pool::BufferPool,
            model::{ResourceClass, *},
            runtime::admission::Admission,
        };
        use std::{
            rc::Rc,
            time::{Duration, Instant},
        };
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let cache = CacheId("cache".into());
        let plaintext = BufferPool::new(admission.clone())
            .plaintext(
                admission
                    .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                    .unwrap(),
                1,
            )
            .unwrap();
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache: cache.clone(),
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        };
        let ciphertext = admission
            .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
            .unwrap();
        let (io, engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope =
            RequestScope::new(RequestId([0; 16]), Instant::now() + Duration::from_secs(5)).unwrap();
        let mut operation = client.execute(
            CryptoInput::Encrypt {
                page,
                plaintext,
                ciphertext,
            },
            key(),
            &scope,
        );
        let mut cx = Context::from_waker(futures::task::noop_waker_ref());
        assert!(operation.as_mut().poll(&mut cx).is_pending());
        drop(engine);
        client.poll_budgeted(1).unwrap();
        assert!(matches!(
            operation.as_mut().poll(&mut cx),
            Poll::Ready(Err(_))
        ));
        drop(operation);
        assert_eq!(client.outstanding(), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert!(client.drain(&scope).as_mut().poll(&mut cx).is_ready());
    }

    #[test]
    fn original_scope_failure_is_returned_without_submission() {
        use crate::{
            memory::pool::BufferPool,
            model::{ResourceClass, *},
            runtime::admission::Admission,
        };
        use std::{rc::Rc, time::Instant};
        let admission = Rc::new(Admission::new(
            crate::test_support::cluster::config(false).limits,
        ));
        let cache = CacheId("cache".into());
        let plaintext = BufferPool::new(admission.clone())
            .plaintext(
                admission
                    .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                    .unwrap(),
                1,
            )
            .unwrap();
        let ciphertext = admission
            .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
            .unwrap();
        let page = PageId {
            version: ObjectVersion {
                object: ObjectId {
                    cache,
                    key: CacheKey([0; 32]),
                },
                etag: StrongEtag::test_value("v1"),
            },
            number: PageNumber(0),
        };
        let (io, mut engine) = pair(WorkerId(0), 0, NonZeroUsize::new(1).unwrap());
        let client = CryptoClient::new(io);
        let scope = RequestScope::new(RequestId([0; 16]), Instant::now()).unwrap();
        let result = futures::executor::block_on(client.execute(
            CryptoInput::Encrypt {
                page,
                plaintext,
                ciphertext,
            },
            key(),
            &scope,
        ));
        assert!(matches!(result, Err(Error::DeadlineExceeded)));
        assert_eq!(client.outstanding(), 0);
        assert_eq!(admission.used(ResourceClass::Plaintext), 0);
        assert!(
            engine
                .poll_job(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
    }

    #[test]
    fn only_owned_messages_and_endpoints_cross_threads() {
        fn send<T: Send + 'static>() {}
        send::<CryptoInput>();
        send::<CryptoOutput>();
        send::<CryptoJob>();
        send::<CryptoCompletion>();
        send::<CryptoPermit>();
        send::<IoCryptoPort>();
        send::<CryptoPort>();
    }

    #[test]
    fn endpoints_share_one_bounded_pair_descriptor() {
        let (io, engine) = pair(WorkerId(7), 12, NonZeroUsize::new(3).unwrap());
        assert!(Arc::ptr_eq(&io.handoff, &engine.handoff));
        assert_eq!(io.handoff.worker, WorkerId(7));
        assert_eq!(engine.handoff.capacity.get(), 3);
        assert_eq!(engine.handoff.generation, 12);
        let (other, _) = pair(WorkerId(7), 13, NonZeroUsize::new(3).unwrap());
        assert!(!Arc::ptr_eq(&io.handoff, &other.handoff));
    }

    // Type-check the ownership path without manufacturing a key, permit, or page.
    // On rejection the caller can retry the very same resource-bearing job.
    #[allow(dead_code)]
    fn round_trip_api(
        io: IoCryptoPort,
        mut engine: CryptoPort,
        job: CryptoJob,
        completion: CryptoCompletion,
    ) {
        if let Err(failure) = io.try_submit(job) {
            let _: CryptoId = failure.command.id();
            let _retry = io.try_submit(failure.command);
        }
        if let Err(failure) = engine.complete(completion) {
            let _retry = engine.complete(failure.command);
        }
    }
}
