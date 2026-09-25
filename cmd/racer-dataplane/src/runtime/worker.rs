//! Stable local page-to-worker dispatch, independent of cluster placement.
//!
//! Construct worker-local service graphs on their selected threads. Rc-owned graphs
//! must never cross threads; only bounded commands and completion-safe leases do.
//! Drain flights before changing the worker map; no live remapping is implied.
//!
//! Each worker is an I/O/crypto thread pair. The I/O thread owns the service graph,
//! flights, storage shard, and admission. Page AEAD runs on the paired crypto thread
//! through bounded owned job/completion messages, retaining buffers and key leases
//! until completion even after cancellation. Queue wakeups and completion capacity
//! must permit progress when both threads share one CPU.

use super::{
    admission::Admission,
    affinity::{current_cpus, pin_cpu, set_cpus, AffinityPlan, WorkerPair},
    crypto::{self, CryptoClient, CryptoPort, IoCryptoPort},
    deadline::{Cancellation, Deadline, RequestScope},
    reactor::{Reactor, ReactorWake},
};
use crate::{
    error::{Error, Operation, Result},
    model::{
        identity::{ObjectId, PageId, RequestId, WorkerId},
        limits::Limits,
    },
};
use sha2::{Digest, Sha256};
use std::{
    collections::HashSet,
    marker::PhantomData,
    num::NonZeroUsize,
    rc::Rc,
    sync::{Arc, Condvar, Mutex, MutexGuard},
    task::{Context, Poll, Wake, Waker},
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};

const WORK_BUDGET: usize = 64;
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
/// use racer_dataplane::{error::Result, model::identity::WorkerId,
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
            replay_entries: n(1024),
            header_bytes: n(32768),
            route_search_work: n(4096),
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
    fn poll_budgeted(&mut self, work_budget: usize) -> Result<()>;
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
            control: Arc::new(Control::new(0, false)),
            threads: Vec::new(),
            generation: 0,
            borrowed: PhantomData,
        }
    }
    /// Independent startup requires an owned recipe: borrowed factories are only
    /// supported by `run`/`run_with_scope`, whose scoped threads cannot escape.
    /// The caller remains a userspace thread, so complete pairs must fit in
    /// max_threads - 1. An eight-thread plan with four pairs must use `run`.
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
        mut allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> Result<()> {
        self.prepare(false)?;
        let limits = factory.limits();
        for (index, pair) in self.plan.pairs.iter().cloned().enumerate() {
            let (io, engine) = match allocate(pair.worker, self.generation, limits.queue_entries) {
                Ok(ports) => ports,
                Err(error) => {
                    self.control.fail(error);
                    break;
                }
            };
            let control = self.control.clone();
            let recipe = factory.clone();
            let startup = scope.clone();
            let placement = pair.clone();
            match thread::Builder::new()
                .name(format!("racer-crypto-{}", pair.worker.0))
                .spawn(move || crypto_thread(&*recipe, placement, engine, startup, control, index))
            {
                Ok(handle) => self.threads.push(handle),
                Err(_) => {
                    self.control.fail(Error::Io);
                    break;
                }
            }
            let control = self.control.clone();
            let recipe = factory.clone();
            let startup = scope.clone();
            let limits = limits.clone();
            match thread::Builder::new()
                .name(format!("racer-io-{}", pair.worker.0))
                .spawn(move || io_thread(&*recipe, pair, io, limits, startup, control, index))
            {
                Ok(handle) => self.threads.push(handle),
                Err(_) => {
                    self.control.close_io(index);
                    self.control.fence_io(index);
                    self.control.fail(Error::Io);
                    break;
                }
            }
        }
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
    /// Join both OS threads of every pair, including partially started pairs.
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

    /// A handle obtained after startup can stop an owned group. For a blocking
    /// run, use a cloned cancellation from run_with_scope to request shutdown.
    pub fn request_stop(&self) {
        self.control.set_phase(Phase::Drain);
    }

    fn prepare(&mut self, caller_is_worker: bool) -> Result<()> {
        if !self.threads.is_empty() || self.plan.pairs.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        let threads = self
            .plan
            .pairs
            .len()
            .checked_mul(2)
            .and_then(|n| n.checked_add(usize::from(!caller_is_worker)))
            .ok_or(Error::InvalidConfiguration)?;
        if threads > self.plan.max_threads {
            return Err(Error::InvalidConfiguration);
        }
        let allowed = current_cpus()?;
        let mut workers = HashSet::new();
        for pair in &self.plan.pairs {
            if !workers.insert(pair.worker)
                || !allowed.contains(&pair.io.cpu)
                || !allowed.contains(&pair.crypto.cpu)
            {
                return Err(Error::InvalidConfiguration);
            }
        }
        self.generation = self
            .generation
            .checked_add(1)
            .ok_or(Error::InvalidConfiguration)?;
        self.control = Arc::new(Control::new(self.plan.pairs.len() * 2, caller_is_worker));
        Ok(())
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
        mut allocate: impl FnMut(WorkerId, u64, NonZeroUsize) -> Result<(IoCryptoPort, CryptoPort)>,
    ) -> Result<()> {
        self.prepare(true)?;
        let original_affinity = current_cpus()?;
        let limits = factory.limits();
        self.control.lock().check_scope = check_scope;
        thread::scope(|threads| {
            let mut handles = Vec::new();
            let mut first_io = None;
            for (index, pair) in self.plan.pairs.iter().cloned().enumerate() {
                let (io, engine) =
                    match allocate(pair.worker, self.generation, limits.queue_entries) {
                        Ok(ports) => ports,
                        Err(error) => {
                            self.control.fail(error);
                            break;
                        }
                    };
                let control = self.control.clone();
                let startup = scope.clone();
                let placement = pair.clone();
                match thread::Builder::new()
                    .name(format!("racer-crypto-{}", pair.worker.0))
                    .spawn_scoped(threads, move || {
                        crypto_thread(factory, placement, engine, startup, control, index)
                    }) {
                    Ok(handle) => handles.push(handle),
                    Err(_) => {
                        self.control.fail(Error::Io);
                        break;
                    }
                }
                if index == 0 {
                    first_io = Some((pair, io));
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
                    Ok(handle) => handles.push(handle),
                    Err(_) => {
                        self.control.close_io(index);
                        self.control.fence_io(index);
                        self.control.fail(Error::Io);
                        break;
                    }
                }
            }
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
    total: usize,
    ready: usize,
    drained: usize,
    done: usize,
    error: Option<Error>,
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
    fn new(total: usize, auto_shutdown: bool) -> Self {
        Self {
            state: Mutex::new(State {
                phase: Phase::Running,
                total,
                ready: 0,
                drained: 0,
                done: 0,
                error: None,
                crypto_ready: vec![false; total / 2],
                io_closed: vec![false; total / 2],
                io_fenced: vec![false; total / 2],
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
    if startup {
        scope.cancellation.register(waker)?;
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
        request: RequestId([0; 16]),
        deadline: Deadline(Instant::now() + Duration::from_secs(30)),
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
        if check_scope {
            record(&control, startup.cancellation.register(&waker));
        }
        while !control.stopping(&startup) {
            runtime.crypto.register_driver(&waker);
            if let Err(error) =
                poll_runtime(&runtime).and_then(|()| service.poll_budgeted(WORK_BUDGET))
            {
                control.fail(error);
                break;
            }
            // A short bounded wait prevents monopolizing a shared single CPU.
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
    pair: WorkerPair,
    port: CryptoPort,
    startup: RequestScope,
    control: Arc<Control>,
    index: usize,
) {
    let _exit = ThreadExit {
        control: control.clone(),
        io: None,
    };
    if let Err(error) = pin_cpu(pair.crypto.cpu) {
        control.fail(error);
        return;
    }
    let waker = Waker::from(Arc::new(ThreadWake(thread::current(), None)));
    port.register_driver(&waker);
    let mut service = match factory.build_crypto(pair.worker, CryptoRuntime { port }) {
        Ok(service) => service,
        Err(error) => {
            control.fail(error);
            return;
        }
    };
    service.register_driver(&waker);
    let started = drive(
        service.start(&startup),
        None,
        &startup,
        &control,
        true,
        &waker,
    );
    if started.is_ok() {
        let mut state = control.lock();
        state.crypto_ready[index] = true;
        state.ready += 1;
        control.changed.notify_all();
    } else {
        record(&control, started);
    }
    // Keep driving accepted jobs throughout I/O drain, even after an error.
    while !control.lock().io_closed[index] {
        service.register_driver(&waker);
        record(&control, service.poll_budgeted(WORK_BUDGET));
        thread::park_timeout(IDLE_WAIT);
    }
    let teardown = lifecycle_scope().unwrap_or_else(|_| startup.clone());
    service.register_driver(&waker);
    record(
        &control,
        drive(
            service.drain(&teardown),
            None,
            &teardown,
            &control,
            false,
            &waker,
        ),
    );
    // Completion publication is not consumption. Keep the engine alive until
    // I/O has reaped every accepted job's completion and released its permit.
    while !control.lock().io_fenced[index] {
        service.register_driver(&waker);
        record(&control, service.poll_budgeted(WORK_BUDGET));
        thread::park_timeout(IDLE_WAIT);
    }
    control.drained();
    service.register_driver(&waker);
    record(
        &control,
        drive(
            service.shutdown(&teardown),
            None,
            &teardown,
            &control,
            false,
            &waker,
        ),
    );
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
        use crate::model::identity::{CacheId, CacheKey, ObjectVersion, PageNumber, StrongEtag};
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
    }
    struct TestCrypto {
        factory: TestFactory,
        _port: CryptoPort,
    }
    impl Clone for TestFactory {
        fn clone(&self) -> Self {
            Self {
                events: self.events.clone(),
                crypto_polls: self.crypto_polls.clone(),
                fail_io: self.fail_io,
                fail_crypto: self.fail_crypto,
                stop_on_poll: self.stop_on_poll,
                cpu: self.cpu,
            }
        }
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
            Box::pin(async move {
                self.factory.event("io-start");
                Ok(())
            })
        }
        fn poll_budgeted(&mut self, budget: usize) -> Result<()> {
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
        let cpu = *current_cpus().unwrap().first().unwrap();
        let location = super::super::affinity::CpuLocation {
            cpu,
            package: 0,
            core: 0,
            numa_node: None,
        };
        (
            WorkerGroup::new(AffinityPlan {
                pairs: vec![WorkerPair {
                    worker: WorkerId(0),
                    io: location.clone(),
                    crypto: location,
                    nic: None,
                }],
                max_threads,
            }),
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
        assert!(engine
            ._port
            .poll_job(&mut Context::from_waker(futures::task::noop_waker_ref()))
            .is_pending());
        io.close_submissions().unwrap();
        assert_eq!(count.0.load(std::sync::atomic::Ordering::SeqCst), 1);
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
                // Ensure rollback covers a live local graph, not merely spawned threads.
                while !events.lock().unwrap().contains(&"io-start") {
                    scope.check()?;
                    thread::sleep(IDLE_WAIT);
                }
                Err(Error::Overloaded)
            },
        );
        assert_eq!(result, Err(Error::Overloaded));
        assert!(group.threads.is_empty());
        assert_eq!(group.control.lock().done, 2);
        let events = events.lock().unwrap();
        assert!(events.contains(&"io-stop"));
        assert!(events.contains(&"io-drain"));
        assert!(events.contains(&"io-shutdown"));
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
                while !factory.events.lock().unwrap().contains(&"crypto-start") {
                    scope.check()?;
                    thread::sleep(IDLE_WAIT);
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
