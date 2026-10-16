//! Shared lifecycle state: phases, barriers, the first failure and statistics.

use std::any::Any;
use std::fmt;
use std::panic::{AssertUnwindSafe, catch_unwind};
use std::sync::{Arc, Condvar, Mutex, MutexGuard};

use super::error::{FatalEvent, FatalPolicy, GroupConfig, GroupError, Role, Stage};
use crate::Scope;

/// Monotonic lifecycle requests shared by every thread of a group.
#[derive(Clone, Copy, Debug, PartialEq, Eq, PartialOrd, Ord)]
pub(super) enum Phase {
    Running,
    Drain,
    Shutdown,
}

/// Whether a lane's first ownership fence has been confirmed.
#[derive(Clone, Copy, Debug, PartialEq, Eq)]
pub(super) enum LaneFence {
    Pending,
    Confirmed,
    /// The fence failed; the lane's service was leaked.
    Failed,
}

/// Lifecycle diagnostics for the current execution of a group.
///
/// Counts are of OS threads (lanes plus helpers), not helper shards. They are
/// a snapshot for monitoring, not a synchronization mechanism: use the
/// [`Group`](super::Group) methods to wait for a phase.
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Hash)]
pub struct Stats {
    /// Planned lane and helper threads.
    pub total: usize,
    /// Threads whose services finished starting.
    pub ready: usize,
    /// Threads whose services passed their first ownership fence (or never
    /// started).
    pub drained: usize,
    /// Threads that exited, including threads that were never spawned.
    pub done: usize,
    /// Fatal events recorded (see [`FatalPolicy`]).
    pub fatal: usize,
    /// Whether a coordinator thread from [`Group::start`](super::Group::start)
    /// is waiting to be joined.
    pub joinable: bool,
}

pub(super) struct State<E> {
    pub(super) phase: Phase,
    pub(super) stats: Stats,
    pub(super) error: Option<GroupError<E>>,
    /// Per lane: its helper shard (if any) has started.
    pub(super) helper_ready: Vec<bool>,
    /// Per lane: the lane closed its helper submissions (or exited).
    pub(super) closed: Vec<bool>,
    /// Per lane: the outcome of its first ownership fence.
    pub(super) fenced: Vec<LaneFence>,
    /// `run` mode: lanes shut down right after draining without waiting for
    /// a shutdown request.
    pub(super) auto_shutdown: bool,
    /// `run_scoped` mode: the scope is checked during steady state.
    pub(super) check_scope: bool,
}

pub(super) struct Control<E> {
    state: Mutex<State<E>>,
    changed: Condvar,
    pub(super) config: GroupConfig,
}

impl<E> Control<E> {
    pub(super) fn new(
        lanes: usize,
        helpers: usize,
        auto_shutdown: bool,
        config: GroupConfig,
    ) -> Self {
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
                fenced: vec![LaneFence::Pending; lanes],
                auto_shutdown,
                check_scope: false,
            }),
            changed: Condvar::new(),
            config,
        }
    }

    /// Lock the state, recovering it after a panic so teardown can proceed.
    pub(super) fn lock(&self) -> MutexGuard<'_, State<E>> {
        self.state.lock().unwrap_or_else(|e| e.into_inner())
    }

    /// Apply `update` to the state and wake every waiter.
    pub(super) fn update<T>(&self, update: impl FnOnce(&mut State<E>) -> T) -> T {
        let result = update(&mut self.lock());
        self.changed.notify_all();
        result
    }

    pub(super) fn phase(&self) -> Phase {
        self.lock().phase
    }

    /// Advance the phase; it never moves backward.
    pub(super) fn set_phase(&self, phase: Phase) {
        self.update(|state| state.phase = state.phase.max(phase));
    }

    /// Record a failure and ask every lane to drain.
    ///
    /// The first failure is kept, except that a fatal failure replaces an
    /// earlier non-fatal one.
    pub(super) fn fail(&self, error: GroupError<E>) {
        self.update(|state| {
            let replace = match &state.error {
                None => true,
                Some(existing) => error.is_fatal() && !existing.is_fatal(),
            };
            if replace {
                state.error = Some(error);
            }
            state.phase = state.phase.max(Phase::Drain);
        });
    }

    /// Record the error of a fallible step, if any.
    pub(super) fn record(&self, result: Result<(), GroupError<E>>) {
        if let Err(error) = result {
            self.fail(error);
        }
    }

    /// Apply the fatal policy and record the event. With
    /// [`FatalPolicy::Abort`] this does not return.
    pub(super) fn fatal(&self, event: FatalEvent, error: Option<E>) {
        match &self.config.fatal {
            FatalPolicy::Abort => {
                eprintln!("uring-runtime: fatal group event: {event}; aborting");
                std::process::abort();
            }
            FatalPolicy::Custom(hook) => {
                let _ = catch_unwind(AssertUnwindSafe(|| hook(&event)));
            }
            FatalPolicy::LeakAndReport => {}
        }
        self.update(|state| state.stats.fatal += 1);
        self.fail(GroupError::Fatal { event, error });
    }

    pub(super) fn close(&self, lane: usize) {
        self.update(|state| state.closed[lane] = true);
    }

    pub(super) fn set_fence(&self, lane: usize, fence: LaneFence) {
        self.update(|state| {
            if state.fenced[lane] == LaneFence::Pending {
                state.fenced[lane] = fence;
            }
        });
    }

    /// Count a thread as drained without waiting for shutdown.
    pub(super) fn count_drained(&self) {
        self.update(|state| state.stats.drained += 1);
    }

    /// Count a thread as drained, then block until shutdown is requested
    /// (or, in `run` mode or after a failure, return immediately).
    pub(super) fn drained(&self) {
        let mut state = self.lock();
        state.stats.drained += 1;
        self.changed.notify_all();
        while state.phase != Phase::Shutdown && !state.auto_shutdown && state.error.is_none() {
            state = self.changed.wait(state).unwrap_or_else(|e| e.into_inner());
        }
    }

    /// Whether steady state should end, checking the scope first in
    /// `run_scoped` mode.
    pub(super) fn stopping<S: Scope<Error = E>>(&self, scope: &S, role: Role) -> bool {
        if self.lock().check_scope {
            self.record(attempt(role, Stage::Run, || scope.check()));
        }
        self.phase() != Phase::Running
    }
}

impl<E: Clone> Control<E> {
    /// The first recorded failure, if any.
    pub(super) fn result(&self) -> Result<(), GroupError<E>> {
        self.lock().error.clone().map_or(Ok(()), Err)
    }

    /// Block until `done` holds or a failure is recorded.
    ///
    /// The scope is checked outside the lock every `wait_interval`; its
    /// failure ends only this wait.
    pub(super) fn wait_for<S: Scope<Error = E>>(
        &self,
        scope: &S,
        role: Role,
        done: impl Fn(&State<E>) -> bool,
    ) -> Result<(), GroupError<E>> {
        let mut state = self.lock();
        loop {
            if let Some(error) = &state.error {
                return Err(error.clone());
            }
            if done(&state) {
                return Ok(());
            }
            // Scope code may panic or inspect the group; never run it locked.
            drop(state);
            attempt(role, Stage::Wait, || scope.check())?;
            state = self.lock();
            state = self
                .changed
                .wait_timeout(state, self.config.wait_interval)
                .unwrap_or_else(|e| e.into_inner())
                .0;
        }
    }
}

/// A handle that lets a service report a failure that happens while none of
/// its methods is running, for example in an I/O completion callback.
///
/// Reporting records the first failure (as [`GroupError::Failed`] with
/// [`Stage::Report`]) and asks every lane to stop admission and drain. It
/// cannot skip teardown steps or ownership fences. Installed with
/// [`Service::set_failure_reporter`](super::Service::set_failure_reporter).
pub struct FailureReporter<E> {
    control: Arc<Control<E>>,
    role: Role,
}

impl<E> FailureReporter<E> {
    pub(super) fn new(control: Arc<Control<E>>, role: Role) -> Self {
        Self { control, role }
    }

    /// Record `error` and ask the group to drain.
    pub fn report(&self, error: E) {
        self.control
            .fail(GroupError::failed(self.role, Stage::Report, error));
    }

    /// The role this reporter attributes failures to.
    pub fn role(&self) -> Role {
        self.role
    }
}

impl<E> Clone for FailureReporter<E> {
    fn clone(&self) -> Self {
        Self {
            control: self.control.clone(),
            role: self.role,
        }
    }
}

impl<E> fmt::Debug for FailureReporter<E> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.debug_struct("FailureReporter")
            .field("role", &self.role)
            .finish_non_exhaustive()
    }
}

/// Run `f`, converting a panic into its message.
pub(super) fn catch<T>(f: impl FnOnce() -> T) -> Result<T, String> {
    catch_unwind(AssertUnwindSafe(f)).map_err(panic_message)
}

/// Run a fallible step, attributing its error or panic to `role` and `stage`.
pub(super) fn attempt<T, E>(
    role: Role,
    stage: Stage,
    f: impl FnOnce() -> Result<T, E>,
) -> Result<T, GroupError<E>> {
    match catch(f) {
        Ok(Ok(value)) => Ok(value),
        Ok(Err(error)) => Err(GroupError::failed(role, stage, error)),
        Err(message) => Err(GroupError::panicked(role, stage, message)),
    }
}

/// Extract a panic payload's message.
pub(super) fn panic_message(payload: Box<dyn Any + Send>) -> String {
    match payload.downcast::<String>() {
        Ok(message) => *message,
        Err(payload) => match payload.downcast::<&'static str>() {
            Ok(message) => (*message).to_owned(),
            Err(_) => "non-string panic payload".to_owned(),
        },
    }
}
