//! Timer backend selection, sharing the reactor's completion fences.

use super::{Failure, OpScope, Reactor, SubmissionKind};
use crate::{Budget, Error, Operation, Scope};
use std::time::Instant;
use uring::Clock;

/// Choose kernel-owned waiting or cooperative observation of a simulated clock.
#[derive(Clone, Copy, Debug)]
pub enum SleepMode {
    /// Submit an owned io_uring timeout on a host clock and driver.
    Kernel,
    /// Poll the scoped clock cooperatively. The owner advances that clock.
    #[cfg(feature = "simulation")]
    ClockPoll,
}

impl<S: Scope, B: Budget> Reactor<S, B> {
    /// Sleep under caller-owned scope policy.
    ///
    /// `until` must belong to the currently entered clock domain
    /// ([`crate::environment::now`]). Kernel mode rejects simulated clocks,
    /// including for expired deadlines; `ClockPoll` requires one and
    /// cooperatively wakes its executor. The scope is checked again after the
    /// timer fires.
    pub fn sleep_until<'a>(
        &'a self,
        until: Instant,
        mode: SleepMode,
        scope: &'a S,
    ) -> Operation<'a, (), S::Error> {
        Box::pin(async move {
            scope.check()?;
            #[cfg(feature = "simulation")]
            match mode {
                // TODO(sim): also reject Kernel mode when the backend is simulated.
                SleepMode::Kernel if crate::environment::simulation_seed().is_some() => {
                    return Err(Error::InvalidConfiguration.into());
                }
                SleepMode::ClockPoll if crate::environment::simulation_seed().is_none() => {
                    return Err(Error::InvalidConfiguration.into());
                }
                _ => (),
            }
            let duration = until.saturating_duration_since(crate::environment::now());
            if duration.is_zero() {
                return Ok(());
            }
            match mode {
                #[cfg(feature = "simulation")]
                SleepMode::ClockPoll => {
                    return std::future::poll_fn(|cx| {
                        scope.check()?;
                        if crate::environment::now() >= until {
                            std::task::Poll::Ready(Ok(()))
                        } else {
                            cx.waker().wake_by_ref();
                            std::task::Poll::Pending
                        }
                    })
                    .await;
                }
                SleepMode::Kernel => (),
            }
            let bytes = Self::bookkeeping::<(), uring::backend::KernelTimespec>();
            let (core, held) = self.admit(scope, None, SubmissionKind::Timeout, bytes, ())?;
            // Kernel mode runs on the host clock, which the backend clock
            // equals outside simulation.
            let deadline = core
                .clock()
                .now()
                .checked_add(duration)
                .ok_or(Error::InvalidInput)?;
            let (result, held) = core
                .sleep_until(deadline, OpScope::wrap(scope))
                .lease(held)
                .await;
            drop(held);
            result.map_err(Failure::into_scope)?;
            scope.check()
        })
    }
}
