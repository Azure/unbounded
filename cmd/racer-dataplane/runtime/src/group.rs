//! Pinned local services with explicit startup, drain, and ownership fences.
//!
//! Fail closed: an unsuccessful ownership fence or an unexpected unwind of a
//! live service aborts the process. Neither an error nor thread exit proves that
//! external I/O stopped referencing storage. We never detach or drop such owners.
use crate::{Error, Operation, Result, Scope};
use std::{
    panic::{AssertUnwindSafe, catch_unwind},
    sync::{Arc, Condvar, Mutex, MutexGuard},
    task::{Context, Poll, Waker},
    thread::{self, JoinHandle},
    time::Duration,
};

const IDLE_WAIT: Duration = Duration::from_millis(1);

const WORK_BUDGET: usize = 64;

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
    fn wait_timeout(&self, maximum: Duration) -> Duration {
        maximum
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

/// Error-only group capability: cannot advance phases or authorize resource release.
#[derive(Clone)]
pub struct FailureReporter<E: Copy> {
    control: Arc<Control<E>>,
}

impl<E: Copy> FailureReporter<E> {
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
        let startup = match attempt(|| Ok::<_, S::Error>(scope.clone())) {
            Ok(startup) => startup,
            Err(error) => {
                rollback_prepared(&self.plan, &*factory, &self.control, error);
                return Err(error);
            }
        };
        let thread_factory = factory.clone();
        let handle = thread::Builder::new()
            .name(plan.lanes[0].name.clone())
            .spawn(move || {
                record(
                    &control,
                    attempt(|| run_prepared(&plan, &*thread_factory, &startup, &control)),
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
            attempt(|| run_prepared(&self.plan, factory, scope, &self.control)),
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
}

impl<E: Copy> Control<E> {
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
        self.changed.notify_all();
    }

    /// Retain the first error and notify all threads to begin draining.
    fn fail(&self, error: E) {
        let mut state = self.lock();
        state.error.get_or_insert(error);
        state.phase = state.phase.max(Phase::Drain);
        self.changed.notify_all();
    }

    /// Announce that a lane has closed its helper submissions.
    fn close(&self, lane: usize) {
        self.lock().closed[lane] = true;
        self.changed.notify_all();
    }

    /// Announce a completed lane ownership fence to its helper.
    fn fence(&self, lane: usize) {
        self.lock().fenced[lane] = true;
        self.changed.notify_all();
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
            attempt(|| scope.check())?;
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
        let check = self.lock().check_scope;
        if check {
            record(self, attempt(|| scope.check()));
        }
        self.lock().phase != Phase::Running
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
struct Exit<E: Copy + From<Error>> {
    control: Arc<Control<E>>,

    lane: Option<usize>,
}

impl<E: Copy + From<Error>> Drop for Exit<E> {
    /// Record thread exit and publication closure, but never claim an ownership fence.
    fn drop(&mut self) {
        if let Some(lane) = self.lane {
            self.control.close(lane);
        }
        if thread::panicking() {
            self.control.fail(Error::Io.into());
        }
        self.control.lock().stats.done += 1;
        self.control.changed.notify_all();
    }
}

/// Convert caller panics into lifecycle errors without losing teardown control.
fn attempt<T, E: From<Error>>(f: impl FnOnce() -> Result<T, E>) -> Result<T, E> {
    catch_unwind(AssertUnwindSafe(f)).unwrap_or_else(|_| Err(Error::Io.into()))
}

/// Record an ordinary lifecycle error without interrupting ownership fencing.
fn record<E: Copy>(control: &Control<E>, result: Result<(), E>) {
    if let Err(error) = result {
        control.fail(error);
    }
}

/// Abort on a failed fence rather than unwind through externally owned resources.
fn require_fence<E: Copy>(control: &Control<E>, result: Result<(), E>) {
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
        record(
            control,
            attempt(|| {
                self.set_failure_reporter(FailureReporter {
                    control: control.clone(),
                });
                Ok(())
            }),
        );
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
        record(
            control,
            attempt(|| drive(self.shutdown(scope), scope, control, false, waker)),
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
                    catch_operation(Box::pin(async {
                        self.register_driver(waker);
                        self.shutdown(scope).await
                    }))
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

/// Obtain fresh teardown policy, recording failure without cloning a fallback.
fn teardown_scope<S: Scope>(
    factory: &dyn Factory<S>,
    startup: &S,
    control: &Control<S::Error>,
) -> Option<S> {
    match attempt(|| Ok::<_, S::Error>(factory.teardown_scope(startup))) {
        Ok(scope) => Some(scope),
        Err(error) => {
            control.fail(error);
            None
        }
    }
}

/// Fence preallocated endpoints for a lane that never constructed a service.
fn abandon_lane<S: Scope>(factory: &dyn Factory<S>, index: usize, control: &Control<S::Error>) {
    require_fence(control, attempt(|| factory.abandon_lane(index)));
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
    let prepared = attempt(|| {
        let original = affinity::current_cpus()?;
        let lane_scopes: Vec<_> = plan.lanes.iter().map(|_| scope.clone()).collect();
        let helper_scopes: Vec<_> = plan.helpers.iter().map(|_| scope.clone()).collect();
        Ok::<_, S::Error>((original, lane_scopes, helper_scopes))
    });
    let (original, mut lane_scopes, helper_scopes) = match prepared {
        Ok(prepared) => prepared,
        Err(error) => {
            rollback_prepared(plan, factory, control, error);
            return Err(error);
        }
    };
    // Restoration also runs if a scoped thread or factory panics.
    let result = attempt(|| {
        // Clone caller policy before any spawn, so a panicking Clone cannot
        // unwind into scoped joins with helpers still waiting for missing lanes.
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
                            attempt(|| helper_thread(factory, helper, startup, &thread_control)),
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
                            attempt(|| lane_thread(factory, lane, index, startup, &thread_control)),
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
                attempt(|| lane_thread(factory, &plan.lanes[0], 0, first_scope, control)),
            );
            for handle in handles {
                if handle.join().is_err() {
                    control.fail(Error::Io.into());
                }
            }
        });
        control.result()
    });
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
            record(control, attempt(|| scope.check()));
        }
        match poll_operation(&mut operation, &mut cx) {
            Poll::Ready(result) => return result,
            Poll::Pending => thread::park_timeout(IDLE_WAIT),
        }
    }
}

/// Poll one bounded service turn and compute its post-poll idle limit.
fn poll_turn<S: Scope>(
    service: &mut dyn Service<S>,
    cx: &mut Context<'_>,
) -> Result<Duration, S::Error> {
    attempt(|| {
        service.register_driver(cx.waker());
        service.poll_budgeted(cx, WORK_BUDGET)?;
        Ok(service.wait_timeout(IDLE_WAIT).min(IDLE_WAIT))
    })
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
    let mut service = match attempt(|| factory.build_lane(index)) {
        Ok(service) => ServiceOwner::new(service),
        Err(error) => {
            abandon_lane(factory, index, control);
            control.lock().stats.drained += 1;
            return Err(error);
        }
    };
    service.attach_reporter(control);
    let waker = attempt(|| service.waker()).unwrap_or_else(|error| {
        control.fail(error);
        crate::drivers::thread_waker(None)
    });
    let started = attempt(|| drive(service.start(&startup), &startup, control, true, &waker));
    if started.is_ok() {
        control.lock().stats.ready += 1;
        control.changed.notify_all();
        let registration = if control.lock().check_scope {
            subscribe_cancellation(&startup, control)
        } else {
            None
        };
        if let Some(registration) = &registration {
            registration.register(&waker);
        }
        let mut cx = Context::from_waker(&waker);
        while !control.stopping(&startup) {
            match poll_turn(&mut *service, &mut cx) {
                Ok(wait) => {
                    if !wait.is_zero() {
                        thread::park_timeout(wait);
                    }
                }
                Err(error) => {
                    control.fail(error);
                    break;
                }
            }
        }
    } else {
        record(control, started);
    }
    record(control, attempt(|| service.stop_admission()));
    let teardown = teardown_scope(factory, &startup, control);
    let teardown = teardown.as_ref().unwrap_or(&startup);
    record(
        control,
        attempt(|| drive(service.drain(teardown), teardown, control, false, &waker)),
    );
    record(control, attempt(|| service.close()));
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
        match attempt(|| factory.build_helper(index)) {
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
    let indices: Vec<_> = services.iter().map(|(i, _)| *i).collect();
    let mut ready = false;
    let operations = services
        .iter_mut()
        .map(|(index, service)| helper_service(*index, service, factory, &startup, control, &waker))
        .collect();
    drive_helpers(operations, control, &waker, || {
        let mut state = control.lock();
        if !ready && !indices.is_empty() && indices.iter().all(|i| state.helper_ready[*i]) {
            state.stats.ready += 1;
            ready = true;
            control.changed.notify_all();
        }
    });
    control.drained();
    let teardown = teardown_scope(factory, &startup, control);
    let teardown = teardown.as_ref().unwrap_or(&startup);
    let operations = services
        .iter_mut()
        .map(|(_, service)| service.shutdown_helper(teardown, control, &waker))
        .collect();
    drive_helpers(operations, control, &waker, || {});
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
) -> Operation<'a, (), S::Error> {
    Box::pin(async move {
        let started = catch_operation(Box::pin(async {
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
        }))
        .await;
        if started.is_ok() {
            control.lock().helper_ready[index] = true;
            control.changed.notify_all();
        } else {
            record(control, started);
        }
        std::future::poll_fn(|cx| {
            record(
                control,
                attempt(|| {
                    service.register_driver(waker);
                    service.poll_budgeted(cx, 1)
                }),
            );
            if control.lock().closed[index] {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;
        let teardown = teardown_scope(factory, startup, control);
        let teardown = teardown.as_ref().unwrap_or(startup);
        record(control, attempt(|| service.stop_admission()));
        record(
            control,
            diagnose_operation(
                catch_operation(Box::pin(async {
                    service.register_driver(waker);
                    service.drain(teardown).await
                })),
                teardown,
                control,
            )
            .await,
        );
        record(control, attempt(|| service.close()));
        std::future::poll_fn(|cx| {
            record(control, attempt(|| teardown.check()));
            record(
                control,
                attempt(|| {
                    service.register_driver(waker);
                    service.poll_budgeted(cx, 1)
                }),
            );
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
    })
}

/// Subscribe to caller cancellation without letting a policy panic bypass teardown.
fn subscribe_cancellation<S: Scope>(
    scope: &S,
    control: &Control<S::Error>,
) -> Option<crate::environment::CancellationRegistration> {
    match attempt(|| {
        scope
            .cancellation()
            .map(|c| c.subscribe())
            .transpose()
            .map_err(S::Error::from)
    }) {
        Ok(registration) => registration,
        Err(error) => {
            control.fail(error);
            None
        }
    }
}

/// Convert a future's poll panic into an ordinary lifecycle error.
fn poll_operation<E: From<Error>>(
    operation: &mut Operation<'_, (), E>,
    cx: &mut Context<'_>,
) -> Poll<Result<(), E>> {
    catch_unwind(AssertUnwindSafe(|| operation.as_mut().poll(cx)))
        .unwrap_or_else(|_| Poll::Ready(Err(Error::Io.into())))
}

/// Wrap ordinary lifecycle polling without changing the future's cancellation policy.
fn catch_operation<'a, E: From<Error> + 'a>(
    mut operation: Operation<'a, (), E>,
) -> Operation<'a, (), E> {
    Box::pin(std::future::poll_fn(move |cx| {
        poll_operation(&mut operation, cx)
    }))
}

/// Drive a lane fence, including fail-closed handling of construction panics.
fn drive_fence<S: Scope>(
    service: &mut dyn Service<S>,
    scope: &S,
    control: &Control<S::Error>,
    waker: &Waker,
) {
    require_fence(
        control,
        attempt(|| {
            drive(
                fence_operation(service.fence(scope), control),
                scope,
                control,
                false,
                waker,
            )
        }),
    );
}

/// Abort immediately if a fence future fails or panics while being polled.
fn fence_operation<'a, E: Copy + From<Error> + 'a>(
    mut operation: Operation<'a, (), E>,
    control: &'a Control<E>,
) -> Operation<'a, (), E> {
    Box::pin(std::future::poll_fn(move |cx| {
        match poll_operation(&mut operation, cx) {
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
        record(control, attempt(|| scope.check()));
        operation.as_mut().poll(cx)
    }))
}

/// Rotate helper polling fairly and retain every future through its final fence.
fn drive_helpers<E: Copy + From<Error>>(
    operations: Vec<Operation<'_, (), E>>,
    control: &Control<E>,
    waker: &Waker,
    mut after_pass: impl FnMut(),
) {
    let mut operations: Vec<_> = operations.into_iter().map(Some).collect();
    let mut remaining = operations.len();
    let mut first = 0;
    let mut passes = 0;
    let mut cx = Context::from_waker(waker);
    while remaining != 0 {
        for offset in 0..operations.len() {
            let index = (first + offset) % operations.len();
            if let Some(operation) = &mut operations[index] {
                let polled = catch_unwind(AssertUnwindSafe(|| operation.as_mut().poll(&mut cx)))
                    .unwrap_or_else(|_| {
                        control.fail(Error::Io.into());
                        std::process::abort()
                    });
                if let Poll::Ready(result) = polled {
                    require_fence(control, result);
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

    /// Compute wait policy after polling and translate policy panics into I/O errors.
    #[test]
    fn steady_state_wait_is_post_poll_bounded_and_panic_safe() {
        /// Service that exposes whether polling preceded its wait-policy callback.
        struct Waiting {
            polled: bool,

            wait: Duration,

            panic: bool,
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

            /// Assert post-poll ordering and optionally inject a wait-policy panic.
            fn wait_timeout(&self, maximum: Duration) -> Duration {
                assert!(self.polled, "wait policy must observe the completed poll");
                assert_eq!(maximum, IDLE_WAIT);
                assert!(!self.panic, "wait hook failure");
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
                panic: false,
            };
            assert_eq!(poll_turn(&mut service, &mut cx), Ok(wait.min(IDLE_WAIT)));
        }
        let mut service = Waiting {
            polled: false,
            wait: IDLE_WAIT,
            panic: true,
        };
        assert_eq!(poll_turn(&mut service, &mut cx), Err(Error::Io));
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
                panic_poll: false,
                stop: Arc::default(),
            }));
            let control = Control::new(1, usize::from(helper), true);
            let waker = crate::drivers::thread_waker(None);
            assert_eq!(owner.stage, ReleaseStage::Live);
            if helper {
                drive_helpers(
                    vec![Box::pin(owner.fence_helper(&TestScope, &control, &waker))],
                    &control,
                    &waker,
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
                    Box::pin(async move {
                        assert!(!(fail && self.mode.contains("panic")), "fence poll");
                        if fail { Err(Error::Io) } else { Ok(()) }
                    })
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
                    let result = attempt(|| {
                        owner.release();
                        Ok::<_, Error>(())
                    });
                    assert_eq!(result, Err(Error::Io));
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
                        0, &mut owner, &factory, &TestScope, &control, &waker,
                    )],
                    &control,
                    &waker,
                    || {},
                );
                // Exercise the post-shutdown helper fence as a separate pass.
                drive_helpers(
                    vec![owner.shutdown_helper(&TestScope, &control, &waker)],
                    &control,
                    &waker,
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
            "lane-panic",
            "lane-construct",
            "lane-second-error",
            "lane-second-panic",
            "lane-second-construct",
            "helper-error",
            "helper-panic",
            "helper-construct",
            "helper-second-error",
            "helper-second-panic",
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

    /// Panicking startup-policy clones roll back every unconstructed lane.
    #[test]
    fn failed_scope_clone_rolls_back_all_unstarted_lanes() {
        use std::sync::atomic::AtomicUsize;

        /// Policy that fails on a configured clone attempt.
        struct CloneScope {
            clones: Arc<AtomicUsize>,

            fail: usize,
        }

        impl Clone for CloneScope {
            /// Count this clone and panic at the configured preparation boundary.
            fn clone(&self) -> Self {
                assert_ne!(
                    self.clones.fetch_add(1, Ordering::SeqCst),
                    self.fail,
                    "clone failure"
                );
                Self {
                    clones: self.clones.clone(),
                    fail: self.fail,
                }
            }
        }

        impl Scope for CloneScope {
            /// Use portable runtime failures for rollback assertions.
            type Error = Error;

            /// Keep scope checks successful so only cloning fails.
            fn check(&self) -> Result<()> {
                Ok(())
            }
        }

        /// Factory that counts abandoned lanes and rejects unexpected construction.
        struct NeverBuilt(AtomicUsize);

        impl Factory<CloneScope> for NeverBuilt {
            /// Fail the test if rollback mistakenly reaches service construction.
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<CloneScope>>> {
                panic!("must not build")
            }

            /// Count each preallocated lane fenced during rollback.
            fn abandon_lane(&self, _: usize) -> Result<()> {
                self.0.fetch_add(1, Ordering::SeqCst);
                Ok(())
            }
        }
        for fail in [0, 1] {
            let factory = Arc::new(NeverBuilt(AtomicUsize::new(0)));
            let mut group = Group::new(plan(2));
            let scope = CloneScope {
                clones: Arc::new(AtomicUsize::new(0)),
                fail,
            };
            assert_eq!(group.start(factory.clone(), &scope), Err(Error::Io));
            assert_eq!(group.join(), Err(Error::Io));
            assert_eq!(group.stats().done, group.stats().total);
            assert_eq!(group.stats().drained, group.stats().total);
            assert_eq!(factory.0.load(Ordering::SeqCst), 1);
            assert!(group.control.lock().fenced[0]);
        }
    }

    /// A panicking teardown-policy factory uses the borrowed startup fallback.
    #[test]
    fn teardown_scope_panic_is_reported_without_cloning_fallback() {
        /// Ordinary service factory with an intentionally panicking teardown policy.
        struct PanickingFactory(Recipe);

        impl Factory<TestScope> for PanickingFactory {
            /// Delegate pinned construction to the lifecycle-recording factory.
            fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<TestScope>>> {
                self.0.build_lane(lane)
            }

            /// Inject a failure while obtaining fresh teardown policy.
            fn teardown_scope(&self, _: &TestScope) -> TestScope {
                panic!("teardown policy");
            }
        }
        let plan = plan(2);
        let factory = Arc::new(PanickingFactory(recipe(&plan, false)));
        let mut group = Group::new(plan);
        group.start(factory.clone(), &TestScope).unwrap();
        assert_eq!(group.join(), Err(Error::Io));
        assert_eq!(
            *factory.0.events.lock().unwrap(),
            [
                "start", "stop", "drain", "close", "fence", "shutdown", "fence", "drop"
            ]
        );
    }

    /// Cancellation subscription panics still drive constructed services through teardown.
    #[test]
    fn cancellation_hook_panics_still_teardown_lane_and_helper() {
        /// Scope with successful checks but an intentionally panicking cancellation hook.
        #[derive(Clone)]
        struct PanicScope;

        impl Scope for PanicScope {
            /// Use runtime failures to inspect panic translation.
            type Error = Error;

            /// Keep ordinary policy checks successful.
            fn check(&self) -> Result<()> {
                Ok(())
            }

            /// Inject failure while accessing caller cancellation policy.
            fn cancellation(&self) -> Option<&crate::environment::Cancellation> {
                panic!("cancellation hook");
            }
        }

        /// Resource-free service and factory used to isolate scope-hook failures.
        struct Empty;

        impl Service<PanicScope> for Empty {
            /// Finish startup immediately when construction reaches the future.
            fn start<'a>(&'a mut self, _: &'a PanicScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Perform no work while the group observes the scope failure.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                Ok(())
            }

            /// Complete the resource-free drain despite failed cancellation hooks.
            fn drain<'a>(&'a mut self, _: &'a PanicScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Complete shutdown without consulting cancellation policy.
            fn shutdown<'a>(&'a mut self, _: &'a PanicScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }

        impl Factory<PanicScope> for Empty {
            /// Construct a resource-free lane on its pinned owner.
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<PanicScope>>> {
                Ok(Box::new(Empty))
            }

            /// Construct a resource-free helper on its pinned owner.
            fn build_helper(&self, _: usize) -> Result<Box<dyn Service<PanicScope>>> {
                Ok(Box::new(Empty))
            }
        }
        for helper in [false, true] {
            let mut plan = plan(2);
            if helper {
                plan.helpers.push(Helper {
                    name: "panic-scope-helper".into(),
                    cpu: plan.lanes[0].cpu,
                    lanes: vec![0],
                });
            }
            let mut group = Group::new(plan);
            assert_eq!(group.run_with_scope(&Empty, &PanicScope), Err(Error::Io));
            assert_eq!(group.stats().done, group.stats().total);
            assert!(group.control.lock().fenced[0]);
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
            || {},
        );
        assert_eq!(polls.get(), 4);
        assert_eq!(control.result(), Err(Error::DeadlineExceeded));
    }

    /// A second cancellation-hook panic stops admission but preserves both lane fences.
    #[test]
    fn steady_state_cancellation_hook_panic_fences_constructed_service() {
        use std::sync::atomic::AtomicUsize;

        /// Policy that permits the startup hook and panics on its next invocation.
        #[derive(Clone)]
        struct SecondHook(Arc<AtomicUsize>);

        impl Scope for SecondHook {
            /// Use runtime errors to inspect the translated hook panic.
            type Error = Error;

            /// Keep admission policy checks successful until the hook fails.
            fn check(&self) -> Result<()> {
                Ok(())
            }

            /// Count hooks and inject failure only after startup's subscription attempt.
            fn cancellation(&self) -> Option<&crate::environment::Cancellation> {
                assert_eq!(
                    self.0.fetch_add(1, Ordering::SeqCst),
                    0,
                    "second cancellation hook"
                );
                None
            }
        }

        /// Service and factory that record both ownership fences after admission stops.
        struct Fenced(Arc<AtomicUsize>);

        impl Service<SecondHook> for Fenced {
            /// Complete startup before the second cancellation hook runs.
            fn start<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Reject any steady-state work after the cancellation hook has failed.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                panic!("admission must stop");
            }

            /// Complete drain without adding unrelated failures.
            fn drain<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }

            /// Count each required ownership fence.
            fn fence<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                self.0.fetch_add(1, Ordering::SeqCst);
                Box::pin(async { Ok(()) })
            }

            /// Complete shutdown before the second ownership fence.
            fn shutdown<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }

        impl Factory<SecondHook> for Fenced {
            /// Construct a local service sharing the fence instrumentation.
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<SecondHook>>> {
                Ok(Box::new(Fenced(self.0.clone())))
            }
        }
        let fences = Arc::new(AtomicUsize::new(0));
        let hooks = Arc::new(AtomicUsize::new(0));
        let mut group = Group::new(plan(1));
        assert_eq!(
            group.run_with_scope(&Fenced(fences.clone()), &SecondHook(hooks.clone())),
            Err(Error::Io)
        );
        assert_eq!(hooks.load(Ordering::SeqCst), 2);
        assert_eq!(fences.load(Ordering::SeqCst), 2);
        assert_eq!(group.stats().done, 1);
    }

    /// Caller scope checks run outside coordinator locks and convert panics to errors.
    #[test]
    fn wait_scope_panic_is_an_error_and_policy_runs_outside_control_lock() {
        /// Scope that reenters coordinator diagnostics before panicking.
        #[derive(Clone)]
        struct Reenter(Arc<Control<Error>>);

        impl Scope for Reenter {
            /// Use portable errors for the converted policy panic.
            type Error = Error;

            /// Reacquire coordinator state to prove policy runs outside its lock.
            fn check(&self) -> Result<()> {
                assert_eq!(self.0.lock().stats.total, 1);
                panic!("scope check");
            }
        }
        let control = Arc::new(Control::new(1, 0, false));
        assert_eq!(
            control.wait_for(&Reenter(control.clone()), |_| false),
            Err(Error::Io)
        );
        assert!(!control.state.is_poisoned());
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

        panic_poll: bool,

        stop: Arc<AtomicBool>,
    }

    /// Non-Send service that records lifecycle order and verifies owner-thread destruction.
    struct Local {
        events: Arc<Mutex<Vec<&'static str>>>,

        owner: Rc<thread::ThreadId>,

        panic_poll: bool,

        stop: Arc<AtomicBool>,
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
                panic_poll: self.panic_poll,
                stop: self.stop.clone(),
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

        /// Verify bounded polling and inject either a panic or explicit stop request.
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
            assert_eq!(budget, 64);
            assert!(!self.panic_poll, "injected local poll panic");
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
                Ok(())
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
    fn recipe(plan: &Plan, panic_poll: bool) -> Recipe {
        Recipe {
            cpu: plan.lanes[0].cpu,
            events: Arc::default(),
            panic_poll,
            stop: Arc::default(),
        }
    }

    /// Borrowed execution tears down a panicking local service and restores caller affinity.
    #[test]
    fn borrowed_local_panic_drains_fences_and_restores_affinity() {
        let before = affinity::current_cpus().unwrap();
        let plan = plan(1);
        let factory = recipe(&plan, true);
        let mut group = Group::new(plan);
        assert_eq!(group.run(&factory, &TestScope), Err(Error::Io));
        assert_eq!(affinity::current_cpus().unwrap(), before);
        assert_eq!(group.stats().done, 1);
        assert_eq!(
            *factory.events.lock().unwrap(),
            [
                "start", "stop", "drain", "close", "fence", "shutdown", "fence", "drop"
            ]
        );
    }

    /// Dropping an owned group joins its lane after both fences and local destruction.
    #[test]
    fn owned_drop_joins_local_service_without_helpers() {
        let plan = plan(2);
        let factory = Arc::new(recipe(&plan, false));
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

    /// Helper startup and drain failures cannot skip polling through the lane's fence.
    #[test]
    fn helper_start_and_drain_panics_still_poll_until_lane_fence() {
        /// Helper that counts polls while injecting lifecycle future panics.
        struct PanickingHelper(Rc<std::cell::Cell<usize>>);

        impl Service<TestScope> for PanickingHelper {
            /// Inject a panic when the helper startup future is polled.
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { panic!("injected helper start panic") })
            }

            /// Count one cooperative turn while checking the single-shard budget.
            fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
                assert_eq!(budget, 1);
                self.0.set(self.0.get() + 1);
                Ok(())
            }

            /// Inject a panic when the helper drain future is polled.
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { panic!("injected helper drain panic") })
            }

            /// Finish shutdown without introducing another failure.
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }
        let plan = plan(2);
        let factory = recipe(&plan, false);
        let polls = Rc::new(std::cell::Cell::new(0));
        let mut service = ServiceOwner::new(Box::new(PanickingHelper(polls.clone())));
        let control = Control::new(1, 1, true);
        let waker = crate::drivers::thread_waker(None);
        let mut cx = Context::from_waker(&waker);
        {
            let mut operation =
                helper_service(0, &mut service, &factory, &TestScope, &control, &waker);
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            assert_eq!(control.result(), Err(Error::Io));
            control.close(0);
            assert!(operation.as_mut().poll(&mut cx).is_pending());
            control.fence(0);
            assert_eq!(operation.as_mut().poll(&mut cx), Poll::Ready(Ok(())));
        }
        assert!(
            polls.get() >= 4,
            "helper keeps driving through both panics and the lane fence"
        );
        drive_helpers(
            vec![service.shutdown_helper(&TestScope, &control, &waker)],
            &control,
            &waker,
            || {},
        );
        service.release();
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
