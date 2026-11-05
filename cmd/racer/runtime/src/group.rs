//! Pinned local services with explicit startup, drain, and ownership fences.
//!
//! Fail closed: an unsuccessful ownership fence or an unexpected unwind of a
//! live service aborts the process. Neither an error nor thread exit proves that
//! external I/O stopped referencing storage. We never detach or drop such owners.
use crate::{Error, Operation, Result, Scope};
use std::{
    cell::Cell,
    rc::Rc,
    sync::{
        Arc, Condvar, Mutex, MutexGuard,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    thread::{self, JoinHandle},
    time::Duration,
};

/// Poll interval of lifecycle phases (startup, drain, fences, barriers).
pub const IDLE_WAIT: Duration = Duration::from_millis(1);

/// Longest steady-state park. Lanes and helpers park until a wake source
/// fires (completion, cross-thread wake, phase change, stop signal) or a
/// service-reported deadline; this only bounds the arithmetic.
const MAX_PARK: Duration = Duration::from_secs(3600);

/// Work steps one service turn may run before the lane parks or yields.
pub const WORK_BUDGET: usize = 64;

/// One locally driven service and the CPU on which it is constructed.
#[derive(Clone)]
pub struct Lane {
    /// OS thread name, without embedded NUL bytes.
    pub name: String,

    /// Allowed logical CPU ID.
    pub cpu: usize,
}

/// A pinned thread that cooperatively drives helper services for several lanes.
#[derive(Clone)]
pub struct Helper {
    /// OS thread name, without embedded NUL bytes.
    pub name: String,

    /// Allowed logical CPU ID.
    pub cpu: usize,

    /// Lane indices in `Plan::lanes`. Each lane has at most one helper service.
    pub lanes: Vec<usize>,
}

/// Caller-selected placement and whole-process thread limit.
#[derive(Clone)]
pub struct Plan {
    /// Services to run, with lane zero acting as coordinator.
    pub lanes: Vec<Lane>,

    /// Optional helper threads and their lane assignments.
    pub helpers: Vec<Helper>,

    /// Whole-process execution budget including the external caller of `start`.
    /// `run` instead uses its caller as lane zero and consumes no extra slot.
    pub max_threads: usize,
}

/// Builds run on the pinned owner. Local services and futures need not be Send.
/// A helper builds one service per associated lane and drives them cooperatively.
pub trait Factory<S: Scope>: Sync {
    /// Construct a lane service on its pinned owner thread.
    fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<S>>, S::Error>;

    /// Construct a helper shard on the lane's assigned helper thread.
    fn build_helper(&self, _lane: usize) -> Result<Box<dyn Service<S>>, S::Error> {
        Err(Error::InvalidConfiguration.into())
    }

    /// Close preallocated endpoints for a lane that never reaches construction.
    /// No accepted work can exist for that lane yet.
    fn abandon_lane(&self, _lane: usize) -> Result<(), S::Error> {
        Ok(())
    }

    /// Called when teardown begins, not at startup. Override to supply a fresh
    /// lifecycle deadline; this scope never authorizes skipping ownership fences.
    fn teardown_scope(&self, startup: &S) -> S {
        startup.clone()
    }
}

/// Lifecycle futures drive their own local resources. `fence` must not finish
/// until accepted work no longer references resources, even after cancellation.
pub trait Service<S: Scope> {
    /// Retain this capability when a lifecycle future drives resources that can
    /// fail while it remains Pending. Reporting wakes group waiters and stops
    /// sibling admission without canceling teardown futures or ownership fences.
    fn set_failure_reporter(&mut self, _reporter: FailureReporter<S::Error>) {}

    /// Return the wake handle used to drive this service's lifecycle.
    fn waker(&self) -> Result<Waker, S::Error> {
        Ok(crate::drivers::thread_waker(None))
    }

    /// Arrange notification when locally owned work can make progress.
    fn register_driver(&self, _waker: &Waker) {}

    /// Initialize the service before it is announced ready.
    fn start<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error>;

    /// Perform at most the caller's budget of steady-state work.
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<(), S::Error>;

    /// Bound the next steady-state wait after polling. Due work returns zero.
    /// Work without a wake source must report its next poll time here.
    fn wait_timeout(&self, maximum: Duration) -> Duration {
        maximum
    }

    /// Park the owning thread for at most `timeout`, returning early when the
    /// service's waker fires. Always called after a steady-state turn, even
    /// with a zero timeout, so a reactor can submit work queued by the turn.
    fn park(&mut self, timeout: Duration) -> Result<(), S::Error> {
        if !timeout.is_zero() {
            thread::park_timeout(timeout);
        }
        Ok(())
    }

    /// The lifecycle parker. Lifecycle futures (start, drain, fence, shutdown)
    /// borrow the service, so their driver parks through this independent
    /// handle until a wake source fires or the parker's next deadline.
    fn parker(&self) -> Box<dyn Park<S::Error>> {
        Box::new(ThreadPark)
    }

    /// Reject new work before draining accepted work.
    fn stop_admission(&mut self) -> Result<(), S::Error> {
        Ok(())
    }

    /// Drain accepted work without using cancellation as an ownership fence.
    fn drain<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error>;

    /// Close helper submissions after lane drain has stopped every producer.
    fn close(&mut self) -> Result<(), S::Error> {
        Ok(())
    }

    /// Prove that external work no longer references service-owned resources.
    /// Failure or panic aborts the process instead of destroying live owners.
    fn fence<'a>(&'a mut self, _scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async { Ok(()) })
    }

    /// Shut down after drain; a second ownership fence runs before destruction.
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error>;
}

/// Parks a lifecycle driver between polls of a service's lifecycle future.
pub trait Park<E> {
    /// Bound the next wait by deadlines without another wake source.
    fn wait_timeout(&self, maximum: Duration) -> Duration {
        maximum
    }

    /// Wait at most `timeout`, returning early when the driver's waker fires.
    /// Always called after a pending poll, even with a zero timeout.
    fn park(&mut self, timeout: Duration) -> Result<(), E>;
}

/// The default parker: the driver's waker unparks this thread.
struct ThreadPark;

impl<E> Park<E> for ThreadPark {
    fn park(&mut self, timeout: Duration) -> Result<(), E> {
        if !timeout.is_zero() {
            thread::park_timeout(timeout);
        }
        Ok(())
    }
}

/// Error-only group capability: cannot advance phases or authorize resource release.
#[derive(Clone)]
pub struct FailureReporter<E: Copy> {
    control: Arc<Control<E>>,
}

impl<E: Copy + PartialEq + From<Error>> FailureReporter<E> {
    /// Record the first failure and request that every lane stop admission.
    pub fn report(&self, error: E) {
        self.control.fail(error);
    }
}

/// Read-only lifecycle diagnostics for the current execution, not synchronization
/// authority. Counts describe OS execution threads, not helper service shards.
/// Use the lifecycle methods to wait for completion rather than polling counts.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    /// Number of planned lane and helper threads.
    pub total: usize,

    /// Threads whose services finished startup.
    pub ready: usize,

    /// Threads whose services finished their first ownership fence.
    pub drained: usize,

    /// Threads that exited, including unstarted threads accounted by rollback.
    pub done: usize,

    /// Owned coordinator handles awaiting join (zero or one), even after exit.
    pub coordinators: usize,
}

/// Explicitly driven thread ownership. Dropping a group requests shutdown and
/// joins all threads; it never detaches resources still owned by a service.
pub struct Group<S: Scope + Send> {
    plan: Plan,

    control: Arc<Control<S::Error>>,

    coordinator: Option<JoinHandle<()>>,
}

impl<S: Scope + Send> Group<S> {
    /// Store a placement plan without spawning threads or constructing services.
    pub fn new(plan: Plan) -> Self {
        Self {
            plan,
            control: Arc::new(Control::new(0, 0, false)),
            coordinator: None,
        }
    }

    /// Snapshot lifecycle progress without exposing mutable coordinator state.
    pub fn stats(&self) -> Stats {
        let mut stats = self.control.lock().stats;
        stats.coordinators = usize::from(self.coordinator.is_some());
        stats
    }

    /// Nonmutating preflight for callers allocating resources before startup.
    /// Pass true for `run`/`run_with_scope`, false for `start`. Startup repeats
    /// validation because CPU affinity and owned-thread state may have changed.
    pub fn validate(&self, caller_is_lane: bool) -> Result<(), S::Error> {
        let invalid = || S::Error::from(Error::InvalidConfiguration);
        let count = self
            .plan
            .lanes
            .len()
            .checked_add(self.plan.helpers.len())
            .and_then(|n| n.checked_add(usize::from(!caller_is_lane)))
            .ok_or_else(invalid)?;
        if self.coordinator.is_some() || self.plan.lanes.is_empty() || count > self.plan.max_threads
        {
            return Err(invalid());
        }
        let allowed = affinity::current_cpus()?;
        let mut assigned = vec![false; self.plan.lanes.len()];
        for lane in &self.plan.lanes {
            if !allowed.contains(&lane.cpu) || lane.name.contains('\0') {
                return Err(invalid());
            }
        }
        for helper in &self.plan.helpers {
            if helper.lanes.is_empty()
                || !allowed.contains(&helper.cpu)
                || helper.name.contains('\0')
            {
                return Err(invalid());
            }
            for &lane in &helper.lanes {
                if lane >= assigned.len() || assigned[lane] {
                    return Err(invalid());
                }
                assigned[lane] = true;
            }
        }
        Ok(())
    }

    /// Validate a new execution and reset its barriers before any construction.
    fn prepare(&mut self, caller_is_lane: bool, check_scope: bool) -> Result<(), S::Error> {
        self.validate(caller_is_lane)?;
        self.control = Arc::new(Control::new(
            self.plan.lanes.len(),
            self.plan.helpers.len(),
            caller_is_lane,
        ));
        self.control
            .check_scope
            .store(check_scope, Ordering::Relaxed);
        let mut state = self.control.lock();
        state.check_scope = check_scope;
        state.helper_ready.fill(true);
        for helper in &self.plan.helpers {
            for &lane in &helper.lanes {
                state.helper_ready[lane] = false;
            }
        }
        Ok(())
    }

    /// Spawn the coordinator and wait until every lane and helper is ready.
    /// Startup failure joins the coordinator after its ownership fences finish.
    pub fn start(
        &mut self,
        factory: Arc<dyn Factory<S> + Send>,
        scope: &S,
    ) -> Result<(), S::Error> {
        self.prepare(false, false)?;
        let plan = self.plan.clone();
        let control = self.control.clone();
        let startup = scope.clone();
        let thread_factory = factory.clone();
        let handle = thread::Builder::new()
            .name(plan.lanes[0].name.clone())
            .spawn(move || {
                record(
                    &control,
                    run_prepared(&plan, &*thread_factory, &startup, &control),
                );
            })
            .map_err(|_| S::Error::from(Error::Io));
        let handle = match handle {
            Ok(handle) => handle,
            Err(error) => {
                rollback_prepared(&self.plan, &*factory, &self.control, error);
                return Err(error);
            }
        };
        self.coordinator = Some(handle);
        let result = self
            .control
            .wait_for(scope, |s| s.stats.ready == s.stats.total);
        if let Err(error) = result {
            self.control.fail(error);
            let _ = self.join();
        }
        result
    }

    /// Run on the caller's thread without making the startup scope a run deadline.
    pub fn run(&mut self, factory: &dyn Factory<S>, scope: &S) -> Result<(), S::Error> {
        self.run_inner(factory, scope, false)
    }

    /// Also interpret scope expiry as a steady-state stop request. Teardown and
    /// fences remain uninterruptible even when the caller stops waiting.
    pub fn run_with_scope(&mut self, factory: &dyn Factory<S>, scope: &S) -> Result<(), S::Error> {
        self.run_inner(factory, scope, true)
    }

    /// Execute lane zero on the caller, optionally checking its steady-state scope.
    fn run_inner(
        &mut self,
        factory: &dyn Factory<S>,
        scope: &S,
        check: bool,
    ) -> Result<(), S::Error> {
        self.prepare(true, check)?;
        record(
            &self.control,
            run_prepared(&self.plan, factory, scope, &self.control),
        );
        self.control.result()
    }

    /// Stop admission, drain local work, close submissions, and fence accepted
    /// resources. A failed wait does not truncate the running ownership fences.
    pub fn drain(&mut self, scope: &S) -> Result<(), S::Error> {
        self.control.set_phase(Phase::Drain);
        self.control
            .wait_for(scope, |s| s.stats.drained == s.stats.total)
    }

    /// Request drain followed by shutdown, or release already-drained services.
    /// Scope failure stops only this wait, not the threads or their fences.
    pub fn shutdown(&mut self, scope: &S) -> Result<(), S::Error> {
        self.control.set_phase(Phase::Shutdown);
        self.control
            .wait_for(scope, |s| s.stats.done == s.stats.total)
    }

    /// Request shutdown and join every owned thread, even after partial startup.
    /// A service that cannot complete its ownership fence can prevent return.
    pub fn join(&mut self) -> Result<(), S::Error> {
        self.control.set_phase(Phase::Shutdown);
        if let Some(handle) = self.coordinator.take()
            && handle.join().is_err()
        {
            self.control.fail(Error::Io.into());
        }
        self.control.result()
    }
}

impl<S: Scope + Send> Drop for Group<S> {
    /// Join every owned service thread instead of detaching live resources.
    fn drop(&mut self) {
        let _ = self.join();
    }
}

/// Monotonic lifecycle requests shared by all execution threads.
#[derive(Clone, Copy, PartialEq, Eq, PartialOrd, Ord)]
enum Phase {
    Running,
    Drain,
    Shutdown,
}

/// Barrier progress and the first failure, protected by the coordinator mutex.
struct State<E> {
    phase: Phase,

    stats: Stats,

    error: Option<E>,

    helper_ready: Vec<bool>,

    closed: Vec<bool>,

    fenced: Vec<bool>,

    auto_shutdown: bool,

    check_scope: bool,
}

/// Shared lifecycle coordination, independent of service-owned resources.
struct Control<E> {
    state: Mutex<State<E>>,

    changed: Condvar,

    /// Mirrors `State::phase != Running`, read once per lane turn without
    /// taking the coordinator lock.
    stopping: AtomicBool,

    /// Mirrors `State::check_scope`.
    check_scope: AtomicBool,

    /// Set once any lane closed its helper submissions.
    closing: AtomicBool,

    /// Wakes of every parked lane and helper, fired on each phase change.
    wakers: Mutex<Vec<Waker>>,
}

impl<E: Copy + PartialEq + From<Error>> Control<E> {
    /// Allocate per-lane barriers and initialize execution counters.
    fn new(lanes: usize, helpers: usize, auto_shutdown: bool) -> Self {
        Self {
            state: Mutex::new(State {
                phase: Phase::Running,
                stats: Stats {
                    total: lanes + helpers,
                    ..Stats::default()
                },
                error: None,
                helper_ready: vec![false; lanes],
                closed: vec![false; lanes],
                fenced: vec![false; lanes],
                auto_shutdown,
                check_scope: false,
            }),
            changed: Condvar::new(),
            stopping: AtomicBool::new(false),
            check_scope: AtomicBool::new(false),
            closing: AtomicBool::new(false),
            wakers: Mutex::new(Vec::new()),
        }
    }

    /// Wake `waker` on every later phase change, close, or fence.
    fn subscribe(&self, waker: &Waker) {
        self.wakers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .push(waker.clone());
    }

    /// Notify condition waiters and wake every parked lane and helper.
    fn notify(&self) {
        self.changed.notify_all();
        let wakers = self
            .wakers
            .lock()
            .unwrap_or_else(|e| e.into_inner())
            .clone();
        for waker in wakers {
            waker.wake();
        }
    }

    /// Recover coordinator state after panic so teardown can still make progress.
    fn lock(&self) -> MutexGuard<'_, State<E>> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Return the first recorded error, if any.
    fn result(&self) -> Result<(), E> {
        self.lock().error.map_or(Ok(()), Err)
    }

    /// Advance the lifecycle request without allowing it to move backward.
    fn set_phase(&self, phase: Phase) {
        let mut state = self.lock();
        state.phase = state.phase.max(phase);
        self.stopping
            .store(state.phase != Phase::Running, Ordering::Release);
        drop(state);
        self.notify();
    }

    /// Retain the first error and notify all threads to begin draining.
    ///
    /// A requested stop surfaces as `Error::Cancelled`. It yields to the first
    /// real failure recorded after it, so drain, checkpoint, or fence errors
    /// during a signaled stop are reported instead of a clean exit.
    fn fail(&self, error: E) {
        let mut state = self.lock();
        let cancelled = E::from(Error::Cancelled);
        match state.error {
            None => state.error = Some(error),
            Some(first) if first == cancelled && error != cancelled => {
                state.error = Some(error);
            }
            Some(_) => {}
        }
        state.phase = state.phase.max(Phase::Drain);
        self.stopping.store(true, Ordering::Release);
        drop(state);
        self.notify();
    }

    /// Announce that a lane has closed its helper submissions.
    fn close(&self, lane: usize) {
        self.lock().closed[lane] = true;
        self.closing.store(true, Ordering::Release);
        self.notify();
    }

    /// Announce a completed lane ownership fence to its helper.
    fn fence(&self, lane: usize) {
        self.lock().fenced[lane] = true;
        self.notify();
    }

    /// Wait for a barrier while checking caller policy outside the state lock.
    fn wait_for<S: Scope<Error = E>>(
        &self,
        scope: &S,
        done: impl Fn(&State<E>) -> bool,
    ) -> Result<(), E>
    where
        E: From<Error>,
    {
        let mut state = self.lock();
        loop {
            if let Some(error) = state.error {
                return Err(error);
            }
            if done(&state) {
                return Ok(());
            }
            // Caller policy may panic or reenter diagnostics. Never call it
            // while holding coordinator state.
            drop(state);
            scope.check()?;
            state = self.lock();
            state = self
                .changed
                .wait_timeout(state, IDLE_WAIT)
                .unwrap_or_else(|e| e.into_inner())
                .0;
        }
    }

    /// Apply optional steady-state scope policy and inspect the stop request.
    fn stopping<S: Scope<Error = E>>(&self, scope: &S) -> bool
    where
        E: From<Error>,
    {
        if self.check_scope.load(Ordering::Relaxed) {
            record(self, scope.check());
        }
        self.stopping.load(Ordering::Acquire)
    }

    /// Publish a completed drain and wait for permission to shut down.
    fn drained(&self) {
        let mut state = self.lock();
        state.stats.drained += 1;
        self.changed.notify_all();
        while state.phase != Phase::Shutdown && !state.auto_shutdown && state.error.is_none() {
            state = self.changed.wait(state).unwrap_or_else(|e| e.into_inner());
        }
    }
}

/// Account thread exit without ever claiming it established an ownership fence.
struct Exit<E: Copy + PartialEq + From<Error>> {
    control: Arc<Control<E>>,

    lane: Option<usize>,
}

impl<E: Copy + PartialEq + From<Error>> Drop for Exit<E> {
    /// Record thread exit and publication closure, but never claim an ownership fence.
    fn drop(&mut self) {
        if let Some(lane) = self.lane {
            self.control.close(lane);
        }
        self.control.lock().stats.done += 1;
        self.control.changed.notify_all();
    }
}

/// Record an ordinary lifecycle error without interrupting ownership fencing.
fn record<E: Copy + PartialEq + From<Error>>(control: &Control<E>, result: Result<(), E>) {
    if let Err(error) = result {
        control.fail(error);
    }
}

/// Abort on a failed fence rather than unwind through externally owned resources.
fn require_fence<E: Copy + PartialEq + From<Error>>(control: &Control<E>, result: Result<(), E>) {
    if let Err(error) = result {
        control.fail(error);
        // Returning would destroy resources whose ownership is still external.
        std::process::abort();
    }
}

/// Ownership evidence, separate from group requests and thread-exit accounting.
#[derive(Debug, PartialEq, Eq)]
enum ReleaseStage {
    /// The service may still own externally referenced work.
    Live,

    /// The first fence succeeded; shutdown may create more external work.
    Drained,

    /// The second fence succeeded, but sibling helper fences may still be pending.
    ShutdownFenced,

    /// All required fences finished and ordinary destruction is now safe.
    Releasable,
}

/// Retain a service until its owner explicitly completes both ownership fences.
/// A live guard aborts before Rust can drop its service, including during unwind.
struct ServiceOwner<S: Scope> {
    service: Box<dyn Service<S>>,

    stage: ReleaseStage,
}

impl<S: Scope> ServiceOwner<S> {
    /// Protect a newly constructed service until explicit lifecycle completion.
    fn new(service: Box<dyn Service<S>>) -> Self {
        Self {
            service,
            stage: ReleaseStage::Live,
        }
    }

    /// Attach error reporting without letting a caller panic release the owner.
    fn attach_reporter(&mut self, control: &Arc<Control<S::Error>>) {
        record(control, {
            self.set_failure_reporter(FailureReporter {
                control: control.clone(),
            });
            Ok(())
        });
    }

    /// Drive the lane's first fence before publishing its drain barrier.
    fn fence_lane(&mut self, scope: &S, control: &Control<S::Error>, waker: &Waker) {
        assert_eq!(self.stage, ReleaseStage::Live);
        drive_fence(&mut **self, scope, control, waker);
        self.stage = ReleaseStage::Drained;
    }

    /// Record shutdown errors but require a successful second lane fence.
    fn shutdown_lane(&mut self, scope: &S, control: &Control<S::Error>, waker: &Waker) {
        assert_eq!(self.stage, ReleaseStage::Drained);
        let mut park = self.parker();
        record(
            control,
            drive(
                self.shutdown(scope),
                scope,
                control,
                false,
                waker,
                &mut *park,
            ),
        );
        drive_fence(&mut **self, scope, control, waker);
        self.stage = ReleaseStage::ShutdownFenced;
    }

    /// Fence independent helper resources without blocking sibling futures.
    async fn fence_helper(
        &mut self,
        scope: &S,
        control: &Control<S::Error>,
        waker: &Waker,
    ) -> Result<(), S::Error> {
        assert_eq!(self.stage, ReleaseStage::Live);
        diagnose_operation(
            fence_operation(
                Box::pin(async {
                    self.register_driver(waker);
                    self.fence(scope).await
                }),
                control,
            ),
            scope,
            control,
        )
        .await?;
        self.stage = ReleaseStage::Drained;
        Ok(())
    }

    /// Cooperatively shut down and fence a helper, retaining cohort ownership.
    fn shutdown_helper<'a>(
        &'a mut self,
        scope: &'a S,
        control: &'a Control<S::Error>,
        waker: &'a Waker,
    ) -> Operation<'a, (), S::Error> {
        diagnose_operation(
            Box::pin(async move {
                assert_eq!(self.stage, ReleaseStage::Drained);
                record(
                    control,
                    Box::pin(async {
                        self.register_driver(waker);
                        self.shutdown(scope).await
                    })
                    .await,
                );
                self.register_driver(waker);
                fence_operation(self.fence(scope), control).await?;
                self.stage = ReleaseStage::ShutdownFenced;
                Ok(())
            }),
            scope,
            control,
        )
    }

    /// Permit destruction only after the second fence and the owner's final pass.
    /// Helpers call this after every sibling finishes, not within a shard future.
    fn release(&mut self) {
        assert_eq!(self.stage, ReleaseStage::ShutdownFenced);
        self.stage = ReleaseStage::Releasable;
    }
}

impl<S: Scope> std::ops::Deref for ServiceOwner<S> {
    /// Locally owned service protected by the lifecycle fences.
    type Target = dyn Service<S>;

    /// Borrow the service without transferring its destruction authority.
    fn deref(&self) -> &Self::Target {
        &*self.service
    }
}

impl<S: Scope> std::ops::DerefMut for ServiceOwner<S> {
    /// Mutably borrow the service while retaining the fail-closed owner guard.
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut *self.service
    }
}

impl<S: Scope> Drop for ServiceOwner<S> {
    /// Destroy a fenced service or abort rather than unwind through a live owner.
    fn drop(&mut self) {
        if self.stage != ReleaseStage::Releasable {
            std::process::abort();
        }
        // Rust drops the service field only after this guard permits release.
    }
}

/// Fence preallocated endpoints for a lane that never constructed a service.
fn abandon_lane<S: Scope>(factory: &dyn Factory<S>, index: usize, control: &Control<S::Error>) {
    require_fence(control, factory.abandon_lane(index));
    control.close(index);
    control.fence(index);
}

/// Account and fence every unstarted lane after preparation fails.
fn rollback_prepared<S: Scope>(
    plan: &Plan,
    factory: &dyn Factory<S>,
    control: &Control<S::Error>,
    error: S::Error,
) {
    control.fail(error);
    for index in 0..plan.lanes.len() {
        abandon_lane(factory, index, control);
    }
    let mut state = control.lock();
    state.stats.done = state.stats.total;
    state.stats.drained = state.stats.total;
    control.changed.notify_all();
}

/// Spawn scoped peers, drive lane zero, join all peers, and restore caller affinity.
fn run_prepared<S: Scope + Send>(
    plan: &Plan,
    factory: &dyn Factory<S>,
    scope: &S,
    control: &Arc<Control<S::Error>>,
) -> Result<(), S::Error> {
    let original = match affinity::current_cpus() {
        Ok(original) => original,
        Err(error) => {
            let error = error.into();
            rollback_prepared(plan, factory, control, error);
            return Err(error);
        }
    };
    let mut lane_scopes: Vec<_> = plan.lanes.iter().map(|_| scope.clone()).collect();
    let helper_scopes: Vec<_> = plan.helpers.iter().map(|_| scope.clone()).collect();
    let result = {
        let first_scope = lane_scopes.remove(0);
        thread::scope(|threads| {
            let mut handles = Vec::new();
            let mut launched = vec![false; plan.lanes.len()];
            for (helper_index, (helper, startup)) in
                plan.helpers.iter().zip(helper_scopes).enumerate()
            {
                let thread_control = control.clone();
                let result = thread::Builder::new()
                    .name(helper.name.clone())
                    .spawn_scoped(threads, move || {
                        record(
                            &thread_control,
                            helper_thread(factory, helper, startup, &thread_control),
                        );
                    });
                match result {
                    Ok(h) => handles.push(h),
                    Err(_) => {
                        control.fail(Error::Io.into());
                        let missing = plan.helpers.len() - helper_index;
                        let mut state = control.lock();
                        state.stats.done += missing;
                        state.stats.drained += missing;
                        break;
                    }
                }
            }
            for ((index, lane), startup) in plan.lanes.iter().enumerate().skip(1).zip(lane_scopes) {
                let thread_control = control.clone();
                let result = thread::Builder::new().name(lane.name.clone()).spawn_scoped(
                    threads,
                    move || {
                        record(
                            &thread_control,
                            lane_thread(factory, lane, index, startup, &thread_control),
                        );
                    },
                );
                match result {
                    Ok(h) => {
                        handles.push(h);
                        launched[index] = true;
                    }
                    Err(_) => {
                        control.fail(Error::Io.into());
                        break;
                    }
                }
            }
            for (index, live) in launched.iter().enumerate().skip(1) {
                if !live {
                    abandon_lane(factory, index, control);
                    let mut state = control.lock();
                    state.stats.done += 1;
                    state.stats.drained += 1;
                }
            }
            record(
                control,
                lane_thread(factory, &plan.lanes[0], 0, first_scope, control),
            );
            for handle in handles {
                if handle.join().is_err() {
                    control.fail(Error::Io.into());
                }
            }
        });
        control.result()
    };
    let restore = affinity::set_cpus(&original).map_err(Into::into);
    result.and(restore)
}

/// Drive a lifecycle future; only startup may stop early for caller cancellation.
fn drive<S: Scope>(
    mut operation: Operation<'_, (), S::Error>,
    scope: &S,
    control: &Control<S::Error>,
    startup: bool,
    waker: &Waker,
    park: &mut dyn Park<S::Error>,
) -> Result<(), S::Error> {
    let registration = if startup {
        scope.cancellation().map(|c| c.subscribe()).transpose()?
    } else {
        None
    };
    if let Some(registration) = &registration {
        registration.register(waker);
    }
    let mut cx = Context::from_waker(waker);
    loop {
        if startup {
            scope.check()?;
            if control.lock().phase != Phase::Running {
                return Err(Error::Cancelled.into());
            }
        } else {
            // Deadlines diagnose a stuck teardown, never cancel its ownership.
            record(control, scope.check());
        }
        if let Poll::Ready(result) = operation.as_mut().poll(&mut cx) {
            return result;
        }
        // Park until a wake source fires: a completion, a cross-thread or
        // phase-change wake, cancellation, the parker's own deadline, or the
        // scope deadline. An expired teardown scope is only diagnosed, so it
        // does not bound the wait.
        let mut wait = park.wait_timeout(MAX_PARK).min(MAX_PARK);
        if let Some(recheck) = scope.recheck_within()
            && !recheck.is_zero()
        {
            wait = wait.min(recheck);
        }
        let parked = park.park(wait);
        if startup {
            parked?;
        } else {
            record(control, parked);
        }
    }
}

/// Poll one bounded service turn and compute its post-poll park limit:
/// the service's next deadline and, when the run scope is checked, the
/// scope's own recheck bound.
fn poll_turn<S: Scope>(
    service: &mut dyn Service<S>,
    cx: &mut Context<'_>,
    scope: &S,
    check_scope: bool,
) -> Result<Duration, S::Error> {
    service.register_driver(cx.waker());
    service.poll_budgeted(cx, WORK_BUDGET)?;
    let mut wait = service.wait_timeout(MAX_PARK).min(MAX_PARK);
    if check_scope && let Some(recheck) = scope.recheck_within() {
        wait = wait.min(recheck);
    }
    Ok(wait)
}

/// Run a pinned lane through startup, steady-state work, and both teardown fences.
fn lane_thread<S: Scope>(
    factory: &dyn Factory<S>,
    lane: &Lane,
    index: usize,
    startup: S,
    control: &Arc<Control<S::Error>>,
) -> Result<(), S::Error> {
    let _exit = Exit {
        control: control.clone(),
        lane: Some(index),
    };
    if let Err(error) = affinity::pin_cpu(lane.cpu)
        .map_err(Into::into)
        .and_then(|()| control.wait_for(&startup, |s| s.helper_ready[index]))
    {
        abandon_lane(factory, index, control);
        control.lock().stats.drained += 1;
        return Err(error);
    }
    let mut service = match factory.build_lane(index) {
        Ok(service) => ServiceOwner::new(service),
        Err(error) => {
            abandon_lane(factory, index, control);
            control.lock().stats.drained += 1;
            return Err(error);
        }
    };
    service.attach_reporter(control);
    let waker = service.waker().unwrap_or_else(|error| {
        control.fail(error);
        crate::drivers::thread_waker(None)
    });
    control.subscribe(&waker);
    let mut park = service.parker();
    let started = drive(
        service.start(&startup),
        &startup,
        control,
        true,
        &waker,
        &mut *park,
    );
    if started.is_ok() {
        control.lock().stats.ready += 1;
        control.changed.notify_all();
        let check_scope = control.check_scope.load(Ordering::Relaxed);
        let registration = if check_scope {
            subscribe_cancellation(&startup, control)
        } else {
            None
        };
        if let Some(registration) = &registration {
            registration.register(&waker);
        }
        let mut cx = Context::from_waker(&waker);
        while !control.stopping(&startup) {
            match poll_turn(&mut *service, &mut cx, &startup, check_scope).and_then(|wait| {
                // Parks until a wake source fires or the service's own deadline.
                service.park(wait)
            }) {
                Ok(()) => {}
                Err(error) => {
                    control.fail(error);
                    break;
                }
            }
        }
    } else {
        record(control, started);
    }
    record(control, service.stop_admission());
    let teardown = &factory.teardown_scope(&startup);
    record(
        control,
        drive(
            service.drain(teardown),
            teardown,
            control,
            false,
            &waker,
            &mut *park,
        ),
    );
    record(control, service.close());
    control.close(index);
    service.fence_lane(teardown, control, &waker);
    control.fence(index);
    control.drained();
    service.shutdown_lane(teardown, control, &waker);
    service.release();
    Ok(())
}

/// Construct and drive helper shards cooperatively on their shared pinned thread.
fn helper_thread<S: Scope>(
    factory: &dyn Factory<S>,
    helper: &Helper,
    startup: S,
    control: &Arc<Control<S::Error>>,
) -> Result<(), S::Error> {
    let _exit = Exit {
        control: control.clone(),
        lane: None,
    };
    if let Err(error) = affinity::pin_cpu(helper.cpu) {
        control.lock().stats.drained += 1;
        return Err(error.into());
    }
    let waker = crate::drivers::thread_waker(None);
    let mut services = Vec::new();
    for &index in &helper.lanes {
        match factory.build_helper(index) {
            Ok(service) => {
                let mut service = ServiceOwner::new(service);
                service.attach_reporter(control);
                services.push((index, service));
            }
            Err(error) => {
                control.fail(error);
                break;
            }
        }
    }
    let registration = subscribe_cancellation(&startup, control);
    if let Some(registration) = &registration {
        registration.register(&waker);
    }
    control.subscribe(&waker);
    let indices: Vec<_> = services.iter().map(|(i, _)| *i).collect();
    let mut ready = false;
    let park = Cell::new(MAX_PARK);
    let operations = services
        .iter_mut()
        .map(|(index, service)| {
            helper_service(*index, service, factory, &startup, control, &waker, &park)
        })
        .collect();
    drive_helpers(operations, control, &waker, Some(&park), || {
        if ready {
            return;
        }
        let mut state = control.lock();
        if !indices.is_empty() && indices.iter().all(|i| state.helper_ready[*i]) {
            state.stats.ready += 1;
            ready = true;
            control.changed.notify_all();
        }
    });
    control.drained();
    let teardown = &factory.teardown_scope(&startup);
    let operations = services
        .iter_mut()
        .map(|(_, service)| service.shutdown_helper(teardown, control, &waker))
        .collect();
    drive_helpers(operations, control, &waker, None, || {});
    for (_, service) in &mut services {
        service.release();
    }
    Ok(())
}

/// Drive one helper shard until its lane and independent resources are fenced.
fn helper_service<'a, S: Scope>(
    index: usize,
    service: &'a mut ServiceOwner<S>,
    factory: &'a dyn Factory<S>,
    startup: &'a S,
    control: &'a Control<S::Error>,
    waker: &'a Waker,
    park: &'a Cell<Duration>,
) -> Operation<'a, (), S::Error> {
    // Outside the steady state, lifecycle futures are polled at `IDLE_WAIT`.
    let steady = Rc::new(Cell::new(false));
    let phase = steady.clone();
    let mut operation: Operation<'a, (), S::Error> = Box::pin(async move {
        let started = Box::pin(async {
            service.register_driver(waker);
            let mut operation = service.start(startup);
            std::future::poll_fn(|cx| {
                startup.check()?;
                if control.lock().phase != Phase::Running {
                    return Poll::Ready(Err(Error::Cancelled.into()));
                }
                operation.as_mut().poll(cx)
            })
            .await
        })
        .await;
        if started.is_ok() {
            control.lock().helper_ready[index] = true;
            control.changed.notify_all();
        } else {
            record(control, started);
        }
        phase.set(true);
        std::future::poll_fn(|cx| {
            record(control, {
                service.register_driver(waker);
                service.poll_budgeted(cx, 1)
            });
            park.set(park.get().min(service.wait_timeout(MAX_PARK)));
            // Skip the coordinator lock until some lane has closed.
            if control.closing.load(Ordering::Acquire) && control.lock().closed[index] {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        phase.set(false);
        let teardown = &factory.teardown_scope(startup);
        record(control, service.stop_admission());
        record(
            control,
            diagnose_operation(
                Box::pin(async {
                    service.register_driver(waker);
                    service.drain(teardown).await
                }),
                teardown,
                control,
            )
            .await,
        );
        record(control, service.close());
        std::future::poll_fn(|cx| {
            record(control, teardown.check());
            record(control, {
                service.register_driver(waker);
                service.poll_budgeted(cx, 1)
            });
            if control.lock().fenced[index] {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        // Helpers can own independent resources, not just lane completions.
        // Fence those before publishing drained, polling sibling futures fairly.
        service.fence_helper(teardown, control, waker).await?;
        Ok(())
    });
    Box::pin(std::future::poll_fn(move |cx| {
        let result = operation.as_mut().poll(cx);
        if !steady.get() {
            park.set(park.get().min(IDLE_WAIT));
        }
        result
    }))
}

/// Subscribe to caller cancellation, recording a subscription failure.
fn subscribe_cancellation<S: Scope>(
    scope: &S,
    control: &Control<S::Error>,
) -> Option<crate::environment::CancellationRegistration> {
    match scope
        .cancellation()
        .map(|c| c.subscribe())
        .transpose()
        .map_err(S::Error::from)
    {
        Ok(registration) => registration,
        Err(error) => {
            control.fail(error);
            None
        }
    }
}

/// Drive a lane fence, including fail-closed handling of construction panics.
fn drive_fence<S: Scope>(
    service: &mut dyn Service<S>,
    scope: &S,
    control: &Control<S::Error>,
    waker: &Waker,
) {
    let mut park = service.parker();
    require_fence(control, {
        drive(
            fence_operation(service.fence(scope), control),
            scope,
            control,
            false,
            waker,
            &mut *park,
        )
    });
}

/// Abort immediately if a fence future fails or panics while being polled.
fn fence_operation<'a, E: Copy + PartialEq + From<Error> + 'a>(
    mut operation: Operation<'a, (), E>,
    control: &'a Control<E>,
) -> Operation<'a, (), E> {
    Box::pin(std::future::poll_fn(move |cx| {
        match operation.as_mut().poll(cx) {
            Poll::Pending => Poll::Pending,
            Poll::Ready(result) => {
                require_fence(control, result);
                Poll::Ready(Ok(()))
            }
        }
    }))
}

/// Report expired teardown policy without canceling the underlying future.
fn diagnose_operation<'a, S: Scope>(
    mut operation: Operation<'a, (), S::Error>,
    scope: &'a S,
    control: &'a Control<S::Error>,
) -> Operation<'a, (), S::Error> {
    Box::pin(std::future::poll_fn(move |cx| {
        record(control, scope.check());
        operation.as_mut().poll(cx)
    }))
}

/// Rotate helper polling fairly and retain every future through its final
/// fence. After `WORK_BUDGET` passes the thread parks until a wake or the
/// bound the futures reported in `park` (`IDLE_WAIT` without one); the
/// short spin keeps wake latency off a busy helper's path.
fn drive_helpers<E: Copy + PartialEq + From<Error>>(
    operations: Vec<Operation<'_, (), E>>,
    control: &Control<E>,
    waker: &Waker,
    park: Option<&Cell<Duration>>,
    mut after_pass: impl FnMut(),
) {
    let mut operations: Vec<_> = operations.into_iter().map(Some).collect();
    let mut remaining = operations.len();
    let mut first = 0;
    let mut passes = 0;
    let mut cx = Context::from_waker(waker);
    while remaining != 0 {
        if let Some(park) = park {
            park.set(MAX_PARK);
        }
        for offset in 0..operations.len() {
            let index = (first + offset) % operations.len();
            if let Some(operation) = &mut operations[index]
                && let Poll::Ready(result) = operation.as_mut().poll(&mut cx)
            {
                require_fence(control, result);
                operations[index] = None;
                remaining -= 1;
            }
        }
        after_pass();
        first = (first + 1) % operations.len();
        passes += 1;
        if remaining != 0 && passes == WORK_BUDGET {
            passes = 0;
            let wait = park.map_or(IDLE_WAIT, Cell::get);
            if !wait.is_zero() {
                thread::park_timeout(wait);
            }
        }
    }
}

/// Linux CPU, cgroup, NUMA, and NIC discovery and calling-thread affinity.
/// Placement policy belongs to the caller, not the runtime.
pub mod affinity {
    use crate::{Error, Result};
    use std::{
        collections::{BTreeMap, BTreeSet, HashSet},
        fs,
        num::NonZeroU64,
        path::{Path, PathBuf},
    };

    /// Logical CPU identity and its physical placement, without scheduling policy.
    #[derive(Clone, Debug)]
    pub struct CpuLocation {
        /// Logical CPU number accepted by Linux affinity syscalls.
        pub cpu: usize,

        /// Physical core IDs are package-local; SMT siblings share this pair.
        pub package: usize,

        /// Physical core number within the package.
        pub core: usize,

        /// NUMA node when sysfs exposes one.
        pub numa_node: Option<usize>,
    }

    /// Tightest applicable effective cgroup CPU-time quota. None means unlimited.
    #[derive(Clone, Copy, Debug)]
    pub struct CpuQuota {
        /// CPU time available during each accounting period, in microseconds.
        pub quota: NonZeroU64,

        /// Accounting period in microseconds.
        pub period: NonZeroU64,
    }

    /// Discovered local hardware, without application placement policy.
    #[derive(Clone, Debug)]
    pub struct NicLocality {
        /// Network or InfiniBand device name reported by sysfs.
        pub device: String,

        /// Device-local NUMA node, if known.
        pub numa_node: Option<usize>,
    }

    /// Effective CPU constraints and local devices visible to the calling thread.
    #[derive(Clone)]
    pub struct EffectiveTopology {
        /// Online CPUs intersected with process affinity and effective cpuset.
        pub cpus: Vec<CpuLocation>,

        /// Tightest ancestor CPU-time quota, or none when every hierarchy is unlimited.
        pub quota: Option<CpuQuota>,

        /// Network and InfiniBand devices, with available NUMA locality.
        pub nics: Vec<NicLocality>,
    }

    /// One lane and its NUMA-local shared helper execution placement.
    pub struct Placement {
        /// CPU that owns the lane's local graph.
        pub lane: CpuLocation,

        /// CPU shared by the lane's helper service.
        pub helper: CpuLocation,

        /// Optional caller-filtered local device hint.
        pub nic: Option<NicLocality>,
    }

    /// Place roughly two lanes per helper within effective CPU and thread budgets.
    /// Device filtering and lane identity limits remain caller policy.
    pub fn place(
        topology: EffectiveTopology,
        max_threads: usize,
        max_lanes: usize,
        allow_smt: bool,
    ) -> Result<Vec<Placement>> {
        if max_threads < 2 || topology.cpus.is_empty() {
            return Err(Error::InvalidConfiguration);
        }
        // Fractional CPU capacity still permits two roles sharing one allowed CPU.
        let quota = topology.quota.map(|quota| {
            usize::try_from(quota.quota.get() / quota.period.get())
                .unwrap_or(usize::MAX)
                .max(1)
        });
        let mut cpus = topology.cpus;
        cpus.sort_by_key(|cpu| cpu.cpu);
        if cpus.windows(2).any(|pair| pair[0].cpu == pair[1].cpu) {
            return Err(Error::InvalidConfiguration);
        }
        if !allow_smt {
            let mut cores = HashSet::new();
            cpus.retain(|cpu| cores.insert((cpu.package, cpu.core)));
        }
        let mut nics = topology.nics;
        nics.sort_by(|a, b| a.device.cmp(&b.device));
        nics.retain(|nic| nic.numa_node.is_some());
        let capacity = quota.unwrap_or(cpus.len()).min(cpus.len());
        let mut remaining = max_threads;
        let mut available = capacity;
        let mut nodes = BTreeMap::<_, Vec<_>>::new();
        for cpu in cpus {
            nodes.entry(cpu.numa_node).or_default().push(cpu);
        }
        let mut nodes = nodes.into_iter().collect::<Vec<_>>();
        nodes.sort_by_key(|(node, _)| (!nics.iter().any(|nic| nic.numa_node == *node), *node));
        let mut pairs = Vec::new();
        for (node, local) in nodes {
            if remaining < 2 || available == 0 {
                break;
            }
            // Prefer physical-core diversity among lanes even with SMT enabled.
            let mut cores = HashSet::new();
            let mut ordered = local
                .into_iter()
                .map(|cpu| (!cores.insert((cpu.package, cpu.core)), cpu))
                .collect::<Vec<_>>();
            ordered.sort_by_key(|(sibling, cpu)| (*sibling, cpu.cpu));
            let mut local = ordered.into_iter().map(|(_, cpu)| cpu).collect::<Vec<_>>();
            let count = local.len().min(remaining).min(available);
            local.truncate(count);
            // Nearest integral 2:1 split spends both remainder cores for n%3=2.
            let helper_count = ((count + 1) / 3).max(1);
            let lane_count = count.saturating_sub(helper_count).max(1);
            let helpers = if count == 1 {
                &local[..]
            } else {
                &local[lane_count..]
            };
            let nic = nics.iter().find(|nic| nic.numa_node == node).cloned();
            for (index, lane) in local[..lane_count].iter().enumerate() {
                if pairs.len() >= max_lanes {
                    break;
                }
                pairs.push(Placement {
                    lane: lane.clone(),
                    helper: helpers[index % helpers.len()].clone(),
                    nic: nic.clone(),
                });
            }
            remaining -= count.max(2);
            available -= count;
        }
        Ok(pairs)
    }

    /// Exact roles on the first preferred NUMA node, without fallback or sharing CPUs.
    pub fn place_exact(
        mut topology: EffectiveTopology,
        max_threads: usize,
        lanes: usize,
        helpers: usize,
        allow_smt: bool,
    ) -> Result<Vec<Placement>> {
        let total = lanes
            .checked_add(helpers)
            .ok_or(Error::InvalidConfiguration)?;
        if lanes == 0 || helpers == 0 || helpers > lanes || total > max_threads {
            return Err(Error::InvalidConfiguration);
        }
        if topology
            .quota
            .is_some_and(|q| u128::from(q.quota.get()) / u128::from(q.period.get()) < total as u128)
        {
            return Err(Error::InvalidConfiguration);
        }
        topology.cpus.sort_by_key(|cpu| cpu.cpu);
        if topology
            .cpus
            .windows(2)
            .any(|pair| pair[0].cpu == pair[1].cpu)
        {
            return Err(Error::InvalidConfiguration);
        }
        if !allow_smt {
            let mut cores = HashSet::new();
            topology
                .cpus
                .retain(|cpu| cores.insert((cpu.package, cpu.core)));
        }
        topology.nics.sort_by(|a, b| a.device.cmp(&b.device));
        topology.nics.retain(|nic| nic.numa_node.is_some());
        let mut nodes = BTreeMap::<_, Vec<_>>::new();
        for cpu in topology.cpus {
            nodes.entry(cpu.numa_node).or_default().push(cpu);
        }
        let mut nodes = nodes.into_iter().collect::<Vec<_>>();
        nodes.sort_by_key(|(node, _)| {
            (
                !topology.nics.iter().any(|nic| nic.numa_node == *node),
                *node,
            )
        });
        let (node, local) = nodes
            .into_iter()
            .next()
            .ok_or(Error::InvalidConfiguration)?;
        if node.is_none() || local.len() < total {
            return Err(Error::InvalidConfiguration);
        }
        let mut cores = HashSet::new();
        let mut ordered = local
            .into_iter()
            .map(|cpu| (!cores.insert((cpu.package, cpu.core)), cpu))
            .collect::<Vec<_>>();
        ordered.sort_by_key(|(sibling, cpu)| (*sibling, cpu.cpu));
        let local = ordered.into_iter().map(|(_, cpu)| cpu).collect::<Vec<_>>();
        let nic = topology.nics.into_iter().find(|nic| nic.numa_node == node);
        Ok((0..lanes)
            .map(|index| Placement {
                lane: local[index].clone(),
                helper: local[lanes + index % helpers].clone(),
                nic: nic.clone(),
            })
            .collect())
    }

    /// Placement regressions independent of application worker IDs and rails.
    #[cfg(test)]
    mod placement_tests {
        use super::*;

        /// Nearest integral 2:1 assignment uses every eligible core and keeps helpers local.
        #[test]
        fn one_through_nine_fill_eligible_cores_with_local_shared_crypto() {
            for (cores, lanes, helpers) in [
                (1, 1, 1),
                (2, 1, 1),
                (3, 2, 1),
                (4, 3, 1),
                (5, 3, 2),
                (6, 4, 2),
                (7, 5, 2),
                (8, 5, 3),
                (9, 6, 3),
            ] {
                for smt in [false, true] {
                    let topology = EffectiveTopology {
                        cpus: (0..cores)
                            .map(|cpu| CpuLocation {
                                cpu,
                                package: 0,
                                core: cpu,
                                numa_node: Some(0),
                            })
                            .collect(),
                        quota: None,
                        nics: vec![],
                    };
                    let plan = place(topology, usize::MAX, usize::MAX, smt).unwrap();
                    assert_eq!(plan.len(), lanes, "cores={cores}, smt={smt}");
                    assert_eq!(
                        plan.iter()
                            .map(|p| p.helper.cpu)
                            .collect::<BTreeSet<_>>()
                            .len(),
                        helpers
                    );
                    let assigned = plan
                        .iter()
                        .flat_map(|p| [p.lane.cpu, p.helper.cpu])
                        .collect::<BTreeSet<_>>();
                    assert_eq!(assigned, (0..cores).collect());
                    assert!(plan.iter().all(|p| p.lane.numa_node == p.helper.numa_node));
                    if cores > 1 {
                        assert!(plan.iter().all(|p| p.lane.cpu != p.helper.cpu));
                    }
                }
            }
        }

        /// Caller identity limits do not overflow, and malformed CPU sets fail before placement.
        #[test]
        fn lane_caps_and_invalid_topology_are_explicit() {
            let hardware = EffectiveTopology {
                cpus: (0..9)
                    .map(|cpu| CpuLocation {
                        cpu,
                        package: 0,
                        core: cpu,
                        numa_node: Some(0),
                    })
                    .collect(),
                quota: None,
                nics: vec![],
            };
            for cap in 0..8 {
                assert_eq!(
                    place(hardware.clone(), 9, cap, false).unwrap().len(),
                    cap.min(6)
                );
            }
            assert!(matches!(
                place(hardware.clone(), 1, 9, false),
                Err(Error::InvalidConfiguration)
            ));
            let mut duplicate = hardware.clone();
            duplicate.cpus[1].cpu = 0;
            assert!(matches!(
                place(duplicate, 9, 9, true),
                Err(Error::InvalidConfiguration)
            ));
            let mut empty = hardware;
            empty.cpus.clear();
            assert!(matches!(
                place(empty, 9, 9, false),
                Err(Error::InvalidConfiguration)
            ));
        }
    }

    impl EffectiveTopology {
        /// Discover constraints from the calling thread's actual Linux namespace.
        /// Walk every visible ancestor: leaf cpu.max alone misses parent restrictions.
        pub fn discover() -> Result<Self> {
            let mut allowed = current_cpus()?;
            let online = parse_cpu_list(
                &fs::read_to_string("/sys/devices/system/cpu/online").map_err(|_| Error::Io)?,
            )?;
            allowed.retain(|cpu| online.contains(cpu));
            let mut quota = None;
            let memberships =
                fs::read_to_string("/proc/thread-self/cgroup").map_err(|_| Error::Io)?;
            let mounts = fs::read_to_string("/proc/self/mountinfo").map_err(|_| Error::Io)?;
            for (leaf, root, v2, cpu, cpuset) in cgroup_paths(&memberships, &mounts)? {
                // Unresolved membership must never be interpreted as unlimited.
                if !fs::metadata(&leaf).map_err(|_| Error::Io)?.is_dir() {
                    return Err(Error::InvalidConfiguration);
                }
                for directory in leaf.ancestors().take_while(|path| path.starts_with(&root)) {
                    if cpu {
                        let candidate = if v2 {
                            optional_text(&directory.join("cpu.max"))?
                                .map(|value| parse_v2_quota(&value))
                                .transpose()?
                                .flatten()
                        } else {
                            match (
                                optional_text(&directory.join("cpu.cfs_quota_us"))?,
                                optional_text(&directory.join("cpu.cfs_period_us"))?,
                            ) {
                                (Some(q), Some(p)) => parse_v1_quota(&q, &p)?,
                                (None, None) => None,
                                _ => return Err(Error::InvalidConfiguration),
                            }
                        };
                        tighten_quota(&mut quota, candidate);
                    }
                    if cpuset {
                        let names: &[&str] = if v2 {
                            &["cpuset.cpus.effective", "cpuset.cpus"]
                        } else {
                            &["cpuset.effective_cpus", "cpuset.cpus"]
                        };
                        for name in names {
                            if let Some(value) = optional_text(&directory.join(name))?
                                && !value.trim().is_empty()
                            {
                                let set = parse_cpu_list(&value)?;
                                allowed.retain(|cpu| set.contains(cpu));
                            }
                        }
                    }
                }
            }
            if allowed.is_empty() {
                return Err(Error::InvalidConfiguration);
            }
            let cpus = allowed
                .into_iter()
                .map(|cpu| {
                    let path = PathBuf::from(format!("/sys/devices/system/cpu/cpu{cpu}"));
                    Ok(CpuLocation {
                        cpu,
                        package: read_number(&path.join("topology/physical_package_id"))?,
                        core: read_number(&path.join("topology/core_id"))?,
                        numa_node: fs::read_dir(&path)
                            .map_err(|_| Error::Io)?
                            .filter_map(|entry| entry.ok())
                            .filter_map(|entry| {
                                entry
                                    .file_name()
                                    .to_str()?
                                    .strip_prefix("node")?
                                    .parse()
                                    .ok()
                            })
                            .min(),
                    })
                })
                .collect::<Result<Vec<_>>>()?;
            let mut nics = Vec::new();
            for directory in ["/sys/class/net", "/sys/class/infiniband"] {
                nics.extend(discover_nics(Path::new(directory))?);
            }
            Ok(Self { cpus, quota, nics })
        }
    }

    fn discover_nics(directory: &Path) -> Result<Vec<NicLocality>> {
        let entries = match fs::read_dir(directory) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(_) => return Err(Error::Io),
        };
        let mut nics = Vec::new();
        for entry in entries {
            let entry = entry.map_err(|_| Error::Io)?;
            // Sysfs class directories also contain controls such as bonding_masters.
            // Follow device symlinks, but do not treat regular files as devices.
            if !fs::metadata(entry.path()).map_err(|_| Error::Io)?.is_dir() {
                continue;
            }
            let numa_node = optional_text(&entry.path().join("device/numa_node"))?
                .and_then(|value| value.trim().parse::<usize>().ok());
            nics.push(NicLocality {
                device: entry.file_name().to_string_lossy().into_owned(),
                numa_node,
            });
        }
        Ok(nics)
    }

    /// Read the calling thread's allowed logical CPU IDs.
    pub fn current_cpus() -> Result<BTreeSet<usize>> {
        let mut mask = vec![0usize; 16];
        loop {
            // SAFETY: the kernel receives the size of the writable, word-aligned mask.
            let result = unsafe {
                libc::sched_getaffinity(
                    0,
                    std::mem::size_of_val(mask.as_slice()),
                    mask.as_mut_ptr().cast(),
                )
            };
            if result == 0 {
                break;
            }
            if std::io::Error::last_os_error().raw_os_error() != Some(libc::EINVAL)
                || mask.len() >= 16384
            {
                return Err(Error::Io);
            }
            mask.resize(mask.len() * 2, 0);
        }
        Ok(mask
            .iter()
            .enumerate()
            .flat_map(|(word, bits)| {
                (0..usize::BITS as usize)
                    .filter(move |bit| bits & (1usize << bit) != 0)
                    .map(move |bit| word * usize::BITS as usize + bit)
            })
            .collect())
    }

    /// Set the calling thread's allowed logical CPU IDs.
    pub fn set_cpus(cpus: &BTreeSet<usize>) -> Result<()> {
        let max = *cpus.last().ok_or(Error::InvalidConfiguration)?;
        if max > 1_048_575 {
            return Err(Error::InvalidConfiguration);
        }
        let mut mask = vec![0usize; (max / usize::BITS as usize + 1).max(16)];
        for cpu in cpus {
            mask[cpu / usize::BITS as usize] |= 1usize << (cpu % usize::BITS as usize);
        }
        // SAFETY: the kernel only reads the sized, word-aligned affinity mask.
        if unsafe {
            libc::sched_setaffinity(
                0,
                std::mem::size_of_val(mask.as_slice()),
                mask.as_ptr().cast(),
            )
        } != 0
        {
            return Err(Error::Io);
        }
        Ok(())
    }

    /// Pin the calling thread to one logical CPU.
    pub fn pin_cpu(cpu: usize) -> Result<()> {
        set_cpus(&BTreeSet::from([cpu]))
    }

    /// Read an optional kernel attribute, distinguishing absence from I/O failure.
    fn optional_text(path: &Path) -> Result<Option<String>> {
        match fs::read_to_string(path) {
            Ok(value) => Ok(Some(value)),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(_) => Err(Error::Io),
        }
    }

    /// Parse a required nonnegative sysfs topology attribute.
    fn read_number(path: &Path) -> Result<usize> {
        fs::read_to_string(path)
            .map_err(|_| Error::Io)?
            .trim()
            .parse()
            .map_err(|_| Error::InvalidConfiguration)
    }

    /// Parse Linux's comma-separated CPU ranges, rejecting reversal and huge indices.
    fn parse_cpu_list(value: &str) -> Result<BTreeSet<usize>> {
        let mut cpus = BTreeSet::new();
        if value.trim().is_empty() {
            return Ok(cpus);
        }
        for part in value.trim().split(',') {
            let (start, end) = part.split_once('-').unwrap_or((part, part));
            let start = start
                .parse::<usize>()
                .map_err(|_| Error::InvalidConfiguration)?;
            let end = end
                .parse::<usize>()
                .map_err(|_| Error::InvalidConfiguration)?;
            if start > end || end > 1_048_575 {
                return Err(Error::InvalidConfiguration);
            }
            cpus.extend(start..=end);
        }
        Ok(cpus)
    }

    /// Parse exactly two cpu.max fields without allocating an intermediate collection.
    fn parse_v2_quota(value: &str) -> Result<Option<CpuQuota>> {
        let mut fields = value.split_whitespace();
        let (Some(quota), Some(period), None) = (fields.next(), fields.next(), fields.next())
        else {
            return Err(Error::InvalidConfiguration);
        };
        parse_v1_quota(if quota == "max" { "-1" } else { quota }, period)
    }

    /// Validate a quota and positive period, including the v1 unlimited sentinel.
    fn parse_v1_quota(quota: &str, period: &str) -> Result<Option<CpuQuota>> {
        let period = period
            .trim()
            .parse::<NonZeroU64>()
            .map_err(|_| Error::InvalidConfiguration)?;
        if quota.trim() == "-1" {
            return Ok(None);
        }
        Ok(Some(CpuQuota {
            quota: quota
                .trim()
                .parse()
                .map_err(|_| Error::InvalidConfiguration)?,
            period,
        }))
    }

    /// Retain the smaller exact ratio using wide products to avoid rounding or overflow.
    fn tighten_quota(current: &mut Option<CpuQuota>, candidate: Option<CpuQuota>) {
        if let Some(candidate) = candidate
            && current.is_none_or(|old| {
                u128::from(candidate.quota.get()) * u128::from(old.period.get())
                    < u128::from(old.quota.get()) * u128::from(candidate.period.get())
            })
        {
            *current = Some(candidate);
        }
    }

    /// Membership leaf, mount boundary, unified flag, and CPU/cpuset controller flags.
    type CgroupPath = (PathBuf, PathBuf, bool, bool, bool);

    /// Resolve only covering cgroup mounts and reject hidden applicable controllers.
    fn cgroup_paths(memberships: &str, mounts: &str) -> Result<Vec<CgroupPath>> {
        let mut paths = Vec::new();
        let memberships = memberships
            .lines()
            .map(|line| {
                let fields = line.splitn(3, ':').collect::<Vec<_>>();
                if fields.len() != 3
                    || !Path::new(fields[2]).is_absolute()
                    || Path::new(fields[2])
                        .components()
                        .any(|part| matches!(part, std::path::Component::ParentDir))
                {
                    return Err(Error::InvalidConfiguration);
                }
                Ok(fields)
            })
            .collect::<Result<Vec<_>>>()?;
        let mut covered = HashSet::new();
        for line in mounts.lines() {
            let Some((before, after)) = line.split_once(" - ") else {
                continue;
            };
            let before = before.split_whitespace().collect::<Vec<_>>();
            let after = after.split_whitespace().collect::<Vec<_>>();
            if before.len() < 5 || after.len() < 3 {
                continue;
            }
            let v2 = after[0] == "cgroup2";
            if !v2 && after[0] != "cgroup" {
                continue;
            }
            let controllers = after[2].split(',').collect::<HashSet<_>>();
            let cpu = v2 || controllers.contains("cpu");
            let cpuset = v2 || controllers.contains("cpuset");
            if !cpu && !cpuset {
                continue;
            }
            for (index, fields) in memberships.iter().enumerate() {
                let matches = if v2 {
                    fields[1].is_empty()
                } else {
                    fields[1]
                        .split(',')
                        .any(|controller| controllers.contains(controller))
                };
                if !matches {
                    continue;
                }
                let root = PathBuf::from(unescape_mount(before[4]));
                let mount_root = PathBuf::from(unescape_mount(before[3]));
                let membership = PathBuf::from(fields[2]);
                if !root.is_absolute() || !mount_root.is_absolute() {
                    return Err(Error::InvalidConfiguration);
                }
                // Both proc files use the calling namespace. A noncovering subtree
                // mount must not act as a fallback root for this membership.
                let Ok(relative) = membership.strip_prefix(&mount_root) else {
                    continue;
                };
                if v2 {
                    covered.insert((index, ""));
                }
                if cpu {
                    covered.insert((index, "cpu"));
                }
                if cpuset {
                    covered.insert((index, "cpuset"));
                }
                paths.push((root.join(relative), root, v2, cpu, cpuset));
            }
        }
        for (index, fields) in memberships.iter().enumerate() {
            for controller in fields[1].split(',') {
                if matches!(controller, "" | "cpu" | "cpuset")
                    && !covered.contains(&(index, controller))
                {
                    // Hidden applicable hierarchies are unknown, not unlimited.
                    return Err(Error::InvalidConfiguration);
                }
            }
        }
        Ok(paths)
    }

    /// Decode proc mountinfo escapes once, leaving escaped backslashes uninterpreted.
    fn unescape_mount(value: &str) -> String {
        value
            .replace("\\040", " ")
            .replace("\\011", "\t")
            .replace("\\012", "\n")
            .replace("\\134", "\\")
    }

    /// Linux affinity and cgroup parsing boundary and state-space regressions.
    #[cfg(test)]
    mod tests {
        use super::*;

        #[test]
        fn nic_discovery_skips_controls_and_follows_device_links() {
            let root =
                std::env::temp_dir().join(format!("racer-nic-discovery-{}", std::process::id()));
            fs::create_dir(&root).unwrap();
            let class = root.join("class");
            fs::create_dir(&class).unwrap();
            fs::create_dir_all(root.join("nic/device")).unwrap();
            fs::write(root.join("nic/device/numa_node"), "1\n").unwrap();
            std::os::unix::fs::symlink(root.join("nic"), class.join("mlx5_00")).unwrap();
            fs::write(class.join("bonding_masters"), "bond0\n").unwrap();
            fs::create_dir(class.join("virtual")).unwrap();
            let mut nics = discover_nics(&class).unwrap();
            nics.sort_by(|a, b| a.device.cmp(&b.device));
            assert_eq!(nics.len(), 2);
            assert_eq!(nics[0].device, "mlx5_00");
            assert_eq!(nics[0].numa_node, Some(1));
            assert_eq!(nics[1].device, "virtual");
            assert_eq!(nics[1].numa_node, None);
            assert!(discover_nics(&root.join("absent")).unwrap().is_empty());
            assert!(matches!(
                discover_nics(&class.join("bonding_masters")),
                Err(Error::Io)
            ));
            fs::remove_dir_all(root).unwrap();
        }

        /// Ignore noncovering mounts and reject hidden applicable CPU controllers.
        #[test]
        fn noncovering_mounts_are_skipped_and_hidden_hierarchies_fail_closed() {
            let unrelated = "1 0 0:1 /other /unrelated rw - cgroup2 cgroup rw\n";
            let covering = "2 0 0:1 /tenant /visible\\040group rw - cgroup2 cgroup rw\n";
            let paths = cgroup_paths("0::/tenant/leaf", &format!("{unrelated}{covering}")).unwrap();
            assert_eq!(paths.len(), 1);
            assert_eq!(paths[0].0, PathBuf::from("/visible group/leaf"));
            assert!(cgroup_paths("0::/tenant/leaf", unrelated).is_err());
            assert!(cgroup_paths("0::/tenant/leaf", "").is_err());
            assert!(cgroup_paths("2:cpu:/tenant/leaf", "").is_err());
            assert!(cgroup_paths("2:cpuset:/tenant/leaf", "").is_err());
            assert!(
                cgroup_paths("2:memory:/tenant/leaf", "")
                    .unwrap()
                    .is_empty()
            );
            let paths = cgroup_paths("0::/", "1 0 0:1 / /visible rw - cgroup2 cgroup rw").unwrap();
            assert_eq!(paths[0].0, PathBuf::from("/visible"));
        }

        /// Resolve all applicable hierarchies and retain the tightest ancestor quota.
        #[test]
        fn cgroup_mounts_and_ancestor_quotas_are_resolved_exactly() {
            let mounts = "1 0 0:1 / /sys/fs/cgroup rw - cgroup2 cgroup rw\n\
                2 0 0:2 /tenant /cpu rw - cgroup cgroup rw,cpu,cpuacct\n\
                3 0 0:3 / /sets rw - cgroup cgroup rw,cpuset\n";
            let paths = cgroup_paths(
                "0::/tenant/leaf\n2:cpu,cpuacct:/tenant/leaf\n3:cpuset:/tenant/leaf\n",
                mounts,
            )
            .unwrap();
            assert_eq!(
                paths,
                vec![
                    (
                        "/sys/fs/cgroup/tenant/leaf".into(),
                        "/sys/fs/cgroup".into(),
                        true,
                        true,
                        true
                    ),
                    ("/cpu/leaf".into(), "/cpu".into(), false, true, false),
                    (
                        "/sets/tenant/leaf".into(),
                        "/sets".into(),
                        false,
                        false,
                        true
                    ),
                ]
            );
            let mut quota = parse_v2_quota("400000 100000").unwrap();
            tighten_quota(&mut quota, parse_v1_quota("150000", "100000").unwrap());
            tighten_quota(&mut quota, parse_v2_quota("max 100000").unwrap());
            tighten_quota(&mut quota, parse_v2_quota("200000 100000").unwrap());
            assert_eq!(quota.unwrap().quota.get(), 150000);
            assert!(parse_v2_quota("0 100000").is_err());
            assert!(parse_v2_quota("max 0").is_err());
            assert!(parse_v1_quota("-2", "100000").is_err());
            assert!(cgroup_paths("0::/../escape", mounts).is_err());
        }

        /// Pin only the calling thread and restore its complete original affinity.
        #[test]
        fn actual_affinity_is_applied_on_the_calling_thread() {
            std::thread::spawn(|| {
                let allowed = current_cpus().unwrap();
                let cpu = *allowed.first().unwrap();
                pin_cpu(cpu).unwrap();
                assert_eq!(current_cpus().unwrap(), BTreeSet::from([cpu]));
                let discovered = EffectiveTopology::discover().unwrap();
                assert_eq!(discovered.cpus.len(), 1);
                assert_eq!(discovered.cpus[0].cpu, cpu);
                set_cpus(&allowed).unwrap();
                assert_eq!(current_cpus().unwrap(), allowed);
            })
            .join()
            .unwrap();
        }

        /// Accept bounded CPU ranges and reject malformed or reversed intervals.
        #[test]
        fn constrained_cpuset_ranges_are_validated() {
            for value in ["", " \n"] {
                assert_eq!(parse_cpu_list(value).unwrap(), BTreeSet::new());
            }
            assert_eq!(
                parse_cpu_list(" 3,1-3,2,0,1048575\n").unwrap(),
                BTreeSet::from([0, 1, 2, 3, 1_048_575])
            );
            assert_eq!(
                parse_cpu_list("1-3,8,10-11\n").unwrap(),
                BTreeSet::from([1, 2, 3, 8, 10, 11])
            );
            for value in [
                "4-1",
                "-1",
                "a",
                "1-2-3",
                "1048576",
                "999999999",
                "1,,2",
                "1,",
            ] {
                assert_eq!(
                    parse_cpu_list(value),
                    Err(Error::InvalidConfiguration),
                    "{value}"
                );
            }
        }

        /// Validate both cgroup quota formats, including unlimited and overflow cases.
        #[test]
        fn quota_formats_validate_fields_and_unlimited_periods() {
            for (quota, period, expected) in [
                ("150000", "100000", Some((150000, 100000))),
                (" 1\n", " 2\n", Some((1, 2))),
                ("-1", "100000", None),
            ] {
                let v1 = parse_v1_quota(quota, period).unwrap();
                let v2 = parse_v2_quota(&format!(
                    "{} {period}",
                    if quota == "-1" { "max" } else { quota }
                ))
                .unwrap();
                let pair = |value: Option<CpuQuota>| value.map(|q| (q.quota.get(), q.period.get()));
                assert_eq!(pair(v1), expected);
                assert_eq!(pair(v2), expected);
            }
            for value in [
                "",
                "max",
                "1 2 3",
                "0 1",
                "-2 1",
                "1 0",
                "max 0",
                "max nope",
                "1 -1",
                "nope 1",
                "18446744073709551616 1",
                "1 18446744073709551616",
            ] {
                assert!(
                    matches!(parse_v2_quota(value), Err(Error::InvalidConfiguration)),
                    "{value}"
                );
            }
        }

        /// Compare quota ratios exactly even near the integer representation limit.
        #[test]
        fn quotas_compare_ratios_without_rounding_or_overflow() {
            let mut quota = None;
            for (candidate, expected) in [
                ("max 10", None),
                ("3 2", Some((3, 2))),
                ("4 3", Some((4, 3))),
                ("8 6", Some((4, 3))),
                ("max 10", Some((4, 3))),
                (
                    "18446744073709551615 18446744073709551614",
                    Some((u64::MAX, u64::MAX - 1)),
                ),
                (
                    "18446744073709551614 18446744073709551615",
                    Some((u64::MAX - 1, u64::MAX)),
                ),
                ("2 1", Some((u64::MAX - 1, u64::MAX))),
            ] {
                tighten_quota(&mut quota, parse_v2_quota(candidate).unwrap());
                assert_eq!(
                    quota.map(|q| (q.quota.get(), q.period.get())),
                    expected,
                    "{candidate}"
                );
            }
        }

        /// Resolve namespace-relative membership and decode mount escapes only once.
        #[test]
        fn cgroup_namespace_paths_and_mount_escapes_are_resolved() {
            let mounts = "malformed\n1 0 0:1 / /ignored rw - tmpfs tmpfs rw\n\
                2 0 0:2 / /memory rw - cgroup cgroup rw,memory\n\
                3 0 0:3 /host\\040root /group\\040mount rw - cgroup2 cgroup rw\n";
            assert_eq!(
                cgroup_paths("0::/host root/leaf", mounts).unwrap(),
                vec![(
                    "/group mount/leaf".into(),
                    "/group mount".into(),
                    true,
                    true,
                    true,
                )]
            );
            for membership in [
                "0::/leaf",
                "0::/",
                "malformed",
                "0::relative",
                "0::/leaf/../escape",
            ] {
                assert_eq!(
                    cgroup_paths(membership, mounts),
                    Err(Error::InvalidConfiguration)
                );
            }
            assert_eq!(unescape_mount(r"a\040b\011c\012d\134040"), "a b\tc\nd\\040");
        }

        /// Invalid masks must fail without changing the calling thread's affinity.
        #[test]
        fn invalid_affinity_masks_return_generic_errors_without_changing_affinity() {
            let allowed = current_cpus().unwrap();
            assert_eq!(set_cpus(&BTreeSet::new()), Err(Error::InvalidConfiguration));
            assert_eq!(pin_cpu(1_048_576), Err(Error::InvalidConfiguration));
            assert_eq!(current_cpus().unwrap(), allowed);
        }
    }
}

/// Lifecycle ordering, failure injection, affinity, and cooperative fencing regressions.
#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        collections::BTreeSet,
        rc::Rc,
        sync::atomic::{AtomicBool, Ordering},
    };

    /// Inert lifecycle policy for tests that control shutdown explicitly.
    #[derive(Clone)]
    struct TestScope;

    impl Scope for TestScope {
        /// Use portable runtime failures for lifecycle assertions.
        type Error = Error;

        /// Leave all lifecycle phases authorized.
        fn check(&self) -> Result<()> {
            Ok(())
        }
    }

    /// Compute wait policy after polling.
    #[test]
    fn steady_state_wait_is_post_poll_bounded() {
        /// Service that exposes whether polling preceded its wait-policy callback.
        struct Waiting {
            polled: bool,

            wait: Duration,
        }

        impl Service<TestScope> for Waiting {
            /// Complete startup without external resources.
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Verify the lane budget and record a completed service turn.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
                assert_eq!(budget, WORK_BUDGET);
                self.polled = true;
                Ok(())
            }

            /// Assert post-poll ordering.
            fn wait_timeout(&self, maximum: Duration) -> Duration {
                assert!(self.polled, "wait policy must observe the completed poll");
                assert_eq!(maximum, MAX_PARK);
                self.wait
            }

            /// Complete drain without retaining any resources.
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Complete shutdown without additional work.
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }
        let mut cx = Context::from_waker(Waker::noop());
        for wait in [Duration::ZERO, IDLE_WAIT / 2, IDLE_WAIT, IDLE_WAIT * 2] {
            let mut service = Waiting {
                polled: false,
                wait,
            };
            assert_eq!(
                poll_turn(&mut service, &mut cx, &TestScope, false),
                Ok(wait)
            );
            service.polled = false;
            assert_eq!(
                poll_turn(&mut service, &mut cx, &TestScope, true),
                Ok(wait.min(IDLE_WAIT))
            );
        }
    }

    /// Thread-exit accounting must not grant ownership-fence authority.
    #[test]
    fn exit_is_not_a_fence() {
        let control = Arc::new(Control::<Error>::new(1, 0, false));
        drop(Exit {
            control: control.clone(),
            lane: Some(0),
        });
        assert!(!control.lock().fenced[0]);
        assert_eq!(control.lock().stats.done, 1);
    }

    /// Both drivers retain ownership through the second fence until explicit release.
    #[test]
    fn service_owner_requires_both_fences_and_explicit_release() {
        for helper in [false, true] {
            let events = Arc::new(Mutex::new(Vec::new()));
            let mut owner = ServiceOwner::new(Box::new(Local {
                events: events.clone(),
                owner: Rc::new(thread::current().id()),
                stop: Arc::default(),
                drain_error: None,
            }));
            let control = Control::new(1, usize::from(helper), true);
            let waker = crate::drivers::thread_waker(None);
            assert_eq!(owner.stage, ReleaseStage::Live);
            if helper {
                drive_helpers(
                    vec![Box::pin(owner.fence_helper(&TestScope, &control, &waker))],
                    &control,
                    &waker,
                    None,
                    || {},
                );
            } else {
                owner.fence_lane(&TestScope, &control, &waker);
            }
            assert_eq!(owner.stage, ReleaseStage::Drained);
            assert_eq!(*events.lock().unwrap(), ["fence"]);
            if helper {
                drive_helpers(
                    vec![owner.shutdown_helper(&TestScope, &control, &waker)],
                    &control,
                    &waker,
                    None,
                    || {},
                );
            } else {
                owner.shutdown_lane(&TestScope, &control, &waker);
            }
            assert_eq!(owner.stage, ReleaseStage::ShutdownFenced);
            assert_eq!(*events.lock().unwrap(), ["fence", "shutdown", "fence"]);
            owner.release();
            assert_eq!(owner.stage, ReleaseStage::Releasable);
            assert_eq!(*events.lock().unwrap(), ["fence", "shutdown", "fence"]);
            drop(owner);
            assert_eq!(
                *events.lock().unwrap(),
                ["fence", "shutdown", "fence", "drop"]
            );
            assert_eq!(control.result(), Ok(()));
        }
    }

    /// A requested stop (`Cancelled`) yields to the first real failure after
    /// it, so post-stop drain or checkpoint errors are not reported as a clean
    /// stop. Real failures are never replaced.
    #[test]
    fn stop_yields_to_later_failures_but_failures_are_kept() {
        let control = Control::<Error>::new(1, 0, false);
        control.fail(Error::Cancelled);
        assert_eq!(control.result(), Err(Error::Cancelled));
        control.fail(Error::Cancelled);
        assert_eq!(control.result(), Err(Error::Cancelled));
        control.fail(Error::Io);
        assert_eq!(control.result(), Err(Error::Io));
        control.fail(Error::DeadlineExceeded);
        control.fail(Error::Cancelled);
        assert_eq!(control.result(), Err(Error::Io));

        let control = Control::<Error>::new(1, 0, false);
        control.fail(Error::DeadlineExceeded);
        control.fail(Error::Cancelled);
        assert_eq!(control.result(), Err(Error::DeadlineExceeded));
    }

    /// Failed fences and incomplete ownership stages abort before service destruction.
    #[test]
    fn fence_failure_aborts_before_service_drop() {
        use std::os::unix::process::ExitStatusExt;

        const CHILD: &str = "RUNTIME_FENCE_FAILURE_CHILD";
        if let Ok(mode) = std::env::var(CHILD) {
            // Do not produce core files for the deliberately fatal child.
            let limit = libc::rlimit {
                rlim_cur: 0,
                rlim_max: 0,
            };
            unsafe {
                libc::setrlimit(libc::RLIMIT_CORE, &limit);
                libc::prctl(libc::PR_SET_DUMPABLE, 0);
            }

            /// Service that injects a chosen fence failure and detects unsafe release.
            struct Fatal {
                mode: String,

                fences: usize,
            }

            impl Drop for Fatal {
                /// Exit distinctly if the failed-fence service is ever destroyed.
                fn drop(&mut self) {
                    std::process::exit(99);
                }
            }

            impl Service<TestScope> for Fatal {
                /// Finish startup so the test can reach the selected fence.
                fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }

                /// Perform no steady-state work before the injected fence failure.
                fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                    Ok(())
                }

                /// Finish drain without masking the injected fence result.
                fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }

                /// Finish shutdown to reach the second fence when requested.
                fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }

                /// Inject construction, polling, or result failure at the chosen fence.
                fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    self.fences += 1;
                    if self.mode.contains("pending") && self.fences == 2 {
                        return Box::pin(std::future::pending());
                    }
                    let fail = !self.mode.starts_with("owner")
                        && self.fences == if self.mode.contains("second") { 2 } else { 1 };
                    assert!(
                        !(fail && self.mode.contains("construct")),
                        "fence construction"
                    );
                    Box::pin(async move { if fail { Err(Error::Io) } else { Ok(()) } })
                }
            }

            /// Construct a service configured for the child process's failure mode.
            struct FatalFactory(String);

            impl Factory<TestScope> for FatalFactory {
                /// Create a fresh service with neither ownership fence completed.
                fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
                    Ok(Box::new(Fatal {
                        mode: self.0.clone(),
                        fences: 0,
                    }))
                }
            }
            if mode.starts_with("owner") {
                let control = Control::new(1, 0, true);
                let factory = FatalFactory(mode.clone());
                let mut owner = ServiceOwner::new(factory.build_lane(0).unwrap());
                let waker = crate::drivers::thread_waker(None);
                if !mode.contains("live") {
                    owner.fence_lane(&TestScope, &control, &waker);
                }
                if mode.contains("shutdown-fenced") {
                    owner.shutdown_lane(&TestScope, &control, &waker);
                }
                if mode.contains("unpolled") || mode.contains("pending") {
                    let mut operation = owner.shutdown_helper(&TestScope, &control, &waker);
                    if mode.contains("pending") {
                        let mut cx = Context::from_waker(&waker);
                        assert!(operation.as_mut().poll(&mut cx).is_pending());
                    }
                    drop(operation);
                    assert_eq!(owner.stage, ReleaseStage::Drained);
                }
                if mode.ends_with("release") {
                    let released = std::panic::catch_unwind(std::panic::AssertUnwindSafe(|| {
                        owner.release();
                    }));
                    assert!(released.is_err());
                }
                drop(owner);
            } else if mode.starts_with("helper") {
                let control = Control::new(1, 1, true);
                control.close(0);
                control.fence(0);
                let factory = FatalFactory(mode.clone());
                let mut owner = ServiceOwner::new(factory.build_lane(0).unwrap());
                let waker = crate::drivers::thread_waker(None);
                drive_helpers(
                    vec![helper_service(
                        0,
                        &mut owner,
                        &factory,
                        &TestScope,
                        &control,
                        &waker,
                        &Cell::new(MAX_PARK),
                    )],
                    &control,
                    &waker,
                    None,
                    || {},
                );
                // Exercise the post-shutdown helper fence as a separate pass.
                drive_helpers(
                    vec![owner.shutdown_helper(&TestScope, &control, &waker)],
                    &control,
                    &waker,
                    None,
                    || {},
                );
            } else {
                let mut plan = plan(1);
                plan.max_threads = 1;
                let control = Arc::new(Control::new(1, 0, true));
                control.lock().helper_ready[0] = true;
                control.set_phase(Phase::Shutdown);
                let _ = lane_thread(&FatalFactory(mode), &plan.lanes[0], 0, TestScope, &control);
            }
            std::process::exit(98);
        }
        for mode in [
            "lane-error",
            "lane-construct",
            "lane-second-error",
            "lane-second-construct",
            "helper-error",
            "helper-construct",
            "helper-second-error",
            "helper-second-construct",
            "owner-live-drop",
            "owner-drained-drop",
            "owner-shutdown-fenced-drop",
            "owner-live-release",
            "owner-drained-release",
            "owner-unpolled-release",
            "owner-pending-release",
        ] {
            let status = std::process::Command::new("timeout")
                .args(["--signal=TERM", "--kill-after=1s", "10s"])
                .arg(std::env::current_exe().unwrap())
                .args([
                    "--exact",
                    "group::tests::fence_failure_aborts_before_service_drop",
                    "--nocapture",
                ])
                .env(CHILD, mode)
                .status()
                .unwrap();
            assert_eq!(status.signal(), Some(libc::SIGABRT), "{mode}: {status}");
        }
    }

    /// Expired teardown policy reports an error without dropping a pending helper future.
    #[test]
    fn helper_shutdown_deadline_is_diagnostic_not_detachment() {
        /// Teardown policy that is expired on every check.
        #[derive(Clone)]
        struct Expired;

        impl Scope for Expired {
            /// Use runtime errors to preserve the deadline diagnosis.
            type Error = Error;

            /// Report expiry without granting permission to release live ownership.
            fn check(&self) -> Result<()> {
                Err(Error::DeadlineExceeded)
            }
        }
        let control = Control::new(0, 1, true);
        let polls = std::cell::Cell::new(0);
        let operation = Box::pin(std::future::poll_fn(|_| {
            polls.set(polls.get() + 1);
            if polls.get() == 4 {
                Poll::Ready(Ok(()))
            } else {
                Poll::Pending
            }
        }));
        let waker = crate::drivers::thread_waker(None);
        drive_helpers(
            vec![diagnose_operation(operation, &Expired, &control)],
            &control,
            &waker,
            None,
            || {},
        );
        assert_eq!(polls.get(), 4);
        assert_eq!(control.result(), Err(Error::DeadlineExceeded));
    }

    /// Place one lane on an allowed CPU with a caller-selected thread budget.
    fn plan(max_threads: usize) -> Plan {
        let cpu = *affinity::current_cpus().unwrap().first().unwrap();
        Plan {
            lanes: vec![Lane {
                name: "generic-lane".into(),
                cpu,
            }],
            helpers: Vec::new(),
            max_threads,
        }
    }

    /// Construction policy and shared lifecycle instrumentation for a local service.
    struct Recipe {
        cpu: usize,

        events: Arc<Mutex<Vec<&'static str>>>,

        stop: Arc<AtomicBool>,

        drain_error: Option<Error>,
    }

    /// Non-Send service that records lifecycle order and verifies owner-thread destruction.
    struct Local {
        events: Arc<Mutex<Vec<&'static str>>>,

        owner: Rc<thread::ThreadId>,

        stop: Arc<AtomicBool>,

        drain_error: Option<Error>,
    }

    impl Factory<TestScope> for Recipe {
        /// Verify affinity before constructing a thread-confined service.
        fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
            assert_eq!(
                affinity::current_cpus().unwrap(),
                BTreeSet::from([self.cpu])
            );
            Ok(Box::new(Local {
                events: self.events.clone(),
                owner: Rc::new(thread::current().id()),
                stop: self.stop.clone(),
                drain_error: self.drain_error,
            }))
        }

        /// Reject helper construction because this fixture plans only lanes.
        fn build_helper(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
            unreachable!()
        }
    }

    impl Drop for Local {
        /// Verify owner-thread destruction and record the final lifecycle event.
        fn drop(&mut self) {
            assert_eq!(*self.owner, thread::current().id());
            self.events.lock().unwrap().push("drop");
        }
    }

    impl Service<TestScope> for Local {
        /// Record startup when its future is driven.
        fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("start");
                Ok(())
            })
        }

        /// Verify bounded polling and inject an explicit stop request.
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
            assert_eq!(budget, 64);
            if self.stop.load(Ordering::SeqCst) {
                Err(Error::Cancelled)
            } else {
                Ok(())
            }
        }

        /// Record that admission stops before drain begins.
        fn stop_admission(&mut self) -> Result<()> {
            self.events.lock().unwrap().push("stop");
            Ok(())
        }

        /// Record completed draining of accepted work.
        fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("drain");
                self.drain_error.map_or(Ok(()), Err)
            })
        }

        /// Record closure of helper submissions after drain.
        fn close(&mut self) -> Result<()> {
            self.events.lock().unwrap().push("close");
            Ok(())
        }

        /// Record each ownership fence in lifecycle order.
        fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("fence");
                Ok(())
            })
        }

        /// Record shutdown between the two ownership fences.
        fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("shutdown");
                Ok(())
            })
        }
    }

    /// Create lifecycle instrumentation for the plan's coordinator lane.
    fn recipe(plan: &Plan) -> Recipe {
        Recipe {
            cpu: plan.lanes[0].cpu,
            events: Arc::default(),
            stop: Arc::default(),
            drain_error: None,
        }
    }

    /// A stop requested while running is a clean `Cancelled`, but a drain
    /// failure after that stop is reported instead of being swallowed.
    #[test]
    fn drain_failure_after_stop_is_reported() {
        for (drain_error, expected) in [(None, Error::Cancelled), (Some(Error::Io), Error::Io)] {
            let plan = plan(1);
            let mut factory = recipe(&plan);
            factory.drain_error = drain_error;
            factory.stop.store(true, Ordering::SeqCst);
            let mut group = Group::new(plan);
            assert_eq!(group.run(&factory, &TestScope), Err(expected));
            assert!(factory.events.lock().unwrap().contains(&"drain"));
        }
    }

    /// Dropping an owned group joins its lane after both fences and local destruction.
    #[test]
    fn owned_drop_joins_local_service_without_helpers() {
        let plan = plan(2);
        let factory = Arc::new(recipe(&plan));
        {
            let mut group = Group::new(plan);
            group.start(factory.clone(), &TestScope).unwrap();
            assert_eq!(group.stats().ready, 1);
        }
        assert_eq!(
            *factory.events.lock().unwrap(),
            [
                "start", "stop", "drain", "close", "fence", "shutdown", "fence", "drop"
            ]
        );
    }

    /// Reject invalid placement, duplicate helper ownership, and exhausted thread budgets.
    #[test]
    fn plans_reject_invalid_links_and_count_external_caller() {
        let plan = plan(1);
        let group = Group::<TestScope>::new(plan.clone());
        assert_eq!(group.validate(true), Ok(()));
        assert_eq!(group.validate(false), Err(Error::InvalidConfiguration));
        for lanes in [vec![], vec![1], vec![0, 0]] {
            let mut invalid = plan.clone();
            invalid.max_threads = 3;
            invalid.helpers.push(Helper {
                name: "helper".into(),
                cpu: plan.lanes[0].cpu,
                lanes,
            });
            assert_eq!(
                Group::<TestScope>::new(invalid).validate(true),
                Err(Error::InvalidConfiguration)
            );
        }
        for helper in [false, true] {
            let mut invalid = plan.clone();
            invalid.max_threads = 3;
            if helper {
                invalid.helpers.push(Helper {
                    name: "bad\0helper".into(),
                    cpu: plan.lanes[0].cpu,
                    lanes: vec![0],
                });
            } else {
                invalid.lanes[0].name = "bad\0lane".into();
            }
            assert_eq!(
                Group::<TestScope>::new(invalid).validate(true),
                Err(Error::InvalidConfiguration)
            );
        }
        let mut invalid = plan.clone();
        invalid.lanes[0].cpu = usize::MAX;
        assert_eq!(
            Group::<TestScope>::new(invalid).validate(true),
            Err(Error::InvalidConfiguration)
        );
        let mut invalid = plan.clone();
        invalid.max_threads = 3;
        let helper = Helper {
            name: "helper".into(),
            cpu: plan.lanes[0].cpu,
            lanes: vec![0],
        };
        invalid.helpers = vec![helper.clone(), helper];
        assert_eq!(
            Group::<TestScope>::new(invalid).validate(true),
            Err(Error::InvalidConfiguration)
        );
        let mut empty = plan;
        empty.lanes.clear();
        assert_eq!(
            Group::<TestScope>::new(empty).validate(true),
            Err(Error::InvalidConfiguration)
        );
    }

    /// Sibling helper fences progress cooperatively before drain accounting and destruction.
    #[test]
    fn helper_independent_resources_fence_cooperatively_before_barrier_and_drop() {
        use std::sync::atomic::AtomicUsize;

        /// Cross-helper fence-entry, completion, and destruction instrumentation.
        struct Shared {
            entered: [AtomicUsize; 2],

            completed: [AtomicUsize; 2],

            dropped: AtomicUsize,
        }

        /// Factory for lanes and helpers sharing cooperative fence state.
        struct Helpers {
            shared: Arc<Shared>,
        }

        /// Thread-confined helper with independent resources requiring both fences.
        struct Independent {
            shared: Arc<Shared>,

            owner: Rc<thread::ThreadId>,

            fences: usize,
        }

        /// Resource-free lane used to isolate helper-owned fence behavior.
        struct Empty;

        impl Factory<TestScope> for Helpers {
            /// Construct a lane without independent resource dependencies.
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
                Ok(Box::new(Empty))
            }

            /// Construct a local helper that must fence alongside its sibling.
            fn build_helper(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
                Ok(Box::new(Independent {
                    shared: self.shared.clone(),
                    owner: Rc::new(thread::current().id()),
                    fences: 0,
                }))
            }
        }

        impl Service<TestScope> for Empty {
            /// Complete the resource-free lane's startup.
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Keep the lane idle while helper lifecycles are exercised.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                Ok(())
            }

            /// Finish draining so the helper can begin its independent fence.
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Complete the resource-free lane's shutdown.
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }

        impl Service<TestScope> for Independent {
            /// Finish helper startup before cooperative resource fencing.
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Verify that each helper shard receives a bounded cooperative turn.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
                assert_eq!(budget, 1);
                Ok(())
            }

            /// Finish drain while retaining independently fenced helper resources.
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Require the sibling to enter the same fence before completing this one.
            fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                let phase = self.fences;
                self.fences += 1;
                self.shared.entered[phase].fetch_add(1, Ordering::SeqCst);
                Box::pin(std::future::poll_fn(move |_| {
                    // Each independent owner needs its sibling's fence to run.
                    // Sequential helper fencing would deadlock this lifecycle.
                    if self.shared.entered[phase].load(Ordering::SeqCst) != 2 {
                        return Poll::Pending;
                    }
                    self.shared.completed[phase].fetch_add(1, Ordering::SeqCst);
                    Poll::Ready(Ok(()))
                }))
            }

            /// Verify the first fence barrier and inject an ordinary shutdown failure.
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    assert_eq!(self.shared.completed[0].load(Ordering::SeqCst), 2);
                    // A shutdown failure still requires the second ownership fence.
                    Err(Error::Io)
                })
            }
        }

        impl Drop for Independent {
            /// Verify owner-thread destruction only after both independent fences ran.
            fn drop(&mut self) {
                assert_eq!(*self.owner, thread::current().id());
                assert_eq!(self.fences, 2);
                assert_eq!(self.shared.completed[1].load(Ordering::SeqCst), 2);
                self.shared.dropped.fetch_add(1, Ordering::SeqCst);
            }
        }
        let shared = Arc::new(Shared {
            entered: Default::default(),
            completed: Default::default(),
            dropped: AtomicUsize::new(0),
        });
        let mut plan = plan(4);
        plan.lanes.push(plan.lanes[0].clone());
        plan.helpers.push(Helper {
            name: "independent-helpers".into(),
            cpu: plan.lanes[0].cpu,
            lanes: vec![0, 1],
        });
        let mut group = Group::new(plan);
        group
            .start(
                Arc::new(Helpers {
                    shared: shared.clone(),
                }),
                &TestScope,
            )
            .unwrap();
        group.drain(&TestScope).unwrap();
        assert_eq!(
            shared.completed[0].load(Ordering::SeqCst),
            2,
            "drained must include independent helper fences"
        );
        assert_eq!(group.join(), Err(Error::Io));
        assert_eq!(shared.completed[1].load(Ordering::SeqCst), 2);
        assert_eq!(shared.dropped.load(Ordering::SeqCst), 2);
    }
}
