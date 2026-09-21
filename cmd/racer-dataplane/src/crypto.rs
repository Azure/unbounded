// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Signed peer authentication and bounded, NUMA-local checksum execution.
//!
//! Attach `Source` to the originating ring. Pool shutdown/join belongs on the
//! coordinator, after stopping I/O admission. Outstanding leases remain owned
//! independently of tickets and even survive a forgotten/destroyed source.

use crate::{
    allocator,
    buffers::{self, ComputeWrite, Fill},
    uring, workers,
};
use sha2::{Digest, Sha256};
use std::{
    collections::{BTreeMap, VecDeque},
    fmt, io,
    marker::PhantomData,
    num::NonZeroUsize,
    rc::Rc,
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Invalid,
    Authentication,
    UnknownKey,
    Disabled,
    ForeignUniverse,
    WouldBlock,
    Closed,
    ForeignOwner,
    Random,
    Expired,
    Sequence,
    WorkerFailed,
}
impl fmt::Display for Error {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "crypto: {self:?}")
    }
}
impl std::error::Error for Error {}
impl From<Error> for io::Error {
    fn from(e: Error) -> Self {
        Self::new(
            if e == Error::WouldBlock {
                io::ErrorKind::WouldBlock
            } else {
                io::ErrorKind::InvalidData
            },
            e,
        )
    }
}

macro_rules! identity {
    ($name:ident) => {
        #[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord, Hash)]
        pub struct $name([u8; 32]);
        impl $name {
            pub const fn new(bytes: [u8; 32]) -> Self {
                Self(bytes)
            }
            pub const fn bytes(self) -> [u8; 32] {
                self.0
            }
        }
    };
}
identity!(KeyId);
identity!(UniverseId);

struct Active {
    universe: UniverseId,
    signatures: crate::signing::Keys,
}
#[derive(Clone)]
pub struct Snapshot(Arc<Active>);
impl Snapshot {
    pub fn signed(universe: UniverseId, signatures: crate::signing::Keys) -> Self {
        Self(Arc::new(Active {
            universe,
            signatures,
        }))
    }
    pub fn signatures(&self) -> &crate::signing::Keys {
        &self.0.signatures
    }
    pub fn universe(&self) -> UniverseId {
        self.0.universe
    }
}

fn random<const N: usize>() -> Result<[u8; N], Error> {
    let mut out = [0; N];
    crate::environment::random(&mut out).map_err(|_| Error::Random)?;
    Ok(out)
}
pub struct Rejected<T> {
    pub error: Error,
    pub resource: T,
}
pub struct PoolConfig {
    /// Per attachment returned by `attach`/`attach_local`, including retained
    /// configuration generations on one IO worker. The aggregate NUMA queue is
    /// separately capped at this value times that node's compute worker count.
    pub max_outstanding_per_worker: NonZeroUsize,
}
impl Default for PoolConfig {
    fn default() -> Self {
        Self {
            max_outstanding_per_worker: NonZeroUsize::new(8).unwrap(),
        }
    }
}
struct Owner;
enum Input {
    Checksum(ComputeWrite, usize, Option<u64>),
}
enum Output {
    Checked(ComputeWrite, usize, u64),
}
struct ResultSlot {
    result: Mutex<Option<Result<Output, Error>>>,
    cancelled: AtomicBool,
    queue: Arc<Queue>,
    released: AtomicBool,
}
impl ResultSlot {
    fn cancel(&self) {
        self.cancelled.store(true, Ordering::Release);
        let output = {
            let mut result = self.result.lock().unwrap();
            match result.take() {
                Some(Ok(output)) => {
                    let release = !self.released.swap(true, Ordering::AcqRel);
                    *result = Some(Err(Error::Closed));
                    Some((output, release))
                }
                Some(Err(_)) => {
                    *result = Some(Err(Error::Closed));
                    self.release();
                    None
                }
                None => None,
            }
        };
        if let Some((output, release)) = output {
            self.queue.discard(output, release);
        }
    }

    fn release(&self) {
        if !self.released.swap(true, Ordering::AcqRel) {
            self.queue.outstanding.fetch_sub(1, Ordering::AcqRel);
            self.queue.changed.notify_all();
            // Capacity is shared across I/O workers on this NUMA node.
            for endpoint in self
                .queue
                .endpoints
                .lock()
                .unwrap()
                .iter()
                .filter_map(std::sync::Weak::upgrade)
            {
                workers::Wake::wake(&*endpoint.wake);
            }
        }
    }
}
impl Drop for ResultSlot {
    fn drop(&mut self) {
        if let Some(Ok(output)) = self.result.get_mut().unwrap().take() {
            let release = !self.released.swap(true, Ordering::AcqRel);
            self.queue.discard(output, release);
        }
        self.release();
    }
}
struct Job {
    input: Input,
    slot: Arc<ResultSlot>,
    endpoint: Arc<Endpoint>,
}
struct Endpoint {
    wake: Arc<uring::Wake>,
    closed: AtomicBool,
}
struct Queue {
    jobs: Mutex<VecDeque<Job>>,
    changed: Condvar,
    stopped: AtomicBool,
    outstanding: std::sync::atomic::AtomicUsize,
    limit: usize,
    slots: Mutex<Vec<std::sync::Weak<ResultSlot>>>,
    endpoints: Mutex<Vec<std::sync::Weak<Endpoint>>>,
    cleanup: Mutex<VecDeque<(Output, bool)>>,
}
impl Queue {
    fn discard(&self, output: Output, release: bool) {
        #[cfg(test)]
        if crate::simulation::current().is_some() {
            self.clean((output, release));
            return;
        }
        let _jobs = self.jobs.lock().unwrap();
        self.cleanup.lock().unwrap().push_back((output, release));
        self.changed.notify_one();
    }
    fn clean(&self, (output, release): (Output, bool)) {
        // Buffer cancellation invokes arbitrary waiter callbacks. A callback
        // panic must neither kill the compute thread nor leak its reservation.
        let _ = guarded(|| drop(output));
        if release {
            self.outstanding.fetch_sub(1, Ordering::AcqRel);
            self.changed.notify_all();
        }
        for endpoint in self
            .endpoints
            .lock()
            .unwrap()
            .iter()
            .filter_map(std::sync::Weak::upgrade)
        {
            workers::Wake::wake(&*endpoint.wake);
        }
    }
}
fn guarded<T>(f: impl FnOnce() -> T) -> Result<T, Error> {
    std::panic::catch_unwind(std::panic::AssertUnwindSafe(f)).map_err(|payload| {
        // Even a user-defined panic payload destructor can panic.
        if let Err(payload) =
            std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| drop(payload)))
        {
            std::mem::forget(payload);
        }
        Error::WorkerFailed
    })
}
pub struct Pool {
    queues: BTreeMap<workers::NumaNodeId, Arc<Queue>>,
    threads: Vec<JoinHandle<()>>,
    limit: usize,
}
pub struct Worker {
    queue: Arc<Queue>,
    endpoint: Arc<Endpoint>,
    buffers: buffers::WorkerPool,
    owner: Rc<Owner>,
    slots: Rc<std::cell::RefCell<Vec<Arc<ResultSlot>>>>,
    capacity: Rc<std::cell::RefCell<Option<Waker>>>,
    limit: usize,
}
pub struct Source {
    endpoint: Arc<Endpoint>,
    slots: Rc<std::cell::RefCell<Vec<Arc<ResultSlot>>>>,
    capacity: Rc<std::cell::RefCell<Option<Waker>>>,
    queue: Arc<Queue>,
    limit: usize,
}
struct Ticket {
    slot: Arc<ResultSlot>,
    owner: Rc<Owner>,
    endpoint: Arc<Endpoint>,
    taken: bool,
}
pub struct ChecksumTicket(Ticket);
impl Drop for Ticket {
    fn drop(&mut self) {
        self.slot.cancelled.store(true, Ordering::Release);
        workers::Wake::wake(&*self.endpoint.wake);
    }
}

impl Pool {
    pub fn start(placement: &workers::ComputePlacement, config: PoolConfig) -> Result<Self, Error> {
        Self::start_on(placement.cpus(), config, workers::pin_compute)
    }
    fn start_on(
        cpus: &[(workers::CpuId, workers::NumaNodeId)],
        config: PoolConfig,
        pin: fn(workers::CpuId) -> io::Result<()>,
    ) -> Result<Self, Error> {
        if cpus.is_empty() {
            return Err(Error::Invalid);
        }
        let mut pool = Self {
            queues: BTreeMap::new(),
            threads: Vec::new(),
            limit: config.max_outstanding_per_worker.get(),
        };
        for &(cpu, node) in cpus {
            let q = pool
                .queues
                .entry(node)
                .or_insert_with(|| {
                    Arc::new(Queue {
                        jobs: Mutex::new(VecDeque::new()),
                        changed: Condvar::new(),
                        stopped: AtomicBool::new(false),
                        outstanding: std::sync::atomic::AtomicUsize::new(0),
                        limit: cpus
                            .iter()
                            .filter(|(_, n)| *n == node)
                            .count()
                            .saturating_mul(pool.limit),
                        endpoints: Mutex::new(Vec::new()),
                        cleanup: Mutex::new(VecDeque::new()),
                        slots: Mutex::new(Vec::new()),
                    })
                })
                .clone();
            let (tx, rx) = std::sync::mpsc::sync_channel(1);
            let handle = thread::Builder::new()
                .name(format!("racer-crc-{}", cpu.0))
                .spawn(move || {
                    let ok = pin(cpu).is_ok();
                    let _ = tx.send(ok);
                    if ok {
                        compute_loop(q);
                    }
                })
                .map_err(|_| Error::WorkerFailed)?;
            pool.threads.push(handle);
            if rx.recv() != Ok(true) {
                return Err(Error::WorkerFailed);
            }
        }
        Ok(pool)
    }
    pub fn attach(
        &self,
        placement: &workers::WorkerContext,
        buffers: &buffers::WorkerPool,
        wake: Arc<uring::Wake>,
    ) -> Result<(Worker, Source), Error> {
        placement
            .bind_pool(buffers)
            .map_err(|_| Error::ForeignOwner)?;
        if placement.numa_node_id() != buffers.numa_node_id() {
            return Err(Error::ForeignOwner);
        }
        // SAFETY: sched_getcpu takes no arguments and only observes placement.
        if unsafe { libc::sched_getcpu() } != placement.cpu_id().0 as i32 {
            return Err(Error::ForeignOwner);
        }
        self.attach_local(buffers, wake)
    }
    pub(crate) fn attach_local(
        &self,
        buffers: &buffers::WorkerPool,
        wake: Arc<uring::Wake>,
    ) -> Result<(Worker, Source), Error> {
        let queue = self
            .queues
            .get(&buffers.numa_node_id())
            .ok_or(Error::ForeignOwner)?
            .clone();
        if queue.stopped.load(Ordering::Acquire) {
            return Err(Error::Closed);
        }
        let endpoint = Arc::new(Endpoint {
            wake,
            closed: AtomicBool::new(false),
        });
        {
            let mut endpoints = queue.endpoints.lock().unwrap();
            endpoints.retain(|e| e.strong_count() != 0);
            endpoints.push(Arc::downgrade(&endpoint));
        }
        let slots = Rc::new(std::cell::RefCell::new(Vec::with_capacity(self.limit)));
        let capacity = Rc::new(std::cell::RefCell::new(None));
        Ok((
            Worker {
                queue: queue.clone(),
                endpoint: endpoint.clone(),
                buffers: buffers.clone(),
                owner: Rc::new(Owner),
                slots: slots.clone(),
                capacity: capacity.clone(),
                limit: self.limit,
            },
            Source {
                endpoint,
                slots,
                capacity,
                queue,
                limit: self.limit,
            },
        ))
    }
    #[cfg(test)]
    pub(crate) fn test_pool(buffers: &buffers::WorkerPool) -> Self {
        if crate::simulation::current().is_some() {
            let limit = PoolConfig::default().max_outstanding_per_worker.get();
            return Self {
                queues: BTreeMap::from([(
                    buffers.numa_node_id(),
                    Arc::new(Queue {
                        jobs: Mutex::new(VecDeque::new()),
                        changed: Condvar::new(),
                        stopped: AtomicBool::new(false),
                        outstanding: std::sync::atomic::AtomicUsize::new(0),
                        limit,
                        slots: Mutex::new(Vec::new()),
                        endpoints: Mutex::new(Vec::new()),
                        cleanup: Mutex::new(VecDeque::new()),
                    }),
                )]),
                threads: Vec::new(),
                limit,
            };
        }
        Self::start_on(
            &[(workers::CpuId(0), buffers.numa_node_id())],
            PoolConfig::default(),
            |_| Ok(()),
        )
        .unwrap()
    }
    pub fn shutdown(mut self) -> Result<(), Error> {
        self.stop()
    }
    fn stop(&mut self) -> Result<(), Error> {
        for q in self.queues.values() {
            let _guard = q.jobs.lock().unwrap();
            q.stopped.store(true, Ordering::Release);
            q.changed.notify_all();
        }
        for q in self.queues.values() {
            // Transfer completed-but-unconsumed output to compute cleanup even
            // when a ticket/source outlives the pool. Running jobs observe stop
            // when publishing and transfer their output themselves.
            let slots: Vec<_> = q
                .slots
                .lock()
                .unwrap()
                .drain(..)
                .filter_map(|s| s.upgrade())
                .collect();
            for slot in slots {
                slot.cancel();
            }
        }
        let mut failed = false;
        for t in self.threads.drain(..) {
            failed |= t.join().is_err();
        }
        if failed {
            Err(Error::WorkerFailed)
        } else {
            Ok(())
        }
    }
}
impl Drop for Pool {
    fn drop(&mut self) {
        let _ = self.stop();
    }
}

fn execute(input: Input) -> Result<Output, Error> {
    match input {
        Input::Checksum(mut bytes, len, expected) => {
            let checksum = allocator::crc64(&bytes.bytes()[..len]);
            if expected.is_some_and(|expected| expected != checksum) {
                return Err(Error::Authentication);
            }
            Ok(Output::Checked(bytes, len, checksum))
        }
    }
}
fn compute_loop(queue: Arc<Queue>) {
    loop {
        let job = {
            let mut jobs = queue.jobs.lock().unwrap();
            while jobs.is_empty() && queue.cleanup.lock().unwrap().is_empty() {
                if queue.stopped.load(Ordering::Acquire) {
                    if queue.outstanding.load(Ordering::Acquire) == 0 {
                        return;
                    }
                    // A ticket destructor may be transferring its reservation
                    // into cleanup. Never exit while that output still exists.
                    jobs = queue
                        .changed
                        .wait_timeout(jobs, Duration::from_millis(10))
                        .unwrap()
                        .0;
                } else {
                    jobs = queue.changed.wait(jobs).unwrap();
                }
            }
            let cleanup = queue.cleanup.lock().unwrap().pop_front();
            if let Some(output) = cleanup {
                drop(jobs);
                queue.clean(output);
                continue;
            }
            match jobs.pop_front() {
                Some(j) => j,
                None => return,
            }
        };
        run_job(&queue, job);
    }
}
fn run_job(queue: &Queue, job: Job) {
    let closed =
        queue.stopped.load(Ordering::Acquire) || job.endpoint.closed.load(Ordering::Acquire);
    let result = guarded(|| {
        if closed || job.slot.cancelled.load(Ordering::Acquire) {
            drop(job.input);
            Err(Error::Closed)
        } else {
            execute(job.input)
        }
    })
    .unwrap_or(Err(Error::WorkerFailed));
    complete(queue, &job.slot, &job.endpoint, result);
}
fn complete(queue: &Queue, slot: &ResultSlot, endpoint: &Endpoint, result: Result<Output, Error>) {
    *slot.result.lock().unwrap() = Some(result);
    // Transfer cancelled output to cleanup without holding a result lock
    // across buffer cancellation callbacks. Source teardown races safely
    // with publication through the slot mutex and cancellation flag.
    if queue.stopped.load(Ordering::Acquire)
        || slot.cancelled.load(Ordering::Acquire)
        || endpoint.closed.load(Ordering::Acquire)
    {
        slot.cancel();
    }
    workers::Wake::wake(&*endpoint.wake);
}

impl Worker {
    fn available(&self) -> Result<(), Error> {
        if self.endpoint.closed.load(Ordering::Acquire)
            || self.queue.stopped.load(Ordering::Acquire)
        {
            return Err(Error::Closed);
        }
        if self.slots.borrow().len() >= self.limit
            || self.queue.outstanding.load(Ordering::Acquire) >= self.queue.limit
        {
            return Err(Error::WouldBlock);
        }
        Ok(())
    }
    fn reserve(&self) -> Result<(), Error> {
        self.queue
            .outstanding
            .fetch_update(Ordering::AcqRel, Ordering::Acquire, |n| {
                (n < self.queue.limit).then_some(n + 1)
            })
            .map(|_| ())
            .map_err(|_| Error::WouldBlock)
    }
    fn submit(&mut self, input: Input) -> Ticket {
        let slot = Arc::new(ResultSlot {
            result: Mutex::new(None),
            cancelled: AtomicBool::new(false),
            queue: self.queue.clone(),
            released: AtomicBool::new(false),
        });
        {
            let mut slots = self.queue.slots.lock().unwrap();
            slots.retain(|s| {
                s.upgrade()
                    .is_some_and(|s| !s.released.load(Ordering::Acquire))
            });
            slots.push(Arc::downgrade(&slot));
        }
        self.slots.borrow_mut().push(slot.clone());
        #[cfg(test)]
        if let Some(world) = crate::simulation::current() {
            let queue = self.queue.clone();
            let job = Job {
                input,
                slot: slot.clone(),
                endpoint: self.endpoint.clone(),
            };
            world.schedule(move || {
                // Queued -> running: cancellation before start skips CRC. Once
                // running, the real ComputeWrite stays owned until completion.
                let closed = queue.stopped.load(Ordering::Acquire)
                    || job.endpoint.closed.load(Ordering::Acquire)
                    || job.slot.cancelled.load(Ordering::Acquire);
                // Avoid a World -> callback -> World ownership cycle.
                crate::simulation::current()
                    .expect("compute scheduler context")
                    .schedule(move || {
                        let result = guarded(|| {
                            if closed {
                                drop(job.input);
                                Err(Error::Closed)
                            } else {
                                execute(job.input)
                            }
                        })
                        .unwrap_or(Err(Error::WorkerFailed));
                        complete(&queue, &job.slot, &job.endpoint, result);
                    });
            });
            return Ticket {
                slot,
                owner: self.owner.clone(),
                endpoint: self.endpoint.clone(),
                taken: false,
            };
        }
        let mut jobs = self.queue.jobs.lock().unwrap();
        if self.queue.stopped.load(Ordering::Acquire) {
            *slot.result.lock().unwrap() = Some(Err(Error::Closed));
            slot.release();
        } else {
            jobs.push_back(Job {
                input,
                slot: slot.clone(),
                endpoint: self.endpoint.clone(),
            });
            self.queue.changed.notify_one();
        }
        Ticket {
            slot,
            owner: self.owner.clone(),
            endpoint: self.endpoint.clone(),
            taken: false,
        }
    }
    pub fn checksum(
        &mut self,
        fill: Fill,
        len: usize,
        expected: Option<u64>,
    ) -> Result<ChecksumTicket, Rejected<Fill>> {
        let check = self.available().and_then(|_| {
            if !self.buffers.owns_fill(&fill) {
                return Err(Error::ForeignOwner);
            }
            if len > buffers::BUFFER_SIZE {
                return Err(Error::Invalid);
            }
            self.reserve()
        });
        if let Err(error) = check {
            return Err(Rejected {
                error,
                resource: fill,
            });
        }
        Ok(ChecksumTicket(self.submit(Input::Checksum(
            fill.into_compute(),
            len,
            expected,
        ))))
    }
    pub fn take_checksum(
        &mut self,
        ticket: &mut ChecksumTicket,
    ) -> Option<Result<(Fill, usize, u64), Error>> {
        self.take(&mut ticket.0).map(|result| {
            result.map(|output| match output {
                Output::Checked(fill, len, checksum) => (fill.into_fill(), len, checksum),
            })
        })
    }
    fn take(&mut self, ticket: &mut Ticket) -> Option<Result<Output, Error>> {
        if !Rc::ptr_eq(&ticket.owner, &self.owner) {
            return Some(Err(Error::ForeignOwner));
        }
        if ticket.taken {
            return Some(Err(Error::Invalid));
        }
        let result = ticket.slot.result.lock().unwrap().take()?;
        ticket.taken = true;
        self.slots
            .borrow_mut()
            .retain(|s| !Arc::ptr_eq(s, &ticket.slot));
        if let Some(w) = self.capacity.borrow_mut().take() {
            w.wake();
        }
        if result.is_err() {
            ticket.slot.release();
        }
        Some(result.and_then(|output| {
            if self.endpoint.closed.load(Ordering::Acquire)
                || self.queue.stopped.load(Ordering::Acquire)
            {
                let release = !ticket.slot.released.swap(true, Ordering::AcqRel);
                self.queue.discard(output, release);
                Err(Error::Closed)
            } else {
                ticket.slot.release();
                Ok(output)
            }
        }))
    }
    pub fn poll_capacity(&mut self, cx: &mut Context<'_>) -> Poll<Result<(), Error>> {
        match self.available() {
            Err(Error::WouldBlock) => {
                *self.capacity.borrow_mut() = Some(cx.waker().clone());
                match self.available() {
                    Err(Error::WouldBlock) => Poll::Pending,
                    result => {
                        self.capacity.borrow_mut().take();
                        Poll::Ready(result)
                    }
                }
            }
            result => Poll::Ready(result),
        }
    }
}
impl Drop for Worker {
    fn drop(&mut self) {
        self.endpoint.closed.store(true, Ordering::Release);
    }
}
impl uring::CompletionSource for Source {
    fn poll(&mut self, _: &mut uring::Ring, budget: usize) -> io::Result<uring::Work> {
        Ok(self.poll_ready(budget))
    }
    fn arm(&mut self, _: &mut uring::Ring) -> io::Result<()> {
        Ok(())
    }
    fn shutdown(&mut self, _: &mut uring::Ring) -> io::Result<()> {
        self.close();
        Ok(())
    }
}
impl Source {
    fn poll_ready(&mut self, budget: usize) -> uring::Work {
        let mut removed = 0;
        self.slots.borrow_mut().retain(|slot| {
            if removed < budget
                && slot.cancelled.load(Ordering::Acquire)
                && slot.result.lock().unwrap().is_some()
            {
                removed += 1;
                false
            } else {
                true
            }
        });
        if self.endpoint.closed.load(Ordering::Acquire)
            || self.queue.stopped.load(Ordering::Acquire)
            || (self.slots.borrow().len() < self.limit
                && self.queue.outstanding.load(Ordering::Acquire) < self.queue.limit)
        {
            let waker = self.capacity.borrow_mut().take();
            if let Some(w) = waker {
                w.wake();
            }
        }
        uring::Work {
            runnable: budget > 0 && removed == budget,
            deadline: None,
        }
    }
    fn close(&mut self) {
        self.endpoint.closed.store(true, Ordering::Release);
        let slots = std::mem::take(&mut *self.slots.borrow_mut());
        for slot in slots {
            slot.cancel();
        }
        let waker = self.capacity.borrow_mut().take();
        if let Some(waker) = waker {
            waker.wake();
        }
    }
}
impl Drop for Source {
    fn drop(&mut self) {
        self.close();
    }
}

/// Signed peer authentication, binding both challenges and exact transport offers.
pub mod auth {
    use super::*;
    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/security/handshake.rs"
    ));
    const DOMAIN: &[u8] = b"racer/auth/v2";
    const MAX_OFFER: usize = 4096;
    pub const MAX_CONTROL: usize = 8192;
    /// Maximum application-defined canonical routing context in a handshake.
    pub const MAX_ROUTING_CONTEXT: usize = 1024;
    const CONTEXT_DOMAIN: &[u8] = b"racer/auth/negotiation/v1";
    const MAX_HANDSHAKE: usize =
        2 * MAX_OFFER + 256 + CONTEXT_DOMAIN.len() + 44 + MAX_ROUTING_CONTEXT;

    /// The exact offer authenticated by a completed handshake. Only
    /// [`Session::take_offer`] constructs this capability; decoding is untrusted.
    /// ```compile_fail
    /// use racer_dataplane::rdma::{Connecting, Offer};
    /// fn activate(c: Connecting, offer: Offer) { let _ = c.connect(offer, 0); }
    /// ```
    /// Arbitrary verifiers cannot turn parsed data into authentication:
    /// ```compile_fail
    /// use racer_dataplane::rdma::Offer;
    /// fn bypass(offer: Offer) { let _ = offer.authenticate(|_| Ok(())); }
    /// ```
    /// ```compile_fail
    /// use racer_dataplane::rdma::{AuthenticatedOffer, Offer};
    /// fn fabricate(offer: Offer) { let _ = AuthenticatedOffer(offer); }
    /// ```
    /// ```compile_fail
    /// use racer_dataplane::rdma::{Connecting, AuthenticatedOffer};
    /// fn reuse(a: Connecting, b: Connecting, offer: AuthenticatedOffer) {
    ///     let _ = a.connect(offer, 0);
    ///     let _ = b.connect(offer, 0);
    /// }
    /// ```
    pub struct AuthenticatedOffer(crate::rdma::Offer, [u8; 32]);
    impl AuthenticatedOffer {
        pub(crate) fn into_parts(self) -> (crate::rdma::Offer, [u8; 32]) {
            (self.0, self.1)
        }
    }

    #[derive(Clone)]
    pub struct PeerContext {
        initiator: [u8; 32],
        responder: [u8; 32],
        negotiation: Option<NegotiationContext>,
    }
    #[derive(Clone)]
    struct NegotiationContext {
        volume: [u8; 32],
        shard: u64,
        routing: Vec<u8>,
    }
    impl PeerContext {
        pub fn new(initiator: [u8; 32], responder: [u8; 32]) -> Result<Self, Error> {
            if initiator == responder {
                return Err(Error::Invalid);
            }
            Ok(Self {
                initiator,
                responder,
                negotiation: None,
            })
        }

        /// Bind the expected volume, canonical shard and routing context in
        /// addition to identities, universe and both offers. Supply these
        /// from local policy, not unchecked peer claims. Both peers must use the
        /// same canonical routing encoding (including any routing generation).
        /// This binding is domain-separated from identity-only `new` contexts;
        /// an empty routing slice still binds volume and shard. Replaces any
        /// previous negotiation binding. Bounds are checked before allocation.
        pub fn with_negotiation(
            mut self,
            volume: [u8; 32],
            shard: u64,
            routing: &[u8],
        ) -> Result<Self, Error> {
            if routing.len() > MAX_ROUTING_CONTEXT {
                return Err(Error::Invalid);
            }
            self.negotiation = Some(NegotiationContext {
                volume,
                shard,
                routing: routing.to_vec(),
            });
            Ok(self)
        }
        fn bytes(&self, snapshot: &Snapshot) -> Vec<u8> {
            let mut bytes = [
                snapshot.universe().0.as_slice(),
                &self.initiator,
                &self.responder,
            ]
            .concat();
            if let Some(context) = &self.negotiation {
                bytes.extend_from_slice(CONTEXT_DOMAIN);
                bytes.extend_from_slice(&context.volume);
                bytes.extend_from_slice(&context.shard.to_be_bytes());
                bytes.extend_from_slice(&(context.routing.len() as u32).to_be_bytes());
                bytes.extend_from_slice(&context.routing);
            }
            bytes
        }
    }
    pub struct Hello(Vec<u8>);
    pub struct Reply(Vec<u8>);
    pub struct Finish([u8; 96]);
    impl Hello {
        pub fn encode(&self) -> &[u8] {
            &self.0
        }
        pub fn decode(bytes: &[u8]) -> Result<Self, Error> {
            if bytes.len() < 64 || bytes.len() > 64 + MAX_OFFER {
                return Err(Error::Invalid);
            }
            Ok(Self(bytes.to_vec()))
        }
    }
    impl Reply {
        pub fn encode(&self) -> &[u8] {
            &self.0
        }
        pub fn decode(bytes: &[u8]) -> Result<Self, Error> {
            if bytes.len() < 128 || bytes.len() > 128 + MAX_OFFER {
                return Err(Error::Invalid);
            }
            Ok(Self(bytes.to_vec()))
        }
    }
    impl Finish {
        pub fn encode(&self) -> &[u8] {
            &self.0
        }
        pub fn decode(bytes: &[u8]) -> Result<Self, Error> {
            Ok(Self(bytes.try_into().map_err(|_| Error::Invalid)?))
        }
    }
    fn offer_bytes(offer: Option<&crate::rdma::Offer>) -> Result<Vec<u8>, Error> {
        let bytes = offer.map(|o| o.encode()).unwrap_or_default();
        if bytes.len() > MAX_OFFER {
            return Err(Error::Invalid);
        }
        Ok(bytes)
    }
    fn deadline(timeout: Duration) -> Result<Instant, Error> {
        if timeout.is_zero() || timeout > Duration::from_secs(60) {
            return Err(Error::Invalid);
        }
        crate::environment::now()
            .checked_add(timeout)
            .ok_or(Error::Invalid)
    }
    fn fresh(deadline: Instant) -> Result<(), Error> {
        if crate::environment::now() >= deadline {
            Err(Error::Expired)
        } else {
            Ok(())
        }
    }
    fn transcript(context: &[u8], hello: &[u8], reply: &[u8]) -> Vec<u8> {
        let mut out = Vec::new();
        for part in [context, hello, reply] {
            out.extend_from_slice(&(part.len() as u64).to_be_bytes());
            out.extend_from_slice(part);
        }
        debug_assert!(out.len() <= MAX_HANDSHAKE);
        out
    }
    /// Single-use handshake continuation; a reply cannot complete it twice.
    /// ```compile_fail
    /// use racer_dataplane::crypto::auth::{Initiator, Reply};
    /// fn replay(i: Initiator, a: Reply, b: Reply) {
    ///     let _ = i.finish(a);
    ///     let _ = i.finish(b);
    /// }
    /// ```
    pub struct Initiator {
        snapshot: Snapshot,
        keys: crate::signing::Keys,
        context: Vec<u8>,
        hello: Vec<u8>,
        deadline: Instant,
    }
    /// Single-use handshake continuation; initiator proof is required to finish.
    /// ```compile_fail
    /// use racer_dataplane::crypto::{Snapshot, auth::Responder};
    /// fn premature(r: &mut Responder, s: &Snapshot) { let _ = r.take_offer(s); }
    /// ```
    /// ```compile_fail
    /// use racer_dataplane::crypto::auth::{Responder, Finish};
    /// fn replay(r: Responder, a: Finish, b: Finish) {
    ///     let _ = r.finish(a);
    ///     let _ = r.finish(b);
    /// }
    /// ```
    pub struct Responder {
        snapshot: Snapshot,
        keys: crate::signing::Keys,
        key: KeyId,
        transcript: Vec<u8>,
        remote: Vec<u8>,
        deadline: Instant,
    }
    impl Initiator {
        pub fn start(
            snapshot: Snapshot,
            expected: PeerContext,
            offer: Option<&crate::rdma::Offer>,
            timeout: Duration,
        ) -> Result<(Self, Hello), Error> {
            let deadline = deadline(timeout)?;
            let keys = snapshot.signatures().pinned();
            let mut hello = Vec::new();
            if !keys.can_authenticate() {
                return Err(Error::Disabled);
            }
            hello.extend_from_slice(&keys.signing_id().map_err(|_| Error::Disabled)?);
            hello.extend_from_slice(&random::<32>()?);
            hello.extend_from_slice(&offer_bytes(offer)?);
            let context = expected.bytes(&snapshot);
            Ok((
                Self {
                    snapshot,
                    keys,
                    context,
                    hello: hello.clone(),
                    deadline,
                },
                Hello(hello),
            ))
        }
        pub fn finish(self, reply: Reply) -> Result<(Session, Finish), Error> {
            fresh(self.deadline)?;
            let split = reply.0.len() - 96;
            let transcript = transcript(&self.context, &self.hello, &reply.0[..split]);
            let key_id = KeyId(
                self.snapshot
                    .signatures()
                    .verify(DOMAIN, &[b"responder", &transcript], &reply.0[split..])
                    .map_err(|_| Error::Authentication)?,
            );
            let finish = Finish(
                self.keys
                    .sign(DOMAIN, &[b"initiator", &transcript])
                    .map_err(|_| Error::Disabled)?,
            );
            let session = Session::new(
                &self.snapshot,
                self.keys,
                key_id,
                &transcript,
                &reply.0[32..split],
                true,
            )?;
            Ok((session, finish))
        }
    }
    impl Responder {
        pub fn accept(
            snapshot: Snapshot,
            expected: PeerContext,
            hello: Hello,
            offer: Option<&crate::rdma::Offer>,
            timeout: Duration,
        ) -> Result<(Self, Reply), Error> {
            let deadline = deadline(timeout)?;
            let keys = snapshot.signatures().pinned();
            let key_id = KeyId(hello.0[..32].try_into().unwrap());
            if !snapshot.signatures().trusts(&key_id.0) {
                return Err(Error::UnknownKey);
            }
            let context = expected.bytes(&snapshot);
            let mut reply = random::<32>()?.to_vec();
            reply.extend_from_slice(&offer_bytes(offer)?);
            let transcript = transcript(&context, &hello.0, &reply);
            reply.extend_from_slice(
                &keys
                    .sign(DOMAIN, &[b"responder", &transcript])
                    .map_err(|_| Error::Disabled)?,
            );
            Ok((
                Self {
                    snapshot,
                    keys,
                    key: key_id,
                    transcript,
                    remote: hello.0[64..].to_vec(),
                    deadline,
                },
                Reply(reply),
            ))
        }
        pub fn finish(self, finish: Finish) -> Result<Session, Error> {
            fresh(self.deadline)?;
            let signer = self
                .snapshot
                .signatures()
                .verify(DOMAIN, &[b"initiator", &self.transcript], &finish.0)
                .map_err(|_| Error::Authentication)?;
            if signer != self.key.0 {
                return Err(Error::Authentication);
            }
            Session::new(
                &self.snapshot,
                self.keys,
                self.key,
                &self.transcript,
                &self.remote,
                false,
            )
        }
    }
    /// An exact, opaque control frame supplied by the transport. All fields
    /// (including request identity, addresses/rkeys, value context and descriptor)
    /// must be encoded in `body`. RPC correlation remains the transport's job.
    pub struct Control {
        request: u64,
        body: Vec<u8>,
    }
    impl Control {
        pub fn new(request: u64, body: Vec<u8>) -> Result<Self, Error> {
            if body.len() > MAX_CONTROL {
                return Err(Error::Invalid);
            }
            Ok(Self { request, body })
        }
    }
    pub struct SignedControl(Vec<u8>);
    impl SignedControl {
        pub fn encode(&self) -> &[u8] {
            &self.0
        }
        pub fn decode(bytes: &[u8]) -> Result<Self, Error> {
            if bytes.len() < 112 || bytes.len() > 112 + MAX_CONTROL {
                return Err(Error::Invalid);
            }
            Ok(Self(bytes.to_vec()))
        }
    }
    pub struct VerifiedControl {
        request: u64,
        body: Vec<u8>,
    }
    impl VerifiedControl {
        pub fn request_id(&self) -> u64 {
            self.request
        }
        pub fn body(&self) -> &[u8] {
            &self.body
        }
    }
    pub struct Session {
        keys: crate::signing::Keys,
        drain: std::cell::Cell<Option<Instant>>,
        binding: [u8; 32],
        transcript: [u8; 32],
        initiator: bool,
        local_key: KeyId,
        tx: u64,
        rx: u64,
        universe: UniverseId,
        key: KeyId,
        remote: Option<Vec<u8>>,
        _local: PhantomData<Rc<()>>,
    }
    impl Session {
        fn new(
            snapshot: &Snapshot,
            keys: crate::signing::Keys,
            key_id: KeyId,
            transcript: &[u8],
            remote: &[u8],
            initiator: bool,
        ) -> Result<Self, Error> {
            if !remote.is_empty() {
                crate::rdma::Offer::decode(remote).map_err(|_| Error::Invalid)?;
            }
            Ok(Self {
                binding: {
                    let mut hash = Sha256::new();
                    hash.update(transcript);
                    hash.update([initiator as u8]);
                    hash.finalize().into()
                },
                transcript: Sha256::digest(transcript).into(),
                initiator,
                local_key: KeyId(keys.signing_id().map_err(|_| Error::Disabled)?),
                keys,
                drain: std::cell::Cell::new(None),
                tx: 0,
                rx: 0,
                universe: snapshot.universe(),
                key: key_id,
                remote: Some(remote.to_vec()),
                _local: PhantomData,
            })
        }
        fn current(&self, snapshot: &Snapshot) -> Result<(), Error> {
            if self.universe != snapshot.universe() {
                return Err(Error::ForeignUniverse);
            }
            let latest = snapshot.signatures().pinned();
            if !latest.trusts(&self.key.0) {
                return Err(Error::UnknownKey);
            }
            if latest.signing_id().map_err(|_| Error::Disabled)? != self.local_key.0 {
                if !latest.trusts(&self.local_key.0) {
                    return Err(Error::UnknownKey);
                }
                let now = crate::environment::now();
                let until = self.drain.get().unwrap_or_else(|| {
                    let until = now + Duration::from_secs(30);
                    self.drain.set(Some(until));
                    until
                });
                if now >= until {
                    return Err(Error::Expired);
                }
            }
            Ok(())
        }
        pub(crate) fn admitting(&self, snapshot: &Snapshot) -> bool {
            self.current(snapshot).is_ok() && self.drain.get().is_none()
        }
        pub(crate) fn healthy(&self, snapshot: &Snapshot) -> bool {
            self.current(snapshot).is_ok()
        }
        pub(crate) fn matches_transport(
            &self,
            snapshot: &Snapshot,
            binding: &[u8; 32],
        ) -> Result<(), Error> {
            self.current(snapshot)?;
            if &self.binding != binding || self.remote.is_some() {
                return Err(Error::Invalid);
            }
            Ok(())
        }
        /// Consumes the exact peer offer bound by the handshake, once only.
        pub fn take_offer(
            &mut self,
            snapshot: &Snapshot,
        ) -> Result<Option<crate::rdma::AuthenticatedOffer>, Error> {
            self.current(snapshot)?;
            let bytes = self.remote.take().ok_or(Error::Invalid)?;
            if bytes.is_empty() {
                return Ok(None);
            }
            let offer = crate::rdma::Offer::decode(&bytes).map_err(|_| Error::Invalid)?;
            if offer.encode() != bytes {
                return Err(Error::Authentication);
            }
            Ok(Some(AuthenticatedOffer(offer, self.binding)))
        }
        pub fn sign(
            &mut self,
            snapshot: &Snapshot,
            control: Control,
        ) -> Result<SignedControl, Error> {
            self.current(snapshot)?;
            let next = self.tx.checked_add(1).ok_or(Error::Sequence)?;
            let mut bytes = self.tx.to_be_bytes().to_vec();
            bytes.extend_from_slice(&control.request.to_be_bytes());
            bytes.extend_from_slice(&control.body);
            bytes.extend_from_slice(
                &self
                    .keys
                    .sign(
                        DOMAIN,
                        &[
                            b"control",
                            &self.transcript,
                            &[self.initiator as u8],
                            &bytes,
                        ],
                    )
                    .map_err(|_| Error::Disabled)?,
            );
            self.tx = next;
            Ok(SignedControl(bytes))
        }
        pub fn verify(
            &mut self,
            snapshot: &Snapshot,
            signed: SignedControl,
        ) -> Result<VerifiedControl, Error> {
            self.current(snapshot)?;
            let next = self.rx.checked_add(1).ok_or(Error::Sequence)?;
            let split = signed.0.len() - 96;
            let signer = snapshot
                .signatures()
                .verify(
                    DOMAIN,
                    &[
                        b"control",
                        &self.transcript,
                        &[(!self.initiator) as u8],
                        &signed.0[..split],
                    ],
                    &signed.0[split..],
                )
                .map_err(|_| Error::Authentication)?;
            if signer != self.key.0 {
                return Err(Error::Authentication);
            }
            let sequence = u64::from_be_bytes(signed.0[..8].try_into().unwrap());
            if sequence != self.rx {
                return Err(Error::Sequence);
            }
            self.rx = next;
            Ok(VerifiedControl {
                request: u64::from_be_bytes(signed.0[8..16].try_into().unwrap()),
                body: signed.0[16..split].to_vec(),
            })
        }
    }
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/crypto.rs"
));
