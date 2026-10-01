//! Stable local page-to-worker dispatch, independent of cluster placement.
//!
//! Construct worker-local service graphs on their selected threads. Rc-owned graphs
//! must never cross threads; only bounded commands and completion-safe leases do.
//! Drain flights before changing the worker map; no live remapping is implied.
//!
//! Each worker is an I/O shard. The I/O thread owns the service graph,
//! flights, storage shard, and admission. Page AEAD runs on a shared crypto thread
//! through bounded owned job/completion messages, retaining buffers and key leases
//! until completion even after cancellation. Queue wakeups and completion capacity
//! must permit progress when both threads share one CPU.

use super::{
    admission::Admission,
    affinity::{AffinityPlan, WorkerPair, current_cpus, pin_cpu, set_cpus},
    crypto::{self, CryptoClient, CryptoPort, IoCryptoPort},
    deadline::{Cancellation, Deadline, RequestScope},
    reactor::{Reactor, ReactorWake},
};
use crate::{
    error::{Error, Operation, Result},
    model::{Limits, ObjectId, PageId, RequestId, WorkerId},
};
use sha2::{Digest, Sha256};
#[cfg(test)]
use std::time::Instant;
use std::{
    collections::{HashMap, HashSet},
    marker::PhantomData,
    num::NonZeroUsize,
    rc::Rc,
    sync::{Arc, Condvar, Mutex, MutexGuard},
    task::{Context, Poll, Wake, Waker},
    thread::{self, JoinHandle},
    time::Duration,
};

const WORK_BUDGET: usize = 64;
// A large page is synchronous work. Yield to sibling shards after each job.
const CRYPTO_QUANTUM: usize = 1;
// Deadline checks, nonblocking accepts, and bounded round-robin passes still need
// a tick when no registered completion or cooperative continuation wakes us.
const IDLE_WAIT: Duration = Duration::from_millis(1);

/// Constructed on I/O; never move the local graph to the crypto thread.
/// ```compile_fail
/// use racer_dataplane::runtime::worker::WorkerRuntime;
/// fn require_send<T: Send>() {}
/// require_send::<WorkerRuntime>();
/// ```
pub struct WorkerRuntime {
    pub reactor: Rc<Reactor>,
    pub admission: Rc<Admission>,
    pub crypto: Rc<CryptoClient>,
}
/// Send endpoint is moved before construction on the crypto thread. No I/O
/// reactor, admission authority, or worker-local Rc can be supplied to the engine.
pub struct CryptoRuntime {
    pub port: CryptoPort,
}
type IoShard = (usize, WorkerPair, IoCryptoPort);
type CryptoShard = (usize, WorkerId, CryptoPort);
pub struct WorkerMap {
    workers: Vec<WorkerId>,
}
pub struct WorkerGroup<'a> {
    plan: AffinityPlan,
    control: Arc<Control>,
    threads: Vec<JoinHandle<()>>,
    generation: u64,
    borrowed: PhantomData<&'a dyn WorkerFactory>,
}

/// Shared construction recipe only. Each build runs on its own already-pinned
/// thread; returned local services/futures need not be Send. The I/O build owns
/// control/diagnostics as budgeted work, never extra userspace threads.
///
/// Rc-backed factories cannot cross the startup boundary:
/// ```compile_fail
/// use std::rc::Rc;
/// use racer_dataplane::{error::Result, model::WorkerId,
///     runtime::worker::{WorkerFactory, WorkerRuntime, WorkerService,
///                       CryptoRuntime, CryptoService}};
/// struct LocalFactory(Rc<()>);
/// impl WorkerFactory for LocalFactory {
///     fn build(&self, _: WorkerId, _: WorkerRuntime) -> Result<Box<dyn WorkerService>> { todo!() }
///     fn build_crypto(&self, _: WorkerId, _: CryptoRuntime) -> Result<Box<dyn CryptoService>> { todo!() }
/// }
/// ```
pub trait WorkerFactory: Sync {
    /// Per-worker limits. Application factories should return their validated
    /// configuration limits here; defaults allow bounded maximum-page progress.
    /// No Config is available to WorkerGroup::new, so this is the composition hook.
    fn limits(&self) -> Limits {
        let n = |value| NonZeroUsize::new(value).expect("positive default");
        Limits {
            plaintext_bytes: n(32 * 1024 * 1024),
            ciphertext_bytes: n(32 * 1024 * 1024),
            dirty_bytes: n(32 * 1024 * 1024),
            registered_bytes: n(32 * 1024 * 1024),
            request_context_bytes: n(4 * 1024 * 1024),
            flights: n(64),
            waiters_per_flight: n(64),
            queue_entries: n(64),
            connections_per_neighbor: n(2),
            client_connections: n(128),
            pipes: n(16),
            range_window_pages: n(2),
            header_bytes: n(32768),
            cached_rankings: n(128),
            cached_paths: n(128),
            retained_snapshots: n(2),
            metadata_entries: n(1024),
            relay_transfers: n(16),
        }
    }
    fn build(&self, worker: WorkerId, runtime: WorkerRuntime) -> Result<Box<dyn WorkerService>>;
    fn build_crypto(
        &self,
        worker: WorkerId,
        runtime: CryptoRuntime,
    ) -> Result<Box<dyn CryptoService>>;
}
pub trait WorkerService {
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    /// Reap crypto completions before new admission, including abandoned results.
    /// Forward the retained driver context to all service futures so cooperative
    /// continuations and cross-worker notifications interrupt the bounded idle wait.
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, work_budget: usize) -> Result<()>;
    fn stop_admission(&mut self) -> Result<()>;
    /// The future drives service-local tasks itself while the group independently
    /// polls reactor and crypto completions (the service is mutably borrowed).
    /// Drain reads/dirty writes while the engine remains live. Close submissions
    /// only after no I/O producer can submit; keep consuming until fully fenced.
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
}
pub trait CryptoService {
    /// Re-arm the worker wake before every drive. Implementations with a port
    /// forward this to CryptoPort::register_driver; lifecycle futures register
    /// their Context waker while polling the port themselves.
    fn register_driver(&self, _waker: &Waker) {}
    /// Lifecycle futures share an executor with sibling shards. Each poll must
    /// do bounded work and yield rather than blocking for another shard.
    fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    fn poll_budgeted(&mut self, work_budget: usize) -> Result<()>;
    /// After submission close, complete every accepted job while I/O reaps results.
    fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
    fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()>;
}

impl WorkerMap {
    /// Canonical worker order makes assignment independent of discovery order.
    /// Changing this set requires draining all flights first.
    pub fn new(mut workers: Vec<WorkerId>) -> Result<Self> {
        workers.sort_by_key(|worker| worker.0);
        if workers.is_empty() || workers.windows(2).any(|pair| pair[0] == pair[1]) {
            return Err(Error::InvalidConfiguration);
        }
        Ok(Self { workers })
    }

    /// Same owner as this object's page zero, without needing an ETag first.
    pub fn metadata_owner(&self, object: &ObjectId) -> Result<WorkerId> {
        self.select(object, 0)
    }
    pub fn owner(&self, page: &PageId) -> Result<WorkerId> {
        self.select(&page.version.object, page.number.0)
    }

    fn select(&self, object: &ObjectId, page: u64) -> Result<WorkerId> {
        if self.workers.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let mut hash = Sha256::new();
        hash.update(b"racer.local-worker.v1\0");
        hash.update((object.cache.0.len() as u64).to_be_bytes());
        hash.update(object.cache.0.as_bytes());
        hash.update(object.key.0);
        hash.update(page.to_be_bytes());
        let digest = hash.finalize();
        let index = u64::from_be_bytes(digest[..8].try_into().expect("eight digest bytes"))
            % self.workers.len() as u64;
        Ok(self.workers[index as usize])
    }
}
impl<'a> WorkerGroup<'a> {
    pub fn new(plan: AffinityPlan) -> Self {
        Self {
            plan,
            control: Arc::new(Control::new(0, 0, false)),
            threads: Vec::new(),
            generation: 0,
            borrowed: PhantomData,
        }
    }
    /// Driver-controlled adapter over the same scoped execution used by `run`.
    /// The coordinator becomes the first I/O worker, not an extra helper thread.
    /// The external driver still requires one slot in the whole-process budget.
    pub fn start(
        &mut self,
        factory: Arc<dyn WorkerFactory + Send>,
        scope: &RequestScope,
    ) -> Result<()> {
        self.start_with_allocator(factory, scope, crypto::try_pair)
    }

    fn start_with_allocator(
        &mut self,
        factory: Arc<dyn WorkerFactory + Send>,
        scope: &RequestScope,
        allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>
        + Send
        + 'static,
    ) -> Result<()> {
        self.prepare(false)?;
        let plan = AffinityPlan {
            pairs: self.plan.pairs.clone(),
            max_threads: self.plan.max_threads,
        };
        let control = self.control.clone();
        let startup = scope.clone();
        let generation = self.generation;
        let handle = thread::Builder::new()
            .name(format!("racer-io-{}", plan.pairs[0].worker.0))
            .spawn(move || {
                let mut group = WorkerGroup::new(plan);
                group.control = control.clone();
                group.generation = generation;
                match std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    group.run_prepared(&*factory, &startup, false, allocate)
                })) {
                    Ok(Ok(())) => (),
                    Ok(Err(error)) => control.fail(error),
                    Err(_) => control.fail(Error::Io),
                }
            })
            .map_err(|_| Error::Io)?;
        self.threads.push(handle);
        let result = self
            .control
            .wait_for(scope, |state| state.ready == state.total);
        if let Err(error) = result {
            self.control.fail(error);
            self.control.set_phase(Phase::Shutdown);
            let _ = self.join();
            return Err(error);
        }
        Ok(())
    }
    /// Stop I/O admission first. Drive both roles during I/O drain, then close
    /// crypto submissions and reap all completions. Deadline expiry requests
    /// cancellation but does not release resources before completion fences.
    pub fn drain(&mut self, scope: &RequestScope) -> Result<()> {
        self.control.set_phase(Phase::Drain);
        // Timeout reports cancellation to the caller; live threads retain their
        // resources and continue fencing. join/Drop still wait for those fences.
        self.control
            .wait_for(scope, |state| state.drained == state.total)
    }
    /// After drain, shut down both services and fence kernel/NIC references.
    /// Never terminate crypto with an outstanding job or unconsumed completion.
    pub fn shutdown(&mut self, scope: &RequestScope) -> Result<()> {
        self.control.set_phase(Phase::Shutdown);
        self.control
            .wait_for(scope, |state| state.done == state.total)
    }
    /// Join every I/O and unique crypto OS thread, including partial startup.
    /// Cannot succeed while a service can still access its retained resources.
    pub fn join(&mut self) -> Result<()> {
        self.control.set_phase(Phase::Shutdown);
        for handle in self.threads.drain(..) {
            if handle.join().is_err() {
                self.control.fail(Error::Io);
            }
        }
        self.control.result()
    }
    /// Ordered start, budgeted drive, drain, shutdown, and join (also on failure).
    pub fn run(&mut self, factory: &'a dyn WorkerFactory) -> Result<()> {
        let scope = lifecycle_scope()?;
        self.run_inner(factory, &scope, false)
    }

    /// Unlike `run`, also treats this scope's cancellation/deadline as a request
    /// to stop the steady-state loop. Neither form truncates completion fencing.
    pub fn run_with_scope(
        &mut self,
        factory: &'a dyn WorkerFactory,
        scope: &RequestScope,
    ) -> Result<()> {
        self.run_inner(factory, scope, true)
    }

    fn prepare(&mut self, caller_is_worker: bool) -> Result<()> {
        if !self.threads.is_empty() || self.plan.pairs.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let threads = self
            .plan
            .pairs
            .len()
            .checked_add(self.plan.crypto_groups().len())
            .and_then(|n| n.checked_add(usize::from(!caller_is_worker)))
            .ok_or(Error::InvalidConfiguration)?;
        if threads > self.plan.max_threads {
            return Err(Error::InvalidConfiguration);
        }
        let allowed = current_cpus()?;
        let mut workers = HashSet::new();
        let mut crypto_locations = HashMap::new();
        for pair in &self.plan.pairs {
            if !workers.insert(pair.worker)
                || !allowed.contains(&pair.io.cpu)
                || !allowed.contains(&pair.crypto.cpu)
            {
                return Err(Error::InvalidConfiguration);
            }
            if matches!((pair.io.numa_node, pair.crypto.numa_node), (Some(io), Some(crypto)) if io != crypto)
            {
                return Err(Error::InvalidConfiguration);
            }
            // Public plan literals bypass discovery. One execution CPU must have
            // one consistent topology record across all of its shared shards.
            let location = (pair.crypto.package, pair.crypto.core, pair.crypto.numa_node);
            if crypto_locations
                .insert(pair.crypto.cpu, location)
                .is_some_and(|previous| previous != location)
            {
                return Err(Error::InvalidConfiguration);
            }
        }
        self.generation = self
            .generation
            .checked_add(1)
            .ok_or(Error::InvalidConfiguration)?;
        self.control = Arc::new(Control::new(
            self.plan.pairs.len(),
            self.plan.crypto_groups().len(),
            caller_is_worker,
        ));
        Ok(())
    }

    fn allocate_group(
        &self,
        indices: &[usize],
        capacity: NonZeroUsize,
        allocate: &mut impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> (Vec<IoShard>, Vec<CryptoShard>) {
        let mut ios = Vec::new();
        let mut engines = Vec::new();
        for &index in indices {
            let pair = self.plan.pairs[index].clone();
            // Report allocation panics before the scoped join so already-started
            // siblings see Drain rather than waiting forever for missing I/O.
            let allocated = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                allocate(pair.worker, self.generation, capacity)
            }))
            .unwrap_or(Err(Error::Io));
            match allocated {
                Ok((io, engine)) => {
                    engines.push((index, pair.worker, engine));
                    ios.push((index, pair, io));
                }
                Err(error) => {
                    self.control.fail(error);
                    break;
                }
            }
        }
        (ios, engines)
    }

    fn close_unlaunched(&self, launched: &[bool]) {
        for (index, launched) in launched.iter().enumerate() {
            if !launched {
                self.control.close_io(index);
                self.control.fence_io(index);
            }
        }
    }

    fn run_inner(
        &mut self,
        factory: &'a dyn WorkerFactory,
        scope: &RequestScope,
        check_scope: bool,
    ) -> Result<()> {
        self.run_with_allocator(factory, scope, check_scope, crypto::try_pair)
    }

    fn run_with_allocator(
        &mut self,
        factory: &'a dyn WorkerFactory,
        scope: &RequestScope,
        check_scope: bool,
        allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> Result<()> {
        self.prepare(true)?;
        self.run_prepared(factory, scope, check_scope, allocate)
    }

    fn run_prepared(
        &mut self,
        factory: &dyn WorkerFactory,
        scope: &RequestScope,
        check_scope: bool,
        mut allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> Result<()> {
        let original_affinity = current_cpus()?;
        let limits = factory.limits();
        self.control.lock().check_scope = check_scope;
        thread::scope(|threads| {
            let mut handles = Vec::new();
            let mut first_io = None;
            let mut launched = vec![false; self.plan.pairs.len()];
            for indices in self.plan.crypto_groups() {
                let (ios, engines) =
                    self.allocate_group(&indices, limits.queue_entries, &mut allocate);
                if engines.is_empty() {
                    break;
                }
                let control = self.control.clone();
                let startup = scope.clone();
                let cpu = self.plan.pairs[indices[0]].crypto.cpu;
                match thread::Builder::new()
                    .name(format!("racer-crypto-{cpu}"))
                    .spawn_scoped(threads, move || {
                        crypto_thread(factory, cpu, engines, startup, control)
                    }) {
                    Ok(handle) => handles.push(handle),
                    Err(_) => {
                        self.control.fail(Error::Io);
                        break;
                    }
                }
                for (index, pair, io) in ios {
                    if index == 0 {
                        first_io = Some((pair, io));
                        launched[index] = true;
                        continue;
                    }
                    let control = self.control.clone();
                    let startup = scope.clone();
                    let limits = limits.clone();
                    match thread::Builder::new()
                        .name(format!("racer-io-{}", pair.worker.0))
                        .spawn_scoped(threads, move || {
                            io_thread(factory, pair, io, limits, startup, control, index)
                        }) {
                        Ok(handle) => {
                            handles.push(handle);
                            launched[index] = true;
                        }
                        Err(_) => {
                            self.control.close_io(index);
                            self.control.fence_io(index);
                            self.control.fail(Error::Io);
                            break;
                        }
                    }
                }
                if self.control.result().is_err() {
                    break;
                }
            }
            self.close_unlaunched(&launched);
            if let Some((pair, io)) = first_io {
                if std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                    io_thread(
                        factory,
                        pair,
                        io,
                        limits,
                        scope.clone(),
                        self.control.clone(),
                        0,
                    )
                }))
                .is_err()
                {
                    self.control.fail(Error::Io);
                }
            }
            for handle in handles {
                if handle.join().is_err() {
                    self.control.fail(Error::Io);
                }
            }
        });
        let restore = set_cpus(&original_affinity);
        self.control.result().and(restore)
    }
}

impl Drop for WorkerGroup<'_> {
    fn drop(&mut self) {
        let _ = self.join();
    }
}

#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
enum Phase {
    Running,
    Drain,
    Shutdown,
}

struct State {
    phase: Phase,
    // Lifecycle counters count actual execution threads, not service instances.
    total: usize,
    ready: usize,
    drained: usize,
    done: usize,
    error: Option<Error>,
    // Handshake/fence state remains indexed by I/O shard.
    crypto_ready: Vec<bool>,
    io_closed: Vec<bool>,
    io_fenced: Vec<bool>,
    auto_shutdown: bool,
    check_scope: bool,
}

struct Control {
    state: Mutex<State>,
    changed: Condvar,
}

impl Control {
    fn new(io_count: usize, crypto_count: usize, auto_shutdown: bool) -> Self {
        Self {
            state: Mutex::new(State {
                phase: Phase::Running,
                total: io_count + crypto_count,
                ready: 0,
                drained: 0,
                done: 0,
                error: None,
                crypto_ready: vec![false; io_count],
                io_closed: vec![false; io_count],
                io_fenced: vec![false; io_count],
                auto_shutdown,
                check_scope: false,
            }),
            changed: Condvar::new(),
        }
    }
    fn lock(&self) -> MutexGuard<'_, State> {
        self.state.lock().unwrap_or_else(|error| error.into_inner())
    }
    fn result(&self) -> Result<()> {
        self.lock().error.map_or(Ok(()), Err)
    }
    fn set_phase(&self, phase: Phase) {
        let mut state = self.lock();
        state.phase = state.phase.max(phase);
        self.changed.notify_all();
    }
    fn fail(&self, error: Error) {
        let mut state = self.lock();
        state.error.get_or_insert(error);
        state.phase = state.phase.max(Phase::Drain);
        self.changed.notify_all();
    }
    fn close_io(&self, index: usize) {
        self.lock().io_closed[index] = true;
        self.changed.notify_all();
    }
    fn fence_io(&self, index: usize) {
        self.lock().io_fenced[index] = true;
        self.changed.notify_all();
    }
    fn wait_for(&self, scope: &RequestScope, done: impl Fn(&State) -> bool) -> Result<()> {
        let mut state = self.lock();
        loop {
            if let Some(error) = state.error {
                return Err(error);
            }
            if done(&state) {
                return Ok(());
            }
            scope.check()?;
            state = self
                .changed
                .wait_timeout(state, IDLE_WAIT)
                .unwrap_or_else(|error| error.into_inner())
                .0;
        }
    }
    fn stopping(&self, scope: &RequestScope) -> bool {
        let check = self.lock().check_scope;
        if check {
            if let Err(error) = scope.check() {
                self.fail(error);
            }
        }
        self.lock().phase != Phase::Running
    }
    fn drained(&self) {
        let mut state = self.lock();
        state.drained += 1;
        self.changed.notify_all();
        while state.phase != Phase::Shutdown && !state.auto_shutdown && state.error.is_none() {
            state = self
                .changed
                .wait(state)
                .unwrap_or_else(|error| error.into_inner());
        }
    }
}

// Even on unwinding, wake the other role and make partial startup observable.
struct ThreadExit {
    control: Arc<Control>,
    io: Option<usize>,
}
impl Drop for ThreadExit {
    fn drop(&mut self) {
        if let Some(index) = self.io {
            self.control.close_io(index);
            self.control.fence_io(index);
        }
        if thread::panicking() {
            self.control.fail(Error::Io);
        }
        self.control.lock().done += 1;
        self.control.changed.notify_all();
    }
}

struct ThreadWake(thread::Thread, Option<ReactorWake>);
impl Wake for ThreadWake {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }
    fn wake_by_ref(self: &Arc<Self>) {
        self.0.unpark();
        if let Some(reactor) = &self.1 {
            let _ = reactor.wake();
        }
    }
}

fn driver_waker(runtime: Option<&WorkerRuntime>) -> Result<Waker> {
    let reactor = runtime.map(|runtime| runtime.reactor.waker()).transpose()?;
    Ok(Waker::from(Arc::new(ThreadWake(
        thread::current(),
        reactor,
    ))))
}

/// Start may be interrupted; teardown may not. During an I/O service future,
/// independently drive reactor CQEs and crypto completions. A CryptoService's
/// async lifecycle methods must drive their own engine: &mut self prevents the
/// group from also calling poll_budgeted until that future completes.
fn drive(
    mut operation: Operation<'_, ()>,
    runtime: Option<&WorkerRuntime>,
    scope: &RequestScope,
    control: &Control,
    startup: bool,
    waker: &Waker,
) -> Result<()> {
    let mut cx = Context::from_waker(waker);
    let cancellation = startup
        .then(|| scope.cancellation.subscribe())
        .transpose()?;
    if let Some(cancellation) = &cancellation {
        cancellation.register(waker);
    }
    let mut error = None;
    loop {
        if startup {
            scope.check()?;
            if control.lock().phase != Phase::Running {
                return Err(Error::Cancelled);
            }
        }
        if let Some(runtime) = runtime {
            runtime.crypto.register_driver(waker);
            if let Err(failure) = poll_runtime(runtime) {
                control.fail(failure);
                error.get_or_insert(failure);
            }
        }
        if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
            return error.map_or(result, Err);
        }
        if let Some(runtime) = runtime {
            if let Err(failure) = runtime.reactor.wait(IDLE_WAIT) {
                control.fail(failure);
                error.get_or_insert(failure);
            }
        } else {
            thread::park_timeout(IDLE_WAIT);
        }
    }
}

fn lifecycle_scope() -> Result<RequestScope> {
    Ok(RequestScope {
        body_deadlines: None,
        request: RequestId([0; 16]),
        deadline: Deadline(crate::runtime::environment::now() + Duration::from_secs(30)),
        cancellation: Cancellation::new()?,
    })
}

fn poll_runtime(runtime: &WorkerRuntime) -> Result<()> {
    let crypto = runtime.crypto.poll_budgeted(WORK_BUDGET);
    let reactor = runtime.reactor.poll_budgeted(WORK_BUDGET);
    crypto.and(reactor.map(|_| ()))
}

fn fence_runtime(runtime: &WorkerRuntime, control: &Control, scope: &RequestScope, waker: &Waker) {
    // Service drain has ended: detach any delivery waiters it left behind, while
    // still retaining every accepted job until the engine returns its completion.
    let fence_scope = lifecycle_scope().unwrap_or_else(|_| scope.clone());
    record(control, fence_scope.cancel());
    record(
        control,
        drive(
            Box::pin(async {
                let (crypto, reactor) =
                    futures::join!(runtime.crypto.drain(&fence_scope), runtime.reactor.drain());
                crypto.and(reactor)
            }),
            Some(runtime),
            &fence_scope,
            control,
            false,
            waker,
        ),
    );
    // Backend failures do not relax ownership fences. A permanently failed
    // backend can therefore prevent join; leaking live owners is not success.
    let waker = Waker::from(Arc::new(ThreadWake(thread::current(), None)));
    while runtime.crypto.outstanding() != 0 || runtime.reactor.in_flight() != 0 {
        runtime.crypto.register_driver(&waker);
        record(control, poll_runtime(runtime));
        thread::park_timeout(IDLE_WAIT);
    }
}

fn record(control: &Control, result: Result<()>) {
    if let Err(error) = result {
        control.fail(error);
    }
}

fn io_thread(
    factory: &dyn WorkerFactory,
    pair: WorkerPair,
    port: IoCryptoPort,
    limits: Limits,
    startup: RequestScope,
    control: Arc<Control>,
    index: usize,
) {
    let _exit = ThreadExit {
        control: control.clone(),
        io: Some(index),
    };
    if let Err(error) = pin_cpu(pair.io.cpu) {
        control.fail(error);
        return;
    }
    let admission = Rc::new(Admission::new(limits));
    let runtime = WorkerRuntime {
        reactor: Rc::new(Reactor::new(admission.clone())),
        admission,
        crypto: Rc::new(CryptoClient::new(port)),
    };
    // Acquire once while the reactor is live. Its wake descriptor remains valid
    // during shutdown; asking for a new driver after drain would reinitialize a
    // stopped reactor and skip the service's shutdown future.
    let waker = match driver_waker(Some(&runtime)) {
        Ok(waker) => waker,
        Err(error) => {
            control.fail(error);
            runtime.admission.stop();
            record(&control, runtime.crypto.close_submissions());
            return;
        }
    };
    // No application graph is constructed until the crypto role is operational.
    let ready = control.wait_for(&startup, |state| state.crypto_ready[index]);
    let mut service = match ready.and_then(|()| {
        factory.build(
            pair.worker,
            WorkerRuntime {
                reactor: runtime.reactor.clone(),
                admission: runtime.admission.clone(),
                crypto: runtime.crypto.clone(),
            },
        )
    }) {
        Ok(service) => service,
        Err(error) => {
            control.fail(error);
            runtime.admission.stop();
            record(&control, runtime.crypto.close_submissions());
            control.close_io(index);
            fence_runtime(&runtime, &control, &startup, &waker);
            return;
        }
    };
    let started = drive(
        service.start(&startup),
        Some(&runtime),
        &startup,
        &control,
        true,
        &waker,
    );
    if started.is_ok() {
        control.lock().ready += 1;
        control.changed.notify_all();
        let check_scope = control.lock().check_scope;
        let _cancellation = if check_scope {
            match startup.cancellation.subscribe() {
                Ok(registration) => {
                    registration.register(&waker);
                    Some(registration)
                }
                Err(error) => {
                    control.fail(error);
                    None
                }
            }
        } else {
            None
        };
        let mut cx = Context::from_waker(&waker);
        while !control.stopping(&startup) {
            runtime.crypto.register_driver(&waker);
            if let Err(error) =
                poll_runtime(&runtime).and_then(|()| service.poll_budgeted(&mut cx, WORK_BUDGET))
            {
                control.fail(error);
                break;
            }
            // Real wakes interrupt this fallback for deadlines and nonblocking accepts.
            if let Err(error) = runtime.reactor.wait(IDLE_WAIT) {
                control.fail(error);
                break;
            }
        }
    } else {
        record(&control, started);
    }
    runtime.admission.stop();
    record(&control, service.stop_admission());
    let teardown = lifecycle_scope().unwrap_or_else(|_| startup.clone());
    record(
        &control,
        drive(
            service.drain(&teardown),
            Some(&runtime),
            &teardown,
            &control,
            false,
            &waker,
        ),
    );
    record(&control, runtime.crypto.close_submissions());
    control.close_io(index);
    // Do not let a canceled service future authorize dropping accepted work.
    fence_runtime(&runtime, &control, &teardown, &waker);
    control.fence_io(index);
    control.drained();
    record(
        &control,
        drive(
            service.shutdown(&teardown),
            Some(&runtime),
            &teardown,
            &control,
            false,
            &waker,
        ),
    );
    fence_runtime(&runtime, &control, &teardown, &waker);
}

fn crypto_thread(
    factory: &dyn WorkerFactory,
    cpu: usize,
    ports: Vec<CryptoShard>,
    startup: RequestScope,
    control: Arc<Control>,
) {
    let _exit = ThreadExit {
        control: control.clone(),
        io: None,
    };
    if let Err(error) = pin_cpu(cpu) {
        control.fail(error);
        return;
    }
    let waker = Waker::from(Arc::new(ThreadWake(thread::current(), None)));
    let mut services = Vec::new();
    // Native contexts and other !Send service state are constructed and destroyed
    // here, never on the spawning thread. A later build failure must still tear
    // down every already-built service.
    for (index, worker, port) in ports {
        port.register_driver(&waker);
        match factory.build_crypto(worker, CryptoRuntime { port }) {
            Ok(service) => services.push((index, service)),
            Err(error) => {
                control.fail(error);
                break;
            }
        }
    }
    let _cancellation = match startup.cancellation.subscribe() {
        Ok(registration) => {
            registration.register(&waker);
            Some(registration)
        }
        Err(error) => {
            control.fail(error);
            None
        }
    };
    let mut ready = false;
    let indices = services.iter().map(|(index, _)| *index).collect::<Vec<_>>();
    {
        let operations = services
            .iter_mut()
            .map(|(index, service)| {
                crypto_shard(*index, &mut **service, &startup, &control, &waker)
            })
            .collect();
        drive_crypto_group(operations, &control, &waker, || {
            let mut state = control.lock();
            if !ready
                && !indices.is_empty()
                && indices.iter().all(|index| state.crypto_ready[*index])
            {
                state.ready += 1;
                ready = true;
                control.changed.notify_all();
            }
        });
    }
    control.drained();
    let teardown = lifecycle_scope().unwrap_or_else(|_| startup.clone());
    let operations = services
        .iter_mut()
        .map(|(_, service)| {
            service.register_driver(&waker);
            service.shutdown(&teardown)
        })
        .collect();
    drive_crypto_group(operations, &control, &waker, || {});
}

fn crypto_shard<'a>(
    index: usize,
    service: &'a mut dyn CryptoService,
    startup: &'a RequestScope,
    control: &'a Control,
    waker: &'a Waker,
) -> Operation<'a, ()> {
    Box::pin(async move {
        service.register_driver(waker);
        let started = {
            let mut operation = service.start(startup);
            std::future::poll_fn(|cx| {
                startup.check()?;
                if control.lock().phase != Phase::Running {
                    return Poll::Ready(Err(Error::Cancelled));
                }
                operation.as_mut().poll(cx)
            })
            .await
        };
        if started.is_ok() {
            control.lock().crypto_ready[index] = true;
            control.changed.notify_all();
        } else {
            record(control, started);
        }
        // One page per poll, including during I/O drain and after errors. Once
        // submissions close, the service's cooperative drain flushes any backend
        // work. Sibling shards remain runnable while that future is pending.
        std::future::poll_fn(|_| {
            service.register_driver(waker);
            record(control, service.poll_budgeted(CRYPTO_QUANTUM));
            if control.lock().io_closed[index] {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        let teardown = lifecycle_scope().unwrap_or_else(|_| startup.clone());
        service.register_driver(waker);
        record(control, service.drain(&teardown).await);
        // Publication is not consumption. Keep native state alive until I/O has
        // reaped every completion, including abandoned/canceled jobs.
        std::future::poll_fn(|_| {
            service.register_driver(waker);
            record(control, service.poll_budgeted(CRYPTO_QUANTUM));
            if control.lock().io_fenced[index] {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        Ok(())
    })
}

/// Poll every shard once per pass, rotating the first shard. Never block on one
/// shard's start/drain/shutdown future while another shard needs engine progress.
fn drive_crypto_group(
    operations: Vec<Operation<'_, ()>>,
    control: &Control,
    waker: &Waker,
    mut after_pass: impl FnMut(),
) {
    let mut operations = operations.into_iter().map(Some).collect::<Vec<_>>();
    let mut remaining = operations.len();
    let mut first = 0;
    let mut passes = 0;
    let mut cx = Context::from_waker(waker);
    while remaining != 0 {
        for offset in 0..operations.len() {
            let index = (first + offset) % operations.len();
            if let Some(operation) = &mut operations[index] {
                if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
                    record(control, result);
                    operations[index] = None;
                    remaining -= 1;
                }
            }
        }
        after_pass();
        first = (first + 1) % operations.len();
        passes += 1;
        if remaining != 0 && passes == WORK_BUDGET {
            thread::park_timeout(IDLE_WAIT);
            passes = 0;
        }
    }
}

#[cfg(test)]
fn colocated_plan(max_threads: usize, workers: u16) -> AffinityPlan {
    let location = super::affinity::CpuLocation {
        cpu: *current_cpus().unwrap().first().unwrap(),
        package: 0,
        core: 0,
        numa_node: None,
    };
    AffinityPlan {
        pairs: (0..workers)
            .map(|id| WorkerPair {
                worker: WorkerId(id),
                io: location.clone(),
                crypto: location.clone(),
                nic: None,
            })
            .collect(),
        max_threads,
    }
}

#[cfg(test)]
mod shared_tests {
    use super::*;
    use crate::{
        memory::pool::BufferPool,
        model::{ResourceClass, *},
        runtime::crypto::{CryptoInput, CryptoOutput},
        security::{
            aead::PageCryptoEngine,
            identity::{KeyPurpose, Keyring},
        },
    };
    use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};

    #[derive(Default)]
    struct Observed {
        events: Mutex<Vec<(u16, &'static str, thread::ThreadId)>>,
        polls: Mutex<Vec<u16>>,
        submitted: AtomicUsize,
        release: AtomicBool,
        io_draining: AtomicUsize,
        shutdown: AtomicUsize,
        completed: AtomicUsize,
        native: Mutex<Vec<crate::rdma::lifecycle::IoPort>>,
    }
    impl Observed {
        fn event(&self, worker: WorkerId, name: &'static str) {
            self.events
                .lock()
                .unwrap()
                .push((worker.0, name, thread::current().id()));
        }
    }

    #[derive(Clone, Copy, PartialEq, Eq)]
    enum Failure {
        None,
        BuildCrypto,
        StartCrypto,
        PendingStart,
        BuildIo,
        StartIo,
        PollCrypto,
        DrainCrypto,
        ShutdownCrypto,
    }

    struct Factory {
        observed: Arc<Observed>,
        failure: Failure,
        jobs: bool,
        roundtrip: bool,
        cpu: usize,
        second_crypto_cpu: usize,
        native: bool,
    }

    fn fixture(cap: usize, failure: Failure, jobs: bool) -> (AffinityPlan, Factory) {
        let plan = colocated_plan(cap, 2);
        let cpu = plan.pairs[0].io.cpu;
        (
            plan,
            Factory {
                observed: Arc::new(Observed::default()),
                failure,
                jobs,
                roundtrip: false,
                cpu,
                second_crypto_cpu: cpu,
                native: false,
            },
        )
    }

    impl WorkerFactory for Factory {
        fn limits(&self) -> Limits {
            let mut limits = crate::test_support::cluster::config(false).limits;
            limits.queue_entries = NonZeroUsize::new(4).unwrap();
            limits
        }
        fn build(
            &self,
            worker: WorkerId,
            runtime: WorkerRuntime,
        ) -> Result<Box<dyn WorkerService>> {
            assert_eq!(
                current_cpus().unwrap(),
                std::collections::BTreeSet::from([self.cpu])
            );
            self.observed.event(worker, "io-build");
            if worker.0 == 1 && self.failure == Failure::BuildIo {
                return Err(Error::InvalidRequest);
            }
            Ok(Box::new(Io {
                worker,
                runtime,
                observed: self.observed.clone(),
                keys: crate::security::identity::keyring_tests::keys(),
                failure: self.failure,
                jobs: self.jobs,
                roundtrip: self.roundtrip,
            }))
        }
        fn build_crypto(
            &self,
            worker: WorkerId,
            runtime: CryptoRuntime,
        ) -> Result<Box<dyn CryptoService>> {
            let cpu = if worker.0 == 1 {
                self.second_crypto_cpu
            } else {
                self.cpu
            };
            assert_eq!(
                current_cpus().unwrap(),
                std::collections::BTreeSet::from([cpu])
            );
            self.observed.event(worker, "crypto-build");
            if worker.0 == 1 && self.failure == Failure::BuildCrypto {
                return Err(Error::Unauthorized);
            }
            let engine = Engine {
                worker,
                engine: PageCryptoEngine::new(runtime),
                observed: self.observed.clone(),
                failure: self.failure,
                local: Rc::new(thread::current().id()),
            };
            if self.native {
                let (io, native) = crate::rdma::lifecycle::pair(1)?;
                self.observed.native.lock().unwrap().push(io);
                Ok(Box::new(crate::rdma::lifecycle::WithNative::new(
                    engine, native,
                )))
            } else {
                Ok(Box::new(engine))
            }
        }
    }

    struct Io {
        worker: WorkerId,
        runtime: WorkerRuntime,
        observed: Arc<Observed>,
        keys: Keyring,
        failure: Failure,
        jobs: bool,
        roundtrip: bool,
    }

    impl Io {
        fn input(&self) -> CryptoInput {
            let cache = CacheId(crate::security::identity::tests::CACHE.into());
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
                plaintext: BufferPool::new(self.runtime.admission.clone())
                    .plaintext(
                        self.runtime
                            .admission
                            .reserve(Some(&cache), ResourceClass::Plaintext, 1)
                            .unwrap(),
                        1,
                    )
                    .unwrap(),
                ciphertext: self
                    .runtime
                    .admission
                    .reserve(Some(&cache), ResourceClass::Ciphertext, 17)
                    .unwrap(),
            }
        }
    }

    impl WorkerService for Io {
        fn start<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.observed.event(self.worker, "io-start");
                if self.roundtrip {
                    let cache = CacheId(crate::security::identity::tests::CACHE.into());
                    let key = self.keys.active(&cache, KeyPurpose::Page)?;
                    let CryptoOutput::Encrypted(plain, ciphertext) = self
                        .runtime
                        .crypto
                        .execute(self.input(), key, scope)
                        .await?
                    else {
                        panic!("encrypted output")
                    };
                    drop(plain);
                    let key = self.keys.active(&cache, KeyPurpose::Page)?;
                    let input = CryptoInput::Decrypt {
                        ciphertext,
                        plaintext: self.runtime.admission.reserve(
                            Some(&cache),
                            ResourceClass::Plaintext,
                            1,
                        )?,
                    };
                    let CryptoOutput::Decrypted(plain, _) =
                        self.runtime.crypto.execute(input, key, scope).await?
                    else {
                        panic!("decrypted output")
                    };
                    assert_eq!(plain.bytes(), &[0]);
                    self.observed.completed.fetch_add(1, Ordering::SeqCst);
                }
                if self.jobs {
                    // Submit several real AEAD jobs, abandoning delivery while their
                    // bounded queue permits and allocations remain in flight.
                    for _ in 0..4 {
                        let key = self
                            .keys
                            .active(
                                &CacheId(crate::security::identity::tests::CACHE.into()),
                                KeyPurpose::Page,
                            )
                            .unwrap();
                        let mut operation = self.runtime.crypto.execute(self.input(), key, scope);
                        assert!(
                            operation
                                .as_mut()
                                .poll(&mut Context::from_waker(futures::task::noop_waker_ref()))
                                .is_pending()
                        );
                        drop(operation);
                        self.observed.submitted.fetch_add(1, Ordering::SeqCst);
                    }
                }
                if self.worker.0 == 1 && self.failure == Failure::StartIo {
                    Err(Error::InvalidRequest)
                } else {
                    Ok(())
                }
            })
        }
        fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
            Ok(())
        }
        fn stop_admission(&mut self) -> Result<()> {
            self.observed.event(self.worker, "io-stop");
            Ok(())
        }
        fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.observed.event(self.worker, "io-drain");
                self.observed.io_draining.fetch_add(1, Ordering::SeqCst);
                Ok(())
            })
        }
        fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                assert_eq!(self.runtime.crypto.outstanding(), 0);
                assert_eq!(self.runtime.admission.used(ResourceClass::Plaintext), 0);
                assert_eq!(self.runtime.admission.used(ResourceClass::Ciphertext), 0);
                self.observed.event(self.worker, "io-shutdown");
                Ok(())
            })
        }
    }

    struct Engine {
        worker: WorkerId,
        engine: PageCryptoEngine,
        observed: Arc<Observed>,
        failure: Failure,
        local: Rc<thread::ThreadId>,
    }
    impl Drop for Engine {
        fn drop(&mut self) {
            assert_eq!(*self.local, thread::current().id());
            self.observed.event(self.worker, "crypto-drop");
        }
    }
    impl CryptoService for Engine {
        fn register_driver(&self, waker: &Waker) {
            self.engine.register_driver(waker);
        }
        fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(std::future::poll_fn(move |_| {
                if self.worker.0 == 1 {
                    if self.failure == Failure::StartCrypto {
                        return Poll::Ready(Err(Error::Unauthorized));
                    }
                    if self.failure == Failure::PendingStart {
                        return Poll::Pending;
                    }
                }
                self.observed.event(self.worker, "crypto-start");
                Poll::Ready(Ok(()))
            }))
        }
        fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
            assert_eq!(*self.local, thread::current().id());
            assert_eq!(budget, 1, "one page per shard per pass");
            let mut polls = self.observed.polls.lock().unwrap();
            if polls.len() < 128 {
                polls.push(self.worker.0);
            }
            drop(polls);
            // Retain outstanding jobs until tests request release or drain begins.
            if self.observed.release.load(Ordering::SeqCst)
                || self.observed.io_draining.load(Ordering::SeqCst) > 0
            {
                self.engine.poll_budgeted(budget)?;
            }
            if self.worker.0 == 1 && self.failure == Failure::PollCrypto {
                Err(Error::Io)
            } else {
                Ok(())
            }
        }
        fn drain<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.observed.event(self.worker, "crypto-drain");
                // A pending drain of one shard needs the other shard to drain. This
                // deadlocks if the shared thread blocks on lifecycle futures in order.
                std::future::poll_fn(|_| {
                    let sibling_entered =
                        self.observed
                            .events
                            .lock()
                            .unwrap()
                            .iter()
                            .any(|(worker, name, _)| {
                                *worker != self.worker.0 && *name == "crypto-drain"
                            });
                    if self.worker.0 == 0
                        && self.failure != Failure::BuildCrypto
                        && !sibling_entered
                    {
                        Poll::Pending
                    } else {
                        Poll::Ready(())
                    }
                })
                .await;
                self.engine.drain(scope).await?;
                if self.worker.0 == 1 && self.failure == Failure::DrainCrypto {
                    Err(Error::Io)
                } else {
                    Ok(())
                }
            })
        }
        fn shutdown<'a>(&'a mut self, scope: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.observed.event(self.worker, "crypto-shutdown");
                self.observed.shutdown.fetch_add(1, Ordering::SeqCst);
                std::future::poll_fn(|_| {
                    let expected = if self.failure == Failure::BuildCrypto {
                        1
                    } else {
                        2
                    };
                    if self.observed.shutdown.load(Ordering::SeqCst) == expected {
                        Poll::Ready(())
                    } else {
                        Poll::Pending
                    }
                })
                .await;
                self.engine.shutdown(scope).await?;
                if self.worker.0 == 1 && self.failure == Failure::ShutdownCrypto {
                    Err(Error::Io)
                } else {
                    Ok(())
                }
            })
        }
    }

    fn scope() -> RequestScope {
        RequestScope::new(RequestId([7; 16]), Instant::now() + Duration::from_secs(3)).unwrap()
    }

    #[test]
    fn two_io_share_one_actual_crypto_thread_and_drain_outstanding_jobs() {
        let (plan, factory) = fixture(4, Failure::None, true);
        let observed = factory.observed.clone();
        let mut group = WorkerGroup::new(plan);
        let scope = scope();
        group.start(Arc::new(factory), &scope).unwrap();
        assert_eq!(
            group.threads.len(),
            1,
            "one coordinator owns the scoped workers"
        );
        assert_eq!(group.control.lock().total, 3);
        assert_eq!(group.control.lock().ready, 3);
        assert_eq!(observed.submitted.load(Ordering::SeqCst), 8);
        group.drain(&scope).unwrap();
        assert_eq!(group.control.lock().drained, 3);
        group.shutdown(&scope).unwrap();
        group.join().unwrap();
        assert_eq!(group.control.lock().done, 3);
        let events = observed.events.lock().unwrap();
        let thread_for = |worker, name| {
            events
                .iter()
                .find(|(id, event, _)| *id == worker && *event == name)
                .unwrap()
                .2
        };
        let crypto = thread_for(0, "crypto-build");
        assert_eq!(crypto, thread_for(1, "crypto-build"));
        assert_ne!(crypto, thread_for(0, "io-build"));
        assert_ne!(crypto, thread_for(1, "io-build"));
        assert_ne!(thread_for(0, "io-build"), thread_for(1, "io-build"));
        for worker in 0..2 {
            assert_eq!(crypto, thread_for(worker, "crypto-drop"));
            let position = |name| {
                events
                    .iter()
                    .position(|(id, event, _)| *id == worker && *event == name)
                    .unwrap()
            };
            assert!(position("crypto-start") < position("io-build"));
            assert!(position("io-drain") < position("crypto-drain"));
            assert!(position("crypto-drain") < position("crypto-shutdown"));
            assert!(position("crypto-shutdown") < position("crypto-drop"));
        }
        // While both shards remain active, every pass grants each a single page and
        // reverses its starting order on the next pass, even with saturated queues.
        let polls = observed.polls.lock().unwrap();
        assert!(polls.len() >= 8);
        assert_eq!(&polls[..8], &[0, 1, 1, 0, 0, 1, 1, 0]);
    }

    #[test]
    fn shared_crypto_borrowed_run_counts_caller_and_restores_affinity() {
        let (plan, factory) = fixture(3, Failure::None, true);
        let mut group = WorkerGroup::new(plan);
        let before = current_cpus().unwrap();
        let mut scope = scope();
        // Leave startup enough room under concurrent builds: this scenario checks
        // deadline-driven teardown after all eight jobs, not startup latency.
        scope.deadline = Deadline(Instant::now() + Duration::from_secs(1));
        assert_eq!(
            group.run_with_scope(&factory, &scope),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(current_cpus().unwrap(), before);
        assert_eq!(group.control.lock().done, 3);
        assert_eq!(factory.observed.submitted.load(Ordering::SeqCst), 8);
        let (plan, factory) = fixture(3, Failure::None, false);
        assert_eq!(
            WorkerGroup::new(plan).start(Arc::new(factory), &scope),
            Err(Error::InvalidConfiguration)
        );
    }

    #[test]
    fn partial_group_build_start_and_pending_start_failures_teardown_every_built_service() {
        for failure in [
            Failure::BuildCrypto,
            Failure::StartCrypto,
            Failure::PendingStart,
            Failure::BuildIo,
            Failure::StartIo,
        ] {
            for borrowed in [false, true] {
                let (plan, factory) = fixture(if borrowed { 3 } else { 4 }, failure, true);
                let observed = factory.observed.clone();
                let mut group = WorkerGroup::new(plan);
                let mut scope = scope();
                if failure == Failure::PendingStart {
                    scope.deadline = Deadline(Instant::now() + Duration::from_millis(30));
                }
                let expected = match failure {
                    Failure::PendingStart => Error::DeadlineExceeded,
                    Failure::BuildIo | Failure::StartIo => Error::InvalidRequest,
                    _ => Error::Unauthorized,
                };
                let result = if borrowed {
                    group.run_with_scope(&factory, &scope)
                } else {
                    group.start(Arc::new(factory), &scope)
                };
                assert_eq!(result, Err(expected));
                assert_eq!(group.control.lock().done, 3);
                let events = observed.events.lock().unwrap();
                for id in 0..if failure == Failure::BuildCrypto {
                    1
                } else {
                    2
                } {
                    assert!(
                        events
                            .iter()
                            .any(|(worker, event, _)| *worker == id && *event == "crypto-shutdown")
                    );
                    assert!(
                        events
                            .iter()
                            .any(|(worker, event, _)| *worker == id && *event == "crypto-drop")
                    );
                }
            }
        }
    }

    #[test]
    fn shared_crypto_errors_do_not_skip_sibling_drain_or_shutdown() {
        for failure in [
            Failure::PollCrypto,
            Failure::DrainCrypto,
            Failure::ShutdownCrypto,
        ] {
            let (plan, factory) = fixture(4, failure, false);
            let observed = factory.observed.clone();
            let mut group = WorkerGroup::new(plan);
            let scope = scope();
            let started = group.start(Arc::new(factory), &scope);
            if failure == Failure::PollCrypto {
                // The poll error may race the parent's readiness observation.
                assert!(started == Ok(()) || started == Err(Error::Io));
            } else {
                started.unwrap();
            }
            let _ = group.drain(&scope);
            let _ = group.shutdown(&scope);
            assert_eq!(group.join(), Err(Error::Io));
            assert_eq!(group.control.lock().done, 3);
            assert_eq!(observed.shutdown.load(Ordering::SeqCst), 2);
        }
    }

    #[test]
    fn shared_services_deliver_independent_real_encrypt_decrypt_results() {
        let (plan, mut factory) = fixture(4, Failure::None, false);
        factory.roundtrip = true;
        let observed = factory.observed.clone();
        observed.release.store(true, Ordering::SeqCst);
        let mut group = WorkerGroup::new(plan);
        let scope = scope();
        group.start(Arc::new(factory), &scope).unwrap();
        assert_eq!(observed.completed.load(Ordering::SeqCst), 2);
        group.drain(&scope).unwrap();
        group.shutdown(&scope).unwrap();
        group.join().unwrap();
    }

    #[test]
    fn shared_group_cancellation_during_live_jobs_fences_before_join() {
        let (plan, factory) = fixture(4, Failure::None, true);
        let mut group = WorkerGroup::new(plan);
        let scope = scope();
        group.start(Arc::new(factory), &scope).unwrap();
        scope.cancel().unwrap();
        // A canceled drain wait may return immediately, but join must still run the
        // real crypto engines and fence every abandoned accepted job.
        let drained = group.drain(&scope);
        assert!(drained == Err(Error::Cancelled) || drained == Ok(()));
        group.join().unwrap();
        assert_eq!(group.control.lock().done, 3);
    }

    #[test]
    fn pending_shard_start_does_not_block_ready_sibling_crypto_work() {
        let (plan, mut factory) = fixture(4, Failure::PendingStart, false);
        factory.roundtrip = true;
        let observed = factory.observed.clone();
        observed.release.store(true, Ordering::SeqCst);
        let mut group = WorkerGroup::new(plan);
        let mut scope = scope();
        scope.deadline = Deadline(Instant::now() + Duration::from_millis(100));
        assert_eq!(
            group.start(Arc::new(factory), &scope),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(observed.completed.load(Ordering::SeqCst), 1);
        assert_eq!(group.control.lock().done, 3);
    }

    #[test]
    fn later_crypto_group_allocation_failure_rolls_back_live_first_group() {
        let allowed = current_cpus().unwrap();
        let Some(&second_cpu) = allowed.iter().nth(1) else {
            return;
        };
        for borrowed in [false, true] {
            // Only the first group is constructed, so its teardown must not wait for
            // the deliberately absent second service in this test fixture.
            let (mut plan, mut factory) =
                fixture(if borrowed { 4 } else { 5 }, Failure::BuildCrypto, false);
            plan.pairs[1].crypto.cpu = second_cpu;
            factory.second_crypto_cpu = second_cpu;
            let observed = factory.observed.clone();
            let mut group = WorkerGroup::new(plan);
            let scope = scope();
            let allocator_observed = observed.clone();
            let allocator_scope = scope.clone();
            let allocate = move |worker, generation, capacity| {
                if worker == WorkerId(0) {
                    return crypto::try_pair(worker, generation, capacity);
                }
                let event = "crypto-start";
                while !allocator_observed
                    .events
                    .lock()
                    .unwrap()
                    .iter()
                    .any(|(_, name, _)| *name == event)
                {
                    allocator_scope.check()?;
                    thread::sleep(IDLE_WAIT);
                }
                Err(Error::Overloaded)
            };
            let result = if borrowed {
                group.run_with_allocator(&factory, &scope, false, allocate)
            } else {
                group.start_with_allocator(Arc::new(factory), &scope, allocate)
            };
            assert_eq!(result, Err(Error::Overloaded));
            assert_eq!(group.control.lock().done, 2);
            assert_eq!(current_cpus().unwrap(), allowed);
            let events = observed.events.lock().unwrap();
            assert!(
                events
                    .iter()
                    .any(|(_, event, _)| *event == "crypto-shutdown")
            );
            // Both entry points allocate all groups before starting the caller's I/O.
        }
    }

    #[test]
    fn distinct_crypto_cpus_create_distinct_execution_threads() {
        let allowed = current_cpus().unwrap();
        let Some(&second_cpu) = allowed.iter().nth(1) else {
            return;
        };
        let (mut plan, mut factory) = fixture(5, Failure::None, false);
        plan.pairs[1].crypto.cpu = second_cpu;
        factory.second_crypto_cpu = second_cpu;
        let observed = factory.observed.clone();
        let mut group = WorkerGroup::new(plan);
        let scope = scope();
        group.start(Arc::new(factory), &scope).unwrap();
        assert_eq!(group.threads.len(), 1);
        assert_eq!(group.control.lock().ready, 4);
        group.drain(&scope).unwrap();
        group.shutdown(&scope).unwrap();
        group.join().unwrap();
        let events = observed.events.lock().unwrap();
        let crypto = events
            .iter()
            .filter(|(_, name, _)| *name == "crypto-build")
            .map(|(_, _, id)| *id)
            .collect::<HashSet<_>>();
        assert_eq!(crypto.len(), 2);
    }

    #[test]
    fn shared_native_wrappers_are_drained_and_destroyed_on_owner_thread() {
        let (plan, mut factory) = fixture(4, Failure::None, true);
        factory.native = true;
        let observed = factory.observed.clone();
        let mut group = WorkerGroup::new(plan);
        let scope = scope();
        group.start(Arc::new(factory), &scope).unwrap();
        assert_eq!(observed.native.lock().unwrap().len(), 2);
        group.drain(&scope).unwrap();
        group.shutdown(&scope).unwrap();
        group.join().unwrap();
        for io in observed.native.lock().unwrap().iter() {
            assert_eq!(
                io.reopen(),
                Err(Error::Unavailable),
                "native owner was destroyed"
            );
        }
        assert_eq!(observed.shutdown.load(Ordering::SeqCst), 2);
        assert_eq!(group.control.lock().done, 3);
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn factory_and_crypto_runtime_have_cross_thread_bounds() {
        fn sync<T: Sync + ?Sized>() {}
        fn send<T: Send + 'static>() {}
        sync::<dyn WorkerFactory>();
        send::<CryptoRuntime>();
    }

    #[test]
    fn stable_assignment_ignores_etag_and_worker_input_order() {
        use crate::model::{CacheId, CacheKey, ObjectVersion, PageNumber, StrongEtag};
        let map = WorkerMap::new(vec![WorkerId(9), WorkerId(3), WorkerId(1)]).unwrap();
        let ordered = WorkerMap::new(vec![WorkerId(1), WorkerId(3), WorkerId(9)]).unwrap();
        let object = ObjectId {
            cache: CacheId("cache".into()),
            key: CacheKey([7; 32]),
        };
        let mut page = PageId {
            version: ObjectVersion {
                object: object.clone(),
                etag: StrongEtag::test_value("a"),
            },
            number: PageNumber(0),
        };
        assert_eq!(map.owner(&page), map.metadata_owner(&object));
        for number in 0..100 {
            page.number = PageNumber(number);
            let owner = map.owner(&page);
            assert_eq!(owner, ordered.owner(&page));
            page.version.etag = StrongEtag::test_value("different");
            assert_eq!(owner, map.owner(&page));
        }
        assert!(WorkerMap::new(vec![]).is_err());
        assert!(WorkerMap::new(vec![WorkerId(1), WorkerId(1)]).is_err());
        // SHA-256 encoding vector, independent of std's randomized hasher.
        assert_eq!(map.metadata_owner(&object).unwrap(), WorkerId(1));
    }

    #[derive(Clone)]
    struct TestFactory {
        events: Arc<Mutex<Vec<&'static str>>>,
        crypto_polls: Arc<std::sync::atomic::AtomicUsize>,
        fail_io: bool,
        fail_crypto: bool,
        stop_on_poll: bool,
        cpu: usize,
    }
    struct TestIo {
        factory: TestFactory,
        local: Rc<()>,
        driver: Option<Waker>,
    }
    struct TestCrypto {
        factory: TestFactory,
        _port: CryptoPort,
    }
    impl TestFactory {
        fn event(&self, event: &'static str) {
            self.events.lock().unwrap().push(event);
        }
    }
    impl WorkerFactory for TestFactory {
        fn build(&self, _: WorkerId, _: WorkerRuntime) -> Result<Box<dyn WorkerService>> {
            assert_eq!(
                current_cpus().unwrap(),
                std::collections::BTreeSet::from([self.cpu])
            );
            self.event("io-build");
            if self.fail_io {
                return Err(Error::InvalidRequest);
            }
            Ok(Box::new(TestIo {
                factory: self.clone(),
                local: Rc::new(()),
                driver: None,
            }))
        }
        fn build_crypto(
            &self,
            _: WorkerId,
            runtime: CryptoRuntime,
        ) -> Result<Box<dyn CryptoService>> {
            assert_eq!(
                current_cpus().unwrap(),
                std::collections::BTreeSet::from([self.cpu])
            );
            self.event("crypto-build");
            if self.fail_crypto {
                return Err(Error::Unauthorized);
            }
            Ok(Box::new(TestCrypto {
                factory: self.clone(),
                _port: runtime.port,
            }))
        }
    }
    impl WorkerService for TestIo {
        fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(std::future::poll_fn(move |cx| {
                self.driver = Some(cx.waker().clone());
                self.factory.event("io-start");
                Poll::Ready(Ok(()))
            }))
        }
        fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<()> {
            assert!(
                self.driver.as_ref().unwrap().will_wake(cx.waker()),
                "steady-state polling must retain the lifecycle driver waker"
            );
            assert!(budget <= WORK_BUDGET);
            assert_eq!(Rc::strong_count(&self.local), 1);
            if self.factory.stop_on_poll {
                Err(Error::Cancelled)
            } else {
                Ok(())
            }
        }
        fn stop_admission(&mut self) -> Result<()> {
            self.factory.event("io-stop");
            Ok(())
        }
        fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            // The engine must still be running while I/O is draining on one CPU.
            let before = self
                .factory
                .crypto_polls
                .load(std::sync::atomic::Ordering::SeqCst);
            Box::pin(std::future::poll_fn(move |cx| {
                if self
                    .factory
                    .crypto_polls
                    .load(std::sync::atomic::Ordering::SeqCst)
                    == before
                {
                    cx.waker().wake_by_ref();
                    return Poll::Pending;
                }
                self.factory.event("io-drain");
                Poll::Ready(Ok(()))
            }))
        }
        fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.factory.event("io-shutdown");
                Ok(())
            })
        }
    }
    impl CryptoService for TestCrypto {
        fn register_driver(&self, waker: &Waker) {
            self._port.register_driver(waker);
        }
        fn start<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.factory.event("crypto-start");
                Ok(())
            })
        }
        fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
            assert!(budget <= WORK_BUDGET);
            self.factory
                .crypto_polls
                .fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            Ok(())
        }
        fn drain<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.factory.event("crypto-drain");
                Ok(())
            })
        }
        fn shutdown<'a>(&'a mut self, _: &'a RequestScope) -> Operation<'a, ()> {
            Box::pin(async move {
                self.factory.event("crypto-shutdown");
                Ok(())
            })
        }
    }
    fn fixture(max_threads: usize) -> (WorkerGroup<'static>, TestFactory) {
        let plan = colocated_plan(max_threads, 1);
        let cpu = plan.pairs[0].io.cpu;
        (
            WorkerGroup::new(plan),
            TestFactory {
                events: Arc::new(Mutex::new(Vec::new())),
                crypto_polls: Arc::new(std::sync::atomic::AtomicUsize::new(0)),
                fail_io: false,
                fail_crypto: false,
                stop_on_poll: false,
                cpu,
            },
        )
    }

    #[test]
    fn prepare_rejects_known_numa_mismatch() {
        let (mut group, _) = fixture(3);
        group.plan.pairs[0].io.numa_node = Some(0);
        group.plan.pairs[0].crypto.numa_node = Some(1);
        assert_eq!(group.prepare(false), Err(Error::InvalidConfiguration));
        assert_eq!(group.generation, 0);
    }

    #[test]
    fn prepare_rejects_contradictory_shared_crypto_metadata() {
        for dimension in 0..3 {
            let (mut group, _) = fixture(4);
            let mut second = group.plan.pairs[0].clone();
            second.worker = WorkerId(1);
            match dimension {
                0 => second.crypto.package += 1,
                1 => second.crypto.core += 1,
                _ => second.crypto.numa_node = Some(1),
            }
            group.plan.pairs.push(second);
            assert_eq!(group.prepare(false), Err(Error::InvalidConfiguration));
            assert_eq!(group.generation, 0);
        }
    }

    #[test]
    fn prepare_accepts_colocated_io_and_unknown_numa() {
        for (io, crypto) in [
            (None, None),
            (Some(0), None),
            (None, Some(0)),
            (Some(0), Some(0)),
        ] {
            let (mut group, _) = fixture(4);
            group.plan.pairs[0].io.numa_node = io;
            group.plan.pairs[0].crypto.numa_node = crypto;
            let mut second = group.plan.pairs[0].clone();
            second.worker = WorkerId(1);
            group.plan.pairs.push(second);
            group.prepare(false).unwrap();
            assert_eq!(group.control.lock().total, 3);
        }
    }

    #[test]
    fn owned_start_drains_and_joins_both_pinned_local_services() {
        let (mut group, factory) = fixture(3);
        let events = factory.events.clone();
        let scope = lifecycle_scope().unwrap();
        group.start(Arc::new(factory), &scope).unwrap();
        group.drain(&scope).unwrap();
        group.shutdown(&scope).unwrap();
        group.join().unwrap();
        let events = events.lock().unwrap();
        let position = |event| events.iter().position(|found| *found == event).unwrap();
        assert!(position("crypto-start") < position("io-build"));
        assert!(position("io-stop") < position("io-drain"));
        assert!(position("io-drain") < position("crypto-drain"));
        assert!(position("crypto-drain") < position("crypto-shutdown"));
        assert!(events.contains(&"io-shutdown"));
        assert_eq!(group.control.lock().done, 2);
        // Completed workers cannot retain cancellation slots in the caller's scope.
        let registrations = (0..1024)
            .map(|_| scope.cancellation.subscribe().unwrap())
            .collect::<Vec<_>>();
        assert!(matches!(
            scope.cancellation.subscribe(),
            Err(Error::Overloaded)
        ));
        drop(registrations);
    }

    #[test]
    fn cancellation_subscription_failure_tears_down_built_services() {
        let (mut group, factory) = fixture(3);
        let events = factory.events.clone();
        let scope = lifecycle_scope().unwrap();
        let _registrations = (0..1024)
            .map(|_| scope.cancellation.subscribe().unwrap())
            .collect::<Vec<_>>();
        assert_eq!(
            group.start(Arc::new(factory), &scope),
            Err(Error::Overloaded)
        );
        assert!(group.threads.is_empty());
        assert_eq!(group.control.lock().done, 2);
        assert!(events.lock().unwrap().contains(&"crypto-shutdown"));
    }

    #[test]
    fn startup_failure_rolls_back_and_owned_start_counts_caller() {
        let scope = lifecycle_scope().unwrap();
        let (mut group, factory) = fixture(2);
        assert_eq!(
            group.start(Arc::new(factory), &scope),
            Err(Error::InvalidConfiguration)
        );
        for fail_crypto in [false, true] {
            let (mut group, mut factory) = fixture(3);
            factory.fail_crypto = fail_crypto;
            factory.fail_io = !fail_crypto;
            let expected = if fail_crypto {
                Error::Unauthorized
            } else {
                Error::InvalidRequest
            };
            assert_eq!(group.start(Arc::new(factory), &scope), Err(expected));
            assert!(group.threads.is_empty());
            assert_eq!(group.control.lock().done, 2);
        }
    }

    #[test]
    fn borrowed_run_uses_caller_and_restores_affinity_on_failure() {
        let (group, mut factory) = fixture(2);
        // Reconstruct to infer the borrowed factory lifetime instead of 'static.
        let plan = AffinityPlan {
            pairs: group.plan.pairs.clone(),
            max_threads: 2,
        };
        factory.stop_on_poll = true;
        let before = current_cpus().unwrap();
        let mut group = WorkerGroup::new(plan);
        assert_eq!(group.run(&factory), Err(Error::Cancelled));
        assert_eq!(current_cpus().unwrap(), before);
        assert_eq!(group.control.lock().done, 2);
        assert!(factory.events.lock().unwrap().contains(&"crypto-shutdown"));
    }

    #[test]
    fn borrowed_run_scope_stops_and_joins_on_deadline() {
        let (group, factory) = fixture(2);
        let mut scoped = WorkerGroup::new(AffinityPlan {
            pairs: group.plan.pairs.clone(),
            max_threads: 2,
        });
        let mut scope = lifecycle_scope().unwrap();
        scope.deadline = Deadline(Instant::now() + Duration::from_millis(50));
        assert_eq!(
            scoped.run_with_scope(&factory, &scope),
            Err(Error::DeadlineExceeded)
        );
        assert_eq!(scoped.control.lock().done, 2);
    }

    #[test]
    fn engine_driver_wake_survives_noop_operation_poll() {
        struct Count(std::sync::atomic::AtomicUsize);
        impl Wake for Count {
            fn wake(self: Arc<Self>) {
                self.0.fetch_add(1, std::sync::atomic::Ordering::SeqCst);
            }
        }
        let (_, factory) = fixture(3);
        let (io, port) = crypto::pair(WorkerId(0), 1, NonZeroUsize::new(1).unwrap());
        let mut engine = TestCrypto {
            factory,
            _port: port,
        };
        let count = Arc::new(Count(std::sync::atomic::AtomicUsize::new(0)));
        engine.register_driver(&Waker::from(count.clone()));
        assert!(
            engine
                ._port
                .poll_job(&mut Context::from_waker(futures::task::noop_waker_ref()))
                .is_pending()
        );
        io.close_submissions().unwrap();
        assert_eq!(count.0.load(std::sync::atomic::Ordering::SeqCst), 1);
    }

    #[test]
    fn coordinator_panic_reports_failure_and_joins_without_waiting_for_startup_deadline() {
        let (mut group, factory) = fixture(3);
        let scope = lifecycle_scope().unwrap();
        assert_eq!(
            group.start_with_allocator(Arc::new(factory), &scope, |_, _, _| {
                panic!("injected allocation panic")
            }),
            Err(Error::Io)
        );
        assert!(group.threads.is_empty());
    }

    #[test]
    fn later_pair_allocation_failure_stops_and_joins_started_pair() {
        let (mut group, factory) = fixture(5);
        let events = factory.events.clone();
        let mut second = group.plan.pairs[0].clone();
        second.worker = WorkerId(1);
        group.plan.pairs.push(second);
        let scope = lifecycle_scope().unwrap();
        let result = group.start_with_allocator(
            Arc::new(factory),
            &scope,
            |worker, generation, capacity| {
                if worker == WorkerId(0) {
                    return crypto::try_pair(worker, generation, capacity);
                }
                // Group endpoints are allocated before its owning thread starts.
                Err(Error::Overloaded)
            },
        );
        assert_eq!(result, Err(Error::Overloaded));
        assert!(group.threads.is_empty());
        assert_eq!(group.control.lock().done, 2);
        let events = events.lock().unwrap();
        assert!(!events.contains(&"io-build"));
        assert!(events.contains(&"crypto-shutdown"));
    }

    #[test]
    fn scoped_allocation_failure_joins_started_crypto_and_restores_affinity() {
        let (group, factory) = fixture(4);
        let mut plan = AffinityPlan {
            pairs: group.plan.pairs.clone(),
            max_threads: 4,
        };
        let mut second = plan.pairs[0].clone();
        second.worker = WorkerId(1);
        plan.pairs.push(second);
        let mut scoped = WorkerGroup::new(plan);
        let scope = lifecycle_scope().unwrap();
        let before = current_cpus().unwrap();
        let result =
            scoped.run_with_allocator(&factory, &scope, false, |worker, generation, capacity| {
                if worker == WorkerId(0) {
                    return crypto::try_pair(worker, generation, capacity);
                }
                Err(Error::Overloaded)
            });
        assert_eq!(result, Err(Error::Overloaded));
        assert_eq!(current_cpus().unwrap(), before);
        assert_eq!(scoped.control.lock().done, 2);
        let events = factory.events.lock().unwrap();
        assert!(!events.contains(&"io-build"));
        assert!(events.contains(&"crypto-shutdown"));
    }
}
