//! Pinned local services with explicit startup, drain, and ownership fences.
//!
//! Fail closed: an unsuccessful ownership fence or an unexpected unwind of a
//! live service aborts the process. Neither an error nor thread exit proves that
//! external I/O stopped referencing storage. We never detach or drop such owners.
use racer_io::exec::Next;
use racer_io::pin as affinity;
use racer_io::{Error, Operation, Result, Scope};
use std::{
    cell::Cell,
    future::Future,
    pin::Pin,
    rc::Rc,
    sync::{
        Arc, Condvar, Mutex, MutexGuard,
        atomic::{AtomicBool, Ordering},
    },
    task::{Context, Poll, Waker},
    thread::{self, JoinHandle},
    time::{Duration, Instant},
};

/// Short recheck interval offered to tests and services that poll a condition
/// without a wake source. The group itself never polls on it: lifecycle phases,
/// barriers, closes, and fences wake every waiter through its condvar or waker.
pub const IDLE_WAIT: Duration = Duration::from_millis(1);

/// Unbounded park. Lanes and helpers park until a wake source fires
/// (completion, cross-thread wake, phase change, stop signal) or a deadline
/// reported by a service or scope.
const FOREVER: Duration = Duration::MAX;

/// Park the calling thread until unparked or `timeout` elapses.
fn park_thread(timeout: Duration) {
    if timeout == FOREVER {
        thread::park();
    } else if !timeout.is_zero() {
        thread::park_timeout(timeout);
    }
}

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
        Ok(racer_io::exec::thread_waker())
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
        park_thread(timeout);
        Ok(())
    }
}

/// Failure and stop capability: cannot advance phases or authorize resource release.
#[derive(Clone)]
pub struct FailureReporter<E: Copy> {
    control: Arc<Control<E>>,
}

impl<E: Copy + PartialEq + From<Error>> FailureReporter<E> {
    /// Record the first failure and request that every lane stop admission.
    pub fn report(&self, error: E) {
        self.control.fail(error);
    }

    /// Request a clean stop ([`StopReason::Requested`]). A service then
    /// returning `Error::Canceled` stops the group without failing it.
    pub fn request_stop(&self) {
        self.control.request_stop(StopReason::Requested);
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

    observe_signal: bool,
}

impl<S: Scope + Send> Group<S> {
    /// Store a placement plan without spawning threads or constructing services.
    pub fn new(plan: Plan) -> Self {
        Self {
            plan,
            control: Arc::new(Control::new(0, 0, false)),
            coordinator: None,
            observe_signal: false,
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
        self.control
            .observe_signal
            .store(self.observe_signal, Ordering::Relaxed);
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

    /// Observe the process stop signal ([`racer_io::signal::stop_raised`]) on
    /// every lane turn of later runs, stopping with [`StopReason::Signal`].
    pub fn observe_stop_signal(&mut self) {
        self.observe_signal = true;
        self.control.observe_signal.store(true, Ordering::Relaxed);
    }

    /// Request a clean stop: drain and shut down without recording an error.
    pub fn request_stop(&self, reason: StopReason) {
        self.control.request_stop(reason);
    }

    /// Why the group stopped, if it stopped without (or before) failing.
    pub fn stop_reason(&self) -> Option<StopReason> {
        self.control.stop_reason()
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

/// Why a group stopped without failing. A stop is not an error: a group that
/// only stopped returns `Ok`, and any real failure recorded before or after
/// the stop is still reported.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub enum StopReason {
    /// A process stop signal (see [`racer_io::signal::raise_stop`]).
    Signal,
    /// A service, caller, or startup scope requested the stop
    /// (surfaced by the service as `Error::Canceled`).
    Requested,
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

    stop: Option<StopReason>,

    helper_ready: Vec<bool>,

    closed: Vec<bool>,

    fenced: Vec<bool>,

    auto_shutdown: bool,

    check_scope: bool,
}

impl<E: Copy + From<Error>> State<E> {
    /// The error that interrupts a barrier wait: the first failure, or
    /// `Canceled` once a stop was requested.
    fn interrupted(&self) -> Option<E> {
        self.error
            .or_else(|| self.stop.map(|_| E::from(Error::Canceled)))
    }
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

    /// Whether lane turns observe the process stop signal.
    observe_signal: AtomicBool,

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
                stop: None,
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
            observe_signal: AtomicBool::new(false),
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

    /// Return the first recorded error, if any. A stop alone is `Ok`.
    fn result(&self) -> Result<(), E> {
        self.lock().error.map_or(Ok(()), Err)
    }

    /// The first recorded stop request, if any.
    fn stop_reason(&self) -> Option<StopReason> {
        self.lock().stop
    }

    /// Record a stop request (the first reason is kept) and begin draining.
    fn request_stop(&self, reason: StopReason) {
        let mut state = self.lock();
        let changed = state.stop.is_none() || state.phase < Phase::Drain;
        state.stop.get_or_insert(reason);
        state.phase = state.phase.max(Phase::Drain);
        self.stopping.store(true, Ordering::Release);
        drop(state);
        // Repeated requests change nothing; waking every subscriber again
        // would make all lanes spin while they re-request the same stop.
        if changed {
            self.notify();
        }
    }

    /// Advance the lifecycle request without allowing it to move backward.
    fn set_phase(&self, phase: Phase) {
        let mut state = self.lock();
        let changed = state.phase < phase;
        state.phase = state.phase.max(phase);
        self.stopping
            .store(state.phase != Phase::Running, Ordering::Release);
        drop(state);
        if changed {
            self.notify();
        }
    }

    /// Retain the first error and notify all threads to begin draining.
    ///
    /// `Error::Canceled` is the echo of a stop, not a failure, but only when
    /// a real stop exists: one already recorded in the group or the raised
    /// process stop signal (recorded as [`StopReason::Signal`]). A stray
    /// `Canceled` with no stop behind it is a failure, so the process exits
    /// nonzero instead of reporting a clean stop. Drain, checkpoint, or fence
    /// errors during a stop are still reported.
    fn fail(&self, error: E) {
        let mut state = self.lock();
        if error == E::from(Error::Canceled) {
            if state.stop.is_some() {
                return;
            }
            if racer_io::signal::stop_raised() {
                drop(state);
                self.request_stop(StopReason::Signal);
                return;
            }
        }
        // An expired scope reports the same error on every poll. Only a
        // real state change may wake subscribers, or every wake would wake
        // every lane again and all cores would spin until teardown ends.
        let changed = state.error.is_none() || state.phase < Phase::Drain;
        state.error.get_or_insert(error);
        state.phase = state.phase.max(Phase::Drain);
        self.stopping.store(true, Ordering::Release);
        drop(state);
        if changed {
            self.notify();
        }
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
    /// Barrier changes and scope cancellation notify the condvar; only the
    /// scope's own recheck bound limits a wait, never a fixed poll interval.
    fn wait_for<S: Scope<Error = E>>(
        self: &Arc<Self>,
        scope: &S,
        done: impl Fn(&State<E>) -> bool,
    ) -> Result<(), E>
    where
        E: From<Error> + Send + 'static,
    {
        let registration = scope.cancel_wait();
        if let Some(registration) = &registration {
            registration.register(&Waker::from(Arc::new(ConditionWake(self.clone()))));
        }
        let mut state = self.lock();
        loop {
            if let Some(error) = state.interrupted() {
                return Err(error);
            }
            if done(&state) {
                return Ok(());
            }
            // Caller policy may panic or reenter diagnostics. Never call it
            // while holding coordinator state.
            drop(state);
            scope.check()?;
            let recheck = scope.recheck_within();
            state = self.lock();
            if state.interrupted().is_some() || done(&state) {
                continue;
            }
            state = match recheck {
                None => self.changed.wait(state).unwrap_or_else(|e| e.into_inner()),
                Some(recheck) => {
                    self.changed
                        .wait_timeout(state, recheck)
                        .unwrap_or_else(|e| e.into_inner())
                        .0
                }
            };
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
        self.poll_signal();
        self.stopping.load(Ordering::Acquire)
    }

    /// Turn an observed process stop signal into a [`StopReason::Signal`] stop.
    fn poll_signal(&self) {
        if self.observe_signal.load(Ordering::Relaxed)
            && !self.stopping.load(Ordering::Relaxed)
            && racer_io::signal::stop_raised()
        {
            self.request_stop(StopReason::Signal);
        }
    }
}

/// Wakes condition waiters when a caller's scope is canceled. Notifying under
/// the state lock orders the wake after any waiter's last check.
struct ConditionWake<E>(Arc<Control<E>>);

impl<E: Copy + PartialEq + From<Error> + Send + 'static> std::task::Wake for ConditionWake<E> {
    fn wake(self: Arc<Self>) {
        self.wake_by_ref();
    }

    fn wake_by_ref(self: &Arc<Self>) {
        let _state = self.0.lock();
        self.0.changed.notify_all();
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
    async fn fence_lane(&mut self, scope: &S, control: &Control<S::Error>, turn: &Turn<S::Error>) {
        assert_eq!(self.stage, ReleaseStage::Live);
        let fenced = drive(
            fence_operation(self.fence(scope), control),
            scope,
            control,
            false,
            turn,
        )
        .await;
        require_fence(control, fenced);
        self.stage = ReleaseStage::Drained;
    }

    /// Record shutdown errors but require a successful second lane fence.
    async fn shutdown_lane(
        &mut self,
        scope: &S,
        control: &Control<S::Error>,
        turn: &Turn<S::Error>,
    ) {
        assert_eq!(self.stage, ReleaseStage::Drained);
        record(
            control,
            drive(self.shutdown(scope), scope, control, false, turn).await,
        );
        let fenced = drive(
            fence_operation(self.fence(scope), control),
            scope,
            control,
            false,
            turn,
        )
        .await;
        require_fence(control, fenced);
        self.stage = ReleaseStage::ShutdownFenced;
    }

    /// Run the first lane fence to completion on the calling thread.
    #[cfg(test)]
    fn fence_lane_blocking(&mut self, scope: &S, control: &Control<S::Error>, waker: &Waker) {
        let turn = Turn::new();
        let fenced = self.fence_lane(scope, control, &turn);
        run_role(fenced, waker, Box::new(ThreadPark), &turn);
    }

    /// Run shutdown and the second lane fence to completion on the calling thread.
    #[cfg(test)]
    fn shutdown_lane_blocking(&mut self, scope: &S, control: &Control<S::Error>, waker: &Waker) {
        let turn = Turn::new();
        let shut = self.shutdown_lane(scope, control, &turn);
        run_role(shut, waker, Box::new(ThreadPark), &turn);
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

/// Per-thread wait state shared by a role and its lifecycle future. The
/// future reports the bound on its next wait; the role reports park failures
/// back to the future, which decides by phase whether they end the phase.
struct Turn<E> {
    /// Longest the next park may last; reset to `FOREVER` after each step.
    wait: Cell<Duration>,

    /// A failed park, consumed by the lifecycle future's next poll.
    parked: Cell<Option<E>>,
}

impl<E: Copy> Turn<E> {
    /// A turn with no bound and no failure.
    fn new() -> Rc<Self> {
        Rc::new(Self {
            wait: Cell::new(FOREVER),
            parked: Cell::new(None),
        })
    }

    /// Lower the next park bound to `wait`.
    fn bound(&self, wait: Duration) {
        self.wait.set(self.wait.get().min(wait));
    }

    /// Take a park failure recorded since the last poll.
    fn take_parked(&self) -> Option<E> {
        self.parked.take()
    }
}

/// A lane or helper thread as a [`racer_io::exec::Role`]: each step polls the
/// thread's whole lifecycle future once; parking waits on the service's wake
/// sources until the bound the future reported (no fixed recheck interval).
struct LifecycleRole<'a, E> {
    lifecycle: Pin<Box<dyn Future<Output = ()> + 'a>>,

    waker: Waker,

    park: Box<dyn Park<E>>,

    turn: Rc<Turn<E>>,
}

impl<E: Copy> racer_io::exec::Role for LifecycleRole<'_, E> {
    fn step(&mut self) -> racer_io::Result<Next> {
        let mut cx = Context::from_waker(&self.waker);
        if self.lifecycle.as_mut().poll(&mut cx).is_ready() {
            return Ok(Next::Done);
        }
        let wait = self
            .park
            .wait_timeout(self.turn.wait.replace(FOREVER).min(FOREVER));
        // A zero bound still parks: the park submits work queued by the turn.
        Ok(if wait == FOREVER {
            Next::Idle
        } else {
            Instant::now()
                .checked_add(wait)
                .map_or(Next::Idle, Next::WaitUntil)
        })
    }

    fn park(&mut self, next: Next) -> racer_io::Result<()> {
        let timeout = match next {
            Next::Ready | Next::Done => Duration::ZERO,
            Next::WaitUntil(until) => until.saturating_duration_since(Instant::now()),
            Next::Idle => FOREVER,
        };
        if let Err(error) = self.park.park(timeout) {
            self.turn.parked.set(Some(error));
        }
        Ok(())
    }
}

/// Run `lifecycle` on the calling thread as a role until it finishes.
fn run_role<'a, E: Copy + 'a>(
    lifecycle: impl Future<Output = ()> + 'a,
    waker: &Waker,
    park: Box<dyn Park<E>>,
    turn: &Rc<Turn<E>>,
) {
    let mut role = LifecycleRole {
        lifecycle: Box::pin(lifecycle),
        waker: waker.clone(),
        park,
        turn: turn.clone(),
    };
    // Steps and parks report failures through `Turn`, never as errors.
    let _ = racer_io::exec::run(&mut role);
}

/// Drive a lifecycle future; only startup may stop early for caller cancellation.
async fn drive<S: Scope>(
    mut operation: Operation<'_, (), S::Error>,
    scope: &S,
    control: &Control<S::Error>,
    startup: bool,
    turn: &Turn<S::Error>,
) -> Result<(), S::Error> {
    let registration = if startup { scope.cancel_wait() } else { None };
    let mut registered = false;
    std::future::poll_fn(|cx| {
        if let Some(registration) = &registration
            && !registered
        {
            registration.register(cx.waker());
            registered = true;
        }
        if let Some(error) = turn.take_parked() {
            if startup {
                return Poll::Ready(Err(error));
            }
            record(control, Err(error));
        }
        if startup {
            scope.check()?;
            control.poll_signal();
            if control.lock().phase != Phase::Running {
                return Poll::Ready(Err(Error::Canceled.into()));
            }
        } else {
            // Deadlines diagnose a stuck teardown, never cancel its ownership.
            record(control, scope.check());
        }
        if let Poll::Ready(result) = operation.as_mut().poll(cx) {
            return Poll::Ready(result);
        }
        // Park until a wake source fires: a completion, a cross-thread or
        // phase-change wake, cancellation, the parker's own deadline, or the
        // scope deadline. An expired teardown scope is only diagnosed, so it
        // does not bound the wait.
        if let Some(recheck) = scope.recheck_within()
            && !recheck.is_zero()
        {
            turn.bound(recheck);
        }
        Poll::Pending
    })
    .await
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
    let mut wait = service.wait_timeout(FOREVER);
    if check_scope && let Some(recheck) = scope.recheck_within() {
        wait = wait.min(recheck);
    }
    Ok(wait)
}

/// Publish a completed drain, then wait (on group wakes, not a thread block)
/// for permission to shut down.
async fn drained<E: Copy + PartialEq + From<Error>>(control: &Control<E>) {
    {
        let mut state = control.lock();
        state.stats.drained += 1;
        control.changed.notify_all();
    }
    std::future::poll_fn(|_| {
        let state = control.lock();
        if state.phase == Phase::Shutdown || state.auto_shutdown || state.interrupted().is_some() {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    })
    .await;
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
        racer_io::exec::thread_waker()
    });
    control.subscribe(&waker);
    let park = service.parker();
    let turn = Turn::new();
    let lifecycle = lane_lifecycle(service, factory, index, startup, control, turn.clone());
    run_role(lifecycle, &waker, park, &turn);
    Ok(())
}

/// The linear lane lifecycle: start, serve, stop admission, drain, close,
/// first fence, drained barrier, shutdown, second fence, release.
async fn lane_lifecycle<'a, S: Scope>(
    mut service: ServiceOwner<S>,
    factory: &'a dyn Factory<S>,
    index: usize,
    startup: S,
    control: &'a Arc<Control<S::Error>>,
    turn: Rc<Turn<S::Error>>,
) {
    let started = drive(service.start(&startup), &startup, control, true, &turn).await;
    if started.is_ok() {
        control.lock().stats.ready += 1;
        control.changed.notify_all();
        let check_scope = control.check_scope.load(Ordering::Relaxed);
        let registration = if check_scope {
            startup.cancel_wait()
        } else {
            None
        };
        let mut registered = false;
        std::future::poll_fn(|cx| {
            if let Some(registration) = &registration
                && !registered
            {
                registration.register(cx.waker());
                registered = true;
            }
            if let Some(error) = turn.take_parked() {
                control.fail(error);
                return Poll::Ready(());
            }
            if control.stopping(&startup) {
                return Poll::Ready(());
            }
            match poll_turn(&mut *service, cx, &startup, check_scope) {
                // Parks until a wake source fires or the service's own deadline.
                Ok(wait) => {
                    turn.bound(wait);
                    Poll::Pending
                }
                Err(error) => {
                    control.fail(error);
                    Poll::Ready(())
                }
            }
        })
        .await;
    } else {
        record(control, started);
    }
    record(control, service.stop_admission());
    let teardown = &factory.teardown_scope(&startup);
    record(
        control,
        drive(service.drain(teardown), teardown, control, false, &turn).await,
    );
    record(control, service.close());
    control.close(index);
    service.fence_lane(teardown, control, &turn).await;
    control.fence(index);
    drained(control).await;
    service.shutdown_lane(teardown, control, &turn).await;
    service.release();
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
    let waker = racer_io::exec::thread_waker();
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
    let registration = startup.cancel_wait();
    if let Some(registration) = &registration {
        registration.register(&waker);
    }
    control.subscribe(&waker);
    let turn = Turn::new();
    let lifecycle = helper_lifecycle(services, factory, &startup, control, &waker, &turn);
    run_role(lifecycle, &waker, Box::new(ThreadPark), &turn);
    Ok(())
}

/// The helper thread lifecycle: every shard through its lane's close and its
/// own first fence, the drained barrier, then shutdown and second fences.
async fn helper_lifecycle<'a, S: Scope>(
    mut services: Vec<(usize, ServiceOwner<S>)>,
    factory: &'a dyn Factory<S>,
    startup: &'a S,
    control: &'a Arc<Control<S::Error>>,
    waker: &'a Waker,
    turn: &'a Turn<S::Error>,
) {
    let indices: Vec<_> = services.iter().map(|(i, _)| *i).collect();
    let mut ready = false;
    let operations = services
        .iter_mut()
        .map(|(index, service)| {
            helper_service(
                *index, service, factory, startup, control, waker, &turn.wait,
            )
        })
        .collect();
    poll_helpers(operations, control, || {
        if ready {
            return;
        }
        let mut state = control.lock();
        if !indices.is_empty() && indices.iter().all(|i| state.helper_ready[*i]) {
            state.stats.ready += 1;
            ready = true;
            control.changed.notify_all();
        }
    })
    .await;
    drained(control).await;
    let teardown = &factory.teardown_scope(startup);
    let operations = services
        .iter_mut()
        .map(|(_, service)| service.shutdown_helper(teardown, control, waker))
        .collect();
    poll_helpers(operations, control, || {}).await;
    for (_, service) in &mut services {
        service.release();
    }
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
    // Outside the steady state, lifecycle futures wake through `waker`; only
    // startup's scope recheck bounds the helper's park.
    let steady = Rc::new(Cell::new(false));
    let phase = steady.clone();
    let mut operation: Operation<'a, (), S::Error> = Box::pin(async move {
        let started = Box::pin(async {
            service.register_driver(waker);
            let mut operation = service.start(startup);
            std::future::poll_fn(|cx| {
                startup.check()?;
                if control.lock().phase != Phase::Running {
                    return Poll::Ready(Err(Error::Canceled.into()));
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
            park.set(park.get().min(service.wait_timeout(FOREVER)));
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
        if !steady.get()
            && let Some(recheck) = startup.recheck_within()
        {
            park.set(park.get().min(recheck));
        }
        result
    }))
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
/// fence. Each poll is one pass over every live future; between passes the
/// helper parks until a wake (a wake during the pass makes the park return
/// at once) or the bound the futures reported.
async fn poll_helpers<E: Copy + PartialEq + From<Error>>(
    operations: Vec<Operation<'_, (), E>>,
    control: &Control<E>,
    mut after_pass: impl FnMut(),
) {
    let mut operations: Vec<_> = operations.into_iter().map(Some).collect();
    let mut remaining = operations.len();
    let mut first = 0;
    std::future::poll_fn(|cx| {
        for offset in 0..operations.len() {
            let index = (first + offset) % operations.len();
            if let Some(operation) = &mut operations[index]
                && let Poll::Ready(result) = operation.as_mut().poll(cx)
            {
                require_fence(control, result);
                operations[index] = None;
                remaining -= 1;
            }
        }
        after_pass();
        if !operations.is_empty() {
            first = (first + 1) % operations.len();
        }
        if remaining == 0 {
            Poll::Ready(())
        } else {
            Poll::Pending
        }
    })
    .await;
}

/// Drive helper futures to completion on the calling thread.
#[cfg(test)]
fn drive_helpers<E: Copy + PartialEq + From<Error>>(
    operations: Vec<Operation<'_, (), E>>,
    control: &Control<E>,
    waker: &Waker,
    park: Option<&Cell<Duration>>,
    after_pass: impl FnMut(),
) {
    let turn = Turn::<E>::new();
    let lifecycle = async {
        poll_helpers(operations, control, after_pass).await;
    };
    let lifecycle = std::future::poll_fn({
        let mut lifecycle = Box::pin(lifecycle);
        let turn = turn.clone();
        move |cx| {
            let result = lifecycle.as_mut().poll(cx);
            if let Some(park) = park {
                turn.bound(park.replace(FOREVER));
            }
            result
        }
    });
    run_role(lifecycle, waker, Box::new(ThreadPark), &turn);
}

/// Drive resource completions while a local operation borrows its service graph.
///
/// Each poll first drives the resources (`poll`, which also registers the
/// task's waker with them), then the operation. Nothing here waits or wakes
/// itself: the surrounding lifecycle driver parks on the resources' wake
/// sources (completions, cross-thread wakes, alarms, and scope deadlines).
///
/// Backend failures are reported immediately but do not abandon the operation.
/// Its completion fence runs to completion, then the first backend error wins.
pub fn drive_local_with<'a, T: 'a, E: Copy + PartialEq + From<racer_io::Error> + 'a>(
    mut operation: Operation<'a, T, E>,
    reporter: Option<&'a FailureReporter<E>>,
    mut poll: impl FnMut(&Waker) -> Result<(), E> + 'a,
) -> Operation<'a, T, E> {
    let mut error = None;
    Box::pin(std::future::poll_fn(move |cx| {
        if let Err(failure) = poll(cx.waker()) {
            if let Some(reporter) = reporter {
                reporter.report(failure);
            }
            error.get_or_insert(failure);
        }
        match operation.as_mut().poll(cx) {
            Poll::Ready(result) => Poll::Ready(error.map_or(result, Err)),
            Poll::Pending => Poll::Pending,
        }
    }))
}

/// Lifecycle ordering, failure injection, affinity, and cooperative fencing regressions.
#[cfg(test)]
mod tests;
