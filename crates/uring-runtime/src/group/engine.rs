//! The per-thread lifecycle: lane threads, helper threads, and the owner guard
//! that keeps a service alive until both of its ownership fences complete.

use std::future::Future;
use std::mem;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Poll, Waker};
use std::thread;

use super::control::{Control, LaneFence, Phase, attempt, catch};
use super::error::{FatalCause, FatalEvent, GroupError, Role, Stage};
use super::{Factory, FailureReporter, Helper, Lane, LifecycleFuture, Plan, Service, thread_waker};
use crate::affinity::{self, CpuSet};
use crate::{CancelRegistration, Error, ErrorKind, Scope};

type Ctl<S> = Control<<S as Scope>::Error>;

/// A helper's per-shard task; every failure is recorded in the control.
type Task<'a> = Pin<Box<dyn Future<Output = ()> + 'a>>;

/// How far a service has progressed toward safe destruction.
#[derive(Debug, PartialEq, Eq)]
enum Release {
    /// Accepted work may still reference the service.
    Live,
    /// The first fence succeeded.
    Drained,
    /// The second fence succeeded; helpers wait for their siblings.
    ShutdownFenced,
    /// Every required fence finished: dropping is safe.
    Releasable,
    /// A fatal event was handled; the service will be leaked, not dropped.
    Leaked,
}

/// The outcome of a lifecycle future polled under `catch_unwind`.
enum Caught<E> {
    Error(E),
    Panic(String),
}

impl<E> Caught<E> {
    fn into_group(self, role: Role, stage: Stage) -> GroupError<E> {
        match self {
            Self::Error(error) => GroupError::failed(role, stage, error),
            Self::Panic(message) => GroupError::panicked(role, stage, message),
        }
    }
}

/// Owns a service until both ownership fences have completed.
///
/// Dropping the owner before then never drops the service: the fatal policy
/// runs and the service is leaked.
struct Owner<S: Scope> {
    service: Option<Box<dyn Service<S>>>,
    release: Release,
    role: Role,
    control: Arc<Ctl<S>>,
}

impl<S: Scope> Owner<S> {
    fn new(service: Box<dyn Service<S>>, role: Role, control: Arc<Ctl<S>>) -> Self {
        let mut owner = Self {
            service: Some(service),
            release: Release::Live,
            role,
            control,
        };
        let reporter = FailureReporter::new(owner.control.clone(), role);
        let result = attempt(role, Stage::Start, || {
            owner.service().set_failure_reporter(reporter);
            Ok(())
        });
        owner.control.record(result);
        owner
    }

    fn service(&mut self) -> &mut dyn Service<S> {
        self.service.as_deref_mut().expect("service present")
    }

    /// Handle a fatal event: apply the policy and mark the service for leaking.
    fn leak(&mut self, stage: Stage, cause: FatalCause, error: Option<S::Error>) {
        self.release = Release::Leaked;
        let event = FatalEvent {
            role: self.role,
            stage,
            cause,
        };
        self.control.fatal(event, error);
    }
}

impl<S: Scope> Drop for Owner<S> {
    fn drop(&mut self) {
        let Some(service) = self.service.take() else {
            return;
        };
        match self.release {
            Release::Releasable => drop(service),
            Release::Leaked => mem::forget(service),
            _ => {
                let event = FatalEvent {
                    role: self.role,
                    stage: Stage::Exit,
                    cause: FatalCause::Abandoned,
                };
                // Forget first: an Abort policy must not run destructors, and
                // a Custom hook must not observe a half-dropped service.
                mem::forget(service);
                self.control.fatal(event, None);
            }
        }
    }
}

/// Poll a lifecycle future, converting its error or panic.
fn poll_caught<E>(
    future: &mut LifecycleFuture<'_, E>,
    cx: &mut Context<'_>,
) -> Poll<Result<(), Caught<E>>> {
    match catch(|| future.as_mut().poll(cx)) {
        Ok(Poll::Pending) => Poll::Pending,
        Ok(Poll::Ready(result)) => Poll::Ready(result.map_err(Caught::Error)),
        Err(message) => Poll::Ready(Err(Caught::Panic(message))),
    }
}

/// Register `waker` with the scope's cancellation token, if any.
fn subscribe<S: Scope>(
    scope: &S,
    control: &Ctl<S>,
    role: Role,
    stage: Stage,
    waker: &Waker,
) -> Option<CancelRegistration> {
    match catch(|| scope.cancellation().map(|token| token.register(waker))) {
        Ok(registration) => registration,
        Err(message) => {
            control.fail(GroupError::panicked(role, stage, message));
            None
        }
    }
}

/// Drive a non-fence lifecycle future on a lane thread to completion.
///
/// Startup stops early when the scope fails or the group leaves `Running`.
/// Teardown steps never stop early: a failing scope is recorded as a
/// diagnosis, and the future keeps being polled.
fn drive<S: Scope>(
    mut future: LifecycleFuture<'_, S::Error>,
    scope: &S,
    control: &Ctl<S>,
    waker: &Waker,
    role: Role,
    stage: Stage,
) -> Result<(), GroupError<S::Error>> {
    let startup = stage == Stage::Start;
    let _registration = startup.then(|| subscribe(scope, control, role, stage, waker));
    let mut cx = Context::from_waker(waker);
    loop {
        if startup {
            attempt(role, stage, || scope.check())?;
            if control.phase() != Phase::Running {
                let error = Error::new(ErrorKind::Canceled).with_context("group stopping");
                return Err(GroupError::failed(role, stage, error.into()));
            }
        } else {
            control.record(attempt(role, stage, || scope.check()));
        }
        match poll_caught(&mut future, &mut cx) {
            Poll::Ready(result) => return result.map_err(|c| c.into_group(role, stage)),
            Poll::Pending => thread::park_timeout(control.config.idle_wait),
        }
    }
}

/// The outcome of a fence.
enum FenceOutcome<E> {
    Confirmed,
    Failed(E),
    Panicked(String),
}

/// Apply a fence outcome to its owner. Returns whether the fence was
/// confirmed; if not, the service has been marked for leaking.
fn settle<S: Scope>(owner: &mut Owner<S>, outcome: FenceOutcome<S::Error>) -> bool {
    match outcome {
        FenceOutcome::Confirmed => true,
        FenceOutcome::Failed(error) => {
            owner.leak(Stage::Fence, FatalCause::FenceFailed, Some(error));
            false
        }
        FenceOutcome::Panicked(message) => {
            owner.leak(Stage::Fence, FatalCause::FencePanicked(message), None);
            false
        }
    }
}

/// Poll a fence future, forgetting it (never dropping it) if it panics.
fn poll_fence<E>(
    future: &mut Option<LifecycleFuture<'_, E>>,
    cx: &mut Context<'_>,
) -> Poll<FenceOutcome<E>> {
    let Some(inner) = future.as_mut() else {
        return Poll::Ready(FenceOutcome::Panicked(
            "fence polled after completion".into(),
        ));
    };
    match catch(|| inner.as_mut().poll(cx)) {
        Ok(Poll::Pending) => Poll::Pending,
        Ok(Poll::Ready(Ok(()))) => Poll::Ready(FenceOutcome::Confirmed),
        Ok(Poll::Ready(Err(error))) => Poll::Ready(FenceOutcome::Failed(error)),
        Err(message) => {
            // The future's state is unknown; never run its destructor.
            mem::forget(future.take());
            Poll::Ready(FenceOutcome::Panicked(message))
        }
    }
}

/// Build a fence future, converting a panic.
fn build_fence<'a, S: Scope>(
    service: &'a mut dyn Service<S>,
    scope: &'a S,
    waker: &Waker,
) -> Result<Option<LifecycleFuture<'a, S::Error>>, String> {
    catch(move || {
        let service = service;
        service.register_driver(waker);
        Some(service.fence(scope))
    })
}

/// Drive a fence on a lane thread, blocking between polls.
fn fence_lane<S: Scope>(owner: &mut Owner<S>, scope: &S, waker: &Waker) -> bool {
    let (role, control) = (owner.role, owner.control.clone());
    let outcome = match build_fence(owner.service(), scope, waker) {
        Err(message) => FenceOutcome::Panicked(message),
        Ok(mut future) => {
            let mut cx = Context::from_waker(waker);
            loop {
                control.record(attempt(role, Stage::Fence, || scope.check()));
                match poll_fence(&mut future, &mut cx) {
                    Poll::Ready(outcome) => break outcome,
                    Poll::Pending => thread::park_timeout(control.config.idle_wait),
                }
            }
        }
    };
    settle(owner, outcome)
}

/// Drive a fence cooperatively on a helper thread.
async fn fence_helper<S: Scope>(owner: &mut Owner<S>, scope: &S, waker: &Waker) -> bool {
    let outcome = match build_fence(owner.service(), scope, waker) {
        Err(message) => FenceOutcome::Panicked(message),
        Ok(mut future) => std::future::poll_fn(|cx| poll_fence(&mut future, cx)).await,
    };
    settle(owner, outcome)
}

/// Wrap a lifecycle future so panics become errors and every poll records a
/// failing teardown scope (a diagnosis that never cancels the future).
fn diagnosed<'a, S: Scope>(
    mut future: LifecycleFuture<'a, S::Error>,
    scope: &'a S,
    control: &'a Ctl<S>,
    role: Role,
    stage: Stage,
) -> impl Future<Output = Result<(), GroupError<S::Error>>> + 'a {
    std::future::poll_fn(move |cx| {
        control.record(attempt(role, stage, || scope.check()));
        poll_caught(&mut future, cx).map_err(|c| c.into_group(role, stage))
    })
}

/// Obtain the teardown scope, falling back to the startup scope on panic.
fn teardown_scope<S: Scope, F: Factory<S> + ?Sized>(
    factory: &F,
    startup: &S,
    control: &Ctl<S>,
    role: Role,
) -> Option<S> {
    match catch(|| factory.teardown_scope(startup)) {
        Ok(scope) => Some(scope),
        Err(message) => {
            control.fail(GroupError::panicked(role, Stage::Drain, message));
            None
        }
    }
}

/// Release a lane's preallocated resources when its service is never built.
pub(super) fn abandon_lane<S: Scope, F: Factory<S> + ?Sized>(
    factory: &F,
    lane: usize,
    control: &Ctl<S>,
) {
    let role = Role::Lane(lane);
    match attempt(role, Stage::Abandon, || factory.abandon_lane(lane)) {
        Ok(()) => control.set_fence(lane, LaneFence::Confirmed),
        Err(error) => {
            let (cause, error) = match error {
                GroupError::Failed { error, .. } => (FatalCause::FenceFailed, Some(error)),
                GroupError::Panicked { message, .. } => (FatalCause::FencePanicked(message), None),
                GroupError::Fatal { event, error } => (event.cause, error),
            };
            control.set_fence(lane, LaneFence::Failed);
            let event = FatalEvent {
                role,
                stage: Stage::Abandon,
                cause,
            };
            control.fatal(event, error);
        }
    }
    control.close(lane);
}

/// Record a failure that ends preparation, and account every lane as
/// abandoned and every thread as finished.
pub(super) fn rollback<S: Scope, F: Factory<S> + ?Sized>(
    plan: &Plan,
    factory: &F,
    control: &Ctl<S>,
    error: GroupError<S::Error>,
) {
    control.fail(error);
    for lane in 0..plan.lanes().len() {
        abandon_lane(factory, lane, control);
    }
    control.update(|state| {
        state.stats.done = state.stats.total;
        state.stats.drained = state.stats.total;
    });
}

/// Accounts for thread exit. Exit is never treated as a fence.
struct Exit<'a, E> {
    control: &'a Control<E>,
    role: Role,
    lane: Option<usize>,
}

impl<E> Drop for Exit<'_, E> {
    fn drop(&mut self) {
        let panicking = thread::panicking();
        if let Some(lane) = self.lane {
            self.control.close(lane);
            if panicking {
                // Unblock the helper; the lane's owner reports the leak.
                self.control.set_fence(lane, LaneFence::Failed);
            }
        }
        if panicking {
            let message = "thread panicked outside a lifecycle step".to_owned();
            self.control
                .fail(GroupError::panicked(self.role, Stage::Exit, message));
        }
        self.control.update(|state| state.stats.done += 1);
    }
}

/// Spawn helpers and lanes 1.., run lane 0 on this thread, join everything,
/// and restore this thread's affinity.
pub(super) fn run_prepared<S, F>(
    plan: &Plan,
    factory: &F,
    scope: &S,
    control: &Arc<Ctl<S>>,
) -> Result<(), GroupError<S::Error>>
where
    S: Scope + Send,
    S::Error: Clone + Send,
    F: Factory<S> + ?Sized,
{
    let role = Role::Group;
    let prepared = affinity::current_cpus()
        .map_err(|error| GroupError::failed(role, Stage::Pin, error.into()))
        .and_then(|original| {
            // Clone every scope before spawning so a panicking Clone cannot
            // leave helpers waiting for lanes that never start.
            catch(|| {
                let lanes: Vec<S> = plan.lanes().iter().map(|_| scope.clone()).collect();
                let helpers: Vec<S> = plan.helpers().iter().map(|_| scope.clone()).collect();
                (original, lanes, helpers)
            })
            .map_err(|message| GroupError::panicked(role, Stage::Prepare, message))
        });
    let (original, mut lane_scopes, helper_scopes) = match prepared {
        Ok(prepared) => prepared,
        Err(error) => {
            rollback(plan, factory, control, error.clone());
            return Err(error);
        }
    };
    let first = lane_scopes.remove(0);
    thread::scope(|threads| {
        let mut handles = Vec::new();
        let spawn_failed = |role, error: std::io::Error| {
            let error = Error::from(error).with_context("spawn group thread");
            control.fail(GroupError::failed(role, Stage::Spawn, error.into()));
        };
        for (index, (helper, startup)) in plan.helpers().iter().zip(helper_scopes).enumerate() {
            let spawned = thread::Builder::new()
                .name(helper.name.clone())
                .spawn_scoped(threads, move || {
                    helper_thread(factory, helper, index, startup, control);
                });
            match spawned {
                Ok(handle) => handles.push((
                    Role::Helper {
                        helper: index,
                        lane: None,
                    },
                    handle,
                )),
                Err(error) => {
                    spawn_failed(
                        Role::Helper {
                            helper: index,
                            lane: None,
                        },
                        error,
                    );
                    let missing = plan.helpers().len() - index;
                    control.update(|state| {
                        state.stats.done += missing;
                        state.stats.drained += missing;
                    });
                    break;
                }
            }
        }
        let mut launched = vec![false; plan.lanes().len()];
        for ((index, lane), startup) in plan.lanes().iter().enumerate().skip(1).zip(lane_scopes) {
            let spawned =
                thread::Builder::new()
                    .name(lane.name.clone())
                    .spawn_scoped(threads, move || {
                        lane_thread(factory, lane, index, startup, control);
                    });
            match spawned {
                Ok(handle) => {
                    handles.push((Role::Lane(index), handle));
                    launched[index] = true;
                }
                Err(error) => {
                    spawn_failed(Role::Lane(index), error);
                    break;
                }
            }
        }
        for lane in (1..launched.len()).filter(|lane| !launched[*lane]) {
            abandon_lane(factory, lane, control);
            control.update(|state| {
                state.stats.done += 1;
                state.stats.drained += 1;
            });
        }
        lane_thread(factory, &plan.lanes()[0], 0, first, control);
        for (role, handle) in handles {
            if let Err(payload) = handle.join() {
                let message = super::control::panic_message(payload);
                control.fail(GroupError::panicked(role, Stage::Exit, message));
            }
        }
    });
    if let Err(error) = affinity::set_cpus(&original) {
        control.fail(GroupError::failed(role, Stage::Pin, error.into()));
    }
    control.result()
}

/// Pin the calling thread if the plan asks for it.
fn pin(cpu: Option<usize>) -> crate::Result<()> {
    cpu.map_or(Ok(()), |cpu| affinity::set_cpus(&CpuSet::from_iter([cpu])))
}

/// Run one lane: build, start, poll until asked to stop, then tear down
/// through both fences. Every failure is recorded in `control`.
pub(super) fn lane_thread<S, F>(
    factory: &F,
    lane: &Lane,
    index: usize,
    startup: S,
    control: &Arc<Ctl<S>>,
) where
    S: Scope,
    S::Error: Clone,
    F: Factory<S> + ?Sized,
{
    let role = Role::Lane(index);
    let exit = Exit {
        control,
        role,
        lane: Some(index),
    };
    let result = catch(|| run_lane(factory, lane, index, &startup, control));
    if let Err(message) = result {
        control.fail(GroupError::panicked(role, Stage::Exit, message));
    }
    drop(exit);
}

fn run_lane<S, F>(factory: &F, lane: &Lane, index: usize, startup: &S, control: &Arc<Ctl<S>>)
where
    S: Scope,
    S::Error: Clone,
    F: Factory<S> + ?Sized,
{
    let role = Role::Lane(index);
    let ready = pin(lane.cpu)
        .map_err(|error| GroupError::failed(role, Stage::Pin, error.into()))
        .and_then(|()| control.wait_for(startup, role, |state| state.helper_ready[index]))
        .and_then(|()| attempt(role, Stage::Build, || factory.build_lane(index)));
    let service = match ready {
        Ok(service) => service,
        Err(error) => {
            control.fail(error);
            abandon_lane(factory, index, control);
            control.count_drained();
            return;
        }
    };
    let mut owner = Owner::new(service, role, control.clone());
    let waker = match attempt(role, Stage::Start, || owner.service().waker()) {
        Ok(waker) => waker,
        Err(error) => {
            control.fail(error);
            thread_waker(None)
        }
    };
    let started = attempt(role, Stage::Start, || {
        owner.service().register_driver(&waker);
        Ok(())
    })
    .and_then(|()| {
        let future = attempt(role, Stage::Start, || Ok(owner.service().start(startup)))?;
        drive(future, startup, control, &waker, role, Stage::Start)
    });
    match started {
        Ok(()) => {
            control.update(|state| state.stats.ready += 1);
            let check_scope = control.lock().check_scope;
            let _registration = if check_scope {
                subscribe(startup, control, role, Stage::Run, &waker)
            } else {
                None
            };
            steady_state(&mut owner, startup, control, &waker, role);
        }
        Err(error) => control.fail(error),
    }

    control.record(attempt(role, Stage::StopAdmission, || {
        owner.service().stop_admission()
    }));
    let teardown = teardown_scope(factory, startup, control, role);
    let teardown = teardown.as_ref().unwrap_or(startup);
    let drained = attempt(role, Stage::Drain, || Ok(owner.service().drain(teardown)))
        .and_then(|future| drive(future, teardown, control, &waker, role, Stage::Drain));
    control.record(drained);
    control.record(attempt(role, Stage::Close, || owner.service().close()));
    control.close(index);

    if !fence_lane(&mut owner, teardown, &waker) {
        control.set_fence(index, LaneFence::Failed);
        control.count_drained();
        return;
    }
    owner.release = Release::Drained;
    control.set_fence(index, LaneFence::Confirmed);
    control.drained();

    let shutdown = attempt(role, Stage::Shutdown, || {
        Ok(owner.service().shutdown(teardown))
    })
    .and_then(|future| drive(future, teardown, control, &waker, role, Stage::Shutdown));
    control.record(shutdown);
    if fence_lane(&mut owner, teardown, &waker) {
        owner.release = Release::Releasable;
    }
}

/// Poll a lane's service until the group leaves `Running`.
fn steady_state<S: Scope>(
    owner: &mut Owner<S>,
    scope: &S,
    control: &Ctl<S>,
    waker: &Waker,
    role: Role,
) {
    let config = &control.config;
    let mut cx = Context::from_waker(waker);
    while !control.stopping(scope, role) {
        let turn = attempt(role, Stage::Run, || {
            let service = owner.service();
            service.register_driver(cx.waker());
            service.poll(&mut cx, config.work_budget)?;
            Ok(service.wait_timeout(config.idle_wait).min(config.idle_wait))
        });
        match turn {
            Ok(wait) if wait.is_zero() => {}
            Ok(wait) => thread::park_timeout(wait),
            Err(error) => {
                control.fail(error);
                break;
            }
        }
    }
}

/// Run one helper thread: build a shard per lane and drive all shards
/// cooperatively through startup, steady state and both fences.
fn helper_thread<S, F>(
    factory: &F,
    helper: &Helper,
    index: usize,
    startup: S,
    control: &Arc<Ctl<S>>,
) where
    S: Scope,
    S::Error: Clone,
    F: Factory<S> + ?Sized,
{
    let role = Role::Helper {
        helper: index,
        lane: None,
    };
    let exit = Exit {
        control,
        role,
        lane: None,
    };
    let result = catch(|| run_helper(factory, helper, index, &startup, control));
    if let Err(message) = result {
        control.fail(GroupError::panicked(role, Stage::Exit, message));
    }
    drop(exit);
}

fn run_helper<S, F>(factory: &F, helper: &Helper, index: usize, startup: &S, control: &Arc<Ctl<S>>)
where
    S: Scope,
    S::Error: Clone,
    F: Factory<S> + ?Sized,
{
    let thread_role = Role::Helper {
        helper: index,
        lane: None,
    };
    if let Err(error) = pin(helper.cpu) {
        control.fail(GroupError::failed(thread_role, Stage::Pin, error.into()));
        control.count_drained();
        return;
    }
    let waker = thread_waker(None);
    let mut shards = Vec::new();
    for &lane in &helper.lanes {
        let role = Role::Helper {
            helper: index,
            lane: Some(lane),
        };
        match attempt(role, Stage::Build, || factory.build_helper(lane)) {
            Ok(service) => shards.push((lane, Owner::new(service, role, control.clone()))),
            Err(error) => {
                control.fail(error);
                break;
            }
        }
    }
    let _registration = subscribe(startup, control, thread_role, Stage::Start, &waker);
    let lanes: Vec<usize> = shards.iter().map(|(lane, _)| *lane).collect();
    let mut ready = false;
    let tasks = shards
        .iter_mut()
        .map(|(lane, owner)| shard_task(*lane, owner, factory, startup, control, &waker))
        .collect();
    drive_tasks(tasks, control, &waker, thread_role, || {
        if !ready && !lanes.is_empty() {
            ready = control.update(|state| {
                let all = lanes.iter().all(|lane| state.helper_ready[*lane]);
                if all {
                    state.stats.ready += 1;
                }
                all
            });
        }
    });
    control.drained();

    let teardown = teardown_scope(factory, startup, control, thread_role);
    let teardown = teardown.as_ref().unwrap_or(startup);
    let tasks = shards
        .iter_mut()
        .filter(|(_, owner)| owner.release == Release::Drained)
        .map(|(_, owner)| shutdown_task(owner, teardown, control, &waker))
        .collect();
    drive_tasks(tasks, control, &waker, thread_role, || {});
    for (_, owner) in &mut shards {
        if owner.release == Release::ShutdownFenced {
            owner.release = Release::Releasable;
        }
    }
}

/// One helper shard from startup to its first fence.
///
/// The shard keeps being polled until its lane closes submissions, and again
/// until the lane's first fence is confirmed, because the lane's outstanding
/// work may depend on the shard making progress.
fn shard_task<'a, S, F>(
    lane: usize,
    owner: &'a mut Owner<S>,
    factory: &'a F,
    startup: &'a S,
    control: &'a Ctl<S>,
    waker: &'a Waker,
) -> Task<'a>
where
    S: Scope,
    F: Factory<S> + ?Sized,
{
    let role = owner.role;
    let budget = control.config.helper_budget;
    Box::pin(async move {
        let started = match catch(|| {
            let service = owner.service();
            service.register_driver(waker);
            service.start(startup)
        }) {
            Err(message) => Err(GroupError::panicked(role, Stage::Start, message)),
            Ok(mut future) => {
                std::future::poll_fn(|cx| {
                    if let Err(error) = attempt(role, Stage::Start, || startup.check()) {
                        return Poll::Ready(Err(error));
                    }
                    if control.phase() != Phase::Running {
                        let error = Error::new(ErrorKind::Canceled).with_context("group stopping");
                        return Poll::Ready(Err(GroupError::failed(
                            role,
                            Stage::Start,
                            error.into(),
                        )));
                    }
                    poll_caught(&mut future, cx).map_err(|c| c.into_group(role, Stage::Start))
                })
                .await
            }
        };
        match started {
            Ok(()) => control.update(|state| state.helper_ready[lane] = true),
            Err(error) => control.fail(error),
        }

        // Serve the lane until it closes its submissions.
        std::future::poll_fn(|cx| {
            control.record(attempt(role, Stage::Run, || {
                let service = owner.service();
                service.register_driver(waker);
                service.poll(cx, budget)
            }));
            if control.lock().closed[lane] {
                Poll::Ready(())
            } else {
                Poll::Pending
            }
        })
        .await;

        let teardown = teardown_scope(factory, startup, control, role);
        let teardown = teardown.as_ref().unwrap_or(startup);
        control.record(attempt(role, Stage::StopAdmission, || {
            owner.service().stop_admission()
        }));
        let drain = match catch(|| {
            let service = owner.service();
            service.register_driver(waker);
            service.drain(teardown)
        }) {
            Ok(future) => diagnosed(future, teardown, control, role, Stage::Drain).await,
            Err(message) => Err(GroupError::panicked(role, Stage::Drain, message)),
        };
        control.record(drain);
        control.record(attempt(role, Stage::Close, || owner.service().close()));

        // Keep serving until the lane's first fence proves its work is done.
        let fence = std::future::poll_fn(|cx| {
            control.record(attempt(role, Stage::Fence, || teardown.check()));
            control.record(attempt(role, Stage::Run, || {
                let service = owner.service();
                service.register_driver(waker);
                service.poll(cx, budget)
            }));
            match control.lock().fenced[lane] {
                LaneFence::Pending => Poll::Pending,
                done => Poll::Ready(done),
            }
        })
        .await;
        if fence == LaneFence::Failed {
            // The lane's unfenced work may reference this shard. The lane
            // already reported the fatal event; leak the shard with it.
            owner.release = Release::Leaked;
            return;
        }
        if fence_helper(owner, teardown, waker).await {
            owner.release = Release::Drained;
        }
    })
}

/// One helper shard from shutdown through its second fence.
fn shutdown_task<'a, S: Scope>(
    owner: &'a mut Owner<S>,
    teardown: &'a S,
    control: &'a Ctl<S>,
    waker: &'a Waker,
) -> Task<'a> {
    let role = owner.role;
    Box::pin(async move {
        let shutdown = match catch(|| {
            let service = owner.service();
            service.register_driver(waker);
            service.shutdown(teardown)
        }) {
            Ok(future) => diagnosed(future, teardown, control, role, Stage::Shutdown).await,
            Err(message) => Err(GroupError::panicked(role, Stage::Shutdown, message)),
        };
        control.record(shutdown);
        if fence_helper(owner, teardown, waker).await {
            owner.release = Release::ShutdownFenced;
        }
    })
}

/// Poll helper tasks round-robin until all finish, parking every
/// `helper_passes_per_park` passes.
///
/// A task that panics is forgotten rather than dropped: it may borrow a
/// service whose fences have not completed. That service's owner reports the
/// abandonment when it is dropped.
fn drive_tasks<E>(
    tasks: Vec<Task<'_>>,
    control: &Control<E>,
    waker: &Waker,
    role: Role,
    mut after_pass: impl FnMut(),
) {
    let config = &control.config;
    let mut tasks: Vec<Option<Task<'_>>> = tasks.into_iter().map(Some).collect();
    let mut remaining = tasks.len();
    let mut first = 0;
    let mut passes = 0;
    let mut cx = Context::from_waker(waker);
    while remaining != 0 {
        for offset in 0..tasks.len() {
            let index = (first + offset) % tasks.len();
            let Some(task) = &mut tasks[index] else {
                continue;
            };
            match catch(|| task.as_mut().poll(&mut cx)) {
                Ok(Poll::Pending) => continue,
                Ok(Poll::Ready(())) => drop(tasks[index].take()),
                Err(message) => {
                    mem::forget(tasks[index].take());
                    control.fail(GroupError::panicked(role, Stage::Run, message));
                }
            }
            remaining -= 1;
        }
        after_pass();
        first = (first + 1) % tasks.len();
        passes += 1;
        if remaining != 0 && passes >= config.helper_passes_per_park {
            thread::park_timeout(config.idle_wait);
            passes = 0;
        }
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::group::GroupConfig;
    use crate::{Error, Unscoped};
    use std::sync::atomic::{AtomicBool, Ordering};

    struct Flagged(Arc<AtomicBool>);

    impl Service<Unscoped> for Flagged {
        fn poll(&mut self, _: &mut Context<'_>, _: usize) -> Result<(), Error> {
            Ok(())
        }
    }

    impl Drop for Flagged {
        fn drop(&mut self) {
            self.0.store(true, Ordering::SeqCst);
        }
    }

    fn control() -> Arc<Control<Error>> {
        Arc::new(Control::new(1, 0, true, GroupConfig::default()))
    }

    #[test]
    #[cfg_attr(miri, ignore = "intentionally leaks under FatalPolicy::LeakAndReport")]
    fn abandoned_owner_leaks_and_reports() {
        for release in [Release::Live, Release::Drained, Release::ShutdownFenced] {
            let dropped = Arc::new(AtomicBool::new(false));
            let control = control();
            let mut owner = Owner::new(
                Box::new(Flagged(dropped.clone())),
                Role::Lane(0),
                control.clone(),
            );
            owner.release = release;
            drop(owner);
            assert!(!dropped.load(Ordering::SeqCst));
            let error = control.result().unwrap_err();
            assert!(matches!(
                error,
                GroupError::Fatal {
                    event: FatalEvent {
                        cause: FatalCause::Abandoned,
                        ..
                    },
                    ..
                }
            ));
            assert_eq!(control.lock().stats.fatal, 1);
        }
    }

    #[test]
    #[cfg_attr(miri, ignore = "intentionally leaks under FatalPolicy::LeakAndReport")]
    fn released_and_leaked_owners() {
        let dropped = Arc::new(AtomicBool::new(false));
        let control = control();
        let mut owner = Owner::new(
            Box::new(Flagged(dropped.clone())),
            Role::Lane(0),
            control.clone(),
        );
        owner.release = Release::Releasable;
        drop(owner);
        assert!(dropped.load(Ordering::SeqCst));

        let dropped = Arc::new(AtomicBool::new(false));
        let mut owner = Owner::new(
            Box::new(Flagged(dropped.clone())),
            Role::Lane(0),
            control.clone(),
        );
        owner.release = Release::Leaked;
        drop(owner);
        assert!(!dropped.load(Ordering::SeqCst));
        assert_eq!(control.result(), Ok(()));
    }

    #[test]
    #[cfg_attr(miri, ignore = "intentionally leaks under FatalPolicy::LeakAndReport")]
    fn panicking_tasks_are_forgotten_and_reported() {
        struct Tracked(Arc<AtomicBool>);
        impl Drop for Tracked {
            fn drop(&mut self) {
                self.0.store(true, Ordering::SeqCst);
            }
        }
        let dropped = Arc::new(AtomicBool::new(false));
        let tracked = Tracked(dropped.clone());
        let control = control();
        let finished = Arc::new(AtomicBool::new(false));
        let done = finished.clone();
        let tasks: Vec<Task<'_>> = vec![
            // State captured by a hand-written future survives its panic.
            Box::pin(std::future::poll_fn(move |_| {
                let _ = &tracked;
                panic!("task panic")
            })),
            Box::pin(async move {
                done.store(true, Ordering::SeqCst);
            }),
        ];
        let waker = thread_waker(None);
        drive_tasks(tasks, &control, &waker, Role::Group, || {});
        assert!(finished.load(Ordering::SeqCst));
        assert!(
            !dropped.load(Ordering::SeqCst),
            "a panicking task must not be dropped"
        );
        assert_eq!(
            control.result().unwrap_err().panic_message(),
            Some("task panic")
        );
    }

    #[test]
    fn diagnosed_teardown_reports_but_never_cancels() {
        #[derive(Clone)]
        struct Expired;

        impl Scope for Expired {
            type Error = Error;

            fn check(&self) -> Result<(), Error> {
                Err(Error::new(crate::ErrorKind::DeadlineExceeded))
            }
        }

        let control = Arc::new(Control::<Error>::new(0, 1, true, GroupConfig::default()));
        let polls = std::cell::Cell::new(0);
        let future: LifecycleFuture<'_, Error> = Box::pin(std::future::poll_fn(|_| {
            polls.set(polls.get() + 1);
            if polls.get() == 4 {
                Poll::Ready(Ok(()))
            } else {
                Poll::Pending
            }
        }));
        let waker = thread_waker(None);
        let task: Task<'_> = Box::pin(async {
            let result = diagnosed(future, &Expired, &control, Role::Group, Stage::Drain).await;
            control.record(result);
        });
        drive_tasks(vec![task], &control, &waker, Role::Group, || {});
        assert_eq!(polls.get(), 4);
        assert!(
            control
                .result()
                .unwrap_err()
                .error()
                .unwrap()
                .is_deadline_exceeded()
        );
    }
}
