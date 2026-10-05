//! Pinned local services with explicit startup, drain, and ownership fences.
//!
//! Fail closed: an unsuccessful ownership fence or an unexpected unwind of a
//! live service aborts the process. Neither an error nor thread exit proves that
//! external I/O stopped referencing storage. We never detach or drop such owners.
use crate::{Error, Operation, Result, Scope, affinity};
use std::{
    panic::{AssertUnwindSafe, catch_unwind},
    sync::{Arc, Condvar, Mutex, MutexGuard},
    task::{Context, Poll, Waker},
    thread::{self, JoinHandle},
    time::Duration,
};

const IDLE_WAIT: Duration = Duration::from_millis(1);
const WORK_BUDGET: usize = 64;

#[derive(Clone)]
pub struct Lane {
    pub name: String,
    pub cpu: usize,
}
#[derive(Clone)]
pub struct Helper {
    pub name: String,
    pub cpu: usize,
    /// Lane indices in `Plan::lanes`. Each lane has at most one helper service.
    pub lanes: Vec<usize>,
}
#[derive(Clone)]
pub struct Plan {
    pub lanes: Vec<Lane>,
    pub helpers: Vec<Helper>,
    /// Whole-process execution budget including the external caller of `start`.
    /// `run` instead uses its caller as lane zero and consumes no extra slot.
    pub max_threads: usize,
}

/// Builds run on the pinned owner. Local services and futures need not be Send.
/// A helper builds one service per associated lane and drives them cooperatively.
pub trait Factory<S: Scope>: Sync {
    fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<S>>, S::Error>;
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
    fn waker(&self) -> Result<Waker, S::Error> {
        Ok(crate::thread_waker(None))
    }
    fn register_driver(&self, _waker: &Waker) {}
    fn start<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error>;
    fn poll_budgeted(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<(), S::Error>;
    /// Bound the next steady-state wait after polling. Due work returns zero.
    fn wait_timeout(&self, maximum: Duration) -> Duration {
        maximum
    }
    fn stop_admission(&mut self) -> Result<(), S::Error> {
        Ok(())
    }
    fn drain<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error>;
    /// Close helper submissions after lane drain has stopped every producer.
    fn close(&mut self) -> Result<(), S::Error> {
        Ok(())
    }
    fn fence<'a>(&'a mut self, _scope: &'a S) -> Operation<'a, (), S::Error> {
        Box::pin(async { Ok(()) })
    }
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> Operation<'a, (), S::Error>;
}

/// Error-only group capability: cannot advance phases or authorize resource release.
#[derive(Clone)]
pub struct FailureReporter<E: Copy> {
    control: Arc<Control<E>>,
}
impl<E: Copy> FailureReporter<E> {
    pub fn report(&self, error: E) {
        self.control.fail(error);
    }
}

/// Read-only lifecycle diagnostics for the current execution, not synchronization
/// authority. Counts describe OS execution threads, not helper service shards.
/// Use the lifecycle methods to wait for completion rather than polling counts.
#[derive(Clone, Copy, Default)]
pub struct Stats {
    pub total: usize,
    pub ready: usize,
    pub drained: usize,
    pub done: usize,
    /// Owned coordinator handles awaiting join (zero or one), even after exit.
    pub coordinators: usize,
}

/// Explicitly driven thread ownership. Dropping a group requests shutdown and
/// joins all threads; it never detaches resources still owned by a service.
pub struct Group<S: Scope + Send> {
    plan: Plan,
    control: Arc<Control<S::Error>>,
    threads: Vec<JoinHandle<()>>,
}
impl<S: Scope + Send> Group<S> {
    pub fn new(plan: Plan) -> Self {
        Self {
            plan,
            control: Arc::new(Control::new(0, 0, false)),
            threads: Vec::new(),
        }
    }
    /// Snapshot lifecycle progress without exposing mutable coordinator state.
    pub fn stats(&self) -> Stats {
        let mut stats = self.control.lock().stats;
        stats.coordinators = self.threads.len();
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
        if !self.threads.is_empty() || self.plan.lanes.is_empty() || count > self.plan.max_threads {
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
        self.threads.push(handle);
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
        for handle in self.threads.drain(..) {
            if handle.join().is_err() {
                self.control.fail(Error::Io.into());
            }
        }
        self.control.result()
    }
}
impl<S: Scope + Send> Drop for Group<S> {
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
struct Control<E> {
    state: Mutex<State<E>>,
    changed: Condvar,
}
impl<E: Copy> Control<E> {
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
    fn lock(&self) -> MutexGuard<'_, State<E>> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }
    fn result(&self) -> Result<(), E> {
        self.lock().error.map_or(Ok(()), Err)
    }
    fn set_phase(&self, phase: Phase) {
        let mut state = self.lock();
        state.phase = state.phase.max(phase);
        self.changed.notify_all();
    }
    fn fail(&self, error: E) {
        let mut state = self.lock();
        state.error.get_or_insert(error);
        state.phase = state.phase.max(Phase::Drain);
        self.changed.notify_all();
    }
    fn close(&self, lane: usize) {
        self.lock().closed[lane] = true;
        self.changed.notify_all();
    }
    fn fence(&self, lane: usize) {
        self.lock().fenced[lane] = true;
        self.changed.notify_all();
    }
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
    fn drained(&self) {
        let mut state = self.lock();
        state.stats.drained += 1;
        self.changed.notify_all();
        while state.phase != Phase::Shutdown && !state.auto_shutdown && state.error.is_none() {
            state = self.changed.wait(state).unwrap_or_else(|e| e.into_inner());
        }
    }
}
struct Exit<E: Copy + From<Error>> {
    control: Arc<Control<E>>,
    lane: Option<usize>,
}
impl<E: Copy + From<Error>> Drop for Exit<E> {
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
fn attempt<T, E: From<Error>>(f: impl FnOnce() -> Result<T, E>) -> Result<T, E> {
    catch_unwind(AssertUnwindSafe(f)).unwrap_or_else(|_| Err(Error::Io.into()))
}
fn record<E: Copy>(control: &Control<E>, result: Result<(), E>) {
    if let Err(error) = result {
        control.fail(error);
    }
}

fn require_fence<E: Copy>(control: &Control<E>, result: Result<(), E>) {
    if let Err(error) = result {
        control.fail(error);
        // Returning would destroy resources whose ownership is still external.
        std::process::abort();
    }
}

struct ServiceOwner<S: Scope> {
    service: std::mem::ManuallyDrop<Box<dyn Service<S>>>,
    releasable: bool,
}
impl<S: Scope> ServiceOwner<S> {
    fn new(service: Box<dyn Service<S>>) -> Self {
        Self {
            service: std::mem::ManuallyDrop::new(service),
            releasable: false,
        }
    }
}
impl<S: Scope> std::ops::Deref for ServiceOwner<S> {
    type Target = dyn Service<S>;
    fn deref(&self) -> &Self::Target {
        &**self.service
    }
}
impl<S: Scope> std::ops::DerefMut for ServiceOwner<S> {
    fn deref_mut(&mut self) -> &mut Self::Target {
        &mut **self.service
    }
}
impl<S: Scope> Drop for ServiceOwner<S> {
    fn drop(&mut self) {
        if !self.releasable {
            std::process::abort();
        }
        // Both lifecycle fences succeeded. This is the only release path.
        unsafe {
            std::mem::ManuallyDrop::drop(&mut self.service);
        }
    }
}

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

fn abandon_lane<S: Scope>(factory: &dyn Factory<S>, index: usize, control: &Control<S::Error>) {
    require_fence(control, attempt(|| factory.abandon_lane(index)));
    control.close(index);
    control.fence(index);
}

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
        match catch_unwind(AssertUnwindSafe(|| operation.as_mut().poll(&mut cx))) {
            Ok(Poll::Ready(result)) => return result,
            Err(_) => return Err(Error::Io.into()),
            Ok(Poll::Pending) => thread::park_timeout(IDLE_WAIT),
        }
    }
}
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
    record(
        control,
        attempt(|| {
            service.set_failure_reporter(FailureReporter {
                control: control.clone(),
            });
            Ok(())
        }),
    );
    let waker = attempt(|| service.waker()).unwrap_or_else(|error| {
        control.fail(error);
        crate::thread_waker(None)
    });
    let started = attempt(|| drive(service.start(&startup), &startup, control, true, &waker));
    if started.is_ok() {
        control.lock().stats.ready += 1;
        control.changed.notify_all();
        let registration = if control.lock().check_scope {
            match attempt(|| {
                startup
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
    require_fence(
        control,
        attempt(|| {
            drive(
                fence_operation(service.fence(teardown), control),
                teardown,
                control,
                false,
                &waker,
            )
        }),
    );
    control.fence(index);
    control.drained();
    record(
        control,
        attempt(|| drive(service.shutdown(teardown), teardown, control, false, &waker)),
    );
    require_fence(
        control,
        attempt(|| {
            drive(
                fence_operation(service.fence(teardown), control),
                teardown,
                control,
                false,
                &waker,
            )
        }),
    );
    service.releasable = true;
    Ok(())
}
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
    let waker = crate::thread_waker(None);
    let mut services = Vec::new();
    for &index in &helper.lanes {
        match attempt(|| factory.build_helper(index)) {
            Ok(service) => {
                let mut service = ServiceOwner::new(service);
                record(
                    control,
                    attempt(|| {
                        service.set_failure_reporter(FailureReporter {
                            control: control.clone(),
                        });
                        Ok(())
                    }),
                );
                services.push((index, service));
            }
            Err(error) => {
                control.fail(error);
                break;
            }
        }
    }
    let registration = match attempt(|| {
        startup
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
    };
    if let Some(registration) = &registration {
        registration.register(&waker);
    }
    let indices: Vec<_> = services.iter().map(|(i, _)| *i).collect();
    let mut ready = false;
    let operations = services
        .iter_mut()
        .map(|(index, service)| {
            helper_service(*index, &mut **service, factory, &startup, control, &waker)
        })
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
        .map(|(_, service)| -> Operation<'_, (), S::Error> {
            diagnose_operation(
                Box::pin(async {
                    record(
                        control,
                        catch_operation(Box::pin(async {
                            service.register_driver(&waker);
                            service.shutdown(teardown).await
                        }))
                        .await,
                    );
                    service.register_driver(&waker);
                    fence_operation(service.fence(teardown), control).await
                }),
                teardown,
                control,
            )
        })
        .collect();
    drive_helpers(operations, control, &waker, || {});
    for (_, service) in &mut services {
        service.releasable = true;
    }
    Ok(())
}
fn helper_service<'a, S: Scope>(
    index: usize,
    service: &'a mut dyn Service<S>,
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
        diagnose_operation(
            fence_operation(
                Box::pin(async {
                    service.register_driver(waker);
                    service.fence(teardown).await
                }),
                control,
            ),
            teardown,
            control,
        )
        .await?;
        Ok(())
    })
}
fn catch_operation<'a, E: From<Error> + 'a>(
    mut operation: Operation<'a, (), E>,
) -> Operation<'a, (), E> {
    Box::pin(std::future::poll_fn(move |cx| {
        catch_unwind(AssertUnwindSafe(|| operation.as_mut().poll(cx)))
            .unwrap_or_else(|_| Poll::Ready(Err(Error::Io.into())))
    }))
}
fn fence_operation<'a, E: Copy + From<Error> + 'a>(
    mut operation: Operation<'a, (), E>,
    control: &'a Control<E>,
) -> Operation<'a, (), E> {
    Box::pin(std::future::poll_fn(move |cx| {
        match catch_unwind(AssertUnwindSafe(|| operation.as_mut().poll(cx))) {
            Ok(Poll::Pending) => Poll::Pending,
            Ok(Poll::Ready(result)) => {
                require_fence(control, result);
                Poll::Ready(Ok(()))
            }
            Err(_) => {
                require_fence(control, Err(Error::Io.into()));
                unreachable!()
            }
        }
    }))
}
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

#[cfg(test)]
mod tests {
    use super::*;
    use std::{
        collections::BTreeSet,
        rc::Rc,
        sync::atomic::{AtomicBool, Ordering},
    };

    #[derive(Clone)]
    struct TestScope;
    impl Scope for TestScope {
        type Error = Error;
        fn check(&self) -> Result<()> {
            Ok(())
        }
    }

    #[test]
    fn steady_state_wait_is_post_poll_bounded_and_panic_safe() {
        struct Waiting {
            polled: bool,
            wait: Duration,
            panic: bool,
        }
        impl Service<TestScope> for Waiting {
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
                assert_eq!(budget, WORK_BUDGET);
                self.polled = true;
                Ok(())
            }
            fn wait_timeout(&self, maximum: Duration) -> Duration {
                assert!(self.polled, "wait policy must observe the completed poll");
                assert_eq!(maximum, IDLE_WAIT);
                assert!(!self.panic, "wait hook failure");
                self.wait
            }
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
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
            struct Fatal {
                mode: String,
                fences: usize,
            }
            impl Drop for Fatal {
                fn drop(&mut self) {
                    std::process::exit(99);
                }
            }
            impl Service<TestScope> for Fatal {
                fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }
                fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                    Ok(())
                }
                fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }
                fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    Box::pin(async { Ok(()) })
                }
                fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                    self.fences += 1;
                    let fail = self.fences == if self.mode.contains("second") { 2 } else { 1 };
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
            struct FatalFactory(String);
            impl Factory<TestScope> for FatalFactory {
                fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
                    Ok(Box::new(Fatal {
                        mode: self.0.clone(),
                        fences: 0,
                    }))
                }
            }
            if mode.starts_with("helper") {
                let control = Control::new(1, 1, true);
                control.close(0);
                control.fence(0);
                let factory = FatalFactory(mode.clone());
                let mut owner = ServiceOwner::new(factory.build_lane(0).unwrap());
                let waker = crate::thread_waker(None);
                drive_helpers(
                    vec![helper_service(
                        0,
                        &mut *owner,
                        &factory,
                        &TestScope,
                        &control,
                        &waker,
                    )],
                    &control,
                    &waker,
                    || {},
                );
                // Exercise the post-shutdown helper fence as a separate pass.
                drive_helpers(
                    vec![Box::pin(async {
                        owner.shutdown(&TestScope).await?;
                        fence_operation(owner.fence(&TestScope), &control).await
                    })],
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

    #[test]
    fn failed_scope_clone_rolls_back_all_unstarted_lanes() {
        use std::sync::atomic::AtomicUsize;
        struct CloneScope {
            clones: Arc<AtomicUsize>,
            fail: usize,
        }
        impl Clone for CloneScope {
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
            type Error = Error;
            fn check(&self) -> Result<()> {
                Ok(())
            }
        }
        struct NeverBuilt(AtomicUsize);
        impl Factory<CloneScope> for NeverBuilt {
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<CloneScope>>> {
                panic!("must not build")
            }
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

    #[test]
    fn teardown_scope_panic_is_reported_without_cloning_fallback() {
        struct PanickingFactory(Recipe);
        impl Factory<TestScope> for PanickingFactory {
            fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<TestScope>>> {
                self.0.build_lane(lane)
            }
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

    #[test]
    fn cancellation_hook_panics_still_teardown_lane_and_helper() {
        #[derive(Clone)]
        struct PanicScope;
        impl Scope for PanicScope {
            type Error = Error;
            fn check(&self) -> Result<()> {
                Ok(())
            }
            fn cancellation(&self) -> Option<&crate::deadline::Cancellation> {
                panic!("cancellation hook");
            }
        }
        struct Empty;
        impl Service<PanicScope> for Empty {
            fn start<'a>(&'a mut self, _: &'a PanicScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a PanicScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn shutdown<'a>(&'a mut self, _: &'a PanicScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }
        impl Factory<PanicScope> for Empty {
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<PanicScope>>> {
                Ok(Box::new(Empty))
            }
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

    #[test]
    fn helper_shutdown_deadline_is_diagnostic_not_detachment() {
        #[derive(Clone)]
        struct Expired;
        impl Scope for Expired {
            type Error = Error;
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
        let waker = crate::thread_waker(None);
        drive_helpers(
            vec![diagnose_operation(operation, &Expired, &control)],
            &control,
            &waker,
            || {},
        );
        assert_eq!(polls.get(), 4);
        assert_eq!(control.result(), Err(Error::DeadlineExceeded));
    }

    #[test]
    fn steady_state_cancellation_hook_panic_fences_constructed_service() {
        use std::sync::atomic::AtomicUsize;
        #[derive(Clone)]
        struct SecondHook(Arc<AtomicUsize>);
        impl Scope for SecondHook {
            type Error = Error;
            fn check(&self) -> Result<()> {
                Ok(())
            }
            fn cancellation(&self) -> Option<&crate::deadline::Cancellation> {
                assert_eq!(
                    self.0.fetch_add(1, Ordering::SeqCst),
                    0,
                    "second cancellation hook"
                );
                None
            }
        }
        struct Fenced(Arc<AtomicUsize>);
        impl Service<SecondHook> for Fenced {
            fn start<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                panic!("admission must stop");
            }
            fn drain<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn fence<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                self.0.fetch_add(1, Ordering::SeqCst);
                Box::pin(async { Ok(()) })
            }
            fn shutdown<'a>(&'a mut self, _: &'a SecondHook) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }
        impl Factory<SecondHook> for Fenced {
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

    #[test]
    fn wait_scope_panic_is_an_error_and_policy_runs_outside_control_lock() {
        #[derive(Clone)]
        struct Reenter(Arc<Control<Error>>);
        impl Scope for Reenter {
            type Error = Error;
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
    struct Recipe {
        cpu: usize,
        events: Arc<Mutex<Vec<&'static str>>>,
        panic_poll: bool,
        stop: Arc<AtomicBool>,
    }
    struct Local {
        events: Arc<Mutex<Vec<&'static str>>>,
        owner: Rc<thread::ThreadId>,
        panic_poll: bool,
        stop: Arc<AtomicBool>,
    }
    impl Factory<TestScope> for Recipe {
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
        fn build_helper(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
            unreachable!()
        }
    }
    impl Drop for Local {
        fn drop(&mut self) {
            assert_eq!(*self.owner, thread::current().id());
            self.events.lock().unwrap().push("drop");
        }
    }
    impl Service<TestScope> for Local {
        fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("start");
                Ok(())
            })
        }
        fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
            assert_eq!(budget, 64);
            assert!(!self.panic_poll, "injected local poll panic");
            if self.stop.load(Ordering::SeqCst) {
                Err(Error::Cancelled)
            } else {
                Ok(())
            }
        }
        fn stop_admission(&mut self) -> Result<()> {
            self.events.lock().unwrap().push("stop");
            Ok(())
        }
        fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("drain");
                Ok(())
            })
        }
        fn close(&mut self) -> Result<()> {
            self.events.lock().unwrap().push("close");
            Ok(())
        }
        fn fence<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("fence");
                Ok(())
            })
        }
        fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
            Box::pin(async {
                self.events.lock().unwrap().push("shutdown");
                Ok(())
            })
        }
    }
    fn recipe(plan: &Plan, panic_poll: bool) -> Recipe {
        Recipe {
            cpu: plan.lanes[0].cpu,
            events: Arc::default(),
            panic_poll,
            stop: Arc::default(),
        }
    }
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

    #[test]
    fn helper_start_and_drain_panics_still_poll_until_lane_fence() {
        struct PanickingHelper(usize);
        impl Service<TestScope> for PanickingHelper {
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { panic!("injected helper start panic") })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
                assert_eq!(budget, 1);
                self.0 += 1;
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { panic!("injected helper drain panic") })
            }
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }
        let plan = plan(2);
        let factory = recipe(&plan, false);
        let mut service = PanickingHelper(0);
        let control = Control::new(1, 1, true);
        let waker = crate::thread_waker(None);
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
            service.0 >= 4,
            "helper keeps driving through both panics and the lane fence"
        );
    }

    #[test]
    fn helper_independent_resources_fence_cooperatively_before_barrier_and_drop() {
        use std::sync::atomic::AtomicUsize;
        struct Shared {
            entered: [AtomicUsize; 2],
            completed: [AtomicUsize; 2],
            dropped: AtomicUsize,
        }
        struct Helpers {
            shared: Arc<Shared>,
        }
        struct Independent {
            shared: Arc<Shared>,
            owner: Rc<thread::ThreadId>,
            fences: usize,
        }
        struct Empty;
        impl Factory<TestScope> for Helpers {
            fn build_lane(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
                Ok(Box::new(Empty))
            }
            fn build_helper(&self, _: usize) -> Result<Box<dyn Service<TestScope>>> {
                Ok(Box::new(Independent {
                    shared: self.shared.clone(),
                    owner: Rc::new(thread::current().id()),
                    fences: 0,
                }))
            }
        }
        impl Service<TestScope> for Empty {
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, _: usize) -> Result<()> {
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
        }
        impl Service<TestScope> for Independent {
            fn start<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
            fn poll_budgeted(&mut self, _: &mut Context<'_>, budget: usize) -> Result<()> {
                assert_eq!(budget, 1);
                Ok(())
            }
            fn drain<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async { Ok(()) })
            }
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
            fn shutdown<'a>(&'a mut self, _: &'a TestScope) -> Operation<'a, ()> {
                Box::pin(async move {
                    assert_eq!(self.shared.completed[0].load(Ordering::SeqCst), 2);
                    // A shutdown failure still requires the second ownership fence.
                    Err(Error::Io)
                })
            }
        }
        impl Drop for Independent {
            fn drop(&mut self) {
                assert_eq!(*self.owner, thread::current().id());
                assert_eq!(self.fences, 2);
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
