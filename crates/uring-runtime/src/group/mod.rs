//! Pinned thread groups with explicit startup, drain and ownership fences.
//!
//! A [`Group`] runs a set of *lanes*, each a thread (optionally pinned to a
//! CPU) that builds and drives one [`Service`], plus optional *helpers*:
//! threads that cooperatively drive one helper service ("shard") per lane
//! they serve. Services are built on their own thread by a [`Factory`], so
//! they need not be `Send` and can own thread-local resources such as an
//! io_uring reactor.
//!
//! # Lifecycle
//!
//! The group moves through three monotonic phases: running, drain and
//! shutdown. Each lane's service goes through:
//!
//! 1. [`Factory::build_lane`], then [`Service::start`]. The group is *ready*
//!    once every lane and helper has started.
//! 2. Steady state: [`Service::poll`] with a work budget, then a park of at
//!    most [`Service::wait_timeout`] (cut short by the service's waker),
//!    until the group leaves the running phase.
//! 3. [`Service::stop_admission`], [`Service::drain`], [`Service::close`],
//!    then the first [`Service::fence`]. The lane is now *drained*.
//! 4. After shutdown is requested: [`Service::shutdown`] and the second
//!    [`Service::fence`]. Only then is the service dropped, on its own thread.
//!
//! The group leaves the running phase when [`Group::drain`] or
//! [`Group::shutdown`] is called, when any service, factory or scope fails or
//! panics, or when a service reports a failure through its
//! [`FailureReporter`]. Every failure and panic is caught and recorded; the
//! first one is returned by [`Group::join`] or [`Group::run`] as a
//! [`GroupError`] that keeps the panic message.
//!
//! # Ownership fences
//!
//! A fence is a service's proof that no external work (typically in-flight
//! kernel I/O) still references memory the service owns. Fences are never
//! skipped: teardown steps are not canceled when the scope expires (that is
//! recorded as a diagnosis instead). If a fence fails or panics, or a service
//! would be destroyed before both fences completed, the group cannot prove
//! the memory is unreferenced and applies the [`FatalPolicy`]: by default the
//! service is leaked and a [`GroupError::Fatal`] is returned; optionally the
//! process aborts. A fence future that panics is leaked too, but a panic
//! unwinds through the future's own frames first and drops whatever those
//! frames hold: keep memory the kernel may reference in the service, not in
//! locals of a fence future.
//!
//! # Helpers
//!
//! A helper shard for a lane starts before that lane's service is built, keeps
//! being polled until the lane closes its submissions ([`Service::close`]) and
//! its first fence completes, and only then drains and fences itself. All
//! shards of a helper are polled round-robin, so shards may wait on each other.
//!
//! # Example
//!
//! ```
//! use std::sync::Arc;
//! use std::task::Context;
//! use uring_runtime::group::{Factory, Group, GroupConfig, Plan, Service};
//! use uring_runtime::{Error, Unscoped};
//!
//! struct Worker;
//!
//! impl Service<Unscoped> for Worker {
//!     fn poll(&mut self, _cx: &mut Context<'_>, _budget: usize) -> Result<(), Error> {
//!         Ok(()) // Do at most `budget` units of work.
//!     }
//! }
//!
//! struct Workers;
//!
//! impl Factory<Unscoped> for Workers {
//!     fn build_lane(&self, _lane: usize) -> Result<Box<dyn Service<Unscoped>>, Error> {
//!         Ok(Box::new(Worker))
//!     }
//! }
//!
//! # fn main() -> Result<(), Box<dyn std::error::Error>> {
//! // One unpinned lane. Use `Some(cpu)` (or just `cpu`) to pin it.
//! let plan = Plan::builder().lane("worker-0", None).build()?;
//! let mut group = Group::new(plan, GroupConfig::default())?;
//! group.start(Arc::new(Workers), &Unscoped)?; // Returns once every lane is ready.
//! group.shutdown(&Unscoped)?; // Drain, fence, shut down and fence again.
//! group.join()?;
//! assert_eq!(group.stats().done, 1);
//! # Ok(())
//! # }
//! ```

mod control;
mod engine;
mod error;
mod plan;

use std::future::Future;
use std::pin::Pin;
use std::sync::Arc;
use std::task::{Context, Wake, Waker};
use std::thread::{self, JoinHandle, Thread};
use std::time::Duration;

pub use control::{FailureReporter, Stats};
pub use error::{FatalCause, FatalEvent, FatalPolicy, GroupConfig, GroupError, Role, Stage};
pub use plan::{Helper, Lane, Plan, PlanBuilder};

use crate::backend::RemoteWaker;
use crate::{Error, ErrorKind, Scope, affinity};
use control::{Control, Phase, catch};

/// A boxed, non-`Send` lifecycle future returned by [`Service`] methods.
pub type LifecycleFuture<'a, E> = Pin<Box<dyn Future<Output = Result<(), E>> + 'a>>;

fn ready<'a, E: 'a>() -> LifecycleFuture<'a, E> {
    Box::pin(std::future::ready(Ok(())))
}

/// Builds services on the thread that will own them.
///
/// The factory is shared by every group thread, so it must be `Sync`; the
/// services it builds need not be `Send`.
pub trait Factory<S: Scope>: Sync {
    /// Build the service for lane `lane` on its (pinned) thread.
    fn build_lane(&self, lane: usize) -> Result<Box<dyn Service<S>>, S::Error>;

    /// Build the helper shard serving lane `lane` on its helper thread.
    ///
    /// Only called for lanes listed in a [`Helper`]. The default fails with
    /// [`ErrorKind::InvalidConfiguration`].
    fn build_helper(&self, lane: usize) -> Result<Box<dyn Service<S>>, S::Error> {
        let _ = lane;
        Err(Error::new(ErrorKind::InvalidConfiguration)
            .with_context("factory does not build helpers")
            .into())
    }

    /// Release resources prepared for lane `lane` whose service will never be
    /// built (because startup failed first). No work can have been accepted
    /// for the lane. An error is a fatal event ([`Stage::Abandon`]): the group
    /// cannot leak factory-owned resources, so the factory must keep them.
    fn abandon_lane(&self, lane: usize) -> Result<(), S::Error> {
        let _ = lane;
        Ok(())
    }

    /// The scope used for teardown (drain, fences, shutdown), derived from the
    /// startup scope when teardown begins. Override to give teardown a fresh
    /// deadline. A failing teardown scope is reported but never skips a fence.
    fn teardown_scope(&self, startup: &S) -> S {
        startup.clone()
    }
}

/// A service driven by one group thread.
///
/// Only [`Service::poll`] is required; every lifecycle step defaults to
/// completing immediately. Lifecycle futures are polled on the service's own
/// thread with the service's [`waker`](Service::waker).
pub trait Service<S: Scope> {
    /// Receive a handle for reporting failures that happen outside of the
    /// service's methods. Called once, right after the service is built.
    fn set_failure_reporter(&mut self, reporter: FailureReporter<S::Error>) {
        let _ = reporter;
    }

    /// The waker used to drive this service. Waking it must end the thread's
    /// park. The default is [`thread_waker(None)`](thread_waker); a service
    /// that blocks in its reactor should combine both, for example with
    /// `thread_waker(Some(reactor.remote_waker()?))`.
    fn waker(&self) -> Result<Waker, S::Error> {
        Ok(thread_waker(None))
    }

    /// Arrange for `waker` to be woken when locally owned work can progress.
    /// Called before each steady-state turn and before helper lifecycle steps.
    fn register_driver(&self, waker: &Waker) {
        let _ = waker;
    }

    /// Initialize the service. The group is ready once every service started.
    /// Startup is abandoned if the scope fails or the group stops first.
    fn start<'a>(&'a mut self, scope: &'a S) -> LifecycleFuture<'a, S::Error> {
        let _ = scope;
        ready()
    }

    /// Do at most `budget` units of steady-state work without blocking.
    ///
    /// An error stops the group. Lanes receive
    /// [`GroupConfig::work_budget`]; helper shards receive
    /// [`GroupConfig::helper_budget`] per pass.
    fn poll(&mut self, cx: &mut Context<'_>, budget: usize) -> Result<(), S::Error>;

    /// How long the lane may park after a turn, at most `maximum`. Return
    /// zero when work is already due. Called after [`Service::poll`].
    fn wait_timeout(&self, maximum: Duration) -> Duration {
        maximum
    }

    /// Stop accepting new work. Called once, before [`Service::drain`].
    fn stop_admission(&mut self) -> Result<(), S::Error> {
        Ok(())
    }

    /// Finish or fail accepted work. Not canceled by the teardown scope.
    fn drain<'a>(&'a mut self, scope: &'a S) -> LifecycleFuture<'a, S::Error> {
        let _ = scope;
        ready()
    }

    /// Stop submitting work to the lane's helper shard. Called after drain.
    fn close(&mut self) -> Result<(), S::Error> {
        Ok(())
    }

    /// Complete only once no external work references service-owned
    /// resources, even after cancellation. Called twice: after drain and
    /// after shutdown. An error or panic is a [`FatalEvent`].
    fn fence<'a>(&'a mut self, scope: &'a S) -> LifecycleFuture<'a, S::Error> {
        let _ = scope;
        ready()
    }

    /// Release resources after drain. Called only once shutdown is requested;
    /// the second fence follows.
    fn shutdown<'a>(&'a mut self, scope: &'a S) -> LifecycleFuture<'a, S::Error> {
        let _ = scope;
        ready()
    }
}

/// A waker that unparks the calling thread and, if given, also wakes
/// `extra` (for example a reactor's [`RemoteWaker`]).
///
/// Group threads park between turns, so this is the natural waker for a
/// service whose progress depends on other threads.
pub fn thread_waker(extra: Option<RemoteWaker>) -> Waker {
    struct ThreadWake {
        thread: Thread,
        extra: Option<RemoteWaker>,
    }

    impl Wake for ThreadWake {
        fn wake(self: Arc<Self>) {
            self.wake_by_ref();
        }

        fn wake_by_ref(self: &Arc<Self>) {
            if let Some(extra) = &self.extra {
                extra.wake();
            }
            self.thread.unpark();
        }
    }

    Waker::from(Arc::new(ThreadWake {
        thread: thread::current(),
        extra,
    }))
}

/// A group of pinned threads running [`Service`]s.
///
/// A group is reusable: after [`Group::join`] (or [`Group::run`]) returns it
/// can be started again. Dropping a group requests shutdown and joins every
/// thread; it never detaches threads that still own services.
pub struct Group<S>
where
    S: Scope + Send,
    S::Error: Clone + Send,
{
    plan: Plan,
    config: GroupConfig,
    control: Arc<Control<S::Error>>,
    coordinator: Option<JoinHandle<()>>,
}

impl<S> Group<S>
where
    S: Scope + Send,
    S::Error: Clone + Send,
{
    /// Create a group. No thread is spawned and no service is built.
    ///
    /// # Errors
    ///
    /// [`ErrorKind::InvalidConfiguration`] if `config` is invalid (see
    /// [`GroupConfig::validate`]).
    pub fn new(plan: Plan, config: GroupConfig) -> crate::Result<Self> {
        config.validate()?;
        let control = Arc::new(Control::new(0, 0, false, config.clone()));
        Ok(Self {
            plan,
            config,
            control,
            coordinator: None,
        })
    }

    /// The plan.
    pub fn plan(&self) -> &Plan {
        &self.plan
    }

    /// A snapshot of the current execution's progress.
    pub fn stats(&self) -> Stats {
        let mut stats = self.control.lock().stats;
        stats.joinable = self.coordinator.is_some();
        stats
    }

    /// Validate the plan against the current environment and reset the shared
    /// state for a new execution.
    fn prepare(
        &mut self,
        caller_is_lane: bool,
        check_scope: bool,
    ) -> Result<(), GroupError<S::Error>> {
        let invalid = |context| {
            let error = Error::new(ErrorKind::InvalidConfiguration).with_context(context);
            Err(GroupError::failed(
                Role::Group,
                Stage::Prepare,
                error.into(),
            ))
        };
        if self.coordinator.is_some() {
            return invalid("group is already running");
        }
        let threads = self.plan.threads() + usize::from(!caller_is_lane);
        if threads > self.plan.max_threads() {
            return invalid("plan exceeds max_threads");
        }
        let allowed = affinity::current_cpus()
            .map_err(|error| GroupError::failed(Role::Group, Stage::Prepare, error.into()))?;
        let cpus = self.plan.lanes().iter().map(|lane| lane.cpu);
        if cpus
            .chain(self.plan.helpers().iter().map(|helper| helper.cpu))
            .flatten()
            .any(|cpu| !allowed.contains(cpu))
        {
            return invalid("plan uses a CPU outside the caller's affinity");
        }
        let control = Control::new(
            self.plan.lanes().len(),
            self.plan.helpers().len(),
            caller_is_lane,
            self.config.clone(),
        );
        {
            let mut state = control.lock();
            state.check_scope = check_scope;
            state.helper_ready.fill(true);
            for helper in self.plan.helpers() {
                for &lane in &helper.lanes {
                    state.helper_ready[lane] = false;
                }
            }
        }
        self.control = Arc::new(control);
        Ok(())
    }

    /// Spawn the group on new threads and wait until every service started.
    ///
    /// Lane 0 runs on a new coordinator thread, which spawns the other
    /// threads. `scope` is cloned into every thread as the startup scope; the
    /// caller's wait also ends if `scope` fails, in which case the group is
    /// stopped and joined before returning.
    ///
    /// # Errors
    ///
    /// The first failure of the group if startup did not complete. The group
    /// has then been fully torn down and joined.
    pub fn start<F>(&mut self, factory: Arc<F>, scope: &S) -> Result<(), GroupError<S::Error>>
    where
        F: Factory<S> + Send + ?Sized + 'static,
    {
        self.prepare(false, false)?;
        let startup = match catch(|| scope.clone()) {
            Ok(startup) => startup,
            Err(message) => {
                let error = GroupError::panicked(Role::Group, Stage::Prepare, message);
                engine::rollback(&self.plan, &*factory, &self.control, error.clone());
                return Err(error);
            }
        };
        let plan = self.plan.clone();
        let control = self.control.clone();
        let thread_factory = factory.clone();
        let spawned = thread::Builder::new()
            .name(plan.lanes()[0].name.clone())
            .spawn(move || {
                let result =
                    catch(|| engine::run_prepared(&plan, &*thread_factory, &startup, &control));
                match result {
                    Ok(result) => control.record(result),
                    Err(message) => {
                        control.fail(GroupError::panicked(Role::Group, Stage::Exit, message))
                    }
                }
            });
        match spawned {
            Ok(handle) => self.coordinator = Some(handle),
            Err(error) => {
                let error = Error::from(error).with_context("spawn group thread");
                let error = GroupError::failed(Role::Group, Stage::Spawn, error.into());
                engine::rollback(&self.plan, &*factory, &self.control, error.clone());
                return Err(error);
            }
        }
        let result = self.control.wait_for(scope, Role::Group, |state| {
            state.stats.ready == state.stats.total
        });
        if let Err(error) = &result {
            self.control.fail(error.clone());
            let _ = self.join();
        }
        result
    }

    /// Run the group with the calling thread as lane 0, returning after every
    /// thread has finished.
    ///
    /// `scope` governs startup only; steady state runs until a service fails
    /// or reports a failure. Use [`Group::run_scoped`] to also stop when the
    /// scope fails. The calling thread's CPU affinity is restored afterwards.
    ///
    /// # Errors
    ///
    /// The group's first failure.
    pub fn run<F>(&mut self, factory: &F, scope: &S) -> Result<(), GroupError<S::Error>>
    where
        F: Factory<S> + ?Sized,
    {
        self.run_inner(factory, scope, false)
    }

    /// Like [`Group::run`], but steady state also ends when `scope` fails.
    /// That failure is returned; teardown and fences still run to completion.
    ///
    /// # Errors
    ///
    /// The group's first failure.
    pub fn run_scoped<F>(&mut self, factory: &F, scope: &S) -> Result<(), GroupError<S::Error>>
    where
        F: Factory<S> + ?Sized,
    {
        self.run_inner(factory, scope, true)
    }

    fn run_inner<F>(
        &mut self,
        factory: &F,
        scope: &S,
        check_scope: bool,
    ) -> Result<(), GroupError<S::Error>>
    where
        F: Factory<S> + ?Sized,
    {
        self.prepare(true, check_scope)?;
        let result = catch(|| engine::run_prepared(&self.plan, factory, scope, &self.control));
        match result {
            Ok(result) => self.control.record(result),
            Err(message) => {
                self.control
                    .fail(GroupError::panicked(Role::Group, Stage::Exit, message))
            }
        }
        self.control.result()
    }

    /// Ask every lane to drain and wait until all have passed their first
    /// fence. The threads then wait for [`Group::shutdown`] or
    /// [`Group::join`].
    ///
    /// A failing `scope` ends only this wait, never the drain itself.
    ///
    /// # Errors
    ///
    /// The group's first failure, or the scope's failure.
    pub fn drain(&self, scope: &S) -> Result<(), GroupError<S::Error>> {
        self.control.set_phase(Phase::Drain);
        self.control.wait_for(scope, Role::Group, |state| {
            state.stats.drained == state.stats.total
        })
    }

    /// Ask the group to drain and shut down, and wait until every thread has
    /// finished. Call [`Group::join`] afterwards to reap the coordinator.
    ///
    /// A failing `scope` ends only this wait.
    ///
    /// # Errors
    ///
    /// The group's first failure, or the scope's failure.
    pub fn shutdown(&self, scope: &S) -> Result<(), GroupError<S::Error>> {
        self.control.set_phase(Phase::Shutdown);
        self.control.wait_for(scope, Role::Group, |state| {
            state.stats.done == state.stats.total
        })
    }

    /// Request shutdown and join the coordinator thread.
    ///
    /// Blocks until every service completed its fences; a fence that never
    /// completes blocks this call forever.
    ///
    /// # Errors
    ///
    /// The group's first failure.
    pub fn join(&mut self) -> Result<(), GroupError<S::Error>> {
        self.control.set_phase(Phase::Shutdown);
        if let Some(handle) = self.coordinator.take()
            && let Err(payload) = handle.join()
        {
            let message = control::panic_message(payload);
            self.control
                .fail(GroupError::panicked(Role::Group, Stage::Exit, message));
        }
        self.control.result()
    }
}

impl<S> Drop for Group<S>
where
    S: Scope + Send,
    S::Error: Clone + Send,
{
    /// Request shutdown and join every thread.
    fn drop(&mut self) {
        let _ = self.join();
    }
}

impl<S> std::fmt::Debug for Group<S>
where
    S: Scope + Send,
    S::Error: Clone + Send,
{
    fn fmt(&self, f: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        f.debug_struct("Group")
            .field("plan", &self.plan)
            .field("config", &self.config)
            .field("stats", &self.stats())
            .finish_non_exhaustive()
    }
}
