// Copyright (c) Microsoft Corporation.
// SPDX-License-Identifier: Apache-2.0

//! Pinned, completion-driven Linux workers. Each physical core contributes at most
//! one allowed CPU; workers are spread across NUMA nodes and own disjoint shards.
//! Drivers are constructed after pinning and remain on that thread until dropped.
//! NUMA placement identifies the intended allocation node; buffer pools must still
//! enforce memory locality themselves.

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fs;
use std::io;
use std::marker::PhantomData;
use std::num::NonZeroUsize;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::path::Path;
use std::rc::Rc;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::{Arc, Condvar, Mutex};
use std::thread::{self, JoinHandle};

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Hash)]
pub struct WorkerId(pub usize);

pub use crate::sharding::{Placement, ShardId, WorkerContext};

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Hash)]
pub struct CpuId(pub usize);

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Hash)]
pub struct NumaNodeId(pub usize);

#[derive(Clone, Copy, Debug)]
pub struct Config {
    pub shard_count: NonZeroUsize,
}

impl Default for Config {
    fn default() -> Self {
        Self {
            shard_count: NonZeroUsize::new(32).unwrap(),
        }
    }
}

/// Worker counts per participating NUMA node. By default, split physical cores
/// evenly, giving I/O the odd core. With one override, the other uses the rest;
/// with both overrides, spare cores stay idle. I/O is also limited by shard count.
#[derive(Clone, Copy, Debug, Default)]
pub struct WorkerCounts {
    pub io_per_node: Option<NonZeroUsize>,
    pub compute_per_node: Option<NonZeroUsize>,
}

/// Shared startup plan: compute and I/O use disjoint physical cores, including
/// SMT siblings. Every participating NUMA node retains at least one I/O core.
pub struct CpuPlan {
    io: Vec<Placement>,
    compute: ComputePlacement,
}

pub struct ComputePlacement {
    cpus: Vec<(CpuId, NumaNodeId)>,
}
impl ComputePlacement {
    pub fn cpus(&self) -> &[(CpuId, NumaNodeId)] {
        &self.cpus
    }
}
impl CpuPlan {
    pub fn discover(config: Config, counts: WorkerCounts) -> io::Result<Self> {
        Self::build(config, counts, discover()?)
    }
    fn build(config: Config, counts: WorkerCounts, cpus: Vec<Cpu>) -> io::Result<Self> {
        // Reuse the existing physical-core validation and NUMA-balanced order.
        let all = place(
            Config {
                shard_count: NonZeroUsize::new(cpus.len()).ok_or_else(|| invalid("no CPUs"))?,
            },
            cpus.clone(),
        )?;
        let mut nodes: BTreeMap<NumaNodeId, Vec<CpuId>> = BTreeMap::new();
        for p in all {
            nodes.entry(p.node).or_default().push(p.cpu);
        }
        // With fewer shards than NUMA nodes only participate on the same first
        // nodes the I/O planner would use. Do not reserve stranded compute CPUs.
        let nodes: BTreeMap<_, _> = nodes.into_iter().take(config.shard_count.get()).collect();
        let mut candidates = Vec::new();
        for (&node, cores) in &nodes {
            let io = match (counts.io_per_node, counts.compute_per_node) {
                (Some(io), _) => io.get(),
                (None, Some(crypto)) => cores.len().saturating_sub(crypto.get()),
                (None, None) => cores.len().div_ceil(2),
            };
            let crypto = counts.compute_per_node.map_or(1, NonZeroUsize::get);
            if io == 0 || io.checked_add(crypto).is_none_or(|n| n > cores.len()) {
                return Err(invalid(format!(
                    "NUMA node {} has {} physical cores; needs disjoint positive I/O and compute counts",
                    node.0,
                    cores.len()
                )));
            }
            candidates.extend(cores.iter().take(io).map(|&id| Cpu {
                id,
                node,
                siblings: cpus.iter().find(|c| c.id == id).unwrap().siblings.clone(),
            }));
        }
        if counts.io_per_node.is_some() && candidates.len() > config.shard_count.get() {
            return Err(invalid("explicit I/O worker count exceeds shard count"));
        }
        let io = place(config, candidates)?;
        let mut compute = Vec::new();
        for (node, cores) in nodes {
            let io_count = io.iter().filter(|p| p.node == node).count();
            let count = counts
                .compute_per_node
                .map_or(cores.len() - io_count, NonZeroUsize::get);
            compute.extend(cores.into_iter().rev().take(count).map(|cpu| (cpu, node)));
        }
        Ok(Self {
            io,
            compute: ComputePlacement { cpus: compute },
        })
    }
    pub fn compute(&self) -> &ComputePlacement {
        &self.compute
    }
    pub fn io(&self) -> &[Placement] {
        &self.io
    }
}

pub(crate) fn pin_compute(cpu: CpuId) -> io::Result<()> {
    pin(cpu)
}

/// Thread-safe, nonblocking, nonpanicking notification. Notifications may coalesce
/// but must be retained if delivered before a wait. A handle must remain safe to
/// call during and after driver shutdown, for as long as the handle exists.
pub trait Wake: Send + Sync {
    fn wake(&self);
}

/// One composite completion loop for all of a worker's I/O and application work.
/// It may contain non-Send/non-Sync state, including Rc and thread-local resources.
/// Implementations must not change the worker's CPU affinity.
///
/// This is a safe trait: the runtime never uses its behavioral contracts to
/// justify unsafe memory access. Drivers own outstanding I/O resources, and their
/// Drop implementation must preserve safety even if shutdown fails, panics, or
/// is skipped. Future raw asynchronous submission APIs must be unsafe unless
/// ownership enforces buffer lifetime and access restrictions. A notification or
/// cancellation acknowledgment alone is not proof that a buffer can be freed.
pub trait Driver {
    type Wake: Wake + 'static;

    fn wake_handle(&self) -> Arc<Self::Wake>;

    /// Progress submissions and bounded, fair batches of application work and
    /// completions across all sources. Do not sleep with known runnable work or
    /// an exhausted budget. Otherwise arm notifications, recheck for work, and
    /// wait until I/O, a software wake, or the next deadline. Return after a wake
    /// even if it yielded no I/O so the runtime can observe a stop request.
    fn turn(&mut self) -> io::Result<()>;

    /// Stop new admission, retaining already admitted HTTP work until completion.
    fn begin_drain(&mut self) {}
    fn drained(&self) -> bool {
        true
    }

    /// Stop admitting work and quiesce all outstanding I/O before releasing its
    /// resources. Called on the owning thread, including after turn fails/panics.
    fn shutdown(&mut self) -> io::Result<()>;
}

#[derive(Clone, Debug)]
struct Cpu {
    id: CpuId,
    node: NumaNodeId,
    siblings: Vec<CpuId>,
}

fn invalid(message: impl Into<String>) -> io::Error {
    io::Error::new(io::ErrorKind::InvalidData, message.into())
}

fn parse_cpu_list(text: &str) -> io::Result<Vec<CpuId>> {
    let mut cpus = BTreeSet::new();
    for part in text.trim().split(',') {
        let (start, end) = part.split_once('-').unwrap_or((part, part));
        let parse = |s: &str| s.parse::<usize>().map_err(|_| invalid("invalid CPU list"));
        let (start, end) = (parse(start)?, parse(end)?);
        if start > end {
            return Err(invalid("reversed CPU range"));
        }
        for cpu in start..=end {
            if !cpus.insert(CpuId(cpu)) {
                return Err(invalid("duplicate CPU in list"));
            }
        }
    }
    Ok(cpus.into_iter().collect())
}

// Use native words, not cpu_set_t's fixed CPU_SETSIZE, for large/sparse systems.
fn allowed_cpus() -> io::Result<Vec<CpuId>> {
    read_affinity(|mask| {
        // SAFETY: mask is aligned, writable, and has exactly the supplied size.
        let result =
            unsafe { libc::sched_getaffinity(0, size_of_val(mask), mask.as_mut_ptr().cast()) };
        if result == 0 {
            Ok(())
        } else {
            Err(io::Error::last_os_error())
        }
    })
}

fn read_affinity(
    mut get: impl FnMut(&mut [libc::c_ulong]) -> io::Result<()>,
) -> io::Result<Vec<CpuId>> {
    let mut mask = vec![0 as libc::c_ulong; 16];
    loop {
        let error = match get(&mut mask) {
            Ok(()) => {
                return Ok(mask
                    .iter()
                    .enumerate()
                    .flat_map(|(word, &bits)| {
                        (0..libc::c_ulong::BITS as usize).filter_map(move |bit| {
                            (bits & (1 << bit) != 0)
                                .then_some(CpuId(word * libc::c_ulong::BITS as usize + bit))
                        })
                    })
                    .collect());
            }
            Err(error) => error,
        };
        if error.raw_os_error() != Some(libc::EINVAL) {
            return Err(error);
        }
        let len = mask
            .len()
            .checked_mul(2)
            .ok_or_else(|| invalid("CPU mask too large"))?;
        mask.resize(len, 0);
    }
}

fn pin(cpu: CpuId) -> io::Result<()> {
    let mask = single_cpu_mask(cpu);
    // SAFETY: mask is aligned, readable, and has exactly the supplied size. PID 0
    // changes only this thread's affinity; the mask contains one selected CPU.
    if unsafe { libc::sched_setaffinity(0, size_of_val(mask.as_slice()), mask.as_ptr().cast()) }
        != 0
    {
        return Err(io::Error::last_os_error());
    }
    Ok(())
}

fn single_cpu_mask(cpu: CpuId) -> Vec<libc::c_ulong> {
    let bits = libc::c_ulong::BITS as usize;
    let mut mask = vec![0 as libc::c_ulong; cpu.0 / bits + 1];
    mask[cpu.0 / bits] = 1 << (cpu.0 % bits);
    mask
}

fn discover() -> io::Result<Vec<Cpu>> {
    discover_at(Path::new("/sys/devices/system"), allowed_cpus()?)
}

fn discover_at(root: &Path, allowed: Vec<CpuId>) -> io::Result<Vec<Cpu>> {
    let has_numa = match fs::metadata(root.join("node")) {
        Ok(metadata) if metadata.is_dir() => true,
        Ok(_) => return Err(invalid("NUMA topology is not a directory")),
        Err(error) if error.kind() == io::ErrorKind::NotFound => false,
        Err(error) => return Err(error),
    };
    allowed
        .into_iter()
        .map(|id| {
            let path = root.join(format!("cpu/cpu{}", id.0));
            let siblings = parse_cpu_list(&fs::read_to_string(
                path.join("topology/thread_siblings_list"),
            )?)?;
            let mut node = None;
            if has_numa {
                for entry in fs::read_dir(&path)? {
                    let entry = entry?;
                    let name = entry.file_name();
                    let name = name.to_string_lossy();
                    if let Some(suffix) = name.strip_prefix("node") {
                        let id = suffix.parse().map_err(|_| invalid("invalid NUMA node"))?;
                        if node.replace(NumaNodeId(id)).is_some() {
                            return Err(invalid(format!(
                                "multiple NUMA nodes for {}",
                                path.display()
                            )));
                        }
                    }
                }
            }
            let node = match (has_numa, node) {
                (_, Some(node)) => node,
                (false, None) => NumaNodeId(0),
                (true, None) => {
                    return Err(invalid(format!("missing NUMA node for {}", path.display())));
                }
            };
            Ok(Cpu { id, node, siblings })
        })
        .collect()
}

// Pure placement also accepts synthetic topology for placement tests.
fn place(config: Config, mut cpus: Vec<Cpu>) -> io::Result<Vec<Placement>> {
    if cpus.is_empty() {
        return Err(invalid("no allowed physical cores"));
    }
    cpus.sort_by_key(|cpu| cpu.id);
    let mut membership: BTreeMap<CpuId, &Cpu> = BTreeMap::new();
    let mut cores = BTreeMap::new();
    for (index, cpu) in cpus.iter().enumerate() {
        if index > 0 && cpus[index - 1].id == cpu.id {
            return Err(invalid("duplicate CPU in topology"));
        }
        if !cpu.siblings.contains(&cpu.id) || cpu.siblings.windows(2).any(|pair| pair[0] >= pair[1])
        {
            return Err(invalid("invalid physical-core membership"));
        }
        for sibling in &cpu.siblings {
            if let Some(other) = membership.insert(*sibling, cpu)
                && (other.siblings != cpu.siblings || other.node != cpu.node)
            {
                return Err(invalid("inconsistent physical-core topology"));
            }
        }
        // The key includes disallowed siblings, but the chosen CPU is allowed.
        cores.entry(cpu.siblings[0]).or_insert(cpu);
    }
    let mut nodes: BTreeMap<NumaNodeId, VecDeque<CpuId>> = BTreeMap::new();
    for cpu in cores.values() {
        nodes.entry(cpu.node).or_default().push_back(cpu.id);
    }
    let count = config.shard_count.get().min(cores.len());
    let mut placements = Vec::with_capacity(count);
    while placements.len() < count {
        for (&node, cpus) in &mut nodes {
            if placements.len() == count {
                break;
            }
            if let Some(cpu) = cpus.pop_front() {
                placements.push((cpu, node));
            }
        }
    }
    crate::sharding::placements(placements, config.shard_count.get())
}

#[derive(Default)]
struct State {
    wakes: Vec<Arc<dyn Wake>>,
    released: bool,
    failure: Option<io::Error>,
}

#[derive(Default)]
struct Shared {
    stopping: AtomicBool,
    lifecycle: Option<Arc<crate::lifecycle::Lifecycle>>,
    state: Mutex<State>,
    changed: Condvar,
}

impl Shared {
    fn request_stop(&self) {
        if !self.stopping.swap(true, Ordering::AcqRel) {
            let wakes = self.state.lock().unwrap().wakes.clone();
            self.changed.notify_all();
            for wake in wakes {
                wake.wake();
            }
        }
    }

    fn fail(&self, worker: WorkerId, error: io::Error) {
        // Display and Drop on a custom error are application code. Neither may
        // unwind through the state lock or prevent stop and driver shutdown.
        let message = match catch_unwind(AssertUnwindSafe(|| error.to_string())) {
            Ok(message) => message,
            Err(payload) => {
                discard_caught(payload);
                "error formatter panicked".to_owned()
            }
        };
        self.state.lock().unwrap().failure.get_or_insert_with(|| {
            io::Error::new(error.kind(), format!("worker {}: {message}", worker.0))
        });
        self.request_stop();
        discard_caught(error);
    }

    fn ready(&self, wake: Arc<dyn Wake>) {
        let mut state = self.state.lock().unwrap();
        state.wakes.push(wake.clone());
        self.changed.notify_all();
        if self.stopping.load(Ordering::Acquire) {
            // Stop may have taken its wake snapshot before this registration.
            drop(state);
            wake.wake();
            return;
        }
        while !state.released && !self.stopping.load(Ordering::Acquire) {
            state = self.changed.wait(state).unwrap();
        }
    }
}

/// Cloneable stop capability, without ownership of threads or raw wake handles.
#[derive(Clone, Default)]
pub struct StopHandle {
    shared: Arc<Shared>,
}

impl StopHandle {
    pub fn supervised(lifecycle: Arc<crate::lifecycle::Lifecycle>) -> Self {
        Self {
            shared: Arc::new(Shared {
                lifecycle: Some(lifecycle),
                ..Default::default()
            }),
        }
    }
    pub fn is_stopping(&self) -> bool {
        self.shared.stopping.load(Ordering::Acquire)
    }
    pub fn check_startup(&self) -> io::Result<()> {
        if self.is_stopping()
            || self
                .shared
                .lifecycle
                .as_ref()
                .is_some_and(|life| life.draining())
        {
            Err(io::Error::new(
                io::ErrorKind::Interrupted,
                "startup cancelled",
            ))
        } else {
            Ok(())
        }
    }
    /// Idempotently request cooperative shutdown and wake sleeping workers.
    pub fn request_stop(&self) {
        self.shared.request_stop();
    }
}

/// Unique owner of the worker threads. Dropping requests stop and joins them all.
/// Driver calls, factories, and wake implementations must eventually return:
/// neither stop nor Drop can forcibly interrupt blocking application code.
pub struct Workers {
    shared: Arc<Shared>,
    placements: Arc<[Placement]>,
    threads: Vec<(WorkerId, JoinHandle<()>)>,
}

impl Workers {
    /// Start using a plan shared with the crypto pool. Construct the pool from
    /// `plan.compute()` first, then move this plan into the I/O runtime.
    pub fn start_planned<D, F>(plan: CpuPlan, factory: F) -> io::Result<Self>
    where
        D: Driver + 'static,
        F: Fn(&WorkerContext) -> io::Result<D> + Send + Sync + 'static,
    {
        Self::start_placed(plan.io, factory, pin)
    }
    /// The caller can cancel before the factory barrier; blocked factories still
    /// require the process watchdog/external supervisor, never detached cleanup.
    pub fn start_supervised<D, F>(plan: CpuPlan, stop: StopHandle, factory: F) -> io::Result<Self>
    where
        D: Driver + 'static,
        F: Fn(&WorkerContext) -> io::Result<D> + Send + Sync + 'static,
    {
        Self::start_shared(plan.io, factory, pin, stop.shared, |builder, task| {
            builder.spawn(task)
        })
    }
    /// Discover allowed physical cores and construct one driver per selected CPU.
    /// The factory runs after pinning, allowing thread-local and NUMA-aware setup.
    /// No driver turns run until every factory and wake registration succeeds.
    /// Failure stops and joins all started threads before returning an error.
    pub fn start<D, F>(config: Config, factory: F) -> io::Result<Self>
    where
        D: Driver + 'static,
        F: Fn(&WorkerContext) -> io::Result<D> + Send + Sync + 'static,
    {
        Self::start_placed(place(config, discover()?)?, factory, pin)
    }

    fn start_placed<D, F, P>(placements: Vec<Placement>, factory: F, pin: P) -> io::Result<Self>
    where
        D: Driver + 'static,
        F: Fn(&WorkerContext) -> io::Result<D> + Send + Sync + 'static,
        P: Fn(CpuId) -> io::Result<()> + Send + Sync + 'static,
    {
        Self::start_with_spawner(placements, factory, pin, |builder, task| {
            builder.spawn(task)
        })
    }

    fn start_with_spawner<D, F, P>(
        placements: Vec<Placement>,
        factory: F,
        pin: P,
        spawn: impl FnMut(thread::Builder, Box<dyn FnOnce() + Send>) -> io::Result<JoinHandle<()>>,
    ) -> io::Result<Self>
    where
        D: Driver + 'static,
        F: Fn(&WorkerContext) -> io::Result<D> + Send + Sync + 'static,
        P: Fn(CpuId) -> io::Result<()> + Send + Sync + 'static,
    {
        Self::start_shared(placements, factory, pin, Arc::default(), spawn)
    }

    fn start_shared<D, F, P>(
        placements: Vec<Placement>,
        factory: F,
        pin: P,
        shared: Arc<Shared>,
        mut spawn: impl FnMut(thread::Builder, Box<dyn FnOnce() + Send>) -> io::Result<JoinHandle<()>>,
    ) -> io::Result<Self>
    where
        D: Driver + 'static,
        F: Fn(&WorkerContext) -> io::Result<D> + Send + Sync + 'static,
        P: Fn(CpuId) -> io::Result<()> + Send + Sync + 'static,
    {
        // This owner also rolls back a partially started group on early return.
        let mut workers = Self {
            shared,
            placements: placements.into(),
            threads: Vec::new(),
        };
        let factory = Arc::new(factory);
        let pin = Arc::new(pin);
        for index in 0..workers.placements.len() {
            if workers.shared.stopping.load(Ordering::Acquire) {
                break;
            }
            let id = workers.placements[index].worker;
            let placements = workers.placements.clone();
            let shared = workers.shared.clone();
            let factory = factory.clone();
            let pin = pin.clone();
            match spawn(
                thread::Builder::new().name(format!("racer-worker-{}", id.0)),
                Box::new(move || {
                    let result = caught(|| {
                        let placement = &placements[index];
                        pin(placement.cpu)?;
                        StopHandle {
                            shared: shared.clone(),
                        }
                        .check_startup()?;
                        let context = WorkerContext::pinned(placements.clone(), index);
                        let driver = factory(&context)?;
                        Worker {
                            id,
                            driver,
                            shared: shared.clone(),
                            _thread_local: PhantomData,
                        }
                        .run();
                        Ok(())
                    });
                    if let Err(error) = result {
                        shared.fail(id, error);
                    }
                }),
            ) {
                Ok(handle) => workers.threads.push((id, handle)),
                Err(error) => {
                    workers.shared.fail(id, error);
                    break;
                }
            }
        }
        let mut state = workers.shared.state.lock().unwrap();
        while state.wakes.len() != workers.placements.len()
            && !workers.shared.stopping.load(Ordering::Acquire)
        {
            state = workers.shared.changed.wait(state).unwrap();
        }
        if workers.shared.stopping.load(Ordering::Acquire) {
            drop(state);
            return Err(workers.join().err().unwrap_or_else(|| {
                io::Error::new(io::ErrorKind::Interrupted, "startup cancelled")
            }));
        }
        state.released = true;
        workers.shared.changed.notify_all();
        drop(state);
        Ok(workers)
    }

    pub fn placements(&self) -> &[Placement] {
        &self.placements
    }

    pub fn stop_handle(&self) -> StopHandle {
        StopHandle {
            shared: self.shared.clone(),
        }
    }

    /// Wait for all threads and return the first observed failure, with worker
    /// context. This does not request stop: use a StopHandle to initiate shutdown.
    pub fn join(mut self) -> io::Result<()> {
        self.join_all();
        match self.shared.state.lock().unwrap().failure.take() {
            Some(error) => Err(error),
            None => Ok(()),
        }
    }

    fn join_all(&mut self) {
        for (id, thread) in self.threads.drain(..) {
            if let Err(panic) = thread.join() {
                self.shared.fail(id, panic_error(panic));
            }
        }
    }
}

impl Drop for Workers {
    fn drop(&mut self) {
        self.shared.request_stop();
        self.join_all();
    }
}

// Even a Send driver cannot make this owner Send or Sync. There is no operation
// that extracts the driver; run consumes its owner on the constructing thread.
struct Worker<D: Driver> {
    id: WorkerId,
    driver: D,
    shared: Arc<Shared>,
    _thread_local: PhantomData<Rc<()>>,
}

impl<D: Driver> Worker<D> {
    fn run(mut self) {
        let result = caught(|| {
            self.shared.ready(self.driver.wake_handle());
            let mut draining = false;
            while !self.shared.stopping.load(Ordering::Acquire) {
                if self
                    .shared
                    .lifecycle
                    .as_ref()
                    .is_some_and(|life| life.draining())
                {
                    if !draining {
                        self.driver.begin_drain();
                        draining = true;
                    }
                    if self.driver.drained() {
                        break;
                    }
                }
                self.driver.turn()?;
            }
            Ok(())
        });
        if let Err(error) = result {
            // Notify peers before potentially lengthy local shutdown/destruction.
            self.shared.fail(self.id, error);
        }
        if let Err(error) = caught(|| self.driver.shutdown()) {
            self.shared.fail(self.id, error);
        }
        // The outer thread boundary catches destructor panics as well.
    }
}

fn caught(f: impl FnOnce() -> io::Result<()>) -> io::Result<()> {
    catch_unwind(AssertUnwindSafe(f)).unwrap_or_else(|panic| Err(panic_error(panic)))
}

fn panic_error(panic: Box<dyn std::any::Any + Send>) -> io::Error {
    let message = panic
        .downcast_ref::<String>()
        .map(String::as_str)
        .or_else(|| panic.downcast_ref::<&str>().copied())
        .unwrap_or("non-string payload");
    let error = io::Error::other(format!("panicked: {message}"));
    discard_caught(panic);
    error
}

fn discard_caught<T>(value: T) {
    if let Err(payload) = catch_unwind(AssertUnwindSafe(|| drop(value))) {
        // A secondary payload can itself have a panicking destructor. Leak only
        // that exceptional payload rather than recurse or abort during cleanup.
        std::mem::forget(payload);
    }
}

#[cfg(test)]
include!(concat!(
    env!("CARGO_MANIFEST_DIR"),
    "/tests/execution/workers.rs"
));

pub mod sharding {
    //! Local shard identities and unique activation capabilities. Cluster slots and
    //! transport rails have separate identities. Placement is immutable; IDs alone
    //! never authorize activation.
    use crate::{
        allocator::{self, Allocator, SlabShard},
        buffers::{ShardPool, WorkerPool},
        workers::{CpuId, NumaNodeId, WorkerId},
    };
    use std::{
        cell::RefCell,
        io,
        ops::Deref,
        rc::Rc,
        sync::{Arc, Mutex},
    };

    /// A validated logical index, independent of worker and CPU numbering.
    /// ```compile_fail
    /// use racer_dataplane::sharding::ShardId;
    /// let id = ShardId(0);
    /// ```
    #[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Hash)]
    pub struct ShardId(usize);
    impl ShardId {
        pub fn index(self) -> usize {
            self.0
        }
        pub(crate) fn at(index: usize) -> Self {
            Self(index)
        }
    }

    #[derive(Debug)]
    pub(crate) struct Identity;

    /// Read-only ownership and locality for one worker in a particular startup plan.
    #[derive(Clone, Debug)]
    pub struct Placement {
        pub(crate) worker: WorkerId,
        pub(crate) cpu: CpuId,
        pub(crate) node: NumaNodeId,
        pub(crate) shards: Vec<ShardId>,
        plan: Arc<Identity>,
        count: usize,
        workers: usize,
        initial: Arc<GenerationIdentity>,
    }
    impl Placement {
        pub fn worker_id(&self) -> WorkerId {
            self.worker
        }
        pub fn cpu_id(&self) -> CpuId {
            self.cpu
        }
        pub fn numa_node_id(&self) -> NumaNodeId {
            self.node
        }
        pub fn shard_ids(&self) -> &[ShardId] {
            &self.shards
        }
        pub fn shard_count(&self) -> usize {
            self.count
        }
        /// Authorize a fresh storage layout on this exact execution plan. Worker,
        /// CPU, NUMA and pool identities are unaffected. Each worker can claim its
        /// ordered assignments once, including when the shard count is unchanged.
        /// Existing validated slabs may exceed new-layout planning ceilings;
        /// LayoutPlan bounds replacements and activation checks slab geometry.
        pub fn storage_generation(&self, count: usize) -> io::Result<StorageGeneration> {
            if count < self.workers {
                return Err(invalid("storage shard count below execution worker count"));
            }
            Ok(StorageGeneration {
                identity: Arc::new(GenerationIdentity {
                    plan: self.plan.clone(),
                    count,
                    workers: self.workers,
                }),
                issued: Mutex::new(vec![false; self.workers]),
            })
        }
        /// Validate an external numeric identity against this plan, without granting ownership.
        pub fn shard_id(&self, index: usize) -> io::Result<ShardId> {
            if index < self.count {
                Ok(ShardId(index))
            } else {
                Err(invalid("shard index outside plan"))
            }
        }
    }

    pub(crate) fn placements(
        cores: Vec<(CpuId, NumaNodeId)>,
        count: usize,
    ) -> io::Result<Vec<Placement>> {
        if cores.is_empty() || count < cores.len() {
            return Err(invalid("invalid shard placement"));
        }
        let mut cpus = std::collections::BTreeSet::new();
        if cores.iter().any(|(cpu, _)| !cpus.insert(*cpu)) {
            return Err(invalid("duplicate worker CPU"));
        }
        let plan = Arc::new(Identity);
        let workers = cores.len();
        let initial = Arc::new(GenerationIdentity {
            plan: plan.clone(),
            count,
            workers,
        });
        Ok(cores
            .into_iter()
            .enumerate()
            .map(|(worker, (cpu, node))| Placement {
                worker: WorkerId(worker),
                cpu,
                node,
                count,
                workers,
                initial: initial.clone(),
                plan: plan.clone(),
                shards: (worker..count).step_by(workers).map(ShardId).collect(),
            })
            .collect())
    }

    #[derive(Debug)]
    pub(crate) struct GenerationIdentity {
        plan: Arc<Identity>,
        count: usize,
        workers: usize,
    }

    /// Nonforgeable generation authority, transferable to the resize coordinator.
    /// Keep this handle until every worker has prepared. Dropping it does not
    /// invalidate activated shards; retiring old caches is the runtime's job.
    /// ```compile_fail
    /// use racer_dataplane::sharding::StorageGeneration;
    /// let forged = StorageGeneration {};
    /// ```
    pub struct StorageGeneration {
        identity: Arc<GenerationIdentity>,
        issued: Mutex<Vec<bool>>,
    }
    impl StorageGeneration {
        pub fn shard_count(&self) -> usize {
            self.identity.count
        }
        pub fn worker_count(&self) -> usize {
            self.identity.workers
        }
        pub fn take_assignments(&self, context: &WorkerContext) -> io::Result<Vec<Assignment>> {
            if !Arc::ptr_eq(&self.identity.plan, &context.plan) {
                return Err(invalid("foreign execution plan"));
            }
            let mut issued = self.issued.lock().unwrap();
            if std::mem::replace(&mut issued[context.index], true) {
                return Err(invalid("generation assignments already issued"));
            }
            Ok((context.index..self.identity.count)
                .step_by(self.identity.workers)
                .map(|id| Assignment {
                    id: ShardId(id),
                    worker: context.worker,
                    generation: self.identity.clone(),
                })
                .collect())
        }
        pub(crate) fn matches(&self, state: &ShardState) -> bool {
            state
                .generation
                .as_ref()
                .is_some_and(|g| Arc::ptr_eq(g, &self.identity))
        }
    }

    /// Unique transferable authorization issued once per worker and generation.
    /// ```compile_fail
    /// use racer_dataplane::sharding::Assignment;
    /// fn duplicate(a: Assignment) { let _ = a.clone(); }
    /// ```
    /// ```compile_fail
    /// use racer_dataplane::sharding::Assignment;
    /// let forged = Assignment {};
    /// ```
    pub struct Assignment {
        id: ShardId,
        worker: WorkerId,
        generation: Arc<GenerationIdentity>,
    }
    impl Assignment {
        pub fn id(&self) -> ShardId {
            self.id
        }
        pub fn shard_count(&self) -> usize {
            self.generation.count
        }
    }

    /// Created by the runtime only after pinning succeeds. Cannot leave its thread.
    /// Clone into the driver to authorize later storage generations. Clones share
    /// the same once-only initial assignments, owner identity and pool binding.
    /// ```compile_fail
    /// use racer_dataplane::sharding::WorkerContext;
    /// fn send<T: Send>() {} send::<WorkerContext>();
    /// ```
    #[derive(Clone)]
    pub struct WorkerContext {
        placements: Arc<[Placement]>,
        index: usize,
        assignments: Rc<RefCell<Option<Vec<Assignment>>>>,
        owner: Rc<()>,
        pool: Rc<RefCell<Option<WorkerPool>>>,
    }
    impl Deref for WorkerContext {
        type Target = Placement;
        fn deref(&self) -> &Placement {
            &self.placements[self.index]
        }
    }
    impl WorkerContext {
        pub(crate) fn pinned(placements: Arc<[Placement]>, index: usize) -> Self {
            let p = &placements[index];
            let assignments = p
                .shards
                .iter()
                .map(|&id| Assignment {
                    id,
                    worker: p.worker,
                    generation: p.initial.clone(),
                })
                .collect();
            Self {
                placements,
                index,
                assignments: Rc::new(RefCell::new(Some(assignments))),
                owner: Rc::new(()),
                pool: Rc::new(RefCell::new(None)),
            }
        }
        pub fn take_assignments(&self) -> io::Result<Vec<Assignment>> {
            self.assignments
                .borrow_mut()
                .take()
                .ok_or_else(|| invalid("assignments already issued"))
        }
        pub(crate) fn identity(&self) -> &Rc<()> {
            &self.owner
        }
        pub(crate) fn bind_pool(&self, pool: &WorkerPool) -> io::Result<()> {
            if pool.numa_node_id() != self.node {
                return Err(invalid("foreign NUMA pool"));
            }
            let mut bound = self.pool.borrow_mut();
            if let Some(existing) = &*bound {
                if !existing.same_pool(pool) {
                    return Err(invalid("foreign worker pool"));
                }
            } else {
                *bound = Some(pool.clone());
            }
            Ok(())
        }
        pub(crate) fn check(&self, assignment: &Assignment) -> io::Result<()> {
            if assignment.worker != self.worker
                || !Arc::ptr_eq(&assignment.generation.plan, &self.plan)
                || assignment.generation.workers != self.workers
                || assignment.id.index() >= assignment.generation.count
                || assignment.id.index() % self.workers != self.index
            {
                return Err(invalid("foreign shard assignment"));
            }
            Ok(())
        }
        #[cfg(test)]
        pub(crate) fn test(count: usize) -> Self {
            Self::pinned(
                placements(vec![(CpuId(0), NumaNodeId(0))], count)
                    .unwrap()
                    .into(),
                0,
            )
        }
    }

    /// Activated shard: storage, admission history, accounting, and a logical view of
    /// the worker's NUMA pool. Mutation requires exclusive access.
    /// ```compile_fail
    /// use racer_dataplane::sharding::ShardState;
    /// fn send<T: Send>() {} send::<ShardState>();
    /// ```
    /// ```compile_fail
    /// use racer_dataplane::sharding::ShardState;
    /// fn sync<T: Sync>() {} sync::<ShardState>();
    /// ```
    /// ```compile_fail
    /// use racer_dataplane::{sharding::{Assignment, WorkerContext, ShardState}, allocator::{SlabShard, Config}, buffers::WorkerPool};
    /// fn twice(c: &WorkerContext, a: Assignment, s: SlabShard, p: &WorkerPool) {
    ///     let _ = ShardState::activate(c, a, s, p, Config::default());
    ///     let _ = ShardState::activate(c, a, s, p, Config::default());
    /// }
    /// ```
    pub struct ShardState {
        id: ShardId,
        pub(crate) slab: Arc<crate::allocator::SlabFile>,
        pub(crate) owner: Rc<()>,
        pub(crate) allocator: Allocator,
        pub(crate) buffers: Option<ShardPool>,
        pub(crate) generation: Option<Arc<GenerationIdentity>>,
    }
    impl ShardState {
        pub(crate) fn activate_empty(
            context: &WorkerContext,
            assignment: Assignment,
            empty: allocator::EmptyShard,
            pool: &WorkerPool,
        ) -> io::Result<Self> {
            context.check(&assignment)?;
            if empty.shard.id() != assignment.id || empty.shard.count() != assignment.shard_count()
            {
                return Err(invalid("empty slab does not match assignment"));
            }
            let buffers = pool.for_assignment(context, &assignment)?;
            Ok(Self {
                id: assignment.id,
                slab: empty.shard.file_identity(),
                owner: context.owner.clone(),
                generation: Some(assignment.generation),
                allocator: Allocator::open_empty(empty, allocator::Config::default())?,
                buffers: Some(buffers),
            })
        }
        pub fn activate(
            context: &WorkerContext,
            assignment: Assignment,
            slab: SlabShard,
            pool: &WorkerPool,
            config: allocator::Config,
        ) -> io::Result<Self> {
            context.check(&assignment)?;
            if slab.id() != assignment.id || slab.count() != assignment.shard_count() {
                return Err(invalid("slab does not match assignment"));
            }
            let buffers = pool.for_assignment(context, &assignment)?;
            let id = assignment.id;
            let generation = assignment.generation.clone();
            Ok(Self {
                id,
                slab: slab.file_identity(),
                owner: context.owner.clone(),
                allocator: Allocator::open_assigned(context, assignment, slab, config)?,
                buffers: Some(buffers),
                generation: Some(generation),
            })
        }
        pub fn id(&self) -> ShardId {
            self.id
        }
        pub(crate) fn validate_collection(context: &WorkerContext, shards: &[Self]) -> bool {
            let Some(first) = shards.first() else {
                return false;
            };
            let (count, workers) = match &first.generation {
                Some(g) if Arc::ptr_eq(&g.plan, &context.plan) => (g.count, g.workers),
                Some(_) => return false,
                None => (context.count, context.workers),
            };
            let ids = (context.index..count).step_by(workers);
            shards.len() == ids.clone().count()
                && shards.iter().zip(ids).all(|(s, id)| {
                    s.id.index() == id
                        && Rc::ptr_eq(&s.owner, context.identity())
                        && match (&first.generation, &s.generation) {
                            (Some(a), Some(b)) => Arc::ptr_eq(a, b),
                            (None, None) => true,
                            _ => false,
                        }
                })
        }
        #[cfg(test)]
        pub(crate) fn test(
            id: ShardId,
            owner: Rc<()>,
            slab: Arc<crate::allocator::SlabFile>,
            allocator: Allocator,
            _words: usize,
        ) -> Self {
            Self {
                id,
                slab,
                owner,
                allocator,
                buffers: None,
                generation: None,
            }
        }
    }
    fn invalid(message: &'static str) -> io::Error {
        io::Error::new(io::ErrorKind::InvalidInput, message)
    }

    #[cfg(test)]
    include!(concat!(
        env!("CARGO_MANIFEST_DIR"),
        "/tests/execution/sharding.rs"
    ));
}
