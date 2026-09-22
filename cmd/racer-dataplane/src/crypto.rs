// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Bounded, NUMA-local checksum execution and universe identity.
//!
//! Attach `Source` to the originating ring. Pool shutdown/join belongs on the
//! coordinator, after stopping I/O admission. Outstanding leases remain owned
//! independently of tickets and even survive a forgotten/destroyed source.

use crate::{
    allocator,
    buffers::{self, ComputeWrite, Fill},
    uring, workers,
};
use std::{
    collections::{BTreeMap, VecDeque},
    fmt, io,
    num::NonZeroUsize,
    rc::Rc,
    sync::{
        Arc, Condvar, Mutex,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    thread::{self, JoinHandle},
    time::Duration,
};

#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum Error {
    Invalid,
    Authentication,
    WouldBlock,
    Closed,
    ForeignOwner,
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
identity!(UniverseId);

#[derive(Clone)]
pub struct Snapshot(UniverseId);
impl Snapshot {
    pub fn new(universe: UniverseId) -> Self {
        Self(universe)
    }
    pub fn universe(&self) -> UniverseId {
        self.0
    }
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

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/security/crypto.rs"
));
