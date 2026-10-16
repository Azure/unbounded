//! Group failures, fatal events and the fatal policy.

use std::fmt;
use std::sync::Arc;
use std::time::Duration;

use crate::{Error, ErrorKind, Result};

/// The thread or service a failure is attributed to.
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum Role {
    /// The group itself (preparation, spawning, waiting, joining).
    Group,
    /// The lane with this index in [`Plan::lanes`](super::Plan::lanes).
    Lane(usize),
    /// A helper thread, and the lane whose shard failed when known.
    Helper {
        /// Index in [`Plan::helpers`](super::Plan::helpers).
        helper: usize,
        /// The lane whose helper shard failed, if the failure is shard specific.
        lane: Option<usize>,
    },
}

impl fmt::Display for Role {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Group => f.write_str("group"),
            Self::Lane(lane) => write!(f, "lane {lane}"),
            Self::Helper {
                helper,
                lane: Some(lane),
            } => write!(f, "helper {helper} (lane {lane})"),
            Self::Helper { helper, lane: None } => write!(f, "helper {helper}"),
        }
    }
}

/// The lifecycle step during which a failure happened.
#[non_exhaustive]
#[derive(Clone, Copy, Debug, Eq, PartialEq, Hash)]
pub enum Stage {
    /// Validating the plan or cloning the scope before any thread runs.
    Prepare,
    /// Spawning a thread.
    Spawn,
    /// Setting or restoring CPU affinity.
    Pin,
    /// [`Factory::build_lane`](super::Factory::build_lane) or
    /// [`Factory::build_helper`](super::Factory::build_helper).
    Build,
    /// [`Service::start`](super::Service::start) or obtaining the service waker.
    Start,
    /// Steady-state polling, including the steady-state scope check.
    Run,
    /// A failure reported through a [`FailureReporter`](super::FailureReporter).
    Report,
    /// [`Service::stop_admission`](super::Service::stop_admission).
    StopAdmission,
    /// [`Service::drain`](super::Service::drain) or the teardown scope.
    Drain,
    /// [`Service::close`](super::Service::close).
    Close,
    /// [`Service::fence`](super::Service::fence).
    Fence,
    /// [`Service::shutdown`](super::Service::shutdown).
    Shutdown,
    /// [`Factory::abandon_lane`](super::Factory::abandon_lane).
    Abandon,
    /// A caller's wait in a [`Group`](super::Group) method.
    Wait,
    /// Thread exit or join.
    Exit,
}

impl Stage {
    /// A short lowercase name.
    pub const fn as_str(self) -> &'static str {
        match self {
            Self::Prepare => "prepare",
            Self::Spawn => "spawn",
            Self::Pin => "pin",
            Self::Build => "build",
            Self::Start => "start",
            Self::Run => "run",
            Self::Report => "report",
            Self::StopAdmission => "stop admission",
            Self::Drain => "drain",
            Self::Close => "close",
            Self::Fence => "fence",
            Self::Shutdown => "shutdown",
            Self::Abandon => "abandon",
            Self::Wait => "wait",
            Self::Exit => "exit",
        }
    }
}

impl fmt::Display for Stage {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        f.write_str(self.as_str())
    }
}

/// Why the group could not prove that a service's resources are unreferenced.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub enum FatalCause {
    /// A fence (or [`Factory::abandon_lane`](super::Factory::abandon_lane))
    /// returned an error.
    FenceFailed,
    /// A fence panicked, with the panic message.
    FencePanicked(String),
    /// A service was about to be destroyed before both of its fences
    /// completed, for example because its lifecycle was interrupted by a panic.
    Abandoned,
}

/// A failure after which the group cannot prove that external work (such as
/// in-flight kernel I/O) no longer references a service's memory.
///
/// What happens next is decided by the [`FatalPolicy`].
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq, Hash)]
pub struct FatalEvent {
    /// Where it happened.
    pub role: Role,
    /// The lifecycle step.
    pub stage: Stage,
    /// What went wrong.
    pub cause: FatalCause,
}

impl fmt::Display for FatalEvent {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(f, "{} {}: ", self.role, self.stage)?;
        match &self.cause {
            FatalCause::FenceFailed => f.write_str("ownership fence failed"),
            FatalCause::FencePanicked(message) => {
                write!(f, "ownership fence panicked: {message}")
            }
            FatalCause::Abandoned => f.write_str("service abandoned before its fences completed"),
        }
    }
}

/// What the group does after a [`FatalEvent`].
///
/// In every policy the affected service is never dropped: its memory may
/// still be referenced by the kernel.
#[non_exhaustive]
#[derive(Clone, Default)]
pub enum FatalPolicy {
    /// Leak the affected service (`std::mem::forget`), record a
    /// [`GroupError::Fatal`] that [`Group::join`](super::Group::join) and
    /// [`Group::run`](super::Group::run) return, and keep tearing down the
    /// rest of the group. A helper shard whose lane failed its fence is leaked
    /// as well, because the lane's outstanding work may reference it. The
    /// default.
    #[default]
    LeakAndReport,
    /// Print the event to standard error and abort the process.
    Abort,
    /// Call the hook, then behave like [`FatalPolicy::LeakAndReport`]. The
    /// hook runs on the affected thread and may itself abort or exit. A panic
    /// in the hook is caught and ignored.
    Custom(Arc<dyn Fn(&FatalEvent) + Send + Sync>),
}

impl fmt::Debug for FatalPolicy {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::LeakAndReport => f.write_str("LeakAndReport"),
            Self::Abort => f.write_str("Abort"),
            Self::Custom(_) => f.write_str("Custom(..)"),
        }
    }
}

/// Tuning for a [`Group`](super::Group).
///
/// ```
/// use std::time::Duration;
/// use uring_runtime::group::{FatalPolicy, GroupConfig};
///
/// let config = GroupConfig::default()
///     .with_idle_wait(Duration::from_micros(500))
///     .with_fatal(FatalPolicy::Abort);
/// assert_eq!(config.work_budget, 64);
/// ```
#[non_exhaustive]
#[derive(Clone, Debug)]
pub struct GroupConfig {
    /// The longest a lane parks between steady-state turns, and the park
    /// interval while a lifecycle future is pending. A service can shorten it
    /// with [`Service::wait_timeout`](super::Service::wait_timeout) and cut it
    /// short with its waker. Default: 1 ms.
    pub idle_wait: Duration,
    /// The budget passed to [`Service::poll`](super::Service::poll) on lanes.
    /// Default: 64.
    pub work_budget: usize,
    /// The budget passed to [`Service::poll`](super::Service::poll) for each
    /// helper shard per pass. Default: 1.
    pub helper_budget: usize,
    /// How many polling passes over its shards a helper makes before parking
    /// for `idle_wait`. Default: 64.
    pub helper_passes_per_park: usize,
    /// How often a caller blocked in a [`Group`](super::Group) method
    /// re-checks its scope. Default: 1 ms.
    pub wait_interval: Duration,
    /// What to do when an ownership fence cannot be confirmed. Default:
    /// [`FatalPolicy::LeakAndReport`].
    pub fatal: FatalPolicy,
}

impl Default for GroupConfig {
    fn default() -> Self {
        Self {
            idle_wait: Duration::from_millis(1),
            work_budget: 64,
            helper_budget: 1,
            helper_passes_per_park: 64,
            wait_interval: Duration::from_millis(1),
            fatal: FatalPolicy::LeakAndReport,
        }
    }
}

impl GroupConfig {
    /// Set [`GroupConfig::idle_wait`].
    #[must_use]
    pub fn with_idle_wait(mut self, idle_wait: Duration) -> Self {
        self.idle_wait = idle_wait;
        self
    }

    /// Set [`GroupConfig::work_budget`].
    #[must_use]
    pub fn with_work_budget(mut self, work_budget: usize) -> Self {
        self.work_budget = work_budget;
        self
    }

    /// Set [`GroupConfig::helper_budget`].
    #[must_use]
    pub fn with_helper_budget(mut self, helper_budget: usize) -> Self {
        self.helper_budget = helper_budget;
        self
    }

    /// Set [`GroupConfig::helper_passes_per_park`].
    #[must_use]
    pub fn with_helper_passes_per_park(mut self, passes: usize) -> Self {
        self.helper_passes_per_park = passes;
        self
    }

    /// Set [`GroupConfig::wait_interval`].
    #[must_use]
    pub fn with_wait_interval(mut self, wait_interval: Duration) -> Self {
        self.wait_interval = wait_interval;
        self
    }

    /// Set [`GroupConfig::fatal`].
    #[must_use]
    pub fn with_fatal(mut self, fatal: FatalPolicy) -> Self {
        self.fatal = fatal;
        self
    }

    /// Check that budgets and pass counts are nonzero.
    ///
    /// # Errors
    ///
    /// [`ErrorKind::InvalidConfiguration`] naming the offending field.
    pub fn validate(&self) -> Result<()> {
        let invalid =
            |context| Err(Error::new(ErrorKind::InvalidConfiguration).with_context(context));
        if self.work_budget == 0 {
            return invalid("work_budget must be nonzero");
        }
        if self.helper_budget == 0 {
            return invalid("helper_budget must be nonzero");
        }
        if self.helper_passes_per_park == 0 {
            return invalid("helper_passes_per_park must be nonzero");
        }
        Ok(())
    }
}

/// The first failure of a group, attributed to a thread and lifecycle step.
///
/// `E` is the scope's error type. [`GroupError::into_error`] converts any
/// variant into `E` for callers that only want their own error type.
#[non_exhaustive]
#[derive(Clone, Debug, Eq, PartialEq)]
pub enum GroupError<E> {
    /// A service, factory, scope or the runtime returned an error.
    #[non_exhaustive]
    Failed {
        /// Where it happened.
        role: Role,
        /// The lifecycle step.
        stage: Stage,
        /// The error.
        error: E,
    },
    /// A service, factory or scope panicked. The panic was caught and the
    /// group tore down normally.
    #[non_exhaustive]
    Panicked {
        /// Where it happened.
        role: Role,
        /// The lifecycle step.
        stage: Stage,
        /// The panic message, or a placeholder for non-string payloads.
        message: String,
    },
    /// An ownership fence could not be confirmed and the affected service was
    /// leaked (see [`FatalPolicy::LeakAndReport`]). A fatal error replaces any
    /// earlier non-fatal one.
    #[non_exhaustive]
    Fatal {
        /// The event.
        event: FatalEvent,
        /// The error the fence returned, if it returned one.
        error: Option<E>,
    },
}

impl<E> GroupError<E> {
    /// Where the failure happened.
    pub fn role(&self) -> Role {
        match self {
            Self::Failed { role, .. } | Self::Panicked { role, .. } => *role,
            Self::Fatal { event, .. } => event.role,
        }
    }

    /// The lifecycle step of the failure.
    pub fn stage(&self) -> Stage {
        match self {
            Self::Failed { stage, .. } | Self::Panicked { stage, .. } => *stage,
            Self::Fatal { event, .. } => event.stage,
        }
    }

    /// The underlying error, if there is one.
    pub fn error(&self) -> Option<&E> {
        match self {
            Self::Failed { error, .. } => Some(error),
            Self::Fatal { error, .. } => error.as_ref(),
            Self::Panicked { .. } => None,
        }
    }

    /// The panic message, if this is a [`GroupError::Panicked`].
    pub fn panic_message(&self) -> Option<&str> {
        match self {
            Self::Panicked { message, .. } => Some(message),
            _ => None,
        }
    }

    /// Whether a service was leaked.
    pub fn is_fatal(&self) -> bool {
        matches!(self, Self::Fatal { .. })
    }

    /// Convert into the scope's error type.
    ///
    /// [`GroupError::Failed`] yields its error. A panic becomes
    /// [`ErrorKind::Unavailable`] with context `"group service panicked"` (the
    /// message is dropped). A fatal event yields the fence's error if it
    /// returned one, and otherwise [`ErrorKind::Unavailable`] with context
    /// `"group ownership fence not confirmed"`.
    pub fn into_error(self) -> E
    where
        E: From<Error>,
    {
        match self {
            Self::Failed { error, .. }
            | Self::Fatal {
                error: Some(error), ..
            } => error,
            Self::Panicked { .. } => Error::new(ErrorKind::Unavailable)
                .with_context("group service panicked")
                .into(),
            Self::Fatal { error: None, .. } => Error::new(ErrorKind::Unavailable)
                .with_context("group ownership fence not confirmed")
                .into(),
        }
    }

    pub(super) fn failed(role: Role, stage: Stage, error: E) -> Self {
        Self::Failed { role, stage, error }
    }

    pub(super) fn panicked(role: Role, stage: Stage, message: String) -> Self {
        Self::Panicked {
            role,
            stage,
            message,
        }
    }
}

impl<E: fmt::Display> fmt::Display for GroupError<E> {
    fn fmt(&self, f: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::Failed { role, stage, error } => write!(f, "{role} {stage}: {error}"),
            Self::Panicked {
                role,
                stage,
                message,
            } => write!(f, "{role} {stage}: panicked: {message}"),
            Self::Fatal { event, error } => {
                write!(f, "fatal: {event}")?;
                if let Some(error) = error {
                    write!(f, ": {error}")?;
                }
                Ok(())
            }
        }
    }
}

impl<E: std::error::Error + 'static> std::error::Error for GroupError<E> {
    fn source(&self) -> Option<&(dyn std::error::Error + 'static)> {
        self.error().map(|error| error as _)
    }
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn config_validation() {
        assert!(GroupConfig::default().validate().is_ok());
        for config in [
            GroupConfig::default().with_work_budget(0),
            GroupConfig::default().with_helper_budget(0),
            GroupConfig::default().with_helper_passes_per_park(0),
        ] {
            let error = config.validate().unwrap_err();
            assert_eq!(error.kind(), ErrorKind::InvalidConfiguration);
        }
        let config = GroupConfig::default()
            .with_wait_interval(Duration::ZERO)
            .with_fatal(FatalPolicy::Custom(Arc::new(|_| {})));
        assert!(format!("{config:?}").contains("Custom(..)"));
    }

    #[test]
    fn error_accessors_and_conversion() {
        let failed = GroupError::failed(
            Role::Lane(1),
            Stage::Drain,
            Error::new(ErrorKind::Overloaded),
        );
        assert_eq!(failed.role(), Role::Lane(1));
        assert_eq!(failed.stage(), Stage::Drain);
        assert_eq!(failed.to_string(), "lane 1 drain: overloaded");
        assert!(std::error::Error::source(&failed).is_some());
        assert_eq!(failed.into_error().kind(), ErrorKind::Overloaded);

        let panicked = GroupError::<Error>::panicked(
            Role::Helper {
                helper: 0,
                lane: Some(2),
            },
            Stage::Start,
            "boom".into(),
        );
        assert_eq!(panicked.panic_message(), Some("boom"));
        assert_eq!(
            panicked.to_string(),
            "helper 0 (lane 2) start: panicked: boom"
        );
        let error = panicked.into_error();
        assert_eq!(error.context(), Some("group service panicked"));

        let fatal = GroupError::<Error>::Fatal {
            event: FatalEvent {
                role: Role::Group,
                stage: Stage::Fence,
                cause: FatalCause::FencePanicked("x".into()),
            },
            error: None,
        };
        assert!(fatal.is_fatal());
        assert_eq!(fatal.error(), None);
        assert_eq!(
            fatal.to_string(),
            "fatal: group fence: ownership fence panicked: x"
        );
        assert_eq!(fatal.into_error().kind(), ErrorKind::Unavailable);
    }
}
